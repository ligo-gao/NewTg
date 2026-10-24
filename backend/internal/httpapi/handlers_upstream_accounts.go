// 上游用量对账：上游账号管理端点（设计 §4.1；plan Task 8）。
//
// 端点面（全部要求 `settings:manage`）：
//
//	GET    /api/upstream-accounts              offset 分页（id 升序），status 可选筛选
//	POST   /api/upstream-accounts              currency/timezone 服务端补默认，status=1
//	PATCH  /api/upstream-accounts/{id}         部分更新 + expected_updated_at 乐观锁
//	DELETE /api/upstream-accounts/{id}         仍被渠道/快照引用 → 409
//
// 与分组端点（handlers_groups.go）保持同一构造/鉴权/审计形状：
//   - 每个动作登记独立的 rbac.FieldPolicy，未登记字段在鉴权层直接 403；
//   - 业务校验在仓储调用之前完成，错误用 RFC 7807 problem+json；
//   - 账号名唯一冲突复用 store.ErrNameConflict（与渠道共享的哨兵），因此
//     409 的 detail 必须使用账号自己的文案，绝不透出仓储的错误文本。
package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"newtg/gateway/internal/api/dto"
	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/rbac"
	"newtg/gateway/internal/store"
)

const (
	upstreamNameMaxRunes       = 64
	upstreamProviderMaxRunes   = 64
	upstreamTimezoneMaxRunes   = 64
	upstreamConsoleURLMaxRunes = 512

	// 服务端默认值（不依赖 DDL DEFAULT）：空时区若原样落库，
	// reconcile.ParseAccountTimeZone("") 会静默按 UTC 切日界，导致对账日期错位。
	upstreamDefaultTimezone = "Asia/Shanghai"
	upstreamDefaultCurrency = "CNY"

	upstreamStatusActive   = int16(1)
	upstreamStatusDisabled = int16(2)
)

// upstreamProblemInvalidField 是字段级 422 的稳定 type URI。沿用全库
// https://newtg.dev/problems/<slug> 约定；problem.fields 列出非法字段名。
const upstreamProblemInvalidField = "https://newtg.dev/problems/invalid-field"

// writeUpstreamInvalidFields 输出 422 字段级问题。problem 的
// additionalProperties 允许 fields 扩展成员（与 rbac 的字段拒绝同形）。
func writeUpstreamInvalidFields(w http.ResponseWriter, fields []string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(http.StatusUnprocessableEntity)
	problem := dto.Problem{
		Type:   upstreamProblemInvalidField,
		Title:  "Validation failed",
		Status: http.StatusUnprocessableEntity,
	}
	if len(fields) > 0 {
		problem.Set("fields", fields)
	}
	// 响应已开始写出；客户端断开导致的编码失败无法再改状态码。
	_ = json.NewEncoder(w).Encode(problem)
}

// UpstreamAccountRepo 是账号端点的存储接缝；*store.UpstreamAccountRepo 满足。
type UpstreamAccountRepo interface {
	List(ctx context.Context, status *int16, page, pageSize int) ([]store.UpstreamAccount, int64, error)
	Get(ctx context.Context, id int64) (*store.UpstreamAccount, error)
	Create(ctx context.Context, a store.UpstreamAccount) (*store.UpstreamAccount, error)
	Update(ctx context.Context, id int64, patch store.UpstreamAccountPatch) (*store.UpstreamAccount, error)
	Delete(ctx context.Context, id int64) error
}

// UpstreamAccountLookup 是只需要「按 id 读账号」的窄接口（快照端点用它取
// 账号时区/币种，不暴露写能力）。
type UpstreamAccountLookup interface {
	Get(ctx context.Context, id int64) (*store.UpstreamAccount, error)
}

// UpstreamAccountHandlerConfig 是账号端点的装配输入。Repo 与 DB 二选一。
// Audit 为 nil 时不写审计（未装配日志库的测试/开发环境）。
type UpstreamAccountHandlerConfig struct {
	DB     *sql.DB
	Repo   UpstreamAccountRepo
	Authz  *rbac.AuthzMiddleware
	Audit  *audit.Logger
	Logger *slog.Logger
}

// UpstreamAccountHandlers 持有账号端点依赖，可并发使用。
type UpstreamAccountHandlers struct {
	repo  UpstreamAccountRepo
	authz *rbac.AuthzMiddleware
	audit *audit.Logger
	log   *slog.Logger
}

var (
	upstreamAccountListAction   = rbac.AuthzAction{ResourceKind: "upstream_account", ActionName: "list"}
	upstreamAccountCreateAction = rbac.AuthzAction{ResourceKind: "upstream_account", ActionName: "create"}
	upstreamAccountUpdateAction = rbac.AuthzAction{ResourceKind: "upstream_account", ActionName: "update"}
	upstreamAccountDeleteAction = rbac.AuthzAction{ResourceKind: "upstream_account", ActionName: "delete"}
)

// NewUpstreamAccountHandlers 构造账号管理端点。
func NewUpstreamAccountHandlers(cfg UpstreamAccountHandlerConfig) (*UpstreamAccountHandlers, error) {
	repo := cfg.Repo
	if repo == nil {
		if cfg.DB == nil {
			return nil, errors.New("httpapi: UpstreamAccountHandlerConfig.Repo or DB is required")
		}
		repo = store.NewUpstreamAccountRepo(cfg.DB)
	}
	authz := cfg.Authz
	if authz == nil {
		authz = rbac.NewAuthzMiddleware()
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	h := &UpstreamAccountHandlers{repo: repo, authz: authz, audit: cfg.Audit, log: log}
	h.registerPolicies()
	return h, nil
}

func (h *UpstreamAccountHandlers) registerPolicies() {
	h.authz.SetWritePolicy(upstreamAccountCreateAction, &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{
		"name":        nil,
		"provider":    nil,
		"currency":    nil,
		"timezone":    nil,
		"console_url": nil,
		"note":        nil,
	}})
	h.authz.SetWritePolicy(upstreamAccountUpdateAction, &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{
		"expected_updated_at": nil,
		"name":                nil,
		"provider":            nil,
		"currency":            nil,
		"timezone":            nil,
		"console_url":         nil,
		"status":              nil,
		"note":                nil,
	}})
	// DELETE 没有业务请求体，但仍必须登记：未登记的动作对所有写请求失败关闭。
	h.authz.SetWritePolicy(upstreamAccountDeleteAction,
		&rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{}})
}

// RegisterRoutes 注册账号端点。wrap 语义同 RegisterGroupRoutes。
func (h *UpstreamAccountHandlers) RegisterRoutes(mux *http.ServeMux, wrap func(http.Handler) http.Handler) {
	mount := func(pattern string, handler http.Handler) {
		if wrap != nil {
			handler = wrap(handler)
		}
		mux.Handle(pattern, handler)
	}
	mount("GET /api/upstream-accounts", h.authz.Handler(rbac.PermSettingsManage, upstreamAccountListAction)(
		http.HandlerFunc(h.handleList)))
	mount("POST /api/upstream-accounts", h.authz.Handler(rbac.PermSettingsManage, upstreamAccountCreateAction)(
		http.HandlerFunc(h.handleCreate)))
	mount("PATCH /api/upstream-accounts/{id}", h.authz.Handler(rbac.PermSettingsManage, upstreamAccountUpdateAction)(
		http.HandlerFunc(h.handleUpdate)))
	mount("DELETE /api/upstream-accounts/{id}", h.authz.Handler(rbac.PermSettingsManage, upstreamAccountDeleteAction)(
		http.HandlerFunc(h.handleDelete)))
}

// ---------------------------------------------------------------------------
// handlers
// ---------------------------------------------------------------------------

func (h *UpstreamAccountHandlers) handleList(w http.ResponseWriter, r *http.Request) {
	page, pageSize := 1, adminPageSizeDefault
	if raw := strings.TrimSpace(r.URL.Query().Get("p")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > adminPageMax {
			writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
				fmt.Sprintf("p must be between 1 and %d", adminPageMax))
			return
		}
		page = value
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("page_size")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > adminPageSizeMax {
			writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
				fmt.Sprintf("page_size must be between 1 and %d", adminPageSizeMax))
			return
		}
		pageSize = value
	}
	var status *int16
	if raw := strings.TrimSpace(r.URL.Query().Get("status")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || (value != int(upstreamStatusActive) && value != int(upstreamStatusDisabled)) {
			writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
				"status must be 1 (active) or 2 (disabled)")
			return
		}
		typed := int16(value)
		status = &typed
	}

	items, total, err := h.repo.List(r.Context(), status, page, pageSize)
	if err != nil {
		h.writeRepoError(w, "list upstream accounts", err)
		return
	}
	views := make([]dto.UpstreamAccount, 0, len(items))
	for _, item := range items {
		views = append(views, upstreamAccountDTO(item))
	}
	writeJSON(w, http.StatusOK, dto.UpstreamAccountList{
		Items: views, Total: int(total), Page: page, PageSize: pageSize,
	})
}

func (h *UpstreamAccountHandlers) handleCreate(w http.ResponseWriter, r *http.Request) {
	p, ok := adminPrincipal(w, r)
	if !ok {
		return
	}
	var req dto.UpstreamAccountCreate
	if err := readAuthJSON(r, &req); err != nil {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"request body must be a JSON object")
		return
	}

	name := strings.TrimSpace(req.Name)
	if !validUpstreamText(name, upstreamNameMaxRunes) {
		writeUpstreamInvalidFields(w, []string{"name"})
		return
	}
	provider := strings.TrimSpace(req.Provider)
	if !validUpstreamText(provider, upstreamProviderMaxRunes) {
		writeUpstreamInvalidFields(w, []string{"provider"})
		return
	}

	// 缺省或空串一律补服务端默认；非空时区必须是可解析的 IANA 名称。
	timezone := upstreamDefaultTimezone
	if req.Timezone != nil && strings.TrimSpace(*req.Timezone) != "" {
		timezone = strings.TrimSpace(*req.Timezone)
	}
	if utf8.RuneCountInString(timezone) > upstreamTimezoneMaxRunes {
		writeUpstreamInvalidFields(w, []string{"timezone"})
		return
	}
	if _, err := time.LoadLocation(timezone); err != nil {
		writeUpstreamInvalidFields(w, []string{"timezone"})
		return
	}

	currency := upstreamDefaultCurrency
	if req.Currency != nil && strings.TrimSpace(*req.Currency) != "" {
		value, ok := normalizeCurrency(*req.Currency)
		if !ok {
			writeUpstreamInvalidFields(w, []string{"currency"})
			return
		}
		currency = value
	}

	consoleURL, ok := normalizeConsoleURL(req.ConsoleUrl)
	if !ok {
		writeUpstreamInvalidFields(w, []string{"console_url"})
		return
	}
	note := ""
	if req.Note != nil {
		note = *req.Note
	}

	rec, err := h.repo.Create(r.Context(), store.UpstreamAccount{
		Name: name, Provider: provider, Currency: currency, Timezone: timezone,
		ConsoleURL: consoleURL, Status: upstreamStatusActive, Note: note,
	})
	if err != nil {
		h.writeRepoError(w, "create upstream account", err)
		return
	}
	h.auditEvent(r, audit.ActionUpstreamAccountCreate, p.UserID, map[string]any{
		"action": audit.ActionUpstreamAccountCreate, "id": rec.ID, "name": rec.Name,
	}, nil, upstreamAccountSnapshot(*rec), http.StatusCreated)
	writeJSON(w, http.StatusCreated, upstreamAccountDTO(*rec))
}

func (h *UpstreamAccountHandlers) handleUpdate(w http.ResponseWriter, r *http.Request) {
	p, ok := adminPrincipal(w, r)
	if !ok {
		return
	}
	id, ok := upstreamAccountID(w, r)
	if !ok {
		return
	}

	// 先取原始对象：RawMessage 保留「缺省 / null / 具体值」三态，
	// 这是 generated DTO 的 *T 无法表达的（null 与缺省都解成 nil）。
	body, err := readRawObject(r)
	if err != nil {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"request body must be a JSON object")
		return
	}
	raw, err := json.Marshal(body)
	if err != nil {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"request body must be a JSON object")
		return
	}
	var req dto.UpstreamAccountUpdate
	if err := json.Unmarshal(raw, &req); err != nil {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"request body contains invalid field types")
		return
	}
	if req.ExpectedUpdatedAt.IsZero() {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"expected_updated_at is required")
		return
	}

	patch := store.UpstreamAccountPatch{ExpectedUpdatedAt: req.ExpectedUpdatedAt.UTC()}
	invalid := make([]string, 0, 4)

	if rawValue, present := body["name"]; present {
		value, valid := upstreamRawString(rawValue)
		value = strings.TrimSpace(value)
		if !valid || !validUpstreamText(value, upstreamNameMaxRunes) {
			invalid = append(invalid, "name")
		} else {
			patch.Name = &value
		}
	}
	if rawValue, present := body["provider"]; present {
		value, valid := upstreamRawString(rawValue)
		value = strings.TrimSpace(value)
		if !valid || !validUpstreamText(value, upstreamProviderMaxRunes) {
			invalid = append(invalid, "provider")
		} else {
			patch.Provider = &value
		}
	}
	if rawValue, present := body["currency"]; present {
		value, valid := upstreamRawString(rawValue)
		normalized, normalizedOK := normalizeCurrency(value)
		if !valid || !normalizedOK {
			invalid = append(invalid, "currency")
		} else {
			patch.Currency = &normalized
		}
	}
	if rawValue, present := body["timezone"]; present {
		value, valid := upstreamRawString(rawValue)
		value = strings.TrimSpace(value)
		// PATCH 的 present-but-empty 是 422，而不是回退默认：
		// 清空时区会让日界退化为 UTC，必须让调用方显式改正。
		if !valid || value == "" || utf8.RuneCountInString(value) > upstreamTimezoneMaxRunes {
			invalid = append(invalid, "timezone")
		} else if _, err := time.LoadLocation(value); err != nil {
			invalid = append(invalid, "timezone")
		} else {
			patch.Timezone = &value
		}
	}
	if rawValue, present := body["console_url"]; present {
		consoleURL, valid := normalizeConsoleURLRaw(rawValue)
		if !valid {
			invalid = append(invalid, "console_url")
		} else {
			// 显式 null / 空串都表示清空（SQL NULL），与契约一致。
			patch.ConsoleURLSet = true
			patch.ConsoleURL = consoleURL
		}
	}
	if rawValue, present := body["status"]; present {
		var value int
		if string(rawValue) == "null" || json.Unmarshal(rawValue, &value) != nil ||
			(value != int(upstreamStatusActive) && value != int(upstreamStatusDisabled)) {
			invalid = append(invalid, "status")
		} else {
			typed := int16(value)
			patch.Status = &typed
		}
	}
	if rawValue, present := body["note"]; present {
		// note 列 NOT NULL：显式 null 归一为空串（与「清空备注」等价）。
		value, valid := upstreamRawString(rawValue)
		if !valid {
			invalid = append(invalid, "note")
		} else {
			patch.Note = &value
		}
	}
	if len(invalid) > 0 {
		writeUpstreamInvalidFields(w, invalid)
		return
	}

	before, err := h.repo.Get(r.Context(), id)
	if err != nil {
		h.writeRepoError(w, "read upstream account before update", err)
		return
	}
	rec, err := h.repo.Update(r.Context(), id, patch)
	if err != nil {
		h.writeRepoError(w, "update upstream account", err)
		return
	}
	h.auditEvent(r, audit.ActionUpstreamAccountUpdate, p.UserID, map[string]any{
		"action": audit.ActionUpstreamAccountUpdate, "id": rec.ID, "name": rec.Name,
		"changed_fields": upstreamChangedFields(*before, *rec),
	}, upstreamAccountSnapshot(*before), upstreamAccountSnapshot(*rec), http.StatusOK)
	writeJSON(w, http.StatusOK, upstreamAccountDTO(*rec))
}

func (h *UpstreamAccountHandlers) handleDelete(w http.ResponseWriter, r *http.Request) {
	p, ok := adminPrincipal(w, r)
	if !ok {
		return
	}
	id, ok := upstreamAccountID(w, r)
	if !ok {
		return
	}
	before, err := h.repo.Get(r.Context(), id)
	if err != nil {
		h.writeRepoError(w, "read upstream account before delete", err)
		return
	}
	if err := h.repo.Delete(r.Context(), id); err != nil {
		h.writeRepoError(w, "delete upstream account", err)
		return
	}
	h.auditEvent(r, audit.ActionUpstreamAccountDelete, p.UserID, map[string]any{
		"action": audit.ActionUpstreamAccountDelete, "id": before.ID, "name": before.Name,
	}, upstreamAccountSnapshot(*before), nil, http.StatusNoContent)
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// 视图 / 快照 / 校验辅助
// ---------------------------------------------------------------------------

// upstreamAccountDTO 把仓储行投影为契约形状。console_url 为空时是 nil
// （generated DTO 以 omitempty 处理），绝不输出空串。
func upstreamAccountDTO(rec store.UpstreamAccount) dto.UpstreamAccount {
	return dto.UpstreamAccount{
		Id:         rec.ID,
		Name:       rec.Name,
		Provider:   rec.Provider,
		Currency:   rec.Currency,
		Timezone:   rec.Timezone,
		ConsoleUrl: rec.ConsoleURL,
		Status:     dto.UpstreamAccountStatus(rec.Status),
		Note:       rec.Note,
		CreatedAt:  rec.CreatedAt,
		UpdatedAt:  rec.UpdatedAt,
	}
}

// upstreamAccountSnapshot 是审计 before/after 的安全投影。console_url 可能
// 携带签名/查询参数，只记录「是否填写」这一布尔事实。
func upstreamAccountSnapshot(rec store.UpstreamAccount) map[string]any {
	return map[string]any{
		"id":              rec.ID,
		"name":            rec.Name,
		"provider":        rec.Provider,
		"currency":        rec.Currency,
		"timezone":        rec.Timezone,
		"status":          rec.Status,
		"note":            rec.Note,
		"has_console_url": rec.ConsoleURL != nil && *rec.ConsoleURL != "",
	}
}

func upstreamChangedFields(before, after store.UpstreamAccount) []string {
	beforeSnap := upstreamAccountSnapshot(before)
	afterSnap := upstreamAccountSnapshot(after)
	changed := make([]string, 0, 4)
	for _, key := range []string{"name", "provider", "currency", "timezone", "status", "note", "has_console_url"} {
		if !adminJSONEqual(beforeSnap[key], afterSnap[key]) {
			changed = append(changed, key)
		}
	}
	return changed
}

// validUpstreamText 判定必填文本：裁剪后 1..max 个 rune。
func validUpstreamText(value string, max int) bool {
	count := utf8.RuneCountInString(value)
	return count >= 1 && count <= max
}

// normalizeCurrency 校验 ISO 4217 三字母并统一大写。
func normalizeCurrency(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if len(value) != 3 {
		return "", false
	}
	upper := strings.ToUpper(value)
	for _, r := range upper {
		if r < 'A' || r > 'Z' {
			return "", false
		}
	}
	return upper, true
}

// normalizeConsoleURL 校验可选的绝对 http(s) 地址；nil/空串表示未填写。
func normalizeConsoleURL(value *string) (*string, bool) {
	if value == nil {
		return nil, true
	}
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return nil, true
	}
	if utf8.RuneCountInString(trimmed) > upstreamConsoleURLMaxRunes {
		return nil, false
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" {
		return nil, false
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return nil, false
	}
	return &trimmed, true
}

// normalizeConsoleURLRaw 是 PATCH 版本：显式 null 表示清空（nil, true）。
func normalizeConsoleURLRaw(raw json.RawMessage) (*string, bool) {
	if string(raw) == "null" {
		return nil, true
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, false
	}
	return normalizeConsoleURL(&value)
}

// upstreamRawString 解析 RawMessage 的字符串形态；null 视为非法（可空列
// 由调用方单独处理）。ok=false 表示类型非法或显式 null。
func upstreamRawString(raw json.RawMessage) (string, bool) {
	if string(raw) == "null" {
		return "", false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}
	return value, true
}

// upstreamAccountID 解析路径 id：非法/非正数与不存在同形 404。
func upstreamAccountID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeAdminProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found", "")
		return 0, false
	}
	return id, true
}

// auditEvent 写账号审计（best-effort：日志库独立，失败只记日志）。
func (h *UpstreamAccountHandlers) auditEvent(r *http.Request, action string, actor int64, detail, before, after map[string]any, status int) {
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
		h.log.Warn("upstream account audit write failed", "action", action, "actor", actor, "error", err)
	}
}

func (h *UpstreamAccountHandlers) writeRepoError(w http.ResponseWriter, op string, err error) {
	switch {
	case errors.Is(err, store.ErrUpstreamAccountNotFound):
		writeAdminProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found", "")
	case errors.Is(err, store.ErrUpstreamAccountConflict):
		writeAdminProblem(w, http.StatusConflict, authProblemConflict, "Conflict",
			"the upstream account was modified by someone else; refresh and retry")
	case errors.Is(err, store.ErrUpstreamAccountInUse):
		writeAdminProblem(w, http.StatusConflict, authProblemConflict, "Conflict",
			"the upstream account is still referenced by channels or usage snapshots")
	case errors.Is(err, store.ErrNameConflict):
		// ErrNameConflict 是与渠道共享的哨兵（错误文本写着 channel name conflict）；
		// 不能透出它的文本，用账号口径的文案回答。
		writeAdminProblem(w, http.StatusConflict, authProblemConflict, "Conflict",
			"upstream account name already exists")
	default:
		h.log.Error(op+" failed", "error", err)
		writeAdminProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error", "")
	}
}
