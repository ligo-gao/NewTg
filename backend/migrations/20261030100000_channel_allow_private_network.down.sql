-- 回滚 20261030100000_channel_allow_private_network。
--
-- 纯增列迁移：回滚丢弃 opt-in 名单，重新升级后默认 false，需重新开启。
ALTER TABLE channels DROP COLUMN IF EXISTS allow_private_network;
