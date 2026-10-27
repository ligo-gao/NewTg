package platform

// W17-2 注册表的静态契约（无外部依赖，随 `go test ./internal/platform` 运行）。
//
// 覆盖：
//   - 同名任务启动期失败；
//   - 白名单恰好是 billing-06 §13.5.1 冻结的 5 个任务 + spec §5.2.1 要求的
//     holiday_sync（手工同步节假日日历）+ 上游对账补跑 upstream_usage_reconcile；
//   - 只有绑定 Run 的任务才能被手动触发（否则触发会留下永不执行的 pending 行）。

import (
	"context"
	"errors"
	"testing"
	"time"

	"newtg/gateway/internal/holiday"
	"newtg/gateway/internal/modelperf"
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
		// spec §5.2.1：手工同步中国节假日日历（整年替换，幂等）。
		"holiday_sync": true,
		// 上游对账补跑：每次生成新 run，历史 run 不可变，幂等。
		"upstream_usage_reconcile": true,
		// spec 2026-10-27 §5.5：定时性能实测需在管理面可见且可手动触发一轮。
		"model_perf_test": true,
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

// TestHolidaySyncYearSelection 锁定「12 月追加下一年」与「按中国日历取今年」：
// 用 UTC 表达跨年瞬间，确认年份边界按 +08:00 判定。
func TestHolidaySyncYearSelection(t *testing.T) {
	cases := []struct {
		name string
		now  time.Time
		want []int
	}{
		{"mid-year", time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC), []int{2026}},
		{"december prep next year", time.Date(2026, 12, 1, 3, 30, 0, 0, holidaySyncLocation), []int{2026, 2027}},
		// 2026-12-31 20:00 UTC = 2027-01-01 04:00 +08：已经是中国的新年。
		{"utc new year's eve", time.Date(2026, 12, 31, 20, 0, 0, 0, time.UTC), []int{2027}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := holidaySyncYears(tc.now)
			if len(got) != len(tc.want) {
				t.Fatalf("years = %v，期望 %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("years = %v，期望 %v", got, tc.want)
				}
			}
		})
	}
}

func TestCountHolidayDays(t *testing.T) {
	off, makeup := countHolidayDays([]holiday.Day{
		{Date: "2026-10-01", OffDay: true},
		{Date: "2026-10-02", OffDay: true},
		{Date: "2026-10-10", OffDay: false},
	})
	if off != 2 || makeup != 1 {
		t.Fatalf("off/makeup = %d/%d，期望 2/1", off, makeup)
	}
}

func TestHolidaySyncDisabledWithoutConfig(t *testing.T) {
	registry, err := worker.DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	if err := bindWorkerTaskRuns(registry, workerTaskDeps{}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	task, ok := registry.Get("holiday_sync")
	if !ok {
		t.Fatal("holiday_sync 未注册")
	}
	// 装配缺配置时按关闭处理：不得在没有开关值的情况下发出站请求。
	if task.Enabled == nil || task.Enabled() {
		t.Fatal("无配置时 holiday_sync 应视为关闭")
	}
}

// TestModelPerfTaskUnavailableWithoutRun 锁定装配降级：没有 RunOnce 时任务
// 仍出现在目录里，但谓词为假（管理面显示不可用、手动触发 409），并且不绑定
// 执行体——否则会留下一条永远不会成功的 pending 行。
func TestModelPerfTaskUnavailableWithoutRun(t *testing.T) {
	registry, err := worker.DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	if err := bindWorkerTaskRuns(registry, workerTaskDeps{}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	task, ok := registry.Get("model_perf_test")
	if !ok {
		t.Fatal("model_perf_test 未注册")
	}
	if task.Run != nil {
		t.Fatal("没有 RunOnce 时不应绑定执行体")
	}
	if task.Enabled == nil || task.Enabled() {
		t.Fatal("没有 RunOnce 时 model_perf_test 应视为不可用")
	}
	if task.BlockedCode != "model_perf_test_unavailable" {
		t.Fatalf("BlockedCode = %q，期望 model_perf_test_unavailable", task.BlockedCode)
	}
}

// TestModelPerfTaskBindsRoundStats 锁定手动触发的运行结果只暴露四个计数键
// （运行历史不得写入模型名或上游响应体）。
func TestModelPerfTaskBindsRoundStats(t *testing.T) {
	registry, err := worker.DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	deps := workerTaskDeps{
		ModelPerfRun: func(context.Context) (modelperf.Stats, error) {
			return modelperf.Stats{Models: 7, Tested: 5, SkippedNoChannel: 1, PersistFailed: 1}, nil
		},
	}
	if err := bindWorkerTaskRuns(registry, deps); err != nil {
		t.Fatalf("bind: %v", err)
	}
	task, _ := registry.Get("model_perf_test")
	if task.Enabled == nil || !task.Enabled() {
		t.Fatal("绑定 RunOnce 后 model_perf_test 应为可用")
	}
	result, err := task.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := map[string]int{
		"models": 7, "tested": 5, "skipped_no_channel": 1, "persist_failed": 1,
	}
	if len(result) != len(want) {
		t.Fatalf("运行结果键集 = %v，期望恰好 %d 个计数", result, len(want))
	}
	for key, value := range want {
		if result[key] != value {
			t.Fatalf("result[%q] = %v，期望 %d", key, result[key], value)
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
