package ir

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"newtg/gateway/internal/contract"
)

// anthropicToolChangeRequest 复刻线上失败形态：客户端在 messages[] 末尾追加
// 一条 system 消息，携带 Anthropic 的 tool_addition 控制块（beta
// mid-conversation-tool-changes-2026-07-01），被引用的工具已在 tools[] 中
// 以 defer_loading 声明。
const anthropicToolChangeRequest = `{
  "model": "deepseek-flash",
  "max_tokens": 16,
  "messages": [
    {"role": "user", "content": "hi"},
    {"role": "assistant", "content": "ok"},
    {"role": "user", "content": "more"},
    {"role": "system", "content": [
      {"type": "tool_addition", "tool": {"type": "tool_reference", "name": "get_forecast"}}
    ]}
  ],
  "tools": [
    {"name": "get_forecast", "description": "5-day forecast", "input_schema": {"type": "object"}, "defer_loading": true}
  ]
}`

// TestAnthropicToolChangeBlocksParseAndRoundTrip 锁定控制块的解析与同协议回写：
// 必须原位保留（不能被提升到顶层 system），且往返后类型/引用不变。
func TestAnthropicToolChangeBlocksParseAndRoundTrip(t *testing.T) {
	req, err := ParseAnthropicRequest([]byte(anthropicToolChangeRequest))
	if err != nil {
		t.Fatalf("ParseAnthropicRequest: %v", err)
	}
	last := req.Messages[len(req.Messages)-1]
	if last.Role != contract.RoleSystem {
		t.Fatalf("末尾消息角色 = %q，want system", last.Role)
	}
	if len(last.Content) != 1 || !isAnthropicToolChangePart(last.Content[0]) {
		t.Fatalf("tool_addition 未映射为控制部件：%+v", last.Content)
	}
	if last.Content[0].Text != "" {
		t.Fatalf("控制块不得冒充正文 text：%q", last.Content[0].Text)
	}

	encoded, err := BuildAnthropicRequestBody(req)
	if err != nil {
		t.Fatalf("BuildAnthropicRequestBody: %v", err)
	}
	var built map[string]any
	if err := json.Unmarshal(encoded, &built); err != nil {
		t.Fatal(err)
	}
	if system, ok := built["system"]; ok {
		t.Fatalf("messages[] 里的 system 消息不得被提升到顶层 system：%v", system)
	}
	messages, _ := built["messages"].([]any)
	if len(messages) != 4 {
		t.Fatalf("messages = %d, want 4（原位保留 system 消息）", len(messages))
	}
	lastOut, _ := messages[3].(map[string]any)
	if lastOut["role"] != "system" {
		t.Fatalf("末尾消息角色 = %v，want system", lastOut["role"])
	}
	blocks, _ := lastOut["content"].([]any)
	if len(blocks) != 1 {
		t.Fatalf("末尾 content = %v, want 1 块", lastOut["content"])
	}
	block, _ := blocks[0].(map[string]any)
	if block["type"] != "tool_addition" {
		t.Fatalf("控制块回写类型 = %v，want tool_addition", block["type"])
	}
	tool, _ := block["tool"].(map[string]any)
	if tool["type"] != "tool_reference" || tool["name"] != "get_forecast" {
		t.Fatalf("tool_reference 未保真：%v", block["tool"])
	}

	// 二次解析确认往返稳定。
	again, err := ParseAnthropicRequest(encoded)
	if err != nil {
		t.Fatalf("往返后 ParseAnthropicRequest: %v", err)
	}
	lastAgain := again.Messages[len(again.Messages)-1]
	if len(lastAgain.Content) != 1 || !isAnthropicToolChangePart(lastAgain.Content[0]) {
		t.Fatalf("往返后控制块丢失：%+v", lastAgain.Content)
	}
}

// TestAnthropicToolChangeBlockShapeRejected 锁定最小形状校验：畸形或位置错误的
// 控制块必须报参数错误，不能静默吞掉。
func TestAnthropicToolChangeBlockShapeRejected(t *testing.T) {
	cases := map[string]string{
		"缺少 tool":           `{"model":"m","max_tokens":1,"messages":[{"role":"system","content":[{"type":"tool_addition"}]}]}`,
		"未知 tool.type":      `{"model":"m","max_tokens":1,"messages":[{"role":"system","content":[{"type":"tool_addition","tool":{"type":"nope","name":"x"}}]}]}`,
		"引用缺少 name":         `{"model":"m","max_tokens":1,"messages":[{"role":"system","content":[{"type":"tool_addition","tool":{"type":"tool_reference"}}]}]}`,
		"tool_removal 按值移除": `{"model":"m","max_tokens":1,"messages":[{"role":"system","content":[{"type":"tool_removal","tool":{"type":"tool_definition","definition":{"name":"x"}}}]}]}`,
		"用户消息里的控制块":         `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":[{"type":"tool_addition","tool":{"type":"tool_reference","name":"x"}}]}]}`,
		"顶层 system 里的控制块":   `{"model":"m","max_tokens":1,"system":[{"type":"tool_addition","tool":{"type":"tool_reference","name":"x"}}],"messages":[]}`,
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

// TestOpenAIBuildDropsAnthropicToolChangeBlocks 锁定跨协议降级：OpenAI 兼容
// 上游没有「延迟加载工具」的概念，按引用添加的 tool_addition 是 no-op；
// 只含控制块的 system 消息整条丢弃，绝不能渲染成空 text 发给上游。
func TestOpenAIBuildDropsAnthropicToolChangeBlocks(t *testing.T) {
	req, err := ParseAnthropicRequest([]byte(anthropicToolChangeRequest))
	if err != nil {
		t.Fatalf("ParseAnthropicRequest: %v", err)
	}
	encoded, err := BuildOpenAIRequestBody(req)
	if err != nil {
		t.Fatalf("BuildOpenAIRequestBody: %v", err)
	}
	if strings.Contains(string(encoded), "tool_addition") {
		t.Fatalf("OpenAI 请求体不得出现 Anthropic 控制块：%s", encoded)
	}
	var built map[string]any
	if err := json.Unmarshal(encoded, &built); err != nil {
		t.Fatal(err)
	}
	messages, _ := built["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("messages = %d, want 3（控制块消息整条丢弃）", len(messages))
	}
	tools, _ := built["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("按引用添加不应改变工具集合，tools = %v", built["tools"])
	}
	item, _ := tools[0].(map[string]any)
	function, _ := item["function"].(map[string]any)
	if function["name"] != "get_forecast" {
		t.Fatalf("工具声明未保真：%v", item)
	}
}

// TestOpenAIBuildAppliesRemovalAndInlineDefinition 锁定可表达的两种控制：
// tool_removal 从 tools[] 剔除同名工具；按值的 tool_addition 把定义并入
// tools[]。
func TestOpenAIBuildAppliesRemovalAndInlineDefinition(t *testing.T) {
	body := `{
	  "model": "deepseek-flash",
	  "max_tokens": 16,
	  "messages": [
	    {"role": "user", "content": "hi"},
	    {"role": "system", "content": [
	      {"type": "tool_removal", "tool": {"type": "tool_reference", "name": "get_forecast"}},
	      {"type": "tool_addition", "tool": {"type": "tool_definition", "definition": {
	        "name": "inline_tool", "description": "added mid-conversation",
	        "input_schema": {"type": "object", "properties": {"q": {"type": "string"}}}
	      }}}
	    ]}
	  ],
	  "tools": [
	    {"name": "get_forecast", "input_schema": {"type": "object"}},
	    {"name": "keep_me", "input_schema": {"type": "object"}}
	  ]
	}`
	req, err := ParseAnthropicRequest([]byte(body))
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
	tools, _ := built["tools"].([]any)
	names := make([]string, 0, len(tools))
	for _, raw := range tools {
		item, _ := raw.(map[string]any)
		function, _ := item["function"].(map[string]any)
		name, _ := function["name"].(string)
		names = append(names, name)
	}
	if strings.Join(names, ",") != "keep_me,inline_tool" {
		t.Fatalf("工具集合 = %v, want [keep_me inline_tool]", names)
	}
}

// TestAnthropicToolChangeAcceptsMCPReferences 锁定 MCP 引用形态：配置了 MCP
// 服务器的客户端会用 mcp_tool_reference / mcp_toolset_reference 表达变更，
// 这些同样是合法块，不能重演同类 400；跨协议出站时整块丢弃。
func TestAnthropicToolChangeAcceptsMCPReferences(t *testing.T) {
	body := `{
	  "model": "m", "max_tokens": 1,
	  "messages": [
	    {"role": "user", "content": "hi"},
	    {"role": "system", "content": [
	      {"type": "tool_addition", "tool": {"type": "mcp_tool_reference", "server_name": "cal", "name": "get_event"}},
	      {"type": "tool_removal", "tool": {"type": "mcp_toolset_reference", "server_name": "cal"}}
	    ]}
	  ]
	}`
	req, err := ParseAnthropicRequest([]byte(body))
	if err != nil {
		t.Fatalf("ParseAnthropicRequest: %v", err)
	}
	encoded, err := BuildAnthropicRequestBody(req)
	if err != nil {
		t.Fatalf("BuildAnthropicRequestBody: %v", err)
	}
	if !strings.Contains(string(encoded), "mcp_toolset_reference") {
		t.Fatalf("Anthropic 回写缺少 MCP 控制块：%s", encoded)
	}

	encoded, err = BuildOpenAIRequestBody(req)
	if err != nil {
		t.Fatalf("BuildOpenAIRequestBody: %v", err)
	}
	if strings.Contains(string(encoded), "mcp_tool") {
		t.Fatalf("OpenAI 请求体不得出现 MCP 控制块：%s", encoded)
	}
	var built map[string]any
	if err := json.Unmarshal(encoded, &built); err != nil {
		t.Fatal(err)
	}
	messages, _ := built["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("messages = %d, want 1（控制块消息整条丢弃）", len(messages))
	}
	if _, ok := built["tools"]; ok {
		t.Fatalf("按引用添加不应凭空生成工具声明：%v", built["tools"])
	}
}

// TestAnthropicToolReferenceInsideToolResult 锁定工具搜索结果形态：defer_loading
// 的工具会在 tool_result.content[] 里以 tool_reference 命名，必须原样保留
// （线上第二类 400 的根因）。
func TestAnthropicToolReferenceInsideToolResult(t *testing.T) {
	body := `{
	  "model": "m", "max_tokens": 1,
	  "messages": [
	    {"role": "assistant", "content": [
	      {"type": "tool_use", "id": "tu_1", "name": "tool_search", "input": {"query": "forecast"}}
	    ]},
	    {"role": "user", "content": [
	      {"type": "tool_result", "tool_use_id": "tu_1", "content": [
	        {"type": "tool_reference", "name": "get_forecast"}
	      ]}
	    ]}
	  ]
	}`
	req, err := ParseAnthropicRequest([]byte(body))
	if err != nil {
		t.Fatalf("ParseAnthropicRequest: %v", err)
	}
	encoded, err := BuildAnthropicRequestBody(req)
	if err != nil {
		t.Fatalf("BuildAnthropicRequestBody: %v", err)
	}
	if !strings.Contains(string(encoded), `"tool_reference"`) {
		t.Fatalf("tool_result 里的 tool_reference 未回写：%s", encoded)
	}
	if _, err := ParseAnthropicRequest(encoded); err != nil {
		t.Fatalf("往返后 ParseAnthropicRequest: %v", err)
	}
}

// TestGeminiBuildSkipsAnthropicToolChangeBlocks 锁定 Gemini 跨协议路径：
// Gemini 的 systemInstruction 只接受文本，控制块不得混入 contents/system。
func TestGeminiBuildSkipsAnthropicToolChangeBlocks(t *testing.T) {
	req, err := ParseAnthropicRequest([]byte(anthropicToolChangeRequest))
	if err != nil {
		t.Fatalf("ParseAnthropicRequest: %v", err)
	}
	encoded, err := BuildGeminiRequestBody(req)
	if err != nil {
		t.Fatalf("BuildGeminiRequestBody: %v", err)
	}
	if strings.Contains(string(encoded), "tool_addition") {
		t.Fatalf("Gemini 请求体不得出现 Anthropic 控制块：%s", encoded)
	}
	var built map[string]any
	if err := json.Unmarshal(encoded, &built); err != nil {
		t.Fatal(err)
	}
	contents, _ := built["contents"].([]any)
	if len(contents) != 3 {
		t.Fatalf("contents = %d, want 3（控制块消息不产生空 content）", len(contents))
	}
}
