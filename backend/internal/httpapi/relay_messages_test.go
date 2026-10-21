package httpapi

// W7/M1：POST /v1/messages（Anthropic 原生入口）的方向矩阵测试。
//
// 纪律（R-P10）：不连真 PG/Redis；令牌/用户/渠道用本包既有的内存 fake，
// 上游用 httptest + 真实 adapters.Anthropic，覆盖「鉴权 → IR → 路由 → 执行 →
// 原生回写」的完整装配面。
//
// R-59 的两条判定必须落在用例上：
//   - 可用方向（Anthropic 入站 → Anthropic 协议族上游）：请求体的
//     system/messages/max_tokens/stop_sequences 真的送到了上游，回写是
//     Anthropic 形态（非流式 JSON / 原生 SSE），且计费只发生一次；
//   - 不可用方向（Anthropic 入站 → OpenAI 上游）：501 problem+json +
//     零计费 + 零上游请求（transport.count()==0）。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/relay/adapters"
	"newtg/gateway/internal/store"
)

const (
	messagesTestModel     = "claude-test"
	messagesUpstreamBody  = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-test","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`
	messagesUpstreamEvent = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","model":"claude-test","usage":{"input_tokens":5,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}

event: message_stop
data: {"type":"message_stop"}

`
)

// messagesTestEnv 装配一个只含给定渠道类型的候选集，并记录上游收到的请求。
func messagesTestEnv(t *testing.T, channelType contract.ChannelType, upstreamHandler http.HandlerFunc) (*relayTestEnv, *countingTransport) {
	t.Helper()
	upstream := httptest.NewServer(upstreamHandler)
	t.Cleanup(upstream.Close)

	sealer := NewSecretSealer(relaySealerMaster)
	baseURL := upstream.URL
	channels := &fakeRelayChannelStore{
		candidates: []contract.ChannelCandidate{{
			ChannelID: int(relayTestChannelID),
			Priority:  10,
			Weight:    1,
		}},
		channels: map[int64]*store.Channel{
			relayTestChannelID: {
				ID:              relayTestChannelID,
				Name:            "test-" + string(channelType),
				Type:            string(channelType),
				BaseURL:         &baseURL,
				Credentials:     sealedTestCredentials(t, sealer),
				SupportedModels: []byte(`["claude-test"]`),
				ModelMapping:    []byte(`{}`),
				Status:          1,
				CreatedAt:       time.Now().UTC(),
			},
		},
		models: []string{messagesTestModel},
	}

	env := newRelayTestEnv(t, upstream, channels)
	base := upstream.Client().Transport
	if base == nil {
		base = http.DefaultTransport
	}
	counter := &countingTransport{base: base}
	env.handler.client = &http.Client{Transport: counter}
	return env, counter
}

func messagesHeaders() map[string]string {
	// x-api-key 是 Claude 客户端的规范载体（relay-01 §3.1）：鉴权中间件
	// 按路径归一化后再交给 BuildRelayContext 重解析。
	return map[string]string{"x-api-key": relayTestToken}
}

// TestMessagesAnthropicUpstreamRoundTrip 证明可用方向真的完成了转码：
// 入站 Anthropic 字段全部到达上游，回写是 Anthropic 形态，计费恰好一次。
func TestMessagesAnthropicUpstreamRoundTrip(t *testing.T) {
	var (
		gotPath    string
		gotBody    map[string]any
		gotAPIKey  string
		gotVersion string
	)
	env, counter := messagesTestEnv(t, contract.ChannelTypeAnthropic, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAPIKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, messagesUpstreamBody)
	})

	body := `{"model":"claude-test","max_tokens":128,"system":"be brief",` +
		`"messages":[{"role":"user","content":"hello"}],"stop_sequences":["\n\n"],"stream":false}`
	w := env.do(t, http.MethodPost, "/v1/messages", body, messagesHeaders())

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Fatalf("content-type = %q, want application/json", got)
	}
	// 回写必须是上游的 Anthropic 原生体（而不是 chat.completion）。
	var respObj map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &respObj); err != nil {
		t.Fatalf("response is not JSON: %v; body=%s", err, w.Body.String())
	}
	if respObj["type"] != "message" || respObj["id"] != "msg_1" {
		t.Fatalf("response = %s, want Anthropic message shape", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"text":"hi"`) {
		t.Fatalf("response = %s, want Anthropic content block", w.Body.String())
	}

	// 上游请求：路径、凭据载体与协议版本头。
	if gotPath != "/v1/messages" {
		t.Fatalf("upstream path = %q, want /v1/messages", gotPath)
	}
	if gotAPIKey != upstreamCredential {
		t.Fatalf("upstream x-api-key = %q, want the sealed channel credential", gotAPIKey)
	}
	if gotVersion == "" {
		t.Fatal("upstream anthropic-version header is missing")
	}
	// 入站字段的语义保真：system/messages/max_tokens/stop_sequences。
	if gotBody["model"] != messagesTestModel {
		t.Fatalf("upstream model = %v, want %s", gotBody["model"], messagesTestModel)
	}
	if gotBody["max_tokens"] != float64(128) {
		t.Fatalf("upstream max_tokens = %v, want 128", gotBody["max_tokens"])
	}
	if gotBody["system"] != "be brief" {
		t.Fatalf("upstream system = %v, want \"be brief\"", gotBody["system"])
	}
	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("upstream messages = %v, want exactly one user message", gotBody["messages"])
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "user" {
		t.Fatalf("upstream messages[0].role = %v, want user", first["role"])
	}
	stops, _ := gotBody["stop_sequences"].([]any)
	if len(stops) != 1 || stops[0] != "\n\n" {
		t.Fatalf("upstream stop_sequences = %v, want [\"\\n\\n\"]", gotBody["stop_sequences"])
	}

	// 计费：与 chat 同一条接缝，恰好一次预扣/结算，不退款。
	if got := env.billing.preConsumeCalls(); got != 1 {
		t.Fatalf("PreConsume calls = %d, want 1", got)
	}
	if est, ok := env.billing.lastEstimate(); !ok || est.MaxTokens == nil || *est.MaxTokens != 128 {
		t.Fatalf("QuotaEstimate = %+v, want MaxTokens=128（免费模型 hold 依赖它）", est)
	}
	if got := env.billing.terminateCount(); got != 1 {
		t.Fatalf("Terminate calls = %d, want 1", got)
	}
	if got := env.billing.refundCount(); got != 0 {
		t.Fatalf("Refund calls = %d, want 0", got)
	}
	if fact := env.billing.lastFact(); fact.Usage.Prompt != 3 || fact.Usage.Completion != 2 {
		t.Fatalf("usage fact = %+v, want prompt 3 / completion 2", fact.Usage)
	}
	if counter.count() != 1 {
		t.Fatalf("upstream calls = %d, want 1", counter.count())
	}
}

// TestMessagesStreamNativeSSE 证明流式方向逐字节保留原生事件：text 的
// content_block_start/stop 也必须到达客户端（事件级重编码会丢掉它们），
// 且不注入 OpenAI 的 [DONE] / chunk 形状。
func TestMessagesStreamNativeSSE(t *testing.T) {
	var gotStream bool
	env, _ := messagesTestEnv(t, contract.ChannelTypeAnthropic, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(raw, &payload)
		gotStream, _ = payload["stream"].(bool)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, messagesUpstreamEvent)
	})

	body := `{"model":"claude-test","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"stream":true}`
	w := env.do(t, http.MethodPost, "/v1/messages", body, messagesHeaders())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if !gotStream {
		t.Fatal("upstream request lost the stream flag")
	}
	if got := w.Header().Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Fatalf("content-type = %q, want text/event-stream", got)
	}
	out := w.Body.String()
	for _, want := range []string{"message_start", "content_block_start", "content_block_stop", "message_stop", `"text":"Hello"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("SSE body missing %q; body=%s", want, out)
		}
	}
	if strings.Contains(out, "[DONE]") || strings.Contains(out, "chat.completion.chunk") {
		t.Fatalf("SSE body contains OpenAI framing: %s", out)
	}
	if strings.TrimSpace(out) != strings.TrimSpace(messagesUpstreamEvent) {
		t.Fatalf("SSE body was rewritten:\n got=%q\nwant=%q", out, messagesUpstreamEvent)
	}

	if got := env.billing.terminateCount(); got != 1 {
		t.Fatalf("Terminate calls = %d, want 1", got)
	}
	if got := env.billing.refundCount(); got != 0 {
		t.Fatalf("Refund calls = %d, want 0", got)
	}
	fact := env.billing.lastFact()
	if fact.Usage.Prompt != 5 || fact.Usage.Completion != 7 {
		t.Fatalf("stream usage fact = %+v, want prompt 5 / completion 7", fact.Usage)
	}
	if !fact.Stream {
		t.Fatal("usage fact must be marked Stream")
	}
}

// TestMessagesOpenAIUpstreamFailsClosed 证明不可用方向 fail-closed：
// 501 problem+json，零计费，零上游请求（R-59 / R-13 同口径）。
func TestMessagesOpenAIUpstreamFailsClosed(t *testing.T) {
	env, counter := messagesTestEnv(t, contract.ChannelTypeOpenAI, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("upstream must not be called for an unsupported direction: %s", r.URL.Path)
	})

	body := `{"model":"claude-test","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"stream":false}`
	w := env.do(t, http.MethodPost, "/v1/messages", body, messagesHeaders())

	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501; body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); !strings.Contains(got, "application/problem+json") {
		t.Fatalf("content-type = %q, want application/problem+json", got)
	}
	var problem map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &problem); err != nil {
		t.Fatalf("501 body is not JSON: %v", err)
	}
	if problem["type"] != problemTypeEndpointNotSupported || problem["status"] != float64(501) {
		t.Fatalf("problem = %v, want type=%s status=501", problem, problemTypeEndpointNotSupported)
	}
	if got := counter.count(); got != 0 {
		t.Fatalf("upstream calls = %d, want 0", got)
	}
	if got := env.billing.preConsumeCalls(); got != 0 {
		t.Fatalf("PreConsume calls = %d, want 0", got)
	}
	if got := env.billing.terminateCount(); got != 0 {
		t.Fatalf("Terminate calls = %d, want 0", got)
	}
	if got := env.billing.refundCount(); got != 0 {
		t.Fatalf("Refund calls = %d, want 0（方向不支持不得开设计费会话）", got)
	}
}

// TestMessagesGeminiUpstreamFailsClosed：Gemini 原生渠道同样不可转码（响应
// 方向缺 IR → Anthropic 编码器），必须走同一条 501 分支。
func TestMessagesGeminiUpstreamFailsClosed(t *testing.T) {
	env, counter := messagesTestEnv(t, contract.ChannelTypeGemini, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("upstream must not be called: %s", r.URL.Path)
	})
	w := env.do(t, http.MethodPost, "/v1/messages",
		`{"model":"claude-test","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"stream":true}`,
		messagesHeaders())
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501; body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); !strings.Contains(got, "application/problem+json") {
		t.Fatalf("content-type = %q, want application/problem+json", got)
	}
	var problem map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &problem); err != nil {
		t.Fatalf("501 body is not JSON: %v", err)
	}
	if problem["type"] != problemTypeEndpointNotSupported {
		t.Fatalf("problem type = %v, want %s", problem["type"], problemTypeEndpointNotSupported)
	}
	if counter.count() != 0 || env.billing.preConsumeCalls() != 0 ||
		env.billing.terminateCount() != 0 || env.billing.refundCount() != 0 {
		t.Fatalf("unsupported direction must be side-effect free: calls=%d pre=%d term=%d refund=%d",
			counter.count(), env.billing.preConsumeCalls(), env.billing.terminateCount(), env.billing.refundCount())
	}
}

// ---------- 方向矩阵补齐：Moonshot 可用 / 任务平台不可用 ----------

// TestMessagesMoonshotUpstreamRoundTrip：type 25 是 Anthropic 兼容协议，
// 方向矩阵声称可用就必须有端点级证据：请求打到 /anthropic/v1/messages，
// 携带解封凭据，计费 1/1/0。
func TestMessagesMoonshotUpstreamRoundTrip(t *testing.T) {
	var gotPath, gotAPIKey string
	env, counter := messagesTestEnv(t, contract.ChannelTypeMoonshot, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAPIKey = r.Header.Get("x-api-key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, messagesUpstreamBody)
	})

	w := env.do(t, http.MethodPost, "/v1/messages",
		`{"model":"claude-test","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`,
		messagesHeaders())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if gotPath != "/anthropic/v1/messages" {
		t.Fatalf("upstream path = %q, want /anthropic/v1/messages", gotPath)
	}
	if gotAPIKey != upstreamCredential {
		t.Fatalf("upstream x-api-key = %q, want the sealed channel credential", gotAPIKey)
	}
	if counter.count() != 1 {
		t.Fatalf("upstream calls = %d, want 1", counter.count())
	}
	if env.billing.preConsumeCalls() != 1 || env.billing.terminateCount() != 1 || env.billing.refundCount() != 0 {
		t.Fatalf("billing = pre %d/term %d/refund %d, want 1/1/0",
			env.billing.preConsumeCalls(), env.billing.terminateCount(), env.billing.refundCount())
	}
}

// TestMessagesTaskPluginUpstreamFailsClosed：任务插件类型不属于
// openai/anthropic/gemini 任一协议族（ProtocolOfChannelType ok=false），
// 必须与其它异构上游走同一条 501。
func TestMessagesTaskPluginUpstreamFailsClosed(t *testing.T) {
	env, counter := messagesTestEnv(t, contract.ChannelTypeTaskPlugin, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("upstream must not be called: %s", r.URL.Path)
	})
	w := env.do(t, http.MethodPost, "/v1/messages",
		`{"model":"claude-test","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
		messagesHeaders())
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501; body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); !strings.Contains(got, "application/problem+json") {
		t.Fatalf("content-type = %q, want application/problem+json", got)
	}
	if counter.count() != 0 || env.billing.preConsumeCalls() != 0 ||
		env.billing.terminateCount() != 0 || env.billing.refundCount() != 0 {
		t.Fatalf("unsupported direction must be side-effect free: calls=%d pre=%d term=%d refund=%d",
			counter.count(), env.billing.preConsumeCalls(), env.billing.terminateCount(), env.billing.refundCount())
	}
}

// TestMessagesDirectionSupported 直接锁住方向判定函数（复核 M-1 的变异 A）：
// 判定实现被改成「恒 true / 恒 false」或 Get 失败语义漂移，这里先红，
// 不再只靠端点用例兜底。
func TestMessagesDirectionSupported(t *testing.T) {
	anthropicCh := &store.Channel{ID: 1, Type: string(contract.ChannelTypeAnthropic)}
	openaiCh := &store.Channel{ID: 2, Type: string(contract.ChannelTypeOpenAI)}
	moonshotCh := &store.Channel{ID: 3, Type: string(contract.ChannelTypeMoonshot)}
	geminiCh := &store.Channel{ID: 4, Type: string(contract.ChannelTypeGemini)}
	cases := []struct {
		name       string
		candidates []contract.ChannelCandidate
		channels   map[int64]*store.Channel
		want       bool
	}{
		{name: "anthropic", candidates: []contract.ChannelCandidate{{ChannelID: 1}},
			channels: map[int64]*store.Channel{1: anthropicCh}, want: true},
		{name: "moonshot", candidates: []contract.ChannelCandidate{{ChannelID: 3}},
			channels: map[int64]*store.Channel{3: moonshotCh}, want: true},
		{name: "openai", candidates: []contract.ChannelCandidate{{ChannelID: 2}},
			channels: map[int64]*store.Channel{2: openaiCh}, want: false},
		{name: "gemini", candidates: []contract.ChannelCandidate{{ChannelID: 4}},
			channels: map[int64]*store.Channel{4: geminiCh}, want: false},
		{name: "empty", candidates: nil, channels: map[int64]*store.Channel{}, want: false},
		// Get 失败的候选按不可用跳过：后面的 Anthropic 候选仍能判定为可用。
		{name: "get error then anthropic",
			candidates: []contract.ChannelCandidate{{ChannelID: 9}, {ChannelID: 1}},
			channels:   map[int64]*store.Channel{1: anthropicCh}, want: true},
		// 全部 Get 失败 → 没有可用 Anthropic 渠道。
		{name: "all get errors", candidates: []contract.ChannelCandidate{{ChannelID: 9}},
			channels: map[int64]*store.Channel{}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			channels := &fakeRelayChannelStore{
				candidates: tc.candidates,
				channels:   tc.channels,
			}
			h := &RelayHandler{channels: channels}
			if got := h.messagesDirectionSupported(context.Background(), tc.candidates); got != tc.want {
				t.Fatalf("messagesDirectionSupported = %v, want %v", got, tc.want)
			}
		})
	}
}

// ---------- 入口校验：max_tokens 必填（I-1） ----------

// TestMessagesMaxTokensRequired：Anthropic Messages 的 max_tokens 必填且 >0。
// 缺失/0/负数都必须在 PreConsume 与上游调用之前 400，否则免费模型的防滥用
// hold 会因 MaxTokens=nil 退化为 0（F-08-12）。
func TestMessagesMaxTokensRequired(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "missing", body: `{"model":"claude-test","messages":[{"role":"user","content":"hi"}]}`},
		{name: "zero", body: `{"model":"claude-test","max_tokens":0,"messages":[{"role":"user","content":"hi"}]}`},
		{name: "negative", body: `{"model":"claude-test","max_tokens":-1,"messages":[{"role":"user","content":"hi"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, counter := messagesTestEnv(t, contract.ChannelTypeAnthropic, func(w http.ResponseWriter, r *http.Request) {
				t.Fatalf("upstream must not be called: %s", r.URL.Path)
			})
			w := env.do(t, http.MethodPost, "/v1/messages", tc.body, messagesHeaders())
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
			}
			if counter.count() != 0 || env.billing.preConsumeCalls() != 0 ||
				env.billing.terminateCount() != 0 || env.billing.refundCount() != 0 {
				t.Fatalf("invalid max_tokens must be side-effect free: calls=%d pre=%d term=%d refund=%d",
					counter.count(), env.billing.preConsumeCalls(), env.billing.terminateCount(), env.billing.refundCount())
			}
		})
	}
}

// ---------- 入口校验：白名单 / 停用模型 / 体积上限 / pin ----------

func TestMessagesTokenModelWhitelistForbidden(t *testing.T) {
	env, counter := messagesTestEnv(t, contract.ChannelTypeAnthropic, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("upstream must not be called: %s", r.URL.Path)
	})
	meta := env.tokens.byHash[auth.HashTokenKey(relayTestToken)]
	meta.ModelLimits = []string{"some-other-model"}

	w := env.do(t, http.MethodPost, "/v1/messages",
		`{"model":"claude-test","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
		messagesHeaders())
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", w.Code, w.Body.String())
	}
	if counter.count() != 0 || env.billing.preConsumeCalls() != 0 ||
		env.billing.terminateCount() != 0 || env.billing.refundCount() != 0 {
		t.Fatalf("whitelist rejection must be side-effect free: calls=%d pre=%d term=%d refund=%d",
			counter.count(), env.billing.preConsumeCalls(), env.billing.terminateCount(), env.billing.refundCount())
	}
}

func TestMessagesDisabledModelReturns404(t *testing.T) {
	env, counter := messagesTestEnv(t, contract.ChannelTypeAnthropic, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("upstream must not be called: %s", r.URL.Path)
	})
	env.handler.modelStatus = &fakeRelayModelStatus{
		status: map[string]int16{messagesTestModel: modelPricingStatusDisabled},
	}

	w := env.do(t, http.MethodPost, "/v1/messages",
		`{"model":"claude-test","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
		messagesHeaders())
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
	if counter.count() != 0 || env.billing.preConsumeCalls() != 0 ||
		env.billing.terminateCount() != 0 || env.billing.refundCount() != 0 {
		t.Fatalf("disabled model must be side-effect free: calls=%d pre=%d term=%d refund=%d",
			counter.count(), env.billing.preConsumeCalls(), env.billing.terminateCount(), env.billing.refundCount())
	}
}

func TestMessagesBodyTooLargeReturns413(t *testing.T) {
	env, counter := messagesTestEnv(t, contract.ChannelTypeAnthropic, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("upstream must not be called: %s", r.URL.Path)
	})
	body := `{"model":"claude-test","max_tokens":16,"messages":[{"role":"user","content":"` +
		strings.Repeat("a", maxRelayBodyBytes+1) + `"}]}`

	w := env.do(t, http.MethodPost, "/v1/messages", body, messagesHeaders())
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body=%s", w.Code, w.Body.String())
	}
	if counter.count() != 0 || env.billing.preConsumeCalls() != 0 ||
		env.billing.terminateCount() != 0 || env.billing.refundCount() != 0 {
		t.Fatalf("oversized body must be side-effect free: calls=%d pre=%d term=%d refund=%d",
			counter.count(), env.billing.preConsumeCalls(), env.billing.terminateCount(), env.billing.refundCount())
	}
}

// TestMessagesNonAdminPinForbidden：legacy sk-<key>-<channelId> 的 pin 是管理员
// 运维能力；普通用户使用必须在 BuildRelayContext 里 403（与 chat 同口径），
// 不得进入计费或上游。
func TestMessagesNonAdminPinForbidden(t *testing.T) {
	env, counter := messagesTestEnv(t, contract.ChannelTypeAnthropic, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("upstream must not be called: %s", r.URL.Path)
	})
	env.tokens = newFakeRelayTokenStore("sk-pintest", &auth.TokenMeta{
		ID:             relayTestTokenID,
		UserID:         relayTestUserID,
		Status:         "enabled",
		UnlimitedQuota: true,
	})
	// AuthMiddleware 在 h.Handler() 时读取 h.tokens，必须同时替换 handler 的引用。
	env.handler.tokens = env.tokens
	env.access.role = "user"

	w := env.do(t, http.MethodPost, "/v1/messages",
		`{"model":"claude-test","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"x-api-key": "sk-pintest-1"})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", w.Code, w.Body.String())
	}
	if counter.count() != 0 || env.billing.preConsumeCalls() != 0 ||
		env.billing.terminateCount() != 0 || env.billing.refundCount() != 0 {
		t.Fatalf("pin rejection must be side-effect free: calls=%d pre=%d term=%d refund=%d",
			counter.count(), env.billing.preConsumeCalls(), env.billing.terminateCount(), env.billing.refundCount())
	}
}

// ---------- 原生流：帧计数（M-1 变异 E 的守卫） ----------

func messagesStreamEnv(t *testing.T, channelType contract.ChannelType, upstreamHandler http.HandlerFunc) (*relayTestEnv, *observingStreamAdapter) {
	t.Helper()
	env, _ := messagesTestEnv(t, channelType, upstreamHandler)
	inner, ok := adapters.NewAnthropicAdapter(channelType).(contract.StreamAdapter)
	if !ok {
		t.Fatal("anthropic adapter must implement StreamAdapter")
	}
	observing := &observingStreamAdapter{inner: inner}
	env.handler.dispatch = func(contract.ChannelType) (contract.Adapter, error) { return observing, nil }
	return env, observing
}

// TestMessagesStreamFrameCount 锁住 SendCount 的口径：SSE 帧数，而不是
// io.Read 的块数。LF 与 CRLF 两种行尾都必须各计 3 帧。
func TestMessagesStreamFrameCount(t *testing.T) {
	frames := []string{
		"event: ping\ndata: {\"type\":\"ping\"}\n\n",
		"event: ping\r\ndata: {\"type\":\"ping\"}\r\n\r\n",
	}
	for _, frame := range frames {
		frame := frame
		lineEnding := "LF"
		if strings.Contains(frame, "\r\n") {
			lineEnding = "CRLF"
		}
		t.Run(lineEnding, func(t *testing.T) {
			env, observing := messagesStreamEnv(t, contract.ChannelTypeAnthropic, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				// 一次写出三帧：变异实现（按 Read 块计数）只会得到 1。
				_, _ = io.WriteString(w, frame+frame+frame)
			})

			w := env.do(t, http.MethodPost, "/v1/messages",
				`{"model":"claude-test","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"stream":true}`,
				messagesHeaders())
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
			}
			if observing.rc == nil || observing.rc.StreamStatus == nil {
				t.Fatal("StreamStatus was not recorded")
			}
			if got := observing.rc.StreamStatus.SendCount; got != 3 {
				t.Fatalf("SendCount = %d, want 3 SSE frames; body=%q", got, w.Body.String())
			}
		})
	}
}

// ---------- 原生流：异常出口（I-2，变异 F 的守卫） ----------

// TestMessagesStreamClientAbortRefunds：客户端断开 → 恰好一次 Refund、
// 零 Terminate、EndReason=client_abort，且不写伪造的收尾帧。
func TestMessagesStreamClientAbortRefunds(t *testing.T) {
	env, observing := messagesStreamEnv(t, contract.ChannelTypeAnthropic, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "event: ping\ndata: {\"type\":\"ping\"}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		// 保持流打开，直到网关关闭上游 body 级联取消请求 context。
		<-r.Context().Done()
	})
	gateway := httptest.NewServer(env.handler.Handler())
	defer gateway.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gateway.URL+"/v1/messages",
		strings.NewReader(`{"model":"claude-test","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"stream":true}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("x-api-key", relayTestToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	buf := make([]byte, 256)
	n, readErr := resp.Body.Read(buf)
	if readErr != nil && n == 0 {
		t.Fatalf("read first frame: %v", readErr)
	}
	first := string(buf[:n])
	cancel()
	// cancel 之后把剩余可观察到的响应体也读完：断言「完整响应体」里没有
	// 伪造的收尾帧，而不是只看首帧。
	rest, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	body := first + string(rest)

	deadline := time.Now().Add(5 * time.Second)
	for env.billing.refundCount() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("billing session was not refunded after client disconnect")
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if got := env.billing.refundCount(); got != 1 {
		t.Fatalf("Refund calls = %d, want exactly 1", got)
	}
	if got := env.billing.terminateCount(); got != 0 {
		t.Fatalf("Terminate calls = %d, want 0（失败出口必须退款）", got)
	}
	if observing.rc == nil || observing.rc.StreamStatus == nil {
		t.Fatal("StreamStatus was not recorded")
	}
	if got := observing.rc.StreamStatus.EndReason; got != contract.EndClientAbort {
		t.Fatalf("EndReason = %d, want EndClientAbort", got)
	}
	if strings.Contains(body, "message_stop") {
		t.Fatalf("gateway forged a message_stop on abort: %q", body)
	}
}

// TestMessagesStreamWatchdogTimeoutRefunds：块间静默超过看门狗阈值 →
// 恰好一次 Refund、零 Terminate、EndReason=stream_timeout。
func TestMessagesStreamWatchdogTimeoutRefunds(t *testing.T) {
	env, observing := messagesStreamEnv(t, contract.ChannelTypeAnthropic, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "event: ping\ndata: {\"type\":\"ping\"}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		<-r.Context().Done()
	})
	env.handler.streamInactivity = 100 * time.Millisecond

	start := time.Now()
	w := env.do(t, http.MethodPost, "/v1/messages",
		`{"model":"claude-test","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"stream":true}`,
		messagesHeaders())
	elapsed := time.Since(start)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200（响应头已发送）; body=%s", w.Code, w.Body.String())
	}
	if elapsed < 100*time.Millisecond {
		t.Fatalf("handler returned after %v，早于看门狗阈值", elapsed)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("watchdog did not fire in time: %v", elapsed)
	}
	if strings.Contains(w.Body.String(), "message_stop") {
		t.Fatalf("超时是异常结束，不得伪造 message_stop; body=%s", w.Body.String())
	}
	if got := env.billing.refundCount(); got != 1 {
		t.Fatalf("Refund calls = %d, want exactly 1", got)
	}
	if got := env.billing.terminateCount(); got != 0 {
		t.Fatalf("Terminate calls = %d, want 0（超时必须退款）", got)
	}
	if observing.rc == nil || observing.rc.StreamStatus == nil {
		t.Fatal("StreamStatus was not recorded")
	}
	if got := observing.rc.StreamStatus.EndReason; got != contract.EndStreamTimeout {
		t.Fatalf("EndReason = %d, want EndStreamTimeout", got)
	}
}

// TestMessagesStreamUpstreamReadErrorRefunds：上游发送若干字节后异常断开 →
// 读取失败是硬错误，恰好一次 Refund、EndReason=upstream_error，不伪造收尾帧。
func TestMessagesStreamUpstreamReadErrorRefunds(t *testing.T) {
	env, observing := messagesStreamEnv(t, contract.ChannelTypeAnthropic, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "event: ping\ndata: {\"type\":\"ping\"}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		// 中断连接：客户端读 body 时得到 unexpected EOF。
		panic(http.ErrAbortHandler)
	})

	w := env.do(t, http.MethodPost, "/v1/messages",
		`{"model":"claude-test","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"stream":true}`,
		messagesHeaders())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200（响应头已发送）; body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "message_stop") {
		t.Fatalf("读错误是异常结束，不得伪造 message_stop; body=%s", w.Body.String())
	}
	if got := env.billing.refundCount(); got != 1 {
		t.Fatalf("Refund calls = %d, want exactly 1", got)
	}
	if got := env.billing.terminateCount(); got != 0 {
		t.Fatalf("Terminate calls = %d, want 0（读错误必须退款）", got)
	}
	if observing.rc == nil || observing.rc.StreamStatus == nil {
		t.Fatal("StreamStatus was not recorded")
	}
	if got := observing.rc.StreamStatus.EndReason; got != contract.EndUpstreamError {
		t.Fatalf("EndReason = %d, want EndUpstreamError", got)
	}
}

// TestMessagesStreamPanicRefunds：StreamEvents 内 panic → 进程不崩、
// 恰好一次 Refund、EndReason=panic，且不写伪造收尾帧。
func TestMessagesStreamPanicRefunds(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	sealer := NewSecretSealer(relaySealerMaster)
	baseURL := upstream.URL
	channels := &fakeRelayChannelStore{
		candidates: []contract.ChannelCandidate{{ChannelID: int(relayTestChannelID), Priority: 10, Weight: 1}},
		channels: map[int64]*store.Channel{
			relayTestChannelID: {
				ID:              relayTestChannelID,
				Type:            string(contract.ChannelTypeAnthropic),
				BaseURL:         &baseURL,
				Credentials:     sealedTestCredentials(t, sealer),
				SupportedModels: []byte(`["claude-test"]`),
				ModelMapping:    []byte(`{}`),
				Status:          1,
				CreatedAt:       time.Now().UTC(),
			},
		},
		models: []string{messagesTestModel},
	}
	env := newRelayTestEnv(t, upstream, channels)
	adapter := &scriptedStreamAdapter{
		url:        upstream.URL + "/v1/messages",
		events:     []contract.StreamEvent{{Kind: contract.EventDelta, Delta: "before-panic"}},
		panicAfter: 1,
	}
	env.handler.dispatch = func(contract.ChannelType) (contract.Adapter, error) { return adapter, nil }

	w := env.do(t, http.MethodPost, "/v1/messages",
		`{"model":"claude-test","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"stream":true}`,
		messagesHeaders())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200（响应头已发送）; body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "message_stop") {
		t.Fatalf("panic 是异常结束，不得伪造 message_stop; body=%s", w.Body.String())
	}
	if got := env.billing.refundCount(); got != 1 {
		t.Fatalf("Refund calls = %d, want exactly 1", got)
	}
	if got := env.billing.terminateCount(); got != 0 {
		t.Fatalf("Terminate calls = %d, want 0（panic 必须退款）", got)
	}
	if adapter.rc == nil || adapter.rc.StreamStatus == nil {
		t.Fatal("StreamStatus was not recorded after panic")
	}
	if got := adapter.rc.StreamStatus.EndReason; got != contract.EndPanic {
		t.Fatalf("EndReason = %d, want EndPanic", got)
	}
}
