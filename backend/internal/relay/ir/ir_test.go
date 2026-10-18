package ir

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"newtg/gateway/internal/contract"
)

// canonicalRequest 是一份覆盖全部归一化字段的 OpenAI Chat Completions 请求
// （文本 + 图片 URL + assistant 工具调用 + tool 结果 + tools/tool_choice/response_format）。
const canonicalRequest = `{
  "model": "gpt-4o",
  "stream": true,
  "messages": [
    {"role": "system", "content": "be nice"},
    {"role": "user", "content": [
      {"type": "text", "text": "what is this?"},
      {"type": "image_url", "image_url": {"url": "https://example.com/a.png", "detail": "high"}}
    ]},
    {"role": "assistant", "content": null, "tool_calls": [
      {"id": "call_1", "type": "function", "function": {"name": "lookup", "arguments": "{\"q\":\"x\"}"}}
    ]},
    {"role": "tool", "tool_call_id": "call_1", "content": "result"}
  ],
  "tools": [{"type": "function", "function": {
    "name": "lookup", "description": "d", "parameters": {"type": "object"}
  }}],
  "tool_choice": {"type": "function", "function": {"name": "lookup"}},
  "response_format": {"type": "json_object"},
  "max_tokens": 128,
  "temperature": 0.2,
  "top_p": 0.9,
  "stop": ["END"],
  "seed": 7,
  "logprobs": true
}`

func TestIRParseOpenAIRequestCanonical(t *testing.T) {
	req, err := ParseOpenAIRequest([]byte(canonicalRequest))
	if err != nil {
		t.Fatalf("ParseOpenAIRequest: %v", err)
	}
	if req.Model != "gpt-4o" || !req.Stream {
		t.Fatalf("model/stream = %q/%v, want gpt-4o/true", req.Model, req.Stream)
	}
	if req.RawFormat != FormatOpenAIChat {
		t.Fatalf("RawFormat = %q, want %q", req.RawFormat, FormatOpenAIChat)
	}
	if string(req.Raw) != canonicalRequest {
		t.Fatal("Raw 必须原样保存入站请求体（双层 IR 的原始层）")
	}
	if len(req.Messages) != 4 {
		t.Fatalf("messages = %d, want 4", len(req.Messages))
	}
	if req.Messages[0].Role != contract.RoleSystem || len(req.Messages[0].Content) != 1 ||
		req.Messages[0].Content[0].Kind != contract.PartText || req.Messages[0].Content[0].Text != "be nice" {
		t.Fatalf("system message = %+v", req.Messages[0])
	}
	user := req.Messages[1]
	if user.Role != contract.RoleUser || len(user.Content) != 2 {
		t.Fatalf("user message = %+v", user)
	}
	if user.Content[1].Kind != contract.PartImageURL || user.Content[1].URL != "https://example.com/a.png" ||
		user.Content[1].Meta["detail"] != "high" {
		t.Fatalf("image part = %+v", user.Content[1])
	}
	asst := req.Messages[2]
	if asst.Role != contract.RoleAssistant || len(asst.Content) != 1 || asst.Content[0].Kind != contract.PartToolCall {
		t.Fatalf("assistant message = %+v", asst)
	}
	if asst.Content[0].Text != `{"q":"x"}` || asst.Content[0].Meta["id"] != "call_1" ||
		asst.Content[0].Meta["name"] != "lookup" {
		t.Fatalf("tool_call part = %+v", asst.Content[0])
	}
	tool := req.Messages[3]
	if tool.Role != contract.RoleTool || len(tool.Content) != 1 || tool.Content[0].Kind != contract.PartToolResult {
		t.Fatalf("tool message = %+v", tool)
	}
	if tool.Content[0].Text != "result" || tool.Content[0].Meta["tool_call_id"] != "call_1" {
		t.Fatalf("tool_result part = %+v", tool.Content[0])
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != "lookup" || req.Tools[0].Description != "d" {
		t.Fatalf("tools = %+v", req.Tools)
	}
	if req.Tools[0].Schema["type"] != "object" {
		t.Fatalf("tool schema = %+v", req.Tools[0].Schema)
	}
	if req.ToolChoice.Mode != contract.ToolChoiceSpecific || req.ToolChoice.Name != "lookup" {
		t.Fatalf("tool_choice = %+v", req.ToolChoice)
	}
	if req.ResponseFormat == nil || req.ResponseFormat.Kind != contract.ResponseFormatJSONObject {
		t.Fatalf("response_format = %+v", req.ResponseFormat)
	}
	if req.MaxTokens == nil || *req.MaxTokens != 128 {
		t.Fatalf("MaxTokens = %v", req.MaxTokens)
	}
	if req.Temperature == nil || *req.Temperature != 0.2 || req.TopP == nil || *req.TopP != 0.9 {
		t.Fatalf("temperature/top_p = %v/%v", req.Temperature, req.TopP)
	}
	if !reflect.DeepEqual(req.Stop, []string{"END"}) {
		t.Fatalf("stop = %v", req.Stop)
	}
	if req.Seed == nil || *req.Seed != 7 {
		t.Fatalf("seed = %v", req.Seed)
	}
}

func TestIRBuildOpenAIRequestBodyRoundTrip(t *testing.T) {
	first, err := ParseOpenAIRequest([]byte(canonicalRequest))
	if err != nil {
		t.Fatalf("ParseOpenAIRequest: %v", err)
	}
	body, err := BuildOpenAIRequestBody(first)
	if err != nil {
		t.Fatalf("BuildOpenAIRequestBody: %v", err)
	}
	second, err := ParseOpenAIRequest(body)
	if err != nil {
		t.Fatalf("re-parse built body: %v (body=%s)", err, body)
	}
	if second.Model != first.Model || second.Stream != first.Stream {
		t.Fatalf("round-trip model/stream mismatch: %q/%v", second.Model, second.Stream)
	}
	if len(second.Messages) != len(first.Messages) {
		t.Fatalf("round-trip messages = %d, want %d", len(second.Messages), len(first.Messages))
	}
	for i := range first.Messages {
		if first.Messages[i].Role != second.Messages[i].Role {
			t.Fatalf("message %d role = %q, want %q", i, second.Messages[i].Role, first.Messages[i].Role)
		}
		if len(first.Messages[i].Content) != len(second.Messages[i].Content) {
			t.Fatalf("message %d content parts = %d, want %d", i, len(second.Messages[i].Content), len(first.Messages[i].Content))
		}
		for j := range first.Messages[i].Content {
			a, b := first.Messages[i].Content[j], second.Messages[i].Content[j]
			if a.Kind != b.Kind || a.Text != b.Text || a.URL != b.URL {
				t.Fatalf("message %d part %d = %+v, want %+v", i, j, b, a)
			}
		}
	}
	if !reflect.DeepEqual(second.Tools, first.Tools) {
		t.Fatalf("round-trip tools = %+v, want %+v", second.Tools, first.Tools)
	}
	if second.ToolChoice != first.ToolChoice {
		t.Fatalf("round-trip tool_choice = %+v, want %+v", second.ToolChoice, first.ToolChoice)
	}
	if second.ResponseFormat == nil || second.ResponseFormat.Kind != first.ResponseFormat.Kind {
		t.Fatalf("round-trip response_format = %+v", second.ResponseFormat)
	}
	if second.MaxTokens == nil || *second.MaxTokens != *first.MaxTokens ||
		second.Temperature == nil || *second.Temperature != *first.Temperature ||
		second.TopP == nil || *second.TopP != *first.TopP ||
		second.Seed == nil || *second.Seed != *first.Seed {
		t.Fatal("round-trip optional scalars mismatch")
	}
	if !reflect.DeepEqual(second.Stop, first.Stop) {
		t.Fatalf("round-trip stop = %v, want %v", second.Stop, first.Stop)
	}
}

func TestIRBuildOpenAIRequestBodyOmitsNilScalars(t *testing.T) {
	body, err := BuildOpenAIRequestBody(&contract.Request{
		Model: "gpt-4o",
		Messages: []contract.Message{{
			Role:    contract.RoleUser,
			Content: []contract.ContentPart{{Kind: contract.PartText, Text: "hi"}},
		}},
	})
	if err != nil {
		t.Fatalf("BuildOpenAIRequestBody: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	for _, key := range []string{"max_tokens", "temperature", "top_p", "stop", "seed", "tools", "tool_choice", "response_format"} {
		if _, ok := obj[key]; ok {
			t.Fatalf("可选标量为 nil 时不得下发 %q（body=%s）", key, body)
		}
	}
	if obj["model"] != "gpt-4o" {
		t.Fatalf("model = %v", obj["model"])
	}
	if _, ok := obj["stream"]; !ok {
		t.Fatal("stream 必须显式下发（false 也是有效值）")
	}
}

func TestIRBuildOpenAIRequestBodyExplicitZeroScalars(t *testing.T) {
	zeroInt, zeroFloat := 0, 0.0
	body, err := BuildOpenAIRequestBody(&contract.Request{
		Model:       "gpt-4o",
		MaxTokens:   &zeroInt,
		Temperature: &zeroFloat,
		TopP:        &zeroFloat,
		Seed:        &zeroInt,
		Messages:    []contract.Message{{Role: contract.RoleUser, Content: []contract.ContentPart{{Kind: contract.PartText, Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("BuildOpenAIRequestBody: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	for _, key := range []string{"max_tokens", "temperature", "top_p", "seed"} {
		if _, ok := obj[key]; !ok {
			t.Fatalf("显式 0 必须下发 %q（body=%s）", key, body)
		}
	}
}

func TestIRBuildOpenAIRequestBodyPreservesRawUnknownFields(t *testing.T) {
	raw := `{"model":"client-model","messages":[],"logprobs":true,"user":"u-1"}`
	body, err := BuildOpenAIRequestBody(&contract.Request{
		Model:     "mapped-model",
		RawFormat: FormatOpenAIChat,
		Raw:       contract.ProtocolBody(raw),
		Messages:  []contract.Message{{Role: contract.RoleUser, Content: []contract.ContentPart{{Kind: contract.PartText, Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("BuildOpenAIRequestBody: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if obj["model"] != "mapped-model" {
		t.Fatalf("归一化字段必须覆盖 Raw：model = %v", obj["model"])
	}
	if obj["logprobs"] != true || obj["user"] != "u-1" {
		t.Fatalf("Raw 中未映射字段必须透传（body=%s）", body)
	}
	msgs, _ := obj["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %v", obj["messages"])
	}
}

func TestIRBuildOpenAIRequestBodyMultimodal(t *testing.T) {
	body, err := BuildOpenAIRequestBody(&contract.Request{
		Model: "gpt-4o",
		Messages: []contract.Message{{
			Role: contract.RoleUser,
			Content: []contract.ContentPart{
				{Kind: contract.PartText, Text: "describe"},
				{Kind: contract.PartImageB64, Payload: []byte("png-bytes"), Meta: map[string]any{"mime_type": "image/png", "detail": "low"}},
				{Kind: contract.PartAudio, Payload: []byte("wav-bytes"), Meta: map[string]any{"format": "wav"}},
			},
		}},
	})
	if err != nil {
		t.Fatalf("BuildOpenAIRequestBody: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	parsed, err := ParseOpenAIRequest(body)
	if err != nil {
		t.Fatalf("re-parse multimodal body: %v (body=%s)", err, body)
	}
	parts := parsed.Messages[0].Content
	if len(parts) != 3 {
		t.Fatalf("parts = %d, want 3 (body=%s)", len(parts), body)
	}
	if parts[1].Kind != contract.PartImageB64 || string(parts[1].Payload) != "png-bytes" ||
		parts[1].Meta["mime_type"] != "image/png" || parts[1].Meta["detail"] != "low" {
		t.Fatalf("image_b64 round-trip = %+v", parts[1])
	}
	if parts[2].Kind != contract.PartAudio || string(parts[2].Payload) != "wav-bytes" || parts[2].Meta["format"] != "wav" {
		t.Fatalf("audio round-trip = %+v", parts[2])
	}
}

func TestIRParseOpenAIToolChoiceSpecificRequiresName(t *testing.T) {
	cases := map[string]string{
		"missing name": `{"model":"m","messages":[],"tool_choice":{"type":"function","function":{}}}`,
		"empty name":   `{"model":"m","messages":[],"tool_choice":{"type":"function","function":{"name":""}}}`,
		"bad mode":     `{"model":"m","messages":[],"tool_choice":"sometimes"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseOpenAIRequest([]byte(body))
			if err == nil {
				t.Fatal("want error, got nil")
			}
			if !errors.Is(err, ErrParamInvalid) {
				t.Fatalf("error = %v, want ErrParamInvalid-class", err)
			}
		})
	}
}

func TestIRParseOpenAIRequestRejectsUnknownRole(t *testing.T) {
	_, err := ParseOpenAIRequest([]byte(`{"model":"m","messages":[{"role":"overlord","content":"hi"}]}`))
	if err == nil || !errors.Is(err, ErrParamInvalid) {
		t.Fatalf("error = %v, want ErrParamInvalid", err)
	}
}

func TestIRParseOpenAIResponseUsage(t *testing.T) {
	body := `{
	  "id": "chatcmpl-1", "object": "chat.completion", "model": "gpt-4o",
	  "choices": [{"index": 0, "message": {"role": "assistant", "content": "hello"}, "finish_reason": "stop"}],
	  "usage": {
	    "prompt_tokens": 100, "completion_tokens": 40, "total_tokens": 140,
	    "prompt_tokens_details": {"cached_tokens": 30, "cache_write_tokens": 5, "image_tokens": 10, "audio_tokens": 0},
	    "completion_tokens_details": {"reasoning_tokens": 20, "audio_tokens": 0}
	  }
	}`
	resp, usage, err := ParseOpenAIResponse([]byte(body))
	if err != nil {
		t.Fatalf("ParseOpenAIResponse: %v", err)
	}
	if resp.ID != "chatcmpl-1" || resp.Model != "gpt-4o" {
		t.Fatalf("response id/model = %q/%q", resp.ID, resp.Model)
	}
	if len(resp.Choices) != 1 || resp.Choices[0].Message.Role != contract.RoleAssistant {
		t.Fatalf("choices = %+v", resp.Choices)
	}
	if len(resp.Choices[0].Message.Content) != 1 || resp.Choices[0].Message.Content[0].Text != "hello" {
		t.Fatalf("choice content = %+v", resp.Choices[0].Message.Content)
	}
	if resp.Choices[0].FinishReason != "stop" || resp.FinishReason != "stop" {
		t.Fatalf("finish_reason = %q/%q", resp.Choices[0].FinishReason, resp.FinishReason)
	}
	if string(resp.Raw) != body {
		t.Fatal("Response.Raw 必须保存上游原始响应体")
	}

	if usage.Prompt != 100 || usage.Completion != 40 || usage.Total != 140 {
		t.Fatalf("usage base = %+v", usage)
	}
	if !usage.PromptIncludesCache {
		t.Fatal("PromptIncludesCache 必须为 true（GPT 格式，relay-03 §6 row 1）")
	}
	if !usage.ReasoningIncludedInCompletion {
		t.Fatal("ReasoningIncludedInCompletion 必须为 true（GPT 格式，relay-03 §6 row 1）")
	}
	if usage.CacheRead != 30 || usage.CacheWrite != 5 || usage.Reasoning != 20 {
		t.Fatalf("cache/reasoning = %+v", usage)
	}
	if usage.PromptDetail.Image != 10 || usage.PromptDetail.Text != 90 {
		t.Fatalf("prompt detail = %+v", usage.PromptDetail)
	}
	// Completion 是文本 token 维度：reasoning 也是文本输出，只扣音频，不扣 reasoning。
	if usage.CompletionDetail.Text != 40 || usage.CompletionDetail.Audio != 0 {
		t.Fatalf("completion detail = %+v", usage.CompletionDetail)
	}
	if usage.Source != contract.UsageUpstreamExact {
		t.Fatalf("source = %v, want upstream_exact", usage.Source)
	}
	if usage.Raw == nil || usage.Raw["prompt_tokens"] == nil {
		t.Fatalf("usage.Raw 必须保留协议原始 usage：%+v", usage.Raw)
	}
}

func TestIRParseOpenAIResponseDeepSeekCacheHitFallback(t *testing.T) {
	body := `{"id":"c","model":"deepseek-chat","choices":[],"usage":{
	  "prompt_tokens":50,"completion_tokens":10,"total_tokens":60,"prompt_cache_hit_tokens":25
	}}`
	_, usage, err := ParseOpenAIResponse([]byte(body))
	if err != nil {
		t.Fatalf("ParseOpenAIResponse: %v", err)
	}
	if usage.CacheRead != 25 {
		t.Fatalf("CacheRead = %d, want 25（prompt_cache_hit_tokens 归一为 CR）", usage.CacheRead)
	}
}

func TestIRParseOpenAIResponseToolCalls(t *testing.T) {
	body := `{"id":"c","model":"m","choices":[{"index":0,"message":{
	  "role":"assistant","content":null,"tool_calls":[
	    {"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}
	  ]},"finish_reason":"tool_calls"}]}`
	resp, _, err := ParseOpenAIResponse([]byte(body))
	if err != nil {
		t.Fatalf("ParseOpenAIResponse: %v", err)
	}
	parts := resp.Choices[0].Message.Content
	if len(parts) != 1 || parts[0].Kind != contract.PartToolCall || parts[0].Meta["name"] != "lookup" {
		t.Fatalf("tool call parts = %+v", parts)
	}
	if resp.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish_reason = %q", resp.Choices[0].FinishReason)
	}
}

func TestIRToIRReturnsCopyAndValidates(t *testing.T) {
	original := &contract.Request{
		Model: "gpt-4o",
		Messages: []contract.Message{{
			Role:    contract.RoleUser,
			Content: []contract.ContentPart{{Kind: contract.PartText, Text: "hi"}},
			Extensions: map[string]any{
				"name": "alice",
			},
		}},
		Tools: []contract.Tool{{
			Name:   "lookup",
			Schema: map[string]any{"type": "object"},
		}},
	}
	got, err := ToIR(original, contract.ChannelTypeOpenAI)
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	if got == original {
		t.Fatal("ToIR 必须返回副本，不得返回入参指针")
	}
	got.Messages[0].Content[0].Text = "mutated"
	got.Messages[0].Extensions["name"] = "bob"
	got.Tools[0].Schema["type"] = "string"
	if original.Messages[0].Content[0].Text != "hi" ||
		original.Messages[0].Extensions["name"] != "alice" ||
		original.Tools[0].Schema["type"] != "object" {
		t.Fatal("ToIR 的副本被修改后影响了原请求（深拷贝不完整）")
	}

	bad := &contract.Request{ToolChoice: contract.ToolChoice{Mode: contract.ToolChoiceSpecific}}
	if _, err := ToIR(bad, contract.ChannelTypeOpenAI); err == nil || !errors.Is(err, ErrParamInvalid) {
		t.Fatalf("specific 无 name 必须返回 ErrParamInvalid，got %v", err)
	}
}

func TestIRFromIRBuildsOpenAIJSON(t *testing.T) {
	src := &contract.Response{
		ID:    "resp-1",
		Model: "m-normalized",
		Choices: []contract.Choice{{
			Index: 0,
			Message: contract.Message{
				Role:    contract.RoleAssistant,
				Content: []contract.ContentPart{{Kind: contract.PartText, Text: "hi"}},
			},
			FinishReason: "stop",
		}},
		FinishReason: "stop",
	}
	out, err := FromIR(src)
	if err != nil {
		t.Fatalf("FromIR: %v", err)
	}
	if out == src {
		t.Fatal("FromIR 必须返回副本")
	}
	var obj map[string]any
	if err := json.Unmarshal(out.Raw, &obj); err != nil {
		t.Fatalf("FromIR Raw 必须是 OpenAI JSON：%v (raw=%s)", err, out.Raw)
	}
	if obj["object"] != "chat.completion" || obj["id"] != "resp-1" || obj["model"] != "m-normalized" {
		t.Fatalf("openai payload = %+v", obj)
	}
	choices, _ := obj["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices = %v", obj["choices"])
	}
	choice := choices[0].(map[string]any)
	if choice["finish_reason"] != "stop" {
		t.Fatalf("finish_reason = %v", choice["finish_reason"])
	}
	msg := choice["message"].(map[string]any)
	if msg["role"] != "assistant" || msg["content"] != "hi" {
		t.Fatalf("message = %+v", msg)
	}
	if string(src.Raw) != "" {
		t.Fatal("FromIR 不得修改入参的 Raw")
	}
}

func TestIRParseOpenAIResponseEmptyUsageIsEstimated(t *testing.T) {
	body := `{"id":"c","model":"m","choices":[],"usage":{}}`
	_, usage, err := ParseOpenAIResponse([]byte(body))
	if err != nil {
		t.Fatalf("ParseOpenAIResponse: %v", err)
	}
	if usage.Source != contract.UsageEstimated {
		t.Fatalf("空 usage 的 Source = %v, want UsageEstimated（不得标成 upstream_exact）", usage.Source)
	}
	if !usage.PromptIncludesCache || !usage.ReasoningIncludedInCompletion {
		t.Fatalf("关系布尔必须显式赋值：%+v", usage)
	}
}

func TestIRParseOpenAIRequestNegativeMaxCompletionTokensRejected(t *testing.T) {
	_, err := ParseOpenAIRequest([]byte(`{"model":"m","messages":[],"max_completion_tokens":-5}`))
	if err == nil || !errors.Is(err, ErrParamInvalid) {
		t.Fatalf("负的 max_completion_tokens 必须被拒绝（回填后仍须校验），got %v", err)
	}
}

func TestIRBuildOpenAIRequestBodySkipsRawWhenFormatUnknown(t *testing.T) {
	// RawFormat 为空说明不是本包解析出的 OpenAI 请求：不得把 Raw 字段混入目标请求。
	body, err := BuildOpenAIRequestBody(&contract.Request{
		Model:     "m",
		RawFormat: "",
		Raw:       contract.ProtocolBody(`{"logprobs":true,"user":"u-1"}`),
		Messages:  []contract.Message{{Role: contract.RoleUser, Content: []contract.ContentPart{{Kind: contract.PartText, Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("BuildOpenAIRequestBody: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if _, ok := obj["logprobs"]; ok {
		t.Fatalf("空 RawFormat 不得透传 Raw 字段（body=%s）", body)
	}
}

func TestIRFromIRRejectsNil(t *testing.T) {
	if _, err := FromIR(nil); err == nil {
		t.Fatal("nil response 必须返回错误")
	}
}

// ============================================================================
// final-review finding 4：tools/response_format 的原始子对象保留
// ============================================================================

// TestIRBuildOpenAIRequestBodyPreservesToolStrict 冻结「IR 无法表达的字段
// 从 Raw 原样保留」：function.strict 与 json_schema.name/strict 必须在
// 重新编码后仍然存在，否则 strict 约束会在中继时被静默丢弃。
func TestIRBuildOpenAIRequestBodyPreservesToolStrict(t *testing.T) {
	const raw = `{
	  "model": "gpt-4o",
	  "messages": [{"role":"user","content":"hi"}],
	  "tools": [{"type":"function","function":{
	    "name":"lookup","description":"d","strict":true,
	    "parameters":{"type":"object","properties":{"q":{"type":"string"}}}
	  }}],
	  "response_format": {"type":"json_schema","json_schema":{
	    "name":"my_schema","strict":true,"description":"schema-desc",
	    "schema":{"type":"object","properties":{"answer":{"type":"string"}}}
	  }}
	}`
	req, err := ParseOpenAIRequest([]byte(raw))
	if err != nil {
		t.Fatalf("ParseOpenAIRequest: %v", err)
	}
	body, err := BuildOpenAIRequestBody(req)
	if err != nil {
		t.Fatalf("BuildOpenAIRequestBody: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("unmarshal body: %v（body=%s）", err, body)
	}

	tools, _ := obj["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v", obj["tools"])
	}
	function, _ := tools[0].(map[string]any)["function"].(map[string]any)
	if function["strict"] != true {
		t.Fatalf("function.strict = %v，期望 true（body=%s）", function["strict"], body)
	}
	if function["name"] != "lookup" || function["description"] != "d" {
		t.Fatalf("归一化字段被破坏：%v", function)
	}
	params, _ := function["parameters"].(map[string]any)
	if params["type"] != "object" {
		t.Fatalf("parameters = %v", function["parameters"])
	}

	format, _ := obj["response_format"].(map[string]any)
	if format["type"] != "json_schema" {
		t.Fatalf("response_format.type = %v", format["type"])
	}
	jsonSchema, _ := format["json_schema"].(map[string]any)
	if jsonSchema["name"] != "my_schema" {
		t.Fatalf("json_schema.name = %v，期望保留客户端 my_schema", jsonSchema["name"])
	}
	if jsonSchema["strict"] != true {
		t.Fatalf("json_schema.strict = %v，期望 true", jsonSchema["strict"])
	}
	if jsonSchema["description"] != "schema-desc" {
		t.Fatalf("json_schema.description = %v，期望保留", jsonSchema["description"])
	}
	schema, _ := jsonSchema["schema"].(map[string]any)
	if schema["type"] != "object" {
		t.Fatalf("json_schema.schema = %v", jsonSchema["schema"])
	}

	// 二次 round-trip：保留字段不得在再解析/再编码后退化。
	second, err := ParseOpenAIRequest(body)
	if err != nil {
		t.Fatalf("re-parse built body: %v", err)
	}
	again, err := BuildOpenAIRequestBody(second)
	if err != nil {
		t.Fatalf("rebuild body: %v", err)
	}
	if !strings.Contains(string(again), `"strict":true`) || !strings.Contains(string(again), `"name":"my_schema"`) {
		t.Fatalf("二次 round-trip 丢失 strict/name：%s", again)
	}
}

// TestIRBuildOpenAIRequestBodyPreservesMaxCompletionTokens 冻结 finding 7：
// 客户端只发 max_completion_tokens 时不得改写为 max_tokens（新模型会 400）。
func TestIRBuildOpenAIRequestBodyPreservesMaxCompletionTokens(t *testing.T) {
	const raw = `{"model":"gpt-5","messages":[{"role":"user","content":"hi"}],"max_completion_tokens":128}`
	req, err := ParseOpenAIRequest([]byte(raw))
	if err != nil {
		t.Fatalf("ParseOpenAIRequest: %v", err)
	}
	if req.MaxTokens == nil || *req.MaxTokens != 128 {
		t.Fatalf("MaxTokens = %v，期望 128", req.MaxTokens)
	}
	body, err := BuildOpenAIRequestBody(req)
	if err != nil {
		t.Fatalf("BuildOpenAIRequestBody: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if got, ok := obj["max_completion_tokens"]; !ok || got != float64(128) {
		t.Fatalf("max_completion_tokens = %v，期望 128（body=%s）", obj["max_completion_tokens"], body)
	}
	if _, ok := obj["max_tokens"]; ok {
		t.Fatalf("不得同时下发 max_tokens（body=%s）", body)
	}

	// 客户端用 max_tokens 时保持旧行为（max_tokens，删除 max_completion_tokens）。
	req, err = ParseOpenAIRequest([]byte(`{"model":"m","messages":[],"max_tokens":64,"max_completion_tokens":32}`))
	if err != nil {
		t.Fatalf("ParseOpenAIRequest: %v", err)
	}
	body, err = BuildOpenAIRequestBody(req)
	if err != nil {
		t.Fatalf("BuildOpenAIRequestBody: %v", err)
	}
	obj = map[string]any{}
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if obj["max_tokens"] != float64(64) {
		t.Fatalf("max_tokens = %v，期望 64（max_tokens 优先）", obj["max_tokens"])
	}
	if _, ok := obj["max_completion_tokens"]; ok {
		t.Fatalf("max_tokens 已归一化时不得保留 max_completion_tokens（body=%s）", body)
	}
}

// TestIRParseOpenAIRequestRejectsUnencodableToolMessages 冻结 finding 8：
// 解析期必须拒绝编码器无法编码的形态，避免客户端参数错误变成上游构造失败（502）。
func TestIRParseOpenAIRequestRejectsUnencodableToolMessages(t *testing.T) {
	cases := map[string]string{
		"tool message missing tool_call_id": `{"model":"m","messages":[{"role":"tool","content":"result"}]}`,
		"tool call missing function name":   `{"model":"m","messages":[{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"arguments":"{}"}}]}]}`,
		"function call missing name":        `{"model":"m","messages":[{"role":"assistant","content":null,"function_call":{"arguments":"{}"}}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseOpenAIRequest([]byte(body))
			if err == nil {
				t.Fatal("want ErrParamInvalid, got nil")
			}
			if !errors.Is(err, ErrParamInvalid) {
				t.Fatalf("error = %v，期望 ErrParamInvalid-class", err)
			}
		})
	}

	// 合法形态仍必须解析+编码成功（校验不得误伤）。
	valid := `{"model":"m","messages":[
	  {"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},
	  {"role":"tool","tool_call_id":"call_1","content":"ok"}
	]}`
	req, err := ParseOpenAIRequest([]byte(valid))
	if err != nil {
		t.Fatalf("合法 tool 消息解析失败：%v", err)
	}
	if _, err := BuildOpenAIRequestBody(req); err != nil {
		t.Fatalf("合法 tool 消息编码失败：%v", err)
	}
}
