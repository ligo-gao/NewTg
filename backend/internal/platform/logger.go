package platform

import (
	"log/slog"
	"os"

	"newtg/gateway/internal/config"
)

// NewLogger 按配置构造 slog.Logger：
//   - APP_ENV=development → 文本 handler + debug 级（本地可读性优先）；
//   - 否则 LOG_FORMAT=json → JSON handler，其余 → 文本 handler；
//   - 级别取自 LOG_LEVEL（默认 info）。
//
// 输出固定为 os.Stdout，结构化字段由调用方按需 With。
func NewLogger(cfg *config.Config) *slog.Logger {
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	switch cfg.LogLevel {
	case "debug":
		opts.Level = slog.LevelDebug
	case "warn":
		opts.Level = slog.LevelWarn
	case "error":
		opts.Level = slog.LevelError
	}
	if cfg.AppEnv == "development" {
		opts.Level = slog.LevelDebug
		return slog.New(slog.NewTextHandler(os.Stdout, opts))
	}
	if cfg.LogFormat == "json" {
		return slog.New(slog.NewJSONHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stdout, opts))
}
