// Package ir 实现协议无关 IR（契约 §2）的 OpenAI Chat Completions 归一化：
// 入站 OpenAI JSON → contract.Request；contract.Request → 出站 OpenAI JSON；
// 上游 OpenAI JSON → contract.Response/contract.Usage；归一化响应 → OpenAI JSON。
//
// 依据：
//   - 契约 `backend/internal/contract/{types,ir}.go`（冻结的 IR 构件）
//   - `docs/relay-02-协议适配器层-设计.md` §5（60 类型规格）、§8（请求构造）
//   - `docs/relay-03-流式与用量提取-设计.md` §6.1/§6.2 row 1（OpenAI usage 字段）
//
// 术语：归一化层 = contract.Request 的字段；原始层 = Request.Raw / Usage.Raw。
// 归一化字段用于通用逻辑；原始层用于透传、未映射字段与对账。
package ir

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"newtg/gateway/internal/contract"
)

// FormatOpenAIChat 是本包写入 `Request.RawFormat` 的协议标识。
//
// 契约只冻结了 `RelayFormat` 类型、没有冻结字面量；全仓暂以 "openai" 表示
// OpenAI Chat Completions（与渠道 slug `openai` 一致）。原始层透传判定据此进行。
const FormatOpenAIChat contract.RelayFormat = "openai"

// ErrParamInvalid 是 IR 归一化的参数错误哨兵，对应 `contract.ErrParamInvalid`
// （契约 §4.1：请求参数非法 → 不重试）。所有结构/取值错误都以 `%w` 包装它，
// 调用方用 `errors.Is` 即可判定是否属于「参数非法」类。
var ErrParamInvalid = errors.New("ir: param invalid")

func paramErrf(format string, args ...any) error {
	return fmt.Errorf("%s: %w", fmt.Sprintf(format, args...), ErrParamInvalid)
}

// ============================================================================
// 入站解析：OpenAI JSON → IR
// ============================================================================

// ParseOpenAIRequest 把 OpenAI Chat Completions 请求体解析为归一化 IR。
//
// 双层 IR 的约定：归一化字段与服务端能理解的语义一一对应；整个原始请求体
// 原样保存在 Raw，供适配器在目标上游同为 OpenAI 形状时透传未映射字段
// （如 logprobs / user / service_tier）。
func ParseOpenAIRequest(body []byte) (*contract.Request, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		// 只接受 JSON 对象：数组/标量/null 都不是 OpenAI 请求体。
		return nil, paramErrf("request body must be a JSON object")
	}
	var payload struct {
		Model    *string           `json:"model"`
		Stream   bool              `json:"stream"`
		Messages []json.RawMessage `json:"messages"`
		Tools    []json.RawMessage `json:"tools"`
		// tool_choice / response_format / stop 的 JSON 形状有多个变体，
		// 交给专用解析函数，避免在 DTO 里丢信息。
		ToolChoice     json.RawMessage `json:"tool_choice"`
		ResponseFormat json.RawMessage `json:"response_format"`
		Stop           json.RawMessage `json:"stop"`
		MaxTokens      *int            `json:"max_tokens"`
		// OpenAI 新字段：部分客户端只发 max_completion_tokens。
		// 归一化到 MaxTokens（契约只定义了 MaxTokens），构造时回写 max_tokens。
		MaxCompletionTokens *int     `json:"max_completion_tokens"`
		Temperature         *float64 `json:"temperature"`
		TopP                *float64 `json:"top_p"`
		Seed                *int     `json:"seed"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, paramErrf("invalid OpenAI request JSON: %v", err)
	}

	req := &contract.Request{
		Stream:    payload.Stream,
		RawFormat: FormatOpenAIChat,
		Raw:       append(contract.ProtocolBody(nil), body...),
	}
	if payload.Model != nil {
		req.Model = *payload.Model
	}
	if err := validateScalars(payload.MaxTokens, payload.Temperature, payload.TopP); err != nil {
		return nil, err
	}
	req.MaxTokens = payload.MaxTokens
	if req.MaxTokens == nil {
		req.MaxTokens = payload.MaxCompletionTokens
	}
	req.Temperature = payload.Temperature
	req.TopP = payload.TopP
	req.Seed = payload.Seed

	msgs, err := parseMessages(payload.Messages)
	if err != nil {
		return nil, err
	}
	req.Messages = msgs

	tools, err := parseTools(payload.Tools)
	if err != nil {
		return nil, err
	}
	req.Tools = tools

	if req.ToolChoice, err = parseToolChoice(payload.ToolChoice); err != nil {
		return nil, err
	}
	if req.ResponseFormat, err = parseResponseFormat(payload.ResponseFormat); err != nil {
		return nil, err
	}
	if req.Stop, err = parseStop(payload.Stop); err != nil {
		return nil, err
	}
	return req, nil
}

func parseMessages(raws []json.RawMessage) ([]contract.Message, error) {
	if len(raws) == 0 {
		return nil, nil
	}
	out := make([]contract.Message, 0, len(raws))
	for i, raw := range raws {
		msg, err := parseMessage(raw, "")
		if err != nil {
			return nil, fmt.Errorf("messages[%d]: %w", i, err)
		}
		out = append(out, msg)
	}
	return out, nil
}

// parseMessage 解析一条 OpenAI 消息。defaultRole 非空时用于「上游响应未写 role」
// 的兼容场景（响应的消息默认 assistant）；请求路径传空串，role 缺失即报错。
func parseMessage(raw json.RawMessage, defaultRole contract.Role) (contract.Message, error) {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return contract.Message{}, paramErrf("message 必须是 JSON 对象")
	}
	roleStr, _ := obj["role"].(string)
	var role contract.Role
	var err error
	if roleStr == "" && defaultRole != "" {
		role = defaultRole
	} else if role, err = parseRole(roleStr); err != nil {
		return contract.Message{}, err
	}

	msg := contract.Message{Role: role}

	if content, ok := obj["content"]; ok && content != nil {
		parts, err := parseContentValue(content)
		if err != nil {
			return contract.Message{}, err
		}
		msg.Content = append(msg.Content, parts...)
	}

	if toolCalls, ok := obj["tool_calls"]; ok && toolCalls != nil {
		parts, err := parseToolCalls(toolCalls)
		if err != nil {
			return contract.Message{}, err
		}
		msg.Content = append(msg.Content, parts...)
	}
	// 遗留 function_call：映射为单条 tool_call（契约没有独立函数调用构件）。
	if functionCall, ok := obj["function_call"]; ok && functionCall != nil {
		part, err := parseFunctionCall(functionCall)
		if err != nil {
			return contract.Message{}, err
		}
		msg.Content = append(msg.Content, part)
	}

	// tool 角色的内容语义是「工具结果」，整条消息归一为一个 tool_result 部件。
	if role == contract.RoleTool {
		callID, _ := obj["tool_call_id"].(string)
		msg.Content = []contract.ContentPart{{
			Kind: contract.PartToolResult,
			Text: joinTextParts(msg.Content),
			Meta: map[string]any{"tool_call_id": callID},
		}}
	}

	// 协议特有但需保留的消息级字段 → Extensions（契约 ir.go 的字段注释）。
	for key, value := range obj {
		switch key {
		case "role", "content", "tool_calls", "function_call", "tool_call_id":
		default:
			if msg.Extensions == nil {
				msg.Extensions = map[string]any{}
			}
			msg.Extensions[key] = value
		}
	}
	return msg, nil
}

func parseRole(s string) (contract.Role, error) {
	switch role := contract.Role(s); role {
	case contract.RoleSystem, contract.RoleDeveloper, contract.RoleUser, contract.RoleAssistant, contract.RoleTool:
		return role, nil
	default:
		// 白名单之外的 role 会让下游适配器无从映射；按参数非法拒绝（不静默猜测）。
		return "", paramErrf("unsupported message role %q", s)
	}
}

func parseContentValue(value any) ([]contract.ContentPart, error) {
	switch content := value.(type) {
	case string:
		return []contract.ContentPart{{Kind: contract.PartText, Text: content}}, nil
	case []any:
		parts := make([]contract.ContentPart, 0, len(content))
		for i, item := range content {
			part, err := parseContentPart(item)
			if err != nil {
				return nil, fmt.Errorf("content[%d]: %w", i, err)
			}
			parts = append(parts, part)
		}
		return parts, nil
	default:
		return nil, paramErrf("content 必须是字符串或部件数组")
	}
}

func parseContentPart(value any) (contract.ContentPart, error) {
	obj, ok := value.(map[string]any)
	if !ok {
		return contract.ContentPart{}, paramErrf("content 部件必须是 JSON 对象")
	}
	partType, _ := obj["type"].(string)
	switch partType {
	case "text":
		text, _ := obj["text"].(string)
		return contract.ContentPart{Kind: contract.PartText, Text: text}, nil
	case "image_url":
		meta := map[string]any{}
		var rawURL string
		switch imageURL := obj["image_url"].(type) {
		case string:
			rawURL = imageURL
		case map[string]any:
			rawURL, _ = imageURL["url"].(string)
			if detail, ok := imageURL["detail"].(string); ok && detail != "" {
				meta["detail"] = detail
			}
		default:
			return contract.ContentPart{}, paramErrf("image_url 部件缺少 image_url 对象")
		}
		return parseImagePart(rawURL, meta)
	case "input_audio":
		audio, ok := obj["input_audio"].(map[string]any)
		if !ok {
			return contract.ContentPart{}, paramErrf("input_audio 部件缺少 input_audio 对象")
		}
		data, _ := audio["data"].(string)
		format, _ := audio["format"].(string)
		payload, err := base64.StdEncoding.DecodeString(data)
		if err != nil {
			return contract.ContentPart{}, paramErrf("input_audio.data 不是合法 base64: %v", err)
		}
		meta := map[string]any{"format": format}
		if format != "" {
			meta["mime_type"] = "audio/" + format
		}
		return contract.ContentPart{Kind: contract.PartAudio, Payload: payload, Meta: meta}, nil
	default:
		// 未建模的部件类型无法进入 IR；静默丢弃会造成请求内容缺失，按参数非法拒绝。
		return contract.ContentPart{}, paramErrf("unsupported content part type %q", partType)
	}
}

// parseImagePart 把 image_url 归一为 PartImageURL 或 PartImageB64。
// data: URL 自带完整字节，归一到 PartImageB64（Payload + mime_type），
// 其余 HTTP(S) URL 归一到 PartImageURL；两条路径都可无损回写 OpenAI 形状。
func parseImagePart(rawURL string, meta map[string]any) (contract.ContentPart, error) {
	if strings.HasPrefix(rawURL, "data:") {
		header, data, ok := strings.Cut(strings.TrimPrefix(rawURL, "data:"), ",")
		if !ok || !strings.HasSuffix(header, ";base64") {
			return contract.ContentPart{}, paramErrf("非法 data URL（缺少 ;base64,）")
		}
		payload, err := base64.StdEncoding.DecodeString(data)
		if err != nil {
			return contract.ContentPart{}, paramErrf("data URL 的 base64 负载非法: %v", err)
		}
		if meta == nil {
			meta = map[string]any{}
		}
		meta["mime_type"] = strings.TrimSuffix(header, ";base64")
		return contract.ContentPart{Kind: contract.PartImageB64, Payload: payload, Meta: meta}, nil
	}
	if rawURL == "" {
		return contract.ContentPart{}, paramErrf("image_url.url 为空")
	}
	if len(meta) == 0 {
		meta = nil
	}
	return contract.ContentPart{Kind: contract.PartImageURL, URL: rawURL, Meta: meta}, nil
}

func parseToolCalls(value any) ([]contract.ContentPart, error) {
	items, ok := value.([]any)
	if !ok {
		return nil, paramErrf("tool_calls 必须是数组")
	}
	parts := make([]contract.ContentPart, 0, len(items))
	for i, item := range items {
		obj, ok := item.(map[string]any)
		if !ok {
			return nil, paramErrf("tool_calls[%d] 必须是 JSON 对象", i)
		}
		callType, _ := obj["type"].(string)
		if callType != "" && callType != "function" {
			return nil, paramErrf("tool_calls[%d] 的类型 %q 不受支持", i, callType)
		}
		function, ok := obj["function"].(map[string]any)
		if !ok {
			return nil, paramErrf("tool_calls[%d] 缺少 function 对象", i)
		}
		name, _ := function["name"].(string)
		arguments := argumentsString(function["arguments"])
		id, _ := obj["id"].(string)
		parts = append(parts, contract.ContentPart{
			Kind: contract.PartToolCall,
			Text: arguments,
			Meta: map[string]any{"id": id, "name": name},
		})
	}
	return parts, nil
}

func parseFunctionCall(value any) (contract.ContentPart, error) {
	obj, ok := value.(map[string]any)
	if !ok {
		return contract.ContentPart{}, paramErrf("function_call 必须是 JSON 对象")
	}
	name, _ := obj["name"].(string)
	return contract.ContentPart{
		Kind: contract.PartToolCall,
		Text: argumentsString(obj["arguments"]),
		Meta: map[string]any{"id": "", "name": name},
	}, nil
}

// argumentsString 兼容参数为 JSON 字符串或对象两种上游写法。
func argumentsString(value any) string {
	switch arguments := value.(type) {
	case string:
		return arguments
	case map[string]any, []any:
		encoded, err := json.Marshal(arguments)
		if err != nil {
			return ""
		}
		return string(encoded)
	default:
		return ""
	}
}

func joinTextParts(parts []contract.ContentPart) string {
	var builder strings.Builder
	for _, part := range parts {
		if part.Kind == contract.PartText {
			builder.WriteString(part.Text)
		}
	}
	return builder.String()
}

func parseTools(raws []json.RawMessage) ([]contract.Tool, error) {
	if len(raws) == 0 {
		return nil, nil
	}
	tools := make([]contract.Tool, 0, len(raws))
	for i, raw := range raws {
		var tool struct {
			Type     string `json:"type"`
			Function struct {
				Name        string         `json:"name"`
				Description string         `json:"description"`
				Parameters  map[string]any `json:"parameters"`
			} `json:"function"`
		}
		if err := json.Unmarshal(raw, &tool); err != nil {
			return nil, paramErrf("tools[%d] 不是合法 JSON 对象", i)
		}
		if tool.Type != "" && tool.Type != "function" {
			return nil, paramErrf("tools[%d] 的类型 %q 不受支持", i, tool.Type)
		}
		if tool.Function.Name == "" {
			return nil, paramErrf("tools[%d] 缺少 function.name", i)
		}
		tools = append(tools, contract.Tool{
			Name:        tool.Function.Name,
			Description: tool.Function.Description,
			Schema:      tool.Function.Parameters,
		})
	}
	return tools, nil
}

func parseToolChoice(raw json.RawMessage) (contract.ToolChoice, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return contract.ToolChoice{}, nil
	}
	var mode string
	if err := json.Unmarshal(raw, &mode); err == nil {
		switch choice := contract.ToolChoiceMode(mode); choice {
		case contract.ToolChoiceAuto, contract.ToolChoiceNone, contract.ToolChoiceRequired:
			return contract.ToolChoice{Mode: choice}, nil
		default:
			return contract.ToolChoice{}, paramErrf("tool_choice 模式 %q 不受支持", mode)
		}
	}
	var obj struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return contract.ToolChoice{}, paramErrf("tool_choice 形状非法")
	}
	if obj.Type != "function" {
		return contract.ToolChoice{}, paramErrf("tool_choice.type %q 不受支持", obj.Type)
	}
	// 契约不变量：Mode == specific 时 Name 必须非空（contract/ir.go）。
	if obj.Function.Name == "" {
		return contract.ToolChoice{}, paramErrf("tool_choice specific 缺少 function.name")
	}
	return contract.ToolChoice{Mode: contract.ToolChoiceSpecific, Name: obj.Function.Name}, nil
}

func parseResponseFormat(raw json.RawMessage) (*contract.ResponseFormat, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var obj struct {
		Type       string `json:"type"`
		JSONSchema *struct {
			Schema map[string]any `json:"schema"`
		} `json:"json_schema"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, paramErrf("response_format 形状非法")
	}
	switch contract.ResponseFormatKind(obj.Type) {
	case contract.ResponseFormatText:
		return &contract.ResponseFormat{Kind: contract.ResponseFormatText}, nil
	case contract.ResponseFormatJSONObject:
		return &contract.ResponseFormat{Kind: contract.ResponseFormatJSONObject}, nil
	case contract.ResponseFormatJSONSchema:
		var schema map[string]any
		if obj.JSONSchema != nil {
			schema = obj.JSONSchema.Schema
		}
		return &contract.ResponseFormat{Kind: contract.ResponseFormatJSONSchema, Schema: schema}, nil
	default:
		return nil, paramErrf("response_format.type %q 不受支持", obj.Type)
	}
}

func parseStop(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		return []string{single}, nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err == nil {
		return many, nil
	}
	return nil, paramErrf("stop 必须是字符串或字符串数组")
}

// validateScalars 做会成为计费乘数的标量的基础校验（billing.md：
// 上界由归一化层钳制、负值一律拒绝）。上界依赖全局配置，属执行层职责。
func validateScalars(maxTokens *int, temperature, topP *float64) error {
	if maxTokens != nil && *maxTokens < 0 {
		return paramErrf("max_tokens 不得为负")
	}
	if temperature != nil && *temperature < 0 {
		return paramErrf("temperature 不得为负")
	}
	if topP != nil && *topP < 0 {
		return paramErrf("top_p 不得为负")
	}
	return nil
}

// ============================================================================
// 出站构造：IR → OpenAI JSON
// ============================================================================

// BuildOpenAIRequestBody 把归一化 IR 编码为 OpenAI Chat Completions 请求体。
//
// 透传策略（双层 IR）：当 RawFormat 与 OpenAI Chat 同形时，以 Raw 的 JSON 对象
// 为基底，再用归一化字段覆盖已建模部分——这样未映射字段（logprobs / user /
// service_tier 等）被保留，而已归一化字段（model/messages/…）以 IR 为准。
// 非 OpenAI 形状的 Raw 不参与透传，避免把异种协议字段混入 OpenAI 请求。
func BuildOpenAIRequestBody(req *contract.Request) ([]byte, error) {
	if req == nil {
		return nil, paramErrf("nil request")
	}
	obj := map[string]any{}
	if len(req.Raw) > 0 && isOpenAIRawFormat(req.RawFormat) {
		var passthrough map[string]any
		if err := json.Unmarshal(req.Raw, &passthrough); err != nil {
			return nil, paramErrf("Raw 不是合法 JSON 对象: %v", err)
		}
		// `null` 会解码为 nil map；此时没有可透传字段，保持空对象以免向 nil map 写入。
		if passthrough != nil {
			obj = passthrough
		}
	}

	// 已建模字段一律以归一化层为准。
	obj["model"] = req.Model
	obj["stream"] = req.Stream
	messages, err := encodeMessages(req.Messages)
	if err != nil {
		return nil, err
	}
	obj["messages"] = messages

	if len(req.Tools) > 0 {
		tools, err := encodeTools(req.Tools)
		if err != nil {
			return nil, err
		}
		obj["tools"] = tools
	}
	if req.ToolChoice.Mode != "" {
		choice, err := encodeToolChoice(req.ToolChoice)
		if err != nil {
			return nil, err
		}
		obj["tool_choice"] = choice
	}
	if req.ResponseFormat != nil {
		format, err := encodeResponseFormat(req.ResponseFormat)
		if err != nil {
			return nil, err
		}
		obj["response_format"] = format
	}
	if req.MaxTokens != nil {
		obj["max_tokens"] = *req.MaxTokens
		// 归一化只保留一个 max_tokens 维度；删除 Raw 里可能并存的
		// max_completion_tokens，避免同一语义下发两次。
		delete(obj, "max_completion_tokens")
	}
	if req.Temperature != nil {
		obj["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		obj["top_p"] = *req.TopP
	}
	if len(req.Stop) > 0 {
		obj["stop"] = req.Stop
	}
	if req.Seed != nil {
		obj["seed"] = *req.Seed
	}
	return json.Marshal(obj)
}

// isOpenAIRawFormat 判断原始层是否与 OpenAI Chat 同形。
//
// 契约未冻结 RelayFormat 字面量，故此处接受常见拼写；未知格式一律按
// 「不同形」处理（不透传），宁可少透传也不污染目标协议。
func isOpenAIRawFormat(format contract.RelayFormat) bool {
	switch strings.ToLower(string(format)) {
	case "", "openai", "openai_chat", "openai_chat_completions", "chat", "chat_completions":
		return true
	default:
		return false
	}
}

func encodeMessages(messages []contract.Message) ([]any, error) {
	out := make([]any, 0, len(messages))
	for i, message := range messages {
		encoded, err := encodeMessage(message)
		if err != nil {
			return nil, fmt.Errorf("messages[%d]: %w", i, err)
		}
		out = append(out, encoded...)
	}
	return out, nil
}

// encodeMessage 返回一个或多个 OpenAI 消息：tool 角色的一条 IR 消息若含多个
// tool_result 部件，必须展开为多条 OpenAI tool 消息（每个调用 ID 一条）。
func encodeMessage(message contract.Message) ([]any, error) {
	if message.Role == contract.RoleTool {
		var results []contract.ContentPart
		for _, part := range message.Content {
			if part.Kind == contract.PartToolResult {
				results = append(results, part)
			}
		}
		if len(results) > 0 {
			out := make([]any, 0, len(results))
			for _, result := range results {
				obj := map[string]any{"role": string(contract.RoleTool), "content": result.Text}
				callID := metaString(result.Meta, "tool_call_id")
				if callID == "" {
					return nil, paramErrf("tool_result 缺少 tool_call_id")
				}
				obj["tool_call_id"] = callID
				mergeExtensions(obj, message.Extensions)
				out = append(out, obj)
			}
			return out, nil
		}
	}

	obj := map[string]any{"role": string(message.Role)}
	mergeExtensions(obj, message.Extensions)

	content := make([]any, 0, len(message.Content))
	var toolCalls []any
	plainText := ""
	nonText := false
	for _, part := range message.Content {
		switch part.Kind {
		case contract.PartText:
			content = append(content, map[string]any{"type": "text", "text": part.Text})
			plainText += part.Text
		case contract.PartToolCall:
			call, err := encodeToolCall(part)
			if err != nil {
				return nil, err
			}
			toolCalls = append(toolCalls, call)
		case contract.PartToolResult:
			return nil, paramErrf("tool_result 只能出现在 tool 角色消息")
		default:
			encoded, err := encodeContentPart(part)
			if err != nil {
				return nil, err
			}
			content = append(content, encoded)
			nonText = true
		}
	}

	switch {
	case len(message.Content) == 0:
		// assistant + tool_calls 的规范形状是 content:null；其余空消息写空串。
		if len(toolCalls) > 0 {
			obj["content"] = nil
		} else {
			obj["content"] = ""
		}
	case len(content) == 1 && !nonText:
		// 纯文本单部件写成字符串（OpenAI 最常见的规范形状）。
		obj["content"] = plainText
	case len(content) > 0:
		obj["content"] = content
	default:
		obj["content"] = nil
	}
	if len(toolCalls) > 0 {
		obj["tool_calls"] = toolCalls
	}
	return []any{obj}, nil
}

func encodeContentPart(part contract.ContentPart) (map[string]any, error) {
	switch part.Kind {
	case contract.PartText:
		return map[string]any{"type": "text", "text": part.Text}, nil
	case contract.PartImageURL:
		if part.URL == "" {
			return nil, paramErrf("image_url 部件缺少 URL")
		}
		imageURL := map[string]any{"url": part.URL}
		if detail := metaString(part.Meta, "detail"); detail != "" {
			imageURL["detail"] = detail
		}
		return map[string]any{"type": "image_url", "image_url": imageURL}, nil
	case contract.PartImageB64:
		if len(part.Payload) == 0 {
			return nil, paramErrf("image_b64 部件缺少负载")
		}
		mimeType := metaString(part.Meta, "mime_type")
		if mimeType == "" {
			mimeType = "image/png"
		}
		url := "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(part.Payload)
		imageURL := map[string]any{"url": url}
		if detail := metaString(part.Meta, "detail"); detail != "" {
			imageURL["detail"] = detail
		}
		return map[string]any{"type": "image_url", "image_url": imageURL}, nil
	case contract.PartAudio:
		if len(part.Payload) == 0 {
			return nil, paramErrf("audio 部件缺少负载")
		}
		format := metaString(part.Meta, "format")
		if format == "" {
			format = "wav"
		}
		return map[string]any{
			"type": "input_audio",
			"input_audio": map[string]any{
				"data":   base64.StdEncoding.EncodeToString(part.Payload),
				"format": format,
			},
		}, nil
	case contract.PartToolCall, contract.PartToolResult:
		return nil, paramErrf("部件类型 %q 不能作为消息内容编码", part.Kind)
	default:
		return nil, paramErrf("未知部件类型 %q", part.Kind)
	}
}

func encodeToolCall(part contract.ContentPart) (map[string]any, error) {
	name := metaString(part.Meta, "name")
	if name == "" {
		return nil, paramErrf("tool_call 缺少 function.name")
	}
	id := metaString(part.Meta, "id")
	return map[string]any{
		"id":   id,
		"type": "function",
		"function": map[string]any{
			"name":      name,
			"arguments": part.Text,
		},
	}, nil
}

func encodeTools(tools []contract.Tool) ([]any, error) {
	out := make([]any, 0, len(tools))
	for i, tool := range tools {
		if tool.Name == "" {
			return nil, paramErrf("tools[%d] 缺少 name", i)
		}
		function := map[string]any{
			"name":       tool.Name,
			"parameters": tool.Schema,
		}
		if tool.Description != "" {
			function["description"] = tool.Description
		}
		out = append(out, map[string]any{"type": "function", "function": function})
	}
	return out, nil
}

func encodeToolChoice(choice contract.ToolChoice) (any, error) {
	switch choice.Mode {
	case contract.ToolChoiceAuto, contract.ToolChoiceNone, contract.ToolChoiceRequired:
		return string(choice.Mode), nil
	case contract.ToolChoiceSpecific:
		if choice.Name == "" {
			return nil, paramErrf("tool_choice specific 缺少 name")
		}
		return map[string]any{
			"type":     "function",
			"function": map[string]any{"name": choice.Name},
		}, nil
	default:
		return nil, paramErrf("tool_choice 模式 %q 不受支持", choice.Mode)
	}
}

func encodeResponseFormat(format *contract.ResponseFormat) (map[string]any, error) {
	switch format.Kind {
	case contract.ResponseFormatText:
		return map[string]any{"type": "text"}, nil
	case contract.ResponseFormatJSONObject:
		return map[string]any{"type": "json_object"}, nil
	case contract.ResponseFormatJSONSchema:
		// 契约的 ResponseFormat 没有 name 字段（OpenAI 要求 json_schema.name）；
		// 用固定占位名 "response"，schema 原样下发。
		return map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   "response",
				"schema": format.Schema,
			},
		}, nil
	default:
		return nil, paramErrf("response_format 类型 %q 不受支持", format.Kind)
	}
}

func mergeExtensions(obj map[string]any, extensions map[string]any) {
	for key, value := range extensions {
		// finish_reason 是 Choice 级字段（契约 Choice 注释把它保留在
		// Message.Extensions 供诊断）；编码响应时由 Choice 层级负责，不能再塞回消息体。
		if key == "finish_reason" {
			continue
		}
		if _, exists := obj[key]; !exists {
			obj[key] = value
		}
	}
}

func metaString(meta map[string]any, key string) string {
	if meta == nil {
		return ""
	}
	value, _ := meta[key].(string)
	return value
}

// ============================================================================
// 上游响应解析：OpenAI JSON → contract.Response/Usage
// ============================================================================

// ParseOpenAIResponse 解析 OpenAI Chat Completions 非流式响应。
// 用量归一化见 relay-03 §6.1/§6.2 row 1：GPT 格式的 prompt 已含缓存读、
// completion 已含推理 token，两个关系布尔必须显式为 true。
func ParseOpenAIResponse(body []byte) (*contract.Response, contract.Usage, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, contract.Usage{}, paramErrf("response body must be a JSON object")
	}
	var payload struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Index        int             `json:"index"`
			Message      json.RawMessage `json:"message"`
			FinishReason string          `json:"finish_reason"`
		} `json:"choices"`
		Usage json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, contract.Usage{}, paramErrf("invalid OpenAI response JSON: %v", err)
	}

	response := &contract.Response{
		ID:    payload.ID,
		Model: payload.Model,
		Raw:   append(contract.ProtocolBody(nil), body...),
	}
	for _, choice := range payload.Choices {
		message := contract.Message{Role: contract.RoleAssistant}
		if len(choice.Message) > 0 {
			parsed, err := parseMessage(choice.Message, contract.RoleAssistant)
			if err != nil {
				return nil, contract.Usage{}, fmt.Errorf("choices[%d].message: %w", choice.Index, err)
			}
			message = parsed
		}
		if choice.FinishReason != "" {
			if message.Extensions == nil {
				message.Extensions = map[string]any{}
			}
			// 契约 Choice 注释：上游原始结束原因保留在 Extensions，供诊断与转码。
			message.Extensions["finish_reason"] = choice.FinishReason
		}
		response.Choices = append(response.Choices, contract.Choice{
			Index:        choice.Index,
			Message:      message,
			FinishReason: choice.FinishReason,
		})
	}
	if len(payload.Choices) > 0 {
		response.FinishReason = payload.Choices[0].FinishReason
	}
	return response, parseOpenAIUsage(payload.Usage), nil
}

// ParseOpenAIUsage 归一一个 OpenAI usage JSON 对象，供流式逐块解析复用
// （include_usage 的末块与非流式整包走同一套字段路径，relay-03 §6 row 1）。
// raw 为 usage 对象的原始 JSON；空/nil 返回 UsageEstimated。
func ParseOpenAIUsage(raw []byte) contract.Usage {
	return parseOpenAIUsage(raw)
}

// parseOpenAIUsage 归一 OpenAI usage。
//
// 字段路径严格按 relay-03 §6.2 row 1：
//   - Prompt/Completion/Total ← prompt_tokens/completion_tokens/total_tokens
//   - CR ← prompt_tokens_details.cached_tokens；DeepSeek 变体 prompt_cache_hit_tokens 兜底
//   - CW ← prompt_tokens_details.cache_write_tokens（非标准）
//   - RZ ← completion_tokens_details.reasoning_tokens
//   - 子类：prompt 的 image/audio token；completion 的 audio token
//
// 所有维度钳 ≥0（relay-03 §6.1 规则 1）；原始对象保留在 Usage.Raw（规则 7）。
// 无 usage 时返回 UsageEstimated，提示调用方按 relay-03 §7 走兜底估算。
func parseOpenAIUsage(raw json.RawMessage) contract.Usage {
	if len(raw) == 0 || string(raw) == "null" {
		// 关系布尔是协议属性（GPT 格式），与 usage 是否出现无关，必须显式赋值。
		return contract.Usage{
			PromptIncludesCache:           true,
			ReasoningIncludedInCompletion: true,
			Source:                        contract.UsageEstimated,
		}
	}
	var payload struct {
		PromptTokens        int64 `json:"prompt_tokens"`
		CompletionTokens    int64 `json:"completion_tokens"`
		TotalTokens         int64 `json:"total_tokens"`
		PromptCacheHitToken int64 `json:"prompt_cache_hit_tokens"` // DeepSeek 变体
		PromptTokensDetails *struct {
			CachedTokens     int64 `json:"cached_tokens"`
			CacheWriteTokens int64 `json:"cache_write_tokens"`
			ImageTokens      int64 `json:"image_tokens"`
			AudioTokens      int64 `json:"audio_tokens"`
		} `json:"prompt_tokens_details"`
		CompletionTokensDetails *struct {
			ReasoningTokens int64 `json:"reasoning_tokens"`
			AudioTokens     int64 `json:"audio_tokens"`
		} `json:"completion_tokens_details"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		// usage 字段类型异常时退化为「需估算」，而不是让整条响应解析失败。
		return contract.Usage{
			PromptIncludesCache:           true,
			ReasoningIncludedInCompletion: true,
			Source:                        contract.UsageEstimated,
		}
	}
	var rawMap map[string]any
	_ = json.Unmarshal(raw, &rawMap)

	usage := contract.Usage{
		Prompt:                        nonNeg(payload.PromptTokens),
		Completion:                    nonNeg(payload.CompletionTokens),
		Total:                         nonNeg(payload.TotalTokens),
		PromptIncludesCache:           true,
		ReasoningIncludedInCompletion: true,
		Source:                        contract.UsageUpstreamExact,
		Raw:                           rawMap,
	}

	var cacheRead, cacheWrite, promptImage, promptAudio int64
	if details := payload.PromptTokensDetails; details != nil {
		cacheRead = nonNeg(details.CachedTokens)
		cacheWrite = nonNeg(details.CacheWriteTokens)
		promptImage = nonNeg(details.ImageTokens)
		promptAudio = nonNeg(details.AudioTokens)
	}
	if cacheRead == 0 {
		cacheRead = nonNeg(payload.PromptCacheHitToken)
	}
	var reasoning, completionAudio int64
	if details := payload.CompletionTokensDetails; details != nil {
		reasoning = nonNeg(details.ReasoningTokens)
		completionAudio = nonNeg(details.AudioTokens)
	}

	usage.CacheRead = cacheRead
	usage.CacheWrite = cacheWrite
	usage.Reasoning = reasoning
	usage.PromptDetail = contract.UsageDetail{
		Image:      promptImage,
		Audio:      promptAudio,
		Text:       nonNeg(usage.Prompt - promptImage - promptAudio),
		CacheRead:  cacheRead,
		CacheWrite: cacheWrite,
	}
	usage.CompletionDetail = contract.UsageDetail{
		Audio: completionAudio,
		// Completion 已包含 Reasoning（ReasoningIncludedInCompletion=true），
		// 文本子类须扣除音频与推理，避免阶梯价重复计入。
		Text: nonNeg(usage.Completion - completionAudio - reasoning),
	}
	// total_tokens 缺失时按 Prompt+Completion 派生（仅在两者非零时）。
	if usage.Total == 0 && usage.Prompt+usage.Completion > 0 {
		usage.Total = usage.Prompt + usage.Completion
	}
	return usage
}

func nonNeg(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}

// ============================================================================
// IR 归一化与回写（brief 要求的 ToIR / FromIR）
// ============================================================================

// ToIR 校验并归一化一份 IR，返回深拷贝（不得让调用方通过返回值影响入参）。
//
// channelType 是目标渠道类型：它必须通过契约白名单 `ChannelType.Valid()`
// （channels.go 是 60 类型白名单的唯一出处）。当前实现的协议归一化仅覆盖
// OpenAI 形状；类型级差异（如 Azure 路径）由适配器读取 RelayContext 处理。
func ToIR(req *contract.Request, channelType contract.ChannelType) (*contract.Request, error) {
	if req == nil {
		return nil, paramErrf("nil request")
	}
	if channelType != contract.ChannelTypeUnknown && !channelType.Valid() {
		return nil, paramErrf("unknown channel type %q", channelType)
	}
	out := cloneRequest(req)
	if err := validateRequest(out); err != nil {
		return nil, err
	}
	return out, nil
}

func validateRequest(req *contract.Request) error {
	validRoles := map[contract.Role]bool{
		contract.RoleSystem: true, contract.RoleDeveloper: true, contract.RoleUser: true,
		contract.RoleAssistant: true, contract.RoleTool: true,
	}
	validParts := map[contract.PartKind]bool{
		contract.PartText: true, contract.PartImageURL: true, contract.PartImageB64: true,
		contract.PartAudio: true, contract.PartToolCall: true, contract.PartToolResult: true,
	}
	for i, message := range req.Messages {
		if !validRoles[message.Role] {
			return paramErrf("messages[%d] 角色 %q 非法", i, message.Role)
		}
		for j, part := range message.Content {
			if !validParts[part.Kind] {
				return paramErrf("messages[%d].content[%d] 类型 %q 非法", i, j, part.Kind)
			}
			if part.Kind == contract.PartToolCall && metaString(part.Meta, "name") == "" {
				return paramErrf("messages[%d].content[%d] tool_call 缺少 name", i, j)
			}
			if part.Kind == contract.PartToolResult && metaString(part.Meta, "tool_call_id") == "" {
				return paramErrf("messages[%d].content[%d] tool_result 缺少 tool_call_id", i, j)
			}
		}
	}
	for i, tool := range req.Tools {
		if tool.Name == "" {
			return paramErrf("tools[%d] 缺少 name", i)
		}
	}
	switch req.ToolChoice.Mode {
	case "", contract.ToolChoiceAuto, contract.ToolChoiceNone, contract.ToolChoiceRequired:
	case contract.ToolChoiceSpecific:
		if req.ToolChoice.Name == "" {
			return paramErrf("tool_choice specific 缺少 name")
		}
	default:
		return paramErrf("tool_choice 模式 %q 非法", req.ToolChoice.Mode)
	}
	if format := req.ResponseFormat; format != nil {
		switch format.Kind {
		case contract.ResponseFormatText, contract.ResponseFormatJSONObject, contract.ResponseFormatJSONSchema:
		default:
			return paramErrf("response_format 类型 %q 非法", format.Kind)
		}
	}
	return validateScalars(req.MaxTokens, req.Temperature, req.TopP)
}

func cloneRequest(in *contract.Request) *contract.Request {
	out := *in
	out.Raw = append(contract.ProtocolBody(nil), in.Raw...)
	out.Stop = append([]string(nil), in.Stop...)
	out.Messages = make([]contract.Message, len(in.Messages))
	for i, message := range in.Messages {
		out.Messages[i] = cloneMessage(message)
	}
	out.Tools = make([]contract.Tool, len(in.Tools))
	for i, tool := range in.Tools {
		out.Tools[i] = contract.Tool{
			Name:        tool.Name,
			Description: tool.Description,
			Schema:      cloneMap(tool.Schema),
		}
	}
	if in.ResponseFormat != nil {
		format := *in.ResponseFormat
		format.Schema = cloneMap(in.ResponseFormat.Schema)
		out.ResponseFormat = &format
	}
	out.MaxTokens = cloneInt(in.MaxTokens)
	out.Temperature = cloneFloat(in.Temperature)
	out.TopP = cloneFloat(in.TopP)
	out.Seed = cloneInt(in.Seed)
	return &out
}

func cloneMessage(in contract.Message) contract.Message {
	out := in
	out.Content = make([]contract.ContentPart, len(in.Content))
	for i, part := range in.Content {
		out.Content[i] = contract.ContentPart{
			Kind:    part.Kind,
			Text:    part.Text,
			Payload: append([]byte(nil), part.Payload...),
			URL:     part.URL,
			Meta:    cloneMap(part.Meta),
		}
	}
	out.Extensions = cloneMap(in.Extensions)
	return out
}

// cloneMap 深拷贝 map[string]any（含嵌套 map/切片），其余标量类型不可变，直接复用。
func cloneMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = cloneAny(value)
	}
	return out
}

func cloneAny(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneMap(typed)
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = cloneAny(item)
		}
		return out
	case []string:
		return append([]string(nil), typed...)
	case []byte:
		return append([]byte(nil), typed...)
	default:
		return value
	}
}

func cloneInt(in *int) *int {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func cloneFloat(in *float64) *float64 {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

// FromIR 把归一化响应回写为 OpenAI Chat Completions 形状的 JSON，存入副本的 Raw。
//
// 仅从原始响应中保留少量非结构性元数据（created/system_fingerprint/usage/
// service_tier）；choices/id/model 等结构字段一律由归一化层重建，避免把异种
// 协议（如 Claude/Gemini）的字段混进 OpenAI 负载。
func FromIR(resp *contract.Response) (*contract.Response, error) {
	if resp == nil {
		return nil, paramErrf("nil response")
	}
	out := *resp
	if len(resp.Binary) > 0 || resp.BinaryStream != nil {
		// 二进制载体（TTS 等）没有 JSON 结构，保持原样（契约 §2.3 决议 C3）。
		return &out, nil
	}

	obj := map[string]any{}
	if len(resp.Raw) > 0 {
		var existing map[string]any
		if json.Unmarshal(resp.Raw, &existing) == nil {
			for _, key := range []string{"created", "system_fingerprint", "usage", "service_tier"} {
				if value, ok := existing[key]; ok {
					obj[key] = value
				}
			}
		}
	}
	obj["object"] = "chat.completion"
	obj["id"] = resp.ID
	obj["model"] = resp.Model
	if _, ok := obj["created"]; !ok {
		// 契约 Response 没有 created 字段；用当前时间补齐 OpenAI 形状。
		obj["created"] = time.Now().Unix()
	}

	choices := make([]any, 0, len(resp.Choices))
	for _, choice := range resp.Choices {
		encoded, err := encodeMessage(choice.Message)
		if err != nil {
			return nil, fmt.Errorf("choices[%d]: %w", choice.Index, err)
		}
		// 响应侧一条 choice 只对应一条消息；tool_result 展开在响应中无意义。
		if len(encoded) != 1 {
			return nil, paramErrf("choices[%d] 的消息无法编码为单条 OpenAI 消息", choice.Index)
		}
		item := map[string]any{
			"index":   choice.Index,
			"message": encoded[0],
		}
		if choice.FinishReason != "" {
			item["finish_reason"] = choice.FinishReason
		} else {
			item["finish_reason"] = nil
		}
		choices = append(choices, item)
	}
	obj["choices"] = choices

	raw, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("ir: marshal OpenAI response: %w", err)
	}
	out.Raw = raw
	return &out, nil
}
