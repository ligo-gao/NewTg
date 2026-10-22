// 系统任务管理端点（billing-06 §13.5 / modules-08 F-08-21 系统任务卡）。
//
//	GET  /api/system/tasks                  settings:manage（只读）
//	GET  /api/system/tasks/{name}           settings:manage（只读）
//	POST /api/system/tasks/{name}:trigger   settings:manage + Idempotency-Key
//
// 【为什么只有三个端点、没有 cancel】billing-06 §13.5 的契约冻结为
// 「列表 / 详情 / 触发」，取消执行不在契约内；新增 cancel 等于扩契约，且
// 与「租约 + 幂等」的单写者不变量冲突（执行者只能由心跳失败自行收敛）。
//
// 【为什么触发只入队】§13.5.2：触发写 pending 行 + 唤醒，由调度 pass 按
// 正常租约认领。旁路执行会绕过租约，多副本下直接造成双执行。
//
// 【安全口径】
//   - 任务名只接受静态注册表的精确匹配，绝不进入 SQL/命令/函数名构造；
//   - 非白名单任务服务端 403（前端隐藏按钮只是体验层，不构成授权）；
//   - 每个出口（202/400/403/404/409/500）都写 system.task.trigger 审计，
//     detail 只含 event/task_type/trigger/run_id/outcome，不含节点标识。
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"newtg/gateway/internal/api/dto"
	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/rbac"
	"newtg/gateway/internal/worker"
)

const (
	// systemTaskTriggerRoute 是审计里的模板路径；具体任务名在 detail.task_type。
	systemTaskTriggerRoute = "/api/system/tasks/{name}:trigger"

	problemTypeTaskNotFound       = "https://newtg.dev/problems/task_not_found"
	problemTypeTaskNotManual      = "https://newtg.dev/problems/task_not_manual"
	problemTypeTaskAlreadyRunning = "https://newtg.dev/problems/task_already_running"
	problemTypeTaskDisabled       = "https://newtg.dev/problems/task_disabled"
)

// 系统任务面的 RBAC 动作。触发是写动作，必须登记空字段策略（无请求体）。
var (
	systemTaskReadAction    = rbac.AuthzAction{ResourceKind: "system_task", ActionName: "read"}
	systemTaskTriggerAction = rbac.AuthzAction{ResourceKind: "system_task", ActionName: "trigger"}
)

// SystemTaskService 是管理面依赖的服务接缝（生产为 *worker.Service）。
type SystemTaskService interface {
	List(ctx context.Context, page, pageSize int) (worker.TaskPage, error)
	Detail(ctx context.Context, name string, page, pageSize int) (worker.TaskDetail, error)
	Trigger(ctx context.Context, name string) (worker.TriggerResult, error)
}

// SystemTaskHandlerConfig 是装配输入。
type SystemTaskHandlerConfig struct {
	Service SystemTaskService
	Authz   *rbac.AuthzMiddleware
	Audit   *audit.Logger
	Idem    IdempotencyStore
	Logger  *slog.Logger
}

// SystemTaskHandlers 持有依赖。
type SystemTaskHandlers struct {
	service SystemTaskService
	authz   *rbac.AuthzMiddleware
	audit   *audit.Logger
	idem    IdempotencyStore
	log     *slog.Logger
}

// NewSystemTaskHandlers 构造端点。Service 必填；Authz 为 nil 时回退 admin-only。
func NewSystemTaskHandlers(cfg SystemTaskHandlerConfig) *SystemTaskHandlers {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &SystemTaskHandlers{
		service: cfg.Service,
		authz:   cfg.Authz,
		audit:   cfg.Audit,
		idem:    cfg.Idem,
		log:     log,
	}
}

// Register 挂载到受保护管理子树。
func (h *SystemTaskHandlers) Register(mux *http.ServeMux) {
	if h == nil || h.service == nil {
		return
	}
	readGate := h.gate(systemTaskReadAction)
	triggerGate := h.gate(systemTaskTriggerAction)
	mux.Handle("GET /api/system/tasks", readGate(http.HandlerFunc(h.handleList)))
	mux.Handle("GET /api/system/tasks/{name}", readGate(http.HandlerFunc(h.handleDetail)))
	// ServeMux 的通配符必须占满整段，无法表达 `{name}:trigger`；这里按
	// `/api/system/tasks/{name}` 注册，再由 handler 校验 `:trigger` 后缀。
	// 缺后缀时与未知任务同口径 404，不暴露「路径写法不对」的额外信息。
	mux.Handle("POST /api/system/tasks/{name}", triggerGate(http.HandlerFunc(h.handleTrigger)))
}

func (h *SystemTaskHandlers) gate(action rbac.AuthzAction) func(http.Handler) http.Handler {
	if h.authz == nil {
		return requireAdminRole
	}
	// 触发无请求体：登记空字段策略，避免 RBAC 中间件按「策略缺失」失败关闭。
	h.authz.SetWritePolicy(systemTaskTriggerAction, &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{}})
	return func(next http.Handler) http.Handler {
		return h.authz.Handler(rbac.PermSettingsManage, action)(next)
	}
}

func (h *SystemTaskHandlers) handleList(w http.ResponseWriter, r *http.Request) {
	page, pageSize, ok := parseSystemTaskPage(w, r)
	if !ok {
		return
	}
	result, err := h.service.List(r.Context(), page, pageSize)
	if err != nil {
		h.log.Error("system tasks: list failed", "error", err)
		writeProblemDetail(w, http.StatusInternalServerError, problemTypeInternal,
			"Internal Server Error", "unable to read worker task state")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, result)
}

func (h *SystemTaskHandlers) handleDetail(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		writeProblemDetail(w, http.StatusNotFound, problemTypeTaskNotFound,
			"Task not found", "no worker task is registered with that name")
		return
	}
	page, pageSize, ok := parseSystemTaskPage(w, r)
	if !ok {
		return
	}
	detail, err := h.service.Detail(r.Context(), name, page, pageSize)
	switch {
	case errors.Is(err, worker.ErrTaskNotFound):
		writeProblemDetail(w, http.StatusNotFound, problemTypeTaskNotFound,
			"Task not found", "no worker task is registered with that name")
		return
	case err != nil:
		h.log.Error("system tasks: detail failed", "task_type", name, "error", err)
		writeProblemDetail(w, http.StatusInternalServerError, problemTypeInternal,
			"Internal Server Error", "unable to read worker task state")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, detail)
}

func (h *SystemTaskHandlers) handleTrigger(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimSpace(r.PathValue("name"))
	const suffix = ":trigger"
	name := ""
	if strings.HasSuffix(raw, suffix) {
		name = strings.TrimSuffix(raw, suffix)
	}
	if name == "" {
		// 未知/缺后缀统一 404，且不写 pending 行、不做任何字符串匹配。
		h.auditTrigger(r, name, nil, "not_found", "failed", http.StatusNotFound)
		writeProblemDetail(w, http.StatusNotFound, problemTypeTaskNotFound,
			"Task not found", "no worker task is registered with that name")
		return
	}

	headerKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if headerKey == "" {
		h.auditTrigger(r, name, nil, "invalid_request", "failed", http.StatusBadRequest)
		writeProblemDetail(w, http.StatusBadRequest, problemTypeValidationFailed,
			"Validation failed", "Idempotency-Key header is required for task triggers")
		return
	}

	// 幂等：与渠道/设置端点共用同一 Redis 存储；键作用域绑定用户 + 方法 +
	// 完整路径，避免跨任务/跨管理员重放他人结果。
	if h.idem == nil {
		h.performTrigger(w, r, name)
		return
	}
	key := idempotencyScopeKey(r, headerKey)
	claim, claimErr := h.idem.Claim(r.Context(), key, IdempotencyPendingTTL)
	if claimErr != nil {
		// fail-open：幂等存储不可用时仍允许触发；重复提交可能多写一条
		// pending 行，调度器按任务串行执行（同类型只有一把租约）。
		h.log.Warn("system tasks: idempotency store unavailable", "error", claimErr)
		h.performTrigger(w, r, name)
		return
	}
	switch claim.Status {
	case IdempotencyClaimReplay:
		w.Header().Set("Idempotent-Replayed", "true")
		w.Header().Set("Cache-Control", "no-store")
		writeJSONBytes(w, claim.Response.Status, claim.Response.Body)
		return
	case IdempotencyClaimInFlight:
		w.Header().Set("Retry-After", "1")
		h.auditTrigger(r, name, nil, "idempotency_in_progress", "failed", http.StatusConflict)
		writeProblemDetail(w, http.StatusConflict, problemTypeIdempotencyInProgress,
			"Idempotency key is in progress",
			"another request with this Idempotency-Key is still being processed; retry shortly")
		return
	}

	status, body := h.triggerResponse(r, name)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), idempotencyFinalizeTimeout)
	defer cancel()
	if status >= http.StatusOK && status < http.StatusMultipleChoices {
		if err := h.idem.Complete(ctx, key, claim.Token,
			IdempotentResponse{Status: status, Body: body}, IdempotencyTTL); err != nil {
			h.log.Error("system tasks: idempotency complete failed", "error", err)
		}
	} else if err := h.idem.Release(ctx, key, claim.Token); err != nil {
		h.log.Error("system tasks: idempotency release failed", "error", err)
	}
	// 202 与列表/详情同口径；全局中间件虽会对 /api/* 注入 no-store，这里
	// 显式声明，避免将来中间件调整时触发响应被缓存。
	w.Header().Set("Cache-Control", "no-store")
	writeJSONBytes(w, status, body)
}

// performTrigger 是无幂等层（idem=nil 或存储故障）的直接路径。
func (h *SystemTaskHandlers) performTrigger(w http.ResponseWriter, r *http.Request, name string) {
	status, body := h.triggerResponse(r, name)
	w.Header().Set("Cache-Control", "no-store")
	writeJSONBytes(w, status, body)
}

// triggerResponse 调用服务并映射错误，返回状态码与响应体（供幂等层固化）。
func (h *SystemTaskHandlers) triggerResponse(r *http.Request, name string) (int, []byte) {
	result, err := h.service.Trigger(r.Context(), name)
	switch {
	case err == nil:
		h.auditTrigger(r, name, &result.RunID, "accepted", "success", http.StatusAccepted)
		body, marshalErr := json.Marshal(result)
		if marshalErr != nil {
			h.log.Error("system tasks: marshal trigger response failed", "error", marshalErr)
			return http.StatusInternalServerError, taskProblem(http.StatusInternalServerError,
				problemTypeInternal, "Internal Server Error", "unable to encode trigger response")
		}
		return http.StatusAccepted, body
	case errors.Is(err, worker.ErrTaskNotFound):
		h.auditTrigger(r, name, nil, "not_found", "failed", http.StatusNotFound)
		return http.StatusNotFound, taskProblem(http.StatusNotFound, problemTypeTaskNotFound,
			"Task not found", "no worker task is registered with that name")
	case errors.Is(err, worker.ErrTaskManualNotAllowed):
		h.auditTrigger(r, name, nil, "not_manual", "failed", http.StatusForbidden)
		return http.StatusForbidden, taskProblem(http.StatusForbidden, problemTypeTaskNotManual,
			"Task cannot be triggered manually", "this task is not on the manual-trigger allowlist")
	case errors.Is(err, worker.ErrTaskAlreadyRunning):
		h.auditTrigger(r, name, nil, "already_running", "failed", http.StatusConflict)
		return http.StatusConflict, taskProblem(http.StatusConflict, problemTypeTaskAlreadyRunning,
			"Task is already running", "wait for the current run to finish before triggering again")
	case errors.Is(err, worker.ErrTaskDisabled):
		h.auditTrigger(r, name, nil, "disabled", "failed", http.StatusConflict)
		return http.StatusConflict, taskProblem(http.StatusConflict, problemTypeTaskDisabled,
			"Task is disabled", "enable the task before triggering it")
	default:
		// 运行时谓词阻断（如 log.partition_drop_enabled=false）：409 + 固定
		// 错误码，零 DROP、零 pending 行，审计 outcome=blocked。
		if code := worker.BlockedCodeOf(err); code != "" {
			h.auditTrigger(r, name, nil, "blocked", "failed", http.StatusConflict)
			return http.StatusConflict, taskProblem(http.StatusConflict,
				"https://newtg.dev/problems/"+code, "Task is blocked",
				"task is blocked by runtime configuration: "+code)
		}
		// 入队失败（DB/Redis）：记录固定分类，不回显内部错误文本。
		h.log.Error("system tasks: trigger failed", "task_type", name, "error", err)
		h.auditTrigger(r, name, nil, "enqueue_failed", "failed", http.StatusInternalServerError)
		return http.StatusInternalServerError, taskProblem(http.StatusInternalServerError,
			problemTypeInternal, "Internal Server Error", "unable to enqueue the task run")
	}
}

// problemBody 是幂等层需要的字节形 problem（与 writeProblemDetail 同形）。
func taskProblem(status int, problemType, title, detail string) []byte {
	problem := dto.Problem{Type: problemType, Title: title, Status: status}
	if detail != "" {
		problem.Detail = &detail
	}
	body, err := json.Marshal(problem)
	if err != nil {
		return []byte(`{"type":"https://newtg.dev/problems/internal_error","title":"Internal Server Error","status":500}`)
	}
	return body
}

// auditTrigger 写一次触发审计（best-effort；失败只记录，不影响响应）。
func (h *SystemTaskHandlers) auditTrigger(r *http.Request, name string, runID *int64, outcome, result string, statusCode int) {
	detail := map[string]any{
		"event":     "system.task.trigger",
		"task_type": name,
		"trigger":   worker.TriggerManual,
		"outcome":   outcome,
	}
	if runID != nil {
		detail["run_id"] = *runID
	}
	raw, err := json.Marshal(detail)
	if err != nil {
		h.log.Warn("system tasks: marshal audit detail failed", "error", err)
		return
	}
	event := audit.Event{
		Action:     audit.ActionSystemTaskTrigger,
		Actor:      actorID(r),
		Method:     r.Method,
		Route:      systemTaskTriggerRoute,
		StatusCode: statusCode,
		Result:     result,
		Detail:     raw,
	}
	if err := writeAuditEvent(r.Context(), h.audit, event); err != nil {
		h.log.Warn("system tasks: audit write failed", "task_type", name, "error", err)
	}
}

// parseSystemTaskPage 解析 p / page_size（默认 1 / 20，上限 100）。
func parseSystemTaskPage(w http.ResponseWriter, r *http.Request) (int, int, bool) {
	page := 1
	pageSize := 20
	query := r.URL.Query()
	if raw := strings.TrimSpace(query.Get("p")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 {
			writeProblemDetail(w, http.StatusBadRequest, problemTypeValidationFailed,
				"Validation failed", "p must be a positive integer")
			return 0, 0, false
		}
		page = value
	}
	if raw := strings.TrimSpace(query.Get("page_size")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 100 {
			writeProblemDetail(w, http.StatusBadRequest, problemTypeValidationFailed,
				"Validation failed", "page_size must be between 1 and 100")
			return 0, 0, false
		}
		pageSize = value
	}
	return page, pageSize, true
}
