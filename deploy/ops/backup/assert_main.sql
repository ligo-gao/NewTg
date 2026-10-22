-- NewTG 主库恢复断言（platform-08 §5.3 的 a1/a3/a4/a5/a6）。
--
-- 用法：
--   psql "$NEWTG_RESTORE_DSN" -v ON_ERROR_STOP=1 -f deploy/ops/backup/assert_main.sql
--
-- 硬失败（RAISE EXCEPTION → psql 非零退出）：
--   - 关键表缺失；
--   - schema_migrations 不存在或 dirty=true；
--   - 存在负余额用户；
--   - 任一用户 users.balance != Σ wallet_transactions.amount。
--
-- 软告警（RAISE WARNING，不改变退出码）：
--   - tasks.private_data 中仍有 settlement_pending=true 的行。恢复快照可能
--     包含在飞任务，不能把“有补偿标记”直接判为恢复失败；演练报告须记录数量。

\set ON_ERROR_STOP on

DO $$
DECLARE
    missing text;
    bad     bigint;
    dirty_v boolean;
BEGIN
    SELECT string_agg(t, ', ') INTO missing
      FROM (VALUES
              ('users'),
              ('wallet_transactions'),
              ('request_registry'),
              ('schema_migrations')
           ) AS v(t)
     WHERE to_regclass('public.' || t) IS NULL;
    IF missing IS NOT NULL THEN
        RAISE EXCEPTION 'restore assert main: missing critical tables: %', missing;
    END IF;

    SELECT COALESCE(bool_or(dirty), false) INTO dirty_v FROM schema_migrations;
    IF dirty_v THEN
        RAISE EXCEPTION 'restore assert main: schema_migrations.dirty=true';
    END IF;

    SELECT count(*) INTO bad FROM users WHERE balance < 0;
    IF bad > 0 THEN
        RAISE EXCEPTION 'restore assert main: % users with negative balance', bad;
    END IF;

    -- 核心资金不变量（platform-08 §5.3 断言 4）：
    -- 逐用户 users.balance == Σ wallet_transactions.amount（全量、有符号）。
    SELECT count(*) INTO bad
      FROM (
            SELECT u.id
              FROM users u
              LEFT JOIN (
                    SELECT user_id, sum(amount) AS total
                      FROM wallet_transactions
                     GROUP BY user_id
              ) w ON w.user_id = u.id
             WHERE u.balance <> COALESCE(w.total, 0)
           ) AS mismatch;
    IF bad > 0 THEN
        RAISE EXCEPTION
          'restore assert main: % users whose balance != sum(wallet_transactions.amount)', bad;
    END IF;

    IF to_regclass('public.tasks') IS NOT NULL THEN
        SELECT count(*) INTO bad
          FROM tasks
         WHERE private_data @> '{"settlement_pending": true}'::jsonb;
        IF bad > 0 THEN
            RAISE WARNING 'restore assert main: % tasks still flagged settlement_pending', bad;
        END IF;
    END IF;

    RAISE NOTICE 'restore assert main: PASS';
END
$$;
