package reconcile

import "math"

// minorUnitsPerCredit 是 1 个额度对应的最小货币单位（分）。上游控制台以
// 「额度」计价，而账单以分展示：1 额度 = 0.01 元 = 1 分，故乘以 100 得到分。
const minorUnitsPerCredit = int64(100)

// bpScale 是「万分之一」的换算基数：1 bp = 0.01%。
const bpScale = int64(10000)

// CreditsToMinor 把额度换算成最小货币单位（分），四舍五入（half away from zero）。
//
// 纯整数运算：quotaPerUnit 表示「1 美元额度包含多少额度单位」，默认 100；
// 其值小于 1 时按 100 处理，避免除零。乘法溢出时饱和到 int64 边界，
// 绝不回绕成负数。
func CreditsToMinor(credits, quotaPerUnit int64) int64 {
	if quotaPerUnit < 1 {
		quotaPerUnit = 100
	}
	return mulDivRoundHalfUp(credits, minorUnitsPerCredit, quotaPerUnit)
}

// DeltaBP 计算 token 差异率（万分之一）：deltaTokens*10000/max(1, snapshotTokens)。
//
// 分母下限为 1，避免快照为 0 时除零；乘积溢出时饱和到 int64 边界。
func DeltaBP(deltaTokens, snapshotTokens int64) int64 {
	den := snapshotTokens
	if den < 1 {
		den = 1
	}
	if deltaTokens > math.MaxInt64/bpScale {
		return math.MaxInt64
	}
	if deltaTokens < math.MinInt64/bpScale {
		return math.MinInt64
	}
	return deltaTokens * bpScale / den
}

// MarginBP 计算毛利率（万分之一）：marginMinor*10000/revenueMinor，
// 四舍五入（half away from zero）。收入不大于 0 时毛利率无定义，返回 false。
func MarginBP(marginMinor, revenueMinor int64) (int64, bool) {
	if revenueMinor <= 0 {
		return 0, false
	}
	return mulDivRoundHalfUp(marginMinor, bpScale, revenueMinor), true
}

// mulDivRoundHalfUp 计算 a*b/den 并按「半数远离零」舍入，全程使用整数。
//
// 调用方保证 b > 0 且 den > 0。乘法或加法溢出时饱和到 int64 边界，
// 使异常大的输入退化为最大值而不是回绕成负数（回绕会让「巨大差异」变成
// 「没有差异」）。
func mulDivRoundHalfUp(a, b, den int64) int64 {
	if a >= 0 {
		if a > math.MaxInt64/b {
			return math.MaxInt64
		}
		num := a * b
		half := den / 2
		if num > math.MaxInt64-half {
			return math.MaxInt64
		}
		return (num + half) / den
	}
	// 负数按绝对值对称处理：-0.5 舍入为 -1，与 math.Round 一致。
	if a < math.MinInt64/b {
		return math.MinInt64
	}
	num := a * b
	half := den / 2
	if num < math.MinInt64+half {
		return math.MinInt64
	}
	return (num - half) / den
}
