-- 回滚 period 维度，恢复 (author, granularity, bucket_start) 自然键。
--
-- 先按旧自然键去重：跨周期可能出现同一个桶的多行（这正是加入 period 要
-- 解决的问题），旧主键无法容纳。保留 period 字典序最大的一行；本表是
-- 派生数据，去重不丢 usage_logs 原始事实，下一次刷新会按旧口径重算。
ALTER TABLE rankings_author_timeseries
    DROP CONSTRAINT rankings_author_timeseries_pkey;

DELETE FROM rankings_author_timeseries a
 USING rankings_author_timeseries b
 WHERE a.author = b.author
   AND a.granularity = b.granularity
   AND a.bucket_start = b.bucket_start
   AND a.period < b.period;

ALTER TABLE rankings_author_timeseries
    ADD CONSTRAINT rankings_author_timeseries_pkey
    PRIMARY KEY (author, granularity, bucket_start);

ALTER TABLE rankings_author_timeseries
    DROP CONSTRAINT rankings_author_timeseries_period_chk;

ALTER TABLE rankings_author_timeseries
    DROP COLUMN period;
