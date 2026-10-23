package pricingstrategy

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

// fakeLookup 是 HolidayLookup 的测试替身，同时记录调用参数，便于断言
// 「策略为 cn_offpeak 时才查询节假日」以及查询使用的国家/日期。
type fakeLookup struct {
	holiday Holiday
	ok      bool
	err     error

	calls       int
	lastCountry string
	lastDate    time.Time
}

func (f *fakeLookup) LookupHoliday(_ context.Context, country string, date time.Time) (Holiday, bool, error) {
	f.calls++
	f.lastCountry = country
	f.lastDate = date
	return f.holiday, f.ok, f.err
}

const deepseekJSON = `{
  "version": 1,
  "time": {
    "enabled": true,
    "timezone": "Asia/Shanghai",
    "holiday_policy": "cn_offpeak",
    "default_multiplier": 0.5,
    "windows": [
      {"name": "上午高峰", "days": [1,2,3,4,5], "start": "09:00", "end": "12:00", "multiplier": 1.0},
      {"name": "下午高峰", "days": [1,2,3,4,5], "start": "14:00", "end": "18:00", "multiplier": 1.0}
    ]
  }
}`

func mustParse(t *testing.T, raw string) Strategy {
	t.Helper()
	s, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse(%s) 期望成功，实际错误：%v", raw, err)
	}
	return s
}

func mustShanghai(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("加载 Asia/Shanghai 失败（宿主缺少 tzdata？）：%v", err)
	}
	return loc
}

func at(t *testing.T, year int, month time.Month, day, hour, minute int) time.Time {
	t.Helper()
	return time.Date(year, month, day, hour, minute, 0, 0, mustShanghai(t))
}

// ---------- Parse ----------

func TestParseNilMeansDisabled(t *testing.T) {
	for _, raw := range [][]byte{nil, []byte(""), []byte("   \n"), []byte("null")} {
		s, err := Parse(raw)
		if err != nil {
			t.Fatalf("Parse(%q) 期望无错误，实际：%v", raw, err)
		}
		if s.Time.Enabled {
			t.Fatalf("Parse(%q) 期望策略为禁用，实际启用了时段", raw)
		}
		m, err := s.MultiplierAt(context.Background(), at(t, 2026, 10, 5, 10, 0), nil)
		if err != nil {
			t.Fatalf("禁用策略 MultiplierAt 期望无错误，实际：%v", err)
		}
		if m.Multiplier != 1 {
			t.Fatalf("禁用策略倍率期望 1，实际 %v", m.Multiplier)
		}
		if m.Window != "" || m.HolidayName != "" {
			t.Fatalf("禁用策略不应返回命中信息，实际 %+v", m)
		}
	}
}

func TestParseValid(t *testing.T) {
	s := mustParse(t, deepseekJSON)

	if s.Version != 1 {
		t.Fatalf("Version 期望 1，实际 %d", s.Version)
	}
	if !s.Time.Enabled {
		t.Fatal("期望 enabled=true")
	}
	if s.Time.Timezone != "Asia/Shanghai" {
		t.Fatalf("Timezone 期望 Asia/Shanghai，实际 %q", s.Time.Timezone)
	}
	if s.Time.HolidayPolicy != HolidayPolicyCNOffpeak {
		t.Fatalf("HolidayPolicy 期望 cn_offpeak，实际 %q", s.Time.HolidayPolicy)
	}
	if s.Time.DefaultMultiplier != 0.5 {
		t.Fatalf("DefaultMultiplier 期望 0.5，实际 %v", s.Time.DefaultMultiplier)
	}
	if len(s.Time.Windows) != 2 {
		t.Fatalf("Windows 期望 2 条，实际 %d", len(s.Time.Windows))
	}
	w := s.Time.Windows[0]
	if w.Name != "上午高峰" || w.Start != "09:00" || w.End != "12:00" || w.Multiplier != 1.0 {
		t.Fatalf("首条窗口解析错误：%+v", w)
	}
	if len(w.Days) != 5 || w.Days[0] != 1 || w.Days[4] != 5 {
		t.Fatalf("Days 解析错误：%v", w.Days)
	}
}

func TestParseNormalizesEmptyHolidayPolicy(t *testing.T) {
	s := mustParse(t, `{"version":1,"time":{"enabled":false}}`)
	if s.Time.HolidayPolicy != HolidayPolicyNone {
		t.Fatalf("空 holiday_policy 期望规范化为 %q，实际 %q", HolidayPolicyNone, s.Time.HolidayPolicy)
	}
}

func TestParseRejectsInvalid(t *testing.T) {
	cases := []struct {
		name  string
		raw   string
		field string // 期望的字段路径前缀；空串表示只要求报错
	}{
		{"版本 2", `{"version":2}`, "version"},
		{"版本缺省", `{"version":0}`, "version"},
		{"未知时区", `{"version":1,"time":{"timezone":"Mars/Olympus"}}`, "time.timezone"},
		{"启用但时区为空", `{"version":1,"time":{"enabled":true,"default_multiplier":0.5}}`, "time.timezone"},
		{"时区为空字符串", `{"version":1,"time":{"enabled":true,"timezone":""}}`, "time.timezone"},
		{"起止时间缺前导零", `{"version":1,"time":{"enabled":true,"timezone":"UTC","default_multiplier":1,"windows":[{"days":[1],"start":"9:00","end":"12:00","multiplier":1}]}}`, "time.windows[0].start"},
		{"小时越界", `{"version":1,"time":{"enabled":true,"timezone":"UTC","default_multiplier":1,"windows":[{"days":[1],"start":"24:00","end":"12:00","multiplier":1}]}}`, "time.windows[0].start"},
		{"分钟越界", `{"version":1,"time":{"enabled":true,"timezone":"UTC","default_multiplier":1,"windows":[{"days":[1],"start":"09:00","end":"12:60","multiplier":1}]}}`, "time.windows[0].end"},
		{"结束时间为空", `{"version":1,"time":{"enabled":true,"timezone":"UTC","default_multiplier":1,"windows":[{"days":[1],"start":"09:00","end":"","multiplier":1}]}}`, "time.windows[0].end"},
		{"默认倍率过小", `{"version":1,"time":{"enabled":true,"timezone":"UTC","default_multiplier":0.006}}`, "time.default_multiplier"},
		{"默认倍率过大", `{"version":1,"time":{"enabled":true,"timezone":"UTC","default_multiplier":100.5}}`, "time.default_multiplier"},
		{"启用但默认倍率为零", `{"version":1,"time":{"enabled":true,"timezone":"UTC","default_multiplier":0}}`, "time.default_multiplier"},
		{"窗口倍率过小", `{"version":1,"time":{"enabled":true,"timezone":"UTC","default_multiplier":1,"windows":[{"days":[1],"start":"09:00","end":"12:00","multiplier":0.006}]}}`, "time.windows[0].multiplier"},
		{"窗口倍率为零", `{"version":1,"time":{"enabled":true,"timezone":"UTC","default_multiplier":1,"windows":[{"days":[1],"start":"09:00","end":"12:00","multiplier":0}]}}`, "time.windows[0].multiplier"},
		{"窗口倍率为负", `{"version":1,"time":{"enabled":true,"timezone":"UTC","default_multiplier":1,"windows":[{"days":[1],"start":"09:00","end":"12:00","multiplier":-1}]}}`, "time.windows[0].multiplier"},
		{"星期 0", `{"version":1,"time":{"enabled":true,"timezone":"UTC","default_multiplier":1,"windows":[{"days":[0],"start":"09:00","end":"12:00","multiplier":1}]}}`, "time.windows[0].days"},
		{"星期 8", `{"version":1,"time":{"enabled":true,"timezone":"UTC","default_multiplier":1,"windows":[{"days":[8],"start":"09:00","end":"12:00","multiplier":1}]}}`, "time.windows[0].days"},
		{"星期为空", `{"version":1,"time":{"enabled":true,"timezone":"UTC","default_multiplier":1,"windows":[{"days":[],"start":"09:00","end":"12:00","multiplier":1}]}}`, "time.windows[0].days"},
		{"星期重复", `{"version":1,"time":{"enabled":true,"timezone":"UTC","default_multiplier":1,"windows":[{"days":[1,1],"start":"09:00","end":"12:00","multiplier":1}]}}`, "time.windows[0].days"},
		{"非法节假日策略", `{"version":1,"time":{"enabled":false,"holiday_policy":"cn"}}`, "time.holiday_policy"},
		{"第一阶段拒绝 tiers", `{"version":1,"tiers":{"enabled":true},"time":{"enabled":false}}`, "tiers"},
		{"第一阶段拒绝 service", `{"version":1,"service":{"batch_multiplier":0.5},"time":{"enabled":false}}`, "service"},
		{"未知窗口字段", `{"version":1,"time":{"enabled":true,"timezone":"UTC","default_multiplier":1,"windows":[{"days":[1],"start":"09:00","end":"12:00","multiplier":1,"kind":"peak"}]}}`, ""},
		{"损坏的 JSON", `{"version":1`, ""},
		{"JSON 后有多余内容", `{"version":1} {"version":1}`, ""},
		{"days 类型错误", `{"version":1,"time":{"enabled":true,"timezone":"UTC","default_multiplier":1,"windows":[{"days":"1","start":"09:00","end":"12:00","multiplier":1}]}}`, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.raw))
			if err == nil {
				t.Fatalf("Parse(%s) 期望错误，实际成功", tc.raw)
			}
			var verr *ValidationError
			if !errors.As(err, &verr) {
				t.Fatalf("错误期望为 *ValidationError，实际 %T：%v", err, err)
			}
			if tc.field != "" && !strings.HasPrefix(verr.Field, tc.field) {
				t.Fatalf("错误字段期望前缀 %q，实际 %q（错误：%v）", tc.field, verr.Field, err)
			}
		})
	}
}

func TestParseValidationFieldPaths(t *testing.T) {
	_, err := Parse([]byte(`{"version":1,"time":{"enabled":true,"timezone":"UTC","default_multiplier":1,"windows":[` +
		`{"days":[1],"start":"09:00","end":"12:00","multiplier":1},` +
		`{"days":[1],"start":"09:00","end":"12:00","multiplier":5},` +
		`{"days":[2],"start":"25:00","end":"12:00","multiplier":1}]}}`))
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("期望 *ValidationError，实际 %T：%v", err, err)
	}
	if verr.Field != "time.windows[2].start" {
		t.Fatalf("字段路径期望 time.windows[2].start，实际 %q", verr.Field)
	}
}

func TestValidateExported(t *testing.T) {
	s := mustParse(t, deepseekJSON)
	if err := s.Validate(); err != nil {
		t.Fatalf("有效策略 Validate 期望 nil，实际 %v", err)
	}

	bad := s
	bad.Version = 3
	if err := bad.Validate(); err == nil {
		t.Fatal("Version=3 期望校验失败")
	}
}

func TestMarshalRoundTrip(t *testing.T) {
	s := mustParse(t, deepseekJSON)
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("Marshal 失败：%v", err)
	}
	again, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse(Marshal) 失败：%v", err)
	}
	if again.Time.Timezone != s.Time.Timezone || again.Time.DefaultMultiplier != s.Time.DefaultMultiplier {
		t.Fatalf("往返后字段不一致：%+v vs %+v", again, s)
	}
	if len(again.Time.Windows) != len(s.Time.Windows) || again.Time.Windows[1].Name != s.Time.Windows[1].Name {
		t.Fatalf("往返后窗口不一致：%+v vs %+v", again.Time.Windows, s.Time.Windows)
	}
	// JSON 字段名必须与设计文档一致（Task 5 会把它原样写进 model_pricing.strategy）。
	for _, key := range []string{`"version"`, `"time"`, `"enabled"`, `"timezone"`, `"holiday_policy"`, `"default_multiplier"`, `"windows"`, `"start"`, `"end"`, `"multiplier"`, `"days"`} {
		if !strings.Contains(string(raw), key) {
			t.Fatalf("序列化结果缺少字段 %s：%s", key, raw)
		}
	}
}

// ---------- MultiplierAt：窗口匹配 ----------

func TestMultiplierAtWindowMatching(t *testing.T) {
	s := mustParse(t, deepseekJSON)
	ctx := context.Background()

	cases := []struct {
		name string
		at   time.Time
		want string
		mult float64
	}{
		{"工作日高峰开始", at(t, 2026, 10, 5, 9, 0), "上午高峰", 1.0},
		{"工作日高峰中段", at(t, 2026, 10, 5, 10, 0), "上午高峰", 1.0},
		{"上午高峰右开区间", at(t, 2026, 10, 5, 12, 0), "", 0.5},
		{"午间低谷", at(t, 2026, 10, 5, 12, 30), "", 0.5},
		{"下午高峰开始", at(t, 2026, 10, 5, 14, 0), "下午高峰", 1.0},
		{"下午高峰结束", at(t, 2026, 10, 5, 18, 0), "", 0.5},
		{"高峰前一分钟", at(t, 2026, 10, 5, 8, 59), "", 0.5},
		{"周六全天低谷", at(t, 2026, 10, 10, 10, 0), "", 0.5},
		{"周日全天低谷", at(t, 2026, 10, 11, 10, 0), "", 0.5},
		{"周五下午高峰", at(t, 2026, 10, 9, 15, 0), "下午高峰", 1.0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := s.MultiplierAt(ctx, tc.at, nil)
			if err != nil {
				t.Fatalf("MultiplierAt 失败：%v", err)
			}
			if m.Window != tc.want {
				t.Fatalf("命中窗口期望 %q，实际 %q", tc.want, m.Window)
			}
			if m.Multiplier != tc.mult {
				t.Fatalf("倍率期望 %v，实际 %v", tc.mult, m.Multiplier)
			}
		})
	}
}

func TestMultiplierAtUsesStrategyTimezone(t *testing.T) {
	s := mustParse(t, deepseekJSON)
	// 02:00 UTC == 10:00 Asia/Shanghai，应命中上午高峰。
	utc := time.Date(2026, 10, 5, 2, 0, 0, 0, time.UTC)
	m, err := s.MultiplierAt(context.Background(), utc, nil)
	if err != nil {
		t.Fatalf("MultiplierAt 失败：%v", err)
	}
	if m.Window != "上午高峰" || m.Multiplier != 1.0 {
		t.Fatalf("期望按策略时区命中上午高峰，实际 %+v", m)
	}
}

func TestMultiplierAtOvernightWindow(t *testing.T) {
	s := mustParse(t, `{"version":1,"time":{"enabled":true,"timezone":"Asia/Shanghai","default_multiplier":1,
		"windows":[{"name":"夜间","days":[6],"start":"22:00","end":"08:00","multiplier":0.3}]}}`)
	ctx := context.Background()

	cases := []struct {
		name string
		at   time.Time
		want float64
	}{
		{"周六开始时刻命中", at(t, 2026, 10, 10, 22, 0), 0.3},
		{"周六深夜命中", at(t, 2026, 10, 10, 23, 59), 0.3},
		{"周日凌晨命中（跨夜按周六）", at(t, 2026, 10, 11, 2, 0), 0.3},
		{"周日结束前命中", at(t, 2026, 10, 11, 7, 59), 0.3},
		{"周日结束时刻不命中", at(t, 2026, 10, 11, 8, 0), 1},
		{"周六开始前不命中", at(t, 2026, 10, 10, 21, 59), 1},
		{"周五深夜不命中（周五不是开始日）", at(t, 2026, 10, 9, 23, 0), 1},
		{"周日深夜不命中（周日不是开始日）", at(t, 2026, 10, 11, 23, 0), 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := s.MultiplierAt(ctx, tc.at, nil)
			if err != nil {
				t.Fatalf("MultiplierAt 失败：%v", err)
			}
			if m.Multiplier != tc.want {
				t.Fatalf("倍率期望 %v，实际 %v（命中 %q）", tc.want, m.Multiplier, m.Window)
			}
		})
	}
}

func TestMultiplierAtEqualStartEndIsFullDay(t *testing.T) {
	s := mustParse(t, `{"version":1,"time":{"enabled":true,"timezone":"Asia/Shanghai","default_multiplier":1,
		"windows":[{"name":"全天","days":[1],"start":"00:00","end":"00:00","multiplier":0.2}]}}`)
	for _, hour := range []int{0, 6, 12, 23} {
		m, err := s.MultiplierAt(context.Background(), at(t, 2026, 10, 5, hour, 0), nil)
		if err != nil {
			t.Fatalf("MultiplierAt 失败：%v", err)
		}
		if m.Multiplier != 0.2 {
			t.Fatalf("%02d:00 期望命中全天窗口，实际倍率 %v", hour, m.Multiplier)
		}
	}
}

func TestMultiplierAtOverlapLastWins(t *testing.T) {
	s := mustParse(t, `{"version":1,"time":{"enabled":true,"timezone":"Asia/Shanghai","default_multiplier":1,
		"windows":[
			{"name":"先声明","days":[1],"start":"09:00","end":"12:00","multiplier":0.8},
			{"name":"后声明","days":[1],"start":"10:00","end":"11:00","multiplier":1.5}
		]}}`)
	m, err := s.MultiplierAt(context.Background(), at(t, 2026, 10, 5, 10, 30), nil)
	if err != nil {
		t.Fatalf("MultiplierAt 失败：%v", err)
	}
	if m.Window != "后声明" || m.Multiplier != 1.5 {
		t.Fatalf("重叠窗口期望最后一条生效，实际 %+v", m)
	}
}

func TestMultiplierAtDSTBoundary(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("加载 America/New_York 失败：%v", err)
	}
	s := mustParse(t, `{"version":1,"time":{"enabled":true,"timezone":"America/New_York","default_multiplier":1,
		"windows":[{"name":"凌晨","days":[7],"start":"01:00","end":"04:00","multiplier":2}]}}`)
	ctx := context.Background()

	// 2026-03-08 春季前跳：当地 02:00–03:00 不存在，03:30 EDT 仍应命中 01:00–04:00。
	m, err := s.MultiplierAt(ctx, time.Date(2026, 3, 8, 3, 30, 0, 0, loc), nil)
	if err != nil {
		t.Fatalf("MultiplierAt 失败：%v", err)
	}
	if m.Multiplier != 2 {
		t.Fatalf("春季前跳后 03:30 期望命中窗口，实际 %+v", m)
	}
	// 前跳当天 00:30 在窗口外。
	m, err = s.MultiplierAt(ctx, time.Date(2026, 3, 8, 0, 30, 0, 0, loc), nil)
	if err != nil {
		t.Fatalf("MultiplierAt 失败：%v", err)
	}
	if m.Multiplier != 1 {
		t.Fatalf("春季前跳当天 00:30 期望不命中，实际 %+v", m)
	}

	// 2026-11-01 秋季回拨：01:30 出现两次，按墙钟时间应命中。
	m, err = s.MultiplierAt(ctx, time.Date(2026, 11, 1, 1, 30, 0, 0, loc), nil)
	if err != nil {
		t.Fatalf("MultiplierAt 失败：%v", err)
	}
	if m.Multiplier != 2 {
		t.Fatalf("秋季回拨 01:30 期望命中窗口，实际 %+v", m)
	}
}

func TestMultiplierAtRejectsBrokenStrategy(t *testing.T) {
	s := Strategy{Version: 1, Time: TimeConfig{Enabled: true, Timezone: "Not/AZone", DefaultMultiplier: 1}}
	if _, err := s.MultiplierAt(context.Background(), time.Now(), nil); err == nil {
		t.Fatal("非法时区期望 MultiplierAt 返回错误")
	}

	s = Strategy{Version: 1, Time: TimeConfig{Enabled: true, Timezone: "UTC", DefaultMultiplier: math.NaN()}}
	if _, err := s.MultiplierAt(context.Background(), time.Now(), nil); err == nil {
		t.Fatal("NaN 倍率期望 MultiplierAt 返回错误")
	}
}

// ---------- MultiplierAt：节假日 ----------

func TestMultiplierAtHolidayOffDay(t *testing.T) {
	s := mustParse(t, deepseekJSON)
	lookup := &fakeLookup{holiday: Holiday{Name: "国庆节", OffDay: true}, ok: true}

	// 2026-10-01 是周四（工作日），但法定假日全天按默认（低谷）倍率。
	m, err := s.MultiplierAt(context.Background(), at(t, 2026, 10, 1, 10, 0), lookup)
	if err != nil {
		t.Fatalf("MultiplierAt 失败：%v", err)
	}
	if m.Multiplier != 0.5 || m.Window != "" {
		t.Fatalf("法定假日期望全天默认倍率 0.5 且不命中窗口，实际 %+v", m)
	}
	if !m.HolidayOffDay || m.HolidayName != "国庆节" {
		t.Fatalf("节假日信息缺失：%+v", m)
	}
	if m.HolidayLookupFailed {
		t.Fatal("查询成功时 HolidayLookupFailed 应为 false")
	}
	if lookup.calls != 1 || lookup.lastCountry != CountryCN {
		t.Fatalf("期望以 CN 查询一次，实际 calls=%d country=%q", lookup.calls, lookup.lastCountry)
	}
	if got := lookup.lastDate.Location().String(); got != "Asia/Shanghai" {
		t.Fatalf("查询日期期望使用策略时区，实际 %q", got)
	}
	if lookup.lastDate.Day() != 1 || lookup.lastDate.Month() != time.October {
		t.Fatalf("查询日期期望 10-01，实际 %s", lookup.lastDate)
	}
}

func TestMultiplierAtMakeupWorkday(t *testing.T) {
	s := mustParse(t, deepseekJSON)
	lookup := &fakeLookup{holiday: Holiday{Name: "国庆节", OffDay: false}, ok: true}

	// 2026-10-10 是周六调休上班日：10:00 应按工作日窗口计价，而不是周末低谷。
	m, err := s.MultiplierAt(context.Background(), at(t, 2026, 10, 10, 10, 0), lookup)
	if err != nil {
		t.Fatalf("MultiplierAt 失败：%v", err)
	}
	if m.Multiplier != 1.0 || m.Window != "上午高峰" {
		t.Fatalf("调休上班日期望命中工作日窗口 1.0，实际 %+v", m)
	}
	if m.HolidayName != "国庆节" || m.HolidayOffDay {
		t.Fatalf("调休上班日节假日信息错误：%+v", m)
	}

	// 同一调休日的非窗口时段仍按默认倍率。
	m, err = s.MultiplierAt(context.Background(), at(t, 2026, 10, 10, 21, 0), lookup)
	if err != nil {
		t.Fatalf("MultiplierAt 失败：%v", err)
	}
	if m.Multiplier != 0.5 || m.Window != "" {
		t.Fatalf("调休日 21:00 期望默认倍率 0.5，实际 %+v", m)
	}
}

func TestMultiplierAtMakeupUsesWorkdayWindowsOnly(t *testing.T) {
	// 周末专属窗口不应在调休上班日生效——调休日是工作日，只看工作日窗口。
	s := mustParse(t, `{"version":1,"time":{"enabled":true,"timezone":"Asia/Shanghai","holiday_policy":"cn_offpeak",
		"default_multiplier":1,
		"windows":[{"name":"周末场","days":[6,7],"start":"09:00","end":"12:00","multiplier":3}]}}`)
	lookup := &fakeLookup{holiday: Holiday{Name: "调休", OffDay: false}, ok: true}

	m, err := s.MultiplierAt(context.Background(), at(t, 2026, 10, 10, 10, 0), lookup)
	if err != nil {
		t.Fatalf("MultiplierAt 失败：%v", err)
	}
	if m.Multiplier != 1 || m.Window != "" {
		t.Fatalf("调休日不应命中周末专属窗口，实际 %+v", m)
	}

	// 普通周六（无节假日数据）仍然命中周末窗口。
	m, err = s.MultiplierAt(context.Background(), at(t, 2026, 10, 17, 10, 0), nil)
	if err != nil {
		t.Fatalf("MultiplierAt 失败：%v", err)
	}
	if m.Multiplier != 3 || m.Window != "周末场" {
		t.Fatalf("普通周六期望命中周末窗口，实际 %+v", m)
	}
}

func TestMultiplierAtHolidayLookupErrorFallsBackToWeekday(t *testing.T) {
	s := mustParse(t, deepseekJSON)
	lookup := &fakeLookup{err: errors.New("holiday source unavailable")}

	m, err := s.MultiplierAt(context.Background(), at(t, 2026, 10, 5, 10, 0), lookup)
	if err != nil {
		t.Fatalf("节假日查询失败不应让定价失败，实际错误：%v", err)
	}
	if !m.HolidayLookupFailed {
		t.Fatal("期望 HolidayLookupFailed=true")
	}
	if m.Multiplier != 1.0 || m.Window != "上午高峰" {
		t.Fatalf("查询失败后期望按星期规则命中高峰，实际 %+v", m)
	}

	// 周六查询失败同样按周末规则（默认倍率），而不是报错或免费。
	m, err = s.MultiplierAt(context.Background(), at(t, 2026, 10, 10, 10, 0), lookup)
	if err != nil {
		t.Fatalf("MultiplierAt 失败：%v", err)
	}
	if !m.HolidayLookupFailed || m.Multiplier != 0.5 {
		t.Fatalf("周六查询失败期望默认倍率 0.5，实际 %+v", m)
	}
}

func TestMultiplierAtHolidayMissingFallsBackToWeekday(t *testing.T) {
	s := mustParse(t, deepseekJSON)
	lookup := &fakeLookup{ok: false}

	m, err := s.MultiplierAt(context.Background(), at(t, 2026, 10, 5, 10, 0), lookup)
	if err != nil {
		t.Fatalf("MultiplierAt 失败：%v", err)
	}
	if m.HolidayLookupFailed || m.HolidayName != "" {
		t.Fatalf("缺少当日数据不应标记为查询失败，实际 %+v", m)
	}
	if m.Multiplier != 1.0 {
		t.Fatalf("缺少数据时期望按星期规则，实际 %+v", m)
	}
}

func TestMultiplierAtHolidayPolicyNoneSkipsLookup(t *testing.T) {
	raw := strings.Replace(deepseekJSON, `"holiday_policy": "cn_offpeak"`, `"holiday_policy": "none"`, 1)
	s := mustParse(t, raw)
	lookup := &fakeLookup{holiday: Holiday{Name: "国庆节", OffDay: true}, ok: true}

	m, err := s.MultiplierAt(context.Background(), at(t, 2026, 10, 1, 10, 0), lookup)
	if err != nil {
		t.Fatalf("MultiplierAt 失败：%v", err)
	}
	if lookup.calls != 0 {
		t.Fatalf("holiday_policy=none 不应查询节假日，实际调用 %d 次", lookup.calls)
	}
	if m.Multiplier != 1.0 || m.HolidayName != "" {
		t.Fatalf("policy=none 时期望按普通窗口计价，实际 %+v", m)
	}
}
