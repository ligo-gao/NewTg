package platform

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"newtg/gateway/internal/config"
)

// freeLocalAddr 返回一个当前空闲的 127.0.0.1 端口地址（测试专用）。
func freeLocalAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func testMetricsLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func waitForHTTP(t *testing.T, url string, headers map[string]string) *http.Response {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			return resp
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not become ready: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestMetricsServerWorkerLifecycle：worker 角色的 metrics 监听可访问、
// /healthz 恒 200，ctx 取消后监听关闭（不泄漏）。
func TestMetricsServerWorkerLifecycle(t *testing.T) {
	addr := freeLocalAddr(t)
	cfg := &config.Config{MetricsAddr: addr, MetricsToken: "worker-token"}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunMetricsServer(ctx, cfg, testMetricsLogger()) }()

	resp := waitForHTTP(t, "http://"+addr+"/metrics", map[string]string{
		"Authorization": "Bearer worker-token",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/metrics status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "newtg_worker_usage_logs_stream_lag") ||
		!strings.Contains(string(body), "newtg_build_info") {
		t.Fatalf("/metrics body missing worker/build families:\n%s", body)
	}

	health := waitForHTTP(t, "http://"+addr+"/healthz", nil)
	if health.StatusCode != http.StatusOK {
		t.Fatalf("/healthz status = %d", health.StatusCode)
	}
	_ = health.Body.Close()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunMetricsServer returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunMetricsServer did not stop after ctx cancel")
	}
	// 监听必须已释放：再监听同一地址应成功。
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("listener not released after shutdown: %v", err)
	}
	_ = ln.Close()
}

// TestMetricsServerTokenSemantics：无 token 时 /metrics 404，/healthz 仍可用。
func TestMetricsServerTokenSemantics(t *testing.T) {
	addr := freeLocalAddr(t)
	cfg := &config.Config{MetricsAddr: addr}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- RunMetricsServer(ctx, cfg, testMetricsLogger()) }()

	resp := waitForHTTP(t, "http://"+addr+"/metrics", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("/metrics without token = %d, want 404", resp.StatusCode)
	}
	_ = resp.Body.Close()

	health := waitForHTTP(t, "http://"+addr+"/healthz", nil)
	if health.StatusCode != http.StatusOK {
		t.Fatalf("/healthz = %d", health.StatusCode)
	}
	_ = health.Body.Close()
}

// TestMetricsServerWithReadiness：I-4，worker 的 /readyz 与 /healthz 同端口。
func TestMetricsServerWithReadiness(t *testing.T) {
	addr := freeLocalAddr(t)
	cfg := &config.Config{MetricsAddr: addr}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	ready := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"status":"degraded"}`))
	})
	go func() {
		done <- RunMetricsServerWithReadiness(ctx, cfg, testMetricsLogger(), ready)
	}()

	resp := waitForHTTP(t, "http://"+addr+"/readyz", nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("/readyz status = %d, want 503 from readiness handler", resp.StatusCode)
	}
	_ = resp.Body.Close()
	health := waitForHTTP(t, "http://"+addr+"/healthz", nil)
	if health.StatusCode != http.StatusOK {
		t.Fatalf("/healthz = %d", health.StatusCode)
	}
	_ = health.Body.Close()
}

// TestMetricsServerDisabled：METRICS_ADDR=0 或空值不启动监听且立即返回。
// 空值防御（M-6）：零值 Config 直接 ListenAndServe 会尝试绑 :80。
func TestMetricsServerDisabled(t *testing.T) {
	for _, addr := range []string{"0", ""} {
		cfg := &config.Config{MetricsAddr: addr}
		done := make(chan error, 1)
		go func() { done <- RunMetricsServer(context.Background(), cfg, testMetricsLogger()) }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("RunMetricsServer(addr=%q) = %v", addr, err)
			}
		case <-time.After(time.Second):
			t.Fatalf("RunMetricsServer(addr=%q) blocked", addr)
		}
	}
}
