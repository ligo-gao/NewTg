-- 回滚顺序与依赖相反：先删关联表（引用 tasks 与 task_batches），再删父表。
-- 批次与子任务的关联是可重建的聚合视图；对应的 tasks 行不受影响，仍可通过
-- /v1/tasks 台账访问。
DROP TABLE IF EXISTS task_batches_items;
DROP TABLE IF EXISTS task_batches;
