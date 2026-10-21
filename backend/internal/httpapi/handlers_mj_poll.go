// Package httpapi 的 MJ 后台轮询驱动（W8/MJ-b）。
//
// 【为什么驱动在 httpapi 而不是 tasks】tasks 是两族共用的数据面，若它 import
// internal/mj 就会形成环（mj 已 import tasks）。驱动需要同时使用 MJ 的
// 查询客户端/状态映射与 tasks 的 CAS/结算接缝，因此放在同时依赖两者的
// httpapi 层；worker 进程与本包共享同一份实现（platform 装配）。
//
// 【与插件 poller 的关系】调度骨架复用 tasks.MJPoller（family='mj' 领取 +
// 超时/失败上限 + 补偿扫描）；本文件只实现「单任务怎么查上游、怎么 CAS、
// 终态怎么交给 Settler」。两族共用 tasks 表与结算接缝，但轮询语义独立
// （relay-05 §6.1：MJ 1h vs 插件 1440m），不互相套用。
package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"newtg/gateway/internal/mj"
	"newtg/gateway/internal/tasks"
)

// MJPollStore 是轮询驱动写状态用的最小存储面（生产 *tasks.Repo）。
//
// 只暴露 CAS 写入：终态结算由 tasks.Settler 负责（它自己持有 UpdateBilling），
// 驱动不直接写 quota/结算标记，避免出现第二套资金写入路径。
type MJPollStore interface {
	ApplyPoll(ctx context.Context, id int64, from tasks.State, u tasks.PollUpdate) (bool, error)
}

// MJPollDriverConfig 是 MJ 轮询驱动的依赖。Store/Channels/Sealer/Client/
// Settler 都是硬依赖：缺任一都不存在安全降级（最多只能让任务永远停在
// 非终态并把预扣永久冻结）。
type MJPollDriverConfig struct {
	Store    MJPollStore
	Channels MJChannelSource
	Sealer   *SecretSealer
	Client   MJQuerier
	// Settler 与 MJPoller 的补偿结算共用同一个实例（tasks.Settler）。
	Settler tasks.MJTaskSettler
	Logger  *slog.Logger
	Now     func() time.Time
	// RetentionDays 读取 `mj.retention_terminal_days`；nil → 设计默认 30。
	// 0 = 不自动清理（终态不写 retention_deadline）。
	RetentionDays func() int64
}

// MJPollDriver 推进单个 MJ 任务，可并发使用。
type MJPollDriver struct {
	store     MJPollStore
	channels  MJChannelSource
	sealer    *SecretSealer
	client    MJQuerier
	settler   tasks.MJTaskSettler
	log       *slog.Logger
	now       func() time.Time
	retention func() int64
}

// NewMJPollDriver 构造驱动。
func NewMJPollDriver(cfg MJPollDriverConfig) (*MJPollDriver, error) {
	if cfg.Store == nil || cfg.Channels == nil || cfg.Sealer == nil || cfg.Client == nil || cfg.Settler == nil {
		return nil, errors.New("httpapi: MJ poll driver requires store, channels, sealer, client and settler")
	}
	d := &MJPollDriver{
		store: cfg.Store, channels: cfg.Channels, sealer: cfg.Sealer,
		client: cfg.Client, settler: cfg.Settler, log: cfg.Logger,
		now: cfg.Now, retention: cfg.RetentionDays,
	}
	if d.log == nil {
		d.log = slog.Default()
	}
	if d.now == nil {
		d.now = time.Now
	}
	if d.retention == nil {
		d.retention = func() int64 { return 30 }
	}
	return d, nil
}

// PollTask 是 tasks.MJPoller 的单任务 Handler。
//
// 流程（每一步都可能因上游/数据面抖动而失败，失败只累计、不改状态）：
//
//	锁定渠道 → 解密凭据 → 上游 fetch → R-MJ-15 归一化 →
//	  ├─ 非终态：CAS 回填 state/raw/progress/data（unknown 不累计失败）
//	  └─ 终态：CAS 写终态 + billing_pending → 赢家交给 Settler 结算/退款
//
// 返回 error 表示本轮观测失败（上游不可达/坏响应/渠道不可用），由
// MJPoller 累计连续失败；达到上限或超过 1h 兜底时 MJPoller 会把行推进到
// FAILURE 并写 billing_pending，随后由补偿扫描退款。
func (d *MJPollDriver) PollTask(ctx context.Context, task *tasks.Task) error {
	if task == nil {
		return nil
	}
	// M-1：family 防御必须先于任何结算动作。终态分支会调用 Settler；若先
	// 结算再判族，一个被错误送进来的插件终态行会被按 MJ 语义结算/退款。
	if task.Family != tasks.FamilyMJ {
		return fmt.Errorf("mj: poll driver received non-mj task %q", task.PublicTaskID)
	}
	if task.State.Terminal() {
		// 终态行不应进入领取集合；若被误送进来，只做结算（幂等），绝不查
		// 上游或改状态——否则会把已成功任务重新拉回非终态。
		d.settler.SettleTerminal(ctx, task)
		return nil
	}
	channelID := task.LockedChannelID
	if channelID == 0 {
		channelID = task.ChannelID
	}
	if channelID <= 0 {
		return fmt.Errorf("mj: task %q has no channel", task.PublicTaskID)
	}
	channel, err := d.channels.GetChannel(ctx, channelID)
	if err != nil {
		return fmt.Errorf("mj: read channel %d for task %q: %w", channelID, task.PublicTaskID, err)
	}
	apiKey, decErr := decryptChannelCredentialWith(d.sealer, channel.Credentials)
	if decErr != nil {
		return decErr
	}
	result, queryErr := d.client.Query(ctx, mj.SubmitConfig{
		BaseURL: channel.BaseURL,
		APIKey:  apiKey,
	}, task.UpstreamTaskID)
	if queryErr != nil {
		// 上游不可达/坏响应/任务不存在：本轮观测失败。轮询侧不改变任务
		// 状态，下一轮重试；1h 兜底负责最终失败 + 退款（relay-05 §6.2/§6.3）。
		return queryErr
	}

	status := mj.NormalizeStatus(result.Status, result.Progress)
	now := d.now().UTC()
	delta := result.DataDelta()

	if status.State.Terminal() {
		update := tasks.PollUpdate{
			State:      status.State,
			RawState:   status.RawState,
			Progress:   status.ProgressPercent,
			Data:       delta,
			FinishTime: &now,
			// billing_pending 与终态同一 UPDATE 落库：覆盖「状态已终态、
			// 结算未落地就崩溃」的窗口，补偿扫描据此发现待办。poll_failures
			// 清零，与插件 finalize 同形状。
			Private: map[string]any{"billing_pending": true, "poll_failures": 0},
			// 终态写保留截止（relay-05 §9）；清理 worker 是后续任务。
			RetentionDeadline: MJRetentionDeadline(now, d.retention()),
		}
		if status.State == tasks.StateFailure {
			reason := truncateMJPollReason(result.FailReason)
			if reason == "" {
				reason = "upstream_failure"
			}
			update.FailReason = &reason
		}
		won, err := d.store.ApplyPoll(ctx, task.ID, task.State, update)
		if err != nil {
			return fmt.Errorf("mj: persist terminal state for task %q: %w", task.PublicTaskID, err)
		}
		if !won {
			// 另一个执行者已推进终态；结算由 CAS 赢家负责，本执行者必须退出
			//（否则会双结算——虽然计费按 request_id 幂等，但不应依赖第二道防线）。
			return nil
		}
		// 同步内存快照后再结算：Settler 读 request_id/token_id/quota，且
		// finishBilling 会把 billing_final 写回内存。
		task.State = status.State
		task.RawState = status.RawState
		task.ProgressPercent = status.ProgressPercent
		task.FinishTime = &now
		if task.PrivateData == nil {
			task.PrivateData = map[string]any{}
		}
		task.PrivateData["billing_pending"] = true
		task.Data = mergeMJPollData(task.Data, delta)
		d.settler.SettleTerminal(ctx, task)
		return nil
	}

	update := tasks.PollUpdate{
		State:    status.State,
		RawState: status.RawState,
		Progress: status.ProgressPercent,
		Data:     delta,
	}
	if status.State != tasks.StateUnknown {
		// 有效响应（含 MODAL/QUEUED 等非终态）清零连续失败。
		update.Private = map[string]any{"poll_failures": 0}
	}
	// unknown：R-MJ-15 明确「不累计失败」。这里既不写 poll_failures，也不
	// 把状态猜成 failure；raw_state 保真，等待下一轮或超时兜底。
	won, err := d.store.ApplyPoll(ctx, task.ID, task.State, update)
	if err != nil {
		return fmt.Errorf("mj: persist poll state for task %q: %w", task.PublicTaskID, err)
	}
	if won {
		// 同步内存快照（I-3）：MJPoller 的 clearFailures 依据 task.State 判断
		// 是否清零；不同步会让 unknown 观测仍以旧状态被清零，失败上限失效。
		task.State = status.State
		task.RawState = status.RawState
		task.ProgressPercent = status.ProgressPercent
		task.Data = mergeMJPollData(task.Data, delta)
	}
	return nil
}

// mergeMJPollData 合并任务行已有 data 与本轮增量（内存镜像用）。
func mergeMJPollData(base, delta map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(delta))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range delta {
		out[k] = v
	}
	return out
}

// truncateMJPollReason 限制失败原因长度（按 rune，避免切断多字节字符）。
//
// 上游 failReason 可能很长；保留前 1024 个 rune 足以定位问题，且不会让
// 一次轮询写入放大成 MB 级 jsonb/列值。
func truncateMJPollReason(reason string) string {
	const maxRunes = 1024
	runes := []rune(reason)
	if len(runes) > maxRunes {
		runes = runes[:maxRunes]
	}
	return string(runes)
}
