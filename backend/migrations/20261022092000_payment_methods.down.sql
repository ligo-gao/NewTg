-- 回退 S4 自动续费（R-05）。
--
-- 先删除 renewal_failed 事件再恢复旧 CHECK：8 值约束不接受该 kind，留着
-- 这些行会让 ALTER 直接失败。事件流是审计数据，回退是显式的破坏性动作，
-- 因此这里选择删除而不是静默留下不满足约束的行。
DELETE FROM subscription_events WHERE kind = 'renewal_failed';

ALTER TABLE subscription_events DROP CONSTRAINT subscription_events_kind_chk;

ALTER TABLE subscription_events
    ADD CONSTRAINT subscription_events_kind_chk CHECK (kind IN (
        'created', 'renewed', 'upgraded', 'downgraded',
        'refunded', 'cancelled', 'expired', 'reset'
    ));

ALTER TABLE subscriptions
    DROP CONSTRAINT IF EXISTS subscriptions_auto_renew_failures_chk;

ALTER TABLE subscriptions
    DROP COLUMN IF EXISTS auto_renew_attempted_at,
    DROP COLUMN IF EXISTS auto_renew_failures,
    DROP COLUMN IF EXISTS auto_renew_payment_method_id;

DROP INDEX IF EXISTS payment_methods_user_created_idx;

DROP TABLE IF EXISTS payment_methods;
