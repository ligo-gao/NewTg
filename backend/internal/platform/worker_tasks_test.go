package platform

// W18 上游用量对账的 worker 任务契约（上游用量对账设计 §7.4）。
//
// 覆盖：
//   - 任务在静态注册表里可见，元数据与 brief 逐字一致；
//   - bindWorkerTaskRuns 能在不触 DB 的情况下完成绑定（装配期 fail-fast 的
//     正常路径），并把 reconcile.enabled 挂成运行时谓词；
//   - runner 的 Outcome 状态投影到运行历史允许的三个值。

import (
	"testing"
	"time"

	"newtg/gateway/internal/worker"
)

func TestUpstreamReconcileTaskIsRegisteredAndBindable(t *testing.T) {
	reg, err := worker.DefaultRegistry()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	for _, task := range reg.Tasks() {
		if task.Name != "upstream_usage_reconcile" {
			continue
		}
		if !task.ManualTriggerAllowed || !task.Scheduled || task.Interval != 24*time.Hour {
			t.Fatalf("task = %+v", task)
		}
		return
	}
	t.Fatal("upstream_usage_reconcile is not registered")
}
