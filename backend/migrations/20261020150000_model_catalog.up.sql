-- ============================================================================
-- D1 —— 模型目录与供应商目录（platform-01 §22.7.1 / §22.7.2；
-- modules-06 F-06-22/23/24/26）
--
-- 为什么两张表放同一个迁移：它们是同一业务域（目录）的两张表，model_meta
-- 的 vendor_id 是**逻辑引用**（不设 FK），没有建表顺序依赖，但成对交付便于
-- down 时一次回退整个域。
--
-- 为什么 vendors.id / model_meta.id 用 GENERATED ALWAYS AS IDENTITY：
-- platform-04 §3.1 统一代理主键规则。
--
-- 为什么 vendor_id 不设 FK（D1-02 / D1-07）：与全库「逻辑引用、不设 FK」纪律
-- 一致；更重要的是级联删会把模型档案一起带走，而删除供应商的正确语义是
-- 「有引用即 409，由 merge 先改挂」（F-06-23/F-06-24）。引用保护由应用层在
-- 同一事务内 `SELECT ... FOR UPDATE` 锁供应商行后统计完成。
--
-- 为什么 vendors 的版本不落列（D1-01）：version 是对 7 列规范化后的 sha256
-- 派生值（store.Vendor.Version）。持久化会形成第二真源：任何绕过应用的写入
-- 都会让存储版本与内容脱节，乐观锁随即失真。
--
-- 枚举 CHECK 一律写成 IN (...)：ENUM 对照表（internal/store/enum.go 的
-- EnumChecks）按列登记、双向对账；新列必须同步登记，否则 TestEnumChecksMatchDB 变红。
-- ============================================================================

-- ==== vendors（platform-01 §22.7.2，7 列）====
CREATE TABLE IF NOT EXISTS vendors (
    id          bigint      GENERATED ALWAYS AS IDENTITY,
    name        varchar(128) NOT NULL,
    description text,
    icon        varchar(128),
    status      smallint    NOT NULL DEFAULT 1,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT vendors_pkey PRIMARY KEY (id),
    -- 1=启用 / 2=停用（D1-01；停用行仍占名）
    CONSTRAINT vendors_status_chk CHECK (status IN (1, 2))
);

-- 唯一性按 lower(name)：供应商名「活跃唯一且大小写不敏感」（F-06-23），
-- 且停用行也占名（D1-01）。表达式索引在三库均可表达，不需要例外登记。
CREATE UNIQUE INDEX IF NOT EXISTS vendors_name_lower_uq ON vendors (lower(name));
CREATE INDEX IF NOT EXISTS vendors_status_idx ON vendors (status);

-- ==== model_meta（platform-01 §22.7.1，12 列 + 公开页 8 列 = 20 列）====
--
-- 与 model_pricing 的分工：本表是目录元数据，不是定价；两者不设 FK、不强制
-- 一一对应（「有价无档」「有档无价」都合法）。
CREATE TABLE IF NOT EXISTS model_meta (
    id               bigint       GENERATED ALWAYS AS IDENTITY,
    model_name       varchar(128) NOT NULL,
    display_name     varchar(64),
    description      text,
    icon             varchar(128),
    tags             jsonb        NOT NULL DEFAULT '[]'::jsonb,
    -- 逻辑引用 vendors.id（不设 FK，见文件头）。
    vendor_id        bigint,
    -- 对象形态：{"/v1/chat/completions":["POST"]}（D1 边界 §3）。
    endpoints        jsonb        NOT NULL DEFAULT '{}'::jsonb,
    capabilities     jsonb        NOT NULL DEFAULT '[]'::jsonb,
    modalities       jsonb        NOT NULL DEFAULT '[]'::jsonb,
    parameters       jsonb        NOT NULL DEFAULT '[]'::jsonb,
    context_length   bigint,
    max_output       bigint,
    knowledge_cutoff varchar(7),
    released_at      date,
    -- 1=启用 / 2=停用（停用 ⇒ 广场态恒 hidden）
    status           smallint     NOT NULL DEFAULT 1,
    sync_official    boolean      NOT NULL DEFAULT true,
    -- 0=精确 / 1=前缀 / 2=包含 / 3=后缀（命中优先级见 F-06-22）
    name_rule        smallint     NOT NULL DEFAULT 0,
    created_at       timestamptz  NOT NULL DEFAULT now(),
    updated_at       timestamptz  NOT NULL DEFAULT now(),
    CONSTRAINT model_meta_pkey PRIMARY KEY (id),
    CONSTRAINT model_meta_model_name_uq UNIQUE (model_name),
    -- 字符集必须放行 `/`：公开模型名 openai/gpt-5 已存在（D1-03）。
    CONSTRAINT model_meta_model_name_chk
        CHECK (model_name ~ '^[A-Za-z0-9._:/-]{1,128}$'),
    -- 三个字符串数组列（D1-04）；tags 不建 GIN（D1-05）。
    CONSTRAINT model_meta_tags_chk CHECK (jsonb_typeof(tags) = 'array'),
    CONSTRAINT model_meta_capabilities_chk CHECK (jsonb_typeof(capabilities) = 'array'),
    CONSTRAINT model_meta_modalities_chk CHECK (jsonb_typeof(modalities) = 'array'),
    CONSTRAINT model_meta_parameters_chk CHECK (jsonb_typeof(parameters) = 'array'),
    CONSTRAINT model_meta_context_length_chk CHECK (context_length IS NULL OR context_length >= 0),
    CONSTRAINT model_meta_max_output_chk CHECK (max_output IS NULL OR max_output >= 0),
    -- YYYY-MM（D1-04）；NULL 表示未知。
    CONSTRAINT model_meta_knowledge_cutoff_chk
        CHECK (knowledge_cutoff IS NULL OR knowledge_cutoff ~ '^[0-9]{4}-(0[1-9]|1[0-2])$'),
    CONSTRAINT model_meta_status_chk CHECK (status IN (1, 2)),
    CONSTRAINT model_meta_name_rule_chk CHECK (name_rule IN (0, 1, 2, 3))
);

CREATE INDEX IF NOT EXISTS model_meta_vendor_idx ON model_meta (vendor_id);
CREATE INDEX IF NOT EXISTS model_meta_status_idx ON model_meta (status);
