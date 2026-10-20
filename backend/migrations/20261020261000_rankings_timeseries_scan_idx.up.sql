-- ============================================================================
-- 历史桶清理/读取的复合索引（B5a 收口；2026-10-20）
--
-- cleanup.go 的删除条件是 `WHERE granularity = $1 AND bucket_start < $2`，
-- 读侧 loadRankingsTimeseries / loadRankingsAuthorTimeseries 也总是先按
-- granularity 等值过滤、再按 bucket_start 范围过滤。只在 bucket_start 上
-- 建单列索引时，planner 无法利用「粒度等值」先缩小范围：hour/day/week 三
-- 组清理会把整段历史扫一遍。
--
-- 复合索引 (granularity, bucket_start) 同时支撑等值前缀与范围后缀，是这两
-- 类查询的最小充分索引。rankings_author_timeseries 在建表迁移里的单列
-- bucket_start 索引由本迁移替换（DROP 后重建，不保留冗余索引）。
-- ============================================================================
CREATE INDEX rankings_timeseries_granularity_bucket_idx
    ON rankings_timeseries (granularity, bucket_start);

DROP INDEX rankings_author_timeseries_bucket_idx;

CREATE INDEX rankings_author_timeseries_granularity_bucket_idx
    ON rankings_author_timeseries (granularity, bucket_start);
