-- ============================================================================
-- M6c Task 4 —— 通知模板与投递记录（modules-08 §3.9.2 / §3.9.5）
--
-- 两张表是一组：
--   notification_templates  模板载体（channel ∈ {email,inapp}）；
--   notification_deliveries 投递记录（channel ∈ {email,webhook}，inapp 不落表）。
-- 两处 channel 枚举**互不为超集**是设计明文（§3.9.2 末），不是笔误：
-- webhook payload 由 owner 定义、不走模板；站内文案随响应下发、不落投递记录。
--
-- 约束口径：
--   - 代理主键用 GENERATED ALWAYS AS IDENTITY（platform-04 §3.1）；
--   - 逻辑引用 users.id，不设 FK（platform-04 §3.4 V5：通知表不在冻结的
--     真实外键清单内；加 FK 会破坏 users 可硬删除的口径）；
--   - locale 值域与 users.language 同集（platform-01 §4 / G7-R14）；
--   - 时间列一律 timestamptz（platform-01 §19.1）。
--
-- 播种不放在 SQL 里：内置文案的唯一真源是 internal/notify 的代码 bundle
-- （§3.9.2「从代码内置 bundle 播种」「启动时缺失则补种，幂等」）。SQL 里再写
-- 一份 7 语言文案会形成第二真源，改文案时必须同时改两处，必然漂移。
-- ============================================================================

-- ==== 1. notification_templates ====
-- UNIQUE(key, channel, locale) 是本表的业务身份：同一通知在不同渠道、不同
-- 语言下各一行；billing.balance_low 因此 email/inapp × 7 语 = 14 行。
-- is_modified=false 表示行内容 == 内置默认（「恢复默认」回写后置 false）。
CREATE TABLE notification_templates (
    id            bigint      GENERATED ALWAYS AS IDENTITY,
    "key"         text        NOT NULL,
    channel       text        NOT NULL,
    locale        text        NOT NULL,
    category      text        NOT NULL,
    subject       text,
    body_markdown text        NOT NULL,
    cta_text      text,
    cta_url       text,
    is_modified   boolean     NOT NULL DEFAULT false,
    enabled       boolean     NOT NULL DEFAULT true,
    updated_by    bigint,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT notification_templates_pkey PRIMARY KEY (id),
    CONSTRAINT notification_templates_key_channel_locale_uq UNIQUE ("key", channel, locale),
    CONSTRAINT notification_templates_channel_chk CHECK (channel IN ('email', 'inapp')),
    CONSTRAINT notification_templates_locale_chk
        CHECK (locale IN ('en', 'zh-CN', 'zh-TW', 'fr', 'ru', 'ja', 'vi')),
    -- key 形态：点分小写（§3.9.2）。注册表封闭性由应用层裁决，DB 只兜底
    -- 形态；category 冗余存储便于列表筛选与审计（§3.9.2）。
    CONSTRAINT notification_templates_key_chk
        CHECK ("key" ~ '^[a-z][a-z0-9_]*(\.[a-z0-9_]+)*$'),
    CONSTRAINT notification_templates_category_chk
        CHECK (category ~ '^[a-z][a-z0-9_]*(\.[a-z0-9_]+)*$'),
    -- 长度上限：subject ≤256，body ≤16KB（与 F-08-04 公告同口径），CTA ≤32。
    -- char_length 计的是字符而非字节，避免中文被字节数误伤。
    CONSTRAINT notification_templates_subject_len_chk
        CHECK (subject IS NULL OR char_length(subject) <= 256),
    CONSTRAINT notification_templates_body_len_chk
        CHECK (char_length(body_markdown) BETWEEN 1 AND 16384),
    CONSTRAINT notification_templates_cta_text_len_chk
        CHECK (cta_text IS NULL OR char_length(cta_text) <= 32),
    -- CTA 成对：有文案必须有 http(s) 链接；无文案时链接也必须为空。
    -- cta_url 允许两种形态：字面 http(s) 链接，或**整个值恰是一个
    -- {{.var}} 变量**——§3.9.4 的内置 CTA 正是 `{{.reset_url}}` 形态，
    -- DB 无法在保存时预知变量值。渲染后的值由应用层再次校验 http(s)，
    -- 否则按钮链接会成为注入面。
    CONSTRAINT notification_templates_cta_pair_chk
        CHECK ((cta_text IS NULL AND cta_url IS NULL)
            OR (cta_text IS NOT NULL AND cta_url IS NOT NULL
                AND (cta_url ~* '^https?://'
                     OR cta_url ~ '^\{\{\s*\.[a-z][a-z0-9_]*\s*\}\}$'))),
    -- inapp 是站内横幅，没有标题：subject 必须为空（§3.9.2）。
    CONSTRAINT notification_templates_inapp_subject_chk
        CHECK (channel <> 'inapp' OR subject IS NULL)
);

-- 主查询已被唯一约束覆盖；列表页按 分类×渠道 筛选另建索引（§3.9.2）。
CREATE INDEX notification_templates_category_channel_idx
    ON notification_templates (category, channel);

-- ==== 2. notification_deliveries ====
-- dedupe_key 是**唯一幂等锚**（§3.9.6b）：同一业务事件重复入队走
-- ON CONFLICT DO NOTHING，达到「至少一次 + 幂等键 = 有效一次」。
-- 本表不是 append-only（status/attempt/updated_at 会变），故保留 updated_at。
CREATE TABLE notification_deliveries (
    id                bigint      GENERATED ALWAYS AS IDENTITY,
    template_key      text,
    category          text        NOT NULL,
    channel           text        NOT NULL,
    recipient         text        NOT NULL,
    recipient_user_id bigint,
    locale            text,
    dedupe_key        text        NOT NULL,
    status            text        NOT NULL DEFAULT 'queued',
    attempt           int         NOT NULL DEFAULT 0,
    next_retry_at     timestamptz,
    last_error        text,
    provider_message_id text,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT notification_deliveries_pkey PRIMARY KEY (id),
    CONSTRAINT notification_deliveries_dedupe_key_uq UNIQUE (dedupe_key),
    CONSTRAINT notification_deliveries_channel_chk CHECK (channel IN ('email', 'webhook')),
    CONSTRAINT notification_deliveries_status_chk
        CHECK (status IN ('queued', 'sent', 'failed', 'bounced', 'suppressed')),
    CONSTRAINT notification_deliveries_locale_chk
        CHECK (locale IS NULL OR locale IN ('en', 'zh-CN', 'zh-TW', 'fr', 'ru', 'ja', 'vi')),
    CONSTRAINT notification_deliveries_attempt_chk CHECK (attempt >= 0),
    CONSTRAINT notification_deliveries_recipient_len_chk
        CHECK (char_length(recipient) BETWEEN 1 AND 512),
    -- last_error ≤512（§3.9.5）；错误原文在应用层脱敏后写入。
    CONSTRAINT notification_deliveries_last_error_len_chk
        CHECK (last_error IS NULL OR char_length(last_error) <= 512),
    CONSTRAINT notification_deliveries_dedupe_key_len_chk
        CHECK (char_length(dedupe_key) BETWEEN 1 AND 512)
);

-- worker N8 取待重试：status + next_retry_at（§3.9.5）。
CREATE INDEX notification_deliveries_status_retry_idx
    ON notification_deliveries (status, next_retry_at);
-- 用户侧追溯：recipient_user_id + created_at DESC（§3.9.5）。
CREATE INDEX notification_deliveries_recipient_created_idx
    ON notification_deliveries (recipient_user_id, created_at DESC);
