package contract

import "time"

// ============================================================================
// 计费契约（M3 · billing/00-计费引擎接口契约.md）
//
// 本文件是**计费侧**的类型定义。它与 ir.go / context.go / channels.go / types.go
// 同属一个包 —— 因为中继与计费共用同一个契约面（IR、用量事实、定价快照、
// 计费会话）。分成两个包会造成 import 循环，故合并于一处。
//
// 分工：
//   - 中继产出：QuotaEstimate（估算输入）、UsageFact（用量事实）
//   - 计费引擎产出：PriceSnapshot（定价快照）、TokenParams（归一化变量）、配额
// ============================================================================

// ---------------------------------------------------------------------------
// 定价模式
// ---------------------------------------------------------------------------

// BillingMode 是模型生效的计价模式。
//
// 解析优先级（契约 §3.1，基线易漏）：
//
//  1. 模型配置了传统价（ModelRatio / ModelPrice 存在）→ BillingModeRatio
//  2. 否则模型在内置表达式表中                    → BillingModeTieredExpr
//  3. 否则                                       → BillingModePerCall
//
// 「已配置旧价优先于新内置表达式」—— 内置价不得静默覆盖管理员的旧价。
type BillingMode int

const (
	BillingModeRatio      BillingMode = iota // 传统倍率
	BillingModeTieredExpr                    // 表达式
	BillingModePerCall                       // 按次 / 固定价
)

// ---------------------------------------------------------------------------
// 资金来源
// ---------------------------------------------------------------------------

// FundingKind 是费用来源。
type FundingKind int

const (
	FundingWallet       FundingKind = iota // 钱包余额
	FundingSubscription                    // 订阅套餐
)

// BillingPreference 是用户的扣费来源偏好。
type BillingPreference int

const (
	PrefSubscriptionFirst BillingPreference = iota // 默认：有活跃订阅用订阅，不足且允许溢出则钱包
	PrefSubscriptionOnly                           // 只用订阅
	PrefWalletOnly                                 // 只用钱包
	PrefWalletFirst                                // 先钱包，不足回退订阅
)

// ---------------------------------------------------------------------------
// 配额数学
// ---------------------------------------------------------------------------

// ClampKind 是钳制/回退的原因。
type ClampKind int

const (
	ClampOverflow  ClampKind = iota // 超上界
	ClampUnderflow                  // 超下界
	ClampNaN                        // NaN / Inf
)

// QuotaClamp 是一次钳制事件的审计标记。
//
// 【硬要求】任何饱和/回退都必须产生此标记，并最终落到
// `usage_logs.other.admin_info.quota_saturation`（管理员可见，非管理员视图剥离）。
// 契约 §12 不变量 9。
type QuotaClamp struct {
	Op       string    // 发生钳制的操作名（辅助函数名）
	Kind     ClampKind // 原因
	Original float64   // 原值（NaN 时不可用于比较，故用 float64 而非 int64）
	Clamped  int64     // 钳制后值
}

// 配额边界（契约 §7.2）。
const (
	MaxQuota       = int64(1)<<31 - 1 // 单请求配额上界（int32 饱和）
	MinQuota       = -int64(1) << 31  // 单请求配额下界
	MaxWalletQuota = int64(1)<<53 - 1 // 钱包/充值边界（JS 安全整数域）
)

// 任务时长边界（M3 决议 A1：**语义不同，拆为两个常量**）。
//
// 二者此前共用一个名字（`MaxTaskDurationSeconds`），导致「请求拒绝阈值」与
// 「冒烟测试上界」在四处出现两个值。拆开后各司其职。
const (
	// MaxRequestTaskDurationSeconds 是**用户在请求里可声明的任务时长上界**；超出即 400 拒绝。
	MaxRequestTaskDurationSeconds = 3600

	// MaxTaskDurationSmokeBound 是**表达式冒烟测试用的宿主上界**（`second` 单位字段的测试覆盖上限）；
	// 仅测试用，不影响请求校验。
	MaxTaskDurationSmokeBound = 86400
)

// InflightLease 是「请求仍在飞」的心跳租约（M3 决议 A3）。
//
// 计费会话在预扣成功后写入、请求期间更新心跳；worker 的崩溃兜底扫描
// **只回收心跳超 TTL 的会话** —— 否则会把仍合法的长请求误当崩溃残留退掉。
type InflightLease struct {
	RequestID   string
	HeartbeatAt time.Time // 最后一次心跳；超过 TTL 视为副本已死
}

// 在飞租约与崩溃兜底的参数（M3 决议 A3b）。
//
// 【关键：这是**两组**参数，不得混用】
//   - 租约参数（TTL / 心跳间隔）决定「会话是否还活着」
//   - 扫描参数（周期 / 判定窗口）决定「worker 多久扫一次、多老的会话才纳入扫描」
//
// 判定窗口必须**显著大于** TTL，否则会把仍在发心跳的活跃会话误判为悬挂。
const (
	// InflightLeaseTTLSeconds 是心跳超时阈值：超过此时长无心跳即视为所属副本已死、可被回收。
	// 必须显著大于正常请求时长，否则会把仍合法的长请求误退。
	InflightLeaseTTLSeconds = 300

	// InflightHeartbeatSeconds 是会话更新心跳的间隔（远小于 TTL）。
	InflightHeartbeatSeconds = 30

	// AbandonedScanIntervalSeconds 是 worker 扫描悬挂会话的周期（属扫描参数，与 TTL 无关）。
	AbandonedScanIntervalSeconds = 300

	// AbandonedWindowSeconds 是「悬挂」判定窗口的下限：会话开始后至少这么久才纳入扫描。
	// 【必须 > InflightLeaseTTLSeconds】否则活跃会话会被误回收。
	AbandonedWindowSeconds = 600
)

// ---------------------------------------------------------------------------
// Token 归一化
// ---------------------------------------------------------------------------

// UsedVars 是表达式引用到的标识符集合（AST 内省的产物）。
//
// 【注意】它是**所有标识符**，含函数名 —— 不是纯粹的变量名集合。
// 归一化只关心其中的 token 变量键（p/c/len/cr/cc/cc1h/img/img_cr/ai/img_o/ao/image_count）。
type UsedVars map[string]bool

// TokenParams 是归一化后的计费变量（表达式的求值输入之一）。
//
// 【核心规则】`p` / `c` 表示「**未被表达式单独定价的 token**」，且归一化是**双向的**：
//   - GPT/OpenAI 格式（prompt 含全部子类）→ **减**：引用了某子类才扣除
//   - Claude/Anthropic 格式（input 仅文本）→ **加**：未引用 `cr` 时把缓存读取**加回** `p`
//
// 只做减法会系统性少计费（契约 §5.2）。
type TokenParams struct {
	// 兜底变量（归一化后）
	P   int64 // 输入 token（未单独定价的部分）
	C   int64 // 输出 token（未单独定价的部分）
	Len int64 // 上下文长度：非 Claude = prompt_tokens；Claude = input + cr + cc5m + cc1h

	// 子类（独立计费维度）
	CR    int64 // 缓存读取
	CC5m  int64 // 缓存创建 5m / 通用
	CC1h  int64 // 缓存创建 1h
	Img   int64 // 图片输入
	ImgCR int64 // 图片缓存读取（三段式拆分，见契约 §5.4）
	AI    int64 // 音频输入
	ImgO  int64 // 图片输出
	AO    int64 // 音频输出

	// 独立计费量
	ImageCount int // 图像张数（默认 1，区间 [1, MaxImageN]）
}

// ---------------------------------------------------------------------------
// 定价快照
// ---------------------------------------------------------------------------

// PriceData 是定价的完整快照（契约 §3.3）。
//
// 【显式零 ≠ 未设置】贯穿全链：所有倍率类字段以 `Set` 后缀的布尔标记
// 「是否被管理员显式配置过」，未设置时取兜底值、显式 0 时必须保留为 0。
type PriceData struct {
	FreeModel bool
	UsePrice  bool // 按次计价

	ModelPrice           float64 // 按次单次价
	ModelRatio           float64 // 模型倍率
	CompletionRatio      float64 // 补全倍率
	CacheRatio           float64 // 缓存读倍率（未配置 → 1.0）
	CacheCreationRatio   float64 // 【M3 补】缓存创建基值（5m/1h 分档的基）
	CacheCreation5mRatio float64 // 缓存创建 5m/通用（未配置 → 1.25）
	CacheCreation1hRatio float64 // 缓存创建 1h（未配置 → CreateCacheRatio × 6/3.75，**该系数只对名字含 claude 的模型继承**）
	ImageRatio           float64 // 图片倍率（为 1 时图片输入含在 p 里，不生成独立 img 项）
	AudioRatio           float64
	AudioCompletionRatio float64

	// 显式零标记（契约 §3.2）：每个倍率各自配一个，避免「未设置」与「显式 0」混淆
	CacheRatioSet    bool
	CacheCreationSet bool
	// CacheCreationRatioSet 标记缓存创建**基准**是否被显式配置 ——
	// 5m/1h 两档由基准派生，故「基准的显式零」需单独判定（M3 决议 C7a）。
	CacheCreationRatioSet bool
	ImageRatioSet         bool
	AudioRatioSet         bool

	GroupRatioInfo GroupRatioInfo

	// otherRatios 私有（契约 §12 不变量 10）：
	// 只能经守卫写入，拒绝非正、NaN、+Inf。此处不导出字段。
	//
	//nolint:unused // 冻结契约骨架的占位字段，M3 定价守卫启用；在此之前无引用。
	otherRatios map[string]float64
}

// GroupRatioInfo 是分组倍率信息。
type GroupRatioInfo struct {
	Group             string // 【M3 补】分组名（日志与诊断需要）
	GroupRatio        float64
	GroupSpecialRatio float64 // 分组特殊倍率（若配置）
	HasSpecialRatio   bool
}

// PriceSnapshot 是计费引擎产出、中继透传的定价快照。
//
// 【对中继不透明】中继只持有与透传，**不读取其内部字段** —— 这正是
// 「中继不参与定价」在类型上的体现（`relay/00` §6.3）。本处给出真实字段，
// 但不构成中继读取它们的许可。
type PriceSnapshot struct {
	Mode                     BillingMode
	Price                    PriceData
	ExprString               string         // 冻结的表达式（表达式模式下）
	ExprHash                 string         // 表达式哈希（用于取 usedVars 缓存，避免重算）
	GroupRatio               float64        // 选中分组的倍率（跨组重试时刷新）
	EstimatedQuotaAfterGroup int64          // 选中分组后的估算配额（抬预留用）
	EstimatedImageCount      int            // 【M3 补】发送前的估算张数（两段预扣的第二段用）
	PreConsumeMultiplier     float64        // 预扣倍数（快照冻结供图片重试复用）
	CacheWriteMode           CacheWriteMode // 只读元信息，不参与求值
	IsClaudeUsageSemantic    bool           // 上游格式事实（归一化的输入之一）
}

// CacheWriteMode 是缓存写入模式的只读元信息（供展示，不参与表达式执行）。
type CacheWriteMode int

const (
	CacheWriteNone      CacheWriteMode = iota // 无缓存写入
	CacheWriteStandard                        // 通用缓存
	CacheWriteClaudeTTL                       // Claude 分 TTL
)

// ---------------------------------------------------------------------------
// 折扣
// ---------------------------------------------------------------------------

// Discount 是一条生效的折扣。
type Discount struct {
	Rate   float64 // 折扣率（1 = 无折扣）；**不叠加，取最低 rate**
	Target string  // 折扣目标标识；**非空才表示折扣生效** —— rate=0（全免）也靠它判定
}

// ---------------------------------------------------------------------------
// 任务用量
// ---------------------------------------------------------------------------

// PluginBillingExpr 是按 `<pluginKey>::<clientModel>` 索引的插件级定价覆盖。
//
// 解析顺序见契约 §3.1：插件级覆盖优先于模型级表达式，
// 且**总是使用任务用量语义**，与模型的计费模式无关。
type PluginBillingExpr map[string]string
