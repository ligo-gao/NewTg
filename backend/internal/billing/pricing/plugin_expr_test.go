package pricing

import (
	"errors"
	"testing"

	"newtg/gateway/internal/contract"
)

// 本文件验收 T5 任务计价表达式的解析顺序、fail-closed 与求值换算。
// 设计依据：billing-02 §2.6、billing-05 §3.1/§3.2/§3.4、billing-00 §3.1。

func taskFactFrom(nums map[string]float64) contract.TaskUsageFact {
	fact := contract.TaskUsageFact{Facts: map[string]contract.UsageFactValue{}}
	for k, v := range nums {
		value := v
		fact.Facts[k] = contract.UsageFactValue{Num: &value}
	}
	return fact
}

func fullTaskSetting() BillingSetting {
	return BillingSetting{
		PluginBillingExpr: map[string]string{
			"sample::client-model":   `u("seconds") * 1`,
			"sample::upstream-model": `u("seconds") * 2`,
		},
		BillingMode: map[string]string{
			"client-model":   "tiered_expr",
			"upstream-model": "tiered_expr",
		},
		BillingExpr: map[string]string{
			"client-model":   `u("seconds") * 3`,
			"upstream-model": `u("seconds") * 4`,
		},
	}
}

// TestResolveTaskExprPriorityOrder 验证四级顺序不可调换（P-06）。
func TestResolveTaskExprPriorityOrder(t *testing.T) {
	cfg := fullTaskSetting()

	e, src, err := ResolveTaskExpr(cfg, "sample", "client-model", "upstream-model")
	if err != nil || e == nil {
		t.Fatalf("level1: e=%v err=%v", e, err)
	}
	if src != ExprSourcePluginClient || e.Source != `u("seconds") * 1` {
		t.Fatalf("level1 命中错误：src=%s expr=%q", src, e.Source)
	}

	delete(cfg.PluginBillingExpr, "sample::client-model")
	e, src, err = ResolveTaskExpr(cfg, "sample", "client-model", "upstream-model")
	if err != nil || e == nil || src != ExprSourcePluginMapped || e.Source != `u("seconds") * 2` {
		t.Fatalf("level2 命中错误：e=%v src=%s err=%v", e, src, err)
	}

	delete(cfg.PluginBillingExpr, "sample::upstream-model")
	e, src, err = ResolveTaskExpr(cfg, "sample", "client-model", "upstream-model")
	if err != nil || e == nil || src != ExprSourceClientTiered || e.Source != `u("seconds") * 3` {
		t.Fatalf("level3 命中错误：e=%v src=%s err=%v", e, src, err)
	}

	delete(cfg.BillingMode, "client-model")
	e, src, err = ResolveTaskExpr(cfg, "sample", "client-model", "upstream-model")
	if err != nil || e == nil || src != ExprSourceMappedExpr || e.Source != `u("seconds") * 4` {
		t.Fatalf("level4 命中错误：e=%v src=%s err=%v", e, src, err)
	}

	// 四级全无 → 未配置（调用方按预扣额结清），不是错误。
	delete(cfg.BillingMode, "upstream-model")
	e, src, err = ResolveTaskExpr(cfg, "sample", "client-model", "upstream-model")
	if err != nil || e != nil || src != "" {
		t.Fatalf("未配置应返回 (nil,\"\",nil)，got e=%v src=%s err=%v", e, src, err)
	}
}

// TestResolveTaskExprMappedEqualsClientSkipsDuplicate 验证 mapped==client 时
// 不重复求值第 2/4 级。用「只有 mapped 键不存在、client 级已配置」的场景
// 断言命中来源是 client_tiered，而不是 mapped_expr。
func TestResolveTaskExprMappedEqualsClientSkipsDuplicate(t *testing.T) {
	cfg := BillingSetting{
		PluginBillingExpr: map[string]string{},
		BillingMode:       map[string]string{"m": "tiered_expr"},
		BillingExpr:       map[string]string{"m": `u("seconds") * 5`},
	}
	e, src, err := ResolveTaskExpr(cfg, "sample", "m", "m")
	if err != nil || e == nil || src != ExprSourceClientTiered {
		t.Fatalf("got e=%v src=%s err=%v", e, src, err)
	}
}

// TestResolveTaskExprPluginBlankFailClosed 验证 R-37：插件键存在但为空/空白
// 是配置错误，不得回退到下一级。
func TestResolveTaskExprPluginBlankFailClosed(t *testing.T) {
	for _, blank := range []string{"", "   ", "\t\n"} {
		cfg := fullTaskSetting()
		cfg.PluginBillingExpr["sample::client-model"] = blank
		if _, _, err := ResolveTaskExpr(cfg, "sample", "client-model", "upstream-model"); !errors.Is(err, ErrInvalidSpec) {
			t.Fatalf("blank %q: err=%v, want ErrInvalidSpec", blank, err)
		}
	}
}

// TestResolveTaskExprRejectsInvalidSpec 验证插件覆盖里的 token 变量/未知函数
// 在解析期即被拒绝（不会拖到结算期才失败）。
func TestResolveTaskExprRejectsInvalidSpec(t *testing.T) {
	for _, src := range []string{`p * 2`, `u("seconds") + c`, `tier("x", 1)`, `u(name)`} {
		cfg := fullTaskSetting()
		cfg.PluginBillingExpr["sample::client-model"] = src
		if _, _, err := ResolveTaskExpr(cfg, "sample", "client-model", "upstream-model"); err == nil {
			t.Fatalf("%q 应被拒绝", src)
		}
	}
}

// TestTaskExprOrNilSkipsTokenExpression 验证模型级表达式不是任务表达式时
// 跳过而不是报错（否则所有 tiered token 模型都会卡在 settlement_pending）。
func TestTaskExprOrNilSkipsTokenExpression(t *testing.T) {
	cfg := BillingSetting{
		PluginBillingExpr: map[string]string{},
		BillingMode:       map[string]string{"m": "tiered_expr"},
		BillingExpr:       map[string]string{"m": "p * 2 + c * 8"},
	}
	e, src, err := ResolveTaskExpr(cfg, "sample", "m", "m")
	if err != nil || e != nil || src != "" {
		t.Fatalf("token 表达式应被跳过：e=%v src=%s err=%v", e, src, err)
	}
}

// TestEvalTaskUsageConvertsUSDToCredits 验证任务表达式按
// 「USD × QuotaPerUnit × groupRatio」（不除 1e6）换算。
func TestEvalTaskUsageConvertsUSDToCredits(t *testing.T) {
	prev := CurrentQuotaPerUnit()
	if err := SetQuotaPerUnit(100); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = SetQuotaPerUnit(prev) })

	e, err := CompileTaskExpr(`u("seconds") * 0.7`)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	// 10 秒 × $0.7 = $7 → 700 积分。
	credits, err := EvalTaskUsage(e, taskFactFrom(map[string]float64{"seconds": 10}))
	if err != nil || credits != 700 {
		t.Fatalf("credits=%d err=%v, want 700", credits, err)
	}
	// 分组倍率 2 → 1400。
	credits, err = EvalTaskUsage(e.WithGroupRatio(2), taskFactFrom(map[string]float64{"seconds": 10}))
	if err != nil || credits != 1400 {
		t.Fatalf("group2 credits=%d err=%v, want 1400", credits, err)
	}
}

// TestEvalTaskUsageMissingOrNonNumericFailsClosed 验证缺失/字符串字段报错，
// 由调用方保留预扣（不用 0 补齐）。
func TestEvalTaskUsageMissingOrNonNumericFailsClosed(t *testing.T) {
	e, err := CompileTaskExpr(`u("seconds") * 0.4`)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if _, err := EvalTaskUsage(e, taskFactFrom(map[string]float64{})); !errors.Is(err, ErrTaskUsageMissing) {
		t.Fatalf("missing field err=%v", err)
	}
	str := "pro"
	fact := contract.TaskUsageFact{Facts: map[string]contract.UsageFactValue{
		"seconds": {Str: &str},
	}}
	if _, err := EvalTaskUsage(e, fact); !errors.Is(err, ErrTaskUsageMissing) {
		t.Fatalf("string field err=%v", err)
	}
}

// TestEvalTaskUsageConstantAndMinimum 验证常量表达式与最小计费口径。
func TestEvalTaskUsageConstantAndMinimum(t *testing.T) {
	prev := CurrentQuotaPerUnit()
	if err := SetQuotaPerUnit(100); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = SetQuotaPerUnit(prev) })

	e, err := CompileTaskExpr("0")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if credits, err := EvalTaskUsage(e, contract.TaskUsageFact{}); err != nil || credits != 0 {
		t.Fatalf("explicit zero: credits=%d err=%v", credits, err)
	}
	e, err = CompileTaskExpr("0.004")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if credits, err := EvalTaskUsage(e, contract.TaskUsageFact{}); err != nil || credits != 1 {
		t.Fatalf("positive rounds to zero must bill 1: credits=%d err=%v", credits, err)
	}
}

// TestCompileTaskExprEmptyFailClosed 验证空表达式 fail-closed。
func TestCompileTaskExprEmptyFailClosed(t *testing.T) {
	for _, src := range []string{"", "  ", "\n\t"} {
		if _, err := CompileTaskExpr(src); !errors.Is(err, ErrEmptyTaskExpression) {
			t.Fatalf("%q: err=%v", src, err)
		}
	}
}

// TestCompileTaskExprAcceptsSpacedUsageCall 验证 u ( "x" ) 的空白容忍。
func TestCompileTaskExprAcceptsSpacedUsageCall(t *testing.T) {
	e, err := CompileTaskExpr(`u ( "seconds" ) * 1`)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if credits, err := EvalTaskUsage(e.WithGroupRatio(1), taskFactFrom(map[string]float64{"seconds": 1})); err != nil || credits != 100 {
		t.Fatalf("credits=%d err=%v", credits, err)
	}
}
