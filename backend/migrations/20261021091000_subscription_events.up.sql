-- ============================================================================
-- W2 / S1 —— 订阅状态迁移事件流（R-05 / R-17 / R-18）
--
-- 订阅的每一次生命周期变化（created / renewed / upgraded / downgraded /
-- refunded / cancelled / expired / reset）都必须在**同一事务**里追加一条
-- 事件，才能回答「这条订阅在什么时候、因为什么变成了现在这样」。
-- S2（升级/降级）、S3（退款）、S4（自动续费）复用这张表，不各自建第二套
-- 历史。
--
-- 【为什么主键是 (subscription_id, seq) 而不是自增 id】
-- seq 是「同一订阅内」连续无洞的序号：客户端用 `after_seq=` 增量拉取，
-- 只要保证同订阅内单调递增，就不会漏事件。全局自增序列在并发提交时会
-- 出现「号段乱序」——seq 小的先分配、后提交，after_seq 客户端已经推进到
-- 更大的值，就会永远看不到晚提交的小号事件。按订阅分区编号 + 订阅行锁
-- （见 store.AppendSubscriptionEvent）避免了这个问题。
--
-- 【为什么 detail 不设业务列】R-18 只允许放 id 与金额（不得写密钥、支付
-- 凭据、完整邮箱）。detail 用 jsonb 承载稀疏字段，新增 kind 不必改表；
-- 约束只守「是对象」，字段白名单由应用层冻结在事件构造处。
-- ============================================================================
CREATE TABLE IF NOT EXISTS subscription_events (
    -- 逻辑引用 subscriptions.id（不设 FK，与全库财务/审计表同口径 V5）：
    -- 订阅行删除后事件仍应可审计，级联删除会改写历史。
    subscription_id bigint      NOT NULL,
    -- 同订阅内从 1 开始、连续无洞的序号（R-17）。
    seq             bigint      NOT NULL,
    -- R-18 冻结的八种迁移。
    kind            text        NOT NULL,
    -- 只放 id 与金额等非敏感上下文；空对象是合法值。
    detail          jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT subscription_events_pkey PRIMARY KEY (subscription_id, seq),
    CONSTRAINT subscription_events_seq_chk CHECK (seq >= 1),
    CONSTRAINT subscription_events_kind_chk CHECK (kind IN (
        'created', 'renewed', 'upgraded', 'downgraded',
        'refunded', 'cancelled', 'expired', 'reset'
    )),
    -- detail 必须是 JSON 对象：数组/标量会让读侧的字段访问形式不统一。
    CONSTRAINT subscription_events_detail_chk CHECK (jsonb_typeof(detail) = 'object')
);

-- 读路径固定是「某订阅 seq > after_seq 升序」——主键 (subscription_id, seq)
-- 已能直接服务该范围扫描，不另建索引（避免每条事件两次索引写放大）。
