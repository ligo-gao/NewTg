package billing

// 本文件补齐 Task 4 简报要求的退款专项覆盖（既有 session_test.go/ledger_test.go
// 已覆盖的部分不重复）：
//   - 退款流水形状：type=release / status=confirmed / reference_type=usage_log /
//     reference_id=requestID / note=billing:refund（brief (e)，billing-00 §6.2）；
//   - 并发 Refund 两次只产生一行 release（brief 测试要求）；
//   - Terminate 后 Refund 顺序 no-op（「已结算不得再退」）；
//   - 无 hold 会话（only ChargeIncrement）的退款锚点；
//   - 跨会话「已结算」结案标记拦截退款（R-41 的 DB 锚点在退款侧的对称实现）；
//   - 不超扣：会话内存 consumed 被钳制到数据库已实际扣减额；
//   - 金额助手的负数/溢出钳制与跨用户损坏的 fail-closed；
//   - 无 hold 退款的 release 唯一索引冲突（23505）按幂等收敛；
//   - lease 登记失败的 hold-only 回滚分支（无增量时）。
//
// 集成测试需要 NEWTG_TEST_DB_DSN（未设置时由 openBillingTestDB 跳过）。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"newtg/gateway/internal/contract"
)

// failingRegistry 让租约登记失败，用于覆盖 registerLeaseLocked 的回滚路径。
type failingRegistry struct{}

func (failingRegistry) Register(context.Context, string, int64, int64, time.Duration) error {
	return errors.New("redis down")
}
func (failingRegistry) Heartbeat(context.Context, string) error { return nil }
func (failingRegistry) Remove(context.Context, string) error    { return nil }

// refundRow 是 release 流水（退款行）的读投影，比 walletRow 多引用列与 note。
type refundRow struct {
	Type         string
	Amount       int64
	BalanceAfter int64
	Status       string
	RefType      string
	RefID        string
	Note         string
}

// readRefundRows 按 id 升序返回某 requestID 的 release 流水。
func readRefundRows(t *testing.T, db *sql.DB, requestID string) []refundRow {
	t.Helper()
	rows, err := db.Query(
		`SELECT type, amount, balance_after, status,
		        COALESCE(reference_type, ''), COALESCE(reference_id, ''), COALESCE(note, '')
		 FROM wallet_transactions
		 WHERE reference_type = 'usage_log' AND reference_id = $1 AND type = 'release'
		 ORDER BY id`, requestID)
	if err != nil {
		t.Fatalf("read refund rows: %v", err)
	}
	defer rows.Close()
	var out []refundRow
	for rows.Next() {
		var r refundRow
		if err := rows.Scan(&r.Type, &r.Amount, &r.BalanceAfter, &r.Status,
			&r.RefType, &r.RefID, &r.Note); err != nil {
			t.Fatalf("scan refund row: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate refund rows: %v", err)
	}
	return out
}

// TestRefundFullAndRowShape 验证全额退款（hold + ChargeIncrement）的到账金额
// 与流水形状，且重复 Refund 不再产生第二行。
func TestRefundFullAndRowShape(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 500)
	e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)
	req := testRequestID("refund-shape")
	s := newBillingTestSession(e, userID, req, perCallSnapshot(100))

	if err := s.PreConsume(contract.QuotaEstimate{Model: "m"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if err := s.ChargeIncrement(30, contract.UsageFact{}); err != nil {
		t.Fatalf("ChargeIncrement: %v", err)
	}
	if err := s.Refund(); err != nil {
		t.Fatalf("Refund: %v", err)
	}

	if got := userBalance(t, db, userID); got != 500 {
		t.Fatalf("balance = %d, want 500（hold 100 + 增量 30 全额退回）", got)
	}
	if s.Reserved() != 0 {
		t.Fatalf("Reserved = %d, want 0", s.Reserved())
	}
	if s.NeedsRefund() {
		t.Fatal("NeedsRefund = true after Refund, want false")
	}

	rows := readRefundRows(t, db, req)
	if len(rows) != 1 {
		t.Fatalf("refund rows = %d, want 1", len(rows))
	}
	want := refundRow{
		Type:         "release",
		Amount:       130,
		BalanceAfter: 500,
		Status:       "confirmed",
		RefType:      "usage_log",
		RefID:        req,
		Note:         "billing:refund",
	}
	if rows[0] != want {
		t.Fatalf("refund row = %+v, want %+v", rows[0], want)
	}
	// 预扣行必须与退款同事务关闭为 released。
	all := readWalletRows(t, db, req)
	if len(all) != 3 || all[0].Status != "released" {
		t.Fatalf("wallet rows = %+v, want released hold + increment + release", all)
	}

	// 二次退款幂等：不得新增流水、不得再次回补。
	if err := s.Refund(); err != nil {
		t.Fatalf("second Refund: %v", err)
	}
	if got := userBalance(t, db, userID); got != 500 {
		t.Fatalf("balance after second Refund = %d, want 500", got)
	}
	if rows := readRefundRows(t, db, req); len(rows) != 1 {
		t.Fatalf("refund rows after second Refund = %d, want 1", len(rows))
	}
}

// TestRefundConcurrentSameSessionOneRelease 验证同一会话并发 Refund 两次：
// 互斥锁 + refunded 状态位只允许一次资金动作，只产生一行 release。
func TestRefundConcurrentSameSessionOneRelease(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 500)
	e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)
	req := testRequestID("refund-concurrent")
	s := newBillingTestSession(e, userID, req, perCallSnapshot(100))

	if err := s.PreConsume(contract.QuotaEstimate{Model: "m"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}

	start := make(chan struct{})
	errs := make([]error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	for i := range errs {
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = s.Refund()
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("Refund %d: %v", i, err)
		}
	}
	if got := userBalance(t, db, userID); got != 500 {
		t.Fatalf("balance = %d, want 500", got)
	}
	if rows := readRefundRows(t, db, req); len(rows) != 1 {
		t.Fatalf("refund rows = %d, want 1（并发退款只能有一条流水）", len(rows))
	}
	if all := readWalletRows(t, db, req); len(all) != 2 {
		t.Fatalf("wallet rows = %d, want 2", len(all))
	}
}

// TestRefundAfterTerminateNoOp 验证顺序调用 Terminate → Refund 的早返回表：
// 已结算的会话不得再走任何退款。
func TestRefundAfterTerminateNoOp(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 500)
	e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)
	req := testRequestID("refund-after-terminate")
	s := newBillingTestSession(e, userID, req, perCallSnapshot(100))

	if err := s.PreConsume(contract.QuotaEstimate{Model: "m"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if err := s.Terminate(contract.UsageFact{Usage: contract.Usage{Prompt: 1, PromptIncludesCache: true}}); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	balance := userBalance(t, db, userID)
	rows := readWalletRows(t, db, req)

	if err := s.Refund(); err != nil {
		t.Fatalf("Refund after Terminate = %v, want nil（幂等 no-op）", err)
	}
	if got := userBalance(t, db, userID); got != balance {
		t.Fatalf("balance = %d, want %d（已结算不得再退）", got, balance)
	}
	if got := readWalletRows(t, db, req); len(got) != len(rows) {
		t.Fatalf("wallet rows = %+v, want unchanged %+v", got, rows)
	}
	if rows := readRefundRows(t, db, req); len(rows) != 0 {
		t.Fatalf("refund rows = %+v, want none", rows)
	}
	if s.NeedsRefund() {
		t.Fatal("NeedsRefund = true after Terminate, want false")
	}
}

// TestRefundNoHoldIncrementOnly 验证无 hold 会话（信任旁路/预算 0）只有增量
// 扣减时的退款：以 requestID 为锚，回补全部增量并写一行 release。
func TestRefundNoHoldIncrementOnly(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 500)
	e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)
	req := testRequestID("refund-no-hold")
	s := newBillingTestSession(e, userID, req, perCallSnapshot(100))

	// 不调用 PreConsume：模拟无 hold 的会话。
	if err := s.ChargeIncrement(30, contract.UsageFact{}); err != nil {
		t.Fatalf("ChargeIncrement: %v", err)
	}
	if !s.NeedsRefund() {
		t.Fatal("NeedsRefund = false, want true（存在增量扣减）")
	}
	if err := s.Refund(); err != nil {
		t.Fatalf("Refund: %v", err)
	}
	if got := userBalance(t, db, userID); got != 500 {
		t.Fatalf("balance = %d, want 500", got)
	}
	rows := readRefundRows(t, db, req)
	if len(rows) != 1 || rows[0].Amount != 30 || rows[0].Note != "billing:refund" {
		t.Fatalf("refund rows = %+v, want single release 30", rows)
	}
	if s.NeedsRefund() {
		t.Fatal("NeedsRefund = true after Refund, want false")
	}
}

// TestRefundAfterOtherSessionSettlementNoOp 验证 R-41 的 DB 结案标记在退款
// 侧的对称使用：另一会话已无 hold 结算该 requestID 后，本会话的 Refund
// 不得把已结算的增量扣减退回（billing-00 §6.2「已 settled → Refund 不得退」）。
func TestRefundAfterOtherSessionSettlementNoOp(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 500)
	e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)
	req := testRequestID("refund-settled-by-other")
	sA := newBillingTestSession(e, userID, req, ratioSnapshot(100, 100))
	sB := newBillingTestSession(e, userID, req, ratioSnapshot(100, 100))

	// B 先做一次增量扣减（账本中 note 为空，不会被误认成结案标记）。
	if err := sB.ChargeIncrement(30, contract.UsageFact{}); err != nil {
		t.Fatalf("ChargeIncrement: %v", err)
	}
	// A 无 hold 结算：写 consume 行并带 billing:settle 标记。
	fact := contract.UsageFact{Usage: contract.Usage{
		Prompt: 1_000_000, PromptIncludesCache: true, Source: contract.UsageUpstreamExact,
	}}
	if err := sA.Terminate(fact); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if got := userBalance(t, db, userID); got != 370 { // 500 - 30 - 100
		t.Fatalf("balance after settle = %d, want 370", got)
	}

	// B 尝试退款：必须命中共用结案标记并 no-op。
	if err := sB.Refund(); err != nil {
		t.Fatalf("Refund after settle = %v, want nil（幂等 no-op）", err)
	}
	if got := userBalance(t, db, userID); got != 370 {
		t.Fatalf("balance after Refund = %d, want 370（已结算不得再退）", got)
	}
	if rows := readRefundRows(t, db, req); len(rows) != 0 {
		t.Fatalf("refund rows = %+v, want none", rows)
	}
	if sB.NeedsRefund() {
		t.Fatal("NeedsRefund = true after no-op Refund, want false")
	}
}

// TestRefundClampsConsumedToPersisted 是不超扣的核心回归：会话内存的
// consumed 被改写为远超实际扣减额时，退款只能回补数据库已扣减的 30。
func TestRefundClampsConsumedToPersisted(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 500)
	e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)
	req := testRequestID("refund-clamp")
	s := newBillingTestSession(e, userID, req, perCallSnapshot(100))

	if err := s.PreConsume(contract.QuotaEstimate{Model: "m"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if err := s.ChargeIncrement(30, contract.UsageFact{}); err != nil {
		t.Fatalf("ChargeIncrement: %v", err)
	}
	// 白盒注入状态损坏：内存声称扣了 10000，账本只有 30。
	s.mu.Lock()
	s.consumed = 10_000
	s.mu.Unlock()

	if err := s.Refund(); err != nil {
		t.Fatalf("Refund: %v", err)
	}
	if got := userBalance(t, db, userID); got != 500 {
		t.Fatalf("balance = %d, want 500（只能退 hold 100 + 实际增量 30）", got)
	}
	rows := readRefundRows(t, db, req)
	if len(rows) != 1 || rows[0].Amount != 130 {
		t.Fatalf("refund rows = %+v, want single release 130", rows)
	}
}

// TestRefundRejectsCrossUserConsumeRows 验证跨用户损坏的 fail-closed：
// 同一 requestID 的 consume 流水属于两个用户时，退款必须报错而不是把
// 别人的扣减退进本用户钱包。
func TestRefundRejectsCrossUserConsumeRows(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userA := createBillingTestUser(t, db, 500)
	userB := createBillingTestUser(t, db, 500)
	req := testRequestID("refund-cross-user")
	for _, uid := range []int64{userA, userB} {
		if _, err := db.Exec(
			`INSERT INTO wallet_transactions
			   (user_id, type, amount, balance_after, status, reference_type, reference_id)
			 VALUES ($1, 'consume', -10, 0, 'confirmed', 'usage_log', $2)`, uid, req); err != nil {
			t.Fatalf("insert consume row: %v", err)
		}
	}

	_, _, err := NewLedger(gormDB).refundSession(context.Background(), userA, req, 10)
	if err == nil {
		t.Fatal("refundSession = nil, want error（跨用户流水必须拒绝退款）")
	}
	if got := userBalance(t, db, userA); got != 500 {
		t.Fatalf("balance = %d, want 500（失败路径不得改动余额）", got)
	}
	if rows := readRefundRows(t, db, req); len(rows) != 0 {
		t.Fatalf("refund rows = %+v, want none", rows)
	}
}

// TestRefundAmountHelpers 覆盖金额钳制/溢出助手：
// 负数与 int64 溢出必须被识别，正常区间保持精确。
func TestRefundAmountHelpers(t *testing.T) {
	if got, err := refundTotal(100, 30); err != nil || got != 130 {
		t.Fatalf("refundTotal(100,30) = %d, %v; want 130, nil", got, err)
	}
	if got, err := refundTotal(0, math.MaxInt64); err != nil || got != math.MaxInt64 {
		t.Fatalf("refundTotal(0,MaxInt64) = %d, %v; want MaxInt64, nil", got, err)
	}
	if _, err := refundTotal(math.MaxInt64-1, 5); !errors.Is(err, errRefundOverflow) {
		t.Fatalf("refundTotal overflow err = %v, want errRefundOverflow", err)
	}
	if _, err := refundTotal(-1, 0); !errors.Is(err, errRefundOverflow) {
		t.Fatalf("refundTotal(-1,0) err = %v, want errRefundOverflow", err)
	}

	// 负数/零内存值在触库前就返回 0（tx 传 nil 证明未访问数据库）。
	if got, err := refundableConsumed(context.Background(), nil, "req", 1, -5); err != nil || got != 0 {
		t.Fatalf("refundableConsumed(-5) = %d, %v; want 0, nil", got, err)
	}
	if got, err := refundableConsumed(context.Background(), nil, "req", 1, 0); err != nil || got != 0 {
		t.Fatalf("refundableConsumed(0) = %d, %v; want 0, nil", got, err)
	}
}

// TestSessionLeaseRegisterFailureRefundsAll 验证租约登记失败时的回滚是全额
// 退款（R-30）：信任会话先增量扣减、再因图片 Reserve 建立 hold，若此时
// 登记租约失败，hold 与增量都必须在同一行 release 里退回——只退 hold 会
// 留下「会话已置 refunded 但增量未回补」的空洞。
func TestSessionLeaseRegisterFailureRefundsAll(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 500)
	// 打开信任旁路（阈值 1 USD = 100 积分）：PreConsume 不写 hold。
	cfg := mapConfig{
		ConfigPreConsumeRatio: json.RawMessage("1.0"),
		ConfigTrustQuotaUSD:   json.RawMessage("1"),
	}
	e := NewEngine(gormDB, nil, nil, cfg, nil, failingRegistry{}, nil)
	req := testRequestID("lease-fail-refund")
	s := newBillingTestSession(e, userID, req, perCallSnapshot(100))

	// 图片请求：信任旁路命中，但 Reserve 会清 trusted 并真实预留。
	if err := s.PreConsume(contract.QuotaEstimate{Model: "img", ImageCount: 1}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if s.Reserved() != 0 {
		t.Fatalf("Reserved = %d, want 0（信任旁路不预扣）", s.Reserved())
	}
	if err := s.ChargeIncrement(30, contract.UsageFact{}); err != nil {
		t.Fatalf("ChargeIncrement: %v", err)
	}
	if got := userBalance(t, db, userID); got != 470 {
		t.Fatalf("balance after increment = %d, want 470", got)
	}

	// Reserve 写入 hold 后登记租约失败 → 全额回滚并终结会话。
	if err := s.Reserve(50); err == nil {
		t.Fatal("Reserve = nil, want lease registration error")
	}
	if got := userBalance(t, db, userID); got != 500 {
		t.Fatalf("balance = %d, want 500（hold 50 + 增量 30 必须全额退回）", got)
	}
	if s.NeedsRefund() || s.Reserved() != 0 {
		t.Fatalf("NeedsRefund=%v Reserved=%d, want false/0", s.NeedsRefund(), s.Reserved())
	}
	rows := readRefundRows(t, db, req)
	if len(rows) != 1 || rows[0].Amount != 80 {
		t.Fatalf("refund rows = %+v, want single release 80", rows)
	}
}

// TestRefundNoHoldUniqueViolationConvergesIdempotent 覆盖无 hold 退款路径上
// release 唯一索引冲突（SQLSTATE 23505）的幂等收敛分支。
//
// 场景：并发退款的两个执行者都在 settlementExists 复核时看不到对方未提交的
// release 行（READ COMMITTED），后提交者会在 INSERT 时撞
// wallet_release_refid_uq。这里用「已落库但 note 为 NULL 的 release 行」确定
// 性地复现同一状态：结案标记不可见（该行不参与 billing:* 判定），但唯一索引
// 仍然阻止第二行 release。正确行为是按 errHoldClosed 幂等收敛，而不是把
// SQL 冲突上抛给调用方。
func TestRefundNoHoldUniqueViolationConvergesIdempotent(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 500)
	e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)
	req := testRequestID("refund-unique-violation")
	s := newBillingTestSession(e, userID, req, perCallSnapshot(100))

	// 会话只有增量扣减（无 hold），退款会走到无 hold 分支的 INSERT。
	if err := s.ChargeIncrement(30, contract.UsageFact{}); err != nil {
		t.Fatalf("ChargeIncrement: %v", err)
	}
	// 模拟并发赢家已经回补：+30 入账并写 release 行（无 billing:* 标记，
	// 与「尚未提交、marker 复核不可见」的可观察状态一致）。
	credited := userBalance(t, db, userID) + 30
	if _, err := db.Exec(`UPDATE users SET balance = $1 WHERE id = $2`, credited, userID); err != nil {
		t.Fatalf("simulate winner credit: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO wallet_transactions
		   (user_id, type, amount, balance_after, status, reference_type, reference_id)
		 VALUES ($1, 'release', 30, $2, 'confirmed', 'usage_log', $3)`,
		userID, credited, req); err != nil {
		t.Fatalf("pre-insert winning release row: %v", err)
	}

	if err := s.Refund(); err != nil {
		t.Fatalf("Refund = %v, want nil（23505 应按幂等命中收敛）", err)
	}
	if got := userBalance(t, db, userID); got != credited {
		t.Fatalf("balance = %d, want %d（只能由赢家回补一次）", got, credited)
	}
	if rows := readRefundRows(t, db, req); len(rows) != 1 {
		t.Fatalf("refund rows = %d, want 1（不得产生第二行 release）", len(rows))
	}
	if s.NeedsRefund() {
		t.Fatal("NeedsRefund = true after converged Refund, want false")
	}
}

// TestSessionLeaseRegisterFailureHoldOnlyRollback 覆盖 registerLeaseLocked
// 回滚的 consumed==0 分支（hold-only 轻路径）：预扣 100 后登记租约失败，
// 只退 hold，随后会话已终结，Refund/Terminate 均为 no-op。
func TestSessionLeaseRegisterFailureHoldOnlyRollback(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 500)
	e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, failingRegistry{}, nil)
	req := testRequestID("lease-fail-hold-only")
	s := newBillingTestSession(e, userID, req, perCallSnapshot(100))

	if err := s.PreConsume(contract.QuotaEstimate{Model: "m"}); err == nil {
		t.Fatal("PreConsume = nil, want lease registration error")
	}
	if got := userBalance(t, db, userID); got != 500 {
		t.Fatalf("balance = %d, want 500（hold 100 必须全额退回）", got)
	}
	want := []walletRow{
		{Type: "consume_pre", Amount: -100, BalanceAfter: 400, Status: "released"},
		{Type: "release", Amount: 100, BalanceAfter: 500, Status: "confirmed"},
	}
	rows := readWalletRows(t, db, req)
	if len(rows) != len(want) {
		t.Fatalf("rows = %+v, want %+v", rows, want)
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Fatalf("row[%d] = %+v, want %+v", i, rows[i], want[i])
		}
	}
	if s.NeedsRefund() {
		t.Fatal("NeedsRefund = true after rollback, want false")
	}

	// 回滚已把会话置为 refunded：两个终结入口都不得再动资金。
	if err := s.Refund(); err != nil {
		t.Fatalf("Refund after rollback: %v", err)
	}
	if err := s.Terminate(contract.UsageFact{Usage: contract.Usage{Prompt: 1, PromptIncludesCache: true}}); err != nil {
		t.Fatalf("Terminate after rollback: %v", err)
	}
	if got := userBalance(t, db, userID); got != 500 {
		t.Fatalf("balance = %d, want 500", got)
	}
	if got := readWalletRows(t, db, req); len(got) != 2 {
		t.Fatalf("rows after no-op closes = %+v, want unchanged 2 rows", got)
	}
}
