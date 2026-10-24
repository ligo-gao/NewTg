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
-- usage_logs 是分区表：父表上的 ALTER TABLE ... ADD COLUMN 会自动递归到
-- 所有分区（含未来 PARTITION OF 创建的分区），因此这里不写逐分区 DDL。
-- ============================================================================

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
