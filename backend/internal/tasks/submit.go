package tasks

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"newtg/gateway/internal/plugin"
)

// 本文件是提交管线的执行段：插件构建请求 → 宿主出站 → 插件解析响应。
// 渠道选择、计费、持久化与 HTTP 映射都在 httpapi；本文件不知道身份/计费，
// 只保证「插件只做变换，宿主拥有连接」这条边界（relay-06 §0.1）。

var (
	// ErrPluginHook 表示插件钩子执行失败（超时/异常/缺钩子）。
	ErrPluginHook = errors.New("tasks: plugin hook failed")
	// ErrSubmitResponse 表示 parseSubmitResponse 的返回值形状非法。
	ErrSubmitResponse = errors.New("tasks: invalid submit response")
	// ErrUpstreamStatus 表示上游返回非 2xx。
	ErrUpstreamStatus = errors.New("tasks: upstream returned non-2xx status")
)

// SubmitInput 是一次提交所需的全部输入。凭据由调用方解密后传入，执行器
// 只在内存中使用，绝不写进返回值。
type SubmitInput struct {
	Plugin       *plugin.CompiledPlugin
	Channel      ChannelTarget
	Model        string
	Body         map[string]any
	BaseURL      string
	APIKey       string
	AllowPrivate bool // 仅测试
}

// SubmitResult 是提交成功后需要持久化的投影。
type SubmitResult struct {
	UpstreamTaskID string
	State          State
	RawState       string
	Progress       int
	Data           map[string]any
	Immediate      bool
}

// Submit 执行一次任务提交。
//
// 顺序冻结为「构建 → 校验/出站 → 解析」：出站前必须完成主机白名单与
// 字面量地址检查，否则插件可以用一个自造 URL 把渠道凭据发到任意主机。
func Submit(ctx context.Context, client *http.Client, in SubmitInput) (*SubmitResult, error) {
	if in.Plugin == nil {
		return nil, fmt.Errorf("tasks: submit without a compiled plugin")
	}
	if client == nil {
		client = NewOutboundClient()
	}
	manifest := in.Plugin.Manifest()
	upstreamModel := in.Model
	if mapped, ok := in.Channel.ModelMapping[in.Model]; ok && mapped != "" {
		upstreamModel = mapped
	}

	pluginCtx := buildPluginContext(in, upstreamModel)
	rawDesc, err := in.Plugin.Call(ctx, "buildSubmitRequest", pluginCtx)
	if err != nil {
		return nil, fmt.Errorf("%w: buildSubmitRequest", ErrPluginHook)
	}
	desc, err := DecodeRequestDescriptor(rawDesc)
	if err != nil {
		return nil, err
	}

	allowed := AllowedHosts(in.BaseURL, manifest.AllowedHosts)
	resp, err := DoOutbound(ctx, client, desc, allowed, in.AllowPrivate)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("%w: %d", ErrUpstreamStatus, resp.StatusCode)
	}

	rawParsed, err := in.Plugin.Call(ctx, "parseSubmitResponse", map[string]any{
		"statusCode": resp.StatusCode,
		"headers":    resp.Headers,
		"body":       resp.Body,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: parseSubmitResponse", ErrPluginHook)
	}
	return decodeSubmitResponse(rawParsed)
}

// buildPluginContext 组装 DriverContext（relay-06 §3.2）。
//
// 设计只冻结身份/基址/凭据几个键；sample 之类的插件还会读 prompt/seconds，
// 因此把请求体字段作为可选上下文并入（宿主键最后写入，body 不能覆盖
// model/apiKey 等身份字段——否则一个请求就能替换计费身份或凭据）。
func buildPluginContext(in SubmitInput, upstreamModel string) map[string]any {
	ctx := make(map[string]any, len(in.Body)+8)
	for k, v := range in.Body {
		ctx[k] = v
	}
	ctx["model"] = in.Model
	ctx["upstreamModel"] = upstreamModel
	ctx["baseUrl"] = in.BaseURL
	ctx["apiKey"] = in.APIKey

	authHeader, auth := "", ""
	switch in.Plugin.Manifest().Auth.Type {
	case "api_key":
		authHeader, auth = "Authorization", "Bearer "+in.APIKey
	case "":
		// 无鉴权：保持空串，插件自行决定是否发头。
	}
	ctx["authHeader"] = authHeader
	ctx["auth"] = auth
	return ctx
}

// decodeSubmitResponse 把 parseSubmitResponse 的返回值映射到持久化投影。
//
// 返回值只允许四字段语义（taskId / taskData / immediate / state），
// 其中 clientResponse 被设计明文拒绝（relay-06 §3.6）。这里不读取未知字段，
// 也就不会把它透传给客户端。
func decodeSubmitResponse(value any) (*SubmitResult, error) {
	obj, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: expected object", ErrSubmitResponse)
	}
	taskID := toString(obj["taskId"])
	if taskID == "" {
		return nil, fmt.Errorf("%w: empty taskId", ErrSubmitResponse)
	}
	result := &SubmitResult{
		UpstreamTaskID: taskID,
		State:          StateSubmitted,
		Data:           map[string]any{},
	}
	if rawState := toString(obj["state"]); rawState != "" {
		result.RawState = rawState
		result.State = ParseState(rawState)
	}
	if immediate, ok := obj["immediate"].(map[string]any); ok {
		status := toString(immediate["status"])
		if status != "" {
			result.Immediate = true
			result.RawState = status
			result.State = ParseState(status)
		}
	}
	// 终态（无论来自 immediate 还是顶层 state）都在提交阶段定格；非终态
	// 默认 submitted，交给 T6b 的后台轮询推进。
	if result.State.Terminal() {
		result.Immediate = true
		result.Progress = 100
	}
	if taskData, exists := obj["taskData"]; exists && taskData != nil {
		if typed, ok := taskData.(map[string]any); ok {
			result.Data = typed
		} else {
			// data 列必须是 jsonb 对象；插件返回标量/数组时包一层，绝不丢数据。
			result.Data = map[string]any{"taskData": taskData}
		}
	}
	return result, nil
}
