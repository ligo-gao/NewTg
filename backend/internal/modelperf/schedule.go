package modelperf

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Schedule 是每日探测时刻表，时刻按升序保存，时区固定为解析时传入的 loc。
//
// 字段不导出：调度器与本次任务同属 modelperf 包，后续若要对外使用，
// 通过 Location / Next 两个方法即可。
type Schedule struct {
	dayTimes []time.Duration
	loc      *time.Location
}

// ParseTimes 解析形如 "00:00,12:00" 的时刻表，loc 为 nil 时按 UTC 处理。
//
// 空项、非 HH:MM 格式、越界的小时/分钟一律报错，避免把配置错误
// 悄悄变成“某个时刻不跑”。解析成功后按升序排序，Next 依赖该顺序。
func ParseTimes(raw string, loc *time.Location) (Schedule, error) {
	if loc == nil {
		loc = time.UTC
	}
	items := strings.Split(raw, ",")
	dayTimes := make([]time.Duration, 0, len(items))
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item == "" {
			return Schedule{}, fmt.Errorf("schedule %q contains an empty time", raw)
		}
		parts := strings.Split(item, ":")
		if len(parts) != 2 {
			return Schedule{}, fmt.Errorf("invalid time %q: want HH:MM", item)
		}
		hour, err := strconv.Atoi(parts[0])
		if err != nil || hour < 0 || hour > 23 {
			return Schedule{}, fmt.Errorf("invalid hour in %q", item)
		}
		minute, err := strconv.Atoi(parts[1])
		if err != nil || minute < 0 || minute > 59 {
			return Schedule{}, fmt.Errorf("invalid minute in %q", item)
		}
		dayTimes = append(dayTimes,
			time.Duration(hour)*time.Hour+time.Duration(minute)*time.Minute)
	}
	sort.Slice(dayTimes, func(i, j int) bool { return dayTimes[i] < dayTimes[j] })
	return Schedule{dayTimes: dayTimes, loc: loc}, nil
}

// Location 返回时刻表使用的时区。
func (s Schedule) Location() *time.Location {
	return s.loc
}

// Next 返回严格晚于 after 的下一个时刻，按 s.loc 解释 after 与结果。
//
// 不假设时区是固定偏移，也不使用 Add(24h)：逐日逐时刻都用 time.Date 构造，
// 由 Go 根据时区数据库处理夏令时（DST）。dayOffset 上限为 2——时刻都在
// 一天之内，跨一天必然命中，多留一天覆盖 DST 跳变当天的边界情况。
func (s Schedule) Next(after time.Time) time.Time {
	loc := s.loc
	if loc == nil {
		loc = time.UTC
	}
	if len(s.dayTimes) == 0 {
		// 零值 Schedule 没有时刻，按“无下次”返回零时间。
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
