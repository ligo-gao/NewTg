package ir

// Gemini generateContent ↔ IR 映射。
//
// 依据：
//   - 契约 `contract/types.go` §2.2/§2.3
//   - `docs/relay-02-协议适配器层-设计.md` §5 行 24（gemini 原生）
//   - `docs/relay-03-流式与用量提取-设计.md` §6.2 row 7（usageMetadata 字段路径）
//   - 计划 M4 R-04：parts 的 text/inlineData/functionCall/functionResponse
//     显式映射，未知类型返回错误（禁止静默丢弃）
//
// Gemini 与 OpenAI/Anthropic 的关键差异（都只在本包内处理）：
//   - 模型名在 URL 里，不在请求体里（解析时接受可选的 "model" 字段以便测试）；
//   - system 指令是顶层 systemInstruction，且只接受文本；
//   - 角色只有 user / model；工具回包以 user 角色的 functionResponse 承载；
//   - 流式用量是跨块累计的（StreamAggregatesUsage），本包提供解析器，
//     合并策略由适配器负责。

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"newtg/gateway/internal/contract"
)

// FormatGeminiGenerateContent 是本包写入 `Request.RawFormat` 的协议标识。
const FormatGeminiGenerateContent contract.RelayFormat = "gemini_generate_content"

// GeminiToIR 是跨任务冻结接口（ledger T2）的名字，转调 ParseGeminiRequest。
func GeminiToIR(body []byte) (*contract.Request, error) {
	return ParseGeminiRequest(body)
}

// IRToGemini 是跨任务冻结接口（ledger T2）的名字，转调 BuildGeminiRequestBody。
func IRToGemini(req *contract.Request) ([]byte, error) {
	return BuildGeminiRequestBody(req)
}

// ============================================================================
// 入站解析：Gemini JSON → IR
// ============================================================================

func ParseGeminiRequest(body []byte) (*contract.Request, error) {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" || trimmed[0] != '{' {
		return nil, paramErrf("Gemini 请求体必须是 JSON 对象")
	}
	var payload struct {
		Model             *string           `json:"model"` // 非协议标准；原生端点从 URL 取
		Contents          []json.RawMessage `json:"contents"`
		SystemInstruction json.RawMessage   `json:"systemInstruction"`
		Tools             []json.RawMessage `json:"tools"`
		ToolConfig        json.RawMessage   `json:"toolConfig"`
		GenerationConfig  json.RawMessage   `json:"generationConfig"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, paramErrf("invalid Gemini request JSON: %v", err)
	}

	req := &contract.Request{
		RawFormat: FormatGeminiGenerateContent,
		Raw:       append(contract.ProtocolBody(nil), body...),
	}
	if payload.Model != nil {
		req.Model = *payload.Model
	}

	system, err := parseGeminiSystemInstruction(payload.SystemInstruction)
	if err != nil {
		return nil, err
	}
	contents, err := parseGeminiContents(payload.Contents)
	if err != nil {
		return nil, err
	}
	if system != nil {
		req.Messages = append([]contract.Message{*system}, contents...)
	} else {
		req.Messages = contents
	}

	tools, err := parseGeminiTools(payload.Tools)
	if err != nil {
		return nil, err
	}
	req.Tools = tools
	if req.ToolChoice, err = parseGeminiToolConfig(payload.ToolConfig); err != nil {
		return nil, err
	}
	if err := parseGeminiGenerationConfig(payload.GenerationConfig, req); err != nil {
		return nil, err
	}
	return req, nil
}

func parseGeminiSystemInstruction(raw json.RawMessage) (*contract.Message, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	// 规范形态：{"parts":[{"text": "..."}]}；部分网关直接给字符串。
	var object struct {
		Parts []json.RawMessage `json:"parts"`
	}
	if err := json.Unmarshal(raw, &object); err != nil {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return nil, paramErrf("systemInstruction 必须是对象或字符串")
		}
		return &contract.Message{Role: contract.RoleSystem, Content: []contract.ContentPart{{
			Kind: contract.PartText, Text: text,
		}}}, nil
	}
	parts := make([]contract.ContentPart, 0, len(object.Parts))
	for i, rawPart := range object.Parts {
		part, err := parseGeminiPart(rawPart)
		if err != nil {
			return nil, fmt.Errorf("systemInstruction.parts[%d]: %w", i, err)
		}
		if part.Kind != contract.PartText {
			return nil, paramErrf("systemInstruction.parts[%d] 只允许 text，得到 %q", i, part.Kind)
		}
		parts = append(parts, part)
	}
	return &contract.Message{Role: contract.RoleSystem, Content: parts}, nil
}

func parseGeminiContents(raws []json.RawMessage) ([]contract.Message, error) {
	if len(raws) == 0 {
		return nil, nil
	}
	out := make([]contract.Message, 0, len(raws))
	for i, raw := range raws {
		var content struct {
			Role  string            `json:"role"`
			Parts []json.RawMessage `json:"parts"`
		}
		if err := json.Unmarshal(raw, &content); err != nil {
			return nil, fmt.Errorf("contents[%d]: %w", i, paramErrf("content 必须是 JSON 对象"))
		}
		var role contract.Role
		switch content.Role {
		case "", "user":
			// Gemini 规范允许省略 role，缺省即 user。
			role = contract.RoleUser
		case "model":
			role = contract.RoleAssistant
		case "function":
			// 少数客户端用 function 角色承载 functionResponse；归一为 tool。
			role = contract.RoleTool
		default:
			return nil, fmt.Errorf("contents[%d]: %w", i, paramErrf("不支持的角色 %q", content.Role))
		}
		parts := make([]contract.ContentPart, 0, len(content.Parts))
		for j, rawPart := range content.Parts {
			part, err := parseGeminiPart(rawPart)
			if err != nil {
				return nil, fmt.Errorf("contents[%d].parts[%d]: %w", i, j, err)
			}
			parts = append(parts, part)
		}
		out = append(out, contract.Message{Role: role, Content: parts})
	}
	return out, nil
}

// parseGeminiPart 显式映射 Gemini part。
//
// 允许的载荷键：text / inlineData / fileData / functionCall / functionResponse，
// 以及伴随的 thought、thoughtSignature。其他键（executableCode、
// codeExecutionResult、videoMetadata 之外的未建模类型）一律参数错误。
func parseGeminiPart(raw json.RawMessage) (contract.ContentPart, error) {
	var part map[string]any
	if err := json.Unmarshal(raw, &part); err != nil {
		return contract.ContentPart{}, paramErrf("part 必须是 JSON 对象")
	}
	payloadKeys := 0
	for _, key := range []string{"text", "inlineData", "fileData", "functionCall", "functionResponse"} {
		if _, ok := part[key]; ok {
			payloadKeys++
		}
	}
	if payloadKeys == 0 {
		return contract.ContentPart{}, paramErrf("不支持的 Gemini part（缺少可识别的载荷键）")
	}
	if payloadKeys > 1 {
		return contract.ContentPart{}, paramErrf("Gemini part 同时包含多个载荷键")
	}

	meta := map[string]any{}
	if thought, ok := part["thought"].(bool); ok && thought {
		meta["thought"] = true
	}
	if signature, ok := part["thoughtSignature"].(string); ok && signature != "" {
		meta["thought_signature"] = signature
	}
	if len(meta) == 0 {
		meta = nil
	}

	if text, ok := part["text"].(string); ok {
		return contract.ContentPart{Kind: contract.PartText, Text: text, Meta: meta}, nil
	}
	if inline, ok := part["inlineData"].(map[string]any); ok {
		mimeType, _ := inline["mimeType"].(string)
		data, _ := inline["data"].(string)
		payload, err := base64.StdEncoding.DecodeString(data)
		if err != nil {
			return contract.ContentPart{}, paramErrf("inlineData.data 不是合法 base64: %v", err)
		}
		return binaryPart(mimeType, payload, meta)
	}
	if file, ok := part["fileData"].(map[string]any); ok {
		mimeType, _ := file["mimeType"].(string)
		fileURI, _ := file["fileUri"].(string)
		if fileURI == "" {
			return contract.ContentPart{}, paramErrf("fileData.fileUri 为空")
		}
		if !strings.HasPrefix(mimeType, "image/") {
			return contract.ContentPart{}, paramErrf("fileData 仅支持图片 URI，得到 mimeType %q", mimeType)
		}
		if meta == nil {
			meta = map[string]any{}
		}
		meta["mime_type"] = mimeType
		return contract.ContentPart{Kind: contract.PartImageURL, URL: fileURI, Meta: meta}, nil
	}
	if call, ok := part["functionCall"].(map[string]any); ok {
		name, _ := call["name"].(string)
		if name == "" {
			return contract.ContentPart{}, paramErrf("functionCall 缺少 name")
		}
		args, ok := call["args"]
		if !ok || args == nil {
			args = map[string]any{}
		}
		encoded, err := json.Marshal(args)
		if err != nil {
			return contract.ContentPart{}, paramErrf("functionCall.args 无法编码: %v", err)
		}
		if meta == nil {
			meta = map[string]any{}
		}
		if id, ok := call["id"].(string); ok {
			meta["id"] = id
		}
		meta["name"] = name
		return contract.ContentPart{Kind: contract.PartToolCall, Text: string(encoded), Meta: meta}, nil
	}
	if response, ok := part["functionResponse"].(map[string]any); ok {
		name, _ := response["name"].(string)
		if name == "" {
			return contract.ContentPart{}, paramErrf("functionResponse 缺少 name")
		}
		responseValue := response["response"]
		if responseValue == nil {
			responseValue = map[string]any{}
		}
		encoded, err := json.Marshal(responseValue)
		if err != nil {
			return contract.ContentPart{}, paramErrf("functionResponse.response 无法编码: %v", err)
		}
		if meta == nil {
			meta = map[string]any{}
		}
		meta["name"] = name
		// Gemini 没有调用 ID；若新版 API 带 id 则保留，否则留空。
		if id, ok := response["id"].(string); ok {
			meta["tool_call_id"] = id
		}
		return contract.ContentPart{Kind: contract.PartToolResult, Text: string(encoded), Meta: meta}, nil
	}
	return contract.ContentPart{}, paramErrf("不支持的 Gemini part 载荷")
}

// isKnownGeminiPart 判断 part 是否含已建模的载荷键。
// 响应解析用它区分「已知 part 的字段错误」（拒绝）与「未来新增 part 类型」
// （保留到 Extensions），与 Anthropic 的 isKnownAnthropicBlockType 同策略。
func isKnownGeminiPart(part map[string]any) bool {
	for _, key := range []string{"text", "inlineData", "fileData", "functionCall", "functionResponse"} {
		if _, ok := part[key]; ok {
			return true
		}
	}
	return false
}

// binaryPart 按 mimeType 把二进制载荷归一到图片或音频部件。
func binaryPart(mimeType string, payload []byte, meta map[string]any) (contract.ContentPart, error) {
	if meta == nil {
		meta = map[string]any{}
	}
	if mimeType != "" {
		meta["mime_type"] = mimeType
	}
	switch {
	case strings.HasPrefix(mimeType, "image/"):
		return contract.ContentPart{Kind: contract.PartImageB64, Payload: payload, Meta: meta}, nil
	case strings.HasPrefix(mimeType, "audio/"):
		return contract.ContentPart{Kind: contract.PartAudio, Payload: payload, Meta: meta}, nil
	default:
		// 契约的二进制载体只有图片/音频；PDF 等类型没有承载位，显式拒绝。
		return contract.ContentPart{}, paramErrf("inlineData 的 mimeType %q 不受支持", mimeType)
	}
}

func parseGeminiTools(raws []json.RawMessage) ([]contract.Tool, error) {
	if len(raws) == 0 {
		return nil, nil
	}
	var tools []contract.Tool
	for i, raw := range raws {
		// L6：逐组检查所有键。内建工具（googleSearch / codeExecution 等）
		// 与 functionDeclarations 混在同一组时，只取后者会静默改变语义；
		// 任一未知组键都显式拒绝，而不是悄悄丢弃。
		var group map[string]json.RawMessage
		if err := json.Unmarshal(raw, &group); err != nil {
			return nil, paramErrf("tools[%d] 不是合法 JSON 对象", i)
		}
		for key := range group {
			if key != "functionDeclarations" {
				return nil, paramErrf("tools[%d] 含不支持的内建工具 %q", i, key)
			}
		}
		declarationsRaw, ok := group["functionDeclarations"]
		if !ok {
			return nil, paramErrf("tools[%d] 没有 functionDeclarations（内建工具暂不支持）", i)
		}
		var declarations []struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			Parameters  map[string]any `json:"parameters"`
		}
		if err := json.Unmarshal(declarationsRaw, &declarations); err != nil {
			return nil, paramErrf("tools[%d].functionDeclarations 形状非法", i)
		}
		if len(declarations) == 0 {
			return nil, paramErrf("tools[%d] 没有 functionDeclarations", i)
		}
		for j, declaration := range declarations {
			if declaration.Name == "" {
				return nil, paramErrf("tools[%d].functionDeclarations[%d] 缺少 name", i, j)
			}
			tools = append(tools, contract.Tool{
				Name:        declaration.Name,
				Description: declaration.Description,
				Schema:      declaration.Parameters,
			})
		}
	}
	return tools, nil
}

func parseGeminiToolConfig(raw json.RawMessage) (contract.ToolChoice, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return contract.ToolChoice{}, nil
	}
	var config struct {
		FunctionCallingConfig struct {
			Mode                 string   `json:"mode"`
			AllowedFunctionNames []string `json:"allowedFunctionNames"`
		} `json:"functionCallingConfig"`
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		return contract.ToolChoice{}, paramErrf("toolConfig 形状非法")
	}
	mode := config.FunctionCallingConfig.Mode
	switch mode {
	case "", "AUTO":
		return contract.ToolChoice{Mode: contract.ToolChoiceAuto}, nil
	case "NONE":
		return contract.ToolChoice{Mode: contract.ToolChoiceNone}, nil
	case "ANY":
		names := config.FunctionCallingConfig.AllowedFunctionNames
		if len(names) == 1 {
			// ANY + 唯一允许项与 IR 的 specific 等价，保留更强语义。
			return contract.ToolChoice{Mode: contract.ToolChoiceSpecific, Name: names[0]}, nil
		}
		return contract.ToolChoice{Mode: contract.ToolChoiceRequired}, nil
	default:
		return contract.ToolChoice{}, paramErrf("toolConfig.functionCallingConfig.mode %q 不受支持", mode)
	}
}

func parseGeminiGenerationConfig(raw json.RawMessage, req *contract.Request) error {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	var config struct {
		MaxOutputTokens  *int           `json:"maxOutputTokens"`
		Temperature      *float64       `json:"temperature"`
		TopP             *float64       `json:"topP"`
		StopSequences    []string       `json:"stopSequences"`
		Seed             *int           `json:"seed"`
		ResponseMimeType string         `json:"responseMimeType"`
		ResponseSchema   map[string]any `json:"responseSchema"`
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		return paramErrf("generationConfig 形状非法")
	}
	req.MaxTokens = config.MaxOutputTokens
	req.Temperature = config.Temperature
	req.TopP = config.TopP
	req.Stop = config.StopSequences
	req.Seed = config.Seed
	if err := validateScalars(req.MaxTokens, req.Temperature, req.TopP); err != nil {
		return err
	}
	switch config.ResponseMimeType {
	case "":
		// 未设置响应格式。
	case "text/plain":
		req.ResponseFormat = &contract.ResponseFormat{Kind: contract.ResponseFormatText}
	case "application/json":
		if config.ResponseSchema != nil {
			req.ResponseFormat = &contract.ResponseFormat{
				Kind:   contract.ResponseFormatJSONSchema,
				Schema: config.ResponseSchema,
			}
		} else {
			req.ResponseFormat = &contract.ResponseFormat{Kind: contract.ResponseFormatJSONObject}
		}
	default:
		return paramErrf("generationConfig.responseMimeType %q 不受支持", config.ResponseMimeType)
	}
	return nil
}

// ============================================================================
// 出站构造：IR → Gemini JSON
// ============================================================================

// BuildGeminiRequestBody 把归一化 IR 编码为 Gemini generateContent 请求体。
// 模型名与流式开关都不在请求体里：前者进 URL，后者由 URL 后缀决定。
func BuildGeminiRequestBody(req *contract.Request) ([]byte, error) {
	if req == nil {
		return nil, paramErrf("nil request")
	}
	// 适配器本地再校验标量（relay-02 §4.1）：IR 可能来自跨协议调用方。
	if err := validateScalars(req.MaxTokens, req.Temperature, req.TopP); err != nil {
		return nil, err
	}
	obj := map[string]any{}
	if len(req.Raw) > 0 && isGeminiRawFormat(req.RawFormat) {
		var passthrough map[string]any
		if err := json.Unmarshal(req.Raw, &passthrough); err != nil {
			return nil, paramErrf("Raw 不是合法 JSON 对象: %v", err)
		}
		if passthrough != nil {
			obj = passthrough
		}
	}

	system, contents, err := encodeGeminiContents(req.Messages)
	if err != nil {
		return nil, err
	}
	// 旧请求体里的 model/stream 没有协议意义（原生端点从 URL 取），
	// 若来自同协议透传应删除，避免把非标准字段发给上游。
	delete(obj, "model")
	delete(obj, "stream")
	if system != nil {
		obj["systemInstruction"] = system
	} else {
		delete(obj, "systemInstruction")
	}
	obj["contents"] = contents

	// L6：原始 functionDeclarations 必须在覆盖前捕获。contract.Tool 只有
	// name/description/parameters，response、behavior 等声明级字段只能靠
	// 原始子对象合并保留。
	rawTools, _ := obj["tools"].([]any)
	if len(req.Tools) > 0 {
		declarations, err := encodeGeminiTools(req.Tools, rawTools)
		if err != nil {
			return nil, err
		}
		obj["tools"] = []any{map[string]any{"functionDeclarations": declarations}}
	} else {
		delete(obj, "tools")
	}

	if req.ToolChoice.Mode != "" {
		config, err := encodeGeminiToolConfig(req.ToolChoice)
		if err != nil {
			return nil, err
		}
		obj["toolConfig"] = config
	} else {
		delete(obj, "toolConfig")
	}

	// generationConfig 必须从原始子对象起步再覆盖已建模字段（复审 P1）：
	// topK / candidateCount / presencePenalty / thinkingConfig /
	// responseModalities 等未建模字段必须原样存活，不能因为 IR 没有对应
	// 字段就被整体丢弃。
	config := map[string]any{}
	if rawConfig, ok := obj["generationConfig"].(map[string]any); ok {
		config = cloneMap(rawConfig)
	}
	if req.MaxTokens != nil {
		config["maxOutputTokens"] = *req.MaxTokens
	}
	if req.Temperature != nil {
		config["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		config["topP"] = *req.TopP
	}
	if len(req.Stop) > 0 {
		config["stopSequences"] = req.Stop
	}
	if req.Seed != nil {
		config["seed"] = *req.Seed
	}
	if req.ResponseFormat != nil {
		switch req.ResponseFormat.Kind {
		case contract.ResponseFormatText:
			config["responseMimeType"] = "text/plain"
			delete(config, "responseSchema")
		case contract.ResponseFormatJSONObject:
			config["responseMimeType"] = "application/json"
			delete(config, "responseSchema")
		case contract.ResponseFormatJSONSchema:
			config["responseMimeType"] = "application/json"
			if req.ResponseFormat.Schema != nil {
				config["responseSchema"] = req.ResponseFormat.Schema
			}
		default:
			return nil, paramErrf("response_format 类型 %q 不受支持", req.ResponseFormat.Kind)
		}
	}
	if len(config) > 0 {
		obj["generationConfig"] = config
	} else {
		delete(obj, "generationConfig")
	}
	return json.Marshal(obj)
}

// encodeGeminiContents 把 IR 消息拆成 systemInstruction 与 contents。
// Gemini 的 contents 必须是 user/model 交替（工具回包为 user 角色），
// 因此连续同角色消息会合并，工具回包并入相邻的 user 消息。
func encodeGeminiContents(messages []contract.Message) (map[string]any, []any, error) {
	var systemParts []any
	contents := make([]any, 0, len(messages))
	// Gemini 的 functionResponse 只认工具名，不认调用 ID；而 OpenAI/Anthropic
	// 的 tool_result 只带 tool_call_id。先从整条消息序列建立 ID → 名称索引，
	// 才能把同一个工具回包无损映射过去。
	toolCallNames := collectToolCallNames(messages)

	appendContent := func(role string, parts []any) {
		if len(contents) > 0 {
			if last, ok := contents[len(contents)-1].(map[string]any); ok {
				if lastRole, _ := last["role"].(string); lastRole == role {
					if existing, ok := last["parts"].([]any); ok {
						last["parts"] = append(existing, parts...)
						return
					}
				}
			}
		}
		contents = append(contents, map[string]any{"role": role, "parts": parts})
	}

	for i, message := range messages {
		switch message.Role {
		case contract.RoleSystem, contract.RoleDeveloper:
			for j, part := range message.Content {
				if part.Kind != contract.PartText {
					return nil, nil, paramErrf("messages[%d].content[%d]: system 消息只允许 text 部件", i, j)
				}
				systemParts = append(systemParts, map[string]any{"text": part.Text})
			}
		case contract.RoleUser:
			parts, err := encodeGeminiParts(message.Content, toolCallNames)
			if err != nil {
				return nil, nil, fmt.Errorf("messages[%d]: %w", i, err)
			}
			appendContent("user", parts)
		case contract.RoleAssistant:
			parts, err := encodeGeminiParts(message.Content, toolCallNames)
			if err != nil {
				return nil, nil, fmt.Errorf("messages[%d]: %w", i, err)
			}
			appendContent("model", parts)
		case contract.RoleTool:
			parts, err := encodeGeminiParts(message.Content, toolCallNames)
			if err != nil {
				return nil, nil, fmt.Errorf("messages[%d]: %w", i, err)
			}
			appendContent("user", parts)
		default:
			return nil, nil, paramErrf("messages[%d]: 不支持的消息角色 %q", i, message.Role)
		}
	}
	var system map[string]any
	if len(systemParts) > 0 {
		system = map[string]any{"parts": systemParts}
	}
	return system, contents, nil
}

// encodeGeminiTools 编码 functionDeclarations；原始声明按名称合并，
// 保留 response/behavior 等未建模字段。
func encodeGeminiTools(tools []contract.Tool, rawTools []any) ([]any, error) {
	rawDeclarations := flattenGeminiDeclarations(rawTools)
	out := make([]any, 0, len(tools))
	for i, tool := range tools {
		if tool.Name == "" {
			return nil, paramErrf("tools[%d] 缺少 name", i)
		}
		declaration := map[string]any{}
		if raw := rawDeclarations[tool.Name]; raw != nil {
			declaration = cloneMap(raw)
		}
		declaration["name"] = tool.Name
		if tool.Description != "" {
			declaration["description"] = tool.Description
		}
		if tool.Schema != nil {
			declaration["parameters"] = tool.Schema
		}
		out = append(out, declaration)
	}
	return out, nil
}

// flattenGeminiDeclarations 建立工具名 → 原始声明索引（跨 tool 分组）。
func flattenGeminiDeclarations(rawTools []any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, rawGroup := range rawTools {
		group, ok := rawGroup.(map[string]any)
		if !ok {
			continue
		}
		declarations, ok := group["functionDeclarations"].([]any)
		if !ok {
			continue
		}
		for _, rawDeclaration := range declarations {
			declaration, ok := rawDeclaration.(map[string]any)
			if !ok {
				continue
			}
			name, _ := declaration["name"].(string)
			if name != "" {
				out[name] = declaration
			}
		}
	}
	return out
}

// encodeGeminiParts 把 IR 部件转为 Gemini parts。
func encodeGeminiParts(parts []contract.ContentPart, toolCallNames map[string]string) ([]any, error) {
	out := make([]any, 0, len(parts))
	for i, part := range parts {
		encoded, err := encodeGeminiPart(part, toolCallNames)
		if err != nil {
			return nil, fmt.Errorf("content[%d]: %w", i, err)
		}
		out = append(out, encoded)
	}
	return out, nil
}

// collectToolCallNames 建立 tool_call ID → 工具名索引（跨整条消息序列）。
func collectToolCallNames(messages []contract.Message) map[string]string {
	names := map[string]string{}
	for _, message := range messages {
		for _, part := range message.Content {
			if part.Kind != contract.PartToolCall {
				continue
			}
			id := metaString(part.Meta, "id")
			name := metaString(part.Meta, "name")
			if id != "" && name != "" {
				names[id] = name
			}
		}
	}
	return names
}

func encodeGeminiPart(part contract.ContentPart, toolCallNames map[string]string) (map[string]any, error) {
	switch part.Kind {
	case contract.PartText:
		item := map[string]any{"text": part.Text}
		if thought, _ := part.Meta["thought"].(bool); thought {
			item["thought"] = true
		}
		if signature := metaString(part.Meta, "thought_signature"); signature != "" {
			item["thoughtSignature"] = signature
		}
		return item, nil
	case contract.PartImageB64:
		if len(part.Payload) == 0 {
			return nil, paramErrf("image_b64 部件缺少负载")
		}
		mimeType := metaString(part.Meta, "mime_type")
		if mimeType == "" {
			mimeType = "image/png"
		}
		return map[string]any{
			"inlineData": map[string]any{
				"mimeType": mimeType,
				"data":     base64.StdEncoding.EncodeToString(part.Payload),
			},
		}, nil
	case contract.PartImageURL:
		if part.URL == "" {
			return nil, paramErrf("image_url 部件缺少 URL")
		}
		mimeType := metaString(part.Meta, "mime_type")
		if mimeType == "" {
			mimeType = inferImageMimeType(part.URL)
		}
		return map[string]any{
			"fileData": map[string]any{
				"mimeType": mimeType,
				"fileUri":  part.URL,
			},
		}, nil
	case contract.PartAudio:
		if len(part.Payload) == 0 {
			return nil, paramErrf("audio 部件缺少负载")
		}
		mimeType := metaString(part.Meta, "mime_type")
		if mimeType == "" {
			mimeType = "audio/wav"
		}
		return map[string]any{
			"inlineData": map[string]any{
				"mimeType": mimeType,
				"data":     base64.StdEncoding.EncodeToString(part.Payload),
			},
		}, nil
	case contract.PartToolCall:
		name := metaString(part.Meta, "name")
		if name == "" {
			return nil, paramErrf("tool_call 缺少 function.name")
		}
		args, err := encodeGeminiArgs(part.Text)
		if err != nil {
			return nil, err
		}
		call := map[string]any{"name": name, "args": args}
		if id := metaString(part.Meta, "id"); id != "" {
			call["id"] = id
		}
		item := map[string]any{"functionCall": call}
		// thoughtSignature 是 part 级兄弟字段，必须随 functionCall 回写（复审 P2）。
		copyThoughtSignature(item, part.Meta)
		return item, nil
	case contract.PartToolResult:
		name := metaString(part.Meta, "name")
		if name == "" {
			// 回落到调用 ID 索引：OpenAI/Anthropic 的 tool_result 只带 ID。
			name = toolCallNames[metaString(part.Meta, "tool_call_id")]
		}
		if name == "" {
			return nil, paramErrf("tool_result 缺少工具名（Gemini functionResponse 必需）")
		}
		response, err := encodeGeminiToolResponse(part.Text)
		if err != nil {
			return nil, err
		}
		inner := map[string]any{"name": name, "response": response}
		if id := metaString(part.Meta, "tool_call_id"); id != "" {
			inner["id"] = id
		}
		item := map[string]any{"functionResponse": inner}
		copyThoughtSignature(item, part.Meta)
		return item, nil
	default:
		return nil, paramErrf("不支持的部件类型 %q", part.Kind)
	}
}

// copyThoughtSignature 把 Meta 中保存的 thoughtSignature 写回 part 层。
func copyThoughtSignature(item map[string]any, meta map[string]any) {
	if signature := metaString(meta, "thought_signature"); signature != "" {
		item["thoughtSignature"] = signature
	}
}

// encodeGeminiArgs 把 IR 的 JSON 字符串参数还原为对象。
func encodeGeminiArgs(text string) (map[string]any, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return map[string]any{}, nil
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(trimmed), &args); err != nil {
		return nil, paramErrf("tool_call 参数必须是 JSON 对象: %v", err)
	}
	if args == nil {
		return map[string]any{}, nil
	}
	return args, nil
}

// encodeGeminiToolResponse 把工具结果文本还原为 Gemini 要求的对象形态。
// 文本本身是 JSON 对象时原样使用；否则包成 {"result": "..."}，
// 因为 Gemini 的 functionResponse.response 必须是对象。
func encodeGeminiToolResponse(text string) (any, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return map[string]any{}, nil
	}
	var value any
	if err := json.Unmarshal([]byte(trimmed), &value); err == nil {
		if object, ok := value.(map[string]any); ok {
			return object, nil
		}
	}
	return map[string]any{"result": text}, nil
}

func encodeGeminiToolConfig(choice contract.ToolChoice) (map[string]any, error) {
	config := map[string]any{}
	switch choice.Mode {
	case contract.ToolChoiceAuto:
		config["mode"] = "AUTO"
	case contract.ToolChoiceNone:
		config["mode"] = "NONE"
	case contract.ToolChoiceRequired:
		config["mode"] = "ANY"
	case contract.ToolChoiceSpecific:
		if choice.Name == "" {
			return nil, paramErrf("tool_choice specific 缺少 name")
		}
		config["mode"] = "ANY"
		config["allowedFunctionNames"] = []string{choice.Name}
	default:
		return nil, paramErrf("tool_choice 模式 %q 不受支持", choice.Mode)
	}
	return map[string]any{"functionCallingConfig": config}, nil
}

func isGeminiRawFormat(format contract.RelayFormat) bool {
	switch strings.ToLower(string(format)) {
	case "gemini", "gemini_generate_content", "gemini_native":
		return true
	default:
		return false
	}
}

// inferImageMimeType 从 URL 扩展名推断图片 MIME。
// 推断失败时用 image/png：Gemini 的 fileData.mimeType 是必填字段，
// 而 IR 的 image_url 没有携带内容类型；选一个最常见的图片类型，
// 具体类型可由上游按内容嗅探（错误时由上游返回参数错误）。
func inferImageMimeType(rawURL string) string {
	lower := strings.ToLower(rawURL)
	if index := strings.IndexAny(lower, "?#"); index >= 0 {
		lower = lower[:index]
	}
	switch {
	case strings.HasSuffix(lower, ".png"):
		return "image/png"
	case strings.HasSuffix(lower, ".jpg"), strings.HasSuffix(lower, ".jpeg"):
		return "image/jpeg"
	case strings.HasSuffix(lower, ".gif"):
		return "image/gif"
	case strings.HasSuffix(lower, ".webp"):
		return "image/webp"
	default:
		return "image/png"
	}
}

// ============================================================================
// 上游响应解析：Gemini JSON → contract.Response/Usage
// ============================================================================

func ParseGeminiResponse(body []byte) (*contract.Response, contract.Usage, error) {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" || trimmed[0] != '{' {
		return nil, contract.Usage{}, paramErrf("Gemini 响应体必须是 JSON 对象")
	}
	var payload struct {
		ResponseID   string `json:"responseId"`
		ModelVersion string `json:"modelVersion"`
		Candidates   []struct {
			Index        int             `json:"index"`
			Content      json.RawMessage `json:"content"`
			FinishReason string          `json:"finishReason"`
		} `json:"candidates"`
		PromptFeedback *struct {
			BlockReason string `json:"blockReason"`
		} `json:"promptFeedback"`
		UsageMetadata json.RawMessage `json:"usageMetadata"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, contract.Usage{}, paramErrf("invalid Gemini response JSON: %v", err)
	}

	response := &contract.Response{
		ID:    payload.ResponseID,
		Model: payload.ModelVersion,
		Raw:   append(contract.ProtocolBody(nil), body...),
	}
	for _, candidate := range payload.Candidates {
		message := contract.Message{Role: contract.RoleAssistant}
		var unmapped []any
		unparseable := 0
		if len(candidate.Content) > 0 {
			var content struct {
				Parts []json.RawMessage `json:"parts"`
			}
			if err := json.Unmarshal(candidate.Content, &content); err != nil {
				return nil, contract.Usage{}, paramErrf("candidates[%d].content 形状非法", candidate.Index)
			}
			for j, rawPart := range content.Parts {
				part, err := parseGeminiPart(rawPart)
				if err != nil {
					// H3：与 Anthropic 响应策略对齐——已建模 part 的字段错误
					// 仍拒绝；真正未知的 part 类型保留到 Extensions，不能让
					// 一个有效 200 响应变成 502。
					var probe map[string]any
					_ = json.Unmarshal(rawPart, &probe)
					if isKnownGeminiPart(probe) {
						return nil, contract.Usage{}, fmt.Errorf("candidates[%d].parts[%d]: %w", candidate.Index, j, err)
					}
					var opaque any
					if json.Unmarshal(rawPart, &opaque) == nil {
						unmapped = append(unmapped, opaque)
					} else {
						// 诊断标记：极端情况下连原样保留都做不到时，至少让
						// 审计侧看到存在被跳过的 part，而不是完全静默。
						unparseable++
					}
					continue
				}
				message.Content = append(message.Content, part)
			}
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
		finishReason := normalizeGeminiFinishReason(candidate.FinishReason)
		if candidate.FinishReason != "" {
			if message.Extensions == nil {
				message.Extensions = map[string]any{}
			}
			message.Extensions["finish_reason"] = candidate.FinishReason
		}
		response.Choices = append(response.Choices, contract.Choice{
			Index:        candidate.Index,
			Message:      message,
			FinishReason: finishReason,
		})
	}
	if len(response.Choices) > 0 {
		response.FinishReason = response.Choices[0].FinishReason
	} else if payload.PromptFeedback != nil && payload.PromptFeedback.BlockReason != "" {
		// 无候选且有拦截原因：显式归一为内容审核拦截，避免客户端看到空成功。
		response.FinishReason = "content_filter"
	}
	return response, ParseGeminiUsage(payload.UsageMetadata), nil
}

// ParseGeminiUsage 归一 usageMetadata。
//
// 字段路径（relay-03 §6.2 row 7）：
//   - Prompt = promptTokenCount + toolUsePromptTokenCount
//   - Completion = candidatesTokenCount + thoughtsTokenCount
//   - Total = totalTokenCount（缺失时派生）
//   - CacheRead = cachedContentTokenCount；Reasoning = thoughtsTokenCount
//   - PromptIncludesCache=false（缓存是独立维度），
//     ReasoningIncludedInCompletion=true（thinking 属于 completion 子集）
//
// Usage.Raw 保留 Gemini 原键（cachedContentTokenCount 等），供 M3 的
// usage_semantic 正向判定与对账；**不得伪装成 Claude 的 input_tokens 形状**。
func ParseGeminiUsage(raw json.RawMessage) contract.Usage {
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
		PromptTokenCount        int64 `json:"promptTokenCount"`
		ToolUsePromptTokenCount int64 `json:"toolUsePromptTokenCount"`
		CandidatesTokenCount    int64 `json:"candidatesTokenCount"`
		TotalTokenCount         int64 `json:"totalTokenCount"`
		ThoughtsTokenCount      int64 `json:"thoughtsTokenCount"`
		CachedContentTokenCount int64 `json:"cachedContentTokenCount"`
		PromptTokensDetails     []struct {
			Modality   string `json:"modality"`
			TokenCount int64  `json:"tokenCount"`
		} `json:"promptTokensDetails"`
		CandidatesTokensDetails []struct {
			Modality   string `json:"modality"`
			TokenCount int64  `json:"tokenCount"`
		} `json:"candidatesTokensDetails"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return base
	}
	var rawMap map[string]any
	if err := json.Unmarshal(raw, &rawMap); err != nil || len(rawMap) == 0 {
		return base
	}
	// M5：非空但没有任何 Gemini 用量键的对象不是上游事实，不能标记
	// upstream_exact（否则跨协议层会合成全零 usage）。
	if !hasAnyMapKey(rawMap, "promptTokenCount", "toolUsePromptTokenCount",
		"candidatesTokenCount", "totalTokenCount", "thoughtsTokenCount",
		"cachedContentTokenCount", "promptTokensDetails", "candidatesTokensDetails",
		"cacheTokensDetails") {
		return base
	}

	prompt := nonNeg(payload.PromptTokenCount) + nonNeg(payload.ToolUsePromptTokenCount)
	completion := nonNeg(payload.CandidatesTokenCount) + nonNeg(payload.ThoughtsTokenCount)
	var promptImage, promptAudio, completionAudio int64
	for _, detail := range payload.PromptTokensDetails {
		switch strings.ToUpper(detail.Modality) {
		case "IMAGE":
			promptImage += nonNeg(detail.TokenCount)
		case "AUDIO":
			promptAudio += nonNeg(detail.TokenCount)
		}
	}
	for _, detail := range payload.CandidatesTokensDetails {
		if strings.ToUpper(detail.Modality) == "AUDIO" {
			completionAudio += nonNeg(detail.TokenCount)
		}
	}

	usage := contract.Usage{
		Prompt:                        prompt,
		Completion:                    completion,
		Total:                         nonNeg(payload.TotalTokenCount),
		CacheRead:                     nonNeg(payload.CachedContentTokenCount),
		Reasoning:                     nonNeg(payload.ThoughtsTokenCount),
		PromptIncludesCache:           false,
		ReasoningIncludedInCompletion: true,
		Source:                        contract.UsageUpstreamExact,
		Raw:                           rawMap,
	}
	if usage.Total == 0 && usage.Prompt+usage.Completion > 0 {
		usage.Total = usage.Prompt + usage.Completion
	}
	usage.PromptDetail = contract.UsageDetail{
		Image:     promptImage,
		Audio:     promptAudio,
		Text:      nonNeg(prompt - promptImage - promptAudio),
		CacheRead: usage.CacheRead,
	}
	usage.CompletionDetail = contract.UsageDetail{
		Audio: completionAudio,
		// Reasoning 是 Completion 的子集，不能从 Text 里再扣一次。
		Text: nonNeg(completion - completionAudio),
	}
	return usage
}

// normalizeGeminiFinishReason 把 finishReason 映射为 IR 归一值。
func normalizeGeminiFinishReason(reason string) string {
	switch reason {
	case "STOP":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "IMAGE_SAFETY":
		return "content_filter"
	case "":
		return ""
	default:
		return strings.ToLower(reason)
	}
}
