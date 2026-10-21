package httpapi

// W7/M4-a：端点层把执行层失败事实转交 reporter 的装配测试。
//
// 判定/状态迁移在 channelhealth 的测试里；这里只验证「生产装配的
// RelayHandler 真的把每轮非 2xx（含 AutoBan 字段）送到了 reporter」。

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"newtg/gateway/internal/contract"
)

type reportedAttemptFailure struct {
	channelID  int
	autoBan    bool
	class      contract.ErrorClass
	statusCode int
	body       string
}

type recordingAttemptReporter struct {
	mu     sync.Mutex
	events []reportedAttemptFailure
}

func (r *recordingAttemptReporter) ReportAttemptFailure(_ context.Context, channelID int, autoBan bool,
	class contract.ErrorClass, statusCode int, body []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, reportedAttemptFailure{
		channelID:  channelID,
		autoBan:    autoBan,
		class:      class,
		statusCode: statusCode,
		body:       string(body),
	})
}

func (r *recordingAttemptReporter) snapshot() []reportedAttemptFailure {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]reportedAttemptFailure(nil), r.events...)
}

// TestRelayReportsUpstreamAuthFailure：上游 401 → 客户端 401，同时 reporter
// 收到 channelID/AutoBan/ErrorClass/status/body 五元组。
func TestRelayReportsUpstreamAuthFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"invalid api key"}}`)
	}))
	defer upstream.Close()

	env := newRelayTestEnv(t, upstream, nil)
	// fake 渠道默认 AutoBan=false（store.Channel 零值）；本用例要求 true。
	env.channels.channels[relayTestChannelID].AutoBan = true
	reporter := &recordingAttemptReporter{}
	env.handler.attemptReporter = reporter

	w := env.doAuthed(t, http.MethodPost, "/v1/chat/completions",
		`{"model":"gpt-test","messages":[{"role":"user","content":"hello"}]}`)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", w.Code, w.Body.String())
	}

	events := reporter.snapshot()
	if len(events) != 1 {
		t.Fatalf("reporter 收到 %d 条事件, want 1: %+v", len(events), events)
	}
	got := events[0]
	if got.channelID != int(relayTestChannelID) || !got.autoBan ||
		got.class != contract.ErrUpstreamAuth || got.statusCode != http.StatusUnauthorized {
		t.Fatalf("事件 = %+v", got)
	}
	if got.body != `{"error":{"message":"invalid api key"}}` {
		t.Fatalf("body = %q", got.body)
	}
	// GC-6：错误响应不得回显上游凭据。
	if body := w.Body.String(); strings.Contains(body, upstreamCredential) {
		t.Fatalf("响应体泄漏凭据: %s", body)
	}
}

// TestRelayReportsAutoBanFalseFromStore：store 行的 auto_ban=false 必须原样
// 进入 reporter（判定层据此不禁用），而不是在端点层被丢掉。
//
// 注意客户端状态码与上游状态码可以不同：§9.3 把整个 ErrUpstreamAuth 映射为
// 401（即使上游是 403），reporter 必须拿到**上游真实状态码**做判定。
func TestRelayReportsAutoBanFalseFromStore(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":"forbidden"}`)
	}))
	defer upstream.Close()

	env := newRelayTestEnv(t, upstream, nil)
	env.channels.channels[relayTestChannelID].AutoBan = false
	reporter := &recordingAttemptReporter{}
	env.handler.attemptReporter = reporter

	w := env.doAuthed(t, http.MethodPost, "/v1/chat/completions",
		`{"model":"gpt-test","messages":[{"role":"user","content":"hello"}]}`)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401（ErrUpstreamAuth 的冻结映射）", w.Code)
	}
	events := reporter.snapshot()
	if len(events) != 1 || events[0].autoBan {
		t.Fatalf("autoBan 透传错误: %+v", events)
	}
	if events[0].statusCode != http.StatusForbidden {
		t.Fatalf("上报状态码 = %d, want 403（上游真实状态）", events[0].statusCode)
	}
}

// TestRelayDoesNotReportSuccessfulRequest：2xx 不产生自动禁用上报。
func TestRelayDoesNotReportSuccessfulRequest(t *testing.T) {
	const upstreamJSON = `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"upstream-model","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, upstreamJSON)
	}))
	defer upstream.Close()

	env := newRelayTestEnv(t, upstream, nil)
	reporter := &recordingAttemptReporter{}
	env.handler.attemptReporter = reporter

	w := env.doAuthed(t, http.MethodPost, "/v1/chat/completions",
		`{"model":"gpt-test","messages":[{"role":"user","content":"hello"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if events := reporter.snapshot(); len(events) != 0 {
		t.Fatalf("2xx 不应上报: %+v", events)
	}
}
