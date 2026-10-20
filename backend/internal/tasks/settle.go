package tasks

import (
	"context"
	"strconv"

	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/plugin"
)

// 本文件是终态结算与最小补偿扫描（relay-06 §9.4/§9.5）。
//
// 本批的计价事实源只有一处：计费引擎在提交期冻结的预扣（hold）。任务侧的
// **计价表达式配置面**（`billing_setting.plugin_billing_expr["<pluginKey>::<model>"]`）
// 在仓库中尚不存在，brief 明确禁止在 T6b 新建第二套。因此：
//
//   - FAILURE：全额退款（RefundByRequestID）。
//   - SUCCESS：按预扣额终结——`TerminateByRequestID(requestID, UsageFact{})`
//     以 hold 为最终成本（差额 0）释放预扣并写成功日志。
//
// 为什么用空 fact 而不是按 pricing 重算：任务提交已 Detach，hold 没有租约；
// 若不终结，用户余额会被永久占用（账本 hold 永不释放）。而重算需要可信的
// 任务用量→金额映射（任务计价表达式），本批没有；空 fact 语义由计费侧
// 明确定义为「按预扣额结清」，不做任何 token 重算，绝不会把成功任务退成 0。
//
// `settlement_pending` 只保留给两种失败：① Terminate 调用失败（可重试）；
// ② 未来表达式求值失败（设计 §9.4「表达式失败保留预扣」）。正常成功任务
// 终结后写 `billing_final=true` 且 `settlement_pending=false`。

// settleTerminal 是终态结算的唯一入口。调用方必须已经通过 CAS 赢得终态。
func (p *Poller) settleTerminal(ctx context.Context, task *Task, _ *plugin.CompiledPlugin) {
	if task == nil {
		return
	}
	requestID := toString(task.PrivateData["request_id"])
	// 令牌身份来自任务行（T6a 持久化）：提交时的 PreConsume 已同事务扣减
	// api_tokens.remain_quota；结算/退款必须带上它，否则令牌额度不会回补
	// （T6b 复核 H1）。0 表示无令牌（/pg 会话路径）。
	tokenID := int64(privateInt(task.PrivateData["token_id"]))
	if requestID == "" {
		// 没有 requestID 就无法按幂等锚点结算/退款。这是数据缺陷（T6a 之后
		// 不应出现）；保留 billing_pending 让补偿扫描持续暴露，不猜金额。
		p.log.Error("tasks: terminal task has no request_id; settlement cannot proceed",
			"task_id", task.PublicTaskID, "state", task.State)
		return
	}
	switch task.State {
	case StateFailure:
		p.refundTerminal(ctx, task, requestID, tokenID)
	case StateSuccess:
		p.settleSuccessAtHeld(ctx, task, requestID, tokenID)
	default:
		// 非终态调用是编程错误：不静默推进，留给调用方修正。
		p.log.Error("tasks: settleTerminal called for non-terminal task",
			"task_id", task.PublicTaskID, "state", task.State)
	}
}

// refundTerminal 全额退款。RefundByRequestID 按 requestID 幂等：重复调用返回
// nil，不会二次退款。
func (p *Poller) refundTerminal(ctx context.Context, task *Task, requestID string, tokenID int64) {
	if err := p.billing.RefundByRequestIDForToken(requestID, tokenID); err != nil {
		// 退款失败：保留 billing_pending，并写 refund_pending 供补偿扫描重试。
		// 终态**不回退**（上游确实失败了），否则会重新进入轮询。
		p.markBilling(ctx, task, nil, map[string]any{
			"refund_pending":  true,
			"billing_pending": true,
		})
		p.log.Error("tasks: refund failed; marked refund_pending for reconciliation",
			"task_id", task.PublicTaskID, "request_id", requestID, "error", err)
		return
	}
	// 退款成功：quota 归零（设计 §9.5「成功后清 quota」），并封账。
	zero := int64(0)
	p.finishBilling(ctx, task, &zero, map[string]any{
		"billing_pending":    false,
		"refund_pending":     false,
		"settlement_pending": false,
	})
}

// settleSuccessAtHeld 以**预扣额**终结成功任务，释放 hold（差额 0）。
//
// 空 UsageFact 是计费侧冻结的「按预扣额结清」语义（billing/handoff.go）：
// 不解析定价、不按 token 重算——因此不会把成功任务退成 0。
//
// 幂等：TerminateByRequestID 按 requestID 幂等；重复调用只会重复写同一
// 结论（日志按 requestID 幂等），金额不变。
func (p *Poller) settleSuccessAtHeld(ctx context.Context, task *Task, requestID string, tokenID int64) {
	if err := p.billing.TerminateByRequestIDForToken(requestID, tokenID, contract.UsageFact{}); err != nil {
		// 结算失败：hold 仍在，写 settlement_pending + 保留 billing_pending，
		// 补偿扫描下一轮重试。终态不回退。
		p.markBilling(ctx, task, nil, map[string]any{
			"settlement_pending": true,
			"billing_pending":    true,
		})
		p.log.Error("tasks: terminate-at-held failed; marked settlement_pending for reconciliation",
			"task_id", task.PublicTaskID, "request_id", requestID, "error", err)
		return
	}
	// 成功：quota 即预扣额（本次结算不改变金额），封账并清除待办标记。
	held := task.Quota
	p.finishBilling(ctx, task, &held, map[string]any{
		"billing_pending":    false,
		"settlement_pending": false,
	})
}

// finishBilling 在资金动作成功后落最终标记；失败只记录（幂等动作可重试）。
func (p *Poller) finishBilling(ctx context.Context, task *Task, quota *int64, extra map[string]any) {
	flags := map[string]any{"billing_final": true}
	for k, v := range extra {
		flags[k] = v
	}
	if err := p.repo.UpdateBilling(ctx, task.ID, quota, flags); err != nil {
		p.log.Error("tasks: persist settlement result failed; reconciliation will retry",
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
func (p *Poller) markBilling(ctx context.Context, task *Task, quota *int64, flags map[string]any) {
	if err := p.repo.UpdateBilling(ctx, task.ID, quota, flags); err != nil {
		p.log.Error("tasks: persist billing pending flags failed", "task_id", task.PublicTaskID, "error", err)
	}
}

// reconcileOnce 是补偿扫描的最小版本。
//
// 处理对象（均为终态）：
//   - FAILURE 且 billing_pending/refund_pending：重试退款；
//   - SUCCESS 且 billing_pending/settlement_pending：重试「按预扣额终结」。
//     这同时覆盖 T6a 遗留的即时成功无 usage 行（当时只写了 settlement_pending）：
//     它们的 hold 同样需要释放，否则永久冻结。
//
// 单轮上限 defaultReconcileLimit（200 行）；更细的分页/水位属 T6c。
func (p *Poller) reconcileOnce(ctx context.Context) {
	tasks, err := p.repo.ListReconcilable(ctx, p.reconcileLimit)
	if err != nil {
		p.log.Error("tasks: list reconcilable tasks failed; retrying next round", "error", err)
		return
	}
	for _, task := range tasks {
		if ctx.Err() != nil {
			return
		}
		p.settleTerminal(ctx, task, nil)
	}
}

// buildTaskUsageFact 把 task.data.usage 转成契约的 TaskUsageFact（只数量）。
//
// 本批结算以预扣额终结、不消费 usage 求值；保留这个唯一构造入口是为了
// T6c 的任务计价表达式：届时的表达式求值只应从这里读 usage，避免出现第二套
// 「从 data 读 usage」的实现。单位来自插件声明；未声明单位的数值字段不入
// Units（设计 §9.3：枚举字段无单位）。
func buildTaskUsageFact(data map[string]any, m *plugin.Manifest) contract.TaskUsageFact {
	fact := contract.TaskUsageFact{Facts: map[string]contract.UsageFactValue{}}
	usage, ok := data["usage"].(map[string]any)
	if !ok || len(usage) == 0 {
		return fact
	}
	units := usageFieldUnits(m)
	for name, value := range usage {
		unit, declared := units[name]
		if !declared {
			continue
		}
		switch typed := value.(type) {
		case string:
			str := typed
			fact.Facts[name] = contract.UsageFactValue{Str: &str}
		default:
			num, ok := numberValue(value)
			if !ok {
				continue
			}
			num = clampUsageNumber(num, unit)
			fact.Facts[name] = contract.UsageFactValue{Num: &num}
			if unit != "" {
				if u, ok := billingUnitFor(unit); ok {
					if fact.Units == nil {
						fact.Units = map[string]contract.BillingUnit{}
					}
					fact.Units[name] = u
				}
			}
		}
	}
	fact.Source = contract.UsageUpstreamExact
	return fact
}

func billingUnitFor(unit string) (contract.BillingUnit, bool) {
	switch unit {
	case "second":
		return contract.UnitSecond, true
	case "count":
		return contract.UnitCount, true
	case "token":
		return contract.UnitToken, true
	case "credit":
		return contract.UnitCredit, true
	}
	return 0, false
}

func boolFlag(v any) bool {
	switch typed := v.(type) {
	case bool:
		return typed
	case string:
		parsed, err := strconv.ParseBool(typed)
		return err == nil && parsed
	}
	return false
}
