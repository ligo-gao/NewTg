// 本文件实现 API 令牌管理端点（modules-02 F-02-01..17 的 M1 后端面）。
//
// 路由挂载：由 routes.go 的装配方调用 RegisterTokenRoutes(mux, wrap)。
// wrap 应为 SessionAuth（Cookie/Bearer 双路径）；Cookie 路径的 CSRF/Origin
// 校验由既有中间件链负责。**唯一的例外**是 GET /api/usage/token：它必须绕过
// SessionAuth 自行解析 Bearer，原因是该端点对「额度耗尽」的令牌仍须返回 200
// （modules-02 §2.13/I-29），而 SessionAuth 会按 tokenUsable 拒绝耗尽令牌；
// 同时它需要区分 token_disabled/token_expired 的错误码。
package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/configcenter"
	"newtg/gateway/internal/tokenstore"
)

// 冻结的 RFC 7807 type URI（沿用 https://newtg.dev/problems/<slug> 约定）。
const (
	problemTypeInvalidRequest    = "https://newtg.dev/problems/invalid_request"
	problemTypeNotFoundToken     = "https://newtg.dev/problems/not_found"
	problemTypeConflictToken     = "https://newtg.dev/problems/conflict"
	problemTypeInvalidToken      = "https://newtg.dev/problems/invalid_token"
	problemTypeTokenDisabled     = "https://newtg.dev/problems/token_disabled"
	problemTypeTokenExpired      = "https://newtg.dev/problems/token_expired"
	problemTypeUnavailable       = "https://newtg.dev/problems/service_unavailable"
	problemTypeIdempotencyReused = "https://newtg.dev/problems/idempotency_key_reuse"
)

const (
	tokenIdempotencyTTL   = 10 * time.Minute
	tokenIdempotencyLease = 10 * time.Second
	tokenMaxRequestBody   = 1 << 20 // 1 MiB

	// tokenPageMax 与管理员列表的 adminPageMax 同风格：防止 p 取超大值导致
	// OFFSET 溢出（int 乘法变负）或全表扫描放大。用户令牌量远低于此值。
	tokenPageMax = 100_000
)

// 令牌列表过滤参数的合法枚举（与 api/openapi/paths/user.yaml 的 enum 逐字一致）。
// 非法值必须 400，不能静默忽略后返回全量——那会让前端过滤条件失效却无感知。
var (
	tokenListStatuses = map[string]struct{}{
		"enabled": {}, "disabled": {}, "expired": {}, "exhausted": {},
	}
	tokenListQuotas = map[string]struct{}{
		"normal": {}, "low": {}, "exhausted": {}, "unlimited": {},
	}
)

// TokenService 是 HTTP 层所需的令牌能力。*tokenstore.Store 满足；单测用 fake，
// 使未打 integration 标签的 go test ./... 不依赖 PG/Redis（GC-8）。
type TokenService interface {
	Create(ctx context.Context, userID int64, in tokenstore.CreateInput) ([]tokenstore.Created, error)
	List(ctx context.Context, userID int64, filter tokenstore.ListFilter) ([]*tokenstore.Token, int64, error)
	Get(ctx context.Context, userID, id int64) (*tokenstore.Token, error)
	Update(ctx context.Context, userID, id int64, in tokenstore.UpdateInput) (*tokenstore.Token, error)
	Delete(ctx context.Context, userID, id int64, expectedUpdatedAt *time.Time) error
	Enable(ctx context.Context, userID, id int64) (*tokenstore.Token, error)
	Disable(ctx context.Context, userID, id int64) (*tokenstore.Token, error)
	ResetKey(ctx context.Context, userID, id int64) (plain, masked string, token *tokenstore.Token, err error)
	BatchEnable(ctx context.Context, userID int64, ids []int64) (int, []tokenstore.BatchFailure, error)
	BatchDisable(ctx context.Context, userID int64, ids []int64) (int, []tokenstore.BatchFailure, error)
	BatchDelete(ctx context.Context, userID int64, ids []int64) (int, []tokenstore.BatchFailure, error)
	AutoGroups(ctx context.Context, userID int64) ([]tokenstore.AutoGroup, int, error)
	Resolve(ctx context.Context, keyHash string) (*auth.TokenMeta, error)
	CrossGroupRetryMaxGroups() int
}

// 编译期钉死具体实现与 HTTP 接缝：Store 签名漂移在这里失败，而不是装配期。
var _ TokenService = (*tokenstore.Store)(nil)

// TokenHandlers 持有令牌端点依赖。可并发使用。
type TokenHandlers struct {
	store  TokenService
	audit  *audit.Logger
	rdb    *redis.Client
	sealer configcenter.SecretSealer
	now    func() time.Time
}

// NewTokenHandlers 构造令牌端点。auditLog 可为 nil（禁用审计，仅测试）；
// rdb 可为 nil（禁用幂等键去重，仅测试）。
//
// sealer 用于加密幂等缓存体（缓存里含一次性明文 key）：nil 时**完全跳过**
// 幂等缓存，绝不允许回落到明文落 Redis。生产装配必须传 CREDENTIAL_ENCRYPTION_KEY
// 派生的 auth.SecretCipher。
func NewTokenHandlers(store TokenService, auditLog *audit.Logger, rdb *redis.Client, sealer configcenter.SecretSealer) *TokenHandlers {
	return &TokenHandlers{store: store, audit: auditLog, rdb: rdb, sealer: sealer, now: time.Now}
}

// RegisterTokenRoutes 把令牌端点挂到 mux。
//
// wrap 应为 SessionAuth 中间件（可再链 CSRF）；为 nil 时直接挂载，handler
// 仍要求 request.Context 里已有 Principal，否则 401。
//
// GET /api/usage/token 刻意不使用 wrap：它自带 Bearer 解析（见文件头注释）。
//
// 与 routes.go 的 Router 集成时推荐分开挂载：
//
//	h.RegisterTokenCRUDRoutes(rt.protected, nil) // 外层 Protect 已统一鉴权
//	// usage 必须挂到根 mux（精确模式优先于 /api/ 子树）：
//	h.RegisterTokenUsageRoute(rootMux)
//
// 若调用方把本函数挂到受保护子树，usage 会被 SessionAuth 拒绝耗尽令牌，
// 与 modules-02 §2.13 冲突——这是本函数只应作用于根 mux 的原因。
func (h *TokenHandlers) RegisterTokenRoutes(mux *http.ServeMux, wrap func(http.Handler) http.Handler) {
	h.RegisterTokenCRUDRoutes(mux, wrap)
	h.RegisterTokenUsageRoute(mux)
}

// RegisterTokenCRUDRoutes 只挂管理面 CRUD/批量/自动分组端点（全部需要身份）。
func (h *TokenHandlers) RegisterTokenCRUDRoutes(mux *http.ServeMux, wrap func(http.Handler) http.Handler) {
	route := func(pattern string, fn http.HandlerFunc) {
		if wrap != nil {
			mux.Handle(pattern, wrap(fn))
			return
		}
		mux.Handle(pattern, fn)
	}

	route("GET /api/tokens", h.listTokens)
	route("POST /api/tokens", h.idempotent("create", h.createTokens))
	route("GET /api/token/auto-groups", h.autoGroups)

	route("GET /api/tokens/{id}", h.getToken)
	route("PATCH /api/tokens/{id}", h.idempotent("update", h.updateToken))
	route("DELETE /api/tokens/{id}", h.deleteToken)

	route("POST /api/tokens:batch-enable", h.idempotent("batch-enable", h.batchEnable))
	route("POST /api/tokens:batch-disable", h.idempotent("batch-disable", h.batchDisable))
	route("POST /api/tokens:batch-delete", h.idempotent("batch-delete", h.batchDelete))

	// Go 1.22 ServeMux 不允许 {id} 与字面量共存于同一段，{id}:enable 无法用
	// 单条模式表达；用子树通配在 handler 内解析 "<id>:<action>"。
	route("POST /api/tokens/{rest...}", h.tokenAction)
}

// RegisterTokenUsageRoute 挂载令牌自读用量端点（**不挂 SessionAuth**）。
func (h *TokenHandlers) RegisterTokenUsageRoute(mux *http.ServeMux) {
	mux.Handle("GET /api/usage/token", http.HandlerFunc(h.tokenUsage))
}

// ---------------------------------------------------------------------------
// DTO
// ---------------------------------------------------------------------------

// tokenExpiresAt 同时接受 epoch 秒（modules-02 的线格式）与 RFC 3339 字符串；
// 响应统一输出 time.Time 的 ISO 8601。Present 区分「未提交」与「显式 null」。
type tokenExpiresAt struct {
	Present bool
	Time    *time.Time
}

func (t *tokenExpiresAt) UnmarshalJSON(data []byte) error {
	t.Present = true
	raw := strings.TrimSpace(string(data))
	if raw == "null" {
		t.Time = nil
		return nil
	}
	if strings.HasPrefix(raw, `"`) {
		var parsed time.Time
		if err := json.Unmarshal(data, &parsed); err != nil {
			return err
		}
		t.Time = &parsed
		return nil
	}
	var epoch int64
	if err := json.Unmarshal(data, &epoch); err != nil {
		return err
	}
	parsed := time.Unix(epoch, 0).UTC()
	t.Time = &parsed
	return nil
}

type createTokenRequest struct {
	Name            string         `json:"name"`
	Group           string         `json:"group"`
	ExpiresAt       tokenExpiresAt `json:"expires_at"`
	Quantity        *int           `json:"quantity"`
	UnlimitedQuota  *bool          `json:"unlimited_quota"`
	RemainQuota     *int64         `json:"remain_quota"`
	ModelLimits     []string       `json:"model_limits"`
	AllowIPs        []string       `json:"allow_ips"`
	AutoGroups      []string       `json:"auto_groups"`
	CrossGroupRetry bool           `json:"cross_group_retry"`
}

type updateTokenRequest struct {
	Name            *string        `json:"name"`
	Group           *string        `json:"group"`
	ExpiresAt       tokenExpiresAt `json:"expires_at"`
	UnlimitedQuota  *bool          `json:"unlimited_quota"`
	RemainQuota     *int64         `json:"remain_quota"`
	ModelLimits     *[]string      `json:"model_limits"`
	AllowIPs        *[]string      `json:"allow_ips"`
	AutoGroups      *[]string      `json:"auto_groups"`
	CrossGroupRetry *bool          `json:"cross_group_retry"`
	UpdatedAt       *time.Time     `json:"updated_at"`
}

type batchTokenRequest struct {
	IDs []int64 `json:"ids"`
}

// tokenView 是管理面令牌响应；永不包含 key 明文或 key_hash（GC-6/GC-9）。
type tokenView struct {
	ID              int64      `json:"id"`
	Name            string     `json:"name"`
	Status          string     `json:"status"`
	KeyMasked       string     `json:"key_masked"`
	RemainQuota     *int64     `json:"remain_quota"`
	UnlimitedQuota  bool       `json:"unlimited_quota"`
	QuotaPercent    *int       `json:"quota_percent"`
	Group           string     `json:"group"`
	ModelLimits     []string   `json:"model_limits"`
	AllowIPs        []string   `json:"allow_ips"`
	AutoGroups      []string   `json:"auto_groups"`
	CrossGroupRetry bool       `json:"cross_group_retry"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
	LastUsedAt      *time.Time `json:"last_used_at"`
	ExpiresAt       *time.Time `json:"expires_at"`
}

type createdTokenView struct {
	tokenView
	Key string `json:"key"` // 仅创建/重置响应出现一次
}

type listTokensResponse struct {
	Items    []tokenView `json:"items"`
	Total    int64       `json:"total"`
	Page     int         `json:"page"`
	PageSize int         `json:"page_size"`
}

type tokenBatchFailureView struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Detail string `json:"detail"`
}

type tokenBatchResponse struct {
	Succeeded int                     `json:"succeeded"`
	Failed    []tokenBatchFailureView `json:"failed"`
}

type tokenAutoGroupsResponse struct {
	Groups   []tokenAutoGroupView `json:"groups"`
	MaxCount int                  `json:"max_count"`
}

type tokenAutoGroupView struct {
	Name  string  `json:"name"`
	Ratio float64 `json:"ratio,omitempty"`
}

type tokenUsageResponse struct {
	Object             string     `json:"object"`
	Name               string     `json:"name"`
	TotalGranted       *int64     `json:"total_granted"`
	TotalUsed          int64      `json:"total_used"`
	TotalAvailable     *int64     `json:"total_available"`
	UnlimitedQuota     bool       `json:"unlimited_quota"`
	ModelLimits        []string   `json:"model_limits"`
	ModelLimitsEnabled bool       `json:"model_limits_enabled"`
	ExpiresAt          *time.Time `json:"expires_at"`
}

// ---------------------------------------------------------------------------
// handlers
// ---------------------------------------------------------------------------

func (h *TokenHandlers) createTokens(w http.ResponseWriter, r *http.Request) {
	userID, ok := tokenPrincipal(r)
	if !ok {
		writeTokenProblem(w, http.StatusUnauthorized, problemTypeUnauthorized, "Unauthorized", "", "")
		return
	}
	var req createTokenRequest
	if !decodeTokenJSON(w, r, &req) {
		return
	}

	quantity := 1
	if req.Quantity != nil {
		quantity = *req.Quantity
	}
	if quantity < 1 || quantity > tokenstore.MaxBatchIDs {
		writeTokenProblem(w, http.StatusBadRequest, problemTypeInvalidRequest, "Invalid request",
			fmt.Sprintf("quantity 必须在 1..%d 之间", tokenstore.MaxBatchIDs), "quantity")
		return
	}
	unlimited := true
	if req.UnlimitedQuota != nil {
		unlimited = *req.UnlimitedQuota
	}
	remain := int64(100000) // DB 默认档（platform-01 §2）
	if req.RemainQuota != nil {
		remain = *req.RemainQuota
	}

	created, err := h.store.Create(r.Context(), userID, tokenstore.CreateInput{
		Name:            req.Name,
		Group:           req.Group,
		ExpiresAt:       req.ExpiresAt.Time,
		Quantity:        quantity,
		UnlimitedQuota:  unlimited,
		RemainQuota:     remain,
		ModelLimits:     req.ModelLimits,
		AllowIPs:        req.AllowIPs,
		AutoGroups:      req.AutoGroups,
		CrossGroupRetry: req.CrossGroupRetry,
	})
	if err != nil {
		writeTokenServiceError(w, err)
		return
	}

	views := make([]createdTokenView, 0, len(created))
	for _, item := range created {
		views = append(views, createdTokenView{tokenView: h.view(item.Token), Key: item.Key})
		h.auditEvent(r, audit.ActionTokenCreate, userID,
			map[string]any{"token_id": item.Token.ID}, nil, tokenSnapshot(item.Token))
	}
	writeTokenJSON(w, http.StatusOK, map[string]any{"created": views})
}

func (h *TokenHandlers) listTokens(w http.ResponseWriter, r *http.Request) {
	userID, ok := tokenPrincipal(r)
	if !ok {
		writeTokenProblem(w, http.StatusUnauthorized, problemTypeUnauthorized, "Unauthorized", "", "")
		return
	}
	page, err := tokenQueryInt(r, "p", 1)
	if err != nil {
		writeTokenProblem(w, http.StatusBadRequest, problemTypeInvalidRequest, "Invalid request", err.Error(), "p")
		return
	}
	if page > tokenPageMax {
		writeTokenProblem(w, http.StatusBadRequest, problemTypeInvalidRequest, "Invalid request",
			fmt.Sprintf("p 必须在 1..%d 之间", tokenPageMax), "p")
		return
	}
	if page < 1 {
		page = 1
	}
	pageSize, err := tokenQueryInt(r, "page_size", 20)
	if err != nil {
		writeTokenProblem(w, http.StatusBadRequest, problemTypeInvalidRequest, "Invalid request", err.Error(), "page_size")
		return
	}
	if pageSize < 1 || pageSize > 100 {
		writeTokenProblem(w, http.StatusBadRequest, problemTypeInvalidRequest, "Invalid request",
			"page_size 必须在 1..100 之间", "page_size")
		return
	}
	status := r.URL.Query().Get("status")
	if status != "" {
		if _, ok := tokenListStatuses[status]; !ok {
			writeTokenProblem(w, http.StatusBadRequest, problemTypeInvalidRequest, "Invalid request",
				"status 必须是 enabled/disabled/expired/exhausted", "status")
			return
		}
	}
	quota := r.URL.Query().Get("quota")
	if quota != "" {
		if _, ok := tokenListQuotas[quota]; !ok {
			writeTokenProblem(w, http.StatusBadRequest, problemTypeInvalidRequest, "Invalid request",
				"quota 必须是 normal/low/exhausted/unlimited", "quota")
			return
		}
	}

	items, total, err := h.store.List(r.Context(), userID, tokenstore.ListFilter{
		Keyword:  r.URL.Query().Get("keyword"),
		Status:   status,
		Group:    r.URL.Query().Get("group"),
		Quota:    quota,
		Page:     page,
		PageSize: pageSize,
	})
	if err != nil {
		writeTokenServiceError(w, err)
		return
	}
	views := make([]tokenView, 0, len(items))
	for _, item := range items {
		views = append(views, h.view(item))
	}
	writeTokenJSON(w, http.StatusOK, listTokensResponse{Items: views, Total: total, Page: page, PageSize: pageSize})
}

func (h *TokenHandlers) getToken(w http.ResponseWriter, r *http.Request) {
	userID, ok := tokenPrincipal(r)
	if !ok {
		writeTokenProblem(w, http.StatusUnauthorized, problemTypeUnauthorized, "Unauthorized", "", "")
		return
	}
	id, ok := tokenPathID(w, r)
	if !ok {
		return
	}
	token, err := h.store.Get(r.Context(), userID, id)
	if err != nil {
		writeTokenServiceError(w, err)
		return
	}
	writeTokenJSON(w, http.StatusOK, h.view(token))
}

func (h *TokenHandlers) updateToken(w http.ResponseWriter, r *http.Request) {
	userID, ok := tokenPrincipal(r)
	if !ok {
		writeTokenProblem(w, http.StatusUnauthorized, problemTypeUnauthorized, "Unauthorized", "", "")
		return
	}
	id, ok := tokenPathID(w, r)
	if !ok {
		return
	}
	var req updateTokenRequest
	if !decodeTokenJSON(w, r, &req) {
		return
	}
	if req.UpdatedAt == nil || req.UpdatedAt.IsZero() {
		writeTokenProblem(w, http.StatusBadRequest, problemTypeInvalidRequest, "Invalid request",
			"缺少乐观锁版本 updated_at", "updated_at")
		return
	}

	in := tokenstore.UpdateInput{
		UpdatedAt:       req.UpdatedAt,
		Name:            req.Name,
		Group:           req.Group,
		ModelLimitsSet:  req.ModelLimits != nil,
		AllowIPsSet:     req.AllowIPs != nil,
		AutoGroupsSet:   req.AutoGroups != nil,
		UnlimitedQuota:  req.UnlimitedQuota,
		RemainQuota:     req.RemainQuota,
		CrossGroupRetry: req.CrossGroupRetry,
	}
	if req.ModelLimits != nil {
		in.ModelLimits = *req.ModelLimits
	}
	if req.AllowIPs != nil {
		in.AllowIPs = *req.AllowIPs
	}
	if req.AutoGroups != nil {
		in.AutoGroups = *req.AutoGroups
	}
	if req.ExpiresAt.Present {
		// 显式 null 表示「永不过期」；数字按 epoch 秒、字符串按 RFC 3339 解析。
		in.ExpiresAtSet = true
		in.ExpiresAt = req.ExpiresAt.Time
	}

	// before 快照仅供审计；404 时保持统一的不存在语义。
	before, err := h.store.Get(r.Context(), userID, id)
	if err != nil {
		writeTokenServiceError(w, err)
		return
	}
	updated, err := h.store.Update(r.Context(), userID, id, in)
	if err != nil {
		writeTokenServiceError(w, err)
		return
	}
	h.auditEvent(r, audit.ActionTokenUpdate, userID,
		map[string]any{"token_id": id, "changed_fields": changedTokenFields(before, updated)},
		tokenSnapshot(before), tokenSnapshot(updated))
	writeTokenJSON(w, http.StatusOK, h.view(updated))
}

func (h *TokenHandlers) deleteToken(w http.ResponseWriter, r *http.Request) {
	userID, ok := tokenPrincipal(r)
	if !ok {
		writeTokenProblem(w, http.StatusUnauthorized, problemTypeUnauthorized, "Unauthorized", "", "")
		return
	}
	id, ok := tokenPathID(w, r)
	if !ok {
		return
	}
	var expected *time.Time
	if raw := r.URL.Query().Get("updated_at"); raw != "" {
		parsed, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			writeTokenProblem(w, http.StatusBadRequest, problemTypeInvalidRequest, "Invalid request",
				"updated_at 必须是 RFC 3339 时间", "updated_at")
			return
		}
		expected = &parsed
	}
	before, err := h.store.Get(r.Context(), userID, id)
	if err != nil {
		writeTokenServiceError(w, err)
		return
	}
	if err := h.store.Delete(r.Context(), userID, id, expected); err != nil {
		writeTokenServiceError(w, err)
		return
	}
	h.auditEvent(r, audit.ActionTokenDelete, userID,
		map[string]any{"token_id": id}, tokenSnapshot(before), nil, http.StatusNoContent)
	w.WriteHeader(http.StatusNoContent)
}

func (h *TokenHandlers) batchEnable(w http.ResponseWriter, r *http.Request) {
	h.batch(w, r, "enable", audit.ActionTokenBatchEnable, h.store.BatchEnable)
}

func (h *TokenHandlers) batchDisable(w http.ResponseWriter, r *http.Request) {
	h.batch(w, r, "disable", audit.ActionTokenBatchDisable, h.store.BatchDisable)
}

func (h *TokenHandlers) batchDelete(w http.ResponseWriter, r *http.Request) {
	h.batch(w, r, "delete", audit.ActionTokenBatchDelete, h.store.BatchDelete)
}

func (h *TokenHandlers) batch(
	w http.ResponseWriter,
	r *http.Request,
	action, auditAction string,
	fn func(ctx context.Context, userID int64, ids []int64) (int, []tokenstore.BatchFailure, error),
) {
	userID, ok := tokenPrincipal(r)
	if !ok {
		writeTokenProblem(w, http.StatusUnauthorized, problemTypeUnauthorized, "Unauthorized", "", "")
		return
	}
	var req batchTokenRequest
	if !decodeTokenJSON(w, r, &req) {
		return
	}
	if len(req.IDs) == 0 || len(req.IDs) > tokenstore.MaxBatchIDs {
		writeTokenProblem(w, http.StatusBadRequest, problemTypeInvalidRequest, "Invalid request",
			fmt.Sprintf("ids 数量必须在 1..%d 之间", tokenstore.MaxBatchIDs), "ids")
		return
	}
	for _, id := range req.IDs {
		if id <= 0 {
			writeTokenProblem(w, http.StatusBadRequest, problemTypeInvalidRequest, "Invalid request",
				"ids 必须是正整数", "ids")
			return
		}
	}

	succeeded, failed, err := fn(r.Context(), userID, req.IDs)
	if err != nil {
		// 部分成功也可能伴随基础设施中止：tokenstore 先提交了 succeeded 条，
		// 再在后续条目上遇到后端错误才返回 ErrBackend。已提交的变更必须留痕
		// （否则一批删除/禁用可以完全不可审计），但审计只记计数，绝不写底层
		// 错误文本。
		status := http.StatusInternalServerError
		if errors.Is(err, tokenstore.ErrBackend) {
			status = http.StatusServiceUnavailable
		}
		h.auditEventResult(r, auditAction, userID, "failed", map[string]any{
			"batch_action": action,
			"ids":          req.IDs,
			"succeeded":    succeeded,
			"failed_count": len(failed),
		}, nil, nil, status)
		writeTokenServiceError(w, err)
		return
	}
	views := make([]tokenBatchFailureView, 0, len(failed))
	for _, failure := range failed {
		views = append(views, tokenBatchFailureView{ID: failure.ID, Name: failure.Name, Detail: failure.Detail})
	}
	h.auditEvent(r, auditAction, userID, map[string]any{
		"batch_action": action,
		"ids":          req.IDs,
		"succeeded":    succeeded,
		"failed_count": len(views),
	}, nil, nil)
	writeTokenJSON(w, http.StatusOK, tokenBatchResponse{Succeeded: succeeded, Failed: views})
}

func (h *TokenHandlers) tokenAction(w http.ResponseWriter, r *http.Request) {
	userID, ok := tokenPrincipal(r)
	if !ok {
		writeTokenProblem(w, http.StatusUnauthorized, problemTypeUnauthorized, "Unauthorized", "", "")
		return
	}
	rest := r.PathValue("rest")
	if strings.Contains(rest, "/") {
		writeTokenProblem(w, http.StatusNotFound, problemTypeNotFoundToken, "Not found", "", "")
		return
	}
	idPart, action, found := strings.Cut(rest, ":")
	if !found {
		writeTokenProblem(w, http.StatusNotFound, problemTypeNotFoundToken, "Not found", "", "")
		return
	}
	id, err := strconv.ParseInt(idPart, 10, 64)
	if err != nil || id <= 0 {
		writeTokenProblem(w, http.StatusNotFound, problemTypeNotFoundToken, "Not found", "", "")
		return
	}

	switch action {
	case "enable", "disable":
		before, err := h.store.Get(r.Context(), userID, id)
		if err != nil {
			writeTokenServiceError(w, err)
			return
		}
		var updated *tokenstore.Token
		var auditAction string
		if action == "enable" {
			updated, err = h.store.Enable(r.Context(), userID, id)
			auditAction = audit.ActionTokenEnable
		} else {
			updated, err = h.store.Disable(r.Context(), userID, id)
			auditAction = audit.ActionTokenDisable
		}
		if err != nil {
			writeTokenServiceError(w, err)
			return
		}
		h.auditEvent(r, auditAction, userID, map[string]any{"token_id": id},
			tokenSnapshot(before), tokenSnapshot(updated))
		writeTokenJSON(w, http.StatusOK, h.view(updated))
	case "reset-key":
		// 重置密钥是非幂等写操作（每次都会生成新 key）：用 Idempotency-Key
		// 包裹，缓存键含请求路径（即 token id），重放返回首次的新 key。
		h.idempotent("reset-key", func(w http.ResponseWriter, r *http.Request) {
			h.resetKeyAction(w, r, userID, id)
		})(w, r)
	default:
		writeTokenProblem(w, http.StatusNotFound, problemTypeNotFoundToken, "Not found", "", "")
	}
}

func (h *TokenHandlers) resetKeyAction(w http.ResponseWriter, r *http.Request, userID, id int64) {
	if _, err := h.store.Get(r.Context(), userID, id); err != nil {
		writeTokenServiceError(w, err)
		return
	}
	plain, masked, _, err := h.store.ResetKey(r.Context(), userID, id)
	if err != nil {
		writeTokenServiceError(w, err)
		return
	}
	// 审计只记「发生了重置」，绝不记录新旧密钥材料。
	h.auditEvent(r, audit.ActionTokenResetKey, userID,
		map[string]any{"token_id": id}, nil, nil)
	writeTokenJSON(w, http.StatusOK, map[string]any{"key": plain, "key_masked": masked})
}

func (h *TokenHandlers) autoGroups(w http.ResponseWriter, r *http.Request) {
	userID, ok := tokenPrincipal(r)
	if !ok {
		writeTokenProblem(w, http.StatusUnauthorized, problemTypeUnauthorized, "Unauthorized", "", "")
		return
	}
	groups, max, err := h.store.AutoGroups(r.Context(), userID)
	if err != nil {
		writeTokenServiceError(w, err)
		return
	}
	views := make([]tokenAutoGroupView, 0, len(groups))
	for _, group := range groups {
		views = append(views, tokenAutoGroupView{Name: group.Name, Ratio: group.Ratio})
	}
	writeTokenJSON(w, http.StatusOK, tokenAutoGroupsResponse{Groups: views, MaxCount: max})
}

// tokenUsage 是令牌自读用量端点（modules-02 §2.13 / I-29）。
// 只认请求头里的 Bearer 本身；不接受 query 参数、不读会话 Cookie。
func (h *TokenHandlers) tokenUsage(w http.ResponseWriter, r *http.Request) {
	raw, ok := bearerCredential(r)
	if !ok {
		writeTokenProblem(w, http.StatusUnauthorized, problemTypeInvalidToken, "Unauthorized", "", "")
		return
	}
	meta, err := h.store.Resolve(r.Context(), auth.HashTokenKey(raw))
	if err != nil {
		switch {
		case errors.Is(err, tokenstore.ErrTokenDisabled):
			writeTokenProblem(w, http.StatusUnauthorized, problemTypeTokenDisabled, "Token disabled",
				"令牌已禁用；请启用或重置密钥后重试", "")
		case errors.Is(err, tokenstore.ErrTokenExpired):
			writeTokenProblem(w, http.StatusUnauthorized, problemTypeTokenExpired, "Token expired",
				"令牌已过期；请延长过期时间或创建新令牌", "")
		case errors.Is(err, tokenstore.ErrInvalidToken):
			writeTokenProblem(w, http.StatusUnauthorized, problemTypeInvalidToken, "Invalid token", "", "")
		default:
			writeTokenProblem(w, http.StatusServiceUnavailable, problemTypeUnavailable, "Service unavailable", "", "")
		}
		return
	}
	token, err := h.store.Get(r.Context(), meta.UserID, meta.ID)
	if err != nil {
		if errors.Is(err, tokenstore.ErrNotFound) {
			writeTokenProblem(w, http.StatusUnauthorized, problemTypeInvalidToken, "Invalid token", "", "")
		} else {
			writeTokenProblem(w, http.StatusServiceUnavailable, problemTypeUnavailable, "Service unavailable", "", "")
		}
		return
	}

	resp := tokenUsageResponse{
		Object:             "token_usage",
		Name:               token.Name,
		UnlimitedQuota:     token.RemainQuota == nil,
		ModelLimits:        token.ModelLimits,
		ModelLimitsEnabled: token.ModelLimits != nil,
		ExpiresAt:          token.ExpiresAt,
	}
	if token.RemainQuota != nil {
		available := *token.RemainQuota
		resp.TotalAvailable = &available
		// total_granted 在 M3 接入 usage_logs 之前无法可靠计算（令牌表没有
		// used_quota 列），按评审结论返回 null 而不是伪造“授予=剩余”。
	}
	writeTokenJSON(w, http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// 视图/错误/请求辅助
// ---------------------------------------------------------------------------

func (h *TokenHandlers) view(token *tokenstore.Token) tokenView {
	view := tokenView{
		ID:              token.ID,
		Name:            token.Name,
		Status:          tokenstore.DeriveStatus(token, h.now()),
		KeyMasked:       tokenstore.MaskedKey(),
		Group:           token.Group,
		ModelLimits:     token.ModelLimits,
		AllowIPs:        token.AllowIPs,
		AutoGroups:      token.AutoGroups,
		CrossGroupRetry: token.CrossGroupRetry,
		CreatedAt:       token.CreatedAt,
		UpdatedAt:       token.UpdatedAt,
		LastUsedAt:      token.LastUsedAt,
		ExpiresAt:       token.ExpiresAt,
	}
	if view.AutoGroups == nil {
		view.AutoGroups = []string{}
	}
	if token.RemainQuota == nil {
		view.UnlimitedQuota = true
	} else {
		view.RemainQuota = token.RemainQuota
		percent := 100
		if *token.RemainQuota == 0 {
			percent = 0
		}
		view.QuotaPercent = &percent
	}
	return view
}

func tokenPrincipal(r *http.Request) (int64, bool) {
	principal, ok := auth.PrincipalFrom(r.Context())
	if !ok || principal.UserID <= 0 {
		return 0, false
	}
	return principal.UserID, true
}

func tokenPathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeTokenProblem(w, http.StatusNotFound, problemTypeNotFoundToken, "Not found", "", "")
		return 0, false
	}
	return id, true
}

func tokenQueryInt(r *http.Request, key string, def int) (int, error) {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return def, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s 必须是整数", key)
	}
	return value, nil
}

func decodeTokenJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, tokenMaxRequestBody)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		writeTokenProblem(w, http.StatusBadRequest, problemTypeInvalidRequest, "Invalid request", err.Error(), "")
		return false
	}
	// 拒绝请求体后跟随的多余 JSON 值，避免「首值生效、其余被静默忽略」。
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeTokenProblem(w, http.StatusBadRequest, problemTypeInvalidRequest, "Invalid request", "请求体只能包含一个 JSON 对象", "")
		return false
	}
	return true
}

func writeTokenJSON(w http.ResponseWriter, status int, payload any) {
	// 令牌响应可能含一次性明文 key（创建/重置）：禁止任何中间层缓存。
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

type tokenProblem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
	Field  string `json:"field,omitempty"`
}

func writeTokenProblem(w http.ResponseWriter, status int, problemType, title, detail, field string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(tokenProblem{Type: problemType, Title: title, Status: status, Detail: detail, Field: field})
}

// writeTokenServiceError 把领域错误映射为 RFC 7807。
func writeTokenServiceError(w http.ResponseWriter, err error) {
	var validation *tokenstore.ValidationError
	switch {
	case errors.As(err, &validation):
		writeTokenProblem(w, http.StatusBadRequest, problemTypeInvalidRequest, "Invalid request",
			validation.Message, validation.Field)
	case errors.Is(err, tokenstore.ErrNotFound):
		// 他人令牌与不存在令牌同响应，不泄露存在性（TC-B-07 口径）。
		writeTokenProblem(w, http.StatusNotFound, problemTypeNotFoundToken, "Not found", "", "")
	case errors.Is(err, tokenstore.ErrConflict):
		writeTokenProblem(w, http.StatusConflict, problemTypeConflictToken, "Conflict",
			"该令牌已被其他操作修改，请刷新后重试", "")
	case errors.Is(err, tokenstore.ErrBackend):
		// 批量操作中的基础设施故障（DB/Redis/驱动）：tokenstore 已中止整个
		// 批量并包上 ErrBackend。绝不能把它写进 200 的 failed[]（那会让系统
		// 性故障看起来像业务部分失败），也不透传底层错误文本（可能含内网
		// host:port/SQL 细节）。Retry-After 给自动化客户端有界退避提示。
		w.Header().Set("Retry-After", "5")
		writeTokenProblem(w, http.StatusServiceUnavailable, problemTypeUnavailable,
			"Service unavailable", "", "")
	default:
		writeTokenProblem(w, http.StatusInternalServerError, "https://newtg.dev/problems/internal_error", "Internal Server Error", "", "")
	}
}

func tokenSnapshot(token *tokenstore.Token) map[string]any {
	if token == nil {
		return nil
	}
	return map[string]any{
		"token_id":          token.ID,
		"name":              token.Name,
		"status":            token.Status,
		"group":             token.Group,
		"expires_at":        token.ExpiresAt,
		"remain_quota":      token.RemainQuota,
		"unlimited_quota":   token.RemainQuota == nil,
		"model_limits":      token.ModelLimits,
		"allow_ips":         token.AllowIPs,
		"auto_groups":       token.AutoGroups,
		"cross_group_retry": token.CrossGroupRetry,
	}
}

func changedTokenFields(before, after *tokenstore.Token) []string {
	beforeSnap := tokenSnapshot(before)
	afterSnap := tokenSnapshot(after)
	changed := make([]string, 0)
	for key, beforeValue := range beforeSnap {
		if key == "token_id" {
			continue
		}
		if !tokenJSONEqual(beforeValue, afterSnap[key]) {
			changed = append(changed, key)
		}
	}
	return changed
}

func tokenJSONEqual(a, b any) bool {
	rawA, errA := json.Marshal(a)
	rawB, errB := json.Marshal(b)
	return errA == nil && errB == nil && bytes.Equal(rawA, rawB)
}

func (h *TokenHandlers) auditEvent(r *http.Request, action string, userID int64, detail, before, after any, status ...int) {
	h.auditEventResult(r, action, userID, "success", detail, before, after, status...)
}

// auditEventResult 是带显式 result 的审计写入（部分成功/失败的批量需要
// result=failed 但请求本身可能返回 200 或 503）。
func (h *TokenHandlers) auditEventResult(r *http.Request, action string, userID int64, result string, detail, before, after any, status ...int) {
	if h.audit == nil {
		return
	}
	statusCode := http.StatusOK
	if len(status) > 0 && status[0] > 0 {
		statusCode = status[0]
	}
	event := audit.Event{
		Action:     action,
		Actor:      userID,
		Result:     result,
		IP:         tokenRemoteIP(r),
		UserAgent:  r.UserAgent(),
		Method:     r.Method,
		Route:      r.Pattern,
		StatusCode: statusCode,
	}
	event.Detail = tokenMarshalRaw(detail)
	event.Before = tokenMarshalRaw(before)
	event.After = tokenMarshalRaw(after)
	// 审计失败不阻断业务（日志库独立且无跨库事务）；由运维告警接手。
	// 统一走 writeAuditEvent 的 3 秒上限：日志库故障不得把令牌请求拖到连接超时。
	if err := writeAuditEvent(r.Context(), h.audit, event); err != nil {
		slog.Warn("token audit write failed", "action", action, "actor", userID, "error", err)
	}
}

func tokenMarshalRaw(value any) json.RawMessage {
	if value == nil {
		return nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return raw
}

func tokenRemoteIP(r *http.Request) string {
	// 与 handlers_auth.go 的 clientIP 同口径：不信任可伪造的 X-Forwarded-For。
	return clientIP(r)
}

// ---------------------------------------------------------------------------
// Idempotency-Key
// ---------------------------------------------------------------------------

type tokenCachedHTTPResponse struct {
	Status      int    `json:"status"`
	ContentType string `json:"content_type"`
	Body        string `json:"body"`
	Fingerprint string `json:"fingerprint"`
}

// idempotent 为写端点提供幂等重放（窗口见 tokenIdempotencyTTL：10 分钟）。
// 同一 key + 同一请求体返回首次响应（逐字节）；不同请求体复用 key → 422。
//
// 缓存体含创建/重置响应里的明文 key，必须以 sealer 密封后落 Redis；sealer
// 为 nil（未装配加密）时直接执行、完全不缓存——「无缓存」是可接受的降级，
// 「明文凭据进 Redis」不是。
func (h *TokenHandlers) idempotent(scope string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := tokenPrincipal(r)
		if !ok {
			writeTokenProblem(w, http.StatusUnauthorized, problemTypeUnauthorized, "Unauthorized", "", "")
			return
		}
		key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		if key == "" || h.rdb == nil || h.sealer == nil {
			next(w, r)
			return
		}
		if len(key) > 128 {
			writeTokenProblem(w, http.StatusBadRequest, problemTypeInvalidRequest, "Invalid request",
				"Idempotency-Key 最长 128 个字符", "Idempotency-Key")
			return
		}

		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, tokenMaxRequestBody))
		if err != nil {
			writeTokenProblem(w, http.StatusBadRequest, problemTypeInvalidRequest, "Invalid request", err.Error(), "")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		fingerprint := tokenBodyFingerprint(body)

		ctx := r.Context()
		// 缓存键含请求路径：同一幂等键用于不同令牌（如 reset-key /api/tokens/1
		// vs /2）时不得互相命中。
		cacheKey := fmt.Sprintf("newtg:idem:token:%d:%s:%s:%s", userID, scope, r.URL.Path, key)
		if cached, ok := h.loadIdempotent(ctx, cacheKey); ok {
			replayTokenIdempotent(w, cached, fingerprint)
			return
		}

		lockKey := cacheKey + ":lock"
		acquired, lockErr := h.rdb.SetNX(ctx, lockKey, "1", tokenIdempotencyLease).Result()
		if lockErr == nil && !acquired {
			// 并发同 key：等首个请求落结果（≤3s），避免重复执行。
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				select {
				case <-ctx.Done():
					writeTokenProblem(w, http.StatusConflict, problemTypeConflictToken, "Conflict",
						"请求已取消，幂等结果未就绪", "")
					return
				case <-time.After(25 * time.Millisecond):
				}
				if cached, ok := h.loadIdempotent(ctx, cacheKey); ok {
					replayTokenIdempotent(w, cached, fingerprint)
					return
				}
			}
			writeTokenProblem(w, http.StatusConflict, problemTypeConflictToken, "Conflict",
				"相同 Idempotency-Key 的请求仍在处理中，请稍后重试", "")
			return
		}

		buffer := newTokenBufferedResponse()
		next(buffer, r)
		if buffer.status >= 200 && buffer.status < 300 {
			h.saveIdempotent(ctx, cacheKey, tokenCachedHTTPResponse{
				Status:      buffer.status,
				ContentType: buffer.header.Get("Content-Type"),
				Body:        buffer.body.String(),
				Fingerprint: fingerprint,
			})
		}
		if lockErr == nil {
			_ = h.rdb.Del(ctx, lockKey).Err()
		}
		buffer.copyTo(w)
	}
}

func replayTokenIdempotent(w http.ResponseWriter, cached tokenCachedHTTPResponse, fingerprint string) {
	if cached.Fingerprint != fingerprint {
		writeTokenProblem(w, http.StatusUnprocessableEntity, problemTypeIdempotencyReused, "Idempotency key reused",
			"同一 Idempotency-Key 不能用于不同的请求体", "Idempotency-Key")
		return
	}
	if cached.ContentType != "" {
		w.Header().Set("Content-Type", cached.ContentType)
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(cached.Status)
	_, _ = io.WriteString(w, cached.Body)
}

func (h *TokenHandlers) loadIdempotent(ctx context.Context, key string) (tokenCachedHTTPResponse, bool) {
	raw, err := h.rdb.Get(ctx, key).Bytes()
	if err != nil {
		return tokenCachedHTTPResponse{}, false
	}
	opened, err := h.sealer.Open(string(raw))
	if err != nil {
		// 解密失败（密钥轮换/数据损坏/旧版本明文残留）按未命中处理并删除该键：
		// 重放不可信的结果比重新执行更危险，而无 TTL 的脏键会持续失败。
		_ = h.rdb.Del(ctx, key).Err()
		return tokenCachedHTTPResponse{}, false
	}
	var cached tokenCachedHTTPResponse
	if err := json.Unmarshal([]byte(opened), &cached); err != nil {
		_ = h.rdb.Del(ctx, key).Err()
		return tokenCachedHTTPResponse{}, false
	}
	return cached, true
}

func (h *TokenHandlers) saveIdempotent(ctx context.Context, key string, cached tokenCachedHTTPResponse) {
	raw, err := json.Marshal(cached)
	if err != nil {
		return
	}
	sealed, err := h.sealer.Seal(string(raw))
	if err != nil {
		// 加密失败同样不写：宁可失去重放保护，也不能明文落缓存。
		return
	}
	// 幂等缓存是尽力而为：Redis 故障时请求已经成功，不能因此回滚业务。
	_ = h.rdb.Set(ctx, key, sealed, tokenIdempotencyTTL).Err()
}

func tokenBodyFingerprint(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

type tokenBufferedResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newTokenBufferedResponse() *tokenBufferedResponse {
	return &tokenBufferedResponse{header: make(http.Header)}
}

func (b *tokenBufferedResponse) Header() http.Header { return b.header }

func (b *tokenBufferedResponse) WriteHeader(status int) {
	if b.status == 0 {
		b.status = status
	}
}

func (b *tokenBufferedResponse) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	return b.body.Write(p)
}

func (b *tokenBufferedResponse) copyTo(w http.ResponseWriter) {
	for key, values := range b.header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	status := b.status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write(b.body.Bytes())
}
