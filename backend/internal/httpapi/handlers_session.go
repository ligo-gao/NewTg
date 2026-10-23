// Package httpapi 的个人会话与自助资料端点（Task 9）。
//
// 端点面（modules-01 F-01-13/14/18/19/27/29/31、modules-08 §3.18）：
//
//	GET    /api/sessions                  ≤50 条，按 last_seen 倒序，标记当前会话
//	DELETE /api/sessions/{sid}            踢单个：他人/未知 sid → 404（不泄露存在性），
//	                                      当前 sid → 400（应使用登出）
//	POST   /api/sessions/revoke-others    吊销除当前会话外的全部本人会话
//	GET    /api/me                        本人资料投影（零敏感字段）
//	PATCH  /api/me                        仅 display_name；其它字段 403 field_not_writable
//	GET    /api/self                      语言 / 侧栏稀疏覆盖 / IP 记录开关 + updated_at
//	PUT    /api/self/setting              乐观锁写入（updated_at 不匹配 → 409）
//	POST   /api/verify-email              {email_code} → 置 email_verified_at
//
// 路由挂载（并行任务负责 routes.go）：
//
//	h, err := NewSessionHandlers(SessionHandlerConfig{
//	    DB: cfg.DB, Repo: nil, Sessions: cfg.Sessions, Authz: cfg.Authz,
//	    Redis: cfg.Redis, SessionSecret: cfg.SessionSecret, Logger: cfg.Logger,
//	})
//	// 方式 A：注册到已挂好鉴权链的受保护子 mux（Router.protected 形态）
//	h.RegisterSessionRoutes(rt.protected, nil)
//	// 方式 B：注册到裸 mux，由 wrap 注入 SessionAuth（或完整 Protect 链）
//	h.RegisterSessionRoutes(rt.mux, Protect(cfg.Sessions, cfg.TokenStore, cfg.PublicBaseURL))
//
// 所有 handler 都假定 request.Context 里已有 auth.Principal；缺失时一律 401
// （fail-closed，绝不把零值身份当匿名放行）。wrap 绝不能把本组端点暴露到
// 未认证路径上——会话列表/踢出是凭证平面操作。
//
// 凭证平面（security-01 I-13 / F-01-27 TC-F-01-27-05）：会话管理端点只接受
// 浏览器会话 Cookie；API 令牌（Bearer）一律 403。令牌是中继平面凭证，
// 若能读取或吊销控制台会话，就等于用一个可长期存储的密钥接管设备管理面。
package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/rbac"
	"newtg/gateway/internal/store"
)

// RFC 7807 type URI。not_found 与 rbac 层的 404 同形：越权 sid 与真实不存在
// 必须逐字相同，否则状态码/文案差异会成为会话存在性信道。
const (
	problemTypeNotFoundSelf = "https://newtg.dev/problems/not_found"

	// problemTypeDefaultModelInvalid 是 modules-02 §2.14 冻结的 422：
	// 默认模型不属于该用户的可用分组。title 用点分码 `user.default_model_invalid`
	// 保留契约里的机器可读字面量（RFC 7807 的 type 必须是 URI，不能直接放点分码）。
	problemTypeDefaultModelInvalid = "https://newtg.dev/problems/default_model_invalid"
	defaultModelInvalidTitle       = "user.default_model_invalid"

	// maxDefaultModelRunes 与迁移的 users_default_model_len_chk（≤128）一致，
	// 也与 model_pricing.model varchar(128) 对齐：超过该长度的名字不可能存在于
	// 可用模型清单中，提前拒绝可给出稳定的 422 而不是 DB CHECK 500。
	maxDefaultModelRunes = 128

	// maxDisplayNameRunes 与 users.display_name 的 CHECK（≤32 字符）一致。
	maxDisplayNameRunes = 32
	// maxSidebarModuleEntries/maxSidebarKeyRunes 是稀疏覆盖的防滥用上限：
	// 模块键来自站点配置的有限集合，正常用户远低于该值。
	maxSidebarModuleEntries = 64
	maxSidebarKeyRunes      = 64
)

// 自助资料/设置的存储错误哨兵。
var (
	// ErrSelfNotFound 表示用户行不存在（账号被硬删除后的残留会话）。
	ErrSelfNotFound = errors.New("httpapi: user not found")
	// ErrSelfConflict 表示乐观锁不匹配：调用方持有的 updated_at 已过期。
	ErrSelfConflict = errors.New("httpapi: self settings updated concurrently")
)

// SelfRecord 是 users 行中自助资料与设置相关列的投影。
//
// SidebarModules 保留 jsonb 原始字节：稀疏覆盖的键集合由站点模块目录决定，
// 服务端不做白名单（站点层可见性在读取侧合并，modules-08 §3.18 Q4）。
type SelfRecord struct {
	Username        string
	DisplayName     *string
	Email           *string
	Role            string
	Status          string
	AvatarURL       *string
	Language        string
	SidebarModules  json.RawMessage
	RecordIPLog     bool
	EmailVerifiedAt *time.Time
	// DefaultModel 是用户默认模型（modules-02 §2.14）；NULL = 未设置。
	// 空串不入库（迁移 CHECK 1..128），因此指针 nil 是唯一的「未设置」表示。
	DefaultModel *string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// SelfPatch 是 PUT /api/self/setting 的部分更新。nil 表示「本次不改该键」；
// SidebarModules 非 nil 且内容为 null 时表示清空覆盖（回到继承默认）。
type SelfPatch struct {
	Language       *string
	SidebarModules *json.RawMessage
	RecordIPLog    *bool
	// DefaultModel 为 nil 表示本次不改；非 nil 时指向的值写入（空串表示清除，
	// 归一为 SQL NULL）。三值语义与 language 不同是有意的：默认模型需要
	// 「显式清除」这一状态，而 language 永远有 canonical 值。
	DefaultModel *string
}

// SelfRepo 是自助端点对 users 表的最小依赖面。单测注入内存假实现；
// 生产实现是 SQLSelfRepo。
type SelfRepo interface {
	GetSelf(ctx context.Context, userID int64) (SelfRecord, error)
	UpdateDisplayName(ctx context.Context, userID int64, displayName *string) (SelfRecord, error)
	UpdateSettings(ctx context.Context, userID int64, patch SelfPatch, expectUpdatedAt time.Time) (SelfRecord, error)
	// UsableGroupsForUser 是 default_model 校验的唯一数据源（可用分组 ×
	// 已定价且有启用渠道的模型）。与 GET /api/status 的 usable_groups 同口径。
	UsableGroupsForUser(ctx context.Context, userID int64) ([]store.UsableGroup, error)
	// MarkEmailVerified 置首次验证时间；signupBonus > 0 时在同一事务内幂等
	// 补发注册赠送（reference_type=signup_grant），插入冲突即视为已发放。
	// granted 报告本次是否真的插入了赠送流水（false = 已发放/无赠送）。
	MarkEmailVerified(ctx context.Context, userID int64, signupBonus int64) (time.Time, bool, error)
}

// selfColumns 是 SelfRecord 的 SELECT/RETURNING 列序，供 scanSelfRecord 消费。
const selfColumns = `username, display_name, email, role, status, avatar_url,
	language, sidebar_modules, record_ip_log, email_verified_at, default_model, created_at, updated_at`

type rowScanner interface {
	Scan(dest ...any) error
}

// scanSelfRecord 统一扫描投影。jsonb 先落 []byte 再拷贝进 RawMessage：
// database/sql 对命名 []byte 类型的直接扫描行为依赖驱动实现，显式转换更稳。
func scanSelfRecord(row rowScanner) (SelfRecord, error) {
	var (
		rec     SelfRecord
		sidebar []byte
	)
	err := row.Scan(&rec.Username, &rec.DisplayName, &rec.Email, &rec.Role, &rec.Status,
		&rec.AvatarURL, &rec.Language, &sidebar, &rec.RecordIPLog,
		&rec.EmailVerifiedAt, &rec.DefaultModel, &rec.CreatedAt, &rec.UpdatedAt)
	if err != nil {
		return SelfRecord{}, err
	}
	if sidebar != nil {
		rec.SidebarModules = append(json.RawMessage(nil), sidebar...)
	}
	return rec, nil
}

// SQLSelfRepo 是 SelfRepo 的 PostgreSQL 实现。
type SQLSelfRepo struct {
	db     *sql.DB
	groups *store.UserGroupsRepo
}

// NewSQLSelfRepo 构造自助资料仓储。
func NewSQLSelfRepo(db *sql.DB) *SQLSelfRepo {
	return &SQLSelfRepo{db: db, groups: store.NewUserGroupsRepo(db)}
}

// UsableGroupsForUser 委托给 store 层：默认模型校验与 /api/status 的
// usable_groups 必须共用同一 SQL 口径（Task 13 brief §2 冻结的形状），
// 否则「页面能选的模型」与「保存时接受的模型」会出现两套答案。
func (r *SQLSelfRepo) UsableGroupsForUser(ctx context.Context, userID int64) ([]store.UsableGroup, error) {
	return r.groups.UsableGroupsForUser(ctx, userID)
}

// GetSelf 读取本人资料行。用户被硬删除后返回 ErrSelfNotFound。
func (r *SQLSelfRepo) GetSelf(ctx context.Context, userID int64) (SelfRecord, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+selfColumns+` FROM users WHERE id = $1`, userID)
	rec, err := scanSelfRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return SelfRecord{}, ErrSelfNotFound
	}
	if err != nil {
		return SelfRecord{}, fmt.Errorf("httpapi: get self: %w", err)
	}
	return rec, nil
}

// UpdateDisplayName 写入 display_name（nil=NULL）。updated_at 同步前进，
// 让未完成的 /api/self/setting 乐观锁令牌随资料变更一起失效——同一行版本。
func (r *SQLSelfRepo) UpdateDisplayName(ctx context.Context, userID int64, displayName *string) (SelfRecord, error) {
	row := r.db.QueryRowContext(ctx,
		`UPDATE users SET display_name = $1, updated_at = now()
		 WHERE id = $2
		 RETURNING `+selfColumns, displayName, userID)
	rec, err := scanSelfRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return SelfRecord{}, ErrSelfNotFound
	}
	if err != nil {
		return SelfRecord{}, fmt.Errorf("httpapi: update display name: %w", err)
	}
	return rec, nil
}

// UpdateSettings 用「WHERE updated_at = 读取时版本」实现乐观并发（modules-08
// §1.3 的语义；这里是 users 行而不是 system_settings 行）。
//
// 部分更新：nil 字段保持原值。sidebar_modules 传 JSON null 时写 SQL NULL
// （回到继承态），否则整体替换为提交的稀疏对象。
func (r *SQLSelfRepo) UpdateSettings(ctx context.Context, userID int64, patch SelfPatch, expectUpdatedAt time.Time) (SelfRecord, error) {
	setSidebar := patch.SidebarModules != nil
	sidebar := "null"
	if setSidebar {
		sidebar = string(*patch.SidebarModules)
	}
	// default_model 的三值语义：nil = 不改；指向空串 = 清除（写 NULL）；
	// 其它 = 写入该模型名。空串在这里归一为 NULL，与迁移的「空串不入库」CHECK
	// 保持一致（读到 NULL 的 UI 才等价于「未设置」）。
	setDefault := patch.DefaultModel != nil
	var defaultModel *string
	if setDefault && *patch.DefaultModel != "" {
		defaultModel = patch.DefaultModel
	}
	row := r.db.QueryRowContext(ctx, `
		UPDATE users SET
			language = COALESCE($1, language),
			sidebar_modules = CASE WHEN $2 THEN NULLIF($3::jsonb, 'null'::jsonb)
			                       ELSE sidebar_modules END,
			record_ip_log = COALESCE($4, record_ip_log),
			default_model = CASE WHEN $5 THEN $6 ELSE default_model END,
			updated_at = now()
		WHERE id = $7 AND updated_at = $8
		RETURNING `+selfColumns,
		patch.Language, setSidebar, sidebar, patch.RecordIPLog,
		setDefault, defaultModel, userID, expectUpdatedAt.UTC())
	rec, err := scanSelfRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		// 无法区分「版本冲突」与「用户行已被删除」时不能猜：查一次存在性，
		// 让已删除账号得到 404 而不是误导性的 409。
		var exists bool
		if qerr := r.db.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM users WHERE id = $1)`, userID).Scan(&exists); qerr != nil {
			return SelfRecord{}, fmt.Errorf("httpapi: check user existence: %w", qerr)
		}
		if !exists {
			return SelfRecord{}, ErrSelfNotFound
		}
		return SelfRecord{}, ErrSelfConflict
	}
	if err != nil {
		return SelfRecord{}, fmt.Errorf("httpapi: update self settings: %w", err)
	}
	return rec, nil
}

// MarkEmailVerified 幂等置 email_verified_at（已存在则保持首次验证时间），并在
// signupBonus > 0 时于同一事务内补发注册赠送（modules-08 F-08-10 / modules-03
// F-03-11：requires_verified_email 开启时注册不发放，首次验证补发一次）。
//
// 为什么用 INSERT … ON CONFLICT DO NOTHING：并发验证会同时通过 handler 的
// 「已验证短路」，若先查后写会各自看到 0 行并重复加余额。部分唯一索引
// uq_wallet_tx_signup_grant 让恰有一个事务插入流水，其余得到 0 行且不加余额——
// 幂等由数据库裁决。插入 0 行不代表验证失败：邮箱照常置为已验证。
func (r *SQLSelfRepo) MarkEmailVerified(ctx context.Context, userID int64, signupBonus int64) (time.Time, bool, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("httpapi: begin mark email verified: %w", err)
	}
	// 主错误优先：成功路径显式 Commit；失败路径 Rollback 的错误无需覆盖主错误。
	defer func() { _ = tx.Rollback() }()

	// 行锁串行化同一用户的并发补发：第二个事务在第一个提交后看到已 +bonus
	// 的余额与已存在的赠送流水。
	var balance int64
	err = tx.QueryRowContext(ctx, `SELECT balance FROM users WHERE id = $1 FOR UPDATE`, userID).
		Scan(&balance)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, ErrSelfNotFound
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("httpapi: lock user for email verification: %w", err)
	}

	if signupBonus > 0 {
		// reference_id 与注册发放同口径（用户 id 字符串），因此同一用户只会
		// 有一行 signup_grant 流水；注册时已发放过的账号在这里是 no-op。
		res, err := tx.ExecContext(ctx, `
			INSERT INTO wallet_transactions
				(user_id, type, amount, balance_after, status, reference_type, reference_id)
			VALUES ($1, 'bonus', $2, $3, 'confirmed', 'signup_grant', $4)
			ON CONFLICT (reference_id) WHERE reference_type = 'signup_grant' DO NOTHING`,
			userID, signupBonus, balance+signupBonus, strconv.FormatInt(userID, 10))
		if err != nil {
			return time.Time{}, false, fmt.Errorf("httpapi: grant signup bonus on verification: %w", err)
		}
		inserted, err := res.RowsAffected()
		if err != nil {
			return time.Time{}, false, fmt.Errorf("httpapi: signup bonus rows affected: %w", err)
		}
		if inserted == 1 {
			var verifiedAt time.Time
			if err := tx.QueryRowContext(ctx, `
				UPDATE users SET balance = balance + $2,
				                 email_verified_at = COALESCE(email_verified_at, now()),
				                 updated_at = now()
				WHERE id = $1
				RETURNING email_verified_at`, userID, signupBonus).Scan(&verifiedAt); err != nil {
				return time.Time{}, false, fmt.Errorf("httpapi: apply signup bonus: %w", err)
			}
			if err := tx.Commit(); err != nil {
				return time.Time{}, false, fmt.Errorf("httpapi: commit email verification: %w", err)
			}
			return verifiedAt, true, nil
		}
	}

	var verifiedAt time.Time
	if err := tx.QueryRowContext(ctx, `
		UPDATE users SET email_verified_at = COALESCE(email_verified_at, now()),
		                 updated_at = now()
		WHERE id = $1
		RETURNING email_verified_at`, userID).Scan(&verifiedAt); err != nil {
		return time.Time{}, false, fmt.Errorf("httpapi: mark email verified: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return time.Time{}, false, fmt.Errorf("httpapi: commit email verification: %w", err)
	}
	return verifiedAt, false, nil
}

// ---------------------------------------------------------------------------
// 端点装配
// ---------------------------------------------------------------------------

// SessionHandlerConfig 是自助端点依赖。Repo 与 DB 二选一（Repo 优先，
// 供单测注入）；Sessions 必填；Redis 用于邮箱验证码消费（nil 时该端点 503）。
type SessionHandlerConfig struct {
	DB            *sql.DB
	Repo          SelfRepo
	Sessions      *auth.SessionManager
	Authz         *rbac.AuthzMiddleware
	Redis         *redis.Client
	SessionSecret []byte
	// Config 是配置中心读取面：首次邮箱验证时按当期配置决定是否补发注册
	// 赠送（发放时点求值，不追溯注册时刻）。nil 时按默认值（不补发）。
	Config ConfigReader
	// Access 是 GET /api/me permissions 的权限回源接缝（M6 R-17 扩展）。
	// 装配层应传入与 Authz 相同的 PermissionLoader；nil 时由 DB 构造
	// SQLAuthRepo。两者都不可用时按 fail-closed 返回空集。
	Access rbac.PermissionLoader
	Logger *slog.Logger
}

// SessionHandlers 持有 Task 9 端点的依赖，可并发使用。
type SessionHandlers struct {
	repo     SelfRepo
	sessions *auth.SessionManager
	authz    *rbac.AuthzMiddleware
	access   rbac.PermissionLoader
	rdb      *redis.Client
	codes    *emailCodeStore
	config   ConfigReader
	log      *slog.Logger
}

// 字段级（BOPLA）动作登记。自服务动作不要求权限键（ActionName=""），但
// 仍然强制可写字段白名单：未知/只读字段 403 field_not_writable，绝不静默丢弃
// （security-02 §2.6.1、controller ruling 3）。
var (
	mePatchAction = rbac.AuthzAction{ResourceKind: "me"}
	selfSetAction = rbac.AuthzAction{ResourceKind: "self_setting"}
)

// NewSessionHandlers 构造自助端点。Authz 为 nil 时自建一个仅含本组字段策略的
// 实例：字段层是端点契约的一部分，不能因为装配方没启用管理面 RBAC 而失效。
func NewSessionHandlers(cfg SessionHandlerConfig) (*SessionHandlers, error) {
	if cfg.Sessions == nil {
		return nil, errors.New("httpapi: SessionHandlerConfig.Sessions is required")
	}
	repo := cfg.Repo
	if repo == nil {
		if cfg.DB == nil {
			return nil, errors.New("httpapi: SessionHandlerConfig.Repo or DB is required")
		}
		repo = NewSQLSelfRepo(cfg.DB)
	}
	authz := cfg.Authz
	if authz == nil {
		authz = rbac.NewAuthzMiddleware()
	}
	authz.SetWritePolicy(mePatchAction, &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{
		"display_name": nil,
	}})
	authz.SetWritePolicy(selfSetAction, &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{
		"language":        nil,
		"sidebar_modules": nil, // 结构由 handler 校验（必须是 object<string,bool>）
		"record_ip_log":   nil,
		"default_model":   nil, // 值域（可用分组模型）由 handler 校验
		"updated_at":      nil, // 乐观锁令牌，不是落库列
	}})

	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	access := cfg.Access
	if access == nil && cfg.DB != nil {
		// SQLAuthRepo.Load 与 Role/权限列口径一致（role=user → []，
		// admin SQL NULL → nil=FULL，["*"] 原样）。
		access = NewSQLAuthRepo(cfg.DB)
	}
	return &SessionHandlers{
		repo:     repo,
		sessions: cfg.Sessions,
		authz:    authz,
		access:   access,
		rdb:      cfg.Redis,
		codes:    newEmailCodeStore(cfg.Redis, cfg.SessionSecret),
		config:   cfg.Config,
		log:      log,
	}, nil
}

// RegisterSessionRoutes 注册本任务全部端点。
//
// wrap 为 nil 时假定 mux 已经在 SessionAuth（及 CSRF/Origin）之后；非 nil 时
// 每个端点都会被 wrap 包裹（推荐传 Protect(...) 或至少 SessionAuth）。
//
// 路由模式使用 Go 1.22 方法+通配语法；DELETE /api/sessions/{sid} 与
// POST /api/sessions/revoke-others 方法不同，不会互相遮蔽。
func (h *SessionHandlers) RegisterSessionRoutes(mux *http.ServeMux, wrap func(http.Handler) http.Handler) {
	mount := func(pattern string, handler http.Handler) {
		if wrap != nil {
			handler = wrap(handler)
		}
		mux.Handle(pattern, handler)
	}
	// 字段层必须位于业务 handler 之前：它读取并校验请求体，然后还原 Body。
	fieldGuarded := func(action rbac.AuthzAction, next http.Handler) http.Handler {
		return h.authz.Handler("", action)(next)
	}

	mount("GET /api/sessions", http.HandlerFunc(h.handleListSessions))
	mount("DELETE /api/sessions/{sid}", http.HandlerFunc(h.handleDeleteSession))
	mount("POST /api/sessions/revoke-others", http.HandlerFunc(h.handleRevokeOthers))

	mount("GET /api/me", http.HandlerFunc(h.handleGetMe))
	mount("PATCH /api/me", fieldGuarded(mePatchAction, http.HandlerFunc(h.handlePatchMe)))

	mount("GET /api/self", http.HandlerFunc(h.handleGetSelf))
	mount("PUT /api/self/setting", fieldGuarded(selfSetAction, http.HandlerFunc(h.handlePutSelfSetting)))

	mount("POST /api/verify-email", http.HandlerFunc(h.handleVerifyEmail))
}

// ---------------------------------------------------------------------------
// 视图
// ---------------------------------------------------------------------------

// meView 是 GET /api/me 的冻结投影：用户名/邮箱/邮箱验证状态/角色/状态/
// 头像/显示名/加入时间/生效权限。任何口令/TOTP/备用码/哈希列都不得出现
// （GC-6）。permissions 是 M6 R-17 的契约扩展：前端据此隐藏 admin-only
// 控件，服务端仍是最终权威。
type meView struct {
	Username      string   `json:"username"`
	Email         *string  `json:"email"`
	EmailVerified bool     `json:"email_verified"`
	Role          string   `json:"role"`
	Status        string   `json:"status"`
	AvatarURL     *string  `json:"avatar_url"`
	DisplayName   *string  `json:"display_name"`
	CreatedAt     string   `json:"created_at"`
	Permissions   []string `json:"permissions"`
}

func newMeView(rec SelfRecord, permissions []string) meView {
	if permissions == nil {
		// JSON null 会让前端把「无权限」与「未知」混在一起；空切片序列化为 []。
		permissions = []string{}
	}
	verified := rec.EmailVerifiedAt != nil
	createdAt := ""
	if !rec.CreatedAt.IsZero() {
		createdAt = rec.CreatedAt.UTC().Format(time.RFC3339)
	}
	return meView{
		Username:      rec.Username,
		Email:         rec.Email,
		EmailVerified: verified,
		Role:          rec.Role,
		Status:        rec.Status,
		AvatarURL:     rec.AvatarURL,
		DisplayName:   rec.DisplayName,
		CreatedAt:     createdAt,
		Permissions:   permissions,
	}
}

// effectivePermissions 把 admin_permissions 归一为调用者的生效权限键：
//   - role != admin（或 status != enabled）→ []（绝不泄露残留列）；
//   - SQL NULL / ["*"] → ["*"]（FULL）；
//   - 显式集合 → 排序后的键集合（空数组仍是 []，不是 FULL）。
//
// 回源失败或接缝缺失时返回 []（fail-closed）：/api/me 是会话探测路径，
// 不能因一次权限读取故障把整个控制台变成 500；代价只是 admin-only 控件
// 暂时隐藏，服务端每个请求仍会独立鉴权。
func (h *SessionHandlers) effectivePermissions(
	ctx context.Context,
	userID int64,
	role string,
	status string,
) []string {
	if role != rbac.RoleAdmin || status != "enabled" {
		return []string{}
	}
	if h.access == nil {
		return []string{}
	}
	permissions, err := h.access.Load(ctx, userID)
	if err != nil {
		// 已删除用户/存储故障都不能伪装成 FULL。
		h.log.Warn("me: load permissions failed", "user_id", userID, "error", err)
		return []string{}
	}
	if permissions == nil || slices.Contains(permissions, "*") {
		return []string{"*"}
	}
	result := append([]string(nil), permissions...)
	slices.Sort(result)
	return result
}

// sessionView 是会话列表的一行。UA/IP/时间为空时仍返回零值，前端展示「未知」。
// ID 是删除该会话的句柄（F-01-27 的 :sid），只下发给会话本人。
type sessionView struct {
	ID         string    `json:"id"`
	UserAgent  string    `json:"user_agent"`
	IP         string    `json:"ip"`
	CreatedAt  time.Time `json:"created_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
	Current    bool      `json:"current"`
}

// sessionListView 不是分页信封：列表固定 ≤50 条、无翻页语义（GC-6 只对
// 分页列表要求 Page）。
type sessionListView struct {
	Sessions []sessionView `json:"sessions"`
}

// selfView 是 GET /api/self / PUT /api/self/setting 的响应。updated_at 是
// 下一次写入必须携带的乐观锁令牌。sidebar_modules 为 null 表示无覆盖。
type selfView struct {
	Language       string          `json:"language"`
	SidebarModules json.RawMessage `json:"sidebar_modules"`
	RecordIPLog    bool            `json:"record_ip_log"`
	// DefaultModel 为 null 表示未设置（Playground 回落到首个可用模型）。
	DefaultModel *string   `json:"default_model"`
	UpdatedAt    time.Time `json:"updated_at"`
}

func newSelfView(rec SelfRecord) selfView {
	sidebar := rec.SidebarModules
	if sidebar == nil {
		sidebar = json.RawMessage("null")
	}
	return selfView{
		Language:       rec.Language,
		SidebarModules: sidebar,
		RecordIPLog:    rec.RecordIPLog,
		DefaultModel:   rec.DefaultModel,
		UpdatedAt:      rec.UpdatedAt,
	}
}

// ---------------------------------------------------------------------------
// 会话端点
// ---------------------------------------------------------------------------

// requireSession 取当前身份。sessionOnly=true 的端点拒绝 Bearer 令牌。
func (h *SessionHandlers) requireSession(w http.ResponseWriter, r *http.Request, sessionOnly bool) (*auth.Principal, string, bool) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok || p == nil || p.UserID == 0 {
		writeUnauthorized(w)
		return nil, "", false
	}
	sid, hasCookie := sessionID(r)
	if sessionOnly && (p.TokenID != nil || !hasCookie) {
		// 统一 403：不区分「令牌有效但无权」与「缺 Cookie」，避免凭证平面探测。
		writeProblem(w, http.StatusForbidden, authProblemForbidden,
			"This endpoint requires a browser session")
		return nil, "", false
	}
	return p, sid, true
}

func (h *SessionHandlers) handleListSessions(w http.ResponseWriter, r *http.Request) {
	p, currentSID, ok := h.requireSession(w, r, true)
	if !ok {
		return
	}
	infos, err := h.sessions.ListSessions(r.Context(), p.UserID)
	if err != nil {
		h.writeUnavailable(w, "list sessions", err)
		return
	}
	views := make([]sessionView, 0, len(infos))
	for _, info := range infos {
		views = append(views, sessionView{
			ID:         info.SID,
			UserAgent:  info.UA,
			IP:         info.IP,
			CreatedAt:  info.CreatedAt,
			LastSeenAt: info.LastSeenAt,
			Current:    info.SID == currentSID,
		})
	}
	writeJSON(w, http.StatusOK, sessionListView{Sessions: views})
}

func (h *SessionHandlers) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	p, currentSID, ok := h.requireSession(w, r, true)
	if !ok {
		return
	}
	target := r.PathValue("sid")
	if target == "" {
		writeProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found")
		return
	}
	// 当前会话必须走登出（清 Cookie + CSRF），否则浏览器会保留一个已失效的
	// 会话 Cookie，下一次请求才 401，体验上像「踢了自己却还显示在线」。
	if target == currentSID {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest,
			"The current session cannot be revoked here; use logout instead")
		return
	}

	sess, err := h.sessions.Get(r.Context(), target)
	if errors.Is(err, auth.ErrSessionNotFound) {
		// 未知 sid 与他人 sid 必须同形：存在性不可枚举（I-12）。
		writeProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found")
		return
	}
	if err != nil {
		h.writeUnavailable(w, "read target session", err)
		return
	}
	if sess.UserID != p.UserID {
		writeProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found")
		return
	}
	if err := h.sessions.Revoke(r.Context(), target); err != nil {
		h.writeUnavailable(w, "revoke session", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *SessionHandlers) handleRevokeOthers(w http.ResponseWriter, r *http.Request) {
	p, currentSID, ok := h.requireSession(w, r, true)
	if !ok {
		return
	}
	if err := h.sessions.RevokeUser(r.Context(), p.UserID, currentSID); err != nil {
		h.writeUnavailable(w, "revoke other sessions", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// /api/me
// ---------------------------------------------------------------------------

func (h *SessionHandlers) handleGetMe(w http.ResponseWriter, r *http.Request) {
	p, sid, ok := h.requireSession(w, r, false)
	if !ok {
		return
	}
	// security-01 §2.2 冻结的恢复路径：Cookie 会话存在但 CSRF Cookie 丢失
	// （浏览器清理/跨设备恢复）时，GET /api/me 补发一次；已有则绝不轮换，
	// 否则每个页面加载都会让进行中的写请求拿到旧令牌而 403。
	if sid != "" {
		if cookie, err := r.Cookie(auth.CSRFCookieName); errors.Is(err, http.ErrNoCookie) ||
			(err == nil && strings.TrimSpace(cookie.Value) == "") {
			token := auth.NewCSRFToken()
			if err := h.sessions.IssueCSRF(r.Context(), sid, token); err != nil {
				// 会话可能在身份校验与签发之间被吊销；补发失败不应把一次自读
				// 变成 5xx——前端下一次写请求会按正常 401/403 流程处理。
				h.log.Warn("reissue csrf token on me failed", "user_id", p.UserID, "error", err)
			} else {
				setCSRFCookie(w, token)
			}
		}
	}
	rec, err := h.repo.GetSelf(r.Context(), p.UserID)
	if err != nil {
		h.writeSelfError(w, "get me", err)
		return
	}
	writeJSON(w, http.StatusOK, newMeView(rec,
		h.effectivePermissions(r.Context(), p.UserID, rec.Role, rec.Status)))
}

// handlePatchMe 只写 display_name。字段白名单由 rbac 字段层先行校验
// （RegisterSessionRoutes 中包裹），这里再做类型/长度校验。
func (h *SessionHandlers) handlePatchMe(w http.ResponseWriter, r *http.Request) {
	p, _, ok := h.requireSession(w, r, false)
	if !ok {
		return
	}
	body, err := readRawObject(r)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request body")
		return
	}
	raw, present := body["display_name"]
	if !present {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "display_name is required")
		return
	}

	var displayName *string
	if string(raw) != "null" {
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "display_name must be a string or null")
			return
		}
		value = strings.TrimSpace(value)
		if utf8.RuneCountInString(value) > maxDisplayNameRunes {
			writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "display_name must be at most 32 characters")
			return
		}
		if value != "" {
			displayName = &value
		}
	}

	rec, err := h.repo.UpdateDisplayName(r.Context(), p.UserID, displayName)
	if err != nil {
		h.writeSelfError(w, "update display name", err)
		return
	}
	writeJSON(w, http.StatusOK, newMeView(rec,
		h.effectivePermissions(r.Context(), p.UserID, rec.Role, rec.Status)))
}

// ---------------------------------------------------------------------------
// /api/self
// ---------------------------------------------------------------------------

func (h *SessionHandlers) handleGetSelf(w http.ResponseWriter, r *http.Request) {
	p, _, ok := h.requireSession(w, r, false)
	if !ok {
		return
	}
	rec, err := h.repo.GetSelf(r.Context(), p.UserID)
	if err != nil {
		h.writeSelfError(w, "get self settings", err)
		return
	}
	writeJSON(w, http.StatusOK, newSelfView(rec))
}

// handlePutSelfSetting 实现 users 行上的乐观并发（controller ruling 4）：
// 请求必须携带 GET /api/self 返回的 updated_at；不匹配返回 409。
func (h *SessionHandlers) handlePutSelfSetting(w http.ResponseWriter, r *http.Request) {
	p, _, ok := h.requireSession(w, r, false)
	if !ok {
		return
	}
	body, err := readRawObject(r)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request body")
		return
	}

	rawToken, present := body["updated_at"]
	if !present {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "updated_at is required for optimistic locking")
		return
	}
	var token string
	if err := json.Unmarshal(rawToken, &token); err != nil || token == "" {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "updated_at must be an RFC 3339 timestamp")
		return
	}
	expectUpdatedAt, err := time.Parse(time.RFC3339Nano, token)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "updated_at must be an RFC 3339 timestamp")
		return
	}

	var patch SelfPatch
	if raw, present := body["language"]; present {
		var language string
		// JSON null 解码进 string 会静默得到 ""；后续 canonical 校验会拒绝，
		// 但显式区分能让错误信息更准确。
		if string(raw) == "null" {
			writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "language must be one of the canonical languages")
			return
		}
		if err := json.Unmarshal(raw, &language); err != nil || !isCanonicalLanguage(language) {
			writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "language must be one of the canonical languages")
			return
		}
		patch.Language = &language
	}
	if raw, present := body["sidebar_modules"]; present {
		sidebar, valid := normalizeSidebarModules(raw)
		if !valid {
			writeProblem(w, http.StatusBadRequest, authProblemBadRequest,
				"sidebar_modules must be an object of boolean values or null")
			return
		}
		patch.SidebarModules = &sidebar
	}
	if raw, present := body["record_ip_log"]; present {
		var record bool
		// null 解码进 bool 是无操作（不报错），必须显式拒绝，避免「传 null
		// 却静默写成 false」的语义漂移。
		if string(raw) == "null" || json.Unmarshal(raw, &record) != nil {
			writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "record_ip_log must be a boolean")
			return
		}
		patch.RecordIPLog = &record
	}
	if raw, present := body["default_model"]; present {
		// 只接受字符串：null 与缺失的差别在这里无关紧要（两者都不能表达
		// 「清除」——清除的冻结语义是空串，见 modules-02 §2.14 主流程 2）。
		// 显式拒绝 null 可避免前端把「字段置空」误当成清除。
		if string(raw) == "null" {
			writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "default_model must be a string")
			return
		}
		var defaultModel string
		if err := json.Unmarshal(raw, &defaultModel); err != nil {
			writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "default_model must be a string")
			return
		}
		// 非空值必须在「可用分组 × 已定价且有启用渠道的模型」集合内；不可用
		// 一律 422 user.default_model_invalid，绝不静默落空（modules-02 §2.14
		// 服务端校验）。空串是清除，不校验。
		if defaultModel != "" {
			if utf8.RuneCountInString(defaultModel) > maxDefaultModelRunes {
				writeDefaultModelInvalid(w)
				return
			}
			groups, err := h.repo.UsableGroupsForUser(r.Context(), p.UserID)
			if err != nil {
				h.log.Error("validate default model failed", "user_id", p.UserID, "error", err)
				writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
				return
			}
			if !defaultModelAvailable(groups, defaultModel) {
				writeDefaultModelInvalid(w)
				return
			}
		}
		patch.DefaultModel = &defaultModel
	}

	if patch.Language == nil && patch.SidebarModules == nil && patch.RecordIPLog == nil &&
		patch.DefaultModel == nil {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "no settings to update")
		return
	}

	rec, err := h.repo.UpdateSettings(r.Context(), p.UserID, patch, expectUpdatedAt)
	if err != nil {
		h.writeSelfError(w, "update self settings", err)
		return
	}
	writeJSON(w, http.StatusOK, newSelfView(rec))
}

// normalizeSidebarModules 校验稀疏覆盖并规范化编码。
//
// 规则（modules-01 F-01-31 / modules-08 §3.18 Q4）：值必须是 object，
// 每个键映射 bool；null 表示清空覆盖（SQL NULL）。重新 Marshal 丢弃原始
// 空白与键序，保证落库的是规范 JSON（jsonb 本身也会规范化，这里提前一步
// 让 handler 层的行为与存储无关）。
func normalizeSidebarModules(raw json.RawMessage) (json.RawMessage, bool) {
	if string(raw) == "null" {
		return json.RawMessage("null"), true
	}
	// 用 *bool 而不是 bool：JSON null 解码进 bool 是无操作（静默变成 false），
	// 会让 {"billing":null} 被当成「隐藏 billing」写入。显式拒绝 null 项，
	// 只接受 true/false（BOPLA 的失败关闭原则）。
	var modules map[string]*bool
	if err := json.Unmarshal(raw, &modules); err != nil {
		return nil, false
	}
	if len(modules) > maxSidebarModuleEntries {
		return nil, false
	}
	normalized := make(map[string]bool, len(modules))
	for key, value := range modules {
		if key == "" || utf8.RuneCountInString(key) > maxSidebarKeyRunes || value == nil {
			return nil, false
		}
		normalized[key] = *value
	}
	compact, err := json.Marshal(normalized)
	if err != nil {
		return nil, false
	}
	return compact, true
}

// ---------------------------------------------------------------------------
// /api/verify-email
// ---------------------------------------------------------------------------

type verifyEmailRequest struct {
	EmailCode string `json:"email_code"`
}

type verifyEmailResponse struct {
	EmailVerified   bool       `json:"email_verified"`
	EmailVerifiedAt *time.Time `json:"email_verified_at,omitempty"`
}

// handleVerifyEmail 消费 purpose=verify 的一次性验证码并把 email_verified_at
// 置为首次验证时间。验证码哈希与 Task 6 共用 emailCodeStore 的键/HMAC 口径，
// 因此本端点与注册/找回流程能互相消费同一套码。
func (h *SessionHandlers) handleVerifyEmail(w http.ResponseWriter, r *http.Request) {
	p, _, ok := h.requireSession(w, r, false)
	if !ok {
		return
	}
	ctx := r.Context()
	ip := clientIP(r)
	rec, err := h.repo.GetSelf(ctx, p.UserID)
	if err != nil {
		h.writeSelfError(w, "get user for email verification", err)
		return
	}
	if rec.Email == nil || strings.TrimSpace(*rec.Email) == "" {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "This account has no email address")
		return
	}
	if rec.EmailVerifiedAt != nil {
		// 幂等：已验证的账号重复调用不再要求验证码，也不改变首次验证时间。
		writeJSON(w, http.StatusOK, verifyEmailResponse{
			EmailVerified:   true,
			EmailVerifiedAt: rec.EmailVerifiedAt,
		})
		return
	}

	var req verifyEmailRequest
	if err := readAuthJSON(r, &req); err != nil || !sixDigitPattern.MatchString(req.EmailCode) {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid or expired verification code")
		return
	}
	if h.codes == nil || h.codes.rdb == nil {
		h.writeUnavailable(w, "verify email", errors.New("email code store is not configured"))
		return
	}
	okCode, err := h.codes.consume(r.Context(), normalizeEmail(*rec.Email), "verify", req.EmailCode)
	if err != nil {
		h.writeUnavailable(w, "consume email code", err)
		return
	}
	if !okCode {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid or expired verification code")
		return
	}
	// 注册赠送的发放时点求值：先按冻结的同 IP 上限预留名额（与邮箱验证码
	// 注册路径共享同一脚本/键），上限达成或计数不可用时跳过补发但仍完成
	// 验证。名额只在真正写出发放流水时保留；已发放（插入 0 行）/事务失败
	// 必须归还，否则重复验证会无谓消耗其他人的领赠额度。
	bonus := h.signupGrantOnVerify()
	grantReserved := false
	if bonus > 0 {
		allowed, err := reserveSignupGrantForIP(ctx, h.rdb, ip)
		switch {
		case err != nil:
			h.log.Warn("signup grant on verification suppressed: counter_unavailable",
				"user_id", p.UserID, "error", err)
			bonus = 0
		case !allowed:
			h.log.Warn("signup grant on verification suppressed: cap", "user_id", p.UserID)
			bonus = 0
		default:
			grantReserved = true
		}
	}

	verifiedAt, granted, err := h.repo.MarkEmailVerified(ctx, p.UserID, bonus)
	if err != nil {
		if grantReserved {
			if relErr := releaseSignupGrantSlotDetached(ctx, h.rdb, ip); relErr != nil {
				h.log.Warn("release signup grant slot after verification failure failed", "error", relErr)
			}
		}
		h.writeSelfError(w, "mark email verified", err)
		return
	}
	if grantReserved && !granted {
		if relErr := releaseSignupGrantSlotDetached(ctx, h.rdb, ip); relErr != nil {
			h.log.Warn("release signup grant slot after already-granted failed", "error", relErr)
		}
	}
	writeJSON(w, http.StatusOK, verifyEmailResponse{
		EmailVerified:   true,
		EmailVerifiedAt: &verifiedAt,
	})
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

// readRawObject 读取 JSON 对象并保留各字段的原始值，便于区分「字段缺失」、
// 「显式 null」与类型错误。空 body / 非对象一律 error。
func readRawObject(r *http.Request) (map[string]json.RawMessage, error) {
	var body map[string]json.RawMessage
	if err := readAuthJSON(r, &body); err != nil {
		return nil, err
	}
	if body == nil {
		return nil, errors.New("request body must be a JSON object")
	}
	return body, nil
}

func isCanonicalLanguage(language string) bool {
	for _, lang := range canonicalLanguages {
		if language == lang {
			return true
		}
	}
	return false
}

// defaultModelAvailable 判定模型是否出现在任一可用分组的模型清单中。
// 精确匹配：模型名是协议标识符，不做大小写/空白归一化（与令牌白名单同口径）。
func defaultModelAvailable(groups []store.UsableGroup, model string) bool {
	for _, group := range groups {
		for _, candidate := range group.Models {
			if candidate == model {
				return true
			}
		}
	}
	return false
}

// writeDefaultModelInvalid 输出冻结的 422：type URI 稳定可机读，title 保留
// 契约里的点分码 `user.default_model_invalid`（前端按 title 显示 / 埋点）。
func writeDefaultModelInvalid(w http.ResponseWriter) {
	writeProblem(w, http.StatusUnprocessableEntity, problemTypeDefaultModelInvalid, defaultModelInvalidTitle)
}

// signupGrantOnVerify 返回首次邮箱验证时应补发的赠送额（0 = 不补发）。
//
// 只在 billing.signup_grant_requires_verified_email=true 时补发：该开关关闭
// 时注册路径已经发放（或被反滥用上限抑制），验证不应二次发放。金额与开关
// 都读取「此刻」的配置快照（发放时点求值），不追溯注册时刻。
func (h *SessionHandlers) signupGrantOnVerify() int64 {
	if h.config == nil {
		return 0
	}
	raw, ok := h.config.Get("billing.signup_grant_requires_verified_email")
	if !ok {
		return 0
	}
	var required bool
	if err := json.Unmarshal(raw, &required); err != nil || !required {
		return 0
	}
	raw, ok = h.config.Get("billing.signup_grant_credits")
	if !ok {
		return 0
	}
	var credits int64
	if err := json.Unmarshal(raw, &credits); err != nil || credits <= 0 {
		return 0
	}
	return credits
}

// writeSelfError 把仓储错误映射为 HTTP 语义：
// 用户不存在 → 404；乐观锁冲突 → 409；其余（DB 故障）→ 500。
// 乐观锁冲突不重试、不合并：服务端无法知道调用方基于哪个版本做的编辑，
// 自动合并会静默覆盖并发写入（modules-08 §1.3 的 409 语义）。
func (h *SessionHandlers) writeSelfError(w http.ResponseWriter, op string, err error) {
	switch {
	case errors.Is(err, ErrSelfNotFound):
		writeProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found")
	case errors.Is(err, ErrSelfConflict):
		writeProblem(w, http.StatusConflict, authProblemConflict,
			"Settings were modified by someone else. Please refresh and retry.")
	default:
		h.log.Error(op+" failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
	}
}

// writeUnavailable 用于 Redis 故障：受保护端点必须 fail-closed（GC-7），
// 且给出有界 Retry-After，避免客户端立即重试风暴。
func (h *SessionHandlers) writeUnavailable(w http.ResponseWriter, op string, err error) {
	h.log.Error(op+" failed", "error", err)
	w.Header().Set("Retry-After", "5")
	writeProblem(w, http.StatusServiceUnavailable, authProblemServiceUnavailable,
		"Service temporarily unavailable")
}
