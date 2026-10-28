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
// 固定样本值生成矩阵；fail 里的查询返回 status=error。instances 可以按
// 查询名覆盖返回的 instance 列表（默认 gw-a），用来验证「哪些查询有权发现
// 副本」。
type fakeOverviewProm struct {
	mu        sync.Mutex
	requests  int
	values    map[string]string   // 查询名 → 样本值
	fail      map[string]bool     // 查询名 → 返回 status=error
	instances map[string][]string // 查询名 → 返回的 instance 列表
	points    int
}

func newFakeOverviewProm() *fakeOverviewProm {
	return &fakeOverviewProm{
		values:    overviewTestValues(),
		fail:      map[string]bool{},
		instances: map[string][]string{},
		points:    24,
	}
}

// setInstances 覆盖某条查询返回的 instance 列表（空列表表示返回空结果）。
func (p *fakeOverviewProm) setInstances(name string, instances ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.instances[name] = instances
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
	instances, hasInstances := p.instances[name]
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

	// 逐副本查询默认带 instance 标签（gw-a）；instances 覆盖时按给定列表
	// 返回多条序列。其它查询的标签集无关紧要。
	var result []string
	switch {
	case hasInstances:
		for _, instance := range instances {
			result = append(result, fmt.Sprintf(`{"metric":{"instance":%q},"values":%s}`, instance, values.String()))
		}
	case strings.Contains(query, "by (instance"):
		result = append(result, fmt.Sprintf(`{"metric":{"instance":"gw-a"},"values":%s}`, values.String()))
	default:
		result = append(result, fmt.Sprintf(`{"metric":{},"values":%s}`, values.String()))
	}
	_, _ = fmt.Fprintf(w,
		`{"status":"success","data":{"resultType":"matrix","result":[%s]}}`,
		strings.Join(result, ","))
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

// vitalDetail 取体检格里指定 key 的副标题数字；不存在直接失败。
func vitalDetail(t *testing.T, vital perfVital, key string) perfVitalDetail {
	t.Helper()
	for _, detail := range vital.Details {
		if detail.Key == key {
			return detail
		}
	}
	t.Fatalf("%s: details 缺少 %q（现有 %v）", vital.Key, key, detailKeys(vital.Details))
	return perfVitalDetail{}
}

func detailKeys(details []perfVitalDetail) []string {
	keys := make([]string, 0, len(details))
	for _, detail := range details {
		keys = append(keys, detail.Key)
	}
	return keys
}

// hasDetail 报告体检格的 details 是否包含某个 key。
func hasDetail(vital perfVital, key string) bool {
	for _, detail := range vital.Details {
		if detail.Key == key {
			return true
		}
	}
	return false
}

func TestOverviewShapes(t *testing.T) {
	fake := newFakeOverviewProm()
	srv := httptest.NewServer(fake)
	defer srv.Close()

	mux := overviewMux(t, srv.URL, func(context.Context) ([]InstanceRecord, error) {
		return []InstanceRecord{overviewInstance("gw-a", instanceRoleGateway, overviewTestNow.Add(-10*time.Second))}, nil
	})

	rr := getOverview(t, mux, "/api/performance/overview")
	resp := decodeOverview(t, rr)

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
	// details 是结构化数字（键名冻结）：p95 用查询原值（秒），errors 是 ratio
	// 原值——不在这里乘 100，百分号由前端按语言格式化。
	if got := detailKeys(gateway.Details); len(got) != 2 || got[0] != "p95" || got[1] != "errors" {
		t.Errorf("gateway.rps details keys = %v, want [p95 errors]", got)
	}
	if p95 := vitalDetail(t, gateway, "p95"); p95.Value != 0.3 || p95.Unit != "s" {
		t.Errorf("gateway.rps p95 = %+v, want value=0.3 unit=s", p95)
	}
	if errorsDetail := vitalDetail(t, gateway, "errors"); errorsDetail.Value != 0.002 || errorsDetail.Unit != "ratio" {
		t.Errorf("gateway.rps errors = %+v, want value=0.002（原值，未乘 100）unit=ratio", errorsDetail)
	}

	// 数据库格带 tps / backends / max_backends 三条计数。
	db := vitalByKey(t, resp, perfquery.NameDBPool)
	if got := detailKeys(db.Details); len(got) != 3 || got[0] != "tps" || got[1] != "backends" || got[2] != "max_backends" {
		t.Errorf("db.pool details keys = %v, want [tps backends max_backends]", got)
	}
	if tps := vitalDetail(t, db, "tps"); tps.Value != 46.2 || tps.Unit != "count" {
		t.Errorf("db.pool tps = %+v, want 46.2/count", tps)
	}
	if backends := vitalDetail(t, db, "backends"); backends.Value != 11 || backends.Unit != "count" {
		t.Errorf("db.pool backends = %+v, want 11/count", backends)
	}
	if maxBackends := vitalDetail(t, db, "max_backends"); maxBackends.Value != 25 || maxBackends.Unit != "count" {
		t.Errorf("db.pool max_backends = %+v, want 25/count", maxBackends)
	}

	// Redis 格与主机格同样是原值：比例类（cpu/mem/disk）不乘 100。
	redis := vitalByKey(t, resp, perfquery.NameRedisHitRate)
	if ops := vitalDetail(t, redis, "ops"); ops.Value != 238 || ops.Unit != "count" {
		t.Errorf("redis.hit_rate ops = %+v, want 238/count", ops)
	}
	if clients := vitalDetail(t, redis, "clients"); clients.Value != 18 || clients.Unit != "count" {
		t.Errorf("redis.hit_rate clients = %+v, want 18/count", clients)
	}
	host := vitalByKey(t, resp, perfquery.NameHostDisk)
	for key, want := range map[string]float64{"cpu": 0.2, "mem": 0.68, "disk": 0.71} {
		if detail := vitalDetail(t, host, key); detail.Value != want || detail.Unit != "ratio" {
			t.Errorf("host.disk %s = %+v, want %v/ratio（原值，未乘 100）", key, detail, want)
		}
	}

	// 服务端不再拼英文副标题：响应里不得出现 note 键。
	if strings.Contains(rr.Body.String(), `"note":`) {
		t.Errorf("overview 响应仍含 note 键: %s", rr.Body.String())
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
	// lock_wait（spec §7.2 的示例字段）。资源数值现在可空，断言先判 nil
	// 再比 *v：nil 与真实 0 是两种不同状态。
	if resp.Resources.Host.CPU == nil || *resp.Resources.Host.CPU != 0.2 ||
		resp.Resources.Host.Disk == nil || *resp.Resources.Host.Disk != 0.71 {
		t.Errorf("host resources = %+v", resp.Resources.Host)
	}
	if resp.Resources.Postgres.Backends == nil || *resp.Resources.Postgres.Backends != 11 ||
		resp.Resources.Postgres.Max == nil || *resp.Resources.Postgres.Max != 25 {
		t.Errorf("postgres resources = %+v", resp.Resources.Postgres)
	}
	// fixture 里 NameDBWait = "0" 是**真实 0 样本**：必须保持 0，不能因为
	// 「可空」就把真实读数一起吞成 null。
	if resp.Resources.Postgres.LockWait == nil || *resp.Resources.Postgres.LockWait != 0 {
		t.Errorf("postgres lock_wait = %v, want 真实 0 样本（不是 null）", resp.Resources.Postgres.LockWait)
	}
	if resp.Resources.Redis.Up == nil || *resp.Resources.Redis.Up != 1 ||
		resp.Resources.Redis.HitRate == nil || *resp.Resources.Redis.HitRate != 0.962 {
		t.Errorf("redis resources = %+v", resp.Resources.Redis)
	}
}

// TestOverviewVitalDetailsSkipMissing 钉住「没有数据点的子指标不产出该条」：
// 用 NaN 模拟某项未暴露，details 里既不能出现该条，也不能拿 0 冒充。
func TestOverviewVitalDetailsSkipMissing(t *testing.T) {
	fake := newFakeOverviewProm()
	for _, name := range []string{
		perfquery.NameGatewayP95,
		perfquery.NamePGTPS,
		perfquery.NameRedisClients,
		perfquery.NameHostMem,
	} {
		fake.values[name] = "NaN"
	}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	mux := overviewMux(t, srv.URL, func(context.Context) ([]InstanceRecord, error) {
		return []InstanceRecord{}, nil
	})

	resp := decodeOverview(t, getOverview(t, mux, "/api/performance/overview"))

	gateway := vitalByKey(t, resp, perfquery.NameGatewayRPS)
	if hasDetail(gateway, "p95") {
		t.Errorf("gateway.rps p95 无数据点却出现在 details: %v", detailKeys(gateway.Details))
	}
	if !hasDetail(gateway, "errors") {
		t.Errorf("gateway.rps errors 有数据点却缺失: %v", detailKeys(gateway.Details))
	}

	db := vitalByKey(t, resp, perfquery.NameDBPool)
	if hasDetail(db, "tps") {
		t.Errorf("db.pool tps 无数据点却出现在 details: %v", detailKeys(db.Details))
	}
	for _, key := range []string{"backends", "max_backends"} {
		if !hasDetail(db, key) {
			t.Errorf("db.pool %s 有数据点却缺失: %v", key, detailKeys(db.Details))
		}
	}

	redis := vitalByKey(t, resp, perfquery.NameRedisHitRate)
	if hasDetail(redis, "clients") {
		t.Errorf("redis.hit_rate clients 无数据点却出现在 details: %v", detailKeys(redis.Details))
	}
	if !hasDetail(redis, "ops") {
		t.Errorf("redis.hit_rate ops 有数据点却缺失: %v", detailKeys(redis.Details))
	}

	host := vitalByKey(t, resp, perfquery.NameHostDisk)
	if hasDetail(host, "mem") {
		t.Errorf("host.disk mem 无数据点却出现在 details: %v", detailKeys(host.Details))
	}
	for _, key := range []string{"cpu", "disk"} {
		if !hasDetail(host, key) {
			t.Errorf("host.disk %s 有数据点却缺失: %v", key, detailKeys(host.Details))
		}
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
		// headline 缺失时 details 必须是空数组：前端按数组遍历，null 会崩。
		if vital.Details == nil {
			t.Errorf("%s: details = nil（编码成了 null），want []", vital.Key)
		}
		if len(vital.Details) != 0 {
			t.Errorf("%s: details = %v, want 空数组", vital.Key, vital.Details)
		}
	}
	// series 与 details 必须是 []，不是 null（前端 .map 不判空）。
	if strings.Contains(rr.Body.String(), `"series":null`) {
		t.Errorf("series 编码成了 null: %s", rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), `"details":null`) {
		t.Errorf("details 编码成了 null: %s", rr.Body.String())
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

	// 资源数字在未接入时为 null（未知），不再用 0 冒充。键仍在，值可空。
	if resp.Resources.Host.CPU != nil || resp.Resources.Redis.Up != nil {
		t.Errorf("resources 未接入时应为 null: %+v", resp.Resources)
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
	if hasDetail(gateway, "p95") {
		t.Errorf("P95 查询失败时 details 不应含 p95: %v", detailKeys(gateway.Details))
	}
	if errorsDetail := vitalDetail(t, gateway, "errors"); errorsDetail.Value != 0.002 || errorsDetail.Unit != "ratio" {
		t.Errorf("gateway.rps errors = %+v, want 0.002/ratio（错误率查询仍成功）", errorsDetail)
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
	// 参数校验发生在任何上游查询之前：非法 range 不得触发一次请求。
	if fake.count() != 0 {
		t.Fatalf("range=7d 向 Prometheus 发了 %d 个请求, want 0", fake.count())
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
	if resp.Resources.Host.CPU != nil {
		t.Errorf("host.cpu = %v, want null（+Inf 被丢弃）", resp.Resources.Host.CPU)
	}
}

// TestOverviewIgnoresNonGatewayInstances 钉住副本发现口径：只有带
// service="gateway" 过滤的逐副本查询可以新增 gateways 行。go_goroutines /
// go_memstats_heap_alloc_bytes 没有 service 过滤，worker、node-exporter 与
// Prometheus 自己都会出现——它们只能用于取值。
func TestOverviewIgnoresNonGatewayInstances(t *testing.T) {
	newMux := func(fake *fakeOverviewProm) *http.ServeMux {
		srv := httptest.NewServer(fake)
		t.Cleanup(srv.Close)
		return overviewMux(t, srv.URL, func(context.Context) ([]InstanceRecord, error) {
			return []InstanceRecord{
				overviewInstance("gw-a", instanceRoleGateway, overviewTestNow.Add(-10*time.Second)),
			}, nil
		})
	}

	t.Run("go_* 不得发现副本", func(t *testing.T) {
		fake := newFakeOverviewProm()
		fake.setInstances(perfquery.NameGatewayByInstanceGoroutines, "worker-1", "node-exporter:9100")
		fake.setInstances(perfquery.NameGatewayByInstanceHeapMB, "worker-1", "node-exporter:9100")
		fake.setInstances(perfquery.NameGatewayByInstanceInflight, "gw-a")

		resp := decodeOverview(t, getOverview(t, newMux(fake), "/api/performance/overview"))
		if len(resp.Gateways) != 1 || resp.Gateways[0].InstanceID != "gw-a" {
			t.Fatalf("gateways = %+v, want 只有 gw-a", resp.Gateways)
		}
	})

	t.Run("inflight 可发现无心跳副本", func(t *testing.T) {
		fake := newFakeOverviewProm()
		fake.setInstances(perfquery.NameGatewayByInstanceGoroutines, "worker-1", "node-exporter:9100")
		fake.setInstances(perfquery.NameGatewayByInstanceHeapMB, "worker-1", "node-exporter:9100")
		fake.setInstances(perfquery.NameGatewayByInstanceInflight, "gw-a", "gw-b")

		resp := decodeOverview(t, getOverview(t, newMux(fake), "/api/performance/overview"))
		if len(resp.Gateways) != 2 || resp.Gateways[0].InstanceID != "gw-a" || resp.Gateways[1].InstanceID != "gw-b" {
			t.Fatalf("gateways = %+v, want [gw-a gw-b]", resp.Gateways)
		}
		gwB := resp.Gateways[1]
		if gwB.HeartbeatFresh {
			t.Error("gw-b heartbeat_fresh = true, want false（没有心跳）")
		}
		if gwB.LastSeen != nil {
			t.Error("gw-b last_seen 应为 null")
		}
		if gwB.MetricsStatus != perfStatusUnknown {
			t.Errorf("gw-b metrics_status = %q, want unknown（只有 inflight，其余缺）", gwB.MetricsStatus)
		}
		if gwB.Metrics == nil || gwB.Metrics.Inflight == nil {
			t.Errorf("gw-b metrics = %+v, want inflight 有值", gwB.Metrics)
		}
	})
}

// TestOverviewSparklineTrimsOldestPoint 钉住裁剪方向：上游按 query_range 的
// 闭区间语义返回 points+1 个点时，只保留最新 points 个，丢的是最旧的点。
func TestOverviewSparklineTrimsOldestPoint(t *testing.T) {
	fake := newFakeOverviewProm()
	fake.points = 25
	srv := httptest.NewServer(fake)
	defer srv.Close()

	mux := overviewMux(t, srv.URL, func(context.Context) ([]InstanceRecord, error) {
		return []InstanceRecord{}, nil
	})

	resp := decodeOverview(t, getOverview(t, mux, "/api/performance/overview"))
	rps := vitalByKey(t, resp, perfquery.NameGatewayRPS)
	if len(rps.Series) != resp.Windows.Points {
		t.Fatalf("series 点数 = %d, want %d", len(rps.Series), resp.Windows.Points)
	}
	wantFirst := resp.Windows.End.Unix() - int64(resp.Windows.Points-1)*resp.Windows.StepSeconds
	if got := int64(rps.Series[0][0]); got != wantFirst {
		t.Errorf("首点时间 = %d, want %d（应丢掉最旧的一点）", got, wantFirst)
	}
	if got := int64(rps.Series[len(rps.Series)-1][0]); got != resp.Windows.End.Unix() {
		t.Errorf("末点时间 = %d, want %d", got, resp.Windows.End.Unix())
	}
}

// TestOverviewPromURLGateSuppressesQueries 钉住 available 的两个条件：
// PromURL 为空时即使注入了可用的 client，也按「未接入」处理且不发请求。
func TestOverviewPromURLGateSuppressesQueries(t *testing.T) {
	fake := newFakeOverviewProm()
	srv := httptest.NewServer(fake)
	defer srv.Close()

	handlers := NewPerformanceHandlers(PerformanceHandlerConfig{
		Logger:   testLogger(),
		Authz:    rbac.NewAuthzMiddleware(),
		Now:      func() time.Time { return overviewTestNow },
		Getenv:   func(string) string { return "" },
		Hostname: func() (string, error) { return "test-host", nil },
		Prom:     perfquery.New(perfquery.Config{BaseURL: srv.URL}),
		PromURL:  "",
		Instances: func(context.Context) ([]InstanceRecord, error) {
			return []InstanceRecord{}, nil
		},
	})
	mux := http.NewServeMux()
	handlers.Register(mux)

	resp := decodeOverview(t, getOverview(t, mux, "/api/performance/overview"))
	if len(resp.Sources) == 0 || resp.Sources[0].Status != perfSourceUnavailable {
		t.Fatalf("sources = %+v, want prometheus=unavailable", resp.Sources)
	}
	if fake.count() != 0 {
		t.Fatalf("PromURL 为空仍发出了 %d 个请求", fake.count())
	}
	for _, vital := range resp.Vitals {
		if vital.Value != nil {
			t.Errorf("%s: value = %v, want nil", vital.Key, *vital.Value)
		}
	}
}
