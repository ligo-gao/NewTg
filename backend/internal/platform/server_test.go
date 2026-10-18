package platform

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"

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
type stubConnectors struct {
	db     *stubResource
	redis  *stubResource
	dbErr  error
	rdsErr error
}

func (sc stubConnectors) openDB(ctx context.Context, cfg *config.Config) (Resource, error) {
	if sc.dbErr != nil {
		return nil, sc.dbErr
	}
	return sc.db, nil
}

func (sc stubConnectors) openRedis(cfg *config.Config) (Resource, error) {
	if sc.rdsErr != nil {
		return nil, sc.rdsErr
	}
	return sc.redis, nil
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

// TestShutdownGrace：装配后的 gateway 跑在随机端口，ctx cancel（等价 SIGTERM 路径）后
// 在 SHUTDOWN_GRACE_SECONDS 窗口内退出且返回 nil；HTTP 监听已关、DB/Redis 已按序关闭。
func TestShutdownGrace(t *testing.T) {
	addr := freeAddr(t)
	var events []string
	db := &stubResource{name: "db", events: &events}
	rds := &stubResource{name: "redis", events: &events}

	cfg := &config.Config{
		AppEnv:               "production",
		LogLevel:             "info",
		LogFormat:            "json",
		HTTPAddr:             addr,
		MetricsAddr:          "0", // 不启动 metrics 侧车
		DBMigrateOnStart:     false,
		ShutdownGraceSeconds: 5,
		DBMaxOpenConns:       5,
		DBMaxIdleConns:       5,
		RedisPoolSize:        5,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- runGateway(ctx, cfg, stubConnectors{db: db, redis: rds})
	}()

	// 等 HTTP 服务就绪。
	client := &http.Client{Timeout: 500 * time.Millisecond}
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := client.Get("http://" + addr + "/healthz")
		if err == nil {
			// 测试客户端只关心状态码；关闭错误不影响断言。
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("gateway http server not ready in 5s")
		}
		time.Sleep(20 * time.Millisecond)
	}

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

	err := runGateway(context.Background(), cfg, stubConnectors{dbErr: dbErr, redis: rds})
	if !errors.Is(err, dbErr) {
		t.Fatalf("err = %v, want wrap %v", err, dbErr)
	}
	if rds.closed {
		t.Fatal("redis must not be closed when db open failed (never opened)")
	}
}
