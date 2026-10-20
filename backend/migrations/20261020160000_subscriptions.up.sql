-- ============================================================================
-- D5a —— 订阅套餐数据层（billing-08 §2/§3/§6；platform-04 §4.5 V10）
--
-- 交付内容：
--   1. subscription_plans       套餐模板（billing-08 §2.1）
--   2. subscriptions            订阅实例（billing-08 §6.1 + R-D5-03 快照列）
--   3. subscription_pre_consume_records  订阅预扣幂等卫星表
--      （platform-04 §4.5 V10 + R-D5-02 的 subscription_id）
--   4. orders ALTER             biz_kind / biz_ref_id（billing-08 §6.2）
--   5. users  ALTER             billing_preference（R-D5-05）
--
-- 为什么这些表不设 FK：全库财务/审计表统一「逻辑引用、不设 FK」
-- （platform-04 §3.4 V5）。订阅删除保护（有实例引用 ⇒ 409）由应用层在同一
-- 事务内锁套餐行后统计完成，而不是靠级联删除改写历史购买事实。
--
-- 为什么 subscriptions 要快照 period_days / reset_on_renew / grace_days /
-- model_scope / concurrency_limit（R-D5-03）：套餐是「一次订阅的模板」，管理
-- 员改价/改额度/改范围不得追溯已购实例——已购实例的周期、重置方式与准入
-- 范围必须在购买瞬间冻结。只快照 plan_title 会让「编辑套餐」在续费/重置
-- 时偷偷改写用户已付费的合同条件。
--
-- 为什么 amount_total = 0 表示「无限额度」（R-D5-07）：0 不是「无额度」，
-- 而是「不设上界」。上界守卫（amount_used <= amount_total）只在
-- amount_total > 0 时由应用层生效，DB 只守 amount_used >= 0；
-- 否则无限额度实例的第一次消费就会被 CHECK 拒绝。
--
-- 枚举 CHECK 一律写成 IN (...)：store.EnumChecks 按 <table>_<column>_chk
-- 双向对账，漂移即集成测试变红（platform-04 §6 V6）。
-- ============================================================================

-- ==== subscription_plans（billing-08 §2.1，17 列）====
--
-- 本表刻意**没有**任何模型价/倍率/折扣字段（billing-08 §2.2 硬约束）：
-- 订阅是资金账户，不是价格表；一次请求的单价仍由 model_pricing /
-- group_pricing / user_groups.ratio 决定。
CREATE TABLE IF NOT EXISTS subscription_plans (
    id                    bigint      GENERATED ALWAYS AS IDENTITY,
    -- 稳定外部标识：改名不改此键；购买/幂等/引用只认它，不认 title。
    plan_key              text        NOT NULL,
    title                 text        NOT NULL,
    description           text,
    -- 周期长度（天）。end_at = start_at + period_days（billing-08 §2.4）。
    period_days           integer     NOT NULL,
    -- 真实货币最小单位（分），与 orders.amount 同口径；不是积分。
    price_amount          bigint      NOT NULL DEFAULT 0,
    -- S1 只支持 CNY（R-D5-07）。char(3) 保留 ISO-4217 形态，便于后续扩币种。
    currency              char(3)     NOT NULL,
    -- 每周期授予订阅账户的积分预算；0 = 无限额度（R-D5-07）。
    grant_credits         bigint      NOT NULL DEFAULT 0,
    reset_on_renew        boolean     NOT NULL DEFAULT true,
    -- subscription_first 回退钱包的开关（billing-08 §4.2 的取值来源）。
    allow_wallet_overflow boolean     NOT NULL DEFAULT true,
    -- 模型白名单（jsonb 文本数组；NULL = 全部模型）。这是**准入过滤**，
    -- 不是定价：不改价，只决定该订阅能否为当前模型付款。
    model_scope           jsonb,
    concurrency_limit     integer,
    -- 到期宽限期（0 = 无宽限）；P1，第一版恒 0。
    grace_days            integer     NOT NULL DEFAULT 0,
    status                text        NOT NULL DEFAULT 'enabled',
    sort_order            integer     NOT NULL DEFAULT 0,
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT subscription_plans_pkey PRIMARY KEY (id),
    CONSTRAINT subscription_plans_plan_key_uq UNIQUE (plan_key),
    CONSTRAINT subscription_plans_status_chk CHECK (status IN ('enabled', 'disabled')),
    CONSTRAINT subscription_plans_currency_chk CHECK (currency IN ('CNY')),
    CONSTRAINT subscription_plans_period_days_chk CHECK (period_days >= 1),
    CONSTRAINT subscription_plans_price_amount_chk CHECK (price_amount >= 0),
    CONSTRAINT subscription_plans_grant_credits_chk CHECK (grant_credits >= 0),
    CONSTRAINT subscription_plans_grace_days_chk CHECK (grace_days >= 0),
    -- billing-08 §2.1：title 1–64、description ≤255。
    CONSTRAINT subscription_plans_title_len_chk
        CHECK (char_length(title) BETWEEN 1 AND 64),
    CONSTRAINT subscription_plans_description_len_chk
        CHECK (description IS NULL OR char_length(description) <= 255),
    -- jsonb 白名单必须是数组（不是对象/标量）；NULL 表示全部模型。
    CONSTRAINT subscription_plans_model_scope_chk
        CHECK (model_scope IS NULL OR jsonb_typeof(model_scope) = 'array')
);

-- billing-08 §2.1 的展示索引：用户侧只取 enabled 且按 sort_order 排序。
CREATE INDEX IF NOT EXISTS subscription_plans_status_sort_idx
    ON subscription_plans (status, sort_order);

-- ==== subscriptions（billing-08 §6.1，23 列）====
--
-- 状态机：active → expired | cancelled，终态不可再迁移（billing-08 §3.1）。
-- 续费不新建行，而是推进 end_at（§3.3 方案甲）：会话在 PreConsume 时冻结
-- subscriptionId，换行会让同一请求的结算落到不同实例。
CREATE TABLE IF NOT EXISTS subscriptions (
    id                    bigint      GENERATED ALWAYS AS IDENTITY,
    -- 逻辑引用 users.id（不设 FK）。
    user_id               bigint      NOT NULL,
    -- 逻辑引用 subscription_plans.id（不设 FK）。
    plan_id               bigint      NOT NULL,
    -- 购买时套餐标题快照：账单日志 subscription_plan_title 的取值来源。
    plan_title_snapshot   text        NOT NULL,
    status                text        NOT NULL DEFAULT 'active',
    start_at              timestamptz NOT NULL DEFAULT now(),
    end_at                timestamptz NOT NULL,
    -- 下一额度重置时刻；B12 条件更新据此扫描并推进（NULL = 不重置）。
    next_reset_at         timestamptz,
    -- 本周期总额度（= 购买瞬间的 plan.grant_credits）；0 = 无限额度。
    amount_total          bigint      NOT NULL DEFAULT 0,
    -- 已用额度由会话（C）经 SubscriptionFunding 读写；下界 0。
    amount_used           bigint      NOT NULL DEFAULT 0,
    -- 快照自 plan：资金来源回退读此，不读当前 plan（编辑不追溯）。
    allow_wallet_overflow boolean     NOT NULL DEFAULT true,
    auto_renew            boolean     NOT NULL DEFAULT false,
    cancel_at_period_end  boolean     NOT NULL DEFAULT false,
    -- 逻辑引用 orders.id；UNIQUE 保证同一订单只建一条实例（§3.2-3）。
    -- PG 唯一索引默认忽略 NULL，免费/人工实例可以不绑订单。
    order_id              bigint,
    currency              char(3)     NOT NULL,
    price_paid_amount     bigint      NOT NULL DEFAULT 0,
    -- R-D5-03 快照列：购买瞬间冻结，之后编辑套餐不影响本实例。
    period_days           integer     NOT NULL,
    reset_on_renew        boolean     NOT NULL DEFAULT true,
    grace_days            integer     NOT NULL DEFAULT 0,
    model_scope           jsonb,
    concurrency_limit     integer,
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT subscriptions_pkey PRIMARY KEY (id),
    CONSTRAINT subscriptions_order_id_uq UNIQUE (order_id),
    CONSTRAINT subscriptions_status_chk CHECK (status IN ('active', 'expired', 'cancelled')),
    CONSTRAINT subscriptions_currency_chk CHECK (currency IN ('CNY')),
    -- 时间列全部 timestamptz 且到期必须晚于生效（R-D5-09）。
    CONSTRAINT subscriptions_end_at_chk CHECK (end_at > start_at),
    CONSTRAINT subscriptions_amount_total_chk CHECK (amount_total >= 0),
    CONSTRAINT subscriptions_amount_used_chk CHECK (amount_used >= 0),
    CONSTRAINT subscriptions_price_paid_amount_chk CHECK (price_paid_amount >= 0),
    CONSTRAINT subscriptions_period_days_chk CHECK (period_days >= 1),
    CONSTRAINT subscriptions_grace_days_chk CHECK (grace_days >= 0),
    CONSTRAINT subscriptions_model_scope_chk
        CHECK (model_scope IS NULL OR jsonb_typeof(model_scope) = 'array')
);

-- 热路径：HasActiveUserSubscription 与「按 end_at asc, id asc 逐个扣减」
-- （billing-08 §3.1 / §4.2）。部分索引只覆盖活跃行，规模跟活跃订阅走。
CREATE INDEX IF NOT EXISTS subscriptions_active_idx
    ON subscriptions (user_id, end_at)
    WHERE status = 'active';
-- B12 到期扫描（billing-08 §7）。
CREATE INDEX IF NOT EXISTS subscriptions_expiry_idx
    ON subscriptions (status, end_at)
    WHERE status = 'active';
-- B12 额度重置扫描；NULL next_reset_at 的行永不参与重置。
CREATE INDEX IF NOT EXISTS subscriptions_reset_idx
    ON subscriptions (status, next_reset_at)
    WHERE status = 'active' AND next_reset_at IS NOT NULL;
-- 套餐删除保护与「按套餐批量重置额度」的反查。
CREATE INDEX IF NOT EXISTS subscriptions_plan_idx ON subscriptions (plan_id);

-- ==== subscription_pre_consume_records（platform-04 §4.5 V10，5 列）====
--
-- 幂等键 = 计费 request_id（uuid，与 request_registry.request_id / usage_logs
-- .request_id 同型；R-D5-02 / billing-08 C-1）。PK 本身是唯一索引，也是
-- 「并发两次同 requestId 收敛为一条」的根。
--
-- subscription_id（R-D5-02）：一次预扣只能落在一个订阅实例，**禁止跨实例
-- 拆分**——否则退款/兜底恢复时无法确定该回退哪条 amount_used。
CREATE TABLE IF NOT EXISTS subscription_pre_consume_records (
    request_id      uuid        NOT NULL,
    subscription_id bigint      NOT NULL,
    -- consumed → refunded 单向；已 refunded 的 requestId 再预扣必须报错。
    status          text        NOT NULL DEFAULT 'consumed',
    pre_consumed    bigint      NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT subscription_pre_consume_records_pkey PRIMARY KEY (request_id),
    CONSTRAINT subscription_pre_consume_records_status_chk
        CHECK (status IN ('consumed', 'refunded'))
);

-- 「某实例的预扣记录」反查（退款/对账/清理时按 subscription_id 聚合）。
CREATE INDEX IF NOT EXISTS subscription_pre_consume_records_subscription_idx
    ON subscription_pre_consume_records (subscription_id);

-- ==== orders：区分充值 / 订阅购买（billing-08 §6.2，R-D5-04）====
--
-- 默认 'topup'：既有充值订单语义不变；'subscribe' 时 biz_ref_id =
-- subscription_plans.id。订阅购买复用 orders 的幂等/回调/订单中心，
-- 不新建第二个支付 owner。
ALTER TABLE orders ADD COLUMN biz_kind text NOT NULL DEFAULT 'topup';
ALTER TABLE orders ADD COLUMN biz_ref_id bigint;
ALTER TABLE orders ADD CONSTRAINT orders_biz_kind_chk
    CHECK (biz_kind IN ('topup', 'subscribe'));

-- ==== users：扣费来源偏好（R-D5-05）====
--
-- 四值与 contract.BillingPreference 对应；空值/未知经 NormalizeBillingPreference
-- 归一为 subscription_first。DB 只接受四个字面量，防止绕过归一的脏值落库。
ALTER TABLE users ADD COLUMN billing_preference text NOT NULL DEFAULT 'subscription_first';
ALTER TABLE users ADD CONSTRAINT users_billing_preference_chk
    CHECK (billing_preference IN ('subscription_first', 'wallet_first', 'subscription_only', 'wallet_only'));
