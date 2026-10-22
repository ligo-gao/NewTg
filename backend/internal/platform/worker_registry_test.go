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

// TestLogCleanupRespectsPartitionDropSwitch 锁定控制端裁决：
// 开关关闭时手动清理零 DROP，并返回固定码 log_partition_drop_disabled；
// 谓词同时挂到注册表，Service.Trigger 会在入队前 409。
func TestLogCleanupRespectsPartitionDropSwitch(t *testing.T) {
	registry, err := worker.DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	// 不触 DB：开关关闭分支在打开任何连接之前返回。
	deps := workerTaskDeps{
		Settings: logRetentionSettings{PartitionDropEnabled: false},
	}
	if err := bindWorkerTaskRuns(registry, deps); err != nil {
		t.Fatalf("bind: %v", err)
	}
	task, ok := registry.Get("log_cleanup")
	if !ok {
		t.Fatal("log_cleanup 未注册")
	}
	if task.Enabled == nil || task.Enabled() {
		t.Fatal("log_cleanup 应挂上 partition_drop_enabled 谓词且当前为关闭")
	}
	_, runErr := task.Run(context.Background())
	if got := worker.BlockedCodeOf(runErr); got != "log_partition_drop_disabled" {
		t.Fatalf("Run 错误码 = %q，期望 log_partition_drop_disabled（err=%v）", got, runErr)
	}

	// 开：缺 retention DSN 时 fail-closed 为 skipped，而不是借应用身份 DROP。
	registry, _ = worker.DefaultRegistry()
	deps.Settings.PartitionDropEnabled = true
	if err := bindWorkerTaskRuns(registry, deps); err != nil {
		t.Fatalf("bind enabled: %v", err)
	}
	task, _ = registry.Get("log_cleanup")
	if task.Enabled == nil || !task.Enabled() {
		t.Fatal("开关打开后谓词应为真")
	}
	result, runErr := task.Run(context.Background())
	if runErr != nil {
		t.Fatalf("开关打开但缺 retention DSN 应 fail-closed 跳过，err=%v", runErr)
	}
	if result["skipped"] == nil {
		t.Fatalf("期望 skipped 结果，实际 %+v", result)
	}
}
