package tasks

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"newtg/gateway/internal/plugin"
)

const contentTestPluginSource = `globalThis.__NEWTG_PLUGIN__ = {
  apiVersion: 1,
  key: "sample",
  name: "Sample",
  version: "1.0.0",
  author: { name: "NewTG" },
  description: { en: "Sample task plugin", zh: "示例任务插件" },
  models: ["sample-model"],
  fetchMode: "per_task",
  auth: { type: "api_key" },
  channelTypes: [],
  routes: [],
  protocols: {},
  usageSchema: {},
  usageProfiles: [],
  usageExamples: [],
  requiredCapabilities: [],
  buildSubmitRequest: function (ctx) { return { url: ctx.baseUrl + "/tasks", method: "POST" }; },
  parseSubmitResponse: function (resp) { return { taskId: "up_1" }; },
  buildQueryRequest: function (ctx) { return { url: ctx.baseUrl + "/q/" + ctx.taskId, method: "GET" }; },
  parseTaskResult: function () { return { state: "IN_PROGRESS" }; },
  listArtifacts: function (ctx) { return []; },
  buildContentRequest: function (ctx) {
    return { url: ctx.baseUrl + "/content/" + ctx.artifactKey, method: "GET" };
  }
};`

func compileContentPlugin(t *testing.T) *plugin.CompiledPlugin {
	t.Helper()
	compiled, err := plugin.Compile(context.Background(), contentTestPluginSource, plugin.CompileOptions{
		ExpectedKey: "sample", Layer: plugin.LayerFactory,
	})
	if err != nil {
		t.Fatalf("compile content plugin: %v", err)
	}
	return compiled
}

func TestArtifactSignerVerify(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	s := NewArtifactSigner("secret", 0)
	access := s.Sign("sample", "task_abc", "v1", now)

	if !s.Verify("sample", "task_abc", "v1", access, now.Add(100*time.Hour)) {
		t.Fatalf("long-lived ticket must verify after any time")
	}
	if s.Verify("sample", "task_abc", "v2", access, now) {
		t.Fatalf("ticket must be bound to the artifact key")
	}
	if s.Verify("other", "task_abc", "v1", access, now) {
		t.Fatalf("ticket must be bound to the plugin key")
	}
	if s.Verify("sample", "task_other", "v1", access, now) {
		t.Fatalf("ticket must be bound to the task id")
	}
	if s.Verify("sample", "task_abc", "v1", access+"x", now) {
		t.Fatalf("tampered ticket must fail")
	}
	if s.Verify("sample", "task_abc", "v1", "", now) {
		t.Fatalf("empty ticket must fail")
	}
	// 不同密钥（即轮换 CREDENTIAL_ENCRYPTION_KEY）签发的票据必须失效。
	if NewArtifactSigner("rotated", 0).Verify("sample", "task_abc", "v1", access, now) {
		t.Fatalf("rotating the secret must revoke tickets")
	}

	// ttl>0 时过期校验生效（TTL 只作为未来收窄位，生产为 0）。
	expiring := NewArtifactSigner("secret", time.Hour)
	short := expiring.Sign("sample", "task_abc", "v1", now)
	if !expiring.Verify("sample", "task_abc", "v1", short, now.Add(30*time.Minute)) {
		t.Fatalf("ticket must verify inside its TTL")
	}
	if expiring.Verify("sample", "task_abc", "v1", short, now.Add(2*time.Hour)) {
		t.Fatalf("expired ticket must fail")
	}
}

func TestContentURLContainsEscapedTicket(t *testing.T) {
	now := time.Now()
	s := NewArtifactSigner("secret", 0)
	task := &Task{PublicTaskID: "task_abc"}
	got := s.ContentURL("https://gw.example.com/", "sample", task, "art/1", now)
	if !strings.HasPrefix(got, "https://gw.example.com/v1/tasks/sample/artifacts/art%2F1/content?") {
		t.Fatalf("unexpected URL: %s", got)
	}
	if !strings.Contains(got, "task_id=task_abc") || !strings.Contains(got, "access=v1.") {
		t.Fatalf("URL missing ticket params: %s", got)
	}
	// 相对基址（未配置公网基址）同样可用。
	rel := s.ContentURL("", "sample", task, "v1", now)
	if !strings.HasPrefix(rel, "/v1/tasks/sample/artifacts/v1/content?") {
		t.Fatalf("relative URL: %s", rel)
	}
}

func TestRedactAccess(t *testing.T) {
	cases := map[string]string{
		"/v1/tasks/sample/artifacts/v1/content?task_id=task_abc&access=v1.123.abc": "access=REDACTED",
		"https://gw.example.com/x?access=v1.1.sig&task_id=t":                       "access=REDACTED",
		"https://gw.example.com/x?task_id=t":                                       "https://gw.example.com/x?task_id=t",
		"access=v1.evil":                                                           "REDACTED",
		"/plain/path":                                                              "/plain/path",
	}
	for raw, want := range cases {
		got := RedactAccess(raw)
		if want == "access=REDACTED" {
			if strings.Contains(got, "v1.123.abc") || strings.Contains(got, "v1.1.sig") {
				t.Fatalf("access ticket leaked for %q: %s", raw, got)
			}
			if !strings.Contains(got, "access=REDACTED") {
				t.Fatalf("redaction marker missing for %q: %s", raw, got)
			}
			continue
		}
		if got != want && !(want == "REDACTED" && got == "REDACTED") {
			t.Fatalf("RedactAccess(%q)=%q, want %q", raw, got, want)
		}
	}
}

func TestFetchContentStreamsLargeBody(t *testing.T) {
	const size = 8 << 20 // 8MiB：远超 JSON 控制面的 4MiB 上限
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		w.Header().Set("Content-Type", "video/mp4")
		chunk := strings.Repeat("x", 32<<10)
		for written := 0; written < size; written += len(chunk) {
			_, _ = io.WriteString(w, chunk)
		}
	}))
	defer srv.Close()

	compiled := compileContentPlugin(t)
	resp, err := FetchContent(context.Background(), NewOutboundClient(), ContentInput{
		Plugin:         compiled,
		BaseURL:        srv.URL,
		UpstreamTaskID: "up_1",
		ArtifactKey:    "v1",
		Method:         http.MethodGet,
		AllowPrivate:   true,
	})
	if err != nil {
		t.Fatalf("FetchContent: %v", err)
	}
	defer resp.Body.Close()
	if resp.Headers["Content-Type"] != "video/mp4" {
		t.Fatalf("content-type = %q", resp.Headers["Content-Type"])
	}
	read, err := io.Copy(io.Discard, resp.Body)
	if err != nil {
		t.Fatalf("stream read: %v", err)
	}
	if read != size {
		t.Fatalf("read %d bytes, want %d", read, size)
	}
}

func TestFetchContentHeadUsesHeadMethod(t *testing.T) {
	var method string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Length", "123")
	}))
	defer srv.Close()

	compiled := compileContentPlugin(t)
	resp, err := FetchContent(context.Background(), NewOutboundClient(), ContentInput{
		Plugin: compiled, BaseURL: srv.URL, UpstreamTaskID: "up_1",
		ArtifactKey: "v1", Method: http.MethodHead, AllowPrivate: true,
	})
	if err != nil {
		t.Fatalf("FetchContent: %v", err)
	}
	resp.Body.Close()
	if method != http.MethodHead {
		t.Fatalf("upstream method = %s, want HEAD", method)
	}
	if resp.Headers["Content-Length"] != "123" {
		t.Fatalf("headers not propagated: %#v", resp.Headers)
	}
}

func TestFetchContentRejectsOversizeByContentLength(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "99999999999")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	compiled := compileContentPlugin(t)
	_, err := FetchContent(context.Background(), NewOutboundClient(), ContentInput{
		Plugin: compiled, BaseURL: srv.URL, UpstreamTaskID: "up_1",
		ArtifactKey: "v1", AllowPrivate: true,
	})
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("want ErrResponseTooLarge, got %v", err)
	}
}

func TestFetchContentEnforcesHostWhitelist(t *testing.T) {
	source := strings.Replace(contentTestPluginSource,
		`return { url: ctx.baseUrl + "/content/" + ctx.artifactKey, method: "GET" };`,
		`return { url: "https://evil.example.com/content", method: "GET" };`, 1)
	compiled, err := plugin.Compile(context.Background(), source, plugin.CompileOptions{
		ExpectedKey: "sample", Layer: plugin.LayerFactory,
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	_, err = FetchContent(context.Background(), NewOutboundClient(), ContentInput{
		Plugin: compiled, BaseURL: "https://api.example.com", UpstreamTaskID: "up_1",
		ArtifactKey: "v1",
	})
	if !errors.Is(err, ErrOutboundHost) {
		t.Fatalf("want ErrOutboundHost, got %v", err)
	}
}

func TestFetchContentRejectsRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()

	compiled := compileContentPlugin(t)
	_, err := FetchContent(context.Background(), NewOutboundClient(), ContentInput{
		Plugin: compiled, BaseURL: srv.URL, UpstreamTaskID: "up_1",
		ArtifactKey: "v1", AllowPrivate: true,
	})
	if !errors.Is(err, ErrOutboundRedirect) {
		t.Fatalf("want ErrOutboundRedirect, got %v", err)
	}
}
