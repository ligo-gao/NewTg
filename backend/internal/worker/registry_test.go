package worker

// 注册表对 model_perf_test 的静态契约。定时性能实测的周期由
// modelperf.RunLoop 自己持有（不经过本包的调度器），因此这里只钉住三件事：
// 任务必须登记、不得被本包周期执行、并且允许管理员手动触发一轮。

import (
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
}
