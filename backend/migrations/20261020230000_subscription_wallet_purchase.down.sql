-- 回退 P2b 的两个枚举 CHECK。
--
-- fail-loud：余额购买的流水与订单是**已发生的资金/履约事实**，不能随迁移
-- 回滚删除。存在 'subscription_purchase' 流水或 provider='wallet' 订单时
-- 显式报错，要求先人工归档/对账再决定回滚代码与约束——而不是让随后的
-- ADD CONSTRAINT 以难读的 SQLSTATE 失败，或静默丢行。
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM wallet_transactions WHERE type = 'subscription_purchase') THEN
        RAISE EXCEPTION
            'cannot roll back 20261020230000: wallet_transactions rows with type=subscription_purchase exist; archive/reconcile them first';
    END IF;
    IF EXISTS (SELECT 1 FROM orders WHERE provider = 'wallet') THEN
        RAISE EXCEPTION
            'cannot roll back 20261020230000: orders with provider=wallet exist; archive/reconcile them first';
    END IF;
END $$;

ALTER TABLE wallet_transactions DROP CONSTRAINT IF EXISTS wallet_transactions_type_chk;

ALTER TABLE wallet_transactions ADD CONSTRAINT wallet_transactions_type_chk
    CHECK (type IN (
        'topup', 'redemption', 'consume_pre', 'consume', 'release',
        'reversal', 'admin_adjustment', 'bonus', 'affiliate_transfer', 'checkin_bonus'));

ALTER TABLE orders DROP CONSTRAINT IF EXISTS orders_provider_chk;

ALTER TABLE orders ADD CONSTRAINT orders_provider_chk
    CHECK (provider IN ('stripe', 'alipay', 'creem', 'waffo', 'waffo_pancake', 'epay'));
