package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"newtg/gateway/internal/perfquery"
	"newtg/gateway/internal/rbac"
)

// 本文件覆盖 GET /api/performance/overview 的形状与降级口径（spec §7.2）：
// 假 Prometheus 用 httptest.NewServer 按 query 参数返回固定矩阵，心跳用
// Instances 接缝注入——两条依赖都可以在内存里精确控制，不需要 miniredis
// 或真实监控栈。

// overviewTestNow 是 handler 的时钟，固定后可以让心跳新鲜度与序列时间戳
// 都可断言。
var overviewTestNow = time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)

// fakeOverviewProm 是假 Prometheus：按 PromQL 反查查询名，再从 values 取
// 固定样本值生成矩阵；fail 里的查询返回 status=error。
type fakeOverviewProm struct {
	mu       sync.Mutex
	requests int
	values   map[string]string // 查询名 → 样本值
	fail     map[string]bool   // 查询名 → 返回 status=error
	points   int
}

func newFakeOverviewProm() *fakeOverviewProm {
	return &fakeOverviewProm{
		values: overviewTestValues(),
		fail:   map[string]bool{},
		points: 24,
	}
}

func (p *fakeOverviewProm) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.requests
}

func (p *fakeOverviewProm) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("query")
	name := nameForPromQL(query)

	p.mu.Lock()
	p.requests++
	value, ok := p.values[name]
	fail := p.fail[name]
	points := p.points
	p.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	switch {
	case name == "":
		_, _ = fmt.Fprint(w, `{"status":"error","error":"unexpected query"}`)
		return
	case fail:
		_, _ = fmt.Fprintf(w, `{"status":"error","error":"boom: %s"}`, name)
		return
	}
	if !ok {
		value = "1"
	}

	end, err := strconv.ParseInt(r.URL.Query().Get("end"), 10, 64)
	if err != nil {
		http.Error(w, "bad end", http.StatusBadRequest)
		return
	}
	step, err := strconv.ParseFloat(r.URL.Query().Get("step"), 64)
	if err != nil || step <= 0 {
		http.Error(w, "bad step", http.StatusBadRequest)
		return
	}

	var values strings.Builder
	values.WriteByte('[')
	for i := 0; i < points; i++ {
		if i > 0 {
			values.WriteByte(',')
		}
		timestamp := end - int64(float64(points-1-i)*step)
		fmt.Fprintf(&values, `[%d,%q]`, timestamp, value)
	}
	values.WriteByte(']')

	// 逐副本查询带 instance 标签；其它查询的标签集无关紧要。
	metric := "{}"
	if strings.Contains(query, "by (instance") {
		metric = `{"instance":"gw-a"}`
	}
	_, _ = fmt.Fprintf(w,
		`{"status":"success","data":{"resultType":"matrix","result":[{"metric":%s,"values":%s}]}}`,
		metric, values.String())
}

// nameForPromQL 反查白名单：测试通过 PromQL 精确识别查询名，从而复用它
// 验证 handler 用的是目录里的 PromQL（而不是自己拼的字符串）。
func nameForPromQL(promQL string) string {
	for _, name := range perfquery.Names() {
		if registered, _, ok := perfquery.Lookup(name); ok && registered == promQL {
			return name
		}
	}
	return ""
}

// overviewTestValues 是各指标的健康样本值：网关 p95 0.3s、5xx 0.2%、
// 连接池 11/25、Redis 内存 21.3%、主机 CPU 20% / 内存 68% / 磁盘 71%——
// 全部落在 ok 区间，便于「单条查询失败」用例断言其余仍为 ok。
func overviewTestValues() map[string]string {
	return map[string]string{
		perfquery.NameGatewayRPS:                  "21.4",
		perfquery.NameGatewayP95:                  "0.3",
		perfquery.NameGatewayErrorRate:            "0.002",
		perfquery.NameGatewayInflight:             "8",
		perfquery.NameDBPool:                      "11",
		perfquery.NameDBWait:                      "0",
		perfquery.NamePGBackends:                  "11",
		perfquery.NamePGMaxBackends:               "25",
		perfquery.NamePGTPS:                       "46.2",
		perfquery.NamePGSize:                      "1048576",
		perfquery.NameRedisUp:                     "1",
		perfquery.NameRedisClients:                "18",
		perfquery.NameRedisMem:                    "0.213",
		perfquery.NameRedisHitRate:                "0.962",
		perfquery.NameRedisCmdRate:                "238",
		perfquery.NameHostCPU:                     "0.2",
		perfquery.NameHostMem:                     "0.68",
		perfquery.NameHostDisk:                    "0.71",
		perfquery.NameHostNetIn:                   "0",
		perfquery.NameHostNetOut:                  "0",
		perfquery.NameTargetUp:                    "1",
		perfquery.NameGatewayByInstanceRPS:        "21.4",
		perfquery.NameGatewayByInstanceP95:        "0.25",
		perfquery.NameGatewayByInstanceInflight:   "8",
		perfquery.NameGatewayByInstanceGoroutines: "61",
		perfquery.NameGatewayByInstanceHeapMB:     "166",
		perfquery.NameGatewayByInstanceErrorRate:  "0.002",
	}
}

// overviewInstance 构造一条心跳记录。
func overviewInstance(id, role string, lastSeen time.Time) InstanceRecord {
	return InstanceRecord{
		InstanceID: id,
		Status:     instanceStatusLive,
		Role:       role,
		Version:    "test",
		LastSeen:   lastSeen,
	}
}

func overviewMux(t *testing.T, promURL string, instances func(context.Context) ([]InstanceRecord, error)) *http.ServeMux {
	t.Helper()
	handlers := NewPerformanceHandlers(PerformanceHandlerConfig{
		Logger:    testLogger(),
		Authz:     rbac.NewAuthzMiddleware(),
		Now:       func() time.Time { return overviewTestNow },
		Getenv:    func(string) string { return "" },
		Hostname:  func() (string, error) { return "test-host", nil },
		Prom:      perfquery.New(perfquery.Config{BaseURL: promURL}),
		PromURL:   promURL,
		Instances: instances,
	})
	mux := http.NewServeMux()
	handlers.Register(mux)
	return mux
}

func getOverview(t *testing.T, mux *http.ServeMux, target string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, perfRequest(http.MethodGet, target, "", nil))
	return rr
}

func decodeOverview(t *testing.T, rr *httptest.ResponseRecorder) perfOverviewResponse {
	t.Helper()
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	var resp perfOverviewResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode overview: %v (body=%s)", err, rr.Body.String())
	}
	return resp
}

func vitalByKey(t *testing.T, resp perfOverviewResponse, key string) perfVital {
	t.Helper()
	for _, vital := range resp.Vitals {
		if vital.Key == key {
			return vital
		}
	}
	t.Fatalf("vital %q 不存在（现有 %v）", key, vitalKeys(resp.Vitals))
	return perfVital{}
}

func vitalKeys(vitals []perfVital) []string {
	keys := make([]string, 0, len(vitals))
	for _, vital := range vitals {
		keys = append(keys, vital.Key)
	}
	return keys
}

func TestOverviewShapes(t *testing.T) {
	fake := newFakeOverviewProm()
	srv := httptest.NewServer(fake)
	defer srv.Close()

	mux := overviewMux(t, srv.URL, func(context.Context) ([]InstanceRecord, error) {
		return []InstanceRecord{overviewInstance("gw-a", instanceRoleGateway, overviewTestNow.Add(-10*time.Second))}, nil
	})

	resp := decodeOverview(t, getOverview(t, mux, "/api/performance/overview"))

	if resp.Range != "1h" {
		t.Errorf("Range = %q, want 1h（缺省窗口）", resp.Range)
	}
	if resp.Windows.Points != 24 || resp.Windows.StepSeconds != 150 {
		t.Errorf("windows = %+v, want points=24 step=150", resp.Windows)
	}
	if !resp.GeneratedAt.Equal(overviewTestNow) {
		t.Errorf("generated_at = %v, want %v", resp.GeneratedAt, overviewTestNow)
	}

	if len(resp.Vitals) != 4 {
		t.Fatalf("vitals 数 = %d, want 4 (%v)", len(resp.Vitals), vitalKeys(resp.Vitals))
	}
	wantKeys := []string{
		perfquery.NameGatewayRPS,
		perfquery.NameDBPool,
		perfquery.NameRedisHitRate,
		perfquery.NameHostDisk,
	}
	validStatus := map[string]bool{
		perfStatusOK: true, perfStatusWarn: true, perfStatusCrit: true, perfStatusUnknown: true,
	}
	for i, key := range wantKeys {
		vital := resp.Vitals[i]
		if vital.Key != key {
			t.Errorf("vitals[%d].key = %q, want %q", i, vital.Key, key)
		}
		if _, unit, _ := perfquery.Lookup(key); vital.Unit != unit {
			t.Errorf("%s: unit = %q, want 目录单位 %q", key, vital.Unit, unit)
		}
		if !validStatus[vital.Status] {
			t.Errorf("%s: status = %q 不在 {ok,warn,crit,unknown}", key, vital.Status)
		}
		if vital.Value == nil {
			t.Errorf("%s: value = nil, want 有值", key)
		}
		if len(vital.Series) != resp.Windows.Points {
			t.Errorf("%s: series 点数 = %d, want %d", key, len(vital.Series), resp.Windows.Points)
		}
		if vital.Status != perfStatusOK {
			t.Errorf("%s: status = %q, want ok（样本值都落在 ok 区间）", key, vital.Status)
		}
	}

	gateway := vitalByKey(t, resp, perfquery.NameGatewayRPS)
	if gateway.Value == nil || *gateway.Value != 21.4 {
		t.Errorf("gateway.rps value = %v, want 21.4", gateway.Value)
	}
	if gateway.Note == "" {
		t.Error("gateway.rps note 为空：副标题应带 p95 与 5xx")
	}

	// series 的点是 [unix 秒, 值]，且按时间升序。
	if len(gateway.Series) > 1 && gateway.Series[0][0] >= gateway.Series[len(gateway.Series)-1][0] {
		t.Errorf("series 未按时间升序: %v", gateway.Series)
	}

	if len(resp.Sources) == 0 || resp.Sources[0].Name != perfSourcePrometheus || resp.Sources[0].Status != perfStatusOK {
		t.Errorf("sources = %+v, want prometheus=ok", resp.Sources)
	}

	if len(resp.Gateways) != 1 {
		t.Fatalf("gateways = %+v, want 1 行", resp.Gateways)
	}
	row := resp.Gateways[0]
	if row.InstanceID != "gw-a" || row.Metrics == nil || row.MetricsStatus != perfStatusOK {
		t.Errorf("gateway row = %+v, want gw-a + metrics ok", row)
	}
	if row.Metrics.Goroutines == nil || *row.Metrics.Goroutines != 61 {
		t.Errorf("goroutines = %v, want 61", row.Metrics.Goroutines)
	}
	if row.Metrics.P95MS == nil || *row.Metrics.P95MS != 250 {
		t.Errorf("p95_ms = %v, want 250（目录单位是秒）", row.Metrics.P95MS)
	}

	// resources：host / postgres / redis 三段都要有值，postgres 必须含
	// lock_wait（spec §7.2 的示例字段）。
	if resp.Resources.Host.CPU != 0.2 || resp.Resources.Host.Disk != 0.71 {
		t.Errorf("host resources = %+v", resp.Resources.Host)
	}
	if resp.Resources.Postgres.Backends != 11 || resp.Resources.Postgres.Max != 25 || resp.Resources.Postgres.LockWait != 0 {
		t.Errorf("postgres resources = %+v", resp.Resources.Postgres)
	}
	if resp.Resources.Redis.Up != 1 || resp.Resources.Redis.HitRate != 0.962 {
		t.Errorf("redis resources = %+v", resp.Resources.Redis)
	}
}

func TestOverviewDegradesWhenPrometheusUnset(t *testing.T) {
	mux := overviewMux(t, "", func(context.Context) ([]InstanceRecord, error) {
		return []InstanceRecord{overviewInstance("gw-a", instanceRoleGateway, overviewTestNow.Add(-10*time.Second))}, nil
	})

	rr := getOverview(t, mux, "/api/performance/overview")
	resp := decodeOverview(t, rr)

	if len(resp.Sources) == 0 || resp.Sources[0].Name != perfSourcePrometheus ||
		resp.Sources[0].Status != perfSourceUnavailable {
		t.Fatalf("sources = %+v, want prometheus=unavailable", resp.Sources)
	}
	for _, vital := range resp.Vitals {
		if vital.Value != nil {
			t.Errorf("%s: value = %v, want nil", vital.Key, *vital.Value)
		}
		if vital.Status != perfStatusUnknown {
			t.Errorf("%s: status = %q, want unknown", vital.Key, vital.Status)
		}
		if len(vital.Series) != 0 {
			t.Errorf("%s: series 非空: %v", vital.Key, vital.Series)
		}
	}
	// series 必须是 []，不是 null（前端 .map 不判空）。
	if strings.Contains(rr.Body.String(), `"series":null`) {
		t.Errorf("series 编码成了 null: %s", rr.Body.String())
	}

	// 监控未接入不影响心跳：副本行仍在，只是没有进程数字。
	if len(resp.Gateways) != 1 || resp.Gateways[0].InstanceID != "gw-a" {
		t.Fatalf("gateways = %+v, want 心跳行 gw-a", resp.Gateways)
	}
	if resp.Gateways[0].Metrics != nil {
		t.Errorf("metrics = %+v, want nil", resp.Gateways[0].Metrics)
	}
	if resp.Gateways[0].MetricsStatus != perfStatusUnknown {
		t.Errorf("metrics_status = %q, want unknown", resp.Gateways[0].MetricsStatus)
	}
	if !resp.Gateways[0].HeartbeatFresh {
		t.Error("heartbeat_fresh = false, want true（10s 前的心跳）")
	}

	// 资源数字在未接入时按 0 输出，保持响应形状稳定。
	if resp.Resources.Host.CPU != 0 || resp.Resources.Redis.Up != 0 {
		t.Errorf("resources 未接入时应为 0: %+v", resp.Resources)
	}
}

func TestOverviewSingleQueryFailureIsLocal(t *testing.T) {
	fake := newFakeOverviewProm()
	fake.fail[perfquery.NameGatewayP95] = true
	srv := httptest.NewServer(fake)
	defer srv.Close()

	mux := overviewMux(t, srv.URL, func(context.Context) ([]InstanceRecord, error) {
		return []InstanceRecord{overviewInstance("gw-a", instanceRoleGateway, overviewTestNow.Add(-10*time.Second))}, nil
	})

	resp := decodeOverview(t, getOverview(t, mux, "/api/performance/overview"))

	// gateway.p95 是网关格的状态输入：它失败时整格 unknown（缺了 p95 就
	// 无法判定这个域健康与否），其余格不受影响。
	gateway := vitalByKey(t, resp, perfquery.NameGatewayRPS)
	if gateway.Status != perfStatusUnknown {
		t.Errorf("gateway.rps status = %q, want unknown（p95 查询失败）", gateway.Status)
	}
	if gateway.Value == nil || *gateway.Value != 21.4 {
		t.Errorf("gateway.rps value = %v, want 21.4（headline 仍有数据）", gateway.Value)
	}
	if strings.Contains(gateway.Note, "P95") {
		t.Errorf("P95 查询失败时 note 不应含 P95: %q", gateway.Note)
	}
	for _, key := range []string{perfquery.NameDBPool, perfquery.NameRedisHitRate, perfquery.NameHostDisk} {
		vital := vitalByKey(t, resp, key)
		if vital.Status != perfStatusOK {
			t.Errorf("%s: status = %q, want ok（单条失败只影响对应格）", key, vital.Status)
		}
	}

	// 数据源只能用 target.up 判健康：单条 vital 查询失败仍是 ok。
	if resp.Sources[0].Status != perfStatusOK {
		t.Errorf("sources[0].status = %q, want ok", resp.Sources[0].Status)
	}
	// 逐副本 p95 是另一条查询，网关行的指标不受影响。
	if len(resp.Gateways) != 1 || resp.Gateways[0].Metrics == nil || resp.Gateways[0].MetricsStatus != perfStatusOK {
		t.Errorf("gateways = %+v, want gw-a metrics ok", resp.Gateways)
	}
}

func TestOverviewRangeValidation(t *testing.T) {
	fake := newFakeOverviewProm()
	srv := httptest.NewServer(fake)
	defer srv.Close()

	mux := overviewMux(t, srv.URL, func(context.Context) ([]InstanceRecord, error) {
		return []InstanceRecord{}, nil
	})

	rr := getOverview(t, mux, "/api/performance/overview?range=7d")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("range=7d status = %d, want 400 (body=%s)", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Content-Type"); !strings.Contains(got, "application/problem+json") {
		t.Errorf("Content-Type = %q, want application/problem+json", got)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "invalid_range") {
		t.Errorf("body 缺少 error code invalid_range: %s", body)
	}
	if !strings.Contains(body, "invalid_value") {
		t.Errorf("body 缺少 problem type invalid_value: %s", body)
	}

	// 四个合法窗口都接受；15m 的点数是 15。
	for rangeName, wantPoints := range map[string]int{"15m": 15, "1h": 24, "6h": 24, "24h": 24} {
		resp := decodeOverview(t, getOverview(t, mux, "/api/performance/overview?range="+rangeName))
		if resp.Range != rangeName {
			t.Errorf("range=%s: Range = %q", rangeName, resp.Range)
		}
		if resp.Windows.Points != wantPoints {
			t.Errorf("range=%s: points = %d, want %d", rangeName, resp.Windows.Points, wantPoints)
		}
	}
}

func TestOverviewInstanceAlignment(t *testing.T) {
	fake := newFakeOverviewProm()
	srv := httptest.NewServer(fake)
	defer srv.Close()

	mux := overviewMux(t, srv.URL, func(context.Context) ([]InstanceRecord, error) {
		return []InstanceRecord{
			// gw-a 同时有心跳与指标（假 Prometheus 只返回 instance=gw-a）。
			overviewInstance("gw-a", instanceRoleGateway, overviewTestNow.Add(-5*time.Second)),
			// gw-b 只有心跳。
			overviewInstance("gw-b", instanceRoleGateway, overviewTestNow.Add(-5*time.Second)),
			// worker 角色不进性能页的网关区。
			overviewInstance("worker-1", instanceRoleWorker, overviewTestNow.Add(-5*time.Second)),
		}, nil
	})

	resp := decodeOverview(t, getOverview(t, mux, "/api/performance/overview"))

	if len(resp.Gateways) != 2 {
		t.Fatalf("gateways = %+v, want gw-a 与 gw-b 两行", resp.Gateways)
	}
	if resp.Gateways[0].InstanceID != "gw-a" || resp.Gateways[1].InstanceID != "gw-b" {
		t.Fatalf("gateways 顺序 = [%s %s], want [gw-a gw-b]（按 instance_id 排序）",
			resp.Gateways[0].InstanceID, resp.Gateways[1].InstanceID)
	}

	aligned := resp.Gateways[0]
	if aligned.Metrics == nil || aligned.MetricsStatus != perfStatusOK {
		t.Errorf("gw-a metrics = %+v status=%q, want 有指标且 ok", aligned.Metrics, aligned.MetricsStatus)
	}
	if aligned.LastSeen == nil || !aligned.HeartbeatFresh {
		t.Errorf("gw-a last_seen/heartbeat_fresh = %v/%v, want 有值且新鲜", aligned.LastSeen, aligned.HeartbeatFresh)
	}

	heartbeatOnly := resp.Gateways[1]
	if heartbeatOnly.Metrics != nil {
		t.Errorf("gw-b metrics = %+v, want nil", heartbeatOnly.Metrics)
	}
	if heartbeatOnly.MetricsStatus != perfStatusUnknown {
		t.Errorf("gw-b metrics_status = %q, want unknown", heartbeatOnly.MetricsStatus)
	}
	if !heartbeatOnly.HeartbeatFresh {
		t.Error("gw-b heartbeat_fresh = false, want true")
	}
}

// TestOverviewHeartbeatFailureIsNot5xx 钉住「Redis 不可用不得拖垮端点」：
// 心跳读取失败时 200、sources 多一条 heartbeat=error、网关区只留指标行。
func TestOverviewHeartbeatFailureIsNot5xx(t *testing.T) {
	fake := newFakeOverviewProm()
	srv := httptest.NewServer(fake)
	defer srv.Close()

	mux := overviewMux(t, srv.URL, func(context.Context) ([]InstanceRecord, error) {
		return nil, fmt.Errorf("redis: connection refused")
	})

	resp := decodeOverview(t, getOverview(t, mux, "/api/performance/overview"))

	if len(resp.Sources) != 2 || resp.Sources[1].Name != perfSourceHeartbeat ||
		resp.Sources[1].Status != perfSourceError {
		t.Fatalf("sources = %+v, want prometheus + heartbeat=error", resp.Sources)
	}
	if resp.Sources[0].Status != perfStatusOK {
		t.Errorf("prometheus source = %q, want ok（心跳失败不影响指标源）", resp.Sources[0].Status)
	}
	if len(resp.Gateways) != 1 || resp.Gateways[0].InstanceID != "gw-a" {
		t.Fatalf("gateways = %+v, want 只列有指标的 gw-a", resp.Gateways)
	}
	if resp.Gateways[0].HeartbeatFresh {
		t.Error("指标行的 heartbeat_fresh = true, want false（没有心跳）")
	}
	if resp.Gateways[0].LastSeen != nil {
		t.Error("指标行的 last_seen 应见 null")
	}
}

// TestOverviewDropsNaNValues 钉住 NaN 剔除：Prometheus 对空直方图返回 "NaN"，
// JSON 编码器无法编码 NaN；handler 必须丢弃该样本，整条序列全是 NaN 时
// 返回 value=null / status=unknown / series=[]。
func TestOverviewDropsNaNValues(t *testing.T) {
	fake := newFakeOverviewProm()
	fake.values[perfquery.NameGatewayRPS] = "NaN"
	fake.values[perfquery.NameHostCPU] = "+Inf"
	srv := httptest.NewServer(fake)
	defer srv.Close()

	mux := overviewMux(t, srv.URL, func(context.Context) ([]InstanceRecord, error) {
		return []InstanceRecord{}, nil
	})

	rr := getOverview(t, mux, "/api/performance/overview")
	resp := decodeOverview(t, rr)

	rps := vitalByKey(t, resp, perfquery.NameGatewayRPS)
	if rps.Value != nil || rps.Status != perfStatusUnknown || len(rps.Series) != 0 {
		t.Errorf("gateway.rps = %+v, want value=nil status=unknown series=[]", rps)
	}
	if resp.Resources.Host.CPU != 0 {
		t.Errorf("host.cpu = %v, want 0（+Inf 被丢弃）", resp.Resources.Host.CPU)
	}
}
