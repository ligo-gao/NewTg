-- ============================================================================
-- audit_logs 保留期清理索引（R-10；platform-01 §5 保留期口径）
--
-- platform-01 §5 规定 audit_logs 行保留 12 个月，由 audit_log_retention worker
-- 夜间分批 DELETE（不归档）；同节「保留期」说明按 (created_at) 扫描，但 §5 的
-- 索引清单只列了 (user_id, created_at DESC)/(category, created_at DESC)/(result)。
-- 缺少 created_at 单列索引时，每次分批 DELETE 都会退化为全表扫描并持有大量行锁，
-- 在 12 个月的审计表上随时间线性恶化，故补建该索引。
--
-- 日志库与主库分离（G7-R36），本索引只属于 migrations-logs 流；非分区表，
-- 索引直接建在 audit_logs 上即可（usage_logs 的 created_at 索引已在 init 中）。
-- ============================================================================

CREATE INDEX audit_logs_created_idx ON audit_logs (created_at);
