-- ============================================================================
-- 上游用量对账：usage_logs 上游账号读取索引（上游用量对账设计 §5、§7.1）
--
-- 对账聚合查询的现代归属分支是 `upstream_account_id = $3`（列顺序：等值在前、
-- 范围/排序在后，窗口扫描收敛到单账号）；没有 upstream_account_id 的历史行
-- 由 `channel_id = ANY($5)` 兜底，已有 usage_logs_channel_created_idx 服务。
--
-- 20261028090600 建的 (upstream_model, created_at DESC) 无法服务该查询：
-- upstream_model 只出现在 GROUP BY 的输出里，查询从不按它过滤，那个索引只是
-- 最热表上的写入放大，本迁移将其删除（不要在 20261028090600 上原地修改：
-- 该版本已在共享测试库与已部署库应用）。
--
-- 建在父表上：PG 11+ 自动下推到既有分区与后续 PARTITION OF 创建的新分区。
-- 不使用并发建索引：PG 16 拒绝在分区表上并发创建索引，且仓内既有 usage_logs
-- 索引同为普通建法（与 20261028090600 的口径一致）。
-- ============================================================================

CREATE INDEX IF NOT EXISTS usage_logs_upstream_account_created_idx
    ON usage_logs (upstream_account_id, created_at DESC);

DROP INDEX IF EXISTS usage_logs_upstream_model_created_idx;
