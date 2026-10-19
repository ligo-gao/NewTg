package billing

import (
	"context"
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
