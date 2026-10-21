-- ============================================================================
-- W8 / MJ-A0 回滚：把 tasks_family_chk 恢复为单值 {plugin}。
--
-- 【破坏性】恢复单值 CHECK 之前必须先清理 family='mj' 的行，否则
-- ADD CONSTRAINT 会因存量行违约而失败。这里刻意选择直接 DELETE：
--
--   1. 回滚意味着二进制回到不认识 MJ 的版本；旧插件 poller 会把这些行当作
--      插件任务领取，累计 20 次 plugin_unavailable 后判 FAILURE 并退款
--      （R-MJ-04）——留着它们比删除更危险（可能在 MJ 上游已成功时白送）。
--   2. 生产回滚必须先备份 tasks WHERE family='mj'（部署流程要求）；
--      本迁移保证「一次性验证库」与「已确认丢弃 MJ 行」的环境可以安全回退。
--
-- 若把 DELETE 去掉，down 会在 ADD CONSTRAINT 处失败——这是有意的保护，
-- 不要改成 NOT VALID 来绕过：那会留下违反新 CHECK 的行。
-- ============================================================================

DELETE FROM tasks WHERE family = 'mj';

DROP INDEX IF EXISTS tasks_family_pending_idx;

ALTER TABLE tasks DROP CONSTRAINT IF EXISTS tasks_family_chk;
ALTER TABLE tasks ADD CONSTRAINT tasks_family_chk
    CHECK (family = ANY (ARRAY['plugin'::text]));
