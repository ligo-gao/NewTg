package store

// 本文件是模型性能测试存储的**纯单元测试**（不连 PostgreSQL）。
//
// SQL 路径由集成测试覆盖；这里只固定两个纯规则：
//   - Percentile 的 nearest-rank 口径与边界（空、单值、p 越界、未排序输入）；
//   - clampModelPerfLimit 的读面契约（默认 20、上限 100）。

import (
	"math"
	"testing"
)

func TestPercentileNearestRank(t *testing.T) {
	cases := []struct {
		name   string
		sorted []int
		p      float64
		want   int
	}{
		{"偶数样本 p50 取第 n/2 个", []int{10, 20, 30, 40}, 0.5, 20},
		{"p95 取靠后的真实样本", []int{10, 20, 30, 40}, 0.95, 40},
		{"1..10 p50", []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 0.5, 5},
		{"1..10 p95", []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 0.95, 10},
		{"单值 p50", []int{42}, 0.5, 42},
		{"单值 p0", []int{42}, 0, 42},
		{"单值 p1", []int{42}, 1, 42},
		{"p0 取最小值", []int{5, 9, 13}, 0, 5},
		{"p1 取最大值", []int{5, 9, 13}, 1, 13},
		{"p<0 按 0 处理", []int{5, 9, 13}, -0.5, 5},
		{"p>1 按 1 处理", []int{5, 9, 13}, 1.5, 13},
		{"NaN 按 0 处理（不 panic）", []int{5, 9, 13}, math.NaN(), 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Percentile(tc.sorted, tc.p)
			if !ok {
				t.Fatalf("Percentile(%v, %v) ok=false，期望 true", tc.sorted, tc.p)
			}
			if got != tc.want {
				t.Fatalf("Percentile(%v, %v) = %d，期望 %d", tc.sorted, tc.p, got, tc.want)
			}
		})
	}
}

func TestPercentileEmpty(t *testing.T) {
	for _, p := range []float64{0, 0.5, 1} {
		got, ok := Percentile(nil, p)
		if ok {
			t.Fatalf("Percentile(nil, %v) ok=true（值 %d），空样本必须 ok=false", p, got)
		}
		if got != 0 {
			t.Fatalf("Percentile(nil, %v) = %d，空样本应返回零值", p, got)
		}
	}
}

// TestPercentileUnsortedInputIsCallerContract 记录未排序输入的约定：
// 函数假设输入升序但不做校验/排序，因此返回值仍是切片中的某个元素，
// 只是不再具备分位数含义。测试只断言「不 panic 且值来自输入」，
// 防止未来实现意外引入越界或排序副作用。
func TestPercentileUnsortedInputIsCallerContract(t *testing.T) {
	input := []int{30, 10, 20}
	got, ok := Percentile(input, 0.5)
	if !ok {
		t.Fatalf("Percentile(%v, 0.5) ok=false，期望 true", input)
	}
	found := false
	for _, candidate := range input {
		if candidate == got {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("未排序输入返回了输入之外的值 %d", got)
	}
}

func TestClampModelPerfLimit(t *testing.T) {
	cases := []struct {
		in   int
		want int
	}{
		{-5, 20},
		{0, 20},
		{1, 1},
		{20, 20},
		{100, 100},
		{101, 100},
		{1000, 100},
	}
	for _, tc := range cases {
		if got := clampModelPerfLimit(tc.in); got != tc.want {
			t.Fatalf("clampModelPerfLimit(%d) = %d，期望 %d", tc.in, got, tc.want)
		}
	}
}
