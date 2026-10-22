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
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/billing"
	"newtg/gateway/internal/billing/pricing"
	"newtg/gateway/internal/configcenter"
	"newtg/gateway/internal/contract"
	deploybilling "newtg/gateway/internal/deploy/billing"
	"newtg/gateway/internal/deploy/ionet"
	"newtg/gateway/internal/egress"
	"newtg/gateway/internal/mj"
	"newtg/gateway/internal/notify"
	"newtg/gateway/internal/perfmetrics"
	"newtg/gateway/internal/plugin"
	"newtg/gateway/internal/rbac"
	"newtg/gateway/internal/settings"
	"newtg/gateway/internal/store"
	"newtg/gateway/internal/tasks"
	"newtg/gateway/internal/updatecheck"
	"newtg/gateway/internal/version"
	"newtg/gateway/internal/worker"
	"newtg/gateway/plugins"
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
	// RateLimiter 是中继/任务提交的准入限流接缝（W7/M5-b）。生产由 platform
	// 用同一个 Redis 客户端构造一次；nil 时不限流（单测装配没有 Redis，
	// 不能把「未装配」变成「每个请求 500」）。
	RateLimiter RelayRateLimiter
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
	// SignupWebhook 是注册成功后的 user_signup 事件生产者（B1b）。生产由
	// platform 用主库 Store + WebhookSender 构造并传入；nil 时不入队。
	SignupWebhook SignupWebhookProducer
	Logger        *slog.Logger

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

	// SubscriptionPurchase 是订阅购买（S3）的支付接缝。生产装配传
	// TopupHandlers.Service()，使购买与充值共用同一个 TopupService
	// （同一渠道配置/验签/幂等实现）。nil 时购买端点返回 503。
	SubscriptionPurchase SubscriptionPurchaseService

	// WalletHandlers 是 M5 Task 6 补齐的 /api/wallet/summary（modules-03
	// §F-03-01）。非 nil 时挂到受保护子树，只读取当前会话用户自己的余额。
	WalletHandlers *WalletHandlers

	// CheckinService 是每日签到服务（W15-C / modules-03 F-03-12）。非 nil 时
	// 挂载 POST /api/checkin 与 GET /api/checkin/calendar；nil 时不挂载
	// （纯 Repo 单测没有 checkins 表）。服务本身由 platform 用主库 GORM
	// 句柄构造，保证签到行与账本同事务。
	CheckinService *billing.CheckinService

	// Turnstile siteverify 测试接缝（生产留零值）。
	TurnstileVerifyURL  string
	TurnstileHTTPClient *http.Client
	// TurnstileSettings 覆盖由配置中心派生的设置读取（测试接缝）。nil 时
	// 使用 TurnstileSettingsProvider(Config, Sealer)；非 nil 时完全以它为准，
	// 便于单测注入「开启 + 固定密钥」而无需真实 sealer。
	TurnstileSettings func() auth.TurnstileSettings

	// SettingsBroadcaster 是设置写成功后的配置广播接缝（生产为 configcenter.Center）。
	// nil 时只写库不广播——单测装配可接受，生产装配必须传入。
	SettingsBroadcaster settings.Broadcaster
	// DeployProvider 是部署控制面的测试接缝（按生效密钥构造 provider）。
	// nil 时使用固定 host 的 ionet 客户端；生产不注入。
	DeployProvider func(apiKey string) ionet.DeploymentProvider
	// TaskPluginStore 是插件管理仓储的可选测试接缝；nil 且 DB 非 nil 时由
	// store.NewTaskPluginRepo(DB) 构造。
	TaskPluginStore TaskPluginStore
	// PluginManager 是任务插件运行时的可选测试接缝；nil 时由内置插件源 +
	// SQLVersionLoader 构造。装配完成后由 BuildGatewayRouter 启动即 Reload。
	PluginManager *plugin.Manager
	// TaskPluginAccess 是上传端点 settings:manage 的权限回源接缝；nil 且
	// DB 非 nil 时使用 store.NewTokenRepo(DB)。
	TaskPluginAccess UserAccessStore

	// NodeName / StartedAt 供 GET /api/system/status 的实例信息使用。
	NodeName string
	// InstanceID 是逐副本心跳与 GET /api/instances 的实例标识
	// （INSTANCE_ID → hostname）。空时回退 NodeName。
	InstanceID string
	StartedAt  time.Time

	// Billing 为每次 /v1 任务提交构造计费会话（M6d Task 6a）。与中继共用
	// 同一个 engine.NewSession；nil 时任务路径退回 billingseam.NewStub
	// （纯内存单测），生产装配必须传入。
	Billing func(*contract.RelayContext) contract.BillingSession
	// PerfMetricsSource 是 F-05-08 模型性能看板的查询接缝（platform-05
	// §3.5）。生产由 platform 在 PERF_METRICS_ENABLED=true 时注入采集器；
	// false/未装配时为 nil，端点走显式闸门。
	PerfMetricsSource perfmetrics.Source
	// PerfMetricsFlushInterval 是采集刷写周期（PERF_METRICS_FLUSH_INTERVAL），
	// 供看板 3× 停摆判定。
	PerfMetricsFlushInterval time.Duration
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
	turnstileSettings := deps.TurnstileSettings
	if turnstileSettings == nil {
		var err error
		turnstileSettings, err = TurnstileSettingsProvider(deps.Config, deps.Sealer, deps.Logger)
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

	// 任务插件管理端点（M6d Task 5 / modules-08 §3.10）。运行时 Manager 与
	// 管理面共用同一个 DB 版本加载器：写路径提交后调用 Reload，快照与 DB
	// 立即一致；30s 轮询/pub-sub 属后续装配项（T3 遗留，已登记）。
	var taskPluginHandlers *TaskPluginHandlers
	// pluginRuntime 是插件注册表热刷新/逐副本状态协调器；非 nil 时由
	// Router 暴露给 platform 启动，并在停机时 DEL 本实例 key。
	var pluginRuntime *plugin.RuntimeCoordinator
	// taskDataHandlers 是 M6d Task 6a 的任务数据面（/v1/tasks* 与
	// /api/task*）。与插件运行时共用同一个 Manager 快照，避免管理面停用
	// 插件后数据面仍从旧注册表接单。
	var taskDataHandlers *TasksHandler
	// mjHandler 是 W8/MJ-a2 的绘图任务族提交面（根 mux `/mj/*`）。与任务
	// 数据面共用同一个 tasks.Repo（同一连接面/同一列语义），计费会话工厂
	// 也与 /v1/tasks 共用 deps.Billing（预扣/退款只有一套实现）。
	var mjHandler *MJHandler
	if deps.DB != nil {
		var err error
		pluginStore := deps.TaskPluginStore
		if pluginStore == nil {
			pluginStore = store.NewTaskPluginRepo(deps.DB)
		}
		sources, sourceErr := plugins.TaskPluginSources()
		if sourceErr != nil {
			// 内置插件是构建期产物；embed 目录不可读说明构建被破坏，
			// 必须阻止上线（relay-06 §2.1）。
			return nil, sourceErr
		}
		manager := deps.PluginManager
		constructed := manager == nil
		if constructed {
			manager, err = plugin.NewManager(sources, plugin.NewSQLVersionLoader(deps.DB), plugin.Options{
				Node:                deps.NodeName,
				MasterEnabled:       readConfigBool(deps.Config, "plugin.enabled", true),
				DisabledFactoryKeys: readConfigStringList(deps.Config, "plugin.disabled_factory_keys"),
			})
			if err != nil {
				// 内置插件编译/校验失败 = 构建期缺陷，启动失败而不是带残缺注册表上线。
				return nil, fmt.Errorf("httpapi: load built-in task plugins: %w", err)
			}
		}

		// 内置插件入库（MEDIUM-2）：列表/详情/启停都读 B-11；没有行时
		// 「已安装」永远为空，即便 factory 层正常运行。upsert 幂等，只维护
		// 内置事实（source/builtin/name/description），不动 enabled 与覆盖层
		// 的 active_version。
		startupCtx, cancelStartup := context.WithTimeout(context.Background(), 10*time.Second)
		builtins := make([]store.BuiltinPlugin, 0, len(sources))
		for _, key := range sortedPluginKeys(sources) {
			compiled, compileErr := plugin.Compile(startupCtx, sources[key], plugin.CompileOptions{
				ExpectedKey: key, Layer: plugin.LayerFactory,
			})
			if compileErr != nil {
				cancelStartup()
				return nil, fmt.Errorf("httpapi: compile built-in task plugin %q: %w", key, compileErr)
			}
			builtins = append(builtins, store.BuiltinPlugin{
				Key: key, Name: compiled.Manifest().Name,
				Icon:        stringPtr(compiled.Manifest().Icon),
				Description: stringPtr(chooseDescription(compiled.Manifest().Description)),
			})
		}
		if len(builtins) > 0 {
			if upsertErr := pluginStore.UpsertBuiltinPlugins(startupCtx, builtins); upsertErr != nil {
				cancelStartup()
				return nil, fmt.Errorf("httpapi: upsert built-in task plugins: %w", upsertErr)
			}
		}

		if constructed {
			// 内置插件的 B-11 enabled=false 与设置禁用集共同决定 factory 层是
			// 否注册：重启后仍维持管理员通过 /status 关闭的内置插件。
			disabled := readConfigStringList(deps.Config, "plugin.disabled_factory_keys")
			if pluginsList, listErr := pluginStore.List(startupCtx); listErr == nil {
				for _, p := range pluginsList {
					if p.Builtin && !p.Enabled {
						disabled = append(disabled, p.Key)
					}
				}
			}
			manager.SetDisabledFactoryKeys(disabled)
		}
		// 启动即 Reload：DB 版本层/内置禁用集必须在监听端口前进入快照。
		// 加载失败不阻止启动——快照保留空/旧内容，runtime/status 暴露
		// reload_error，而不是让一次瞬时的 DB 抖动把整个网关下线。
		if reloadErr := manager.Reload(startupCtx); reloadErr != nil {
			if deps.Logger != nil {
				deps.Logger.Warn("httpapi: initial task plugin reload failed", "err", reloadErr)
			}
		}
		cancelStartup()

		access := deps.TaskPluginAccess
		if access == nil {
			access = store.NewTokenRepo(deps.DB)
		}
		// 插件运行时协调器（G-M6D-04/05/21）：30s 重载 + pub/sub 失效 +
		// 逐副本 runtime 状态。Redis 是必填依赖的真实装配；纯内存单测不进入
		// 本分支（deps.DB==nil），由 handler 降级为单副本投影。
		if deps.Redis != nil {
			pluginRuntime, err = plugin.NewRuntimeCoordinator(plugin.RuntimeCoordinatorConfig{
				Manager:    manager,
				Redis:      deps.Redis,
				InstanceID: deps.InstanceID,
				Node:       deps.NodeName,
				Role:       "gateway",
				Version:    version.Version,
				StartedAt:  deps.StartedAt,
				Logger:     deps.Logger,
				// 每次 Reload 前重算内置禁用集：B-11 enabled=false 不在
				// active 版本查询里，只在写路径重算会让其它副本收到广播后
				// 仍用旧禁用集（状态分裂）。
				RefreshDisabledFactoryKeys: func(ctx context.Context) ([]string, error) {
					disabled := readConfigStringList(deps.Config, "plugin.disabled_factory_keys")
					list, listErr := pluginStore.List(ctx)
					if listErr != nil {
						return nil, listErr
					}
					for _, p := range list {
						if p.Builtin && !p.Enabled {
							disabled = append(disabled, p.Key)
						}
					}
					return disabled, nil
				},
			})
			if err != nil {
				return nil, fmt.Errorf("httpapi: plugin runtime coordinator: %w", err)
			}
		}
		taskPluginHandlers, err = NewTaskPluginHandlers(TaskPluginHandlerConfig{
			Store:   pluginStore,
			Manager: manager,
			Config:  deps.Config,
			Authz:   deps.Authz,
			Access:  access,
			Audit:   deps.Audit,
			Logger:  deps.Logger,
			Idem:    NewRedisIdempotencyStore(deps.Redis),
			// 绑定渠道数实时读 channels（M6d Task 7a）；与渠道管理共用同一
			// 仓库口径（status=1 + 单绑/多绑命中）。
			Bindings: store.NewChannelRepo(deps.DB),
			// 写成功后广播失效；读取面聚合全部未过期副本。
			Changes: pluginRuntime,
			Runtime: pluginRuntime,
		})
		if err != nil {
			return nil, err
		}

		// 任务插件数据面的统一出站策略（relay-06 §5.2 / security-04 §2.4）：
		// 提交、查询、轮询、产物内容与 dryrun 共用 sec.ssrf.* 的同一份快照。
		// 任务面另叠加「渠道 base URL + meta.allowedHosts」允许清单（见
		// tasks.PolicyForHosts）；dryrun 用同一策略读取面。
		tasks.SetOutboundPolicy(egress.PolicyFromConfig(deps.Config))
		plugin.ConfigureEgress(deps.Config)

		// 任务数据面（M6d Task 6a）：渠道选择与任务行共用一个 tasks.Repo
		// 连接面；凭据 Sealer 与渠道管理同源（CREDENTIAL_ENCRYPTION_KEY）。
		taskRepo := tasks.NewRepo(deps.DB)
		taskDataHandlers = NewTasksHandler(TasksHandlerConfig{
			Store:    taskRepo,
			Channels: taskRepo,
			Plugins:  manager,
			Sealer:   NewSecretSealer(deps.CredentialEncryptionKey),
			Tokens:   store.NewTokenRepo(deps.DB),
			Billing:  deps.Billing,
			// 产物能力 URL 的签名密钥从 CREDENTIAL_ENCRYPTION_KEY 派生
			// （T6b brief：不新增明文配置）；轮换该键即撤销所有已签发 URL。
			ArtifactSigner: tasks.NewArtifactSigner(deps.CredentialEncryptionKey, 0),
			PublicBaseURL:  deps.PublicBaseURL,
			// T5：任务计价表达式。插件覆盖表来自设置中心快照；第 4 级
			// （映射模型）回源 model_pricing。两者缺失都只是「没有表达式」，
			// 结算沿用按预扣额结清。
			BillingSetting: func() pricing.BillingSetting {
				return pricing.BillingSettingFromConfig(deps.Config)
			},
			ModelExpr: TaskModelExprLookup(deps.DB),
			// W7/M5-b：提交面与中继共用同一 limiter/config（同一次装配）。
			RateLimiter: deps.RateLimiter,
			Config:      deps.Config,
		})

		// W8/MJ-a2：绘图任务族提交面。与任务数据面共用 taskRepo 与 Sealer；
		// 计费会话工厂同样复用 deps.Billing（engine.NewSession），保证
		// 「提交 hold → 终态结算/退款」只有一套资金实现。
		mjHandler = NewMJHandler(MJHandlerConfig{
			Store:       taskRepo,
			Channels:    taskRepo,
			Sealer:      NewSecretSealer(deps.CredentialEncryptionKey),
			Tokens:      store.NewTokenRepo(deps.DB),
			Billing:     deps.Billing,
			Config:      deps.Config,
			RateLimiter: deps.RateLimiter,
			// W8/MJ-d：提交幂等键表（R-MJ-06 第二步）。与 tasks 共用同一
			// 仓储/连接池；缺失会让携带 Idempotency-Key 的提交 fail-closed。
			Idempotency: taskRepo,
			// 图片签名从 CREDENTIAL_ENCRYPTION_KEY 派生，但用独立域前缀
			// newtg/mj-image/v1（R-MJ-09）：轮换主密钥即撤销全部已签发
			// 图片 URL，且与产物票据互不重放。不新增必填 env。
			ImageSigner: mj.NewImageSigner(deps.CredentialEncryptionKey),
			// 图片签名/归属失败的 best-effort 安全审计（security-02 §2.4）。
			Audit: deps.Audit,
		})
	}

	// 通知模板/投递服务（M6c Task 4 + A5）必须在 NewRouter **之前**构造并
	// 播种：公告 handler 与用户侧读取端点都在 NewRouter 内部装配，共用同一个
	// 实例。若这里传 nil，NewRouter 会为直连装配兜底构造一个**未播种**的服务，
	// 生产公告投递就会用临时实例（模板缺失只降级 body，且审计计数来自另一个
	// store）——这正是 A5 收口 MEDIUM-1 修复的装配顺序缺陷。
	//
	// 播种失败阻止上线：模板行缺失会让所有通知 fail-closed。两个表的行都在
	// 主库（§3.9.2「独立表」），因此与其它 DB 装配一样只在 deps.DB 非 nil 时
	// 构造；纯 Repo 单测没有这两个表，保持 nil（公告发布不投递）。
	var notifyService *notify.Service
	if deps.DB != nil {
		notifyStore, err := notify.NewStore(deps.DB)
		if err != nil {
			return nil, err
		}
		notifyService, err = notify.NewService(notify.ServiceConfig{
			Store: notifyStore,
			// SMTP 配置读同一配置中心快照；密钥类 password 经同一 Sealer 解封。
			Sender: notify.NewSMTPSender(deps.Config, deps.Sealer),
			Logger: deps.Logger,
		})
		if err != nil {
			return nil, err
		}
		seedCtx, cancelSeed := context.WithTimeout(context.Background(), 10*time.Second)
		seedErr := notifyService.EnsureSeeded(seedCtx)
		cancelSeed()
		if seedErr != nil {
			return nil, seedErr
		}
	}

	rt, err := NewRouter(RouterConfig{
		DB:                  deps.DB,
		LogDB:               deps.LogDB,
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
		TaskPluginHandlers:  taskPluginHandlers,
		LoginLimiter:        limiter,
		LoginEncryption:     loginEncryption,
		TurnstileSettings:   turnstileSettings,
		TurnstileVerifyURL:  deps.TurnstileVerifyURL,
		TurnstileHTTPClient: deps.TurnstileHTTPClient,
		Audit:               deps.Audit,
		SignupGrantAlert:    deps.SignupGrantAlert,
		SignupWebhook:       deps.SignupWebhook,
		Logger:              deps.Logger,
		// 订阅购买与充值共用同一个支付服务实例。
		SubscriptionPurchase: deps.SubscriptionPurchase,
		NotifyService:        notifyService,
		// F-05-08：采集管道未启用时 source 为 nil，端点走闸门。
		PerfMetricsSource:        deps.PerfMetricsSource,
		PerfMetricsFlushInterval: deps.PerfMetricsFlushInterval,
	})
	if err != nil {
		return nil, err
	}

	rt.tasks = taskDataHandlers
	// platform 在监听前启动协调器（启动即 Reload），停机时通过 getter 停止
	// 并删除逐副本状态 key。
	rt.pluginRuntime = pluginRuntime
	// W8/MJ-a2：/mj/* 提交 handler 由 platform 挂到根 mux（令牌链），不挂在
	// /api 管理面，也不经过 SessionAuth/CSRF。
	rt.mj = mjHandler
	if taskDataHandlers != nil {
		// 台账端点复用管理面受保护链（SessionAuth → CSRF → Origin）。
		// /v1 提交/查询在 platform.mountRelayRoutes 挂到令牌链上。
		taskDataHandlers.RegisterAdmin(rt.protected)
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

	if deps.WalletHandlers != nil {
		deps.WalletHandlers.RegisterWalletRoutes(rt.protected, nil)
	}

	// W15-C：每日签到。handler 复用 NewRouter 已经构造的 Turnstile gate
	// 实例（第 5 个接入点），保证与登录/注册同一份配置快照与重放守卫。
	if deps.CheckinService != nil {
		checkinHandlers, err := NewCheckinHandlers(CheckinHandlerConfig{
			Service:   deps.CheckinService,
			Redis:     deps.Redis,
			Turnstile: rt.handlers.turnstile,
			Logger:    deps.Logger,
		})
		if err != nil {
			return nil, err
		}
		checkinHandlers.RegisterCheckinRoutes(rt.protected, nil)
	}

	// 系统设置读写（FE-08 基础设施）与系统状态。两者都依赖主库；纯 Repo 单测
	// 装配没有 system_settings 表，缺 DB 时跳过而不是挂一个必然 500 的端点。
	if deps.DB != nil {
		settingsService, err := settings.NewService(settings.ServiceConfig{
			DB:          deps.DB,
			Broadcaster: deps.SettingsBroadcaster,
			Sealer:      deps.Sealer,
			Logger:      deps.Logger,
		})
		if err != nil {
			return nil, err
		}
		settingsHandlers, err := NewSettingsHandlers(SettingsHandlerConfig{
			Service: settingsService,
			Authz:   deps.Authz,
			Audit:   deps.Audit,
			Idem:    NewRedisIdempotencyStore(deps.Redis),
			Logger:  deps.Logger,
			// F-08-20 测试事件：与投递进程共用同一配置快照与 Sealer，
			// 保证「测试通过」意味着真实投递的签名/目标配置可用。
			Webhook: notify.NewWebhookSender(deps.Config, deps.Sealer),
			Redis:   deps.Redis,
			// modules-08 §3.4：支付设置 Tab 的首次进入合规确认。持久化载体
			// 是 users.payment_compliance_confirmed_at（per-admin），审计面
			// 仍由 audit_logs 承担 who/when。
			Compliance: store.NewPaymentComplianceRepo(deps.DB),
		})
		if err != nil {
			return nil, err
		}
		settingsHandlers.Register(rt.protected)

		// 模型部署控制面设置（M6d Task 2 / modules-08 §3.20）：作用域化
		// 投影复用同一个 settings.Service 与 Sealer；provider host 固定，
		// 管理端不可配置。部署管理端点（T4）会复用同一 provider 客户端。
		deploySettingsHandlers, err := NewDeploySettingsHandlers(DeploySettingsHandlerConfig{
			Settings: settingsService,
			DB:       deps.DB,
			Sealer:   deps.Sealer,
			Provider: deps.DeployProvider,
			Authz:    deps.Authz,
			Audit:    deps.Audit,
			Redis:    deps.Redis,
			Logger:   deps.Logger,
		})
		if err != nil {
			return nil, err
		}
		deploySettingsHandlers.Register(rt.protected)

		// 模型部署控制面管理端点（M6d Task 4 / modules-08 §3.20）：
		// 与设置端点共用同一 settings.Service、Sealer 与 provider 工厂；
		// 幂等复用渠道/设置端点的 Redis 存储（不新建第二套）。
		deploySecrets := DeploySecretStore(missingSecretStore{})
		if deps.Sealer != nil {
			deploySecrets = &sqlDeploySecretStore{db: deps.DB, sealer: deps.Sealer}
		}
		// 部署计费账本（W3 H1/H2）：管理面写路径的 Hold/Release/SettleTerminated
		// 复用与 worker 同一个 DeploymentBillingLedger 实现（不另写记账）。
		// 账本只读写成本表，不触碰用户资金；provider 接缝留空——管理面只做
		// 本地预扣/结算，provider 观测由调用方即时传入。
		deployBillingLedger, err := deploybilling.NewLedger(deploybilling.LedgerConfig{
			DB:     deps.DB,
			Logger: deps.Logger,
		})
		if err != nil {
			return nil, fmt.Errorf("router: deploy billing ledger: %w", err)
		}
		deployHandlers, err := NewDeployManagementHandlers(DeployManagementHandlerConfig{
			Settings: settingsService,
			Secrets:  deploySecrets,
			Provider: deps.DeployProvider,
			Billing:  deployBillingLedger,
			Authz:    deps.Authz,
			Audit:    deps.Audit,
			Redis:    deps.Redis,
			Idem:     NewRedisIdempotencyStore(deps.Redis),
			Logger:   deps.Logger,
		})
		if err != nil {
			return nil, err
		}
		deployHandlers.Register(rt.protected)

		NewSystemStatusHandlers(SystemStatusConfig{
			DB:         deps.DB,
			Redis:      deps.Redis,
			NodeName:   deps.NodeName,
			InstanceID: deps.InstanceID,
			Role:       "gateway",
			StartedAt:  deps.StartedAt,
			Logger:     deps.Logger,
		}).Register(rt.protected)

		// 更新检查（modules-08 §3.15 F-08-42）：服务端代取版本清单。
		// Checker 读与全局配置相同的 ConfigReader；出站走 egress（sec.ssrf.*），
		// 结果只进 Redis 缓存，不落 PG、不写审计。Redis 必填——缺缓存会让
		// 每个管理员浏览器各自出站，违背 Q5。
		updateChecker, err := updatecheck.New(updatecheck.Config{
			Reader:         deps.Config,
			Redis:          deps.Redis,
			CurrentVersion: version.Version,
			Logger:         deps.Logger,
		})
		if err != nil {
			return nil, fmt.Errorf("router: update checker: %w", err)
		}
		NewUpdateCheckHandlers(UpdateCheckHandlerConfig{
			Checker: updateChecker,
			Logger:  deps.Logger,
		}).Register(rt.protected)

		// 逐副本实例列表（modules-08 §3.7 F-08-21）：与系统状态页共用
		// 同一 Redis 心跳源；权限 settings:manage，与性能页页头的实例标识
		// 同门。
		NewInstanceHandlers(InstanceHandlerConfig{
			Redis:      deps.Redis,
			InstanceID: deps.InstanceID,
			Role:       "gateway",
			Version:    version.Version,
			StartedAt:  deps.StartedAt,
			Authz:      deps.Authz,
			Logger:     deps.Logger,
		}).Register(rt.protected)

		// 系统任务管理面（W17-2 / billing-06 §13.5）：注册表是静态代码表，
		// 调度行与运行历史在主库。gateway 只读 + 受限触发；执行由 worker
		// 进程的调度器完成，触发只写 pending 行并通过 Redis 唤醒。
		taskRegistry, err := worker.DefaultRegistry()
		if err != nil {
			return nil, fmt.Errorf("router: worker task registry: %w", err)
		}
		taskStore, err := worker.NewSQLStore(deps.DB)
		if err != nil {
			return nil, fmt.Errorf("router: worker task store: %w", err)
		}
		taskService, err := worker.NewService(worker.ServiceConfig{
			Registry: taskRegistry,
			Store:    taskStore,
			Lease:    worker.NewRedisLease(deps.Redis),
			Logger:   deps.Logger,
			Wakeup: func(ctx context.Context, taskType string) error {
				return worker.PublishWakeup(ctx, deps.Redis, taskType)
			},
		})
		if err != nil {
			return nil, fmt.Errorf("router: worker task service: %w", err)
		}
		NewSystemTaskHandlers(SystemTaskHandlerConfig{
			Service: taskService,
			Authz:   deps.Authz,
			Audit:   deps.Audit,
			Idem:    NewRedisIdempotencyStore(deps.Redis),
			Logger:  deps.Logger,
		}).Register(rt.protected)

		// 通知模板（M6c Task 4 / modules-08 §3.9）：模板表是独立表，不在
		// system_settings 的 KV 里。notifyService 已在 NewRouter 之前构造并
		// 完成启动播种（ON CONFLICT DO NOTHING），这里只把同一个实例交给
		// 管理端点，避免模板视图与投递路径各持一个 store。
		notifyHandlers, err := NewNotificationTemplateHandlers(NotificationTemplateHandlerConfig{
			Service: notifyService,
			Authz:   deps.Authz,
			Audit:   deps.Audit,
			Logger:  deps.Logger,
		})
		if err != nil {
			return nil, err
		}
		notifyHandlers.Register(rt.protected)
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
	// /api/me 的 permissions（M6 R-17）必须使用与 Authz 相同的权限真源，
	// 否则生产装配下该字段会恒为 []。解析顺序与 NewRouter 一致：显式
	// Permissions → 实现 PermissionLoader 的 AuthRepo → SQLAuthRepo(DB)。
	sessionAccess := deps.Permissions
	if sessionAccess == nil {
		if loader, ok := deps.Repo.(rbac.PermissionLoader); ok {
			sessionAccess = loader
		}
	}
	if sessionAccess == nil && deps.DB != nil {
		sessionAccess = NewSQLAuthRepo(deps.DB)
	}
	sessions, err := NewSessionHandlers(SessionHandlerConfig{
		DB:            deps.DB,
		Repo:          selfRepo,
		Sessions:      deps.Sessions,
		Authz:         deps.Authz,
		Access:        sessionAccess,
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

// sortedPluginKeys 返回内置源 key 的稳定序（upsert 与编译顺序可复现）。
func sortedPluginKeys(sources map[string]string) []string {
	keys := make([]string, 0, len(sources))
	for key := range sources {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// readConfigStringList 读取字符串数组类设置（JSON null/缺省 → nil）。
//
// settings 的 TypeList 在配置中心快照中以 JSON 数组出现；同时容忍 []any 的
// 通用形态，避免设置服务实现变化让禁用清单静默变成空。
func readConfigStringList(config ConfigReader, key string) []string {
	if config == nil {
		return nil
	}
	raw, ok := config.Get(key)
	if !ok {
		return nil
	}
	var direct []string
	if err := json.Unmarshal(raw, &direct); err == nil {
		return direct
	}
	var generic []any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil
	}
	out := make([]string, 0, len(generic))
	for _, item := range generic {
		if value, ok := item.(string); ok {
			out = append(out, value)
		}
	}
	return out
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
