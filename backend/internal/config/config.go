// Package config 解析平台级环境变量（platform-03 §3.1 的 26 项应用侧变量）。
//
// 口径冻结（task-3 brief）：
//   - DATABASE_TLS / REDIS_TLS 是 compose 插值变量，不参与 Load；REDIS_TLS 仅由
//     RedisURL() 在调用时读取以决定是否 rediss://。
//   - MainDSN 以 DATABASE_URL 为唯一来源，DSN 已含 sslmode 时原样透传。
package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// DemoEnabledKey 是演示模式设置键（settings 注册表同名）。
const DemoEnabledKey = "site.demo.enabled"

// Config 覆盖 platform-03 §3.1 全部 26 项应用侧环境变量。
type Config struct {
	// 必填（#1-#4、#22）。DB_LOGS_DSN 在平台设计表 #22 标「✅ 必填」；
	// 计划 GC-7 曾按 4 项枚举，M0 收尾按设计表改为第 5 项（控制器裁决）：
	// 日志库迁移不可被静默跳过，否则 usage_logs / audit_logs 永远不会建出来。
	DatabaseURL             string // DATABASE_URL
	SessionSecret           string // SESSION_SECRET
	CredentialEncryptionKey string // CREDENTIAL_ENCRYPTION_KEY
	DBLogsDSN               string // DB_LOGS_DSN
	// DBRetentionDSN 是保留/维护主体（newtg_retention）的日志库 DSN（W10-N10/N11）。
	//
	// **可选**：为空表示部署未提供独立维护凭据，分区删除与审计保留一律
	// 不执行（fail-closed）。绝不回退到 DB_LOGS_DSN——那会用应用身份执行
	// DROP/DELETE，既违反 platform-01 §12.6 的最小权限，也会把「没配置」
	// 静默变成「用错身份删数据」。主库不需要该凭据（保留任务只碰日志库）。
	DBRetentionDSN string // DB_RETENTION_DSN

	// 可选（#5-#21、#23-#26）；REDIS_URL 因与同名的 RedisURL() 方法冲突，以非导出字段存储。
	redisURL                 string        // REDIS_URL
	RunMode                  string        // RUN_MODE
	HTTPAddr                 string        // HTTP_ADDR
	AppEnv                   string        // APP_ENV
	LogLevel                 string        // LOG_LEVEL
	LogFormat                string        // LOG_FORMAT
	NodeName                 string        // NODE_NAME（空 → hostname）
	TZ                       string        // TZ
	DBMigrateOnStart         bool          // DB_MIGRATE_ON_START
	DBMaxOpenConns           int           // DB_MAX_OPEN_CONNS
	DBMaxIdleConns           int           // DB_MAX_IDLE_CONNS
	RedisPoolSize            int           // REDIS_POOL_SIZE
	TrustedProxies           string        // TRUSTED_PROXIES
	ShutdownGraceSeconds     int           // SHUTDOWN_GRACE_SECONDS
	MetricsAddr              string        // METRICS_ADDR
	MetricsToken             string        // METRICS_TOKEN
	PublicBaseURL            string        // PUBLIC_BASE_URL
	OpenAPIValidate          string        // OPENAPI_VALIDATE
	PerfMetricsEnabled       bool          // PERF_METRICS_ENABLED
	PerfMetricsFlushInterval time.Duration // PERF_METRICS_FLUSH_INTERVAL
	PerfMetricsRetentionDays int           // PERF_METRICS_RETENTION_DAYS
	// PerfMetricsDSN 是**独立指标库**的连接串（PERF_METRICS_DSN）。
	//
	// 与 DATABASE_URL / DB_LOGS_DSN 严格分离：性能指标不落主库、也不落日志库
	// （platform-05 §3.5.3 的「不落业务库」）。该变量为可选：未设置时
	// PERF_METRICS_ENABLED=true 仍可按 fail-closed 方式启动，但
	// /api/model-perf 会返回 pipeline.enabled=false + 不可用原因，绝不回退到
	// 日志库或主库。
	PerfMetricsDSN string // PERF_METRICS_DSN
	TLSDMinVersion string // TLS_MIN_VERSION（仅 edge 消费）

	// DemoEnabled 是演示模式的**部署侧覆盖**（NEWTG_DEMO_ENABLED）。
	//
	// nil 表示未设置/非法值：演示模式以 site.demo.enabled 设置为准（保持
	// 既有单一真源）；非 nil 时强制覆盖设置值。它解决的是设计 F-08-46 的
	// 单向门：只读守卫同时拒绝了 PUT /api/settings，演示站开启后没有任何
	// 受控出口，只能由部署侧显式关闭（控制端裁决 I2）。
	DemoEnabled *bool // NEWTG_DEMO_ENABLED
}

// defaults 包级默认值表，与 platform-03 §3.1 的「默认」列一一对应。
var defaults = map[string]string{
	"RUN_MODE":                    "gateway",
	"HTTP_ADDR":                   ":3000",
	"APP_ENV":                     "production",
	"LOG_LEVEL":                   "info",
	"LOG_FORMAT":                  "json",
	"NODE_NAME":                   "",
	"TZ":                          "UTC",
	"DB_MIGRATE_ON_START":         "true",
	"DB_MAX_OPEN_CONNS":           "25",
	"DB_MAX_IDLE_CONNS":           "25",
	"REDIS_POOL_SIZE":             "10",
	"TRUSTED_PROXIES":             "",
	"SHUTDOWN_GRACE_SECONDS":      "30",
	"METRICS_ADDR":                ":8006",
	"METRICS_TOKEN":               "",
	"PUBLIC_BASE_URL":             "",
	"OPENAPI_VALIDATE":            "warn",
	"PERF_METRICS_ENABLED":        "true",
	"PERF_METRICS_FLUSH_INTERVAL": "5m",
	"PERF_METRICS_RETENTION_DAYS": "400",
	"TLS_MIN_VERSION":             "1.2",
}

// lookup 集中读取环境变量：非空值优先，否则回落 defaults[key]。
func lookup(key string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaults[key]
}

// required 五项必填环境变量（platform-03 §3.1 #1-#4、#22）。
// DB_LOGS_DSN 按设计表 #22 的「必填 ✅」执行（M0 控制器裁决，覆盖计划 GC-7 的 4 项枚举）。
var required = []string{
	"DATABASE_URL",
	"REDIS_URL",
	"SESSION_SECRET",
	"CREDENTIAL_ENCRYPTION_KEY",
	"DB_LOGS_DSN",
}

// Load 解析 26 项环境变量并校验；必填缺失或非法值返回 error（信息含变量名）。
func Load() (*Config, error) {
	for _, key := range required {
		if os.Getenv(key) == "" {
			return nil, fmt.Errorf("config: required environment variable %s is not set", key)
		}
	}

	c := &Config{
		DatabaseURL:             os.Getenv("DATABASE_URL"),
		redisURL:                os.Getenv("REDIS_URL"),
		SessionSecret:           os.Getenv("SESSION_SECRET"),
		CredentialEncryptionKey: os.Getenv("CREDENTIAL_ENCRYPTION_KEY"),
		DBLogsDSN:               os.Getenv("DB_LOGS_DSN"),
		// 只消费已拼好的 DSN；缺失时保留空串，由 worker 装配层按 fail-closed
		// 处理（W10-N10/N11 不静默复用应用 DSN）。
		DBRetentionDSN:  os.Getenv("DB_RETENTION_DSN"),
		RunMode:         lookup("RUN_MODE"),
		HTTPAddr:        lookup("HTTP_ADDR"),
		AppEnv:          lookup("APP_ENV"),
		LogLevel:        lookup("LOG_LEVEL"),
		LogFormat:       lookup("LOG_FORMAT"),
		TZ:              lookup("TZ"),
		TrustedProxies:  lookup("TRUSTED_PROXIES"),
		MetricsAddr:     lookup("METRICS_ADDR"),
		MetricsToken:    lookup("METRICS_TOKEN"),
		PublicBaseURL:   lookup("PUBLIC_BASE_URL"),
		OpenAPIValidate: lookup("OPENAPI_VALIDATE"),
		TLSDMinVersion:  lookup("TLS_MIN_VERSION"),
		// 指标库 DSN 是可选部署配置；缺失时由 platform 以 fail-closed 方式
		// 装配（读面闸门），不在 config.Load 阶段失败——否则一个可选监控
		// 能力的缺失会让整个网关无法启动。
		PerfMetricsDSN: os.Getenv("PERF_METRICS_DSN"),
		// 非法值按「未设置」处理：部署侧拼写错误只告警，不得让整站无法启动
		// （控制端裁决 I2；fail-closed 在这里反而会把演示站永久锁死）。
		DemoEnabled: parseDemoEnabled(lookup("NEWTG_DEMO_ENABLED")),
	}

	// NODE_NAME 空 → hostname。
	c.NodeName = lookup("NODE_NAME")
	if c.NodeName == "" {
		hostname, err := os.Hostname()
		if err != nil {
			return nil, fmt.Errorf("config: NODE_NAME empty and os.Hostname() failed: %w", err)
		}
		c.NodeName = hostname
	}

	var err error
	if c.DBMigrateOnStart, err = parseBool("DB_MIGRATE_ON_START", lookup("DB_MIGRATE_ON_START")); err != nil {
		return nil, err
	}
	if c.PerfMetricsEnabled, err = parseBool("PERF_METRICS_ENABLED", lookup("PERF_METRICS_ENABLED")); err != nil {
		return nil, err
	}
	if c.DBMaxOpenConns, err = parseInt("DB_MAX_OPEN_CONNS", lookup("DB_MAX_OPEN_CONNS")); err != nil {
		return nil, err
	}
	if c.DBMaxIdleConns, err = parseInt("DB_MAX_IDLE_CONNS", lookup("DB_MAX_IDLE_CONNS")); err != nil {
		return nil, err
	}
	if c.RedisPoolSize, err = parseInt("REDIS_POOL_SIZE", lookup("REDIS_POOL_SIZE")); err != nil {
		return nil, err
	}
	if c.ShutdownGraceSeconds, err = parseInt("SHUTDOWN_GRACE_SECONDS", lookup("SHUTDOWN_GRACE_SECONDS")); err != nil {
		return nil, err
	}
	if c.PerfMetricsRetentionDays, err = parseInt("PERF_METRICS_RETENTION_DAYS", lookup("PERF_METRICS_RETENTION_DAYS")); err != nil {
		return nil, err
	}
	flush := lookup("PERF_METRICS_FLUSH_INTERVAL")
	c.PerfMetricsFlushInterval, err = time.ParseDuration(flush)
	if err != nil {
		return nil, fmt.Errorf("config: invalid PERF_METRICS_FLUSH_INTERVAL %q: %w", flush, err)
	}

	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

func parseBool(key, raw string) (bool, error) {
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("config: invalid %s %q: %w", key, raw, err)
	}
	return v, nil
}

// parseDemoEnabled 解析 NEWTG_DEMO_ENABLED 的冻结取值：
//
//	"true"/"1"  → 强制开启
//	"false"/"0" → 强制关闭
//	空/非法    → nil（跟随 site.demo.enabled），非法值额外告警
//
// 为什么不用 strconv.ParseBool：它会接受 "t"/"T"/"TRUE" 等拼写，扩大了
// 部署面可表达的集合；这里只认 frozen 四值，其余一律按未设置处理。
// 为什么不 fail 启动：一个拼错的覆盖变量不应该让整站下线（I2 裁决）。
func parseDemoEnabled(raw string) *bool {
	switch strings.TrimSpace(raw) {
	case "":
		return nil
	case "true", "1":
		enabled := true
		return &enabled
	case "false", "0":
		enabled := false
		return &enabled
	default:
		slog.Warn("config: invalid NEWTG_DEMO_ENABLED; ignoring override",
			"value", raw, "want", "true|false|1|0")
		return nil
	}
}

// ConfigGetter 是 configcenter.Center.Get 的最小读取面。
//
// httpapi / billing 各自声明了形状相同的窄接口；这里的类型只用于让
// WithDemoOverride 同时适配两者（接口的方法集相同即可赋值）。
type ConfigGetter interface {
	Get(key string) (json.RawMessage, bool)
}

// DemoEnabled 返回**有效**演示模式：读取（可能已被 WithDemoOverride 包装的）
// 配置快照。
//
// 这是守卫、中继额度、计费豁免三处唯一的有效值函数（控制端裁决 I2）：
// 三处都调用它，避免「守卫按设置关闭、额度按 env 仍在扣」的分裂。
// 缺键/脏值视为关闭，与 settings 注册表默认 false 一致。
func DemoEnabled(getter ConfigGetter) bool {
	if getter == nil {
		return false
	}
	raw, ok := getter.Get(DemoEnabledKey)
	if !ok {
		return false
	}
	var enabled bool
	if err := json.Unmarshal(raw, &enabled); err != nil {
		return false
	}
	return enabled
}

// WithDemoOverride 返回配置读取面包装：NEWTG_DEMO_ENABLED 非 nil 时
// 覆盖 site.demo.enabled 的读取值。
//
// 只覆盖这一个键，其余键原样透传——调用方可以把这个包装同时交给管理面
// 守卫、中继 handler 与计费引擎，而不改变其它设置语义。override 为 nil
// 时原样返回 inner（未设置 env 的部署与既有行为完全一致）。
func WithDemoOverride(inner ConfigGetter, override *bool) ConfigGetter {
	if override == nil {
		return inner
	}
	return demoOverrideReader{inner: inner, enabled: *override}
}

type demoOverrideReader struct {
	inner   ConfigGetter
	enabled bool
}

func (r demoOverrideReader) Get(key string) (json.RawMessage, bool) {
	if key == DemoEnabledKey {
		// 返回合法 JSON：下游（如 settings 校验器）仍按 bool 反序列化。
		if r.enabled {
			return json.RawMessage("true"), true
		}
		return json.RawMessage("false"), true
	}
	if r.inner == nil {
		return nil, false
	}
	return r.inner.Get(key)
}

func parseInt(key, raw string) (int, error) {
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("config: invalid %s %q: %w", key, raw, err)
	}
	return v, nil
}

// RedisURL 返回 Redis DSN；调用时读取 REDIS_TLS（off|on），on 时 redis:// → rediss://。
// REDIS_TLS 是 compose 插值变量，不参与 Load。
func (c *Config) RedisURL() string {
	if os.Getenv("REDIS_TLS") == "on" {
		return strings.Replace(c.redisURL, "redis://", "rediss://", 1)
	}
	return c.redisURL
}

// MainDSN 返回主库 DSN。DATABASE_TLS 是 compose 插值变量，应用只消费拼好的
// DATABASE_URL——已含 sslmode 时原样透传。
func (c *Config) MainDSN() string {
	return c.DatabaseURL
}

// Validate 区间与枚举校验（Load 亦调用）。
func (c *Config) Validate() error {
	switch c.RunMode {
	case "gateway", "worker", "migrate":
	default:
		return fmt.Errorf("config: invalid RUN_MODE %q (want gateway|worker|migrate)", c.RunMode)
	}
	switch c.AppEnv {
	case "production", "development":
	default:
		return fmt.Errorf("config: invalid APP_ENV %q (want production|development)", c.AppEnv)
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("config: invalid LOG_LEVEL %q (want debug|info|warn|error)", c.LogLevel)
	}
	switch c.OpenAPIValidate {
	case "strict", "warn", "off":
	default:
		return fmt.Errorf("config: invalid OPENAPI_VALIDATE %q (want strict|warn|off)", c.OpenAPIValidate)
	}
	switch c.TLSDMinVersion {
	case "1.2", "1.3":
	default:
		return fmt.Errorf("config: invalid TLS_MIN_VERSION %q (want 1.2|1.3)", c.TLSDMinVersion)
	}
	if c.PerfMetricsFlushInterval < time.Minute || c.PerfMetricsFlushInterval > 60*time.Minute {
		return fmt.Errorf("config: PERF_METRICS_FLUSH_INTERVAL %v out of range [1m, 60m]", c.PerfMetricsFlushInterval)
	}
	if c.PerfMetricsRetentionDays < 30 || c.PerfMetricsRetentionDays > 400 {
		return fmt.Errorf("config: PERF_METRICS_RETENTION_DAYS %d out of range [30, 400]", c.PerfMetricsRetentionDays)
	}
	return nil
}
