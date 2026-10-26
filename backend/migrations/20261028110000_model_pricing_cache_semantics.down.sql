-- 回退 20261028110000_model_pricing_cache_semantics。
--
-- 本迁移只写过列注释，没有改类型或数据；回滚清空注释即可。
COMMENT ON COLUMN model_pricing.prompt_price IS NULL;
COMMENT ON COLUMN model_pricing.cache_read_price IS NULL;
COMMENT ON COLUMN model_pricing.completion_price IS NULL;
COMMENT ON COLUMN model_pricing.cache_write_price IS NULL;
COMMENT ON COLUMN model_pricing.strategy IS NULL;
COMMENT ON COLUMN group_pricing.cache_read_price IS NULL;
COMMENT ON COLUMN group_pricing.cache_write_price IS NULL;
