package billing

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"newtg/gateway/internal/contract"
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
