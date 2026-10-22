-- ============================================================================
-- W8 / MJ-d 复核修复 —— 幂等键上界对齐、claim_token 归属、public_task_id 对齐
--
-- I1：HTTP 冻结 Idempotency-Key 上界 1KiB（mj/spec.go 的
--     MaxIdempotencyKey），初版表却是 varchar(128) + 1..128 CHECK，
--     129–1024 字节的合法键会在 claim 时触发 DB 错误并变成 500。
--     这里把列与 CHECK 一并放宽到 1024，**不收紧 HTTP**（1KiB 是冻结值）。
--
-- I2：租约接管后新旧执行者的 request_hash 可能完全相同，只按
--     (user,key,hash,'in_flight') 收尾会让旧执行者覆写/删除接管者。
--     新增 claim_token：每次 claim（含接管）轮换，Complete/Abandon 必须
--     同时匹配。存量行回填随机 token（它们都是 in_flight/completed 的
--     历史行，回填后旧执行者自然不会持有 token）。
--
-- M8：public_task_id 与 tasks.public_task_id 的 varchar(191) 对齐；本表
--     只存引用，实际生成器远短于 191，对齐避免「同一个 id 两张表不同上界」。
-- ============================================================================

ALTER TABLE task_submit_idempotency ALTER COLUMN idem_key TYPE varchar(1024);
ALTER TABLE task_submit_idempotency DROP CONSTRAINT task_submit_idempotency_key_len_chk;
ALTER TABLE task_submit_idempotency ADD CONSTRAINT task_submit_idempotency_key_len_chk
    CHECK (char_length(idem_key) BETWEEN 1 AND 1024);

ALTER TABLE task_submit_idempotency ALTER COLUMN public_task_id TYPE varchar(191);

-- 存量行回填：md5 只用于迁移期生成非空随机 token，不是安全凭据（真正的
-- claim token 由应用层 crypto/rand 生成，32 hex 字符）。
ALTER TABLE task_submit_idempotency ADD COLUMN claim_token text;
UPDATE task_submit_idempotency
   SET claim_token = md5(random()::text || clock_timestamp()::text || id::text);
ALTER TABLE task_submit_idempotency ALTER COLUMN claim_token SET NOT NULL;
ALTER TABLE task_submit_idempotency ADD CONSTRAINT task_submit_idempotency_claim_token_len_chk
    CHECK (char_length(claim_token) BETWEEN 16 AND 128);
