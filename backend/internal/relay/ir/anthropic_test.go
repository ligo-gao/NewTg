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
