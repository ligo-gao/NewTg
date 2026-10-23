package pricing

import (
	"context"
	"errors"
	"testing"
	"time"

	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/pricingstrategy"
)

// fakeHolidayLookup 是 pricingstrategy.HolidayLookup 的可控实现。
type fakeHolidayLookup struct {
	holiday pricingstrategy.Holiday
	ok      bool
	err     error
	calls   int
}

func (f *fakeHolidayLookup) LookupHoliday(_ context.Context, _ string, _ time.Time) (pricingstrategy.Holiday, bool, error) {
	f.calls++
	return f.holiday, f.ok, f.err
}

// deepSeekStrategyJSON 是 spec §5.1 的 DeepSeek 形态：工作日 09–12 高峰 1.0，
// 其余时段 0.5，法定假日全天按低谷。
const deepSeekStrategyJSON = `{
  "version": 1,
  "time": {
    "enabled": true,
    "timezone": "Asia/Shanghai",
    "holiday_policy": "cn_offpeak",
    "default_multiplier": 0.5,
    "windows": [
      {"name": "上午高峰", "days": [1,2,3,4,5], "start": "09:00", "end": "12:00", "multiplier": 1.0}
    ]
  }
}`

func shanghaiTime(t *testing.T, year int, month time.Month, day, hour int) time.Time {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("load Asia/Shanghai: %v", err)
	}
	return time.Date(year, month, day, hour, 0, 0, 0, loc)
}

func strategyRow(strategy string) modelPricingRow {
	return modelPricingRow{Model: "deepseek-flash", Strategy: []byte(strategy), Status: 1}
}

// TestApplyTimeStrategyPeakAndOffPeak 验证窗口内 1.0、窗口外 0.5，
// 且只有五个绝对单价字段被缩放。
func TestApplyTimeStrategyPeakAndOffPeak(t *testing.T) {
	src := &DBSource{}
	row := strategyRow(deepSeekStrategyJSON)
	snap := ratioSnapshot(200, 800, 4, 250)
	snap.Price.CacheCreationRatio = 250

	peak, err := src.applyTimeStrategy(context.Background(), snap, row, row.Model,
		shanghaiTime(t, 2026, time.October, 26, 10)) // 周一 10:00
	if err != nil {
		t.Fatalf("applyTimeStrategy(peak): %v", err)
	}
	if peak.TimeMultiplier != 1.0 {
		t.Fatalf("peak TimeMultiplier = %v, want 1", peak.TimeMultiplier)
	}
	if peak.TimeWindow != "上午高峰" {
		t.Fatalf("peak TimeWindow = %q, want 上午高峰", peak.TimeWindow)
	}
	if peak.Price.ModelRatio != 200 || peak.Price.CompletionRatio != 800 ||
		peak.Price.CacheRatio != 4 || peak.Price.CacheCreation5mRatio != 250 {
		t.Fatalf("peak 单价被改动: %+v", peak.Price)
	}

	off, err := src.applyTimeStrategy(context.Background(), snap, row, row.Model,
		shanghaiTime(t, 2026, time.October, 26, 21)) // 周一 21:00
	if err != nil {
		t.Fatalf("applyTimeStrategy(off-peak): %v", err)
	}
	if off.TimeMultiplier != 0.5 {
		t.Fatalf("off-peak TimeMultiplier = %v, want 0.5", off.TimeMultiplier)
	}
	if off.TimeWindow != "" {
		t.Fatalf("off-peak TimeWindow = %q, want empty", off.TimeWindow)
	}
	want := map[string]float64{
		"model_ratio":            100,
		"completion_ratio":       400,
		"cache_ratio":            2,
		"cache_creation_5m":      125,
		"cache_creation_1h":      125,
		"cache_creation_base":    250, // 基准只用于派生 5m/1h，不在缩放字段里
		"image_ratio":            1,
		"audio_ratio":            1,
		"audio_completion_ratio": 1,
	}
	got := map[string]float64{
		"model_ratio":            off.Price.ModelRatio,
		"completion_ratio":       off.Price.CompletionRatio,
		"cache_ratio":            off.Price.CacheRatio,
		"cache_creation_5m":      off.Price.CacheCreation5mRatio,
		"cache_creation_1h":      off.Price.CacheCreation1hRatio,
		"cache_creation_base":    off.Price.CacheCreationRatio,
		"image_ratio":            off.Price.ImageRatio,
		"audio_ratio":            off.Price.AudioRatio,
		"audio_completion_ratio": off.Price.AudioCompletionRatio,
	}
	for key, w := range want {
		if got[key] != w {
			t.Fatalf("%s = %v, want %v（全字段: %+v）", key, got[key], w, off.Price)
		}
	}
}

// TestApplyTimeStrategyPerCall 验证按次价被缩放。
func TestApplyTimeStrategyPerCall(t *testing.T) {
	src := &DBSource{}
	row := strategyRow(deepSeekStrategyJSON)
	snap := contract.PriceSnapshot{
		Mode: contract.BillingModePerCall,
		Price: contract.PriceData{
			UsePrice: true, ModelPrice: 100,
		},
		GroupRatio: 1,
	}
	out, err := src.applyTimeStrategy(context.Background(), snap, row, row.Model,
		shanghaiTime(t, 2026, time.October, 26, 21))
	if err != nil {
		t.Fatalf("applyTimeStrategy: %v", err)
	}
	if out.Price.ModelPrice != 50 {
		t.Fatalf("per-call price = %v, want 50", out.Price.ModelPrice)
	}
	if out.TimeMultiplier != 0.5 {
		t.Fatalf("TimeMultiplier = %v, want 0.5", out.TimeMultiplier)
	}
}

// TestApplyTimeStrategyExplicitZeroSentinel 验证 -1 哨兵（显式免费）不被乘，
// 其余字段正常缩放。乘 -1 会变成正价（0.5 × -1 = -0.5 被拒绝），乘 0 会
// 把显式免费伪装成「未设置」。
func TestApplyTimeStrategyExplicitZeroSentinel(t *testing.T) {
	src := &DBSource{}
	row := strategyRow(deepSeekStrategyJSON)
	snap := ratioSnapshot(200, 800, 4, 250)
	snap.Price.CompletionRatio = -1

	out, err := src.applyTimeStrategy(context.Background(), snap, row, row.Model,
		shanghaiTime(t, 2026, time.October, 26, 21))
	if err != nil {
		t.Fatalf("applyTimeStrategy: %v", err)
	}
	if out.Price.CompletionRatio != -1 {
		t.Fatalf("explicit zero sentinel = %v, want -1", out.Price.CompletionRatio)
	}
	if out.Price.ModelRatio != 100 {
		t.Fatalf("model ratio = %v, want 100", out.Price.ModelRatio)
	}
}

// TestApplyTimeStrategyRejectsExpressionConflict 验证 strategy 与 billing_expr
// 同时存在按配置错误处理（spec §5.2 第 4 条）。
func TestApplyTimeStrategyRejectsExpressionConflict(t *testing.T) {
	src := &DBSource{}
	expr := "p * 1"
	row := strategyRow(deepSeekStrategyJSON)
	row.BillingExpr = &expr

	_, err := src.applyTimeStrategy(context.Background(), ratioSnapshot(200, 800, 4, 250),
		row, row.Model, shanghaiTime(t, 2026, time.October, 26, 10))
	if !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("err = %v, want ErrInvalidSpec", err)
	}
}

// TestApplyTimeStrategyDisabledLeavesSnapshotUntouched 验证 time.enabled=false
// 时快照逐字段不变（TimeMultiplier 保持 0，日志不出现时段键）。
func TestApplyTimeStrategyDisabledLeavesSnapshotUntouched(t *testing.T) {
	src := &DBSource{}
	row := strategyRow(`{"version":1,"time":{"enabled":false}}`)
	snap := ratioSnapshot(200, 800, 4, 250)

	out, err := src.applyTimeStrategy(context.Background(), snap, row, row.Model,
		shanghaiTime(t, 2026, time.October, 26, 21))
	if err != nil {
		t.Fatalf("applyTimeStrategy: %v", err)
	}
	if out.TimeMultiplier != 0 || out.TimeWindow != "" || out.HolidayName != "" {
		t.Fatalf("disabled strategy 写入了时段字段: %+v", out)
	}
	if out.Price.ModelRatio != 200 || out.Price.CompletionRatio != 800 {
		t.Fatalf("disabled strategy 改动了价格: %+v", out.Price)
	}
}

// TestApplyTimeStrategyHolidayOffDay 验证法定假日全天按 default_multiplier。
func TestApplyTimeStrategyHolidayOffDay(t *testing.T) {
	lookup := &fakeHolidayLookup{holiday: pricingstrategy.Holiday{Name: "国庆节", OffDay: true}, ok: true}
	src := &DBSource{holidayLookup: lookup}
	row := strategyRow(deepSeekStrategyJSON)

	// 2026-10-01 是周四，10:00 本来是高峰窗口。
	out, err := src.applyTimeStrategy(context.Background(), ratioSnapshot(200, 800, 4, 250),
		row, row.Model, shanghaiTime(t, 2026, time.October, 1, 10))
	if err != nil {
		t.Fatalf("applyTimeStrategy: %v", err)
	}
	if out.TimeMultiplier != 0.5 {
		t.Fatalf("法定假日 multiplier = %v, want 0.5", out.TimeMultiplier)
	}
	if out.TimeWindow != "" {
		t.Fatalf("法定假日不应命中窗口，got %q", out.TimeWindow)
	}
	if out.HolidayName != "国庆节" {
		t.Fatalf("HolidayName = %q, want 国庆节", out.HolidayName)
	}
	if out.Price.ModelRatio != 100 {
		t.Fatalf("model ratio = %v, want 100", out.Price.ModelRatio)
	}
}

// TestApplyTimeStrategyMakeupWorkday 验证调休上班日（周六）按工作日窗口计价，
// 而不是周末全天低谷。2026-10-10 是国庆后的调休上班日。
func TestApplyTimeStrategyMakeupWorkday(t *testing.T) {
	lookup := &fakeHolidayLookup{holiday: pricingstrategy.Holiday{Name: "国庆节", OffDay: false}, ok: true}
	src := &DBSource{holidayLookup: lookup}
	row := strategyRow(deepSeekStrategyJSON)

	out, err := src.applyTimeStrategy(context.Background(), ratioSnapshot(200, 800, 4, 250),
		row, row.Model, shanghaiTime(t, 2026, time.October, 10, 10))
	if err != nil {
		t.Fatalf("applyTimeStrategy: %v", err)
	}
	if out.TimeMultiplier != 1.0 {
		t.Fatalf("调休上班日 10:00 multiplier = %v, want 工作日窗口 1.0", out.TimeMultiplier)
	}
	if out.TimeWindow != "上午高峰" {
		t.Fatalf("TimeWindow = %q, want 上午高峰", out.TimeWindow)
	}
}

// TestApplyTimeStrategyHolidayLookupErrorFallsBack 验证日历不可达时按星期
// 规则继续，不让定价失败，也不退化为免费。
func TestApplyTimeStrategyHolidayLookupErrorFallsBack(t *testing.T) {
	lookup := &fakeHolidayLookup{err: errors.New("db down")}
	src := &DBSource{holidayLookup: lookup}
	row := strategyRow(deepSeekStrategyJSON)

	out, err := src.applyTimeStrategy(context.Background(), ratioSnapshot(200, 800, 4, 250),
		row, row.Model, shanghaiTime(t, 2026, time.October, 26, 21))
	if err != nil {
		t.Fatalf("applyTimeStrategy: %v", err)
	}
	if out.TimeMultiplier != 0.5 {
		t.Fatalf("lookup 失败后 multiplier = %v, want 星期规则 0.5", out.TimeMultiplier)
	}
	if lookup.calls != 1 {
		t.Fatalf("lookup calls = %d, want 1", lookup.calls)
	}
}

// TestApplyTimeStrategyRejectsMalformedJSON 验证非法策略 JSON 是配置错误。
func TestApplyTimeStrategyRejectsMalformedJSON(t *testing.T) {
	src := &DBSource{}
	_, err := src.applyTimeStrategy(context.Background(), ratioSnapshot(200, 800, 4, 250),
		strategyRow(`{"version":1,`), "m", shanghaiTime(t, 2026, time.October, 26, 10))
	if !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("err = %v, want ErrInvalidSpec", err)
	}
}

// TestScaleSnapshotByTimeUsesFrozenMultiplier 验证 ResolveFixed 的核心缩放：
// 冻结倍率直接乘到基础快照，不查节假日、不按当前时间匹配窗口。
func TestScaleSnapshotByTimeUsesFrozenMultiplier(t *testing.T) {
	out, err := scaleSnapshotByTime(ratioSnapshot(200, 800, 4, 250), 0.5, "", "")
	if err != nil {
		t.Fatalf("scaleSnapshotByTime: %v", err)
	}
	if out.Price.ModelRatio != 100 || out.TimeMultiplier != 0.5 || out.TimeWindow != "" {
		t.Fatalf("ResolveFixed 结果不符: %+v", out)
	}
}

// TestScaleSnapshotByTimeRejectsInvalidMultiplier 验证越界/非有限倍率被拒绝。
func TestScaleSnapshotByTimeRejectsInvalidMultiplier(t *testing.T) {
	for _, m := range []float64{0, -0.5, 100.1, 1e9} {
		if _, err := scaleSnapshotByTime(ratioSnapshot(200, 800, 4, 250), m, "", ""); !errors.Is(err, ErrInvalidPrice) {
			t.Fatalf("multiplier %v: err = %v, want ErrInvalidPrice", m, err)
		}
	}
}
