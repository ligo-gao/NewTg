-- ============================================================================
-- W16-1：渠道指标（F-06-11）的日志库读取索引
--
-- 端点 GET /api/channels/{id}/metrics 与渠道列表 24h 概要都按
--   WHERE channel_id = $1 AND created_at >= $2 AND created_at < $3
-- 聚合 usage_logs。没有该索引时，24h/7d 查询会扫过窗口内**所有渠道**的分区行；
-- 有了 (channel_id, created_at) 后，扫描收敛到单渠道的时间窗。
--
-- 建在父表上：PG 11+ 会把父表索引自动下推到既有分区与后续 CREATE TABLE
-- ... PARTITION OF 创建的新分区（无需逐月补索引）。列顺序固定
-- (channel_id, created_at DESC)：等值列在前、范围/排序列在后，两个谓词都能
-- 走索引；DESC 与既有 usage_logs_user_created_idx 的口径一致。
-- ============================================================================

CREATE INDEX IF NOT EXISTS usage_logs_channel_created_idx
    ON usage_logs (channel_id, created_at DESC);
