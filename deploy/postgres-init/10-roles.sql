-- NewTG PostgreSQL 最小权限角色（platform-03 §2.5 / security-08 §2.4）
--
-- 执行时机：官方 postgres:16 镜像的 docker-entrypoint-initdb.d，仅在空数据卷的
-- initdb 阶段执行一次；由引导超级用户（POSTGRES_USER）连接 POSTGRES_DB 执行。
-- 口令来源：容器环境变量，经 psql \getenv（PostgreSQL 15+）读取——脚本内不写明文。
--
-- 四个独立 LOGIN 角色（口令互不派生、与引导超级用户口令也无关系）：
--   newtg_app     ：仅 DML（SELECT/INSERT/UPDATE/DELETE），无 DDL；gateway/worker。
--   newtg_migrate ：仅 DDL（迁移）；一次性 migrate 服务。
--   newtg_backup  ：只读 + pg_dump；profile ops 的备份服务。
--   newtg_retention：保留/维护主体（W10-N10/N11，R-W10-01 A）：日志库 SELECT +
--                    分区 DROP（子分区属主）+ 审计 DELETE / 列级 UPDATE(ip)；
--                    不授予主库，也**不得**复用 newtg_app 的 DSN/口令。

\set ON_ERROR_STOP on

\getenv app_password       POSTGRES_APP_PASSWORD
\getenv migrate_password   POSTGRES_MIGRATE_PASSWORD
\getenv backup_password    POSTGRES_BACKUP_PASSWORD
\getenv retention_password POSTGRES_RETENTION_PASSWORD
\getenv db_name            POSTGRES_DB

-- fail-fast：口令为空则中止 initdb（compose 侧已有 :? 守卫，此处为兜底）。
-- 注意：psql 的 \if 不接受 `:'var' = ''` 这类表达式作为直接参数，须先用 \gset
-- 把比较结果存成变量；且 psql 16 的 \quit 不支持退出码参数（会被忽略并退 0），
-- 故用 ON_ERROR_STOP + RAISE EXCEPTION 产生非 0 退出以真正中止 entrypoint。
SELECT (:'app_password' = '') AS pw_empty \gset
\if :pw_empty
\echo 'ERROR: POSTGRES_APP_PASSWORD is empty'
DO $$ BEGIN RAISE EXCEPTION 'POSTGRES_APP_PASSWORD is empty'; END $$;
\endif
SELECT (:'migrate_password' = '') AS pw_empty \gset
\if :pw_empty
\echo 'ERROR: POSTGRES_MIGRATE_PASSWORD is empty'
DO $$ BEGIN RAISE EXCEPTION 'POSTGRES_MIGRATE_PASSWORD is empty'; END $$;
\endif
SELECT (:'backup_password' = '') AS pw_empty \gset
\if :pw_empty
\echo 'ERROR: POSTGRES_BACKUP_PASSWORD is empty'
DO $$ BEGIN RAISE EXCEPTION 'POSTGRES_BACKUP_PASSWORD is empty'; END $$;
\endif
SELECT (:'retention_password' = '') AS pw_empty \gset
\if :pw_empty
\echo 'ERROR: POSTGRES_RETENTION_PASSWORD is empty'
DO $$ BEGIN RAISE EXCEPTION 'POSTGRES_RETENTION_PASSWORD is empty'; END $$;
\endif

-- 建角色（幂等；initdb 只会跑一次，幂等仅为人工重放的容错）。
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'newtg_app') THEN
        CREATE ROLE newtg_app LOGIN;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'newtg_migrate') THEN
        CREATE ROLE newtg_migrate LOGIN;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'newtg_backup') THEN
        CREATE ROLE newtg_backup LOGIN;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'newtg_retention') THEN
        CREATE ROLE newtg_retention LOGIN;
    END IF;
END
$$;

ALTER ROLE newtg_app       PASSWORD :'app_password';
ALTER ROLE newtg_migrate   PASSWORD :'migrate_password';
ALTER ROLE newtg_backup    PASSWORD :'backup_password';
ALTER ROLE newtg_retention PASSWORD :'retention_password';

-- migrate 需要能 SET ROLE 到 retention，才能执行
-- `ALTER TABLE usage_logs_YYYYMM OWNER TO newtg_retention`（PostgreSQL 要求
-- 「改属主」的执行者是新属主的成员）。migrate 本来就是唯一 DDL 入口、
-- 拥有全部对象，这条成员关系**不扩大**它的实际能力；反过来不授权
-- （retention 不是 migrate 的成员，无法执行 DDL）。
GRANT newtg_retention TO newtg_migrate;

-- ---- 主库（POSTGRES_DB）上的权限 -----------------------------------------
-- 连接权：三个应用角色都只连需要的库；newtg_retention **不连主库**
-- （保留任务只碰日志库的 usage_logs / audit_logs，R-W10-01）。
-- 先收回 PUBLIC 的默认 CONNECT：PostgreSQL 默认把 CONNECT 授给所有角色，
-- 只写「不 GRANT 给 retention」并不能阻止它连主库。收回后仅显式列出的
-- 角色可连（引导超级用户不受影响）。
REVOKE CONNECT ON DATABASE :"db_name" FROM PUBLIC;
GRANT CONNECT ON DATABASE :"db_name" TO newtg_app, newtg_migrate, newtg_backup;

-- schema public：app/backup 仅使用，migrate 可建对象（唯一 DDL 入口）。
GRANT USAGE ON SCHEMA public TO newtg_app, newtg_backup;
GRANT USAGE, CREATE ON SCHEMA public TO newtg_migrate;
REVOKE CREATE ON SCHEMA public FROM newtg_app, newtg_backup;

-- 迁移后新建的表/序列：newtg_migrate 是创建者，按角色预置默认权限。
-- （主库业务表：app 常规 DML；backup 只读。）
ALTER DEFAULT PRIVILEGES FOR ROLE newtg_migrate IN SCHEMA public
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO newtg_app;
ALTER DEFAULT PRIVILEGES FOR ROLE newtg_migrate IN SCHEMA public
    GRANT USAGE, SELECT ON SEQUENCES TO newtg_app;
ALTER DEFAULT PRIVILEGES FOR ROLE newtg_migrate IN SCHEMA public
    GRANT SELECT ON TABLES TO newtg_backup;
ALTER DEFAULT PRIVILEGES FOR ROLE newtg_migrate IN SCHEMA public
    GRANT SELECT ON SEQUENCES TO newtg_backup;
