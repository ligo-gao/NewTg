-- refund_required 订单是「钱已收、待退款」的对账事实，不能随迁移回滚删除或
-- 改写成别的状态。存在这类行时 ADD CONSTRAINT 会失败，这是有意的 fail-loud：
-- 它们必须先由退款流程处理完（或人工归档）再回滚。
--
-- 注意：ADD CONSTRAINT 失败会让本次迁移失败、schema_migrations 中该版本保持
-- dirty，需要用 `go run ./cmd/newtg migrate force <version>`（或对应 migrate
-- force 命令）把版本戳回滚到上一个已应用版本，处理完 refund_required 行后
-- 再重跑迁移。
ALTER TABLE orders DROP CONSTRAINT IF EXISTS orders_status_chk;

ALTER TABLE orders ADD CONSTRAINT orders_status_chk
    CHECK (status IN ('pending', 'succeeded', 'failed', 'expired', 'cancelled'));
