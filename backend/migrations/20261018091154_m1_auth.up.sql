-- ============================================================================
-- M1 账户体系认证迁移（Task 1）
-- 范围：
--   1) users / api_tokens 补列（R-02 / R-01）；
--   2) 认证族 6 张新表（R-07：列权威 = modules-01 §3.5/§3.6/§3.13/§3.19；
--      login_sessions 的列权威 = platform-01 §22.10）。
-- DDL 纪律沿用 M0 init：identity PK（platform-04 §3.1）、保留字双引号（§3.7 V8）、
-- 金额/额度一律 bigint（§3.8）。
--
-- 新表一律逻辑引用 users.id，不设 FK：platform-04 §3.4 V5 的「全库真实外键」
-- 清单冻结为 6 条（M0 集成测试逐条断言），认证族不在清单内；§22.10 亦明确
-- login_sessions.user_id 为逻辑引用。外键增加会破坏「users 可硬删除」的口径。
-- ============================================================================

-- ==== 1. users 补列（R-02）====
-- 管理员重置密码后下发临时口令，用户首次登录必须改密（F-01-03/F-07-12）。
-- DEFAULT false：存量用户不受影响，只有重置路径显式置 true。
ALTER TABLE users ADD COLUMN must_change_password boolean NOT NULL DEFAULT false;

-- ==== 2. api_tokens 补列（R-01，13 → 15 列）====
-- auto_groups：自动分组候选（modules-02 §3.10）；NULL=未启用。以 text 承载
-- JSON 数组（契约按字符串数组序列化），CHECK 保证非 NULL 时必须是 JSON 数组。
ALTER TABLE api_tokens ADD COLUMN auto_groups text;
ALTER TABLE api_tokens ADD CONSTRAINT api_tokens_auto_groups_chk
    CHECK (auto_groups IS NULL OR jsonb_typeof(auto_groups::jsonb) = 'array');
-- cross_group_retry：主分组失败后按 auto_groups 顺序跨组重试（modules-02 §3.11）。
ALTER TABLE api_tokens ADD COLUMN cross_group_retry boolean NOT NULL DEFAULT false;

-- ==== 3. login_sessions（platform-01 §22.10，4 列）====
-- 会话真源是 Redis（sess:{id}）；本表只异步落盘可审计的非敏感标识。
-- 明确不设 ip / user_agent / token 原文（§22.10）；行保留期随审计口径。
CREATE TABLE login_sessions (
    id              bigint      GENERATED ALWAYS AS IDENTITY,
    session_id_hash text        NOT NULL,
    user_id         bigint      NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT login_sessions_pkey PRIMARY KEY (id),
    CONSTRAINT login_sessions_session_id_hash_uq UNIQUE (session_id_hash)
);
-- 按用户清理/核对会话审计行（踢出、硬删账号时）。
CREATE INDEX login_sessions_user_idx ON login_sessions (user_id);

-- ==== 4. login_encryption_keys（modules-01 §3.14，I-7 双槽）====
-- 前端加密传输的 RSA 私钥；active=true 为当前槽，旧槽降为 false 继续保留
-- ≥15 分钟（> 前端公钥缓存 5 分钟），轮换期旧 kid 仍可解密。
-- private_key_enc 存 AES-256-GCM 密文（security-03 §2.4），不落明文私钥。
CREATE TABLE login_encryption_keys (
    id              bigint      GENERATED ALWAYS AS IDENTITY,
    active          boolean     NOT NULL DEFAULT false,
    public_key_pem  text        NOT NULL,
    private_key_enc bytea       NOT NULL,
    kid             text        NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT login_encryption_keys_pkey PRIMARY KEY (id),
    CONSTRAINT login_encryption_keys_kid_uq UNIQUE (kid)
);
-- 双槽的「当前槽唯一」由 DB 兜底：至多一行 active=true（I-7）。
CREATE UNIQUE INDEX login_encryption_keys_single_active_uq
    ON login_encryption_keys (active) WHERE active;

-- ==== 5. user_oauth_bindings（modules-01 §3.5 + §3.12，7 列）====
-- 绑定键是 (slug, 外部身份键)，不是邮箱（§3.12 I-2）：provider 存提供方 slug
-- （内置 github/google/... 与 §3.13 自定义提供方共用同一命名空间）；
-- provider_user_id 的 ≤128 上限取自 §3.5 字段表。
-- 同一提供方账号只能绑定一个本站账号；同一用户对同一提供方至多一条绑定。
CREATE TABLE user_oauth_bindings (
    id               bigint      GENERATED ALWAYS AS IDENTITY,
    user_id          bigint      NOT NULL,
    provider         text        NOT NULL,
    provider_user_id text        NOT NULL,
    provider_email   text,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT user_oauth_bindings_pkey PRIMARY KEY (id),
    CONSTRAINT user_oauth_bindings_provider_user_uq UNIQUE (provider, provider_user_id),
    CONSTRAINT user_oauth_bindings_user_provider_uq UNIQUE (user_id, provider),
    CONSTRAINT user_oauth_bindings_provider_chk
        CHECK (provider ~ '^[a-z0-9_-]{2,64}$'),
    CONSTRAINT user_oauth_bindings_provider_user_id_chk
        CHECK (char_length(provider_user_id) BETWEEN 1 AND 128),
    CONSTRAINT user_oauth_bindings_provider_email_chk
        CHECK (provider_email IS NULL OR char_length(provider_email) <= 254)
);

-- ==== 6. user_passkeys（modules-01 §3.6，9 列）====
-- credential_id 全局唯一（同一凭据不能注册到两个账号）；sign_count 用于
-- 克隆检测（回退即吊销，§3.6 异常流程），故禁止负值。
CREATE TABLE user_passkeys (
    id            bigint      GENERATED ALWAYS AS IDENTITY,
    user_id       bigint      NOT NULL,
    name          text        NOT NULL,
    credential_id bytea       NOT NULL,
    public_key    bytea       NOT NULL,
    sign_count    bigint      NOT NULL DEFAULT 0,
    transports    jsonb,
    created_at    timestamptz NOT NULL DEFAULT now(),
    last_used_at  timestamptz,
    CONSTRAINT user_passkeys_pkey PRIMARY KEY (id),
    CONSTRAINT user_passkeys_credential_id_uq UNIQUE (credential_id),
    CONSTRAINT user_passkeys_name_chk
        CHECK (char_length(name) BETWEEN 1 AND 64 AND btrim(name) <> ''),
    CONSTRAINT user_passkeys_sign_count_chk CHECK (sign_count >= 0),
    CONSTRAINT user_passkeys_transports_chk
        CHECK (transports IS NULL OR jsonb_typeof(transports) = 'array')
);
CREATE INDEX user_passkeys_user_idx ON user_passkeys (user_id);

-- ==== 7. custom_oauth_providers（modules-01 §3.13，25 列）====
-- 身份提供方注册表：内置 provider 由设置键承载，本表只存管理员自建的
-- OAuth2/OIDC 提供方。slug 与内置保留字冲突由应用层拒绝，DB 以 UNIQUE 兜底
-- （§3.13 I-4 的「落库唯一约束 + 创建时同步校验」）。
-- client_secret 落库即密文（security-03 §2.4 / T-SEC-02）：列存 AES-256-GCM
-- envelope，接口永不回传明文（§3.13 I-6）。
-- 字段默认值对齐参照侧管理面语义（modules-01 §3.13 管理面字段）：
-- user_id_field=sub 等字段映射、auth_style 0=auto / 1=params / 2=header。
CREATE TABLE custom_oauth_providers (
    id                     bigint      GENERATED ALWAYS AS IDENTITY,
    name                   text        NOT NULL,
    slug                   text        NOT NULL,
    icon                   text        NOT NULL DEFAULT '',
    enabled                boolean     NOT NULL DEFAULT false,
    client_id              text        NOT NULL DEFAULT '',
    client_secret          text        NOT NULL DEFAULT '',
    authorization_endpoint text,
    token_endpoint         text,
    user_info_endpoint     text,
    scopes                 text        NOT NULL DEFAULT '',
    well_known             text,
    auth_style             smallint    NOT NULL DEFAULT 0,
    user_id_field          text        NOT NULL DEFAULT 'sub',
    username_field         text        NOT NULL DEFAULT 'preferred_username',
    display_name_field     text        NOT NULL DEFAULT 'name',
    email_field            text        NOT NULL DEFAULT 'email',
    access_policy          jsonb,
    issuer                 text,
    redirect_uri           text,
    auto_register          boolean     NOT NULL DEFAULT false,
    domain_allowlist       jsonb,
    group_role_map         jsonb,
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT custom_oauth_providers_pkey PRIMARY KEY (id),
    CONSTRAINT custom_oauth_providers_slug_uq UNIQUE (slug),
    CONSTRAINT custom_oauth_providers_name_chk
        CHECK (char_length(name) BETWEEN 1 AND 64),
    CONSTRAINT custom_oauth_providers_slug_chk
        CHECK (slug ~ '^[a-z0-9_-]{2,64}$'),
    CONSTRAINT custom_oauth_providers_auth_style_chk
        CHECK (auth_style BETWEEN 0 AND 2),
    CONSTRAINT custom_oauth_providers_access_policy_chk
        CHECK (access_policy IS NULL OR jsonb_typeof(access_policy) = 'object'),
    CONSTRAINT custom_oauth_providers_domain_allowlist_chk
        CHECK (domain_allowlist IS NULL OR jsonb_typeof(domain_allowlist) = 'array'),
    CONSTRAINT custom_oauth_providers_group_role_map_chk
        CHECK (group_role_map IS NULL OR jsonb_typeof(group_role_map) = 'object')
);

-- ==== 8. checkins（modules-01 §3.19 + modules-03 F-03-12，5 列）====
-- 签到记录。platform-01 §22.2 与 modules-03 的登记名均为复数 checkins。
-- (user_id, checkin_date) UNIQUE 是「一天一次」的唯一兜底（I-18/F-03-12），
-- 并发双击由 DB 拒绝恰好一次；入账与签到记录同事务由应用层保证。
-- checkin_date 用 date（站点时区判定「今天」，I-19），不使用时间戳。
-- quota_awarded 允许 0（checkin.min_quota=0 时只记签到、不发流水）。
CREATE TABLE checkins (
    id            bigint      GENERATED ALWAYS AS IDENTITY,
    user_id       bigint      NOT NULL,
    checkin_date  date        NOT NULL,
    quota_awarded bigint      NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT checkins_pkey PRIMARY KEY (id),
    CONSTRAINT checkins_user_date_uq UNIQUE (user_id, checkin_date),
    CONSTRAINT checkins_quota_awarded_chk CHECK (quota_awarded >= 0)
);
