//go:build integration

package httpapi

// W18-B 集成验收（真实 PostgreSQL + Redis + 真实计费引擎）：
// OpenAI 上游承接 /v1/messages → IR 响应编码为 Claude 消息 → 结算恰好一次，
// usage_logs 恰好一行，且 token 数与金额和编码器映射一致（R-W18-03）。
//
// 运行（隔离库，绝不复用共享 newtg_test）：
//
//	NEWTG_TEST_DB_DSN=postgres://newtg:newtg@localhost:55432/newtg_dom_w18b \
//	NEWTG_TEST_LOGS_DSN=postgres://newtg:newtg@localhost:55432/newtg_dom_w18b_logs \
//	NEWTG_TEST_REDIS_ADDR=localhost:56379 \
//	go test -tags=integration ./internal/httpapi/ -run TestIntegrationW18B -v

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"newtg/gateway/internal/billing"
)

// TestIntegrationW18BMessagesOpenAIUpstreamSettlesOnce：一次 OpenAI 上游的
// /v1/messages 请求 → 结算一次、usage_logs 一行、金额与映射一致。
func TestIntegrationW18BMessagesOpenAIUpstreamSettlesOnce(t *testing.T) {
	mainDB, logsDB, gormDB := m2TestDBs(t)
	rdb := m2TestRedis(t)
	suffix := m2Suffix()
	model := "w18b-model-" + suffix

	// default 分组（100 = 1.0x；pricing 侧按整数百分比归一）。
	if _, err := mainDB.Exec(
		`INSERT INTO user_groups (name, ratio, user_selectable, is_system, status)
		 VALUES ('default', 100, true, true, 1) ON CONFLICT (name) DO NOTHING`); err != nil {
		t.Fatalf("ensure default group: %v", err)
	}

	const initialBalance int64 = 1000
	userID := m2SeedUser(t, mainDB, "default", initialBalance)
	_, plain := m2SeedToken(t, mainDB, userID, "default", "[]", false, nil, nil)
	// usage_logs 落在共享日志库：清理必须在用例**开始**就登记（而不是断言后），
	// 否则用例中途失败会把行留给后续用例的「24h 内恰 N 条」断言。
	t.Cleanup(func() { _, _ = logsDB.Exec(`DELETE FROM usage_logs WHERE user_id = $1`, userID) })

	if _, err := mainDB.Exec(
		`INSERT INTO model_pricing (model, mode, prompt_price, completion_price,
			cache_read_price, cache_write_price, author, status)
		 VALUES ($1, 0, 1000000, 2000000, 0, 0, 'w18b', 1)`, model); err != nil {
		t.Fatalf("seed token pricing: %v", err)
	}
	t.Cleanup(func() {
		_, _ = mainDB.Exec(`DELETE FROM group_pricing WHERE model = $1`, model)
		_, _ = mainDB.Exec(`DELETE FROM model_pricing WHERE model = $1`, model)
	})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			t.Errorf("上游路径 = %q，期望 OpenAI chat/completions", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-w18b","object":"chat.completion",`+
			`"model":"gpt-upstream","choices":[{"index":0,"message":{"role":"assistant",`+
			`"content":"settled once"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	}))
	t.Cleanup(upstream.Close)

	sealer := NewSecretSealer(relaySealerMaster)
	m2SeedChannel(t, mainDB, sealer, "w18b-openai-"+suffix, "default", upstream.URL, model)
	engine := m2NewEngine(t, gormDB, logsDB, rdb)
	handler := m2NewHandler(t, mainDB, engine)
	handler.client = upstream.Client()

	body := `{"model":"` + model + `","max_tokens":64,` +
		`"messages":[{"role":"user","content":"hi"}],"stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", plain)
	rec := httptest.NewRecorder()
	handler.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var respObj map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &respObj); err != nil {
		t.Fatalf("response is not JSON: %v; body=%s", err, rec.Body.String())
	}
	if respObj["type"] != "message" || respObj["stop_reason"] != "end_turn" {
		t.Fatalf("response = %s, want Anthropic message/end_turn", rec.Body.String())
	}
	usageObj := respObj["usage"].(map[string]any)
	if usageObj["input_tokens"] != float64(10) || usageObj["output_tokens"] != float64(5) {
		t.Fatalf("response usage = %v, want 10/5（编码器映射）", usageObj)
	}

	// usage_logs：恰好一行、status=success、token 数与编码器映射一致。
	rows, err := logsDB.Query(
		`SELECT status, prompt_tokens, completion_tokens, cost, refunded
		   FROM usage_logs WHERE user_id = $1 ORDER BY id`, userID)
	if err != nil {
		t.Fatalf("read usage_logs: %v", err)
	}
	defer func() { _ = rows.Close() }()
	type logRow struct {
		status, prompt, completion, cost, refunded int64
	}
	var logs []logRow
	for rows.Next() {
		var row logRow
		if err := rows.Scan(&row.status, &row.prompt, &row.completion, &row.cost, &row.refunded); err != nil {
			t.Fatalf("scan usage_logs: %v", err)
		}
		logs = append(logs, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate usage_logs: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("usage_logs rows = %d, want exactly 1（只结算一次）", len(logs))
	}
	got := logs[0]
	if got.status != int64(billing.UsageLogStatusSuccess) {
		t.Fatalf("usage_logs.status = %d, want success", got.status)
	}
	if got.prompt != 10 || got.completion != 5 {
		t.Fatalf("usage_logs tokens = %d/%d, want 10/5（与 message.usage 一致）",
			got.prompt, got.completion)
	}
	// 10 * 1e6/1M + 5 * 2e6/1M = 20。
	if got.cost != 20 || got.refunded != 0 {
		t.Fatalf("usage_logs cost/refunded = %d/%d, want 20/0", got.cost, got.refunded)
	}

	var balance int64
	if err := mainDB.QueryRow(`SELECT balance FROM users WHERE id = $1`, userID).Scan(&balance); err != nil {
		t.Fatalf("read balance: %v", err)
	}
	if balance != initialBalance-20 {
		t.Fatalf("balance = %d, want %d（金额与映射一致）", balance, initialBalance-20)
	}
}
