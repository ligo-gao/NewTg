-- ============================================================================
-- W10-N10/N11：保留/维护主体 newtg_retention 的日志库授权与分区所有权
-- （R-W10-01 A；platform-01 §12.6 / security-09 §2.11 / billing-06 §6.2）
--
-- 角色本身由 deploy/postgres-init/10-roles.sql 创建（initdb 早于迁移）。
-- 本迁移只做「表存在之后」才能做的授权，因此可安全重复执行：
--   · schema USAGE + 全部表 SELECT（保留任务要枚举分区/审计窗口）；
--   · audit_logs 的 DELETE（12 个月到期删行）与**列级** UPDATE(ip)
--     （90 天字段级擦除）；表级 UPDATE / INSERT / TRUNCATE 一律不授，
--     audit_logs 对维护角色也保持 append-only（唯一例外是 ip 列）；
--   · usage_logs 子分区属主转给 newtg_retention（保留角色只拥有子分区，
--     不拥有父表：DROP 子分区即可隐式 detach，最小权限）。
--
-- 【为什么需要 migrate → retention 的角色成员关系】PostgreSQL 要求
-- 「ALTER TABLE ... OWNER TO 新角色」的执行者能 SET ROLE 到新角色。因此
-- 10-roles.sql 额外 GRANT newtg_retention TO newtg_migrate；migrate 本来就是
-- 唯一 DDL 入口（拥有全部对象），这条成员关系不扩大其实际能力。
--
-- 【角色守卫】测试/CI 库可能没有 newtg_retention：缺角色时只 RAISE NOTICE
-- 并跳过，不制造无法执行的授权错误（部署环境角色必定先于 migrate 存在）。
-- ============================================================================

DO $$
DECLARE
    month_start timestamptz := date_trunc('month', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC';
    from_ts     timestamptz;
    to_ts       timestamptz;
    part_name   text;
    part_schema text;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'newtg_retention') THEN
        -- 不能静默 RETURN：迁移会以成功结束、之后所有保留任务都以「角色
        -- 不存在」失败且无法靠重跑迁移修复（I-2 升级路径）。失败会让
        -- golang-migrate 留下 dirty 版本（阻断后续迁移并强制人工修复），
        -- 运维按顺序补齐角色后清理 dirty 再重跑。
        RAISE EXCEPTION 'newtg_retention role is missing; run deploy/postgres-init/10-roles.sql '
            '(or deploy/scripts/retention-upgrade.sql) as the bootstrap superuser, then re-run this migration';
    END IF;

    -- schema USAGE + CREATE。CREATE 仅为满足 PostgreSQL 的所有权转移前置
    -- 条件：ALTER TABLE ... OWNER TO 要求**新属主**在 schema 上有 CREATE。
    -- 保留角色因此可创建对象，但仍不拥有父表 usage_logs（无法整表 DROP）。
    -- 生产由 postgres-init 以超级用户授予；migrate 不是 schema 属主、也没有
    -- GRANT OPTION 时无法代授，只能探测后跳过（由 deploy/scripts/retention-upgrade.sql
    -- 以超级用户身份补齐，再重跑迁移）。
    IF has_schema_privilege(current_user, 'public', 'USAGE WITH GRANT OPTION') THEN
        EXECUTE 'GRANT USAGE ON SCHEMA public TO newtg_retention';
    END IF;
    IF has_schema_privilege(current_user, 'public', 'CREATE WITH GRANT OPTION') THEN
        EXECUTE 'GRANT CREATE ON SCHEMA public TO newtg_retention';
    END IF;
    -- 【N11 回归说明】DELETE 的 WHERE 引用 created_at/id，PostgreSQL 要求
    -- 调用者对这些列有 SELECT；只给 DELETE 不给 SELECT 仍会 permission denied。
    -- 因而 SELECT 是审计保留的必要授权，不是「只读便利」。
    EXECUTE 'GRANT SELECT ON ALL TABLES IN SCHEMA public TO newtg_retention';

    IF to_regclass('public.audit_logs') IS NOT NULL THEN
        EXECUTE 'GRANT DELETE ON audit_logs TO newtg_retention';
        -- 防御性收回：表级 UPDATE 会覆盖列级授权、等于允许改写整行；
        -- INSERT/TRUNCATE 不属于保留职责（自审计走 app 的 append-only 入口）。
        --
        -- 【顺序】必须先 REVOKE 表级 UPDATE，再 GRANT 列级 UPDATE(ip)：
        -- PostgreSQL 的表级 REVOKE UPDATE 会**同时清除**该角色的列级 UPDATE
        -- 授权（20261019000001 已实测）。反过来写会把 ip 擦除权限悄悄抹掉。
        EXECUTE 'REVOKE UPDATE, INSERT, TRUNCATE ON audit_logs FROM newtg_retention';
        EXECUTE 'GRANT UPDATE (ip) ON audit_logs TO newtg_retention';
    END IF;

    IF to_regclass('public.usage_logs') IS NOT NULL THEN
        -- 12 个月窗口（与 R-56 的既有预建口径一致）：既补建缺失月份，也把
        -- 每个目标分区转给保留角色。重复执行 = no-op（IF NOT EXISTS /
        -- ALTER OWNER 幂等）。
        FOR i IN 0..11 LOOP
            from_ts := month_start + (i || ' months')::interval;
            to_ts   := month_start + ((i + 1) || ' months')::interval;
            part_name := 'usage_logs_' || to_char(from_ts AT TIME ZONE 'UTC', 'YYYYMM');
            EXECUTE format(
                'CREATE TABLE IF NOT EXISTS %I PARTITION OF usage_logs FOR VALUES FROM (%L) TO (%L)',
                part_name, from_ts, to_ts);
            EXECUTE format('ALTER TABLE %I OWNER TO newtg_retention', part_name);
        END LOOP;

        -- 窗口之外的历史分区（若存在）同样需要转属主，否则保留任务永远
        -- 删不掉它们（会得到可识别的 ownership 错误）。
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

    -- 后续由 migrate 创建的分区/表默认给保留角色 SELECT；属主仍需在创建时
    -- 转移（R-W10-01 的部署注记：扩窗迁移必须同时 ALTER OWNER）。
    -- migrate 角色在测试/CI 库可能不存在（迁移由超级用户执行），此时跳过，
    -- 否则整条迁移会因为「角色不存在」而失败。
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'newtg_migrate') THEN
        EXECUTE 'ALTER DEFAULT PRIVILEGES FOR ROLE newtg_migrate IN SCHEMA public ' ||
                'GRANT SELECT ON TABLES TO newtg_retention';
    END IF;
END
$$;
