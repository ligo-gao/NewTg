// Package triage 实现失败排障的判定引擎。
//
// 它是一个纯函数包：入参是已经从 usage_logs 读出的证据（retry_chain 与
// other.admin_info.triage 的投影），不访问数据库、不导入 relay 契约或计费包，
// 因此可以脱离 DB 与 HTTP 层单测。规则只作用于读取期，演进时无需回填历史数据。
package triage

// Owner 是一次失败的责任方。
type Owner string

const (
	// OwnerUser 表示问题出在用户请求侧（结构非法、额度/鉴权等前置拒绝）。
	OwnerUser Owner = "user"
	// OwnerChannel 表示问题出在上游渠道侧（鉴权失效、5xx）。
	OwnerChannel Owner = "channel"
	// OwnerGateway 表示问题出在网关自身（协议转换、未实现的能力）。
	OwnerGateway Owner = "gateway"
	// OwnerUnknown 表示现有证据不足以归责。
	OwnerUnknown Owner = "unknown"
)

// Signal 的 Vote 取值。方向票（user/channel/gateway）指向责任方，
// exclude-* 表示已排除某一方，neutral 表示该事实本身无法区分责任方。
const (
	VoteUser           = "user"
	VoteChannel        = "channel"
	VoteGateway        = "gateway"
	VoteExcludeUser    = "exclude-user"
	VoteExcludeChannel = "exclude-channel"
	VoteNeutral        = "neutral"
)

// Evidence 是判定所需的全部事实，全部来自已落库的证据。
// 字段刻意保持为原始类型（ErrorClass 是 usage_logs.error_class 的 varchar），
// 避免这个叶子包耦合 relay 的枚举与 JSON 结构。
type Evidence struct {
	// Status 是 usage_logs.status（error/refused/success/refunded 等）。
	Status string
	// ErrorClass 是 usage_logs.error_class 的已持久化分类。
	ErrorClass string
	// UpstreamStatus 是最近一次上游响应状态码；0 表示没有拿到响应。
	UpstreamStatus int
	// Attempts 是本请求的上游尝试次数。
	Attempts int
	// ConversionSteps 是本次请求经过的协议转换步数；0 表示原样转发。
	ConversionSteps int
	// RouteFrom / RouteTo 是协议转换的起止标签（如 anthropic → openai）。
	RouteFrom string
	RouteTo   string
	// RequestStructure 是中继前结构校验的结论：valid / invalid / 空（未校验）。
	RequestStructure string
	// RejectStage 是网关本地拒绝发生的阶段（quota/rate_limit/auth/...）。
	RejectStage string
	// ServerFailureSeen 表示整条重试链上出现过 5xx 或（非末轮的）超时/无响应。
	// 末轮即使以 4xx 收尾，也不能据此声称「所有尝试都没有服务端故障」。
	ServerFailureSeen bool
}

// Signal 是判定依据的一条原子事实。前端按 Code + Params 本地化渲染，
// 服务端从不下发自然语言。Params 只放分类时刻已知的小值（状态码、计数、
// 阶段、协议标签），不得放入自由文本。
type Signal struct {
	Code   string         `json:"code"`
	Vote   string         `json:"vote"`
	Params map[string]any `json:"params,omitempty"`
}

// Verdict 是一次判定结论。Rule 是命中的规则编号，Sub 是规则内的细分原因，
// 二者都由前端字典渲染。
type Verdict struct {
	Owner   Owner    `json:"owner"`
	Sub     string   `json:"sub"`
	Rule    string   `json:"rule"`
	Signals []Signal `json:"signals"`
}

// 规则编号。编号是前后端共享的稳定标识（CSS 变量、字典 key 都按它组织）。
const (
	ruleUSR01   = "USR-01"
	ruleUSR02   = "USR-02"
	ruleCH01    = "CH-01"
	ruleCH02    = "CH-02"
	ruleGW01    = "GW-01"
	ruleGW02    = "GW-02"
	ruleUnknown = "UNKNOWN"
)

// 规则内的细分原因（Verdict.Sub）。
const (
	subRequestStructure     = "request_structure"
	subUpstreamAuth         = "upstream_auth"
	subUpstreamServer       = "upstream_server"
	subProtocolConversion   = "protocol_conversion"
	subUnsupported          = "unsupported"
	subInsufficientEvidence = "insufficient_evidence"
)

// Signal.Code 的稳定取值。前端字典按 code 渲染「判定依据」。
const (
	sigRequestStructureInvalid    = "request_structure_invalid"
	sigRequestStructureValid      = "request_structure_valid"
	sigRequestStructureUnverified = "request_structure_unverified"
	sigRefusedBeforeUpstream      = "refused_before_upstream"
	sigNoUpstreamAttempt          = "no_upstream_attempt"
	sigNoUpstreamResponse         = "no_upstream_response"
	sigNoUpstreamServerError      = "no_upstream_server_error"
	sigUpstreamStatus             = "upstream_status"
	sigUpstreamAuthRejected       = "upstream_auth_rejected"
	sigUpstreamServerError        = "upstream_server_error"
	sigAttemptCount               = "attempt_count"
	sigConversionApplied          = "conversion_applied"
	sigConversionAbsent           = "conversion_absent"
	sigErrorClass                 = "error_class"
	sigErrorClassUnsupported      = "error_class_unsupported"
)

// 错误分类中明确指向网关能力缺失的取值（与 usage_logs.error_class 对齐）。
const (
	errorClassEndpointUnsupported      = "endpoint_unsupported"
	errorClassChannelTypeUnimplemented = "channel_type_unimplemented"
)

// Classify 在证据上判定失败责任方。
//
// 规则严格按序匹配，首个命中即返回。顺序本身是规格的一部分：例如
// RequestStructure=="invalid" 与 ConversionSteps>0 同时成立时必须判 USR-01，
// 因为请求在进入转换层之前就已经非法。
//
// 每个返回值都至少包含 2 条 signal，且至少有一条是 exclude-* 或 neutral：
// 「判定依据」必须体现出排除过程，而不只是匹配到一个错误码。
func Classify(ev Evidence) Verdict {
	switch {
	case ev.RequestStructure == "invalid":
		return classifyUserStructure(ev)
	case ev.Status == "refused":
		// 防御：refused 只应写在上游接触之前。一旦存在尝试记录或上游状态码，
		// 这条拒绝记录就与其他证据自相矛盾——绝不据此归责用户，退回「证据不足」。
		// 正常写入方在接触上游后不会写 refused（见 refusal 漏斗的守卫）。
		if ev.Attempts > 0 || ev.UpstreamStatus != 0 {
			return classifyUnknown(ev)
		}
		return classifyRefused(ev)
	case ev.UpstreamStatus == 401 || ev.UpstreamStatus == 403:
		return classifyUpstreamAuth(ev)
	case ev.UpstreamStatus >= 500:
		return classifyUpstreamServer(ev)
	case ev.ConversionSteps > 0 && ev.UpstreamStatus >= 400 && ev.UpstreamStatus < 500:
		return classifyProtocolConversion(ev)
	case ev.ErrorClass == errorClassEndpointUnsupported || ev.ErrorClass == errorClassChannelTypeUnimplemented:
		return classifyUnsupported(ev)
	default:
		return classifyUnknown(ev)
	}
}

// classifyUserStructure：USR-01，用户原始请求结构非法。
func classifyUserStructure(ev Evidence) Verdict {
	return Verdict{
		Owner: OwnerUser, Sub: subRequestStructure, Rule: ruleUSR01,
		Signals: []Signal{
			structureSignal(ev),
			// 结构非法发生在转换之前，转换只作为中性事实，不指向网关。
			conversionSignal(ev, VoteNeutral),
			upstreamSignal(ev),
		},
	}
}

// classifyRefused：USR-02，网关在发往上游之前本地拒绝。
func classifyRefused(ev Evidence) Verdict {
	stage := ev.RejectStage
	if stage == "" {
		// 证据缺阶段时给前端一个可本地化的稳定子原因，避免空 key。
		stage = "unknown"
	}
	return Verdict{
		Owner: OwnerUser, Sub: stage, Rule: ruleUSR02,
		Signals: []Signal{
			{Code: sigRefusedBeforeUpstream, Vote: VoteUser, Params: map[string]any{"stage": stage}},
			upstreamSignal(ev),
			structureSignal(ev),
		},
	}
}

// classifyUpstreamAuth：CH-01，上游以 401/403 拒绝凭据。
func classifyUpstreamAuth(ev Evidence) Verdict {
	return Verdict{
		Owner: OwnerChannel, Sub: subUpstreamAuth, Rule: ruleCH01,
		Signals: []Signal{
			{Code: sigUpstreamAuthRejected, Vote: VoteChannel, Params: map[string]any{"upstream_status": ev.UpstreamStatus}},
			structureSignal(ev),
			conversionSignal(ev, VoteNeutral),
		},
	}
}

// classifyUpstreamServer：CH-02，上游 5xx。
func classifyUpstreamServer(ev Evidence) Verdict {
	// 只有发生过重试时，尝试次数才额外指向渠道（说明故障转移已生效但没找到
	// 健康渠道）；单次尝试的中性计数不构成归责。
	attemptVote := VoteNeutral
	if ev.Attempts > 1 {
		attemptVote = VoteChannel
	}
	return Verdict{
		Owner: OwnerChannel, Sub: subUpstreamServer, Rule: ruleCH02,
		Signals: []Signal{
			{Code: sigUpstreamServerError, Vote: VoteChannel, Params: map[string]any{"upstream_status": ev.UpstreamStatus}},
			{Code: sigAttemptCount, Vote: attemptVote, Params: map[string]any{"attempts": ev.Attempts}},
			structureSignal(ev),
			conversionSignal(ev, VoteNeutral),
		},
	}
}

// classifyProtocolConversion：GW-01，4xx 出现在协议转换之后。
func classifyProtocolConversion(ev Evidence) Verdict {
	return Verdict{
		Owner: OwnerGateway, Sub: subProtocolConversion, Rule: ruleGW01,
		Signals: []Signal{
			// 4xx 排除了上游 5xx；该事实同时也满足 exclude-channel 要求。
			upstreamSignal(ev),
			conversionSignal(ev, VoteGateway),
			structureSignal(ev),
		},
	}
}

// classifyUnsupported：GW-02，上游能力在网关侧尚未实现。
func classifyUnsupported(ev Evidence) Verdict {
	return Verdict{
		Owner: OwnerGateway, Sub: subUnsupported, Rule: ruleGW02,
		Signals: []Signal{
			{Code: sigErrorClassUnsupported, Vote: VoteGateway, Params: map[string]any{"class": ev.ErrorClass}},
			upstreamSignal(ev),
			structureSignal(ev),
			conversionSignal(ev, VoteNeutral),
		},
	}
}

// classifyUnknown：UNKNOWN，证据不足以归责。仍然列出已掌握的事实与排除项，
// 让管理员看到引擎确实做过检查，而不是静默失败。
func classifyUnknown(ev Evidence) Verdict {
	signals := []Signal{
		upstreamSignal(ev),
		structureSignal(ev),
	}
	if ev.ErrorClass != "" {
		signals = append(signals, Signal{
			Code: sigErrorClass, Vote: VoteNeutral, Params: map[string]any{"class": ev.ErrorClass},
		})
	}
	signals = append(signals, conversionSignal(ev, VoteNeutral))
	return Verdict{
		Owner: OwnerUnknown, Sub: subInsufficientEvidence, Rule: ruleUnknown,
		Signals: signals,
	}
}

// structureSignal 把中继前的结构校验结论转成一条信号。
func structureSignal(ev Evidence) Signal {
	switch ev.RequestStructure {
	case "valid":
		return Signal{Code: sigRequestStructureValid, Vote: VoteExcludeUser}
	case "invalid":
		return Signal{Code: sigRequestStructureInvalid, Vote: VoteUser}
	default:
		return Signal{Code: sigRequestStructureUnverified, Vote: VoteNeutral}
	}
}

// upstreamSignal 描述上游响应这一事实本身。
//
// UpstreamStatus==0 只说明没有拿到响应，不等于没有尝试：结合 Attempts 区分
// 「根本没发往上游」（本地拒绝）与「尝试过但网络错误/超时」（不能声称未尝试，
// 那会与 params.attempts 自相矛盾）。401/403 即使被更高优先级规则让位
// （如 USR-01），也只作中性事实——鉴权失败指向渠道，不能反过来当作排除
// 渠道的依据。非鉴权类 4xx 作为「排除上游 5xx」的证据；其余（5xx/3xx 等）
// 只作中性事实。
func upstreamSignal(ev Evidence) Signal {
	switch {
	case ev.UpstreamStatus == 0 && ev.Attempts == 0:
		return Signal{Code: sigNoUpstreamAttempt, Vote: VoteNeutral, Params: map[string]any{"attempts": ev.Attempts}}
	case ev.UpstreamStatus == 0:
		return Signal{Code: sigNoUpstreamResponse, Vote: VoteNeutral, Params: map[string]any{"attempts": ev.Attempts}}
	case ev.UpstreamStatus == 401 || ev.UpstreamStatus == 403:
		return Signal{Code: sigUpstreamStatus, Vote: VoteNeutral, Params: map[string]any{"upstream_status": ev.UpstreamStatus}}
	case ev.UpstreamStatus >= 400 && ev.UpstreamStatus < 500:
		if ev.ServerFailureSeen {
			// 末轮 4xx 之前已出现过 5xx 或超时：不能声称「没有服务端故障」，
			// 更不能据此排除渠道。只作中性事实（保留最终状态码）。
			return Signal{Code: sigUpstreamStatus, Vote: VoteNeutral, Params: map[string]any{"upstream_status": ev.UpstreamStatus}}
		}
		return Signal{Code: sigNoUpstreamServerError, Vote: VoteExcludeChannel, Params: map[string]any{"attempts": ev.Attempts}}
	default:
		return Signal{Code: sigUpstreamStatus, Vote: VoteNeutral, Params: map[string]any{"upstream_status": ev.UpstreamStatus}}
	}
}

// conversionSignal 描述协议转换事实。apply 为调用方对「转换已发生」的投票：
// GW-01 指向网关，其余规则只作中性事实。
func conversionSignal(ev Evidence, applyVote string) Signal {
	params := routeParams(ev)
	if ev.ConversionSteps > 0 {
		if params == nil {
			params = make(map[string]any, 3)
		}
		params["steps"] = ev.ConversionSteps
		return Signal{Code: sigConversionApplied, Vote: applyVote, Params: params}
	}
	return Signal{Code: sigConversionAbsent, Vote: VoteNeutral, Params: params}
}

// routeParams 只在起止标签都已知时返回参数，避免把空字符串塞给前端。
func routeParams(ev Evidence) map[string]any {
	if ev.RouteFrom == "" || ev.RouteTo == "" {
		return nil
	}
	return map[string]any{"from": ev.RouteFrom, "to": ev.RouteTo}
}
