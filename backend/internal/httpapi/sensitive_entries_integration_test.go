//go:build integration

package httpapi

// W14-SENSITIVE 的数据库级验收：真实 PostgreSQL（配置中心快照）+ Redis，
// 上游仍用 httptest / 捕获 dispatcher（任务书要求「真实 PG + fake 上游」）。
//
// 覆盖：四个 sec.sensitive.* 键从 system_settings 读入后，多模态/任务/视频/
// 批次入口的 block 路径零 PreConsume、零上游、零任务/批次行；log 与空词表
// 在同一条缓存上立即放行（快照哈希失效）。
//
// 运行（隔离库，先 CREATE DATABASE + migrate；绝不复用共享 newtg_test）：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_w14s?sslmode=disable' \
//	NEWTG_TEST_LOGS_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_w14s_logs?sslmode=disable' \
//	NEWTG_TEST_REDIS_ADDR='localhost:56379' \
//	  go test -tags=integration -p 1 ./internal/httpapi/ -run TestIntegrationW14Sensitive -v

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/configcenter"
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/tasks"
)

const (
	w14sMainDSNEnv  = "NEWTG_TEST_DB_DSN"
	w14sLogsDSNEnv  = "NEWTG_TEST_LOGS_DSN"
	w14sRedisEnvVar = "NEWTG_TEST_REDIS_ADDR"
)

type w14sEnv struct {
	db     *sql.DB
	center *configcenter.Center
}

// newW14SEnv 装配真实 PG 配置中心与真实 Redis（W11 同口径）。四个
// sec.sensitive.* 键在用例结束时删除，保证隔离库可重复运行。
func newW14SEnv(t *testing.T) *w14sEnv {
	t.Helper()
	mainDSN := os.Getenv(w14sMainDSNEnv)
	logsDSN := os.Getenv(w14sLogsDSNEnv)
	redisAddr := os.Getenv(w14sRedisEnvVar)
	if mainDSN == "" || logsDSN == "" || redisAddr == "" {
		t.Skip("NEWTG_TEST_DB_DSN / NEWTG_TEST_LOGS_DSN / NEWTG_TEST_REDIS_ADDR 未设置，跳过 W14-SENSITIVE 集成测试")
	}
	open := func(dsn, label string) *sql.DB {
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatalf("open %s: %v", label, err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := db.PingContext(ctx); err != nil {
			_ = db.Close()
			t.Fatalf("ping %s: %v", label, err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db
	}
	db := open(mainDSN, "main db")
	// 日志库只用于证明隔离 DSN 可达；本用例不写 audit/usage 行。
	logsDB := open(logsDSN, "logs db")
	_ = logsDB

	rdb := redis.NewClient(&redis.Options{Addr: redisAddr, MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("ping redis: %v", err)
	}
	center := configcenter.New(db, rdb)
	env := &w14sEnv{db: db, center: center}
	t.Cleanup(func() {
		for _, key := range []string{
			"sec.sensitive.enabled", "sec.sensitive.scope",
			"sec.sensitive.words", "sec.sensitive.action",
		} {
			_, _ = db.Exec(`DELETE FROM system_settings WHERE "key" = $1`, key)
		}
	})
	return env
}

func (e *w14sEnv) save(t *testing.T, key string, value any) {
	t.Helper()
	if err := e.center.Save(context.Background(), key, value, nil); err != nil {
		t.Fatalf("save %s: %v", key, err)
	}
}

func (e *w14sEnv) enable(t *testing.T, words []string, action string) {
	t.Helper()
	e.save(t, "sec.sensitive.enabled", true)
	e.save(t, "sec.sensitive.scope", map[string]bool{"prompt": true, "system": false})
	e.save(t, "sec.sensitive.words", words)
	e.save(t, "sec.sensitive.action", action)
}

// TestIntegrationW14SensitiveMultimodal 证明多模态入口读的是真实 PG 快照。
func TestIntegrationW14SensitiveMultimodal(t *testing.T) {
	env := newW14SEnv(t)
	env.enable(t, []string{"BadWord"}, "block")

	for _, tc := range sensitiveMultimodalCases(t, "BadWord") {
		t.Run(tc.name, func(t *testing.T) {
			dispatcher := &captureMultimodalDispatcher{}
			handler := newSensitiveMultimodalHandler(t, dispatcher, env.center)
			resp := doMultimodal(t, handler, tc.req, true)
			if resp.Code != http.StatusBadRequest ||
				!strings.Contains(resp.Body.String(), "content_blocked") {
				t.Fatalf("block = %d %s, want 400 content_blocked", resp.Code, resp.Body.String())
			}
			if dispatcher.calls != 0 {
				t.Fatalf("dispatcher calls = %d, want 0", dispatcher.calls)
			}
		})
	}

	// 热变更 action=log：同一条 Cache 必须按新快照放行。
	env.save(t, "sec.sensitive.action", "log")
	dispatcher := &captureMultimodalDispatcher{}
	handler := newSensitiveMultimodalHandler(t, dispatcher, env.center)
	resp := doMultimodal(t, handler, multimodalRequest{
		path: "/v1/embeddings", contentType: "application/json",
		body: []byte(`{"model":"gpt-test","input":"BadWord"}`),
	}, true)
	if resp.Code != http.StatusOK || dispatcher.calls != 1 {
		t.Fatalf("log 热变更 = %d calls=%d, want 200/1; body=%s",
			resp.Code, dispatcher.calls, resp.Body.String())
	}

	// 词表清空（开关仍开启）：设计口径「不会拦截任何内容」。
	env.save(t, "sec.sensitive.action", "block")
	env.save(t, "sec.sensitive.words", []string{})
	dispatcher = &captureMultimodalDispatcher{}
	handler = newSensitiveMultimodalHandler(t, dispatcher, env.center)
	resp = doMultimodal(t, handler, multimodalRequest{
		path: "/v1/embeddings", contentType: "application/json",
		body: []byte(`{"model":"gpt-test","input":"BadWord"}`),
	}, true)
	if resp.Code != http.StatusOK || dispatcher.calls != 1 {
		t.Fatalf("空词表 = %d calls=%d, want 200/1; body=%s",
			resp.Code, dispatcher.calls, resp.Body.String())
	}
}

// TestIntegrationW14SensitiveTaskEntries 用真实 PG 设置驱动任务面的三条入口。
func TestIntegrationW14SensitiveTaskEntries(t *testing.T) {
	env := newW14SEnv(t)
	env.enable(t, []string{"BadWord"}, "block")

	// /v1/tasks/{key}
	t.Run("tasks", func(t *testing.T) {
		upstream, hits := newCountingUpstream(t)
		sealer := NewSecretSealer("task-handler-test-key")
		store := &fakeTaskStore{}
		channels := &fakeTaskChannels{target: &tasks.ChannelTarget{
			ID: 1, BaseURL: upstream.URL, Credentials: sealedCredential(t, sealer, "sk"),
		}}
		billing := newTaskBillingRecorder()
		mux := sensitiveTaskMuxEnv(t, store, channels, sampleTaskManager(t),
			func(*contract.RelayContext) contract.BillingSession { return billing }, env.center)

		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, newTaskSubmitHTTPReq(
			`{"model":"sample-model","prompt":"BadWord","seconds":5}`))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "content_blocked") {
			t.Fatalf("tasks block = %d %s", rec.Code, rec.Body.String())
		}
		if store.inserted != 0 || perConsumeCount(billing) != 0 || hits.Load() != 0 {
			t.Fatalf("tasks 副作用：rows=%d preconsume=%d upstream=%d",
				store.inserted, perConsumeCount(billing), hits.Load())
		}
	})

	// /v1/videos
	t.Run("videos", func(t *testing.T) {
		upstream, hits := newCountingUpstream(t)
		sealer := NewSecretSealer("task-handler-test-key")
		store := &fakeTaskStore{}
		channels := &fakeTaskChannels{target: &tasks.ChannelTarget{
			ID: 9, BaseURL: upstream.URL, Credentials: sealedCredential(t, sealer, "sk"),
		}}
		billing := newTaskBillingRecorder()
		manager := newVideoManager(t, map[string]string{"vid": videoPluginSource("vid", "vid-model", false)})
		mux := sensitiveTaskMuxEnv(t, store, channels, manager,
			func(*contract.RelayContext) contract.BillingSession { return billing }, env.center)

		rec := postVideo(t, mux, `{"model":"vid-model","prompt":"BadWord","seconds":5}`)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "content_blocked") {
			t.Fatalf("videos block = %d %s", rec.Code, rec.Body.String())
		}
		if store.inserted != 0 || perConsumeCount(billing) != 0 || hits.Load() != 0 {
			t.Fatalf("videos 副作用：rows=%d preconsume=%d upstream=%d",
				store.inserted, perConsumeCount(billing), hits.Load())
		}
	})

	// /v1/batches（逐行扫描，任一行命中 → 整批 400、零落库）
	t.Run("batches", func(t *testing.T) {
		batchEnv, mux := newSensitiveBatchEnv(t, env.center)
		rec := postBatch(t, mux, batchBodyWithPrompts("clean", "BadWord in row two"))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "content_blocked") {
			t.Fatalf("batches block = %d %s", rec.Code, rec.Body.String())
		}
		if batchEnv.store.inserted != 0 || batchEnv.store.fakeTaskStore.inserted != 0 {
			t.Fatalf("batches 副作用：batch=%d tasks=%d",
				batchEnv.store.inserted, batchEnv.store.fakeTaskStore.inserted)
		}
		if calls := batchEnv.billing.snapshot(); len(calls) != 0 {
			t.Fatalf("batches billing calls = %v, want 空", calls)
		}
		if batchEnv.hits() != 0 {
			t.Fatalf("batches upstream = %d, want 0", batchEnv.hits())
		}
	})

	// log 模式放行（任务面按行/按请求各自记日志后继续）
	t.Run("log", func(t *testing.T) {
		env.save(t, "sec.sensitive.action", "log")
		upstream, hits := newCountingUpstream(t)
		sealer := NewSecretSealer("task-handler-test-key")
		store := &fakeTaskStore{}
		channels := &fakeTaskChannels{target: &tasks.ChannelTarget{
			ID: 1, BaseURL: upstream.URL, Credentials: sealedCredential(t, sealer, "sk"),
		}}
		billing := newTaskBillingRecorder()
		mux := sensitiveTaskMuxEnv(t, store, channels, sampleTaskManager(t),
			func(*contract.RelayContext) contract.BillingSession { return billing }, env.center)

		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, newTaskSubmitHTTPReq(
			`{"model":"sample-model","prompt":"BadWord","seconds":5}`))
		if rec.Code != http.StatusAccepted || store.inserted != 1 || hits.Load() != 1 {
			t.Fatalf("log = %d rows=%d upstream=%d; body=%s",
				rec.Code, store.inserted, hits.Load(), rec.Body.String())
		}
	})
}

// TestIntegrationW14SensitiveLogNoPlaintext 锁定命中日志不含明文词。
func TestIntegrationW14SensitiveLogNoPlaintext(t *testing.T) {
	env := newW14SEnv(t)
	env.enable(t, []string{"BadWord"}, "block")

	var logs strings.Builder
	dispatcher := &captureMultimodalDispatcher{}
	handler := newSensitiveMultimodalHandler(t, dispatcher, env.center)
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	// MultimodalHandler 使用 slog.Default；测试内把默认 logger 临时指向缓冲。
	previous := slog.Default()
	slog.SetDefault(logger)
	t.Cleanup(func() { slog.SetDefault(previous) })

	_ = doMultimodal(t, handler, multimodalRequest{
		path: "/v1/embeddings", contentType: "application/json",
		body: []byte(`{"model":"gpt-test","input":"hello BadWord"}`),
	}, true)

	output := logs.String()
	if !strings.Contains(output, "security.sensitive_word_hit") {
		t.Fatalf("缺少命中事件：%s", output)
	}
	if strings.Contains(strings.ToLower(output), "badword") {
		t.Fatalf("日志泄露命中词：%s", output)
	}
}
