-- ============================================================================
-- W2 / S2 订单状态增加 'refund_required'（R-29②）
--
-- 场景：升降级订单支付成功后，订阅却被并发取消/到期/换了套餐，套餐迁移无法
-- 应用。钱已经收了、货没发，必须有人退款。
--
-- 【为什么必须是独立的终态，而不是退回 pending 或标 failed】
--   - 退回 pending：渠道会按 5xx/未确认无限重投，每次回调都要重跑一遍注定
--     失败的迁移；订单也永远不会离开扫描窗口。
--   - 标 failed：failed 的语义是「支付没有成功」，与「钱已收、需要退款」
--     相反，会让对账把已收的钱当成未收。
--   - refund_required 是终态：不再接受回调迁移，保留 paid_at/provider 字段
--     作为已收款证据，由退款流程（S3）处理。
--
-- 只放开 CHECK，不动既有行与索引（既有订单全部落在原有五态内）。
-- ============================================================================
ALTER TABLE orders DROP CONSTRAINT orders_status_chk;

ALTER TABLE orders ADD CONSTRAINT orders_status_chk
    CHECK (status IN ('pending', 'succeeded', 'failed', 'expired', 'cancelled', 'refund_required'));
