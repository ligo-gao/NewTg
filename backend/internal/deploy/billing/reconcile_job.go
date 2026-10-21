package billing

import (
	"context"
	"time"
)

// TerminationObservation 是管理面确认删除成功后对 provider 的终态观测。
//
// 字段全部来自删除前的 GetDeployment（best-effort）。两个兜底口径必须分清：
//   - 段自身已有 started_at（对账推进过），只是 provider 观测缺失 → 以段起点
//   - 停止宽限做内部记账（billing-05 §4.4 信任顺序的末位），不把「未知用量」
//     当免费；
//   - 段从未启动（段与 provider 都没有起点、累计为 0）→ 释放全部预扣（成本 0）。
//     这是「从未运行」，不是「未知」，与 billing-03 §5.9.2 一致。
type TerminationObservation struct {
	DeploymentID string
	// ProviderStartedAt 是 provider 记录的部署开始时刻（部署级，非段级）。
	ProviderStartedAt *time.Time
	// FinishedAt 是 provider 记录的结束时刻。
	FinishedAt *time.Time
	// ComputeSeconds 是 provider 部署级累计 GPU 秒（ComputeMinutesServed×60）。
	ComputeSeconds int64
	// ExcludeSegmentIDs 是要排除的段：续期边界的收敛必须跳过刚预扣的新段
	// （它是「续期后的段」，不是「被续期关闭的旧段」）。
	ExcludeSegmentIDs []string
}

// TerminationResult 汇总一次终止收敛的结果。
type TerminationResult struct {
	// SegmentsSettled 是按用量结算的段数。
	SegmentsSettled int
	// SegmentsReleased 是释放全部预扣（从未运行）的段数。
	SegmentsReleased int
	// ConfirmedCredits 是各段确认成本之和（站点成本，非用户扣费）。
	ConfirmedCredits int64
	// ReleasedCredits 是释放的预扣之和。
	ReleasedCredits int64
}

// SettleTerminated 在管理面确认删除成功后立即收敛该部署的全部未终态段。
//
// 为什么不等 N16 的 provider-404 窗口：删除成功是**强证据**，provider 之后
// 查不到部署是必然的，再等 900s 只会把已知用量拖成「provider 不可达」，
// 一旦窗口到期按 Release 收尾就会把已发生的成本记成 0。这里直接用删除前
// 的 provider 观测（或内部记账）结算。
//
// 幂等：段已终态时 settleTerminal/Release 都短路；事实键由 segment_id 固定，
// 重复调用不会产生第二条 settlement/released 事实（billing-03 §5.9.2）。
func (l *Ledger) SettleTerminated(ctx context.Context, obs TerminationObservation) (TerminationResult, error) {
	var result TerminationResult
	if obs.DeploymentID == "" {
		return result, ErrSegmentNotFound
	}
	segments, err := l.store.listOpenSegmentsByDeployment(ctx, obs.DeploymentID)
	if err != nil {
		return result, err
	}
	now := l.now().UTC()
	excluded := make(map[string]struct{}, len(obs.ExcludeSegmentIDs))
	for _, id := range obs.ExcludeSegmentIDs {
		excluded[id] = struct{}{}
	}
	for _, seg := range segments {
		if _, skip := excluded[seg.SegmentID]; skip {
			continue
		}
		// 段级快照：内部记账的起点优先取段自身的 started_at（续期段从这里
		// 起算），只有在段还没开始时才回退到 provider 的部署级开始时刻。
		started := seg.StartedAt
		if started == nil {
			started = obs.ProviderStartedAt
		}
		snapshot := DeploymentSnapshot{
			DeploymentID:   obs.DeploymentID,
			Status:         "deleted",
			Class:          StatusTerminal,
			StartedAt:      started,
			FinishedAt:     obs.FinishedAt,
			ComputeSeconds: obs.ComputeSeconds,
		}
		state, err := l.settleTerminal(ctx, seg, snapshot, now)
		if err != nil {
			return result, err
		}
		switch state {
		case SegmentSettled:
			result.SegmentsSettled++
			if settlement, err := l.store.segmentByID(ctx, seg.SegmentID); err == nil {
				if settlement.SettledCredits != nil {
					result.ConfirmedCredits += *settlement.SettledCredits
				}
			}
		case SegmentAbandoned:
			result.SegmentsReleased++
			result.ReleasedCredits += seg.PreconsumedCredits
		}
	}
	return result, nil
}
