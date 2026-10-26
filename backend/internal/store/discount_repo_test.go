package store

// 折扣规则的**纯单元测试**（不连 PostgreSQL）：只固定选择算法的优先级与
// 目标编码，SQL 读取路径由集成测试覆盖。
//
// 优先级是计费口径的一部分（spec §5.2）：同一请求可能同时命中个人规则与
// 分组规则、或多个候选模型名的规则，选错一条就是金额错误。这里把三条
// 排序键逐条钉死，避免后续「优化」把 tie-break 改成依赖数据库返回顺序。

import "testing"

// TestPickCustomerDiscountLowestRateWins：折扣力度最大（rate_percent 最低）
// 的规则胜出，即使它是较不具体的分组规则。
func TestPickCustomerDiscountLowestRateWins(t *testing.T) {
	rows := []CustomerDiscount{
		{ID: 1, ModelName: "claude-sonnet-5-5", TargetType: "user", TargetID: "7", RatePercent: 90},
		{ID: 2, ModelName: "claude-sonnet-5-5", TargetType: "group", TargetID: "acme", RatePercent: 80},
	}
	got, ok := PickCustomerDiscount(rows, []string{"claude-sonnet-5-5"})
	if !ok {
		t.Fatal("PickCustomerDiscount ok=false，期望命中")
	}
	if got.ID != 2 {
		t.Fatalf("PickCustomerDiscount 选中 ID=%d，期望 2（rate_percent 最低）", got.ID)
	}
	if got.RatePercent != 80 || got.Target() != "group:acme" {
		t.Fatalf("PickCustomerDiscount = rate %d target %q，期望 80 / group:acme",
			got.RatePercent, got.Target())
	}
}

// TestPickCustomerDiscountUserWinsTie：rate_percent 相同时个人规则胜分组规则
// （更具体的目标优先）。
func TestPickCustomerDiscountUserWinsTie(t *testing.T) {
	rows := []CustomerDiscount{
		{ID: 1, ModelName: "claude-sonnet-5-5", TargetType: "group", TargetID: "acme", RatePercent: 80},
		{ID: 2, ModelName: "claude-sonnet-5-5", TargetType: "user", TargetID: "7", RatePercent: 80},
	}
	got, ok := PickCustomerDiscount(rows, []string{"claude-sonnet-5-5"})
	if !ok {
		t.Fatal("PickCustomerDiscount ok=false，期望命中")
	}
	if got.Target() != "user:7" {
		t.Fatalf("PickCustomerDiscount target = %q，期望 user:7（同 rate 时个人优先）", got.Target())
	}
}

// TestPickCustomerDiscountEarlierCandidateWins：同 target_type、同 rate 时，
// 取 candidates 里更靠前的模型名（规范名优先），与数据库中行的返回顺序无关。
func TestPickCustomerDiscountEarlierCandidateWins(t *testing.T) {
	rows := []CustomerDiscount{
		{ID: 1, ModelName: "claude-sonnet-5-5-latest", TargetType: "user", TargetID: "7", RatePercent: 80},
		{ID: 2, ModelName: "claude-sonnet-5-5", TargetType: "user", TargetID: "7", RatePercent: 80},
	}
	candidates := []string{"claude-sonnet-5-5", "claude-sonnet-5-5-latest"}
	got, ok := PickCustomerDiscount(rows, candidates)
	if !ok {
		t.Fatal("PickCustomerDiscount ok=false，期望命中")
	}
	if got.ModelName != "claude-sonnet-5-5" {
		t.Fatalf("PickCustomerDiscount model = %q，期望 candidates[0]=claude-sonnet-5-5", got.ModelName)
	}
}

// TestPickCustomerDiscountNoRows：没有命中任何规则时返回零值与 false。
func TestPickCustomerDiscountNoRows(t *testing.T) {
	for _, rows := range [][]CustomerDiscount{nil, {}} {
		got, ok := PickCustomerDiscount(rows, []string{"claude-sonnet-5-5"})
		if ok {
			t.Fatalf("PickCustomerDiscount(%v) ok=true，期望 false", rows)
		}
		if got != (CustomerDiscount{}) {
			t.Fatalf("PickCustomerDiscount(%v) = %+v，期望零值", rows, got)
		}
	}
}

// TestCustomerDiscountRateAndTarget：rate_percent → 实收倍率，以及目标字符串
// 的两种编码（user:<id> / group:<name>）。
func TestCustomerDiscountRateAndTarget(t *testing.T) {
	cases := []struct {
		name       string
		discount   CustomerDiscount
		wantRate   float64
		wantTarget string
	}{
		{"85 折", CustomerDiscount{TargetType: "user", TargetID: "42", RatePercent: 85}, 0.85, "user:42"},
		{"全免", CustomerDiscount{TargetType: "group", TargetID: "acme", RatePercent: 0}, 0, "group:acme"},
		{"原价", CustomerDiscount{TargetType: "group", TargetID: "acme", RatePercent: 100}, 1, "group:acme"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.discount.Rate(); got != tc.wantRate {
				t.Fatalf("Rate() = %v，期望 %v", got, tc.wantRate)
			}
			if got := tc.discount.Target(); got != tc.wantTarget {
				t.Fatalf("Target() = %q，期望 %q", got, tc.wantTarget)
			}
		})
	}
}
