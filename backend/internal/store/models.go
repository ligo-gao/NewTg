package store

import "time"

// M0 占位模型：16 个空壳 struct + TableName()。字段随里程碑逐步填充：
// M1 只填 User / APIToken / UserGroup（含 M1 迁移新增列）；M2 填 Channel
// （渠道存储与中继路由所需列）。
// 列定义唯一权威：platform-01 各 §；M1 补列裁决见实现计划 R-01/R-02/R-07。
// 表序同 platform-01 §0.1（主库 16 表；usage_logs/audit_logs 属日志库 Task 8）。
//
// 这些 struct 是**读投影**（数据库是唯一权威），刻意不带任何 GORM `default:`
// 标签：GORM 的默认值语义会把 Go 零值（0 / false / ""）替换成标签值，导致
// 「显式写 0 / false / 空串」被静默改写——对有意义的零值（remain_quota=0 表示
// 额度耗尽、record_ip_log=false 表示隐私开关关闭）是数据损坏。任何 Create/
// Update 调用都必须显式赋值；零值会按零值落库，数据库列默认值只在 SQL 省略
// 该列时生效（本包不提供绕过路径）。

// User 对应 users（platform-01 §1：23 列 + M1 must_change_password = 24 列）。
type User struct {
	ID              int64      `gorm:"column:id;primaryKey"`
	Username        string     `gorm:"column:username"`
	DisplayName     *string    `gorm:"column:display_name"`
	Email           *string    `gorm:"column:email"`
	EmailVerifiedAt *time.Time `gorm:"column:email_verified_at"`
	// PasswordHash 可为 NULL：OAuth 即时注册用户没有口令，只能走 OAuth
	// 或之后自行设密码（platform-01 §1 / R-01-11），故用指针区分「无口令」。
	PasswordHash            *string   `gorm:"column:password_hash"`
	Role                    string    `gorm:"column:role"`
	Status                  string    `gorm:"column:status"`
	Balance                 int64     `gorm:"column:balance"`
	Group                   string    `gorm:"column:group"` // SQL 标识符引用由 dialector 负责，tag 只写列名（platform-04 §3.7 V8）
	AdminPermissions        []byte    `gorm:"column:admin_permissions"`
	TOTPSecretEnc           []byte    `gorm:"column:totp_secret"` // DB 列名 totp_secret；内容是 AES-256-GCM 密文（§1）
	TOTPEnabled             bool      `gorm:"column:totp_enabled"`
	BackupCodes             []byte    `gorm:"column:backup_codes"` // jsonb 哈希数组，不存明文（§1）
	Language                string    `gorm:"column:language"`
	AvatarURL               *string   `gorm:"column:avatar_url"`
	Aff                     *string   `gorm:"column:aff"`
	Remark                  *string   `gorm:"column:remark"`
	NotificationPreferences []byte    `gorm:"column:notification_preferences"`
	SidebarModules          []byte    `gorm:"column:sidebar_modules"` // jsonb 稀疏覆盖（§1 / B-16）
	RecordIPLog             bool      `gorm:"column:record_ip_log"`
	MustChangePassword      bool      `gorm:"column:must_change_password"` // R-02：临时口令强制改密
	CreatedAt               time.Time `gorm:"column:created_at"`
	UpdatedAt               time.Time `gorm:"column:updated_at"`
}

// TableName 返回 GORM 表名。
func (User) TableName() string { return "users" }

// APIToken 对应 api_tokens（§2：13 列 + M1 R-01 两列 = 15 列；R2：id bigint）。
type APIToken struct {
	ID          int64      `gorm:"column:id;primaryKey"`
	UserID      int64      `gorm:"column:user_id"`
	Name        string     `gorm:"column:name"`
	KeyHash     string     `gorm:"column:key_hash"` // 只存 SHA-256，明文仅创建时返回一次（GC-9）
	Status      string     `gorm:"column:status"`
	ExpiresAt   *time.Time `gorm:"column:expires_at"`
	ModelLimits []byte     `gorm:"column:model_limits"`
	Group       string     `gorm:"column:group"` // SQL 标识符引用由 dialector 负责，tag 只写列名（platform-04 §3.7 V8）
	// RemainQuota 用指针表达 DB 的三值语义：NULL=不限额度，0=额度耗尽，
	// 正值=剩余额度。若用 int64 + default，显式 0 会被改写成默认值。
	RemainQuota *int64 `gorm:"column:remain_quota"`
	// UnlimitedQuota 是派生字段（DB 语义：remain_quota IS NULL=不限额度），
	// **不落库**（platform-01 §2「unlimited_quota 为 UI/派生字段」）。
	UnlimitedQuota  bool       `gorm:"-"`
	AllowIPs        []byte     `gorm:"column:allow_ips"`
	LastUsedAt      *time.Time `gorm:"column:last_used_at"`
	CreatedAt       time.Time  `gorm:"column:created_at"`
	UpdatedAt       time.Time  `gorm:"column:updated_at"`
	AutoGroups      []byte     `gorm:"column:auto_groups"`       // R-01：JSON 数组文本（modules-02 §3.10）
	CrossGroupRetry bool       `gorm:"column:cross_group_retry"` // R-01：跨组重试开关（modules-02 §3.11）
}

// TableName 返回 GORM 表名。
func (APIToken) TableName() string { return "api_tokens" }

// UserGroup 对应 user_groups（§3，9 列；R4：代理 id PK + name 唯一）。
type UserGroup struct {
	ID             int64     `gorm:"column:id;primaryKey"`
	Name           string    `gorm:"column:name"`
	Description    *string   `gorm:"column:description"`
	Ratio          int32     `gorm:"column:ratio"` // PG integer（整数百分比，120=1.20x）
	UserSelectable bool      `gorm:"column:user_selectable"`
	IsSystem       bool      `gorm:"column:is_system"`
	Status         int16     `gorm:"column:status"` // PG smallint（1=启用 / 2=停用）
	CreatedAt      time.Time `gorm:"column:created_at"`
	UpdatedAt      time.Time `gorm:"column:updated_at"`
}

// SystemSetting 对应 system_settings（§4，5 列；自然主键 "key"）。
type SystemSetting struct{}

// TableName 返回 GORM 表名。
func (SystemSetting) TableName() string { return "system_settings" }

// Channel 对应 channels（§6，22 列 + M6d Task 7a 任务插件绑定 2 列 = 24 列；
// 含 G-API-043 tag）。
//
// 字段口径（platform-01 §6、modules-06 §1.4/§1.5、GC-6/GC-8）：
//   - Type 是 text slug，不做 DB CHECK（Tier-2）；白名单唯一真源是
//     contract.ChannelTypes / ChannelType.Valid()（modules-06 §1.3）。
//   - Credentials 落库即 AES-256-GCM 密文（GC-8），明文只在创建/更新请求里出现一次；
//     任何读取路径都不得把它直接序列化给客户端（GC-6）。
//   - GroupTags 为 NULL 表示「全部分组可用」，与空数组（DB CHECK 拒绝）语义不同，
//     故用 []byte nil 承载 NULL。
//   - Status 三态：1=enabled / 2=manual_disabled / 3=auto_disabled（modules-06 §1.4）；
//     不存在 deleted 状态。
//   - UpdatedAt 同时是乐观锁版本（R-P8：读取时派生，不落 version 列）。
type Channel struct {
	ID                 int64      `gorm:"column:id;primaryKey"`
	Name               string     `gorm:"column:name"`
	Type               string     `gorm:"column:type"`
	BaseURL            *string    `gorm:"column:base_url"`
	Credentials        []byte     `gorm:"column:credentials"`
	SupportedModels    []byte     `gorm:"column:supported_models"`
	GroupTags          []byte     `gorm:"column:group_tags"`
	ModelMapping       []byte     `gorm:"column:model_mapping"`
	Weight             int16      `gorm:"column:weight"`
	Priority           int16      `gorm:"column:priority"`
	RateLimit          *int32     `gorm:"column:rate_limit"`
	Status             int16      `gorm:"column:status"`
	AutoBan            bool       `gorm:"column:auto_ban"`
	TestModel          *string    `gorm:"column:test_model"`
	Remark             *string    `gorm:"column:remark"`
	Tag                *string    `gorm:"column:tag"`
	FailCount          int32      `gorm:"column:fail_count"`
	AutoDisabledReason *string    `gorm:"column:auto_disabled_reason"`
	LastTestAt         *time.Time `gorm:"column:last_test_at"`
	LastTestResult     *bool      `gorm:"column:last_test_result"`
	// 任务插件绑定（M6d Task 7a / relay-06 §1.3）。两种形态互斥：
	//   - 类型 61（task_plugin）：TaskPluginKey 单绑，Extend 必须为空数组；
	//   - 类型 60（newapi）：TaskPluginKey 必须为 NULL，Extend 非空。
	// 其他类型两者都必须为空/空数组。指针表示 NULL（未绑定），空数组是
	//「没有多绑」，二者语义不同但都合法，故 Extend 不用指针。
	TaskPluginKey        *string   `gorm:"column:task_plugin_key"`
	TaskExtendPluginKeys []byte    `gorm:"column:task_extend_plugin_keys"`
	CreatedAt            time.Time `gorm:"column:created_at"`
	UpdatedAt            time.Time `gorm:"column:updated_at"`
}

// TableName 返回 GORM 表名。
func (Channel) TableName() string { return "channels" }

// ModelPricing 对应 model_pricing（§7，13 列；R9 判别列 + R10 代理 id PK）。
type ModelPricing struct{}

// TableName 返回 GORM 表名。
func (ModelPricing) TableName() string { return "model_pricing" }

// GroupPricing 对应 group_pricing（§8，10 列；UNIQUE("group", model)）。
type GroupPricing struct{}

// TableName 返回 GORM 表名。
func (GroupPricing) TableName() string { return "group_pricing" }

// RedemptionCode 对应 redemption_codes（§9，11 列；R11 不物化 expired_at）。
//
// 口径（裁决 P-4 / R11）：
//   - 只存 code_hash（SHA-256 十六进制），明文码仅在生成响应中出现一次；
//     因此这里**没有** code 列，任何按明文查询的 API 都必须先归一化哈希。
//   - 过期是虚拟态：不落 expired_at 列，判定 = status='unused' AND
//     expires_at <= now()；ExpiresAt=NULL 表示永不过期。
//   - UsedBy/UsedAt 未使用时为 NULL（三值语义用指针表达）；核销靠条件
//     UPDATE（status='unused'）保证同一码只入账一次。
type RedemptionCode struct {
	ID        int64      `gorm:"column:id;primaryKey"`
	CodeHash  string     `gorm:"column:code_hash"`
	Credits   int64      `gorm:"column:credits"` // 面额（积分），DB CHECK >0
	Status    string     `gorm:"column:status"`  // unused/disabled/used（expired 为虚拟态）
	BatchName *string    `gorm:"column:batch_name"`
	CreatedBy int64      `gorm:"column:created_by"` // 逻辑引用 admin user id（不设 FK）
	UsedBy    *int64     `gorm:"column:used_by"`    // 逻辑引用 users.id（不设 FK）
	UsedAt    *time.Time `gorm:"column:used_at"`
	ExpiresAt *time.Time `gorm:"column:expires_at"` // NULL=永不过期
	CreatedAt time.Time  `gorm:"column:created_at"`
	UpdatedAt time.Time  `gorm:"column:updated_at"`
}

// TableName 返回 GORM 表名。
func (RedemptionCode) TableName() string { return "redemption_codes" }

// Order 对应 orders（§10，23 列；含 G-1R P1/P2 新增 11 列）。
//
// 口径（P-1 / P-6）：
//   - Amount 是最小货币单位整数（分），不是积分；积分为入账快照
//     CreditedCredits（入账时写入，供对账，非派生值——汇率与
//     billing.quota_per_unit 都可能变化）。
//   - Status 五态：pending → succeeded/failed/expired/cancelled；后四者
//     终态。paid 是历史别名，落到 succeeded；refunded 预留不启用（MVP 不
//     做订单级退款）。
//   - IdempotencyKey / ProviderTxnID 各为 UNIQUE（双唯一是硬要求，
//     ARCH §2.2-2）；ProviderTxnID 为 NULL 时不参与唯一性。
//   - 过期时间不落列：派生为 created_at + 30min（F-04-07）。
//   - ManualReason/ManualOperatorID 在 manual_completed=true 时由 DB CHECK
//     强制非空。
type Order struct {
	ID       int64  `gorm:"column:id;primaryKey"`
	UserID   int64  `gorm:"column:user_id"`  // 逻辑引用 users.id（不设 FK）
	Provider string `gorm:"column:provider"` // stripe/alipay/creem/waffo/waffo_pancake/epay，创建后不可改
	Amount   int64  `gorm:"column:amount"`   // 最小货币单位（分），DB CHECK >=0
	Currency string `gorm:"column:currency"` // ISO-4217 三字母
	Status   string `gorm:"column:status"`   // 五态，见上
	// BizKind / BizRefID 区分充值订单与订阅购买订单（billing-08 §6.2）。
	// 两列在 D5a 迁移中已落库；S3 之前刻意不投影，现在写路径显式赋值。
	// 读取一律按列名（GORM 自动 SELECT *），写入走 billing 的显式 INSERT。
	// `default:topup` 让 GORM Create 在零值时交给 DB 默认值：否则零值 ""
	// 会被显式写入并撞 orders_biz_kind_chk（既有测试用 GORM Create 建单）。
	BizKind          string     `gorm:"column:biz_kind;default:topup"` // topup（默认）/ subscribe
	BizRefID         *int64     `gorm:"column:biz_ref_id"`             // subscribe 时 = subscription_plans.id
	IdempotencyKey   string     `gorm:"column:idempotency_key"`        // 创建订单防重
	ProviderTxnID    *string    `gorm:"column:provider_txn_id"`        // 上游流水号；人工旁路为 manual:{order_id}
	PaidAt           *time.Time `gorm:"column:paid_at"`
	RawCallback      []byte     `gorm:"column:raw_callback"`      // jsonb，仅管理员可见
	ProviderCurrency *string    `gorm:"column:provider_currency"` // 渠道结算币种（与站点计价分离）
	ProviderAmount   *int64     `gorm:"column:provider_amount"`
	CurrencyScale    *int16     `gorm:"column:currency_scale"` // 渠道币种小数位（CNY/USD=2，JPY=0）
	FxRateNum        *int32     `gorm:"column:fx_rate_num"`    // 渠道币种 → CNY 的汇率分子
	FxRateDen        *int32     `gorm:"column:fx_rate_den"`    // 汇率分母（与 num 成对）
	CreditedCredits  int64      `gorm:"column:credited_credits"`
	FailureCode      *string    `gorm:"column:failure_code"`     // 统一失败码（不落渠道原始错误码）
	ChannelMetadata  []byte     `gorm:"column:channel_metadata"` // jsonb，不含密钥
	ManualCompleted  bool       `gorm:"column:manual_completed"`
	ManualReason     *string    `gorm:"column:manual_reason"`
	ManualOperatorID *int64     `gorm:"column:manual_operator_id"`
	CreatedAt        time.Time  `gorm:"column:created_at"`
	UpdatedAt        time.Time  `gorm:"column:updated_at"`
}

// TableName 返回 GORM 表名。
func (Order) TableName() string { return "orders" }

// WalletTransaction 对应 wallet_transactions（platform-01 §11，11 列；有符号 bigint）。
//
// 可空列用指针表达 DB 的三值语义（reference_type/reference_id/note），
// 其余列 NOT NULL。金额列（amount/balance_after）写入后不可修改；
// 这里同样不带任何 GORM `default:` 标签：显式 0 必须按 0 落库，
// 数据库默认值只在 SQL 省略该列时生效。
type WalletTransaction struct {
	ID            int64     `gorm:"column:id;primaryKey"`
	UserID        int64     `gorm:"column:user_id"`
	Type          string    `gorm:"column:type"`          // 10 值枚举，见 platform-01 §11
	Amount        int64     `gorm:"column:amount"`        // 有符号：收入正、支出负；不可为 0
	BalanceAfter  int64     `gorm:"column:balance_after"` // 变动后余额（≥0）
	Status        string    `gorm:"column:status"`        // hold/settled/released/expired/confirmed
	ReferenceType *string   `gorm:"column:reference_type"`
	ReferenceID   *string   `gorm:"column:reference_id"` // usage_log id / order_id / requestID
	Note          *string   `gorm:"column:note"`
	CreatedAt     time.Time `gorm:"column:created_at"`
	UpdatedAt     time.Time `gorm:"column:updated_at"`
}

// TableName 返回 GORM 表名。
func (WalletTransaction) TableName() string { return "wallet_transactions" }

// RankingsSnapshotRun 对应 rankings_snapshot_runs（§13，8 列；append-only，快照时刻 snapshot_at）。
//
// 无 created_at/updated_at：R6 规定快照表只有 snapshot_at 一个时间戳。
type RankingsSnapshotRun struct {
	ID              int64     `gorm:"column:id;primaryKey"`
	Period          string    `gorm:"column:period"`
	WindowStart     time.Time `gorm:"column:window_start"`
	WindowEnd       time.Time `gorm:"column:window_end"`
	SnapshotAt      time.Time `gorm:"column:snapshot_at"`
	SampleThreshold int       `gorm:"column:sample_threshold"`
	Status          string    `gorm:"column:status"`
	ModelCount      int       `gorm:"column:model_count"`
}

// TableName 返回 GORM 表名。
func (RankingsSnapshotRun) TableName() string { return "rankings_snapshot_runs" }

// RankingsModelStat 对应 rankings_model_stats（§14，10 列；自然唯一 (run_id, model)）。
//
// Rank/PrevRank/DeltaTokens/DeltaPct 用指针保留可空语义：prev_rank NULL 表示
// 上一窗口样本不足/未入榜，delta_pct NULL 表示上窗口该模型 token 为 0。
// Slot 不是本表列，而是读投影时从 rankings_entities LEFT JOIN 来的稳定色板槽位
// （§C1.9 颜色跟随实体），故 gorm:"-"。
type RankingsModelStat struct {
	RunID       int64    `gorm:"column:run_id"`
	Model       string   `gorm:"column:model"`
	DisplayName *string  `gorm:"column:display_name"`
	Author      *string  `gorm:"column:author"`
	TotalTokens int64    `gorm:"column:total_tokens"`
	Requests    int64    `gorm:"column:requests"`
	Rank        *int     `gorm:"column:rank"`
	PrevRank    *int     `gorm:"column:prev_rank"`
	DeltaTokens *int64   `gorm:"column:delta_tokens"`
	DeltaPct    *float64 `gorm:"column:delta_pct"`
	Slot        *int16   `gorm:"-"`
}

// TableName 返回 GORM 表名。
func (RankingsModelStat) TableName() string { return "rankings_model_stats" }

// RankingsProviderStat 对应 rankings_provider_stats（§15，5 列；自然唯一 (run_id, author)）。
// Slot 同 RankingsModelStat，是读投影字段而非本表列。
type RankingsProviderStat struct {
	RunID       int64    `gorm:"column:run_id"`
	Author      string   `gorm:"column:author"`
	TotalTokens int64    `gorm:"column:total_tokens"`
	SharePct    *float64 `gorm:"column:share_pct"`
	Rank        *int     `gorm:"column:rank"`
	Slot        *int16   `gorm:"-"`
}

// TableName 返回 GORM 表名。
func (RankingsProviderStat) TableName() string { return "rankings_provider_stats" }

// RankingsTimeseries 对应 rankings_timeseries（§16，4 列；自然唯一 (model, granularity, bucket_start)）。
type RankingsTimeseries struct {
	Model       string    `gorm:"column:model"`
	Granularity string    `gorm:"column:granularity"`
	BucketStart time.Time `gorm:"column:bucket_start"`
	Tokens      int64     `gorm:"column:tokens"`
}

// TableName 返回 GORM 表名。
func (RankingsTimeseries) TableName() string { return "rankings_timeseries" }

// RankingsEntity 对应 rankings_entities（§17，3 列；自然唯一 (entity, kind)）。
// 槽位首次见即分配、永不回收/重排（R-D6-06）。
type RankingsEntity struct {
	Entity string `gorm:"column:entity"`
	Kind   string `gorm:"column:kind"`
	Slot   int16  `gorm:"column:slot"`
}

// TableName 返回 GORM 表名。
func (RankingsEntity) TableName() string { return "rankings_entities" }

// RequestRegistry 对应 request_registry（§18 / platform-04 V9，9 列；uuid 自然主键）。
type RequestRegistry struct{}

// TableName 返回 GORM 表名。
func (RequestRegistry) TableName() string { return "request_registry" }

// TaskPlugin 对应 task_plugins（modules-08 §3.10.2 B-11，10 列）。
//
// 口径（M6d R-M6D-01/05）：
//   - Key 是自然主键（text），不是代理 id；列名 "key" 在 SQL 中必须双引号，
//     GORM tag 只写裸列名（引用由 dialector 负责，见包注释）。
//   - Source 是 {builtin,upload} 层枚举，与版本行的 source_text 不同义
//     （R-M6D-04）；Builtin 标记是否内置 factory 层。
//   - ActiveVersion 是 active 版本的同事务镜像，**仅供展示**；active 的
//     唯一真源是 task_plugin_versions.active 的部分唯一索引（R-M6D-05）。
type TaskPlugin struct {
	Key           string    `gorm:"column:key;primaryKey"`
	Name          string    `gorm:"column:name"`
	Icon          *string   `gorm:"column:icon"`
	Source        string    `gorm:"column:source"`
	Builtin       bool      `gorm:"column:builtin"`
	Enabled       bool      `gorm:"column:enabled"`
	ActiveVersion *string   `gorm:"column:active_version"`
	Description   *string   `gorm:"column:description"`
	CreatedAt     time.Time `gorm:"column:created_at"`
	UpdatedAt     time.Time `gorm:"column:updated_at"`
}

// TableName 返回 GORM 表名。
func (TaskPlugin) TableName() string { return "task_plugins" }

// TaskPluginVersion 对应 task_plugin_versions（B-12，11 列）。
//
// 口径（R-M6D-04/05/11）：
//   - ID 是 uuid 字符串（PG uuid 列）；写入由 DB 的 gen_random_uuid() 生成，
//     本模型不设 default 标签（包级「零值不被标签改写」纪律）。
//   - SourceText 是插件源码正文；列名刻意不叫 source，避免与 B-11 的
//     层枚举同名异义（R-M6D-04）。列表接口不得下发该字段（S5）。
//   - SourceHash 是服务端计算的 SHA-256（64 hex），完整性校验依据（S3）。
//   - Active 是 active 的唯一真源；UploadedBy 是 bigint 用户 id，
//     逻辑引用 users.id，不设 FK（R-M6D-11）。
type TaskPluginVersion struct {
	ID         string    `gorm:"column:id;primaryKey"`
	Key        string    `gorm:"column:key"`
	Version    string    `gorm:"column:version"`
	APIVersion string    `gorm:"column:api_version"`
	SourceText string    `gorm:"column:source_text"`
	SourceHash string    `gorm:"column:source_hash"`
	Icon       *string   `gorm:"column:icon"`
	Enabled    bool      `gorm:"column:enabled"`
	Active     bool      `gorm:"column:active"`
	UploadedBy int64     `gorm:"column:uploaded_by"`
	CreatedAt  time.Time `gorm:"column:created_at"`
}

// TableName 返回 GORM 表名。
func (TaskPluginVersion) TableName() string { return "task_plugin_versions" }

// PluginMarketplaceSource 对应 plugin_marketplace_sources（B-13，7 列）。
//
// URL 仅 https（R-M6D-03），出站另走 sec.ssrf.* 守卫；LastError 截断存储，
// 不含远端响应体。
type PluginMarketplaceSource struct {
	ID              string     `gorm:"column:id;primaryKey"`
	Name            string     `gorm:"column:name"`
	URL             string     `gorm:"column:url"`
	Enabled         bool       `gorm:"column:enabled"`
	LastCheckStatus string     `gorm:"column:last_check_status"`
	LastCheckAt     *time.Time `gorm:"column:last_check_at"`
	LastError       *string    `gorm:"column:last_error"`
}

// TableName 返回 GORM 表名。
func (PluginMarketplaceSource) TableName() string { return "plugin_marketplace_sources" }

// Vendor 对应 vendors（platform-01 §22.7.2，7 列；D1-01）。
//
// 口径：
//   - 没有 version 列：乐观锁版本由 id/name/description/icon/status/
//     created_at/updated_at 规范化后 sha256 派生（见 Vendor.Version）。
//     持久化版本会形成第二真源，绕过应用的写入会让存储版本与内容脱节。
//   - Name 的唯一性由表达式索引 lower(name) 保证（大小写不敏感，停用行也占名）。
//   - ModelCount 是列表/详情投影的派生字段，**不落库**；Create/Update 返回
//     的记录可能未填充（0），需要计数时以 Get/List 为准。
type Vendor struct {
	ID          int64     `gorm:"column:id;primaryKey"`
	Name        string    `gorm:"column:name"`
	Description *string   `gorm:"column:description"`
	Icon        *string   `gorm:"column:icon"`
	Status      int16     `gorm:"column:status"` // smallint：1=启用 / 2=停用
	CreatedAt   time.Time `gorm:"column:created_at"`
	UpdatedAt   time.Time `gorm:"column:updated_at"`
	ModelCount  int64     `gorm:"-"`
}

// TableName 返回 GORM 表名。
func (Vendor) TableName() string { return "vendors" }

// ModelMeta 对应 model_meta（platform-01 §22.7.1 的 12 列 + 公开页 8 列 = 20 列；
// D1-02）。
//
// 口径：
//   - jsonb 列用 []byte 承载原始 JSON（与 Channel.SupportedModels 同型）；
//     Endpoints 固定为对象形态 {"/path":["POST"]}。
//   - VendorID 是逻辑引用 vendors.id（**无 FK**）；NULL = 未归属。
//   - Status：1=启用 / 2=停用（停用 ⇒ 公开面恒 hidden）。
//   - SyncOfficial=false 表示本地锁定，上游同步 preview 判 blocked。
//   - NameRule：0 精确 / 1 前缀 / 2 包含 / 3 后缀；注意 0 是合法零值。
type ModelMeta struct {
	ID              int64      `gorm:"column:id;primaryKey"`
	ModelName       string     `gorm:"column:model_name"`
	DisplayName     *string    `gorm:"column:display_name"`
	Description     *string    `gorm:"column:description"`
	Icon            *string    `gorm:"column:icon"`
	Tags            []byte     `gorm:"column:tags"`
	VendorID        *int64     `gorm:"column:vendor_id"`
	Endpoints       []byte     `gorm:"column:endpoints"`
	Capabilities    []byte     `gorm:"column:capabilities"`
	Modalities      []byte     `gorm:"column:modalities"`
	Parameters      []byte     `gorm:"column:parameters"`
	ContextLength   *int64     `gorm:"column:context_length"`
	MaxOutput       *int64     `gorm:"column:max_output"`
	KnowledgeCutoff *string    `gorm:"column:knowledge_cutoff"`
	ReleasedAt      *time.Time `gorm:"column:released_at"`
	Status          int16      `gorm:"column:status"`
	SyncOfficial    bool       `gorm:"column:sync_official"`
	NameRule        int16      `gorm:"column:name_rule"`
	CreatedAt       time.Time  `gorm:"column:created_at"`
	UpdatedAt       time.Time  `gorm:"column:updated_at"`
}

// TableName 返回 GORM 表名。
func (ModelMeta) TableName() string { return "model_meta" }

// SubscriptionPlan 对应 subscription_plans（billing-08 §2.1，17 列；D5a）。
//
// 口径：
//   - 本表**没有**模型价/倍率/折扣字段：订阅是资金账户，不是第二定价 owner
//     （billing-08 §2.2）。一次请求的单价仍由定价层产出。
//   - GrantCredits = 0 表示**无限额度**（R-D5-07），不是「无额度」；
//     上界守卫只在 AmountTotal/GrantCredits > 0 时由应用层生效。
//   - ModelScope 用 []byte 承载 jsonb 原文；nil = SQL NULL = 全部模型。
//   - Currency 目前只允许 CNY（R-D5-07 / DB CHECK）。
type SubscriptionPlan struct {
	ID                  int64     `gorm:"column:id;primaryKey"`
	PlanKey             string    `gorm:"column:plan_key"`
	Title               string    `gorm:"column:title"`
	Description         *string   `gorm:"column:description"`
	PeriodDays          int32     `gorm:"column:period_days"`
	PriceAmount         int64     `gorm:"column:price_amount"`
	Currency            string    `gorm:"column:currency"`
	GrantCredits        int64     `gorm:"column:grant_credits"`
	ResetOnRenew        bool      `gorm:"column:reset_on_renew"`
	AllowWalletOverflow bool      `gorm:"column:allow_wallet_overflow"`
	ModelScope          []byte    `gorm:"column:model_scope"`
	ConcurrencyLimit    *int32    `gorm:"column:concurrency_limit"`
	GraceDays           int32     `gorm:"column:grace_days"`
	Status              string    `gorm:"column:status"` // enabled/disabled
	SortOrder           int32     `gorm:"column:sort_order"`
	CreatedAt           time.Time `gorm:"column:created_at"`
	UpdatedAt           time.Time `gorm:"column:updated_at"`
}

// TableName 返回 GORM 表名。
func (SubscriptionPlan) TableName() string { return "subscription_plans" }

// Subscription 对应 subscriptions（billing-08 §6.1 + R-D5-03，23 列；D5a）。
//
// 口径：
//   - 状态三态 active/expired/cancelled；expired/cancelled 是终态，续费是
//     推进 EndAt 而不是新建行（§3.3 方案甲）。
//   - PeriodDays/ResetOnRenew/GraceDays/ModelScope/ConcurrencyLimit 是
//     购买瞬间的**套餐快照**（R-D5-03）：管理员编辑套餐不追溯已购实例。
//   - AmountTotal = 0 表示无限额度；AmountUsed 下界 0，上界只在
//     AmountTotal > 0 时由应用层守卫（R-D5-07 / R-D5-09）。
//   - OrderID 唯一（同一订单只建一条实例）；NULL 不参与唯一性。
type Subscription struct {
	ID                  int64      `gorm:"column:id;primaryKey"`
	UserID              int64      `gorm:"column:user_id"`
	PlanID              int64      `gorm:"column:plan_id"`
	PlanTitleSnapshot   string     `gorm:"column:plan_title_snapshot"`
	Status              string     `gorm:"column:status"`
	StartAt             time.Time  `gorm:"column:start_at"`
	EndAt               time.Time  `gorm:"column:end_at"`
	NextResetAt         *time.Time `gorm:"column:next_reset_at"`
	AmountTotal         int64      `gorm:"column:amount_total"`
	AmountUsed          int64      `gorm:"column:amount_used"`
	AllowWalletOverflow bool       `gorm:"column:allow_wallet_overflow"`
	AutoRenew           bool       `gorm:"column:auto_renew"`
	CancelAtPeriodEnd   bool       `gorm:"column:cancel_at_period_end"`
	OrderID             *int64     `gorm:"column:order_id"`
	Currency            string     `gorm:"column:currency"`
	PricePaidAmount     int64      `gorm:"column:price_paid_amount"`
	PeriodDays          int32      `gorm:"column:period_days"`
	ResetOnRenew        bool       `gorm:"column:reset_on_renew"`
	GraceDays           int32      `gorm:"column:grace_days"`
	ModelScope          []byte     `gorm:"column:model_scope"`
	ConcurrencyLimit    *int32     `gorm:"column:concurrency_limit"`
	CreatedAt           time.Time  `gorm:"column:created_at"`
	UpdatedAt           time.Time  `gorm:"column:updated_at"`
}

// TableName 返回 GORM 表名。
func (Subscription) TableName() string { return "subscriptions" }

// SubscriptionPreConsume 对应 subscription_pre_consume_records
// （platform-04 §4.5 V10 + R-D5-02，5 列；D5a）。
//
// RequestID 是 uuid 字符串（PG uuid 列），与 request_registry.request_id
// 同源同型；它是幂等唯一键，也是「已 refunded 再预扣报错」的判定行。
// SubscriptionID 让一次预扣只能落在一个实例上（禁止跨实例拆分）。
type SubscriptionPreConsume struct {
	RequestID      string    `gorm:"column:request_id;primaryKey"`
	SubscriptionID int64     `gorm:"column:subscription_id"`
	Status         string    `gorm:"column:status"` // consumed/refunded
	PreConsumed    int64     `gorm:"column:pre_consumed"`
	CreatedAt      time.Time `gorm:"column:created_at"`
}

// TableName 返回 GORM 表名。
func (SubscriptionPreConsume) TableName() string {
	return "subscription_pre_consume_records"
}

// Announcement 对应 announcements（modules-08 F-08-04 / R-D7-01，13 列；D7a）。
//
// 口径：
//   - ID 是 uuid 字符串（PG uuid 列），由 DB 的 gen_random_uuid() 生成；
//     本模型不设 default 标签（包级「零值不被标签改写」纪律）。
//   - Status 只有 draft/published/offline 三值；**expired 是虚拟态**
//     （status='published' 且 expire_at <= now），不落库（R-D7-10）。
//   - PublishAt 是「生效时间」（可预排未来），PublishedAt 是「实际点击发布
//     的时刻」，OfflineAt 是下线时刻；三者的区别是 N-35 的原地更新语义：
//     编辑已发布公告不重写 PublishAt/PublishedAt。
//   - CreatedBy/UpdatedBy 是 bigint 用户 id，逻辑引用 users.id（无 FK）。
//   - Read 是用户侧投影（LEFT JOIN announcement_reads），**不落库**。
type Announcement struct {
	ID          string     `gorm:"column:id;primaryKey"`
	Title       string     `gorm:"column:title"`
	Category    string     `gorm:"column:category"`
	Content     string     `gorm:"column:content"`
	PublishAt   time.Time  `gorm:"column:publish_at"`
	ExpireAt    *time.Time `gorm:"column:expire_at"`
	Status      string     `gorm:"column:status"`
	CreatedBy   int64      `gorm:"column:created_by"`
	UpdatedBy   int64      `gorm:"column:updated_by"`
	PublishedAt *time.Time `gorm:"column:published_at"`
	OfflineAt   *time.Time `gorm:"column:offline_at"`
	CreatedAt   time.Time  `gorm:"column:created_at"`
	UpdatedAt   time.Time  `gorm:"column:updated_at"`
	Read        bool       `gorm:"-"`
}

// TableName 返回 GORM 表名。
func (Announcement) TableName() string { return "announcements" }

// AnnouncementRead 对应 announcement_reads（R-D7-02，3 列）。复合主键即
// 幂等键：(user_id, announcement_id) 至多一行。
type AnnouncementRead struct {
	UserID         int64     `gorm:"column:user_id;primaryKey"`
	AnnouncementID string    `gorm:"column:announcement_id;primaryKey"`
	ReadAt         time.Time `gorm:"column:read_at"`
}

// TableName 返回 GORM 表名。
func (AnnouncementRead) TableName() string { return "announcement_reads" }

// 注：orders.biz_kind / biz_ref_id 已于 S3 加入 Order 投影（见 Order 注释）。
// 写入路径必须显式赋值，不能依赖 GORM 的零值：biz_kind 的零值 "" 会撞
// `orders_biz_kind_chk`。当前仅有 billing 包的显式 INSERT 写这两列。
// users.billing_preference 仍只存在于迁移与 EnumChecks（D5b 按列名读写）。
const (
	// OrderBizKindTopup 是充值订单（DB 默认值）。
	OrderBizKindTopup = "topup"
	// OrderBizKindSubscribe 是订阅购买订单；BizRefID = subscription_plans.id。
	OrderBizKindSubscribe = "subscribe"
)
