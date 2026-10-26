// 批量建用户端点（plan 2026-10-26 Task 9）。
//
// 端点面（全部要求 users:create）：
//
//	GET  /api/users/bulk/template   CSV 模板（仅表头）
//	POST /api/users/bulk/import     multipart/form-data 导入
//	POST /api/users/bulk/generate   系统生成账号并返回一次性 CSV
//
// 安全口径：
//   - CSV 只接受 username,password,customer_type,group 四列，顺序任意。没有
//     email/role/display_name/remark 列——这不是省略而是权限边界：users:create
//     不得通过批量通道铸造 admin（单条创建要额外过 AuthorizeGrant）；
//   - 导入端点走 HandlerPermissionOnly：readJSONObject 无法解析 multipart，
//     请求体由本文件严格解析；
//   - 生成端点仍是普通 Handler，字段白名单 count/customer_type/group；
//   - 任何错误响应都是全有或全无：有任意一行不合法就一行都不插；
//   - 生成结果含明文口令，响应禁止缓存（Cache-Control: no-store），也绝不套
//     idempotentCreate（那会把明文口令缓存进 Redis）；
//   - 审计 detail 只放 {count, source, filename?, customer_type?, group?}，
//     任何情况下不含口令或用户名列表。
package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/rbac"
	"newtg/gateway/internal/store"
)

const (
	// bulkCSVMaxRows 是单次导入/生成的行数上限；超过只报一条 row_limit。
	bulkCSVMaxRows = 500

	// bulkUploadMaxBytes 是 CSV 文件本体上限；multipart 头部另有多余开销。
	bulkUploadMaxBytes = 1 << 20

	// bulkMultipartOverhead 为 multipart 边界/C-A 头预留的空间（同
	// handlers_task_plugins.go 的 multipartOverhead 口径）。
	bulkMultipartOverhead = 64 << 10

	// bulkHashConcurrency 是 argon2id 并发的上限：每次哈希约 19 MiB 内存，
	// 4 路是安全上限（见计划 task-9-corrections 修正 6/已核实参数）。
	bulkHashConcurrency = 4
)

// bulkCSVHeader 是冻结的四列 CSV 表头（导入时列顺序可任意）。
var bulkCSVHeader = []string{"username", "password", "customer_type", "group"}

// 批量端点的字段级动作登记。导入不使用字段层（multipart），但保留独立动作名
// 便于审计与未来的路由级统计。
var (
	bulkTemplateAction = rbac.AuthzAction{ResourceKind: "user", ActionName: "bulk_template"}
	bulkImportAction   = rbac.AuthzAction{ResourceKind: "user", ActionName: "bulk_import"}
	bulkGenerateAction = rbac.AuthzAction{ResourceKind: "user", ActionName: "bulk_generate"}
)

// errBulkMarkersUnavailable 表示提交前的临时口令标记写入失败。事务已回滚，
// 没有用户落库；HTTP 映射为 503（Redis 故障属于暂时不可用，不是输入错误）。
var errBulkMarkersUnavailable = errors.New("httpapi: write temp password markers")

// BulkUserRepo 是批量建用户端点消费的仓储能力（store.BulkUserRepo 实现）。
type BulkUserRepo interface {
	ExistingUsernames(ctx context.Context, usernames []string) (map[string]bool, error)
	ExistingGroups(ctx context.Context, names []string) (map[string]bool, error)
	BulkCreateUsers(ctx context.Context, rows []store.BulkUserRow, opts store.BulkCreateOptions,
		afterInsert func(tx *sql.Tx, ids []int64) error) ([]store.BulkUserRow, error)
}

// BulkUserHandlerConfig 是批量端点的装配输入。Repo 与 DB 二选一（Repo 优先）。
// Redis 用于临时口令标记；为 nil 时导入/生成在需要标记的路径上返回 503。
type BulkUserHandlerConfig struct {
	DB      *sql.DB
	Repo    BulkUserRepo
	Redis   *redis.Client
	Authz   *rbac.AuthzMiddleware
	Checker auth.CompromisedPasswordChecker
	Audit   *audit.Logger
	Logger  *slog.Logger
}

// BulkUserHandlers 持有批量端点依赖，可并发使用。
type BulkUserHandlers struct {
	repo    BulkUserRepo
	redis   *redis.Client
	authz   *rbac.AuthzMiddleware
	checker auth.CompromisedPasswordChecker
	audit   *audit.Logger
	log     *slog.Logger
}

// NewBulkUserHandlers 构造批量端点。
func NewBulkUserHandlers(cfg BulkUserHandlerConfig) (*BulkUserHandlers, error) {
	repo := cfg.Repo
	if repo == nil {
		if cfg.DB == nil {
			return nil, errors.New("httpapi: BulkUserHandlerConfig.Repo or DB is required")
		}
		repo = store.NewBulkUserRepo(cfg.DB)
	}
	authz := cfg.Authz
	if authz == nil {
		authz = rbac.NewAuthzMiddleware()
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	h := &BulkUserHandlers{
		repo: repo, redis: cfg.Redis, authz: authz, checker: cfg.Checker,
		audit: cfg.Audit, log: log,
	}
	h.registerPolicies()
	return h, nil
}

func (h *BulkUserHandlers) registerPolicies() {
	// 生成端点是 JSON 写请求：必须登记白名单，否则中间件对未登记动作
	// fail-closed。导入端点用 HandlerPermissionOnly，不读取 body。
	h.authz.SetWritePolicy(bulkGenerateAction, &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{
		"count":         nil,
		"customer_type": nil,
		"group":         nil,
	}})
}

// RegisterBulkUserRoutes 注册三个批量端点。
func (h *BulkUserHandlers) RegisterBulkUserRoutes(mux *http.ServeMux, wrap func(http.Handler) http.Handler) {
	mount := func(pattern string, handler http.Handler) {
		if wrap != nil {
			handler = wrap(handler)
		}
		mux.Handle(pattern, handler)
	}
	fixed := func(required string, action rbac.AuthzAction, next http.HandlerFunc) http.Handler {
		return h.authz.Handler(required, action)(next)
	}
	permissionOnly := func(required string, action rbac.AuthzAction, next http.HandlerFunc) http.Handler {
		return h.authz.HandlerPermissionOnly(required, action)(next)
	}

	mount("GET /api/users/bulk/template",
		fixed(rbac.PermUsersCreate, bulkTemplateAction, h.handleBulkTemplate))
	mount("POST /api/users/bulk/import",
		permissionOnly(rbac.PermUsersCreate, bulkImportAction, h.handleBulkImport))
	mount("POST /api/users/bulk/generate",
		fixed(rbac.PermUsersCreate, bulkGenerateAction, h.handleBulkGenerate))
}

// ---------------------------------------------------------------------------
// CSV 解析 / 校验 / 生成（纯函数）
// ---------------------------------------------------------------------------

// bulkCSVRow 是一行解析后的导入数据。Line 是 CSV 物理行号（表头为 1）。
type bulkCSVRow struct {
	Line         int
	Username     string
	Password     string
	CustomerType string
	Group        string
}

// bulkRowError 是 422 响应中的行级错误（对应 OpenAPI BulkRowError）。
type bulkRowError struct {
	Line     int    `json:"line"`
	Field    string `json:"field"`
	Code     string `json:"code"`
	Message  string `json:"message"`
	Username string `json:"username,omitempty"`
}

// bulkCSVOutputRow 是生成端点的 CSV 输出行（含一次性明文口令）。
type bulkCSVOutputRow struct {
	Username     string
	Password     string
	CustomerType string
	Group        string
}

// parseBulkCSV 解析导入文件。表头必须是且仅是四列（顺序任意），BOM/CRLF/
// 单元格空格都被规范化。任何文件级错误（缺列/多列/重复列/行数超限）都只
// 返回一条错误，不再逐行分析。
func parseBulkCSV(raw []byte) ([]bulkCSVRow, []bulkRowError) {
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})
	reader := csv.NewReader(bytes.NewReader(raw))
	reader.FieldsPerRecord = -1
	reader.TrimLeadingSpace = true

	header, err := reader.Read()
	if err != nil {
		return nil, []bulkRowError{bulkHeaderError("CSV is empty or has no header row")}
	}
	index := make(map[string]int, len(bulkCSVHeader))
	for i, name := range header {
		name = strings.TrimSpace(name)
		if !isBulkCSVColumn(name) {
			return nil, []bulkRowError{bulkHeaderError("CSV header contains an unknown column: " + name)}
		}
		if _, duplicate := index[name]; duplicate {
			return nil, []bulkRowError{bulkHeaderError("CSV header contains a duplicate column: " + name)}
		}
		index[name] = i
	}
	if len(index) != len(bulkCSVHeader) {
		return nil, []bulkRowError{bulkHeaderError("CSV header must contain exactly username,password,customer_type,group")}
	}

	rows := make([]bulkCSVRow, 0, 16)
	for {
		record, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, []bulkRowError{bulkHeaderError("CSV row is malformed")}
		}
		line, _ := reader.FieldPos(0)
		if len(record) != len(bulkCSVHeader) {
			return nil, []bulkRowError{{
				Line: line, Field: "row", Code: "header_invalid",
				Message: "CSV row must contain exactly four fields",
			}}
		}
		if len(rows) >= bulkCSVMaxRows {
			return nil, []bulkRowError{{
				Line: line, Field: "file", Code: "row_limit",
				Message: fmt.Sprintf("CSV import is limited to %d rows", bulkCSVMaxRows),
			}}
		}
		rows = append(rows, bulkCSVRow{
			Line:         line,
			Username:     strings.TrimSpace(record[index["username"]]),
			Password:     strings.TrimSpace(record[index["password"]]),
			CustomerType: strings.TrimSpace(record[index["customer_type"]]),
			Group:        strings.TrimSpace(record[index["group"]]),
		})
	}
	return rows, nil
}

func isBulkCSVColumn(name string) bool {
	for _, column := range bulkCSVHeader {
		if name == column {
			return true
		}
	}
	return false
}

func bulkHeaderError(message string) bulkRowError {
	return bulkRowError{Line: 1, Field: "header", Code: "header_invalid", Message: message}
}

// validateBulkRows 逐行校验并把空值规范化为默认值（customer_type=general、
// group=default）。任意一行有错都返回错误，调用方必须放弃整批插入。
func validateBulkRows(
	rows []bulkCSVRow, existingUsers, existingGroups map[string]bool,
	checker auth.CompromisedPasswordChecker,
) []bulkRowError {
	if len(rows) > bulkCSVMaxRows {
		line := 1
		if len(rows) > bulkCSVMaxRows {
			line = rows[bulkCSVMaxRows].Line
		}
		return []bulkRowError{{
			Line: line, Field: "file", Code: "row_limit",
			Message: fmt.Sprintf("CSV import is limited to %d rows", bulkCSVMaxRows),
		}}
	}

	usernameCounts := make(map[string]int, len(rows))
	for _, row := range rows {
		if usernamePattern.MatchString(row.Username) {
			usernameCounts[row.Username]++
		}
	}

	var errs []bulkRowError
	for i := range rows {
		row := &rows[i]
		usernameValid := usernamePattern.MatchString(row.Username)
		switch {
		case !usernameValid:
			errs = append(errs, bulkRowViolation(row, "username", "username_invalid",
				"username must be 3-32 characters of [a-zA-Z0-9_-]"))
		case usernameCounts[row.Username] > 1:
			errs = append(errs, bulkRowViolation(row, "username", "username_duplicate",
				"username appears more than once in the file"))
		case existingUsers[row.Username]:
			errs = append(errs, bulkRowViolation(row, "username", "username_duplicate",
				"username already exists"))
		}

		if !auth.ValidatePasswordLength(row.Password) || checker.CheckWithContext(row.Password, row.Username) {
			errs = append(errs, bulkRowViolation(row, "password", "password_invalid",
				fmt.Sprintf("password must be %d-%d characters and must not be weak or breached", auth.PWDMinLen, auth.PWDMaxLen)))
		}

		customerType := row.CustomerType
		if customerType == "" {
			customerType = customerTypeGeneral
		} else if !validCustomerType(customerType) {
			errs = append(errs, bulkRowViolation(row, "customer_type", "customer_type_invalid",
				"customer_type must be general or enterprise"))
		}
		row.CustomerType = customerType

		group := row.Group
		if group == "" {
			group = "default"
		}
		if !existingGroups[group] {
			errs = append(errs, bulkRowViolation(row, "group", "group_not_found",
				"group does not exist"))
		}
		row.Group = group
	}
	return errs
}

func bulkRowViolation(row *bulkCSVRow, field, code, message string) bulkRowError {
	return bulkRowError{Line: row.Line, Field: field, Code: code, Message: message, Username: row.Username}
}

// formatBulkCSV 输出冻结的 CSV（表头 + 行）。
//
// 已知取舍：导入口令由管理员自填，可能以 =/+/-/@ 开头。CSV 公式注入的常规
// 防护是加前缀转义，但那样下载后再导入会改变口令语义（用户拿到的就不是
// 管理员设置的口令）。这里的用户名受字符集约束、口令是管理员主动设置的
// 值，且文件只经管理员浏览器下载，因此**不转义**——可用性优先。
func formatBulkCSV(rows []bulkCSVOutputRow) ([]byte, error) {
	var buffer bytes.Buffer
	writer := csv.NewWriter(&buffer)
	if err := writer.Write(bulkCSVHeader); err != nil {
		return nil, err
	}
	for _, row := range rows {
		if err := writer.Write([]string{row.Username, row.Password, row.CustomerType, row.Group}); err != nil {
			return nil, err
		}
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

// generateBulkPassword 生成 16 位 CSPRNG 口令（复用 tempPasswordAlphabet，
// 排除 I/O/0/1）。生成的口令是随机高熵值，不需要再过泄露检查（修正 6）。
func generateBulkPassword() (string, error) {
	out := make([]byte, tempPasswordLength)
	upper := big.NewInt(int64(len(tempPasswordAlphabet)))
	for i := range out {
		n, err := rand.Int(rand.Reader, upper)
		if err != nil {
			return "", fmt.Errorf("httpapi: generate bulk password: %w", err)
		}
		out[i] = tempPasswordAlphabet[n.Int64()]
	}
	return string(out), nil
}

// hashBulkPasswords 以固定并发上限计算 argon2id 哈希；任一行失败即返回错误，
// 调用方不得插入任何用户。
func hashBulkPasswords(passwords []string, concurrency int) ([]string, error) {
	hashes := make([]string, len(passwords))
	if len(passwords) == 0 {
		return hashes, nil
	}
	if concurrency < 1 {
		concurrency = 1
	}
	semaphore := make(chan struct{}, concurrency)
	var (
		wait     sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	for i := range passwords {
		wait.Add(1)
		semaphore <- struct{}{}
		go func(i int) {
			defer wait.Done()
			defer func() { <-semaphore }()
			hash, err := auth.HashPassword(passwords[i])
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
				return
			}
			hashes[i] = hash
		}(i)
	}
	wait.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return hashes, nil
}

// ---------------------------------------------------------------------------
// 端点
// ---------------------------------------------------------------------------

func (h *BulkUserHandlers) handleBulkTemplate(w http.ResponseWriter, _ *http.Request) {
	body, err := formatBulkCSV(nil)
	if err != nil {
		writeAdminProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error", "")
		return
	}
	h.writeCSVAttachment(w, "newtg-users-template.csv", body, http.StatusOK)
}

func (h *BulkUserHandlers) handleBulkImport(w http.ResponseWriter, r *http.Request) {
	p, ok := adminPrincipal(w, r)
	if !ok {
		return
	}
	data, filename, mustChange, ok := h.readBulkUpload(w, r)
	if !ok {
		return
	}

	rows, parseErrs := parseBulkCSV(data)
	if len(parseErrs) > 0 {
		// 修正 7：表头/结构错误是 400 header_invalid；行数超限与行级校验
		// 是 422（语义不可处理）。两者都用 BulkRowError 列表承载。
		status := http.StatusUnprocessableEntity
		if parseErrs[0].Code == "header_invalid" {
			status = http.StatusBadRequest
		}
		writeBulkRowErrors(w, status, parseErrs)
		return
	}
	if len(rows) == 0 {
		writeBulkRowErrors(w, http.StatusBadRequest, []bulkRowError{bulkHeaderError("CSV contains no data rows")})
		return
	}

	usernames := make([]string, len(rows))
	groupNames := make([]string, 0, len(rows))
	seenGroup := make(map[string]struct{}, len(rows))
	for i, row := range rows {
		usernames[i] = row.Username
		group := row.Group
		if group == "" {
			group = "default"
		}
		if _, seen := seenGroup[group]; !seen {
			seenGroup[group] = struct{}{}
			groupNames = append(groupNames, group)
		}
	}

	existingUsers, err := h.repo.ExistingUsernames(r.Context(), usernames)
	if err != nil {
		h.log.Error("bulk import: check usernames failed", "error", err)
		writeAdminProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error", "")
		return
	}
	existingGroups, err := h.repo.ExistingGroups(r.Context(), groupNames)
	if err != nil {
		h.log.Error("bulk import: check groups failed", "error", err)
		writeAdminProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error", "")
		return
	}
	if rowErrs := validateBulkRows(rows, existingUsers, existingGroups, h.checker); len(rowErrs) > 0 {
		writeBulkRowErrors(w, http.StatusUnprocessableEntity, rowErrs)
		return
	}
	if mustChange && h.redis == nil {
		h.writeBulkServiceUnavailable(w, "temp password markers require Redis")
		return
	}

	passwords := make([]string, len(rows))
	for i := range rows {
		passwords[i] = rows[i].Password
	}
	hashes, err := hashBulkPasswords(passwords, bulkHashConcurrency)
	if err != nil {
		h.log.Error("bulk import: hash passwords failed", "error", err)
		writeAdminProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error", "")
		return
	}
	storeRows := make([]store.BulkUserRow, len(rows))
	for i := range rows {
		storeRows[i] = store.BulkUserRow{
			Username: rows[i].Username, PasswordHash: hashes[i],
			CustomerType: rows[i].CustomerType, Group: rows[i].Group,
		}
	}

	_, insertedIDs, err := h.bulkCreate(r.Context(), storeRows,
		store.BulkCreateOptions{MustChangePassword: mustChange})
	if err != nil {
		h.cleanupMarkers(r.Context(), insertedIDs, mustChange)
		h.writeBulkRepoError(w, err)
		return
	}

	h.auditBulk(r, p.UserID, map[string]any{
		"count": len(storeRows), "source": "csv", "filename": filename,
	}, http.StatusCreated)
	writeJSON(w, http.StatusCreated, map[string]int{"created": len(storeRows)})
}

func (h *BulkUserHandlers) handleBulkGenerate(w http.ResponseWriter, r *http.Request) {
	p, ok := adminPrincipal(w, r)
	if !ok {
		return
	}
	var req struct {
		Count        int     `json:"count"`
		CustomerType *string `json:"customer_type"`
		Group        *string `json:"group"`
	}
	if err := readAuthJSON(r, &req); err != nil {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"request body must be a JSON object")
		return
	}
	fields := make([]string, 0, 3)
	if req.Count < 1 || req.Count > bulkCSVMaxRows {
		fields = append(fields, "count")
	}
	customerType := customerTypeGeneral
	if req.CustomerType == nil || !validCustomerType(strings.TrimSpace(*req.CustomerType)) {
		fields = append(fields, "customer_type")
	} else {
		customerType = strings.TrimSpace(*req.CustomerType)
	}
	group := ""
	if req.Group == nil || strings.TrimSpace(*req.Group) == "" {
		fields = append(fields, "group")
	} else {
		group = strings.TrimSpace(*req.Group)
	}
	if len(fields) > 0 {
		writeValidationProblem(w, fields)
		return
	}
	existingGroups, err := h.repo.ExistingGroups(r.Context(), []string{group})
	if err != nil {
		h.log.Error("bulk generate: check group failed", "error", err)
		writeAdminProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error", "")
		return
	}
	if !existingGroups[group] {
		writeValidationProblem(w, []string{"group"})
		return
	}
	if h.redis == nil {
		h.writeBulkServiceUnavailable(w, "temp password markers require Redis")
		return
	}

	passwords := make([]string, req.Count)
	for i := range passwords {
		password, err := generateBulkPassword()
		if err != nil {
			h.log.Error("bulk generate: generate password failed", "error", err)
			writeAdminProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error", "")
			return
		}
		passwords[i] = password
	}
	hashes, err := hashBulkPasswords(passwords, bulkHashConcurrency)
	if err != nil {
		h.log.Error("bulk generate: hash passwords failed", "error", err)
		writeAdminProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error", "")
		return
	}
	storeRows := make([]store.BulkUserRow, req.Count)
	for i := range storeRows {
		storeRows[i] = store.BulkUserRow{
			PasswordHash: hashes[i], CustomerType: customerType, Group: group,
		}
	}

	created, insertedIDs, err := h.bulkCreate(r.Context(), storeRows, store.BulkCreateOptions{
		MustChangePassword: true,
		AssignSequential:   true,
	})
	if err != nil {
		h.cleanupMarkers(r.Context(), insertedIDs, true)
		h.writeBulkRepoError(w, err)
		return
	}
	if len(created) != len(passwords) {
		// 行数与口令数必须一致；不一致时宁可不返回任何凭据。
		h.log.Error("bulk generate: row/password count mismatch", "rows", len(created), "passwords", len(passwords))
		h.cleanupMarkers(r.Context(), insertedIDs, true)
		writeAdminProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error", "")
		return
	}
	output := make([]bulkCSVOutputRow, len(created))
	for i := range created {
		// 修正 8：按输入顺序配对重写后的 username 与明文口令。
		output[i] = bulkCSVOutputRow{
			Username: created[i].Username, Password: passwords[i],
			CustomerType: created[i].CustomerType, Group: created[i].Group,
		}
	}
	body, err := formatBulkCSV(output)
	if err != nil {
		h.log.Error("bulk generate: format csv failed", "error", err)
		writeAdminProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error", "")
		return
	}

	h.auditBulk(r, p.UserID, map[string]any{
		"count": len(created), "source": "generated",
		"customer_type": customerType, "group": group,
	}, http.StatusOK)
	filename := "newtg-users-" + time.Now().UTC().Format("20060102T150405Z") + ".csv"
	h.writeCSVAttachment(w, filename, body, http.StatusOK)
}

// bulkCreate 执行仓储插入，并在 afterInsert 中（提交前）写入临时口令标记。
// 同时返回 afterInsert 记录的 ids 与仓储重写后的 rows；前者供提交失败后的
// best-effort 清理，后者供生成端点按输入顺序配对明文口令。
func (h *BulkUserHandlers) bulkCreate(
	ctx context.Context, rows []store.BulkUserRow, opts store.BulkCreateOptions,
) (created []store.BulkUserRow, insertedIDs []int64, err error) {
	created, err = h.repo.BulkCreateUsers(ctx, rows, opts, func(_ *sql.Tx, ids []int64) error {
		insertedIDs = append([]int64(nil), ids...)
		if !opts.MustChangePassword {
			return nil
		}
		if err := h.setTempPasswordMarkers(ctx, ids); err != nil {
			return fmt.Errorf("%w: %v", errBulkMarkersUnavailable, err)
		}
		return nil
	})
	return created, insertedIDs, err
}

// setTempPasswordMarkers 用 TxPipeline（MULTI/EXEC）原子写无 TTL 标记。
// 无 TTL 是硬要求：DB 的 must_change_password=true 而标记缺失会让账号永远
// 登录不上（401 temp_password_expired），而不是「没有强制改密」。
func (h *BulkUserHandlers) setTempPasswordMarkers(ctx context.Context, ids []int64) error {
	if h.redis == nil {
		return errors.New("httpapi: Redis is not configured")
	}
	pipe := h.redis.TxPipeline()
	for _, id := range ids {
		pipe.Set(ctx, tempPasswordKeyPrefix+strconv.FormatInt(id, 10), "1", 0)
	}
	_, err := pipe.Exec(ctx)
	return err
}

// cleanupMarkers 在事务未提交时删除已写入的标记，避免留下「没有对应账号」
// 或「未提交账号」的孤立凭据标记。
func (h *BulkUserHandlers) cleanupMarkers(ctx context.Context, ids []int64, mustChange bool) {
	if !mustChange || h.redis == nil || len(ids) == 0 {
		return
	}
	keys := make([]string, len(ids))
	for i, id := range ids {
		keys[i] = tempPasswordKeyPrefix + strconv.FormatInt(id, 10)
	}
	// 提交失败时客户端可能已断开：清理不能因为请求上下文取消而跳过，否则
	// 会留下没有对应账号的孤立标记。
	cleanupCtx := context.WithoutCancel(ctx)
	if err := h.redis.Del(cleanupCtx, keys...).Err(); err != nil {
		h.log.Warn("bulk create: cleanup temp password markers failed", "error", err)
	}
}

// readBulkUpload 按 handlers_task_plugins.go 的样板解析 multipart：禁用
// r.ParseMultipartForm，先用 MaxBytesReader 读全量，再用 multipart.NewReader
// 逐段读取，文件段做二次体积校验。
func (h *BulkUserHandlers) readBulkUpload(
	w http.ResponseWriter, r *http.Request,
) (data []byte, filename string, mustChange bool, ok bool) {
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"body must be multipart/form-data")
		return nil, "", false, false
	}

	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, bulkUploadMaxBytes+bulkMultipartOverhead))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeAdminProblem(w, http.StatusRequestEntityTooLarge, authProblemBadRequest,
				"File too large", "CSV upload exceeds 1 MiB")
			return nil, "", false, false
		}
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"unable to read multipart body")
		return nil, "", false, false
	}

	reader := multipart.NewReader(bytes.NewReader(raw), params["boundary"])
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
				"malformed multipart body")
			return nil, "", false, false
		}
		switch part.FormName() {
		case "file":
			content, readErr := io.ReadAll(io.LimitReader(part, bulkUploadMaxBytes+1))
			_ = part.Close()
			if readErr != nil {
				writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
					"unable to read uploaded file")
				return nil, "", false, false
			}
			if len(content) > bulkUploadMaxBytes {
				writeAdminProblem(w, http.StatusRequestEntityTooLarge, authProblemBadRequest,
					"File too large", "CSV upload exceeds 1 MiB")
				return nil, "", false, false
			}
			data = content
			filename = part.FileName()
		case "must_change_password":
			value, readErr := io.ReadAll(io.LimitReader(part, 16))
			_ = part.Close()
			if readErr != nil {
				writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
					"unable to read must_change_password")
				return nil, "", false, false
			}
			text := strings.TrimSpace(string(value))
			mustChange = text == "true" || text == "1"
		default:
			_ = part.Close()
		}
	}
	if data == nil {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"multipart body must contain a file part")
		return nil, "", false, false
	}
	return data, filename, mustChange, true
}

func (h *BulkUserHandlers) writeCSVAttachment(w http.ResponseWriter, filename string, body []byte, status int) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	// 生成结果含一次性明文口令，任何中间层都不得缓存。
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeBulkRowErrors(w http.ResponseWriter, status int, errs []bulkRowError) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":   problemTypeValidationFailed,
		"title":  "Validation failed",
		"status": status,
		"errors": errs,
	})
}

func (h *BulkUserHandlers) writeBulkRepoError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrBulkDuplicateUsername):
		writeAdminProblem(w, http.StatusConflict, authProblemConflict, "Conflict",
			"one or more usernames already exist")
	case errors.Is(err, store.ErrBulkInvalidGroup):
		writeAdminProblem(w, http.StatusConflict, authProblemConflict, "Conflict",
			"a group referenced by the batch no longer exists")
	case errors.Is(err, store.ErrBulkSequenceExhausted):
		writeAdminProblem(w, http.StatusConflict, authProblemConflict, "Conflict",
			"the generated username sequence is exhausted")
	case errors.Is(err, errBulkMarkersUnavailable):
		h.writeBulkServiceUnavailable(w, "temporary password markers could not be written")
	default:
		h.log.Error("bulk create users failed", "error", err)
		writeAdminProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error", "")
	}
}

func (h *BulkUserHandlers) writeBulkServiceUnavailable(w http.ResponseWriter, detail string) {
	h.log.Error("bulk create users unavailable", "detail", detail)
	w.Header().Set("Retry-After", "5")
	writeAdminProblem(w, http.StatusServiceUnavailable, authProblemServiceUnavailable,
		"Service temporarily unavailable", "")
}

// auditBulk 写一条 user.bulk_create；detail 只含计数与来源维度，绝不包含
// 口令或用户名列表。
func (h *BulkUserHandlers) auditBulk(r *http.Request, actor int64, detail map[string]any, status int) {
	raw, err := json.Marshal(detail)
	if err != nil {
		h.log.Warn("bulk audit marshal failed", "error", err)
		return
	}
	if err := writeAuditEvent(r.Context(), h.audit, audit.Event{
		Action:     audit.ActionUserBulkCreate,
		Actor:      actor,
		IP:         tokenRemoteIP(r),
		UserAgent:  r.UserAgent(),
		Method:     r.Method,
		Route:      r.Pattern,
		StatusCode: status,
		Detail:     raw,
	}); err != nil {
		h.log.Warn("bulk audit write failed", "action", audit.ActionUserBulkCreate, "error", err)
	}
}
