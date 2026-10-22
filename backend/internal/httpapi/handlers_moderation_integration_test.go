//go:build integration

package httpapi

// W18-D 审核端点（POST /v1/moderations）的端到端集成验收（真实 PostgreSQL
// 主库 + 日志库 + Redis + 真实计费引擎）：
//
//	1. 成功请求只计费一次、只写一行 success usage_logs，响应原样透传；
//	2. Caps().Endpoints 不含 moderations 的渠道（deepseek）在 BuildRequest 前
//	   被 ErrEndpointUnsupported 拒绝：零上游、零计费、零 usage_logs。
//
// 运行（隔离库，绝不复用共享 newtg_test）：
//
//	NEWTG_TEST_DB_DSN=postgres://newtg:newtg@localhost:55432/newtg_dom_w18d?sslmode=disable \
//	NEWTG_TEST_LOGS_DSN=postgres://newtg:newtg@localhost:55432/newtg_dom_w18d_logs?sslmode=disable \
//	NEWTG_TEST_REDIS_ADDR=localhost:56379 \
//	go test -tags=integration ./internal/httpapi/ -run TestIntegrationModeration -v

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"newtg/gateway/internal/billing"
)

// w18dSeedChannel 与 m2SeedChannel 同形，但允许指定渠道类型——用于构造
// 「适配器 Caps 不含 moderations」的反例。
func w18dSeedChannel(t *testing.T, db *sql.DB, sealer *SecretSealer, name, group, upstreamURL, model, channelType string) int64 {
	t.Helper()
	sealed, err := sealer.Seal([]byte(`{"api_key":"w18d-upstream-key"}`))
	if err != nil {
		t.Fatalf("seal credentials: %v", err)
	}
	var id int64
	if err := db.QueryRow(`INSERT INTO channels
		(name, type, base_url, credentials, supported_models, group_tags, model_mapping, weight, priority, status)
		VALUES ($1, $2, $3, $4, $5::jsonb, $6::jsonb, '{}'::jsonb, 1, 10, 1) RETURNING id`,
		name, channelType, upstreamURL, sealed,
		`["`+model+`"]`, `["`+group+`"]`).Scan(&id); err != nil {
		t.Fatalf("seed channel: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM channels WHERE id = $1`, id) })
	return id
}

// w18dSeedTokenPricing 以 mode=0（per_token）写入 prompt_price（积分 / 1M
// prompt tokens）。m2SeedModel 固定 mode=1（按次），无法验证 token 计费路径；
// 审核按 token 计费（relay-04 §5.5），所以这里显式用 token 价。
func w18dSeedTokenPricing(t *testing.T, db *sql.DB, model string, promptPrice int64) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO model_pricing (model, mode, prompt_price, author, status)
		VALUES ($1, 0, $2, 'w18d', 1)`, model, promptPrice); err != nil {
		t.Fatalf("seed model_pricing: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM group_pricing WHERE model = $1`, model)
		_, _ = db.Exec(`DELETE FROM model_pricing WHERE model = $1`, model)
	})
}

func w18dModerationRequest(t *testing.T, handler *RelayHandler, plain, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/moderations", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+plain)
	req.RemoteAddr = "203.0.113.9:1234"
	w := httptest.NewRecorder()
	handler.Handler().ServeHTTP(w, req)
	return w
}

// TestIntegrationModerationBillsOnce 验证审核端点的计费接缝：一次请求 →
// 一次 PreConsume/Terminate、一行 success usage_logs、上游 usage 被采信。
func TestIntegrationModerationBillsOnce(t *testing.T) {
	mainDB, logsDB, gormDB := m2TestDBs(t)
	rdb := m2TestRedis(t)
	suffix := m2Suffix()
	group := "w18dg_" + suffix
	model := "w18d-mod-" + suffix
	m2SeedGroup(t, mainDB, group, 100, 1) // 倍率 1.0
	// 1_000_000 积分 / 1M prompt tokens = 1 积分/token；上游报 7 tokens → 7 积分。
	w18dSeedTokenPricing(t, mainDB, model, 1_000_000)
	userID := m2SeedUser(t, mainDB, group, 10_000)
	m2CleanupUsageLogs(t, logsDB, userID)
	_, plain := m2SeedToken(t, mainDB, userID, group, `[]`, false, nil, nil)

	upstream := m2Counter(t, http.StatusOK,
		`{"id":"modr-1","object":"moderation","model":"`+model+`","results":[{"flagged":false}],"usage":{"prompt_tokens":7,"total_tokens":7}}`)
	w18dSeedChannel(t, mainDB, NewSecretSealer(relaySealerMaster), "w18d-mod-"+suffix, group, upstream.server.URL, model, "openai")

	handler := m2NewHandler(t, mainDB, m2NewEngine(t, gormDB, logsDB, rdb))
	w := w18dModerationRequest(t, handler, plain, `{"model":"`+model+`","input":"hello moderation"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"id":"modr-1"`) {
		t.Fatalf("body = %s, want upstream passthrough", w.Body.String())
	}
	if upstream.count() != 1 {
		t.Fatalf("upstream calls = %d, want 1", upstream.count())
	}
	rows := m2UsageRows(t, logsDB, userID)
	if len(rows) != 1 {
		t.Fatalf("usage_logs = %d，期望恰好 1 行（计费一次）", len(rows))
	}
	if rows[0].status != billing.UsageLogStatusSuccess || rows[0].cost != 7 {
		t.Fatalf("usage_log = status %d / cost %d，期望 success/7（上游 prompt_tokens=7）", rows[0].status, rows[0].cost)
	}
	if balance := m2Balance(t, mainDB, userID); balance != 10_000-7 {
		t.Fatalf("balance = %d, want %d", balance, 10_000-7)
	}
}

// TestIntegrationModerationUnsupportedChannel 验证能力真源：deepseek 渠道
// 的 Caps 不含 moderations，请求在 BuildRequest 前被拒绝，零上游、零扣费。
func TestIntegrationModerationUnsupportedChannel(t *testing.T) {
	mainDB, logsDB, gormDB := m2TestDBs(t)
	rdb := m2TestRedis(t)
	suffix := m2Suffix()
	group := "w18dg_" + suffix
	model := "w18d-mod-nc-" + suffix
	m2SeedGroup(t, mainDB, group, 100, 1)
	w18dSeedTokenPricing(t, mainDB, model, 1_000_000)
	userID := m2SeedUser(t, mainDB, group, 10_000)
	m2CleanupUsageLogs(t, logsDB, userID)
	_, plain := m2SeedToken(t, mainDB, userID, group, `[]`, false, nil, nil)

	upstream := m2Counter(t, http.StatusOK, `{"id":"should-not-be-called"}`)
	w18dSeedChannel(t, mainDB, NewSecretSealer(relaySealerMaster), "w18d-mod-nc-"+suffix, group, upstream.server.URL, model, "deepseek")

	handler := m2NewHandler(t, mainDB, m2NewEngine(t, gormDB, logsDB, rdb))
	w := w18dModerationRequest(t, handler, plain, `{"model":"`+model+`","input":"hello moderation"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 endpoint_not_supported; body=%s", w.Code, w.Body.String())
	}
	if code := decodeRelayError(t, w.Body.Bytes()).Error.Code; code != "endpoint_not_supported" {
		t.Fatalf("code = %q, want endpoint_not_supported", code)
	}
	if upstream.count() != 0 {
		t.Fatalf("upstream calls = %d, want 0（Caps 不含 moderations）", upstream.count())
	}
	rows := m2UsageRows(t, logsDB, userID)
	if len(rows) != 0 {
		t.Fatalf("usage_logs = %#v，期望 0 行（能力拒绝走 PreConsume→Refund，净计费为零、不落 success 行）", rows)
	}
	if balance := m2Balance(t, mainDB, userID); balance != 10_000 {
		t.Fatalf("balance = %d, want 10000（全额退回）", balance)
	}
}
