package billing

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/store"
)

// 本文件是任务异步交接面（contract.BillingHandoff）的验收。
// Detach 单测用内存 Registry；ByRequestID 走真实账本（NEWTG_TEST_DB_DSN）。

// handoffRegistry 是 lease Registry 的内存实现，记录心跳与移除。
type handoffRegistry struct {
	mu         sync.Mutex
	removed    []string
	heartbeats int
}

func (r *handoffRegistry) Register(context.Context, string, int64, int64, time.Duration) error {
	return nil
}
func (r *handoffRegistry) Heartbeat(context.Context, string) error {
	r.mu.Lock()
	r.heartbeats++
	r.mu.Unlock()
	return nil
}
func (r *handoffRegistry) Remove(_ context.Context, requestID string) error {
	r.mu.Lock()
	r.removed = append(r.removed, requestID)
	r.mu.Unlock()
	return nil
}
func (r *handoffRegistry) heartbeatCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.heartbeats
}
func (r *handoffRegistry) removedIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.removed...)
}

func TestSessionDetachStopsHeartbeatAndKeepsHold(t *testing.T) {
	registry := &handoffRegistry{}
	engine := &Engine{registry: registry, heartbeat: 5 * time.Millisecond}
	requestID := "req-detach"
	s := &Session{
		engine:          engine,
		requestID:       requestID,
		rc:              &contract.RelayContext{RequestID: requestID},
		leaseRegistered: true,
		preConsumed:     100,
	}
	s.startHeartbeatLocked()

	deadline := time.Now().Add(time.Second)
	for registry.heartbeatCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if registry.heartbeatCount() == 0 {
		t.Fatalf("heartbeat did not start")
	}

	if err := s.Detach(requestID); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	// 心跳必须停止：Detach 返回后计数不再增长。
	before := registry.heartbeatCount()
	time.Sleep(40 * time.Millisecond)
	if got := registry.heartbeatCount(); got != before {
		t.Fatalf("heartbeat still running after Detach: %d -> %d", before, got)
	}
	// hold 必须保留（交接不是结算）。
	if got := s.Reserved(); got != 100 {
		t.Fatalf("Detach changed the hold: %d, want 100", got)
	}
	if ids := registry.removedIDs(); len(ids) != 1 || ids[0] != requestID {
		t.Fatalf("lease not removed: %v", ids)
	}
	// 幂等。
	if err := s.Detach(requestID); err != nil {
		t.Fatalf("second Detach: %v", err)
	}
	// 错 ID 必须拒绝，不能停掉别人的心跳。
	if err := s.Detach("other"); err == nil {
		t.Fatalf("Detach with a foreign request id must fail")
	}
}

// TestTerminateAtHeldWithoutPricing 验收 T6b 的成功任务默认结算路径：
// 空 UsageFact = 按预扣额终结（差额 0），**不需要**重新解析定价。
//
// 这是「资金不冻结」的关键：重建会话没有冻结快照，若走按 token 重算的
// Terminate，在 ratio/tiered 模式下空用量会算成 0 并全额退款（成功任务变
// 免费服务）；而完全不结算会让 Detach 后的 hold 永久冻结。
func TestTerminateAtHeldWithoutPricing(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 500)
	// source=nil：任何定价解析都会失败；at-held 路径必须完全不依赖它。
	engine := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)

	requestID := testRequestID("handoff-held")
	session := newBillingTestSession(engine, userID, requestID, perCallSnapshot(100))
	if err := session.PreConsume(contract.QuotaEstimate{Model: "test-model"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if got := userBalance(t, db, userID); got != 400 {
		t.Fatalf("balance after pre-consume = %d, want 400", got)
	}
	// 模拟 Detach 后的持久化交接：只有 requestID，没有会话对象。
	if err := session.Detach(requestID); err != nil {
		t.Fatalf("Detach: %v", err)
	}

	if err := engine.TerminateByRequestID(requestID, contract.UsageFact{}); err != nil {
		t.Fatalf("TerminateByRequestID(empty): %v", err)
	}
	if got := userBalance(t, db, userID); got != 400 {
		t.Fatalf("at-held settlement changed balance: %d, want 400 (pre-consumed retained)", got)
	}
	st, err := engine.ledger.openHoldState(context.Background(), requestID)
	if err != nil {
		t.Fatalf("openHoldState: %v", err)
	}
	if st.Held != 0 || !st.Closed || st.ClosedStatus != "settled" {
		t.Fatalf("hold not settled at held amount: %+v", st)
	}

	// 幂等：重复终结不改变余额，也不产生第二条结算行。
	if err := engine.TerminateByRequestID(requestID, contract.UsageFact{}); err != nil {
		t.Fatalf("idempotent TerminateByRequestID(empty): %v", err)
	}
	if got := userBalance(t, db, userID); got != 400 {
		t.Fatalf("idempotent at-held settlement changed balance: %d", got)
	}
	// 已结算的请求不再被 worker 兜底扫描退款。
	if err := engine.RecoverAbandonedByID(context.Background(), requestID, 0); err != nil {
		t.Fatalf("RecoverAbandonedByID on settled request: %v", err)
	}
	if got := userBalance(t, db, userID); got != 400 {
		t.Fatalf("abandoned recovery touched a settled request: balance %d", got)
	}
}

func TestEngineHandoffByRequestID(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 500)
	engine := NewEngine(gormDB, nil, nil, billingTestConfig(),
		&staticPricingSource{snap: *perCallSnapshot(100)}, nil, nil)

	// 结算路径：TerminateByRequestID 释放 hold 并按成本落账。
	terminateReq := testRequestID("handoff-term")
	termSession := newBillingTestSession(engine, userID, terminateReq, perCallSnapshot(100))
	if err := termSession.PreConsume(contract.QuotaEstimate{Model: "test-model"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if err := engine.TerminateByRequestID(terminateReq,
		contract.UsageFact{Model: "test-model"}); err != nil {
		t.Fatalf("TerminateByRequestID: %v", err)
	}
	if got := userBalance(t, db, userID); got != 400 {
		t.Fatalf("balance after terminate = %d, want 400", got)
	}
	if err := engine.TerminateByRequestID(terminateReq,
		contract.UsageFact{Model: "test-model"}); err != nil {
		t.Fatalf("idempotent TerminateByRequestID: %v", err)
	}
	if got := userBalance(t, db, userID); got != 400 {
		t.Fatalf("idempotent terminate changed balance: %d", got)
	}

	// 退款路径：RefundByRequestID 全额退回 hold，且幂等。
	refundUser := createBillingTestUser(t, db, 500)
	refundReq := testRequestID("handoff-refund")
	refundSession := newBillingTestSession(engine, refundUser, refundReq, perCallSnapshot(100))
	if err := refundSession.PreConsume(contract.QuotaEstimate{Model: "test-model"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if err := engine.RefundByRequestID(refundReq); err != nil {
		t.Fatalf("RefundByRequestID: %v", err)
	}
	if got := userBalance(t, db, refundUser); got != 500 {
		t.Fatalf("balance after refund = %d, want 500", got)
	}
	if err := engine.RefundByRequestID(refundReq); err != nil {
		t.Fatalf("idempotent RefundByRequestID: %v", err)
	}
	if got := userBalance(t, db, refundUser); got != 500 {
		t.Fatalf("idempotent refund changed balance: %d", got)
	}

	// 重启模拟：worker 的兜底扫描不再看到已结案/已退款的请求。
	if err := engine.RecoverAbandonedByID(context.Background(), refundReq, 0); err != nil {
		t.Fatalf("RecoverAbandonedByID on settled request: %v", err)
	}
	if got := userBalance(t, db, refundUser); got != 500 {
		t.Fatalf("abandoned recovery touched a refunded request: balance %d", got)
	}
}

// TestTerminateByRequestIDForTokenCostAdjustsBothWays 验收 T5 的冻结成本结算：
// hold=100、成本=70 → 退 30；成本=130 → 补扣 30。两条方向都走同一 hold CAS，
// 且不重新解析定价（source=nil 也能结算）。
func TestTerminateByRequestIDForTokenCostAdjustsBothWays(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 500)
	// source=nil：冻结成本路径不得依赖定价源。
	engine := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)

	refundReq := testRequestID("handoff-cost-refund")
	refundSession := newBillingTestSession(engine, userID, refundReq, perCallSnapshot(100))
	if err := refundSession.PreConsume(contract.QuotaEstimate{Model: "test-model"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if got := userBalance(t, db, userID); got != 400 {
		t.Fatalf("balance after pre-consume = %d, want 400", got)
	}
	if err := engine.TerminateByRequestIDForTokenCost(refundReq, 0, 70, contract.UsageFact{}); err != nil {
		t.Fatalf("cost settlement (refund): %v", err)
	}
	if got := userBalance(t, db, userID); got != 430 {
		t.Fatalf("balance after cost 70 = %d, want 430 (hold 100, refund 30)", got)
	}
	// 幂等：重复调用不得再调整。
	if err := engine.TerminateByRequestIDForTokenCost(refundReq, 0, 70, contract.UsageFact{}); err != nil {
		t.Fatalf("idempotent cost settlement: %v", err)
	}
	if got := userBalance(t, db, userID); got != 430 {
		t.Fatalf("idempotent cost settlement changed balance: %d", got)
	}

	chargeReq := testRequestID("handoff-cost-charge")
	chargeSession := newBillingTestSession(engine, userID, chargeReq, perCallSnapshot(100))
	if err := chargeSession.PreConsume(contract.QuotaEstimate{Model: "test-model"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if err := engine.TerminateByRequestIDForTokenCost(chargeReq, 0, 130, contract.UsageFact{}); err != nil {
		t.Fatalf("cost settlement (charge): %v", err)
	}
	if got := userBalance(t, db, userID); got != 300 {
		t.Fatalf("balance after cost 130 = %d, want 300 (hold 100, charge extra 30)", got)
	}

	// 负成本是调用方错误，必须在引擎边界拒绝。
	if err := engine.TerminateByRequestIDForTokenCost(chargeReq, 0, -1, contract.UsageFact{}); err == nil {
		t.Fatalf("negative cost must be rejected")
	}
}

// TestTerminateByRequestIDForTokenCostInsufficientChargeWritesNoLog 覆盖复核
// I5：补扣失败时资金回滚，不得提前写成功/全价 usage_log；余额补足后差额
// 结算成功才落日志。
func TestTerminateByRequestIDForTokenCostInsufficientChargeWritesNoLog(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	logsDB := openTask5LogsDB(t)
	userID := createBillingTestUser(t, db, 100)
	engine := NewEngine(gormDB, logsDB, nil, billingTestConfig(), nil, nil, NewDBLogger(logsDB))
	req := newTask5UUID(t)
	t.Cleanup(func() { _, _ = logsDB.Exec(`DELETE FROM usage_logs WHERE request_id = $1::uuid`, req) })

	session := newBillingTestSession(engine, userID, req, perCallSnapshot(100))
	if err := session.PreConsume(contract.QuotaEstimate{Model: "test-model"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if got := userBalance(t, db, userID); got != 0 {
		t.Fatalf("balance after pre-consume = %d, want 0", got)
	}
	// cost=130 超出 hold 100，需要补扣 30；钱包为 0 → 资金失败。
	if err := engine.TerminateByRequestIDForTokenCost(req, 0, 130, contract.UsageFact{}); err == nil {
		t.Fatalf("insufficient wallet must fail the extra charge")
	}
	var n int
	if err := logsDB.QueryRow(
		`SELECT count(*) FROM usage_logs WHERE request_id = $1::uuid`, req).Scan(&n); err != nil {
		t.Fatalf("count usage_logs: %v", err)
	}
	if n != 0 {
		t.Fatalf("usage_logs rows = %d, want 0 (no success log before funds are charged)", n)
	}
	if got := userBalance(t, db, userID); got != 0 {
		t.Fatalf("hold must remain after failed charge: balance=%d", got)
	}

	// 成本 70：多退 30，成功写日志（status=success，cost=70）。
	if err := engine.TerminateByRequestIDForTokenCost(req, 0, 70, contract.UsageFact{}); err != nil {
		t.Fatalf("cost settlement after hold remains: %v", err)
	}
	if got := userBalance(t, db, userID); got != 30 {
		t.Fatalf("balance after cost 70 = %d, want 30", got)
	}
	var status int16
	var cost int64
	if err := logsDB.QueryRow(
		`SELECT status, cost FROM usage_logs WHERE request_id = $1::uuid`, req).
		Scan(&status, &cost); err != nil {
		t.Fatalf("read usage_logs: %v", err)
	}
	if status != UsageLogStatusSuccess || cost != 70 {
		t.Fatalf("usage_log status/cost = %d/%d, want %d/70", status, cost, UsageLogStatusSuccess)
	}
}

// TestRefundByRequestIDForToken 验收 T6b 复核 H1：后台退款必须同时回补
// 钱包与令牌额度。提交时的 PreConsume 在同一事务里扣了两者；重建会话若
// 只水合 preConsumed，Refund 的 `tokenDeducted > 0` 条件恒假，令牌额度会
// 永久少扣。
func TestRefundByRequestIDForToken(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 500)
	tokenID := createBillingTestToken(t, db, userID, int64(500))
	engine := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)

	requestID := testRequestID("handoff-token-refund")
	session := newBillingTestSessionWithToken(engine, userID, tokenID, requestID, perCallSnapshot(100))
	if err := session.PreConsume(contract.QuotaEstimate{Model: "test-model"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if got := userBalance(t, db, userID); got != 400 {
		t.Fatalf("wallet after preconsume = %d, want 400", got)
	}
	if got := readTokenRemain(t, db, tokenID); got != 400 {
		t.Fatalf("token after preconsume = %d, want 400", got)
	}
	if err := session.Detach(requestID); err != nil {
		t.Fatalf("Detach: %v", err)
	}

	if err := engine.RefundByRequestIDForToken(requestID, tokenID); err != nil {
		t.Fatalf("RefundByRequestIDForToken: %v", err)
	}
	if got := userBalance(t, db, userID); got != 500 {
		t.Fatalf("wallet after refund = %d, want 500", got)
	}
	if got := readTokenRemain(t, db, tokenID); got != 500 {
		t.Fatalf("token after refund = %d, want 500 (HIGH-1: token quota must be restored)", got)
	}
	// 幂等：重复退款不得二次入账。
	if err := engine.RefundByRequestIDForToken(requestID, tokenID); err != nil {
		t.Fatalf("second RefundByRequestIDForToken: %v", err)
	}
	if got := userBalance(t, db, userID); got != 500 {
		t.Fatalf("wallet changed on idempotent refund: %d", got)
	}
	if got := readTokenRemain(t, db, tokenID); got != 500 {
		t.Fatalf("token changed on idempotent refund: %d", got)
	}
}

// TestTerminateAtHeldForToken 验收成功任务按预扣额终结时：
// 令牌额度保持「初始−预扣」（差额 0），且 usage_logs 能归属到令牌。
func TestTerminateAtHeldForToken(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 500)
	tokenID := createBillingTestToken(t, db, userID, int64(500))
	engine := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)

	requestID := testRequestID("handoff-token-held")
	session := newBillingTestSessionWithToken(engine, userID, tokenID, requestID, perCallSnapshot(100))
	if err := session.PreConsume(contract.QuotaEstimate{Model: "test-model"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if err := session.Detach(requestID); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	if err := engine.TerminateByRequestIDForToken(requestID, tokenID, contract.UsageFact{}); err != nil {
		t.Fatalf("TerminateByRequestIDForToken: %v", err)
	}
	if got := userBalance(t, db, userID); got != 400 {
		t.Fatalf("wallet after at-held settle = %d, want 400", got)
	}
	if got := readTokenRemain(t, db, tokenID); got != 400 {
		t.Fatalf("token after at-held settle = %d, want 400 (no delta)", got)
	}
	if err := engine.TerminateByRequestIDForToken(requestID, tokenID, contract.UsageFact{}); err != nil {
		t.Fatalf("idempotent terminate: %v", err)
	}
	if got := userBalance(t, db, userID); got != 400 {
		t.Fatalf("wallet changed on idempotent terminate: %d", got)
	}
}

// TestRefundByRequestIDForTokenSubscription 验收 S4a 复核 H1：异步任务失败/
// 超时时，订阅来源必须按 requestID 回退订阅额度与令牌额度（订阅没有钱包
// hold 行，旧的 openHoldState 路径会直接 no-op，令牌永久少扣）。
func TestRefundByRequestIDForTokenSubscription(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 500)
	tokenID := createBillingTestToken(t, db, userID, int64(500))
	repo := store.NewSubscriptionRepo(db)
	engine := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)
	engine.SetSubscriptionFunding(NewStoreSubscriptionFunding(repo, db))

	ctx := context.Background()
	plan, err := repo.CreatePlan(ctx, store.SubscriptionPlanInput{
		PlanKey:      "handoff-sub-" + time.Now().Format("150405.000000000"),
		Title:        "Handoff Sub",
		PeriodDays:   30,
		Currency:     "CNY",
		GrantCredits: 1000,
	})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM subscription_plans WHERE id = $1`, plan.ID) })
	now := time.Now().UTC()
	sub, err := repo.CreateSubscription(ctx, store.SubscriptionCreateInput{
		UserID: userID, PlanID: plan.ID, PlanTitleSnapshot: plan.Title,
		StartAt: now.Add(-time.Hour), EndAt: now.Add(time.Hour),
		AmountTotal: plan.GrantCredits, AmountUsed: 0,
		AllowWalletOverflow: plan.AllowWalletOverflow, Currency: plan.Currency,
		PricePaidAmount: plan.PriceAmount, PeriodDays: plan.PeriodDays,
		ResetOnRenew: plan.ResetOnRenew, GraceDays: plan.GraceDays,
	})
	if err != nil {
		t.Fatalf("CreateSubscription: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM subscription_pre_consume_records WHERE subscription_id = $1`, sub.ID)
		_, _ = db.Exec(`DELETE FROM subscriptions WHERE id = $1`, sub.ID)
	})

	requestID := testUUID(t)
	session := newBillingTestSessionWithToken(engine, userID, tokenID, requestID, perCallSnapshot(100))
	if err := session.PreConsume(contract.QuotaEstimate{Model: "test-model"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if session.fundingSource != contract.FundingSubscription {
		t.Fatalf("来源 = %v，want subscription", session.fundingSource)
	}
	if got := userBalance(t, db, userID); got != 500 {
		t.Fatalf("订阅预扣动了钱包: %d，want 500", got)
	}
	if got := readTokenRemain(t, db, tokenID); got != 400 {
		t.Fatalf("预扣后令牌 = %d，want 400", got)
	}
	var used int64
	if err := db.QueryRow(`SELECT amount_used FROM subscriptions WHERE id = $1`, sub.ID).Scan(&used); err != nil {
		t.Fatalf("read amount_used: %v", err)
	}
	if used != 100 {
		t.Fatalf("amount_used = %d，want 100", used)
	}
	if err := session.Detach(requestID); err != nil {
		t.Fatalf("Detach: %v", err)
	}

	// 任务失败退款：只带 requestID + tokenID 的持久化入口。
	if err := engine.RefundByRequestIDForToken(requestID, tokenID); err != nil {
		t.Fatalf("RefundByRequestIDForToken: %v", err)
	}
	if got := readTokenRemain(t, db, tokenID); got != 500 {
		t.Fatalf("退款后令牌 = %d，want 500（H1：令牌额度必须回补）", got)
	}
	if got := userBalance(t, db, userID); got != 500 {
		t.Fatalf("退款后钱包 = %d，want 500（订阅退款不得动钱包）", got)
	}
	if err := db.QueryRow(`SELECT amount_used FROM subscriptions WHERE id = $1`, sub.ID).Scan(&used); err != nil {
		t.Fatalf("read amount_used after refund: %v", err)
	}
	if used != 0 {
		t.Fatalf("退款后 amount_used = %d，want 0", used)
	}
	rec, err := repo.GetPreConsume(ctx, requestID)
	if err != nil {
		t.Fatalf("GetPreConsume: %v", err)
	}
	if rec.Status != "refunded" {
		t.Fatalf("预扣记录状态 = %q，want refunded", rec.Status)
	}

	// 幂等：重复退款不得二次回补令牌或额度。
	if err := engine.RefundByRequestIDForToken(requestID, tokenID); err != nil {
		t.Fatalf("second RefundByRequestIDForToken: %v", err)
	}
	if got := readTokenRemain(t, db, tokenID); got != 500 {
		t.Fatalf("幂等退款后令牌 = %d，want 500", got)
	}
	if err := db.QueryRow(`SELECT amount_used FROM subscriptions WHERE id = $1`, sub.ID).Scan(&used); err != nil {
		t.Fatalf("read amount_used after idempotent refund: %v", err)
	}
	if used != 0 {
		t.Fatalf("幂等退款后 amount_used = %d，want 0", used)
	}
}

func readTokenRemain(t *testing.T, db *sql.DB, tokenID int64) int64 {
	t.Helper()
	var remain int64
	if err := db.QueryRow(`SELECT remain_quota FROM api_tokens WHERE id = $1`, tokenID).Scan(&remain); err != nil {
		t.Fatalf("read token remain: %v", err)
	}
	return remain
}

// TestTerminateByRequestIDForTokenNonEmptyFactAdjustsToken 验收 T6b 复核
// MEDIUM-A：非空 fact 的「多退少补」必须同步调整令牌额度。
func TestTerminateByRequestIDForTokenNonEmptyFactAdjustsToken(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 500)
	tokenID := createBillingTestToken(t, db, userID, int64(500))
	// 提交时按 100 预扣；终结时按 50 重新定价（差额退回）。
	engine := NewEngine(gormDB, nil, nil, billingTestConfig(),
		&staticPricingSource{snap: *perCallSnapshot(50)}, nil, nil)

	requestID := testRequestID("handoff-token-fact")
	session := newBillingTestSessionWithToken(engine, userID, tokenID, requestID, perCallSnapshot(100))
	if err := session.PreConsume(contract.QuotaEstimate{Model: "test-model"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if err := session.Detach(requestID); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	if err := engine.TerminateByRequestIDForToken(requestID, tokenID,
		contract.UsageFact{Model: "test-model"}); err != nil {
		t.Fatalf("TerminateByRequestIDForToken: %v", err)
	}
	if got := userBalance(t, db, userID); got != 450 {
		t.Fatalf("wallet after reprice = %d, want 450", got)
	}
	if got := readTokenRemain(t, db, tokenID); got != 450 {
		t.Fatalf("token after reprice = %d, want 450 (MEDIUM-A: token must follow the delta)", got)
	}
}

// TestTaskForcedCostStillReadsFrozenDiscount 是 needsFrozenPricing 的回归：
// 冻结成本（cost != nil）路径不读 request_pricing 的时间/倍率，但冻结折扣
// 必须照读——否则重建会话按原价结算，把预扣时已让利的部分再追回来。
func TestTaskForcedCostStillReadsFrozenDiscount(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 500)
	sat := &recordingSaturator{db: db}
	engine := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, sat)
	engine.SetRequestPricingStore(store.NewRequestPricingRepo(db))

	req := newTask5UUID(t)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM request_pricing WHERE request_id = $1::uuid`, req) })

	session := newBillingTestSession(engine, userID, req, perCallSnapshot(100))
	session.applyFrozenDiscount(0.9, "user:7")
	if err := session.PreConsume(contract.QuotaEstimate{Model: "test-model"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if got := userBalance(t, db, userID); got != 410 {
		t.Fatalf("预扣后 balance = %d, want 410（hold 90）", got)
	}
	if err := session.Detach(req); err != nil {
		t.Fatalf("Detach: %v", err)
	}

	// cost 是提交期冻结的任务表达式原价；结算必须按冻结折扣收敛到 81。
	if err := engine.TerminateByRequestIDForTokenCost(req, 0, 90, contract.UsageFact{}); err != nil {
		t.Fatalf("TerminateByRequestIDForTokenCost: %v", err)
	}
	out := sat.outcome
	if out.Cost != 81 {
		t.Fatalf("outcome.Cost = %d, want 81（90 × 0.9）", out.Cost)
	}
	if out.OriginalCost != 90 || out.DiscountRate != 0.9 || out.DiscountTarget != "user:7" {
		t.Fatalf("outcome = %+v, want original 90 / rate 0.9 / target user:7", out)
	}
}

// TestTerminateAtHeldReconstructsOriginalCost 验证按预扣额结清时，hold 已是
// 折后金额；结算日志的原价必须由冻结折扣反推（90 / 0.9 = 100），而不是
// 让 original_cost 与 cost 相等。
func TestTerminateAtHeldReconstructsOriginalCost(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 500)
	sat := &recordingSaturator{db: db}
	engine := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, sat)
	engine.SetRequestPricingStore(store.NewRequestPricingRepo(db))

	req := newTask5UUID(t)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM request_pricing WHERE request_id = $1::uuid`, req) })

	session := newBillingTestSession(engine, userID, req, perCallSnapshot(100))
	session.applyFrozenDiscount(0.9, "user:7")
	if err := session.PreConsume(contract.QuotaEstimate{Model: "test-model"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if err := session.Detach(req); err != nil {
		t.Fatalf("Detach: %v", err)
	}

	if err := engine.TerminateByRequestID(req, contract.UsageFact{}); err != nil {
		t.Fatalf("TerminateByRequestID(empty): %v", err)
	}
	out := sat.outcome
	if out.Cost != 90 || out.OriginalCost != 100 {
		t.Fatalf("outcome = %+v, want cost 90 / original 100", out)
	}
	if out.DiscountRate != 0.9 || out.DiscountTarget != "user:7" {
		t.Fatalf("outcome = %+v, want rate 0.9 / target user:7", out)
	}
	if got := userBalance(t, db, userID); got != 410 {
		t.Fatalf("balance = %d, want 410（at-held 差额 0）", got)
	}
}

// TestTerminateAtHeldZeroRateOmitsOriginalCost 验证全免（rate=0）不反推原价：
// OriginalCost 保持 0，但 Target 非空，与「未命中」可区分。
func TestTerminateAtHeldZeroRateOmitsOriginalCost(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 500)
	sat := &recordingSaturator{db: db}
	repo := store.NewRequestPricingRepo(db)
	engine := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, sat)
	engine.SetRequestPricingStore(repo)

	req := newTask5UUID(t)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM request_pricing WHERE request_id = $1::uuid`, req) })

	// 先在无折扣下预扣，得到非零 hold；随后覆盖冻结事实为全免规则。
	session := newBillingTestSession(engine, userID, req, perCallSnapshot(100))
	if err := session.PreConsume(contract.QuotaEstimate{Model: "test-model"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if err := repo.Upsert(context.Background(), store.RequestPricing{
		RequestID:      req,
		RequestTime:    time.Now().UTC(),
		TimeMultiplier: 1,
		DiscountRate:   0,
		DiscountTarget: "user:7",
	}); err != nil {
		t.Fatalf("Upsert(frozen discount): %v", err)
	}
	if err := session.Detach(req); err != nil {
		t.Fatalf("Detach: %v", err)
	}

	if err := engine.TerminateByRequestID(req, contract.UsageFact{}); err != nil {
		t.Fatalf("TerminateByRequestID(empty): %v", err)
	}
	out := sat.outcome
	if out.Cost != 100 {
		t.Fatalf("outcome.Cost = %d, want 100（at-held 以 hold 为准）", out.Cost)
	}
	if out.OriginalCost != 0 || out.DiscountRate != 0 {
		t.Fatalf("outcome = %+v, want original 0 / rate 0（全免不反推原价）", out)
	}
	if out.DiscountTarget != "user:7" {
		t.Fatalf("outcome.DiscountTarget = %q, want user:7（全免必须与未命中可区分）", out.DiscountTarget)
	}
}

// TestTerminateByRequestIDMissingFrozenRowBillsListPrice 验证上线前在飞请求
// （没有 request_pricing 行）按原价结算：不报错，也不写折扣键。
func TestTerminateByRequestIDMissingFrozenRowBillsListPrice(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 500)
	sat := &recordingSaturator{db: db}
	engine := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, sat)
	engine.SetRequestPricingStore(store.NewRequestPricingRepo(db))

	req := newTask5UUID(t)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM request_pricing WHERE request_id = $1::uuid`, req) })

	session := newBillingTestSession(engine, userID, req, perCallSnapshot(100))
	if err := session.PreConsume(contract.QuotaEstimate{Model: "test-model"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	// 模拟功能上线前的在飞请求：没有冻结行。
	if _, err := db.Exec(`DELETE FROM request_pricing WHERE request_id = $1::uuid`, req); err != nil {
		t.Fatalf("delete frozen row: %v", err)
	}
	if err := session.Detach(req); err != nil {
		t.Fatalf("Detach: %v", err)
	}

	if err := engine.TerminateByRequestIDForTokenCost(req, 0, 100, contract.UsageFact{}); err != nil {
		t.Fatalf("缺行不得阻塞结算: %v", err)
	}
	out := sat.outcome
	if out.Cost != 100 {
		t.Fatalf("outcome.Cost = %d, want 100（原价）", out.Cost)
	}
	if out.OriginalCost != 0 || out.DiscountRate != 0 || out.DiscountTarget != "" {
		t.Fatalf("outcome = %+v, want 无折扣字段", out)
	}
	if got := userBalance(t, db, userID); got != 400 {
		t.Fatalf("balance = %d, want 400（原价结清 hold）", got)
	}
}
