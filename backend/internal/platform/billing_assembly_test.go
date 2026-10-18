package platform

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"strings"
	"testing"

	"gorm.io/gorm"

	"newtg/gateway/internal/billing/pricing"
	"newtg/gateway/internal/config"
)

// 本文件覆盖 gateway/worker 共享的计费进程级装配（N2/N5）：
//   - 定价源构造必须写入进程级 QPU（admin API 的元↔积分换算基准）；
//   - 非法 QPU 必须让装配失败；
//   - 钳制审计必须接到 slog 的 Error 级（billing-07 §4.5）。
//
// 测试用 &gorm.DB{} 占位：NewSource 在构造期只读配置、不访问数据库。

// billingAssemblyConfigReader 是 pricing.ConfigReader 的内存实现。
type billingAssemblyConfigReader map[string]json.RawMessage

func (m billingAssemblyConfigReader) Get(key string) (json.RawMessage, bool) {
	v, ok := m[key]
	return v, ok
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestInitBillingAssemblySetsQuotaPerUnit 钉死 N2：共享装配以配置中心的
// QPU 构造定价源，进程级 CurrentQuotaPerUnit 随之生效（gateway 的管理面
// list/preview 读的就是这个值）。
func TestInitBillingAssemblySetsQuotaPerUnit(t *testing.T) {
	old := pricing.CurrentQuotaPerUnit()
	t.Cleanup(func() {
		_ = pricing.SetQuotaPerUnit(old)
		pricing.SetClampLogger(nil)
	})

	reader := billingAssemblyConfigReader{
		"billing.quota_per_unit": json.RawMessage("200"),
	}
	source, err := initBillingAssembly(&gorm.DB{}, reader,
		&config.Config{NodeName: "node-a"}, discardLogger(), "gateway")
	if err != nil {
		t.Fatalf("initBillingAssembly: %v", err)
	}
	if source == nil {
		t.Fatal("initBillingAssembly returned nil source")
	}
	if got := pricing.CurrentQuotaPerUnit(); got != 200 {
		t.Fatalf("CurrentQuotaPerUnit = %d, want 200（QPU 未从配置中心生效）", got)
	}
}

// TestInitBillingAssemblyRejectsInvalidQuotaPerUnit 验证非法 QPU（越界、
// 非整数、非数字）让启动失败，且不会改动当前生效值。
func TestInitBillingAssemblyRejectsInvalidQuotaPerUnit(t *testing.T) {
	old := pricing.CurrentQuotaPerUnit()
	t.Cleanup(func() { _ = pricing.SetQuotaPerUnit(old) })
	if err := pricing.SetQuotaPerUnit(123); err != nil {
		t.Fatalf("seed QPU: %v", err)
	}

	cases := map[string]json.RawMessage{
		"zero":      json.RawMessage("0"),
		"too-large": json.RawMessage("1000001"),
		"fraction":  json.RawMessage("100.5"),
		"not-num":   json.RawMessage(`"abc"`),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			reader := billingAssemblyConfigReader{"billing.quota_per_unit": raw}
			if _, err := initBillingAssembly(&gorm.DB{}, reader,
				&config.Config{NodeName: "node-a"}, discardLogger(), "gateway"); err == nil {
				t.Fatalf("initBillingAssembly(%s) = nil error, want error", raw)
			}
			if got := pricing.CurrentQuotaPerUnit(); got != 123 {
				t.Fatalf("CurrentQuotaPerUnit = %d, want unchanged 123", got)
			}
		})
	}
}

// TestConfigureClampLoggerLogsSysError 钉死 N5：钳制钩子以 Error 级输出，
// 并带 role/node 上下文与 ClampAuditMap 的四个字段。
func TestConfigureClampLoggerLogsSysError(t *testing.T) {
	t.Cleanup(func() { pricing.SetClampLogger(nil) })

	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	configureClampLogger(log, &config.Config{NodeName: "node-7"}, "worker")

	if _, clamp := pricing.QuotaFromFloatChecked(math.Inf(1)); clamp == nil {
		t.Fatal("QuotaFromFloatChecked(+Inf) returned no clamp")
	}
	out := buf.String()
	for _, want := range []string{
		"level=ERROR", "billing quota clamp", "role=worker", "node=node-7", "overflow",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("clamp log missing %q: %s", want, out)
		}
	}
}
