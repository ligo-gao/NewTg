package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"newtg/gateway/internal/deploy/ionet"
)

// ProviderFunc 按当前设置构造 provider 客户端。
//
// 每次 pass 重新构造而不是启动时缓存：`deploy.ionet.api_key` 可在管理面
// 轮换（modules-08 §3.20.10），缓存旧客户端会让 worker 一直用失效密钥；
// enabled=false 或密钥为空时返回 ok=false（worker 仍对已有段做内部记账）。
type ProviderFunc func(ctx context.Context) (provider ionet.DeploymentProvider, ok bool, err error)

// StatusClass 是 provider 部署状态的三分类。
type StatusClass int

const (
	// StatusUnknown 表示状态未实测/无法归类：保持当前账，不确认成本。
	StatusUnknown StatusClass = iota
	// StatusRunning 表示部署仍在运行，可以继续累计。
	StatusRunning
	// StatusTerminal 表示部署已停止/失败，可以终态结算或释放。
	StatusTerminal
)

// ClassifyStatus 归一化 provider 状态字符串（adapter 已 lower-case）。
//
// 真实状态词表未实测（R-M6D-13），因此集合宁可多含常见同义词；未命中的
// 状态按 unknown 处理——绝不能把未知状态当成成功结算。
func ClassifyStatus(status string) StatusClass {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "running", "active", "ready", "deployed", "started", "in_progress":
		return StatusRunning
	case "terminated", "stopped", "deleted", "completed", "finished",
		"failed", "error", "cancelled", "canceled", "expired":
		return StatusTerminal
	default:
		return StatusUnknown
	}
}

// DeploymentSnapshot 是 worker 从 provider 详情推导出的计费输入。
type DeploymentSnapshot struct {
	DeploymentID     string
	Status           string
	Class            StatusClass
	Name             string
	HardwareID       int64
	LocationIDs      []int64
	GPUsPerContainer int
	ReplicaCount     int
	PlannedSeconds   int64
	StartedAt        *time.Time
	FinishedAt       *time.Time
	QuoteUSD         decimal.Decimal
	ComputeSeconds   int64
}

// snapshotFromDetail 把 provider 详情转换为计费输入；报价缺失时回退到
// EstimatePrice（provider 价格估算）。
//
// 计价口径：unit_price = quote / (gpus × replicas × planned_seconds)，
// 与 billing-02 §5.3 的「报价除成每 GPU·秒」一致；金额全程 decimal，
// 不经过 float64 的乘积（详细计算见 estimateFromSnapshot）。
func snapshotFromDetail(ctx context.Context, provider ionet.DeploymentProvider, detail ionet.DeploymentDetail) (DeploymentSnapshot, error) {
	if detail.ID == "" {
		return DeploymentSnapshot{}, errors.New("deploy billing: provider detail has no id")
	}
	gpus := detail.GPUsPerContainer
	replicas := detail.TotalContainers
	if gpus <= 0 && replicas > 0 && detail.TotalGPUs > 0 {
		gpus = detail.TotalGPUs / replicas
	}
	if gpus <= 0 {
		gpus = 1
	}
	if replicas <= 0 {
		replicas = 1
	}
	planned := int64(detail.ComputeMinutesServed+detail.ComputeMinutesRemaining) * 60
	if planned <= 0 {
		// provider 没给运行时长时按 1 小时估算；这只是段上界的输入，
		// 实际计费按 elapsed 计算（billing-05 §4.4.4 第 5 条）。
		planned = 3600
	}
	maxPlanned := int64(MaxDeploymentDurationHours) * 3600
	if planned > maxPlanned {
		planned = maxPlanned
	}
	started := detail.StartedAt
	if started == nil {
		started = detail.CreatedAt
	}
	quote := decimal.NewFromFloat(detail.AmountPaid)
	if !quote.IsPositive() {
		hours := int(math.Ceil(float64(planned) / 3600))
		if hours < 1 {
			hours = 1
		}
		price, err := provider.EstimatePrice(ctx, ionet.PriceEstimationRequest{
			HardwareID:       detail.HardwareID,
			LocationIDs:      intLocations(detail.Locations),
			GPUsPerContainer: gpus,
			ReplicaCount:     replicas,
			DurationHours:    hours,
			Currency:         "USD",
		})
		if err != nil {
			return DeploymentSnapshot{}, fmt.Errorf("deploy billing: estimate price: %w", err)
		}
		switch strings.ToUpper(strings.TrimSpace(price.Currency)) {
		case "", "USD", "USDC":
		default:
			return DeploymentSnapshot{}, fmt.Errorf("deploy billing: unsupported currency %q", price.Currency)
		}
		quote = decimal.NewFromFloat(price.EstimatedCost)
	}
	if quote.IsNegative() {
		quote = decimal.Zero
	}
	locations := locationIDs(detail.Locations)
	if len(locations) == 0 {
		return DeploymentSnapshot{}, errors.New("deploy billing: provider detail has no locations")
	}
	return DeploymentSnapshot{
		DeploymentID:     detail.ID,
		Status:           detail.Status,
		Class:            ClassifyStatus(detail.Status),
		Name:             firstNonEmpty(detail.ID),
		HardwareID:       int64(detail.HardwareID),
		LocationIDs:      locations,
		GPUsPerContainer: gpus,
		ReplicaCount:     replicas,
		PlannedSeconds:   planned,
		StartedAt:        started,
		FinishedAt:       detail.FinishedAt,
		QuoteUSD:         quote,
		ComputeSeconds:   int64(detail.ComputeMinutesServed) * 60,
	}, nil
}

// estimateFromSnapshot 生成幂等段估算。
//
// provider 观察到的同一部署同一开始时刻必须得到同一个 Idempotency-Key：
// worker 重启/多副本重复扫描只会命中既有段（ON CONFLICT），不会二次预扣。
func estimateFromSnapshot(s DeploymentSnapshot, now time.Time) (SegmentEstimate, error) {
	started := s.StartedAt
	if started == nil {
		// 没有起点就没有可累计的计费量；用观测时刻占位并保持
		// pending_confirm，等 provider 给出 started_at 后再转 running。
		v := now.UTC()
		started = &v
	}
	plannedGPUSeconds := int64(s.GPUsPerContainer) * int64(s.ReplicaCount) * s.PlannedSeconds
	if plannedGPUSeconds <= 0 {
		return SegmentEstimate{}, errors.New("deploy billing: planned gpu seconds is zero")
	}
	quote := s.QuoteUSD
	if quote.IsNegative() {
		quote = decimal.Zero
	}
	unitPrice := quote.Div(decimal.NewFromInt(plannedGPUSeconds))
	state := SegmentPendingConfirm
	if s.StartedAt != nil && s.Class == StatusRunning {
		state = SegmentRunning
	}
	return SegmentEstimate{
		DeploymentID:             s.DeploymentID,
		IdempotencyKey:           fmt.Sprintf("deploy-observed:%s:%d", s.DeploymentID, started.Unix()),
		SegmentKind:              SegmentKindCreate,
		Name:                     s.Name,
		HardwareID:               s.HardwareID,
		LocationIDs:              s.LocationIDs,
		GPUsPerContainer:         s.GPUsPerContainer,
		ReplicaCount:             s.ReplicaCount,
		PlannedSeconds:           s.PlannedSeconds,
		StartedAt:                s.StartedAt,
		QuoteUSD:                 quote,
		UnitPriceUSDPerGPUSecond: unitPrice,
		PriceSource:              PriceSourceProviderQuote,
		State:                    state,
	}, nil
}

func locationIDs(locations []ionet.DeploymentLocation) []int64 {
	out := make([]int64, 0, len(locations))
	for _, loc := range locations {
		if loc.ID > 0 {
			out = append(out, int64(loc.ID))
		}
	}
	return out
}

// intLocations 是 provider 报价请求的 []int 形状（ionet 契约使用 int）。
func intLocations(locations []ionet.DeploymentLocation) []int {
	out := make([]int, 0, len(locations))
	for _, loc := range locations {
		if loc.ID > 0 {
			out = append(out, loc.ID)
		}
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// SettingsReader 是配置中心快照的只读接缝（生产为 *configcenter.Center）。
type SettingsReader interface {
	Get(key string) (json.RawMessage, bool)
}

// SecretOpener 解封密钥类设置（生产为 auth.SecretCipher）。
type SecretOpener interface {
	Open(sealed string) (string, error)
}

// 设置键是冻结名（modules-08 §3.20.10），不得改成第二套别名。
const (
	DeployEnabledKey = "deploy.ionet.enabled"
	DeployAPIKeyKey  = "deploy.ionet.api_key"
)

// ProviderFromSettings 按配置中心快照构造 provider 客户端。
//
// 密钥在快照中以密文保存，这里用与 gateway 相同的 Sealer 解封；解密失败
// 不视为「未配置」（否则会静默停止计费），而是返回错误让 worker 记录并
// 在下一轮重试。
func ProviderFromSettings(settings SettingsReader, opener SecretOpener, log *slog.Logger) ProviderFunc {
	return func(ctx context.Context) (ionet.DeploymentProvider, bool, error) {
		_ = ctx
		if settings == nil {
			return nil, false, nil
		}
		raw, ok := settings.Get(DeployEnabledKey)
		if !ok {
			return nil, false, nil
		}
		var enabled bool
		if err := json.Unmarshal(raw, &enabled); err != nil {
			return nil, false, fmt.Errorf("deploy billing: parse %s: %w", DeployEnabledKey, err)
		}
		if !enabled {
			return nil, false, nil
		}
		rawKey, ok := settings.Get(DeployAPIKeyKey)
		if !ok || opener == nil {
			return nil, false, nil
		}
		var sealed string
		if err := json.Unmarshal(rawKey, &sealed); err != nil {
			return nil, false, fmt.Errorf("deploy billing: parse %s: %w", DeployAPIKeyKey, err)
		}
		if sealed == "" {
			return nil, false, nil
		}
		apiKey, err := opener.Open(sealed)
		if err != nil {
			return nil, false, fmt.Errorf("deploy billing: open api key: %w", err)
		}
		if apiKey == "" {
			return nil, false, nil
		}
		return ionet.NewClient(ionet.Config{APIKey: apiKey, Logger: log}), true, nil
	}
}
