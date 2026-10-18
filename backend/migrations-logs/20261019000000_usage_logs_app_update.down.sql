-- 回滚 usage_logs 的列级 UPDATE 例外（R-25/R-59 收敛依赖它；回滚后 worker
-- 的 refunded 收敛会因权限失败而报错，而不是静默成功——保持 fail-closed）。
-- 12 个月预建的分区不回滚：删除含数据的分区是破坏性操作，生产不应由迁移
-- 回滚触发；需要清理时走 retention 主体的 DETACH/DROP。
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'newtg_app') THEN
        EXECUTE 'REVOKE UPDATE (status, cost, refunded) ON usage_logs FROM newtg_app';
    END IF;
END
$$;
