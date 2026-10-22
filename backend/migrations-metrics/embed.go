// Package migrationsmetrics 内嵌**独立指标库**的迁移目录。
//
// 与 migrations（主库）/migrations-logs（日志库）严格分离：性能指标不落
// 业务库，也不落日志库（platform-05 §3.5.3）。迁移流由 PERF_METRICS_DSN
// 指向的 PostgreSQL 执行；未配置 DSN 时 RUN_MODE=migrate 跳过本流并记录日志。
package migrationsmetrics

import "embed"

// FS 内嵌全部指标库迁移 SQL（up/down 成对）。
//
//go:embed *.sql
var FS embed.FS
