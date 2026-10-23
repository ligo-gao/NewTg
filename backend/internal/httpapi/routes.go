package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/billing"
	"newtg/gateway/internal/configcenter"
	"newtg/gateway/internal/notify"
	"newtg/gateway/internal/perfmetrics"
	"newtg/gateway/internal/plugin"
	"newtg/gateway/internal/rbac"
	"newtg/gateway/internal/store"
)

// RouterConfig 是管理面 HTTP 路由的装配输入。必填 Redis（会话/失败锁定/
// 邮箱验证码都依赖它）与 Sessions（或 SessionSecret）；Repo 与 DB 二选一，
// 单测注入内存 Repo，生产注入 *sql.DB 由 SQLAuthRepo 落地。
type RouterConfig struct {
	DB *sql.DB
	// LogDB 是日志库（usage_logs / audit_logs）。与 DB 同时提供时
	// NewRouter 挂载 M6 的只读日志/统计/审计端点；nil 时跳过
	// （纯内存装配的单元测试没有日志表）。
	LogDB    *sql.DB
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
	// EmailSender 实现 Configured 且返回 false（或为 nil）时，邮箱验证码等
	// 事务性邮件端点 fail-closed 503（email_unavailable），不再静默 204。
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
	// TaskPluginHandlers 是 M6d 任务插件管理端点（插件/版本/市场/dryrun）。
	// 非 nil 时挂到受保护管理子树；handler 内部按「读 admin、写
	// settings:manage」做功能级判定（R-M6D-02）。
	TaskPluginHandlers *TaskPluginHandlers
	// PricingHandlers 是 M3 定价管理端点（modules-06 F-06-13/14/16）。
	// 非 nil 时挂到受保护管理子树；装配方负责构造 Repo 与字段策略。
	// 定价端点的路径族是 /api/model-pricing 与 /api/group-pricing——不得复用
	// 公开目录 /api/pricing（modules-06 G-5 命名空间裁决）。
	PricingHandlers *PricingHandlers
	// SubscriptionHandlers 是 D5b 订阅套餐端点。nil 且 DB 非 nil 时由
	// NewRouter 自动构造（store.NewSubscriptionRepo + users 偏好读写）；
	// 单元测试可显式注入。
	SubscriptionHandlers *SubscriptionHandlers
	// SubscriptionPurchase 是订阅购买（S3）的支付接缝；生产为
	// *billing.TopupService（与充值共用同一渠道实现）。nil 时购买端点
	// fail closed 返回 503，而不是静默建单。
	SubscriptionPurchase SubscriptionPurchaseService
	// AnnouncementHandlers 是 D7a 站点公告端点（modules-08 F-08-04）。
	// nil 且 DB 非 nil 时由 NewRouter 自动构造：仓储是 store.NewAnnouncementRepo，
	// Webhook 发送器从配置中心读取 notify.webhook.*（未配置时静默跳过），
	// 站点基址取 site.server_address。单元测试可显式注入。
	AnnouncementHandlers *AnnouncementHandlers
	// NotifyService 是通知模板/投递服务（M6c + A5）。非 nil 时公告发布
	// 会为每个启用用户生成 `system.announcement` 站内通知。生产由
	// assembly 传入（启动期已播种模板）；nil 且 DB 非 nil 时由 NewRouter
	// 构造一个未播种的实例（投递仍会落库，模板渲染失败只降级 body）。
	NotifyService *notify.Service
	// NotificationHandlers 是 A5 用户侧站内通知读取端点。nil 且 DB 非 nil
	// 时由 NewRouter 自动构造并挂载。
	NotificationHandlers *NotificationHandlers
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
	// SignupWebhook 是注册成功后的 user_signup 事件生产者（B1b）。
	// 生产为 *notify.WebhookEventProducer（与充值事件共用订阅过滤）；
	// nil 时注册照常完成但不写 Webhook 投递。
	SignupWebhook SignupWebhookProducer

	// PublicPricingRepo 是公开读面（GET /api/pricing、/api/home/stats）的
	// 存储接缝。nil 且 DB 非 nil 时由 store.NewPublicPricingRepo(DB) 构造；
	// 两者都为空时不挂载公开读面（纯内存 Repo 的单元装配没有定价表）。
	// 公开路由与受保护路由共享同一 DB，但只读可见集合，不触碰内部字段。
	PublicPricingRepo store.PublicPricingRepo
	// PublicUserGroupLookup 是软鉴权读取 users.group 的接缝；nil 且 DB 非 nil
	// 时使用 SQL 实现。缺失只影响「登录用户的 group_price」，不影响公开访问。
	PublicUserGroupLookup PublicUserGroupLookup
	// PerfMetricsSource 是 F-05-08 模型性能看板的查询接缝
	// （platform-05 §3.5）。nil 表示 PERF_METRICS_ENABLED=false 或采集管道
	// 未装配，端点返回「采集未启用/不可用」闸门，绝不用 usage_logs 伪造。
	PerfMetricsSource perfmetrics.Source
	// PerfMetricsFlushInterval 是采集刷写周期，供看板的 3× 停摆判定使用。
	// 零值时 handler 取设计默认 5 分钟。
	PerfMetricsFlushInterval time.Duration
	// RankingsRepo 是公开排行榜（GET /api/rankings）的快照只读接缝。
	// nil 且 DB 非 nil 时由 store.NewRankingsRepo(DB) 构造；两者都为空时
	// 不挂载端点（纯内存装配没有 rankings_* 表）。
	RankingsRepo store.RankingsRepo
	// UserGroups 是 GET /api/status 的 usable_groups 查询接缝（Task 13）。
	// nil 且 DB 非 nil 时由 store.NewUserGroupsRepo(DB) 构造；两者都为空时
	// 该字段恒为空数组（纯内存装配的单元测试）。
	UserGroups UsableGroupLookup
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
	// tasks 是任务数据面 handler（M6d Task 6a）。非 nil 时 platform 的
	// mountRelayRoutes 把它挂到 /v1 令牌链（与中继共用 RelayAuth）。
	tasks *TasksHandler
	// mj 是绘图任务族提交 handler（W8/MJ-a2）。非 nil 时 platform 把它挂到
	// 根 mux 的 /mj/*（含别名重写），与中继共用 RelayAuth。
	mj *MJHandler
	// handler 是 mux 外层的统一响应策略（/api/ 默认 no-store）。在 NewRouter
	// 构造时包住 mux 指针：之后追加的路由仍会经过该层。
	handler http.Handler
	// notifyService 是本次装配实际使用的通知服务（A5）。装配测试据此断言
	// 生产路径传入的是已在启动期播种的实例，而不是 NewRouter 的兜底实例。
	notifyService *notify.Service
	// notifyFallback 记录 NewRouter 是否自行构造了未播种的兜底服务。
	// BuildGatewayRouter 必须在传入前完成构造与播种；该标志为 true 表示
	// 装配顺序回归（A5 收口 MEDIUM-1）。
	notifyFallback bool
	// pluginRuntime 是插件注册表热刷新/逐副本状态协调器（G-M6D-04/05/21）。
	// BuildGatewayRouter 在 DB/Redis 装配分支中构造；nil 表示纯内存装配。
	pluginRuntime *plugin.RuntimeCoordinator
}

// PluginRuntime 返回插件运行时协调器；未装配时为 nil。
// platform 在 HTTP 监听前调用 Start，停机时调用 Stop（DEL 本实例 key）。
func (r *Router) PluginRuntime() *plugin.RuntimeCoordinator {
	if r == nil {
		return nil
	}
	return r.pluginRuntime
}

// TasksHandler 返回任务数据面 handler；未装配（无 DB 的单测）时为 nil。
func (r *Router) TasksHandler() *TasksHandler {
	if r == nil {
		return nil
	}
	return r.tasks
}

// MJHandler 返回绘图任务族提交 handler；未装配（无 DB 的单测）时为 nil。
func (r *Router) MJHandler() *MJHandler {
	if r == nil {
		return nil
	}
	return r.mj
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
	h.signupWebhook = cfg.SignupWebhook
	h.initialized = cfg.Initialized
	h.geoIP = cfg.GeoIP
	// usable_groups 与默认模型校验共用同一 Sql 口径（store.UserGroupsRepo）。
	// 只有提供了 DB 才接线；纯 Repo 单测没有分组/渠道表，保持空数组语义。
	h.usableGroups = cfg.UserGroups
	if h.usableGroups == nil && cfg.DB != nil {
		h.usableGroups = store.NewUserGroupsRepo(cfg.DB)
	}
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
	// 全局 X-Request-ID 在最外层：公开/管理全部 /api/* 响应（含 401/404/500）
	// 都带同一个 id；no-store 紧随其后，二者都不依赖具体 handler。中继 /v1
	// 不走本 Router，由 RelayContext 自行生成，互不冲突（R-PUB-10）。
	//
	// 演示站只读守卫放在 mux 之前、SessionAuth 链之外（modules-08 §3.17 Q1）：
	// 它必须覆盖公开端点（register/email-code 等）与受保护子树在内的**全部**
	// /api/* 写方法；放在链内会漏掉公开端点，放在 handler 内会漏掉未来新增
	// 路由。关闭时原样透传。
	rt.handler = RequestIDMiddleware(noStoreAPI(DemoReadOnlyGuard(cfg.Config)(rt.mux)))

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

	// 订阅套餐端点（D5b；billing-08 §15）。与定价同属受保护子树；订阅表没有
	// 内存兜底，只有提供 DB 时才自动挂载。用户的 billing_preference 存在
	// users 行，因此 DB 是构造前提（Repo 只是订阅表接缝）。
	if cfg.SubscriptionHandlers == nil && cfg.DB != nil {
		// S4 支付令牌/自动续费（P-14）：需要 credential SecretSealer 才能把
		// 令牌加密落库。未配置 Sealer 时不装配 service，相关端点 fail closed
		// 503，而不是把令牌明文存进数据库。
		var paymentMethods SubscriptionPaymentMethodService
		if cfg.Sealer != nil {
			service, err := billing.NewPaymentMethodService(billing.PaymentMethodServiceConfig{
				Repo:   store.NewPaymentMethodRepo(cfg.DB),
				Subs:   store.NewSubscriptionRepo(cfg.DB),
				Config: cfg.Config,
				Sealer: cfg.Sealer,
				Logger: cfg.Logger,
			})
			if err != nil {
				return nil, err
			}
			paymentMethods = service
		}
		subscriptionHandlers, err := NewSubscriptionHandlers(SubscriptionHandlerConfig{
			Repo:           store.NewSubscriptionRepo(cfg.DB),
			DB:             cfg.DB,
			Redis:          cfg.Redis,
			Authz:          cfg.Authz,
			Audit:          cfg.Audit,
			Logger:         cfg.Logger,
			Purchase:       cfg.SubscriptionPurchase,
			PaymentMethods: paymentMethods,
		})
		if err != nil {
			return nil, err
		}
		cfg.SubscriptionHandlers = subscriptionHandlers
	}
	if cfg.SubscriptionHandlers != nil {
		cfg.SubscriptionHandlers.Register(rt.protected)
	}

	// 站点公告端点（D7a；modules-08 F-08-04）。公告表没有内存兜底，只有
	// 提供 DB 时才自动挂载；Webhook 发送器从配置中心快照读取，未配置时
	// 发布副作用静默跳过（R-D7-09）。
	if cfg.AnnouncementHandlers == nil && cfg.DB != nil {
		// 直连 NewRouter 的装配（单测/嵌入式）没有 assembly 的启动播种；
		// 仍构造一个 service，让投递记录照常落库（模板缺失只降级 body）。
		if cfg.NotifyService == nil {
			notifyStore, storeErr := notify.NewStore(cfg.DB)
			if storeErr != nil {
				return nil, storeErr
			}
			cfg.NotifyService, storeErr = notify.NewService(notify.ServiceConfig{Store: notifyStore})
			if storeErr != nil {
				return nil, storeErr
			}
			rt.notifyFallback = true
		}
		announcementHandlers, err := NewAnnouncementHandlers(AnnouncementHandlerConfig{
			Repo:     store.NewAnnouncementRepo(cfg.DB),
			Redis:    cfg.Redis,
			Authz:    cfg.Authz,
			Audit:    cfg.Audit,
			Logger:   cfg.Logger,
			Webhook:  notify.NewWebhookSender(cfg.Config, cfg.Sealer),
			Notifier: cfg.NotifyService,
			// F-08-18：公告 title/body 在落库前按 sec.sensitive.* 过滤。
			Config: cfg.Config,
			// 与 assembly 的基址回退链一致：PUBLIC_BASE_URL → site.server_address。
			// cfg.PublicBaseURL 在生产装配中已由 effectivePublicBaseURL 解析过，
			// 这里再读一次 site.server_address 是为了直连 NewRouter 的装配
			// （单测/嵌入式）也遵守同一条链。
			BaseURL: announcementBaseURL(cfg.Config, cfg.PublicBaseURL),
		})
		if err != nil {
			return nil, err
		}
		cfg.AnnouncementHandlers = announcementHandlers
	}
	if cfg.AnnouncementHandlers != nil {
		cfg.AnnouncementHandlers.Register(rt.protected)
		// D7b 公开只读详情：挂根 mux（精确模式先于 `/api/` 子树命中），
		// 匿名可访问且不经过 SessionAuth/CSRF/Origin 链。
		cfg.AnnouncementHandlers.RegisterPublic(rt.mux)
	}

	// 站内通知读取（A5；modules-08 §3.9.1 system.announcement）。投递记录
	// 在 notification_deliveries，与公告表分离；只有提供 DB 时才挂载。
	if cfg.NotificationHandlers == nil && cfg.DB != nil {
		notifyService := cfg.NotifyService
		if notifyService == nil {
			notifyStore, err := notify.NewStore(cfg.DB)
			if err != nil {
				return nil, err
			}
			notifyService, err = notify.NewService(notify.ServiceConfig{Store: notifyStore})
			if err != nil {
				return nil, err
			}
			rt.notifyFallback = true
		}
		handlers, err := NewNotificationHandlers(NotificationHandlerConfig{
			Service: notifyService,
			Authz:   cfg.Authz,
			Logger:  cfg.Logger,
		})
		if err != nil {
			return nil, err
		}
		cfg.NotificationHandlers = handlers
	}
	if cfg.NotificationHandlers != nil {
		cfg.NotificationHandlers.Register(rt.protected)
	}

	// 记录本次装配实际使用的通知服务：公告 handler 与用户侧读取端点在同一
	// 段装配中共享它（A5）。notifyFallback 为 true 时是 NewRouter 的兜底
	// 实例——生产装配（BuildGatewayRouter）必须在调用前完成构造与播种。
	rt.notifyService = cfg.NotifyService

	// 模型目录 / 供应商目录（modules-06 F-06-22～26；D1b）。目录没有内存兜底：
	// 只有提供 DB 时才挂载，避免纯内存装配用空数据伪造 200（与定价接缝一致）。
	// 派生字段读取面直接查 channels/user_groups（固定条数查询，非逐行）；
	// 上游同步地址从配置中心 model_sync.source_url 读取，未配置时 preview 503。
	if cfg.DB != nil {
		catalogHandlers, err := NewModelCatalogHandlers(ModelCatalogHandlerConfig{
			Repo:       store.NewModelCatalogRepo(cfg.DB),
			Redis:      cfg.Redis,
			Authz:      cfg.Authz,
			Audit:      cfg.Audit,
			Logger:     cfg.Logger,
			Derived:    NewSQLCatalogDerived(cfg.DB),
			SyncSource: NewModelSyncSourceFromConfig(cfg.Config),
		})
		if err != nil {
			return nil, err
		}
		catalogHandlers.RegisterModelCatalogRoutes(rt.protected, nil)
	}

	// 任务插件管理端点（M6d Task 5）：与其余管理面共用受保护链。
	if cfg.TaskPluginHandlers != nil {
		cfg.TaskPluginHandlers.Register(rt.protected)
		// URL 导入是独立 handler 文件（G-M6D-10）；在 Register 之外挂载，
		// 避免与 runtime status 的共享段互相影响。
		cfg.TaskPluginHandlers.RegisterTaskPluginImport(rt.protected)
	}

	// 性能管理端点（modules-08 §3.19 / F-08-48～50；D3a）。三个对象都是
	// 进程本地的，不依赖主库；日志库只用于「最近操作」的审计查询，缺省时
	// 该端点返回空列表而不是拒绝挂载。六个端点统一要求 settings:manage。
	NewPerformanceHandlers(PerformanceHandlerConfig{
		LogDB:  cfg.LogDB,
		Audit:  cfg.Audit,
		Logger: cfg.Logger,
		Redis:  cfg.Redis,
		Authz:  cfg.Authz,
	}).Register(rt.protected)

	// M6 只读日志/统计/审计端点：日志库是数据源、主库提供用户名与权限
	// 回源。两者缺一时不挂载（部署配置缺失应在启动期由日志库健康检查暴露，
	// 而不是让这里用空数据伪造 200）。
	if cfg.LogDB != nil && cfg.DB != nil {
		// 只读按 key 查询（F-05-11）需要一条**宽松**的令牌解析接缝：
		// Resolve 会拒绝过期/耗尽令牌，只读面只拒绝禁用。优先从令牌服务
		// 的具体实现取容量（生产 *tokenstore.Store），没有就保持 nil——
		// 端点会返回 503，而不是悄悄退回严格口径把过期令牌判 401。
		var readOnly auth.ReadOnlyTokenLookup
		if ro, ok := cfg.TokenStore.(auth.ReadOnlyTokenLookup); ok {
			readOnly = ro
		}
		if readOnly == nil {
			if ro, ok := cfg.TokenService.(auth.ReadOnlyTokenLookup); ok {
				readOnly = ro
			}
		}
		usageLogs, err := NewUsageLogHandlers(UsageLogHandlerConfig{
			DB:             cfg.DB,
			LogDB:          cfg.LogDB,
			Audit:          cfg.Audit,
			Logger:         cfg.Logger,
			ReadOnlyTokens: readOnly,
			Redis:          cfg.Redis,
			// PERF_METRICS_ENABLED=false 时 assembly 不注入 source（nil），
			// 端点走显式闸门；周期始终传入，供 3× 停摆判定使用。
			Perf:              cfg.PerfMetricsSource,
			PerfFlushInterval: cfg.PerfMetricsFlushInterval,
		})
		if err != nil {
			return nil, err
		}
		usageLogs.RegisterUsageLogRoutes(rt.protected, nil)
		// 只读面必须绕过 SessionAuth：过期/额度耗尽的令牌仍可查自己的明细。
		// 挂根 mux 的精确模式，与 GET /api/usage/token 同挂载方式（D2-01）。
		usageLogs.RegisterLogsByKeyRoute(rt.mux)
	}

	// 公开端点：精确注册，先于 /api/ 子树（ServeMux 按特异性择路，顺序无关，
	// 这里仍先注册以免读者误判）。
	rt.mux.HandleFunc("GET /api/status", h.handleStatus)
	// 公开地理区域：精确模式挂根 mux，绕过 SessionAuth（匿名恒 200）。
	rt.mux.HandleFunc("GET /api/geoip", h.handleGeoIP)

	// 公开读面（modules-09；M6c Task 7）：首页统计与定价目录。
	// 与上面的精确端点同属「绕过 SessionAuth」的根 mux 路由；个性化分组价由
	// handler 内部的软鉴权解析，匿名请求绝不 401。
	publicRepo := cfg.PublicPricingRepo
	if publicRepo == nil && cfg.DB != nil {
		publicRepo = store.NewPublicPricingRepo(cfg.DB)
	}
	if publicRepo != nil {
		userGroups := cfg.PublicUserGroupLookup
		if userGroups == nil && cfg.DB != nil {
			userGroups = &sqlPublicUserGroupLookup{db: cfg.DB}
		}
		homeStats, err := NewHomeStatsHandlers(HomeStatsHandlerConfig{
			Repo: publicRepo, Redis: cfg.Redis, Logger: cfg.Logger,
		})
		if err != nil {
			return nil, err
		}
		homeStats.RegisterHomeStatsRoutes(rt)
		publicPricing, err := NewPublicPricingHandlers(PublicPricingHandlerConfig{
			Repo: publicRepo, Config: cfg.Config, Sessions: cfg.Sessions,
			UserGroups: userGroups, Redis: cfg.Redis, Logger: cfg.Logger,
			Metrics: publicModelMetricsSource(cfg.LogDB),
		})
		if err != nil {
			return nil, err
		}
		publicPricing.RegisterPublicPricingRoutes(rt)
	}

	// 公开排行榜（D6b；modules-09 F-09-29～F-09-33）。快照表没有内存兜底：
	// 只有提供 DB 时才自动挂载（与定价/目录同一原则，绝不用空数据伪造 200）。
	// Redis 是共享缓存，缺失时端点仍可回源快照表，只是少一层加速。
	rankingsRepo := cfg.RankingsRepo
	if rankingsRepo == nil && cfg.DB != nil {
		rankingsRepo = store.NewRankingsRepo(cfg.DB)
	}
	if rankingsRepo != nil {
		rankingsHandlers, err := NewRankingsHandlers(RankingsHandlerConfig{
			Repo:   rankingsRepo,
			Redis:  cfg.Redis,
			Config: cfg.Config,
			Logger: cfg.Logger,
		})
		if err != nil {
			return nil, err
		}
		rankingsHandlers.RegisterRankingsRoutes(rt)
	}

	// 首页 live_feed（D6b；modules-09 F-09-02）。事件源是计费写入方投递到
	// Redis 的脱敏事件；没有 Redis 就没有真实数据源，因此不提供内存兜底。
	liveFeedHandlers, err := NewLiveFeedHandlers(LiveFeedHandlerConfig{
		Redis:  cfg.Redis,
		Config: cfg.Config,
		Logger: cfg.Logger,
	})
	if err != nil {
		return nil, err
	}
	liveFeedHandlers.RegisterLiveFeedRoutes(rt)

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
// 因此会绕过 SessionAuth 链——只允许公开只读端点或自带凭据语义的端点使用
// （如公开定价/公告详情、GET /api/usage/token 的令牌自读用量）。
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

// announcementBaseURL 返回公告 Webhook payload 使用的站点基址，回退链与
// assembly 一致：PUBLIC_BASE_URL（cfg.PublicBaseURL，可逗号分隔多个 → 取
// 第一个）→ site.server_address → 空串（发送侧据此降级为站内相对路径，
// R-D7-09）。注意：生产装配时 BuildGatewayRouter 会把启动时解析出的基址作为
// publicBaseURL 传入，因此一旦启动时已确定，运行期修改 site.server_address
// 不会生效（需重启）；仅启动时为空、随后由安装向导写入的场景才动态读取。
func announcementBaseURL(config ConfigReader, publicBaseURL string) func() string {
	envBase := ""
	if origins := parseAllowedOrigins(publicBaseURL); len(origins) > 0 {
		envBase = strings.TrimRight(origins[0], "/")
	}
	return func() string {
		if envBase != "" {
			return envBase
		}
		if config == nil {
			return ""
		}
		raw, ok := config.Get("site.server_address")
		if !ok {
			return ""
		}
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return ""
		}
		return strings.TrimSpace(value)
	}
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

// publicModelMetricsSource 是公开性能接口的装配接缝：只有提供日志库时
// 才挂载真实聚合；缺失时 handler 返回 503，绝不返回伪造数据。
func publicModelMetricsSource(logDB *sql.DB) store.PublicModelMetricsSource {
	if logDB == nil {
		return nil
	}
	return store.NewPublicModelMetricsRepo(logDB)
}
