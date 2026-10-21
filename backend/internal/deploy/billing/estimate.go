package billing

import (
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// Quote 是管理面写路径冻结的 provider 报价（H1 接线输入）。
//
// 报价是预扣的唯一来源：金额缺失/无效时 Available=false，调用方必须拒绝
// 写操作（P-21 / billing-03 §5.9 逐项约束 1），不得按 0 计。
type Quote struct {
	// Available 表示 provider 给出了可用的 USD 报价。
	Available bool
	// CostUSD 是本次购买/续期的总报价（decimal，不经 float64）。
	CostUSD decimal.Decimal
	// Name/HardwareID/LocationIDs 是段快照字段（对账与成本展示用）。
	Name        string
	HardwareID  int64
	LocationIDs []int64
	// GPUsPerContainer/ReplicaCount 是计费乘数。
	GPUsPerContainer int
	ReplicaCount     int
	// DurationSeconds 是本次购买/续期时长（秒）。
	DurationSeconds int64
}

// QuoteFromPrice 把 provider 价格估算响应归一成计费报价。
//
// 可用性判定（顺序即语义）：
//   - provider 标记 estimation_valid=false → 不可用；
//   - 负金额 → 不可用（签名字段异常，不能当折扣）；
//   - 未设置 EstimatedCostSet 且金额为 0 → 不可用：这是「provider 没给报价」，
//     与「明确报价 0（免费）」不同；只有带报价标记的 0 才算免费；
//   - 币种非 USD/USDC（含空串）→ 不可用：冻结口径是 USD 计价。
func QuoteFromPrice(cost decimal.Decimal, costSet, estimationValid bool, currency string) Quote {
	available := estimationValid &&
		!cost.IsNegative() &&
		(costSet || cost.IsPositive())
	switch strings.ToUpper(strings.TrimSpace(currency)) {
	case "", "USD", "USDC":
	default:
		available = false
	}
	return Quote{Available: available, CostUSD: cost}
}

// EstimateForCreate 由冻结报价构造创建段的估算（H1 冻结接口）。
//
// seat 是冻结的计费乘数（gpus_per_container × replica_count）：报价摊成
// 「每 GPU·秒单价」只在整数乘数上做分母，金额全程 decimal（billing-02 §5.3）。
// DeploymentID / IdempotencyKey 由调用方补齐——创建时 provider 尚未返回部署
// ID，段只能在写成功之后落库（失败即回滚创建，见 handlers_deployments.go）。
func EstimateForCreate(quote Quote, seat int) SegmentEstimate {
	est := SegmentEstimate{
		SegmentKind:      SegmentKindCreate,
		Name:             quote.Name,
		HardwareID:       quote.HardwareID,
		LocationIDs:      quote.LocationIDs,
		GPUsPerContainer: quote.GPUsPerContainer,
		ReplicaCount:     quote.ReplicaCount,
		PlannedSeconds:   quote.DurationSeconds,
		QuoteUSD:         quote.CostUSD,
		PriceSource:      PriceSourceProviderQuote,
		// 创建后 provider 尚未确认开始时刻：pending_confirm 由 N16 对账在
		// provider 给出 started_at 后转 running（billing-03 §5.9.3）。
		State: SegmentPendingConfirm,
	}
	denominator := int64(seat) * quote.DurationSeconds
	if denominator > 0 && !quote.CostUSD.IsNegative() {
		est.UnitPriceUSDPerGPUSecond = quote.CostUSD.Div(decimal.NewFromInt(denominator))
	}
	return est
}

// EstimateForExtend 由冻结报价构造续期段的估算（H1）。
//
// 续期是**新段**（新报价、新预扣），不是对旧段的 Reserve（billing-03 §3.7）；
// 旧段在续期生效点结算关闭（见 SettleTerminated）。续期段从管理面续期时刻
// 起算：provider 的部署 started_at 是**原始**开始时刻，直接沿用会把整段历史
// 再记一遍。
func EstimateForExtend(quote Quote, seat int, startedAt time.Time) SegmentEstimate {
	est := EstimateForCreate(quote, seat)
	est.SegmentKind = SegmentKindExtend
	at := startedAt.UTC()
	est.StartedAt = &at
	est.State = SegmentRunning
	return est
}

// ValidateQuoteShape 校验报价携带的段快照字段是否足以落段。
func ValidateQuoteShape(quote Quote) error {
	if !quote.Available {
		return fmt.Errorf("%w: quote is not available", ErrGPUPriceUnavailable)
	}
	if quote.DurationSeconds < 1 {
		return fmt.Errorf("%w: duration_seconds", ErrInvalidSegment)
	}
	if quote.GPUsPerContainer < 1 || quote.GPUsPerContainer > MaxDeploymentGPUsPerContainer {
		return fmt.Errorf("%w: gpus_per_container %d", ErrInvalidSegment, quote.GPUsPerContainer)
	}
	if quote.ReplicaCount < 1 || quote.ReplicaCount > MaxDeploymentReplicas {
		return fmt.Errorf("%w: replica_count %d", ErrInvalidSegment, quote.ReplicaCount)
	}
	return nil
}
