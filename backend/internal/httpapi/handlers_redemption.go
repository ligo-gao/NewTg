// Package httpapi 的兑换码端点（M5 Task 2 / modules-04 §4.F F-04-14/15/16）。
//
// 端点面（P-9 冻结）：
//
//	POST   /api/redemption-codes               管理：批量生成，明文码仅在此响应出现一次
//	GET    /api/redemption-codes               管理：offset 分页 + 状态/批次/面额筛选
//	PATCH  /api/redemption-codes/{id}          管理：unused ↔ disabled；used → 409
//	DELETE /api/redemption-codes/{id}          管理：used → 409
//	POST   /api/redemption-codes/batch-delete  管理：{ids:[...]} ≤100，逐条报告失败
//	POST   /api/redemption-codes/redeem        任意已登录用户：核销入账
//
// 管理端点全部要求 `redemption:manage`（rbac 三层判定，写侧登记字段白名单）。
// 兑换端点用独立 Redis 桶 10 次/分钟/用户（F-08-16 唯一阈值来源）。
//
// 安全口径：
//   - 列表/详情永不返回码值，只返回固定掩码 `••••••`；明文只在生成响应中出现。
//   - 核销的原子性由 billing.RedemptionService 保证（条件 UPDATE + 入账同事务），
//     本层只做限流、参数与错误映射。
//   - 日志/错误详情绝不写入码值；失败原因用语义错误而不是回显输入。
package httpapi

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/billing"
	"newtg/gateway/internal/rbac"
)

// 列表掩码与限流阈值（F-04-16 / F-08-16）。
const (
	redemptionCodeMask = "••••••"
	// redemptionRateLimit = 10 次/分钟/用户（F-08-16 唯一来源表）。
	redemptionRateLimit = 10
	// redemptionRateWindow 是固定窗口长度。固定窗口而非滑动窗口：兑换是
	// 低频操作，边界处最多允许 2× 突刺；用更简单的 INCR+EXPIRE 换取
	// 可预测的 Redis 成本，与登录锁定桶同一口径。
	redemptionRateWindow = time.Minute
	// redemptionRateKeyPrefix 独立命名空间：绝不与登录/中继桶共键。
	redemptionRateKeyPrefix = "newtg:redeem:rate:user:"
	// redemptionBatchLimit 是单次生成/批量删除的条目上限（P-9 ≤100）。
	redemptionBatchLimit = 100
)

// RFC 7807 type URI（与全库约定一致）。
const (
	problemTypeRedemptionInvalid  = "https://newtg.dev/problems/redemption_code_invalid"
	problemTypeRedemptionUsed     = "https://newtg.dev/problems/redemption_code_used"
	problemTypeRedemptionDisabled = "https://newtg.dev/problems/redemption_code_disabled"
	problemTypeRedemptionExpired  = "https://newtg.dev/problems/redemption_code_expired"
)

// 管理端点动作（字段策略按动作登记）。
var (
	redemptionCreateAction      = rbac.AuthzAction{ResourceKind: "redemption_code", ActionName: "create"}
	redemptionListAction        = rbac.AuthzAction{ResourceKind: "redemption_code", ActionName: "list"}
	redemptionUpdateAction      = rbac.AuthzAction{ResourceKind: "redemption_code", ActionName: "update"}
	redemptionDeleteAction      = rbac.AuthzAction{ResourceKind: "redemption_code", ActionName: "delete"}
	redemptionBatchDeleteAction = rbac.AuthzAction{ResourceKind: "redemption_code", ActionName: "batch_delete"}
)

// RedemptionHandlerConfig 是兑换码端点的装配输入。
type RedemptionHandlerConfig struct {
	DB      *gorm.DB
	Service *billing.RedemptionService
	Redis   *redis.Client
	Config  ConfigReader
	Authz   *rbac.AuthzMiddleware
	Logger  *slog.Logger
	// Now 是限流窗口的时钟接缝；nil 时 time.Now。
	Now func() time.Time
}

// RedemptionHandlers 持有兑换码端点的依赖，可并发使用。
type RedemptionHandlers struct {
	db    *gorm.DB
	svc   *billing.RedemptionService
	rdb   *redis.Client
	authz *rbac.AuthzMiddleware
	log   *slog.Logger
	now   func() time.Time
}

// NewRedemptionHandlers 构造兑换码端点。
//
// Service 与 DB 二选一：不提供 Service 时用 NewWalletFunding(db) 构造默认
// 实现（余额 + 恰一行流水）。Redis 必填——兑换限流是资金接口的防刷闸门，
// 缺 Redis 时不能静默不限流。
func NewRedemptionHandlers(cfg RedemptionHandlerConfig) (*RedemptionHandlers, error) {
	if cfg.DB == nil {
		return nil, errors.New("httpapi: RedemptionHandlerConfig.DB is required")
	}
	if cfg.Redis == nil {
		return nil, errors.New("httpapi: RedemptionHandlerConfig.Redis is required (redeem rate limit)")
	}
	svc := cfg.Service
	if svc == nil {
		svc = billing.NewRedemptionService(cfg.DB, billing.NewWalletFunding(cfg.DB), cfg.Config)
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
	h := &RedemptionHandlers{db: cfg.DB, svc: svc, rdb: cfg.Redis, authz: authz, log: log, now: now}
	h.registerPolicies()
	return h, nil
}

// registerPolicies 登记每个写动作的字段白名单：未登记动作对任何写请求
// 都 403（security-02 §2.6.1 规则 6）。DELETE 也必须有空策略，否则
// 字段层会失败关闭。
func (h *RedemptionHandlers) registerPolicies() {
	h.authz.SetWritePolicy(redemptionCreateAction, &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{
		"batch_name": nil,
		"credits":    nil,
		"count":      nil,
		"expires_at": nil,
	}})
	h.authz.SetWritePolicy(redemptionUpdateAction, &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{
		"status": nil,
	}})
	h.authz.SetWritePolicy(redemptionDeleteAction, &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{}})
	h.authz.SetWritePolicy(redemptionBatchDeleteAction, &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{
		"ids": nil,
	}})
}

// RegisterRedemptionRoutes 注册全部兑换码端点。wrap 语义同 RegisterUserRoutes。
func (h *RedemptionHandlers) RegisterRedemptionRoutes(mux *http.ServeMux, wrap func(http.Handler) http.Handler) {
	mount := func(pattern string, handler http.Handler) {
		if wrap != nil {
			handler = wrap(handler)
		}
		mux.Handle(pattern, handler)
	}
	fixed := func(required string, action rbac.AuthzAction, next http.HandlerFunc) http.Handler {
		return h.authz.Handler(required, action)(next)
	}

	mount("GET /api/redemption-codes", fixed(rbac.PermRedemptionManage, redemptionListAction, h.handleList))
	mount("POST /api/redemption-codes", fixed(rbac.PermRedemptionManage, redemptionCreateAction, h.handleCreate))
	mount("PATCH /api/redemption-codes/{id}", fixed(rbac.PermRedemptionManage, redemptionUpdateAction, h.handleUpdateStatus))
	mount("DELETE /api/redemption-codes/{id}", fixed(rbac.PermRedemptionManage, redemptionDeleteAction, h.handleDelete))
	mount("POST /api/redemption-codes/batch-delete",
		fixed(rbac.PermRedemptionManage, redemptionBatchDeleteAction, h.handleBatchDelete))
	// 兑换是自服务动作（required=""）：任何已认证用户都可调用，
	// 限流与输入归一化在 handler 内部完成。
	mount("POST /api/redemption-codes/redeem", http.HandlerFunc(h.handleRedeem))
}

// ---------------------------------------------------------------------------
// 视图
// ---------------------------------------------------------------------------

// adminRedemptionCodeView 是列表项投影。刻意没有 code 字段：
// 码值不可再取，前端只显示固定掩码（F-04-16）。
type adminRedemptionCodeView struct {
	ID        int64      `json:"id"`
	BatchName *string    `json:"batch_name"`
	Credits   int64      `json:"credits"`
	Status    string     `json:"status"` // unused/disabled/used/expired（虚拟）
	CodeMask  string     `json:"code_mask"`
	CreatedBy int64      `json:"created_by"`
	UsedBy    *int64     `json:"used_by"`
	UsedAt    *time.Time `json:"used_at"`
	ExpiresAt *time.Time `json:"expires_at"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

type adminRedemptionCodeListView struct {
	Items    []adminRedemptionCodeView `json:"items"`
	Total    int64                     `json:"total"`
	Page     int                       `json:"page"`
	PageSize int                       `json:"page_size"`
}

// redemptionCodeListRow 是 SQL 扫描结构；虚拟过期态在 SELECT 里计算，
// 不依赖应用时钟与数据库时钟的一致性。
type redemptionCodeListRow struct {
	ID        int64      `gorm:"column:id"`
	BatchName *string    `gorm:"column:batch_name"`
	Credits   int64      `gorm:"column:credits"`
	Status    string     `gorm:"column:status"`
	CreatedBy int64      `gorm:"column:created_by"`
	UsedBy    *int64     `gorm:"column:used_by"`
	UsedAt    *time.Time `gorm:"column:used_at"`
	ExpiresAt *time.Time `gorm:"column:expires_at"`
	CreatedAt time.Time  `gorm:"column:created_at"`
	UpdatedAt time.Time  `gorm:"column:updated_at"`
}

// ---------------------------------------------------------------------------
// 管理端点
// ---------------------------------------------------------------------------

type createRedemptionCodesRequest struct {
	BatchName *string    `json:"batch_name"`
	Credits   int64      `json:"credits"`
	Count     int        `json:"count"`
	ExpiresAt *time.Time `json:"expires_at"`
}

type createRedemptionCodesResponse struct {
	BatchName *string    `json:"batch_name"`
	Credits   int64      `json:"credits"`
	Count     int        `json:"count"`
	ExpiresAt *time.Time `json:"expires_at"`
	// Codes 仅在本次响应返回；库中只有 SHA-256 哈希，之后任何端点都不再提供。
	Codes []string `json:"codes"`
}

func (h *RedemptionHandlers) handleCreate(w http.ResponseWriter, r *http.Request) {
	p, ok := adminPrincipal(w, r)
	if !ok {
		return
	}
	var req createRedemptionCodesRequest
	if err := readAuthJSON(r, &req); err != nil {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"request body must be a JSON object")
		return
	}

	// 批次名在入口 trim 后再落库：避免「相同批次因前后空白分成两组」。
	var batchName *string
	if req.BatchName != nil {
		trimmed := strings.TrimSpace(*req.BatchName)
		batchName = &trimmed
	}
	codes, err := h.svc.GenerateCodes(r.Context(), batchName, req.Credits, req.Count, req.ExpiresAt, p.UserID)
	if err != nil {
		if errors.Is(err, billing.ErrInvalidRedemptionBatch) {
			writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request", err.Error())
			return
		}
		// 哈希碰撞重试耗尽属服务端生成故障，不是客户端参数问题。
		h.log.Error("generate redemption codes failed", "error", err)
		writeAdminProblem(w, http.StatusInternalServerError, authProblemInternal,
			"Internal server error", "")
		return
	}
	writeJSON(w, http.StatusCreated, createRedemptionCodesResponse{
		BatchName: batchName,
		Credits:   req.Credits,
		Count:     len(codes),
		ExpiresAt: req.ExpiresAt,
		Codes:     codes,
	})
}

// handleList 返回 offset 分页信封。status 支持虚拟的 expired：
// 它永远不落库，筛选与展示都在 SQL 中按 expires_at 计算。
func (h *RedemptionHandlers) handleList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page, pageSize := 1, adminPageSizeDefault
	if raw := strings.TrimSpace(q.Get("p")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > adminPageMax {
			writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
				fmt.Sprintf("p must be between 1 and %d", adminPageMax))
			return
		}
		page = value
	}
	if raw := strings.TrimSpace(q.Get("page_size")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > adminPageSizeMax {
			writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
				fmt.Sprintf("page_size must be between 1 and %d", adminPageSizeMax))
			return
		}
		pageSize = value
	}

	build := func() *gorm.DB {
		base := h.db.WithContext(r.Context()).Table("redemption_codes")
		switch status := strings.TrimSpace(q.Get("status")); status {
		case "":
		case "unused":
			base = base.Where(`status = 'unused' AND (expires_at IS NULL OR expires_at > now())`)
		case "expired":
			base = base.Where(`status = 'unused' AND expires_at IS NOT NULL AND expires_at <= now()`)
		case "used", "disabled":
			base = base.Where("status = ?", status)
		default:
			return nil
		}
		if batch := strings.TrimSpace(q.Get("batch_name")); batch != "" {
			base = base.Where("batch_name = ?", batch)
		}
		if raw := strings.TrimSpace(q.Get("min_credits")); raw != "" {
			value, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || value < 0 {
				return nil
			}
			base = base.Where("credits >= ?", value)
		}
		if raw := strings.TrimSpace(q.Get("max_credits")); raw != "" {
			value, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || value < 0 {
				return nil
			}
			base = base.Where("credits <= ?", value)
		}
		return base
	}

	base := build()
	if base == nil {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"status must be unused|disabled|used|expired; credits filters must be non-negative integers")
		return
	}
	var total int64
	if err := base.Count(&total).Error; err != nil {
		h.log.Error("count redemption codes failed", "error", err)
		writeAdminProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error", "")
		return
	}

	rows := make([]redemptionCodeListRow, 0, pageSize)
	selectSQL := `id, batch_name, credits,
		CASE WHEN status = 'unused' AND expires_at IS NOT NULL AND expires_at <= now()
		     THEN 'expired' ELSE status END AS status,
		created_by, used_by, used_at, expires_at, created_at, updated_at`
	err := build().Select(selectSQL).
		Order("id DESC").
		Limit(pageSize).
		Offset((page - 1) * pageSize).
		Scan(&rows).Error
	if err != nil {
		h.log.Error("list redemption codes failed", "error", err)
		writeAdminProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error", "")
		return
	}

	items := make([]adminRedemptionCodeView, 0, len(rows))
	for _, row := range rows {
		items = append(items, adminRedemptionCodeView{
			ID:        row.ID,
			BatchName: row.BatchName,
			Credits:   row.Credits,
			Status:    row.Status,
			CodeMask:  redemptionCodeMask,
			CreatedBy: row.CreatedBy,
			UsedBy:    row.UsedBy,
			UsedAt:    row.UsedAt,
			ExpiresAt: row.ExpiresAt,
			CreatedAt: row.CreatedAt,
			UpdatedAt: row.UpdatedAt,
		})
	}
	writeJSON(w, http.StatusOK, adminRedemptionCodeListView{
		Items: items, Total: total, Page: page, PageSize: pageSize,
	})
}

type updateRedemptionStatusRequest struct {
	Status string `json:"status"`
}

func (h *RedemptionHandlers) handleUpdateStatus(w http.ResponseWriter, r *http.Request) {
	id, ok := parseRedemptionID(w, r)
	if !ok {
		return
	}
	var req updateRedemptionStatusRequest
	if err := readAuthJSON(r, &req); err != nil {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"request body must be a JSON object")
		return
	}
	status := strings.TrimSpace(req.Status)
	// used 是核销的结果而不是可写入的状态：手工置 used 会绕过入账。
	if status != "unused" && status != "disabled" {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"status must be unused or disabled")
		return
	}

	res := h.db.WithContext(r.Context()).Exec(`
		UPDATE redemption_codes SET status = ?, updated_at = now()
		WHERE id = ? AND status <> 'used'`, status, id)
	if res.Error != nil {
		h.log.Error("update redemption status failed", "error", res.Error)
		writeAdminProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error", "")
		return
	}
	if res.RowsAffected == 0 {
		h.writeRedemptionMiss(w, r, "update", id)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": status})
}

func (h *RedemptionHandlers) handleDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := parseRedemptionID(w, r)
	if !ok {
		return
	}
	res := h.db.WithContext(r.Context()).
		Exec(`DELETE FROM redemption_codes WHERE id = ? AND status <> 'used'`, id)
	if res.Error != nil {
		h.log.Error("delete redemption code failed", "error", res.Error)
		writeAdminProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error", "")
		return
	}
	if res.RowsAffected == 0 {
		h.writeRedemptionMiss(w, r, "delete", id)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type batchDeleteRedemptionRequest struct {
	IDs []int64 `json:"ids"`
}

type batchDeleteFailure struct {
	ID     int64  `json:"id"`
	Detail string `json:"detail"`
}

type batchDeleteRedemptionResponse struct {
	Succeeded int                  `json:"succeeded"`
	Failed    []batchDeleteFailure `json:"failed"`
}

// handleBatchDelete 逐条删除并报告失败；不用一条 `WHERE id = ANY(...)`：
// 已使用码必须进入 failed 明细，批量删除不能把 409 静默降级成「删了一部分」。
func (h *RedemptionHandlers) handleBatchDelete(w http.ResponseWriter, r *http.Request) {
	var req batchDeleteRedemptionRequest
	if err := readAuthJSON(r, &req); err != nil {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"request body must be a JSON object")
		return
	}
	if len(req.IDs) == 0 || len(req.IDs) > redemptionBatchLimit {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			fmt.Sprintf("ids must contain between 1 and %d entries", redemptionBatchLimit))
		return
	}

	resp := batchDeleteRedemptionResponse{Failed: make([]batchDeleteFailure, 0)}
	err := h.db.WithContext(r.Context()).Transaction(func(tx *gorm.DB) error {
		for _, id := range req.IDs {
			if id <= 0 {
				resp.Failed = append(resp.Failed, batchDeleteFailure{ID: id, Detail: "invalid_id"})
				continue
			}
			res := tx.Exec(`DELETE FROM redemption_codes WHERE id = ? AND status <> 'used'`, id)
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected > 0 {
				resp.Succeeded++
				continue
			}
			detail, err := redemptionMissDetail(tx, id)
			if err != nil {
				return err
			}
			resp.Failed = append(resp.Failed, batchDeleteFailure{ID: id, Detail: detail})
		}
		return nil
	})
	if err != nil {
		h.log.Error("batch delete redemption codes failed", "error", err)
		writeAdminProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error", "")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// 用户兑换
// ---------------------------------------------------------------------------

type redeemRequest struct {
	Code string `json:"code"`
}

type redeemResponse struct {
	Credits      int64 `json:"credits"`
	BalanceAfter int64 `json:"balance_after"`
}

func (h *RedemptionHandlers) handleRedeem(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok || p == nil || p.UserID <= 0 {
		writeProblem(w, http.StatusUnauthorized, problemTypeUnauthorized, "Unauthorized")
		return
	}
	if !h.allowRedeem(w, r, p.UserID) {
		return
	}

	var req redeemRequest
	if err := readAuthJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request")
		return
	}
	// 输入长度上限 64（F-04-14）：归一化前的粗筛，避免把超长字符串送进哈希。
	if utf8.RuneCountInString(req.Code) > 64 {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid redemption code")
		return
	}

	balanceAfter, credits, err := h.svc.RedeemDetailed(r.Context(), p.UserID, req.Code)
	if err != nil {
		switch {
		case errors.Is(err, billing.ErrRedemptionCodeInvalid):
			writeProblemDetail(w, http.StatusBadRequest, problemTypeRedemptionInvalid,
				"Invalid redemption code", "The redemption code is invalid.")
		case errors.Is(err, billing.ErrRedemptionCodeUsed):
			writeProblemDetail(w, http.StatusConflict, problemTypeRedemptionUsed,
				"Redemption code already used", "This code has already been redeemed.")
		case errors.Is(err, billing.ErrRedemptionCodeDisabled):
			writeProblemDetail(w, http.StatusConflict, problemTypeRedemptionDisabled,
				"Redemption code disabled", "This code has been disabled.")
		case errors.Is(err, billing.ErrRedemptionCodeExpired):
			writeProblemDetail(w, http.StatusGone, problemTypeRedemptionExpired,
				"Redemption code expired", "This code has expired.")
		default:
			// 配置错误（QPU/倍率非法）与溢出属资损风险，必须 500 且不进响应。
			h.log.Error("redeem code failed", "user_id", p.UserID, "error", err)
			writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
		}
		return
	}
	// 注意：这里不回显码值，也不把原始输入写日志。credits 由服务层按
	// code.credits × QPU × ratio 计算，客户端不能影响。
	writeJSON(w, http.StatusOK, redeemResponse{BalanceAfter: balanceAfter, Credits: credits})
}

// allowRedeem 执行 10 次/分钟/用户的固定窗口限流。
//
// Redis 故障时 fail closed（503）：资金接口不能因为限流器不可用而无限放量。
// 超限返回 429 + Retry-After（F-08-16 验收）。
func (h *RedemptionHandlers) allowRedeem(w http.ResponseWriter, r *http.Request, userID int64) bool {
	now := h.now()
	bucket := now.Unix() / int64(redemptionRateWindow/time.Second)
	key := fmt.Sprintf("%s%d:%d", redemptionRateKeyPrefix, userID, bucket)

	count, err := h.rdb.Incr(r.Context(), key).Result()
	if err != nil {
		h.log.Error("redeem rate limiter unavailable", "error", err)
		w.Header().Set("Retry-After", "5")
		writeProblem(w, http.StatusServiceUnavailable, authProblemServiceUnavailable,
			"Service temporarily unavailable")
		return false
	}
	if count == 1 {
		// 窗口 + 1s 的 TTL：固定窗口键在下一分钟边界后自然消失。
		h.rdb.Expire(r.Context(), key, redemptionRateWindow+time.Second)
	}
	if count > redemptionRateLimit {
		retry := time.Unix((bucket+1)*int64(redemptionRateWindow/time.Second), 0).Sub(now)
		seconds := int(retry / time.Second)
		if seconds < 1 {
			seconds = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(seconds))
		writeProblem(w, http.StatusTooManyRequests, authProblemTooManyRequests, "Too many requests")
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

func parseRedemptionID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := strings.TrimSpace(r.PathValue("id"))
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		writeAdminProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found", "")
		return 0, false
	}
	return id, true
}

// writeRedemptionMiss 在条件 UPDATE/DELETE 影响 0 行后区分 404 与 409。
func (h *RedemptionHandlers) writeRedemptionMiss(w http.ResponseWriter, r *http.Request, op string, id int64) {
	detail, err := redemptionMissDetail(h.db.WithContext(r.Context()), id)
	if err != nil {
		h.log.Error(op+" redemption code failed", "id", id, "error", err)
		writeAdminProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error", "")
		return
	}
	switch detail {
	case "used":
		writeAdminProblem(w, http.StatusConflict, authProblemConflict, "Conflict",
			"Redeemed codes cannot be modified.")
	default:
		writeAdminProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found", "")
	}
}

// redemptionMissDetail 返回 "used" / "not_found" / "invalid_id"。
func redemptionMissDetail(db *gorm.DB, id int64) (string, error) {
	if id <= 0 {
		return "invalid_id", nil
	}
	var status string
	res := db.Raw(`SELECT status FROM redemption_codes WHERE id = ?`, id).Scan(&status)
	if res.Error != nil {
		return "", res.Error
	}
	if res.RowsAffected == 0 {
		return "not_found", nil
	}
	if status == "used" {
		return "used", nil
	}
	// 行存在但条件更新失败（并发把状态改成 used 或行刚被删除后重建）：
	// 按已使用处理，绝不把它当作成功。
	return "used", nil
}
