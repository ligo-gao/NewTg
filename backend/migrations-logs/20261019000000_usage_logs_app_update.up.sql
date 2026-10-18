-- R-25 / billing-06 §5.3：worker 兜底退款需要把已有的 usage_logs 行**单向收敛**
-- 为 refunded（status=3、cost=0）。newtg_app 在日志库默认只有 SELECT/INSERT
-- （platform-01 §12.6），故这里只对 usage_logs 补授 UPDATE。
--
-- 【为什么放在迁移而不是只放 postgres-init】docker-entrypoint-initdb.d 在
-- initdb 阶段执行，早于迁移；首次初始化时 usage_logs 尚不存在，表级授权
-- 必须等建表之后执行。postgres-init/20-databases.sql 保留了「表已存在时」
-- 的条件补授（对既有数据卷重跑脚本的场景）。
--
-- 【角色守卫】测试/CI 的数据库可能没有 newtg_app（只有部署环境的
-- postgres-init 会创建角色）。缺角色时跳过，避免迁移在无 app 角色的环境
-- 里失败；部署环境角色必定先于 migrate 存在。
--
-- 【不做全表族授权】不授 audit_logs：它必须保持 append-only。
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'newtg_app') THEN
        EXECUTE 'GRANT UPDATE ON usage_logs TO newtg_app';
    END IF;
END
$$;
