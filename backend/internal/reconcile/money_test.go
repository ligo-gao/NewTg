package reconcile

import (
	"math"
	"testing"
)

func TestCreditsToMinorRoundsHalfUp(t *testing.T) {
	cases := []struct{ credits, qpu, want int64 }{
		{150, 100, 150}, {1, 100, 1}, {1, 3, 33}, {2, 3, 67}, {0, 100, 0}, {5, 2, 250},
		// 恰好一半（0.5）必须进位；略低于一半必须舍去；负数对称。
		{1, 200, 1}, {1, 201, 0}, {-1, 200, -1},
	}
	for _, c := range cases {
		if got := CreditsToMinor(c.credits, c.qpu); got != c.want {
			t.Fatalf("CreditsToMinor(%d,%d) = %d, want %d", c.credits, c.qpu, got, c.want)
		}
	}
}

// 单价小于 1 时按 100 处理，避免除零与「1 额度 = 1 分」以外的意外口径。
func TestCreditsToMinorClampsInvalidQuotaPerUnit(t *testing.T) {
	for _, qpu := range []int64{0, -5} {
		if got := CreditsToMinor(150, qpu); got != 150 {
			t.Fatalf("CreditsToMinor(150,%d) = %d, want 150 (qpu clamped to 100)", qpu, got)
		}
	}
}

func TestDeltaBPSaturatesInsteadOfOverflowing(t *testing.T) {
	got := DeltaBP(1<<62, 1)
	if got != math.MaxInt64 {
		t.Fatalf("DeltaBP overflow handling = %d, want MaxInt64", got)
	}
	if DeltaBP(5, 0) != 50000 { // denominator floors at 1
		t.Fatalf("DeltaBP with zero snapshot tokens must use denominator 1")
	}
}

func TestMarginBP(t *testing.T) {
	if bp, ok := MarginBP(272580, 996540); !ok || bp != 2735 {
		t.Fatalf("MarginBP = %d,%v; want 2735,true", bp, ok)
	}
	if _, ok := MarginBP(10, 0); ok {
		t.Fatalf("zero revenue must not produce a margin rate")
	}
}

func TestMarginBPRoundsHalfUp(t *testing.T) {
	// 1*10000/8 = 1250；5*10000/8 = 6250；负数对称。
	// 另外两条真正钉住舍入：1/20000 = 0.5bp 必须进位到 1，
	// 1/20001 ≈ 0.49998bp 必须舍去为 0。
	cases := []struct{ margin, revenue, want int64 }{
		{1, 8, 1250},
		{5, 8, 6250},
		{-1, 8, -1250},
		{1, 20000, 1},
		{1, 20001, 0},
		{-1, 20000, -1},
	}
	for _, c := range cases {
		if bp, ok := MarginBP(c.margin, c.revenue); !ok || bp != c.want {
			t.Fatalf("MarginBP(%d,%d) = %d,%v want %d,true", c.margin, c.revenue, bp, ok, c.want)
		}
	}
}
