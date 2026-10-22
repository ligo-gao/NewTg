package metrics

import (
	"time"

	"newtg/gateway/internal/version"
)

// 观察函数统一用 recover 兜底：指标是旁路，任何采样/标签错误都不得把
// panic 带回请求或 worker 循环（R-W10-08「采样失败不得影响请求」）。
func swallowPanic() { _ = recover() }

// statusClass 只产生冻结枚举 2xx/4xx/5xx；1xx/3xx 归 other。
//
// 为什么不按具体状态码做标签：状态码是有限集但组合会随错误面膨胀，且
// 告警/看板只按类聚合；冻结口径见 platform-05 §3.2.1。
func statusClass(code int) string {
	switch {
	case code >= 200 && code < 300:
		return "2xx"
	case code >= 400 && code < 500:
		return "4xx"
	case code >= 500 && code < 600:
		return "5xx"
	default:
		return "other"
	}
}

func streamLabel(stream bool) string {
	if stream {
		return "true"
	}
	return "false"
}

func (s *Set) ObserveHTTP(service, method, route string, status int, d time.Duration, size int) {
	defer swallowPanic()
	if route == "" {
		// 未匹配路由绝不能退化成原始 path：path 含 id/令牌，基数与 PII 双红线。
		route = "unmatched"
	}
	s.httpRequests.WithLabelValues(service, method, route, statusClass(status)).Inc()
	s.httpDuration.WithLabelValues(service, method, route).Observe(d.Seconds())
	s.httpResponseSize.WithLabelValues(service, route).Observe(float64(size))
}

func (s *Set) IncHTTPInFlight(service string) {
	defer swallowPanic()
	s.httpInFlight.WithLabelValues(service).Inc()
}

func (s *Set) DecHTTPInFlight(service string) {
	defer swallowPanic()
	s.httpInFlight.WithLabelValues(service).Dec()
}

// ObserveRelayRequest 记录中继最终结果。
//
// status 取值对齐 usage_logs.status：success/error/refused/refunded。首批
// 接线只区分 success/error（其余状态来自结算路径，后续批次补齐）。
func (s *Set) ObserveRelayRequest(endpoint, model string, stream bool, status string, d time.Duration) {
	defer swallowPanic()
	m := s.guardModel.Value(model)
	s.relayRequests.WithLabelValues(endpoint, m, streamLabel(stream), status).Inc()
	s.relayDuration.WithLabelValues(endpoint, m, streamLabel(stream)).Observe(d.Seconds())
}

func (s *Set) ObserveRelayFirstToken(endpoint, model string, d time.Duration) {
	defer swallowPanic()
	if d <= 0 {
		// 非流式/缺失不得填占位值（contract.FirstTokenMS 同为 nil 语义）。
		return
	}
	s.relayFirstToken.WithLabelValues(endpoint, s.guardModel.Value(model)).Observe(d.Seconds())
}

// ObserveRelayAttempt 记录一次上游尝试与渠道维度。
//
// result ∈ {ok, retryable_fail, nonretryable_fail}；errorClass 只在失败时
// 非空。fromType 非空且 attemptIndex>0 时同时记一次故障转移（同类型重试
// 也记：它同样是「上一轮失败后换了一轮」的可观测量）。
//
// 成功（result=="ok"）绝不递增 errors_total：调用方即使传了 ErrUnknown
// （枚举零值），也不能把成功样本算成错误，否则 A3 错误率恒 ~100%。
func (s *Set) ObserveRelayAttempt(endpoint, model string, stream bool, channelID int,
	channelType, fromType, result, errorClass string, attemptIndex int, d time.Duration) {
	defer swallowPanic()
	s.relayAttempts.WithLabelValues(endpoint, result).Inc()
	if result != "ok" && errorClass != "" {
		s.relayErrors.WithLabelValues(endpoint, errorClass).Inc()
	}
	if channelID > 0 {
		channel := s.guardChannel.Value(itoa(channelID))
		// 渠道成功率的冻结取值是 success/error（platform-05 §3.2.3），
		// 与中继尝试的三态（ok/retryable_fail/nonretryable_fail）不同口径：
		// 失败一律折叠为 error，避免同一渠道被两种枚举重复计数。
		channelResult := "error"
		if result == "ok" {
			channelResult = "success"
		}
		s.channelRequests.WithLabelValues(channel, channelType, channelResult).Inc()
		s.channelDuration.WithLabelValues(channel, channelType).Observe(d.Seconds())
	}
	if attemptIndex > 0 && fromType != "" {
		// reason 优先用冻结的错误类（可归因），缺失时退回尝试结果。
		reason := errorClass
		if reason == "" {
			reason = result
		}
		s.channelFailover.WithLabelValues(fromType, channelType, reason).Inc()
	}
}

func (s *Set) ObserveChannelAutoDisabled(channelType, reason string) {
	defer swallowPanic()
	s.channelAutoDisabled.WithLabelValues(channelType, reason).Inc()
}

func (s *Set) ObservePreConsume(model, group string, credits int64) {
	defer swallowPanic()
	if credits <= 0 {
		return
	}
	s.billingPreConsume.WithLabelValues(s.guardModel.Value(model), s.guardGroup.Value(group)).Add(float64(credits))
}

func (s *Set) ObserveSettle(model, group, billingMode string, credits int64) {
	defer swallowPanic()
	if credits <= 0 {
		return
	}
	s.billingSettle.WithLabelValues(s.guardModel.Value(model), s.guardGroup.Value(group), billingMode).Add(float64(credits))
}

func (s *Set) ObserveRefund(reason string, credits int64) {
	defer swallowPanic()
	if credits <= 0 {
		return
	}
	s.billingRefund.WithLabelValues(reason).Add(float64(credits))
}

func (s *Set) ObserveQuotaClamp(op, kind string) {
	defer swallowPanic()
	s.billingQuotaClamp.WithLabelValues(op, kind).Inc()
}

func (s *Set) ObserveInsufficientQuota(model string) {
	defer swallowPanic()
	s.billingInsufficient.WithLabelValues(s.guardModel.Value(model)).Inc()
}

// ObserveWorkerTask 记录任务执行；成功时刷新 last_success。
func (s *Set) ObserveWorkerTask(taskType, status string, d time.Duration) {
	defer swallowPanic()
	s.workerTaskRuns.WithLabelValues(taskType, status).Inc()
	if d >= 0 {
		s.workerTaskDuration.WithLabelValues(taskType).Observe(d.Seconds())
	}
	if status == "succeeded" {
		s.workerTaskLastSuccess.WithLabelValues(taskType).Set(float64(time.Now().Unix()))
	}
}

// RegisterWorkerTask 预建任务子序列。
//
// 刻意**不**在启动时写 last_success：若预置为启动时刻，A11（陈旧度 >
// 3×周期）会在任务从未成功运行过时被这个假基线长期掩盖；若预置 0，又会
// 立即误报。正确语义是「没有成功过就没有时间序列」——PromQL 对空集合
// 不告警。family 在首次成功（ObserveWorkerTask status=succeeded）时出现。
func (s *Set) RegisterWorkerTask(taskType string) {
	defer swallowPanic()
	s.workerTaskRuns.WithLabelValues(taskType, "succeeded").Add(0)
	s.workerTaskRuns.WithLabelValues(taskType, "failed").Add(0)
	s.workerTaskRuns.WithLabelValues(taskType, "skipped").Add(0)
	s.workerTaskDuration.WithLabelValues(taskType)
}

func (s *Set) SetWorkerAbandoned(source string, refunds int64, oldestAge time.Duration) {
	defer swallowPanic()
	if refunds > 0 {
		s.workerAbandonedRefund.WithLabelValues(source).Add(float64(refunds))
	}
	if oldestAge < 0 {
		oldestAge = 0
	}
	s.workerAbandonedAge.Set(oldestAge.Seconds())
}

func (s *Set) SetWorkerStreamLag(lag float64) {
	defer swallowPanic()
	if lag < 0 {
		lag = 0
	}
	s.workerStreamLag.Set(lag)
}

// workerPlaceholderTasks 是首批交付中尚未有独立循环或只在循环外层计数的
// 任务类型。命名与 platform-05 §3.2.5 / A10 的关键任务对齐
// （preconsume_reconcile、log_flush、rankings_snapshot 等），避免规则与
// 实际 task_type 对不上。
var workerPlaceholderTasks = []string{
	"task_poller",
	"channel_health",
	"preconsume_reconcile",
	"subscription_reconcile",
	"log_flush",
	"rankings_snapshot",
	"email",
	"retention",
	// W10-N10：过期 usage_logs 分区删除（默认关闭，见 log.partition_drop_enabled）。
	"log_partition_drop",
}

// InitWorker 初始化 worker 角色指标基线。
//
// usage_logs_stream_lag：N2 的 Redis Stream 消费者尚未落地，本批按冻结口径
// **注册为 0 并注明**，而不是留空（A13 依赖该指标存在；空 family 无法被
// 规则命中，等于告警失效）。
func (s *Set) InitWorker() {
	defer swallowPanic()
	for _, task := range workerPlaceholderTasks {
		s.RegisterWorkerTask(task)
	}
	s.workerAbandonedRefund.WithLabelValues("wallet").Add(0)
	s.workerAbandonedRefund.WithLabelValues("subscription").Add(0)
	s.workerAbandonedAge.Set(0)
	s.workerStreamLag.Set(0)
}

func (s *Set) SetRedisUp(up bool) {
	defer swallowPanic()
	if up {
		s.redisUp.Set(1)
		return
	}
	s.redisUp.Set(0)
}

// redisCommandWhitelist 是 platform-05 §3.2.7 冻结的命令白名单。
//
// 命令名本身是有限枚举，但仍按白名单收敛：第三方库新增命令或 Lua 脚本
// 的动态名不得把基数带进 TSDB。
var redisCommandWhitelist = map[string]struct{}{
	"get": {}, "set": {}, "del": {}, "incr": {}, "expire": {},
	"xadd": {}, "xreadgroup": {}, "publish": {}, "ping": {},
	// pipeline 是「一次管道批次」的聚合口径：逐条命令无法单独计时，
	// 用批次总耗时记一条，避免把整条 pipeline 的时长重复记到每条命令上。
	"pipeline": {},
}

func (s *Set) ObserveRedisCommand(cmd string, d time.Duration) {
	defer swallowPanic()
	if _, ok := redisCommandWhitelist[cmd]; !ok {
		cmd = "other"
	}
	s.redisCommandDuration.WithLabelValues(cmd).Observe(d.Seconds())
}

func (s *Set) SetDependencyUp(dependency string, up bool) {
	defer swallowPanic()
	v := 0.0
	if up {
		v = 1
	}
	s.dependencyUp.WithLabelValues(dependency).Set(v)
}

// SetMigration 用 Reset 保证同一时刻只有一组 (version,dirty) 序列。
//
// 每次启动/迁移后调用；value 恒为 1，版本与 dirty 都在标签里（A19 以
// newtg_migration_version{dirty="true"} == 1 判定迁移半途）。
func (s *Set) SetMigration(migration int64, dirty bool) {
	defer swallowPanic()
	dirtyLabel := "false"
	if dirty {
		dirtyLabel = "true"
	}
	s.migrationState.Reset()
	s.migrationState.WithLabelValues(itoa64(migration), dirtyLabel).Set(1)
}

// BuildVersion 返回当前构建版本标签值（构建信息只在 init 设置一次）。
func (s *Set) BuildVersion() string { return version.Version }
