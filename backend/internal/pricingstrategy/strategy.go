// Package pricingstrategy 实现 model_pricing.strategy 的结构化定价策略：
// JSON 解析与校验，以及按冻结的请求时间计算时段倍率。
//
// 【为什么不写进 billing_expr】公开模型库的可见条件是
// `status = 1 AND billing_expr IS NULL`（store/pricing_public_repo.go 的
// visibleModelWhere）。把时段策略编码进表达式会让模型从首页消失，正是之前
// 修过的问题；而且 billing/expr.go 的算术子集还不能表达时间函数。因此策略
// 独立存进 `strategy jsonb`，由定价源在解析快照时直接乘到绝对单价上。
//
// 【本包不碰 IO】校验与匹配都是纯函数：节假日数据由调用方通过
// HolidayLookup 注入（Task 3 的 holiday.Store），时区用 time.LoadLocation。
// 这样回退规则（数据缺失/查询失败时按星期规则继续）可以在单测里完全覆盖。
//
// 【时间语义】
//   - days 用 1=周一 … 7=周日，JSON 不接受 0，避免与 Go time.Weekday() 的
//     0=周日混淆；
//   - start/end 为 HH:MM，左闭右开；end <= start 表示跨夜（end == start
//     表示覆盖全天）；
//   - 跨夜窗口的 days 指窗口**开始**的那一天：`days:[6], 22:00–08:00` 表示
//     周六 22:00 到周日 08:00，周日 02:00 命中这条窗口；
//   - 同一时刻命中多个窗口时，数组里最后一条生效（保存时前端提示重叠）。
//
// 【节假日】holiday_policy=cn_offpeak 时：法定假日（is_off_day=true）全天按
// default_multiplier，忽略窗口；调休上班日（is_off_day=false）按工作日窗口
// 计价。查询失败或当日无数据时按星期与窗口的常规规则继续，并把失败标记在
// Match.HolidayLookupFailed 上，绝不因为节假日不可用而让定价失败或退化为免费。
package pricingstrategy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"time"
)

// CurrentVersion 是第一阶段唯一接受的策略 JSON 版本。
const CurrentVersion = 1

// CountryCN 是中国节假日日历的国家代码（第一阶段只支持中国）。
const CountryCN = "CN"

// 节假日策略取值。空字符串在 Parse 时规范化为 HolidayPolicyNone。
const (
	HolidayPolicyNone      = "none"
	HolidayPolicyCNOffpeak = "cn_offpeak"
)

// 倍率的合法区间（十进制倍数，0.5 表示半价）。不要存百分比，避免前后端各乘一次 100。
const (
	MinMultiplier = 0.01
	MaxMultiplier = 100.0
)

// Strategy 是 model_pricing.strategy 的反序列化结果。
type Strategy struct {
	Version int        `json:"version"`
	Time    TimeConfig `json:"time"`
}

// TimeConfig 描述按星期与本地时间切换价格的规则。
type TimeConfig struct {
	Enabled bool `json:"enabled"`
	// Timezone 必须是 IANA 名称（time.LoadLocation 校验）。每个策略显式保存
	// 自己的时区，避免站点时区变更后历史策略被静默改写。
	Timezone string `json:"timezone"`
	// HolidayPolicy 取 none（默认）或 cn_offpeak。
	HolidayPolicy string `json:"holiday_policy"`
	// DefaultMultiplier 是未命中任何窗口时的倍率（也是 cn_offpeak 下法定假日的全天倍率）。
	DefaultMultiplier float64  `json:"default_multiplier"`
	Windows           []Window `json:"windows"`
}

// Window 是一条时段规则。Start/End 为 HH:MM，左闭右开。
type Window struct {
	Name       string  `json:"name"`
	Days       []int   `json:"days"`
	Start      string  `json:"start"`
	End        string  `json:"end"`
	Multiplier float64 `json:"multiplier"`
}

// Holiday 是一条节假日事实。
type Holiday struct {
	Name string
	// OffDay=true 是法定假日；false 是调休上班日。
	OffDay bool
}

// HolidayLookup 是节假日查询面，由 holiday.Store 实现。
//
// date 已经位于策略自己的时区（调用方负责），实现应按该时间所在地的
// YYYY-MM-DD 查表；第二个返回值为 false 表示该日期没有数据（不是错误）。
type HolidayLookup interface {
	LookupHoliday(ctx context.Context, country string, date time.Time) (Holiday, bool, error)
}

// Match 是一次时段匹配的结果。
type Match struct {
	// Window 是命中的窗口名；未命中窗口时为空（使用 DefaultMultiplier）。
	Window     string
	Multiplier float64
	// HolidayName 是当天命中的节假日/调休名称（没有数据时为空）。
	HolidayName string
	// HolidayOffDay 表示当天是法定假日（全天默认倍率）。
	HolidayOffDay bool
	// HolidayLookupFailed 表示查询过节假日但失败了；此时倍率来自星期规则。
	HolidayLookupFailed bool
}

// ValidationError 是策略 JSON 的字段级校验错误。Field 使用相对策略对象的
// JSON 路径（例如 time.windows[0].start），上层可直接映射为 422 的字段名。
type ValidationError struct {
	Field  string
	Reason string
}

func (e *ValidationError) Error() string {
	if e.Field == "" {
		return "pricingstrategy: " + e.Reason
	}
	return "pricingstrategy: " + e.Field + ": " + e.Reason
}

// Parse 解析并校验策略 JSON。
//
// raw 为 nil/空/`null` 时返回零值 Strategy（禁用）且不报错——已有行
// strategy IS NULL 的行为完全不变。
func Parse(raw []byte) (Strategy, error) {
	var s Strategy
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return s, nil
	}

	dec := json.NewDecoder(bytes.NewReader(trimmed))
	// 第一阶段不接受 tiers/service 等未开放字段：未知字段按校验错误拒绝，
	// 避免出现「配了但从不生效」的死配置（对应设计 §3 非目标）。
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return Strategy{}, decodeError(err)
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return Strategy{}, &ValidationError{Reason: "strategy JSON 之后存在多余内容"}
	}

	// 规范化：空 holiday_policy 等同于 none，让存库值与公开面/前端读取一致。
	if s.Time.HolidayPolicy == "" {
		s.Time.HolidayPolicy = HolidayPolicyNone
	}
	if err := s.Validate(); err != nil {
		return Strategy{}, err
	}
	return s, nil
}

// Validate 校验策略（字段级错误带 JSON 路径）。空 holiday_policy 视为 none。
func (s Strategy) Validate() error {
	if s.Version != CurrentVersion {
		return &ValidationError{Field: "version", Reason: fmt.Sprintf("只支持 version=%d", CurrentVersion)}
	}

	switch s.Time.HolidayPolicy {
	case "", HolidayPolicyNone, HolidayPolicyCNOffpeak:
	default:
		return &ValidationError{
			Field:  "time.holiday_policy",
			Reason: fmt.Sprintf("只支持 %q 或 %q", HolidayPolicyNone, HolidayPolicyCNOffpeak),
		}
	}

	// 时区：启用时必须显式给出；未启用时若给了也要是合法 IANA 名称。
	if s.Time.Timezone == "" {
		if s.Time.Enabled {
			return &ValidationError{Field: "time.timezone", Reason: "启用时段价格时必须指定 IANA 时区"}
		}
	} else if _, err := time.LoadLocation(s.Time.Timezone); err != nil {
		return &ValidationError{Field: "time.timezone", Reason: fmt.Sprintf("%q 不是有效的 IANA 时区名称", s.Time.Timezone)}
	}

	// 默认倍率：启用时必须落在区间内（0 是缺省值，不是「免费」）；
	// 未启用时只校验显式给出的非零值，允许草稿保留空值。
	if s.Time.Enabled || s.Time.DefaultMultiplier != 0 {
		if err := validateMultiplier(s.Time.DefaultMultiplier); err != nil {
			return &ValidationError{Field: "time.default_multiplier", Reason: err.Error()}
		}
	}

	for i, w := range s.Time.Windows {
		if err := validateWindow(w); err != nil {
			var verr *ValidationError
			if errors.As(err, &verr) {
				return &ValidationError{
					Field:  fmt.Sprintf("time.windows[%d].%s", i, verr.Field),
					Reason: verr.Reason,
				}
			}
			return &ValidationError{Field: fmt.Sprintf("time.windows[%d]", i), Reason: err.Error()}
		}
	}
	return nil
}

// MultiplierAt 计算 at 时刻生效的时段倍率。
//
// 未启用时段价格时返回 Match{Multiplier: 1}。节假日查询失败不会返回错误，
// 而是回退到星期规则并置 HolidayLookupFailed。lookup 为 nil 时不做节假日
// 判断（等价于当日无数据）。
func (s Strategy) MultiplierAt(ctx context.Context, at time.Time, lookup HolidayLookup) (Match, error) {
	if !s.Time.Enabled {
		return Match{Multiplier: 1}, nil
	}

	loc, err := time.LoadLocation(s.Time.Timezone)
	if err != nil {
		return Match{}, &ValidationError{Field: "time.timezone", Reason: fmt.Sprintf("%q 不是有效的 IANA 时区名称", s.Time.Timezone)}
	}
	if err := validateMultiplier(s.Time.DefaultMultiplier); err != nil {
		return Match{}, &ValidationError{Field: "time.default_multiplier", Reason: err.Error()}
	}

	local := at.In(loc)
	minutes := local.Hour()*60 + local.Minute()
	today := isoWeekday(local.Weekday())
	yesterday := isoWeekday(local.AddDate(0, 0, -1).Weekday())

	match := Match{}
	makeupWorkday := false
	if s.Time.HolidayPolicy == HolidayPolicyCNOffpeak && lookup != nil {
		holiday, ok, lookupErr := lookup.LookupHoliday(ctx, CountryCN, local)
		switch {
		case lookupErr != nil:
			// 数据源不可达：按星期规则继续，不让请求失败（设计 §5.2.1 规则 4）。
			match.HolidayLookupFailed = true
		case ok:
			match.HolidayName = holiday.Name
			match.HolidayOffDay = holiday.OffDay
			if holiday.OffDay {
				// 法定假日：全天按默认（低谷）倍率，忽略窗口。
				match.Multiplier = s.Time.DefaultMultiplier
				return match, nil
			}
			// 调休上班日：按工作日窗口计价。
			makeupWorkday = true
		}
	}

	best := -1
	for i, w := range s.Time.Windows {
		if err := validateWindow(w); err != nil {
			var verr *ValidationError
			if errors.As(err, &verr) {
				return Match{}, &ValidationError{
					Field:  fmt.Sprintf("time.windows[%d].%s", i, verr.Field),
					Reason: verr.Reason,
				}
			}
			return Match{}, &ValidationError{Field: fmt.Sprintf("time.windows[%d]", i), Reason: err.Error()}
		}
		start, _ := parseClock(w.Start)
		end, _ := parseClock(w.End)
		if windowHit(w.Days, start, end, minutes, today, yesterday, makeupWorkday) {
			// 重叠时数组顺序最后一条生效。
			best = i
		}
	}

	if best >= 0 {
		match.Window = s.Time.Windows[best].Name
		match.Multiplier = s.Time.Windows[best].Multiplier
		return match, nil
	}
	match.Multiplier = s.Time.DefaultMultiplier
	return match, nil
}

// windowHit 判断某个窗口是否命中。minutes 为当地 0 点起的分钟数；today /
// yesterday 为 ISO 星期（1=周一…7=周日）。makeupWorkday 为真时按「工作日」
// 处理：忽略 days 里的具体星期，只保留工作日的判定（窗口的 days 与
// 周一至周五有交集即视为工作日窗口），从而让调休上班日使用工作日窗口，
// 而不是周末专属窗口或周末低谷。
func windowHit(days []int, start, end, minutes, today, yesterday int, makeupWorkday bool) bool {
	todayOK := containsInt(days, today)
	yesterdayOK := containsInt(days, yesterday)
	if makeupWorkday {
		workdayWindow := containsWorkday(days)
		todayOK, yesterdayOK = workdayWindow, workdayWindow
	}
	if end > start {
		return todayOK && minutes >= start && minutes < end
	}
	// 跨夜（含 end == start 的全天窗口）：今天从 start 起，或昨天从 start 起延续到今天。
	return (todayOK && minutes >= start) || (yesterdayOK && minutes < end)
}

func containsInt(values []int, want int) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

// containsWorkday 判断窗口的 days 是否覆盖任一周一至周五。
func containsWorkday(days []int) bool {
	for _, d := range days {
		if d >= 1 && d <= 5 {
			return true
		}
	}
	return false
}

func isoWeekday(d time.Weekday) int {
	if d == time.Sunday {
		return 7
	}
	return int(d)
}

// parseClock 把 HH:MM 解析成 0 点起的分钟数。调用方通常已通过 Validate；
// 这里仍返回 ok=false 供 MultiplierAt 对手工构造的策略兜底。
func parseClock(v string) (int, bool) {
	if len(v) != 5 || v[2] != ':' {
		return 0, false
	}
	t, err := time.Parse("15:04", v)
	if err != nil {
		return 0, false
	}
	return t.Hour()*60 + t.Minute(), true
}

func validateWindow(w Window) error {
	if len(w.Days) == 0 {
		return &ValidationError{Field: "days", Reason: "星期列表不能为空"}
	}
	seen := make(map[int]bool, len(w.Days))
	for _, d := range w.Days {
		if d < 1 || d > 7 {
			return &ValidationError{Field: "days", Reason: fmt.Sprintf("星期取值必须在 1–7（1=周一），实际 %d", d)}
		}
		if seen[d] {
			return &ValidationError{Field: "days", Reason: fmt.Sprintf("星期 %d 重复", d)}
		}
		seen[d] = true
	}
	if _, ok := parseClock(w.Start); !ok {
		return &ValidationError{Field: "start", Reason: fmt.Sprintf("开始时间 %q 必须是 HH:MM", w.Start)}
	}
	if _, ok := parseClock(w.End); !ok {
		return &ValidationError{Field: "end", Reason: fmt.Sprintf("结束时间 %q 必须是 HH:MM", w.End)}
	}
	if err := validateMultiplier(w.Multiplier); err != nil {
		return &ValidationError{Field: "multiplier", Reason: err.Error()}
	}
	return nil
}

func validateMultiplier(m float64) error {
	if math.IsNaN(m) || math.IsInf(m, 0) {
		return errors.New("倍率必须是有限数值")
	}
	if m < MinMultiplier || m > MaxMultiplier {
		return fmt.Errorf("倍率必须在 %g–%g 之间（十进制倍数，0.5 表示半价），实际 %v", MinMultiplier, MaxMultiplier, m)
	}
	return nil
}

// decodeError 把 encoding/json 的错误映射成带 JSON 路径的 ValidationError，
// 让上层可以按字段返回 422（Task 5 需要 strategy.time.windows[0].start 这类路径）。
func decodeError(err error) error {
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		return &ValidationError{
			Field:  typeErr.Field,
			Reason: fmt.Sprintf("类型不正确，期望 %s", typeErr.Type.String()),
		}
	}
	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		return &ValidationError{Reason: fmt.Sprintf("不是合法的 JSON（偏移 %d）", syntaxErr.Offset)}
	}
	const unknownField = `json: unknown field `
	if strings.HasPrefix(err.Error(), unknownField) {
		field := strings.Trim(err.Error()[len(unknownField):], `"`)
		return &ValidationError{Field: field, Reason: "未知字段（第一阶段不支持）"}
	}
	return &ValidationError{Reason: err.Error()}
}
