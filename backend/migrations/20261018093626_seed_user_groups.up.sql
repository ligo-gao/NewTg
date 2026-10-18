-- ============================================================================
-- 系统分组种子（M1 部署级补丁）
--
-- 为什么需要：`users."group"` 与 `api_tokens."group"` 都有
-- FK → user_groups(name)（init 迁移的 users_group_fk / api_tokens_group_fk，
-- ON DELETE RESTRICT）。新库若没有 'default' 行，注册（users.group 默认
-- 'default'）与管理员建用户都会直接违反外键，Task 6 集成测试此前只能手工 seed。
--
-- 只播种规范要求存在的系统组，逐条注明出处：
--   1) `default` —— platform-01 §1（users.group DEFAULT 'default'）、§3
--      （「default 内置不可删改名」）；modules-07 F-07-10（系统内置组）。
--   2) `auto`    —— modules-02 §3.10 字段定义：令牌 group 取值
--      ∈ 用户可用分组 ∪ {`auto`}；而 api_tokens.group 有 FK →
--      user_groups(name)，故 'auto' 必须是注册表内的保留行，否则
--      group=auto 的令牌无法落库。user_selectable=false + status=2 使其
--      不出现在任何可选分组候选（modules-02 §3.10 候选集明确排除 auto；
--      platform-01 §3 停用组不出现在下游候选）。
--
-- ON CONFLICT (name) DO NOTHING：已有同名行时不覆盖（管理员可能已自建），
-- 使迁移在既有库上可重复执行且不破坏数据。
-- ============================================================================

INSERT INTO user_groups (name, description, ratio, user_selectable, is_system, status)
VALUES ('default', '默认分组', 100, true, true, 1)
ON CONFLICT (name) DO NOTHING;

INSERT INTO user_groups (name, description, ratio, user_selectable, is_system, status)
VALUES ('auto', '自动分组路由（保留字，不对用户展示）', 100, false, true, 2)
ON CONFLICT (name) DO NOTHING;
