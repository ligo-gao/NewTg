-- 撤回复合索引，恢复 author 表建表时的单列 bucket_start 索引。
DROP INDEX rankings_author_timeseries_granularity_bucket_idx;

CREATE INDEX rankings_author_timeseries_bucket_idx
    ON rankings_author_timeseries (bucket_start);

DROP INDEX rankings_timeseries_granularity_bucket_idx;
