package adapters

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"newtg/gateway/internal/contract"
)

func anthropicContext(credential string) *contract.RelayContext {
	return &contract.RelayContext{
		ChannelID:   14,
		ChannelType: contract.ChannelTypeAnthropic,
		Credential:  credential,
		APIBase:     "",
		OriginModel: "claude-sonnet-4-5",
	}
}

func collectStreamEvents(seq func(func(contract.StreamEvent) bool)) []contract.StreamEvent {
	var events []contract.StreamEvent
	for event := range seq {
		events = append(events, event)
	}
	return events
}

func TestAnthropicBuildRequestHeadersAndBody(t *testing.T) {
	adapter := NewAnthropicAdapter(contract.ChannelTypeAnthropic)
	req := &contract.Request{
		Model:  "claude-sonnet-4-5",
		Stream: true,
		Messages: []contract.Message{{
			Role:    contract.RoleUser,
			Content: []contract.ContentPart{{Kind: contract.PartText, Text: "hi"}},
		}},
	}
	httpReq, err := adapter.BuildRequest(anthropicContext("sk-ant-secret"), req)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if httpReq.URL.String() != "https://api.anthropic.com/v1/messages" {
		t.Fatalf("URL = %q", httpReq.URL.String())
	}
	if got := httpReq.Header.Get("x-api-key"); got != "sk-ant-secret" {
		t.Fatalf("x-api-key = %q", got)
	}
	if got := httpReq.Header.Get("anthropic-version"); got != anthropicVersion {
		t.Fatalf("anthropic-version = %q", got)
	}
	if got := httpReq.Header.Get("Accept"); got != "text/event-stream" {
		t.Fatalf("Accept = %q", got)
	}
	if httpReq.GetBody == nil || httpReq.ContentLength <= 0 {
		t.Fatal("BuildRequest 必须可重放（ContentLength/GetBody）")
	}
	body, _ := io.ReadAll(httpReq.Body)
	var built map[string]any
	if err := jsonUnmarshal(body, &built); err != nil {
		t.Fatal(err)
	}
	if built["model"] != "claude-sonnet-4-5" || built["stream"] != true {
		t.Fatalf("请求体字段错误：%s", body)
	}
	if _, ok := built["max_tokens"]; !ok {
		t.Fatalf("Anthropic 请求体必须含 max_tokens：%s", body)
	}
}

func TestAnthropicMoonshotURLPrefix(t *testing.T) {
	adapter := NewAnthropicAdapter(contract.ChannelTypeMoonshot)
	req := &contract.Request{
		Model:    "kimi-k2",
		Messages: []contract.Message{{Role: contract.RoleUser, Content: []contract.ContentPart{{Kind: contract.PartText, Text: "hi"}}}},
	}
	rc := &contract.RelayContext{ChannelID: 25, ChannelType: contract.ChannelTypeMoonshot, Credential: "sk-moon"}
	httpReq, err := adapter.BuildRequest(rc, req)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if got := httpReq.URL.String(); got != "https://api.moonshot.cn/anthropic/v1/messages" {
		t.Fatalf("moonshot URL = %q", got)
	}
	if adapter.Type() != contract.ChannelTypeMoonshot {
		t.Fatalf("Type = %q", adapter.Type())
	}

	// base_url 已带 /anthropic 时不得重复。
	rc.APIBase = "https://api.moonshot.cn/anthropic"
	httpReq, err = adapter.BuildRequest(rc, req)
	if err != nil {
		t.Fatal(err)
	}
	if got := httpReq.URL.String(); got != "https://api.moonshot.cn/anthropic/v1/messages" {
		t.Fatalf("去重后 URL = %q", got)
	}

	// base_url 已带 /v1 时不得拼出 /v1/v1；当前实现产生 /v1/anthropic/v1/messages
	// （前缀是类型语义，不参与 /v1 去重），这里钉住实际路径防止回归。
	rc.APIBase = "https://proxy.example.com/v1"
	httpReq, err = adapter.BuildRequest(rc, req)
	if err != nil {
		t.Fatal(err)
	}
	if got := httpReq.URL.String(); got != "https://proxy.example.com/v1/anthropic/v1/messages" {
		t.Fatalf("base_url=/v1 的实际路径 = %q, want https://proxy.example.com/v1/anthropic/v1/messages", got)
	}

	// base_url 同时带前缀与 /v1 时也不得重复任一段。
	rc.APIBase = "https://api.moonshot.cn/anthropic/v1"
	httpReq, err = adapter.BuildRequest(rc, req)
	if err != nil {
		t.Fatal(err)
	}
	if got := httpReq.URL.String(); got != "https://api.moonshot.cn/anthropic/v1/messages" {
		t.Fatalf("前缀+版本去重后 URL = %q", got)
	}
}

func TestAnthropicCredentialNotLeakedInErrors(t *testing.T) {
	adapter := NewAnthropicAdapter(contract.ChannelTypeAnthropic)
	req := &contract.Request{
		Model:    "m",
		Messages: []contract.Message{{Role: contract.RoleUser, Content: []contract.ContentPart{{Kind: contract.PartText, Text: "hi"}}}},
	}
	const secret = "sk-ant-supersecret"
	rc := anthropicContext(secret)
	rc.APIBase = "http://[::bad-host"
	if _, err := adapter.BuildRequest(rc, req); err == nil {
		t.Fatal("非法 base URL 必须报错")
	} else if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "bad-host") {
		t.Fatalf("GC-6：错误文本不得包含凭据或 base URL：%v", err)
	}

	// 401 的响应体可能回显 key：错误串不得包含它。
	resp := &http.Response{
		StatusCode: http.StatusUnauthorized,
		Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"invalid key ` + secret + `"}}`)),
	}
	_, _, err := adapter.ParseResponse(rc, resp)
	if err == nil {
		t.Fatal("401 必须报错")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("GC-6：401 错误文本泄露凭据：%v", err)
	}
	if class, ok := ClassOf(err); !ok || class != contract.ErrUpstreamAuth {
		t.Fatalf("401 分类 = %v/%v, want ErrUpstreamAuth", class, ok)
	}
}

func TestAnthropicParseResponse(t *testing.T) {
	adapter := NewAnthropicAdapter(contract.ChannelTypeAnthropic)
	body := `{"id":"msg_1","model":"claude-x","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`
	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}
	response, usage, err := adapter.ParseResponse(anthropicContext("k"), resp)
	if err != nil {
		t.Fatalf("ParseResponse: %v", err)
	}
	if response.FinishReason != "stop" || response.Choices[0].Message.Content[0].Text != "hello" {
		t.Fatalf("response = %+v", response)
	}
	if usage.Prompt != 3 || usage.Completion != 2 || usage.PromptIncludesCache {
		t.Fatalf("usage = %+v", usage)
	}

	// 非 JSON 的成功响应按上游故障处理。
	bad := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("not-json"))}
	if _, _, err := adapter.ParseResponse(anthropicContext("k"), bad); err == nil {
		t.Fatal("非法响应体必须报错")
	} else if class, _ := ClassOf(err); class != contract.ErrUpstreamServer {
		t.Fatalf("非法 2xx 响应分类 = %v, want ErrUpstreamServer", class)
	}
}

func TestAnthropicClassifyError(t *testing.T) {
	adapter := NewAnthropicAdapter(contract.ChannelTypeAnthropic)
	cases := map[int]contract.ErrorClass{
		400: contract.ErrParamInvalid,
		401: contract.ErrUpstreamAuth,
		403: contract.ErrUpstreamAuth,
		404: contract.ErrParamInvalid,
		408: contract.ErrUnknown,
		422: contract.ErrParamInvalid,
		429: contract.ErrUpstreamRateLimit,
		500: contract.ErrUpstreamServer,
		503: contract.ErrUpstreamServer,
		302: contract.ErrRetryable,
	}
	for status, want := range cases {
		if got := adapter.ClassifyError(status, []byte("secret-body")); got != want {
			t.Fatalf("status %d: got %v, want %v", status, got, want)
		}
	}
}

func TestAnthropicStreamEvents(t *testing.T) {
	adapter := NewAnthropicAdapter(contract.ChannelTypeAnthropic)
	sse := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"model":"claude-x","usage":{"input_tokens":10,"cache_read_input_tokens":3}}}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"lookup"}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"q\":"}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}`,
		"",
		"event: ping",
		`data: {"type":"ping"}`,
		"",
		"event: unknown_future_event",
		`data: {"type":"unknown_future_event","payload":1}`,
		"",
		"event: message_delta",
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
	}, "\n")
	buf := bytes.NewBufferString(sse)
	events := collectStreamEvents(adapter.(contract.StreamAdapter).StreamEvents(anthropicContext("k"), buf))

	var usageEvents []contract.Usage
	var deltas []string
	var reasoning []string
	var toolDeltas []contract.ToolCallDelta
	finishCount, pingCount := 0, 0
	for _, event := range events {
		switch event.Kind {
		case contract.EventUsage:
			if event.Usage != nil {
				usageEvents = append(usageEvents, *event.Usage)
			}
		case contract.EventDelta:
			if event.Delta != "" {
				deltas = append(deltas, event.Delta)
			}
			if event.Reasoning != "" {
				reasoning = append(reasoning, event.Reasoning)
			}
			if event.ToolDelta != nil {
				toolDeltas = append(toolDeltas, *event.ToolDelta)
			}
		case contract.EventFinish:
			finishCount++
		case contract.EventPing:
			pingCount++
		}
	}
	if strings.Join(deltas, "") != "hi" {
		t.Fatalf("文本增量 = %v, want [hi]", deltas)
	}
	if strings.Join(reasoning, "") != "hmm" {
		t.Fatalf("思考增量 = %v", reasoning)
	}
	if len(toolDeltas) != 2 || toolDeltas[0].ID != "toolu_1" || toolDeltas[0].Name != "lookup" ||
		toolDeltas[1].ArgumentsFragment != `{"q":` {
		t.Fatalf("工具增量 = %+v", toolDeltas)
	}
	if len(usageEvents) != 2 {
		t.Fatalf("usage 事件 = %d, want 2（message_start + message_delta）", len(usageEvents))
	}
	lastUsage := usageEvents[len(usageEvents)-1]
	if lastUsage.Prompt != 10 || lastUsage.Completion != 5 || lastUsage.CacheRead != 3 || lastUsage.Total != 15 {
		t.Fatalf("流式累计 usage = %+v", lastUsage)
	}
	if lastUsage.PromptIncludesCache || !lastUsage.ReasoningIncludedInCompletion {
		t.Fatalf("流式关系布尔错误：%+v", lastUsage)
	}
	if finishCount != 1 {
		t.Fatalf("finish 事件 = %d, want 1（message_stop 不得重复）", finishCount)
	}
	if pingCount != 1 {
		t.Fatalf("ping 事件 = %d, want 1", pingCount)
	}
}

func TestAnthropicStreamErrorEvent(t *testing.T) {
	adapter := NewAnthropicAdapter(contract.ChannelTypeAnthropic)
	sse := "event: error\n" + `data: {"type":"error","error":{"type":"overloaded_error","message":"busy"}}` + "\n\n"
	events := collectStreamEvents(adapter.(contract.StreamAdapter).StreamEvents(anthropicContext("k"), bytes.NewBufferString(sse)))
	if len(events) != 1 || events[0].Kind != contract.EventError || len(events[0].Raw) == 0 {
		t.Fatalf("error 事件映射错误：%+v", events)
	}
}

// TestAnthropicStreamThinkingSignatureAndRedacted 覆盖复审 P1：signature_delta
// 与 redacted_thinking 内容不得在流式解析中丢失。
func TestAnthropicStreamThinkingSignatureAndRedacted(t *testing.T) {
	adapter := NewAnthropicAdapter(contract.ChannelTypeAnthropic)
	sse := strings.Join([]string{
		"event: content_block_start",
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"redacted_thinking","data":"enc-payload"}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-value"}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
	}, "\n")
	events := collectStreamEvents(adapter.(contract.StreamAdapter).StreamEvents(anthropicContext("k"), bytes.NewBufferString(sse)))
	var sawRedacted, sawSignature bool
	for _, event := range events {
		raw := string(event.Raw)
		if event.Kind == contract.EventRaw && strings.Contains(raw, "redacted_thinking") && strings.Contains(raw, "enc-payload") {
			sawRedacted = true
		}
		if event.Kind == contract.EventRaw && strings.Contains(raw, "signature_delta") && strings.Contains(raw, "sig-value") {
			sawSignature = true
		}
	}
	if !sawRedacted || !sawSignature {
		t.Fatalf("流式思考签名/加密块必须保留（redacted=%v signature=%v）：%+v", sawRedacted, sawSignature, events)
	}
}

// TestAnthropicClassifyMatchesOpenAI 锁定两份状态码分类实现的一致性
// （复审 #8：不复用同一函数，但必须同口径）。
func TestAnthropicClassifyMatchesOpenAI(t *testing.T) {
	openAI := &OpenAI{}
	anthropic := NewAnthropicAdapter(contract.ChannelTypeAnthropic)
	gemini := NewGeminiAdapter(contract.ChannelTypeGemini)
	for status := 0; status <= 600; status++ {
		want := openAI.ClassifyError(status, nil)
		if got := anthropic.ClassifyError(status, nil); got != want {
			t.Fatalf("status %d: Anthropic 分类 %v != OpenAI %v", status, got, want)
		}
		if got := gemini.ClassifyError(status, nil); got != want {
			t.Fatalf("status %d: Gemini 分类 %v != OpenAI %v", status, got, want)
		}
	}
}

func TestAnthropicRegistryDispatch(t *testing.T) {
	for _, channelType := range []contract.ChannelType{contract.ChannelTypeAnthropic, contract.ChannelTypeMoonshot} {
		adapter, err := Dispatch(channelType)
		if err != nil {
			t.Fatalf("Dispatch(%q): %v", channelType, err)
		}
		anthropic, ok := adapter.(*Anthropic)
		if !ok {
			t.Fatalf("Dispatch(%q) = %T, want *Anthropic（R-13 原生适配器）", channelType, adapter)
		}
		if anthropic.Type() != channelType {
			t.Fatalf("Type() = %q, want %q", anthropic.Type(), channelType)
		}
		if _, ok := adapter.(contract.StreamAdapter); !ok {
			t.Fatalf("Dispatch(%q) 必须实现 StreamAdapter", channelType)
		}
	}
	ctor, ok := Lookup(contract.ChannelTypeAnthropic)
	if !ok || ctor == nil {
		t.Fatal("Lookup(anthropic) 必须命中")
	}
	caps := ctor().Caps()
	if !hasRole(caps.Roles, contract.RoleChat) {
		t.Fatalf("anthropic 必须有 RoleChat：%+v", caps.Roles)
	}
	for _, endpoint := range []contract.Endpoint{contract.EndpointChat, contract.EndpointMessages} {
		if _, ok := caps.Endpoints[endpoint]; !ok {
			t.Fatalf("anthropic Caps 缺少端点 %q", endpoint)
		}
	}
	if caps.StreamUsage {
		t.Fatal("Anthropic 没有 include_usage 请求项，StreamUsage 必须为 false")
	}
	if !caps.StreamAggregatesUsage {
		t.Fatal("Anthropic usage 跨 message_start/message_delta，StreamAggregatesUsage 必须为 true")
	}
}

// jsonUnmarshal 是测试内的小包装，避免每处重复导入 encoding/json。
func jsonUnmarshal(data []byte, out any) error {
	return json.Unmarshal(data, out)
}

func hasRole(roles []contract.ChannelRole, want contract.ChannelRole) bool {
	for _, role := range roles {
		if role == want {
			return true
		}
	}
	return false
}

// TestAnthropicBuildRequestForwardsAllowlistedBeta 锁定 R-W18-05 的出站头：
// 只转发白名单内的 anthropic-beta 值；空值不发送；Authorization 永不出现。
func TestAnthropicBuildRequestForwardsAllowlistedBeta(t *testing.T) {
	adapter := NewAnthropicAdapter(contract.ChannelTypeAnthropic)
	req := &contract.Request{
		Model: "claude-sonnet-4-5",
		Messages: []contract.Message{{
			Role:    contract.RoleUser,
			Content: []contract.ContentPart{{Kind: contract.PartText, Text: "hi"}},
		}},
		AnthropicBeta: "interleaved-thinking-2025-05-14, not-a-real-beta",
	}
	httpReq, err := adapter.BuildRequest(anthropicContext("sk-ant-secret"), req)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if got := httpReq.Header.Get("anthropic-beta"); got != "interleaved-thinking-2025-05-14" {
		t.Fatalf("anthropic-beta = %q, want allowlisted value only", got)
	}
	if got := httpReq.Header.Get("Authorization"); got != "" {
		t.Fatalf("Authorization 不得转发：%q", got)
	}

	// 全被白名单丢弃 / 空值：不得发送该头（空头与「无 beta」语义不同）。
	req.AnthropicBeta = "definitely-not-a-beta"
	httpReq, err = adapter.BuildRequest(anthropicContext("sk-ant-secret"), req)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if _, ok := httpReq.Header["Anthropic-Beta"]; ok {
		t.Fatalf("非白名单值必须丢弃，实际头=%v", httpReq.Header["Anthropic-Beta"])
	}
}

// TestAnthropicBetaAllowlistIncludesToolChangeBetas 锁定会话中段工具变更所需
// 的两个 beta：白名单漏掉它们时，同协议 Anthropic 上游会因缺少 beta 拒绝
// tool_addition / tool_removal 块。
func TestAnthropicBetaAllowlistIncludesToolChangeBetas(t *testing.T) {
	const betas = "mid-conversation-tool-changes-2026-07-01, inline-tools-2026-09-15"
	if got := FilterAnthropicBeta(betas); got != betas {
		t.Fatalf("FilterAnthropicBeta(%q) = %q, want both kept", betas, got)
	}
}

// TestAnthropicBuildCountTokensRequest 锁定 count_tokens 的路径、模型映射与
// 未建模字段透传（精确计数必须基于客户端真实内容）。
func TestAnthropicBuildCountTokensRequest(t *testing.T) {
	adapter := NewAnthropicAdapter(contract.ChannelTypeAnthropic).(*Anthropic)
	rc := anthropicContext("sk-ant-secret")
	rc.ChannelID = 14
	rc.UpstreamModel = "claude-upstream"
	body := []byte(`{"model":"claude-client","system":"keep","messages":[{"role":"user","content":"hi"}]}`)

	httpReq, err := adapter.BuildCountTokensRequest(rc, body)
	if err != nil {
		t.Fatalf("BuildCountTokensRequest: %v", err)
	}
	if httpReq.URL.String() != "https://api.anthropic.com/v1/messages/count_tokens" {
		t.Fatalf("URL = %q", httpReq.URL.String())
	}
	if got := httpReq.Header.Get("x-api-key"); got != "sk-ant-secret" {
		t.Fatalf("x-api-key = %q", got)
	}
	if got := httpReq.Header.Get("anthropic-version"); got != anthropicVersion {
		t.Fatalf("anthropic-version = %q", got)
	}
	raw, _ := io.ReadAll(httpReq.Body)
	var built map[string]any
	if err := json.Unmarshal(raw, &built); err != nil {
		t.Fatalf("body: %v", err)
	}
	if built["model"] != "claude-upstream" {
		t.Fatalf("model = %v, want upstream mapping target", built["model"])
	}
	if built["system"] != "keep" {
		t.Fatalf("未建模字段被丢失：%v", built["system"])
	}
}

// TestAnthropicBuildCountTokensMoonshotPrefix 证明 Moonshot 的 /anthropic
// 前缀在 count_tokens 上同样生效，不会退化成官方基址。
func TestAnthropicBuildCountTokensMoonshotPrefix(t *testing.T) {
	adapter := NewAnthropicAdapter(contract.ChannelTypeMoonshot).(*Anthropic)
	rc := &contract.RelayContext{
		ChannelID:   25,
		ChannelType: contract.ChannelTypeMoonshot,
		Credential:  "sk-moonshot",
	}
	httpReq, err := adapter.BuildCountTokensRequest(rc, []byte(`{"model":"claude-test","messages":[]}`))
	if err != nil {
		t.Fatalf("BuildCountTokensRequest: %v", err)
	}
	if httpReq.URL.String() != "https://api.moonshot.cn/anthropic/v1/messages/count_tokens" {
		t.Fatalf("URL = %q", httpReq.URL.String())
	}
}

func TestAnthropicParseCountTokensResponse(t *testing.T) {
	adapter := NewAnthropicAdapter(contract.ChannelTypeAnthropic).(*Anthropic)
	ok := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"input_tokens":42}`))}
	tokens, err := adapter.ParseCountTokensResponse(ok)
	if err != nil || tokens != 42 {
		t.Fatalf("ParseCountTokensResponse = (%d, %v), want (42, nil)", tokens, err)
	}

	bad := []string{`{}`, `{"input_tokens":-1}`, `not json`}
	for _, body := range bad {
		resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}
		if _, err := adapter.ParseCountTokensResponse(resp); err == nil {
			t.Fatalf("body %q 应被拒绝", body)
		}
	}
	errResp := &http.Response{StatusCode: http.StatusTooManyRequests,
		Body: io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`))}
	if _, err := adapter.ParseCountTokensResponse(errResp); err == nil {
		t.Fatal("非 2xx 必须返回错误")
	}
}
