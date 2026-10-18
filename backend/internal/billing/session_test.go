package billing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/contract"
)

// newBillingTestSession 构造绑定到固定 requestID 的会话（无令牌）。
func newBillingTestSession(e *Engine, userID int64, requestID string, snap *contract.PriceSnapshot) *Session {
	return newBillingTestSessionWithToken(e, userID, 0, requestID, snap)
}

// newBillingTestSessionWithToken 构造绑定 API 令牌的会话（R-73）。
func newBillingTestSessionWithToken(e *Engine, userID, tokenID int64,
	requestID string, snap *contract.PriceSnapshot) *Session {
	rc := &contract.RelayContext{
		RequestID:     requestID,
		UserID:        int(userID),
		TokenID:       int(tokenID),
		OriginModel:   "test-model",
		UsingGroup:    "default",
		PriceSnapshot: snap,
	}
	return e.NewSession(rc).(*Session)
}

// createBillingTestToken 创建 API 令牌；remain 为 nil 表示不限额度。
func createBillingTestToken(t *testing.T, db *sql.DB, userID int64, remain any) int64 {
	t.Helper()
	var id int64
	err := db.QueryRow(
		`INSERT INTO api_tokens (user_id, name, key_hash, "group", remain_quota)
		 VALUES ($1, 'tok', $2, 'default', $3) RETURNING id`,
		userID, testRequestID("keyhash"), remain).Scan(&id)
	if err != nil {
		t.Fatalf("create test token: %v", err)
	}
	return id
}

// tokenRemain 读取令牌剩余额度（NULL 表示无限额）。
func tokenRemain(t *testing.T, db *sql.DB, tokenID int64) sql.NullInt64 {
	t.Helper()
	var remain sql.NullInt64
	if err := db.QueryRow(`SELECT remain_quota FROM api_tokens WHERE id = $1`, tokenID).
		Scan(&remain); err != nil {
		t.Fatalf("read token remain: %v", err)
	}
	return remain
}

// staticPricingSource 是 pricing.Source 的固定快照实现（Minor-2 用）。
type staticPricingSource struct {
	snap contract.PriceSnapshot
	err  error
}

func (s staticPricingSource) Resolve(context.Context, string, string) (contract.PriceSnapshot, error) {
	return s.snap, s.err
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
	outcomes   []SettlementOutcome
	err        error
	// failFirst > 0 时只让前 failFirst 次调用返回 err，之后成功；
	// 0 表示 err 永远生效（用于「日志库持续不可用」的用例）。
	failFirst int
}

func (r *recordingSaturator) Saturate(_ context.Context, _ *contract.UsageFact,
	_ *contract.RelayContext, outcome SettlementOutcome) error {
	r.called++
	r.outcome = outcome
	r.outcomes = append(r.outcomes, outcome)
	var n int
	if err := r.db.QueryRow(
		`SELECT count(*) FROM wallet_transactions WHERE reference_type='usage_log' AND reference_id=$1`,
		r.requestID).Scan(&n); err == nil {
		r.rowsAtCall = n
	}
	if r.err != nil && (r.failFirst <= 0 || r.called <= r.failFirst) {
		return r.err
	}
	return nil
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
// error，hold 原样保留（供 worker 兜底退款），随后 Refund 仍能全额退回，
// 并把已写出的终态日志通过 Saturator 收敛为 refunded（Minor-3）。
func TestSessionSaturateFailureKeepsHold(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 500)
	req := testRequestID("saturate-fail")
	sat := &recordingSaturator{
		db: db, requestID: req, err: errors.New("logs db down"), failFirst: 1,
	}
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
	if len(sat.outcomes) != 2 {
		t.Fatalf("Saturate calls = %d, want 2（Terminate + Refund 收敛）", len(sat.outcomes))
	}
	if sat.outcomes[1].Status != UsageLogStatusRefunded {
		t.Fatalf("refund log status = %d, want %d", sat.outcomes[1].Status, UsageLogStatusRefunded)
	}
	if sat.outcomes[1].Refunded != 100 {
		t.Fatalf("refund log refunded = %d, want 100", sat.outcomes[1].Refunded)
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

// TestSessionFreeModelNoMaxTokens 验证免费模型在 MaxTokens 缺失时的路径：
// 无从估算放大面 → 不写 hold、结算不收费（有 MaxTokens 的 F-08-12 预扣见
// TestSessionFreeModelPreconsumeEnabled）。
func TestSessionFreeModelNoMaxTokens(t *testing.T) {
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

// TestSessionFreeModelPreconsumeEnabled 覆盖 F-08-12 默认开启的免费模型预扣：
// hold = ceil(MaxTokens × multiplier)，结算 0 价全额退回。
//
// 该用例同时把 billing-00 §6.4 的信任阈值设为 10 USD 且用户余额充足：
// 若两套策略被错误合并，§6.4 旁路会跳过预扣；断言 hold 存在即证明两者独立。
func TestSessionFreeModelPreconsumeEnabled(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 100_000)
	cfg := mapConfig{
		ConfigPreConsumeRatio: json.RawMessage("1.0"),
		ConfigTrustQuotaUSD:   json.RawMessage("10"),
	}
	e := NewEngine(gormDB, nil, nil, cfg, nil, nil, nil)
	req := testRequestID("free-enabled")
	snap := perCallSnapshot(100)
	snap.Price.FreeModel = true
	s := newBillingTestSession(e, userID, req, snap)

	est := contract.QuotaEstimate{Model: "free-model", MaxTokens: intPtr(200)}
	if err := s.PreConsume(est); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if got := s.Reserved(); got != 200 {
		t.Fatalf("Reserved = %d, want 200（ceil(200 × 1.00)）", got)
	}
	if got := userBalance(t, db, userID); got != 99_800 {
		t.Fatalf("balance = %d, want 99800（免费模型也要 hold）", got)
	}
	rows := readWalletRows(t, db, req)
	if len(rows) != 1 || rows[0].Type != "consume_pre" || rows[0].Amount != -200 || rows[0].Status != "hold" {
		t.Fatalf("rows = %+v, want 单行 consume_pre -200 hold", rows)
	}

	// 免费模型结算价 0 → 全额退回。
	if err := s.Terminate(contract.UsageFact{Usage: contract.Usage{Prompt: 50_000, PromptIncludesCache: true}}); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if got := userBalance(t, db, userID); got != 100_000 {
		t.Fatalf("balance = %d, want 100000（免费模型全额退回）", got)
	}
	rows = readWalletRows(t, db, req)
	want := []walletRow{
		{Type: "consume_pre", Amount: -200, BalanceAfter: 99_800, Status: "settled"},
		{Type: "release", Amount: 200, BalanceAfter: 100_000, Status: "confirmed"},
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

// TestSessionFreeModelZeroPricedNotFlagged 覆盖「计算配额 == 0」的免费模型判定：
// PerCall 单价 0（FreeModel 标志未设）同样走免费模型预扣策略。
func TestSessionFreeModelZeroPricedNotFlagged(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 500)
	e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)
	req := testRequestID("free-zero-price")
	s := newBillingTestSession(e, userID, req, perCallSnapshot(0))

	if err := s.PreConsume(contract.QuotaEstimate{Model: "zero", MaxTokens: intPtr(50)}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if got := s.Reserved(); got != 50 {
		t.Fatalf("Reserved = %d, want 50（零价模型按 max_tokens 预扣）", got)
	}
	if err := s.Terminate(contract.UsageFact{Usage: contract.Usage{Prompt: 1, PromptIncludesCache: true}}); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if got := userBalance(t, db, userID); got != 500 {
		t.Fatalf("balance = %d, want 500", got)
	}
}

// TestSessionFreeModelTrustSkip 覆盖可信用户阈值：
// Σ type='consume' ≥ trust_credits 时跳过免费模型预扣。
func TestSessionFreeModelTrustSkip(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 500)
	cfg := mapConfig{
		ConfigPreConsumeRatio:                 json.RawMessage("1.0"),
		ConfigTrustQuotaUSD:                   json.RawMessage("0"),
		ConfigFreeModelPreconsumeTrustCredits: json.RawMessage("100"),
	}
	e := NewEngine(gormDB, nil, nil, cfg, nil, nil, nil)

	// 历史累计消费恰好等于阈值（100）。
	if _, err := e.ledger.ApplyDelta(context.Background(), userID, -100,
		"consume", "confirmed", "usage_log", testRequestID("seed")); err != nil {
		t.Fatalf("seed consume: %v", err)
	}

	req := testRequestID("free-trust")
	snap := perCallSnapshot(0)
	snap.Price.FreeModel = true
	s := newBillingTestSession(e, userID, req, snap)
	if err := s.PreConsume(contract.QuotaEstimate{Model: "free", MaxTokens: intPtr(200)}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if got := s.Reserved(); got != 0 {
		t.Fatalf("Reserved = %d, want 0（可信用户跳过预扣）", got)
	}
	if got := userBalance(t, db, userID); got != 400 {
		t.Fatalf("balance = %d, want 400（仅种子消费）", got)
	}
	if rows := readWalletRows(t, db, req); len(rows) != 0 {
		t.Fatalf("rows = %d, want 0", len(rows))
	}
	if err := s.Terminate(contract.UsageFact{Usage: contract.Usage{Prompt: 1, PromptIncludesCache: true}}); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if got := userBalance(t, db, userID); got != 400 {
		t.Fatalf("balance = %d, want 400（免费模型结算 0）", got)
	}
}

// TestSessionFreeModelDisabled 覆盖开关关闭：不产生 hold。
func TestSessionFreeModelDisabled(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 500)
	cfg := mapConfig{
		ConfigPreConsumeRatio:            json.RawMessage("1.0"),
		ConfigTrustQuotaUSD:              json.RawMessage("0"),
		ConfigFreeModelPreconsumeEnabled: json.RawMessage("false"),
	}
	e := NewEngine(gormDB, nil, nil, cfg, nil, nil, nil)
	req := testRequestID("free-disabled")
	snap := perCallSnapshot(0)
	snap.Price.FreeModel = true
	s := newBillingTestSession(e, userID, req, snap)

	if err := s.PreConsume(contract.QuotaEstimate{Model: "free", MaxTokens: intPtr(200)}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if s.Reserved() != 0 {
		t.Fatalf("Reserved = %d, want 0（开关关闭）", s.Reserved())
	}
	if rows := readWalletRows(t, db, req); len(rows) != 0 {
		t.Fatalf("rows = %d, want 0", len(rows))
	}
}

// TestSessionFreeModelMultiplier 覆盖上浮系数的 decimal 取整（ceil）与非法值拒绝。
func TestSessionFreeModelMultiplier(t *testing.T) {
	t.Run("1.5 × 101 = ceil(151.5) = 152", func(t *testing.T) {
		gormDB, db := openBillingTestDB(t)
		userID := createBillingTestUser(t, db, 500)
		cfg := mapConfig{
			ConfigPreConsumeRatio:               json.RawMessage("1.0"),
			ConfigTrustQuotaUSD:                 json.RawMessage("0"),
			ConfigFreeModelPreconsumeMultiplier: json.RawMessage("1.5"),
		}
		e := NewEngine(gormDB, nil, nil, cfg, nil, nil, nil)
		req := testRequestID("free-mult")
		snap := perCallSnapshot(0)
		snap.Price.FreeModel = true
		s := newBillingTestSession(e, userID, req, snap)
		if err := s.PreConsume(contract.QuotaEstimate{Model: "free", MaxTokens: intPtr(101)}); err != nil {
			t.Fatalf("PreConsume: %v", err)
		}
		if got := s.Reserved(); got != 152 {
			t.Fatalf("Reserved = %d, want 152", got)
		}
	})

	for _, bad := range []string{"0", "-1", `"NaN"`} {
		t.Run("非法系数 "+bad, func(t *testing.T) {
			gormDB, db := openBillingTestDB(t)
			userID := createBillingTestUser(t, db, 500)
			cfg := mapConfig{
				ConfigPreConsumeRatio:               json.RawMessage("1.0"),
				ConfigTrustQuotaUSD:                 json.RawMessage("0"),
				ConfigFreeModelPreconsumeMultiplier: json.RawMessage(bad),
			}
			e := NewEngine(gormDB, nil, nil, cfg, nil, nil, nil)
			snap := perCallSnapshot(0)
			snap.Price.FreeModel = true
			s := newBillingTestSession(e, userID, testRequestID("free-bad-mult"), snap)
			err := s.PreConsume(contract.QuotaEstimate{Model: "free", MaxTokens: intPtr(100)})
			if !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("PreConsume = %v, want ErrInvalidConfig", err)
			}
			if rows := readWalletRows(t, db, s.requestID); len(rows) != 0 {
				t.Fatalf("rows = %d, want 0", len(rows))
			}
		})
	}
}

// TestSessionFreeModelWorkerRecovery 覆盖残留 hold 的 worker 兜底退款：
// 免费模型 hold 在请求崩溃后由 RecoverAbandoned 全额退回（hold→expired）。
func TestSessionFreeModelWorkerRecovery(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 500)
	e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)
	req := testRequestID("free-recover")
	snap := perCallSnapshot(0)
	snap.Price.FreeModel = true
	s := newBillingTestSession(e, userID, req, snap)

	if err := s.PreConsume(contract.QuotaEstimate{Model: "free", MaxTokens: intPtr(200)}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	worker := newBillingTestSession(e, userID, testRequestID("free-worker"), perCallSnapshot(1))
	if err := worker.RecoverAbandoned(req); err != nil {
		t.Fatalf("RecoverAbandoned: %v", err)
	}
	if got := userBalance(t, db, userID); got != 500 {
		t.Fatalf("balance = %d, want 500", got)
	}
	rows := readWalletRows(t, db, req)
	if len(rows) != 2 || rows[0].Status != "expired" || rows[1].Type != "release" {
		t.Fatalf("rows = %+v, want expired + release", rows)
	}
}

// TestSessionTieredZeroEstimateIsNotFree 是 R-24「计算配额为 0」的安全边界：
// tiered 表达式的估算 P=0 只说明本次估算为 0，不说明模型免费；否则真实用量
// 会在结算时被按 0 收费。
func TestSessionTieredZeroEstimateIsNotFree(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 500)
	e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)
	req := testRequestID("tiered-zero")
	snap := &contract.PriceSnapshot{
		Mode:                 contract.BillingModeTieredExpr,
		ExprString:           "p * 1",
		GroupRatio:           1,
		PreConsumeMultiplier: 1,
	}
	s := newBillingTestSession(e, userID, req, snap)

	// 估算 P=0 → 预算 0，不写 hold；但请求不是免费模型。
	if err := s.PreConsume(contract.QuotaEstimate{Model: "tiered", MaxTokens: intPtr(100)}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if s.Reserved() != 0 {
		t.Fatalf("Reserved = %d, want 0", s.Reserved())
	}
	// 真实用量 1M tokens：表达式 p*1 → 1M/1e6 × QPU(100) = 100 积分。
	fact := contract.UsageFact{Usage: contract.Usage{
		Prompt: 1_000_000, PromptIncludesCache: true, Source: contract.UsageUpstreamExact,
	}}
	if err := s.Terminate(fact); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if got := userBalance(t, db, userID); got != 400 {
		t.Fatalf("balance = %d, want 400（真实用量必须收费）", got)
	}
}

func intPtr(n int) *int { return &n }

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

// TestSessionTieredEmptyUsageBillsUsageIndependentTerms 是 C1 的核心回归：
// Usage 为空不等于「无计费用量」。表达式里的 image_count、固定价、常量项
// 仍必须求值；会话层不得用提前返回把整笔费用退掉。
func TestSessionTieredEmptyUsageBillsUsageIndependentTerms(t *testing.T) {
	t.Run("image_count 表达式在空 usage 下按采信张数计费", func(t *testing.T) {
		gormDB, db := openBillingTestDB(t)
		userID := createBillingTestUser(t, db, 500)
		req := testRequestID("tiered-image-empty")
		sat := &recordingSaturator{db: db, requestID: req}
		e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, sat)
		snap := &contract.PriceSnapshot{
			Mode:                 contract.BillingModeTieredExpr,
			ExprString:           "image_count * 10000",
			GroupRatio:           1,
			PreConsumeMultiplier: 1,
		}
		s := newBillingTestSession(e, userID, req, snap)

		// 请求域声明 2 张：预扣 = 20000/1e6 × QPU(100) = 2。
		if err := s.PreConsume(contract.QuotaEstimate{Model: "img", ImageCount: 2}); err != nil {
			t.Fatalf("PreConsume: %v", err)
		}
		if s.Reserved() != 2 {
			t.Fatalf("Reserved = %d, want 2", s.Reserved())
		}
		// 结算事实的 Usage 为空，但张数采信为 2：仍应计 2。
		fact := contract.UsageFact{
			Usage:      contract.Usage{PromptIncludesCache: true, Source: contract.UsageUpstreamExact},
			ImageCount: 2,
		}
		if err := s.Terminate(fact); err != nil {
			t.Fatalf("Terminate: %v", err)
		}
		if sat.outcome.Cost != 2 {
			t.Fatalf("outcome.Cost = %d, want 2（空 usage 不得吞掉 image_count 项）", sat.outcome.Cost)
		}
		if got := userBalance(t, db, userID); got != 498 {
			t.Fatalf("balance = %d, want 498", got)
		}
	})

	t.Run("常量表达式在空 usage 下正常结算", func(t *testing.T) {
		gormDB, db := openBillingTestDB(t)
		userID := createBillingTestUser(t, db, 500)
		req := testRequestID("tiered-const-empty")
		sat := &recordingSaturator{db: db, requestID: req}
		e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, sat)
		snap := &contract.PriceSnapshot{
			Mode:                 contract.BillingModeTieredExpr,
			ExprString:           "10000",
			GroupRatio:           1,
			PreConsumeMultiplier: 1,
		}
		s := newBillingTestSession(e, userID, req, snap)

		// 10000/1e6 × 100 = 1（正成本取整为 0 时由定价层保底 1）。
		if err := s.PreConsume(contract.QuotaEstimate{Model: "const"}); err != nil {
			t.Fatalf("PreConsume: %v", err)
		}
		if s.Reserved() != 1 {
			t.Fatalf("Reserved = %d, want 1", s.Reserved())
		}
		fact := contract.UsageFact{Usage: contract.Usage{PromptIncludesCache: true}}
		if err := s.Terminate(fact); err != nil {
			t.Fatalf("Terminate: %v", err)
		}
		if sat.outcome.Cost != 1 {
			t.Fatalf("outcome.Cost = %d, want 1", sat.outcome.Cost)
		}
		if got := userBalance(t, db, userID); got != 499 {
			t.Fatalf("balance = %d, want 499", got)
		}
	})
}

// TestSessionTrustedBypassRequiresBudgetCoverage 是 C2 的回归：
// 余额超阈但覆盖不了本次估算预留时不得旁路，必须走正常预扣返回 402。
func TestSessionTrustedBypassRequiresBudgetCoverage(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 1500) // > 阈值 1000，但 < 估算 2000
	cfg := mapConfig{
		ConfigPreConsumeRatio: json.RawMessage("1.0"),
		ConfigTrustQuotaUSD:   json.RawMessage("10"),
	}
	e := NewEngine(gormDB, nil, nil, cfg, nil, nil, nil)
	req := testRequestID("trust-budget")
	s := newBillingTestSession(e, userID, req, perCallSnapshot(2000))

	err := s.PreConsume(contract.QuotaEstimate{Model: "m"})
	if !errors.Is(err, ErrInsufficientQuota) {
		t.Fatalf("PreConsume = %v, want ErrInsufficientQuota（不得旁路）", err)
	}
	if s.Reserved() != 0 {
		t.Fatalf("Reserved = %d, want 0", s.Reserved())
	}
	if rows := readWalletRows(t, db, req); len(rows) != 0 {
		t.Fatalf("rows = %d, want 0", len(rows))
	}
	if got := userBalance(t, db, userID); got != 1500 {
		t.Fatalf("balance = %d, want 1500", got)
	}
}

// TestSessionTrustForcedPreConsume 覆盖条件①：任务/固定价路径强制预扣，
// 即使余额与令牌都满足其余条件也不得旁路。
func TestSessionTrustForcedPreConsume(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 100_000)
	cfg := mapConfig{
		ConfigPreConsumeRatio: json.RawMessage("1.0"),
		ConfigTrustQuotaUSD:   json.RawMessage("10"),
	}
	e := NewEngine(gormDB, nil, nil, cfg, nil, nil, nil)
	req := testRequestID("trust-forced")
	s := newBillingTestSession(e, userID, req, perCallSnapshot(100))

	est := contract.QuotaEstimate{Model: "task", TaskDuration: 60}
	if err := s.PreConsume(est); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if s.Reserved() != 100 {
		t.Fatalf("Reserved = %d, want 100（任务路径必须预扣）", s.Reserved())
	}
}

// TestSessionRefundIncludesChargeIncrement 是 I1 的回归：
// Refund 必须连 ChargeIncrement 的实际消耗一起全额退回，且只退一次。
func TestSessionRefundIncludesChargeIncrement(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 500)
	e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)
	req := testRequestID("refund-increment")
	s := newBillingTestSession(e, userID, req, perCallSnapshot(100))

	if err := s.PreConsume(contract.QuotaEstimate{Model: "m"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if err := s.ChargeIncrement(30, contract.UsageFact{}); err != nil {
		t.Fatalf("ChargeIncrement: %v", err)
	}
	if got := userBalance(t, db, userID); got != 370 {
		t.Fatalf("balance = %d, want 370", got)
	}
	if !s.NeedsRefund() {
		t.Fatal("NeedsRefund = false, want true（存在 hold + 增量）")
	}
	if err := s.Refund(); err != nil {
		t.Fatalf("Refund: %v", err)
	}
	if got := userBalance(t, db, userID); got != 500 {
		t.Fatalf("balance = %d, want 500（hold 100 + 增量 30 全额退回）", got)
	}
	if s.NeedsRefund() {
		t.Fatal("NeedsRefund = true after Refund, want false")
	}
	rows := readWalletRows(t, db, req)
	want := []walletRow{
		{Type: "consume_pre", Amount: -100, BalanceAfter: 400, Status: "released"},
		{Type: "consume", Amount: -30, BalanceAfter: 370, Status: "confirmed"},
		{Type: "release", Amount: 130, BalanceAfter: 500, Status: "confirmed"},
	}
	if len(rows) != len(want) {
		t.Fatalf("rows = %+v, want %+v", rows, want)
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Fatalf("row[%d] = %+v, want %+v", i, rows[i], want[i])
		}
	}
	// 重复退款不得再回补。
	if err := s.Refund(); err != nil {
		t.Fatalf("second Refund: %v", err)
	}
	if got := userBalance(t, db, userID); got != 500 {
		t.Fatalf("balance = %d, want 500（不得重复退款）", got)
	}
}

// TestSessionSettlementExactZeroImageCount 是 I2 的结算域有效 0 张回归：
// 采信 ImageCount=0 时 image_count 表达式计 0，hold 全额释放。
func TestSessionSettlementExactZeroImageCount(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 500)
	req := testRequestID("settle-zero-image")
	sat := &recordingSaturator{db: db, requestID: req}
	e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, sat)
	snap := &contract.PriceSnapshot{
		Mode:                 contract.BillingModeTieredExpr,
		ExprString:           "image_count * 10000",
		GroupRatio:           1,
		PreConsumeMultiplier: 1,
	}
	s := newBillingTestSession(e, userID, req, snap)

	if err := s.PreConsume(contract.QuotaEstimate{Model: "img", ImageCount: 2}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if s.Reserved() != 2 {
		t.Fatalf("Reserved = %d, want 2", s.Reserved())
	}
	// 上游确认 0 张：结算域有效 0 → 计 0。
	fact := contract.UsageFact{
		Usage:      contract.Usage{PromptIncludesCache: true, Source: contract.UsageUpstreamExact},
		ImageCount: 0,
	}
	if err := s.Terminate(fact); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if sat.outcome.Cost != 0 {
		t.Fatalf("outcome.Cost = %d, want 0（有效 0 张计 0）", sat.outcome.Cost)
	}
	if got := userBalance(t, db, userID); got != 500 {
		t.Fatalf("balance = %d, want 500", got)
	}
	rows := readWalletRows(t, db, req)
	if len(rows) != 2 || rows[0].Status != "settled" || rows[1].Amount != 2 {
		t.Fatalf("rows = %+v, want settled + release 2", rows)
	}
}

// TestSessionNoHoldSettlementNeedsFundsBeforeLog 是 I3 的回归：
// 无 hold 且需要补扣时，扣款失败不得写 success 日志（无 worker 回收锚点）。
func TestSessionNoHoldSettlementNeedsFundsBeforeLog(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 50)
	req := testRequestID("no-hold-insufficient")
	sat := &recordingSaturator{db: db, requestID: req}
	e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, sat)
	s := newBillingTestSession(e, userID, req, perCallSnapshot(100))

	// 故意不 PreConsume：无 hold，结算需要补扣 100。
	fact := contract.UsageFact{Usage: contract.Usage{Prompt: 1, PromptIncludesCache: true}}
	err := s.Terminate(fact)
	if !errors.Is(err, ErrInsufficientQuota) {
		t.Fatalf("Terminate = %v, want ErrInsufficientQuota", err)
	}
	if sat.called != 0 {
		t.Fatalf("Saturate called %d times, want 0（扣款失败不得写成功日志）", sat.called)
	}
	if rows := readWalletRows(t, db, req); len(rows) != 0 {
		t.Fatalf("rows = %d, want 0", len(rows))
	}
	if got := userBalance(t, db, userID); got != 50 {
		t.Fatalf("balance = %d, want 50", got)
	}
}

// TestSessionConcurrentTerminateRefund 验证 Terminate 与 Refund 互斥：
// 只允许一个资金动作生效，另一个幂等 no-op。
func TestSessionConcurrentTerminateRefund(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 500)
	e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)
	req := testRequestID("concurrent-close")
	s := newBillingTestSession(e, userID, req, perCallSnapshot(100))

	if err := s.PreConsume(contract.QuotaEstimate{Model: "m"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}

	start := make(chan struct{})
	errs := make([]error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		errs[0] = s.Terminate(contract.UsageFact{Usage: contract.Usage{Prompt: 1, PromptIncludesCache: true}})
	}()
	go func() {
		defer wg.Done()
		<-start
		errs[1] = s.Refund()
	}()
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("op %d: %v", i, err)
		}
	}
	balance := userBalance(t, db, userID)
	rows := readWalletRows(t, db, req)
	switch balance {
	case 400: // Terminate 赢：hold settled，按次扣 100。
		if len(rows) != 1 || rows[0].Status != "settled" {
			t.Fatalf("balance 400 but rows = %+v", rows)
		}
	case 500: // Refund 赢：hold released + release 100。
		if len(rows) != 2 || rows[0].Status != "released" || rows[1].Amount != 100 {
			t.Fatalf("balance 500 but rows = %+v", rows)
		}
	default:
		t.Fatalf("balance = %d, want 400（结算赢）或 500（退款赢）", balance)
	}
	if s.NeedsRefund() {
		t.Fatal("NeedsRefund = true after close, want false")
	}
}

// TestSessionChargeIncrementBounds 验证增量扣减的边界：非正增量 no-op，
// 超出余额的增量返回额度不足且不落流水。
func TestSessionChargeIncrementBounds(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 500)
	e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)
	req := testRequestID("increment-bounds")
	s := newBillingTestSession(e, userID, req, perCallSnapshot(100))

	if err := s.PreConsume(contract.QuotaEstimate{Model: "m"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	for _, delta := range []int64{0, -5} {
		if err := s.ChargeIncrement(delta, contract.UsageFact{}); err != nil {
			t.Fatalf("ChargeIncrement(%d) = %v, want nil", delta, err)
		}
	}
	if err := s.ChargeIncrement(math.MaxInt64, contract.UsageFact{}); !errors.Is(err, ErrInsufficientQuota) {
		t.Fatalf("ChargeIncrement(MaxInt64) = %v, want ErrInsufficientQuota", err)
	}
	if got := userBalance(t, db, userID); got != 400 {
		t.Fatalf("balance = %d, want 400（非正/超额增量不得扣款）", got)
	}
	if rows := readWalletRows(t, db, req); len(rows) != 1 {
		t.Fatalf("rows = %d, want 1（只有预扣行）", len(rows))
	}
}

// TestSessionTieredPreConsumeAndSettleNormalizes 验证 tiered 模式下预扣与
// 结算都经过 NormalizeTokens：预扣 C=0，结算按 usedVars 扣除 cr。
func TestSessionTieredPreConsumeAndSettleNormalizes(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 500)
	e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)
	req := testRequestID("tiered-normalize")
	snap := &contract.PriceSnapshot{
		Mode:                 contract.BillingModeTieredExpr,
		ExprString:           "p * 1 + cr * 2",
		GroupRatio:           1,
		PreConsumeMultiplier: 1,
	}
	s := newBillingTestSession(e, userID, req, snap)

	// 估算 P=1M → 1M/1e6 × QPU(100) = 100。
	if err := s.PreConsume(contract.QuotaEstimate{Model: "m", PromptTokens: 1_000_000}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if s.Reserved() != 100 {
		t.Fatalf("Reserved = %d, want 100", s.Reserved())
	}
	// 真实用量：prompt 1M 含 cache 400k → P=600k、CR=400k；
	// out = 600k + 800k = 1.4M → 140 积分，少补 40。
	fact := contract.UsageFact{Usage: contract.Usage{
		Prompt: 1_000_000, PromptIncludesCache: true,
		CacheRead: 400_000, Source: contract.UsageUpstreamExact,
	}}
	if err := s.Terminate(fact); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if got := userBalance(t, db, userID); got != 360 {
		t.Fatalf("balance = %d, want 360（100 预扣 + 40 少补）", got)
	}
}

// walletNotes 返回某 requestID 的流水 note（按 id 升序，NULL 归一为空串）。
func walletNotes(t *testing.T, db *sql.DB, requestID string) []string {
	t.Helper()
	rows, err := db.Query(
		`SELECT COALESCE(note, '') FROM wallet_transactions
		 WHERE reference_type = 'usage_log' AND reference_id = $1 ORDER BY id`, requestID)
	if err != nil {
		t.Fatalf("read wallet notes: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var note string
		if err := rows.Scan(&note); err != nil {
			t.Fatalf("scan note: %v", err)
		}
		out = append(out, note)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate notes: %v", err)
	}
	return out
}

// TestSessionPaidRatioZeroEstimateNotFree 是 R-40 的核心回归：
// 付费 Ratio 模型在估算 P=0（CountToken 关闭等）时不得被判为免费，
// 结算必须按真实用量收费。
func TestSessionPaidRatioZeroEstimateNotFree(t *testing.T) {
	t.Run("无增量：结算少补 100", func(t *testing.T) {
		gormDB, db := openBillingTestDB(t)
		userID := createBillingTestUser(t, db, 500)
		req := testRequestID("paid-zero-estimate")
		sat := &recordingSaturator{db: db, requestID: req}
		e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, sat)
		s := newBillingTestSession(e, userID, req, ratioSnapshot(100, 100))

		// 估算 P=0：预算 0、不写 hold，但模型仍是付费模型。
		if err := s.PreConsume(contract.QuotaEstimate{Model: "paid"}); err != nil {
			t.Fatalf("PreConsume: %v", err)
		}
		if s.Reserved() != 0 {
			t.Fatalf("Reserved = %d, want 0", s.Reserved())
		}
		if s.freeModel {
			t.Fatal("session classified as free, want paid")
		}
		fact := contract.UsageFact{Usage: contract.Usage{
			Prompt: 1_000_000, PromptIncludesCache: true, Source: contract.UsageUpstreamExact,
		}}
		if err := s.Terminate(fact); err != nil {
			t.Fatalf("Terminate: %v", err)
		}
		if sat.outcome.Cost != 100 {
			t.Fatalf("outcome.Cost = %d, want 100（真实用量必须收费）", sat.outcome.Cost)
		}
		if got := userBalance(t, db, userID); got != 400 {
			t.Fatalf("balance = %d, want 400", got)
		}
	})

	t.Run("实时增量：先扣 30 再补 70", func(t *testing.T) {
		gormDB, db := openBillingTestDB(t)
		userID := createBillingTestUser(t, db, 500)
		req := testRequestID("paid-zero-estimate-increment")
		e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)
		s := newBillingTestSession(e, userID, req, ratioSnapshot(100, 100))

		if err := s.PreConsume(contract.QuotaEstimate{Model: "paid"}); err != nil {
			t.Fatalf("PreConsume: %v", err)
		}
		if err := s.ChargeIncrement(30, contract.UsageFact{}); err != nil {
			t.Fatalf("ChargeIncrement: %v", err)
		}
		fact := contract.UsageFact{Usage: contract.Usage{
			Prompt: 1_000_000, PromptIncludesCache: true, Source: contract.UsageUpstreamExact,
		}}
		if err := s.Terminate(fact); err != nil {
			t.Fatalf("Terminate: %v", err)
		}
		if got := userBalance(t, db, userID); got != 400 {
			t.Fatalf("balance = %d, want 400（总扣 100）", got)
		}
		notes := walletNotes(t, db, req)
		want := []string{"", "billing:settle"} // 增量行无标记；结算行带结案标记
		if len(notes) != len(want) {
			t.Fatalf("notes = %v, want %v", notes, want)
		}
		for i := range want {
			if notes[i] != want[i] {
				t.Fatalf("note[%d] = %q, want %q", i, notes[i], want[i])
			}
		}
	})
}

// TestSessionZeroPricedRatioStillFree 确认 R-40 的分类器没有误伤真正的
// 零价 Ratio 模型：全部绝对价格字段为 0 时仍走 F-08-12 预扣并全额退回。
func TestSessionZeroPricedRatioStillFree(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 500)
	e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)
	req := testRequestID("zero-price-ratio")
	s := newBillingTestSession(e, userID, req, ratioSnapshot(0, 0))

	if err := s.PreConsume(contract.QuotaEstimate{Model: "free-ratio", MaxTokens: intPtr(100)}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if s.Reserved() != 100 {
		t.Fatalf("Reserved = %d, want 100（零价模型按 max_tokens 预扣）", s.Reserved())
	}
	if !s.freeModel {
		t.Fatal("zero-priced Ratio model not classified as free")
	}
	fact := contract.UsageFact{Usage: contract.Usage{
		Prompt: 1_000_000, PromptIncludesCache: true, Source: contract.UsageUpstreamExact,
	}}
	if err := s.Terminate(fact); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if got := userBalance(t, db, userID); got != 500 {
		t.Fatalf("balance = %d, want 500（零价模型全额退回）", got)
	}
}

// TestSessionTwoSessionsSameRequestIDNoHoldSettleOnce 是 R-41 的顺序重放回归：
// 两个 Session 对象对同一 requestID 依次做无 hold 结算，第二次被
// settlementExists 拦截，不得双扣。
func TestSessionTwoSessionsSameRequestIDNoHoldSettleOnce(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 500)
	e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)
	req := testRequestID("same-req-no-hold")
	sA := newBillingTestSession(e, userID, req, ratioSnapshot(100, 100))
	sB := newBillingTestSession(e, userID, req, ratioSnapshot(100, 100))
	fact := contract.UsageFact{Usage: contract.Usage{
		Prompt: 1_000_000, PromptIncludesCache: true, Source: contract.UsageUpstreamExact,
	}}

	if err := sA.Terminate(fact); err != nil {
		t.Fatalf("first Terminate: %v", err)
	}
	if err := sB.Terminate(fact); err != nil {
		t.Fatalf("second Terminate: %v（幂等应返回 nil）", err)
	}
	if got := userBalance(t, db, userID); got != 400 {
		t.Fatalf("balance = %d, want 400（不得双扣）", got)
	}
	settles := 0
	for _, note := range walletNotes(t, db, req) {
		if note == "billing:settle" {
			settles++
		}
	}
	if settles != 1 {
		t.Fatalf("settlement rows = %d, want 1", settles)
	}
}

// TestSessionTwoSessionsSameRequestIDHoldSettleOnce 验证 hold 路径的 DB CAS
// 锚点：两个 Session 对象并发终结同一 requestID，只结算一次。
func TestSessionTwoSessionsSameRequestIDHoldSettleOnce(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 500)
	e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)
	req := testRequestID("same-req-hold")
	sA := newBillingTestSession(e, userID, req, perCallSnapshot(100))
	sB := newBillingTestSession(e, userID, req, perCallSnapshot(100))
	if err := sA.PreConsume(contract.QuotaEstimate{Model: "m"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	fact := contract.UsageFact{Usage: contract.Usage{Prompt: 1, PromptIncludesCache: true}}

	start := make(chan struct{})
	errs := make([]error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		errs[0] = sA.Terminate(fact)
	}()
	go func() {
		defer wg.Done()
		<-start
		errs[1] = sB.Terminate(fact)
	}()
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("Terminate %d: %v", i, err)
		}
	}
	if got := userBalance(t, db, userID); got != 400 {
		t.Fatalf("balance = %d, want 400（只结算一次）", got)
	}
	rows := readWalletRows(t, db, req)
	if len(rows) != 1 || rows[0].Status != "settled" {
		t.Fatalf("rows = %+v, want 单行 settled", rows)
	}
}

// TestSessionConcurrentNoHoldSettleWithRedisLock 验证 R-41 的并发窗口：
// 两个 Session 对象同时做无 hold 结算，Redis requestID 锁只允许一个进入；
// 另一个要么拿到 ErrSettlementInProgress，要么在锁释放后命中 DB 标记。
func TestSessionConcurrentNoHoldSettleWithRedisLock(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 500)
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	e := NewEngine(gormDB, nil, rdb, billingTestConfig(), nil, nil, nil)
	req := testRequestID("same-req-no-hold-lock")
	sA := newBillingTestSession(e, userID, req, ratioSnapshot(100, 100))
	sB := newBillingTestSession(e, userID, req, ratioSnapshot(100, 100))
	fact := contract.UsageFact{Usage: contract.Usage{
		Prompt: 1_000_000, PromptIncludesCache: true, Source: contract.UsageUpstreamExact,
	}}

	start := make(chan struct{})
	errs := make([]error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		errs[0] = sA.Terminate(fact)
	}()
	go func() {
		defer wg.Done()
		<-start
		errs[1] = sB.Terminate(fact)
	}()
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil && !errors.Is(err, ErrSettlementInProgress) {
			t.Fatalf("Terminate %d = %v, want nil or ErrSettlementInProgress", i, err)
		}
	}
	if got := userBalance(t, db, userID); got != 400 {
		t.Fatalf("balance = %d, want 400（不得双扣）", got)
	}
	settles := 0
	for _, note := range walletNotes(t, db, req) {
		if note == "billing:settle" {
			settles++
		}
	}
	if settles != 1 {
		t.Fatalf("settlement rows = %d, want 1", settles)
	}
}

// TestSessionOutOfBandRecoveryThenReserveFailsClosed 是 Important-1 的回归：
// 请求被 out-of-band 兜底退款后，仍存活的会话不得再创建/扩展 hold 或增量
// 扣减；否则新 hold 永远无法再退（wallet_release_refid_uq 23505 wedge）。
func TestSessionOutOfBandRecoveryThenReserveFailsClosed(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 500)
	e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)
	req := testRequestID("oob-recover")
	s := newBillingTestSession(e, userID, req, perCallSnapshot(100))

	// 图片请求：使 Reserve 的第二段上浮是合法路径。
	if err := s.PreConsume(contract.QuotaEstimate{Model: "m", ImageCount: 1}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	worker := newBillingTestSession(e, userID, testRequestID("worker"), perCallSnapshot(1))
	if err := worker.RecoverAbandoned(req); err != nil {
		t.Fatalf("RecoverAbandoned: %v", err)
	}
	if got := userBalance(t, db, userID); got != 500 {
		t.Fatalf("balance = %d, want 500（兜底已退款）", got)
	}

	if err := s.Reserve(50); !errors.Is(err, ErrRequestClosed) {
		t.Fatalf("Reserve after recovery = %v, want ErrRequestClosed", err)
	}
	if s.Reserved() != 0 {
		t.Fatalf("Reserved = %d, want 0（不得创建新 hold）", s.Reserved())
	}
	if err := s.Refund(); err != nil {
		t.Fatalf("Refund after terminal = %v, want nil", err)
	}

	// 另一个同 requestID 的会话也不得再增量扣减。
	s2 := newBillingTestSession(e, userID, req, perCallSnapshot(100))
	if err := s2.ChargeIncrement(10, contract.UsageFact{}); !errors.Is(err, ErrRequestClosed) {
		t.Fatalf("ChargeIncrement after recovery = %v, want ErrRequestClosed", err)
	}

	if got := userBalance(t, db, userID); got != 500 {
		t.Fatalf("balance = %d, want 500", got)
	}
	if rows := readWalletRows(t, db, req); len(rows) != 2 {
		t.Fatalf("rows = %+v, want 2（hold expired + release，无新行）", rows)
	}
}

// TestLedgerReleaseAbandonedConvergesOnExistingRelease 验证兜底退款在已有
// release 行时的收敛：只关闭 hold 状态，不插入第二行 release（避免 23505）。
func TestLedgerReleaseAbandonedConvergesOnExistingRelease(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 500)
	ledger := NewLedger(gormDB)
	ctx := context.Background()
	req := testRequestID("converge")

	// 构造异常历史状态：先有 release，后又出现新的 hold。
	if _, err := ledger.ApplyDelta(ctx, userID, -100,
		"consume_pre", "hold", "usage_log", req); err != nil {
		t.Fatalf("create hold: %v", err)
	}
	if _, err := ledger.ApplyDelta(ctx, userID, 10,
		"release", "confirmed", "usage_log", req); err != nil {
		t.Fatalf("create pre-existing release: %v", err)
	}

	held, _, err := ledger.releaseAbandoned(ctx, req)
	if err != nil {
		t.Fatalf("releaseAbandoned = %v, want nil（23505 收敛）", err)
	}
	if held != 0 {
		t.Fatalf("held = %d, want 0（没有第二次资金回补）", held)
	}
	if got := userBalance(t, db, userID); got != 410 {
		t.Fatalf("balance = %d, want 410（不重复退款）", got)
	}
	rows := readWalletRows(t, db, req)
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want 2（hold expired + 既有 release）", rows)
	}
	if rows[0].Status != "expired" {
		t.Fatalf("hold status = %q, want expired", rows[0].Status)
	}
}

// TestSessionTokenQuotaLifecycle 覆盖 R-73：令牌额度在预扣/增量/结算/退款
// 全链路上的扣减与回补。
func TestSessionTokenQuotaLifecycle(t *testing.T) {
	t.Run("结算多退：令牌额度回补", func(t *testing.T) {
		gormDB, db := openBillingTestDB(t)
		userID := createBillingTestUser(t, db, 500)
		tokenID := createBillingTestToken(t, db, userID, int64(500))
		e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)
		s := newBillingTestSessionWithToken(e, userID, tokenID,
			testRequestID("token-refund"), ratioSnapshot(100, 100))

		if err := s.PreConsume(contract.QuotaEstimate{Model: "m", PromptTokens: 1_000_000}); err != nil {
			t.Fatalf("PreConsume: %v", err)
		}
		if got := tokenRemain(t, db, tokenID); !got.Valid || got.Int64 != 400 {
			t.Fatalf("token remain = %+v, want 400", got)
		}
		fact := contract.UsageFact{Usage: contract.Usage{Prompt: 800_000, PromptIncludesCache: true}}
		if err := s.Terminate(fact); err != nil {
			t.Fatalf("Terminate: %v", err)
		}
		// 实耗 80 < 预扣 100 → 令牌回补 20。
		if got := tokenRemain(t, db, tokenID); !got.Valid || got.Int64 != 420 {
			t.Fatalf("token remain = %+v, want 420", got)
		}
		if got := userBalance(t, db, userID); got != 420 {
			t.Fatalf("balance = %d, want 420", got)
		}
	})

	t.Run("结算少补：令牌额度补扣", func(t *testing.T) {
		gormDB, db := openBillingTestDB(t)
		userID := createBillingTestUser(t, db, 500)
		tokenID := createBillingTestToken(t, db, userID, int64(500))
		e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)
		s := newBillingTestSessionWithToken(e, userID, tokenID,
			testRequestID("token-supplement"), ratioSnapshot(100, 100))

		if err := s.PreConsume(contract.QuotaEstimate{Model: "m", PromptTokens: 1_000_000}); err != nil {
			t.Fatalf("PreConsume: %v", err)
		}
		fact := contract.UsageFact{Usage: contract.Usage{Prompt: 1_500_000, PromptIncludesCache: true}}
		if err := s.Terminate(fact); err != nil {
			t.Fatalf("Terminate: %v", err)
		}
		if got := tokenRemain(t, db, tokenID); !got.Valid || got.Int64 != 350 {
			t.Fatalf("token remain = %+v, want 350", got)
		}
		if got := userBalance(t, db, userID); got != 350 {
			t.Fatalf("balance = %d, want 350", got)
		}
	})

	t.Run("退款：令牌额度全额回补（含增量）", func(t *testing.T) {
		gormDB, db := openBillingTestDB(t)
		userID := createBillingTestUser(t, db, 500)
		tokenID := createBillingTestToken(t, db, userID, int64(500))
		e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)
		s := newBillingTestSessionWithToken(e, userID, tokenID,
			testRequestID("token-refund-all"), perCallSnapshot(100))

		if err := s.PreConsume(contract.QuotaEstimate{Model: "m"}); err != nil {
			t.Fatalf("PreConsume: %v", err)
		}
		if err := s.ChargeIncrement(30, contract.UsageFact{}); err != nil {
			t.Fatalf("ChargeIncrement: %v", err)
		}
		if got := tokenRemain(t, db, tokenID); !got.Valid || got.Int64 != 370 {
			t.Fatalf("token remain = %+v, want 370", got)
		}
		if err := s.Refund(); err != nil {
			t.Fatalf("Refund: %v", err)
		}
		if got := tokenRemain(t, db, tokenID); !got.Valid || got.Int64 != 500 {
			t.Fatalf("token remain = %+v, want 500", got)
		}
		if got := userBalance(t, db, userID); got != 500 {
			t.Fatalf("balance = %d, want 500", got)
		}
	})
}

// TestSessionTokenQuotaInsufficient 验证令牌额度不足 → 402 类错误，
// 钱包与令牌都不变动、不写流水。
func TestSessionTokenQuotaInsufficient(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 500)
	tokenID := createBillingTestToken(t, db, userID, int64(50))
	e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)
	req := testRequestID("token-insufficient")
	s := newBillingTestSessionWithToken(e, userID, tokenID, req, perCallSnapshot(100))

	err := s.PreConsume(contract.QuotaEstimate{Model: "m"})
	if !errors.Is(err, ErrInsufficientQuota) {
		t.Fatalf("PreConsume = %v, want ErrInsufficientQuota", err)
	}
	var qe *QuotaError
	if !errors.As(err, &qe) || qe.Required != 100 || qe.Available != 50 {
		t.Fatalf("QuotaError = %+v", qe)
	}
	if got := tokenRemain(t, db, tokenID); !got.Valid || got.Int64 != 50 {
		t.Fatalf("token remain = %+v, want 50", got)
	}
	if got := userBalance(t, db, userID); got != 500 {
		t.Fatalf("balance = %d, want 500", got)
	}
	if rows := readWalletRows(t, db, req); len(rows) != 0 {
		t.Fatalf("rows = %d, want 0", len(rows))
	}
}

// TestSessionTokenQuotaWalletRollback 验证钱包失败时令牌扣减整体回滚。
func TestSessionTokenQuotaWalletRollback(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 50) // 钱包不足
	tokenID := createBillingTestToken(t, db, userID, int64(500))
	e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)
	req := testRequestID("token-wallet-rollback")
	s := newBillingTestSessionWithToken(e, userID, tokenID, req, perCallSnapshot(100))

	err := s.PreConsume(contract.QuotaEstimate{Model: "m"})
	if !errors.Is(err, ErrInsufficientQuota) {
		t.Fatalf("PreConsume = %v, want ErrInsufficientQuota", err)
	}
	if got := tokenRemain(t, db, tokenID); !got.Valid || got.Int64 != 500 {
		t.Fatalf("token remain = %+v, want 500（令牌扣减必须回滚）", got)
	}
	if got := userBalance(t, db, userID); got != 50 {
		t.Fatalf("balance = %d, want 50", got)
	}
	if rows := readWalletRows(t, db, req); len(rows) != 0 {
		t.Fatalf("rows = %d, want 0", len(rows))
	}
}

// TestSessionTokenQuotaUnlimited 验证 NULL remain_quota = 无限额：
// 预扣/结算/退款都不改该列。
func TestSessionTokenQuotaUnlimited(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 500)
	tokenID := createBillingTestToken(t, db, userID, nil)
	e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)
	s := newBillingTestSessionWithToken(e, userID, tokenID,
		testRequestID("token-unlimited"), perCallSnapshot(100))

	if err := s.PreConsume(contract.QuotaEstimate{Model: "m"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if err := s.Refund(); err != nil {
		t.Fatalf("Refund: %v", err)
	}
	if got := tokenRemain(t, db, tokenID); got.Valid {
		t.Fatalf("token remain = %+v, want NULL（无限额不落值）", got)
	}
	if got := userBalance(t, db, userID); got != 500 {
		t.Fatalf("balance = %d, want 500", got)
	}
}

// TestSessionTokenQuotaTrustBypass 验证信任旁路/零预算不扣令牌额度。
func TestSessionTokenQuotaTrustBypass(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 100_000)
	tokenID := createBillingTestToken(t, db, userID, int64(5000)) // > 信任阈值 1000
	cfg := mapConfig{
		ConfigPreConsumeRatio: json.RawMessage("1.0"),
		ConfigTrustQuotaUSD:   json.RawMessage("10"),
	}
	e := NewEngine(gormDB, nil, nil, cfg, nil, nil, nil)
	s := newBillingTestSessionWithToken(e, userID, tokenID,
		testRequestID("token-trusted"), perCallSnapshot(100))

	if err := s.PreConsume(contract.QuotaEstimate{Model: "m"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if s.Reserved() != 0 {
		t.Fatalf("Reserved = %d, want 0（信任旁路）", s.Reserved())
	}
	if got := tokenRemain(t, db, tokenID); !got.Valid || got.Int64 != 5000 {
		t.Fatalf("token remain = %+v, want 5000（旁路不扣令牌）", got)
	}
}

// TestSessionTokenQuotaConcurrent 验证令牌额度的并发扣减串行化：
// 令牌 100、两会话各 60 → 只一个成功，令牌剩 40，钱包也只扣一笔。
func TestSessionTokenQuotaConcurrent(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 500)
	tokenID := createBillingTestToken(t, db, userID, int64(100))
	e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)
	sA := newBillingTestSessionWithToken(e, userID, tokenID,
		testRequestID("token-concurrent-a"), perCallSnapshot(60))
	sB := newBillingTestSessionWithToken(e, userID, tokenID,
		testRequestID("token-concurrent-b"), perCallSnapshot(60))

	start := make(chan struct{})
	results := make([]error, 2)
	var wg sync.WaitGroup
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

	successes, quotaFailures := 0, 0
	for _, err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrInsufficientQuota):
			quotaFailures++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if successes != 1 || quotaFailures != 1 {
		t.Fatalf("successes=%d failures=%d, want 1/1", successes, quotaFailures)
	}
	if got := tokenRemain(t, db, tokenID); !got.Valid || got.Int64 != 40 {
		t.Fatalf("token remain = %+v, want 40", got)
	}
	if got := userBalance(t, db, userID); got != 440 {
		t.Fatalf("balance = %d, want 440", got)
	}
}

// TestSessionAdoptsResolvedSnapshot 验证 Minor-2：引擎自行解析快照时回填
// rc.PriceSnapshot，供 Saturator 读取 billing_mode/表达式元信息。
func TestSessionAdoptsResolvedSnapshot(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 500)
	rc := &contract.RelayContext{
		RequestID:   testRequestID("adopt"),
		UserID:      int(userID),
		OriginModel: "m",
		UsingGroup:  "default",
	}
	src := staticPricingSource{snap: *perCallSnapshot(100)}
	e := NewEngine(gormDB, nil, nil, billingTestConfig(), src, nil, nil)
	s := e.NewSession(rc).(*Session)

	if err := s.PreConsume(contract.QuotaEstimate{Model: "m"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if rc.PriceSnapshot == nil {
		t.Fatal("rc.PriceSnapshot = nil, want adopted snapshot")
	}
	if rc.PriceSnapshot.Price.ModelPrice != 100 {
		t.Fatalf("adopted ModelPrice = %v, want 100", rc.PriceSnapshot.Price.ModelPrice)
	}
	if s.Reserved() != 100 {
		t.Fatalf("Reserved = %d, want 100", s.Reserved())
	}
}

// TestSessionPreConsumeRejectsExistingClamp 验证 Minor-8：预扣前已有
// QuotaClamp → 价格错误（400 语义），不得继续预扣。
func TestSessionPreConsumeRejectsExistingClamp(t *testing.T) {
	e := NewEngine(nil, nil, nil, billingTestConfig(), nil, nil, nil)
	rc := &contract.RelayContext{RequestID: testRequestID("clamp"), UserID: 1}
	rc.QuotaClamp = &contract.QuotaClamp{
		Op:       "QuotaFromFloatChecked",
		Kind:     contract.ClampOverflow,
		Original: 1e20,
		Clamped:  contract.MaxQuota,
	}
	s := e.NewSession(rc).(*Session)

	err := s.PreConsume(contract.QuotaEstimate{Model: "m", PromptTokens: 10})
	if !errors.Is(err, ErrPriceError) {
		t.Fatalf("PreConsume = %v, want ErrPriceError", err)
	}
	if s.Reserved() != 0 {
		t.Fatalf("Reserved = %d, want 0", s.Reserved())
	}
}
