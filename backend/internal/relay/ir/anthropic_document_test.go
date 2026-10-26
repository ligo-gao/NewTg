package ir

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// anthropicDocumentRequest 复刻 G-DOM-85 的线上形态：客户端把 PDF 以
// document 块塞进 user 消息（source.type=base64）。
const anthropicDocumentRequest = `{
  "model": "deepseek-flash",
  "max_tokens": 16,
  "messages": [
    {"role": "user", "content": [
      {"type": "text", "text": "总结这份合同"},
      {"type": "document",
       "source": {"type": "base64", "media_type": "application/pdf", "data": "JVBERi0xLjQK"},
       "title": "contract",
       "cache_control": {"type": "ephemeral"}}
    ]}
  ]
}`

// anthropicServerToolRequest 是开启 Anthropic 服务端工具后历史回放的形态：
// assistant 带 server_tool_use，user 带配对的 web_search_tool_result。
const anthropicServerToolRequest = `{
  "model": "claude-sonnet-4-5",
  "max_tokens": 16,
  "messages": [
    {"role": "user", "content": "搜一下天气"},
    {"role": "assistant", "content": [
      {"type": "text", "text": "我来搜索"},
      {"type": "server_tool_use", "id": "srvtoolu_1", "name": "web_search", "input": {"query": "天气"}}
    ]},
    {"role": "user", "content": [
      {"type": "web_search_tool_result", "tool_use_id": "srvtoolu_1", "content": [
        {"type": "web_search_result", "title": "天气", "url": "https://example.com", "encrypted_content": "enc-1"}
      ]}
    ]}
  ]
}`

// TestAnthropicDocumentBlocksRoundTrip 锁定 document 块的解析与同协议回写：
// 原始块（含 source/title/cache_control）必须逐字节保留，不能被当成正文。
func TestAnthropicDocumentBlocksRoundTrip(t *testing.T) {
	req, err := ParseAnthropicRequest([]byte(anthropicDocumentRequest))
	if err != nil {
		t.Fatalf("ParseAnthropicRequest: %v", err)
	}
	if len(req.Messages) != 1 || len(req.Messages[0].Content) != 2 {
		t.Fatalf("消息结构异常：%+v", req.Messages)
	}
	doc := req.Messages[0].Content[1]
	if !isAnthropicDocumentPart(doc) {
		t.Fatalf("document 未映射为文档部件：%+v", doc)
	}
	if doc.Text != "" {
		t.Fatalf("二进制文档不得冒充正文 text：%q", doc.Text)
	}
	if doc.Meta["mime_type"] != "application/pdf" {
		t.Fatalf("mime_type 未保留：%v", doc.Meta["mime_type"])
	}

	encoded, err := BuildAnthropicRequestBody(req)
	if err != nil {
		t.Fatalf("BuildAnthropicRequestBody: %v", err)
	}
	var built map[string]any
	if err := json.Unmarshal(encoded, &built); err != nil {
		t.Fatal(err)
	}
	messages, _ := built["messages"].([]any)
	blocks, _ := messages[0].(map[string]any)["content"].([]any)
	block, _ := blocks[1].(map[string]any)
	if block["type"] != "document" {
		t.Fatalf("回写类型 = %v，want document", block["type"])
	}
	source, _ := block["source"].(map[string]any)
	if source["type"] != "base64" || source["media_type"] != "application/pdf" || source["data"] != "JVBERi0xLjQK" {
		t.Fatalf("source 未保真：%v", source)
	}
	if block["title"] != "contract" {
		t.Fatalf("title 未保留：%v", block["title"])
	}
	if _, ok := block["cache_control"].(map[string]any); !ok {
		t.Fatalf("cache_control 未保留：%v", block["cache_control"])
	}

	// 二次解析确认往返稳定。
	if _, err := ParseAnthropicRequest(encoded); err != nil {
		t.Fatalf("往返后 ParseAnthropicRequest: %v", err)
	}
}

// TestAnthropicDocumentTextSourceProjectsText 锁定文本来源的投影：正文即
// 文档内容，跨协议降级时内容不会丢失。
func TestAnthropicDocumentTextSourceProjectsText(t *testing.T) {
	body := `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":[
	  {"type":"document","source":{"type":"text","media_type":"text/plain","data":"hello doc"}}]}]}`
	req, err := ParseAnthropicRequest([]byte(body))
	if err != nil {
		t.Fatalf("ParseAnthropicRequest: %v", err)
	}
	part := req.Messages[0].Content[0]
	if part.Text != "hello doc" || !isAnthropicDocumentPart(part) {
		t.Fatalf("文本文档投影错误：%+v", part)
	}
	encoded, err := BuildOpenAIRequestBody(req)
	if err != nil {
		t.Fatalf("BuildOpenAIRequestBody: %v", err)
	}
	if !strings.Contains(string(encoded), "hello doc") || strings.Contains(string(encoded), "document") {
		t.Fatalf("OpenAI 降级必须保留文档正文：%s", encoded)
	}
}

// TestAnthropicDocumentSourceShapeRejected 锁定最小形状校验：畸形文档、错误
// 位置以及缺少必填字段的服务端工具块都必须报参数错误。
func TestAnthropicDocumentSourceShapeRejected(t *testing.T) {
	cases := map[string]string{
		"缺少 source":              `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":[{"type":"document"}]}]}`,
		"未知 source.type":         `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":[{"type":"document","source":{"type":"nope"}}]}]}`,
		"base64 为空":              `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":[{"type":"document","source":{"type":"base64","media_type":"application/pdf"}}]}]}`,
		"base64 非法":              `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":[{"type":"document","source":{"type":"base64","data":"!!!"}}]}]}`,
		"url 为空":                 `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":[{"type":"document","source":{"type":"url","url":""}}]}]}`,
		"file 缺 file_id":         `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":[{"type":"document","source":{"type":"file"}}]}]}`,
		"顶层 system 文档":           `{"model":"m","max_tokens":1,"system":[{"type":"document","source":{"type":"text","data":"x"}}],"messages":[]}`,
		"system 消息文档":            `{"model":"m","max_tokens":1,"messages":[{"role":"system","content":[{"type":"document","source":{"type":"text","data":"x"}}]}]}`,
		"server_tool_use 缺 name": `{"model":"m","max_tokens":1,"messages":[{"role":"assistant","content":[{"type":"server_tool_use","id":"s"}]}]}`,
		"结果块缺 tool_use_id":       `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":[{"type":"web_search_tool_result","content":[]}]}]}`,
	}
	for name, body := range cases {
		_, err := ParseAnthropicRequest([]byte(body))
		if err == nil {
			t.Fatalf("%s: 必须报错", name)
		}
		if !errors.Is(err, ErrParamInvalid) {
			t.Fatalf("%s: 错误必须是 ErrParamInvalid，得到 %v", name, err)
		}
	}
}

// TestOpenAIBuildEncodesBase64DocumentAsFilePart 锁定 base64 文档的 OpenAI
// 降级：标准 file 部件 + data URI，索引类上游（如 OpenAI 官方）可直接读 PDF。
func TestOpenAIBuildEncodesBase64DocumentAsFilePart(t *testing.T) {
	req, err := ParseAnthropicRequest([]byte(anthropicDocumentRequest))
	if err != nil {
		t.Fatalf("ParseAnthropicRequest: %v", err)
	}
	encoded, err := BuildOpenAIRequestBody(req)
	if err != nil {
		t.Fatalf("BuildOpenAIRequestBody: %v", err)
	}
	var built map[string]any
	if err := json.Unmarshal(encoded, &built); err != nil {
		t.Fatal(err)
	}
	messages, _ := built["messages"].([]any)
	content, _ := messages[0].(map[string]any)["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("content = %v, want 2 块", content)
	}
	filePart, _ := content[1].(map[string]any)
	if filePart["type"] != "file" {
		t.Fatalf("文档降级类型 = %v，want file", filePart["type"])
	}
	file, _ := filePart["file"].(map[string]any)
	if file["file_data"] != "data:application/pdf;base64,JVBERi0xLjQK" {
		t.Fatalf("file_data = %v", file["file_data"])
	}
	if file["filename"] != "contract.pdf" {
		t.Fatalf("filename = %v，want contract.pdf", file["filename"])
	}
	if strings.Contains(string(encoded), "document") {
		t.Fatalf("OpenAI 请求体不得出现 Anthropic 块类型：%s", encoded)
	}
}

// TestGeminiBuildEncodesBase64DocumentAsInlineData 锁定 Gemini 降级：
// inlineData 的 mimeType 保持 application/pdf。
func TestGeminiBuildEncodesBase64DocumentAsInlineData(t *testing.T) {
	req, err := ParseAnthropicRequest([]byte(anthropicDocumentRequest))
	if err != nil {
		t.Fatalf("ParseAnthropicRequest: %v", err)
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
	parts, _ := contents[0].(map[string]any)["parts"].([]any)
	inline, ok := parts[1].(map[string]any)["inlineData"].(map[string]any)
	if !ok {
		t.Fatalf("Gemini 文档降级必须是 inlineData：%v", parts[1])
	}
	if inline["mimeType"] != "application/pdf" || inline["data"] != "JVBERi0xLjQK" {
		t.Fatalf("inlineData = %v", inline)
	}
}

// TestDocumentUnsupportedSourceRejectedCrossProtocol 锁定没有承载位的来源：
// url / file_id 在 OpenAI/Gemini 上都显式报错，绝不静默丢弃用户内容。
func TestDocumentUnsupportedSourceRejectedCrossProtocol(t *testing.T) {
	cases := map[string]string{
		"url":  `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":[{"type":"document","source":{"type":"url","url":"https://example.com/a.pdf"}}]}]}`,
		"file": `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":[{"type":"document","source":{"type":"file","file_id":"file_1"}}]}]}`,
	}
	for name, body := range cases {
		req, err := ParseAnthropicRequest([]byte(body))
		if err != nil {
			t.Fatalf("%s: ParseAnthropicRequest: %v", name, err)
		}
		if _, err := BuildOpenAIRequestBody(req); !errors.Is(err, ErrParamInvalid) {
			t.Fatalf("%s: OpenAI 构建必须报 ErrParamInvalid，得到 %v", name, err)
		}
		if _, err := BuildGeminiRequestBody(req); !errors.Is(err, ErrParamInvalid) {
			t.Fatalf("%s: Gemini 构建必须报 ErrParamInvalid，得到 %v", name, err)
		}
		// 同协议上游必须仍然可回写。
		if _, err := BuildAnthropicRequestBody(req); err != nil {
			t.Fatalf("%s: Anthropic 回写失败：%v", name, err)
		}
	}
}

// TestAnthropicServerToolBlocksRoundTrip 锁定服务端工具块：server_tool_use /
// web_search_tool_result 必须在 Anthropic 上游原样往返（加密负载不得改写）。
func TestAnthropicServerToolBlocksRoundTrip(t *testing.T) {
	req, err := ParseAnthropicRequest([]byte(anthropicServerToolRequest))
	if err != nil {
		t.Fatalf("ParseAnthropicRequest: %v", err)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("messages = %d, want 3", len(req.Messages))
	}
	if !isAnthropicServerToolPart(req.Messages[1].Content[1]) {
		t.Fatalf("server_tool_use 未映射为控制部件：%+v", req.Messages[1].Content)
	}
	if !isAnthropicServerToolPart(req.Messages[2].Content[0]) {
		t.Fatalf("web_search_tool_result 未映射为控制部件：%+v", req.Messages[2].Content)
	}

	encoded, err := BuildAnthropicRequestBody(req)
	if err != nil {
		t.Fatalf("BuildAnthropicRequestBody: %v", err)
	}
	if !strings.Contains(string(encoded), `"encrypted_content":"enc-1"`) {
		t.Fatalf("服务端工具结果未被原样回写：%s", encoded)
	}
	if !strings.Contains(string(encoded), `"name":"web_search"`) {
		t.Fatalf("server_tool_use 未被原样回写：%s", encoded)
	}
	if _, err := ParseAnthropicRequest(encoded); err != nil {
		t.Fatalf("往返后 ParseAnthropicRequest: %v", err)
	}
}

// TestAnthropicMCPToolBlocksAccepted 锁定 MCP 连接器的块形态：mcp_tool_use /
// mcp_tool_result 与 server_tool_use 同族，不能被当成未知块 400。
func TestAnthropicMCPToolBlocksAccepted(t *testing.T) {
	body := `{"model":"m","max_tokens":1,"messages":[
	  {"role":"user","content":"查日程"},
	  {"role":"assistant","content":[{"type":"mcp_tool_use","id":"mcptoolu_1","name":"get_event","server_name":"cal","input":{}}]},
	  {"role":"user","content":[{"type":"mcp_tool_result","tool_use_id":"mcptoolu_1","is_error":false,"content":[{"type":"text","text":"ok"}]}]}
	]}`
	req, err := ParseAnthropicRequest([]byte(body))
	if err != nil {
		t.Fatalf("ParseAnthropicRequest: %v", err)
	}
	encoded, err := BuildAnthropicRequestBody(req)
	if err != nil {
		t.Fatalf("BuildAnthropicRequestBody: %v", err)
	}
	if !strings.Contains(string(encoded), "mcp_tool_use") || !strings.Contains(string(encoded), "mcp_tool_result") {
		t.Fatalf("MCP 工具块未回写：%s", encoded)
	}
}

// TestOpenAIBuildDropsServerToolBlocks 锁定跨协议降级：服务端工具块对其它
// 上游没有语义，跳过；只含控制块的消息整条丢弃，绝不能渲染成空 text。
func TestOpenAIBuildDropsServerToolBlocks(t *testing.T) {
	req, err := ParseAnthropicRequest([]byte(anthropicServerToolRequest))
	if err != nil {
		t.Fatalf("ParseAnthropicRequest: %v", err)
	}
	encoded, err := BuildOpenAIRequestBody(req)
	if err != nil {
		t.Fatalf("BuildOpenAIRequestBody: %v", err)
	}
	if strings.Contains(string(encoded), "server_tool_use") || strings.Contains(string(encoded), "web_search_tool_result") {
		t.Fatalf("OpenAI 请求体不得出现服务端工具块：%s", encoded)
	}
	var built map[string]any
	if err := json.Unmarshal(encoded, &built); err != nil {
		t.Fatal(err)
	}
	messages, _ := built["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("messages = %d, want 2（只含结果块的消息整条丢弃）", len(messages))
	}
	assistant, _ := messages[1].(map[string]any)
	if assistant["role"] != "assistant" || assistant["content"] != "我来搜索" {
		t.Fatalf("可见正文丢失：%v", assistant)
	}
}

// TestGeminiBuildSkipsServerToolBlocks 锁定 Gemini 路径同样跳过控制块且
// 不产生空 content。
func TestGeminiBuildSkipsServerToolBlocks(t *testing.T) {
	req, err := ParseAnthropicRequest([]byte(anthropicServerToolRequest))
	if err != nil {
		t.Fatalf("ParseAnthropicRequest: %v", err)
	}
	encoded, err := BuildGeminiRequestBody(req)
	if err != nil {
		t.Fatalf("BuildGeminiRequestBody: %v", err)
	}
	if strings.Contains(string(encoded), "server_tool_use") || strings.Contains(string(encoded), "web_search_tool_result") {
		t.Fatalf("Gemini 请求体不得出现服务端工具块：%s", encoded)
	}
	var built map[string]any
	if err := json.Unmarshal(encoded, &built); err != nil {
		t.Fatal(err)
	}
	contents, _ := built["contents"].([]any)
	if len(contents) != 2 {
		t.Fatalf("contents = %d, want 2", len(contents))
	}
}

// TestToolResultContentAcceptsDocument 锁定工具结果里夹带文档的形态：
// 原始块数组必须保留，不能因为嵌套类型未知而 400。
func TestToolResultContentAcceptsDocument(t *testing.T) {
	body := `{"model":"m","max_tokens":1,"messages":[
	  {"role":"assistant","content":[{"type":"tool_use","id":"tu_1","name":"fetch_docs","input":{}}]},
	  {"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":[
	    {"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBERi0xLjQK"}},
	    {"type":"search_result","source":"doc","title":"t","content":[{"type":"text","text":"hit"}]}
	  ]}]}
	]}`
	req, err := ParseAnthropicRequest([]byte(body))
	if err != nil {
		t.Fatalf("ParseAnthropicRequest: %v", err)
	}
	encoded, err := BuildAnthropicRequestBody(req)
	if err != nil {
		t.Fatalf("BuildAnthropicRequestBody: %v", err)
	}
	if !strings.Contains(string(encoded), "search_result") || !strings.Contains(string(encoded), "JVBERi0xLjQK") {
		t.Fatalf("工具结果里的文档/检索块未回写：%s", encoded)
	}
}
