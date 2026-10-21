package platform

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"newtg/gateway/internal/tasks"
)

// 本文件验收 W9-B4 的配置映射（不依赖 PG/Redis）：
// 未配置 → nil（交给轮询器默认）；显式 0 → 指向 0 的指针（禁用）；
// 正值 → 按值；坏值/负值 → 回落默认并 warn，绝不 panic。

type fakeSettingReader map[string]json.RawMessage

func (f fakeSettingReader) Get(key string) (json.RawMessage, bool) {
	raw, ok := f[key]
	return raw, ok
}

func newLimitsLog() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

func TestTaskPollLimitsUnsetReturnsNil(t *testing.T) {
	log, buf := newLimitsLog()
	timeout, maxFailures := taskPollLimits(fakeSettingReader{}, log)
	if timeout != nil || maxFailures != nil {
		t.Fatalf("unset must stay nil (design default), got %v/%v", timeout, maxFailures)
	}
	if buf.Len() != 0 {
		t.Fatalf("unset must not warn: %s", buf.String())
	}
}

func TestTaskPollLimitsExplicitZeroPreserved(t *testing.T) {
	timeout, maxFailures := taskPollLimits(fakeSettingReader{
		settingTaskPollTimeoutMinutes: json.RawMessage(`0`),
		settingTaskPollMaxFailures:    json.RawMessage(`0`),
	}, slog.Default())
	if timeout == nil || *timeout != 0 {
		t.Fatalf("explicit zero timeout must be a pointer to 0, got %v", timeout)
	}
	if maxFailures == nil || *maxFailures != 0 {
		t.Fatalf("explicit zero maxFailures must be a pointer to 0, got %v", maxFailures)
	}
}

func TestTaskPollLimitsPositiveValues(t *testing.T) {
	timeout, maxFailures := taskPollLimits(fakeSettingReader{
		settingTaskPollTimeoutMinutes: json.RawMessage(`90`),
		settingTaskPollMaxFailures:    json.RawMessage(`7`),
	}, slog.Default())
	if timeout == nil || *timeout != 90*time.Minute {
		t.Fatalf("timeout = %v, want 90m", timeout)
	}
	if maxFailures == nil || *maxFailures != 7 {
		t.Fatalf("maxFailures = %v, want 7", maxFailures)
	}
}

func TestTaskPollLimitsDefaultsMatchPollerConstants(t *testing.T) {
	timeout, maxFailures := taskPollLimits(fakeSettingReader{
		settingTaskPollTimeoutMinutes: json.RawMessage(`1440`),
		settingTaskPollMaxFailures:    json.RawMessage(`20`),
	}, slog.Default())
	if timeout == nil || *timeout != tasks.DefaultTaskTimeout {
		t.Fatalf("1440 minutes must equal DefaultTaskTimeout: %v", timeout)
	}
	if maxFailures == nil || *maxFailures != tasks.DefaultMaxPollFailures {
		t.Fatalf("20 must equal DefaultMaxPollFailures: %v", maxFailures)
	}
}

func TestTaskPollLimitsBadValuesFallBackWithWarning(t *testing.T) {
	cases := []struct {
		name     string
		timeout  string
		failures string
	}{
		{"string", `"abc"`, `"abc"`},
		{"fraction", `1.5`, `1.5`},
		{"bool", `true`, `true`},
		{"object", `{"v":1}`, `{"v":1}`},
		{"negative", `-1`, `-1`},
		// 2^53-1 分钟 > MaxInt64/time.Minute：必须被溢出护栏拦下。
		{"overflow-minutes", `9007199254740991`, `"abc"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			log, buf := newLimitsLog()
			timeout, maxFailures := taskPollLimits(fakeSettingReader{
				settingTaskPollTimeoutMinutes: json.RawMessage(tc.timeout),
				settingTaskPollMaxFailures:    json.RawMessage(tc.failures),
			}, log)
			if timeout != nil || maxFailures != nil {
				t.Fatalf("bad value %q/%q must fall back to design default, got %v/%v",
					tc.timeout, tc.failures, timeout, maxFailures)
			}
			logged := buf.String()
			if !strings.Contains(logged, settingTaskPollTimeoutMinutes) ||
				!strings.Contains(logged, settingTaskPollMaxFailures) {
				t.Fatalf("bad value must warn for both keys, got: %s", logged)
			}
		})
	}
}

func TestTaskPollLimitsIndependentKeys(t *testing.T) {
	log, buf := newLimitsLog()
	timeout, maxFailures := taskPollLimits(fakeSettingReader{
		settingTaskPollTimeoutMinutes: json.RawMessage(`0`),
		settingTaskPollMaxFailures:    json.RawMessage(`"oops"`),
	}, log)
	if timeout == nil || *timeout != 0 {
		t.Fatalf("valid key must survive a bad sibling, timeout=%v", timeout)
	}
	if maxFailures != nil {
		t.Fatalf("bad sibling must fall back, maxFailures=%v", maxFailures)
	}
	if !strings.Contains(buf.String(), settingTaskPollMaxFailures) {
		t.Fatalf("only the bad key should warn: %s", buf.String())
	}
}

func TestTaskPollLimitsNilReader(t *testing.T) {
	timeout, maxFailures := taskPollLimits(nil, slog.Default())
	if timeout != nil || maxFailures != nil {
		t.Fatalf("nil reader must mean unset, got %v/%v", timeout, maxFailures)
	}
}

func TestDescribeLimitsDistinguishesUnsetFromZero(t *testing.T) {
	zero := 0
	zeroTimeout := time.Duration(0)
	if got := describeTimeout(nil); !strings.Contains(got, "default") {
		t.Fatalf("nil timeout must describe as default: %q", got)
	}
	if got := describeTimeout(&zeroTimeout); got == "default" || got != "0s" {
		t.Fatalf("explicit zero must not describe as default: %q", got)
	}
	if got := describeMaxFailures(nil); !strings.Contains(got, "default") {
		t.Fatalf("nil maxFailures must describe as default: %q", got)
	}
	if got := describeMaxFailures(&zero); got != "0" {
		t.Fatalf("explicit zero maxFailures = %q, want 0", got)
	}
}
