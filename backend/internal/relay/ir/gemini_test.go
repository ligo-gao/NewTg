package ir

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"newtg/gateway/internal/contract"
)

// geminiMultiTurnRequest 覆盖 systemInstruction + 多轮 user/model 交替
// + functionCall/functionResponse + inlineData 图片 + 工具声明/工具配置。
const geminiMultiTurnRequest = `{
  "systemInstruction": {"parts": [{"text": "be brief"}]},
  "contents": [
    {"role": "user", "parts": [
      {"text": "describe"},
      {"inlineData": {"mimeType": "image/png", "data": "aGVsbG8="}}
    ]},
    {"role": "model", "parts": [
      {"text": "checking"},
      {"functionCall": {"name": "lookup", "args": {"q": "x"}}}
    ]},
    {"role": "user", "parts": [
      {"functionResponse": {"name": "lookup", "response": {"result": "found"}}}
    ]}
  ],
  "tools": [{"functionDeclarations": [
    {"name": "lookup", "description": "d", "parameters": {"type": "object"}}
  ]}],
  "toolConfig": {"functionCallingConfig": {"mode": "ANY", "allowedFunctionNames": ["lookup"]}},
  "generationConfig": {
    "maxOutputTokens": 128,
    "temperature": 0.3,
    "topP": 0.8,
    "stopSequences": ["END"],
    "seed": 7,
    "responseMimeType": "application/json",
    "responseSchema": {"type": "object"}
  }
}`

func TestGeminiRequestMultiTurnMapping(t *testing.T) {
	req, err := ParseGeminiRequest([]byte(geminiMultiTurnRequest))
	if err != nil {
		t.Fatalf("ParseGeminiRequest: %v", err)
	}
	if req.RawFormat != FormatGeminiGenerateContent || string(req.Raw) != geminiMultiTurnRequest {
		t.Fatalf("RawFormat/Raw 未保留：%q", req.RawFormat)
	}
	if len(req.Messages) != 4 {
		t.Fatalf("messages = %d, want 4（system + user + model + user）", len(req.Messages))
	}
	if req.Messages[0].Role != contract.RoleSystem || req.Messages[0].Content[0].Text != "be brief" {
		t.Fatalf("systemInstruction 映射错误：%+v", req.Messages[0])
	}
	user := req.Messages[1]
	if user.Role != contract.RoleUser || len(user.Content) != 2 ||
		user.Content[1].Kind != contract.PartImageB64 || string(user.Content[1].Payload) != "hello" {
		t.Fatalf("user parts 映射错误：%+v", user)
	}
	model := req.Messages[2]
	if model.Role != contract.RoleAssistant || len(model.Content) != 2 || model.Content[1].Kind != contract.PartToolCall {
		t.Fatalf("model functionCall 映射错误：%+v", model)
	}
	if metaString(model.Content[1].Meta, "name") != "lookup" {
		t.Fatalf("functionCall name = %+v", model.Content[1].Meta)
	}
	toolResult := req.Messages[3]
	if toolResult.Role != contract.RoleUser || toolResult.Content[0].Kind != contract.PartToolResult ||
		metaString(toolResult.Content[0].Meta, "name") != "lookup" {
		t.Fatalf("functionResponse 映射错误：%+v", toolResult)
	}
	if !strings.Contains(toolResult.Content[0].Text, "found") {
		t.Fatalf("functionResponse.response 未保留：%q", toolResult.Content[0].Text)
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != "lookup" {
		t.Fatalf("tools = %+v", req.Tools)
	}
	if req.ToolChoice.Mode != contract.ToolChoiceSpecific || req.ToolChoice.Name != "lookup" {
		t.Fatalf("toolConfig ANY+单名应归一为 specific：%+v", req.ToolChoice)
	}
	if req.MaxTokens == nil || *req.MaxTokens != 128 || req.Temperature == nil || *req.Temperature != 0.3 {
		t.Fatalf("generationConfig 标量映射错误：%+v", req)
	}
	if req.ResponseFormat == nil || req.ResponseFormat.Kind != contract.ResponseFormatJSONSchema {
		t.Fatalf("responseMimeType+responseSchema 应映射为 json_schema：%+v", req.ResponseFormat)
	}

	// 回写：角色必须是 user/model 交替，工具回包落 user。
	encoded, err := BuildGeminiRequestBody(req)
	if err != nil {
		t.Fatalf("BuildGeminiRequestBody: %v", err)
	}
	var built map[string]any
	if err := json.Unmarshal(encoded, &built); err != nil {
		t.Fatal(err)
	}
	if _, exists := built["model"]; exists {
		t.Fatal("Gemini 请求体不得包含 model（模型在 URL）")
	}
	contents, _ := built["contents"].([]any)
	if len(contents) != 3 {
		t.Fatalf("回写 contents = %d, want 3", len(contents))
	}
	roles := make([]string, 0, len(contents))
	for _, item := range contents {
		obj, _ := item.(map[string]any)
		role, _ := obj["role"].(string)
		roles = append(roles, role)
	}
	if strings.Join(roles, ",") != "user,model,user" {
		t.Fatalf("角色序列 = %v, want [user model user]", roles)
	}
	last, _ := contents[2].(map[string]any)
	lastParts, _ := last["parts"].([]any)
	fnResponse, _ := lastParts[0].(map[string]any)
	inner, _ := fnResponse["functionResponse"].(map[string]any)
	if inner["name"] != "lookup" {
		t.Fatalf("functionResponse 回写缺少 name：%v", fnResponse)
	}
	if _, ok := inner["response"].(map[string]any); !ok {
		t.Fatalf("functionResponse.response 必须是对象：%T", inner["response"])
	}
}

func TestGeminiUnknownPartRejected(t *testing.T) {
	cases := map[string]string{
		"executableCode": `{"contents":[{"role":"user","parts":[{"executableCode":{"language":"PYTHON","code":"x"}}]}]}`,
		"无载荷":            `{"contents":[{"role":"user","parts":[{}]}]}`,
		"多载荷":            `{"contents":[{"role":"user","parts":[{"text":"a","inlineData":{"mimeType":"image/png","data":"eA=="}}]}]}`,
	}
	for name, body := range cases {
		if _, err := ParseGeminiRequest([]byte(body)); !errors.Is(err, ErrParamInvalid) {
			t.Fatalf("%s: 未知/非法 part 必须报 ErrParamInvalid，得到 %v", name, err)
		}
	}
}

func TestGeminiUnsupportedToolRejected(t *testing.T) {
	body := `{"contents":[{"role":"user","parts":[{"text":"x"}]}],"tools":[{"googleSearch":{}}]}`
	if _, err := ParseGeminiRequest([]byte(body)); !errors.Is(err, ErrParamInvalid) {
		t.Fatalf("内建工具没有 IR 承载位，必须显式拒绝，得到 %v", err)
	}
}

func TestGeminiBuildResolvesToolResultNameFromCallID(t *testing.T) {
	// 模拟 OpenAI 入站经 IR 转到 Gemini：tool 消息只有 tool_call_id，
	// 编码器必须从前面的 assistant tool_call 找到工具名。
	req := &contract.Request{
		Model: "gemini-2.5-flash",
		Messages: []contract.Message{
			{Role: contract.RoleUser, Content: []contract.ContentPart{{Kind: contract.PartText, Text: "hi"}}},
			{Role: contract.RoleAssistant, Content: []contract.ContentPart{{
				Kind: contract.PartToolCall,
				Text: `{"q":"x"}`,
				Meta: map[string]any{"id": "call_1", "name": "lookup"},
			}}},
			{Role: contract.RoleTool, Content: []contract.ContentPart{{
				Kind: contract.PartToolResult,
				Text: "result",
				Meta: map[string]any{"tool_call_id": "call_1"},
			}}},
		},
	}
	encoded, err := BuildGeminiRequestBody(req)
	if err != nil {
		t.Fatalf("BuildGeminiRequestBody: %v", err)
	}
	var built map[string]any
	if err := json.Unmarshal(encoded, &built); err != nil {
		t.Fatal(err)
	}
	contents, _ := built["contents"].([]any)
	last, _ := contents[len(contents)-1].(map[string]any)
	parts, _ := last["parts"].([]any)
	item, _ := parts[0].(map[string]any)
	response, _ := item["functionResponse"].(map[string]any)
	if response["name"] != "lookup" {
		t.Fatalf("应从调用 ID 解析出工具名 lookup，得到 %v", response)
	}
	if result, _ := response["response"].(map[string]any); result["result"] != "result" {
		t.Fatalf("非 JSON 文本工具结果应包成 {result: ...}：%v", response["response"])
	}
}

func TestGeminiUsageParse(t *testing.T) {
	usage := ParseGeminiUsage([]byte(`{
	  "promptTokenCount": 10,
	  "toolUsePromptTokenCount": 2,
	  "candidatesTokenCount": 5,
	  "thoughtsTokenCount": 3,
	  "totalTokenCount": 20,
	  "cachedContentTokenCount": 4,
	  "promptTokensDetails": [
	    {"modality": "TEXT", "tokenCount": 8},
	    {"modality": "IMAGE", "tokenCount": 4}
	  ],
	  "candidatesTokensDetails": [{"modality": "TEXT", "tokenCount": 8}]
	}`))
	if usage.Prompt != 12 || usage.Completion != 8 || usage.Total != 20 {
		t.Fatalf("prompt/completion/total = %d/%d/%d, want 12/8/20", usage.Prompt, usage.Completion, usage.Total)
	}
	if usage.CacheRead != 4 || usage.Reasoning != 3 {
		t.Fatalf("cache/reasoning = %d/%d", usage.CacheRead, usage.Reasoning)
	}
	if usage.PromptIncludesCache {
		t.Fatal("Gemini 语义：promptTokenCount 不含缓存，PromptIncludesCache 必须为 false")
	}
	if !usage.ReasoningIncludedInCompletion {
		t.Fatal("Gemini 语义：thoughts 计入 completion，ReasoningIncludedInCompletion 必须为 true")
	}
	if usage.PromptDetail.Image != 4 || usage.PromptDetail.Text != 8 || usage.PromptDetail.Audio != 0 {
		t.Fatalf("PromptDetail = %+v", usage.PromptDetail)
	}
	if usage.CompletionDetail.Text != 8 {
		t.Fatalf("CompletionDetail = %+v", usage.CompletionDetail)
	}
	// Raw 必须保留 Gemini 原键；不得伪装成 Claude/OpenAI 的键。
	if _, ok := usage.Raw["cachedContentTokenCount"]; !ok {
		t.Fatalf("Raw 必须保留 cachedContentTokenCount：%v", usage.Raw)
	}
	for _, forbidden := range []string{"input_tokens", "output_tokens", "cache_read_input_tokens"} {
		if _, ok := usage.Raw[forbidden]; ok {
			t.Fatalf("Gemini usage 不得伪装成 Claude 键 %q", forbidden)
		}
	}
}

func TestGeminiUsageEmptyIsEstimated(t *testing.T) {
	usage := ParseGeminiUsage(nil)
	if usage.Source != contract.UsageEstimated {
		t.Fatalf("空 usageMetadata 必须标记 estimated，得到 %v", usage.Source)
	}
	if usage.PromptIncludesCache || !usage.ReasoningIncludedInCompletion {
		t.Fatal("关系布尔是协议属性，空 usage 也必须显式赋值")
	}
}

func TestGeminiResponseParse(t *testing.T) {
	body := []byte(`{
	  "responseId": "resp_1",
	  "modelVersion": "gemini-2.5-flash",
	  "candidates": [{
	    "index": 0,
	    "content": {"role": "model", "parts": [
	      {"text": "hello"},
	      {"functionCall": {"name": "lookup", "args": {"q": "x"}}}
	    ]},
	    "finishReason": "MAX_TOKENS"
	  }],
	  "usageMetadata": {"promptTokenCount": 3, "candidatesTokenCount": 4, "totalTokenCount": 7}
	}`)
	resp, usage, err := ParseGeminiResponse(body)
	if err != nil {
		t.Fatalf("ParseGeminiResponse: %v", err)
	}
	if resp.ID != "resp_1" || resp.Model != "gemini-2.5-flash" {
		t.Fatalf("id/model = %q/%q", resp.ID, resp.Model)
	}
	if resp.FinishReason != "length" || len(resp.Choices) != 1 {
		t.Fatalf("finish_reason = %q, choices = %d", resp.FinishReason, len(resp.Choices))
	}
	content := resp.Choices[0].Message.Content
	if len(content) != 2 || content[0].Text != "hello" || content[1].Kind != contract.PartToolCall {
		t.Fatalf("content = %+v", content)
	}
	if usage.Prompt != 3 || usage.Completion != 4 || usage.Total != 7 {
		t.Fatalf("usage = %+v", usage)
	}
	if string(resp.Raw) != string(body) {
		t.Fatal("Response.Raw 必须保留上游原始响应体")
	}
}

func TestGeminiResponseBlockedPrompt(t *testing.T) {
	body := []byte(`{"promptFeedback":{"blockReason":"SAFETY"},"candidates":[]}`)
	resp, _, err := ParseGeminiResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.FinishReason != "content_filter" {
		t.Fatalf("无条件候选且被拦截时 finish_reason = %q, want content_filter", resp.FinishReason)
	}
}

// TestGeminiGenerationConfigPassthrough 覆盖复审 P1：generationConfig 中
// 未建模字段（topK / candidateCount / presencePenalty / thinkingConfig /
// responseModalities）必须原样存活。
func TestGeminiGenerationConfigPassthrough(t *testing.T) {
	body := `{
	  "contents":[{"role":"user","parts":[{"text":"hi"}]}],
	  "generationConfig":{
	    "maxOutputTokens":64,
	    "temperature":0.4,
	    "topK":40,
	    "candidateCount":2,
	    "presencePenalty":0.5,
	    "frequencyPenalty":0.25,
	    "thinkingConfig":{"thinkingBudget":1024,"includeThoughts":true},
	    "responseModalities":["TEXT","IMAGE"]
	  }
	}`
	req, err := ParseGeminiRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := BuildGeminiRequestBody(req)
	if err != nil {
		t.Fatal(err)
	}
	var built map[string]any
	if err := json.Unmarshal(encoded, &built); err != nil {
		t.Fatal(err)
	}
	config, _ := built["generationConfig"].(map[string]any)
	for _, key := range []string{"topK", "candidateCount", "presencePenalty", "frequencyPenalty"} {
		if _, ok := config[key]; !ok {
			t.Fatalf("generationConfig.%s 必须透传：%v", key, config)
		}
	}
	if _, ok := config["thinkingConfig"].(map[string]any); !ok {
		t.Fatalf("thinkingConfig 必须透传：%v", config)
	}
	if modalities, _ := config["responseModalities"].([]any); len(modalities) != 2 {
		t.Fatalf("responseModalities 必须透传：%v", config["responseModalities"])
	}
	if config["maxOutputTokens"] != float64(64) || config["temperature"] != 0.4 {
		t.Fatalf("已建模字段被覆盖错误：%v", config)
	}
}

// TestGeminiThoughtSignatureRoundTrip 覆盖复审 P2：functionCall /
// functionResponse 上的 thoughtSignature 必须随 part 回写。
func TestGeminiThoughtSignatureRoundTrip(t *testing.T) {
	body := `{
	  "contents":[
	    {"role":"model","parts":[
	      {"functionCall":{"name":"lookup","args":{"q":"x"}},"thoughtSignature":"sig-call"}
	    ]},
	    {"role":"user","parts":[
	      {"functionResponse":{"name":"lookup","response":{"result":"ok"}},"thoughtSignature":"sig-resp"}
	    ]}
	  ]
	}`
	req, err := ParseGeminiRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if metaString(req.Messages[0].Content[0].Meta, "thought_signature") != "sig-call" {
		t.Fatalf("functionCall thoughtSignature 未保留：%+v", req.Messages[0].Content[0].Meta)
	}
	if metaString(req.Messages[1].Content[0].Meta, "thought_signature") != "sig-resp" {
		t.Fatalf("functionResponse thoughtSignature 未保留：%+v", req.Messages[1].Content[0].Meta)
	}
	encoded, err := BuildGeminiRequestBody(req)
	if err != nil {
		t.Fatal(err)
	}
	var built map[string]any
	if err := json.Unmarshal(encoded, &built); err != nil {
		t.Fatal(err)
	}
	contents, _ := built["contents"].([]any)
	first, _ := contents[0].(map[string]any)
	firstParts, _ := first["parts"].([]any)
	firstPart, _ := firstParts[0].(map[string]any)
	if firstPart["thoughtSignature"] != "sig-call" {
		t.Fatalf("functionCall thoughtSignature 回写丢失：%v", firstPart)
	}
	if _, ok := firstPart["functionCall"].(map[string]any); !ok {
		t.Fatalf("functionCall 载荷丢失：%v", firstPart)
	}
	second, _ := contents[1].(map[string]any)
	secondParts, _ := second["parts"].([]any)
	secondPart, _ := secondParts[0].(map[string]any)
	if secondPart["thoughtSignature"] != "sig-resp" {
		t.Fatalf("functionResponse thoughtSignature 回写丢失：%v", secondPart)
	}
}

// TestGeminiThoughtDoesNotBecomeAnthropicThinking 是复审 N1 的回归：
// Gemini 推理 part 与 Anthropic thinking 共用 Meta["thought"]，但跨协议
// 编码到 Anthropic 时绝不能生成没有 signature 的 thinking 块。
func TestGeminiThoughtDoesNotBecomeAnthropicThinking(t *testing.T) {
	body := `{"contents":[{"role":"user","parts":[
	  {"text":"question"},
	  {"thought":true,"text":"gemini reasoning"}
	]}]}`
	req, err := ParseGeminiRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := BuildAnthropicRequestBody(req)
	if err != nil {
		t.Fatalf("Gemini→Anthropic 编码失败：%v", err)
	}
	var built map[string]any
	if err := json.Unmarshal(encoded, &built); err != nil {
		t.Fatal(err)
	}
	messages, _ := built["messages"].([]any)
	msg, _ := messages[0].(map[string]any)
	blocks, _ := msg["content"].([]any)
	for _, item := range blocks {
		block, _ := item.(map[string]any)
		if block["type"] == "thinking" {
			t.Fatalf("Gemini reasoning 不得变成 Anthropic thinking 块：%v", block)
		}
	}
	last, _ := blocks[len(blocks)-1].(map[string]any)
	if last["type"] != "text" || last["text"] != "gemini reasoning" {
		t.Fatalf("Gemini reasoning 应作为普通 text 保留：%v", last)
	}
}

// TestGeminiThoughtExcludedFromOpenAIContent 是终验 H1 的回归：Gemini 2.5
// 非流式响应里的 CoT 不得混入可见 content。
func TestGeminiThoughtExcludedFromOpenAIContent(t *testing.T) {
	body := []byte(`{"responseId":"r","modelVersion":"gemini-2.5-flash","candidates":[{
	  "index":0,
	  "content":{"role":"model","parts":[{"thought":true,"text":"internal cot"},{"text":"answer"}]},
	  "finishReason":"STOP"
	}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":2,"totalTokenCount":3}}`)
	resp, _, err := ParseGeminiResponse(body)
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
		t.Fatalf("content 不得包含 CoT：%v", message["content"])
	}
	if message["reasoning_content"] != "internal cot" {
		t.Fatalf("CoT 必须走 reasoning_content：%v", message)
	}
}

// TestGeminiResponseUnknownPartPreserved 是终验 H3 的回归：未知 part 不得
// 让有效 200 响应变成 502，原始 part 保留到 Extensions。
func TestGeminiResponseUnknownPartPreserved(t *testing.T) {
	body := []byte(`{"responseId":"r","candidates":[{"index":0,"content":{"role":"model","parts":[
	  {"codeExecutionResult":{"outcome":"OUTCOME_OK","output":"1"}},
	  {"text":"ok"}
	]},"finishReason":"STOP"}]}`)
	resp, _, err := ParseGeminiResponse(body)
	if err != nil {
		t.Fatalf("未知响应 part 必须保留而不是报错：%v", err)
	}
	message := resp.Choices[0].Message
	if len(message.Content) != 1 || message.Content[0].Text != "ok" {
		t.Fatalf("已建模内容映射错误：%+v", message.Content)
	}
	unmapped, ok := message.Extensions["unmapped_blocks"].([]any)
	if !ok || len(unmapped) != 1 {
		t.Fatalf("未知 part 必须保留在 Extensions：%+v", message.Extensions)
	}

	// 已建模 part 的字段错误仍然拒绝（不能把坏 base64 当未知类型）。
	bad := []byte(`{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"!!!"}}]}}]}`)
	if _, _, err := ParseGeminiResponse(bad); err == nil {
		t.Fatal("已建模 part 的非法字段必须报错")
	}
}

// TestGeminiUsageUnknownObjectIsEstimated 是终验 M5 的回归：非空但无
// 已识别用量键的对象不能标记 upstream_exact。
func TestGeminiUsageUnknownObjectIsEstimated(t *testing.T) {
	usage := ParseGeminiUsage([]byte(`{"foo":1}`))
	if usage.Source != contract.UsageEstimated {
		t.Fatalf("未知键 usage 应标记 estimated，得到 %v", usage.Source)
	}
}

// TestGeminiToolDeclarationExtrasPreserved 是终验 L6 的回归：声明级未建模
// 字段必须随往返保留。
func TestGeminiToolDeclarationExtrasPreserved(t *testing.T) {
	body := `{
	  "contents":[{"role":"user","parts":[{"text":"hi"}]}],
	  "tools":[{"functionDeclarations":[{
	    "name":"lookup","description":"d","parameters":{"type":"object"},
	    "response":{"type":"object","properties":{"x":{"type":"string"}}},
	    "behavior":"BLOCKING"
	  }]}]
	}`
	req, err := ParseGeminiRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := BuildGeminiRequestBody(req)
	if err != nil {
		t.Fatal(err)
	}
	var built map[string]any
	if err := json.Unmarshal(encoded, &built); err != nil {
		t.Fatal(err)
	}
	tools, _ := built["tools"].([]any)
	group, _ := tools[0].(map[string]any)
	declarations, _ := group["functionDeclarations"].([]any)
	declaration, _ := declarations[0].(map[string]any)
	if _, ok := declaration["response"].(map[string]any); !ok {
		t.Fatalf("声明级 response 字段丢失：%v", declaration)
	}
	if declaration["behavior"] != "BLOCKING" {
		t.Fatalf("声明级 behavior 字段丢失：%v", declaration)
	}
	if declaration["name"] != "lookup" {
		t.Fatalf("已建模字段错误：%v", declaration)
	}
}

// TestGeminiMixedBuiltinToolRejected 是终验 L6 的回归：内建工具与
// functionDeclarations 混在同一组时必须显式拒绝，不能只取后者。
func TestGeminiMixedBuiltinToolRejected(t *testing.T) {
	body := `{"contents":[{"role":"user","parts":[{"text":"x"}]}],"tools":[{"googleSearch":{},"functionDeclarations":[{"name":"f"}]}]}`
	if _, err := ParseGeminiRequest([]byte(body)); !errors.Is(err, ErrParamInvalid) {
		t.Fatalf("混合内建工具必须报 ErrParamInvalid，得到 %v", err)
	}
}

func TestGeminiBuildFunctionCallArgsMustBeObject(t *testing.T) {
	req := &contract.Request{
		Model: "gemini",
		Messages: []contract.Message{{Role: contract.RoleAssistant, Content: []contract.ContentPart{{
			Kind: contract.PartToolCall,
			Text: `[1,2]`,
			Meta: map[string]any{"name": "lookup"},
		}}}},
	}
	if _, err := BuildGeminiRequestBody(req); !errors.Is(err, ErrParamInvalid) {
		t.Fatalf("非对象参数必须报 ErrParamInvalid，得到 %v", err)
	}
}
