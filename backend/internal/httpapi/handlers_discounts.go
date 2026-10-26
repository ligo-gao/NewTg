// 折扣规则管理端点（spec §7.3 / 审计 §9）。
//
// 端点面：
//
//	GET    /api/users/discount-models                     模型选择器（users:update）
//	GET    /api/users/{id}/discounts                      个人规则列表（users:update）
//	POST   /api/users/{id}/discounts                      新建（users:update）
//	PATCH  /api/users/{id}/discounts/{discount_id}        改比例/有效期/启用（users:update）
//	DELETE /api/users/{id}/discounts/{discount_id}        删除（users:update）
//	GET    /api/groups/discount-models                    模型选择器（groups:manage）
//	GET    /api/groups/{name}/discounts                   分组规则列表（groups:manage）
//	POST   /api/groups/{name}/discounts                   新建（groups:manage）
//	PATCH  /api/groups/{name}/discounts/{discount_id}     更新（groups:manage）
//	DELETE /api/groups/{name}/discounts/{discount_id}     删除（groups:manage）
//
// 安全口径：
//   - 用户折扣按路径 {id} 做数据级归属判定（HandlerWithTarget）；分组目标的
//     target_id 是字符串，TargetOwnerUserID 保持 0，只用功能级权限键；
//   - 每个写动作各自登记 BOPLA 字段白名单（创建与更新分开登记）；DELETE 无
//     请求体也登记空白名单——rbac 对「未登记动作」的写请求一律 fail-closed；
//   - PATCH 的 model_name 是**有意的双层**：字段白名单放行（否则中间件会先
//     返回 403 field_not_writable），处理器再显式返回 422 model_immutable。
//     两层缺一不可，见 plan R2；
//   - 折扣不存在、或 discount_id 属于别的目标，一律 404（不泄露存在性）；
//   - 审计 detail 只含规则事实（target_type/target_id/model_name/rate_percent/
//     valid_from/valid_until/enabled），绝不回写请求体原文。
package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
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
	// discountTargetUser / discountTargetGroup 是 discounts.target_type 的冻结枚举。
	discountTargetUser  = "user"
	discountTargetGroup = "group"

	// discountModelMaxRunes 与 OpenAPI DiscountRuleInput.model_name.maxLength 一致。
	discountModelMaxRunes = 200

	// 模型选择器的 limit 口径：缺省 50，越界钳到 [1,100]。
	discountPickerDefaultLimit = 50
	discountPickerMinLimit     = 1
	discountPickerMaxLimit     = 100

	// discountProblemModelImmutable 是 PATCH 携带 model_name 时的机器可读 code。
	discountProblemModelImmutable = "model_immutable"
)

// DiscountRepo 是折扣规则管理面消费的仓储能力（store.CustomerDiscountRepo 实现）。
type DiscountRepo interface {
	ListForTarget(ctx context.Context, targetType, targetID string) ([]store.CustomerDiscount, error)
	Get(ctx context.Context, id int64) (store.CustomerDiscount, bool, error)
	Create(ctx context.Context, in store.CustomerDiscountInput) (store.CustomerDiscount, error)
	Update(ctx context.Context, id int64, in store.CustomerDiscountInput) (store.CustomerDiscount, error)
	Delete(ctx context.Context, id int64) error
	ListEnabledModels(ctx context.Context, q string, limit int) ([]string, error)
	TargetExists(ctx context.Context, targetType, targetID string) (bool, error)
}

// DiscountHandlerConfig 是折扣端点的装配输入。Repo 与 DB 二选一；
// Audit 为 nil 时不写审计（未装配日志库的测试/开发环境）。
type DiscountHandlerConfig struct {
	DB     *sql.DB
	Repo   DiscountRepo
	Authz  *rbac.AuthzMiddleware
	Audit  *audit.Logger
	Logger *slog.Logger
}

// DiscountHandlers 持有折扣端点依赖，可并发使用。
type DiscountHandlers struct {
	repo  DiscountRepo
	authz *rbac.AuthzMiddleware
	audit *audit.Logger
	log   *slog.Logger
}

// 字段级（BOPLA）动作登记。ResourceKind 固定为 discount；ActionName 区分
// 目标类型（user/group）与动作（list/create/update/delete）。
var (
	discountUserListAction   = rbac.AuthzAction{ResourceKind: "discount", ActionName: "user_list"}
	discountUserCreateAction = rbac.AuthzAction{ResourceKind: "discount", ActionName: "user_create"}
	discountUserUpdateAction = rbac.AuthzAction{ResourceKind: "discount", ActionName: "user_update"}
	discountUserDeleteAction = rbac.AuthzAction{ResourceKind: "discount", ActionName: "user_delete"}

	discountGroupListAction   = rbac.AuthzAction{ResourceKind: "discount", ActionName: "group_list"}
	discountGroupCreateAction = rbac.AuthzAction{ResourceKind: "discount", ActionName: "group_create"}
	discountGroupUpdateAction = rbac.AuthzAction{ResourceKind: "discount", ActionName: "group_update"}
	discountGroupDeleteAction = rbac.AuthzAction{ResourceKind: "discount", ActionName: "group_delete"}
)

// NewDiscountHandlers 构造折扣端点。
func NewDiscountHandlers(cfg DiscountHandlerConfig) (*DiscountHandlers, error) {
	repo := cfg.Repo
	if repo == nil {
		if cfg.DB == nil {
			return nil, errors.New("httpapi: DiscountHandlerConfig.Repo or DB is required")
		}
		repo = store.NewCustomerDiscountRepo(cfg.DB)
	}
	authz := cfg.Authz
	if authz == nil {
		// 字段层是端点契约的一部分，不能因为装配方没启用管理面 RBAC 而失效。
		authz = rbac.NewAuthzMiddleware()
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	h := &DiscountHandlers{repo: repo, authz: authz, audit: cfg.Audit, log: log}
	h.registerPolicies()
	return h, nil
}

func (h *DiscountHandlers) registerPolicies() {
	writable := func() *rbac.FieldPolicy {
		return &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{
			// model_name 必须保留在白名单里：PATCH 的 422 model_immutable 由
			// 处理器负责，若在字段层就拦成 403 会与 spec 的 422 不符（plan R2）。
			"model_name":   nil,
			"rate_percent": nil,
			"valid_from":   nil,
			"valid_until":  nil,
			"enabled":      nil,
		}}
	}
	h.authz.SetWritePolicy(discountUserCreateAction, writable())
	h.authz.SetWritePolicy(discountUserUpdateAction, writable())
	h.authz.SetWritePolicy(discountGroupCreateAction, writable())
	h.authz.SetWritePolicy(discountGroupUpdateAction, writable())

	// 无请求体的动作登记空白名单：带任何字段的 body 都会被字段层拒绝，
	// 空对象 {} 则等价于无 body。未登记的动作对 DELETE 也会 fail-closed。
	empty := func() *rbac.FieldPolicy {
		return &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{}}
	}
	h.authz.SetWritePolicy(discountUserDeleteAction, empty())
	h.authz.SetWritePolicy(discountGroupDeleteAction, empty())
}

// RegisterDiscountRoutes 注册本任务全部折扣端点。
//
// wrap 语义同 RegisterUserRoutes：nil 时假定 mux 已经在 SessionAuth（及
// CSRF/Origin）之后。用户折扣 ID 寻址的归属由 PathValue("id") 在路由匹配
// 后解析（rbac.HandlerWithTarget）；分组目标的 target_id 是字符串，归属保持 0。
func (h *DiscountHandlers) RegisterDiscountRoutes(mux *http.ServeMux, wrap func(http.Handler) http.Handler) {
	mount := func(pattern string, handler http.Handler) {
		if wrap != nil {
			handler = wrap(handler)
		}
		mux.Handle(pattern, handler)
	}
	fixed := func(required string, action rbac.AuthzAction, next http.HandlerFunc) http.Handler {
		return h.authz.Handler(required, action)(next)
	}
	byUser := func(required string, action rbac.AuthzAction, next http.HandlerFunc) http.Handler {
		return h.authz.HandlerWithTarget(required, action, adminTargetUserID)(next)
	}

	// 字面量段 discount-models 比通配段 {id} 更具体，Go 1.22 ServeMux 不会冲突。
	mount("GET /api/users/discount-models",
		fixed(rbac.PermUsersUpdate, discountUserListAction, h.handleListUserDiscountModels))
	mount("GET /api/users/{id}/discounts",
		byUser(rbac.PermUsersUpdate, discountUserListAction, h.handleListUserDiscounts))
	mount("POST /api/users/{id}/discounts",
		byUser(rbac.PermUsersUpdate, discountUserCreateAction, h.handleCreateUserDiscount))
	mount("PATCH /api/users/{id}/discounts/{discount_id}",
		byUser(rbac.PermUsersUpdate, discountUserUpdateAction, h.handleUpdateUserDiscount))
	mount("DELETE /api/users/{id}/discounts/{discount_id}",
		byUser(rbac.PermUsersUpdate, discountUserDeleteAction, h.handleDeleteUserDiscount))

	mount("GET /api/groups/discount-models",
		fixed(rbac.PermGroupsManage, discountGroupListAction, h.handleListGroupDiscountModels))
	mount("GET /api/groups/{name}/discounts",
		fixed(rbac.PermGroupsManage, discountGroupListAction, h.handleListGroupDiscounts))
	mount("POST /api/groups/{name}/discounts",
		fixed(rbac.PermGroupsManage, discountGroupCreateAction, h.handleCreateGroupDiscount))
	mount("PATCH /api/groups/{name}/discounts/{discount_id}",
		fixed(rbac.PermGroupsManage, discountGroupUpdateAction, h.handleUpdateGroupDiscount))
	mount("DELETE /api/groups/{name}/discounts/{discount_id}",
		fixed(rbac.PermGroupsManage, discountGroupDeleteAction, h.handleDeleteGroupDiscount))
}

// ---------------------------------------------------------------------------
// 视图
// ---------------------------------------------------------------------------

// discountRuleView 是折扣规则的冻结投影；时间是 UTC 的 RFC3339 或 null。
type discountRuleView struct {
	ID          int64   `json:"id"`
	ModelName   string  `json:"model_name"`
	RatePercent int16   `json:"rate_percent"`
	ValidFrom   *string `json:"valid_from"`
	ValidUntil  *string `json:"valid_until"`
	Enabled     bool    `json:"enabled"`
	CreatedAt   string  `json:"created_at"`
	UpdatedAt   string  `json:"updated_at"`
}

type discountRuleListView struct {
	Items []discountRuleView `json:"items"`
}

type discountModelOption struct {
	ModelName string `json:"model_name"`
}

type discountModelListView struct {
	Items []discountModelOption `json:"items"`
}

func newDiscountRuleView(d store.CustomerDiscount) discountRuleView {
	return discountRuleView{
		ID:          d.ID,
		ModelName:   d.ModelName,
		RatePercent: d.RatePercent,
		ValidFrom:   formatDiscountTime(d.ValidFrom),
		ValidUntil:  formatDiscountTime(d.ValidUntil),
		Enabled:     d.Enabled,
		CreatedAt:   d.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:   d.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

func formatDiscountTime(t *time.Time) *string {
	if t == nil {
		return nil
	}
	formatted := t.UTC().Format(time.RFC3339)
	return &formatted
}

// ---------------------------------------------------------------------------
// 请求体
// ---------------------------------------------------------------------------

// discountRuleRequest 是 POST/PATCH 共用的请求体。指针字段用于区分「缺省」
// 与「显式值」；PATCH 的 model_name 出现即 422（由原始键判断存在性）。
type discountRuleRequest struct {
	ModelName   *string `json:"model_name"`
	RatePercent *int16  `json:"rate_percent"`
	ValidFrom   *string `json:"valid_from"`
	ValidUntil  *string `json:"valid_until"`
	Enabled     *bool   `json:"enabled"`
}

// parseDiscountTime 把 RFC3339 字符串解析为 UTC 时刻；nil/空串表示无限制。
func parseDiscountTime(raw *string) (*time.Time, error) {
	if raw == nil {
		return nil, nil
	}
	value := strings.TrimSpace(*raw)
	if value == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return nil, err
	}
	utc := parsed.UTC()
	return &utc, nil
}

// validateDiscountWindow 校验左闭右开区间；两端都非空时必须 from < until。
func validateDiscountWindow(from, until *time.Time) bool {
	return from == nil || until == nil || from.Before(*until)
}

func writeDiscountValidationProblem(w http.ResponseWriter, fields []string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(http.StatusUnprocessableEntity)
	problem := dto.Problem{
		Type:   problemTypeValidationFailed,
		Title:  "Validation failed",
		Status: http.StatusUnprocessableEntity,
	}
	problem.Set("fields", fields)
	_ = json.NewEncoder(w).Encode(problem)
}

// writeDiscountModelImmutable 是 R2 的显式 422：PATCH 不允许改模型名。
func writeDiscountModelImmutable(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(http.StatusUnprocessableEntity)
	problem := dto.Problem{
		Type:   problemTypeValidationFailed,
		Title:  "Validation failed",
		Status: http.StatusUnprocessableEntity,
	}
	problem.Set("fields", []string{"model_name"})
	problem.Set("code", discountProblemModelImmutable)
	_ = json.NewEncoder(w).Encode(problem)
}

// ---------------------------------------------------------------------------
// 目标解析
// ---------------------------------------------------------------------------

func discountUserTarget(r *http.Request) (string, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return "", false
	}
	return strconv.FormatInt(id, 10), true
}

func discountGroupTarget(r *http.Request) (string, bool) {
	name := r.PathValue("name")
	if name == "" {
		return "", false
	}
	return name, true
}

func discountPathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("discount_id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// ---------------------------------------------------------------------------
// handlers
// ---------------------------------------------------------------------------

func (h *DiscountHandlers) handleListUserDiscounts(w http.ResponseWriter, r *http.Request) {
	h.listDiscounts(w, r, discountTargetUser, discountUserTarget)
}

func (h *DiscountHandlers) handleListGroupDiscounts(w http.ResponseWriter, r *http.Request) {
	h.listDiscounts(w, r, discountTargetGroup, discountGroupTarget)
}

func (h *DiscountHandlers) listDiscounts(w http.ResponseWriter, r *http.Request, targetType string, resolve func(*http.Request) (string, bool)) {
	targetID, ok := resolve(r)
	if !ok {
		writeAdminProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found", "")
		return
	}
	rows, err := h.repo.ListForTarget(r.Context(), targetType, targetID)
	if err != nil {
		h.writeRepoError(w, "list discounts", err)
		return
	}
	items := make([]discountRuleView, 0, len(rows))
	for _, row := range rows {
		items = append(items, newDiscountRuleView(row))
	}
	writeJSON(w, http.StatusOK, discountRuleListView{Items: items})
}

func (h *DiscountHandlers) handleCreateUserDiscount(w http.ResponseWriter, r *http.Request) {
	h.createDiscount(w, r, discountTargetUser, discountUserTarget, audit.ActionUserDiscountSet)
}

func (h *DiscountHandlers) handleCreateGroupDiscount(w http.ResponseWriter, r *http.Request) {
	h.createDiscount(w, r, discountTargetGroup, discountGroupTarget, audit.ActionGroupDiscountSet)
}

func (h *DiscountHandlers) createDiscount(w http.ResponseWriter, r *http.Request, targetType string, resolve func(*http.Request) (string, bool), auditAction string) {
	p, ok := adminPrincipal(w, r)
	if !ok {
		return
	}
	targetID, ok := resolve(r)
	if !ok {
		writeAdminProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found", "")
		return
	}

	var req discountRuleRequest
	if err := readAuthJSON(r, &req); err != nil {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"request body must be a JSON object")
		return
	}
	modelName := ""
	if req.ModelName != nil {
		modelName = strings.TrimSpace(*req.ModelName)
	}
	if modelName == "" || utf8.RuneCountInString(modelName) > discountModelMaxRunes {
		writeDiscountValidationProblem(w, []string{"model_name"})
		return
	}
	if req.RatePercent == nil || *req.RatePercent < 0 || *req.RatePercent > 100 {
		writeDiscountValidationProblem(w, []string{"rate_percent"})
		return
	}
	validFrom, err := parseDiscountTime(req.ValidFrom)
	if err != nil {
		writeDiscountValidationProblem(w, []string{"valid_from"})
		return
	}
	validUntil, err := parseDiscountTime(req.ValidUntil)
	if err != nil {
		writeDiscountValidationProblem(w, []string{"valid_until"})
		return
	}
	if !validateDiscountWindow(validFrom, validUntil) {
		writeDiscountValidationProblem(w, []string{"valid_until"})
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	exists, err := h.repo.TargetExists(r.Context(), targetType, targetID)
	if err != nil {
		h.writeRepoError(w, "check discount target", err)
		return
	}
	if !exists {
		writeAdminProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found", "")
		return
	}

	d, err := h.repo.Create(r.Context(), store.CustomerDiscountInput{
		ModelName:   modelName,
		TargetType:  targetType,
		TargetID:    targetID,
		RatePercent: *req.RatePercent,
		ValidFrom:   validFrom,
		ValidUntil:  validUntil,
		Enabled:     enabled,
	})
	if err != nil {
		h.writeRepoError(w, "create discount", err)
		return
	}
	h.auditEvent(r, auditAction, p.UserID, discountAuditDetail(d), http.StatusCreated)
	writeJSON(w, http.StatusCreated, newDiscountRuleView(d))
}

func (h *DiscountHandlers) handleUpdateUserDiscount(w http.ResponseWriter, r *http.Request) {
	h.updateDiscount(w, r, discountTargetUser, discountUserTarget, audit.ActionUserDiscountSet)
}

func (h *DiscountHandlers) handleUpdateGroupDiscount(w http.ResponseWriter, r *http.Request) {
	h.updateDiscount(w, r, discountTargetGroup, discountGroupTarget, audit.ActionGroupDiscountSet)
}

func (h *DiscountHandlers) updateDiscount(w http.ResponseWriter, r *http.Request, targetType string, resolve func(*http.Request) (string, bool), auditAction string) {
	p, ok := adminPrincipal(w, r)
	if !ok {
		return
	}
	targetID, ok := resolve(r)
	if !ok {
		writeAdminProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found", "")
		return
	}
	id, ok := discountPathID(r)
	if !ok {
		writeAdminProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found", "")
		return
	}
	existing, ok := h.loadOwnedDiscount(w, r, id, targetType, targetID)
	if !ok {
		return
	}

	body, err := readRawObject(r)
	if err != nil {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"request body must be a JSON object")
		return
	}
	// R2：model_name 出现即 422（显式 null 也算出现），不静默忽略。
	if _, present := body["model_name"]; present {
		writeDiscountModelImmutable(w)
		return
	}

	rawBody, err := json.Marshal(body)
	if err != nil {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"request body must be a JSON object")
		return
	}
	var req discountRuleRequest
	if err := json.Unmarshal(rawBody, &req); err != nil {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"request body must be a JSON object")
		return
	}

	in := store.CustomerDiscountInput{
		// model_name / target 不可经 PATCH 修改，始终沿用已存行的值。
		ModelName:   existing.ModelName,
		TargetType:  targetType,
		TargetID:    targetID,
		RatePercent: existing.RatePercent,
		ValidFrom:   existing.ValidFrom,
		ValidUntil:  existing.ValidUntil,
		Enabled:     existing.Enabled,
	}
	if _, present := body["rate_percent"]; present {
		if req.RatePercent == nil || *req.RatePercent < 0 || *req.RatePercent > 100 {
			writeDiscountValidationProblem(w, []string{"rate_percent"})
			return
		}
		in.RatePercent = *req.RatePercent
	}
	if _, present := body["enabled"]; present {
		if req.Enabled == nil {
			writeDiscountValidationProblem(w, []string{"enabled"})
			return
		}
		in.Enabled = *req.Enabled
	}
	if _, present := body["valid_from"]; present {
		parsed, err := parseDiscountTime(req.ValidFrom)
		if err != nil {
			writeDiscountValidationProblem(w, []string{"valid_from"})
			return
		}
		in.ValidFrom = parsed
	}
	if _, present := body["valid_until"]; present {
		parsed, err := parseDiscountTime(req.ValidUntil)
		if err != nil {
			writeDiscountValidationProblem(w, []string{"valid_until"})
			return
		}
		in.ValidUntil = parsed
	}
	if !validateDiscountWindow(in.ValidFrom, in.ValidUntil) {
		writeDiscountValidationProblem(w, []string{"valid_until"})
		return
	}

	d, err := h.repo.Update(r.Context(), id, in)
	if err != nil {
		h.writeRepoError(w, "update discount", err)
		return
	}
	h.auditEvent(r, auditAction, p.UserID, discountAuditDetail(d), http.StatusOK)
	writeJSON(w, http.StatusOK, newDiscountRuleView(d))
}

func (h *DiscountHandlers) handleDeleteUserDiscount(w http.ResponseWriter, r *http.Request) {
	h.deleteDiscount(w, r, discountTargetUser, discountUserTarget, audit.ActionUserDiscountDelete)
}

func (h *DiscountHandlers) handleDeleteGroupDiscount(w http.ResponseWriter, r *http.Request) {
	h.deleteDiscount(w, r, discountTargetGroup, discountGroupTarget, audit.ActionGroupDiscountDelete)
}

func (h *DiscountHandlers) deleteDiscount(w http.ResponseWriter, r *http.Request, targetType string, resolve func(*http.Request) (string, bool), auditAction string) {
	p, ok := adminPrincipal(w, r)
	if !ok {
		return
	}
	targetID, ok := resolve(r)
	if !ok {
		writeAdminProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found", "")
		return
	}
	id, ok := discountPathID(r)
	if !ok {
		writeAdminProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found", "")
		return
	}
	existing, ok := h.loadOwnedDiscount(w, r, id, targetType, targetID)
	if !ok {
		return
	}
	if err := h.repo.Delete(r.Context(), id); err != nil {
		h.writeRepoError(w, "delete discount", err)
		return
	}
	h.auditEvent(r, auditAction, p.UserID, discountAuditDetail(existing), http.StatusNoContent)
	w.WriteHeader(http.StatusNoContent)
}

// loadOwnedDiscount 读取 discount_id 并把「不存在」「不属于路径目标」都映射为
// 404。仓储在行不存在时同时返回 ok=false 与 ErrDiscountNotFound，两者都必须
// 走 404，不能因为 err 非 nil 落到 500。
func (h *DiscountHandlers) loadOwnedDiscount(w http.ResponseWriter, r *http.Request, id int64, targetType, targetID string) (store.CustomerDiscount, bool) {
	d, ok, err := h.repo.Get(r.Context(), id)
	if errors.Is(err, store.ErrDiscountNotFound) || (!ok && err == nil) {
		writeAdminProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found", "")
		return store.CustomerDiscount{}, false
	}
	if err != nil {
		h.writeRepoError(w, "read discount", err)
		return store.CustomerDiscount{}, false
	}
	if !ok || d.TargetType != targetType || d.TargetID != targetID {
		writeAdminProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found", "")
		return store.CustomerDiscount{}, false
	}
	return d, true
}

// ---------------------------------------------------------------------------
// 模型选择器
// ---------------------------------------------------------------------------

func (h *DiscountHandlers) handleListUserDiscountModels(w http.ResponseWriter, r *http.Request) {
	h.listDiscountModels(w, r)
}

func (h *DiscountHandlers) handleListGroupDiscountModels(w http.ResponseWriter, r *http.Request) {
	h.listDiscountModels(w, r)
}

func (h *DiscountHandlers) listDiscountModels(w http.ResponseWriter, r *http.Request) {
	// q 原样透传：仓储内部已 TrimSpace 并转义 LIKE 通配符。
	query := r.URL.Query().Get("q")
	limit := discountPickerDefaultLimit
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
				"limit must be an integer")
			return
		}
		if parsed < discountPickerMinLimit {
			parsed = discountPickerMinLimit
		}
		if parsed > discountPickerMaxLimit {
			parsed = discountPickerMaxLimit
		}
		limit = parsed
	}
	models, err := h.repo.ListEnabledModels(r.Context(), query, limit)
	if err != nil {
		h.writeRepoError(w, "list discount models", err)
		return
	}
	items := make([]discountModelOption, 0, len(models))
	for _, model := range models {
		items = append(items, discountModelOption{ModelName: model})
	}
	writeJSON(w, http.StatusOK, discountModelListView{Items: items})
}

// ---------------------------------------------------------------------------
// 审计 / 错误映射
// ---------------------------------------------------------------------------

// discountAuditDetail 只输出规则事实。绝不回写请求体原文（PATCH 未来若增加
// 字段，不会被顺带落库）。
func discountAuditDetail(d store.CustomerDiscount) map[string]any {
	return map[string]any{
		"target_type":  d.TargetType,
		"target_id":    d.TargetID,
		"model_name":   d.ModelName,
		"rate_percent": d.RatePercent,
		"valid_from":   discountAuditTime(d.ValidFrom),
		"valid_until":  discountAuditTime(d.ValidUntil),
		"enabled":      d.Enabled,
	}
}

func discountAuditTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}

// auditEvent 写折扣审计（best-effort：日志库独立，失败只记日志）。
func (h *DiscountHandlers) auditEvent(r *http.Request, action string, actor int64, detail map[string]any, status int) {
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
	}
	if err := writeAuditEvent(r.Context(), h.audit, event); err != nil {
		h.log.Warn("admin discount audit write failed", "action", action, "actor", actor, "error", err)
	}
}

func (h *DiscountHandlers) writeRepoError(w http.ResponseWriter, op string, err error) {
	switch {
	case errors.Is(err, store.ErrDiscountNotFound):
		writeAdminProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found", "")
	case errors.Is(err, store.ErrDiscountDuplicate):
		writeAdminProblem(w, http.StatusConflict, authProblemConflict, "Conflict",
			"a discount rule for this model already exists for the target")
	default:
		h.log.Error(op+" failed", "error", err)
		writeAdminProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error", "")
	}
}
