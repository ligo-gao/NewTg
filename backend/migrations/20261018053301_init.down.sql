-- 主库 init 的逆操作：逆序 DROP 全 16 表（CASCADE 连带 FK 与索引，platform-04 §7.2）。
-- 数据全丢——空库/全新环境无损；生产执行前须有可恢复转储（§7.4）。
DROP TABLE IF EXISTS request_registry CASCADE;
DROP TABLE IF EXISTS rankings_entities CASCADE;
DROP TABLE IF EXISTS rankings_timeseries CASCADE;
DROP TABLE IF EXISTS rankings_provider_stats CASCADE;
DROP TABLE IF EXISTS rankings_model_stats CASCADE;
DROP TABLE IF EXISTS rankings_snapshot_runs CASCADE;
DROP TABLE IF EXISTS wallet_transactions CASCADE;
DROP TABLE IF EXISTS orders CASCADE;
DROP TABLE IF EXISTS redemption_codes CASCADE;
DROP TABLE IF EXISTS group_pricing CASCADE;
DROP TABLE IF EXISTS model_pricing CASCADE;
DROP TABLE IF EXISTS channels CASCADE;
DROP TABLE IF EXISTS system_settings CASCADE;
DROP TABLE IF EXISTS user_groups CASCADE;
DROP TABLE IF EXISTS api_tokens CASCADE;
DROP TABLE IF EXISTS users CASCADE;
