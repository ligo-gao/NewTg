-- ============================================================================
-- S3 / R-37 —— orders.failure_code 增加 'refund_in_progress'
--
-- 渠道退款调用与本地落库之间没有原子性。调渠道**之前**把订单标为
-- refund_in_progress，可以回答「这笔订单卡在渠道退款中还是根本没退」：
--   - 成功 finalize 清空 failure_code；
--   - 渠道失败改写为 provider_unavailable / provider_rejected；
--   - 进程崩溃会留下 refund_in_progress，运营据此人工对账（订单状态仍是
--     succeeded/refund_failed，可通过同一端点重试）。
--
-- 只放开 CHECK，不改既有行。
-- ============================================================================
ALTER TABLE orders DROP CONSTRAINT orders_failure_code_chk;

ALTER TABLE orders ADD CONSTRAINT orders_failure_code_chk CHECK (failure_code IN (
    'provider_unavailable', 'provider_rejected', 'signature_invalid', 'amount_mismatch',
    'currency_mismatch', 'order_not_found', 'order_state_conflict', 'manual_required',
    'refund_in_progress')
    OR failure_code IS NULL);
