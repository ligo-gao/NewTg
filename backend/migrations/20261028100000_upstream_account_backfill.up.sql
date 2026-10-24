-- 一次性回填：每个现有渠道建一个同名上游账号并回填归属（设计 §12.1）。
--
-- 升级路径上 `channels` 已有数据而 `upstream_accounts` 还是空表：这里为
-- 每个尚未归属的渠道建一个可独立对账的账期主体，provider 取 channels.type
-- （Tier-2 不校验，这里原样落库），币种/时区用设计规定的默认值。
-- note 写入来源渠道，作为后续 down 的溯源标记（见 down 文件）。
--
-- 跳过 type 为空或超过 varchar(64) 的渠道：upstream_accounts.provider 是
-- varchar(64)，超长会中止整个升级。这些渠道保持 upstream_account_id = NULL，
-- 控制台显示为「未归属」，由管理员手工建账号后关联，好过升级失败。
--
-- NOT EXISTS 溯源护栏：一个渠道最多只能有一个本次迁移建的账号。down 会保留
-- 有历史数据的迁移账号，若之后渠道改名再前滚，按名字去重挡不住第二个账号
--（名字已不同），同一个渠道 id 会出现两个同 provider 的候选，归属就不可判定。
INSERT INTO upstream_accounts (name, provider, timezone, note)
SELECT c.name, c.type, 'Asia/Shanghai', 'migrated from channel #' || c.id
FROM channels c
WHERE c.upstream_account_id IS NULL
  AND length(c.type) BETWEEN 1 AND 64
  AND NOT EXISTS (
    SELECT 1 FROM upstream_accounts a
    WHERE a.note = 'migrated from channel #' || c.id
  )
ON CONFLICT (name) DO NOTHING;

-- 回填分两趟，顺序即优先级，且每一趟都不可能匹配到多个账号：
--
-- 1) 溯源优先：note 精确指出来源渠道 id，即本次迁移为该渠道建的账号。
--    上面 INSERT 的 NOT EXISTS 保证一个渠道 id 至多一个这样的账号。
UPDATE channels c
SET upstream_account_id = a.id
FROM upstream_accounts a
WHERE c.upstream_account_id IS NULL
  AND a.provider = c.type
  AND a.note = 'migrated from channel #' || c.id;

-- 2) 同名兜底：只针对管理员在迁移之前就建好的账号。必须同时校验 provider ——
--    否则管理员预建的同名账号（例如 provider = 'anthropic' 的 openai-prod）会
--    吞掉 openai 渠道，之后拿错误的上游控制台账单参与对账。
--    upstream_accounts_name_uq 唯一，本趟对同一渠道至多匹配一个账号。
UPDATE channels c
SET upstream_account_id = a.id
FROM upstream_accounts a
WHERE c.upstream_account_id IS NULL
  AND a.provider = c.type
  AND a.name = c.name;
