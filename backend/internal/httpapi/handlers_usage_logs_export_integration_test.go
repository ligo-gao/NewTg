//go:build integration

package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"newtg/gateway/internal/rbac"
)

// M6c Task 2：F-05-07 日志导出 / F-05-08 模型性能 / F-05-06 敏感开关。
//
// 运行：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_test?sslmode=disable' \
//	NEWTG_TEST_LOGS_DSN='postgres://newtg:newtg@localhost:55432/newtg_logs?sslmode=disable' \
//	go test -tags=integration ./internal/httpapi/ -run 'TestIntegrationExport|TestIntegrationModelPerf' -v

// --- F-05-07 导出 ---------------------------------------------------------

func TestIntegrationExportFormatsAndMasking(t *testing.T) {
	env := newUsageTestEnv(t)
	userA, _ := ensureUsageTestUser(t, env, "ex")
	userB, _ := ensureUsageTestUser(t, env, "ey")
	first := 120
	env.seed(t, usageSeed{
		UserID: userA, Model: "gpt-export", TokenName: "tok-a", Upstream: "up-a",
		Status: 1, PromptTokens: 10, Completion: 20, Cost: 100, LatencyMS: 800,
		FirstTokenMS: &first, Stream: true, CreatedAt: time.Now().UTC().Add(-10 * time.Minute),
	})
	env.seed(t, usageSeed{
		UserID: userA, Model: "gpt-export", TokenName: "tok|name\twith\tTAB", Upstream: "up-b",
		Status: 2, LatencyMS: 300, ErrorClass: "=SUM(A1:A2)",
		ErrorMessage: "upstream exploded", CreatedAt: time.Now().UTC().Add(-9 * time.Minute),
	})
	env.seed(t, usageSeed{
		UserID: userB, Model: "gpt-foreign", Status: 1,
		CreatedAt: time.Now().UTC().Add(-8 * time.Minute),
	})

	rec := env.do(t, http.MethodGet, "/api/usage-logs/export?scope=filtered&format=txt&range=24h", userPrincipal(userA))
	if rec.Code != http.StatusOK {
		t.Fatalf("txt export status = %d body=%s", rec.Code, rec.Body.String())
	}
	disposition := rec.Header().Get("Content-Disposition")
	if !strings.HasPrefix(disposition, "attachment;") || !strings.Contains(disposition, ".txt") {
		t.Fatalf("Content-Disposition = %q", disposition)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("Content-Type = %q, want text/plain", ct)
	}
	if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "no-store") {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	body := rec.Body.String()
	metaLines := 0
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "#") {
			metaLines++
		}
	}
	if metaLines != 4 {
		t.Fatalf("metadata lines = %d, want 4:\n%s", metaLines, body)
	}
	if !strings.Contains(body, "time\tstatus\tmodel\ttoken_name") {
		t.Fatalf("txt header missing: %s", body)
	}
	if strings.Contains(body, "username") {
		t.Fatalf("user export must not contain username column: %s", body)
	}
	if strings.Contains(body, "gpt-foreign") {
		t.Fatalf("user export leaked foreign row: %s", body)
	}
	if strings.Contains(body, "upstream exploded") {
		// 默认关闭敏感开关：错误原文必须打码。
		t.Fatalf("export leaked error message without reveal: %s", body)
	}
	if !strings.Contains(body, maskedSensitive) {
		t.Fatalf("export missing masked error value: %s", body)
	}
	if !strings.Contains(body, "'=SUM(A1:A2)") {
		t.Fatalf("TXT formula injection not neutralized: %s", body)
	}
	if !strings.Contains(body, "tok|name") || !strings.Contains(body, "with TAB") {
		t.Fatalf("TXT cell should keep pipe and replace TAB: %s", body)
	}
	if strings.Contains(body, "tok|name\twith") {
		t.Fatalf("TXT must replace embedded TAB in text cells: %q", body)
	}

	rec = env.do(t, http.MethodGet, "/api/usage-logs/export?scope=filtered&format=markdown&range=24h", userPrincipal(userA))
	if rec.Code != http.StatusOK {
		t.Fatalf("markdown export status = %d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/markdown") {
		t.Fatalf("markdown Content-Type = %q", ct)
	}
	md := rec.Body.String()
	if !strings.Contains(md, "| time | status | model | token_name |") {
		t.Fatalf("markdown table header missing:\n%s", md)
	}
	if !strings.Contains(md, `tok\|name with TAB`) {
		t.Fatalf("markdown pipe not escaped: %s", md)
	}
	if !strings.Contains(md, "'=SUM(A1:A2)") {
		t.Fatalf("markdown formula injection not neutralized: %s", md)
	}
	if strings.Contains(md, "upstream exploded") || !strings.Contains(md, maskedSensitive) {
		t.Fatalf("markdown masking failed: %s", md)
	}
}

func TestIntegrationExportScopeFilteredVersusAll(t *testing.T) {
	env := newUsageTestEnv(t)
	userA, nameA := ensureUsageTestUser(t, env, "sa")
	userB, _ := ensureUsageTestUser(t, env, "sb")
	env.seed(t, usageSeed{UserID: userA, Model: "gpt-recent", Status: 1, CreatedAt: time.Now().UTC().Add(-2 * time.Minute)})
	env.seed(t, usageSeed{UserID: userA, Model: "gpt-old", Status: 1, CreatedAt: time.Now().UTC().Add(-3 * time.Hour)})
	env.seed(t, usageSeed{UserID: userB, Model: "gpt-foreign", Status: 1, CreatedAt: time.Now().UTC().Add(-1 * time.Minute)})
	// 列表筛选没有 1h 预设（§3.4 只有 24h/7d/14d/30d），用显式窗口
	// 证明 filtered 只覆盖窗口内行。
	end := time.Now().UTC()
	window := "start=" + url.QueryEscape(end.Add(-30*time.Minute).Format(time.RFC3339)) +
		"&end=" + url.QueryEscape(end.Format(time.RFC3339))

	rec := env.do(t, http.MethodGet, "/api/usage-logs/export?scope=filtered&format=txt&"+window, userPrincipal(userA))
	if rec.Code != http.StatusOK {
		t.Fatalf("filtered export status = %d body=%s", rec.Code, rec.Body.String())
	}
	filtered := rec.Body.String()
	if !strings.Contains(filtered, "gpt-recent") || strings.Contains(filtered, "gpt-old") {
		t.Fatalf("filtered export window mismatch: %s", filtered)
	}
	if strings.Contains(filtered, "gpt-foreign") {
		t.Fatalf("filtered export leaked foreign row")
	}

	// scope=all 忽略时间筛选，但仍受角色可见范围约束。
	rec = env.do(t, http.MethodGet, "/api/usage-logs/export?scope=all&format=txt&"+window, userPrincipal(userA))
	if rec.Code != http.StatusOK {
		t.Fatalf("all export status = %d body=%s", rec.Code, rec.Body.String())
	}
	all := rec.Body.String()
	if !strings.Contains(all, "gpt-recent") || !strings.Contains(all, "gpt-old") {
		t.Fatalf("scope=all must ignore the time filter: %s", all)
	}
	if strings.Contains(all, "gpt-foreign") {
		t.Fatalf("scope=all must not widen role visibility: %s", all)
	}

	// admin + logs:read 的 scope=all 才是全量。
	rec = env.do(t, http.MethodGet, "/api/usage-logs/export?scope=all&format=txt", usageAdminPrincipal(1, rbac.PermLogsRead))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "gpt-foreign") {
		t.Fatalf("admin all export status=%d body=%s", rec.Code, rec.Body.String())
	}

	// user 注入 admin-only 参数 → 403（即使 scope=all 也不放宽）。
	rec = env.do(t, http.MethodGet, "/api/usage-logs/export?scope=all&username="+nameA, userPrincipal(userA))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("user username export status = %d, want 403", rec.Code)
	}
}

func TestIntegrationExportRowLimit(t *testing.T) {
	env := newUsageTestEnv(t)
	overUser, _ := ensureUsageTestUser(t, env, "ov")
	exactUser, _ := ensureUsageTestUser(t, env, "eq")
	t.Cleanup(func() {
		_, _ = env.logsDB.Exec(`DELETE FROM usage_logs WHERE user_id IN ($1, $2)`, overUser, exactUser)
	})

	bulkSeed := func(userID int64, count int) {
		t.Helper()
		if _, err := env.logsDB.Exec(`
			INSERT INTO usage_logs (request_id, user_id, model, "group", prompt_tokens,
				completion_tokens, cost, stream, status, created_at)
			SELECT (md5(random()::text || v::text || clock_timestamp()::text))::uuid, $1,
				'gpt-bulk', 'default', 1, 1, 0, false, 1, now()
			FROM generate_series(1, $2) AS g(v)`, userID, count); err != nil {
			t.Fatalf("bulk seed %d rows: %v", count, err)
		}
	}

	bulkSeed(overUser, exportRowLimit+1)
	rec := env.do(t, http.MethodGet, "/api/usage-logs/export?scope=filtered&format=txt&range=24h", userPrincipal(overUser))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("over-limit status = %d, want 422 body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Fatalf("over-limit Content-Type = %q, want problem+json", ct)
	}
	if disposition := rec.Header().Get("Content-Disposition"); disposition != "" {
		t.Fatalf("over-limit must not return a file, got Content-Disposition %q", disposition)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "EXPORT_ROW_LIMIT") {
		t.Fatalf("over-limit body missing EXPORT_ROW_LIMIT: %s", body)
	}
	if strings.Contains(body, "gpt-bulk") {
		t.Fatalf("over-limit response must not contain partial rows: %s", body)
	}

	bulkSeed(exactUser, exportRowLimit)
	rec = env.do(t, http.MethodGet, "/api/usage-logs/export?scope=filtered&format=txt&range=24h", userPrincipal(exactUser))
	if rec.Code != http.StatusOK {
		t.Fatalf("exact-limit status = %d, want 200 body=%s", rec.Code, rec.Body.String())
	}
	// 4 行元信息 + 1 行表头 + 10,000 行数据。
	if lines := strings.Count(rec.Body.String(), "\n"); lines != exportRowLimit+5 {
		t.Fatalf("exact-limit lines = %d, want %d", lines, exportRowLimit+5)
	}
}

func TestIntegrationSensitiveRevealMatrix(t *testing.T) {
	env := newUsageTestEnv(t)
	userID, _ := ensureUsageTestUser(t, env, "rv")
	id := env.seed(t, usageSeed{
		UserID: userID, Model: "gpt-secret", Status: 2,
		ErrorMessage: "secret-error", ErrorClass: "upstream_error",
	})
	t.Cleanup(func() {
		_, _ = env.logsDB.Exec(`DELETE FROM audit_logs WHERE detail->>'action' = 'log.export' AND user_id IN ($1, 1)`, userID)
	})
	detailPath := fmt.Sprintf("/api/usage-logs/%d", id)

	// 默认 / 无 prompt 权限：列表、详情、导出都打码。
	for _, path := range []string{
		"/api/usage-logs",
		"/api/usage-logs?reveal_sensitive=1",
		detailPath,
		detailPath + "?reveal_sensitive=1",
	} {
		rec := env.do(t, http.MethodGet, path, userPrincipal(userID))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d body=%s", path, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "secret-error") || !strings.Contains(rec.Body.String(), maskedSensitive) {
			t.Fatalf("%s masking failed: %s", path, rec.Body.String())
		}
	}
	for _, format := range []string{"txt", "markdown"} {
		rec := env.do(t, http.MethodGet, "/api/usage-logs/export?format="+format+"&reveal_sensitive=1", userPrincipal(userID))
		if rec.Code != http.StatusOK {
			t.Fatalf("export %s status = %d body=%s", format, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "secret-error") || !strings.Contains(rec.Body.String(), maskedSensitive) {
			t.Fatalf("export %s masking failed: %s", format, rec.Body.String())
		}
	}

	// admin 有 logs:read 但无 log.prompt.view → 仍打码。
	for _, path := range []string{"/api/usage-logs?reveal_sensitive=1", detailPath + "?reveal_sensitive=1"} {
		rec := env.do(t, http.MethodGet, path, usageAdminPrincipal(1, rbac.PermLogsRead))
		if strings.Contains(rec.Body.String(), "secret-error") {
			t.Fatalf("admin without prompt perm leaked raw error: %s", rec.Body.String())
		}
	}

	// admin 同时持有 log.prompt.view → 原文。
	for _, path := range []string{"/api/usage-logs?reveal_sensitive=1", detailPath + "?reveal_sensitive=1"} {
		rec := env.do(t, http.MethodGet, path, usageAdminPrincipal(1, rbac.PermLogsRead, rbac.PermLogPromptView))
		if !strings.Contains(rec.Body.String(), "secret-error") {
			t.Fatalf("admin with prompt perm did not reveal: %s", rec.Body.String())
		}
	}
	rec := env.do(t, http.MethodGet, "/api/usage-logs/export?format=txt&reveal_sensitive=1", usageAdminPrincipal(1, rbac.PermLogsRead, rbac.PermLogPromptView))
	if !strings.Contains(rec.Body.String(), "secret-error") {
		t.Fatalf("admin export with prompt perm did not reveal: %s", rec.Body.String())
	}

	// 非法开关值 → 422，不静默当作 0。
	rec = env.do(t, http.MethodGet, "/api/usage-logs?reveal_sensitive=2", userPrincipal(userID))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid reveal_sensitive status = %d, want 422", rec.Code)
	}
}

func TestIntegrationExportAudit(t *testing.T) {
	env := newUsageTestEnv(t)
	userID, _ := ensureUsageTestUser(t, env, "au")
	env.seed(t, usageSeed{UserID: userID, Model: "gpt-audit", Status: 1})
	t.Cleanup(func() {
		_, _ = env.logsDB.Exec(`DELETE FROM audit_logs WHERE user_id = $1 AND detail->>'action' = 'log.export'`, userID)
	})

	rec := env.do(t, http.MethodGet, "/api/usage-logs/export?scope=filtered&format=markdown&range=24h&model=gpt-audit", userPrincipal(userID))
	if rec.Code != http.StatusOK {
		t.Fatalf("export status = %d body=%s", rec.Code, rec.Body.String())
	}

	var category, route, method, result string
	var statusCode *int64
	var detailRaw []byte
	if err := env.logsDB.QueryRow(`
		SELECT category, route, method, result, status_code, detail
		FROM audit_logs
		WHERE user_id = $1 AND detail->>'action' = 'log.export'
		ORDER BY id DESC LIMIT 1`, userID).Scan(&category, &route, &method, &result, &statusCode, &detailRaw); err != nil {
		t.Fatalf("load export audit: %v", err)
	}
	if category != "operation" || result != "success" {
		t.Fatalf("audit category/result = %s/%s, want operation/success", category, result)
	}
	if method != http.MethodGet || !strings.HasSuffix(route, "/api/usage-logs/export") {
		t.Fatalf("audit method/route = %s/%s", method, route)
	}
	var detail map[string]any
	if err := json.Unmarshal(detailRaw, &detail); err != nil {
		t.Fatalf("decode audit detail: %v", err)
	}
	if detail["scope"] != "filtered" || detail["format"] != "markdown" || detail["row_count"] != float64(1) {
		t.Fatalf("audit detail = %v", detail)
	}
	filters, _ := detail["filters"].([]any)
	seen := map[string]bool{}
	for _, item := range filters {
		seen[fmt.Sprint(item)] = true
	}
	if !seen["model"] || !seen["range"] {
		t.Fatalf("audit filters missing dimensions: %v", filters)
	}
	// 审计只记录维度名，不得记录筛选值或导出行内容。
	if strings.Contains(string(detailRaw), "gpt-audit") {
		t.Fatalf("audit detail leaked filter value: %s", detailRaw)
	}
}

// --- F-05-08 模型性能 -----------------------------------------------------

type fakeModelPerfSource struct {
	buckets []ModelPerfBucket
	updated time.Time
	err     error
}

func (f *fakeModelPerfSource) ModelPerfBuckets(_ context.Context, start, end time.Time, _ string, model string) ([]ModelPerfBucket, time.Time, error) {
	if f.err != nil {
		return nil, time.Time{}, f.err
	}
	out := make([]ModelPerfBucket, 0, len(f.buckets))
	for _, bucket := range f.buckets {
		at := bucket.Start.UTC()
		if at.Before(start) || !at.Before(end) {
			continue
		}
		if model != "" && bucket.Model != model {
			continue
		}
		out = append(out, bucket)
	}
	return out, f.updated, nil
}

type modelPerfTestResponse struct {
	Window struct {
		Range       string `json:"range"`
		Granularity string `json:"granularity"`
	} `json:"window"`
	Buckets []struct {
		Start *time.Time `json:"start"`
		Model string     `json:"model"`
	} `json:"buckets"`
	Summary struct {
		TTFTp50     *float64 `json:"ttft_p50_ms"`
		TTFTp95     *float64 `json:"ttft_p95_ms"`
		LatencyP50  *float64 `json:"latency_p50_ms"`
		LatencyP95  *float64 `json:"latency_p95_ms"`
		SuccessRate *float64 `json:"success_rate"`
		Refused     int64    `json:"refused"`
		Uptime      *float64 `json:"uptime"`
		Incidents   *int64   `json:"incidents"`
		TPS         *float64 `json:"tps"`
		SampleSize  *int64   `json:"sample_size"`
	} `json:"summary"`
	Incidents []struct {
		DurationSeconds int64  `json:"duration_seconds"`
		Severity        string `json:"severity"`
	} `json:"incidents"`
	Models []struct {
		Model     string `json:"model"`
		Incidents int64  `json:"incidents"`
		Badge     string `json:"badge"`
	} `json:"models"`
	Pipeline struct {
		Enabled   bool       `json:"enabled"`
		UpdatedAt *time.Time `json:"updated_at"`
		Stale     bool       `json:"stale"`
	} `json:"pipeline"`
	Comparison *struct {
		PipelineSuccessRate  *float64 `json:"pipeline_success_rate"`
		UsageLogsSuccessRate *float64 `json:"usage_logs_success_rate"`
		Delta                *float64 `json:"delta"`
	} `json:"comparison"`
	Dimensions []struct {
		Dimension string `json:"dimension"`
		Key       string `json:"key"`
		Requests  int64  `json:"requests"`
	} `json:"dimensions"`
}

func TestIntegrationModelPerfGate(t *testing.T) {
	env := newUsageTestEnv(t)
	userID, _ := ensureUsageTestUser(t, env, "pg")

	for _, path := range []string{"/api/model-perf", "/api/usage-logs/model-performance"} {
		rec := env.do(t, http.MethodGet, path, userPrincipal(userID))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d body=%s", path, rec.Code, rec.Body.String())
		}
		body := decodeUsageJSON[modelPerfTestResponse](t, rec)
		if body.Pipeline.Enabled || !body.Pipeline.Stale {
			t.Fatalf("%s pipeline = %+v, want disabled+stale gate", path, body.Pipeline)
		}
		if body.Summary.TTFTp95 != nil || body.Summary.LatencyP95 != nil ||
			body.Summary.SuccessRate != nil || body.Summary.Uptime != nil ||
			body.Summary.Incidents != nil || body.Summary.TPS != nil || body.Summary.SampleSize != nil {
			t.Fatalf("%s gate must not fabricate metrics: %+v", path, body.Summary)
		}
		if len(body.Buckets) != 0 || len(body.Incidents) != 0 || len(body.Models) != 0 {
			t.Fatalf("%s gate must return empty series", path)
		}
		if body.Comparison != nil {
			t.Fatalf("%s gate comparison = %+v, want null", path, body.Comparison)
		}
	}
}

func TestIntegrationModelPerfMetricsAndPermissions(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	latency := [modelPerfHistogramBuckets]int64{}
	latency[1] = 100
	ttft := [modelPerfHistogramBuckets]int64{}
	ttft[0] = 50
	channel := int64(7)
	upstream := "up-fast"
	fake := &fakeModelPerfSource{
		updated: now.Add(-time.Minute),
		buckets: []ModelPerfBucket{{
			Model: "gpt-test", Start: now.Add(-10 * time.Minute),
			ChannelID: &channel, Upstream: &upstream,
			Success: 90, Error: 8, Refused: 2,
			LatencyCount: 100, LatencyBuckets: latency,
			TTFTCount: 50, TTFTBuckets: ttft,
			CompletionTokens: 1000, GenerationSeconds: 20,
			ObservedSeconds: 60, UpSeconds: 60,
		}},
	}
	env := newUsageTestEnvWith(t, usageTestOptions{Perf: fake, Now: func() time.Time { return now }})
	userID, _ := ensureUsageTestUser(t, env, "pm")

	rec := env.do(t, http.MethodGet, "/api/model-perf?range=24h&model=gpt-test", userPrincipal(userID))
	if rec.Code != http.StatusOK {
		t.Fatalf("model perf status = %d body=%s", rec.Code, rec.Body.String())
	}
	body := decodeUsageJSON[modelPerfTestResponse](t, rec)
	if !body.Pipeline.Enabled || body.Pipeline.Stale {
		t.Fatalf("pipeline = %+v, want enabled+fresh", body.Pipeline)
	}
	if body.Window.Granularity != "1m" || body.Window.Range != "24h" {
		t.Fatalf("window = %+v", body.Window)
	}
	s := body.Summary
	if s.TTFTp50 == nil || *s.TTFTp50 != 25 || s.TTFTp95 == nil || *s.TTFTp95 != 47.5 {
		t.Fatalf("ttft summary = p50:%v p95:%v, want 25/47.5", s.TTFTp50, s.TTFTp95)
	}
	if s.LatencyP50 == nil || *s.LatencyP50 != 75 || s.LatencyP95 == nil || *s.LatencyP95 != 97.5 {
		t.Fatalf("latency summary = p50:%v p95:%v, want 75/97.5", s.LatencyP50, s.LatencyP95)
	}
	if s.SuccessRate == nil || *s.SuccessRate != 0.9 || s.Refused != 2 {
		t.Fatalf("success rate/refused = %v/%d, want 0.9/2", s.SuccessRate, s.Refused)
	}
	if s.Uptime == nil || *s.Uptime != 1 || s.TPS == nil || *s.TPS != 50 {
		t.Fatalf("uptime/tps = %v/%v, want 1/50", s.Uptime, s.TPS)
	}
	if s.SampleSize == nil || *s.SampleSize != 100 || s.Incidents == nil || *s.Incidents != 0 {
		t.Fatalf("sample/incidents = %v/%v, want 100/0", s.SampleSize, s.Incidents)
	}
	if len(body.Buckets) != 1 || body.Buckets[0].Model != "gpt-test" {
		t.Fatalf("buckets = %+v", body.Buckets)
	}

	// 无样本模型：指标为 null，不是 0。
	rec = env.do(t, http.MethodGet, "/api/model-perf?range=24h&model=no-such-model", userPrincipal(userID))
	empty := decodeUsageJSON[modelPerfTestResponse](t, rec)
	if empty.Summary.SampleSize != nil || empty.Summary.Incidents != nil ||
		empty.Summary.SuccessRate != nil || empty.Summary.Uptime != nil {
		t.Fatalf("empty model summary = %+v, want null values", empty.Summary)
	}
	if len(empty.Buckets) != 0 || len(empty.Models) != 0 {
		t.Fatalf("empty model series = %+v/%+v", empty.Buckets, empty.Models)
	}

	// user 传 dimension → 403；admin 可见渠道分解。
	rec = env.do(t, http.MethodGet, "/api/model-perf?dimension=channel", userPrincipal(userID))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("user dimension status = %d, want 403", rec.Code)
	}
	rec = env.do(t, http.MethodGet, "/api/model-perf?dimension=channel&model=gpt-test", usageAdminPrincipal(1, rbac.PermLogsRead))
	if rec.Code != http.StatusOK {
		t.Fatalf("admin dimension status = %d body=%s", rec.Code, rec.Body.String())
	}
	adminBody := decodeUsageJSON[modelPerfTestResponse](t, rec)
	if len(adminBody.Dimensions) != 1 || adminBody.Dimensions[0].Key != "7" || adminBody.Dimensions[0].Requests != 100 {
		t.Fatalf("channel dimensions = %+v", adminBody.Dimensions)
	}

	// 非法 range / dimension → 422。
	for _, query := range []string{"range=2h", "dimension=region"} {
		rec = env.do(t, http.MethodGet, "/api/model-perf?"+query, usageAdminPrincipal(1, rbac.PermLogsRead))
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s status = %d, want 422", query, rec.Code)
		}
	}
}

func TestIntegrationModelPerfIncidents(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	buckets := make([]ModelPerfBucket, 0, 6)
	for i := 0; i < 6; i++ {
		buckets = append(buckets, ModelPerfBucket{
			Model: "bad-test", Start: now.Add(-10*time.Minute + time.Duration(i)*time.Minute),
			Success: 0, Error: 10, ObservedSeconds: 60, UpSeconds: 0,
		})
	}
	fake := &fakeModelPerfSource{updated: now.Add(-time.Minute), buckets: buckets}
	env := newUsageTestEnvWith(t, usageTestOptions{Perf: fake, Now: func() time.Time { return now }})
	userID, _ := ensureUsageTestUser(t, env, "pi")

	rec := env.do(t, http.MethodGet, "/api/model-perf?range=24h&model=bad-test", userPrincipal(userID))
	body := decodeUsageJSON[modelPerfTestResponse](t, rec)
	if body.Summary.Incidents == nil || *body.Summary.Incidents != 1 {
		t.Fatalf("incidents = %v, want 1", body.Summary.Incidents)
	}
	if len(body.Incidents) != 1 || body.Incidents[0].DurationSeconds != 360 || body.Incidents[0].Severity != "major" {
		t.Fatalf("incident detail = %+v", body.Incidents)
	}
	if len(body.Models) != 1 || body.Models[0].Incidents != 1 || body.Models[0].Badge != "incident" {
		t.Fatalf("model row = %+v", body.Models)
	}
}

func TestIntegrationModelPerfComparison(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	fake := &fakeModelPerfSource{
		updated: now.Add(-time.Minute),
		buckets: []ModelPerfBucket{{
			Model: "cmp-model", Start: now.Add(-10 * time.Minute),
			Success: 8, Error: 2, ObservedSeconds: 60, UpSeconds: 60,
		}},
	}
	env := newUsageTestEnvWith(t, usageTestOptions{Perf: fake, Now: func() time.Time { return now }})
	userID, _ := ensureUsageTestUser(t, env, "pc")
	for i := 0; i < 9; i++ {
		env.seed(t, usageSeed{UserID: userID, Model: "cmp-model", Status: 1, CreatedAt: now.Add(-5 * time.Minute)})
	}
	for i := 0; i < 3; i++ {
		env.seed(t, usageSeed{UserID: userID, Model: "cmp-model", Status: 2, CreatedAt: now.Add(-5 * time.Minute)})
	}

	rec := env.do(t, http.MethodGet, "/api/model-perf?range=24h&model=cmp-model", userPrincipal(userID))
	body := decodeUsageJSON[modelPerfTestResponse](t, rec)
	if body.Comparison == nil {
		t.Fatalf("comparison = null, want 24h pipeline/usage_logs pair")
	}
	if body.Comparison.PipelineSuccessRate == nil || *body.Comparison.PipelineSuccessRate != 0.8 {
		t.Fatalf("pipeline success rate = %v, want 0.8", body.Comparison.PipelineSuccessRate)
	}
	if body.Comparison.UsageLogsSuccessRate == nil || *body.Comparison.UsageLogsSuccessRate != 0.75 {
		t.Fatalf("usage_logs success rate = %v, want 0.75", body.Comparison.UsageLogsSuccessRate)
	}
	if body.Comparison.Delta == nil || *body.Comparison.Delta < 0.049 || *body.Comparison.Delta > 0.051 {
		t.Fatalf("delta = %v, want ~0.05", body.Comparison.Delta)
	}
}
