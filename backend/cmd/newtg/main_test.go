package main

import "testing"

func TestResolveRole(t *testing.T) {
	cases := []struct {
		name string
		env  string
		args []string
		want string
	}{
		{"env 与 arg 均空，默认 gateway", "", nil, "gateway"},
		{"arg 为 worker", "", []string{"worker"}, "worker"},
		{"env 为 migrate", "migrate", nil, "migrate"},
		{"env 优先于 arg", "worker", []string{"gateway"}, "worker"},
		{"拼写错误保留原值，交给 switch fatal", "woker", nil, "woker"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveRole(tc.env, tc.args); got != tc.want {
				t.Errorf("resolveRole(%q, %v) = %q, want %q", tc.env, tc.args, got, tc.want)
			}
		})
	}
}

// resolveRole 不得读取真实环境变量，必须只依赖传入参数。
func TestResolveRole_DoesNotReadRealEnv(t *testing.T) {
	t.Setenv("RUN_MODE", "worker")
	if got := resolveRole("", nil); got != "gateway" {
		t.Errorf("resolveRole 读取了真实环境: got %q, want %q", got, "gateway")
	}
}
