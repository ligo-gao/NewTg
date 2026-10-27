//go:build integration

package httpapi

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/billing"
	"newtg/gateway/internal/rbac"
)

// M6 Task 2 的日志/统计/审计只读端点集成测试。
//
// 运行：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_test?sslmode=disable' \
//	NEWTG_TEST_LOGS_DSN='postgres://newtg:newtg@localhost:55432/newtg_logs?sslmode=disable' \
//	go test -tags=integration ./internal/httpapi/ -run TestIntegrationUsage -v
//
// 与既有集成测试同一口径：未设置 DSN 时跳过。

type usageTestEnv struct {
	mainDB  *sql.DB
	logsDB  *sql.DB
	handler http.Handler
	reqIDs  []string
	users   []int64
}

func newUsageTestEnv(t *testing.T) *usageTestEnv {
	t.Helper()
	return newUsageTestEnvWith(t, usageTestOptions{})
}

// usageTestOptions 让 F-05-07/F-05-08 的用例注入审计写入器与指标源；
// 其余用例保持零值（导出仍可用，只是不写审计）。
type usageTestOptions struct {
	Perf ModelPerfSource
	Now  func() time.Time
}

func newUsageTestEnvWith(t *testing.T, opts usageTestOptions) *usageTestEnv {
	t.Helper()
	mainDSN := os.Getenv("NEWTG_TEST_DB_DSN")
	logsDSN := os.Getenv("NEWTG_TEST_LOGS_DSN")
	if mainDSN == "" || logsDSN == "" {
		t.Skip("NEWTG_TEST_DB_DSN / NEWTG_TEST_LOGS_DSN 未设置，跳过集成测试")
	}
	mainDB, err := sql.Open("pgx", mainDSN)
	if err != nil {
		t.Fatalf("open main db: %v", err)
	}
	logsDB, err := sql.Open("pgx", logsDSN)
	if err != nil {
		_ = mainDB.Close()
		t.Fatalf("open logs db: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := mainDB.PingContext(ctx); err != nil {
		_ = mainDB.Close()
		_ = logsDB.Close()
		t.Fatalf("ping main db: %v", err)
	}
	if err := logsDB.PingContext(ctx); err != nil {
		_ = mainDB.Close()
		_ = logsDB.Close()
		t.Fatalf("ping logs db: %v", err)
	}

	ensureUsageTestTables(t, mainDB, logsDB)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h, err := NewUsageLogHandlers(UsageLogHandlerConfig{
		DB:     mainDB,
		LogDB:  logsDB,
		Audit:  audit.New(logsDB),
		Logger: logger,
		Perf:   opts.Perf,
		Now:    opts.Now,
	})
	if err != nil {
		t.Fatalf("NewUsageLogHandlers: %v", err)
	}
	mux := http.NewServeMux()
	h.RegisterUsageLogRoutes(mux, nil)

	env := &usageTestEnv{mainDB: mainDB, logsDB: logsDB, handler: mux}
	t.Cleanup(func() {
		for _, id := range env.reqIDs {
			if _, err := logsDB.Exec(`DELETE FROM usage_logs WHERE request_id = $1::uuid`, id); err != nil {
				t.Logf("cleanup usage_log %s: %v", id, err)
			}
		}
		for _, id := range env.users {
			_, _ = mainDB.Exec(`DELETE FROM users WHERE id = $1`, id)
		}
		_ = mainDB.Close()
		_ = logsDB.Close()
	})
	// 后登记先执行：在连接关闭前清掉历史上误建在主库的 usage_logs 残留表。
	dropMainUsageLogsArtifact(t, mainDB)
	return env
}

// ensureUsageTestTables 幂等补建两张日志表与当前/相邻月份分区。测试库已跑过
// migrations-logs 时是纯 no-op；空库时也能自举。
//
// 约束：usage_logs / audit_logs 的 DDL 只允许执行在 logsDB 上；主库
// （mainDB）只用于补默认用户组，绝不在这里建日志表——主库 schema 契约
// 由 internal/store.TestMainSchemaTableCount 守护。
func ensureUsageTestTables(t *testing.T, mainDB, logsDB *sql.DB) {
	t.Helper()
	if _, err := mainDB.Exec(`INSERT INTO user_groups (name) VALUES ('default') ON CONFLICT (name) DO NOTHING`); err != nil {
		t.Fatalf("ensure default group: %v", err)
	}
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS usage_logs (
			id bigint GENERATED ALWAYS AS IDENTITY,
			request_id uuid NOT NULL,
			user_id bigint NOT NULL,
			token_id bigint, token_name varchar(64),
			channel_id bigint, upstream varchar(128),
			model varchar(128) NOT NULL, "group" varchar(32),
			prompt_tokens int DEFAULT 0, completion_tokens int DEFAULT 0,
			cache_read_tokens int DEFAULT 0, cache_write_tokens int DEFAULT 0,
			pre_deducted bigint, cost bigint DEFAULT 0, refunded bigint DEFAULT 0,
			latency_ms int, first_token_ms int,
			stream boolean NOT NULL DEFAULT false,
			status smallint NOT NULL,
			error_class varchar, error_message varchar,
			retry_chain jsonb, other jsonb NOT NULL DEFAULT '{}'::jsonb,
			created_at timestamptz NOT NULL DEFAULT now(),
			CONSTRAINT usage_logs_pkey PRIMARY KEY (id, created_at),
			CONSTRAINT usage_logs_status_chk CHECK (status IN (0,1,2,3))
		) PARTITION BY RANGE (created_at)`,
		`CREATE INDEX IF NOT EXISTS usage_logs_user_created_idx ON usage_logs (user_id, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS usage_logs_created_idx ON usage_logs (created_at DESC)`,
		`CREATE TABLE IF NOT EXISTS audit_logs (
			id bigint GENERATED ALWAYS AS IDENTITY,
			user_id bigint NOT NULL, username text NOT NULL, category text NOT NULL,
			ip text, user_agent text, method text, route text, status_code smallint,
			result text NOT NULL, detail jsonb, before jsonb, after jsonb,
			created_at timestamptz NOT NULL DEFAULT now(),
			CONSTRAINT audit_logs_pkey PRIMARY KEY (id),
			CONSTRAINT audit_logs_category_chk CHECK (category IN ('login','security','operation','access_token')),
			CONSTRAINT audit_logs_result_chk CHECK (result IN ('success','failed'))
		)`,
		`CREATE INDEX IF NOT EXISTS audit_logs_user_created_idx ON audit_logs (user_id, created_at DESC)`,
	}
	for _, stmt := range stmts {
		if _, err := logsDB.Exec(stmt); err != nil {
			t.Fatalf("ensure logs schema: %v", err)
		}
	}
	// 覆盖上月/当月/下月，避免测试跨月时落入无分区区间。
	now := time.Now().UTC()
	for i := -1; i <= 1; i++ {
		from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, i, 0)
		to := from.AddDate(0, 1, 0)
		name := "usage_logs_" + from.Format("200601")
		stmt := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s PARTITION OF usage_logs FOR VALUES FROM ('%s') TO ('%s')`, name, from.Format(time.RFC3339), to.Format(time.RFC3339))
		if _, err := logsDB.Exec(stmt); err != nil {
			t.Fatalf("ensure partition %s: %v", name, err)
		}
	}
}

func ensureUsageTestUser(t *testing.T, env *usageTestEnv, prefix string) (int64, string) {
	t.Helper()
	username := prefix + strconv.FormatInt(time.Now().UnixNano(), 36)
	var id int64
	if err := env.mainDB.QueryRow(`INSERT INTO users (username, "group") VALUES ($1, 'default') RETURNING id`, username).Scan(&id); err != nil {
		t.Fatalf("insert user %s: %v", username, err)
	}
	env.users = append(env.users, id)
	return id, username
}

type usageSeed struct {
	UserID       int64
	RequestID    string
	Model        string
	Group        string
	Status       int16
	PromptTokens int
	Completion   int
	CacheRead    int
	Cost         int64
	PreDeducted  *int64
	Refunded     int64
	CreatedAt    time.Time
	Other        string
	RetryChain   string
	ErrorClass   string
	ErrorMessage string
	TokenID      *int64
	TokenName    string
	ChannelID    int64
	Upstream     string
	LatencyMS    int
	FirstTokenMS *int
	Stream       bool
}

func newRequestID(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func (env *usageTestEnv) seed(t *testing.T, s usageSeed) int64 {
	t.Helper()
	if s.RequestID == "" {
		s.RequestID = newRequestID(t)
	}
	if s.Model == "" {
		s.Model = "gpt-test"
	}
	if s.Group == "" {
		s.Group = "default"
	}
	if s.CreatedAt.IsZero() {
		s.CreatedAt = time.Now().UTC().Add(-time.Minute)
	}
	if s.Other == "" {
		s.Other = "{}"
	}
	var retry any
	if s.RetryChain != "" {
		retry = s.RetryChain
	}
	var tokenID any
	if s.TokenID != nil {
		tokenID = *s.TokenID
	}
	var pre any
	if s.PreDeducted != nil {
		pre = *s.PreDeducted
	}
	var first any
	if s.FirstTokenMS != nil {
		first = *s.FirstTokenMS
	}
	var errClass, errMessage any
	if s.ErrorClass != "" {
		errClass = s.ErrorClass
	}
	if s.ErrorMessage != "" {
		errMessage = s.ErrorMessage
	}
	var channel any
	if s.ChannelID != 0 {
		channel = s.ChannelID
	}
	var upstream, tokenName any
	if s.Upstream != "" {
		upstream = s.Upstream
	}
	if s.TokenName != "" {
		tokenName = s.TokenName
	}
	var latency any
	if s.LatencyMS != 0 {
		latency = s.LatencyMS
	}
	var id int64
	err := env.logsDB.QueryRow(`
		INSERT INTO usage_logs (request_id, user_id, token_id, token_name, channel_id,
			upstream, model, "group", prompt_tokens, completion_tokens, pre_deducted,
			cost, refunded, latency_ms, first_token_ms, stream, status, error_class,
			error_message, retry_chain, other, created_at, cache_read_tokens)
		VALUES ($1::uuid,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20::jsonb,$21::jsonb,$22,$23)
		RETURNING id`,
		s.RequestID, s.UserID, tokenID, tokenName, channel, upstream, s.Model, s.Group,
		s.PromptTokens, s.Completion, pre, s.Cost, s.Refunded, latency, first,
		s.Stream, s.Status, errClass, errMessage, retry, s.Other, s.CreatedAt, s.CacheRead,
	).Scan(&id)
	if err != nil {
		t.Fatalf("seed usage log: %v", err)
	}
	env.reqIDs = append(env.reqIDs, s.RequestID)
	return id
}

func (env *usageTestEnv) seedAudit(t *testing.T, userID int64, username, category, result string, createdAt time.Time) int64 {
	t.Helper()
	if createdAt.IsZero() {
		createdAt = time.Now().UTC().Add(-time.Minute)
	}
	var id int64
	if err := env.logsDB.QueryRow(`INSERT INTO audit_logs
		(user_id, username, category, ip, user_agent, method, route, status_code, result, detail, created_at)
		VALUES ($1,$2,$3,'203.0.113.9','test-agent','POST','/api/auth/login',200,$4,'{"event":"test"}'::jsonb,$5)
		RETURNING id`, userID, username, category, result, createdAt).Scan(&id); err != nil {
		t.Fatalf("seed audit log: %v", err)
	}
	t.Cleanup(func() {
		_, _ = env.logsDB.Exec(`DELETE FROM audit_logs WHERE id = $1`, id)
	})
	return id
}

func (env *usageTestEnv) do(t *testing.T, method, path string, p *auth.Principal) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, http.NoBody)
	req = req.WithContext(auth.WithPrincipal(req.Context(), p))
	return serve(env.handler, req)
}

func userPrincipal(id int64) *auth.Principal {
	return &auth.Principal{UserID: id, Role: "user", Permissions: []string{"users:read"}}
}

func usageAdminPrincipal(id int64, perms ...string) *auth.Principal {
	if perms == nil {
		perms = []string{rbac.PermLogsRead}
	}
	return &auth.Principal{UserID: id, Role: "admin", Permissions: perms}
}

func decodeUsageJSON[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response %d %q: %v", rec.Code, rec.Body.String(), err)
	}
	return out
}

// --- scoping / admin 参数 -------------------------------------------------

func TestIntegrationUsageLogsScoping(t *testing.T) {
	env := newUsageTestEnv(t)
	userA, nameA := ensureUsageTestUser(t, env, "ua")
	userB, nameB := ensureUsageTestUser(t, env, "ub")
	for i := 0; i < 3; i++ {
		env.seed(t, usageSeed{UserID: userA, Model: "model-a", Cost: 10})
	}
	for i := 0; i < 2; i++ {
		env.seed(t, usageSeed{UserID: userB, Model: "model-b", Cost: 20})
	}

	// 普通用户：只看到自己的 3 条。
	rec := env.do(t, http.MethodGet, "/api/usage-logs?range=24h", userPrincipal(userA))
	if rec.Code != http.StatusOK {
		t.Fatalf("user list status = %d body=%s", rec.Code, rec.Body.String())
	}
	page := decodeUsageJSON[struct {
		Items    []map[string]any `json:"items"`
		Total    int64            `json:"total"`
		Page     int              `json:"page"`
		PageSize int              `json:"page_size"`
	}](t, rec)
	if page.Total != 3 || len(page.Items) != 3 {
		t.Fatalf("user total/items = %d/%d, want 3/3", page.Total, len(page.Items))
	}
	for _, item := range page.Items {
		if item["model"] != "model-a" {
			t.Fatalf("user saw foreign model %v", item["model"])
		}
		if _, ok := item["username"]; ok {
			t.Fatalf("user list must not expose username")
		}
		// 对外字段名冻结为 ttft（G7-R51）；不得同时出现内部列名。
		if _, ok := item["ttft"]; !ok {
			t.Fatalf("list item missing ttft: %v", item)
		}
		if _, ok := item["first_token_ms"]; ok {
			t.Fatalf("list item leaked internal first_token_ms name: %v", item)
		}
	}

	// admin + logs:read：全部 5 条，且带 username。
	rec = env.do(t, http.MethodGet, "/api/usage-logs?range=24h", usageAdminPrincipal(1, rbac.PermLogsRead))
	if rec.Code != http.StatusOK {
		t.Fatalf("admin list status = %d body=%s", rec.Code, rec.Body.String())
	}
	adminPage := decodeUsageJSON[struct {
		Items []map[string]any `json:"items"`
		Total int64            `json:"total"`
	}](t, rec)
	if adminPage.Total != 5 {
		t.Fatalf("admin total = %d, want 5", adminPage.Total)
	}
	names := map[string]bool{}
	for _, item := range adminPage.Items {
		if v, ok := item["username"].(string); ok {
			names[v] = true
		}
	}
	if !names[nameA] || !names[nameB] {
		t.Fatalf("admin username lookup missing: %v", names)
	}

	// admin 但无 logs:read：退回本人范围（0 条）。
	rec = env.do(t, http.MethodGet, "/api/usage-logs?range=24h", usageAdminPrincipal(1, rbac.PermUsersRead))
	if rec.Code != http.StatusOK {
		t.Fatalf("admin-no-perm status = %d body=%s", rec.Code, rec.Body.String())
	}
	own := decodeUsageJSON[struct{ Total int64 }](t, rec)
	if own.Total != 0 {
		t.Fatalf("admin without logs:read total = %d, want 0 (own rows of user 1)", own.Total)
	}

	// 普通用户传 username → 403。
	rec = env.do(t, http.MethodGet, "/api/usage-logs?username="+nameA, userPrincipal(userA))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("user username filter status = %d, want 403", rec.Code)
	}
	// admin 无 logs:read 传 username → 403。
	rec = env.do(t, http.MethodGet, "/api/usage-logs?username="+nameA, usageAdminPrincipal(1, rbac.PermUsersRead))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("admin-no-perm username status = %d, want 403", rec.Code)
	}
	// admin + logs:read 传 username → 精确命中 3 条。
	rec = env.do(t, http.MethodGet, "/api/usage-logs?username="+nameA, usageAdminPrincipal(2, rbac.PermLogsRead))
	filtered := decodeUsageJSON[struct{ Total int64 }](t, rec)
	if rec.Code != http.StatusOK || filtered.Total != 3 {
		t.Fatalf("admin username filter status/total = %d/%d, want 200/3 body=%s", rec.Code, filtered.Total, rec.Body.String())
	}

	// 非 admin 传 scope / dimension → 403。
	for _, param := range []string{"scope=all", "dimension=channel"} {
		rec = env.do(t, http.MethodGet, "/api/usage-stats/summary?"+param, userPrincipal(userA))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("user stats %s status = %d, want 403", param, rec.Code)
		}
	}
}

// --- 参数校验 -------------------------------------------------------------

func TestIntegrationUsageLogsValidation(t *testing.T) {
	env := newUsageTestEnv(t)
	userID, _ := ensureUsageTestUser(t, env, "uv")
	p := userPrincipal(userID)

	cases := []struct{ name, query string }{
		{"bad range", "range=1y"},
		{"reversed", "start=2026-10-02T00:00:00Z&end=2026-10-01T00:00:00Z"},
		{"oversized", "start=2026-01-01T00:00:00Z&end=2026-05-01T00:00:00Z"},
		{"half window", "start=2026-10-01T00:00:00Z"},
		{"malformed start", "start=not-a-time&end=2026-10-02T00:00:00Z"},
		{"bad uuid", "request_id=not-a-uuid"},
	}
	for _, tc := range cases {
		rec := env.do(t, http.MethodGet, "/api/usage-logs?"+tc.query, p)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s: status = %d, want 422 (body=%s)", tc.name, rec.Code, rec.Body.String())
		}
		var problem map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
			t.Fatalf("%s: decode problem: %v", tc.name, err)
		}
		if problem["type"] != "https://newtg.dev/problems/invalid_request" {
			t.Fatalf("%s: problem type = %v", tc.name, problem["type"])
		}
		if detail, _ := problem["detail"].(string); detail == "" {
			t.Fatalf("%s: 422 problem must name the rule", tc.name)
		}
	}

	// 90 天整应被接受（边界含端点）。
	start := time.Now().UTC().Add(-90 * 24 * time.Hour)
	end := time.Now().UTC()
	rec := env.do(t, http.MethodGet, "/api/usage-logs?start="+start.Format(time.RFC3339)+"&end="+end.Format(time.RFC3339), p)
	if rec.Code != http.StatusOK {
		t.Fatalf("exactly 90d status = %d, want 200 body=%s", rec.Code, rec.Body.String())
	}
}

// --- 分页与 tie-breaker ---------------------------------------------------

func TestIntegrationUsageLogsPagination(t *testing.T) {
	env := newUsageTestEnv(t)
	userID, _ := ensureUsageTestUser(t, env, "up")
	at := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second)
	var seeded []int64
	for i := 0; i < 5; i++ {
		seeded = append(seeded, env.seed(t, usageSeed{UserID: userID, CreatedAt: at}))
	}

	seen := map[int64]bool{}
	for page := 1; page <= 3; page++ {
		rec := env.do(t, http.MethodGet, fmt.Sprintf("/api/usage-logs?p=%d&page_size=2", page), userPrincipal(userID))
		body := decodeUsageJSON[struct {
			Items []struct {
				ID int64 `json:"id"`
			} `json:"items"`
			Total    int64 `json:"total"`
			Page     int   `json:"page"`
			PageSize int   `json:"page_size"`
		}](t, rec)
		if body.Total != 5 || body.Page != page || body.PageSize != 2 {
			t.Fatalf("page %d envelope = %+v", page, body)
		}
		want := 2
		if page == 3 {
			want = 1
		}
		if len(body.Items) != want {
			t.Fatalf("page %d items = %d, want %d", page, len(body.Items), want)
		}
		for _, item := range body.Items {
			if seen[item.ID] {
				t.Fatalf("duplicate id %d across pages", item.ID)
			}
			seen[item.ID] = true
		}
	}
	if len(seen) != 5 {
		t.Fatalf("saw %d distinct ids, want 5", len(seen))
	}
	// created_at 相同时 id DESC：第一页首行必须是最大 id。
	first := env.do(t, http.MethodGet, "/api/usage-logs?p=1&page_size=2", userPrincipal(userID))
	body := decodeUsageJSON[struct {
		Items []struct {
			ID int64 `json:"id"`
		} `json:"items"`
	}](t, first)
	if body.Items[0].ID != seeded[4] || body.Items[1].ID != seeded[3] {
		t.Fatalf("tie-breaker order = %d,%d; want %d,%d", body.Items[0].ID, body.Items[1].ID, seeded[4], seeded[3])
	}
}

// --- 状态映射 -------------------------------------------------------------

func TestIntegrationUsageLogsStatusMapping(t *testing.T) {
	env := newUsageTestEnv(t)
	userID, _ := ensureUsageTestUser(t, env, "us")
	want := map[int16]string{0: "refused", 1: "success", 2: "error", 3: "refunded"}
	for status := int16(0); status <= 3; status++ {
		env.seed(t, usageSeed{UserID: userID, Status: status})
	}
	rec := env.do(t, http.MethodGet, "/api/usage-logs", userPrincipal(userID))
	body := decodeUsageJSON[struct {
		Items []struct {
			Status string `json:"status"`
		} `json:"items"`
	}](t, rec)
	if len(body.Items) != 4 {
		t.Fatalf("items = %d, want 4", len(body.Items))
	}
	got := map[string]bool{}
	for _, item := range body.Items {
		got[item.Status] = true
	}
	for _, status := range want {
		if !got[status] {
			t.Fatalf("status string %q missing (got %v)", status, got)
		}
	}
	for raw, name := range want {
		rec := env.do(t, http.MethodGet, fmt.Sprintf("/api/usage-logs?status=%s", name), userPrincipal(userID))
		filtered := decodeUsageJSON[struct {
			Items []struct {
				Status string `json:"status"`
			} `json:"items"`
		}](t, rec)
		if len(filtered.Items) != 1 || filtered.Items[0].Status != name {
			t.Fatalf("status filter %s (raw %d) = %+v", name, raw, filtered.Items)
		}
	}
}

// --- other / retry_chain 投影 --------------------------------------------

func TestIntegrationUsageLogsProjection(t *testing.T) {
	env := newUsageTestEnv(t)
	userID, _ := ensureUsageTestUser(t, env, "uj")
	other := `{
		"public_key":"visible",
		"channel_id":99,
		"channel_name":"legacy-channel",
		"channel_type":"openai",
		"reject_reason":"legacy-reason",
		"admin_info":{"quota_saturation":true},
		"root_info":{"root_only":1},
		"audit_info":{"root_audit":"secret"}
	}`
	retry := `[{"attempt":"1/2","channel_id":7,"channel_name":"ch-a","upstream":"up-a","use_group":"default","priority":10,"weight":5,"decision":"retry_next_priority","error_class":"timeout","latency_ms":120,"upstream_request_id":"req-1"}]`
	id := env.seed(t, usageSeed{UserID: userID, Other: other, RetryChain: retry})

	// user 列表：other 不出现；retry_chain 仅保留 8 个用户可见键。
	rec := env.do(t, http.MethodGet, "/api/usage-logs", userPrincipal(userID))
	userItem := decodeUsageJSON[struct {
		Items []struct {
			RetryChain []map[string]any `json:"retry_chain"`
		} `json:"items"`
	}](t, rec).Items[0]
	if _, ok := userItem.RetryChain[0]["priority"]; ok {
		t.Fatalf("user retry_chain leaked priority: %v", userItem.RetryChain[0])
	}
	if _, ok := userItem.RetryChain[0]["weight"]; ok {
		t.Fatalf("user retry_chain leaked weight: %v", userItem.RetryChain[0])
	}
	for _, key := range []string{"attempt", "channel_id", "channel_name", "upstream", "decision", "error_class", "latency_ms", "upstream_request_id"} {
		if _, ok := userItem.RetryChain[0][key]; !ok {
			t.Fatalf("user retry_chain missing %s: %v", key, userItem.RetryChain[0])
		}
	}

	// user 详情：other 仅剩 public_key，遗留敏感键与三分区全部剥离。
	rec = env.do(t, http.MethodGet, fmt.Sprintf("/api/usage-logs/%d", id), userPrincipal(userID))
	userDetail := decodeUsageJSON[struct {
		Other map[string]any `json:"other"`
	}](t, rec)
	if len(userDetail.Other) != 1 || userDetail.Other["public_key"] != "visible" {
		t.Fatalf("user other projection = %v, want only public_key", userDetail.Other)
	}

	// admin 详情：public + admin_info；root_info 与 audit_info（即使非空）
	// 都是 root 专属分区，不得下发给管理员（platform-01 §12.5）。
	rec = env.do(t, http.MethodGet, fmt.Sprintf("/api/usage-logs/%d", id), usageAdminPrincipal(1, rbac.PermLogsRead))
	adminDetail := decodeUsageJSON[struct {
		Other      map[string]any   `json:"other"`
		RetryChain []map[string]any `json:"retry_chain"`
		Username   *string          `json:"username"`
	}](t, rec)
	if _, ok := adminDetail.Other["audit_info"]; ok {
		t.Fatalf("admin other must not emit audit_info: %v", adminDetail.Other)
	}
	if _, ok := adminDetail.Other["admin_info"]; !ok {
		t.Fatalf("admin other missing admin_info: %v", adminDetail.Other)
	}
	if _, ok := adminDetail.Other["root_info"]; ok {
		t.Fatalf("admin other must not emit root_info: %v", adminDetail.Other)
	}
	if _, ok := adminDetail.RetryChain[0]["priority"]; !ok {
		t.Fatalf("admin retry_chain missing priority: %v", adminDetail.RetryChain[0])
	}
}

// --- 详情可见性 -----------------------------------------------------------

func TestIntegrationUsageLogDetailVisibility(t *testing.T) {
	env := newUsageTestEnv(t)
	userA, _ := ensureUsageTestUser(t, env, "ud")
	userB, _ := ensureUsageTestUser(t, env, "ue")
	foreignID := env.seed(t, usageSeed{UserID: userB})
	ownID := env.seed(t, usageSeed{UserID: userA})

	rec := env.do(t, http.MethodGet, fmt.Sprintf("/api/usage-logs/%d", ownID), userPrincipal(userA))
	if rec.Code != http.StatusOK {
		t.Fatalf("own detail status = %d body=%s", rec.Code, rec.Body.String())
	}
	rec = env.do(t, http.MethodGet, fmt.Sprintf("/api/usage-logs/%d", foreignID), userPrincipal(userA))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign detail status = %d, want 404", rec.Code)
	}
	rec = env.do(t, http.MethodGet, "/api/usage-logs/999999999", userPrincipal(userA))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing detail status = %d, want 404", rec.Code)
	}
}

// --- 令牌名补齐 -----------------------------------------------------------

// TestIntegrationUsageLogTokenNames 覆盖列表/详情的令牌名补齐。线上写入路径
// （saturate / logstream）把 usage_logs.token_name 恒写成 NULL，该列不可依赖；
// 读路径必须从主库 api_tokens 批量补齐。已存在的快照不得被覆盖，查不到的
// token_id 保持空名，请求仍返回 200。
func TestIntegrationUsageLogTokenNames(t *testing.T) {
	env := newUsageTestEnv(t)
	userID, _ := ensureUsageTestUser(t, env, "utk")
	group := fmt.Sprintf("utk-%d", time.Now().UnixNano()%1_000_000_000)

	tokenName := fmt.Sprintf("prod-key-%d", time.Now().UnixNano()%1_000_000_000)
	var tokenID int64
	if err := env.mainDB.QueryRow(`INSERT INTO api_tokens (user_id, name, key_hash, "group")
		VALUES ($1, $2, $3, 'default') RETURNING id`,
		userID, tokenName, fmt.Sprintf("it-utk-%d", time.Now().UnixNano())).Scan(&tokenID); err != nil {
		t.Fatalf("insert api_token: %v", err)
	}
	t.Cleanup(func() {
		_, _ = env.mainDB.Exec(`DELETE FROM api_tokens WHERE id = $1`, tokenID)
	})

	// 第一行按线上口径写入 token_name=NULL；第二行带历史快照；第三行的
	// token_id 在主库没有对应行。
	namedID := env.seed(t, usageSeed{UserID: userID, Group: group, TokenID: &tokenID})
	snapshotID := env.seed(t, usageSeed{UserID: userID, Group: group, TokenID: &tokenID, TokenName: "legacy-snapshot"})
	orphanTokenID := int64(980001)
	orphanID := env.seed(t, usageSeed{UserID: userID, Group: group, TokenID: &orphanTokenID})

	type listItem struct {
		ID        int64   `json:"id"`
		TokenID   *int64  `json:"token_id"`
		TokenName *string `json:"token_name"`
	}
	deref := func(value *string) string {
		if value == nil {
			return ""
		}
		return *value
	}

	p := usageAdminPrincipal(1, rbac.PermLogsRead)
	rec := env.do(t, http.MethodGet, "/api/usage-logs?group="+group+"&page_size=100", p)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200 body=%s", rec.Code, rec.Body.String())
	}
	list := decodeUsageJSON[struct {
		Items []listItem `json:"items"`
		Total int64      `json:"total"`
	}](t, rec)
	if list.Total != 3 || len(list.Items) != 3 {
		t.Fatalf("list total=%d items=%d, want 3/3 body=%s", list.Total, len(list.Items), rec.Body.String())
	}
	byID := make(map[int64]listItem, len(list.Items))
	for _, item := range list.Items {
		byID[item.ID] = item
	}
	if got := byID[namedID]; deref(got.TokenName) != tokenName {
		t.Fatalf("list token_name = %q, want %q (row %+v)", deref(got.TokenName), tokenName, got)
	}
	if got := byID[snapshotID]; deref(got.TokenName) != "legacy-snapshot" {
		t.Fatalf("list must keep stored snapshot, got %q", deref(got.TokenName))
	}
	if got := byID[orphanID]; deref(got.TokenName) != "" {
		t.Fatalf("list missing-token row token_name = %q, want empty", deref(got.TokenName))
	}

	for _, tc := range []struct {
		name string
		id   int64
		want string
	}{
		{"named", namedID, tokenName},
		{"snapshot", snapshotID, "legacy-snapshot"},
		{"orphan", orphanID, ""},
	} {
		rec := env.do(t, http.MethodGet, fmt.Sprintf("/api/usage-logs/%d", tc.id), p)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s detail status = %d, want 200 body=%s", tc.name, rec.Code, rec.Body.String())
		}
		detail := decodeUsageJSON[struct {
			TokenName *string `json:"token_name"`
		}](t, rec)
		if got := deref(detail.TokenName); got != tc.want {
			t.Fatalf("%s detail token_name = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// --- Prompt 无载体 --------------------------------------------------------

func TestIntegrationUsageLogPromptNoCarrier(t *testing.T) {
	env := newUsageTestEnv(t)
	userID, _ := ensureUsageTestUser(t, env, "uq")
	id := env.seed(t, usageSeed{UserID: userID})
	path := fmt.Sprintf("/api/usage-logs/%d/prompt", id)

	// 普通用户没有 log.prompt.view → 403。
	rec := env.do(t, http.MethodGet, path, userPrincipal(userID))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("user prompt status = %d, want 403 body=%s", rec.Code, rec.Body.String())
	}
	// admin 只有 logs:read、无 log.prompt.view → 403。
	rec = env.do(t, http.MethodGet, path, usageAdminPrincipal(1, rbac.PermLogsRead))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("admin without prompt perm status = %d, want 403", rec.Code)
	}
	// 有权限 + 可见行：无载体 → 404 + no-store。
	rec = env.do(t, http.MethodGet, path, usageAdminPrincipal(1, rbac.PermLogsRead, rbac.PermLogPromptView))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("prompt with permission status = %d, want 404 body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "no-store") {
		t.Fatalf("prompt 404 Cache-Control = %q, want no-store", got)
	}
}

// --- 统计 ----------------------------------------------------------------

func TestIntegrationUsageStatsSummary(t *testing.T) {
	env := newUsageTestEnv(t)
	userA, nameA := ensureUsageTestUser(t, env, "st")
	userB, _ := ensureUsageTestUser(t, env, "su")
	pre1, pre2, pre3 := int64(120), int64(50), int64(60)
	env.seed(t, usageSeed{UserID: userA, Status: 1, Cost: 100, PreDeducted: &pre1, Refunded: 20, PromptTokens: 10, Completion: 20, CacheRead: 40})
	env.seed(t, usageSeed{UserID: userA, Status: 2, Cost: 0, PreDeducted: &pre2, Refunded: 50, PromptTokens: 5, Completion: 0})
	env.seed(t, usageSeed{UserID: userA, Status: 1, Cost: 50, PreDeducted: &pre3, Refunded: 10, PromptTokens: 1, Completion: 2})
	env.seed(t, usageSeed{UserID: userA, Status: 0, Cost: 0, PromptTokens: 0, Completion: 0})
	env.seed(t, usageSeed{UserID: userB, Status: 1, Cost: 999, PreDeducted: &pre1})

	type summary struct {
		CostTotal             int64   `json:"cost_total"`
		RequestCount          int64   `json:"request_count"`
		SuccessRate           float64 `json:"success_rate"`
		PromptTokensTotal     int64   `json:"prompt_tokens_total"`
		CompletionTokensTotal int64   `json:"completion_tokens_total"`
		PreDeductedTotal      int64   `json:"pre_deducted_total"`
		RefundedTotal         int64   `json:"refunded_total"`
		CacheReadTokensTotal  int64   `json:"cache_read_tokens_total"`
		SuccessCount          int64   `json:"success_count"`
		ErrorCount            int64   `json:"error_count"`
		RefundedCount         int64   `json:"refunded_count"`
		RefusedCount          int64   `json:"refused_count"`
	}

	rec := env.do(t, http.MethodGet, "/api/usage-stats/summary", userPrincipal(userA))
	got := decodeUsageJSON[summary](t, rec)
	want := summary{
		CostTotal: 150, RequestCount: 4, SuccessRate: 0.5,
		PromptTokensTotal: 16, CompletionTokensTotal: 22,
		PreDeductedTotal: 230, RefundedTotal: 80,
		SuccessCount: 2, ErrorCount: 1, RefundedCount: 0, RefusedCount: 1,
		CacheReadTokensTotal: 40,
	}
	if got != want {
		t.Fatalf("user summary = %+v, want %+v", got, want)
	}

	// admin 默认看全部（含 userB 的 999）。日志库可能残留其它测试的行，
	// 因此断言下界而不是精确值；精确口径由 username 过滤用例覆盖。
	rec = env.do(t, http.MethodGet, "/api/usage-stats/summary", usageAdminPrincipal(1, rbac.PermLogsRead))
	all := decodeUsageJSON[summary](t, rec)
	if all.CostTotal < 1149 || all.RequestCount < 5 {
		t.Fatalf("admin summary = %+v, want at least cost=1149 count=5", all)
	}
	// admin username 过滤。
	rec = env.do(t, http.MethodGet, "/api/usage-stats/summary?username="+nameA, usageAdminPrincipal(1, rbac.PermLogsRead))
	filtered := decodeUsageJSON[summary](t, rec)
	if filtered != want {
		t.Fatalf("admin username summary = %+v, want %+v", filtered, want)
	}
	// 空范围 success_rate 必须为 0 而不是 NaN。
	rec = env.do(t, http.MethodGet, "/api/usage-stats/summary?username=nobody_here__", usageAdminPrincipal(1, rbac.PermLogsRead))
	if rec.Code != http.StatusOK {
		t.Fatalf("unknown username summary status = %d", rec.Code)
	}
	empty := decodeUsageJSON[summary](t, rec)
	if empty.RequestCount != 0 || empty.SuccessRate != 0 {
		t.Fatalf("empty summary = %+v", empty)
	}
}

func TestIntegrationUsageStatsByModel(t *testing.T) {
	env := newUsageTestEnv(t)
	userID, _ := ensureUsageTestUser(t, env, "bm")
	// 各模型请求数刻意不同（model-0 三次、model-1 两次、其余一次），
	// 且 tokens 与请求数不相等：count(*) 漏算或 scan 顺序错位都会被断言抓到。
	for i := 0; i < 3; i++ {
		env.seed(t, usageSeed{UserID: userID, Model: "model-0", Cost: 10, PromptTokens: 1, Completion: 1})
	}
	for i := 0; i < 2; i++ {
		env.seed(t, usageSeed{UserID: userID, Model: "model-1", Cost: 10, PromptTokens: 1, Completion: 1})
	}
	for i := 2; i < 9; i++ {
		env.seed(t, usageSeed{UserID: userID, Model: fmt.Sprintf("model-%d", i), Cost: 10, PromptTokens: 1, Completion: 1})
	}
	rec := env.do(t, http.MethodGet, "/api/usage-stats/by-model", userPrincipal(userID))
	body := decodeUsageJSON[[]struct {
		Model    string  `json:"model"`
		Cost     int64   `json:"cost"`
		Tokens   int64   `json:"tokens"`
		Requests int64   `json:"requests"`
		Percent  float64 `json:"percent"`
	}](t, rec)
	if rec.Code != http.StatusOK {
		t.Fatalf("by-model status = %d body=%s", rec.Code, rec.Body.String())
	}
	if len(body) != 9 {
		t.Fatalf("by-model rows = %d, want top 8 + other = 9", len(body))
	}
	if body[0].Model != "model-0" || body[0].Cost != 30 || body[0].Tokens != 6 || body[0].Requests != 3 {
		t.Fatalf("model-0 = %+v, want cost=30 tokens=6 requests=3", body[0])
	}
	if body[1].Model != "model-1" || body[1].Cost != 20 || body[1].Tokens != 4 || body[1].Requests != 2 {
		t.Fatalf("model-1 = %+v, want cost=20 tokens=4 requests=2", body[1])
	}
	if body[8].Model != "other" || body[8].Cost != 10 || body[8].Tokens != 2 || body[8].Requests != 1 {
		t.Fatalf("other bucket = %+v, want cost=10 tokens=2 requests=1", body[8])
	}
	var sum, requests int64
	for _, item := range body {
		sum += int64(math.Round(item.Percent * 100))
		requests += item.Requests
	}
	if sum != 10000 {
		t.Fatalf("percent basis points = %d, want 10000", sum)
	}
	if requests != 12 {
		t.Fatalf("total requests = %d, want 12 (3+2+7)", requests)
	}

	// 0 行 → 空数组（不是 null）。
	otherUser, _ := ensureUsageTestUser(t, env, "b0")
	rec = env.do(t, http.MethodGet, "/api/usage-stats/by-model", userPrincipal(otherUser))
	if got := strings.TrimSpace(rec.Body.String()); got != `[]` {
		t.Fatalf("empty by-model = %s, want []", got)
	}
}

// TestIntegrationUsageStatsByModelPercentRegression 覆盖评审 round 1 的
// 残差回归：9 个已知消费在四舍五入后基点合计 9999，残差必须在整数基点上
// 补齐；旧实现先把百分数转 float64 再重建 int64，会因截断丢掉残差（99.99）。
func TestIntegrationUsageStatsByModelPercentRegression(t *testing.T) {
	env := newUsageTestEnv(t)
	userID, _ := ensureUsageTestUser(t, env, "br")
	costs := []int64{3975, 3443, 2842, 2632, 2602, 2311, 2283, 1262, 1257}
	for i, cost := range costs {
		env.seed(t, usageSeed{UserID: userID, Model: fmt.Sprintf("reg-%d", i), Cost: cost})
	}
	rec := env.do(t, http.MethodGet, "/api/usage-stats/by-model", userPrincipal(userID))
	body := decodeUsageJSON[[]struct {
		Model   string  `json:"model"`
		Percent float64 `json:"percent"`
	}](t, rec)
	if rec.Code != http.StatusOK || len(body) != 9 {
		t.Fatalf("by-model status/rows = %d/%d body=%s", rec.Code, len(body), rec.Body.String())
	}
	var bps int64
	for _, item := range body {
		bps += int64(math.Round(item.Percent * 100))
	}
	if bps != 10000 {
		t.Fatalf("percent basis points = %d, want exactly 10000 (%+v)", bps, body)
	}
	if body[0].Percent != 17.59 {
		t.Fatalf("largest model percent = %v, want 17.59 (residual applied)", body[0].Percent)
	}
}

func TestIntegrationUsageStatsTimeseriesUTC(t *testing.T) {
	env := newUsageTestEnv(t)
	userID, _ := ensureUsageTestUser(t, env, "ts")
	day2 := time.Now().UTC().Truncate(24 * time.Hour)
	day1 := day2.Add(-24 * time.Hour)
	env.seed(t, usageSeed{UserID: userID, Cost: 7, CreatedAt: day1.Add(23*time.Hour + 30*time.Minute)})
	env.seed(t, usageSeed{UserID: userID, Cost: 11, CreatedAt: day2.Add(30 * time.Minute)})
	q := fmt.Sprintf("?start=%s&end=%s", day1.Format(time.RFC3339), day2.Add(24*time.Hour).Format(time.RFC3339))

	rec := env.do(t, http.MethodGet, "/api/usage-stats/timeseries"+q+"&granularity=day", userPrincipal(userID))
	day := decodeUsageJSON[[]struct {
		Bucket   string `json:"bucket"`
		Cost     int64  `json:"cost"`
		Requests int64  `json:"requests"`
	}](t, rec)
	if rec.Code != http.StatusOK || len(day) != 2 {
		t.Fatalf("day buckets = %d status=%d body=%s", len(day), rec.Code, rec.Body.String())
	}
	wantDay1 := day1.Format("2006-01-02") + "T00:00:00Z"
	wantDay2 := day2.Format("2006-01-02") + "T00:00:00Z"
	if day[0].Bucket != wantDay1 || day[1].Bucket != wantDay2 {
		t.Fatalf("day buckets = %s,%s want %s,%s", day[0].Bucket, day[1].Bucket, wantDay1, wantDay2)
	}
	if day[0].Cost != 7 || day[1].Cost != 11 {
		t.Fatalf("day costs = %+v", day)
	}

	rec = env.do(t, http.MethodGet, "/api/usage-stats/timeseries"+q+"&granularity=hour", userPrincipal(userID))
	hour := decodeUsageJSON[[]struct {
		Bucket string `json:"bucket"`
	}](t, rec)
	if len(hour) != 2 {
		t.Fatalf("hour buckets = %d, want 2", len(hour))
	}
	if hour[0].Bucket != day1.Format("2006-01-02")+"T23:00:00Z" ||
		hour[1].Bucket != day2.Format("2006-01-02")+"T00:00:00Z" {
		t.Fatalf("hour buckets = %+v", hour)
	}

	// 非法 granularity → 422。
	rec = env.do(t, http.MethodGet, "/api/usage-stats/timeseries"+q+"&granularity=week", userPrincipal(userID))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bad granularity status = %d, want 422", rec.Code)
	}
}

func TestIntegrationUsageStatsRates(t *testing.T) {
	env := newUsageTestEnv(t)
	userID, _ := ensureUsageTestUser(t, env, "rt")
	end := time.Now().UTC().Truncate(time.Minute)
	start := end.Add(-10 * time.Minute)
	cost := int64(5)
	// 5 条同一分钟 + 5 条另一分钟 = 10 条 / 10 分钟。
	for i := 0; i < 5; i++ {
		env.seed(t, usageSeed{UserID: userID, Cost: cost, PromptTokens: 50, Completion: 50, CreatedAt: start.Add(time.Minute)})
		env.seed(t, usageSeed{UserID: userID, Cost: cost, PromptTokens: 50, Completion: 50, CreatedAt: start.Add(9 * time.Minute)})
	}
	q := fmt.Sprintf("?start=%s&end=%s", start.Format(time.RFC3339), end.Format(time.RFC3339))
	rec := env.do(t, http.MethodGet, "/api/usage-stats/rates"+q, userPrincipal(userID))
	body := decodeUsageJSON[struct {
		RPMAvg    float64 `json:"rpm_avg"`
		RPMPeak   float64 `json:"rpm_peak"`
		TPMAvg    float64 `json:"tpm_avg"`
		CostTotal int64   `json:"cost_total"`
	}](t, rec)
	if rec.Code != http.StatusOK {
		t.Fatalf("rates status = %d body=%s", rec.Code, rec.Body.String())
	}
	if body.CostTotal != 50 {
		t.Fatalf("rates cost_total = %d, want 50", body.CostTotal)
	}
	if body.RPMAvg != 1 || body.RPMPeak != 5 {
		t.Fatalf("rates rpm avg/peak = %v/%v, want 1/5", body.RPMAvg, body.RPMPeak)
	}
	// 1000 tokens / 10 分钟 = 100 tokens/min。
	if body.TPMAvg != 100 {
		t.Fatalf("rates tpm_avg = %v, want 100", body.TPMAvg)
	}

	// user 传 scope → 403；admin + logs:read 传 scope=all → 200。
	rec = env.do(t, http.MethodGet, "/api/usage-stats/rates"+"?scope=all", userPrincipal(userID))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("user scope status = %d, want 403", rec.Code)
	}
	rec = env.do(t, http.MethodGet, "/api/usage-stats/rates"+q+"&scope=all", usageAdminPrincipal(1, rbac.PermLogsRead))
	if rec.Code != http.StatusOK {
		t.Fatalf("admin scope=all status = %d body=%s", rec.Code, rec.Body.String())
	}
}

// --- 审计日志 -------------------------------------------------------------

func TestIntegrationAuditLogsScoping(t *testing.T) {
	env := newUsageTestEnv(t)
	userA, nameA := ensureUsageTestUser(t, env, "aa")
	userB, nameB := ensureUsageTestUser(t, env, "ab")
	env.seedAudit(t, userA, nameA, "login", "success", time.Now().UTC().Add(-2*time.Minute))
	env.seedAudit(t, userA, nameA, "security", "failed", time.Now().UTC().Add(-3*time.Minute))
	env.seedAudit(t, userB, nameB, "operation", "success", time.Now().UTC().Add(-4*time.Minute))

	rec := env.do(t, http.MethodGet, "/api/audit-logs", userPrincipal(userA))
	if rec.Code != http.StatusOK {
		t.Fatalf("user audit status = %d body=%s", rec.Code, rec.Body.String())
	}
	page := decodeUsageJSON[struct {
		Items []map[string]any `json:"items"`
		Total int64            `json:"total"`
	}](t, rec)
	if page.Total != 2 || len(page.Items) != 2 {
		t.Fatalf("user audit total/items = %d/%d, want 2/2", page.Total, len(page.Items))
	}
	for _, item := range page.Items {
		if item["user_id"] != float64(userA) {
			t.Fatalf("user audit saw foreign row: %v", item)
		}
		if _, ok := item["username"]; ok {
			t.Fatalf("user audit must hide username: %v", item)
		}
	}

	rec = env.do(t, http.MethodGet, "/api/audit-logs?page_size=100", usageAdminPrincipal(1, rbac.PermLogsRead))
	all := decodeUsageJSON[struct {
		Items []map[string]any `json:"items"`
		Total int64            `json:"total"`
	}](t, rec)
	// 日志库可能残留其它测试的审计行；断言本用例的两条事件都在，
	// 且 admin 视图带 username。精确过滤由后续 username 用例覆盖。
	if all.Total < 3 {
		t.Fatalf("admin audit total = %d, want >= 3", all.Total)
	}
	names := map[string]bool{}
	for _, item := range all.Items {
		if v, ok := item["username"].(string); ok {
			names[v] = true
		}
	}
	if !names[nameA] || !names[nameB] {
		t.Fatalf("admin audit username missing: %v", names)
	}

	// admin 无 logs:read：只看自己（0 条）。
	rec = env.do(t, http.MethodGet, "/api/audit-logs", usageAdminPrincipal(1, rbac.PermUsersRead))
	own := decodeUsageJSON[struct{ Total int64 }](t, rec)
	if own.Total != 0 {
		t.Fatalf("admin without logs:read audit total = %d, want 0", own.Total)
	}

	// user 传 username → 403；admin 传 username → 过滤。
	rec = env.do(t, http.MethodGet, "/api/audit-logs?username="+nameA, userPrincipal(userA))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("user audit username status = %d, want 403", rec.Code)
	}
	rec = env.do(t, http.MethodGet, "/api/audit-logs?username="+nameB, usageAdminPrincipal(1, rbac.PermLogsRead))
	filtered := decodeUsageJSON[struct{ Total int64 }](t, rec)
	if rec.Code != http.StatusOK || filtered.Total != 1 {
		t.Fatalf("admin audit username status/total = %d/%d", rec.Code, filtered.Total)
	}

	// category + result 组合。
	rec = env.do(t, http.MethodGet, "/api/audit-logs?category=security&result=failed", usageAdminPrincipal(1, rbac.PermLogsRead))
	combo := decodeUsageJSON[struct {
		Items []map[string]any `json:"items"`
		Total int64            `json:"total"`
	}](t, rec)
	if combo.Total != 1 || combo.Items[0]["category"] != "security" || combo.Items[0]["result"] != "failed" {
		t.Fatalf("audit combo = %+v", combo)
	}

	// 非法 category → 422。
	rec = env.do(t, http.MethodGet, "/api/audit-logs?category=bogus", userPrincipal(userA))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bad audit category status = %d, want 422", rec.Code)
	}
}

// --- 仪表台核心指标与构成 ------------------------------------------------

func TestIntegrationUsageStatsLatencyAndActiveUsers(t *testing.T) {
	env := newUsageTestEnv(t)
	userA, _ := ensureUsageTestUser(t, env, "lt")
	userB, _ := ensureUsageTestUser(t, env, "lu")
	group := fmt.Sprintf("lat-%d", time.Now().UnixNano()%1_000_000_000)
	env.seed(t, usageSeed{UserID: userA, Group: group, Status: 1, Cost: 10, LatencyMS: 100})
	env.seed(t, usageSeed{UserID: userA, Group: group, Status: 1, Cost: 10, LatencyMS: 200})
	env.seed(t, usageSeed{UserID: userB, Group: group, Status: 1, Cost: 10, LatencyMS: 300})
	// 失败请求的 latency 不参与分位；它仍计入成功率与活跃用户。
	env.seed(t, usageSeed{UserID: userB, Group: group, Status: 2, Cost: 10, LatencyMS: 5000})

	type summary struct {
		P50LatencyMS   int64 `json:"p50_latency_ms"`
		P95LatencyMS   int64 `json:"p95_latency_ms"`
		LatencySamples int64 `json:"latency_samples"`
		ActiveUsers    int64 `json:"active_users"`
	}

	path := "/api/usage-stats/summary?group=" + group
	rec := env.do(t, http.MethodGet, path, usageAdminPrincipal(1, rbac.PermLogsRead))
	all := decodeUsageJSON[summary](t, rec)
	// [100,200,300] → p50=200, p95=290。
	if all.P50LatencyMS != 200 || all.P95LatencyMS != 290 || all.LatencySamples != 3 || all.ActiveUsers != 2 {
		t.Fatalf("admin latency/active = %+v, want p50=200 p95=290 samples=3 active=2", all)
	}

	// 普通用户只看自己的两行：[100,200] → p50=150, p95=195。
	rec = env.do(t, http.MethodGet, path, userPrincipal(userA))
	mine := decodeUsageJSON[summary](t, rec)
	if mine.P50LatencyMS != 150 || mine.P95LatencyMS != 195 || mine.LatencySamples != 2 || mine.ActiveUsers != 1 {
		t.Fatalf("user latency/active = %+v, want p50=150 p95=195 samples=2 active=1", mine)
	}

	// 空集必须返回 0，而不是 NaN 或 NULL 扫描错误。
	rec = env.do(t, http.MethodGet, "/api/usage-stats/summary?username=nobody_latency__", usageAdminPrincipal(1, rbac.PermLogsRead))
	empty := decodeUsageJSON[summary](t, rec)
	if empty.P50LatencyMS != 0 || empty.P95LatencyMS != 0 || empty.LatencySamples != 0 || empty.ActiveUsers != 0 {
		t.Fatalf("empty latency/active = %+v, want zeros", empty)
	}
}

func TestIntegrationUsageStatsByUser(t *testing.T) {
	env := newUsageTestEnv(t)
	userA, nameA := ensureUsageTestUser(t, env, "bu")
	userB, nameB := ensureUsageTestUser(t, env, "bv")
	group := fmt.Sprintf("bu-%d", time.Now().UnixNano()%1_000_000_000)
	env.seed(t, usageSeed{UserID: userA, Group: group, Status: 1, Cost: 70})
	env.seed(t, usageSeed{UserID: userB, Group: group, Status: 1, Cost: 30})

	type item struct {
		UserID   int64   `json:"user_id"`
		Username string  `json:"username"`
		Cost     int64   `json:"cost"`
		Requests int64   `json:"requests"`
		Tokens   int64   `json:"tokens"`
		Percent  float64 `json:"percent"`
	}
	type envelope struct {
		Items []item `json:"items"`
		Total int64  `json:"total"`
	}

	path := "/api/usage-stats/by-user?group=" + group
	rec := env.do(t, http.MethodGet, path, usageAdminPrincipal(1, rbac.PermLogsRead))
	body := decodeUsageJSON[envelope](t, rec)
	if rec.Code != http.StatusOK || body.Total != 100 || len(body.Items) != 2 {
		t.Fatalf("by-user = %d total=%d items=%d body=%s", rec.Code, body.Total, len(body.Items), rec.Body.String())
	}
	byName := map[string]item{}
	for _, it := range body.Items {
		byName[it.Username] = it
	}
	if byName[nameA].Cost != 70 || byName[nameA].Percent != 70 {
		t.Fatalf("user A row = %+v", byName[nameA])
	}
	if byName[nameB].Cost != 30 || byName[nameB].Percent != 30 {
		t.Fatalf("user B row = %+v", byName[nameB])
	}

	// 非管理员只能看到自己，百分比为 100。
	rec = env.do(t, http.MethodGet, path, userPrincipal(userA))
	mine := decodeUsageJSON[envelope](t, rec)
	if len(mine.Items) != 1 || mine.Items[0].Username != nameA || mine.Items[0].Cost != 70 || mine.Items[0].Percent != 100 {
		t.Fatalf("user by-user = %+v", mine)
	}
}

func TestIntegrationUsageStatsByGroup(t *testing.T) {
	env := newUsageTestEnv(t)
	userA, _ := ensureUsageTestUser(t, env, "bg")
	userB, _ := ensureUsageTestUser(t, env, "bh")
	groupA := fmt.Sprintf("ga-%d", time.Now().UnixNano()%1_000_000_000)
	groupB := fmt.Sprintf("gb-%d", time.Now().UnixNano()%1_000_000_000)
	env.seed(t, usageSeed{UserID: userA, Group: groupA, Status: 1, Cost: 60})
	env.seed(t, usageSeed{UserID: userB, Group: groupB, Status: 1, Cost: 40})

	type item struct {
		Group   string  `json:"group"`
		Cost    int64   `json:"cost"`
		Percent float64 `json:"percent"`
	}
	type envelope struct {
		Items []item `json:"items"`
		Total int64  `json:"total"`
	}

	for _, tc := range []struct {
		group string
		cost  int64
	}{{groupA, 60}, {groupB, 40}} {
		rec := env.do(t, http.MethodGet, "/api/usage-stats/by-group?group="+tc.group, usageAdminPrincipal(1, rbac.PermLogsRead))
		body := decodeUsageJSON[envelope](t, rec)
		if rec.Code != http.StatusOK || body.Total != tc.cost || len(body.Items) != 1 {
			t.Fatalf("by-group(%s) = %d total=%d items=%d", tc.group, rec.Code, body.Total, len(body.Items))
		}
		if body.Items[0].Group != tc.group || body.Items[0].Cost != tc.cost || body.Items[0].Percent != 100 {
			t.Fatalf("by-group(%s) item = %+v", tc.group, body.Items[0])
		}
	}
}

func TestIntegrationUsageStatsByToken(t *testing.T) {
	env := newUsageTestEnv(t)
	userA, _ := ensureUsageTestUser(t, env, "btt")
	userB, _ := ensureUsageTestUser(t, env, "btu")
	tokenA, tokenB := int64(910001), int64(910002)
	group := fmt.Sprintf("btt-%d", time.Now().UnixNano()%1_000_000_000)
	env.seed(t, usageSeed{UserID: userA, Group: group, Status: 1, Cost: 70, TokenID: &tokenA, TokenName: "prod-key"})
	// 第二个用户的同组行：若用户维度过滤失效，普通用户的响应会多出一项。
	env.seed(t, usageSeed{UserID: userB, Group: group, Status: 1, Cost: 30, TokenID: &tokenB, TokenName: "batch-01"})

	type item struct {
		TokenID   int64   `json:"token_id"`
		TokenName string  `json:"token_name"`
		Cost      int64   `json:"cost"`
		Requests  int64   `json:"requests"`
		Tokens    int64   `json:"tokens"`
		Percent   float64 `json:"percent"`
	}
	type envelope struct {
		Items []item `json:"items"`
		Total int64  `json:"total"`
	}

	rec := env.do(t, http.MethodGet, "/api/usage-stats/by-token?group="+group, usageAdminPrincipal(1, rbac.PermLogsRead))
	body := decodeUsageJSON[envelope](t, rec)
	if rec.Code != http.StatusOK || body.Total != 100 || len(body.Items) != 2 {
		t.Fatalf("by-token = %d total=%d items=%d body=%s", rec.Code, body.Total, len(body.Items), rec.Body.String())
	}
	if body.Items[0].TokenName != "prod-key" || body.Items[0].Cost != 70 || body.Items[0].Percent != 70 {
		t.Fatalf("first by-token item = %+v", body.Items[0])
	}
	if body.Items[1].TokenID != tokenB || body.Items[1].Percent != 30 {
		t.Fatalf("second by-token item = %+v", body.Items[1])
	}

	// A normal user is scoped to their own rows, and percent is re-based to 100.
	rec = env.do(t, http.MethodGet, "/api/usage-stats/by-token?group="+group, userPrincipal(userA))
	mine := decodeUsageJSON[envelope](t, rec)
	if len(mine.Items) != 1 || mine.Total != 70 ||
		mine.Items[0].TokenID != tokenA || mine.Items[0].Percent != 100 {
		t.Fatalf("user by-token = %+v", mine)
	}
}

// TestIntegrationUsageStatsByTokenOtherBucketAndNames 覆盖 by-token 的
// other 桶 / 无令牌桶 / Requests/Tokens 聚合，并证明令牌名来自主库 api_tokens：
// 线上写入路径把 usage_logs.token_name 写成 NULL，这里保持该列为 NULL。
func TestIntegrationUsageStatsByTokenOtherBucketAndNames(t *testing.T) {
	env := newUsageTestEnv(t)
	userID, _ := ensureUsageTestUser(t, env, "btn")
	group := fmt.Sprintf("btn-%d", time.Now().UnixNano()%1_000_000_000)

	// 9 个令牌桶 + 1 个无令牌桶 = 10 个桶；前 8 名之外聚合为 other。
	// 无令牌桶取最高消费，保证它留在前 8 名内可被单独断言。
	costs := []int64{900, 800, 700, 600, 500, 400, 300, 200, 100}
	tokenIDs := make([]int64, len(costs))
	for i, cost := range costs {
		tokenID := int64(0)
		if err := env.mainDB.QueryRow(`INSERT INTO api_tokens (user_id, name, key_hash, "group")
			VALUES ($1, $2, $3, 'default') RETURNING id`,
			userID, fmt.Sprintf("tk-%d", i+1),
			fmt.Sprintf("it-btn-%d-%d", time.Now().UnixNano(), i)).Scan(&tokenID); err != nil {
			t.Fatalf("insert api_token %d: %v", i+1, err)
		}
		tokenIDs[i] = tokenID
		tid := tokenID
		env.seed(t, usageSeed{
			UserID: userID, Group: group, Status: 1, Cost: cost,
			TokenID: &tid, PromptTokens: i + 1, Completion: i + 1,
		})
	}
	t.Cleanup(func() {
		for _, id := range tokenIDs {
			_, _ = env.mainDB.Exec(`DELETE FROM api_tokens WHERE id = $1`, id)
		}
	})
	env.seed(t, usageSeed{UserID: userID, Group: group, Status: 1, Cost: 950, PromptTokens: 5, Completion: 5})

	type item struct {
		TokenID   int64   `json:"token_id"`
		TokenName string  `json:"token_name"`
		Cost      int64   `json:"cost"`
		Requests  int64   `json:"requests"`
		Tokens    int64   `json:"tokens"`
		Percent   float64 `json:"percent"`
	}
	type envelope struct {
		Items []item `json:"items"`
		Total int64  `json:"total"`
	}

	rec := env.do(t, http.MethodGet, "/api/usage-stats/by-token?group="+group, usageAdminPrincipal(1, rbac.PermLogsRead))
	body := decodeUsageJSON[envelope](t, rec)
	// 总消费 950 + 4500；10 个桶收敛为 9 项（前 8 + other）。
	if rec.Code != http.StatusOK || body.Total != 5450 || len(body.Items) != 9 {
		t.Fatalf("by-token other = %d total=%d items=%d body=%s", rec.Code, body.Total, len(body.Items), rec.Body.String())
	}

	noToken := body.Items[0]
	if noToken.TokenID != 0 || noToken.TokenName != "" || noToken.Cost != 950 ||
		noToken.Requests != 1 || noToken.Tokens != 10 {
		t.Fatalf("no-token bucket = %+v", noToken)
	}
	for i := 1; i <= 7; i++ {
		got := body.Items[i]
		want := fmt.Sprintf("tk-%d", i)
		if got.TokenID != tokenIDs[i-1] || got.TokenName != want || got.Cost != costs[i-1] ||
			got.Requests != 1 || got.Tokens != int64(2*i) {
			t.Fatalf("rank %d item = %+v, want name=%s cost=%d", i+1, got, want, costs[i-1])
		}
	}
	other := body.Items[8]
	if other.TokenID != 0 || other.TokenName != "other" || other.Cost != 300 ||
		other.Requests != 2 || other.Tokens != 34 {
		t.Fatalf("other bucket = %+v", other)
	}
}

// --- 失败排障（Task 5） ---------------------------------------------------

const (
	triageRefundedOther = `{
		"admin_info": {
			"triage": {
				"endpoint": "/v1/messages",
				"channel_type": "openai",
				"upstream_status": 400,
				"attempts": 1,
				"request_structure": "valid",
				"conversion": [{"stage": "request", "from": "anthropic", "to": "openai"}]
			}
		}
	}`
	triageRefundedChain = `[{"attempt":"1/1","channel_id":7,"upstream":"openai","error_class":"upstream_4xx","latency_ms":120,"upstream_status":400}]`
)

type triageJSON struct {
	Found     bool   `json:"found"`
	RequestID string `json:"request_id"`
	Log       *struct {
		Status       string           `json:"status"`
		ErrorMessage *string          `json:"error_message"`
		RetryChain   []map[string]any `json:"retry_chain"`
		Other        map[string]any   `json:"other"`
	} `json:"log"`
	Evidence *struct {
		Endpoint         string           `json:"endpoint"`
		RouteFrom        string           `json:"route_from"`
		RouteTo          string           `json:"route_to"`
		Conversion       []map[string]any `json:"conversion"`
		UpstreamStatus   int              `json:"upstream_status"`
		Attempts         int              `json:"attempts"`
		ChannelType      string           `json:"channel_type"`
		ChannelID        *int64           `json:"channel_id"`
		RequestStructure string           `json:"request_structure"`
		RejectStage      string           `json:"reject_stage"`
		Billing          map[string]any   `json:"billing"`
		ErrorClass       string           `json:"error_class"`
		CreatedAt        string           `json:"created_at"`
	} `json:"evidence"`
	Verdict *struct {
		Owner   string `json:"owner"`
		Sub     string `json:"sub"`
		Rule    string `json:"rule"`
		Signals []struct {
			Code   string         `json:"code"`
			Vote   string         `json:"vote"`
			Params map[string]any `json:"params"`
		} `json:"signals"`
	} `json:"verdict"`
}

func (env *usageTestEnv) triage(t *testing.T, requestID string, p *auth.Principal) (*httptest.ResponseRecorder, triageJSON) {
	t.Helper()
	rec := env.do(t, http.MethodGet, "/api/usage-logs/triage/"+requestID, p)
	return rec, decodeUsageJSON[triageJSON](t, rec)
}

type triageRecentBody struct {
	Items []struct {
		RequestID string `json:"request_id"`
		Status    string `json:"status"`
		Owner     string `json:"owner"`
		Sub       string `json:"sub"`
		Rule      string `json:"rule"`
		CreatedAt string `json:"created_at"`
	} `json:"items"`
	Counts struct {
		User    int `json:"user"`
		Channel int `json:"channel"`
		Gateway int `json:"gateway"`
		Unknown int `json:"unknown"`
		Total   int `json:"total"`
	} `json:"counts"`
}

func (env *usageTestEnv) recentFailures(t *testing.T, query string, p *auth.Principal) (*httptest.ResponseRecorder, triageRecentBody) {
	t.Helper()
	rec := env.do(t, http.MethodGet, "/api/usage-logs/triage/recent?"+query, p)
	return rec, decodeUsageJSON[triageRecentBody](t, rec)
}

func TestIntegrationRequestTriageDetail(t *testing.T) {
	env := newUsageTestEnv(t)
	userID, _ := ensureUsageTestUser(t, env, "tr")
	requestID := newRequestID(t)
	now := time.Now().UTC()
	// 终态行比 success 行更旧：只按「最新一行」会选错，必须按终态优先级。
	env.seed(t, usageSeed{
		UserID: userID, RequestID: requestID,
		Status: billing.UsageLogStatusRefunded, ChannelID: 7,
		Other: triageRefundedOther, RetryChain: triageRefundedChain,
		ErrorClass: "upstream_4xx", CreatedAt: now.Add(-2 * time.Minute),
	})
	env.seed(t, usageSeed{
		UserID: userID, RequestID: requestID,
		Status: billing.UsageLogStatusSuccess, CreatedAt: now.Add(-time.Minute),
	})

	rec, body := env.triage(t, requestID, usageAdminPrincipal(1, rbac.PermLogsRead))
	if rec.Code != http.StatusOK || !body.Found {
		t.Fatalf("triage status/found = %d/%v body=%s", rec.Code, body.Found, rec.Body.String())
	}
	if body.RequestID != requestID {
		t.Fatalf("request_id = %q, want %q", body.RequestID, requestID)
	}
	if body.Log == nil || body.Log.Status != "refunded" {
		t.Fatalf("log = %+v, want refunded terminal row", body.Log)
	}
	if body.Log.Other["admin_info"] == nil {
		t.Fatalf("admin response missing admin_info: %v", body.Log.Other)
	}
	if body.Verdict == nil {
		t.Fatalf("missing verdict: %s", rec.Body.String())
	}
	if body.Verdict.Owner != "gateway" || body.Verdict.Rule != "GW-01" || body.Verdict.Sub != "protocol_conversion" {
		t.Fatalf("verdict = %+v, want gateway/GW-01/protocol_conversion", *body.Verdict)
	}
	if len(body.Verdict.Signals) < 2 {
		t.Fatalf("signals = %d, want >= 2", len(body.Verdict.Signals))
	}
	ev := body.Evidence
	if ev == nil {
		t.Fatalf("missing evidence: %s", rec.Body.String())
	}
	if ev.Endpoint != "/v1/messages" || ev.ChannelType != "openai" || ev.ChannelID == nil || *ev.ChannelID != 7 {
		t.Fatalf("evidence endpoint/channel = %+v", ev)
	}
	if ev.RouteFrom != "anthropic" || ev.RouteTo != "openai" || len(ev.Conversion) != 1 {
		t.Fatalf("evidence conversion = %+v", ev)
	}
	if ev.UpstreamStatus != 400 || ev.Attempts != 1 {
		t.Fatalf("evidence status/attempts = %d/%d", ev.UpstreamStatus, ev.Attempts)
	}
	if ev.RequestStructure != "valid" || ev.RejectStage != "" {
		t.Fatalf("evidence structure/stage = %q/%q", ev.RequestStructure, ev.RejectStage)
	}
	if ev.ErrorClass != "upstream_4xx" || ev.Billing["cost"] == nil {
		t.Fatalf("evidence class/billing = %+v", ev)
	}

	// 32 位无连字符拼写必须命中同一行（$1::uuid 绑定）。
	compact := strings.ReplaceAll(requestID, "-", "")
	rec, compactBody := env.triage(t, compact, usageAdminPrincipal(1, rbac.PermLogsRead))
	if rec.Code != http.StatusOK || !compactBody.Found || compactBody.RequestID != requestID {
		t.Fatalf("compact uuid = %d found=%v id=%q", rec.Code, compactBody.Found, compactBody.RequestID)
	}

	// 非管理员：403，响应体不得含 admin_info。
	rec, _ = env.triage(t, requestID, userPrincipal(userID))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin status = %d, want 403", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "admin_info") {
		t.Fatalf("non-admin body leaked admin_info: %s", rec.Body.String())
	}
	// admin 但缺 logs:read：同样 403（不收敛成本人范围）。
	rec, _ = env.triage(t, requestID, usageAdminPrincipal(1, rbac.PermUsersRead))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("admin without logs:read status = %d, want 403", rec.Code)
	}

	// 不存在的 UUID：200 + found=false，不是 404。
	unknown := newRequestID(t)
	rec, missing := env.triage(t, unknown, usageAdminPrincipal(1, rbac.PermLogsRead))
	if rec.Code != http.StatusOK || missing.Found {
		t.Fatalf("unknown uuid = %d found=%v, want 200/false", rec.Code, missing.Found)
	}
	if missing.Log != nil || missing.Evidence != nil || missing.Verdict != nil {
		t.Fatalf("unknown uuid leaked payload: %s", rec.Body.String())
	}

	// 非法 id：422（不是 404）。
	for _, malformed := range []string{"not-a-uuid", "1234", strings.ReplaceAll(requestID, "-", "") + "00"} {
		rec, _ = env.triage(t, malformed, usageAdminPrincipal(1, rbac.PermLogsRead))
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("malformed id %q status = %d, want 422 body=%s", malformed, rec.Code, rec.Body.String())
		}
	}
}

func TestIntegrationRequestTriageRefusal(t *testing.T) {
	env := newUsageTestEnv(t)
	userID, _ := ensureUsageTestUser(t, env, "tf")
	requestID := newRequestID(t)
	env.seed(t, usageSeed{
		UserID: userID, RequestID: requestID,
		Status: billing.UsageLogStatusRefused,
		Other:  `{"admin_info":{"triage":{"stage":"quota","endpoint":"/v1/chat/completions","request_structure":"valid"}}}`,
	})

	rec, body := env.triage(t, requestID, usageAdminPrincipal(1, rbac.PermLogsRead))
	if rec.Code != http.StatusOK || !body.Found {
		t.Fatalf("refusal triage = %d found=%v body=%s", rec.Code, body.Found, rec.Body.String())
	}
	if body.Log == nil || body.Log.Status != "refused" {
		t.Fatalf("refusal log = %+v", body.Log)
	}
	if body.Verdict == nil || body.Verdict.Owner != "user" || body.Verdict.Rule != "USR-02" || body.Verdict.Sub != "quota" {
		t.Fatalf("refusal verdict = %+v, want user/USR-02/quota", body.Verdict)
	}
	if body.Evidence == nil || body.Evidence.RejectStage != "quota" || body.Evidence.UpstreamStatus != 0 || body.Evidence.Attempts != 0 {
		t.Fatalf("refusal evidence = %+v", body.Evidence)
	}
	var refusedSignal bool
	for _, sig := range body.Verdict.Signals {
		if sig.Code == "refused_before_upstream" && sig.Vote == "user" {
			if stage, _ := sig.Params["stage"].(string); stage == "quota" {
				refusedSignal = true
			}
		}
	}
	if !refusedSignal {
		t.Fatalf("missing refused_before_upstream quota signal: %+v", body.Verdict.Signals)
	}
}

func TestIntegrationRecentFailures(t *testing.T) {
	env := newUsageTestEnv(t)
	userID, _ := ensureUsageTestUser(t, env, "tl")
	at := time.Now().UTC().Add(-72 * time.Hour).Truncate(time.Second)

	gatewayID := newRequestID(t)
	env.seed(t, usageSeed{
		UserID: userID, RequestID: gatewayID, Status: billing.UsageLogStatusRefunded,
		ChannelID: 7, Other: triageRefundedOther, RetryChain: triageRefundedChain,
		CreatedAt: at,
	})
	userReqID := newRequestID(t)
	env.seed(t, usageSeed{
		UserID: userID, RequestID: userReqID, Status: billing.UsageLogStatusRefused,
		Other:     `{"admin_info":{"triage":{"stage":"quota"}}}`,
		CreatedAt: at.Add(time.Second),
	})
	// 同一窄窗口内的成功行不得出现在「最近失败」里。
	env.seed(t, usageSeed{
		UserID: userID, Status: billing.UsageLogStatusSuccess, CreatedAt: at.Add(2 * time.Second),
	})

	start := at.Add(-time.Minute).Format(time.RFC3339)
	end := at.Add(time.Minute).Format(time.RFC3339)
	window := "start=" + start + "&end=" + end

	type recentBody struct {
		Items []struct {
			RequestID string `json:"request_id"`
			Status    string `json:"status"`
			Owner     string `json:"owner"`
			Sub       string `json:"sub"`
			Rule      string `json:"rule"`
			Model     string `json:"model"`
			CreatedAt string `json:"created_at"`
		} `json:"items"`
		Counts struct {
			User    int `json:"user"`
			Channel int `json:"channel"`
			Gateway int `json:"gateway"`
			Unknown int `json:"unknown"`
			Total   int `json:"total"`
		} `json:"counts"`
		Window struct {
			Start string `json:"start"`
			End   string `json:"end"`
		} `json:"window"`
	}

	rec := env.do(t, http.MethodGet, "/api/usage-logs/triage/recent?"+window, usageAdminPrincipal(1, rbac.PermLogsRead))
	body := decodeUsageJSON[recentBody](t, rec)
	if rec.Code != http.StatusOK {
		t.Fatalf("recent status = %d body=%s", rec.Code, rec.Body.String())
	}
	if len(body.Items) != 2 {
		t.Fatalf("recent items = %d, want 2 (success excluded) body=%s", len(body.Items), rec.Body.String())
	}
	if body.Counts.Total != len(body.Items) || body.Counts.Gateway != 1 || body.Counts.User != 1 || body.Counts.Channel != 0 || body.Counts.Unknown != 0 {
		t.Fatalf("recent counts = %+v, want total=2 gateway=1 user=1", body.Counts)
	}
	if body.Window.Start == "" || body.Window.End == "" {
		t.Fatalf("recent window missing: %+v", body.Window)
	}
	byID := map[string]string{}
	for _, item := range body.Items {
		byID[item.RequestID] = item.Owner + "/" + item.Rule
	}
	if byID[gatewayID] != "gateway/GW-01" || byID[userReqID] != "user/USR-02" {
		t.Fatalf("recent verdicts = %v", byID)
	}

	// owner 过滤只影响返回条目与 counts。
	rec = env.do(t, http.MethodGet, "/api/usage-logs/triage/recent?owner=gateway&"+window, usageAdminPrincipal(1, rbac.PermLogsRead))
	filtered := decodeUsageJSON[recentBody](t, rec)
	if rec.Code != http.StatusOK || len(filtered.Items) != 1 || filtered.Items[0].RequestID != gatewayID {
		t.Fatalf("gateway filter = %d %+v", rec.Code, filtered.Items)
	}
	if filtered.Counts.Total != 1 || filtered.Counts.Gateway != 1 {
		t.Fatalf("gateway filter counts = %+v", filtered.Counts)
	}
	rec = env.do(t, http.MethodGet, "/api/usage-logs/triage/recent?owner=channel&"+window, usageAdminPrincipal(1, rbac.PermLogsRead))
	empty := decodeUsageJSON[recentBody](t, rec)
	if rec.Code != http.StatusOK || len(empty.Items) != 0 || empty.Counts.Total != 0 {
		t.Fatalf("channel filter = %d items=%d counts=%+v", rec.Code, len(empty.Items), empty.Counts)
	}

	// 参数校验：非法 limit/owner/range 都是 422。
	for _, query := range []string{"limit=51", "limit=0", "limit=abc", "owner=ops"} {
		rec = env.do(t, http.MethodGet, "/api/usage-logs/triage/recent?"+query, usageAdminPrincipal(1, rbac.PermLogsRead))
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("recent %s status = %d, want 422 body=%s", query, rec.Code, rec.Body.String())
		}
	}
	rec = env.do(t, http.MethodGet, "/api/usage-logs/triage/recent?range=1y", usageAdminPrincipal(1, rbac.PermLogsRead))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("recent bad range status = %d, want 422", rec.Code)
	}

	// 非管理员 / 无 logs:read → 403。
	rec = env.do(t, http.MethodGet, "/api/usage-logs/triage/recent?"+window, userPrincipal(userID))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("recent non-admin status = %d, want 403", rec.Code)
	}
	rec = env.do(t, http.MethodGet, "/api/usage-logs/triage/recent?"+window, usageAdminPrincipal(1, rbac.PermUsersRead))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("recent admin without logs:read status = %d, want 403", rec.Code)
	}
}

// 缺少 admin_info.triage 的历史行（含 90 天保留期内的旧数据）必须仍然
// 下发 evidence，且 conversion 序列化为 [] 而不是 null——前端按
// TriageConversionStep[] 直接 map。
func TestIntegrationRequestTriageMissingTriage(t *testing.T) {
	env := newUsageTestEnv(t)
	userID, _ := ensureUsageTestUser(t, env, "tn")
	requestID := newRequestID(t)
	env.seed(t, usageSeed{
		UserID: userID, RequestID: requestID,
		Status: billing.UsageLogStatusError,
		Other:  `{"admin_info":{"route":"/v1/messages"}}`,
	})

	rec, body := env.triage(t, requestID, usageAdminPrincipal(1, rbac.PermLogsRead))
	if rec.Code != http.StatusOK || !body.Found {
		t.Fatalf("triage = %d found=%v body=%s", rec.Code, body.Found, rec.Body.String())
	}
	if body.Evidence == nil {
		t.Fatalf("missing evidence: %s", rec.Body.String())
	}
	if body.Evidence.Conversion == nil || len(body.Evidence.Conversion) != 0 {
		t.Fatalf("conversion = %#v, want empty non-null array", body.Evidence.Conversion)
	}
	raw := rec.Body.String()
	if strings.Contains(raw, `"conversion":null`) {
		t.Fatalf("conversion serialised as null: %s", raw)
	}
	if !strings.Contains(raw, `"conversion":[]`) {
		t.Fatalf("conversion not serialised as []: %s", raw)
	}
}

// limit 小于窗口内失败行数时，counts 只统计本次返回的 items，total 等于
// len(items)；顺带验证 status=2 渲染为 "error"。
func TestIntegrationRecentFailuresLimitBelowRows(t *testing.T) {
	env := newUsageTestEnv(t)
	userID, _ := ensureUsageTestUser(t, env, "tq")
	at := time.Now().UTC().Add(-72 * time.Hour).Truncate(time.Second)
	for i := 0; i < 3; i++ {
		env.seed(t, usageSeed{
			UserID:    userID,
			Status:    billing.UsageLogStatusError,
			CreatedAt: at.Add(time.Duration(i) * time.Minute),
		})
	}
	window := "start=" + at.Add(-time.Minute).Format(time.RFC3339) +
		"&end=" + at.Add(5*time.Minute).Format(time.RFC3339)

	rec, body := env.recentFailures(t, window+"&limit=1", usageAdminPrincipal(1, rbac.PermLogsRead))
	if rec.Code != http.StatusOK {
		t.Fatalf("recent status = %d body=%s", rec.Code, rec.Body.String())
	}
	if len(body.Items) != 1 {
		t.Fatalf("items = %d, want 1 body=%s", len(body.Items), rec.Body.String())
	}
	if body.Items[0].Status != "error" {
		t.Fatalf("item status = %q, want error", body.Items[0].Status)
	}
	if body.Counts.Total != len(body.Items) {
		t.Fatalf("counts.total = %d, want %d", body.Counts.Total, len(body.Items))
	}
	if sum := body.Counts.User + body.Counts.Channel + body.Counts.Gateway + body.Counts.Unknown; sum != len(body.Items) {
		t.Fatalf("counts sum = %d, want %d (counts=%+v)", sum, len(body.Items), body.Counts)
	}
}

// 同一 request_id 有重复行时，列表与详情必须用同一终态优先级挑行：
// 更旧的 refunded 行胜过更新的 error 行，两个端点都报 refunded/gateway。
func TestIntegrationRecentFailuresDuplicateRows(t *testing.T) {
	env := newUsageTestEnv(t)
	userID, _ := ensureUsageTestUser(t, env, "td")
	requestID := newRequestID(t)
	at := time.Now().UTC().Add(-72 * time.Hour).Truncate(time.Second)
	env.seed(t, usageSeed{
		UserID: userID, RequestID: requestID,
		Status: billing.UsageLogStatusRefunded, ChannelID: 7,
		Other: triageRefundedOther, RetryChain: triageRefundedChain,
		CreatedAt: at,
	})
	// 更新的一行是 error：按「最新一行」会得到不同判定。
	env.seed(t, usageSeed{
		UserID: userID, RequestID: requestID,
		Status:    billing.UsageLogStatusError,
		CreatedAt: at.Add(time.Minute),
	})
	window := "start=" + at.Add(-time.Minute).Format(time.RFC3339) +
		"&end=" + at.Add(2*time.Minute).Format(time.RFC3339)

	rec, body := env.recentFailures(t, window, usageAdminPrincipal(1, rbac.PermLogsRead))
	if rec.Code != http.StatusOK || len(body.Items) != 1 {
		t.Fatalf("recent = %d items=%d body=%s", rec.Code, len(body.Items), rec.Body.String())
	}
	item := body.Items[0]
	if item.Status != "refunded" || item.Owner != "gateway" || item.Rule != "GW-01" {
		t.Fatalf("recent item = %+v, want refunded/gateway/GW-01", item)
	}

	rec, detail := env.triage(t, requestID, usageAdminPrincipal(1, rbac.PermLogsRead))
	if rec.Code != http.StatusOK || !detail.Found || detail.Log == nil {
		t.Fatalf("detail = %d found=%v body=%s", rec.Code, detail.Found, rec.Body.String())
	}
	if detail.Log.Status != item.Status || detail.Verdict == nil ||
		detail.Verdict.Owner != item.Owner || detail.Verdict.Rule != item.Rule {
		t.Fatalf("detail verdict = %+v (log=%s), want match recent item %+v", detail.Verdict, detail.Log.Status, item)
	}
}

// --- 折扣账单字段（Task 10） ----------------------------------------------

// discountFieldsItem 只解出本任务新增的三个顶层字段；其余契约由既有用例覆盖。
type discountFieldsItem struct {
	ID              int64    `json:"id"`
	OriginalCost    *int64   `json:"original_cost"`
	AppliedDiscount *float64 `json:"applied_discount"`
	DiscountTarget  *string  `json:"discount_target"`
}

// TestUsageLogDiscountFieldsEndToEnd 覆盖 list / detail / export 三处投影
// 一致，并验证两条易错分支：applied_discount=0 是「全免」而非缺键；
// JSON 数字 100.0 必须经 ::numeric 中转才能落成 bigint（修正 3/4）。
func TestUsageLogDiscountFieldsEndToEnd(t *testing.T) {
	env := newUsageTestEnv(t)
	userID, _ := ensureUsageTestUser(t, env, "dsc")
	p := userPrincipal(userID)

	const other = `{"original_cost":100,"applied_discount":0.9,"discount_target":"user:7"}`
	id := env.seed(t, usageSeed{
		UserID: userID, Model: "gpt-discount", Status: usageStatusSuccess,
		Cost: 90, Other: other,
	})

	assertDiscount := func(t *testing.T, label string, item discountFieldsItem) {
		t.Helper()
		if item.OriginalCost == nil || *item.OriginalCost != 100 {
			t.Fatalf("%s original_cost = %v, want 100", label, item.OriginalCost)
		}
		if item.AppliedDiscount == nil || *item.AppliedDiscount != 0.9 {
			t.Fatalf("%s applied_discount = %v, want 0.9", label, item.AppliedDiscount)
		}
		if item.DiscountTarget == nil || *item.DiscountTarget != "user:7" {
			t.Fatalf("%s discount_target = %v, want user:7", label, item.DiscountTarget)
		}
	}

	// 列表：三字段在顶层；列表响应仍不携带 other。
	rec := env.do(t, http.MethodGet, "/api/usage-logs", p)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d body=%s", rec.Code, rec.Body.String())
	}
	listRaw := decodeUsageJSON[struct {
		Items []map[string]json.RawMessage `json:"items"`
	}](t, rec)
	if len(listRaw.Items) != 1 {
		t.Fatalf("list items = %d, want 1 body=%s", len(listRaw.Items), rec.Body.String())
	}
	if _, ok := listRaw.Items[0]["other"]; ok {
		t.Fatalf("list must not expose other: %v", listRaw.Items[0])
	}
	var listItem discountFieldsItem
	raw, err := json.Marshal(listRaw.Items[0])
	if err != nil {
		t.Fatalf("re-encode list item: %v", err)
	}
	if err := json.Unmarshal(raw, &listItem); err != nil {
		t.Fatalf("decode list item: %v", err)
	}
	assertDiscount(t, "list", listItem)

	// 详情：与列表一致。
	rec = env.do(t, http.MethodGet, fmt.Sprintf("/api/usage-logs/%d", id), p)
	if rec.Code != http.StatusOK {
		t.Fatalf("detail status = %d body=%s", rec.Code, rec.Body.String())
	}
	assertDiscount(t, "detail", decodeUsageJSON[discountFieldsItem](t, rec))

	// 导出：三列追加在末尾，数值原样、文本列照常转义。
	rec = env.do(t, http.MethodGet, "/api/usage-logs/export?scope=filtered&format=txt&range=24h", p)
	if rec.Code != http.StatusOK {
		t.Fatalf("export status = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "\terror_message\toriginal_cost\tapplied_discount\tdiscount_target\n") {
		t.Fatalf("export header missing trailing discount columns: %s", body)
	}
	lines := strings.Split(strings.TrimSpace(body), "\n")
	dataLine := lines[len(lines)-1]
	if !strings.HasSuffix(dataLine, "\t100\t0.9\tuser:7") {
		t.Fatalf("export row discount cells = %q, want suffix 100/0.9/user:7", dataLine)
	}

	// 全免行：只有 applied_discount=0 与 target，没有 original_cost。
	waivedID := env.seed(t, usageSeed{
		UserID: userID, Model: "gpt-waived", Status: usageStatusSuccess,
		Other: `{"applied_discount":0,"discount_target":"group:vip"}`,
	})
	// 小数形式：jsonb ->> 返回 "100.0"，直接 ::bigint 会 500。
	decimalID := env.seed(t, usageSeed{
		UserID: userID, Model: "gpt-decimal", Status: usageStatusSuccess,
		Other: `{"original_cost":100.0,"applied_discount":0.75,"discount_target":"user:7"}`,
	})
	rec = env.do(t, http.MethodGet, "/api/usage-logs", p)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d body=%s", rec.Code, rec.Body.String())
	}
	list := decodeUsageJSON[struct {
		Items []discountFieldsItem `json:"items"`
	}](t, rec)
	byID := make(map[int64]discountFieldsItem, len(list.Items))
	for _, item := range list.Items {
		byID[item.ID] = item
	}
	waived := byID[waivedID]
	if waived.OriginalCost != nil {
		t.Fatalf("waived original_cost = %v, want null", waived.OriginalCost)
	}
	if waived.AppliedDiscount == nil || *waived.AppliedDiscount != 0 {
		t.Fatalf("waived applied_discount = %v, want non-nil 0", waived.AppliedDiscount)
	}
	if waived.DiscountTarget == nil || *waived.DiscountTarget != "group:vip" {
		t.Fatalf("waived discount_target = %v, want group:vip", waived.DiscountTarget)
	}
	decimal := byID[decimalID]
	if decimal.OriginalCost == nil || *decimal.OriginalCost != 100 {
		t.Fatalf("decimal original_cost = %v, want 100", decimal.OriginalCost)
	}
}

// TestUsageLogDiscountFieldsTolerateDirtyValues：`other` 是通用 jsonb 侧信道，
// 任何写入方塞进非数值的 original_cost / applied_discount 都不能让整页 500。
// 正则守卫把脏值退化成 NULL（与缺键同义），同一页的合法行照常返回。
func TestUsageLogDiscountFieldsTolerateDirtyValues(t *testing.T) {
	env := newUsageTestEnv(t)
	userID, _ := ensureUsageTestUser(t, env, "dirty")
	p := userPrincipal(userID)

	dirtyID := env.seed(t, usageSeed{
		UserID: userID, Model: "gpt-dirty", Status: usageStatusSuccess,
		Other: `{"original_cost":"oops","applied_discount":{},"discount_target":"user:7"}`,
	})
	// 同页的合法行必须不受影响：失败被限制在列级，而不是整行或整页。
	cleanID := env.seed(t, usageSeed{
		UserID: userID, Model: "gpt-clean", Status: usageStatusSuccess,
		Cost: 90, Other: `{"original_cost":100,"applied_discount":0,"discount_target":"group:vip"}`,
	})

	rec := env.do(t, http.MethodGet, "/api/usage-logs", p)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200 body=%s", rec.Code, rec.Body.String())
	}
	list := decodeUsageJSON[struct {
		Items []discountFieldsItem `json:"items"`
	}](t, rec)
	byID := make(map[int64]discountFieldsItem, len(list.Items))
	for _, item := range list.Items {
		byID[item.ID] = item
	}
	dirty, found := byID[dirtyID]
	if !found {
		t.Fatalf("dirty row missing from list: %s", rec.Body.String())
	}
	if dirty.OriginalCost != nil || dirty.AppliedDiscount != nil {
		t.Fatalf("dirty discount fields = %v/%v, want both null",
			dirty.OriginalCost, dirty.AppliedDiscount)
	}
	if dirty.DiscountTarget == nil || *dirty.DiscountTarget != "user:7" {
		t.Fatalf("dirty discount_target = %v, want user:7", dirty.DiscountTarget)
	}
	clean, found := byID[cleanID]
	if !found {
		t.Fatalf("clean row missing from list: %s", rec.Body.String())
	}
	if clean.OriginalCost == nil || *clean.OriginalCost != 100 {
		t.Fatalf("clean original_cost = %v, want 100", clean.OriginalCost)
	}
	if clean.AppliedDiscount == nil || *clean.AppliedDiscount != 0 {
		// 0 是「全免」，守卫不能把它当成缺键。
		t.Fatalf("clean applied_discount = %v, want non-nil 0", clean.AppliedDiscount)
	}

	// 详情与导出共用同一条列清单，必须一起存活。
	rec = env.do(t, http.MethodGet, fmt.Sprintf("/api/usage-logs/%d", dirtyID), p)
	if rec.Code != http.StatusOK {
		t.Fatalf("detail status = %d body=%s", rec.Code, rec.Body.String())
	}
	rec = env.do(t, http.MethodGet, "/api/usage-logs/export?scope=filtered&format=txt&range=24h", p)
	if rec.Code != http.StatusOK {
		t.Fatalf("export status = %d body=%s", rec.Code, rec.Body.String())
	}
}

// 日志库句柄缺失必须是 503（可重试），绝不能塌缩成 500。
func TestIntegrationRequestTriageLogDBUnavailable(t *testing.T) {
	h := &UsageLogHandlers{log: slog.New(slog.NewTextHandler(io.Discard, nil)), now: time.Now}
	mux := http.NewServeMux()
	h.RegisterUsageLogRoutes(mux, nil)
	for _, path := range []string{
		"/api/usage-logs/triage/" + newRequestID(t),
		"/api/usage-logs/triage/recent",
	} {
		req := httptest.NewRequest(http.MethodGet, path, http.NoBody)
		req = req.WithContext(auth.WithPrincipal(req.Context(), usageAdminPrincipal(1, rbac.PermLogsRead)))
		rec := serve(mux, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s status = %d, want 503 body=%s", path, rec.Code, rec.Body.String())
		}
	}
}
