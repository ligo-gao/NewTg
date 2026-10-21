package httpapi

import (
	"context"
	"testing"

	"newtg/gateway/internal/billing/pricing"
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/relay/billingseam"
)

// 本文件验收 T5 实时结算路径（复核 I4）：有表达式必须按求值成本结算，
// 无表达式按预扣额结清，求值失败/接口缺失保留预扣。
// 断言「最终额度 = 求值结果」可区分「误退回 heldQuota」的变异。

func frozenExprSnapshot(expr string) map[string]any {
	return map[string]any{
		"mode":           "tiered_expr",
		"expr":           expr,
		"group_ratio":    1.0,
		"quota_per_unit": float64(100),
		"units":          map[string]any{"seconds": "second"},
		"source":         "plugin_client",
	}
}

func TestSettleImmediateSuccessUsesEvaluatedCost(t *testing.T) {
	prev := pricing.CurrentQuotaPerUnit()
	if err := pricing.SetQuotaPerUnit(100); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pricing.SetQuotaPerUnit(prev) })

	session := newTaskBillingRecorder()
	h := &TasksHandler{}
	// u("seconds") * 0.7，seconds=10 → $7 → 700 积分。
	done, quota := h.settleImmediateSuccess(session, "req-cost",
		frozenExprSnapshot(`u("seconds") * 0.7`),
		map[string]any{"usage": map[string]any{"seconds": float64(10)}}, nil, 1000)
	if !done {
		t.Fatalf("expression settlement should succeed")
	}
	// 变异检查：若实现退回 heldQuota，这里会是 1000。
	if quota != 700 {
		t.Fatalf("quota=%d, want 700 (evaluated cost)", quota)
	}
	if len(session.costCalls) != 1 || session.costCalls[0] != 700 {
		t.Fatalf("cost calls=%v, want [700]", session.costCalls)
	}
}

func TestSettleImmediateSuccessWithoutExpressionKeepsHeld(t *testing.T) {
	session := newTaskBillingRecorder()
	h := &TasksHandler{}
	done, quota := h.settleImmediateSuccess(session, "req-held", nil,
		map[string]any{"usage": map[string]any{"seconds": float64(10)}}, nil, 1000)
	if !done || quota != 1000 {
		t.Fatalf("held settlement: done=%v quota=%d, want true/1000", done, quota)
	}
	if len(session.costCalls) != 0 {
		t.Fatalf("cost settlement must not be used without an expression: %v", session.costCalls)
	}
}

func TestSettleImmediateSuccessEvalFailureKeepsHold(t *testing.T) {
	session := newTaskBillingRecorder()
	h := &TasksHandler{}
	done, quota := h.settleImmediateSuccess(session, "req-fail",
		frozenExprSnapshot(`u("clips") * 0.5`),
		map[string]any{"usage": map[string]any{"seconds": float64(10)}}, nil, 1000)
	if done || quota != 0 {
		t.Fatalf("eval failure must not settle: done=%v quota=%d", done, quota)
	}
	if len(session.costCalls) != 0 {
		t.Fatalf("eval failure must not call cost settlement: %v", session.costCalls)
	}
	if session.Stub.Terminated() || session.Stub.Refunded() {
		t.Fatalf("eval failure must keep the hold (no terminate/refund)")
	}
}

// noCostSession 实现 contract.BillingHandoff 但没有成本结算方法。
type noCostSession struct{ *billingseam.Stub }

func (noCostSession) Detach(string) error                                   { return nil }
func (noCostSession) TerminateByRequestID(string, contract.UsageFact) error { return nil }
func (noCostSession) RefundByRequestID(string) error                        { return nil }

func TestSettleImmediateSuccessWithoutCostInterfaceKeepsHold(t *testing.T) {
	session := noCostSession{Stub: billingseam.NewStub()}
	h := &TasksHandler{}
	done, _ := h.settleImmediateSuccess(session, "req-nocost",
		frozenExprSnapshot(`u("seconds") * 0.7`),
		map[string]any{"usage": map[string]any{"seconds": float64(10)}}, nil, 1000)
	if done {
		t.Fatalf("missing cost interface must keep the hold, not settle at held")
	}
	if session.Stub.Terminated() || session.Stub.Refunded() {
		t.Fatalf("missing cost interface must not terminate/refund")
	}
}

func TestSettleImmediateSuccessCostErrorKeepsHold(t *testing.T) {
	session := newTaskBillingRecorder()
	session.costErr = context.DeadlineExceeded
	h := &TasksHandler{}
	done, _ := h.settleImmediateSuccess(session, "req-cost-err",
		frozenExprSnapshot(`u("seconds") * 0.7`),
		map[string]any{"usage": map[string]any{"seconds": float64(10)}}, nil, 1000)
	if done {
		t.Fatalf("cost settlement error must keep the hold")
	}
}
