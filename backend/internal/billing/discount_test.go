package billing

import (
	"math"
	"testing"

	"newtg/gateway/internal/contract"
)

// TestDiscountSpecificity 验证「更具体者胜」（C-12/D15）与「不叠加取最低」。
func TestDiscountSpecificity(t *testing.T) {
	target := DiscountTarget{Model: "gpt-4o", Group: "vip", Channel: "42"}

	t.Run("平级按具体度：模型 > 分组 > 渠道 > 全局", func(t *testing.T) {
		all := []contract.Discount{
			{Rate: 0.8, Target: "global"},
			{Rate: 0.8, Target: "channel:42"},
			{Rate: 0.8, Target: "group:vip"},
			{Rate: 0.8, Target: "model:gpt-4o"},
		}
		// 打乱输入顺序，证明结果与快照遍历顺序无关。
		orders := [][]int{{0, 1, 2, 3}, {3, 2, 1, 0}, {2, 0, 3, 1}}
		for _, order := range orders {
			var shuffled []contract.Discount
			for _, i := range order {
				shuffled = append(shuffled, all[i])
			}
			got := ResolveDiscount(shuffled, target)
			if got.Target != "model:gpt-4o" || got.Rate != 0.8 {
				t.Fatalf("order %v: ResolveDiscount = %+v, want model:gpt-4o @0.8", order, got)
			}
		}
	})

	t.Run("去掉模型规则后分组胜出", func(t *testing.T) {
		got := ResolveDiscount([]contract.Discount{
			{Rate: 0.7, Target: "global"},
			{Rate: 0.7, Target: "channel:42"},
			{Rate: 0.7, Target: "group:vip"},
		}, target)
		if got.Target != "group:vip" {
			t.Fatalf("ResolveDiscount = %+v, want group:vip", got)
		}
	})

	t.Run("渠道规则比全局更紧时取更紧（C-14/D14）", func(t *testing.T) {
		got := ResolveDiscount([]contract.Discount{
			{Rate: 1, Target: "global"},
			{Rate: 0.6, Target: "channel:42"},
		}, target)
		if got.Target != "channel:42" || got.Rate != 0.6 {
			t.Fatalf("ResolveDiscount = %+v, want channel:42 @0.6", got)
		}
	})

	t.Run("全局更低时不叠加、取最低", func(t *testing.T) {
		got := ResolveDiscount([]contract.Discount{
			{Rate: 0.9, Target: "model:gpt-4o"},
			{Rate: 0.5, Target: "global"},
		}, target)
		if got.Target != "global" || got.Rate != 0.5 {
			t.Fatalf("ResolveDiscount = %+v, want global @0.5（不叠加）", got)
		}
	})
}

// TestDiscountMatching 验证作用域匹配是精确等值，未知/未命中一律不打折。
func TestDiscountMatching(t *testing.T) {
	target := DiscountTarget{Model: "m1", Group: "vip", Channel: "7"}

	tests := []struct {
		name      string
		discounts []contract.Discount
		wantRate  float64
		wantTgt   string
	}{
		{
			name:      "空列表返回无折扣",
			discounts: nil,
			wantRate:  1,
			wantTgt:   "",
		},
		{
			name: "模型不匹配时不用它",
			discounts: []contract.Discount{
				{Rate: 0.1, Target: "model:other"},
			},
			wantRate: 1,
			wantTgt:  "",
		},
		{
			name: "分组成员不匹配",
			discounts: []contract.Discount{
				{Rate: 0.1, Target: "group:default"},
			},
			wantRate: 1,
			wantTgt:  "",
		},
		{
			name: "渠道不匹配",
			discounts: []contract.Discount{
				{Rate: 0.1, Target: "channel:8"},
			},
			wantRate: 1,
			wantTgt:  "",
		},
		{
			name: "目标为空时空折扣目标不生效（不能把零值当全免）",
			discounts: []contract.Discount{
				{Rate: 0, Target: ""},
			},
			wantRate: 1,
			wantTgt:  "",
		},
		{
			name: "未知作用域 fail-closed",
			discounts: []contract.Discount{
				{Rate: 0.1, Target: "tenant:acme"},
				{Rate: 0.2, Target: "user:1"},
			},
			wantRate: 1,
			wantTgt:  "",
		},
		{
			name: "作用域前缀大小写不敏感",
			discounts: []contract.Discount{
				{Rate: 0.5, Target: "MODEL:m1"},
			},
			wantRate: 0.5,
			wantTgt:  "MODEL:m1",
		},
		{
			name: "global 大小写不敏感",
			discounts: []contract.Discount{
				{Rate: 0.4, Target: "Global"},
			},
			wantRate: 0.4,
			wantTgt:  "Global",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveDiscount(tc.discounts, target)
			if got.Rate != tc.wantRate || got.Target != tc.wantTgt {
				t.Fatalf("ResolveDiscount = %+v, want rate=%v target=%q", got, tc.wantRate, tc.wantTgt)
			}
		})
	}
}

// TestDiscountTieDeterminism 验证同 rate 的确定性与 SortDiscounts 的稳定排序。
func TestDiscountTieDeterminism(t *testing.T) {
	target := DiscountTarget{Model: "m", Group: "vip", Channel: "1"}
	// 同一作用域只有「标识等于目标值」的规则会命中，所以平级确定性来自
	// 重复规则与不同作用域之间的具体度比较；这里钉死重复规则不放大结果。
	got := ResolveDiscount([]contract.Discount{
		{Rate: 0.5, Target: "group:vip"},
		{Rate: 0.5, Target: "group:vip"},
	}, target)
	if got.Target != "group:vip" || got.Rate != 0.5 {
		t.Fatalf("ResolveDiscount = %+v, want group:vip @0.5", got)
	}

	// SortDiscounts 使用同一口径：具体度降序、标识升序。
	sorted := SortDiscounts([]contract.Discount{
		{Rate: 0.9, Target: "global"},
		{Rate: 0.9, Target: "channel:2"},
		{Rate: 0.9, Target: "group:b"},
		{Rate: 0.9, Target: "group:a"},
		{Rate: 0.9, Target: "model:z"},
	})
	want := []string{"model:z", "group:a", "group:b", "channel:2", "global"}
	for i, w := range want {
		if sorted[i].Target != w {
			t.Fatalf("SortDiscounts[%d] = %q, want %q（all=%v）", i, sorted[i].Target, w, sorted)
		}
	}
}

// TestDiscountRejectsNonFiniteRate 验证 NaN/Inf 不参与比较（防止坏配置全站白送）。
func TestDiscountRejectsNonFiniteRate(t *testing.T) {
	target := DiscountTarget{Model: "m"}
	tests := []struct {
		name string
		rate float64
	}{
		{"NaN", math.NaN()},
		{"+Inf", math.Inf(1)},
		{"-Inf", math.Inf(-1)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveDiscount([]contract.Discount{
				{Rate: tc.rate, Target: "model:m"},
				{Rate: 0.9, Target: "global"},
			}, target)
			if got.Rate != 0.9 || got.Target != "global" {
				t.Fatalf("ResolveDiscount = %+v, want global @0.9（非有限 rate 被跳过）", got)
			}
		})
	}
}

// TestDiscountApply 覆盖 ApplyDiscount 的全部顺序分支。
func TestDiscountApply(t *testing.T) {
	tests := []struct {
		name  string
		quota int64
		d     contract.Discount
		want  int64
	}{
		{
			name:  "未命中时原样返回（即使 rate=0）",
			quota: 100,
			d:     contract.Discount{Rate: 0, Target: ""},
			want:  100,
		},
		{
			name:  "命中全免返回 0 且不被保底抬升",
			quota: 100,
			d:     contract.Discount{Rate: 0, Target: "group:vip"},
			want:  0,
		},
		{
			name:  "全免对小配额同样为 0",
			quota: 1,
			d:     contract.Discount{Rate: 0, Target: "model:m"},
			want:  0,
		},
		{
			name:  "折扣后为正数",
			quota: 100,
			d:     contract.Discount{Rate: 0.5, Target: "group:vip"},
			want:  50,
		},
		{
			name:  "正配额折后截断为 0 时保底 1",
			quota: 1,
			d:     contract.Discount{Rate: 0.5, Target: "group:vip"},
			want:  1,
		},
		{
			name:  "极小正率仍保底 1（全免之后的分支）",
			quota: 1,
			d:     contract.Discount{Rate: 0.001, Target: "group:vip"},
			want:  1,
		},
		{
			name:  "rate=1 原价",
			quota: 100,
			d:     contract.Discount{Rate: 1, Target: "global"},
			want:  100,
		},
		{
			name:  "负率归零",
			quota: 100,
			d:     contract.Discount{Rate: -0.5, Target: "global"},
			want:  0,
		},
		{
			name:  "零配额不产生保底",
			quota: 0,
			d:     contract.Discount{Rate: 0.5, Target: "global"},
			want:  0,
		},
		{
			name:  "负配额原样返回",
			quota: -5,
			d:     contract.Discount{Rate: 0.5, Target: "global"},
			want:  -5,
		},
		{
			name:  "NaN 率按最小计费 1",
			quota: 100,
			d:     contract.Discount{Rate: math.NaN(), Target: "global"},
			want:  1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ApplyDiscount(tc.quota, tc.d); got != tc.want {
				t.Fatalf("ApplyDiscount(%d, %+v) = %d, want %d", tc.quota, tc.d, got, tc.want)
			}
		})
	}
}

// TestDiscountFullFreeBeforeMinimum 显式钉死顺序：全免（rate=0）必须返回 0，
// 而「0<rate<1 且折后为 0」才置 1。颠倒顺序会让全免被抬成 1（契约 §8）。
func TestDiscountFullFreeBeforeMinimum(t *testing.T) {
	fullFree := ApplyDiscount(1, contract.Discount{Rate: 0, Target: "model:m"})
	tiny := ApplyDiscount(1, contract.Discount{Rate: 0.01, Target: "model:m"})
	if fullFree != 0 {
		t.Fatalf("全免 ApplyDiscount = %d, want 0", fullFree)
	}
	if tiny != 1 {
		t.Fatalf("极小折扣 ApplyDiscount = %d, want 1（最小计费保底）", tiny)
	}
}
