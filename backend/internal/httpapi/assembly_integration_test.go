//go:build integration

package httpapi

// A5 收口 MEDIUM-1 的装配回归：生产装配（BuildGatewayRouter）必须在调用
// NewRouter **之前**构造并播种 notify.Service，否则公告投递会使用 NewRouter
// 内部临时构造的未播种实例。
//
// 运行：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_a5' \
//	NEWTG_TEST_LOGS_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_a5_logs' \
//	  go test -tags=integration ./internal/httpapi/ -run TestBuildGatewayRouterWiresSeededNotifyService -v

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/notify"
	"newtg/gateway/internal/rbac"
)

func TestBuildGatewayRouterWiresSeededNotifyService(t *testing.T) {
	dsn := os.Getenv("NEWTG_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("NEWTG_TEST_DB_DSN 未设置，跳过集成测试")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("ping test db: %v", err)
	}

	logsDB := db
	if logsDSN := os.Getenv("NEWTG_TEST_LOGS_DSN"); logsDSN != "" {
		logs, err := sql.Open("pgx", logsDSN)
		if err != nil {
			t.Fatalf("open logs db: %v", err)
		}
		t.Cleanup(func() { _ = logs.Close() })
		logsDB = logs
	}

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })

	deps := GatewayDeps{
		DB:                      db,
		LogDB:                   logsDB,
		Redis:                   rdb,
		Sessions:                auth.NewSessionManager(rdb, []byte(testSessionSecret)),
		Config:                  mapConfig{},
		Audit:                   audit.New(logsDB),
		Authz:                   rbac.NewAuthzMiddleware(),
		TokenService:            &fakeTokenService{},
		SessionSecret:           []byte(testSessionSecret),
		CredentialEncryptionKey: "assembly-integration-credential-key",
		PublicBaseURL:           "http://localhost:3000",
		Logger:                  testLogger(),
	}
	rt, err := BuildGatewayRouter(deps)
	if err != nil {
		t.Fatalf("BuildGatewayRouter: %v", err)
	}

	// 1) NewRouter 不得走兜底分支：兜底实例未播种，正是 MEDIUM-1 的生产症状。
	if rt.notifyFallback {
		t.Fatal("BuildGatewayRouter 让 NewRouter 构造了未播种的 notify 兜底实例（装配顺序回归）")
	}
	if rt.notifyService == nil {
		t.Fatal("RouterConfig.NotifyService 为 nil（公告投递将静默跳过）")
	}

	// 2) 启动播种已生效：system.announcement/inapp 的每个注册语言都有行。
	var seeded int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM notification_templates
		WHERE "key" = 'system.announcement' AND channel = 'inapp'`).Scan(&seeded); err != nil {
		t.Fatalf("count seeded templates: %v", err)
	}
	if seeded == 0 {
		t.Fatal("notify.Service 未播种 system.announcement 模板")
	}

	// 3) 播种后的服务可真正解析模板：读取路径（allowBundle）即使不依赖播种
	// 也会回退 bundle，所以这里用 Send 路径的语义证据——List 返回的行
	// customized=false 且 updated_at 非空，说明来自 DB 而非仅 bundle。
	views, err := rt.notifyService.List(ctx, notify.TemplateFilter{})
	if err != nil {
		t.Fatalf("notify.Service.List: %v", err)
	}
	found := false
	for _, v := range views {
		if v.Key == "system.announcement" && v.Channel == "inapp" && !v.Customized && v.UpdatedAt != nil {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("system.announcement/inapp 没有来自 DB 的播种行（List 未观察到 updated_at）")
	}
}
