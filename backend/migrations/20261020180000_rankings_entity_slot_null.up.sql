-- ============================================================================
-- rankings_entities.slot 允许 NULL（D6a 复核裁决，2026-10-20）
--
-- 背景：色板只有 8 槽且实体跨 period 永久累积。旧实现槽位耗尽后用回绕
-- (n+i)%8+1 复用槽位，导致两个实体永久同色，违反「颜色跟随实体」的唯一性。
-- 裁决：耗尽后新实体登记 slot=NULL（不复用、不重排），公开面不分配独立颜色。
--
-- CHECK 改为「NULL 或 1–8」：显式写出 IS NULL 只是让意图可读；PostgreSQL
-- 的 CHECK 本身对 NULL 结果为 unknown 也视为通过。
-- ============================================================================
ALTER TABLE rankings_entities
    ALTER COLUMN slot DROP NOT NULL;

ALTER TABLE rankings_entities
    DROP CONSTRAINT rankings_entities_slot_chk;

ALTER TABLE rankings_entities
    ADD CONSTRAINT rankings_entities_slot_chk
    CHECK (slot IS NULL OR (slot BETWEEN 1 AND 8));
