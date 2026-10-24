package platform

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/config"
)

// stubResource 是不触真 PG/Redis 的内存 Resource（GC-8）。
type stubResource struct {
	name   string
	closed bool
	events *[]string
}

func (s *stubResource) PingContext(ctx context.Context) error { return nil }

func (s *stubResource) Close() error {
	s.closed = true
	if s.events != nil {
		*s.events = append(*s.events, "close:"+s.name)
	}
	return nil
}

// stubConnectors 注入内存 stub，禁止单测触真 PG/Redis。
// runMigrate 记录调用次数并可注入错误，使 runGateway 的启动迁移分支可单测
// （M0 评审 Task 6）：仅用 stub 即可覆盖「按开关调用 / 不调用」。
type stubConnectors struct {
	db     *stubResource
	redis  *stubResource
	logs   *stubResource
	dbErr  error
	rdsErr error
	logErr error

	// apiHandler/apiErr 控制 runGateway 的 /api/* 装配（nil = 只跑探针）。
	apiHandler http.Handler
	apiErr     error

	migrateErr   error
	migrateCalls atomic.Int32
}

func (sc *stubConnectors) openDB(ctx context.Context, cfg *config.Config) (Resource, error) {
	if sc.dbErr != nil {
		return nil, sc.dbErr
	}
	return sc.db, nil
}

func (sc *stubConnectors) openLogsDB(ctx context.Context, cfg *config.Config) (Resource, error) {
	if sc.logErr != nil {
		return nil, sc.logErr
	}
	if sc.logs != nil {
		return sc.logs, nil
	}
	return &stubResource{name: "logs"}, nil
}

func (sc *stubConnectors) newAPIHandler(ctx context.Context, cfg *config.Config,
	db, logsDB, rds Resource, log *slog.Logger) (*gatewayAssembly, error) {
	if sc.apiErr != nil {
		return nil, sc.apiErr
	}
	if sc.apiHandler == nil {
		return nil, nil
	}
	return &gatewayAssembly{Handler: sc.apiHandler}, nil
}

func (sc *stubConnectors) openRedis(cfg *config.Config) (Resource, error) {
	if sc.rdsErr != nil {
		return nil, sc.rdsErr
	}
	return sc.redis, nil
}

func (sc *stubConnectors) runMigrate(ctx context.Context, cfg *config.Config) error {
	sc.migrateCalls.Add(1)
	return sc.migrateErr
}

// gatewayTestConfig 返回可在随机端口完成装配的最小配置：metrics 关闭、
// 启动迁移默认关闭，各用例按需覆盖单个字段。
func gatewayTestConfig(addr string) *config.Config {
	return &config.Config{
		AppEnv:               "production",
		LogLevel:             "info",
		LogFormat:            "json",
		HTTPAddr:             addr,
		MetricsAddr:          "0",
		ShutdownGraceSeconds: 5,
		DBMaxOpenConns:       5,
		DBMaxIdleConns:       5,
		RedisPoolSize:        5,
	}
}

// freeAddr 取一个随即释放的本机地址（测试监听端口用）。
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve addr: %v", err)
	}
	addr := ln.Addr().String()
	// 测试辅助：立即释放预留端口，关闭错误不影响后续监听断言。
	_ = ln.Close()
	return addr
}

// waitHealthy 轮询 /healthz 直至 200：以服务真实就绪代替固定 sleep，
// 避免慢机器上的偶然超时。
func waitHealthy(t *testing.T, addr string) {
	t.Helper()
	client := &http.Client{Timeout: 500 * time.Millisecond}
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := client.Get("http://" + addr + "/healthz")
		if err == nil {
			// 测试客户端只关心状态码；关闭错误不影响断言。
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("gateway http server not ready in 5s")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestNewGatewayMuxKeepsProbesAndMountsAPI：/healthz 与 /readyz 保持在
// 根路径，/api/* 交给管理面 handler（同一 server/mux）。
func TestNewGatewayMuxKeepsProbesAndMountsAPI(t *testing.T) {
	checker := NewChecker(nil, nil, 0, nil) // 依赖未配置：/readyz 恒 503，但必须可达
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Api-Path", r.URL.Path)
		w.WriteHeader(http.StatusTeapot)
	})
	mux := newGatewayMux(checker, api)

	serve := func(path string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		return rr
	}

	if rr := serve("/healthz"); rr.Code != http.StatusOK {
		t.Fatalf("/healthz = %d, want 200", rr.Code)
	}
	if rr := serve("/readyz"); rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz = %d, want 503（依赖未配置；端点必须已注册）", rr.Code)
	}
	rr := serve("/api/status")
	if rr.Code != http.StatusTeapot {
		t.Fatalf("/api/status = %d, want 418（应进入管理面 handler）", rr.Code)
	}
	if got := rr.Header().Get("X-Api-Path"); got != "/api/status" {
		t.Fatalf("api handler saw path %q, want /api/status（Handle 不裁剪前缀）", got)
	}
}

// TestNewGatewayMuxWithoutAPI：api 为 nil 时只暴露探针端点。
func TestNewGatewayMuxWithoutAPI(t *testing.T) {
	mux := newGatewayMux(NewChecker(nil, nil, 0, nil), nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("/api/status without api = %d, want 404", rr.Code)
	}
}

// TestNewGatewayHTTPServerHardening 覆盖 A2：server 构造必须带 32 KiB
// MaxHeaderBytes（默认 1 MiB 对未认证客户端过于宽松）与头部读取超时。
func TestNewGatewayHTTPServerHardening(t *testing.T) {
	srv := newGatewayHTTPServer(http.NotFoundHandler())
	if srv.MaxHeaderBytes != 32<<10 {
		t.Fatalf("MaxHeaderBytes = %d, want %d", srv.MaxHeaderBytes, 32<<10)
	}
	if srv.ReadHeaderTimeout != 5*time.Second {
		t.Fatalf("ReadHeaderTimeout = %v, want 5s", srv.ReadHeaderTimeout)
	}
}

// TestShutdownGrace：装配后的 gateway 跑在随机端口，ctx cancel（等价 SIGTERM 路径）后
// 在 SHUTDOWN_GRACE_SECONDS 窗口内退出且返回 nil；HTTP 监听已关、DB/Redis 已按序关闭。
func TestShutdownGrace(t *testing.T) {
	addr := freeAddr(t)
	var events []string
	db := &stubResource{name: "db", events: &events}
	rds := &stubResource{name: "redis", events: &events}

	cfg := gatewayTestConfig(addr)
	cfg.DBMigrateOnStart = false

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- runGateway(ctx, cfg, &stubConnectors{db: db, redis: rds})
	}()

	waitHealthy(t, addr)

	start := time.Now()
	cancel()

	grace := time.Duration(cfg.ShutdownGraceSeconds)*time.Second + 3*time.Second
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runGateway returned error: %v", err)
		}
		if elapsed := time.Since(start); elapsed > grace {
			t.Fatalf("shutdown took %v, exceeded grace window %v", elapsed, grace)
		}
	case <-time.After(grace + 2*time.Second):
		t.Fatal("runGateway did not exit within grace window")
	}

	// HTTP 监听必须已关闭。
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err == nil {
		// 连接已建立本身就说明监听未关闭；关闭错误无关断言。
		_ = conn.Close()
		t.Fatal("http listener still open after shutdown")
	}

	// DB / Redis 必须已关闭，且顺序为 DB 先、Redis 后。
	if !db.closed || !rds.closed {
		t.Fatalf("resources not closed: db=%v redis=%v", db.closed, rds.closed)
	}
	want := []string{"close:db", "close:redis"}
	if len(events) != len(want) {
		t.Fatalf("close events = %v, want %v", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("close order = %v, want %v", events, want)
		}
	}
}

// TestRunGatewayOpenDBError：DB 建连失败时 gateway 直接返回错误，不泄漏 Redis 连接。
func TestRunGatewayOpenDBError(t *testing.T) {
	cfg := &config.Config{
		AppEnv:    "production",
		LogFormat: "json",
		HTTPAddr:  freeAddr(t),
	}
	dbErr := errors.New("db down")
	rds := &stubResource{name: "redis"}

	err := runGateway(context.Background(), cfg, &stubConnectors{dbErr: dbErr, redis: rds})
	if !errors.Is(err, dbErr) {
		t.Fatalf("err = %v, want wrap %v", err, dbErr)
	}
	if rds.closed {
		t.Fatal("redis must not be closed when db open failed (never opened)")
	}
}

// TestRunGatewayMigrateOnStartFlag 验证 DB_MIGRATE_ON_START 是启动迁移的唯一开关：
// 开启时迁移钩子恰好调用一次，关闭时一次也不调用。用 stub 钩子即可覆盖，
// 无需真 PG（M0 评审 Task 6）。
func TestRunGatewayMigrateOnStartFlag(t *testing.T) {
	cases := []struct {
		name      string
		migrateOn bool
		wantCalls int32
	}{
		{name: "开启时调用迁移", migrateOn: true, wantCalls: 1},
		{name: "关闭时不调用迁移", migrateOn: false, wantCalls: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			addr := freeAddr(t)
			conn := &stubConnectors{
				db:    &stubResource{name: "db"},
				redis: &stubResource{name: "redis"},
			}
			cfg := gatewayTestConfig(addr)
			cfg.DBMigrateOnStart = tc.migrateOn

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- runGateway(ctx, cfg, conn) }()

			// /healthz 就绪时 runGateway 已越过迁移分支，计数已稳定。
			waitHealthy(t, addr)
			if got := conn.migrateCalls.Load(); got != tc.wantCalls {
				cancel()
				// 等待 runGateway 退出，避免失败路径遗留监听 goroutine。
				runErr := <-done
				t.Fatalf("runMigrate calls = %d, want %d (DB_MIGRATE_ON_START=%v); runGateway err = %v",
					got, tc.wantCalls, tc.migrateOn, runErr)
			}

			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("runGateway returned error: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("runGateway did not exit after ctx cancel")
			}
		})
	}
}

// TestRunGatewayMigrateOnStartError：启动迁移失败时 gateway 不得上线；
// 返回包装错误，并释放已建立的 DB / Redis 资源，避免连接泄漏。
func TestRunGatewayMigrateOnStartError(t *testing.T) {
	db := &stubResource{name: "db"}
	rds := &stubResource{name: "redis"}
	boom := errors.New("migration exploded")
	conn := &stubConnectors{db: db, redis: rds, migrateErr: boom}
	cfg := gatewayTestConfig(freeAddr(t))
	cfg.DBMigrateOnStart = true

	err := runGateway(context.Background(), cfg, conn)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want wrap %v", err, boom)
	}
	if got := conn.migrateCalls.Load(); got != 1 {
		t.Fatalf("runMigrate calls = %d, want 1", got)
	}
	if !db.closed || !rds.closed {
		t.Fatalf("resources not closed after migrate failure: db=%v redis=%v", db.closed, rds.closed)
	}
}

// TestMountRelayRoutesConcreteResources 验证生产具体类型（*sql.DB / redisResource）
// 会挂载 /v1/ 中继路由。sql.Open / redis.NewClient 都是惰性的，不建立连接
// （GC-8：单测不触真 PG/Redis）。管理面（含 /api/channels）由 newAPIHandler
// 在 httpapi 组合根中装配，其挂载由 httpapi 的装配测试覆盖。
func TestMountRelayRoutesConcreteResources(t *testing.T) {
	sqlDB, err := sql.Open("pgx", "postgres://user:pass@127.0.0.1:1/newtg?sslmode=disable")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() { _ = sqlDB.Close() }()
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	defer func() { _ = rdb.Close() }()

	mux := http.NewServeMux()
	mountRelayRoutes(mux, gatewayTestConfig("127.0.0.1:0"), sqlDB, nil, redisResource{client: rdb}, nil)

	cases := []struct {
		method string
		path   string
		want   string
	}{
		{http.MethodGet, "/v1/models", "GET /v1/models"},
		{http.MethodPost, "/v1/chat/completions", "POST /v1/chat/completions"},
	}
	for _, tc := range cases {
		_, pattern := mux.Handler(httptest.NewRequest(tc.method, tc.path, nil))
		if pattern != tc.want {
			t.Errorf("%s %s matched %q, want %q", tc.method, tc.path, pattern, tc.want)
		}
	}
}

// TestMountRelayRoutesSkipsStubResources 冻结「非具体资源保持 health-only mux」：
// platform 单测的 stubResource 无法还原 *sql.DB / *redis.Client，不得挂载业务路由。
func TestMountRelayRoutesSkipsStubResources(t *testing.T) {
	mux := http.NewServeMux()
	mountRelayRoutes(mux, gatewayTestConfig("127.0.0.1:0"),
		&stubResource{name: "db"}, nil, &stubResource{name: "redis"}, nil)

	for _, path := range []string{"/v1/models", "/v1/chat/completions", "/pg/chat/completions", "/api/channels"} {
		if _, pattern := mux.Handler(httptest.NewRequest(http.MethodGet, path, nil)); pattern != "" {
			t.Fatalf("%s unexpectedly mounted (pattern %q) for stub resources", path, pattern)
		}
	}
}

// TestEffectivePublicBaseURL：/pg 的 Origin 白名单与管理面同口径——
// 显式 PUBLIC_BASE_URL 优先，为空时回退 site.server_address，均不可用时
// 返回空串（OriginRefererCheck 退化为同源 Host 比对，仍 fail-closed）。
func TestEffectivePublicBaseURL(t *testing.T) {
	config := billingAssemblyConfigReader{
		"site.server_address": json.RawMessage(`"https://gw.example.com/"`),
	}
	cases := []struct {
		name     string
		explicit string
		config   billingAssemblyConfigReader
		want     string
	}{
		{"显式优先", "https://a.example.com", config, "https://a.example.com"},
		{"回退站点地址", "", config, "https://gw.example.com/"},
		{"两者都缺", "  ", nil, ""},
		{"配置解析失败", "", billingAssemblyConfigReader{
			"site.server_address": json.RawMessage(`123`),
		}, ""},
	}
	for _, tc := range cases {
		if got := effectivePublicBaseURL(tc.explicit, tc.config); got != tc.want {
			t.Errorf("%s: effectivePublicBaseURL = %q, want %q", tc.name, got, tc.want)
		}
	}
}
