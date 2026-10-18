// Package platform 是单二进制 newtg 的三种运行角色入口。
// 本文件为桩实现，由 Task 4/6 填实。
package platform

import (
	"context"
	"errors"

	"newtg/gateway/internal/config"
)

var errNotImplemented = errors.New("not implemented")

// RunGateway 以 gateway 角色运行：HTTP 服务、计费、限流等。
func RunGateway(ctx context.Context, cfg *config.Config) error {
	return errNotImplemented
}

// RunWorker 以 worker 角色运行：后台任务消费等。
func RunWorker(ctx context.Context, cfg *config.Config) error {
	return errNotImplemented
}

// RunMigrate 以 migrate 角色运行：数据库迁移，成功后退出。
func RunMigrate(ctx context.Context, cfg *config.Config) error {
	return errNotImplemented
}
