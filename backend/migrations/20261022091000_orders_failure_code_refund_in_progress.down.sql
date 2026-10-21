-- 回退 S3/R-37 的 failure_code 取值。
--
-- refund_in_progress 是**瞬时标记**而不是资金事实：回滚前把它清成 NULL
-- 即可（订单状态与退款金额不受影响），随后恢复旧 CHECK。其它失败码保持
-- 原样，若存在它们之外的残留值，恢复约束会以 SQLSTATE 失败（fail loud）。
UPDATE orders SET failure_code = NULL WHERE failure_code = 'refund_in_progress';

ALTER TABLE orders DROP CONSTRAINT IF EXISTS orders_failure_code_chk;

ALTER TABLE orders ADD CONSTRAINT orders_failure_code_chk CHECK (failure_code IN (
    'provider_unavailable', 'provider_rejected', 'signature_invalid', 'amount_mismatch',
    'currency_mismatch', 'order_not_found', 'order_state_conflict', 'manual_required')
    OR failure_code IS NULL);
