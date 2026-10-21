// Package mj 是 Midjourney 系绘图任务族（family='mj'）的领域包：动作枚举与
// 解析、上游状态归一化、提交入参上界、错误信封、任务行映射、MjId 归属与
// 渠道选择/锁定语义，以及固定形状的上游提交客户端。
//
// 本包不碰 DB、不建 HTTP 路由、不做计费：HTTP 装配（MJ-a2）、查询驱动
// （MJ-b）与图片代理（MJ-c）在各自的层里调用这里的结果。提交客户端只负责
// 「归一化结果 → `{base}/mj/submit/<kind>`」这一段出站线协议，渠道选择、
// 预扣/退款顺序与任务落库都在 HTTP 编排层。这样「归一化/上界/映射」可以
// 脱离 IO 单测，也不会把凭据或路由语义带进数据面。
package mj

import (
	"encoding/json"
	"errors"
	"fmt"
)

// 线协议返回码（relay-05 §4.2；基线 constant/midjourney.go）。
//
// 【为什么沿用这些数字】它们是客户端（midjourney-proxy 生态）已经按数字
// 解析的既有契约。网关内部可以有更细的错误分类，但对外必须落回这几个
// 值：成功恒为 1；参数错误 4；未知错误 5。21/22/23 是上游的「已受理」码，
// 网关结构化解析后**重新编码为 1**回给客户端（甲类-2：不再做响应体字符串
// 替换），但内部仍要保留原始语义以决定落库状态与复用。
const (
	CodeSuccess        = 1  // 提交成功
	CodeNoAccount      = 3  // 上游无可用 Discord 账号实例
	CodeRequestError   = 4  // 请求参数错误（基线 MjRequestError）
	CodeUnknownError   = 5  // 未知/内部错误（基线 MjErrorUnknown）
	CodeTaskExists     = 21 // 任务已存在（可能已出结果）
	CodeQueued         = 22 // 排队中
	CodeQueueFull      = 23 // 队列已满（仍视为已受理，不重试）
	CodeSensitiveWord  = 24 // prompt 含敏感词
	CodeGroupSaturated = 30 // 分组负载饱和（分派层 429）
)

// 固定的错误描述。全部是常量、绝不回显用户输入：错误体是客户端唯一能
// 看到的诊断面，把请求片段（尤其 base64/提示词）拼进 description 会把
// 用户内容泄露到日志、代理与浏览器历史里。R-MJ-17 要求所有错误只有
// {code, description} 两个字段，描述必须稳定可断言。
const (
	DescriptionInvalidRequest      = "invalid_request"
	DescriptionUnknownEndpoint     = "unknown_endpoint"
	DescriptionUnknownAction       = "unknown_action"
	DescriptionIndexParseFailed    = "index_parse_failed"
	DescriptionPromptRequired      = "prompt_is_required"
	DescriptionContentRequired     = "content_is_required"
	DescriptionContentParseFailed  = "content_parse_failed"
	DescriptionCustomIDRequired    = "custom_id_is_required"
	DescriptionTaskIDRequired      = "task_id_is_required"
	DescriptionActionRequired      = "action_is_required"
	DescriptionIndexRequired       = "index_is_required"
	DescriptionBase64Required      = "base64_array_is_required"
	DescriptionSourceRequired      = "source_base64_is_required"
	DescriptionTargetRequired      = "target_base64_is_required"
	DescriptionBase64ArrayTooLarge = "base64_array_too_large"
	DescriptionBase64ItemTooLarge  = "base64_item_too_large"
	DescriptionPromptTooLong       = "prompt_too_long"
	DescriptionContentTooLong      = "content_too_long"
	DescriptionCustomIDTooLong     = "custom_id_too_long"
	DescriptionMaskBase64TooLarge  = "mask_base64_too_large"
	DescriptionSourceTooLarge      = "source_base64_too_large"
	DescriptionTargetTooLarge      = "target_base64_too_large"
	DescriptionInvalidTaskID       = "invalid_task_id"
	DescriptionTooManyIDs          = "too_many_ids"
	DescriptionInvalidID           = "invalid_id"
	DescriptionIdempotencyTooLong  = "idempotency_key_too_long"
	DescriptionInvalidIdempotency  = "invalid_idempotency_key"
	DescriptionNoChannel           = "no_available_channel"
	DescriptionOriginTaskMissing   = "get_channel_info_failed"
	DescriptionChannelDisabled     = "channel_disabled"
	DescriptionTaskNotSuccess      = "action_task_not_success"
	DescriptionInternalError       = "internal_error"

	// 以下描述由 MJ-a2 的 HTTP/上游编排使用。全部是固定文案：错误体是
	// 客户端唯一可见的诊断面，不允许拼接上游 URL、渠道凭据或上游原始
	// description（上游可能把内部地址/账号信息带进错误文本）。
	DescriptionDisabled                 = "mj_not_enabled"
	DescriptionPlusChannelRequired      = "plus_channel_required"
	DescriptionQuotaNotEnough           = "quota_not_enough"
	DescriptionPricingUnavailable       = "task_pricing_unavailable"
	DescriptionBodyTooLarge             = "request_body_too_large"
	DescriptionChannelMisconfigured     = "channel_misconfigured"
	DescriptionUpstreamError            = "upstream_error"
	DescriptionUpstreamBadResponse      = "upstream_bad_response"
	DescriptionUpstreamResponseTooLarge = "upstream_response_too_large"
	DescriptionNoAccount                = "no_account_instance"
	DescriptionSensitiveWord            = "sensitive_word_detected"
	DescriptionGroupSaturated           = "group_saturated"
	DescriptionSubmitFailed             = "submit_failed"
	DescriptionInvalidToken             = "invalid_token"
	DescriptionForbidden                = "forbidden"
	DescriptionRateLimited              = "rate_limited"
)

// Error 是领域层错误：Code/Description 直接进 MJ 错误信封，Status 是给
// a2 的建议 HTTP 状态（本包不写响应，所以只给建议值）。
//
// 不实现 json.Marshaler：错误体只能由 Envelope 生成，避免有人把 Error
// 直接序列化出 Status 等额外字段（R-MJ-17 的「只有两个字段」）。
type Error struct {
	Code        int
	Description string
	Status      int
}

// Error 实现 error。字符串只用于日志，不进客户端响应。
func (e *Error) Error() string {
	if e == nil {
		return "mj: <nil>"
	}
	return fmt.Sprintf("mj: %s (code %d)", e.Description, e.Code)
}

// MarshalJSON 把 Error 折叠为 R-MJ-17 的两字段信封。
//
// 【为什么必须显式实现】Code/Description/Status 三个导出字段如果被默认
// 序列化，会得到 `{"Code":4,"Description":"...","Status":400}`——大小写与
// 字段集都不符合线协议，还把内部 HTTP 建议值暴露给客户端。任何直接
// json.Marshal(err)（日志、代理、测试）都必须只看到 {code,description}。
func (e Error) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Code        int    `json:"code"`
		Description string `json:"description"`
	}{Code: e.Code, Description: e.Description})
}

// HTTPStatus 返回建议状态码；未设置时按请求错误处理（400）。
func (e *Error) HTTPStatus() int {
	if e == nil || e.Status == 0 {
		return 400
	}
	return e.Status
}

// BadRequest 构造参数错误（code 4 / 400）。
func BadRequest(description string) *Error {
	return &Error{Code: CodeRequestError, Description: description, Status: 400}
}

// Forbidden 构造能力/门控拒绝（code 4 / 403）。
//
// 业务码仍用 4：MJ 客户端按 code 分支，403 只是 HTTP 状态；用 5 会让客户端
// 把「功能未开启/渠道能力不符」当成上游故障重试。
func Forbidden(description string) *Error {
	return &Error{Code: CodeRequestError, Description: description, Status: 403}
}

// ServiceUnavailable 构造「没有可用渠道」这类暂时性失败（code 5 / 503）。
func ServiceUnavailable(description string) *Error {
	return &Error{Code: CodeUnknownError, Description: description, Status: 503}
}

// Envelope 是 /mj/* 的响应信封：成功 {code:1, description, result}，
// 失败 {code≠0, description}（R-MJ-17）。
//
// Result 用 any + omitempty：成功路径必须带 result（可能是空数组、空串），
// 但错误路径不能出现 result 字段。omitempty 对非 nil 接口值不生效，
// 因此 NewSuccess(desc, []string{}) 仍会输出 "result":[]。
type Envelope struct {
	Code        int    `json:"code"`
	Description string `json:"description"`
	Result      any    `json:"result,omitempty"`
}

// NewSuccess 构造成功信封。description 沿用上游/固定文案，不做改写。
//
// result 为 nil 时补空数组：成功受理体按设计恒有 result 字段（上传端点是
// []string），缺字段会让客户端无法区分「空结果」与「旧/异常响应」。
func NewSuccess(description string, result any) Envelope {
	if result == nil {
		result = []string{}
	}
	return Envelope{Code: CodeSuccess, Description: description, Result: result}
}

// NewFailure 把任意 error 折叠成 MJ 错误信封。
//
// 非 *Error 一律退化为固定的 internal_error：绝不能把 err.Error() 写进
// description——底层错误可能包含 DSN、上游地址、响应片段或用户输入。
func NewFailure(err error) Envelope {
	var me *Error
	if errors.As(err, &me) && me != nil {
		return Envelope{Code: me.Code, Description: me.Description}
	}
	return Envelope{Code: CodeUnknownError, Description: DescriptionInternalError}
}

// CodeAccepted 报告上游返回码是否表示「任务已被受理」。
//
// 21/22/23 都是受理：21 可能已出结果，22 排队，23 队列已满。23 不返回
// 429/不重试——否则客户端会立刻重复建任务（G7-R51 裁决）。
func CodeAccepted(code int) bool {
	switch code {
	case CodeSuccess, CodeTaskExists, CodeQueued, CodeQueueFull:
		return true
	}
	return false
}

// ClientCode 把上游码重新编码为客户端可见码（甲类-2）。
//
// 21/22/23 → 1；其余原样。这样客户端看到的 code 与基线完全一致，而网关
// 不再需要修改上游 JSON 文本。
func ClientCode(code int) int {
	if CodeAccepted(code) {
		return CodeSuccess
	}
	return code
}

// UpstreamError 把「上游受理失败码」折叠为固定描述的错误信封。
//
// 只用于 code ∉ {1,21,22,23} 的提交结果。description 按码固定，绝不转发
// 上游原文：上游错误文本可能包含账号/代理地址等内部信息。客户端仍能看到
// 原始 code（R-MJ-13：响应透传上游 code）。
func UpstreamError(code int) *Error {
	switch code {
	case CodeNoAccount:
		return &Error{Code: CodeNoAccount, Description: DescriptionNoAccount, Status: 502}
	case CodeSensitiveWord:
		return &Error{Code: CodeSensitiveWord, Description: DescriptionSensitiveWord, Status: 400}
	case CodeGroupSaturated:
		return &Error{Code: CodeGroupSaturated, Description: DescriptionGroupSaturated, Status: 429}
	}
	return &Error{Code: code, Description: DescriptionSubmitFailed, Status: 502}
}

// QuotaNotEnough 构造余额不足错误（402，业务码 4）。
func QuotaNotEnough() *Error {
	return &Error{Code: CodeRequestError, Description: DescriptionQuotaNotEnough, Status: 402}
}

// PricingUnavailable 构造「模型未配置价格」错误（400，业务码 4）。
//
// 与余额不足分开：管理员没配价不是「充值就能用」，客户端/运营需要能区分。
func PricingUnavailable() *Error {
	return &Error{Code: CodeRequestError, Description: DescriptionPricingUnavailable, Status: 400}
}
