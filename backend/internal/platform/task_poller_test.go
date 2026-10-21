package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/plugin"
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
		// JSON null 解到 int64 会返回零值且无错误：必须与「显式 0」区分，
		// 否则会静默禁用兜底（fail-open）。
		{"null", `null`, `null`},
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

// ---- 接线用例（I1）---------------------------------------------------------
//
// buildTaskPollers 是 startTaskPoller 唯一的装配路径。这里用最小 fake 走
// 同一函数，并从两个 poller 的 accessor 读回**生效值**：删除插件侧或 MJ
// 侧的 Timeout/MaxFailures 赋值都会让用例变红，而不是静默回落默认。

type wiringRepo struct{}

func (wiringRepo) ListPendingByFamily(context.Context, string, int) ([]*tasks.Task, error) {
	return nil, nil
}

func (wiringRepo) ListReconcilable(context.Context, string, int) ([]*tasks.Task, error) {
	return nil, nil
}

func (wiringRepo) GetChannel(context.Context, int64) (*tasks.ChannelTarget, error) {
	return nil, nil
}

func (wiringRepo) ApplyPoll(context.Context, int64, tasks.State, tasks.PollUpdate) (bool, error) {
	return false, nil
}

func (wiringRepo) UpdateBilling(context.Context, int64, *int64, map[string]any) error {
	return nil
}

type wiringPlugins struct{}

func (wiringPlugins) Snapshot() *plugin.Snapshot   { return &plugin.Snapshot{} }
func (wiringPlugins) Reload(context.Context) error { return nil }

type wiringBilling struct{}

func (wiringBilling) TerminateByRequestIDForToken(string, int64, contract.UsageFact) error {
	return nil
}

func (wiringBilling) TerminateByRequestIDForTokenCost(string, int64, int64, contract.UsageFact) error {
	return nil
}

func (wiringBilling) RefundByRequestIDForToken(string, int64) error { return nil }

// RefreshInflightLeases 是 R-W9B-02 的续租接缝；装配测试不关心租约，
// 实现为 no-op 即可让 fake 满足 tasks.BillingHandoff。
func (wiringBilling) RefreshInflightLeases(context.Context, []string) error { return nil }

func TestBuildTaskPollersWiresLimitsIntoBothPollers(t *testing.T) {
	cases := []struct {
		name        string
		reader      fakeSettingReader
		wantTimeout time.Duration
		wantMax     int
	}{
		{"unset", fakeSettingReader{}, tasks.DefaultTaskTimeout, tasks.DefaultMaxPollFailures},
		{"zero", fakeSettingReader{
			settingTaskPollTimeoutMinutes: json.RawMessage(`0`),
			settingTaskPollMaxFailures:    json.RawMessage(`0`),
		}, 0, 0},
		{"positive", fakeSettingReader{
			settingTaskPollTimeoutMinutes: json.RawMessage(`90`),
			settingTaskPollMaxFailures:    json.RawMessage(`7`),
		}, 90 * time.Minute, 7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			poller, mjPoller, err := buildTaskPollers(taskPollerDeps{
				Repo:    wiringRepo{},
				Plugins: wiringPlugins{},
				Billing: wiringBilling{},
				Limits:  tc.reader,
				Log:     slog.Default(),
			})
			if err != nil {
				t.Fatalf("buildTaskPollers: %v", err)
			}
			if got := poller.TimeoutLimit(); got != tc.wantTimeout {
				t.Fatalf("plugin poller timeout = %v, want %v (wiring lost?)", got, tc.wantTimeout)
			}
			if got := poller.MaxFailuresLimit(); got != tc.wantMax {
				t.Fatalf("plugin poller maxFailures = %d, want %d (wiring lost?)", got, tc.wantMax)
			}
			if got := mjPoller.TimeoutLimit(); got != tc.wantTimeout {
				t.Fatalf("mj poller timeout = %v, want %v (wiring lost?)", got, tc.wantTimeout)
			}
			if got := mjPoller.MaxFailuresLimit(); got != tc.wantMax {
				t.Fatalf("mj poller maxFailures = %d, want %d (wiring lost?)", got, tc.wantMax)
			}
		})
	}
}

// TestMJSettlerGuard 冻结 I2 的装配门禁：mj.enabled=true 且没有 MJ 结算
// 驱动时必须拒绝上线；未开启或已注入 handler 时放行。
func TestMJSettlerGuard(t *testing.T) {
	handler := func(context.Context, *tasks.Task) error { return nil }
	cases := []struct {
		name    string
		reader  fakeSettingReader
		handler tasks.MJTaskHandler
		wantErr bool
	}{
		{"disabled without handler", fakeSettingReader{}, nil, false},
		{"enabled without handler", fakeSettingReader{"mj.enabled": json.RawMessage(`true`)}, nil, true},
		{"enabled with handler", fakeSettingReader{"mj.enabled": json.RawMessage(`true`)}, handler, false},
		{"bad value fails closed", fakeSettingReader{"mj.enabled": json.RawMessage(`"yes"`)}, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := mjSettlerGuard(tc.reader, tc.handler)
			if (err != nil) != tc.wantErr {
				t.Fatalf("mjSettlerGuard error = %v, want error=%v", err, tc.wantErr)
			}
		})
	}
}

// TestBuildTaskPollersRefusesMJWithoutSettler 证明守卫接在唯一装配路径上，
// 而不只是一个孤立的辅助函数。
func TestBuildTaskPollersRefusesMJWithoutSettler(t *testing.T) {
	_, _, err := buildTaskPollers(taskPollerDeps{
		Repo:    wiringRepo{},
		Plugins: wiringPlugins{},
		Billing: wiringBilling{},
		Limits:  fakeSettingReader{"mj.enabled": json.RawMessage(`true`)},
		Log:     slog.Default(),
	})
	if err == nil {
		t.Fatal("buildTaskPollers must refuse mj.enabled without an MJ settlement handler")
	}
}
