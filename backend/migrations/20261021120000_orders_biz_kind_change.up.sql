-- ============================================================================
-- W2 / S2 订阅升级降级：orders.biz_kind 增加 'change'
--
-- 升降级订单判别列（任务书 R-23）：biz_kind='change'、
-- biz_ref_id=subscriptions.id（被升降级的实例）。'change' 与 'renew' 的
-- biz_ref_id 同义，但履约动作不同（续费推进 end_at 不改套餐；升降级改写
-- 套餐快照与额度并追加 upgraded/downgraded 事件），因此必须有独立判别值，
-- 不能靠解析快照猜业务——结算分支只认 biz_kind。
--
-- 只放开 CHECK，不动既有行与索引：'topup' 默认语义不变；既有 'subscribe'
-- 行继续把 biz_ref_id 解释为套餐 id，'renew'/'change' 行解释为实例 id。
--
-- 金额口径：'change' 订单的 amount 是**应补差价**（新套餐价 − 剩余价值，
-- 不为负），不是套餐全价；0 元差价走同事务即时生效，不经过渠道。
-- ============================================================================
ALTER TABLE orders DROP CONSTRAINT orders_biz_kind_chk;

ALTER TABLE orders ADD CONSTRAINT orders_biz_kind_chk
    CHECK (biz_kind IN ('topup', 'subscribe', 'renew', 'change'));
