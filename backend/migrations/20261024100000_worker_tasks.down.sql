-- W17-2 回滚：删除 worker 任务框架三表。
--
-- 三张表只承载调度状态与运行历史，不参与计费或请求路径；删除后 worker
-- 退化为「没有任务框架」的旧行为（既有循环照旧运行，只是不再有租约目录
-- 与管理面 API）。运行历史随之丢弃——这是旧二进制的已知语义。
DROP TABLE IF EXISTS worker_task_runs;
DROP TABLE IF EXISTS worker_task_locks;
DROP TABLE IF EXISTS worker_tasks;
