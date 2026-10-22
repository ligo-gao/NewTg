package platform

// W17-2 注册表的静态契约（无外部依赖，随 `go test ./internal/platform` 运行）。
//
// 覆盖：
//   - 同名任务启动期失败；
//   - 白名单恰好是 billing-06 §13.5.1 冻结的 5 个任务；
//   - 只有绑定 Run 的任务才能被手动触发（否则触发会留下永不执行的 pending 行）。

import (
	"context"
	"errors"
	"testing"
	"time"

	"newtg/gateway/internal/worker"
)

func TestWorkerRegistryRejectsDuplicateNames(t *testing.T) {
	_, err := worker.NewRegistry(
		worker.Task{Name: "same", Interval: time.Minute},
		worker.Task{Name: "same", Interval: time.Minute},
	)
	if !errors.Is(err, worker.ErrDuplicateTask) {
		t.Fatalf("重复名字 err = %v，期望 ErrDuplicateTask", err)
	}
}

func TestDefaultRegistryManualWhitelistIsFrozen(t *testing.T) {
	registry, err := worker.DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	want := map[string]bool{
		"log_cleanup":             true,
		"log_partition_precreate": true,
		"redemption_expiry":       true,
		"rankings_snapshot":       true,
		"worker_history_cleanup":  true,
	}
	got := make(map[string]bool)
	for _, task := range registry.Tasks() {
		if task.ManualTriggerAllowed {
			got[task.Name] = true
		}
		if task.Concurrency != worker.ConcurrencySingle {
			t.Fatalf("%s concurrency = %q，期望 single", task.Name, task.Concurrency)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("白名单 = %v，期望 %v", got, want)
	}
	for name := range want {
		if !got[name] {
			t.Fatalf("白名单缺少 %s", name)
		}
	}
	// 业务轮询类/计费对账不得手动触发（§13.5.1 明确排除）。
	for _, name := range []string{"async_task_poll", "midjourney_poll", "channel_test", "deploy_billing_reconcile"} {
		task, ok := registry.Get(name)
		if !ok {
			t.Fatalf("注册表缺少 %s", name)
		}
		if task.ManualTriggerAllowed {
			t.Fatalf("%s 不应允许手动触发", name)
		}
	}
}

func TestRegistryBindRunRejectsUnregisteredTask(t *testing.T) {
	registry, err := worker.DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	if err := registry.BindRun("no-such-task", func(_ context.Context) (worker.Result, error) {
		return nil, nil
	}); err == nil {
		t.Fatal("绑定未注册任务必须失败")
	}
}
