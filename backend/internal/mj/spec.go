package mj

import (
	"crypto/sha256"
	"strings"
	"unicode"
)

// 输入上界（R-MJ-16，冻结值）。
//
// 【为什么领域层也要校验】HTTP 层的 body ≤32MiB 只能限制总量：一个 32MiB
// 的 JSON 里仍可以塞 100 万个小 base64 项、超长 prompt 或超长 ids 数组，
// 让解析与后续渠道选择放大成 CPU/内存故障。用户可控量必须先有独立上界，
// 超限统一 400 固定错误（不回显输入）。
const (
	MaxBase64Items     = 8        // base64Array 最多 8 项
	MaxBase64ItemBytes = 8 << 20  // 单项 base64 文本 8MiB
	MaxPromptBytes     = 32 << 10 // prompt 32KiB
	MaxContentBytes    = 1 << 10  // content 1KiB
	MaxCustomIDBytes   = 1 << 10  // customId 1KiB
	MaxListIDs         = 100      // list-by-condition.ids 最多 100
	MaxIdempotencyKey  = 1 << 10  // Idempotency-Key 1KiB
)

// SubmitRequest 是 12 条提交端点的统一入参（relay-05 §2.1）。
//
// 指针语义（AGENTS.md 中继指针规则）：上游可选标量一律指针 + omitempty，
// 「缺席」与「显式零值」必须可区分——例如 index=0 是非法入参，而 index
// 缺席是另一条错误。base64Array 用切片（nil/空都表示未提供）。
//
// 本结构只承载客户端字段；网关侧字段（user/token/channel）不在这里，
// 避免归一化层意外把凭据或归属信息带进上游请求体。
type SubmitRequest struct {
	Prompt        *string        `json:"prompt,omitempty"`
	CustomID      *string        `json:"customId,omitempty"`
	BotType       *string        `json:"botType,omitempty"`
	NotifyHook    *string        `json:"notifyHook,omitempty"`
	Action        *string        `json:"action,omitempty"`
	Index         *int           `json:"index,omitempty"`
	State         *string        `json:"state,omitempty"`
	TaskID        *string        `json:"taskId,omitempty"`
	Base64Array   []string       `json:"base64Array,omitempty"`
	Content       *string        `json:"content,omitempty"`
	MaskBase64    *string        `json:"maskBase64,omitempty"`
	Dimensions    *string        `json:"dimensions,omitempty"`
	SourceBase64  *string        `json:"sourceBase64,omitempty"`
	TargetBase64  *string        `json:"targetBase64,omitempty"`
	AccountFilter map[string]any `json:"accountFilter,omitempty"`
}

// Options 是归一化时的透传开关（mj.* 设置由 a2 读取后传入）。
//
// 零值 = 全部关闭，也就是设计默认：剥离 accountFilter/notifyHook、保留
// prompt 里的模式标志。fail-closed 的默认值让「设置读失败」退化为基线
// 行为，而不是把用户提供的回调地址意外发给上游。
type Options struct {
	// ForwardAccountFilter 对应 mj.account_filter_enabled：透传给上游选号。
	ForwardAccountFilter bool
	// ForwardNotifyHook 对应 mj.notify_enabled：透传用户自备回调地址。
	// 网关自身不提供 /mj/notify 接收端点（甲类-4）。
	ForwardNotifyHook bool
	// ClearModeFlags 对应 mj.mode_clear_enabled：从 prompt 去掉
	// --fast/--relax/--turbo（基线按字面量删除，不重排空格）。
	ClearModeFlags bool
}

// NormalizedSubmit 是校验/解析后的提交 IR，供 a2 组装上游请求与任务行。
type NormalizedSubmit struct {
	Endpoint Endpoint
	Action   Action
	Index    int
	Model    string
	// TaskID 是动作类端点的原任务 MjId；simple-change 从 content 解析，
	// 其余端点取 taskId 字段。空串 = 不是基于既有任务的动作。
	TaskID string

	Prompt     *string
	Content    *string
	CustomID   *string
	BotType    *string
	State      *string
	Dimensions *string
	MaskBase64 *string

	Base64Array  []string
	SourceBase64 *string
	TargetBase64 *string

	// AccountFilter / NotifyHook 已经按 Options 剥离；nil 表示不得转发。
	AccountFilter map[string]any
	NotifyHook    *string

	PlusOnly bool
	// LockOrigin 表示必须使用原任务渠道；a2 在提交前按 ResolveLock 校验。
	LockOrigin bool
}

// Normalize 校验并归一化一次提交。
//
// 顺序固定为「端点 → 上界 → 必填 → 动作解析」：上界先于任何字符串解析，
// 保证超长输入不会先被业务逻辑处理；动作解析最后，因为它依赖必填字段
// 存在（change 的 action、action 端点的 customId、simple-change 的 content）。
func Normalize(endpoint Endpoint, req *SubmitRequest, opts Options) (*NormalizedSubmit, *Error) {
	if !endpoint.Valid() {
		return nil, BadRequest(DescriptionUnknownEndpoint)
	}
	if req == nil {
		return nil, BadRequest(DescriptionInvalidRequest)
	}
	if err := checkBounds(req); err != nil {
		return nil, err
	}
	if err := checkRequired(endpoint, req); err != nil {
		return nil, err
	}

	resolved, err := ResolveAction(endpoint, req)
	if err != nil {
		return nil, err
	}
	model, ok := resolved.Action.Model()
	if !ok {
		// ResolveAction 已保证动作合法；这里是纵深防御，绝不返回空模型。
		return nil, BadRequest(DescriptionUnknownAction)
	}

	n := &NormalizedSubmit{
		Endpoint:     endpoint,
		Action:       resolved.Action,
		Index:        resolved.Index,
		Model:        model,
		Prompt:       req.Prompt,
		Content:      req.Content,
		CustomID:     req.CustomID,
		BotType:      req.BotType,
		State:        req.State,
		Dimensions:   req.Dimensions,
		MaskBase64:   req.MaskBase64,
		SourceBase64: req.SourceBase64,
		TargetBase64: req.TargetBase64,
		PlusOnly:     RequiresPlus(endpoint),
	}

	if req.TaskID != nil && strings.TrimSpace(*req.TaskID) != "" {
		n.TaskID = strings.TrimSpace(*req.TaskID)
	}
	if endpoint == EndpointSimpleChange {
		// content 已由 ResolveAction 解析校验；taskId 以解析结果为准。
		n.TaskID, _, _, _ = ParseSimpleChange(*req.Content)
	}

	if len(req.Base64Array) > 0 {
		n.Base64Array = append([]string(nil), req.Base64Array...)
	}

	if opts.ForwardAccountFilter {
		n.AccountFilter = req.AccountFilter
	}
	if opts.ForwardNotifyHook {
		n.NotifyHook = req.NotifyHook
	}

	if opts.ClearModeFlags && n.Prompt != nil {
		// 新建字符串而不是原地修改：调用方的 req 可能被日志/审计复用，
		// 归一化不得产生副作用。
		cleared := stripModeFlags(*n.Prompt)
		n.Prompt = &cleared
	}

	n.LockOrigin = LocksOrigin(endpoint, n.TaskID)
	return n, nil
}

// checkBounds 执行 R-MJ-16 的全部上界。
//
// 所有超限都返回固定的 400 错误；description 只说明哪一类超限，不含长度
// 或输入片段，避免把用户内容带进响应/日志。
func checkBounds(req *SubmitRequest) *Error {
	if req.Prompt != nil && len(*req.Prompt) > MaxPromptBytes {
		return BadRequest(DescriptionPromptTooLong)
	}
	if req.Content != nil && len(*req.Content) > MaxContentBytes {
		return BadRequest(DescriptionContentTooLong)
	}
	if req.CustomID != nil && len(*req.CustomID) > MaxCustomIDBytes {
		return BadRequest(DescriptionCustomIDTooLong)
	}
	if len(req.Base64Array) > MaxBase64Items {
		return BadRequest(DescriptionBase64ArrayTooLarge)
	}
	for _, item := range req.Base64Array {
		if len(item) > MaxBase64ItemBytes {
			return BadRequest(DescriptionBase64ItemTooLarge)
		}
	}
	if req.MaskBase64 != nil && len(*req.MaskBase64) > MaxBase64ItemBytes {
		return BadRequest(DescriptionMaskBase64TooLarge)
	}
	if req.SourceBase64 != nil && len(*req.SourceBase64) > MaxBase64ItemBytes {
		return BadRequest(DescriptionSourceTooLarge)
	}
	if req.TargetBase64 != nil && len(*req.TargetBase64) > MaxBase64ItemBytes {
		return BadRequest(DescriptionTargetTooLarge)
	}
	if req.TaskID != nil {
		trimmed := strings.TrimSpace(*req.TaskID)
		if trimmed != "" && !ValidMjID(trimmed) {
			return BadRequest(DescriptionInvalidTaskID)
		}
	}
	return nil
}

// checkRequired 是逐端点必填校验（relay-05 §2.2）。
func checkRequired(endpoint Endpoint, req *SubmitRequest) *Error {
	switch endpoint {
	case EndpointImagine, EndpointShorten, EndpointEdits:
		if !present(req.Prompt) {
			return BadRequest(DescriptionPromptRequired)
		}
	case EndpointChange:
		if !present(req.TaskID) {
			return BadRequest(DescriptionTaskIDRequired)
		}
		if !present(req.Action) {
			return BadRequest(DescriptionActionRequired)
		}
		if req.Index == nil || *req.Index == 0 {
			return BadRequest(DescriptionIndexRequired)
		}
	case EndpointSimpleChange:
		if !present(req.Content) {
			return BadRequest(DescriptionContentRequired)
		}
	case EndpointAction:
		if !present(req.CustomID) {
			return BadRequest(DescriptionCustomIDRequired)
		}
	case EndpointModal, EndpointVideo:
		if !present(req.TaskID) {
			return BadRequest(DescriptionTaskIDRequired)
		}
	case EndpointDescribe, EndpointBlend, EndpointUploadDiscord:
		if len(req.Base64Array) == 0 {
			return BadRequest(DescriptionBase64Required)
		}
	case EndpointInsightFace:
		if !present(req.SourceBase64) {
			return BadRequest(DescriptionSourceRequired)
		}
		if !present(req.TargetBase64) {
			return BadRequest(DescriptionTargetRequired)
		}
	}
	return nil
}

// present 报告字符串指针是否存在且非空白。
//
// 空白视同缺失：prompt="   " 既没有语义，又会让上游生成空提示词任务并
// 计入固定价。customId/taskId 同理（空白 id 不可能存在）。
func present(s *string) bool {
	return s != nil && strings.TrimSpace(*s) != ""
}

// stripModeFlags 删除 prompt 中的 --fast/--relax/--turbo 三类模式标志。
//
// 按**完整 token** 删除，不做子串替换：strings.ReplaceAll("--fast") 会把
// "--fastest" 改写成 "est"，用户提示词语义被破坏。没有可删除的标志时
// 原样返回（避免不必要的空白归一化）；有删除时按单空格重新连接。
func stripModeFlags(prompt string) string {
	fields := strings.Fields(prompt)
	if len(fields) == 0 {
		return prompt
	}
	kept := make([]string, 0, len(fields))
	removed := false
	for _, field := range fields {
		switch strings.ToLower(field) {
		case "--fast", "--relax", "--turbo":
			removed = true
			continue
		}
		kept = append(kept, field)
	}
	if !removed {
		return prompt
	}
	return strings.Join(kept, " ")
}

// SubmitRequestHash 计算一次提交的幂等请求指纹。
//
// 组成是「端点 + 0x00 + 原始请求体」：
//   - 端点必须进哈希：同一个键被复用到 /submit/imagine 与 /submit/blend 是
//     两个不同的副作用，不能算重放；
//   - 原始体进哈希：客户端重放时发送的字节可能不完全一致（字段顺序/空白），
//     我们按「不相同即冲突」的保守语义处理——把语义等价但字节不同的请求
//     当成冲突，会得到 409，而不是悄悄执行第二个副作用。反过来放宽成
//     「解析后 IR 相同」需要一份稳定的规范化序列化，任何字段漏掉都会让
//     两个不同请求命中同一指纹（更危险）。
//
// 0x00 分隔符不在 JSON/路径字符集里，避免「端点+体」拼接的二义性。
func SubmitRequestHash(endpoint Endpoint, body []byte) []byte {
	h := sha256.New()
	h.Write([]byte(endpoint))
	h.Write([]byte{0})
	h.Write(body)
	return h.Sum(nil)
}

// IdempotencyKeyHeader 是客户端可重放提交的幂等键请求头（R-MJ-06 第一步）。
//
// 与计费幂等键 RelayContext.RequestID 严格分离：前者由客户端提供、用于
// 「同一次提交重放」；后者由网关生成、是资金侧唯一锚点。混用会让重放
// 请求命中别人的结算会话。
const IdempotencyKeyHeader = "Idempotency-Key"

// ParseIdempotencyKey 解析可选的 Idempotency-Key 头。
//
// 返回 (key, present, err)。缺省/空白 → present=false；超长或含控制字符
// → 400。完整幂等表（(user,key) 去重与 24h 窗口）排 MJ-d；本阶段只保证
// 解析入口存在且上界明确，供 a2 原样透传/记录。
func ParseIdempotencyKey(raw string) (string, bool, *Error) {
	key := strings.TrimSpace(raw)
	if key == "" {
		return "", false, nil
	}
	if len(key) > MaxIdempotencyKey {
		return "", false, BadRequest(DescriptionIdempotencyTooLong)
	}
	if strings.ContainsFunc(key, unicode.IsControl) {
		return "", false, BadRequest(DescriptionInvalidIdempotency)
	}
	return key, true, nil
}
