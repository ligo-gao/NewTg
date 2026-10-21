package mj

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// 上游提交客户端（W8/MJ-a2）。
//
// 本文件只负责「把一次已归一化的提交变成一个 midjourney-proxy 请求，并把
// 它的 JSON 响应结构化解析」。渠道选择、预扣、落库与退款顺序在 HTTP 层
// 编排；这里不碰 DB/计费，便于用 httptest 假上游穷举线协议分支。
//
// 【为什么不复用插件提交器】插件提交走 JS 钩子 + 动态描述符；MJ 上游是固定
// HTTP 形状（`{base}/mj/submit/<kind>`、`mj-api-secret` 头），且提交不重试
// （relay-05 §6.2）。把 MJ 强塞进插件宿主既表达不了固定形状，也会让插件
// 的 30s 超时/白名单语义污染 MJ 的 60s 单次尝试。

const (
	// SubmitTimeout 是单次提交/换脸的上游时限（relay-05 §6.1 冻结 60s）。
	SubmitTimeout = 60 * time.Second

	// MaxSubmitResponseBytes 是上游 JSON 响应上限（与插件出站同口径 4MiB）。
	// 超限必须报错而不是截断：截断后的 JSON 可能被解析成「成功但无 result」。
	MaxSubmitResponseBytes = 4 << 20

	// upstreamSecretHeader 是上游 proxy 的渠道密钥载体（基线-07 §0）。
	// 客户端自己的 `mj-api-secret` 令牌载体只用于入站鉴权，**绝不**透传；
	// 出站头必须覆盖为渠道凭据。
	upstreamSecretHeader = "mj-api-secret"
)

// SubmitResponse 是一次上游提交的结构化解析结果。
//
// Code 保留上游原始码（不被重编码）：调用方需要按 21/22/23/24/3/30 决定
// 落库状态、复用、退款或 429；对客户端可见的码由 ClientCode 再折叠。
type SubmitResponse struct {
	// HTTPStatus 是上游 HTTP 状态码。midjourney-proxy 在部分错误上会返回
	// 非 200 + 内嵌 code，两者都要保留，不能只看其中一个。
	HTTPStatus int
	Code       int
	// Description 是上游描述；调用方转发成功描述时必须走固定/净化后的
	// 路径，不能把上游内部细节原样透给客户端。
	Description string
	// Result 是单值 MjId（上传端点之外的所有提交）。
	Result string
	// Results 是 upload-discord-images 的数组形结果。
	Results []string
	// Properties 是上游受理体的 properties（队列数/实例 id 等审计快照）。
	Properties map[string]any
	// Status / Progress 是上游在受理体里附带的状态快照（code 21 可能已出结果）。
	Status   string
	Progress string
}

// SubmitConfig 是目标渠道的出站参数。APIKey 是**渠道凭据明文**，只在本次
// 请求内存中存在，不得进入日志、任务行或响应。
type SubmitConfig struct {
	BaseURL string
	APIKey  string
	// HTTP 为空时使用客户端构造时的默认客户端。
	HTTP *http.Client
}

// SubmitClient 是上游提交执行器，可并发使用。
type SubmitClient struct {
	client *http.Client
}

// NewSubmitClient 构造提交客户端。
//
// 默认客户端禁止重定向：渠道 base URL 是管理员配置的目标，重定向会让实际
// 出站目标逃逸出该配置（凭据随请求头一起被送往重定向目标）。测试可注入
// 自己的 *http.Client，但仍应在假上游里断言没有二次请求。
func NewSubmitClient(client *http.Client) *SubmitClient {
	if client == nil {
		client = &http.Client{
			Timeout: SubmitTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	return &SubmitClient{client: client}
}

// Submit 把一次归一化提交发送到 `{base}/mj/<endpoint>`。
//
// 返回两类失败：
//   - 传输/编码/解析失败 → *Error（502 upstream_error / bad_response）；
//   - 上游可解析的失败码 → (response, nil)，由调用方按码分类（退款/429）。
//
// 不重试（relay-05 §6.2）：重复提交会重复建任务与重复计费。调用方不得在
// 本函数外再包一层重试。
func (c *SubmitClient) Submit(ctx context.Context, cfg SubmitConfig, endpoint Endpoint, normalized *NormalizedSubmit) (*SubmitResponse, *Error) {
	if normalized == nil {
		return nil, BadRequest(DescriptionInvalidRequest)
	}
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		return nil, &Error{Code: CodeUnknownError, Description: DescriptionChannelMisconfigured, Status: http.StatusInternalServerError}
	}
	body, err := buildSubmitBody(endpoint, normalized)
	if err != nil {
		return nil, &Error{Code: CodeUnknownError, Description: DescriptionInternalError, Status: http.StatusInternalServerError}
	}

	client := cfg.HTTP
	if client == nil {
		client = c.client
	}
	// 双保险：即使注入的客户端没有 Timeout，也按冻结值约束本次提交。
	reqCtx, cancel := context.WithTimeout(ctx, SubmitTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, base+"/mj/"+string(endpoint), bytes.NewReader(body))
	if err != nil {
		return nil, &Error{Code: CodeUnknownError, Description: DescriptionUpstreamError, Status: http.StatusBadGateway}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	// 渠道凭据覆盖客户端载体：客户端可能用 mj-api-secret 作为**网关**令牌，
	// 那个值绝不能转发给上游（会泄露用户令牌，也会覆盖渠道认证）。
	req.Header.Set(upstreamSecretHeader, cfg.APIKey)

	resp, err := client.Do(req)
	if err != nil {
		return nil, &Error{Code: CodeUnknownError, Description: DescriptionUpstreamError, Status: http.StatusBadGateway}
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxSubmitResponseBytes+1))
	if err != nil {
		return nil, &Error{Code: CodeUnknownError, Description: DescriptionUpstreamError, Status: http.StatusBadGateway}
	}
	if len(raw) > MaxSubmitResponseBytes {
		return nil, &Error{Code: CodeUnknownError, Description: DescriptionUpstreamResponseTooLarge, Status: http.StatusBadGateway}
	}
	// 重定向不跟随（CheckRedirect 返回 ErrUseLastResponse），因此 3xx 是
	// 失败而不是「上游正常响应」；不把 Location 头/目标暴露给上层。
	if resp.StatusCode >= http.StatusMultipleChoices && resp.StatusCode < http.StatusBadRequest {
		return nil, &Error{Code: CodeUnknownError, Description: DescriptionUpstreamError, Status: http.StatusBadGateway}
	}
	parsed, parseErr := parseSubmitResponse(raw)
	if parseErr != nil {
		if resp.StatusCode >= http.StatusBadRequest {
			// 上游错误页不是 JSON：不向上层透出任何正文（可能含内部地址）。
			return nil, &Error{Code: CodeUnknownError, Description: DescriptionUpstreamError, Status: http.StatusBadGateway}
		}
		return nil, &Error{Code: CodeUnknownError, Description: DescriptionUpstreamBadResponse, Status: http.StatusBadGateway}
	}
	parsed.HTTPStatus = resp.StatusCode
	// HTTP 失败状态不能被内嵌的「成功 code」洗白：500 + {"code":1} 通常是
	// 代理错误页/残缺响应，按失败处理并从上层退款，绝不落一条无法轮询的行。
	if resp.StatusCode >= http.StatusBadRequest && CodeAccepted(parsed.Code) {
		return nil, &Error{Code: CodeUnknownError, Description: DescriptionUpstreamError, Status: http.StatusBadGateway}
	}
	return parsed, nil
}

// submitWire 是出站请求体的冻结形状。
//
// 全部字段指针/切片 + omitempty：缺席与显式零值必须可区分（index=0 是非法
// 入参，不能因为 omitempty 被静默丢掉后让上游按缺省索引执行）。本结构只
// 承载客户端字段；网关侧身份/渠道/归属不在这里，避免把凭据带进上游请求体。
type submitWire struct {
	Prompt        *string        `json:"prompt,omitempty"`
	CustomID      *string        `json:"customId,omitempty"`
	BotType       *string        `json:"botType,omitempty"`
	NotifyHook    *string        `json:"notifyHook,omitempty"`
	Action        *string        `json:"action,omitempty"`
	Index         *int           `json:"index,omitempty"`
	State         *string        `json:"state,omitempty"`
	TaskID        *string        `json:"taskId,omitempty"`
	Base64Array   []string       `json:"base64Array,omitempty"`
	Content       *string        `json:"content,omitempty"`
	MaskBase64    *string        `json:"maskBase64,omitempty"`
	Dimensions    *string        `json:"dimensions,omitempty"`
	SourceBase64  *string        `json:"sourceBase64,omitempty"`
	TargetBase64  *string        `json:"targetBase64,omitempty"`
	AccountFilter map[string]any `json:"accountFilter,omitempty"`
}

// buildSubmitBody 组装上游请求体。
//
// 只发归一化层允许的字段：accountFilter/notifyHook 已在 Normalize 里按开关
// 剥离（nil 即不发），base64 数组已过 R-MJ-16 上界。动作类端点补发 action/
// index/taskId，使上游无需再推断网关解析出的动作。
func buildSubmitBody(endpoint Endpoint, n *NormalizedSubmit) ([]byte, error) {
	wire := submitWire{
		Prompt:        n.Prompt,
		CustomID:      n.CustomID,
		BotType:       n.BotType,
		NotifyHook:    n.NotifyHook,
		State:         n.State,
		Base64Array:   n.Base64Array,
		Content:       n.Content,
		MaskBase64:    n.MaskBase64,
		Dimensions:    n.Dimensions,
		SourceBase64:  n.SourceBase64,
		TargetBase64:  n.TargetBase64,
		AccountFilter: n.AccountFilter,
	}
	// taskId：simple-change 的 content 已含 taskId，基线也不在该端点发 taskId；
	// edits 没有 taskId。其余携带 taskId 的端点都原样补发，供上游锁定任务。
	if endpoint != EndpointSimpleChange && endpoint != EndpointEdits && n.TaskID != "" {
		taskID := n.TaskID
		wire.TaskID = &taskID
	}
	// change 必须显式发 action + index（上游按这两个字段执行放大/变换）；
	// video 强制 VIDEO（relay-05 §2.2）。其它端点的动作由固定路径或 customId
	// 决定，不发 action 以免与上游默认值冲突。
	switch endpoint {
	case EndpointChange:
		action := string(n.Action)
		index := n.Index
		wire.Action = &action
		wire.Index = &index
	case EndpointVideo:
		action := string(ActionVideo)
		wire.Action = &action
	case EndpointAction:
		// customId 即动作载体；不发 action/index，避免上游按两个来源做出
		// 不一致的解释（动作/索引已由网关解析，但上游以按钮为准）。
	}
	return json.Marshal(wire)
}

// parseSubmitResponse 结构化解析上游受理体。
//
// code 同时接受 number 与 string（midjourney-proxy 不同版本/配置下两种都
// 出现过）；result 接受 string/number/[]string。解析失败返回错误，调用方
// 按「上游坏响应」处理——绝不把无法解析的响应当成成功。
func parseSubmitResponse(raw []byte) (*SubmitResponse, error) {
	var wire struct {
		Code        json.RawMessage `json:"code"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
		Properties  map[string]any  `json:"properties"`
		Status      json.RawMessage `json:"status"`
		Progress    json.RawMessage `json:"progress"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, err
	}
	out := &SubmitResponse{
		Description: strings.TrimSpace(wire.Description),
		Properties:  wire.Properties,
	}
	code, ok := parseFlexibleInt(wire.Code)
	if !ok {
		return nil, fmt.Errorf("mj: upstream response has no usable code")
	}
	out.Code = code
	if wire.Result != nil {
		result, results, resultErr := parseResult(wire.Result)
		if resultErr != nil {
			return nil, resultErr
		}
		out.Result, out.Results = result, results
	}
	out.Status = rawString(wire.Status)
	out.Progress = rawString(wire.Progress)
	// 状态也可能嵌在 properties 里（基线受理体的 properties.status）。
	if out.Status == "" && wire.Properties != nil {
		out.Status = rawStringFromAny(wire.Properties["status"])
	}
	if out.Progress == "" && wire.Properties != nil {
		out.Progress = rawStringFromAny(wire.Properties["progress"])
	}
	return out, nil
}

// parseFlexibleInt 解析 number 或十进制 string。
func parseFlexibleInt(raw json.RawMessage) (int, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, false
	}
	var asInt int
	if err := json.Unmarshal(raw, &asInt); err == nil {
		return asInt, true
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err != nil {
		return 0, false
	}
	asString = strings.TrimSpace(asString)
	if asString == "" {
		return 0, false
	}
	value, err := strconv.Atoi(asString)
	if err != nil {
		return 0, false
	}
	return value, true
}

// parseResult 把 result 解析为单值 + 数组两种投影。
//
// 单值端点拿到数组时取第一个元素（部分 proxy 版本对单任务也返回数组）；
// 上传端点拿到字符串时也放进 Results，避免调用方按端点再写一套解析。
//
// 元素类型严格（M5）：数组里出现 object/bool/null 等无法表达为 MjId/URL
// 的值时返回错误，而不是静默丢弃——静默丢项会让「上游返回了坏数据」被
// 伪装成「部分成功」，客户端/计费都无法察觉。
func parseResult(raw json.RawMessage) (string, []string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil, nil
	}
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		return single, []string{single}, nil
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err == nil && number.String() != "" {
		return number.String(), []string{number.String()}, nil
	}
	var items []any
	if err := json.Unmarshal(raw, &items); err == nil {
		out := make([]string, 0, len(items))
		for _, item := range items {
			switch typed := item.(type) {
			case string:
				out = append(out, typed)
			case float64:
				out = append(out, strconv.FormatFloat(typed, 'f', -1, 64))
			default:
				return "", nil, fmt.Errorf("mj: upstream result contains unsupported element type %T", item)
			}
		}
		first := ""
		if len(out) > 0 {
			first = out[0]
		}
		return first, out, nil
	}
	return "", nil, fmt.Errorf("mj: upstream result is neither a scalar nor an array")
}

// rawString 把 JSON string 解出来；非 string/空值返回空串。
func rawString(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}
	return strings.TrimSpace(value)
}

// rawStringFromAny 把 properties 里的标量转成字符串；非标量返回空串。
func rawStringFromAny(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	}
	return ""
}
