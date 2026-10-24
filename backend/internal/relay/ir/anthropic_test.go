package ir

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"newtg/gateway/internal/contract"
)

// anthropicFourBlockRequest 覆盖 R-04 的四类块：text / image / tool_use / tool_result，
// 且 tool_result 内含图片（验证 Meta["content_blocks"] 非文本保留）。
const anthropicFourBlockRequest = `{
  "model": "claude-sonnet-4-5",
  "max_tokens": 256,
  "stream": true,
  "system": "be precise",
  "messages": [
    {"role": "user", "content": [
      {"type": "text", "text": "look at this"},
      {"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "aGVsbG8="}}
    ]},
    {"role": "assistant", "content": [
      {"type": "text", "text": "let me check"},
      {"type": "tool_use", "id": "toolu_1", "name": "lookup", "input": {"q": "x"}}
    ]},
    {"role": "user", "content": [
      {"type": "tool_result", "tool_use_id": "toolu_1", "content": [
        {"type": "text", "text": "found it"},
        {"type": "image", "source": {"type": "base64", "media_type": "image/jpeg", "data": "d29ybGQ="}}
      ]}
    ]}
  ],
  "tools": [{"name": "lookup", "description": "d", "input_schema": {"type": "object"}}],
  "tool_choice": {"type": "tool", "name": "lookup"},
  "temperature": 0.2,
  "top_p": 0.9,
  "stop_sequences": ["END"]
}`

func TestAnthropicRequestFourBlockTypesRoundTrip(t *testing.T) {
	req, err := ParseAnthropicRequest([]byte(anthropicFourBlockRequest))
	if err != nil {
		t.Fatalf("ParseAnthropicRequest: %v", err)
	}
	if req.RawFormat != FormatAnthropicMessages {
		t.Fatalf("RawFormat = %q, want %q", req.RawFormat, FormatAnthropicMessages)
	}
	if string(req.Raw) != anthropicFourBlockRequest {
		t.Fatal("Raw 必须原样保存入站请求体")
	}
	if req.Model != "claude-sonnet-4-5" || !req.Stream || req.MaxTokens == nil || *req.MaxTokens != 256 {
		t.Fatalf("model/stream/max_tokens = %q/%v/%v", req.Model, req.Stream, req.MaxTokens)
	}
	if len(req.Messages) != 4 {
		t.Fatalf("messages = %d, want 4（system + user + assistant + user）", len(req.Messages))
	}
	if req.Messages[0].Role != contract.RoleSystem || req.Messages[0].Content[0].Text != "be precise" {
		t.Fatalf("system 消息映射错误：%+v", req.Messages[0])
	}
	if got := req.Messages[1].Content; len(got) != 2 || got[0].Kind != contract.PartText ||
		got[1].Kind != contract.PartImageB64 || string(got[1].Payload) != "hello" {
		t.Fatalf("user 的 text+image 映射错误：%+v", got)
	}
	assistant := req.Messages[2].Content
	if len(assistant) != 2 || assistant[1].Kind != contract.PartToolCall {
		t.Fatalf("assistant 的 text+tool_use 映射错误：%+v", assistant)
	}
	if metaString(assistant[1].Meta, "id") != "toolu_1" || metaString(assistant[1].Meta, "name") != "lookup" {
		t.Fatalf("tool_use id/name = %+v", assistant[1].Meta)
	}
	var input map[string]any
	if err := json.Unmarshal([]byte(assistant[1].Text), &input); err != nil || input["q"] != "x" {
		t.Fatalf("tool_use.input 未保真：%q err=%v", assistant[1].Text, err)
	}
	result := req.Messages[3].Content[0]
	if result.Kind != contract.PartToolResult || result.Text != "found it" ||
		metaString(result.Meta, "tool_call_id") != "toolu_1" {
		t.Fatalf("tool_result 映射错误：%+v", result)
	}
	blocks, ok := result.Meta["content_blocks"].([]any)
	if !ok || len(blocks) != 2 {
		t.Fatalf("tool_result 的原始块（text+image）必须保留在 content_blocks：%+v", result.Meta)
	}
	if imageBlock, _ := blocks[1].(map[string]any); imageBlock["type"] != "image" {
		t.Fatalf("content_blocks[1] 应为图片块：%+v", blocks[1])
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != "lookup" {
		t.Fatalf("tools 映射错误：%+v", req.Tools)
	}
	if req.ToolChoice.Mode != contract.ToolChoiceSpecific || req.ToolChoice.Name != "lookup" {
		t.Fatalf("tool_choice 映射错误：%+v", req.ToolChoice)
	}
	if len(req.Stop) != 1 || req.Stop[0] != "END" {
		t.Fatalf("stop_sequences 映射错误：%+v", req.Stop)
	}

	// 回写后再解析一次，保证 Go IR 可无损往返四类块。
	encoded, err := BuildAnthropicRequestBody(req)
	if err != nil {
		t.Fatalf("BuildAnthropicRequestBody: %v", err)
	}
	var built map[string]any
	if err := json.Unmarshal(encoded, &built); err != nil {
		t.Fatalf("构造体不是 JSON: %v", err)
	}
	if built["system"] != "be precise" {
		t.Fatalf("system 回写错误：%v", built["system"])
	}
	messages, _ := built["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("回写 messages = %d, want 3", len(messages))
	}
	second, _ := messages[1].(map[string]any)
	blocksOut, _ := second["content"].([]any)
	toolUse, _ := blocksOut[1].(map[string]any)
	if toolUse["type"] != "tool_use" || toolUse["id"] != "toolu_1" {
		t.Fatalf("tool_use 回写错误：%v", toolUse)
	}
	if _, ok := toolUse["input"].(map[string]any); !ok {
		t.Fatalf("tool_use.input 必须以对象回写，得到 %T", toolUse["input"])
	}
	third, _ := messages[2].(map[string]any)
	resultOut, _ := third["content"].([]any)
	resultBlock, _ := resultOut[0].(map[string]any)
	if resultBlock["type"] != "tool_result" || resultBlock["tool_use_id"] != "toolu_1" {
		t.Fatalf("tool_result 回写错误：%v", resultBlock)
	}
	if contentBlocks, ok := resultBlock["content"].([]any); !ok || len(contentBlocks) != 2 {
		t.Fatalf("tool_result.content 必须保留 text+image 两块：%v", resultBlock["content"])
	}

	// 二次解析确认往返稳定。
	again, err := ParseAnthropicRequest(encoded)
	if err != nil {
		t.Fatalf("round-trip ParseAnthropicRequest: %v", err)
	}
	if len(again.Messages) != 4 || again.Messages[3].Content[0].Kind != contract.PartToolResult {
		t.Fatalf("往返后消息结构发生变化：%+v", again.Messages)
	}
}

func TestAnthropicUnknownBlockTypeRejected(t *testing.T) {
	cases := map[string]string{
		"document":        `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":[{"type":"document","source":{"type":"base64","data":"x"}}]}]}`,
		"server_tool_use": `{"model":"m","max_tokens":1,"messages":[{"role":"assistant","content":[{"type":"server_tool_use","id":"s","name":"web"}]}]}`,
		"system图片":        `{"model":"m","max_tokens":1,"system":[{"type":"image","source":{"type":"url","url":"https://x"}}],"messages":[]}`,
	}
	for name, body := range cases {
		_, err := ParseAnthropicRequest([]byte(body))
		if err == nil {
			t.Fatalf("%s: 未知块类型必须报错，不能静默丢弃", name)
		}
		if !errors.Is(err, ErrParamInvalid) {
			t.Fatalf("%s: 错误必须是 ErrParamInvalid，得到 %v", name, err)
		}
	}
}

func TestAnthropicUnknownImageSourceRejected(t *testing.T) {
	body := `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"file","file_id":"f1"}}]}]}`
	if _, err := ParseAnthropicRequest([]byte(body)); !errors.Is(err, ErrParamInvalid) {
		t.Fatalf("未知 image.source.type 必须报 ErrParamInvalid，得到 %v", err)
	}
}

func TestAnthropicToolChoiceModes(t *testing.T) {
	cases := []struct {
		body string
		want contract.ToolChoice
	}{
		{`{"type":"auto"}`, contract.ToolChoice{Mode: contract.ToolChoiceAuto}},
		{`{"type":"none"}`, contract.ToolChoice{Mode: contract.ToolChoiceNone}},
		{`{"type":"any"}`, contract.ToolChoice{Mode: contract.ToolChoiceRequired}},
		{`{"type":"tool","name":"f"}`, contract.ToolChoice{Mode: contract.ToolChoiceSpecific, Name: "f"}},
	}
	for _, tc := range cases {
		got, err := parseAnthropicToolChoice([]byte(tc.body))
		if err != nil {
			t.Fatalf("%s: %v", tc.body, err)
		}
		if got != tc.want {
			t.Fatalf("%s: got %+v, want %+v", tc.body, got, tc.want)
		}
	}
	if _, err := parseAnthropicToolChoice([]byte(`{"type":"tool"}`)); !errors.Is(err, ErrParamInvalid) {
		t.Fatal("tool_choice tool 缺 name 必须报错")
	}
}

func TestAnthropicBuildDefaultsMaxTokens(t *testing.T) {
	req := &contract.Request{
		Model:    "claude-sonnet-4-5",
		Messages: []contract.Message{{Role: contract.RoleUser, Content: []contract.ContentPart{{Kind: contract.PartText, Text: "hi"}}}},
	}
	encoded, err := BuildAnthropicRequestBody(req)
	if err != nil {
		t.Fatalf("BuildAnthropicRequestBody: %v", err)
	}
	var built map[string]any
	if err := json.Unmarshal(encoded, &built); err != nil {
		t.Fatal(err)
	}
	if got, _ := built["max_tokens"].(float64); got != anthropicDefaultMaxTokens {
		t.Fatalf("max_tokens = %v, want %d（Anthropic 必填）", built["max_tokens"], anthropicDefaultMaxTokens)
	}
}

func TestAnthropicUsageParse(t *testing.T) {
	usage := ParseAnthropicUsage([]byte(`{
	  "input_tokens": 10,
	  "output_tokens": 7,
	  "cache_creation_input_tokens": 5,
	  "cache_read_input_tokens": 3,
	  "cache_creation": {"ephemeral_5m_input_tokens": 2, "ephemeral_1h_input_tokens": 3}
	}`))
	if usage.Prompt != 10 || usage.Completion != 7 || usage.Total != 17 {
		t.Fatalf("prompt/completion/total = %d/%d/%d", usage.Prompt, usage.Completion, usage.Total)
	}
	if usage.CacheRead != 3 || usage.CacheWrite != 5 {
		t.Fatalf("cache_read/write = %d/%d", usage.CacheRead, usage.CacheWrite)
	}
	if usage.PromptIncludesCache {
		t.Fatal("Claude 语义：Prompt 不含缓存，PromptIncludesCache 必须为 false")
	}
	if !usage.ReasoningIncludedInCompletion {
		t.Fatal("Claude 语义：thinking 计入 output_tokens，ReasoningIncludedInCompletion 必须为 true")
	}
	if usage.Source != contract.UsageUpstreamExact {
		t.Fatalf("Source = %v, want upstream_exact", usage.Source)
	}
	for _, key := range []string{"input_tokens", "output_tokens", "cache_creation_input_tokens", "cache_read_input_tokens"} {
		if _, ok := usage.Raw[key]; !ok {
			t.Fatalf("Raw 必须保留 Anthropic 原键 %q：%v", key, usage.Raw)
		}
	}
	if usage.PromptDetail.Text != 10 || usage.PromptDetail.CacheRead != 3 || usage.PromptDetail.CacheWrite != 5 {
		t.Fatalf("PromptDetail = %+v", usage.PromptDetail)
	}
	// cache_creation_input_tokens 缺席时才用子字段求和兜底。
	fallback := ParseAnthropicUsage([]byte(`{"input_tokens":1,"output_tokens":2,"cache_creation":{"ephemeral_5m_input_tokens":4,"ephemeral_1h_input_tokens":6}}`))
	if fallback.CacheWrite != 10 {
		t.Fatalf("cache_creation 子字段求和 = %d, want 10", fallback.CacheWrite)
	}
}

func TestAnthropicUsageEmptyIsEstimated(t *testing.T) {
	usage := ParseAnthropicUsage(nil)
	if usage.Source != contract.UsageEstimated {
		t.Fatalf("空 usage 必须标记 estimated，得到 %v", usage.Source)
	}
	if usage.PromptIncludesCache || !usage.ReasoningIncludedInCompletion {
		t.Fatal("关系布尔是协议属性，空 usage 也必须显式赋值")
	}
}

// TestAnthropicUsageUnknownObjectIsEstimated 是终验 M5 的回归。
func TestAnthropicUsageUnknownObjectIsEstimated(t *testing.T) {
	usage := ParseAnthropicUsage([]byte(`{"foo":1}`))
	if usage.Source != contract.UsageEstimated {
		t.Fatalf("未知键 usage 应标记 estimated，得到 %v", usage.Source)
	}
	// null 键同样不算事实（与 T3 claudeUsageKeysPresent 同口径）。
	nullUsage := ParseAnthropicUsage([]byte(`{"input_tokens":null}`))
	if nullUsage.Source != contract.UsageEstimated {
		t.Fatalf("null 用量键应标记 estimated，得到 %v", nullUsage.Source)
	}
	// 有一个非 null 的已识别键时仍是 exact（零值键不擦除事实）。
	mixed := ParseAnthropicUsage([]byte(`{"input_tokens":null,"output_tokens":5}`))
	if mixed.Source != contract.UsageUpstreamExact || mixed.Completion != 5 {
		t.Fatalf("部分 null 键解析错误：%+v", mixed)
	}
}

func TestAnthropicResponseParse(t *testing.T) {
	body := []byte(`{
	  "id": "msg_1",
	  "type": "message",
	  "role": "assistant",
	  "model": "claude-sonnet-4-5",
	  "content": [
	    {"type": "text", "text": "hi"},
	    {"type": "tool_use", "id": "toolu_9", "name": "lookup", "input": {"q": "x"}}
	  ],
	  "stop_reason": "tool_use",
	  "usage": {"input_tokens": 4, "output_tokens": 6, "cache_read_input_tokens": 2}
	}`)
	resp, usage, err := ParseAnthropicResponse(body)
	if err != nil {
		t.Fatalf("ParseAnthropicResponse: %v", err)
	}
	if resp.ID != "msg_1" || resp.Model != "claude-sonnet-4-5" {
		t.Fatalf("id/model = %q/%q", resp.ID, resp.Model)
	}
	if resp.FinishReason != "tool_calls" || len(resp.Choices) != 1 {
		t.Fatalf("finish_reason/choices = %q/%d", resp.FinishReason, len(resp.Choices))
	}
	content := resp.Choices[0].Message.Content
	if len(content) != 2 || content[1].Kind != contract.PartToolCall || metaString(content[1].Meta, "name") != "lookup" {
		t.Fatalf("content = %+v", content)
	}
	if resp.Choices[0].Message.Extensions["finish_reason"] != "tool_use" {
		t.Fatalf("原始 stop_reason 必须保留在 Extensions：%+v", resp.Choices[0].Message.Extensions)
	}
	if usage.Prompt != 4 || usage.Completion != 6 || usage.CacheRead != 2 {
		t.Fatalf("usage = %+v", usage)
	}
	if string(resp.Raw) != string(body) {
		t.Fatal("Response.Raw 必须保留上游原始响应体")
	}
}

// TestAnthropicExtendedThinkingResponse 是复审 P1 的回归：thinking /
// redacted_thinking 是标准块，有效 200 响应绝不能变成 502。
func TestAnthropicExtendedThinkingResponse(t *testing.T) {
	body := []byte(`{
	  "id":"m","model":"claude-sonnet-4-5",
	  "content":[
	    {"type":"thinking","thinking":"let me think","signature":"sig-1"},
	    {"type":"redacted_thinking","data":"encrypted"},
	    {"type":"text","text":"answer"}
	  ],
	  "stop_reason":"end_turn",
	  "usage":{"input_tokens":1,"output_tokens":2}
	}`)
	resp, _, err := ParseAnthropicResponse(body)
	if err != nil {
		t.Fatalf("扩展思考响应不得报错：%v", err)
	}
	content := resp.Choices[0].Message.Content
	if len(content) != 3 {
		t.Fatalf("content = %+v", content)
	}
	if content[0].Kind != contract.PartText || content[0].Text != "let me think" ||
		content[0].Meta["thought"] != true || content[0].Meta["signature"] != "sig-1" {
		t.Fatalf("thinking 映射错误：%+v", content[0])
	}
	if content[1].Kind != contract.PartText || content[1].Meta["redacted_thinking"] != "encrypted" {
		t.Fatalf("redacted_thinking 映射错误：%+v", content[1])
	}
	if content[2].Text != "answer" || content[2].Meta["thought"] == true {
		t.Fatalf("正文映射错误：%+v", content[2])
	}

	// 同一 IR 编码回 Anthropic 时必须恢复原块类型，不能当成正文 text。
	req := &contract.Request{Model: "m", MaxTokens: intPtr(16), Messages: []contract.Message{
		{Role: contract.RoleAssistant, Content: content},
	}}
	encoded, err := BuildAnthropicRequestBody(req)
	if err != nil {
		t.Fatalf("BuildAnthropicRequestBody: %v", err)
	}
	var built map[string]any
	if err := json.Unmarshal(encoded, &built); err != nil {
		t.Fatal(err)
	}
	messages, _ := built["messages"].([]any)
	msg, _ := messages[0].(map[string]any)
	blocks, _ := msg["content"].([]any)
	first, _ := blocks[0].(map[string]any)
	second, _ := blocks[1].(map[string]any)
	if first["type"] != "thinking" || first["thinking"] != "let me think" || first["signature"] != "sig-1" {
		t.Fatalf("thinking 回写错误：%v", first)
	}
	if second["type"] != "redacted_thinking" || second["data"] != "encrypted" {
		t.Fatalf("redacted_thinking 回写错误：%v", second)
	}
}

func TestAnthropicResponseUnknownBlockPreserved(t *testing.T) {
	// 真正未知的（未来新增）块：响应解析不得 502，原始块进 Extensions。
	body := []byte(`{"id":"m","content":[{"type":"future_block","payload":{"x":1}},{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`)
	resp, _, err := ParseAnthropicResponse(body)
	if err != nil {
		t.Fatalf("未知响应块必须保留而不是报错：%v", err)
	}
	message := resp.Choices[0].Message
	if len(message.Content) != 1 || message.Content[0].Text != "ok" {
		t.Fatalf("已建模内容映射错误：%+v", message.Content)
	}
	unmapped, ok := message.Extensions["unmapped_blocks"].([]any)
	if !ok || len(unmapped) != 1 {
		t.Fatalf("未知块必须保留在 Extensions：%+v", message.Extensions)
	}
}

func intPtr(value int) *int { return &value }

func TestAnthropicBuildPassthroughKeepsUnmodelledFields(t *testing.T) {
	body := `{"model":"claude","max_tokens":8,"system":"s","messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":1024},"top_k":5}`
	req, err := ParseAnthropicRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := BuildAnthropicRequestBody(req)
	if err != nil {
		t.Fatal(err)
	}
	var built map[string]any
	if err := json.Unmarshal(encoded, &built); err != nil {
		t.Fatal(err)
	}
	if _, ok := built["thinking"]; !ok {
		t.Fatal("同协议透传必须保留未建模字段 thinking")
	}
	if _, ok := built["top_k"]; !ok {
		t.Fatal("同协议透传必须保留未建模字段 top_k")
	}
}

func TestAnthropicSystemCacheControlPreserved(t *testing.T) {
	body := `{"model":"m","max_tokens":8,"system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"hi"}]}`
	req, err := ParseAnthropicRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := BuildAnthropicRequestBody(req)
	if err != nil {
		t.Fatal(err)
	}
	var built map[string]any
	if err := json.Unmarshal(encoded, &built); err != nil {
		t.Fatal(err)
	}
	blocks, ok := built["system"].([]any)
	if !ok || len(blocks) != 1 {
		t.Fatalf("带 cache_control 的 system 必须回写为块数组：%v", built["system"])
	}
	block, _ := blocks[0].(map[string]any)
	if block["text"] != "s" {
		t.Fatalf("system 文本丢失：%v", block)
	}
	if _, ok := block["cache_control"].(map[string]any); !ok {
		t.Fatalf("system 的 cache_control 丢失：%v", block)
	}
}

// TestAnthropicToolFieldsPreserved 覆盖复审 P2：工具定义的 type/cache_control
// 与 tool_choice.disable_parallel_tool_use 必须经 IR 往返存活。
func TestAnthropicToolFieldsPreserved(t *testing.T) {
	body := `{
	  "model":"m","max_tokens":8,
	  "messages":[{"role":"user","content":"hi"}],
	  "tools":[{"type":"custom","name":"f","description":"d","input_schema":{"type":"object"},
	            "cache_control":{"type":"ephemeral"}}],
	  "tool_choice":{"type":"auto","disable_parallel_tool_use":true}
	}`
	req, err := ParseAnthropicRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := BuildAnthropicRequestBody(req)
	if err != nil {
		t.Fatal(err)
	}
	var built map[string]any
	if err := json.Unmarshal(encoded, &built); err != nil {
		t.Fatal(err)
	}
	tools, _ := built["tools"].([]any)
	tool, _ := tools[0].(map[string]any)
	if tool["type"] != "custom" {
		t.Fatalf("工具 type 丢失：%v", tool)
	}
	if _, ok := tool["cache_control"].(map[string]any); !ok {
		t.Fatalf("工具 cache_control 丢失：%v", tool)
	}
	if tool["name"] != "f" || tool["input_schema"] == nil {
		t.Fatalf("已建模工具字段错误：%v", tool)
	}
	choice, _ := built["tool_choice"].(map[string]any)
	if choice["type"] != "auto" || choice["disable_parallel_tool_use"] != true {
		t.Fatalf("tool_choice 扩展字段丢失：%v", choice)
	}
	if _, ok := choice["name"]; ok {
		t.Fatalf("auto 模式不得残留 name：%v", choice)
	}
}

// TestAnthropicThinkingExcludedFromOpenAIContent 是终验 H1 的回归：
// 非流式 thinking 必须走 reasoning_content，不能混进 visible content。
func TestAnthropicThinkingExcludedFromOpenAIContent(t *testing.T) {
	body := []byte(`{"id":"m","model":"claude","content":[
	  {"type":"thinking","thinking":"let me think","signature":"sig"},
	  {"type":"text","text":"answer"}
	],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2}}`)
	resp, _, err := ParseAnthropicResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := FromIR(resp)
	if err != nil {
		t.Fatal(err)
	}
	var built map[string]any
	if err := json.Unmarshal(encoded.Raw, &built); err != nil {
		t.Fatal(err)
	}
	choices, _ := built["choices"].([]any)
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	if message["content"] != "answer" {
		t.Fatalf("content 不得包含 thinking：%v", message["content"])
	}
	if message["reasoning_content"] != "let me think" {
		t.Fatalf("thinking 必须走 reasoning_content：%v", message)
	}
}

func TestAnthropicBase64RoundTripStable(t *testing.T) {
	payload := []byte("png-bytes")
	req := &contract.Request{
		Model: "m",
		Messages: []contract.Message{{Role: contract.RoleUser, Content: []contract.ContentPart{{
			Kind:    contract.PartImageB64,
			Payload: payload,
			Meta:    map[string]any{"mime_type": "image/webp"},
		}}}},
	}
	encoded, err := BuildAnthropicRequestBody(req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), base64.StdEncoding.EncodeToString(payload)) {
		t.Fatalf("图片负载必须按 base64 写入：%s", encoded)
	}
}

// ============================================================================
// W18-B · R-W18-03：IR → Anthropic 响应编码与 SSE 渲染
// ============================================================================

func decodeAnthropicJSON(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("响应不是 JSON 对象: %v; body=%s", err, body)
	}
	return obj
}

func contentBlocks(t *testing.T, obj map[string]any) []any {
	t.Helper()
	blocks, ok := obj["content"].([]any)
	if !ok {
		t.Fatalf("content 不是数组: %v", obj["content"])
	}
	return blocks
}

// TestBuildAnthropicResponseTextAndUsage：OpenAI 上游的 IR 响应回写为 Claude
// 消息，且 usage 按关系布尔换算（OpenAI prompt 含缓存读，Anthropic input 不含）。
func TestBuildAnthropicResponseTextAndUsage(t *testing.T) {
	raw := `{"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,` +
		`"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":10,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":4}}}`
	resp, usage, err := ParseOpenAIResponse([]byte(raw))
	if err != nil {
		t.Fatalf("ParseOpenAIResponse: %v", err)
	}
	body, err := BuildAnthropicResponse(resp, usage)
	if err != nil {
		t.Fatalf("BuildAnthropicResponse: %v", err)
	}
	obj := decodeAnthropicJSON(t, body)
	if obj["type"] != "message" || obj["role"] != "assistant" {
		t.Fatalf("type/role = %v/%v, want message/assistant", obj["type"], obj["role"])
	}
	if obj["id"] != "chatcmpl-1" || obj["model"] != "gpt-4o" {
		t.Fatalf("id/model = %v/%v", obj["id"], obj["model"])
	}
	if obj["stop_reason"] != "end_turn" {
		t.Fatalf("stop_reason = %v, want end_turn", obj["stop_reason"])
	}
	if obj["stop_sequence"] != nil {
		t.Fatalf("stop_sequence = %v, want null", obj["stop_sequence"])
	}
	blocks := contentBlocks(t, obj)
	if len(blocks) != 1 {
		t.Fatalf("content blocks = %d, want 1", len(blocks))
	}
	block := blocks[0].(map[string]any)
	if block["type"] != "text" || block["text"] != "hello" {
		t.Fatalf("block = %v, want text hello", block)
	}
	usageObj := obj["usage"].(map[string]any)
	// input_tokens = prompt(10) - cached(4)；output_tokens = completion(5)。
	if usageObj["input_tokens"] != float64(6) || usageObj["output_tokens"] != float64(5) {
		t.Fatalf("usage = %v, want input 6 / output 5", usageObj)
	}
	if usageObj["cache_read_input_tokens"] != float64(4) {
		t.Fatalf("cache_read_input_tokens = %v, want 4", usageObj["cache_read_input_tokens"])
	}
}

// TestAnthropicStopReasonMapping 锁定 IR 归一值 → Anthropic stop_reason 的
// 互逆映射（R-W18-03；变异「把 stop_reason 映射改错」必须让它红）。
func TestAnthropicStopReasonMapping(t *testing.T) {
	cases := map[string]string{
		"stop":           "end_turn",
		"length":         "max_tokens",
		"tool_calls":     "tool_use",
		"tool_use":       "tool_use",
		"content_filter": "refusal",
		"":               "",
		"pause_turn":     "pause_turn", // 未知值保留小写原文，不猜成 end_turn
	}
	for in, want := range cases {
		if got := AnthropicStopReason(in); got != want {
			t.Fatalf("AnthropicStopReason(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestBuildAnthropicResponseToolUseAndThinking：thinking / tool_use 按 IR 已有
// 载体回写；stop_reason 由 tool_calls 映射为 tool_use。
func TestBuildAnthropicResponseToolUseAndThinking(t *testing.T) {
	resp := &contract.Response{
		ID: "msg_x", Model: "upstream-model", FinishReason: "tool_calls",
		Choices: []contract.Choice{{Index: 0, FinishReason: "tool_calls", Message: contract.Message{
			Role: contract.RoleAssistant,
			Content: []contract.ContentPart{
				{Kind: contract.PartText, Text: "plan", Meta: map[string]any{
					"thought": true, "anthropic_thinking": true, "signature": "sig-1",
				}},
				{Kind: contract.PartText, Text: "answer"},
				{Kind: contract.PartToolCall, Text: `{"q":"x"}`, Meta: map[string]any{
					"id": "toolu_1", "name": "lookup",
				}},
			},
		}}},
	}
	body, err := BuildAnthropicResponse(resp, contract.Usage{Prompt: 1, Completion: 2})
	if err != nil {
		t.Fatalf("BuildAnthropicResponse: %v", err)
	}
	obj := decodeAnthropicJSON(t, body)
	if obj["stop_reason"] != "tool_use" {
		t.Fatalf("stop_reason = %v, want tool_use", obj["stop_reason"])
	}
	blocks := contentBlocks(t, obj)
	if len(blocks) != 3 {
		t.Fatalf("content blocks = %d, want 3", len(blocks))
	}
	thinking := blocks[0].(map[string]any)
	if thinking["type"] != "thinking" || thinking["thinking"] != "plan" || thinking["signature"] != "sig-1" {
		t.Fatalf("thinking block = %v", thinking)
	}
	if text := blocks[1].(map[string]any); text["type"] != "text" || text["text"] != "answer" {
		t.Fatalf("text block = %v", text)
	}
	tool := blocks[2].(map[string]any)
	if tool["type"] != "tool_use" || tool["id"] != "toolu_1" || tool["name"] != "lookup" {
		t.Fatalf("tool_use block = %v", tool)
	}
	if input := tool["input"].(map[string]any); input["q"] != "x" {
		t.Fatalf("tool_use.input = %v, want {q:x}", input)
	}
}

// TestBuildAnthropicResponseUnmappableBlocksFailLoudly：无法用 Anthropic 响应
// 块表达的部件必须显式报错（信息损失清单），不得静默丢弃。
func TestBuildAnthropicResponseUnmappableBlocksFailLoudly(t *testing.T) {
	cases := map[string]contract.ContentPart{
		"tool_result": {Kind: contract.PartToolResult, Text: "x"},
		"audio":       {Kind: contract.PartAudio, Payload: []byte("a")},
	}
	for name, part := range cases {
		t.Run(name, func(t *testing.T) {
			resp := &contract.Response{Choices: []contract.Choice{{Message: contract.Message{
				Role: contract.RoleAssistant, Content: []contract.ContentPart{part},
			}}}}
			if _, err := BuildAnthropicResponse(resp, contract.Usage{}); err == nil {
				t.Fatal("无法映射的块必须返回错误，而不是静默丢弃")
			}
		})
	}
}

// TestBuildAnthropicResponseRedactedThinking：加密思考块按 data 原样回写。
func TestBuildAnthropicResponseRedactedThinking(t *testing.T) {
	resp := &contract.Response{Choices: []contract.Choice{{Message: contract.Message{
		Role: contract.RoleAssistant,
		Content: []contract.ContentPart{{
			Kind: contract.PartText, Meta: map[string]any{"redacted_thinking": "cipher"},
		}},
	}}}}
	body, err := BuildAnthropicResponse(resp, contract.Usage{})
	if err != nil {
		t.Fatal(err)
	}
	block := contentBlocks(t, decodeAnthropicJSON(t, body))[0].(map[string]any)
	if block["type"] != "redacted_thinking" || block["data"] != "cipher" {
		t.Fatalf("block = %v, want redacted_thinking cipher", block)
	}
}

// TestAnthropicResponseRoundTrip 证明编码结果能被本包再次解析回等价 IR。
func TestAnthropicResponseRoundTrip(t *testing.T) {
	original := `{"id":"msg_rt","type":"message","role":"assistant","model":"claude-x",` +
		`"content":[{"type":"thinking","thinking":"t","signature":"s"},` +
		`{"type":"text","text":"hi"},` +
		`{"type":"tool_use","id":"toolu_9","name":"lookup","input":{"q":"y"}}],` +
		`"stop_reason":"tool_use","usage":{"input_tokens":3,"output_tokens":7}}`
	first, usage, err := ParseAnthropicResponse([]byte(original))
	if err != nil {
		t.Fatal(err)
	}
	body, err := BuildAnthropicResponse(first, usage)
	if err != nil {
		t.Fatal(err)
	}
	second, reParsed, err := ParseAnthropicResponse(body)
	if err != nil {
		t.Fatalf("二次解析失败: %v; body=%s", err, body)
	}
	if second.FinishReason != "tool_calls" {
		t.Fatalf("finish reason = %q, want tool_calls", second.FinishReason)
	}
	if reParsed.Prompt != 3 || reParsed.Completion != 7 {
		t.Fatalf("usage = %+v, want prompt 3 / completion 7", reParsed)
	}
	var kinds []string
	for _, part := range second.Choices[0].Message.Content {
		kinds = append(kinds, string(part.Kind))
	}
	want := []string{"text", "text", "tool_call"} // thinking 在 IR 里是 PartText+Meta
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}
	if !isReasoningPart(second.Choices[0].Message.Content[0]) {
		t.Fatal("thinking 块往返后必须仍被识别为推理部件（不得混入正文）")
	}
}

// TestAnthropicStreamRendererTextOrder：文本流的完整事件顺序与 usage。
// 变异「删掉 message_stop」会让本用例红（R-W18-03 变异点①）。
func TestAnthropicStreamRendererTextOrder(t *testing.T) {
	r := NewAnthropicStreamRenderer("msg_1", "m")
	var frames []AnthropicSSEFrame
	frames = append(frames, r.Start()...)
	frames = append(frames, r.Push(contract.StreamEvent{Kind: contract.EventDelta, Delta: "Hel"})...)
	frames = append(frames, r.Push(contract.StreamEvent{Kind: contract.EventDelta, Delta: "lo"})...)
	frames = append(frames, r.Push(contract.StreamEvent{
		Kind: contract.EventFinish,
		Raw:  []byte(`data: {"choices":[{"finish_reason":"stop"}]}`),
	})...)
	frames = append(frames, r.Close(contract.Usage{Prompt: 4, Completion: 2})...)

	var names []string
	for _, frame := range frames {
		names = append(names, frame.Event)
	}
	want := []string{
		"message_start",
		"content_block_start", "content_block_delta", "content_block_delta", "content_block_stop",
		"message_delta", "message_stop",
	}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v, want %v", names, want)
	}
	delta := decodeAnthropicJSON(t, frames[5].Data)
	if delta["type"] != "message_delta" {
		t.Fatalf("message_delta = %v", delta)
	}
	d := delta["delta"].(map[string]any)
	if d["stop_reason"] != "end_turn" {
		t.Fatalf("stop_reason = %v, want end_turn", d["stop_reason"])
	}
	usage := delta["usage"].(map[string]any)
	if usage["input_tokens"] != float64(4) || usage["output_tokens"] != float64(2) {
		t.Fatalf("message_delta usage = %v, want 4/2", usage)
	}
	// message_start 的 usage 必须是 0（异构上游的用量在末尾才可知）。
	start := decodeAnthropicJSON(t, frames[0].Data)
	if start["type"] != "message_start" {
		t.Fatalf("first frame = %v, want message_start", start)
	}
	inner := start["message"].(map[string]any)
	startUsage := inner["usage"].(map[string]any)
	if startUsage["input_tokens"] != float64(0) || startUsage["output_tokens"] != float64(0) {
		t.Fatalf("message_start usage = %v, want zeros", startUsage)
	}
}

// TestAnthropicStreamRendererThinkingAndTools：推理与工具调用的块切换、
// input_json_delta 与 tool_use 的 stop_reason。
func TestAnthropicStreamRendererThinkingAndTools(t *testing.T) {
	r := NewAnthropicStreamRenderer("msg_t", "m")
	var frames []AnthropicSSEFrame
	frames = append(frames, r.Start()...)
	frames = append(frames, r.Push(contract.StreamEvent{Kind: contract.EventDelta, Reasoning: "think"})...)
	frames = append(frames, r.Push(contract.StreamEvent{Kind: contract.EventDelta, Delta: "answer"})...)
	frames = append(frames, r.Push(contract.StreamEvent{Kind: contract.EventDelta, ToolDelta: &contract.ToolCallDelta{
		Index: 0, ID: "call_1", Name: "lookup", ArgumentsFragment: `{"q":`,
	}})...)
	frames = append(frames, r.Push(contract.StreamEvent{Kind: contract.EventDelta, ToolDelta: &contract.ToolCallDelta{
		Index: 0, ArgumentsFragment: `"x"}`,
	}})...)
	frames = append(frames, r.Close(contract.Usage{})...)

	var (
		sawThinkingDelta bool
		sawToolStart     bool
		sawToolArgs      bool
		lastStopReason   any
	)
	for _, frame := range frames {
		obj := decodeAnthropicJSON(t, frame.Data)
		switch frame.Event {
		case "content_block_delta":
			delta := obj["delta"].(map[string]any)
			if delta["type"] == "thinking_delta" && delta["thinking"] == "think" {
				sawThinkingDelta = true
			}
			if delta["type"] == "input_json_delta" && delta["partial_json"] == `{"q":"x"}` {
				sawToolArgs = true
			}
		case "content_block_start":
			block := obj["content_block"].(map[string]any)
			if block["type"] == "tool_use" && block["id"] == "call_1" && block["name"] == "lookup" {
				sawToolStart = true
			}
		case "message_delta":
			lastStopReason = obj["delta"].(map[string]any)["stop_reason"]
		}
	}
	if !sawThinkingDelta || !sawToolStart || !sawToolArgs {
		t.Fatalf("thinking=%v toolStart=%v toolArgs=%v, want all true",
			sawThinkingDelta, sawToolStart, sawToolArgs)
	}
	if lastStopReason != "tool_use" {
		t.Fatalf("stop_reason = %v, want tool_use（有工具块时的兜底）", lastStopReason)
	}
}

// TestAnthropicStreamRendererGeminiFinishReason：Finish 帧的原始体按协议
// 嗅探（Gemini MAX_TOKENS → max_tokens），不依赖 IR 事件新增字段。
func TestAnthropicStreamRendererGeminiFinishReason(t *testing.T) {
	r := NewAnthropicStreamRenderer("msg_g", "m")
	_ = r.Start()
	_ = r.Push(contract.StreamEvent{
		Kind: contract.EventFinish,
		Raw:  []byte(`data: {"candidates":[{"finishReason":"MAX_TOKENS"}]}`),
	})
	frames := r.Close(contract.Usage{})
	delta := decodeAnthropicJSON(t, frames[0].Data)
	if got := delta["delta"].(map[string]any)["stop_reason"]; got != "max_tokens" {
		t.Fatalf("stop_reason = %v, want max_tokens", got)
	}
}

// TestBuildAnthropicResponseSynthesizesToolUseID 锁定复核 F1：
// Gemini functionCall 没有 id，响应方向必须合成稳定非空 id，且客户端回传
// tool_result 时本网关自己的解析器能接受（否则多轮工具调用链断开）。
func TestBuildAnthropicResponseSynthesizesToolUseID(t *testing.T) {
	raw := `{"candidates":[{"content":{"role":"model","parts":[` +
		`{"functionCall":{"name":"lookup","args":{"q":"x"}}}]},"finishReason":"STOP"}],` +
		`"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5}}`
	resp, usage, err := ParseGeminiResponse([]byte(raw))
	if err != nil {
		t.Fatalf("ParseGeminiResponse: %v", err)
	}
	body, err := BuildAnthropicResponse(resp, usage)
	if err != nil {
		t.Fatalf("BuildAnthropicResponse: %v", err)
	}
	blocks := contentBlocks(t, decodeAnthropicJSON(t, body))
	if len(blocks) != 1 {
		t.Fatalf("blocks = %d, want 1", len(blocks))
	}
	tool := blocks[0].(map[string]any)
	if tool["type"] != "tool_use" {
		t.Fatalf("block = %v, want tool_use", tool)
	}
	id, _ := tool["id"].(string)
	if id == "" {
		t.Fatal("tool_use id 为空：Gemini functionCall 无 id 时必须合成非空 id")
	}

	// 回传：用响应给出的 id 构造 tool_result，本网关解析器必须接受。
	followup := `{"model":"claude-test","max_tokens":16,"messages":[` +
		`{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"` + id + `","name":"lookup","input":{}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + id + `","content":"ok"}]}]}`
	if _, err := ParseAnthropicRequest([]byte(followup)); err != nil {
		t.Fatalf("回传 tool_result 被拒绝（id=%q）: %v", id, err)
	}
}

// TestBuildAnthropicResponseKeepsExistingToolUseID：已有 id 必须原样保留。
func TestBuildAnthropicResponseKeepsExistingToolUseID(t *testing.T) {
	resp := &contract.Response{ID: "msg_keep", Choices: []contract.Choice{{Message: contract.Message{
		Role: contract.RoleAssistant,
		Content: []contract.ContentPart{{
			Kind: contract.PartToolCall, Text: `{}`,
			Meta: map[string]any{"id": "toolu_given", "name": "lookup"},
		}},
	}}}}
	body, err := BuildAnthropicResponse(resp, contract.Usage{})
	if err != nil {
		t.Fatal(err)
	}
	tool := contentBlocks(t, decodeAnthropicJSON(t, body))[0].(map[string]any)
	if tool["id"] != "toolu_given" {
		t.Fatalf("id = %v, want toolu_given（不得改写已有 id）", tool["id"])
	}
}

// TestAnthropicStreamRendererSynthesizesToolUseID：流式工具增量没有 id 时，
// tool_use 块同样必须带非空 id，并能被回传解析。
func TestAnthropicStreamRendererSynthesizesToolUseID(t *testing.T) {
	r := NewAnthropicStreamRenderer("msg_req1", "m")
	_ = r.Start()
	_ = r.Push(contract.StreamEvent{Kind: contract.EventDelta, ToolDelta: &contract.ToolCallDelta{
		Index: 0, Name: "lookup", ArgumentsFragment: `{"q":"x"}`,
	}})
	frames := r.Close(contract.Usage{})

	var id string
	for _, frame := range frames {
		if frame.Event != "content_block_start" {
			continue
		}
		obj := decodeAnthropicJSON(t, frame.Data)
		block, _ := obj["content_block"].(map[string]any)
		if block["type"] == "tool_use" {
			id, _ = block["id"].(string)
		}
	}
	if id == "" {
		t.Fatal("流式 tool_use id 为空：缺失时必须合成非空 id")
	}
	followup := `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"` + id + `","content":"ok"}]}]}`
	if _, err := ParseAnthropicRequest([]byte(followup)); err != nil {
		t.Fatalf("流式合成 id 回传 tool_result 被拒绝（id=%q）: %v", id, err)
	}
}

func TestAnthropicToolResultMapsToToolRole(t *testing.T) {
	body := `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]}]}`
	req, err := ParseAnthropicRequest([]byte(body))
	if err != nil {
		t.Fatalf("ParseAnthropicRequest: %v", err)
	}
	if len(req.Messages) != 1 || req.Messages[0].Role != contract.RoleTool {
		t.Fatalf("tool_result 必须归一化为 tool 角色：%+v", req.Messages)
	}
	if req.Messages[0].Content[0].Kind != contract.PartToolResult ||
		metaString(req.Messages[0].Content[0].Meta, "tool_call_id") != "toolu_1" {
		t.Fatalf("tool_result 内容 = %+v", req.Messages[0].Content[0])
	}
}

func TestAnthropicSystemRoleInsideMessages(t *testing.T) {
	body := `{"model":"m","max_tokens":8,"messages":[{"role":"system","content":"be nice"},{"role":"user","content":"hi"}]}`
	req, err := ParseAnthropicRequest([]byte(body))
	if err != nil {
		t.Fatalf("ParseAnthropicRequest: %v", err)
	}
	if len(req.Messages) != 2 || req.Messages[0].Role != contract.RoleSystem {
		t.Fatalf("messages 内的 system 角色必须被接受：%+v", req.Messages)
	}
	if req.Messages[0].Content[0].Text != "be nice" {
		t.Fatalf("system content = %+v", req.Messages[0].Content)
	}
}

// TestAnthropicMixedContentKeepsToolMessageAdjacent 冻结 OpenAI/DeepSeek 的
// 消息相邻约束：assistant(tool_calls) 之后必须立刻是 tool 消息。
//
// Anthropic 把 tool_result 与正文放在同一条 user 消息里是合法形态；跨协议
// 出站时必须先工具结果、后正文，否则 OpenAI 兼容上游会 400：
// "messages with role 'tool' must be a response to a preceding message with
// 'tool_calls'"（正文消息把 tool 与 assistant 隔开了）。
func TestAnthropicMixedContentKeepsToolMessageAdjacent(t *testing.T) {
	const reminder = "<system-reminder>note</system-reminder>"
	body := `{"model":"m","max_tokens":8,"messages":[
		{"role":"user","content":"start"},
		{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"lookup","input":{"q":"x"}}]},
		{"role":"user","content":[
			{"type":"text","text":"` + reminder + `"},
			{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}
		]}
	]}`
	req, err := ParseAnthropicRequest([]byte(body))
	if err != nil {
		t.Fatalf("ParseAnthropicRequest: %v", err)
	}
	wantRoles := []contract.Role{
		contract.RoleUser,
		contract.RoleAssistant,
		contract.RoleTool,
		contract.RoleUser,
	}
	if len(req.Messages) != len(wantRoles) {
		t.Fatalf("messages = %d, want %d: %+v", len(req.Messages), len(wantRoles), req.Messages)
	}
	for i, want := range wantRoles {
		if req.Messages[i].Role != want {
			t.Fatalf("messages[%d].Role = %q, want %q（tool 必须紧跟 assistant/tool_calls）",
				i, req.Messages[i].Role, want)
		}
	}
	if len(req.Messages[3].Content) != 1 || req.Messages[3].Content[0].Text != reminder {
		t.Fatalf("正文不得丢失或串位：%+v", req.Messages[3].Content)
	}
	if metaString(req.Messages[2].Content[0].Meta, "tool_call_id") != "toolu_1" {
		t.Fatalf("tool_result 未保留 tool_call_id：%+v", req.Messages[2].Content[0])
	}
}
