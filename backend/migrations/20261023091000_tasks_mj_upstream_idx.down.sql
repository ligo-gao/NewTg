-- 回退 W8/MJ-a2 的 MJ 归属查询索引。
--
-- 只删索引，不动数据：索引是纯读优化，删除不会让已有 mj 行失效。
DROP INDEX IF EXISTS tasks_mj_upstream_idx;
DROP INDEX IF EXISTS tasks_mj_user_upstream_idx;
