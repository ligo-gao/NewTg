-- ============================================================================
-- 上游用量对账：usage_logs 上游模型读取索引（上游用量对账设计 §5）
--
-- 对账按 (upstream_model, created_at 窗口) 聚合本地侧用量：
--   WHERE upstream_model = $1 AND created_at >= $2 AND created_at < $3
-- 列顺序等值在前、范围/排序在后，窗口扫描收敛到单模型。
--
-- 建在父表上：PG 11+ 自动下推到既有分区与后续 PARTITION OF 创建的新分区。
-- 不使用 CONCURRENTLY：PG 16 拒绝在分区表上并发建索引，且仓内既有
-- usage_logs 索引（如 20261022100000_usage_logs_channel_idx）同为普通建法。
-- ============================================================================

CREATE INDEX IF NOT EXISTS usage_logs_upstream_model_created_idx
    ON usage_logs (upstream_model, created_at DESC);
