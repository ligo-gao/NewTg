// Package httpapi 的任务数据面端点（M6d Task 6a）。
//
// 端点面（relay-06 §10.2 / §10.4）：
//
//	POST /v1/tasks/{key}                    提交任务（Bearer 令牌）
//	GET  /v1/tasks/{key}?task_id=           查询任务（Bearer 令牌）
//	GET  /v1/tasks/{key}/artifacts?task_id= 列产物（Bearer 令牌）
//	GET  /api/task/self                     本人任务台账（会话）
//	GET  /api/task                          管理员台账（logs:read）
//
// /v1 端点复用 RelayAuth 中间件（由 RelayHandler 注入同一个 authMiddleware），
// /api 端点挂在管理面受保护链上。计费只经 contract.BillingSession 接缝：
// 本文件不实现定价/结算，T6b 接管轮询与终态结算。
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/billing"
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/plugin"
	"newtg/gateway/internal/rbac"
	"newtg/gateway/internal/relay"
	"newtg/gateway/internal/relay/billingseam"
	"newtg/gateway/internal/tasks"
)

const (
	// maxTaskSubmitBytes 是任务提交请求体上限。任务请求通常只含 prompt/参数，
	// 4MiB 足以覆盖长提示与参考图 URL 列表；文件上传走后续任务，不在本任务。
	maxTaskSubmitBytes = 4 << 20

	// 台账分页默认值与 channels/usage-logs 同口径。
	tasksDefaultPageSize = 20
	tasksMaxPageSize     = 100

	// 管理面问题类型：任务台账的 403/500 用固定 type，便于前端按类别处理。
	problemTypeTasksForbidden = "https://newtg.dev/problems/forbidden"
	problemTypeTasksDatabase  = "https://newtg.dev/problems/database_error"
)

// TaskRepository 是任务数据面的存储接缝（生产 *tasks.Repo）。
type TaskRepository interface {
	Insert(ctx context.Context, t *tasks.Task) error
	GetByPublicID(ctx context.Context, userID int64, publicID, pluginKey string) (*tasks.Task, error)
	ListByUser(ctx context.Context, userID int64, f tasks.ListFilter) ([]tasks.Task, int, error)
	ListAdmin(ctx context.Context, f tasks.ListFilter) ([]tasks.Task, int, error)
}

var _ TaskRepository = (*tasks.Repo)(nil)

// TaskChannelSource 是按插件绑定选渠道的读取面（生产 *tasks.Repo）。
type TaskChannelSource interface {
	SelectChannel(ctx context.Context, pluginKey, model, group string) (*tasks.ChannelTarget, error)
}

var _ TaskChannelSource = (*tasks.Repo)(nil)

// TasksHandlerConfig 是任务端点的装配输入。
type TasksHandlerConfig struct {
	Store    TaskRepository
	Channels TaskChannelSource
	Plugins  *plugin.Manager
	// Sealer 解开渠道凭据（GC-8）。必填：没有它就只能拒绝提交，绝不回落明文。
	Sealer *SecretSealer
	// Tokens 是 relay.BuildRelayContext 回源令牌分组的接缝（Bearer 路径）。
	Tokens auth.TokenLookup
	// Billing 为每次提交构造计费会话；nil 时使用 billingseam.NewStub（测试）。
	Billing func(*contract.RelayContext) contract.BillingSession
	// Client 是出站客户端；nil 时构造禁重定向的默认客户端。
	Client *http.Client
	// AllowPrivateUpstream 只允许测试注入 httptest（127.0.0.1）时置 true。
	// 生产装配恒为 false；它只跳过字面量私网地址检查，不关闭白名单/重定向/大小限制。
	AllowPrivateUpstream bool
}

// TasksHandler 持有任务端点依赖，可并发使用。
type TasksHandler struct {
	store        TaskRepository
	channels     TaskChannelSource
	plugins      *plugin.Manager
	sealer       *SecretSealer
	tokens       auth.TokenLookup
	billing      func(*contract.RelayContext) contract.BillingSession
	client       *http.Client
	allowPrivate bool
}

// NewTasksHandler 构造 handler。Store/Channels/Plugins/Sealer 是硬依赖：
// 缺任一都不存在「安全降级」，只会把配置错误伪装成用户可见的失败。
func NewTasksHandler(cfg TasksHandlerConfig) *TasksHandler {
	if cfg.Store == nil || cfg.Channels == nil || cfg.Plugins == nil || cfg.Sealer == nil {
		panic("httpapi: NewTasksHandler requires store, channel source, plugin manager and sealer")
	}
	billing := cfg.Billing
	if billing == nil {
		billing = func(*contract.RelayContext) contract.BillingSession { return billingseam.NewStub() }
	}
	client := cfg.Client
	if client == nil {
		client = tasks.NewOutboundClient()
	}
	return &TasksHandler{
		store:        cfg.Store,
		channels:     cfg.Channels,
		plugins:      cfg.Plugins,
		sealer:       cfg.Sealer,
		tokens:       cfg.Tokens,
		billing:      billing,
		client:       client,
		allowPrivate: cfg.AllowPrivateUpstream,
	}
}

// RegisterRelay 把 /v1/tasks* 挂到 mux，复用中继的 Bearer 鉴权链。
func (h *TasksHandler) RegisterRelay(mux *http.ServeMux, authMiddleware func(http.Handler) http.Handler) {
	mux.Handle("POST /v1/tasks/{key}", authMiddleware(http.HandlerFunc(h.handleSubmit)))
	mux.Handle("GET /v1/tasks/{key}", authMiddleware(http.HandlerFunc(h.handleGet)))
	mux.Handle("GET /v1/tasks/{key}/artifacts", authMiddleware(http.HandlerFunc(h.handleArtifacts)))
}

// RegisterAdmin 把台账端点挂到管理面受保护链。
func (h *TasksHandler) RegisterAdmin(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/task/self", h.handleSelfList)
	mux.HandleFunc("GET /api/task", h.handleAdminList)
}

// ---------- /v1：提交 ----------

type taskSubmitRequest struct {
	Model string `json:"model"`
}

func (h *TasksHandler) handleSubmit(w http.ResponseWriter, r *http.Request) {
	requestID := newRelayRequestID()
	principal, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		writeRelayError(w, http.StatusUnauthorized, requestID, "", "invalid token")
		return
	}
	key := strings.TrimSpace(r.PathValue("key"))
	if key == "" || len(key) > plugin.MaxKeyLen {
		writeRelayError(w, http.StatusNotFound, requestID, "model_not_found", "unknown task plugin")
		return
	}

	// 插件准入（brief 步骤 2）：只接受 registered / disabled_fallback。
	// 未注册与已禁用用不同状态码，让调用方能区分「没有这个插件」与
	// 「插件被管理员关闭」；两者都不产生任何计费或持久化副作用。
	snapshot := h.plugins.Snapshot()
	entry := snapshot.Lookup(key)
	if entry.Plugin == nil {
		if entry.Status == plugin.StatusNotRegistered {
			writeRelayError(w, http.StatusNotFound, requestID, "model_not_found", "unknown task plugin")
			return
		}
		writeRelayError(w, http.StatusConflict, requestID, "plugin_unavailable", "task plugin is not available")
		return
	}
	compiled := entry.Plugin

	// 请求体：有界读取 + JSON 对象。model 必填且必须是非空字符串。
	r.Body = http.MaxBytesReader(w, r.Body, maxTaskSubmitBytes)
	var body map[string]any
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&body); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeRelayError(w, http.StatusRequestEntityTooLarge, requestID, "payload_too_large", "request body is too large")
			return
		}
		writeRelayError(w, http.StatusBadRequest, requestID, "invalid_request_error", "request body must be a JSON object")
		return
	}
	model := strings.TrimSpace(toStringValue(body["model"]))
	if model == "" {
		writeRelayError(w, http.StatusBadRequest, requestID, "invalid_request_error", "model is required")
		return
	}
	// action 是族内动作标识（解释权归插件）；列宽 40，超长必须在落库前拒绝，
	// 否则会变成 500 的 CHECK 违约而不是可读的 400。
	action := strings.TrimSpace(toStringValue(body["action"]))
	if len(action) > 40 {
		writeRelayError(w, http.StatusBadRequest, requestID, "invalid_request_error", "action is too long")
		return
	}
	if !tokenModelAllowed(principal.ModelLimits, model) {
		writeRelayError(w, http.StatusForbidden, requestID, "distributor_token_model_forbidden",
			"this token is not allowed to access the requested model")
		return
	}
	taskDuration, ok := taskDurationSeconds(body["seconds"])
	if !ok {
		writeRelayError(w, http.StatusBadRequest, requestID, "invalid_request_error",
			"seconds must be a positive number no greater than the task duration limit")
		return
	}

	// 复用中继上下文组装：RequestID / UserID / TokenID / 分组来自同一条
	// 鉴权链，pin 的「非管理员拒绝」也在这里执行（R-P2a）。失败一律 401：
	// 不区分存储故障与坏令牌，避免成为令牌探测信道。
	rc, err := relay.BuildRelayContext(r.Context(), principal, r, h.tokens)
	if err != nil {
		writeRelayError(w, http.StatusUnauthorized, requestID, "", "invalid token")
		return
	}
	// 计费幂等键与响应里的 requestId 对齐：客户端报障时同一个 ID 能同时定位
	// 失败响应与计费会话（BuildRelayContext 生成的是另一个随机值）。
	if requestID != "" {
		rc.RequestID = requestID
	}
	relay.WithRequest(rc, &contract.Request{Model: model}, "openai", r.URL.Path)

	// 渠道候选（brief 步骤 3）：status=1 + 绑定插件 key + 支持模型，
	// 按 priority/weight 取一个。无候选 → 404，发生在任何计费之前。
	channel, err := h.channels.SelectChannel(r.Context(), key, model, rc.UsingGroup)
	if err != nil {
		if errors.Is(err, tasks.ErrChannelNotFound) {
			writeRelayError(w, http.StatusNotFound, requestID, "model_not_found",
				"no task channel is available for the requested model")
			return
		}
		writeRelayError(w, http.StatusInternalServerError, requestID, "database_error",
			"failed to select a task channel")
		return
	}

	// 渠道凭据解密（GC-8）：明文只活在本次请求的内存里，绝不落库/日志/响应。
	apiKey, err := h.decryptChannelCredential(channel.Credentials)
	if err != nil {
		writeRelayError(w, http.StatusInternalServerError, requestID, "channel_credential_invalid",
			"task channel credential is not usable")
		return
	}
	if strings.TrimSpace(channel.BaseURL) == "" {
		// 基址缺失是渠道配置错误，不是用户参数错误；不退回插件 meta.baseUrl——
		// 那会让「渠道配置漏填」静默走一个可能不受管理员控制的地址。
		writeRelayError(w, http.StatusInternalServerError, requestID, "channel_misconfigured",
			"task channel base url is not configured")
		return
	}

	// 计费预扣（brief 步骤 5）：估算进入 RelayContext，PreConsume 失败即 402
	// 且不落任务行。定价/快照解析由计费引擎负责，本端点不读价格字段。
	estimate := &contract.QuotaEstimate{
		Model:        model,
		Group:        rc.UsingGroup,
		TaskDuration: taskDuration,
	}
	relay.WithQuotaEstimate(rc, estimate)
	session := h.billing(rc)
	if session == nil {
		session = billingseam.NewStub()
	}
	if err := session.PreConsume(*estimate); err != nil {
		// M1：定价键缺失/表达式错误不是余额不足。只有计费引擎明确判定额度
		// 不足才映射 402；其余价格/配置错误映射 400 model_price_error，避免
		// 客户端把「管理员没配价」误读成「充值就能用」。
		if errors.Is(err, billing.ErrInsufficientQuota) {
			writeRelayError(w, http.StatusPaymentRequired, requestID, "insufficient_quota",
				"insufficient quota for this task")
			return
		}
		writeRelayError(w, http.StatusBadRequest, requestID, "model_price_error",
			"task pricing is not available for the requested model")
		return
	}
	committed := false
	// settled 表示资金已按本请求的终态收敛（退款或结算成功）。它只用于
	// defer 的「是否还需要退款」，与任务的 state 不是同一概念。
	settled := false
	defer func() {
		if !committed && !settled {
			// 预扣成功后的任何失败都必须全额退款；Refund 幂等（契约 §6.1）。
			if err := session.Refund(); err != nil {
				slog.Warn("tasks: refund after failed submit did not complete",
					"request_id", rc.RequestID, "error", err)
			}
		}
	}()

	// 出站提交（brief 步骤 6/7）：插件构建 → 宿主连接 → 插件解析。
	result, err := tasks.Submit(r.Context(), h.client, tasks.SubmitInput{
		Plugin:       compiled,
		Channel:      *channel,
		Model:        model,
		Body:         body,
		BaseURL:      channel.BaseURL,
		APIKey:       apiKey,
		AllowPrivate: h.allowPrivate,
	})
	if err != nil {
		// 提交失败不落任务行（brief 步骤 8 的冻结选择）：没有 upstream_task_id
		// 的行既不能轮询也不能结算，只会成为孤儿。失败事实由 usage_logs 与
		// 退款流水承载。
		writeRelayError(w, http.StatusBadGateway, requestID, "upstream_error",
			"task submission failed upstream")
		return
	}

	publicID, err := tasks.NewPublicTaskID()
	if err != nil {
		writeRelayError(w, http.StatusInternalServerError, requestID, "internal_error",
			"failed to allocate a task id")
		return
	}

	// 双屏障的「向上差额预留」：当前没有已定价的目标额度，预留增量为 0
	//（幂等 no-op），持久化的 quota 就是本次预扣量。带定价的差额上浮归
	// T6b；**错误不能吞**：非零预留适配后这里的失败必须可见（M4）。
	if err := session.Reserve(0); err != nil {
		slog.Warn("tasks: reserve after submit failed", "request_id", rc.RequestID, "error", err)
		writeRelayError(w, http.StatusInternalServerError, requestID, "billing_error",
			"failed to reserve quota for the task")
		return
	}
	quota := session.Reserved()

	// 即时终态的资金收敛（HIGH-2）：
	//   - FAILURE：设计 §9.5「即时失败 settle 为 0」→ 全额退款，quota 落 0；
	//   - SUCCESS 且已有 usage：立即 Terminate 结算；
	//   - SUCCESS 无 usage：差额不可信，保留预扣，由 T6b 终态结算。
	refundPending := false
	settlementPending := false
	switch {
	case result.State == tasks.StateFailure:
		if err := session.Refund(); err != nil {
			// 退款失败不能伪装成已退：持久化 refund_pending 供补偿扫描发现
			//（M3），并保留预扣金额，避免账实不符。
			refundPending = true
			slog.Warn("tasks: refund for immediate failure did not complete",
				"request_id", rc.RequestID, "error", err)
		} else {
			settled = true
			quota = 0
		}
	case result.State == tasks.StateSuccess && hasTaskUsage(result.Data):
		if err := session.Terminate(contract.UsageFact{
			Model: model,
			Usage: contract.Usage{Source: contract.UsageEstimated},
		}); err != nil {
			settlementPending = true
			slog.Warn("tasks: terminate for immediate success did not complete",
				"request_id", rc.RequestID, "error", err)
		} else {
			settled = true
		}
	}

	now := time.Now().UTC()
	fetchMode := "poll"
	if result.Immediate {
		// 即时终态不再需要轮询取回；fetch_mode 记录事实而不是默认值（LOW）。
		fetchMode = "inline"
	}
	task := &tasks.Task{
		PublicTaskID:     publicID,
		UpstreamTaskID:   result.UpstreamTaskID,
		Family:           "plugin",
		Action:           action,
		PluginKey:        key,
		PluginGeneration: tasks.PluginGeneration(compiled),
		FetchMode:        fetchMode,
		LockedChannelID:  0,
		OriginTaskIDs:    []string{},
		State:            result.State,
		RawState:         result.RawState,
		ProgressPercent:  result.Progress,
		UserID:           principal.UserID,
		Group:            rc.UsingGroup,
		ChannelID:        channel.ID,
		Quota:            quota,
		FailReason:       "",
		SubmitTime:       &now,
		Properties: map[string]any{
			"OriginModelName":   model,
			"UpstreamModelName": upstreamModelName(channel, model),
			"Input":             body,
		},
		PrivateData: map[string]any{
			// 执行溯源（设计 §7）+ 资金来源。request_id 是 T6b 在进程外
			// 结算/退款的唯一锚点；凭据/明文 key 绝不进入此列。
			"request_id":     rc.RequestID,
			"request_path":   rc.RequestPath,
			"billing_source": "wallet",
			"plugin": map[string]any{
				"key":         key,
				"name":        compiled.Manifest().Name,
				"version":     entry.Version,
				"api_version": compiled.Manifest().APIVersion,
				"generation":  tasks.PluginGeneration(compiled),
			},
		},
		Data: result.Data,
	}
	if principal.TokenID != nil {
		task.PrivateData["token_id"] = *principal.TokenID
	}
	if refundPending {
		task.PrivateData["refund_pending"] = true
	}
	if settlementPending {
		task.PrivateData["settlement_pending"] = true
	}
	if result.Immediate {
		// 即时终态：不再轮询，定格完成时间。
		task.FinishTime = &now
	}

	if err := h.store.Insert(r.Context(), task); err != nil {
		writeRelayError(w, http.StatusInternalServerError, requestID, "internal_error",
			"failed to persist the task")
		return
	}
	committed = true

	// 异步交接（HIGH-1）：任务行落库成功后，凡仍有预扣 hold 的路径都必须
	// 停止心跳并移除在飞租约，否则 goroutine 泄漏、worker 永远看不到悬挂，
	// 重启后还会被 RecoverAbandoned 当成废弃请求全额退款（免费服务）。
	// 已 refund/terminate 的路径没有 hold，无需交接。
	if !settled {
		if handoff, ok := session.(contract.BillingHandoff); ok {
			if err := handoff.Detach(rc.RequestID); err != nil {
				// 任务行已落库，无法回滚；心跳未停会让 worker 延迟兜底（安全方向）。
				// 记录告警供运维发现，任务行本身仍有 request_id 可人工收敛。
				slog.Warn("tasks: billing detach failed after task insert",
					"request_id", rc.RequestID, "error", err)
			}
		}
	}

	writeJSON(w, http.StatusAccepted, taskSubmitResponse{
		PublicTaskID:   task.PublicTaskID,
		State:          task.State,
		UpstreamTaskID: task.UpstreamTaskID,
	})
}

type taskSubmitResponse struct {
	PublicTaskID   string      `json:"public_task_id"`
	State          tasks.State `json:"state"`
	UpstreamTaskID string      `json:"upstream_task_id,omitempty"`
}

// ---------- /v1：查询 / 产物 ----------

type taskQueryResponse struct {
	TaskID          string               `json:"task_id"`
	State           tasks.State          `json:"state"`
	RawState        string               `json:"raw_state"`
	ProgressPercent int                  `json:"progress_percent"`
	Usage           map[string]any       `json:"usage,omitempty"`
	Artifacts       []tasks.ArtifactView `json:"artifacts,omitempty"`
	FailReason      string               `json:"fail_reason,omitempty"`
}

func (h *TasksHandler) handleGet(w http.ResponseWriter, r *http.Request) {
	requestID := newRelayRequestID()
	task, key, ok := h.lookupOwnedTask(w, r, requestID)
	if !ok {
		return
	}
	resp := taskQueryResponse{
		TaskID:          task.PublicTaskID,
		State:           task.State,
		RawState:        task.RawState,
		ProgressPercent: task.ProgressPercent,
		Artifacts:       tasks.ProjectArtifacts(task.Data),
		FailReason:      task.FailReason,
	}
	// usage 只投影插件声明字段；插件已被卸载时连 schema 都不可信，宁可不返回。
	if compiled, ok := h.plugins.Snapshot().Plugin(key); ok {
		resp.Usage = tasks.ProjectUsage(task.Data, tasks.DeclaredUsageFields(compiled.Manifest()))
	}
	writeJSON(w, http.StatusOK, resp)
}

type artifactsResponse struct {
	Items []tasks.ArtifactView `json:"items"`
}

func (h *TasksHandler) handleArtifacts(w http.ResponseWriter, r *http.Request) {
	requestID := newRelayRequestID()
	task, _, ok := h.lookupOwnedTask(w, r, requestID)
	if !ok {
		return
	}
	items := tasks.ProjectArtifacts(task.Data)
	if items == nil {
		items = []tasks.ArtifactView{}
	}
	writeJSON(w, http.StatusOK, artifactsResponse{Items: items})
}

// lookupOwnedTask 完成查询的公共前置：鉴权身份、task_id 参数、归属读取。
//
// 不存在 / 不属于本人 / plugin_key 不匹配 一律 404：任何更细的区分都会
// 让 public_task_id 变成跨用户探测信道。
func (h *TasksHandler) lookupOwnedTask(w http.ResponseWriter, r *http.Request, requestID string) (*tasks.Task, string, bool) {
	principal, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		writeRelayError(w, http.StatusUnauthorized, requestID, "", "invalid token")
		return nil, "", false
	}
	key := strings.TrimSpace(r.PathValue("key"))
	publicID := strings.TrimSpace(r.URL.Query().Get("task_id"))
	if key == "" || publicID == "" {
		writeRelayError(w, http.StatusBadRequest, requestID, "invalid_request_error", "task_id is required")
		return nil, "", false
	}
	task, err := h.store.GetByPublicID(r.Context(), principal.UserID, publicID, key)
	if err != nil {
		if errors.Is(err, tasks.ErrTaskNotFound) {
			writeRelayError(w, http.StatusNotFound, requestID, "task_not_found", "task not found")
			return nil, "", false
		}
		writeRelayError(w, http.StatusInternalServerError, requestID, "database_error", "failed to read the task")
		return nil, "", false
	}
	return task, key, true
}

// ---------- /api：台账 ----------

type taskListResponse struct {
	Items    []tasks.Task `json:"items"`
	Total    int          `json:"total"`
	Page     int          `json:"page"`
	PageSize int          `json:"page_size"`
}

func (h *TasksHandler) handleSelfList(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, problemTypeUnauthorized, "unauthorized")
		return
	}
	page, pageSize := tasksPageParams(r)
	states, ok := tasks.NormalizeStateFilter(r.URL.Query().Get("state"))
	if !ok {
		writeProblemDetail(w, http.StatusBadRequest, problemTypeInvalidRequest, "invalid_request", "unknown state filter")
		return
	}
	items, total, err := h.store.ListByUser(r.Context(), principal.UserID, tasks.ListFilter{
		States: states, Page: page, PageSize: pageSize,
	})
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, problemTypeTasksDatabase, "database_error")
		return
	}
	// self 台账不暴露 user_id（本人恒等）；列表不含 data / private_data。
	for i := range items {
		items[i].UserID = 0
	}
	writeJSON(w, http.StatusOK, taskListResponse{Items: items, Total: total, Page: page, PageSize: pageSize})
}

func (h *TasksHandler) handleAdminList(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, problemTypeUnauthorized, "unauthorized")
		return
	}
	// 管理员可见性沿用 logs:read 口径：role=admin 且持有 logs:read（或 FULL）。
	// 缺权限返回 403，而不是「静默收窄成本人」——管理端的过滤参数与权限
	// 语义必须一致，收窄会掩盖越权尝试。
	if principal.Role != rbac.RoleAdmin || !rbac.HasPermission(*principal, rbac.PermLogsRead) {
		writeProblem(w, http.StatusForbidden, problemTypeTasksForbidden, "permission_denied")
		return
	}
	page, pageSize := tasksPageParams(r)
	states, ok := tasks.NormalizeStateFilter(r.URL.Query().Get("state"))
	if !ok {
		writeProblemDetail(w, http.StatusBadRequest, problemTypeInvalidRequest, "invalid_request", "unknown state filter")
		return
	}
	var userID int64
	if raw := strings.TrimSpace(r.URL.Query().Get("user_id")); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed <= 0 {
			writeProblemDetail(w, http.StatusBadRequest, problemTypeInvalidRequest, "invalid_request", "user_id must be a positive integer")
			return
		}
		userID = parsed
	}
	items, total, err := h.store.ListAdmin(r.Context(), tasks.ListFilter{
		UserID: userID, States: states, Page: page, PageSize: pageSize,
	})
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, problemTypeTasksDatabase, "database_error")
		return
	}
	writeJSON(w, http.StatusOK, taskListResponse{Items: items, Total: total, Page: page, PageSize: pageSize})
}

// ---------- 工具 ----------

// decryptChannelCredential 解开 channels.credentials 并取出 api_key。
// 空密文表示渠道未配置凭据：允许（auth=none 的插件存在），返回空串。
func (h *TasksHandler) decryptChannelCredential(sealed []byte) (string, error) {
	if len(sealed) == 0 {
		return "", nil
	}
	plain, err := h.sealer.Open(sealed)
	if err != nil {
		return "", err
	}
	var creds ChannelCredentials
	if err := json.Unmarshal(plain, &creds); err != nil {
		return "", err
	}
	return creds.APIKey, nil
}

// upstreamModelName 返回渠道 model_mapping 映射后的模型名（缺省回退原名）。
func upstreamModelName(ch *tasks.ChannelTarget, model string) string {
	if ch != nil {
		if mapped, ok := ch.ModelMapping[model]; ok && mapped != "" {
			return mapped
		}
	}
	return model
}

// hasTaskUsage 报告提交结果里是否已有可用量事实。
//
// 只有「即时 SUCCESS 且带 usage」才可安全地立即 Terminate；否则差额不可信，
// 保留预扣交给 T6b（设计 §9.4 的保守分支）。
func hasTaskUsage(data map[string]any) bool {
	usage, ok := data["usage"].(map[string]any)
	return ok && len(usage) > 0
}

// taskDurationSeconds 读取请求体里的 seconds（任务时长）。
//
// 上界用契约的 MaxRequestTaskDurationSeconds（3600s）：任何会成为计费乘数的
// 用户可控量在进入计费前必须带上界（.agents/rules/billing.md）。缺失返回 0
// （表示无时长维度）；存在但不是正数或越界则返回 ok=false，由调用方 400。
func taskDurationSeconds(v any) (int, bool) {
	if v == nil {
		return 0, true
	}
	var num float64
	switch typed := v.(type) {
	case float64:
		num = typed
	case int:
		num = float64(typed)
	case json.Number:
		parsed, err := typed.Float64()
		if err != nil {
			return 0, false
		}
		num = parsed
	default:
		return 0, false
	}
	if num <= 0 || num > contract.MaxRequestTaskDurationSeconds {
		return 0, false
	}
	if num != float64(int(num)) {
		return 0, false
	}
	return int(num), true
}

func tasksPageParams(r *http.Request) (int, int) {
	page, _ := strconv.Atoi(r.URL.Query().Get("p"))
	pageSize, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = tasksDefaultPageSize
	}
	if pageSize > tasksMaxPageSize {
		pageSize = tasksMaxPageSize
	}
	return page, pageSize
}

// toStringValue 把 JSON 标量转成字符串（model 等必填字段用）。
func toStringValue(v any) string {
	switch typed := v.(type) {
	case string:
		return typed
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	default:
		return ""
	}
}
