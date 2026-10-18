package clientip

// 本包是 TRUSTED_PROXIES 感知的客户端 IP 解析器，供 httpapi（限流/审计）、
// rbac（风控上下文）与 auth（Turnstile remoteip）共用，保证边缘部署下三个
// 消费点看到同一个客户端地址（security-07 §2.4「唯一取值路径」）。

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"testing"
)

// TestParseRejectsBroadOrZeroPrefixes 覆盖 R-07-8 的主机段约束：
// `0.0.0.0/0` / `::/0` 与过宽前缀（IPv4 < /8、IPv6 < /16）必须走 invalid
// 路径（忽略且报告），绝不能因为一条配置把全部来源变成可信代理。
func TestParseRejectsBroadOrZeroPrefixes(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{"IPv4 默认路由", "0.0.0.0/0", true},
		{"IPv6 默认路由", "::/0", true},
		{"IPv4 过宽 /7", "10.0.0.0/7", true},
		{"IPv6 过宽 /15", "2001:db8::/15", true},
		{"IPv4-mapped 归一化后为 /0", "::ffff:0.0.0.0/96", true},
		{"IPv4 最小 /8", "10.0.0.0/8", false},
		{"IPv6 最小 /16", "2001:db8::/16", false},
		{"单个 IPv4", "203.0.113.9", false},
		{"单个 IPv6", "2001:db8::1", false},
		{"畸形条目", "not-an-ip", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prefixes, invalid := Parse(tc.raw)
			if tc.wantErr {
				if len(invalid) == 0 {
					t.Fatalf("Parse(%q) invalid = 空，期望拒绝", tc.raw)
				}
				if len(prefixes) != 0 {
					t.Fatalf("Parse(%q) prefixes = %v，被拒绝的条目不得生效", tc.raw, prefixes)
				}
				return
			}
			if len(invalid) != 0 || len(prefixes) != 1 {
				t.Fatalf("Parse(%q) = (%v, %v)，期望 1 个合法前缀且无 invalid", tc.raw, prefixes, invalid)
			}
		})
	}
}

// TestConfigureRejectsBroadPrefix 验证装配接缝：过宽配置返回错误，且不会
// 让直连 peer 变成被信任的代理。
func TestConfigureRejectsBroadPrefix(t *testing.T) {
	t.Cleanup(func() { _ = Configure(os.Getenv(EnvVar)) })

	if err := Configure("0.0.0.0/0"); err == nil {
		t.Fatal("0.0.0.0/0 必须返回错误")
	}
	req := httptest.NewRequest(http.MethodGet, "/api/whatever", nil)
	req.RemoteAddr = "203.0.113.9:1234"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	if got := ClientIP(req); got != "203.0.113.9" {
		t.Fatalf("过宽配置被拒绝后 ClientIP = %q，期望直连 peer 203.0.113.9", got)
	}

	if err := Configure("10.0.0.0/8"); err != nil {
		t.Fatalf("合法配置返回错误: %v", err)
	}
	req.RemoteAddr = "10.0.0.2:1234"
	if got := ClientIP(req); got != "1.2.3.4" {
		t.Fatalf("合法可信代理下 ClientIP = %q，期望 XFF 客户端 1.2.3.4", got)
	}
}

// TestResolveTrustBoundary 是解析算法的核心表：空列表、可信/不可信直连、
// 链式跳过、IPv4/IPv6、畸形条目回退。
func TestResolveTrustBoundary(t *testing.T) {
	prefixes := func(raw ...string) []netip.Prefix {
		out := make([]netip.Prefix, 0, len(raw))
		for _, s := range raw {
			out = append(out, netip.MustParsePrefix(s))
		}
		return out
	}

	cases := []struct {
		name       string
		remoteAddr string
		xff        []string
		trusted    []netip.Prefix
		want       string
	}{
		{
			name:       "空列表永不信任头",
			remoteAddr: "203.0.113.9:1234",
			xff:        []string{"198.51.100.7"},
			want:       "203.0.113.9",
		},
		{
			name:       "不可信直连忽略头",
			remoteAddr: "203.0.113.9:1234",
			xff:        []string{"198.51.100.7"},
			trusted:    prefixes("10.0.0.0/8"),
			want:       "203.0.113.9",
		},
		{
			name:       "可信代理取右侧第一个不可信地址",
			remoteAddr: "10.0.0.2:1234",
			xff:        []string{"1.1.1.1, 203.0.113.7, 10.0.0.3"},
			trusted:    prefixes("10.0.0.0/8"),
			want:       "203.0.113.7",
		},
		{
			name:       "IPv6 可信代理",
			remoteAddr: "[2001:db8:1::2]:443",
			xff:        []string{"2001:db8:2::7"},
			trusted:    prefixes("2001:db8:1::/48"),
			want:       "2001:db8:2::7",
		},
		{
			name:       "带端口的 XFF 条目按畸形回退",
			remoteAddr: "10.0.0.2:1234",
			xff:        []string{"203.0.113.7:5678"},
			trusted:    prefixes("10.0.0.0/8"),
			want:       "10.0.0.2",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/whatever", nil)
			req.RemoteAddr = tc.remoteAddr
			for _, v := range tc.xff {
				req.Header.Add("X-Forwarded-For", v)
			}
			if got := Resolve(req, tc.trusted); got != tc.want {
				t.Fatalf("Resolve = %q，期望 %q", got, tc.want)
			}
		})
	}
}

// 防回归：EnvVar 必须与 config.Config 读取的变量名一致。
func TestEnvVarName(t *testing.T) {
	if strings.TrimSpace(EnvVar) != "TRUSTED_PROXIES" {
		t.Fatalf("EnvVar = %q，期望 TRUSTED_PROXIES", EnvVar)
	}
}
