-- ============================================================================
-- P2a 手动续费：orders.biz_kind 增加 'renew'
--
-- 续费订单判别列（任务书 P2a 冻结语义 2）：biz_kind='renew'、
-- biz_ref_id=subscriptions.id（与 'subscribe' 的 biz_ref_id=plan_id 多态，
-- 按 biz_kind 解释）。复用既有订单/幂等/回调/订单中心，不新建支付 owner。
--
-- 只放开 CHECK，不动既有行与索引：'topup' 默认语义不变；'subscribe' 行
-- 继续把 biz_ref_id 解释为套餐 id，'renew' 行解释为实例 id。
-- ============================================================================
ALTER TABLE orders DROP CONSTRAINT orders_biz_kind_chk;

ALTER TABLE orders ADD CONSTRAINT orders_biz_kind_chk
    CHECK (biz_kind IN ('topup', 'subscribe', 'renew'));
