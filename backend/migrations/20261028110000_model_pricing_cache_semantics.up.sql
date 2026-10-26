-- 把「缓存命中 / 未命中」的价格语义写进 schema（docs/billing-02-定价解析与配置-设计.md）。
--
-- 【为什么不改类型、不回填】四列价格与 strategy 列在 20261018053301_init 与
-- 20261027090000_model_pricing_strategy 中已经就是这套语义；缺的是库层可读的
-- 说明。后来者只看列名容易把 prompt_price 读成「全部输入」，把 cache_read_price
-- 的 NULL 读成「免费」。本迁移只加注释，不改任何数据，回滚清空注释即可。
--
-- NULL 与 0 的三态必须在注释里说清：NULL = 未设置（走默认/兜底），
-- 0 = 显式免费。这个区别直接决定结算金额，不能靠读代码猜。
COMMENT ON COLUMN model_pricing.prompt_price IS '输入（缓存未命中）价格，整数积分 / 1M tokens；NULL = 未设置，走默认价';
COMMENT ON COLUMN model_pricing.cache_read_price IS '输入（缓存命中）价格，整数积分 / 1M tokens；NULL = 按未命中价计（DefaultCacheRatio = 1.0）；0 = 免费';
COMMENT ON COLUMN model_pricing.completion_price IS '输出价格，整数积分 / 1M tokens；NULL = 未设置，走模型族兜底';
COMMENT ON COLUMN model_pricing.cache_write_price IS '缓存写入价格，整数积分 / 1M tokens；NULL = 未命中价 × 1.25；0 = 显式免费';
COMMENT ON COLUMN model_pricing.strategy IS '时段策略 {"version":1,"time":{...}}；NULL = 未启用';
COMMENT ON COLUMN group_pricing.cache_read_price IS '缓存命中覆盖价，整数积分 / 1M tokens；0 = 免费';
COMMENT ON COLUMN group_pricing.cache_write_price IS '缓存写入覆盖价，整数积分 / 1M tokens；0 = 免费';
