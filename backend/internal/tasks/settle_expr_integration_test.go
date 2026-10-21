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

// snapshotFor 构造与提交期冻结同形状的快照。
func snapshotFor(t *testing.T, expr string, group float64) map[string]any {
	t.Helper()
	compiled, err := pricing.CompileTaskExpr(expr)
	if err != nil {
		t.Fatalf("compile task expr: %v", err)
	}
	return FreezeTieredSnapshot(compiled, pricing.ExprSourcePluginClient, group)
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
	// 表达式引用插件未上报的字段（schema 声明了 seconds，但 data 只有 clips）。
	task := repo.add(successTaskWithSnapshot(t,
		snapshotFor(t, `u("clips") * 0.5`, 1), map[string]any{"clips": float64(10)}))
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

// 编译期断言：fakeHandoff 必须实现 T5 扩展后的交接面。
var _ BillingHandoff = (*fakeHandoff)(nil)
