package tasks

import (
	"context"
	"strconv"
	"time"

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
//   - SUCCESS：保留预扣（KeepReservation）——**不调用 TerminateByRequestID**。
//
// 为什么成功也不 Terminate：Terminate 会用当前定价快照按「不完整的 usage」
// 重算实际成本；任务用量事实（seconds/count）并不映射到 token 计费的入参，
// 一旦快照是 token/倍率模式，重算会得到 0 并把一笔已成功的任务全额退掉
// （免费服务）。差额不可信时保留预扣是设计 §9.4「表达式失败保留预扣」的
// 同一保守方向。待 T6c 落地任务计价表达式后，由表达式求值产生可信的
// 实际额度，再走差额结算并清除 settlement_pending。
//
// 代价（明确登记）：成功任务的 hold 在 T6c 之前保持冻结；本批不释放它，
// 也不把它写成消耗。补偿扫描会把这些任务计入 settlement_pending 计数。

// settleTerminal 是终态结算的唯一入口。调用方必须已经通过 CAS 赢得终态。
func (p *Poller) settleTerminal(ctx context.Context, task *Task, compiled *plugin.CompiledPlugin) {
	if task == nil {
		return
	}
	requestID := toString(task.PrivateData["request_id"])
	if requestID == "" {
		// 没有 requestID 就无法按幂等锚点结算/退款。这是数据缺陷（T6a 之后
		// 不应出现）；保留 billing_pending 让补偿扫描持续暴露，不猜金额。
		p.log.Error("tasks: terminal task has no request_id; settlement cannot proceed",
			"task_id", task.PublicTaskID, "state", task.State)
		return
	}
	switch task.State {
	case StateFailure:
		p.refundTerminal(ctx, task, requestID)
	case StateSuccess:
		p.keepReservation(ctx, task, compiled)
	default:
		// 非终态调用是编程错误：不静默推进，留给调用方修正。
		p.log.Error("tasks: settleTerminal called for non-terminal task",
			"task_id", task.PublicTaskID, "state", task.State)
	}
}

// refundTerminal 全额退款。RefundByRequestID 按 requestID 幂等：重复调用返回
// nil，不会二次退款。
func (p *Poller) refundTerminal(ctx context.Context, task *Task, requestID string) {
	if err := p.billing.RefundByRequestID(requestID); err != nil {
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

// keepReservation 记录「保留预扣」的结算结论。
//
// 幂等：同一任务重复结算只刷新 settled_at，不改变金额；告警只在首次
// （settlement_pending 从无到有）发出，避免 15s 一轮的补偿扫描刷屏。
func (p *Poller) keepReservation(ctx context.Context, task *Task, compiled *plugin.CompiledPlugin) {
	alreadyPending := boolFlag(task.PrivateData["settlement_pending"])
	if err := p.repo.UpdateBilling(ctx, task.ID, nil, map[string]any{
		"billing_pending":    false,
		"settlement_pending": true,
		"settled_at":         p.now().UTC().Format(time.RFC3339),
	}); err != nil {
		p.log.Error("tasks: persist keep-reservation failed; billing_pending retained",
			"task_id", task.PublicTaskID, "error", err)
		return
	}
	if task.PrivateData != nil {
		task.PrivateData["settlement_pending"] = true
		task.PrivateData["billing_pending"] = false
	}
	if alreadyPending {
		return
	}
	var manifest *plugin.Manifest
	if compiled != nil {
		manifest = compiled.Manifest()
	}
	fact := buildTaskUsageFact(task.Data, manifest)
	p.log.Warn("tasks: successful task kept its reservation because no task billing expression is configured; "+
		"settlement deferred to the task-pricing batch (T6c)",
		"task_id", task.PublicTaskID, "plugin", task.PluginKey,
		"request_id", toString(task.PrivateData["request_id"]),
		"quota_held", task.Quota, "usage_facts", len(fact.Facts))
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
// 处理对象：
//   - FAILURE 且 billing_pending/refund_pending：重试退款；
//   - SUCCESS 且 billing_pending（尚未写下保留预扣结论）：补写 settlement_pending。
//
// settlement_pending 的成功任务**不重试**：本批没有表达式可求值，重试只会
// 产生同一结论并每轮写库。只统计数量供运维观察；真正的补结算属 T6c。
func (p *Poller) reconcileOnce(ctx context.Context) {
	tasks, err := p.repo.ListReconcilable(ctx, p.reconcileLimit)
	if err != nil {
		p.log.Error("tasks: list reconcilable tasks failed; retrying next round", "error", err)
		return
	}
	if len(tasks) == 0 {
		return
	}
	var settlementPending int
	for _, task := range tasks {
		if ctx.Err() != nil {
			return
		}
		switch task.State {
		case StateFailure:
			p.settleTerminal(ctx, task, nil)
		case StateSuccess:
			if boolFlag(task.PrivateData["settlement_pending"]) && !boolFlag(task.PrivateData["billing_pending"]) {
				settlementPending++
				continue
			}
			p.settleTerminal(ctx, task, nil)
		}
	}
	if settlementPending > 0 {
		p.log.Debug("tasks: successful tasks await task-pricing expression settlement (T6c)",
			"count", settlementPending)
	}
}

// buildTaskUsageFact 把 task.data.usage 转成契约的 TaskUsageFact（只数量）。
//
// 单位来自插件声明；未声明单位的数值字段不入 Units（设计 §9.3：枚举字段
// 无单位）。本批不消费它做计价——保留它是为了 T6c 的表达式求值有唯一入口，
// 避免届时候选两套「从 data 读 usage」的实现。
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
