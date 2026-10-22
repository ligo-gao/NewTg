-- 回退支付设置合规确认列（modules-08 §3.4）。审计行是 append-only，不回退。
ALTER TABLE users DROP COLUMN IF EXISTS payment_compliance_confirmed_at;
