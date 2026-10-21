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
	snap, err := h.freezeTaskBilling(context.Background(), rc, "sample", "client", "upstream")
	if err != nil {
		t.Fatalf("freeze: %v", err)
	}
	if snap == nil || snap["expr"] != `u("seconds") * 0.7` || snap["source"] != "plugin_client" {
		t.Fatalf("unexpected snapshot: %#v", snap)
	}
	if snap["group_ratio"] != float64(2) {
		t.Fatalf("group ratio not frozen: %#v", snap["group_ratio"])
	}
}

func TestFreezeTaskBillingBlankPluginExprRejected(t *testing.T) {
	h := &TasksHandler{billingSetting: func() pricing.BillingSetting {
		return pricing.BillingSetting{
			PluginBillingExpr: map[string]string{"sample::client": "   "},
		}
	}}
	if _, err := h.freezeTaskBilling(context.Background(), &contract.RelayContext{},
		"sample", "client", "upstream"); err == nil {
		t.Fatalf("blank plugin expression must be rejected at submission")
	}
}

func TestFreezeTaskBillingNoExpression(t *testing.T) {
	h := &TasksHandler{}
	snap, err := h.freezeTaskBilling(context.Background(), &contract.RelayContext{},
		"sample", "client", "upstream")
	if err != nil || snap != nil {
		t.Fatalf("no expression: snap=%#v err=%v", snap, err)
	}
}
