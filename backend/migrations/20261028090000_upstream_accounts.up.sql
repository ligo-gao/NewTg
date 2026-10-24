-- 上游账号目录（上游用量对账设计 §4.1）。
--
-- 一个上游账号 = 一份可独立对账的账期主体（供应商 + 结算币种 + 时区）。
-- status：1 = active，2 = disabled。
-- 渠道通过 channels.upstream_account_id 归属账号；ON DELETE RESTRICT 保证
-- 被渠道引用的账号不能被删除（历史对账必须以当时的归属为准）。
CREATE TABLE upstream_accounts (
    id          bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name        varchar(64) NOT NULL,
    provider    varchar(64) NOT NULL,
    currency    char(3)     NOT NULL DEFAULT 'CNY',
    timezone    varchar(64) NOT NULL DEFAULT 'Asia/Shanghai',
    console_url varchar(512),
    status      smallint    NOT NULL DEFAULT 1,
    note        text        NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT upstream_accounts_name_uq UNIQUE (name),
    CONSTRAINT upstream_accounts_status_chk CHECK (status IN (1, 2))
);
CREATE INDEX upstream_accounts_status_idx ON upstream_accounts (status, id);

ALTER TABLE channels
    ADD COLUMN upstream_account_id bigint
    REFERENCES upstream_accounts(id) ON DELETE RESTRICT;
CREATE INDEX channels_upstream_account_idx ON channels (upstream_account_id);
