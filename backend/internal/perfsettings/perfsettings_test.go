package perfsettings

import (
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"newtg/gateway/internal/modelperf"
)

type fakeReader map[string]json.RawMessage

func (f fakeReader) Get(key string) (json.RawMessage, bool) {
	raw, ok := f[key]
	return raw, ok
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testDefaults() Defaults {
	return Defaults{
		Enabled:     true,
		Times:       "00:00,12:00",
		Timezone:    "Asia/Shanghai",
		Runs:        3,
		Concurrency: 2,
	}
}

func mustLoadLocation(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("LoadLocation(%q): %v", name, err)
	}
	return loc
}

func TestSettingsFallsBackToEnvWhenKeysAbsent(t *testing.T) {
	r := New(nil, testDefaults(), discardLogger())
	got := r.Settings(time.Now())

	if got.Source != "env" {
		t.Fatalf("Source = %q, want env", got.Source)
	}
	if !got.Enabled || got.IntervalHours != 0 || got.Runs != 3 || got.Concurrency != 2 {
		t.Fatalf("unexpected env fallback: %+v", got)
	}
	if got.Timezone != "Asia/Shanghai" || got.LegacyTimes != "00:00,12:00" {
		t.Fatalf("timezone/legacy times not from defaults: %+v", got)
	}
}

func TestSettingsUsesPersistedValues(t *testing.T) {
	reader := fakeReader{
		"perf.schedule.enabled":        json.RawMessage("false"),
		"perf.schedule.interval_hours": json.RawMessage("6"),
		"perf.schedule.runs":           json.RawMessage("5"),
		"perf.schedule.concurrency":    json.RawMessage("4"),
	}
	r := newWithReader(reader, testDefaults(), discardLogger())
	got := r.Settings(time.Now())

	if got.Source != "settings" {
		t.Fatalf("Source = %q, want settings", got.Source)
	}
	if got.Enabled || got.IntervalHours != 6 || got.Runs != 5 || got.Concurrency != 4 {
		t.Fatalf("unexpected persisted values: %+v", got)
	}
	// 时区/时刻表仍是环境变量面，只由 Defaults 提供。
	if got.Timezone != "Asia/Shanghai" || got.LegacyTimes != "00:00,12:00" {
		t.Fatalf("timezone/legacy times not from defaults: %+v", got)
	}
}

func TestSettingsMixedSource(t *testing.T) {
	reader := fakeReader{"perf.schedule.interval_hours": json.RawMessage("8")}
	r := newWithReader(reader, testDefaults(), discardLogger())
	got := r.Settings(time.Now())

	if got.Source != "mixed" {
		t.Fatalf("Source = %q, want mixed", got.Source)
	}
	if got.IntervalHours != 8 {
		t.Fatalf("IntervalHours = %d, want 8", got.IntervalHours)
	}
	if got.Runs != 3 || got.Concurrency != 2 || !got.Enabled {
		t.Fatalf("absent keys must use env defaults: %+v", got)
	}
}

func TestSettingsDirtyValueFallsBack(t *testing.T) {
	for _, raw := range []string{`"abc"`, "null", "-5"} {
		t.Run(raw, func(t *testing.T) {
			reader := fakeReader{"perf.schedule.interval_hours": json.RawMessage(raw)}
			r := newWithReader(reader, testDefaults(), discardLogger())
			got := r.Settings(time.Now())

			// 键存在（Source 仍记为 mixed），但值不可用 → 按未落库处理，走环境变量时刻表。
			if got.Source != "mixed" {
				t.Fatalf("Source = %q, want mixed (key is present, value dirty)", got.Source)
			}
			if got.IntervalHours != 0 {
				t.Fatalf("IntervalHours = %d, want 0 for dirty %s", got.IntervalHours, raw)
			}
			if got.Runs != 3 || got.Concurrency != 2 || !got.Enabled {
				t.Fatalf("dirty interval must not disturb other keys: %+v", got)
			}

			schedule, _ := r.Schedule(time.Now())
			if _, ok := schedule.(modelperf.DailyTimes); !ok {
				t.Fatalf("Schedule() = %T, want modelperf.DailyTimes for dirty %s", schedule, raw)
			}
		})
	}
}

func TestSettingsNilCenterUsesEnv(t *testing.T) {
	r := New(nil, testDefaults(), discardLogger())
	got := r.Settings(time.Now())
	if got.Source != "env" || !got.Enabled || got.Runs != 3 || got.Concurrency != 2 {
		t.Fatalf("New(nil) must behave like all keys absent: %+v", got)
	}
}

func TestScheduleLegacyTimesWhenIntervalAbsent(t *testing.T) {
	r := New(nil, testDefaults(), discardLogger())
	loc := mustLoadLocation(t, "Asia/Shanghai")
	now := time.Date(2026, 10, 27, 13, 0, 0, 0, loc)

	schedule, settings := r.Schedule(now)
	daily, ok := schedule.(modelperf.DailyTimes)
	if !ok {
		t.Fatalf("Schedule() = %T, want modelperf.DailyTimes", schedule)
	}
	want := time.Date(2026, 10, 28, 0, 0, 0, 0, loc)
	if got := daily.Next(now); !got.Equal(want) {
		t.Fatalf("Next(%v) = %v, want %v", now, got, want)
	}
	if settings.IntervalHours != 0 || settings.Source != "env" {
		t.Fatalf("unexpected settings: %+v", settings)
	}
}

func TestScheduleIntervalWhenPersisted(t *testing.T) {
	reader := fakeReader{"perf.schedule.interval_hours": json.RawMessage("5")}
	r := newWithReader(reader, testDefaults(), discardLogger())
	loc := mustLoadLocation(t, "Asia/Shanghai")
	now := time.Date(2026, 10, 27, 13, 0, 0, 0, loc)

	schedule, settings := r.Schedule(now)
	interval, ok := schedule.(modelperf.IntervalSchedule)
	if !ok {
		t.Fatalf("Schedule() = %T, want modelperf.IntervalSchedule", schedule)
	}
	if interval.Hours != 5 {
		t.Fatalf("Hours = %d, want 5", interval.Hours)
	}
	if got := interval.Location().String(); got != loc.String() {
		t.Fatalf("Loc = %v, want %v", got, loc)
	}
	if settings.IntervalHours != 5 {
		t.Fatalf("Settings.IntervalHours = %d, want 5", settings.IntervalHours)
	}
}

func TestNextRunMatchesScheduleFor(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reader fakeReader
	}{
		{name: "env", reader: fakeReader{}},
		{name: "interval", reader: fakeReader{"perf.schedule.interval_hours": json.RawMessage("7")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newWithReader(tc.reader, testDefaults(), discardLogger())
			loc := mustLoadLocation(t, "Asia/Shanghai")
			now := time.Date(2026, 10, 27, 13, 17, 0, 0, loc)

			schedule, _ := r.Schedule(now)
			want := schedule.Next(now)
			if got := r.NextRun(now); !got.Equal(want) {
				t.Fatalf("NextRun(%v) = %v, want %v", now, got, want)
			}
		})
	}
}

func TestScheduleInvalidTimezoneFallsBackToUTC(t *testing.T) {
	defaults := testDefaults()
	defaults.Timezone = "Not/AZone"
	r := New(nil, defaults, discardLogger())
	now := time.Date(2026, 10, 27, 13, 0, 0, 0, time.UTC)

	schedule, _ := r.Schedule(now)
	if schedule.Location() != time.UTC {
		t.Fatalf("Location() = %v, want UTC", schedule.Location())
	}
	// 00:00/12:00 UTC：13:00 之后的下一次是次日 00:00。
	want := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	if got := r.NextRun(now); !got.Equal(want) {
		t.Fatalf("NextRun(%v) = %v, want %v", now, got, want)
	}
}

func TestScheduleInvalidTimesFallBackToMidnightAndNoon(t *testing.T) {
	defaults := testDefaults()
	defaults.Timezone = "UTC"
	defaults.Times = "25:99,noon"
	r := New(nil, defaults, discardLogger())
	now := time.Date(2026, 10, 27, 13, 0, 0, 0, time.UTC)

	schedule, _ := r.Schedule(now)
	daily, ok := schedule.(modelperf.DailyTimes)
	if !ok {
		t.Fatalf("Schedule() = %T, want modelperf.DailyTimes", schedule)
	}
	want := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	if got := daily.Next(now); !got.Equal(want) {
		t.Fatalf("Next(%v) = %v, want %v (fallback 00:00/12:00)", now, got, want)
	}
}
