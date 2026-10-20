-- D1 —— 回退模型目录与供应商目录（D1-06：无数据迁移，直接 DROP）。
--
-- 顺序：先 model_meta 后 vendors。虽然两表之间没有 FK，但 model_meta.vendor_id
-- 是逻辑引用；按依赖相反顺序删除，避免回退到一半时出现「只有供应商没有档案」
-- 的中间状态被后续操作读到（down 在同一迁移事务内执行，顺序仍是必要文档）。
DROP TABLE IF EXISTS model_meta;
DROP TABLE IF EXISTS vendors;
