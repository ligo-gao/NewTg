package httpapi

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/rbac"
)

// RouterConfig 是管理面 HTTP 路由的装配输入。必填 Redis（会话/失败锁定/
// 邮箱验证码都依赖它）与 Sessions（或 SessionSecret）；Repo 与 DB 二选一，
// 单测注入内存 Repo，生产注入 *sql.DB 由 SQLAuthRepo 落地。
type RouterConfig struct {
	DB       *sql.DB
	Redis    *redis.Client
	Sessions *auth.SessionManager

	// Repo 是 users 表的认证读取面；nil 时由 DB 构造 SQLAuthRepo。
	Repo AuthRepo
	// Permissions 覆盖 RBAC 权限装载器（默认使用 Repo 的 Load 实现）。
	Permissions rbac.PermissionLoader
	// Config 是 configcenter.Center 的读取面；nil 时全部取设计默认值。
	Config ConfigReader
	// SessionSecret 用于 CSRF HMAC 与邮箱验证码 HMAC（config.SessionSecret）。
	SessionSecret []byte
	// PublicBaseURL 是 Origin/Referer 白名单（逗号分隔，可为多个）；空时按请求 Host 同源。
	PublicBaseURL string
	// EmailSender 为 nil 时验证码只生成不下发（未配置 SMTP 的 M1 状态）。
	EmailSender EmailCodeSender
	// Authz 是 RBAC 中间件；提供时 NewRouter 会接线权限装载器。
	Authz *rbac.AuthzMiddleware
	// PasswordChecker 是可选的泄露口令检查器（零值即启用内置词表）。
	PasswordChecker auth.CompromisedPasswordChecker
	// TokenStore 是 Bearer 令牌查询接缝（Task 12 实现；nil 时 Bearer 一律 401）。
	TokenStore TokenLookup
	Logger     *slog.Logger
}

// Router 是管理面根路由：公开认证端点直接注册，其余 /api/* 走
// SessionAuth → CSRF → OriginRefererCheck 的受保护链。
//
// 公开端点（status/register/login/email-code/logout）不挂 CSRF/Origin：
// security-01 §2.2 的豁免域要求它们不依赖已建会话；logout 额外是
// public-safe 的（重复登出/无会话登出都必须 204）。
type Router struct {
	mux       *http.ServeMux
	protected *http.ServeMux
	handlers  *authHandlers
}

// NewRouter 组装路由。返回 error 而不是 panic：缺 Redis/仓储属于装配错误，
// 应让进程启动失败，而不是带病提供服务。
func NewRouter(cfg RouterConfig) (*Router, error) {
	if cfg.Redis == nil {
		return nil, errors.New("httpapi: RouterConfig.Redis is required (sessions, login lockout, email codes)")
	}
	if cfg.Sessions == nil {
		if len(cfg.SessionSecret) == 0 {
			return nil, errors.New("httpapi: RouterConfig.Sessions or SessionSecret is required")
		}
		cfg.Sessions = auth.NewSessionManager(cfg.Redis, cfg.SessionSecret)
	}

	repo := cfg.Repo
	if repo == nil {
		if cfg.DB == nil {
			return nil, errors.New("httpapi: RouterConfig.Repo or DB is required")
		}
		repo = NewSQLAuthRepo(cfg.DB)
	}

	// login_sessions 只是审计副本：只在有 DB 时接线；哈希由 SessionManager
	// 在调用前计算（HashSessionID），本层拿不到明文 sid（I11）。
	if cfg.DB != nil {
		cfg.Sessions.SetSessionRecorder(NewSQLSessionRecorder(cfg.DB))
	}

	if cfg.Authz != nil {
		loader := cfg.Permissions
		if loader == nil {
			if l, ok := repo.(rbac.PermissionLoader); ok {
				loader = l
			}
		}
		if loader == nil {
			// 没有装载器时 Principal.Permissions 恒为 nil，而 nil 在 RBAC 语义里
			// 是 DB NULL=FULL——这会把任何 admin 误判为全权管理员。装配期拒绝。
			return nil, errors.New("httpapi: Authz requires a PermissionLoader (RouterConfig.Permissions or an AuthRepo implementing Load)")
		}
		cfg.Authz.SetPermissionLoader(loader)
	}

	h := newAuthHandlers(repo, cfg.Sessions, cfg.Redis, cfg.Config,
		cfg.SessionSecret, cfg.EmailSender, withSiteTitle(cfg.Config, cfg.PasswordChecker), cfg.Logger)

	rt := &Router{
		mux:       http.NewServeMux(),
		protected: http.NewServeMux(),
		handlers:  h,
	}

	// 公开端点：精确注册，先于 /api/ 子树（ServeMux 按特异性择路，顺序无关，
	// 这里仍先注册以免读者误判）。
	rt.mux.HandleFunc("GET /api/status", h.handleStatus)
	rt.mux.HandleFunc("POST /api/auth/email-code", h.handleEmailCode)
	rt.mux.HandleFunc("POST /api/auth/register", h.handleRegister)
	rt.mux.HandleFunc("POST /api/auth/login", h.handleLogin)
	rt.mux.HandleFunc("POST /api/logout", h.handleLogout)

	// 受保护子树：所有未显式登记的 /api/* 必须先通过认证链。顺序冻结为
	// SessionAuth → CSRF → OriginRefererCheck（Task 4/5 评审裁决）。
	protected := Protect(cfg.Sessions, cfg.TokenStore, cfg.PublicBaseURL)(rt.protected)
	rt.mux.Handle("/api/", protected)

	return rt, nil
}

// Protect 返回受保护路由的中间件链。装配顺序即执行顺序：
// 先确认身份（SessionAuth），再校验同步器令牌（CSRF），最后同源纵深。
func Protect(sessions *auth.SessionManager, tokenStore TokenLookup, publicBaseURL string) func(http.Handler) http.Handler {
	origins := parseAllowedOrigins(publicBaseURL)
	return func(next http.Handler) http.Handler {
		return SessionAuth(sessions, tokenStore)(CSRF(sessions)(OriginRefererCheck(origins)(next)))
	}
}

// HandleProtected 把业务 handler 注册到受保护子树。后续任务（令牌/会话/
// 管理端点）经此挂载，无需重复装配鉴权链。
func (rt *Router) HandleProtected(pattern string, h http.Handler) {
	rt.protected.Handle(pattern, h)
}

// ServeHTTP 实现 http.Handler。
func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rt.mux.ServeHTTP(w, r)
}

func parseAllowedOrigins(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// withSiteTitle 把 site.title 注入泄露口令检查器的站点级上下文词
// （security-01 §2.3：站点名/产品名不得出现在口令中）。
func withSiteTitle(config ConfigReader, checker auth.CompromisedPasswordChecker) auth.CompromisedPasswordChecker {
	if config == nil {
		return checker
	}
	raw, ok := config.Get("site.title")
	if !ok {
		return checker
	}
	var title string
	if err := json.Unmarshal(raw, &title); err != nil {
		return checker
	}
	title = strings.TrimSpace(title)
	if title == "" {
		return checker
	}
	checker.ContextWords = append(checker.ContextWords, title)
	return checker
}
