package platform

import (
	"context"
	"database/sql"
	"log/slog"
	"strings"
	"sync"

	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/billing"
	"newtg/gateway/internal/configcenter"
)

// 本文件装配 W10-N2 的 usage_logs 批处理链路（R-W10-02 / R-W10-11）。
//
// 两个角色各承担一半：
//   - gateway（以及 worker 复用的计费引擎）是生产者：`Saturate` 先 XADD，
//     成功即受理，失败同步回退；
//   - worker 是消费者：单消费组批量落库，提交后 XACK。
//
// 【默认关闭】`log.batch_enabled` 默认 false：先保持既有同步逐条写入上线，
// 验证后再打开。开关在启动时解析一次快照（与 task 轮询上限同口径），改动
// 随进程重建生效——避免两个副本同时走不同写入路径造成排障混乱。
//
// 【部署前提】打开开关前必须存在 worker 角色：gateway 只生产不消费，若没有
// worker，事件会堆在 Stream 里直到 MAXLEN 修剪。启动日志会显式记录状态。

// settingLogBatchEnabled 是 usage_logs 批处理的总开关（默认 false）。
const settingLogBatchEnabled = "log.batch_enabled"

// usageLogBatchEnabled 解析批处理开关。标签解析失败按 false（同步路径）处理：
// 批处理是加速面，配置异常必须回落到更保守的同步写入，而不是 fail-open。
func usageLogBatchEnabled(center *configcenter.Center, log *slog.Logger) bool {
	enabled := configBool(center, settingLogBatchEnabled, false)
	if log != nil {
		log.Info("usage_logs batching setting resolved",
			"key", settingLogBatchEnabled, "enabled", enabled,
			"mode", map[bool]string{true: "redis_stream", false: "synchronous"}[enabled])
	}
	return enabled
}

// newUsageLogDBLogger 构造日志写入器：始终挂 feed；开关打开时再挂 Stream 生产者。
//
// 生产者在 gateway 与 worker 都按同一开关装配：worker 的兜底退款也要与
// 成功事件同流（R-W10-02），否则退款收敛会绕开消费者。
func newUsageLogDBLogger(logsDB *sql.DB, feed billing.FeedPublisher,
	center *configcenter.Center, rdb *redis.Client, log *slog.Logger) *billing.DBLogger {

	writer := billing.NewDBLogger(logsDB).WithFeed(feed, log)
	if usageLogBatchEnabled(center, log) {
		writer = writer.WithBatchQueue(billing.NewRedisUsageLogQueue(rdb), log)
	}
	return writer
}

// startUsageLogStreamWorker 在 worker 角色启动 Stream 消费循环，返回停止函数。
//
// 停止函数取消上下文并等待循环退出；调用方必须在关闭 Redis/日志库之前调用。
// 消费循环自身不因 Redis/DB 故障退出（失败只记录 + 下一轮重试），因此这里
// 只需要处理「未启用」与「正常停机」两种路径。
func startUsageLogStreamWorker(parent context.Context, center *configcenter.Center,
	rdb *redis.Client, logsDB *sql.DB, feed billing.FeedPublisher,
	node, instanceID string, log *slog.Logger) func() {

	if log == nil {
		log = slog.Default()
	}
	if !usageLogBatchEnabled(center, log) {
		return func() {}
	}

	consumer := strings.TrimSpace(node)
	if consumer == "" {
		consumer = "worker"
	}
	if instanceID = strings.TrimSpace(instanceID); instanceID != "" {
		consumer = consumer + "-" + instanceID
	}

	ctx, cancel := context.WithCancel(parent)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := billing.RunUsageLogStreamLoop(ctx, billing.UsageLogStreamDeps{
			Redis:    rdb,
			LogsDB:   logsDB,
			Logger:   log,
			Consumer: consumer,
			Feed:     feed,
		}); err != nil {
			// 只有装配错误会走到这里（Redis/DB 故障由循环内部重试）。
			// 不阻止 worker 上线，但必须让「日志只进不出」可见。
			log.Error("usage log stream consumer exited with error; batch logs will accumulate",
				"error", err, "consumer", consumer)
		}
	}()
	return func() {
		cancel()
		wg.Wait()
	}
}
