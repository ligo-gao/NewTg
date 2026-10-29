-- NewTG 日志库 newtg_logs（G7-R36：主库 / 日志库分离；M0 同实例不同 database）
--
-- 执行时机：接在 10-roles.sql 之后（同一 initdb 阶段）；角色已存在。
-- 库名取 POSTGRES_LOGS_DB，与 compose 的 DB_LOGS_DSN 使用同一插值变量。

\set ON_ERROR_STOP on

\getenv logs_db POSTGRES_LOGS_DB

-- fail-fast：库名为空则中止 initdb（psql \if 不接受表达式，先 \gset 存布尔结果；
-- psql 16 的 \quit 不支持退出码，故用 ON_ERROR_STOP + RAISE 产生非 0 退出）。
SELECT (:'logs_db' = '') AS db_empty \gset
\if :db_empty
\echo 'ERROR: POSTGRES_LOGS_DB is empty'
DO $$ BEGIN RAISE EXCEPTION 'POSTGRES_LOGS_DB is empty'; END $$;
\endif

-- 幂等创建（\gexec 执行查询结果；CREATE DATABASE 不能位于事务块内，psql 自动提交）。
SELECT format('CREATE DATABASE %I', :'logs_db')
WHERE NOT EXISTS (SELECT 1 FROM pg_database WHERE datname = :'logs_db')\gexec

-- 日志库也只允许显式角色连接（PUBLIC 默认 CONNECT 被收回；见 10-roles.sql
-- 对主库的同口径处理）。newtg_retention 只在这里获得 CONNECT。
REVOKE CONNECT ON DATABASE :"logs_db" FROM PUBLIC;
GRANT CONNECT ON DATABASE :"logs_db" TO newtg_app, newtg_migrate, newtg_backup, newtg_retention;

-- 切到日志库：schema 与默认权限按库分别设置。
\connect :"logs_db"

GRANT USAGE ON SCHEMA public TO newtg_app, newtg_backup;
-- newtg_retention 需要 CREATE 才能成为 usage_logs 子分区的新属主：
-- PostgreSQL 的 ALTER TABLE ... OWNER TO 要求新属主在对象所在 schema 上有
-- CREATE（只是所有权转移的前置检查，不是让保留任务执行 DDL）。它仍不拥有
-- 父表 usage_logs，无法 DROP/改写整表。
GRANT USAGE, CREATE ON SCHEMA public TO newtg_retention;
GRANT USAGE, CREATE ON SCHEMA public TO newtg_migrate;
REVOKE CREATE ON SCHEMA public FROM newtg_app, newtg_backup;

-- 日志库内全部表都是日志/审计表：newtg_app 默认只有 INSERT + SELECT
-- （platform-01 §12.6 / security-09：不授 UPDATE/DELETE；保留清理归独立维护主体）。
-- 【R-25/R-59/R-60 例外】usage_logs 是唯一需要 UPDATE 的日志表：worker 兜底
-- 退款要把已有的 success/error 行单向收敛为 refunded，并记录保留消耗与退款
-- 原因。只授**列级** UPDATE (status, cost, refunded, error_class,
-- error_message)，不授表级 UPDATE；audit_logs **保持 append-only**，绝不授
-- UPDATE。
ALTER DEFAULT PRIVILEGES FOR ROLE newtg_migrate IN SCHEMA public
    GRANT SELECT, INSERT ON TABLES TO newtg_app;
ALTER DEFAULT PRIVILEGES FOR ROLE newtg_migrate IN SCHEMA public
    GRANT USAGE, SELECT ON SEQUENCES TO newtg_app;
ALTER DEFAULT PRIVILEGES FOR ROLE newtg_migrate IN SCHEMA public
    GRANT SELECT ON TABLES TO newtg_backup;
ALTER DEFAULT PRIVILEGES FOR ROLE newtg_migrate IN SCHEMA public
    GRANT SELECT ON SEQUENCES TO newtg_backup;
-- W10-N10/N11：保留/维护主体只读 + 审计行的删除/ip 擦除（表存在后由日志库
-- 迁移 20261022000000 授权；此处为「对既有数据卷重跑脚本」的条件补授）。
ALTER DEFAULT PRIVILEGES FOR ROLE newtg_migrate IN SCHEMA public
    GRANT SELECT ON TABLES TO newtg_retention;

-- usage_logs 的列级 UPDATE 例外（R-25/R-59/R-60）。initdb 早于迁移：首次
-- 初始化时 usage_logs 还不存在，因此这里只能对「表已存在」的情形补授（例如
-- 对既有数据卷重跑本脚本）；新库的真正授权由日志库迁移
-- 20261019000001_usage_logs_app_update_v2 执行。只授五列，audit_logs 不受影响。
DO $$
BEGIN
    IF to_regclass('public.usage_logs') IS NOT NULL THEN
        EXECUTE 'GRANT UPDATE (status, cost, refunded, error_class, error_message) ON usage_logs TO newtg_app';
    END IF;
END
$$;

-- W10-N10/N11：newtg_retention 的表级授权与分区属主（R-W10-01 A）。
-- 同样只在「表已存在」时生效：首次 initdb 早于迁移，真正的授权由日志库
-- 迁移 20261022000000_retention_role 执行；这里是既有数据卷重跑脚本的补授。
DO $$
DECLARE
    part_name   text;
    part_schema text;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'newtg_retention') THEN
        RETURN;
    END IF;
    -- 既有表补 SELECT：审计 DELETE 的 WHERE 引用 created_at/id，PostgreSQL
    -- 要求 SELECT；旧版 20-databases 没有对既有表补授，重跑本脚本即可修复
    -- （I-2 升级路径）。新库的同类授权由日志库迁移执行。
    EXECUTE 'GRANT SELECT ON ALL TABLES IN SCHEMA public TO newtg_retention';
    IF to_regclass('public.audit_logs') IS NOT NULL THEN
        EXECUTE 'GRANT DELETE ON audit_logs TO newtg_retention';
        -- 顺序同日志库迁移：表级 REVOKE UPDATE 会清掉列级授权，必须先收回
        -- 表级 UPDATE 再授 ip 列，否则 90 天 IP 擦除会因权限不足失败。
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
END
$$;
