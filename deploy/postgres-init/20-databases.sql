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

GRANT CONNECT ON DATABASE :"logs_db" TO newtg_app, newtg_migrate, newtg_backup;

-- 切到日志库：schema 与默认权限按库分别设置。
\connect :"logs_db"

GRANT USAGE ON SCHEMA public TO newtg_app, newtg_backup;
GRANT USAGE, CREATE ON SCHEMA public TO newtg_migrate;
REVOKE CREATE ON SCHEMA public FROM newtg_app, newtg_backup;

-- 日志库内全部表都是日志/审计表：newtg_app 只有 INSERT + SELECT
-- （platform-01 §12.6 / security-09：不授 UPDATE/DELETE；保留清理归独立维护主体）。
ALTER DEFAULT PRIVILEGES FOR ROLE newtg_migrate IN SCHEMA public
    GRANT SELECT, INSERT ON TABLES TO newtg_app;
ALTER DEFAULT PRIVILEGES FOR ROLE newtg_migrate IN SCHEMA public
    GRANT USAGE, SELECT ON SEQUENCES TO newtg_app;
ALTER DEFAULT PRIVILEGES FOR ROLE newtg_migrate IN SCHEMA public
    GRANT SELECT ON TABLES TO newtg_backup;
ALTER DEFAULT PRIVILEGES FOR ROLE newtg_migrate IN SCHEMA public
    GRANT SELECT ON SEQUENCES TO newtg_backup;
