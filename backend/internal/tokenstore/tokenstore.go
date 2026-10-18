// Package tokenstore 实现 API 令牌的持久化、缓存与自动分组规则。
//
// 设计权威：docs/modules-02-API令牌-子功能设计.md（§1 字段、§2.9 自动分组、
// §2.10 跨组重试、§2.13 自读用量）、docs/platform-01-数据库字段汇总.md §2
// （api_tokens 15 列）。本计划冻结项：key = nkt- + 48 位 Base62（crypto/rand），
// DB 只存 SHA-256(完整明文)（GC-9）；列表只按 name 搜索（R-08）；缓存 TTL 60s +
// token:changed:<id> pub/sub 即时失效（GC-11）。
//
// GORM 陷阱（Task 1 复评）：store.APIToken 上的 `default:` tag 会让 Create 把
// Go 零值替换为默认值（remain_quota 0 → 100000）。本包因此不使用该模型做写入，
// 而是用无 default tag 的内部行结构 tokenRow（RemainQuota 为 *int64），
// 让「0 额度」与「NULL=不限额度」都能精确表达。
package tokenstore

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"newtg/gateway/internal/auth"
)

const (
	// KeyPrefix 是冻结的令牌前缀（security-03 §2.6）。
	KeyPrefix = "nkt-"
	// KeyRandomLength 是前缀后的 Base62 随机串长度。
	KeyRandomLength = 48

	// 配置键（modules-02 §1.3 / §2.9 / §2.10）。
	KeyAutoGroups         = "token.auto_groups"
	KeyMaxAutoGroups      = "token.max_auto_groups_per_token"
	KeyCrossGroupRetryMax = "relay.cross_group_retry_max_groups"

	// DefaultMaxAutoGroups 是单令牌自动分组条目上限默认值（业主裁决 O1）。
	DefaultMaxAutoGroups = 20
	// DefaultCrossGroupRetryMaxGroups 是跨组重试的尝试分组数上限默认值（裁决 O2）。
	DefaultCrossGroupRetryMaxGroups = 3
	// CacheTTL 是令牌元数据缓存的兜底有效期（GC-11）。
	CacheTTL = 60 * time.Second

	// GroupAuto 是自动分组的哨兵值。
	GroupAuto = "auto"

	// MaxBatchIDs 是批量端点单次上限（modules-02 §2.2）。
	MaxBatchIDs = 100

	// maxRemainQuota 是令牌额度上限（10^15 积分）；防止异常大数进入账本口径。
	maxRemainQuota = int64(1_000_000_000_000_000)
)

// api_tokens.status 的人工位（派生状态不入库，见 modules-02 §2.8）。
const (
	StatusEnabled  = "enabled"
	StatusDisabled = "disabled"
	// 以下为读时派生态（不落库）。
	StatusExpired   = "expired"
	StatusExhausted = "exhausted"
)

// 令牌鉴权/自读端点的错误分类（GC-11）。
var (
	ErrNotFound      = errors.New("tokenstore: token not found")
	ErrConflict      = errors.New("tokenstore: token modified elsewhere")
	ErrInvalidToken  = errors.New("tokenstore: invalid token")
	ErrTokenDisabled = errors.New("tokenstore: token disabled")
	ErrTokenExpired  = errors.New("tokenstore: token expired")
)

// Settings 是 configcenter.Center 的最小读接口（Get 返回 JSON 值）。
// 用缝隙而不是具体类型，单测无需 PG（GC-8）。
type Settings interface {
	Get(key string) (json.RawMessage, bool)
}

// ValidationError 是字段级校验错误；HTTP 层映射 400 + field。
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	if e.Field == "" {
		return e.Message
	}
	return e.Field + ": " + e.Message
}

// Token 是管理面令牌视图。刻意不含 KeyHash：任何读取路径都不得返回哈希。
type Token struct {
	ID              int64
	UserID          int64
	Name            string
	Status          string // 人工位：enabled|disabled
	Group           string
	ExpiresAt       *time.Time
	ModelLimits     []string // nil=不限
	RemainQuota     *int64   // nil=不限额度（DB NULL）
	AllowIPs        []string // nil=不限
	AutoGroups      []string // 有序；仅 group=auto 时非空
	CrossGroupRetry bool
	LastUsedAt      *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Created 是创建结果：明文 key 只在创建响应出现一次（GC-9）。
type Created struct {
	Token *Token
	Key   string
}

// CreateInput 是创建令牌的领域输入（HTTP DTO 在 httpapi 层）。
type CreateInput struct {
	Name            string
	Group           string // "" = 用户当前分组
	ExpiresAt       *time.Time
	Quantity        int // 0 → 1
	UnlimitedQuota  bool
	RemainQuota     int64
	ModelLimits     []string
	AllowIPs        []string
	AutoGroups      []string
	CrossGroupRetry bool
}

// UpdateInput 是部分更新输入。指针 nil = 未提交该字段（部分更新语义）。
type UpdateInput struct {
	UpdatedAt       *time.Time // 乐观锁版本（必填）
	Name            *string
	Group           *string
	ExpiresAtSet    bool
	ExpiresAt       *time.Time
	UnlimitedQuota  *bool
	RemainQuota     *int64
	ModelLimitsSet  bool
	ModelLimits     []string
	AllowIPsSet     bool
	AllowIPs        []string
	AutoGroupsSet   bool
	AutoGroups      []string
	CrossGroupRetry *bool
}

// ListFilter 是列表筛选（offset 分页，GC-6）。
type ListFilter struct {
	Keyword  string
	Status   string // enabled|disabled|expired|exhausted
	Group    string
	Quota    string // normal|low|exhausted|unlimited
	Page     int
	PageSize int
}

// BatchFailure 是批量操作的条目级失败（modules-02 §2.2）。
// Name 仅在该令牌属于当前用户且仍存在时填写；他人/不存在一律留空（不泄露存在性）。
type BatchFailure struct {
	ID     int64
	Name   string
	Detail string
}

// AutoGroup 是自动分组候选（ratio 为小数倍率：DB 150 → 1.5）。
type AutoGroup struct {
	Name  string
	Ratio float64
}

// Store 是令牌 CRUD + 缓存 + 自动分组规则的实现。
//
// 缓存放在 Redis（GC-11）：token:meta:<hash> 与 token:view:<id> 均为 60s TTL，
// 变更时 DEL + INCR token:gen + PUBLISH token:changed:<id>。用 Redis 而不是
// 进程内 map 的原因：多副本共享同一份失效状态，避免「A 禁用、B 仍缓存启用」
// 只能靠 TTL 兜底。token:gen 是全局代际计数，用于消除 read-add 竞争：
// 缓存回填前 WATCH 它，若期间发生过任何变更就放弃回填，防止把陈旧行写回缓存。
type Store struct {
	db       *gorm.DB
	rdb      *redis.Client
	settings Settings
	ttl      time.Duration
	now      func() time.Time
}

// 编译期钉死 Task 4 的鉴权接缝：Store 必须能直接作为 auth.TokenLookup 交给
// SessionAuth。签名漂移会在这里编译失败，而不是等到装配期才发现。
var _ auth.TokenLookup = (*Store)(nil)

// New 构造令牌存储。settings 可为 nil（全部走默认值）；rdb 可为 nil（无缓存，
// 仅测试装配会这么做）。
func New(db *gorm.DB, rdb *redis.Client, settings Settings) *Store {
	return &Store{
		db:       db,
		rdb:      rdb,
		settings: settings,
		ttl:      CacheTTL,
		now:      time.Now,
	}
}

// ---------------------------------------------------------------------------
// 密钥生成
// ---------------------------------------------------------------------------

const base62Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// GenerateKey 生成 api_tokens 的明文/哈希/掩码。
//
// 随机源为 crypto/rand；使用拒绝采样（丢弃 >=248 的字节）避免取模偏置。
// keyHash 哈希的是**完整明文**（含 nkt- 前缀），与 auth.HashTokenKey 同口径。
func GenerateKey() (plain, keyHash, masked string, err error) {
	var sb strings.Builder
	sb.Grow(len(KeyPrefix) + KeyRandomLength)
	sb.WriteString(KeyPrefix)

	const limit = 248 // 256 - (256 % 62)：拒绝采样边界，消除取模偏置
	buf := make([]byte, KeyRandomLength+8)
	for written := 0; written < KeyRandomLength; {
		if _, err := rand.Read(buf); err != nil {
			return "", "", "", fmt.Errorf("tokenstore: crypto/rand: %w", err)
		}
		for _, b := range buf {
			if b >= limit {
				continue
			}
			sb.WriteByte(base62Alphabet[int(b)%len(base62Alphabet)])
			written++
			if written == KeyRandomLength {
				break
			}
		}
	}
	plain = sb.String()
	return plain, auth.HashTokenKey(plain), MaskedKey(), nil
}

// MaskedKey 返回列表/详情使用的固定掩码：nkt- + 24×•（U+2022）。
func MaskedKey() string { return KeyPrefix + strings.Repeat("•", 24) }

// ---------------------------------------------------------------------------
// 校验/派生（纯函数，单测覆盖）
// ---------------------------------------------------------------------------

// normalizeAutoList 只做裁剪与去空，**不去重**：重复项必须在
// validateAutoGroups 里显式拒绝（TC-F-02-13-03），不能静默吞掉。
func normalizeAutoList(list []string) []string {
	if len(list) == 0 {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, raw := range list {
		if value := strings.TrimSpace(raw); value != "" {
			out = append(out, value)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func validateAutoGroups(list []string, allowed map[string]struct{}, max int) *ValidationError {
	list = normalizeAutoList(list)
	if len(list) == 0 {
		return &ValidationError{Field: "auto_groups", Message: "自动分组至少需要一个分组"}
	}
	if max <= 0 {
		max = DefaultMaxAutoGroups
	}
	if len(list) > max {
		return &ValidationError{
			Field:   "auto_groups",
			Message: fmt.Sprintf("最多可选择 %d 个自动分组", max),
		}
	}
	seen := make(map[string]struct{}, len(list))
	for _, group := range list {
		if _, dup := seen[group]; dup {
			return &ValidationError{Field: "auto_groups", Message: "自动分组不能重复：" + group}
		}
		seen[group] = struct{}{}
		if _, ok := allowed[group]; !ok {
			return &ValidationError{Field: "auto_groups", Message: "分组不在自动分组候选集内：" + group}
		}
	}
	return nil
}

// deriveStatus 实现 modules-02 §2.8 的优先级：disabled > expired > exhausted > enabled。
func deriveStatus(raw string, expiresAt *time.Time, remain *int64, now time.Time) string {
	switch {
	case raw == StatusDisabled:
		return StatusDisabled
	case expiresAt != nil && !now.Before(*expiresAt):
		return StatusExpired
	case remain != nil && *remain == 0:
		return StatusExhausted
	default:
		return StatusEnabled
	}
}

// DeriveStatus 是 deriveStatus 的公开形态（HTTP 层映射列表 status 字段）。
func DeriveStatus(t *Token, now time.Time) string {
	if t == nil {
		return ""
	}
	return deriveStatus(t.Status, t.ExpiresAt, t.RemainQuota, now)
}

// normalizeGroupCoupling 冻结 I-23：仅 group=auto 保留候选集与跨组开关。
func normalizeGroupCoupling(group string, auto []string, retry bool) ([]string, bool) {
	if group != GroupAuto {
		return nil, false
	}
	return auto, retry
}

func normalizeName(raw string) (string, *ValidationError) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", &ValidationError{Field: "name", Message: "令牌名不能为空"}
	}
	if utf8.RuneCountInString(name) > 64 {
		return "", &ValidationError{Field: "name", Message: "令牌名最长 64 个字符"}
	}
	return name, nil
}

func normalizeStringList(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, raw := range in {
		value := strings.TrimSpace(raw)
		if value == "" {
			continue
		}
		if _, dup := seen[value]; dup {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func normalizeIPs(in []string) ([]string, *ValidationError) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, raw := range in {
		value := strings.TrimSpace(raw)
		if value == "" {
			continue
		}
		if _, _, err := net.ParseCIDR(value); err != nil {
			if net.ParseIP(value) == nil {
				return nil, &ValidationError{Field: "allow_ips", Message: "非法 IP 或 CIDR：" + value}
			}
		}
		if _, dup := seen[value]; dup {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 配置读取（损坏值回退默认，避免令牌接口 500）
// ---------------------------------------------------------------------------

// maxAutoGroups 返回单令牌自动分组条目上限（配置损坏时回退默认 20）。
func maxAutoGroups(settings Settings) int {
	return configIntFrom(settings, KeyMaxAutoGroups, DefaultMaxAutoGroups)
}

func crossGroupRetryMax(settings Settings) int {
	return configIntFrom(settings, KeyCrossGroupRetryMax, DefaultCrossGroupRetryMaxGroups)
}

func configIntFrom(settings Settings, key string, def int) int {
	if settings == nil {
		return def
	}
	raw, ok := settings.Get(key)
	if !ok || len(raw) == 0 {
		return def
	}
	var n int
	if err := json.Unmarshal(raw, &n); err == nil && n > 0 {
		return n
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if parsed, err := strconv.Atoi(strings.TrimSpace(text)); err == nil && parsed > 0 {
			return parsed
		}
	}
	return def
}

func siteAutoGroups(settings Settings) []string {
	if settings == nil {
		return nil
	}
	raw, ok := settings.Get(KeyAutoGroups)
	if !ok || len(raw) == 0 {
		return nil
	}
	var groups []string
	if err := json.Unmarshal(raw, &groups); err != nil {
		return nil
	}
	return normalizeStringList(groups)
}

// CrossGroupRetryMaxGroups 暴露 relay.cross_group_retry_max_groups（默认 3）。
// M1 的令牌接口不消费该值；保留给中继跨组重试（modules-02 §2.10，M3）。
func (s *Store) CrossGroupRetryMaxGroups() int {
	return crossGroupRetryMax(s.settings)
}

// ---------------------------------------------------------------------------
// GORM 行结构（无 default tag — 规避 GORM 的零值替换）
// ---------------------------------------------------------------------------

type tokenRow struct {
	ID              int64      `gorm:"column:id;primaryKey;autoIncrement"`
	UserID          int64      `gorm:"column:user_id"`
	Name            string     `gorm:"column:name"`
	KeyHash         string     `gorm:"column:key_hash"`
	Status          string     `gorm:"column:status"`
	ExpiresAt       *time.Time `gorm:"column:expires_at"`
	ModelLimits     *string    `gorm:"column:model_limits"`
	Group           string     `gorm:"column:group"`
	RemainQuota     *int64     `gorm:"column:remain_quota"`
	AllowIPs        *string    `gorm:"column:allow_ips"`
	LastUsedAt      *time.Time `gorm:"column:last_used_at"`
	CreatedAt       time.Time  `gorm:"column:created_at"`
	UpdatedAt       time.Time  `gorm:"column:updated_at"`
	AutoGroups      *string    `gorm:"column:auto_groups"`
	CrossGroupRetry bool       `gorm:"column:cross_group_retry"`
}

func (tokenRow) TableName() string { return "api_tokens" }

func (r *tokenRow) toToken() *Token {
	return &Token{
		ID:              r.ID,
		UserID:          r.UserID,
		Name:            r.Name,
		Status:          r.Status,
		Group:           r.Group,
		ExpiresAt:       r.ExpiresAt,
		ModelLimits:     decodeStringArray(r.ModelLimits),
		RemainQuota:     r.RemainQuota,
		AllowIPs:        decodeStringArray(r.AllowIPs),
		AutoGroups:      decodeStringArray(r.AutoGroups),
		CrossGroupRetry: r.CrossGroupRetry,
		LastUsedAt:      r.LastUsedAt,
		CreatedAt:       r.CreatedAt,
		UpdatedAt:       r.UpdatedAt,
	}
}

func decodeStringArray(raw *string) []string {
	if raw == nil || *raw == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(*raw), &out); err != nil {
		return nil
	}
	return out
}

func encodeStringArray(values []string) *string {
	if len(values) == 0 {
		return nil
	}
	raw, err := json.Marshal(values)
	if err != nil {
		return nil
	}
	text := string(raw)
	return &text
}

// encodeAutoGroups 返回 auto_groups 列值：非 auto 用规范空字符串（Task 1 约定），
// auto 用有序 JSON 数组文本。
func encodeAutoGroups(group string, values []string) *string {
	if group != GroupAuto {
		empty := ""
		return &empty
	}
	return encodeStringArray(values)
}

func (s *Store) dbWith(ctx context.Context) *gorm.DB {
	return s.db.WithContext(ctx)
}

func (s *Store) loadRow(ctx context.Context, id int64) (*tokenRow, error) {
	var row tokenRow
	err := s.dbWith(ctx).Take(&row, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

func (s *Store) loadToken(ctx context.Context, id int64) (*Token, error) {
	row, err := s.loadRow(ctx, id)
	if err != nil {
		return nil, err
	}
	return row.toToken(), nil
}

// ---------------------------------------------------------------------------
// 用户分组作用域 / 自动分组
// ---------------------------------------------------------------------------

type userScope struct {
	userGroup  string
	usable     map[string]struct{}
	candidates map[string]struct{}
	ratios     map[string]float64
	maxAuto    int
}

func (s *Store) userScope(ctx context.Context, userID int64) (*userScope, error) {
	var userGroup sql.NullString
	err := s.dbWith(ctx).Raw(`SELECT "group" FROM users WHERE id = ?`, userID).Row().Scan(&userGroup)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	type groupRow struct {
		Name           string
		Ratio          int32
		UserSelectable bool
		Status         int16
	}
	var groups []groupRow
	if err := s.dbWith(ctx).Table("user_groups").
		Select(`name, ratio, user_selectable, status`).
		Find(&groups).Error; err != nil {
		return nil, err
	}

	usable := make(map[string]struct{}, len(groups))
	ratios := make(map[string]float64, len(groups))
	for _, group := range groups {
		if group.Status != 1 {
			continue
		}
		// 用户自己的分组即使未被标记为 user_selectable 也可用；其余仅限可选分组。
		if !group.UserSelectable && group.Name != userGroup.String {
			continue
		}
		usable[group.Name] = struct{}{}
		ratios[group.Name] = float64(group.Ratio) / 100
	}

	candidates := make(map[string]struct{})
	for _, name := range siteAutoGroups(s.settings) {
		// auto 是哨兵而不是真实分组；即使它被误配置进白名单或成为用户分组，
		// 也绝不能出现在自动分组候选集里（modules-02 §2.9）。
		if name == GroupAuto {
			continue
		}
		if _, ok := usable[name]; ok {
			candidates[name] = struct{}{}
		}
	}

	return &userScope{
		userGroup:  userGroup.String,
		usable:     usable,
		candidates: candidates,
		ratios:     ratios,
		maxAuto:    maxAutoGroups(s.settings),
	}, nil
}

// AutoGroups 返回当前用户可选的自动分组候选 + 条目上限。
// 顺序 = 站点白名单顺序（I-22），并去重（I-21）。
func (s *Store) AutoGroups(ctx context.Context, userID int64) ([]AutoGroup, int, error) {
	scope, err := s.userScope(ctx, userID)
	if err != nil {
		return nil, 0, err
	}
	seen := make(map[string]struct{})
	out := make([]AutoGroup, 0, len(scope.candidates))
	for _, name := range siteAutoGroups(s.settings) {
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		if _, ok := scope.candidates[name]; !ok {
			continue
		}
		out = append(out, AutoGroup{Name: name, Ratio: scope.ratios[name]})
	}
	return out, scope.maxAuto, nil
}

// validateGroup 校验最终分组与自动分组列表，并返回规范化后的 (autoGroups, retry)。
func (scope *userScope) validateGroup(group string, auto []string, retry bool) ([]string, bool, *ValidationError) {
	if group == "" {
		return nil, false, &ValidationError{Field: "group", Message: "分组不能为空"}
	}
	if group == GroupAuto {
		if err := validateAutoGroups(auto, scope.candidates, scope.maxAuto); err != nil {
			return nil, false, err
		}
		normalized, retry := normalizeGroupCoupling(group, normalizeAutoList(auto), retry)
		return normalized, retry, nil
	}
	if _, ok := scope.usable[group]; !ok {
		return nil, false, &ValidationError{Field: "group", Message: "分组不在用户可用分组内：" + group}
	}
	normalized, retry := normalizeGroupCoupling(group, auto, retry)
	return normalized, retry, nil
}

// ---------------------------------------------------------------------------
// CRUD
// ---------------------------------------------------------------------------

// Create 创建 1..100 枚令牌（单事务）。name 允许重复；key 明文只在返回值出现。
func (s *Store) Create(ctx context.Context, userID int64, in CreateInput) ([]Created, error) {
	quantity := in.Quantity
	if quantity == 0 {
		quantity = 1
	}
	if quantity < 0 || quantity > MaxBatchIDs {
		return nil, &ValidationError{Field: "quantity", Message: fmt.Sprintf("数量必须在 1..%d 之间", MaxBatchIDs)}
	}
	name, verr := normalizeName(in.Name)
	if verr != nil {
		return nil, verr
	}
	modelLimits := normalizeStringList(in.ModelLimits)
	allowIPs, verr := normalizeIPs(in.AllowIPs)
	if verr != nil {
		return nil, verr
	}
	now := s.now().UTC().Truncate(time.Microsecond)
	if in.ExpiresAt != nil && !in.ExpiresAt.After(now) {
		return nil, &ValidationError{Field: "expires_at", Message: "过期时间必须晚于当前时间"}
	}
	if !in.UnlimitedQuota && (in.RemainQuota < 0 || in.RemainQuota > maxRemainQuota) {
		return nil, &ValidationError{Field: "remain_quota", Message: "剩余额度必须在 0..10^15 之间"}
	}
	// 批量创建按数量宽度编号（quantity=12 → name-01…name-12），单枚保持原名。
	names := make([]string, quantity)
	if quantity == 1 {
		names[0] = name
	} else {
		width := len(strconv.Itoa(quantity))
		for i := 1; i <= quantity; i++ {
			itemName := fmt.Sprintf("%s-%0*d", name, width, i)
			if utf8.RuneCountInString(itemName) > 64 {
				return nil, &ValidationError{Field: "name", Message: "批量创建时名称过长（含序号需 ≤64 字符）"}
			}
			names[i-1] = itemName
		}
	}

	scope, err := s.userScope(ctx, userID)
	if err != nil {
		return nil, err
	}
	group := in.Group
	if group == "" {
		group = scope.userGroup
	}
	autoGroups, retry, verr := scope.validateGroup(group, in.AutoGroups, in.CrossGroupRetry)
	if verr != nil {
		return nil, verr
	}

	var remain *int64
	if !in.UnlimitedQuota {
		quota := in.RemainQuota
		remain = &quota
	}
	autoEncoded := encodeAutoGroups(group, autoGroups)
	modelEncoded := encodeStringArray(modelLimits)
	ipEncoded := encodeStringArray(allowIPs)

	created := make([]Created, 0, quantity)
	err = s.dbWith(ctx).Transaction(func(tx *gorm.DB) error {
		for i := 0; i < quantity; i++ {
			plain, keyHash, _, err := GenerateKey()
			if err != nil {
				return err
			}
			row := tokenRow{
				UserID:          userID,
				Name:            names[i],
				KeyHash:         keyHash,
				Status:          StatusEnabled,
				ExpiresAt:       in.ExpiresAt,
				ModelLimits:     modelEncoded,
				Group:           group,
				RemainQuota:     remain,
				AllowIPs:        ipEncoded,
				AutoGroups:      autoEncoded,
				CrossGroupRetry: retry,
				CreatedAt:       now,
				UpdatedAt:       now,
			}
			if err := tx.Create(&row).Error; err != nil {
				return err
			}
			created = append(created, Created{Token: row.toToken(), Key: plain})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}

// List 返回当前用户的令牌（offset 分页）。搜索只匹配 name（R-08）。
func (s *Store) List(ctx context.Context, userID int64, filter ListFilter) ([]*Token, int64, error) {
	page := filter.Page
	if page < 1 {
		page = 1
	}
	pageSize := filter.PageSize
	if pageSize <= 0 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}
	now := s.now().UTC()

	apply := func(q *gorm.DB) *gorm.DB {
		q = q.Where("user_id = ?", userID)
		if keyword := strings.TrimSpace(filter.Keyword); keyword != "" {
			q = q.Where(`name ILIKE ? ESCAPE '\'`, "%"+escapeLike(keyword)+"%")
		}
		switch filter.Status {
		case StatusEnabled:
			q = q.Where("status = ? AND (expires_at IS NULL OR expires_at > ?) AND (remain_quota IS NULL OR remain_quota > 0)", StatusEnabled, now)
		case StatusDisabled:
			q = q.Where("status = ?", StatusDisabled)
		case StatusExpired:
			q = q.Where("status = ? AND expires_at IS NOT NULL AND expires_at <= ?", StatusEnabled, now)
		case StatusExhausted:
			q = q.Where("status = ? AND (expires_at IS NULL OR expires_at > ?) AND remain_quota = 0", StatusEnabled, now)
		}
		if filter.Group != "" {
			q = q.Where(`"group" = ?`, filter.Group)
		}
		switch filter.Quota {
		case "unlimited":
			q = q.Where("remain_quota IS NULL")
		case "exhausted":
			q = q.Where("remain_quota = 0")
		case "normal":
			q = q.Where("remain_quota > 0")
		case "low":
			q = q.Where("remain_quota > 0 AND remain_quota < 10000")
		}
		return q
	}

	var total int64
	if err := apply(s.dbWith(ctx).Model(&tokenRow{})).Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []tokenRow
	if err := apply(s.dbWith(ctx).Model(&tokenRow{})).
		Order("created_at DESC, id DESC").
		Offset((page - 1) * pageSize).
		Limit(pageSize).
		Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	items := make([]*Token, 0, len(rows))
	for i := range rows {
		items = append(items, rows[i].toToken())
	}
	return items, total, nil
}

// Get 读取单个令牌（Redis 缓存 60s；token:changed 即时失效，GC-11）。
// 非本人令牌返回 ErrNotFound（不泄露存在性）。
func (s *Store) Get(ctx context.Context, userID, id int64) (*Token, error) {
	if cached, ok := s.loadCachedToken(ctx, id); ok {
		if cached.UserID != userID {
			return nil, ErrNotFound
		}
		return cached, nil
	}

	// 代际在读 DB 之前取：任何并发变更都会让 WATCH 失败并放弃回填。
	gen, cacheable := s.cacheGeneration(ctx)
	row, err := s.loadRow(ctx, id)
	if err != nil {
		return nil, err
	}
	token := row.toToken()
	if cacheable {
		s.storeCachedToken(ctx, gen, token, row.KeyHash)
	}
	if token.UserID != userID {
		return nil, ErrNotFound
	}
	return token, nil
}

// Update 部分更新 + updated_at 乐观锁。版本不匹配 → ErrConflict（409）。
func (s *Store) Update(ctx context.Context, userID, id int64, in UpdateInput) (*Token, error) {
	if in.UpdatedAt == nil {
		return nil, &ValidationError{Field: "updated_at", Message: "缺少乐观锁版本 updated_at"}
	}
	row, err := s.loadRow(ctx, id)
	if err != nil {
		return nil, err
	}
	if row.UserID != userID {
		return nil, ErrNotFound
	}
	if !sameInstant(row.UpdatedAt, *in.UpdatedAt) {
		return nil, ErrConflict
	}
	// PG timestamptz 精度为微秒；把客户端版本对齐到同一精度，避免
	// sameInstant 已判等而 WHERE 仍因亚微秒尾数不匹配（误报 409）。
	expectedUpdatedAt := in.UpdatedAt.UTC().Truncate(time.Microsecond)

	next := row.toToken()
	if in.Name != nil {
		name, verr := normalizeName(*in.Name)
		if verr != nil {
			return nil, verr
		}
		next.Name = name
	}
	if in.Group != nil {
		next.Group = *in.Group
	}
	if in.ExpiresAtSet {
		next.ExpiresAt = in.ExpiresAt
	}
	if in.UnlimitedQuota != nil {
		if *in.UnlimitedQuota {
			next.RemainQuota = nil
		} else if in.RemainQuota != nil {
			quota := *in.RemainQuota
			next.RemainQuota = &quota
		} else if next.RemainQuota == nil {
			return nil, &ValidationError{Field: "remain_quota", Message: "关闭不限额度时必须提供剩余额度"}
		}
	} else if in.RemainQuota != nil {
		quota := *in.RemainQuota
		next.RemainQuota = &quota
	}
	if next.RemainQuota != nil && (*next.RemainQuota < 0 || *next.RemainQuota > maxRemainQuota) {
		return nil, &ValidationError{Field: "remain_quota", Message: "剩余额度必须在 0..10^15 之间"}
	}
	if in.ModelLimitsSet {
		next.ModelLimits = normalizeStringList(in.ModelLimits)
	}
	if in.AllowIPsSet {
		ips, verr := normalizeIPs(in.AllowIPs)
		if verr != nil {
			return nil, verr
		}
		next.AllowIPs = ips
	}
	if in.AutoGroupsSet {
		next.AutoGroups = normalizeStringList(in.AutoGroups)
	}
	if in.CrossGroupRetry != nil {
		next.CrossGroupRetry = *in.CrossGroupRetry
	}

	nowTime := s.now().UTC().Truncate(time.Microsecond)
	// 只有显式提交了**新的** expires_at 才校验未来时间：已过期令牌的改名/
	// 改额度等 PATCH 必须继续可用（延长过期时间本来就是为了让 token 复活）。
	if in.ExpiresAtSet && next.ExpiresAt != nil && !next.ExpiresAt.After(nowTime) {
		return nil, &ValidationError{Field: "expires_at", Message: "过期时间必须晚于当前时间"}
	}

	// 只在校验相关字段被提交时重新校验分组：仅改 name 的 PATCH 不得因为
	// 管理员事后调整候选集而失败（TC-F-02-13-07 的部分更新语义）。
	groupChanged := in.Group != nil && *in.Group != row.Group
	autoChanged := in.AutoGroupsSet || in.CrossGroupRetry != nil
	if groupChanged || autoChanged {
		scope, err := s.userScope(ctx, userID)
		if err != nil {
			return nil, err
		}
		autoGroups, retry, verr := scope.validateGroup(next.Group, next.AutoGroups, next.CrossGroupRetry)
		if verr != nil {
			return nil, verr
		}
		next.AutoGroups = autoGroups
		next.CrossGroupRetry = retry
	} else if next.Group != GroupAuto {
		next.AutoGroups = nil
		next.CrossGroupRetry = false
	}

	updates := map[string]any{
		"name":              next.Name,
		"group":             next.Group,
		"expires_at":        next.ExpiresAt,
		"remain_quota":      next.RemainQuota,
		"model_limits":      encodeStringArray(next.ModelLimits),
		"allow_ips":         encodeStringArray(next.AllowIPs),
		"auto_groups":       encodeAutoGroups(next.Group, next.AutoGroups),
		"cross_group_retry": next.CrossGroupRetry,
		"updated_at":        nowTime,
	}
	result := s.dbWith(ctx).Model(&tokenRow{}).
		Where("id = ? AND user_id = ? AND updated_at = ?", id, userID, expectedUpdatedAt).
		Updates(updates)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		// 区分「并发修改」与「行已删/非本人」：前者 409，后者 404。
		var count int64
		if err := s.dbWith(ctx).Model(&tokenRow{}).Where("id = ? AND user_id = ?", id, userID).Count(&count).Error; err != nil {
			return nil, err
		}
		if count == 0 {
			return nil, ErrNotFound
		}
		return nil, ErrConflict
	}
	s.invalidateToken(ctx, id, row.KeyHash)

	return s.loadToken(ctx, id)
}

// Delete 物理删除（R14：无软删除、无撤销窗口）。
// expectedUpdatedAt 非 nil 时执行乐观锁（陈旧 → ErrConflict）。
func (s *Store) Delete(ctx context.Context, userID, id int64, expectedUpdatedAt *time.Time) error {
	// 先读 key_hash：删除后无法再查，而 token:meta:<hash> 必须在同一轮失效。
	row, err := s.loadRow(ctx, id)
	if err != nil {
		return err
	}
	if row.UserID != userID {
		return ErrNotFound
	}
	query := s.dbWith(ctx).Where("id = ? AND user_id = ?", id, userID)
	if expectedUpdatedAt != nil {
		query = query.Where("updated_at = ?", expectedUpdatedAt.UTC().Truncate(time.Microsecond))
	}
	result := query.Delete(&tokenRow{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		var count int64
		if err := s.dbWith(ctx).Model(&tokenRow{}).Where("id = ? AND user_id = ?", id, userID).Count(&count).Error; err != nil {
			return err
		}
		if count == 0 {
			return ErrNotFound
		}
		return ErrConflict
	}
	s.invalidateToken(ctx, id, row.KeyHash)
	return nil
}

// Enable 启用令牌（人工位，不改变派生状态）。
func (s *Store) Enable(ctx context.Context, userID, id int64) (*Token, error) {
	return s.setStatus(ctx, userID, id, StatusEnabled)
}

// Disable 禁用令牌。
func (s *Store) Disable(ctx context.Context, userID, id int64) (*Token, error) {
	return s.setStatus(ctx, userID, id, StatusDisabled)
}

func (s *Store) setStatus(ctx context.Context, userID, id int64, status string) (*Token, error) {
	row, err := s.loadRow(ctx, id)
	if err != nil {
		return nil, err
	}
	if row.UserID != userID {
		return nil, ErrNotFound
	}
	result := s.dbWith(ctx).Model(&tokenRow{}).
		Where("id = ? AND user_id = ?", id, userID).
		Updates(map[string]any{"status": status, "updated_at": s.now().UTC().Truncate(time.Microsecond)})
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		return nil, ErrNotFound
	}
	s.invalidateToken(ctx, id, row.KeyHash)
	return s.loadToken(ctx, id)
}

// ResetKey 生成新密钥并立即废弃旧 key。明文只在此返回值出现一次。
func (s *Store) ResetKey(ctx context.Context, userID, id int64) (plain, masked string, token *Token, err error) {
	row, err := s.loadRow(ctx, id)
	if err != nil {
		return "", "", nil, err
	}
	if row.UserID != userID {
		return "", "", nil, ErrNotFound
	}
	oldHash := row.KeyHash
	plain, keyHash, masked, err := GenerateKey()
	if err != nil {
		return "", "", nil, err
	}
	result := s.dbWith(ctx).Model(&tokenRow{}).
		Where("id = ? AND user_id = ?", id, userID).
		Updates(map[string]any{"key_hash": keyHash, "updated_at": s.now().UTC().Truncate(time.Microsecond)})
	if result.Error != nil {
		return "", "", nil, result.Error
	}
	if result.RowsAffected == 0 {
		return "", "", nil, ErrNotFound
	}
	s.invalidateToken(ctx, id, oldHash)
	token, err = s.loadToken(ctx, id)
	if err != nil {
		return "", "", nil, err
	}
	return plain, masked, token, nil
}

// ---------------------------------------------------------------------------
// 批量操作（逐条独立结果，部分失败不中断）
// ---------------------------------------------------------------------------

// BatchEnable 逐条启用；返回成功数 + 失败明细。
func (s *Store) BatchEnable(ctx context.Context, userID int64, ids []int64) (int, []BatchFailure, error) {
	return s.batch(ctx, userID, ids, func(id int64) error {
		_, err := s.Enable(ctx, userID, id)
		return err
	})
}

// BatchDisable 逐条禁用。
func (s *Store) BatchDisable(ctx context.Context, userID int64, ids []int64) (int, []BatchFailure, error) {
	return s.batch(ctx, userID, ids, func(id int64) error {
		_, err := s.Disable(ctx, userID, id)
		return err
	})
}

// BatchDelete 逐条物理删除。
func (s *Store) BatchDelete(ctx context.Context, userID int64, ids []int64) (int, []BatchFailure, error) {
	return s.batch(ctx, userID, ids, func(id int64) error {
		return s.Delete(ctx, userID, id, nil)
	})
}

func (s *Store) batch(ctx context.Context, userID int64, ids []int64, action func(id int64) error) (int, []BatchFailure, error) {
	succeeded := 0
	failed := make([]BatchFailure, 0)
	seen := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		if err := action(id); err != nil {
			failure := BatchFailure{ID: id, Detail: batchFailureDetail(err)}
			// 只有确认属于当前用户且仍存在的令牌才回填名称；他人/不存在保持空（不泄露）。
			if !errors.Is(err, ErrNotFound) {
				if token, getErr := s.loadToken(ctx, id); getErr == nil && token.UserID == userID {
					failure.Name = token.Name
				}
			}
			failed = append(failed, failure)
			continue
		}
		succeeded++
	}
	return succeeded, failed, nil
}

func batchFailureDetail(err error) string {
	var verr *ValidationError
	switch {
	case errors.Is(err, ErrNotFound):
		return "token not found"
	case errors.Is(err, ErrConflict):
		return "token was modified elsewhere"
	case errors.As(err, &verr):
		return verr.Message
	default:
		return err.Error()
	}
}

// ---------------------------------------------------------------------------
// 鉴权路径（Task 4 的 auth.TokenLookup）
// ---------------------------------------------------------------------------

type resolveRow struct {
	ID          int64
	UserID      int64
	Status      string
	Group       string
	ExpiresAt   *time.Time
	RemainQuota *int64
}

// Resolve 实现 auth.TokenLookup：入参是完整明文的 SHA-256 哈希。
//
// 返回口径（GC-11）：不存在/已删除/已重置 → ErrInvalidToken；禁用 →
// ErrTokenDisabled；过期 → ErrTokenExpired；额度耗尽仍返回 meta（自读用量
// 必须可用，modules-02 §2.13），由调用方决定 402/401。
func (s *Store) Resolve(ctx context.Context, keyHash string) (*auth.TokenMeta, error) {
	if keyHash == "" {
		return nil, ErrInvalidToken
	}
	now := s.now()
	if meta, ok := s.loadCachedMeta(ctx, keyHash); ok {
		return checkResolved(meta, now)
	}

	// 代际在读 DB 之前取：并发变更会让回填的 WATCH 失败（防 read-add 竞争）。
	gen, cacheable := s.cacheGeneration(ctx)
	var row resolveRow
	err := s.dbWith(ctx).Table("api_tokens").
		Select(`id, user_id, status, "group", expires_at, remain_quota`).
		Where("key_hash = ?", keyHash).
		Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrInvalidToken
	}
	if err != nil {
		// 存储故障 fail-closed：调用方必须拒绝，不能降级为匿名（GC-7）。
		return nil, fmt.Errorf("tokenstore: resolve: %w", err)
	}

	meta := &auth.TokenMeta{
		ID:             row.ID,
		UserID:         row.UserID,
		Status:         row.Status,
		Group:          row.Group,
		ExpiresAt:      row.ExpiresAt,
		UnlimitedQuota: row.RemainQuota == nil,
	}
	if row.RemainQuota != nil {
		meta.RemainQuota = *row.RemainQuota
	}
	if cacheable {
		s.storeCachedMeta(ctx, gen, keyHash, meta)
	}
	return checkResolved(meta, now)
}

func checkResolved(meta *auth.TokenMeta, now time.Time) (*auth.TokenMeta, error) {
	if meta.Status != StatusEnabled {
		return nil, ErrTokenDisabled
	}
	if meta.ExpiresAt != nil && !now.Before(*meta.ExpiresAt) {
		return nil, ErrTokenExpired
	}
	return cloneMeta(meta), nil
}

// ---------------------------------------------------------------------------
// Redis 缓存（GC-11）
// ---------------------------------------------------------------------------

const (
	cacheMetaPrefix    = "token:meta:"
	cacheViewPrefix    = "token:view:"
	cacheIDHashPrefix  = "token:idhash:"
	cacheGenerationKey = "token:gen"
	errCacheGeneration = "tokenstore: cache generation changed"
)

var errCacheGenerationChanged = errors.New(errCacheGeneration)

func cacheMetaKey(hash string) string { return cacheMetaPrefix + hash }

func cacheViewKey(id int64) string {
	return cacheViewPrefix + strconv.FormatInt(id, 10)
}

func cacheIDHashKey(id int64) string {
	return cacheIDHashPrefix + strconv.FormatInt(id, 10)
}

// cacheGeneration 读取全局代际计数。ok=false 表示 Redis 不可用（跳过缓存，
// 直接以 DB 为准），绝不影响鉴权结论。
func (s *Store) cacheGeneration(ctx context.Context) (string, bool) {
	if s.rdb == nil {
		return "", false
	}
	value, err := s.rdb.Get(ctx, cacheGenerationKey).Result()
	if errors.Is(err, redis.Nil) {
		return "0", true
	}
	if err != nil {
		return "", false
	}
	return value, true
}

func (s *Store) loadCachedMeta(ctx context.Context, keyHash string) (*auth.TokenMeta, bool) {
	if s.rdb == nil {
		return nil, false
	}
	raw, err := s.rdb.Get(ctx, cacheMetaKey(keyHash)).Bytes()
	if err != nil {
		return nil, false
	}
	var meta auth.TokenMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil, false
	}
	return &meta, true
}

func (s *Store) storeCachedMeta(ctx context.Context, gen, keyHash string, meta *auth.TokenMeta) {
	raw, err := json.Marshal(meta)
	if err != nil {
		return
	}
	s.writeCacheGuarded(ctx, gen, map[string]any{
		cacheMetaKey(keyHash):   raw,
		cacheIDHashKey(meta.ID): keyHash,
	})
}

func (s *Store) loadCachedToken(ctx context.Context, id int64) (*Token, bool) {
	if s.rdb == nil {
		return nil, false
	}
	raw, err := s.rdb.Get(ctx, cacheViewKey(id)).Bytes()
	if err != nil {
		return nil, false
	}
	var token Token
	if err := json.Unmarshal(raw, &token); err != nil {
		return nil, false
	}
	return cloneToken(&token), true
}

func (s *Store) storeCachedToken(ctx context.Context, gen string, token *Token, keyHash string) {
	raw, err := json.Marshal(token)
	if err != nil {
		return
	}
	s.writeCacheGuarded(ctx, gen, map[string]any{
		cacheViewKey(token.ID):   raw,
		cacheIDHashKey(token.ID): keyHash,
	})
}

// writeCacheGuarded 用 WATCH token:gen 实现「代际不变的才回填」：
// 读取 DB 期间只要发生任何令牌变更，缓存写入就放弃，避免把并发删除/禁用的
// 旧行重新写回并存活 60s。
func (s *Store) writeCacheGuarded(ctx context.Context, gen string, entries map[string]any) {
	if s.rdb == nil || gen == "" {
		return
	}
	err := s.rdb.Watch(ctx, func(tx *redis.Tx) error {
		current, err := tx.Get(ctx, cacheGenerationKey).Result()
		switch {
		case errors.Is(err, redis.Nil):
			current = "0"
		case err != nil:
			return err
		}
		if current != gen {
			return errCacheGenerationChanged
		}
		_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			for key, value := range entries {
				pipe.Set(ctx, key, value, s.ttl)
			}
			return nil
		})
		return err
	}, cacheGenerationKey)
	if err != nil && !errors.Is(err, errCacheGenerationChanged) {
		// 回填失败只影响性能；下一请求会再读 DB。
		slog.Debug("tokenstore: cache fill failed", "error", err)
	}
}

// ---------------------------------------------------------------------------
// 失效：DEL + 代际自增 + pub/sub（GC-11）
// ---------------------------------------------------------------------------

// Subscribe 订阅 token:changed:* 并清理 Redis 缓存键；ctx 结束即停止。
// 由于缓存本身在 Redis 里，写路径的 DEL 已全局生效；订阅是 GC-11 要求的
// 第二道失效通道（兼容外部只发广播/写路径 DEL 失败的情形）。
func (s *Store) Subscribe(ctx context.Context) error {
	if s.rdb == nil {
		return nil
	}
	pubsub := s.rdb.PSubscribe(ctx, "token:changed:*")
	if _, err := pubsub.Receive(ctx); err != nil {
		_ = pubsub.Close()
		return fmt.Errorf("tokenstore: subscribe token:changed: %w", err)
	}
	go s.watch(ctx, pubsub)
	return nil
}

func (s *Store) watch(ctx context.Context, pubsub *redis.PubSub) {
	// 退出路径：关闭失败已无补救动作。
	defer func() { _ = pubsub.Close() }()
	messages := pubsub.Channel()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-messages:
			if !ok {
				return
			}
			id, err := strconv.ParseInt(strings.TrimSpace(msg.Payload), 10, 64)
			if err != nil {
				// 兼容只发频道名不写 payload 的实现：从 token:changed:<id> 取后缀。
				suffix := msg.Channel[strings.LastIndex(msg.Channel, ":")+1:]
				id, err = strconv.ParseInt(suffix, 10, 64)
				if err != nil {
					continue
				}
			}
			s.invalidateOnChange(ctx, id)
		}
	}
}

// invalidateOnChange 清理某令牌的全部缓存键（view/idhash/meta）。
func (s *Store) invalidateOnChange(ctx context.Context, id int64) {
	if s.rdb == nil {
		return
	}
	idHashKey := cacheIDHashKey(id)
	keys := []string{cacheViewKey(id), idHashKey}
	if hash, err := s.rdb.Get(ctx, idHashKey).Result(); err == nil && hash != "" {
		keys = append(keys, cacheMetaKey(hash))
	}
	_ = s.rdb.Del(ctx, keys...).Err()
}

// invalidateToken 是写路径的统一失效入口：代际自增（阻断在途回填）→
// DEL 已知键（含旧 hash 的 meta）→ 广播 token:changed:<id>。
func (s *Store) invalidateToken(ctx context.Context, id int64, keyHashes ...string) {
	if s.rdb == nil {
		return
	}
	if err := s.rdb.Incr(ctx, cacheGenerationKey).Err(); err != nil {
		slog.Warn("tokenstore: bump cache generation failed", "token_id", id, "error", err)
	}
	keys := []string{cacheViewKey(id), cacheIDHashKey(id)}
	// 反向索引可能记录着比调用方 row.KeyHash 更新的密钥（并发 reset 后刚被
	// Resolve 缓存），一并删除，避免旧响应留下可鉴权的 meta。
	if hash, err := s.rdb.Get(ctx, cacheIDHashKey(id)).Result(); err == nil && hash != "" {
		keys = append(keys, cacheMetaKey(hash))
	}
	for _, hash := range keyHashes {
		if hash != "" {
			keys = append(keys, cacheMetaKey(hash))
		}
	}
	if err := s.rdb.Del(ctx, keys...).Err(); err != nil {
		slog.Warn("tokenstore: cache invalidation failed", "token_id", id, "error", err)
	}
	s.publishChange(ctx, id)
}

// publishChange 广播 token:changed:<id>。pub/sub 失败不影响写操作：
// DB 是权威，其他副本最迟在 60s TTL 后自愈（GC-11 兜底）。
func (s *Store) publishChange(ctx context.Context, id int64) {
	if s.rdb == nil {
		return
	}
	if err := s.rdb.Publish(ctx, fmt.Sprintf("token:changed:%d", id), strconv.FormatInt(id, 10)).Err(); err != nil {
		// 主动失效失败只影响跨副本的 ≤1s 目标；60s TTL 兜底仍然成立。
		slog.Warn("tokenstore: publish token change failed", "token_id", id, "error", err)
	}
}

func cloneToken(t *Token) *Token {
	if t == nil {
		return nil
	}
	clone := *t
	clone.ModelLimits = append([]string(nil), t.ModelLimits...)
	clone.AllowIPs = append([]string(nil), t.AllowIPs...)
	clone.AutoGroups = append([]string(nil), t.AutoGroups...)
	if t.RemainQuota != nil {
		quota := *t.RemainQuota
		clone.RemainQuota = &quota
	}
	if t.ExpiresAt != nil {
		expires := *t.ExpiresAt
		clone.ExpiresAt = &expires
	}
	if t.LastUsedAt != nil {
		lastUsed := *t.LastUsedAt
		clone.LastUsedAt = &lastUsed
	}
	return &clone
}

func cloneMeta(meta *auth.TokenMeta) *auth.TokenMeta {
	if meta == nil {
		return nil
	}
	clone := *meta
	if meta.ExpiresAt != nil {
		expires := *meta.ExpiresAt
		clone.ExpiresAt = &expires
	}
	return &clone
}

func sameInstant(a, b time.Time) bool {
	return a.Truncate(time.Microsecond).Equal(b.Truncate(time.Microsecond))
}

// escapeLike 转义 ILIKE 的通配符，避免用户输入被当成模式。
func escapeLike(s string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return replacer.Replace(s)
}
