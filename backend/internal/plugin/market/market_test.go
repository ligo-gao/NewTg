package market

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
	return &Client{HTTP: &http.Client{Transport: base}}
}
