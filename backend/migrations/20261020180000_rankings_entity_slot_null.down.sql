-- 回滚：旧 CHECK 不允许 NULL，slot 也要恢复 NOT NULL。
-- slot=NULL 的实体只表示「色板已耗尽、没有独立颜色」，注册表是派生数据
-- （下次快照会重新登记），删除这些行不损失原始事实；保留它们会让
-- ALTER COLUMN SET NOT NULL 失败。
DELETE FROM rankings_entities WHERE slot IS NULL;

ALTER TABLE rankings_entities
    DROP CONSTRAINT rankings_entities_slot_chk;

ALTER TABLE rankings_entities
    ADD CONSTRAINT rankings_entities_slot_chk
    CHECK (slot BETWEEN 1 AND 8);

ALTER TABLE rankings_entities
    ALTER COLUMN slot SET NOT NULL;
