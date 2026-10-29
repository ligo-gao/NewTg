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
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"newtg/gateway/internal/modelperf"
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

	// Prometheus 查询（性能页 overview 的数据源，spec §7.1）。
	//
	// PrometheusURL 为空是**合法**的降级态：监控栈未接入时性能页显示
	// 「未接入」而不是让进程启动失败，也不会发出任何查询请求。
	// 该地址只来自部署配置，不得由请求参数拼接（overview 只接受命名查询）。
	PrometheusURL string // PROMETHEUS_URL
	// PrometheusTimeout 是单次查询超时。
	PrometheusTimeout time.Duration // PROMETHEUS_TIMEOUT
	// PrometheusCacheTTL 是查询结果缓存；同一 (name, window) 在该窗口内
	// 复用一次上游查询，避免多管理员刷新把 Prometheus 打满。
	PrometheusCacheTTL time.Duration // PROMETHEUS_CACHE_TTL

	// DemoEnabled 是演示模式的**部署侧覆盖**（NEWTG_DEMO_ENABLED）。
	//
	// nil 表示未设置/非法值：演示模式以 site.demo.enabled 设置为准（保持
	// 既有单一真源）；非 nil 时强制覆盖设置值。它解决的是设计 F-08-46 的
	// 单向门：只读守卫同时拒绝了 PUT /api/settings，演示站开启后没有任何
	// 受控出口，只能由部署侧显式关闭（控制端裁决 I2）。
	DemoEnabled *bool // NEWTG_DEMO_ENABLED

	// HolidaySyncEnabled 控制 holiday_sync worker 任务（spec §5.2.1）。
	//
	// 关闭后调度器跳过该任务、手动触发返回 409 holiday_sync_disabled；
	// 已同步的 holiday_calendar 行仍参与计价——关掉的是「拉取」，不是
	// 「使用」。离线部署可用它彻底禁掉出站请求。
	HolidaySyncEnabled bool // HOLIDAY_SYNC_ENABLED
	// HolidaySyncURLTemplate 是年度 JSON 的地址模板，%d 会被替换成年份。
	// 默认值是 jsDelivr 缓存的 NateScarlet/holiday-cn（MIT，见 THIRD_PARTY_NOTICES）。
	HolidaySyncURLTemplate string // HOLIDAY_SYNC_URL_TEMPLATE

	// 模型性能探测调度（spec §5.6）。五个变量在 gateway 与 worker 进程里都会
	// 解析并校验（配置错误必须在启动期暴露），但**只有 worker 角色会真正启动
	// 调度循环**，gateway 解析后不会发起任何探测。
	//
	// 注意：compose 只把变量注入 worker 容器，所以 `.env` 里的覆盖值不会到达
	// gateway——gateway 侧读到的始终是默认值。任何依赖 gateway 展示这些设置的
	// 功能都必须先解决这一点（改为双角色注入，或由 worker 持久化实际生效值）。
	//
	// PerfScheduleEnabled 是定时探测的**默认**开关：性能页保存过
	// perf.schedule.enabled 后以设置为准；调度循环总是启动，关闭由每个 tick
	// 判定，因此运行期改设置不需要重启。
	PerfScheduleEnabled bool // PERF_SCHEDULE_ENABLED
	// PerfScheduleTimes 是每日探测时刻表，逗号分隔的 HH:MM 列表（如 "00:00,12:00"）。
	PerfScheduleTimes string // PERF_SCHEDULE_TIMES
	// PerfScheduleTimezone 是解释时刻表所用的 IANA 时区名。
	PerfScheduleTimezone string // PERF_SCHEDULE_TIMEZONE
	// PerfScheduleRuns 是每个时刻的重复轮数，加载时钳制到 1..5。
	PerfScheduleRuns int // PERF_SCHEDULE_RUNS
	// PerfScheduleConcurrency 是单个时刻内的并发探测数，加载时钳制到 1..8。
	PerfScheduleConcurrency int // PERF_SCHEDULE_CONCURRENCY
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
	// Prometheus 查询（性能页 overview）。空 URL = 未接入监控（合法降级态）。
	"PROMETHEUS_URL":       "",
	"PROMETHEUS_TIMEOUT":   "5s",
	"PROMETHEUS_CACHE_TTL": "15s",
	// 中国法定节假日同步（holiday_sync）。默认开启、默认数据源对公网可达；
	// 离线部署把 HOLIDAY_SYNC_ENABLED 置 false 即可不发任何出站请求。
	"HOLIDAY_SYNC_ENABLED":      "true",
	"HOLIDAY_SYNC_URL_TEMPLATE": "https://cdn.jsdelivr.net/gh/NateScarlet/holiday-cn@master/%d.json",
	// 模型性能探测调度（spec §5.6）。默认每天午夜与正午各跑一次，
	// 时区取国内部署最常用的 Asia/Shanghai。
	"PERF_SCHEDULE_ENABLED":     "true",
	"PERF_SCHEDULE_TIMES":       "00:00,12:00",
	"PERF_SCHEDULE_TIMEZONE":    "Asia/Shanghai",
	"PERF_SCHEDULE_RUNS":        "3",
	"PERF_SCHEDULE_CONCURRENCY": "2",
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
		// 节假日同步只在 worker 角色实际执行；gateway 也解析同一份配置，
		// 保证管理面展示的开关与 worker 行为一致。
		HolidaySyncURLTemplate: lookup("HOLIDAY_SYNC_URL_TEMPLATE"),
	}

	var err error
	if c.HolidaySyncEnabled, err = parseBool("HOLIDAY_SYNC_ENABLED", lookup("HOLIDAY_SYNC_ENABLED")); err != nil {
		return nil, err
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

	// Prometheus 查询：URL 为空是未接入监控的合法状态，不做必填校验。
	c.PrometheusURL = strings.TrimSpace(lookup("PROMETHEUS_URL"))
	promTimeout := lookup("PROMETHEUS_TIMEOUT")
	c.PrometheusTimeout, err = time.ParseDuration(promTimeout)
	if err != nil {
		return nil, fmt.Errorf("config: invalid PROMETHEUS_TIMEOUT %q: %w", promTimeout, err)
	}
	promCacheTTL := lookup("PROMETHEUS_CACHE_TTL")
	c.PrometheusCacheTTL, err = time.ParseDuration(promCacheTTL)
	if err != nil {
		return nil, fmt.Errorf("config: invalid PROMETHEUS_CACHE_TTL %q: %w", promCacheTTL, err)
	}

	if c.PerfScheduleEnabled, err = parseBool("PERF_SCHEDULE_ENABLED", lookup("PERF_SCHEDULE_ENABLED")); err != nil {
		return nil, err
	}
	c.PerfScheduleTimes = lookup("PERF_SCHEDULE_TIMES")
	c.PerfScheduleTimezone = lookup("PERF_SCHEDULE_TIMEZONE")
	if c.PerfScheduleRuns, err = parseInt("PERF_SCHEDULE_RUNS", lookup("PERF_SCHEDULE_RUNS")); err != nil {
		return nil, err
	}
	if c.PerfScheduleConcurrency, err = parseInt("PERF_SCHEDULE_CONCURRENCY", lookup("PERF_SCHEDULE_CONCURRENCY")); err != nil {
		return nil, err
	}
	// 数值越界按 spec §5.6 钳制而不是报错：部署者写 99 轮/0 并发是想表达
	// “尽量多/不要并发”，收敛到区间比让整站启动失败更符合预期。非数字
	// 仍由 parseInt 报错——那是无法猜测的拼写错误。
	c.PerfScheduleRuns = min(max(c.PerfScheduleRuns, 1), 5)
	c.PerfScheduleConcurrency = min(max(c.PerfScheduleConcurrency, 1), 8)

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

// RedisURLCAPath 返回 Redis 私有 CA 的 PEM 路径；调用时读取 REDIS_TLS_CA，
// 与 RedisURL() 读 REDIS_TLS 同源（不参与 Load）。空值表示只用系统根证书。
func (c *Config) RedisURLCAPath() string { return os.Getenv("REDIS_TLS_CA") }

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
	// Prometheus 查询（性能页 overview）。空 URL 合法：监控未接入时页面整体
	// 降级，绝不能因为它让网关启动失败。非空时必须是 http/https 且带 host：
	// 该地址会被 Client 原样拼成请求 URL，其它 scheme（file://、gopher://）
	// 既没有意义，也会把部署配置错误的后果放大。
	//
	// 错误信息刻意不回显原值：URL 里可能嵌着 user:pass（反向代理的 basic
	// auth），启动失败日志与验收输出都不该把它带出来。
	if c.PrometheusURL != "" {
		parsed, err := url.Parse(c.PrometheusURL)
		if err != nil {
			return fmt.Errorf("config: invalid PROMETHEUS_URL: must be a valid absolute http(s) URL")
		}
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			return fmt.Errorf("config: invalid PROMETHEUS_URL: scheme must be http or https")
		}
		if parsed.Host == "" {
			return fmt.Errorf("config: invalid PROMETHEUS_URL: missing host")
		}
		// 客户端把该地址直接拼成 `baseURL + /api/v1/query_range?...`：原值若自带
		// `?query` 或 `#fragment`，查询串会把 API 路径吞进参数，整页指标永远
		// unknown。与其运行期静默错乱，不如启动期拒绝。错误信息仍不回显原值。
		if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
			return fmt.Errorf("config: invalid PROMETHEUS_URL: must not contain a query string or fragment")
		}
	}
	if c.PrometheusTimeout <= 0 || c.PrometheusTimeout > 60*time.Second {
		return fmt.Errorf("config: PROMETHEUS_TIMEOUT %v out of range (0s, 60s]", c.PrometheusTimeout)
	}
	// 0 不合法：perfquery.New 把非正 CacheTTL 当作「用默认 15s」，接受 0 会
	// 让「关掉缓存」变成「退回 15s」，与部署文档的语义不符。
	if c.PrometheusCacheTTL <= 0 || c.PrometheusCacheTTL > 5*time.Minute {
		return fmt.Errorf("config: PROMETHEUS_CACHE_TTL %v out of range (0s, 5m]", c.PrometheusCacheTTL)
	}
	// holiday_sync 的 URL 模板必须恰好有一个 %d：拼错的模板会让任务每天
	// 拉同一个年份或一个带 `%!d(MISSING)` 的地址。启动期失败比运行期
	// 静默拉错更早暴露。空串表示「未配置」（Load 总会填入默认模板；
	// 手工构造的 Config 不写该字段时不应被这条规则拦下）。
	if c.HolidaySyncURLTemplate != "" && strings.Count(c.HolidaySyncURLTemplate, "%d") != 1 {
		return fmt.Errorf("config: HOLIDAY_SYNC_URL_TEMPLATE %q must contain exactly one %%d", c.HolidaySyncURLTemplate)
	}
	// 性能探测调度：即使 PERF_SCHEDULE_ENABLED=false 也照常校验，这样把开关
	// 重新打开之前就能暴露写错的时区/时刻表，而不是等到 worker 静默不跑。
	// Load 总会填入默认值，因此空值只会出现在手工构造的 Config 上。
	loc, err := time.LoadLocation(c.PerfScheduleTimezone)
	if err != nil {
		return fmt.Errorf("config: invalid PERF_SCHEDULE_TIMEZONE %q: %w", c.PerfScheduleTimezone, err)
	}
	if _, err := modelperf.ParseTimes(c.PerfScheduleTimes, loc); err != nil {
		return fmt.Errorf("config: invalid PERF_SCHEDULE_TIMES %q: %w", c.PerfScheduleTimes, err)
	}
	return nil
}
