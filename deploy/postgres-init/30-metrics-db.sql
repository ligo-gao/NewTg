-- NewTG 独立指标库 newtg_metrics（PERF_METRICS_DSN）
--
-- 性能指标不落主库/日志库：采集端（gateway）与聚合端（worker）只通过
-- PERF_METRICS_DSN 访问本库。与 20-databases.sql 同口径：显式授权，
-- PUBLIC 只保留默认的 CONNECT 收回策略。
--
-- 该脚本只在空数据卷的 initdb 阶段执行；既有部署按相同语句手工建库即可。

\set ON_ERROR_STOP on
\getenv metrics_db POSTGRES_METRICS_DB

SELECT (:'metrics_db' = '') AS db_empty \gset
\if :db_empty
\echo 'ERROR: POSTGRES_METRICS_DB is empty'
DO $$ BEGIN RAISE EXCEPTION 'POSTGRES_METRICS_DB is empty'; END $$;
\endif

-- CREATE DATABASE 不能位于事务块内；\gexec 由 psql 自动提交。
SELECT format('CREATE DATABASE %I', :'metrics_db')
WHERE NOT EXISTS (SELECT 1 FROM pg_database WHERE datname = :'metrics_db')\gexec

REVOKE CONNECT ON DATABASE :"metrics_db" FROM PUBLIC;
GRANT CONNECT ON DATABASE :"metrics_db" TO newtg_app, newtg_migrate, newtg_backup, newtg_retention;

\connect :"metrics_db"

GRANT USAGE ON SCHEMA public TO newtg_app, newtg_backup, newtg_retention;
GRANT USAGE, CREATE ON SCHEMA public TO newtg_migrate;
REVOKE CREATE ON SCHEMA public FROM public;

-- 迁移 20261024110000_model_perf_buckets 会在建表后按属主权限授予
-- newtg_app DML、newtg_backup/retention SELECT；这里不重复授权。
