//go:build integration

package httpapi

// W18-A 集成验收（真实 PostgreSQL 主库 + 日志库 + Redis + 真实计费引擎）。
//
// 单测已用 fake 计费证明「count_tokens 不调用 PreConsume」；这里补真正的
// 资金与流水副作用（R-W18-06 的零计费契约）：
//   - 钱包余额不变、wallet_transactions 0 行；
//   - 日志库 usage_logs 0 行；
//   - B10/B11/B12 三个限流桶的 Redis 键都不存在（阈值设 1，若误用限流会立刻
//     在第二次请求上 429）；
//   - 令牌鉴权仍由同一条 RelayAuth 承担（坏令牌 → Claude 形 401）。
//
// 运行（隔离库，绝不复用共享 newtg_test）：
//
//	NEWTG_TEST_DB_DSN=postgres://newtg:newtg@localhost:55432/newtg_dom_w18a
//	NEWTG_TEST_LOGS_DSN=postgres://newtg:newtg@localhost:55432/newtg_dom_w18a_logs
//	NEWTG_TEST_REDIS_ADDR=localhost:56379
//	go test -tags=integration ./internal/httpapi/ -run TestIntegrationW18A -v

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"newtg/gateway/internal/ratelimit"
	"newtg/gateway/internal/store"
)

// w18aSeedAnthropicChannel 插入一个指向 httptest 的 Anthropic 原生渠道。
func w18aSeedAnthropicChannel(t *testing.T, db *sql.DB, sealer *SecretSealer,
	name, group, upstreamURL, model string) int64 {
	t.Helper()
	sealed, err := sealer.Seal([]byte(`{"api_key":"w18a-upstream-key"}`))
	if err != nil {
		t.Fatalf("seal credentials: %v", err)
	}
	var id int64
	if err := db.QueryRow(`INSERT INTO channels
		(name, type, base_url, credentials, supported_models, group_tags, model_mapping, weight, priority, status)
		VALUES ($1, 'anthropic', $2, $3, $4::jsonb, $5::jsonb, '{}'::jsonb, 1, 10, 1) RETURNING id`,
		name, upstreamURL, sealed, `["`+model+`"]`, `["`+group+`"]`).Scan(&id); err != nil {
		t.Fatalf("seed anthropic channel: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM channels WHERE id = $1`, id) })
	return id
}

// TestIntegrationW18ACountTokensZeroBilling 锁定 R-W18-06 的资金面。
func TestIntegrationW18ACountTokensZeroBilling(t *testing.T) {
	mainDB, logsDB, gormDB := m2TestDBs(t)
	rdb := m2TestRedis(t)
	suffix := m2Suffix()
	group := "w18a_" + suffix
	model := "w18a-model-" + suffix
	m2SeedGroup(t, mainDB, group, 100, 1)
	// 故意给一个明显的按次价：任何误入计费都会改变余额/流水。
	m2SeedModel(t, mainDB, model, 500)
	userID := m2SeedUser(t, mainDB, group, 10_000)
	m2CleanupUsageLogs(t, logsDB, userID)
	tokenID, plain := m2SeedToken(t, mainDB, userID, group, `[]`, false, nil, nil)

	var upstreamCalls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		if r.URL.Path != "/v1/messages/count_tokens" {
			t.Errorf("upstream path = %q, want /v1/messages/count_tokens", r.URL.Path)
		}
		if got := r.Header.Get("x-api-key"); got != "w18a-upstream-key" {
			t.Errorf("upstream x-api-key = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"input_tokens":123}`)
	}))
	t.Cleanup(upstream.Close)

	sealer := NewSecretSealer(relaySealerMaster)
	w18aSeedAnthropicChannel(t, mainDB, sealer, "w18a-"+suffix, group, upstream.URL, model)

	cfg := mapConfig{
		"rl.user_model_rpm":  json.RawMessage("1"),
		"rl.token_rpm":       json.RawMessage("1"),
		"rl.user_global_rpm": json.RawMessage("1"),
	}
	tokenRepo := store.NewTokenRepo(mainDB)
	pricingRepo := store.NewPricingRepo(mainDB)
	handler := NewRelayHandler(RelayHandlerOptions{
		Channels:        store.NewChannelRepo(mainDB),
		Tokens:          tokenRepo,
		Access:          tokenRepo,
		Sealer:          sealer,
		ModelStatus:     pricingRepo,
		GroupConfigured: pricingRepo.GroupConfigured,
		Client:          &http.Client{Timeout: 10 * time.Second},
		Billing:         m2NewEngine(t, gormDB, logsDB, rdb).NewSession,
		RateLimiter:     ratelimit.New(rdb),
		Config:          cfg,
	})

	do := func(token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens",
			strings.NewReader(fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hello"}]}`, model)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-api-key", token)
		req.RemoteAddr = "203.0.113.77:1234"
		rec := httptest.NewRecorder()
		handler.Handler().ServeHTTP(rec, req)
		return rec
	}

	// 两次请求：限流阈值都是 1，若 count_tokens 误用 B10/B11/B12，第二次必为 429。
	for i := 0; i < 2; i++ {
		w := do(plain)
		if w.Code != http.StatusOK {
			t.Fatalf("第 %d 次 status = %d；body=%s", i+1, w.Code, w.Body.String())
		}
		var resp struct {
			InputTokens int64 `json:"input_tokens"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || resp.InputTokens != 123 {
			t.Fatalf("第 %d 次响应 = %s（err=%v），want input_tokens=123", i+1, w.Body.String(), err)
		}
	}

	if upstreamCalls != 2 {
		t.Fatalf("上游调用 = %d，want 2", upstreamCalls)
	}
	// 零资金：余额不变、无钱包流水、无 usage_logs。
	if balance := m2Balance(t, mainDB, userID); balance != 10_000 {
		t.Fatalf("余额 = %d，want 10000（count_tokens 不得计费）", balance)
	}
	if rows := m2WalletRows(t, mainDB, userID); rows != 0 {
		t.Fatalf("wallet_transactions = %d 行，want 0", rows)
	}
	if rows := m2UsageRows(t, logsDB, userID); len(rows) != 0 {
		t.Fatalf("usage_logs = %d 行，want 0（不写流水）", len(rows))
	}
	// 零限流桶：三个键都不存在。
	for _, key := range []string{
		ratelimit.TokenKey(tokenID),
		ratelimit.UserKey(userID),
		ratelimit.UserGlobalKey(userID),
	} {
		if n, err := rdb.Exists(t.Context(), key).Result(); err != nil || n != 0 {
			t.Fatalf("限流键 %q 存在（n=%d, err=%v），count_tokens 不得消耗 B10/B11/B12", key, n, err)
		}
	}

	// 令牌鉴权仍生效，且失败形状是 Claude 形（路径感知写出器）。
	bad := do("nkt-not-a-real-token")
	assertClaudeError(t, bad, http.StatusUnauthorized, "authentication_error")
	if upstreamCalls != 2 {
		t.Fatalf("鉴权失败不得触达上游：calls=%d", upstreamCalls)
	}

	// 防回归：同一条 RelayAuth 链上的 /v1/chat/completions 仍是 OpenAI 形。
	chatReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{bad json`))
	chatReq.Header.Set("Content-Type", "application/json")
	chatReq.Header.Set("Authorization", "Bearer "+plain)
	chatRec := httptest.NewRecorder()
	handler.Handler().ServeHTTP(chatRec, chatReq)
	if chatRec.Code != http.StatusBadRequest {
		t.Fatalf("chat status = %d，want 400；body=%s", chatRec.Code, chatRec.Body.String())
	}
	if strings.Contains(chatRec.Body.String(), `"type":"error"`) {
		t.Fatalf("chat 不得使用 Claude 顶层形状：%s", chatRec.Body.String())
	}
}
