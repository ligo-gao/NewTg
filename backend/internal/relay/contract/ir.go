package contract

// ============================================================================
// §2.6 IR 构件（v1.1 · 补遗 9）
//
// 背景：契约 v1.1 声称采用「双层 IR」，但只定义了顶层容器 Request/Response ——
// 它的构件（消息角色、内容部件、工具、选择项、原始协议体）**全部未定义**，
// 由 `go build` 检查出 8 处 undefined。本节补齐。
//
// 取值集的来源：契约 §2.2 的 `Message.Role` 与 `ContentPart.Kind` 注释里
// 已经列出了取值，本节把它们提升为**受控类型**，避免裸字符串散落。
// ============================================================================

// Role 是消息角色。取值来自契约 §2.2 的注释。
type Role string

const (
	RoleSystem    Role = "system"
	RoleDeveloper Role = "developer"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// PartKind 是内容部件类型。取值来自契约 §2.2 的注释。
type PartKind string

const (
	PartText       PartKind = "text"
	PartImageURL   PartKind = "image_url"
	PartImageB64   PartKind = "image_b64"
	PartAudio      PartKind = "audio"
	PartToolCall   PartKind = "tool_call"
	PartToolResult PartKind = "tool_result"
)

// Tool 是归一化后的工具定义。
type Tool struct {
	Name        string         // 工具名
	Description string         // 说明（用于上游提示）
	Schema      map[string]any // JSON Schema 形式的参数定义
}

// ToolChoiceMode 是工具选择模式。
type ToolChoiceMode string

const (
	ToolChoiceAuto     ToolChoiceMode = "auto"
	ToolChoiceNone     ToolChoiceMode = "none"
	ToolChoiceRequired ToolChoiceMode = "required"
	ToolChoiceSpecific ToolChoiceMode = "specific" // 指定单个工具，Name 必有值
)

// ToolChoice 是归一化后的工具选择。
// 不变量：Mode == ToolChoiceSpecific 时 Name 必须非空，否则视为参数错误（ErrParamInvalid）。
type ToolChoice struct {
	Mode ToolChoiceMode
	Name string // 仅 specific 时有值
}

// ResponseFormatKind 是响应格式种类。
type ResponseFormatKind string

const (
	ResponseFormatText       ResponseFormatKind = "text"
	ResponseFormatJSONObject ResponseFormatKind = "json_object"
	ResponseFormatJSONSchema ResponseFormatKind = "json_schema"
)

// ResponseFormat 是归一化后的响应格式约束。
type ResponseFormat struct {
	Kind   ResponseFormatKind
	Schema map[string]any // 仅 json_schema 时有值
}

// Choice 是响应中的一个候选项。
type Choice struct {
	Index        int
	Message      Message // 归一化消息（含工具调用）
	FinishReason string  // 归一化后的结束原因；上游原始值放 Message.Extensions
}

// ToolCallDelta 是流式工具调用增量。
//
// 流式的工具调用按 Index 分组、片段拼接：ID 与 Name 通常只出现在首片，
// ArgumentsFragment 逐片追加。拼接属执行层职责，适配器只负责切分。
type ToolCallDelta struct {
	Index             int
	ID                string
	Name              string
	ArgumentsFragment string
}

// ProtocolBody 是**原始协议体的不透明载体**。
//
// 双层 IR 的「原始层」由它承载：透传、未映射字段、对账与审计都从这里取。
// 不解析、不修改 —— 一旦解析就不叫「原始」了。
type ProtocolBody []byte

// RelayFormat 是协议标识（入站协议或目标上游协议）。
//
// 取值示例见契约 §3.1 的 Endpoint 枚举与 `02` 的分派表；
// 本类型只做**标识**，不承载行为。
type RelayFormat string
