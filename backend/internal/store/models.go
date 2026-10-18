package store

import "time"

// M0 占位模型：16 个空壳 struct + TableName()。字段随里程碑逐步填充：
// M1 只填 User / APIToken / UserGroup（含 M1 迁移新增列）。
// 列定义唯一权威：platform-01 各 §；M1 补列裁决见实现计划 R-01/R-02/R-07。
// 表序同 platform-01 §0.1（主库 16 表；usage_logs/audit_logs 属日志库 Task 8）。

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
	Group                   string    `gorm:"column:\"group\""` // 保留字双引号（platform-04 §3.7 V8）
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
	Group       string     `gorm:"column:\"group\""` // 保留字双引号（platform-04 §3.7 V8）
	RemainQuota int64      `gorm:"column:remain_quota"`
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
type Channel struct{}

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
type RedemptionCode struct{}

// TableName 返回 GORM 表名。
func (RedemptionCode) TableName() string { return "redemption_codes" }

// Order 对应 orders（§10，23 列；含 G-1R P1/P2 新增 11 列）。
type Order struct{}

// TableName 返回 GORM 表名。
func (Order) TableName() string { return "orders" }

// WalletTransaction 对应 wallet_transactions（§11，11 列；有符号 bigint）。
type WalletTransaction struct{}

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
