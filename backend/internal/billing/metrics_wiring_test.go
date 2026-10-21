package billing

// W10-METRICS 接线级用例（I-3）：真实 Session/Engine 调用经过指标接缝。
// 需要 NEWTG_TEST_DB_DSN（与仓内其他计费集成测试同约定，未设置时跳过）。

import (
	"errors"
	"testing"

	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/metrics"
)

func billingMetricCounter(t *testing.T, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := metrics.Registry().Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, fam := range families {
		if fam.GetName() != name {
			continue
		}
		total := 0.0
		for _, m := range fam.Metric {
			match := true
			for wantName, wantValue := range labels {
				found := false
				for _, pair := range m.Label {
					if pair.GetName() == wantName {
						found = pair.GetValue() == wantValue
						break
					}
				}
				if !found {
					match = false
					break
				}
			}
			if match {
				total += m.GetCounter().GetValue()
			}
		}
		return total
	}
	return 0
}

// TestBillingMetricsWiring：PreConsume / Settle / Refund / QuotaClamp / 402
// 五条接缝在真实计费路径上各产生一次预期样本。
func TestBillingMetricsWiring(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)

	// —— 预扣 + 结算（钱包、按次 100）——
	userID := createBillingTestUser(t, db, 500)
	preModel := "w10metrics-pre"
	pre := newBillingTestSession(e, userID, testRequestID("w10m-pre"), perCallSnapshot(100))
	before := billingMetricCounter(t, "newtg_billing_preconsume_credits_total",
		map[string]string{"model": preModel, "group": "default"})
	if err := pre.PreConsume(contract.QuotaEstimate{Model: preModel}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if delta := billingMetricCounter(t, "newtg_billing_preconsume_credits_total",
		map[string]string{"model": preModel, "group": "default"}) - before; delta != 100 {
		t.Fatalf("preconsume delta = %v, want 100", delta)
	}

	settleModel := "w10metrics-settle"
	settle := newBillingTestSession(e, userID, testRequestID("w10m-settle"), perCallSnapshot(100))
	if err := settle.PreConsume(contract.QuotaEstimate{Model: settleModel}); err != nil {
		t.Fatalf("settle PreConsume: %v", err)
	}
	before = billingMetricCounter(t, "newtg_billing_settle_credits_total",
		map[string]string{"model": settleModel, "group": "default", "billing_mode": "per_request"})
	if err := settle.Terminate(contract.UsageFact{Model: settleModel}); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if delta := billingMetricCounter(t, "newtg_billing_settle_credits_total",
		map[string]string{"model": settleModel, "group": "default", "billing_mode": "per_request"}) - before; delta != 100 {
		t.Fatalf("settle delta = %v, want 100", delta)
	}

	// —— 同步退款 ——
	refundUser := createBillingTestUser(t, db, 500)
	refund := newBillingTestSession(e, refundUser, testRequestID("w10m-refund"), perCallSnapshot(100))
	if err := refund.PreConsume(contract.QuotaEstimate{Model: "w10metrics-refund"}); err != nil {
		t.Fatalf("refund PreConsume: %v", err)
	}
	before = billingMetricCounter(t, "newtg_billing_refund_credits_total",
		map[string]string{"reason": "sync"})
	if err := refund.Refund(); err != nil {
		t.Fatalf("Refund: %v", err)
	}
	if delta := billingMetricCounter(t, "newtg_billing_refund_credits_total",
		map[string]string{"reason": "sync"}) - before; delta != 100 {
		t.Fatalf("refund sync delta = %v, want 100", delta)
	}

	// —— 配额钳制 ——
	before = billingMetricCounter(t, "newtg_billing_quota_clamp_total",
		map[string]string{"op": "w10metrics_op", "kind": "overflow"})
	settle.noteQuotaClamp(&contract.QuotaClamp{Op: "w10metrics_op", Kind: contract.ClampOverflow})
	if delta := billingMetricCounter(t, "newtg_billing_quota_clamp_total",
		map[string]string{"op": "w10metrics_op", "kind": "overflow"}) - before; delta != 1 {
		t.Fatalf("quota clamp delta = %v, want 1", delta)
	}

	// —— 402 额度不足 ——
	poorUser := createBillingTestUser(t, db, 0)
	insufficientModel := "w10metrics-insufficient"
	poor := newBillingTestSession(e, poorUser, testRequestID("w10m-402"), perCallSnapshot(100))
	before = billingMetricCounter(t, "newtg_billing_insufficient_quota_total",
		map[string]string{"model": insufficientModel})
	err := poor.PreConsume(contract.QuotaEstimate{Model: insufficientModel})
	if !errors.Is(err, ErrInsufficientQuota) {
		t.Fatalf("PreConsume error = %v, want ErrInsufficientQuota", err)
	}
	if delta := billingMetricCounter(t, "newtg_billing_insufficient_quota_total",
		map[string]string{"model": insufficientModel}) - before; delta != 1 {
		t.Fatalf("insufficient quota delta = %v, want 1", delta)
	}
}
