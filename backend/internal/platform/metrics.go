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
)

// metricsStubBody 是 M0 占位响应体；M7 按 platform-05 换 promhttp 后删除。
const metricsStubBody = "newtg_metrics_stub 1\n"

// RunMetricsServer 运行 metrics 侧车 HTTP 服务，阻塞至 ctx done 后优雅退出：
//   - MetricsAddr=="0" → 不启动，立即返回 nil；
//   - /metrics 仅当 MetricsToken 非空时挂载 handler，鉴权不符一律 404
//     （非 401，不暴露端点存在性——本计划冻结口径）。
func RunMetricsServer(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	if cfg.MetricsAddr == "0" {
		return nil
	}
	mux := http.NewServeMux()
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

// newMetricsHandler 返回带鉴权的 stub handler：接受 header
// Authorization: Bearer <token> 或 ?token=<token>，任一匹配即 200，否则 404。
func newMetricsHandler(token string) http.Handler {
	want := []byte("Bearer " + token)
	raw := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authOK := subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) == 1
		queryOK := subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("token")), raw) == 1
		if !authOK && !queryOK {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		// 常量响应体；写失败通常是对端已断开，此时 HTTP server 无法再补救。
		_, _ = fmt.Fprint(w, metricsStubBody)
	})
}
