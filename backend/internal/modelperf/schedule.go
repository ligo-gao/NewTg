package modelperf

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Schedule 是定时探测的触发时刻表。调度器只依赖「下一次何时触发」，
// 具体节奏由实现决定：固定的每日时刻表（DailyTimes）或固定小时间隔
// （IntervalSchedule）。
type Schedule interface {
	// Next 返回严格晚于 after 的下一个触发时刻；没有下次时返回零时间。
	Next(after time.Time) time.Time
	// Location 返回解释时刻所用的时区。
	Location() *time.Location
}

// 编译期断言：两种实现都必须满足 Schedule。
var (
	_ Schedule = DailyTimes{}
	_ Schedule = IntervalSchedule{}
)

// DailyTimes 是每日探测时刻表，时刻按升序保存，时区固定为解析时传入的 loc。
//
// 字段不导出：调度器与本次任务同属 modelperf 包，后续若要对外使用，
// 通过 Location / Next 两个方法即可。
type DailyTimes struct {
	dayTimes []time.Duration
	loc      *time.Location
}

// ParseTimes 解析形如 "00:00,12:00" 的时刻表，loc 为 nil 时按 UTC 处理。
//
// 空项、非 HH:MM 格式、越界的小时/分钟一律报错，避免把配置错误
// 悄悄变成“某个时刻不跑”。解析成功后按升序排序，Next 依赖该顺序。
func ParseTimes(raw string, loc *time.Location) (DailyTimes, error) {
	if loc == nil {
		loc = time.UTC
	}
	items := strings.Split(raw, ",")
	dayTimes := make([]time.Duration, 0, len(items))
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item == "" {
			return DailyTimes{}, fmt.Errorf("schedule %q contains an empty time", raw)
		}
		parts := strings.Split(item, ":")
		if len(parts) != 2 || !isDigits(parts[0]) || !isDigits(parts[1]) {
			return DailyTimes{}, fmt.Errorf("invalid time %q: want HH:MM", item)
		}
		hour, err := strconv.Atoi(parts[0])
		if err != nil || hour < 0 || hour > 23 {
			return DailyTimes{}, fmt.Errorf("invalid hour in %q", item)
		}
		minute, err := strconv.Atoi(parts[1])
		if err != nil || minute < 0 || minute > 59 {
			return DailyTimes{}, fmt.Errorf("invalid minute in %q", item)
		}
		dayTimes = append(dayTimes,
			time.Duration(hour)*time.Hour+time.Duration(minute)*time.Minute)
	}
	sort.Slice(dayTimes, func(i, j int) bool { return dayTimes[i] < dayTimes[j] })
	return DailyTimes{dayTimes: dayTimes, loc: loc}, nil
}

// isDigits 报告 s 是否恰好是两个 ASCII 数字：HH / MM 固定两位宽，
// 这样 "12:0"、"+12:00"、"-0:30" 这类写法一律当配置错误，而不是被
// Atoi 宽松解析成看上去合法的时刻。
func isDigits(s string) bool {
	if len(s) != 2 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// Location 返回时刻表使用的时区。
func (s DailyTimes) Location() *time.Location {
	return s.loc
}

// Next 返回严格晚于 after 的下一个时刻，按 s.loc 解释 after 与结果。
//
// 不假设时区是固定偏移，也不使用 Add(24h)：逐日逐时刻都用 time.Date 构造，
// 由 Go 根据时区数据库处理夏令时（DST）。dayOffset 上限为 2——时刻都在
// 一天之内，跨一天必然命中，多留一天覆盖 DST 跳变当天的边界情况。
//
// 零值 DailyTimes（没有时刻）返回零时间，调用方应先确认 ParseTimes 成功；
// 把零时间交给 time.Until 会得到一个极大的负时长。
func (s DailyTimes) Next(after time.Time) time.Time {
	loc := s.loc
	if loc == nil {
		loc = time.UTC
	}
	if len(s.dayTimes) == 0 {
		// 零值 DailyTimes 没有时刻，按“无下次”返回零时间。
		return time.Time{}
	}
	local := after.In(loc)
	for dayOffset := 0; dayOffset <= 2; dayOffset++ {
		for _, dayTime := range s.dayTimes {
			hour := int(dayTime / time.Hour)
			minute := int((dayTime % time.Hour) / time.Minute)
			candidate := time.Date(local.Year(), local.Month(), local.Day()+dayOffset,
				hour, minute, 0, 0, loc)
			if candidate.After(after) {
				return candidate
			}
		}
	}
	// 理论上不可达：dayTimes 非空时 dayOffset=1 起必然构造出未来时刻。
	return time.Time{}
}

// IntervalSchedule 是按固定小时间隔触发的时刻表：每个本地日从零点起
// 每 Hours 小时一格，越过日界时回落到次日本地零点。
//
// 为什么按日零点锚定而不是「上次触发 + Hours 小时」：重启、多副本与
// Redis 抖动都不会改变节奏，任意副本在任意时刻算出的槽位完全一致。
// Hours 不能整除 24 时，当天最后一格短于 Hours（Hours=5 →
// 00/05/10/15/20/次日 00），所以对外只说「每 N 小时」，不承诺「每天整 N 次」。
type IntervalSchedule struct {
	Hours int
	Loc   *time.Location
}

// Location 返回解释间隔所用的时区；Loc 为 nil 时按 UTC。
func (s IntervalSchedule) Location() *time.Location {
	if s.Loc == nil {
		return time.UTC
	}
	return s.Loc
}

// Next 返回严格晚于 after 的下一个槽位。
//
// 槽位一律用 time.Date 按本地日历（日期 + 小时）构造，而不是对本地零点
// 做 Add：DST 切换当天的日界仍落在本地零点，槽位的墙上时间也不会被时差
// 偏移。Hours<=0 属于配置错误，返回零时间（没有下次），不 panic 也不回显 after。
func (s IntervalSchedule) Next(after time.Time) time.Time {
	if s.Hours <= 0 {
		return time.Time{}
	}
	// 超过 24 小时的间隔每天最多命中一次（格子从当天零点起，第二格必然
	// 越过日界），按 24 处理等价，同时避免大数值在秒/小时换算时溢出。
	hours := min(s.Hours, 24)
	loc := s.Location()
	local := after.In(loc)
	stepSecs := int64(hours) * 3600
	// dayOffset 上限为 2：当天或次日必然命中，多留一天覆盖 DST 跳变。
	for dayOffset := 0; dayOffset <= 2; dayOffset++ {
		year, month, day := local.Year(), local.Month(), local.Day()+dayOffset
		dayEnd := time.Date(year, month, day+1, 0, 0, 0, 0, loc)
		firstSlot := int64(0)
		if dayOffset == 0 {
			// 当天第一个可能晚于 after 的槽位：墙上时间按秒切格。
			secs := int64(local.Hour())*3600 + int64(local.Minute())*60 + int64(local.Second())
			firstSlot = secs/stepSecs + 1
		}
		for slot := firstSlot; slot*int64(hours) < 24; slot++ {
			candidate := time.Date(year, month, day, int(slot)*hours, 0, 0, 0, loc)
			if candidate.After(after) {
				return candidate
			}
		}
		// 当天槽位用尽：最后一格是短格，收尾在次日本地零点。
		if dayEnd.After(after) {
			return dayEnd
		}
	}
	return time.Time{}
}
