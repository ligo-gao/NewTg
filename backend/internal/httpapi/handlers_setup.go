// 本文件实现 modules-01 F-01-20「首次安装初始化向导」的服务端面。
//
// 幂等锚（2026-10-17 业主裁决 G7-R36）：system_settings 单键
// `system.install.completed_at`。首个 admin 的 INSERT 与该锚的 INSERT 必须在
// 同一事务内完成：
//   - 任一步失败都整体回滚，绝不留下「有 admin 无锚」或「有锚无 admin」的
//     半初始化状态；
//   - 并发双提交由锚的主键唯一约束裁决——第二个事务的
//     INSERT ... ON CONFLICT DO NOTHING 返回 0 行后回滚，因此只有一次提交能
//     落库（TC-F-01-20-02）。预检查只是为了避免为必然失败的请求付出 argon2
//     成本，绝不能把它当作幂等保证。
//
// 路由挂载：由 routes.go 的装配方调用 RegisterSetupRoutes(mux)。setup 是
// 公开端点（未初始化时系统里还没有任何账号），因此不挂 SessionAuth/CSRF。
package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/configcenter"
)

const (
	// SetupCompletedKey 是初始化完成标记（system_settings 的只读内部键，
	// G7-R36 冻结）。键的存在性即幂等锚，值仅用于运维排查（RFC3339 时间）。
	SetupCompletedKey = "system.install.completed_at"

	// 三个写入 system_settings 的键：锚 + setup 第一步收集的站点信息。
	// 站点键名以《系统配置键映射》与 modules-08 §1.4 为准（site.* 域）。
	setupSiteTitleKey     = "site.title"
	setupServerAddressKey = "site.server_address"

	// modules-08 的字段边界：site.title 1–64；site.server_address ≤256。
	maxSetupSiteTitleRunes     = 64
	maxSetupServerAddressRunes = 256

	// setupProblemAlreadyCompleted 是重复初始化专用的 problem type：前端据此
	// 跳 /sign-in（modules-01 F-01-20 的异常流程），不必解析标题文案。
	setupProblemAlreadyCompleted = "https://newtg.dev/problems/setup_already_completed"
)

// ErrSetupAlreadyCompleted 表示锚已存在：系统已完成初始化，本次提交整体回滚。
// 上层必须把它映射为 409，且不得与普通写失败混同。
var ErrSetupAlreadyCompleted = errors.New("httpapi: setup already completed")

// SetupInput 是初始化事务的全部写侧入参。PasswordHash 必须是调用方用
// auth.HashPassword 产出的 PHC 字符串——存储层绝不接触明文口令。
type SetupInput struct {
	SiteTitle     string
	ServerAddress string
	Username      string
	Email         string
	PasswordHash  string
}

// SetupResult 是事务内首管 INSERT ... RETURNING 的结果。Role/Status 取自信赖
// 的 DB 行（显式写入 admin/enabled），绝不接受客户端声明。
type SetupResult struct {
	UserID int64
	Role   string
	Status string
}

// SetupStore 是 setup 端点对持久层的依赖面。单测注入内存假实现；
// 生产实现是 SQLSetupStore。
type SetupStore interface {
	// IsInitialized 报告锚是否存在。错误必须上抛：调用方据此 fail-closed，
	// 不能在状态未知时放行写事务。
	IsInitialized(ctx context.Context) (bool, error)
	// CompleteSetup 在一个事务内写锚、首管与站点配置。锚已存在时返回
	// ErrSetupAlreadyCompleted；username/email 撞唯一约束时返回 ErrDuplicateUser。
	CompleteSetup(ctx context.Context, in SetupInput) (SetupResult, error)
}

// SQLSetupStore 是 SetupStore 的 PostgreSQL 实现（database/sql + pgx stdlib）。
//
// 为什么不用 configcenter.Center.Save 写锚：Save 自己开连接、独立提交，
// 无法与用户 INSERT 组成同一事务；而「同事务」正是 G7-R36 的核心约束。
// 这里直接写表，提交后再通过 configcenter 的变更频道通知各副本重载。
type SQLSetupStore struct {
	db *sql.DB
}

// NewSQLSetupStore 构造主库存储。
func NewSQLSetupStore(db *sql.DB) *SQLSetupStore { return &SQLSetupStore{db: db} }

// IsInitialized 用 EXISTS 读取锚的存在性。只读单行、无锁：权威判定仍由
// CompleteSetup 的 INSERT ... ON CONFLICT 完成。
func (s *SQLSetupStore) IsInitialized(ctx context.Context) (bool, error) {
	var exists bool
	if err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM system_settings WHERE "key" = $1)`,
		SetupCompletedKey).Scan(&exists); err != nil {
		return false, fmt.Errorf("httpapi: check setup anchor: %w", err)
	}
	return exists, nil
}

// CompleteSetup 在一个事务内完成：写锚 → 建首管 → 写 site.title /
// site.server_address。
//
// 顺序刻意让锚先 INSERT：并发双提交在锚的主键上串行化，第二个事务会阻塞到
// 第一个提交/回滚；第一个提交后它拿到 0 行受影响并回滚，连用户 INSERT 都不
// 会执行。若把锚放到最后，两个事务都会先写出 admin 行，虽然最终仍会回滚，
// 但白白消耗一次唯一索引写入并把失败路径拉长。
func (s *SQLSetupStore) CompleteSetup(ctx context.Context, in SetupInput) (SetupResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SetupResult{}, fmt.Errorf("httpapi: begin setup: %w", err)
	}
	// 主错误优先：成功路径显式 Commit；失败路径 Rollback 的错误无需覆盖主错误。
	defer func() { _ = tx.Rollback() }()

	anchor, err := json.Marshal(time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return SetupResult{}, fmt.Errorf("httpapi: encode setup anchor: %w", err)
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO system_settings ("key", value)
		VALUES ($1, $2)
		ON CONFLICT ("key") DO NOTHING`, SetupCompletedKey, anchor)
	if err != nil {
		return SetupResult{}, fmt.Errorf("httpapi: write setup anchor: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return SetupResult{}, fmt.Errorf("httpapi: setup anchor rows affected: %w", err)
	}
	if affected == 0 {
		return SetupResult{}, ErrSetupAlreadyCompleted
	}

	var out SetupResult
	err = tx.QueryRowContext(ctx, `
		INSERT INTO users (username, email, email_verified_at, password_hash, role, status)
		VALUES ($1, $2, now(), $3, 'admin', 'enabled')
		RETURNING id, role, status`,
		in.Username, in.Email, in.PasswordHash).
		Scan(&out.UserID, &out.Role, &out.Status)
	if isUniqueViolation(err) {
		return SetupResult{}, ErrDuplicateUser
	}
	if err != nil {
		return SetupResult{}, fmt.Errorf("httpapi: insert setup admin: %w", err)
	}

	// 站点配置与 admin 同事务：初始化完成后 site.title 与 site.server_address
	// 必须已经生效，否则首屏/邮件链接会短暂回落到设计默认值。
	if err := upsertSetupSetting(ctx, tx, setupSiteTitleKey, in.SiteTitle, out.UserID); err != nil {
		return SetupResult{}, err
	}
	if err := upsertSetupSetting(ctx, tx, setupServerAddressKey, in.ServerAddress, out.UserID); err != nil {
		return SetupResult{}, err
	}

	if err := tx.Commit(); err != nil {
		return SetupResult{}, fmt.Errorf("httpapi: commit setup: %w", err)
	}
	return out, nil
}

// upsertSetupSetting 写单个站点设置。使用 upsert 而不是纯 INSERT：部署方可能
// 在启动前预置了 site.* 行，setup 向导的值应当覆盖预置值。
func upsertSetupSetting(ctx context.Context, tx *sql.Tx, key, value string, updatedBy int64) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("httpapi: encode setup setting %q: %w", key, err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO system_settings ("key", value, updated_by)
		VALUES ($1, $2, $3)
		ON CONFLICT ("key") DO UPDATE
		SET value = EXCLUDED.value, updated_by = EXCLUDED.updated_by, updated_at = now()`,
		key, raw, updatedBy); err != nil {
		return fmt.Errorf("httpapi: write setup setting %q: %w", key, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// HTTP handler
// ---------------------------------------------------------------------------

// SetupAuditor 是初始化完成事件的审计接缝，签名与 audit.Logger.Log 一致：
// (ctx, action, actor, detail)。审计失败不得回滚已提交的初始化（日志库独立，
// 无跨库事务），只记录告警。
type SetupAuditor func(ctx context.Context, action string, actor int64, detail json.RawMessage) error

// SetupConfig 是装配输入。Store 必填；Sessions 用于 F-01-20 的自动登录，
// 生产装配必须提供（测试/受限装配用 AllowNoSession 显式豁免）；Redis 为 nil
// 时不广播配置变更；Audit 为 nil 时不写审计；Checker 零值即启用内置弱口令词表。
type SetupConfig struct {
	Store    SetupStore
	Sessions *auth.SessionManager
	Checker  auth.CompromisedPasswordChecker
	Redis    *redis.Client
	Audit    SetupAuditor
	Logger   *slog.Logger

	// AllowNoSession 显式允许 Sessions 为 nil（仅测试/受限装配）。生产路径
	// 缺少 Sessions 会让「初始化后自动登录」静默失效，必须启动期报错。
	AllowNoSession bool
}

// SetupHandlers 持有 setup 端点依赖。可并发使用。
type SetupHandlers struct {
	store    SetupStore
	sessions *auth.SessionManager
	checker  auth.CompromisedPasswordChecker
	rdb      *redis.Client
	audit    SetupAuditor
	log      *slog.Logger

	// 测试接缝；生产恒为 auth.HashPassword。
	hashPassword func(plain string) (string, error)
}

// NewSetupHandlers 构造 setup 端点。缺少 Store 属装配错误；缺少 Sessions 同样
// 是装配错误——F-01-20 要求初始化完成即自动登录，静默跳过会让前端拿到 201
// 却没有会话，直到跳转 /dashboard 才以 401 失败。
func NewSetupHandlers(cfg SetupConfig) (*SetupHandlers, error) {
	if cfg.Store == nil {
		return nil, errors.New("httpapi: SetupConfig.Store is required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.Sessions == nil {
		if !cfg.AllowNoSession {
			return nil, errors.New("httpapi: SetupConfig.Sessions is required for auto-login (set AllowNoSession only for tests or restricted assembly)")
		}
		logger.Warn("setup: auto-login disabled because Sessions is nil (AllowNoSession)")
	}
	return &SetupHandlers{
		store:        cfg.Store,
		sessions:     cfg.Sessions,
		checker:      cfg.Checker,
		rdb:          cfg.Redis,
		audit:        cfg.Audit,
		log:          logger,
		hashPassword: auth.HashPassword,
	}, nil
}

// RegisterSetupRoutes 把 setup 端点挂到公开 mux。setup 只允许未初始化时调用，
// 鉴权由「锚是否存在」在 handler 内判定——此时系统里还没有任何可登录账号，
// 因此不能挂 SessionAuth（否则永远无法完成首次初始化）。
func (h *SetupHandlers) RegisterSetupRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/setup", h.handleSetup)
}

// IsInitialized 读取锚的存在性，供装配层/状态接口复用。
//
// 读失败时回落 false：写路径的原子锚仍是权威闸门（DB 不可用时 POST 会 503，
// 不会创建任何数据）；若这里返回 true，一次短暂的 DB 抖动就会让安装向导
// 永久不可进入，且没有恢复入口。
func (h *SetupHandlers) IsInitialized(ctx context.Context) bool {
	initialized, err := h.store.IsInitialized(ctx)
	if err != nil {
		h.log.Warn("setup: read initialization anchor failed", "error", err)
		return false
	}
	return initialized
}

// setupRequest 是三步向导的扁平提交体（第一步 site、第二步 admin）。
//
// confirmPassword 与注册端点同名（modules-01 F-01-01 的字段表）；同时接受
// confirm_password 只为止损前端命名差异，两者都空或不一致都会被拒绝。
type setupRequest struct {
	SiteTitle            string `json:"site_title"`
	ServerAddress        string `json:"server_address"`
	Username             string `json:"username"`
	Email                string `json:"email"`
	Password             string `json:"password"`
	ConfirmPassword      string `json:"confirmPassword"`
	ConfirmPasswordSnake string `json:"confirm_password"`
}

// setupResponse 是 201 的公开投影：绝不包含口令、口令哈希或任何密钥材料。
type setupResponse struct {
	User setupUserView `json:"user"`
}

type setupUserView struct {
	ID            int64  `json:"id"`
	Username      string `json:"username"`
	Email         string `json:"email"`
	Role          string `json:"role"`
	Status        string `json:"status"`
	EmailVerified bool   `json:"email_verified"`
}

// setupCompletedProblem 在 RFC 7807 之外带 initialized=true：前端据此把重复
// 提交直接导流到 /sign-in（modules-01 F-01-20「已初始化标记」）。
type setupCompletedProblem struct {
	Type        string `json:"type"`
	Title       string `json:"title"`
	Status      int    `json:"status"`
	Initialized bool   `json:"initialized"`
}

func (h *SetupHandlers) handleSetup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// B13 裁决：/api/setup 加保守匿名桶（10 次/分钟/IP）。初始化前每个请求
	// 都付 argon2 成本，且未初始化窗口允许任何人抢先建首管；桶先于一切
	// 解析与 KDF 执行。认证面 Redis 故障 fail-closed（503）。
	// rdb 为 nil 仅出现在受限测试装配；生产装配必须提供 Redis。
	if h.rdb != nil {
		over, retry, err := allowRequestRate(ctx, h.rdb, SetupRateLimitKey(clientIP(r)),
			SetupRateLimit, SetupRateWindow)
		if err != nil {
			h.log.Error("setup request rate limiter unavailable", "error", err)
			h.writeSetupUnavailable(w)
			return
		}
		if over {
			w.Header().Set("Retry-After", retrySeconds(retry))
			writeProblem(w, http.StatusTooManyRequests, authProblemTooManyRequests, "Too many setup requests")
			return
		}
	}

	// 已初始化时直接 409：不为必然失败的请求付出 argon2 成本。这只是优化，
	// 并发窗口的最终裁决在 CompleteSetup 的事务里。
	initialized, err := h.store.IsInitialized(ctx)
	if err != nil {
		h.log.Error("setup: check initialization state failed", "error", err)
		h.writeSetupUnavailable(w)
		return
	}
	if initialized {
		writeSetupCompleted(w)
		return
	}

	var req setupRequest
	if err := readAuthJSON(r, &req); err != nil {
		writeProblemDetail(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request body", err.Error())
		return
	}
	if req.ConfirmPassword == "" {
		req.ConfirmPassword = req.ConfirmPasswordSnake
	}

	username := strings.TrimSpace(req.Username)
	email := normalizeEmail(req.Email)
	siteTitle := strings.TrimSpace(req.SiteTitle)

	if !usernamePattern.MatchString(username) {
		writeProblemDetail(w, http.StatusBadRequest, authProblemBadRequest, "Invalid username",
			"Username must be 3-32 characters of letters, digits, '_' or '-'")
		return
	}
	if !validEmail(email) {
		writeProblemDetail(w, http.StatusBadRequest, authProblemBadRequest, "Invalid email address", "")
		return
	}
	if !auth.ValidatePasswordLength(req.Password) {
		writeProblemDetail(w, http.StatusBadRequest, authProblemBadRequest,
			"Password must be between 8 and 128 characters", "")
		return
	}
	if req.Password != req.ConfirmPassword {
		writeProblemDetail(w, http.StatusBadRequest, authProblemBadRequest, "Passwords do not match", "")
		return
	}
	if n := utf8.RuneCountInString(siteTitle); n < 1 || n > maxSetupSiteTitleRunes {
		writeProblemDetail(w, http.StatusBadRequest, authProblemBadRequest, "Invalid site title",
			"Site title must be between 1 and 64 characters")
		return
	}
	serverAddress, ok := normalizeSetupServerAddress(req.ServerAddress)
	if !ok {
		writeProblemDetail(w, http.StatusBadRequest, authProblemBadRequest, "Invalid server address",
			"Server address must be an absolute http(s) URL without query or fragment")
		return
	}
	// 泄露/弱口令筛查（security-01 §2.3）：用户名与站点名都作为每用户上下文
	// 传入，阻止「admin + admin2026」「NewTG + newtg2026」这类可猜测组合。
	if h.checker.CheckWithContext(req.Password, username, siteTitle) {
		writeProblemDetail(w, http.StatusBadRequest, authProblemBadRequest,
			"Password is too weak or has been compromised", "")
		return
	}

	passwordHash, err := h.hashPassword(req.Password)
	if err != nil {
		h.log.Error("setup: hash admin password failed", "error", err)
		writeProblemDetail(w, http.StatusInternalServerError, authProblemInternal, "Setup failed", "")
		return
	}

	result, err := h.store.CompleteSetup(ctx, SetupInput{
		SiteTitle:     siteTitle,
		ServerAddress: serverAddress,
		Username:      username,
		Email:         email,
		PasswordHash:  passwordHash,
	})
	switch {
	case errors.Is(err, ErrSetupAlreadyCompleted):
		// 并发双提交的败者：整个事务已回滚，本次没有创建任何数据。
		writeSetupCompleted(w)
		return
	case errors.Is(err, ErrDuplicateUser):
		// 与注册路径同文案：不区分用户名/邮箱冲突，避免字段级枚举信道。
		writeProblemDetail(w, http.StatusConflict, authProblemConflict,
			"Username or email is already in use", "")
		return
	case err != nil:
		h.log.Error("setup: complete setup failed", "error", err)
		writeProblemDetail(w, http.StatusInternalServerError, authProblemInternal, "Setup failed", "")
		return
	}
	if result.Role != "admin" || result.Status != "enabled" {
		// DB 显式写入 admin/enabled；不一致说明数据被篡改或实现漂移，拒绝建会话。
		h.log.Error("setup: unexpected admin projection", "role", result.Role, "status", result.Status)
		writeProblemDetail(w, http.StatusInternalServerError, authProblemInternal, "Setup failed", "")
		return
	}

	// modules-01 F-01-20：初始化动作必须在审计日志可查。审计位于提交之后、
	// 建会话之前：即使随后建会话失败，事件也已落库；审计失败只告警，绝不
	// 回滚已完成的初始化（日志库独立，无跨库事务）。
	h.auditSystemInitialized(ctx, result, username, siteTitle, serverAddress)

	// setup 直接写表（为了事务），提交后必须主动广播，让各副本的 configcenter
	// 快照按 DB 重载，而不是等 5 分钟兜底。
	h.publishConfigChanged(ctx)

	// F-01-20：初始化完成后自动登录并跳 /dashboard。
	if h.sessions != nil {
		if err := h.startSetupSession(ctx, w, r, result); err != nil {
			// 账号与锚已提交；重试会得到 409（前端据此跳登录页），因此这里
			// 只需 fail-closed 地报告暂时不可用，绝不回滚已完成的初始化。
			h.log.Error("setup: create auto-login session failed", "error", err)
			h.writeSetupUnavailable(w)
			return
		}
	}

	writeJSON(w, http.StatusCreated, setupResponse{User: setupUserView{
		ID:            result.UserID,
		Username:      username,
		Email:         email,
		Role:          result.Role,
		Status:        result.Status,
		EmailVerified: true, // setup 路径显式置 email_verified_at=now()
	}})
}

// startSetupSession 建立自动登录会话并写 Cookie。与登录路径同口径：会话 id
// 只进 HttpOnly Cookie，CSRF 令牌另行下发给前端读取。
func (h *SetupHandlers) startSetupSession(ctx context.Context, w http.ResponseWriter, r *http.Request, user SetupResult) error {
	sid, err := h.sessions.Create(ctx, user.UserID, auth.SessionOpts{
		UA:   r.UserAgent(),
		IP:   clientIP(r),
		Role: user.Role, // 来自事务 RETURNING 的 DB 行
	})
	if err != nil {
		return err
	}
	token := auth.NewCSRFToken()
	if err := h.sessions.IssueCSRF(ctx, sid, token); err != nil {
		// 半建状态必须清理，避免留下没有 CSRF 的会话。
		_ = h.sessions.Revoke(ctx, sid)
		return err
	}
	setSessionCookies(w, sid, token)
	return nil
}

// publishConfigChanged 用 configcenter 冻结频道广播 setup 写入的键。载荷是
// 纯 key（Task 16 约定）；各副本收到后按 DB 重载。广播失败只告警：setup
// 已经提交，不能因为通知失败而回滚或报错。
//
// 使用剥离取消信号的短期上下文：客户端可能在收到响应前断开，但配置失效
// 通知是已提交写入的一部分，不能因为请求 ctx 取消而三连失败（否则副本要等
// 5 分钟兜底快照）。
func (h *SetupHandlers) publishConfigChanged(ctx context.Context) {
	if h.rdb == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	for _, key := range []string{SetupCompletedKey, setupSiteTitleKey, setupServerAddressKey} {
		if err := h.rdb.Publish(ctx, configcenter.ChangedChannel, key).Err(); err != nil {
			h.log.Warn("setup: publish config change failed", "key", key, "error", err)
		}
	}
}

// auditSystemInitialized 写 modules-01 F-01-20 的 system_initialized 事件。
// detail 同时携带 action（platform-01 §5 的审计分类口径）与首管身份；
// 绝不包含口令/哈希。
func (h *SetupHandlers) auditSystemInitialized(ctx context.Context, result SetupResult, username, siteTitle, serverAddress string) {
	if h.audit == nil {
		return
	}
	detail, err := json.Marshal(map[string]any{
		"action":         audit.ActionSystemInitialized,
		"user_id":        result.UserID,
		"username":       username,
		"site_title":     siteTitle,
		"server_address": serverAddress,
	})
	if err != nil {
		h.log.Warn("setup: encode system_initialized audit detail failed", "error", err)
		return
	}
	if err := h.audit(ctx, audit.ActionSystemInitialized, result.UserID, detail); err != nil {
		// 审计失败不得回滚初始化：账号/锚已经提交，日志库故障不应让部署者
		// 看到一个失败但其实已初始化完成的系统。
		h.log.Error("setup: write system_initialized audit failed", "error", err)
	}
}

func (h *SetupHandlers) writeSetupUnavailable(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "5")
	writeProblemDetail(w, http.StatusServiceUnavailable, authProblemServiceUnavailable,
		"Setup is temporarily unavailable", "")
}

// normalizeSetupServerAddress 校验并规范化 site.server_address：
// http(s) 绝对 URL、无 userinfo/查询串/片段、无空白，末尾斜杠去掉
// （modules-08 §1.4 的字段规则）。
//
// 除了 RawQuery/Fragment 非空，还要拒绝「空标记」：`https://host?` 会得到
// ForceQuery=true 而 RawQuery=""，`https://host/#` 会得到 Fragment=""，
// 只看字段值会把它们当成合法基址放行。
func normalizeSetupServerAddress(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || utf8.RuneCountInString(raw) > maxSetupServerAddressRunes {
		return "", false
	}
	if strings.ContainsAny(raw, " \t\r\n") || strings.Contains(raw, "#") {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery {
		return "", false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", false
	}
	return strings.TrimRight(raw, "/"), true
}

func writeSetupCompleted(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(http.StatusConflict)
	_ = json.NewEncoder(w).Encode(setupCompletedProblem{
		Type:        setupProblemAlreadyCompleted,
		Title:       "Setup has already been completed",
		Status:      http.StatusConflict,
		Initialized: true,
	})
}
