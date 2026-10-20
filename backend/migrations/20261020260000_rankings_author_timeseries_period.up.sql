-- ============================================================================
-- 作者时间序列加入 period 维度（B5a 收口；冻结语义 2026-10-20）
--
-- 背景：week 与 month 周期的粒度都是 day，作者桶幂等键若只有
-- (author, granularity, bucket_start)，两个周期会写同一行并互相覆盖。
-- 更直接的泄漏在读侧：loadRankingsAuthorTimeseries 为了保留 year 的首个
-- 周桶，把下界放宽「恰好一个桶」（day −1d）。month 在 week 起点前一天的
-- 日桶因此会落进 week 的读取范围——公开面把一个月的作者量混进本周序列。
--
-- 把 period 放进自然键后，读写两侧按 run.Period 过滤，跨周期彻底隔离。
--
-- 【为什么先清空】旧行写入时没有记录周期，无法可靠反推（同一
-- author/granularity/bucket_start 可能同时属于 week 与 month）。本表是
-- usage_logs 的派生聚合，下一轮刷新即可重建，清空不损失原始事实；
-- 臆造 period 反而会把错误数据永久固化。
-- ============================================================================
DELETE FROM rankings_author_timeseries;

ALTER TABLE rankings_author_timeseries
    ADD COLUMN period text NOT NULL;

ALTER TABLE rankings_author_timeseries
    DROP CONSTRAINT rankings_author_timeseries_pkey;

ALTER TABLE rankings_author_timeseries
    ADD CONSTRAINT rankings_author_timeseries_pkey
    PRIMARY KEY (period, author, granularity, bucket_start);

ALTER TABLE rankings_author_timeseries
    ADD CONSTRAINT rankings_author_timeseries_period_chk
    CHECK (period IN ('today', 'week', 'month', 'year'));
