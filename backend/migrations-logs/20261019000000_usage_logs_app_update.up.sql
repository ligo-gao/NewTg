-- R-25 / billing-06 §5.3 + R-56 / R-59 的日志库部署修复：
--
-- 1) newtg_app 需要 **列级** UPDATE (status, cost, refunded) ON usage_logs：
--    worker 兜底退款要把已有行单向收敛为 refunded，并在保留 ChargeIncrement
--    消耗时更新 cost（R-59）。只授这三列，不授表级 UPDATE，更不授
--    audit_logs（它必须保持 append-only）。
-- 2) R-56：运行期 worker 以 newtg_app 身份运行，没有 schema CREATE、也不是
--    usage_logs 的属主，无法执行分区 DDL。因此由**本迁移（migrate 凭据）**
--    预建 12 个月窗口：当月 + 下 11 个月。名称与区间口径与 init 迁移完全
--    一致（usage_logs_YYYYMM，[月初, 次月初)，UTC），幂等、不建 DEFAULT。
--
-- 【为什么授权放在迁移而不是只放 postgres-init】docker-entrypoint-initdb.d
-- 在 initdb 阶段执行，早于迁移；首次初始化时 usage_logs 尚不存在，表级/
-- 列级授权必须等建表之后执行。postgres-init/20-databases.sql 保留了
-- 「表已存在时」的条件补授（对既有数据卷重跑脚本的场景）。
--
-- 【角色守卫】测试/CI 的数据库可能没有 newtg_app（只有部署环境的
-- postgres-init 会创建角色）。缺角色时跳过授权，避免迁移在无 app 角色的
-- 环境里失败；部署环境角色必定先于 migrate 存在。

-- ==== 1. usage_logs 的列级 UPDATE 例外（R-25/R-59）====
-- 只允许改这三列：status（收敛）、cost（保留消耗）、refunded（退回金额）。
-- 其余列（request_id/user_id/model/created_at 等）不可改写，保持可对账。
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'newtg_app') THEN
        EXECUTE 'GRANT UPDATE (status, cost, refunded) ON usage_logs TO newtg_app';
    END IF;
END
$$;

-- ==== 2. 12 个月分区预建（R-56）====
-- 当月 + 下 11 个月。date_trunc 的 UTC 口径与 init 迁移一致；循环变量 i
-- 从 0 到 11，使用 timestamptz + interval 推进，避免手工拼接月份。
DO $$
DECLARE
    month_start timestamptz := date_trunc('month', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC';
    from_ts     timestamptz;
    to_ts       timestamptz;
BEGIN
    FOR i IN 0..11 LOOP
        from_ts := month_start + (i || ' months')::interval;
        to_ts   := month_start + ((i + 1) || ' months')::interval;
        EXECUTE format(
            'CREATE TABLE IF NOT EXISTS %I PARTITION OF usage_logs FOR VALUES FROM (%L) TO (%L)',
            'usage_logs_' || to_char(from_ts AT TIME ZONE 'UTC', 'YYYYMM'),
            from_ts, to_ts);
    END LOOP;
END
$$;
