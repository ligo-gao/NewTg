-- ============================================================================
-- 排行榜趋势粒度扩展（D6a R-D6-04；modules-09 §C1.7 / F-09-32）
--
-- 原 CHECK 只允许 ('day','hour')，而 year 周期的天桶跨度 > 90 天，前端按
-- F-09-32 分支流程必须降采样到周桶；若不先放开约束，year 快照写入会直接
-- 违反 rankings_timeseries_granularity_chk 并让整个快照事务回滚。
-- 只 ALTER CHECK，不动既有行与主键（幂等键仍是 model+granularity+bucket_start）。
-- ============================================================================
ALTER TABLE rankings_timeseries
    DROP CONSTRAINT rankings_timeseries_granularity_chk;

ALTER TABLE rankings_timeseries
    ADD CONSTRAINT rankings_timeseries_granularity_chk
    CHECK (granularity IN ('hour', 'day', 'week'));
