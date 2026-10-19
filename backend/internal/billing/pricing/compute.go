package pricing

import (
	"errors"
	"fmt"
	"math"

	"github.com/shopspring/decimal"

	"newtg/gateway/internal/billing/expr"
	"newtg/gateway/internal/contract"
)

// 计算相关的哨兵错误。
var (
	// ErrInvalidParams 表示 TokenParams 含负数等非法计费量。
	ErrInvalidParams = errors.New("pricing: TokenParams 含非法计费量")
	// ErrInvalidGroupRatio 表示分组倍率非有限正数。
	ErrInvalidGroupRatio = errors.New("pricing: 分组倍率必须为有限正数")
	// ErrNegativeExpression 表示表达式产出负成本（配置错误，不得按负额抵扣）。
	ErrNegativeExpression = errors.New("pricing: 表达式产出负成本")
	// ErrNoExpression 表示表达式模式快照缺少表达式。
	ErrNoExpression = errors.New("pricing: 表达式模式快照缺少表达式")
)

// ImageCountUnset 是 TokenParams.ImageCount 的「未设置」约定值。
//
// contract.TokenParams.ImageCount 没有独立的 set 标记，无法区分「未设置」
// 与「结算域有效 0 张」。M3 冻结约定：
//   - -1 = 未设置 → 请求域缺省 1（按次 max(1,n)、表达式 image_count=1）；
//   - >= 0 = 精确张数 → 0 合法且**计 0**（billing-00 §9.1 有效 0 计 0）。
//
// 【调用方必须显式传值】零值 0 现在表示「精确 0 张」，不是未设置。
// T3（会话层）、T7（管理/预览）与 T9（中继装配）在「未设置」场景必须显式
// 传 ImageCountUnset，不得依赖 Go 零值；否则纯文本按次请求会被计 0。
// 契约文件的注释仍写 [1,MaxImageN]（冻结文件不改），以本约定为准。
const ImageCountUnset = -1

// oneMillion 是「每 1M tokens」的换算分母。
var oneMillion = decimal.NewFromInt(1_000_000)

// ComputeQuota 把一次请求的计费量换算为整数积分（冻结接口）。
//
// 公式（billing-00 §7.1，唯一实现）：
//
//	表达式 · token   exprOut($/1M) / 1e6 × QuotaPerUnit × groupRatio
//	表达式 · 任务用量 exprOut(USD) × QuotaPerUnit × groupRatio（不除 1e6）
//	传统倍率         (P·输入价 + C·输出价 + CR·缓存读价 + CC5m·5m 价 + CC1h·1h 价
//	                  + Img·输入价·图片倍率 + ImgCR·缓存读价 + ImgO·输出价
//	                  + AI·输入价·音频倍率 + AO·输入价·音频倍率·音频补全倍率)
//	                 / 1e6 × groupRatio
//	按次             ModelPrice(积分/次) × max(1, image_count) × groupRatio
//
// 任务用量与 token 用量的区分沿用 billing-01 §4.1 的 usedVars 约定：
// usedVars 含函数名 `u` 即为任务表达式（表达式语言里只有任务用量会引用 u）。
//
// 失败与边界（billing-00 §7.5）：
//   - 无任何计费用量（Ratio 模式）→ 0；
//   - 价格为正但结果取整为 0 → 最小计费 1；
//   - 表达式显式 0 → 0（fixed(0)/免费叶合法，不得被保底抬升）；
//   - 表达式产出负成本 → error（配置错误，绝不静默按负额入账）。
//
// 【钳制标记如何返回】冻结签名只有 (int64, error)。发生饱和时返回
// (实际饱和值, *ClampError)，调用方用 AsClamp(err) 取回 QuotaClamp 落
// admin_info.quota_saturation；值本身是审计所需的饱和结果。策略（保留预扣、
// 退款等）由 Task 3 的会话层决定，ComputeQuota 不擅自处理。
func ComputeQuota(snap contract.PriceSnapshot, params contract.TokenParams, usedVars map[string]bool) (int64, error) {
	if err := validateParams(params); err != nil {
		return 0, err
	}
	group := snap.GroupRatio
	// 显式快照可能没有经过 Resolve：缺省分组倍率按 1 处理（billing-02 §4.7）。
	if group == 0 {
		group = 1
	}
	if err := validateGroupRatio(group); err != nil {
		return 0, err
	}

	switch snap.Mode {
	case contract.BillingModeTieredExpr:
		return computeExpressionQuota(snap, params, usedVars, group)
	case contract.BillingModePerCall:
		return computePerCallQuota(snap, params, group)
	case contract.BillingModeRatio:
		return computeRatioQuota(snap, params, group)
	default:
		return 0, fmt.Errorf("%w: %d", ErrInvalidMode, snap.Mode)
	}
}

// validateParams 拒绝负的计费量。
//
// 负 token 若参与乘算会直接抵扣费用（等于退款放大），必须作为调用方 bug
// 显式失败；归一化层已经做过下限钳制（billing-00 §5.2）。
func validateParams(p contract.TokenParams) error {
	vals := map[string]int64{
		"p": p.P, "c": p.C, "len": p.Len, "cr": p.CR,
		"cc5m": p.CC5m, "cc1h": p.CC1h, "img": p.Img, "img_cr": p.ImgCR,
		"img_o": p.ImgO, "ai": p.AI, "ao": p.AO,
	}
	for name, v := range vals {
		if v < 0 {
			return fmt.Errorf("%w: %s=%d", ErrInvalidParams, name, v)
		}
	}
	if p.ImageCount < ImageCountUnset {
		return fmt.Errorf("%w: image_count=%d（-1=未设置，>=0=精确张数）", ErrInvalidParams, p.ImageCount)
	}
	return nil
}

func validateGroupRatio(g float64) error {
	if math.IsNaN(g) || math.IsInf(g, 0) || g <= 0 {
		return fmt.Errorf("%w: %v", ErrInvalidGroupRatio, g)
	}
	return nil
}

// validatePrice 拒绝非法价格/倍率（NaN、±Inf、负值）。
// 0 是合法的显式免费，不在这里拒绝。
func validatePrice(name string, v float64) error {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return fmt.Errorf("%w: %s=%v", ErrInvalidPrice, name, v)
	}
	return nil
}

func dec(v float64) decimal.Decimal { return decimal.NewFromFloat(v) }

// hasBillableUsage 判断是否存在任何计费用量。
//
// Len 只是档位条件，不参与计费；ImageCount 在 Ratio 模式没有单价列，
// 但「有请求、有价格、算出 0」仍应触发最小计费，故计入用量事实。
func hasBillableUsage(p contract.TokenParams) bool {
	return p.P > 0 || p.C > 0 || p.CR > 0 || p.CC5m > 0 || p.CC1h > 0 ||
		p.Img > 0 || p.ImgCR > 0 || p.ImgO > 0 || p.AI > 0 || p.AO > 0 ||
		p.ImageCount > 0
}

// computeRatioQuota 实现传统倍率模式的绝对值价格公式。
//
// 字段单位见包文档：Ratio 类字段一律是「积分 / 1M tokens」，媒体字段是
// 相对输入价的乘数。所有乘法走 decimal，避免 float64 乘积在换算前溢出或丢精度。
//
// 图像张数（R-42）：billing-05 §2.7 的 token 计价路径把张数作为 `"n"` 乘数
// （「图片额度基数（preConsumedTokens × modelRatio）× OtherRatios（含 "n"）」），
// billing-00 §7.1 的传统公式也规定「再乘 OtherRatios」。故本函数在分组倍率
// 之后乘有效张数：
//   - -1（未设置）→ 请求域缺省 1；
//   - 0（结算域有效 0）→ 直接 0（billing-00 §9.1「有效 0 计 0」）；
//   - N（>=1）→ 整个 token 额度 × N（每个图各消耗一次 prompt/图片 token）。
func computeRatioQuota(snap contract.PriceSnapshot, p contract.TokenParams, group float64) (int64, error) {
	pr := snap.Price
	prices := []struct {
		name string
		v    float64
	}{
		{"model_ratio", pr.ModelRatio},
		{"completion_ratio", pr.CompletionRatio},
		{"cache_ratio", pr.CacheRatio},
		{"cache_creation_ratio", pr.CacheCreationRatio},
		{"cache_creation_5m_ratio", pr.CacheCreation5mRatio},
		{"cache_creation_1h_ratio", pr.CacheCreation1hRatio},
		{"image_ratio", pr.ImageRatio},
		{"audio_ratio", pr.AudioRatio},
		{"audio_completion_ratio", pr.AudioCompletionRatio},
	}
	for _, x := range prices {
		if err := validatePrice(x.name, x.v); err != nil {
			return 0, err
		}
	}

	imageCount := p.ImageCount
	if imageCount == ImageCountUnset {
		imageCount = 1
	}
	if imageCount == 0 {
		// 结算域确认 0 张：有效 0 计 0（billing-00 §9.1），不是「取整为 0」，
		// 因此也不触发最小计费。
		return 0, nil
	}
	if !hasBillableUsage(p) {
		return 0, nil
	}

	in := dec(pr.ModelRatio)
	out := dec(pr.CompletionRatio)
	cacheRead := dec(pr.CacheRatio)
	cc5 := dec(pr.CacheCreation5mRatio)
	cc1h := dec(pr.CacheCreation1hRatio)
	imgRatio := dec(pr.ImageRatio)
	audioRatio := dec(pr.AudioRatio)
	audioCompletion := dec(pr.AudioCompletionRatio)

	sum := decimal.Zero
	sum = sum.Add(decimal.NewFromInt(p.P).Mul(in))
	sum = sum.Add(decimal.NewFromInt(p.C).Mul(out))
	sum = sum.Add(decimal.NewFromInt(p.CR).Mul(cacheRead))
	sum = sum.Add(decimal.NewFromInt(p.CC5m).Mul(cc5))
	sum = sum.Add(decimal.NewFromInt(p.CC1h).Mul(cc1h))
	// 图片输入按「输入价 × 图片倍率」计；图片缓存读属于缓存读维度，
	// 图片输出 token 按输出价计（billing-00 §4.1 的变量语义）。
	sum = sum.Add(decimal.NewFromInt(p.Img).Mul(in).Mul(imgRatio))
	sum = sum.Add(decimal.NewFromInt(p.ImgCR).Mul(cacheRead))
	sum = sum.Add(decimal.NewFromInt(p.ImgO).Mul(out))
	sum = sum.Add(decimal.NewFromInt(p.AI).Mul(in).Mul(audioRatio))
	sum = sum.Add(decimal.NewFromInt(p.AO).Mul(in).Mul(audioRatio).Mul(audioCompletion))

	total := sum.Div(oneMillion).
		Mul(dec(group)).
		Mul(decimal.NewFromInt(int64(imageCount)))
	quota, clamp := QuotaFromDecimalChecked(total)
	if clamp != nil {
		return quota, &ClampError{Clamp: clamp}
	}
	if quota == 0 && usedDimensionPositive(p, pr) {
		// 最小计费只对**实际使用的维度**生效（R-36）：若本请求只走了免费
		// 维度（该维价格 0），结果必须保持 0；只有「用到的维度价格 >0、
		// 但取整为 0」才抬到 1。
		return 1, nil
	}
	return quota, nil
}

// usedDimensionPositive 判断本次请求实际使用的维度是否有正价。
//
// 对应裁决 R-36：最小计费按「已使用维度」判定，而不是「模型任意一项价 >0」。
// 否则「输入免费、输出收费」的模型在只消耗输入时会凭空收 1 积分。
func usedDimensionPositive(p contract.TokenParams, pr contract.PriceData) bool {
	if p.P > 0 && pr.ModelRatio > 0 {
		return true
	}
	if p.C > 0 && pr.CompletionRatio > 0 {
		return true
	}
	if p.CR > 0 && pr.CacheRatio > 0 {
		return true
	}
	if p.CC5m > 0 && pr.CacheCreation5mRatio > 0 {
		return true
	}
	if p.CC1h > 0 && pr.CacheCreation1hRatio > 0 {
		return true
	}
	if p.Img > 0 && pr.ModelRatio*pr.ImageRatio > 0 {
		return true
	}
	if p.ImgCR > 0 && pr.CacheRatio > 0 {
		return true
	}
	if p.ImgO > 0 && pr.CompletionRatio > 0 {
		return true
	}
	if p.AI > 0 && pr.ModelRatio*pr.AudioRatio > 0 {
		return true
	}
	if p.AO > 0 && pr.ModelRatio*pr.AudioRatio*pr.AudioCompletionRatio > 0 {
		return true
	}
	return false
}

// computePerCallQuota 实现按次模式：单次价 × 张数 × 分组倍率。
//
// 图片按次乘 n（billing-02 §3.3）。ImageCount 采用包内冻结约定
// （见 ImageCountUnset）：-1 = 未设置 → 请求域缺省 1；0 = 精确 0 张 →
// **计 0**（billing-00 §9.1「有效 0 计 0」），不得触发最小计费保底。
func computePerCallQuota(snap contract.PriceSnapshot, p contract.TokenParams, group float64) (int64, error) {
	if err := validatePrice("model_price", snap.Price.ModelPrice); err != nil {
		return 0, err
	}
	imageCount := p.ImageCount
	if imageCount == ImageCountUnset {
		imageCount = 1
	}
	if imageCount == 0 {
		// 结算域确认 0 张：按次金额为 0，且这是**有效 0**而不是「取整为 0」，
		// 所以不能套用 §7.5 的最小计费 1。
		return 0, nil
	}
	total := dec(snap.Price.ModelPrice).
		Mul(decimal.NewFromInt(int64(imageCount))).
		Mul(dec(group))
	quota, clamp := QuotaFromDecimalChecked(total)
	if clamp != nil {
		return quota, &ClampError{Clamp: clamp}
	}
	if quota == 0 && snap.Price.ModelPrice > 0 {
		return 1, nil
	}
	return quota, nil
}

// computeExpressionQuota 求值表达式并按单位换算为积分。
func computeExpressionQuota(snap contract.PriceSnapshot, p contract.TokenParams, usedVars map[string]bool, group float64) (int64, error) {
	if snap.ExprString == "" {
		return 0, ErrNoExpression
	}
	evaluator, err := expr.Compile(snap.ExprString)
	if err != nil {
		// 表达式失败必须向上返回 error：Task 3 按 §7.5 保留预扣，不得退款。
		return 0, fmt.Errorf("pricing: 编译表达式: %w", err)
	}
	if usedVars == nil {
		usedVars = evaluator.UsedVars()
	}
	out, err := evaluator.Eval(tokenVars(p))
	if err != nil {
		return 0, fmt.Errorf("pricing: 求值表达式: %w", err)
	}
	if out < 0 {
		// 负成本会变成负配额（抵扣），属于配置错误；§7.5 的「≤0 置 1」
		// 只适用于非负截断到 0 的场景，这里不吞负值。
		return 0, fmt.Errorf("%w: %v", ErrNegativeExpression, out)
	}

	qpu := decimal.NewFromInt(CurrentQuotaPerUnit())
	total := dec(out)
	if usesTaskUsage(usedVars) {
		// 任务用量表达式返回「本次请求的 USD 成本」，不除 1e6（billing-00 §7.1）。
		total = total.Mul(qpu)
	} else {
		total = total.Div(oneMillion).Mul(qpu)
	}
	total = total.Mul(dec(group))

	quota, clamp := QuotaFromDecimalChecked(total)
	if clamp != nil {
		return quota, &ClampError{Clamp: clamp}
	}
	if quota == 0 && out > 0 {
		return 1, nil // 正价格被取整为 0 → 最小计费；out==0 是显式免费
	}
	return quota, nil
}

// tokenVars 把归一化后的 TokenParams 映射为表达式变量。
//
// 所有变量都提供（未使用的多给不会影响求值），这样同一份参数可对任意
// 表达式复用；usedVars 只用于任务语义判定，不用于裁剪入参。
func tokenVars(p contract.TokenParams) map[string]float64 {
	imageCount := p.ImageCount
	if imageCount == ImageCountUnset {
		// 未设置 → 表达式请求域缺省 1（billing-01 §2.1）。
		imageCount = 1
	}
	// 精确 0 张保持 0：表达式可据此计 0（billing-00 §9.1）。
	return map[string]float64{
		"p":           float64(p.P),
		"c":           float64(p.C),
		"len":         float64(p.Len),
		"cr":          float64(p.CR),
		"cc":          float64(p.CC5m),
		"cc1h":        float64(p.CC1h),
		"img":         float64(p.Img),
		"img_cr":      float64(p.ImgCR),
		"img_o":       float64(p.ImgO),
		"ai":          float64(p.AI),
		"ao":          float64(p.AO),
		"image_count": float64(imageCount),
	}
}

// usesTaskUsage 判定表达式是否属于任务用量语义。
//
// billing-01 §4.1：usedVars 含函数名；任务表达式的唯一用量入口是 u()。
// 当前 Task 1 的表达式子集尚未实现 u()，该判定为未来兼容而保留：
// 一旦表达式编译器支持 u()，这里无需改动即可切到「不除 1e6」的分支。
func usesTaskUsage(usedVars map[string]bool) bool {
	return usedVars["u"]
}
