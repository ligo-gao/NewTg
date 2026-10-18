package platform

// gateway 角色的管理面装配：配置中心快照/订阅 → 令牌缓存订阅 → 审计 →
// httpapi 组合根。所有启动副作用都在监听端口之前完成，任何一步失败都让
// 进程启动失败（绝不以「无配置订阅/无令牌缓存失效/无审计」的状态上线）。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/redis/go-redis/v9"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/config"
	"newtg/gateway/internal/configcenter"
	"newtg/gateway/internal/httpapi"
	"newtg/gateway/internal/rbac"
	"newtg/gateway/internal/tokenstore"
)

// newAPIHandler 是 connectors 的生产实现：把 Resource 还原为具体类型后
// 交给 newGatewayAPI。类型不匹配说明装配被错误调用（例如测试 stub 走了
// 生产 connectors），直接报错而不是静默跳过 API 装配。
func (prodConnectors) newAPIHandler(ctx context.Context, cfg *config.Config,
	db, logsDB, rds Resource, log *slog.Logger) (http.Handler, error) {

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
	rdb *redis.Client, log *slog.Logger) (http.Handler, error) {

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
	audit.SetDefault(auditor)

	return httpapi.BuildGatewayRouter(httpapi.GatewayDeps{
		DB:                      db,
		LogDB:                   logsDB,
		Redis:                   rdb,
		Sessions:                auth.NewSessionManager(rdb, []byte(cfg.SessionSecret)),
		Config:                  center,
		Sealer:                  cipher,
		Audit:                   auditor,
		Authz:                   rbac.NewAuthzMiddleware(),
		TokenService:            tokens,
		SessionSecret:           []byte(cfg.SessionSecret),
		CredentialEncryptionKey: cfg.CredentialEncryptionKey,
		PublicBaseURL:           cfg.PublicBaseURL,
		Logger:                  log,
	})
}
