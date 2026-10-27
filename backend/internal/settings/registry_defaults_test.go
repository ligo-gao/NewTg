package settings_test

import (
	"testing"
	"time"

	"newtg/gateway/internal/settings"
	"newtg/gateway/internal/tasks"
)

// TestTaskPollDefaultsMatchPollerConstants 守护「注册表默认」与「运行期默认」
// 的唯一性（W9-B4 / M3）：两处各自维护常量，漂移时管理面展示的默认值与
// 轮询器实际回落值会不一致，且不会有任何编译错误。这里把二者钉在一起。
//
// 本用例放在外部测试包（settings_test）：tasks → plugin → egress → settings
// 的依赖链在 settings 同包测试里会形成 import cycle；外部测试包按
// settings_test → tasks 编译，不构成环。
func TestTaskPollDefaultsMatchPollerConstants(t *testing.T) {
	cases := map[string]int64{
		"task.poll_timeout_minutes": int64(tasks.DefaultTaskTimeout / time.Minute),
		"task.poll_max_failures":    int64(tasks.DefaultMaxPollFailures),
	}
	for key, want := range cases {
		setting, ok := settings.Lookup(key)
		if !ok {
			t.Fatalf("设置键 %q 未登记", key)
		}
		got, ok := setting.Default.(int64)
		if !ok {
			t.Fatalf("%q 默认值类型 = %T，期望 int64", key, setting.Default)
		}
		if got != want {
			t.Fatalf("%q 注册表默认 = %d，运行期默认 = %d（默认值漂移）", key, got, want)
		}
	}
}

// TestPerfSectionDefaults 守护 perf section 的注册形状（性能页改版设计 §7.4）。
//
// 管理页按这四键读写测速参数；键名、默认值或上下界漂移会让页面与 worker
// 对不上。上下界只存在于服务端写校验（线上 item 不带 min/max 字段），
// 因此只能通过 settings.Lookup 读到，这里逐一钉死。
func TestPerfSectionDefaults(t *testing.T) {
	if !settings.IsKnownSection("perf") {
		t.Fatal("perf section 未登记")
	}
	rows, ok := settings.Section("perf")
	if !ok {
		t.Fatal("perf section 未登记")
	}

	int64Ptr := func(v int64) *int64 { return &v }
	sameBound := func(got, want *int64) bool {
		if got == nil || want == nil {
			return got == nil && want == nil
		}
		return *got == *want
	}

	want := []struct {
		key      string
		typ      settings.Type
		def      any
		min, max *int64
	}{
		{"perf.schedule.enabled", settings.TypeBool, true, nil, nil},
		{"perf.schedule.interval_hours", settings.TypeInt, int64(12), int64Ptr(1), int64Ptr(168)},
		{"perf.schedule.runs", settings.TypeInt, int64(3), int64Ptr(1), int64Ptr(5)},
		{"perf.schedule.concurrency", settings.TypeInt, int64(2), int64Ptr(1), int64Ptr(8)},
	}
	if len(rows) != len(want) {
		t.Fatalf("perf section 行数 = %d，期望 %d", len(rows), len(want))
	}

	for i, w := range want {
		row := rows[i]
		if row.Key != w.key {
			t.Fatalf("perf 第 %d 行键 = %q，期望 %q", i+1, row.Key, w.key)
		}
		if row.Type != w.typ {
			t.Fatalf("%q 类型 = %v，期望 %v", w.key, row.Type, w.typ)
		}
		if row.Default != w.def {
			t.Fatalf("%q 默认值 = %v（%T），期望 %v（%T）", w.key, row.Default, row.Default, w.def, w.def)
		}
		if !sameBound(row.MinInt, w.min) || !sameBound(row.MaxInt, w.max) {
			t.Fatalf("%q 上下界 = %v/%v，期望 %v/%v", w.key, row.MinInt, row.MaxInt, w.min, w.max)
		}
		if row.Public {
			t.Fatalf("%q 是非公开键，不得进入 /api/status", w.key)
		}
		if row.Secret {
			t.Fatalf("%q 不是密钥类，不得登记为 secret", w.key)
		}
	}

	// 显式复核管理页最依赖的间隔上下界（brief Step 1 的钉死项）。
	interval, ok := settings.Lookup("perf.schedule.interval_hours")
	if !ok {
		t.Fatal("perf.schedule.interval_hours 未登记")
	}
	if interval.MinInt == nil || *interval.MinInt != 1 || interval.MaxInt == nil || *interval.MaxInt != 168 {
		t.Fatalf("interval_hours 上下界 = %v/%v，期望 1/168", interval.MinInt, interval.MaxInt)
	}
}
