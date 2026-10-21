-- ============================================================================
-- W8 / MJ-a2 —— MJ 归属查询索引
--
-- 依据：R-MJ-05（MjId 不是全局唯一键，归属键是 (user_id, upstream_task_id)）；
--       MJ-b 的 fetch/list 与 MJ-c 的图片代理都要按 MjId 读取任务。
--
-- 为什么是部分索引（WHERE family='mj' AND upstream_task_id <> ''）：
--   - 插件行没有 MjId 语义，把它们纳入索引只会让索引随插件任务量膨胀；
--   - 上传 inline 行的 upstream_task_id='' 没有上游任务，任何按 MjId 的查询
--     都不应命中（a2 的复用查询显式拒绝空 id）。
--
-- 为什么两条而不是一条：
--   - tasks_mj_user_upstream_idx 服务 code 21 复用与动作类端点锁定原渠道
--     （WHERE user_id=$1 AND upstream_task_id=$2）；顺序 user_id 在前使
--     同用户的查询只需一次索引定位。
--   - tasks_mj_upstream_idx 服务图片代理/MJ-b 的「按 MjId 取候选 owner」
--     （无 user_id 约束），全局按 MjId 定位后逐个验签。
-- ============================================================================

CREATE INDEX IF NOT EXISTS tasks_mj_user_upstream_idx
    ON tasks (user_id, upstream_task_id)
    WHERE family = 'mj' AND upstream_task_id <> '';

CREATE INDEX IF NOT EXISTS tasks_mj_upstream_idx
    ON tasks (upstream_task_id)
    WHERE family = 'mj' AND upstream_task_id <> '';
