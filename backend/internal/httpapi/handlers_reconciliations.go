// 上游用量对账：运行 / 明细 / 概览端点（上游用量对账设计 §4.1、§4.5；plan Task 9）。
//
// 端点面（全部要求 `settings:manage`）：
//
//	GET  /api/upstream-reconciliations/runs                   offset 分页（started_at 降序）
//	GET  /api/upstream-reconciliations/runs/{id}/items        offset 分页 + severity/status 筛选
//	GET  /api/upstream-reconciliations/summary?run_id=…       按天汇总（run 必填）
//	POST /api/upstream-reconciliations/run                    手动同步触发（60s 超时）
//	POST /api/upstream-reconciliations/items/{id}/ack         标记已确认 / 已解决
//
// 与账号端点（handlers_upstream_accounts.go）保持同一构造/鉴权/审计形状：
//   - 每个写动作登记独立的 rbac.FieldPolicy，未登记字段在鉴权层直接 403；
//   - 业务校验在仓储调用之前完成，错误用 RFC 7807 problem+json；
//   - 一次手动触发只写一条 `reconcile.run` 审计，ack 写
//     `reconcile.item_ack`（含 before/after）。
//
// runner 以窄接口注入：internal/platform 依赖本包，因此本包绝不能导入
// platform，由装配层把 *reconcile.Runner 适配进来。
package httpapi

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"newtg/gateway/internal/api/dto"
	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/rbac"
	"newtg/gateway/internal/reconcile"
	"newtg/gateway/internal/store"
)

const (
	// reconcileRunTimeout 是手动触发的同步执行上限（契约：超时 504）。
	reconcileRunTimeout = 60 * time.Second

	// reconcileAckNoteMaxRunes 与契约 ReconcileAckRequest.note 的 maxLength
	// 一致。DDL 的 note 是 text（无列级上限），2000 的边界只能在这里执行。
	reconcileAckNoteMaxRunes = 2000

	// 手动触发固定写入的 triggered_by 值（迁移 CHECK 的闭集）。
	reconcileTriggerManual = "manual"

	// runner Outcome.Status 的两个特殊值：skipped 表示没有可处理账号，
	// disabled 只可能由 worker 触发产生（手动触发不读 reconcile.enabled）。
	reconcileOutcomeSkipped  = "skipped"
	reconcileOutcomeDisabled = "disabled"
)

// ReconcileStore 是运行/明细/概览端点的存储接缝；*store.ReconcileRepo 满足。
type ReconcileStore interface {
	ListRuns(ctx context.Context, accountID *int64, page, pageSize int) ([]store.ReconcileRun, int64, error)
	ListItems(ctx context.Context, runID int64, severity, status string, page, pageSize int) ([]store.ReconcileItem, int64, error)
	Summary(ctx context.Context, runID int64) ([]store.SummaryDay, error)
	GetRun(ctx context.Context, id int64) (*store.ReconcileRun, error)
	GetItem(ctx context.Context, id int64) (*store.ReconcileItem, error)
	AckItem(ctx context.Context, id int64, status, note string) error
}

// ReconcileRunner 是手动触发对账的执行缝；*reconcile.Runner 满足。
//
// 本包不导入 platform（platform 依赖本包），生产由 platform 注入真实 runner；
// nil 表示未装配，触发端点返回 503，读端点照常挂载。
type ReconcileRunner interface {
	Run(ctx context.Context, trigger string, accountIDs []int64) (reconcile.Outcome, error)
}

// ReconcileHandlerConfig 是运行的装配输入。Repo 与 DB 二选一。
// Audit 为 nil 时不写审计（未装配日志库的测试/开发环境）。
type ReconcileHandlerConfig struct {
	DB     *sql.DB
	Repo   ReconcileStore
	Runner ReconcileRunner
	Authz  *rbac.AuthzMiddleware
	Audit  *audit.Logger
	Logger *slog.Logger
	// Now 是 summary.generated_at 的时钟接缝；nil 时使用 time.Now。
	Now func() time.Time
}

// ReconcileHandlers 持有运行/明细/概览端点依赖，可并发使用。
type ReconcileHandlers struct {
	repo   ReconcileStore
	runner ReconcileRunner
	authz  *rbac.AuthzMiddleware
	audit  *audit.Logger
	log    *slog.Logger
	now    func() time.Time
}

var (
	reconcileRunListAction  = rbac.AuthzAction{ResourceKind: "reconcile_run", ActionName: "list"}
	reconcileItemListAction = rbac.AuthzAction{ResourceKind: "reconcile_item", ActionName: "list"}
	reconcileSummaryAction  = rbac.AuthzAction{ResourceKind: "reconcile_run", ActionName: "summary"}
	reconcileTriggerAction  = rbac.AuthzAction{ResourceKind: "reconcile_run", ActionName: "run"}
	reconcileAckAction      = rbac.AuthzAction{ResourceKind: "reconcile_item", ActionName: "ack"}
)

// NewReconcileHandlers 构造运行/明细/概览端点。
func NewReconcileHandlers(cfg ReconcileHandlerConfig) (*ReconcileHandlers, error) {
	repo := cfg.Repo
	if repo == nil {
		if cfg.DB == nil {
			return nil, errors.New("httpapi: ReconcileHandlerConfig.Repo or DB is required")
		}
		repo = store.NewReconcileRepo(cfg.DB)
	}
	authz := cfg.Authz
	if authz == nil {
		authz = rbac.NewAuthzMiddleware()
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	h := &ReconcileHandlers{
		repo: repo, runner: cfg.Runner, authz: authz, audit: cfg.Audit, log: log, now: now,
	}
	h.registerPolicies()
	return h, nil
}

func (h *ReconcileHandlers) registerPolicies() {
	// POST /run 的唯一合法字段是 account_id；{} 表示遍历全部启用账号。
	h.authz.SetWritePolicy(reconcileTriggerAction, &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{
		"account_id": nil,
	}})
	// ack 的 status 缺省 acknowledged，note 可空串但字段存在。
	h.authz.SetWritePolicy(reconcileAckAction, &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{
		"status": nil,
		"note":   nil,
	}})
}

// RegisterRoutes 注册运行/明细/概览端点。wrap 语义同 RegisterGroupRoutes。
func (h *ReconcileHandlers) RegisterRoutes(mux *http.ServeMux, wrap func(http.Handler) http.Handler) {
	mount := func(pattern string, handler http.Handler) {
		if wrap != nil {
			handler = wrap(handler)
		}
		mux.Handle(pattern, handler)
	}
	mount("GET /api/upstream-reconciliations/runs", h.authz.Handler(rbac.PermSettingsManage, reconcileRunListAction)(
		http.HandlerFunc(h.handleListRuns)))
	mount("GET /api/upstream-reconciliations/runs/{id}/items", h.authz.Handler(rbac.PermSettingsManage, reconcileItemListAction)(
		http.HandlerFunc(h.handleListItems)))
	mount("GET /api/upstream-reconciliations/summary", h.authz.Handler(rbac.PermSettingsManage, reconcileSummaryAction)(
		http.HandlerFunc(h.handleSummary)))
	mount("POST /api/upstream-reconciliations/run", h.authz.Handler(rbac.PermSettingsManage, reconcileTriggerAction)(
		http.HandlerFunc(h.handleTriggerRun)))
	mount("POST /api/upstream-reconciliations/items/{id}/ack", h.authz.Handler(rbac.PermSettingsManage, reconcileAckAction)(
		http.HandlerFunc(h.handleAckItem)))
}

// ---------------------------------------------------------------------------
// handlers
// ---------------------------------------------------------------------------

func (h *ReconcileHandlers) handleListRuns(w http.ResponseWriter, r *http.Request) {
	var accountID *int64
	if raw := strings.TrimSpace(r.URL.Query().Get("account_id")); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
				"account_id must be a positive integer")
			return
		}
		accountID = &id
	}

	page, pageSize := pageParams(r.URL.Query())
	runs, total, err := h.repo.ListRuns(r.Context(), accountID, page, pageSize)
	if err != nil {
		h.writeRepoError(w, "list reconciliation runs", err)
		return
	}
	views := make([]dto.ReconcileRun, 0, len(runs))
	for _, run := range runs {
		views = append(views, reconcileRunDTO(run))
	}
	writeJSON(w, http.StatusOK, dto.ReconcileRunList{
		Items: views, Total: int(total), Page: page, PageSize: pageSize,
	})
}

func (h *ReconcileHandlers) handleListItems(w http.ResponseWriter, r *http.Request) {
	runID, ok := reconcilePathID(w, r)
	if !ok {
		return
	}
	// 明细按 run_id 过滤：不存在的 run 与「存在但没有明细」在列表上同形，
	// 必须先证明 run 存在，才能按契约返回 404。
	if _, ok := h.requireRun(w, r, runID); !ok {
		return
	}

	severity := strings.TrimSpace(r.URL.Query().Get("severity"))
	if severity != "" && !dto.ReconcileItemSeverity(severity).Valid() {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"severity is not a known reconciliation severity")
		return
	}
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	if status != "" && !dto.ReconcileItemStatus(status).Valid() {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"status is not a known reconciliation item status")
		return
	}

	page, pageSize := pageParams(r.URL.Query())
	items, total, err := h.repo.ListItems(r.Context(), runID, severity, status, page, pageSize)
	if err != nil {
		h.writeRepoError(w, "list reconciliation items", err)
		return
	}
	views := make([]dto.ReconcileItem, 0, len(items))
	for _, item := range items {
		views = append(views, reconcileItemDTO(item))
	}
	writeJSON(w, http.StatusOK, dto.ReconcileItemList{
		Items: views, Total: int(total), Page: page, PageSize: pageSize,
	})
}

func (h *ReconcileHandlers) handleSummary(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimSpace(r.URL.Query().Get("run_id"))
	if raw == "" {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"run_id is required")
		return
	}
	runID, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || runID <= 0 {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"run_id must be a positive integer")
		return
	}
	// 汇总查询本身对不存在的 run 只会返回空 days；account_id 也必须来自
	// run 行，因此这里先取 run，顺带把「不存在」映射成 404。
	run, ok := h.requireRun(w, r, runID)
	if !ok {
		return
	}
	days, err := h.repo.Summary(r.Context(), runID)
	if err != nil {
		h.writeRepoError(w, "summarize reconciliation run", err)
		return
	}
	views := make([]dto.ReconcileSummaryDay, 0, len(days))
	for _, day := range days {
		views = append(views, dto.ReconcileSummaryDay{
			UsageDate:         openapi_types.Date{Time: day.UsageDate},
			ItemCount:         day.ItemCount,
			AlertCount:        day.AlertCount,
			WarnCount:         day.WarnCount,
			DeltaTokenBp:      day.DeltaTokenBP,
			RevenueMinor:      day.RevenueMinor,
			UpstreamCostMinor: day.UpstreamCostMinor,
			MarginMinor:       day.MarginMinor,
		})
	}
	writeJSON(w, http.StatusOK, dto.ReconcileSummary{
		RunId: run.ID, AccountId: run.AccountID,
		GeneratedAt: h.now().UTC(), Days: views,
	})
}

func (h *ReconcileHandlers) handleTriggerRun(w http.ResponseWriter, r *http.Request) {
	p, ok := adminPrincipal(w, r)
	if !ok {
		return
	}
	var req dto.ReconcileRunRequest
	if err := readAuthJSON(r, &req); err != nil {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"request body must be a JSON object")
		return
	}
	if req.AccountId != nil && *req.AccountId <= 0 {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"account_id must be a positive integer")
		return
	}
	if h.runner == nil {
		// 测试/开发装配可能没有 runner；读端点仍可用，但触发必须显式失败。
		writeAdminProblem(w, http.StatusServiceUnavailable, authProblemServiceUnavailable,
			"Service unavailable", "reconciliation runner is not configured")
		return
	}

	var accountIDs []int64
	if req.AccountId != nil {
		accountIDs = []int64{*req.AccountId}
	}
	// 手动触发是管理员的显式补跑：不读 reconcile.enabled，开关只约束 worker。
	ctx, cancel := context.WithTimeout(r.Context(), reconcileRunTimeout)
	defer cancel()
	outcome, err := h.runner.Run(ctx, reconcileTriggerManual, accountIDs)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			detail := fmt.Sprintf("reconciliation did not finish within %s", reconcileRunTimeout)
			if len(outcome.RunIDs) > 0 {
				detail += fmt.Sprintf(" (created run ids: %s)", joinInt64s(outcome.RunIDs))
			}
			writeAdminProblem(w, http.StatusGatewayTimeout, problemTypeGatewayTimeout,
				"Gateway timeout", detail)
			return
		}
		h.log.Error("trigger reconciliation run failed", "error", err)
		writeAdminProblem(w, http.StatusInternalServerError, authProblemInternal,
			"Internal server error", "")
		return
	}

	// 折叠 runner 的 Outcome 到契约的三值状态：
	//   - skipped（没有可处理账号）→ succeeded + 零计数，语义是「无事可做」；
	//   - disabled 只可能由 worker 触发产生；手动触发不读该开关，这里保留为
	//     不可达的防御分支，按契约以 409 明确告知开关状态。
	status := outcome.Status
	switch status {
	case reconcileOutcomeSkipped:
		status = "succeeded"
		outcome.RunIDs = []int64{}
		outcome.Items = 0
		outcome.Alerts = 0
	case reconcileOutcomeDisabled:
		writeAdminProblem(w, http.StatusConflict, authProblemConflict, "Conflict",
			"reconciliation is disabled (reconcile_disabled)")
		return
	}
	if outcome.RunIDs == nil {
		// 契约要求 run_ids 是数组；nil 会序列化成 null。
		outcome.RunIDs = []int64{}
	}

	detail := map[string]any{
		"action":  audit.ActionReconcileRun,
		"trigger": reconcileTriggerManual,
		"run_ids": outcome.RunIDs,
		"items":   outcome.Items,
		"alerts":  outcome.Alerts,
		"status":  status,
	}
	if req.AccountId != nil {
		detail["account_id"] = *req.AccountId
	}
	h.auditEvent(r, audit.ActionReconcileRun, p.UserID, detail, nil, nil, http.StatusOK)
	writeJSON(w, http.StatusOK, dto.ReconcileRunResult{
		RunIds: outcome.RunIDs,
		Items:  int64(outcome.Items),
		Alerts: int64(outcome.Alerts),
		Status: dto.ReconcileRunResultStatus(status),
	})
}

func (h *ReconcileHandlers) handleAckItem(w http.ResponseWriter, r *http.Request) {
	p, ok := adminPrincipal(w, r)
	if !ok {
		return
	}
	id, ok := reconcilePathID(w, r)
	if !ok {
		return
	}
	var req dto.ReconcileAckRequest
	if err := readAuthJSON(r, &req); err != nil {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"request body must be a JSON object")
		return
	}
	status := string(dto.ReconcileAckRequestStatusAcknowledged)
	if req.Status != nil {
		if !req.Status.Valid() {
			writeUpstreamInvalidFields(w, []string{"status"})
			return
		}
		status = string(*req.Status)
	}
	if utf8.RuneCountInString(req.Note) > reconcileAckNoteMaxRunes {
		writeUpstreamInvalidFields(w, []string{"note"})
		return
	}

	before, err := h.repo.GetItem(r.Context(), id)
	if err != nil {
		h.writeRepoError(w, "read reconciliation item before ack", err)
		return
	}
	if err := h.repo.AckItem(r.Context(), id, status, req.Note); err != nil {
		h.writeRepoError(w, "ack reconciliation item", err)
		return
	}
	// 契约要求返回更新后的 updated_at；重新读取而不是在内存里伪造时间戳。
	after, err := h.repo.GetItem(r.Context(), id)
	if err != nil {
		h.writeRepoError(w, "read reconciliation item after ack", err)
		return
	}

	h.auditEvent(r, audit.ActionReconcileItemAck, p.UserID, map[string]any{
		"action":     audit.ActionReconcileItemAck,
		"id":         after.ID,
		"run_id":     after.RunID,
		"account_id": after.AccountID,
	}, reconcileItemSnapshot(before), reconcileItemSnapshot(after), http.StatusOK)

	updatedAt := after.UpdatedAt
	note := after.Note
	writeJSON(w, http.StatusOK, dto.ReconcileAckResult{
		Id:        after.ID,
		Status:    dto.ReconcileAckResultStatus(after.Status),
		Note:      &note,
		UpdatedAt: &updatedAt,
	})
}

// ---------------------------------------------------------------------------
// 视图 / 审计 / 错误映射
// ---------------------------------------------------------------------------

// reconcileRunDTO 把仓储行投影为契约形状。
func reconcileRunDTO(run store.ReconcileRun) dto.ReconcileRun {
	return dto.ReconcileRun{
		Id:                   run.ID,
		AccountId:            run.AccountID,
		WindowStart:          openapi_types.Date{Time: run.WindowStart},
		WindowEnd:            openapi_types.Date{Time: run.WindowEnd},
		TriggeredBy:          dto.ReconcileRunTriggeredBy(run.TriggeredBy),
		Status:               dto.ReconcileRunStatus(run.Status),
		TokenToleranceBp:     run.TokenToleranceBP,
		AmountToleranceMinor: run.AmountToleranceMinor,
		QuotaPerUnit:         run.QuotaPerUnit,
		Timezone:             run.Timezone,
		StartedAt:            run.StartedAt,
		FinishedAt:           run.FinishedAt,
		Error:                run.Error,
	}
}

// reconcileItemDTO 把仓储行投影为契约形状；可空列原样保留 NULL 语义。
func reconcileItemDTO(item store.ReconcileItem) dto.ReconcileItem {
	return dto.ReconcileItem{
		Id:                        item.ID,
		RunId:                     item.RunID,
		AccountId:                 item.AccountID,
		UsageDate:                 openapi_types.Date{Time: item.UsageDate},
		UpstreamModel:             item.UpstreamModel,
		LocalRequests:             item.LocalRequests,
		LocalEstimatedRequests:    item.LocalEstimatedRequests,
		LocalRefundedRequests:     item.LocalRefundedRequests,
		LocalCacheHitTokens:       item.LocalCacheHitTokens,
		LocalCacheMissTokens:      item.LocalCacheMissTokens,
		LocalOutputTokens:         item.LocalOutputTokens,
		LocalWalletCredits:        item.LocalWalletCredits,
		LocalSubscriptionCredits:  item.LocalSubscriptionCredits,
		LocalCostCredits:          item.LocalCostCredits,
		LocalUnknownRequests:      item.LocalUnknownRequests,
		LocalMultiAttemptRequests: item.LocalMultiAttemptRequests,
		SnapshotCacheHitTokens:    item.SnapshotCacheHitTokens,
		SnapshotCacheMissTokens:   item.SnapshotCacheMissTokens,
		SnapshotCacheWriteTokens:  item.SnapshotCacheWriteTokens,
		SnapshotOutputTokens:      item.SnapshotOutputTokens,
		SnapshotRequests:          item.SnapshotRequests,
		SnapshotAmountMinor:       item.SnapshotAmountMinor,
		DeltaCacheHitTokens:       item.DeltaCacheHitTokens,
		DeltaCacheMissTokens:      item.DeltaCacheMissTokens,
		DeltaOutputTokens:         item.DeltaOutputTokens,
		DeltaTokenBp:              item.DeltaTokenBP,
		SnapshotPartial:           item.SnapshotPartial,
		RevenueMinor:              item.RevenueMinor,
		UpstreamCostMinor:         item.UpstreamCostMinor,
		MarginMinor:               item.MarginMinor,
		MarginBp:                  item.MarginBP,
		Severity:                  dto.ReconcileItemSeverity(item.Severity),
		Status:                    dto.ReconcileItemStatus(item.Status),
		Note:                      item.Note,
		CreatedAt:                 item.CreatedAt,
		UpdatedAt:                 item.UpdatedAt,
	}
}

// reconcileItemSnapshot 是 ack 审计的 before/after 投影：只记录处置状态与
// 说明，不复制差异明细的统计口径。
func reconcileItemSnapshot(item *store.ReconcileItem) map[string]any {
	if item == nil {
		return nil
	}
	return map[string]any{
		"status": item.Status,
		"note":   item.Note,
	}
}

// reconcilePathID 解析路径 id：非法/非正数与不存在同形 404。
func reconcilePathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeAdminProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found", "")
		return 0, false
	}
	return id, true
}

// requireRun 读取 run；不存在映射 404，其余错误 500。
func (h *ReconcileHandlers) requireRun(w http.ResponseWriter, r *http.Request, id int64) (*store.ReconcileRun, bool) {
	run, err := h.repo.GetRun(r.Context(), id)
	if err != nil {
		h.writeRepoError(w, "get reconciliation run", err)
		return nil, false
	}
	return run, true
}

func (h *ReconcileHandlers) writeRepoError(w http.ResponseWriter, op string, err error) {
	switch {
	case errors.Is(err, store.ErrRunNotFound), errors.Is(err, store.ErrItemNotFound):
		writeAdminProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found", "")
	default:
		h.log.Error(op+" failed", "error", err)
		writeAdminProblem(w, http.StatusInternalServerError, authProblemInternal,
			"Internal server error", "")
	}
}

// auditEvent 写对账审计（best-effort：日志库独立，失败只记日志）。
func (h *ReconcileHandlers) auditEvent(r *http.Request, action string, actor int64, detail, before, after map[string]any, status int) {
	if h.audit == nil {
		return
	}
	event := audit.Event{
		Action:     action,
		Actor:      actor,
		IP:         tokenRemoteIP(r),
		UserAgent:  r.UserAgent(),
		Method:     r.Method,
		Route:      r.Pattern,
		StatusCode: status,
		Detail:     adminMarshalRaw(detail),
		Before:     adminMarshalRaw(before),
		After:      adminMarshalRaw(after),
	}
	if err := writeAuditEvent(r.Context(), h.audit, event); err != nil {
		h.log.Warn("reconciliation audit write failed", "action", action, "actor", actor, "error", err)
	}
}

// joinInt64s 把 run id 列表渲染成 504 detail 里的可读片段。
func joinInt64s(values []int64) string {
	parts := make([]string, 0, len(values))
	for _, value := range values {
		parts = append(parts, strconv.FormatInt(value, 10))
	}
	return strings.Join(parts, ", ")
}
