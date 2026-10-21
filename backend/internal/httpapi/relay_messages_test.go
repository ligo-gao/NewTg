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
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"newtg/gateway/internal/contract"
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
	if counter.count() != 0 || env.billing.preConsumeCalls() != 0 {
		t.Fatalf("unsupported direction must not bill or call upstream: calls=%d preconsume=%d",
			counter.count(), env.billing.preConsumeCalls())
	}
}
