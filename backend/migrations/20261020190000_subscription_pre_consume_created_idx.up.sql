-- ============================================================================
-- B12 收口（M1）—— 预扣幂等记录的清理索引
--
-- 清理谓词是 `created_at < cutoff`（7 天保留期，billing-04 §3.4 /
-- billing-08 §7），并按 created_at 升序 LIMIT 分批。没有索引时每批都要在
-- 保留窗口（最多 7 天的全部请求量）内顺序扫描才能取到最旧的 N 行；请求量
-- 增长后清理的固定成本会变成 O(窗口内总行数)。
--
-- 单列 btree 让子查询直接沿索引取前 N 行；ORDER BY created_at, request_id
-- 仍可由索引前缀满足，request_id 只用于同秒内的稳定次序。
--
-- 写放大：每次预扣插入多维护一个 btree 条目，与请求路径的其余写入相比
-- 可忽略，远小于清理的扫描收益。
-- ============================================================================

CREATE INDEX IF NOT EXISTS subscription_pre_consume_records_created_idx
    ON subscription_pre_consume_records (created_at);
