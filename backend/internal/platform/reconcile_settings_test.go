package platform

// 任务 10：上游用量对账的装配契约。
//
// 覆盖：
//   - 设置适配器（reconcile.SettingsReader 的生产实现）：六个键的读取、
//     缺省/脏值回落 reconcile.DefaultSettings()；
//   - reconcile.enabled 谓词：缺省开启，显式 false 关闭；
//   - bind 之后任务有执行体、超时与固定阻断码；
//   - runner 的 Outcome 状态投影到运行历史允许的三个值。

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"newtg/gateway/internal/configcenter"
	"newtg/gateway/internal/reconcile"
	"newtg/gateway/internal/worker"
)

func TestReconcileSettingsUnsetUsesDesignDefaults(t *testing.T) {
	log, buf := newLimitsLog()
	reader := newReconcileSettingsReader(fakeSettingReader{}, log)
	got, err := reader.ReconcileSettings(context.Background())
	if err != nil {
		t.Fatalf("ReconcileSettings: %v", err)
	}
	if got != reconcile.DefaultSettings() {
		t.Fatalf("settings = %+v, want defaults %+v", got, reconcile.DefaultSettings())
	}
	if buf.Len() != 0 {
		t.Fatalf("unset keys must not warn: %s", buf.String())
	}
}

func TestReconcileSettingsReadsEveryKey(t *testing.T) {
	log, _ := newLimitsLog()
	reader := newReconcileSettingsReader(fakeSettingReader{
		"reconcile.enabled":                json.RawMessage(`false`),
		"reconcile.lookback_days":          json.RawMessage(`7`),
		"reconcile.token_tolerance_bp":     json.RawMessage(`250`),
		"reconcile.min_delta_tokens":       json.RawMessage(`12345`),
		"reconcile.amount_tolerance_minor": json.RawMessage(`700`),
		"billing.quota_per_unit":           json.RawMessage(`1000`),
	}, log)
	got, err := reader.ReconcileSettings(context.Background())
	if err != nil {
		t.Fatalf("ReconcileSettings: %v", err)
	}
	want := reconcile.Settings{
		Enabled:              false,
		LookbackDays:         7,
		TokenBP:              250,
		MinDeltaTokens:       12345,
		AmountToleranceMinor: 700,
		QuotaPerUnit:         1000,
	}
	if got != want {
		t.Fatalf("settings = %+v, want %+v", got, want)
	}
}

// 脏值不是「另一种开关」：null / 负数 / 错类型都必须回落设计默认并 warn。
func TestReconcileSettingsInvalidValuesFallBack(t *testing.T) {
	log, buf := newLimitsLog()
	reader := newReconcileSettingsReader(fakeSettingReader{
		"reconcile.enabled":                json.RawMessage(`null`),
		"reconcile.lookback_days":          json.RawMessage(`-1`),
		"reconcile.token_tolerance_bp":     json.RawMessage(`"lots"`),
		"reconcile.min_delta_tokens":       json.RawMessage(`null`),
		"reconcile.amount_tolerance_minor": json.RawMessage(`-5`),
		"billing.quota_per_unit":           json.RawMessage(`null`),
	}, log)
	got, err := reader.ReconcileSettings(context.Background())
	if err != nil {
		t.Fatalf("ReconcileSettings: %v", err)
	}
	if got != reconcile.DefaultSettings() {
		t.Fatalf("settings = %+v, want defaults %+v", got, reconcile.DefaultSettings())
	}
	if buf.Len() == 0 {
		t.Fatal("invalid values must warn")
	}
}

// 导出的构造器是 HTTP 装配层复用的入口：空配置中心必须等于注册表默认值。
func TestNewReconcileSettingsReaderWithEmptyCenterUsesDefaults(t *testing.T) {
	reader := NewReconcileSettingsReader(configcenter.New(nil, nil), nil)
	got, err := reader.ReconcileSettings(context.Background())
	if err != nil {
		t.Fatalf("ReconcileSettings: %v", err)
	}
	if got != reconcile.DefaultSettings() {
		t.Fatalf("settings = %+v, want defaults %+v", got, reconcile.DefaultSettings())
	}
}

func TestReconcileEnabledDefaultsTrueWithoutCenter(t *testing.T) {
	if !reconcileEnabled(nil) {
		t.Fatal("reconcile.enabled must default to true")
	}
}

func TestReconcileEnabledReadsTheSwitch(t *testing.T) {
	on := newReconcileSettingsReader(fakeSettingReader{}, nil)
	if !on.Enabled() {
		t.Fatal("unset switch must read as enabled")
	}
	off := newReconcileSettingsReader(fakeSettingReader{
		"reconcile.enabled": json.RawMessage(`false`),
	}, nil)
	if off.Enabled() {
		t.Fatal("explicit false must read as disabled")
	}
}

// TestUpstreamReconcileTaskBindsRunAndEnabledGate 锁定装配结果：bind 之后
// 任务必须有可执行体（否则手动触发只会留下永不执行的 pending 行），
// 且挂了 reconcile.enabled 谓词与固定阻断码。
//
// 空 deps 也要能完成绑定：装配层只构造仓储句柄，真正的数据库错误留到
// 执行期——与既有 holiday_sync/log_cleanup 的装配口径一致。
func TestUpstreamReconcileTaskBindsRunAndEnabledGate(t *testing.T) {
	reg, err := worker.DefaultRegistry()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	if err := bindWorkerTaskRuns(reg, workerTaskDeps{}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	task, ok := reg.Get("upstream_usage_reconcile")
	if !ok {
		t.Fatal("upstream_usage_reconcile is not registered")
	}
	if task.Run == nil {
		t.Fatal("upstream_usage_reconcile has no bound Run")
	}
	if task.Timeout != 5*time.Minute {
		t.Fatalf("timeout = %v, want 5m", task.Timeout)
	}
	if task.BlockedCode != "reconcile_disabled" {
		t.Fatalf("blocked code = %q, want reconcile_disabled", task.BlockedCode)
	}
	if task.Enabled == nil {
		t.Fatal("upstream_usage_reconcile has no enabled predicate")
	}
	// 没有配置中心（装配不完整）时按注册表默认开启：配置缺失不该静默停跑对账。
	if !task.Enabled() {
		t.Fatal("enabled predicate should default to true without a config center")
	}
}

// worker.Result 的 status 只允许 succeeded|partial|failed；runner 的
// skipped（没有可处理账号）与 disabled（开关关闭兜底）收敛为 succeeded。
func TestReconcileResultStatusIsLimitedToTheWorkerContract(t *testing.T) {
	cases := map[string]string{
		"succeeded": "succeeded",
		"partial":   "partial",
		"failed":    "failed",
		"skipped":   "succeeded",
		"disabled":  "succeeded",
	}
	for in, want := range cases {
		if got := reconcileResultStatus(in); got != want {
			t.Fatalf("reconcileResultStatus(%q) = %q, want %q", in, got, want)
		}
	}
}
