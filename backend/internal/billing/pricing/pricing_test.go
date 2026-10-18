package pricing

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"reflect"
	"strconv"
	"testing"

	"newtg/gateway/internal/contract"
)

func TestParsePricingEmptyUsesDefaults(t *testing.T) {
	p, err := ParsePricing("   ")
	if err != nil {
		t.Fatalf("空 spec 不应报错: %v", err)
	}
	if want := DefaultPromptPricePer1M(); p.ModelRatio != want {
		t.Fatalf("默认输入价=%v，期望 %v", p.ModelRatio, want)
	}
	if p.CompletionRatio != 0 {
		t.Fatalf("输出价未设置应为 0（Resolve 再兜底），got %v", p.CompletionRatio)
	}
	if p.CacheRatioSet || p.CacheCreationRatioSet || p.ImageRatioSet || p.AudioRatioSet {
		t.Fatalf("默认不应带显式零标记: %+v", p)
	}
	if p.ImageRatio != 1 || p.AudioRatio != 1 || p.AudioCompletionRatio != 1 {
		t.Fatalf("媒体倍率默认应为 1: %+v", p)
	}
}

func TestParsePricingLegacyStyle(t *testing.T) {
	// 旧倍率风格：billing-02 §3.2 的 1 = $2/1M；积分 = ratio × 2 × QPU。
	old := CurrentQuotaPerUnit()
	t.Cleanup(func() { _ = SetQuotaPerUnit(old) })
	if err := SetQuotaPerUnit(100); err != nil {
		t.Fatal(err)
	}

	p, err := ParsePricing(`{"model_ratio":1,"completion_ratio":3,"cache_ratio":0.5,"create_cache_ratio":1.25}`)
	if err != nil {
		t.Fatalf("解析旧倍率 spec: %v", err)
	}
	if p.ModelRatio != 200 {
		t.Fatalf("输入价=%v，期望 200", p.ModelRatio)
	}
	if p.CompletionRatio != 600 {
		t.Fatalf("输出价=%v，期望 600", p.CompletionRatio)
	}
	if p.CacheRatio != 100 || !p.CacheRatioSet {
		t.Fatalf("缓存读价=%v set=%v，期望 100/true", p.CacheRatio, p.CacheRatioSet)
	}
	if p.CacheCreationRatio != 250 || !p.CacheCreationRatioSet {
		t.Fatalf("缓存写价=%v set=%v，期望 250/true", p.CacheCreationRatio, p.CacheCreationRatioSet)
	}
}

func TestParsePricingAbsoluteStyle(t *testing.T) {
	p, err := ParsePricing(`{"prompt_price":1000,"completion_price":2000,"cache_read_price":100,"cache_write_price":1250}`)
	if err != nil {
		t.Fatalf("解析绝对值 spec: %v", err)
	}
	if p.ModelRatio != 1000 || p.CompletionRatio != 2000 ||
		p.CacheRatio != 100 || !p.CacheRatioSet ||
		p.CacheCreationRatio != 1250 || !p.CacheCreationRatioSet {
		t.Fatalf("绝对值映射错误: %+v", *p)
	}
	if p.UsePrice {
		t.Fatal("per_token 不应设置 UsePrice")
	}
}

func TestParsePricingExplicitZeroPreserved(t *testing.T) {
	p, err := ParsePricing(`{"prompt_price":0,"completion_price":500}`)
	if err != nil {
		t.Fatalf("解析: %v", err)
	}
	if p.ModelRatio != 0 {
		t.Fatalf("输入价显式 0 被吞：%v", p.ModelRatio)
	}
	if p.CompletionRatio != 500 {
		t.Fatalf("输出价=%v，期望 500", p.CompletionRatio)
	}

	// 显式 0 用 -1 哨兵承载（0 是「未设置」），Resolve 后必须还原为 0。
	p, err = ParsePricing(`{"prompt_price":1000,"completion_price":0}`)
	if err != nil {
		t.Fatalf("解析: %v", err)
	}
	if p.CompletionRatio != explicitZeroRatio {
		t.Fatalf("显式零应记为哨兵 %v，got %v", explicitZeroRatio, p.CompletionRatio)
	}
	snap, err := Resolve(p, "claude-sonnet-4", contract.BillingModeRatio)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if snap.Price.CompletionRatio != 0 {
		t.Fatalf("显式零输出价必须保留为 0，got %v", snap.Price.CompletionRatio)
	}
}

func TestParsePricingRejectsMixedStyles(t *testing.T) {
	_, err := ParsePricing(`{"model_ratio":1,"prompt_price":100}`)
	if !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("混合风格应拒绝，got %v", err)
	}
}

func TestParsePricingRejectsInvalid(t *testing.T) {
	cases := []struct {
		name string
		spec string
		want error
	}{
		{"负价", `{"prompt_price":-1}`, ErrInvalidPrice},
		{"未知键", `{"prompt_pirce":1}`, ErrInvalidSpec},
		{"非法 JSON", `{"prompt_price":`, ErrInvalidSpec},
		{"尾随内容", `{"prompt_price":1}{"prompt_price":2}`, ErrInvalidSpec},
		{"未知模式", `{"mode":"magic"}`, ErrInvalidMode},
		{"per_call 缺价", `{"mode":"per_call"}`, ErrInvalidSpec},
		{"非表达式带 expr", `{"mode":"ratio","expr":"p*2"}`, ErrUnexpectedExpression},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := ParsePricing(c.spec); !errors.Is(err, c.want) {
				t.Fatalf("err=%v，期望 errors.Is(%v)", err, c.want)
			}
		})
	}
}

func TestParsePricingTieredExpr(t *testing.T) {
	parsed, err := ParsePricingSpec(`{"mode":"tiered_expr","expr":"p*2 + c"}`)
	if err != nil {
		t.Fatalf("解析表达式 spec: %v", err)
	}
	if parsed.Mode != contract.BillingModeTieredExpr || parsed.Expr != "p*2 + c" {
		t.Fatalf("表达式模式解析错误: %+v", parsed)
	}

	// expr 存在但未写 mode：按表达式推断。
	parsed, err = ParsePricingSpec(`{"expr":"p"}`)
	if err != nil || parsed.Mode != contract.BillingModeTieredExpr {
		t.Fatalf("应推断为表达式模式: %+v err=%v", parsed, err)
	}

	if _, err := ParsePricing(`{"mode":"tiered_expr"}`); !errors.Is(err, ErrMissingExpression) {
		t.Fatalf("缺表达式应报 ErrMissingExpression，got %v", err)
	}
	if _, err := ParsePricing(`{"mode":"tiered_expr","expr":"p+"}`); !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("非法表达式应在保存期拒绝，got %v", err)
	}
}

func TestParsePricingPerCall(t *testing.T) {
	parsed, err := ParsePricingSpec(`{"mode":"per_call","model_price":150}`)
	if err != nil {
		t.Fatalf("解析按次 spec: %v", err)
	}
	if parsed.Mode != contract.BillingModePerCall || !parsed.Price.UsePrice || parsed.Price.ModelPrice != 150 {
		t.Fatalf("按次解析错误: %+v", parsed)
	}

	// 只给 model_price：按 billing-02「ModelPrice 命中派生按次」推断。
	p, err := ParsePricing(`{"model_price":0}`)
	if err != nil || !p.UsePrice || p.ModelPrice != 0 || !p.FreeModel {
		t.Fatalf("按次显式 0 应为免费: %+v err=%v", p, err)
	}
}

func TestResolveCompletionRatioTwoTier(t *testing.T) {
	cases := []struct {
		name   string
		model  string
		cfg    float64
		cfgSet bool
		want   float64
	}{
		{"locked 忽略配置", "claude-sonnet-4-5", 2, true, 5},
		{"含 / 跳过锁定用配置", "vendor/claude-sonnet-4", 3, true, 3},
		{"含 / 未配置回硬编码（前缀不匹配→1）", "vendor/claude-sonnet-4", 0, false, 1},
		{"locked gpt-5", "gpt-5", 0, false, 8},
		{"locked gpt-5 配 2 仍 8", "gpt-5", 2, true, 8},
		{"unlocked gpt-5.5 配 3 用配置", "gpt-5.5-turbo", 3, true, 3},
		{"unlocked 兜底 1", "unknown-model", 0, false, 1},
		{"gemini 图片族 60", "gemini-3-pro-image-preview", 0, false, 60},
		{"-all 族 2", "some-model-all", 0, false, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ResolveCompletionRatio(c.model, c.cfg, c.cfgSet); got != c.want {
				t.Fatalf("got %v，期望 %v", got, c.want)
			}
		})
	}
}

func TestResolveFillsRatioDefaults(t *testing.T) {
	p, err := ParsePricing("")
	if err != nil {
		t.Fatal(err)
	}
	snap, err := Resolve(p, "plain-model", contract.BillingModeRatio)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	pr := snap.Price
	prompt := DefaultPromptPricePer1M()
	if pr.CompletionRatio != prompt {
		t.Fatalf("未配置输出价应按族兜底 1×输入价：got %v want %v", pr.CompletionRatio, prompt)
	}
	if pr.CacheRatio != prompt {
		t.Fatalf("缓存读应默认 1×输入价：got %v", pr.CacheRatio)
	}
	if pr.CacheCreationRatio != prompt*DefaultCacheCreationRatio {
		t.Fatalf("缓存创建基准应默认 1.25×输入价：got %v", pr.CacheCreationRatio)
	}
	if pr.CacheCreation5mRatio != pr.CacheCreationRatio {
		t.Fatalf("5m 应等于基准：got %v", pr.CacheCreation5mRatio)
	}
	if pr.CacheCreation1hRatio != pr.CacheCreationRatio {
		t.Fatalf("非 claude 的 1h 不应乘 1.6：got %v want %v", pr.CacheCreation1hRatio, pr.CacheCreationRatio)
	}
	if snap.GroupRatio != 1 || snap.Mode != contract.BillingModeRatio {
		t.Fatalf("快照默认值错误: mode=%v group=%v", snap.Mode, snap.GroupRatio)
	}
	if snap.PreConsumeMultiplier != 1 {
		t.Fatalf("预扣倍数默认应为 1，got %v", snap.PreConsumeMultiplier)
	}
}

func TestResolveClaudeCacheCreation1h(t *testing.T) {
	p := contract.PriceData{ModelRatio: 1000}
	snap, err := Resolve(&p, "claude-sonnet-4-5", contract.BillingModeRatio)
	if err != nil {
		t.Fatal(err)
	}
	// 基准 1250 → 1h = 2000（6/3.75）。
	if got, want := snap.Price.CacheCreation1hRatio, 1250*1.6; got != want {
		t.Fatalf("claude 1h=%v，期望 %v", got, want)
	}
	if snap.CacheWriteMode != contract.CacheWriteClaudeTTL {
		t.Fatalf("claude 应标 CacheWriteClaudeTTL，got %v", snap.CacheWriteMode)
	}
}

func TestResolvePreservesExplicitZero(t *testing.T) {
	p := contract.PriceData{
		ModelRatio:            1000,
		CompletionRatio:       explicitZeroRatio, // -1 = 显式零
		CacheRatio:            0,
		CacheRatioSet:         true,
		CacheCreationRatio:    0,
		CacheCreationRatioSet: true,
		ImageRatio:            0,
		ImageRatioSet:         true,
		AudioRatio:            0,
		AudioRatioSet:         true,
		AudioCompletionRatio:  0,
	}
	snap, err := Resolve(&p, "claude-x", contract.BillingModeRatio)
	if err != nil {
		t.Fatal(err)
	}
	pr := snap.Price
	if pr.CompletionRatio != 0 || pr.CacheRatio != 0 || pr.CacheCreationRatio != 0 ||
		pr.CacheCreation5mRatio != 0 || pr.CacheCreation1hRatio != 0 ||
		pr.ImageRatio != 0 || pr.AudioRatio != 0 || pr.AudioCompletionRatio != 0 {
		t.Fatalf("显式零被默认值覆盖: %+v", pr)
	}
}

func TestResolveGroupRatio(t *testing.T) {
	p := contract.PriceData{ModelRatio: 1, GroupRatioInfo: contract.GroupRatioInfo{Group: "vip", GroupRatio: 1.2}}
	snap, err := Resolve(&p, "m", contract.BillingModeRatio)
	if err != nil {
		t.Fatal(err)
	}
	if snap.GroupRatio != 1.2 {
		t.Fatalf("分组倍率=%v，期望 1.2", snap.GroupRatio)
	}

	p.GroupRatioInfo = contract.GroupRatioInfo{
		Group: "vip", GroupRatio: 1.2, GroupSpecialRatio: 0.9, HasSpecialRatio: true,
	}
	snap, _ = Resolve(&p, "m", contract.BillingModeRatio)
	if snap.GroupRatio != 0.9 {
		t.Fatalf("组间覆盖应优先（0.9），got %v", snap.GroupRatio)
	}

	p.GroupRatioInfo = contract.GroupRatioInfo{}
	snap, _ = Resolve(&p, "m", contract.BillingModeRatio)
	if snap.GroupRatio != 1 {
		t.Fatalf("缺省分组倍率应为 1，got %v", snap.GroupRatio)
	}
}

func TestResolveModeAndExpression(t *testing.T) {
	// UsePrice 命中 → 按次。
	p := contract.PriceData{UsePrice: true, ModelPrice: 5}
	snap, err := Resolve(&p, "m", contract.BillingModeRatio)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Mode != contract.BillingModePerCall || !snap.Price.UsePrice {
		t.Fatalf("UsePrice 应派生按次: %+v", snap)
	}

	// 表达式模式冻结哈希。
	exprStr := "min(p*2, c)"
	snap, err = ResolveExpression(&contract.PriceData{}, "m", contract.BillingModeTieredExpr, exprStr)
	if err != nil {
		t.Fatalf("表达式模式: %v", err)
	}
	sum := sha256.Sum256([]byte(exprStr))
	if snap.ExprString != exprStr || snap.ExprHash != hex.EncodeToString(sum[:]) {
		t.Fatalf("表达式冻结错误: %q / %q", snap.ExprString, snap.ExprHash)
	}

	// 非表达式模式携带表达式 → 拒绝。
	if _, err := ResolveExpression(&contract.PriceData{}, "m", contract.BillingModeRatio, "p"); !errors.Is(err, ErrUnexpectedExpression) {
		t.Fatalf("应拒绝意外表达式，got %v", err)
	}
	// 缺表达式。
	if _, err := ResolveExpression(&contract.PriceData{}, "m", contract.BillingModeTieredExpr, ""); !errors.Is(err, ErrMissingExpression) {
		t.Fatalf("应要求表达式，got %v", err)
	}
	// nil PriceData。
	if _, err := Resolve(nil, "m", contract.BillingModeRatio); !errors.Is(err, ErrNilPrice) {
		t.Fatalf("nil PriceData 应拒绝，got %v", err)
	}
}

func TestResolveDoesNotMutateInput(t *testing.T) {
	p := contract.PriceData{ModelRatio: 1000}
	before := p
	if _, err := Resolve(&p, "claude-x", contract.BillingModeRatio); err != nil {
		t.Fatal(err)
	}
	// PriceData 含私有 map 字段，不能用 == 比较。
	if !reflect.DeepEqual(p, before) {
		t.Fatalf("Resolve 改写了调用方对象: before=%+v after=%+v", before, p)
	}
}

func TestResolveBillingModelName(t *testing.T) {
	SetCanonicalBillingModelNames(map[string]string{"alias-model": "canonical-model"})
	t.Cleanup(func() { SetCanonicalBillingModelNames(nil) })

	cases := map[string]string{
		" model@channel ": "model",
		"alias-model":     "canonical-model",
		"plain":           "plain",
		"@only":           "",
	}
	for in, want := range cases {
		if got := ResolveBillingModelName(in); got != want {
			t.Errorf("ResolveBillingModelName(%q)=%q，期望 %q", in, got, want)
		}
	}

	// BaseModelName：-latest 与 Claude legacy -thinking 别名剥除；
	// 日期版本与 "/" 命名空间保留。
	baseCases := map[string]string{
		"qwen3-max-latest":           "qwen3-max",
		"claude-3-5-sonnet-thinking": "claude-3-5-sonnet",
		"gpt-4o-2024-05-13":          "gpt-4o-2024-05-13",
		"openai/gpt-4o-latest":       "openai/gpt-4o",
		"gpt-5.1-codex-max":          "gpt-5.1-codex-max",
	}
	for in, want := range baseCases {
		if got := BaseModelName(in); got != want {
			t.Errorf("BaseModelName(%q)=%q，期望 %q", in, got, want)
		}
	}

	// 候选顺序：原名 → 基名的规范名 → 基名。
	cands := ResolveBillingModelCandidates("alias-model-latest")
	want := []string{"alias-model-latest", "canonical-model", "alias-model"}
	if len(cands) != len(want) {
		t.Fatalf("候选数=%d，期望 %d: %v", len(cands), len(want), cands)
	}
	for i := range want {
		if cands[i] != want[i] {
			t.Fatalf("候选[%d]=%q，期望 %q（全部 %v）", i, cands[i], want[i], cands)
		}
	}

	// 精确名本身有规范映射时，规范名优先。
	cands = ResolveBillingModelCandidates("alias-model")
	if len(cands) == 0 || cands[0] != "canonical-model" {
		t.Fatalf("规范映射应优先，got %v", cands)
	}
}

func TestResolveModePriority(t *testing.T) {
	SetBuiltinBillingExpressions(map[string]string{"builtin-x": "p*2"})
	t.Cleanup(func() { SetBuiltinBillingExpressions(nil) })

	// 显式表达式模式最高优先（即使配置了旧价）。
	if got := ResolveMode(&contract.PriceData{ModelRatio: 5}, "m", contract.BillingModeTieredExpr, true); got != contract.BillingModeTieredExpr {
		t.Fatalf("显式模式应胜出，got %v", got)
	}
	// 显式 ratio（R9 的 mode 列）不被内置表达式抢走。
	if got := ResolveMode(&contract.PriceData{}, "builtin-x", contract.BillingModeRatio, true); got != contract.BillingModeRatio {
		t.Fatalf("显式 ratio 应胜出内置表达式，got %v", got)
	}
	// 已配置旧价优先于内置表达式（未显式声明模式时）。
	if got := ResolveMode(&contract.PriceData{ModelRatio: 5}, "builtin-x", contract.BillingModeRatio, false); got != contract.BillingModeRatio {
		t.Fatalf("已配置旧价应优先于内置表达式，got %v", got)
	}
	// 无旧价 → 内置表达式。
	if got := ResolveMode(&contract.PriceData{}, "builtin-x", contract.BillingModeRatio, false); got != contract.BillingModeTieredExpr {
		t.Fatalf("应命中内置表达式，got %v", got)
	}
	// 按次。
	if got := ResolveMode(&contract.PriceData{UsePrice: true, ModelPrice: 1}, "m", contract.BillingModeRatio, false); got != contract.BillingModePerCall {
		t.Fatalf("UsePrice 应按次，got %v", got)
	}
	// nil price 不得 panic；无配置兜底 ratio。
	if got := ResolveMode(nil, "m", contract.BillingModeRatio, false); got != contract.BillingModeRatio {
		t.Fatalf("nil price 应兜底 ratio，got %v", got)
	}
	if got := ResolveMode(nil, "m", contract.BillingModePerCall, true); got != contract.BillingModePerCall {
		t.Fatalf("nil price + 显式按次，got %v", got)
	}
	// 无任何配置 → 传统倍率兜底。
	if got := ResolveMode(&contract.PriceData{}, "m", contract.BillingModeRatio, false); got != contract.BillingModeRatio {
		t.Fatalf("应兜底 ratio，got %v", got)
	}
}

func TestBuiltinExpressionsRejectUnknownOnly(t *testing.T) {
	SetBuiltinBillingExpressions(map[string]string{"known": "p"})
	t.Cleanup(func() { SetBuiltinBillingExpressions(nil) })
	if _, ok := BuiltinBillingExpression("known"); !ok {
		t.Fatal("known 应命中")
	}
	if _, ok := BuiltinBillingExpression("known@channel"); !ok {
		t.Fatal("@ 修饰符应在查表前剥离")
	}
	if _, ok := BuiltinBillingExpression("unknown"); ok {
		t.Fatal("unknown 不应命中")
	}
}

func TestDefaultPriceDerivation(t *testing.T) {
	// 默认应为 37.5 × 2 × QPU（QPU 默认 100 → 7500）。
	old := CurrentQuotaPerUnit()
	t.Cleanup(func() { _ = SetQuotaPerUnit(old) })
	if err := SetQuotaPerUnit(100); err != nil {
		t.Fatal(err)
	}
	if got := DefaultPromptPricePer1M(); got != 7500 {
		t.Fatalf("默认输入价=%v，期望 7500", got)
	}
	if got := DefaultPerCallPrice(); got != 1875 {
		t.Fatalf("默认按次价=%v，期望 1875", got)
	}
}

func TestParsePricingFiniteCheck(t *testing.T) {
	// JSON 不允许 NaN/Inf 字面量，1e999 会因超出 float64 被拒绝。
	if _, err := ParsePricing(`{"prompt_price":1e999}`); err == nil {
		t.Fatal("超出 float64 范围的数值应拒绝")
	}

	// 换算后的溢出（model_ratio 1e308 × 2 × QPU）必须拒绝，不得把 +Inf
	// 带进 PriceData（review I3）。
	if _, err := ParsePricing(`{"model_ratio":1e308}`); !errors.Is(err, ErrInvalidPrice) {
		t.Fatalf("换算后 +Inf 应拒绝，got %v", err)
	}
}

func TestParsePricingRejectsUnknownExpressionIdentifiers(t *testing.T) {
	// 拼写错误（cch1）必须在保存期拒绝，而不是等到运行期「变量未绑定」。
	if _, err := ParsePricing(`{"mode":"tiered_expr","expr":"cch1*2"}`); !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("未知标识符应拒绝，got %v", err)
	}
	if _, err := ParsePricing(`{"mode":"tiered_expr","expr":"p + bogus"}`); !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("未知变量应拒绝，got %v", err)
	}
	// ResolveExpression 同样校验（直接构造快照的入口）。
	if _, err := ResolveExpression(&contract.PriceData{}, "m", contract.BillingModeTieredExpr, "cch1*2"); !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("ResolveExpression 应拒绝未知标识符，got %v", err)
	}

	// 全部合法变量 + 函数名应接受。
	valid := "min(max(p,c),len) + cr + cc + cc1h + img + img_cr + img_o + ai + ao + image_count + round(1.5) + ceil(1.2) + floor(1.8)"
	if _, err := ParsePricing(`{"mode":"tiered_expr","expr":` + strconv.Quote(valid) + `}`); err != nil {
		t.Fatalf("合法表达式被拒: %v", err)
	}
}
