// Package httpapi 的 OpenAI Batches 端点（T4 / P-04 / R-10）。
//
// 端点面（父批次只是聚合视图，不新建异步状态机）：
//
//	POST /v1/batches                 逐行提交子请求，每行建一条 tasks 记录
//	GET  /v1/batches/{id}            聚合视图（request_counts 由子任务状态现算）
//	POST /v1/batches/{id}/cancel     当前 fail-closed（见 handleBatchesCancel）
//
// 与 OpenAI Batch 的输入差异（R-13 登记）：OpenAI 的批次输入是 /v1/files
// 上传的 JSONL 文件，本仓库没有文件数据面，因此只接受**内联** `requests[]`
// 数组（JSONL 行同形：custom_id/method/url/body）。给了 input_file_id 而不给
// 内联数组时返回 501 problem+json——绝不假装读过文件后返回空批次。
//
// 每一行目前只支持 `url=/v1/videos`：batch 的语义是「每条子请求走任务数据面」，
// 而任务数据面唯一完整的 OpenAI 协议入口就是 Videos（chat/completions 是
// 同步中继，没有 task 行可聚合）。不支持的 url 在提交任何一行**之前** 400，
// 避免出现「部分行已计费、批次创建失败」的半成品。
package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/tasks"
)

const (
	// maxBatchSubmitBytes 是批次提交请求体上限。相对单任务 4MiB 放大一倍：
	// 批次最多 100 行 inline JSONL，行内 body 仍是单个 video 请求。
	maxBatchSubmitBytes = 8 << 20

	// maxBatchItems 是单批次的子请求上限。这是成本边界：每行都会独立预扣，
	// 无上限的一次 POST 会把钱包/上游打成大批量并发；100 与令牌批量操作
	// 同口径（modules-02 §2.2）。
	maxBatchItems = 100

	// batchEndpointVideos 是当前唯一可聚合的 OpenAI 端点。
	batchEndpointVideos = "/v1/videos"
	// batchCompletionWindow 是 OpenAI Batch 的固定窗口枚举值（本实现立即逐行
	// 提交，窗口只是协议字段，不代表调度）。
	batchCompletionWindow = "24h"
	// batchObjectType 是 OpenAI batch 对象的 object 字段冻结值。
	batchObjectType = "batch"
)

// BatchRepository 是批次聚合的存储接缝（生产 *tasks.Repo）。
type BatchRepository interface {
	InsertBatch(ctx context.Context, b *tasks.Batch, itemPublicIDs []string) error
	GetBatchByID(ctx context.Context, userID int64, id string) (*tasks.Batch, error)
}

var _ BatchRepository = (*tasks.Repo)(nil)

// batchCreateRequest 是 POST /v1/batches 的请求体。
//
// requests 是本仓库的内联扩展（见文件头 why）；metadata 不在冻结表结构里，
// 因此非空时显式 400，不做「收了但丢弃」的静默降级。
type batchCreateRequest struct {
	InputFileID      string            `json:"input_file_id"`
	Endpoint         string            `json:"endpoint"`
	CompletionWindow string            `json:"completion_window"`
	Metadata         map[string]string `json:"metadata"`
	Requests         []batchItemInput  `json:"requests"`
}

// batchItemInput 是内联 JSONL 的一行。
type batchItemInput struct {
	CustomID string         `json:"custom_id"`
	Method   string         `json:"method"`
	URL      string         `json:"url"`
	Body     map[string]any `json:"body"`
}

// batchObject 是 OpenAI Batch 对象的只读子集 + 宿主扩展 items[]。
//
// items 是网关扩展：OpenAI 通过 output_file_id 交付逐行结果，本实现没有
// 文件面，直接给出每条子任务的任务 id 与状态，避免客户端无从追踪。
type batchObject struct {
	ID               string             `json:"id"`
	Object           string             `json:"object"`
	Endpoint         string             `json:"endpoint"`
	CompletionWindow string             `json:"completion_window"`
	Status           string             `json:"status"`
	RequestCounts    batchRequestCounts `json:"request_counts"`
	CreatedAt        int64              `json:"created_at"`
	Items            []batchItemObject  `json:"items,omitempty"`
}

type batchRequestCounts struct {
	Total     int `json:"total"`
	Completed int `json:"completed"`
	Failed    int `json:"failed"`
}

type batchItemObject struct {
	TaskID          string `json:"task_id"`
	State           string `json:"state"`
	ProgressPercent int    `json:"progress_percent"`
}

// handleBatchesSubmit 是 POST /v1/batches：校验全部行 → 逐行提交 → 一个事务
// 写入父批次与全部关联。
//
// 校验（行形状 / 自定义 id / url / model 可解析）在任何提交与计费之前完成：
// 参数错误必须零副作用。提交阶段的行失败（额度不足、上游拒绝）不回滚整批——
// 那些行没有任务行、退款由提交管线自己完成；它们计入 request_counts.failed。
func (h *TasksHandler) handleBatchesSubmit(w http.ResponseWriter, r *http.Request) {
	requestID := newRelayRequestID()
	principal, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		writeRelayError(w, http.StatusUnauthorized, requestID, "", "invalid token")
		return
	}
	if h.batches == nil {
		writeRelayError(w, http.StatusServiceUnavailable, requestID, "database_error",
			"batch storage is not configured")
		return
	}
	body, err := readBoundedBody(w, r, maxBatchSubmitBytes)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeRelayError(w, http.StatusRequestEntityTooLarge, requestID, "payload_too_large", "request body is too large")
			return
		}
		writeRelayError(w, http.StatusBadRequest, requestID, "invalid_request_error", "failed to read request body")
		return
	}
	var req batchCreateRequest
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&req); err != nil {
		writeRelayError(w, http.StatusBadRequest, requestID, "invalid_request_error", "request body must be a JSON object")
		return
	}
	if len(req.Requests) == 0 {
		if strings.TrimSpace(req.InputFileID) != "" {
			// R-13：没有 /v1/files，读不到 JSONL；返回明确的不支持，而不是
			// 解析出一个空批次（客户端会误以为任务已受理）。
			writeBatchUnsupported(w, requestID,
				"batch input_file_id is not supported",
				"this gateway has no /v1/files data plane; send the JSONL rows inline as requests[]")
			return
		}
		writeRelayError(w, http.StatusBadRequest, requestID, "invalid_request_error", "requests is required")
		return
	}
	if len(req.Requests) > maxBatchItems {
		writeRelayError(w, http.StatusBadRequest, requestID, "invalid_request_error", "too many batch requests")
		return
	}
	if len(req.Metadata) > 0 {
		writeRelayError(w, http.StatusBadRequest, requestID, "invalid_request_error", "batch metadata is not supported")
		return
	}
	if endpoint := strings.TrimSpace(req.Endpoint); endpoint != "" && endpoint != batchEndpointVideos {
		writeRelayError(w, http.StatusBadRequest, requestID, "invalid_request_error", "unsupported batch endpoint")
		return
	}
	if window := strings.TrimSpace(req.CompletionWindow); window != "" && window != batchCompletionWindow {
		writeRelayError(w, http.StatusBadRequest, requestID, "invalid_request_error", "unsupported completion_window")
		return
	}
	if status, code, message, ok := h.validateBatchItems(principal, req.Requests); !ok {
		writeRelayError(w, status, requestID, code, message)
		return
	}

	// 逐个提交：每行沿用 /v1/videos 的同一提交管线（渠道/预扣/退款/落库）。
	succeeded := make([]string, 0, len(req.Requests))
	for _, item := range req.Requests {
		publicID, ok := h.submitBatchVideo(r, item.Body)
		if !ok {
			// 提交失败已在提交管线内退款；这里只记事实供运维排查，响应只带计数。
			slog.Warn("tasks: batch item submission failed",
				"user_id", principal.UserID, "custom_id", item.CustomID)
			continue
		}
		succeeded = append(succeeded, publicID)
	}

	batch := &tasks.Batch{
		UserID:  principal.UserID,
		Status:  tasks.BatchStatusInProgress,
		Total:   len(req.Requests),
		Items:   nil,
		TokenID: tokenIDOf(principal),
	}
	id, err := tasks.NewBatchID()
	if err != nil {
		writeRelayError(w, http.StatusInternalServerError, requestID, "internal_error", "failed to allocate a batch id")
		return
	}
	batch.ID = id
	if err := h.batches.InsertBatch(r.Context(), batch, succeeded); err != nil {
		// 事务已回滚：批次与关联都不存在。已提交的子任务仍是有效的独立任务
		// （各自已落库/计费），不会因为批次创建失败而丢失或重复扣费。
		writeRelayError(w, http.StatusInternalServerError, requestID, "database_error", "failed to persist the batch")
		return
	}
	view, err := h.batches.GetBatchByID(r.Context(), principal.UserID, id)
	if err != nil {
		// 批次已落库；聚合读取失败不回滚（那会让已受理的批次消失），
		// 用创建期快照兜底响应。
		view = batch
		view.ID = tasks.FormatBatchPublicID(batch.ID)
		view.Status = tasks.BatchStatusInProgress
	}
	writeJSON(w, http.StatusOK, batchToObject(view, len(req.Requests)))
}

// validateBatchItems 在提交/计费之前校验全部行。
//
// 返回 (status, code, message, ok)；ok=true 表示全部行可提交。model 的解析
// 与 /v1/videos 同一实现（taskPluginKeyForModel），避免两个入口对「同一模型
// 属于哪个插件」给出两套答案。
func (h *TasksHandler) validateBatchItems(principal *auth.Principal, items []batchItemInput) (int, string, string, bool) {
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		customID := strings.TrimSpace(item.CustomID)
		if customID == "" {
			return http.StatusBadRequest, "invalid_request_error", "custom_id is required", false
		}
		if len(customID) > 64 {
			return http.StatusBadRequest, "invalid_request_error", "custom_id is too long", false
		}
		if _, dup := seen[customID]; dup {
			return http.StatusBadRequest, "invalid_request_error", "duplicate custom_id", false
		}
		seen[customID] = struct{}{}
		if method := strings.TrimSpace(item.Method); method != "" && !strings.EqualFold(method, http.MethodPost) {
			return http.StatusBadRequest, "invalid_request_error", "batch item method must be POST", false
		}
		if strings.TrimSpace(item.URL) != batchEndpointVideos {
			return http.StatusBadRequest, "invalid_request_error",
				"batch item url must be " + batchEndpointVideos, false
		}
		if item.Body == nil {
			return http.StatusBadRequest, "invalid_request_error", "batch item body is required", false
		}
		model := strings.TrimSpace(toStringValue(item.Body["model"]))
		if model == "" {
			return http.StatusBadRequest, "invalid_request_error", "batch item model is required", false
		}
		if !tokenModelAllowed(principal.ModelLimits, model) {
			return http.StatusForbidden, "distributor_token_model_forbidden",
				"this token is not allowed to access the requested model", false
		}
		_, ambiguous, found := h.taskPluginKeyForModel(model)
		if !found {
			return http.StatusNotFound, "model_not_found", "no task plugin serves the requested model", false
		}
		if ambiguous {
			return http.StatusBadRequest, "invalid_request_error", "multiple task plugins declare the requested model", false
		}
	}
	return 0, "", "", true
}

// submitBatchVideo 把一行内联请求交给 /v1/videos 的提交实现。
//
// 子请求继承父请求的 context 与认证头：RelayAuth 已经完成，handleSubmit 只
// 从 context 取 principal；这里不再经过中间件，也就不存在第二条鉴权链。
// 只转发 body（model/seconds/...），不转发 custom_id——它是批次维度，不属于
// 任务语义。
func (h *TasksHandler) submitBatchVideo(parent *http.Request, body map[string]any) (string, bool) {
	raw, err := json.Marshal(body)
	if err != nil {
		return "", false
	}
	sub := parent.Clone(parent.Context())
	urlCopy := *parent.URL
	urlCopy.Path = batchEndpointVideos
	urlCopy.RawPath = ""
	urlCopy.RawQuery = ""
	sub.URL = &urlCopy
	sub.RequestURI = ""
	sub.Method = http.MethodPost
	sub.Body = io.NopCloser(bytes.NewReader(raw))
	sub.ContentLength = int64(len(raw))
	sub.Header = parent.Header.Clone()

	captured := newCapturedResponse()
	h.handleVideosSubmit(captured, sub)
	if captured.status != http.StatusOK {
		return "", false
	}
	var video videoObject
	if err := json.Unmarshal(captured.body.Bytes(), &video); err != nil {
		return "", false
	}
	taskID, ok := resolveTaskFromVideoID(video.ID)
	if !ok {
		return "", false
	}
	return taskID, true
}

// handleBatchesGet 是 GET /v1/batches/{id}：归属过滤的聚合视图。
func (h *TasksHandler) handleBatchesGet(w http.ResponseWriter, r *http.Request) {
	requestID := newRelayRequestID()
	principal, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		writeRelayError(w, http.StatusUnauthorized, requestID, "", "invalid token")
		return
	}
	if h.batches == nil {
		writeRelayError(w, http.StatusServiceUnavailable, requestID, "database_error",
			"batch storage is not configured")
		return
	}
	batchID, ok := tasks.ParseBatchPublicID(r.PathValue("id"))
	if !ok {
		writeBatchNotFound(w, requestID)
		return
	}
	batch, err := h.batches.GetBatchByID(r.Context(), principal.UserID, batchID)
	if err != nil {
		if errors.Is(err, tasks.ErrBatchNotFound) {
			writeBatchNotFound(w, requestID)
			return
		}
		writeRelayError(w, http.StatusInternalServerError, requestID, "database_error", "failed to read the batch")
		return
	}
	writeJSON(w, http.StatusOK, batchToObject(batch, batch.Total))
}

// handleBatchesCancel 是 POST /v1/batches/{id}/cancel：当前 fail-closed 501。
//
// 为什么不能「就地取消」（R-13）：子任务在创建时已经逐个提交到上游，取消
// 需要插件的取消钩子（当前 manifest 没有该能力）。若只把本地任务置为
// failure/批次置 cancelled，会出现三处不一致：上游仍在生成并计费、本地预扣
// 被按失败退款（免费服务）、轮询器随后又把任务推进回 success。与其给出
// 「看起来成功但什么也没停」的 200，不如明确不支持，且该分支零计费。
//
// 归属校验先于 501：他人批次与不存在同为 404，不制造存在性 oracle。
func (h *TasksHandler) handleBatchesCancel(w http.ResponseWriter, r *http.Request) {
	requestID := newRelayRequestID()
	principal, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		writeRelayError(w, http.StatusUnauthorized, requestID, "", "invalid token")
		return
	}
	if h.batches == nil {
		writeRelayError(w, http.StatusServiceUnavailable, requestID, "database_error",
			"batch storage is not configured")
		return
	}
	batchID, ok := tasks.ParseBatchPublicID(r.PathValue("id"))
	if !ok {
		writeBatchNotFound(w, requestID)
		return
	}
	if _, err := h.batches.GetBatchByID(r.Context(), principal.UserID, batchID); err != nil {
		if errors.Is(err, tasks.ErrBatchNotFound) {
			writeBatchNotFound(w, requestID)
			return
		}
		writeRelayError(w, http.StatusInternalServerError, requestID, "database_error", "failed to read the batch")
		return
	}
	if requestID != "" {
		w.Header().Set("X-Request-ID", requestID)
	}
	writeProblemDetail(w, http.StatusNotImplemented, problemTypeEndpointNotSupported,
		"Batch cancellation is not supported",
		"tasks are submitted to upstream at batch creation and cannot be cancelled without a plugin cancel hook; this endpoint performs no billing and no state change")
}

// writeBatchNotFound 是批次端点唯一的「不存在」出口（文案固定、不区分原因）。
func writeBatchNotFound(w http.ResponseWriter, requestID string) {
	writeRelayError(w, http.StatusNotFound, requestID, "not_found", "batch not found")
}

// writeBatchUnsupported 输出与 Responses/compact 同一类型的 problem+json。
func writeBatchUnsupported(w http.ResponseWriter, requestID, title, detail string) {
	if requestID != "" {
		w.Header().Set("X-Request-ID", requestID)
	}
	writeProblemDetail(w, http.StatusNotImplemented, problemTypeEndpointNotSupported, title, detail)
}

// batchToObject 把仓储投影翻译成 OpenAI Batch 对象。
//
// total 取提交行数（含上游提交失败的子请求），而不是已关联任务数：
// OpenAI 的 request_counts.total 是请求数，失败的行必须体现在 failed 里。
func batchToObject(batch *tasks.Batch, total int) batchObject {
	if batch == nil {
		return batchObject{}
	}
	obj := batchObject{
		ID:               batch.ID,
		Object:           batchObjectType,
		Endpoint:         batchEndpointVideos,
		CompletionWindow: batchCompletionWindow,
		Status:           batch.Status,
		RequestCounts: batchRequestCounts{
			Total:     total,
			Completed: batch.Completed,
			Failed:    batch.Failed,
		},
		CreatedAt: batchCreatedAt(batch),
	}
	for _, item := range batch.Items {
		obj.Items = append(obj.Items, batchItemObject{
			TaskID:          item.PublicTaskID,
			State:           string(item.State),
			ProgressPercent: item.ProgressPercent,
		})
	}
	return obj
}

// batchCreatedAt 返回批次创建时间（Unix 秒）；零值回退 0，绝不用 now()。
func batchCreatedAt(batch *tasks.Batch) int64 {
	if batch.CreatedAt.IsZero() {
		return 0
	}
	return batch.CreatedAt.Unix()
}

// tokenIDOf 返回主令牌 id（会话路径没有令牌时为 0）。
func tokenIDOf(principal *auth.Principal) int64 {
	if principal == nil || principal.TokenID == nil {
		return 0
	}
	return *principal.TokenID
}
