-- 签到入账的 DB 幂等锚点（modules-03 F-03-12 / 模块 01 I-18；W15-C）。
--
-- 一次签到恰好一行 checkin_bonus 流水：reference_type='checkin'，
-- reference_id = checkins.id。应用层在**同一事务**内先写签到行、再用新
-- id 写流水；本部分唯一索引是第二道防线——任何重复入账（重放/并发/未来
-- 新路径）都会被数据库以 23505 拒绝，而不是在账本里留下两行。
--
-- 命名与 uq_wallet_tx_signup_grant 同一惯例（init.up.sql §10）。
CREATE UNIQUE INDEX uq_wallet_tx_checkin
    ON wallet_transactions (reference_id)
    WHERE reference_type = 'checkin';
