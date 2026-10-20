// Package httpapi 的 OpenAI Videos 端点（T4 / P-04 / R-10）。
//
// 端点面（只服务插件任务数据面，不新建异步表）：
//
//	POST /v1/videos                 提交视频生成任务 → tasks 行
//	GET  /v1/videos/{id}            查询任务 → OpenAI video 对象
//	GET  /v1/videos/{id}/content    取视频产物（Bearer 归属校验，不经能力票据）
//
// 映射（P-04）：视频请求与 /v1/tasks/{key} 完全同一条提交管线——渠道选择、
// 预扣、退款、落库只有一份实现；差别只在外层形状：OpenAI Videos 把插件 key
// 藏在 `model` 后面（没有路径段），响应是 video 对象而不是 public_task_id。
//
// 为什么不把这两个端点交给插件原生路由（R-10）：`/v1/videos*` 是冻结的
// 内建形状，插件声明同名路径会在注册期被拒绝；本文件也不查 RouteIndex，
// 因此这些形状**不得**加入 pluginDispatchedBuiltinShapes。
package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/tasks"
)

const (
	// videoIDPrefix 是 OpenAI Videos 的对外 id 前缀。任务行用 task_ 前缀，
	// 两者共享同一段随机量，映射是纯前缀替换（与 resp_/task_ 同口径）。
	videoIDPrefix = "video_"
	// videoObjectType 是 OpenAI video 对象的 object 字段冻结值。
	videoObjectType = "video"
)

// handleVideosSubmit 是 POST /v1/videos：把 OpenAI Videos 提交翻译为一次
// 任务提交。
//
// 插件选择：OpenAI Videos 请求体没有插件 key，只有 `model`。宿主按当前
// 插件快照把 model 解析为唯一的生效插件；0 个命中 → 404 model_not_found，
// 多个命中 → 400（歧义没有安全消解方式，静默按字典序选一个会让计费/路由
// 随快照重建而漂移）。
//
// 复用方式：先把 body 读成有界内存（后续两个分支都要用），解析出 model
// 后把插件 key 写回 PathValue("key")，再交给 handleSubmit。handleSubmit
// 内部的二次读体是有界的（MaxBytesReader），不会重复放大。
// 响应改写用捕获式 ResponseWriter：提交管线的状态码/错误信封原样透传，
// 只有 202 成功被翻译成 video 对象——不在本文件复制一份计费/退款逻辑。
func (h *TasksHandler) handleVideosSubmit(w http.ResponseWriter, r *http.Request) {
	requestID := newRelayRequestID()
	principal, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		writeRelayError(w, http.StatusUnauthorized, requestID, "", "invalid token")
		return
	}
	body, err := readBoundedBody(w, r, maxTaskSubmitBytes)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeRelayError(w, http.StatusRequestEntityTooLarge, requestID, "payload_too_large", "request body is too large")
			return
		}
		writeRelayError(w, http.StatusBadRequest, requestID, "invalid_request_error", "failed to read request body")
		return
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		// multipart 不在支持范围：不静默降级（R-13），明确告诉调用方。
		writeRelayError(w, http.StatusBadRequest, requestID, "invalid_request_error",
			"videos request body must be a JSON object (multipart uploads are not supported)")
		return
	}
	model := strings.TrimSpace(toStringValue(payload["model"]))
	if model == "" {
		writeRelayError(w, http.StatusBadRequest, requestID, "invalid_request_error", "model is required")
		return
	}
	if !tokenModelAllowed(principal.ModelLimits, model) {
		writeRelayError(w, http.StatusForbidden, requestID, "distributor_token_model_forbidden",
			"this token is not allowed to access the requested model")
		return
	}
	key, ambiguous, found := h.taskPluginKeyForModel(model)
	if !found {
		writeRelayError(w, http.StatusNotFound, requestID, "model_not_found",
			"no task plugin serves the requested model")
		return
	}
	if ambiguous {
		writeRelayError(w, http.StatusBadRequest, requestID, "invalid_request_error",
			"multiple task plugins declare the requested model")
		return
	}

	// 复原 body 并写回 key：handleSubmit 的唯一输入面就是这两项。
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.SetPathValue("key", key)

	captured := newCapturedResponse()
	h.handleSubmit(captured, r)
	if captured.status != http.StatusAccepted {
		captured.flush(w)
		return
	}
	var accepted taskSubmitResponse
	if err := json.Unmarshal(captured.body.Bytes(), &accepted); err != nil || accepted.PublicTaskID == "" {
		// 提交管线刚返回 202 却给不出任务 id：这是宿主内部契约破裂。
		// 不构造一个没有 id 的 video 对象（客户端会当成功并无限轮询）。
		writeRelayError(w, http.StatusInternalServerError, requestID, "internal_error",
			"task submission returned an unusable task id")
		return
	}
	now := time.Now().UTC()
	writeJSON(w, http.StatusOK, videoObject{
		ID:        videoPublicID(accepted.PublicTaskID),
		Object:    videoObjectType,
		CreatedAt: now.Unix(),
		Status:    videoStatusForState(accepted.State),
		Model:     model,
		Progress:  videoProgressForState(accepted.State, 0),
		Seconds:   payload["seconds"],
		Size:      toStringValue(payload["size"]),
	})
}

// handleVideosGet 是 GET /v1/videos/{id}：按归属读取任务并投影为 video 对象。
//
// 不存在 / 非 video_ 前缀 / 不属于本人一律 404 且响应相同：公开 id 是高熵
// 随机量，但「存在但无权」与「不存在」的分叉仍然构成存在性 oracle。
func (h *TasksHandler) handleVideosGet(w http.ResponseWriter, r *http.Request) {
	requestID := newRelayRequestID()
	task, ok := h.lookupOwnedVideo(w, r, requestID)
	if !ok {
		return
	}
	input, _ := task.Properties["Input"].(map[string]any)
	writeJSON(w, http.StatusOK, videoObject{
		ID:        videoPublicID(task.PublicTaskID),
		Object:    videoObjectType,
		CreatedAt: videoCreatedAt(task),
		Status:    videoStatusForState(task.State),
		Model:     toStringValue(task.Properties["OriginModelName"]),
		Progress:  videoProgressForState(task.State, task.ProgressPercent),
		Seconds:   input["seconds"],
		Size:      toStringValue(input["size"]),
		Error:     videoErrorForTask(task),
	})
}

// handleVideosContent 是 GET|HEAD /v1/videos/{id}/content：代理第一个视频产物。
//
// 授权用 Bearer 归属（不是 /v1/tasks 的能力 URL 票据）：Videos 客户端按
// OpenAI SDK 的约定带 Bearer 直接取内容，宿主不需要把票据塞进 video 对象。
// 产物选择：优先 type=video 的项，其次第一项——任务可能同时有封面图等产物，
// 只要没有视频产物就 404，绝不把一张图当视频返回（R-13）。
func (h *TasksHandler) handleVideosContent(w http.ResponseWriter, r *http.Request) {
	requestID := newRelayRequestID()
	task, ok := h.lookupOwnedVideo(w, r, requestID)
	if !ok {
		return
	}
	artifact, ok := videoArtifact(tasks.ProjectArtifacts(task.Data))
	if !ok {
		writeRelayError(w, http.StatusNotFound, requestID, "artifact_not_found", "video content is not available")
		return
	}
	h.serveTaskArtifact(w, r, requestID, task, artifact)
}

// lookupOwnedVideo 完成 video 端点的公共前置：Bearer 身份、video_ → task_
// 映射、归属读取。任何失败都写 404（除未认证 401），见 handleVideosGet。
func (h *TasksHandler) lookupOwnedVideo(w http.ResponseWriter, r *http.Request, requestID string) (*tasks.Task, bool) {
	principal, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		writeRelayError(w, http.StatusUnauthorized, requestID, "", "invalid token")
		return nil, false
	}
	taskID, ok := resolveTaskFromVideoID(r.PathValue("id"))
	if !ok {
		writeVideoNotFound(w, requestID)
		return nil, false
	}
	task, err := h.store.GetByPublicIDAny(r.Context(), taskID)
	if err != nil {
		if errors.Is(err, tasks.ErrTaskNotFound) {
			writeVideoNotFound(w, requestID)
			return nil, false
		}
		writeRelayError(w, http.StatusInternalServerError, requestID, "database_error", "failed to read the task")
		return nil, false
	}
	if task.UserID != principal.UserID {
		writeVideoNotFound(w, requestID)
		return nil, false
	}
	return task, true
}

// taskPluginKeyForModel 把客户端模型解析为唯一的生效插件 key。
//
// models 列表是插件 manifest 的声明面；快照里只会保留可执行插件
// （disabled/compile_failed 的 entry.Plugin 为 nil）。多个插件声明同一模型
// 是配置歧义：返回 ambiguous 由调用方 400，绝不按 key 字典序静默择一。
func (h *TasksHandler) taskPluginKeyForModel(model string) (key string, ambiguous, found bool) {
	snapshot := h.plugins.Snapshot()
	for _, candidate := range snapshot.Keys() {
		compiled, ok := snapshot.Plugin(candidate)
		if !ok {
			continue
		}
		manifest := compiled.Manifest()
		if manifest == nil || !containsStringValue(manifest.Models, model) {
			continue
		}
		if found {
			return "", true, true
		}
		key, found = candidate, true
	}
	return key, false, found
}

func containsStringValue(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// resolveTaskFromVideoID 把 video_<x> 映射为 task_<x>（纯前缀替换，同 resp_）。
func resolveTaskFromVideoID(raw string) (string, bool) {
	id := strings.TrimSpace(raw)
	if !strings.HasPrefix(id, videoIDPrefix) {
		return "", false
	}
	suffix := id[len(videoIDPrefix):]
	if suffix == "" {
		return "", false
	}
	return tasks.PublicTaskIDPrefix + suffix, true
}

// videoPublicID 把 task_<x> 写成 video_<x>。
func videoPublicID(publicTaskID string) string {
	return videoIDPrefix + strings.TrimPrefix(publicTaskID, tasks.PublicTaskIDPrefix)
}

// writeVideoNotFound 是 video 端点唯一的「不存在」出口（文案固定、不区分原因）。
func writeVideoNotFound(w http.ResponseWriter, requestID string) {
	writeRelayError(w, http.StatusNotFound, requestID, "not_found", "video not found")
}

// videoObject 是 OpenAI Videos 的成功响应体。
//
// 字段只填宿主确实拥有的事实（id/状态/模型/进度/输入参数）；不构造上游
// 原始响应字段（如 remixed_from_video_id）。Seconds 原样回显请求值，
// 因为 OpenAI 对该字段的类型在版本间有 string/number 两种写法，宿主不复刻
// 某个特定版本的枚举。
type videoObject struct {
	ID        string      `json:"id"`
	Object    string      `json:"object"`
	CreatedAt int64       `json:"created_at"`
	Status    string      `json:"status"`
	Model     string      `json:"model,omitempty"`
	Progress  int         `json:"progress"`
	Seconds   any         `json:"seconds,omitempty"`
	Size      string      `json:"size,omitempty"`
	Error     *videoError `json:"error,omitempty"`
}

// videoError 是 failed 状态下的错误对象（OpenAI video 的 error 子对象）。
type videoError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// videoStatusForState 把任务的 7 态映射为 OpenAI Videos 的 4 值状态。
//
// UNKNOWN 映射为 failed 而不是 in_progress：设计禁止把无法识别的上游状态
// 默认成进行中（客户端会无限轮询），而 Videos 状态机没有 unknown 值。
func videoStatusForState(state tasks.State) string {
	switch state {
	case tasks.StateSuccess:
		return "completed"
	case tasks.StateFailure, tasks.StateUnknown:
		return "failed"
	case tasks.StateInProgress:
		return "in_progress"
	default:
		// not_start / submitted / queued
		return "queued"
	}
}

// videoProgressForState 归一进度：终态恒为 100/0，其余回显任务进度。
func videoProgressForState(state tasks.State, progress int) int {
	switch state {
	case tasks.StateSuccess:
		return 100
	case tasks.StateFailure, tasks.StateUnknown:
		return 0
	}
	if progress < 0 {
		return 0
	}
	if progress > 100 {
		return 100
	}
	return progress
}

// videoErrorForTask 只为 failure/unknown 生成错误对象。
//
// FailReason 是插件解析出的失败原因（已通过 /v1/tasks 查询对本人可见），
// 原样回显不会扩大暴露面；unknown 给出固定文案而不是伪造上游错误。
func videoErrorForTask(task *tasks.Task) *videoError {
	switch task.State {
	case tasks.StateFailure:
		message := task.FailReason
		if strings.TrimSpace(message) == "" {
			message = "video generation failed"
		}
		return &videoError{Code: "generation_failed", Message: message}
	case tasks.StateUnknown:
		return &videoError{Code: "unknown_state", Message: "upstream reported an unrecognized task state"}
	default:
		return nil
	}
}

// videoCreatedAt 返回 video 对象的 created_at（Unix 秒）：优先 submit_time，
// 回退 created_at，绝不使用 now()（同一对象每次查询必须给出同一时间）。
func videoCreatedAt(task *tasks.Task) int64 {
	if task.SubmitTime != nil {
		return task.SubmitTime.Unix()
	}
	if !task.CreatedAt.IsZero() {
		return task.CreatedAt.Unix()
	}
	return 0
}

// videoArtifact 选择要返回内容的产物：只接受 type=video 的项。
//
// 不回落「第一项」：任务可能同时有封面图/字幕等产物，把一张图当视频返回
// 会让客户端把非视频字节当成内容保存（R-13：宁可 404 也不静默给错答案）。
func videoArtifact(items []tasks.ArtifactView) (tasks.ArtifactView, bool) {
	for _, item := range items {
		if strings.EqualFold(item.Type, "video") {
			return item, true
		}
	}
	return tasks.ArtifactView{}, false
}

// readBoundedBody 读取并限幅请求体（多读 1 字节区分「恰好等于上限」与超限）。
func readBoundedBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	return body, nil
}

// capturedResponse 是只用于「复用 handleSubmit 并改写响应」的内存写入器。
//
// handleSubmit 只写固定 JSON（不流式、不 flush），因此捕获完整响应是安全的；
// 出错时 flush 把原始错误信封（含状态码/Content-Type/X-Request-ID）原样
// 交给客户端，成功时由调用方翻译成目标形状。
type capturedResponse struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func newCapturedResponse() *capturedResponse {
	return &capturedResponse{header: make(http.Header)}
}

func (c *capturedResponse) Header() http.Header { return c.header }

func (c *capturedResponse) WriteHeader(status int) {
	if c.status == 0 {
		c.status = status
	}
}

func (c *capturedResponse) Write(p []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	return c.body.Write(p)
}

// flush 把捕获的响应原样写回真实 ResponseWriter。
func (c *capturedResponse) flush(w http.ResponseWriter) {
	for name, values := range c.header {
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
	status := c.status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write(c.body.Bytes())
}
