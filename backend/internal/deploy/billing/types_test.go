package billing

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"newtg/gateway/internal/deploy/ionet"
)

func at(month time.Month, day, hour, minute int) time.Time {
	return time.Date(2026, month, day, hour, minute, 0, 0, time.UTC)
}

// TestSplitPeriods 锁定「按墙钟整点切分」：
//   - 跨整点：10:30→12:15 产生 10:00、11:00、12:00 三个事实；
//   - 未闭合小时：closed 模式不写 12:00 桶（避免 ON CONFLICT 把短时长钉死）；
//   - 不足一小时：10:30→10:45 只产生一个 10:00 桶。
func TestSplitPeriods(t *testing.T) {
	start := at(time.October, 20, 10, 30)
	cases := []struct {
		name          string
		end           time.Time
		inclusiveOpen bool
		wantStarts    []time.Time
		wantSeconds   []int64
	}{
		{
			name:          "跨整点含开放小时",
			end:           at(time.October, 20, 12, 15),
			inclusiveOpen: true,
			wantStarts: []time.Time{
				at(time.October, 20, 10, 0),
				at(time.October, 20, 11, 0),
				at(time.October, 20, 12, 0),
			},
			wantSeconds: []int64{1800, 3600, 900},
		},
		{
			name:          "只写已闭合小时",
			end:           at(time.October, 20, 12, 15),
			inclusiveOpen: false,
			wantStarts: []time.Time{
				at(time.October, 20, 10, 0),
				at(time.October, 20, 11, 0),
			},
			wantSeconds: []int64{1800, 3600},
		},
		{
			name:          "不足一小时",
			end:           at(time.October, 20, 10, 45),
			inclusiveOpen: true,
			wantStarts:    []time.Time{at(time.October, 20, 10, 0)},
			wantSeconds:   []int64{900},
		},
		{
			name:          "恰好整点不产生空桶",
			end:           at(time.October, 20, 11, 0),
			inclusiveOpen: true,
			wantStarts:    []time.Time{at(time.October, 20, 10, 0)},
			wantSeconds:   []int64{1800},
		},
		{
			name:          "起止相同不产生事实",
			end:           start,
			inclusiveOpen: true,
			wantStarts:    nil,
			wantSeconds:   nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := splitPeriods(start, tc.end, tc.inclusiveOpen)
			if len(got) != len(tc.wantStarts) {
				t.Fatalf("periods = %d，want %d: %+v", len(got), len(tc.wantStarts), got)
			}
			for i, p := range got {
				if !p.Start.Equal(tc.wantStarts[i]) {
					t.Fatalf("period[%d].Start = %s，want %s", i, p.Start, tc.wantStarts[i])
				}
				if p.seconds() != tc.wantSeconds[i] {
					t.Fatalf("period[%d].seconds = %d，want %d", i, p.seconds(), tc.wantSeconds[i])
				}
			}
		})
	}
}

func TestFactKeyStable(t *testing.T) {
	p := at(time.October, 20, 10, 0)
	if got := FactKey(FactAccrual, "seg-1", &p); got != "accrual:seg-1:2026-10-20T10:00:00Z" {
		t.Fatalf("FactKey = %q", got)
	}
	if got := FactKey(FactSettlement, "seg-1", nil); got != "settlement:seg-1:-" {
		t.Fatalf("FactKey(nil) = %q", got)
	}
}

func TestClassifyStatus(t *testing.T) {
	cases := map[string]StatusClass{
		"running":    StatusRunning,
		"Running":    StatusRunning,
		"active":     StatusRunning,
		"terminated": StatusTerminal,
		"failed":     StatusTerminal,
		"deleted":    StatusTerminal,
		"weird":      StatusUnknown,
		"":           StatusUnknown,
	}
	for status, want := range cases {
		if got := ClassifyStatus(status); got != want {
			t.Fatalf("ClassifyStatus(%q) = %v，want %v", status, got, want)
		}
	}
}

func TestValidateEstimateBounds(t *testing.T) {
	base := SegmentEstimate{
		DeploymentID:             "d1",
		IdempotencyKey:           "k1",
		SegmentKind:              SegmentKindCreate,
		Name:                     "n1",
		HardwareID:               1,
		LocationIDs:              []int64{1},
		GPUsPerContainer:         1,
		ReplicaCount:             1,
		PlannedSeconds:           3600,
		QuoteUSD:                 decimal.NewFromInt(1),
		UnitPriceUSDPerGPUSecond: decimal.NewFromFloat(0.001),
		PriceSource:              PriceSourceProviderQuote,
	}
	if err := ValidateEstimate(normalizeEstimate(base)); err != nil {
		t.Fatalf("valid estimate rejected: %v", err)
	}
	bad := []struct {
		name string
		mut  func(*SegmentEstimate)
	}{
		{"gpu 上界", func(e *SegmentEstimate) { e.GPUsPerContainer = MaxDeploymentGPUsPerContainer + 1 }},
		{"replica 上界", func(e *SegmentEstimate) { e.ReplicaCount = MaxDeploymentReplicas + 1 }},
		{"时长上界", func(e *SegmentEstimate) { e.PlannedSeconds = MaxDeploymentDurationHours*3600 + 1 }},
		{"负报价", func(e *SegmentEstimate) { e.QuoteUSD = decimal.NewFromInt(-1) }},
		{"缺地域", func(e *SegmentEstimate) { e.LocationIDs = nil }},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			e := base
			tc.mut(&e)
			if err := ValidateEstimate(normalizeEstimate(e)); err == nil {
				t.Fatalf("invalid estimate accepted: %+v", e)
			}
		})
	}
}

func TestSegmentMaxGPUSecondsIncludesGrace(t *testing.T) {
	seg := Segment{GPUsPerContainer: 8, ReplicaCount: 64, PlannedSeconds: MaxDeploymentDurationHours * 3600}
	want := int64(MaxDeploymentGPUSecondsPerSegment)
	if got := SegmentMaxGPUSeconds(seg); got != want {
		t.Fatalf("SegmentMaxGPUSeconds = %d，want %d（必须含 300s 宽限）", got, want)
	}
}

// TestSnapshotRejectsUnavailablePrice 锁定 M3：报价缺失/负数一律阻断，
// 只有显式 0 且带报价标记才允许免费。
func TestSnapshotRejectsUnavailablePrice(t *testing.T) {
	base := ionet.DeploymentDetail{
		ID: "d1", HardwareID: 1, GPUsPerContainer: 1, TotalContainers: 1,
		ComputeMinutesServed: 60, Locations: []ionet.DeploymentLocation{{ID: 1}},
	}
	fake := &ionet.FakeProvider{}

	cases := []struct {
		name    string
		amount  string
		price   ionet.PriceEstimationResponse
		wantErr bool
	}{
		{"缺省报价", "0", ionet.PriceEstimationResponse{EstimationValid: true}, true},
		{"负报价", "0", ionet.PriceEstimationResponse{
			EstimatedCost:    ionet.Money{Decimal: decimal.NewFromInt(-1)},
			EstimatedCostSet: true, EstimationValid: true,
		}, true},
		{"估算无效", "0", ionet.PriceEstimationResponse{
			EstimatedCost:    ionet.Money{Decimal: decimal.NewFromInt(1)},
			EstimatedCostSet: true, EstimationValid: false,
		}, true},
		{"显式 0 免费", "0", ionet.PriceEstimationResponse{
			EstimatedCost:    ionet.Money{Decimal: decimal.Zero},
			EstimatedCostSet: true, EstimationValid: true,
		}, false},
		{"已付金额优先", "7.2", ionet.PriceEstimationResponse{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			detail := base
			detail.AmountPaid = ionet.Money{Decimal: decimal.RequireFromString(tc.amount)}
			fake.Price = tc.price
			snapshot, err := snapshotFromDetail(context.Background(), fake, detail)
			if tc.wantErr {
				if !errors.Is(err, ErrGPUPriceUnavailable) {
					t.Fatalf("err = %v，期望 ErrGPUPriceUnavailable", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if tc.amount == "0" && !snapshot.QuoteUSD.IsZero() {
				t.Fatalf("显式 0 报价应得到 0 quote，实际 %s", snapshot.QuoteUSD)
			}
			if tc.amount == "7.2" && !snapshot.QuoteUSD.Equal(decimal.RequireFromString("7.2")) {
				t.Fatalf("quote = %s", snapshot.QuoteUSD)
			}
		})
	}
}

// TestSnapshotDurationHoursIntegerCeil 锁定 M2：报价请求时长用整数向上取整。
func TestSnapshotDurationHoursIntegerCeil(t *testing.T) {
	fake := &ionet.FakeProvider{Price: ionet.PriceEstimationResponse{
		EstimatedCost:    ionet.Money{Decimal: decimal.NewFromInt(1)},
		EstimatedCostSet: true,
		EstimationValid:  true,
	}}
	detail := ionet.DeploymentDetail{
		ID: "d1", HardwareID: 1, GPUsPerContainer: 1, TotalContainers: 1,
		ComputeMinutesServed: 61, // 3660s → 2h（向上取整，不能用截断）
		Locations:            []ionet.DeploymentLocation{{ID: 1}},
	}
	if _, err := snapshotFromDetail(context.Background(), fake, detail); err != nil {
		t.Fatalf("snapshotFromDetail: %v", err)
	}
	if fake.LastPriceRequest.DurationHours != 2 {
		t.Fatalf("duration_hours = %d，want 2", fake.LastPriceRequest.DurationHours)
	}
}

// TestNormalizeUsageBounds 锁定 Settle 的乘数快照与段上界校验。
func TestNormalizeUsageBounds(t *testing.T) {
	ledger := &Ledger{}
	seg := Segment{SegmentID: "s1", DeploymentID: "d1", GPUsPerContainer: 2, ReplicaCount: 3, PlannedSeconds: 3600}
	valid := GPUUsageFact{GPUSeconds: SegmentMaxGPUSeconds(seg)}
	if _, err := ledger.normalizeUsage(seg, valid); err != nil {
		t.Fatalf("上界内用量被拒: %v", err)
	}
	over := GPUUsageFact{GPUSeconds: SegmentMaxGPUSeconds(seg) + 1}
	if _, err := ledger.normalizeUsage(seg, over); !errors.Is(err, ErrInvalidSegment) {
		t.Fatalf("超上界用量 err = %v", err)
	}
	mismatch := GPUUsageFact{GPUSeconds: 100, GPUsPerContainer: 1}
	if _, err := ledger.normalizeUsage(seg, mismatch); !errors.Is(err, ErrInvalidSegment) {
		t.Fatalf("乘数不一致 err = %v", err)
	}
}
