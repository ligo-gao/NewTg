package platform

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/config"
	"newtg/gateway/internal/httpapi"
	"newtg/gateway/internal/store"
	"newtg/gateway/internal/version"
	"newtg/gateway/migrations"
)

// RunGateway 以 gateway 角色运行。装配序（platform-03）：logger → OpenDB →
// OpenRedis →（DBMigrateOnStart 时）RunMigrate → metrics goroutine → HTTP server →
// 阻塞至 ctx done → 优雅停机（SHUTDOWN_GRACE_SECONDS 窗口内先停 HTTP 监听，再关 DB/Redis）。
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
	redisRes, err := conn.openRedis(cfg)
	if err != nil {
		// 正在返回打开 Redis 的主错误；清理 DB 的失败不覆盖它。
		_ = db.Close()
		return fmt.Errorf("gateway: open redis: %w", err)
	}

	if cfg.DBMigrateOnStart {
		// 经 connectors 钩子调用：生产即 RunMigrate，测试可注入 stub 断言调用与否。
		if err := conn.runMigrate(ctx, cfg); err != nil {
			// 主错误优先：迁移失败时尽力释放两个资源，关闭错误不再上报。
			_ = redisRes.Close()
			_ = db.Close()
			return fmt.Errorf("gateway: migrate on start: %w", err)
		}
	}

	// metrics 侧车：MetricsAddr=="0" 时 RunMetricsServer 直接返回 nil，goroutine 立即结束。
	go func() {
		if err := RunMetricsServer(ctx, cfg, log); err != nil {
			log.Error("metrics server exited with error", "error", err)
		}
	}()

	// HTTP 主服务：/healthz（恒 200）与 /readyz（五项实时判定）。
	checker := checkerFromResources(db, redisRes, int(expectedVersion), log)
	checker.node = cfg.NodeName
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", checker.HandlerHealthz)
	mux.HandleFunc("/readyz", checker.HandlerReadyz)
	// /api/*（管理面）与 /v1/*（中继面）只在资源是生产具体类型时挂载；
	// platform 单测注入的 stub Resource 无法还原 *sql.DB / *redis.Client，
	// 此时保持 M0 的 health-only mux（既有测试不触真 PG/Redis）。
	mountGatewayRoutes(mux, cfg, db, redisRes)
	srv := &http.Server{
		// 安全响应头在 mux 外层统一注入（security-08 §2.1/§5.2）：
		// 404、探针、后续 API 与 SSE 都必须经过同一层，避免按路由漏配。
		Handler:           SecurityHeaders(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}
	ln, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		// 监听失败是主错误；资源清理失败不覆盖它。
		_ = redisRes.Close()
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
	if err := db.Close(); err != nil {
		log.Warn("close db failed", "error", err)
	}
	if err := redisRes.Close(); err != nil {
		log.Warn("close redis failed", "error", err)
	}
	log.Info("gateway stopped")
	return nil
}

// mountGatewayRoutes 挂载管理面与中继面路由。
//
// 只有生产资源（OpenDB 的 *sql.DB、OpenRedis 的 redisResource）才能构造仓储与
// 会话管理器；测试 path 的 stubResource 无法还原具体类型，此时静默跳过——
// 这不是降级（生产永远命中具体类型），而是为了保持 M0 单测的 GC-8 隔离。
//
// 装配顺序（Task 8 phase 8B）：
//   - /api/* ：渠道管理端点 → SessionAuth（Cookie/Bearer）→ CSRF → Origin/Referer；
//   - /v1/*  ：中继 handler 自带 RelayAuth（令牌面，不使用 Cookie 会话）。
func mountGatewayRoutes(mux *http.ServeMux, cfg *config.Config, db, rds Resource) {
	sqlDB, ok := db.(*sql.DB)
	if !ok || sqlDB == nil {
		return
	}
	redisRes, ok := rds.(redisResource)
	if !ok || redisRes.client == nil {
		return
	}
	client := redisRes.client

	// 仓储与密钥封装（GC-8：凭据密文的唯一读写面）。
	tokenRepo := store.NewTokenRepo(sqlDB)
	channelRepo := store.NewChannelRepo(sqlDB)
	sealer := httpapi.NewSecretSealer(cfg.CredentialEncryptionKey)

	// 管理面：渠道端点自带权限检查，但仍需 SessionAuth 先注入 Principal（R-P9）。
	channelHandler := httpapi.NewChannelHandler(channelRepo, tokenRepo, sealer, httpapi.ChannelHandlerOptions{
		Tester:      httpapi.NewChannelConnectivityTester(sealer, nil),
		Idempotency: httpapi.NewRedisIdempotencyStore(client),
	})
	apiMux := http.NewServeMux()
	channelHandler.Register(apiMux)

	sessionMgr := auth.NewSessionManager(client, []byte(cfg.SessionSecret))
	apiHandler := httpapi.SessionAuth(sessionMgr, tokenRepo)(
		httpapi.CSRF(sessionMgr)(
			httpapi.OriginRefererCheck(splitPublicOrigins(cfg.PublicBaseURL))(apiMux),
		),
	)
	mux.Handle("/api/", apiHandler)

	// 中继面：/v1/chat/completions 与 /v1/models（鉴权中间件在 handler 内挂载）。
	relayHandler := httpapi.NewRelayHandler(httpapi.RelayHandlerOptions{
		Channels: channelRepo,
		Tokens:   tokenRepo,
		Access:   tokenRepo,
		Sealer:   sealer,
	})
	relayHandler.Register(mux)
}

// splitPublicOrigins 把 PUBLIC_BASE_URL 拆成 OriginRefererCheck 的允许清单。
// 配置目前是单值，但按逗号拆分可兼容多主机名部署；空值返回 nil，
// 让中间件退化为「与请求 Host 同源」（本地开发仍受保护）。
func splitPublicOrigins(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
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

// RunWorker 以 worker 角色运行：M0 装配到连上 DB/Redis 并打一条
// worker started 日志后阻塞等 ctx；调度循环 M3 填。
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

	db, err := conn.openDB(ctx, cfg)
	if err != nil {
		return fmt.Errorf("worker: open db: %w", err)
	}
	redisRes, err := conn.openRedis(cfg)
	if err != nil {
		// 正在返回打开 Redis 的主错误；清理 DB 的失败不覆盖它。
		_ = db.Close()
		return fmt.Errorf("worker: open redis: %w", err)
	}

	log.Info("worker started")
	<-ctx.Done()

	if err := db.Close(); err != nil {
		log.Warn("close db failed", "error", err)
	}
	if err := redisRes.Close(); err != nil {
		log.Warn("close redis failed", "error", err)
	}
	log.Info("worker stopped")
	return nil
}
