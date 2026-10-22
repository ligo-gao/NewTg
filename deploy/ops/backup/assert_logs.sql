-- NewTG 日志库恢复断言（platform-08 §5.3 的 a1/a2/a3，针对 newtg_logs）。
--
-- 用法：
--   psql "$NEWTG_RESTORE_LOGS_DSN" -v ON_ERROR_STOP=1 -f deploy/ops/backup/assert_logs.sql
--
-- 硬失败：
--   - usage_logs / audit_logs / schema_migrations 缺失；
--   - schema_migrations.dirty=true；
--   - usage_logs.status 出现 0..3 之外的值；
--   - 当月 usage_logs 分区不存在（platform-04 §5.2 预建当月+下月）。
--
-- 软告警：
--   - 同一 request_id 出现多行。正常情况下 worker 批量落库按 request_id
--     幂等；恢复快照来自旧代码/旧窗口时可能出现历史重复，演练报告须记录。

\set ON_ERROR_STOP on

DO $$
DECLARE
    missing text;
    bad     bigint;
    dirty_v boolean;
    part    text;
BEGIN
    SELECT string_agg(t, ', ') INTO missing
      FROM (VALUES ('usage_logs'), ('audit_logs'), ('schema_migrations')) AS v(t)
     WHERE to_regclass('public.' || t) IS NULL;
    IF missing IS NOT NULL THEN
        RAISE EXCEPTION 'restore assert logs: missing critical tables: %', missing;
    END IF;

    SELECT COALESCE(bool_or(dirty), false) INTO dirty_v FROM schema_migrations;
    IF dirty_v THEN
        RAISE EXCEPTION 'restore assert logs: schema_migrations.dirty=true';
    END IF;

    SELECT count(*) INTO bad FROM usage_logs WHERE status NOT IN (0, 1, 2, 3);
    IF bad > 0 THEN
        RAISE EXCEPTION 'restore assert logs: % usage_logs rows with invalid status', bad;
    END IF;

    part := 'usage_logs_' || to_char(now() AT TIME ZONE 'UTC', 'YYYYMM');
    IF to_regclass('public.' || part) IS NULL THEN
        RAISE EXCEPTION 'restore assert logs: current-month partition % is missing', part;
    END IF;

    SELECT count(*) INTO bad
      FROM (
            SELECT request_id
              FROM usage_logs
             GROUP BY request_id
            HAVING count(*) > 1
           ) AS dup;
    IF bad > 0 THEN
        RAISE WARNING 'restore assert logs: % request_ids have duplicate usage_logs rows', bad;
    END IF;

    RAISE NOTICE 'restore assert logs: PASS (current partition %)', part;
END
$$;
