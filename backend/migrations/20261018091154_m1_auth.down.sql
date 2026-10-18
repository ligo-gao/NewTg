-- ============================================================================
-- M1 认证迁移的逆操作（20261018091154_m1_auth.up.sql）。
-- 逆序：先 DROP 六张新表（无 FK 依赖，CASCADE 仅为幂等清理索引），
-- 再按补列的反序 DROP api_tokens 两列与 users.must_change_password。
-- 数据全丢——回滚前须确认认证族数据已导出（platform-04 §7.4 备份口径）。
-- ============================================================================

-- ==== 新表（逆序 DROP）====
DROP TABLE IF EXISTS checkins CASCADE;
DROP TABLE IF EXISTS custom_oauth_providers CASCADE;
DROP TABLE IF EXISTS user_passkeys CASCADE;
DROP TABLE IF EXISTS user_oauth_bindings CASCADE;
DROP TABLE IF EXISTS login_encryption_keys CASCADE;
DROP TABLE IF EXISTS login_sessions CASCADE;

-- ==== 补列（逆序 DROP；列级 CHECK 随列一并删除）====
ALTER TABLE api_tokens DROP COLUMN IF EXISTS cross_group_retry;
ALTER TABLE api_tokens DROP COLUMN IF EXISTS auto_groups;
ALTER TABLE users DROP COLUMN IF EXISTS must_change_password;
