package contract

import (
	"io"
	"time"
)

// ============================================================================
// §5.4 RelayContext 与附属类型（v1.1 · 补遗 9）
//
// 背景：契约 §5 用整节给出了 RelayContext 的**字段表**（8 组字段、每个字段标注唯一写入方），
// 但**没有给出 Go 结构体**。它是全部适配器方法的第一个参数（12 处引用），
// 由 `go build` 检查出。本节把字段表落成类型。
// ============================================================================

// RelayContext 是一次请求的完整状态，是各子设计的公共依赖。
//
// 分组与写入方与契约 §5 的字段表一一对应；**每个字段只有一个写入方**（注释标注）。
type RelayContext struct {
	// —— 身份组（写入方：鉴权层；UsingGroup 初值由鉴权层写、路由层可在重试时轮换）——
	RequestID  string // 计费幂等键；与「客户端可重放的提交幂等键」是两个概念（契约 §5.4）
	UserID     int
	TokenID    int
	UsingGroup string
	UserGroup  string

	// —— 请求组（写入方：归一化层）——
	OriginModel string
	Streaming   bool
	Request     *Request
	RawFormat   RelayFormat
	RequestPath string

	// —— 路由组（写入方：路由层；执行层只读）——
	AttemptPlan  *AttemptPlan
	AttemptIndex int
	LastError    *RelayError

	// —— 渠道组（写入方：执行层，每轮写入）——
	ChannelID     int
	ChannelType   ChannelType
	APIBase       string
	Credential    string
	MultiKeyIndex int
	UpstreamModel string
	GroupRatio    float64
	CreatedTime   time.Time // 渠道路由元数据的建渠道时点（v1.1 · 决议 F2，供 Azure 点号规则用）

	// —— 计费组（写入方：PriceSnapshot/QuotaEstimate/UsageFact/Discount 由计费接缝写；
	//             ClampEvent 由产生钳制的一方写、计费接缝汇总，见契约 §5.1）——
	PriceSnapshot *PriceSnapshot
	QuotaEstimate *QuotaEstimate
	UsageFact     *UsageFact
	ClampEvent    *ClampEvent
	Discount      float64

	// —— 流式组（写入方：执行层）——
	StreamStatus *StreamStatus
	SendCount    int
	RecvCount    int
	FirstTokenMS *int // 非流式为 nil，不得填占位值（甲类修正）

	// —— 任务组（写入方：任务路径）——
	TaskHandle      *TaskHandle
	OriginTaskIDs   []string
	LockedChannelID int

	// —— 审计组（追加式）——
	ConversionChain []ConversionStepEntry
	OverrideAudit   []OverrideAuditEntry
	Diagnostics     []DiagnosticEntry
}

// ClampEvent 是钳制/饱和事件的审计标记（契约 §5；rules/billing.md 要求「钳制事件可审计」）。
// 由**产生钳制的一方**（归一化层或适配器本地）写入，在计费接缝处汇总，日志层落库到
// `usage_logs.other.admin_info.quota_saturation`（管理员可见，非管理员日志视图剥离）。
type ClampEvent struct {
	Field     string // 发生钳制的字段名
	Reason    string // saturation | nan | negative | overflow
	Requested int64  // 原值（溢出时截断保存）
	Clamped   int64  // 钳制后值
}

// PriceSnapshot 是计费引擎产出的定价快照。
//
// **对中继是不透明的**：中继只持有与透传，不读取其内部字段 —— 这正是契约 §6.3
// 「中继不参与定价」的直接体现。字段定义归 M3 计费引擎，本处**刻意不展开**
// （这不是缺口，是有意的边界）。
type PriceSnapshot struct{}

// ConversionStepEntry 是请求转换链的一步（追加式审计）。
type ConversionStepEntry struct {
	Stage string // 阶段名（如 "model_mapped" / "param_override" / "protocol_convert"）
	From  RelayFormat
	To    RelayFormat
	At    time.Time
}

// OverrideAuditEntry 是参数/请求头覆盖的审计记录（追加式）。
type OverrideAuditEntry struct {
	Stage  string // param_override | header_override
	Op     string // set / delete / move / …
	Path   string // 被作用的字段路径
	Before any
	After  any
}

// DiagnosticEntry 是转换诊断（追加式）。
type DiagnosticEntry struct {
	Stage   string
	Message string
	Detail  map[string]any
}

// ChannelSnapshot 是建连时锁定的渠道快照（供连接级会话复用，见契约 §5.3）。
type ChannelSnapshot struct {
	ChannelID     int
	ChannelType   ChannelType
	APIBase       string
	Credential    string
	UpstreamModel string
	GroupRatio    float64
}

// ConnectionAccumulator 是连接级累加器（跨多次请求/帧累计用量）。
type ConnectionAccumulator struct {
	TotalUsage   Usage
	RequestCount int
}

// CloseReason 是连接关闭原因。
type CloseReason int

const (
	CloseNormal         CloseReason = iota // 正常关闭
	CloseClientGone                        // 客户端断开
	CloseUpstreamGone                      // 上游断开
	CloseServerShutdown                    // 副本优雅下线（无状态集群的已知限制，见契约 §8）
	CloseError                             // 出错关闭
)

// ============================================================================
// §3.4 附属类型（v1.1 · 补遗 9）
// ============================================================================

// Set 是契约中使用的泛型集合（如 Capabilities.Endpoints）。
type Set[T comparable] map[T]struct{}

// PinRetryMode 是固定渠道（pin）时的重试策略。
type PinRetryMode int

const (
	PinRetryDefault       PinRetryMode = iota // 遵循全局重试策略
	PinRetrySingleAttempt                     // 固定渠道单次尝试，不重试（基线行为）
)

// SessionMode 是会话亲和（channel affinity）的绑定强度。
type SessionMode int

const (
	SessionModeNone   SessionMode = iota // 不启用亲和
	SessionModeStrict                    // 严格绑定：亲和渠道不可用时直接 503，不降级到加权随机
)

// ChannelFilter 是候选渠道的请求级过滤器种类（契约 §5.2 的 ChannelConstraints.Filters）。
type ChannelFilter int

const (
	FilterRequestPath        ChannelFilter = iota // 路径支持性（AdvancedCustom 专属）
	FilterTaskPluginIdentity                      // 任务插件身份匹配
	FilterResponsesWebSocket                      // 是否支持 Responses WebSocket
)

// ChannelType 见 channels.go（该枚举的唯一出处；60 个具名类型 + 显示名映射）。

// AdapterKey 是适配器注册键（等价基线的「API 类型」，见契约 §3.3）。
type AdapterKey string

// MultiKeyMode 是多密钥选择模式。
type MultiKeyMode int

const (
	MultiKeyNone    MultiKeyMode = iota // 非多密钥渠道
	MultiKeyRandom                      // 随机
	MultiKeyPolling                     // 轮询（游标持久化）
)

// RateLimitSpec 是渠道级限流声明。
type RateLimitSpec struct {
	RPM int // 单渠道每分钟请求上限；0 表示不限
}

// OtherSemantic 声明多义 `other` 字段在该渠道类型下的语义
// （基线中同一字段在不同类型下分别是 api_version / region / bot_id / plugin / AccountID —— 乙类基线怪癖，照搬）。
type OtherSemantic string

const (
	OtherNone      OtherSemantic = ""
	OtherAPIVer    OtherSemantic = "api_version"
	OtherRegion    OtherSemantic = "region"
	OtherBotID     OtherSemantic = "bot_id"
	OtherPlugin    OtherSemantic = "plugin"
	OtherAccountID OtherSemantic = "account_id"
)

// BaseURLMode 是基址解析模式。
type BaseURLMode int

const (
	BaseURLDefault  BaseURLMode = iota // 有默认基址，渠道可覆盖
	BaseURLRequired                    // 必须由渠道配置
	BaseURLSpecial                     // 走特殊基址解析（对应基线 ChannelSpecialBases —— 乙类怪癖，照搬）
)

// TaskSpec 是任务平台声明（仅 TaskAdapter 渠道有值）。
type TaskSpec struct {
	Platforms []string       // 该渠道可绑定的任务插件键
	Extra     map[string]any // 平台特有声明
}

// ValueKind 是字段规格的取值类型。
type ValueKind string

const (
	ValueString ValueKind = "string"
	ValueJSON   ValueKind = "json"
	ValueInt    ValueKind = "int"
	ValueEnum   ValueKind = "enum"
)

// FieldSpec 是渠道表单字段的声明式规格（凭据字段与附加字段共用）。
// 定义在此以保证契约自洽；**取值的唯一真源是 02**。
type FieldSpec struct {
	Name     string    // 字段逻辑名（如 "api_version" / "region" / "deployment"）
	Label    string    // i18n 键
	Required bool      // 前端必填 = 后端必填（甲类修正：两端必须一致）
	Masked   bool      // 是否敏感（脱敏显示）
	Kind     ValueKind // string | json | int | enum
	Example  string
	Pattern  *string // 可选校验正则
}

// UpstreamConn 是到上游的连接抽象（WebSocket 形态）。
// 只读上游事件、可写客户端帧，最终由执行层关闭。
type UpstreamConn interface {
	io.ReadWriteCloser
}

// ============================================================================
// §3.5 端点响应类型（v1.1 · 补遗 9）
//
// 说明：以下为**契约层的最小形状声明** —— 保证接口签名可编译。
// 完整字段集归 `04-多模态端点族-设计.md`（该文档为唯一真源）。
// ============================================================================

// EmbeddingResponse 是嵌入端点的归一化响应。
type EmbeddingResponse struct {
	Model string
	Data  []EmbeddingDatum
	Usage Usage
}

type EmbeddingDatum struct {
	Index  int
	Vector []float32
}

// RerankResponse 是重排端点的归一化响应。
type RerankResponse struct {
	Model   string
	Results []RerankResult
	Usage   Usage
}

type RerankResult struct {
	Index int
	Score float64
}

// AudioResponse 是音频端点的归一化响应。
// 语音合成走 Binary（非 JSON）；转写与翻译走 Text。
type AudioResponse struct {
	Binary      []byte // 合成音频
	ContentType string // 如 "audio/mpeg"
	Text        string // 转写/翻译文本
	Usage       Usage
}
