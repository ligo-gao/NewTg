-- 回退 M6d Task 1 的任务插件三表。
-- 逆序删除：先删独立的市场源表，再删依赖 task_plugins 的版本行（FK
-- ON DELETE CASCADE 也会随主表删除，但显式顺序让 down 在任何 PG 版本上
-- 都是确定的，不依赖级联路径）。索引随表删除，无需单独 DROP。
DROP TABLE IF EXISTS plugin_marketplace_sources;
DROP TABLE IF EXISTS task_plugin_versions;
DROP TABLE IF EXISTS task_plugins;
