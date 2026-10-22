-- ============================================================================
-- W17-2 worker 任务框架三表（billing-06 §2.4 / §13.5 系统任务 API）
--
-- 依据：
--   - billing-06 §2.2  无 cron：`last_run_at + interval_seconds` 判定到期；
--   - billing-06 §2.3.1 每类型一把 DB 租约（60s TTL / 20s 心跳 / generation）；
--   - billing-06 §2.4  三张表的字段清单（本文逐列落地，另补设计要求的
--     created_at/updated_at）；
--   - billing-06 §13.5 管理面只读 + 白名单手动触发。
--
-- 【为什么三张表而不是一张】
--   worker_tasks 是调度目录（每类型一行，低频写）；worker_task_locks 是
--   互斥状态（每次认领/心跳都写）；worker_task_runs 是 append-only 执行
--   历史（每轮一行，按保留策略收敛）。三者的写频、保留期与读取面都不同，
--   合并会让「运行历史」把调度行撑大，也会让租约更新与历史写入抢同一行锁。
--
-- 【为什么 worker_task_runs 不用软删除】platform-04 §3 的统一口径：历史
-- 由保留清理（worker_history_cleanup）物理删除，不引入 deleted_at——被删
-- 除的运行记录不应再参与任何统计或列表。
--
-- 【索引口径】管理员读取面固定三种查询：按任务（详情最近 N 条）、按状态
-- （过期租约清理 / 运行计数）、按时间（保留清理）。三条索引与之一一对应。
-- ============================================================================

-- 调度目录：每类型一行。无调度行时 API 回落到注册表默认值（不在本表虚构行）。
CREATE TABLE worker_tasks (
    task_type         text        NOT NULL,
    -- 周期秒数；0 表示非周期（仅手动触发，如 log_cleanup / log_partition_precreate）。
    interval_seconds  integer     NOT NULL DEFAULT 0,
    -- 总开关；任务自身的 Enabled() 谓词可在调度 pass 中再叠加（§2.6），
    -- 但不会回写本列——本列表达运营意图，谓词表达运行时条件。
    enabled           boolean     NOT NULL DEFAULT true,
    -- 队列分片号（§2.2「按队列分片」：不同类型分散到不同 worker），MVP 单分片。
    shard             smallint    NOT NULL DEFAULT 0,
    last_run_at       timestamptz,
    last_status       text,
    -- 派生列 last_run_at + interval；物化以加速到期扫描（§2.4）。
    next_due_at       timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT worker_tasks_pkey PRIMARY KEY (task_type),
    CONSTRAINT worker_tasks_interval_chk CHECK (interval_seconds >= 0),
    CONSTRAINT worker_tasks_last_status_chk
        CHECK (last_status IS NULL OR last_status IN ('succeeded', 'failed', 'skipped'))
);

-- 到期扫描只关心启用行；部分索引避免把停用任务带进每一轮 pass。
CREATE INDEX worker_tasks_due_idx ON worker_tasks (enabled, next_due_at) WHERE enabled;

-- 每类型一把租约。字段与 §2.3.1 的 CAS 语句逐列对应。
CREATE TABLE worker_task_locks (
    task_type        text        NOT NULL,
    holder_id        text,
    lease_expires_at timestamptz,
    heartbeat_at     timestamptz,
    -- 单调递增的抢占代数：旧持有者的迟到续约必须同时匹配 holder_id 与
    -- generation，不能只靠 holder 字符串（同一节点重启后 holder 可能复用）。
    generation       bigint      NOT NULL DEFAULT 0,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT worker_task_locks_pkey PRIMARY KEY (task_type),
    CONSTRAINT worker_task_locks_generation_chk CHECK (generation >= 0)
);

-- 过期租约清理 loop（30s）按到期时间扫描。
CREATE INDEX worker_task_locks_expiry_idx ON worker_task_locks (lease_expires_at);

-- 执行历史（append-only）。status 的终态由执行者写入；cleanup loop 只把
-- running 改成 failed，因此 CHECK 不需要额外的迁移态。
CREATE TABLE worker_task_runs (
    id           bigint      GENERATED ALWAYS AS IDENTITY,
    task_type    text        NOT NULL,
    status       text        NOT NULL DEFAULT 'pending',
    -- 执行节点；pending 行还没有执行者，故可空。
    node_id      text,
    started_at   timestamptz,
    finished_at  timestamptz,
    -- 失败原因只写**固定分类/固定文案**（timeout / task lease expired /
    -- execution_error / cancelled），不写原始 err 文本：任务可能接触上游
    -- 凭据或响应体，原始错误进入长期保留的运行历史会变成密钥副本。
    error        text,
    -- 结果汇总（如轮询任务的 {unfinished, platforms, null_failed}）。只放
    -- 计数与枚举，不放任务载荷。
    result       jsonb,
    trigger      text        NOT NULL DEFAULT 'schedule',
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT worker_task_runs_pkey PRIMARY KEY (id),
    CONSTRAINT worker_task_runs_status_chk
        CHECK (status IN ('pending', 'running', 'succeeded', 'failed')),
    CONSTRAINT worker_task_runs_trigger_chk
        CHECK (trigger IN ('schedule', 'manual', 'wakeup')),
    CONSTRAINT worker_task_runs_error_len_chk
        CHECK (error IS NULL OR char_length(error) <= 500)
);

-- 详情页：按任务取最近 N 条（started_at DESC）。
CREATE INDEX worker_task_runs_task_started_idx
    ON worker_task_runs (task_type, started_at DESC);
-- 租约过期清理 / 运行中判定：按状态扫描 running 行。
CREATE INDEX worker_task_runs_status_started_idx
    ON worker_task_runs (status, started_at DESC);
-- 保留清理：按时间删除早于窗口的历史（worker_history_cleanup）。
CREATE INDEX worker_task_runs_started_idx ON worker_task_runs (started_at);
