-- ============================================================================
-- W2 / S3 —— 订阅购买退款（R-30/R-31/R-32/R-34）
--
-- 退款需要三类持久化事实：
--   1. orders.status 增加两个状态：
--        refunded       —— 退款已完成（钱已原路退回或已补回钱包）；
--        refund_failed  —— 渠道退款失败，可重试。它不是终态：重试成功后
--                          迁移到 refunded（R-32）。与 failed 严格区分：
--                          failed 表示「没收到钱」，refund_failed 表示
--                          「钱收到了但没退成功」。
--   2. orders.refunded_amount —— 已退金额（分）。R-31 要求它与钱包/原路
--      流水在同一事务内写入。CHECK 限定「只有 refunded 才允许非零，且
--      必须等于订单金额」——本任务只做全额退款（P-13），部分退款没有语义；
--      把这条不变量写进 DB 而不是只靠应用层，避免未来出现「状态已退、
--      金额只退了一半」的对账空洞。
--   3. refunded_at / refund_reason —— 退款完成时刻与原因（审计）。
--      refund_requested_at / 复用 refund_reason —— 用户侧「申请退款」只写
--      标记，不直接动钱（S3 brief）。管理员通过 GET /api/orders 看到请求
--      后调用管理员退款端点。
--
-- 为什么 refund_reason 同时服务「申请」与「完成」：一次购买只有一笔退款，
-- 申请理由就是退款理由；管理员可以在批准时覆盖它（后写覆盖前写），
-- 少一列就少一个可能互相矛盾的真源。
--
-- 为什么不在 refunded_amount 上放部分退款语义：P-13 冻结「未开始且未消耗
-- 才全额退款」，任何部分退款都意味着订阅已被部分消费，必须走人工核账，
-- 不允许自动化把一个已消耗的订阅退成负数。
-- ============================================================================

ALTER TABLE orders ADD COLUMN refunded_amount bigint NOT NULL DEFAULT 0;
ALTER TABLE orders ADD COLUMN refunded_at timestamptz;
ALTER TABLE orders ADD COLUMN refund_reason varchar(200);
ALTER TABLE orders ADD COLUMN refund_requested_at timestamptz;

ALTER TABLE orders DROP CONSTRAINT orders_status_chk;

ALTER TABLE orders ADD CONSTRAINT orders_status_chk
    CHECK (status IN ('pending', 'succeeded', 'failed', 'expired', 'cancelled',
                      'refund_required', 'refunded', 'refund_failed'));

-- 全额退款不变量（见文件头）：
--   - 退款金额非负且不超过订单金额；
--   - 只有 refunded 状态可以携带退款金额，且必须是全额；
--   - 非 refunded 状态的 refunded_amount 必须为 0（refund_failed 没有退到钱）。
ALTER TABLE orders ADD CONSTRAINT orders_refunded_amount_chk
    CHECK (refunded_amount >= 0
           AND refunded_amount <= amount
           AND (status <> 'refunded' OR refunded_amount = amount)
           AND (status = 'refunded' OR refunded_amount = 0));

ALTER TABLE orders ADD CONSTRAINT orders_refunded_at_chk
    CHECK ((status = 'refunded') = (refunded_at IS NOT NULL));

ALTER TABLE orders ADD CONSTRAINT orders_refund_reason_chk
    CHECK (refund_reason IS NULL OR char_length(refund_reason) BETWEEN 1 AND 200);

-- 余额退款的幂等锚点：同一订单至多一行 reversal 流水。
--
-- 为什么放在 DB 而不是只信 orders.status：订单状态与钱包流水在同一事务，
-- 正常路径已经串行化；这条部分唯一索引是第二道闸门——任何绕过状态机的
-- 写方（脚本/人工修数/未来的第二个退款入口）都无法把同一笔订单回补两次。
-- 只约束 reference_type='order'：type='reversal' 将来也可能用于别的对象
-- （例如消费撤销），reference_id 在不同对象域内不保证全局唯一。
CREATE UNIQUE INDEX wallet_reversal_order_uq ON wallet_transactions (reference_id)
    WHERE type = 'reversal' AND reference_type = 'order';
