-- 回退模型性能测试两表。明细先删（外键引用汇总表），再删汇总表。
-- 测试数据是一次性操作产物，不参与计费/审计对账，直接丢弃即可。
DROP TABLE IF EXISTS model_performance_runs;
DROP TABLE IF EXISTS model_performance_tests;
