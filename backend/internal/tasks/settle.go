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
// 计价事实源有两条，按任务行是否冻结了任务计价表达式区分：
//
//   - FAILURE：全额退款（RefundByRequestID）。
//   - SUCCESS 且 `private_data.tiered_snapshot` 存在：按冻结快照求值真实
//     成本，差额结算（多退少补）——T5。
//   - SUCCESS 且无快照：按预扣额终结——`TerminateByRequestID(requestID,
//     UsageFact{})` 以 hold 为最终成本（差额 0）。这是 T5 之前提交的存量
//     任务与「未配置任务表达式」的模型，语义必须保持不变。
//
// 为什么无表达式时仍用空 fact：任务提交已 Detach，hold 没有租约；若不
// 终结，用户余额会被永久占用（账本 hold 永不释放）。空 fact 语义由计费侧
// 明确定义为「按预扣额结清」，不做任何 token 重算，绝不会把成功任务退成 0。
//
// `settlement_pending` 保留给两种失败：① Terminate 调用失败（可重试）；
// ② 表达式求值失败——P-07 冻结：**保留预扣、不退款**，写审计/日志，由补偿
// 扫描重试（差额不可信时退款等于把「表达式写错」变成免费服务）。
// 正常成功任务终结后写 `billing_final=true` 且 `settlement_pending=false`。

// settleTerminal 是终态结算的唯一入口。调用方必须已经通过 CAS 赢得终态。
func (p *Poller) settleTerminal(ctx context.Context, task *Task, compiled *plugin.CompiledPlugin) {
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
		p.settleSuccess(ctx, task, requestID, tokenID, compiled)
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

// settleSuccess 结算成功任务：有冻结表达式按表达式差额结算，否则按预扣额结清。
//
// 【为什么实时与扫描共用本函数】两条路径（提交期即时成功 / 后台轮询 +
// 补偿扫描）对同一任务必须给出同一金额。金额只由任务行里的冻结快照
// 决定，不读配置、不看进程内状态，因此重试/重启/双执行者只会得到同一
// 结论（幂等锚点仍是 requestID）。
func (p *Poller) settleSuccess(ctx context.Context, task *Task, requestID string, tokenID int64, compiled *plugin.CompiledPlugin) {
	credits, configured, evalErr := EvalTaskCost(
		task.PrivateData[PrivateTieredSnapshot], task.Data, p.manifestFor(task, compiled))
	if !configured {
		// 没有冻结表达式：沿用 T5 之前的「按预扣额结清」，存量任务语义不变。
		p.settleSuccessAtHeld(ctx, task, requestID, tokenID)
		return
	}
	if evalErr != nil {
		// P-07：求值失败 → 保留预扣（不退款），置 settlement_pending 供
		// 补偿扫描重试。终态不回退（上游确实成功了，不能重新进入轮询）。
		p.markSettlementPending(ctx, task, requestID, evalErr)
		return
	}
	// 差额结算：credits 是最终应扣（多退少补由计费侧在 hold 上完成）。
	// 传空 UsageFact 只影响 usage_logs 的 token 字段；真实金额由 cost 承载，
	// 不会被误当「按 token 重算」。
	if err := p.billing.TerminateByRequestIDForTokenCost(requestID, tokenID, credits, contract.UsageFact{}); err != nil {
		p.markSettlementPending(ctx, task, requestID, err)
		return
	}
	p.log.Info("tasks: settled success task by frozen expression",
		"task_id", task.PublicTaskID, "request_id", requestID, "credits", credits,
		"source", snapshotSource(task.PrivateData[PrivateTieredSnapshot]))
	p.finishBilling(ctx, task, &credits, map[string]any{
		"billing_pending":    false,
		"settlement_pending": false,
	})
}

// markSettlementPending 记录「保留预扣、等待补偿」的状态。
//
// 写 billing_pending 是兼容 T6b 补偿扫描的既有谓词（ListReconcilable 同时
// 看 settlement_pending），两者一起写让旧扫描器与新扫描器都能看到。
func (p *Poller) markSettlementPending(ctx context.Context, task *Task, requestID string, cause error) {
	flags := map[string]any{
		"settlement_pending": true,
		"billing_pending":    true,
		// 复核 I3：长期 settlement_pending 必须可观测。把失败原因与首次进入
		// 待定的时间落进 private_data，运维无需翻日志即可区分「表达式/用量
		// 损坏」与「暂时性 DB 故障」，并据此判断 hold 被钉住的时长。
		"settlement_pending_reason": truncateSettlementReason(cause),
	}
	if task.PrivateData == nil || task.PrivateData["settlement_pending_since"] == nil {
		flags["settlement_pending_since"] = p.now().UTC().Format(time.RFC3339)
	}
	p.markBilling(ctx, task, nil, flags)
	p.log.Error("tasks: settlement failed; kept pre-charged hold and marked settlement_pending",
		"task_id", task.PublicTaskID, "request_id", requestID,
		"pending_since", flags["settlement_pending_since"], "error", cause)
}

// truncateSettlementReason 截断待定原因，避免把上游大段错误写进 jsonb。
//
// 按 rune 而不是字节截断：PostgreSQL 的 UTF8 编码拒绝非法字节序列，按
// 字节切会把多字节字符截成半个，导致整轮 private_data 更新失败
// （settlement_pending_reason/since 反而落不了库）。
func truncateSettlementReason(err error) string {
	if err == nil {
		return ""
	}
	const maxRunes = 512
	runes := []rune(err.Error())
	if len(runes) > maxRunes {
		runes = runes[:maxRunes]
	}
	return string(runes)
}

// manifestFor 取任务所属插件的 manifest。
//
// 复核 I1 之后它只是**旧快照**的回落：新快照已冻结表达式引用的字段单位，
// 结算不再依赖活插件。轮询路径可能已经拿到 compiled；补偿扫描没有，因此
// 回落到插件快照按 task.PluginKey 查找。
func (p *Poller) manifestFor(task *Task, compiled *plugin.CompiledPlugin) *plugin.Manifest {
	if compiled != nil {
		return compiled.Manifest()
	}
	if p.plugins == nil || task == nil || task.PluginKey == "" {
		return nil
	}
	entry := p.plugins.Snapshot().Lookup(task.PluginKey)
	if entry.Plugin == nil {
		return nil
	}
	return entry.Plugin.Manifest()
}

// settleSuccessAtHeld 以**预扣额**终结成功任务，释放 hold（差额 0）。
//
// 空 UsageFact 是计费侧冻结的「按预扣额结清」语义（billing/handoff.go）：
// 不解析定价、不按 token 重算——因此不会把成功任务退成 0。仅用于没有
// 任务计价表达式的任务；配置了表达式的成功任务必须走 settleSuccess。
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
	flags := map[string]any{
		"billing_final": true,
		// 复核 Minor-b：封账成功后清除待定审计字段。它们只在
		// settlement_pending 期间有意义，残留会误导运维以为仍在待定。
		// UpdateBilling 把 JSON null 解释为“删除该键”。
		"settlement_pending_reason": nil,
		"settlement_pending_since":  nil,
	}
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
// 这是「从 data 读 usage」的唯一构造入口：表达式求值只走这里，避免出现
// 第二套实现。单位来自插件声明（或提交期冻结的单位表，见
// buildTaskUsageFactWithUnits）；未声明单位的数值字段不入 Units
// （设计 §9.3：枚举字段无单位）。
func buildTaskUsageFact(data map[string]any, m *plugin.Manifest) contract.TaskUsageFact {
	return buildTaskUsageFactWithUnits(data, usageFieldUnits(m))
}

// buildTaskUsageFactWithUnits 按给定的「字段 → 单位」表构造用量事实。
//
// 复核 I1：结算优先使用提交期冻结的单位表，插件改名/下架/升级都不会改变
// 已提交任务的取数与钳制口径。
func buildTaskUsageFactWithUnits(data map[string]any, units map[string]string) contract.TaskUsageFact {
	fact := contract.TaskUsageFact{Facts: map[string]contract.UsageFactValue{}}
	usage, ok := data["usage"].(map[string]any)
	if !ok || len(usage) == 0 {
		return fact
	}
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
