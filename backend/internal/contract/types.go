// Package contract 是 NewTG **中继 + 计费**两侧共用的**冻结接口契约**的 Go 表达。
//
// 路径为中性（`internal/contract`）而非挂在 relay 下 —— 因为中继与计费共用同一个
// 契约面（IR、用量事实、定价快照、计费会话），拆成两个包会产生 import 循环。
//
// 来源：
//   - 中继侧（IR / 适配器 / 上下文 / 渠道类型）→ `docs/design/relay/00-中继核心接口契约.md` v1.1
//   - 计费侧（定价模式 / 快照 / 归一化 / 配额数学）→ `docs/design/billing/00-计费引擎接口契约.md` v1.0
//
// 唯一目的是让编译器替我们检查契约的自洽性：任何「被引用却没有定义」的类型，
// `go build` 都会报 undefined。
//
// 纪律：
//   - 本文件只放**类型与常量**，不放任何逻辑实现。
//   - 转写不得「顺手补全」——契约里没有的类型就不在这里造，让编译器报出来。
//   - 契约修订时本文件必须同步；反之亦然（本文件编译不过 = 契约有缺口）。
package contract

import (
	"io"
	"iter"
	"net/http"
)

// ============================================================================
// §2.2 请求（IR 双层结构）
// ============================================================================

type Request struct {
	// —— 归一化层 ——
	Model          string // 客户端请求的模型名（未映射）
	Stream         bool
	Messages       []Message // 归一化消息序列
	Tools          []Tool
	ToolChoice     ToolChoice
	ResponseFormat *ResponseFormat

	// 可选标量一律用指针：缺席 = nil（不发），显式 0/false = 非 nil（要发）
	MaxTokens   *int
	Temperature *float64
	TopP        *float64
	Stop        []string
	Seed        *int

	// —— 原始层 ——
	RawFormat RelayFormat  // 入站协议
	Raw       ProtocolBody // 原始请求体（透传与未映射字段的唯一来源）

	// AnthropicBeta 是客户端 `anthropic-beta` 头经白名单过滤后的值
	// （R-W18-05；逗号分隔的 beta 标识，如 "interleaved-thinking-2025-05-14"）。
	//
	// 语义与零值：
	//   - 空串 = 客户端未提供，或提供的值全部不在白名单内；适配器**不得**发送
	//     该头（空头与「没有 beta」在上游语义上不同，不能互写）。
	//   - 写入方唯一：relay.BuildRelayContext 在 HTTP 边界解析并按白名单过滤；
	//     归一化层经 WithRequest 从 RelayContext 复制，不重新解析头。
	//   - 读取方唯一：adapters.Anthropic 的 BuildRequest / BuildCountTokensRequest。
	//   - 该值**不得**进入日志/审计（与凭据同级纪律，R-W18-05）。
	AnthropicBeta string
}

type Message struct {
	Role    Role          // system | developer | user | assistant | tool
	Content []ContentPart // 文本 / 图片 / 音频 / 工具调用 / 工具结果
	// 协议特有但需保留的字段放这里，不放 Raw，因为它是「消息级」语义
	Extensions map[string]any
}

type ContentPart struct {
	Kind    PartKind // text | image_url | image_b64 | audio | tool_call | tool_result
	Text    string
	Payload []byte // 二进制内容（图片/音频），与 URL 二选一
	URL     string
	Meta    map[string]any
}

// ============================================================================
// §2.3 响应与用量
// ============================================================================

type Response struct {
	ID           string
	Model        string // 上游声明的模型名（用于诊断与对账）
	Choices      []Choice
	FinishReason string
	Raw          ProtocolBody // 上游原始响应体

	// —— 二进制载体（v1.1 · 决议 C3，合并 CCR-10）——
	// TTS 等非 JSON 响应：一次性响应走 Binary；流式响应走 BinaryStream。
	// 二者与 Choices/Raw 互斥：命中二进制载体时无 JSON 结构可解析。
	Binary       []byte
	BinaryStream io.Reader
}

type Usage struct {
	// —— 归一化维度（供通用逻辑与日志）——
	Prompt     int64
	Completion int64
	Total      int64

	// —— 关系布尔（v1.1 · 决议 C1，合并 CR-C-01；登记为强制字段）——
	// 方向搞反会成倍错算费用，适配器必须显式赋值。
	PromptIncludesCache           bool // true: Prompt 已含 CacheRead（GPT 格式）；false: CacheRead 为独立维度（Claude/Gemini）
	ReasoningIncludedInCompletion bool // true: Completion 已含 Reasoning（Gemini）；false: Reasoning 为独立维度

	// —— 独立计费维度（不可并入上面三个）——
	CacheRead    int64 // 缓存读（归一后口径）
	CacheWrite   int64 // 缓存写（归一后口径）
	Reasoning    int64 // 思考 token
	AudioSeconds int64 // 音频时长（秒）（v1.1 · 决议 C2，合并 CCR-5）

	// —— 子类明细（阶梯表达式结算需要）——
	PromptDetail     UsageDetail // 图片 / 音频 / 文本 各自的 prompt 构成
	CompletionDetail UsageDetail

	// —— 来源与可信度 ——
	Source UsageSource    // upstream_exact | upstream_partial | estimated | reserved_default
	Raw    map[string]any // 协议原始 usage，用于对账与审计

	// —— 异常时的保护 ——
	Reserved int64
}

type UsageDetail struct {
	Image                 int64 // 图像 token
	Audio                 int64 // 音频 token
	Text                  int64 // 文本 token
	CacheRead, CacheWrite int64
	CacheReadImage        int64 // 【M3 补】图片缓存读（img_cr 的承载位，契约 §5.4 三段式拆分用）
}

type BillingUnit int

const (
	UnitCount  BillingUnit = iota // 张/个
	UnitSecond                    // 秒
	UnitToken                     // token
	UnitCredit                    // 积分
)

// ResultUnit 是**表达式求值结果**的单位（与 BillingUnit 不同：后者是任务用量事实的单位）。
//
// 【命名避让 · M3 决议 B2】`BillingUnit` 一词曾被兼指三件事（任务用量单位、
// 结果单位、日志键 `billing_unit`）。现拆分：结果单位独立为 `ResultUnit`；
// 日志键 `billing_unit` 保持原名（既有对外字段，不改名）。
type ResultUnit int

const (
	ResultToken   ResultUnit = iota // 结果按 token 计价（$/1M tokens）
	ResultRequest                   // 结果按请求固定价（USD/request）
)

// UsageSource 字面量冻结唯一一套（v1.1 · 决议 C5）。
type UsageSource int

const (
	UsageUpstreamExact   UsageSource = iota // 序列化字面量：upstream_exact
	UsageUpstreamPartial                    // 序列化字面量：upstream_partial
	UsageEstimated                          // 序列化字面量：estimated
	UsageReservedDefault                    // 序列化字面量：reserved_default
)

// ============================================================================
// §2.4 流式事件
// ============================================================================

type StreamEvent struct {
	Kind      EventKind // delta | usage | finish | error | ping | raw
	Delta     string    // 文本增量
	ToolDelta *ToolCallDelta
	Reasoning string // 思考增量（独立通道）
	Usage     *Usage // 出现时机由适配器决定（见契约 §9）
	Model     string // 上游声明模型
	Raw       []byte // 原始 SSE 事件体（转码时保留）
}

type EventKind int

const (
	EventDelta EventKind = iota
	EventUsage
	EventFinish
	EventError
	EventPing
	EventRaw
)

// ============================================================================
// §2.5 任务契约（v1.1 · 决议 A1–A5）
// ============================================================================

type TaskFamily int

const (
	FamilyMJ     TaskFamily = iota // Midjourney 系（绘图任务族 05）
	FamilyPlugin                   // 任务插件平台（06）
)

// FetchMode 是**本系统如何从上游取回任务结果**。
//
// ⚠️ 命名避让（v1.1 · 补遗 8）：与任务插件清单里的 `meta.fetchMode`
// （取值 per_task / batch）**同名不同义**——后者描述插件侧一次取回几条任务。
// 二者不得互相赋值或混引；插件清单的 per_task 在本系统侧一律映射为 FetchPoll。
type FetchMode int

const (
	FetchPoll     FetchMode = iota // 轮询上游
	FetchCallback                  // 上游回调网关
	FetchInline                    // 提交即返回结果（同步任务）
)

// TaskState 冻结为 7 态超集（v1.1 · 决议 A2）：
// 05 的 5 态并入 06 的 7 态；废弃 05 的 PENDING（→ TaskNotStart）与 RUNNING（→ TaskInProgress）。
type TaskState int

const (
	TaskNotStart   TaskState = iota // 未开始（原 05 PENDING）
	TaskSubmitted                   // 已提交
	TaskQueued                      // 排队中
	TaskInProgress                  // 进行中（原 05 RUNNING）
	TaskSuccess                     // 终态：成功
	TaskFailure                     // 终态：失败
	TaskUnknown                     // 未知
)

type TaskHandle struct {
	PublicTaskID     string     // 对外任务 ID（唯一，替代 TaskID/PublicTaskID 两种写法）
	UpstreamTaskID   string     // 上游返回的任务 ID
	Family           TaskFamily // mj | plugin —— 判别键，解决两族 Action 枚举不兼容
	Action           string     // 族内动作标识；解释权归 Family
	PluginKey        string     // 仅 Family=plugin
	PluginGeneration int        // 仅 Family=plugin（插件版本变更时失效旧任务）
	FetchMode        FetchMode  // poll | callback | inline
	LockedChannelID  int        // 锁定渠道 ID；0 = 未锁定
	OriginTaskIDs    []string   // 动作类请求的原始任务（替代 OrigTaskID 单值）
	State            TaskState  // 见上：7 态
}

type TaskStatus struct {
	State           TaskState
	RawState        string        // 上游原始状态串（保留用于诊断）
	ProgressPercent int           // 0–100；上游只给字符串进度时由适配器归一
	Usage           TaskUsageFact // 见下
	Artifacts       []Artifact    // 产物（按需投影的能力 URL）
	FailReason      string
	PluginState     map[string]any // 仅 Family=plugin，插件自定义态
}

// TaskUsageFact 是任务用量事实（v1.1 · 决议 A1/A5）。
type TaskUsageFact struct {
	// 【键 = 插件在 meta.usageSchema 里声明的字段名】，**不是单位枚举** ——
	// 同一单位可有多个字段（如 seconds 与 clips 都是 second），按单位做键会互相覆盖。
	Facts  map[string]UsageFactValue // 字段名 → 取值；键必须是插件已声明的字面量键
	Units  map[string]BillingUnit    // 字段名 → 单位（与 Facts 同键；枚举字段无单位，可不入表）
	Source UsageSource
}

// UsageFactValue 是任务用量事实的取值：**数值或字符串**。
//
// 【为什么不能只用 float64 · M3 决议 A2b】任务用量表达式同时支持
// **数值档位**（`u("seconds") * 0.4`）与**枚举档位**（`u("mode") == "pro" ? ... : ...`）。
// 插件 schema 里既有数值字段也有枚举字段，二者共用同一张 Facts 表；
// 用 `map[string]float64` 会把枚举字段直接堵死。
type UsageFactValue struct {
	Num *float64 // 数值事实（数值字段）；与 Str 互斥
	Str *string  // 字符串事实（枚举字段）；与 Num 互斥
}

type Artifact struct {
	Kind string // image | video | audio | file …
	URL  string // 产物能力 URL（按需投影）
	Meta map[string]any
}

// ============================================================================
// §3.1 适配器接口（小接口拆分）
// ============================================================================

// Adapter 是所有渠道必须实现的接口。
type Adapter interface {
	Type() ChannelType
	Caps() Capabilities
	BuildRequest(*RelayContext, *Request) (*http.Request, error)
	ParseResponse(*RelayContext, *http.Response) (*Response, Usage, error)
	ClassifyError(status int, body []byte) ErrorClass
}

// StreamAdapter 是流式能力（v1.1 · 决议 D3：入参由 io.Reader 泛化为 io.ReadWriter，
// 单向 SSE 与双向 WS 共用同一事件迭代器）。
type StreamAdapter interface {
	Adapter
	StreamEvents(*RelayContext, io.ReadWriter) iter.Seq[StreamEvent]
}

// ImageAdapter 是图像可计费量推导（仅图像类渠道实现）。
type ImageAdapter interface {
	Adapter
	CountImages(payload []byte) (count int, ok bool) // ok=false → 计费引擎保持预留量
}

// ChannelImageUsageAdapter 是渠道专用上游图像计数（v1.1 · 决议 D2）。
// 信任顺序见契约 §9.2：有界上游计数优先 → payload 推导 → 保留预留量。
type ChannelImageUsageAdapter interface {
	ImageAdapter
	ImageUsage(*RelayContext, *http.Response) (count int, ok bool) // ok=false → 落到下一级信任
}

// ModelListAdapter 提供上游模型列表。
type ModelListAdapter interface {
	Adapter
	FetchModels(*RelayContext) ([]string, error)
}

// TaskAdapter 是任务平台（视频/音乐/绘图）。
type TaskAdapter interface {
	Adapter
	Submit(*RelayContext, *Request) (*TaskHandle, error)
	Query(*RelayContext, *TaskHandle) (*TaskStatus, error)
	Cancel(*RelayContext, *TaskHandle) error
	// v1.1 · 决议 A5：返回类型由 (Usage, error) 改为 TaskUsageFact
	TaskUsage(*TaskStatus) (TaskUsageFact, error)
}

// 多模态端点（嵌入 / 重排）。
type EmbeddingAdapter interface {
	Adapter
	Embeddings(*RelayContext, *Request) (*EmbeddingResponse, Usage, error)
}

type RerankAdapter interface {
	Adapter
	Rerank(*RelayContext, *Request) (*RerankResponse, Usage, error)
}

// 音频三接口拆分（v1.1 · 决议 D1；替代原单一 AudioAdapter）。
type SpeechAdapter interface {
	Adapter
	Speech(*RelayContext, *Request) (*AudioResponse, Usage, error)
}

type TranscriptionAdapter interface {
	Adapter
	Transcribe(*RelayContext, *Request) (*AudioResponse, Usage, error)
}

type TranslationAdapter interface {
	Adapter
	Translate(*RelayContext, *Request) (*AudioResponse, Usage, error)
}

// RealtimeAdapter 是 WebSocket 形态（Responses / Realtime）。
// DialUpstream 负责建连；建连后的事件迭代统一走 StreamAdapter.StreamEvents。
type RealtimeAdapter interface {
	Adapter
	DialUpstream(*RelayContext) (UpstreamConn, error)
}

// ============================================================================
// §3.1 能力声明
// ============================================================================

// Endpoint 枚举一次冻结（v1.1 · 决议 D6，阻断项 3）。
type Endpoint string

const (
	EndpointChat            Endpoint = "chat"
	EndpointMessages        Endpoint = "messages"
	EndpointGenerateContent Endpoint = "generate_content"
	EndpointCompletions     Endpoint = "completions"
	EndpointEmbeddings      Endpoint = "embeddings"
	EndpointRerank          Endpoint = "rerank"
	EndpointModerations     Endpoint = "moderations"
	EndpointAlphaSearch     Endpoint = "alpha_search" // 补：原枚举遗漏（CCR-1）
	EndpointAudioSpeech     Endpoint = "audio_speech"
	EndpointAudioTranscribe Endpoint = "audio_transcribe"
	EndpointAudioTranslate  Endpoint = "audio_translate"
	EndpointImageGenerate   Endpoint = "image_generate"
	EndpointImageEdit       Endpoint = "image_edit"
	EndpointImageVariation  Endpoint = "image_variation"
	EndpointResponses       Endpoint = "responses"
	EndpointResponsesWS     Endpoint = "responses_ws" // WS：失败语义=连接级终止
	EndpointRealtime        Endpoint = "realtime"     // WS：失败语义=连接级终止
	EndpointModels          Endpoint = "models"
	EndpointMJSubmit        Endpoint = "mj_submit" // 补：05 使用却漏登（D6）
	EndpointMJTask          Endpoint = "mj_task"   // 补：05 使用却漏登（D6）
	EndpointMJImage         Endpoint = "mj_image"  // 补：05 使用却漏登（D6）
)

type Capabilities struct {
	Endpoints Set[Endpoint] // 见上：一次冻结的完整枚举

	Stream      bool         // 是否支持流式
	StreamUsage bool         // 是否需显式请求用量（对应基线的 StreamOptions）
	ModelList   bool         // 可否拉取模型列表
	MultiKey    MultiKeyMode // none | random | polling

	// —— v1.1 · 决议 D5（合并 8 条 CR）新增字段 ——
	ChannelRateLimit         *RateLimitSpec       // 渠道级限流声明
	CredentialFields         []FieldSpec          // 凭据字段规格（替代散落表单提示）
	ExtraFields              []FieldSpec          // 渠道附加字段规格
	OtherSemantic            OtherSemantic        // 多义 other 的声明（如 Gemini 类型 24）
	Roles                    []ChannelRole        // 渠道身份（可多个：对话 / 任务双重身份）
	FieldPassthrough         FieldPassthroughSpec // 透传字段白名单（三组互异集合）
	SupportsResponsesCompact bool                 // Responses compact 子能力
	SupportsResponsesWS      bool                 // Responses WebSocket 子能力
	BaseURLMode              BaseURLMode          // 基址解析模式
	StreamAggregatesUsage    bool                 // 流式需跨块累计 usage（Gemini 等）
	TaskSpec                 *TaskSpec            // 任务平台声明（仅 TaskAdapter）

	TaskPlatforms  []string    // 任务插件键（仅 TaskAdapter）
	CreditShape    CreditShape // 凭据形态，驱动渠表单的字段提示
	DefaultBaseURL string      // 默认基址；空表示必须由渠道配置
	SpecialBase    bool        // 是否走特殊基址解析
}

type CreditShape int

const (
	CreditSingleKey    CreditShape = iota // 单 api_key
	CreditKeyMultiLine                    // 多行 key（每行一个）
	CreditPipedFields                     // Ak|Sk|Region 之类
	CreditOAuthJSON                       // OAuth/Codex JSON
	CreditJSONObject                      // 结构化 JSON（如 Vertex）
	CreditMultiField                      // 多独立字段
)

// ChannelRole 决定「同一 ChannelType 按角色分派到不同适配器」（v1.1 · 补遗 7）。
// 基线中类型 1/17/24/35/41/45/55 兼具对话与任务身份，故单值枚举不够，用位集合。
type ChannelRole int

const (
	RoleChat ChannelRole = 1 << iota // 对话与多模态端点
	RoleTask                         // 任务平台端点
)

// FieldPassthroughSpec 是透传字段白名单：三组互异集合，语义与生效条件各不相同。
// 【不可压平为 []string】——压平会丢失集合归属。
type FieldPassthroughSpec struct {
	ServiceTier  []string // service_tier 类字段
	OpenAICompat []string // OpenAI 兼容字段
	ClaudeCompat []string // Claude 兼容字段
}

// ============================================================================
// §3.3 DispatchTable / AdapterRegistry
// ============================================================================

type DispatchEntry struct {
	AdapterKey     string // 适配器注册键
	FallbackTo     string // 回退目标键（空 = 无回退）
	ForbidFallback bool   // 显式禁止回退
}

// DispatchTable 是渠道类型 → 分派入口。
type DispatchTable map[ChannelType]DispatchEntry

type AdapterRegistry interface {
	Register(key string, a Adapter)
	Resolve(key string) (Adapter, bool)
	// Dispatch 两级解析 + 按角色分派（v1.1 · 补遗 7）。
	Dispatch(t ChannelType, role ChannelRole) (Adapter, error)
}

// ============================================================================
// §4 错误分类与重试语义
// ============================================================================

type ErrorClass int

const (
	ErrUnknown           ErrorClass = iota
	ErrRetryable                    // 可重试（上游瞬时故障）
	ErrUpstreamAuth                 // 上游鉴权失败（凭据问题）
	ErrUpstreamRateLimit            // 上游限流
	ErrUpstreamServer               // 上游 5xx
	ErrParamInvalid                 // 请求参数非法（不重试）
	ErrChannelConfig                // 渠道配置错误（不重试，可自动禁用）
	ErrContentFilter                // 内容审核拦截（不重试）
	ErrInsufficientQuota            // 额度不足（不重试）
	ErrChannelError                 // 渠道级错误，强制可重试 + 可自动禁用

	// 该渠道不支持该端点：不重试、不自动禁用（v1.1 · 决议 E2）。
	ErrEndpointUnsupported

	// 渠道类型整体无适配器实现（如基线类型 21 GetAdaptor 返回 nil）：
	// 属渠道配置问题 → 不重试、可自动禁用（v1.1 · 补遗 7）。
	ErrChannelTypeUnimplemented

	// 连接级终止：渠道握手变更 → 403 要求重连；不做请求级重试（v1.1 · 决议 E4）。
	ErrConnectionTerminated
)

// String 返回 ErrorClass 的稳定标签（日志 / usage_logs / 指标标签共用）。
//
// 只用于服务端诊断与有界枚举分组，不面向客户端文案（客户端文案仍由
// httpapi 的 mapRelayError 决定）。未知取值一律回落 "unknown"：新增枚举
// 而忘记补标签时，宁可分组到 unknown，也不能让数字进入日志标签。
func (c ErrorClass) String() string {
	switch c {
	case ErrRetryable:
		return "retryable"
	case ErrUpstreamAuth:
		return "upstream_auth"
	case ErrUpstreamRateLimit:
		return "upstream_rate_limit"
	case ErrUpstreamServer:
		return "upstream_server"
	case ErrParamInvalid:
		return "param_invalid"
	case ErrChannelConfig:
		return "channel_config"
	case ErrContentFilter:
		return "content_filter"
	case ErrInsufficientQuota:
		return "insufficient_quota"
	case ErrChannelError:
		return "channel_error"
	case ErrEndpointUnsupported:
		return "endpoint_unsupported"
	case ErrChannelTypeUnimplemented:
		return "channel_type_unimplemented"
	case ErrConnectionTerminated:
		return "connection_terminated"
	default:
		return "unknown"
	}
}

// RelayError 是三态判定的承载类型（v1.1 · 决议 E1）。
type RelayError struct {
	Class        ErrorClass
	StatusCode   int    // 上游 HTTP 状态码；0 表示非 HTTP 错误
	Code         string // 内部错误码（"channel:" 前缀为渠道级）
	SkipRetry    bool   // 显式跳过重试标记
	IsChannelErr bool   // 渠道级错误（最先判定）
	Message      string
}

// PollOutcome 是任务轮询专用分类；它是 ErrorClass 的任务侧投影，不是平行体系。
type PollOutcome int

const (
	PollOK            PollOutcome = iota // 正常
	PollImmediateFail                    // 立即失败（不可重试）
	PollAuthFail                         // 鉴权失败
	PollRetryable                        // 可重试
	PollUnrecognized                     // 未识别
)

// ============================================================================
// §5 中继上下文
// ============================================================================

type AttemptPlan struct {
	Candidates  []ChannelCandidate // 按优先级降序
	MaxAttempts int
	CrossGroup  bool // 是否允许跨分组重试
}

type ChannelCandidate struct {
	ChannelID   int
	Priority    int
	Weight      uint
	Group       string
	Constraints ChannelConstraints
}

type ChannelConstraints struct {
	PinChannelID int
	RetryMode    PinRetryMode
	SessionMode  SessionMode
	Filters      []ChannelFilter // request_path | task_plugin_identity | responses_websocket
}

// ConnectionContext 是连接级状态（跨多次请求/帧）。RelayContext 仍是「一次请求」状态。
type ConnectionContext struct {
	LockedChannelSnapshot ChannelSnapshot       // 建连时锁定的渠道快照
	LastResponseID        string                // Responses 会话串联
	Accumulator           ConnectionAccumulator // 连接级累加器
	OnClose               func(reason CloseReason)
}

// ============================================================================
// §6.1 计费接缝
// ============================================================================

// QuotaEstimate 是中继 → 计费的估算输入。
type QuotaEstimate struct {
	Model        string
	Group        string
	MaxTokens    *int  // 用户声明的输出上界
	PromptTokens int64 // 本地估算
	ImageCount   int   // 图像请求量
	TaskDuration int   // 任务时长（秒）——用户声明的值；使用前必须校验 ≤ MaxRequestTaskDurationSeconds
	UsePrice     bool  // 按次计价

	// —— v1.1 · 决议 B5 ——
	AudioSeconds      int64  // 音频时长（秒）（CCR-4）
	AttemptImageCount int    // 每次尝试的出站张数（CCR-7）；必须带上界 ≤ MaxImageN
	FixedPriceKey     string // 按动作固定价键（合并 FixedAction/FixedPriceKey，只保留后者）

	Raw *Request // 完整 IR
}

// UsageFact 是中继 → 计费的实际用量事实。
type UsageFact struct {
	Model            string
	Usage            Usage // 契约 §2.3
	ImageCount       int   // 图像可计费张数（可能被 CountImages 下调）
	LatencyMS        int
	FirstTokenMS     *int // v1.1 · 决议 B6：非流式为 null
	Stream           bool
	Attempts         int
	BuiltInToolCalls map[string]int // v1.1 · 决议 B6：内建工具调用次数

	// —— 上游对账维度（写日志时固化，usage_logs 迁移 20261028090500）——
	//
	// 中继在产出事实时从 RelayContext 一次性复制这三个值；此后计费/日志只读
	// 事实，绝不再回读（可能已被重试改写的）RelayContext。零值 = 未归属，
	// 日志落 NULL 而不是编造 0。
	ResponseID        string // 上游响应 id；流式路径不保留，留空
	UpstreamModel     string // 上游实际模型名（渠道映射后）
	UpstreamAccountID int64  // 归属的上游账号 id；0 = 未归属
}

// BillingSession 是计费 → 中继的会话（v1.1 · 决议 B1/B4 + 补遗 2/3）。
//
// 【补遗 2】Settle 已删除，Terminate 完全取代它（不留双方法）。
// 【补遗 3】Reserve 与 ChargeIncrement 职责严格分离，不得混用：
//
//	Reserve         = 仅上浮预留，不产生消耗
//	ChargeIncrement = 实际扣减并累计，产生消耗与流水（连接级的每个 response.done）
type BillingSession interface {
	PreConsume(est QuotaEstimate) error                // 首次预扣；不足返回 ErrInsufficientQuota
	Reserve(delta int64) error                         // 仅上浮预留、不产生消耗；幂等；已达标则 no-op
	ChargeIncrement(delta int64, fact UsageFact) error // 实际扣减并累计（连接级）
	Terminate(fact UsageFact) error                    // 终结会话并结算最终差额；幂等（取代原 Settle）
	Refund() error                                     // 全额退款，幂等
	Reserved() int64                                   // 当前预留量
	NeedsRefund() bool                                 // 【M3 新增】调用方据此决定是否走退款路径（对齐基线的 BillingSettler）
	RecoverAbandoned(requestID string) error           // 【M3 新增 · 决议 A3】崩溃兜底：按 requestID 终结悬挂会话并全额退款；幂等。
	//                                                    由 worker 的兜底扫描调用（见 billing/06），归属本接口的实现方。
}

// SessionBilling 是连接级会话（v1.1 · 决议 B3 + 补遗 8）。
// 不新增终结方法——连接结束时直接调用继承来的 Terminate(finalFact)。
type SessionBilling interface {
	BillingSession
}

// BillingHandoff 是「请求结束、任务继续」的异步交接面（M6d Task 6a）。
//
// 任务提交与普通中继的差别：提交请求返回后，任务仍在上游运行，预扣 hold
// 必须保留到任务终态。若沿用普通会话，请求函数返回后心跳 goroutine 失去
// 引用却继续运行，worker 也永远看不到「悬挂」；而重启后 worker 会把一个
// 仍在成功的任务当成悬挂请求全额退款（免费服务）。因此需要显式交接：
//
//	Detach                 停止心跳、移除在飞租约，**保留 hold 不结算**；
//	                       幂等；由提交路径在任务行落库成功后调用。
//	TerminateByRequestID   T6b 轮询器在任务终态时按持久化 requestID 结算
//	                       （**任务路径必须使用引擎的 ForToken 变体**，否则
//	                       令牌额度不调整/不归属）；
//	RefundByRequestID      T6b 在失败/超时时按持久化 requestID 全额退款
//	                       （同上：任务路径使用 ForToken 变体，否则令牌
//	                       额度不回补）。
//
// 两个 ByRequestID 只依赖持久化账本行，不依赖进程内 Session；幂等语义与
// BillingSession.Terminate/Refund 相同。
type BillingHandoff interface {
	Detach(requestID string) error
	TerminateByRequestID(requestID string, fact UsageFact) error
	RefundByRequestID(requestID string) error
}

// ============================================================================
// §7.2 流式状态（v1.1 · 决议 E6，统一 01 CR#5 与 03 CR-C-04）
// ============================================================================

type StreamStatus struct {
	Phase     StreamPhase // 建连 / 传输中 / 已终结
	EndReason EndReason   // 终结原因（见下）
	Outcome   ResponseOutcome
	RecvCount int
	SendCount int
}

type EndReason int

const (
	EndUpstreamDone   EndReason = iota // 上游正常结束（收到 [DONE] 或协议终止事件）
	EndUpstreamEOF                     // 上游连接被关闭（无终止标记）
	EndUpstreamError                   // 上游返回错误
	EndClientAbort                     // 客户端断开
	EndStreamTimeout                   // 块间不活动超时（看门狗）
	EndWriteTimeout                    // 慢客户端保护触发
	EndServerShutdown                  // 副本优雅下线
	EndPanic                           // 处理过程 panic
	EndUnknown
)

type ResponseOutcome int

const (
	OutcomeSuccess ResponseOutcome = iota
	OutcomePartial                 // 有部分内容但未正常终结
	OutcomeFailed
)

type StreamPhase int

const (
	PhaseConnecting StreamPhase = iota
	PhaseStreaming
	PhaseTerminated
)
