// 管理面用户分组端点（Task 14 / modules-07 F-07-10）。
//
// 端点面（全部要求 `groups:manage`，brief 冻结）：
//
//	GET    /api/groups                  offset 分页；含 users/pricing/channels 引用计数
//	POST   /api/groups                  {name, description}
//	PATCH  /api/groups/{name}           {name?, description?}
//	DELETE /api/groups/{name}           系统组 400；仍被引用 409
//
// 路由挂载（并行任务负责 routes.go 的最终接线）：
//
//	h, err := NewGroupHandlers(GroupHandlerConfig{DB: cfg.DB, Authz: cfg.Authz, Logger: cfg.Logger})
//	h.RegisterGroupRoutes(mux, nil /* 已在 Protect 之后 */)
//
// 设计取舍：
//   - user_groups.name 是 users.group / api_tokens.group / group_pricing.group
//     的 FK 目标（ON DELETE RESTRICT），也是 channels.group_tags 的命名空间。
//     因此删除/改名都必须先检查全部引用，否则 DB 会以 23503 拒绝；这里把
//     RESTRICT 语义提前成可读的 409（brief：referenced group → 409）；
//   - default / auto 是系统保留组（种子迁移写入）：不可删除、不可改名。
//     描述仍可编辑；其它 is_system 组同样受保护。
package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"newtg/gateway/internal/rbac"
)

const (
	groupDescriptionMaxRunes = 128
	groupNameDefault         = "default"
	groupNameAuto            = "auto"
)

// groupNamePattern 与 user_groups_name_chk（init 迁移）逐字一致：
// 2–32 位小写字母/数字/下划线/连字符。上限在正则里，无需二次长度判断。
var groupNamePattern = regexp.MustCompile(`^[a-z0-9_-]{2,32}$`)

// 分组仓储错误哨兵。
var (
	ErrGroupNotFound   = errors.New("httpapi: group not found")
	ErrGroupConflict   = errors.New("httpapi: group name already exists")
	ErrGroupSystem     = errors.New("httpapi: system groups cannot be renamed or deleted")
	ErrGroupReferenced = errors.New("httpapi: group is still referenced")
)

// AdminGroupRecord 是 user_groups 行的投影，附带三类引用计数。
type AdminGroupRecord struct {
	ID                    int64
	Name                  string
	Description           *string
	Ratio                 int32
	UserSelectable        bool
	IsSystem              bool
	Status                int16
	UsersCount            int64
	PricingOverridesCount int64
	ChannelsCount         int64
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

// AdminGroupRepo 是分组管理端点的存储接缝。单测注入内存假实现；
// 生产实现是 SQLAdminGroupRepo。
type AdminGroupRepo interface {
	ListGroups(ctx context.Context, page, pageSize int) ([]AdminGroupRecord, int64, error)
	GetGroup(ctx context.Context, name string) (AdminGroupRecord, error)
	CreateGroup(ctx context.Context, name string, description *string) (AdminGroupRecord, error)
	UpdateGroup(ctx context.Context, currentName string, newName *string, description *string, descriptionSet bool) (AdminGroupRecord, error)
	DeleteGroup(ctx context.Context, name string) error
}

// GroupHandlerConfig 是分组端点的装配输入。Repo 与 DB 二选一。
//
// 说明：audit 包的冻结动作枚举没有 group.*，而未知动作会被审计层拒绝；
// 因此分组端点在 M1 不写审计（用户/权限变更仍写 user.*）。扩充枚举后再接线。
type GroupHandlerConfig struct {
	DB     *sql.DB
	Repo   AdminGroupRepo
	Authz  *rbac.AuthzMiddleware
	Logger *slog.Logger
}

// GroupHandlers 持有分组端点依赖，可并发使用。
type GroupHandlers struct {
	repo  AdminGroupRepo
	authz *rbac.AuthzMiddleware
	log   *slog.Logger
}

var (
	adminGroupListAction   = rbac.AuthzAction{ResourceKind: "group", ActionName: "list"}
	adminGroupCreateAction = rbac.AuthzAction{ResourceKind: "group", ActionName: "create"}
	adminGroupUpdateAction = rbac.AuthzAction{ResourceKind: "group", ActionName: "update"}
	adminGroupDeleteAction = rbac.AuthzAction{ResourceKind: "group", ActionName: "delete"}
)

// NewGroupHandlers 构造分组管理端点。
func NewGroupHandlers(cfg GroupHandlerConfig) (*GroupHandlers, error) {
	repo := cfg.Repo
	if repo == nil {
		if cfg.DB == nil {
			return nil, errors.New("httpapi: GroupHandlerConfig.Repo or DB is required")
		}
		repo = NewSQLAdminGroupRepo(cfg.DB)
	}
	authz := cfg.Authz
	if authz == nil {
		authz = rbac.NewAuthzMiddleware()
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	h := &GroupHandlers{repo: repo, authz: authz, log: log}
	h.registerPolicies()
	return h, nil
}

func (h *GroupHandlers) registerPolicies() {
	h.authz.SetWritePolicy(adminGroupCreateAction, &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{
		"name":        nil,
		"description": nil,
	}})
	h.authz.SetWritePolicy(adminGroupUpdateAction, &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{
		"name":        nil,
		"description": nil,
	}})
	h.authz.SetWritePolicy(adminGroupDeleteAction,
		&rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{}})
}

// RegisterGroupRoutes 注册分组端点。wrap 语义同 RegisterUserRoutes。
func (h *GroupHandlers) RegisterGroupRoutes(mux *http.ServeMux, wrap func(http.Handler) http.Handler) {
	mount := func(pattern string, handler http.Handler) {
		if wrap != nil {
			handler = wrap(handler)
		}
		mux.Handle(pattern, handler)
	}
	mount("GET /api/groups", h.authz.Handler(rbac.PermGroupsManage, adminGroupListAction)(
		http.HandlerFunc(h.handleListGroups)))
	mount("POST /api/groups", h.authz.Handler(rbac.PermGroupsManage, adminGroupCreateAction)(
		http.HandlerFunc(h.handleCreateGroup)))
	mount("PATCH /api/groups/{name}", h.authz.Handler(rbac.PermGroupsManage, adminGroupUpdateAction)(
		http.HandlerFunc(h.handleUpdateGroup)))
	mount("DELETE /api/groups/{name}", h.authz.Handler(rbac.PermGroupsManage, adminGroupDeleteAction)(
		http.HandlerFunc(h.handleDeleteGroup)))
}

// ---------------------------------------------------------------------------
// 视图
// ---------------------------------------------------------------------------

type adminGroupView struct {
	ID                    int64     `json:"id"`
	Name                  string    `json:"name"`
	Description           *string   `json:"description"`
	Ratio                 int32     `json:"ratio"`
	UserSelectable        bool      `json:"user_selectable"`
	IsSystem              bool      `json:"is_system"`
	Status                int16     `json:"status"`
	UsersCount            int64     `json:"users_count"`
	PricingOverridesCount int64     `json:"pricing_overrides_count"`
	ChannelsCount         int64     `json:"channels_count"`
	CreatedAt             time.Time `json:"created_at"`
	UpdatedAt             time.Time `json:"updated_at"`
}

func newAdminGroupView(rec AdminGroupRecord) adminGroupView {
	return adminGroupView{
		ID:                    rec.ID,
		Name:                  rec.Name,
		Description:           rec.Description,
		Ratio:                 rec.Ratio,
		UserSelectable:        rec.UserSelectable,
		IsSystem:              rec.IsSystem,
		Status:                rec.Status,
		UsersCount:            rec.UsersCount,
		PricingOverridesCount: rec.PricingOverridesCount,
		ChannelsCount:         rec.ChannelsCount,
		CreatedAt:             rec.CreatedAt,
		UpdatedAt:             rec.UpdatedAt,
	}
}

type adminGroupListView struct {
	Items    []adminGroupView `json:"items"`
	Total    int64            `json:"total"`
	Page     int              `json:"page"`
	PageSize int              `json:"page_size"`
}

// ---------------------------------------------------------------------------
// handlers
// ---------------------------------------------------------------------------

func (h *GroupHandlers) handleListGroups(w http.ResponseWriter, r *http.Request) {
	page, pageSize := 1, adminPageSizeDefault
	if raw := strings.TrimSpace(r.URL.Query().Get("p")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 {
			writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request", "p must be a positive integer")
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
	items, total, err := h.repo.ListGroups(r.Context(), page, pageSize)
	if err != nil {
		h.writeGroupRepoError(w, "list groups", err)
		return
	}
	views := make([]adminGroupView, 0, len(items))
	for _, item := range items {
		views = append(views, newAdminGroupView(item))
	}
	writeJSON(w, http.StatusOK, adminGroupListView{Items: views, Total: total, Page: page, PageSize: pageSize})
}

type adminCreateGroupRequest struct {
	Name        string  `json:"name"`
	Description *string `json:"description"`
}

func (h *GroupHandlers) handleCreateGroup(w http.ResponseWriter, r *http.Request) {
	var req adminCreateGroupRequest
	if err := readAuthJSON(r, &req); err != nil {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request", "request body must be a JSON object")
		return
	}
	name := strings.TrimSpace(req.Name)
	if !groupNamePattern.MatchString(name) {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"name must match ^[a-z0-9_-]+$ and be 2-32 characters")
		return
	}
	description, err := normalizeOptionalText(req.Description, groupDescriptionMaxRunes)
	if err != nil {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"description must be at most 128 characters")
		return
	}
	rec, err := h.repo.CreateGroup(r.Context(), name, description)
	if err != nil {
		h.writeGroupRepoError(w, "create group", err)
		return
	}
	writeJSON(w, http.StatusCreated, newAdminGroupView(rec))
}

func (h *GroupHandlers) handleUpdateGroup(w http.ResponseWriter, r *http.Request) {
	currentName := strings.TrimSpace(r.PathValue("name"))
	if currentName == "" {
		writeAdminProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found", "")
		return
	}
	body, err := readRawObject(r)
	if err != nil {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request", "request body must be a JSON object")
		return
	}

	var newName *string
	if raw, present := body["name"]; present {
		var value string
		if string(raw) == "null" || json.Unmarshal(raw, &value) != nil {
			writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request", "name must be a string")
			return
		}
		value = strings.TrimSpace(value)
		if !groupNamePattern.MatchString(value) {
			writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
				"name must match ^[a-z0-9_-]+$ and be 2-32 characters")
			return
		}
		if value != currentName {
			newName = &value
		}
	}
	var description *string
	descriptionSet := false
	if raw, present := body["description"]; present {
		value, valid := decodeNullableText(raw)
		if !valid {
			writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
				"description must be a string or null")
			return
		}
		normalized, err := normalizeOptionalText(value, groupDescriptionMaxRunes)
		if err != nil {
			writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
				"description must be at most 128 characters")
			return
		}
		description = normalized
		descriptionSet = true
	}
	if newName == nil && !descriptionSet {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request", "no fields to update")
		return
	}

	rec, err := h.repo.UpdateGroup(r.Context(), currentName, newName, description, descriptionSet)
	if err != nil {
		h.writeGroupRepoError(w, "update group", err)
		return
	}
	writeJSON(w, http.StatusOK, newAdminGroupView(rec))
}

func (h *GroupHandlers) handleDeleteGroup(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		writeAdminProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found", "")
		return
	}
	if err := h.repo.DeleteGroup(r.Context(), name); err != nil {
		h.writeGroupRepoError(w, "delete group", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *GroupHandlers) writeGroupRepoError(w http.ResponseWriter, op string, err error) {
	switch {
	case errors.Is(err, ErrGroupNotFound):
		writeAdminProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found", "")
	case errors.Is(err, ErrGroupConflict):
		writeAdminProblem(w, http.StatusConflict, authProblemConflict, "Conflict", "the group name is already taken")
	case errors.Is(err, ErrGroupSystem):
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"system groups cannot be renamed or deleted")
	case errors.Is(err, ErrGroupReferenced):
		writeAdminProblem(w, http.StatusConflict, authProblemConflict, "Conflict",
			"the group is still referenced by users, tokens, pricing entries or channels")
	default:
		h.log.Error(op+" failed", "error", err)
		writeAdminProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error", "")
	}
}

// ---------------------------------------------------------------------------
// SQL 实现
// ---------------------------------------------------------------------------

// adminGroupColumns 是 AdminGroupRecord 的基础列序（带表别名 g）。
// 所有查询都使用别名 g（列表/详情显式前缀，避免与子查询里的同名列歧义）。
const adminGroupColumns = `g.id, g.name, g.description, g.ratio, g.user_selectable,
	g.is_system, g.status, g.created_at, g.updated_at`

// adminGroupCountColumns 是列表/详情追加的三类引用计数。channels 的
// group_tags 是 jsonb 数组，用包含运算符判断是否仍然引用该组。
const adminGroupCountColumns = `
	(SELECT count(*) FROM users u WHERE u."group" = g.name),
	(SELECT count(*) FROM group_pricing p WHERE p."group" = g.name),
	(SELECT count(*) FROM channels c WHERE c.group_tags @> jsonb_build_array(g.name))`

// SQLAdminGroupRepo 是 AdminGroupRepo 的 PostgreSQL 实现。
type SQLAdminGroupRepo struct{ db *sql.DB }

// NewSQLAdminGroupRepo 构造分组仓储。
func NewSQLAdminGroupRepo(db *sql.DB) *SQLAdminGroupRepo { return &SQLAdminGroupRepo{db: db} }

func scanAdminGroupRecord(row rowScanner) (AdminGroupRecord, error) {
	var rec AdminGroupRecord
	err := row.Scan(&rec.ID, &rec.Name, &rec.Description, &rec.Ratio, &rec.UserSelectable,
		&rec.IsSystem, &rec.Status, &rec.CreatedAt, &rec.UpdatedAt,
		&rec.UsersCount, &rec.PricingOverridesCount, &rec.ChannelsCount)
	if err != nil {
		return AdminGroupRecord{}, err
	}
	return rec, nil
}

func (r *SQLAdminGroupRepo) ListGroups(ctx context.Context, page, pageSize int) ([]AdminGroupRecord, int64, error) {
	var total int64
	if err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM user_groups`).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("httpapi: count groups: %w", err)
	}
	rows, err := r.db.QueryContext(ctx, `SELECT `+adminGroupColumns+
		`,`+adminGroupCountColumns+`
		FROM user_groups g
		ORDER BY g.is_system DESC, g.name ASC
		LIMIT $1 OFFSET $2`, pageSize, (page-1)*pageSize)
	if err != nil {
		return nil, 0, fmt.Errorf("httpapi: list groups: %w", err)
	}
	defer rows.Close()

	out := make([]AdminGroupRecord, 0, pageSize)
	for rows.Next() {
		rec, err := scanAdminGroupRecord(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("httpapi: scan group: %w", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("httpapi: iterate groups: %w", err)
	}
	return out, total, nil
}

func (r *SQLAdminGroupRepo) GetGroup(ctx context.Context, name string) (AdminGroupRecord, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+adminGroupColumns+
		`,`+adminGroupCountColumns+` FROM user_groups g WHERE g.name = $1`, name)
	rec, err := scanAdminGroupRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return AdminGroupRecord{}, ErrGroupNotFound
	}
	if err != nil {
		return AdminGroupRecord{}, fmt.Errorf("httpapi: get group %q: %w", name, err)
	}
	return rec, nil
}

func (r *SQLAdminGroupRepo) CreateGroup(ctx context.Context, name string, description *string) (AdminGroupRecord, error) {
	row := r.db.QueryRowContext(ctx, `
		INSERT INTO user_groups AS g (name, description) VALUES ($1, $2)
		RETURNING `+adminGroupColumns+`, 0::bigint, 0::bigint, 0::bigint`,
		name, description)
	rec, err := scanAdminGroupRecord(row)
	if isUniqueViolation(err) {
		return AdminGroupRecord{}, ErrGroupConflict
	}
	if err != nil {
		return AdminGroupRecord{}, fmt.Errorf("httpapi: create group: %w", err)
	}
	return rec, nil
}

func (r *SQLAdminGroupRepo) UpdateGroup(ctx context.Context, currentName string, newName *string, description *string, descriptionSet bool) (AdminGroupRecord, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return AdminGroupRecord{}, fmt.Errorf("httpapi: begin group update: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var (
		groupID  int64
		name     string
		isSystem bool
	)
	err = tx.QueryRowContext(ctx,
		`SELECT id, name, is_system FROM user_groups WHERE name = $1 FOR UPDATE`, currentName).
		Scan(&groupID, &name, &isSystem)
	if errors.Is(err, sql.ErrNoRows) {
		return AdminGroupRecord{}, ErrGroupNotFound
	}
	if err != nil {
		return AdminGroupRecord{}, fmt.Errorf("httpapi: lock group: %w", err)
	}
	if newName != nil && *newName != name {
		if isSystem || name == groupNameDefault || name == groupNameAuto {
			return AdminGroupRecord{}, ErrGroupSystem
		}
		// name 是多个表的 FK 目标且没有 ON UPDATE CASCADE；改名被引用组
		// 必须由调用方先迁移引用，管理端点直接 409（brief 冻结）。
		referenced, err := groupReferenceCount(ctx, tx, name)
		if err != nil {
			return AdminGroupRecord{}, err
		}
		if referenced > 0 {
			return AdminGroupRecord{}, ErrGroupReferenced
		}
	}

	sets := make([]string, 0, 3)
	args := make([]any, 0, 3)
	if newName != nil {
		args = append(args, *newName)
		sets = append(sets, fmt.Sprintf("name = $%d", len(args)))
	}
	if descriptionSet {
		args = append(args, description)
		sets = append(sets, fmt.Sprintf("description = $%d", len(args)))
	}
	sets = append(sets, "updated_at = now()")
	args = append(args, groupID)
	idPosition := len(args)

	query := `UPDATE user_groups g SET ` + strings.Join(sets, ", ") +
		fmt.Sprintf(" WHERE g.id = $%d", idPosition) +
		` RETURNING ` + adminGroupColumns + `,` + adminGroupCountColumns
	rec, err := scanAdminGroupRecord(tx.QueryRowContext(ctx, query, args...))
	if isUniqueViolation(err) {
		return AdminGroupRecord{}, ErrGroupConflict
	}
	if err != nil {
		return AdminGroupRecord{}, fmt.Errorf("httpapi: update group: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return AdminGroupRecord{}, fmt.Errorf("httpapi: commit group update: %w", err)
	}
	return rec, nil
}

func (r *SQLAdminGroupRepo) DeleteGroup(ctx context.Context, name string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("httpapi: begin group delete: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var (
		groupID  int64
		isSystem bool
	)
	err = tx.QueryRowContext(ctx,
		`SELECT id, is_system FROM user_groups WHERE name = $1 FOR UPDATE`, name).
		Scan(&groupID, &isSystem)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrGroupNotFound
	}
	if err != nil {
		return fmt.Errorf("httpapi: lock group for delete: %w", err)
	}
	if isSystem || name == groupNameDefault || name == groupNameAuto {
		return ErrGroupSystem
	}
	referenced, err := groupReferenceCount(ctx, tx, name)
	if err != nil {
		return err
	}
	if referenced > 0 {
		return ErrGroupReferenced
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM user_groups WHERE id = $1`, groupID); err != nil {
		return fmt.Errorf("httpapi: delete group: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("httpapi: commit group delete: %w", err)
	}
	return nil
}

// groupReferenceCount 汇总四类引用。users/api_tokens/group_pricing 是真实
// FK（ON DELETE RESTRICT）；channels.group_tags 是 jsonb 数组，无 FK，
// 必须显式检查，否则删除后渠道会引用一个不存在的组名。
func groupReferenceCount(ctx context.Context, tx *sql.Tx, name string) (int64, error) {
	var count int64
	err := tx.QueryRowContext(ctx, `
		SELECT (SELECT count(*) FROM users WHERE "group" = $1)
		     + (SELECT count(*) FROM api_tokens WHERE "group" = $1)
		     + (SELECT count(*) FROM group_pricing WHERE "group" = $1)
		     + (SELECT count(*) FROM channels WHERE group_tags @> jsonb_build_array($1::text))`,
		name).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("httpapi: count group references: %w", err)
	}
	return count, nil
}
