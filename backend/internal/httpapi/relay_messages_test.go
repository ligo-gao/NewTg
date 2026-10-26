package httpapi

// W7/M1：POST /v1/messages（Anthropic 原生入口）的方向矩阵测试。
//
// 纪律（R-P10）：不连真 PG/Redis；令牌/用户/渠道用本包既有的内存 fake，
// 上游用 httptest + 真实 adapters.Anthropic，覆盖「鉴权 → IR → 路由 → 执行 →
// 原生回写」的完整装配面。
//
// 方向矩阵（W18-B · R-W18-03 放开异构上游后）：
//   - Anthropic 入站 → Anthropic 协议族上游：请求经 IR 重建、响应/SSE
//     逐字节透传，计费只发生一次；
//   - Anthropic 入站 → OpenAI / Gemini 上游：请求经 IR 编码，响应经
//     ir.BuildAnthropicResponse / AnthropicStreamRenderer 回写为 Claude 形状，
//     计费同样只发生一次（usage 与回写映射一致）；
//   - 三类协议之外的渠道（任务插件）：501 problem+json + 零计费 + 零上游。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/billing"
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/relay/adapters"
	"newtg/gateway/internal/relay/ir"
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

// parseSSEEvents 把 Anthropic SSE 响应体拆成事件名与 data JSON（按帧序）。
// 只服务测试：Anthropic 帧以空行分隔，每个帧最多一个 event: 与 data: 行。
func parseSSEEvents(t *testing.T, body string) ([]string, []map[string]any) {
	t.Helper()
	var (
		events []string
		datas  []map[string]any
	)
	for _, block := range strings.Split(strings.TrimSpace(body), "\n\n") {
		if strings.TrimSpace(block) == "" {
			continue
		}
		var (
			event string
			data  string
		)
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event:"):
				event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			}
		}
		if data == "" {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(data), &obj); err != nil {
			t.Fatalf("SSE data 不是 JSON: %v; frame=%q", err, block)
		}
		events = append(events, event)
		datas = append(datas, obj)
	}
	return events, datas
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

// TestMessagesOpenAIUpstreamJSONRoundTrip 证明新方向真的完成了响应转码
// （W18-B · R-W18-03）：OpenAI chat.completion → Claude message，usage 按
// 关系布尔换算（OpenAI prompt 含缓存，Anthropic input 不含），计费恰好一次。
func TestMessagesOpenAIUpstreamJSONRoundTrip(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	env, counter := messagesTestEnv(t, contract.ChannelTypeOpenAI, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-9","object":"chat.completion","model":"gpt-upstream",`+
			`"choices":[{"index":0,"message":{"role":"assistant","content":"hello from openai"},`+
			`"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,`+
			`"prompt_tokens_details":{"cached_tokens":4}}}`)
	})

	w := env.do(t, http.MethodPost, "/v1/messages",
		`{"model":"claude-test","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"stream":false}`,
		messagesHeaders())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Fatalf("content-type = %q, want application/json", got)
	}
	var respObj map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &respObj); err != nil {
		t.Fatalf("response is not JSON: %v; body=%s", err, w.Body.String())
	}
	if respObj["type"] != "message" || respObj["role"] != "assistant" || respObj["id"] != "chatcmpl-9" {
		t.Fatalf("response = %s, want Anthropic message shape", w.Body.String())
	}
	if respObj["stop_reason"] != "end_turn" {
		t.Fatalf("stop_reason = %v, want end_turn", respObj["stop_reason"])
	}
	blocks := respObj["content"].([]any)
	if len(blocks) != 1 || blocks[0].(map[string]any)["text"] != "hello from openai" {
		t.Fatalf("content = %v, want one text block", blocks)
	}
	usage := respObj["usage"].(map[string]any)
	// OpenAI prompt_tokens(10) 含 cached(4) → Anthropic input_tokens = 6。
	if usage["input_tokens"] != float64(6) || usage["output_tokens"] != float64(5) {
		t.Fatalf("usage = %v, want input 6 / output 5", usage)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("upstream path = %q, want /v1/chat/completions", gotPath)
	}
	// 请求方向仍经 IR 编码：Anthropic 的 messages/system/max_tokens 送到 OpenAI 形状。
	if gotBody["max_tokens"] != float64(16) {
		t.Fatalf("upstream max_tokens = %v, want 16", gotBody["max_tokens"])
	}
	// 计费：与 chat 同一条接缝，恰好一次；usage 与回写映射同源。
	if env.billing.preConsumeCalls() != 1 || env.billing.terminateCount() != 1 || env.billing.refundCount() != 0 {
		t.Fatalf("billing = pre %d/term %d/refund %d, want 1/1/0",
			env.billing.preConsumeCalls(), env.billing.terminateCount(), env.billing.refundCount())
	}
	if fact := env.billing.lastFact(); fact.Usage.Prompt != 10 || fact.Usage.Completion != 5 {
		t.Fatalf("usage fact = %+v, want prompt 10 / completion 5", fact.Usage)
	}
	if counter.count() != 1 {
		t.Fatalf("upstream calls = %d, want 1", counter.count())
	}
}

// TestMessagesGeminiUpstreamJSONRoundTrip：Gemini generateContent → Claude message。
func TestMessagesGeminiUpstreamJSONRoundTrip(t *testing.T) {
	var gotPath string
	env, counter := messagesTestEnv(t, contract.ChannelTypeGemini, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"hello from gemini"}]},`+
			`"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":3,`+
			`"totalTokenCount":10}}`)
	})
	w := env.do(t, http.MethodPost, "/v1/messages",
		`{"model":"claude-test","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"stream":false}`,
		messagesHeaders())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var respObj map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &respObj); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if respObj["type"] != "message" || respObj["stop_reason"] != "end_turn" {
		t.Fatalf("response = %s, want Anthropic message with end_turn", w.Body.String())
	}
	usage := respObj["usage"].(map[string]any)
	if usage["input_tokens"] != float64(7) || usage["output_tokens"] != float64(3) {
		t.Fatalf("usage = %v, want input 7 / output 3", usage)
	}
	if !strings.Contains(gotPath, ":generateContent") {
		t.Fatalf("upstream path = %q, want Gemini generateContent", gotPath)
	}
	if env.billing.terminateCount() != 1 || env.billing.refundCount() != 0 || counter.count() != 1 {
		t.Fatalf("billing/upstream = term %d/refund %d/upstream %d, want 1/0/1",
			env.billing.terminateCount(), env.billing.refundCount(), counter.count())
	}
	if fact := env.billing.lastFact(); fact.Usage.Prompt != 7 || fact.Usage.Completion != 3 {
		t.Fatalf("usage fact = %+v, want prompt 7 / completion 3", fact.Usage)
	}
}

// TestMessagesGeminiUpstreamToolCallID（复核 F1）：Gemini functionCall 没有 id，
// 端到端回写的 tool_use 块必须带非空 id；否则客户端回传的 tool_result 会被
// 本网关的 Anthropic 解析器拒绝（多轮工具调用链断开）。
func TestMessagesGeminiUpstreamToolCallID(t *testing.T) {
	env, _ := messagesTestEnv(t, contract.ChannelTypeGemini, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[`+
			`{"functionCall":{"name":"lookup","args":{"q":"x"}}}]},"finishReason":"STOP"}],`+
			`"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":1,"totalTokenCount":3}}`)
	})
	w := env.do(t, http.MethodPost, "/v1/messages",
		`{"model":"claude-test","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"stream":false}`,
		messagesHeaders())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var respObj map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &respObj); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	blocks, _ := respObj["content"].([]any)
	var toolID string
	for _, raw := range blocks {
		block, _ := raw.(map[string]any)
		if block["type"] == "tool_use" {
			toolID, _ = block["id"].(string)
		}
	}
	if toolID == "" {
		t.Fatalf("response = %s, want non-empty tool_use.id", w.Body.String())
	}
	// 回传：用响应给出的 id 构造 tool_result，网关入口必须能解析（非 400）。
	followup := `{"model":"claude-test","max_tokens":8,"messages":[` +
		`{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"` + toolID + `","name":"lookup","input":{}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + toolID + `","content":"ok"}]}]}`
	if _, err := ir.ParseAnthropicRequest([]byte(followup)); err != nil {
		t.Fatalf("回传 tool_result 被拒绝（id=%q）: %v", toolID, err)
	}
}

// TestMessagesOpenAICrossProtocolStream 证明流式新方向的 Claude 事件序列：
// message_start → content_block_start/delta/stop → message_delta → message_stop，
// 且 usage 与结算同源（message_delta 的 usage == UsageFact）。
func TestMessagesOpenAICrossProtocolStream(t *testing.T) {
	var gotIncludeUsage any
	env, counter := messagesTestEnv(t, contract.ChannelTypeOpenAI, func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &payload)
		if options, ok := payload["stream_options"].(map[string]any); ok {
			gotIncludeUsage = options["include_usage"]
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"id":"chatcmpl-s","model":"gpt-upstream","choices":[{"index":0,"delta":{"content":"Hel"}}]}

data: {"id":"chatcmpl-s","model":"gpt-upstream","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":"stop"}]}

data: {"id":"chatcmpl-s","model":"gpt-upstream","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5}}

data: [DONE]

`)
	})
	w := env.do(t, http.MethodPost, "/v1/messages",
		`{"model":"claude-test","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"stream":true}`,
		messagesHeaders())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Fatalf("content-type = %q, want text/event-stream", got)
	}
	wantOrder := []string{
		"message_start", "content_block_start", "content_block_delta", "content_block_delta",
		"content_block_stop", "message_delta", "message_stop",
	}
	events, datas := parseSSEEvents(t, w.Body.String())
	if strings.Join(events, ",") != strings.Join(wantOrder, ",") {
		t.Fatalf("event sequence = %v, want %v; body=%s", events, wantOrder, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "[DONE]") || strings.Contains(w.Body.String(), "chat.completion.chunk") {
		t.Fatalf("SSE body contains OpenAI framing: %s", w.Body.String())
	}
	var text string
	for i, name := range events {
		if name == "content_block_delta" {
			delta := datas[i]["delta"].(map[string]any)
			if delta["type"] == "text_delta" {
				text += delta["text"].(string)
			}
		}
	}
	if text != "Hello" {
		t.Fatalf("rendered text = %q, want Hello", text)
	}
	var stopReason any
	var outUsage map[string]any
	for i, name := range events {
		if name == "message_delta" {
			stopReason = datas[i]["delta"].(map[string]any)["stop_reason"]
			outUsage = datas[i]["usage"].(map[string]any)
		}
	}
	if stopReason != "end_turn" {
		t.Fatalf("stop_reason = %v, want end_turn", stopReason)
	}
	if outUsage["input_tokens"] != float64(10) || outUsage["output_tokens"] != float64(5) {
		t.Fatalf("message_delta usage = %v, want 10/5", outUsage)
	}
	if gotIncludeUsage != true {
		t.Fatalf("upstream stream_options.include_usage = %v, want true", gotIncludeUsage)
	}
	if env.billing.preConsumeCalls() != 1 || env.billing.terminateCount() != 1 || env.billing.refundCount() != 0 {
		t.Fatalf("billing = pre %d/term %d/refund %d, want 1/1/0",
			env.billing.preConsumeCalls(), env.billing.terminateCount(), env.billing.refundCount())
	}
	fact := env.billing.lastFact()
	if fact.Usage.Prompt != 10 || fact.Usage.Completion != 5 {
		t.Fatalf("usage fact = %+v, want prompt 10 / completion 5", fact.Usage)
	}
	if !fact.Stream {
		t.Fatal("usage fact must be marked Stream")
	}
	if counter.count() != 1 {
		t.Fatalf("upstream calls = %d, want 1", counter.count())
	}
}

// TestMessagesGeminiCrossProtocolStream：Gemini 流式帧同样渲染为 Claude 事件，
// 且 usageMetadata → message_delta.usage。
func TestMessagesGeminiCrossProtocolStream(t *testing.T) {
	env, counter := messagesTestEnv(t, contract.ChannelTypeGemini, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"Hi"}]}}]}

data: {"candidates":[{"content":{"role":"model","parts":[{"text":" there"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":2,"totalTokenCount":6}}

`)
	})
	w := env.do(t, http.MethodPost, "/v1/messages",
		`{"model":"claude-test","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"stream":true}`,
		messagesHeaders())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	events, datas := parseSSEEvents(t, w.Body.String())
	wantOrder := []string{
		"message_start", "content_block_start", "content_block_delta", "content_block_delta",
		"content_block_stop", "message_delta", "message_stop",
	}
	if strings.Join(events, ",") != strings.Join(wantOrder, ",") {
		t.Fatalf("event sequence = %v, want %v; body=%s", events, wantOrder, w.Body.String())
	}
	var outUsage map[string]any
	for i, name := range events {
		if name == "message_delta" {
			outUsage = datas[i]["usage"].(map[string]any)
		}
	}
	if outUsage["input_tokens"] != float64(4) || outUsage["output_tokens"] != float64(2) {
		t.Fatalf("message_delta usage = %v, want 4/2", outUsage)
	}
	if fact := env.billing.lastFact(); fact.Usage.Prompt != 4 || fact.Usage.Completion != 2 {
		t.Fatalf("usage fact = %+v, want prompt 4 / completion 2", fact.Usage)
	}
	if env.billing.terminateCount() != 1 || env.billing.refundCount() != 0 || counter.count() != 1 {
		t.Fatalf("billing/upstream = term %d/refund %d/upstream %d, want 1/0/1",
			env.billing.terminateCount(), env.billing.refundCount(), counter.count())
	}
}

// TestMessagesCrossProtocolStreamMessageStartUsage：异构上游（OpenAI）的精确
// 用量只在流末尾出现，但 message_start 不能写 0 —— 否则客户端在整段生成期间
// 都以为上下文是空的。message_start 用本地估算的展示值，message_delta 仍以
// 上游精确用量覆盖（计费只读后者）。
func TestMessagesCrossProtocolStreamMessageStartUsage(t *testing.T) {
	env, _ := messagesTestEnv(t, contract.ChannelTypeOpenAI, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"id":"chatcmpl-u","model":"gpt-upstream","choices":[{"index":0,"delta":{"content":"Hi"}}]}

data: {"id":"chatcmpl-u","model":"gpt-upstream","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: {"id":"chatcmpl-u","model":"gpt-upstream","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5}}

data: [DONE]

`)
	})
	w := env.do(t, http.MethodPost, "/v1/messages",
		`{"model":"claude-test","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"stream":true}`,
		messagesHeaders())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	events, datas := parseSSEEvents(t, w.Body.String())
	var startUsage, finalUsage map[string]any
	for i, name := range events {
		switch name {
		case "message_start":
			message, _ := datas[i]["message"].(map[string]any)
			startUsage, _ = message["usage"].(map[string]any)
		case "message_delta":
			finalUsage, _ = datas[i]["usage"].(map[string]any)
		}
	}
	if startUsage == nil {
		t.Fatalf("no message_start in %s", w.Body.String())
	}
	inputAtStart, _ := startUsage["input_tokens"].(float64)
	if inputAtStart <= 0 {
		t.Fatalf("message_start usage.input_tokens = %v, want the local display estimate > 0", startUsage["input_tokens"])
	}
	if startUsage["output_tokens"] != float64(0) {
		t.Fatalf("message_start usage.output_tokens = %v, want 0", startUsage["output_tokens"])
	}
	// 精确用量仍以 message_delta 为准，估算值绝不回流到结算。
	if finalUsage["input_tokens"] != float64(10) || finalUsage["output_tokens"] != float64(5) {
		t.Fatalf("message_delta usage = %v, want exact 10/5", finalUsage)
	}
	if fact := env.billing.lastFact(); fact.Usage.Prompt != 10 || fact.Usage.Completion != 5 {
		t.Fatalf("usage fact = %+v, want prompt 10 / completion 5", fact.Usage)
	}
}

// TestMessagesGeminiStreamSignatureDelta：Gemini 把思考块的可回放签名挂在
// thought part 上，网关必须转成 Anthropic 的 signature_delta 并落在 thinking
// 块内（块关闭之前），否则客户端回放历史思考块时会判定为被篡改。
func TestMessagesGeminiStreamSignatureDelta(t *testing.T) {
	env, _ := messagesTestEnv(t, contract.ChannelTypeGemini, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"ponder","thought":true,"thoughtSignature":"sig-think"}]}}]}

data: {"candidates":[{"content":{"role":"model","parts":[{"text":"Answer"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":2,"totalTokenCount":6}}

`)
	})
	w := env.do(t, http.MethodPost, "/v1/messages",
		`{"model":"claude-test","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"stream":true}`,
		messagesHeaders())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	events, datas := parseSSEEvents(t, w.Body.String())
	sigPos, sigIdx := -1, float64(-1)
	for i, name := range events {
		if name != "content_block_delta" {
			continue
		}
		delta, _ := datas[i]["delta"].(map[string]any)
		if delta["type"] != "signature_delta" {
			continue
		}
		sigPos = i
		sigIdx, _ = datas[i]["index"].(float64)
		if delta["signature"] != "sig-think" {
			t.Fatalf("signature_delta.signature = %v, want sig-think", delta["signature"])
		}
	}
	if sigPos < 0 {
		t.Fatalf("没有 signature_delta 事件；body=%s", w.Body.String())
	}
	startPos, stopPos := -1, -1
	for i, name := range events {
		idx, _ := datas[i]["index"].(float64)
		if idx != sigIdx {
			continue
		}
		switch name {
		case "content_block_start":
			if block, ok := datas[i]["content_block"].(map[string]any); ok && block["type"] == "thinking" {
				startPos = i
			}
		case "content_block_stop":
			stopPos = i
		}
	}
	if startPos < 0 || stopPos < 0 {
		t.Fatalf("signature_delta(index=%v) 不在 thinking 块内；body=%s", sigIdx, w.Body.String())
	}
	if !(startPos < sigPos && sigPos < stopPos) {
		t.Fatalf("顺序 = start #%d, signature #%d, stop #%d；签名必须在 thinking 块内、关闭前",
			startPos, sigPos, stopPos)
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
//
// W18-B · R-W18-03：OpenAI/Gemini 也属支持（经响应编码器），任务插件等
// 无三类适配器的类型仍不支持。
func TestMessagesDirectionSupported(t *testing.T) {
	anthropicCh := &store.Channel{ID: 1, Type: string(contract.ChannelTypeAnthropic)}
	openaiCh := &store.Channel{ID: 2, Type: string(contract.ChannelTypeOpenAI)}
	moonshotCh := &store.Channel{ID: 3, Type: string(contract.ChannelTypeMoonshot)}
	geminiCh := &store.Channel{ID: 4, Type: string(contract.ChannelTypeGemini)}
	taskCh := &store.Channel{ID: 5, Type: string(contract.ChannelTypeTaskPlugin)}
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
			channels: map[int64]*store.Channel{2: openaiCh}, want: true},
		{name: "gemini", candidates: []contract.ChannelCandidate{{ChannelID: 4}},
			channels: map[int64]*store.Channel{4: geminiCh}, want: true},
		{name: "task plugin", candidates: []contract.ChannelCandidate{{ChannelID: 5}},
			channels: map[int64]*store.Channel{5: taskCh}, want: false},
		{name: "empty", candidates: nil, channels: map[int64]*store.Channel{}, want: false},
		// Get 失败的候选按不可用跳过：后面的 Anthropic 候选仍能判定为可用。
		{name: "get error then anthropic",
			candidates: []contract.ChannelCandidate{{ChannelID: 9}, {ChannelID: 1}},
			channels:   map[int64]*store.Channel{1: anthropicCh}, want: true},
		// 全部 Get 失败 → 没有可用渠道。
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

// messagesCrossProtoStreamEnv 与 messagesStreamEnv 同构，但包装的是
// OpenAI/Gemini 适配器——用于跨协议流式渲染（W18-B）的观察点。
func messagesCrossProtoStreamEnv(t *testing.T, channelType contract.ChannelType, upstreamHandler http.HandlerFunc) (*relayTestEnv, *observingStreamAdapter) {
	t.Helper()
	env, _ := messagesTestEnv(t, channelType, upstreamHandler)
	var (
		inner contract.StreamAdapter
		ok    bool
	)
	switch channelType {
	case contract.ChannelTypeOpenAI:
		// *OpenAI 直接满足 StreamAdapter（无需断言）。
		inner = adapters.NewOpenAI()
		ok = true
	case contract.ChannelTypeGemini:
		inner, ok = adapters.NewGeminiAdapter(channelType).(contract.StreamAdapter)
	default:
		t.Fatalf("messagesCrossProtoStreamEnv: unsupported channel type %q", channelType)
	}
	if !ok {
		t.Fatalf("%q adapter must implement StreamAdapter", channelType)
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

// TestMessagesCrossProtocolStreamClientAbortRefunds（复核 F5）：跨协议渲染路径
// 的客户端断开同样必须恰好一次 Refund、零 Terminate，且不写伪造的收尾帧。
// 与同协议透传的差异：没有 tee 写端可以立刻发现断开，因此这里覆盖
// 「取消 → 上游 body 级联取消 → 事件循环 EOF」这条不会再有 yield 回调的路径。
func TestMessagesCrossProtocolStreamClientAbortRefunds(t *testing.T) {
	env, observing := messagesCrossProtoStreamEnv(t, contract.ChannelTypeOpenAI, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		// 先给一段内容，确保网关已经写出 message_start 与 text delta。
		_, _ = io.WriteString(w, `data: {"id":"chatcmpl-abort","model":"gpt","choices":[{"index":0,"delta":{"content":"partial"}}]}`+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		// 保持流打开，直到网关因客户端断开而关闭上游请求。
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
	buf := make([]byte, 4096)
	n, readErr := resp.Body.Read(buf)
	if readErr != nil && n == 0 {
		t.Fatalf("read first frame: %v", readErr)
	}
	first := string(buf[:n])
	cancel()
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
	if strings.Contains(body, "message_stop") || strings.Contains(body, "message_delta") {
		t.Fatalf("gateway forged a closing event on abort: %q", body)
	}
}

// cleanEOFCrossProtoAdapter 模拟「客户端断开 → 上游干净 EOF、此后不再产出
// 任何事件」的适配器：专门覆盖渲染桥**在事件循环结束后**才观察到 ctx 取消的
// 路径（复核 F5）。真实的 OpenAI/Gemini 适配器通常会在读失败时交一个
// EventError，从而走循环内的 ctx 检查；本适配器刻意不交，确保收尾判定不只
// 依赖「下一次 yield」。
type cleanEOFCrossProtoAdapter struct {
	*adapters.OpenAI
}

func (a *cleanEOFCrossProtoAdapter) StreamEvents(_ *contract.RelayContext, rw io.ReadWriter) iter.Seq[contract.StreamEvent] {
	return func(yield func(contract.StreamEvent) bool) {
		buf := make([]byte, 256)
		for {
			if _, err := rw.Read(buf); err != nil {
				// 干净 EOF/读错都不产出事件：模拟上游正常关闭且没有收尾语义。
				return
			}
		}
	}
}

// TestMessagesCrossProtocolStreamCleanEOFAfterAbortRefunds：客户端断开后上游
// 干净 EOF（无收尾事件）时，必须判定为 client_abort 并 Refund，绝不因为
// 「循环正常结束」就写 message_delta/message_stop 或结算。
func TestMessagesCrossProtocolStreamCleanEOFAfterAbortRefunds(t *testing.T) {
	env, observing := func() (*relayTestEnv, *observingStreamAdapter) {
		env, _ := messagesTestEnv(t, contract.ChannelTypeOpenAI, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			flusher, _ := w.(http.Flusher)
			// 一个真实字节块（由假适配器读取后丢弃），随后保持连接直到客户端断开。
			_, _ = io.WriteString(w, `data: {"id":"chatcmpl-eof","model":"gpt","choices":[{"index":0,"delta":{"content":"partial"}}]}`+"\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			<-r.Context().Done()
		})
		observing := &observingStreamAdapter{inner: &cleanEOFCrossProtoAdapter{OpenAI: adapters.NewOpenAI()}}
		env.handler.dispatch = func(contract.ChannelType) (contract.Adapter, error) { return observing, nil }
		return env, observing
	}()
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
	buf := make([]byte, 512)
	n, readErr := resp.Body.Read(buf)
	if readErr != nil && n == 0 {
		t.Fatalf("read first frame: %v", readErr)
	}
	first := string(buf[:n])
	cancel()
	rest, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	body := first + string(rest)

	deadline := time.Now().Add(5 * time.Second)
	for env.billing.refundCount() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("billing session was not refunded after clean-EOF disconnect")
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if got := env.billing.refundCount(); got != 1 {
		t.Fatalf("Refund calls = %d, want exactly 1", got)
	}
	if got := env.billing.terminateCount(); got != 0 {
		t.Fatalf("Terminate calls = %d, want 0（干净 EOF + 客户端断开仍属失败出口）", got)
	}
	if observing.rc == nil || observing.rc.StreamStatus == nil {
		t.Fatal("StreamStatus was not recorded")
	}
	if got := observing.rc.StreamStatus.EndReason; got != contract.EndClientAbort {
		t.Fatalf("EndReason = %d, want EndClientAbort", got)
	}
	if strings.Contains(body, "message_stop") || strings.Contains(body, "message_delta") {
		t.Fatalf("gateway forged a closing event on abort: %q", body)
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

func TestMessagesEstimateCarriesRawRequest(t *testing.T) {
	env, _ := messagesTestEnv(t, contract.ChannelTypeAnthropic, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, messagesUpstreamBody)
	})

	w := env.do(t, http.MethodPost, "/v1/messages",
		`{"model":"claude-test","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
		messagesHeaders())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	est, ok := env.billing.lastEstimate()
	if !ok {
		t.Fatal("PreConsume 没有收到估算")
	}
	if est.Raw == nil || len(est.Raw.Messages) == 0 {
		t.Fatalf("QuotaEstimate.Raw = %+v, want 本次请求 IR", est.Raw)
	}
}

func TestMessagesSettlementFailureReturns402(t *testing.T) {
	env, _ := messagesTestEnv(t, contract.ChannelTypeAnthropic, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, messagesUpstreamBody)
	})
	env.billing.terminateErr = fmt.Errorf("%w: settle", billing.ErrInsufficientQuota)

	w := env.do(t, http.MethodPost, "/v1/messages",
		`{"model":"claude-test","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
		messagesHeaders())
	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "insufficient quota") {
		t.Fatalf("body = %s, want Claude 402 文案", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "msg_1") {
		t.Fatal("结算失败时不得交付上游响应")
	}
}

// TestMessagesPostChargeLogFailureStillDelivers 是 Important-2 的 Claude 入口：
// 资金已提交后日志写失败仍交付，信封保持 200。
func TestMessagesPostChargeLogFailureStillDelivers(t *testing.T) {
	env, _ := messagesTestEnv(t, contract.ChannelTypeAnthropic, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, messagesUpstreamBody)
	})
	env.billing.terminateErr = fmt.Errorf("%w: logdb down", billing.ErrPostChargeLogFailed)

	w := env.do(t, http.MethodPost, "/v1/messages",
		`{"model":"claude-test","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
		messagesHeaders())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200（钱已收，必须交付）; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "msg_1") {
		t.Fatalf("body = %s, want 已交付的上游响应", w.Body.String())
	}
}
