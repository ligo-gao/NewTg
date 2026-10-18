-- ============================================================================
-- 日志库初始 schema（platform-04 §2.4 / §3.2 / §3.3 / §5）
-- 权威列定义：docs/platform-01-数据库字段汇总.md §5 audit_logs、§12 usage_logs
-- 日志库独立于主库：仅两张表，无跨库 FK。
-- ============================================================================

-- ==== usage_logs（platform-01 §12，25 列，按月声明式分区）====
-- R5：分区表主键必须包含分区键，故为 (id, created_at)。
-- R6：append-only，只有 created_at，无 updated_at。
CREATE TABLE usage_logs (
    id                 bigint      GENERATED ALWAYS AS IDENTITY,
    request_id         uuid        NOT NULL,
    user_id            bigint      NOT NULL,
    token_id           bigint,
    token_name         varchar(64),
    channel_id         bigint,
    upstream           varchar(128),
    model              varchar(128) NOT NULL,
    "group"            varchar(32),
    prompt_tokens      int         DEFAULT 0,
    completion_tokens  int         DEFAULT 0,
    cache_read_tokens  int         DEFAULT 0,
    cache_write_tokens int         DEFAULT 0,
    pre_deducted       bigint,
    cost               bigint      DEFAULT 0,
    refunded           bigint      DEFAULT 0,
    latency_ms         int,
    first_token_ms     int,
    stream             boolean     NOT NULL DEFAULT false,
    status             smallint    NOT NULL,
    error_class        varchar,
    error_message      varchar,
    retry_chain        jsonb,
    other              jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at         timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT usage_logs_pkey PRIMARY KEY (id, created_at),
    -- 小枚举内联 CHECK（platform-04 §6.2）：refused/success/error/refunded = 0..3
    CONSTRAINT usage_logs_status_chk CHECK (status IN (0, 1, 2, 3))
) PARTITION BY RANGE (created_at);

-- 父表索引自动下推到既有分区与后续新分区（PG 11+）。
-- request_id 为普通索引而非 UNIQUE（R5）；other 不建 GIN（V7）。
CREATE INDEX usage_logs_request_id_idx    ON usage_logs (request_id);
CREATE INDEX usage_logs_user_created_idx  ON usage_logs (user_id, created_at DESC);
CREATE INDEX usage_logs_created_idx       ON usage_logs (created_at DESC);
CREATE INDEX usage_logs_model_created_idx ON usage_logs (model, created_at DESC);
CREATE INDEX usage_logs_cursor_idx        ON usage_logs (created_at DESC, id);

-- ==== audit_logs（platform-01 §5，14 列，非分区）====
-- 无 FK、无 request_id 列（platform-05 U-05-7）；detail/before/after 为 jsonb。
CREATE TABLE audit_logs (
    id          bigint      GENERATED ALWAYS AS IDENTITY,
    user_id     bigint      NOT NULL,
    username    text        NOT NULL,
    category    text        NOT NULL,
    ip          text,
    user_agent  text,
    method      text,
    route       text,
    status_code smallint,
    result      text        NOT NULL,
    detail      jsonb,
    before      jsonb,
    after       jsonb,
    created_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT audit_logs_pkey PRIMARY KEY (id),
    CONSTRAINT audit_logs_category_chk CHECK (category IN ('login', 'security', 'operation', 'access_token')),
    CONSTRAINT audit_logs_result_chk CHECK (result IN ('success', 'failed'))
);

CREATE INDEX audit_logs_user_created_idx     ON audit_logs (user_id, created_at DESC);
CREATE INDEX audit_logs_category_created_idx ON audit_logs (category, created_at DESC);
CREATE INDEX audit_logs_result_idx           ON audit_logs (result);

-- ==== 预建分区：当月 + 下月（platform-04 §5.2，裁决 V3）====
-- 命名 usage_logs_YYYYMM；区间左闭右开 [月初, 次月初)，UTC。
-- 月份在执行时用 date_trunc('month', now()) 计算，禁止硬编码日期。
-- 不建 DEFAULT 分区（裁决 V11）：落入未建月份直接失败并由 /readyz 告警。
DO $$
DECLARE
    month_start timestamptz := date_trunc('month', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC';
    next_start  timestamptz := date_trunc('month', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' + interval '1 month';
    after_next  timestamptz := date_trunc('month', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' + interval '2 months';
BEGIN
    EXECUTE format(
        'CREATE TABLE IF NOT EXISTS %I PARTITION OF usage_logs FOR VALUES FROM (%L) TO (%L)',
        'usage_logs_' || to_char(month_start AT TIME ZONE 'UTC', 'YYYYMM'),
        month_start, next_start);
    EXECUTE format(
        'CREATE TABLE IF NOT EXISTS %I PARTITION OF usage_logs FOR VALUES FROM (%L) TO (%L)',
        'usage_logs_' || to_char(next_start AT TIME ZONE 'UTC', 'YYYYMM'),
        next_start, after_next);
END
$$;
