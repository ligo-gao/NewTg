package relay

import "newtg/gateway/internal/contract"

// maxAttemptRecords 是单请求 Attempts 的上限。
//
// 重试上限为 5（maxRetries），跨组再放大一次尝试数；8 条足以覆盖正常路径并
// 留出余量，同时防止被构造的上下文无界增长。超出部分直接丢弃，保留最早
// 的尝试（排障更关心请求先打到了哪些渠道）。
const maxAttemptRecords = 8

// RecordAttempt 把一轮上游尝试的执行层证据追加到 RelayContext。
//
// rc 为 nil 时直接返回：证据采集是尽力而为的旁路，绝不能影响热路径。
// 调用点必须在指标观察者的 nil 判断**之前**调用，否则未接指标的测试/降级
// 装配会静默丢掉证据。
func RecordAttempt(rc *contract.RelayContext, rec contract.AttemptRecord) {
	if rc == nil {
		return
	}
	if len(rc.Attempts) >= maxAttemptRecords {
		return
	}
	rc.Attempts = append(rc.Attempts, rec)
}
