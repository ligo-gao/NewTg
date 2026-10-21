package httpapi

// W9-sensitive：sec.sensitive.*（modules-08 F-08-18）接入中继与公告写路径。
//
// 纪律（与 relay_endpoints_test.go 一致）：不连真 PG/Redis，上游用 httptest。
// 真实 PostgreSQL 的设置在 relay_sensitive_integration_test.go（build tag
// integration）。本文件锁定：
//   - 关闭时行为不变（上游收到的 JSON 与请求语义一致）；
//   - block 命中 → 400 content_blocked，零 PreConsume、零上游；
//   - log 命中 → 放行且零明文回显；
//   - scope 作用面、大小写/Unicode/全半角/通配、多词；
//   - 公告 create/update 在 repo 之前拦截（零写入）。

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/rbac"
	"newtg/gateway/internal/store"
)

const sensitiveUpstreamBody = `{"id":"x","object":"chat.completion","created":1,"model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`

// sensitiveSettings 构造 configcenter 快照语义的四个设置键。
func sensitiveSettings(t *testing.T, enabled bool, scope map[string]bool, words []string, action string) fakeRelayConfig {
	t.Helper()
	encode := func(value any) json.RawMessage {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("marshal setting: %v", err)
		}
		return raw
	}
	return fakeRelayConfig{
		"sec.sensitive.enabled": encode(enabled),
		"sec.sensitive.scope":   encode(scope),
		"sec.sensitive.words":   encode(words),
		"sec.sensitive.action":  encode(action),
	}
}

func sensitiveChatEnv(t *testing.T, cfg ConfigReader, upstream http.HandlerFunc) (*relayTestEnv, *countingTransport) {
	t.Helper()
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)
	env := newRelayTestEnv(t, server, nil)
	base := server.Client().Transport
	if base == nil {
		base = http.DefaultTransport
	}
	counter := &countingTransport{base: base}
	env.handler.client = &http.Client{Transport: counter}
	// 默认丢弃命中日志；需要断言的用例再覆盖 handler.log。
	env.handler.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	if cfg != nil {
		env.handler.config = cfg
	}
	return env, counter
}

func noUpstream(t *testing.T) http.HandlerFunc {
	return func(http.ResponseWriter, *http.Request) {
		t.Error("sensitive block must not reach upstream")
	}
}

// ---------- 中继：关闭态 ----------

func TestSensitiveDisabledKeepsRequestUnchanged(t *testing.T) {
	body := `{"model":"gpt-test","messages":[{"role":"user","content":"contains BadWord"}]}`
	var got map[string]any
	env, counter := sensitiveChatEnv(t, sensitiveSettings(t, false,
		map[string]bool{"prompt": true}, []string{"BadWord"}, "block"),
		func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &got)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, sensitiveUpstreamBody)
		})

	w := env.doAuthed(t, http.MethodPost, "/v1/chat/completions", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if counter.count() != 1 || env.billing.preConsumeCalls() != 1 {
		t.Fatalf("关闭态应正常转发：upstream=%d PreConsume=%d", counter.count(), env.billing.preConsumeCalls())
	}
	var want map[string]any
	if err := json.Unmarshal([]byte(body), &want); err != nil {
		t.Fatalf("decode want: %v", err)
	}
	// 编码器会补 stream:false 等归一化字段；关闭态的关键契约是
	// messages/model 语义不变、命中词原样送给上游。
	if !reflect.DeepEqual(got["messages"], want["messages"]) || got["model"] != want["model"] {
		t.Fatalf("关闭态上游请求体变了：got=%v want=%v", got, want)
	}
	if _, present := got["stream"]; !present {
		t.Fatalf("归一化层应补 stream 字段：%v", got)
	}
}

// ---------- 中继：block ----------

func TestSensitiveBlockChatZeroBillingZeroUpstream(t *testing.T) {
	env, counter := sensitiveChatEnv(t, sensitiveSettings(t, true,
		map[string]bool{"prompt": true}, []string{"BadWord"}, "block"), noUpstream(t))

	body := `{"model":"gpt-test","messages":[{"role":"user","content":"hello BADWORD world"}]}`
	w := env.doAuthed(t, http.MethodPost, "/v1/chat/completions", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"code":"content_blocked"`) {
		t.Fatalf("body = %s, want content_blocked", w.Body.String())
	}
	if strings.Contains(strings.ToLower(w.Body.String()), "badword") {
		t.Fatalf("错误响应回显了命中词：%s", w.Body.String())
	}
	if counter.count() != 0 {
		t.Fatalf("block 必须零上游请求，got %d", counter.count())
	}
	if env.billing.preConsumeCalls() != 0 || env.billing.refundCount() != 0 || env.billing.terminateCount() != 0 {
		t.Fatalf("block 必须零计费：PreConsume=%d Refund=%d Terminate=%d",
			env.billing.preConsumeCalls(), env.billing.refundCount(), env.billing.terminateCount())
	}
}

func TestSensitiveBlockStreamingChat(t *testing.T) {
	env, counter := sensitiveChatEnv(t, sensitiveSettings(t, true,
		map[string]bool{"prompt": true}, []string{"bad"}, "block"), noUpstream(t))

	body := `{"model":"gpt-test","stream":true,"messages":[{"role":"user","content":"bad"}]}`
	w := env.doAuthed(t, http.MethodPost, "/v1/chat/completions", body)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"content_blocked"`) {
		t.Fatalf("stream block = %d %s, want 400 content_blocked", w.Code, w.Body.String())
	}
	if counter.count() != 0 || env.billing.preConsumeCalls() != 0 {
		t.Fatalf("stream block 零副作用失败：upstream=%d PreConsume=%d", counter.count(), env.billing.preConsumeCalls())
	}
}

func TestSensitiveBlockMessagesZeroBillingZeroUpstream(t *testing.T) {
	env, counter := messagesTestEnv(t, contract.ChannelTypeAnthropic, func(http.ResponseWriter, *http.Request) {
		t.Error("sensitive block must not reach upstream")
	})
	env.handler.config = sensitiveSettings(t, true, map[string]bool{"prompt": true}, []string{"BadWord"}, "block")

	body := `{"model":"claude-test","max_tokens":64,"messages":[{"role":"user","content":[{"type":"text","text":"hi BADWORD"}]}]}`
	w := env.do(t, http.MethodPost, "/v1/messages", body, messagesHeaders())
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"code":"content_blocked"`) {
		t.Fatalf("body = %s, want content_blocked", w.Body.String())
	}
	if strings.Contains(strings.ToLower(w.Body.String()), "badword") {
		t.Fatalf("错误响应回显了命中词：%s", w.Body.String())
	}
	if counter.count() != 0 || env.billing.preConsumeCalls() != 0 {
		t.Fatalf("messages block 零副作用失败：upstream=%d PreConsume=%d", counter.count(), env.billing.preConsumeCalls())
	}
}

// ---------- 中继：log 放行 ----------

func TestSensitiveLogForwardsOriginalText(t *testing.T) {
	var got map[string]any
	env, counter := sensitiveChatEnv(t, sensitiveSettings(t, true,
		map[string]bool{"prompt": true}, []string{"BadWord"}, "log"),
		func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &got)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, sensitiveUpstreamBody)
		})

	body := `{"model":"gpt-test","messages":[{"role":"user","content":"hello BadWord"}]}`
	w := env.doAuthed(t, http.MethodPost, "/v1/chat/completions", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if counter.count() != 1 || env.billing.preConsumeCalls() != 1 {
		t.Fatalf("log 模式应正常转发：upstream=%d PreConsume=%d", counter.count(), env.billing.preConsumeCalls())
	}
	messages, _ := got["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("上游 messages = %v", got["messages"])
	}
	content, _ := messages[0].(map[string]any)["content"].(string)
	if !strings.Contains(content, "BadWord") {
		t.Fatalf("log 模式不得改写文本，got %q", content)
	}
}

// ---------- 作用面 ----------

func TestSensitiveScopeControlsRelaySurfaces(t *testing.T) {
	upstream := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, sensitiveUpstreamBody)
	}

	t.Run("prompt only blocks user", func(t *testing.T) {
		env, counter := sensitiveChatEnv(t, sensitiveSettings(t, true,
			map[string]bool{"prompt": true, "system": false}, []string{"bad"}, "block"), upstream)
		w := env.doAuthed(t, http.MethodPost, "/v1/chat/completions",
			`{"model":"gpt-test","messages":[{"role":"user","content":"bad"}]}`)
		if w.Code != http.StatusBadRequest || counter.count() != 0 {
			t.Fatalf("prompt 面应拦截：status=%d upstream=%d", w.Code, counter.count())
		}
	})
	t.Run("system not covered", func(t *testing.T) {
		env, counter := sensitiveChatEnv(t, sensitiveSettings(t, true,
			map[string]bool{"prompt": true, "system": false}, []string{"bad"}, "block"), upstream)
		w := env.doAuthed(t, http.MethodPost, "/v1/chat/completions",
			`{"model":"gpt-test","messages":[{"role":"system","content":"bad"},{"role":"user","content":"hi"}]}`)
		if w.Code != http.StatusOK || counter.count() != 1 {
			t.Fatalf("system 面未勾选应放行：status=%d upstream=%d body=%s", w.Code, counter.count(), w.Body.String())
		}
	})
	t.Run("system only blocks system", func(t *testing.T) {
		env, counter := sensitiveChatEnv(t, sensitiveSettings(t, true,
			map[string]bool{"prompt": false, "system": true}, []string{"bad"}, "block"), upstream)
		w := env.doAuthed(t, http.MethodPost, "/v1/chat/completions",
			`{"model":"gpt-test","messages":[{"role":"system","content":"bad"},{"role":"user","content":"hi"}]}`)
		if w.Code != http.StatusBadRequest || counter.count() != 0 {
			t.Fatalf("system 面应拦截：status=%d upstream=%d", w.Code, counter.count())
		}
	})
	t.Run("prompt not covered", func(t *testing.T) {
		env, counter := sensitiveChatEnv(t, sensitiveSettings(t, true,
			map[string]bool{"prompt": false, "system": true}, []string{"bad"}, "block"), upstream)
		w := env.doAuthed(t, http.MethodPost, "/v1/chat/completions",
			`{"model":"gpt-test","messages":[{"role":"user","content":"bad"}]}`)
		if w.Code != http.StatusOK || counter.count() != 1 {
			t.Fatalf("prompt 面未勾选应放行：status=%d upstream=%d", w.Code, counter.count())
		}
	})
	t.Run("anthropic system scope", func(t *testing.T) {
		env, counter := messagesTestEnv(t, contract.ChannelTypeAnthropic, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, messagesUpstreamBody)
		})
		env.handler.config = sensitiveSettings(t, true,
			map[string]bool{"prompt": true, "system": true}, []string{"bad"}, "block")
		body := `{"model":"claude-test","max_tokens":64,"system":"bad","messages":[{"role":"user","content":"hi"}]}`
		w := env.do(t, http.MethodPost, "/v1/messages", body, messagesHeaders())
		if w.Code != http.StatusBadRequest || counter.count() != 0 {
			t.Fatalf("/v1/messages system 应拦截：status=%d upstream=%d", w.Code, counter.count())
		}
	})
}

// ---------- 匹配规则 / 不误伤 ----------

func TestSensitiveMatchVariants(t *testing.T) {
	cases := []struct {
		name  string
		words []string
		body  string
	}{
		{"case insensitive upper input", []string{"badword"}, `{"model":"gpt-test","messages":[{"role":"user","content":"BADWORD"}]}`},
		{"case insensitive lower word", []string{"BADWORD"}, `{"model":"gpt-test","messages":[{"role":"user","content":"badword"}]}`},
		{"unicode", []string{"机密"}, `{"model":"gpt-test","messages":[{"role":"user","content":"这是机密内容"}]}`},
		{"full width", []string{"secret"}, `{"model":"gpt-test","messages":[{"role":"user","content":"ＳＥＣＲＥＴ"}]}`},
		{"wildcard", []string{"暴*词"}, `{"model":"gpt-test","messages":[{"role":"user","content":"暴风词"}]}`},
		{"multi word second", []string{"other", "second"}, `{"model":"gpt-test","messages":[{"role":"user","content":"a second b"}]}`},
		{"no word boundary", []string{"ass"}, `{"model":"gpt-test","messages":[{"role":"user","content":"class"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, counter := sensitiveChatEnv(t, sensitiveSettings(t, true,
				map[string]bool{"prompt": true}, tc.words, "block"), noUpstream(t))
			w := env.doAuthed(t, http.MethodPost, "/v1/chat/completions", tc.body)
			if w.Code != http.StatusBadRequest || counter.count() != 0 {
				t.Fatalf("应命中拦截：status=%d upstream=%d body=%s", w.Code, counter.count(), w.Body.String())
			}
		})
	}
}

func TestSensitiveDoesNotScanModelNamesOrNonText(t *testing.T) {
	upstream := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, sensitiveUpstreamBody)
	}
	cases := []struct {
		name string
		body string
	}{
		{"model name", `{"model":"bad-model","messages":[{"role":"user","content":"hi"}]}`},
		{"assistant content", `{"model":"gpt-test","messages":[{"role":"assistant","content":"bad"},{"role":"user","content":"hi"}]}`},
		{"tool message", `{"model":"gpt-test","messages":[{"role":"tool","tool_call_id":"c1","content":"bad"},{"role":"user","content":"hi"}]}`},
		{"image url", `{"model":"gpt-test","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://x/bad.png"}},{"type":"text","text":"hi"}]}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, counter := sensitiveChatEnv(t, sensitiveSettings(t, true,
				map[string]bool{"prompt": true}, []string{"bad"}, "block"), upstream)
			w := env.doAuthed(t, http.MethodPost, "/v1/chat/completions", tc.body)
			if w.Code != http.StatusOK || counter.count() != 1 {
				t.Fatalf("不应误伤：status=%d upstream=%d body=%s", w.Code, counter.count(), w.Body.String())
			}
		})
	}
}

func TestSensitiveEmptyWordsOrBadConfigDoesNotBlock(t *testing.T) {
	upstream := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, sensitiveUpstreamBody)
	}
	for name, cfg := range map[string]fakeRelayConfig{
		"empty words": sensitiveSettings(t, true, map[string]bool{"prompt": true}, nil, "block"),
		"bad action":  sensitiveSettings(t, true, map[string]bool{"prompt": true}, []string{"bad"}, "replace"),
		"bad words":   {"sec.sensitive.enabled": json.RawMessage("true"), "sec.sensitive.words": json.RawMessage(`{"bad":true}`)},
	} {
		t.Run(name, func(t *testing.T) {
			env, counter := sensitiveChatEnv(t, cfg, upstream)
			w := env.doAuthed(t, http.MethodPost, "/v1/chat/completions",
				`{"model":"gpt-test","messages":[{"role":"user","content":"bad"}]}`)
			if w.Code != http.StatusOK || counter.count() != 1 {
				t.Fatalf("坏配置/空词表应回落放行：status=%d upstream=%d", w.Code, counter.count())
			}
		})
	}
}

// ---------- 日志：事件名 + 零明文 ----------

func TestSensitiveHitLogHasEventAndNoPlaintext(t *testing.T) {
	var logs bytes.Buffer
	env, _ := sensitiveChatEnv(t, sensitiveSettings(t, true,
		map[string]bool{"prompt": true}, []string{"BadWord"}, "block"), noUpstream(t))
	env.handler.log = slog.New(slog.NewTextHandler(&logs, nil))

	w := env.doAuthed(t, http.MethodPost, "/v1/chat/completions",
		`{"model":"gpt-test","messages":[{"role":"user","content":"hello BadWord"}]}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	output := logs.String()
	if !strings.Contains(output, "security.sensitive_word_hit") {
		t.Fatalf("缺少命中事件：%s", output)
	}
	if strings.Contains(strings.ToLower(output), "badword") {
		t.Fatalf("日志泄露命中词：%s", output)
	}
}

// ---------- 公告 create/update ----------

func newSensitiveAnnouncementEnv(t *testing.T, repo *fakeAnnouncementRepo,
	cfg ConfigReader, logger *slog.Logger) http.Handler {
	t.Helper()
	_, mux := newAnnouncementTestHandlers(t, AnnouncementHandlerConfig{
		Repo: repo, Config: cfg, Logger: logger,
	})
	return mux
}

func TestAnnouncementSensitiveBlockZeroWrite(t *testing.T) {
	var createCalls, updateCalls int
	repo := &fakeAnnouncementRepo{
		createFn: func(context.Context, store.AnnouncementInput, int64) (store.AnnouncementMutation, error) {
			createCalls++
			return store.AnnouncementMutation{Announcement: sampleAnnouncement()}, nil
		},
		updateFn: func(context.Context, string, store.AnnouncementInput, int64) (store.AnnouncementMutation, error) {
			updateCalls++
			return store.AnnouncementMutation{Announcement: sampleAnnouncement()}, nil
		},
	}
	cfg := sensitiveSettings(t, true, map[string]bool{"prompt": true}, []string{"BadWord"}, "block")
	mux := newSensitiveAnnouncementEnv(t, repo, cfg, nil)
	admin := announcementTestPrincipal(1, rbac.RoleAdmin, rbac.PermSettingsManage)

	cases := []struct {
		name   string
		method string
		path   string
		body   map[string]any
	}{
		{"create title", http.MethodPost, "/api/announcements",
			map[string]any{"title": "BADWORD", "content": "ok"}},
		{"create body", http.MethodPost, "/api/announcements",
			map[string]any{"title": "ok", "content": "has BadWord inside"}},
		{"update title", http.MethodPut, "/api/announcements/11111111-1111-4111-8111-111111111111",
			map[string]any{"title": "badword", "content": "ok"}},
		{"update body", http.MethodPut, "/api/announcements/11111111-1111-4111-8111-111111111111",
			map[string]any{"title": "ok", "content": "badword"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := doAnnouncementRequest(t, mux, admin, tc.method, tc.path, tc.body)
			if got.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422; body=%s", got.Code, got.Body.String())
			}
			if strings.Contains(strings.ToLower(got.Body.String()), "badword") {
				t.Fatalf("错误响应回显命中词：%s", got.Body.String())
			}
		})
	}
	if createCalls != 0 || updateCalls != 0 {
		t.Fatalf("block 必须零写库：create=%d update=%d", createCalls, updateCalls)
	}
}

func TestAnnouncementSensitiveLogWritesOriginal(t *testing.T) {
	var gotInput store.AnnouncementInput
	repo := &fakeAnnouncementRepo{
		createFn: func(_ context.Context, in store.AnnouncementInput, _ int64) (store.AnnouncementMutation, error) {
			gotInput = in
			return store.AnnouncementMutation{Announcement: sampleAnnouncement()}, nil
		},
	}
	cfg := sensitiveSettings(t, true, map[string]bool{"prompt": true}, []string{"BadWord"}, "log")
	mux := newSensitiveAnnouncementEnv(t, repo, cfg, nil)
	admin := announcementTestPrincipal(1, rbac.RoleAdmin, rbac.PermSettingsManage)

	got := doAnnouncementRequest(t, mux, admin, http.MethodPost, "/api/announcements",
		map[string]any{"title": "ok", "content": "has BadWord inside"})
	if got.Code != http.StatusCreated {
		t.Fatalf("log 模式应写入：status=%d body=%s", got.Code, got.Body.String())
	}
	if gotInput.Content != "has BadWord inside" {
		t.Fatalf("log 模式不得改写正文，got %q", gotInput.Content)
	}
}

func TestAnnouncementSensitiveScopeAndDisabled(t *testing.T) {
	cases := []struct {
		name string
		cfg  ConfigReader
	}{
		{"disabled", sensitiveSettings(t, false, map[string]bool{"prompt": true}, []string{"bad"}, "block")},
		{"prompt scope off", sensitiveSettings(t, true, map[string]bool{"prompt": false, "system": true}, []string{"bad"}, "block")},
		{"empty words", sensitiveSettings(t, true, map[string]bool{"prompt": true}, nil, "block")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			repo := &fakeAnnouncementRepo{createFn: func(context.Context, store.AnnouncementInput, int64) (store.AnnouncementMutation, error) {
				called = true
				return store.AnnouncementMutation{Announcement: sampleAnnouncement()}, nil
			}}
			mux := newSensitiveAnnouncementEnv(t, repo, tc.cfg, nil)
			admin := announcementTestPrincipal(1, rbac.RoleAdmin, rbac.PermSettingsManage)
			got := doAnnouncementRequest(t, mux, admin, http.MethodPost, "/api/announcements",
				map[string]any{"title": "bad", "content": "bad"})
			if got.Code != http.StatusCreated || !called {
				t.Fatalf("不应拦截：status=%d called=%v body=%s", got.Code, called, got.Body.String())
			}
		})
	}
}

func TestAnnouncementSensitiveHitLogHasNoPlaintext(t *testing.T) {
	var logs bytes.Buffer
	repo := &fakeAnnouncementRepo{}
	mux := newSensitiveAnnouncementEnv(t, repo,
		sensitiveSettings(t, true, map[string]bool{"prompt": true}, []string{"BadWord"}, "block"),
		slog.New(slog.NewTextHandler(&logs, nil)))
	admin := announcementTestPrincipal(1, rbac.RoleAdmin, rbac.PermSettingsManage)

	got := doAnnouncementRequest(t, mux, admin, http.MethodPost, "/api/announcements",
		map[string]any{"title": "ok", "content": "BadWord"})
	if got.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", got.Code)
	}
	output := logs.String()
	if !strings.Contains(output, "security.sensitive_word_hit") {
		t.Fatalf("缺少命中事件：%s", output)
	}
	if strings.Contains(strings.ToLower(output), "badword") {
		t.Fatalf("日志泄露命中词：%s", output)
	}
}
