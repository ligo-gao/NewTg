package pricing

import (
	"errors"
	"math"
	"testing"

	"github.com/shopspring/decimal"

	"newtg/gateway/internal/contract"
)

// ---------------------------------------------------------------------------
// 截断族（billing-00 §7.2：向零截断）
// ---------------------------------------------------------------------------

func TestQuotaFromFloatTruncatesTowardZero(t *testing.T) {
	cases := []struct {
		in   float64
		want int64
	}{
		{1.9, 1},
		{-1.9, -1},
		{0.999, 0},
		{-0.999, 0},
		{0, 0},
		{2147483647, 2147483647},
	}
	for _, c := range cases {
		if got := QuotaFromFloat(c.in); got != c.want {
			t.Errorf("QuotaFromFloat(%v)=%d，期望 %d", c.in, got, c.want)
		}
	}
}

func TestQuotaFromFloatCheckedClamps(t *testing.T) {
	// 上溢：钳到 MaxQuota，Original 保留原值。
	q, clamp := QuotaFromFloatChecked(math.MaxInt32 + 10)
	if q != contract.MaxQuota || clamp == nil {
		t.Fatalf("上溢应钳到 MaxQuota 并返回标记，got q=%d clamp=%v", q, clamp)
	}
	if clamp.Kind != contract.ClampOverflow || clamp.Op != "QuotaFromFloatChecked" ||
		clamp.Original != float64(math.MaxInt32+10) || clamp.Clamped != contract.MaxQuota {
		t.Fatalf("上溢标记不符合契约: %+v", *clamp)
	}

	// 下溢。
	q, clamp = QuotaFromFloatChecked(float64(contract.MinQuota) - 10)
	if q != contract.MinQuota || clamp == nil || clamp.Kind != contract.ClampUnderflow {
		t.Fatalf("下溢应钳到 MinQuota，got q=%d clamp=%+v", q, clamp)
	}

	// NaN → 0 + ClampNaN。
	q, clamp = QuotaFromFloatChecked(math.NaN())
	if q != 0 || clamp == nil || clamp.Kind != contract.ClampNaN || clamp.Clamped != 0 {
		t.Fatalf("NaN 应回退 0 且 kind=nan，got q=%d clamp=%+v", q, clamp)
	}

	// +Inf / -Inf。
	if _, c := QuotaFromFloatChecked(math.Inf(1)); c == nil || c.Kind != contract.ClampOverflow {
		t.Fatalf("+Inf 应为 overflow，got %+v", c)
	}
	if _, c := QuotaFromFloatChecked(math.Inf(-1)); c == nil || c.Kind != contract.ClampUnderflow {
		t.Fatalf("-Inf 应为 underflow，got %+v", c)
	}

	// 界内不产生标记。
	if q, clamp := QuotaFromFloatChecked(42.9); q != 42 || clamp != nil {
		t.Fatalf("界内不应产生标记，got q=%d clamp=%v", q, clamp)
	}
}

func TestQuotaFromFloatStrict(t *testing.T) {
	q, err := QuotaFromFloatStrict(float64(contract.MaxQuota) + 1)
	if q != 0 {
		t.Fatalf("Strict 越界应返回值 0，got %d", q)
	}
	clamp, ok := AsClamp(err)
	if !ok || clamp.Kind != contract.ClampOverflow {
		t.Fatalf("Strict 越界应返回可提取的钳制错误，got %v", err)
	}
	if clamp.Op != "QuotaFromFloatStrict" {
		t.Fatalf("Strict 的 op 应为 QuotaFromFloatStrict，got %q", clamp.Op)
	}

	if q, err := QuotaFromFloatStrict(3.7); q != 3 || err != nil {
		t.Fatalf("界内 Strict 应正常返回，got q=%d err=%v", q, err)
	}
}

// ---------------------------------------------------------------------------
// 取整族（半离零）
// ---------------------------------------------------------------------------

func TestQuotaRoundHalfAwayFromZero(t *testing.T) {
	cases := []struct {
		in   float64
		want int64
	}{
		{0.5, 1},
		{-0.5, -1},
		{2.5, 3},
		{-2.5, -3},
		{2.4, 2},
		{-2.4, -2},
		{0.49, 0},
	}
	for _, c := range cases {
		if got := QuotaRound(c.in); got != c.want {
			t.Errorf("QuotaRound(%v)=%d，期望 %d", c.in, got, c.want)
		}
	}
}

func TestQuotaRoundCheckedAndStrict(t *testing.T) {
	if q, clamp := QuotaRoundChecked(math.NaN()); q != 0 || clamp == nil || clamp.Kind != contract.ClampNaN {
		t.Fatalf("QuotaRoundChecked(NaN) 应回退 0 并标记，got q=%d clamp=%+v", q, clamp)
	}
	if _, err := QuotaRoundStrict(math.Round(0)); err != nil {
		t.Fatalf("QuotaRoundStrict(0) 不应报错: %v", err)
	}
	if _, err := QuotaRoundStrict(float64(contract.MaxQuota) * 2); err == nil {
		t.Fatal("QuotaRoundStrict 越界应报错")
	}
}

// ---------------------------------------------------------------------------
// 十进制族
// ---------------------------------------------------------------------------

func TestQuotaFromDecimalRoundsThenSaturates(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"1.4", 1},
		{"1.5", 2},
		{"-1.5", -2},
		{"2.5", 3},
		{"-2.5", -3},
	}
	for _, c := range cases {
		d, err := decimal.NewFromString(c.in)
		if err != nil {
			t.Fatalf("构造 decimal %q: %v", c.in, err)
		}
		if got := QuotaFromDecimal(d); got != c.want {
			t.Errorf("QuotaFromDecimal(%s)=%d，期望 %d", c.in, got, c.want)
		}
	}

	// 先 Round(0) 再饱和：2147483647.4 → 2147483647（不越界）。
	d, _ := decimal.NewFromString("2147483647.4")
	if got, clamp := QuotaFromDecimalChecked(d); got != contract.MaxQuota || clamp != nil {
		t.Fatalf("Round 后恰好在上界不应产生标记，got q=%d clamp=%+v", got, clamp)
	}

	// 2147483647.6 → Round 后越界。
	d, _ = decimal.NewFromString("2147483647.6")
	if got, clamp := QuotaFromDecimalChecked(d); got != contract.MaxQuota || clamp == nil ||
		clamp.Kind != contract.ClampOverflow {
		t.Fatalf("Round 后越界应钳制，got q=%d clamp=%+v", got, clamp)
	}

	// 负方向。
	d, _ = decimal.NewFromString("-2147483648.6")
	if got, clamp := QuotaFromDecimalChecked(d); got != contract.MinQuota || clamp == nil ||
		clamp.Kind != contract.ClampUnderflow {
		t.Fatalf("负方向越界应钳制，got q=%d clamp=%+v", got, clamp)
	}
}

func TestQuotaFromDecimalStrict(t *testing.T) {
	d, _ := decimal.NewFromString("999999999999999999999")
	q, err := QuotaFromDecimalStrict(d)
	if q != 0 || err == nil {
		t.Fatalf("Strict 越界应 (0,error)，got q=%d err=%v", q, err)
	}
	if clamp, ok := AsClamp(err); !ok || clamp.Op != "QuotaFromDecimalStrict" {
		t.Fatalf("应可提取 QuotaFromDecimalStrict 标记，got %v", err)
	}
}

// ---------------------------------------------------------------------------
// 钱包族（2^53−1）
// ---------------------------------------------------------------------------

func TestWalletQuotaBounds(t *testing.T) {
	maxD := decimal.NewFromInt(contract.MaxWalletQuota)
	if q, clamp := WalletQuotaFromDecimalChecked(maxD); q != contract.MaxWalletQuota || clamp != nil {
		t.Fatalf("恰在上界应通过，got q=%d clamp=%+v", q, clamp)
	}
	q, clamp := WalletQuotaFromDecimalChecked(maxD.Add(decimal.NewFromInt(1)))
	if q != contract.MaxWalletQuota || clamp == nil || clamp.Kind != contract.ClampOverflow {
		t.Fatalf("上界 +1 应钳制，got q=%d clamp=%+v", q, clamp)
	}
	if clamp.Op != "WalletQuotaFromDecimalChecked" {
		t.Fatalf("钱包 Checked 的 op 不符: %q", clamp.Op)
	}

	negMax := decimal.NewFromInt(-contract.MaxWalletQuota)
	if q, clamp := WalletQuotaFromDecimalChecked(negMax); q != -contract.MaxWalletQuota || clamp != nil {
		t.Fatalf("恰在下界应通过，got q=%d clamp=%+v", q, clamp)
	}
	if _, err := WalletQuotaFromDecimalStrict(negMax.Sub(decimal.NewFromInt(1))); err == nil {
		t.Fatal("钱包下界 -1 应被 Strict 阻断")
	}

	// ValidateWalletQuota：只挡上界，负值放行（乙类怪癖 Q1）。
	if err := ValidateWalletQuota(-123); err != nil {
		t.Fatalf("负值应放行: %v", err)
	}
	if err := ValidateWalletQuota(contract.MaxWalletQuota + 1); err == nil {
		t.Fatal("超过 JS 安全整数上界应报错")
	}
}

// ---------------------------------------------------------------------------
// op 枚举：Checked 与 Strict 必须不同（billing-00 §7.2 冻结）
// ---------------------------------------------------------------------------

func TestClampOpsDifferBetweenCheckedAndStrict(t *testing.T) {
	over := float64(contract.MaxQuota) + 100

	_, cFloat := QuotaFromFloatChecked(over)
	_, eFloat := QuotaFromFloatStrict(over)
	_, cRound := QuotaRoundChecked(over)
	_, eRound := QuotaRoundStrict(over)
	d := decimal.NewFromInt(int64(contract.MaxQuota) + 100)
	_, cDec := QuotaFromDecimalChecked(d)
	_, eDec := QuotaFromDecimalStrict(d)

	pairs := []struct {
		checked *contract.QuotaClamp
		err     error
		name    string
	}{
		{cFloat, eFloat, "QuotaFromFloat"},
		{cRound, eRound, "QuotaRound"},
		{cDec, eDec, "QuotaFromDecimal"},
	}
	seen := map[string]bool{}
	for _, p := range pairs {
		if p.checked == nil {
			t.Fatalf("%s Checked 未产生标记", p.name)
		}
		strictClamp, ok := AsClamp(p.err)
		if !ok {
			t.Fatalf("%s Strict 未返回钳制错误", p.name)
		}
		if p.checked.Op == strictClamp.Op {
			t.Fatalf("%s 的 Checked/Strict op 相同（%q），违反审计区分要求", p.name, p.checked.Op)
		}
		if seen[p.checked.Op] || seen[strictClamp.Op] {
			t.Fatalf("op %q 重复出现，op 必须唯一标识变体", p.checked.Op)
		}
		seen[p.checked.Op] = true
		seen[strictClamp.Op] = true
	}
}

func TestClampErrorAndAuditMap(t *testing.T) {
	_, clamp := QuotaFromFloatChecked(math.Inf(1))
	err := &ClampError{Clamp: clamp}
	if err.Error() == "" {
		t.Fatal("ClampError.Error 不应为空")
	}
	m := ClampAuditMap(clamp)
	if len(m) != 4 {
		t.Fatalf("AuditMap 必须恰为 4 键，got %d: %v", len(m), m)
	}
	for _, k := range []string{"op", "kind", "original", "clamped"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("AuditMap 缺少键 %q: %v", k, m)
		}
	}
	if m["kind"] != "overflow" {
		t.Fatalf("kind 应为 overflow，got %v", m["kind"])
	}
	var ce *ClampError
	if !errors.As(err, &ce) || ce.Clamp != clamp {
		t.Fatal("errors.As 应能取出 ClampError")
	}
}

func TestClampLoggerHook(t *testing.T) {
	var got []*contract.QuotaClamp
	SetClampLogger(func(c *contract.QuotaClamp) { got = append(got, c) })
	t.Cleanup(func() { SetClampLogger(nil) })

	_, _ = QuotaFromFloatChecked(math.NaN())
	_, _ = QuotaRoundStrict(float64(contract.MaxQuota) + 1)
	if len(got) != 2 {
		t.Fatalf("Checked 与 Strict 的钳制都应通知钩子，got %d", len(got))
	}
}

// ---------------------------------------------------------------------------
// quota_per_unit
// ---------------------------------------------------------------------------

func TestSetQuotaPerUnitValidates(t *testing.T) {
	old := CurrentQuotaPerUnit()
	t.Cleanup(func() { _ = SetQuotaPerUnit(old) })

	for _, bad := range []int64{0, -1, 1_000_001} {
		if err := SetQuotaPerUnit(bad); err == nil {
			t.Fatalf("SetQuotaPerUnit(%d) 应报错", bad)
		}
	}
	if err := SetQuotaPerUnit(250); err != nil {
		t.Fatalf("合法值应接受: %v", err)
	}
	if got := CurrentQuotaPerUnit(); got != 250 {
		t.Fatalf("CurrentQuotaPerUnit=%d，期望 250", got)
	}
}
