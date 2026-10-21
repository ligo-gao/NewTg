-- 回退 S3 退款状态与字段。
--
-- fail-loud：退款是**已发生的资金事实**，不能随迁移回滚删除退款金额/时刻，
-- 也不能把 refunded/refund_failed 行硬塞回旧六态。存在这类行时显式报错，
-- 要求先人工归档/对账，而不是让随后的 ADD CONSTRAINT 以难读的 SQLSTATE
-- 失败或静默丢列。
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM orders WHERE status IN ('refunded', 'refund_failed')) THEN
        RAISE EXCEPTION
            'cannot roll back 20261022090000: orders with status refunded/refund_failed exist; archive/reconcile them first';
    END IF;
END $$;

DROP INDEX IF EXISTS wallet_reversal_order_uq;

ALTER TABLE orders DROP CONSTRAINT IF EXISTS orders_refund_reason_chk;
ALTER TABLE orders DROP CONSTRAINT IF EXISTS orders_refunded_at_chk;
ALTER TABLE orders DROP CONSTRAINT IF EXISTS orders_refunded_amount_chk;

ALTER TABLE orders DROP CONSTRAINT IF EXISTS orders_status_chk;

ALTER TABLE orders ADD CONSTRAINT orders_status_chk
    CHECK (status IN ('pending', 'succeeded', 'failed', 'expired', 'cancelled', 'refund_required'));

ALTER TABLE orders DROP COLUMN IF EXISTS refund_requested_at;
ALTER TABLE orders DROP COLUMN IF EXISTS refund_reason;
ALTER TABLE orders DROP COLUMN IF EXISTS refunded_at;
ALTER TABLE orders DROP COLUMN IF EXISTS refunded_amount;
