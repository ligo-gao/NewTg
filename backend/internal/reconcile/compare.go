package reconcile

import "math"

// Compare 按固定顺序比较本地聚合与上游快照，返回差异与严重程度。
//
// 判定顺序是刻意的：
//   - 已提供维度的真实差异优先于「快照不完整」——否则一个确实差了几十万
//     token 的日期会被 partial_snapshot 掩盖；
//   - 反过来，快照没提供的维度不参与比较，也就绝不会因此产生 alert。
func Compare(in Input) Comparison {
	// 1. 没有快照：待录入，不算告警。
	if in.Snapshot == nil {
		return Comparison{Severity: SeverityMissingSnapshot}
	}
	// 2. 有快照没有本地聚合：可能漏记或渠道未归属。
	if in.Local == nil {
		return Comparison{Severity: SeverityMissingLocal}
	}

	local, snap := in.Local, in.Snapshot
	c := Comparison{
		// 3. 快照缺少任一 token 维度即为不完整；nil 表示「未提供」，绝不能当作 0。
		SnapshotPartial: snap.CacheHitTokens == nil ||
			snap.CacheMissTokens == nil ||
			snap.OutputTokens == nil,
	}

	// 4. 金额始终可以比较：本地成本按额度单价换算成分。
	c.RevenueMinor = CreditsToMinor(local.CostCredits, in.QuotaPerUnit)
	overAmount := snap.AmountMinor != nil &&
		absInt64(c.RevenueMinor-*snap.AmountMinor) > in.Tolerances.AmountMinor

	// 5. 只对快照提供了的 token 维度计算差值；总量必须取同一组维度。
	var (
		absSum, snapSum, maxAbs int64
		comparable              bool
	)
	recordDim := func(delta, snapValue *int64) {
		if delta == nil {
			return
		}
		comparable = true
		a := absInt64(*delta)
		absSum = addSat(absSum, a)
		snapSum = addSat(snapSum, *snapValue)
		if a > maxAbs {
			maxAbs = a
		}
	}
	c.DeltaCacheHitTokens = diffDim(local.CacheHitTokens, snap.CacheHitTokens)
	recordDim(c.DeltaCacheHitTokens, snap.CacheHitTokens)
	c.DeltaCacheMissTokens = diffDim(local.CacheMissTokens, snap.CacheMissTokens)
	recordDim(c.DeltaCacheMissTokens, snap.CacheMissTokens)
	c.DeltaOutputTokens = diffDim(local.OutputTokens, snap.OutputTokens)
	recordDim(c.DeltaOutputTokens, snap.OutputTokens)

	var bp *int64
	if comparable {
		v := DeltaBP(absSum, snapSum)
		bp = &v
		c.DeltaTokenBP = bp
	}

	switch {
	// 6. 差异率与绝对量同时超阈值才是 alert。
	case bp != nil && *bp > int64(in.Tolerances.TokenBP) && maxAbs > in.Tolerances.MinDeltaTokens:
		c.Severity = SeverityAlert
	// 7. 金额超出容差。
	case overAmount:
		c.Severity = SeverityWarn
	// 8. 没有任何可比 token 维度：金额已检查，其余维度不告警。
	case bp == nil:
		c.Severity = SeverityPartialSnapshot
	// 9. 差异率超阈值，或存在语义未知的请求（无法判断应计入哪个维度）。
	case *bp > int64(in.Tolerances.TokenBP) || local.UnknownRequests > 0:
		c.Severity = SeverityWarn
	// 10. 有可比且都在容差内，但快照本身不完整。
	case c.SnapshotPartial:
		c.Severity = SeverityPartialSnapshot
	// 11. 全部在容差内。
	default:
		c.Severity = SeverityOK
	}
	return c
}

// diffDim 计算 local - snapshot；snapshot 为 nil 时返回 nil，表示该维度不可比较。
func diffDim(local int64, snapshot *int64) *int64 {
	if snapshot == nil {
		return nil
	}
	delta := local - *snapshot
	return &delta
}

// absInt64 返回绝对值；MinInt64 的绝对值溢出时饱和到 MaxInt64。
func absInt64(v int64) int64 {
	if v < 0 {
		if v == math.MinInt64 {
			return math.MaxInt64
		}
		return -v
	}
	return v
}

// addSat 饱和相加，避免多个超大差值求和时回绕成负数。
func addSat(a, b int64) int64 {
	sum := a + b
	if b > 0 && sum < a {
		return math.MaxInt64
	}
	if b < 0 && sum > a {
		return math.MinInt64
	}
	return sum
}
