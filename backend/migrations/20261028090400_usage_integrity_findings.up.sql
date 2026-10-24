-- 用量完整性发现（上游用量对账设计 §4.8）。
--
-- 与 per-(run, account, date, model) 的对账明细不同，这里的检查是全局的
-- （重复 request_id、日志与账本差异等），因此单独建表、以 check_kind/scope
-- 区分维度。P1 完整性 worker 才会写入；P0 先建表，保持迁移流线性。
-- run_id 可空（发现可能独立于某次对账 run 产生），run 被删时置 NULL。
CREATE TABLE usage_integrity_findings (
    id         bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    run_id     bigint      REFERENCES upstream_reconciliation_runs(id) ON DELETE SET NULL,
    check_kind varchar(64) NOT NULL,
    scope      varchar(64) NOT NULL,
    account_id bigint,
    usage_date date,
    model      varchar(128),
    severity   varchar(16) NOT NULL,
    details    jsonb       NOT NULL DEFAULT '{}'::jsonb,
    status     varchar(16) NOT NULL DEFAULT 'open',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT usage_integrity_findings_severity_chk CHECK (severity IN ('info', 'warn', 'alert')),
    CONSTRAINT usage_integrity_findings_status_chk CHECK (status IN ('open', 'acknowledged', 'resolved'))
);
CREATE INDEX usage_integrity_findings_kind_idx ON usage_integrity_findings (check_kind, created_at DESC);
