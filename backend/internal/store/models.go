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

// Channel 对应 channels（§6，22 列；含 G-API-043 tag）。
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
	CreatedAt          time.Time  `gorm:"column:created_at"`
	UpdatedAt          time.Time  `gorm:"column:updated_at"`
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
	ID               int64      `gorm:"column:id;primaryKey"`
	UserID           int64      `gorm:"column:user_id"`         // 逻辑引用 users.id（不设 FK）
	Provider         string     `gorm:"column:provider"`        // stripe/alipay/creem/waffo/waffo_pancake/epay，创建后不可改
	Amount           int64      `gorm:"column:amount"`          // 最小货币单位（分），DB CHECK >=0
	Currency         string     `gorm:"column:currency"`        // ISO-4217 三字母
	Status           string     `gorm:"column:status"`          // 五态，见上
	IdempotencyKey   string     `gorm:"column:idempotency_key"` // 创建订单防重
	ProviderTxnID    *string    `gorm:"column:provider_txn_id"` // 上游流水号；人工旁路为 manual:{order_id}
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
type RankingsSnapshotRun struct{}

// TableName 返回 GORM 表名。
func (RankingsSnapshotRun) TableName() string { return "rankings_snapshot_runs" }

// RankingsModelStat 对应 rankings_model_stats（§14，10 列；自然唯一 (run_id, model)）。
type RankingsModelStat struct{}

// TableName 返回 GORM 表名。
func (RankingsModelStat) TableName() string { return "rankings_model_stats" }

// RankingsProviderStat 对应 rankings_provider_stats（§15，5 列；自然唯一 (run_id, author)）。
type RankingsProviderStat struct{}

// TableName 返回 GORM 表名。
func (RankingsProviderStat) TableName() string { return "rankings_provider_stats" }

// RankingsTimeseries 对应 rankings_timeseries（§16，4 列；自然唯一 (model, granularity, bucket_start)）。
type RankingsTimeseries struct{}

// TableName 返回 GORM 表名。
func (RankingsTimeseries) TableName() string { return "rankings_timeseries" }

// RankingsEntity 对应 rankings_entities（§17，3 列；自然唯一 (entity, kind)）。
type RankingsEntity struct{}

// TableName 返回 GORM 表名。
func (RankingsEntity) TableName() string { return "rankings_entities" }

// RequestRegistry 对应 request_registry（§18 / platform-04 V9，9 列；uuid 自然主键）。
type RequestRegistry struct{}

// TableName 返回 GORM 表名。
func (RequestRegistry) TableName() string { return "request_registry" }
