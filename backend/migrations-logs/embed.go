// Package migrationslogs 内嵌日志库迁移目录，供运行时迁移（RUN_MODE=migrate / Task 6）
// 经 golang-migrate 的 iofs 驱动执行（R-02：embed 源落在迁移目录自身）。
//
// 日志库与主库分离（G7-R36）：本目录只含 usage_logs / audit_logs 的迁移。
// golang-migrate CLI 只按 *.sql 正则识别版本文件，本文件不影响其扫描。
package migrationslogs

import "embed"

// FS 内嵌全部日志库迁移 SQL（up/down 成对，platform-04 §2.1 平铺目录）。
//
//go:embed *.sql
var FS embed.FS
