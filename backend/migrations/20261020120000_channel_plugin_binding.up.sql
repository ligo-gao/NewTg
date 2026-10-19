-- ============================================================================
-- M6d Task 7a —— 渠道 → 任务插件绑定列（relay-06 §1.3 / 基线-08 §1.1）
--
-- 两类绑定形态（互斥，应用层校验为主、DB 只兜底形态）：
--   类型 61（task_plugin，任务插件渠道）：单绑 task_plugin_key；
--   类型 60（newapi，兼容网关渠道）：多绑 task_extend_plugin_keys。
-- 其余渠道类型两列都必须为空/空数组。
--
-- 为什么多绑用 jsonb 数组而不是关联表：绑定是渠道行的一部分（随渠道
-- 创建/更新整体提交、随渠道删除消失），读路径只有「按 key 反查渠道」
-- 一种模式，jsonb + GIN 足够；独立关联表会引入第二处一致性维护点，
-- 而排序语义（同一模型多候选按插件 key 升序执行）由数据面计算，不落序。
--
-- key 字符集与 task_plugins.key / R-M6D-01 同源：^[a-z0-9][a-z0-9-]{0,29}$。
-- DB CHECK 只约束形态存在性；「必须已注册」「跨类型不得误填」需要运行时
-- 注册表，属应用层职责（DB 无法看到 factory 层）。
--
-- task_extend_plugin_keys 用 NOT NULL DEFAULT '[]'：空数组表示「无多绑」，
-- 与 NULL（不可表达的缺省）区分，查询 @> 时无需额外 IS NOT NULL 分支。
-- ============================================================================

ALTER TABLE channels
    ADD COLUMN IF NOT EXISTS task_plugin_key text,
    ADD COLUMN IF NOT EXISTS task_extend_plugin_keys jsonb NOT NULL DEFAULT '[]'::jsonb;

-- 单绑 key 形态（空串会被本 CHECK 拒绝；「不绑」只能写 NULL）。
ALTER TABLE channels
    ADD CONSTRAINT channels_task_plugin_key_chk
    CHECK (task_plugin_key IS NULL OR task_plugin_key ~ '^[a-z0-9][a-z0-9-]{0,29}$');

-- 多绑必须是 JSON 数组（元素形态由应用层逐项校验）。
ALTER TABLE channels
    ADD CONSTRAINT channels_task_extend_plugin_keys_chk
    CHECK (jsonb_typeof(task_extend_plugin_keys) = 'array');

-- 单绑反查（插件列表的绑定渠道数 / 停用预检）按 key 等值过滤。
CREATE INDEX IF NOT EXISTS channels_task_plugin_key_idx
    ON channels (task_plugin_key) WHERE task_plugin_key IS NOT NULL;

-- 多绑反查用 jsonb 包含（@> jsonb_build_array($n)），与既有 GIN 口径
-- 一致使用 jsonb_path_ops（它覆盖 @>，不覆盖 ? 谓词）。
CREATE INDEX IF NOT EXISTS channels_task_extend_plugin_keys_gin
    ON channels USING gin (task_extend_plugin_keys jsonb_path_ops);
