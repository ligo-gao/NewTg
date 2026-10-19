-- ============================================================================
-- M6d Task 1 —— 任务插件三表（modules-08 §3.10.2 B-11/B-12/B-13）
--
-- 表间关系：task_plugins 是插件主表（自然键 "key"），task_plugin_versions
-- 是它的版本行（一对一 FK，级联删除），plugin_marketplace_sources 独立。
--
-- 为什么 key 上限是 30（R-M6D-01）：relay-06 §1.1 的平台 key 口径比
-- modules-08 §3.10.2 的 ≤64 更严（key 会进入 URL 路径、路由索引与前端
-- 选择器），冻结取两者交集 30；DB CHECK 与应用层校验同值，避免「应用层
-- 收 64、DB 收 64」而路由层按 30 截断的静默漂移。
--
-- 为什么 source_text 而不是 source（R-M6D-04）：B-11 的 `source` 列是
-- {builtin,upload} 枚举，B-12 的同名设计列却是源码文本；同名异义会让
-- 「source 字段」在两层之间产生歧义。版本行改用 source_text 后，读代码
-- 即可区分枚举与正文，也避免 GORM 模型/查询别名互相污染。
--
-- 为什么 active 用部分唯一索引（R-M6D-05）：同一 key 至多一行 active 是
-- S4「不允许出现无 active 版本的 key」的数据库兜底。普通 UNIQUE(key)
-- 会禁止历史版本行；UNIQUE(key) WHERE active 只约束 active=true 的
-- 子集，恰好表达「至多一个活跃版本」，且并发激活时由 PG 保证串行化冲突
-- 而不是依赖应用层先查后写（Check-then-Act 的竞态窗口）。
--
-- 为什么 uploaded_by 是 bigint（R-M6D-11）：users.id 是 bigint；设计稿
-- 曾写 uuid，按实现侧用户主键口径冻结为 bigint，且**不设 FK**——
-- 管理员账号可被硬删除/停用，上传审计要保留（platform-04 §3.4 V5：
-- 逻辑引用，不级联删除版本行）。
--
-- 约束命名口径：枚举 CHECK 一律 <table>_<column>_chk，供 EnumChecks 反向
-- 对账（internal/store/enum.go）按名称定位；长度/形态 CHECK 用 *_len_chk /
-- *_chk 的语义名，不参与枚举对账。
-- ============================================================================

-- ==== 1. task_plugins（插件主表，B-11）====
-- "key" 是保留字，DDL 与查询文本一律双引号引用（platform-04 §3.7 V8）。
CREATE TABLE IF NOT EXISTS task_plugins (
    "key"          text        NOT NULL,
    name           text        NOT NULL,
    icon           text,
    source         text        NOT NULL,
    builtin        boolean     NOT NULL DEFAULT false,
    enabled        boolean     NOT NULL DEFAULT true,
    active_version text,
    description    text,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT task_plugins_pkey PRIMARY KEY ("key"),
    -- key 形态：小写字母/数字/连字符，1–30（R-M6D-01）。字符集同时是
    -- 路由段的合法集合，DB 与路由索引共用同一断言。
    CONSTRAINT task_plugins_key_chk
        CHECK ("key" ~ '^[a-z0-9-]{1,30}$'),
    CONSTRAINT task_plugins_name_len_chk
        CHECK (char_length(name) <= 128),
    CONSTRAINT task_plugins_source_chk
        CHECK (source IN ('builtin', 'upload')),
    CONSTRAINT task_plugins_active_version_len_chk
        CHECK (active_version IS NULL OR char_length(active_version) <= 32),
    CONSTRAINT task_plugins_description_len_chk
        CHECK (description IS NULL OR char_length(description) <= 1024)
);

-- ==== 2. task_plugin_versions（版本行，B-12）====
-- 版本行的完整源码落 source_text，**不对列表接口下发**（S5/源查看器按需
-- 只读拉取）；source_hash 是 64 位小写 hex 的 SHA-256，市场包体完整性
-- （S3）与 30s 快照比对都以此为准。
CREATE TABLE IF NOT EXISTS task_plugin_versions (
    id          uuid        NOT NULL DEFAULT gen_random_uuid(),
    "key"       text        NOT NULL,
    version     text        NOT NULL,
    api_version text        NOT NULL DEFAULT 'v1',
    source_text text        NOT NULL,
    source_hash text        NOT NULL,
    icon        text,
    enabled     boolean     NOT NULL DEFAULT true,
    active      boolean     NOT NULL DEFAULT false,
    uploaded_by bigint      NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT task_plugin_versions_pkey PRIMARY KEY (id),
    CONSTRAINT task_plugin_versions_key_fk
        FOREIGN KEY ("key") REFERENCES task_plugins ("key") ON DELETE CASCADE,
    -- 同 key+version 唯一（relay-06 §2.4）：异源同版本在应用层先比对
    -- source_hash 再拒绝，DB 兜底防重复行。
    CONSTRAINT task_plugin_versions_key_version_uq UNIQUE ("key", version),
    CONSTRAINT task_plugin_versions_version_len_chk
        CHECK (char_length(version) BETWEEN 1 AND 32),
    -- 刻意写成 = ANY (ARRAY[...]) 而不是 IN ('v1')：PG 会把单元素 IN
    -- 规范化成 `api_version = 'v1'::text`，EnumChecks 的枚举解析（依赖
    -- ANY/ARRAY 形状）无法识别，反向对账会误报「DB 枚举 CHECK 未登记」。
    -- 语义与 IN 相同，只是保留数组形状供 schema 断言解析。
    CONSTRAINT task_plugin_versions_api_version_chk
        CHECK (api_version = ANY (ARRAY['v1'::text])),
    CONSTRAINT task_plugin_versions_source_hash_chk
        CHECK (source_hash ~ '^[0-9a-f]{64}$')
);

-- 至多一个 active 版本（S4 / R-M6D-05）。见文件头「为什么 active 用部分
-- 唯一索引」。名称钉死，schema 集成测试按名断言。
CREATE UNIQUE INDEX IF NOT EXISTS task_plugin_versions_active_key_uq
    ON task_plugin_versions ("key") WHERE active;

-- 版本列表与「删除 active 后提升上一版本」按 key + 创建时间倒序取，
-- 与 modules-08 §3.10.2 的排序依据一致。
CREATE INDEX IF NOT EXISTS task_plugin_versions_key_created_idx
    ON task_plugin_versions ("key", created_at DESC);

-- ==== 3. plugin_marketplace_sources（市场源，B-13）====
-- R-M6D-03：MVP 仅接受 https（F-08-34 的 http 例外不实现）；URL 出站
-- 另走 sec.ssrf.* 守卫，DB 只兜底 scheme。UNIQUE(url) 防止同一索引重复
-- 聚合。last_error 截断 ≤512，不存响应体原文（避免把远端内容写进库）。
CREATE TABLE IF NOT EXISTS plugin_marketplace_sources (
    id                uuid        NOT NULL DEFAULT gen_random_uuid(),
    name              text        NOT NULL,
    url               text        NOT NULL,
    enabled           boolean     NOT NULL DEFAULT true,
    last_check_status text        NOT NULL DEFAULT 'pending',
    last_check_at     timestamptz,
    last_error        text,
    CONSTRAINT plugin_marketplace_sources_pkey PRIMARY KEY (id),
    CONSTRAINT plugin_marketplace_sources_url_uq UNIQUE (url),
    CONSTRAINT plugin_marketplace_sources_name_len_chk
        CHECK (char_length(name) <= 64),
    CONSTRAINT plugin_marketplace_sources_url_chk
        CHECK (url ~ '^https://'),
    CONSTRAINT plugin_marketplace_sources_last_check_status_chk
        CHECK (last_check_status IN ('pending', 'ok', 'failed')),
    CONSTRAINT plugin_marketplace_sources_last_error_len_chk
        CHECK (last_error IS NULL OR char_length(last_error) <= 512)
);
