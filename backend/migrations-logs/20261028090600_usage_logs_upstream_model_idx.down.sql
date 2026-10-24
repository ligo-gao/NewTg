-- 回滚 usage_logs 上游模型读取索引（20261028090600_usage_logs_upstream_model_idx）。
--
-- IF EXISTS 让「部分失败后重跑」与「全新库 down」两种路径都安全。
DROP INDEX IF EXISTS usage_logs_upstream_model_created_idx;
