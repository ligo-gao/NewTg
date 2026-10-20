package egress

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type mapConfig map[string]any

func (m mapConfig) Get(key string) (json.RawMessage, bool) {
	value, ok := m[key]
	if !ok {
		return nil, false
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, false
	}
	return raw, true
}

type staticResolver map[string][]net.IP

func (r staticResolver) resolve(_ context.Context, host string) ([]net.IP, error) {
	ips, ok := r[host]
	if !ok {
		return nil, errors.New("no such host")
	}
	return ips, nil
}

func TestPolicyFromConfigDefaults(t *testing.T) {
	policy := PolicyFromConfig(nil)
	if policy.Mode != ModeBasic {
		t.Fatalf("mode = %q，期望 basic", policy.Mode)
	}
	if policy.DomainMode != DomainAllowlist {
		t.Fatalf("domain_mode = %q，期望 allowlist", policy.DomainMode)
	}
	if !containsString(policy.DomainList, "api.openai.com") {
		t.Fatalf("默认域名清单缺少出厂预置项：%v", policy.DomainList)
	}
	if policy.AllowPrivateIP {
		t.Fatal("allow_private_ip 默认必须为 false")
	}
	if policy.MaxRedirects != 3 {
		t.Fatalf("max_redirects = %d，期望 3", policy.MaxRedirects)
	}
	if policy.ConnectTimeout != 10*time.Second {
		t.Fatalf("connect_timeout = %v，期望 10s", policy.ConnectTimeout)
	}
}

func TestPolicyFromConfigOverridesAndLegacy(t *testing.T) {
	policy := PolicyFromConfig(mapConfig{
		KeyMode:                  "strict",
		KeyDomainMode:            "denylist",
		KeyDomainList:            []string{"bad.example", "*.evil.example"},
		KeyAllowPrivateIP:        true,
		KeyMaxRedirects:          5,
		KeyConnectTimeoutSeconds: 3,
	})
	if policy.Mode != ModeStrict || policy.DomainMode != DomainDenylist {
		t.Fatalf("policy = %+v", policy)
	}
	if !policy.AllowPrivateIP || policy.MaxRedirects != 5 || policy.ConnectTimeout != 3*time.Second {
		t.Fatalf("policy = %+v", policy)
	}

	// 旧布尔开关仅在本键缺失时生效。
	if got := PolicyFromConfig(mapConfig{KeyEnabled: false}); got.Mode != ModeOff {
		t.Fatalf("enabled=false 应等价 off，得到 %q", got.Mode)
	}
	if got := PolicyFromConfig(mapConfig{KeyEnabled: false, KeyMode: "basic"}); got.Mode != ModeBasic {
		t.Fatalf("mode 存在时应忽略 enabled，得到 %q", got.Mode)
	}
	// 无法识别的枚举按更严方向回退。
	if got := PolicyFromConfig(mapConfig{KeyMode: "bogus", KeyDomainMode: "bogus"}); got.Mode != ModeBasic || got.DomainMode != DomainAllowlist {
		t.Fatalf("非法枚举回退 = %+v", got)
	}
	// allowlist 下损坏的清单 fail-closed。
	broken := PolicyFromConfig(mapConfig{KeyDomainList: "not-an-array"})
	if len(broken.DomainList) != 0 {
		t.Fatalf("损坏清单应清空，得到 %v", broken.DomainList)
	}
}

func TestValidateURLShapeRules(t *testing.T) {
	basic := NewGuard(Policy{Mode: ModeBasic, DomainMode: DomainAll})
	allowed := []string{
		"http://example.com/manifest.json",
		"https://example.com/x",
		"https://example.com:443/x",
		"http://example.com:80/x",
	}
	for _, raw := range allowed {
		if _, err := basic.ValidateURLShape(raw); err != nil {
			t.Fatalf("basic 应允许 %q：%v", raw, err)
		}
	}
	blocked := []string{
		"ftp://example.com/x",
		"http://example.com:6379/x",
		"https://example.com:8443/x",
		"https://user:pass@example.com/x",
		"https://example.com:99999/x",
		"http:///no-host",
	}
	for _, raw := range blocked {
		if _, err := basic.ValidateURLShape(raw); !errors.Is(err, ErrBlocked) {
			t.Fatalf("basic 应拒绝 %q：%v", raw, err)
		}
	}

	strict := NewGuard(Policy{Mode: ModeStrict, DomainMode: DomainAll})
	if _, err := strict.ValidateURLShape("http://example.com/x"); !errors.Is(err, ErrBlocked) {
		t.Fatalf("strict 应拒绝 http：%v", err)
	}
	if _, err := strict.ValidateURLShape("https://example.com:8443/x"); !errors.Is(err, ErrBlocked) {
		t.Fatalf("strict 应拒绝非 443：%v", err)
	}
	if _, err := strict.ValidateURLShape("https://example.com/x"); err != nil {
		t.Fatalf("strict 应允许 https:443：%v", err)
	}
}

func TestValidateURLDomainModes(t *testing.T) {
	allow := NewGuard(Policy{
		Mode: ModeBasic, DomainMode: DomainAllowlist,
		DomainList: []string{"api.example.com", "*.trusted.example"},
	})
	for _, raw := range []string{
		"https://api.example.com/x",
		"https://a.trusted.example/x",
		"https://a.b.trusted.example/x",
	} {
		if _, err := allow.ValidateURLShape(raw); err != nil {
			t.Fatalf("allowlist 应允许 %q：%v", raw, err)
		}
	}
	for _, raw := range []string{
		"https://other.example/x",
		"https://trusted.example/x", // 通配不匹配裸域
		"https://api.example.com.evil.example/x",
	} {
		if _, err := allow.ValidateURLShape(raw); !errors.Is(err, ErrBlocked) {
			t.Fatalf("allowlist 应拒绝 %q：%v", raw, err)
		}
	}

	deny := NewGuard(Policy{
		Mode: ModeBasic, DomainMode: DomainDenylist,
		DomainList: []string{"bad.example", "*.blocked.example"},
	})
	if _, err := deny.ValidateURLShape("https://bad.example/x"); !errors.Is(err, ErrBlocked) {
		t.Fatalf("denylist 应拒绝精确命中：%v", err)
	}
	if _, err := deny.ValidateURLShape("https://a.blocked.example/x"); !errors.Is(err, ErrBlocked) {
		t.Fatalf("denylist 应拒绝通配命中：%v", err)
	}
	if _, err := deny.ValidateURLShape("https://good.example/x"); err != nil {
		t.Fatalf("denylist 应允许清单外主机：%v", err)
	}
}

func TestValidateURLAddressClassification(t *testing.T) {
	ctx := context.Background()
	guard := NewGuard(Policy{Mode: ModeBasic, DomainMode: DomainAll})

	blocked := []string{
		"127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1",
		"169.254.169.254", "::1", "fe80::1", "fc00::1",
		"224.0.0.1", "0.0.0.0", "100.64.0.1", "192.0.0.10",
		"198.18.0.1", "240.0.0.1",
	}
	for _, host := range blocked {
		if _, err := guard.ValidateURL(ctx, "https://"+host+"/x"); !errors.Is(err, ErrBlocked) {
			t.Fatalf("basic 应拒绝 %s：%v", host, err)
		}
	}
	for _, host := range []string{"8.8.8.8", "203.0.113.5"} {
		if _, err := guard.ValidateURL(ctx, "https://"+host+"/x"); err != nil {
			t.Fatalf("basic 应允许 %s：%v", host, err)
		}
	}

	// 多 A 记录里混入一个私网地址就必须整体拒绝。
	var calls int32
	mixed := guard.WithResolver(func(context.Context, string) ([]net.IP, error) {
		atomic.AddInt32(&calls, 1)
		return []net.IP{net.ParseIP("203.0.113.5"), net.ParseIP("10.0.0.8")}, nil
	})
	if _, err := mixed.ValidateURL(ctx, "https://mixed.example/x"); !errors.Is(err, ErrBlocked) {
		t.Fatalf("混合解析应整体拒绝：%v", err)
	}
	if calls != 1 {
		t.Fatalf("解析次数 = %d，期望 1", calls)
	}

	// 未解析主机有独立分类。
	unresolved := guard.WithResolver(func(context.Context, string) ([]net.IP, error) {
		return nil, errors.New("no such host")
	})
	if _, err := unresolved.ValidateURL(ctx, "https://gone.example/x"); !errors.Is(err, ErrUnresolved) {
		t.Fatalf("未解析 err = %v，期望 ErrUnresolved", err)
	}
}

func TestAllowPrivateIPCombinations(t *testing.T) {
	ctx := context.Background()
	guard := NewGuard(Policy{Mode: ModeBasic, DomainMode: DomainAll, AllowPrivateIP: true})

	// RFC1918/ULA 放行。
	for _, host := range []string{"10.1.2.3", "172.16.0.1", "192.168.1.1", "[fc00::1]"} {
		if _, err := guard.ValidateURL(ctx, "https://"+host+"/x"); err != nil {
			t.Fatalf("allow_private_ip 应放行 %s：%v", host, err)
		}
	}
	// loopback / link-local / 元数据 / CGNAT 仍然拒绝。
	for _, host := range []string{"127.0.0.1", "169.254.169.254", "100.100.100.200", "100.64.0.1"} {
		if _, err := guard.ValidateURL(ctx, "https://"+host+"/x"); !errors.Is(err, ErrBlocked) {
			t.Fatalf("allow_private_ip 仍应拒绝 %s：%v", host, err)
		}
	}

	// 只有 allowlist 显式列出的主机可以越过 loopback/元数据限制。
	override := NewGuard(Policy{
		Mode: ModeBasic, DomainMode: DomainAllowlist, AllowPrivateIP: true,
		DomainList: []string{"hooks.internal"},
	}).WithResolver(staticResolver{"hooks.internal": {net.ParseIP("127.0.0.1")}}.resolve)
	if _, err := override.ValidateURL(ctx, "https://hooks.internal/x"); err != nil {
		t.Fatalf("allowlist 覆盖应放行 loopback：%v", err)
	}

	// allow_private_ip=false 时，即使主机在 allowlist 内也不放行。
	noOverride := NewGuard(Policy{
		Mode: ModeBasic, DomainMode: DomainAllowlist,
		DomainList: []string{"hooks.internal"},
	}).WithResolver(staticResolver{"hooks.internal": {net.ParseIP("127.0.0.1")}}.resolve)
	if _, err := noOverride.ValidateURL(ctx, "https://hooks.internal/x"); !errors.Is(err, ErrBlocked) {
		t.Fatalf("allow_private_ip=false 应拒绝 loopback：%v", err)
	}
}

func TestStrictRejectsMetadataAndIPv6Local(t *testing.T) {
	ctx := context.Background()
	basic := NewGuard(Policy{Mode: ModeBasic, DomainMode: DomainAll})
	strict := NewGuard(Policy{Mode: ModeStrict, DomainMode: DomainAll})

	// 168.63.129.16 不在 CGNAT/link-local 段，basic 放行、strict 拒绝；
	// 100.100.100.200（阿里云）落在 100.64/10，basic 已由 CGNAT 规则拦截。
	for _, host := range []string{"168.63.129.16"} {
		if _, err := basic.ValidateURL(ctx, "https://"+host+"/x"); err != nil {
			t.Fatalf("basic 不应拒绝非 link-local 元数据 %s：%v", host, err)
		}
		if _, err := strict.ValidateURL(ctx, "https://"+host+"/x"); !errors.Is(err, ErrBlocked) {
			t.Fatalf("strict 应拒绝元数据 %s：%v", host, err)
		}
	}
	for _, host := range []string{"fec0::1", "fe80::1", "fd00::1"} {
		if _, err := strict.ValidateURL(ctx, "https://["+host+"]/x"); !errors.Is(err, ErrBlocked) {
			t.Fatalf("strict 应拒绝 IPv6 本地 %s：%v", host, err)
		}
	}
}

// newTestGuard 构造 httptest 可用的执行器：loopback + 任意端口 +
// allowlist 覆盖（冻结语义允许显式 allowlist 主机越过地址限制）。
func newTestGuard(t *testing.T, resolver staticResolver) *Guard {
	t.Helper()
	return NewGuard(Policy{
		Mode: ModeBasic, DomainMode: DomainAllowlist,
		DomainList:     []string{"first.test", "second.test", "third.test", "evil.test"},
		AllowPrivateIP: true, AllowLoopback: true, AllowAnyPort: true,
		MaxRedirects: 2,
	}).WithResolver(resolver.resolve)
}

func TestClientFollowsRedirectsWithPerHopValidation(t *testing.T) {
	var finalHits int32
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&finalHits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer final.Close()
	_, finalPort, _ := net.SplitHostPort(final.Listener.Addr().String())

	start := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://second.test:"+finalPort+"/final", http.StatusFound)
	}))
	defer start.Close()
	_, startPort, _ := net.SplitHostPort(start.Listener.Addr().String())

	guard := newTestGuard(t, staticResolver{"first.test": {net.ParseIP("127.0.0.1")}, "second.test": {net.ParseIP("127.0.0.1")}})
	client := guard.NewClient(ClientOptions{Timeout: 3 * time.Second, FollowRedirects: true})
	defer client.CloseIdleConnections()

	resp, err := client.Get("http://first.test:" + startPort + "/start")
	if err != nil {
		t.Fatalf("跟随重定向失败：%v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || atomic.LoadInt32(&finalHits) != 1 {
		t.Fatalf("status=%d hits=%d", resp.StatusCode, finalHits)
	}
}

func TestClientRedirectPerHopRejection(t *testing.T) {
	var evilHits int32
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&evilHits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer evil.Close()
	_, evilPort, _ := net.SplitHostPort(evil.Listener.Addr().String())

	start := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://evil.test:"+evilPort+"/final", http.StatusFound)
	}))
	defer start.Close()
	_, startPort, _ := net.SplitHostPort(start.Listener.Addr().String())

	// evil.test 不在 allowlist：第二跳必须在发起连接前被拒绝。
	guard := NewGuard(Policy{
		Mode: ModeBasic, DomainMode: DomainAllowlist,
		DomainList:     []string{"first.test"},
		AllowPrivateIP: true, AllowLoopback: true, AllowAnyPort: true,
		MaxRedirects: 2,
	}).WithResolver(staticResolver{"first.test": {net.ParseIP("127.0.0.1")}, "evil.test": {net.ParseIP("127.0.0.1")}}.resolve)
	client := guard.NewClient(ClientOptions{Timeout: 3 * time.Second, FollowRedirects: true})
	defer client.CloseIdleConnections()

	_, err := client.Get("http://first.test:" + startPort + "/start")
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("重定向 err = %v，期望 ErrBlocked", err)
	}
	if atomic.LoadInt32(&evilHits) != 0 {
		t.Fatal("被拒重定向的下一跳不应收到连接")
	}
}

func TestClientRedirectLimit(t *testing.T) {
	env := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, port, _ := net.SplitHostPort(r.Host)
		http.Redirect(w, r, "http://first.test:"+port+"/loop", http.StatusFound)
	}))
	defer env.Close()
	_, port, _ := net.SplitHostPort(env.Listener.Addr().String())

	guard := newTestGuard(t, staticResolver{"first.test": {net.ParseIP("127.0.0.1")}})
	// MaxRedirects=2：第 3 跳必须被拒绝。
	client := guard.NewClient(ClientOptions{Timeout: 3 * time.Second, FollowRedirects: true})
	defer client.CloseIdleConnections()

	_, err := client.Get("http://first.test:" + port + "/loop")
	if !errors.Is(err, ErrRedirect) {
		t.Fatalf("err = %v，期望 ErrRedirect", err)
	}
}

func TestClientDoesNotFollowRedirectsByDefault(t *testing.T) {
	var finalHits int32
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&finalHits, 1)
	}))
	defer final.Close()
	_, finalPort, _ := net.SplitHostPort(final.Listener.Addr().String())

	start := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://second.test:"+finalPort+"/final", http.StatusFound)
	}))
	defer start.Close()
	_, startPort, _ := net.SplitHostPort(start.Listener.Addr().String())

	guard := newTestGuard(t, staticResolver{"first.test": {net.ParseIP("127.0.0.1")}, "second.test": {net.ParseIP("127.0.0.1")}})
	client := guard.NewClient(ClientOptions{Timeout: 3 * time.Second})
	defer client.CloseIdleConnections()

	resp, err := client.Get("http://first.test:" + startPort + "/start")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound || atomic.LoadInt32(&finalHits) != 0 {
		t.Fatalf("status=%d hits=%d", resp.StatusCode, finalHits)
	}
}

// TestDialResolvesOnceAndDialsValidatedIP 是反 rebinding 的核心回归：
// 解析器第一次给 loopback（显式 allowlist 放行）、之后给私网；拨号必须
// 使用第一次的答案，第二次解析（若发生）不得影响连接目标。
func TestDialResolvesOnceAndDialsValidatedIP(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = listener.Close() }()

	var accepted int32
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			atomic.AddInt32(&accepted, 1)
			_ = conn.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(listener.Addr().String())

	var calls int32
	guard := NewGuard(Policy{
		Mode: ModeBasic, DomainMode: DomainAllowlist,
		DomainList: []string{"rebind.test"}, AllowPrivateIP: true, AllowLoopback: true,
	}).WithResolver(func(context.Context, string) ([]net.IP, error) {
		if atomic.AddInt32(&calls, 1) == 1 {
			return []net.IP{net.ParseIP("127.0.0.1")}, nil
		}
		return []net.IP{net.ParseIP("10.0.0.8")}, nil
	})

	conn, err := guard.DialContext(context.Background(), "tcp", net.JoinHostPort("rebind.test", port))
	if err != nil {
		t.Fatalf("DialContext: %v", err)
	}
	_ = conn.Close()
	if calls != 1 {
		t.Fatalf("解析次数 = %d，期望 1", calls)
	}
	deadline := time.Now().Add(time.Second)
	for atomic.LoadInt32(&accepted) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if atomic.LoadInt32(&accepted) != 1 {
		t.Fatalf("listener accept = %d，期望 1（必须拨向已校验 IP）", accepted)
	}
}

func TestDialRejectsPrivateResolutionBeforeConnecting(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = listener.Close() }()
	_, port, _ := net.SplitHostPort(listener.Addr().String())

	guard := NewGuard(Policy{Mode: ModeBasic, DomainMode: DomainAll}).
		WithResolver(func(context.Context, string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("10.0.0.8")}, nil
		})
	if _, err := guard.DialContext(context.Background(), "tcp", net.JoinHostPort("rebind.test", port)); !errors.Is(err, ErrBlocked) {
		t.Fatalf("err = %v，期望 ErrBlocked", err)
	}
}

func TestErrorsDoNotLeakURLQuery(t *testing.T) {
	guard := NewGuard(Policy{Mode: ModeBasic, DomainMode: DomainAll})
	const token = "super-secret-value"
	_, err := guard.ValidateURL(context.Background(), "https://127.0.0.1/hook?token="+token)
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("err = %v，期望 ErrBlocked", err)
	}
	if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), "/hook") {
		t.Fatalf("错误文本泄露 URL：%v", err)
	}

	// 解析错误同样不回显入参。
	_, err = guard.ValidateURLShape("https://user:pass@example.com/x?token=" + token)
	if !errors.Is(err, ErrBlocked) || strings.Contains(err.Error(), token) {
		t.Fatalf("userinfo 拒绝泄露 URL：%v", err)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
