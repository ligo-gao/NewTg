package relay

import (
	"time"

	"newtg/gateway/internal/contract"
)

// AttemptEvent 是一次上游尝试的可观测事实（W10-METRICS）。
//
// 为什么用回调而不是让 relay 直接 import metrics：relay 是执行层边界包，
// 只负责产出事实；标签收敛（model/channel 基数护栏）与导出格式属于指标包。
// 回调必须非阻塞且不得改变返回语义（Observer 为 nil 时零开销）。
type AttemptEvent struct {
	// Endpoint 是模板化端点名（chat_completions/messages/responses/other），
	// 不是原始 path：后者含 id/令牌，禁止做标签。
	Endpoint string
	Model    string
	Stream   bool

	ChannelID   int
	ChannelType string
	// FromType 是上一轮尝试的渠道类型；首次尝试为空。仅用于故障转移计数。
	FromType string
	// AttemptIndex 是整笔请求内的尝试序号（0 起，跨组不重置）。
	AttemptIndex int
	// Result ∈ {ok, retryable_fail, nonretryable_fail}。
	Result     string
	ErrorClass contract.ErrorClass
	Duration   time.Duration
}

// AttemptObserver 接收每次上游尝试的结果；实现方必须非阻塞。
type AttemptObserver func(AttemptEvent)

// 失败结果标签（有限枚举，冻结）。
const (
	attemptOK            = "ok"
	attemptRetryableFail = "retryable_fail"
	attemptFatalFail     = "nonretryable_fail"
)

// EndpointLabel 把请求路径收敛为有界端点标签。
//
// 只认已注册的公开端点；插件原生路由/未知路径一律 other。绝不能直接回传
// RequestPath（可能含路径参数）。
func EndpointLabel(rc *contract.RelayContext) string {
	if rc == nil {
		return "other"
	}
	switch rc.RequestPath {
	case "/v1/chat/completions":
		return "chat_completions"
	case "/v1/messages":
		return "messages"
	case "/v1/responses":
		return "responses"
	default:
		return "other"
	}
}

// ErrorClassName 把冻结的 ErrorClass 映射为指标标签（有限枚举）。
//
// 单一来源在 contract.ErrorClass.String()：指标、usage_logs 与结构化日志
// 必须共用同一批标签，否则同一个错误在不同视图里会分成两个分组。
func ErrorClassName(class contract.ErrorClass) string {
	return class.String()
}

// notifyAttempt 是观察者的唯一调用入口：nil 安全，不让观察失败进入热路径。
func notifyAttempt(observer AttemptObserver, ev AttemptEvent) {
	if observer == nil {
		return
	}
	observer(ev)
}
