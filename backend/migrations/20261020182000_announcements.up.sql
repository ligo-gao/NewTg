-- ============================================================================
-- D7a —— 站点公告（modules-08 F-08-04 / R-D7-01 / R-D7-02）
--
-- 两张表：
--   announcements     公告主表，uuid 代理主键；
--   announcement_reads 用户维度已读记录（复合主键 user_id + announcement_id）。
--
-- 为什么 content 的应用层上限是 16KB、DB CHECK 却是 65536 字节（R-D7-01）：
-- 设计稿冻结的是「≤16KB」（用户可见的编辑器计数），DB 只做**兜底**——
-- 把 CHECK 卡在 16384 会让一次合法的多字节字符边界计算差异（前端按字符、
-- 后端按字节）直接变成 500/422 抖动。应用层按字节 16384 拒绝并给出字段错误，
-- DB 的 65536 只防「绕过 API 直接写库」把 text 列变成内存放大器。
--
-- 为什么 expire_at 用 CHECK 而不是触发器：expire_at 是公告自己的时间语义
-- （生效早于过期），只与同一行的 publish_at 比较，单行 CHECK 足够；跨行/
-- 跨表的一致性（例如已过期公告不得再发布）属于应用层状态机，不进 DDL。
--
-- 为什么状态只有三值（draft/published/offline）而 UI 有四个徽章：
-- 「已过期 expired」是**虚拟态**（R-D7-10）——DB 仍为 published，
-- 仅由 expire_at <= now 派生。落库会引入一个需要 worker 迁移的状态，
-- 而时间流逝是纯函数，不需要后台任务，也不会出现「worker 停摆导致过期
-- 公告继续展示」的故障面。
--
-- 为什么不设 FK（R-D7-02 / platform-04 §3.4 V5）：announcement_reads 的
-- user_id 是逻辑引用 users.id；管理员/用户可被硬删除，已读记录随公告删除
-- 同事务清理（repo 的 Delete 实现），不依赖级联约束。
-- ============================================================================

-- ==== 1. announcements ====
CREATE TABLE IF NOT EXISTS announcements (
    id           uuid        NOT NULL DEFAULT gen_random_uuid(),
    title        varchar(80) NOT NULL,
    category     text        NOT NULL DEFAULT 'notice',
    content      text        NOT NULL,
    publish_at   timestamptz NOT NULL DEFAULT now(),
    expire_at    timestamptz,
    status       text        NOT NULL DEFAULT 'draft',
    created_by   bigint      NOT NULL,
    updated_by   bigint      NOT NULL,
    published_at timestamptz,
    offline_at   timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT announcements_pkey PRIMARY KEY (id),
    -- 约束命名口径：枚举 CHECK 一律 <table>_<column>_chk，供 EnumChecks
    -- 反向对账（internal/store/enum.go）；非枚举约束用语义名，不参与对账。
    CONSTRAINT announcements_category_chk
        CHECK (category IN ('notice', 'maintenance', 'release')),
    CONSTRAINT announcements_status_chk
        CHECK (status IN ('draft', 'published', 'offline')),
    CONSTRAINT announcements_title_len_chk
        CHECK (char_length(btrim(title)) BETWEEN 1 AND 80),
    -- 16KB 是应用层口径；这里放宽到 64KiB 只做硬兜底（见文件头）。
    CONSTRAINT announcements_content_len_chk
        CHECK (octet_length(content) <= 65536),
    CONSTRAINT announcements_expire_after_publish_chk
        CHECK (expire_at IS NULL OR expire_at > publish_at)
);

-- 列表页的常态查询：按状态过滤 + publish_at 倒序分页（R-D7-06 排序口径）。
CREATE INDEX IF NOT EXISTS announcements_status_publish_idx
    ON announcements (status, publish_at DESC);

-- 过期扫描/虚拟态筛选是部分索引：只关心 published 行，历史 draft/offline
-- 不进入索引，避免索引体积随删除/下线历史无限增长。
CREATE INDEX IF NOT EXISTS announcements_expire_idx
    ON announcements (expire_at) WHERE status = 'published';

-- ==== 2. announcement_reads（R-D7-02）====
-- 复合主键即幂等键：同一用户对同一公告至多一行，重复标记已读由
-- ON CONFLICT DO NOTHING 收敛（R-D7-05 的 read 幂等）。
CREATE TABLE IF NOT EXISTS announcement_reads (
    user_id         bigint      NOT NULL,
    announcement_id uuid        NOT NULL,
    read_at         timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT announcement_reads_pkey PRIMARY KEY (user_id, announcement_id)
);
