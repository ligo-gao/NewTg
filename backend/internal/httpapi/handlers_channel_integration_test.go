//go:build integration

package httpapi

// W7/M4-c：R-M4-16 的真实 PostgreSQL 验收。
//
// fake store 用例只能证明 handler 把 patch 传下去；本文件直接对真库断言
// PATCH 之后 channels.auto_disabled_reason 的值，覆盖两条人工迁移：
//   - auto_disabled → manual_disabled：必须清空 reason；
//   - auto_disabled → enabled：继续清空 reason 与 fail_count（既有行为）。
//
// 运行：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_m4?sslmode=disable' \
//	  go test -tags=integration -p 1 ./internal/httpapi/ -run TestChannelPatchClearsAutoDisabledReasonIntegration -v

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"newtg/gateway/internal/channelhealth"
	"newtg/gateway/internal/store"
)

func openChannelPatchTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("NEWTG_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("NEWTG_TEST_DB_DSN 未设置，跳过渠道 PATCH 集成测试")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("ping test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// insertAutoDisabledChannel 直插一行 status=3 的渠道（绕过 handler create，
// 因为创建路径恒为 enabled，无法覆盖自动禁用后的 PATCH）。
func insertAutoDisabledChannel(t *testing.T, db *sql.DB, sealer *SecretSealer, reason string, failCount int32) int64 {
	t.Helper()
	name := fmt.Sprintf("m4c-patch-%d", time.Now().UnixNano())
	var id int64
	err := db.QueryRow(`
		INSERT INTO channels (name, type, base_url, credentials, supported_models,
			model_mapping, weight, priority, status, auto_ban, fail_count, auto_disabled_reason)
		VALUES ($1, 'openai', 'https://upstream.example.com', $2, '["gpt-4o"]'::jsonb,
			'{}'::jsonb, 1, 0, 3, true, $3, $4)
		RETURNING id`,
		name, mustSealCredentials(t, sealer, ChannelCredentials{APIKey: "sk-m4c-1234"}), failCount, reason,
	).Scan(&id)
	if err != nil {
		t.Fatalf("insert auto-disabled channel: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM channels WHERE id = $1`, id) })
	return id
}

// channelPatchRow 读回落库值；reason NULL 时 valid=false。
func channelPatchRow(t *testing.T, db *sql.DB, id int64) (status int16, reason sql.NullString, failCount int32) {
	t.Helper()
	if err := db.QueryRow(`
		SELECT status, auto_disabled_reason, fail_count FROM channels WHERE id = $1`, id).
		Scan(&status, &reason, &failCount); err != nil {
		t.Fatalf("read channel %d: %v", id, err)
	}
	return status, reason, failCount
}

func getChannelVersion(t *testing.T, mux *http.ServeMux, id int64) string {
	t.Helper()
	rr := serveWithPrincipal(t, mux,
		httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/channels/%d", id), nil),
		channelAdminPrincipal())
	if rr.Code != http.StatusOK {
		t.Fatalf("get channel %d = %d（body=%s）", id, rr.Code, rr.Body.String())
	}
	var resp ChannelResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode channel: %v", err)
	}
	return resp.Version
}

func patchChannelStatus(t *testing.T, mux *http.ServeMux, id int64, status int16) *httptest.ResponseRecorder {
	t.Helper()
	body := map[string]any{
		"expected_version": getChannelVersion(t, mux, id),
		"status":           status,
	}
	rr := serveWithPrincipal(t, mux,
		jsonReq(t, http.MethodPatch, fmt.Sprintf("/api/channels/%d", id), body),
		channelAdminPrincipal())
	if rr.Code != http.StatusOK {
		t.Fatalf("patch status=%d = %d（body=%s）", status, rr.Code, rr.Body.String())
	}
	return rr
}

func TestChannelPatchClearsAutoDisabledReasonIntegration(t *testing.T) {
	db := openChannelPatchTestDB(t)
	sealer := NewSecretSealer(testSealerMaster)
	h := NewChannelHandler(store.NewChannelRepo(db), newFakeUserAccess(), sealer,
		ChannelHandlerOptions{Idempotency: NewMemoryIdempotencyStore(), Egress: testEgressGuard()})
	mux := http.NewServeMux()
	h.Register(mux)

	t.Run("auto_disabled_to_manual_disabled_clears_reason", func(t *testing.T) {
		id := insertAutoDisabledChannel(t, db, sealer, channelhealth.ReasonAuthError, 2)
		rr := patchChannelStatus(t, mux, id, 2)

		var resp ChannelResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode patch response: %v", err)
		}
		if resp.AutoDisabledReason != nil {
			t.Fatalf("响应 auto_disabled_reason = %v，期望 null（R-M4-16）", *resp.AutoDisabledReason)
		}
		status, reason, failCount := channelPatchRow(t, db, id)
		if status != 2 {
			t.Fatalf("落库 status = %d，期望 2", status)
		}
		if reason.Valid {
			t.Fatalf("落库 auto_disabled_reason = %q，期望 NULL（R-M4-16）", reason.String)
		}
		// R-M4-16 只清 reason；fail_count 不在人工禁用路径清零（R-M4-04）。
		if failCount != 2 {
			t.Fatalf("落库 fail_count = %d，期望 2（不得顺手清零）", failCount)
		}
	})

	t.Run("auto_disabled_to_enabled_clears_reason_and_fail_count", func(t *testing.T) {
		id := insertAutoDisabledChannel(t, db, sealer, channelhealth.ReasonKeyword, 5)
		patchChannelStatus(t, mux, id, 1)

		status, reason, failCount := channelPatchRow(t, db, id)
		if status != 1 {
			t.Fatalf("落库 status = %d，期望 1", status)
		}
		if reason.Valid {
			t.Fatalf("落库 auto_disabled_reason = %q，期望 NULL", reason.String)
		}
		if failCount != 0 {
			t.Fatalf("落库 fail_count = %d，期望 0（F-06-08）", failCount)
		}
	})
}
