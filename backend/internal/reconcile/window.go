package reconcile

import (
	"fmt"
	"strings"
	"time"
)

// DayWindow 返回 date 所在账号时区的自然日半开区间 [start, end)。
//
// 日界必须由账号时区决定：先把这个瞬间转换到账号时区取出年月日，再用
// time.Date(y, m, d+1, 0, 0, 0, 0, loc) 构造次日零点。绝不能对 start 加
// 24 小时——夏令时切换日的一天可能是 23 或 25 小时。
//
// loc 为 nil 时按 UTC 处理。
func DayWindow(date time.Time, loc *time.Location) (start, end time.Time) {
	if loc == nil {
		loc = time.UTC
	}
	y, m, d := date.In(loc).Date()
	start = time.Date(y, m, d, 0, 0, 0, 0, loc)
	end = time.Date(y, m, d+1, 0, 0, 0, 0, loc)
	return start, end
}

// ParseAccountTimeZone 解析账号的 IANA 时区名（如 "Asia/Shanghai"）。
//
// 空字符串与 "UTC" 一样返回 UTC；非法名称返回错误，以便配置错误在上线时
// 暴露，而不是静默按某个默认时区对账。
func ParseAccountTimeZone(name string) (*time.Location, error) {
	name = strings.TrimSpace(name)
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("reconcile: invalid account time zone %q: %w", name, err)
	}
	return loc, nil
}
