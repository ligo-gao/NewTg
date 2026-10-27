package modelperf

import (
	"testing"
	"time"
)

func shanghai(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("load Asia/Shanghai: %v", err)
	}
	return loc
}

func TestParseTimesRejectsBadInput(t *testing.T) {
	loc := shanghai(t)
	for _, raw := range []string{"", "24:00", "12:60", "1200", "12:00,", "aa:bb", "12:0", "+12:00", "-0:30"} {
		if _, err := ParseTimes(raw, loc); err == nil {
			t.Fatalf("ParseTimes(%q) accepted invalid input", raw)
		}
	}
	sched, err := ParseTimes("12:00, 00:00", loc)
	if err != nil {
		t.Fatalf("ParseTimes valid input: %v", err)
	}
	// 00:00 排在 12:00 前面：Next 依赖升序。
	after := time.Date(2026, 10, 27, 0, 0, 0, 0, loc).Add(-time.Second)
	if got := sched.Next(after); !got.Equal(time.Date(2026, 10, 27, 0, 0, 0, 0, loc)) {
		t.Fatalf("Next = %v, want 2026-10-27T00:00+08:00", got)
	}
}

func TestNextIsStrictAndCrossesDays(t *testing.T) {
	loc := shanghai(t)
	sched, _ := ParseTimes("00:00,12:00", loc)

	// 恰好命中时刻 → 下一个时段，不是同一个。
	at := time.Date(2026, 10, 27, 12, 0, 0, 0, loc)
	if got := sched.Next(at); !got.Equal(time.Date(2026, 10, 28, 0, 0, 0, 0, loc)) {
		t.Fatalf("Next(12:00) = %v, want next day 00:00", got)
	}

	// UTC 输入必须按目标时区解释（13:00+08:00 == 05:00Z）。
	// 13:00 已过当天 12:00，严格晚于它的下一个时段是次日 00:00。
	utc := time.Date(2026, 10, 27, 5, 0, 0, 0, time.UTC)
	if got := sched.Next(utc); !got.Equal(time.Date(2026, 10, 28, 0, 0, 0, 0, loc)) {
		t.Fatalf("Next(05:00Z) = %v, want 2026-10-28T00:00+08:00", got)
	}
}

func TestNextSameDayNilLocationAndDuplicates(t *testing.T) {
	loc := shanghai(t)
	sched, err := ParseTimes("00:00,12:00", loc)
	if err != nil {
		t.Fatalf("ParseTimes: %v", err)
	}

	// 未到当天最后一个时刻 → 返回当天的下一个时刻。
	after := time.Date(2026, 10, 27, 10, 0, 0, 0, loc)
	if got := sched.Next(after); !got.Equal(time.Date(2026, 10, 27, 12, 0, 0, 0, loc)) {
		t.Fatalf("Next(10:00) = %v, want 2026-10-27T12:00+08:00", got)
	}

	// loc 为 nil 时按 UTC 解释。
	utcSched, err := ParseTimes("09:30", nil)
	if err != nil {
		t.Fatalf("ParseTimes(nil loc): %v", err)
	}
	if got := utcSched.Next(time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)); !got.Equal(
		time.Date(2026, 10, 27, 9, 30, 0, 0, time.UTC)) {
		t.Fatalf("Next(nil loc) = %v, want 2026-10-27T09:30Z", got)
	}

	// 重复时刻不影响严格晚于语义。
	dup, err := ParseTimes("12:00,12:00", loc)
	if err != nil {
		t.Fatalf("ParseTimes(duplicate): %v", err)
	}
	if got := dup.Next(time.Date(2026, 10, 27, 12, 0, 0, 0, loc)); !got.Equal(
		time.Date(2026, 10, 28, 12, 0, 0, 0, loc)) {
		t.Fatalf("Next(duplicate slots) = %v, want next day 12:00", got)
	}
}

func TestNextFollowsDST(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load America/New_York: %v", err)
	}
	sched, _ := ParseTimes("00:00,12:00", loc)
	// 2026-03-08 凌晨 2 点进入夏令时。05:30Z = 00:30 EST，00:00 已过；
	// 下一个时段是当地 12:00 EDT = 16:00Z。固定 -05:00 偏移会算成 17:00Z。
	after := time.Date(2026, 3, 8, 5, 30, 0, 0, time.UTC)
	want := time.Date(2026, 3, 8, 16, 0, 0, 0, time.UTC)
	if got := sched.Next(after); !got.Equal(want) {
		t.Fatalf("Next across DST = %v, want %v", got.UTC(), want)
	}
}
