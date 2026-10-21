package platform

// W10-EMAIL：worker 角色的通知模板补种。
//
// 为什么需要独立函数：模板播种原本只在 gateway 装配（httpapi assembly）与
// channel health 的 best-effort 路径发生。单独部署 worker、或 health 装配
// 失败时，email worker 会把所有投递判成 "email template is missing"——这是
// 配置性故障而不是投递失败，且没有可观测的重试路径。把播种收敛到 worker
// 启动期，保证「有 email worker 就一定有内置模板行」。
//
// 失败语义：只 Warn，不阻止 worker 上线。与 channel health 既有口径一致：
// 模板缺失会让个别通知 fail-closed，但不应阻断计费/任务主循环。

import (
	"context"
	"log/slog"
	"time"

	"newtg/gateway/internal/notify"
)

func seedNotifyTemplates(ctx context.Context, store *notify.Store, log *slog.Logger) {
	if store == nil {
		return
	}
	if log == nil {
		log = slog.Default()
	}
	service, err := notify.NewService(notify.ServiceConfig{Store: store, Logger: log})
	if err != nil {
		log.Warn("worker: construct notify service for template seeding failed", "error", err)
		return
	}
	seedCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := service.EnsureSeeded(seedCtx); err != nil {
		log.Warn("worker: seed notification templates failed", "error", err)
	}
}
