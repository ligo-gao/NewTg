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
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"newtg/gateway/internal/configcenter"
	"newtg/gateway/internal/perfsettings"
	"newtg/gateway/internal/rbac"
	"newtg/gateway/internal/settings"
)

// perfConfigTestNow 固定时钟：09:00 UTC 让「下次运行 12:00」与各种默认值
// 都能逐字断言。
var perfConfigTestNow = time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)

func perfConfigTestDefaults() perfsettings.Defaults {
	// 时区用生产默认值 Asia/Shanghai：schedule 用例要连带钉住 RFC3339 的
	// 偏移量（+08:00），只用 UTC 会掩盖「服务端按部署时区计算」这条契约。
	return perfsettings.Defaults{
		Enabled:     true,
		Times:       "00:00,12:00",
		Timezone:    "Asia/Shanghai",
		Runs:        3,
		Concurrency: 2,
	}
}

// newPerfConfigMux 装配端点。env 为 nil 表示所有环境变量为空。
func newPerfConfigMux(t *testing.T, reader *perfsettings.Reader,
	settingGet func(key string) ([]byte, bool), env map[string]string) *http.ServeMux {
	t.Helper()
	return newPerfConfigMuxWithLogger(t, testLogger(), reader, settingGet, env)
}

// newPerfConfigMuxWithLogger 同 newPerfConfigMux，但允许注入可捕获告警的
// logger（脏值/null/越界都必须留下 Warn）。
func newPerfConfigMuxWithLogger(t *testing.T, logger *slog.Logger, reader *perfsettings.Reader,
	settingGet func(key string) ([]byte, bool), env map[string]string) *http.ServeMux {
	t.Helper()
	return newPerfConfigMuxWithConfig(t, PerformanceHandlerConfig{
		Logger:     logger,
		Authz:      rbac.NewAuthzMiddleware(),
		Now:        func() time.Time { return perfConfigTestNow },
		Getenv:     func(key string) string { return env[key] },
		Hostname:   func() (string, error) { return "test-host", nil },
		Schedule:   reader,
		SettingGet: settingGet,
	})
}

// newPerfConfigMuxWithEnv 注入一份静态环境快照（Env 接缝）。
func newPerfConfigMuxWithEnv(t *testing.T, snapshot *PerfEnvSnapshot) *http.ServeMux {
	t.Helper()
	return newPerfConfigMuxWithConfig(t, PerformanceHandlerConfig{
		Logger:   testLogger(),
		Authz:    rbac.NewAuthzMiddleware(),
		Now:      func() time.Time { return perfConfigTestNow },
		Getenv:   func(string) string { return "" },
		Hostname: func() (string, error) { return "test-host", nil },
		Env:      snapshot,
	})
}

func newPerfConfigMuxWithConfig(t *testing.T, cfg PerformanceHandlerConfig) *http.ServeMux {
	t.Helper()
	handlers := NewPerformanceHandlers(cfg)
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

// QueryContext 只服务 configcenter.Load 的那条 SELECT：query 不含
// system_settings 时返回错误，避免将来 Load 改了查询、测试却仍然「通过」。
func (c *perfConfigCenterConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	if !strings.Contains(query, "system_settings") {
		return nil, fmt.Errorf("perfConfigCenterConn: unexpected query %q", query)
	}
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

// perfConfigWarnLog 是可捕获的 slog.Handler：脏值告警必须能在测试里被数出来，
// 而不是只写进 io.Discard。
type perfConfigWarnLog struct {
	mu      sync.Mutex
	records []slog.Record
}

func (c *perfConfigWarnLog) Enabled(context.Context, slog.Level) bool { return true }

func (c *perfConfigWarnLog) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, r.Clone())
	return nil
}

func (c *perfConfigWarnLog) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *perfConfigWarnLog) WithGroup(string) slog.Handler      { return c }

func (c *perfConfigWarnLog) warnings() []slog.Record {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.records)
}

// perfConfigWarnsForKey 取出去重后属于某个设置键的告警。
func perfConfigWarnsForKey(records []slog.Record, key string) []slog.Record {
	var out []slog.Record
	for _, record := range records {
		matched := false
		record.Attrs(func(attr slog.Attr) bool {
			if attr.Key == "key" && attr.Value.String() == key {
				matched = true
				return false
			}
			return true
		})
		if matched {
			out = append(out, record)
		}
	}
	return out
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
	body, resp := getPerfConfig(t, mux)

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
	if schedule.Timezone != "Asia/Shanghai" {
		t.Errorf("schedule.timezone = %q, want Asia/Shanghai", schedule.Timezone)
	}
	wantNext := reader.NextRun(perfConfigTestNow)
	if schedule.NextRunAt == nil || !schedule.NextRunAt.Equal(wantNext) {
		t.Errorf("schedule.next_run_at = %v, want %v (Reader.NextRun)", schedule.NextRunAt, wantNext)
	}
	// 不只比瞬间：wire 文本必须带部署时区的偏移量，客户端直接展示该字符串。
	wantWire := `"next_run_at":"` + wantNext.Format(time.RFC3339) + `"`
	if !strings.Contains(body, wantWire) {
		t.Errorf("body 缺少 %s（next_run_at 未按部署时区序列化）:\n%s", wantWire, body)
	}
	if !strings.Contains(body, "+08:00") {
		t.Errorf("body 不含 +08:00，next_run_at 可能被序列化成 UTC:\n%s", body)
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

// TestConfigIncludesSettingsGroups 钉住 A 类分组：稳定 id/tab/link、14 个键
// 的完整清单与顺序、单位词，以及值来自设置中心或注册表默认、脏值/null
// 回落默认但仍如实标注。
//
// 期望表全部硬编码（不引用生产常量 perfConfigGroupSpecs）：测试要能发现
// 生产清单被误改，而不是跟随它一起变。
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

	type wantGroup struct {
		id, tab, link string
		items         []perfConfigSettingSpec
	}
	wantGroups := []wantGroup{
		{
			id: "channel_health", tab: "policy", link: "/admin/settings?tab=policy",
			items: []perfConfigSettingSpec{
				{Key: "rl.health.test_interval_minutes", Unit: "minutes"},
				{Key: "rl.health.test_concurrency", Unit: "count"},
				{Key: "rl.health.response_time_threshold_seconds", Unit: "seconds"},
				{Key: "rl.health.auto_disable_enabled", Unit: "onoff"},
				{Key: "rl.health.auto_enable_enabled", Unit: "onoff"},
			},
		},
		{
			id: "rate_limit", tab: "security", link: "/admin/settings?tab=security",
			items: []perfConfigSettingSpec{
				{Key: "rl.user_global_rpm", Unit: "rpm"},
				{Key: "rl.token_rpm", Unit: "rpm"},
				{Key: "rl.user_model_rpm", Unit: "rpm"},
			},
		},
		{
			// 设置页没有 audit/redemption/task/mj/log 的入口：只给值，不给 tab/link。
			id: "retention", tab: "", link: "",
			items: []perfConfigSettingSpec{
				{Key: "audit.retention_months", Unit: "months"},
				{Key: "redemption.retention_days", Unit: "days"},
				{Key: "task.retention_terminal_days", Unit: "days"},
				{Key: "mj.retention_terminal_days", Unit: "days"},
				{Key: "log.partition_lookahead_months", Unit: "months"},
				{Key: "log.partition_drop_enabled", Unit: "onoff"},
			},
		},
	}

	if len(resp.SettingsGroups) != len(wantGroups) {
		t.Fatalf("settings_groups = %d, want %d", len(resp.SettingsGroups), len(wantGroups))
	}
	for i, want := range wantGroups {
		got := resp.SettingsGroups[i]
		if got.ID != want.id || got.Tab != want.tab || got.Link != want.link {
			t.Errorf("group[%d] = id=%q tab=%q link=%q, want id=%q tab=%q link=%q",
				i, got.ID, got.Tab, got.Link, want.id, want.tab, want.link)
		}
		if len(got.Items) != len(want.items) {
			t.Fatalf("group %q has %d items, want %d", want.id, len(got.Items), len(want.items))
		}
		for j, wantItem := range want.items {
			item := got.Items[j]
			if item.Key != wantItem.Key || item.Unit != wantItem.Unit {
				t.Errorf("group %q item[%d] = %q/%q, want %q/%q",
					want.id, j, item.Key, item.Unit, wantItem.Key, wantItem.Unit)
			}
			if item.Source != perfConfigSourceSettings && item.Source != perfConfigSourceDefault {
				t.Errorf("item %q source = %q, want settings|default", item.Key, item.Source)
			}
			if item.Type == "" {
				t.Errorf("item %q type is empty", item.Key)
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

// TestConfigOutOfRangeSettingFallsBackAndWarns 钉住「页面立论 = 看到什么就是
// 跑什么」：消费方对越界值会 clamp/回落，所以端点不能把它标成 settings。
func TestConfigOutOfRangeSettingFallsBackAndWarns(t *testing.T) {
	values := map[string]string{
		"rl.health.test_interval_minutes": "5000", // MaxInt 1440
		"audit.retention_months":          "-1",   // MinInt 0
	}
	logs := &perfConfigWarnLog{}
	settingGet := func(key string) ([]byte, bool) {
		raw, ok := values[key]
		return []byte(raw), ok
	}
	mux := newPerfConfigMuxWithLogger(t, slog.New(logs),
		perfsettings.New(nil, perfConfigTestDefaults(), testLogger()), settingGet, nil)
	_, resp := getPerfConfig(t, mux)

	interval := perfConfigItem(t, resp.SettingsGroups, "rl.health.test_interval_minutes")
	if interval.Value != float64(10) || interval.Source != perfConfigSourceDefault {
		t.Errorf("test_interval_minutes(5000) = %v/%s, want registry default 10/default",
			interval.Value, interval.Source)
	}
	audit := perfConfigItem(t, resp.SettingsGroups, "audit.retention_months")
	if audit.Value != float64(12) || audit.Source != perfConfigSourceDefault {
		t.Errorf("retention_months(-1) = %v/%s, want registry default 12/default",
			audit.Value, audit.Source)
	}

	for _, key := range []string{"rl.health.test_interval_minutes", "audit.retention_months"} {
		records := perfConfigWarnsForKey(logs.warnings(), key)
		if len(records) != 1 {
			t.Fatalf("warn for %q = %d, want exactly 1", key, len(records))
		}
		if !strings.Contains(records[0].Message, "out of range") {
			t.Errorf("warn for %q = %q, want an out-of-range message", key, records[0].Message)
		}
	}
}

// TestConfigNullSettingWarns 钉住 null 也告警：落库的 null 不是「显式关闭」
// 而是脏行，静默回落会让页面与设置页对不上。
func TestConfigNullSettingWarns(t *testing.T) {
	logs := &perfConfigWarnLog{}
	settingGet := func(key string) ([]byte, bool) {
		if key == "log.partition_drop_enabled" {
			return []byte("null"), true
		}
		return nil, false
	}
	mux := newPerfConfigMuxWithLogger(t, slog.New(logs),
		perfsettings.New(nil, perfConfigTestDefaults(), testLogger()), settingGet, nil)
	_, resp := getPerfConfig(t, mux)

	dropEnabled := perfConfigItem(t, resp.SettingsGroups, "log.partition_drop_enabled")
	if dropEnabled.Value != false || dropEnabled.Source != perfConfigSourceDefault {
		t.Errorf("partition_drop_enabled(null) = %v/%s, want false/default",
			dropEnabled.Value, dropEnabled.Source)
	}

	records := perfConfigWarnsForKey(logs.warnings(), "log.partition_drop_enabled")
	if len(records) != 1 {
		t.Fatalf("warn for null value = %d, want exactly 1 (all warns: %v)",
			len(records), perfConfigWarnMessages(logs.warnings()))
	}
	if !strings.Contains(records[0].Message, "is null") {
		t.Errorf("warn message = %q, want the distinct null wording", records[0].Message)
	}
}

func perfConfigWarnMessages(records []slog.Record) []string {
	messages := make([]string, 0, len(records))
	for _, record := range records {
		messages = append(messages, record.Message)
	}
	return messages
}

// TestConfigWarnMasksSecretSettingRaw 钉住日志侧脱敏：将来把密钥类键加进
// A 类分组表时，脏值不能把明文写进日志。
func TestConfigWarnMasksSecretSettingRaw(t *testing.T) {
	logs := &perfConfigWarnLog{}
	handlers := NewPerformanceHandlers(PerformanceHandlerConfig{Logger: slog.New(logs)})

	secret := settings.Setting{Key: "demo.secret", Secret: true}
	handlers.warnPerfConfigSetting(secret, []byte("s3cr3t-token"), "declined", nil)
	plain := settings.Setting{Key: "demo.plain"}
	handlers.warnPerfConfigSetting(plain, []byte("12"), "declined", nil)

	records := logs.warnings()
	if len(records) != 2 {
		t.Fatalf("warns = %d, want 2", len(records))
	}
	rawAttr := func(record slog.Record) string {
		var raw string
		record.Attrs(func(attr slog.Attr) bool {
			if attr.Key == "raw" {
				raw = attr.Value.String()
				return false
			}
			return true
		})
		return raw
	}
	if got := rawAttr(records[0]); got != "<redacted>" {
		t.Errorf("secret raw attr = %q, want %q", got, "<redacted>")
	}
	if got := rawAttr(records[1]); got != "12" {
		t.Errorf("plain raw attr = %q, want %q", got, "12")
	}
}

// TestConfigEnvRowsCoverSnapshot 钉住 env[] 的行集与形态：顺序固定、恰好
// 18 行（PerfMetricsDSNSet 不占行）、METRICS_TOKEN 只有 configured/set、
// 没有 value 键，set 用来区分「按默认值跑」与「部署者显式配置」。
func TestConfigEnvRowsCoverSnapshot(t *testing.T) {
	env := map[string]string{
		"METRICS_TOKEN":   "tok",
		"DISK_CACHE_PATH": "/var/cache/tg",
	}
	mux := newPerfConfigMux(t, perfsettings.New(nil, perfConfigTestDefaults(), testLogger()), nil, env)
	body, resp := getPerfConfig(t, mux)

	wantKeys := []string{
		"DB_MAX_OPEN_CONNS",
		"REDIS_POOL_SIZE",
		"SHUTDOWN_GRACE_SECONDS",
		"METRICS_ADDR",
		"METRICS_TOKEN",
		"PERF_METRICS_ENABLED",
		"PERF_METRICS_FLUSH_INTERVAL",
		"PERF_METRICS_RETENTION_DAYS",
		"PROMETHEUS_URL",
		"PROMETHEUS_TIMEOUT",
		"PROMETHEUS_CACHE_TTL",
		"PERF_SCHEDULE_TIMEZONE",
		"DISK_CACHE_ENABLED",
		"DISK_CACHE_PATH",
		"LOG_DIR",
		"LOG_FILE_PATTERN",
		"DISK_CACHE_MAX_MB",
		"DISK_CACHE_THRESHOLD_MB",
	}
	gotKeys := perfConfigEnvKeys(resp.Env)
	if !slices.Equal(gotKeys, wantKeys) {
		t.Fatalf("env keys = %v, want exactly %v", gotKeys, wantKeys)
	}
	// 行集已逐项固定；这条显式声明避免以后误把 DSN 加回来（值里可能带口令）。
	if slices.Contains(gotKeys, "PERF_METRICS_DSN") {
		t.Error("PERF_METRICS_DSN must never be rendered (it can embed a password)")
	}

	// wire 层断言：METRICS_TOKEN 行没有 value 键（不是 "value":""、也不是 null）。
	var wire struct {
		Env []map[string]json.RawMessage `json:"env"`
	}
	if err := json.Unmarshal([]byte(body), &wire); err != nil {
		t.Fatalf("decode raw env rows: %v", err)
	}
	if len(wire.Env) != len(wantKeys) {
		t.Fatalf("raw env rows = %d, want %d", len(wire.Env), len(wantKeys))
	}
	tokenIndex := slices.Index(wantKeys, "METRICS_TOKEN")
	if _, ok := wire.Env[tokenIndex]["value"]; ok {
		t.Errorf("METRICS_TOKEN row has a value key: %s", body)
	}
	if _, ok := wire.Env[0]["value"]; !ok {
		t.Errorf("DB_MAX_OPEN_CONNS row must always render value: %s", body)
	}

	defaults := map[string]string{
		"DB_MAX_OPEN_CONNS":           "25",
		"REDIS_POOL_SIZE":             "10",
		"SHUTDOWN_GRACE_SECONDS":      "30",
		"METRICS_ADDR":                ":8006",
		"PERF_METRICS_ENABLED":        "true",
		"PERF_METRICS_FLUSH_INTERVAL": "5m0s",
		"PERF_METRICS_RETENTION_DAYS": "400",
		"PROMETHEUS_TIMEOUT":          "5s",
		"PROMETHEUS_CACHE_TTL":        "15s",
		"PERF_SCHEDULE_TIMEZONE":      "Asia/Shanghai",
		"DISK_CACHE_ENABLED":          "false",
		"LOG_FILE_PATTERN":            "newtg-*.log",
		"DISK_CACHE_MAX_MB":           "0",
		"DISK_CACHE_THRESHOLD_MB":     "0",
	}
	for key, want := range defaults {
		row := findPerfConfigEnvRow(t, resp.Env, key)
		if row.Value == nil || *row.Value != want {
			t.Errorf("%s value = %v, want %q (applied default)", key, row.Value, want)
		}
		if !row.Configured {
			t.Errorf("%s configured = false, want true for a non-empty rendered default", key)
		}
		if row.Set {
			t.Errorf("%s set = true, want false (deployer did not set it)", key)
		}
	}

	tokenRow := findPerfConfigEnvRow(t, resp.Env, "METRICS_TOKEN")
	if !tokenRow.Configured || !tokenRow.Set || tokenRow.Value != nil {
		t.Errorf("METRICS_TOKEN row = %+v, want configured+set with no value", tokenRow)
	}
	pathRow := findPerfConfigEnvRow(t, resp.Env, "DISK_CACHE_PATH")
	if !pathRow.Configured || !pathRow.Set || pathRow.Value == nil || *pathRow.Value != "/var/cache/tg" {
		t.Errorf("DISK_CACHE_PATH row = %+v, want configured+set with the env value", pathRow)
	}
}

// TestConfigEnvRowsPrometheusCacheTTLAndLogFilePattern：两个新增行的口径与
// DB_MAX_OPEN_CONNS 一致——显式设置时展示部署值且 set=true；未设置时展示
// 内置默认、set=false、configured=true（「实际生效值」而不是「是否设置」）。
func TestConfigEnvRowsPrometheusCacheTTLAndLogFilePattern(t *testing.T) {
	t.Run("显式设置", func(t *testing.T) {
		mux := newPerfConfigMux(t, perfsettings.New(nil, perfConfigTestDefaults(), testLogger()), nil, map[string]string{
			"PROMETHEUS_CACHE_TTL": "30s",
			"LOG_FILE_PATTERN":     "app-*.log",
		})
		_, resp := getPerfConfig(t, mux)

		for key, want := range map[string]string{
			"PROMETHEUS_CACHE_TTL": "30s",
			"LOG_FILE_PATTERN":     "app-*.log",
		} {
			row := findPerfConfigEnvRow(t, resp.Env, key)
			if row.Value == nil || *row.Value != want {
				t.Errorf("%s value = %v, want %q（部署值）", key, row.Value, want)
			}
			if !row.Set || !row.Configured {
				t.Errorf("%s = %+v, want set=true configured=true", key, row)
			}
		}
	})

	t.Run("未设置走默认", func(t *testing.T) {
		mux := newPerfConfigMux(t, perfsettings.New(nil, perfConfigTestDefaults(), testLogger()), nil, nil)
		_, resp := getPerfConfig(t, mux)

		for key, want := range map[string]string{
			"PROMETHEUS_CACHE_TTL": "15s",
			"LOG_FILE_PATTERN":     "newtg-*.log",
		} {
			row := findPerfConfigEnvRow(t, resp.Env, key)
			if row.Value == nil || *row.Value != want {
				t.Errorf("%s value = %v, want %q（内置默认）", key, row.Value, want)
			}
			if row.Set {
				t.Errorf("%s set = true, want false（部署者未设置）", key)
			}
			if !row.Configured {
				t.Errorf("%s configured = false, want true（默认值非空）", key)
			}
		}
	})
}

// TestConfigFixedRowsAreFrozen 钉住 C 类四条：key/value/unit/effect 全表
// 逐字固定（期望值硬编码，防止生产常量漂移悄悄改掉页面文案）。
func TestConfigFixedRowsAreFrozen(t *testing.T) {
	mux := newPerfConfigMux(t, perfsettings.New(nil, perfConfigTestDefaults(), testLogger()), nil, nil)
	_, resp := getPerfConfig(t, mux)

	want := []perfConfigFixedRow{
		{Key: "probe_timeout", Value: "30s", Unit: "duration", Effect: perfConfigEffectCode},
		{Key: "write_rate_limit", Value: "10/min", Unit: "rate", Effect: perfConfigEffectCode},
		{Key: "disk_cache_idle", Value: "10m", Unit: "duration", Effect: perfConfigEffectCode},
		{Key: "usage_log_retention", Value: "90d", Unit: "duration", Effect: perfConfigEffectCode},
	}
	if len(resp.Fixed) != len(want) {
		t.Fatalf("fixed rows = %d, want %d", len(resp.Fixed), len(want))
	}
	for i := range want {
		if resp.Fixed[i] != want[i] {
			t.Errorf("fixed[%d] = %+v, want %+v", i, resp.Fixed[i], want[i])
		}
	}
}

// TestConfigFixedProbeTimeoutUsesInjectedSnapshot 钉住 fixed[].probe_timeout
// 取注入快照字段而不是包级常量：否则 Env 接缝对它不生效，将来把测速上限
// 做成可配置时页面会继续显示旧值。
func TestConfigFixedProbeTimeoutUsesInjectedSnapshot(t *testing.T) {
	snapshot := PerfEnvSnapshot{ProbeTimeout: 45 * time.Second}
	mux := newPerfConfigMuxWithEnv(t, &snapshot)
	_, resp := getPerfConfig(t, mux)

	row := findPerfConfigFixedRow(t, resp.Fixed, "probe_timeout")
	if row.Value != "45s" {
		t.Errorf("probe_timeout = %q, want %q from the injected snapshot", row.Value, "45s")
	}
}

func findPerfConfigFixedRow(t *testing.T, rows []perfConfigFixedRow, key string) perfConfigFixedRow {
	t.Helper()
	for _, row := range rows {
		if row.Key == key {
			return row
		}
	}
	t.Fatalf("fixed row %q not found", key)
	return perfConfigFixedRow{}
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
