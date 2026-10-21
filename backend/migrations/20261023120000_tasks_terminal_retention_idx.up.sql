-- ============================================================================
-- W10-N9（R-W10-10）——终态任务保留清理的部分索引
--
-- 为什么需要：任务保留 worker 每天按族删除「终态且超过保留期」的行。
-- 没有索引时每批 DELETE 的子查询要在 tasks 全表（历史任务不断增长）上
-- 过滤 family/state/时间并按 id 排序；`tasks_pending_idx` 只覆盖**未终结**
-- 行，对清理无用。部分索引把扫描面压到「可回收的终态行」这一个子集。
--
-- 为什么谓词与删除 SQL 逐字一致（NOT pending）：`private_data` 里的
-- billing_pending/settlement_pending/refund_pending 是 T6b 补偿扫描的唯一
-- 线索，删除 SQL 永不碰这些行。把同样的 NOT 谓词写进索引，让索引大小只
-- 跟随「真正可回收」的集合，查询也能和谓词完全匹配。jsonb `@>` 是
-- immutable，可用于部分索引谓词（与 tasks_billing_pending_idx 同先例）。
--
-- 为什么列是 (family, id) 而不是 (family, finish_time)：删除批按
-- `ORDER BY id ASC LIMIT` 取最旧的行；以 id 为索引第二列可直接顺序扫描，
-- 不需要排序临时集。时间条件（COALESCE(retention_deadline, finish_time…)
-- < now）作为索引过滤后的残余谓词执行——每批只触碰有限行。
--
-- 为什么谓词里没有「未被 task_batches_items 引用」：部分索引谓词只能引用
-- 本表列，跨表 NOT EXISTS 不能进谓词。批次引用保护在删除 SQL 里作为残余
-- 过滤执行（task_batches_items_task_idx 支撑反查）。
-- ============================================================================

CREATE INDEX IF NOT EXISTS tasks_terminal_retention_idx
    ON tasks (family, id)
    WHERE state IN ('success', 'failure')
      AND NOT (private_data @> '{"billing_pending": true}'::jsonb
            OR private_data @> '{"settlement_pending": true}'::jsonb
            OR private_data @> '{"refund_pending": true}'::jsonb);
