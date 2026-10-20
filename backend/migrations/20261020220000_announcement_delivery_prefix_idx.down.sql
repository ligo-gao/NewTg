-- 回退顺序与 up 相反：先删索引，再撤销 payload 回填。
-- 旧版本读取面不消费 announcement_id，删除该键对旧版本无影响；dedupe_key
-- 仍是删除级联的幂等锚，因此 down 后公告删除逻辑照常工作（只是走扫描）。
DROP INDEX IF EXISTS notification_deliveries_dedupe_prefix_idx;

UPDATE notification_deliveries
SET payload = payload - 'announcement_id'
WHERE channel = 'inapp'
  AND dedupe_key LIKE 'announcement:%'
  AND payload IS NOT NULL
  AND payload ? 'announcement_id';
