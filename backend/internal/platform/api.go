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
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/billing"
	"newtg/gateway/internal/billing/pricing"
	"newtg/gateway/internal/channelhealth"
	"newtg/gateway/internal/clientip"
	"newtg/gateway/internal/config"
	"newtg/gateway/internal/configcenter"
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/httpapi"
	"newtg/gateway/internal/notify"
	"newtg/gateway/internal/perfmetrics"
	"newtg/gateway/internal/perfquery"
	"newtg/gateway/internal/ratelimit"
	"newtg/gateway/internal/rbac"
	"newtg/gateway/internal/store"
	"newtg/gateway/internal/tasks"
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
	// MJ 是绘图任务族提交 handler（W8/MJ-a2）；由 mountRelayRoutes 挂到根
	// mux 的 /mj/*（令牌链）。nil 表示本次装配没有 MJ 提交面。
	MJ *httpapi.MJHandler
	// RateLimiter 是 W7/M5-b 的准入限流器：用同一个 Redis 客户端构造一次，
	// 同时交给中继（chat/messages/多模态）与任务提交面。每次请求只做一次
	// EVAL，不新建客户端、不持有计数（计数在 Redis）。
	RateLimiter httpapi.RelayRateLimiter
	// AttemptFailureReporter 是 W7/M4-a 的自动禁用判定接缝（生产
	// *channelhealth.Reporter）。随装配启动、随 RunGateway ctx 退出；
	// nil 表示探针-only/测试装配，中继不做自动禁用。
	AttemptFailureReporter httpapi.AttemptFailureReporter
	// AttemptFailureReporterStop 取消 reporter 的唯一 goroutine 并等待它
	// 排空队列。runGateway 必须在关闭主库/日志库**之前**调用，否则停机窗口
	// 的排空写会撞上已关闭的连接池（I-2）。
	AttemptFailureReporterStop func()
	// PerfMetricsStop 取消模型性能采集器的周期任务并执行一次强制 flush
	// （G-M6C-13）。与 reporter 同理，必须在关闭日志库之前调用；nil 表示
	// PERF_METRICS_ENABLED=false（禁用时零开销，不构造采集器）。
	PerfMetricsStop func()
	// PluginRuntimeStop 停止插件注册表热刷新/状态循环，并在 Redis 关闭前
	// DEL 本实例的 newtg:plugin:runtime:<id>。nil 表示未装配插件运行时。
	PluginRuntimeStop func()
	// EmailSender 是事务性邮件适配器（W13-EMAIL）：同一实例同时实现验证码、
	// 密码重置与通知测试三个 handler 接缝。装配测试据此断言生产路径确实
	// 注入，而不是让端点退化到 503。
	EmailSender httpapi.EmailCodeSender
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

// newRelayRateLimiter 构造生产准入限流器（W7/M5-b）。
//
// handler 侧把 nil limiter 解释为「跳过限流」，那只允许出现在单测/降级装配里。
// 生产装配必须把「没有 Redis / 构造返回 nil」变成**启动错误**：静默返回 nil
// 会让 B11/B10/B12 三档桶在没有任何提示的情况下全部停用，而这正是限流要防
// 的滥用面（装配遗漏不应该等同于「管理员关闭了限流」）。
func newRelayRateLimiter(rdb *redis.Client) (httpapi.RelayRateLimiter, error) {
	if rdb == nil {
		return nil, errors.New("gateway: relay rate limiter requires a redis client")
	}
	limiter := ratelimit.New(rdb)
	if limiter == nil {
		return nil, errors.New("gateway: relay rate limiter construction returned nil")
	}
	return limiter, nil
}

// perfMetricsAssembly 是一次独立指标库装配的产物。
//
// source 语义：
//   - 配置禁用（PERF_METRICS_ENABLED=false）：nil，handler 走「采集未启用」
//     闸门；
//   - 启用但 DSN 缺失/连不上：*perfmetrics.UnavailableSource，handler 返回
//     pipeline.enabled=false + 脱敏原因码；绝不回退日志库/主库；
//   - 正常：*perfmetrics.Collector。
type perfMetricsAssembly struct {
	collector *perfmetrics.Collector
	source    perfmetrics.Source
	stop      func()
}

// newPerfMetricsAssembly 消费 PERF_METRICS_ENABLED / PERF_METRICS_DSN /
// PERF_METRICS_FLUSH_INTERVAL / PERF_METRICS_RETENTION_DAYS。
//
// 【独立指标库】只打开 PERF_METRICS_DSN，**绝不**回退 DATABASE_URL 或
// DB_LOGS_DSN（platform-05 §3.5.3 / platform-01 §30）。DSN 缺失或首次
// 连接失败不阻止网关启动：读面 fail-closed，给出不可用原因，采集器不构造、
// 不写任何数据库。PostgreSQL 指标库是 M7 指标库产品确定前的过渡实现。
func newPerfMetricsAssembly(ctx context.Context, cfg *config.Config,
	log *slog.Logger) (*perfMetricsAssembly, error) {

	if cfg == nil || !cfg.PerfMetricsEnabled {
		return &perfMetricsAssembly{}, nil
	}
	if strings.TrimSpace(cfg.PerfMetricsDSN) == "" {
		log.Warn("perf metrics enabled but PERF_METRICS_DSN is not set; /api/model-perf will be gated")
		return &perfMetricsAssembly{
			source: perfmetrics.NewUnavailableSource(perfmetrics.ReasonDSNMissing),
		}, nil
	}

	db, err := sql.Open("pgx", cfg.PerfMetricsDSN)
	if err != nil {
		// sql.Open 的解析错误可能带 DSN 片段；只记录脱敏提示，不回显 DSN。
		log.Warn("perf metrics: open metrics database failed; /api/model-perf will be gated")
		return &perfMetricsAssembly{
			source: perfmetrics.NewUnavailableSource(perfmetrics.ReasonStorageUnavailable),
		}, nil
	}
	// 指标库是旁路观测面，连接数不需要与业务库同规模。
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(5)

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	err = db.PingContext(pingCtx)
	cancel()
	if err != nil {
		_ = db.Close()
		// 不记录底层连接错误：pgx 的错误可能包含 host/user，且部署方通常
		// 能通过 /api/model-perf 的 reason 与网络排查定位。绝不回显 DSN。
		log.Warn("perf metrics: metrics database unreachable; /api/model-perf will be gated")
		return &perfMetricsAssembly{
			source: perfmetrics.NewUnavailableSource(perfmetrics.ReasonStorageUnavailable),
		}, nil
	}

	collector, err := perfmetrics.NewCollector(perfmetrics.Config{
		Store:         store.NewModelPerfRepo(db),
		FlushInterval: cfg.PerfMetricsFlushInterval,
		Node:          cfg.NodeName,
		HourRetention: time.Duration(cfg.PerfMetricsRetentionDays) * 24 * time.Hour,
		Logger:        log,
	})
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("gateway: perf metrics collector: %w", err)
	}
	return &perfMetricsAssembly{
		collector: collector,
		source:    collector,
		stop: func() {
			// 先停采集器（强制 flush），再关指标库连接。
			collector.Stop()
			if closeErr := db.Close(); closeErr != nil {
				log.Warn("perf metrics: close metrics database failed", "error", closeErr)
			}
		},
	}, nil
}

// newGatewayAPI 构造 /api/* 的完整路由。db/logsDB/rdb 必须已通过
// OpenDB/OpenLogsDB/OpenRedis 验证连通性。
//
// 启动副作用（设计冻结）：
//   - configcenter.Load：配置快照必须先于任何 handler 读取；
//   - configcenter.Subscribe：≤1s 的跨副本配置生效路径；
//   - tokenstore.Subscribe：GC-11 的令牌缓存第二失效通道（强制）。
//
// newDemoAwareBillingEngine 构造 role 计费引擎，并注入**带演示覆盖**的配置面
// （控制端 W13 裁决 I2）。
//
// 为什么必须是唯一构造点：gateway 与 worker 都要按同一有效值判定演示豁免
// （gateway 免单、worker 兜底退款/补扣）。任何一处直接把 `center` 传给
// billing.NewEngine，就会在 NEWTG_DEMO_ENABLED 覆盖设置时出现「守卫按 env
// 生效、引擎按设置计费」的跨进程分裂，而且这类回归不会让行为测试必然变红。
// 因此包装动作集中在这里，装配测试用 Engine.EffectiveConfig 直接断言。
func newDemoAwareBillingEngine(
	gormDB *gorm.DB, logsDB *sql.DB, rdb *redis.Client,
	center config.ConfigGetter, demoOverride *bool,
	source pricing.Source, registry billing.Registry, saturator billing.Saturator,
) *billing.Engine {
	return billing.NewEngine(gormDB, logsDB, rdb,
		config.WithDemoOverride(center, demoOverride), source, registry, saturator)
}

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
	// I2（控制端裁决）：演示模式的部署侧覆盖 NEWTG_DEMO_ENABLED。把配置
	// 读取面包装一次后同时交给**守卫、中继额度与计费引擎**，三处读到的
	// 是同一个有效值，不会出现「守卫关了、额度还在扣」的分裂。
	// 未设置 env 时 WithDemoOverride 原样返回 center（行为不变）。
	//
	// 包装点唯一：引擎由 newDemoAwareBillingEngine 构造，守卫/额度/中继的
	// 读取面从引擎的有效配置面派生，避免两处各自包装后再次漂移。
	// I2 的 MJ 结算守卫需要计费引擎，因此推迟到引擎构造之后（见下方
	// engine.ValidateSubscriptionLeaseConfig 之后的 mjSettlerGuard）。
	// 计费进程级装配（N2/N5）：定价源构造写入进程级 QPU（admin API 的
	// 元↔积分换算基准），并接入钳制审计日志。必须在 Load 之后、任何
	// handler 读取 CurrentQuotaPerUnit 之前；非法 QPU 让启动失败。
	//
	// 返回的 source 实例必须保留：它同时是（a）真实计费引擎的定价源，
	// （b）管理面定价写成功后的缓存失效目标（R-63）。两处必须共用同一实例，
	// 否则失效钩子清的是另一个缓存，定价变更仍要等 30s TTL。
	source, err := initBillingAssembly(gormDB, db, center, cfg, log, "gateway")
	if err != nil {
		return nil, fmt.Errorf("gateway: billing assembly: %w", err)
	}

	// T9：真实计费引擎。R-41/R-88 的生产保证要求 Redis 非 nil（结算双扣
	// 窗口靠 requestID 锁收窄）；Saturator 写独立日志库（usage_logs）。
	//
	// W10-N2：`log.batch_enabled` 打开时日志写入器改为「先 XADD、失败同步
	// 回退」；默认关闭，行为与既有同步路径完全一致。live_feed 投递无论在
	// 哪种模式下都保持「只有真正插入新行才投递」。
	feedPublisher := billing.NewRedisFeedPublisher(rdb)
	// G-M6C-13：独立指标库装配（禁用时 source=nil）。在 DBLogger 构造前
	// 创建，使每个请求的最终结算行能同步采样；周期刷写 goroutine 在装配
	// 末尾启动，避免装配失败时留下后台任务。
	perfAssembly, err := newPerfMetricsAssembly(ctx, cfg, log)
	if err != nil {
		return nil, err
	}
	// 装配中途失败时必须关闭已打开的指标库连接；成功移交后由 assembly
	// 的 PerfMetricsStop 负责（stop 幂等）。
	perfOwned := true
	defer func() {
		if perfOwned && perfAssembly != nil && perfAssembly.stop != nil {
			perfAssembly.stop()
		}
	}()
	var perfRecorder billing.PerfMetricsRecorder
	if perfAssembly.collector != nil {
		perfRecorder = perfAssembly.collector
	}
	engine := newDemoAwareBillingEngine(gormDB, logsDB, rdb, center, cfg.DemoEnabled, source,
		billing.NewRegistry(rdb), newUsageLogDBLogger(logsDB, feedPublisher, center, rdb, log, perfRecorder))
	// spec §5.2：冻结请求时间与时段倍率，供任务轮询/崩溃恢复的异步结算
	// 读回（不使用 time.Now() 重新匹配窗口）。
	engine.SetRequestPricingStore(store.NewRequestPricingRepo(db))
	// spec §5.2：客户折扣与 gateway/worker 必须同一份折扣源，否则任务结算
	// 会按原价计费（或与预扣命中不同规则）。与上面同为装配期一次写入。
	engine.SetDiscountStore(store.NewCustomerDiscountRepo(db))
	// 其余读取面（守卫、中继额度、管理面 RouterConfig）派生自引擎的有效
	// 配置面，确保三处与计费看到同一个 NEWTG_DEMO_ENABLED 有效值。
	demoConfig := engine.EffectiveConfig()
	// S4a：订阅是请求路径的资金来源之一，必须装配到引擎；未装配时引擎
	// 会保持纯钱包行为（订阅偏好被忽略）。
	engine.SetSubscriptionFunding(billing.NewStoreSubscriptionFunding(
		store.NewSubscriptionRepo(db), db))
	// M-1 复核：启动期校验租约 TTL / 兜底窗口，错误配置不得让 gateway 上线。
	if err := engine.ValidateSubscriptionLeaseConfig(); err != nil {
		return nil, fmt.Errorf("gateway: subscription lease config: %w", err)
	}
	// I2 / W8/MJ-b：mj.enabled=true 时必须存在 MJ 结算驱动，否则每笔普通
	// 提交 Detach 后的 hold 会永久冻结。worker 侧用同一构造器运行驱动；
	// gateway 这里也构造一份，只为让守卫验证「本部署确实带 MJ 结算路径」。
	// 构造不会启动任何 goroutine。
	mjDriver, _, err := buildMJPollDriver(tasks.NewRepo(db), center, engine,
		httpapi.NewSecretSealer(cfg.CredentialEncryptionKey), log)
	if err != nil {
		return nil, fmt.Errorf("gateway: %w", err)
	}
	if err := mjSettlerGuard(center, mjDriver.PollTask); err != nil {
		return nil, fmt.Errorf("gateway: %w", err)
	}
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

	// W15-C 每日签到（modules-01 §3.19 / modules-03 F-03-12）：签到记录与
	// checkin_bonus 入场必须同事务，因此服务持有主库 GORM 句柄。「今天」按
	// 站点时区判定，复用 httpapi 里 demo 额度的同一解析（非法时区告警回落
	// UTC，I-19）。
	checkinService, err := billing.NewCheckinService(billing.CheckinServiceConfig{
		DB:     gormDB,
		Config: center,
		Logger: log,
		Location: func() *time.Location {
			return httpapi.CheckinSiteLocation(center, log)
		},
	})
	if err != nil {
		return nil, fmt.Errorf("gateway: checkin service: %w", err)
	}

	// B1b：Webhook 业务事件生产者（payment_success / user_signup）。
	// 订阅过滤与 N8 worker 共用同一 WebhookSender（读取 notify.webhook.events
	// 快照）；投递行写主库，由 worker 进程按 1/5/25 分钟重试。生产者在
	// 这里构造一次，同时供充值结算与注册路径使用。
	notifyStore, err := notify.NewStore(db)
	if err != nil {
		return nil, fmt.Errorf("gateway: notify store: %w", err)
	}
	webhookEvents, err := notify.NewWebhookEventProducer(
		notifyStore, notify.NewWebhookSender(center, cipher), log)
	if err != nil {
		return nil, fmt.Errorf("gateway: webhook event producer: %w", err)
	}

	// M5 在线充值端点（Task 3）：密钥类配置（Stripe secret/webhook secret、
	// 支付宝私钥/公钥）经与配置中心同一个 SecretSealer 解封；webhook 限流
	// 复用同一 Redis 客户端，缺 Redis 时构造失败（fail closed）。
	topupHandler, err := httpapi.NewTopupHandlers(httpapi.TopupHandlerConfig{
		DB:      gormDB,
		Redis:   rdb,
		Config:  center,
		Sealer:  cipher,
		Logger:  log,
		Webhook: webhookEvents,
		// 订单端点按 orders:read/orders:manage 判定管理员范围；必须复用
		// 同一个 authz 实例，才能拿到 SessionAuth 未填充的权威权限集合。
		Authz: authz,
		// S3 退款的资金动作审计（日志库）。
		Audit: auditor,
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
		// R-M3-11：钱包流水读取（列表+详情共用）的用户维限流桶。
		Redis: rdb,
	})
	if err != nil {
		return nil, fmt.Errorf("gateway: wallet handlers: %w", err)
	}

	// W7/M4-a/b：渠道自动禁用/恢复的异步执行面。构造失败属于装配错误（条件
	// 更新是唯一状态迁移，缺 Store 会把「判定成功」变成静默无操作），直接
	// 阻止上线。通知复用主库的 notification_deliveries；W10-EM1 起管理员侧
	// 事件同时进入 email 队列（SMTP 未配置时行留队，由 worker 稍后发出）。
	//
	// 手工连通性测试（M4-b / R-M4-15）复用同一个 Reporter：handler 只上报
	// ProbeResult，判定/条件更新/审计/站内通知仍由 reporter 的后台 goroutine
	// 完成，且它只会禁用、永不自动恢复。
	channelEmailProducer, err := notify.NewEmailEventProducer(notifyStore, center, cfg.PublicBaseURL, log)
	if err != nil {
		return nil, fmt.Errorf("gateway: channel auto-disable email producer: %w", err)
	}
	channelNotifier, err := notify.NewService(notify.ServiceConfig{
		Store:         notifyStore,
		EmailProducer: channelEmailProducer,
		Logger:        log,
	})
	if err != nil {
		return nil, fmt.Errorf("gateway: channel auto-disable notifier: %w", err)
	}
	attemptReporter, err := channelhealth.NewReporter(channelhealth.ReporterConfig{
		Store:    store.NewChannelRepo(db),
		Config:   center,
		Audit:    auditor,
		Notifier: channelNotifier,
		Logger:   log,
		// 相对路径即可：通知中心的 LinkURL 由前端自行拼接站点基址，
		// 不依赖 PUBLIC_BASE_URL（为空时也不产生死链）。
		ChannelsURL: "/admin/channels",
	})
	if err != nil {
		return nil, fmt.Errorf("gateway: channel auto-disable reporter: %w", err)
	}

	// W11：渠道凭据揭示的步进认证门（security-03 §2.10）。复用用户口令/TOTP
	// 材料源：已启用 MFA 只接受 totp_code，未启用只接受 password。缺门时
	// handler 会 fail-closed（503），但生产不能依赖降级——构造失败即启动失败。
	// 门实例与账号 handler 内部的实例各自持有同一 Redis 计数桶，语义一致。
	revealSource, err := httpapi.NewSQLAccountReauthSource(db, cfg.CredentialEncryptionKey)
	if err != nil {
		return nil, fmt.Errorf("gateway: channel reveal reauth source: %w", err)
	}
	channelRevealGate := auth.NewReauthGate(rdb, revealSource, auth.VerifyTOTP)

	// 模型性能测速：与体检/手工测试共用同一出站客户端与凭据解封面，不新增
	// 第二套上游策略。30s 覆盖「建连 + 整段生成」（R-M4-14 的例外口径）。
	perfRunner, err := channelhealth.NewPerfRunner(channelhealth.ProberConfig{
		Opener:  channelSealer,
		Client:  httpapi.NewChannelProbeClient(),
		Timeout: 30 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("gateway: channel performance runner: %w", err)
	}

	channelHandler := httpapi.NewChannelHandler(
		store.NewChannelRepo(db),
		store.NewTokenRepo(db),
		channelSealer,
		httpapi.ChannelHandlerOptions{
			Tester:        httpapi.NewChannelConnectivityTester(channelSealer, nil),
			Idempotency:   httpapi.NewRedisIdempotencyStore(rdb),
			ProbeReporter: attemptReporter,
			// 手工测试与体检共用 rl.health 阈值（复核 M-6）：同一配置中心快照。
			Config: center,
			Plugins: httpapi.NewTaskPluginCatalog(
				factoryKeys, store.NewTaskPluginRepo(db)),
			// W11：揭示端点的管理员维限流（10 次/小时）与步进认证。
			Redis:  rdb,
			Reauth: channelRevealGate,
			Audit:  auditor,
			// W16-1：渠道指标直查日志库 usage_logs（+60s Redis 缓存）。
			// 主库渠道行与日志库分离，指标仓储必须拿 logsDB，不能拿 db。
			Metrics: store.NewChannelMetricsRepo(logsDB),
			// 测速结果保存到主库（依赖 channels/users 外键），不是日志库。
			Performer:   perfRunner.Run,
			Performance: store.NewSQLModelPerformanceStore(db),
		},
	)

	// W7/M5-b：限流器只构造一次（无状态包装，复用上面的 Redis 客户端）。
	// 中继面与任务提交面必须拿到同一个实例：两处各建一个包装虽无害，但会让
	// 「跨副本共享计数」的验证失去单一装配点。
	//
	// 构造失败必须让启动失败（见 newRelayRateLimiter）：handler 把 nil 解释为
	// 「跳过限流」，静默 nil 会让三档桶在无提示的情况下全部停用。
	rateLimiter, err := newRelayRateLimiter(rdb)
	if err != nil {
		return nil, err
	}

	// W13-EMAIL：事务性邮件发送器只构造一次，验证码 / 密码重置 / 通知测试
	// 三个接缝共用同一个出口与同一份 notify.smtp.* 配置快照（与通知 worker、
	// SMTP 测试发送同源）。SMTP 未配置时 handler 统一 fail-closed 503
	// （email_unavailable），绝不返回 204 假装已发送。
	transactionalEmail := httpapi.NewTransactionalEmailSender(
		notify.NewSMTPSender(center, cipher))

	// 上游用量对账手动触发（Task 9）：复用 worker 周期任务的同一个构造器，
	// 保证两处数据源分工一致（usage_logs 在**日志库**，账号/快照/run 在主库）
	// 且共用同一份设置读取。装配失败阻止上线，而不是让触发端点静默 500。
	reconcileRunner, err := newReconcileRunner(workerTaskDeps{
		DB: db, LogsDB: logsDB, Center: center, Logger: log,
	})
	if err != nil {
		return nil, fmt.Errorf("gateway: reconcile runner: %w", err)
	}
	// 快照列表的缺省回看窗口与 runner 共用同一份设置读取：每次请求读一次内存
	// 快照，改 reconcile.lookback_days 无需重启。读取失败只是拿不到配置值，
	// 返回 0 让 handler 回落编译期默认，而不是让整个读端点 500。
	reconcileSettings := NewReconcileSettingsReader(center, log)

	router, err := httpapi.BuildGatewayRouter(httpapi.GatewayDeps{
		DB:       db,
		LogDB:    logsDB,
		Redis:    rdb,
		Sessions: auth.NewSessionManager(rdb, []byte(cfg.SessionSecret)),
		Config:   demoConfig,
		Sealer:   cipher,
		Audit:    auditor,
		Authz:    authz,
		// 性能页 overview 的 Prometheus 查询客户端。空 PROMETHEUS_URL 时
		// New 返回不可用实现（永不 nil），端点按「未接入」降级。
		//
		// **不走 egress.Guard**：PROMETHEUS_URL 是部署内地址
		// （http://prometheus:9090），basic/strict 策略默认拒绝私网地址，
		// 接进来会让监控随安全设置收紧而整体失效（spec §7.1）。安全性由
		// 「URL 只来自部署配置 + 查询名白名单」保证。
		Prom: perfquery.New(perfquery.Config{
			BaseURL:  cfg.PrometheusURL,
			Timeout:  cfg.PrometheusTimeout,
			CacheTTL: cfg.PrometheusCacheTTL,
		}),
		PromURL:                 cfg.PrometheusURL,
		SignupWebhook:           webhookEvents,
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
		CheckinService:          checkinService,
		// R-63：定价写成功后立即清空 30s 缓存；未接线时新价最迟 30s 才生效。
		PricingOnChange: pricingChanged,
		// 设置写成功后的广播必须复用同一个配置中心实例：其它副本的
		// pub/sub 失效路径与本地快照才会一致。
		SettingsBroadcaster: center,
		NodeName:            cfg.NodeName,
		// 逐副本心跳/实例列表的标识：INSTANCE_ID → hostname（与 D3 性能
		// 端点同一解析函数，避免两处漂移）。
		InstanceID: httpapi.ResolveProcessInstanceID(),
		StartedAt:  time.Now(),
		// 任务提交与中继共用同一个计费引擎会话工厂（M6d Task 6a）：
		// 预扣/退款/结算语义只能有一套实现。
		Billing: engine.NewSession,
		// F-05-08：采集管道未启用时 source 为 nil，/api/model-perf 走闸门。
		// 周期即使禁用也传入，避免 handler 回落到与部署配置不一致的默认值。
		PerfMetricsSource:        perfAssembly.source,
		PerfMetricsFlushInterval: cfg.PerfMetricsFlushInterval,
		// W7/M5-b：准入限流器复用同一个 Redis 客户端（跨副本共享计数），
		// 装配一次、由中继面与任务提交面共同持有（R-M5-04/07）。
		RateLimiter: rateLimiter,
		// W13-EMAIL：事务性邮件（验证码/密码重置/通知测试）共用一个适配器。
		EmailSender:        transactionalEmail,
		NotificationSender: transactionalEmail,
		// Task 9：对账手动触发与 worker 周期任务共用同一个 runner 构造器。
		ReconcileRunner: reconcileRunner,
		// 快照列表缺省窗口跟随 reconcile.lookback_days；读错误按脏值处理。
		ReconcileLookbackDays: func(ctx context.Context) int {
			settings, err := reconcileSettings.ReconcileSettings(ctx)
			if err != nil {
				return 0
			}
			return settings.LookbackDays
		},
	})
	if err != nil {
		return nil, err
	}

	// 插件注册表热刷新（G-M6D-04/05/21）：启动即 Reload，此后 30s 重载 +
	// 订阅 newtg:plugin:changed；每 30s 写本副本 runtime 状态。Start 不会因
	// Redis 故障失败——ticker 是 Redis 不可用时的收敛路径。
	pluginRuntimeStop := func() {}
	if pluginRuntime := router.PluginRuntime(); pluginRuntime != nil {
		pluginRuntime.Start(ctx)
		pluginRuntimeStop = pluginRuntime.Stop
	}

	// 单 goroutine、随网关 ctx 退出；ReportAttemptFailure/ReportProbeFailure
	// 只做非阻塞入队，请求响应不等待条件更新/审计/通知（brief 第 2 条）。
	//
	// I-2：用可独立取消的 ctx 启动，runGateway 在关闭 DB/日志库之前调用
	// Stop（cancel + Wait）——既保证停机窗口把队列排空，也保证不会在连接池
	// 关闭后还有后台写。
	reporterCtx, reporterCancel := context.WithCancel(ctx)
	attemptReporter.Start(reporterCtx)

	// G-M6C-13：采集器的周期刷写随 gateway ctx 退出；Stop 会取消循环、做
	// 一次强制 flush（含当前分钟），再关闭指标库连接。runGateway 在关闭
	// 日志库之前调用 PerfMetricsStop。装配成功后所有权移交给 assembly。
	perfStop := func() {}
	if perfAssembly.collector != nil {
		perfAssembly.collector.Start(ctx)
		perfStop = perfAssembly.stop
		perfOwned = false
	}

	return &gatewayAssembly{
		Handler:                router,
		Billing:                engine.NewSession,
		ModelStatus:            pricingRepo,
		Config:                 demoConfig,
		PricingSource:          source,
		PricingOnChange:        pricingChanged,
		Tasks:                  router.TasksHandler(),
		MJ:                     router.MJHandler(),
		RateLimiter:            rateLimiter,
		AttemptFailureReporter: attemptReporter,
		EmailSender:            transactionalEmail,
		PerfMetricsStop:        perfStop,
		PluginRuntimeStop:      pluginRuntimeStop,
		AttemptFailureReporterStop: func() {
			reporterCancel()
			attemptReporter.Wait()
		},
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
