package platform

// 测速调度装配的接缝测试（spec 2026-10-27 §5.4 / admin-performance-redesign Task 7）。
//
// 【能测什么、不能测什么】`perf.schedule.*` 的落库值只能经 configcenter.Center
// 的快照读出，而 Center 没有对外的快照写入接口（只有 New/Load/Save/Refresh，
// 后两者都要真库与 Redis）。因此这里用「空配置中心」表达「四个键都没落库」，
// 覆盖 env 默认值、nil 中心与 `PERF_SCHEDULE_ENABLED=false` 三条路径；「已落库
// 的 interval_hours 走间隔时刻表」由 internal/perfsettings 自己的单测覆盖
// （那里有 newWithReader 测试接缝），本文件不再重复构造。
//
// 装配只依赖构造期参数，调度器在拿到 Models 之前不会触库，因此 db/rdb 传 nil
// 是安全的：这些用例只观察时刻表与生效设置，不跑轮次。

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"newtg/gateway/internal/config"
	"newtg/gateway/internal/configcenter"
	"newtg/gateway/internal/perfsettings"
)

// newPerfTestConfig 返回一份只填测速相关字段的配置。时刻表刻意选 03:00/15:00：
// 与 perfsettings 的兜底 00:00/12:00 不同，NextDue 因此能证明时刻表真的来自
// env 默认值，而不是硬编码或兜底常量。
func newPerfTestConfig(enabled bool) *config.Config {
	return &config.Config{
		PerfScheduleEnabled:     enabled,
		PerfScheduleTimes:       "03:00,15:00",
		PerfScheduleTimezone:    "UTC",
		PerfScheduleRuns:        4,
		PerfScheduleConcurrency: 5,
	}
}

// TestNewModelPerfSchedulerEnvDefaults 钉住两个升级安全属性：
//   - 四个 perf.schedule.* 键都没落库时，生效值逐项等于 PERF_SCHEDULE_*；
//   - 空配置中心与 nil 中心行为一致（未接配置中心的装配不得 panic）。
func TestNewModelPerfSchedulerEnvDefaults(t *testing.T) {
	now := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)

	tests := []struct {
		name   string
		center *configcenter.Center
	}{
		{name: "empty center", center: configcenter.New(nil, nil)},
		{name: "nil center", center: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, enabled := range []bool{false, true} {
				cfg := newPerfTestConfig(enabled)
				scheduler, reader, err := newModelPerfScheduler(cfg, nil, nil, tc.center, slog.Default())
				if err != nil {
					t.Fatalf("newModelPerfScheduler: %v", err)
				}
				if scheduler == nil || reader == nil {
					t.Fatal("newModelPerfScheduler must return both the scheduler and the settings reader")
				}

				settings := reader.Settings(now)
				if settings.Enabled != enabled {
					t.Fatalf("Settings().Enabled = %v, want %v (PERF_SCHEDULE_ENABLED must remain the default)",
						settings.Enabled, enabled)
				}
				if settings.Source != perfsettings.SourceEnv {
					t.Fatalf("Settings().Source = %q, want %q when nothing is persisted",
						settings.Source, perfsettings.SourceEnv)
				}
				if settings.Runs != cfg.PerfScheduleRuns || settings.Concurrency != cfg.PerfScheduleConcurrency {
					t.Fatalf("Settings() runs/concurrency = %d/%d, want %d/%d",
						settings.Runs, settings.Concurrency,
						cfg.PerfScheduleRuns, cfg.PerfScheduleConcurrency)
				}
				if settings.IntervalHours != 0 {
					t.Fatalf("Settings().IntervalHours = %d, want 0 (legacy times mode)",
						settings.IntervalHours)
				}
				if settings.Timezone != cfg.PerfScheduleTimezone || settings.LegacyTimes != cfg.PerfScheduleTimes {
					t.Fatalf("Settings() timezone/times = %q/%q, want %q/%q",
						settings.Timezone, settings.LegacyTimes,
						cfg.PerfScheduleTimezone, cfg.PerfScheduleTimes)
				}

				// ScheduleFor 必须委托给 reader：下一次触发点由 env 时刻表决定。
				want := time.Date(2026, 10, 28, 15, 0, 0, 0, time.UTC)
				if got := scheduler.NextDue(now); !got.Equal(want) {
					t.Fatalf("NextDue = %s, want %s (env times 03:00/15:00)", got, want)
				}
			}
		})
	}
}

// TestStartModelPerfLoopStartsWhenSchedulingDisabled 钉住运行期开关语义：
// 循环必须无条件启动，启停由每 tick 读取的 Settings().Enabled 判定。若沿用
// 启动期的 PERF_SCHEDULE_ENABLED 短路 goroutine，页面上打开开关就必须重启
// worker 才生效。
func TestStartModelPerfLoopStartsWhenSchedulingDisabled(t *testing.T) {
	cfg := newPerfTestConfig(false)
	handler := &captureLogHandler{}
	log := slog.New(handler)

	scheduler, reader, err := newModelPerfScheduler(cfg, nil, nil, configcenter.New(nil, nil), log)
	if err != nil {
		t.Fatalf("newModelPerfScheduler: %v", err)
	}
	if reader.Settings(time.Now()).Enabled {
		t.Fatal("test premise broken: scheduling must be disabled by the env default")
	}

	stop := startModelPerfLoop(context.Background(), scheduler, log, nil)
	defer stop()

	// RunLoop 启动即记录 "modelperf: scheduler started"；等它出现即可证明
	// goroutine 真的起来了，不必等 30s 的第一个 tick。
	deadline := time.Now().Add(2 * time.Second)
	for {
		for _, record := range handler.snapshot() {
			if strings.Contains(record.Message, "scheduler started") {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("model perf loop did not start although scheduling is disabled at runtime only")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestStartModelPerfLoopNilSchedulerIsNoop 钉住装配失败时的降级：返回可直接
// 调用的 no-op，停机序列不必再判空，也不能阻塞。
func TestStartModelPerfLoopNilSchedulerIsNoop(t *testing.T) {
	stop := startModelPerfLoop(context.Background(), nil, slog.Default(), nil)
	if stop == nil {
		t.Fatal("stop function is nil")
	}
	done := make(chan struct{})
	go func() {
		stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stop returned by the nil-scheduler path blocked")
	}
}
