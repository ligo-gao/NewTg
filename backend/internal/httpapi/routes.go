package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/configcenter"
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
	// TokenHandlers 是 Task 12 的令牌端点集合。非 nil 时 NewRouter 自动挂载：
	// 管理面 CRUD 进受保护子树，GET /api/usage/token 走 HandlePublic 绕过
	// SessionAuth（自读用量对耗尽令牌仍须 200，modules-02 §2.13）。
	TokenHandlers *TokenHandlers
	// TokenService 是令牌 CRUD 服务（生产 *tokenstore.Store）。提供且未显式
	// 给出 TokenHandlers/TokenStore 时，NewRouter 自动构造并接线两者。
	TokenService TokenService
	// PricingHandlers 是 M3 定价管理端点（modules-06 F-06-13/14/16）。
	// 非 nil 时挂到受保护管理子树；装配方负责构造 Repo 与字段策略。
	// 定价端点的路径族是 /api/model-pricing 与 /api/group-pricing——不得复用
	// 公开目录 /api/pricing（modules-06 G-5 命名空间裁决）。
	PricingHandlers *PricingHandlers
	// Sealer 密封令牌幂等缓存体（含一次性明文 key）。nil 时令牌端点跳过
	// 幂等缓存（绝不回落明文）；生产由 CREDENTIAL_ENCRYPTION_KEY 派生。
	Sealer configcenter.SecretSealer
	// Initialized 查询安装向导是否完成（GET /api/status 的 system_initialized）。
	// nil 时状态端点按未初始化处理；生产装配必须传入 SetupStore.IsInitialized。
	Initialized func(ctx context.Context) (bool, error)
	// GeoIP 是本地 GeoIP 库查询接缝（测试/未来里程碑）；nil 时 /api/geoip
	// 返回 source=unknown。
	GeoIP GeoIPLookup
	// LoginLimiter 是登录失败锁定桶；nil 时按 Redis 构造。与 2FA 共享同一
	// 实例可保证「同 Redis 键、同一阈值」，装配方应在 BuildGatewayRouter 中
	// 构造一次复用。
	LoginLimiter *LoginLimiter
	// LoginEncryption 是登录前端加密的解密接缝（F-01-25）。非 nil 时
	// NewRouter 挂载公开的 GET /api/auth/login/encryption-key 并把
	// password_encrypted/encryption_key_id 接入登录路径。
	LoginEncryption *PasswordEncryption
	// TurnstileSettings 返回人机校验配置快照。非 nil 时 NewRouter 为 login/
	// register/verification/reset_password 四个已存在的接入点构造 **handler
	// 内层** 校验（限流外层 → Turnstile 内层）。nil 表示未配置 Turnstile。
	TurnstileSettings func() auth.TurnstileSettings
	// TurnstileVerifyURL / TurnstileHTTPClient 是 siteverify 的测试接缝。
	TurnstileVerifyURL  string
	TurnstileHTTPClient *http.Client
	// Audit 是登录审计写入器（nil 时不写审计）。写失败不影响登录结果。
	Audit *audit.Logger
	// SignupGrantAlert 在注册赠送被反滥用上限抑制时触发（nil 时仅 Warn 日志）。
	SignupGrantAlert func(ctx context.Context, userID int64, ip string)
	Logger           *slog.Logger
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
	// handler 是 mux 外层的统一响应策略（/api/ 默认 no-store）。在 NewRouter
	// 构造时包住 mux 指针：之后追加的路由仍会经过该层。
	handler http.Handler
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
	h.audit = cfg.Audit
	h.signupGrantAlert = cfg.SignupGrantAlert
	h.initialized = cfg.Initialized
	h.geoIP = cfg.GeoIP
	if cfg.LoginLimiter != nil {
		// 与 2FA 共享同一失败锁定桶：阈值/键形只有一处实现。
		h.limiter = cfg.LoginLimiter
	}
	if cfg.LoginEncryption != nil {
		// 登录解密接缝必须与公钥端点使用同一实例（同一双槽密钥管理器）。
		h.passwordEncryption = cfg.LoginEncryption
	}
	if cfg.TurnstileSettings != nil {
		h.turnstile = newTurnstileGates(cfg.TurnstileSettings, cfg.Redis,
			cfg.TurnstileVerifyURL, cfg.TurnstileHTTPClient, cfg.Logger)
	}

	// 令牌服务的便利接线：生产只需提供 TokenService，路由层负责构造
	// TokenHandlers 并让 Bearer 鉴权使用同一 Store。
	if cfg.TokenService != nil {
		if cfg.TokenHandlers == nil {
			cfg.TokenHandlers = NewTokenHandlers(cfg.TokenService, cfg.Audit, cfg.Redis, cfg.Sealer)
		}
		if cfg.TokenStore == nil {
			cfg.TokenStore = cfg.TokenService
		}
	}

	rt := &Router{
		mux:       http.NewServeMux(),
		protected: http.NewServeMux(),
		handlers:  h,
	}
	rt.handler = noStoreAPI(rt.mux)

	// 登录加密公钥端点是公开的（登录前取公钥），精确模式优先于 /api/ 子树。
	if cfg.LoginEncryption != nil {
		RegisterLoginEncryptionRoutes(rt.mux, cfg.LoginEncryption)
	}

	// 令牌端点（Task 12）：CRUD 走受保护链，自读用量走根 mux 的精确模式。
	if cfg.TokenHandlers != nil {
		cfg.TokenHandlers.RegisterTokenCRUDRoutes(rt.protected, nil)
		rt.HandlePublic("GET /api/usage/token", http.HandlerFunc(cfg.TokenHandlers.tokenUsage))
	}

	// 定价管理端点（M3 Task 7）：只挂受保护子树；RBAC 与写字段策略在
	// PricingHandlers 构造时登记（未登记动作由 authz 失败关闭）。
	if cfg.PricingHandlers != nil {
		cfg.PricingHandlers.RegisterPricingRoutes(rt.protected, nil)
	}

	// 公开端点：精确注册，先于 /api/ 子树（ServeMux 按特异性择路，顺序无关，
	// 这里仍先注册以免读者误判）。
	rt.mux.HandleFunc("GET /api/status", h.handleStatus)
	// 公开地理区域：精确模式挂根 mux，绕过 SessionAuth（匿名恒 200）。
	rt.mux.HandleFunc("GET /api/geoip", h.handleGeoIP)
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

// HandlePublic 把 handler 挂到根 mux。精确模式比 `/api/` 子树更具体，
// 因此会绕过 SessionAuth 链——只允许自带凭据语义的端点使用。
//
// 当前唯一使用者是 GET /api/usage/token：令牌自读用量必须对「额度耗尽」
// 的令牌返回 200（modules-02 §2.13），而 SessionAuth 的 tokenUsable 会拒绝
// 耗尽令牌；它还需要返回 token_disabled/token_expired 的精确错误码。
func (rt *Router) HandlePublic(pattern string, h http.Handler) {
	rt.mux.Handle(pattern, h)
}

// ServeHTTP 实现 http.Handler。
func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	handler := rt.handler
	if handler == nil {
		// 零值/手工字面量（仅测试）没有外层响应策略；生产路径恒由
		// NewRouter 设置 handler（含 /api/ 默认 no-store）。
		handler = rt.mux
	}
	handler.ServeHTTP(w, r)
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
