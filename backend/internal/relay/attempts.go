package relay

import "newtg/gateway/internal/contract"

// RecordAttempt 把一轮上游尝试的执行层证据追加到 RelayContext。
//
// rc 为 nil 时直接返回：证据采集是尽力而为的旁路，绝不能影响热路径。
// 调用点必须在指标观察者的 nil 判断**之前**调用，否则未接指标的测试/降级
// 装配会静默丢掉证据。
//
// 上限取 contract.MaxAttemptRecords（与 billing.buildRetryChain 的落库上界
// 同一常量）：超出部分直接丢弃，保留最早的尝试（排障更关心请求先打到了
// 哪些渠道）。
func RecordAttempt(rc *contract.RelayContext, rec contract.AttemptRecord) {
	if rc == nil {
		return
	}
	if len(rc.Attempts) >= contract.MaxAttemptRecords {
		return
	}
	rc.Attempts = append(rc.Attempts, rec)
}
