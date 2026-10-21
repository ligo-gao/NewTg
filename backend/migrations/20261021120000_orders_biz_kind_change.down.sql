-- change 订单是支付事实（可能已 succeeded 并改写过订阅的套餐/额度），不能随
-- 迁移回滚删除。存在这类行时 ADD CONSTRAINT 会失败，这是有意的 fail-loud：
-- 先人工归档/对账，再决定是否回滚代码与约束。
ALTER TABLE orders DROP CONSTRAINT IF EXISTS orders_biz_kind_chk;

ALTER TABLE orders ADD CONSTRAINT orders_biz_kind_chk
    CHECK (biz_kind IN ('topup', 'subscribe', 'renew'));
