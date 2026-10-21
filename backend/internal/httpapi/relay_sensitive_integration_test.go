//go:build integration

package httpapi

// W9-sensitive 的数据库级验收：真实 PostgreSQL（配置中心快照 + 公告仓储），
// 上游仍用 httptest（任务书要求「真实 PG + httptest/fake 上游」）。
//
// 覆盖：设置真的从 system_settings 读入 → 中继 block 零计费/零上游；
// 设置热变更（action=log / 词表清空）后同一 handler 立即放行；公告
// create/update 在 SQL 仓储之前拦截（零写入），log 模式写入原文。
//
// 运行（隔离库，先 CREATE DATABASE + migrate；绝不复用共享 newtg_test）：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_w9s?sslmode=disable' \
//	NEWTG_TEST_LOGS_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_w9s_logs?sslmode=disable' \
//	  go test -tags=integration ./internal/httpapi/ -run TestIntegrationW9Sensitive -v -p 1

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/configcenter"
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/rbac"
	"newtg/gateway/internal/store"
)

func w9sDBs(t *testing.T) (*sql.DB, *sql.DB) {
	t.Helper()
	mainDSN := os.Getenv("NEWTG_TEST_DB_DSN")
	logsDSN := os.Getenv("NEWTG_TEST_LOGS_DSN")
	if mainDSN == "" || logsDSN == "" {
		t.Skip("NEWTG_TEST_DB_DSN / NEWTG_TEST_LOGS_DSN 未设置，跳过 W9-sensitive 集成测试")
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
	return open(mainDSN, "main db"), open(logsDSN, "logs db")
}

type w9sEnv struct {
	db     *sql.DB
	center *configcenter.Center
}

// newW9SEnv 装配真实 PG 配置中心（Redis 用 miniredis，避免跨用例污染）。
// 四个 sec.sensitive.* 键在用例结束时删除，保证隔离库可重复运行。
func newW9SEnv(t *testing.T) *w9sEnv {
	t.Helper()
	db, _ := w9sDBs(t)
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	center := configcenter.New(db, rdb)
	env := &w9sEnv{db: db, center: center}
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

func (e *w9sEnv) save(t *testing.T, key string, value any) {
	t.Helper()
	if err := e.center.Save(context.Background(), key, value, nil); err != nil {
		t.Fatalf("save %s: %v", key, err)
	}
}

func (e *w9sEnv) enable(t *testing.T, scope map[string]bool, words []string, action string) {
	t.Helper()
	e.save(t, "sec.sensitive.enabled", true)
	e.save(t, "sec.sensitive.scope", scope)
	e.save(t, "sec.sensitive.words", words)
	e.save(t, "sec.sensitive.action", action)
}

// ---------- 中继：真实 PG 设置 ----------

func TestIntegrationW9SensitiveRelayUsesSettings(t *testing.T) {
	env := newW9SEnv(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, sensitiveUpstreamBody)
	}))
	t.Cleanup(upstream.Close)

	relayEnv := newRelayTestEnv(t, upstream, nil)
	relayEnv.handler.config = env.center
	relayEnv.handler.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	// billingseam.Stub 是单会话实例（一个桩只服务一次 PreConsume），而本用例
	// 连续发三个请求；每次请求前换一个新桩，同时保持断言面不变。
	relayEnv.handler.billing = func(*contract.RelayContext) contract.BillingSession {
		return relayEnv.billing
	}
	resetBilling := func() { relayEnv.billing = newRecordingBilling() }

	env.enable(t, map[string]bool{"prompt": true, "system": false}, []string{"BadWord"}, "block")

	// block：真实 PG 的设置必须生效，且零 PreConsume / 零上游。
	resetBilling()
	w := relayEnv.doAuthed(t, http.MethodPost, "/v1/chat/completions",
		`{"model":"gpt-test","messages":[{"role":"user","content":"BADWORD"}]}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"code":"content_blocked"`) {
		t.Fatalf("block = %d %s, want 400 content_blocked", w.Code, w.Body.String())
	}
	if relayEnv.billing.preConsumeCalls() != 0 {
		t.Fatalf("block PreConsume = %d, want 0", relayEnv.billing.preConsumeCalls())
	}

	// 热变更 action=log：同一 handler（同一敏感词缓存）必须立即放行。
	env.save(t, "sec.sensitive.action", "log")
	resetBilling()
	w = relayEnv.doAuthed(t, http.MethodPost, "/v1/chat/completions",
		`{"model":"gpt-test","messages":[{"role":"user","content":"BADWORD"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("log = %d %s, want 200", w.Code, w.Body.String())
	}
	if relayEnv.billing.preConsumeCalls() != 1 {
		t.Fatalf("log PreConsume = %d, want 1", relayEnv.billing.preConsumeCalls())
	}

	// 词表清空（enabled 仍为 true）：设计口径「不会拦截任何内容」。
	env.save(t, "sec.sensitive.action", "block")
	env.save(t, "sec.sensitive.words", []string{})
	resetBilling()
	w = relayEnv.doAuthed(t, http.MethodPost, "/v1/chat/completions",
		`{"model":"gpt-test","messages":[{"role":"user","content":"BADWORD"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("empty words = %d %s, want 200", w.Code, w.Body.String())
	}
}

// ---------- 公告：真实 SQL 仓储 ----------

func TestIntegrationW9SensitiveAnnouncements(t *testing.T) {
	env := newW9SEnv(t)
	repo := store.NewAnnouncementRepo(env.db)
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	handlers, err := NewAnnouncementHandlers(AnnouncementHandlerConfig{
		Repo:   repo,
		Redis:  rdb,
		Authz:  rbac.NewAuthzMiddleware(),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config: env.center,
	})
	if err != nil {
		t.Fatalf("NewAnnouncementHandlers: %v", err)
	}
	mux := http.NewServeMux()
	handlers.Register(mux)
	adminID := time.Now().UnixNano()%1_000_000_000 + 3_000_000_000
	admin := announcementTestPrincipal(adminID, rbac.RoleAdmin, rbac.PermSettingsManage)

	count := func() int {
		t.Helper()
		var n int
		if err := env.db.QueryRow(`SELECT count(*) FROM announcements WHERE created_by = $1`, adminID).Scan(&n); err != nil {
			t.Fatalf("count announcements: %v", err)
		}
		return n
	}

	env.enable(t, map[string]bool{"prompt": true}, []string{"BadWord"}, "block")

	// block：422 且 SQL 零写入。
	before := count()
	w := doAnnouncementRequest(t, mux, admin, http.MethodPost, "/api/announcements",
		map[string]any{"title": "ok", "content": "has BadWord"})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("block create = %d, want 422; body=%s", w.Code, w.Body.String())
	}
	if after := count(); after != before {
		t.Fatalf("block 写入了 %d 行（before=%d）", after, before)
	}

	// log：写入原文（设计 action=log 只记日志放行）。
	env.save(t, "sec.sensitive.action", "log")
	w = doAnnouncementRequest(t, mux, admin, http.MethodPost, "/api/announcements",
		map[string]any{"title": "ok", "content": "has BadWord"})
	if w.Code != http.StatusCreated {
		t.Fatalf("log create = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	var view map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	id, _ := view["id"].(string)
	t.Cleanup(func() {
		_, _ = env.db.Exec(`DELETE FROM announcement_reads WHERE announcement_id = $1`, id)
		_, _ = env.db.Exec(`DELETE FROM announcements WHERE id = $1`, id)
	})
	var stored string
	if err := env.db.QueryRow(`SELECT content FROM announcements WHERE id = $1`, id).Scan(&stored); err != nil {
		t.Fatalf("read stored announcement: %v", err)
	}
	if stored != "has BadWord" {
		t.Fatalf("log 模式应写原文，got %q", stored)
	}

	// update block：SQL 行保持原值。
	env.save(t, "sec.sensitive.action", "block")
	beforeContent := stored
	w = doAnnouncementRequest(t, mux, admin, http.MethodPut, "/api/announcements/"+id,
		map[string]any{"title": "ok", "content": "badword"})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("block update = %d, want 422; body=%s", w.Code, w.Body.String())
	}
	if err := env.db.QueryRow(`SELECT content FROM announcements WHERE id = $1`, id).Scan(&stored); err != nil {
		t.Fatalf("re-read announcement: %v", err)
	}
	if stored != beforeContent {
		t.Fatalf("block update 改写了行：got %q want %q", stored, beforeContent)
	}
}
