-- 上游每日/每模型用量快照（上游用量对账设计 §4.1）。
--
-- 每个 token/amount 列都可空：NULL = 上游没有报告该项，0 = 真实的零
-- （设计 §4.2，二者判定语义不同，不得用 0 冒充缺报）。
-- (account_id, usage_date, upstream_model) 唯一，同一维度重复导入按 upsert。
-- source：manual（人工录入）| csv（批量导入）。
CREATE TABLE upstream_usage_snapshots (
    id                     bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    account_id             bigint      NOT NULL REFERENCES upstream_accounts(id) ON DELETE RESTRICT,
    usage_date             date        NOT NULL,
    upstream_model         varchar(128) NOT NULL,
    input_cache_hit_tokens bigint,
    input_cache_miss_tokens bigint,
    cache_write_tokens     bigint,
    output_tokens          bigint,
    requests               bigint,
    amount_minor           bigint,
    currency               char(3)     NOT NULL DEFAULT 'CNY',
    source                 varchar(16) NOT NULL DEFAULT 'manual',
    entered_by             bigint,
    entered_at             timestamptz NOT NULL DEFAULT now(),
    note                   text        NOT NULL DEFAULT '',
    raw                    jsonb,
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT upstream_usage_snapshots_uq UNIQUE (account_id, usage_date, upstream_model),
    CONSTRAINT upstream_usage_snapshots_source_chk CHECK (source IN ('manual', 'csv')),
    CONSTRAINT upstream_usage_snapshots_nonneg_chk CHECK (
        input_cache_hit_tokens >= 0 AND input_cache_miss_tokens >= 0 AND
        cache_write_tokens >= 0 AND output_tokens >= 0 AND requests >= 0)
);
CREATE INDEX upstream_usage_snapshots_account_date_idx
    ON upstream_usage_snapshots (account_id, usage_date DESC);
