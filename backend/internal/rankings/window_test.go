package rankings

import (
	"encoding/json"
	"log/slog"
	"testing"
	"time"
)

// mapConfig 是 ConfigReader 的内存假实现（值的 JSON 文本）。
type mapConfig map[string]string

func (m mapConfig) Get(key string) (json.RawMessage, bool) {
	value, ok := m[key]
	if !ok {
		return nil, false
	}
	return json.RawMessage(value), true
}

func quietLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func TestParsePeriod(t *testing.T) {
	for _, want := range []Period{PeriodToday, PeriodWeek, PeriodMonth, PeriodYear} {
		got, ok := ParsePeriod(string(want))
		if !ok || got != want {
			t.Fatalf("ParsePeriod(%q) = %q,%v", want, got, ok)
		}
		if !want.Valid() {
			t.Fatalf("%q 应为合法周期", want)
		}
	}
	for _, bad := range []string{"", "day", "WEEK", "yesterday"} {
		if _, ok := ParsePeriod(bad); ok {
			t.Fatalf("ParsePeriod(%q) 应为非法", bad)
		}
	}
}

func TestPeriodIntervalsAndGranularity(t *testing.T) {
	cases := []struct {
		period      Period
		interval    time.Duration
		granularity Granularity
	}{
		{PeriodToday, 5 * time.Minute, GranularityHour},
		{PeriodWeek, 30 * time.Minute, GranularityDay},
		{PeriodMonth, 30 * time.Minute, GranularityDay},
		{PeriodYear, 6 * time.Hour, GranularityWeek},
	}
	for _, tc := range cases {
		if got := tc.period.RefreshInterval(); got != tc.interval {
			t.Fatalf("%s 刷新间隔 = %s，want %s", tc.period, got, tc.interval)
		}
		if got := tc.period.Granularity(); got != tc.granularity {
			t.Fatalf("%s 粒度 = %s，want %s", tc.period, got, tc.granularity)
		}
	}
}

func mustLocation(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("LoadLocation(%q): %v", name, err)
	}
	return loc
}

func TestCurrentWindowSiteTimezone(t *testing.T) {
	loc := mustLocation(t, "Asia/Shanghai")
	// 2026-10-20 是周二。
	now := time.Date(2026, 10, 20, 12, 34, 56, 0, loc)

	cases := []struct {
		period Period
		want   time.Time
	}{
		// 站点时区当日 00:00 = 前一 UTC 日 16:00。
		{PeriodToday, time.Date(2026, 10, 19, 16, 0, 0, 0, time.UTC)},
		// ISO 周起点是周一 2026-10-19 00:00 +08。
		{PeriodWeek, time.Date(2026, 10, 18, 16, 0, 0, 0, time.UTC)},
		{PeriodMonth, time.Date(2026, 9, 30, 16, 0, 0, 0, time.UTC)},
		{PeriodYear, time.Date(2025, 12, 31, 16, 0, 0, 0, time.UTC)},
	}
	for _, tc := range cases {
		window := CurrentWindow(tc.period, now, loc)
		if !window.Start.Equal(tc.want) {
			t.Fatalf("%s 窗口起点 = %s，want %s", tc.period, window.Start, tc.want)
		}
		if !window.End.Equal(now.UTC()) {
			t.Fatalf("%s 窗口终点 = %s，want now=%s", tc.period, window.End, now.UTC())
		}
	}
}

func TestWindowFallsBackToUTC(t *testing.T) {
	now := time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)
	window := CurrentWindow(PeriodToday, now, nil)
	if !window.Start.Equal(time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("nil location 应按 UTC 取当日 00:00，实际 %s", window.Start)
	}
}

func TestPreviousWindowEqualLength(t *testing.T) {
	loc := mustLocation(t, "Asia/Shanghai")
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, loc)
	for _, period := range allPeriods {
		cur := CurrentWindow(period, now, loc)
		prev := PreviousWindow(period, now, loc)
		if prev.End != cur.Start {
			t.Fatalf("%s 上一窗口终点应等于当前窗口起点", period)
		}
		if prev.Duration() != cur.Duration() {
			t.Fatalf("%s 上一窗口长度 %s != 当前 %s", period, prev.Duration(), cur.Duration())
		}
	}
}

func TestWindowDSTBoundaries(t *testing.T) {
	loc := mustLocation(t, "America/New_York")

	// 2026-03-08 春季拨快：当日 00:00 EST(UTC-5)，中午已是 EDT(UTC-4)。
	springNow := time.Date(2026, 3, 8, 12, 0, 0, 0, loc)
	spring := CurrentWindow(PeriodToday, springNow, loc)
	if want := time.Date(2026, 3, 8, 5, 0, 0, 0, time.UTC); !spring.Start.Equal(want) {
		t.Fatalf("春季 DST 日窗口起点 = %s，want %s", spring.Start, want)
	}
	if got := spring.Duration(); got != 11*time.Hour {
		t.Fatalf("春季 DST 日窗口长度 = %s，want 11h", got)
	}

	// 2026-11-01 秋季拨回：00:00 EDT(UTC-4)，中午已是 EST(UTC-5)。
	fallNow := time.Date(2026, 11, 1, 12, 0, 0, 0, loc)
	fall := CurrentWindow(PeriodToday, fallNow, loc)
	if want := time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC); !fall.Start.Equal(want) {
		t.Fatalf("秋季 DST 日窗口起点 = %s，want %s", fall.Start, want)
	}
	if got := fall.Duration(); got != 13*time.Hour {
		t.Fatalf("秋季 DST 日窗口长度 = %s，want 13h", got)
	}
}

func TestWeekStartsOnMonday(t *testing.T) {
	loc := time.UTC
	// 2026-10-25 是周日；ISO 周起点应回退到 10-19（周一）。
	sunday := time.Date(2026, 10, 25, 15, 0, 0, 0, loc)
	window := CurrentWindow(PeriodWeek, sunday, loc)
	if want := time.Date(2026, 10, 19, 0, 0, 0, 0, loc); !window.Start.Equal(want) {
		t.Fatalf("周日所在周的起点 = %s，want %s", window.Start, want)
	}
	// 周一当天起点就是当天 00:00，不前移一周。
	monday := time.Date(2026, 10, 19, 0, 0, 1, 0, loc)
	if got := CurrentWindow(PeriodWeek, monday, loc).Start; !got.Equal(time.Date(2026, 10, 19, 0, 0, 0, 0, loc)) {
		t.Fatalf("周一窗口起点 = %s", got)
	}
}

func TestResolveLocation(t *testing.T) {
	log := quietLogger()

	if got := ResolveLocation(mapConfig{}, log); got != time.UTC {
		t.Fatalf("缺失 site.timezone 应回退 UTC，实际 %s", got)
	}
	if got := ResolveLocation(mapConfig{configKeyTimezone: `"Mars/Olympus"`}, log); got != time.UTC {
		t.Fatalf("非法时区应回退 UTC，实际 %s", got)
	}
	if got := ResolveLocation(mapConfig{configKeyTimezone: `123`}, log); got != time.UTC {
		t.Fatalf("非字符串时区应回退 UTC，实际 %s", got)
	}
	if got := ResolveLocation(mapConfig{configKeyTimezone: `"Asia/Shanghai"`}, log); got.String() != "Asia/Shanghai" {
		t.Fatalf("合法时区 = %s", got)
	}
}

func TestResolveMinSamples(t *testing.T) {
	log := quietLogger()
	cases := []struct {
		raw  string
		want int64
	}{
		{"", defaultMinSamples},
		{`80`, 80},
		{`0`, minSamplesFloor},
		{`49`, minSamplesFloor},
		{`100001`, minSamplesCeil},
		{`"80"`, defaultMinSamples},
	}
	for _, tc := range cases {
		cfg := mapConfig{}
		if tc.raw != "" {
			cfg[configKeyMinSamples] = tc.raw
		}
		if got := ResolveMinSamples(cfg, log); got != tc.want {
			t.Fatalf("ResolveMinSamples(%q) = %d，want %d", tc.raw, got, tc.want)
		}
	}
}

func TestRankingsEnabled(t *testing.T) {
	if !RankingsEnabled(mapConfig{}) {
		t.Fatal("缺失 nav.rankings.enabled 应按开启处理")
	}
	if RankingsEnabled(mapConfig{configKeyEnabled: `false`}) {
		t.Fatal("显式 false 应关闭")
	}
	if !RankingsEnabled(mapConfig{configKeyEnabled: `true`}) {
		t.Fatal("显式 true 应开启")
	}
	if !RankingsEnabled(mapConfig{configKeyEnabled: `"not-bool"`}) {
		t.Fatal("非法值应回退开启（与注册表默认值一致）")
	}
}
