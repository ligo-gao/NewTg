-- 回滚 W10-N10/N11 的保留角色授权与分区所有权。
--
-- 目标终态：newtg_retention 对日志库不再有任何 DELETE/UPDATE 与分区属主；
-- 分区属主回到唯一 DDL 入口 newtg_migrate（与 20261019000001 之后的状态一致）。
-- 角色本身保留（由 deploy/postgres-init 管理，迁移不负责删角色）。
--
-- 【角色守卫】与 up 对称：缺角色时无可回滚，直接跳过。

DO $$
DECLARE
    part_name   text;
    part_schema text;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'newtg_retention') THEN
        RAISE NOTICE 'newtg_retention role absent; retention rollback skipped';
        RETURN;
    END IF;

    IF to_regclass('public.audit_logs') IS NOT NULL THEN
        EXECUTE 'REVOKE DELETE ON audit_logs FROM newtg_retention';
        EXECUTE 'REVOKE UPDATE (ip) ON audit_logs FROM newtg_retention';
    END IF;

    IF to_regclass('public.usage_logs') IS NOT NULL THEN
        -- 只有 newtg_migrate 存在时才把属主还给它；测试/CI 库可能没有该角色，
        -- 此时保留现状（分区仍归 retention，回滚的授权面已收回）。
        IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'newtg_migrate') THEN
            FOR part_name, part_schema IN
                SELECT c.relname, n.nspname
                  FROM pg_inherits i
                  JOIN pg_class c     ON c.oid = i.inhrelid
                  JOIN pg_class p     ON p.oid = i.inhparent
                  JOIN pg_namespace n ON n.oid = c.relnamespace
                 WHERE p.oid = to_regclass('public.usage_logs')
            LOOP
                EXECUTE format('ALTER TABLE %I.%I OWNER TO newtg_migrate', part_schema, part_name);
            END LOOP;
        END IF;
    END IF;

    EXECUTE 'REVOKE SELECT ON ALL TABLES IN SCHEMA public FROM newtg_retention';
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'newtg_migrate') THEN
        EXECUTE 'ALTER DEFAULT PRIVILEGES FOR ROLE newtg_migrate IN SCHEMA public ' ||
                'REVOKE SELECT ON TABLES FROM newtg_retention';
    END IF;
END
$$;
