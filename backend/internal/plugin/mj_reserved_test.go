package plugin

// W8/MJ-a2：`/mj` 是根 mux 的内建保留族（R-MJ-08）。
//
// R-09 目前只允许插件声明 `/v1/` 路径，因此插件无法在今天占用 `/mj`；本用例
// 冻结的是「保留族判定」本身，防止将来 R-09 放开前缀后 `/mj` 下的插件路由
// 变成注册成功但永远不可达（ServeMux 的内建模式优先）。

import "testing"

func TestMJIsReservedBuiltinRouteFamily(t *testing.T) {
	cases := []struct {
		path   string
		family string
	}{
		{"/mj", "/mj"},
		{"/mj/", "/mj"},
		{"/mj/submit/imagine", "/mj"},
		{"/mj/task/x/fetch", "/mj"},
		{"mj/submit/imagine", "/mj"},
		{"/v1/models", "/v1/models"},
		{"/v1/models/gpt-4o", "/v1/models"},
		{"/v1/tasks/x", ""},
		{"/mjson", ""},
	}
	for _, tc := range cases {
		if got := builtinReservedFamilyFor(tc.path); got != tc.family {
			t.Errorf("builtinReservedFamilyFor(%q) = %q, want %q", tc.path, got, tc.family)
		}
	}
}
