-- 回滚上游对账运行批次（20261028090200_upstream_reconciliation_runs）。
--
-- 依赖顺序：items（20261028090300）的 down 先删除子表，随后本文件的 down
-- 才能删除父表；usage_integrity_findings 通过 ON DELETE SET NULL 引用本表，
-- 其 down 也先于本文件执行。运行历史不可恢复（对账可随时重跑）。
DROP TABLE IF EXISTS upstream_reconciliation_runs;
