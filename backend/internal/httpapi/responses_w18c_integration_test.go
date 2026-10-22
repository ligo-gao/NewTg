//go:build integration

package httpapi

// W18-C 集成验收（真实 PostgreSQL + Redis）：
//   - protocols 派生绑定下 create 返回 Responses 对象；
//   - 任务只落库一次、结算一次、usage_logs 恰一行；
//   - 重复 retrieve 不触发任何新的结算/流水。
//
// 运行：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_w18c?sslmode=disable' \
//	NEWTG_TEST_LOGS_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_w18c_logs?sslmode=disable' \
//	NEWTG_TEST_REDIS_ADDR=localhost:56379 \
//	  go test -tags=integration -p 1 ./internal/httpapi -run TestIntegrationResponsesW18C -count=1

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/billing"
	"newtg/gateway/internal/billing/pricing"
	"newtg/gateway/internal/tasks"
)

var responsesW18CSeq atomic.Int64

// responsesW18CIntegrationSource 与单测源同形，但 taskData 带一个可检索产物：
// 这样 retrieve 面有内容可回放，能证明「重复 retrieve 不重复结算」。
const responsesW18CIntegrationSource = `globalThis.__NEWTG_PLUGIN__ = {
  apiVersion: 1,
  key: "video",
  name: "Video",
  version: "1.2.0",
  author: { name: "NewTG" },
  description: { en: "Video task plugin", zh: "视频任务插件" },
  models: ["video-model"],
  fetchMode: "per_task",
  auth: { type: "api_key" },
  channelTypes: [],
  routes: [],
  protocols: { openai_responses: { supports: ["create", "retrieve"] } },
  usageSchema: {
    seconds: { type: "number", unit: "second", description: { en: "Video generation unit price", zh: "视频生成单价" } }
  },
  usageProfiles: [],
  usageExamples: [{ seconds: 5 }],
  requiredCapabilities: [],
  decodeRequest: function (ctx) { return ctx.body; },
  buildSubmitRequest: function (ctx) {
    return { url: ctx.baseUrl + "/tasks", method: "POST", body: { model: ctx.upstreamModel } };
  },
  parseSubmitResponse: function () {
    return {
      taskId: "up_w18c",
      immediate: { status: "SUCCESS" },
      taskData: {
        usage: { input_tokens: 5, output_tokens: 7 },
        artifacts: [{ key: "v1", type: "video", mimeType: "video/mp4" }]
      }
    };
  },
  buildQueryRequest: function (ctx) { return { url: ctx.baseUrl + "/t/" + ctx.taskId, method: "GET" }; },
  parseTaskResult: function () { return { state: "SUCCESS" }; },
  renderFinal: function () {
    return {
      output: [{ type: "message", role: "assistant", content: [{ type: "output_text", text: "hello" }] }],
      usage: { input_tokens: 5, output_tokens: 7 }
    };
  },
  renderEvents: function () {
    return [{ type: "response.output_text.delta", delta: "hello", output_index: 0, content_index: 0 }];
  }
};`

type responsesW18CIntegrationEnv struct {
	mux    http.Handler
	mainDB *sql.DB
	logsDB *sql.DB
	userID int64
}

func newResponsesW18CIntegrationEnv(t *testing.T) *responsesW18CIntegrationEnv {
	t.Helper()
	dsn := os.Getenv("NEWTG_TEST_DB_DSN")
	logsDSN := os.Getenv("NEWTG_TEST_LOGS_DSN")
	redisAddr := os.Getenv("NEWTG_TEST_REDIS_ADDR")
	if dsn == "" || logsDSN == "" || redisAddr == "" {
		t.Skip("NEWTG_TEST_DB_DSN / NEWTG_TEST_LOGS_DSN / NEWTG_TEST_REDIS_ADDR 未设置，跳过 W18-C 集成测试")
	}
	mainDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open main db: %v", err)
	}
	t.Cleanup(func() { _ = mainDB.Close() })
	if err := mainDB.Ping(); err != nil {
		t.Fatalf("ping main db: %v", err)
	}
	logsDB, err := sql.Open("pgx", logsDSN)
	if err != nil {
		t.Fatalf("open logs db: %v", err)
	}
	t.Cleanup(func() { _ = logsDB.Close() })
	if err := logsDB.Ping(); err != nil {
		t.Fatalf("ping logs db: %v", err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: redisAddr})
	t.Cleanup(func() { _ = rdb.Close() })
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("ping redis: %v", err)
	}
	gormDB, err := gorm.Open(postgres.New(postgres.Config{Conn: mainDB}), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		t.Fatalf("open gorm: %v", err)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"up_w18c"}`))
	}))
	t.Cleanup(upstream.Close)

	sealer := NewSecretSealer("task-handler-test-key")
	sealed, err := sealer.Seal([]byte(`{"api_key":"sk-test"}`))
	if err != nil {
		t.Fatalf("seal channel credentials: %v", err)
	}

	// 独立用户与令牌：断言按 user_id 精确计数，不与共享库里的历史行混合。
	if _, err := mainDB.Exec(`INSERT INTO user_groups (name, ratio, user_selectable, is_system, status)
		VALUES ('default', 100, true, true, 1) ON CONFLICT (name) DO NOTHING`); err != nil {
		t.Fatalf("ensure default group: %v", err)
	}
	username := fmt.Sprintf("w18c%d%d", time.Now().UnixNano()%1_000_000, responsesW18CSeq.Add(1))
	var userID int64
	if err := mainDB.QueryRow(
		`INSERT INTO users (username, password_hash, balance, "group")
		 VALUES ($1, 'x', 1000000, 'default') RETURNING id`, username).Scan(&userID); err != nil {
		t.Fatalf("create test user: %v", err)
	}
	var tokenID int64
	if err := mainDB.QueryRow(
		`INSERT INTO api_tokens (user_id, name, key_hash, "group", remain_quota)
		 VALUES ($1, 'w18c-test', $2, 'default', NULL) RETURNING id`,
		userID, fmt.Sprintf("hash-%d-%d", time.Now().UnixNano(), responsesW18CSeq.Add(1))).Scan(&tokenID); err != nil {
		t.Fatalf("create test token: %v", err)
	}
	var channelID int64
	if err := mainDB.QueryRow(
		`INSERT INTO channels (name, type, base_url, credentials, supported_models, task_plugin_key, status)
		 VALUES ($1, 'plugin', $2, $3, '["video-model"]'::jsonb, 'video', 1) RETURNING id`,
		fmt.Sprintf("w18c-channel-%d", responsesW18CSeq.Add(1)), upstream.URL, sealed).Scan(&channelID); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	t.Cleanup(func() {
		_, _ = mainDB.Exec(`DELETE FROM tasks WHERE user_id = $1`, userID)
		_, _ = mainDB.Exec(`DELETE FROM wallet_transactions WHERE user_id = $1`, userID)
		_, _ = logsDB.Exec(`DELETE FROM usage_logs WHERE user_id = $1`, userID)
		_, _ = mainDB.Exec(`DELETE FROM api_tokens WHERE user_id = $1`, userID)
		_, _ = mainDB.Exec(`DELETE FROM users WHERE id = $1`, userID)
		_, _ = mainDB.Exec(`DELETE FROM channels WHERE id = $1`, channelID)
	})

	// 定价源必须在插入 model_pricing 之后构造（DBSource 有缓存）。
	if _, err := mainDB.Exec(
		`INSERT INTO model_pricing (model, mode, prompt_price, author, status)
		 VALUES ('video-model', 1, 100, 'w18c', 1)`); err != nil {
		t.Fatalf("insert model pricing: %v", err)
	}
	t.Cleanup(func() {
		_, _ = mainDB.Exec(`DELETE FROM group_pricing WHERE model = 'video-model'`)
		_, _ = mainDB.Exec(`DELETE FROM model_pricing WHERE model = 'video-model' AND author = 'w18c'`)
	})

	source, err := pricing.NewSource(gormDB, nil)
	if err != nil {
		t.Fatalf("pricing.NewSource: %v", err)
	}
	engine := billing.NewEngine(gormDB, logsDB, rdb, relayBillingTestConfig(), source,
		billing.NewRegistry(rdb), billing.NewDBLogger(logsDB))

	manager := newTaskManager(t, map[string]string{"video": responsesW18CIntegrationSource})
	repo := tasks.NewRepo(mainDB)
	tokens := &fakeTaskTokens{meta: &auth.TokenMeta{
		ID: tokenID, UserID: userID, Status: "enabled", Group: "default", UnlimitedQuota: true,
	}}
	taskHandler := NewTasksHandler(TasksHandlerConfig{
		Store:                repo,
		Channels:             repo,
		Plugins:              manager,
		Sealer:               sealer,
		Tokens:               tokens,
		Billing:              engine.NewSession,
		ArtifactSigner:       tasks.NewArtifactSigner("task-handler-test-key", 0),
		AllowPrivateUpstream: true,
	})
	relayHandler := NewRelayHandler(RelayHandlerOptions{
		Channels:     &fakeRelayChannelStore{models: []string{"gpt-test"}},
		Tokens:       tokens,
		Access:       &fakeRelayAccess{role: "admin", status: "enabled"},
		Sealer:       sealer,
		NativeRoutes: taskHandler,
	})
	mux := http.NewServeMux()
	relayHandler.Register(mux)
	taskHandler.RegisterRelay(mux, relayHandler.AuthMiddleware())

	return &responsesW18CIntegrationEnv{mux: mux, mainDB: mainDB, logsDB: logsDB, userID: userID}
}

func (e *responsesW18CIntegrationEnv) counts(t *testing.T) (tasksCount, usageLogs int) {
	t.Helper()
	if err := e.mainDB.QueryRow(`SELECT count(*) FROM tasks WHERE user_id = $1`, e.userID).Scan(&tasksCount); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if err := e.logsDB.QueryRow(`SELECT count(*) FROM usage_logs WHERE user_id = $1`, e.userID).Scan(&usageLogs); err != nil {
		t.Fatalf("count usage_logs: %v", err)
	}
	return tasksCount, usageLogs
}

func TestIntegrationResponsesW18CCreateSettlesOnce(t *testing.T) {
	env := newResponsesW18CIntegrationEnv(t)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"video-model","input":"hello","seconds":5}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+relayTestToken)
	rec := httptest.NewRecorder()
	env.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("create status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var object struct {
		ID     string `json:"id"`
		Object string `json:"object"`
		Status string `json:"status"`
		Output []any  `json:"output"`
		Usage  map[string]any
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &object); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, rec.Body.String())
	}
	if object.Object != "response" || !strings.HasPrefix(object.ID, "resp_") {
		t.Fatalf("Responses object shape wrong: %+v", object)
	}
	if object.Status != "completed" || len(object.Output) != 1 {
		t.Fatalf("terminal object wrong: %+v", object)
	}

	tasksCount, usageLogs := env.counts(t)
	if tasksCount != 1 {
		t.Fatalf("tasks rows = %d, want 1 (persist exactly once)", tasksCount)
	}
	if usageLogs != 1 {
		t.Fatalf("usage_logs = %d, want 1 (settle exactly once)", usageLogs)
	}

	// 重复 retrieve 只读任务行，不得产生新的结算/流水。
	for i := 0; i < 2; i++ {
		get := httptest.NewRequest(http.MethodGet, "/v1/responses/"+object.ID, nil)
		get.Header.Set("Authorization", "Bearer "+relayTestToken)
		getRec := httptest.NewRecorder()
		env.mux.ServeHTTP(getRec, get)
		if getRec.Code != http.StatusOK {
			t.Fatalf("retrieve #%d status = %d, want 200; body=%s", i, getRec.Code, getRec.Body.String())
		}
	}
	tasksCount, usageLogs = env.counts(t)
	if tasksCount != 1 {
		t.Fatalf("tasks rows after retrieve = %d, want 1", tasksCount)
	}
	if usageLogs != 1 {
		t.Fatalf("usage_logs after retrieve = %d, want 1 (retrieve must not settle again)", usageLogs)
	}
}
