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
