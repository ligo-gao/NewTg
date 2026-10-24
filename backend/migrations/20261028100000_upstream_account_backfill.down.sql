-- 回退出 20261028100000_upstream_account_backfill（设计 §12.1/§12.4）。
--
-- 目标：把升级时自动建的账号和归属撤回，但不破坏升级后已经积累的对账数据。
--
-- 1) 只清除指向「带溯源 note 的账号」的归属链接。管理员自己建/自己关联的
--    账号与链接不动（列本身由更早的 20261028090000 down 负责删除）。
UPDATE channels c
SET upstream_account_id = NULL
FROM upstream_accounts a
WHERE c.upstream_account_id = a.id
  AND a.note LIKE 'migrated from channel #%';

-- 2) 只删除「没有快照、也没有对账 run」的溯源账号。有历史数据的账号必须保留：
--    upstream_usage_snapshots / upstream_reconciliation_runs 以 ON DELETE RESTRICT
--    引用账号，硬删会中止 down；而数据本身是升级后的真实对账记录，丢弃不可接受。
--    因此带历史的迁移账号在 down 之后**有意留下**（link 已清空，控制台显示为
--    未归属，管理员可自行处理）；这也是 down 在已有对账数据的库上仍能成功的原因。
DELETE FROM upstream_accounts a
WHERE a.note LIKE 'migrated from channel #%'
  AND NOT EXISTS (
      SELECT 1 FROM upstream_usage_snapshots s WHERE s.account_id = a.id)
  AND NOT EXISTS (
      SELECT 1 FROM upstream_reconciliation_runs r WHERE r.account_id = a.id);
