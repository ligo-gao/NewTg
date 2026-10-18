-- R-60 / R-61 迁移卫生：20261019000000 已在 ad28967/e2920c6 中被提交并被部分
-- 环境应用，**不再修改它**（golang-migrate 只记录版本号，不重放已应用的迁移）。
-- 本版本把两类环境收敛到同一终态：
--   - 应用过 ad28967（表级 UPDATE）的环境；
--   - 新装环境（先应用 20261019000000 的 3 列授权，再应用本版本）。
--
-- 终态：newtg_app 只拥有 usage_logs 的 5 列 UPDATE
-- （status/cost/refunded/error_class/error_message，R-25/R-59/R-60），
-- 表级 UPDATE 与 audit_logs 的 UPDATE 都不存在。
--
-- 【REVOKE 语义】PostgreSQL 的表级 REVOKE UPDATE 同时清除该角色的列级
-- UPDATE 授权（已实测：revoke 后 has_table_privilege 与 has_column_privilege
-- 均为 false），因此下面一条 REVOKE 足以清掉 20261019000000 的 3 列授权或
-- ad28967 的表级授权。
--
-- 【角色守卫】测试/CI 数据库可能没有 newtg_app；缺角色时跳过，部署环境
-- 角色必定先于 migrate 存在。
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'newtg_app') THEN
        EXECUTE 'REVOKE UPDATE ON usage_logs FROM newtg_app';
        EXECUTE 'GRANT UPDATE (status, cost, refunded, error_class, error_message) ON usage_logs TO newtg_app';
    END IF;
END
$$;

-- R-56：12 个月分区窗口（当月 + 下 11 个月）。对已应用老版 20261019000000
-- 的环境，这一步补上预建；对新装环境是幂等重复（IF NOT EXISTS）。
DO $$
DECLARE
    month_start timestamptz := date_trunc('month', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC';
    from_ts     timestamptz;
    to_ts       timestamptz;
BEGIN
    FOR i IN 0..11 LOOP
        from_ts := month_start + (i || ' months')::interval;
        to_ts   := month_start + ((i + 1) || ' months')::interval;
        EXECUTE format(
            'CREATE TABLE IF NOT EXISTS %I PARTITION OF usage_logs FOR VALUES FROM (%L) TO (%L)',
            'usage_logs_' || to_char(from_ts AT TIME ZONE 'UTC', 'YYYYMM'),
            from_ts, to_ts);
    END LOOP;
END
$$;
