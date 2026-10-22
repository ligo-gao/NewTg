//go:build integration

package httpapi

// W8/MJ-a2 提交路径的真实 PostgreSQL 验收（httptest 假上游 + fake 计费会话）。
//
// 覆盖：真实 tasks.Repo 写入 family='mj' 行、channel 锁定/origin ids、
// upstream_task_id/private_data 计费锚点、提交失败不落行。预扣/退款次数由
// fake 会话记录，真实资金语义在计费包测试中覆盖。
//
// 运行：NEWTG_TEST_DB_DSN=postgres://... go test -tags=integration ./internal/httpapi/

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/mj"
	"newtg/gateway/internal/tasks"
)

func openMJIntegrationDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("NEWTG_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("NEWTG_TEST_DB_DSN 未设置，跳过集成测试")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// insertMJIntegrationChannel 写一条真实 channels 行（凭据已按生产格式密封）。
func insertMJIntegrationChannel(t *testing.T, db *sql.DB, sealer *SecretSealer,
	name, typ, baseURL string, models []string) int64 {
	t.Helper()
	sealed, err := sealer.Seal([]byte(`{"api_key":"integration-channel-key"}`))
	if err != nil {
		t.Fatalf("seal credential: %v", err)
	}
	modelsJSON, _ := json.Marshal(models)
	var id int64
	if err := db.QueryRow(`
		INSERT INTO channels (name, type, base_url, credentials, supported_models,
			group_tags, model_mapping, weight, priority, status)
		VALUES ($1,$2,$3,$4,$5::jsonb,NULL,'{}'::jsonb,1,100,1)
		RETURNING id`, name, typ, baseURL, sealed, string(modelsJSON)).Scan(&id); err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	return id
}

func TestMJSubmitPersistsFamilyRowOnRealDB(t *testing.T) {
	db := openMJIntegrationDB(t)
	repo := tasks.NewRepo(db)
	sealer := NewSecretSealer("mj-integration-key")

	userID := time.Now().UnixNano() % 1_000_000
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM tasks WHERE user_id = $1`, userID)
	})
	// 清理历史失败用例残留的渠道，避免它们以更高优先级参与选择。
	_, _ = db.Exec(`DELETE FROM channels WHERE name LIKE 'mj-a2-int-%'`)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("mj-api-secret") != "integration-channel-key" {
			t.Errorf("upstream secret = %q", r.Header.Get("mj-api-secret"))
		}
		_, _ = w.Write([]byte(`{"code":1,"description":"ok","result":"mj_integration_1"}`))
	}))
	defer upstream.Close()

	channelID := insertMJIntegrationChannel(t, db, sealer,
		fmt.Sprintf("mj-a2-int-%d", userID), mj.ChannelTypeMidjourney, upstream.URL, mjAllModels())
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM channels WHERE id = $1`, channelID) })

	tokens := mjTokenLookup{
		auth.HashTokenKey("nkt-mj-int"): {
			ID: 1, UserID: userID, Status: "enabled", Group: "default", UnlimitedQuota: true,
		},
	}
	var recorder *taskBillingRecorder
	handler := NewMJHandler(MJHandlerConfig{
		Store: repo, Channels: repo, Sealer: sealer, Tokens: tokens,
		Billing: func(*contract.RelayContext) contract.BillingSession {
			recorder = newTaskBillingRecorder()
			recorder.hold = 70
			return recorder
		},
		Config:      mjConfigured(nil),
		ImageSigner: mj.NewImageSigner("mj-integration-key"),
		Uint64N:     func(uint64) uint64 { return 0 },
	})
	mux := http.NewServeMux()
	handler.Register(mux, RelayAuth(tokens, fakeTaskAccess{}, nil))

	req := httptest.NewRequest(http.MethodPost, "/mj/submit/imagine", strings.NewReader(`{"prompt":"a cat"}`))
	req.Header.Set("Authorization", "Bearer nkt-mj-int")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body=%s)", rec.Code, rec.Body.String())
	}

	row, err := repo.GetByUserUpstreamID(context.Background(), userID, "mj_integration_1")
	if err != nil {
		t.Fatalf("persisted row not found: %v", err)
	}
	if row.Family != tasks.FamilyMJ || row.PluginKey != "" || row.Action != string(mj.ActionImagine) {
		t.Fatalf("row family/action = %+v", row)
	}
	if row.ChannelID != channelID || row.LockedChannelID != 0 {
		t.Fatalf("row channel/locked = %d/%d", row.ChannelID, row.LockedChannelID)
	}
	if row.Quota != 70 {
		t.Fatalf("row quota = %d, want 70", row.Quota)
	}
	if row.PrivateData[mj.PrivateRequestID] == nil || row.PrivateData[mj.PrivateTokenID] == nil {
		t.Fatalf("row missing billing anchors: %+v", row.PrivateData)
	}
	if recorder == nil {
		t.Fatalf("billing session was not constructed")
	}
	calls := recorder.ReserveCalls()
	if countCall(calls, "preconsume") != 1 || countCall(calls, "detach") != 1 || countCall(calls, "refund") != 0 {
		t.Fatalf("billing calls = %v", calls)
	}
}

func TestMJSubmitUpstreamFailureDoesNotPersistOnRealDB(t *testing.T) {
	db := openMJIntegrationDB(t)
	repo := tasks.NewRepo(db)
	sealer := NewSecretSealer("mj-integration-key")
	userID := time.Now().UnixNano()%1_000_000 + 500_000
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM tasks WHERE user_id = $1`, userID) })
	_, _ = db.Exec(`DELETE FROM channels WHERE name LIKE 'mj-a2-int-%'`)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":3,"description":"internal account list"}`))
	}))
	defer upstream.Close()
	channelID := insertMJIntegrationChannel(t, db, sealer,
		fmt.Sprintf("mj-a2-int-fail-%d", userID), mj.ChannelTypeMidjourney, upstream.URL, mjAllModels())
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM channels WHERE id = $1`, channelID) })

	tokens := mjTokenLookup{
		auth.HashTokenKey("nkt-mj-int-fail"): {
			ID: 2, UserID: userID, Status: "enabled", Group: "default", UnlimitedQuota: true,
		},
	}
	var recorder *taskBillingRecorder
	handler := NewMJHandler(MJHandlerConfig{
		Store: repo, Channels: repo, Sealer: sealer, Tokens: tokens,
		Billing: func(*contract.RelayContext) contract.BillingSession {
			recorder = newTaskBillingRecorder()
			recorder.hold = 70
			return recorder
		},
		Config:      mjConfigured(nil),
		ImageSigner: mj.NewImageSigner("mj-integration-key"),
		Uint64N:     func(uint64) uint64 { return 0 },
	})
	mux := http.NewServeMux()
	handler.Register(mux, RelayAuth(tokens, fakeTaskAccess{}, nil))

	req := httptest.NewRequest(http.MethodPost, "/mj/submit/imagine", strings.NewReader(`{"prompt":"a cat"}`))
	req.Header.Set("Authorization", "Bearer nkt-mj-int-fail")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d (body=%s)", rec.Code, rec.Body.String())
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM tasks WHERE user_id = $1`, userID).Scan(&count); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if count != 0 {
		t.Fatalf("upstream failure persisted %d rows", count)
	}
	calls := recorder.ReserveCalls()
	if countCall(calls, "preconsume") != 1 || countCall(calls, "refund") != 1 {
		t.Fatalf("billing calls = %v", calls)
	}
}
