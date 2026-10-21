package platform

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/billing"
	"newtg/gateway/internal/config"
	"newtg/gateway/internal/configcenter"
	"newtg/gateway/internal/contract"
	deploybilling "newtg/gateway/internal/deploy/billing"
	"newtg/gateway/internal/httpapi"
	"newtg/gateway/internal/notify"
	"newtg/gateway/internal/rankings"
	"newtg/gateway/internal/store"
	"newtg/gateway/internal/subscriptionworker"
	"newtg/gateway/internal/version"
	"newtg/gateway/migrations"
)

// RunGateway 以 gateway 角色运行。装配序（platform-03）：logger → OpenDB →
// OpenLogsDB → OpenRedis →（DBMigrateOnStart 时）RunMigrate → 配置中心与令牌
// 缓存订阅 + 管理面路由装配（newAPIHandler）→ metrics goroutine → HTTP server
// → 阻塞至 ctx done → 优雅停机（SHUTDOWN_GRACE_SECONDS 窗口内先停 HTTP 监听，
// 再按日志库/主库/Redis 顺序关闭）。
func RunGateway(ctx context.Context, cfg *config.Config) error {
	return runGateway(ctx, cfg, prodConnectors{})
}

func runGateway(ctx context.Context, cfg *config.Config, conn connectors) error {
	log := NewLogger(cfg)
	log.Info("gateway starting",
		"version", version.Version,
		"commit", version.Commit,
		"node", cfg.NodeName,
	)

	// R3 期望版本与 schema_migrations.version 同口径（14 位时间戳）。
	// 嵌入迁移不可用时 fail-fast，避免以错误期望值对外提供服务。
	expectedVersion, err := ExpectedSchemaVersion(migrations.FS)
	if err != nil {
		return fmt.Errorf("gateway: expected schema version: %w", err)
	}

	db, err := conn.openDB(ctx, cfg)
	if err != nil {
		return fmt.Errorf("gateway: open db: %w", err)
	}
	// 日志库与主库分开建连：审计/PII 匿名化在日志库，不与业务事务抢连接。
	logsDB, err := conn.openLogsDB(ctx, cfg)
	if err != nil {
		// 正在返回打开日志库的主错误；清理主库的失败不覆盖它。
		_ = db.Close()
		return fmt.Errorf("gateway: open logs db: %w", err)
	}
	redisRes, err := conn.openRedis(cfg)
	if err != nil {
		// 正在返回打开 Redis 的主错误；清理数据库的失败不覆盖它。
		_ = logsDB.Close()
		_ = db.Close()
		return fmt.Errorf("gateway: open redis: %w", err)
	}

	if cfg.DBMigrateOnStart {
		// 经 connectors 钩子调用：生产即 RunMigrate，测试可注入 stub 断言调用与否。
		if err := conn.runMigrate(ctx, cfg); err != nil {
			// 主错误优先：迁移失败时尽力释放资源，关闭错误不再上报。
			_ = redisRes.Close()
			_ = logsDB.Close()
			_ = db.Close()
			return fmt.Errorf("gateway: migrate on start: %w", err)
		}
	}

	// 管理面路由必须在监听之前装配完成：配置中心/令牌缓存订阅失败、
	// 权限装载器或审计缺失都必须阻止进程上线（GC-7）。
	assembly, err := conn.newAPIHandler(ctx, cfg, db, logsDB, redisRes, log)
	if err != nil {
		_ = redisRes.Close()
		_ = logsDB.Close()
		_ = db.Close()
		return fmt.Errorf("gateway: assemble http api: %w", err)
	}
	var apiHandler http.Handler
	if assembly != nil {
		apiHandler = assembly.Handler
	}
	// I-2：自动禁用 reporter 的停机顺序固定为「先 cancel + Wait，再关
	// 主库/日志库」。幂等（cancel/Wait 均可重复调用），四条资源清理路径
	// 都调用它，避免停机排空写撞上已关闭的连接池。
	stopReporter := func() {
		if assembly != nil && assembly.AttemptFailureReporterStop != nil {
			assembly.AttemptFailureReporterStop()
		}
	}

	// metrics 侧车：MetricsAddr=="0" 时 RunMetricsServer 直接返回 nil，goroutine 立即结束。
	go func() {
		if err := RunMetricsServer(ctx, cfg, log); err != nil {
			log.Error("metrics server exited with error", "error", err)
		}
	}()

	// HTTP 主服务：/healthz（恒 200）与 /readyz（五项实时判定）与同一
	// server 上的 /api/* 管理面（apiHandler 为 nil 时仅探针）。
	checker := checkerFromResources(db, redisRes, int(expectedVersion), log)
	checker.node = cfg.NodeName
	// /api/* 管理面（M1 路由 + M2 渠道端点）由 newAPIHandler 在 httpapi 组合根
	// 中装配，newGatewayMux 把它整体挂到 /api/。
	mux := newGatewayMux(checker, apiHandler)
	// /v1/*（中继面）只在资源是生产具体类型时挂载；platform 单测注入的 stub
	// Resource 无法还原 *sql.DB / *redis.Client，此时保持 health-only mux
	// （既有测试不触真 PG/Redis）。assembly 携带真实计费引擎工厂；nil 时中继
	// 回退 M2 桩（测试装配）。
	mountRelayRoutes(mux, cfg, db, redisRes, assembly)
	srv := newGatewayHTTPServer(SecurityHeaders(mux))
	ln, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		// 监听失败是主错误；资源清理失败不覆盖它。
		stopReporter()
		_ = redisRes.Close()
		_ = logsDB.Close()
		_ = db.Close()
		return fmt.Errorf("gateway: listen %s: %w", cfg.HTTPAddr, err)
	}
	// 逐副本心跳（modules-08 §3.7 F-08-21）：监听成功后立即写一次，此后
	// 每 30s 刷新 TTL 90s 的 Redis key。独立于请求 ctx，停机时由 Stop
	// 删除；先于 Redis 连接关闭，避免留下「幽灵实例」直到 TTL 到期。
	var heartbeat *httpapi.InstanceHeartbeat
	if rr, ok := redisRes.(redisResource); ok {
		hb, err := httpapi.StartInstanceHeartbeat(context.Background(), httpapi.InstanceHeartbeatConfig{
			Redis:      rr.client,
			InstanceID: httpapi.ResolveProcessInstanceID(),
			Role:       "gateway",
			Version:    version.Version,
			StartedAt:  time.Now(),
			Logger:     log,
		})
		if err != nil {
			stopReporter()
			_ = ln.Close()
			_ = redisRes.Close()
			_ = logsDB.Close()
			_ = db.Close()
			return fmt.Errorf("gateway: start instance heartbeat: %w", err)
		}
		heartbeat = hb
	}
	// R4：装配完成（监听就绪）即置位。
	checker.MarkConfigLoaded()
	serveErr := make(chan error, 1)
	go func() {
		err := srv.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErr <- err
	}()
	// R5：HTTP server 开始服务后置位。
	checker.MarkStartupDone()
	log.Info("gateway ready", "http_addr", ln.Addr().String())

	select {
	case <-ctx.Done():
	case err := <-serveErr:
		// 监听异常退出（端口被抢、进程内关闭等）：清理资源后上报。
		stopReporter()
		heartbeat.Stop()
		_ = redisRes.Close()
		_ = logsDB.Close()
		_ = db.Close()
		if err != nil {
			return fmt.Errorf("gateway: http serve: %w", err)
		}
		return nil
	}

	// 优雅停机：先停 HTTP 监听（窗口 SHUTDOWN_GRACE_SECONDS），再关 DB / Redis。
	grace := time.Duration(cfg.ShutdownGraceSeconds) * time.Second
	shutdownCtx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Warn("http shutdown exceeded grace window; forcing close", "error", err)
		// Shutdown 已失败并记录；Close 仅兜底释放监听，错误无需再次上报。
		_ = srv.Close()
	}
	// HTTP 已停止对外服务：先停 reporter（排空队列）再关心跳与连接池。
	// 顺序：HTTP 停止 → reporter 排空 → 心跳 → 日志库/主库/Redis。
	stopReporter()
	heartbeat.Stop()
	if err := logsDB.Close(); err != nil {
		log.Warn("close logs db failed", "error", err)
	}
	if err := db.Close(); err != nil {
		log.Warn("close db failed", "error", err)
	}
	if err := redisRes.Close(); err != nil {
		log.Warn("close redis failed", "error", err)
	}
	log.Info("gateway stopped")
	return nil
}

// mountRelayRoutes 把 /v1 中继面挂到 gateway 主 mux。
//
// 中继 handler 自带 RelayAuth（令牌面，不使用 Cookie 会话），因此直接挂在
// 根 mux，不经过管理面的 SessionAuth → CSRF → Origin/Referer 链。管理面
// （含 M2 渠道端点）由 newAPIHandler 在 httpapi 组合根中装配，newGatewayMux
// 负责把整个管理面挂到 /api/；本函数只处理需要 db/redis 具体类型的 /v1。
//
// assembly 提供 T9 的真实计费会话工厂与模型停用检查；nil（测试装配）时
// 中继回退 billingseam.NewStub（M2 行为），模型停用检查跳过。
//
// 只有生产资源（OpenDB 的 *sql.DB、OpenRedis 的 redisResource）才能构造仓储与
// 密钥封装；测试 path 的 stubResource 无法还原具体类型，此时静默跳过——
// 这不是降级（生产永远命中具体类型），而是为了保持 M0 单测的 GC-8 隔离。
func mountRelayRoutes(mux *http.ServeMux, cfg *config.Config, db, rds Resource, assembly *gatewayAssembly) {
	sqlDB, ok := db.(*sql.DB)
	if !ok || sqlDB == nil {
		return
	}
	redisRes, ok := rds.(redisResource)
	if !ok || redisRes.client == nil {
		return
	}

	// 仓储与密钥封装（GC-8：凭据密文的唯一读写面）。
	tokenRepo := store.NewTokenRepo(sqlDB)
	channelRepo := store.NewChannelRepo(sqlDB)
	sealer := httpapi.NewSecretSealer(cfg.CredentialEncryptionKey)

	var billingFactory func(*contract.RelayContext) contract.BillingSession
	var modelStatus httpapi.RelayModelStatusStore
	var settings httpapi.ConfigReader
	if assembly != nil {
		billingFactory = assembly.Billing
		modelStatus = assembly.ModelStatus
		settings = assembly.Config
	}

	// 插件原生路由（T6c-a）的执行面就是任务数据面 handler：两者共用同一个
	// 插件管理器与同一份提交/查询实现，不能各自持有一个 Manager 实例。
	// 显式判空避免 typed-nil 接口陷阱（*TasksHandler(nil) 装进接口后 != nil）。
	var nativeRoutes httpapi.NativeRouteDispatcher
	if assembly != nil && assembly.Tasks != nil {
		nativeRoutes = assembly.Tasks
	}

	// 中继面：/v1/chat/completions 与 /v1/models（鉴权中间件在 handler 内挂载）。
	// TRUSTED_PROXIES 决定 RelayAuth 是否采信 X-Forwarded-For（M2 策略见 relay_auth.go）。
	//
	// R-64：分组倍率的严格存在性查询与定价管理共用同一个仓储口径
	// （user_groups.name=? AND status=1），不复用 DBSource 的「未命中缓存 1.0」。
	pricingRepo := store.NewPricingRepo(sqlDB)
	// M4-a：自动禁用 reporter 由管理面装配（newGatewayAPI）构造并已 Start；
	// 这里只做接口透传。nil（探针-only/测试装配）时中继完全跳过判定。
	var attemptReporter httpapi.AttemptFailureReporter
	// W7/M5-b：准入限流器同样是一次装配的产物（newGatewayAPI 用同一个 Redis
	// 客户端构造一次），中继只透传。生产装配由 newRelayRateLimiter 保证非 nil
	// （缺 Redis 即启动失败）；nil 只可能来自探针-only/测试装配，此时跳过限流。
	var rateLimiter httpapi.RelayRateLimiter
	if assembly != nil {
		attemptReporter = assembly.AttemptFailureReporter
		rateLimiter = assembly.RateLimiter
	}
	relayHandler := httpapi.NewRelayHandler(httpapi.RelayHandlerOptions{
		Channels:        channelRepo,
		Tokens:          tokenRepo,
		Access:          tokenRepo,
		Sealer:          sealer,
		Billing:         billingFactory,
		ModelStatus:     modelStatus,
		Config:          settings,
		GroupConfigured: pricingRepo.GroupConfigured,
		TrustedProxies:  cfg.TrustedProxies,
		// /pg/* 会话路径的分组回源：与 /api/status 的 usable_groups 同口径。
		UserGroups: store.NewUserGroupsRepo(sqlDB),
		// 原生路由索引/分发（manifest routes[] → /v1 兜底）。
		NativeRoutes: nativeRoutes,
		// M4-a：中继每轮非 2xx 失败 → 自动禁用判定（异步、非阻塞）。
		AttemptFailureReporter: attemptReporter,
		// W7/M5-b：三档准入桶（B11 → B10 → B12）与 429/500 错误出口。
		RateLimiter: rateLimiter,
	})
	relayHandler.Register(mux)
	// 任务数据面（M6d Task 6a）：/v1/tasks* 与中继共用同一 RelayAuth
	// （令牌面），但不经过 chat 的 IR/适配器/计费管线——任务由插件描述符
	// 驱动，计费只经 BillingSession 接缝。
	if assembly != nil && assembly.Tasks != nil {
		assembly.Tasks.RegisterRelay(mux, relayHandler.AuthMiddleware())
	}
	// /pg/*（Playground 会话中继）：只接受会话 Cookie，重写到 /v1 后复用同一
	// 管线。SessionManager 与 /api/* 使用同一 Redis 与 secret，登录态天然共享。
	//
	// Origin 白名单与管理面同口径：PUBLIC_BASE_URL 为空时回退到
	// site.server_address（安装向导写入的站点地址）。只传裸 cfg.PublicBaseURL
	// 会让「只配置了站点地址」的部署把同源写请求误判成跨站（/pg 的 CSRF 纵深）。
	relayHandler.RegisterPlayground(mux,
		auth.NewSessionManager(redisRes.client, []byte(cfg.SessionSecret)),
		effectivePublicBaseURL(cfg.PublicBaseURL, settings))
}

// effectivePublicBaseURL 返回 Origin/Referer 白名单来源。
//
// 优先显式 PUBLIC_BASE_URL（可多值）；为空时回退 site.server_address。
// 该键由安装向导写入、经配置中心下发，是部署方声明的规范站点地址。
// 读取失败/为空时返回空串——OriginRefererCheck 会退化为「与请求 Host 同源」，
// 仍保持 fail-closed，不会放开跨站。
func effectivePublicBaseURL(explicit string, config httpapi.ConfigReader) string {
	if strings.TrimSpace(explicit) != "" {
		return explicit
	}
	if config == nil {
		return ""
	}
	raw, ok := config.Get("site.server_address")
	if !ok {
		return ""
	}
	var address string
	if err := json.Unmarshal(raw, &address); err != nil {
		return ""
	}
	return strings.TrimSpace(address)
}

// maxHeaderBytes 是 HTTP 请求头总大小上限（32 KiB）。
//
// net/http 的默认值是 1 MiB：一个未认证客户端就能让每次连接在 TLS/业务之前
// 占用并解析近 1 MiB 头部（同时放大日志与代理内存）。32 KiB 覆盖最长现实
// 场景（会话/CSRF Cookie + 常见转发头 + 超长 UA），是纵深防御而非业务上限。
const maxHeaderBytes = 32 << 10

// newGatewayHTTPServer 构造 gateway 的主 HTTP server。
//
// 安全响应头在 mux 外层统一注入（security-08 §2.1/§5.2）：404、探针、后续
// API 与 SSE 都必须经过同一层，避免按路由漏配。
func newGatewayHTTPServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		MaxHeaderBytes:    maxHeaderBytes,
	}
}

// newGatewayMux 把探针与管理面挂到同一 mux：/api/ 子树交给 httpapi.Router
// （其内部已包含公开/受保护两条链），/healthz 与 /readyz 保持在根路径。
// api 为 nil 时只暴露探针端点（worker/测试装配）。
func newGatewayMux(checker *Checker, api http.Handler) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", checker.HandlerHealthz)
	mux.HandleFunc("/readyz", checker.HandlerReadyz)
	if api != nil {
		// Handle 不裁剪前缀：Router 的模式本身就是 /api/...。
		mux.Handle("/api/", api)
	}
	return mux
}

// checkerFromResources 从装配好的 Resource 还原具体类型构造 Checker（生产路径）。
// 非生产实现（测试 stub）无法还原时降级为 nil 依赖的恒失败 Checker，不 panic。
func checkerFromResources(db, rds Resource, migrationExpected int, log *slog.Logger) *Checker {
	sqldb, _ := db.(*sql.DB)
	var client *redis.Client
	if rr, ok := rds.(redisResource); ok {
		client = rr.client
	}
	return NewChecker(sqldb, client, migrationExpected, log)
}

// RunWorker 以 worker 角色运行：装配计费引擎与在飞租约注册表，进入兜底
// 扫描循环，阻塞至 ctx done 后按 Redis → 日志库 → 主库顺序关闭。
func RunWorker(ctx context.Context, cfg *config.Config) error {
	return runWorker(ctx, cfg, prodConnectors{})
}

func runWorker(ctx context.Context, cfg *config.Config, conn connectors) error {
	log := NewLogger(cfg)
	log.Info("worker starting",
		"version", version.Version,
		"commit", version.Commit,
		"node", cfg.NodeName,
	)

	dbRes, err := conn.openDB(ctx, cfg)
	if err != nil {
		return fmt.Errorf("worker: open db: %w", err)
	}
	// worker 的计费引擎需要 GORM 句柄（复用同一个 database/sql 连接池，
	// 不另开第二组连接，避免连接数与事务边界分裂）。
	sqlDB, ok := dbRes.(*sql.DB)
	if !ok {
		_ = dbRes.Close()
		return errors.New("worker: main database resource is not *sql.DB")
	}

	// 日志库与主库分开建连：usage_logs 的分区维护与日志写入都在日志库，
	// 不能让它和钱包事务抢连接（platform-03 §3.4 的两库分治）。
	logsRes, err := conn.openLogsDB(ctx, cfg)
	if err != nil {
		_ = dbRes.Close()
		return fmt.Errorf("worker: open logs db: %w", err)
	}
	sqlLogs, ok := logsRes.(*sql.DB)
	if !ok {
		_ = logsRes.Close()
		_ = dbRes.Close()
		return errors.New("worker: logs database resource is not *sql.DB")
	}

	redisRes, err := conn.openRedis(cfg)
	if err != nil {
		_ = logsRes.Close()
		_ = dbRes.Close()
		return fmt.Errorf("worker: open redis: %w", err)
	}
	rr, ok := redisRes.(redisResource)
	if !ok {
		_ = redisRes.Close()
		_ = logsRes.Close()
		_ = dbRes.Close()
		return errors.New("worker: redis resource has unexpected type")
	}
	if rr.client == nil {
		// redisResource.Close 会解引用 client；nil 客户端说明装配被错误
		// 调用，直接失败（R-41：引擎与注册表都必须拿到非 nil 客户端）。
		_ = logsRes.Close()
		_ = dbRes.Close()
		return errors.New("worker: redis client is nil")
	}

	gormDB, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Warn),
	})
	if err != nil {
		_ = redisRes.Close()
		_ = logsRes.Close()
		_ = dbRes.Close()
		return fmt.Errorf("worker: open gorm: %w", err)
	}

	// 配置中心必须在定价源之前装载：NewSource 从快照读取
	// billing.quota_per_unit 并写入进程级 QPU。订阅是兜底刷新路径
	// （5 分钟整表重载 + pub/sub），worker 与 gateway 同口径。
	center := configcenter.New(sqlDB, rr.client)
	// 密钥类配置（支付宝 app 私钥/公钥）在配置中心以 AES-GCM 密封存储；
	// worker 的轮询兜底要解封后才能调用 alipay.trade.query/trade.close，
	// 因此必须与 gateway 使用同一把 CREDENTIAL_ENCRYPTION_KEY。
	cipher, err := auth.NewSecretCipher(cfg.CredentialEncryptionKey)
	if err != nil {
		_ = redisRes.Close()
		_ = logsRes.Close()
		_ = dbRes.Close()
		return fmt.Errorf("worker: credential cipher: %w", err)
	}
	center.Sealer = cipher
	if err := center.Load(ctx); err != nil {
		_ = redisRes.Close()
		_ = logsRes.Close()
		_ = dbRes.Close()
		return fmt.Errorf("worker: load config center: %w", err)
	}
	// 计费进程级装配（与 gateway 共用）：定价源构造写入进程级 QPU，
	// 并把钳制审计接到 slog（billing-07 §4.5）。必须在 configcenter.Load
	// 之后；非法 QPU 让 worker 启动失败，而不是带着错误汇率运行。
	source, err := initBillingAssembly(gormDB, center, cfg, log, "worker")
	if err != nil {
		_ = redisRes.Close()
		_ = logsRes.Close()
		_ = dbRes.Close()
		return fmt.Errorf("worker: billing assembly: %w", err)
	}
	if err := center.Subscribe(ctx); err != nil {
		_ = redisRes.Close()
		_ = logsRes.Close()
		_ = dbRes.Close()
		return fmt.Errorf("worker: subscribe config center: %w", err)
	}

	registry := billing.NewRegistry(rr.client)
	// Saturator 是 usage_logs 的落库实现（Task 6 的 DBLogger）。worker 的
	// 兜底退款只依赖钱包流水，日志是辅助状态面（R-14）；但装配真实的
	// Saturator 让引擎在将来被 worker 复用时行为与 gateway 一致。
	engine := billing.NewEngine(gormDB, sqlLogs, rr.client, center, source, registry,
		billing.NewDBLogger(sqlLogs).
			// worker 的退款收敛不产生新调用（fact=nil），DBLogger 只在真正
			// 插入新行时投递 live_feed；接线与 gateway 保持一致。
			WithFeed(billing.NewRedisFeedPublisher(rr.client), log))
	// S4a：与 gateway 同一装配口径。worker 复用引擎做兜底恢复，
	// 未装配会让订阅偏好退化为纯钱包行为。
	engine.SetSubscriptionFunding(billing.NewStoreSubscriptionFunding(
		store.NewSubscriptionRepo(sqlDB), sqlDB))
	// M-1 复核：租约 TTL / 兜底窗口的校验前移到启动期。错误配置（TTL 低于
	// 单任务预算、窗口 <= TTL）必须在 worker 上线前失败，而不是等到第一轮
	// 扫描被当成瞬时 DB 故障反复重试。
	if err := engine.ValidateSubscriptionLeaseConfig(); err != nil {
		_ = redisRes.Close()
		_ = logsRes.Close()
		_ = dbRes.Close()
		return fmt.Errorf("worker: subscription lease config: %w", err)
	}

	// C2 GPU 站点成本账（Task 5c / G-OPS-012）：账本只读写
	// deployment_billing_segments / deployment_cost_facts，**不触碰任何用户
	// 资金入口**。审计走日志库（deploy.billing_settle / deploy.billing_reconcile）。
	// 账本在这里构造（不启动 goroutine），失败路径只需关闭连接池。
	deployAudit := audit.New(sqlLogs)
	deployLedger, err := deploybilling.NewLedger(deploybilling.LedgerConfig{
		DB:       sqlDB,
		Provider: deploybilling.ProviderFromSettings(center, cipher, log),
		Logger:   log,
		Audit: func(ctx context.Context, action string, detail map[string]any) error {
			raw, marshalErr := json.Marshal(detail)
			if marshalErr != nil {
				return marshalErr
			}
			return deployAudit.Log(ctx, action, 0, raw)
		},
	})
	if err != nil {
		_ = redisRes.Close()
		_ = logsRes.Close()
		_ = dbRes.Close()
		return fmt.Errorf("worker: deploy billing ledger: %w", err)
	}

	// 任务轮询（M6d T6b）：装配插件运行时 + async_task_poll 循环。失败即
	// 阻止 worker 上线——没有轮询器时任务会一直停在 submitted，预扣永久冻结。
	stopTaskPoller, err := startTaskPoller(ctx, cfg, sqlDB, center, engine, log)
	if err != nil {
		_ = redisRes.Close()
		_ = logsRes.Close()
		_ = dbRes.Close()
		return fmt.Errorf("worker: start task poller: %w", err)
	}

	// 排行榜快照 worker（D6a / M09 F-09-29~33）：每 period 一个 ticker，用
	// Redis 租约收敛多副本执行。与计费兜底/任务轮询并行，但独立取消：
	// 它只读 usage_logs、写 rankings_* 与 Redis 缓存，不参与资金路径。
	// 依赖装配在同一次启动内完成，失败只可能是编程错误，RunLoop 的校验
	// 在 goroutine 内记录 Error，不阻塞 worker 上线（排行榜是 P1 展示面）。
	rankingsCtx, cancelRankings := context.WithCancel(ctx)
	var rankingsWG sync.WaitGroup
	rankingsWG.Add(1)
	go func() {
		defer rankingsWG.Done()
		if err := rankings.RunLoop(rankingsCtx, rankings.Deps{
			MainDB: sqlDB,
			LogsDB: sqlLogs,
			Redis:  rr.client,
			Config: center,
			Logger: log,
		}); err != nil {
			log.Error("rankings worker exited with error", "error", err)
		}
	}()

	// S4 自动续费（P-14/P-16/P-17/R-40/R-42）：与 B12 同线程执行，因此这里
	// 只构造 worker（不启动 goroutine），把 RunPass 交给 subscriptionworker。
	// 扣款复用 TopupService 的下单/结算路径（R-41）；令牌经 credential
	// SecretSealer 解封（P-14）。装配失败阻止 worker 上线：没有自动续费循环
	// 时，窗口内的实例会静默过期（用户以为已开启自动续费）。
	autoRenewTopup, err := billing.NewTopupService(billing.TopupServiceConfig{
		DB:     gormDB,
		Config: center,
		Sealer: cipher,
		Logger: log,
		// Webhook 不接线：自动续费的结算分支（renew）只推进实例并写订阅
		// 事件，不产生 payment_success 投递；充值事件的 producer 在下方
		// 构造，本服务只用于续费下单/扣款。
	})
	if err != nil {
		cancelRankings()
		rankingsWG.Wait()
		_ = redisRes.Close()
		_ = logsRes.Close()
		_ = dbRes.Close()
		return fmt.Errorf("worker: auto-renew topup service: %w", err)
	}
	autoRenewNotifyStore, err := notify.NewStore(sqlDB)
	if err != nil {
		cancelRankings()
		rankingsWG.Wait()
		_ = redisRes.Close()
		_ = logsRes.Close()
		_ = dbRes.Close()
		return fmt.Errorf("worker: auto-renew notify store: %w", err)
	}
	// W10-EM1：email 投递生产者（低余额、自动续费失败、渠道自动禁用）。
	// 与 webhook 生产者共用同一个 Store/配置中心快照；SMTP 是否可用不影响
	// 入队（未配置时 worker 不领取，配置修复后原样发出）。
	emailProducer, err := notify.NewEmailEventProducer(autoRenewNotifyStore, center, cfg.PublicBaseURL, log)
	if err != nil {
		cancelRankings()
		rankingsWG.Wait()
		_ = redisRes.Close()
		_ = logsRes.Close()
		_ = dbRes.Close()
		return fmt.Errorf("worker: email event producer: %w", err)
	}
	autoRenewWorker, err := billing.NewAutoRenewWorker(billing.AutoRenewDeps{
		Subs:     store.NewSubscriptionRepo(sqlDB),
		Methods:  store.NewPaymentMethodRepo(sqlDB),
		Charger:  autoRenewTopup,
		Sealer:   cipher,
		Config:   center,
		Redis:    rr.client,
		Notifier: newRenewalFailureNotifier(sqlDB, autoRenewNotifyStore, emailProducer, log),
		Logger:   log,
	})
	if err != nil {
		cancelRankings()
		rankingsWG.Wait()
		_ = redisRes.Close()
		_ = logsRes.Close()
		_ = dbRes.Close()
		return fmt.Errorf("worker: auto-renew worker: %w", err)
	}

	// B12 订阅到期 / 额度重置 / 预扣幂等记录清理（billing-06 §9.3、
	// billing-08 §7）。与排行榜循环同样的装配形态：独立取消 + WaitGroup，
	// 停机时先于连接池关闭退出。三条维护都只读写订阅域表，不参与请求
	// 路径；多副本重复执行由 Redis SET NX PX 锁收敛，且 SQL 条件更新本身
	// 幂等（到期终态单向、重置推进游标、清理按时间窗）。
	subscriptionCtx, cancelSubscriptions := context.WithCancel(ctx)
	var subscriptionsWG sync.WaitGroup
	subscriptionsWG.Add(1)
	go func() {
		defer subscriptionsWG.Done()
		repo := store.NewSubscriptionRepo(sqlDB)
		if err := subscriptionworker.RunLoop(subscriptionCtx, subscriptionworker.Deps{
			Repo:    repo,
			Cleaner: repo,
			Redis:   rr.client,
			Logger:  log,
			// P-17：自动续费与到期/重置同一 goroutine、同一 tick。
			AutoRenew: autoRenewWorker.RunPass,
		}); err != nil {
			log.Error("subscription worker exited with error", "error", err)
		}
	}()

	// N8 Webhook 发送/重试（billing-06 §9.7 / modules-08 F-08-20）：独立 goroutine
	// + cancel/WaitGroup，ticker 1 分钟，停机时先于连接池关闭退出。发送器与
	// gateway 的测试端点共用配置中心快照与同一把 SecretSealer，保证「测试通过」
	// 与真实投递使用同一份签名/目标配置。
	//
	// 装配失败（Store 构造）阻止 worker 上线：没有该循环时业务事件会永远停在
	// queued，且恢复端点/暂停状态失去消费者。循环内的 DB/Redis 故障只记录，
	// 下一轮重试。
	notifyStore, err := notify.NewStore(sqlDB)
	if err != nil {
		cancelRankings()
		rankingsWG.Wait()
		cancelSubscriptions()
		subscriptionsWG.Wait()
		_ = redisRes.Close()
		_ = logsRes.Close()
		_ = dbRes.Close()
		return fmt.Errorf("worker: notify store: %w", err)
	}
	// W10-EMAIL：worker 角色也必须在启动期补种模板。此前播种只发生在
	// gateway 装配（httpapi assembly）与 channel health 的 best-effort 路径，
	// 单独部署 worker（或 health 装配失败）时 email worker 会把所有投递判成
	// "template is missing"。模板行缺失属于配置性故障：失败只 Warn，不阻止
	// worker 上线（与 channel health 的既有口径一致），但必须留下明确日志。
	seedNotifyTemplates(ctx, notifyStore, log)
	// B1b：worker 侧的充值结算（过期回收/支付宝轮询兜底）也走 settleOrder，
	// 必须与 gateway 共享同一生产者语义。订阅过滤读取同一配置中心快照。
	// 同一个 WebhookSender 也作为 balance_low 扫描器的订阅面：事件订阅语义
	// 只有一份真源。
	webhookSender := notify.NewWebhookSender(center, cipher)
	webhookEvents, err := notify.NewWebhookEventProducer(notifyStore, webhookSender, log)
	if err != nil {
		cancelRankings()
		rankingsWG.Wait()
		cancelSubscriptions()
		subscriptionsWG.Wait()
		_ = redisRes.Close()
		_ = logsRes.Close()
		_ = dbRes.Close()
		return fmt.Errorf("worker: webhook event producer: %w", err)
	}
	webhookCtx, cancelWebhook := context.WithCancel(ctx)
	var webhookWG sync.WaitGroup
	webhookWG.Add(1)
	go func() {
		defer webhookWG.Done()
		if err := notify.RunWebhookWorkerLoop(webhookCtx, notify.WebhookWorkerDeps{
			Store:  notifyStore,
			Sender: webhookSender,
			Redis:  rr.client,
			Logger: log,
		}); err != nil {
			log.Error("webhook worker exited with error", "error", err)
		}
	}()

	// W10-EMAIL：email 投递 worker（modules-08 §3.9.6b，R-W10-06）。与 webhook
	// 同一生命周期形态：独立 cancel + WaitGroup，停机先于连接池关闭退出。
	// SMTP 配置复用 gateway 测试端点同一份快照与 SecretSealer；未配置时
	// worker 不领取投递（行保持 queued），而不是把行误标 failed。
	emailSender := notify.NewSMTPSender(center, cipher)
	emailCtx, cancelEmail := context.WithCancel(ctx)
	var emailWG sync.WaitGroup
	emailWG.Add(1)
	go func() {
		defer emailWG.Done()
		if err := notify.RunEmailWorkerLoop(emailCtx, notify.EmailWorkerDeps{
			Store:  notifyStore,
			Sender: emailSender,
			Logger: log,
		}); err != nil {
			log.Error("email worker exited with error", "error", err)
		}
	}()

	// N6 低余额扫描（billing-06 §9.6 / modules-08 F-08-11，tasks B4）：每 5
	// 分钟扫描 balance < notify.webhook.balance_low_threshold 的启用用户，写入
	// balance_low 投递行，由上面的 N8 worker 发送。独立 cancel/WaitGroup，
	// 停机先于连接池关闭退出；跨副本重复扫描由 dedupe_key 按天收敛。
	balanceCtx, cancelBalance := context.WithCancel(ctx)
	var balanceWG sync.WaitGroup
	balanceWG.Add(1)
	go func() {
		defer balanceWG.Done()
		if err := notify.RunBalanceLowWorkerLoop(balanceCtx, notify.BalanceLowWorkerDeps{
			Store:         notifyStore,
			Producer:      webhookEvents,
			EmailProducer: emailProducer,
			Config:        center,
			Subscription:  webhookSender,
			Logger:        log,
		}); err != nil {
			log.Error("balance low worker exited with error", "error", err)
		}
	}()

	// W7/M4-b 渠道健康体检（modules-06 F-06-12）：默认常开（R-M4-07），
	// 每 test_interval_minutes 探测 status IN (1,3) 的渠道，失败按 R-M4-01
	// 判定自动禁用，成功且开关开启时自动恢复（R-M4-09/15）。
	//
	// 装配失败只记录并继续：体检不是交易路径，缺 worker 不应阻止计费/任务
	// 上线（与任务轮询/计费的失败语义不同）。停机时 cancel + Wait 先于
	// Redis/DB 关闭（与其他后台循环同一顺序）。
	healthStop, err := startChannelHealthWorker(ctx, cfg, sqlDB, sqlLogs, rr.client, center, notifyStore, log)
	if err != nil {
		log.Error("worker: channel health worker disabled", "error", err)
		healthStop = nil
	}

	// M0 语义：装配全部成功（DB/日志库/Redis/配置中心/定价源/引擎/注册表）
	// 后才打 success marker；运维据此区分「进程起来了但装配失败」与
	// 「worker 已进入循环」。失败路径在 marker 之前返回，不会出现假成功。
	log.Info("worker started")

	// M5 Token 5：订单过期回收（F-04-07）与支付宝轮询兜底（F-04-06）。
	// 与 M3 兜底循环并行运行：两者依赖独立、失败语义一致（记录后下一轮
	// 重试）。TopupService 只用于渠道查询/关单与入账，复用 worker 的
	// 配置中心快照与 SecretSealer（支付宝密钥是密封配置）。
	topup, err := billing.NewTopupService(billing.TopupServiceConfig{
		DB:      gormDB,
		Config:  center,
		Sealer:  cipher,
		Logger:  log,
		Webhook: webhookEvents,
	})
	if err != nil {
		// 排行榜与订阅维护循环已经启动：先取消并等待它们退出，再关连接池，
		// 避免聚合/扫描 SQL 撞上已关闭的 *sql.DB（vet 的 context-leak 检查同口径）。
		cancelRankings()
		rankingsWG.Wait()
		cancelSubscriptions()
		subscriptionsWG.Wait()
		cancelWebhook()
		webhookWG.Wait()
		cancelEmail()
		emailWG.Wait()
		cancelBalance()
		balanceWG.Wait()
		// 体检循环可能正持有 Redis 锁或写 last_test_*，必须先于连接池关闭退出。
		if healthStop != nil {
			healthStop()
		}
		_ = redisRes.Close()
		_ = logsRes.Close()
		_ = dbRes.Close()
		return fmt.Errorf("worker: topup service: %w", err)
	}

	expiryCtx, cancelExpiry := context.WithCancel(ctx)
	var expiryWG sync.WaitGroup
	var expiryErr error
	expiryWG.Add(1)
	go func() {
		defer expiryWG.Done()
		expiryErr = billing.RunExpiryLoop(expiryCtx, billing.ExpiryWorkerDeps{
			DB:     gormDB,
			Topup:  topup,
			Logger: log,
		})
	}()

	// C2 GPU 计费 worker（N16）：60s 对账 + 每小时记账。独立取消 + WaitGroup，
	// 停机时先于连接池关闭退出。provider 每轮按配置中心快照重建（密钥可轮换）；
	// Redis 租约只做跨副本节流，SQL 唯一约束才是幂等真源。记账/对账失败只
	// 记录并下一轮重试，不阻断其它 worker。
	deployCtx, cancelDeploy := context.WithCancel(ctx)
	var deployWG sync.WaitGroup
	deployWG.Add(1)
	go func() {
		defer deployWG.Done()
		if err := deploybilling.RunLoop(deployCtx, deploybilling.WorkerDeps{
			Ledger:   deployLedger,
			Provider: deploybilling.ProviderFromSettings(center, cipher, log),
			Redis:    rr.client,
			Logger:   log,
		}); err != nil {
			log.Error("deploy billing worker exited with error", "error", err)
		}
	}()

	// 逐副本心跳（modules-08 §3.7 F-08-21）：与 gateway 同一套 key/周期。
	// 放在全部装配完成之后启动：装配失败直接返回，不会留下 TTL 内仍显示
	// 存活的幽灵条目；停机时先 Stop 删除 key，再断开 Redis。
	heartbeat, err := httpapi.StartInstanceHeartbeat(context.Background(), httpapi.InstanceHeartbeatConfig{
		Redis:      rr.client,
		InstanceID: httpapi.ResolveProcessInstanceID(),
		Role:       "worker",
		Version:    version.Version,
		StartedAt:  time.Now(),
		Logger:     log,
	})
	if err != nil {
		cancelExpiry()
		expiryWG.Wait()
		cancelBalance()
		balanceWG.Wait()
		stopTaskPoller()
		cancelRankings()
		rankingsWG.Wait()
		cancelSubscriptions()
		subscriptionsWG.Wait()
		cancelWebhook()
		webhookWG.Wait()
		cancelEmail()
		emailWG.Wait()
		cancelDeploy()
		deployWG.Wait()
		if healthStop != nil {
			healthStop()
		}
		_ = redisRes.Close()
		_ = logsRes.Close()
		_ = dbRes.Close()
		return fmt.Errorf("worker: start instance heartbeat: %w", err)
	}

	runErr := billing.RunWorkerLoop(ctx, billing.WorkerDeps{
		Abandoned:             registry,
		Recoverer:             engine,
		SubscriptionRecoverer: engine,
		LogsDB:                sqlLogs,
		Logger:                log,
	})
	// 先停后台循环（过期扫描 + 任务轮询 + 排行榜 + 订阅维护）再关连接池：
	// 否则下一次 DB 调用会拿到「连接已关闭」错误，把正常停机噪声打进告警。
	cancelExpiry()
	expiryWG.Wait()
	stopTaskPoller()
	cancelRankings()
	rankingsWG.Wait()
	cancelSubscriptions()
	subscriptionsWG.Wait()
	cancelWebhook()
	webhookWG.Wait()
	cancelEmail()
	emailWG.Wait()
	cancelBalance()
	balanceWG.Wait()
	cancelDeploy()
	deployWG.Wait()
	// 体检在停机窗口内不再发起新探测；Wait 保证进行中的探测与状态写完成
	// 后才关闭 Redis/DB（与 M4-a reporter 的清理顺序同口径）。
	if healthStop != nil {
		healthStop()
	}
	heartbeat.Stop()
	if runErr == nil {
		runErr = expiryErr
	}

	// 停机序与启动序相反：先断 Redis（循环已退出，不再有租约写入），
	// 再关日志库与主库。
	if err := redisRes.Close(); err != nil {
		log.Warn("close redis failed", "error", err)
	}
	if err := logsRes.Close(); err != nil {
		log.Warn("close logs db failed", "error", err)
	}
	if err := dbRes.Close(); err != nil {
		log.Warn("close db failed", "error", err)
	}
	log.Info("worker stopped")
	return runErr
}
