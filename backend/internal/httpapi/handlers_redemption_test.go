package httpapi

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/billing"
	"newtg/gateway/internal/rbac"
)

// 本文件是 M5 Task 2 的 HTTP 端点验收（P-9/P-10/P-12）：
// 兑换码管理 CRUD 的 RBAC 403、创建响应只出现一次明文、列表固定掩码、
// 状态机/删除的 409，以及用户兑换的四个失败原因与 10 次/分钟限流。
//
// 需要真实 PostgreSQL（未设置 NEWTG_TEST_DB_DSN 时跳过）与 miniredis：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_test?sslmode=disable' \
//	go test ./internal/httpapi/ -run 'TestRedemption|TestRedeem' -v

const (
	redemptionTestMask = "••••••"
)

type redemptionConfigMap map[string]json.RawMessage

func (m redemptionConfigMap) Get(key string) (json.RawMessage, bool) {
	v, ok := m[key]
	return v, ok
}

type redemptionTestEnv struct {
	t      *testing.T
	db     *sql.DB
	gorm   *gorm.DB
	mr     *miniredis.Miniredis
	rdb    *redis.Client
	router http.Handler
	batch  string
}

func newRedemptionTestEnv(t *testing.T, cfg ConfigReader) *redemptionTestEnv {
	t.Helper()
	dsn := os.Getenv("NEWTG_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("NEWTG_TEST_DB_DSN 未设置，跳过兑换码集成测试")
	}
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	if err := sqlDB.Ping(); err != nil {
		_ = sqlDB.Close()
		t.Fatalf("ping test db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	gormDB, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		t.Fatalf("open gorm: %v", err)
	}

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })

	if cfg == nil {
		cfg = redemptionConfigMap{billing.ConfigQuotaPerUnit: json.RawMessage("100")}
	}
	h, err := NewRedemptionHandlers(RedemptionHandlerConfig{
		DB:     gormDB,
		Redis:  rdb,
		Config: cfg,
		Authz:  rbac.NewAuthzMiddleware(),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewRedemptionHandlers: %v", err)
	}
	mux := http.NewServeMux()
	h.RegisterRedemptionRoutes(mux, nil)

	// 每个环境使用独立批次名：共享测试库不会被其它用例/历史残留污染。
	batch := fmt.Sprintf("promo-%d", time.Now().UnixNano()%1_000_000_000)
	t.Cleanup(func() { _, _ = sqlDB.Exec(`DELETE FROM redemption_codes WHERE batch_name = $1`, batch) })

	return &redemptionTestEnv{t: t, db: sqlDB, gorm: gormDB, mr: mr, rdb: rdb, router: mux, batch: batch}
}

func (e *redemptionTestEnv) request(p *auth.Principal, method, path string, body any) *httptest.ResponseRecorder {
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

// createRedemptionTestUser 创建独立用户（默认分组）并在结束后清理。
func createRedemptionTestUser(t *testing.T, db *sql.DB, balance int64) int64 {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO user_groups (name, ratio, user_selectable, is_system, status)
		 VALUES ('default', 100, true, true, 1) ON CONFLICT (name) DO NOTHING`); err != nil {
		t.Fatalf("ensure default group: %v", err)
	}
	username := fmt.Sprintf("redeem%d", time.Now().UnixNano())
	var id int64
	if err := db.QueryRow(
		`INSERT INTO users (username, password_hash, balance, "group")
		 VALUES ($1, 'x', $2, 'default') RETURNING id`, username, balance).Scan(&id); err != nil {
		t.Fatalf("create test user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM wallet_transactions WHERE user_id = $1`, id)
		_, _ = db.Exec(`DELETE FROM users WHERE id = $1`, id)
	})
	return id
}

// insertExpiredRedemptionCode 直接落一行已过期码（管理 API 不允许创建过去时间）。
func (e *redemptionTestEnv) insertExpiredRedemptionCode(plain string, credits int64) {
	e.t.Helper()
	hash := billing.HashRedemptionCode(billing.NormalizeRedemptionCode(plain))
	if err := e.gorm.Exec(`INSERT INTO redemption_codes
		(code_hash, credits, status, batch_name, created_by, expires_at)
		VALUES (?, ?, 'unused', ?, 1, now() - interval '1 minute')`,
		hash, credits, e.batch).Error; err != nil {
		e.t.Fatalf("insert expired code: %v", err)
	}
}

func decodeRedemptionProblem(t *testing.T, rr *httptest.ResponseRecorder) struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail"`
} {
	t.Helper()
	var p struct {
		Type   string `json:"type"`
		Title  string `json:"title"`
		Status int    `json:"status"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &p); err != nil {
		t.Fatalf("problem JSON 非法: %v（body=%q）", err, rr.Body.String())
	}
	return p
}

type redemptionCreateResponse struct {
	BatchName *string  `json:"batch_name"`
	Credits   int64    `json:"credits"`
	Count     int      `json:"count"`
	Codes     []string `json:"codes"`
}

func createCodes(t *testing.T, env *redemptionTestEnv, p *auth.Principal, credits int64, count int) []string {
	t.Helper()
	rr := env.request(p, http.MethodPost, "/api/redemption-codes", map[string]any{
		"batch_name": env.batch,
		"credits":    credits,
		"count":      count,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201（body=%q）", rr.Code, rr.Body.String())
	}
	var resp redemptionCreateResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("create JSON: %v", err)
	}
	if len(resp.Codes) != count {
		t.Fatalf("codes = %d, want %d", len(resp.Codes), count)
	}
	for _, code := range resp.Codes {
		if len(code) != billing.RedemptionCodeLength {
			t.Fatalf("code 长度 = %d, want %d", len(code), billing.RedemptionCodeLength)
		}
	}
	return resp.Codes
}

func TestRedemptionManagementRBAC(t *testing.T) {
	env := newRedemptionTestEnv(t, nil)

	list := func(p *auth.Principal) int {
		return env.request(p, http.MethodGet, "/api/redemption-codes", nil).Code
	}
	if got := list(plainUser()); got != http.StatusForbidden {
		t.Fatalf("普通用户 GET = %d, want 403", got)
	}
	if got := list(&auth.Principal{UserID: 9, Role: rbac.RoleAdmin, Permissions: []string{}}); got != http.StatusForbidden {
		t.Fatalf("显式空权限 admin GET = %d, want 403", got)
	}
	if got := list(fullAdmin()); got != http.StatusOK {
		t.Fatalf("全权 admin GET = %d, want 200", got)
	}

	// 写端点同样要求 redemption:manage。
	limited := &auth.Principal{UserID: 9, Role: rbac.RoleAdmin, Permissions: []string{rbac.PermUsersRead}}
	for _, tc := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, "/api/redemption-codes", map[string]any{"batch_name": "b", "credits": 1, "count": 1}},
		{http.MethodPatch, "/api/redemption-codes/1", map[string]any{"status": "disabled"}},
		{http.MethodDelete, "/api/redemption-codes/1", nil},
		{http.MethodPost, "/api/redemption-codes/batch-delete", map[string]any{"ids": []int64{1}}},
	} {
		if rr := env.request(limited, tc.method, tc.path, tc.body); rr.Code != http.StatusForbidden {
			t.Fatalf("%s %s with unrelated permission = %d, want 403（body=%q）",
				tc.method, tc.path, rr.Code, rr.Body.String())
		}
	}
}

func TestRedemptionCreateValidation(t *testing.T) {
	env := newRedemptionTestEnv(t, nil)
	admin := fullAdmin()

	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{"count=0", map[string]any{"batch_name": "b", "credits": 10, "count": 0}},
		{"count=101", map[string]any{"batch_name": "b", "credits": 10, "count": 101}},
		{"credits=0", map[string]any{"batch_name": "b", "credits": 0, "count": 1}},
		{"credits<0", map[string]any{"batch_name": "b", "credits": -5, "count": 1}},
		{"batch missing", map[string]any{"credits": 10, "count": 1}},
		{"batch too long", map[string]any{"batch_name": strings.Repeat("x", 21), "credits": 10, "count": 1}},
		{"expired", map[string]any{"batch_name": "b", "credits": 10, "count": 1,
			"expires_at": time.Now().Add(-time.Minute).Format(time.RFC3339)}},
	} {
		rr := env.request(admin, http.MethodPost, "/api/redemption-codes", tc.body)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400（body=%q）", tc.name, rr.Code, rr.Body.String())
		}
	}
}

func TestRedemptionPlaintextOnlyInCreateResponse(t *testing.T) {
	env := newRedemptionTestEnv(t, nil)
	admin := fullAdmin()
	codes := createCodes(t, env, admin, 1_000, 3)

	rr := env.request(admin, http.MethodGet, "/api/redemption-codes?p=1&page_size=50&batch_name="+env.batch, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200（body=%q）", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, code := range codes {
		if strings.Contains(body, code) {
			t.Fatalf("列表响应泄露明文码 %q", code)
		}
	}
	var list struct {
		Items    []map[string]any `json:"items"`
		Total    int64            `json:"total"`
		Page     int              `json:"page"`
		PageSize int              `json:"page_size"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("list JSON: %v", err)
	}
	if list.Total < 3 || list.Page != 1 || len(list.Items) < 3 {
		t.Fatalf("list envelope = %+v", list)
	}
	for _, item := range list.Items {
		if item["code_mask"] != redemptionTestMask {
			t.Fatalf("code_mask = %v, want %q", item["code_mask"], redemptionTestMask)
		}
		if _, ok := item["code"]; ok {
			t.Fatalf("列表项不得包含 code 字段: %+v", item)
		}
	}
}

func TestRedemptionStatusAndDeleteRules(t *testing.T) {
	env := newRedemptionTestEnv(t, nil)
	admin := fullAdmin()
	_ = createCodes(t, env, admin, 500, 2)

	// 找到 id。
	rr := env.request(admin, http.MethodGet, "/api/redemption-codes?page_size=50&batch_name="+env.batch, nil)
	var list struct {
		Items []struct {
			ID     int64  `json:"id"`
			Status string `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("list JSON: %v", err)
	}
	ids := make([]int64, 0, len(list.Items))
	for _, item := range list.Items {
		if item.Status != "unused" {
			t.Fatalf("新码 status = %q, want unused", item.Status)
		}
		ids = append(ids, item.ID)
	}
	if len(ids) < 2 {
		t.Fatalf("ids = %v", ids)
	}

	// unused → disabled → unused。
	rr = env.request(admin, http.MethodPatch, fmt.Sprintf("/api/redemption-codes/%d", ids[0]),
		map[string]any{"status": "disabled"})
	if rr.Code != http.StatusOK {
		t.Fatalf("disable status = %d, want 200（body=%q）", rr.Code, rr.Body.String())
	}
	rr = env.request(admin, http.MethodPatch, fmt.Sprintf("/api/redemption-codes/%d", ids[0]),
		map[string]any{"status": "unused"})
	if rr.Code != http.StatusOK {
		t.Fatalf("enable status = %d, want 200（body=%q）", rr.Code, rr.Body.String())
	}
	// 不允许手工把状态改成 used。
	rr = env.request(admin, http.MethodPatch, fmt.Sprintf("/api/redemption-codes/%d", ids[0]),
		map[string]any{"status": "used"})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=used = %d, want 400", rr.Code)
	}

	// 已使用码：PATCH / DELETE 均 409。
	if _, err := env.db.Exec(`UPDATE redemption_codes SET status='used', used_by=1, used_at=now() WHERE id=$1`, ids[1]); err != nil {
		t.Fatalf("mark used: %v", err)
	}
	rr = env.request(admin, http.MethodPatch, fmt.Sprintf("/api/redemption-codes/%d", ids[1]),
		map[string]any{"status": "disabled"})
	if rr.Code != http.StatusConflict {
		t.Fatalf("patch used = %d, want 409（body=%q）", rr.Code, rr.Body.String())
	}
	rr = env.request(admin, http.MethodDelete, fmt.Sprintf("/api/redemption-codes/%d", ids[1]), nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("delete used = %d, want 409（body=%q）", rr.Code, rr.Body.String())
	}

	// 删除未使用码 → 204；再删 → 404。
	rr = env.request(admin, http.MethodDelete, fmt.Sprintf("/api/redemption-codes/%d", ids[0]), nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete unused = %d, want 204（body=%q）", rr.Code, rr.Body.String())
	}
	rr = env.request(admin, http.MethodDelete, fmt.Sprintf("/api/redemption-codes/%d", ids[0]), nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("delete missing = %d, want 404", rr.Code)
	}

	// 批量删除：未使用成功、已使用进入 failed。
	_ = createCodes(t, env, admin, 100, 1)
	var newID int64
	if err := env.db.QueryRow(`SELECT id FROM redemption_codes WHERE status='unused' ORDER BY id DESC LIMIT 1`).Scan(&newID); err != nil {
		t.Fatalf("read new id: %v", err)
	}
	rr = env.request(admin, http.MethodPost, "/api/redemption-codes/batch-delete",
		map[string]any{"ids": []int64{newID, ids[1]}})
	if rr.Code != http.StatusOK {
		t.Fatalf("batch-delete = %d, want 200（body=%q）", rr.Code, rr.Body.String())
	}
	var batch struct {
		Succeeded int `json:"succeeded"`
		Failed    []struct {
			ID     int64  `json:"id"`
			Detail string `json:"detail"`
		} `json:"failed"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &batch); err != nil {
		t.Fatalf("batch JSON: %v", err)
	}
	if batch.Succeeded != 1 || len(batch.Failed) != 1 || batch.Failed[0].ID != ids[1] {
		t.Fatalf("batch-delete = %+v", batch)
	}

	// 超过 100 个 id → 400。
	ids101 := make([]int64, 101)
	for i := range ids101 {
		ids101[i] = int64(i + 1)
	}
	rr = env.request(admin, http.MethodPost, "/api/redemption-codes/batch-delete", map[string]any{"ids": ids101})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("batch-delete 101 ids = %d, want 400", rr.Code)
	}
}

func TestRedeemHTTPFlow(t *testing.T) {
	env := newRedemptionTestEnv(t, nil)
	admin := fullAdmin()
	userID := createRedemptionTestUser(t, env.db, 0)
	user := &auth.Principal{UserID: userID, Role: rbac.RoleUser}

	codes := createCodes(t, env, admin, 1_000, 3)

	// 成功兑换：返回 credits 与 balance_after。
	rr := env.request(user, http.MethodPost, "/api/redemption-codes/redeem", map[string]any{"code": codes[0]})
	if rr.Code != http.StatusOK {
		t.Fatalf("redeem = %d, want 200（body=%q）", rr.Code, rr.Body.String())
	}
	var resp struct {
		Credits      int64 `json:"credits"`
		BalanceAfter int64 `json:"balance_after"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("redeem JSON: %v", err)
	}
	if resp.Credits != 1_000 || resp.BalanceAfter != 1_000 {
		t.Fatalf("redeem = %+v, want credits=1000 balance_after=1000", resp)
	}

	// 重复兑换 → 409 且 problem.type 区分「已使用」。
	rr = env.request(user, http.MethodPost, "/api/redemption-codes/redeem", map[string]any{"code": codes[0]})
	if rr.Code != http.StatusConflict {
		t.Fatalf("redeem used = %d, want 409（body=%q）", rr.Code, rr.Body.String())
	}
	if got := decodeRedemptionProblem(t, rr); got.Type == "" {
		t.Fatal("used problem.type 为空")
	}

	// 无效码 → 400。
	rr = env.request(user, http.MethodPost, "/api/redemption-codes/redeem", map[string]any{"code": "NOPE"})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("redeem invalid = %d, want 400（body=%q）", rr.Code, rr.Body.String())
	}

	// 禁用码 → 409。
	rr = env.request(admin, http.MethodPatch, fmt.Sprintf("/api/redemption-codes/%d", codeIDByValue(t, env, codes[1])),
		map[string]any{"status": "disabled"})
	if rr.Code != http.StatusOK {
		t.Fatalf("disable code = %d, want 200（body=%q）", rr.Code, rr.Body.String())
	}
	rr = env.request(user, http.MethodPost, "/api/redemption-codes/redeem", map[string]any{"code": codes[1]})
	if rr.Code != http.StatusConflict {
		t.Fatalf("redeem disabled = %d, want 409（body=%q）", rr.Code, rr.Body.String())
	}

	// 过期码（虚拟态）→ 410。
	expired := fmt.Sprintf("EXPR%d", time.Now().UnixNano())
	env.insertExpiredRedemptionCode(expired, 1_000)
	rr = env.request(user, http.MethodPost, "/api/redemption-codes/redeem", map[string]any{"code": expired})
	if rr.Code != http.StatusGone {
		t.Fatalf("redeem expired = %d, want 410（body=%q）", rr.Code, rr.Body.String())
	}

	// 未认证 → 401。
	rr = env.request(nil, http.MethodPost, "/api/redemption-codes/redeem", map[string]any{"code": codes[2]})
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated redeem = %d, want 401", rr.Code)
	}
}

// codeIDByValue 用明文反查 id（列表不返回码值，测试可以直查库）。
func codeIDByValue(t *testing.T, env *redemptionTestEnv, plain string) int64 {
	t.Helper()
	hash := billing.HashRedemptionCode(billing.NormalizeRedemptionCode(plain))
	var id int64
	if err := env.db.QueryRow(`SELECT id FROM redemption_codes WHERE code_hash=$1`, hash).Scan(&id); err != nil {
		t.Fatalf("find code id: %v", err)
	}
	return id
}

func TestRedeemRateLimit(t *testing.T) {
	env := newRedemptionTestEnv(t, nil)
	userID := createRedemptionTestUser(t, env.db, 0)
	user := &auth.Principal{UserID: userID, Role: rbac.RoleUser}

	for i := 0; i < 10; i++ {
		rr := env.request(user, http.MethodPost, "/api/redemption-codes/redeem",
			map[string]any{"code": fmt.Sprintf("BAD%d", i)})
		if rr.Code == http.StatusTooManyRequests {
			t.Fatalf("第 %d 次请求就被限流，阈值应为 10 次/分钟", i+1)
		}
	}
	rr := env.request(user, http.MethodPost, "/api/redemption-codes/redeem", map[string]any{"code": "BAD-FINAL"})
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("第 11 次请求 = %d, want 429（body=%q）", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Fatal("429 必须带 Retry-After")
	}
}
