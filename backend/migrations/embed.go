// Package migrations 内嵌主库迁移目录，供运行时迁移（RUN_MODE=migrate / Task 6）
// 经 golang-migrate 的 iofs 驱动执行（R-02：embed 源落在迁移目录自身）。
//
// golang-migrate CLI 只按 *.sql 正则识别版本文件，本文件不影响其扫描。
package migrations

import "embed"

// FS 内嵌全部主库迁移 SQL（up/down 成对，platform-04 §2.1 平铺目录）。
//
//go:embed *.sql
var FS embed.FS
