-- platform-01 §6 / §10 约束补齐的逆操作（R-24）。
-- 按名删除 up 中新增的 6 条 CHECK；IF EXISTS 使回滚幂等，且不触碰 init 迁移
-- 自带的约束（枚举/数值区间等保持不变）。
ALTER TABLE channels DROP CONSTRAINT IF EXISTS channels_name_len_chk;
ALTER TABLE channels DROP CONSTRAINT IF EXISTS channels_test_model_in_models_chk;
ALTER TABLE channels DROP CONSTRAINT IF EXISTS channels_tag_chk;

ALTER TABLE orders DROP CONSTRAINT IF EXISTS orders_currency_chk;
ALTER TABLE orders DROP CONSTRAINT IF EXISTS orders_provider_currency_chk;
ALTER TABLE orders DROP CONSTRAINT IF EXISTS orders_fx_rate_pair_chk;
