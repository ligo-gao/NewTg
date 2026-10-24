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
			error_message, retry_chain, other, created_at)
		VALUES ($1::uuid,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20::jsonb,$21::jsonb,$22)
		RETURNING id`,
		s.RequestID, s.UserID, tokenID, tokenName, channel, upstream, s.Model, s.Group,
		s.PromptTokens, s.Completion, pre, s.Cost, s.Refunded, latency, first,
		s.Stream, s.Status, errClass, errMessage, retry, s.Other, s.CreatedAt,
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
	env.seed(t, usageSeed{UserID: userA, Status: 1, Cost: 100, PreDeducted: &pre1, Refunded: 20, PromptTokens: 10, Completion: 20})
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
	}

	rec := env.do(t, http.MethodGet, "/api/usage-stats/summary", userPrincipal(userA))
	got := decodeUsageJSON[summary](t, rec)
	want := summary{150, 4, 0.5, 16, 22, 230, 80}
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
	for i := 0; i < 9; i++ {
		env.seed(t, usageSeed{UserID: userID, Model: fmt.Sprintf("model-%d", i), Cost: 10, PromptTokens: 1, Completion: 1})
	}
	rec := env.do(t, http.MethodGet, "/api/usage-stats/by-model", userPrincipal(userID))
	body := decodeUsageJSON[[]struct {
		Model   string  `json:"model"`
		Cost    int64   `json:"cost"`
		Tokens  int64   `json:"tokens"`
		Percent float64 `json:"percent"`
	}](t, rec)
	if rec.Code != http.StatusOK {
		t.Fatalf("by-model status = %d body=%s", rec.Code, rec.Body.String())
	}
	if len(body) != 9 {
		t.Fatalf("by-model rows = %d, want top 8 + other = 9", len(body))
	}
	if body[8].Model != "other" || body[8].Cost != 10 {
		t.Fatalf("other bucket = %+v", body[8])
	}
	var sum float64
	for _, item := range body {
		sum += item.Percent
	}
	if sum < 99.999 || sum > 100.001 {
		t.Fatalf("percent sum = %v, want 100", sum)
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
