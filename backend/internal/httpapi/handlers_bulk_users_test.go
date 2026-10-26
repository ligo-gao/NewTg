package httpapi

// 批量建用户端点的单元验收（plan 2026-10-26 Task 9）。
//
// 本文件不触外部 PG/Redis：解析/校验是纯函数；handler 用例用 miniredis 与
// 内存 fake BulkUserRepo。重点钉住三条会静默造成生产事故的不变量：
//   1. must_change_password=true 时 Redis 标记必须在提交前写入，失败/提交
//      失败时不得留下任何标记（DB 有标记而 Redis 无标记 = 永远 401）；
//   2. CSV 导入所有行通过才插入（全有或全无）；
//   3. 生成端点的明文口令必须与返回的 username 同序配对（错位会把 A 的
//      口令发给 B）。

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/rbac"
	"newtg/gateway/internal/store"
)

// ---------------------------------------------------------------------------
// 纯函数：parseBulkCSV
// ---------------------------------------------------------------------------

func TestParseBulkCSVAcceptsAnyColumnOrder(t *testing.T) {
	raw := []byte("group,customer_type,password,username\ndefault,enterprise,longpassword123,alice\n")
	rows, errs := parseBulkCSV(raw)
	if len(errs) != 0 {
		t.Fatalf("解析错误 = %+v，期望无错误", errs)
	}
	if len(rows) != 1 {
		t.Fatalf("行数 = %d，期望 1", len(rows))
	}
	got := rows[0]
	if got.Username != "alice" || got.Password != "longpassword123" ||
		got.CustomerType != "enterprise" || got.Group != "default" {
		t.Fatalf("列映射错误：%+v", got)
	}
	if got.Line != 2 {
		t.Fatalf("line = %d，期望 2", got.Line)
	}
}

func TestParseBulkCSVRejectsMissingColumn(t *testing.T) {
	raw := []byte("username,group,customer_type\nalice,default,general\n")
	_, errs := parseBulkCSV(raw)
	if len(errs) != 1 || errs[0].Code != "header_invalid" {
		t.Fatalf("错误 = %+v，期望单条 header_invalid", errs)
	}
}

func TestParseBulkCSVRejectsUnknownAndDuplicateColumns(t *testing.T) {
	for name, raw := range map[string]string{
		"unknown":   "username,password,customer_type,group,role\nalice,longpassword123,general,default,admin\n",
		"duplicate": "username,password,password,group\nalice,longpassword123,longpassword456,default\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, errs := parseBulkCSV([]byte(raw))
			if len(errs) != 1 || errs[0].Code != "header_invalid" {
				t.Fatalf("错误 = %+v，期望单条 header_invalid", errs)
			}
		})
	}
}

func TestParseBulkCSVStripsBOMAndSpaces(t *testing.T) {
	raw := []byte("\ufeff username , password , customer_type , group \r\n alice , longpassword123 , general , default \r\n")
	rows, errs := parseBulkCSV(raw)
	if len(errs) != 0 {
		t.Fatalf("解析错误 = %+v，期望无错误", errs)
	}
	if len(rows) != 1 {
		t.Fatalf("行数 = %d，期望 1", len(rows))
	}
	got := rows[0]
	if got.Username != "alice" || got.Password != "longpassword123" ||
		got.CustomerType != "general" || got.Group != "default" {
		t.Fatalf("未剥除 BOM/空格：%+v", got)
	}
}

func TestParseBulkCSVRowLimit(t *testing.T) {
	var builder strings.Builder
	builder.WriteString("username,password,customer_type,group\n")
	for i := 0; i <= 500; i++ {
		fmt.Fprintf(&builder, "u%04d,longpassword123,general,default\n", i)
	}
	_, errs := parseBulkCSV([]byte(builder.String()))
	if len(errs) != 1 || errs[0].Code != "row_limit" {
		t.Fatalf("错误 = %+v，期望单条 row_limit", errs)
	}
}

// ---------------------------------------------------------------------------
// 纯函数：validateBulkRows
// ---------------------------------------------------------------------------

func TestValidateBulkRowsPasswordPolicy(t *testing.T) {
	rows := []bulkCSVRow{{Line: 2, Username: "alice", Password: "short", CustomerType: "general", Group: "default"}}
	errs := validateBulkRows(rows, nil, map[string]bool{"default": true}, auth.CompromisedPasswordChecker{})
	if len(errs) != 1 || errs[0].Code != "password_invalid" || errs[0].Line != 2 || errs[0].Field != "password" {
		t.Fatalf("错误 = %+v，期望 line=2 field=password code=password_invalid", errs)
	}
}

func TestValidateBulkRowsRejectsCompromisedPassword(t *testing.T) {
	// "password" 在 top-3000 命中且长度合法（8 位）：必须由泄露检查拒绝，
	// 而不是只靠长度策略。
	rows := []bulkCSVRow{{Line: 2, Username: "alice", Password: "password", CustomerType: "general", Group: "default"}}
	errs := validateBulkRows(rows, nil, map[string]bool{"default": true}, auth.CompromisedPasswordChecker{})
	if len(errs) != 1 || errs[0].Code != "password_invalid" {
		t.Fatalf("错误 = %+v，期望 password_invalid", errs)
	}
}

func TestValidateBulkRowsDuplicateInFile(t *testing.T) {
	rows := []bulkCSVRow{
		{Line: 2, Username: "alice", Password: "longpassword123", CustomerType: "general", Group: "default"},
		{Line: 3, Username: "alice", Password: "longpassword456", CustomerType: "general", Group: "default"},
	}
	errs := validateBulkRows(rows, nil, map[string]bool{"default": true}, auth.CompromisedPasswordChecker{})
	if len(errs) != 2 {
		t.Fatalf("错误数 = %d，期望 2（每个重复行各一条）：%+v", len(errs), errs)
	}
	for _, e := range errs {
		if e.Code != "username_duplicate" {
			t.Fatalf("code = %q，期望 username_duplicate", e.Code)
		}
	}
}

func TestValidateBulkRowsDuplicateInDB(t *testing.T) {
	rows := []bulkCSVRow{{Line: 2, Username: "alice", Password: "longpassword123", CustomerType: "general", Group: "default"}}
	errs := validateBulkRows(rows, map[string]bool{"alice": true}, map[string]bool{"default": true}, auth.CompromisedPasswordChecker{})
	if len(errs) != 1 || errs[0].Code != "username_duplicate" {
		t.Fatalf("错误 = %+v，期望 username_duplicate", errs)
	}
}

func TestValidateBulkRowsUnknownGroup(t *testing.T) {
	rows := []bulkCSVRow{{Line: 2, Username: "alice", Password: "longpassword123", CustomerType: "general", Group: "ghost"}}
	errs := validateBulkRows(rows, nil, map[string]bool{"default": true}, auth.CompromisedPasswordChecker{})
	if len(errs) != 1 || errs[0].Code != "group_not_found" || errs[0].Field != "group" {
		t.Fatalf("错误 = %+v，期望 group_not_found", errs)
	}
}

func TestValidateBulkRowsCustomerType(t *testing.T) {
	rows := []bulkCSVRow{{Line: 2, Username: "alice", Password: "longpassword123", CustomerType: "platinum", Group: "default"}}
	errs := validateBulkRows(rows, nil, map[string]bool{"default": true}, auth.CompromisedPasswordChecker{})
	if len(errs) != 1 || errs[0].Code != "customer_type_invalid" {
		t.Fatalf("错误 = %+v，期望 customer_type_invalid", errs)
	}
}

func TestValidateBulkRowsDefaults(t *testing.T) {
	rows := []bulkCSVRow{{Line: 2, Username: "alice", Password: "longpassword123"}}
	errs := validateBulkRows(rows, nil, map[string]bool{"default": true}, auth.CompromisedPasswordChecker{})
	if len(errs) != 0 {
		t.Fatalf("错误 = %+v，期望无错误", errs)
	}
	if rows[0].CustomerType != customerTypeGeneral || rows[0].Group != "default" {
		t.Fatalf("默认值未落：%+v", rows[0])
	}
}

func TestValidateBulkRowsRowLimit(t *testing.T) {
	rows := make([]bulkCSVRow, 501)
	for i := range rows {
		rows[i] = bulkCSVRow{Line: i + 2, Username: fmt.Sprintf("u%04d", i), Password: "longpassword123"}
	}
	errs := validateBulkRows(rows, nil, map[string]bool{"default": true}, auth.CompromisedPasswordChecker{})
	if len(errs) != 1 || errs[0].Code != "row_limit" {
		t.Fatalf("错误 = %+v，期望只有一条 row_limit（不再逐行报错）", errs)
	}
}

func TestFormatBulkCSV(t *testing.T) {
	raw, err := formatBulkCSV([]bulkCSVOutputRow{
		{Username: "alice", Password: "longpassword123", CustomerType: "general", Group: "default"},
	})
	if err != nil {
		t.Fatalf("formatBulkCSV: %v", err)
	}
	records, err := csv.NewReader(bytes.NewReader(raw)).ReadAll()
	if err != nil {
		t.Fatalf("读取输出 CSV: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("记录数 = %d，期望表头 + 1 行", len(records))
	}
	if strings.Join(records[0], ",") != "username,password,customer_type,group" {
		t.Fatalf("表头 = %v", records[0])
	}
	if strings.Join(records[1], ",") != "alice,longpassword123,general,default" {
		t.Fatalf("数据行 = %v", records[1])
	}
}

// ---------------------------------------------------------------------------
// 测试环境与 fake 仓储
// ---------------------------------------------------------------------------

type fakeBulkUserRepo struct {
	mu sync.Mutex

	existingUsers  map[string]bool
	existingGroups map[string]bool

	capturedRows   []store.BulkUserRow
	capturedOpts   store.BulkCreateOptions
	createCalls    int
	seq            int64
	afterInsertFun func(rows []store.BulkUserRow, afterInsert func(tx *sql.Tx, ids []int64) error) ([]store.BulkUserRow, error)
}

func newFakeBulkUserRepo() *fakeBulkUserRepo {
	return &fakeBulkUserRepo{
		existingUsers:  map[string]bool{},
		existingGroups: map[string]bool{"default": true},
		seq:            100,
	}
}

func (f *fakeBulkUserRepo) ExistingUsernames(_ context.Context, _ []string) (map[string]bool, error) {
	return f.existingUsers, nil
}

func (f *fakeBulkUserRepo) ExistingGroups(_ context.Context, _ []string) (map[string]bool, error) {
	return f.existingGroups, nil
}

func (f *fakeBulkUserRepo) BulkCreateUsers(
	_ context.Context, rows []store.BulkUserRow, opts store.BulkCreateOptions,
	afterInsert func(tx *sql.Tx, ids []int64) error,
) ([]store.BulkUserRow, error) {
	f.mu.Lock()
	f.createCalls++
	f.capturedRows = append([]store.BulkUserRow(nil), rows...)
	f.capturedOpts = opts
	f.mu.Unlock()

	if f.afterInsertFun != nil {
		return f.afterInsertFun(rows, afterInsert)
	}

	out := append([]store.BulkUserRow(nil), rows...)
	ids := make([]int64, len(out))
	for i := range out {
		if opts.AssignSequential {
			f.seq++
			out[i].Username = fmt.Sprintf("NewTG_user%04d", f.seq)
		}
		ids[i] = int64(1000 + i)
	}
	if afterInsert != nil {
		if err := afterInsert(nil, ids); err != nil {
			return nil, err
		}
	}
	return out, nil
}

type bulkTestEnv struct {
	t      *testing.T
	mr     *miniredis.Miniredis
	repo   *fakeBulkUserRepo
	audit  *adminAuditCapture
	router http.Handler
}

func newBulkTestEnv(t *testing.T, repo *fakeBulkUserRepo) *bulkTestEnv {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })

	capture := &adminAuditCapture{}
	handlers, err := NewBulkUserHandlers(BulkUserHandlerConfig{
		Repo:    repo,
		Redis:   rdb,
		Authz:   rbac.NewAuthzMiddleware(),
		Checker: auth.CompromisedPasswordChecker{},
		Audit:   audit.New(capture),
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewBulkUserHandlers: %v", err)
	}
	mux := http.NewServeMux()
	handlers.RegisterBulkUserRoutes(mux, nil)
	return &bulkTestEnv{t: t, mr: mr, repo: repo, audit: capture, router: mux}
}

func (e *bulkTestEnv) request(p *auth.Principal, method, path, contentType string, body io.Reader) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, path, body)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if p != nil {
		req = req.WithContext(auth.WithPrincipal(req.Context(), p))
	}
	return serve(e.router, req)
}

func (e *bulkTestEnv) importRequest(p *auth.Principal, csvBody string, filename string, mustChange bool) *httptest.ResponseRecorder {
	e.t.Helper()
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		e.t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := io.WriteString(part, csvBody); err != nil {
		e.t.Fatalf("write csv part: %v", err)
	}
	value := "false"
	if mustChange {
		value = "true"
	}
	if err := writer.WriteField("must_change_password", value); err != nil {
		e.t.Fatalf("WriteField: %v", err)
	}
	if err := writer.Close(); err != nil {
		e.t.Fatalf("close multipart writer: %v", err)
	}
	return e.request(p, http.MethodPost, "/api/users/bulk/import", writer.FormDataContentType(), &buf)
}

type bulkErrorList struct {
	Errors []bulkRowError `json:"errors"`
}

func decodeBulkErrors(t *testing.T, rr *httptest.ResponseRecorder) bulkErrorList {
	t.Helper()
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Fatalf("Content-Type = %q，期望 application/problem+json", ct)
	}
	var list bulkErrorList
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("错误响应 JSON 非法: %v（body=%q）", err, rr.Body.String())
	}
	return list
}

// ---------------------------------------------------------------------------
// 模板
// ---------------------------------------------------------------------------

func TestBulkTemplateHeadersAndBody(t *testing.T) {
	env := newBulkTestEnv(t, newFakeBulkUserRepo())
	rr := env.request(limitedAdmin(rbac.PermUsersCreate), http.MethodGet, "/api/users/bulk/template", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("状态 = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Content-Type"); got != "text/csv; charset=utf-8" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := rr.Header().Get("Content-Disposition"); !strings.Contains(got, "newtg-users-template.csv") {
		t.Fatalf("Content-Disposition = %q", got)
	}
	if got := rr.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q，期望 no-store", got)
	}
	if got := rr.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q，期望 nosniff", got)
	}
	if got := rr.Body.String(); got != "username,password,customer_type,group\n" {
		t.Fatalf("模板正文 = %q", got)
	}
}

func TestBulkEndpointsRequireUsersCreate(t *testing.T) {
	env := newBulkTestEnv(t, newFakeBulkUserRepo())
	readOnly := limitedAdmin(rbac.PermUsersRead)
	cases := []struct {
		method, path string
		body         io.Reader
		contentType  string
	}{
		{http.MethodGet, "/api/users/bulk/template", nil, ""},
		{http.MethodPost, "/api/users/bulk/import", strings.NewReader("x"), "multipart/form-data; boundary=b"},
		{http.MethodPost, "/api/users/bulk/generate", strings.NewReader(`{"count":1,"customer_type":"general","group":"default"}`), "application/json"},
	}
	for _, tc := range cases {
		rr := env.request(readOnly, tc.method, tc.path, tc.contentType, tc.body)
		if rr.Code != http.StatusForbidden {
			t.Fatalf("%s %s 状态 = %d，期望 403", tc.method, tc.path, rr.Code)
		}
	}
}

// ---------------------------------------------------------------------------
// 导入
// ---------------------------------------------------------------------------

func TestBulkImportCreatesUsers(t *testing.T) {
	repo := newFakeBulkUserRepo()
	env := newBulkTestEnv(t, repo)
	body := "username,password,customer_type,group\nalice,longpassword123,enterprise,default\n"
	rr := env.importRequest(limitedAdmin(rbac.PermUsersCreate), body, "users.csv", false)
	if rr.Code != http.StatusCreated {
		t.Fatalf("状态 = %d，期望 201（body=%q）", rr.Code, rr.Body.String())
	}
	var result struct {
		Created int `json:"created"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil || result.Created != 1 {
		t.Fatalf("响应 = %q，期望 {\"created\":1}", rr.Body.String())
	}
	if len(repo.capturedRows) != 1 {
		t.Fatalf("插入行数 = %d", len(repo.capturedRows))
	}
	row := repo.capturedRows[0]
	if row.Username != "alice" || row.CustomerType != "enterprise" || row.Group != "default" {
		t.Fatalf("插入行 = %+v", row)
	}
	if !auth.VerifyPassword(row.PasswordHash, "longpassword123") {
		t.Fatal("PasswordHash 与 CSV 口令不匹配")
	}
	if repo.capturedOpts.MustChangePassword {
		t.Fatal("must_change_password 未传 false")
	}
	// 审计 detail 只允许计数与来源，不得含口令/用户名列表。
	auditArgs := env.audit.concatenatedArgs()
	if !strings.Contains(auditArgs, `"source":"csv"`) || !strings.Contains(auditArgs, `"count":1`) {
		t.Fatalf("审计缺少来源/计数：%s", auditArgs)
	}
	if strings.Contains(auditArgs, "longpassword123") || strings.Contains(auditArgs, "alice") {
		t.Fatalf("审计含口令/用户名：%s", auditArgs)
	}
}

func TestBulkImportRejectsInvalidRowsWithoutInsert(t *testing.T) {
	repo := newFakeBulkUserRepo()
	env := newBulkTestEnv(t, repo)
	body := "username,password,customer_type,group\nalice,short,general,default\nbob,longpassword123,general,default\n"
	rr := env.importRequest(limitedAdmin(rbac.PermUsersCreate), body, "users.csv", false)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("状态 = %d，期望 422（body=%q）", rr.Code, rr.Body.String())
	}
	list := decodeBulkErrors(t, rr)
	if len(list.Errors) != 1 || list.Errors[0].Code != "password_invalid" || list.Errors[0].Line != 2 {
		t.Fatalf("行错误 = %+v", list.Errors)
	}
	if repo.createCalls != 0 {
		t.Fatalf("校验失败仍调用 BulkCreateUsers %d 次", repo.createCalls)
	}
}

func TestBulkImportRejectsLegacyColumns(t *testing.T) {
	repo := newFakeBulkUserRepo()
	env := newBulkTestEnv(t, repo)
	// role 列是权限边界：csv 一旦能指定 role，users:create 就能造管理员。
	body := "username,password,customer_type,group,role\nalice,longpassword123,general,default,admin\n"
	rr := env.importRequest(limitedAdmin(rbac.PermUsersCreate), body, "users.csv", false)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("状态 = %d，期望 400（header_invalid）", rr.Code)
	}
	list := decodeBulkErrors(t, rr)
	if len(list.Errors) != 1 || list.Errors[0].Code != "header_invalid" {
		t.Fatalf("行错误 = %+v", list.Errors)
	}
	if repo.createCalls != 0 {
		t.Fatal("header 非法仍插入")
	}
}

func TestBulkImportWritesMarkersBeforeCommit(t *testing.T) {
	repo := newFakeBulkUserRepo()
	env := newBulkTestEnv(t, repo)
	repo.afterInsertFun = func(_ []store.BulkUserRow, afterInsert func(tx *sql.Tx, ids []int64) error) ([]store.BulkUserRow, error) {
		if err := afterInsert(nil, []int64{41, 42}); err != nil {
			t.Fatalf("afterInsert: %v", err)
		}
		// 提交前标记必须已存在（否则提交成功后帐号永远登录不上）。
		for _, id := range []int64{41, 42} {
			if !env.mr.Exists(tempPasswordKeyPrefix + fmt.Sprint(id)) {
				t.Fatalf("提交前缺少 Redis 标记 id=%d", id)
			}
		}
		return nil, nil
	}
	body := "username,password,customer_type,group\nalice,longpassword123,general,default\n"
	rr := env.importRequest(limitedAdmin(rbac.PermUsersCreate), body, "users.csv", true)
	if rr.Code != http.StatusCreated {
		t.Fatalf("状态 = %d，期望 201（body=%q）", rr.Code, rr.Body.String())
	}
	// 无 TTL：临时口令标记不能自行过期（DB 列仍是 true 时过期即永久 401）。
	for _, id := range []int64{41, 42} {
		if got := env.mr.TTL(tempPasswordKeyPrefix + fmt.Sprint(id)); got != 0 {
			t.Fatalf("标记 id=%d TTL = %v，期望 0（无过期）", id, got)
		}
	}
}

func TestBulkImportRemovesMarkersWhenCommitFails(t *testing.T) {
	repo := newFakeBulkUserRepo()
	env := newBulkTestEnv(t, repo)
	repo.afterInsertFun = func(_ []store.BulkUserRow, afterInsert func(tx *sql.Tx, ids []int64) error) ([]store.BulkUserRow, error) {
		if err := afterInsert(nil, []int64{41, 42}); err != nil {
			t.Fatalf("afterInsert: %v", err)
		}
		return nil, errors.New("commit failed")
	}
	body := "username,password,customer_type,group\nalice,longpassword123,general,default\n"
	rr := env.importRequest(limitedAdmin(rbac.PermUsersCreate), body, "users.csv", true)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("状态 = %d，期望 500（body=%q）", rr.Code, rr.Body.String())
	}
	if keys := env.mr.Keys(); len(keys) != 0 {
		t.Fatalf("提交失败后残留 Redis 标记：%v", keys)
	}
}

// ---------------------------------------------------------------------------
// 生成
// ---------------------------------------------------------------------------

func TestBulkGenerateKeepsPasswordOrder(t *testing.T) {
	repo := newFakeBulkUserRepo()
	env := newBulkTestEnv(t, repo)
	rr := env.request(limitedAdmin(rbac.PermUsersCreate), http.MethodPost, "/api/users/bulk/generate",
		"application/json", strings.NewReader(`{"count":3,"customer_type":"enterprise","group":"default"}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("状态 = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	for header, want := range map[string]string{
		"Content-Type":           "text/csv; charset=utf-8",
		"Cache-Control":          "no-store",
		"X-Content-Type-Options": "nosniff",
	} {
		if got := rr.Header().Get(header); got != want {
			t.Fatalf("%s = %q，期望 %q", header, got, want)
		}
	}
	if got := rr.Header().Get("Content-Disposition"); !strings.Contains(got, "newtg-users-") || !strings.HasSuffix(got, ".csv\"") {
		t.Fatalf("Content-Disposition = %q", got)
	}

	records, err := csv.NewReader(bytes.NewReader(rr.Body.Bytes())).ReadAll()
	if err != nil {
		t.Fatalf("读取生成 CSV: %v", err)
	}
	if len(records) != 4 {
		t.Fatalf("记录数 = %d，期望表头 + 3 行", len(records))
	}
	if len(repo.capturedRows) != 3 {
		t.Fatalf("插入行数 = %d", len(repo.capturedRows))
	}
	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		username, password := records[i+1][0], records[i+1][1]
		if username != fmt.Sprintf("NewTG_user%04d", repo.seq-2+int64(i)) {
			t.Fatalf("第 %d 行 username = %q，期望顺序分配", i, username)
		}
		// 修正 8：明文口令必须与同一输入序号的哈希配对。
		if !auth.VerifyPassword(repo.capturedRows[i].PasswordHash, password) {
			t.Fatalf("第 %d 行口令与哈希错位（username=%s）", i, username)
		}
		if seen[password] {
			t.Fatalf("口令重复：%s", password)
		}
		seen[password] = true
		if records[i+1][2] != "enterprise" || records[i+1][3] != "default" {
			t.Fatalf("第 %d 行 customer_type/group 列错误：%v", i, records[i+1])
		}
	}
	if !repo.capturedOpts.AssignSequential || !repo.capturedOpts.MustChangePassword {
		t.Fatalf("生成选项 = %+v，期望 AssignSequential+MustChangePassword", repo.capturedOpts)
	}
	for i := 0; i < 3; i++ {
		key := tempPasswordKeyPrefix + fmt.Sprint(int64(1000+i))
		if !env.mr.Exists(key) {
			t.Fatalf("缺少标记 %s", key)
		}
		if ttl := env.mr.TTL(key); ttl != 0 {
			t.Fatalf("标记 %s TTL = %v，期望无 TTL", key, ttl)
		}
	}
}

func TestBulkGenerateValidation(t *testing.T) {
	repo := newFakeBulkUserRepo()
	repo.existingGroups = map[string]bool{"default": true}
	env := newBulkTestEnv(t, repo)
	cases := []struct {
		name string
		body string
	}{
		{"count out of range", `{"count":0,"customer_type":"general","group":"default"}`},
		{"count too large", `{"count":501,"customer_type":"general","group":"default"}`},
		{"invalid customer type", `{"count":1,"customer_type":"platinum","group":"default"}`},
		{"unknown group", `{"count":1,"customer_type":"general","group":"ghost"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := env.request(limitedAdmin(rbac.PermUsersCreate), http.MethodPost, "/api/users/bulk/generate",
				"application/json", strings.NewReader(tc.body))
			if rr.Code != http.StatusUnprocessableEntity && rr.Code != http.StatusBadRequest {
				t.Fatalf("状态 = %d，期望 422/400（body=%q）", rr.Code, rr.Body.String())
			}
			if repo.createCalls != 0 {
				t.Fatalf("非法请求仍插入 %d 次", repo.createCalls)
			}
		})
	}
}

func TestBulkGenerateAuditExcludesCredentials(t *testing.T) {
	repo := newFakeBulkUserRepo()
	env := newBulkTestEnv(t, repo)
	rr := env.request(limitedAdmin(rbac.PermUsersCreate), http.MethodPost, "/api/users/bulk/generate",
		"application/json", strings.NewReader(`{"count":2,"customer_type":"general","group":"default"}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("状态 = %d", rr.Code)
	}
	auditArgs := env.audit.concatenatedArgs()
	if !strings.Contains(auditArgs, `"source":"generated"`) || !strings.Contains(auditArgs, `"count":2`) {
		t.Fatalf("审计缺少来源/计数：%s", auditArgs)
	}
	records, err := csv.NewReader(bytes.NewReader(rr.Body.Bytes())).ReadAll()
	if err != nil {
		t.Fatalf("读取生成 CSV: %v", err)
	}
	for _, record := range records[1:] {
		if strings.Contains(auditArgs, record[1]) {
			t.Fatalf("审计含明文口令：%s", auditArgs)
		}
		if strings.Contains(auditArgs, record[0]) {
			t.Fatalf("审计含用户名：%s", auditArgs)
		}
	}
}
