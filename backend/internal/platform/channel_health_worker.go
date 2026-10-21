package platform

// W7/M4-b：worker 角色内的渠道健康体检循环。
//
// 为什么不放在 gateway 角色：体检会周期性对全部渠道发起真实出站请求；
// 与中继/管理面隔离到 worker 进程，避免探测流量与用户请求争抢同一进程
// 的连接池/CPU，也让「worker 副本数=1」成为锁之外的第二重收敛（R-M4-13）。
//
// 默认常开（R-M4-07）：设计没有「启用体检」开关。部署后每 10 分钟会对
// status IN (1,3) 的全部可探测渠道发起一次最小请求（后台出站流量）。

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/channelhealth"
	"newtg/gateway/internal/config"
	"newtg/gateway/internal/configcenter"
	"newtg/gateway/internal/httpapi"
	"newtg/gateway/internal/notify"
	"newtg/gateway/internal/store"
)

// startChannelHealthWorker 装配并启动体检循环，返回 cancel+Wait 的停止函数。
//
// 装配失败返回 error 由调用方记录并继续：体检不是交易路径，缺 worker 不应
// 阻止计费/任务上线；但确定性装配错误（无 Store/无 Redis 锁）会在这里暴露。
func startChannelHealthWorker(parent context.Context, cfg *config.Config, db, logsDB *sql.DB,
	rdb *redis.Client, center *configcenter.Center, notifyStore *notify.Store,
	log *slog.Logger) (func(), error) {
	// 渠道凭据与 gateway 使用同一个 CREDENTIAL_ENCRYPTION_KEY（GC-8）。
	// 复用 httpapi.SecretSealer 而不是另写解封逻辑：密钥派生只有一份真源。
	prober, err := channelhealth.NewProber(channelhealth.ProberConfig{
		Opener: httpapi.NewSecretSealer(cfg.CredentialEncryptionKey),
		// 与中继/手工测试同一 transport（不新增出站策略）；整体超时由
		// Prober 的默认值 15s 控制（R-M4-14）。
		Client: httpapi.NewChannelProbeClient(),
	})
	if err != nil {
		return nil, err
	}

	// 站内通知（R-M4-09 的 C 节奏）：email 不在 M4（R-M4-18），因此
	// Service 不需要 Sender。直投走内置 bundle 兜底，播种失败只 Warn。
	notifier, err := notify.NewService(notify.ServiceConfig{Store: notifyStore, Logger: log})
	if err != nil {
		return nil, err
	}
	seedCtx, seedCancel := context.WithTimeout(parent, 5*time.Second)
	if err := notifier.EnsureSeeded(seedCtx); err != nil {
		log.Warn("worker: seed notification templates failed (health alerts use built-in copy)",
			"error", err)
	}
	seedCancel()

	worker, err := channelhealth.NewWorker(channelhealth.WorkerConfig{
		Store:    store.NewChannelRepo(db),
		Config:   center,
		Prober:   prober,
		Locker:   channelhealth.NewRedisLocker(rdb).WithLogger(log),
		Audit:    audit.New(logsDB),
		Notifier: notifier,
		Logger:   log,
		// 与 gateway 的 reporter 使用同一跳转路径。
		ChannelsURL: "/admin/channels",
	})
	if err != nil {
		return nil, err
	}

	healthCtx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := worker.RunLoop(healthCtx); err != nil {
			log.Error("worker: channel health loop exited with error", "error", err)
		}
	}()
	return func() {
		cancel()
		<-done
	}, nil
}
