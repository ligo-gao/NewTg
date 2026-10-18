// 本文件是 internal/clientip 在 httpapi 包内的薄适配层。
//
// 解析算法、配置与安全边界（空清单不信任 XFF、从右向左跳过可信代理、
// 禁止 /0 与过宽前缀）统一实现在 internal/clientip；rbac 的风控上下文与
// auth 的 Turnstile remoteip 复用同一实现，避免三个消费点结果漂移
// （security-07 §2.4「唯一取值路径」）。
package httpapi

import (
	"net/http"
	"net/netip"

	"newtg/gateway/internal/clientip"
)

// trustedProxiesEnv 与 config.Config 的 TRUSTED_PROXIES 保持同一来源。
const trustedProxiesEnv = clientip.EnvVar

// clientIP 是包内共享入口：所有 IP 维限流桶与审计字段都经它取得客户端地址。
func clientIP(r *http.Request) string {
	return clientip.ClientIP(r)
}

// resolveClientIP 是显式信任列表的入口（测试与按请求覆盖）。
func resolveClientIP(r *http.Request, trustedProxies []netip.Prefix) string {
	return clientip.Resolve(r, trustedProxies)
}

// ConfigureTrustedProxies 供路由装配方在开始服务前用 cfg.TrustedProxies
// 调用一次。未调用时 clientip 初始化会读取同一环境变量 TRUSTED_PROXIES，
// 因此未接线的部署仍遵循文档配置。畸形/过宽条目被忽略并返回错误。
func ConfigureTrustedProxies(raw string) error {
	return clientip.Configure(raw)
}
