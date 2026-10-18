// 管理面用户端点（Task 14 / modules-07 F-07-01..09、F-07-11、F-07-12）。
//
// 端点面：
//
//	GET    /api/users                          keyword/status/role/group/p/page_size/sort
//	POST   /api/users                          Idempotency-Key；email 恒为 NULL
//	GET    /api/users/{id}
//	PATCH  /api/users/{id}                     部分更新 + updated_at 乐观锁；password → 400
//	POST   /api/users/{id}/status              {status}；封禁/解禁
//	DELETE /api/users/{id}                     前置 status=disabled，硬删除
//	POST   /api/users/{id}/quota-adjustment    {mode, amount, idempotency_key}
//	POST   /api/users/{id}/reset-2fa
//	POST   /api/users/{id}/reset-password      → {temp_password}（仅此一次）
//	PATCH  /api/users/{id}/permissions         超集校验的权限矩阵
//
// 路由挂载（并行任务负责 routes.go 的最终接线）：
//
//	h, err := NewUserHandlers(UserHandlerConfig{
//	    DB: cfg.DB, Sessions: cfg.Sessions, Redis: cfg.Redis,
//	    Authz: cfg.Authz, Audit: cfg.Audit, Logger: cfg.Logger,
//	})
//	h.RegisterUserRoutes(mux, nil)          // mux 已在 Protect(...) 之后
//	h.RegisterUserRoutes(rootMux, Protect(cfg.Sessions, cfg.TokenStore, cfg.PublicBaseURL))
//
// 安全口径：
//   - 每个端点都由 rbac 三层判定包裹：功能级权限键、ID 寻址的数据级归属
//     （无管理键访问他人一律 404，不泄露存在性）、写侧字段白名单；
//   - 口令材料绝不进入响应或审计：PATCH 不接受口令（400）；重置口令只在
//     reset-password 响应出现一次；
//   - 封禁/重置类操作先吊销 Redis 会话再改 DB：Redis 故障时返回 503 且不改
//     DB（fail-closed）；DB 失败时会话已失效（安全方向），绝不出现「已封禁但
//     旧会话仍可用」；
//   - 余额只经原子 UPDATE（GC-5）：subtract 用 `balance >= amount` 条件更新，
//     并发下不可能为负；每笔变动恰写一行 wallet_transactions。
package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/api/dto"
	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/rbac"
)

const (
	// tempPasswordLength / tempPasswordTTL：F-07-12 冻结的临时口令形态。
	tempPasswordLength = 16
	tempPasswordTTL    = 24 * time.Hour

	// tempPasswordAlphabet 去掉易混淆的 I/O/0/1，避免管理员转述时出错。
	tempPasswordAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789"

	adminMaxKeywordRunes = 64
	adminMaxRemarkRunes  = 255
	adminMaxGroupRunes   = 32
	adminPageSizeDefault = 20
	adminPageSizeMax     = 100
	// adminPageMax 防止 p 取超大值导致 OFFSET 溢出/全表扫描放大。
	adminPageMax = 1_000_000

	// adminIdempotencyKeyMax：wallet_transactions.reference_id 是 varchar(64)。
	adminIdempotencyKeyMax    = 64
	adminIdempotencyKeyHdrMax = 128
	adminIdempotencyTTL       = 24 * time.Hour
	adminIdempotencyLease     = 10 * time.Second

	// userBanTokenKeyPrefix 记录「封禁前处于 enabled 的令牌 ID」。api_tokens
	// 只有 enabled/disabled 两态，没有列记录「是谁禁用的」；解禁时若把所有
	// 令牌重新置 enabled，会违背 TC-F-07-05-02（封禁前已 disabled 的令牌
	// 必须保持 disabled）。因此封禁时把原 enabled 集写入 Redis，解禁时只恢复
	// 这一集合。它只是「恢复提示」而非第二真源：键丢失/TTL 到期时保守地保持
	// 全部 disabled（需管理员手工重新启用），绝不猜测性恢复。
	userBanTokenKeyPrefix = "newtg:user_ban_tokens:"
	userBanTokenTTL       = 30 * 24 * time.Hour

	// tempPasswordKeyPrefix 是临时口令的 24h 有效期标记：reset-password 写入，
	// 登录路径（handlers_auth.go）在 must_change_password=true 时读取并校验，
	// 用户自选新口令成功后删除。缺失即视为过期。
	tempPasswordKeyPrefix = "newtg:temp_password:"
)

// 管理用户仓储的错误哨兵；HTTP 映射集中在 writeUserRepoError。
var (
	ErrAdminUserNotFound    = errors.New("httpapi: admin user not found")
	ErrAdminDuplicate       = errors.New("httpapi: admin username already exists")
	ErrAdminConflict        = errors.New("httpapi: admin user was modified concurrently")
	ErrAdminNotDisabled     = errors.New("httpapi: admin user must be disabled first")
	ErrAdminInsufficient    = errors.New("httpapi: adjustment exceeds current balance")
	ErrAdminOverflow        = errors.New("httpapi: adjustment would overflow the balance")
	ErrAdminNoChange        = errors.New("httpapi: adjustment would not change the balance")
	ErrAdminInvalidGroup    = errors.New("httpapi: group does not exist")
	ErrAdminTwoFANotEnabled = errors.New("httpapi: two-factor authentication is not enabled")
	ErrAdminNotAdmin        = errors.New("httpapi: permissions can only be set for admin users")
)

// AdminUserRecord 是 users 行的安全投影（绝不含 password_hash/totp_secret/
// backup_codes）。AdminPermissions 保留 jsonb 原始字节：nil = SQL NULL = FULL，
// `[]` = 显式空集合，二者语义不同（rbac.hasFullPermissions）。
type AdminUserRecord struct {
	ID                 int64
	Username           string
	DisplayName        *string
	Email              *string
	Role               string
	Status             string
	Balance            int64
	Group              string
	AdminPermissions   json.RawMessage
	TOTPEnabled        bool
	MustChangePassword bool
	Remark             *string
	TokenCount         int64
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// UserListFilter 是 GET /api/users 的查询条件。Sort 已归一化为
// "<column>" 或 "-<column>"，列名白名单在 parseUserListFilter 冻结。
type UserListFilter struct {
	Keyword  string
	Status   string
	Role     string
	Group    string
	Page     int
	PageSize int
	Sort     string
}

// AdminCreateUserInput 是 POST /api/users 的写侧入参。Email 刻意不存在：
// 管理员创建的用户 email 为 NULL（modules-07 F-07-02）。
type AdminCreateUserInput struct {
	Username         string
	DisplayName      *string
	Role             string
	Group            string
	PasswordHash     string
	Remark           *string
	AdminPermissions []string // nil → SQL NULL（仅 role=user 时使用）
}

// AdminUserPatch 是 PATCH /api/users/{id} 的部分更新。Set 标志区分
// 「显式写 NULL」与「未提交该字段」。
type AdminUserPatch struct {
	DisplayNameSet bool
	DisplayName    *string
	Group          *string
	RemarkSet      bool
	Remark         *string
	Role           *string
}

// QuotaAdjustment 是调额事务的结果。Replayed=true 表示同一 idempotency_key
// 命中既有流水（返回首次结果，不再扣加）。BeforeBalance 是事务内读取的
// 调整前余额，供审计对账（override 时不能由 amount 反推）。
type QuotaAdjustment struct {
	UserID        int64
	BeforeBalance int64
	NewBalance    int64
	TransactionID int64
	Replayed      bool
}

// DeletedUser 是硬删除前留存的审计摘要。TokenIDs 用于删除后广播
// token:changed:<id>，让中继侧缓存 ≤1s 失效（GC-11）。
type DeletedUser struct {
	ID            int64
	Username      string
	TokensDeleted int64
	TokenIDs      []int64
}

// AdminUserRepo 是用户管理端点的存储接缝。单测注入内存假实现；生产实现是
// SQLAdminUserRepo。所有方法都必须自行保证原子性/锁语义（GC-5）。
type AdminUserRepo interface {
	ListUsers(ctx context.Context, filter UserListFilter) ([]AdminUserRecord, int64, error)
	CreateUser(ctx context.Context, in AdminCreateUserInput) (AdminUserRecord, error)
	GetUser(ctx context.Context, id int64) (AdminUserRecord, error)
	UpdateUser(ctx context.Context, id int64, patch AdminUserPatch, expectUpdatedAt time.Time) (AdminUserRecord, error)
	SetStatus(ctx context.Context, id int64, status string, restoreTokenIDs []int64) (changed bool, enabledTokenIDs []int64, err error)
	DeleteUser(ctx context.Context, id int64) (DeletedUser, error)
	AdjustQuota(ctx context.Context, id int64, mode string, amount int64, idempotencyKey string) (QuotaAdjustment, error)
	ResetTwoFA(ctx context.Context, id int64) error
	ResetPassword(ctx context.Context, id int64, passwordHash string) error
	SetPermissions(ctx context.Context, id int64, perms []string) (AdminUserRecord, error)
}

// UserHandlerConfig 是用户端点的装配输入。Repo 与 DB 二选一（Repo 优先）；
// Sessions/Redis 必填：封禁/重置必须在 Redis 不可用时 fail-closed。
type UserHandlerConfig struct {
	DB       *sql.DB
	Repo     AdminUserRepo
	Sessions *auth.SessionManager
	Redis    *redis.Client
	Authz    *rbac.AuthzMiddleware
	Audit    *audit.Logger
	// PasswordChecker 用于创建用户时的弱口令/泄露筛查（零值即启用内置词表）。
	PasswordChecker auth.CompromisedPasswordChecker
	Logger          *slog.Logger
}

// UserHandlers 持有用户管理端点依赖，可并发使用。
type UserHandlers struct {
	repo     AdminUserRepo
	sessions *auth.SessionManager
	rdb      *redis.Client
	authz    *rbac.AuthzMiddleware
	audit    *audit.Logger
	checker  auth.CompromisedPasswordChecker
	log      *slog.Logger
}

// 字段级（BOPLA）动作登记。ActionName 是「控制器/动作」维度的稳定标识，
// 与功能级权限键分开：同一权限键下的不同动作可以有不同的可写字段集合。
var (
	adminUserListAction        = rbac.AuthzAction{ResourceKind: "user", ActionName: "list"}
	adminUserCreateAction      = rbac.AuthzAction{ResourceKind: "user", ActionName: "create"}
	adminUserReadAction        = rbac.AuthzAction{ResourceKind: "user", ActionName: "read"}
	adminUserUpdateAction      = rbac.AuthzAction{ResourceKind: "user", ActionName: "update"}
	adminUserStatusAction      = rbac.AuthzAction{ResourceKind: "user", ActionName: "status"}
	adminUserDeleteAction      = rbac.AuthzAction{ResourceKind: "user", ActionName: "delete"}
	adminUserQuotaAction       = rbac.AuthzAction{ResourceKind: "user", ActionName: "quota_adjustment"}
	adminUserReset2FAAction    = rbac.AuthzAction{ResourceKind: "user", ActionName: "reset_2fa"}
	adminUserResetPwdAction    = rbac.AuthzAction{ResourceKind: "user", ActionName: "reset_password"}
	adminUserPermissionsAction = rbac.AuthzAction{ResourceKind: "user", ActionName: "permissions"}
)

// NewUserHandlers 构造用户管理端点。
func NewUserHandlers(cfg UserHandlerConfig) (*UserHandlers, error) {
	if cfg.Sessions == nil {
		return nil, errors.New("httpapi: UserHandlerConfig.Sessions is required")
	}
	if cfg.Redis == nil {
		return nil, errors.New("httpapi: UserHandlerConfig.Redis is required (session revocation, idempotency)")
	}
	repo := cfg.Repo
	if repo == nil {
		if cfg.DB == nil {
			return nil, errors.New("httpapi: UserHandlerConfig.Repo or DB is required")
		}
		repo = NewSQLAdminUserRepo(cfg.DB)
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
	h := &UserHandlers{
		repo:     repo,
		sessions: cfg.Sessions,
		rdb:      cfg.Redis,
		authz:    authz,
		audit:    cfg.Audit,
		checker:  cfg.PasswordChecker,
		log:      log,
	}
	h.registerPolicies()
	return h, nil
}

func (h *UserHandlers) registerPolicies() {
	h.authz.SetWritePolicy(adminUserCreateAction, &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{
		"username":     nil,
		"display_name": nil,
		"role":         nil,
		"group":        nil,
		"password":     nil,
		"remark":       nil,
	}})
	h.authz.SetWritePolicy(adminUserUpdateAction, &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{
		"display_name": nil,
		"group":        nil,
		"remark":       nil,
		"role":         nil,
		"updated_at":   nil,
		// password 登记为叶子不是为了允许写：F-07-03 的验收要求提交该字段
		// 返回 400（而不是字段层的 403）。放行到 handler 后立刻显式拒绝，
		// password_hash/其它任何字段仍由字段层 403 兜底。
		"password": nil,
	}})
	h.authz.SetWritePolicy(adminUserStatusAction, &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{
		"status": nil,
	}})
	h.authz.SetWritePolicy(adminUserQuotaAction, &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{
		"mode":            nil,
		"amount":          nil,
		"idempotency_key": nil,
	}})
	h.authz.SetWritePolicy(adminUserPermissionsAction, &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{
		"admin_permissions": nil,
	}})
	// 无请求体的动作登记空白名单：带任何字段的 body 都会被字段层拒绝，
	// 空对象 {} 则等价于无 body。
	empty := func() *rbac.FieldPolicy {
		return &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{}}
	}
	h.authz.SetWritePolicy(adminUserDeleteAction, empty())
	h.authz.SetWritePolicy(adminUserReset2FAAction, empty())
	h.authz.SetWritePolicy(adminUserResetPwdAction, empty())
}

// RegisterUserRoutes 注册本任务全部用户端点。
//
// wrap 为 nil 时假定 mux 已经在 SessionAuth（及 CSRF/Origin）之后；非 nil 时
// 每个端点先经过 wrap，再进入 rbac 三层判定。ID 寻址端点的目标归属由
// PathValue("id") 在路由匹配后解析（rbac.HandlerWithTarget）。
func (h *UserHandlers) RegisterUserRoutes(mux *http.ServeMux, wrap func(http.Handler) http.Handler) {
	mount := func(pattern string, handler http.Handler) {
		if wrap != nil {
			handler = wrap(handler)
		}
		mux.Handle(pattern, handler)
	}
	fixed := func(required string, action rbac.AuthzAction, next http.HandlerFunc) http.Handler {
		return h.authz.Handler(required, action)(next)
	}
	byID := func(required string, action rbac.AuthzAction, next http.HandlerFunc) http.Handler {
		return h.authz.HandlerWithTarget(required, action, adminTargetUserID)(next)
	}

	mount("GET /api/users", fixed(rbac.PermUsersRead, adminUserListAction, h.handleListUsers))
	mount("POST /api/users", fixed(rbac.PermUsersCreate, adminUserCreateAction, h.idempotentCreate(h.handleCreateUser)))
	mount("GET /api/users/{id}", byID(rbac.PermUsersRead, adminUserReadAction, h.handleGetUser))
	mount("PATCH /api/users/{id}", byID(rbac.PermUsersUpdate, adminUserUpdateAction, h.handleUpdateUser))
	mount("POST /api/users/{id}/status", byID(rbac.PermUsersDisable, adminUserStatusAction, h.handleUserStatus))
	mount("DELETE /api/users/{id}", byID(rbac.PermUsersDelete, adminUserDeleteAction, h.handleDeleteUser))
	mount("POST /api/users/{id}/quota-adjustment",
		byID(rbac.PermUsersAdjustQuota, adminUserQuotaAction, h.handleQuotaAdjustment))
	mount("POST /api/users/{id}/reset-2fa",
		byID(rbac.PermUsersResetSecurity, adminUserReset2FAAction, h.handleReset2FA))
	mount("POST /api/users/{id}/reset-password",
		byID(rbac.PermUsersResetSecurity, adminUserResetPwdAction, h.handleResetPassword))
	mount("PATCH /api/users/{id}/permissions",
		byID(rbac.PermUsersManageRoles, adminUserPermissionsAction, h.handleSetPermissions))
}

// adminTargetUserID 从路由通配符读取被寻址的用户 ID。解析失败返回 0
// （无归属），由业务 handler 统一映射为 404。
func adminTargetUserID(r *http.Request) int64 {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0
	}
	return id
}

// ---------------------------------------------------------------------------
// 视图
// ---------------------------------------------------------------------------

// adminUserView 是用户管理端点的冻结投影。任何口令/TOTP/备用码列都不得
// 出现在这里（GC-6）。
type adminUserView struct {
	ID                 int64           `json:"id"`
	Username           string          `json:"username"`
	DisplayName        *string         `json:"display_name"`
	Email              *string         `json:"email"`
	Role               string          `json:"role"`
	Status             string          `json:"status"`
	Balance            int64           `json:"balance"`
	Group              string          `json:"group"`
	AdminPermissions   json.RawMessage `json:"admin_permissions"` // null = FULL
	TOTPEnabled        bool            `json:"totp_enabled"`
	MustChangePassword bool            `json:"must_change_password"`
	Remark             *string         `json:"remark"`
	TokenCount         int64           `json:"token_count"`
	CreatedAt          time.Time       `json:"created_at"`
	UpdatedAt          time.Time       `json:"updated_at"`
}

func newAdminUserView(rec AdminUserRecord) adminUserView {
	perms := rec.AdminPermissions
	if len(perms) == 0 || string(perms) == "null" {
		perms = json.RawMessage("null")
	}
	return adminUserView{
		ID:                 rec.ID,
		Username:           rec.Username,
		DisplayName:        rec.DisplayName,
		Email:              rec.Email,
		Role:               rec.Role,
		Status:             rec.Status,
		Balance:            rec.Balance,
		Group:              rec.Group,
		AdminPermissions:   perms,
		TOTPEnabled:        rec.TOTPEnabled,
		MustChangePassword: rec.MustChangePassword,
		Remark:             rec.Remark,
		TokenCount:         rec.TokenCount,
		CreatedAt:          rec.CreatedAt,
		UpdatedAt:          rec.UpdatedAt,
	}
}

type adminUserListView struct {
	Items    []adminUserView `json:"items"`
	Total    int64           `json:"total"`
	Page     int             `json:"page"`
	PageSize int             `json:"page_size"`
}

// ---------------------------------------------------------------------------
// 列表 / 详情
// ---------------------------------------------------------------------------

func (h *UserHandlers) handleListUsers(w http.ResponseWriter, r *http.Request) {
	filter, err := parseUserListFilter(r)
	if err != nil {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request", err.Error())
		return
	}
	items, total, err := h.repo.ListUsers(r.Context(), filter)
	if err != nil {
		h.writeUserRepoError(w, "list users", err)
		return
	}
	views := make([]adminUserView, 0, len(items))
	for _, item := range items {
		views = append(views, newAdminUserView(item))
	}
	writeJSON(w, http.StatusOK, adminUserListView{
		Items: views, Total: total, Page: filter.Page, PageSize: filter.PageSize,
	})
}

func (h *UserHandlers) handleGetUser(w http.ResponseWriter, r *http.Request) {
	id, ok := adminPathUserID(w, r)
	if !ok {
		return
	}
	rec, err := h.repo.GetUser(r.Context(), id)
	if err != nil {
		h.writeUserRepoError(w, "get user", err)
		return
	}
	writeJSON(w, http.StatusOK, newAdminUserView(rec))
}

// parseUserListFilter 校验查询参数并归一化排序。非法的枚举/排序列返回 400
// 而不是静默忽略：静默会导致前端以为筛选生效。
func parseUserListFilter(r *http.Request) (UserListFilter, error) {
	q := r.URL.Query()
	filter := UserListFilter{Page: 1, PageSize: adminPageSizeDefault, Sort: "-created_at"}

	if raw := strings.TrimSpace(q.Get("p")); raw != "" {
		page, err := strconv.Atoi(raw)
		if err != nil || page < 1 || page > adminPageMax {
			return UserListFilter{}, fmt.Errorf("p must be between 1 and %d", adminPageMax)
		}
		filter.Page = page
	}
	if raw := strings.TrimSpace(q.Get("page_size")); raw != "" {
		size, err := strconv.Atoi(raw)
		if err != nil || size < 1 || size > adminPageSizeMax {
			return UserListFilter{}, fmt.Errorf("page_size must be between 1 and %d", adminPageSizeMax)
		}
		filter.PageSize = size
	}

	filter.Keyword = strings.TrimSpace(q.Get("keyword"))
	if utf8.RuneCountInString(filter.Keyword) > adminMaxKeywordRunes {
		return UserListFilter{}, fmt.Errorf("keyword must be at most %d characters", adminMaxKeywordRunes)
	}
	filter.Status = strings.TrimSpace(q.Get("status"))
	if filter.Status != "" && filter.Status != "enabled" && filter.Status != "disabled" {
		return UserListFilter{}, errors.New("status must be enabled or disabled")
	}
	filter.Role = strings.TrimSpace(q.Get("role"))
	if filter.Role != "" && filter.Role != rbac.RoleUser && filter.Role != rbac.RoleAdmin {
		return UserListFilter{}, errors.New("role must be user or admin")
	}
	filter.Group = strings.TrimSpace(q.Get("group"))
	if utf8.RuneCountInString(filter.Group) > adminMaxGroupRunes {
		return UserListFilter{}, errors.New("group is too long")
	}
	sort, ok := normalizeUserSort(q.Get("sort"))
	if !ok {
		return UserListFilter{}, errors.New("sort must be one of id, username, balance, created_at, updated_at (prefix - for descending)")
	}
	filter.Sort = sort
	return filter, nil
}

// normalizeUserSort 把客户端排序表达式收敛到白名单列。返回的字符串直接用于
// SQL 拼接，因此绝不能让任意输入通过（ORDER BY 注入）。
func normalizeUserSort(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "-created_at", true
	}
	desc := strings.HasPrefix(raw, "-")
	column := strings.TrimPrefix(raw, "-")
	switch column {
	case "id", "username", "balance", "created_at", "updated_at":
	default:
		return "", false
	}
	if desc {
		return "-" + column, true
	}
	return column, true
}

// ---------------------------------------------------------------------------
// 创建
// ---------------------------------------------------------------------------

type adminCreateUserRequest struct {
	Username    string  `json:"username"`
	DisplayName *string `json:"display_name"`
	Role        string  `json:"role"`
	Group       string  `json:"group"`
	Password    string  `json:"password"`
	Remark      *string `json:"remark"`
}

func (h *UserHandlers) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	p, ok := adminPrincipal(w, r)
	if !ok {
		return
	}
	var req adminCreateUserRequest
	if err := readAuthJSON(r, &req); err != nil {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request", "request body must be a JSON object")
		return
	}

	username := strings.TrimSpace(req.Username)
	if !usernamePattern.MatchString(username) {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"username must be 3-32 characters of [a-zA-Z0-9_-]")
		return
	}
	displayName, err := normalizeOptionalText(req.DisplayName, 32)
	if err != nil {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request", "display_name must be at most 32 characters")
		return
	}
	remark, err := normalizeOptionalText(req.Remark, adminMaxRemarkRunes)
	if err != nil {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request", "remark must be at most 255 characters")
		return
	}
	role := strings.TrimSpace(req.Role)
	if role == "" {
		role = rbac.RoleUser
	}
	if role != rbac.RoleUser && role != rbac.RoleAdmin {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request", "role must be user or admin")
		return
	}
	group := strings.TrimSpace(req.Group)
	if group == "" {
		group = "default"
	}
	if utf8.RuneCountInString(group) > adminMaxGroupRunes {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request", "group is too long")
		return
	}
	if !auth.ValidatePasswordLength(req.Password) {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			fmt.Sprintf("password must be %d-%d characters", auth.PWDMinLen, auth.PWDMaxLen))
		return
	}
	if h.checker.CheckWithContext(req.Password, username) {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"password is too weak or has appeared in a breach")
		return
	}
	// 创建 admin 等于授予角色 + 一组默认权限。必须走 AuthorizeGrant：
	// 执行者既要持有 users:manage-roles，也要持有默认权限集的超集；否则只持
	// users:create 的管理员可以创建自己知道口令的 admin 账号完成提权。
	if role == rbac.RoleAdmin {
		if decision := h.authz.AuthorizeGrant(*p, defaultAdminPermissions()); decision.Effect != rbac.EffectAllow {
			writeAdminProblem(w, http.StatusForbidden, authProblemForbidden, "Forbidden",
				"creating an admin requires users:manage-roles and a superset of the default permissions")
			return
		}
	}
	passwordHash, err := auth.HashPassword(req.Password)
	if err != nil {
		h.log.Error("hash admin-created password failed", "error", err)
		writeAdminProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error", "")
		return
	}
	var permissions []string
	if role == rbac.RoleAdmin {
		// 新 admin 只读起步（F-07-08 矩阵默认值）。绝不能留 NULL——NULL 在
		// RBAC 语义里是 FULL，等于 users:create 直接造出一个全权管理员。
		permissions = defaultAdminPermissions()
	}

	rec, err := h.repo.CreateUser(r.Context(), AdminCreateUserInput{
		Username:         username,
		DisplayName:      displayName,
		Role:             role,
		Group:            group,
		PasswordHash:     passwordHash,
		Remark:           remark,
		AdminPermissions: permissions,
	})
	if err != nil {
		h.writeUserRepoError(w, "create user", err)
		return
	}
	h.auditEvent(r, audit.ActionUserCreate, p.UserID, map[string]any{
		"user_id": rec.ID, "username": rec.Username, "role": rec.Role, "group": rec.Group,
	}, nil, adminUserSnapshot(rec), http.StatusCreated)
	writeJSON(w, http.StatusCreated, newAdminUserView(rec))
}

// ---------------------------------------------------------------------------
// 编辑
// ---------------------------------------------------------------------------

func (h *UserHandlers) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	p, ok := adminPrincipal(w, r)
	if !ok {
		return
	}
	id, ok := adminPathUserID(w, r)
	if !ok {
		return
	}
	body, err := readRawObject(r)
	if err != nil {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request", "request body must be a JSON object")
		return
	}
	// F-07-03 验收：编辑接口不含口令字段，提交 password → 400。字段层把
	// password 放行到此处正是为了给出这个明确的 400 而不是笼统的 403。
	if _, present := body["password"]; present {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"password cannot be set here; use POST /api/users/{id}/reset-password")
		return
	}
	rawToken, present := body["updated_at"]
	if !present {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"updated_at is required for optimistic locking")
		return
	}
	var token string
	if err := json.Unmarshal(rawToken, &token); err != nil || strings.TrimSpace(token) == "" {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"updated_at must be an RFC 3339 timestamp")
		return
	}
	expectUpdatedAt, err := time.Parse(time.RFC3339Nano, token)
	if err != nil {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"updated_at must be an RFC 3339 timestamp")
		return
	}

	var patch AdminUserPatch
	if raw, present := body["display_name"]; present {
		value, valid := decodeNullableText(raw)
		if !valid {
			writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request", "display_name must be a string or null")
			return
		}
		normalized, err := normalizeOptionalText(value, 32)
		if err != nil {
			writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request", "display_name must be at most 32 characters")
			return
		}
		patch.DisplayNameSet = true
		patch.DisplayName = normalized
	}
	if raw, present := body["group"]; present {
		var group string
		if string(raw) == "null" || json.Unmarshal(raw, &group) != nil {
			writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request", "group must be a string")
			return
		}
		group = strings.TrimSpace(group)
		if group == "" {
			group = "default"
		}
		if utf8.RuneCountInString(group) > adminMaxGroupRunes {
			writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request", "group is too long")
			return
		}
		patch.Group = &group
	}
	if raw, present := body["remark"]; present {
		value, valid := decodeNullableText(raw)
		if !valid {
			writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request", "remark must be a string or null")
			return
		}
		normalized, err := normalizeOptionalText(value, adminMaxRemarkRunes)
		if err != nil {
			writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request", "remark must be at most 255 characters")
			return
		}
		patch.RemarkSet = true
		patch.Remark = normalized
	}
	if raw, present := body["role"]; present {
		var role string
		if string(raw) == "null" || json.Unmarshal(raw, &role) != nil {
			writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request", "role must be user or admin")
			return
		}
		role = strings.TrimSpace(role)
		if role != rbac.RoleUser && role != rbac.RoleAdmin {
			writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request", "role must be user or admin")
			return
		}
		if p.UserID == id {
			// F-07-06：不能对自己执行降权（服务端兜底）。
			writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
				"you cannot change your own role")
			return
		}
		if !rbac.HasPermission(*p, rbac.PermUsersManageRoles) {
			writeAdminProblem(w, http.StatusForbidden, authProblemForbidden, "Forbidden",
				"changing roles requires users:manage-roles")
			return
		}
		patch.Role = &role
	}
	if !patch.DisplayNameSet && patch.Group == nil && !patch.RemarkSet && patch.Role == nil {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request", "no fields to update")
		return
	}

	before, err := h.repo.GetUser(r.Context(), id)
	if err != nil {
		h.writeUserRepoError(w, "read user before update", err)
		return
	}
	if patch.Role != nil {
		// 角色是会话里固定的事实（SessionAuth 从登录时写入的 auth_level 读取），
		// 降权后旧会话仍带 Role=admin；即使权限装载层立即生效，脏会话也不应
		// 继续存在（F-07-06 / IS-B-5）。升降双向都先吊销：失败即 503 且不改角色。
		if err := h.sessions.RevokeUser(r.Context(), id, ""); err != nil {
			h.writeServiceUnavailable(w, "revoke sessions for role change", err)
			return
		}
		// 提权会写入默认权限集：执行者必须能授予该集合（AuthorizeGrant 同时
		// 校验 users:manage-roles 与超集），否则 users:update 就能造出超出
		// 自身范围的 admin。
		if *patch.Role == rbac.RoleAdmin && before.Role != rbac.RoleAdmin {
			if decision := h.authz.AuthorizeGrant(*p, defaultAdminPermissions()); decision.Effect != rbac.EffectAllow {
				writeAdminProblem(w, http.StatusForbidden, authProblemForbidden, "Forbidden",
					"promoting an admin requires users:manage-roles and a superset of the default permissions")
				return
			}
		}
	}
	updated, err := h.repo.UpdateUser(r.Context(), id, patch, expectUpdatedAt)
	if err != nil {
		h.writeUserRepoError(w, "update user", err)
		return
	}
	h.auditEvent(r, audit.ActionUserUpdate, p.UserID, map[string]any{
		"user_id": id, "changed_fields": adminChangedUserFields(before, updated),
	}, adminUserSnapshot(before), adminUserSnapshot(updated))
	writeJSON(w, http.StatusOK, newAdminUserView(updated))
}

// ---------------------------------------------------------------------------
// 封禁 / 解禁
// ---------------------------------------------------------------------------

type adminUserStatusRequest struct {
	Status string `json:"status"`
}

type adminUserStatusResponse struct {
	ID     int64  `json:"id"`
	Status string `json:"status"`
}

func (h *UserHandlers) handleUserStatus(w http.ResponseWriter, r *http.Request) {
	p, ok := adminPrincipal(w, r)
	if !ok {
		return
	}
	id, ok := adminPathUserID(w, r)
	if !ok {
		return
	}
	var req adminUserStatusRequest
	if err := readAuthJSON(r, &req); err != nil {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request", "request body must be a JSON object")
		return
	}
	status := strings.TrimSpace(req.Status)
	if status != "enabled" && status != "disabled" {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request", "status must be enabled or disabled")
		return
	}

	if status == "disabled" {
		// 先吊销 Redis 会话再改 DB（fail-closed）：Redis 故障 → 503 且不动 DB；
		// DB 失败 → 会话已失效（安全方向）。绝不允许「已封禁但旧会话仍可用」。
		if err := h.sessions.RevokeUser(r.Context(), id, ""); err != nil {
			h.writeServiceUnavailable(w, "revoke sessions for disable", err)
			return
		}
		changed, enabledTokenIDs, err := h.repo.SetStatus(r.Context(), id, status, nil)
		if err != nil {
			h.writeUserRepoError(w, "disable user", err)
			return
		}
		if changed && len(enabledTokenIDs) > 0 {
			// 令牌已从 enabled → disabled：广播即时失效（GC-11）。广播失败只
			// 记日志，缓存 60s TTL 兜底。
			h.invalidateTokenCache(r.Context(), enabledTokenIDs)
			// 记录解禁时需要恢复的令牌集合。写失败只记日志：用户已经封禁、
			// 令牌已禁用；丢失恢复信息的结果是解禁后令牌保持 disabled（保守）。
			if err := h.storeBanRestoreSet(r.Context(), id, enabledTokenIDs); err != nil {
				h.log.Error("record ban restore set failed", "user_id", id, "error", err)
			}
		}
		if changed {
			h.auditEvent(r, audit.ActionUserDisable, p.UserID, map[string]any{
				"user_id": id, "tokens_disabled": len(enabledTokenIDs),
			}, nil, nil)
		}
		writeJSON(w, http.StatusOK, adminUserStatusResponse{ID: id, Status: status})
		return
	}

	// 解禁：恢复封禁前处于 enabled 的令牌（TC-F-07-05-02 要求原本 disabled
	// 的令牌保持 disabled）。Redis 读取失败 → 503 fail-closed，宁可不解禁。
	restore, err := h.loadBanRestoreSet(r.Context(), id)
	if err != nil {
		h.writeServiceUnavailable(w, "read ban restore set", err)
		return
	}
	changed, _, err := h.repo.SetStatus(r.Context(), id, status, restore)
	if err != nil {
		h.writeUserRepoError(w, "enable user", err)
		return
	}
	if changed {
		// 恢复的令牌从 disabled → enabled：同样广播，避免缓存里残留失效态
		// 导致解禁后最长 60s 仍被拒绝（GC-11）。
		h.invalidateTokenCache(r.Context(), restore)
		_ = h.rdb.Del(r.Context(), userBanTokenKeyPrefix+strconv.FormatInt(id, 10)).Err()
		h.auditEvent(r, audit.ActionUserEnable, p.UserID, map[string]any{
			"user_id": id, "tokens_restored": len(restore),
		}, nil, nil, http.StatusOK)
	}
	writeJSON(w, http.StatusOK, adminUserStatusResponse{ID: id, Status: status})
}

func (h *UserHandlers) storeBanRestoreSet(ctx context.Context, userID int64, tokenIDs []int64) error {
	raw, err := json.Marshal(tokenIDs)
	if err != nil {
		return err
	}
	return h.rdb.Set(ctx, userBanTokenKeyPrefix+strconv.FormatInt(userID, 10), raw, userBanTokenTTL).Err()
}

func (h *UserHandlers) loadBanRestoreSet(ctx context.Context, userID int64) ([]int64, error) {
	raw, err := h.rdb.Get(ctx, userBanTokenKeyPrefix+strconv.FormatInt(userID, 10)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []int64
	if err := json.Unmarshal(raw, &ids); err != nil {
		// 损坏的恢复集不能猜：返回错误让解禁 503；管理员可通过再次封禁/解禁
		// 或手工启用令牌恢复。
		return nil, fmt.Errorf("httpapi: corrupt ban restore set for user %d: %w", userID, err)
	}
	return ids, nil
}

// ---------------------------------------------------------------------------
// 删除
// ---------------------------------------------------------------------------

func (h *UserHandlers) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	p, ok := adminPrincipal(w, r)
	if !ok {
		return
	}
	id, ok := adminPathUserID(w, r)
	if !ok {
		return
	}
	if p.UserID == id {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request", "you cannot delete your own account")
		return
	}
	// 先做存在性/状态预检，避免对 enabled 用户返回 409 时先吊销了其会话。
	target, err := h.repo.GetUser(r.Context(), id)
	if err != nil {
		h.writeUserRepoError(w, "read user before delete", err)
		return
	}
	if target.Status != "disabled" {
		writeAdminProblem(w, http.StatusConflict, authProblemConflict, "Conflict",
			"the user must be disabled before deletion")
		return
	}
	// 删行后会话身份无法再被 DB 校验，必须在硬删前吊销，否则残留会话仍可按
	// 已删除用户身份操作（SessionAuth 不查 users 行）。
	if err := h.sessions.RevokeUser(r.Context(), id, ""); err != nil {
		h.writeServiceUnavailable(w, "revoke sessions for delete", err)
		return
	}
	deleted, err := h.repo.DeleteUser(r.Context(), id)
	if err != nil {
		h.writeUserRepoError(w, "delete user", err)
		return
	}
	// 令牌行已删除；广播让中继侧缓存立即失效（GC-11），与自助删除同口径。
	h.invalidateTokenCache(r.Context(), deleted.TokenIDs)
	// 流水/订单/日志按 GC-4 保留；它们只存 user_id，不含用户名/邮箱。审计里
	// 记录匿名化后的 handle，让删除事件仍可追溯而不复述 PII。
	h.auditEvent(r, audit.ActionUserDelete, p.UserID, map[string]any{
		"deleted_user_id":     deleted.ID,
		"anonymized_username": fmt.Sprintf("deleted_%d", deleted.ID),
		"tokens_deleted":      deleted.TokensDeleted,
	}, nil, nil, http.StatusNoContent)
	w.WriteHeader(http.StatusNoContent)
}

// invalidateTokenCache 通过 token:changed:<id> 广播即时失效（GC-11）。
// pub/sub 失败只记日志：缓存 60s TTL 是兜底，不能因此回滚已经完成的写操作。
func (h *UserHandlers) invalidateTokenCache(ctx context.Context, tokenIDs []int64) {
	for _, tokenID := range tokenIDs {
		if err := h.rdb.Publish(ctx,
			fmt.Sprintf("token:changed:%d", tokenID),
			strconv.FormatInt(tokenID, 10)).Err(); err != nil {
			h.log.Warn("publish token invalidation failed", "token_id", tokenID, "error", err)
		}
	}
}

// ---------------------------------------------------------------------------
// 额度调整（GC-5）
// ---------------------------------------------------------------------------

type adminQuotaAdjustmentRequest struct {
	Mode           string `json:"mode"`
	Amount         int64  `json:"amount"`
	IdempotencyKey string `json:"idempotency_key"`
}

type adminQuotaAdjustmentResponse struct {
	UserID        int64 `json:"user_id"`
	NewBalance    int64 `json:"new_balance"`
	TransactionID int64 `json:"transaction_id"`
}

func (h *UserHandlers) handleQuotaAdjustment(w http.ResponseWriter, r *http.Request) {
	p, ok := adminPrincipal(w, r)
	if !ok {
		return
	}
	id, ok := adminPathUserID(w, r)
	if !ok {
		return
	}
	var req adminQuotaAdjustmentRequest
	if err := readAuthJSON(r, &req); err != nil {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request", "request body must be a JSON object")
		return
	}
	mode := strings.TrimSpace(req.Mode)
	switch mode {
	case "add", "subtract":
		if req.Amount < 1 {
			writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request", "amount must be at least 1")
			return
		}
	case "override":
		if req.Amount < 0 {
			writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request", "amount must not be negative")
			return
		}
	default:
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request", "mode must be add, subtract or override")
		return
	}
	key := strings.TrimSpace(req.IdempotencyKey)
	if key == "" || len(key) > adminIdempotencyKeyMax {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			fmt.Sprintf("idempotency_key is required and must be at most %d characters", adminIdempotencyKeyMax))
		return
	}

	result, err := h.repo.AdjustQuota(r.Context(), id, mode, req.Amount, key)
	if err != nil {
		h.writeUserRepoError(w, "adjust quota", err)
		return
	}
	if !result.Replayed {
		h.auditEvent(r, audit.ActionUserQuotaAdjust, p.UserID, map[string]any{
			"user_id": id, "mode": mode, "amount": req.Amount,
			"before_balance": result.BeforeBalance, "new_balance": result.NewBalance,
			"transaction_id": result.TransactionID,
		}, nil, nil)
	}
	writeJSON(w, http.StatusOK, adminQuotaAdjustmentResponse{
		UserID: result.UserID, NewBalance: result.NewBalance, TransactionID: result.TransactionID,
	})
}

// ---------------------------------------------------------------------------
// 账号恢复
// ---------------------------------------------------------------------------

func (h *UserHandlers) handleReset2FA(w http.ResponseWriter, r *http.Request) {
	p, ok := adminPrincipal(w, r)
	if !ok {
		return
	}
	id, ok := adminPathUserID(w, r)
	if !ok {
		return
	}
	if p.UserID == id {
		// 管理员不得通过管理端点重置自己的 2FA：那会绕过自助/再认证门。
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"you cannot reset your own two-factor authentication here")
		return
	}
	target, err := h.repo.GetUser(r.Context(), id)
	if err != nil {
		h.writeUserRepoError(w, "read user before reset-2fa", err)
		return
	}
	if !target.TOTPEnabled {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"two-factor authentication is not enabled for this user")
		return
	}
	if err := h.sessions.RevokeUser(r.Context(), id, ""); err != nil {
		h.writeServiceUnavailable(w, "revoke sessions for reset-2fa", err)
		return
	}
	if err := h.repo.ResetTwoFA(r.Context(), id); err != nil {
		h.writeUserRepoError(w, "reset 2fa", err)
		return
	}
	h.auditEvent(r, audit.ActionUserReset2FA, p.UserID, map[string]any{"user_id": id}, nil, nil, http.StatusNoContent)
	w.WriteHeader(http.StatusNoContent)
}

type adminResetPasswordResponse struct {
	TempPassword string `json:"temp_password"`
}

func (h *UserHandlers) handleResetPassword(w http.ResponseWriter, r *http.Request) {
	p, ok := adminPrincipal(w, r)
	if !ok {
		return
	}
	id, ok := adminPathUserID(w, r)
	if !ok {
		return
	}
	if p.UserID == id {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"you cannot reset your own password here")
		return
	}
	if _, err := h.repo.GetUser(r.Context(), id); err != nil {
		h.writeUserRepoError(w, "read user before reset-password", err)
		return
	}
	// 与封禁同序：先吊销全部会话，Redis 故障时整单 503 且不改口令。
	if err := h.sessions.RevokeUser(r.Context(), id, ""); err != nil {
		h.writeServiceUnavailable(w, "revoke sessions for reset-password", err)
		return
	}
	tempPassword, err := h.generateTempPassword()
	if err != nil {
		h.log.Error("generate temp password failed", "error", err)
		writeAdminProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error", "")
		return
	}
	hash, err := auth.HashPassword(tempPassword)
	if err != nil {
		h.log.Error("hash temp password failed", "error", err)
		writeAdminProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error", "")
		return
	}
	if err := h.repo.ResetPassword(r.Context(), id, hash); err != nil {
		h.writeUserRepoError(w, "reset password", err)
		return
	}
	// 24h 有效期标记。登录侧（Task 6）目前尚未消费该键；写入它是为了让
	// 登录侧接线后无需改本端点。标记写失败不影响本次重置：口令已更换，
	// 用户仍需在首登时改密（must_change_password）。
	if err := h.rdb.Set(r.Context(), tempPasswordKeyPrefix+strconv.FormatInt(id, 10),
		"1", tempPasswordTTL).Err(); err != nil {
		h.log.Error("record temp password TTL failed", "user_id", id, "error", err)
	}
	// 审计只记录「发生了重置」，绝不包含临时口令、哈希或任何口令材料。
	h.auditEvent(r, audit.ActionUserResetPassword, p.UserID, map[string]any{
		"user_id": id, "must_change_password": true, "temp_password_ttl_seconds": int64(tempPasswordTTL.Seconds()),
	}, nil, nil, http.StatusOK)
	// 临时口令只应出现在本次响应：禁止任何中间层缓存。
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, adminResetPasswordResponse{TempPassword: tempPassword})
}

// generateTempPassword 用 CSPRNG 生成 16 位临时口令。字母表去掉易混淆字符；
// 若（极小概率）命中弱口令词表则重试，保证临时口令同样满足 F-07-12 的
// 「仍遵循口令策略」要求。复用端点配置的 checker（含站点上下文词）。
func (h *UserHandlers) generateTempPassword() (string, error) {
	for attempt := 0; attempt < 5; attempt++ {
		out := make([]byte, tempPasswordLength)
		for i := range out {
			n, err := rand.Int(rand.Reader, big.NewInt(int64(len(tempPasswordAlphabet))))
			if err != nil {
				return "", fmt.Errorf("httpapi: generate temp password: %w", err)
			}
			out[i] = tempPasswordAlphabet[n.Int64()]
		}
		candidate := string(out)
		if !h.checker.Check(candidate) {
			return candidate, nil
		}
	}
	return "", errors.New("httpapi: unable to generate an acceptable temp password")
}

// ---------------------------------------------------------------------------
// 权限矩阵
// ---------------------------------------------------------------------------

func (h *UserHandlers) handleSetPermissions(w http.ResponseWriter, r *http.Request) {
	p, ok := adminPrincipal(w, r)
	if !ok {
		return
	}
	id, ok := adminPathUserID(w, r)
	if !ok {
		return
	}
	if p.UserID == id {
		// F-07-09 + 本任务 brief：不能修改自己的权限矩阵（防自提权/自降权）。
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"you cannot change your own permissions")
		return
	}
	body, err := readRawObject(r)
	if err != nil {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request", "request body must be a JSON object")
		return
	}
	raw, present := body["admin_permissions"]
	if !present || string(raw) == "null" {
		// null 在 RBAC 语义里是 FULL；允许匿名的 null→FULL 等于绕过超集校验
		// 的提权通道，必须使用显式的 ["*"]（仅 FULL 管理员可授予）。
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"admin_permissions must be an array of permission keys (use [\"*\"] for full access)")
		return
	}
	var requested []string
	if err := json.Unmarshal(raw, &requested); err != nil || requested == nil {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"admin_permissions must be an array of permission keys")
		return
	}

	// 超集校验（TC-B-04）：授予集合必须是调用者自身权限的子集；未知键、"*"
	// 由非 FULL 调用者授予一律 403，且不写库。
	if decision := h.authz.AuthorizeGrant(*p, requested); decision.Effect != rbac.EffectAllow {
		writeAdminProblem(w, http.StatusForbidden, authProblemForbidden, "Forbidden",
			"you cannot grant permissions beyond your own")
		return
	}

	target, err := h.repo.GetUser(r.Context(), id)
	if err != nil {
		h.writeUserRepoError(w, "read user before permissions update", err)
		return
	}
	if target.Role != rbac.RoleAdmin {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"permissions can only be set for admin users")
		return
	}
	updated, err := h.repo.SetPermissions(r.Context(), id, requested)
	if err != nil {
		h.writeUserRepoError(w, "set permissions", err)
		return
	}
	h.auditEvent(r, audit.ActionUserUpdate, p.UserID, map[string]any{
		"user_id": id, "changed_fields": []string{"admin_permissions"},
	}, adminUserSnapshot(target), adminUserSnapshot(updated))
	writeJSON(w, http.StatusOK, newAdminUserView(updated))
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

func adminPathUserID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeAdminProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found", "")
		return 0, false
	}
	return id, true
}

func adminPrincipal(w http.ResponseWriter, r *http.Request) (*auth.Principal, bool) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok || p == nil || p.UserID == 0 {
		// 鉴权中间件缺失/顺序错误：fail-closed，绝不把零值身份当匿名。
		writeAdminProblem(w, http.StatusUnauthorized, problemTypeUnauthorized, "Unauthorized", "")
		return nil, false
	}
	return p, true
}

// writeAdminProblem 输出带可选 detail 的 RFC 7807 Problem（GC-6）。
func writeAdminProblem(w http.ResponseWriter, status int, problemType, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	problem := dto.Problem{Type: problemType, Title: title, Status: status}
	if detail != "" {
		problem.Detail = &detail
	}
	// 响应已开始写出；编码失败（客户端断开）无法再改状态码。
	_ = json.NewEncoder(w).Encode(problem)
}

func (h *UserHandlers) writeServiceUnavailable(w http.ResponseWriter, op string, err error) {
	h.log.Error(op+" failed", "error", err)
	w.Header().Set("Retry-After", "5")
	writeAdminProblem(w, http.StatusServiceUnavailable, authProblemServiceUnavailable,
		"Service temporarily unavailable", "")
}

// writeUserRepoError 把仓储错误映射为 HTTP 语义。409 用于乐观锁冲突与
// 「必须先封禁」；400 用于可修复的输入/余额问题；其余 500。
func (h *UserHandlers) writeUserRepoError(w http.ResponseWriter, op string, err error) {
	switch {
	case errors.Is(err, ErrAdminUserNotFound):
		writeAdminProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found", "")
	case errors.Is(err, ErrAdminDuplicate):
		writeAdminProblem(w, http.StatusConflict, authProblemConflict, "Conflict", "the username is already taken")
	case errors.Is(err, ErrAdminConflict):
		writeAdminProblem(w, http.StatusConflict, authProblemConflict, "Conflict",
			"the user was modified by someone else; refresh and retry")
	case errors.Is(err, ErrAdminNotDisabled):
		writeAdminProblem(w, http.StatusConflict, authProblemConflict, "Conflict",
			"the user must be disabled before deletion")
	case errors.Is(err, ErrAdminInsufficient):
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"the amount exceeds the current balance")
	case errors.Is(err, ErrAdminOverflow):
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"the adjustment would overflow the balance")
	case errors.Is(err, ErrAdminNoChange):
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"the adjustment would not change the balance")
	case errors.Is(err, ErrAdminInvalidGroup):
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"the group does not exist")
	case errors.Is(err, ErrAdminTwoFANotEnabled):
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"two-factor authentication is not enabled for this user")
	case errors.Is(err, ErrAdminNotAdmin):
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"permissions can only be set for admin users")
	default:
		h.log.Error(op+" failed", "error", err)
		writeAdminProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error", "")
	}
}

// normalizeOptionalText 裁剪并校验可选文本；空白字符串规范化为 NULL。
func normalizeOptionalText(value *string, maxRunes int) (*string, error) {
	if value == nil {
		return nil, nil
	}
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return nil, nil
	}
	if utf8.RuneCountInString(trimmed) > maxRunes {
		return nil, errors.New("text too long")
	}
	return &trimmed, nil
}

// decodeNullableText 解析 "string" 或 null。ok=false 表示类型非法。
func decodeNullableText(raw json.RawMessage) (*string, bool) {
	if string(raw) == "null" {
		return nil, true
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, false
	}
	return &value, true
}

// adminUserSnapshot 是审计 before/after 的安全快照：绝不含口令/TOTP/备用码。
func adminUserSnapshot(rec AdminUserRecord) map[string]any {
	snapshot := map[string]any{
		"id":                   rec.ID,
		"username":             rec.Username,
		"display_name":         rec.DisplayName,
		"email":                rec.Email,
		"role":                 rec.Role,
		"status":               rec.Status,
		"balance":              rec.Balance,
		"group":                rec.Group,
		"totp_enabled":         rec.TOTPEnabled,
		"must_change_password": rec.MustChangePassword,
		"remark":               rec.Remark,
		"updated_at":           rec.UpdatedAt,
	}
	if len(rec.AdminPermissions) > 0 && string(rec.AdminPermissions) != "null" {
		snapshot["admin_permissions"] = rec.AdminPermissions
	} else {
		snapshot["admin_permissions"] = nil
	}
	return snapshot
}

func adminChangedUserFields(before, after AdminUserRecord) []string {
	beforeSnap := adminUserSnapshot(before)
	afterSnap := adminUserSnapshot(after)
	changed := make([]string, 0, 4)
	for key, beforeValue := range beforeSnap {
		if key == "id" || key == "updated_at" || key == "username" {
			continue
		}
		if !adminJSONEqual(beforeValue, afterSnap[key]) {
			changed = append(changed, key)
		}
	}
	return changed
}

func adminJSONEqual(a, b any) bool {
	rawA, errA := json.Marshal(a)
	rawB, errB := json.Marshal(b)
	return errA == nil && errB == nil && bytes.Equal(rawA, rawB)
}

// auditEvent 写入管理面审计。审计失败不阻断业务（日志库独立且无跨库事务），
// 由运维告警接手；detail/before/after 必须只含安全字段。status 是本次请求
// 实际写出的 HTTP 状态码（201/204 等），缺省按 200。
func (h *UserHandlers) auditEvent(r *http.Request, action string, actor int64, detail, before, after map[string]any, status ...int) {
	if h.audit == nil {
		return
	}
	statusCode := http.StatusOK
	if len(status) > 0 && status[0] > 0 {
		statusCode = status[0]
	}
	event := audit.Event{
		Action:     action,
		Actor:      actor,
		IP:         tokenRemoteIP(r),
		UserAgent:  r.UserAgent(),
		Method:     r.Method,
		Route:      r.Pattern,
		StatusCode: statusCode,
		Detail:     adminMarshalRaw(detail),
		Before:     adminMarshalRaw(before),
		After:      adminMarshalRaw(after),
	}
	if err := h.audit.LogEvent(r.Context(), event); err != nil {
		h.log.Warn("admin user audit write failed", "action", action, "actor", actor, "error", err)
	}
}

func adminMarshalRaw(value map[string]any) json.RawMessage {
	if value == nil {
		return nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return raw
}

// ---------------------------------------------------------------------------
// 创建端点幂等（Idempotency-Key）
// ---------------------------------------------------------------------------

type adminCachedHTTPResponse struct {
	Status      int    `json:"status"`
	ContentType string `json:"content_type"`
	Body        string `json:"body"`
	Fingerprint string `json:"fingerprint"`
}

// idempotentCreate 为 POST /api/users 提供 24h 幂等重放。同一 key + 同一请求体
// 返回首次响应；不同请求体复用 key → 422。实现与令牌端点同构（Redis 缓存 +
// 短锁），键里带操作者 ID，避免跨管理员共享幂等空间。
func (h *UserHandlers) idempotentCreate(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := auth.PrincipalFrom(r.Context())
		if !ok || p == nil || p.UserID == 0 {
			writeAdminProblem(w, http.StatusUnauthorized, problemTypeUnauthorized, "Unauthorized", "")
			return
		}
		key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		if key == "" {
			next(w, r)
			return
		}
		if len(key) > adminIdempotencyKeyHdrMax {
			writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
				"Idempotency-Key is too long")
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxAuthBodyBytes))
		if err != nil {
			writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request", "unable to read request body")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		fingerprint := tokenBodyFingerprint(body)

		ctx := r.Context()
		cacheKey := fmt.Sprintf("newtg:idem:admin_user:create:%d:%s", p.UserID, key)
		if cached, ok := h.loadIdempotent(ctx, cacheKey); ok {
			replayAdminIdempotent(w, cached, fingerprint)
			return
		}
		lockKey := cacheKey + ":lock"
		acquired, lockErr := h.rdb.SetNX(ctx, lockKey, "1", adminIdempotencyLease).Result()
		if lockErr == nil && !acquired {
			// 并发同 key：等首个请求落结果（≤3s），避免重复创建。
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				select {
				case <-ctx.Done():
					writeAdminProblem(w, http.StatusConflict, authProblemConflict, "Conflict",
						"request cancelled before the idempotent result was ready")
					return
				case <-time.After(25 * time.Millisecond):
				}
				if cached, ok := h.loadIdempotent(ctx, cacheKey); ok {
					replayAdminIdempotent(w, cached, fingerprint)
					return
				}
			}
			writeAdminProblem(w, http.StatusConflict, authProblemConflict, "Conflict",
				"a request with the same Idempotency-Key is still in progress; retry shortly")
			return
		}

		buffer := newAdminBufferedResponse()
		next(buffer, r)
		if buffer.status >= 200 && buffer.status < 300 {
			h.saveIdempotent(ctx, cacheKey, adminCachedHTTPResponse{
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

func replayAdminIdempotent(w http.ResponseWriter, cached adminCachedHTTPResponse, fingerprint string) {
	if cached.Fingerprint != fingerprint {
		writeAdminProblem(w, http.StatusUnprocessableEntity, problemTypeIdempotencyReused,
			"Idempotency key reused", "the same Idempotency-Key cannot be used with a different request body")
		return
	}
	if cached.ContentType != "" {
		w.Header().Set("Content-Type", cached.ContentType)
	}
	w.WriteHeader(cached.Status)
	_, _ = io.WriteString(w, cached.Body)
}

func (h *UserHandlers) loadIdempotent(ctx context.Context, key string) (adminCachedHTTPResponse, bool) {
	raw, err := h.rdb.Get(ctx, key).Bytes()
	if err != nil {
		return adminCachedHTTPResponse{}, false
	}
	var cached adminCachedHTTPResponse
	if err := json.Unmarshal(raw, &cached); err != nil {
		return adminCachedHTTPResponse{}, false
	}
	return cached, true
}

func (h *UserHandlers) saveIdempotent(ctx context.Context, key string, cached adminCachedHTTPResponse) {
	raw, err := json.Marshal(cached)
	if err != nil {
		return
	}
	// 幂等缓存是尽力而为：Redis 故障时请求已经成功，不能因此回滚业务。
	_ = h.rdb.Set(ctx, key, raw, adminIdempotencyTTL).Err()
}

type adminBufferedResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newAdminBufferedResponse() *adminBufferedResponse {
	return &adminBufferedResponse{header: make(http.Header)}
}

func (b *adminBufferedResponse) Header() http.Header { return b.header }

func (b *adminBufferedResponse) WriteHeader(status int) {
	if b.status == 0 {
		b.status = status
	}
}

func (b *adminBufferedResponse) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	return b.body.Write(p)
}

func (b *adminBufferedResponse) copyTo(w http.ResponseWriter) {
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

// ---------------------------------------------------------------------------
// SQL 实现
// ---------------------------------------------------------------------------

// defaultAdminPermissions 是新 admin 的只读起步权限（F-07-08 矩阵默认值）。
// 返回副本：调用方可以安全地把它交给 AuthorizeGrant 或写库。
func defaultAdminPermissions() []string {
	return []string{rbac.PermUsersRead, rbac.PermLogsRead}
}

// adminUserColumns 是 AdminUserRecord 的基础列序；token_count 由查询单独
// 追加一列（列表/详情用相关子查询，写入路径用 0 占位）。
const adminUserColumns = `id, username, display_name, email, role, status, balance, "group",
	admin_permissions, totp_enabled, must_change_password, remark, created_at, updated_at`

// SQLAdminUserRepo 是 AdminUserRepo 的 PostgreSQL 实现（database/sql + pgx stdlib）。
type SQLAdminUserRepo struct{ db *sql.DB }

// NewSQLAdminUserRepo 构造管理用户仓储。
func NewSQLAdminUserRepo(db *sql.DB) *SQLAdminUserRepo { return &SQLAdminUserRepo{db: db} }

func scanAdminUserRecord(row rowScanner) (AdminUserRecord, error) {
	var (
		rec   AdminUserRecord
		perms []byte
	)
	err := row.Scan(&rec.ID, &rec.Username, &rec.DisplayName, &rec.Email, &rec.Role,
		&rec.Status, &rec.Balance, &rec.Group, &perms, &rec.TOTPEnabled,
		&rec.MustChangePassword, &rec.Remark, &rec.CreatedAt, &rec.UpdatedAt,
		&rec.TokenCount)
	if err != nil {
		return AdminUserRecord{}, err
	}
	// jsonb NULL 与 JSON null 都归一为「NULL = FULL」；其余原样保留（含 []）。
	if len(perms) > 0 && string(perms) != "null" {
		rec.AdminPermissions = append(json.RawMessage(nil), perms...)
	}
	return rec, nil
}

func (r *SQLAdminUserRepo) ListUsers(ctx context.Context, f UserListFilter) ([]AdminUserRecord, int64, error) {
	where, args := adminUserWhere(f)
	var total int64
	if err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM users`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("httpapi: count users: %w", err)
	}

	queryArgs := append(append([]any(nil), args...), f.PageSize, (f.Page-1)*f.PageSize)
	query := `SELECT ` + adminUserColumns + `,
		(SELECT count(*) FROM api_tokens t WHERE t.user_id = users.id AND t.status = 'enabled')
		FROM users` + where + ` ORDER BY ` + adminUserOrder(f.Sort) +
		fmt.Sprintf(` LIMIT $%d OFFSET $%d`, len(args)+1, len(args)+2)
	rows, err := r.db.QueryContext(ctx, query, queryArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("httpapi: list users: %w", err)
	}
	defer rows.Close()

	out := make([]AdminUserRecord, 0, f.PageSize)
	for rows.Next() {
		rec, err := scanAdminUserRecord(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("httpapi: scan user: %w", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("httpapi: iterate users: %w", err)
	}
	return out, total, nil
}

func (r *SQLAdminUserRepo) GetUser(ctx context.Context, id int64) (AdminUserRecord, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+adminUserColumns+`,
		(SELECT count(*) FROM api_tokens t WHERE t.user_id = users.id AND t.status = 'enabled')
		FROM users WHERE id = $1`, id)
	rec, err := scanAdminUserRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return AdminUserRecord{}, ErrAdminUserNotFound
	}
	if err != nil {
		return AdminUserRecord{}, fmt.Errorf("httpapi: get user %d: %w", id, err)
	}
	return rec, nil
}

func (r *SQLAdminUserRepo) CreateUser(ctx context.Context, in AdminCreateUserInput) (AdminUserRecord, error) {
	var permissions any
	if in.AdminPermissions != nil {
		raw, err := json.Marshal(in.AdminPermissions)
		if err != nil {
			return AdminUserRecord{}, fmt.Errorf("httpapi: marshal admin permissions: %w", err)
		}
		permissions = string(raw)
	}
	row := r.db.QueryRowContext(ctx, `
		INSERT INTO users (username, display_name, role, "group", password_hash, remark, admin_permissions)
		VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb)
		RETURNING `+adminUserColumns+`, 0::bigint`,
		in.Username, in.DisplayName, in.Role, in.Group, in.PasswordHash, in.Remark, permissions)
	rec, err := scanAdminUserRecord(row)
	if isUniqueViolation(err) {
		return AdminUserRecord{}, ErrAdminDuplicate
	}
	if isForeignKeyViolation(err) {
		return AdminUserRecord{}, ErrAdminInvalidGroup
	}
	if err != nil {
		return AdminUserRecord{}, fmt.Errorf("httpapi: create admin user: %w", err)
	}
	return rec, nil
}

func (r *SQLAdminUserRepo) UpdateUser(ctx context.Context, id int64, patch AdminUserPatch, expectUpdatedAt time.Time) (AdminUserRecord, error) {
	sets := make([]string, 0, 5)
	args := make([]any, 0, 6)
	add := func(expression string, value any) {
		args = append(args, value)
		sets = append(sets, fmt.Sprintf(expression, len(args)))
	}
	if patch.DisplayNameSet {
		add("display_name = $%d", patch.DisplayName)
	}
	if patch.Group != nil {
		add(`"group" = $%d`, *patch.Group)
	}
	if patch.RemarkSet {
		add("remark = $%d", patch.Remark)
	}
	if patch.Role != nil {
		args = append(args, *patch.Role)
		position := len(args)
		sets = append(sets, fmt.Sprintf("role = $%d", position))
		// PostgreSQL 的 SET 表达式读取的是旧行：role 左值仍是变更前的值。
		// 提权时给只读起步权限，降权时清空——绝不让 NULL（=FULL）随提权继承。
		sets = append(sets, fmt.Sprintf(`admin_permissions = CASE
			WHEN $%d = 'admin' AND role <> 'admin' THEN '["users:read","logs:read"]'::jsonb
			WHEN $%d = 'user' THEN NULL
			ELSE admin_permissions END`, position, position))
	}
	sets = append(sets, "updated_at = now()")
	args = append(args, id)
	idPosition := len(args)
	args = append(args, expectUpdatedAt.UTC())
	tokenPosition := len(args)

	query := `UPDATE users SET ` + strings.Join(sets, ", ") +
		fmt.Sprintf(` WHERE id = $%d AND updated_at = $%d`, idPosition, tokenPosition) +
		` RETURNING ` + adminUserColumns + `,
		(SELECT count(*) FROM api_tokens t WHERE t.user_id = users.id AND t.status = 'enabled')`
	rec, err := scanAdminUserRecord(r.db.QueryRowContext(ctx, query, args...))
	if isForeignKeyViolation(err) {
		return AdminUserRecord{}, ErrAdminInvalidGroup
	}
	if errors.Is(err, sql.ErrNoRows) {
		// 无法区分「并发冲突」与「行已删除」：查一次存在性，给出准确语义。
		var exists bool
		if qerr := r.db.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM users WHERE id = $1)`, id).Scan(&exists); qerr != nil {
			return AdminUserRecord{}, fmt.Errorf("httpapi: check user existence: %w", qerr)
		}
		if !exists {
			return AdminUserRecord{}, ErrAdminUserNotFound
		}
		return AdminUserRecord{}, ErrAdminConflict
	}
	if err != nil {
		return AdminUserRecord{}, fmt.Errorf("httpapi: update user %d: %w", id, err)
	}
	return rec, nil
}

// SetStatus 在单事务内切换用户状态与令牌状态。disabled 时返回封禁前处于
// enabled 的令牌 ID（供解禁恢复）；enabled 时只恢复 restoreTokenIDs 中仍为
// disabled 的令牌。
func (r *SQLAdminUserRepo) SetStatus(ctx context.Context, id int64, status string, restoreTokenIDs []int64) (bool, []int64, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, nil, fmt.Errorf("httpapi: begin status change: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var current string
	err = tx.QueryRowContext(ctx, `SELECT status FROM users WHERE id = $1 FOR UPDATE`, id).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil, ErrAdminUserNotFound
	}
	if err != nil {
		return false, nil, fmt.Errorf("httpapi: lock user %d: %w", id, err)
	}
	if current == status {
		// 幂等：重复封禁/解禁不报错，也不改写恢复集。
		if err := tx.Commit(); err != nil {
			return false, nil, fmt.Errorf("httpapi: commit idempotent status: %w", err)
		}
		return false, nil, nil
	}

	if status == "disabled" {
		rows, err := tx.QueryContext(ctx,
			`SELECT id FROM api_tokens WHERE user_id = $1 AND status = 'enabled' ORDER BY id`, id)
		if err != nil {
			return false, nil, fmt.Errorf("httpapi: list enabled tokens: %w", err)
		}
		var enabledIDs []int64
		for rows.Next() {
			var tokenID int64
			if err := rows.Scan(&tokenID); err != nil {
				_ = rows.Close()
				return false, nil, fmt.Errorf("httpapi: scan enabled token: %w", err)
			}
			enabledIDs = append(enabledIDs, tokenID)
		}
		if err := rows.Close(); err != nil {
			return false, nil, fmt.Errorf("httpapi: close enabled token rows: %w", err)
		}
		if err := rows.Err(); err != nil {
			return false, nil, fmt.Errorf("httpapi: iterate enabled tokens: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE api_tokens SET status = 'disabled', updated_at = now()
			 WHERE user_id = $1 AND status = 'enabled'`, id); err != nil {
			return false, nil, fmt.Errorf("httpapi: disable tokens: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE users SET status = 'disabled', updated_at = now() WHERE id = $1`, id); err != nil {
			return false, nil, fmt.Errorf("httpapi: disable user: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return false, nil, fmt.Errorf("httpapi: commit disable: %w", err)
		}
		return true, enabledIDs, nil
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE users SET status = 'enabled', updated_at = now() WHERE id = $1`, id); err != nil {
		return false, nil, fmt.Errorf("httpapi: enable user: %w", err)
	}
	if len(restoreTokenIDs) > 0 {
		if _, err := tx.ExecContext(ctx,
			`UPDATE api_tokens SET status = 'enabled', updated_at = now()
			 WHERE user_id = $1 AND id = ANY($2) AND status = 'disabled'`,
			id, restoreTokenIDs); err != nil {
			return false, nil, fmt.Errorf("httpapi: restore tokens: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return false, nil, fmt.Errorf("httpapi: commit enable: %w", err)
	}
	return true, nil, nil
}

func (r *SQLAdminUserRepo) DeleteUser(ctx context.Context, id int64) (DeletedUser, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return DeletedUser{}, fmt.Errorf("httpapi: begin delete user: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var deleted DeletedUser
	var status string
	err = tx.QueryRowContext(ctx,
		`SELECT id, username, status FROM users WHERE id = $1 FOR UPDATE`, id).
		Scan(&deleted.ID, &deleted.Username, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return DeletedUser{}, ErrAdminUserNotFound
	}
	if err != nil {
		return DeletedUser{}, fmt.Errorf("httpapi: lock user for delete: %w", err)
	}
	if status != "disabled" {
		return DeletedUser{}, ErrAdminNotDisabled
	}
	rows, err := tx.QueryContext(ctx, `DELETE FROM api_tokens WHERE user_id = $1 RETURNING id`, id)
	if err != nil {
		return DeletedUser{}, fmt.Errorf("httpapi: delete user tokens: %w", err)
	}
	for rows.Next() {
		var tokenID int64
		if err := rows.Scan(&tokenID); err != nil {
			_ = rows.Close()
			return DeletedUser{}, fmt.Errorf("httpapi: scan deleted token id: %w", err)
		}
		deleted.TokenIDs = append(deleted.TokenIDs, tokenID)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return DeletedUser{}, fmt.Errorf("httpapi: iterate deleted token ids: %w", err)
	}
	_ = rows.Close()
	deleted.TokensDeleted = int64(len(deleted.TokenIDs))
	// 与自助删除（SQLAccountRepo.DeleteAccount）保持同一依赖清单：这些表都是
	// 逻辑引用 users.id（无 FK），不清理会留下孤儿行。
	for _, statement := range []string{
		`DELETE FROM login_sessions WHERE user_id = $1`,
		`DELETE FROM user_oauth_bindings WHERE user_id = $1`,
		`DELETE FROM user_passkeys WHERE user_id = $1`,
		`DELETE FROM checkins WHERE user_id = $1`,
	} {
		if _, err := tx.ExecContext(ctx, statement, id); err != nil {
			return DeletedUser{}, fmt.Errorf("httpapi: delete user dependents: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, id); err != nil {
		return DeletedUser{}, fmt.Errorf("httpapi: delete user row: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return DeletedUser{}, fmt.Errorf("httpapi: commit delete user: %w", err)
	}
	return deleted, nil
}

// AdjustQuota 实现 GC-5：余额只经原子 UPDATE，每笔变动恰写一行流水。
//
// 幂等：以 (user_id, idempotency_key) 为键先查既有 admin_adjustment 流水；
// 并发的同键请求由 pg_advisory_xact_lock 串行化（DB 没有对应的唯一索引，
// 不能靠唯一约束兜底）。
func (r *SQLAdminUserRepo) AdjustQuota(ctx context.Context, id int64, mode string, amount int64, idempotencyKey string) (QuotaAdjustment, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return QuotaAdjustment{}, fmt.Errorf("httpapi: begin quota adjustment: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	lockKey := fmt.Sprintf("admin_adjustment:%d:%s", id, idempotencyKey)
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, lockKey); err != nil {
		return QuotaAdjustment{}, fmt.Errorf("httpapi: lock quota adjustment: %w", err)
	}

	var replay QuotaAdjustment
	err = tx.QueryRowContext(ctx, `
		SELECT user_id, balance_after, id FROM wallet_transactions
		WHERE user_id = $1 AND type = 'admin_adjustment'
		  AND reference_type = 'admin_op' AND reference_id = $2
		ORDER BY id LIMIT 1`, id, idempotencyKey).
		Scan(&replay.UserID, &replay.NewBalance, &replay.TransactionID)
	if err == nil {
		if err := tx.Commit(); err != nil {
			return QuotaAdjustment{}, fmt.Errorf("httpapi: commit replayed adjustment: %w", err)
		}
		replay.Replayed = true
		return replay, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return QuotaAdjustment{}, fmt.Errorf("httpapi: read idempotent adjustment: %w", err)
	}

	var current int64
	err = tx.QueryRowContext(ctx, `SELECT balance FROM users WHERE id = $1 FOR UPDATE`, id).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return QuotaAdjustment{}, ErrAdminUserNotFound
	}
	if err != nil {
		return QuotaAdjustment{}, fmt.Errorf("httpapi: lock user balance: %w", err)
	}

	var newBalance int64
	switch mode {
	case "add":
		// int64 加法溢出会让 PostgreSQL 报 22003（500）；在进入 SQL 前给出
		// 可修复的 400，并避免依赖驱动错误文本。
		if amount > math.MaxInt64-current {
			return QuotaAdjustment{}, ErrAdminOverflow
		}
		err = tx.QueryRowContext(ctx, `
			UPDATE users SET balance = balance + $1, updated_at = now()
			WHERE id = $2 RETURNING balance`, amount, id).Scan(&newBalance)
	case "subtract":
		// 条件 UPDATE 是「永不为负」的唯一真源：并发扣减时第二个事务在
		// READ COMMITTED 下重新求值 WHERE，余额不足则影响 0 行。
		err = tx.QueryRowContext(ctx, `
			UPDATE users SET balance = balance - $1, updated_at = now()
			WHERE id = $2 AND balance >= $1 RETURNING balance`, amount, id).Scan(&newBalance)
		if errors.Is(err, sql.ErrNoRows) {
			return QuotaAdjustment{}, ErrAdminInsufficient
		}
	case "override":
		if current == amount {
			return QuotaAdjustment{}, ErrAdminNoChange
		}
		err = tx.QueryRowContext(ctx, `
			UPDATE users SET balance = $1, updated_at = now()
			WHERE id = $2 RETURNING balance`, amount, id).Scan(&newBalance)
	default:
		return QuotaAdjustment{}, fmt.Errorf("httpapi: unknown quota mode %q", mode)
	}
	if err != nil {
		return QuotaAdjustment{}, fmt.Errorf("httpapi: apply quota adjustment: %w", err)
	}

	// amount 是有符号的变动额；override 记录「新值 - 旧值」。
	delta := newBalance - current
	var transactionID int64
	err = tx.QueryRowContext(ctx, `
		INSERT INTO wallet_transactions
			(user_id, type, amount, balance_after, status, reference_type, reference_id, note)
		VALUES ($1, 'admin_adjustment', $2, $3, 'confirmed', 'admin_op', $4, $5)
		RETURNING id`,
		id, delta, newBalance, idempotencyKey, "admin adjustment: "+mode).Scan(&transactionID)
	if err != nil {
		return QuotaAdjustment{}, fmt.Errorf("httpapi: insert adjustment transaction: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return QuotaAdjustment{}, fmt.Errorf("httpapi: commit quota adjustment: %w", err)
	}
	return QuotaAdjustment{
		UserID:        id,
		BeforeBalance: current,
		NewBalance:    newBalance,
		TransactionID: transactionID,
	}, nil
}

func (r *SQLAdminUserRepo) ResetTwoFA(ctx context.Context, id int64) error {
	res, err := r.db.ExecContext(ctx, `
		UPDATE users SET totp_secret = NULL, backup_codes = NULL,
		                 totp_enabled = false, updated_at = now()
		WHERE id = $1 AND totp_enabled = true`, id)
	if err != nil {
		return fmt.Errorf("httpapi: reset 2fa: %w", err)
	}
	applied, err := rowsApplied(res)
	if err != nil {
		return err
	}
	if applied {
		return nil
	}
	var exists bool
	if err := r.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM users WHERE id = $1)`, id).Scan(&exists); err != nil {
		return fmt.Errorf("httpapi: check user existence: %w", err)
	}
	if !exists {
		return ErrAdminUserNotFound
	}
	return ErrAdminTwoFANotEnabled
}

func (r *SQLAdminUserRepo) ResetPassword(ctx context.Context, id int64, passwordHash string) error {
	res, err := r.db.ExecContext(ctx, `
		UPDATE users SET password_hash = $2, must_change_password = true, updated_at = now()
		WHERE id = $1`, id, passwordHash)
	if err != nil {
		return fmt.Errorf("httpapi: reset password: %w", err)
	}
	applied, err := rowsApplied(res)
	if err != nil {
		return err
	}
	if !applied {
		return ErrAdminUserNotFound
	}
	return nil
}

func (r *SQLAdminUserRepo) SetPermissions(ctx context.Context, id int64, perms []string) (AdminUserRecord, error) {
	raw, err := json.Marshal(perms)
	if err != nil {
		return AdminUserRecord{}, fmt.Errorf("httpapi: marshal permissions: %w", err)
	}
	row := r.db.QueryRowContext(ctx, `
		UPDATE users SET admin_permissions = $2::jsonb, updated_at = now()
		WHERE id = $1 AND role = 'admin'
		RETURNING `+adminUserColumns+`,
		(SELECT count(*) FROM api_tokens t WHERE t.user_id = users.id AND t.status = 'enabled')`,
		id, string(raw))
	rec, err := scanAdminUserRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		var exists bool
		if qerr := r.db.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM users WHERE id = $1)`, id).Scan(&exists); qerr != nil {
			return AdminUserRecord{}, fmt.Errorf("httpapi: check user existence: %w", qerr)
		}
		if !exists {
			return AdminUserRecord{}, ErrAdminUserNotFound
		}
		return AdminUserRecord{}, ErrAdminNotAdmin
	}
	if err != nil {
		return AdminUserRecord{}, fmt.Errorf("httpapi: set permissions: %w", err)
	}
	return rec, nil
}

// adminUserWhere 构造列表/计数的 WHERE 子句。keyword 中的 LIKE 通配符
// （% _ \）必须转义为字面量，否则用户可以拿 "%" 扫全表。
func adminUserWhere(f UserListFilter) (string, []any) {
	clauses := make([]string, 0, 4)
	args := make([]any, 0, 4)
	if f.Keyword != "" {
		args = append(args, "%"+escapeLikePattern(f.Keyword)+"%")
		n := len(args)
		clauses = append(clauses, fmt.Sprintf(
			`(username ILIKE $%d ESCAPE '\' OR COALESCE(display_name, '') ILIKE $%d ESCAPE '\' OR COALESCE(email, '') ILIKE $%d ESCAPE '\')`,
			n, n, n))
	}
	if f.Status != "" {
		args = append(args, f.Status)
		clauses = append(clauses, fmt.Sprintf("status = $%d", len(args)))
	}
	if f.Role != "" {
		args = append(args, f.Role)
		clauses = append(clauses, fmt.Sprintf("role = $%d", len(args)))
	}
	if f.Group != "" {
		args = append(args, f.Group)
		clauses = append(clauses, fmt.Sprintf(`"group" = $%d`, len(args)))
	}
	if len(clauses) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

// adminUserOrder 把白名单排序表达式翻译为 SQL。Sort 已由 handler 归一化；
// 这里仍做一次白名单过滤（纵深防御：仓储不应信任调用方）。
func adminUserOrder(sort string) string {
	desc := strings.HasPrefix(sort, "-")
	column := strings.TrimPrefix(sort, "-")
	switch column {
	case "id", "username", "balance", "created_at", "updated_at":
	default:
		column, desc = "created_at", true
	}
	direction := "ASC"
	if desc {
		direction = "DESC"
	}
	// 追加 id 作为稳定次序，避免同值行在翻页时漂移。
	return column + " " + direction + ", id " + direction
}

func escapeLikePattern(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, "%", `\%`)
	value = strings.ReplaceAll(value, "_", `\_`)
	return value
}

func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}
