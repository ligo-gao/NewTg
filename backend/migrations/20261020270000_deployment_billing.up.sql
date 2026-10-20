-- ============================================================================
-- C2 / Task 5c —— GPU 资源租赁计费两张表（`G-OPS-012`）
--
-- 设计权威：
--   - `platform-01` §22.13：24 + 19 字段、索引与 CHECK（单一真源）；
--   - `platform-04` §11.2：成对 up/down、numeric 精度、text + CHECK 枚举、
--     不分区、不软删除；
--   - `billing-03` §3.7/§5.9、`billing-05` §4.4：语义（站点成本账，
--     付款方是站点，不从任何用户钱包/订阅/令牌扣减）。
--
-- 为什么两张表都与 provider 镜像无关：provider 仍是部署/容器/日志的唯一
-- 真源（§22.12）；这里只保存「计费对象的最小关联」与 append-only 账本。
--
-- 为什么枚举不用 PG enum：迁移期修改枚举需要重建类型；text + CHECK 可
-- 直接 ADD/DROP CONSTRAINT，且 store.EnumChecks 双向对账能捕获漂移。
--
-- 为什么 fact 表有 FK 而其它财务表没有：段是事实的父对象，删除段后孤立的
-- 成本事实没有对账意义；RESTRICT 阻止误删仍在账上的段（V5 的「逻辑引用」
-- 例外在此不适用——同域父表）。
-- ============================================================================

-- ==== deployment_billing_segments（24 列）====

CREATE TABLE IF NOT EXISTS deployment_billing_segments (
    -- 本地段 ID（UUID 文本）。计费与幂等锚点；不是 provider 部署 id。
    segment_id                    text        NOT NULL,
    -- provider 侧部署 ID，只作关联与对账。
    deployment_id                 text        NOT NULL,
    segment_kind                  text        NOT NULL,
    -- 创建/续期的 Idempotency-Key；重复请求复用同一段（同一段只预扣一次）。
    idempotency_key               text        NOT NULL,
    provider_request_id           text,
    name                          text        NOT NULL,
    hardware_id                   bigint      NOT NULL,
    location_ids                  bigint[]    NOT NULL,
    gpus_per_container            integer     NOT NULL,
    replica_count                 integer     NOT NULL,
    -- 本段计划时长（720h 上界）。严格上界 = planned_seconds + 300s 停止宽限。
    planned_seconds               bigint      NOT NULL,
    started_at                    timestamptz,
    planned_end_at                timestamptz,
    -- 冻结的 provider 报价（USD）与由它推导的每 GPU·秒单价（decimal，禁 float）。
    quote_usd                     numeric(20,6)  NOT NULL,
    unit_price_usd_per_gpu_second numeric(30,18) NOT NULL,
    price_source                  text        NOT NULL,
    -- 站点成本账预扣（整数积分）；不从用户钱包扣减。
    preconsumed_credits           bigint      NOT NULL,
    settled_credits               bigint,
    state                         text        NOT NULL,
    -- 对账/崩溃兜底心跳；超过 DeploymentBillingLeaseTTLSeconds=300s 可被 worker 回收。
    billing_heartbeat_at          timestamptz NOT NULL,
    last_provider_observed_at     timestamptz,
    created_at                    timestamptz NOT NULL,
    updated_at                    timestamptz NOT NULL,
    -- 进入当前 state 的时刻；900s 对账窗口从这里起算，重试不重置。
    state_changed_at              timestamptz NOT NULL,

    CONSTRAINT deployment_billing_segments_pk PRIMARY KEY (segment_id),
    CONSTRAINT deployment_billing_segments_segment_kind_chk
        CHECK (segment_kind IN ('create', 'extend')),
    CONSTRAINT deployment_billing_segments_gpus_chk
        CHECK (gpus_per_container BETWEEN 1 AND 8),
    CONSTRAINT deployment_billing_segments_replicas_chk
        CHECK (replica_count BETWEEN 1 AND 64),
    CONSTRAINT deployment_billing_segments_planned_chk
        CHECK (planned_seconds BETWEEN 1 AND 2592000),
    CONSTRAINT deployment_billing_segments_quote_chk
        CHECK (quote_usd >= 0),
    CONSTRAINT deployment_billing_segments_unit_price_chk
        CHECK (unit_price_usd_per_gpu_second >= 0),
    CONSTRAINT deployment_billing_segments_price_source_chk
        CHECK (price_source IN ('provider_quote')),
    CONSTRAINT deployment_billing_segments_preconsumed_chk
        CHECK (preconsumed_credits >= 0),
    CONSTRAINT deployment_billing_segments_settled_chk
        CHECK (settled_credits IS NULL OR settled_credits >= 0),
    CONSTRAINT deployment_billing_segments_state_chk
        CHECK (state IN ('pending_confirm', 'running', 'deleting_unknown', 'settled', 'abandoned', 'failed'))
);

-- 幂等锚：同一 Idempotency-Key 只能生成一段。
CREATE UNIQUE INDEX IF NOT EXISTS deployment_billing_segments_idempotency_uq
    ON deployment_billing_segments (idempotency_key);

-- 按部署查活动段 / worker 对账扫描。
CREATE INDEX IF NOT EXISTS deployment_billing_segments_deployment_state_idx
    ON deployment_billing_segments (deployment_id, state);

-- N16 的心跳扫描（billing_heartbeat_at < now - 300s）。
CREATE INDEX IF NOT EXISTS deployment_billing_segments_heartbeat_idx
    ON deployment_billing_segments (billing_heartbeat_at);

-- ==== deployment_cost_facts（19 列，append-only）====

CREATE TABLE IF NOT EXISTS deployment_cost_facts (
    fact_id                       bigint      GENERATED ALWAYS AS IDENTITY,
    -- 幂等键：<fact_type>:<segment_id>:<period_start>（period_start 为空用 '-'）。
    fact_key                      text        NOT NULL,
    segment_id                    text        NOT NULL,
    deployment_id                 text        NOT NULL,
    fact_type                     text        NOT NULL,
    period_start                  timestamptz,
    period_end                    timestamptz,
    gpus_per_container            integer     NOT NULL,
    replica_count                 integer     NOT NULL,
    -- 规范计费量（GPU·秒）；container_hour 只在管理面展示，不落账本。
    gpu_seconds                   bigint      NOT NULL,
    unit                          text        NOT NULL,
    usage_source                  text        NOT NULL,
    estimated_credits             bigint      NOT NULL,
    confirmed_credits             bigint,
    state                         text        NOT NULL,
    provider_observed_at          timestamptz,
    -- 审计/请求引用（不含任何 provider 密钥或秘密环境变量）。
    audit_ref                     text,
    created_at                    timestamptz NOT NULL,
    -- provider 返回的部署级累计秒数（原始值）；段内值 = 本值 − 上一段基线。
    provider_cumulative_seconds   bigint,

    CONSTRAINT deployment_cost_facts_pk PRIMARY KEY (fact_id),
    CONSTRAINT deployment_cost_facts_segment_fk
        FOREIGN KEY (segment_id) REFERENCES deployment_billing_segments (segment_id)
        ON DELETE RESTRICT,
    CONSTRAINT deployment_cost_facts_fact_type_chk
        CHECK (fact_type IN ('preconsume', 'accrual', 'settlement', 'reconciliation_adjustment', 'released')),
    CONSTRAINT deployment_cost_facts_gpus_chk
        CHECK (gpus_per_container BETWEEN 1 AND 8),
    CONSTRAINT deployment_cost_facts_replicas_chk
        CHECK (replica_count BETWEEN 1 AND 64),
    CONSTRAINT deployment_cost_facts_gpu_seconds_chk
        CHECK (gpu_seconds >= 0),
    CONSTRAINT deployment_cost_facts_unit_chk
        CHECK (unit IN ('gpu_second')),
    CONSTRAINT deployment_cost_facts_usage_source_chk
        CHECK (usage_source IN ('provider_exact', 'provider_partial', 'internal_accounting', 'estimated', 'reserved_default')),
    CONSTRAINT deployment_cost_facts_estimated_chk
        CHECK (estimated_credits >= 0),
    CONSTRAINT deployment_cost_facts_confirmed_chk
        CHECK (confirmed_credits IS NULL OR confirmed_credits >= 0),
    CONSTRAINT deployment_cost_facts_state_chk
        CHECK (state IN ('held', 'accrued', 'confirmed', 'released', 'adjusted')),
    CONSTRAINT deployment_cost_facts_cumulative_chk
        CHECK (provider_cumulative_seconds IS NULL OR provider_cumulative_seconds >= 0)
);

-- 幂等：同一段同一周期同类型事实只写一次。
CREATE UNIQUE INDEX IF NOT EXISTS deployment_cost_facts_fact_key_uq
    ON deployment_cost_facts (fact_key);

-- 按部署的时间线（对账/成本汇总）。
CREATE INDEX IF NOT EXISTS deployment_cost_facts_deployment_created_idx
    ON deployment_cost_facts (deployment_id, created_at);

-- 按段查事实类型。
CREATE INDEX IF NOT EXISTS deployment_cost_facts_segment_type_idx
    ON deployment_cost_facts (segment_id, fact_type);
