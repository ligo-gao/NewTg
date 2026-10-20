-- 回退 D5a 订阅套餐数据层。
--
-- 顺序：先移除对既有表的扩展（users/orders），再按依赖相反顺序删订阅三表。
-- 虽然没有 FK，subscription_pre_consume_records / subscriptions 都逻辑引用
-- subscription_plans；先删引用方再删被引用方，避免回退到一半时出现悬挂引用
-- 被后续操作读到（down 在同一迁移事务内执行，顺序仍是必要文档）。
--
-- 约束先删再删列：约束依赖列，显式顺序让回退在部分 PG 版本上不依赖
-- DROP COLUMN 的隐式级联行为（与 default_model 回退同口径）。

ALTER TABLE users DROP CONSTRAINT IF EXISTS users_billing_preference_chk;
ALTER TABLE users DROP COLUMN IF EXISTS billing_preference;

ALTER TABLE orders DROP CONSTRAINT IF EXISTS orders_biz_kind_chk;
ALTER TABLE orders DROP COLUMN IF EXISTS biz_ref_id;
ALTER TABLE orders DROP COLUMN IF EXISTS biz_kind;

DROP TABLE IF EXISTS subscription_pre_consume_records;
DROP TABLE IF EXISTS subscriptions;
DROP TABLE IF EXISTS subscription_plans;
