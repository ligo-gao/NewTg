package billing

// S4b 订阅结算接缝的验收用例（billing-08 §5、billing-04 §3.5/§4）。
//
// 覆盖：
//   - Terminate 正/负 delta、超上界拒绝、令牌差额调整（含不足）、空 fact；
//   - ChargeIncrement 立即追加与失败语义；
//   - 崩溃兜底扫描（旧记录退款、已退款跳过、并发幂等）；
//   - 成功日志键完整且钱包零扣减。
//
// 与既有 billing 测试一致：NEWTG_TEST_DB_DSN 未设置时 t.Skip。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/store"
)

// TestSessionSubscriptionTerminatePositiveDelta 覆盖少补：预扣 50、实际 100，
// delta=+50 追加订阅已用额度，并把令牌额度补扣到实际成本。
func TestSessionSubscriptionTerminatePositiveDelta(t *testing.T) {
	funding := newFakeSubscriptionFunding(contract.PrefSubscriptionFirst, subscriptionAccount(7, 1000, nil))
	gormDB, db := openBillingTestDB(t)
	sat := &recordingSaturator{db: db}
	e := NewEngine(gormDB, nil, nil, mapConfig{
		ConfigPreConsumeRatio: json.RawMessage("0.5"),
	}, nil, nil, sat)
	e.SetSubscriptionFunding(funding)
	userID := createBillingTestUser(t, db, 500)
	tokenID := createBillingTestToken(t, db, userID, int64(500))
	req := testRequestID("s4b-pos")
	s := newBillingTestSessionWithToken(e, userID, tokenID, req, perCallSnapshot(100))

	if err := s.PreConsume(contract.QuotaEstimate{Model: "m"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if got := funding.usedOf(7); got != 50 {
		t.Fatalf("预扣后 amount_used = %d，want 50", got)
	}
	if err := s.Terminate(contract.UsageFact{Model: "m"}); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if got := funding.usedOf(7); got != 100 {
		t.Fatalf("结算后 amount_used = %d，want 100（少补 delta）", got)
	}
	if got := tokenRemain(t, db, tokenID).Int64; got != 400 {
		t.Fatalf("令牌剩余 = %d，want 400（预扣 50 + 补扣 50）", got)
	}
	if got := userBalance(t, db, userID); got != 500 {
		t.Fatalf("钱包余额 = %d，want 500（订阅来源不动钱包）", got)
	}
	if rows := readWalletRows(t, db, req); len(rows) != 0 {
		t.Fatalf("钱包流水 = %+v，want 无（订阅来源没有 hold）", rows)
	}
	deltas := funding.deltas()
	if len(deltas) != 1 || deltas[0].subscriptionID != 7 || deltas[0].delta != 50 {
		t.Fatalf("结算增量 = %+v，want [{7 50}]", deltas)
	}
	// 成功结算必须留下 request_registry.settled：否则 15 分钟后的兜底扫描
	// 会把这条已结算的预扣再退一次。
	if state := funding.registry[req]; state != "settled" {
		t.Fatalf("request_registry state = %q，want settled", state)
	}
	if sat.called != 1 {
		t.Fatalf("Saturate 调用 = %d，want 1", sat.called)
	}
	out := sat.outcome
	if out.Status != UsageLogStatusSuccess || out.Cost != 100 || out.PreDeducted != 50 ||
		out.SubscriptionPostDelta != 50 || out.SubscriptionUsed != 100 ||
		out.SubscriptionTotal != 1000 || out.SubscriptionPlanID != 107 ||
		out.SubscriptionPlanTitle != "Plan" || out.BillingPreference != "subscription_first" {
		t.Fatalf("成功 outcome = %+v，字段不符", out)
	}
}

// TestSessionSubscriptionTerminateNegativeDelta 覆盖多退：预扣 100、实际 0，
// delta=-100 回退订阅已用额度，并回补令牌额度。
func TestSessionSubscriptionTerminateNegativeDelta(t *testing.T) {
	funding := newFakeSubscriptionFunding(contract.PrefSubscriptionFirst, subscriptionAccount(7, 5000, nil))
	gormDB, db := openBillingTestDB(t)
	sat := &recordingSaturator{db: db}
	e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, sat)
	e.SetSubscriptionFunding(funding)
	userID := createBillingTestUser(t, db, 500)
	tokenID := createBillingTestToken(t, db, userID, int64(500))
	req := testRequestID("s4b-neg")
	// ratioSnapshot(100,0)：1M prompt tokens = 100 积分；预估恰好预扣 100。
	s := newBillingTestSessionWithToken(e, userID, tokenID, req, ratioSnapshot(100, 0))

	if err := s.PreConsume(contract.QuotaEstimate{Model: "m", PromptTokens: 1_000_000}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if got := funding.usedOf(7); got != 100 {
		t.Fatalf("预扣后 amount_used = %d，want 100", got)
	}
	if err := s.Terminate(contract.UsageFact{Model: "m"}); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if got := funding.usedOf(7); got != 0 {
		t.Fatalf("结算后 amount_used = %d，want 0（多退）", got)
	}
	if got := tokenRemain(t, db, tokenID).Int64; got != 500 {
		t.Fatalf("令牌剩余 = %d，want 500（预扣全额回补）", got)
	}
	out := sat.outcome
	if out.Cost != 0 || out.PreDeducted != 100 || out.Refunded != 100 ||
		out.SubscriptionPostDelta != -100 || out.SubscriptionUsed != 0 {
		t.Fatalf("负差额 outcome = %+v，字段不符", out)
	}
}

// TestSessionSubscriptionTerminateRejectsOverTotal 覆盖上界拒绝：结算增量
// 会把 amount_used 推过 amount_total 时，delta 不落地、预扣保留、会话不
// 标记结算（供兜底扫描处置）。
func TestSessionSubscriptionTerminateRejectsOverTotal(t *testing.T) {
	sub := subscriptionAccount(7, 80, nil)
	funding := newFakeSubscriptionFunding(contract.PrefSubscriptionFirst, sub)
	gormDB, db := openBillingTestDB(t)
	e := NewEngine(gormDB, nil, nil, mapConfig{
		ConfigPreConsumeRatio: json.RawMessage("0.5"),
	}, nil, nil, nil)
	e.SetSubscriptionFunding(funding)
	userID := createBillingTestUser(t, db, 500)
	tokenID := createBillingTestToken(t, db, userID, int64(500))
	s := newBillingTestSessionWithToken(e, userID, tokenID, testRequestID("s4b-over"), perCallSnapshot(100))

	if err := s.PreConsume(contract.QuotaEstimate{Model: "m"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	err := s.Terminate(contract.UsageFact{Model: "m"})
	if !errors.Is(err, store.ErrSubscriptionSettleExceeded) {
		t.Fatalf("Terminate err = %v，want ErrSubscriptionSettleExceeded", err)
	}
	if got := funding.usedOf(7); got != 50 {
		t.Fatalf("越界拒绝后 amount_used = %d，want 50（不做部分结算）", got)
	}
	if got := tokenRemain(t, db, tokenID).Int64; got != 450 {
		t.Fatalf("令牌剩余 = %d，want 450（结算失败不得调整令牌）", got)
	}
	if s.settled {
		t.Fatal("越界拒绝后 settled = true，want false（保留预扣待补偿）")
	}
	if !s.NeedsRefund() {
		t.Fatal("越界拒绝后 NeedsRefund = false，want true（预扣仍在）")
	}
}

// TestSessionSubscriptionTerminateTokenAdjustmentShortfall 覆盖令牌不足：
// 订阅结算必须成功（真实消耗已经发生），令牌差额失败只记 settlement_pending，
// 不得回滚订阅、也不得把会话留在未结算态（重试会二次结算）。
func TestSessionSubscriptionTerminateTokenAdjustmentShortfall(t *testing.T) {
	funding := newFakeSubscriptionFunding(contract.PrefSubscriptionFirst, subscriptionAccount(7, 1000, nil))
	gormDB, db := openBillingTestDB(t)
	sat := &recordingSaturator{db: db}
	e := NewEngine(gormDB, nil, nil, mapConfig{
		ConfigPreConsumeRatio: json.RawMessage("0.5"),
	}, nil, nil, sat)
	e.SetSubscriptionFunding(funding)
	userID := createBillingTestUser(t, db, 500)
	// 令牌只有 50：预扣 50 后余额为 0，少补 50 必然失败。
	tokenID := createBillingTestToken(t, db, userID, int64(50))
	s := newBillingTestSessionWithToken(e, userID, tokenID, testRequestID("s4b-token-short"), perCallSnapshot(100))

	if err := s.PreConsume(contract.QuotaEstimate{Model: "m"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if err := s.Terminate(contract.UsageFact{Model: "m"}); err != nil {
		t.Fatalf("Terminate 不得因令牌差额失败而报错: %v", err)
	}
	if got := funding.usedOf(7); got != 100 {
		t.Fatalf("订阅 amount_used = %d，want 100（资金已正确结算）", got)
	}
	if got := tokenRemain(t, db, tokenID).Int64; got != 0 {
		t.Fatalf("令牌剩余 = %d，want 0（差额补扣失败）", got)
	}
	if !s.settled {
		t.Fatal("令牌差额失败后 settled = false，want true（防止二次结算）")
	}
	if sat.called != 1 || sat.outcome.Cost != 100 {
		t.Fatalf("成功日志 = %d/%+v，want 1 次 cost=100", sat.called, sat.outcome)
	}
}

// TestSessionSubscriptionTerminateEmptyFact 覆盖空 fact（任务按预扣额终结）：
// delta=0、订阅与令牌都不再变化，仍写成功日志。
func TestSessionSubscriptionTerminateEmptyFact(t *testing.T) {
	funding := newFakeSubscriptionFunding(contract.PrefSubscriptionFirst, subscriptionAccount(7, 1000, nil))
	gormDB, db := openBillingTestDB(t)
	sat := &recordingSaturator{db: db}
	e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, sat)
	e.SetSubscriptionFunding(funding)
	userID := createBillingTestUser(t, db, 500)
	tokenID := createBillingTestToken(t, db, userID, int64(500))
	s := newBillingTestSessionWithToken(e, userID, tokenID, testRequestID("s4b-empty"), perCallSnapshot(100))

	if err := s.PreConsume(contract.QuotaEstimate{Model: "m"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if err := s.Terminate(contract.UsageFact{}); err != nil {
		t.Fatalf("Terminate(empty): %v", err)
	}
	if got := funding.usedOf(7); got != 100 {
		t.Fatalf("amount_used = %d，want 100（空 fact 按预扣额结算）", got)
	}
	if got := tokenRemain(t, db, tokenID).Int64; got != 400 {
		t.Fatalf("令牌剩余 = %d，want 400（不再调整）", got)
	}
	if sat.called != 1 || sat.outcome.SubscriptionPostDelta != 0 || sat.outcome.Cost != 100 {
		t.Fatalf("空 fact outcome = %d/%+v，want delta=0 cost=100", sat.called, sat.outcome)
	}
}

// TestSessionSubscriptionChargeIncrement 覆盖实时增量：立即追加订阅额度并
// 同步扣令牌；Terminate 以「已扣额」为基准，不会把增量再扣一次。
func TestSessionSubscriptionChargeIncrement(t *testing.T) {
	funding := newFakeSubscriptionFunding(contract.PrefSubscriptionFirst, subscriptionAccount(7, 1000, nil))
	gormDB, db := openBillingTestDB(t)
	sat := &recordingSaturator{db: db}
	e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, sat)
	e.SetSubscriptionFunding(funding)
	userID := createBillingTestUser(t, db, 500)
	tokenID := createBillingTestToken(t, db, userID, int64(500))
	s := newBillingTestSessionWithToken(e, userID, tokenID, testRequestID("s4b-inc"), perCallSnapshot(100))

	if err := s.PreConsume(contract.QuotaEstimate{Model: "m"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if err := s.ChargeIncrement(30, contract.UsageFact{Model: "m"}); err != nil {
		t.Fatalf("ChargeIncrement: %v", err)
	}
	if got := funding.usedOf(7); got != 130 {
		t.Fatalf("增量后 amount_used = %d，want 130", got)
	}
	if got := tokenRemain(t, db, tokenID).Int64; got != 370 {
		t.Fatalf("令牌剩余 = %d，want 370（预扣 100 + 增量 30）", got)
	}
	// 空 fact 终止：成本 = 预扣 100 + 增量 30，delta 必须为 0。
	if err := s.Terminate(contract.UsageFact{}); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if got := funding.usedOf(7); got != 130 {
		t.Fatalf("终止后 amount_used = %d，want 130（增量不得重复扣）", got)
	}
	if got := tokenRemain(t, db, tokenID).Int64; got != 370 {
		t.Fatalf("终止后令牌剩余 = %d，want 370", got)
	}
	if sat.outcome.SubscriptionPostDelta != 0 || sat.outcome.Cost != 130 {
		t.Fatalf("终止 outcome = %+v，want delta=0 cost=130", sat.outcome)
	}
}

// TestSessionSubscriptionChargeIncrementFailure 覆盖增量结算失败：请求失败、
// 已发生的增量不可回滚（令牌不得再扣），订阅额度保持原状。
func TestSessionSubscriptionChargeIncrementFailure(t *testing.T) {
	funding := newFakeSubscriptionFunding(contract.PrefSubscriptionFirst, subscriptionAccount(7, 1000, nil))
	gormDB, db := openBillingTestDB(t)
	e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)
	e.SetSubscriptionFunding(funding)
	userID := createBillingTestUser(t, db, 500)
	tokenID := createBillingTestToken(t, db, userID, int64(500))
	s := newBillingTestSessionWithToken(e, userID, tokenID, testRequestID("s4b-inc-fail"), perCallSnapshot(100))

	if err := s.PreConsume(contract.QuotaEstimate{Model: "m"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	funding.mu.Lock()
	funding.postDeltaErr = errors.New("fake: subscription delta down")
	funding.mu.Unlock()

	if err := s.ChargeIncrement(30, contract.UsageFact{Model: "m"}); err == nil {
		t.Fatal("ChargeIncrement err = nil，want 失败")
	}
	if got := funding.usedOf(7); got != 100 {
		t.Fatalf("失败后 amount_used = %d，want 100（delta 未落地）", got)
	}
	if got := tokenRemain(t, db, tokenID).Int64; got != 400 {
		t.Fatalf("令牌剩余 = %d，want 400（订阅失败不得再扣令牌）", got)
	}
	if s.consumed != 0 {
		t.Fatalf("consumed = %d，want 0", s.consumed)
	}
}

// TestSessionSubscriptionChargeIncrementExceedsTotal 覆盖实时增量越界：
// 立即拒绝、请求失败，已用额度不变。
func TestSessionSubscriptionChargeIncrementExceedsTotal(t *testing.T) {
	funding := newFakeSubscriptionFunding(contract.PrefSubscriptionFirst, subscriptionAccount(7, 100, nil))
	gormDB, db := openBillingTestDB(t)
	e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)
	e.SetSubscriptionFunding(funding)
	userID := createBillingTestUser(t, db, 500)
	tokenID := createBillingTestToken(t, db, userID, int64(500))
	s := newBillingTestSessionWithToken(e, userID, tokenID, testRequestID("s4b-inc-over"), perCallSnapshot(100))

	if err := s.PreConsume(contract.QuotaEstimate{Model: "m"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if err := s.ChargeIncrement(30, contract.UsageFact{Model: "m"}); !errors.Is(err, store.ErrSubscriptionSettleExceeded) {
		t.Fatalf("ChargeIncrement err = %v，want ErrSubscriptionSettleExceeded", err)
	}
	if got := funding.usedOf(7); got != 100 {
		t.Fatalf("越界后 amount_used = %d，want 100", got)
	}
	if got := tokenRemain(t, db, tokenID).Int64; got != 400 {
		t.Fatalf("令牌剩余 = %d，want 400", got)
	}
}

// TestSessionSubscriptionSuccessLogKeys 覆盖 billing-08 §5.3 的成功日志键：
// 订阅快照完整、钱包扣减固定 0，且不写钱包流水。
func TestSessionSubscriptionSuccessLogKeys(t *testing.T) {
	funding := newFakeSubscriptionFunding(contract.PrefSubscriptionFirst, subscriptionAccount(7, 1000, nil))
	gormDB, db := openBillingTestDB(t)
	sat := &recordingSaturator{db: db}
	e := NewEngine(gormDB, nil, nil, mapConfig{
		ConfigPreConsumeRatio: json.RawMessage("0.5"),
	}, nil, nil, sat)
	e.SetSubscriptionFunding(funding)
	userID := createBillingTestUser(t, db, 500)
	tokenID := createBillingTestToken(t, db, userID, int64(500))
	req := testRequestID("s4b-log")
	s := newBillingTestSessionWithToken(e, userID, tokenID, req, perCallSnapshot(100))

	if err := s.PreConsume(contract.QuotaEstimate{Model: "m"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if err := s.Terminate(contract.UsageFact{Model: "m"}); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	other := buildUsageLogOther(s.rc, "m", sql.NullInt64{}, sat.outcome, false, nil)

	want := map[string]any{
		"billing_source":            "subscription",
		"billing_preference":        "subscription_first",
		"subscription_id":           int64(7),
		"subscription_plan_id":      int64(107),
		"subscription_plan_title":   "Plan",
		"subscription_total":        int64(1000),
		"subscription_used":         int64(100),
		"subscription_remain":       int64(900),
		"subscription_consumed":     int64(100),
		"subscription_pre_consumed": int64(50),
		"subscription_post_delta":   int64(50),
		"wallet_quota_deducted":     int64(0),
	}
	for key, value := range want {
		if got := other[key]; got != value {
			t.Fatalf("other[%q] = %#v，want %#v", key, got, value)
		}
	}
	if rows := readWalletRows(t, db, req); len(rows) != 0 {
		t.Fatalf("钱包流水 = %+v，want 无", rows)
	}
}

// TestRecoverStaleSubscriptionPreConsumes 覆盖崩溃兜底扫描：旧 consumed 记录
// 退款、已 refunded 与未到窗口的记录跳过、并发扫描幂等。
func TestRecoverStaleSubscriptionPreConsumes(t *testing.T) {
	funding := newFakeSubscriptionFunding(contract.PrefSubscriptionFirst,
		subscriptionAccount(7, 1000, nil), subscriptionAccount(8, 1000, nil))
	gormDB, _ := openBillingTestDB(t)
	e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)
	e.SetSubscriptionFunding(funding)
	now := time.Now().UTC()
	old := now.Add(-20 * time.Minute)

	funding.records["stale-old"] = store.SubscriptionPreConsume{
		RequestID: "stale-old", SubscriptionID: 7, Status: "consumed",
		PreConsumed: 60, CreatedAt: old,
	}
	funding.used[7] = 60
	funding.records["stale-refunded"] = store.SubscriptionPreConsume{
		RequestID: "stale-refunded", SubscriptionID: 7, Status: "refunded",
		PreConsumed: 40, CreatedAt: old,
	}
	funding.records["stale-fresh"] = store.SubscriptionPreConsume{
		RequestID: "stale-fresh", SubscriptionID: 8, Status: "consumed",
		PreConsumed: 10, CreatedAt: now.Add(-time.Minute),
	}
	funding.used[8] = 10

	recovered, err := e.RecoverStaleSubscriptionPreConsumes(context.Background(), now, 100)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if recovered != 1 {
		t.Fatalf("recovered = %d，want 1（仅旧 consumed 记录）", recovered)
	}
	if got := funding.usedOf(7); got != 0 {
		t.Fatalf("subscription 7 amount_used = %d，want 0（旧记录已退）", got)
	}
	if got := funding.usedOf(8); got != 10 {
		t.Fatalf("subscription 8 amount_used = %d，want 10（未到窗口不得退）", got)
	}
	if rec := funding.records["stale-old"]; rec.Status != "refunded" {
		t.Fatalf("stale-old status = %q，want refunded", rec.Status)
	}

	// 并发扫描：同一记录被两个执行者同时看到，退款必须幂等。
	funding.records["stale-concurrent"] = store.SubscriptionPreConsume{
		RequestID: "stale-concurrent", SubscriptionID: 8, Status: "consumed",
		PreConsumed: 10, CreatedAt: old,
	}
	funding.used[8] = 20 // 含该预扣的已用额度；退款后应剩 10。
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = e.RecoverStaleSubscriptionPreConsumes(context.Background(), now, 100)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent recover %d: %v", i, err)
		}
	}
	if got := funding.usedOf(8); got != 10 {
		t.Fatalf("并发退款后 subscription 8 amount_used = %d，want 10（只退一次）", got)
	}
}

// TestTerminateByRequestIDSubscriptionTask 覆盖订阅来源的任务/崩溃恢复结算：
// 空 fact 按预扣额结清、写入 registry 终态；重复调用幂等（不二次结算）。
func TestTerminateByRequestIDSubscriptionTask(t *testing.T) {
	funding := newFakeSubscriptionFunding(contract.PrefSubscriptionFirst, subscriptionAccount(7, 1000, nil))
	gormDB, db := openBillingTestDB(t)
	e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)
	e.SetSubscriptionFunding(funding)
	userID := createBillingTestUser(t, db, 500)
	tokenID := createBillingTestToken(t, db, userID, int64(500))
	req := testUUID(t)

	// 模拟 Detach 前的订阅预扣：记录 + 已用额度 + 令牌已扣。
	funding.mu.Lock()
	funding.records[req] = store.SubscriptionPreConsume{
		RequestID: req, SubscriptionID: 7, Status: "consumed",
		PreConsumed: 100, CreatedAt: time.Now().UTC(),
	}
	funding.owners[req] = userID
	funding.used[7] = 100
	funding.mu.Unlock()
	if _, err := db.Exec(`UPDATE api_tokens SET remain_quota = remain_quota - 100 WHERE id = $1`, tokenID); err != nil {
		t.Fatalf("simulate token debit: %v", err)
	}

	if err := e.TerminateByRequestIDForToken(req, tokenID, contract.UsageFact{}); err != nil {
		t.Fatalf("TerminateByRequestIDForToken: %v", err)
	}
	if got := funding.usedOf(7); got != 100 {
		t.Fatalf("结算后 amount_used = %d，want 100（空 fact delta=0）", got)
	}
	if state := funding.registry[req]; state != "settled" {
		t.Fatalf("registry state = %q，want settled", state)
	}
	if got := tokenRemain(t, db, tokenID).Int64; got != 400 {
		t.Fatalf("令牌剩余 = %d，want 400（已扣额即最终成本）", got)
	}

	// 重复调用：registry 已 settled → no-op，不再产生 delta。
	before := len(funding.deltas())
	if err := e.TerminateByRequestIDForToken(req, tokenID, contract.UsageFact{}); err != nil {
		t.Fatalf("重复 TerminateByRequestIDForToken: %v", err)
	}
	if got := len(funding.deltas()); got != before {
		t.Fatalf("重复结算产生 %d 次 delta，want %d（幂等）", got, before)
	}

	// 结算成功后迟到的失败回调不得退款（终态互斥）。
	if err := e.RefundByRequestIDForToken(req, tokenID); err != nil {
		t.Fatalf("结算后 RefundByRequestIDForToken: %v", err)
	}
	if got := funding.usedOf(7); got != 100 {
		t.Fatalf("结算后退款改变了 amount_used = %d，want 100", got)
	}
	if state := funding.records[req].Status; state != "consumed" {
		t.Fatalf("结算后退款把预扣改为 %q，want consumed", state)
	}

	// 已退款记录：终态互斥，不再结算。
	refundedReq := testUUID(t)
	funding.mu.Lock()
	funding.records[refundedReq] = store.SubscriptionPreConsume{
		RequestID: refundedReq, SubscriptionID: 7, Status: "refunded",
		PreConsumed: 100, CreatedAt: time.Now().UTC(),
	}
	funding.mu.Unlock()
	before = len(funding.deltas())
	if err := e.TerminateByRequestIDForToken(refundedReq, tokenID, contract.UsageFact{}); err != nil {
		t.Fatalf("已退款记录的 Terminate: %v", err)
	}
	if got := len(funding.deltas()); got != before {
		t.Fatalf("已退款记录产生 %d 次 delta，want %d", got, before)
	}

	// 未结算请求的订阅退款：回退额度、回补令牌并写 registry.refunded。
	refundReq := testUUID(t)
	funding.mu.Lock()
	funding.records[refundReq] = store.SubscriptionPreConsume{
		RequestID: refundReq, SubscriptionID: 7, Status: "consumed",
		PreConsumed: 100, CreatedAt: time.Now().UTC(),
	}
	funding.owners[refundReq] = userID
	funding.used[7] = 200 // 含该请求的 100
	funding.mu.Unlock()
	if _, err := db.Exec(`UPDATE api_tokens SET remain_quota = remain_quota - 100 WHERE id = $1`, tokenID); err != nil {
		t.Fatalf("simulate token debit: %v", err)
	}
	if err := e.RefundByRequestIDForToken(refundReq, tokenID); err != nil {
		t.Fatalf("RefundByRequestIDForToken: %v", err)
	}
	if got := funding.usedOf(7); got != 100 {
		t.Fatalf("退款后 amount_used = %d，want 100", got)
	}
	if state := funding.registry[refundReq]; state != "refunded" {
		t.Fatalf("registry state = %q，want refunded", state)
	}
	if got := tokenRemain(t, db, tokenID).Int64; got != 400 {
		t.Fatalf("退款后令牌剩余 = %d，want 400", got)
	}
}

// TestSessionSubscriptionRefundReversesIncrements 覆盖 S4b 的退款完整性：
// ChargeIncrement 立即落账的增量必须随全额退款一起回退（订阅与令牌两条账），
// 否则失败请求会永久占用订阅额度。
func TestSessionSubscriptionRefundReversesIncrements(t *testing.T) {
	funding := newFakeSubscriptionFunding(contract.PrefSubscriptionFirst, subscriptionAccount(7, 1000, nil))
	gormDB, db := openBillingTestDB(t)
	sat := &recordingSaturator{db: db}
	e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, sat)
	e.SetSubscriptionFunding(funding)
	userID := createBillingTestUser(t, db, 500)
	tokenID := createBillingTestToken(t, db, userID, int64(500))
	req := testRequestID("s4b-refund-inc")
	s := newBillingTestSessionWithToken(e, userID, tokenID, req, perCallSnapshot(100))

	if err := s.PreConsume(contract.QuotaEstimate{Model: "m"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if err := s.ChargeIncrement(30, contract.UsageFact{Model: "m"}); err != nil {
		t.Fatalf("ChargeIncrement: %v", err)
	}
	if got := funding.usedOf(7); got != 130 {
		t.Fatalf("退款前 amount_used = %d，want 130", got)
	}
	if err := s.Refund(); err != nil {
		t.Fatalf("Refund: %v", err)
	}
	if got := funding.usedOf(7); got != 0 {
		t.Fatalf("退款后 amount_used = %d，want 0（预扣 + 增量都回退）", got)
	}
	if got := tokenRemain(t, db, tokenID).Int64; got != 500 {
		t.Fatalf("退款后令牌剩余 = %d，want 500", got)
	}
	if got := funding.records[req].Status; got != "refunded" {
		t.Fatalf("预扣记录 status = %q，want refunded", got)
	}
	if state := funding.registry[req]; state != "refunded" {
		t.Fatalf("registry state = %q，want refunded", state)
	}
	if sat.outcome.Status != UsageLogStatusRefunded || sat.outcome.Refunded != 130 {
		t.Fatalf("退款日志 outcome = %+v，want refunded 130", sat.outcome)
	}
	if got := userBalance(t, db, userID); got != 500 {
		t.Fatalf("钱包余额 = %d，want 500", got)
	}
}

// TestSubscriptionLeaseConfigValidation 覆盖 M-1：窗口 <= TTL、TTL 低于安全
// 下界、TTL <= 0 都必须被判为配置错误；同一套校验在启动期
// （ValidateSubscriptionLeaseConfig）与扫描入口（RecoverStale...）一致生效。
func TestSubscriptionLeaseConfigValidation(t *testing.T) {
	cases := []struct {
		name    string
		cfg     mapConfig
		wantErr bool
	}{
		{"defaults", mapConfig{}, false},
		{"window == ttl", mapConfig{
			ConfigSubscriptionRecoveryWindowSeconds:   json.RawMessage("120"),
			ConfigSubscriptionInflightLeaseTTLSeconds: json.RawMessage("120"),
		}, true},
		{"window < ttl", mapConfig{
			ConfigSubscriptionRecoveryWindowSeconds:   json.RawMessage("60"),
			ConfigSubscriptionInflightLeaseTTLSeconds: json.RawMessage("120"),
		}, true},
		{"ttl below safety floor", mapConfig{
			ConfigSubscriptionInflightLeaseTTLSeconds: json.RawMessage("60"),
		}, true},
		{"ttl zero", mapConfig{
			ConfigSubscriptionInflightLeaseTTLSeconds: json.RawMessage("0"),
		}, true},
		{"ttl negative", mapConfig{
			ConfigSubscriptionInflightLeaseTTLSeconds: json.RawMessage("-1"),
		}, true},
		{"valid explicit", mapConfig{
			ConfigSubscriptionRecoveryWindowSeconds:   json.RawMessage("900"),
			ConfigSubscriptionInflightLeaseTTLSeconds: json.RawMessage("120"),
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := NewEngine(nil, nil, nil, tc.cfg, nil, nil, nil)
			e.SetSubscriptionFunding(newFakeSubscriptionFunding(contract.PrefSubscriptionFirst))

			err := e.ValidateSubscriptionLeaseConfig()
			if tc.wantErr && !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("ValidateSubscriptionLeaseConfig = %v，want ErrInvalidConfig", err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("ValidateSubscriptionLeaseConfig = %v，want nil", err)
			}

			// 扫描入口必须复用同一套校验：配置错误时在 list 之前就失败，
			// 不能把错误配置当成「本轮没有候选」。
			_, recErr := e.RecoverStaleSubscriptionPreConsumes(
				context.Background(), time.Now().UTC(), 1)
			if tc.wantErr && !errors.Is(recErr, ErrInvalidConfig) {
				t.Fatalf("RecoverStale = %v，want ErrInvalidConfig", recErr)
			}
			if !tc.wantErr && recErr != nil {
				t.Fatalf("RecoverStale = %v，want nil", recErr)
			}
		})
	}
}

// TestSubscriptionLeaseTTLTestOverride 覆盖测试注入旋钮：非零 subLeaseTTL
// 完全覆盖配置（允许毫秒级 TTL），零值仍走生产配置与 120s 下界。
func TestSubscriptionLeaseTTLTestOverride(t *testing.T) {
	e := NewEngine(nil, nil, nil, mapConfig{
		ConfigSubscriptionInflightLeaseTTLSeconds: json.RawMessage("1"),
	}, nil, nil, nil)
	if _, err := e.subscriptionLeaseTTL(); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("生产配置 1s = %v，want ErrInvalidConfig（安全下界 120s）", err)
	}
	e.subLeaseTTL = 25 * time.Millisecond
	got, err := e.subscriptionLeaseTTL()
	if err != nil || got != 25*time.Millisecond {
		t.Fatalf("override = %v/%v，want 25ms/nil", got, err)
	}
}
