package worker

// 注册表对 model_perf_test 的静态契约。定时性能实测的周期由
// modelperf.RunLoop 自己持有（不经过本包的调度器），因此这里只钉住三件事：
// 任务必须登记、不得被本包周期执行、并且允许管理员手动触发一轮。

import (
	"strings"
	"testing"
	"time"
)

func TestDefaultRegistrySchedulesModelPerfTest(t *testing.T) {
	registry, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	task, ok := registry.Get("model_perf_test")
	if !ok {
		t.Fatal("model_perf_test must be registered")
	}
	if task.Scheduled {
		t.Fatal("model_perf_test is scheduled by modelperf.RunLoop, not the task scheduler")
	}
	if !task.ManualTriggerAllowed {
		t.Fatal("administrators must be able to trigger a round manually")
	}
	// 一轮要覆盖全部公开对话模型，默认 10 分钟会把长轮次截断在租约里。
	if task.Timeout < time.Hour {
		t.Fatal("a full round can exceed the 10m default timeout")
	}
	// 目录里的 Interval 只是出厂展示默认值：EnsureTaskRows 每次 worker 启动都会
	// 把它 UPSERT 进 worker_tasks.interval_seconds，真实节奏由性能页的
	// perf.schedule.* 决定。文案必须说清真源，否则管理员会以为改这个常量
	// 或者等 12 小时就能调整周期。
	if !strings.Contains(task.Description, "performance settings page") {
		t.Fatalf("Description = %q, must state that the cadence follows the performance settings page", task.Description)
	}
}
