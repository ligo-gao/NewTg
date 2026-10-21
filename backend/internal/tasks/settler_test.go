package tasks

// W8/MJ-b 的终态结算接缝验收（无任务表达式的按预扣额结清 / 失败退款）。

import (
	"context"
	"errors"
	"testing"
)

func newSettlerFixture(t *testing.T) (*Settler, *fakePollRepo, *fakeHandoff) {
	t.Helper()
	repo := newFakePollRepo()
	handoff := &fakeHandoff{}
	settler, err := NewSettler(SettlerConfig{Billing: handoff, Repo: repo})
	if err != nil {
		t.Fatalf("NewSettler: %v", err)
	}
	return settler, repo, handoff
}

func mjSettlerTask(state State) *Task {
	return &Task{
		ID: 1, PublicTaskID: "task_settle", Family: FamilyMJ,
		State: state, Quota: 50,
		PrivateData: map[string]any{
			"request_id": "req_settle",
			"token_id":   float64(7),
		},
	}
}

func TestSettlerSuccessTerminatesAtHeld(t *testing.T) {
	settler, repo, handoff := newSettlerFixture(t)
	task := repo.add(mjSettlerTask(StateSuccess))
	live := repo.get(task.ID)

	settler.SettleTerminal(context.Background(), live)

	if handoff.terminateCount() != 1 || handoff.refundCount() != 0 {
		t.Fatalf("billing calls terminate=%d refund=%d, want 1/0", handoff.terminateCount(), handoff.refundCount())
	}
	if handoff.terminates[0] != "req_settle" || handoff.terminateTokens[0] != 7 {
		t.Fatalf("terminate request/token = %q/%d", handoff.terminates[0], handoff.terminateTokens[0])
	}
	if handoff.costs[0] != -1 {
		t.Fatalf("success settlement must use at-held (empty fact), cost marker = %d", handoff.costs[0])
	}
	got := repo.get(task.ID)
	if got.Quota != 50 {
		t.Fatalf("at-held settlement changed quota to %d, want 50", got.Quota)
	}
	if got.PrivateData["billing_final"] != true || got.PrivateData["billing_pending"] != false {
		t.Fatalf("billing flags = %#v, want billing_final", got.PrivateData)
	}
}

func TestSettlerFailureRefundsAndZeroesQuota(t *testing.T) {
	settler, repo, handoff := newSettlerFixture(t)
	task := repo.add(mjSettlerTask(StateFailure))
	live := repo.get(task.ID)

	settler.SettleTerminal(context.Background(), live)

	if handoff.refundCount() != 1 || handoff.terminateCount() != 0 {
		t.Fatalf("billing calls refund=%d terminate=%d, want 1/0", handoff.refundCount(), handoff.terminateCount())
	}
	if handoff.refunds[0] != "req_settle" || handoff.refundTokens[0] != 7 {
		t.Fatalf("refund request/token = %q/%d", handoff.refunds[0], handoff.refundTokens[0])
	}
	got := repo.get(task.ID)
	if got.Quota != 0 {
		t.Fatalf("refunded task quota = %d, want 0", got.Quota)
	}
	if got.PrivateData["billing_final"] != true {
		t.Fatalf("billing flags = %#v, want billing_final", got.PrivateData)
	}
}

func TestSettlerFailureKeepsHoldAndMarksPending(t *testing.T) {
	t.Run("terminate error", func(t *testing.T) {
		settler, repo, handoff := newSettlerFixture(t)
		handoff.terminateErr = errors.New("ledger unavailable")
		task := repo.add(mjSettlerTask(StateSuccess))
		settler.SettleTerminal(context.Background(), repo.get(task.ID))
		got := repo.get(task.ID)
		if got.PrivateData["settlement_pending"] != true || got.PrivateData["billing_pending"] != true {
			t.Fatalf("flags = %#v, want settlement_pending+billing_pending", got.PrivateData)
		}
		if got.Quota != 50 {
			t.Fatalf("settlement failure must keep the hold, quota = %d", got.Quota)
		}
	})
	t.Run("refund error", func(t *testing.T) {
		settler, repo, handoff := newSettlerFixture(t)
		handoff.refundErr = errors.New("wallet unavailable")
		task := repo.add(mjSettlerTask(StateFailure))
		settler.SettleTerminal(context.Background(), repo.get(task.ID))
		got := repo.get(task.ID)
		if got.PrivateData["refund_pending"] != true || got.PrivateData["billing_pending"] != true {
			t.Fatalf("flags = %#v, want refund_pending+billing_pending", got.PrivateData)
		}
		if got.Quota != 50 {
			t.Fatalf("failed refund must keep the original quota, got %d", got.Quota)
		}
	})
}

func TestSettlerWithoutRequestIDDoesNotMoveMoney(t *testing.T) {
	settler, repo, handoff := newSettlerFixture(t)
	task := mjSettlerTask(StateSuccess)
	task.PrivateData = map[string]any{}
	repo.add(task)

	settler.SettleTerminal(context.Background(), repo.get(task.ID))

	if handoff.terminateCount() != 0 || handoff.refundCount() != 0 {
		t.Fatalf("missing request_id must not settle/refund")
	}
}

func TestSettlerIgnoresNonTerminalTask(t *testing.T) {
	settler, repo, handoff := newSettlerFixture(t)
	task := repo.add(mjSettlerTask(StateInProgress))
	settler.SettleTerminal(context.Background(), repo.get(task.ID))
	if handoff.terminateCount() != 0 || handoff.refundCount() != 0 {
		t.Fatalf("non-terminal task must not be settled")
	}
}

func TestSettlerUsesTaskTokenForTokenAwareSettlement(t *testing.T) {
	// token_id 从 jsonb 解出是 float64；结算必须把它还原成 int64 传给计费，
	// 否则令牌额度不会回补（T6b 复核 H1）。
	settler, repo, handoff := newSettlerFixture(t)
	task := mjSettlerTask(StateSuccess)
	task.PrivateData["token_id"] = float64(99)
	repo.add(task)
	settler.SettleTerminal(context.Background(), repo.get(task.ID))
	if handoff.terminateTokens[0] != 99 {
		t.Fatalf("token id = %d, want 99", handoff.terminateTokens[0])
	}
}
