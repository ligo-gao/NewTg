package httpapi

// 本文件验收 GET /api/performance/config（spec §7.6 / 本计划 Task 10）：
// 生效节奏（含服务端计算的 next_run_at）、部署环境参数（秘钥不回显）、
// 代码内置参数与 A 类分组当前值。
//
// 不依赖外部服务：环境变量走 Getenv 接缝；A 类值走 SettingGet 闭包；
// 「已落库的 perf.schedule.*」用 sql.OpenDB + 一个只服务 Load 的
// connector 灌进 configcenter 内存快照（Center 没有导出的种值入口，
// 不为一条测试新开生产 API，见进度记录 Task 7 的裁定）。

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"newtg/gateway/internal/configcenter"
	"newtg/gateway/internal/perfsettings"
	"newtg/gateway/internal/rbac"
)

// perfConfigTestNow 固定时钟：09:00 UTC 让「下次运行 12:00」与各种默认值
// 都能逐字断言。
var perfConfigTestNow = time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)

func perfConfigTestDefaults() perfsettings.Defaults {
	return perfsettings.Defaults{
		Enabled:     true,
		Times:       "00:00,12:00",
		Timezone:    "UTC",
		Runs:        3,
		Concurrency: 2,
	}
}

// newPerfConfigMux 装配端点。env 为 nil 表示所有环境变量为空。
func newPerfConfigMux(t *testing.T, reader *perfsettings.Reader,
	settingGet func(key string) ([]byte, bool), env map[string]string) *http.ServeMux {
	t.Helper()
	handlers := NewPerformanceHandlers(PerformanceHandlerConfig{
		Logger:     testLogger(),
		Authz:      rbac.NewAuthzMiddleware(),
		Now:        func() time.Time { return perfConfigTestNow },
		Getenv:     func(key string) string { return env[key] },
		Hostname:   func() (string, error) { return "test-host", nil },
		Schedule:   reader,
		SettingGet: settingGet,
	})
	mux := http.NewServeMux()
	handlers.Register(mux)
	return mux
}

func getPerfConfig(t *testing.T, mux *http.ServeMux) (string, perfConfigResponse) {
	t.Helper()
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, perfRequest(http.MethodGet, "/api/performance/config", "", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	var resp perfConfigResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode config response: %v (body=%s)", err, rr.Body.String())
	}
	return rr.Body.String(), resp
}

// ---------------------------------------------------------------------------
// 已落库配置的读入接缝（不连 PG）
// ---------------------------------------------------------------------------

// perfConfigCenterConnector 把静态行集装成 *sql.DB。configcenter.Center 只能
// 通过 New(db, rdb) + Load 获得内存快照，这里只实现 Load 用到的
// QueryContext，其余方法返回明确错误（误用即可见）。
type perfConfigCenterConnector struct {
	rows [][3]driver.Value // key, value, updated_at
}

func (c perfConfigCenterConnector) Connect(context.Context) (driver.Conn, error) {
	return &perfConfigCenterConn{rows: c.rows}, nil
}

func (c perfConfigCenterConnector) Driver() driver.Driver { return perfConfigCenterDriver{} }

type perfConfigCenterDriver struct{}

func (perfConfigCenterDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("perfConfigCenterDriver: use sql.OpenDB with the connector")
}

type perfConfigCenterConn struct {
	rows [][3]driver.Value
}

func (c *perfConfigCenterConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("perfConfigCenterConn: Prepare is not supported")
}

func (c *perfConfigCenterConn) Close() error { return nil }

func (c *perfConfigCenterConn) Begin() (driver.Tx, error) {
	return nil, errors.New("perfConfigCenterConn: Begin is not supported")
}

func (c *perfConfigCenterConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	return &perfConfigCenterRows{rows: c.rows}, nil
}

type perfConfigCenterRows struct {
	rows  [][3]driver.Value
	index int
}

func (r *perfConfigCenterRows) Columns() []string { return []string{"key", "value", "updated_at"} }

func (r *perfConfigCenterRows) Close() error { return nil }

func (r *perfConfigCenterRows) Next(dest []driver.Value) error {
	if r.index >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.index][:])
	r.index++
	return nil
}

// newSeededConfigCenter 返回一个内存快照里已有 values 的配置中心。
// value 传原始 JSON 文本（"5"、`"abc"`、null 都能表达）。
func newSeededConfigCenter(t *testing.T, values map[string]string) *configcenter.Center {
	t.Helper()
	rows := make([][3]driver.Value, 0, len(values))
	for key, raw := range values {
		rows = append(rows, [3]driver.Value{key, []byte(raw), perfConfigTestNow})
	}
	db := sql.OpenDB(perfConfigCenterConnector{rows: rows})
	t.Cleanup(func() { _ = db.Close() })
	center := configcenter.New(db, nil)
	if err := center.Load(context.Background()); err != nil {
		t.Fatalf("load seeded config center: %v", err)
	}
	return center
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func findPerfConfigEnvRow(t *testing.T, rows []perfConfigEnvRow, key string) perfConfigEnvRow {
	t.Helper()
	for _, row := range rows {
		if row.Key == key {
			return row
		}
	}
	t.Fatalf("env row %q not found in %v", key, perfConfigEnvKeys(rows))
	return perfConfigEnvRow{}
}

func perfConfigEnvKeys(rows []perfConfigEnvRow) []string {
	keys := make([]string, 0, len(rows))
	for _, row := range rows {
		keys = append(keys, row.Key)
	}
	return keys
}

func perfConfigItem(t *testing.T, groups []perfConfigSettingsGroup, key string) perfConfigSettingItem {
	t.Helper()
	for _, group := range groups {
		for _, item := range group.Items {
			if item.Key == key {
				return item
			}
		}
	}
	t.Fatalf("settings item %q not found", key)
	return perfConfigSettingItem{}
}

// ---------------------------------------------------------------------------
// 用例
// ---------------------------------------------------------------------------

// TestConfigScheduleFromEnvSource 钉住「四键都未落库」的升级安全属性：
// 页面必须如实说 source=env、legacy_times 回显 PERF_SCHEDULE_TIMES，且
// next_run_at 与 worker 用的 Reader.NextRun 完全一致（服务端计算）。
func TestConfigScheduleFromEnvSource(t *testing.T) {
	reader := perfsettings.New(nil, perfConfigTestDefaults(), testLogger())
	mux := newPerfConfigMux(t, reader, nil, nil)
	_, resp := getPerfConfig(t, mux)

	if resp.Schedule == nil {
		t.Fatal("schedule = null, want an object (reader is wired)")
	}
	schedule := *resp.Schedule
	if schedule.Source != perfsettings.SourceEnv {
		t.Errorf("schedule.source = %q, want %q", schedule.Source, perfsettings.SourceEnv)
	}
	if schedule.LegacyTimes != "00:00,12:00" {
		t.Errorf("schedule.legacy_times = %q, want %q", schedule.LegacyTimes, "00:00,12:00")
	}
	if !schedule.Enabled || schedule.IntervalHours != 0 || schedule.Runs != 3 || schedule.Concurrency != 2 {
		t.Errorf("schedule = %+v, want enabled/0h/3/2 from env defaults", schedule)
	}
	if schedule.Timezone != "UTC" {
		t.Errorf("schedule.timezone = %q, want UTC", schedule.Timezone)
	}
	wantNext := reader.NextRun(perfConfigTestNow)
	if schedule.NextRunAt == nil || !schedule.NextRunAt.Equal(wantNext) {
		t.Errorf("schedule.next_run_at = %v, want %v (Reader.NextRun)", schedule.NextRunAt, wantNext)
	}
}

// TestConfigScheduleAfterSave 钉住「页面保存过 interval_hours」后的形状：
// source 切换为 settings，legacy_times 消失（间隔模式不再使用旧时刻表），
// next_run_at 仍由同一个 Reader 计算。
func TestConfigScheduleAfterSave(t *testing.T) {
	center := newSeededConfigCenter(t, map[string]string{
		"perf.schedule.enabled":        "true",
		"perf.schedule.interval_hours": "5",
		"perf.schedule.runs":           "4",
		"perf.schedule.concurrency":    "1",
	})
	reader := perfsettings.New(center, perfConfigTestDefaults(), testLogger())
	mux := newPerfConfigMux(t, reader, nil, nil)
	_, resp := getPerfConfig(t, mux)

	if resp.Schedule == nil {
		t.Fatal("schedule = null, want an object after settings were saved")
	}
	schedule := *resp.Schedule
	if schedule.Source != perfsettings.SourceSettings {
		t.Errorf("schedule.source = %q, want %q", schedule.Source, perfsettings.SourceSettings)
	}
	if schedule.LegacyTimes != "" {
		t.Errorf("schedule.legacy_times = %q, want empty in interval mode", schedule.LegacyTimes)
	}
	if schedule.IntervalHours != 5 || schedule.Runs != 4 || schedule.Concurrency != 1 {
		t.Errorf("schedule = %+v, want interval 5 / runs 4 / concurrency 1", schedule)
	}
	wantNext := reader.NextRun(perfConfigTestNow)
	if schedule.NextRunAt == nil || !schedule.NextRunAt.Equal(wantNext) {
		t.Errorf("schedule.next_run_at = %v, want %v", schedule.NextRunAt, wantNext)
	}
}

// TestConfigHidesSecrets 钉住白名单口径：口令类只给「已设置」布尔，
// 原值绝不进响应；DSN 行整行不出现。
func TestConfigHidesSecrets(t *testing.T) {
	const token = "s3cr3t-metrics-token-value"
	const dsn = "postgres://perf:pa55w0rd@db:5432/perfmetrics"
	mux := newPerfConfigMux(t, perfsettings.New(nil, perfConfigTestDefaults(), testLogger()), nil,
		map[string]string{
			"METRICS_TOKEN":        token,
			"PERF_METRICS_DSN":     dsn,
			"PERF_METRICS_ENABLED": "true",
		})
	body, resp := getPerfConfig(t, mux)

	if strings.Contains(body, token) {
		t.Errorf("response body leaks METRICS_TOKEN value: %s", body)
	}
	if strings.Contains(body, "pa55w0rd") || strings.Contains(body, "PERF_METRICS_DSN") {
		t.Errorf("response body leaks PERF_METRICS_DSN: %s", body)
	}

	tokenRow := findPerfConfigEnvRow(t, resp.Env, "METRICS_TOKEN")
	if !tokenRow.Configured {
		t.Error("METRICS_TOKEN configured = false, want true when the variable is set")
	}
	if tokenRow.Value != nil {
		t.Errorf("METRICS_TOKEN value = %q, want no value rendered", *tokenRow.Value)
	}
}

// TestConfigIncludesSettingsGroups 钉住 A 类分组：稳定 id/tab/link、组内
// 3–6 项、值来自设置中心或注册表默认，脏值/null 回落默认但仍如实标注。
func TestConfigIncludesSettingsGroups(t *testing.T) {
	values := map[string]string{
		"rl.health.test_interval_minutes": "12",
		"rl.health.test_concurrency":      "8",
		"rl.health.auto_disable_enabled":  "true",
		"rl.user_global_rpm":              "120",
		// 脏值与 JSON null 都必须回落默认，而不是让端点 500。
		"rl.health.response_time_threshold_seconds": `"not-a-number"`,
		"log.partition_drop_enabled":                "null",
	}
	settingGet := func(key string) ([]byte, bool) {
		raw, ok := values[key]
		return []byte(raw), ok
	}
	mux := newPerfConfigMux(t, perfsettings.New(nil, perfConfigTestDefaults(), testLogger()), settingGet, nil)
	_, resp := getPerfConfig(t, mux)

	if len(resp.SettingsGroups) != len(perfConfigGroupSpecs) {
		t.Fatalf("settings_groups = %d, want %d", len(resp.SettingsGroups), len(perfConfigGroupSpecs))
	}
	wantGroups := map[string]struct{ tab, link string }{
		"channel_health": {tab: "policy", link: "/admin/settings?tab=policy"},
		"rate_limit":     {tab: "security", link: "/admin/settings?tab=security"},
		"retention":      {tab: "", link: ""},
	}
	for _, group := range resp.SettingsGroups {
		want, ok := wantGroups[group.ID]
		if !ok {
			t.Fatalf("unexpected group id %q", group.ID)
		}
		if group.Tab != want.tab || group.Link != want.link {
			t.Errorf("group %q tab/link = %q/%q, want %q/%q",
				group.ID, group.Tab, group.Link, want.tab, want.link)
		}
		if len(group.Items) < 3 || len(group.Items) > 6 {
			t.Errorf("group %q has %d items, want 3..6", group.ID, len(group.Items))
		}
		for _, item := range group.Items {
			if item.Source != perfConfigSourceSettings && item.Source != perfConfigSourceDefault {
				t.Errorf("item %q source = %q, want settings|default", item.Key, item.Source)
			}
			if item.Type == "" || item.Unit == "" {
				t.Errorf("item %q type/unit = %q/%q, want both non-empty", item.Key, item.Type, item.Unit)
			}
		}
	}

	interval := perfConfigItem(t, resp.SettingsGroups, "rl.health.test_interval_minutes")
	if interval.Value != float64(12) || interval.Source != perfConfigSourceSettings {
		t.Errorf("test_interval_minutes = %v/%s, want 12/settings", interval.Value, interval.Source)
	}
	if interval.Type != "int" || interval.Unit != "minutes" {
		t.Errorf("test_interval_minutes type/unit = %q/%q, want int/minutes", interval.Type, interval.Unit)
	}

	autoDisable := perfConfigItem(t, resp.SettingsGroups, "rl.health.auto_disable_enabled")
	if autoDisable.Value != true || autoDisable.Source != perfConfigSourceSettings || autoDisable.Type != "bool" {
		t.Errorf("auto_disable_enabled = %v/%s/%s, want true/settings/bool",
			autoDisable.Value, autoDisable.Source, autoDisable.Type)
	}

	// 未落库的键：值取注册表默认（rl.user_model_rpm = 60）。
	modelRPM := perfConfigItem(t, resp.SettingsGroups, "rl.user_model_rpm")
	if modelRPM.Value != float64(60) || modelRPM.Source != perfConfigSourceDefault {
		t.Errorf("user_model_rpm = %v/%s, want 60/default", modelRPM.Value, modelRPM.Source)
	}

	// 脏值：回落注册表默认 5（number 类型），来源如实报 default。
	threshold := perfConfigItem(t, resp.SettingsGroups, "rl.health.response_time_threshold_seconds")
	if threshold.Value != float64(5) || threshold.Source != perfConfigSourceDefault {
		t.Errorf("response_time_threshold_seconds = %v/%s, want 5/default",
			threshold.Value, threshold.Source)
	}

	// JSON null：同样回落默认（false），不把 null 读成显式关闭。
	dropEnabled := perfConfigItem(t, resp.SettingsGroups, "log.partition_drop_enabled")
	if dropEnabled.Value != false || dropEnabled.Source != perfConfigSourceDefault {
		t.Errorf("partition_drop_enabled = %v/%s, want false/default",
			dropEnabled.Value, dropEnabled.Source)
	}
}

// TestConfigEmptyPrometheusURL 钉住降级线索：未配置的 URL 也要出现在
// 清单里，configured=false，前端据此显示「未配置」而不是空表格。
func TestConfigEmptyPrometheusURL(t *testing.T) {
	mux := newPerfConfigMux(t, perfsettings.New(nil, perfConfigTestDefaults(), testLogger()), nil, nil)
	_, resp := getPerfConfig(t, mux)

	row := findPerfConfigEnvRow(t, resp.Env, "PROMETHEUS_URL")
	if row.Configured {
		t.Error("PROMETHEUS_URL configured = true, want false when unset")
	}
	if row.Value == nil || *row.Value != "" {
		t.Errorf("PROMETHEUS_URL value = %v, want empty string", row.Value)
	}
}

// TestConfigWithoutScheduleIsNull 钉住未装配的降级：没有 perfsettings.Reader
// 时 schedule 整段为 null，其余三段照常输出，端点仍 200。
func TestConfigWithoutScheduleIsNull(t *testing.T) {
	mux := newPerfConfigMux(t, nil, nil, nil)
	_, resp := getPerfConfig(t, mux)

	if resp.Schedule != nil {
		t.Errorf("schedule = %+v, want null when no reader is wired", *resp.Schedule)
	}
	if len(resp.Env) == 0 || len(resp.Fixed) == 0 || len(resp.SettingsGroups) == 0 {
		t.Errorf("env/fixed/settings_groups must still be rendered (got %d/%d/%d)",
			len(resp.Env), len(resp.Fixed), len(resp.SettingsGroups))
	}
}

// TestConfigScheduleDisabledHasNoNextRun 钉住停用语义：没有「下次运行」，
// 页面显示「已停用」而不是一个过期的时刻。
func TestConfigScheduleDisabledHasNoNextRun(t *testing.T) {
	defaults := perfConfigTestDefaults()
	defaults.Enabled = false
	reader := perfsettings.New(nil, defaults, testLogger())
	mux := newPerfConfigMux(t, reader, nil, nil)
	_, resp := getPerfConfig(t, mux)

	if resp.Schedule == nil {
		t.Fatal("schedule = null, want an object")
	}
	if resp.Schedule.Enabled {
		t.Error("schedule.enabled = true, want false")
	}
	if resp.Schedule.NextRunAt != nil {
		t.Errorf("schedule.next_run_at = %v, want null when disabled", resp.Schedule.NextRunAt)
	}
}
