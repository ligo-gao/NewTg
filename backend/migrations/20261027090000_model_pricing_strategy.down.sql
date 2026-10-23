-- 回退模型定价时段策略（20261027090000_model_pricing_strategy）。
--
-- 顺序与 up 相反：先删两张新表，再删 model_pricing 上的约束与列。
-- 已保存的 strategy 会随列一起丢失（spec §5.6：可接受的回滚代价）。
DROP TABLE IF EXISTS request_pricing;
DROP TABLE IF EXISTS holiday_calendar;

ALTER TABLE model_pricing DROP CONSTRAINT IF EXISTS model_pricing_strategy_object_chk;
ALTER TABLE model_pricing DROP COLUMN IF EXISTS strategy;
