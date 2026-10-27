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

// IntervalSchedule 是按固定小时间隔触发的时刻表：以固定的本地参考日零点
// （2000-01-01 00:00）为锚点，按民用小时序号每 Hours 小时一格。
//
// 为什么不用「当日零点 + k×Hours」：Hours>24 时当天只有一个零点格，k 恒为 1，
// 选 168（每周）会退化成每天跑，按天烧上游额度。固定参考锚点让 N=48 真的
// 是每 2 天、N=168 真的是每周；N=12 仍与今天的 00:00/12:00 逐点一致。
// 槽位只依赖日历（民用小时序号），因此重启、多副本与 Redis 抖动都不会改变
// 节奏，任意副本在任意时刻算出的槽位完全相同。
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
// 用「固定参考锚点 + 民用小时序号」而不是时长加法：Add(N*time.Hour) 在 DST
// 切换后会让槽位漂移到本地 01:00/13:00；这里按民用小时序号算出槽位后，再用
// time.Date 按本地日历字段重建，槽位始终落在本地整点（N=12 仍是 00:00/12:00）。
// Hours<=0 属于配置错误，返回零时间（没有下次），不 panic 也不回显 after。
func (s IntervalSchedule) Next(after time.Time) time.Time {
	if s.Hours <= 0 {
		return time.Time{}
	}
	loc := s.Location()
	ref := time.Date(2000, 1, 1, 0, 0, 0, 0, loc)
	local := after.In(loc)
	// 民用日差：两端都按 UTC 零点重建后相减，DST 不会把日差算成 23/25 小时。
	refDay := time.Date(ref.Year(), ref.Month(), ref.Day(), 0, 0, 0, 0, time.UTC)
	afterDay := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
	days := int64(afterDay.Sub(refDay) / (24 * time.Hour))
	idx := days*24 + int64(local.Hour())
	// k 取「严格大于 idx 的下一格」：idx/Hours 向下取整后 +1。
	k := idx/int64(s.Hours) + 1
	if idx < 0 && idx%int64(s.Hours) != 0 {
		// Go 的整数除法向零截断；参考锚点之前的时刻要再退一格才是向下取整。
		k--
	}
	// 正常情况下 k 已指向下一格；最多重算 3 次覆盖 DST 重复小时里槽位
	// 构造出来仍不晚于 after 的罕见情形。
	for attempt := 0; attempt < 3; attempt++ {
		slot := k * int64(s.Hours)
		dayOff, hour := slot/24, slot%24
		next := time.Date(ref.Year(), ref.Month(), ref.Day()+int(dayOff), int(hour), 0, 0, 0, loc)
		if next.After(after) {
			return next
		}
		k++
	}
	return time.Time{}
}
