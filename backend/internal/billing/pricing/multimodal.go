package pricing

import "math"

// 音频时长 → token 的单位系数（relay-04 §8.2.1 / 契约 §9.4 冻结）。
//
// 【为什么放在计费引擎】CCR-5（relay-04 §8.2 / 契约 §2.3）明确：中继只报
// **测得时长**（`Usage.AudioSeconds`），「1 分钟 = 1000 token」这个换算常量
// 归计费引擎，适配器不得取整。但 M4 的 usage 提取需要把时长填成可结算的
// 音频 token（`UsageDetail.Audio`），因此把唯一真源导出给中继复用，避免
// 两边各写一份 1000 而漂移（GC-4：禁止硬编码折算常量）。
const (
	// audioTokensPerMinute 是 1 分钟的音频 token 数。
	audioTokensPerMinute int64 = 1000
	// secondsPerMinute 是分钟与秒的换算。
	secondsPerMinute int64 = 60
	// maxAudioSeconds 是 ceil 后仍可安全参与 int64 乘法的秒数上界。
	// 超过该值直接饱和，避免 `seconds * 1000` 回绕成负数（GC-9）。
	maxAudioSeconds = math.MaxInt64 / audioTokensPerMinute * secondsPerMinute
)

// AudioTokensPerMinute 导出换算系数供审计/测试引用，禁止调用方另写 1000。
func AudioTokensPerMinute() int64 { return audioTokensPerMinute }

// AudioSecondsFromDuration 把响应中测得的音频时长（秒，可为小数）换算为
// 契约 `Usage.AudioSeconds` 的整数秒口径：向上取整。
//
// 为什么向上取整：relay-04 §8.2.1 的公式是 `ceil(duration_seconds)`，
// 不足 1 秒的音频也要按 1 秒度量（基线-06 §3 照搬）。负值/NaN 钳 0；
// +Inf 或超上界饱和到 maxAudioSeconds（不得回绕为负）。
func AudioSecondsFromDuration(seconds float64) int64 {
	if math.IsNaN(seconds) || seconds <= 0 {
		return 0
	}
	if math.IsInf(seconds, 1) || seconds > float64(maxAudioSeconds) {
		return maxAudioSeconds
	}
	return int64(math.Ceil(seconds))
}

// AudioTokensFromSeconds 把音频时长（秒）按「1 分钟 = 1000 token」换算为
// 音频 token：`round(ceil(seconds) / 60 × 1000)`。
//
// 取整口径与基线一致（基线 QuotaRound = math.Round）：例如 1 秒 → 17、
// 60 秒 → 1000、90 秒 → 2500。负值/NaN 一律 0（GC-9 负值不进计费）；
// 越界饱和到 MaxInt64。中继侧不自行取整（relay-03 §11.4），此处是计费
// 引擎提供的唯一换算入口。
func AudioTokensFromSeconds(seconds float64) int64 {
	ceilSeconds := AudioSecondsFromDuration(seconds)
	if ceilSeconds >= maxAudioSeconds {
		return math.MaxInt64
	}
	return int64(math.Round(float64(ceilSeconds) / float64(secondsPerMinute) * float64(audioTokensPerMinute)))
}
