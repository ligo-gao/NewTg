package httpapi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/rbac"
)

// 本文件是 Task 14 的单元验收：用户管理 / 分组 / 权限矩阵的全部端点。
// 依赖全部是内存假实现（miniredis + fakeAdminRepo + 捕获式 audit Execer），
// 未打 integration 标签时不得触碰外部 PG/Redis（GC-8）。
//
// 验收要点（brief (a)..(h)）：
//   - (a) TC-F-07-01-05：user 角色访问管理端点 → 403；
//   - (b) TC-F-07-02-01：同一 Idempotency-Key 只创建一条；
//   - (c) TC-F-07-03-01：PATCH 含 password → 400；
//   - (d) TC-F-07-04/05：封禁吊销会话/令牌，解禁恢复原 enabled 令牌；
//   - (e) TC-F-07-06-01：对自己降权 → 400；
//   - (f) TC-F-07-11-01：并发扣减不产生负余额；
//   - (g) TC-F-07-12-05：审计不含任何口令材料；
//   - (h) TC-B-04：授予超出自身权限 → 403 且不变更。

const adminTestActorID int64 = 1

// ---------------------------------------------------------------------------
// 假仓储
// ---------------------------------------------------------------------------

type fakeAdminToken struct {
	ID     int64
	UserID int64
	Status string
}

type fakeWalletTx struct {
	ID           int64
	UserID       int64
	Amount       int64
	BalanceAfter int64
	ReferenceID  string
}

type fakeAdminRepo struct {
	mu         sync.Mutex
	users      map[int64]*AdminUserRecord
	tokens     []*fakeAdminToken
	nextUserID int64
	nextToken  int64
	nextTxID   int64
	txs        []fakeWalletTx
	quotaKeys  map[string]QuotaAdjustment

	groups   map[string]*AdminGroupRecord
	groupID  int64
	groupRef map[string]int64

	now          time.Time
	createCalls  int
	permWrites   int
	lastFilter   UserListFilter
	forcedErr    error
	passwordHash string // 最近一次 ResetPassword 写入的哈希（测试断言用）
}

func newFakeAdminRepo() *fakeAdminRepo {
	repo := &fakeAdminRepo{
		users:      map[int64]*AdminUserRecord{},
		quotaKeys:  map[string]QuotaAdjustment{},
		groups:     map[string]*AdminGroupRecord{},
		groupRef:   map[string]int64{},
		now:        time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC),
		nextUserID: 100,
	}
	repo.addGroup("default", true)
	repo.addGroup("auto", true)
	return repo
}

func (r *fakeAdminRepo) tick() time.Time {
	r.now = r.now.Add(time.Second)
	return r.now
}

func (r *fakeAdminRepo) addGroup(name string, system bool) AdminGroupRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.groupID++
	rec := &AdminGroupRecord{
		ID: r.groupID, Name: name, Ratio: 100, UserSelectable: true,
		IsSystem: system, Status: 1, CreatedAt: r.tick(), UpdatedAt: r.now,
	}
	if system {
		desc := "system group"
		rec.Description = &desc
	}
	r.groups[name] = rec
	return cloneGroup(rec)
}

func (r *fakeAdminRepo) addUser(rec AdminUserRecord) *AdminUserRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextUserID++
	rec.ID = r.nextUserID
	if rec.Role == "" {
		rec.Role = rbac.RoleUser
	}
	if rec.Status == "" {
		rec.Status = "enabled"
	}
	if rec.Group == "" {
		rec.Group = "default"
	}
	// 与真实列一致：NOT NULL DEFAULT 'general'。
	if rec.CustomerType == "" {
		rec.CustomerType = customerTypeGeneral
	}
	rec.CreatedAt = r.tick()
	rec.UpdatedAt = r.now
	// 存入与返回各自的副本：调用方拿到的快照不能随后续写操作一起变化
	// （否则「陈旧 updated_at」测试会永远读到最新版本）。
	stored := cloneUser(rec)
	r.users[rec.ID] = &stored
	snapshot := cloneUser(rec)
	return &snapshot
}

func (r *fakeAdminRepo) addToken(userID int64, status string) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextToken++
	token := &fakeAdminToken{ID: r.nextToken, UserID: userID, Status: status}
	r.tokens = append(r.tokens, token)
	return token.ID
}

func (r *fakeAdminRepo) tokenStatus(id int64) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, token := range r.tokens {
		if token.ID == id {
			return token.Status
		}
	}
	return ""
}

func (r *fakeAdminRepo) userStatus(id int64) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if rec, ok := r.users[id]; ok {
		return rec.Status
	}
	return ""
}

func (r *fakeAdminRepo) userRole(id int64) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if rec, ok := r.users[id]; ok {
		return rec.Role
	}
	return ""
}

func cloneUser(rec AdminUserRecord) AdminUserRecord {
	out := rec
	if rec.DisplayName != nil {
		value := *rec.DisplayName
		out.DisplayName = &value
	}
	if rec.Email != nil {
		value := *rec.Email
		out.Email = &value
	}
	if rec.Remark != nil {
		value := *rec.Remark
		out.Remark = &value
	}
	out.AdminPermissions = append(json.RawMessage(nil), rec.AdminPermissions...)
	return out
}

func cloneGroup(rec *AdminGroupRecord) AdminGroupRecord {
	out := *rec
	if rec.Description != nil {
		value := *rec.Description
		out.Description = &value
	}
	return out
}

func (r *fakeAdminRepo) ListUsers(_ context.Context, filter UserListFilter) ([]AdminUserRecord, int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.forcedErr != nil {
		return nil, 0, r.forcedErr
	}
	r.lastFilter = filter
	all := make([]AdminUserRecord, 0, len(r.users))
	for _, rec := range r.users {
		if filter.Status != "" && rec.Status != filter.Status {
			continue
		}
		if filter.Role != "" && rec.Role != filter.Role {
			continue
		}
		if filter.Group != "" && rec.Group != filter.Group {
			continue
		}
		if filter.CustomerType != "" && rec.CustomerType != filter.CustomerType {
			continue
		}
		if filter.Keyword != "" && !strings.Contains(strings.ToLower(rec.Username), strings.ToLower(filter.Keyword)) {
			continue
		}
		all = append(all, cloneUser(*rec))
	}
	total := int64(len(all))
	start := (filter.Page - 1) * filter.PageSize
	if start > len(all) {
		start = len(all)
	}
	end := start + filter.PageSize
	if end > len(all) {
		end = len(all)
	}
	return all[start:end], total, nil
}

func (r *fakeAdminRepo) CreateUser(_ context.Context, in AdminCreateUserInput) (AdminUserRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.forcedErr != nil {
		return AdminUserRecord{}, r.forcedErr
	}
	r.createCalls++
	if _, ok := r.groups[in.Group]; !ok {
		return AdminUserRecord{}, ErrAdminInvalidGroup
	}
	for _, existing := range r.users {
		if existing.Username == in.Username {
			return AdminUserRecord{}, ErrAdminDuplicate
		}
	}
	r.nextUserID++
	rec := &AdminUserRecord{
		ID:               r.nextUserID,
		Username:         in.Username,
		DisplayName:      in.DisplayName,
		Role:             in.Role,
		Status:           "enabled",
		Balance:          0,
		Group:            in.Group,
		CustomerType:     in.CustomerType,
		Remark:           in.Remark,
		TOTPEnabled:      false,
		CreatedAt:        r.tick(),
		UpdatedAt:        r.now,
		TokenCount:       0,
		AdminPermissions: nil,
	}
	if in.AdminPermissions != nil {
		raw, _ := json.Marshal(in.AdminPermissions)
		rec.AdminPermissions = raw
	}
	copied := cloneUser(*rec)
	r.users[rec.ID] = &copied
	return copied, nil
}

func (r *fakeAdminRepo) GetUser(_ context.Context, id int64) (AdminUserRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.forcedErr != nil {
		return AdminUserRecord{}, r.forcedErr
	}
	rec, ok := r.users[id]
	if !ok {
		return AdminUserRecord{}, ErrAdminUserNotFound
	}
	return cloneUser(*rec), nil
}

func (r *fakeAdminRepo) UpdateUser(_ context.Context, id int64, patch AdminUserPatch, expectUpdatedAt time.Time) (AdminUserRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.forcedErr != nil {
		return AdminUserRecord{}, r.forcedErr
	}
	rec, ok := r.users[id]
	if !ok {
		return AdminUserRecord{}, ErrAdminUserNotFound
	}
	if !rec.UpdatedAt.Equal(expectUpdatedAt) {
		return AdminUserRecord{}, ErrAdminConflict
	}
	if patch.DisplayNameSet {
		rec.DisplayName = patch.DisplayName
	}
	if patch.Group != nil {
		if _, ok := r.groups[*patch.Group]; !ok {
			return AdminUserRecord{}, ErrAdminInvalidGroup
		}
		rec.Group = *patch.Group
	}
	if patch.RemarkSet {
		rec.Remark = patch.Remark
	}
	if patch.CustomerType != nil {
		rec.CustomerType = *patch.CustomerType
	}
	if patch.Role != nil && *patch.Role != rec.Role {
		oldRole := rec.Role
		rec.Role = *patch.Role
		switch {
		case rec.Role == rbac.RoleAdmin && oldRole != rbac.RoleAdmin:
			raw, _ := json.Marshal([]string{rbac.PermUsersRead, rbac.PermLogsRead})
			rec.AdminPermissions = raw
		case rec.Role == rbac.RoleUser:
			rec.AdminPermissions = nil
		}
	}
	rec.UpdatedAt = r.tick()
	return cloneUser(*rec), nil
}

func (r *fakeAdminRepo) SetStatus(_ context.Context, id int64, status string, restoreTokenIDs []int64) (bool, []int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.forcedErr != nil {
		return false, nil, r.forcedErr
	}
	rec, ok := r.users[id]
	if !ok {
		return false, nil, ErrAdminUserNotFound
	}
	if rec.Status == status {
		return false, nil, nil
	}
	rec.Status = status
	rec.UpdatedAt = r.tick()
	if status == "disabled" {
		var enabled []int64
		for _, token := range r.tokens {
			if token.UserID == id && token.Status == "enabled" {
				enabled = append(enabled, token.ID)
				token.Status = "disabled"
			}
		}
		return true, enabled, nil
	}
	for _, restore := range restoreTokenIDs {
		for _, token := range r.tokens {
			if token.ID == restore && token.UserID == id && token.Status == "disabled" {
				token.Status = "enabled"
			}
		}
	}
	return true, nil, nil
}

func (r *fakeAdminRepo) DeleteUser(_ context.Context, id int64) (DeletedUser, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.forcedErr != nil {
		return DeletedUser{}, r.forcedErr
	}
	rec, ok := r.users[id]
	if !ok {
		return DeletedUser{}, ErrAdminUserNotFound
	}
	if rec.Status != "disabled" {
		return DeletedUser{}, ErrAdminNotDisabled
	}
	deleted := DeletedUser{ID: rec.ID, Username: rec.Username}
	kept := r.tokens[:0]
	for _, token := range r.tokens {
		if token.UserID == id {
			deleted.TokensDeleted++
			deleted.TokenIDs = append(deleted.TokenIDs, token.ID)
			continue
		}
		kept = append(kept, token)
	}
	r.tokens = kept
	delete(r.users, id)
	return deleted, nil
}

func (r *fakeAdminRepo) AdjustQuota(_ context.Context, id int64, mode string, amount int64, idempotencyKey string) (QuotaAdjustment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.forcedErr != nil {
		return QuotaAdjustment{}, r.forcedErr
	}
	key := fmt.Sprintf("%d:%s", id, idempotencyKey)
	if replayed, ok := r.quotaKeys[key]; ok {
		replayed.Replayed = true
		return replayed, nil
	}
	rec, ok := r.users[id]
	if !ok {
		return QuotaAdjustment{}, ErrAdminUserNotFound
	}
	newBalance := rec.Balance
	switch mode {
	case "add":
		if amount > math.MaxInt64-rec.Balance {
			return QuotaAdjustment{}, ErrAdminOverflow
		}
		newBalance += amount
	case "subtract":
		if rec.Balance < amount {
			return QuotaAdjustment{}, ErrAdminInsufficient
		}
		newBalance -= amount
	case "override":
		if rec.Balance == amount {
			return QuotaAdjustment{}, ErrAdminNoChange
		}
		newBalance = amount
	default:
		return QuotaAdjustment{}, fmt.Errorf("unknown mode %q", mode)
	}
	delta := newBalance - rec.Balance
	rec.Balance = newBalance
	rec.UpdatedAt = r.tick()
	r.nextTxID++
	r.txs = append(r.txs, fakeWalletTx{
		ID: r.nextTxID, UserID: id, Amount: delta,
		BalanceAfter: newBalance, ReferenceID: idempotencyKey,
	})
	result := QuotaAdjustment{
		UserID: id, BeforeBalance: rec.Balance - delta, NewBalance: newBalance,
		TransactionID: r.nextTxID,
	}
	r.quotaKeys[key] = result
	return result, nil
}

func (r *fakeAdminRepo) ResetTwoFA(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.forcedErr != nil {
		return r.forcedErr
	}
	rec, ok := r.users[id]
	if !ok {
		return ErrAdminUserNotFound
	}
	if !rec.TOTPEnabled {
		return ErrAdminTwoFANotEnabled
	}
	rec.TOTPEnabled = false
	rec.UpdatedAt = r.tick()
	return nil
}

func (r *fakeAdminRepo) ResetPassword(_ context.Context, id int64, passwordHash string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.forcedErr != nil {
		return r.forcedErr
	}
	rec, ok := r.users[id]
	if !ok {
		return ErrAdminUserNotFound
	}
	rec.MustChangePassword = true
	rec.UpdatedAt = r.tick()
	r.passwordHash = passwordHash
	return nil
}

func (r *fakeAdminRepo) SetPermissions(_ context.Context, id int64, perms []string) (AdminUserRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.forcedErr != nil {
		return AdminUserRecord{}, r.forcedErr
	}
	rec, ok := r.users[id]
	if !ok {
		return AdminUserRecord{}, ErrAdminUserNotFound
	}
	if rec.Role != rbac.RoleAdmin {
		return AdminUserRecord{}, ErrAdminNotAdmin
	}
	raw, _ := json.Marshal(perms)
	rec.AdminPermissions = raw
	rec.UpdatedAt = r.tick()
	r.permWrites++
	return cloneUser(*rec), nil
}

// ---- 分组 ----

func (r *fakeAdminRepo) ListGroups(_ context.Context, page, pageSize int) ([]AdminGroupRecord, int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.forcedErr != nil {
		return nil, 0, r.forcedErr
	}
	all := make([]AdminGroupRecord, 0, len(r.groups))
	for _, rec := range r.groups {
		out := cloneGroup(rec)
		out.UsersCount = r.groupRef[rec.Name]
		all = append(all, out)
	}
	total := int64(len(all))
	start := (page - 1) * pageSize
	if start > len(all) {
		start = len(all)
	}
	end := start + pageSize
	if end > len(all) {
		end = len(all)
	}
	return all[start:end], total, nil
}

func (r *fakeAdminRepo) GetGroup(_ context.Context, name string) (AdminGroupRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.groups[name]
	if !ok {
		return AdminGroupRecord{}, ErrGroupNotFound
	}
	return cloneGroup(rec), nil
}

func (r *fakeAdminRepo) CreateGroup(_ context.Context, name string, description *string) (AdminGroupRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.forcedErr != nil {
		return AdminGroupRecord{}, r.forcedErr
	}
	if _, ok := r.groups[name]; ok {
		return AdminGroupRecord{}, ErrGroupConflict
	}
	r.groupID++
	rec := &AdminGroupRecord{
		ID: r.groupID, Name: name, Description: description, Ratio: 100,
		UserSelectable: true, Status: 1, CreatedAt: r.tick(), UpdatedAt: r.now,
	}
	r.groups[name] = rec
	return cloneGroup(rec), nil
}

func (r *fakeAdminRepo) UpdateGroup(_ context.Context, currentName string, newName *string, description *string, descriptionSet bool) (AdminGroupRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.forcedErr != nil {
		return AdminGroupRecord{}, r.forcedErr
	}
	rec, ok := r.groups[currentName]
	if !ok {
		return AdminGroupRecord{}, ErrGroupNotFound
	}
	if newName != nil && *newName != currentName {
		if rec.IsSystem || currentName == groupNameDefault || currentName == groupNameAuto {
			return AdminGroupRecord{}, ErrGroupSystem
		}
		if r.groupRef[currentName] > 0 {
			return AdminGroupRecord{}, &GroupReferencedError{Count: r.groupRef[currentName]}
		}
		if _, exists := r.groups[*newName]; exists {
			return AdminGroupRecord{}, ErrGroupConflict
		}
		delete(r.groups, currentName)
		rec.Name = *newName
		r.groups[*newName] = rec
	}
	if descriptionSet {
		rec.Description = description
	}
	rec.UpdatedAt = r.tick()
	return cloneGroup(rec), nil
}

func (r *fakeAdminRepo) DeleteGroup(_ context.Context, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.forcedErr != nil {
		return r.forcedErr
	}
	rec, ok := r.groups[name]
	if !ok {
		return ErrGroupNotFound
	}
	if rec.IsSystem || name == groupNameDefault || name == groupNameAuto {
		return ErrGroupSystem
	}
	if r.groupRef[name] > 0 {
		return &GroupReferencedError{Count: r.groupRef[name]}
	}
	delete(r.groups, name)
	return nil
}

// ---------------------------------------------------------------------------
// 捕获式审计 Execer
// ---------------------------------------------------------------------------

type adminAuditCall struct {
	query string
	args  []any
}

type adminAuditCapture struct {
	mu    sync.Mutex
	calls []adminAuditCall
}

func (c *adminAuditCapture) ExecContext(_ context.Context, query string, args ...any) (sql.Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, adminAuditCall{query: query, args: append([]any(nil), args...)})
	return adminFakeResult(1), nil
}

func (c *adminAuditCapture) allCalls() []adminAuditCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]adminAuditCall, len(c.calls))
	copy(out, c.calls)
	return out
}

// concatenatedArgs 把一次 INSERT 的全部实参摊平成字符串，便于断言
// 「审计行不含某段材料」。
func (c *adminAuditCapture) concatenatedArgs() string {
	var builder strings.Builder
	for _, call := range c.allCalls() {
		for _, arg := range call.args {
			switch value := arg.(type) {
			case nil:
			case string:
				builder.WriteString(value)
			case []byte:
				builder.Write(value)
			default:
				fmt.Fprintf(&builder, "%v", value)
			}
			builder.WriteString("\n")
		}
	}
	return builder.String()
}

type adminFakeResult int64

func (r adminFakeResult) LastInsertId() (int64, error) { return 0, nil }
func (r adminFakeResult) RowsAffected() (int64, error) { return int64(r), nil }

// ---------------------------------------------------------------------------
// 测试环境
// ---------------------------------------------------------------------------

type adminTestEnv struct {
	t        *testing.T
	mr       *miniredis.Miniredis
	rdb      *redis.Client
	sessions *auth.SessionManager
	repo     *fakeAdminRepo
	audit    *adminAuditCapture
	authz    *rbac.AuthzMiddleware
	users    *UserHandlers
	groups   *GroupHandlers
	router   http.Handler
}

func newAdminTestEnv(t *testing.T) *adminTestEnv {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })

	sessions := auth.NewSessionManager(rdb, []byte(testSessionSecret))
	repo := newFakeAdminRepo()
	capture := &adminAuditCapture{}
	authz := rbac.NewAuthzMiddleware()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	users, err := NewUserHandlers(UserHandlerConfig{
		Repo: repo, Sessions: sessions, Redis: rdb, Authz: authz,
		Audit: audit.New(capture), Logger: logger,
	})
	if err != nil {
		t.Fatalf("NewUserHandlers: %v", err)
	}
	groups, err := NewGroupHandlers(GroupHandlerConfig{
		Repo: repo, Authz: authz, Audit: audit.New(capture), Logger: logger,
	})
	if err != nil {
		t.Fatalf("NewGroupHandlers: %v", err)
	}

	mux := http.NewServeMux()
	users.RegisterUserRoutes(mux, nil)
	groups.RegisterGroupRoutes(mux, nil)

	return &adminTestEnv{
		t: t, mr: mr, rdb: rdb, sessions: sessions, repo: repo,
		audit: capture, authz: authz, users: users, groups: groups, router: mux,
	}
}

// request 以指定身份发起请求（nil 表示未认证）。安全链由各任务自己的测试
// 覆盖；这里直接注入 Principal 以隔离端点逻辑。
func (e *adminTestEnv) request(p *auth.Principal, method, path string, body any) *httptest.ResponseRecorder {
	e.t.Helper()
	var reader io.Reader = http.NoBody
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			e.t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if p != nil {
		req = req.WithContext(auth.WithPrincipal(req.Context(), p))
	}
	return serve(e.router, req)
}

func (e *adminTestEnv) rawRequest(p *auth.Principal, method, path, raw string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if p != nil {
		req = req.WithContext(auth.WithPrincipal(req.Context(), p))
	}
	return serve(e.router, req)
}

func fullAdmin() *auth.Principal {
	// Permissions=nil 是 DB NULL=FULL（rbac 的冻结语义）。
	return &auth.Principal{UserID: adminTestActorID, Role: rbac.RoleAdmin}
}

func limitedAdmin(perms ...string) *auth.Principal {
	return &auth.Principal{UserID: adminTestActorID, Role: rbac.RoleAdmin, Permissions: perms}
}

func plainUser() *auth.Principal {
	return &auth.Principal{UserID: 2, Role: rbac.RoleUser}
}

func decodeUserView(t *testing.T, rr *httptest.ResponseRecorder) adminUserView {
	t.Helper()
	var view adminUserView
	if err := json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
		t.Fatalf("user view JSON 非法: %v（body=%q）", err, rr.Body.String())
	}
	return view
}

func decodeAdminList(t *testing.T, rr *httptest.ResponseRecorder) adminUserListView {
	t.Helper()
	var list adminUserListView
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("user list JSON 非法: %v（body=%q）", err, rr.Body.String())
	}
	return list
}

// ---------------------------------------------------------------------------
// (a) 越权
// ---------------------------------------------------------------------------

func TestUsersListRejectsNonAdmin(t *testing.T) {
	env := newAdminTestEnv(t)
	env.repo.addUser(AdminUserRecord{Username: "alice"})

	rr := env.request(plainUser(), http.MethodGet, "/api/users", nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("user 角色 status = %d，期望 403（body=%q）", rr.Code, rr.Body.String())
	}

	rr = env.request(limitedAdmin(rbac.PermUsersCreate), http.MethodGet, "/api/users", nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("缺 users:read 的 admin status = %d，期望 403", rr.Code)
	}

	rr = env.request(limitedAdmin(rbac.PermUsersRead), http.MethodGet, "/api/users", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("持 users:read 的 admin status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
}

func TestUsersListFiltersAndEnvelope(t *testing.T) {
	env := newAdminTestEnv(t)
	env.repo.addUser(AdminUserRecord{Username: "alice", Status: "enabled", Role: rbac.RoleAdmin, Group: "default"})
	env.repo.addUser(AdminUserRecord{Username: "bob", Status: "disabled", Role: rbac.RoleUser, Group: "vip"})
	env.repo.addGroup("vip", false)

	rr := env.request(fullAdmin(), http.MethodGet,
		"/api/users?keyword=ali&status=enabled&role=admin&group=default&p=2&page_size=10&sort=-balance", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	got := env.repo.lastFilter
	want := UserListFilter{Keyword: "ali", Status: "enabled", Role: "admin", Group: "default",
		Page: 2, PageSize: 10, Sort: "-balance"}
	if got != want {
		t.Fatalf("filter = %+v，期望 %+v", got, want)
	}
	list := decodeAdminList(t, rr)
	// 只有 alice 命中筛选；p=2 的页为空，但 total/page/page_size 仍是契约值。
	if list.Page != 2 || list.PageSize != 10 || list.Total != 1 || len(list.Items) != 0 {
		t.Fatalf("分页信封 = %+v，期望 page=2 size=10 total=1 items=0", list)
	}
	if got := env.repo.lastFilter; got != want {
		t.Fatalf("filter = %+v，期望 %+v", got, want)
	}

	// 第一页可见行，且 admin_permissions 的 NULL=FULL 以 JSON null 表达。
	rr = env.request(fullAdmin(), http.MethodGet,
		"/api/users?keyword=ali&status=enabled&role=admin&group=default&page_size=10", nil)
	list = decodeAdminList(t, rr)
	if len(list.Items) != 1 || list.Items[0].Username != "alice" {
		t.Fatalf("第一页 = %+v，期望单行 alice", list.Items)
	}
	if string(list.Items[0].AdminPermissions) != "null" {
		t.Fatalf("admin_permissions = %s，期望 null（DB NULL=FULL）", list.Items[0].AdminPermissions)
	}

	// 默认排序是 created_at 倒序；非法排序值必须 400，而不是被静默忽略。
	rr = env.request(fullAdmin(), http.MethodGet, "/api/users", nil)
	if rr.Code != http.StatusOK || env.repo.lastFilter.Sort != "-created_at" {
		t.Fatalf("默认排序 = %q（status=%d），期望 -created_at", env.repo.lastFilter.Sort, rr.Code)
	}
	for _, path := range []string{
		"/api/users?sort=password_hash",
		"/api/users?page_size=101",
		"/api/users?p=0",
		"/api/users?p=1000001",
		"/api/users?status=deleted",
		"/api/users?role=root",
	} {
		rr = env.request(fullAdmin(), http.MethodGet, path, nil)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d，期望 400", path, rr.Code)
		}
	}
}

// ---------------------------------------------------------------------------
// (b) 幂等创建
// ---------------------------------------------------------------------------

func TestUsersCreateIdempotent(t *testing.T) {
	env := newAdminTestEnv(t)
	body := map[string]any{"username": "bob", "password": "correct-horse-battery", "role": "user"}

	first := env.request(fullAdmin(), http.MethodPost, "/api/users", body)
	// 第一次请求未带 Idempotency-Key：创建。
	if first.Code != http.StatusCreated {
		t.Fatalf("first status = %d，期望 201（body=%q）", first.Code, first.Body.String())
	}
	if env.repo.createCalls != 1 {
		t.Fatalf("createCalls = %d，期望 1", env.repo.createCalls)
	}

	// 第二次带同一 Idempotency-Key，请求体相同 → 直接重放，不再建用户。
	req := httptest.NewRequest(http.MethodPost, "/api/users", strings.NewReader(`{"username":"carol","password":"correct-horse-battery","role":"user"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "create-carol")
	req = req.WithContext(auth.WithPrincipal(req.Context(), fullAdmin()))
	one := serve(env.router, req)
	if one.Code != http.StatusCreated {
		t.Fatalf("idempotent first status = %d，期望 201（body=%q）", one.Code, one.Body.String())
	}
	if env.repo.createCalls != 2 {
		t.Fatalf("createCalls = %d，期望 2", env.repo.createCalls)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/users", strings.NewReader(`{"username":"carol","password":"correct-horse-battery","role":"user"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "create-carol")
	req = req.WithContext(auth.WithPrincipal(req.Context(), fullAdmin()))
	replay := serve(env.router, req)
	if replay.Code != http.StatusCreated {
		t.Fatalf("replay status = %d，期望 201（body=%q）", replay.Code, replay.Body.String())
	}
	if replay.Body.String() != one.Body.String() {
		t.Fatalf("重放响应不一致:\n first=%s\nreplay=%s", one.Body.String(), replay.Body.String())
	}
	if env.repo.createCalls != 2 {
		t.Fatalf("同一 Idempotency-Key 重复创建：createCalls = %d，期望 2", env.repo.createCalls)
	}

	// 同一 key + 不同请求体 → 422。
	req = httptest.NewRequest(http.MethodPost, "/api/users", strings.NewReader(`{"username":"dave","password":"correct-horse-battery","role":"user"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "create-carol")
	req = req.WithContext(auth.WithPrincipal(req.Context(), fullAdmin()))
	mismatch := serve(env.router, req)
	if mismatch.Code != http.StatusUnprocessableEntity {
		t.Fatalf("不同请求体复用 key status = %d，期望 422（body=%q）", mismatch.Code, mismatch.Body.String())
	}
	if env.repo.createCalls != 2 {
		t.Fatalf("422 路径不应创建用户：createCalls = %d", env.repo.createCalls)
	}
}

func TestUsersCreateDefaultsAndValidation(t *testing.T) {
	env := newAdminTestEnv(t)

	rr := env.request(fullAdmin(), http.MethodPost, "/api/users",
		map[string]any{"username": "bob", "password": "correct-horse-battery"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d，期望 201（body=%q）", rr.Code, rr.Body.String())
	}
	view := decodeUserView(t, rr)
	if view.Group != "default" || view.Role != rbac.RoleUser || view.Email != nil {
		t.Fatalf("默认值错误: %+v（期望 group=default role=user email=null）", view)
	}

	// 重复用户名 → 409。
	rr = env.request(fullAdmin(), http.MethodPost, "/api/users",
		map[string]any{"username": "bob", "password": "correct-horse-battery"})
	if rr.Code != http.StatusConflict {
		t.Fatalf("重复用户名 status = %d，期望 409（body=%q）", rr.Code, rr.Body.String())
	}

	for name, body := range map[string]map[string]any{
		"bad username":  {"username": "a b", "password": "correct-horse-battery"},
		"short pwd":     {"username": "dave", "password": "short"},
		"weak pwd":      {"username": "dave", "password": "password"},
		"unknown group": {"username": "dave", "password": "correct-horse-battery", "group": "nope"},
		"bad role":      {"username": "dave", "password": "correct-horse-battery", "role": "root"},
	} {
		rr = env.request(fullAdmin(), http.MethodPost, "/api/users", body)
		if rr.Code != http.StatusBadRequest && rr.Code != http.StatusConflict {
			t.Fatalf("%s status = %d，期望 400/409（body=%q）", name, rr.Code, rr.Body.String())
		}
	}

	// 缺 users:manage-roles 的 admin 不能创建 admin（防越权提权）。
	rr = env.request(limitedAdmin(rbac.PermUsersCreate), http.MethodPost, "/api/users",
		map[string]any{"username": "eve", "password": "correct-horse-battery", "role": "admin"})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("受限 admin 创建 admin status = %d，期望 403（body=%q）", rr.Code, rr.Body.String())
	}

	// 全权 admin 创建 admin：权限默认为只读起步，不是 NULL=FULL。
	rr = env.request(fullAdmin(), http.MethodPost, "/api/users",
		map[string]any{"username": "frank", "password": "correct-horse-battery", "role": "admin"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("创建 admin status = %d，期望 201（body=%q）", rr.Code, rr.Body.String())
	}
	view = decodeUserView(t, rr)
	var perms []string
	if err := json.Unmarshal(view.AdminPermissions, &perms); err != nil {
		t.Fatalf("admin_permissions 不是数组: %v（raw=%s）", err, view.AdminPermissions)
	}
	if len(perms) != 2 || perms[0] != rbac.PermUsersRead || perms[1] != rbac.PermLogsRead {
		t.Fatalf("新 admin 权限 = %v，期望只读起步 [users:read logs:read]", perms)
	}
}

// ---------------------------------------------------------------------------
// (b2) 客户属性 customer_type：仅用于筛选/统计，折扣规则绝不按它匹配
// ---------------------------------------------------------------------------

func TestCreateUserDefaultsCustomerTypeGeneral(t *testing.T) {
	env := newAdminTestEnv(t)

	rr := env.request(fullAdmin(), http.MethodPost, "/api/users",
		map[string]any{"username": "bob", "password": "correct-horse-battery"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d，期望 201（body=%q）", rr.Code, rr.Body.String())
	}
	if view := decodeUserView(t, rr); view.CustomerType != "general" {
		t.Fatalf("customer_type = %q，期望 general", view.CustomerType)
	}
}

func TestCreateUserAcceptsExplicitCustomerType(t *testing.T) {
	env := newAdminTestEnv(t)

	rr := env.request(fullAdmin(), http.MethodPost, "/api/users", map[string]any{
		"username": "bob", "password": "correct-horse-battery", "customer_type": "enterprise",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d，期望 201（body=%q）", rr.Code, rr.Body.String())
	}
	view := decodeUserView(t, rr)
	if view.CustomerType != customerTypeEnterprise {
		t.Fatalf("customer_type = %q，期望 enterprise", view.CustomerType)
	}
	// 假仓储的 CreateUser 只在收到入参时才落该值：回读证明入参确实带上了它。
	stored, err := env.repo.GetUser(context.Background(), view.ID)
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if stored.CustomerType != customerTypeEnterprise {
		t.Fatalf("仓储 customer_type = %q，期望 enterprise", stored.CustomerType)
	}
}

func TestCreateUserRejectsUnknownCustomerType(t *testing.T) {
	env := newAdminTestEnv(t)

	rr := env.request(fullAdmin(), http.MethodPost, "/api/users", map[string]any{
		"username": "bob", "password": "correct-horse-battery", "customer_type": "vip",
	})
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d，期望 422（body=%q）", rr.Code, rr.Body.String())
	}
	assertProblemFields(t, rr, "customer_type")
	if env.repo.createCalls != 0 {
		t.Fatalf("createCalls = %d，期望 0（校验失败不得落库）", env.repo.createCalls)
	}
}

func TestUpdateUserChangesCustomerType(t *testing.T) {
	env := newAdminTestEnv(t)
	user := env.repo.addUser(AdminUserRecord{Username: "alice"})

	rr := env.request(fullAdmin(), http.MethodPatch, fmt.Sprintf("/api/users/%d", user.ID),
		map[string]any{"customer_type": "enterprise", "updated_at": user.UpdatedAt})
	if rr.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	if view := decodeUserView(t, rr); view.CustomerType != "enterprise" {
		t.Fatalf("customer_type = %q，期望 enterprise", view.CustomerType)
	}

	rr = env.request(fullAdmin(), http.MethodGet, fmt.Sprintf("/api/users/%d", user.ID), nil)
	if rr.Code != http.StatusOK || decodeUserView(t, rr).CustomerType != "enterprise" {
		t.Fatalf("GET user status = %d（body=%q），期望 customer_type=enterprise", rr.Code, rr.Body.String())
	}
}

func TestListUsersFiltersByCustomerType(t *testing.T) {
	env := newAdminTestEnv(t)
	env.repo.addUser(AdminUserRecord{Username: "alice", CustomerType: "general"})
	env.repo.addUser(AdminUserRecord{Username: "bob", CustomerType: "enterprise"})

	rr := env.request(fullAdmin(), http.MethodGet, "/api/users?customer_type=enterprise", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	if got := env.repo.lastFilter.CustomerType; got != "enterprise" {
		t.Fatalf("filter.customer_type = %q，期望 enterprise", got)
	}
	list := decodeAdminList(t, rr)
	if list.Total != 1 || len(list.Items) != 1 || list.Items[0].Username != "bob" {
		t.Fatalf("列表 = %+v，期望只回企业客户 bob", list)
	}
	if list.Items[0].CustomerType != "enterprise" {
		t.Fatalf("customer_type = %q，期望 enterprise", list.Items[0].CustomerType)
	}

	// 非法枚举 → 400，而不是静默忽略筛选。
	rr = env.request(fullAdmin(), http.MethodGet, "/api/users?customer_type=vip", nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("非法 customer_type status = %d，期望 400（body=%q）", rr.Code, rr.Body.String())
	}
}

func TestUpdateUserRejectsUnknownCustomerType(t *testing.T) {
	env := newAdminTestEnv(t)
	user := env.repo.addUser(AdminUserRecord{Username: "alice"})

	rr := env.request(fullAdmin(), http.MethodPatch, fmt.Sprintf("/api/users/%d", user.ID),
		map[string]any{"customer_type": "vip", "updated_at": user.UpdatedAt})
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d，期望 422（body=%q）", rr.Code, rr.Body.String())
	}
	assertProblemFields(t, rr, "customer_type")

	rr = env.request(fullAdmin(), http.MethodGet, fmt.Sprintf("/api/users/%d", user.ID), nil)
	if rr.Code != http.StatusOK || decodeUserView(t, rr).CustomerType != "general" {
		t.Fatalf("非法 PATCH 后 customer_type 被改写（status=%d body=%q）", rr.Code, rr.Body.String())
	}
}

func TestUpdateUserRejectsNullCustomerType(t *testing.T) {
	env := newAdminTestEnv(t)
	user := env.repo.addUser(AdminUserRecord{Username: "alice"})

	rr := env.request(fullAdmin(), http.MethodPatch, fmt.Sprintf("/api/users/%d", user.ID),
		map[string]any{"customer_type": nil, "updated_at": user.UpdatedAt})
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d，期望 422（body=%q）", rr.Code, rr.Body.String())
	}
	assertProblemFields(t, rr, "customer_type")

	rr = env.request(fullAdmin(), http.MethodGet, fmt.Sprintf("/api/users/%d", user.ID), nil)
	if rr.Code != http.StatusOK || decodeUserView(t, rr).CustomerType != "general" {
		t.Fatalf("null PATCH 后 customer_type 被改写（status=%d body=%q）", rr.Code, rr.Body.String())
	}
}

// ---------------------------------------------------------------------------
// (c) PATCH 拒绝口令字段
// ---------------------------------------------------------------------------

func TestUsersPatchRejectsPassword(t *testing.T) {
	env := newAdminTestEnv(t)
	user := env.repo.addUser(AdminUserRecord{Username: "alice"})

	rr := env.request(fullAdmin(), http.MethodPatch, fmt.Sprintf("/api/users/%d", user.ID),
		map[string]any{"password": "attacker-chosen-password", "updated_at": user.UpdatedAt})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("PATCH 含 password status = %d，期望 400（body=%q）", rr.Code, rr.Body.String())
	}

	// 其它未登记字段仍由字段层 403 兜底。
	rr = env.request(fullAdmin(), http.MethodPatch, fmt.Sprintf("/api/users/%d", user.ID),
		map[string]any{"password_hash": "x", "updated_at": user.UpdatedAt})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("PATCH 含 password_hash status = %d，期望 403（body=%q）", rr.Code, rr.Body.String())
	}
}

func TestUsersPatchOptimisticLockAndFields(t *testing.T) {
	env := newAdminTestEnv(t)
	user := env.repo.addUser(AdminUserRecord{Username: "alice", Remark: strPtrAdmin("old")})

	rr := env.request(fullAdmin(), http.MethodPatch, fmt.Sprintf("/api/users/%d", user.ID),
		map[string]any{"remark": "new", "updated_at": user.UpdatedAt})
	if rr.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	view := decodeUserView(t, rr)
	if view.Remark == nil || *view.Remark != "new" {
		t.Fatalf("remark = %v，期望 new", view.Remark)
	}

	// 旧 updated_at → 409。
	rr = env.request(fullAdmin(), http.MethodPatch, fmt.Sprintf("/api/users/%d", user.ID),
		map[string]any{"remark": "stale", "updated_at": user.UpdatedAt})
	if rr.Code != http.StatusConflict {
		t.Fatalf("陈旧 updated_at status = %d，期望 409（body=%q）", rr.Code, rr.Body.String())
	}

	// 缺 updated_at → 400。
	rr = env.request(fullAdmin(), http.MethodPatch, fmt.Sprintf("/api/users/%d", user.ID),
		map[string]any{"remark": "x"})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("缺 updated_at status = %d，期望 400", rr.Code)
	}

	// 不存在的分组 → 400。
	rr = env.request(fullAdmin(), http.MethodPatch, fmt.Sprintf("/api/users/%d", user.ID),
		map[string]any{"group": "nope", "updated_at": view.UpdatedAt})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("不存在分组 status = %d，期望 400（body=%q）", rr.Code, rr.Body.String())
	}

	rr = env.request(fullAdmin(), http.MethodGet, fmt.Sprintf("/api/users/%d", user.ID), nil)
	if rr.Code != http.StatusOK || decodeUserView(t, rr).Username != "alice" {
		t.Fatalf("GET user status = %d（body=%q）", rr.Code, rr.Body.String())
	}
	rr = env.request(fullAdmin(), http.MethodGet, "/api/users/9999", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("不存在用户 status = %d，期望 404", rr.Code)
	}
}

// ---------------------------------------------------------------------------
// (d) 封禁/解禁
// ---------------------------------------------------------------------------

func TestUsersStatusDisablesAndEnables(t *testing.T) {
	env := newAdminTestEnv(t)
	user := env.repo.addUser(AdminUserRecord{Username: "alice"})
	enabledToken := env.repo.addToken(user.ID, "enabled")
	disabledToken := env.repo.addToken(user.ID, "disabled")

	sid, err := env.sessions.Create(context.Background(), user.ID, auth.SessionOpts{
		UA: "Test/1.0", IP: "203.0.113.9", Role: rbac.RoleUser,
	})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}

	rr := env.request(fullAdmin(), http.MethodPost, fmt.Sprintf("/api/users/%d/status", user.ID),
		map[string]any{"status": "disabled"})
	if rr.Code != http.StatusOK {
		t.Fatalf("disable status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	if got := env.repo.userStatus(user.ID); got != "disabled" {
		t.Fatalf("user status = %q，期望 disabled", got)
	}
	if got := env.repo.tokenStatus(enabledToken); got != "disabled" {
		t.Fatalf("封禁前 enabled 的令牌 = %q，期望 disabled", got)
	}
	if got := env.repo.tokenStatus(disabledToken); got != "disabled" {
		t.Fatalf("封禁前 disabled 的令牌 = %q，期望保持 disabled", got)
	}
	if _, err := env.sessions.Get(context.Background(), sid); !errors.Is(err, auth.ErrSessionNotFound) {
		t.Fatalf("封禁后会话仍存在：err=%v", err)
	}
	if env.mr.Exists(userBanTokenKeyPrefix+strconv.FormatInt(user.ID, 10)) != true {
		t.Fatal("未记录解禁恢复集")
	}

	// 重复封禁幂等：不再改写恢复集。
	rr = env.request(fullAdmin(), http.MethodPost, fmt.Sprintf("/api/users/%d/status", user.ID),
		map[string]any{"status": "disabled"})
	if rr.Code != http.StatusOK {
		t.Fatalf("重复封禁 status = %d，期望 200", rr.Code)
	}

	rr = env.request(fullAdmin(), http.MethodPost, fmt.Sprintf("/api/users/%d/status", user.ID),
		map[string]any{"status": "enabled"})
	if rr.Code != http.StatusOK {
		t.Fatalf("enable status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	if got := env.repo.userStatus(user.ID); got != "enabled" {
		t.Fatalf("user status = %q，期望 enabled", got)
	}
	if got := env.repo.tokenStatus(enabledToken); got != "enabled" {
		t.Fatalf("解禁后原 enabled 令牌 = %q，期望恢复 enabled", got)
	}
	if got := env.repo.tokenStatus(disabledToken); got != "disabled" {
		t.Fatalf("解禁后原 disabled 令牌 = %q，期望保持 disabled（TC-F-07-05-02）", got)
	}
	if env.mr.Exists(userBanTokenKeyPrefix + strconv.FormatInt(user.ID, 10)) {
		t.Fatal("解禁后应删除恢复集")
	}

	// 封禁不存在的用户 → 404。
	rr = env.request(fullAdmin(), http.MethodPost, "/api/users/9999/status", map[string]any{"status": "disabled"})
	if rr.Code != http.StatusNotFound {
		t.Fatalf("封禁不存在用户 status = %d，期望 404", rr.Code)
	}
	// 无 users:disable → 403。
	rr = env.request(limitedAdmin(rbac.PermUsersRead), http.MethodPost,
		fmt.Sprintf("/api/users/%d/status", user.ID), map[string]any{"status": "disabled"})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("缺 users:disable status = %d，期望 403", rr.Code)
	}
}

// ---------------------------------------------------------------------------
// 删除
// ---------------------------------------------------------------------------

func TestUsersDeleteRules(t *testing.T) {
	env := newAdminTestEnv(t)
	user := env.repo.addUser(AdminUserRecord{Username: "alice"})
	env.repo.addToken(user.ID, "enabled")

	rr := env.request(fullAdmin(), http.MethodDelete, fmt.Sprintf("/api/users/%d", user.ID), nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("删除 enabled 用户 status = %d，期望 409（body=%q）", rr.Code, rr.Body.String())
	}

	rr = env.request(fullAdmin(), http.MethodDelete, fmt.Sprintf("/api/users/%d", adminTestActorID), nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("删除自己 status = %d，期望 400", rr.Code)
	}

	rr = env.request(fullAdmin(), http.MethodPost, fmt.Sprintf("/api/users/%d/status", user.ID),
		map[string]any{"status": "disabled"})
	if rr.Code != http.StatusOK {
		t.Fatalf("disable status = %d", rr.Code)
	}
	rr = env.request(fullAdmin(), http.MethodDelete, fmt.Sprintf("/api/users/%d", user.ID), nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("删除 disabled 用户 status = %d，期望 204（body=%q）", rr.Code, rr.Body.String())
	}
	if _, err := env.repo.GetUser(context.Background(), user.ID); !errors.Is(err, ErrAdminUserNotFound) {
		t.Fatalf("删除后用户仍存在：err=%v", err)
	}
	if env.repo.tokenStatus(1) != "" {
		t.Fatal("删除后令牌未清理")
	}
	// 审计仍记录被删用户 id；不得依赖改写 audit_logs 才能完成删除。
	captured := env.audit.concatenatedArgs()
	if !strings.Contains(captured, "deleted_user_id") {
		t.Fatalf("审计缺少 deleted_user_id：%q", captured)
	}
}

// ---------------------------------------------------------------------------
// (e) 自我降权 / 权限矩阵自身
// ---------------------------------------------------------------------------

func TestUsersSelfDemotionRejected(t *testing.T) {
	env := newAdminTestEnv(t)
	actor := env.repo.addUser(AdminUserRecord{Username: "root", Role: rbac.RoleAdmin})
	// 让请求身份与 repo 中的行一致（ID 由假仓储分配）。
	principal := &auth.Principal{UserID: actor.ID, Role: rbac.RoleAdmin}

	rr := env.request(principal, http.MethodPatch, fmt.Sprintf("/api/users/%d", actor.ID),
		map[string]any{"role": "user", "updated_at": actor.UpdatedAt})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("对自己降权 status = %d，期望 400（body=%q）", rr.Code, rr.Body.String())
	}
	if got := env.repo.userRole(actor.ID); got != rbac.RoleAdmin {
		t.Fatalf("自我降权失败后角色 = %q，期望保持 admin", got)
	}

	rr = env.request(principal, http.MethodPatch, fmt.Sprintf("/api/users/%d/permissions", actor.ID),
		map[string]any{"admin_permissions": []string{rbac.PermUsersRead}})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("修改自己的权限 status = %d，期望 400（body=%q）", rr.Code, rr.Body.String())
	}
}

// ---------------------------------------------------------------------------
// (f) 调额（含并发）
// ---------------------------------------------------------------------------

func TestUsersQuotaAdjustmentModes(t *testing.T) {
	env := newAdminTestEnv(t)
	user := env.repo.addUser(AdminUserRecord{Username: "alice", Balance: 1000})

	rr := env.request(fullAdmin(), http.MethodPost, fmt.Sprintf("/api/users/%d/quota-adjustment", user.ID),
		map[string]any{"mode": "add", "amount": 500, "idempotency_key": "add-1"})
	if rr.Code != http.StatusOK {
		t.Fatalf("add status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	var added adminQuotaAdjustmentResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &added); err != nil {
		t.Fatalf("add JSON: %v", err)
	}
	if added.NewBalance != 1500 || added.TransactionID == 0 {
		t.Fatalf("add result = %+v，期望 new_balance=1500", added)
	}

	// 同一 idempotency_key 重放：余额与流水都不再变化。
	rr = env.request(fullAdmin(), http.MethodPost, fmt.Sprintf("/api/users/%d/quota-adjustment", user.ID),
		map[string]any{"mode": "add", "amount": 500, "idempotency_key": "add-1"})
	var replayed adminQuotaAdjustmentResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &replayed)
	if rr.Code != http.StatusOK || replayed != added {
		t.Fatalf("idempotent replay = %+v（status=%d），期望 %+v", replayed, rr.Code, added)
	}
	if len(env.repo.txs) != 1 {
		t.Fatalf("幂等重放产生了额外流水：%d", len(env.repo.txs))
	}

	rr = env.request(fullAdmin(), http.MethodPost, fmt.Sprintf("/api/users/%d/quota-adjustment", user.ID),
		map[string]any{"mode": "subtract", "amount": 500, "idempotency_key": "sub-1"})
	_ = json.Unmarshal(rr.Body.Bytes(), &replayed)
	if rr.Code != http.StatusOK || replayed.NewBalance != 1000 {
		t.Fatalf("subtract status = %d result=%+v，期望 1000", rr.Code, replayed)
	}

	// 超额扣减 → 400 且余额不变。
	rr = env.request(fullAdmin(), http.MethodPost, fmt.Sprintf("/api/users/%d/quota-adjustment", user.ID),
		map[string]any{"mode": "subtract", "amount": 1001, "idempotency_key": "sub-2"})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("超额扣减 status = %d，期望 400（body=%q）", rr.Code, rr.Body.String())
	}
	stored, _ := env.repo.GetUser(context.Background(), user.ID)
	if stored.Balance != 1000 {
		t.Fatalf("超额扣减改变了余额：%d", stored.Balance)
	}

	// override 0 合法（清零）；override 无变化 → 400。
	rr = env.request(fullAdmin(), http.MethodPost, fmt.Sprintf("/api/users/%d/quota-adjustment", user.ID),
		map[string]any{"mode": "override", "amount": 0, "idempotency_key": "ovr-1"})
	_ = json.Unmarshal(rr.Body.Bytes(), &replayed)
	if rr.Code != http.StatusOK || replayed.NewBalance != 0 {
		t.Fatalf("override 0 status = %d result=%+v", rr.Code, replayed)
	}
	rr = env.request(fullAdmin(), http.MethodPost, fmt.Sprintf("/api/users/%d/quota-adjustment", user.ID),
		map[string]any{"mode": "override", "amount": 0, "idempotency_key": "ovr-2"})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("override 无变化 status = %d，期望 400", rr.Code)
	}

	// 参数校验。
	for name, body := range map[string]map[string]any{
		"bad mode":   {"mode": "multiply", "amount": 1, "idempotency_key": "x"},
		"zero add":   {"mode": "add", "amount": 0, "idempotency_key": "x"},
		"neg sub":    {"mode": "subtract", "amount": -5, "idempotency_key": "x"},
		"missing id": {"mode": "add", "amount": 5},
	} {
		rr = env.request(fullAdmin(), http.MethodPost, fmt.Sprintf("/api/users/%d/quota-adjustment", user.ID), body)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d，期望 400", name, rr.Code)
		}
	}
	// 不存在的用户 → 404。
	rr = env.request(fullAdmin(), http.MethodPost, "/api/users/9999/quota-adjustment",
		map[string]any{"mode": "add", "amount": 5, "idempotency_key": "x"})
	if rr.Code != http.StatusNotFound {
		t.Fatalf("不存在用户调额 status = %d，期望 404", rr.Code)
	}
}

func TestUsersQuotaConcurrentSubtractNeverNegative(t *testing.T) {
	env := newAdminTestEnv(t)
	user := env.repo.addUser(AdminUserRecord{Username: "alice", Balance: 100})
	principal := fullAdmin()

	const attempts = 20
	var wg sync.WaitGroup
	start := make(chan struct{})
	codes := make(chan int, attempts)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			rr := env.request(principal, http.MethodPost,
				fmt.Sprintf("/api/users/%d/quota-adjustment", user.ID),
				map[string]any{"mode": "subtract", "amount": 10,
					"idempotency_key": fmt.Sprintf("concurrent-%d", i)})
			codes <- rr.Code
		}(i)
	}
	close(start)
	wg.Wait()
	close(codes)

	successes := 0
	for code := range codes {
		switch code {
		case http.StatusOK:
			successes++
		case http.StatusBadRequest:
		default:
			t.Fatalf("并发扣减出现非预期状态码 %d", code)
		}
	}
	if successes != 10 {
		t.Fatalf("并发扣减成功数 = %d，期望 10（余额 100 / 每次 10）", successes)
	}
	stored, _ := env.repo.GetUser(context.Background(), user.ID)
	if stored.Balance != 0 {
		t.Fatalf("最终余额 = %d，期望 0", stored.Balance)
	}
	for _, tx := range env.repo.txs {
		if tx.BalanceAfter < 0 {
			t.Fatalf("流水出现负余额: %+v", tx)
		}
	}
}

// ---------------------------------------------------------------------------
// 账号恢复
// ---------------------------------------------------------------------------

func TestUsersReset2FA(t *testing.T) {
	env := newAdminTestEnv(t)
	user := env.repo.addUser(AdminUserRecord{Username: "alice", TOTPEnabled: false})

	rr := env.request(fullAdmin(), http.MethodPost, fmt.Sprintf("/api/users/%d/reset-2fa", user.ID), nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("未启用 2FA 重置 status = %d，期望 400（body=%q）", rr.Code, rr.Body.String())
	}

	env.repo.mu.Lock()
	env.repo.users[user.ID].TOTPEnabled = true
	env.repo.mu.Unlock()
	sid, err := env.sessions.Create(context.Background(), user.ID, auth.SessionOpts{
		UA: "Test/1.0", IP: "203.0.113.9", Role: rbac.RoleUser,
	})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}
	rr = env.request(fullAdmin(), http.MethodPost, fmt.Sprintf("/api/users/%d/reset-2fa", user.ID), nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("reset-2fa status = %d，期望 204（body=%q）", rr.Code, rr.Body.String())
	}
	stored, _ := env.repo.GetUser(context.Background(), user.ID)
	if stored.TOTPEnabled {
		t.Fatal("重置后 totp_enabled 仍为 true")
	}
	if _, err := env.sessions.Get(context.Background(), sid); !errors.Is(err, auth.ErrSessionNotFound) {
		t.Fatalf("重置 2FA 后会话仍存在：err=%v", err)
	}
}

func TestUsersResetPasswordAuditHasNoPassword(t *testing.T) {
	env := newAdminTestEnv(t)
	user := env.repo.addUser(AdminUserRecord{Username: "alice"})
	sid, err := env.sessions.Create(context.Background(), user.ID, auth.SessionOpts{
		UA: "Test/1.0", IP: "203.0.113.9", Role: rbac.RoleUser,
	})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}

	rr := env.request(fullAdmin(), http.MethodPost, fmt.Sprintf("/api/users/%d/reset-password", user.ID), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("reset-password status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	var resp adminResetPasswordResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("reset-password JSON: %v", err)
	}
	if len(resp.TempPassword) != tempPasswordLength {
		t.Fatalf("临时口令长度 = %d，期望 %d", len(resp.TempPassword), tempPasswordLength)
	}
	if !auth.ValidatePasswordLength(resp.TempPassword) {
		t.Fatalf("临时口令不满足长度策略: %q", resp.TempPassword)
	}
	stored, _ := env.repo.GetUser(context.Background(), user.ID)
	if !stored.MustChangePassword {
		t.Fatal("must_change_password 未置 true")
	}
	if env.repo.passwordHash == "" || strings.Contains(env.repo.passwordHash, resp.TempPassword) {
		t.Fatal("仓储收到了错误的口令材料（应只收到哈希）")
	}
	if _, err := env.sessions.Get(context.Background(), sid); !errors.Is(err, auth.ErrSessionNotFound) {
		t.Fatalf("重置口令后会话仍存在：err=%v", err)
	}
	if !env.mr.Exists(tempPasswordKeyPrefix + strconv.FormatInt(user.ID, 10)) {
		t.Fatal("未写入临时口令 TTL 标记")
	}

	// TC-F-07-12-05：审计行绝不含临时口令或哈希材料。审计 Logger 的实参
	// 包含 detail/username 等；逐字检查整个捕获内容。
	captured := env.audit.concatenatedArgs()
	if strings.Contains(captured, resp.TempPassword) {
		t.Fatalf("审计泄漏临时口令：%q", captured)
	}
	if strings.Contains(captured, env.repo.passwordHash) || strings.Contains(captured, "argon2id") {
		t.Fatalf("审计泄漏口令哈希：%q", captured)
	}
	// audit_logs 没有 action 列（Task 12 冻结 schema）：动作只用于推导
	// category。user.reset_password 属 operation 类；至少必须有一行审计。
	if len(env.audit.allCalls()) == 0 || !strings.Contains(captured, "operation") {
		t.Fatalf("缺少 reset-password 审计行：%q", captured)
	}

	// 创建用户的审计同样不得包含提交的口令。
	env2 := newAdminTestEnv(t)
	env2.request(fullAdmin(), http.MethodPost, "/api/users",
		map[string]any{"username": "bob", "password": "correct-horse-battery"})
	createAudit := env2.audit.concatenatedArgs()
	if strings.Contains(createAudit, "correct-horse-battery") || strings.Contains(createAudit, "argon2id") {
		t.Fatalf("创建用户审计泄漏口令材料：%q", createAudit)
	}

	// 重置自己 → 400。
	self := env.repo.addUser(AdminUserRecord{Username: "root", Role: rbac.RoleAdmin})
	rr = env.request(&auth.Principal{UserID: self.ID, Role: rbac.RoleAdmin},
		http.MethodPost, fmt.Sprintf("/api/users/%d/reset-password", self.ID), nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("重置自己口令 status = %d，期望 400", rr.Code)
	}
}

// ---------------------------------------------------------------------------
// (h) 权限矩阵超集校验
// ---------------------------------------------------------------------------

func TestUsersPermissionsSuperset(t *testing.T) {
	env := newAdminTestEnv(t)
	target := env.repo.addUser(AdminUserRecord{Username: "admin2", Role: rbac.RoleAdmin})

	// 全权 admin 可以授予任意目录键与 "*"。
	rr := env.request(fullAdmin(), http.MethodPatch, fmt.Sprintf("/api/users/%d/permissions", target.ID),
		map[string]any{"admin_permissions": []string{rbac.PermUsersRead, rbac.PermSettingsManage}})
	if rr.Code != http.StatusOK {
		t.Fatalf("全权 admin 授权 status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}

	// 受限 admin：授予超出自身 → 403 且不写库。
	limited := limitedAdmin(rbac.PermUsersManageRoles, rbac.PermUsersRead)
	storedBefore := env.repo.permWrites
	rr = env.request(limited, http.MethodPatch, fmt.Sprintf("/api/users/%d/permissions", target.ID),
		map[string]any{"admin_permissions": []string{rbac.PermSettingsManage}})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("越权授予 status = %d，期望 403（body=%q）", rr.Code, rr.Body.String())
	}
	if env.repo.permWrites != storedBefore {
		t.Fatal("403 后仍发生权限写入（必须无变更）")
	}

	// 非 FULL 授予 "*" → 403。
	rr = env.request(limited, http.MethodPatch, fmt.Sprintf("/api/users/%d/permissions", target.ID),
		map[string]any{"admin_permissions": []string{"*"}})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("受限 admin 授予 * status = %d，期望 403", rr.Code)
	}

	// 未知键 → 403。
	rr = env.request(limited, http.MethodPatch, fmt.Sprintf("/api/users/%d/permissions", target.ID),
		map[string]any{"admin_permissions": []string{"users:read", "users:superuser"}})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("未知权限键 status = %d，期望 403", rr.Code)
	}

	// 受限 admin 授予自身子集 → 200。
	rr = env.request(limited, http.MethodPatch, fmt.Sprintf("/api/users/%d/permissions", target.ID),
		map[string]any{"admin_permissions": []string{rbac.PermUsersRead}})
	if rr.Code != http.StatusOK {
		t.Fatalf("授予子集 status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}

	// 显式空数组 = 没有任何权限（不是 NULL=FULL）。
	rr = env.request(fullAdmin(), http.MethodPatch, fmt.Sprintf("/api/users/%d/permissions", target.ID),
		map[string]any{"admin_permissions": []string{}})
	if rr.Code != http.StatusOK {
		t.Fatalf("清空权限 status = %d，期望 200", rr.Code)
	}
	view := decodeUserView(t, rr)
	if string(view.AdminPermissions) != "[]" {
		t.Fatalf("admin_permissions = %s，期望 []", view.AdminPermissions)
	}

	// role=user 的目标 → 400（权限矩阵只对 admin 有意义）。
	plain := env.repo.addUser(AdminUserRecord{Username: "plain"})
	rr = env.request(fullAdmin(), http.MethodPatch, fmt.Sprintf("/api/users/%d/permissions", plain.ID),
		map[string]any{"admin_permissions": []string{rbac.PermUsersRead}})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("对普通用户设置权限 status = %d，期望 400（body=%q）", rr.Code, rr.Body.String())
	}

	// 缺 users:manage-roles → 403（功能级）。
	rr = env.request(limitedAdmin(rbac.PermUsersUpdate), http.MethodPatch,
		fmt.Sprintf("/api/users/%d/permissions", target.ID),
		map[string]any{"admin_permissions": []string{}})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("缺 users:manage-roles status = %d，期望 403", rr.Code)
	}
}

// ---------------------------------------------------------------------------
// 身份缺失
// ---------------------------------------------------------------------------

func TestUsersRequirePrincipal(t *testing.T) {
	env := newAdminTestEnv(t)
	rr := env.request(nil, http.MethodGet, "/api/users", nil)
	if rr.Code != http.StatusForbidden {
		// rbac Handler 先于业务 handler：缺 Principal → 403 fail-closed。
		t.Fatalf("未认证 status = %d，期望 403（body=%q）", rr.Code, rr.Body.String())
	}
}

func strPtrAdmin(value string) *string { return &value }

// ---------------------------------------------------------------------------
// Critical 1：角色变更吊销会话 + 陈旧 admin 会话不得借 NULL=FULL 继续
// ---------------------------------------------------------------------------

func TestUsersRoleChangeRevokesSessions(t *testing.T) {
	env := newAdminTestEnv(t)
	ctx := context.Background()

	// 降权他人：目标现有会话必须立即失效（SessionAuth 的 Role 是登录时快照）。
	target := env.repo.addUser(AdminUserRecord{Username: "admin2", Role: rbac.RoleAdmin})
	sid, err := env.sessions.Create(ctx, target.ID, auth.SessionOpts{
		UA: "Test/1.0", IP: "203.0.113.9", Role: rbac.RoleAdmin,
	})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}
	rr := env.request(fullAdmin(), http.MethodPatch, fmt.Sprintf("/api/users/%d", target.ID),
		map[string]any{"role": "user", "updated_at": target.UpdatedAt})
	if rr.Code != http.StatusOK {
		t.Fatalf("降权 status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	if got := env.repo.userRole(target.ID); got != rbac.RoleUser {
		t.Fatalf("降权后角色 = %q，期望 user", got)
	}
	if _, err := env.sessions.Get(ctx, sid); !errors.Is(err, auth.ErrSessionNotFound) {
		t.Fatalf("降权后目标会话仍存在：err=%v", err)
	}

	// 提权同样吊销：旧 user 会话不能继续代表旧角色，必须重新登录。
	user := env.repo.addUser(AdminUserRecord{Username: "bob"})
	sid2, err := env.sessions.Create(ctx, user.ID, auth.SessionOpts{
		UA: "Test/1.0", IP: "203.0.113.9", Role: rbac.RoleUser,
	})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}
	rr = env.request(fullAdmin(), http.MethodPatch, fmt.Sprintf("/api/users/%d", user.ID),
		map[string]any{"role": "admin", "updated_at": user.UpdatedAt})
	if rr.Code != http.StatusOK {
		t.Fatalf("提权 status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	if got := env.repo.userRole(user.ID); got != rbac.RoleAdmin {
		t.Fatalf("提权后角色 = %q，期望 admin", got)
	}
	if _, err := env.sessions.Get(ctx, sid2); !errors.Is(err, auth.ErrSessionNotFound) {
		t.Fatalf("提权后旧会话仍存在：err=%v", err)
	}
}

// adminStubLoader 模拟 SQLAuthRepo.Load 在「已降权」行上的返回值：
// 非 admin 行一律显式空集（绝不是 nil=FULL）。
type adminStubLoader struct{ perms []string }

func (l adminStubLoader) Load(context.Context, int64) ([]string, error) { return l.perms, nil }

func TestUsersStaleAdminSessionDeniedAfterDemotion(t *testing.T) {
	env := newAdminTestEnv(t)
	demoted := env.repo.addUser(AdminUserRecord{Username: "demoted", Role: rbac.RoleUser})

	// 旧会话仍带 Role=admin、Permissions=nil；Loader 按 DB 角色返回空集。
	env.authz.SetPermissionLoader(adminStubLoader{perms: []string{}})
	defer env.authz.SetPermissionLoader(nil)
	stale := &auth.Principal{UserID: demoted.ID, Role: rbac.RoleAdmin}
	rr := env.request(stale, http.MethodGet, "/api/users", nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("陈旧 admin 会话 status = %d，期望 403（body=%q）", rr.Code, rr.Body.String())
	}

	// 真正的 admin 行（NULL=FULL）仍由 Loader 返回 nil，管理端点保持可用。
	env.authz.SetPermissionLoader(adminStubLoader{perms: nil})
	rr = env.request(&auth.Principal{UserID: 999, Role: rbac.RoleAdmin}, http.MethodGet, "/api/users", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("FULL admin status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
}

// TestUsersRolePromotionRequiresGrantSubset：提权写入默认权限集，执行者必须
// 持有该集合的超集（否则 users:update 就能造出超出自身范围的 admin）。
func TestUsersRolePromotionRequiresGrantSubset(t *testing.T) {
	env := newAdminTestEnv(t)
	user := env.repo.addUser(AdminUserRecord{Username: "bob"})
	ctx := context.Background()
	sid, err := env.sessions.Create(ctx, user.ID, auth.SessionOpts{
		UA: "Test/1.0", IP: "203.0.113.9", Role: rbac.RoleUser,
	})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}

	limited := limitedAdmin(rbac.PermUsersUpdate, rbac.PermUsersManageRoles)
	rr := env.request(limited, http.MethodPatch, fmt.Sprintf("/api/users/%d", user.ID),
		map[string]any{"role": "admin", "updated_at": user.UpdatedAt})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("缺默认权限子集时提权 status = %d，期望 403（body=%q）", rr.Code, rr.Body.String())
	}
	if got := env.repo.userRole(user.ID); got != rbac.RoleUser {
		t.Fatalf("403 后角色 = %q，期望保持 user", got)
	}
	// 授权检查在副作用之前：403 不得吊销目标会话。
	if _, err := env.sessions.Get(ctx, sid); err != nil {
		t.Fatalf("403 路径不应吊销目标会话：err=%v", err)
	}

	allowed := limitedAdmin(rbac.PermUsersUpdate, rbac.PermUsersManageRoles,
		rbac.PermUsersRead, rbac.PermLogsRead)
	rr = env.request(allowed, http.MethodPatch, fmt.Sprintf("/api/users/%d", user.ID),
		map[string]any{"role": "admin", "updated_at": user.UpdatedAt})
	if rr.Code != http.StatusOK {
		t.Fatalf("持默认权限子集时提权 status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
}

// TestUsersRoleChangeStaleVersionDoesNotRevoke：陈旧 updated_at 的 409 路径
// 不得产生「先登出再失败」的副作用。
func TestUsersRoleChangeStaleVersionDoesNotRevoke(t *testing.T) {
	env := newAdminTestEnv(t)
	user := env.repo.addUser(AdminUserRecord{Username: "bob"})
	ctx := context.Background()
	sid, err := env.sessions.Create(ctx, user.ID, auth.SessionOpts{
		UA: "Test/1.0", IP: "203.0.113.9", Role: rbac.RoleUser,
	})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}

	rr := env.request(fullAdmin(), http.MethodPatch, fmt.Sprintf("/api/users/%d", user.ID),
		map[string]any{"role": "admin", "updated_at": user.UpdatedAt.Add(-time.Hour)})
	if rr.Code != http.StatusConflict {
		t.Fatalf("陈旧版本改角色 status = %d，期望 409（body=%q）", rr.Code, rr.Body.String())
	}
	if _, err := env.sessions.Get(ctx, sid); err != nil {
		t.Fatalf("409 路径不应吊销目标会话：err=%v", err)
	}
	if got := env.repo.userRole(user.ID); got != rbac.RoleUser {
		t.Fatalf("409 后角色 = %q，期望保持 user", got)
	}
}

// TestUsersResetPasswordFailsClosedWhenMarkerUnavailable：临时口令标记写失败
// 时不得改口令/返回密码——否则返回的密码在登录侧必然 401。
func TestUsersResetPasswordFailsClosedWhenMarkerUnavailable(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	// 会话管理器可用（miniredis），但端点自己的 Redis 客户端不可用：
	// 吊销会话成功，随后写 24h 标记失败 → 必须 503 且不落库。
	broken := redis.NewClient(&redis.Options{
		Addr: "127.0.0.1:1", MaxRetries: -1, DialTimeout: 50 * time.Millisecond,
	})
	t.Cleanup(func() { _ = broken.Close() })

	repo := newFakeAdminRepo()
	sessions := auth.NewSessionManager(rdb, []byte(testSessionSecret))
	h, err := NewUserHandlers(UserHandlerConfig{
		Repo: repo, Sessions: sessions, Redis: broken, Authz: rbac.NewAuthzMiddleware(),
	})
	if err != nil {
		t.Fatalf("NewUserHandlers: %v", err)
	}
	mux := http.NewServeMux()
	h.RegisterUserRoutes(mux, nil)
	user := repo.addUser(AdminUserRecord{Username: "alice"})

	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/users/%d/reset-password", user.ID), http.NoBody)
	req = req.WithContext(auth.WithPrincipal(req.Context(), fullAdmin()))
	rr := serve(mux, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("标记不可用时 reset-password status = %d，期望 503（body=%q）", rr.Code, rr.Body.String())
	}
	if repo.passwordHash != "" {
		t.Fatal("标记写失败后仍写入了口令哈希")
	}
	stored, _ := repo.GetUser(context.Background(), user.ID)
	if stored.MustChangePassword {
		t.Fatal("标记写失败后仍置了 must_change_password")
	}
	if strings.Contains(rr.Body.String(), "temp_password") {
		t.Fatalf("失败响应泄漏临时口令：%q", rr.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Important 1：封禁/删除必须广播 token:changed:<id>
// ---------------------------------------------------------------------------

func (e *adminTestEnv) subscribeToken(t *testing.T, tokenID int64) (*redis.PubSub, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	sub := e.rdb.Subscribe(ctx, fmt.Sprintf("token:changed:%d", tokenID))
	if _, err := sub.Receive(ctx); err != nil {
		cancel()
		_ = sub.Close()
		t.Fatalf("subscribe token:changed: %v", err)
	}
	return sub, cancel
}

func TestUsersStatusInvalidatesTokenCache(t *testing.T) {
	env := newAdminTestEnv(t)
	user := env.repo.addUser(AdminUserRecord{Username: "alice"})
	tokenID := env.repo.addToken(user.ID, "enabled")

	sub, cancel := env.subscribeToken(t, tokenID)
	defer cancel()
	defer func() { _ = sub.Close() }()

	rr := env.request(fullAdmin(), http.MethodPost, fmt.Sprintf("/api/users/%d/status", user.ID),
		map[string]any{"status": "disabled"})
	if rr.Code != http.StatusOK {
		t.Fatalf("disable status = %d（body=%q）", rr.Code, rr.Body.String())
	}
	msg, err := sub.ReceiveMessage(context.Background())
	if err != nil {
		t.Fatalf("未收到封禁的 token:changed 广播: %v", err)
	}
	if want := fmt.Sprintf("token:changed:%d", tokenID); msg.Channel != want {
		t.Fatalf("广播频道 = %q，期望 %q", msg.Channel, want)
	}
}

func TestUsersDeleteInvalidatesTokenCache(t *testing.T) {
	env := newAdminTestEnv(t)
	user := env.repo.addUser(AdminUserRecord{Username: "alice"})
	tokenID := env.repo.addToken(user.ID, "disabled")

	// 先封禁（会广播 enabled 令牌，但该令牌已 disabled，不会广播），再订阅，
	// 把删除本身的广播隔离出来。
	rr := env.request(fullAdmin(), http.MethodPost, fmt.Sprintf("/api/users/%d/status", user.ID),
		map[string]any{"status": "disabled"})
	if rr.Code != http.StatusOK {
		t.Fatalf("disable status = %d", rr.Code)
	}
	sub, cancel := env.subscribeToken(t, tokenID)
	defer cancel()
	defer func() { _ = sub.Close() }()

	rr = env.request(fullAdmin(), http.MethodDelete, fmt.Sprintf("/api/users/%d", user.ID), nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d（body=%q）", rr.Code, rr.Body.String())
	}
	if _, err := sub.ReceiveMessage(context.Background()); err != nil {
		t.Fatalf("未收到删除的 token:changed 广播: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Minor：溢出 / 分页上限
// ---------------------------------------------------------------------------

func TestUsersQuotaOverflowRejected(t *testing.T) {
	env := newAdminTestEnv(t)
	user := env.repo.addUser(AdminUserRecord{Username: "alice"})
	env.repo.mu.Lock()
	env.repo.users[user.ID].Balance = math.MaxInt64
	env.repo.mu.Unlock()

	rr := env.request(fullAdmin(), http.MethodPost, fmt.Sprintf("/api/users/%d/quota-adjustment", user.ID),
		map[string]any{"mode": "add", "amount": 1, "idempotency_key": "overflow"})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("溢出加额 status = %d，期望 400（body=%q）", rr.Code, rr.Body.String())
	}
	stored, _ := env.repo.GetUser(context.Background(), user.ID)
	if stored.Balance != math.MaxInt64 {
		t.Fatalf("溢出不变量：余额 = %d，期望不变", stored.Balance)
	}
	if len(env.repo.txs) != 0 {
		t.Fatal("溢出不变量：不应写流水")
	}
}

// TestUsersGrantDenialAudited 覆盖 D1 复核新增项：AuthorizeGrant 的 403 是
// 提权尝试，必须以 authz.denied（security）留痕，而不是只返回 403。
func TestUsersGrantDenialAudited(t *testing.T) {
	env := newAdminTestEnv(t)
	target := env.repo.addUser(AdminUserRecord{Username: "bob", Role: rbac.RoleAdmin})
	// 持有 users:manage-roles 但没有 pricing:manage：授予超出自身范围。
	limited := &auth.Principal{
		UserID:      900001,
		Role:        rbac.RoleAdmin,
		Permissions: []string{rbac.PermUsersManageRoles},
	}
	rr := env.request(limited, http.MethodPatch,
		fmt.Sprintf("/api/users/%d/permissions", target.ID),
		map[string]any{"admin_permissions": []string{rbac.PermPricingManage}})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d，期望 403（body=%q）", rr.Code, rr.Body.String())
	}

	var found bool
	for _, call := range env.audit.allCalls() {
		detail, ok := call.args[9].(string)
		if !ok {
			continue
		}
		if !strings.Contains(detail, `"event":"authz_denied"`) ||
			!strings.Contains(detail, `"reason":"grant_superset"`) ||
			!strings.Contains(detail, `"required_permission":"`+rbac.PermUsersManageRoles+`"`) {
			continue
		}
		if call.args[2] != "security" || call.args[8] != "failed" {
			t.Fatalf("拒绝审计 category/result = %v/%v，期望 security/failed", call.args[2], call.args[8])
		}
		found = true
	}
	if !found {
		t.Fatal("未写入 authz.denied（grant_superset）审计行")
	}
}

// TestUsersQuotaAdjustmentIdempotencyReuseMapsTo422 覆盖 C9 的 HTTP 映射：
// 同 key 不同参数 → 422 idempotency_key_reuse。
func TestUsersQuotaAdjustmentIdempotencyReuseMapsTo422(t *testing.T) {
	env := newAdminTestEnv(t)
	user := env.repo.addUser(AdminUserRecord{Username: "alice"})
	env.repo.mu.Lock()
	env.repo.forcedErr = ErrAdminIdempotencyReused
	env.repo.mu.Unlock()

	rr := env.request(fullAdmin(), http.MethodPost,
		fmt.Sprintf("/api/users/%d/quota-adjustment", user.ID),
		map[string]any{"mode": "add", "amount": 10, "idempotency_key": "k"})
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d，期望 422（body=%q）", rr.Code, rr.Body.String())
	}
	var p map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &p); err != nil {
		t.Fatalf("problem JSON 非法: %v", err)
	}
	if p["type"] != problemTypeIdempotencyReused {
		t.Fatalf("problem.type = %v，期望 %s", p["type"], problemTypeIdempotencyReused)
	}
}
