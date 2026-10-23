//go:build integration

package httpapi

// 模型定价时段策略的端到端验收（计划 Task 9）。
//
// 覆盖前面各任务单独测不到、但必须由同一份数据串起来的真相：
//   - 管理面写入的 strategy jsonb 被真实 DBSource 读取；
//   - 高峰（10:00）与闲时（21:00）的生效输入价严格 2:1，且冻结的
//     TimeMultiplier 分别是 1.0 / 0.5（不是只改展示、不是只改结算）；
//   - 节假日走真实 holiday_calendar：法定假日全天按 default_multiplier，
//     调休上班日按工作日窗口（周六 10:00 命中工作日高峰）；
//   - 配置了时段策略的模型仍出现在 GET /api/pricing（strategy 不写
//     billing_expr，公开可见条件不变），列表与详情都带 pricing_schedule。
//
// 「固定时钟」由把请求时间显式传给 ResolveAt 实现：计费会话冻结的正是这个
// 入参，测试不需要伪造 time.Now()（spec §5.2 的冻结时间不变量）。
//
// 运行（隔离库，确保已应用全部迁移，尤其是 20261027090000_model_pricing_strategy）：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_test?sslmode=disable' \
//	  go test -tags=integration ./internal/httpapi/ -run PricingStrategy -v

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	_ "github.com/jackc/pgx/v5/stdlib"

	"newtg/gateway/internal/billing/pricing"
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/holiday"
	"newtg/gateway/internal/store"
)

// pricingStrategyIntegrationJSON 是计划 Task 9 指定的策略：
// 工作日 09:00–12:00 高峰 1.0，其余时段 0.5，法定假日按低谷。
const pricingStrategyIntegrationJSON = `{
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

// holidayCalendarSnapshot 保存测试前的既有行，用于恢复（测试库可能已有真实
// 同步数据，不把测试事实永久写进共享库）。
type holidayCalendarSnapshot struct {
	exists bool
	offDay bool
	name   string
	source string
	synced time.Time
}

func TestPricingStrategyIntegrationPeakOffPeakAndPublicVisibility(t *testing.T) {
	dsn := os.Getenv("NEWTG_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("NEWTG_TEST_DB_DSN 未设置，跳过集成测试")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		t.Fatalf("ping test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	// ping 的 3s 超时只用于快速失败；后续查询与 Resolve 不能被这个 deadline
	// 限制（慢 CI 上整个用例可能超过 3s，否则会得到误导性的 context deadline）。
	ctx = context.Background()

	model := fmt.Sprintf("it-strategy-%d", time.Now().UnixNano())

	// 1) 定价行：基础价 200/800/4/250，加时段策略。
	if _, err := db.Exec(`INSERT INTO model_pricing
		(model, mode, prompt_price, completion_price, cache_read_price, cache_write_price, status, strategy)
		VALUES ($1, 0, 200, 800, 4, 250, 1, $2::jsonb)`,
		model, pricingStrategyIntegrationJSON); err != nil {
		t.Fatalf("insert model_pricing: %v", err)
	}
	t.Cleanup(func() {
		if _, err := db.Exec(`DELETE FROM model_pricing WHERE model = $1`, model); err != nil {
			t.Errorf("cleanup model_pricing(%s): %v", model, err)
		}
	})

	// 2) 节假日事实：2026-10-01（周四）国庆法定假日；
	//    2026-10-10（周六）调休上班日。先快照旧值，测试后恢复。
	const (
		offDayDate = "2026-10-01"
		makeupDate = "2026-10-10"
		offDayName = "国庆节"
		makeupName = "国庆节（调休上班）"
	)
	restore := make(map[string]holidayCalendarSnapshot, 2)
	for _, date := range []string{offDayDate, makeupDate} {
		snapshot, err := loadHolidayCalendarRow(ctx, db, date)
		if err != nil {
			t.Fatalf("read holiday_calendar(%s): %v", date, err)
		}
		restore[date] = snapshot
	}
	t.Cleanup(func() {
		for date, snapshot := range restore {
			if snapshot.exists {
				_, err := db.Exec(`UPDATE holiday_calendar
					SET is_off_day = $2, name = $3, source = $4, synced_at = $5
					WHERE country = 'CN' AND date = $1::date`,
					date, snapshot.offDay, snapshot.name, snapshot.source, snapshot.synced)
				if err != nil {
					t.Errorf("restore holiday_calendar(%s): %v", date, err)
				}
				continue
			}
			if _, err := db.Exec(`DELETE FROM holiday_calendar WHERE country = 'CN' AND date = $1::date`, date); err != nil {
				t.Errorf("cleanup holiday_calendar(%s): %v", date, err)
			}
		}
	})
	for _, row := range []struct {
		date   string
		offDay bool
		name   string
	}{{offDayDate, true, offDayName}, {makeupDate, false, makeupName}} {
		if _, err := db.Exec(`INSERT INTO holiday_calendar (country, date, is_off_day, name, source)
			VALUES ('CN', $1::date, $2, $3, 'integration-test')
			ON CONFLICT (country, date) DO UPDATE
			SET is_off_day = EXCLUDED.is_off_day, name = EXCLUDED.name,
			    source = EXCLUDED.source, synced_at = now()`,
			row.date, row.offDay, row.name); err != nil {
			t.Fatalf("seed holiday_calendar(%s): %v", row.date, err)
		}
	}

	// 3) 真实定价源 + 真实 holiday.Store（与生产装配同一接缝）。
	gormDB, err := gorm.Open(postgres.New(postgres.Config{Conn: db}), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm open: %v", err)
	}
	source, err := pricing.NewSource(gormDB, nil)
	if err != nil {
		t.Fatalf("pricing.NewSource: %v", err)
	}
	source.SetHolidayLookup(holiday.NewStore(db))

	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("load Asia/Shanghai: %v", err)
	}
	at := func(year int, month time.Month, day, hour int) time.Time {
		return time.Date(year, month, day, hour, 0, 0, 0, loc)
	}

	// 4) 高峰 vs 闲时：2026-10-26 是周一。
	peak, err := source.ResolveAt(ctx, model, "default", at(2026, time.October, 26, 10))
	if err != nil {
		t.Fatalf("ResolveAt(peak): %v", err)
	}
	offPeak, err := source.ResolveAt(ctx, model, "default", at(2026, time.October, 26, 21))
	if err != nil {
		t.Fatalf("ResolveAt(off-peak): %v", err)
	}
	if peak.TimeMultiplier != 1.0 || peak.TimeWindow != "上午高峰" {
		t.Fatalf("peak = multiplier %v window %q，期望 1.0/上午高峰",
			peak.TimeMultiplier, peak.TimeWindow)
	}
	if offPeak.TimeMultiplier != 0.5 || offPeak.TimeWindow != "" {
		t.Fatalf("off-peak = multiplier %v window %q，期望 0.5/空",
			offPeak.TimeMultiplier, offPeak.TimeWindow)
	}
	if peak.Price.ModelRatio != 200 || offPeak.Price.ModelRatio != 100 {
		t.Fatalf("输入价 = %v/%v，期望 200/100（2:1）",
			peak.Price.ModelRatio, offPeak.Price.ModelRatio)
	}
	if offPeak.Price.CompletionRatio != 400 || offPeak.Price.CacheRatio != 2 ||
		offPeak.Price.CacheCreation5mRatio != 125 {
		t.Fatalf("闲时绝对单价未全部减半: %+v", offPeak.Price)
	}
	// 图片/音频是相对输入价的倍数，不得被时段折扣二次缩放。
	if offPeak.Price.ImageRatio != peak.Price.ImageRatio ||
		offPeak.Price.AudioRatio != peak.Price.AudioRatio {
		t.Fatalf("图片/音频倍率被时段折扣缩放: peak=%+v off=%+v", peak.Price, offPeak.Price)
	}

	// 实际配额也必须严格 2:1（证明乘在计费结果上，而不只是快照展示）。
	params := contract.TokenParams{P: 1_000_000, ImageCount: pricing.ImageCountUnset}
	peakQuota, err := pricing.ComputeQuota(peak, params, nil)
	if err != nil {
		t.Fatalf("ComputeQuota(peak): %v", err)
	}
	offQuota, err := pricing.ComputeQuota(offPeak, params, nil)
	if err != nil {
		t.Fatalf("ComputeQuota(off-peak): %v", err)
	}
	if peakQuota != 2*offQuota {
		t.Fatalf("1M 输入 token 的配额 = %d/%d，期望 2:1", peakQuota, offQuota)
	}

	// 5) 法定假日全天低谷：2026-10-01 周四 10:00 本应命中高峰。
	holiday, err := source.ResolveAt(ctx, model, "default", at(2026, time.October, 1, 10))
	if err != nil {
		t.Fatalf("ResolveAt(holiday): %v", err)
	}
	if holiday.TimeMultiplier != 0.5 || holiday.TimeWindow != "" || holiday.HolidayName != offDayName {
		t.Fatalf("法定假日 = multiplier %v window %q name %q，期望 0.5/空/%s",
			holiday.TimeMultiplier, holiday.TimeWindow, holiday.HolidayName, offDayName)
	}
	if holiday.Price.ModelRatio != 100 {
		t.Fatalf("法定假日输入价 = %v，期望 100", holiday.Price.ModelRatio)
	}

	// 6) 调休上班日按工作日窗口：2026-10-10 周六 10:00 命中「上午高峰」。
	makeup, err := source.ResolveAt(ctx, model, "default", at(2026, time.October, 10, 10))
	if err != nil {
		t.Fatalf("ResolveAt(makeup): %v", err)
	}
	if makeup.TimeMultiplier != 1.0 || makeup.TimeWindow != "上午高峰" || makeup.HolidayName != makeupName {
		t.Fatalf("调休上班 = multiplier %v window %q name %q，期望 1.0/上午高峰/%s",
			makeup.TimeMultiplier, makeup.TimeWindow, makeup.HolidayName, makeupName)
	}
	if makeup.Price.ModelRatio != 200 {
		t.Fatalf("调休上班输入价 = %v，期望 200", makeup.Price.ModelRatio)
	}

	// 同一周六、没有日历行为（例如一周后）仍按周末低谷：证明上一步的 1.0
	// 来自调休事实，而不是周六本身命中了工作日窗口。
	weekend, err := source.ResolveAt(ctx, model, "default", at(2026, time.October, 17, 10))
	if err != nil {
		t.Fatalf("ResolveAt(weekend): %v", err)
	}
	if weekend.TimeMultiplier != 0.5 || weekend.HolidayName != "" {
		t.Fatalf("普通周六 = multiplier %v name %q，期望 0.5/无节假日",
			weekend.TimeMultiplier, weekend.HolidayName)
	}

	// 7) 公开可见性：strategy 不写 billing_expr，模型仍在 /api/pricing，
	//    且列表/详情都返回 pricing_schedule。
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })

	publicHandlers, err := NewPublicPricingHandlers(PublicPricingHandlerConfig{
		Repo:   store.NewPublicPricingRepo(db),
		Config: mapConfig{},
		Redis:  rdb,
		Logger: testLogger(),
	})
	if err != nil {
		t.Fatalf("NewPublicPricingHandlers: %v", err)
	}
	rt := &Router{mux: http.NewServeMux()}
	publicHandlers.RegisterPublicPricingRoutes(rt)

	// search 收敛到本次模型：共享测试库里可能已有大量定价行，避免分页把
	// 断言目标挤到第二页。
	rr := serve(rt, httptest.NewRequest(http.MethodGet,
		"/api/pricing?page_size=100&search="+url.QueryEscape(model), http.NoBody))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/pricing = %d（body=%q）", rr.Code, rr.Body.String())
	}
	var list PublicPricingList
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode pricing list: %v", err)
	}
	card := findPublicCard(list.Items, model)
	if card == nil {
		t.Fatalf("配置时段策略后模型 %s 从公开目录消失（items=%d）", model, len(list.Items))
	}
	assertPricingSchedule(t, "列表", card.PricingSchedule)

	rr = serve(rt, httptest.NewRequest(http.MethodGet,
		"/api/pricing/"+url.PathEscape(model), http.NoBody))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/pricing/{model} = %d（body=%q）", rr.Code, rr.Body.String())
	}
	var detail PublicModelDetail
	if err := json.Unmarshal(rr.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode pricing detail: %v", err)
	}
	assertPricingSchedule(t, "详情", detail.PricingSchedule)
}

func loadHolidayCalendarRow(ctx context.Context, db *sql.DB, date string) (holidayCalendarSnapshot, error) {
	var snapshot holidayCalendarSnapshot
	err := db.QueryRowContext(ctx, `SELECT is_off_day, name, source, synced_at
		FROM holiday_calendar WHERE country = 'CN' AND date = $1::date`, date).
		Scan(&snapshot.offDay, &snapshot.name, &snapshot.source, &snapshot.synced)
	if err == sql.ErrNoRows {
		return holidayCalendarSnapshot{}, nil
	}
	if err != nil {
		return holidayCalendarSnapshot{}, err
	}
	snapshot.exists = true
	return snapshot, nil
}

func findPublicCard(items []PublicModelCard, model string) *PublicModelCard {
	for i := range items {
		if items[i].ModelID == model {
			return &items[i]
		}
	}
	return nil
}

// assertPricingSchedule 断言公开时段表与策略一致，且响应形状是结构化枚举
// （kind/timezone/default_multiplier/holiday_policy/windows），不是中文字符串。
func assertPricingSchedule(t *testing.T, surface string, schedule *PricingScheduleView) {
	t.Helper()
	if schedule == nil {
		t.Fatalf("%s缺少 pricing_schedule", surface)
	}
	if schedule.Kind != "peak_offpeak" || schedule.Timezone != "Asia/Shanghai" ||
		schedule.DefaultMultiplier != 0.5 || schedule.HolidayPolicy != "cn_offpeak" {
		t.Fatalf("%s schedule 头部错误: %+v", surface, schedule)
	}
	if len(schedule.Windows) != 1 {
		t.Fatalf("%s schedule.windows = %+v，期望 1 条", surface, schedule.Windows)
	}
	window := schedule.Windows[0]
	if window.Name != "上午高峰" || window.Start != "09:00" || window.End != "12:00" ||
		window.Multiplier != 1.0 || len(window.Days) != 5 {
		t.Fatalf("%s schedule window 错误: %+v", surface, window)
	}
}
