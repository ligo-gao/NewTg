-- ============================================================================
-- P2b 余额购买订阅：放开两个枚举 CHECK
--
-- 冻结语义（任务书 P2b）：
--   - provider='wallet' 是订阅购买的余额支付渠道，不是第三方渠道。订单仍复用
--     orders（biz_kind='subscribe'、biz_ref_id=plan_id），但没有 provider_txn_id
--     与异步回调：下单事务内完成扣款、流水、订单 succeeded 与实例创建。
--   - wallet_transactions.type 新增 'subscription_purchase'（amount 为负，
--     balance_after 为扣款后余额快照）。与 'topup' 分型，账单可按业务统计。
--
-- 只放开枚举，不改列型/索引/既有行。orders.provider 的其它取值语义不变；
-- 'wallet' 行由应用层限定为订阅购买（充值/续费入口不接受该 provider）。
-- ============================================================================

ALTER TABLE orders DROP CONSTRAINT orders_provider_chk;

ALTER TABLE orders ADD CONSTRAINT orders_provider_chk
    CHECK (provider IN ('stripe', 'alipay', 'creem', 'waffo', 'waffo_pancake', 'epay', 'wallet'));

ALTER TABLE wallet_transactions DROP CONSTRAINT wallet_transactions_type_chk;

ALTER TABLE wallet_transactions ADD CONSTRAINT wallet_transactions_type_chk
    CHECK (type IN (
        'topup', 'redemption', 'consume_pre', 'consume', 'release',
        'reversal', 'admin_adjustment', 'bonus', 'affiliate_transfer', 'checkin_bonus',
        'subscription_purchase'));
