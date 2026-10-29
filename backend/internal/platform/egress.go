package platform

import "newtg/gateway/internal/egress"

// channelEgressGuard 构造渠道出站的统一执行器（R-01 / security-04 §2.4）。
//
// 渠道面策略（egress.ChannelPolicy）与 webhook/插件面不同：不套语法域名
// 清单、不限制端口、不允许用 sec.ssrf.mode=off 关闭；私网默认拒绝，只有
// channels.allow_private_network=true 的渠道在单次请求上下文中放宽。
//
// cfg 为 nil（探针/降级装配）时按注册表默认值 fail-closed，不会退化成
// 「不校验」。策略在装配期读取一次：sec.ssrf.* 的变更对渠道面需要重启
// 生效（渠道面只在乎 basic/strict 强度与连接超时）。
func channelEgressGuard(cfg egress.ConfigReader) *egress.Guard {
	return egress.NewGuard(egress.ChannelPolicy(cfg))
}
