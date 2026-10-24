-- 回滚上游账号目录（20261028090000_upstream_accounts）。
--
-- 顺序与 up 相反：先删 channels 上的索引与外键列，再删账号表本身。
-- 若仍有渠道引用账号，DROP COLUMN 会随列一起删除外键，无数据保留需求
-- （归属关系可在回滚后重新录入）。
DROP INDEX IF EXISTS channels_upstream_account_idx;
ALTER TABLE channels DROP COLUMN IF EXISTS upstream_account_id;
DROP INDEX IF EXISTS upstream_accounts_status_idx;
DROP TABLE IF EXISTS upstream_accounts;
