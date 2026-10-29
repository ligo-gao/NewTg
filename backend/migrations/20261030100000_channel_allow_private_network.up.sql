-- 渠道私网出站 opt-in（R-01：channels base_url 的 SSRF 双闸，D1 裁决）。
--
-- 背景：渠道 base_url 由管理员填写，中继/体检/测速/模型目录都会向它出站。
-- 出站统一经 internal/egress 的地址分类（解析一次、校验全部候选 IP、再拨
-- 校验过的 IP）。RFC1918/ULA 默认拒绝；只有本列为 true 的渠道才放宽到
-- 私网——而 loopback、链路本地（含 169.254.0.0/16 云元数据）、CGNAT、
-- 保留段**任何取值都不放行**。
--
-- 默认 false 是安全默认值：升级后指向内网的自建上游会立刻被拒绝，直到
-- 管理员在渠道上显式开启。这是有意的收紧，发版说明必须写明。
ALTER TABLE channels ADD COLUMN allow_private_network boolean NOT NULL DEFAULT false;

COMMENT ON COLUMN channels.allow_private_network IS '允许该渠道访问私网（RFC1918/ULA）上游；loopback/链路本地/元数据/保留段不受本列影响';
