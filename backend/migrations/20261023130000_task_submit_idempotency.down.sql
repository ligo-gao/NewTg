-- W8 / MJ-d 回滚：删除任务提交幂等键表。
--
-- 该表只承载客户端重放去重，不参与计费（计费锚点是 request_id）；删除不会
-- 改变任何已发生资金。回滚后的行为退化为 MJ-d 之前：同键重放不再去重，
-- 客户端重试可能重复建任务/扣费——这是旧二进制的已知语义。
DROP TABLE IF EXISTS task_submit_idempotency;
