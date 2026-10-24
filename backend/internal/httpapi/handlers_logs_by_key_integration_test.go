//go:build integration

package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/tokenstore"
)

// GET /api/logs/by-key（modules-05 F-05-11 / D2-01..08）的集成验收。
//
// 运行：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_d2?sslmode=disable' \
//	NEWTG_TEST_LOGS_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_d2_logs?sslmode=disable' \
//	NEWTG_TEST_REDIS_ADDR=localhost:56379 \
//	go test -tags=integration ./internal/httpapi/ -run TestIntegrationLogsByKey -v
//
// 与既有集成测试同口径：未设置 DSN 时跳过。

type logsByKeyEnv struct {
	mainDB *sql.DB
	logsDB *sql.DB
	rdb    *redis.Client
	router http.Handler
	userA  int64
	userB  int64
	tokens map[string]logsByKeyToken
	suffix string
	reqIDs []string
}

type logsByKeyToken struct {
	ID    int64
	Plain string
}

func newLogsByKeyEnv(t *testing.T) *logsByKeyEnv {
	t.Helper()
	mainDSN := os.Getenv("NEWTG_TEST_DB_DSN")
	logsDSN := os.Getenv("NEWTG_TEST_LOGS_DSN")
	redisAddr := os.Getenv("NEWTG_TEST_REDIS_ADDR")
	if mainDSN == "" || logsDSN == "" || redisAddr == "" {
		t.Skip("NEWTG_TEST_DB_DSN / NEWTG_TEST_LOGS_DSN / NEWTG_TEST_REDIS_ADDR 未设置，跳过集成测试")
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
		t.Fatalf("ping main db: %v", err)
	}
	if err := logsDB.PingContext(ctx); err != nil {
		t.Fatalf("ping logs db: %v", err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: redisAddr, MaxRetries: -1})
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Fatalf("ping redis: %v", err)
	}
	gormDB, err := gorm.Open(postgres.New(postgres.Config{Conn: mainDB}), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("gorm open: %v", err)
	}

	env := &logsByKeyEnv{
		mainDB: mainDB, logsDB: logsDB, rdb: rdb,
		tokens: make(map[string]logsByKeyToken),
		suffix: fmt.Sprintf("%d", time.Now().UnixNano()),
	}
	t.Cleanup(func() {
		for _, id := range env.reqIDs {
			_, _ = logsDB.Exec(`DELETE FROM usage_logs WHERE request_id = $1::uuid`, id)
		}
		for _, userID := range []int64{env.userA, env.userB} {
			if userID != 0 {
				_, _ = logsDB.Exec(`DELETE FROM usage_logs WHERE user_id = $1`, userID)
				_, _ = mainDB.Exec(`DELETE FROM api_tokens WHERE user_id = $1`, userID)
				_, _ = mainDB.Exec(`DELETE FROM users WHERE id = $1`, userID)
			}
		}
		_ = rdb.Close()
		_ = mainDB.Close()
		_ = logsDB.Close()
	})

	if _, err := mainDB.Exec(`INSERT INTO user_groups (name) VALUES ('default') ON CONFLICT (name) DO NOTHING`); err != nil {
		t.Fatalf("ensure default group: %v", err)
	}
	env.userA = seedLogsByKeyUser(t, mainDB, "lb_a_"+env.suffix)
	env.userB = seedLogsByKeyUser(t, mainDB, "lb_b_"+env.suffix)

	store := tokenstore.New(gormDB, rdb, nil)
	rt, err := NewRouter(RouterConfig{
		DB:            mainDB,
		LogDB:         logsDB,
		Redis:         rdb,
		Sessions:      auth.NewSessionManager(rdb, []byte("logs-by-key-test-secret")),
		SessionSecret: []byte("logs-by-key-test-secret"),
		TokenService:  store,
	})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	env.router = rt

	// 令牌矩阵：同用户双令牌、跨用户、过期、耗尽、禁用。
	env.addToken(t, "a1", env.userA, "enabled", nil, nil)
	env.addToken(t, "a2", env.userA, "enabled", nil, nil)
	env.addToken(t, "b1", env.userB, "enabled", nil, nil)
	env.addToken(t, "expired", env.userA, "enabled", nil, func() *time.Time {
		past := time.Now().UTC().Add(-24 * time.Hour)
		return &past
	}())
	env.addToken(t, "exhausted", env.userA, "enabled", byKeyInt64Ptr(0), nil)
	env.addToken(t, "disabled", env.userA, "disabled", nil, nil)
	return env
}

func byKeyInt64Ptr(v int64) *int64 { return &v }

func seedLogsByKeyUser(t *testing.T, db *sql.DB, username string) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow(`INSERT INTO users (username, "group") VALUES ($1, 'default') RETURNING id`, username).Scan(&id); err != nil {
		t.Fatalf("seed user %s: %v", username, err)
	}
	return id
}

func (e *logsByKeyEnv) addToken(t *testing.T, name string, userID int64, status string, remain *int64, expires *time.Time) {
	t.Helper()
	plain := "nkt-" + strings.Repeat(strings.ToLower(name), 1) + e.suffix
	// 补齐到合法长度即可：key_hash 的唯一约束才是真凭据，前缀形态由 bearerCredential 校验。
	createdAt := time.Now().UTC().Add(-48 * time.Hour)
	var createdAtArg any = createdAt
	if expires == nil {
		createdAtArg = time.Now().UTC()
	}
	var id int64
	err := e.mainDB.QueryRow(`
		INSERT INTO api_tokens (user_id, name, key_hash, status, expires_at, "group", remain_quota, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, 'default', $6, $7, now())
		RETURNING id`,
		userID, "by-key-"+name+"-"+e.suffix[:8], auth.HashTokenKey(plain), status, expires, remain, createdAtArg,
	).Scan(&id)
	if err != nil {
		t.Fatalf("insert token %s: %v", name, err)
	}
	e.tokens[name] = logsByKeyToken{ID: id, Plain: plain}
}

// insertLog 写入一条 usage_log。tokenID 为 nil 时代表账户级（token_id NULL）。
func (e *logsByKeyEnv) insertLog(t *testing.T, userID int64, tokenID *int64, model string, createdAt time.Time, other, retry string) string {
	t.Helper()
	if other == "" {
		other = "{}"
	}
	var retryArg any
	if retry != "" {
		retryArg = retry
	}
	var id string
	err := e.logsDB.QueryRow(`
		INSERT INTO usage_logs (request_id, user_id, token_id, token_name, model, "group",
			prompt_tokens, completion_tokens, cost, status, error_class, error_message,
			retry_chain, other, created_at)
		VALUES (gen_random_uuid(), $1, $2, $3, $4, 'default',
			10, 20, 30, 1, NULL, 'raw upstream failure',
			$5::jsonb, $6::jsonb, $7)
		RETURNING request_id::text`,
		userID, tokenID, "tok-"+model, model, retryArg, other, createdAt).Scan(&id)
	if err != nil {
		t.Fatalf("insert usage log: %v", err)
	}
	e.reqIDs = append(e.reqIDs, id)
	return id
}

func (e *logsByKeyEnv) get(t *testing.T, method, target string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	rr := httptest.NewRecorder()
	e.router.ServeHTTP(rr, req)
	return rr
}

func bearerFor(e *logsByKeyEnv, name string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + e.tokens[name].Plain}
}

type byKeyTestItem struct {
	ID           int64                        `json:"id"`
	RequestID    string                       `json:"request_id"`
	ErrorMessage *string                      `json:"error_message"`
	RetryChain   []map[string]json.RawMessage `json:"retry_chain"`
	Other        map[string]json.RawMessage   `json:"other"`
	CreatedAt    time.Time                    `json:"created_at"`
}

type byKeyTestPage struct {
	Items []byKeyTestItem `json:"items"`
}

func decodeByKeyPage(t *testing.T, rr *httptest.ResponseRecorder) byKeyTestPage {
	t.Helper()
	var page byKeyTestPage
	if err := json.Unmarshal(rr.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode response: %v body=%s", err, rr.Body.String())
	}
	return page
}

func TestIntegrationLogsByKey(t *testing.T) {
	env := newLogsByKeyEnv(t)
	a1 := env.tokens["a1"].ID
	base := time.Now().UTC().Add(-time.Hour)

	// 供 valid/isolation 共用的四条日志：A1 两条、A2 一条、B1 一条、账户级一条。
	otherRich := `{
		"keep":"visible",
		"admin_info":{"note":"admin only"},
		"root_info":{"note":"root only"},
		"audit_info":{"note":"audit only"},
		"channel_id":9,"channel_name":"secret-channel","channel_type":2,
		"reject_reason":"raw reject reason","error_message":"raw other error"
	}`
	retryRich := `[{"attempt":1,"channel_id":5,"channel_name":"c","upstream":"u",
		"decision":"retry","error_class":"upstream_5xx","latency_ms":3,
		"upstream_request_id":"req-1","priority":7,"weight":3,"use_group":"vip"}]`
	env.insertLog(t, env.userA, &a1, "gpt-old", base, otherRich, retryRich)
	env.insertLog(t, env.userA, &a1, "gpt-new", base.Add(10*time.Minute), "", "")
	a2 := env.tokens["a2"].ID
	env.insertLog(t, env.userA, &a2, "gpt-a2", base.Add(5*time.Minute), "", "")
	b1 := env.tokens["b1"].ID
	env.insertLog(t, env.userB, &b1, "gpt-b1", base.Add(5*time.Minute), "", "")
	env.insertLog(t, env.userA, nil, "gpt-account-level", base.Add(20*time.Minute), "", "")

	t.Run("valid_token_returns_own_rows_only", func(t *testing.T) {
		rr := env.get(t, http.MethodGet, "/api/logs/by-key", bearerFor(env, "a1"))
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d，期望 200（body=%s）", rr.Code, rr.Body.String())
		}
		page := decodeByKeyPage(t, rr)
		if len(page.Items) != 2 {
			t.Fatalf("items = %d，期望 2（token 级：A2/账户级/B1 均不可见）", len(page.Items))
		}
		if page.Items[0].RequestID == "" || page.Items[1].RequestID == "" {
			t.Fatal("request_id 缺失")
		}
		// created_at DESC：后写入的 gpt-new 在前。
		if page.Items[0].ID <= page.Items[1].ID {
			t.Fatalf("排序不是 created_at DESC, id DESC：%d, %d", page.Items[0].ID, page.Items[1].ID)
		}

		// 回包不得出现敏感/身份字段（username/key/key_masked/明文）。
		raw := rr.Body.String()
		for _, forbidden := range []string{"username", "key_masked", "\"key\"", env.tokens["a1"].Plain} {
			if strings.Contains(raw, forbidden) {
				t.Fatalf("回包出现禁用字段 %q：%s", forbidden, raw)
			}
		}

		// 富 other 行：user 投影必须剥掉特权分区与渠道/拒绝原因，并打码错误原文。
		rich := page.Items[1]
		for _, key := range []string{"admin_info", "root_info", "audit_info",
			"channel_id", "channel_name", "channel_type", "reject_reason"} {
			if _, ok := rich.Other[key]; ok {
				t.Fatalf("other 仍包含禁用键 %q: %s", key, string(byKeyJSON(t, rich.Other)))
			}
		}
		if string(rich.Other["keep"]) != `"visible"` {
			t.Fatalf("other.keep = %s，期望保留", rich.Other["keep"])
		}
		if string(rich.Other["error_message"]) != `"`+maskedSensitive+`"` {
			t.Fatalf("other.error_message = %s，期望打码", rich.Other["error_message"])
		}
		if rich.ErrorMessage == nil || *rich.ErrorMessage != maskedSensitive {
			t.Fatalf("error_message = %v，期望打码 %q", rich.ErrorMessage, maskedSensitive)
		}
		if len(rich.RetryChain) != 1 {
			t.Fatalf("retry_chain 长度 = %d，期望 1", len(rich.RetryChain))
		}
		wantKeys := map[string]bool{
			"attempt": true, "channel_id": true, "channel_name": true, "upstream": true,
			"decision": true, "error_class": true, "latency_ms": true, "upstream_request_id": true,
		}
		for key := range rich.RetryChain[0] {
			if !wantKeys[key] {
				t.Fatalf("retry_chain 泄露 admin 键 %q", key)
			}
		}
		if len(rich.RetryChain[0]) != len(wantKeys) {
			t.Fatalf("retry_chain user 视图键数 = %d，期望 %d", len(rich.RetryChain[0]), len(wantKeys))
		}
	})

	t.Run("expired_and_exhausted_are_readable", func(t *testing.T) {
		for _, name := range []string{"expired", "exhausted"} {
			rr := env.get(t, http.MethodGet, "/api/logs/by-key", bearerFor(env, name))
			if rr.Code != http.StatusOK {
				t.Fatalf("%s status = %d，期望 200（body=%s）", name, rr.Code, rr.Body.String())
			}
			if page := decodeByKeyPage(t, rr); page.Items == nil {
				t.Fatalf("%s 回包 items 必须是数组", name)
			}
		}
	})

	t.Run("disabled_is_401_invalid_token", func(t *testing.T) {
		rr := env.get(t, http.MethodGet, "/api/logs/by-key", bearerFor(env, "disabled"))
		assertInvalidToken(t, rr)
	})

	t.Run("credential_forms", func(t *testing.T) {
		plain := env.tokens["a1"].Plain
		cases := map[string]map[string]string{
			"no header":      {},
			"cookie only":    {"Cookie": auth.SessionCookieName + "=anything"},
			"query key":      {},
			"x-api-key":      {"X-Api-Key": plain},
			"unknown prefix": {"Authorization": "Bearer xsk-" + plain},
		}
		for name, headers := range cases {
			target := "/api/logs/by-key"
			if name == "query key" {
				target += "?key=" + plain
			}
			rr := env.get(t, http.MethodGet, target, headers)
			if rr.Code != http.StatusUnauthorized {
				t.Fatalf("%s status = %d，期望 401", name, rr.Code)
			}
		}
	})

	t.Run("token_isolation", func(t *testing.T) {
		rr := env.get(t, http.MethodGet, "/api/logs/by-key", bearerFor(env, "a2"))
		page := decodeByKeyPage(t, rr)
		if len(page.Items) != 1 {
			t.Fatalf("a2 items = %d，期望 1（看不到同用户 a1 的日志）", len(page.Items))
		}
		rr = env.get(t, http.MethodGet, "/api/logs/by-key", bearerFor(env, "b1"))
		page = decodeByKeyPage(t, rr)
		if len(page.Items) != 1 {
			t.Fatalf("b1 items = %d，期望 1", len(page.Items))
		}
	})

	t.Run("empty_is_200_array", func(t *testing.T) {
		env.addToken(t, "empty", env.userA, "enabled", nil, nil)
		rr := env.get(t, http.MethodGet, "/api/logs/by-key", bearerFor(env, "empty"))
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d，期望 200", rr.Code)
		}
		if strings.TrimSpace(rr.Body.String()) != `{"items":[]}` {
			t.Fatalf("空态回包 = %s，期望 {\"items\":[]}", rr.Body.String())
		}
	})

	t.Run("limit_1000", func(t *testing.T) {
		env.addToken(t, "many", env.userA, "enabled", nil, nil)
		many := env.tokens["many"].ID
		// 1005 行：多出的 5 行必须被 SQL LIMIT 截断，而不是靠 Go 侧截断。
		_, err := env.logsDB.Exec(`
			INSERT INTO usage_logs (request_id, user_id, token_id, token_name, model, "group", status, other, created_at)
			SELECT gen_random_uuid(), $1, $2, 'bulk', 'gpt-bulk', 'default', 1, '{}'::jsonb,
			       now() - (i || ' seconds')::interval
			FROM generate_series(1, 1005) AS i`, env.userA, many)
		if err != nil {
			t.Fatalf("bulk insert: %v", err)
		}
		rr := env.get(t, http.MethodGet, "/api/logs/by-key?limit=99999&page=2", bearerFor(env, "many"))
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d，期望 200", rr.Code)
		}
		page := decodeByKeyPage(t, rr)
		if len(page.Items) != logsByKeyLimit {
			t.Fatalf("items = %d，期望固定上限 %d（查询参数必须被忽略）", len(page.Items), logsByKeyLimit)
		}
		// 最新一行（i=1）必须在首位：created_at 必须严格非增。
		if page.Items[0].CreatedAt.Before(page.Items[len(page.Items)-1].CreatedAt) {
			t.Fatal("1000 条结果未按 created_at DESC 排序")
		}
		// 截断必须保留最近的 1000 条（i=1..1000），丢掉最旧的 5 条：
		// 末位应恰好是 SQL OFFSET 999 的那一行。
		var boundary time.Time
		if err := env.logsDB.QueryRow(`
			SELECT created_at FROM usage_logs WHERE user_id = $1 AND token_id = $2
			ORDER BY created_at DESC, id DESC LIMIT 1 OFFSET $3`,
			env.userA, many, logsByKeyLimit-1).Scan(&boundary); err != nil {
			t.Fatalf("read boundary row: %v", err)
		}
		if !page.Items[len(page.Items)-1].CreatedAt.Equal(boundary) {
			t.Fatalf("末位 created_at = %v，期望第 %d 新的行 %v",
				page.Items[len(page.Items)-1].CreatedAt, logsByKeyLimit, boundary)
		}
	})

	t.Run("rate_limit_61st_429", func(t *testing.T) {
		env.addToken(t, "limited", env.userA, "enabled", nil, nil)
		limited := env.tokens["limited"].ID
		_ = env.rdb.Del(context.Background(), logsByKeyRatePrefix+fmt.Sprint(limited)).Err()
		for i := 0; i < logsByKeyRateLimit; i++ {
			rr := env.get(t, http.MethodGet, "/api/logs/by-key", bearerFor(env, "limited"))
			if rr.Code != http.StatusOK {
				t.Fatalf("第 %d 次 status = %d，期望 200", i+1, rr.Code)
			}
		}
		rr := env.get(t, http.MethodGet, "/api/logs/by-key", bearerFor(env, "limited"))
		if rr.Code != http.StatusTooManyRequests {
			t.Fatalf("第 61 次 status = %d，期望 429（body=%s）", rr.Code, rr.Body.String())
		}
		if rr.Header().Get("Retry-After") == "" {
			t.Fatal("429 缺少 Retry-After")
		}
	})

	t.Run("post_is_405", func(t *testing.T) {
		rr := env.get(t, http.MethodPost, "/api/logs/by-key", bearerFor(env, "a1"))
		if rr.Code != http.StatusMethodNotAllowed {
			t.Fatalf("POST status = %d，期望 405", rr.Code)
		}
	})
}

func TestIntegrationLogsByKeyInfrastructureFailures(t *testing.T) {
	env := newLogsByKeyEnv(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Run("resolve_failure_is_503_and_sanitized", func(t *testing.T) {
		h := &UsageLogHandlers{
			logDB:    env.logsDB,
			readOnly: fakeReadOnlyLookup{err: errors.New("dial tcp 10.1.2.3:5432: connection refused")},
			log:      logger,
			now:      time.Now,
		}
		req := httptest.NewRequest(http.MethodGet, "/api/logs/by-key", nil)
		req.Header.Set("Authorization", "Bearer nkt-anything")
		rr := httptest.NewRecorder()
		h.handleLogsByKey(rr, req)
		if rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d，期望 503", rr.Code)
		}
		for _, leak := range []string{"10.1.2.3", "5432", "connection refused", "dial tcp"} {
			if strings.Contains(rr.Body.String(), leak) {
				t.Fatalf("503 响应泄露内部错误 %q：%s", leak, rr.Body.String())
			}
		}
	})

	t.Run("log_db_failure_is_503", func(t *testing.T) {
		closed := openClosedSQL(t)
		h := &UsageLogHandlers{
			logDB:    closed,
			readOnly: fakeReadOnlyLookup{meta: &auth.TokenMeta{ID: 1, UserID: 2, Status: "enabled"}},
			log:      logger,
			now:      time.Now,
		}
		req := httptest.NewRequest(http.MethodGet, "/api/logs/by-key", nil)
		req.Header.Set("Authorization", "Bearer nkt-anything")
		rr := httptest.NewRecorder()
		h.handleLogsByKey(rr, req)
		if rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d，期望 503（body=%s）", rr.Code, rr.Body.String())
		}
	})
}

type fakeReadOnlyLookup struct {
	meta *auth.TokenMeta
	err  error
}

func (f fakeReadOnlyLookup) ResolveReadOnly(context.Context, string) (*auth.TokenMeta, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.meta, nil
}

func openClosedSQL(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", "postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	db.SetMaxOpenConns(1)
	_ = db.Close()
	return db
}

func assertInvalidToken(t *testing.T, rr *httptest.ResponseRecorder) {
	t.Helper()
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d，期望 401（body=%s）", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "invalid_token") {
		t.Fatalf("401 必须是 invalid_token：%s", rr.Body.String())
	}
	// 禁用与不存在不得可区分：detail 必须为空。
	if strings.Contains(rr.Body.String(), "disabled") || strings.Contains(rr.Body.String(), "禁用") {
		t.Fatalf("401 泄露了禁用状态：%s", rr.Body.String())
	}
}

func byKeyJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}
