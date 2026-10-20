-- ============================================================================
-- T4 / P-04：/v1/batches 的父批次视图
--
-- 冻结语义：批次**不建第二套异步状态机**。每条子请求仍是 tasks 表的一行
-- （提交→轮询→产物全在既有数据面），父批次只做聚合视图；因此本表没有
-- 上游 id、轮询游标或结果快照，只有归属、计数与状态。
--
-- total 是提交行数（含上游提交失败的子请求）；completed/failed 是读取时由
-- task_batches_items ⋈ tasks 现算并回写的快照（见 tasks.Repo.GetBatchByID），
-- 不是权威来源——子任务状态由轮询器推进，唯一天然真相在 tasks.state。
-- ============================================================================
CREATE TABLE IF NOT EXISTS task_batches (
    -- id 由应用层用 crypto/rand 生成（批次数是用户可见低量资源，不需要
    -- identity 序列）；DEFAULT gen_random_uuid() 只是让手工/工具插入也有
    -- 安全默认值，schema 断言据此把它归入 uuid 代理主键表。
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),

    -- 归属：批次与 /v1/tasks 的台账同口径按 user_id 过滤；token_id 记录
    -- 创建它的令牌（Bearer 路径恒有），便于审计与按令牌排查。
    user_id    bigint      NOT NULL,
    token_id   bigint      NOT NULL DEFAULT 0,

    -- 状态取值对齐 OpenAI Batch 的只读子集；本实现产出 in_progress /
    -- completed / failed（取消端点当前 fail-closed，不产出 cancelled）。
    status     text        NOT NULL
        CONSTRAINT task_batches_status_chk
        CHECK (status IN ('in_progress', 'completed', 'failed')),

    total      integer     NOT NULL CHECK (total >= 0),
    completed  integer     NOT NULL DEFAULT 0 CHECK (completed >= 0),
    failed     integer     NOT NULL DEFAULT 0 CHECK (failed >= 0),

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- 本人批次列表（将来 GET /v1/batches 分页）按时间倒序；与 tasks 台账同口径。
CREATE INDEX IF NOT EXISTS task_batches_user_created_idx
    ON task_batches (user_id, created_at DESC, id);

-- 子任务关联：只存 (batch_id, task_id)，任务行本身不复制。
-- ON DELETE CASCADE：任务被清理时关联随之消失（聚合计数再按 total 兜底），
-- 批次行不因此变成半垃圾数据。
CREATE TABLE IF NOT EXISTS task_batches_items (
    batch_id uuid   NOT NULL REFERENCES task_batches (id) ON DELETE CASCADE,
    task_id  bigint NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
    PRIMARY KEY (batch_id, task_id)
);

-- 反查「某任务属于哪些批次」（管理面排障）；主键只覆盖 batch→task 方向。
CREATE INDEX IF NOT EXISTS task_batches_items_task_idx
    ON task_batches_items (task_id);
