package platform

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"newtg/gateway/internal/config"
	"newtg/gateway/internal/metrics"
)

// RunMetricsServer 运行 metrics 侧车 HTTP 服务，阻塞至 ctx done 后优雅退出：
//   - MetricsAddr=="0" → 不启动，立即返回 nil；
//   - /metrics 仅当 MetricsToken 非空时挂载 handler，鉴权不符一律 404
//     （非 401，不暴露端点存在性——本计划冻结口径）；
//   - /healthz 恒 200：worker 没有主 HTTP 端口，运维/编排需要最小探活面
//     （platform-05 §3.4「worker 同样在 METRICS_ADDR 暴露健康端点」）。
func RunMetricsServer(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	if cfg.MetricsAddr == "0" {
		return nil
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
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
