// Package clientip 实现 security-07 §2.4 的客户端标识信任边界（TRUSTED_PROXIES）。
//
// 唯一取值路径：所有 IP 维限流桶（B1/B3/B4/B5/B8/B9/B17）、风控上下文与
// Turnstile remoteip 都经本包解析，避免边缘部署下三个消费点各自解析、结果漂移。
//
// 解析规则：
//   - 仅当直连 peer（RemoteAddr）属于 TRUSTED_PROXIES 时才采信
//     X-Forwarded-For；否则一律使用 RemoteAddr（空清单 = 永不信任头）。
//   - 采信时从右向左遍历 XFF（右侧是最近的代理），跳过可信代理，第一个
//     不可信地址即客户端。伪造在左侧的条目不生效。
//   - XFF 中出现任何无法解析的条目时整体回退直连 peer（fail-closed）：
//     半信半疑地挑一个「合法」条目会让攻击者用垃圾前缀影响结果。
//   - 信任清单里的畸形/过宽条目被忽略（不因单条写错而信任全部）。按
//     R-07-8：禁止 0.0.0.0/0、::/0；IPv4 前缀不得短于 /8、IPv6 不得短于 /16。
//     生产必须显式填 edge 网段。
//
// 配置来源：config.Config.TrustedProxies（底层环境变量 TRUSTED_PROXIES）。
// 路由装配方应在开始服务前调用 Configure(cfg.TrustedProxies)；未接线时包
// 初始化直接读取同一环境变量，保证两种装配方式语义一致。
package clientip

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"sync/atomic"
)

// EnvVar 与 config.Config 的 TRUSTED_PROXIES 保持同一来源。
const EnvVar = "TRUSTED_PROXIES"

// 最小前缀长度（R-07-8）。比这更宽的前缀（含 /0）会让几乎所有来源变成
// 「可信代理」，攻击者可自带 X-Forwarded-For 逐个 IP 绕过限流。
const (
	minIPv4PrefixBits = 8
	minIPv6PrefixBits = 16
)

// proxySet 是不可变解析结果；通过 atomic.Pointer 整体替换，允许装配期/测试
// 并发读取而不加锁。
type proxySet struct {
	prefixes []netip.Prefix
}

var configured atomic.Pointer[proxySet]

func init() {
	// 与 config.load 相同：逗号分隔的 CIDR/IP。畸形条目被忽略（fail-closed）。
	prefixes, _ := Parse(os.Getenv(EnvVar))
	configured.Store(&proxySet{prefixes: prefixes})
}

// Configure 解析并安装 TRUSTED_PROXIES 列表。合法条目立即生效；畸形/过宽
// 条目被忽略并通过返回错误报告（装配方决定记录还是拒绝启动）。空串表示
// 「永不信任 X-Forwarded-For」。
func Configure(raw string) error {
	prefixes, invalid := Parse(raw)
	configured.Store(&proxySet{prefixes: prefixes})
	if len(invalid) > 0 {
		return fmt.Errorf("clientip: %s contains invalid entries (ignored): %s",
			EnvVar, strings.Join(invalid, ", "))
	}
	return nil
}

// Parse 把逗号分隔的 CIDR/IP 解析为前缀集合。单个 IP 按 /32（IPv4）或
// /128（IPv6）处理；IPv4-mapped IPv6 前缀统一归一化为 IPv4 语义。
// 返回合法前缀与被拒绝的原始条目。
func Parse(raw string) ([]netip.Prefix, []string) {
	var prefixes []netip.Prefix
	var invalid []string
	for _, part := range strings.Split(raw, ",") {
		entry := strings.TrimSpace(part)
		if entry == "" {
			continue
		}
		var prefix netip.Prefix
		if strings.Contains(entry, "/") {
			p, err := netip.ParsePrefix(entry)
			if err != nil {
				invalid = append(invalid, entry)
				continue
			}
			prefix = p
		} else {
			addr, err := netip.ParseAddr(entry)
			if err != nil {
				invalid = append(invalid, entry)
				continue
			}
			addr = addr.Unmap()
			prefix = netip.PrefixFrom(addr, addr.BitLen())
		}
		normalized, ok := normalizePrefix(prefix)
		if !ok {
			invalid = append(invalid, entry)
			continue
		}
		prefixes = append(prefixes, normalized)
	}
	return prefixes, invalid
}

// normalizePrefix 归一化前缀并拒绝不可用/过宽形态。
// IPv4-mapped IPv6 前缀（::ffff:a.b.c.d/N）转换为 IPv4 前缀；N<96 的映射
// 前缀覆盖范围无法用 IPv4 语义表达，按畸形条目拒绝（fail-closed）。
func normalizePrefix(p netip.Prefix) (netip.Prefix, bool) {
	if !p.IsValid() {
		return netip.Prefix{}, false
	}
	addr := p.Addr()
	bits := p.Bits()
	if addr.Is4In6() {
		if bits < 96 {
			return netip.Prefix{}, false
		}
		addr = addr.Unmap()
		bits -= 96
	}
	if addr.Is4() {
		if bits < minIPv4PrefixBits {
			return netip.Prefix{}, false
		}
	} else if bits < minIPv6PrefixBits {
		return netip.Prefix{}, false
	}
	return netip.PrefixFrom(addr, bits).Masked(), true
}

// ClientIP 使用进程级 TRUSTED_PROXIES 配置解析客户端地址。
func ClientIP(r *http.Request) string {
	if set := configured.Load(); set != nil {
		return Resolve(r, set.prefixes)
	}
	return remoteHost(r.RemoteAddr)
}

// Resolve 实现 §2.4 的解析算法。它接受显式信任列表（测试与未来按请求覆盖
// 使用），空列表 = 永不信任 XFF。
func Resolve(r *http.Request, trustedProxies []netip.Prefix) string {
	rawPeer := remoteHost(r.RemoteAddr)
	peer, ok := parseAddr(rawPeer)
	if !ok || !addrInPrefixes(peer, trustedProxies) {
		// 直连 peer 不可解析或不可信：XFF 完全不可采信。
		return rawPeer
	}

	forwarded, ok := forwardedChain(r.Header.Values("X-Forwarded-For"))
	if !ok || len(forwarded) == 0 {
		return peer.String()
	}

	client := peer
	for i := len(forwarded) - 1; i >= 0; i-- {
		if !addrInPrefixes(forwarded[i], trustedProxies) {
			return forwarded[i].String()
		}
		client = forwarded[i]
	}
	// 全部条目都属可信网段：最左侧就是链的起点。
	return client.String()
}

// remoteHost 从 RemoteAddr 提取主机部分。非标准值（集成测试用 "it-xxx:1234"
// 之类的占位符）原样返回，保证与旧行为一致。
func remoteHost(remoteAddr string) string {
	raw := strings.TrimSpace(remoteAddr)
	if host, _, err := net.SplitHostPort(raw); err == nil {
		return strings.TrimSpace(host)
	}
	return raw
}

func parseAddr(raw string) (netip.Addr, bool) {
	addr, err := netip.ParseAddr(strings.TrimSpace(raw))
	if err != nil {
		return netip.Addr{}, false
	}
	return addr.Unmap(), true
}

// forwardedChain 解析全部 X-Forwarded-For 行（多行按出现顺序拼接）。
// 任一非空条目无法解析即整体判为不可信（ok=false）。
func forwardedChain(values []string) ([]netip.Addr, bool) {
	var out []netip.Addr
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			addr, ok := parseAddr(part)
			if !ok {
				return nil, false
			}
			out = append(out, addr)
		}
	}
	return out, true
}

func addrInPrefixes(addr netip.Addr, prefixes []netip.Prefix) bool {
	for _, prefix := range prefixes {
		if !prefix.IsValid() {
			continue
		}
		// 调用方可能直接传入未归一化的 IPv4-mapped 前缀（Resolve 是显式
		// 列表入口）；此处再做一次廉价归一化，保证与 Parse 语义一致。
		if prefix.Addr().Is4In6() {
			if prefix.Bits() < 96 {
				continue
			}
			prefix = netip.PrefixFrom(prefix.Addr().Unmap(), prefix.Bits()-96)
		}
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}
