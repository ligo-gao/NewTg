-- ============================================================================
-- W2 / S4 —— 自动续费：支付令牌 + 订阅续费状态
--
-- 冻结裁决（R-05 / P-14 / P-16 / P-17 / R-40 / R-42）：
--   * 支付令牌只支持 Stripe（P-14）。`token_cipher` 存 SecretSealer 的
--     AES-256-GCM 密文（"v1:" 前缀 + base64(nonce||ciphertext)），**永不**
--     存明文；品牌/末四位/有效期是展示所需的非敏感元数据，用于「零回显」
--     的读取响应（只回 brand/last4/exp，绝不回令牌）。
--   * 令牌是 Stripe PaymentMethod id（pm_...），不是卡号：网关不接触 PAN，
--     保持 PCI 范围最小化（与在线充值的 Elements 口径一致）。
--   * subscriptions.auto_renew 已存在（D5a）；这里补三列把「自动续费」
--     从布尔开关变成可执行、可审计的状态机：
--       - auto_renew_payment_method_id：本实例扣费使用的令牌（逻辑引用，
--         V5 不设 FK；令牌删除由应用层同事务 fail-closed 关闭 auto_renew）。
--       - auto_renew_failures：连续失败次数。P-16 冻结「连续 3 次失败 →
--         auto_renew=false」，成功续费后归零；跨周期不累计。
--       - auto_renew_attempted_at：最近一次扣费尝试时刻。它同时承担两件事：
--         ① 失败重试的退避（否则每分钟扫描会在一小时内打满 3 次）；
--         ② 成功后的重复扣费保护（成功会推进 end_at，窗口条件随之后移，
--            该时刻只作为审计/退避锚点，不参与「本周期已扣过」的判定）。
--   * subscription_events.kind 增加 'renewal_failed'（P-16 明确要求该事件）。
--     原 8 值是 S1 冻结白名单，S4 是**有意扩展**而非拼写漂移；CHECK 与
--     store.subscriptionEventKinds / store.EnumChecks 三处必须同步。
--
-- 为什么 payment_methods.id 用 uuid：它是外部可寻址的支付凭据句柄，uuid
-- 避免自增 id 泄露令牌数量；同时与列表分页/删除路径的自然键一致。
-- ============================================================================

CREATE TABLE IF NOT EXISTS payment_methods (
    id            uuid        NOT NULL DEFAULT gen_random_uuid(),
    -- 逻辑引用 users.id（V5：财务/凭据类表不设 FK，用户硬删除后审计仍可追溯）。
    user_id       bigint      NOT NULL,
    -- P-14：只允许 Stripe。非法值进不来，未来扩展渠道必须改 CHECK。
    provider      text        NOT NULL DEFAULT 'stripe',
    -- SecretSealer 密文；任何读取路径都不得把它投影到响应/日志（P-14 零回显）。
    token_cipher  text        NOT NULL,
    -- 展示元数据（非敏感）：card brand / last4 / 有效期。
    brand         text        NOT NULL,
    last4         text        NOT NULL,
    exp_month     smallint    NOT NULL,
    exp_year      smallint    NOT NULL,
    -- invalid 由 worker 在解密失败/渠道拒绝该令牌时置位；保留行以便审计，
    -- 不物理删除（删除是用户的显式动作）。
    status        text        NOT NULL DEFAULT 'active',
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT payment_methods_pkey PRIMARY KEY (id),
    CONSTRAINT payment_methods_provider_chk CHECK (provider IN ('stripe')),
    CONSTRAINT payment_methods_status_chk CHECK (status IN ('active', 'invalid')),
    CONSTRAINT payment_methods_token_cipher_len_chk
        CHECK (char_length(token_cipher) BETWEEN 16 AND 4096),
    CONSTRAINT payment_methods_brand_len_chk
        CHECK (char_length(brand) BETWEEN 1 AND 32),
    CONSTRAINT payment_methods_last4_chk CHECK (last4 ~ '^[0-9]{4}$'),
    CONSTRAINT payment_methods_exp_month_chk CHECK (exp_month BETWEEN 1 AND 12),
    CONSTRAINT payment_methods_exp_year_chk CHECK (exp_year BETWEEN 2000 AND 9999)
);

-- 用户侧列表：本人令牌按创建时间倒序（写入面只有一个用户，无需全局索引）。
CREATE INDEX IF NOT EXISTS payment_methods_user_created_idx
    ON payment_methods (user_id, created_at DESC);

ALTER TABLE subscriptions
    ADD COLUMN auto_renew_payment_method_id uuid,
    ADD COLUMN auto_renew_failures integer NOT NULL DEFAULT 0,
    ADD COLUMN auto_renew_attempted_at timestamptz;

ALTER TABLE subscriptions
    ADD CONSTRAINT subscriptions_auto_renew_failures_chk
    CHECK (auto_renew_failures >= 0);

ALTER TABLE subscription_events DROP CONSTRAINT subscription_events_kind_chk;

ALTER TABLE subscription_events
    ADD CONSTRAINT subscription_events_kind_chk CHECK (kind IN (
        'created', 'renewed', 'upgraded', 'downgraded',
        'refunded', 'cancelled', 'expired', 'reset',
        -- S4：每次自动续费扣款失败（含第 3 次触发 auto_renew=false）。
        'renewal_failed'
    ));
