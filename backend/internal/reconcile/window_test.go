package reconcile

import (
	"testing"
	"time"
)

func TestDayWindowUsesAccountTimeZone(t *testing.T) {
	sh, _ := time.LoadLocation("Asia/Shanghai")
	start, end := DayWindow(time.Date(2026, 10, 25, 0, 0, 0, 0, sh), sh)
	if !start.Equal(time.Date(2026, 10, 24, 16, 0, 0, 0, time.UTC)) || !end.Equal(start.Add(24*time.Hour)) {
		t.Fatalf("Shanghai day = %v..%v", start, end)
	}
}

// 同一天在账号时区的归属：UTC 下午 16:30 已经是上海的次日。
func TestDayWindowConvertsInstantToAccountTimeZone(t *testing.T) {
	sh, _ := time.LoadLocation("Asia/Shanghai")
	start, end := DayWindow(time.Date(2026, 10, 24, 16, 30, 0, 0, time.UTC), sh)
	if want := time.Date(2026, 10, 25, 0, 0, 0, 0, sh); !start.Equal(want) {
		t.Fatalf("start = %v, want %v", start, want)
	}
	if want := time.Date(2026, 10, 26, 0, 0, 0, 0, sh); !end.Equal(want) {
		t.Fatalf("end = %v, want %v", end, want)
	}
}

func TestDayWindowHandlesDSTDays(t *testing.T) {
	ny, _ := time.LoadLocation("America/New_York")
	start, end := DayWindow(time.Date(2026, 11, 1, 0, 0, 0, 0, ny), ny)
	if hours := end.Sub(start).Hours(); hours != 25 {
		t.Fatalf("2026-11-01 in New York = %v hours, want 25", hours)
	}
}

func TestParseAccountTimeZone(t *testing.T) {
	loc, err := ParseAccountTimeZone("Asia/Shanghai")
	if err != nil || loc == nil || loc.String() != "Asia/Shanghai" {
		t.Fatalf("ParseAccountTimeZone(Asia/Shanghai) = %v,%v", loc, err)
	}
	if _, err := ParseAccountTimeZone("Not/AZone"); err == nil {
		t.Fatal("an invalid IANA name must return an error")
	}
}
