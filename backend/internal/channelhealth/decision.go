package channelhealth

import (
	"bytes"
	"time"

	"newtg/gateway/internal/contract"
)

// 自动禁用原因的稳定 code（R-M4-05）。UI/邮件按 code 本地化，绝不把上游
// 错误原文写进 channels.auto_disabled_reason（可能含凭据或 URL，GC-6）。
const (
	// ReasonUpstreamError：渠道级错误（ErrorClass == ErrChannelError）。
	// 当前适配器没有该分类的生产者，判定保留为接缝（R-M4-01 C）。
	ReasonUpstreamError = "channel_upstream_error"
	// ReasonAuthError：上游鉴权失败（401/403 → ErrUpstreamAuth）。
	ReasonAuthError = "channel_auth_error"
	// ReasonKeyword：响应体命中 rl.health.disable_keywords。
	ReasonKeyword = "channel_keyword"
	// ReasonTimeout：仅体检路径的响应时间超阈（R-M4-02；M4-b 使用，
	// 中继逐请求不产生该 code）。
	ReasonTimeout = "channel_timeout"
)

// 触发来源（审计 detail.trigger）。与 reason code 分开：同一 reason 可能由
// 不同入口产生（中继/体检/手工测试），审计需要能区分「谁判的」。
const (
	TriggerUpstreamError = "upstream_error"
	TriggerAuth          = "auth"
	TriggerKeyword       = "keyword"
	// TriggerTimeout：体检/手工测试的响应头耗时 ≥ 阈值（R-M4-02），
	// 中继逐请求不产生该触发。
	TriggerTimeout = "timeout"
)

// maxDecisionBodyBytes 与执行层错误体读取上限一致（relay/route.go 的
// 64 KiB）。判定函数再做一次防御性截断：M4-b 的探测路径可能传入完整响应体，
// 关键词匹配必须是有界操作（R-M4-12）。
const maxDecisionBodyBytes = 64 << 10

// Decision 是一次「应当自动禁用」的判定结果。
type Decision struct {
	// ReasonCode 是写入 channels.auto_disabled_reason 的稳定 code。
	ReasonCode string
	// Trigger 是触发来源，只进审计 detail，不落库。
	Trigger string
}

// DecideFailure 判定一次上游失败是否应触发自动禁用（R-M4-01 C 的完整实现）。
//
// 入参全部来自**本轮真实尝试**：class/status/body 由执行层在非 2xx 分支
// 产生，autoBan 来自同一渠道行。返回 ok=false 表示「不满足任一触发条件」，
// 调用方必须保持渠道状态不变。
//
// 守卫顺序与冻结语义：
//  1. 全局开关 automatic_disable 默认 false——关闭时直接返回；
//  2. 渠道 auto_ban=false 不禁（R-M4-01 C）；
//  3. ErrUpstreamAuth 且状态 401/403 → channel_auth_error；
//  4. ErrChannelError → channel_upstream_error（保留接缝）；
//  5. 其余响应体命中关键词 → channel_keyword。纯 5xx/429 未命中关键词、
//     ErrEndpointUnsupported、ErrChannelConfig、参数类错误、ctx 取消与
//     传输层错误都不会走到这里（执行层不回调）或在此返回 false。
//
// 响应时间阈值**不在**本函数：R-M4-02 冻结为仅体检/手工测试路径应用，
// 中继逐请求不计时、不因耗时禁用。
func DecideFailure(class contract.ErrorClass, statusCode int, body []byte, autoBan bool, settings DecisionSettings) (Decision, bool) {
	// 默认关闭（R-M4-01 C / modules-08 F-08-45）：开关缺失或脏值都是 false。
	// 这条守卫必须在最前面：它同时保护关键词与鉴权两条触发路径。
	if !settings.AutoDisableEnabled {
		return Decision{}, false
	}
	// 渠道级开关。执行层已把 auto_ban 随 assignment 传入，这里再判一次，
	// 让手工测试/体检路径（M4-b 直接调用本函数）无需重复实现同一守卫。
	if !autoBan {
		return Decision{}, false
	}

	switch class {
	case contract.ErrUpstreamAuth:
		// 401/403 显式限定：ErrUpstreamAuth 在适配器层就只由这两个状态码
		// 产生，但判定函数是共享契约，不能在别的调用方传入错误分类时把
		// 任意状态码当成鉴权失败（变异测试：放宽到 5xx 必须失败）。
		if statusCode == 401 || statusCode == 403 {
			return Decision{ReasonCode: ReasonAuthError, Trigger: TriggerAuth}, true
		}
	case contract.ErrChannelError:
		return Decision{ReasonCode: ReasonUpstreamError, Trigger: TriggerUpstreamError}, true
	case contract.ErrEndpointUnsupported, contract.ErrChannelConfig,
		contract.ErrParamInvalid, contract.ErrUnknown:
		// I-1（复核）：这四类**不是渠道健康信号**，关键词匹配必须在这里
		// 早退。否则上游把「Permission denied」回显在 400/422/EOF 等响应体
		// 里时，会被关键词规则误判成渠道故障，违反 R-M4-01 C 的「参数类/
		// 端点不支持/渠道配置错误不禁」。注意 429/5xx（ErrUpstreamRateLimit /
		// ErrUpstreamServer / ErrRetryable）仍会落到下面的关键词匹配。
		return Decision{}, false
	}

	if matchDisableKeyword(body, settings.DisableKeywords) {
		return Decision{ReasonCode: ReasonKeyword, Trigger: TriggerKeyword}, true
	}
	return Decision{}, false
}

// matchDisableKeyword 做 lowercase 子串匹配（R-M4-12）。
//
// keywords 已在 LoadDecisionSettings/ParseDisableKeywords 里 lowercase；
// body 在每次判定时 lowercase 一次。使用 bytes.ToLower 而非逐关键词转换，
// 避免 7 次整段拷贝。响应体先按 64 KiB 截断：关键词只在前 64 KiB 内生效，
// 与执行层读取上限一致，恶意超长错误体不会放大判定成本（GC-6）。
func matchDisableKeyword(body []byte, keywords []string) bool {
	if len(body) == 0 || len(keywords) == 0 {
		return false
	}
	if len(body) > maxDecisionBodyBytes {
		body = body[:maxDecisionBodyBytes]
	}
	lowered := bytes.ToLower(body)
	for _, keyword := range keywords {
		if keyword == "" {
			continue
		}
		if bytes.Contains(lowered, []byte(keyword)) {
			return true
		}
	}
	return false
}

// DecideProbeFailure 判定一次**探测结果**是否应触发自动禁用。
//
// 与 DecideFailure 的关系：
//   - 非 2xx 响应完全委托 DecideFailure（同一触发集合，R-M4-01/15）；
//   - 增加一条**仅探测路径**的触发：2xx 但响应头耗时 ≥ 阈值 → channel_timeout
//     （R-M4-02；中继逐请求不计时，因此该分支只在本函数出现）。
//
// 明确不触发（R-M4-01 C / R-M4-06）：
//   - 传输层超时/网络错误：那是「本次没拿到响应」，不构成渠道故障证据，
//     只记 last_test_result=false；
//   - 无 adapter（Unsupported）：整轮跳过，不改 last_test_*；
//   - 凭据解封/请求构造失败：本地配置问题，禁用会掩盖真正原因。
//
// 阈值比较用 `>=`（R-M4-14 的边界语义：「达到阈值即记失败」）；success 的
// 判定用互补的 `<`，两处必须同时改，否则边界用例会红。
func DecideProbeFailure(result ProbeResult, autoBan bool, settings WorkerSettings) (Decision, bool) {
	// 开关与 auto_ban 的守卫必须在这里也做一次：阈值分支不经过
	// DecideFailure，若只依赖委托，关闭开关的站点仍会因「慢响应」禁用
	// （变异：删掉这两行 → TestDecideProbeFailureSwitchOff 红）。
	if !settings.AutoDisableEnabled || !autoBan {
		return Decision{}, false
	}
	if result.Unsupported || result.CredentialInvalid || result.BuildFailed || result.TransportClass != "" {
		return Decision{}, false
	}
	if result.Success {
		if settings.ResponseTimeThreshold <= 0 {
			return Decision{}, false
		}
		elapsed := time.Duration(result.LatencyMS) * time.Millisecond
		if elapsed >= settings.ResponseTimeThreshold {
			return Decision{ReasonCode: ReasonTimeout, Trigger: TriggerTimeout}, true
		}
		return Decision{}, false
	}
	return DecideFailure(result.ErrorClass, result.StatusCode, result.Body, autoBan, settings.DecisionSettings)
}

// ProbeSucceeded 报告一次探测是否算「健康」（2xx 且未超过阈值）。
//
// 与 DecideProbeFailure 的边界必须一致（>= 判失败 ⇔ < 判成功）；worker 用
// 它写 last_test_result，自动恢复也只在 true 时发生（R-M4-09/15）。
func ProbeSucceeded(result ProbeResult, settings WorkerSettings) bool {
	if !result.Success || result.TransportClass != "" || result.Unsupported ||
		result.CredentialInvalid || result.BuildFailed {
		return false
	}
	if settings.ResponseTimeThreshold <= 0 {
		return true
	}
	return time.Duration(result.LatencyMS)*time.Millisecond < settings.ResponseTimeThreshold
}
