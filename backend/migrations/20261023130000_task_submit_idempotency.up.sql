-- ============================================================================
-- W8 / MJ-d —— 任务提交幂等键表（R-MJ-06 第二步）
--
-- 依据：relay-00 §5.4（提交幂等键 vs 计费幂等键，两者不得混用）、
--       relay-05 §6/§10（客户端可重放提交、24h 窗口）、
--       mj-contract-research.md R-MJ-06 备选 A：
--       (user_id, key) 唯一，保存请求哈希与首次结果。
--
-- 【为什么不是 tasks 上的列】幂等键在**任务行之前**就要落库：没有它，两个
-- 并发的同键请求会各自提交上游、各自预扣，事后再想合并已经产生了两条真实
-- 任务与两笔资金。独立的 claim 行让「同键只允许一个执行者」成为一条唯一
-- 约束，而不是应用层的竞态检查。
--
-- 【为什么与 request_id 分开】request_id 是服务端计费幂等锚点（wallet_
-- transactions 用它做唯一收敛）；把客户端键塞进计费锚点会让重放请求命中
-- 别人的结算会话（relay-00 §5.4 决议 J9）。
--
-- 24h 窗口由 expires_at 承载。过期行不参与重放判定，可被后续同键请求
-- 整体覆写（保留一行，避免表随键无限增长）。
-- ============================================================================

CREATE TABLE task_submit_idempotency (
    id            bigint      GENERATED ALWAYS AS IDENTITY,
    user_id       bigint      NOT NULL,
    idem_key      varchar(128) NOT NULL,
    request_hash  bytea       NOT NULL,
    endpoint      text        NOT NULL,
    status        text        NOT NULL DEFAULT 'in_flight',
    public_task_id varchar(64),
    -- 首次结果的完整 {code,description,result} 信封。重放必须原样返回首次
    -- 结果（包括 upload 的数组形 result），只靠 public_task_id 再查 tasks
    -- 会丢失「首次响应长什么样」以及 code 21 复用的情形。
    --
    -- 用 text 而不是 jsonb：jsonb 会规范化键序/空白，重放体就不再与首次
    -- 响应逐字节一致。写入前仍由仓储校验 json.Valid，不接受任意文本。
    response_body text,
    claimed_at    timestamptz NOT NULL DEFAULT now(),
    created_at    timestamptz NOT NULL DEFAULT now(),
    expires_at    timestamptz NOT NULL,
    CONSTRAINT task_submit_idempotency_pkey PRIMARY KEY (id),
    CONSTRAINT task_submit_idempotency_user_key_uq UNIQUE (user_id, idem_key),
    CONSTRAINT task_submit_idempotency_status_chk CHECK (status IN ('in_flight', 'completed')),
    CONSTRAINT task_submit_idempotency_key_len_chk CHECK (char_length(idem_key) BETWEEN 1 AND 128),
    CONSTRAINT task_submit_idempotency_hash_len_chk CHECK (octet_length(request_hash) = 32),
    CONSTRAINT task_submit_idempotency_completed_chk
        CHECK (status <> 'completed' OR (public_task_id IS NOT NULL AND response_body IS NOT NULL))
);

-- 过期行清理（worker/运维）按 expires_at 扫描；claim 覆写走唯一键，不需要
-- 第二个索引。
CREATE INDEX task_submit_idempotency_expires_idx ON task_submit_idempotency (expires_at);
