package platform

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"newtg/gateway/internal/config"
	"newtg/gateway/internal/metrics"
)

// waitGroupWithin 等待 wg 结束，最多等 d。
//
// 指标监听/探针都以进程 ctx 退出为终止条件；错误路径下 ctx 可能尚未取消，
// 无界 Wait 会把停机卡死。有界等待保证连接池关闭前尽力收敛，超时则由进程
// 退出兜底（这些 goroutine 只读，不会写业务状态）。
func waitGroupWithin(wg *sync.WaitGroup, d time.Duration) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(d):
	}
}

// RunMetricsServer 运行 metrics 侧车 HTTP 服务（无 /readyz，gateway 用主端口探针）。
func RunMetricsServer(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	return RunMetricsServerWithReadiness(ctx, cfg, log, nil)
}

// RunMetricsServerWithReadiness 运行 metrics 侧车 HTTP 服务，阻塞至 ctx done 后
// 优雅退出：
//   - MetricsAddr=="0" 或空 → 不启动，立即返回 nil（空值防御：零值 Config
//     直接 ListenAndServe 会绑 :80，测试/误装配下必须视为关闭）；
//   - /metrics 仅当 MetricsToken 非空时挂载 handler，鉴权不符一律 404
//     （非 401，不暴露端点存在性——本计划冻结口径）；
//   - /healthz 恒 200；ready 非 nil 时挂 /readyz（worker 角色传入完整五项
//     判定，compose healthcheck 据此判活）。
func RunMetricsServerWithReadiness(ctx context.Context, cfg *config.Config,
	log *slog.Logger, ready http.Handler) error {
	if cfg.MetricsAddr == "" || cfg.MetricsAddr == "0" {
		return nil
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	if ready != nil {
		mux.Handle("/readyz", ready)
	}
	if cfg.MetricsToken != "" {
		mux.Handle("/metrics", newMetricsHandler(cfg.MetricsToken))
	}
	srv := &http.Server{
		Addr:              cfg.MetricsAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	serveErr := make(chan error, 1)
	go func() {
		err := srv.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErr <- err
	}()
	log.Info("metrics server listening", "addr", cfg.MetricsAddr, "auth", cfg.MetricsToken != "")

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("platform: metrics server shutdown: %w", err)
		}
		return nil
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("platform: metrics server: %w", err)
		}
		return nil
	}
}

// newMetricsHandler 返回带鉴权的 /metrics handler：接受 header
// Authorization: Bearer <token> 或 ?token=<token>，任一匹配即 200，否则 404。
//
// 鉴权层原样保留 M0 冻结语义；响应体换成真实 Prometheus 文本（W10-METRICS）。
func newMetricsHandler(token string) http.Handler {
	want := []byte("Bearer " + token)
	raw := []byte(token)
	inner := metrics.Handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authOK := subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) == 1
		queryOK := subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("token")), raw) == 1
		if !authOK && !queryOK {
			http.NotFound(w, r)
			return
		}
		inner.ServeHTTP(w, r)
	})
}
