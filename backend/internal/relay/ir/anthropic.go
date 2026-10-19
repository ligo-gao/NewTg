package ir

// Anthropic Messages API ↔ IR 映射。
//
// 依据：
//   - 契约 `contract/types.go` §2.2/§2.3（Request/Response/Usage 构件）
//   - `docs/relay-02-协议适配器层-设计.md` §5 行 14/25（anthropic / moonshot）
//   - `docs/relay-03-流式与用量提取-设计.md` §6.2 row 4（Claude usage 字段路径）
//   - 计划 M4 R-04：content 块只显式映射 text/image/tool_use/tool_result 四类，
//     **未知块类型一律返回错误，禁止静默丢弃**（Review Focus 1）
//
// 设计纪律（GC-2）：协议差异只停在本包；路由/计费/日志只读 contract IR。
// 原始请求体完整保存在 Request.Raw（双层 IR 的原始层），供同协议上游透传
// 未建模字段（thinking / top_k / metadata 等）。

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"newtg/gateway/internal/contract"
)

// FormatAnthropicMessages 是本包写入 `Request.RawFormat` 的协议标识。
// 与 OpenAI 侧的 FormatOpenAIChat 同构：只做标识，不承载行为。
const FormatAnthropicMessages contract.RelayFormat = "anthropic_messages"

// anthropicDefaultMaxTokens 是 IR 没有 max_tokens 时写入请求体的缺省值。
//
// 为什么需要缺省：Anthropic Messages API 把 max_tokens 列为**必填**字段，
// 而 OpenAI 客户端经常省略它；跨协议转发（T5）经 IR 中转时会丢掉「未设置」
// 这一事实。基线的 `claude.default_max_tokens` 缺省即为 8192（模块 02 §3.4），
// 当前冻结的 RelayContext 没有承载该配置的字段（契约 §5 渠道组），
// 因此这里取基线缺省值并在报告中登记「可配置化」缺口。
const anthropicDefaultMaxTokens = 8192

// AnthropicToIR 是跨任务冻结接口（ledger T2）的名字，转调 ParseAnthropicRequest。
// 保留别名是为了让 T5 按冻结签名调用时无需知道内部命名风格。
func AnthropicToIR(body []byte) (*contract.Request, error) {
	return ParseAnthropicRequest(body)
}

// IRToAnthropic 是跨任务冻结接口（ledger T2）的名字，转调 BuildAnthropicRequestBody。
func IRToAnthropic(req *contract.Request) ([]byte, error) {
	return BuildAnthropicRequestBody(req)
}

// ============================================================================
// 入站解析：Anthropic JSON → IR
// ============================================================================

// ParseAnthropicRequest 把 Anthropic Messages 请求体解析为归一化 IR。
//
// system 支持字符串与「文本块数组」两种规范形态；两种都会合并为一条
// RoleSystem 消息（Anthropic 的 system 是顶层字段，不是消息）。
// messages 的 content 同样支持字符串与块数组。
func ParseAnthropicRequest(body []byte) (*contract.Request, error) {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" || trimmed[0] != '{' {
		return nil, paramErrf("Anthropic 请求体必须是 JSON 对象")
	}
	var payload struct {
		Model         *string           `json:"model"`
		Stream        bool              `json:"stream"`
		MaxTokens     *int              `json:"max_tokens"`
		Temperature   *float64          `json:"temperature"`
		TopP          *float64          `json:"top_p"`
		System        json.RawMessage   `json:"system"`
		Messages      []json.RawMessage `json:"messages"`
		Tools         []json.RawMessage `json:"tools"`
		ToolChoice    json.RawMessage   `json:"tool_choice"`
		StopSequences json.RawMessage   `json:"stop_sequences"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, paramErrf("invalid Anthropic request JSON: %v", err)
	}

	req := &contract.Request{
		Stream:    payload.Stream,
		MaxTokens: payload.MaxTokens,
		RawFormat: FormatAnthropicMessages,
		Raw:       append(contract.ProtocolBody(nil), body...),
	}
	if payload.Model != nil {
		req.Model = *payload.Model
	}
	req.Temperature = payload.Temperature
	req.TopP = payload.TopP
	if err := validateScalars(req.MaxTokens, req.Temperature, req.TopP); err != nil {
		return nil, err
	}

	system, err := parseAnthropicSystem(payload.System)
	if err != nil {
		return nil, err
	}
	msgs, err := parseAnthropicMessages(payload.Messages)
	if err != nil {
		return nil, err
	}
	if system != nil {
		// system 必须位于消息序列最前：Anthropic 语义中它是全局指令，
		// 与多轮消息的顺序无关；放到最前也便于 BuildAnthropicRequestBody 重新抽取。
		req.Messages = append([]contract.Message{*system}, msgs...)
	} else {
		req.Messages = msgs
	}

	tools, err := parseAnthropicTools(payload.Tools)
	if err != nil {
		return nil, err
	}
	req.Tools = tools
	if req.ToolChoice, err = parseAnthropicToolChoice(payload.ToolChoice); err != nil {
		return nil, err
	}
	if req.Stop, err = parseAnthropicStopSequences(payload.StopSequences); err != nil {
		return nil, err
	}
	return req, nil
}

// parseAnthropicSystem 解析顶层 system 字段（字符串或文本块数组）。
func parseAnthropicSystem(raw json.RawMessage) (*contract.Message, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	// 规范形态一：纯字符串。
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return &contract.Message{Role: contract.RoleSystem, Content: []contract.ContentPart{{
			Kind: contract.PartText,
			Text: text,
		}}}, nil
	}
	// 规范形态二：文本块数组（可带 cache_control）。
	var blocks []json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, paramErrf("system 必须是字符串或文本块数组")
	}
	parts := make([]contract.ContentPart, 0, len(blocks))
	for i, block := range blocks {
		part, err := parseAnthropicBlock(block)
		if err != nil {
			return nil, fmt.Errorf("system[%d]: %w", i, err)
		}
		if part.Kind != contract.PartText || isAnthropicThinkingPart(part) {
			// Anthropic 的 system 只接受正文文本块；图片/工具/思考块出现在
			// system 是调用方错误，显式拒绝而不是丢弃。
			return nil, paramErrf("system[%d] 只允许正文 text 块", i)
		}
		parts = append(parts, part)
	}
	return &contract.Message{Role: contract.RoleSystem, Content: parts}, nil
}

func parseAnthropicMessages(raws []json.RawMessage) ([]contract.Message, error) {
	if len(raws) == 0 {
		return nil, nil
	}
	out := make([]contract.Message, 0, len(raws))
	for i, raw := range raws {
		var msg struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(raw, &msg); err != nil {
			return nil, fmt.Errorf("messages[%d]: %w", i, paramErrf("消息必须是 JSON 对象"))
		}
		var role contract.Role
		switch msg.Role {
		case "user":
			role = contract.RoleUser
		case "assistant":
			role = contract.RoleAssistant
		default:
			// Anthropic 只有 user/assistant；system 已在顶层。
			return nil, fmt.Errorf("messages[%d]: %w", i, paramErrf("不支持的消息角色 %q", msg.Role))
		}
		parts, err := parseAnthropicContent(msg.Content)
		if err != nil {
			return nil, fmt.Errorf("messages[%d]: %w", i, err)
		}
		out = append(out, contract.Message{Role: role, Content: parts})
	}
	return out, nil
}

// parseAnthropicContent 解析消息 content（字符串或块数组）。
func parseAnthropicContent(raw json.RawMessage) ([]contract.ContentPart, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return []contract.ContentPart{{Kind: contract.PartText, Text: text}}, nil
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, paramErrf("content 必须是字符串或块数组")
	}
	parts := make([]contract.ContentPart, 0, len(blocks))
	for i, block := range blocks {
		part, err := parseAnthropicBlock(block)
		if err != nil {
			return nil, fmt.Errorf("content[%d]: %w", i, err)
		}
		parts = append(parts, part)
	}
	return parts, nil
}

// parseAnthropicBlock 解析单个 content 块。
//
// R-04 冻结的四类显式映射：text / image / tool_use / tool_result；
// 扩展思考（thinking / redacted_thinking）是 Anthropic 的标准块，
// 同样显式映射到 Meta 逃生舱（复审 ruling：对有效 200 响应不得返回 502）。
// 其余任何类型（document、server_tool_use …）都返回参数错误 ——
// 静默丢弃会让请求内容悄悄缺失，属于不可接受的语义漂移。
func parseAnthropicBlock(raw json.RawMessage) (contract.ContentPart, error) {
	var block map[string]any
	if err := json.Unmarshal(raw, &block); err != nil {
		return contract.ContentPart{}, paramErrf("content 块必须是 JSON 对象")
	}
	blockType, _ := block["type"].(string)
	switch blockType {
	case "text":
		text, _ := block["text"].(string)
		meta := blockMeta(block, "cache_control")
		return contract.ContentPart{Kind: contract.PartText, Text: text, Meta: meta}, nil
	case "thinking":
		// 扩展思考是 Anthropic 的标准块，不是「未知类型」。IR 没有独立的
		// reasoning 部件，用 text + Meta 作为逃生舱：顺序与 signature 都能
		// 无损回写；下游若要区分正文与思考可读 Meta。
		//
		// 【复审 N1】anthropic_thinking 是 Anthropic 专属来源标记：Gemini 的
		// thought part 也复用 Meta["thought"]=true，若编码器只看 thought 就会
		// 把 Gemini 推理误写成没有 signature 的 Anthropic thinking 块，
		// 上游会拒绝。因此 Anthropic 的 thinking 回写只认本标记。
		thinking, _ := block["thinking"].(string)
		meta := blockMeta(block, "cache_control")
		if meta == nil {
			meta = map[string]any{}
		}
		meta["thought"] = true
		meta["anthropic_thinking"] = true
		if signature, _ := block["signature"].(string); signature != "" {
			meta["signature"] = signature
		}
		return contract.ContentPart{Kind: contract.PartText, Text: thinking, Meta: meta}, nil
	case "redacted_thinking":
		// 加密思考块没有可读正文；data 原样保存在 Meta，回写时恢复原块。
		data, _ := block["data"].(string)
		meta := blockMeta(block, "cache_control")
		if meta == nil {
			meta = map[string]any{}
		}
		meta["redacted_thinking"] = data
		return contract.ContentPart{Kind: contract.PartText, Meta: meta}, nil
	case "image":
		return parseAnthropicImageBlock(block)
	case "tool_use":
		id, _ := block["id"].(string)
		name, _ := block["name"].(string)
		if name == "" {
			return contract.ContentPart{}, paramErrf("tool_use 缺少 name")
		}
		input, ok := block["input"]
		if !ok || input == nil {
			input = map[string]any{}
		}
		encoded, err := json.Marshal(input)
		if err != nil {
			return contract.ContentPart{}, paramErrf("tool_use.input 无法编码: %v", err)
		}
		meta := blockMeta(block, "cache_control")
		if meta == nil {
			meta = map[string]any{}
		}
		meta["id"] = id
		meta["name"] = name
		return contract.ContentPart{Kind: contract.PartToolCall, Text: string(encoded), Meta: meta}, nil
	case "tool_result":
		return parseAnthropicToolResultBlock(block)
	default:
		return contract.ContentPart{}, paramErrf("不支持的 Anthropic content 块类型 %q（仅 text/image/tool_use/tool_result）", blockType)
	}
}

// parseAnthropicImageBlock 把 image 块映射为 PartImageB64 / PartImageURL。
//
// Anthropic 的 source 有两种：base64（media_type + data）与 url（url）。
// 两种都能无损回写；未知 source type 显式报错（例如 file_id 属于 Files API，
// 当前的 IR 没有对应构件）。
func parseAnthropicImageBlock(block map[string]any) (contract.ContentPart, error) {
	source, ok := block["source"].(map[string]any)
	if !ok {
		return contract.ContentPart{}, paramErrf("image 块缺少 source 对象")
	}
	sourceType, _ := source["type"].(string)
	meta := blockMeta(block, "cache_control")
	switch sourceType {
	case "base64":
		mediaType, _ := source["media_type"].(string)
		data, _ := source["data"].(string)
		payload, err := base64.StdEncoding.DecodeString(data)
		if err != nil {
			return contract.ContentPart{}, paramErrf("image.source.data 不是合法 base64: %v", err)
		}
		if meta == nil {
			meta = map[string]any{}
		}
		if mediaType != "" {
			meta["mime_type"] = mediaType
		}
		return contract.ContentPart{Kind: contract.PartImageB64, Payload: payload, Meta: meta}, nil
	case "url":
		rawURL, _ := source["url"].(string)
		if rawURL == "" {
			return contract.ContentPart{}, paramErrf("image.source.url 为空")
		}
		return contract.ContentPart{Kind: contract.PartImageURL, URL: rawURL, Meta: meta}, nil
	default:
		return contract.ContentPart{}, paramErrf("不支持的 image.source.type %q", sourceType)
	}
}

// parseAnthropicToolResultBlock 把 tool_result 映射为 PartToolResult。
//
// content 有两种规范形态：字符串，或「文本 + 图片」块数组。IR 的
// PartToolResult 只有 Text 字段；非文本块原样保存在 Meta["content_blocks"]，
// 由编码器回写，避免工具结果里的图片被静默丢弃。
func parseAnthropicToolResultBlock(block map[string]any) (contract.ContentPart, error) {
	toolUseID, _ := block["tool_use_id"].(string)
	if toolUseID == "" {
		return contract.ContentPart{}, paramErrf("tool_result 缺少 tool_use_id")
	}
	meta := blockMeta(block, "cache_control")
	if meta == nil {
		meta = map[string]any{}
	}
	meta["tool_call_id"] = toolUseID
	if isError, ok := block["is_error"].(bool); ok && isError {
		meta["is_error"] = true
	}

	switch content := block["content"].(type) {
	case nil:
		return contract.ContentPart{Kind: contract.PartToolResult, Meta: meta}, nil
	case string:
		return contract.ContentPart{Kind: contract.PartToolResult, Text: content, Meta: meta}, nil
	case []any:
		var text strings.Builder
		for i, item := range content {
			obj, ok := item.(map[string]any)
			if !ok {
				return contract.ContentPart{}, paramErrf("tool_result.content[%d] 必须是 JSON 对象", i)
			}
			itemType, _ := obj["type"].(string)
			switch itemType {
			case "text":
				if value, ok := obj["text"].(string); ok {
					text.WriteString(value)
				}
			case "image":
				// 图片属于工具结果内容，IR 的 Text 无法承载；保留原始块数组。
			default:
				return contract.ContentPart{}, paramErrf("tool_result.content[%d] 不支持的块类型 %q", i, itemType)
			}
		}
		// 完整保留原始块数组（含 text）：回写时才能逐块还原。
		// Text 只是给只读 IR 消费方的文本投影，不是唯一真源。
		meta["content_blocks"] = content
		return contract.ContentPart{Kind: contract.PartToolResult, Text: text.String(), Meta: meta}, nil
	default:
		return contract.ContentPart{}, paramErrf("tool_result.content 必须是字符串或块数组")
	}
}

func parseAnthropicTools(raws []json.RawMessage) ([]contract.Tool, error) {
	if len(raws) == 0 {
		return nil, nil
	}
	tools := make([]contract.Tool, 0, len(raws))
	for i, raw := range raws {
		var tool struct {
			Type        string         `json:"type"`
			Name        string         `json:"name"`
			Description string         `json:"description"`
			InputSchema map[string]any `json:"input_schema"`
		}
		if err := json.Unmarshal(raw, &tool); err != nil {
			return nil, paramErrf("tools[%d] 不是合法 JSON 对象", i)
		}
		// 新版 API 用 type:"custom" 指代普通工具；缺省类型视为普通工具。
		if tool.Type != "" && tool.Type != "custom" {
			return nil, paramErrf("tools[%d] 的类型 %q 不受支持", i, tool.Type)
		}
		if tool.Name == "" {
			return nil, paramErrf("tools[%d] 缺少 name", i)
		}
		tools = append(tools, contract.Tool{
			Name:        tool.Name,
			Description: tool.Description,
			Schema:      tool.InputSchema,
		})
	}
	return tools, nil
}

func parseAnthropicToolChoice(raw json.RawMessage) (contract.ToolChoice, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return contract.ToolChoice{}, nil
	}
	var choice struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &choice); err != nil {
		return contract.ToolChoice{}, paramErrf("tool_choice 形状非法")
	}
	switch choice.Type {
	case "auto":
		return contract.ToolChoice{Mode: contract.ToolChoiceAuto}, nil
	case "none":
		return contract.ToolChoice{Mode: contract.ToolChoiceNone}, nil
	case "any":
		// Anthropic 的 any = 必须调用某个工具，对应 IR 的 required。
		return contract.ToolChoice{Mode: contract.ToolChoiceRequired}, nil
	case "tool":
		if choice.Name == "" {
			return contract.ToolChoice{}, paramErrf("tool_choice tool 缺少 name")
		}
		return contract.ToolChoice{Mode: contract.ToolChoiceSpecific, Name: choice.Name}, nil
	default:
		return contract.ToolChoice{}, paramErrf("tool_choice.type %q 不受支持", choice.Type)
	}
}

func parseAnthropicStopSequences(raw json.RawMessage) ([]string, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	var stops []string
	if err := json.Unmarshal(raw, &stops); err != nil {
		return nil, paramErrf("stop_sequences 必须是字符串数组")
	}
	return stops, nil
}

// isAnthropicThinkingPart 判断 IR 部件是否由 Anthropic thinking /
// redacted_thinking 映射而来。只看 anthropic_thinking：Gemini 的 thought part
// 同样带 Meta["thought"]，不能据此当作 Anthropic 思考块（复审 N1）。
func isAnthropicThinkingPart(part contract.ContentPart) bool {
	if anthropicThinking, _ := part.Meta["anthropic_thinking"].(bool); anthropicThinking {
		return true
	}
	_, redacted := part.Meta["redacted_thinking"]
	return redacted
}

// isKnownAnthropicBlockType 判断块类型是否已显式建模。
// 用于响应解析区分「已知类型的字段错误」（拒绝）与「未来新增类型」（保留）。
func isKnownAnthropicBlockType(blockType string) bool {
	switch blockType {
	case "text", "image", "tool_use", "tool_result", "thinking", "redacted_thinking":
		return true
	default:
		return false
	}
}

// blockMeta 只抽取指定的块级扩展字段（目前是 cache_control）。
// 返回 nil 而不是空 map：避免编码器把空的扩展字段写进上游请求。
func blockMeta(block map[string]any, keys ...string) map[string]any {
	var meta map[string]any
	for _, key := range keys {
		if value, ok := block[key]; ok {
			if meta == nil {
				meta = map[string]any{}
			}
			meta[key] = value
		}
	}
	return meta
}

// ============================================================================
// 出站构造：IR → Anthropic JSON
// ============================================================================

// BuildAnthropicRequestBody 把归一化 IR 编码为 Anthropic Messages 请求体。
//
// 透传策略与 OpenAI 侧一致：当 RawFormat 是 Anthropic 时，以 Raw 对象为基底，
// 再用归一化字段覆盖已建模部分，保留 thinking / top_k / metadata 等未建模字段。
// 跨协议（RawFormat=openai 或空）不参与透传，避免异种协议字段混入。
func BuildAnthropicRequestBody(req *contract.Request) ([]byte, error) {
	if req == nil {
		return nil, paramErrf("nil request")
	}
	// 适配器本地再校验一次标量（relay-02 §4.1）：IR 可能来自跨协议调用方，
	// 负数会成为计费乘数（billing.md 硬约束），不能只依赖入站解析。
	if err := validateScalars(req.MaxTokens, req.Temperature, req.TopP); err != nil {
		return nil, err
	}
	obj := map[string]any{}
	if len(req.Raw) > 0 && isAnthropicRawFormat(req.RawFormat) {
		var passthrough map[string]any
		if err := json.Unmarshal(req.Raw, &passthrough); err != nil {
			return nil, paramErrf("Raw 不是合法 JSON 对象: %v", err)
		}
		if passthrough != nil {
			obj = passthrough
		}
	}

	obj["model"] = req.Model
	// max_tokens 是 Anthropic 的必填字段（见 anthropicDefaultMaxTokens 注释）。
	maxTokens := anthropicDefaultMaxTokens
	if req.MaxTokens != nil {
		maxTokens = *req.MaxTokens
	}
	obj["max_tokens"] = maxTokens
	obj["stream"] = req.Stream

	system, messages, err := encodeAnthropicMessages(req.Messages)
	if err != nil {
		return nil, err
	}
	if system != nil {
		obj["system"] = system
	} else {
		delete(obj, "system")
	}
	obj["messages"] = messages

	// 原始 tools / tool_choice 子对象必须在覆盖前捕获（复审 P2）：
	// contract.Tool 没有 type/cache_control，IR 也不建模
	// disable_parallel_tool_use；以原始子对象为基底做增量合并才能保住它们。
	rawTools, _ := obj["tools"].([]any)
	rawToolChoice, _ := obj["tool_choice"].(map[string]any)

	if len(req.Tools) > 0 {
		tools, err := encodeAnthropicTools(req.Tools, rawTools)
		if err != nil {
			return nil, err
		}
		obj["tools"] = tools
	} else {
		delete(obj, "tools")
	}

	if req.ToolChoice.Mode != "" {
		choice, err := encodeAnthropicToolChoice(req.ToolChoice, rawToolChoice)
		if err != nil {
			return nil, err
		}
		obj["tool_choice"] = choice
	} else {
		delete(obj, "tool_choice")
	}

	if req.Temperature != nil {
		obj["temperature"] = *req.Temperature
	} else {
		delete(obj, "temperature")
	}
	if req.TopP != nil {
		obj["top_p"] = *req.TopP
	} else {
		delete(obj, "top_p")
	}
	if len(req.Stop) > 0 {
		obj["stop_sequences"] = req.Stop
	} else {
		delete(obj, "stop_sequences")
	}
	// ResponseFormat / Seed 在 Anthropic 协议里没有对应字段：显式忽略。
	// 不把它们塞进请求体（上游会 400），也不伪造语义。
	// 【复审 #11 登记】Anthropic 结构化输出（output_format）尚未建模；
	// 跨协议 JSON 模式目前是「尽力而为」而非强保证。

	return json.Marshal(obj)
}

func encodeAnthropicMessages(messages []contract.Message) (any, []any, error) {
	var systemParts []contract.ContentPart
	out := make([]any, 0, len(messages))

	appendMessage := func(role string, blocks []any) {
		if len(out) > 0 {
			if last, ok := out[len(out)-1].(map[string]any); ok {
				if lastRole, _ := last["role"].(string); lastRole == role {
					// Anthropic 要求消息角色交替；连续同角色消息合并为一个
					// content 块数组，既满足协议约束又不改变语义。
					if existing, ok := last["content"].([]any); ok {
						last["content"] = append(existing, blocks...)
						return
					}
				}
			}
		}
		out = append(out, map[string]any{"role": role, "content": blocks})
	}

	for i, message := range messages {
		switch message.Role {
		case contract.RoleSystem, contract.RoleDeveloper:
			for j, part := range message.Content {
				if part.Kind != contract.PartText || isAnthropicThinkingPart(part) {
					return nil, nil, paramErrf("messages[%d].content[%d]: system 消息只允许正文 text 部件", i, j)
				}
				systemParts = append(systemParts, part)
			}
		case contract.RoleUser:
			blocks, err := encodeAnthropicBlocks(message.Content)
			if err != nil {
				return nil, nil, fmt.Errorf("messages[%d]: %w", i, err)
			}
			appendMessage("user", blocks)
		case contract.RoleAssistant:
			blocks, err := encodeAnthropicBlocks(message.Content)
			if err != nil {
				return nil, nil, fmt.Errorf("messages[%d]: %w", i, err)
			}
			appendMessage("assistant", blocks)
		case contract.RoleTool:
			// tool 角色在 Anthropic 中承载于 user 消息的 tool_result 块。
			blocks, err := encodeAnthropicBlocks(message.Content)
			if err != nil {
				return nil, nil, fmt.Errorf("messages[%d]: %w", i, err)
			}
			appendMessage("user", blocks)
		default:
			return nil, nil, paramErrf("messages[%d]: 不支持的消息角色 %q", i, message.Role)
		}
	}
	// 多个 system 消息按出现顺序合并：Anthropic 只有一个 system 字段。
	// 单条且无扩展字段时写成字符串（最常见形态）；否则写文本块数组，
	// 保留 cache_control 等块级字段。
	if len(systemParts) == 0 {
		return nil, out, nil
	}
	if len(systemParts) == 1 && len(systemParts[0].Meta) == 0 {
		return systemParts[0].Text, out, nil
	}
	blocks := make([]any, 0, len(systemParts))
	for _, part := range systemParts {
		block := map[string]any{"type": "text", "text": part.Text}
		copyMetaField(block, part.Meta, "cache_control")
		blocks = append(blocks, block)
	}
	return blocks, out, nil
}

func encodeAnthropicBlocks(parts []contract.ContentPart) ([]any, error) {
	blocks := make([]any, 0, len(parts))
	for i, part := range parts {
		block, err := encodeAnthropicBlock(part)
		if err != nil {
			return nil, fmt.Errorf("content[%d]: %w", i, err)
		}
		blocks = append(blocks, block)
	}
	return blocks, nil
}

func encodeAnthropicBlock(part contract.ContentPart) (map[string]any, error) {
	switch part.Kind {
	case contract.PartText:
		// 加密思考块与可读思考块在 IR 里都以 text + Meta 承载；
		// 回写时必须恢复原块类型，不能当成正文 text 发回上游。
		if data, ok := part.Meta["redacted_thinking"].(string); ok {
			block := map[string]any{"type": "redacted_thinking", "data": data}
			copyMetaField(block, part.Meta, "cache_control")
			return block, nil
		}
		if thought, _ := part.Meta["anthropic_thinking"].(bool); thought {
			block := map[string]any{"type": "thinking", "thinking": part.Text}
			if signature := metaString(part.Meta, "signature"); signature != "" {
				block["signature"] = signature
			}
			copyMetaField(block, part.Meta, "cache_control")
			return block, nil
		}
		block := map[string]any{"type": "text", "text": part.Text}
		copyMetaField(block, part.Meta, "cache_control")
		return block, nil
	case contract.PartImageURL:
		if part.URL == "" {
			return nil, paramErrf("image_url 部件缺少 URL")
		}
		block := map[string]any{
			"type":   "image",
			"source": map[string]any{"type": "url", "url": part.URL},
		}
		copyMetaField(block, part.Meta, "cache_control")
		return block, nil
	case contract.PartImageB64:
		if len(part.Payload) == 0 {
			return nil, paramErrf("image_b64 部件缺少负载")
		}
		mediaType := metaString(part.Meta, "mime_type")
		if mediaType == "" {
			mediaType = "image/png"
		}
		block := map[string]any{
			"type": "image",
			"source": map[string]any{
				"type":       "base64",
				"media_type": mediaType,
				"data":       base64.StdEncoding.EncodeToString(part.Payload),
			},
		}
		copyMetaField(block, part.Meta, "cache_control")
		return block, nil
	case contract.PartToolCall:
		name := metaString(part.Meta, "name")
		if name == "" {
			return nil, paramErrf("tool_call 缺少 function.name")
		}
		input, err := encodeAnthropicToolInput(part.Text)
		if err != nil {
			return nil, err
		}
		block := map[string]any{
			"type":  "tool_use",
			"id":    metaString(part.Meta, "id"),
			"name":  name,
			"input": input,
		}
		copyMetaField(block, part.Meta, "cache_control")
		return block, nil
	case contract.PartToolResult:
		callID := metaString(part.Meta, "tool_call_id")
		if callID == "" {
			return nil, paramErrf("tool_result 缺少 tool_call_id")
		}
		block := map[string]any{
			"type":        "tool_result",
			"tool_use_id": callID,
		}
		// content_blocks 是解析期保留的非文本内容（工具结果里的图片）；
		// 有它时按原始块数组回写，避免图片被降级成文本。
		if blocks, ok := part.Meta["content_blocks"]; ok {
			block["content"] = blocks
		} else {
			block["content"] = part.Text
		}
		if isError, _ := part.Meta["is_error"].(bool); isError {
			block["is_error"] = true
		}
		copyMetaField(block, part.Meta, "cache_control")
		return block, nil
	case contract.PartAudio:
		// Anthropic Messages 不接受音频输入；显式报错而不是丢弃。
		return nil, paramErrf("Anthropic 不支持 audio 部件")
	default:
		return nil, paramErrf("不支持的部件类型 %q", part.Kind)
	}
}

// encodeAnthropicToolInput 把 IR 的 JSON 字符串参数还原为对象。
func encodeAnthropicToolInput(text string) (any, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return map[string]any{}, nil
	}
	var input any
	if err := json.Unmarshal([]byte(trimmed), &input); err != nil {
		return nil, paramErrf("tool_call 参数不是合法 JSON: %v", err)
	}
	return input, nil
}

// encodeAnthropicTools 编码工具声明。rawTools 是原始请求中的 tools 数组，
// 按名称匹配后作为基底，保留 type（custom）、cache_control 等未建模字段。
func encodeAnthropicTools(tools []contract.Tool, rawTools []any) ([]any, error) {
	out := make([]any, 0, len(tools))
	for i, tool := range tools {
		if tool.Name == "" {
			return nil, paramErrf("tools[%d] 缺少 name", i)
		}
		item := map[string]any{}
		if raw := matchAnthropicTool(rawTools, i, tool.Name); raw != nil {
			item = cloneMap(raw) // 深拷贝：不得原地改写 Raw 解码结果
		}
		schema := tool.Schema
		if schema == nil {
			schema = map[string]any{"type": "object"}
		}
		item["name"] = tool.Name
		item["input_schema"] = schema
		if tool.Description != "" {
			item["description"] = tool.Description
		}
		out = append(out, item)
	}
	return out, nil
}

// matchAnthropicTool 优先按下标、其次按名称匹配原始工具子对象。
// 归一化层若过滤/重排工具，下标会错位；名称匹配避免把 A 工具的
// cache_control 贴到 B 工具上。
func matchAnthropicTool(rawTools []any, index int, name string) map[string]any {
	if index < len(rawTools) {
		if raw, ok := rawTools[index].(map[string]any); ok {
			if rawName, _ := raw["name"].(string); rawName == name {
				return raw
			}
		}
	}
	for _, candidate := range rawTools {
		raw, ok := candidate.(map[string]any)
		if !ok {
			continue
		}
		if rawName, _ := raw["name"].(string); rawName == name {
			return raw
		}
	}
	return nil
}

func encodeAnthropicToolChoice(choice contract.ToolChoice, raw map[string]any) (map[string]any, error) {
	// 以原始子对象为基底保留 disable_parallel_tool_use 等未建模字段。
	item := map[string]any{}
	if raw != nil {
		item = cloneMap(raw)
	}
	delete(item, "name") // 先清掉旧名称；仅 specific 重新写入。
	switch choice.Mode {
	case contract.ToolChoiceAuto:
		item["type"] = "auto"
		return item, nil
	case contract.ToolChoiceNone:
		item["type"] = "none"
		return item, nil
	case contract.ToolChoiceRequired:
		item["type"] = "any"
		return item, nil
	case contract.ToolChoiceSpecific:
		if choice.Name == "" {
			return nil, paramErrf("tool_choice specific 缺少 name")
		}
		item["type"] = "tool"
		item["name"] = choice.Name
		return item, nil
	default:
		return nil, paramErrf("tool_choice 模式 %q 不受支持", choice.Mode)
	}
}

func isAnthropicRawFormat(format contract.RelayFormat) bool {
	switch strings.ToLower(string(format)) {
	case "anthropic", "anthropic_messages", "claude", "claude_messages":
		return true
	default:
		return false
	}
}

// copyMetaField 把 Meta 中的可选扩展字段复制到块上。
func copyMetaField(block map[string]any, meta map[string]any, key string) {
	if meta == nil {
		return
	}
	if value, ok := meta[key]; ok {
		block[key] = value
	}
}

// ============================================================================
// 上游响应解析：Anthropic JSON → contract.Response/Usage
// ============================================================================

// ParseAnthropicResponse 解析非流式 Messages 响应。
func ParseAnthropicResponse(body []byte) (*contract.Response, contract.Usage, error) {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" || trimmed[0] != '{' {
		return nil, contract.Usage{}, paramErrf("Anthropic 响应体必须是 JSON 对象")
	}
	var payload struct {
		ID         string            `json:"id"`
		Model      string            `json:"model"`
		Content    []json.RawMessage `json:"content"`
		StopReason string            `json:"stop_reason"`
		Usage      json.RawMessage   `json:"usage"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, contract.Usage{}, paramErrf("invalid Anthropic response JSON: %v", err)
	}

	message := contract.Message{Role: contract.RoleAssistant}
	var unmapped []any
	for i, raw := range payload.Content {
		part, err := parseAnthropicBlock(raw)
		if err != nil {
			// 复审 P1：有效 200 响应绝不能因为「未来新增的块类型」变成 502。
			// 已建模类型（含 thinking）的字段错误仍然拒绝；真正未知的类型
			// 原样存进 Extensions，绝不静默丢弃。
			var probe struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal(raw, &probe)
			if isKnownAnthropicBlockType(probe.Type) {
				return nil, contract.Usage{}, fmt.Errorf("content[%d]: %w", i, err)
			}
			var opaque any
			if json.Unmarshal(raw, &opaque) == nil {
				unmapped = append(unmapped, opaque)
			}
			continue
		}
		message.Content = append(message.Content, part)
	}
	if len(unmapped) > 0 {
		if message.Extensions == nil {
			message.Extensions = map[string]any{}
		}
		message.Extensions["unmapped_blocks"] = unmapped
	}
	finishReason := normalizeAnthropicStopReason(payload.StopReason)
	if payload.StopReason != "" {
		if message.Extensions == nil {
			message.Extensions = map[string]any{}
		}
		// 原始结束原因保留给诊断/转码（契约 Choice 注释）。
		message.Extensions["finish_reason"] = payload.StopReason
	}

	response := &contract.Response{
		ID:           payload.ID,
		Model:        payload.Model,
		Choices:      []contract.Choice{{Index: 0, Message: message, FinishReason: finishReason}},
		FinishReason: finishReason,
		Raw:          append(contract.ProtocolBody(nil), body...),
	}
	return response, ParseAnthropicUsage(payload.Usage), nil
}

// ParseAnthropicUsage 归一一个 Anthropic usage JSON 对象。
//
// 字段路径（relay-03 §6.2 row 4）：
//   - Prompt ← input_tokens（**不含**缓存），Completion ← output_tokens
//   - CacheRead ← cache_read_input_tokens
//   - CacheWrite ← cache_creation_input_tokens；缺失时用 cache_creation 的
//     ephemeral_5m/1h 子字段求和兜底
//   - PromptIncludesCache=false（Claude 语义：缓存是独立维度）
//   - ReasoningIncludedInCompletion=true（thinking 计入 output_tokens）
//
// 原始键（input_tokens / cache_creation_input_tokens / ...）原样保留在
// Usage.Raw —— M3 的 usage_semantic 正向判定与对账都依赖这些键名，
// 不得改写/伪装成 OpenAI 或 Gemini 的键。
func ParseAnthropicUsage(raw json.RawMessage) contract.Usage {
	base := contract.Usage{
		PromptIncludesCache:           false,
		ReasoningIncludedInCompletion: true,
		Source:                        contract.UsageEstimated,
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return base
	}
	var payload struct {
		InputTokens              int64          `json:"input_tokens"`
		OutputTokens             int64          `json:"output_tokens"`
		CacheCreationInputTokens int64          `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int64          `json:"cache_read_input_tokens"`
		CacheCreation            map[string]any `json:"cache_creation"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return base
	}
	var rawMap map[string]any
	if err := json.Unmarshal(raw, &rawMap); err != nil || len(rawMap) == 0 {
		// 空对象 / 非法形状没有任何上游事实，不谎报 upstream_exact。
		return base
	}
	// M5：非空但没有任何 Anthropic 用量键的对象（如 {"foo":1}）不是上游
	// 用量事实；若标成 upstream_exact，跨协议层会据此合成一份全零 usage。
	if !hasAnyMapKey(rawMap, "input_tokens", "output_tokens",
		"cache_creation_input_tokens", "cache_read_input_tokens", "cache_creation") {
		return base
	}

	cacheWrite := payload.CacheCreationInputTokens
	if cacheWrite == 0 {
		// 部分上游把总量拆成 ephemeral_5m_input_tokens / ephemeral_1h_input_tokens。
		cacheWrite = cacheCreationSum(payload.CacheCreation)
	}

	usage := contract.Usage{
		Prompt:                        nonNeg(payload.InputTokens),
		Completion:                    nonNeg(payload.OutputTokens),
		CacheRead:                     nonNeg(payload.CacheReadInputTokens),
		CacheWrite:                    nonNeg(cacheWrite),
		PromptIncludesCache:           false,
		ReasoningIncludedInCompletion: true,
		Source:                        contract.UsageUpstreamExact,
		Raw:                           rawMap,
	}
	usage.Total = usage.Prompt + usage.Completion
	usage.PromptDetail = contract.UsageDetail{
		Text:       usage.Prompt,
		CacheRead:  usage.CacheRead,
		CacheWrite: usage.CacheWrite,
	}
	usage.CompletionDetail = contract.UsageDetail{Text: usage.Completion}
	return usage
}

// hasAnyMapKey 判断 map 是否至少含一个给定键（键存在即为已识别，
// 与取值是否为零无关——上游可能合法地报告 0）。
func hasAnyMapKey(values map[string]any, keys ...string) bool {
	for _, key := range keys {
		if _, ok := values[key]; ok {
			return true
		}
	}
	return false
}

// cacheCreationSum 对 cache_creation 子字段求和（非数值键忽略）。
func cacheCreationSum(values map[string]any) int64 {
	var total int64
	for _, value := range values {
		switch number := value.(type) {
		case float64:
			if number > 0 {
				total += int64(number)
			}
		case json.Number:
			if parsed, err := number.Int64(); err == nil && parsed > 0 {
				total += parsed
			}
		}
	}
	return total
}

// normalizeAnthropicStopReason 把 Anthropic stop_reason 映射为 IR 的
// OpenAI 风格归一值（契约 Choice.FinishReason 只约定「归一化后的结束原因」）。
// 未知值保留小写原文，便于诊断——不猜测成语义相近的值。
func normalizeAnthropicStopReason(reason string) string {
	switch reason {
	case "end_turn", "stop_sequence", "pause_turn":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	case "":
		return ""
	default:
		return strings.ToLower(reason)
	}
}
