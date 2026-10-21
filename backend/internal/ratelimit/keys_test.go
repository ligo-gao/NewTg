package ratelimit

// A9：键形状逐字冻结（security-07 §2.2.2 / relay-01 §4.1 / M5 简报第 2 项）。
// 任何前缀、分隔符或维度改动都必须让本测试变红。

import "testing"

func TestKeyShapes(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"B10 user", UserKey(42), "rateLimit:42"},
		{"B10 user zero", UserKey(0), "rateLimit:0"},
		{"B11 token", TokenKey(9), "rateLimit:token:9"},
		{"B12 user global", UserGlobalKey(42), "rateLimit:v2:user:GR:42"},
		{"B12 user global zero", UserGlobalKey(0), "rateLimit:v2:user:GR:0"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s 键 = %q，冻结形状 = %q", tc.name, tc.got, tc.want)
		}
	}
}

func TestKeyPrefixes(t *testing.T) {
	if UserKeyPrefix != "rateLimit:" {
		t.Errorf("B10 前缀 = %q，冻结值 = %q", UserKeyPrefix, "rateLimit:")
	}
	if TokenKeyPrefix != "rateLimit:token:" {
		t.Errorf("B11 前缀 = %q，冻结值 = %q", TokenKeyPrefix, "rateLimit:token:")
	}
	if UserGlobalKeyPrefix != "rateLimit:v2:user:GR:" {
		t.Errorf("B12 前缀 = %q，冻结值 = %q", UserGlobalKeyPrefix, "rateLimit:v2:user:GR:")
	}
}
