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
	"newtg/gateway/internal/clientip"
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
	// 计费进程级装配（N2/N5）：定价源构造写入进程级 QPU（admin API 的
	// 元↔积分换算基准），并接入钳制审计日志。必须在 Load 之后、任何
	// handler 读取 CurrentQuotaPerUnit 之前；非法 QPU 让启动失败。
	if _, err := initBillingAssembly(gormDB, center, cfg, log, "gateway"); err != nil {
		return nil, fmt.Errorf("gateway: billing assembly: %w", err)
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

	return httpapi.BuildGatewayRouter(httpapi.GatewayDeps{
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
	})
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
