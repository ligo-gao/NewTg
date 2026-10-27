// Package metrics 是 NEW TG 的 Prometheus 指标叶子包（W10-METRICS）。
//
// 依赖方向：本包只依赖 prometheus client 与 version（构建信息），不 import
// 任何业务域包；业务域通过本包的 Observe* 接缝上报，避免指标包反向依赖。
//
// 冻结口径：
//   - 首批指标见 platform-05 §3.2（R-W10-08 允许分批），必须包含
//     quota_clamp / settle_credits / usage_logs_stream_lag / abandoned_* /
//     dependency_up / migration_version；
//   - model/channel_id/group 是「允许但需控制」标签，写标签前经 labelGuard
//     归并到 other（§3.3 业主裁决 U-05-5）；
//   - 所有观察函数都不得 panic、不得阻塞请求路径：内部 recover 兜底，采样
//     失败只丢样本，绝不影响业务。
package metrics

import (
	"database/sql"
	"net/http"
	"runtime"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"newtg/gateway/internal/version"
)

// Namespace 是全部指标的统一前缀（newtg_<subsystem>_<name>_<unit>）。
const Namespace = "newtg"

// 直方图桶边界是冻结契约的一部分：测试与看板/告警按这些边界插值，
// 调整边界属于指标契约变更，不得随手改。理由：HTTP 与中继的长尾在
// 30–300s（SSE/大模型），而 Redis/任务需要毫秒到小时的跨度。
var (
	httpDurationBuckets  = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}
	httpSizeBuckets      = []float64{256, 1024, 4096, 16384, 65536, 262144, 1048576, 4194304, 16777216, 67108864}
	relayDurationBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300}
	firstTokenBuckets    = []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}
	taskDurationBuckets  = []float64{0.001, 0.01, 0.1, 1, 5, 15, 60, 300, 900, 3600}
	redisDurationBuckets = []float64{0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1}
)

// Set 是一次装配产生的指标集合。
//
// 不用包级全局 vec 而用 Set：测试可以构造独立 registry，不会与默认 registry
// 的注册冲突；生产进程经 Default() 共用同一份。
type Set struct {
	httpRequests     *prometheus.CounterVec
	httpDuration     *prometheus.HistogramVec
	httpInFlight     *prometheus.GaugeVec
	httpResponseSize *prometheus.HistogramVec

	relayRequests   *prometheus.CounterVec
	relayDuration   *prometheus.HistogramVec
	relayFirstToken *prometheus.HistogramVec
	relayAttempts   *prometheus.CounterVec
	relayErrors     *prometheus.CounterVec

	channelRequests     *prometheus.CounterVec
	channelDuration     *prometheus.HistogramVec
	channelFailover     *prometheus.CounterVec
	channelAutoDisabled *prometheus.CounterVec

	billingPreConsume   *prometheus.CounterVec
	billingSettle       *prometheus.CounterVec
	billingRefund       *prometheus.CounterVec
	billingQuotaClamp   *prometheus.CounterVec
	billingInsufficient *prometheus.CounterVec
	// billingSettleFailures 是「上游已执行但结算失败」的计数：非流式会被
	// 402 阻断交付，流式只能靠这条指标与 ERROR 日志发现欠费请求。
	billingSettleFailures *prometheus.CounterVec

	workerTaskRuns        *prometheus.CounterVec
	workerTaskDuration    *prometheus.HistogramVec
	workerTaskLastSuccess *prometheus.GaugeVec
	workerAbandonedRefund *prometheus.CounterVec
	workerAbandonedAge    prometheus.Gauge
	workerStreamLag       prometheus.Gauge

	dbPool               *dbPoolCollector
	redisUp              prometheus.Gauge
	redisCommandDuration *prometheus.HistogramVec

	// 演示站（modules-08 §3.17 Q6 / G7-R48）：访客的试探性写入不写审计，
	// 只按路由族累加计数；中继超限按 scope（ip/global）累加。两个标签都是
	// 有限枚举（路由族来自服务端白名单，scope 只有两个值）。
	demoWriteRejected      *prometheus.CounterVec
	demoRelayQuotaExceeded *prometheus.CounterVec

	buildInfo      *prometheus.GaugeVec
	dependencyUp   *prometheus.GaugeVec
	migrationState *prometheus.GaugeVec

	guardModel   *labelGuard
	guardChannel *labelGuard
	guardGroup   *labelGuard
}

func newSet() *Set {
	s := &Set{
		httpRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "http", Name: "requests_total",
			Help: "HTTP requests by service, method, templated route and status class.",
		}, []string{"service", "method", "route", "status_class"}),
		httpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: Namespace, Subsystem: "http", Name: "request_duration_seconds",
			Help: "HTTP request latency in seconds.", Buckets: httpDurationBuckets,
		}, []string{"service", "method", "route"}),
		httpInFlight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: Namespace, Subsystem: "http", Name: "in_flight_requests",
			Help: "HTTP requests currently being served.",
		}, []string{"service"}),
		httpResponseSize: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: Namespace, Subsystem: "http", Name: "response_size_bytes",
			Help: "HTTP response body size in bytes.", Buckets: httpSizeBuckets,
		}, []string{"service", "route"}),

		relayRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "relay", Name: "requests_total",
			Help: "Relay requests by endpoint, model, stream flag and final status.",
		}, []string{"endpoint", "model", "stream", "status"}),
		relayDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: Namespace, Subsystem: "relay", Name: "request_duration_seconds",
			Help: "Relay end-to-end latency in seconds.", Buckets: relayDurationBuckets,
		}, []string{"endpoint", "model", "stream"}),
		relayFirstToken: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: Namespace, Subsystem: "relay", Name: "first_token_seconds",
			Help: "Time to first streamed token in seconds (streaming only).", Buckets: firstTokenBuckets,
		}, []string{"endpoint", "model"}),
		relayAttempts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "relay", Name: "attempts_total",
			Help: "Upstream attempts by endpoint and result.",
		}, []string{"endpoint", "result"}),
		relayErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "relay", Name: "errors_total",
			Help: "Relay errors by endpoint and frozen error class.",
		}, []string{"endpoint", "error_class"}),

		channelRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "channel", Name: "requests_total",
			Help: "Upstream attempts by channel and result.",
		}, []string{"channel_id", "channel_type", "result"}),
		channelDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: Namespace, Subsystem: "channel", Name: "request_duration_seconds",
			Help: "Upstream attempt latency by channel in seconds.", Buckets: relayDurationBuckets,
		}, []string{"channel_id", "channel_type"}),
		channelFailover: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "channel", Name: "failover_total",
			Help: "Retry/failover transitions between upstream channel types.",
		}, []string{"from_type", "to_type", "reason"}),
		channelAutoDisabled: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "channel", Name: "auto_disabled_total",
			Help: "Automatic channel disable events by channel type and reason code.",
		}, []string{"channel_type", "reason"}),

		billingPreConsume: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "billing", Name: "preconsume_credits_total",
			Help: "Credits reserved by pre-consume by model and group.",
		}, []string{"model", "group"}),
		billingSettle: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "billing", Name: "settle_credits_total",
			Help: "Credits settled by billing mode, model and group.",
		}, []string{"model", "group", "billing_mode"}),
		billingRefund: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "billing", Name: "refund_credits_total",
			Help: "Credits refunded/released by reason.",
		}, []string{"reason"}),
		billingQuotaClamp: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "billing", Name: "quota_clamp_total",
			Help: "Quota saturation events by operation and clamp kind.",
		}, []string{"op", "kind"}),
		billingInsufficient: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "billing", Name: "insufficient_quota_total",
			Help: "Requests refused with HTTP 402 (insufficient quota) by model.",
		}, []string{"model"}),
		billingSettleFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "billing", Name: "settle_failures_total",
			Help: "Settlement failures after the upstream call by model, group and reason.",
		}, []string{"model", "group", "reason"}),

		workerTaskRuns: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "worker", Name: "task_runs_total",
			Help: "Worker task runs by task type and status.",
		}, []string{"task_type", "status"}),
		workerTaskDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: Namespace, Subsystem: "worker", Name: "task_duration_seconds",
			Help: "Worker task duration in seconds.", Buckets: taskDurationBuckets,
		}, []string{"task_type"}),
		workerTaskLastSuccess: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: Namespace, Subsystem: "worker", Name: "task_last_success_timestamp_seconds",
			Help: "Unix timestamp of the last successful run per worker task type.",
		}, []string{"task_type"}),
		workerAbandonedRefund: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "worker", Name: "abandoned_refund_total",
			Help: "Abandoned pre-consume refunds by funding source.",
		}, []string{"source"}),
		workerAbandonedAge: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: Namespace, Subsystem: "worker", Name: "abandoned_oldest_age_seconds",
			Help: "Age of the oldest unsettled hold in seconds.",
		}),
		workerStreamLag: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: Namespace, Subsystem: "worker", Name: "usage_logs_stream_lag",
			Help: "Unacked entries in the usage_logs Redis Stream.",
		}),

		dbPool: newDBPoolCollector(),
		redisUp: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: Namespace, Subsystem: "redis", Name: "up",
			Help: "Redis PING result (1 = up, 0 = down).",
		}),
		redisCommandDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: Namespace, Subsystem: "redis", Name: "command_duration_seconds",
			Help: "Redis command latency in seconds (cmd is whitelisted).", Buckets: redisDurationBuckets,
		}, []string{"cmd"}),

		demoWriteRejected: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "demo", Name: "write_rejected_total",
			Help: "Demo-site write rejections by server-side route family.",
		}, []string{"route_family"}),
		demoRelayQuotaExceeded: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "demo", Name: "relay_quota_exceeded_total",
			Help: "Demo-site relay quota rejections by scope (ip|global).",
		}, []string{"scope"}),

		buildInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: Namespace, Name: "build_info",
			Help: "Build information; value is always 1.",
		}, []string{"version", "commit", "goversion", "goos", "goarch"}),
		dependencyUp: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: Namespace, Name: "dependency_up",
			Help: "Dependency availability (1 = up, 0 = down).",
		}, []string{"dependency"}),
		migrationState: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: Namespace, Name: "migration_version",
			Help: "Current schema migration version; the series is 1 for the active (version,dirty) pair.",
		}, []string{"version", "dirty"}),

		guardModel:   newLabelGuard(guardLimit),
		guardChannel: newLabelGuard(guardLimit),
		guardGroup:   newLabelGuard(guardLimit),
	}
	s.buildInfo.WithLabelValues(version.Version, version.Commit, runtime.Version(),
		runtime.GOOS, runtime.GOARCH).Set(1)
	return s
}

// collectors 返回需要注册到 registry 的 collector 列表。
func (s *Set) collectors() []prometheus.Collector {
	return []prometheus.Collector{
		s.httpRequests, s.httpDuration, s.httpInFlight, s.httpResponseSize,
		s.relayRequests, s.relayDuration, s.relayFirstToken, s.relayAttempts, s.relayErrors,
		s.channelRequests, s.channelDuration, s.channelFailover, s.channelAutoDisabled,
		s.billingPreConsume, s.billingSettle, s.billingRefund, s.billingQuotaClamp,
		s.billingInsufficient,
		s.billingSettleFailures,
		s.workerTaskRuns, s.workerTaskDuration, s.workerTaskLastSuccess,
		s.workerAbandonedRefund, s.workerAbandonedAge, s.workerStreamLag,
		s.dbPool, s.redisUp, s.redisCommandDuration,
		s.demoWriteRejected, s.demoRelayQuotaExceeded,
		s.buildInfo, s.dependencyUp, s.migrationState,
	}
}

// NewRegistry 构造独立的 registry 与指标集合（测试与多实例场景）。
//
// 同时注册 go_* / process_* 标准采集器（platform-05 §3.1）：这些是客户端库
// 自带能力，不重复造，但 /metrics 必须能看到它们。
//
// instanceID 非空时，所有序列（含标准采集器与新注册的 newtg_*）都带上常量
// instance 标签，取值与 /api/instances 心跳 id 同源，便于在 Prometheus 侧
// 按实例 join。不传或传空串时保持原有指标形状（无 instance 标签）。
func NewRegistry(instanceID ...string) (*prometheus.Registry, *Set) {
	set := newSet()
	reg := prometheus.NewRegistry()
	var registerer prometheus.Registerer = reg
	if len(instanceID) > 0 && instanceID[0] != "" {
		registerer = prometheus.WrapRegistererWith(
			prometheus.Labels{"instance": instanceID[0]}, reg)
	}
	registerer.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	registerer.MustRegister(set.collectors()...)
	return reg, set
}

var (
	defaultOnce     sync.Once
	defaultRegistry *prometheus.Registry
	defaultSet      *Set

	instanceMu sync.RWMutex
	instanceID string
)

// SetInstanceID 设置注入到全部 newtg_*（及 go_*/process_*）序列上的常量
// instance 标签值。必须在第一次使用本包之前调用（各角色的 run* 装配开头）：
// Default 是懒初始化的，初始化后再调用不会重建 registry，tag 不会生效。
func SetInstanceID(id string) {
	instanceMu.Lock()
	instanceID = id
	instanceMu.Unlock()
}

func processInstanceID() string {
	instanceMu.RLock()
	defer instanceMu.RUnlock()
	return instanceID
}

// Default 返回进程级默认指标集合（懒初始化，并发安全）。
func Default() *Set {
	defaultOnce.Do(func() { defaultRegistry, defaultSet = NewRegistry(processInstanceID()) })
	return defaultSet
}

// Registry 返回进程级默认 registry。
func Registry() *prometheus.Registry {
	Default()
	return defaultRegistry
}

// Handler 返回 /metrics 的 promhttp handler。
//
// Content-Type 固定为 text/plain; version=0.0.4（与既有 stub 的冻结契约
// 一致），不开启 OpenMetrics，避免抓取端按新格式解析时行为漂移。
func Handler() http.Handler {
	return promhttp.HandlerFor(Registry(), promhttp.HandlerOpts{EnableOpenMetrics: false})
}

// ---------- 包级便捷入口（委托 Default，保持业务域调用简洁） ----------

// ObserveHTTP 记录一次 HTTP 请求（中间件调用）。
func ObserveHTTP(service, method, route string, status int, d time.Duration, size int) {
	Default().ObserveHTTP(service, method, route, status, d, size)
}

// IncHTTPInFlight / DecHTTPInFlight 维护并发水位。
func IncHTTPInFlight(service string) { Default().IncHTTPInFlight(service) }
func DecHTTPInFlight(service string) { Default().DecHTTPInFlight(service) }

// HTTPMiddleware 用 service 标签包装 http.Handler。
func HTTPMiddleware(service string, next http.Handler) http.Handler {
	return Default().HTTPMiddleware(service, next)
}

// ObserveRelayRequest 记录中继最终结果与端到端延迟。
func ObserveRelayRequest(endpoint, model string, stream bool, status string, d time.Duration) {
	Default().ObserveRelayRequest(endpoint, model, stream, status, d)
}

// ObserveRelayFirstToken 记录 TTFT（仅流式）。
func ObserveRelayFirstToken(endpoint, model string, d time.Duration) {
	Default().ObserveRelayFirstToken(endpoint, model, d)
}

// ObserveRelayAttempt 记录一次上游尝试。
func ObserveRelayAttempt(endpoint, model string, stream bool, channelID int, channelType,
	fromType, result, errorClass string, attemptIndex int, d time.Duration) {
	Default().ObserveRelayAttempt(endpoint, model, stream, channelID, channelType,
		fromType, result, errorClass, attemptIndex, d)
}

// ObserveChannelAutoDisabled 记录一次自动禁用。
func ObserveChannelAutoDisabled(channelType, reason string) {
	Default().ObserveChannelAutoDisabled(channelType, reason)
}

// ObservePreConsume 记录预扣积分。
func ObservePreConsume(model, group string, credits int64) {
	Default().ObservePreConsume(model, group, credits)
}

// ObserveSettle 记录结算积分。
func ObserveSettle(model, group, billingMode string, credits int64) {
	Default().ObserveSettle(model, group, billingMode, credits)
}

// ObserveRefund 记录退款/释放积分。
func ObserveRefund(reason string, credits int64) { Default().ObserveRefund(reason, credits) }

// ObserveQuotaClamp 记录一次配额钳制事件。
func ObserveQuotaClamp(op, kind string) { Default().ObserveQuotaClamp(op, kind) }

// ObserveInsufficientQuota 记录一次 402 拒绝。
func ObserveInsufficientQuota(model string) { Default().ObserveInsufficientQuota(model) }

// ObserveSettleFailure 记录一次结算失败（响应可能已交付或即将被 402 阻断）。
func ObserveSettleFailure(model, group, reason string) {
	Default().ObserveSettleFailure(model, group, reason)
}

// ObserveDemoWriteRejected 记录一次演示站只读守卫拒绝。
func ObserveDemoWriteRejected(routeFamily string) { Default().ObserveDemoWriteRejected(routeFamily) }

// ObserveDemoRelayQuotaExceeded 记录一次演示站中继额度耗尽。
func ObserveDemoRelayQuotaExceeded(scope string) { Default().ObserveDemoRelayQuotaExceeded(scope) }

// ObserveWorkerTask 记录一次 worker 任务执行。
func ObserveWorkerTask(taskType, status string, d time.Duration) {
	Default().ObserveWorkerTask(taskType, status, d)
}

// RegisterWorkerTask 预建任务指标子序列，保证空跑任务也有 family。
func RegisterWorkerTask(taskType string) { Default().RegisterWorkerTask(taskType) }

// SetWorkerAbandoned 设置预扣残留兜底指标。
func SetWorkerAbandoned(source string, refunds int64, oldestAge time.Duration) {
	Default().SetWorkerAbandoned(source, refunds, oldestAge)
}

// SetWorkerStreamLag 设置 usage_logs Stream 积压（无 Stream 时固定 0）。
func SetWorkerStreamLag(lag float64) { Default().SetWorkerStreamLag(lag) }

// InitWorker 初始化 worker 角色的指标基线。
func InitWorker() { Default().InitWorker() }

// AttachDBPools 绑定主库/日志库连接池（只读 Stats，不执行 SQL）。
func AttachDBPools(main, logs *sql.DB) { Default().AttachDBPools(main, logs) }

// SetRedisUp 设置 Redis 可用性。
func SetRedisUp(up bool) { Default().SetRedisUp(up) }

// ObserveRedisCommand 记录一次 Redis 命令耗时。
func ObserveRedisCommand(cmd string, d time.Duration) {
	Default().ObserveRedisCommand(cmd, d)
}

// SetDependencyUp 设置依赖可用性。
func SetDependencyUp(dependency string, up bool) { Default().SetDependencyUp(dependency, up) }

// SetMigration 设置迁移版本与 dirty 标志。
func SetMigration(version int64, dirty bool) { Default().SetMigration(version, dirty) }
