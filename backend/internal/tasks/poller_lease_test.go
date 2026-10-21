package tasks

// W9-B2（R-W9B-02）的任务轮询续租验收。
//
// 设计约束：任务提交后会话 Detach，流式心跳停止；轮询器成为任务的守护者。
// 单轮可能处理上千个任务、任务间隔 1s，整轮远超租约 TTL，因此必须在
// 轮批开始与每个任务处理前续租。否则兜底扫描会把仍在排队的活跃任务当成
// 崩溃请求退款，任务随后成功即免费（G-DOM-35）。

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// leaseEventHandoff 在 fakeHandoff 之上把续租调用写入共享事件流，用于断言
// 「批量续租 → 轮内长间隔 → 逐任务续租」的相对顺序。
type leaseEventHandoff struct {
	fakeHandoff
	mu     *sync.Mutex
	events *[]string
}

func (h *leaseEventHandoff) RefreshInflightLeases(_ context.Context, requestIDs []string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	*h.events = append(*h.events, "refresh:"+strings.Join(requestIDs, ","))
	return nil
}

// TestPollerRenewsLeaseForBatchAndEachTaskAcrossLongRound 覆盖单轮持续时间
// 远超租约 TTL 的场景：轮批开头先批量续租，轮内长间隔之后每个任务处理前
// 再各自续租（排队靠后的任务不会因等待而租约过期）。
func TestPollerRenewsLeaseForBatchAndEachTaskAcrossLongRound(t *testing.T) {
	repo := newFakePollRepo()
	// 渠道 999 不存在：processTask 会在续租之后、查询渠道时失败并累计失败；
	// 本用例只关心续租调用，不关心任务是否推进。
	repo.add(submittedTask(1, 999))
	repo.add(submittedTask(2, 999))

	var (
		mu     sync.Mutex
		events []string
	)
	handoff := &leaseEventHandoff{mu: &mu, events: &events}

	// TaskGap 取 30 分钟：模拟「单轮 > 300s」的真实量级。Sleep 被注入为
	// 立即返回，但在事件流里留下标记，用来证明逐任务续租确实发生在长间隔
	// 之后（只在轮批开头续租的实现会缺失这两条事件）。
	p, err := NewPoller(PollerConfig{
		Repo: repo, Plugins: newTestSnapshotter(t), Billing: handoff,
		Client: NewOutboundClient(), AllowPrivate: true,
		TaskGap: 30 * time.Minute,
		Sleep: func(context.Context, time.Duration) error {
			mu.Lock()
			events = append(events, "sleep")
			mu.Unlock()
			return nil
		},
		Now: func() time.Time { return time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	p.RunOnce(context.Background())

	mu.Lock()
	got := append([]string(nil), events...)
	mu.Unlock()
	want := []string{
		"refresh:req_1,req_2", // 轮批开始：整批续租
		"refresh:req_1",       // 任务 1 处理前逐任务续租
		"sleep",               // 单轮内 30m 的任务间隔（> 任何租约 TTL）
		"refresh:req_2",       // 任务 2 在长间隔之后处理前再次续租
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("续租事件序 = %v，want %v", got, want)
	}

	// 续租是纯副作用：任务仍按既有失败累计语义推进，未被续租改变。
	if task := repo.get(1); task.State != StateSubmitted {
		t.Fatalf("task 1 state = %q，want submitted（续租不得改变状态）", task.State)
	}
}

// TestPollerRenewalFailureDoesNotChangeSettlementDirection 覆盖续租失败：
// 只记录，不阻断轮询，也不触发任何结算/退款。
func TestPollerRenewalFailureDoesNotChangeSettlementDirection(t *testing.T) {
	repo := newFakePollRepo()
	task := repo.add(submittedTask(1, 999))
	handoff := &fakeHandoff{refreshErr: errors.New("lease store unavailable")}
	p := newTestPoller(t, repo, handoff, NewOutboundClient())

	p.RunOnce(context.Background())

	if got := handoff.terminateCount(); got != 0 {
		t.Fatalf("续租失败触发了 %d 次结算，want 0", got)
	}
	if got := handoff.refundCount(); got != 0 {
		t.Fatalf("续租失败触发了 %d 次退款，want 0", got)
	}
	// 任务继续按轮询失败累计（证明确实处理了，而不是被续租错误中断）。
	after := repo.get(task.ID)
	if failures := privateInt(after.PrivateData["poll_failures"]); failures != 1 {
		t.Fatalf("poll_failures = %d，want 1（轮询未被续租失败阻断）", failures)
	}
}

// TestMJPollerDoesNotRenewOrChangeState 覆盖 R-MJ-04 的族隔离：MJ poller
// 在 A0 只领取，不续租、不改状态。若它误改状态（或未来误接计费），会与
// 插件族的租约/结算语义冲突。
func TestMJPollerDoesNotRenewOrChangeState(t *testing.T) {
	repo := newFakePollRepo()
	task := repo.add(mjSubmittedTask(1, 999))

	p, err := NewMJPoller(MJPollerConfig{Repo: repo})
	if err != nil {
		t.Fatalf("NewMJPoller: %v", err)
	}
	p.RunOnce(context.Background())

	after := repo.get(task.ID)
	if after.State != StateSubmitted {
		t.Fatalf("MJ poller 把状态改为 %q，want submitted", after.State)
	}
	if after.RawState != task.RawState || after.ProgressPercent != task.ProgressPercent {
		t.Fatalf("MJ poller 改动了只读字段：%+v", after)
	}
	if len(after.PrivateData) != len(task.PrivateData) {
		t.Fatalf("MJ poller 改动了 private_data：%+v", after.PrivateData)
	}
	if len(repo.casLog) != 0 {
		t.Fatalf("MJ poller 产生了 CAS 写入：%v", repo.casLog)
	}
	if fams := repo.requestedFamilies(); len(fams) != 1 || fams[0] != FamilyMJ {
		t.Fatalf("MJ poller 领取 family = %v，want [mj]", fams)
	}
}
