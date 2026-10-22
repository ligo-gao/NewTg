-- ============================================================================
-- W10-N10/N11 既有数据卷升级脚本（R-W10-01 / I-2 升级路径）
--
-- 适用：**已存在的数据卷**（initdb 早已跑过，20-databases.sql 的初始授权
-- 早于 newtg_retention 与新表存在）。新装环境不需要本脚本——按顺序执行
-- postgres-init → migrate 即可。
--
-- 升级顺序（严格按此顺序）：
--   1) 以引导超级用户重跑 `deploy/postgres-init/10-roles.sql`（创建/更新
--      newtg_retention 角色与 migrate→retention 成员关系）；
--   2) 以引导超级用户对**日志库**执行本脚本；
--   3) 以 newtg_migrate 运行 `RUN_MODE=migrate`（应用日志库迁移；迁移会
--      再做一遍同样的授权，属主/窗口幂等）。
--
-- 执行方式（容器部署，口令由 psql 交互/环境提供，脚本内不写明文）：
--   docker compose -f deploy/docker-compose.yml exec -T postgres \
--     psql -U "$POSTGRES_USER" -d "$POSTGRES_LOGS_DB" \
--     -f /dev/stdin < deploy/scripts/retention-upgrade.sql
--
-- 本脚本可重复执行：全部 GRANT/REVOKE/ALTER OWNER 幂等。
-- ============================================================================

DO $$
DECLARE
    part_name   text;
    part_schema text;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'newtg_retention') THEN
        RAISE EXCEPTION 'newtg_retention role is missing; run deploy/postgres-init/10-roles.sql first';
    END IF;

    -- 所有权转移的前置：新属主在 schema 上需要 CREATE（见迁移注释）。
    EXECUTE 'GRANT USAGE, CREATE ON SCHEMA public TO newtg_retention';

    -- SELECT 是审计保留的必要授权：DELETE 的 WHERE 引用 created_at/id。
    EXECUTE 'GRANT SELECT ON ALL TABLES IN SCHEMA public TO newtg_retention';

    IF to_regclass('public.audit_logs') IS NOT NULL THEN
        EXECUTE 'GRANT DELETE ON audit_logs TO newtg_retention';
        -- 先收回表级 UPDATE（会连带清除列级授权），再授 ip 一列。
        EXECUTE 'REVOKE UPDATE, INSERT, TRUNCATE ON audit_logs FROM newtg_retention';
        EXECUTE 'GRANT UPDATE (ip) ON audit_logs TO newtg_retention';
    END IF;

    IF to_regclass('public.usage_logs') IS NOT NULL THEN
        FOR part_name, part_schema IN
            SELECT c.relname, n.nspname
              FROM pg_inherits i
              JOIN pg_class c     ON c.oid = i.inhrelid
              JOIN pg_class p     ON p.oid = i.inhparent
              JOIN pg_namespace n ON n.oid = c.relnamespace
             WHERE p.oid = to_regclass('public.usage_logs')
        LOOP
            EXECUTE format('ALTER TABLE %I.%I OWNER TO newtg_retention', part_schema, part_name);
        END LOOP;
    END IF;

    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'newtg_migrate') THEN
        EXECUTE 'ALTER DEFAULT PRIVILEGES FOR ROLE newtg_migrate IN SCHEMA public ' ||
                'GRANT SELECT ON TABLES TO newtg_retention';
    END IF;
END
$$;
