package ionet

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 本文件锁定 provider 客户端的契约：固定 host/path、鉴权头、超时、TLS 下限、
// 不跟随重定向、只读 GET 的有界重试与错误脱敏。真实 provider 响应形状
// 待实测（R-M6D-13），因此这里只断言代码承诺的传输层行为。

func TestClientFixedHostAndTimeout(t *testing.T) {
	if DefaultBaseURL != "https://api.io.solutions" {
		t.Fatalf("DefaultBaseURL = %q，控制面主机是固定常量（§3.20.7）", DefaultBaseURL)
	}
	if DefaultTimeout != 30*time.Second {
		t.Fatalf("DefaultTimeout = %v，设计冻结 30s", DefaultTimeout)
	}
	client := NewClient(Config{})
	if client.BaseURL() != DefaultBaseURL {
		t.Fatalf("默认 baseURL = %q", client.BaseURL())
	}
	httpClient := client.HTTPClient()
	if httpClient.Timeout != DefaultTimeout {
		t.Fatalf("HTTP 超时 = %v，期望 %v", httpClient.Timeout, DefaultTimeout)
	}
	transport, ok := httpClient.Transport.(*http.Transport)
	if !ok || transport.TLSClientConfig == nil {
		t.Fatal("默认 transport 必须配置 TLS")
	}
	if transport.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Fatalf("TLS 下限 = %#x，期望 TLS 1.2", transport.TLSClientConfig.MinVersion)
	}
	if transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("TLS 证书校验不得关闭")
	}
	if httpClient.CheckRedirect == nil {
		t.Fatal("必须配置重定向策略")
	}
}

func TestClientMethodPathAndHeaders(t *testing.T) {
	var (
		method, path, apiKey, contentType, accept string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		path = r.URL.Path
		apiKey = r.Header.Get("X-API-KEY")
		contentType = r.Header.Get("Content-Type")
		accept = r.Header.Get("Accept")
		_, _ = w.Write([]byte(`{"hardware_types":[{"id":1,"available_replicas":2},{"id":2,"available_replicas":3}],"total":2,"total_available":5}`))
	}))
	defer srv.Close()

	client := NewClient(Config{APIKey: "test-key", BaseURL: srv.URL, Timeout: 2 * time.Second})
	info, err := client.TestConnection(context.Background())
	if err != nil {
		t.Fatalf("TestConnection: %v", err)
	}
	if method != http.MethodGet {
		t.Fatalf("method = %s，期望 GET", method)
	}
	if path != PathHardwareTypes {
		t.Fatalf("path = %q，期望 %q", path, PathHardwareTypes)
	}
	if apiKey != "test-key" {
		t.Fatalf("X-API-KEY = %q", apiKey)
	}
	if contentType != "application/json" || accept != "application/json" {
		t.Fatalf("Content-Type/Accept = %q/%q", contentType, accept)
	}
	if info.HardwareCount != 2 || info.TotalAvailable != 5 {
		t.Fatalf("ConnectionInfo = %+v", info)
	}

	// locations 路径同样由 apiBasePath 派生，且仍走只读 GET。
	locSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != PathLocations {
			t.Errorf("locations path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"locations":[{"id":7,"name":"us-east"}],"total":1}`))
	}))
	defer locSrv.Close()
	locations, err := NewClient(Config{APIKey: "k", BaseURL: locSrv.URL}).ListLocations(context.Background())
	if err != nil {
		t.Fatalf("ListLocations: %v", err)
	}
	if len(locations.Locations) != 1 || locations.Locations[0].Name != "us-east" {
		t.Fatalf("locations = %+v", locations)
	}
}

func TestClientDoesNotFollowRedirects(t *testing.T) {
	var targetHits int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&targetHits, 1)
		_, _ = w.Write([]byte(`{"hardware_types":[]}`))
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+PathHardwareTypes, http.StatusFound)
	}))
	defer redirector.Close()

	client := NewClient(Config{APIKey: "secret", BaseURL: redirector.URL})
	if _, err := client.ListHardware(context.Background()); err == nil {
		t.Fatal("3xx 必须作为错误返回，不得跟随")
	}
	if hits := atomic.LoadInt32(&targetHits); hits != 0 {
		t.Fatalf("重定向目标被访问 %d 次", hits)
	}
}

func TestClientRetriesReadsBoundedly(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&attempts, 1) < 3 {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"detail":"temporary"}`))
			return
		}
		_, _ = w.Write([]byte(`{"hardware_types":[],"total":0,"total_available":0}`))
	}))
	defer srv.Close()

	client := NewClient(Config{APIKey: "k", BaseURL: srv.URL, Timeout: 5 * time.Second})
	if _, err := client.ListHardware(context.Background()); err != nil {
		t.Fatalf("两次 5xx 后应重试成功: %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Fatalf("尝试次数 = %d，期望 3（首次 + 2 次重试）", got)
	}
}

func TestClientRespectsRetryAfter(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&attempts, 1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"detail":"slow down"}`))
			return
		}
		_, _ = w.Write([]byte(`{"hardware_types":[],"total":0,"total_available":0}`))
	}))
	defer srv.Close()

	client := NewClient(Config{APIKey: "k", BaseURL: srv.URL, Timeout: 5 * time.Second})
	started := time.Now()
	if _, err := client.ListHardware(context.Background()); err != nil {
		t.Fatalf("Retry-After 后应重试成功: %v", err)
	}
	elapsed := time.Since(started)
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Fatalf("尝试次数 = %d，期望 2", got)
	}
	// 退避必须至少等满 provider 建议的 1s（固定窗口交界抖动不计）。
	if elapsed < 900*time.Millisecond {
		t.Fatalf("退避耗时 = %v，未尊重 Retry-After=1", elapsed)
	}
}

func TestClientDoesNotRetryAuthFailures(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"detail":"invalid key test-key"}`))
	}))
	defer srv.Close()

	client := NewClient(Config{APIKey: "test-key", BaseURL: srv.URL})
	_, err := client.ListHardware(context.Background())
	if err == nil {
		t.Fatal("401 应返回错误")
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Fatalf("4xx 不得重试，实际尝试 %d 次", got)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("err = %v，期望 APIError 401", err)
	}
	// provider 若在错误体里回显了密钥，也必须被抹掉。
	if strings.Contains(apiErr.Detail, "test-key") {
		t.Fatalf("错误详情泄漏了 API Key: %q", apiErr.Detail)
	}
	if !strings.Contains(apiErr.Detail, "[REDACTED]") {
		t.Fatalf("错误详情未脱敏: %q", apiErr.Detail)
	}
}

func TestClientErrorDetailTruncated(t *testing.T) {
	long := strings.Repeat("x", 500)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"detail":"` + long + `"}`))
	}))
	defer srv.Close()

	client := NewClient(Config{APIKey: "k", BaseURL: srv.URL})
	_, err := client.ListHardware(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v", err)
	}
	if len([]rune(apiErr.Detail)) > maxErrorRunes+1 {
		t.Fatalf("detail 未截断：%d runes", len([]rune(apiErr.Detail)))
	}
}

func TestClientTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	client := NewClient(Config{APIKey: "k", BaseURL: srv.URL, Timeout: 20 * time.Millisecond})
	_, err := client.ListHardware(context.Background())
	if err == nil {
		t.Fatal("超时应返回错误")
	}
	var netErr net.Error
	if !IsTimeout(err) && !(errors.As(err, &netErr) && netErr.Timeout()) {
		t.Fatalf("err = %v，未识别为超时", err)
	}
}

func TestClientInvalidResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`not-json`))
	}))
	defer srv.Close()

	client := NewClient(Config{APIKey: "k", BaseURL: srv.URL})
	_, err := client.ListHardware(context.Background())
	var invalid *InvalidResponseError
	if !errors.As(err, &invalid) {
		t.Fatalf("err = %v，期望 InvalidResponseError", err)
	}
}
