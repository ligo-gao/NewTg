-- 上游对账运行批次（上游用量对账设计 §4.1）。
--
-- 容差参数（token_tolerance_bp / amount_tolerance_minor）与 quota_per_unit、
-- timezone 在开跑时**快照**进来：之后修改设置不会改写历史判定，重算历史
-- 只能靠重跑并生成新的 run。
-- status：running | succeeded | partial | failed；triggered_by：worker | manual。
CREATE TABLE upstream_reconciliation_runs (
    id                     bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    account_id             bigint      NOT NULL REFERENCES upstream_accounts(id) ON DELETE RESTRICT,
    window_start           date        NOT NULL,
    window_end             date        NOT NULL,
    triggered_by           varchar(16) NOT NULL,
    status                 varchar(16) NOT NULL DEFAULT 'running',
    token_tolerance_bp     integer     NOT NULL,
    amount_tolerance_minor bigint      NOT NULL,
    quota_per_unit         bigint      NOT NULL,
    timezone               varchar(64) NOT NULL,
    started_at             timestamptz NOT NULL DEFAULT now(),
    finished_at            timestamptz,
    error                  text        NOT NULL DEFAULT '',
    CONSTRAINT upstream_reconciliation_runs_triggered_chk
        CHECK (triggered_by IN ('worker', 'manual')),
    CONSTRAINT upstream_reconciliation_runs_status_chk
        CHECK (status IN ('running', 'succeeded', 'partial', 'failed')),
    CONSTRAINT upstream_reconciliation_runs_window_chk CHECK (window_end > window_start)
);
CREATE INDEX upstream_reconciliation_runs_account_idx
    ON upstream_reconciliation_runs (account_id, started_at DESC);
