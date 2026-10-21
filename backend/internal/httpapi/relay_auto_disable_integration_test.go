//go:build integration

package httpapi

// W7/M4-a：真实 relay → 生产 Reporter → 真实 PostgreSQL 状态迁移的端到端验收。
//
// 现有 reporter 集成测试直接调用 ReportAttemptFailure，覆盖不到「执行层接缝
// 是否真的把失败事实送到 reporter」。本用例走完整 HTTP 端点：上游 401 →
// ExecuteAttempts 回调 → Reporter 异步条件 UPDATE → channels.status=3。
//
// 运行：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_m4?sslmode=disable' \
//	  go test -tags=integration -p 1 ./internal/httpapi/ -run TestRelayFailureAutoDisablesChannel -v

import (
	"context"
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

	_ "github.com/jackc/pgx/v5/stdlib"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/channelhealth"
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/relay/billingseam"
	"newtg/gateway/internal/store"
)

// autoDisableConfig 是 channelhealth.SettingsSource 的集成测试实现。
type autoDisableConfig map[string]json.RawMessage

func (m autoDisableConfig) Get(key string) (json.RawMessage, bool) {
	value, ok := m[key]
	return value, ok
}

func TestRelayFailureAutoDisablesChannelEndToEnd(t *testing.T) {
	dsn := os.Getenv("NEWTG_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("NEWTG_TEST_DB_DSN 未设置，跳过 relay→reporter 端到端集成测试")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping db: %v", err)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"invalid api key"}}`)
	}))
	t.Cleanup(upstream.Close)

	sealer := NewSecretSealer(relaySealerMaster)
	baseURL := upstream.URL
	name := fmt.Sprintf("m4a-e2e-%d", time.Now().UnixNano())
	var channelID int64
	err = db.QueryRow(`
		INSERT INTO channels (name, type, base_url, credentials, supported_models,
			model_mapping, weight, priority, status, auto_ban)
		VALUES ($1, 'openai', $2, $3, '["gpt-test"]'::jsonb, '{}'::jsonb, 1, 10, 1, true)
		RETURNING id`,
		name, baseURL, sealedTestCredentials(t, sealer)).Scan(&channelID)
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM channels WHERE id = $1`, channelID) })

	repo := store.NewChannelRepo(db)
	reporter, err := channelhealth.NewReporter(channelhealth.ReporterConfig{
		Store:  repo,
		Config: autoDisableConfig{channelhealth.KeyAutoDisableEnabled: json.RawMessage(`true`)},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewReporter: %v", err)
	}
	reporterCtx, stopReporter := context.WithCancel(context.Background())
	reporter.Start(reporterCtx)
	t.Cleanup(func() {
		stopReporter()
		reporter.Wait()
	})

	handler := NewRelayHandler(RelayHandlerOptions{
		Channels: repo,
		Tokens: newFakeRelayTokenStore(relayTestToken, &auth.TokenMeta{
			ID:             relayTestTokenID,
			UserID:         relayTestUserID,
			Status:         "enabled",
			UnlimitedQuota: true,
		}),
		Access:  &fakeRelayAccess{role: "admin", status: "enabled"},
		Sealer:  sealer,
		Client:  upstream.Client(),
		Billing: func(*contract.RelayContext) contract.BillingSession { return billingseam.NewStub() },
		// 生产接缝：与 platform 装配相同的注入方式。
		AttemptFailureReporter: reporter,
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-test","messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+relayTestToken)
	recorder := httptest.NewRecorder()
	handler.Handler().ServeHTTP(recorder, req)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", recorder.Code, recorder.Body.String())
	}

	// 异步副作用：轮询到条件更新落地（最多 5s）。
	deadline := time.Now().Add(5 * time.Second)
	for {
		channel, err := repo.Get(context.Background(), channelID)
		if err != nil {
			t.Fatalf("Get channel: %v", err)
		}
		if channel.Status == 3 {
			if channel.AutoDisabledReason == nil ||
				*channel.AutoDisabledReason != channelhealth.ReasonAuthError {
				t.Fatalf("reason = %v, want %s", channel.AutoDisabledReason, channelhealth.ReasonAuthError)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("渠道未被自动禁用（status=%d）；删掉 routeDeps.OnAttemptFailure 应导致本用例超时", channel.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
