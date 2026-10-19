-- ============================================================================
-- M6d Task 6a —— 任务数据面主表 `tasks`（relay-06 §7 / 契约 v1.1 §2.5）
--
-- 为什么统一列在前、插件附加列在后：契约 §2.5 冻结的 14 列是 05（MJ）与
-- 06（插件）**两族共用的同一张表**，判别键 family 决定 Action 的解释权；
-- 列顺序照抄设计表，便于审计时逐行对照文档。
--
-- 为什么没有 FK：user_id / channel_id 是逻辑引用（platform-04 §3.4 V5）。
-- 任务行是计费/审计事实，用户或渠道被硬删除后仍需保留历史任务，级联删除
-- 会破坏对账。quota 是当前计费额度（预扣 → 结算后为实际），也不是外键。
--
-- 为什么 "group" 双引号：SQL 保留字，DDL 必须引用（platform-04 §3.7 V8）。
--
-- 枚举 CHECK 一律写成 = ANY (ARRAY[...])：PG 会把单元素 IN 规范化掉，
-- EnumChecks 反向对账（internal/store/enum.go）依赖 ANY/ARRAY 形状解析；
-- 语义与 IN 相同。
-- ============================================================================

CREATE TABLE IF NOT EXISTS tasks (
    id                 bigint       GENERATED ALWAYS AS IDENTITY PRIMARY KEY,

    -- 契约 §2.5 统一列。
    public_task_id     varchar(191) NOT NULL,
    upstream_task_id   text         NOT NULL DEFAULT '',
    family             text         NOT NULL,
    action             varchar(40)  NOT NULL,
    plugin_key         varchar(30)  NOT NULL,
    plugin_generation  bigint       NOT NULL DEFAULT 0,
    fetch_mode         text         NOT NULL,
    locked_channel_id  bigint       NOT NULL DEFAULT 0,
    origin_task_ids    jsonb        NOT NULL DEFAULT '[]'::jsonb,
    state              text         NOT NULL,
    raw_state          text         NOT NULL DEFAULT '',
    progress_percent   integer      NOT NULL DEFAULT 0,
    retention_deadline timestamptz  NULL,

    -- 插件平台附加列（relay-06 §7）。
    user_id            bigint       NOT NULL,
    "group"            text         NOT NULL DEFAULT '',
    channel_id         bigint       NOT NULL DEFAULT 0,
    quota              bigint       NOT NULL DEFAULT 0,
    fail_reason        text         NOT NULL DEFAULT '',
    submit_time        timestamptz  NULL,
    start_time         timestamptz  NULL,
    finish_time        timestamptz  NULL,
    properties         jsonb        NOT NULL DEFAULT '{}'::jsonb,
    -- private_data 含 PluginState / BillingSource / 凭据无关的执行溯源，
    -- **不出现在任何响应**（handler 的 JSON 投影用 json:"-"）。
    private_data       jsonb        NOT NULL DEFAULT '{}'::jsonb,
    -- data 是上游响应快照，产物（artifacts）从这里按需投影，不建第二份真相。
    data               jsonb        NOT NULL DEFAULT '{}'::jsonb,
    created_at         timestamptz  NOT NULL DEFAULT now(),
    updated_at         timestamptz  NOT NULL DEFAULT now(),

    -- 对外任务 ID 全局唯一：能力式读取（/v1/tasks/{key}?task_id=）先按
    -- (user_id, public_task_id) 定位，唯一约束防碰撞重放。
    CONSTRAINT tasks_public_task_id_uq UNIQUE (public_task_id),

    -- 本族判别键恒为 plugin（FamilyPlugin）；05 的 MJ 行用 mj。
    CONSTRAINT tasks_family_chk
        CHECK (family = ANY (ARRAY['plugin'::text])),

    -- FetchMode 是「本系统如何取回结果」，与插件 meta.fetchMode（per_task /
    -- batch）同名不同义（契约 §2.5 补遗 8），不得混填。
    CONSTRAINT tasks_fetch_mode_chk
        CHECK (fetch_mode = ANY (ARRAY['poll'::text, 'callback'::text, 'inline'::text])),

    -- 7 态持久化枚举（TaskState，契约 §2.5）。终态不可逆由应用层状态机
    -- 保证；DB 只兜底取值域。
    CONSTRAINT tasks_state_chk
        CHECK (state = ANY (ARRAY[
            'not_start'::text, 'submitted'::text, 'queued'::text,
            'in_progress'::text, 'success'::text, 'failure'::text,
            'unknown'::text
        ])),

    -- 进度是 0–100 的数值投影；上游字符串进度由适配器归一后写入。
    CONSTRAINT tasks_progress_percent_chk
        CHECK (progress_percent >= 0 AND progress_percent <= 100)
);

-- 归属读取：能力式读取恒带 user_id 过滤，(user_id, public_task_id) 复合索引
-- 让「他人任务」与「不存在」走同一次索引探测，不产生跨用户唯一性侧信道。
CREATE INDEX IF NOT EXISTS tasks_user_public_idx
    ON tasks (user_id, public_task_id);

-- 轮询器只关心未完成任务（relay-06 §8.1）：部分索引把扫描面压到在飞行，
-- 避免随着历史任务增长把整表拖进后台每 15s 的调度。
CREATE INDEX IF NOT EXISTS tasks_pending_idx
    ON tasks (state) WHERE state NOT IN ('success', 'failure');

-- 客户端台账按创建时间倒序分页（GET /api/task/self）。
CREATE INDEX IF NOT EXISTS tasks_user_created_idx
    ON tasks (user_id, created_at DESC);
