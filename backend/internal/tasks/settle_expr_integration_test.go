//go:build integration

package tasks

import (
	"context"
	"errors"
	"testing"

	"newtg/gateway/internal/billing/pricing"
)

// 本文件验收 T5 的任务侧差额结算：冻结表达式成功求值 → 带真实成本调用
// Terminate；求值失败 → 保留预扣并置 settlement_pending；无快照 → 保持
// 既有的「按预扣额结清」语义。
//
// 运行（集成门禁，-p 1）：
//
//	NEWTG_TEST_DB_DSN=postgres://... NEWTG_TEST_LOGS_DSN=... \
//	NEWTG_TEST_REDIS_ADDR=... go test -tags=integration -p 1 ./internal/tasks/
//
// 本文件本身只用内存 fake（真实验证在计费引擎的 hold CAS 测试里），
// 因此不依赖 PG；放在集成门禁里是为了与 T5 的验收命令一致。

// snapshotFor 构造与提交期冻结同形状的快照（单位来自当前插件 manifest）。
func snapshotFor(t *testing.T, expr string, group float64) map[string]any {
	t.Helper()
	compiled, err := pricing.CompileTaskExpr(expr)
	if err != nil {
		t.Fatalf("compile task expr: %v", err)
	}
	return FreezeTieredSnapshot(compiled, pricing.ExprSourcePluginClient, group,
		compilePollPlugin(t).Manifest())
}

func successTaskWithSnapshot(t *testing.T, snapshot map[string]any, usage map[string]any) *Task {
	t.Helper()
	task := submittedTask(1, 1)
	task.State = StateSuccess
	task.Quota = 1000
	task.Data = map[string]any{"usage": usage}
	if snapshot != nil {
		task.PrivateData[PrivateTieredSnapshot] = snapshot
	}
	return task
}

// TestSettleExprDifferenceSettlement 验证 hold=1000、表达式求值 700 → 以 700
// 终结（多退 300 由计费侧在 hold 上完成），任务额度落最终成本。
func TestSettleExprDifferenceSettlement(t *testing.T) {
	prev := pricing.CurrentQuotaPerUnit()
	if err := pricing.SetQuotaPerUnit(100); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pricing.SetQuotaPerUnit(prev) })

	repo := newFakePollRepo()
	// u("seconds") * 0.7，seconds=10 → $7 → 700 积分。
	task := repo.add(successTaskWithSnapshot(t,
		snapshotFor(t, `u("seconds") * 0.7`, 1), map[string]any{"seconds": float64(10)}))
	handoff := &fakeHandoff{}
	p := newTestPoller(t, repo, handoff, nil)

	p.settleTerminal(context.Background(), task, compilePollPlugin(t))

	if len(handoff.costs) != 1 || handoff.costs[0] != 700 {
		t.Fatalf("costs=%v, want [700]", handoff.costs)
	}
	got := repo.get(task.ID)
	if got.Quota != 700 {
		t.Fatalf("quota=%d, want 700", got.Quota)
	}
	if !boolFlag(got.PrivateData["billing_final"]) || boolFlag(got.PrivateData["settlement_pending"]) {
		t.Fatalf("success settlement flags wrong: %#v", got.PrivateData)
	}
}

// TestSettleExprErrorKeepsHoldAndMarksPending 验证 P-07：求值失败保留预扣、
// 不退款、置 settlement_pending。
func TestSettleExprErrorKeepsHoldAndMarksPending(t *testing.T) {
	repo := newFakePollRepo()
	// 表达式引用的字段在任务 data 里不存在（快照冻结了单位但上游没报数）。
	task := repo.add(successTaskWithSnapshot(t,
		snapshotFor(t, `u("missing") * 0.5`, 1), map[string]any{"clips": float64(10)}))
	handoff := &fakeHandoff{}
	p := newTestPoller(t, repo, handoff, nil)

	p.settleTerminal(context.Background(), task, compilePollPlugin(t))

	if len(handoff.costs) != 0 || handoff.refundCount() != 0 {
		t.Fatalf("eval failure must neither settle nor refund: costs=%v refunds=%d",
			handoff.costs, handoff.refundCount())
	}
	got := repo.get(task.ID)
	if got.Quota != 1000 {
		t.Fatalf("hold must be kept: quota=%d", got.Quota)
	}
	if !boolFlag(got.PrivateData["settlement_pending"]) || !boolFlag(got.PrivateData["billing_pending"]) {
		t.Fatalf("settlement_pending/billing_pending missing: %#v", got.PrivateData)
	}
	if boolFlag(got.PrivateData["billing_final"]) {
		t.Fatalf("billing_final must stay false on eval failure: %#v", got.PrivateData)
	}
}

// TestSettleWithoutSnapshotKeepsHeldSemantics 验证存量任务（无冻结表达式）
// 仍按预扣额结清，兼容性不被 T5 改变。
func TestSettleWithoutSnapshotKeepsHeldSemantics(t *testing.T) {
	repo := newFakePollRepo()
	task := repo.add(successTaskWithSnapshot(t, nil, map[string]any{"seconds": float64(10)}))
	handoff := &fakeHandoff{}
	p := newTestPoller(t, repo, handoff, nil)

	p.settleTerminal(context.Background(), task, compilePollPlugin(t))

	if len(handoff.costs) != 1 || handoff.costs[0] != -1 {
		t.Fatalf("expected at-held settlement (cost sentinel -1), got %v", handoff.costs)
	}
	got := repo.get(task.ID)
	if got.Quota != 1000 || !boolFlag(got.PrivateData["billing_final"]) {
		t.Fatalf("held settlement changed semantics: quota=%d private=%#v", got.Quota, got.PrivateData)
	}
}

// TestSettleExprTerminateErrorKeepsPending 验证资金动作失败也保留预扣并等待重试。
func TestSettleExprTerminateErrorKeepsPending(t *testing.T) {
	repo := newFakePollRepo()
	task := repo.add(successTaskWithSnapshot(t,
		snapshotFor(t, `u("seconds") * 0.7`, 1), map[string]any{"seconds": float64(10)}))
	handoff := &fakeHandoff{costErr: errors.New("db down")}
	p := newTestPoller(t, repo, handoff, nil)

	p.settleTerminal(context.Background(), task, compilePollPlugin(t))

	got := repo.get(task.ID)
	if !boolFlag(got.PrivateData["settlement_pending"]) || boolFlag(got.PrivateData["billing_final"]) {
		t.Fatalf("failed terminate must mark pending: %#v", got.PrivateData)
	}
	if got.Quota != 1000 {
		t.Fatalf("hold must be kept: quota=%d", got.Quota)
	}
}

// TestSettleUsesFrozenQuotaPerUnit 覆盖复核 M1：快照冻结的 QPU 必须优先于
// 结算时进程当前的 QPU（变异：删掉 EvalTaskCost 的 WithQuotaPerUnit 会变红）。
func TestSettleUsesFrozenQuotaPerUnit(t *testing.T) {
	prev := pricing.CurrentQuotaPerUnit()
	// 进程当前 QPU=100；快照把提交期的 QPU 改成 200。
	if err := pricing.SetQuotaPerUnit(100); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pricing.SetQuotaPerUnit(prev) })

	repo := newFakePollRepo()
	snapshot := snapshotFor(t, `u("seconds") * 0.7`, 1)
	snapshot["quota_per_unit"] = float64(200)
	task := repo.add(successTaskWithSnapshot(t, snapshot,
		map[string]any{"seconds": float64(10)}))
	handoff := &fakeHandoff{}
	p := newTestPoller(t, repo, handoff, nil)

	p.settleTerminal(context.Background(), task, compilePollPlugin(t))

	// 10 秒 × $0.7 × 200 = 1400；若误用进程 QPU=100 则会是 700。
	if len(handoff.costs) != 1 || handoff.costs[0] != 1400 {
		t.Fatalf("costs=%v, want [1400] from frozen QPU", handoff.costs)
	}
}

// TestSettleUsesFrozenUnitsAfterPluginRename 覆盖复核 I1：插件在任务提交后
// 改名/下架，结算仍按快照冻结的「字段 → 单位」求值，不因 live manifest
// 缺失而永久卡在 settlement_pending。
func TestSettleUsesFrozenUnitsAfterPluginRename(t *testing.T) {
	prev := pricing.CurrentQuotaPerUnit()
	if err := pricing.SetQuotaPerUnit(100); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pricing.SetQuotaPerUnit(prev) })

	repo := newFakePollRepo()
	task := successTaskWithSnapshot(t,
		snapshotFor(t, `u("seconds") * 0.7`, 1), map[string]any{"seconds": float64(10)})
	task.PluginKey = "renamed-plugin-after-submit"
	task = repo.add(task)
	handoff := &fakeHandoff{}
	p := newTestPoller(t, repo, handoff, nil)

	// compiled=nil：模拟补偿扫描/重启后只能按 PluginKey 查快照（查不到）。
	p.settleTerminal(context.Background(), task, nil)

	if len(handoff.costs) != 1 || handoff.costs[0] != 700 {
		t.Fatalf("costs=%v, want [700] from frozen units", handoff.costs)
	}
	got := repo.get(task.ID)
	if got.Quota != 700 || !boolFlag(got.PrivateData["billing_final"]) {
		t.Fatalf("settlement did not complete: quota=%d private=%#v", got.Quota, got.PrivateData)
	}
}

// TestSettleCorruptSnapshotFailsClosed 覆盖复核 M2：快照存在但 expr 为空是
// 数据损坏，必须报错保留预扣，不能静默按 held 结清。
func TestSettleCorruptSnapshotFailsClosed(t *testing.T) {
	repo := newFakePollRepo()
	task := successTaskWithSnapshot(t, map[string]any{
		"mode": "tiered_expr", "expr": "   ",
	}, map[string]any{"seconds": float64(10)})
	task = repo.add(task)
	handoff := &fakeHandoff{}
	p := newTestPoller(t, repo, handoff, nil)

	p.settleTerminal(context.Background(), task, compilePollPlugin(t))

	if len(handoff.costs) != 0 || handoff.refundCount() != 0 {
		t.Fatalf("corrupt snapshot must neither settle nor refund: costs=%v refunds=%d",
			handoff.costs, handoff.refundCount())
	}
	got := repo.get(task.ID)
	if !boolFlag(got.PrivateData["settlement_pending"]) || got.Quota != 1000 {
		t.Fatalf("corrupt snapshot must keep hold and mark pending: %#v", got.PrivateData)
	}
}

// 编译期断言：fakeHandoff 必须实现 T5 扩展后的交接面。
var _ BillingHandoff = (*fakeHandoff)(nil)
