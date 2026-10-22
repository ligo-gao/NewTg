-- ============================================================================
-- G-M6C-13：模型性能采集管道落库（platform-05 §3.5）
--
-- 存储位置：**日志库**，不是主库业务表。性能指标是观测事实，不是计费真源；
-- 与 usage_logs / audit_logs 同属观测面，避免污染主库 schema（platform-01
-- §30 裁决「不在主库建 model_perf_*」）。单 PostgreSQL 部署下没有独立的
-- VictoriaMetrics/ClickHouse，本表是当前可用的指标库落点；M7 选型后可由
-- store.ModelPerfRepo 平滑替换。
--
-- 语义：
--   · 每行是一个 (bucket_start, model, node, epoch) 的**绝对快照**，不是增量；
--   · flush_seq 单调递增，重复写入旧序号不覆盖新值（条件 upsert）；
--   · 多副本/重启后的旧 epoch 行保留，查询按 (bucket_start, model) 求和；
--   · 直方图每桶为非累计计数（非 Prometheus 累计桶），固定 9 个有限桶，
--     +Inf 由 count - Σbuckets 推导。
-- ============================================================================

CREATE TABLE model_perf_buckets (
    bucket_start       timestamptz     NOT NULL,
    model              varchar(128)    NOT NULL,
    node               varchar(128)    NOT NULL,
    epoch              uuid            NOT NULL,
    flush_seq          bigint          NOT NULL,
    success            bigint          NOT NULL DEFAULT 0,
    error              bigint          NOT NULL DEFAULT 0,
    refused            bigint          NOT NULL DEFAULT 0,
    latency_count      bigint          NOT NULL DEFAULT 0,
    latency_sum_ms     double precision NOT NULL DEFAULT 0,
    lat_b0             bigint          NOT NULL DEFAULT 0,
    lat_b1             bigint          NOT NULL DEFAULT 0,
    lat_b2             bigint          NOT NULL DEFAULT 0,
    lat_b3             bigint          NOT NULL DEFAULT 0,
    lat_b4             bigint          NOT NULL DEFAULT 0,
    lat_b5             bigint          NOT NULL DEFAULT 0,
    lat_b6             bigint          NOT NULL DEFAULT 0,
    lat_b7             bigint          NOT NULL DEFAULT 0,
    lat_b8             bigint          NOT NULL DEFAULT 0,
    ttft_count         bigint          NOT NULL DEFAULT 0,
    ttft_b0            bigint          NOT NULL DEFAULT 0,
    ttft_b1            bigint          NOT NULL DEFAULT 0,
    ttft_b2            bigint          NOT NULL DEFAULT 0,
    ttft_b3            bigint          NOT NULL DEFAULT 0,
    ttft_b4            bigint          NOT NULL DEFAULT 0,
    ttft_b5            bigint          NOT NULL DEFAULT 0,
    ttft_b6            bigint          NOT NULL DEFAULT 0,
    ttft_b7            bigint          NOT NULL DEFAULT 0,
    ttft_b8            bigint          NOT NULL DEFAULT 0,
    completion_tokens  bigint          NOT NULL DEFAULT 0,
    generation_seconds double precision NOT NULL DEFAULT 0,
    updated_at         timestamptz     NOT NULL DEFAULT now(),
    CONSTRAINT model_perf_buckets_pkey PRIMARY KEY (bucket_start, model, node, epoch),
    CONSTRAINT model_perf_buckets_counts_chk
        CHECK (success >= 0 AND error >= 0 AND refused >= 0
               AND latency_count >= 0 AND ttft_count >= 0
               AND completion_tokens >= 0),
    CONSTRAINT model_perf_buckets_seq_chk CHECK (flush_seq > 0)
);

-- 看板查询固定按窗口 + 可选模型过滤，再按 (bucket_start, model) 分组。
-- 父表索引对查询足够；本表不分片。
CREATE INDEX model_perf_buckets_window_idx
    ON model_perf_buckets (bucket_start DESC, model);

-- newtg_app 需要 UPDATE 才能执行 flush_seq 条件 upsert（日志库其余表默认
-- 只有 SELECT/INSERT）。仅授本表，usage_logs 的列级例外不受影响。
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'newtg_app') THEN
        EXECUTE 'GRANT SELECT, INSERT, UPDATE ON model_perf_buckets TO newtg_app';
    END IF;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'newtg_backup') THEN
        EXECUTE 'GRANT SELECT ON model_perf_buckets TO newtg_backup';
    END IF;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'newtg_retention') THEN
        EXECUTE 'GRANT SELECT ON model_perf_buckets TO newtg_retention';
    END IF;
END
$$;
