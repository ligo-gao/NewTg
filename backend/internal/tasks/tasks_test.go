package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"newtg/gateway/internal/plugin"
)

const testPluginSource = `globalThis.__NEWTG_PLUGIN__ = {
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
  usageSchema: {
    seconds: {
      type: "number",
      unit: "second",
      description: { en: "Video generation unit price", zh: "视频生成单价" }
    }
  },
  usageProfiles: [],
  usageExamples: [{ seconds: 5 }],
  requiredCapabilities: [],
  buildSubmitRequest: function (ctx) {
    return {
      url: ctx.baseUrl + "/tasks",
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: { model: ctx.upstreamModel, prompt: ctx.prompt }
    };
  },
  parseSubmitResponse: function (resp) {
    var body = JSON.parse(resp.body || "{}");
    return { taskId: body.id, taskData: body, state: body.state || "SUBMITTED" };
  },
  buildQueryRequest: function (ctx) {
    return { url: ctx.baseUrl + "/tasks/" + ctx.taskId, method: "GET" };
  },
  parseTaskResult: function (ctx, body) {
    return { state: "UNKNOWN" };
  }
};`

func compileTestPlugin(t *testing.T, source string) *plugin.CompiledPlugin {
	t.Helper()
	compiled, err := plugin.Compile(context.Background(), source, plugin.CompileOptions{
		ExpectedKey: "sample", Layer: plugin.LayerFactory,
	})
	if err != nil {
		t.Fatalf("compile plugin: %v", err)
	}
	return compiled
}

func TestParseState(t *testing.T) {
	cases := map[string]State{
		"NOT_START":   StateNotStart,
		"submitted":   StateSubmitted,
		"QUEUED":      StateQueued,
		"RUNNING":     StateInProgress,
		"SUCCEEDED":   StateSuccess,
		"FAILED":      StateFailure,
		"nonsense":    StateUnknown,
		"":            StateUnknown,
		"  SUCCESS  ": StateSuccess,
	}
	for raw, want := range cases {
		if got := ParseState(raw); got != want {
			t.Fatalf("ParseState(%q)=%q，want %q", raw, got, want)
		}
	}
	if StateSuccess.Terminal() != true || StateInProgress.Terminal() != false {
		t.Fatalf("terminal classification drifted")
	}
}

func TestNewPublicTaskID(t *testing.T) {
	seen := map[string]struct{}{}
	for i := 0; i < 100; i++ {
		id, err := NewPublicTaskID()
		if err != nil {
			t.Fatalf("NewPublicTaskID: %v", err)
		}
		if !strings.HasPrefix(id, PublicTaskIDPrefix) || len(id) != len(PublicTaskIDPrefix)+32 {
			t.Fatalf("unexpected id shape: %q", id)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate public task id %q", id)
		}
		seen[id] = struct{}{}
	}
}

func TestProjectArtifacts(t *testing.T) {
	data := map[string]any{
		"artifacts": []any{
			map[string]any{"key": "v1", "type": "video", "mimeType": "video/mp4"},
			map[string]any{"id": "v2", "kind": "image"},
			map[string]any{"mime_type": "audio/mpeg"},
			map[string]any{"key": "v1", "type": "duplicate"},
			"not-an-object",
		},
	}
	got := ProjectArtifacts(data)
	want := []ArtifactView{
		{Key: "v1", Type: "video", MIMEType: "video/mp4"},
		{Key: "v2", Type: "image"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("artifacts = %#v，want %#v", got, want)
	}
	if ProjectArtifacts(map[string]any{}) != nil {
		t.Fatalf("empty data must project no artifacts")
	}
}

func TestProjectUsageFiltersDeclaredFields(t *testing.T) {
	manifest := &plugin.Manifest{
		UsageSchema: map[string]plugin.UsageField{"seconds": {Type: "number"}},
		UsageProfiles: []plugin.UsageProfile{{
			Schema: map[string]plugin.UsageField{"mode": {Type: "enum"}},
		}},
	}
	declared := DeclaredUsageFields(manifest)
	data := map[string]any{"usage": map[string]any{
		"seconds": float64(5),
		"mode":    "pro",
		"quota":   float64(999), // 未声明字段必须被丢弃（不泄露额度/价格）
		"latency": float64(12),
	}}
	got := ProjectUsage(data, declared)
	if got["seconds"] != float64(5) || got["mode"] != "pro" {
		t.Fatalf("declared usage missing: %#v", got)
	}
	if _, ok := got["quota"]; ok {
		t.Fatalf("undeclared field leaked: %#v", got)
	}
	if _, ok := got["latency"]; ok {
		t.Fatalf("undeclared field leaked: %#v", got)
	}
	if ProjectUsage(data, nil) != nil {
		t.Fatalf("no schema must project no usage")
	}
}

func TestPluginGenerationIsVersionDerived(t *testing.T) {
	a := compileTestPlugin(t, testPluginSource)
	b := compileTestPlugin(t, testPluginSource)
	if PluginGeneration(a) == 0 || PluginGeneration(a) != PluginGeneration(b) {
		t.Fatalf("generation must be stable for identical version+source")
	}
	changed := strings.Replace(testPluginSource, `version: "1.0.0"`, `version: "1.0.1"`, 1)
	c := compileTestPlugin(t, changed)
	if PluginGeneration(a) == PluginGeneration(c) {
		t.Fatalf("version change must change the generation")
	}
}

func TestValidateOutboundURL(t *testing.T) {
	allowed := []string{"api.example.com", "cdn.example.com:8443"}
	cases := []struct {
		name string
		url  string
		ok   bool
	}{
		{"exact host", "https://api.example.com/v1/tasks", true},
		{"host with any port", "https://api.example.com:9443/v1/tasks", true},
		{"host:port exact", "https://cdn.example.com:8443/x", true},
		{"host:port mismatch", "https://cdn.example.com:9000/x", false},
		{"not whitelisted", "https://evil.example.com/x", false},
		{"scheme ftp", "ftp://api.example.com/x", false},
		{"userinfo", "https://user:pass@api.example.com/x", false},
		{"loopback literal", "http://127.0.0.1/x", false},
		{"ipv6 loopback", "http://[::1]/x", false},
		{"private literal", "http://10.1.2.3/x", false},
		{"link local", "http://169.254.169.254/latest/meta-data", false},
		{"multicast", "http://224.0.0.1/x", false},
		{"mapped loopback", "http://[::ffff:127.0.0.1]/x", false},
		{"unspecified", "http://0.0.0.0/x", false},
	}
	for _, c := range cases {
		_, err := ValidateOutboundURL(c.url, allowed)
		if c.ok && err != nil {
			t.Fatalf("%s: unexpected error %v", c.name, err)
		}
		if !c.ok {
			if err == nil {
				t.Fatalf("%s: expected rejection for %s", c.name, c.url)
			}
			if strings.Contains(c.url, "127.0.0.1") || strings.Contains(c.url, "10.1.2.3") ||
				strings.Contains(c.url, "169.254") || strings.Contains(c.url, "224.0.0.1") ||
				strings.Contains(c.url, "0.0.0.0") || strings.Contains(c.url, "[::1]") {
				if !errors.Is(err, ErrOutboundBlockedAddr) && !errors.Is(err, ErrOutboundHost) {
					t.Fatalf("%s: want blocked-address error, got %v", c.name, err)
				}
			}
		}
	}
}

func TestDoOutboundRejectsRedirectAndEnforcesLimit(t *testing.T) {
	var tooLarge bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tooLarge {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(strings.Repeat("x", MaxResponseBytes+1)))
			return
		}
		http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()

	client := NewOutboundClient()
	desc := &RequestDescriptor{URL: srv.URL + "/tasks", Method: http.MethodGet}
	allowed := []string{strings.TrimPrefix(srv.URL, "http://")}
	_, err := DoOutbound(context.Background(), client, desc, allowed, true)
	if !errors.Is(err, ErrOutboundRedirect) {
		t.Fatalf("redirect must be rejected, got %v", err)
	}

	tooLarge = true
	_, err = DoOutbound(context.Background(), client, desc, allowed, true)
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("oversize response must be rejected, got %v", err)
	}
}

func TestSubmitPipeline(t *testing.T) {
	compiled := compileTestPlugin(t, testPluginSource)
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("missing content-type header")
		}
		w.Header().Set("Content-Type", "application/json")
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		// 回显上游实际收到的 model：请求体里的 "model":"spoofed" 不得覆盖宿主身份。
		fmt.Fprintf(w, `{"id":"up_1","state":"QUEUED","model":%q}`, body["model"])
	}))
	defer srv.Close()

	result, err := Submit(context.Background(), NewOutboundClient(), SubmitInput{
		Plugin: compiled,
		Channel: ChannelTarget{
			ID:           7,
			BaseURL:      srv.URL,
			ModelMapping: map[string]string{"sample-model": "upstream-model"},
		},
		Model:        "sample-model",
		Body:         map[string]any{"prompt": "hi", "model": "spoofed"},
		BaseURL:      srv.URL,
		APIKey:       "sk-secret",
		AllowPrivate: true,
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if result.UpstreamTaskID != "up_1" || result.State != StateQueued || result.RawState != "QUEUED" {
		t.Fatalf("unexpected result: %#v", result)
	}
	if hits != 1 {
		t.Fatalf("upstream hits = %d, want 1", hits)
	}
	// 请求体里的 model 是「不可信 body」，必须被宿主身份覆盖：测试插件回写
	// ctx.upstreamModel，若 body 覆盖成功上游会收到 spoofed。
	if result.Data["model"] != "upstream-model" {
		t.Fatalf("body overrode host identity: %#v", result.Data)
	}
}

func TestSubmitRejectsHostOutsideWhitelist(t *testing.T) {
	source := strings.Replace(testPluginSource,
		`url: ctx.baseUrl + "/tasks",`, `url: "https://evil.example.com/tasks",`, 1)
	compiled := compileTestPlugin(t, source)
	_, err := Submit(context.Background(), NewOutboundClient(), SubmitInput{
		Plugin:  compiled,
		Channel: ChannelTarget{ID: 1, BaseURL: "https://api.example.com"},
		Model:   "sample-model",
		Body:    map[string]any{},
		BaseURL: "https://api.example.com",
	})
	if !errors.Is(err, ErrOutboundHost) {
		t.Fatalf("want ErrOutboundHost, got %v", err)
	}
}

func TestSubmitPropagatesUpstreamStatus(t *testing.T) {
	compiled := compileTestPlugin(t, testPluginSource)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	_, err := Submit(context.Background(), NewOutboundClient(), SubmitInput{
		Plugin:       compiled,
		Channel:      ChannelTarget{ID: 1, BaseURL: srv.URL},
		Model:        "sample-model",
		Body:         map[string]any{},
		BaseURL:      srv.URL,
		AllowPrivate: true,
	})
	if !errors.Is(err, ErrUpstreamStatus) {
		t.Fatalf("want ErrUpstreamStatus, got %v", err)
	}
}

func TestDecodeSubmitResponseImmediateTerminal(t *testing.T) {
	result, err := decodeSubmitResponse(map[string]any{
		"taskId":    "up_9",
		"immediate": map[string]any{"status": "SUCCESS"},
		"taskData":  map[string]any{"artifacts": []any{}},
	})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !result.Immediate || result.State != StateSuccess || result.Progress != 100 {
		t.Fatalf("immediate terminal not recognized: %#v", result)
	}
	if _, err := decodeSubmitResponse(map[string]any{"state": "SUCCESS"}); !errors.Is(err, ErrSubmitResponse) {
		t.Fatalf("missing taskId must fail, got %v", err)
	}
}

func TestDecodeRequestDescriptor(t *testing.T) {
	desc, err := DecodeRequestDescriptor(map[string]any{
		"url": "https://api.example.com/v1/tasks", "method": "post",
		"headers": map[string]any{"X-Test": "1"}, "body": map[string]any{"a": 1},
	})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if desc.Method != "POST" || desc.Headers["X-Test"] != "1" || len(desc.Body) == 0 {
		t.Fatalf("unexpected descriptor: %#v", desc)
	}
	if _, err := DecodeRequestDescriptor(map[string]any{"url": "https://x", "responseType": "sse"}); !errors.Is(err, ErrUnsupportedResponseType) {
		t.Fatalf("sse must be rejected in T6a, got %v", err)
	}
}
