package billing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"newtg/gateway/internal/contract"
)

// newBillingTestSession 构造绑定到固定 requestID 的会话。
func newBillingTestSession(e *Engine, userID int64, requestID string, snap *contract.PriceSnapshot) *Session {
	rc := &contract.RelayContext{
		RequestID:     requestID,
		UserID:        int(userID),
		OriginModel:   "test-model",
		UsingGroup:    "default",
		PriceSnapshot: snap,
	}
	return e.NewSession(rc).(*Session)
}

// TestSessionPreConsumeBudgetEnough 覆盖预扣成功路径：预算=快照价、余额扣减、
// 恰一行 consume_pre+hold 流水、NeedsRefund 置位。
func TestSessionPreConsumeBudgetEnough(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 500)
	e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)
	req := testRequestID("pre-ok")
	s := newBillingTestSession(e, userID, req, perCallSnapshot(100))

	if err := s.PreConsume(contract.QuotaEstimate{Model: "m", PromptTokens: 10}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if got := s.Reserved(); got != 100 {
		t.Fatalf("Reserved = %d, want 100", got)
	}
	if !s.NeedsRefund() {
		t.Fatal("NeedsRefund = false, want true（存在未结算 hold）")
	}
	if got := userBalance(t, db, userID); got != 400 {
		t.Fatalf("balance = %d, want 400", got)
	}
	rows := readWalletRows(t, db, req)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	want := walletRow{Type: "consume_pre", Amount: -100, BalanceAfter: 400, Status: "hold"}
	if rows[0] != want {
		t.Fatalf("row = %+v, want %+v", rows[0], want)
	}
}

// TestSessionPreConsumeInsufficient 覆盖余额不足：errors.Is 命中哨兵、
// QuotaError 携带 Class/Required/Available、不产生流水、余额不变。
func TestSessionPreConsumeInsufficient(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 50)
	e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)
	req := testRequestID("pre-insufficient")
	s := newBillingTestSession(e, userID, req, perCallSnapshot(100))

	err := s.PreConsume(contract.QuotaEstimate{Model: "m"})
	if !errors.Is(err, ErrInsufficientQuota) {
		t.Fatalf("err = %v, want ErrInsufficientQuota", err)
	}
	var qe *QuotaError
	if !errors.As(err, &qe) {
		t.Fatalf("err type = %T, want *QuotaError", err)
	}
	if qe.Class != contract.ErrInsufficientQuota {
		t.Fatalf("Class = %v, want ErrInsufficientQuota", qe.Class)
	}
	if qe.Required != 100 || qe.Available != 50 {
		t.Fatalf("QuotaError = %+v", qe)
	}
	if qe.ErrorClass() != contract.ErrInsufficientQuota {
		t.Fatalf("ErrorClass() = %v", qe.ErrorClass())
	}
	if s.Reserved() != 0 || s.NeedsRefund() {
		t.Fatalf("Reserved=%d NeedsRefund=%v, want 0/false", s.Reserved(), s.NeedsRefund())
	}
	if got := userBalance(t, db, userID); got != 50 {
		t.Fatalf("balance = %d, want 50", got)
	}
	if rows := readWalletRows(t, db, req); len(rows) != 0 {
		t.Fatalf("rows = %d, want 0（预扣失败不得写流水）", len(rows))
	}
}

// TestSessionTerminateDeltas 覆盖多退少补的三个方向：
// 实耗 < 预扣（release）、实耗 > 预扣（consume 少补）、相等（无差额行）。
func TestSessionTerminateDeltas(t *testing.T) {
	tests := []struct {
		name        string
		actualP     int64
		wantBalance int64
		wantRows    []walletRow
	}{
		{
			name:        "多退：实耗 80 < 预扣 100",
			actualP:     800_000,
			wantBalance: 420,
			wantRows: []walletRow{
				{Type: "consume_pre", Amount: -100, BalanceAfter: 400, Status: "settled"},
				{Type: "release", Amount: 20, BalanceAfter: 420, Status: "confirmed"},
			},
		},
		{
			name:        "少补：实耗 150 > 预扣 100",
			actualP:     1_500_000,
			wantBalance: 350,
			wantRows: []walletRow{
				{Type: "consume_pre", Amount: -100, BalanceAfter: 400, Status: "settled"},
				{Type: "consume", Amount: -50, BalanceAfter: 350, Status: "confirmed"},
			},
		},
		{
			name:        "相等：实耗 100 == 预扣 100",
			actualP:     1_000_000,
			wantBalance: 400,
			wantRows: []walletRow{
				{Type: "consume_pre", Amount: -100, BalanceAfter: 400, Status: "settled"},
			},
		},
		{
			name:        "无计费用量：配额 0，全额释放 hold",
			actualP:     0,
			wantBalance: 500,
			wantRows: []walletRow{
				{Type: "consume_pre", Amount: -100, BalanceAfter: 400, Status: "settled"},
				{Type: "release", Amount: 100, BalanceAfter: 500, Status: "confirmed"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gormDB, db := openBillingTestDB(t)
			userID := createBillingTestUser(t, db, 500)
			e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)
			req := testRequestID("terminate")
			s := newBillingTestSession(e, userID, req, ratioSnapshot(100, 100))

			est := contract.QuotaEstimate{Model: "m", PromptTokens: 1_000_000}
			if err := s.PreConsume(est); err != nil {
				t.Fatalf("PreConsume: %v", err)
			}
			fact := contract.UsageFact{Usage: contract.Usage{
				Prompt: tc.actualP, PromptIncludesCache: true, Source: contract.UsageUpstreamExact,
			}}
			if err := s.Terminate(fact); err != nil {
				t.Fatalf("Terminate: %v", err)
			}
			if got := userBalance(t, db, userID); got != tc.wantBalance {
				t.Fatalf("balance = %d, want %d", got, tc.wantBalance)
			}
			rows := readWalletRows(t, db, req)
			if len(rows) != len(tc.wantRows) {
				t.Fatalf("rows = %+v, want %+v", rows, tc.wantRows)
			}
			for i := range rows {
				if rows[i] != tc.wantRows[i] {
					t.Fatalf("row[%d] = %+v, want %+v", i, rows[i], tc.wantRows[i])
				}
			}
			if s.NeedsRefund() {
				t.Fatal("NeedsRefund = true after Terminate, want false")
			}
		})
	}
}

// TestSessionChargeIncrementTerminate 覆盖实时三段式：
// PreConsume 100 → ChargeIncrement 30（实际扣）→ Terminate 实耗 120。
// 结算差额 = 120 − 30(已扣) − 100(hold) = −10，释放 10；用户净付 120。
func TestSessionChargeIncrementTerminate(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 500)
	e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)
	req := testRequestID("increment")
	s := newBillingTestSession(e, userID, req, ratioSnapshot(100, 100))

	if err := s.PreConsume(contract.QuotaEstimate{Model: "m", PromptTokens: 1_000_000}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if err := s.ChargeIncrement(30, contract.UsageFact{}); err != nil {
		t.Fatalf("ChargeIncrement: %v", err)
	}
	if got := userBalance(t, db, userID); got != 370 {
		t.Fatalf("balance after increment = %d, want 370", got)
	}
	fact := contract.UsageFact{Usage: contract.Usage{
		Prompt: 1_200_000, PromptIncludesCache: true, Source: contract.UsageUpstreamExact,
	}}
	if err := s.Terminate(fact); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if got := userBalance(t, db, userID); got != 380 {
		t.Fatalf("balance = %d, want 380（净扣 120）", got)
	}
	rows := readWalletRows(t, db, req)
	want := []walletRow{
		{Type: "consume_pre", Amount: -100, BalanceAfter: 400, Status: "settled"},
		{Type: "consume", Amount: -30, BalanceAfter: 370, Status: "confirmed"},
		{Type: "release", Amount: 10, BalanceAfter: 380, Status: "confirmed"},
	}
	if len(rows) != len(want) {
		t.Fatalf("rows = %+v, want %+v", rows, want)
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Fatalf("row[%d] = %+v, want %+v", i, rows[i], want[i])
		}
	}
	if err := s.ChargeIncrement(5, contract.UsageFact{}); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("ChargeIncrement after Terminate = %v, want ErrSessionClosed", err)
	}
}

// TestSessionTerminateIdempotent 验证重复 Terminate 不重复结算。
func TestSessionTerminateIdempotent(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 500)
	e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)
	req := testRequestID("terminate-twice")
	s := newBillingTestSession(e, userID, req, perCallSnapshot(100))

	if err := s.PreConsume(contract.QuotaEstimate{Model: "m"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	fact := contract.UsageFact{Usage: contract.Usage{Prompt: 1, PromptIncludesCache: true}}
	if err := s.Terminate(fact); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if err := s.Terminate(fact); err != nil {
		t.Fatalf("second Terminate: %v（幂等应返回 nil）", err)
	}
	if got := userBalance(t, db, userID); got != 400 {
		t.Fatalf("balance = %d, want 400（不得二次扣费）", got)
	}
	if rows := readWalletRows(t, db, req); len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
}

// TestSessionRefundIdempotent 验证 Refund 全额且幂等，且 Refund 后 Terminate 不再扣费。
func TestSessionRefundIdempotent(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 500)
	e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)
	req := testRequestID("refund")
	s := newBillingTestSession(e, userID, req, perCallSnapshot(100))

	if err := s.PreConsume(contract.QuotaEstimate{Model: "m"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if err := s.Refund(); err != nil {
		t.Fatalf("Refund: %v", err)
	}
	if got := userBalance(t, db, userID); got != 500 {
		t.Fatalf("balance = %d, want 500", got)
	}
	if s.NeedsRefund() {
		t.Fatal("NeedsRefund = true after Refund, want false")
	}
	if err := s.Refund(); err != nil {
		t.Fatalf("second Refund: %v（幂等应返回 nil）", err)
	}
	if err := s.Terminate(contract.UsageFact{Usage: contract.Usage{Prompt: 1, PromptIncludesCache: true}}); err != nil {
		t.Fatalf("Terminate after Refund: %v（应 no-op）", err)
	}
	if got := userBalance(t, db, userID); got != 500 {
		t.Fatalf("balance = %d, want 500（退款后不得再扣）", got)
	}
	rows := readWalletRows(t, db, req)
	want := []walletRow{
		{Type: "consume_pre", Amount: -100, BalanceAfter: 400, Status: "released"},
		{Type: "release", Amount: 100, BalanceAfter: 500, Status: "confirmed"},
	}
	if len(rows) != len(want) {
		t.Fatalf("rows = %+v, want %+v", rows, want)
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Fatalf("row[%d] = %+v, want %+v", i, rows[i], want[i])
		}
	}
}

// TestSessionRecoverAbandonedIdempotent 验证崩溃兜底：从 hold 行复原退款，
// 重复调用不重复退款（worker 只持有 requestID，不持有原会话）。
func TestSessionRecoverAbandonedIdempotent(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 500)
	e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)
	req := testRequestID("recover")
	s := newBillingTestSession(e, userID, req, perCallSnapshot(100))

	if err := s.PreConsume(contract.QuotaEstimate{Model: "m"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}

	// worker 场景：另一个会话对象（甚至不同请求）按 requestID 调用。
	worker := newBillingTestSession(e, userID, testRequestID("worker"), perCallSnapshot(1))
	if err := worker.RecoverAbandoned(req); err != nil {
		t.Fatalf("RecoverAbandoned: %v", err)
	}
	if got := userBalance(t, db, userID); got != 500 {
		t.Fatalf("balance = %d, want 500", got)
	}
	rows := readWalletRows(t, db, req)
	want := []walletRow{
		{Type: "consume_pre", Amount: -100, BalanceAfter: 400, Status: "expired"},
		{Type: "release", Amount: 100, BalanceAfter: 500, Status: "confirmed"},
	}
	if len(rows) != len(want) {
		t.Fatalf("rows = %+v, want %+v", rows, want)
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Fatalf("row[%d] = %+v, want %+v", i, rows[i], want[i])
		}
	}

	// 重复扫描/同一会话再次调用：no-op。
	if err := worker.RecoverAbandoned(req); err != nil {
		t.Fatalf("second RecoverAbandoned: %v", err)
	}
	if err := s.RecoverAbandoned(req); err != nil {
		t.Fatalf("same-session RecoverAbandoned: %v", err)
	}
	if got := userBalance(t, db, userID); got != 500 {
		t.Fatalf("balance = %d, want 500（不得重复退款）", got)
	}
	if rows := readWalletRows(t, db, req); len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
}

// fakeRegistry 记录租约调用序，用于断言 Register → Heartbeat… → Remove。
type fakeRegistry struct {
	mu         sync.Mutex
	events     []string
	lease      time.Duration
	heartbeats int
}

func (f *fakeRegistry) Register(_ context.Context, _ string, _ int64, lease time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, "register")
	f.lease = lease
	return nil
}

func (f *fakeRegistry) Heartbeat(_ context.Context, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, "heartbeat")
	f.heartbeats++
	return nil
}

func (f *fakeRegistry) Remove(_ context.Context, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, "remove")
	return nil
}

func (f *fakeRegistry) snapshot() (events []string, lease time.Duration, heartbeats int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...), f.lease, f.heartbeats
}

// TestSessionLeaseLifecycle 验证租约调用序：预扣成功 Register（TTL=300s）、
// 在飞期心跳、Terminate 后 Remove。
func TestSessionLeaseLifecycle(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 500)
	reg := &fakeRegistry{}
	e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, reg, nil)
	e.heartbeat = 10 * time.Millisecond
	req := testRequestID("lease")
	s := newBillingTestSession(e, userID, req, perCallSnapshot(100))

	if err := s.PreConsume(contract.QuotaEstimate{Model: "m"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	events, lease, _ := reg.snapshot()
	if len(events) == 0 || events[0] != "register" {
		t.Fatalf("events = %v, want first register", events)
	}
	if lease != time.Duration(contract.InflightLeaseTTLSeconds)*time.Second {
		t.Fatalf("lease = %v, want %ds", lease, contract.InflightLeaseTTLSeconds)
	}

	waitHeartbeats(t, reg, 2)
	if err := s.Terminate(contract.UsageFact{Usage: contract.Usage{Prompt: 1, PromptIncludesCache: true}}); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	events, _, heartbeats := reg.snapshot()
	if last := events[len(events)-1]; last != "remove" {
		t.Fatalf("events = %v, want last remove", events)
	}
	if heartbeats < 2 {
		t.Fatalf("heartbeats = %d, want >= 2", heartbeats)
	}
}

func waitHeartbeats(t *testing.T, reg *fakeRegistry, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, _, hb := reg.snapshot()
		if hb >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	_, _, hb := reg.snapshot()
	t.Fatalf("heartbeats = %d, want >= %d（心跳未运行）", hb, want)
}

// recordingSaturator 在调用时记录该 requestID 的流水行数与 outcome。
type recordingSaturator struct {
	db         *sql.DB
	requestID  string
	rowsAtCall int
	called     int
	outcome    SettlementOutcome
	err        error
}

func (r *recordingSaturator) Saturate(_ context.Context, _ *contract.UsageFact,
	_ *contract.RelayContext, outcome SettlementOutcome) error {
	r.called++
	r.outcome = outcome
	var n int
	if err := r.db.QueryRow(
		`SELECT count(*) FROM wallet_transactions WHERE reference_type='usage_log' AND reference_id=$1`,
		r.requestID).Scan(&n); err == nil {
		r.rowsAtCall = n
	}
	return r.err
}

// TestSessionSaturateBeforeLedgerCommit 验证裁决的顺序：Saturator 被调用时
// hold 尚未关闭（只有 1 行 hold），返回后主库才写差额行（共 2 行）。
func TestSessionSaturateBeforeLedgerCommit(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 500)
	req := testRequestID("saturate-order")
	sat := &recordingSaturator{db: db, requestID: req}
	e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, sat)
	s := newBillingTestSession(e, userID, req, ratioSnapshot(100, 100))

	if err := s.PreConsume(contract.QuotaEstimate{Model: "m", PromptTokens: 1_000_000}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	fact := contract.UsageFact{Usage: contract.Usage{
		Prompt: 800_000, PromptIncludesCache: true, Source: contract.UsageUpstreamExact,
	}}
	if err := s.Terminate(fact); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if sat.called != 1 {
		t.Fatalf("Saturate called %d times, want 1", sat.called)
	}
	if sat.rowsAtCall != 1 {
		t.Fatalf("rows at Saturate = %d, want 1（日志必须先于主库结算）", sat.rowsAtCall)
	}
	if sat.outcome.Cost != 80 || sat.outcome.PreDeducted != 100 || sat.outcome.Refunded != 20 {
		t.Fatalf("outcome = %+v, want cost 80 / pre 100 / refunded 20", sat.outcome)
	}
	if sat.outcome.Status != UsageLogStatusSuccess {
		t.Fatalf("status = %d, want %d", sat.outcome.Status, UsageLogStatusSuccess)
	}
	if rows := readWalletRows(t, db, req); len(rows) != 2 {
		t.Fatalf("rows after Terminate = %d, want 2", len(rows))
	}
}

// TestSessionSaturateFailureKeepsHold 验证日志失败不结算：Terminate 返回
// error，hold 原样保留（供 worker 兜底退款），随后 Refund 仍能全额退回。
func TestSessionSaturateFailureKeepsHold(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 500)
	req := testRequestID("saturate-fail")
	sat := &recordingSaturator{db: db, requestID: req, err: errors.New("logs db down")}
	e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, sat)
	s := newBillingTestSession(e, userID, req, ratioSnapshot(100, 100))

	if err := s.PreConsume(contract.QuotaEstimate{Model: "m", PromptTokens: 1_000_000}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	fact := contract.UsageFact{Usage: contract.Usage{Prompt: 800_000, PromptIncludesCache: true}}
	if err := s.Terminate(fact); err == nil {
		t.Fatal("Terminate = nil, want error（日志失败不得结算）")
	}
	if got := userBalance(t, db, userID); got != 400 {
		t.Fatalf("balance = %d, want 400（hold 必须保留）", got)
	}
	rows := readWalletRows(t, db, req)
	if len(rows) != 1 || rows[0].Status != "hold" {
		t.Fatalf("rows = %+v, want 单行 hold", rows)
	}
	if err := s.Refund(); err != nil {
		t.Fatalf("Refund: %v", err)
	}
	if got := userBalance(t, db, userID); got != 500 {
		t.Fatalf("balance = %d, want 500", got)
	}
}

// TestSessionNilSaturator 验证 Saturator 为 nil 时 Terminate 正常工作。
func TestSessionNilSaturator(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 500)
	e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)
	req := testRequestID("nil-saturator")
	s := newBillingTestSession(e, userID, req, perCallSnapshot(100))

	if err := s.PreConsume(contract.QuotaEstimate{Model: "m"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if err := s.Terminate(contract.UsageFact{Usage: contract.Usage{Prompt: 1, PromptIncludesCache: true}}); err != nil {
		t.Fatalf("Terminate with nil saturator: %v", err)
	}
	if got := userBalance(t, db, userID); got != 400 {
		t.Fatalf("balance = %d, want 400", got)
	}
}

// TestSessionReserveOnlyUp 验证 Reserve 仅上浮、降低与零增量 no-op，
// 且每次上浮都产生真实 hold；Terminate 释放多余预留。
func TestSessionReserveOnlyUp(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 500)
	e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)
	req := testRequestID("reserve")
	s := newBillingTestSession(e, userID, req, perCallSnapshot(100))

	if err := s.PreConsume(contract.QuotaEstimate{Model: "m"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if err := s.Reserve(50); err != nil {
		t.Fatalf("Reserve(50): %v", err)
	}
	if err := s.Reserve(0); err != nil {
		t.Fatalf("Reserve(0): %v", err)
	}
	if err := s.Reserve(-10); err != nil {
		t.Fatalf("Reserve(-10): %v", err)
	}
	if got := s.Reserved(); got != 150 {
		t.Fatalf("Reserved = %d, want 150（零/负增量不得改变预留）", got)
	}
	if err := s.Reserve(20); err != nil {
		t.Fatalf("Reserve(20): %v", err)
	}
	if got := s.Reserved(); got != 170 {
		t.Fatalf("Reserved = %d, want 170", got)
	}
	if got := userBalance(t, db, userID); got != 330 {
		t.Fatalf("balance = %d, want 330", got)
	}

	// Terminate：按次实际 100，释放 70。
	if err := s.Terminate(contract.UsageFact{Usage: contract.Usage{Prompt: 1, PromptIncludesCache: true}}); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if got := userBalance(t, db, userID); got != 400 {
		t.Fatalf("balance = %d, want 400", got)
	}
	rows := readWalletRows(t, db, req)
	if len(rows) != 4 { // 三行 hold + 一行 release
		t.Fatalf("rows = %+v, want 4", rows)
	}
	if rows[3].Type != "release" || rows[3].Amount != 70 {
		t.Fatalf("last row = %+v, want release 70", rows[3])
	}
}

// TestSessionTrustedBypass 验证信任旁路：命中时不预扣，但结算仍按真实用量扣费；
// 阈值为 0 时禁用旁路。
func TestSessionTrustedBypass(t *testing.T) {
	t.Run("命中旁路：不预扣，结算少补", func(t *testing.T) {
		gormDB, db := openBillingTestDB(t)
		userID := createBillingTestUser(t, db, 100_000)
		cfg := mapConfig{
			ConfigPreConsumeRatio: json.RawMessage("1.0"),
			ConfigTrustQuotaUSD:   json.RawMessage("10"), // 10 USD × 100 = 1000 积分阈值
		}
		e := NewEngine(gormDB, nil, nil, cfg, nil, nil, nil)
		req := testRequestID("trusted")
		s := newBillingTestSession(e, userID, req, perCallSnapshot(100))

		if err := s.PreConsume(contract.QuotaEstimate{Model: "m"}); err != nil {
			t.Fatalf("PreConsume: %v", err)
		}
		if s.Reserved() != 0 {
			t.Fatalf("Reserved = %d, want 0（信任旁路）", s.Reserved())
		}
		if rows := readWalletRows(t, db, req); len(rows) != 0 {
			t.Fatalf("rows = %d, want 0（旁路不写 hold）", len(rows))
		}
		if err := s.Terminate(contract.UsageFact{Usage: contract.Usage{Prompt: 1, PromptIncludesCache: true}}); err != nil {
			t.Fatalf("Terminate: %v", err)
		}
		if got := userBalance(t, db, userID); got != 99_900 {
			t.Fatalf("balance = %d, want 99900（旁路只免预扣，不免费用）", got)
		}
	})

	t.Run("阈值为 0 禁用旁路", func(t *testing.T) {
		gormDB, db := openBillingTestDB(t)
		userID := createBillingTestUser(t, db, 100_000)
		e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)
		req := testRequestID("trusted-off")
		s := newBillingTestSession(e, userID, req, perCallSnapshot(100))
		if err := s.PreConsume(contract.QuotaEstimate{Model: "m"}); err != nil {
			t.Fatalf("PreConsume: %v", err)
		}
		if s.Reserved() != 100 {
			t.Fatalf("Reserved = %d, want 100", s.Reserved())
		}
	})
}

// TestSessionTrustedImageReserve 验证 trusted 的图片例外：
// 普通请求 Reserve no-op；图片请求清 trusted 并真正预留。
func TestSessionTrustedImageReserve(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 100_000)
	cfg := mapConfig{
		ConfigPreConsumeRatio: json.RawMessage("1.0"),
		ConfigTrustQuotaUSD:   json.RawMessage("10"),
	}
	e := NewEngine(gormDB, nil, nil, cfg, nil, nil, nil)
	req := testRequestID("trusted-image")
	s := newBillingTestSession(e, userID, req, perCallSnapshot(100))

	// 图片请求：ImageCount>0 → imageRequest。
	if err := s.PreConsume(contract.QuotaEstimate{Model: "m", ImageCount: 1}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if s.Reserved() != 0 {
		t.Fatalf("Reserved = %d, want 0（旁路命中）", s.Reserved())
	}
	if err := s.Reserve(50); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if s.Reserved() != 50 {
		t.Fatalf("Reserved = %d, want 50（图片请求必须真正预留）", s.Reserved())
	}
	if got := userBalance(t, db, userID); got != 99_950 {
		t.Fatalf("balance = %d, want 99950", got)
	}
}

// TestSessionFreeModel 验证免费模型：不预扣、结算不收费。
func TestSessionFreeModel(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 500)
	e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)
	req := testRequestID("free")
	snap := perCallSnapshot(100)
	snap.Price.FreeModel = true
	s := newBillingTestSession(e, userID, req, snap)

	if err := s.PreConsume(contract.QuotaEstimate{Model: "m"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if s.Reserved() != 0 {
		t.Fatalf("Reserved = %d, want 0", s.Reserved())
	}
	if err := s.Reserve(100); err != nil {
		t.Fatalf("Reserve on free model: %v", err)
	}
	if s.Reserved() != 0 {
		t.Fatalf("Reserved = %d, want 0（免费模型不上浮）", s.Reserved())
	}
	if err := s.Terminate(contract.UsageFact{Usage: contract.Usage{Prompt: 1000, PromptIncludesCache: true}}); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if got := userBalance(t, db, userID); got != 500 {
		t.Fatalf("balance = %d, want 500（免费模型不收费）", got)
	}
}

// TestSessionDoublePreConsume 验证同一会话二次 PreConsume 返回预算错误。
func TestSessionDoublePreConsume(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 500)
	e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)
	req := testRequestID("double-pre")
	s := newBillingTestSession(e, userID, req, perCallSnapshot(100))

	est := contract.QuotaEstimate{Model: "m"}
	if err := s.PreConsume(est); err != nil {
		t.Fatalf("first PreConsume: %v", err)
	}
	if err := s.PreConsume(est); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("second PreConsume = %v, want ErrBudgetExceeded", err)
	}
	if got := userBalance(t, db, userID); got != 400 {
		t.Fatalf("balance = %d, want 400（第二次不得扣款）", got)
	}
}

// TestSessionConcurrentPreConsume 是 Review Focus 第 1 项：同一用户两个会话
// 并发预扣，只能有一个成功（或两者总和不超过余额），绝不产生负余额。
func TestSessionConcurrentPreConsume(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 100)
	e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)

	reqA := testRequestID("concurrent-a")
	reqB := testRequestID("concurrent-b")
	sA := newBillingTestSession(e, userID, reqA, perCallSnapshot(60))
	sB := newBillingTestSession(e, userID, reqB, perCallSnapshot(60))

	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]error, 2)
	for i, s := range []*Session{sA, sB} {
		wg.Add(1)
		go func(i int, s *Session) {
			defer wg.Done()
			<-start
			results[i] = s.PreConsume(contract.QuotaEstimate{Model: "m"})
		}(i, s)
	}
	close(start)
	wg.Wait()

	successes := 0
	failures := 0
	for _, err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrInsufficientQuota):
			failures++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if successes != 1 || failures != 1 {
		t.Fatalf("successes=%d failures=%d, want 1/1", successes, failures)
	}
	if got := userBalance(t, db, userID); got != 40 {
		t.Fatalf("balance = %d, want 40", got)
	}
	holdRows := 0
	for _, req := range []string{reqA, reqB} {
		for _, r := range readWalletRows(t, db, req) {
			if r.Type == "consume_pre" && r.Status == "hold" {
				holdRows++
			}
		}
	}
	if holdRows != 1 {
		t.Fatalf("hold rows = %d, want 1", holdRows)
	}
}
