-- W10-N9 回滚：删除终态任务保留清理的部分索引。
-- 索引是纯性能制品，删除不改变任何行或资金语义；worker 的删除 SQL
-- 仍可正确执行（只是退化为更大范围的扫描）。
DROP INDEX IF EXISTS tasks_terminal_retention_idx;
