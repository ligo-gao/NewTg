-- 回滚 R-60 的列级 UPDATE 例外。表级 REVOKE UPDATE 同时清除所有列级
-- UPDATE 授权（已实测）；worker 的 refunded 收敛随后会以 permission denied
-- 失败（fail-closed），不会静默丢日志。
-- 12 个月预建的分区不回滚：删除含数据的分区是破坏性操作，且分区保留期
-- 由 retention 主体负责。
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'newtg_app') THEN
        EXECUTE 'REVOKE UPDATE ON usage_logs FROM newtg_app';
    END IF;
END
$$;
