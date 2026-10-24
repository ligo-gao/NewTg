-- 回滚 usage_logs 对账维度列（20261028090500_usage_logs_reconcile_dimensions）。
--
-- 顺序与 up 相反：先删约束，再按相反顺序删列。分区表的父表 DROP COLUMN
-- 同样会递归到所有分区。列内数据随列丢弃（对账维度可由日志重放/重导重建）。
ALTER TABLE usage_logs DROP CONSTRAINT IF EXISTS usage_logs_usage_semantic_chk;
ALTER TABLE usage_logs DROP CONSTRAINT IF EXISTS usage_logs_usage_source_chk;
ALTER TABLE usage_logs DROP COLUMN IF EXISTS attempts;
ALTER TABLE usage_logs DROP COLUMN IF EXISTS response_id;
ALTER TABLE usage_logs DROP COLUMN IF EXISTS upstream_account_id;
ALTER TABLE usage_logs DROP COLUMN IF EXISTS upstream_model;
ALTER TABLE usage_logs DROP COLUMN IF EXISTS reasoning_tokens;
ALTER TABLE usage_logs DROP COLUMN IF EXISTS usage_semantic;
ALTER TABLE usage_logs DROP COLUMN IF EXISTS usage_source;
