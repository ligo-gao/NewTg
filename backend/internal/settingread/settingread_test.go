package settingread

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// fakeReader 用 map 模拟配置中心读取面；键不存在时返回 ok=false。
type fakeReader map[string]json.RawMessage

func (f fakeReader) Get(key string) (json.RawMessage, bool) {
	raw, ok := f[key]
	return raw, ok
}

func captureLog() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

func TestIntFallsBack(t *testing.T) {
	cases := []struct {
		name     string
		reader   Reader
		key      string
		want     int64
		wantOK   bool
		wantWarn bool
	}{
		{name: "nil reader", reader: nil, key: "task.poll_max_failures"},
		{
			name:   "missing key",
			reader: fakeReader{},
			key:    "task.poll_max_failures",
		},
		{
			name:     "json null",
			reader:   fakeReader{"task.poll_max_failures": json.RawMessage("null")},
			key:      "task.poll_max_failures",
			wantWarn: true,
		},
		{
			name:     "negative value",
			reader:   fakeReader{"task.poll_max_failures": json.RawMessage("-1")},
			key:      "task.poll_max_failures",
			wantWarn: true,
		},
		{
			name:     "non-integer",
			reader:   fakeReader{"task.poll_max_failures": json.RawMessage(`"abc"`)},
			key:      "task.poll_max_failures",
			wantWarn: true,
		},
		{
			name:   "zero is a valid value",
			reader: fakeReader{"task.poll_max_failures": json.RawMessage("0")},
			key:    "task.poll_max_failures",
			want:   0,
			wantOK: true,
		},
		{
			name:   "present value",
			reader: fakeReader{"task.poll_max_failures": json.RawMessage("20")},
			key:    "task.poll_max_failures",
			want:   20,
			wantOK: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			log, buf := captureLog()
			got, ok := Int(tc.reader, tc.key, log)
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("Int() = (%d, %v), want (%d, %v)", got, ok, tc.want, tc.wantOK)
			}
			warned := strings.Contains(buf.String(), "worker:")
			if warned != tc.wantWarn {
				t.Fatalf("warn log present = %v, want %v; log=%q", warned, tc.wantWarn, buf.String())
			}
		})
	}
}

func TestBoolFallsBack(t *testing.T) {
	cases := []struct {
		name     string
		reader   Reader
		fallback bool
		want     bool
		wantWarn bool
	}{
		{name: "nil reader", reader: nil, fallback: true, want: true},
		{name: "missing key", reader: fakeReader{}, fallback: true, want: true},
		{
			name:     "json null",
			reader:   fakeReader{"perf.schedule.enabled": json.RawMessage("null")},
			fallback: true,
			want:     true,
			wantWarn: true,
		},
		{
			name:     "string is not a bool",
			reader:   fakeReader{"perf.schedule.enabled": json.RawMessage(`"true"`)},
			fallback: true,
			want:     true,
			wantWarn: true,
		},
		{
			name:     "number is not a bool",
			reader:   fakeReader{"perf.schedule.enabled": json.RawMessage("1")},
			fallback: false,
			want:     false,
			wantWarn: true,
		},
		{
			name:     "explicit false wins over true fallback",
			reader:   fakeReader{"perf.schedule.enabled": json.RawMessage("false")},
			fallback: true,
			want:     false,
		},
		{
			name:     "explicit true",
			reader:   fakeReader{"perf.schedule.enabled": json.RawMessage("true")},
			fallback: false,
			want:     true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			log, buf := captureLog()
			got := Bool(tc.reader, "perf.schedule.enabled", tc.fallback, log)
			if got != tc.want {
				t.Fatalf("Bool() = %v, want %v", got, tc.want)
			}
			warned := strings.Contains(buf.String(), "worker:")
			if warned != tc.wantWarn {
				t.Fatalf("warn log present = %v, want %v; log=%q", warned, tc.wantWarn, buf.String())
			}
		})
	}
}

func TestBoolOKReportsUsability(t *testing.T) {
	cases := []struct {
		name     string
		reader   Reader
		fallback bool
		want     bool
		wantOK   bool
	}{
		{name: "nil reader", reader: nil, fallback: true, want: true},
		{name: "missing key", reader: fakeReader{}, fallback: true, want: true},
		{
			name:     "json null",
			reader:   fakeReader{"perf.schedule.enabled": json.RawMessage("null")},
			fallback: true,
			want:     true,
		},
		{
			name:     "non-bool",
			reader:   fakeReader{"perf.schedule.enabled": json.RawMessage(`"true"`)},
			fallback: true,
			want:     true,
		},
		{
			name:     "explicit true",
			reader:   fakeReader{"perf.schedule.enabled": json.RawMessage("true")},
			fallback: false,
			want:     true,
			wantOK:   true,
		},
		{
			name:     "explicit false",
			reader:   fakeReader{"perf.schedule.enabled": json.RawMessage("false")},
			fallback: true,
			want:     false,
			wantOK:   true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			log, _ := captureLog()
			got, ok := BoolOK(tc.reader, "perf.schedule.enabled", tc.fallback, log)
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("BoolOK() = (%v, %v), want (%v, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestPresent(t *testing.T) {
	if Present(nil, "perf.schedule.runs") {
		t.Fatal("Present(nil) = true, want false")
	}
	if Present(fakeReader{}, "perf.schedule.runs") {
		t.Fatal("Present(missing) = true, want false")
	}
	// 键存在即算落库，哪怕值是 JSON null：是否可用由 Int/Bool 判定。
	if !Present(fakeReader{"perf.schedule.runs": json.RawMessage("null")}, "perf.schedule.runs") {
		t.Fatal("Present(null) = false, want true")
	}
}
