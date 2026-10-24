-- ============================================================================
-- 上游用量对账：usage_logs 维度列（上游用量对账设计 §4.6）
--
-- 对账需要把每条消费日志归到 (上游账号, 上游模型, 用量来源, 计价语义)。
-- 这些列在**写日志时固化**：日志库与主库不能 JOIN，且渠道以后可能改挂账号，
-- 历史账必须按当时的归属计算。
--
-- usage_source / usage_semantic 可空：迁移前的历史行没有记录，未知就是
-- NULL，不得用 0 冒充（§4.6）。reasoning_tokens / attempts 用 NOT NULL
-- DEFAULT 给出可解释的基线（0 / 1）。
--
-- usage_logs 是分区表。父表上的 ALTER TABLE ... ADD COLUMN 会递归到所有
-- 分区，而 PostgreSQL 要求执行者**同时拥有父表和每一个分区**；但
-- 20261022000000_retention_role 已把分区属主转给 newtg_retention（保留任务
-- 需要 DROP 它们），迁移角色 newtg_migrate 只是其成员、并不拥有分区，
-- 于是递归 ADD COLUMN 会以 42501 "must be owner of table usage_logs_YYYYMM"
-- 失败。因此这里先把分区属主临时改成当前迁移角色，做完 DDL 再逐个还原，
-- 保证保留任务的属主前提不被破坏。迁移角色必须是这些原属主角色的成员
-- （deploy/postgres-init/10-roles.sql 里 newtg_migrate 属于 newtg_retention），
-- 否则还原属主会失败——失败比静默改错属主好。
--
-- 约束不进分区（CHECK 只加在父表），CREATE INDEX 也不要求拥有分区，
-- 所以只有 ADD COLUMN 需要这段属主交接。
-- ============================================================================

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

    ALTER TABLE usage_logs ADD COLUMN usage_source smallint;
    ALTER TABLE usage_logs ADD COLUMN usage_semantic smallint;
    ALTER TABLE usage_logs ADD COLUMN reasoning_tokens integer NOT NULL DEFAULT 0;
    ALTER TABLE usage_logs ADD COLUMN upstream_model varchar(128);
    ALTER TABLE usage_logs ADD COLUMN upstream_account_id bigint;
    ALTER TABLE usage_logs ADD COLUMN response_id varchar(128);
    ALTER TABLE usage_logs ADD COLUMN attempts integer NOT NULL DEFAULT 1;
    ALTER TABLE usage_logs ADD CONSTRAINT usage_logs_usage_source_chk
        CHECK (usage_source IS NULL OR usage_source BETWEEN 0 AND 3);
    ALTER TABLE usage_logs ADD CONSTRAINT usage_logs_usage_semantic_chk
        CHECK (usage_semantic IS NULL OR usage_semantic IN (0, 1));

    IF part_rels IS NOT NULL THEN
        FOR i IN 1 .. array_length(part_rels, 1) LOOP
            IF part_owners[i] <> current_user THEN
                EXECUTE format('ALTER TABLE %s OWNER TO %I', part_rels[i], part_owners[i]);
            END IF;
        END LOOP;
    END IF;
END
$migration$;
