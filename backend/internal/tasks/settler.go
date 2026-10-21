package tasks

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"newtg/gateway/internal/contract"
)

// 本文件是终态结算的共享接缝（W8/MJ-b）。
//
// 【为什么必须抽出来】插件轮询器与 MJ 轮询器对终态的**资金语义**是同一套：
// SUCCESS 按预扣额终结（差额 0）、FAILURE 全额退款；差别只在「谁来发现终态」
// 与「是否有任务计价表达式」。把这段逻辑留在 Poller 私有方法里，MJ 侧只能
// 复制一份——两套结算实现迟早漂移，而漂移的代价是重复扣费或免费服务。
//
// 【表达式边界】Settler 只实现「无任务表达式」的按预扣额结清/退款。插件族
// 的成功任务可能带冻结的 tiered_snapshot，必须由 Poller.settleTerminal 先
// 求值再走 cost 结算；MJ 没有 u() 表达式（relay-05 §7.1），永远走本接缝。

// BillingSettler 是 Settler 需要的计费子集（token-aware）。
//
// 与 BillingHandoff 分开是为了让 MJ 驱动只依赖「结算/退款」两件事，而不
// 被迫实现/伪造续租与成本结算方法。
type BillingSettler interface {
	TerminateByRequestIDForToken(requestID string, tokenID int64, fact contract.UsageFact) error
	RefundByRequestIDForToken(requestID string, tokenID int64) error
}

// BillingFlagStore 是结算标记的写入口（生产 *Repo）。
type BillingFlagStore interface {
	UpdateBilling(ctx context.Context, id int64, quota *int64, private map[string]any) error
}

// SettlerConfig 是 Settler 的依赖。Billing/Repo 是硬依赖：缺任一都不存在
// 「安全降级」——静默不结算只会让预扣永久冻结。
type SettlerConfig struct {
	Billing BillingSettler
	Repo    BillingFlagStore
	Logger  *slog.Logger
	Now     func() time.Time
}

// Settler 执行无表达式的终态结算，可并发使用。
type Settler struct {
	billing BillingSettler
	repo    BillingFlagStore
	log     *slog.Logger
	now     func() time.Time
}

// NewSettler 构造结算器。
func NewSettler(cfg SettlerConfig) (*Settler, error) {
	if cfg.Billing == nil || cfg.Repo == nil {
		return nil, errors.New("tasks: settler requires billing and repo")
	}
	s := &Settler{billing: cfg.Billing, repo: cfg.Repo, log: cfg.Logger, now: cfg.Now}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.now == nil {
		s.now = time.Now
	}
	return s, nil
}

// SettleTerminal 结算一个**已经由 CAS 赢得的终态**任务。
//
// 幂等：TerminateByRequestIDForToken / RefundByRequestIDForToken 都按
// request_id 幂等，重复调用只会重复写同一结论。调用方仍必须先赢终态 CAS
// ——CAS 是「恰好一次」的第一道保证，计费幂等是第二道。
//
// 失败时保留 hold 并写 settlement_pending/refund_pending + billing_pending，
// 由同族的补偿扫描（ListReconcilable）在后续轮次重试；绝不把「结算失败」
// 伪装成成功（否则 hold 永久冻结且无人知晓）。
func (s *Settler) SettleTerminal(ctx context.Context, task *Task) {
	if task == nil {
		return
	}
	requestID := toString(task.PrivateData["request_id"])
	// 令牌身份来自任务行：提交时的 PreConsume 已同事务扣减
	// api_tokens.remain_quota；不带 tokenID 时钱包退回而令牌额度永久少扣。
	tokenID := int64(privateInt(task.PrivateData["token_id"]))
	if requestID == "" {
		// 没有 requestID 就无法按幂等锚点结算。保留 billing_pending 让补偿
		// 扫描持续暴露，不猜金额。
		s.log.Error("tasks: terminal task has no request_id; settlement cannot proceed",
			"task_id", task.PublicTaskID, "state", task.State)
		return
	}
	switch task.State {
	case StateFailure:
		s.refundTerminal(ctx, task, requestID, tokenID)
	case StateSuccess:
		s.settleSuccessAtHeld(ctx, task, requestID, tokenID)
	default:
		// 非终态调用是编程错误：不静默推进，留给调用方修正。
		s.log.Error("tasks: SettleTerminal called for non-terminal task",
			"task_id", task.PublicTaskID, "state", task.State)
	}
}

// refundTerminal 全额退款（FAILURE/CANCEL 的唯一资金动作）。
func (s *Settler) refundTerminal(ctx context.Context, task *Task, requestID string, tokenID int64) {
	if err := s.billing.RefundByRequestIDForToken(requestID, tokenID); err != nil {
		// 退款失败：保留 billing_pending，并写 refund_pending 供补偿扫描重试。
		// 终态**不回退**（上游确实失败了），否则会重新进入轮询。
		s.markBilling(ctx, task, nil, map[string]any{
			"refund_pending":  true,
			"billing_pending": true,
		})
		s.log.Error("tasks: refund failed; marked refund_pending for reconciliation",
			"task_id", task.PublicTaskID, "request_id", requestID, "error", err)
		return
	}
	// 退款成功：quota 归零（设计 §9.5「成功后清 quota」），并封账。
	zero := int64(0)
	s.finishBilling(ctx, task, &zero, map[string]any{
		"billing_pending":    false,
		"refund_pending":     false,
		"settlement_pending": false,
	})
}

// settleSuccessAtHeld 以**预扣额**终结成功任务，释放 hold（差额 0）。
//
// 空 UsageFact 是计费侧冻结的「按预扣额结清」语义（billing/handoff.go）：
// 不解析定价、不按 token 重算——因此不会把成功任务退成 0。
func (s *Settler) settleSuccessAtHeld(ctx context.Context, task *Task, requestID string, tokenID int64) {
	if err := s.billing.TerminateByRequestIDForToken(requestID, tokenID, contract.UsageFact{}); err != nil {
		// 结算失败：hold 仍在，写 settlement_pending + 保留 billing_pending，
		// 补偿扫描下一轮重试。终态不回退。
		s.markBilling(ctx, task, nil, map[string]any{
			"settlement_pending": true,
			"billing_pending":    true,
		})
		s.log.Error("tasks: terminate-at-held failed; marked settlement_pending for reconciliation",
			"task_id", task.PublicTaskID, "request_id", requestID, "error", err)
		return
	}
	// 成功：quota 即预扣额（本次结算不改变金额），封账并清除待办标记。
	held := task.Quota
	s.finishBilling(ctx, task, &held, map[string]any{
		"billing_pending":    false,
		"settlement_pending": false,
	})
}

// finishBilling 在资金动作成功后落最终标记；失败只记录（幂等动作可重试）。
func (s *Settler) finishBilling(ctx context.Context, task *Task, quota *int64, extra map[string]any) {
	flags := map[string]any{
		"billing_final": true,
		// 封账成功后清除待定审计字段。它们只在 settlement_pending 期间有意
		// 义，残留会误导运维以为仍在待定。UpdateBilling 把 JSON null 解释为
		// “删除该键”。
		"settlement_pending_reason": nil,
		"settlement_pending_since":  nil,
	}
	for k, v := range extra {
		flags[k] = v
	}
	if err := s.repo.UpdateBilling(ctx, task.ID, quota, flags); err != nil {
		s.log.Error("tasks: persist settlement result failed; reconciliation will retry",
			"task_id", task.PublicTaskID, "error", err)
		return
	}
	if task.PrivateData != nil {
		for k, v := range flags {
			task.PrivateData[k] = v
		}
	}
}

// markBilling 写待办标记；失败只记录（billing_pending 仍在，下一轮还会扫到）。
func (s *Settler) markBilling(ctx context.Context, task *Task, quota *int64, flags map[string]any) {
	if err := s.repo.UpdateBilling(ctx, task.ID, quota, flags); err != nil {
		s.log.Error("tasks: persist billing pending flags failed", "task_id", task.PublicTaskID, "error", err)
	}
}
