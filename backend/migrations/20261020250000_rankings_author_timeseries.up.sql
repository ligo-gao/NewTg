-- ============================================================================
-- 供应商（作者）时间序列（B5a；冻结语义 2026-10-20）
--
-- 背景：rankings_timeseries 只存模型维桶，providers[].series 无数据来源
-- （G-DOM-20 / R-D6-05）。本表按「作者 × 粒度 × 桶起点」累积 token，供公开
-- 面渲染供应商份额随时间的堆叠图。
--
-- 写入口径（worker）：
--   * 只写当轮作者榜 Top 5（与 rankings_provider_stats 的 rank 顺序一致）；
--     Other 不落表（前端用份额余量表达）；
--   * 以 (author, granularity, bucket_start) 为幂等键 ON CONFLICT DO UPDATE，
--     与 rankings_timeseries 同口径（append-only 的 usage_logs 使覆盖天然幂等，
--     累加会双计）。
--
-- 保留清理：hour 30 天 / day 730 天 / week 1825 天，按 bucket_start 分批删除
-- （见 internal/rankings/cleanup.go）。bucket_start 上建单列索引支撑该扫描。
-- ============================================================================
CREATE TABLE rankings_author_timeseries (
    author      text        NOT NULL,
    granularity text        NOT NULL,
    bucket_start timestamptz NOT NULL,
    tokens      bigint      NOT NULL,
    PRIMARY KEY (author, granularity, bucket_start),
    CONSTRAINT rankings_author_timeseries_granularity_chk
        CHECK (granularity IN ('hour', 'day', 'week'))
);

CREATE INDEX rankings_author_timeseries_bucket_idx
    ON rankings_author_timeseries (bucket_start);
