-- 上游对账明细（上游用量对账设计 §4.1）。
--
-- 一行 = 一个 run 内 (account, usage_date, upstream_model) 的本地侧 +
-- 上游快照侧 + 差异与利润判定。
--   * local_* 全部 bigint NOT NULL DEFAULT 0：本地侧聚合结果，缺省即 0。
--   * snapshot_* / delta_* / 金额列可空：NULL = 上游未报告或无法计算，
--     与真实的 0 区分（§4.2）。
--   * severity 闭集区分「差异等级」与「快照缺失类」判定；
--     status 为运营处置状态（open | acknowledged | resolved）。
-- run_id ON DELETE CASCADE：明细只对所属 run 有意义。
CREATE TABLE upstream_reconciliation_items (
    id                         bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    run_id                     bigint      NOT NULL REFERENCES upstream_reconciliation_runs(id) ON DELETE CASCADE,
    account_id                 bigint      NOT NULL,
    usage_date                 date        NOT NULL,
    upstream_model             varchar(128) NOT NULL,
    local_requests             bigint      NOT NULL DEFAULT 0,
    local_estimated_requests   bigint      NOT NULL DEFAULT 0,
    local_refunded_requests    bigint      NOT NULL DEFAULT 0,
    local_cache_hit_tokens     bigint      NOT NULL DEFAULT 0,
    local_cache_miss_tokens    bigint      NOT NULL DEFAULT 0,
    local_output_tokens        bigint      NOT NULL DEFAULT 0,
    local_wallet_credits       bigint      NOT NULL DEFAULT 0,
    local_subscription_credits bigint      NOT NULL DEFAULT 0,
    local_cost_credits         bigint      NOT NULL DEFAULT 0,
    local_unknown_requests     bigint      NOT NULL DEFAULT 0,
    local_multi_attempt_requests bigint    NOT NULL DEFAULT 0,
    snapshot_cache_hit_tokens  bigint,
    snapshot_cache_miss_tokens bigint,
    snapshot_cache_write_tokens bigint,
    snapshot_output_tokens     bigint,
    snapshot_requests          bigint,
    snapshot_amount_minor      bigint,
    delta_cache_hit_tokens     bigint,
    delta_cache_miss_tokens    bigint,
    delta_output_tokens        bigint,
    delta_token_bp             bigint,
    snapshot_partial           boolean     NOT NULL DEFAULT false,
    revenue_minor              bigint,
    upstream_cost_minor        bigint,
    margin_minor               bigint,
    margin_bp                  bigint,
    severity                   varchar(24) NOT NULL,
    status                     varchar(16) NOT NULL DEFAULT 'open',
    note                       text        NOT NULL DEFAULT '',
    created_at                 timestamptz NOT NULL DEFAULT now(),
    updated_at                 timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT upstream_reconciliation_items_uq
        UNIQUE (run_id, account_id, usage_date, upstream_model),
    CONSTRAINT upstream_reconciliation_items_severity_chk CHECK (severity IN (
        'ok', 'warn', 'alert', 'partial_snapshot',
        'missing_snapshot', 'missing_local', 'local_expired')),
    CONSTRAINT upstream_reconciliation_items_status_chk
        CHECK (status IN ('open', 'acknowledged', 'resolved')),
    CONSTRAINT upstream_reconciliation_items_refunded_chk
        CHECK (local_refunded_requests <= local_requests)
);
CREATE INDEX upstream_reconciliation_items_run_idx
    ON upstream_reconciliation_items (run_id, severity, usage_date DESC);
CREATE INDEX upstream_reconciliation_items_open_idx
    ON upstream_reconciliation_items (status, severity);
