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
	// stream_options 是 OpenAI Chat 专有字段（include_usage）。/v1/messages 的
	// 流式入口会为跨协议路径统一注入它（OpenAI 上游需要），但本函数也可能
	// 服务同协议 Anthropic 上游——那里没有该请求项，原样转发会被严格上游
	// 拒绝。Anthropic 流恒带 usage，不需要这个开关。
	delete(obj, "stream_options")

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
	unparseable := 0
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
			} else {
				// 诊断标记：极端情况下无法原样保留时，让审计侧可见。
				unparseable++
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
	if unparseable > 0 {
		if message.Extensions == nil {
			message.Extensions = map[string]any{}
		}
		message.Extensions["unmapped_blocks_unparseable"] = unparseable
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

// hasAnyMapKey 判断 map 是否至少含一个给定键且取值为非 null。
// null 键不算「已识别」：与 T3 的 claudeUsageKeysPresent 同口径——
// `{"input_tokens":null}` 没有携带任何上游事实，不能标记 upstream_exact。
func hasAnyMapKey(values map[string]any, keys ...string) bool {
	for _, key := range keys {
		if value, ok := values[key]; ok && value != nil {
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

// ============================================================================
// 响应方向编码：IR → Anthropic JSON / SSE（W18-B · 冻结裁决 R-W18-03）
// ============================================================================
//
// 背景：请求方向早已闭合（ParseOpenAIRequest/ParseGeminiRequest →
// BuildAnthropicRequestBody），但响应方向只有 ir.FromIR（IR → OpenAI）。
// `/v1/messages` 因此把 OpenAI/Gemini 上游固定 501（见 relay_endpoints.go
// 的方向矩阵）。R-W18-03 裁决补齐本节的编码器，让 Claude 客户端也能由
// 异构上游承接；usage 不改冻结的 contract.Response，而是由端点层按
// crossproto 既有模式从上游 Raw 重新解析（见 crossproto.UsageFromNative）。
//
// 信息损失清单（不得静默丢弃；无法回写的块一律显式报错）：
//   - stop_sequence → IR 只保留归一化的 "stop"，具体命中的序列在解析期丢失，
//     编码回写时 stop_sequence 只能为 null；
//   - 非 Anthropic 上游的推理块（OpenAI reasoning_content / Gemini thought）
//     没有 signature，回写为 thinking 块时不带 signature 字段；
//   - tool_result 不属于 assistant 响应，出现时显式报错而不是伪造；
//   - 音频等无法用 Anthropic 响应块表达的部件显式报错。

// AnthropicStopReason 把 IR 归一化结束原因回写为 Anthropic stop_reason。
//
// 映射与 normalizeAnthropicStopReason 互逆（R-W18-03 的单测锁定）：
//
//	stop → end_turn；length → max_tokens；tool_calls → tool_use；
//	content_filter → refusal。
//
// 空值/未知值保留空串/小写原文：非流式响应里由调用方写成 null，流式由
// 渲染器兜底为 end_turn。这里不把未知值猜成 end_turn——那会把「上游给了
// 我们没见过的结束原因」伪装成正常收尾。
func AnthropicStopReason(finishReason string) string {
	switch strings.ToLower(strings.TrimSpace(finishReason)) {
	case "":
		return ""
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls", "tool_use":
		return "tool_use"
	case "content_filter", "refusal":
		return "refusal"
	default:
		return strings.ToLower(strings.TrimSpace(finishReason))
	}
}

// AnthropicUsageObject 把归一化 usage 映射为 Anthropic usage 对象。
//
// 方向差异（契约 §2.3 关系布尔）：
//   - Anthropic 的 input_tokens **不含**缓存；OpenAI 的 prompt_tokens 已含缓存读/写。
//     因此 PromptIncludesCache=true 时必须先扣掉 CacheRead/CacheWrite；
//   - output_tokens 在三种协议里都含 thinking/reasoning token，直接取 Completion
//     （ReasoningIncludedInCompletion 为 true 是 Claude/Gemini/OpenAI 的共同语义）。
//
// 缓存键只在非零时写出：零值省略可以避免向客户端暗示「发生过缓存命中」。
func AnthropicUsageObject(usage contract.Usage) map[string]any {
	input := nonNeg(usage.Prompt)
	if usage.PromptIncludesCache {
		input = nonNeg(input - nonNeg(usage.CacheRead) - nonNeg(usage.CacheWrite))
	}
	obj := map[string]any{
		"input_tokens":  input,
		"output_tokens": nonNeg(usage.Completion),
	}
	if usage.CacheRead > 0 {
		obj["cache_read_input_tokens"] = usage.CacheRead
	}
	if usage.CacheWrite > 0 {
		obj["cache_creation_input_tokens"] = usage.CacheWrite
	}
	return obj
}

// BuildAnthropicResponse 把归一化响应编码为 Anthropic Messages JSON（非流式）。
//
// 产出形状（R-W18-03 冻结）：
//
//	{id,type:"message",role:"assistant",model,content:[...],
//	 stop_reason,stop_sequence,usage:{input_tokens,output_tokens}}
//
// 只编码第一条 choice：Anthropic Messages 没有候选数组语义（且 /v1/messages
// 入口不接受 n>1，见 relay_endpoints.go 的既有拒绝）。usage 由调用方提供——
// 契约 Response 没有 Usage 字段（R-W18-03 明确不扩展冻结结构）。
func BuildAnthropicResponse(resp *contract.Response, usage contract.Usage) ([]byte, error) {
	if resp == nil {
		return nil, paramErrf("nil response")
	}
	if len(resp.Choices) == 0 {
		return nil, paramErrf("响应没有可编码的 choice")
	}
	id := resp.ID
	if strings.TrimSpace(id) == "" {
		// Anthropic 客户端按 id 做消息关联；上游没给 id 时给一个稳定的占位，
		// 绝不省略该字段（缺字段会让 SDK 的类型校验先失败）。
		id = "msg_newtg"
	}
	message := resp.Choices[0].Message
	blocks := make([]any, 0, len(message.Content))
	for i, part := range message.Content {
		block, err := encodeAnthropicResponseBlock(part, id, i)
		if err != nil {
			return nil, fmt.Errorf("content[%d]: %w", i, err)
		}
		blocks = append(blocks, block)
	}
	obj := map[string]any{
		"id":            id,
		"type":          "message",
		"role":          "assistant",
		"model":         resp.Model,
		"content":       blocks,
		"stop_reason":   nilOrString(AnthropicStopReason(resp.FinishReason)),
		"stop_sequence": nil,
		"usage":         AnthropicUsageObject(usage),
	}
	encoded, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("ir: marshal Anthropic response: %w", err)
	}
	return encoded, nil
}

// nilOrString 把空串转成 nil：Anthropic 的 stop_reason 是「字符串或 null」。
func nilOrString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// encodeAnthropicResponseBlock 编码响应消息里的单个 IR 部件。
//
// 与请求方向 encodeAnthropicBlock 的差异仅一处：推理部件。请求方向只认
// Meta["anthropic_thinking"] 标记（避免把 Gemini thought 伪装成需要
// signature 的 Anthropic thinking 发给上游）；响应方向是**发给客户端**的，
// 任何 IR 推理载体都应还原为 thinking/redacted_thinking，否则 Gemini 的
// CoT 会混进可见正文（与 ir.go H1 同一判据）。
//
// scope/index 只用于 tool_use id 缺失时的合成（见 anthropicToolUseID）：
// Gemini 的 functionCall 与旧式 OpenAI function_call 都不带 id。
func encodeAnthropicResponseBlock(part contract.ContentPart, scope string, index int) (map[string]any, error) {
	if part.Kind == contract.PartText && isReasoningPart(part) {
		if data, ok := part.Meta["redacted_thinking"].(string); ok {
			return map[string]any{"type": "redacted_thinking", "data": data}, nil
		}
		block := map[string]any{"type": "thinking", "thinking": part.Text}
		// 非 Anthropic 上游没有 signature；有才写，不伪造。
		if signature := metaString(part.Meta, "signature"); signature != "" {
			block["signature"] = signature
		}
		return block, nil
	}
	if part.Kind == contract.PartToolResult {
		// assistant 响应不含 tool_result；静默丢弃会让内容悄悄缺失。
		return nil, paramErrf("响应消息不应包含 tool_result 块")
	}
	if part.Kind == contract.PartToolCall {
		return encodeAnthropicResponseToolUse(part, scope, index)
	}
	return encodeAnthropicBlock(part)
}

// encodeAnthropicResponseToolUse 编码 assistant 的 tool_use 块。
//
// 为什么必须保证 id 非空（复核 F1）：Gemini 的 functionCall 与旧式 OpenAI
// function_call 没有 id；若原样写 `id:""`，Claude 客户端下一轮回传的
// tool_result.tool_use_id 也是空串，本网关自己的 Anthropic 解析器会拒绝
// （parseAnthropicToolResultBlock 要求非空），多轮工具调用链直接断开。
// 已有 id 一律原样保留；仅在缺失时按「响应 scope + 块序号」合成稳定值，
// 同一次响应内可重复、跨响应不冲突。
func encodeAnthropicResponseToolUse(part contract.ContentPart, scope string, index int) (map[string]any, error) {
	name := metaString(part.Meta, "name")
	if name == "" {
		return nil, paramErrf("tool_call 缺少 name，无法编码为 tool_use")
	}
	input, err := encodeAnthropicToolInput(part.Text)
	if err != nil {
		return nil, err
	}
	id := strings.TrimSpace(metaString(part.Meta, "id"))
	if id == "" {
		id = anthropicToolUseID(scope, index)
	}
	return map[string]any{"type": "tool_use", "id": id, "name": name, "input": input}, nil
}

// anthropicToolUseID 为缺失 id 的工具调用合成稳定非空 id。
//
// 形状 `toolu_<scope>_<index>`：scope 对非流式是消息 id、对流式是
// `msg_<requestID>`；同一响应内不同工具块按序号区分。只做占位，不参与
// 计费或对账。
func anthropicToolUseID(scope string, index int) string {
	scope = strings.TrimSpace(scope)
	if scope == "" {
		return fmt.Sprintf("toolu_%d", index)
	}
	return fmt.Sprintf("toolu_%s_%d", scope, index)
}

// ----------------------------------------------------------------------------
// 流式：IR StreamEvent → Anthropic SSE 事件
// ----------------------------------------------------------------------------

// AnthropicSSEFrame 是一个 Anthropic SSE 事件帧（event + data 两行）。
//
// 与 OpenAI 的纯 data 帧不同，Anthropic 客户端按 `event:` 名称分派；
// data 必须是单个 JSON 对象。
type AnthropicSSEFrame struct {
	Event string
	Data  []byte
}

// anthropicStreamTool 是流式工具调用在渲染器里的累积缓冲。
//
// OpenAI 的 tool_calls 可能并行交错（同一帧里多个 index）；Anthropic 的
// content_block_start/delta/stop 要求同一时刻只有一个块在流。为不把交错
// 片段写成损坏的 input_json_delta，这里按 index 累积，在流结束时顺序输出
// 完整的 tool_use 块（信息无损，只是工具参数不是增量下发）。
type anthropicStreamTool struct {
	index int
	id    string
	name  string
	args  strings.Builder
}

// AnthropicStreamRenderer 把适配器产出的 IR StreamEvent 序列渲染为
// Anthropic SSE 事件序列（R-W18-03）。
//
// 事件顺序（Anthropic 规范，测试锁定）：
//
//	message_start
//	content_block_start → content_block_delta* → content_block_stop   （按块重复）
//	message_delta(stop_reason/usage) → message_stop
//
// usage 只可能在流末尾拿到（OpenAI 需 include_usage、Gemini usageMetadata
// 也常在后段），所以 message_start 的 usage 为 0，最终用量放进 message_delta。
// 这是异构上游的固有信息时序差异，登记为已知限制。
type AnthropicStreamRenderer struct {
	id    string
	model string

	nextIndex  int
	openBlock  bool
	openKind   string // text | thinking
	openIndex  int
	stopReason string

	tools       []*anthropicStreamTool
	toolByIndex map[int]*anthropicStreamTool
	started     bool
}

// NewAnthropicStreamRenderer 构造渲染器。id 建议由请求 ID 派生（msg_ 前缀）。
func NewAnthropicStreamRenderer(id, model string) *AnthropicStreamRenderer {
	if strings.TrimSpace(id) == "" {
		id = "msg_newtg"
	}
	return &AnthropicStreamRenderer{
		id:          id,
		model:       model,
		toolByIndex: map[int]*anthropicStreamTool{},
	}
}

// Start 返回 message_start 帧。
func (r *AnthropicStreamRenderer) Start() []AnthropicSSEFrame {
	if r.started {
		return nil
	}
	r.started = true
	data, _ := json.Marshal(map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            r.id,
			"type":          "message",
			"role":          "assistant",
			"model":         r.model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})
	return []AnthropicSSEFrame{{Event: "message_start", Data: data}}
}

// Push 吃一个 IR 流事件，返回需要写出的 Anthropic 帧。
//
// usage 事件不在这里渲染（端点层负责累计与结算），只在 Close 时统一回写。
func (r *AnthropicStreamRenderer) Push(ev contract.StreamEvent) []AnthropicSSEFrame {
	switch ev.Kind {
	case contract.EventDelta:
		var frames []AnthropicSSEFrame
		if ev.Reasoning != "" {
			frames = append(frames, r.ensureBlock("thinking")...)
			frames = append(frames, r.delta("thinking_delta", "thinking", ev.Reasoning))
		}
		if ev.Delta != "" {
			frames = append(frames, r.ensureBlock("text")...)
			frames = append(frames, r.delta("text_delta", "text", ev.Delta))
		}
		if ev.ToolDelta != nil {
			r.bufferTool(*ev.ToolDelta)
		}
		return frames
	case contract.EventFinish:
		if reason := anthropicStreamFinishReason(ev.Raw); reason != "" {
			r.stopReason = reason
		}
		return nil
	default:
		return nil
	}
}

// Close 在流正常结束后输出收尾事件：关闭当前块、回写缓冲的工具块、
// message_delta（stop_reason + 最终 usage）与 message_stop。
//
// 异常结束（客户端断开/上游错误/超时）绝不能调用 Close：伪造 message_stop
// 会让客户端把失败流当成功（relay-03 §5.2/§10.3）。
func (r *AnthropicStreamRenderer) Close(usage contract.Usage) []AnthropicSSEFrame {
	var frames []AnthropicSSEFrame
	if r.openBlock {
		frames = append(frames, r.stopBlock())
	}
	for _, tool := range r.tools {
		frames = append(frames, r.toolFrames(tool)...)
	}
	stopReason := AnthropicStopReason(r.stopReason)
	if stopReason == "" {
		// 上游没给结束原因时的兜底：有工具块按 tool_use，否则 end_turn。
		if len(r.tools) > 0 {
			stopReason = "tool_use"
		} else {
			stopReason = "end_turn"
		}
	}
	// message_delta 同时带上 input/cache：异构上游的 usage 只在末尾出现，
	// message_start 已经写成 0；不带的话客户端永远看不到输入 token。
	delta, _ := json.Marshal(map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": AnthropicUsageObject(usage),
	})
	frames = append(frames, AnthropicSSEFrame{Event: "message_delta", Data: delta})
	stop, _ := json.Marshal(map[string]any{"type": "message_stop"})
	frames = append(frames, AnthropicSSEFrame{Event: "message_stop", Data: stop})
	return frames
}

// ensureBlock 保证当前打开的块类型是 kind，必要时关闭旧块并开新块。
func (r *AnthropicStreamRenderer) ensureBlock(kind string) []AnthropicSSEFrame {
	if r.openBlock && r.openKind == kind {
		return nil
	}
	var frames []AnthropicSSEFrame
	if r.openBlock {
		frames = append(frames, r.stopBlock())
	}
	contentBlock := map[string]any{}
	switch kind {
	case "thinking":
		contentBlock = map[string]any{"type": "thinking", "thinking": ""}
	default:
		contentBlock = map[string]any{"type": "text", "text": ""}
	}
	r.openBlock = true
	r.openKind = kind
	r.openIndex = r.nextIndex
	r.nextIndex++
	start, _ := json.Marshal(map[string]any{
		"type":          "content_block_start",
		"index":         r.openIndex,
		"content_block": contentBlock,
	})
	frames = append(frames, AnthropicSSEFrame{Event: "content_block_start", Data: start})
	return frames
}

// delta 生成一个 content_block_delta 帧。
func (r *AnthropicStreamRenderer) delta(deltaType, field, value string) AnthropicSSEFrame {
	data, _ := json.Marshal(map[string]any{
		"type":  "content_block_delta",
		"index": r.openIndex,
		"delta": map[string]any{"type": deltaType, field: value},
	})
	return AnthropicSSEFrame{Event: "content_block_delta", Data: data}
}

// stopBlock 关闭当前块。
func (r *AnthropicStreamRenderer) stopBlock() AnthropicSSEFrame {
	index := r.openIndex
	r.openBlock = false
	r.openKind = ""
	data, _ := json.Marshal(map[string]any{"type": "content_block_stop", "index": index})
	return AnthropicSSEFrame{Event: "content_block_stop", Data: data}
}

// bufferTool 按工具 index 累积 id/name/参数片段（首片才有 id/name）。
func (r *AnthropicStreamRenderer) bufferTool(delta contract.ToolCallDelta) {
	tool, ok := r.toolByIndex[delta.Index]
	if !ok {
		tool = &anthropicStreamTool{index: delta.Index}
		r.toolByIndex[delta.Index] = tool
		r.tools = append(r.tools, tool)
	}
	if delta.ID != "" {
		tool.id = delta.ID
	}
	if delta.Name != "" {
		tool.name = delta.Name
	}
	tool.args.WriteString(delta.ArgumentsFragment)
}

// toolFrames 把一个完整工具调用渲染为 start/delta/stop 三个帧。
func (r *AnthropicStreamRenderer) toolFrames(tool *anthropicStreamTool) []AnthropicSSEFrame {
	index := r.nextIndex
	r.nextIndex++
	// id 缺失时合成（复核 F1）：Gemini functionCall / 旧式 OpenAI function_call
	// 不带 id；空 id 会让下游 tool_result 回传无法被本网关解析。
	id := strings.TrimSpace(tool.id)
	if id == "" {
		id = anthropicToolUseID(r.id, tool.index)
	}
	// name 为空时保留空串：让问题在客户端显式暴露，而不是悄悄丢掉这次
	// 工具调用（调用方仍能看到 id 与参数）。
	start, _ := json.Marshal(map[string]any{
		"type":  "content_block_start",
		"index": index,
		"content_block": map[string]any{
			"type":  "tool_use",
			"id":    id,
			"name":  tool.name,
			"input": map[string]any{},
		},
	})
	args := tool.args.String()
	if strings.TrimSpace(args) == "" {
		// 无参数工具：给一个空对象，避免客户端拼出非法 JSON。
		args = "{}"
	}
	delta, _ := json.Marshal(map[string]any{
		"type":  "content_block_delta",
		"index": index,
		"delta": map[string]any{"type": "input_json_delta", "partial_json": args},
	})
	stop, _ := json.Marshal(map[string]any{"type": "content_block_stop", "index": index})
	return []AnthropicSSEFrame{
		{Event: "content_block_start", Data: start},
		{Event: "content_block_delta", Data: delta},
		{Event: "content_block_stop", Data: stop},
	}
}

// anthropicStreamFinishReason 从上游 Finish 帧的原始 SSE 体里提取结束原因。
//
// 为什么从 Raw 读：契约 StreamEvent 没有 FinishReason 字段（冻结结构），
// 而 EventFinish 保留了上游原始帧。这里只嗅探 OpenAI 的
// choices[].finish_reason 与 Gemini 的 candidates[].finishReason；同协议
// Anthropic 上游走逐字节透传，不经过本渲染器。
func anthropicStreamFinishReason(raw []byte) string {
	payload := sseDataPayload(raw)
	if len(payload) == 0 {
		return ""
	}
	var probe struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Candidates []struct {
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(payload, &probe); err != nil {
		return ""
	}
	for _, choice := range probe.Choices {
		if choice.FinishReason != "" {
			return strings.ToLower(choice.FinishReason)
		}
	}
	for _, candidate := range probe.Candidates {
		if candidate.FinishReason != "" {
			return normalizeGeminiFinishReason(candidate.FinishReason)
		}
	}
	return ""
}

// sseDataPayload 从原始 SSE 事件块里取 data 负载（多 data 行以 \n 合并）。
// 原始体本身就是裸 JSON 时（单测直接喂 JSON），退回整段原文。
func sseDataPayload(raw []byte) []byte {
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return nil
	}
	var data []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if len(data) == 0 {
		// 裸 JSON（无 SSE 包装）：直接当 data 负载。
		if strings.HasPrefix(text, "{") {
			return []byte(text)
		}
		return nil
	}
	return []byte(strings.Join(data, "\n"))
}
