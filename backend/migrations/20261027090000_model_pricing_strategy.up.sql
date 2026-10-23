-- 模型定价时段策略（docs/spec-2026-10-23-模型定价策略-design.md §5.1、§5.2.1）
--
-- 【为什么是独立列而不是 billing_expr】公开模型库的可见条件是
-- `status = 1 AND billing_expr IS NULL`。把时段策略编码进表达式会让模型从首页
-- 消失（之前修过的公开目录问题正是这个）；而且 billing/expr 目前只有算术子集，
-- 表达不了时间函数。策略因此存进独立 JSONB 列，由定价源在解析快照时应用。
--
-- 【为什么只加列、不回填】已有行 strategy IS NULL，行为与本次改动前逐字段一致；
-- 基础费率继续写 prompt_price/completion_price/cache_read_price/cache_write_price，
-- 按次价继续写 prompt_price 且 mode = 1。回滚只需 DROP COLUMN，已保存的策略
-- 丢失是可接受的代价（不需要数据回填）。
-- 幂等写法：本迁移可能在一个已经由早前手工/预发验证写入过 schema 的库上执行
-- （迁移版本号后移是为了排在既有的 20261026120000_model_performance_tests 之后，
-- 避免旧库因版本号更高而跳过本迁移）。列与表用 IF NOT EXISTS；约束用 DO 块。
ALTER TABLE model_pricing ADD COLUMN IF NOT EXISTS strategy jsonb;
-- JSONB 接受数组/标量；策略永远是对象（{"version":1,"time":{...}}），在库层拦住
-- 明显写错的形状，避免解析端拿到非对象时才能发现。
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'model_pricing_strategy_object_chk'
    ) THEN
        ALTER TABLE model_pricing ADD CONSTRAINT model_pricing_strategy_object_chk
            CHECK (strategy IS NULL OR jsonb_typeof(strategy) = 'object');
    END IF;
END
$$;

-- 中国法定节假日与调休上班日日历（数据源 NateScarlet/holiday-cn，MIT）。
-- is_off_day = true 是法定假日；false 是调休上班日（周末照常上班）。
-- 每年一个 JSON 整体替换该年的行：PRIMARY KEY (country, date) 让「同日重复」
-- 在库层失败，而不是让定价读到两行相冲突的事实。
CREATE TABLE IF NOT EXISTS holiday_calendar (
    country    char(2)     NOT NULL,
    date       date        NOT NULL,
    is_off_day boolean     NOT NULL,
    name       text        NOT NULL DEFAULT '',
    source     text        NOT NULL,
    synced_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (country, date)
);

-- 冻结的请求计价事实（spec §5.2「冻结时间」）。
-- 预扣成功时写入一条以 request_id 为键的记录；异步/handoff 结算
-- （Engine.TerminateByRequestID / RefundByRequestID）据此读回**请求开始时**
-- 的时间与时段倍率，不得用 time.Now() 或当前策略重新解析——否则跨整点、
-- 预扣后改策略、跨组重试都会得到与预扣不一致的价。
-- 不设到 request_registry 的外键：这是计价审计事实，行可能比请求行保留更久，
-- 与财务/日志类表一样按逻辑引用处理。
CREATE TABLE IF NOT EXISTS request_pricing (
    request_id      uuid PRIMARY KEY,
    request_time    timestamptz NOT NULL,
    time_multiplier double precision NOT NULL,
    time_window     text NOT NULL DEFAULT '',
    created_at      timestamptz NOT NULL DEFAULT now()
);
-- preconsume_reconcile 定期清理 7 天前的行，按 created_at 扫描。
CREATE INDEX IF NOT EXISTS request_pricing_created_idx ON request_pricing (created_at);
