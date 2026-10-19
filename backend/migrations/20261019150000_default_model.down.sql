-- 回退 M6c Task 13 的用户默认模型列。先删 CHECK 再删列：约束依赖该列，
-- 依赖顺序反过来会在部分 PG 版本上把 DROP COLUMN 变成级联删除的歧义路径。
ALTER TABLE users DROP CONSTRAINT IF EXISTS users_default_model_len_chk;
ALTER TABLE users DROP COLUMN IF EXISTS default_model;
