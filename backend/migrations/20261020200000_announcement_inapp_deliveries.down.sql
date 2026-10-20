-- 回退前先删除 inapp 投递行：旧 CHECK 不包含 'inapp'，否则约束无法重建。
DELETE FROM notification_deliveries WHERE channel = 'inapp';

ALTER TABLE notification_deliveries
    DROP CONSTRAINT notification_deliveries_read_at_chk;

ALTER TABLE notification_deliveries
    DROP CONSTRAINT notification_deliveries_payload_obj_chk;

ALTER TABLE notification_deliveries
    DROP COLUMN read_at,
    DROP COLUMN payload;

ALTER TABLE notification_deliveries
    DROP CONSTRAINT notification_deliveries_channel_chk;

ALTER TABLE notification_deliveries
    ADD CONSTRAINT notification_deliveries_channel_chk
    CHECK (channel IN ('email', 'webhook'));
