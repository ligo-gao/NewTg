package metrics

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

// TestMain 固定名称校验方案：prometheus/common v0.67 要求显式选择
// Legacy/UTF-8；本项目的指标名全部是 legacy 合法名，选 Legacy 与抓取端一致。
func TestMain(m *testing.M) {
	model.NameValidationScheme = model.LegacyValidation
	os.Exit(m.Run())
}

// requiredFamilies 是首批交付的冻结清单（R-W10-08）。
//
// 其中 newtg_billing_quota_clamp_total / settle_credits_total /
// worker_usage_logs_stream_lag / worker_abandoned_* / dependency_up /
// migration_version 是「无论怎么裁量都必须进第一批」的成员。
var requiredFamilies = []string{
	"newtg_http_requests_total",
	"newtg_http_request_duration_seconds",
	"newtg_http_in_flight_requests",
	"newtg_http_response_size_bytes",

	"newtg_relay_requests_total",
	"newtg_relay_request_duration_seconds",
	"newtg_relay_first_token_seconds",
	"newtg_relay_attempts_total",
	"newtg_relay_errors_total",

	"newtg_channel_requests_total",
	"newtg_channel_request_duration_seconds",
	"newtg_channel_failover_total",
	"newtg_channel_auto_disabled_total",

	"newtg_billing_preconsume_credits_total",
	"newtg_billing_settle_credits_total",
	"newtg_billing_refund_credits_total",
	"newtg_billing_quota_clamp_total",
	"newtg_billing_insufficient_quota_total",
	"newtg_billing_settle_failures_total",

	"newtg_worker_task_runs_total",
	"newtg_worker_task_duration_seconds",
	"newtg_worker_task_last_success_timestamp_seconds",
	"newtg_worker_abandoned_refund_total",
	"newtg_worker_abandoned_oldest_age_seconds",
	"newtg_worker_usage_logs_stream_lag",

	"newtg_db_pool_connections",
	"newtg_db_pool_wait_total",
	"newtg_redis_up",
	"newtg_redis_command_duration_seconds",

	"newtg_build_info",
	"newtg_dependency_up",
	"newtg_migration_version",
}

// populateAll 让每个首批 family 都有至少一个样本。
func populateAll(set *Set) {
	now := time.Millisecond
	set.ObserveHTTP("gateway", "GET", "GET /api/status", 200, now, 128)
	set.IncHTTPInFlight("gateway")
	set.DecHTTPInFlight("gateway")
	// 连接池 collector 需要真实 *sql.DB 才会产出样本；用不建连的 stub
	// connector，Stats() 全 0，足以验证 family 形状与 db 标签。
	db := sql.OpenDB(stubConnector{})
	defer db.Close()
	set.AttachDBPools(db, db)
	set.ObserveRelayRequest("chat_completions", "gpt-x", false, "success", now*10)
	set.ObserveRelayRequest("chat_completions", "gpt-x", true, "error", now*20)
	set.ObserveRelayFirstToken("chat_completions", "gpt-x", 350*time.Millisecond)
	set.ObserveRelayAttempt("chat_completions", "gpt-x", false, 7, "openai",
		"", "ok", "", 0, now)
	set.ObserveRelayAttempt("chat_completions", "gpt-x", false, 8, "openai",
		"openai", "retryable_fail", "retryable", 1, now)
	set.ObserveChannelAutoDisabled("openai", "upstream_auth")
	set.ObservePreConsume("gpt-x", "default", 100)
	set.ObserveSettle("gpt-x", "default", "ratio", 90)
	set.ObserveRefund("sync", 10)
	set.ObserveQuotaClamp("quotaChecked", "overflow")
	set.ObserveInsufficientQuota("gpt-x")
	set.ObserveSettleFailure("gpt-x", "default", "insufficient_quota")
	set.InitWorker()
	set.ObserveWorkerTask("preconsume_reconcile", "succeeded", now)
	set.SetWorkerAbandoned("wallet", 3, 12*time.Minute)
	set.SetWorkerStreamLag(0)
	set.SetRedisUp(true)
	set.ObserveRedisCommand("get", now)
	set.SetDependencyUp("postgres", true)
	set.SetDependencyUp("redis", true)
	set.SetMigration(20261021000001, false)
}

func gatherText(t *testing.T, reg *prometheus.Registry) (string, map[string]*dto.MetricFamily) {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var sb strings.Builder
	enc := expfmt.NewEncoder(&sb, expfmt.NewFormat(expfmt.TypeTextPlain))
	for _, mf := range families {
		if err := enc.Encode(mf); err != nil {
			t.Fatalf("encode %s: %v", mf.GetName(), err)
		}
	}
	parser := expfmt.NewTextParser(model.LegacyValidation)
	parsed, err := parser.TextToMetricFamilies(strings.NewReader(sb.String()))
	if err != nil {
		t.Fatalf("re-parse exposition: %v\n%s", err, sb.String())
	}
	return sb.String(), parsed
}

// TestExpositionFormatAndRequiredFamilies：文本 exposition 可被 Prometheus
// 标准解析器读回，且首批 31 个 family 全部存在（变异：删任一必需指标即红）。
func TestExpositionFormatAndRequiredFamilies(t *testing.T) {
	reg, set := NewRegistry()
	populateAll(set)

	text, parsed := gatherText(t, reg)
	if !strings.Contains(text, "# TYPE newtg_http_requests_total counter") {
		t.Fatalf("exposition missing TYPE line:\n%s", text)
	}
	for _, name := range requiredFamilies {
		if _, ok := parsed[name]; !ok {
			t.Errorf("required family %q missing from exposition", name)
		}
	}
	// go_* / process_* 标准采集器必须同时暴露。
	for _, name := range []string{"go_goroutines", "process_start_time_seconds"} {
		if _, ok := parsed[name]; !ok {
			t.Errorf("standard collector family %q missing", name)
		}
	}
}

// TestCounterGaugeHistogramBehavior：三类指标的语义正确。
func TestCounterGaugeHistogramBehavior(t *testing.T) {
	reg, set := NewRegistry()
	set.ObserveHTTP("gateway", "POST", "POST /x", 200, 50*time.Millisecond, 10)
	set.ObserveHTTP("gateway", "POST", "POST /x", 500, 150*time.Millisecond, 20)
	set.IncHTTPInFlight("gateway")
	set.IncHTTPInFlight("gateway")
	set.DecHTTPInFlight("gateway")
	set.SetWorkerStreamLag(42)
	set.ObserveQuotaClamp("op", "nan")

	_, parsed := gatherText(t, reg)

	req := parsed["newtg_http_requests_total"]
	if len(req.Metric) != 2 {
		t.Fatalf("http requests series = %d, want 2", len(req.Metric))
	}
	var total float64
	for _, m := range req.Metric {
		total += m.GetCounter().GetValue()
	}
	if total != 2 {
		t.Fatalf("http counter total = %v, want 2", total)
	}
	inflight := findMetric(t, parsed, "newtg_http_in_flight_requests", nil)
	if got := inflight.GetGauge().GetValue(); got != 1 {
		t.Fatalf("in_flight = %v, want 1", got)
	}
	lag := findMetric(t, parsed, "newtg_worker_usage_logs_stream_lag", nil)
	if got := lag.GetGauge().GetValue(); got != 42 {
		t.Fatalf("stream lag = %v, want 42", got)
	}
	duration := parsed["newtg_http_request_duration_seconds"]
	if len(duration.Metric) != 1 {
		t.Fatalf("duration series = %d, want 1", len(duration.Metric))
	}
	if got := duration.Metric[0].GetHistogram().GetSampleCount(); got != 2 {
		t.Fatalf("histogram count = %d, want 2", got)
	}
	if got := duration.Metric[0].GetHistogram().GetSampleSum(); got < 0.19 || got > 0.21 {
		t.Fatalf("histogram sum = %v, want ~0.2", got)
	}
	if got := findMetric(t, parsed, "newtg_billing_quota_clamp_total", map[string]string{"kind": "nan"}); got.GetCounter().GetValue() != 1 {
		t.Fatalf("quota clamp nan counter = %v, want 1", got.GetCounter().GetValue())
	}
}

// TestLabelGuardCardinality：第 N+1 个动态值归并 other，超长值不登记。
func TestLabelGuardCardinality(t *testing.T) {
	g := newLabelGuard(3)
	if got := g.Value(""); got != "unknown" {
		t.Fatalf("empty = %q, want unknown", got)
	}
	for _, v := range []string{"a", "b", "c"} {
		if got := g.Value(v); got != v {
			t.Fatalf("value %q = %q", v, got)
		}
	}
	if got := g.Value("d"); got != "other" {
		t.Fatalf("4th value = %q, want other", got)
	}
	if got := g.Value("a"); got != "a" {
		t.Fatalf("seen value must stay stable, got %q", got)
	}
	if g.size() != 3 {
		t.Fatalf("guard size = %d, want 3", g.size())
	}
	long := strings.Repeat("m", maxLabelValueLen+1)
	if got := g.Value(long); got != "other" {
		t.Fatalf("overlong value = %q, want other", got)
	}
}

// TestLabelGuardWiredIntoMetrics：护栏真的作用在写标签前（变异：删护栏 →
// model 序列数会变成 3 而不是 2+other）。
func TestLabelGuardWiredIntoMetrics(t *testing.T) {
	reg, set := NewRegistry()
	set.guardModel = newLabelGuard(2)
	for _, model := range []string{"m1", "m2", "m3"} {
		set.ObserveRelayRequest("chat_completions", model, false, "success", time.Millisecond)
	}
	_, parsed := gatherText(t, reg)
	fam := parsed["newtg_relay_requests_total"]
	if len(fam.Metric) != 3 {
		t.Fatalf("series = %d, want 3 (m1,m2,other)", len(fam.Metric))
	}
	models := map[string]bool{}
	for _, m := range fam.Metric {
		for _, pair := range m.Label {
			if pair.GetName() == "model" {
				models[pair.GetValue()] = true
			}
		}
	}
	if !models["other"] || models["m3"] {
		t.Fatalf("models = %v, want other and not m3", models)
	}
}

// TestHTTPMiddlewareNoSensitiveLabels：路由取模板，原始 path/查询串/邮箱
// 不得进入任何标签；同时验证状态类与 Flusher 透传。
func TestHTTPMiddlewareNoSensitiveLabels(t *testing.T) {
	reg, set := NewRegistry()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/users/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	})
	h := set.HTTPMiddleware("gateway", mux)

	req := httptest.NewRequest(http.MethodGet,
		"/v1/users/123456?token=supersecret&email=alice@example.com", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rec.Code)
	}

	text, parsed := gatherText(t, reg)
	for _, forbidden := range []string{"123456", "supersecret", "alice@example.com", "user_id", "request_id"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("exposition leaked %q:\n%s", forbidden, text)
		}
	}
	m := findMetric(t, parsed, "newtg_http_requests_total",
		map[string]string{"route": "GET /v1/users/{id}", "status_class": "5xx", "method": "GET"})
	if m == nil {
		t.Fatalf("expected templated route/status_class labels:\n%s", text)
	}
	inflight := findMetric(t, parsed, "newtg_http_in_flight_requests", map[string]string{"service": "gateway"})
	if inflight.GetGauge().GetValue() != 0 {
		t.Fatalf("in_flight must return to 0, got %v", inflight.GetGauge().GetValue())
	}
}

// TestUnmatchedRouteFallsBack：未匹配路由归 unmatched，绝不回退原始 path。
func TestUnmatchedRouteFallsBack(t *testing.T) {
	reg, set := NewRegistry()
	h := set.HTTPMiddleware("gateway", http.NotFoundHandler())
	req := httptest.NewRequest(http.MethodGet, "/no/such/987654", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)

	text, parsed := gatherText(t, reg)
	if strings.Contains(text, "987654") {
		t.Fatalf("raw path leaked: %s", text)
	}
	if findMetric(t, parsed, "newtg_http_requests_total",
		map[string]string{"route": "unmatched", "status_class": "4xx"}) == nil {
		t.Fatalf("expected route=unmatched 4xx:\n%s", text)
	}
}

// TestMetricsHandlerContentType：/metrics 文本格式与 Content-Type 冻结。
func TestMetricsHandlerContentType(t *testing.T) {
	_, set := NewRegistry()
	set.SetWorkerStreamLag(0)
	// Handler 使用默认 registry；用 Default() 确保样本存在。
	populateAll(Default())
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("content-type = %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "newtg_dependency_up") {
		t.Fatalf("body missing dependency_up: %s", rec.Body.String())
	}
}

// TestObserveRelayAttemptSuccessDoesNotCountError：C-1 回归。
// 成功尝试即使误传 error_class="unknown" 也不得递增 errors_total，
// 否则 A3 错误率恒 ~100%。
func TestObserveRelayAttemptSuccessDoesNotCountError(t *testing.T) {
	reg, set := NewRegistry()
	set.ObserveRelayAttempt("chat_completions", "m", false, 1, "openai", "",
		"ok", "unknown", 0, time.Millisecond)
	_, parsed := gatherText(t, reg)
	if fam := parsed["newtg_relay_errors_total"]; fam != nil && len(fam.Metric) > 0 {
		t.Fatalf("success attempt must not increment errors_total: %v", fam.Metric)
	}

	set.ObserveRelayAttempt("chat_completions", "m", false, 1, "openai", "",
		"nonretryable_fail", "upstream_auth", 0, time.Millisecond)
	_, parsed = gatherText(t, reg)
	count, ok := counterSum(parsed, "newtg_relay_errors_total",
		map[string]string{"error_class": "upstream_auth"})
	if !ok || count != 1 {
		t.Fatalf("failure attempt errors_total = %v (ok=%v), want 1", count, ok)
	}
}

// TestHTTPMethodLabelBounded：I-1 回归。自定义 method 不得制造无界序列。
func TestHTTPMethodLabelBounded(t *testing.T) {
	reg, set := NewRegistry()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {})
	h := set.HTTPMiddleware("gateway", mux)
	for i := 0; i < 500; i++ {
		req := httptest.NewRequest("X-CUSTOM-"+strconv.Itoa(i), "/", nil)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	text, parsed := gatherText(t, reg)
	if strings.Contains(text, "X-CUSTOM") {
		t.Fatal("custom method leaked into exposition")
	}
	fam := parsed["newtg_http_requests_total"]
	if fam == nil {
		t.Fatal("http requests family missing")
	}
	methods := map[string]bool{}
	for _, m := range fam.Metric {
		for _, pair := range m.Label {
			if pair.GetName() == "method" {
				methods[pair.GetValue()] = true
			}
		}
	}
	if len(methods) != 1 || !methods["other"] {
		t.Fatalf("method labels = %v, want only other", methods)
	}
}

// TestGuardConstantsFrozen：M-4，护栏阈值是冻结口径，改动必须同步测试。
func TestGuardConstantsFrozen(t *testing.T) {
	if guardLimit != 5000 {
		t.Fatalf("guardLimit = %d, want 5000", guardLimit)
	}
	if maxLabelValueLen != 128 {
		t.Fatalf("maxLabelValueLen = %d, want 128", maxLabelValueLen)
	}
}

// TestInitWorkerDoesNotSeedLastSuccess：I-5，启动时不得预置 last_success。
func TestInitWorkerDoesNotSeedLastSuccess(t *testing.T) {
	reg, set := NewRegistry()
	set.InitWorker()
	_, parsed := gatherText(t, reg)
	if fam := parsed["newtg_worker_task_last_success_timestamp_seconds"]; fam != nil {
		t.Fatalf("InitWorker must not seed last_success: %v", fam.Metric)
	}
	if fam := parsed["newtg_worker_task_runs_total"]; fam == nil ||
		findMetric(t, parsed, "newtg_worker_task_runs_total",
			map[string]string{"task_type": "preconsume_reconcile"}) == nil {
		t.Fatal("InitWorker must pre-create preconsume_reconcile task runs")
	}

	set.ObserveWorkerTask("preconsume_reconcile", "succeeded", time.Millisecond)
	_, parsed = gatherText(t, reg)
	m := findMetric(t, parsed, "newtg_worker_task_last_success_timestamp_seconds",
		map[string]string{"task_type": "preconsume_reconcile"})
	if m == nil || m.GetGauge().GetValue() <= 0 {
		t.Fatalf("last_success not set after a real success: %v", m)
	}
}

// counterSum 返回匹配标签子集的 counter 值之和；family 不存在返回 ok=false。
func counterSum(families map[string]*dto.MetricFamily, name string,
	labels map[string]string) (float64, bool) {
	fam, ok := families[name]
	if !ok {
		return 0, false
	}
	total := 0.0
	matched := false
	for _, m := range fam.Metric {
		match := true
		for wantName, wantValue := range labels {
			found := false
			for _, pair := range m.Label {
				if pair.GetName() == wantName {
					found = pair.GetValue() == wantValue
					break
				}
			}
			if !found {
				match = false
				break
			}
		}
		if match {
			matched = true
			total += m.GetCounter().GetValue()
		}
	}
	return total, matched
}

// findMetric 按标签子集查找一个样本；找不到返回 nil。
func findMetric(t *testing.T, families map[string]*dto.MetricFamily, name string,
	labels map[string]string) *dto.Metric {
	t.Helper()
	fam, ok := families[name]
	if !ok {
		return nil
	}
	for _, m := range fam.Metric {
		match := true
		for wantName, wantValue := range labels {
			found := false
			for _, pair := range m.Label {
				if pair.GetName() == wantName {
					found = pair.GetValue() == wantValue
					break
				}
			}
			if !found {
				match = false
				break
			}
		}
		if match {
			return m
		}
	}
	return nil
}

// instanceLabelOf 取样本上的 instance 标签值；不存在时返回空串与 false。
func instanceLabelOf(m *dto.Metric) (string, bool) {
	for _, pair := range m.GetLabel() {
		if pair.GetName() == "instance" {
			return pair.GetValue(), true
		}
	}
	return "", false
}

// TestNewRegistryInstanceLabel：显式传入 instance 时，所有产出样本的 family
// （含 go_* / process_* 标准采集器与 newtg_build_info）都必须带常量
// instance 标签，让 Prometheus 序列能与 /api/instances 的心跳 id 对齐。
func TestNewRegistryInstanceLabel(t *testing.T) {
	const want = "gw-7f9c"
	reg, set := NewRegistry(want)
	set.ObserveHTTP("gateway", "GET", "GET /api/status", 200, time.Millisecond, 1)

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var sawBuildInfo, sawGo, sawProcess, sawHTTP bool
	for _, mf := range families {
		samples := mf.GetMetric()
		if len(samples) == 0 {
			continue
		}
		switch name := mf.GetName(); {
		case name == "newtg_build_info":
			sawBuildInfo = true
		case name == "newtg_http_requests_total":
			sawHTTP = true
		case strings.HasPrefix(name, "go_"):
			sawGo = true
		case strings.HasPrefix(name, "process_"):
			sawProcess = true
		}
		for i, m := range samples {
			got, ok := instanceLabelOf(m)
			if !ok || got != want {
				t.Errorf("%s[%d] instance label = %q (present=%v), want %q",
					mf.GetName(), i, got, ok, want)
			}
		}
	}
	if !sawBuildInfo || !sawHTTP || !sawGo || !sawProcess {
		t.Errorf("expected families missing: build_info=%v http_requests=%v go_*=%v process_*=%v",
			sawBuildInfo, sawHTTP, sawGo, sawProcess)
	}
}

// TestNewRegistryWithoutInstanceKeepsTodayShape：不传 instance 时保持现状——
// 不注入任何 instance 标签，newtg_build_info 的标签集仍是构建信息五元组。
func TestNewRegistryWithoutInstanceKeepsTodayShape(t *testing.T) {
	reg, _ := NewRegistry()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var buildInfo *dto.MetricFamily
	for _, mf := range families {
		if mf.GetName() == "newtg_build_info" {
			buildInfo = mf
		}
		for i, m := range mf.GetMetric() {
			if got, ok := instanceLabelOf(m); ok {
				t.Errorf("%s[%d] unexpectedly has instance=%q", mf.GetName(), i, got)
			}
		}
	}
	if buildInfo == nil || len(buildInfo.GetMetric()) == 0 {
		t.Fatal("newtg_build_info family missing or empty")
	}
	// 标签顺序按字典序由 client_golang 输出，这里比较标签集合。
	want := map[string]bool{
		"version": true, "commit": true, "goversion": true, "goos": true, "goarch": true,
	}
	labels := buildInfo.GetMetric()[0].GetLabel()
	if len(labels) != len(want) {
		t.Fatalf("newtg_build_info labels = %v, want keys %v", labels, want)
	}
	for _, pair := range labels {
		if !want[pair.GetName()] {
			t.Errorf("unexpected newtg_build_info label %q", pair.GetName())
		}
	}
}

// ---- 连接池 collector 的测试替身：不发起任何真实连接 ----

type stubConnector struct{}

func (stubConnector) Connect(context.Context) (driver.Conn, error) { return stubConn{}, nil }
func (stubConnector) Driver() driver.Driver                        { return stubDriver{} }

type stubDriver struct{}

func (stubDriver) Open(string) (driver.Conn, error) { return stubConn{}, nil }

type stubConn struct{}

func (stubConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("stub: prepare unsupported")
}
func (stubConn) Close() error              { return nil }
func (stubConn) Begin() (driver.Tx, error) { return nil, errors.New("stub: begin unsupported") }
