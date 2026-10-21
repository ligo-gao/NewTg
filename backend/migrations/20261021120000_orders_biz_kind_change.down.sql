-- change 订单是支付事实（可能已 succeeded 并改写过订阅的套餐/额度），不能随
-- 迁移回滚删除。存在这类行时 ADD CONSTRAINT 会失败，这是有意的 fail-loud：
-- 先人工归档/对账，再决定是否回滚代码与约束。
--
-- 失败会让 schema_migrations 中该版本保持 dirty，需先
-- `migrate force <version>` 把版本戳回上一个已应用版本，处理后重跑。
ALTER TABLE orders DROP CONSTRAINT IF EXISTS orders_biz_kind_chk;

ALTER TABLE orders ADD CONSTRAINT orders_biz_kind_chk
    CHECK (biz_kind IN ('topup', 'subscribe', 'renew'));
