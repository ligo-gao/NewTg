-- 回滚 usage_logs 上游账号读取索引（20261028090700_usage_logs_account_created_idx）。
--
-- 恢复 20261028090600 的模型索引（即使它不服务对账查询，回滚必须回到该迁移
-- 之后的形状），再删除本迁移的账号索引。
-- IF EXISTS/IF NOT EXISTS 让「部分失败后重跑」与「全新库 down」两种路径都安全。
CREATE INDEX IF NOT EXISTS usage_logs_upstream_model_created_idx
    ON usage_logs (upstream_model, created_at DESC);

DROP INDEX IF EXISTS usage_logs_upstream_account_created_idx;
