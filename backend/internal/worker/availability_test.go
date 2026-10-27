package worker

// 手动触发的运行期可用性前置（F-3）单测：不依赖 PG/Redis。
//
// 不可用判定发生在触库之前，所以这里可以嵌入一个 nil 的 Store 接口当作桩：
// 一旦代码顺序被改回「先查库再判断」，这些用例会以 nil pointer panic 失败，
// 这正是要钉住的性质——409 的副作用必须为零。

import (
	"context"
	"errors"
	"testing"
)

// nilStore 嵌入 nil Store：任何方法调用都会 panic，用来证明触发在触库前返回。
type nilStore struct{ Store }

func newUnavailableService(t *testing.T, probe func(context.Context) (bool, string)) *Service {
	t.Helper()
	registry, err := NewRegistry(Task{
		Name:                 "worker_history_cleanup",
		Interval:             0,
		ManualTriggerAllowed: true,
		// 刻意不 BindRun：网关进程的注册表就长这样。
	})
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	service, err := NewService(ServiceConfig{
		Registry:            registry,
		Store:               nilStore{},
		RuntimeAvailability: probe,
	})
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	return service
}

func TestTriggerRejectedWhenWorkerUnavailable(t *testing.T) {
	var probes int
	service := newUnavailableService(t, func(context.Context) (bool, string) {
		probes++
		return false, BlockedCodeWorkerUnavailable
	})

	result, err := service.Trigger(context.Background(), "worker_history_cleanup")
	if err == nil {
		t.Fatalf("worker 不在线时触发成功（result=%+v），期望 409 阻断", result)
	}
	var blocked *TaskBlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("错误类型 = %T（%v），期望 *TaskBlockedError", err, err)
	}
	if got := BlockedCodeOf(err); got != BlockedCodeWorkerUnavailable {
		t.Fatalf("阻断码 = %q，期望 %q", got, BlockedCodeWorkerUnavailable)
	}
	if probes != 1 {
		t.Fatalf("可用性探针调用 %d 次，期望 1 次", probes)
	}
}

func TestTriggerFallsBackToWorkerUnavailableCode(t *testing.T) {
	// 探针说不可用但没给原因：码必须回退成固定值，否则 handler 会拼出
	// 空的 problem type 后缀。
	service := newUnavailableService(t, func(context.Context) (bool, string) {
		return false, ""
	})

	_, err := service.Trigger(context.Background(), "worker_history_cleanup")
	if got := BlockedCodeOf(err); got != BlockedCodeWorkerUnavailable {
		t.Fatalf("空原因回退码 = %q，期望 %q（err=%v）", got, BlockedCodeWorkerUnavailable, err)
	}
}

func TestRuntimeAvailabilityDefaultsToAvailable(t *testing.T) {
	// 未注入探针 = 本进程就是执行者（worker 角色），必须恒可用。
	service := newUnavailableService(t, nil)
	available, reason := service.runtimeAvailability(context.Background())
	if !available || reason != "" {
		t.Fatalf("未注入探针返回 (%v, %q)，期望 (true, \"\")", available, reason)
	}
}
