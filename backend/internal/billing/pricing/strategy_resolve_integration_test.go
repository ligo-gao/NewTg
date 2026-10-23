//go:build integration

package pricing

// 结构化时段策略的数据库级验收（spec §5.2）：真实 PostgreSQL + jsonb 列。
//
// 覆盖单测无法证明的接缝：
//   - lookupModel 的 `strategy` jsonb 能扫描进 modelPricingRow.Strategy；
//   - ResolveAt 在基础价（价列 + 分组倍率）之后应用时段倍率；
//   - 节假日查表通过 SetHolidayLookup 注入，法定假日走 default_multiplier。
//
// 运行（隔离库，先应用 model_pricing_strategy 迁移）：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_test_verify_t4?sslmode=disable' \
//	  go test -tags=integration ./internal/billing/pricing/ -run TestResolveAtStrategy -v

import (
	"context"
	"testing"
	"time"

	"newtg/gateway/internal/pricingstrategy"
)

type integrationHolidayLookup struct {
	holiday pricingstrategy.Holiday
	ok      bool
}

func (f integrationHolidayLookup) LookupHoliday(context.Context, string, time.Time) (pricingstrategy.Holiday, bool, error) {
	return f.holiday, f.ok, nil
}

func TestResolveAtStrategyPeakOffPeakAndHoliday(t *testing.T) {
	db := openTestDB(t)
	prefix := testPrefix(t)
	cleanupPrefix(t, db, prefix)
	model := prefix + "-deepseek"
	group := prefix + "g"

	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS model_pricing (
			id bigserial PRIMARY KEY,
			model text NOT NULL UNIQUE,
			mode smallint NOT NULL DEFAULT 0,
			billing_expr text,
			prompt_price bigint,
			completion_price bigint,
			cache_read_price bigint,
			cache_write_price bigint,
			status smallint NOT NULL DEFAULT 1,
			strategy jsonb,
			created_at timestamptz NOT NULL DEFAULT now(),
			updated_at timestamptz NOT NULL DEFAULT now())`,
		`CREATE TABLE IF NOT EXISTS group_pricing (
			id bigserial PRIMARY KEY,
			"group" text NOT NULL,
			model text NOT NULL,
			prompt_price bigint NOT NULL DEFAULT 0,
			completion_price bigint NOT NULL DEFAULT 0,
			cache_read_price bigint NOT NULL DEFAULT 0,
			cache_write_price bigint NOT NULL DEFAULT 0,
			status smallint NOT NULL DEFAULT 1)`,
		`CREATE TABLE IF NOT EXISTS user_groups (
			name text PRIMARY KEY,
			ratio integer NOT NULL DEFAULT 100,
			status smallint NOT NULL DEFAULT 1)`,
	} {
		if err := db.Exec(stmt).Error; err != nil {
			t.Fatalf("ensure schema: %v", err)
		}
	}

	if err := db.Exec(`INSERT INTO user_groups (name, ratio, status) VALUES (?, 100, 1)`, group).Error; err != nil {
		t.Fatalf("insert user_groups: %v", err)
	}
	if err := db.Exec(`INSERT INTO model_pricing
		(model, mode, prompt_price, completion_price, cache_read_price, cache_write_price, status, strategy)
		VALUES (?, 0, 200, 800, 4, 250, 1, ?::jsonb)`,
		model, deepSeekStrategyJSON).Error; err != nil {
		t.Fatalf("insert model_pricing: %v", err)
	}

	src, err := NewSource(db, nil)
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("load Asia/Shanghai: %v", err)
	}

	peak, err := src.ResolveAt(context.Background(), model, group,
		time.Date(2026, time.October, 26, 10, 0, 0, 0, loc)) // 周一 10:00
	if err != nil {
		t.Fatalf("ResolveAt(peak): %v", err)
	}
	if peak.TimeMultiplier != 1.0 || peak.TimeWindow != "上午高峰" {
		t.Fatalf("peak = multiplier %v window %q, want 1/上午高峰",
			peak.TimeMultiplier, peak.TimeWindow)
	}
	if peak.Price.ModelRatio != 200 || peak.Price.CacheRatio != 4 {
		t.Fatalf("peak price = %+v, want 基础价不变", peak.Price)
	}

	off, err := src.ResolveAt(context.Background(), model, group,
		time.Date(2026, time.October, 26, 21, 0, 0, 0, loc)) // 周一 21:00
	if err != nil {
		t.Fatalf("ResolveAt(off-peak): %v", err)
	}
	if off.TimeMultiplier != 0.5 || off.TimeWindow != "" {
		t.Fatalf("off-peak = multiplier %v window %q, want 0.5/空",
			off.TimeMultiplier, off.TimeWindow)
	}
	if off.Price.ModelRatio != 100 || off.Price.CompletionRatio != 400 ||
		off.Price.CacheRatio != 2 || off.Price.CacheCreation5mRatio != 125 {
		t.Fatalf("off-peak price = %+v, want 五折", off.Price)
	}
	// 缓存创建基准不在缩放字段里（5m/1h 才参与计费）。
	if off.Price.CacheCreationRatio != 250 {
		t.Fatalf("CacheCreationRatio = %v, want 250（不应被乘）", off.Price.CacheCreationRatio)
	}

	// 法定假日：周四 10:00 本应是高峰，但全天按 default_multiplier。
	src.SetHolidayLookup(integrationHolidayLookup{
		holiday: pricingstrategy.Holiday{Name: "国庆节", OffDay: true},
		ok:      true,
	})
	holiday, err := src.ResolveAt(context.Background(), model, group,
		time.Date(2026, time.October, 1, 10, 0, 0, 0, loc))
	if err != nil {
		t.Fatalf("ResolveAt(holiday): %v", err)
	}
	if holiday.TimeMultiplier != 0.5 || holiday.TimeWindow != "" || holiday.HolidayName != "国庆节" {
		t.Fatalf("holiday = multiplier %v window %q name %q, want 0.5/空/国庆节",
			holiday.TimeMultiplier, holiday.TimeWindow, holiday.HolidayName)
	}
}
