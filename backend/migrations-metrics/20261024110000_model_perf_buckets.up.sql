-- ============================================================================
-- G-M6C-13：独立指标库 schema（platform-05 §3.5）
--
-- 本迁移**只**在 PERF_METRICS_DSN 指向的独立 PostgreSQL 指标库执行；不得在
-- 主库或日志库执行（性能指标不落业务库，platform-01 §30）。
--
-- 本 PostgreSQL 指标库是 M7 指标库产品（VictoriaMetrics / ClickHouse /
-- Mimir）确定前的**过渡实现**；语义按设计冻结为「绝对快照 + 条件 upsert +
-- 服务端保留」。产品替换时只需替换 store.ModelPerfRepo，perfmetrics.Store
-- 接缝与查询/采集契约不变。
--
-- 两层表：
--   · model_perf_buckets        1 分钟绝对快照，保留 7 天；
--   · model_perf_hourly_buckets 由分钟桶物化的小时桶，保留
--     PERF_METRICS_RETENTION_DAYS（30–400，默认 400）。
--
-- 幂等：DDL 全部 IF NOT EXISTS，允许在测试/人工重放时重复执行。
-- ============================================================================

CREATE TABLE IF NOT EXISTS model_perf_buckets (
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

CREATE INDEX IF NOT EXISTS model_perf_buckets_window_idx
    ON model_perf_buckets (bucket_start DESC, model);

CREATE TABLE IF NOT EXISTS model_perf_hourly_buckets (
    bucket_start       timestamptz     NOT NULL,
    model              varchar(128)    NOT NULL,
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
    observed_seconds   double precision NOT NULL DEFAULT 0,
    up_seconds         double precision NOT NULL DEFAULT 0,
    updated_at         timestamptz     NOT NULL DEFAULT now(),
    CONSTRAINT model_perf_hourly_buckets_pkey PRIMARY KEY (bucket_start, model)
);

CREATE INDEX IF NOT EXISTS model_perf_hourly_buckets_window_idx
    ON model_perf_hourly_buckets (bucket_start DESC, model);

-- 指标库是独立 database：应用角色需要 schema USAGE 才能读写。若当前迁移
-- 执行者没有 GRANT OPTION（例如非属主的迁移角色），跳过并依赖部署侧
-- postgres-init 预先授权——不制造无法执行的迁移错误。
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'newtg_app')
       AND has_schema_privilege(current_user, 'public', 'USAGE WITH GRANT OPTION') THEN
        EXECUTE 'GRANT USAGE ON SCHEMA public TO newtg_app';
    END IF;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'newtg_app') THEN
        -- DELETE 供指标库侧保留清理（RunRetention）使用；这是独立指标库，
        -- 不影响主库/日志库的 append-only 约束。
        EXECUTE 'GRANT SELECT, INSERT, UPDATE, DELETE ON model_perf_buckets TO newtg_app';
        EXECUTE 'GRANT SELECT, INSERT, UPDATE, DELETE ON model_perf_hourly_buckets TO newtg_app';
    END IF;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'newtg_backup') THEN
        EXECUTE 'GRANT SELECT ON model_perf_buckets TO newtg_backup';
        EXECUTE 'GRANT SELECT ON model_perf_hourly_buckets TO newtg_backup';
    END IF;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'newtg_retention') THEN
        EXECUTE 'GRANT SELECT ON model_perf_buckets TO newtg_retention';
        EXECUTE 'GRANT SELECT ON model_perf_hourly_buckets TO newtg_retention';
    END IF;
END
$$;
