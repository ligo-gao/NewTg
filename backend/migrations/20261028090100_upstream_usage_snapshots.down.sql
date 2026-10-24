-- 回滚上游用量快照（20261028090100_upstream_usage_snapshots）。
--
-- 快照是可重新导入的上游事实副本；回滚直接丢弃，索引随表删除。
DROP TABLE IF EXISTS upstream_usage_snapshots;
