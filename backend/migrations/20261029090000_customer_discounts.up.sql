-- 客户折扣规则（docs/superpowers/specs/2026-10-26-customer-discounts-design.md §4）。
--
-- 【为什么 id 用 GENERATED ALWAYS AS IDENTITY 而不是 bigserial】平台 DDL
-- 规范（platform-04 §3.1）统一用 identity 作单列代理主键。bigserial 建的是
-- 序列 default，information_schema.is_identity = NO，会被主库 schema 契约
-- （wantIdentityPKTables）判为漂移；identity 还拒绝未经 OVERRIDING 的显式
-- id 写入，主键分配只有一条路径，因此按 house style 落地。
--
-- 【为什么 discounts 不设外键】目标既可能是 users.id（十进制文本）也可能是
-- user_groups.name，单列无法同时引用两张表。删除用户/分组、分组改名由应用层
-- 在同一事务内级联（spec §4.5），规则本身是配置而不是资金事实。
--
-- 【为什么 request_pricing 的两列都带默认值】默认 1 / '' 表示无折扣，存量行
-- 天然兼容，不需要回填；结算只读冻结值，不会回源查询 discounts。
--
-- 【为什么 rate_percent 用 smallint + CHECK】实收百分比是 0–100 的整数，
-- 浮点比例（0.85）在 DB 里会引入二进制舍入，账单金额应以百分数为准。
-- target_id 的形态由 discounts_target_shape_chk 兜底：user 必须是正整数
-- 十进制文本，group 必须非空。

ALTER TABLE users ADD COLUMN customer_type text;
UPDATE users SET customer_type = 'general' WHERE customer_type IS NULL;
ALTER TABLE users ALTER COLUMN customer_type SET DEFAULT 'general';
ALTER TABLE users ALTER COLUMN customer_type SET NOT NULL;
ALTER TABLE users ADD CONSTRAINT users_customer_type_chk
  CHECK (customer_type IN ('general', 'enterprise'));
CREATE INDEX users_customer_type_idx ON users (customer_type);

CREATE TABLE discounts (
    id            bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    model_name    text        NOT NULL,
    target_type   text        NOT NULL,
    target_id     text        NOT NULL,
    rate_percent  smallint    NOT NULL,
    valid_from    timestamptz,
    valid_until   timestamptz,
    enabled       boolean     NOT NULL DEFAULT true,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT discounts_target_type_chk CHECK (target_type IN ('user', 'group')),
    CONSTRAINT discounts_rate_chk CHECK (rate_percent BETWEEN 0 AND 100),
    CONSTRAINT discounts_target_shape_chk CHECK (
      (target_type = 'user'  AND target_id ~ '^[1-9][0-9]*$') OR
      (target_type = 'group' AND target_id <> '')
    ),
    CONSTRAINT discounts_window_chk
      CHECK (valid_from IS NULL OR valid_until IS NULL OR valid_until > valid_from),
    CONSTRAINT discounts_model_chk CHECK (model_name <> '' AND length(model_name) <= 200)
);
CREATE UNIQUE INDEX discounts_rule_uq ON discounts (model_name, target_type, target_id);
CREATE INDEX discounts_lookup_idx ON discounts (model_name) WHERE enabled;
CREATE INDEX discounts_target_idx ON discounts (target_type, target_id);

ALTER TABLE request_pricing
  ADD COLUMN discount_rate   double precision NOT NULL DEFAULT 1,
  ADD COLUMN discount_target text NOT NULL DEFAULT '';
