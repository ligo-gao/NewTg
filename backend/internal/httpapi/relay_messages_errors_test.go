package httpapi

// W18-A：Anthropic 客户端保真（R-W18-04/05/06）。
//
// 本文件锁定三类冻结行为：
//   - /v1/messages 族（含 count_tokens）的全部错误出口是 Claude 形
//     `{"type":"error","error":{"type","message"}}`，且 X-Request-ID 仍在；
//   - OpenAI 形端点（chat 等）不回归；
//   - anthropic-beta 按白名单转发，count_tokens 零计费。
//
// 全部为 HTTP/适配器单测：上游用 httptest 假服务，不连真 PG/Redis。
// 真实计费引擎的零计费断言在 relay_count_tokens_integration_test.go。

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/ratelimit"
	"newtg/gateway/internal/relay/adapters"
	"newtg/gateway/internal/relay/billingseam"
)

// assertClaudeError 断言响应是 Claude 形错误信封（R-W18-04）。
//
// 除形状外还锁定三条「不得改变」的语义：
//   - 状态码原样；
//   - X-Request-ID 头存在（message 末尾的 requestId 与之同源）；
//   - 顶层与 error 对象内都**没有** OpenAI 的 code 字段（两种信封不得混用）。
func assertClaudeError(t *testing.T, w *httptest.ResponseRecorder, wantStatus int, wantType string) {
	t.Helper()
	if w.Code != wantStatus {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, wantStatus, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content-type = %q, want application/json", ct)
	}
	if rid := w.Header().Get("X-Request-ID"); rid == "" {
		t.Fatal("X-Request-ID 缺失")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("body 不是 JSON: %v (%s)", err, w.Body.String())
	}
	if _, ok := raw["code"]; ok {
		t.Fatalf("Claude 形不得有顶层 code 字段：%s", w.Body.String())
	}
	var envelope claudeErrorEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode Claude envelope: %v", err)
	}
	if envelope.Type != claudeErrorTopLevel {
		t.Fatalf("type = %q, want %q", envelope.Type, claudeErrorTopLevel)
	}
	if envelope.Error.Type != wantType {
		t.Fatalf("error.type = %q, want %q; body=%s", envelope.Error.Type, wantType, w.Body.String())
	}
	if strings.TrimSpace(envelope.Error.Message) == "" {
		t.Fatal("error.message 为空")
	}
	var inner map[string]json.RawMessage
	if err := json.Unmarshal(raw["error"], &inner); err != nil {
		t.Fatalf("decode error object: %v", err)
	}
	if _, ok := inner["code"]; ok {
		t.Fatalf("Claude 形 error 对象内不得有 code：%s", w.Body.String())
	}
}

// assertRateLimitedClaude429 断言 /v1/messages 的 429 在 Claude 形下仍保留
// 限流的可重试信号：Retry-After 与 message 中的秒数一致。
func assertRateLimitedClaude429(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	assertClaudeError(t, w, http.StatusTooManyRequests, "rate_limit_error")
	retry := w.Header().Get("Retry-After")
	if retry == "" {
		t.Fatal("Retry-After 缺失")
	}
	var envelope claudeErrorEnvelope
	_ = json.Unmarshal(w.Body.Bytes(), &envelope)
	if !strings.HasPrefix(envelope.Error.Message, "rate limit exceeded; retry in "+retry+"s") {
		t.Fatalf("message = %q，期望固定文案且秒数与 Retry-After 一致", envelope.Error.Message)
	}
}

// assertRateLimitCheckFailedClaude500 断言限流器故障的 500 是 api_error，
// 且不设 Retry-After（R-M5-10 的语义在信封改造后不变）。
func assertRateLimitCheckFailedClaude500(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	assertClaudeError(t, w, http.StatusInternalServerError, "api_error")
	if got := w.Header().Get("Retry-After"); got != "" {
		t.Fatalf("500 不得带 Retry-After，got %q", got)
	}
}

// TestMessagesErrorEnvelopesAreClaude 覆盖 400/401/402/403/404/503 六种入口错误：
// 状态码原样、形状是 Claude、零计费零上游。
func TestMessagesErrorEnvelopesAreClaude(t *testing.T) {
	t.Run("400 invalid body", func(t *testing.T) {
		env, counter := messagesTestEnv(t, contract.ChannelTypeAnthropic, func(http.ResponseWriter, *http.Request) {
			t.Error("upstream must not be called")
		})
		w := env.do(t, http.MethodPost, "/v1/messages", `{"messages":[]}`, messagesHeaders())
		assertClaudeError(t, w, http.StatusBadRequest, "invalid_request_error")
		if counter.count() != 0 || env.billing.preConsumeCalls() != 0 {
			t.Fatalf("400 must be side-effect free: upstream=%d pre=%d", counter.count(), env.billing.preConsumeCalls())
		}
	})

	t.Run("401 invalid token", func(t *testing.T) {
		env, counter := messagesTestEnv(t, contract.ChannelTypeAnthropic, func(http.ResponseWriter, *http.Request) {
			t.Error("upstream must not be called")
		})
		w := env.do(t, http.MethodPost, "/v1/messages",
			`{"model":"claude-test","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
			map[string]string{"x-api-key": "nkt-not-a-real-token"})
		assertClaudeError(t, w, http.StatusUnauthorized, "authentication_error")
		if counter.count() != 0 || env.billing.preConsumeCalls() != 0 {
			t.Fatalf("401 must be side-effect free: upstream=%d pre=%d", counter.count(), env.billing.preConsumeCalls())
		}
	})

	t.Run("402 insufficient quota", func(t *testing.T) {
		env, counter := messagesTestEnv(t, contract.ChannelTypeAnthropic, func(http.ResponseWriter, *http.Request) {
			t.Error("upstream must not be called")
		})
		// 桩的上限小于 max_tokens → PreConsume 返回 402 语义。
		env.billing.stub = billingseam.NewStubWithLimit(1)
		w := env.do(t, http.MethodPost, "/v1/messages",
			`{"model":"claude-test","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
			messagesHeaders())
		assertClaudeError(t, w, http.StatusPaymentRequired, "invalid_request_error")
		if !strings.Contains(w.Body.String(), "insufficient quota") {
			t.Fatalf("402 message 必须是固定文案 insufficient quota：%s", w.Body.String())
		}
		if counter.count() != 0 {
			t.Fatalf("402 must not reach upstream: %d", counter.count())
		}
	})

	t.Run("403 token model whitelist", func(t *testing.T) {
		env, counter := messagesTestEnv(t, contract.ChannelTypeAnthropic, func(http.ResponseWriter, *http.Request) {
			t.Error("upstream must not be called")
		})
		meta := env.tokens.byHash[auth.HashTokenKey(relayTestToken)]
		meta.ModelLimits = []string{"some-other-model"}
		w := env.do(t, http.MethodPost, "/v1/messages",
			`{"model":"claude-test","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
			messagesHeaders())
		assertClaudeError(t, w, http.StatusForbidden, "permission_error")
		if counter.count() != 0 {
			t.Fatalf("403 must not reach upstream: %d", counter.count())
		}
	})

	t.Run("404 disabled model", func(t *testing.T) {
		env, _ := messagesTestEnv(t, contract.ChannelTypeAnthropic, func(http.ResponseWriter, *http.Request) {
			t.Error("upstream must not be called")
		})
		env.handler.modelStatus = &fakeRelayModelStatus{
			status: map[string]int16{messagesTestModel: modelPricingStatusDisabled},
		}
		w := env.do(t, http.MethodPost, "/v1/messages",
			`{"model":"claude-test","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
			messagesHeaders())
		assertClaudeError(t, w, http.StatusNotFound, "not_found_error")
	})

	t.Run("502 upstream server error", func(t *testing.T) {
		env, counter := messagesTestEnv(t, contract.ChannelTypeAnthropic, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"api_error","message":"boom"}}`)
		})
		w := env.do(t, http.MethodPost, "/v1/messages",
			`{"model":"claude-test","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
			messagesHeaders())
		assertClaudeError(t, w, http.StatusBadGateway, "api_error")
		if counter.count() == 0 {
			t.Fatal("502 用例应实际触达上游（重试预算内）")
		}
	})
}

// TestClaudeErrorTypeMapping 锁住状态码 → Claude error.type 的冻结映射，
// 包括 HTTP 用例难以稳定构造的 500/502/503。
func TestClaudeErrorTypeMapping(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{http.StatusBadRequest, "invalid_request_error"},
		{http.StatusUnprocessableEntity, "invalid_request_error"},
		{http.StatusUnauthorized, "authentication_error"},
		{http.StatusPaymentRequired, "invalid_request_error"},
		{http.StatusForbidden, "permission_error"},
		{http.StatusNotFound, "not_found_error"},
		{http.StatusRequestEntityTooLarge, "request_too_large"},
		{http.StatusTooManyRequests, "rate_limit_error"},
		{http.StatusInternalServerError, "api_error"},
		{http.StatusBadGateway, "api_error"},
		{http.StatusServiceUnavailable, "api_error"},
	}
	for _, tc := range cases {
		if got := claudeErrorTypeForStatus(tc.status); got != tc.want {
			t.Fatalf("claudeErrorTypeForStatus(%d) = %q, want %q", tc.status, got, tc.want)
		}
	}
}

// TestChatErrorEnvelopeStaysOpenAI 是防回归：chat 的 400 仍是 OpenAI 形
// （type=new_api_error 且带 code 槽位），不得被 Claude 改造波及。
func TestChatErrorEnvelopeStaysOpenAI(t *testing.T) {
	env, _ := messagesTestEnv(t, contract.ChannelTypeOpenAI, func(http.ResponseWriter, *http.Request) {
		t.Error("upstream must not be called")
	})
	w := env.do(t, http.MethodPost, "/v1/chat/completions", `{invalid json`,
		map[string]string{"Authorization": "Bearer " + relayTestToken})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	envelope := decodeRelayError(t, w.Body.Bytes())
	if envelope.Error.Type != relayErrorType {
		t.Fatalf("type = %q, want %q", envelope.Error.Type, relayErrorType)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	var inner map[string]json.RawMessage
	if err := json.Unmarshal(raw["error"], &inner); err != nil {
		t.Fatalf("decode error object: %v", err)
	}
	if _, ok := inner["code"]; !ok {
		t.Fatal("OpenAI 形必须保留 code 字段（即使空串）")
	}
	if strings.Contains(w.Body.String(), `"type":"error"`) {
		t.Fatalf("chat 不得使用 Claude 顶层形状：%s", w.Body.String())
	}
}

// TestMessagesAnthropicBetaForwarding 覆盖 R-W18-05 的端到端行为：
// 白名单值转发、非白名单值丢弃、客户端 Authorization 永不转发、
// 空/全被丢弃时不发送该头。
func TestMessagesAnthropicBetaForwarding(t *testing.T) {
	cases := []struct {
		name    string
		header  string
		want    string
		wantSet bool
	}{
		{name: "allowlisted", header: "interleaved-thinking-2025-05-14", want: "interleaved-thinking-2025-05-14", wantSet: true},
		{name: "mixed drops unknown", header: "context-1m-2025-08-07, not-a-real-beta", want: "context-1m-2025-08-07", wantSet: true},
		{name: "unknown only", header: "not-a-real-beta", want: "", wantSet: false},
		{name: "empty", header: "", want: "", wantSet: false},
		{name: "dedupes", header: "context-1m-2025-08-07, context-1m-2025-08-07", want: "context-1m-2025-08-07", wantSet: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotBeta, gotAuth string
			var betaPresent bool
			env, _ := messagesTestEnv(t, contract.ChannelTypeAnthropic, func(w http.ResponseWriter, r *http.Request) {
				gotBeta = r.Header.Get("anthropic-beta")
				_, betaPresent = r.Header["Anthropic-Beta"]
				gotAuth = r.Header.Get("Authorization")
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, messagesUpstreamBody)
			})

			headers := map[string]string{"x-api-key": relayTestToken}
			if tc.header != "" {
				headers["anthropic-beta"] = tc.header
			}
			w := env.do(t, http.MethodPost, "/v1/messages",
				`{"model":"claude-test","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, headers)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d; body=%s", w.Code, w.Body.String())
			}
			if tc.wantSet {
				if gotBeta != tc.want {
					t.Fatalf("upstream anthropic-beta = %q, want %q", gotBeta, tc.want)
				}
				if !betaPresent {
					t.Fatal("上游应收到 anthropic-beta 头")
				}
			} else {
				if gotBeta != "" || betaPresent {
					t.Fatalf("上游不应收到 anthropic-beta，got %q", gotBeta)
				}
			}
			// Authorization 与 Cookie 永不转发：适配器只设 x-api-key。
			if gotAuth != "" {
				t.Fatalf("客户端 Authorization 被转发到上游：%q", gotAuth)
			}
		})
	}
}

// TestMessagesAnthropicBetaNotLogged 锁定「beta 值不进日志/审计」的纪律。
//
// 成功路径不记请求头；这里仍用捕获 logger 断言，防止未来有人把
// 「转发头清单」打进 debug 日志。
func TestMessagesAnthropicBetaNotLogged(t *testing.T) {
	var logs bytes.Buffer
	env, _ := messagesTestEnv(t, contract.ChannelTypeAnthropic, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, messagesUpstreamBody)
	})
	env.handler.log = slog.New(slog.NewTextHandler(&logs, nil))

	const betaValue = "interleaved-thinking-2025-05-14"
	w := env.do(t, http.MethodPost, "/v1/messages",
		`{"model":"claude-test","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"x-api-key": relayTestToken, "anthropic-beta": betaValue})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(logs.String(), betaValue) {
		t.Fatalf("anthropic-beta 值进入了日志：%s", logs.String())
	}
}

// TestFilterAnthropicBeta 直接锁住白名单函数（变异点 4：改成全量透传时先红）。
func TestFilterAnthropicBeta(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"   ", ""},
		{"messages-2023-12-15", "messages-2023-12-15"},
		{"messages-2023-12-15,evil", "messages-2023-12-15"},
		{"evil", ""},
		{"messages-2023-12-15, messages-2023-12-15", "messages-2023-12-15"},
		{"MESSAGES-2023-12-15", ""}, // 精确匹配，不做大小写归一化
		{"not-a-beta, context-1m-2025-08-07 ", "context-1m-2025-08-07"},
	}
	for _, tc := range cases {
		if got := adapters.FilterAnthropicBeta(tc.in); got != tc.want {
			t.Fatalf("FilterAnthropicBeta(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ---------- count_tokens ----------

const countTokensResponseBody = `{"input_tokens":17}`

type countTokensCallLimiter struct {
	calls int
}

func (l *countTokensCallLimiter) Allow(_ context.Context, _ ratelimit.Bucket, _ string, _ int) (ratelimit.Decision, error) {
	l.calls++
	return ratelimit.Decision{Allowed: false}, nil
}

// TestCountTokensAnthropicUpstreamZeroBilling 覆盖 R-W18-06 的核心：
// Anthropic 上游精确透传、响应形状 `{"input_tokens":N}`、零计费、零限流桶。
func TestCountTokensAnthropicUpstreamZeroBilling(t *testing.T) {
	var gotPath, gotAPIKey, gotVersion string
	var gotBody map[string]any
	env, counter := messagesTestEnv(t, contract.ChannelTypeAnthropic, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAPIKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, countTokensResponseBody)
	})
	limiter := &countTokensCallLimiter{}
	env.handler.rateLimiter = limiter
	env.handler.config = mapConfig{
		"rl.user_model_rpm":  json.RawMessage("1"),
		"rl.token_rpm":       json.RawMessage("1"),
		"rl.user_global_rpm": json.RawMessage("1"),
	}

	w := env.do(t, http.MethodPost, "/v1/messages/count_tokens",
		`{"model":"claude-test","messages":[{"role":"user","content":"hello"}]}`,
		messagesHeaders())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		InputTokens int64 `json:"input_tokens"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not JSON: %v (%s)", err, w.Body.String())
	}
	if resp.InputTokens != 17 {
		t.Fatalf("input_tokens = %d, want 17", resp.InputTokens)
	}
	if gotPath != "/v1/messages/count_tokens" {
		t.Fatalf("upstream path = %q", gotPath)
	}
	if gotAPIKey != upstreamCredential {
		t.Fatalf("upstream x-api-key = %q", gotAPIKey)
	}
	if gotVersion == "" {
		t.Fatal("upstream anthropic-version 缺失")
	}
	if gotBody["model"] != "claude-test" {
		t.Fatalf("upstream model = %v", gotBody["model"])
	}
	// 零计费：不创建会话、不写 usage_logs（由集成用例断言）、不碰三个限流桶。
	if env.billing.preConsumeCalls() != 0 || env.billing.terminateCount() != 0 || env.billing.refundCount() != 0 {
		t.Fatalf("count_tokens 计费副作用 = pre %d/term %d/refund %d，期望 0/0/0",
			env.billing.preConsumeCalls(), env.billing.terminateCount(), env.billing.refundCount())
	}
	if limiter.calls != 0 {
		t.Fatalf("count_tokens 调用了限流器 %d 次，期望 0（不消耗 B10/B11/B12）", limiter.calls)
	}
	if counter.count() != 1 {
		t.Fatalf("upstream calls = %d, want 1", counter.count())
	}
}

// TestCountTokensNonAnthropicUpstream501 覆盖裁决的方向限制：解析到其它
// 上游时 501 problem+json（count_tokens_unsupported），零计费零上游。
func TestCountTokensNonAnthropicUpstream501(t *testing.T) {
	env, counter := messagesTestEnv(t, contract.ChannelTypeOpenAI, func(http.ResponseWriter, *http.Request) {
		t.Error("upstream must not be called")
	})
	w := env.do(t, http.MethodPost, "/v1/messages/count_tokens",
		`{"model":"claude-test","messages":[{"role":"user","content":"hi"}]}`,
		messagesHeaders())
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501; body=%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/problem+json") {
		t.Fatalf("content-type = %q, want application/problem+json", ct)
	}
	var problem map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &problem); err != nil {
		t.Fatalf("problem body is not JSON: %v", err)
	}
	if problem["type"] != problemTypeCountTokensUnsupported {
		t.Fatalf("problem type = %v, want %s", problem["type"], problemTypeCountTokensUnsupported)
	}
	if !strings.Contains(w.Body.String(), "count_tokens_unsupported") {
		t.Fatalf("problem detail 应包含 count_tokens_unsupported：%s", w.Body.String())
	}
	if counter.count() != 0 || env.billing.preConsumeCalls() != 0 {
		t.Fatalf("unsupported direction must be side-effect free: upstream=%d pre=%d",
			counter.count(), env.billing.preConsumeCalls())
	}
}

// TestCountTokensAuthUsesClaudeEnvelope 确认 count_tokens 的鉴权失败也走
// Claude 形（路径感知写出器覆盖 /v1/messages/*）。
func TestCountTokensAuthUsesClaudeEnvelope(t *testing.T) {
	env, _ := messagesTestEnv(t, contract.ChannelTypeAnthropic, func(http.ResponseWriter, *http.Request) {
		t.Error("upstream must not be called")
	})
	w := env.do(t, http.MethodPost, "/v1/messages/count_tokens",
		`{"model":"claude-test","messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"x-api-key": "nkt-invalid"})
	assertClaudeError(t, w, http.StatusUnauthorized, "authentication_error")
}

// TestCountTokensModelMapping 证明上游模型映射在 count_tokens 上同样生效
// （body 的 model 被改写，其余字段原样保留）。
func TestCountTokensModelMapping(t *testing.T) {
	var gotBody map[string]any
	env, _ := messagesTestEnv(t, contract.ChannelTypeAnthropic, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, countTokensResponseBody)
	})
	env.channels.mu.Lock()
	channel := env.channels.channels[relayTestChannelID]
	channel.ModelMapping = []byte(`{"claude-test":"claude-upstream"}`)
	env.channels.mu.Unlock()

	w := env.do(t, http.MethodPost, "/v1/messages/count_tokens",
		`{"model":"claude-test","system":"keep me","messages":[{"role":"user","content":"hi"}]}`,
		messagesHeaders())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", w.Code, w.Body.String())
	}
	if gotBody["model"] != "claude-upstream" {
		t.Fatalf("upstream model = %v, want claude-upstream", gotBody["model"])
	}
	if gotBody["system"] != "keep me" {
		t.Fatalf("未建模字段被丢失：system=%v", gotBody["system"])
	}
}
