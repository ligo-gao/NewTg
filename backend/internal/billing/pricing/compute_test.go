package pricing

import (
	"errors"
	"math"
	"testing"

	"newtg/gateway/internal/contract"
)

// ratioSnapshot 构造一个完整的 Ratio 模式快照（ComputeQuota 不做默认填充，
// 默认填充属于 Resolve 的职责）。
func ratioSnapshot(prompt, completion, cacheRead, cacheWrite int64) contract.PriceSnapshot {
	return contract.PriceSnapshot{
		Mode: contract.BillingModeRatio,
		Price: contract.PriceData{
			ModelRatio:           float64(prompt),
			CompletionRatio:      float64(completion),
			CacheRatio:           float64(cacheRead),
			CacheCreationRatio:   float64(cacheWrite),
			CacheCreation5mRatio: float64(cacheWrite),
			CacheCreation1hRatio: float64(cacheWrite),
			ImageRatio:           1,
			AudioRatio:           1,
			AudioCompletionRatio: 1,
		},
		GroupRatio: 1,
	}
}

// TestComputeRatioQuotaModuleExample 复现 modules-06 TC-F-06-14-01：
// 输入价 500、输出价 1500（积分/1M），usage 1k/2k → 0.5+3=3.5 → 4 积分。
func TestComputeRatioQuotaModuleExample(t *testing.T) {
	snap := ratioSnapshot(500, 1500, 0, 0)
	q, err := ComputeQuota(snap, contract.TokenParams{P: 1000, C: 2000}, nil)
	if err != nil {
		t.Fatalf("ComputeQuota: %v", err)
	}
	if q != 4 {
		t.Fatalf("quota=%d，期望 4（HALF_UP 3.5→4）", q)
	}
}

func TestComputeRatioAllDimensions(t *testing.T) {
	cases := []struct {
		name   string
		params contract.TokenParams
		want   int64
	}{
		{"缓存读", contract.TokenParams{CR: 1_000_000}, 100},
		{"缓存创建 5m", contract.TokenParams{CC5m: 1_000_000}, 1250},
		{"缓存创建 1h", contract.TokenParams{CC1h: 1_000_000}, 1250},
		{"图片输入（输入价×图片倍率）", contract.TokenParams{P: 0, Img: 1_000_000}, 1000}, // 500×2
		{"图片缓存读（缓存读价）", contract.TokenParams{ImgCR: 1_000_000}, 100},
		{"图片输出（输出价）", contract.TokenParams{ImgO: 1_000_000}, 1500},
		{"音频输入（输入价×音频倍率）", contract.TokenParams{AI: 1_000_000}, 2000}, // 500×4
		{"音频输出（×音频补全倍率）", contract.TokenParams{AO: 1_000_000}, 4000},  // 500×4×2
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			snap := ratioSnapshot(500, 1500, 100, 1250)
			snap.Price.ImageRatio = 2
			snap.Price.AudioRatio = 4
			snap.Price.AudioCompletionRatio = 2
			q, err := ComputeQuota(snap, c.params, nil)
			if err != nil {
				t.Fatalf("ComputeQuota: %v", err)
			}
			if q != c.want {
				t.Fatalf("quota=%d，期望 %d", q, c.want)
			}
		})
	}
}

// TestComputeRatioExplicitFreeInput 覆盖 modules-06 TC-F-06-13-03 的计费侧：
// 输入价显式 0、输出价 >0 时，必须只收输出费（相对倍率表示无法表达该场景）。
func TestComputeRatioExplicitFreeInput(t *testing.T) {
	snap := ratioSnapshot(0, 500, 0, 0)
	q, err := ComputeQuota(snap, contract.TokenParams{P: 1_000_000, C: 1_000_000}, nil)
	if err != nil {
		t.Fatalf("ComputeQuota: %v", err)
	}
	if q != 500 {
		t.Fatalf("quota=%d，期望 500（输入免费、输出 500/1M）", q)
	}
}

func TestComputeRatioMinimumBillingAndNoUsage(t *testing.T) {
	snap := ratioSnapshot(1, 0, 0, 0)
	// 有请求、有价格但取整为 0 → 最小计费 1。
	if q, err := ComputeQuota(snap, contract.TokenParams{P: 1}, nil); err != nil || q != 1 {
		t.Fatalf("最小计费: q=%d err=%v，期望 1", q, err)
	}
	// 无任何计费用量 → 0（不抬成 1）。
	if q, err := ComputeQuota(snap, contract.TokenParams{}, nil); err != nil || q != 0 {
		t.Fatalf("无用量: q=%d err=%v，期望 0", q, err)
	}
	// 价格为 0（显式免费）→ 不触发保底。
	free := ratioSnapshot(0, 0, 0, 0)
	if q, err := ComputeQuota(free, contract.TokenParams{P: 1}, nil); err != nil || q != 0 {
		t.Fatalf("免费模型: q=%d err=%v，期望 0", q, err)
	}
}

func TestComputeRatioGroupRatio(t *testing.T) {
	snap := ratioSnapshot(2, 0, 0, 0)
	snap.GroupRatio = 1.5
	if q, err := ComputeQuota(snap, contract.TokenParams{P: 1_000_000}, nil); err != nil || q != 3 {
		t.Fatalf("q=%d err=%v，期望 3（2×1.5）", q, err)
	}
}

func TestComputeRatioRejectsInvalid(t *testing.T) {
	snap := ratioSnapshot(1, 0, 0, 0)
	snap.Price.ModelRatio = math.NaN()
	if _, err := ComputeQuota(snap, contract.TokenParams{P: 1}, nil); !errors.Is(err, ErrInvalidPrice) {
		t.Fatalf("NaN 价格应拒绝，got %v", err)
	}

	snap = ratioSnapshot(1, 0, 0, 0)
	if _, err := ComputeQuota(snap, contract.TokenParams{P: -1}, nil); !errors.Is(err, ErrInvalidParams) {
		t.Fatalf("负 token 应拒绝，got %v", err)
	}

	snap.GroupRatio = math.NaN()
	if _, err := ComputeQuota(snap, contract.TokenParams{P: 1}, nil); !errors.Is(err, ErrInvalidGroupRatio) {
		t.Fatalf("NaN 分组倍率应拒绝，got %v", err)
	}

	snap = ratioSnapshot(1, 0, 0, 0)
	snap.Mode = contract.BillingMode(99)
	if _, err := ComputeQuota(snap, contract.TokenParams{P: 1}, nil); !errors.Is(err, ErrInvalidMode) {
		t.Fatalf("未知模式应拒绝，got %v", err)
	}
}

func TestComputePerCall(t *testing.T) {
	snap := contract.PriceSnapshot{
		Mode:       contract.BillingModePerCall,
		Price:      contract.PriceData{UsePrice: true, ModelPrice: 150},
		GroupRatio: 1,
	}
	if q, err := ComputeQuota(snap, contract.TokenParams{}, nil); err != nil || q != 150 {
		t.Fatalf("按次 q=%d err=%v，期望 150（usage 不计）", q, err)
	}
	// 图片按次乘 n。
	if q, err := ComputeQuota(snap, contract.TokenParams{ImageCount: 3}, nil); err != nil || q != 450 {
		t.Fatalf("按次×3 q=%d err=%v，期望 450", q, err)
	}
	// 显式免费按次 → 0，不触发保底。
	free := snap
	free.Price.ModelPrice = 0
	if q, err := ComputeQuota(free, contract.TokenParams{}, nil); err != nil || q != 0 {
		t.Fatalf("免费按次 q=%d err=%v，期望 0", q, err)
	}
	// 正价取整为 0 → 最小计费 1。
	tiny := snap
	tiny.Price.ModelPrice = 0.4
	if q, err := ComputeQuota(tiny, contract.TokenParams{}, nil); err != nil || q != 1 {
		t.Fatalf("最小计费 q=%d err=%v，期望 1", q, err)
	}
}

func TestComputeExpressionToken(t *testing.T) {
	restore := setQPU(t, 100)
	defer restore()

	snap := contract.PriceSnapshot{
		Mode:       contract.BillingModeTieredExpr,
		ExprString: "p*2",
		GroupRatio: 1,
	}
	// exprOut = 2e6 ($/1M)；1M tokens → 2e6/1e6×100 = 200 积分。
	q, err := ComputeQuota(snap, contract.TokenParams{P: 1_000_000}, nil)
	if err != nil {
		t.Fatalf("ComputeQuota: %v", err)
	}
	if q != 200 {
		t.Fatalf("quota=%d，期望 200", q)
	}

	// 分组倍率参与表达式换算。
	snap.GroupRatio = 1.5
	if q, err := ComputeQuota(snap, contract.TokenParams{P: 1_000_000}, nil); err != nil || q != 300 {
		t.Fatalf("分组后 q=%d err=%v，期望 300", q, err)
	}
}

func TestComputeExpressionTaskUsageDoesNotDivide(t *testing.T) {
	restore := setQPU(t, 100)
	defer restore()

	snap := contract.PriceSnapshot{
		Mode:       contract.BillingModeTieredExpr,
		ExprString: "p*2",
		GroupRatio: 1,
	}
	// 任务用量语义：exprOut 是本次请求的 USD 成本，不除 1e6。
	q, err := ComputeQuota(snap, contract.TokenParams{P: 1000}, map[string]bool{"u": true})
	if err != nil {
		t.Fatalf("ComputeQuota: %v", err)
	}
	if q != 200_000 {
		t.Fatalf("任务用量 quota=%d，期望 200000（2000×100）", q)
	}
}

func TestComputeExpressionQuotaPerUnit(t *testing.T) {
	restore := setQPU(t, 50)
	defer restore()

	snap := contract.PriceSnapshot{
		Mode:       contract.BillingModeTieredExpr,
		ExprString: "p*2",
		GroupRatio: 1,
	}
	if q, err := ComputeQuota(snap, contract.TokenParams{P: 1_000_000}, nil); err != nil || q != 100 {
		t.Fatalf("QPU=50 q=%d err=%v，期望 100", q, err)
	}
}

func TestComputeExpressionBoundaries(t *testing.T) {
	restore := setQPU(t, 100)
	defer restore()

	// 显式 0（免费叶）→ 0，不被保底抬升。
	zero := contract.PriceSnapshot{
		Mode: contract.BillingModeTieredExpr, ExprString: "p*0", GroupRatio: 1,
	}
	if q, err := ComputeQuota(zero, contract.TokenParams{P: 1_000_000}, nil); err != nil || q != 0 {
		t.Fatalf("表达式 0: q=%d err=%v，期望 0", q, err)
	}

	// 正价格取整为 0 → 1。
	tiny := contract.PriceSnapshot{
		Mode: contract.BillingModeTieredExpr, ExprString: "p", GroupRatio: 1,
	}
	if q, err := ComputeQuota(tiny, contract.TokenParams{P: 1}, nil); err != nil || q != 1 {
		t.Fatalf("表达式最小计费: q=%d err=%v，期望 1", q, err)
	}

	// 负成本 → 配置错误，不得按负额抵扣。
	neg := contract.PriceSnapshot{
		Mode: contract.BillingModeTieredExpr, ExprString: "p-10", GroupRatio: 1,
	}
	if _, err := ComputeQuota(neg, contract.TokenParams{P: 1}, nil); !errors.Is(err, ErrNegativeExpression) {
		t.Fatalf("负成本应拒绝，got %v", err)
	}

	// 编译失败 → error（Task 3 据此保留预扣）。
	bad := contract.PriceSnapshot{
		Mode: contract.BillingModeTieredExpr, ExprString: "p+", GroupRatio: 1,
	}
	if _, err := ComputeQuota(bad, contract.TokenParams{P: 1}, nil); err == nil {
		t.Fatal("非法表达式应返回 error")
	}

	// 缺表达式。
	missing := contract.PriceSnapshot{Mode: contract.BillingModeTieredExpr, GroupRatio: 1}
	if _, err := ComputeQuota(missing, contract.TokenParams{P: 1}, nil); !errors.Is(err, ErrNoExpression) {
		t.Fatalf("缺表达式应报 ErrNoExpression，got %v", err)
	}
}

func TestComputeExpressionClamp(t *testing.T) {
	restore := setQPU(t, 100)
	defer restore()

	snap := contract.PriceSnapshot{
		Mode:       contract.BillingModeTieredExpr,
		ExprString: "p*100000000",
		GroupRatio: 1,
	}
	q, err := ComputeQuota(snap, contract.TokenParams{P: 1_000_000}, nil)
	if err == nil {
		t.Fatal("超大配额应以钳制错误返回")
	}
	clamp, ok := AsClamp(err)
	if !ok || clamp.Kind != contract.ClampOverflow || clamp.Op != "QuotaFromDecimalChecked" {
		t.Fatalf("钳制标记不符: %+v", clamp)
	}
	if q != contract.MaxQuota || clamp.Clamped != contract.MaxQuota {
		t.Fatalf("饱和值应为 MaxQuota，got q=%d clamp=%+v", q, clamp)
	}
}

// setQPU 修改全局兑换比并返回恢复函数。
func setQPU(t *testing.T, n int64) func() {
	t.Helper()
	old := CurrentQuotaPerUnit()
	if err := SetQuotaPerUnit(n); err != nil {
		t.Fatal(err)
	}
	return func() { _ = SetQuotaPerUnit(old) }
}
