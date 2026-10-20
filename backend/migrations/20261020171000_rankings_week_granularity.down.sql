-- 回滚前先清掉 week 桶：旧 CHECK 不容忍该值，留着会让 ADD CONSTRAINT 失败。
-- week 桶只由 D6a 的 year 快照写入，属派生数据（可从 usage_logs 重建），
-- 删除不损失原始事实。
DELETE FROM rankings_timeseries WHERE granularity = 'week';

ALTER TABLE rankings_timeseries
    DROP CONSTRAINT rankings_timeseries_granularity_chk;

ALTER TABLE rankings_timeseries
    ADD CONSTRAINT rankings_timeseries_granularity_chk
    CHECK (granularity IN ('day', 'hour'));
