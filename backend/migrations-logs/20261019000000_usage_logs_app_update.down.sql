-- 回滚 usage_logs 的 UPDATE 例外（R-25 收敛依赖它；回滚后 worker 的
-- refunded 收敛会因权限失败而报错，而不是静默成功——保持 fail-closed）。
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'newtg_app') THEN
        EXECUTE 'REVOKE UPDATE ON usage_logs FROM newtg_app';
    END IF;
END
$$;
