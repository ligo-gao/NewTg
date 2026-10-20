-- ============================================================================
-- A5 公告站内通知投递（modules-08 F-08-04 / §3.9.1 `system.announcement`）
--
-- 设计原文（§3.9.2 末 / §3.9.5b）写「inapp 不落投递记录」；A5 的冻结语义
-- 明确要求公告发布时为每个启用用户生成一条 `channel='inapp'` 的投递记录，
-- 以便用户侧读取与已读状态持久化。本迁移是**有意的契约扩展**，不是笔误：
--   - channel CHECK 扩为 {email, webhook, inapp}；
--   - payload 承载站内通知的渲染输入/结果（title/summary/link_url/body），
--     避免读取端回查公告表时把「发布时的标题」漂移成「当前标题」；
--   - read_at 是站内通知的已读时刻（幂等标记，NULL=未读）。
--
-- 其余约束不动：dedupe_key 仍是唯一幂等锚（A5 构造
-- `announcement:<announcement_id>:<user_id>`）；recipient 仍是 NOT NULL
-- （inapp 写入 `user:<id>` 作为可读占位，真正的收件人身份在
-- recipient_user_id）。
-- ============================================================================

ALTER TABLE notification_deliveries
    DROP CONSTRAINT notification_deliveries_channel_chk;

ALTER TABLE notification_deliveries
    ADD CONSTRAINT notification_deliveries_channel_chk
    CHECK (channel IN ('email', 'webhook', 'inapp'));

ALTER TABLE notification_deliveries
    ADD COLUMN payload jsonb,
    ADD COLUMN read_at timestamptz;

-- payload 只允许对象形态：读取端按固定键取值，数组/标量没有意义。
ALTER TABLE notification_deliveries
    ADD CONSTRAINT notification_deliveries_payload_obj_chk
    CHECK (payload IS NULL OR jsonb_typeof(payload) = 'object');

-- 已读只对站内通知成立；邮件/webhook 投递没有「用户已读」概念。
ALTER TABLE notification_deliveries
    ADD CONSTRAINT notification_deliveries_read_at_chk
    CHECK (read_at IS NULL OR channel = 'inapp');
