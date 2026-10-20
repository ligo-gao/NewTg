-- ============================================================================
-- A5 残余补批：公告站内投递的读取失效与级联删除索引（G-DOM-37 ⑧⑨）
--
-- 1) payload 回填 announcement_id。
--    读取面（GET /api/notifications）按 payload->>'announcement_id' 关联
--    announcements，只有仍生效的公告投递才返回；A5 首版写入的 payload 不含
--    该键（归因只藏在 dedupe_key 里）。历史行从冻结的 dedupe 前缀
--    `announcement:<uuid>:<user_id>` 提取 uuid 回填，保证升级后这些投递
--    也会随公告下线/过期/删除失效，而不是被当成「无关联的普通投递」。
--    payload 是 JSONB 对象（有 CHECK 约束），用 || 合并不会破坏其它键。
--
-- 2) dedupe_key 的 text_pattern_ops 索引。
--    公告删除（announcement_repo.Delete）在同一事务里按
--    `dedupe_key LIKE 'announcement:<id>:%'` 清理投递行。dedupe_key 上只有
--    UNIQUE 索引，其默认 ops 依赖数据库 collation，普通 LIKE 前缀匹配无法
--    使用它；text_pattern_ops 让前缀 LIKE 走索引范围扫描，避免大表全扫。
--
-- 回填是幂等的（只处理 announcement_id 缺失的行），可安全重跑。
-- ============================================================================

UPDATE notification_deliveries
SET payload = payload || jsonb_build_object(
        'announcement_id', split_part(dedupe_key, ':', 2))
WHERE channel = 'inapp'
  AND dedupe_key LIKE 'announcement:%'
  AND payload IS NOT NULL
  AND payload->>'announcement_id' IS NULL;

CREATE INDEX IF NOT EXISTS notification_deliveries_dedupe_prefix_idx
    ON notification_deliveries (dedupe_key text_pattern_ops);
