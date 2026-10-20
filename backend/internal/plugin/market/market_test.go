package market

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"newtg/gateway/internal/egress"
)

func TestValidateURLMinimalSSRFGuard(t *testing.T) {
	allowed := []string{
		"https://market.example/index.json",
		"https://market.example:8443/index.json",
		"https://203.0.113.10/index.json",
	}
	for _, raw := range allowed {
		if _, err := ValidateURL(raw); err != nil {
			t.Fatalf("合法 URL 被拒：%s: %v", raw, err)
		}
	}
	rejected := map[string]error{
		"http://market.example/index.json":    ErrNonHTTPS,
		"https://user:pass@market.example/x":  ErrUnsafeURL,
		"https://127.0.0.1/index.json":        ErrUnsafeURL,
		"https://10.0.0.8/index.json":         ErrUnsafeURL,
		"https://192.168.1.2/index.json":      ErrUnsafeURL,
		"https://169.254.169.254/latest/meta": ErrUnsafeURL,
		"https://localhost/index.json":        ErrUnsafeURL,
		"https://svc.localhost/index.json":    ErrUnsafeURL,
		"https://api.internal/index.json":     ErrUnsafeURL,
		"https://[::1]/index.json":            ErrUnsafeURL,
	}
	for raw, want := range rejected {
		_, err := ValidateURL(raw)
		if !errors.Is(err, want) {
			t.Fatalf("%s: err = %v，期望 %v", raw, err, want)
		}
	}
}

func TestFetchIndexAndPackage(t *testing.T) {
	source := []byte("globalThis.__NEWTG_PLUGIN__ = { key: 'x' };")
	sum := sha256.Sum256(source)
	hash := hex.EncodeToString(sum[:])
	index := `{"schema_version":1,"plugins":[{"key":"x","name":"X","versions":[` +
		`{"version":"1.0.0","api_version":"v1","source_url":"https://market.example/p.js","source_hash":"` + hash + `"}]}]}`

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/index.json":
			_, _ = w.Write([]byte(index))
		case "/p.js":
			_, _ = w.Write(source)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := testTLSClient(server)

	ctx := context.Background()
	got, err := client.FetchIndex(ctx, "https://market.example/index.json")
	if err != nil {
		t.Fatalf("FetchIndex: %v", err)
	}
	p, v := got.FindVersion("x", "1.0.0")
	if p == nil || v == nil {
		t.Fatalf("FindVersion 未命中：%+v", got)
	}

	body, err := client.FetchSource(ctx, v.SourceURL, v.SourceHash)
	if err != nil || string(body) != string(source) {
		t.Fatalf("FetchSource = %q, %v", body, err)
	}

	bad := strings.Repeat("0", 64)
	if _, err := client.FetchSource(ctx, v.SourceURL, bad); !errors.Is(err, ErrHashMismatch) {
		t.Fatalf("hash 不符 err = %v，期望 ErrHashMismatch", err)
	}
}

func TestFetchIndexRejectsRedirects(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://elsewhere.example/index.json", http.StatusFound)
	}))
	defer server.Close()
	client := testTLSClient(server)

	_, err := client.FetchIndex(context.Background(), "https://market.example/index.json")
	if !errors.Is(err, ErrRedirect) {
		t.Fatalf("重定向 err = %v，期望 ErrRedirect", err)
	}
}

func TestFetchIndexRejectsInvalidPayload(t *testing.T) {
	cases := []string{
		`{"schema_version":2,"plugins":[]}`,
		`{"schema_version":1,"plugins":[{"key":"Bad Key","name":"x","versions":[]}]}`,
		`{"schema_version":1,"plugins":[{"key":"x","name":"x","versions":[{"version":"1","source_url":"http://a.example/p.js","source_hash":"` + strings.Repeat("a", 64) + `"}]}]}`,
		`not json`,
	}
	for _, payload := range cases {
		payload := payload
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(payload))
		}))
		client := testTLSClient(server)
		_, err := client.FetchIndex(context.Background(), "https://market.example/index.json")
		server.Close()
		if !errors.Is(err, ErrInvalidIndex) {
			t.Fatalf("%s: err = %v，期望 ErrInvalidIndex", payload, err)
		}
	}
}

func TestFetchIndexSizeLimit(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("a", 128)))
	}))
	defer server.Close()
	client := testTLSClient(server)
	client.MaxIndexBytes = 32

	_, err := client.FetchIndex(context.Background(), "https://market.example/index.json")
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("超限 err = %v，期望 ErrTooLarge", err)
	}
}

func testTLSClient(server *httptest.Server) *Client {
	base := server.Client().Transport.(*http.Transport).Clone()
	base.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec
	address := server.Listener.Addr().String()
	base.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	return &Client{http: &http.Client{Transport: base}}
}

type marketConfig map[string]any

func (m marketConfig) Get(key string) (json.RawMessage, bool) {
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

// TestFetchIndexUsesEgressPolicy 是 B2 接入回归：生产路径（HTTP == nil）
// 必须经统一出站执行器校验域名与解析后的地址。
func TestFetchIndexUsesEgressPolicy(t *testing.T) {
	ctx := context.Background()

	// 默认策略是 allowlist：未登记主机在解析前即被拒。
	unlisted := &Client{
		Config: marketConfig{},
		resolve: func(context.Context, string) ([]net.IP, error) {
			t.Fatal("allowlist 拒绝不应触发解析")
			return nil, nil
		},
	}
	if _, err := unlisted.FetchIndex(ctx, "https://not-listed.example/index.json"); !errors.Is(err, ErrUnsafeURL) {
		t.Fatalf("未登记主机 err = %v，期望 ErrUnsafeURL", err)
	}

	// 域名放开后，解析到私网的主机仍被地址规则拒绝。
	private := &Client{
		Config: marketConfig{"sec.ssrf.domain_mode": "all"},
		resolve: func(context.Context, string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("10.0.0.8")}, nil
		},
	}
	if _, err := private.FetchIndex(ctx, "https://rebind.example/index.json"); !errors.Is(err, ErrUnsafeURL) {
		t.Fatalf("私网解析 err = %v，期望 ErrUnsafeURL", err)
	}
}

// TestFetchIndexThroughGuardedClient 是 LOW-5 补测：生产路径（未注入
// HTTP）经 egress 执行器成功出站——解析一次、校验后拨已校验 IP，并接受
// httptest 的自签证书。它同时证明 guardOverride/ tlsConfig 测试接缝
// 走的确实是受守卫的 transport，而不是被静默替换成裸 http.Client。
func TestFetchIndexThroughGuardedClient(t *testing.T) {
	index := `{"schema_version":1,"plugins":[]}`
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(index))
	}))
	defer server.Close()
	_, port, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split listener addr: %v", err)
	}

	guard := egress.NewGuard(egress.Policy{
		Mode: egress.ModeBasic, DomainMode: egress.DomainAllowlist,
		DomainList: []string{"market.example"},
		// allow_private_ip + 显式 allowlist 命中 = 冻结的覆盖例外；
		// AllowAnyPort 是测试接缝（httptest 随机端口）。
		AllowPrivateIP: true, AllowAnyPort: true,
	}).WithResolver(func(context.Context, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("127.0.0.1")}, nil
	})
	client := &Client{
		guardOverride: guard,
		tlsConfig:     &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
	}
	got, err := client.FetchIndex(context.Background(), "https://market.example:"+port+"/index.json")
	if err != nil {
		t.Fatalf("guarded FetchIndex: %v", err)
	}
	if got.SchemaVersion != 1 {
		t.Fatalf("schema_version = %d，期望 1", got.SchemaVersion)
	}
}
