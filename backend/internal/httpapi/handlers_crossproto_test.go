package httpapi

// 跨协议转发端点行为测试（M4 Task 5）。
//
// 覆盖计划 Step 1 的 (a)–(e) 与补充项：
//   - 开关关闭 + 协议头 → 403（固定口径；未知头值也 403）；
//   - 开关开启 + 未知协议 → 400；
//   - 开启 + OpenAI 请求 → Anthropic/Gemini 上游（请求方向经 IR 编码）；
//   - 上游原生响应 → 客户端 OpenAI 形状（响应方向经 IR 回写）；
//   - 鉴权/计费不降级（未认证 401；成功请求仍 PreConsume/Terminate）；
//   - GC-6 凭据零回传；
//   - 未携带协议头时与 M2 行为逐字节一致。

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/store"
)

// fakeRelayConfig 是 ConfigReader 的内存实现（值按 configcenter 的 JSON 快照语义存放）。
type fakeRelayConfig map[string]json.RawMessage

func (f fakeRelayConfig) Get(key string) (json.RawMessage, bool) {
	raw, ok := f[key]
	return raw, ok
}

func crossProtoConfig(enabled bool) fakeRelayConfig {
	value, _ := json.Marshal(enabled)
	return fakeRelayConfig{"relay.cross_protocol_forward_enabled": value}
}

// recordedUpstream 记录最后一次上游请求，并按路径返回对应协议的响应。
type recordedUpstream struct {
	mu        sync.Mutex
	calls     int
	path      string
	apiKey    string
	googKey   string
	body      []byte
	anthropic bool
	gemini    bool
}

const (
	anthropicUpstreamBody = `{
	  "id": "msg_up",
	  "type": "message",
	  "role": "assistant",
	  "model": "claude-sonnet-4-5",
	  "content": [{"type": "text", "text": "hi from claude"}],
	  "stop_reason": "end_turn",
	  "usage": {"input_tokens": 4, "output_tokens": 6}
	}`
	geminiUpstreamBody = `{
	  "responseId": "resp_up",
	  "modelVersion": "gemini-2.5-flash",
	  "candidates": [{
	    "index": 0,
	    "content": {"role": "model", "parts": [{"text": "hi from gemini"}]},
	    "finishReason": "STOP"
	  }],
	  "usageMetadata": {"promptTokenCount": 3, "candidatesTokenCount": 4, "totalTokenCount": 7}
	}`
	openAIUpstreamBody = `{"id":"chatcmpl-up","object":"chat.completion","model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"hi from openai"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`
)

func newRecordedUpstream() (*recordedUpstream, *httptest.Server) {
	rec := &recordedUpstream{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.mu.Lock()
		rec.calls++
		rec.path = r.URL.Path
		rec.apiKey = r.Header.Get("x-api-key")
		rec.googKey = r.Header.Get("x-goog-api-key")
		rec.body = body
		rec.anthropic = strings.Contains(r.URL.Path, "/v1/messages")
		rec.gemini = strings.Contains(r.URL.Path, ":generateContent")
		anthropic, gemini := rec.anthropic, rec.gemini
		rec.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch {
		case anthropic:
			_, _ = w.Write([]byte(anthropicUpstreamBody))
		case gemini:
			_, _ = w.Write([]byte(geminiUpstreamBody))
		default:
			_, _ = w.Write([]byte(openAIUpstreamBody))
		}
	}))
	return rec, server
}

func (r *recordedUpstream) snapshot() (calls int, path, apiKey, googKey string, body []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls, r.path, r.apiKey, r.googKey, append([]byte(nil), r.body...)
}

// crossProtoChannels 构造 n 个渠道（ID 从 1 起），每个候选优先级/权重相同。
func crossProtoChannels(t *testing.T, sealer *SecretSealer, upstreamURL string, types ...contract.ChannelType) *fakeRelayChannelStore {
	t.Helper()
	channels := &fakeRelayChannelStore{
		channels: make(map[int64]*store.Channel, len(types)),
	}
	for i, channelType := range types {
		id := int64(i + 1)
		channels.candidates = append(channels.candidates, contract.ChannelCandidate{
			ChannelID: int(id),
			Priority:  10,
			Weight:    1,
		})
		baseURL := upstreamURL
		channels.channels[id] = &store.Channel{
			ID:              id,
			Name:            "test-" + string(channelType),
			Type:            string(channelType),
			BaseURL:         &baseURL,
			Credentials:     sealedTestCredentials(t, sealer),
			SupportedModels: []byte(`["gpt-test"]`),
			ModelMapping:    []byte(`{}`),
			Status:          1,
		}
	}
	return channels
}

// newCrossProtoEnv 构造完整中继依赖；enabled 决定跨协议转发开关的取值。
func newCrossProtoEnv(t *testing.T, upstream *httptest.Server, channels *fakeRelayChannelStore, enabled bool) *relayTestEnv {
	t.Helper()
	sealer := NewSecretSealer(relaySealerMaster)
	tokenStore := newFakeRelayTokenStore(relayTestToken, &auth.TokenMeta{
		ID:             relayTestTokenID,
		UserID:         relayTestUserID,
		Status:         "enabled",
		UnlimitedQuota: true,
	})
	access := &fakeRelayAccess{role: "admin", status: "enabled"}
	billing := newRecordingBilling()
	h := NewRelayHandler(RelayHandlerOptions{
		Channels: channels,
		Tokens:   tokenStore,
		Access:   access,
		Sealer:   sealer,
		Client:   upstream.Client(),
		Billing:  func(*contract.RelayContext) contract.BillingSession { return billing },
		Config:   crossProtoConfig(enabled),
	})
	return &relayTestEnv{handler: h, channels: channels, tokens: tokenStore, access: access, billing: billing, sealer: sealer}
}

const crossProtoChatBody = `{"model":"gpt-test","messages":[{"role":"user","content":"hi"}]}`

func crossProtoHeaders(protocol string) map[string]string {
	return map[string]string{
		"Authorization":    "Bearer " + relayTestToken,
		"x-newtg-protocol": protocol,
		"Content-Type":     "application/json",
	}
}

// decodeRelayError 解析 /v1 的 OpenAI 错误信封。
func decodeRelayError(t *testing.T, body []byte) relayErrorEnvelope {
	t.Helper()
	var envelope relayErrorEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("错误信封不是合法 JSON: %v（%s）", err, body)
	}
	return envelope
}

func TestCrossProtoDisabledHeaderReturns403(t *testing.T) {
	_, upstream := newRecordedUpstream()
	defer upstream.Close()
	sealer := NewSecretSealer(relaySealerMaster)
	channels := crossProtoChannels(t, sealer, upstream.URL, contract.ChannelTypeAnthropic)
	env := newCrossProtoEnv(t, upstream, channels, false)

	w := env.do(t, http.MethodPost, "/v1/chat/completions", crossProtoChatBody, crossProtoHeaders("anthropic"))
	if w.Code != http.StatusForbidden {
		t.Fatalf("状态码 = %d，期望 403（body=%s）", w.Code, w.Body.String())
	}
	if got := decodeRelayError(t, w.Body.Bytes()).Error.Code; got != "cross_protocol_forward_disabled" {
		t.Fatalf("错误码 = %q", got)
	}
	if env.billing.preConsumeCalls() != 0 || env.billing.terminateCount() != 0 {
		t.Fatal("开关关闭时不得进入计费路径（不得降级鉴权/计费语义）")
	}
}

// 关闭 + 未知头值必须仍是 403：这是 GC-8 的固定口径（先判开关，后判取值），
// 防止关闭状态把「功能未启用」泄露成「值拼错了」。
func TestCrossProtoDisabledUnknownHeaderReturns403(t *testing.T) {
	_, upstream := newRecordedUpstream()
	defer upstream.Close()
	sealer := NewSecretSealer(relaySealerMaster)
	channels := crossProtoChannels(t, sealer, upstream.URL, contract.ChannelTypeOpenAI)
	env := newCrossProtoEnv(t, upstream, channels, false)

	w := env.do(t, http.MethodPost, "/v1/chat/completions", crossProtoChatBody, crossProtoHeaders("cohere"))
	if w.Code != http.StatusForbidden {
		t.Fatalf("状态码 = %d，期望 403（body=%s）", w.Code, w.Body.String())
	}
	if got := decodeRelayError(t, w.Body.Bytes()).Error.Code; got != "cross_protocol_forward_disabled" {
		t.Fatalf("错误码 = %q", got)
	}
}

func TestCrossProtoUnknownProtocolReturns400(t *testing.T) {
	_, upstream := newRecordedUpstream()
	defer upstream.Close()
	sealer := NewSecretSealer(relaySealerMaster)
	channels := crossProtoChannels(t, sealer, upstream.URL, contract.ChannelTypeOpenAI)
	env := newCrossProtoEnv(t, upstream, channels, true)

	w := env.do(t, http.MethodPost, "/v1/chat/completions", crossProtoChatBody, crossProtoHeaders("cohere"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400（body=%s）", w.Code, w.Body.String())
	}
	if got := decodeRelayError(t, w.Body.Bytes()).Error.Code; got != "invalid_protocol" {
		t.Fatalf("错误码 = %q", got)
	}
}

// 目标协议没有匹配渠道：显式 400，不得静默回退到其他协议（Review Focus 5）。
func TestCrossProtoNoMatchingChannelReturns400(t *testing.T) {
	rec, upstream := newRecordedUpstream()
	defer upstream.Close()
	sealer := NewSecretSealer(relaySealerMaster)
	channels := crossProtoChannels(t, sealer, upstream.URL, contract.ChannelTypeOpenAI)
	env := newCrossProtoEnv(t, upstream, channels, true)

	w := env.do(t, http.MethodPost, "/v1/chat/completions", crossProtoChatBody, crossProtoHeaders("anthropic"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400（body=%s）", w.Code, w.Body.String())
	}
	if got := decodeRelayError(t, w.Body.Bytes()).Error.Code; got != "endpoint_not_supported" {
		t.Fatalf("错误码 = %q", got)
	}
	if calls, _, _, _, _ := rec.snapshot(); calls != 0 {
		t.Fatalf("无匹配渠道时不得发起上游请求，calls=%d", calls)
	}
	if env.billing.preConsumeCalls() != 0 {
		t.Fatal("协议过滤发生在预扣之前：不得产生任何计费副作用")
	}
}

func TestCrossProtoOpenAIToAnthropicViaIR(t *testing.T) {
	rec, upstream := newRecordedUpstream()
	defer upstream.Close()
	sealer := NewSecretSealer(relaySealerMaster)
	channels := crossProtoChannels(t, sealer, upstream.URL,
		contract.ChannelTypeOpenAI, contract.ChannelTypeAnthropic)
	env := newCrossProtoEnv(t, upstream, channels, true)

	w := env.do(t, http.MethodPost, "/v1/chat/completions", crossProtoChatBody, crossProtoHeaders("anthropic"))
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（body=%s）", w.Code, w.Body.String())
	}

	calls, path, apiKey, _, body := rec.snapshot()
	if calls != 1 {
		t.Fatalf("上游调用次数 = %d，期望 1", calls)
	}
	// 请求方向：经 IR 编码为 Anthropic Messages 形状。
	if path != "/v1/messages" {
		t.Fatalf("上游路径 = %q，期望 /v1/messages", path)
	}
	if apiKey != upstreamCredential {
		t.Fatalf("x-api-key = %q（Anthropic 凭据头缺失/错误）", apiKey)
	}
	var upstreamReq map[string]any
	if err := json.Unmarshal(body, &upstreamReq); err != nil {
		t.Fatalf("上游请求体不是 JSON: %v（%s）", err, body)
	}
	if _, ok := upstreamReq["max_tokens"]; !ok {
		t.Fatalf("Anthropic 请求缺少 max_tokens：%s", body)
	}
	if _, ok := upstreamReq["messages"]; !ok {
		t.Fatalf("Anthropic 请求缺少 messages：%s", body)
	}

	// 响应方向：Anthropic 原生响应必须回写为 OpenAI Chat Completions 形状，
	// 而不是把上游原生 JSON 直接透传给 OpenAI 客户端。
	var payload struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("客户端响应不是 JSON: %v（%s）", err, w.Body.String())
	}
	if payload.Object != "chat.completion" || len(payload.Choices) != 1 ||
		payload.Choices[0].Message.Content != "hi from claude" {
		t.Fatalf("客户端响应 = %s", w.Body.String())
	}

	// 鉴权/计费不降级：成功请求仍走 PreConsume → Terminate。
	if env.billing.preConsumeCalls() != 1 || env.billing.terminateCount() != 1 {
		t.Fatalf("计费调用 = preConsume:%d terminate:%d", env.billing.preConsumeCalls(), env.billing.terminateCount())
	}
	// GC-6：凭据零回传。
	if strings.Contains(w.Body.String(), upstreamCredential) {
		t.Fatalf("客户端响应泄露上游凭据: %s", w.Body.String())
	}
}

func TestCrossProtoOpenAIToGeminiViaIR(t *testing.T) {
	rec, upstream := newRecordedUpstream()
	defer upstream.Close()
	sealer := NewSecretSealer(relaySealerMaster)
	channels := crossProtoChannels(t, sealer, upstream.URL, contract.ChannelTypeGemini)
	env := newCrossProtoEnv(t, upstream, channels, true)

	w := env.do(t, http.MethodPost, "/v1/chat/completions", crossProtoChatBody, crossProtoHeaders("gemini"))
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（body=%s）", w.Code, w.Body.String())
	}

	calls, path, _, googKey, body := rec.snapshot()
	if calls != 1 {
		t.Fatalf("上游调用次数 = %d，期望 1", calls)
	}
	if !strings.Contains(path, "/models/gpt-test:generateContent") {
		t.Fatalf("上游路径 = %q，期望 Gemini generateContent", path)
	}
	if googKey != upstreamCredential {
		t.Fatalf("x-goog-api-key = %q（Gemini 凭据头缺失/错误）", googKey)
	}
	var upstreamReq map[string]any
	if err := json.Unmarshal(body, &upstreamReq); err != nil {
		t.Fatalf("上游请求体不是 JSON: %v（%s）", err, body)
	}
	if _, ok := upstreamReq["contents"]; !ok {
		t.Fatalf("Gemini 请求缺少 contents：%s", body)
	}

	var payload struct {
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("客户端响应不是 JSON: %v（%s）", err, w.Body.String())
	}
	if payload.Object != "chat.completion" || len(payload.Choices) != 1 ||
		payload.Choices[0].Message.Content != "hi from gemini" {
		t.Fatalf("客户端响应 = %s", w.Body.String())
	}
	if env.billing.preConsumeCalls() != 1 || env.billing.terminateCount() != 1 {
		t.Fatalf("计费调用 = preConsume:%d terminate:%d", env.billing.preConsumeCalls(), env.billing.terminateCount())
	}
	if strings.Contains(w.Body.String(), upstreamCredential) {
		t.Fatalf("客户端响应泄露上游凭据: %s", w.Body.String())
	}
}

// 未携带协议头：即使开关开启，也必须与 M2 完全一致（OpenAI 原始响应逐字节透传，
// 且不做任何协议过滤）。
func TestCrossProtoNoHeaderKeepsM2Behavior(t *testing.T) {
	rec, upstream := newRecordedUpstream()
	defer upstream.Close()
	sealer := NewSecretSealer(relaySealerMaster)
	channels := crossProtoChannels(t, sealer, upstream.URL, contract.ChannelTypeOpenAI)
	env := newCrossProtoEnv(t, upstream, channels, true)

	w := env.doAuthed(t, http.MethodPost, "/v1/chat/completions", crossProtoChatBody)
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（body=%s）", w.Code, w.Body.String())
	}
	if w.Body.String() != openAIUpstreamBody {
		t.Fatalf("未携带协议头时必须逐字节透传上游 OpenAI 响应；得到 %s", w.Body.String())
	}
	if calls, _, _, _, _ := rec.snapshot(); calls != 1 {
		t.Fatalf("上游调用次数 = %d", calls)
	}
}

// 鉴权不降级：协议头不能绕过 RelayAuth。
func TestCrossProtoRequiresAuth(t *testing.T) {
	_, upstream := newRecordedUpstream()
	defer upstream.Close()
	sealer := NewSecretSealer(relaySealerMaster)
	channels := crossProtoChannels(t, sealer, upstream.URL, contract.ChannelTypeAnthropic)
	env := newCrossProtoEnv(t, upstream, channels, true)

	w := env.do(t, http.MethodPost, "/v1/chat/completions", crossProtoChatBody,
		map[string]string{"x-newtg-protocol": "anthropic", "Content-Type": "application/json"})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("未认证请求状态码 = %d，期望 401（body=%s）", w.Code, w.Body.String())
	}
	if env.billing.preConsumeCalls() != 0 {
		t.Fatal("未认证请求不得进入计费路径")
	}
}
