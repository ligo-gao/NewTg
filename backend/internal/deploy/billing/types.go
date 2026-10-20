// Package billing 实现 GPU 资源租赁的站点成本账（Task 5c / `G-OPS-012`）。
//
// 付款方是站点（运营方），不是用户/租户：本包只写
// `deployment_billing_segments` 与 append-only 的 `deployment_cost_facts`，
// **不调用任何用户资金入口**（IncreaseUserQuota / DecreaseUserQuota /
// PostConsumeUserSubscriptionDelta / IncreaseTokenQuota）。
//
// 设计权威：
//   - `platform-01` §22.13（字段/索引/CHECK）；
//   - `billing-00` §6.6（DeploymentBillingLedger 契约）、§7.1（GPU 换算公式）；
//   - `billing-02` §5.3（冻结报价 → 每 GPU·秒单价）；
//   - `billing-03` §3.7/§5.9（预扣、多退少补、未知结果对账）；
//   - `billing-05` §4.4（GPU·秒推导、信任顺序、跨段基线相减）；
//   - `billing-06` §3.3.3 N16（60s 对账周期、300s 租约、900s 窗口）。
package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/shopspring/decimal"
)

// 冻结常量（billing-05 §4.4.4 / billing-03 §5.9.3 / billing-06 §3.3.3）。
const (
	// MaxDeploymentGPUsPerContainer 是每容器 GPU 数上界（计费乘数）。
	MaxDeploymentGPUsPerContainer = 8
	// MaxDeploymentReplicas 是副本数上界（计费乘数）。
	MaxDeploymentReplicas = 64
	// MaxDeploymentDurationHours 是单次创建/续期时长上界（30 天）。
	MaxDeploymentDurationHours = 720
	// MaxDeploymentGPUSecondsPerSegment 是单段 GPU·秒绝对上界：
	// 8 × 64 × (720×3600 + 300)。必须含 300s 停止宽限，否则 planned=720h
	// 的合法宽限会被误判为越界（billing-05 §4.4.4 的边界算式）。
	MaxDeploymentGPUSecondsPerSegment = int64(1_327_257_600)
	// DeploymentStopGraceSeconds 是停止/删除滞后的计费宽限。
	DeploymentStopGraceSeconds = int64(300)
	// DeploymentBillingLeaseTTLSeconds 是计费段心跳租约（N16 扫描窗口）。
	DeploymentBillingLeaseTTLSeconds = 300 * time.Second
	// DeploymentReconcileWindowSeconds 是 provider 找不到部署时的放弃窗口，
	// 从进入 pending_confirm/deleting_unknown 的 state_changed_at 起算；
	// 重试不重置（billing-03 §5.9.3）。
	DeploymentReconcileWindowSeconds = 900 * time.Second

	// DefaultAccrualInterval 是记账周期（按设计默认每小时）。
	DefaultAccrualInterval = time.Hour
	// DefaultReconcileInterval 是 N16 的对账周期（billing-06 §3.3.3：60s）。
	DefaultReconcileInterval = 60 * time.Second

	// accrueBatchSize 限制单轮扫描的段数，避免长事务与一次性 provider 调用。
	accrueBatchSize = 500
)

// 计费段枚举（platform-01 §22.13.1）。
const (
	SegmentKindCreate = "create"
	SegmentKindExtend = "extend"

	PriceSourceProviderQuote = "provider_quote"

	SegmentPendingConfirm  = "pending_confirm"
	SegmentRunning         = "running"
	SegmentDeletingUnknown = "deleting_unknown"
	SegmentSettled         = "settled"
	SegmentAbandoned       = "abandoned"
	SegmentFailed          = "failed"
)

// 成本事实枚举（platform-01 §22.13.2）。
const (
	FactPreconsume               = "preconsume"
	FactAccrual                  = "accrual"
	FactSettlement               = "settlement"
	FactReconciliationAdjustment = "reconciliation_adjustment"
	FactReleased                 = "released"

	UnitGPUSecond = "gpu_second"

	UsageProviderExact      = "provider_exact"
	UsageProviderPartial    = "provider_partial"
	UsageInternalAccounting = "internal_accounting"
	UsageEstimated          = "estimated"
	UsageReservedDefault    = "reserved_default"

	FactHeld          = "held"
	FactAccrued       = "accrued"
	FactConfirmed     = "confirmed"
	FactReleasedState = "released"
	FactAdjusted      = "adjusted"
)

// ErrSegmentNotFound 表示计费段不存在。
var ErrSegmentNotFound = errors.New("deploy billing: segment not found")

// ErrInvoiceUnavailable 表示当前 provider 适配器没有账单能力（接缝未接线）。
var ErrInvoiceUnavailable = errors.New("deploy billing: provider invoice unavailable")

// ErrInvalidSegment 表示段/事实输入不满足设计约束。
var ErrInvalidSegment = errors.New("deploy billing: invalid segment")

// ErrGPUPriceUnavailable 表示 provider 没有给出可用报价（缺省或负数）。
//
// 报价缺失不得当作免费：调用方必须阻断建段/续期（M3 复核）。只有 provider
// 显式返回 0 且带了报价标记时才允许 0 单价。
var ErrGPUPriceUnavailable = errors.New("deploy billing: gpu price unavailable")

// Segment 是 deployment_billing_segments 的一行。
type Segment struct {
	SegmentID                string
	DeploymentID             string
	SegmentKind              string
	IdempotencyKey           string
	ProviderRequestID        *string
	Name                     string
	HardwareID               int64
	LocationIDs              []int64
	GPUsPerContainer         int
	ReplicaCount             int
	PlannedSeconds           int64
	StartedAt                *time.Time
	PlannedEndAt             *time.Time
	QuoteUSD                 decimal.Decimal
	UnitPriceUSDPerGPUSecond decimal.Decimal
	PriceSource              string
	PreconsumedCredits       int64
	SettledCredits           *int64
	State                    string
	BillingHeartbeatAt       time.Time
	LastProviderObservedAt   *time.Time
	CreatedAt                time.Time
	UpdatedAt                time.Time
	StateChangedAt           time.Time
}

// CostFact 是 deployment_cost_facts 的一行（append-only）。
type CostFact struct {
	FactID                    int64
	FactKey                   string
	SegmentID                 string
	DeploymentID              string
	FactType                  string
	PeriodStart               *time.Time
	PeriodEnd                 *time.Time
	GPUsPerContainer          int
	ReplicaCount              int
	GPUSeconds                int64
	Unit                      string
	UsageSource               string
	EstimatedCredits          int64
	ConfirmedCredits          *int64
	State                     string
	ProviderObservedAt        *time.Time
	AuditRef                  *string
	CreatedAt                 time.Time
	ProviderCumulativeSeconds *int64
}

// SegmentEstimate 是 `Hold` 的输入（billing-00 §6.6 SegmentEstimate 的本地扩展：
// 增加 provider/观测字段，便于管理面在写操作成功后立即落段）。
type SegmentEstimate struct {
	DeploymentID             string
	IdempotencyKey           string
	SegmentKind              string
	ProviderRequestID        string
	Name                     string
	HardwareID               int64
	LocationIDs              []int64
	GPUsPerContainer         int
	ReplicaCount             int
	PlannedSeconds           int64
	StartedAt                *time.Time
	QuoteUSD                 decimal.Decimal
	UnitPriceUSDPerGPUSecond decimal.Decimal
	PriceSource              string
	// State 为空时按是否有 StartedAt 推导：有起点 → running，否则 pending_confirm。
	State string
}

// GPUUsageFact 是结算/对账的计费量事实（billing-00 §9.4）。
type GPUUsageFact struct {
	SegmentID                 string
	DeploymentID              string
	GPUSeconds                int64
	GPUsPerContainer          int
	ReplicaCount              int
	Source                    string
	PeriodStart               *time.Time
	PeriodEnd                 *time.Time
	ProviderCumulativeSeconds *int64
	ProviderObservedAt        *time.Time
	AuditRef                  string
}

// Invoice 是 provider 账单（外部真源）的最小投影。provider 报价是基准，
// 发票差异写 reconciliation_adjustment（billing-03 §5.9.3 第 6 条）。
type Invoice struct {
	BillReference string
	AmountUSD     decimal.Decimal
	PeriodStart   *time.Time
	PeriodEnd     *time.Time
}

// InvoiceReader 是 provider 账单能力的接缝。io.net 适配器当前没有账单端点
// （FakeProvider 同样没有），因此本批只定义接口并登记为残余项；接线后
// ReconcileInvoice 即可把差异写成 reconciliation_adjustment。
type InvoiceReader interface {
	Invoice(ctx context.Context, deploymentID string) (Invoice, error)
}

// SegmentTerminal 报告段是否已进入终态（重复结算/释放必须短路）。
func SegmentTerminal(state string) bool {
	switch state {
	case SegmentSettled, SegmentAbandoned, SegmentFailed:
		return true
	default:
		return false
	}
}

// ValidateEstimate 在写库前校验乘数与金额，越界不钳制（billing-05 §4.4.4）：
// 静默接受更大的乘数会让成本账失真。
func ValidateEstimate(est SegmentEstimate) error {
	if est.DeploymentID == "" {
		return fmt.Errorf("%w: deployment_id is required", ErrInvalidSegment)
	}
	if est.IdempotencyKey == "" {
		return fmt.Errorf("%w: idempotency_key is required", ErrInvalidSegment)
	}
	if est.SegmentKind != SegmentKindCreate && est.SegmentKind != SegmentKindExtend {
		return fmt.Errorf("%w: segment_kind %q", ErrInvalidSegment, est.SegmentKind)
	}
	if est.GPUsPerContainer < 1 || est.GPUsPerContainer > MaxDeploymentGPUsPerContainer {
		return fmt.Errorf("%w: gpus_per_container %d outside 1..%d",
			ErrInvalidSegment, est.GPUsPerContainer, MaxDeploymentGPUsPerContainer)
	}
	if est.ReplicaCount < 1 || est.ReplicaCount > MaxDeploymentReplicas {
		return fmt.Errorf("%w: replica_count %d outside 1..%d",
			ErrInvalidSegment, est.ReplicaCount, MaxDeploymentReplicas)
	}
	if est.PlannedSeconds < 1 || est.PlannedSeconds > MaxDeploymentDurationHours*3600 {
		return fmt.Errorf("%w: planned_seconds %d outside 1..%d",
			ErrInvalidSegment, est.PlannedSeconds, MaxDeploymentDurationHours*3600)
	}
	if est.QuoteUSD.IsNegative() {
		return fmt.Errorf("%w: quote_usd is negative", ErrInvalidSegment)
	}
	if est.UnitPriceUSDPerGPUSecond.IsNegative() {
		return fmt.Errorf("%w: unit_price is negative", ErrInvalidSegment)
	}
	if est.PriceSource == "" {
		est.PriceSource = PriceSourceProviderQuote
	}
	if est.PriceSource != PriceSourceProviderQuote {
		return fmt.Errorf("%w: price_source %q", ErrInvalidSegment, est.PriceSource)
	}
	if est.Name == "" {
		return fmt.Errorf("%w: name is required", ErrInvalidSegment)
	}
	if est.HardwareID <= 0 {
		return fmt.Errorf("%w: hardware_id must be positive", ErrInvalidSegment)
	}
	if len(est.LocationIDs) == 0 {
		return fmt.Errorf("%w: location_ids is required", ErrInvalidSegment)
	}
	return nil
}

// PlannedGPUSeconds 返回整段计划 GPU·秒（预扣事实的规范计费量）。
func PlannedGPUSeconds(est SegmentEstimate) int64 {
	return int64(est.GPUsPerContainer) * int64(est.ReplicaCount) * est.PlannedSeconds
}

// SegmentMaxGPUSeconds 返回本段的上界（含停止宽限），并夹到绝对上界。
func SegmentMaxGPUSeconds(seg Segment) int64 {
	upper := int64(seg.GPUsPerContainer) * int64(seg.ReplicaCount) *
		(seg.PlannedSeconds + DeploymentStopGraceSeconds)
	if upper > MaxDeploymentGPUSecondsPerSegment {
		return MaxDeploymentGPUSecondsPerSegment
	}
	return upper
}

// splitPeriods 把 [start, end] 按**墙钟整点**切分（"跨整点/不足一小时"）。
//
// 为什么按整点而不是按段起点切：worker 默认每小时运行一次，整点边界让
// 同一段的每次扫描产生完全相同的 period_start，fact_key 因此稳定；
// 若按段起点切，重启时刻不同会生成新的周期键，破坏幂等。
//
// inclusiveOpen=false 时跳过仍在进行中的当前小时（记账只写已闭合周期，
// 否则本小时先写入的短周期事实会永远停留在短时长——ON CONFLICT 不更新）。
func splitPeriods(start, end time.Time, inclusiveOpen bool) []period {
	if !end.After(start) {
		return nil
	}
	start = start.UTC()
	end = end.UTC()
	var out []period
	cursor := start
	for cursor.Before(end) {
		periodStart := cursor.Truncate(time.Hour)
		periodEnd := periodStart.Add(time.Hour)
		isOpenHour := end.Before(periodEnd)
		if isOpenHour && !inclusiveOpen {
			break
		}
		segStart := cursor
		if segStart.Before(periodStart) {
			segStart = periodStart
		}
		segEnd := periodEnd
		if end.Before(segEnd) {
			segEnd = end
		}
		if segEnd.After(segStart) {
			out = append(out, period{Start: periodStart, SegmentStart: segStart, SegmentEnd: segEnd})
		}
		cursor = periodEnd
	}
	return out
}

type period struct {
	// Start 是整点边界（fact_key 的 period_start）。
	Start time.Time
	// SegmentStart/SegmentEnd 是本段在小时内的实际重叠区间。
	SegmentStart time.Time
	SegmentEnd   time.Time
}

func (p period) seconds() int64 {
	d := p.SegmentEnd.Sub(p.SegmentStart)
	if d <= 0 {
		return 0
	}
	return int64(d / time.Second)
}

// FactKey 构造幂等键 `<fact_type>:<segment_id>:<period_start>`；
// period_start 为空时用 '-'（billing-00 §6.6 冻结约束 1）。
func FactKey(factType, segmentID string, periodStart *time.Time) string {
	period := "-"
	if periodStart != nil {
		period = periodStart.UTC().Format(time.RFC3339)
	}
	return factType + ":" + segmentID + ":" + period
}

// ReconciliationFactKeyBase 是发票调整事实的稳定前缀。
//
// 发票调整不能只按 (段, 周期) 做键：PeriodStart 可能为空，且同一周期可能
// 收到更正发票（新 BillReference / 新金额）。前缀纳入账单引用，实际键再
// 追加序号（见 ReconciliationFactKey）。
func ReconciliationFactKeyBase(segmentID string, periodStart *time.Time, billReference string) string {
	period := "-"
	if periodStart != nil {
		period = periodStart.UTC().Format(time.RFC3339)
	}
	return FactReconciliationAdjustment + ":" + segmentID + ":" + period + ":" + billReferenceToken(billReference)
}

// ReconciliationFactKey 在基键后追加序号，保证每次金额变化都有独立事实。
func ReconciliationFactKey(segmentID string, periodStart *time.Time, billReference string, sequence int) string {
	return fmt.Sprintf("%s:%d", ReconciliationFactKeyBase(segmentID, periodStart, billReference), sequence)
}

// billReferenceToken 把账单引用压成定长 token。
//
// 为什么用 hash 而不是原字符串：fact_key 以 ':' 分段，原引用里若含 ':'
// 会让「a」与「a:1」产生前缀歧义（序号前缀匹配会串组）；定长 hex 不会。
func billReferenceToken(billReference string) string {
	if billReference == "" {
		return "-"
	}
	sum := sha256.Sum256([]byte(billReference))
	return hex.EncodeToString(sum[:8])
}
