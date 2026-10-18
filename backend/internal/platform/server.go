package platform

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"newtg/gateway/internal/billing"
	"newtg/gateway/internal/billing/pricing"
	"newtg/gateway/internal/config"
	"newtg/gateway/internal/configcenter"
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
	apiHandler, err := conn.newAPIHandler(ctx, cfg, db, logsDB, redisRes, log)
	if err != nil {
		_ = redisRes.Close()
		_ = logsDB.Close()
		_ = db.Close()
		return fmt.Errorf("gateway: assemble http api: %w", err)
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
	mux := newGatewayMux(checker, apiHandler)
	srv := newGatewayHTTPServer(SecurityHeaders(mux))
	ln, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		// 监听失败是主错误；资源清理失败不覆盖它。
		_ = redisRes.Close()
		_ = logsDB.Close()
		_ = db.Close()
		return fmt.Errorf("gateway: listen %s: %w", cfg.HTTPAddr, err)
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
	if err := center.Load(ctx); err != nil {
		_ = redisRes.Close()
		_ = logsRes.Close()
		_ = dbRes.Close()
		return fmt.Errorf("worker: load config center: %w", err)
	}
	if err := center.Subscribe(ctx); err != nil {
		_ = redisRes.Close()
		_ = logsRes.Close()
		_ = dbRes.Close()
		return fmt.Errorf("worker: subscribe config center: %w", err)
	}

	source, err := pricing.NewSource(gormDB, center)
	if err != nil {
		_ = redisRes.Close()
		_ = logsRes.Close()
		_ = dbRes.Close()
		return fmt.Errorf("worker: build pricing source: %w", err)
	}

	registry := billing.NewRegistry(rr.client)
	// Saturator 是 usage_logs 的落库实现（Task 6 的 DBLogger）。worker 的
	// 兜底退款只依赖钱包流水，日志是辅助状态面（R-14）；但装配真实的
	// Saturator 让引擎在将来被 worker 复用时行为与 gateway 一致。
	engine := billing.NewEngine(gormDB, sqlLogs, rr.client, center, source, registry,
		billing.NewDBLogger(sqlLogs))

	runErr := billing.RunWorkerLoop(ctx, billing.WorkerDeps{
		Abandoned: registry,
		Recoverer: engine,
		LogsDB:    sqlLogs,
		Logger:    log,
	})

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
