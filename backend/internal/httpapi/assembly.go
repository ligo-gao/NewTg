package httpapi

// M1 管理面路由的完整装配（gateway 角色）。
//
// NewRouter 只负责认证主链与令牌端点；密码、2FA、会话/自助、setup、账号、
// 登录加密、用户/分组等任务各自导出 Register*Routes，由本文件统一接线。
// 这样 routes.go 保持「路由核心」、本文件是「组合根」，单测可以注入 Repo
// 缝隙（与各 handler 的「Repo 与 DB 二选一」约定一致）而不触真 PG。
//
// 装配失败必须阻止进程上线（GC-7 / security-02 IS-B-6）：
//   - 缺权限装载器会让 Principal.Permissions=nil 被 RBAC 解释成 FULL；
//   - 缺审计写库会静默丢失登录/管理事件。
// 上述任一情况都返回 error，绝不降级为「带病运行」。
//
// PUBLIC_BASE_URL **不是**启动错误：生效基址走 env → site.server_address →
// 空（相关功能禁用）的回退链（见 effectivePublicBaseURL）。M1 无 SMTP，空
// 基址只表示重置链接与 Origin 白名单暂不可用，compose 在未设置该变量时
// 仍必须能启动。

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
	"newtg/gateway/internal/store"
)

// GatewayDeps 是 gateway 角色装配管理面所需的完整依赖。
//
// 生产路径只填基础设施（DB/Redis/Session/Config/Audit/Authz/TokenService），
// 各 Repo 留 nil 由 SQL 实现兜底；单测则可以填 Repo 缝隙与 LoginEncryption，
// 从而在没有 PostgreSQL 的情况下验证路由挂载与鉴权链。
type GatewayDeps struct {
	// 基础设施（必填）。
	DB       *sql.DB
	LogDB    *sql.DB // audit_logs 所在日志库
	Redis    *redis.Client
	Sessions *auth.SessionManager
	Config   ConfigReader
	// Sealer 解封配置中心的密钥类键（auth.turnstile.secret_key）。生产为
	// CREDENTIAL_ENCRYPTION_KEY 派生的 auth.SecretCipher。
	Sealer       configcenter.SecretSealer
	Audit        *audit.Logger
	Authz        *rbac.AuthzMiddleware
	TokenService TokenService

	SessionSecret           []byte
	CredentialEncryptionKey string
	PublicBaseURL           string
	EmailSender             EmailCodeSender
	PasswordChecker         auth.CompromisedPasswordChecker
	SignupGrantAlert        func(ctx context.Context, userID int64, ip string)
	Logger                  *slog.Logger

	// 可选覆盖/测试缝隙。nil 时由 DB 构造 SQL 实现。
	Repo              AuthRepo
	Permissions       rbac.PermissionLoader
	SetupStore        SetupStore
	SelfRepo          SelfRepo
	AccountRepo       AccountRepo
	AccountAnonymizer PIIAnonymizer
	// UserAnonymizer 是管理端删除的 PII 匿名化测试接缝；生产留 nil 由 LogDB
	// 构造 SQLPIIAnonymizer。与 AccountAnonymizer 分开命名，避免把自助删除的
	// 依赖误当作管理端删除已接线。
	UserAnonymizer PIIAnonymizer
	AccountReauth  *auth.ReauthGate
	VerifyTOTP     auth.TOTPVerifier
	TwoFARepo      TwoFARepo
	AdminUserRepo  AdminUserRepo
	AdminGroupRepo AdminGroupRepo
	// PricingRepo 是定价管理仓储的可选测试接缝；nil 且 DB 非 nil 时由
	// store.NewPricingRepo(DB) 构造。DB 与 PricingRepo 都为空时不挂定价端点
	// （M1 的纯 Repo 单测装配没有定价库表）。
	PricingRepo store.PricingRepo
	// PricingOnChange 在定价写成功后触发（R-63）：生产接 pricing.DBSource.Invalidate，
	// 让新价立即生效，而不是等 sourceCacheTTL=30s 的兜底。nil 表示只靠 TTL。
	PricingOnChange    func()
	NotificationSender NotificationTestSender
	LoginEncryption    *PasswordEncryption

	// ChannelHandler 是 M2 渠道管理端点集合（GET/POST /api/channels 等）。
	// 非 nil 时挂到受保护子树，与其余管理端点共用 SessionAuth → CSRF →
	// OriginRefererCheck 链；handler 内部再按 channels:* 做功能级权限检查。
	ChannelHandler *ChannelHandler

	// RedemptionHandlers 是 M5 兑换码端点集合（Task 2）。非 nil 时挂到
	// 受保护子树；管理端点内部按 redemption:manage 做 RBAC，用户兑换走
	// 独立 Redis 限流桶。
	RedemptionHandlers *RedemptionHandlers

	// TopupHandlers 是 M5 在线充值端点集合（Task 3）。非 nil 时：
	//   - GET /api/topup/config、POST /api/orders 挂到受保护子树；
	//   - POST /api/webhooks/{stripe,alipay} 以精确模式挂根 mux，绕过
	//     SessionAuth/CSRF（渠道回调没有会话，安全性由验签+限流承担）。
	TopupHandlers *TopupHandlers

	// Turnstile siteverify 测试接缝（生产留零值）。
	TurnstileVerifyURL  string
	TurnstileHTTPClient *http.Client
	// TurnstileSettings 覆盖由配置中心派生的设置读取（测试接缝）。nil 时
	// 使用 TurnstileSettingsProvider(Config, Sealer)；非 nil 时完全以它为准，
	// 便于单测注入「开启 + 固定密钥」而无需真实 sealer。
	TurnstileSettings func() auth.TurnstileSettings
}

// BuildGatewayRouter 组装全部 M1 端点。返回的 Router 已包含：
//
//	公开：status/email-code/register/login/logout、setup、密码重置三端点、
//	      登录加密公钥、2FA challenge、GET /api/usage/token；
//	受保护：SessionAuth → CSRF → OriginRefererCheck 之后的全部 /api/*。
func BuildGatewayRouter(deps GatewayDeps) (*Router, error) {
	if err := deps.validate(); err != nil {
		return nil, err
	}

	// Turnstile 配置读取：启动期已开启但缺 SecretSealer 属于装配错误。
	settings := deps.TurnstileSettings
	if settings == nil {
		var err error
		settings, err = TurnstileSettingsProvider(deps.Config, deps.Sealer, deps.Logger)
		if err != nil {
			return nil, err
		}
	}

	loginEncryption := deps.LoginEncryption
	if loginEncryption == nil {
		created, err := NewPasswordEncryption(PasswordEncryptionConfig{
			DB:            deps.DB,
			Config:        deps.Config,
			EncryptionKey: deps.CredentialEncryptionKey,
			Logger:        deps.Logger,
		})
		if err != nil {
			return nil, err
		}
		loginEncryption = created
	}

	// 登录失败锁定桶只构造一次，登录与 2FA 复用以保证同阈值/同键形。
	limiter := NewLoginLimiter(deps.Redis)

	// 生效基址（platform-03 §3.1 #20）：PUBLIC_BASE_URL 优先；为空时回退到
	// 初始化向导写入 system_settings 的 site.server_address；仍为空表示
	// 「相关功能禁用」，而不是启动失败（M1 无 SMTP，重置投递本就不可用）。
	baseURL := effectivePublicBaseURL(deps.PublicBaseURL, deps.Config)

	// SetupStore 同时服务安装端点与 GET /api/status 的 system_initialized
	// （同一份 EXISTS 语义，避免状态端点与写路径各自解释「已初始化」）。
	setupStore := deps.SetupStore
	if setupStore == nil {
		setupStore = NewSQLSetupStore(deps.DB)
	}

	// 定价管理端点（M3 Task 7）。生产走 DB；单测可注入 PricingRepo。
	// 两者都为空时跳过——M1 的内存 Repo 装配没有 model_pricing 表，强行构造
	// 只会在第一个请求上失败。
	var pricingHandlers *PricingHandlers
	if deps.PricingRepo != nil || deps.DB != nil {
		pricingRepo := deps.PricingRepo
		if pricingRepo == nil {
			pricingRepo = store.NewPricingRepo(deps.DB)
		}
		handlers, err := NewPricingHandlers(PricingHandlerConfig{
			Repo:     pricingRepo,
			Redis:    deps.Redis,
			Authz:    deps.Authz,
			Logger:   deps.Logger,
			OnChange: deps.PricingOnChange,
		})
		if err != nil {
			return nil, err
		}
		pricingHandlers = handlers
	}

	rt, err := NewRouter(RouterConfig{
		DB:                  deps.DB,
		Repo:                deps.Repo,
		Redis:               deps.Redis,
		Sessions:            deps.Sessions,
		Permissions:         deps.Permissions,
		Config:              deps.Config,
		Sealer:              deps.Sealer,
		Initialized:         setupStore.IsInitialized,
		SessionSecret:       deps.SessionSecret,
		PublicBaseURL:       baseURL,
		EmailSender:         deps.EmailSender,
		Authz:               deps.Authz,
		PasswordChecker:     deps.PasswordChecker,
		TokenService:        deps.TokenService,
		PricingHandlers:     pricingHandlers,
		LoginLimiter:        limiter,
		LoginEncryption:     loginEncryption,
		TurnstileSettings:   settings,
		TurnstileVerifyURL:  deps.TurnstileVerifyURL,
		TurnstileHTTPClient: deps.TurnstileHTTPClient,
		Audit:               deps.Audit,
		SignupGrantAlert:    deps.SignupGrantAlert,
		Logger:              deps.Logger,
	})
	if err != nil {
		return nil, err
	}

	// M2 渠道管理端点：注册到 rt.protected（模式本身就是完整 /api/channels*），
	// 因此复用 NewRouter 已挂好的受保护链与 no-store 响应策略。
	if deps.ChannelHandler != nil {
		deps.ChannelHandler.Register(rt.protected)
	}

	// M5 兑换码端点：与渠道/定价共用同一条受保护链。用户兑换端点也挂在
	// 受保护子树（必须已认证），限流在 handler 内执行。
	if deps.RedemptionHandlers != nil {
		deps.RedemptionHandlers.RegisterRedemptionRoutes(rt.protected, nil)
	}

	// M5 在线充值端点（Task 3）：用户端点共用受保护链；webhook 精确挂根
	// mux（验签 + 每 provider 独立限流，见 handlers_topup.go）。
	if deps.TopupHandlers != nil {
		deps.TopupHandlers.RegisterTopupRoutes(rt.protected, nil)
		deps.TopupHandlers.RegisterWebhookRoutes(rt)
	}

	checker := withSiteTitle(deps.Config, deps.PasswordChecker)

	setup, err := NewSetupHandlers(SetupConfig{
		Store:    setupStore,
		Sessions: deps.Sessions,
		Checker:  checker,
		Redis:    deps.Redis,
		// 统一走 writeAuditEvent：WithoutCancel + 3s 上限。setup 的审计发生在
		// 提交之后，客户端断连或日志库变慢都不能丢事件/拖住响应。
		Audit: func(ctx context.Context, action string, actor int64, detail json.RawMessage) error {
			return writeAuditEvent(ctx, deps.Audit, audit.Event{
				Action: action, Actor: actor, Detail: detail,
			})
		},
		Logger: deps.Logger,
	})
	if err != nil {
		return nil, err
	}
	setup.RegisterSetupRoutes(rt.mux)

	selfRepo := deps.SelfRepo
	if selfRepo == nil {
		selfRepo = NewSQLSelfRepo(deps.DB)
	}
	sessions, err := NewSessionHandlers(SessionHandlerConfig{
		Repo:          selfRepo,
		Sessions:      deps.Sessions,
		Authz:         deps.Authz,
		Redis:         deps.Redis,
		SessionSecret: deps.SessionSecret,
		Config:        deps.Config,
		Logger:        deps.Logger,
	})
	if err != nil {
		return nil, err
	}
	sessions.RegisterSessionRoutes(rt.protected, nil)

	account, err := NewAccountHandlers(AccountHandlerConfig{
		DB:                 deps.DB,
		Repo:               deps.AccountRepo,
		LogDB:              deps.LogDB,
		Redis:              deps.Redis,
		Sessions:           deps.Sessions,
		Audit:              deps.Audit,
		Anonymizer:         deps.AccountAnonymizer,
		Config:             deps.Config,
		EncryptionKey:      deps.CredentialEncryptionKey,
		SessionSecret:      deps.SessionSecret,
		Reauth:             deps.AccountReauth,
		VerifyTOTP:         deps.VerifyTOTP,
		NotificationSender: deps.NotificationSender,
		Logger:             deps.Logger,
	})
	if err != nil {
		return nil, err
	}
	account.RegisterAccountRoutes(rt.protected, nil)

	twoFA, err := NewTwoFAHandlers(TwoFAConfig{
		DB:            deps.DB,
		Repo:          deps.TwoFARepo,
		Redis:         deps.Redis,
		Sessions:      deps.Sessions,
		EncryptionKey: deps.CredentialEncryptionKey,
		Issuer:        readConfigString(deps.Config, "site.title"),
		Limiter:       limiter,
		Audit:         deps.Audit,
		Logger:        deps.Logger,
	})
	if err != nil {
		return nil, err
	}
	Register2FARoutes(rt.mux, rt.protected, twoFA)

	users, err := NewUserHandlers(UserHandlerConfig{
		DB:              deps.DB,
		Repo:            deps.AdminUserRepo,
		LogDB:           deps.LogDB,
		Anonymizer:      deps.UserAnonymizer,
		Sessions:        deps.Sessions,
		Redis:           deps.Redis,
		Authz:           deps.Authz,
		Audit:           deps.Audit,
		PasswordChecker: checker,
		Logger:          deps.Logger,
	})
	if err != nil {
		return nil, err
	}
	users.RegisterUserRoutes(rt.protected, nil)

	groups, err := NewGroupHandlers(GroupHandlerConfig{
		DB:     deps.DB,
		Repo:   deps.AdminGroupRepo,
		Authz:  deps.Authz,
		Audit:  deps.Audit,
		Logger: deps.Logger,
	})
	if err != nil {
		return nil, err
	}
	groups.RegisterGroupRoutes(rt.protected, nil)

	// 密码端点最后挂载：它要求认证仓储同时实现 PasswordRepo，并在有发信器
	// 时校验 PUBLIC_BASE_URL 的契约。
	if err := rt.RegisterPasswordRoutes(baseURL, deps.Audit); err != nil {
		return nil, err
	}
	return rt, nil
}

// effectivePublicBaseURL 实施 platform-03 §3.1 #20 的回退链：
//
//	PUBLIC_BASE_URL（env，可逗号分隔多个）→ site.server_address（setup 向导
//	写入 system_settings）→ ""（相关功能禁用）。
//
// 返回值原样交给 NewRouter（Origin/Referer 白名单）与密码重置链接拼接；
// 合法性由各自的消费点判定（空值不再作为启动错误）。
func effectivePublicBaseURL(envValue string, config ConfigReader) string {
	if value := strings.TrimSpace(envValue); value != "" {
		return value
	}
	return strings.TrimSpace(readConfigString(config, setupServerAddressKey))
}

// validate 集中声明装配期的必填项。消息列出全部缺失项，便于一次修完。
func (d GatewayDeps) validate() error {
	var missing []string
	add := func(cond bool, name string) {
		if cond {
			missing = append(missing, name)
		}
	}

	add(d.Redis == nil, "Redis")
	add(d.Sessions == nil, "Sessions")
	add(len(d.SessionSecret) == 0, "SessionSecret")
	add(d.Config == nil, "Config")
	add(d.Authz == nil, "Authz (RBAC middleware)")
	add(d.Audit == nil || !d.Audit.Configured(), "Audit logger bound to the logs database")
	add(d.TokenService == nil, "TokenService")
	add(strings.TrimSpace(d.CredentialEncryptionKey) == "", "CredentialEncryptionKey")

	// Repo 缝隙与 DB 二选一：认证主链、密码重置、令牌 store 都以 DB 为
	// 生产真源；单测可注入完整的 Repo 集合。
	if d.DB == nil {
		add(d.Repo == nil, "Repo or DB")
		add(d.SetupStore == nil, "SetupStore or DB")
		add(d.SelfRepo == nil, "SelfRepo or DB")
		add(d.AccountRepo == nil, "AccountRepo or DB")
		add(d.TwoFARepo == nil, "TwoFARepo or DB")
		add(d.AdminUserRepo == nil, "AdminUserRepo or DB")
		add(d.AdminGroupRepo == nil, "AdminGroupRepo or DB")
		add(d.AccountReauth == nil, "AccountReauth or DB")
	}
	add(d.AccountAnonymizer == nil && d.LogDB == nil, "LogDB or AccountAnonymizer")

	// 权限装载器：Authz 存在时必须有权威来源，否则 nil Permissions 会被
	// 解释成 DB NULL=FULL。NewRouter 也会校验，这里提前给出装配错误。
	if d.Permissions == nil && d.DB == nil {
		if _, ok := d.Repo.(rbac.PermissionLoader); !ok {
			missing = append(missing, "Permissions or an AuthRepo implementing Load")
		}
	}

	if len(missing) > 0 {
		return errors.New("httpapi: incomplete gateway assembly: missing " + strings.Join(missing, ", "))
	}
	return nil
}
