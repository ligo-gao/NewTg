-- audit_logs 保留期清理索引的逆操作（R-10）。
-- 仅删除本迁移新增的索引；audit_logs 表与 init 迁移的索引保持不变。
DROP INDEX IF EXISTS audit_logs_created_idx;
