// Package config 的测试：全部用 t.Setenv 注入环境，不读写真实环境。
package config

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// setRequired 设置 5 个必填项为合法值，供默认值/非法值用例使用。
// DB_LOGS_DSN 是 platform-03 §3.1 #22 的必填项（M0 控制器裁决）。
func setRequired(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://app:secret@db:5432/newtg?sslmode=disable")
	t.Setenv("REDIS_URL", "redis://:secret@redis:6379/0")
	t.Setenv("SESSION_SECRET", "test-session-secret")
	t.Setenv("CREDENTIAL_ENCRYPTION_KEY", "test-encryption-key")
	t.Setenv("DB_LOGS_DSN", "postgres://app:secret@db:5432/newtg_logs?sslmode=disable")
	// 可选指标库 DSN：默认用例显式清空，避免宿主环境变量影响断言。
	t.Setenv("PERF_METRICS_DSN", "")
}

func TestLoadDefaults(t *testing.T) {
	setRequired(t)

	c, err := Load()
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}

	if c.DatabaseURL != "postgres://app:secret@db:5432/newtg?sslmode=disable" {
		t.Errorf("DatabaseURL = %q", c.DatabaseURL)
	}
	if c.RedisURL() != "redis://:secret@redis:6379/0" {
		t.Errorf("RedisURL() = %q", c.RedisURL())
	}
	if c.SessionSecret != "test-session-secret" {
		t.Errorf("SessionSecret = %q", c.SessionSecret)
	}
	if c.CredentialEncryptionKey != "test-encryption-key" {
		t.Errorf("CredentialEncryptionKey = %q", c.CredentialEncryptionKey)
	}
	if c.DBLogsDSN != "postgres://app:secret@db:5432/newtg_logs?sslmode=disable" {
		t.Errorf("DBLogsDSN = %q", c.DBLogsDSN)
	}
	if c.RunMode != "gateway" {
		t.Errorf("RunMode = %q, want gateway", c.RunMode)
	}
	if c.HTTPAddr != ":3000" {
		t.Errorf("HTTPAddr = %q, want :3000", c.HTTPAddr)
	}
	if c.AppEnv != "production" {
		t.Errorf("AppEnv = %q, want production", c.AppEnv)
	}
	if c.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want info", c.LogLevel)
	}
	if c.LogFormat != "json" {
		t.Errorf("LogFormat = %q, want json", c.LogFormat)
	}
	// NODE_NAME 空 → hostname。
	hostname, err := os.Hostname()
	if err != nil {
		t.Fatalf("os.Hostname() error: %v", err)
	}
	if c.NodeName != hostname {
		t.Errorf("NodeName = %q, want hostname %q", c.NodeName, hostname)
	}
	if c.TZ != "UTC" {
		t.Errorf("TZ = %q, want UTC", c.TZ)
	}
	if c.DBMigrateOnStart != true {
		t.Errorf("DBMigrateOnStart = %v, want true", c.DBMigrateOnStart)
	}
	if c.DBMaxOpenConns != 25 {
		t.Errorf("DBMaxOpenConns = %d, want 25", c.DBMaxOpenConns)
	}
	if c.DBMaxIdleConns != 25 {
		t.Errorf("DBMaxIdleConns = %d, want 25", c.DBMaxIdleConns)
	}
	if c.RedisPoolSize != 10 {
		t.Errorf("RedisPoolSize = %d, want 10", c.RedisPoolSize)
	}
	if c.TrustedProxies != "" {
		t.Errorf("TrustedProxies = %q, want empty", c.TrustedProxies)
	}
	if c.ShutdownGraceSeconds != 30 {
		t.Errorf("ShutdownGraceSeconds = %d, want 30", c.ShutdownGraceSeconds)
	}
	if c.MetricsAddr != ":8006" {
		t.Errorf("MetricsAddr = %q, want :8006", c.MetricsAddr)
	}
	if c.MetricsToken != "" {
		t.Errorf("MetricsToken = %q, want empty", c.MetricsToken)
	}
	if c.PublicBaseURL != "" {
		t.Errorf("PublicBaseURL = %q, want empty", c.PublicBaseURL)
	}
	if c.OpenAPIValidate != "warn" {
		t.Errorf("OpenAPIValidate = %q, want warn", c.OpenAPIValidate)
	}
	if c.PerfMetricsEnabled != true {
		t.Errorf("PerfMetricsEnabled = %v, want true", c.PerfMetricsEnabled)
	}
	if c.PerfMetricsFlushInterval != 5*time.Minute {
		t.Errorf("PerfMetricsFlushInterval = %v, want 5m", c.PerfMetricsFlushInterval)
	}
	if c.PerfMetricsRetentionDays != 400 {
		t.Errorf("PerfMetricsRetentionDays = %d, want 400", c.PerfMetricsRetentionDays)
	}
	if c.PerfMetricsDSN != "" {
		t.Errorf("PerfMetricsDSN = %q, want empty (optional)", c.PerfMetricsDSN)
	}
	if c.TLSDMinVersion != "1.2" {
		t.Errorf("TLSDMinVersion = %q, want 1.2", c.TLSDMinVersion)
	}
}

func TestLoadPerfMetricsDSN(t *testing.T) {
	setRequired(t)
	t.Setenv("PERF_METRICS_DSN", "postgres://metrics:secret@metrics:5432/newtg_metrics?sslmode=disable")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if c.PerfMetricsDSN != "postgres://metrics:secret@metrics:5432/newtg_metrics?sslmode=disable" {
		t.Errorf("PerfMetricsDSN = %q", c.PerfMetricsDSN)
	}
}

func TestLoadRequiredMissing(t *testing.T) {
	cases := []struct {
		name string
		env  string
	}{
		{"DATABASE_URL", "DATABASE_URL"},
		{"REDIS_URL", "REDIS_URL"},
		{"SESSION_SECRET", "SESSION_SECRET"},
		{"CREDENTIAL_ENCRYPTION_KEY", "CREDENTIAL_ENCRYPTION_KEY"},
		{"DB_LOGS_DSN", "DB_LOGS_DSN"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setRequired(t)
			t.Setenv(tc.env, "") // 空即缺失

			_, err := Load()
			if err == nil {
				t.Fatalf("Load() with empty %s: expected error, got nil", tc.env)
			}
			if !strings.Contains(err.Error(), tc.env) {
				t.Errorf("Load() error = %q, want it to contain %q", err.Error(), tc.env)
			}
		})
	}
}

func TestRedisURLTLSScheme(t *testing.T) {
	setRequired(t)
	t.Setenv("REDIS_URL", "redis://:secret@redis:6379/0")

	t.Run("off 保持 redis", func(t *testing.T) {
		t.Setenv("REDIS_TLS", "off")
		c, err := Load()
		if err != nil {
			t.Fatalf("Load() error: %v", err)
		}
		if got := c.RedisURL(); got != "redis://:secret@redis:6379/0" {
			t.Errorf("RedisURL() = %q, want redis:// scheme", got)
		}
	})

	t.Run("on 变 rediss", func(t *testing.T) {
		t.Setenv("REDIS_TLS", "on")
		c, err := Load()
		if err != nil {
			t.Fatalf("Load() error: %v", err)
		}
		if got := c.RedisURL(); got != "rediss://:secret@redis:6379/0" {
			t.Errorf("RedisURL() = %q, want rediss:// scheme", got)
		}
	})
}

func TestLoadInvalidValues(t *testing.T) {
	cases := []struct {
		name string
		env  string
		val  string
	}{
		{"APP_ENV 非法", "APP_ENV", "staging"},
		{"LOG_LEVEL 非法", "LOG_LEVEL", "trace"},
		{"OPENAPI_VALIDATE 非法", "OPENAPI_VALIDATE", "banana"},
		{"PERF_METRICS_RETENTION_DAYS 低于下限", "PERF_METRICS_RETENTION_DAYS", "10"},
		{"PERF_METRICS_RETENTION_DAYS 高于上限", "PERF_METRICS_RETENTION_DAYS", "401"},
		{"PERF_METRICS_FLUSH_INTERVAL 低于下限", "PERF_METRICS_FLUSH_INTERVAL", "30s"},
		{"PERF_METRICS_FLUSH_INTERVAL 高于上限", "PERF_METRICS_FLUSH_INTERVAL", "61m"},
		{"RUN_MODE 非法", "RUN_MODE", "banana"},
		{"DB_MAX_OPEN_CONNS 非整数", "DB_MAX_OPEN_CONNS", "lots"},
		{"TLS_MIN_VERSION 非法", "TLS_MIN_VERSION", "1.1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setRequired(t)
			t.Setenv(tc.env, tc.val)

			_, err := Load()
			if err == nil {
				t.Fatalf("Load() with %s=%q: expected error, got nil", tc.env, tc.val)
			}
			if !strings.Contains(err.Error(), tc.env) {
				t.Errorf("Load() error = %q, want it to contain %q", err.Error(), tc.env)
			}
		})
	}
}

func TestMainDSNPassthrough(t *testing.T) {
	setRequired(t)
	t.Setenv("DATABASE_URL", "postgres://app:secret@db:5432/newtg?sslmode=verify-full")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if got := c.MainDSN(); got != "postgres://app:secret@db:5432/newtg?sslmode=verify-full" {
		t.Errorf("MainDSN() = %q, want passthrough of DATABASE_URL", got)
	}
}

func TestValidateStandalone(t *testing.T) {
	valid := &Config{
		RunMode:                  "gateway",
		AppEnv:                   "production",
		LogLevel:                 "info",
		OpenAPIValidate:          "warn",
		PerfMetricsFlushInterval: 5 * time.Minute,
		PerfMetricsRetentionDays: 400,
		TLSDMinVersion:           "1.2",
	}
	if err := valid.Validate(); err != nil {
		t.Errorf("Validate() on valid config: unexpected error: %v", err)
	}

	bad := *valid
	bad.PerfMetricsRetentionDays = 10
	if err := bad.Validate(); err == nil {
		t.Errorf("Validate() with RetentionDays=10: expected error, got nil")
	}

	bad = *valid
	bad.PerfMetricsFlushInterval = 61 * time.Minute
	if err := bad.Validate(); err == nil {
		t.Errorf("Validate() with FlushInterval=61m: expected error, got nil")
	}

	bad = *valid
	bad.LogLevel = "trace"
	if err := bad.Validate(); err == nil {
		t.Errorf("Validate() with LogLevel=trace: expected error, got nil")
	}

	bad = *valid
	bad.OpenAPIValidate = "banana"
	if err := bad.Validate(); err == nil {
		t.Errorf("Validate() with OpenAPIValidate=banana: expected error, got nil")
	}
}

// ---------- NEWTG_DEMO_ENABLED（I2 部署侧覆盖）----------

type demoTestConfig map[string]json.RawMessage

func (m demoTestConfig) Get(key string) (json.RawMessage, bool) {
	v, ok := m[key]
	return v, ok
}

func TestParseDemoEnabledFrozenValues(t *testing.T) {
	cases := []struct {
		raw  string
		want *bool
	}{
		{"", nil},
		{"true", boolPtr(true)},
		{"1", boolPtr(true)},
		{"false", boolPtr(false)},
		{"0", boolPtr(false)},
		// 非法值按未设置处理；parseBool 会接受的 t/T/TRUE 也不在冻结集合内。
		{"yes", nil},
		{"t", nil},
		{" FALSE ", nil},
	}
	for _, tc := range cases {
		got := parseDemoEnabled(tc.raw)
		if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
			t.Errorf("parseDemoEnabled(%q) = %v，期望 %v", tc.raw, fmtPointer(got), fmtPointer(tc.want))
		}
	}
}

func boolPtr(v bool) *bool { return &v }

func fmtPointer(p *bool) string {
	if p == nil {
		return "<nil>"
	}
	return strconv.FormatBool(*p)
}

func TestLoadDemoEnabledOverride(t *testing.T) {
	setRequired(t)
	t.Setenv("NEWTG_DEMO_ENABLED", "false")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.DemoEnabled == nil || *c.DemoEnabled {
		t.Fatalf("DemoEnabled = %v，期望 ptr(false)", c.DemoEnabled)
	}

	// 未设置 → nil（跟随 site.demo.enabled）。
	t.Setenv("NEWTG_DEMO_ENABLED", "")
	c, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.DemoEnabled != nil {
		t.Fatalf("未设置时 DemoEnabled = %v，期望 nil", *c.DemoEnabled)
	}
}

func TestLoadInvalidDemoEnabledWarnsAndFallsBack(t *testing.T) {
	setRequired(t)
	t.Setenv("NEWTG_DEMO_ENABLED", "maybe")

	old := slog.Default()
	defer slog.SetDefault(old)
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))

	c, err := Load()
	if err != nil {
		t.Fatalf("非法覆盖值不得让启动失败: %v", err)
	}
	if c.DemoEnabled != nil {
		t.Fatalf("非法值应按未设置处理，got %v", *c.DemoEnabled)
	}
	if !strings.Contains(buf.String(), "NEWTG_DEMO_ENABLED") {
		t.Fatalf("期望 warn 日志包含变量名，got %q", buf.String())
	}
}

func TestDemoEnabledEffectiveValue(t *testing.T) {
	withSetting := func(v bool) demoTestConfig {
		return demoTestConfig{DemoEnabledKey: json.RawMessage(strconv.FormatBool(v))}
	}
	cases := []struct {
		name     string
		setting  bool
		override *bool
		want     bool
	}{
		{"未设置跟随开启", true, nil, true},
		{"未设置跟随关闭", false, nil, false},
		{"env=false 覆盖设置 true", true, boolPtr(false), false},
		{"env=true 覆盖设置 false", false, boolPtr(true), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DemoEnabled(WithDemoOverride(withSetting(tc.setting), tc.override))
			if got != tc.want {
				t.Fatalf("有效演示模式 = %v，期望 %v", got, tc.want)
			}
		})
	}
	// 包装只覆盖演示键，其余键原样透传。
	inner := demoTestConfig{"site.title": json.RawMessage(`"TG"`)}
	wrapped := WithDemoOverride(inner, boolPtr(true))
	if raw, ok := wrapped.Get("site.title"); !ok || string(raw) != `"TG"` {
		t.Fatalf("其它键被包装改动: %s ok=%v", raw, ok)
	}
	if DemoEnabled(nil) {
		t.Fatal("nil 读取面必须按关闭处理")
	}
}
