package platform

// gateway 角色的管理面装配：配置中心快照/订阅 → 令牌缓存订阅 → 审计 →
// httpapi 组合根。所有启动副作用都在监听端口之前完成，任何一步失败都让
// 进程启动失败（绝不以「无配置订阅/无令牌缓存失效/无审计」的状态上线）。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/billing"
	"newtg/gateway/internal/billing/pricing"
	"newtg/gateway/internal/clientip"
	"newtg/gateway/internal/config"
	"newtg/gateway/internal/configcenter"
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/httpapi"
	"newtg/gateway/internal/rbac"
	"newtg/gateway/internal/store"
	"newtg/gateway/internal/tokenstore"
	"newtg/gateway/plugins"
)

// gatewayAssembly 是 gateway 角色一次装配的产物：管理面 handler 与中继面
// （/v1）所需的计费接线。
//
// 中继路由在 platform.mountRelayRoutes 挂载，但计费引擎依赖 gorm、配置中心
// 快照与独立日志库——这些只在 newGatewayAPI 里就绪。把工厂与模型状态查询
// 一并交回 runGateway，保证引擎/定价源**只装配一次**（同一 source 实例既供
// 引擎结算，又接管理面的 OnChange 失效钩子）。
type gatewayAssembly struct {
	// Handler 是 /api/* 管理面；nil 表示探针-only 装配（测试）。
	Handler http.Handler
	// Billing 为每个中继请求构造计费会话；nil 时中继回退 M2 桩。
	Billing func(*contract.RelayContext) contract.BillingSession
	// ModelStatus 是派发前的模型停用检查（R-84）。
	ModelStatus httpapi.RelayModelStatusStore
	// Config 是配置中心读取面（M4 跨协议转发开关等中继侧配置）。
	// 与管理面 RouterConfig.Config 共用同一个 configcenter.Center 快照。
	Config httpapi.ConfigReader
	// PricingSource 是引擎与管理面共用的定价源（装配验证/诊断用）。
	PricingSource *pricing.DBSource
	// PricingOnChange 就是注入管理面的缓存失效回调（R-63）；与
	// PricingSource.Invalidate 同一个函数值，供装配测试验证接线。
	PricingOnChange func()
	// Tasks 是任务数据面 handler（M6d Task 6a）；由 mountRelayRoutes 挂到
	// /v1 令牌链。nil 表示本次装配没有任务数据面（探针-only 测试）。
	Tasks *httpapi.TasksHandler
}

// pricingCacheInvalidator 是定价源进程内缓存失效的最小缝。
//
// 生产实现是 *pricing.DBSource（NewSource 返回）；单测用计数 fake 证明
// newGatewayAPI 把 **同一个** Invalidate 接给了管理面 OnChange（R-63）。
type pricingCacheInvalidator interface {
	Invalidate()
}

var _ pricingCacheInvalidator = (*pricing.DBSource)(nil)

// pricingOnChange 把定价源适配成管理面的「写后失效」回调。
//
// nil 源返回 nil：管理面只靠 sourceCacheTTL=30s 兜底，而不是 panic 或静默
// 接一个空实现（两种都难排查）。
func pricingOnChange(source pricingCacheInvalidator) func() {
	if source == nil {
		return nil
	}
	return source.Invalidate
}

// newAPIHandler 是 connectors 的生产实现：把 Resource 还原为具体类型后
// 交给 newGatewayAPI。类型不匹配说明装配被错误调用（例如测试 stub 走了
// 生产 connectors），直接报错而不是静默跳过 API 装配。
func (prodConnectors) newAPIHandler(ctx context.Context, cfg *config.Config,
	db, logsDB, rds Resource, log *slog.Logger) (*gatewayAssembly, error) {

	sqlDB, ok := db.(*sql.DB)
	if !ok {
		return nil, errors.New("gateway: api assembly requires a *sql.DB main database")
	}
	sqlLogs, ok := logsDB.(*sql.DB)
	if !ok {
		return nil, errors.New("gateway: api assembly requires a *sql.DB logs database")
	}
	redisRes, ok := rds.(redisResource)
	if !ok {
		return nil, errors.New("gateway: api assembly requires a redis resource")
	}
	if redisRes.client == nil {
		// R-41/R-88：计费引擎与在飞租约都必须拿到非 nil 客户端（结算双扣
		// 窗口靠 requestID 锁收窄，租约靠心跳续期）。
		return nil, errors.New("gateway: api assembly requires a non-nil redis client")
	}
	return newGatewayAPI(ctx, cfg, sqlDB, sqlLogs, redisRes.client, log)
}

// newGatewayAPI 构造 /api/* 的完整路由。db/logsDB/rdb 必须已通过
// OpenDB/OpenLogsDB/OpenRedis 验证连通性。
//
// 启动副作用（设计冻结）：
//   - configcenter.Load：配置快照必须先于任何 handler 读取；
//   - configcenter.Subscribe：≤1s 的跨副本配置生效路径；
//   - tokenstore.Subscribe：GC-11 的令牌缓存第二失效通道（强制）。
func newGatewayAPI(ctx context.Context, cfg *config.Config, db, logsDB *sql.DB,
	rdb *redis.Client, log *slog.Logger) (*gatewayAssembly, error) {

	// tokenstore 的 CRUD 用 GORM；复用已建好的 database/sql 连接池，
	// 不另开第二组连接（避免连接数与事务边界分裂）。
	gormDB, err := gorm.Open(postgres.New(postgres.Config{Conn: db}), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Warn),
	})
	if err != nil {
		return nil, fmt.Errorf("gateway: open gorm: %w", err)
	}

	// 配置中心：Sealer 与登录私钥/2FA 种子共用 CREDENTIAL_ENCRYPTION_KEY，
	// 密钥类配置（Turnstile secret）才能跨副本解封。
	center := configcenter.New(db, rdb)
	cipher, err := auth.NewSecretCipher(cfg.CredentialEncryptionKey)
	if err != nil {
		return nil, fmt.Errorf("gateway: credential cipher: %w", err)
	}
	center.Sealer = cipher
	if err := center.Load(ctx); err != nil {
		return nil, fmt.Errorf("gateway: load config center: %w", err)
	}
	// 计费进程级装配（N2/N5）：定价源构造写入进程级 QPU（admin API 的
	// 元↔积分换算基准），并接入钳制审计日志。必须在 Load 之后、任何
	// handler 读取 CurrentQuotaPerUnit 之前；非法 QPU 让启动失败。
	//
	// 返回的 source 实例必须保留：它同时是（a）真实计费引擎的定价源，
	// （b）管理面定价写成功后的缓存失效目标（R-63）。两处必须共用同一实例，
	// 否则失效钩子清的是另一个缓存，定价变更仍要等 30s TTL。
	source, err := initBillingAssembly(gormDB, center, cfg, log, "gateway")
	if err != nil {
		return nil, fmt.Errorf("gateway: billing assembly: %w", err)
	}

	// T9：真实计费引擎。R-41/R-88 的生产保证要求 Redis 非 nil（结算双扣
	// 窗口靠 requestID 锁收窄）；Saturator 写独立日志库（usage_logs）。
	engine := billing.NewEngine(gormDB, logsDB, rdb, center, source,
		billing.NewRegistry(rdb), billing.NewDBLogger(logsDB).
			// live_feed 投递（D6b）：事件在 usage_logs 提交成功后投递到 Redis；
			// 失败只告警，不影响计费。Redis 与计费引擎共用同一连接。
			WithFeed(billing.NewRedisFeedPublisher(rdb), log))
	if err := center.Subscribe(ctx); err != nil {
		return nil, fmt.Errorf("gateway: subscribe config center: %w", err)
	}

	tokens := tokenstore.New(gormDB, rdb, center)
	// 订阅是 GC-11 的强制启动项：缺失时跨副本的即时失效只剩 60s TTL 兜底。
	if err := tokens.Subscribe(ctx); err != nil {
		return nil, fmt.Errorf("gateway: subscribe token cache: %w", err)
	}

	// TRUSTED_PROXIES 的畸形条目被忽略（fail-closed：该条目永不进入信任集）；
	// 记录告警而拒绝启动会让一个拼写错误把整站下线。
	if err := httpapi.ConfigureTrustedProxies(cfg.TrustedProxies); err != nil {
		log.Warn("trusted proxies contains invalid entries; they are ignored", "error", err)
	}

	auditor := audit.New(logsDB)
	// 用户名快照解析器：审计行保留 12 个月，username 是 NOT NULL 的操作者
	// 快照；调用侧不总携带用户名（令牌/管理事件只有 actor id）。失败不阻断
	// 审计——writer 会回退 user#<id>，绝不能因为解析失败丢掉安全事件。
	auditor.UsernameResolver = func(ctx context.Context, userID int64) (string, error) {
		var username string
		err := db.QueryRowContext(ctx, `SELECT username FROM users WHERE id = $1`, userID).Scan(&username)
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		if err != nil {
			return "", fmt.Errorf("gateway: resolve audit username for %d: %w", userID, err)
		}
		return username, nil
	}
	audit.SetDefault(auditor)

	authz := newAuthzMiddleware(auditor, log)

	// M2 渠道管理端点：与 M1 管理面共用同一条受保护链（SessionAuth → CSRF →
	// OriginRefererCheck）；渠道 handler 自身再按 channels:* 做功能级权限检查。
	// 这里只构造 handler，由 BuildGatewayRouter 负责挂到受保护子树，避免在
	// platform 层重复装配一条 /api/ 中间件链（两条 /api/ 挂载会冲突）。
	channelSealer := httpapi.NewSecretSealer(cfg.CredentialEncryptionKey)
	// 渠道绑定校验的注册表视图：factory 层来自编入二进制的内置源，
	// 上传侧查 active 版本行（relay-06 §1.3）。这里与 assembly 的内置插件
	// 装配各自读取同一份 embed 源，代价仅一次目录遍历。
	pluginSources, err := plugins.TaskPluginSources()
	if err != nil {
		return nil, fmt.Errorf("gateway: load built-in task plugin sources: %w", err)
	}
	factoryKeys := make([]string, 0, len(pluginSources))
	for key := range pluginSources {
		factoryKeys = append(factoryKeys, key)
	}
	channelHandler := httpapi.NewChannelHandler(
		store.NewChannelRepo(db),
		store.NewTokenRepo(db),
		channelSealer,
		httpapi.ChannelHandlerOptions{
			Tester:      httpapi.NewChannelConnectivityTester(channelSealer, nil),
			Idempotency: httpapi.NewRedisIdempotencyStore(rdb),
			Plugins: httpapi.NewTaskPluginCatalog(
				factoryKeys, store.NewTaskPluginRepo(db)),
		},
	)

	// 定价仓储同时服务管理面与中继的停用模型检查（R-84）：同一实例、
	// 同一套候选解析语义，避免两处对「这个模型是否被停用」得出不同答案。
	pricingRepo := store.NewPricingRepo(db)
	// R-63：定价写成功后立即清空 30s 缓存。这里把回调保存到 assembly，
	// 既注入 GatewayDeps，也让装配测试能验证「就是 source.Invalidate」。
	pricingChanged := pricingOnChange(source)

	// M5 兑换码端点（Task 2）：默认 FundingSource 复用主库 GORM 句柄；
	// 配置读取走 configcenter，兑换限流用同一 Redis 客户端。
	redemptionHandler, err := httpapi.NewRedemptionHandlers(httpapi.RedemptionHandlerConfig{
		DB:     gormDB,
		Redis:  rdb,
		Config: center,
		Authz:  authz,
		Logger: log,
	})
	if err != nil {
		return nil, fmt.Errorf("gateway: redemption handlers: %w", err)
	}

	// M5 在线充值端点（Task 3）：密钥类配置（Stripe secret/webhook secret、
	// 支付宝私钥/公钥）经与配置中心同一个 SecretSealer 解封；webhook 限流
	// 复用同一 Redis 客户端，缺 Redis 时构造失败（fail closed）。
	topupHandler, err := httpapi.NewTopupHandlers(httpapi.TopupHandlerConfig{
		DB:     gormDB,
		Redis:  rdb,
		Config: center,
		Sealer: cipher,
		Logger: log,
		// 订单端点按 orders:read/orders:manage 判定管理员范围；必须复用
		// 同一个 authz 实例，才能拿到 SessionAuth 未填充的权威权限集合。
		Authz: authz,
	})
	if err != nil {
		return nil, fmt.Errorf("gateway: topup handlers: %w", err)
	}

	// 钱包摘要（modules-03 §F-03-01）：Task 6 的余额卡从该端点读取余额。
	// 主库提供余额/冻结/消费，日志库提供请求总数；阈值来自站点配置。
	walletHandler, err := httpapi.NewWalletHandlers(httpapi.WalletHandlerConfig{
		DB:     db,
		LogDB:  logsDB,
		Config: center,
		Logger: log,
	})
	if err != nil {
		return nil, fmt.Errorf("gateway: wallet handlers: %w", err)
	}

	router, err := httpapi.BuildGatewayRouter(httpapi.GatewayDeps{
		DB:                      db,
		LogDB:                   logsDB,
		Redis:                   rdb,
		Sessions:                auth.NewSessionManager(rdb, []byte(cfg.SessionSecret)),
		Config:                  center,
		Sealer:                  cipher,
		Audit:                   auditor,
		Authz:                   authz,
		TokenService:            tokens,
		SessionSecret:           []byte(cfg.SessionSecret),
		CredentialEncryptionKey: cfg.CredentialEncryptionKey,
		PublicBaseURL:           cfg.PublicBaseURL,
		Logger:                  log,
		ChannelHandler:          channelHandler,
		PricingRepo:             pricingRepo,
		RedemptionHandlers:      redemptionHandler,
		TopupHandlers:           topupHandler,
		SubscriptionPurchase:    topupHandler.Service(),
		WalletHandlers:          walletHandler,
		// R-63：定价写成功后立即清空 30s 缓存；未接线时新价最迟 30s 才生效。
		PricingOnChange: pricingChanged,
		// 设置写成功后的广播必须复用同一个配置中心实例：其它副本的
		// pub/sub 失效路径与本地快照才会一致。
		SettingsBroadcaster: center,
		NodeName:            cfg.NodeName,
		StartedAt:           time.Now(),
		// 任务提交与中继共用同一个计费引擎会话工厂（M6d Task 6a）：
		// 预扣/退款/结算语义只能有一套实现。
		Billing: engine.NewSession,
	})
	if err != nil {
		return nil, err
	}
	return &gatewayAssembly{
		Handler:         router,
		Billing:         engine.NewSession,
		ModelStatus:     pricingRepo,
		Config:          center,
		PricingSource:   source,
		PricingOnChange: pricingChanged,
		Tasks:           router.TasksHandler(),
	}, nil
}

// newAuthzMiddleware 构造 RBAC 中间件并接线拒绝审计（security-09 §2.7 设计 B /
// security-02 TC-B-17）：所有 403/404 拒绝都落 audit_logs（category=security、
// result=failed、status_code=403）。
//
// 回调必须自行限时且绝不 panicking 到拒绝路径之外：审计不可用时 403 仍要
// 正常返回。因此这里用 2s 超时 + defer recover，失败只记日志。
func newAuthzMiddleware(auditor *audit.Logger, log *slog.Logger) *rbac.AuthzMiddleware {
	authz := rbac.NewAuthzMiddleware()
	if auditor == nil || !auditor.Configured() {
		return authz
	}
	authz.SetDenyAuditor(func(r *http.Request, e rbac.DenyEvent) {
		defer func() {
			if recovered := recover(); recovered != nil {
				log.Error("rbac deny auditor panicked", "panic", recovered)
			}
		}()
		// detail 键名冻结（security-09 §2.7）：event 必须是 "authz_denied"。
		detail, err := json.Marshal(map[string]any{
			"event":               "authz_denied",
			"required_permission": e.Permission,
			"reason":              e.Reason,
			"fields":              e.Fields,
		})
		if err != nil {
			log.Warn("marshal authz deny audit detail failed", "error", err)
			return
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Second)
		defer cancel()
		// 数据级拒绝按契约返回 404（避免 ID 枚举），其余拒绝 403；审计
		// status_code 必须与客户端实际收到的状态码一致。
		statusCode := http.StatusForbidden
		if e.Reason == rbac.DenyReasonDataScope {
			statusCode = http.StatusNotFound
		}
		event := audit.Event{
			Action:     audit.ActionAuthzDenied,
			Actor:      e.UserID,
			Result:     "failed",
			StatusCode: statusCode,
			Method:     e.Method,
			Route:      e.Route,
			IP:         clientip.ClientIP(r),
			UserAgent:  r.UserAgent(),
			Detail:     detail,
		}
		if err := auditor.LogEvent(ctx, event); err != nil {
			log.Warn("authz deny audit write failed",
				"user_id", e.UserID, "permission", e.Permission, "error", err)
		}
	})
	return authz
}
