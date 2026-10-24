-- 回滚 usage_logs 对账维度列（20261028090500_usage_logs_reconcile_dimensions）。
--
-- 顺序与 up 相反：先删约束，再按相反顺序删列。分区表的父表 DROP COLUMN
-- 同样会递归到所有分区，也同样要求执行者拥有每一个分区（分区属主是
-- newtg_retention），因此与 up 一样先做属主交接、结束后还原。
-- 列内数据随列丢弃（对账维度可由日志重放/重导重建）。
DO $migration$
DECLARE
    part_rels   regclass[];
    part_owners text[];
    i           integer;
BEGIN
    SELECT array_agg(c.oid::regclass ORDER BY c.relname),
           array_agg(pg_get_userbyid(c.relowner) ORDER BY c.relname)
      INTO part_rels, part_owners
      FROM pg_inherits inh
      JOIN pg_class c ON c.oid = inh.inhrelid
     WHERE inh.inhparent = 'usage_logs'::regclass;

    IF part_rels IS NOT NULL THEN
        FOR i IN 1 .. array_length(part_rels, 1) LOOP
            IF part_owners[i] <> current_user THEN
                EXECUTE format('ALTER TABLE %s OWNER TO %I', part_rels[i], current_user);
            END IF;
        END LOOP;
    END IF;

    ALTER TABLE usage_logs DROP CONSTRAINT IF EXISTS usage_logs_usage_semantic_chk;
    ALTER TABLE usage_logs DROP CONSTRAINT IF EXISTS usage_logs_usage_source_chk;
    ALTER TABLE usage_logs DROP COLUMN IF EXISTS attempts;
    ALTER TABLE usage_logs DROP COLUMN IF EXISTS response_id;
    ALTER TABLE usage_logs DROP COLUMN IF EXISTS upstream_account_id;
    ALTER TABLE usage_logs DROP COLUMN IF EXISTS upstream_model;
    ALTER TABLE usage_logs DROP COLUMN IF EXISTS reasoning_tokens;
    ALTER TABLE usage_logs DROP COLUMN IF EXISTS usage_semantic;
    ALTER TABLE usage_logs DROP COLUMN IF EXISTS usage_source;

    IF part_rels IS NOT NULL THEN
        FOR i IN 1 .. array_length(part_rels, 1) LOOP
            IF part_owners[i] <> current_user THEN
                EXECUTE format('ALTER TABLE %s OWNER TO %I', part_rels[i], part_owners[i]);
            END IF;
        END LOOP;
    END IF;
END
$migration$;
