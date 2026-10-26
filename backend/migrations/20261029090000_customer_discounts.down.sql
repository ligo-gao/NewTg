-- 回退客户折扣规则（20261029090000_customer_discounts）。
--
-- 顺序与 up 相反：先删 request_pricing 的冻结折扣列，再删 discounts 表，
-- 最后回退 users.customer_type。折扣规则是配置数据，随表一起丢弃。
ALTER TABLE request_pricing
  DROP COLUMN IF EXISTS discount_target,
  DROP COLUMN IF EXISTS discount_rate;

DROP TABLE IF EXISTS discounts;

DROP INDEX IF EXISTS users_customer_type_idx;
ALTER TABLE users DROP CONSTRAINT IF EXISTS users_customer_type_chk;
ALTER TABLE users DROP COLUMN IF EXISTS customer_type;
