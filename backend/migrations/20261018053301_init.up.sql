-- ============================================================================
-- 主库 init 迁移（M0，Task 7）
-- 范围：platform-01 §0.1 正册 18 表减去 usage_logs/audit_logs（日志库归 Task 8）
--       = 主库 16 表；列定义逐列转录自 platform-01 各 §（唯一权威）。
-- DDL 纪律：platform-04 §3 —— identity PK（§3.1）；双引号保留字（§3.7 V8）；
--   FK 集中末节按 V5；部分唯一索引名钉死（§3.3.3）；GIN 仅 channels 两处（§3.3.3/§3.3.4）；
--   append-only 豁免时间戳（§3.9）；金额一律 bigint（§3.8）。
-- 建表顺序同 §0.1 表序；FK 单列于末节（platform-04 §3.5 示例形态）。
-- ============================================================================

-- ==== 1. users（platform-01 §1，23 列）====
CREATE TABLE users (
    id                       bigint      GENERATED ALWAYS AS IDENTITY,
    username                 text        NOT NULL,
    display_name             text,
    email                    text,
    email_verified_at        timestamptz,
    password_hash            text,
    role                     text        NOT NULL DEFAULT 'user',
    status                   text        NOT NULL DEFAULT 'enabled',
    balance                  bigint      NOT NULL DEFAULT 0,
    "group"                  text        NOT NULL DEFAULT 'default',
    admin_permissions        jsonb,
    totp_secret              text,
    totp_enabled             boolean     NOT NULL DEFAULT false,
    backup_codes             jsonb,
    language                 text        NOT NULL DEFAULT 'en',
    avatar_url               text,
    aff                      text,
    remark                   text,
    notification_preferences jsonb,
    sidebar_modules          jsonb,
    record_ip_log            boolean     NOT NULL DEFAULT true,
    created_at               timestamptz NOT NULL DEFAULT now(),
    updated_at               timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT users_pkey PRIMARY KEY (id),
    CONSTRAINT users_username_uq UNIQUE (username),
    CONSTRAINT users_username_chk CHECK (username ~ '^[a-zA-Z0-9_-]{3,32}$'),
    CONSTRAINT users_display_name_chk CHECK (char_length(display_name) <= 32),
    CONSTRAINT users_email_chk CHECK (char_length(email) <= 254),
    CONSTRAINT users_role_chk CHECK (role IN ('user', 'admin')),
    CONSTRAINT users_status_chk CHECK (status IN ('enabled', 'disabled')),
    CONSTRAINT users_balance_chk CHECK (balance >= 0),
    CONSTRAINT users_group_chk CHECK (char_length("group") > 0),
    CONSTRAINT users_language_chk CHECK (language IN ('en', 'zh-CN', 'zh-TW', 'fr', 'ru', 'ja', 'vi')),
    CONSTRAINT users_aff_chk CHECK (char_length(aff) <= 64),
    CONSTRAINT users_remark_chk CHECK (char_length(remark) <= 255),
    CONSTRAINT users_sidebar_modules_chk CHECK (jsonb_typeof(sidebar_modules) = 'object')
);
-- 部分唯一索引（§3.3.3；名称钉死 users_email_uq）
CREATE UNIQUE INDEX users_email_uq ON users (email) WHERE email IS NOT NULL;
CREATE INDEX users_status_idx ON users (status);
CREATE INDEX users_role_idx ON users (role);
CREATE INDEX users_group_idx ON users ("group");
CREATE INDEX users_created_idx ON users (created_at DESC);

-- ==== 2. api_tokens（§2，13 列；R2 id bigint，R14 无软删除）====
CREATE TABLE api_tokens (
    id           bigint      GENERATED ALWAYS AS IDENTITY,
    user_id      bigint      NOT NULL,
    name         text        NOT NULL,
    key_hash     text        NOT NULL,
    status       text        NOT NULL DEFAULT 'enabled',
    expires_at   timestamptz,
    model_limits jsonb,
    "group"      text        NOT NULL,
    remain_quota bigint      DEFAULT 100000,
    allow_ips    jsonb,
    last_used_at timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT api_tokens_pkey PRIMARY KEY (id),
    CONSTRAINT api_tokens_key_hash_uq UNIQUE (key_hash),
    CONSTRAINT api_tokens_name_chk CHECK (char_length(name) BETWEEN 1 AND 64 AND btrim(name) <> ''),
    CONSTRAINT api_tokens_status_chk CHECK (status IN ('enabled', 'disabled')),
    CONSTRAINT api_tokens_expires_at_chk CHECK (expires_at IS NULL OR expires_at > created_at),
    CONSTRAINT api_tokens_remain_quota_chk CHECK (remain_quota >= 0)
);
CREATE INDEX api_tokens_user_idx ON api_tokens (user_id);
CREATE INDEX api_tokens_user_created_idx ON api_tokens (user_id, created_at DESC);

-- ==== 3. user_groups（§3，9 列；R4 代理 id PK + name 唯一）====
CREATE TABLE user_groups (
    id              bigint      GENERATED ALWAYS AS IDENTITY,
    name            text        NOT NULL,
    description     text,
    ratio           integer     NOT NULL DEFAULT 100,
    user_selectable boolean     NOT NULL DEFAULT true,
    is_system       boolean     NOT NULL DEFAULT false,
    status          smallint    NOT NULL DEFAULT 1,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT user_groups_pkey PRIMARY KEY (id),
    CONSTRAINT user_groups_name_uq UNIQUE (name),
    CONSTRAINT user_groups_name_chk CHECK (name ~ '^[a-z0-9_-]+$' AND char_length(name) BETWEEN 2 AND 32),
    CONSTRAINT user_groups_description_chk CHECK (char_length(description) <= 128),
    CONSTRAINT user_groups_ratio_chk CHECK (ratio BETWEEN 50 AND 1000),
    CONSTRAINT user_groups_status_chk CHECK (status IN (1, 2))
);
CREATE INDEX user_groups_status_idx ON user_groups (status);

-- ==== 4. system_settings（§4，5 列；自然主键 "key"，R12 updated_by bigint）====
CREATE TABLE system_settings (
    "key"       text        NOT NULL,
    value       jsonb       NOT NULL,
    updated_by  bigint,
    updated_at  timestamptz NOT NULL DEFAULT now(),
    created_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT system_settings_pkey PRIMARY KEY ("key")
);

-- ==== 5. channels（§6，22 列；含 G-API-043 tag；type 为 Tier-2 无 DB CHECK，V6）====
CREATE TABLE channels (
    id                   bigint      GENERATED ALWAYS AS IDENTITY,
    name                 varchar(64) NOT NULL,
    type                 text        NOT NULL,
    base_url             varchar(512),
    credentials          bytea       NOT NULL,
    supported_models     jsonb       NOT NULL,
    group_tags           jsonb,
    model_mapping        jsonb       NOT NULL DEFAULT '{}'::jsonb,
    weight               smallint    NOT NULL DEFAULT 1,
    priority             smallint    NOT NULL DEFAULT 0,
    rate_limit           integer,
    status               smallint    NOT NULL DEFAULT 1,
    auto_ban             boolean     NOT NULL DEFAULT true,
    test_model           varchar(128),
    remark               varchar(255),
    tag                  varchar(32),
    fail_count           integer     NOT NULL DEFAULT 0,
    auto_disabled_reason varchar(255),
    last_test_at         timestamptz,
    last_test_result     boolean,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT channels_pkey PRIMARY KEY (id),
    CONSTRAINT channels_name_uq UNIQUE (name),
    CONSTRAINT channels_supported_models_chk
        CHECK (jsonb_typeof(supported_models) = 'array' AND jsonb_array_length(supported_models) >= 1),
    CONSTRAINT channels_model_mapping_chk CHECK (jsonb_typeof(model_mapping) = 'object'),
    CONSTRAINT channels_weight_chk CHECK (weight BETWEEN 0 AND 1000),
    CONSTRAINT channels_priority_chk CHECK (priority BETWEEN -1000 AND 1000),
    CONSTRAINT channels_rate_limit_chk CHECK (rate_limit BETWEEN 1 AND 100000),
    CONSTRAINT channels_status_chk CHECK (status IN (1, 2, 3)),
    CONSTRAINT channels_fail_count_chk CHECK (fail_count >= 0)
);
CREATE INDEX channels_status_priority_idx ON channels (status, priority DESC);
-- GIN 两处（§3.3.3，路由热路径；usage_logs.other 依 V7 不建）
CREATE INDEX channels_supported_models_gin ON channels USING gin (supported_models jsonb_path_ops);
CREATE INDEX channels_group_tags_gin ON channels USING gin (group_tags jsonb_path_ops);
CREATE INDEX channels_tag_idx ON channels (tag);

-- ==== 6. model_pricing（§7，13 列；R9 判别列+子集列 NULL 语义，R10 代理 id PK）====
CREATE TABLE model_pricing (
    id               bigint      GENERATED ALWAYS AS IDENTITY,
    model            varchar(128) NOT NULL,
    mode             smallint    NOT NULL DEFAULT 0,
    billing_expr     text,
    prompt_price     bigint,
    completion_price bigint,
    cache_read_price bigint,
    cache_write_price bigint,
    author           varchar(64) NOT NULL DEFAULT 'Unknown',
    vendor           varchar(64),
    status           smallint    NOT NULL DEFAULT 1,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT model_pricing_pkey PRIMARY KEY (id),
    CONSTRAINT model_pricing_model_uq UNIQUE (model),
    CONSTRAINT model_pricing_mode_chk CHECK (mode IN (0, 1)),
    CONSTRAINT model_pricing_prompt_price_chk CHECK (prompt_price >= 0),
    CONSTRAINT model_pricing_completion_price_chk CHECK (completion_price >= 0),
    CONSTRAINT model_pricing_cache_read_price_chk CHECK (cache_read_price >= 0),
    CONSTRAINT model_pricing_cache_write_price_chk CHECK (cache_write_price >= 0),
    CONSTRAINT model_pricing_status_chk CHECK (status IN (1, 2))
);
CREATE INDEX model_pricing_status_idx ON model_pricing (status);
CREATE INDEX model_pricing_author_idx ON model_pricing (author);
CREATE INDEX model_pricing_prompt_price_idx ON model_pricing (prompt_price);
CREATE INDEX model_pricing_completion_price_idx ON model_pricing (completion_price);

-- ==== 7. group_pricing（§8，10 列；UNIQUE("group", model)，整数绝对价）====
CREATE TABLE group_pricing (
    id                bigint      GENERATED ALWAYS AS IDENTITY,
    "group"           varchar(32) NOT NULL,
    model             varchar(128) NOT NULL,
    prompt_price      bigint      NOT NULL DEFAULT 0,
    completion_price  bigint      NOT NULL DEFAULT 0,
    cache_read_price  bigint      NOT NULL DEFAULT 0,
    cache_write_price bigint      NOT NULL DEFAULT 0,
    status            smallint    NOT NULL DEFAULT 1,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT group_pricing_pkey PRIMARY KEY (id),
    CONSTRAINT group_pricing_group_model_uq UNIQUE ("group", model),
    CONSTRAINT group_pricing_prompt_price_chk CHECK (prompt_price >= 0),
    CONSTRAINT group_pricing_completion_price_chk CHECK (completion_price >= 0),
    CONSTRAINT group_pricing_cache_read_price_chk CHECK (cache_read_price >= 0),
    CONSTRAINT group_pricing_cache_write_price_chk CHECK (cache_write_price >= 0),
    CONSTRAINT group_pricing_status_chk CHECK (status IN (1, 2))
);
CREATE INDEX group_pricing_group_idx ON group_pricing ("group");
CREATE INDEX group_pricing_model_idx ON group_pricing (model);

-- ==== 8. redemption_codes（§9，11 列；R11 不物化 expired_at）====
CREATE TABLE redemption_codes (
    id         bigint      GENERATED ALWAYS AS IDENTITY,
    code_hash  text        NOT NULL,
    credits    bigint      NOT NULL,
    status     text        NOT NULL DEFAULT 'unused',
    batch_name varchar(32),
    created_by bigint      NOT NULL,
    used_by    bigint,
    used_at    timestamptz,
    expires_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT redemption_codes_pkey PRIMARY KEY (id),
    CONSTRAINT redemption_codes_code_hash_uq UNIQUE (code_hash),
    CONSTRAINT redemption_codes_credits_chk CHECK (credits > 0),
    CONSTRAINT redemption_codes_status_chk CHECK (status IN ('unused', 'disabled', 'used'))
);
CREATE INDEX redemption_codes_status_expires_idx ON redemption_codes (status, expires_at);
CREATE INDEX redemption_codes_batch_idx ON redemption_codes (batch_name);
CREATE INDEX redemption_codes_created_idx ON redemption_codes (created_at);

-- ==== 9. orders（§10，23 列；含 G-1R P1/P2 新增 11 列；refunded 预留不启用 U-08）====
CREATE TABLE orders (
    id                  bigint      GENERATED ALWAYS AS IDENTITY,
    user_id             bigint      NOT NULL,
    provider            text        NOT NULL,
    amount              bigint      NOT NULL,
    currency            char(3)     NOT NULL,
    status              text        NOT NULL DEFAULT 'pending',
    idempotency_key     text        NOT NULL,
    provider_txn_id     text,
    paid_at             timestamptz,
    raw_callback        jsonb,
    provider_currency   char(3),
    provider_amount     bigint,
    currency_scale      smallint,
    fx_rate_num         integer,
    fx_rate_den         integer,
    credited_credits    bigint      NOT NULL DEFAULT 0,
    failure_code        varchar(64),
    channel_metadata    jsonb,
    manual_completed    boolean     NOT NULL DEFAULT false,
    manual_reason       varchar(200),
    manual_operator_id  bigint,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT orders_pkey PRIMARY KEY (id),
    CONSTRAINT orders_idempotency_key_uq UNIQUE (idempotency_key),
    CONSTRAINT orders_provider_txn_id_uq UNIQUE (provider_txn_id),
    CONSTRAINT orders_provider_chk
        CHECK (provider IN ('stripe', 'alipay', 'creem', 'waffo', 'waffo_pancake', 'epay')),
    CONSTRAINT orders_amount_chk CHECK (amount >= 0),
    CONSTRAINT orders_status_chk
        CHECK (status IN ('pending', 'succeeded', 'failed', 'expired', 'cancelled')),
    CONSTRAINT orders_provider_amount_chk CHECK (provider_amount > 0),
    CONSTRAINT orders_currency_scale_chk CHECK (currency_scale BETWEEN 0 AND 3),
    CONSTRAINT orders_fx_rate_num_chk CHECK (fx_rate_num > 0),
    CONSTRAINT orders_fx_rate_den_chk CHECK (fx_rate_den > 0),
    CONSTRAINT orders_credited_credits_chk CHECK (credited_credits >= 0),
    CONSTRAINT orders_failure_code_chk CHECK (failure_code IN (
        'provider_unavailable', 'provider_rejected', 'signature_invalid', 'amount_mismatch',
        'currency_mismatch', 'order_not_found', 'order_state_conflict', 'manual_required')
        OR failure_code IS NULL),
    CONSTRAINT orders_manual_reason_chk CHECK (
        manual_completed = false
        OR (manual_reason IS NOT NULL AND char_length(manual_reason) BETWEEN 10 AND 200)),
    CONSTRAINT orders_manual_operator_chk CHECK (
        manual_completed = false OR manual_operator_id IS NOT NULL)
);
CREATE INDEX orders_user_created_idx ON orders (user_id, created_at DESC);
CREATE INDEX orders_status_created_idx ON orders (status, created_at);
CREATE INDEX orders_provider_status_idx ON orders (provider, status);

-- ==== 10. wallet_transactions（§11，11 列；有符号金额；部分唯一索引名钉死）====
CREATE TABLE wallet_transactions (
    id             bigint      GENERATED ALWAYS AS IDENTITY,
    user_id        bigint      NOT NULL,
    type           text        NOT NULL,
    amount         bigint      NOT NULL,
    balance_after  bigint      NOT NULL,
    status         text        NOT NULL DEFAULT 'confirmed',
    reference_type text,
    reference_id   varchar(64),
    note           varchar(200),
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT wallet_transactions_pkey PRIMARY KEY (id),
    CONSTRAINT wallet_transactions_type_chk CHECK (type IN (
        'topup', 'redemption', 'consume_pre', 'consume', 'release',
        'reversal', 'admin_adjustment', 'bonus', 'affiliate_transfer', 'checkin_bonus')),
    CONSTRAINT wallet_transactions_amount_chk CHECK (amount <> 0),
    CONSTRAINT wallet_transactions_balance_after_chk CHECK (balance_after >= 0),
    CONSTRAINT wallet_transactions_status_chk
        CHECK (status IN ('hold', 'settled', 'released', 'expired', 'confirmed')),
    CONSTRAINT wallet_transactions_reference_type_chk CHECK (reference_type IN (
        'order', 'redemption', 'usage_log', 'admin_op', 'reversal',
        'signup_grant', 'promo', 'checkin', 'affiliate_transfer') OR reference_type IS NULL)
);
CREATE INDEX wallet_transactions_user_created_idx ON wallet_transactions (user_id, created_at DESC);
CREATE INDEX wallet_transactions_status_created_idx ON wallet_transactions (status, created_at);
CREATE INDEX wallet_transactions_reference_idx ON wallet_transactions (reference_type, reference_id);
-- 部分唯一（§3.3.3 / U-05 全表范围 / G-1R P19；名称钉死）
CREATE UNIQUE INDEX wallet_release_refid_uq ON wallet_transactions (reference_id) WHERE type = 'release';
CREATE UNIQUE INDEX uq_wallet_tx_signup_grant ON wallet_transactions (reference_id) WHERE reference_type = 'signup_grant';

-- ==== 11. rankings_snapshot_runs（§13，8 列；append-only，snapshot_at 承担时间戳 R6）====
CREATE TABLE rankings_snapshot_runs (
    id               bigint      GENERATED ALWAYS AS IDENTITY,
    period           text        NOT NULL,
    window_start     timestamptz NOT NULL,
    window_end       timestamptz NOT NULL,
    snapshot_at      timestamptz NOT NULL DEFAULT now(),
    sample_threshold integer     NOT NULL,
    status           text        NOT NULL,
    model_count      integer     NOT NULL,
    CONSTRAINT rankings_snapshot_runs_pkey PRIMARY KEY (id),
    CONSTRAINT rankings_snapshot_runs_period_chk CHECK (period IN ('today', 'week', 'month', 'year')),
    CONSTRAINT rankings_snapshot_runs_status_chk CHECK (status IN ('ok', 'partial', 'empty'))
);
CREATE INDEX rankings_snapshot_runs_period_idx ON rankings_snapshot_runs (period, snapshot_at DESC);

-- ==== 12. rankings_model_stats（§14，10 列；自然唯一 (run_id, model)，不引入代理 id）====
CREATE TABLE rankings_model_stats (
    run_id       bigint       NOT NULL,
    model        varchar(128) NOT NULL,
    display_name varchar(128),
    author       varchar(64),
    total_tokens bigint       NOT NULL,
    requests     bigint       NOT NULL,
    rank         integer,
    prev_rank    integer,
    delta_tokens bigint,
    delta_pct    numeric(7, 2),
    CONSTRAINT rankings_model_stats_pkey PRIMARY KEY (run_id, model)
);

-- ==== 13. rankings_provider_stats（§15，5 列；自然唯一 (run_id, author)）====
CREATE TABLE rankings_provider_stats (
    run_id       bigint      NOT NULL,
    author       varchar(64) NOT NULL,
    total_tokens bigint      NOT NULL,
    share_pct    numeric(5, 2),
    rank         integer,
    CONSTRAINT rankings_provider_stats_pkey PRIMARY KEY (run_id, author)
);

-- ==== 14. rankings_timeseries（§16，4 列；自然唯一 (model, granularity, bucket_start)）====
CREATE TABLE rankings_timeseries (
    model        varchar(128) NOT NULL,
    granularity  text         NOT NULL,
    bucket_start timestamptz  NOT NULL,
    tokens       bigint,
    CONSTRAINT rankings_timeseries_pkey PRIMARY KEY (model, granularity, bucket_start),
    CONSTRAINT rankings_timeseries_granularity_chk CHECK (granularity IN ('day', 'hour'))
);

-- ==== 15. rankings_entities（§17，3 列；自然唯一 (entity, kind)；色板 8 槽）====
CREATE TABLE rankings_entities (
    entity varchar(128) NOT NULL,
    kind   text         NOT NULL,
    slot   smallint     NOT NULL,
    CONSTRAINT rankings_entities_pkey PRIMARY KEY (entity, kind),
    CONSTRAINT rankings_entities_kind_chk CHECK (kind IN ('model', 'author')),
    CONSTRAINT rankings_entities_slot_chk CHECK (slot BETWEEN 1 AND 8)
);

-- ==== 16. request_registry（§18 / platform-04 V9，9 列；uuid 自然主键，非分区）====
CREATE TABLE request_registry (
    request_id    uuid        NOT NULL,
    user_id       bigint      NOT NULL,
    state         text        NOT NULL DEFAULT 'in_flight',
    heartbeat_at  timestamptz NOT NULL DEFAULT now(),
    pre_deducted  bigint      NOT NULL DEFAULT 0,
    cost          bigint      NOT NULL DEFAULT 0,
    created_at    timestamptz NOT NULL DEFAULT now(),
    settled_at    timestamptz,
    refunded_at   timestamptz,
    CONSTRAINT request_registry_pkey PRIMARY KEY (request_id),
    CONSTRAINT request_registry_state_chk
        CHECK (state IN ('in_flight', 'settled', 'refunded', 'failed'))
);
-- 两索引（§18：在飞兜底扫描 + housekeeping 清理）
CREATE INDEX request_registry_inflight_idx ON request_registry (heartbeat_at) WHERE state = 'in_flight';
CREATE INDEX request_registry_created_idx ON request_registry (created_at);

-- ==== 外键（platform-04 §3.4 V5；财务/日志类一律逻辑引用不设 FK）====
ALTER TABLE api_tokens
    ADD CONSTRAINT api_tokens_user_id_fk FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE;
ALTER TABLE api_tokens
    ADD CONSTRAINT api_tokens_group_fk FOREIGN KEY ("group") REFERENCES user_groups (name) ON DELETE RESTRICT;
ALTER TABLE users
    ADD CONSTRAINT users_group_fk FOREIGN KEY ("group") REFERENCES user_groups (name) ON DELETE RESTRICT;
ALTER TABLE group_pricing
    ADD CONSTRAINT group_pricing_group_fk FOREIGN KEY ("group") REFERENCES user_groups (name) ON DELETE RESTRICT;
ALTER TABLE rankings_model_stats
    ADD CONSTRAINT rankings_model_stats_run_id_fk
    FOREIGN KEY (run_id) REFERENCES rankings_snapshot_runs (id) ON DELETE CASCADE;
ALTER TABLE rankings_provider_stats
    ADD CONSTRAINT rankings_provider_stats_run_id_fk
    FOREIGN KEY (run_id) REFERENCES rankings_snapshot_runs (id) ON DELETE CASCADE;
