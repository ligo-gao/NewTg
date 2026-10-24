// 上游用量对账：上游用量快照端点（设计 §4.1；plan Task 8）。
//
// 端点面（全部要求 `settings:manage`）：
//
//	GET /api/upstream-snapshots   按账号 + [from, to] 闭区间读取（不分页）
//	PUT /api/upstream-snapshots   批量 upsert（≤500 条，单事务，返回 {updated:n}）
//
// 溯源字段由服务端盖章，客户端不可提交（写入 DTO 也不含这些字段）：
//   - entered_by = 当前管理员；
//   - entered_at = 写入时刻；
//   - source = manual（本端点就是人工录入面）；
//   - account_id = 请求体（读取面则来自 query）；
//   - currency 缺省取所属账号的币种（账号也没有才回退 CNY）。
//
// 「一天」的边界由账号时区决定：usage_date 不允许晚于账号时区里的今天，
// 否则一条未来日期的快照永远不会被 runner 的窗口读到。
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

	openapi_types "github.com/oapi-codegen/runtime/types"

	"newtg/gateway/internal/api/dto"
	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/rbac"
	"newtg/gateway/internal/reconcile"
	"newtg/gateway/internal/store"
)

const (
	upstreamModelMaxRunes      = 128
	maxUpstreamSnapshotItems   = 500
	upstreamSnapshotDateLayout = "2006-01-02"
)

// UpstreamSnapshotRepo 是快照端点的存储接缝；*store.UpstreamSnapshotRepo 满足。
type UpstreamSnapshotRepo interface {
	ListRange(ctx context.Context, accountID int64, from, to time.Time) ([]store.UpstreamSnapshot, error)
	Upsert(ctx context.Context, items []store.UpstreamSnapshot) (int, error)
}

// UpstreamSnapshotHandlerConfig 是快照端点的装配输入。Repo 与 DB 二选一；
// Accounts 用于取账号时区/币种与 404 判定（DB 非 nil 时自动构造）。
type UpstreamSnapshotHandlerConfig struct {
	DB       *sql.DB
	Repo     UpstreamSnapshotRepo
	Accounts UpstreamAccountLookup
	Authz    *rbac.AuthzMiddleware
	Audit    *audit.Logger
	Logger   *slog.Logger
	// Now 是「今天」判定的时钟接缝；nil 时使用 time.Now。
	Now func() time.Time
	// LookbackDays 是 from/to 缺省时的回看天数；<=0 时使用 runner 的默认值。
	LookbackDays int
}

// UpstreamSnapshotHandlers 持有快照端点依赖，可并发使用。
type UpstreamSnapshotHandlers struct {
	repo         UpstreamSnapshotRepo
	accounts     UpstreamAccountLookup
	authz        *rbac.AuthzMiddleware
	audit        *audit.Logger
	log          *slog.Logger
	now          func() time.Time
	lookbackDays int
}

var (
	upstreamSnapshotListAction   = rbac.AuthzAction{ResourceKind: "upstream_snapshot", ActionName: "list"}
	upstreamSnapshotUpsertAction = rbac.AuthzAction{ResourceKind: "upstream_snapshot", ActionName: "upsert"}
)

// NewUpstreamSnapshotHandlers 构造快照端点。
func NewUpstreamSnapshotHandlers(cfg UpstreamSnapshotHandlerConfig) (*UpstreamSnapshotHandlers, error) {
	repo := cfg.Repo
	if repo == nil {
		if cfg.DB == nil {
			return nil, errors.New("httpapi: UpstreamSnapshotHandlerConfig.Repo or DB is required")
		}
		repo = store.NewUpstreamSnapshotRepo(cfg.DB)
	}
	accounts := cfg.Accounts
	if accounts == nil {
		if cfg.DB == nil {
			return nil, errors.New("httpapi: UpstreamSnapshotHandlerConfig.Accounts or DB is required")
		}
		accounts = store.NewUpstreamAccountRepo(cfg.DB)
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
	lookback := cfg.LookbackDays
	if lookback <= 0 {
		lookback = reconcile.DefaultLookbackDays
	}
	h := &UpstreamSnapshotHandlers{
		repo: repo, accounts: accounts, authz: authz, audit: cfg.Audit, log: log,
		now: now, lookbackDays: lookback,
	}
	h.registerPolicies()
	return h, nil
}

func (h *UpstreamSnapshotHandlers) registerPolicies() {
	h.authz.SetWritePolicy(upstreamSnapshotUpsertAction, &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{
		"account_id": nil,
		"items": {Fields: map[string]*rbac.FieldPolicy{
			"usage_date":              nil,
			"upstream_model":          nil,
			"input_cache_hit_tokens":  nil,
			"input_cache_miss_tokens": nil,
			"cache_write_tokens":      nil,
			"output_tokens":           nil,
			"requests":                nil,
			"amount_minor":            nil,
			"currency":                nil,
			"note":                    nil,
		}},
	}})
}

// RegisterRoutes 注册快照端点。wrap 语义同 RegisterGroupRoutes。
func (h *UpstreamSnapshotHandlers) RegisterRoutes(mux *http.ServeMux, wrap func(http.Handler) http.Handler) {
	mount := func(pattern string, handler http.Handler) {
		if wrap != nil {
			handler = wrap(handler)
		}
		mux.Handle(pattern, handler)
	}
	mount("GET /api/upstream-snapshots", h.authz.Handler(rbac.PermSettingsManage, upstreamSnapshotListAction)(
		http.HandlerFunc(h.handleList)))
	mount("PUT /api/upstream-snapshots", h.authz.Handler(rbac.PermSettingsManage, upstreamSnapshotUpsertAction)(
		http.HandlerFunc(h.handleUpsert)))
}

// ---------------------------------------------------------------------------
// handlers
// ---------------------------------------------------------------------------

func (h *UpstreamSnapshotHandlers) handleList(w http.ResponseWriter, r *http.Request) {
	accountID, ok := h.requireAccountID(w, strings.TrimSpace(r.URL.Query().Get("account_id")))
	if !ok {
		return
	}
	account, ok := h.loadAccount(w, r, accountID)
	if !ok {
		return
	}
	loc, ok := h.accountLocation(w, account)
	if !ok {
		return
	}

	today := upstreamToday(h.now(), loc)
	from := today.AddDate(0, 0, -h.lookbackDays)
	to := today
	if raw := strings.TrimSpace(r.URL.Query().Get("from")); raw != "" {
		parsed, valid := parseUpstreamDate(raw)
		if !valid {
			writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
				"from must be a valid YYYY-MM-DD date")
			return
		}
		from = parsed
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("to")); raw != "" {
		parsed, valid := parseUpstreamDate(raw)
		if !valid {
			writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
				"to must be a valid YYYY-MM-DD date")
			return
		}
		to = parsed
	}
	if from.After(to) {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"from must not be after to")
		return
	}

	rows, err := h.repo.ListRange(r.Context(), accountID, from, to)
	if err != nil {
		h.writeRepoError(w, "list upstream snapshots", err)
		return
	}
	items := make([]dto.UpstreamSnapshot, 0, len(rows))
	for _, row := range rows {
		items = append(items, upstreamSnapshotDTO(row))
	}
	writeJSON(w, http.StatusOK, dto.UpstreamSnapshotList{Items: items})
}

func (h *UpstreamSnapshotHandlers) handleUpsert(w http.ResponseWriter, r *http.Request) {
	p, ok := adminPrincipal(w, r)
	if !ok {
		return
	}
	var req dto.UpstreamSnapshotUpsert
	if err := readAuthJSON(r, &req); err != nil {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"request body must be a JSON object with account_id and items")
		return
	}
	if req.AccountId <= 0 {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"account_id is required and must be a positive integer")
		return
	}
	if req.Items == nil {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"items is required and must be an array")
		return
	}
	if len(req.Items) > maxUpstreamSnapshotItems {
		writeUpstreamInvalidFields(w, []string{"items"})
		return
	}

	account, ok := h.loadAccount(w, r, req.AccountId)
	if !ok {
		return
	}
	loc, ok := h.accountLocation(w, account)
	if !ok {
		return
	}
	currency := upstreamAccountCurrency(account)
	today := upstreamToday(h.now(), loc)
	enteredAt := h.now().UTC()

	invalid := make([]string, 0, 4)
	items := make([]store.UpstreamSnapshot, 0, len(req.Items))
	for index, item := range req.Items {
		prefix := fmt.Sprintf("items[%d].", index)

		model := strings.TrimSpace(item.UpstreamModel)
		if !validUpstreamText(model, upstreamModelMaxRunes) {
			invalid = append(invalid, prefix+"upstream_model")
		}

		usageDate := item.UsageDate.Time
		if usageDate.IsZero() {
			invalid = append(invalid, prefix+"usage_date")
		} else if upstreamDateAfter(usageDate, today) {
			// 未来日期按账号时区的「今天」判定：晚一天的快照不属于任何已发生
			// 的账期，只会静默躺在表里不进 runner 窗口。
			invalid = append(invalid, prefix+"usage_date")
		}

		if upstreamNegative(item.InputCacheHitTokens) {
			invalid = append(invalid, prefix+"input_cache_hit_tokens")
		}
		if upstreamNegative(item.InputCacheMissTokens) {
			invalid = append(invalid, prefix+"input_cache_miss_tokens")
		}
		if upstreamNegative(item.CacheWriteTokens) {
			invalid = append(invalid, prefix+"cache_write_tokens")
		}
		if upstreamNegative(item.OutputTokens) {
			invalid = append(invalid, prefix+"output_tokens")
		}
		if upstreamNegative(item.Requests) {
			invalid = append(invalid, prefix+"requests")
		}
		if upstreamNegative(item.AmountMinor) {
			invalid = append(invalid, prefix+"amount_minor")
		}

		itemCurrency := currency
		if item.Currency != nil && strings.TrimSpace(*item.Currency) != "" {
			normalized, valid := normalizeCurrency(*item.Currency)
			if !valid {
				invalid = append(invalid, prefix+"currency")
			} else {
				itemCurrency = normalized
			}
		}

		note := ""
		if item.Note != nil {
			note = *item.Note
		}
		enteredBy := p.UserID
		items = append(items, store.UpstreamSnapshot{
			AccountID:        req.AccountId,
			UsageDate:        usageDate,
			UpstreamModel:    model,
			Currency:         itemCurrency,
			Source:           string(dto.UpstreamSnapshotSourceManual),
			CacheHitTokens:   item.InputCacheHitTokens,
			CacheMissTokens:  item.InputCacheMissTokens,
			CacheWriteTokens: item.CacheWriteTokens,
			OutputTokens:     item.OutputTokens,
			Requests:         item.Requests,
			AmountMinor:      item.AmountMinor,
			EnteredBy:        &enteredBy,
			EnteredAt:        enteredAt,
			Note:             note,
		})
	}
	if len(invalid) > 0 {
		writeUpstreamInvalidFields(w, invalid)
		return
	}

	updated, err := h.repo.Upsert(r.Context(), items)
	if err != nil {
		h.writeRepoError(w, "upsert upstream snapshots", err)
		return
	}
	// 一次批量导入只写一条审计：逐行写会把 500 行导入放大成 500 条噪声，
	// 且审计面只需要「谁、哪个账号、写了多少行」。
	h.auditEvent(r, p.UserID, map[string]any{
		"action":     audit.ActionUpstreamSnapshotUpsert,
		"account_id": req.AccountId,
		"updated":    updated,
		"submitted":  len(items),
	}, http.StatusOK)
	writeJSON(w, http.StatusOK, dto.UpstreamSnapshotUpsertResult{Updated: updated})
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

// requireAccountID 校验 query 里的 account_id（必填、正整数）。
func (h *UpstreamSnapshotHandlers) requireAccountID(w http.ResponseWriter, raw string) (int64, bool) {
	if raw == "" {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"account_id is required")
		return 0, false
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"account_id must be a positive integer")
		return 0, false
	}
	return id, true
}

// loadAccount 读取账号；不存在映射 404（与账号端点同形）。
func (h *UpstreamSnapshotHandlers) loadAccount(w http.ResponseWriter, r *http.Request, id int64) (*store.UpstreamAccount, bool) {
	account, err := h.accounts.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrUpstreamAccountNotFound) {
			writeAdminProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found", "")
		} else {
			h.log.Error("load upstream account for snapshots failed", "account_id", id, "error", err)
			writeAdminProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error", "")
		}
		return nil, false
	}
	return account, true
}

// accountLocation 解析账号时区；空值先补默认再解析（绝不把空串直接交给
// ParseAccountTimeZone，那会静默按 UTC 切日界）。
func (h *UpstreamSnapshotHandlers) accountLocation(w http.ResponseWriter, account *store.UpstreamAccount) (*time.Location, bool) {
	timezone := strings.TrimSpace(account.Timezone)
	if timezone == "" {
		timezone = upstreamDefaultTimezone
	}
	loc, err := reconcile.ParseAccountTimeZone(timezone)
	if err != nil {
		h.log.Error("upstream account has an invalid timezone", "account_id", account.ID, "timezone", account.Timezone)
		writeAdminProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error", "")
		return nil, false
	}
	return loc, true
}

func (h *UpstreamSnapshotHandlers) writeRepoError(w http.ResponseWriter, op string, err error) {
	switch {
	case errors.Is(err, store.ErrUpstreamAccountNotFound):
		writeAdminProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found", "")
	default:
		h.log.Error(op+" failed", "error", err)
		writeAdminProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error", "")
	}
}

func (h *UpstreamSnapshotHandlers) auditEvent(r *http.Request, actor int64, detail map[string]any, status int) {
	if h.audit == nil {
		return
	}
	event := audit.Event{
		Action:     audit.ActionUpstreamSnapshotUpsert,
		Actor:      actor,
		IP:         tokenRemoteIP(r),
		UserAgent:  r.UserAgent(),
		Method:     r.Method,
		Route:      r.Pattern,
		StatusCode: status,
		Detail:     adminMarshalRaw(detail),
	}
	if err := writeAuditEvent(r.Context(), h.audit, event); err != nil {
		h.log.Warn("upstream snapshot audit write failed", "actor", actor, "error", err)
	}
}

// upstreamSnapshotDTO 把仓储行投影为契约形状；全部可空列原样保留 NULL 语义
// （nil = 上游未报告，0 = 真实零）。
func upstreamSnapshotDTO(row store.UpstreamSnapshot) dto.UpstreamSnapshot {
	id := row.ID
	note := row.Note
	return dto.UpstreamSnapshot{
		Id:                   &id,
		AccountId:            row.AccountID,
		UsageDate:            openapi_types.Date{Time: row.UsageDate},
		UpstreamModel:        row.UpstreamModel,
		InputCacheHitTokens:  row.CacheHitTokens,
		InputCacheMissTokens: row.CacheMissTokens,
		CacheWriteTokens:     row.CacheWriteTokens,
		OutputTokens:         row.OutputTokens,
		Requests:             row.Requests,
		AmountMinor:          row.AmountMinor,
		Currency:             row.Currency,
		Source:               dto.UpstreamSnapshotSource(row.Source),
		EnteredBy:            row.EnteredBy,
		EnteredAt:            row.EnteredAt,
		Note:                 &note,
	}
}

// upstreamAccountCurrency 返回账号币种；账号币种缺失/非法时回退 CNY。
func upstreamAccountCurrency(account *store.UpstreamAccount) string {
	if account == nil {
		return upstreamDefaultCurrency
	}
	if normalized, ok := normalizeCurrency(account.Currency); ok {
		return normalized
	}
	return upstreamDefaultCurrency
}

// upstreamToday 返回账号时区里的「今天」，以 UTC 零点承载日字面值
// （仓储只按 YYYY-MM-DD 比较，不关心承载时区）。
func upstreamToday(now time.Time, loc *time.Location) time.Time {
	y, m, d := now.In(loc).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// upstreamDateAfter 按年月日比较（忽略时刻/时区），判断 date 是否晚于 today。
func upstreamDateAfter(date, today time.Time) bool {
	dy, dm, dd := date.Date()
	ty, tm, td := today.Date()
	return time.Date(dy, dm, dd, 0, 0, 0, 0, time.UTC).
		After(time.Date(ty, tm, td, 0, 0, 0, 0, time.UTC))
}

// parseUpstreamDate 解析 ISO date 字面值。
func parseUpstreamDate(raw string) (time.Time, bool) {
	parsed, err := time.Parse(upstreamSnapshotDateLayout, raw)
	if err != nil {
		return time.Time{}, false
	}
	return parsed, true
}

// upstreamNegative 报告可空计数值是否为负。
func upstreamNegative(value *int64) bool {
	return value != nil && *value < 0
}
