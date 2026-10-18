-- ============================================================================
-- 系统分组种子的逆操作（20261018093626_seed_user_groups.up.sql）。
--
-- 只删除本迁移插入的系统组（按 name + is_system 双条件，避免误删管理员
-- 后来把同名行改成非系统组的场景）。
--
-- 注意：user_groups.name 是 users."group" / api_tokens."group" /
-- group_pricing."group" 的 FK 目标（ON DELETE RESTRICT）。若仍有引用，
-- DELETE 会被数据库拒绝——回滚前须先把引用方改到其它分组
--（users.group 回落 'default'、令牌/定价改为具体分组），不得改用 CASCADE
-- 静默删除引用数据。先删 auto、后删 default：default 是其它分组的回落目标。
-- ============================================================================

DELETE FROM user_groups WHERE name = 'auto' AND is_system = true;
DELETE FROM user_groups WHERE name = 'default' AND is_system = true;
