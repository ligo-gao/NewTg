-- 回滚 W16-1 的渠道指标索引。
--
-- IF EXISTS 让回滚在「迁移部分失败后重跑」与「全新库 down」两种路径都安全：
-- 索引不存在时 drop 是 no-op，不制造 dirty 状态。
DROP INDEX IF EXISTS usage_logs_channel_created_idx;
