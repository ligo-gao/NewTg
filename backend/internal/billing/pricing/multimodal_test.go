package pricing

import (
	"math"
	"testing"
)

// TestAudioTokensFromSecondsBoundaries 覆盖音频时长→token 的边界与饱和。
//
// 口径（relay-04 §8.2.1 / 契约 §9.4）：round(ceil(seconds) / 60 × 1000)。
// 负值/NaN 必须为 0（GC-9）；+Inf/超上界必须饱和而不是回绕成负值；
// ceil 边界（59.1 → 60 秒 → 1000 token）是「不足 1 秒按 1 秒」的直接证据。
func TestAudioTokensFromSecondsBoundaries(t *testing.T) {
	cases := []struct {
		name    string
		seconds float64
		want    int64
	}{
		{"负数", -5, 0},
		{"负无穷", math.Inf(-1), 0},
		{"NaN", math.NaN(), 0},
		{"零", 0, 0},
		{"极小正数向上取整", 0.01, 17}, // ceil=1 → round(16.67)=17
		{"整数秒", 1, 17},
		{"1.5 秒", 1.5, 33}, // ceil=2 → 33.33 → 33
		{"59.1 秒跨分钟", 59.1, 1000},
		{"60 秒", 60, 1000},
		{"90 秒", 90, 1500},
		{"3600 秒", 3600, 60000},
		{"正无穷饱和", math.Inf(1), math.MaxInt64},
		{"超大值饱和", math.MaxFloat64, math.MaxInt64},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := AudioTokensFromSeconds(tc.seconds); got != tc.want {
				t.Fatalf("AudioTokensFromSeconds(%v) = %d, want %d", tc.seconds, got, tc.want)
			}
		})
	}
}

// TestAudioSecondsFromDurationBoundaries 覆盖整秒口径的钳制与饱和。
func TestAudioSecondsFromDurationBoundaries(t *testing.T) {
	cases := []struct {
		name    string
		seconds float64
		want    int64
	}{
		{"负数", -1, 0},
		{"NaN", math.NaN(), 0},
		{"零", 0, 0},
		{"小数向上取整", 1.2, 2},
		{"整数", 90, 90},
		{"正无穷饱和", math.Inf(1), maxSafeAudioSeconds},
		{"超大值饱和", math.MaxFloat64, maxSafeAudioSeconds},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := AudioSecondsFromDuration(tc.seconds); got != tc.want {
				t.Fatalf("AudioSecondsFromDuration(%v) = %d, want %d", tc.seconds, got, tc.want)
			}
		})
	}
}

// TestAudioTokensPerMinute 固定「1 分钟 = 1000 token」的单一真源。
func TestAudioTokensPerMinute(t *testing.T) {
	if got := AudioTokensPerMinute(); got != 1000 {
		t.Fatalf("AudioTokensPerMinute() = %d, want 1000", got)
	}
}
