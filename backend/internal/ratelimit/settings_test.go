package ratelimit

// A10：阈值解析——缺键回落注册表默认（60/0/0）、脏值回落、group_overrides
// 损坏忽略覆盖且不 panic、覆盖只作用于 B10（R-M5-06/R-M5-11/R-M5-14）。

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"testing"
)

// fakeConfig 是 ConfigReader 的内存实现；同 configcenter，未知键返回 ok=false。
type fakeConfig map[string]string

func (f fakeConfig) Get(key string) (json.RawMessage, bool) {
	raw, ok := f[key]
	if !ok {
		return nil, false
	}
	return json.RawMessage(raw), true
}

// captureLog 记录 warn 及以上日志，用于验证「回落时记 warn」。
type captureLog struct {
	mu       sync.Mutex
	warnings []string
}

func (c *captureLog) Enabled(context.Context, slog.Level) bool { return true }

func (c *captureLog) Handle(_ context.Context, r slog.Record) error {
	if r.Level >= slog.LevelWarn {
		c.mu.Lock()
		c.warnings = append(c.warnings, r.Message)
		c.mu.Unlock()
	}
	return nil
}

func (c *captureLog) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *captureLog) WithGroup(string) slog.Handler      { return c }

func (c *captureLog) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.warnings)
}

func newTestLogger() (*slog.Logger, *captureLog) {
	sink := &captureLog{}
	return slog.New(sink), sink
}

func TestResolveFallsBackToRegistryDefaults(t *testing.T) {
	for name, config := range map[string]ConfigReader{
		"nil config":   nil,
		"empty config": fakeConfig{},
		// 只落了 token_rpm：其余三个键必须各自回落默认。
		"partial config": fakeConfig{KeyTokenRPM: "5"},
	} {
		t.Run(name, func(t *testing.T) {
			log, _ := newTestLogger()
			s := Resolve(config, log)
			if s.UserModelRPM != 60 {
				t.Errorf("UserModelRPM = %d，注册表默认 = 60", s.UserModelRPM)
			}
			if s.UserGlobalRPM != 0 {
				t.Errorf("UserGlobalRPM = %d，注册表默认 = 0", s.UserGlobalRPM)
			}
			wantToken := int64(0)
			if name == "partial config" {
				wantToken = 5
			}
			if s.TokenRPM != wantToken {
				t.Errorf("TokenRPM = %d，期望 %d", s.TokenRPM, wantToken)
			}
			if s.GroupOverrides == nil || len(s.GroupOverrides) != 0 {
				t.Errorf("GroupOverrides = %v，期望空表", s.GroupOverrides)
			}
			if got := s.UserModelLimit("any"); got != 60 {
				t.Errorf("UserModelLimit(any) = %d，期望 60", got)
			}
			if got := s.TokenLimit(); got != int(wantToken) {
				t.Errorf("TokenLimit() = %d，期望 %d", got, wantToken)
			}
			if got := s.UserGlobalLimit(); got != 0 {
				t.Errorf("UserGlobalLimit() = %d，期望 0", got)
			}
		})
	}
}

func TestResolveReadsConfiguredValues(t *testing.T) {
	log, _ := newTestLogger()
	s := Resolve(fakeConfig{
		KeyUserModelRPM:   "120",
		KeyTokenRPM:       "30",
		KeyUserGlobalRPM:  "300",
		KeyGroupOverrides: `{"vip":1200,"free":0}`,
	}, log)

	if s.UserModelRPM != 120 || s.TokenRPM != 30 || s.UserGlobalRPM != 300 {
		t.Fatalf("解析结果 = %d/%d/%d，期望 120/30/300", s.UserModelRPM, s.TokenRPM, s.UserGlobalRPM)
	}
	if got := s.UserModelLimit("vip"); got != 1200 {
		t.Errorf("UserModelLimit(vip) = %d，期望覆盖值 1200", got)
	}
	if got := s.UserModelLimit("free"); got != 0 {
		t.Errorf("UserModelLimit(free) = %d，期望覆盖值 0（该组不限）", got)
	}
	if got := s.UserModelLimit("default"); got != 120 {
		t.Errorf("UserModelLimit(default) = %d，未覆盖组应走 120", got)
	}
	// R-M5-06：覆盖只作用于 B10，不得影响 token/global 桶。
	if got := s.TokenLimit(); got != 30 {
		t.Errorf("TokenLimit() = %d，覆盖不得作用于 B11", got)
	}
	if got := s.UserGlobalLimit(); got != 300 {
		t.Errorf("UserGlobalLimit() = %d，覆盖不得作用于 B12", got)
	}
}

func TestResolveDirtyScalarsFallBack(t *testing.T) {
	cases := map[string]string{
		"non-integer": `60.5`,
		"negative":    `-1`,
		"null":        `null`,
		"string":      `"60"`,
		"malformed":   `{`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			log, sink := newTestLogger()
			s := Resolve(fakeConfig{KeyUserModelRPM: raw}, log)
			if s.UserModelRPM != 60 {
				t.Fatalf("UserModelRPM = %d，脏值必须回落注册表默认 60", s.UserModelRPM)
			}
			if sink.count() == 0 {
				t.Error("脏值回落必须记 warn")
			}
		})
	}
}

func TestResolveCorruptGroupOverridesIgnored(t *testing.T) {
	log, sink := newTestLogger()
	// 整体损坏（不是 JSON 对象）：忽略全部覆盖、保留其余阈值、绝不 panic。
	s := Resolve(fakeConfig{
		KeyUserModelRPM:   "90",
		KeyGroupOverrides: `{"vip":`,
	}, log)

	if s.UserModelRPM != 90 {
		t.Errorf("UserModelRPM = %d，损坏的覆盖不应影响标量阈值", s.UserModelRPM)
	}
	if len(s.GroupOverrides) != 0 {
		t.Errorf("GroupOverrides = %v，损坏时必须整体忽略", s.GroupOverrides)
	}
	if got := s.UserModelLimit("vip"); got != 90 {
		t.Errorf("UserModelLimit(vip) = %d，损坏时应走默认 90", got)
	}
	if sink.count() == 0 {
		t.Error("损坏的 group_overrides 必须记 warn")
	}
}

func TestResolvePerEntryGroupOverrideValidation(t *testing.T) {
	log, sink := newTestLogger()
	s := Resolve(fakeConfig{
		KeyGroupOverrides: `{"vip":1200,"free":0,"neg":-1,"frac":1.5,"text":"x","nul":null}`,
	}, log)

	want := map[string]int64{"vip": 1200, "free": 0}
	if len(s.GroupOverrides) != len(want) {
		t.Fatalf("GroupOverrides = %v，期望只保留 %v", s.GroupOverrides, want)
	}
	for group, value := range want {
		if got, ok := s.GroupOverrides[group]; !ok || got != value {
			t.Errorf("GroupOverrides[%q] = %d/%v，期望 %d", group, got, ok, value)
		}
	}
	// null 不得静默变成 0（0 = 不限，会把拼错的分组配置放大成无限制）。
	if _, ok := s.GroupOverrides["nul"]; ok {
		t.Error("null 覆盖值必须被丢弃，而不是当成 0")
	}
	// 坏条目只是回落到默认阈值（60），不会让整个表失效。
	if got := s.UserModelLimit("frac"); got != 60 {
		t.Errorf("UserModelLimit(frac) = %d，坏条目应回落默认 60", got)
	}
	if sink.count() < 4 {
		t.Errorf("warn 次数 = %d，四个坏条目应各记一条", sink.count())
	}
}
