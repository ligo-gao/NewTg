-- ============================================================================
-- W8 / MJ-A0 —— tasks.family 扩为 {plugin, mj}
--
-- 依据：relay-05（绘图任务族）§字段表「与 06 共用同一张 tasks 表」、
--       relay-06:64、契约 §2.5（family 是 Action 解释权的判别键）、
--       R-MJ-03 方案 A（不建 mj_tasks，也不为 MJ 增设专属列）。
--
-- 为什么只改 CHECK 而不动列：两族共用 tasks 是冻结裁决；MJ 专属字段
-- （code/prompt/image_url/buttons 等）落 data / properties（jsonb），
-- 共享表不为单族长出 10+ 个永久 NULL 列（R-MJ-03）。
--
-- 为什么仍写 = ANY (ARRAY[...])：store.EnumChecks 的反向对账按这一形状
-- 解析（见 20261020130000_tasks.up.sql 的同类说明）；PG 会把单元素 IN
-- 规范化掉，导致枚举对账失配。
-- ============================================================================

ALTER TABLE tasks DROP CONSTRAINT IF EXISTS tasks_family_chk;
ALTER TABLE tasks ADD CONSTRAINT tasks_family_chk
    CHECK (family = ANY (ARRAY['plugin'::text, 'mj'::text]));

-- 两族的轮询路径独立（R-MJ-04）：插件 poller 固定领取 family='plugin'，
-- MJ poller 固定领取 family='mj'。没有这个索引时每次领取都要在
-- tasks_pending_idx 上再过滤 family，family 的选择性随两族任务量漂移。
--
-- 为什么列是 (family, id) 而不是设计建议的 (family, state, next_poll_at)：
-- tasks 没有 next_poll_at 列（调度是 15s 扫描 + id 升序，见 repo.ListPendingByFamily），
-- 「非终态」由部分谓词承担，state 再进键列只会让索引变大而没有额外选择性；
-- id 进键列正好支撑领取查询的 ORDER BY id。
CREATE INDEX IF NOT EXISTS tasks_family_pending_idx
    ON tasks (family, id) WHERE state NOT IN ('success', 'failure');
