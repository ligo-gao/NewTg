package tasks

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"net/http"
	"strings"

	"newtg/gateway/internal/plugin"
)

// PluginGeneration 返回插件**版本代际**（写 tasks.plugin_generation）。
//
// 不能用快照的全局 generation：那是节点本地的发布序号，任何插件的
// reload 都会让它变化，会让所有历史任务的代际无意义地失效。这里用
// version + source_hash 派生（版本或源码任一变化即换代），跨节点一致，
// 也满足设计「插件版本变更时失效旧任务」的语义。
func PluginGeneration(p *plugin.CompiledPlugin) int64 {
	if p == nil {
		return 0
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(p.Version))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(p.SourceHash()))
	return int64(h.Sum64())
}

// 本文件是提交管线的执行段：插件构建请求 → 宿主出站 → 插件解析响应。
// 渠道选择、计费、持久化与 HTTP 映射都在 httpapi；本文件不知道身份/计费，
// 只保证「插件只做变换，宿主拥有连接」这条边界（relay-06 §0.1）。

var (
	// ErrPluginHook 表示插件钩子执行失败（超时/异常/缺钩子）。
	ErrPluginHook = errors.New("tasks: plugin hook failed")
	// ErrDecodeRequest 表示入站协议体无法被插件解码。
	//
	// 与 ErrPluginHook 分开是为了错误分类（W18-C 复核 M2）：decodeRequest
	// 阶段的失败通常可归因于请求参数/形状（客户端可修正），HTTP 面映射 400；
	// 其余钩子/上游失败才是 502。内部细节不外泄给客户端。
	ErrDecodeRequest = errors.New("tasks: decode request failed")
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

	// Action 是族内动作标识（relay-06 §3.2）；插件用它选择厂商动作。
	Action string
	// UpstreamKind 是 `ctx.upstream.kind`（vendor | new_api）。空值按渠道
	// 类型推断（type 60 → new_api），否则 vendor。
	UpstreamKind string
	// RequestHeaders 是入站请求头的只读投影（可能含客户端凭据，宿主只把
	// 它交给插件做变换，绝不写进任务行或日志）。
	RequestHeaders map[string]string
	// PublicTaskID 在提交阶段通常尚未分配；保留给需要稳定标识的插件。
	PublicTaskID string
	// OriginTasks 是 remix/续写解析出的原任务行（只读投影）。
	OriginTasks []OriginTask
	// Files 是本请求的上传文件。Data 只存在于宿主内存；注入 JS 的只有
	// 不透明引用（ref/field/filename/mimeType/size）。
	Files []InboundFile
}

// OriginTask 是注入 `ctx.originTasks` 的只读任务投影（relay-06 §3.2）。
type OriginTask struct {
	TaskID         string
	UpstreamTaskID string
	Action         string
	Status         string
	Data           any
}

// InboundFile 是一个上传文件在宿主侧的完整表示。
//
// 安全边界：Data 绝不经 buildPluginContext 或任何 JS 参数暴露；插件只拿到
// `ctx.files[]` 里由 opaqueFileView 生成的不透明引用。字段命名与
// FileReference{ref, field, filename, mimeType, size} 对齐。
type InboundFile struct {
	Ref      string
	Field    string
	Filename string
	MIMEType string
	Size     int64
	// Data 是宿主内存中的文件字节（或由文件数据面在替换占位符时提供）。
	Data []byte
}

// FileReference 生成字段内第 index 个文件的不透明引用（0 基）。
// 首文件为 request_file:<field>，后续为 request_file:<field>#<index>。
func FileReference(field string, index int) string {
	if index <= 0 {
		return "request_file:" + field
	}
	return fmt.Sprintf("request_file:%s#%d", field, index)
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
	// 宿主协议执行钩子（R-W18-07）：插件声明了 decodeRequest 时，先把入站
	// 协议请求（Responses 的 input/instructions/tools 等）解成插件可用的
	// 提交载荷，再交给 buildSubmitRequest。
	//
	// 为什么在 tasks 层而不是 HTTP 层执行：decodeRequest 与
	// buildSubmitRequest/parseSubmitResponse 同属「插件只做变换」的执行段，
	// 放在这里能让原生路由、/v1/tasks 与宿主协议三个入口共享同一份调用顺序，
	// 不会出现某个入口忘了执行钩子而把原始协议体直接发给上游。
	//
	// 兼容性：没有该钩子的插件（历史 routes[] 插件）完全沿用原 body —— 这是
	// 「插件不必为了升级宿主而新增钩子」的回退路径。
	if in.Plugin.HasHook("decodeRequest") {
		decoded, err := in.Plugin.Call(ctx, "decodeRequest", pluginCtx)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrDecodeRequest, err)
		}
		decodedBody, ok := decoded.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%w: decodeRequest must return an object", ErrDecodeRequest)
		}
		next := in
		next.Body = decodedBody
		pluginCtx = buildPluginContext(next, upstreamModel)
	}
	rawDesc, err := in.Plugin.Call(ctx, "buildSubmitRequest", pluginCtx)
	if err != nil {
		return nil, fmt.Errorf("%w: buildSubmitRequest", ErrPluginHook)
	}
	desc, err := DecodeRequestDescriptor(rawDesc)
	if err != nil {
		return nil, err
	}
	// 文件占位符必须在宿主发送前替换；失败属于客户端/插件可修正的输入
	// 形状问题，包装为 ErrDecodeRequest 让 HTTP 面映射 400 而非 502。
	if err := prepareSubmitBody(desc, in.Files); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrDecodeRequest, err)
	}
	if err := validateResponseType(manifest, desc); err != nil {
		return nil, err
	}

	allowed := AllowedHosts(in.BaseURL, manifest.AllowedHosts)
	if desc.ResponseType == "sse" {
		return submitSSE(ctx, client, in, desc, pluginCtx, allowed)
	}
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

// validateResponseType 落实「selected type must be declared」
// （relay-06 §3.5：meta.submitResponseTypes 默认 ["json"]）。
func validateResponseType(manifest *plugin.Manifest, desc *RequestDescriptor) error {
	declared := manifest.SubmitResponseTypes
	if len(declared) == 0 {
		declared = []string{"json"}
	}
	for _, typ := range declared {
		if typ == desc.ResponseType {
			return nil
		}
	}
	return fmt.Errorf("%w: %q is not declared in meta.submitResponseTypes", ErrUnsupportedResponseType, desc.ResponseType)
}

// prepareSubmitBody 递归替换 buildSubmitRequest JSON 体里的文件占位符。
//
// 占位符形状（relay-06 §3.4）：
//
//	{__fileRef, encoding, mimeType?, maxBytes?}
//
// 编码为 base64 或 dataUrl 字符串；宿主同时施加 64 MiB 单文件/单请求上限。
// 文件字节、reader 与路径只在本函数内部可见，输出是普通字符串。
func prepareSubmitBody(desc *RequestDescriptor, files []InboundFile) error {
	if desc == nil || desc.rawBody == nil {
		return nil
	}
	byRef := make(map[string]InboundFile, len(files))
	for _, file := range files {
		ref := strings.TrimSpace(file.Ref)
		if ref == "" {
			continue
		}
		if _, dup := byRef[ref]; dup {
			return fmt.Errorf("%w: duplicate file reference %q", ErrFilePlaceholder, ref)
		}
		byRef[ref] = file
	}
	total := int64(0)
	replaced, changed, err := replaceFilePlaceholders(desc.rawBody, byRef, &total)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	encoded, err := json.Marshal(replaced)
	if err != nil {
		return fmt.Errorf("%w: encode submit body: %v", ErrFilePlaceholder, err)
	}
	desc.Body = encoded
	return nil
}

// replaceFilePlaceholders 深度遍历 JSON 值并替换占位符对象。
//
// changed 用于区分「没有任何占位符」：无占位符时保留 DecodeRequestDescriptor
// 已编码的 Body，避免改变既有插件的数字/顺序编码结果。
func replaceFilePlaceholders(value any, files map[string]InboundFile, total *int64) (any, bool, error) {
	switch typed := value.(type) {
	case map[string]any:
		if _, isPlaceholder := typed["__fileRef"]; isPlaceholder {
			encoded, err := encodeFilePlaceholder(typed, files, total)
			return encoded, true, err
		}
		out := make(map[string]any, len(typed))
		changed := false
		for key, item := range typed {
			replaced, itemChanged, err := replaceFilePlaceholders(item, files, total)
			if err != nil {
				return nil, false, err
			}
			changed = changed || itemChanged
			out[key] = replaced
		}
		return out, changed, nil
	case []any:
		out := make([]any, len(typed))
		changed := false
		for index, item := range typed {
			replaced, itemChanged, err := replaceFilePlaceholders(item, files, total)
			if err != nil {
				return nil, false, err
			}
			changed = changed || itemChanged
			out[index] = replaced
		}
		return out, changed, nil
	default:
		return value, false, nil
	}
}

// encodeFilePlaceholder 校验占位符形状并编码一个文件。
func encodeFilePlaceholder(placeholder map[string]any, files map[string]InboundFile, total *int64) (string, error) {
	for key := range placeholder {
		switch key {
		case "__fileRef", "encoding", "mimeType", "maxBytes":
		default:
			return "", fmt.Errorf("%w: unknown placeholder field %q", ErrFilePlaceholder, key)
		}
	}
	ref, ok := placeholder["__fileRef"].(string)
	if !ok || strings.TrimSpace(ref) == "" {
		return "", fmt.Errorf("%w: __fileRef must be a non-empty string", ErrFilePlaceholder)
	}
	encoding, _ := placeholder["encoding"].(string)
	if encoding != "base64" && encoding != "dataUrl" {
		return "", fmt.Errorf("%w: encoding must be base64 or dataUrl", ErrFilePlaceholder)
	}
	file, ok := files[ref]
	if !ok {
		return "", fmt.Errorf("%w: unknown file reference %q", ErrFilePlaceholder, ref)
	}
	limit := int64(MaxFileInlinedBytes)
	if raw, exists := placeholder["maxBytes"]; exists {
		maxBytes, ok := positiveInt(raw)
		if !ok {
			return "", fmt.Errorf("%w: maxBytes must be a positive integer", ErrFilePlaceholder)
		}
		if maxBytes < limit {
			limit = maxBytes
		}
	}
	if file.Size > limit {
		return "", fmt.Errorf("%w: file %q exceeds the %d byte limit", ErrFilePlaceholder, ref, limit)
	}
	data := file.Data
	if int64(len(data)) > limit {
		return "", fmt.Errorf("%w: file %q exceeds the %d byte limit", ErrFilePlaceholder, ref, limit)
	}
	if *total+int64(len(data)) > MaxFileInlinedBytes {
		return "", fmt.Errorf("%w: inlined files exceed the %d byte limit", ErrFilePlaceholder, MaxFileInlinedBytes)
	}
	*total += int64(len(data))
	encoded := base64.StdEncoding.EncodeToString(data)
	if encoding == "base64" {
		return encoded, nil
	}
	mimeType := strings.TrimSpace(file.MIMEType)
	if override, ok := placeholder["mimeType"].(string); ok && strings.TrimSpace(override) != "" {
		mimeType = strings.TrimSpace(override)
	}
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	return "data:" + mimeType + ";base64," + encoded, nil
}

// positiveInt 接受 sobek 导出的 float64/int64/json.Number 正整数。
func positiveInt(v any) (int64, bool) {
	switch typed := v.(type) {
	case float64:
		if typed <= 0 || typed != math.Trunc(typed) || typed > math.MaxInt64 {
			return 0, false
		}
		return int64(typed), true
	case int64:
		return typed, typed > 0
	case int:
		return int64(typed), typed > 0
	case json.Number:
		n, err := typed.Int64()
		return n, err == nil && n > 0
	default:
		return 0, false
	}
}

// submitSSE 执行宿主托管的提交事件流（relay-06 §3.6）。
//
// 两种语义只能二选一：
//   - 快照（默认）：parseSubmitEvent 每次返回完整 state，宿主把最后一个
//     state 作为 response.body 交给 parseSubmitResponse；
//   - 增量（submit-sse-delta@1）：parseSubmitEventDelta 只返回 changes +
//     控制 state，宿主在 Go 内维护累积结果并施加变更；累积结果永不回到 JS。
//
// 上游事件不转发给客户端；一旦 2xx 流已开始，任何本地失败都不可重试
// （重新提交可能重复产生计费），错误原样上抛由调用方映射。
func submitSSE(ctx context.Context, client *http.Client, in SubmitInput, desc *RequestDescriptor, pluginCtx map[string]any, allowedHosts []string) (*SubmitResult, error) {
	manifest := in.Plugin.Manifest()
	delta := hasCapability(manifest.RequiredCapabilities, "submit-sse-delta@1")
	hook := "parseSubmitEvent"
	if delta {
		hook = "parseSubmitEventDelta"
	}
	if !in.Plugin.HasHook(hook) {
		return nil, fmt.Errorf("%w: %s is required for responseType=sse", ErrPluginHook, hook)
	}

	var (
		previousState any // 回传给 JS 的控制状态
		result        any // 仅增量模式：宿主累积的响应体
		resultSize    int
	)
	status, headers, err := DoOutboundSSE(ctx, client, desc, allowedHosts, OutboundPolicy(), in.AllowPrivate,
		func(event SSEEvent) error {
			raw, callErr := in.Plugin.Call(ctx, hook, pluginCtx, map[string]any{
				"event": event.Event,
				"id":    event.ID,
				"data":  event.Data,
			}, previousState)
			if callErr != nil {
				return callErr
			}
			obj, ok := raw.(map[string]any)
			if !ok {
				return fmt.Errorf("%w: %s must return an object", ErrPluginHook, hook)
			}
			if delta {
				state, changes, done, decodeErr := decodeDeltaEvent(obj)
				if decodeErr != nil {
					return decodeErr
				}
				for _, change := range changes {
					result, resultSize, decodeErr = applyJSONChange(result, resultSize, change)
					if decodeErr != nil {
						return decodeErr
					}
				}
				if resultSize > SSEEventMaxBytes {
					return fmt.Errorf("%w: accumulated result exceeds %d bytes", ErrPluginHook, SSEEventMaxBytes)
				}
				previousState = state
				if done {
					return errStopSSE
				}
				return nil
			}
			state, done, decodeErr := decodeSnapshotEvent(obj)
			if decodeErr != nil {
				return decodeErr
			}
			previousState = state
			if done {
				return errStopSSE
			}
			return nil
		})
	if err != nil {
		return nil, err
	}

	bodyValue := previousState
	if delta {
		bodyValue = result
	}
	body, err := json.Marshal(bodyValue)
	if err != nil {
		return nil, fmt.Errorf("%w: encode accumulated submit state: %v", ErrPluginHook, err)
	}
	if len(body) > SSEEventMaxBytes {
		return nil, fmt.Errorf("%w: accumulated submit state exceeds %d bytes", ErrPluginHook, SSEEventMaxBytes)
	}
	rawParsed, err := in.Plugin.Call(ctx, "parseSubmitResponse", map[string]any{
		"statusCode": status,
		"headers":    headers,
		"body":       string(body),
	})
	if err != nil {
		return nil, fmt.Errorf("%w: parseSubmitResponse", ErrPluginHook)
	}
	return decodeSubmitResponse(rawParsed)
}

// decodeSnapshotEvent 校验 parseSubmitEvent 的 {state, done}。
func decodeSnapshotEvent(obj map[string]any) (any, bool, error) {
	state, exists := obj["state"]
	if !exists {
		return nil, false, fmt.Errorf("%w: parseSubmitEvent must return state", ErrPluginHook)
	}
	done, ok := obj["done"].(bool)
	if !ok {
		return nil, false, fmt.Errorf("%w: parseSubmitEvent must return boolean done", ErrPluginHook)
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return nil, false, fmt.Errorf("%w: state is not JSON: %v", ErrPluginHook, err)
	}
	// 快照模式的累积状态同样限 1 MiB（relay-06 §5.3）。
	if len(encoded) > SSEEventMaxBytes {
		return nil, false, fmt.Errorf("%w: submit state exceeds %d bytes", ErrPluginHook, SSEEventMaxBytes)
	}
	return state, done, nil
}

// Delta 状态上限（relay-06 §3.5 / v1.md）。
const (
	deltaStateMaxBytes = 64 << 10
	deltaMaxChanges    = 256
	deltaMaxPath       = 32
	deltaMaxDepth      = 32
	deltaMaxNodes      = 32768
)

// jsonChange 是一个已校验的增量变更。
type jsonChange struct {
	Op    string
	Path  []any
	Value any
}

// decodeDeltaEvent 校验 parseSubmitEventDelta 的 {changes, state, done}。
func decodeDeltaEvent(obj map[string]any) (any, []jsonChange, bool, error) {
	for key := range obj {
		switch key {
		case "changes", "state", "done":
		default:
			return nil, nil, false, fmt.Errorf("%w: unknown delta result field %q", ErrPluginHook, key)
		}
	}
	state, exists := obj["state"]
	if !exists {
		return nil, nil, false, fmt.Errorf("%w: parseSubmitEventDelta must return state", ErrPluginHook)
	}
	encoded, err := json.Marshal(state)
	if err != nil || len(encoded) > deltaStateMaxBytes {
		return nil, nil, false, fmt.Errorf("%w: delta state must be JSON and at most %d bytes", ErrPluginHook, deltaStateMaxBytes)
	}
	done, ok := obj["done"].(bool)
	if !ok {
		return nil, nil, false, fmt.Errorf("%w: parseSubmitEventDelta must return boolean done", ErrPluginHook)
	}
	rawChanges, ok := obj["changes"].([]any)
	if !ok || len(rawChanges) > deltaMaxChanges {
		return nil, nil, false, fmt.Errorf("%w: changes must be an array of at most %d items", ErrPluginHook, deltaMaxChanges)
	}
	changes := make([]jsonChange, 0, len(rawChanges))
	for _, raw := range rawChanges {
		change, err := decodeJSONChange(raw)
		if err != nil {
			return nil, nil, false, err
		}
		changes = append(changes, change)
	}
	return state, changes, done, nil
}

func decodeJSONChange(raw any) (jsonChange, error) {
	obj, ok := raw.(map[string]any)
	if !ok {
		return jsonChange{}, fmt.Errorf("%w: each change must be an object", ErrPluginHook)
	}
	for key := range obj {
		switch key {
		case "op", "path", "value":
		default:
			return jsonChange{}, fmt.Errorf("%w: unknown change field %q", ErrPluginHook, key)
		}
	}
	op, _ := obj["op"].(string)
	switch op {
	case "set", "append", "appendText":
	default:
		return jsonChange{}, fmt.Errorf("%w: unsupported change op %q", ErrPluginHook, op)
	}
	value, exists := obj["value"]
	if !exists {
		return jsonChange{}, fmt.Errorf("%w: change %s must carry value", ErrPluginHook, op)
	}
	rawPath, ok := obj["path"].([]any)
	if !ok || len(rawPath) > deltaMaxPath {
		return jsonChange{}, fmt.Errorf("%w: change path must be an array of at most %d segments", ErrPluginHook, deltaMaxPath)
	}
	path := make([]any, len(rawPath))
	for index, segment := range rawPath {
		switch typed := segment.(type) {
		case string:
			path[index] = typed
		case float64:
			if typed < 0 || typed != math.Trunc(typed) {
				return jsonChange{}, fmt.Errorf("%w: path index must be a non-negative integer", ErrPluginHook)
			}
			path[index] = int(typed)
		case int64:
			if typed < 0 {
				return jsonChange{}, fmt.Errorf("%w: path index must be a non-negative integer", ErrPluginHook)
			}
			path[index] = int(typed)
		case int:
			if typed < 0 {
				return jsonChange{}, fmt.Errorf("%w: path index must be a non-negative integer", ErrPluginHook)
			}
			path[index] = typed
		default:
			return jsonChange{}, fmt.Errorf("%w: path segments must be strings or non-negative integers", ErrPluginHook)
		}
	}
	return jsonChange{Op: op, Path: path, Value: value}, nil
}

// applyJSONChange 把一条变更施加到宿主累积结果上。
//
// 路径语义（v1.md）：空路径 set 替换根；父容器必须已存在；数组索引必须
// 命中已有元素；不创建隐式父容器（防插件用路径拼出深层结构放大内存）。
func applyJSONChange(root any, size int, change jsonChange) (any, int, error) {
	if change.Op != "set" && len(change.Path) == 0 {
		return nil, 0, fmt.Errorf("%w: %s requires a non-empty path", ErrPluginHook, change.Op)
	}
	updated, deltaSize, err := applyChangeAt(root, change.Path, change)
	if err != nil {
		return nil, 0, err
	}
	size += deltaSize
	if size > SSEEventMaxBytes {
		return nil, 0, fmt.Errorf("%w: accumulated result exceeds %d bytes", ErrPluginHook, SSEEventMaxBytes)
	}
	nodes := 0
	if err := checkResultLimits(updated, 0, &nodes); err != nil {
		return nil, 0, err
	}
	return updated, size, nil
}

// applyChangeAt 递归定位父容器并执行叶操作；返回 (新根, 编码字节增量)。
func applyChangeAt(node any, path []any, change jsonChange) (any, int, error) {
	if len(path) == 0 {
		// 只有 set 能到达这里：替换根。
		encoded, err := json.Marshal(change.Value)
		if err != nil {
			return nil, 0, fmt.Errorf("%w: change value is not JSON: %v", ErrPluginHook, err)
		}
		return change.Value, len(encoded), nil
	}
	key := path[0]
	if len(path) == 1 {
		return applyLeaf(node, key, change)
	}
	child, err := childAt(node, key)
	if err != nil {
		return nil, 0, err
	}
	updatedChild, delta, err := applyChangeAt(child, path[1:], change)
	if err != nil {
		return nil, 0, err
	}
	if err := setChild(node, key, updatedChild); err != nil {
		return nil, 0, err
	}
	return node, delta, nil
}

func applyLeaf(container any, key any, change jsonChange) (any, int, error) {
	switch change.Op {
	case "set":
		old, exists := childMaybe(container, key)
		if err := setChild(container, key, change.Value); err != nil {
			return nil, 0, err
		}
		newLen := mustJSONLen(change.Value)
		if exists {
			return container, newLen - mustJSONLen(old), nil
		}
		if name, ok := key.(string); ok {
			// 新成员还要计入 `"key":`。
			return container, newLen + mustJSONLen(name) + 1, nil
		}
		return container, newLen, nil
	case "append":
		old, exists := childMaybe(container, key)
		if !exists {
			return nil, 0, fmt.Errorf("%w: append path does not exist", ErrPluginHook)
		}
		list, ok := old.([]any)
		if !ok {
			return nil, 0, fmt.Errorf("%w: append target must be an array", ErrPluginHook)
		}
		value := append(list, change.Value)
		if err := setChild(container, key, value); err != nil {
			return nil, 0, err
		}
		delta := mustJSONLen(change.Value)
		if len(list) > 0 {
			delta++ // 逗号
		}
		return container, delta, nil
	case "appendText":
		old, exists := childMaybe(container, key)
		if !exists {
			return nil, 0, fmt.Errorf("%w: appendText path does not exist", ErrPluginHook)
		}
		text, ok := old.(string)
		if !ok {
			return nil, 0, fmt.Errorf("%w: appendText target must be a string", ErrPluginHook)
		}
		fragment, ok := change.Value.(string)
		if !ok {
			return nil, 0, fmt.Errorf("%w: appendText value must be a string", ErrPluginHook)
		}
		merged := text + fragment
		if err := setChild(container, key, merged); err != nil {
			return nil, 0, err
		}
		return container, mustJSONLen(merged) - mustJSONLen(text), nil
	}
	return nil, 0, fmt.Errorf("%w: unsupported change op", ErrPluginHook)
}

func childMaybe(node any, key any) (any, bool) {
	switch typed := node.(type) {
	case map[string]any:
		name, ok := key.(string)
		if !ok {
			return nil, false
		}
		value, exists := typed[name]
		return value, exists
	case []any:
		index, ok := key.(int)
		if !ok || index < 0 || index >= len(typed) {
			return nil, false
		}
		return typed[index], true
	default:
		return nil, false
	}
}

func childAt(node any, key any) (any, error) {
	value, exists := childMaybe(node, key)
	if !exists {
		return nil, fmt.Errorf("%w: change path parent does not exist", ErrPluginHook)
	}
	return value, nil
}

func setChild(node any, key any, value any) error {
	switch typed := node.(type) {
	case map[string]any:
		name, ok := key.(string)
		if !ok {
			return fmt.Errorf("%w: object path segment must be a string", ErrPluginHook)
		}
		typed[name] = value
		return nil
	case []any:
		index, ok := key.(int)
		if !ok || index < 0 || index >= len(typed) {
			return fmt.Errorf("%w: array index must address an existing element", ErrPluginHook)
		}
		typed[index] = value
		return nil
	default:
		return fmt.Errorf("%w: change path does not address a container", ErrPluginHook)
	}
}

func mustJSONLen(value any) int {
	encoded, err := json.Marshal(value)
	if err != nil {
		return 0
	}
	return len(encoded)
}

// checkResultLimits 在每次变更后检查累积结果的深度与节点数。
func checkResultLimits(value any, depth int, nodes *int) error {
	if depth > deltaMaxDepth {
		return fmt.Errorf("%w: accumulated result exceeds %d levels", ErrPluginHook, deltaMaxDepth)
	}
	*nodes++
	if *nodes > deltaMaxNodes {
		return fmt.Errorf("%w: accumulated result exceeds %d nodes", ErrPluginHook, deltaMaxNodes)
	}
	switch typed := value.(type) {
	case map[string]any:
		for _, item := range typed {
			if err := checkResultLimits(item, depth+1, nodes); err != nil {
				return err
			}
		}
	case []any:
		for _, item := range typed {
			if err := checkResultLimits(item, depth+1, nodes); err != nil {
				return err
			}
		}
	}
	return nil
}

func hasCapability(capabilities []string, wanted string) bool {
	for _, capability := range capabilities {
		if capability == wanted {
			return true
		}
	}
	return false
}

// buildPluginContext 组装 DriverContext（relay-06 §3.2）。
//
// 设计只冻结身份/基址/凭据几个键；sample 之类的插件还会读 prompt/seconds，
// 因此把请求体字段作为可选上下文并入（宿主键最后写入，body 不能覆盖
// model/apiKey 等身份字段——否则一个请求就能替换计费身份或凭据）。
//
// 文件只能以不透明引用进入 JS：opaqueFileView 只复制 ref/field/filename/
// mimeType/size，`Data`、reader 与路径永不出现。`upstream`/`originTasks`
// 是只读 JSON 投影；`artifacts` 由 Responses 终态观察注入（此处不含）。
func buildPluginContext(in SubmitInput, upstreamModel string) map[string]any {
	ctx := make(map[string]any, len(in.Body)+12)
	for k, v := range in.Body {
		ctx[k] = v
	}
	// body 是**原始入站对象**的稳定入口：decodeRequest 与 buildSubmitRequest
	// 都通过 ctx.body 读取未拆分的请求（顶层字段仍并列展开以兼容既有插件）。
	// requestBody 是新契约名（v1.d.ts DriverContext）。
	ctx["body"] = in.Body
	ctx["requestBody"] = in.Body
	ctx["requestHeaders"] = copyStringMap(in.RequestHeaders)
	ctx["model"] = in.Model
	ctx["upstreamModel"] = upstreamModel
	ctx["baseUrl"] = in.BaseURL
	ctx["apiKey"] = in.APIKey
	ctx["action"] = in.Action
	ctx["publicTaskId"] = in.PublicTaskID
	ctx["upstream"] = map[string]any{"kind": upstreamKind(in)}
	ctx["files"] = opaqueFileViews(in.Files)
	if len(in.OriginTasks) > 0 {
		ctx["originTasks"] = originTaskViews(in.OriginTasks)
	}

	authHeader, auth := "", ""
	switch in.Plugin.Manifest().Auth.Type {
	case "api_key":
		// authHeader 是**完整头值**（插件直接放进 Authorization），不是头名。
		// auth 保留同一凭据串：既有插件把它直接作为 Authorization 值使用。
		authHeader = "Bearer " + in.APIKey
		auth = authHeader
	case "":
		// 无鉴权：保持空串，插件自行决定是否发头。
	}
	ctx["authHeader"] = authHeader
	ctx["auth"] = auth
	return ctx
}

// upstreamKind 返回 ctx.upstream.kind。显式声明优先；否则按渠道类型推断
// （type 60 = Token Gateway 兼容网关 → new_api），默认 vendor。
func upstreamKind(in SubmitInput) string {
	if in.UpstreamKind != "" {
		return in.UpstreamKind
	}
	if strings.TrimSpace(in.Channel.Type) == "60" {
		return "new_api"
	}
	return "vendor"
}

// opaqueFileViews 生成 JS 可见的文件引用列表（不含字节）。
func opaqueFileViews(files []InboundFile) []any {
	out := make([]any, 0, len(files))
	for _, file := range files {
		out = append(out, map[string]any{
			"ref":      file.Ref,
			"field":    file.Field,
			"filename": file.Filename,
			"mimeType": file.MIMEType,
			"size":     file.Size,
		})
	}
	return out
}

// originTaskViews 把原任务行投影成纯 JSON（不暴露 BillingContext/凭据）。
func originTaskViews(tasks []OriginTask) []any {
	out := make([]any, 0, len(tasks))
	for _, task := range tasks {
		out = append(out, map[string]any{
			"taskId":         task.TaskID,
			"upstreamTaskId": task.UpstreamTaskID,
			"action":         task.Action,
			"status":         task.Status,
			"data":           task.Data,
		})
	}
	return out
}

func copyStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
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
