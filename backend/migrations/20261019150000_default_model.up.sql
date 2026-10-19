-- ============================================================================
-- M6c Task 13 —— 用户默认模型（modules-02 F-02-18 / §2.14）
--
-- 为什么是 users 列而不是独立表：默认模型是「用户级单值偏好」，与
-- language/sidebar_modules 同为 users 行的设置位，读写走同一个
-- PUT /api/self/setting 乐观锁（updated_at），不引入第二套并发控制。
--
-- 值域约束只做到长度（≤128，与 model_pricing.model varchar(128) 对齐）；
-- 「模型必须属于用户可用分组」是跨表事实，DB 无法在不引入触发器/外键的
-- 前提下表达，由 handler 在写入前校验（不可用 → 422 user.default_model_invalid）。
--
-- 空串不落库：应用层把「清除」归一为 NULL；CHAR_LENGTH >= 1 的 CHECK 是
-- 防止绕过该归一化后把空串当成「已设置」读回来（空串在 UI 上无法区分）。
-- ============================================================================

ALTER TABLE users ADD COLUMN default_model text;

ALTER TABLE users ADD CONSTRAINT users_default_model_len_chk
    CHECK (default_model IS NULL OR char_length(default_model) BETWEEN 1 AND 128);
