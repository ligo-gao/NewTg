package tasks

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"newtg/gateway/internal/egress"
)

func TestDoOutboundBlocksPrivateLiteralByDefault(t *testing.T) {
	desc := &RequestDescriptor{URL: "http://127.0.0.1:8080/v1/tasks", Method: http.MethodGet}
	_, err := DoOutboundWithPolicy(context.Background(), NewOutboundClient(), desc,
		[]string{"127.0.0.1:8080"}, egress.PolicyFromConfig(nil), false)
	if !errors.Is(err, ErrOutboundBlockedAddr) {
		t.Fatalf("want ErrOutboundBlockedAddr, got %v", err)
	}
}

func TestDoOutboundGuardRejectsDNSRebinding(t *testing.T) {
	var calls atomic.Int32
	resolver := func(_ context.Context, _ string) ([]net.IP, error) {
		if calls.Add(1) == 1 {
			// 第一次（预校验）返回公网地址，让请求进入拨号阶段。
			return []net.IP{net.ParseIP("93.184.216.34")}, nil
		}
		// 拨号阶段换答案 → 必须被 DialContext 拒绝。
		return []net.IP{net.ParseIP("10.0.0.8")}, nil
	}
	guard := egress.NewGuard(egress.Policy{
		Mode: egress.ModeBasic, DomainMode: egress.DomainAllowlist,
		DomainList: []string{"rebind.example"}, ConnectTimeout: time.Second,
	}).WithResolver(resolver)

	desc := &RequestDescriptor{URL: "http://rebind.example/steal", Method: http.MethodGet}
	_, err := DoOutboundWithGuard(context.Background(), NewOutboundClient(), desc,
		[]string{"rebind.example"}, guard)
	if !errors.Is(err, ErrOutboundPolicy) {
		t.Fatalf("DNS rebinding must be rejected at dial time, got %v", err)
	}
}

func TestDoOutboundGuardRejectsPrivateResolution(t *testing.T) {
	resolver := func(context.Context, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("192.168.1.10")}, nil
	}
	guard := egress.NewGuard(egress.Policy{
		Mode: egress.ModeBasic, DomainMode: egress.DomainAllowlist,
		DomainList: []string{"internal.example"}, ConnectTimeout: time.Second,
	}).WithResolver(resolver)
	desc := &RequestDescriptor{URL: "http://internal.example/", Method: http.MethodGet}
	if _, err := DoOutboundWithGuard(context.Background(), NewOutboundClient(), desc,
		[]string{"internal.example"}, guard); !errors.Is(err, ErrOutboundPolicy) {
		t.Fatalf("private DNS answer must be rejected, got %v", err)
	}
}

func TestDoOutboundRedirectRejectedUnderGuard(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer target.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirector.Close()

	desc := &RequestDescriptor{URL: redirector.URL, Method: http.MethodGet}
	_, err := DoOutboundWithPolicy(context.Background(), NewOutboundClient(), desc,
		[]string{hostOf(t, redirector.URL), hostOf(t, target.URL)},
		egress.PolicyFromConfig(nil), true)
	if !errors.Is(err, ErrOutboundRedirect) {
		t.Fatalf("redirects must not be followed, got %v", err)
	}
}

func hostOf(t *testing.T, raw string) string {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return parsed.Host
}

func TestCredentiallessDescriptorRestrictions(t *testing.T) {
	if _, err := DecodeRequestDescriptor(map[string]any{
		"url": "https://cdn.example/a", "method": "POST", "credentialless": true,
	}); !errors.Is(err, ErrOutboundPolicy) {
		t.Fatalf("credentialless POST must be rejected, got %v", err)
	}
	if _, err := DecodeRequestDescriptor(map[string]any{
		"url": "https://cdn.example/a", "method": "GET", "credentialless": true,
		"headers": map[string]any{"Authorization": "secret"},
	}); !errors.Is(err, ErrOutboundPolicy) {
		t.Fatalf("credentialless headers must be rejected, got %v", err)
	}
	if _, err := DecodeRequestDescriptor(map[string]any{
		"url": "https://cdn.example/a", "method": "GET", "credentialless": true,
		"body": map[string]any{"a": 1},
	}); !errors.Is(err, ErrOutboundPolicy) {
		t.Fatalf("credentialless body must be rejected, got %v", err)
	}
}

func TestCredentiallessSkipsHostAllowlistButKeepsAddressPolicy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("cdn"))
	}))
	defer srv.Close()

	desc := &RequestDescriptor{URL: srv.URL + "/asset", Method: http.MethodGet, Credentialless: true}
	resp, err := DoOutboundWithPolicy(context.Background(), NewOutboundClient(), desc,
		nil, egress.PolicyFromConfig(nil), true)
	if err != nil {
		t.Fatalf("credentialless GET to dbless CDN host: %v", err)
	}
	if resp.Body != "cdn" {
		t.Fatalf("unexpected body: %q", resp.Body)
	}

	private := &RequestDescriptor{URL: "http://127.0.0.1:9/asset", Method: http.MethodGet, Credentialless: true}
	if _, err := DoOutboundWithPolicy(context.Background(), NewOutboundClient(), private,
		nil, egress.PolicyFromConfig(nil), false); !errors.Is(err, ErrOutboundBlockedAddr) {
		t.Fatalf("credentialless must still reject private literals, got %v", err)
	}
}

func TestPolicyForHostsNormalizesPorts(t *testing.T) {
	policy := PolicyForHosts(egress.PolicyFromConfig(nil),
		[]string{"API.Example.com:8443", "[::1]:9000"}, false, false)
	if policy.DomainMode != egress.DomainAllowlist {
		t.Fatalf("domain mode = %q", policy.DomainMode)
	}
	want := []string{"api.example.com", "::1"}
	if len(policy.DomainList) != len(want) {
		t.Fatalf("domain list = %#v", policy.DomainList)
	}
	for i := range want {
		if policy.DomainList[i] != want[i] {
			t.Fatalf("domain list = %#v, want %v", policy.DomainList, want)
		}
	}
	deny := PolicyForHosts(egress.Policy{DomainMode: egress.DomainDenylist, DomainList: []string{"evil.example"}},
		[]string{"api.example.com"}, false, false)
	if deny.DomainMode != egress.DomainDenylist || len(deny.DomainList) != 1 {
		t.Fatalf("denylist must be preserved: %#v", deny)
	}
}
