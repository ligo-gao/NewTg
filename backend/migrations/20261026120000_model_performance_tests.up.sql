-- ============================================================================
-- 模型性能测试（渠道测速）主库两表
--
-- 【为什么落主库而不是指标库】桌面端/管理面的「对某渠道的某模型跑 N 次请求」
-- 是一次**显式、低频、带操作者**的测试，不是持续采集的观测流。它需要与
-- channels/users 的引用完整性（渠道删除后测试记录随之消失、操作者删除后仅
-- 解除关联），也需要在管理面直接分页读取；这些都属于主库的业务事实。
-- 高频性能指标仍走独立指标库（PERF_METRICS_DSN），两者不混用。
--
-- 【两表：汇总 + 逐次】model_performance_tests 保存一次测试的聚合结果
-- （成功率、P50/P95、平均 TPS/输出 token），model_performance_runs 保存每次
-- 请求的原始观测（成功/失败、TTFT、延迟、输出 token、错误分类）。分开的
-- 理由：列表页只读汇总行（避免每次扫描 N 条明细）；明细保留到测试被删除为止，
-- 供点击展开或排障。runs 上限 10（见 CHECK），明细表不会无界膨胀。
--
-- 【为什么 success_rate 单独存且限定 [0,1]】读面需要按成功率排序/筛选，实时
-- 从 runs 重算会让列表查询退化成聚合；写入端（store.Create）在构造汇总值时
-- 计算一次。CHECK 保证它与 success_count/runs 的口径不会因写入 bug 漂移成
-- 非法比例（0–1 之外的百分比）。
--
-- 【索引口径】两个读面各一条：
--   - (model, created_at DESC)：按模型查最近测试（LatestByModel / 模型详情）；
--   - (channel_id, created_at DESC)：管理面按渠道列最近测试（ListRecent）。
-- 明细表按 test_id 聚合读取。
-- ============================================================================

CREATE TABLE model_performance_tests (
    id                bigint           GENERATED ALWAYS AS IDENTITY,
    channel_id        bigint           NOT NULL,
    model             text             NOT NULL,
    provider          text             NOT NULL,
    -- 本次计划的请求次数；1–10 是产品上限，防止一次操作放大成无界压测。
    runs              integer          NOT NULL,
    success_count     integer          NOT NULL,
    -- 成功比例（0–1），不是百分数。
    success_rate      double precision NOT NULL,
    -- 空值表示该次测试没有可用的对应样本（例如全部失败），读面显示 "—"。
    ttft_ms_p50       integer,
    ttft_ms_p95       integer,
    latency_ms_p50    integer,
    latency_ms_p95    integer,
    tps_avg           double precision,
    output_tokens_avg double precision,
    -- 操作者；用户被删除后置空，测试事实本身保留。
    created_by        bigint,
    created_at        timestamptz      NOT NULL DEFAULT now(),
    CONSTRAINT model_performance_tests_pkey PRIMARY KEY (id),
    CONSTRAINT model_performance_tests_runs_chk CHECK (runs BETWEEN 1 AND 10),
    CONSTRAINT model_performance_tests_success_count_chk
        CHECK (success_count BETWEEN 0 AND runs),
    CONSTRAINT model_performance_tests_success_rate_chk
        CHECK (success_rate BETWEEN 0 AND 1),
    CONSTRAINT model_performance_tests_channel_fk
        FOREIGN KEY (channel_id) REFERENCES channels (id) ON DELETE CASCADE,
    CONSTRAINT model_performance_tests_created_by_fk
        FOREIGN KEY (created_by) REFERENCES users (id) ON DELETE SET NULL
);

CREATE INDEX model_performance_tests_model_created_idx
    ON model_performance_tests (model, created_at DESC);
CREATE INDEX model_performance_tests_channel_created_idx
    ON model_performance_tests (channel_id, created_at DESC);

CREATE TABLE model_performance_runs (
    id            bigint           GENERATED ALWAYS AS IDENTITY,
    test_id       bigint           NOT NULL,
    -- 第几次请求（1 基）；由写入端保证落在 1..runs。
    run_index     integer          NOT NULL,
    success       boolean          NOT NULL,
    ttft_ms       integer,
    latency_ms    integer,
    output_tokens integer,
    tps           double precision,
    -- 失败分类（固定枚举/短文案），成功行留空；不写原始上游错误文本。
    error_class   text,
    created_at    timestamptz      NOT NULL DEFAULT now(),
    CONSTRAINT model_performance_runs_pkey PRIMARY KEY (id),
    CONSTRAINT model_performance_runs_test_fk
        FOREIGN KEY (test_id) REFERENCES model_performance_tests (id) ON DELETE CASCADE
);

CREATE INDEX model_performance_runs_test_idx ON model_performance_runs (test_id);
