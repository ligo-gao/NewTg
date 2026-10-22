-- W8 / MJ-d 复核修复回滚。
--
-- 【破坏性】收窄 idem_key 前必须删除超长键行，否则 ALTER TYPE 失败。删除的
-- 只是「初版 schema 本应拒绝」的记录（客户端重放去重事实，不参与计费）。
-- claim_token 直接丢弃：回滚后的旧二进制不读该列。
DELETE FROM task_submit_idempotency WHERE char_length(idem_key) > 128;

ALTER TABLE task_submit_idempotency DROP CONSTRAINT IF EXISTS task_submit_idempotency_claim_token_len_chk;
ALTER TABLE task_submit_idempotency DROP COLUMN IF EXISTS claim_token;

ALTER TABLE task_submit_idempotency ALTER COLUMN public_task_id TYPE varchar(64);

ALTER TABLE task_submit_idempotency DROP CONSTRAINT IF EXISTS task_submit_idempotency_key_len_chk;
ALTER TABLE task_submit_idempotency ALTER COLUMN idem_key TYPE varchar(128);
ALTER TABLE task_submit_idempotency ADD CONSTRAINT task_submit_idempotency_key_len_chk
    CHECK (char_length(idem_key) BETWEEN 1 AND 128);
