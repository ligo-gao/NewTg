package store

// M0 占位模型：16 个空壳 struct + TableName()。字段填充与 gorm column tag
// 随 M1+ 各里程碑落地（platform-01 各 §为列定义唯一权威）。
// 表序同 platform-01 §0.1（主库 16 表；usage_logs/audit_logs 属日志库 Task 8）。

// User 对应 users（platform-01 §1，23 列）。
type User struct{}

// TableName 返回 GORM 表名。
func (User) TableName() string { return "users" }

// APIToken 对应 api_tokens（§2，13 列；R2：id bigint）。
type APIToken struct{}

// TableName 返回 GORM 表名。
func (APIToken) TableName() string { return "api_tokens" }

// UserGroup 对应 user_groups（§3，9 列；R4：代理 id PK + name 唯一）。
type UserGroup struct{}

// TableName 返回 GORM 表名。
func (UserGroup) TableName() string { return "user_groups" }

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
