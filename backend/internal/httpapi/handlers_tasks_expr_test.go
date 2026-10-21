package httpapi

import (
	"context"
	"testing"

	"newtg/gateway/internal/billing/pricing"
	"newtg/gateway/internal/contract"
)

// 本文件验收 T5 提交期冻结：解析顺序来自配置/快照，冻结结果是
// private_data 可序列化的快照，且空表达式在提交期即拒绝。

func TestFreezeTaskBillingPluginOverrideWins(t *testing.T) {
	h := &TasksHandler{billingSetting: func() pricing.BillingSetting {
		return pricing.BillingSetting{
			PluginBillingExpr: map[string]string{"sample::client": `u("seconds") * 0.7`},
		}
	}}
	// 即使模型快照是 tiered（第 3 级），插件第 1 级也必须优先。
	rc := &contract.RelayContext{PriceSnapshot: &contract.PriceSnapshot{
		Mode:       contract.BillingModeTieredExpr,
		ExprString: `u("seconds") * 9`,
		GroupRatio: 2,
	}}
	snap, err := h.freezeTaskBilling(context.Background(), rc, "sample", "client", "upstream", nil)
	if err != nil {
		t.Fatalf("freeze: %v", err)
	}
	if snap.snapshot == nil || snap.snapshot["expr"] != `u("seconds") * 0.7` || snap.snapshot["source"] != "plugin_client" {
		t.Fatalf("unexpected snapshot: %#v", snap)
	}
	if snap.snapshot["group_ratio"] != float64(2) {
		t.Fatalf("group ratio not frozen: %#v", snap.snapshot["group_ratio"])
	}
	if snap.snapshot["quota_per_unit"] == nil {
		t.Fatalf("quota per unit must be frozen: %#v", snap.snapshot)
	}
}

func TestFreezeTaskBillingBlankPluginExprRejected(t *testing.T) {
	h := &TasksHandler{billingSetting: func() pricing.BillingSetting {
		return pricing.BillingSetting{
			PluginBillingExpr: map[string]string{"sample::client": "   "},
		}
	}}
	if _, err := h.freezeTaskBilling(context.Background(), &contract.RelayContext{},
		"sample", "client", "upstream", nil); err == nil {
		t.Fatalf("blank plugin expression must be rejected at submission")
	}
}

func TestFreezeTaskBillingNoExpression(t *testing.T) {
	h := &TasksHandler{}
	snap, err := h.freezeTaskBilling(context.Background(), &contract.RelayContext{},
		"sample", "client", "upstream", nil)
	if err != nil || snap.snapshot != nil || snap.skip != nil {
		t.Fatalf("no expression: snap=%#v err=%v", snap, err)
	}
}

// TestFreezeTaskBillingSkipsTokenExpressionWithMarker 覆盖复核 I2：模型级
// token 表达式被跳过时必须留下 held_skip 标记与原因。
func TestFreezeTaskBillingSkipsTokenExpressionWithMarker(t *testing.T) {
	h := &TasksHandler{}
	rc := &contract.RelayContext{PriceSnapshot: &contract.PriceSnapshot{
		Mode:       contract.BillingModeTieredExpr,
		ExprString: "p * 2 + c * 8",
	}}
	res, err := h.freezeTaskBilling(context.Background(), rc, "sample", "client", "upstream", nil)
	if err != nil {
		t.Fatalf("freeze: %v", err)
	}
	if res.snapshot != nil || res.skip == nil {
		t.Fatalf("token expression must be skipped: %#v", res)
	}
	if res.skip["source"] != "held_skip" || res.skip["reason"] == "" {
		t.Fatalf("skip marker incomplete: %#v", res.skip)
	}
}

// TestFreezeTaskBillingBrokenTaskExpressionRejected 覆盖复核 I2：
// 含 u( 却不可编译的模型表达式必须在提交期 fail-closed。
func TestFreezeTaskBillingBrokenTaskExpressionRejected(t *testing.T) {
	h := &TasksHandler{}
	rc := &contract.RelayContext{PriceSnapshot: &contract.PriceSnapshot{
		Mode:       contract.BillingModeTieredExpr,
		ExprString: `u("seconds") +`,
	}}
	if _, err := h.freezeTaskBilling(context.Background(), rc, "sample", "client", "upstream", nil); err == nil {
		t.Fatalf("broken u() expression must be rejected at submission")
	}
}
