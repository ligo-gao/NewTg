-- 日志库初始 schema 逆操作（platform-04 §2.2 / §7.2）。
-- 分区随父表一起删除，不单独 drop 分区；数据全丢，仅适用于空库/全新环境回滚。
DROP TABLE IF EXISTS usage_logs, audit_logs CASCADE;
