-- 回退渠道任务插件绑定列（M6d Task 7a）。
-- 先删索引/约束再删列：列删除本身会级联移除依赖对象，但显式顺序让 down
-- 在任何 PG 版本上都确定，也便于失败时定位具体对象。
DROP INDEX IF EXISTS channels_task_extend_plugin_keys_gin;
DROP INDEX IF EXISTS channels_task_plugin_key_idx;

ALTER TABLE channels DROP CONSTRAINT IF EXISTS channels_task_extend_plugin_keys_chk;
ALTER TABLE channels DROP CONSTRAINT IF EXISTS channels_task_plugin_key_chk;

ALTER TABLE channels
    DROP COLUMN IF EXISTS task_extend_plugin_keys,
    DROP COLUMN IF EXISTS task_plugin_key;
