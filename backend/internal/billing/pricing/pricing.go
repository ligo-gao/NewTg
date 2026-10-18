// Package pricing 实现 M3 计费引擎的定价解析、快照产出、配额计算与数据库定价源。
//
// 设计权威（按优先级）：
//   - docs/billing-00-计费引擎接口契约.md §3（模式/显式零/倍率/PriceData）、§7（配额数学）；
//   - docs/billing-02-定价解析与配置-设计.md §2（解析优先级/冲突胜出）、§3（字段全集）、§4（倍率体系）；
//   - docs/platform-01-数据库字段汇总.md §7/§8（R9：model_pricing/group_pricing 的整数价承载）；
//   - docs/platform-99-一致性检查.md S-5：R9 的「整数价 + 判别列」**有意取代** billing-02 的
//     options/倍率存储（本包按 R9 落库形态实现，倍率语义仅作为转换入口保留）。
//
// # PriceData 的单位约定（M3 的运行时口径）
//
// contract.PriceData 的字段名沿用旧倍率体系（ModelRatio/CompletionRatio/...），
// 但 R9 之后数据库里的价格是**整数积分绝对值**。为避免「倍率为 0 的输入价 +
// 非 0 输出价」这类独立定价被单一基准倍率吞掉，本包对 Ratio 模式采用如下
// 明确口径（并在 Resolve 中保持）：
//
//   - ModelRatio：输入价，单位 **积分 / 1M tokens**；
//   - CompletionRatio：输出价，单位 **积分 / 1M tokens**；
//   - CacheRatio：缓存读价，单位 **积分 / 1M tokens**；
//   - CacheCreationRatio / CacheCreation5mRatio / CacheCreation1hRatio：缓存写价，
//     单位 **积分 / 1M tokens**；
//   - ImageRatio / AudioRatio / AudioCompletionRatio：相对输入价的**乘数**（默认 1）；
//   - ModelPrice（UsePrice=true）：按次价，单位 **积分 / 次**。
//
// 为什么不用「基准倍率 × 相对倍率」：DB 允许输入价显式 0 而输出价 >0
// （modules-06 TC-F-06-13-03），相对倍率表示在该场景无解（除以 0）；绝对值
// 表示与 model_pricing 的四个价列一一对应，可精确复现管理员配置。
//
// 表达式模式的输出仍是 **USD / 1M tokens**（billing-00 §7.1 冻结），由
// ComputeQuota 乘 billing.quota_per_unit 换成积分；这是唯一需要 QPU 的路径。
//
// # 显式零
//
// 价格类字段（ModelRatio/CompletionRatio/缓存读写价）的 0 表示「未设置，由
// Resolve 按模型族/基准兜底」；显式免费用包内哨兵 -1 承载（契约缺少
// CompletionRatioSet / AudioCompletionRatioSet / CacheCreation1hSet 三个标记）。
// ParsePricing 与 Source 负责把「显式 0」写成 -1、「未设置」写成 0；Resolve
// 负责把 -1 还原为 0 并填充其余缺省值。-1 不会出现在 Resolve 的产出中。
// 媒体乘数（ImageRatio/AudioRatio）用契约已有的 Set 标记区分；按次价用
// UsePrice 区分「未启用」与「显式 0 元/次」。
package pricing

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"sync"

	"newtg/gateway/internal/billing/expr"
	"newtg/gateway/internal/contract"
)

// ---------------------------------------------------------------------------
// 默认值与模型族兜底（billing-02 §3.2、§4）
// ---------------------------------------------------------------------------

const (
	// DefaultModelRatio 是 billing-02 §4.1 的模型倍率兜底 37.5。
	// 它只在「输入价未设置」时参与默认价推导，不是运行时查表结果。
	DefaultModelRatio = 37.5

	// DefaultCacheRatio 是缓存读倍率缺省 1.0（billing-00 §3.3）。
	DefaultCacheRatio = 1.0

	// DefaultCacheCreationRatio 是缓存创建基准缺省 1.25（billing-00 §3.3）。
	DefaultCacheCreationRatio = 1.25

	// DefaultImageRatio / DefaultAudioRatio / DefaultAudioCompletionRatio
	// 是三类媒体倍率缺省 1（billing-00 §3.3）。
	DefaultImageRatio           = 1.0
	DefaultAudioRatio           = 1.0
	DefaultAudioCompletionRatio = 1.0

	// claudeCacheCreation1hFactor 是 Claude 1h 缓存创建相对 5m 的系数
	// 6/3.75 = 1.6（billing-00 §3.3）：**只对名字含 claude 的模型继承**。
	// 对所有模型一律乘 1.6 会系统性抬高非 Claude 模型的 1h 价。
	claudeCacheCreation1hFactor = 6.0 / 3.75

	// explicitZeroRatio 是包内「该字段被显式配置为 0」的哨兵。
	//
	// 契约的 PriceData 缺少 CompletionRatioSet / CacheCreation1hSet 等标记，
	// 无法用 0 同时表达「未设置」与「显式零」。本包约定：0 = 未设置（由
	// Resolve 按模型族/基准值兜底），-1 = 显式零。这样直接构造
	// PriceData{ModelRatio: x} 的调用方会得到「输出价按族兜底」而不是被
	// 静默当成免费输出；显式免费由 ParsePricing/Source 写入 -1 表达。
	// 该值只在 Resolve 之前出现，Resolve 的产出中不再含 -1。
	explicitZeroRatio = -1.0
)

// DefaultPromptPricePer1M 返回未知模型 / 未设置输入价的默认输入价（积分/1M）。
//
// 推导：billing-02 §3.2 定义 ModelRatio 1 = $2/1M；旧美元常量和
// QuotaPerUnit=500_000 已被 R-01 废止，但「1 倍率 = 2 个货币单位/1M」
// 的几何关系保留。积分/1M = 37.5 × 2 × quota_per_unit；默认 100 时 = 7500。
func DefaultPromptPricePer1M() float64 {
	return DefaultModelRatio * 2 * float64(CurrentQuotaPerUnit())
}

// DefaultPerCallPrice 返回按次模式的兜底单价（积分/次）。
//
// 推导：billing-02 §3.3 的第三兜底为 ModelRatio/2（货币单位/次）；
// 换积分乘 quota_per_unit。仅用于 model_pricing 的 per_request 行
// prompt_price 为 NULL 的情形（R9：NULL=未设置→兜底）。
func DefaultPerCallPrice() float64 {
	return DefaultModelRatio / 2 * float64(CurrentQuotaPerUnit())
}

// clampModelName 统一模型名的大小写比较口径。
func clampModelName(model string) string { return strings.ToLower(strings.TrimSpace(model)) }

// modelHasClaude 判断模型名是否含 claude（不分大小写）。
func modelHasClaude(model string) bool { return strings.Contains(clampModelName(model), "claude") }

// hardcodedCompletionRatio 返回补全倍率的硬编码族与「强制锁定」标记
// （billing-02 §4.2 的族摘要；完整族见基线-11 §4.1，本实现覆盖设计文档列出的代表项）。
//
// locked=true 表示管理员配置不得覆盖（billing-02 §4.2 步骤 2）；
// 名字含 '/' 的模型在 ResolveCompletionRatio 中先走配置，跳过锁定。
func hardcodedCompletionRatio(model string) (ratio float64, locked bool) {
	m := clampModelName(model)
	switch {
	case m == "gpt-4o-2024-05-13":
		return 3, true
	case m == "gpt-5": // 「gpt-5（无点）」：精确匹配，不含 gpt-5.x
		return 8, true
	case strings.HasPrefix(m, "gpt-5.4"):
		return 6, true
	case strings.HasPrefix(m, "gpt-5.5"):
		return 6, false
	case strings.HasPrefix(m, "o1"), strings.HasPrefix(m, "o3"):
		return 4, true
	case strings.HasPrefix(m, "claude-3"),
		strings.HasPrefix(m, "claude-sonnet-4"),
		strings.HasPrefix(m, "claude-opus-4"),
		strings.HasPrefix(m, "claude-haiku-4"):
		return 5, true
	case strings.HasPrefix(m, "mistral-"):
		return 3, true
	case strings.HasPrefix(m, "gemini-3-pro-image"):
		return 60, false
	case strings.HasSuffix(m, "-all"), strings.Contains(m, "-gizmo-"):
		return 2, false
	default:
		return 1, false
	}
}

// ResolveCompletionRatio 实现 billing-02 §4.2 的两档语义（早返回）：
//
//  1. 模型名含 '/' 且配置了 → 配置值（跳过硬编码锁定）；
//  2. 硬编码族为 locked → 强制硬编码，忽略配置；
//  3. 配置了 → 配置值；
//  4. 未配置 → 硬编码兜底。
//
// 该函数供「旧倍率转换 / 配置中心迁移」等仍以相对倍率表达定价的入口使用；
// DB 绝对值口径由 CompletionPricePer1M 处理（显式价优先，仅 NULL 时兜底）。
func ResolveCompletionRatio(model string, configured float64, configuredSet bool) float64 {
	if strings.Contains(model, "/") && configuredSet {
		return configured
	}
	hard, locked := hardcodedCompletionRatio(model)
	if locked {
		return hard
	}
	if configuredSet {
		return configured
	}
	return hard
}

// CompletionPricePer1M 把「输出价」解析为绝对值（积分/1M tokens）。
//
// configured 为 nil 表示 DB 列 NULL（未设置）→ 用模型族硬编码倍率 × 输入价；
// 非 nil（含显式 0）一律保留：管理员显式写入的绝对价不得被硬编码族静默覆盖
// （billing-00 §3.1「已配置旧价优先」的精神；这也与 R9 的整数绝对价承载一致）。
func CompletionPricePer1M(model string, promptPricePer1M float64, configured *float64) float64 {
	if configured != nil {
		return *configured
	}
	ratio, _ := hardcodedCompletionRatio(model)
	return promptPricePer1M * ratio
}

// ---------------------------------------------------------------------------
// 模型名解析（billing-02 §2.3）
// ---------------------------------------------------------------------------

var (
	canonicalMu    sync.RWMutex
	canonicalNames = map[string]string{}
)

// SetCanonicalBillingModelNames 安装「规范计费模型名」映射（整表替换，读取副本）。
// 配置来源（管理面 / 配置文件）由装配方提供；本包不读配置。
func SetCanonicalBillingModelNames(m map[string]string) {
	copyMap := make(map[string]string, len(m))
	for k, v := range m {
		copyMap[k] = v
	}
	canonicalMu.Lock()
	canonicalNames = copyMap
	canonicalMu.Unlock()
}

// stripModelModifiers 去首尾空白并剥离 `@` 修饰符（如 model@channel → model）。
func stripModelModifiers(name string) string {
	name = strings.TrimSpace(name)
	if i := strings.IndexByte(name, '@'); i >= 0 {
		name = strings.TrimSpace(name[:i])
	}
	return name
}

// BaseModelName 实现 billing-02 §2.3 第 4 步的「基名」归一（保守版）。
//
// 规则边界（净室，按可观察语义重写）：
//   - `-latest` 别名后缀剥除（最常见的「版本别名」）；
//   - Claude 旧版 `-thinking` 推理别名剥除；GPT/o 系列的 effort 后缀
//     （-high/-low/...）**不**剥除：它们可能是真实模型 ID 的一部分，
//     误裁会让合法模型落到默认价；
//   - `vendor/model` 的命名空间保留（provider 前缀视为不透明；
//     billing-02 §4.2 对含 `/` 的名字另有补全倍率语义）；
//   - 日期版本（`-2024-05-13`）保留：locked 补全倍率族依赖全名匹配。
//
// 结果只作为**候选**参与 DB 查找（ResolveBillingModelCandidates），
// 原名始终先试，不会因为基名归一覆盖精确登记的模型。
func BaseModelName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	lower := strings.ToLower(name)
	if strings.HasSuffix(lower, "-latest") {
		name = name[:len(name)-len("-latest")]
	}
	lower = strings.ToLower(name)
	if strings.HasPrefix(lower, "claude-") && strings.HasSuffix(lower, "-thinking") {
		name = name[:len(name)-len("-thinking")]
	}
	return name
}

func canonicalBillingName(name string) string {
	canonicalMu.RLock()
	mapped := canonicalNames[name]
	canonicalMu.RUnlock()
	return mapped
}

// ResolveBillingModelCandidates 按 billing-02 §2.3 的顺序返回计费模型名候选：
//
//	规范名（若显式映射）→ 去修饰原名 → 基名 → 基名的规范名
//
// 【为什么返回候选而不是单名】候选让 Source 按序查 model_pricing：精确名
// 优先，别名/基名只在前者未登记时兜底——既支持 `-latest`/`-thinking` 别名
// 落到同一价目，又不会把精确登记的模型改写成另一个键。
func ResolveBillingModelCandidates(name string) []string {
	raw := stripModelModifiers(name)
	var out []string
	add := func(candidate string) {
		if candidate == "" {
			return
		}
		for _, existing := range out {
			if existing == candidate {
				return
			}
		}
		out = append(out, candidate)
	}

	if mapped := canonicalBillingName(raw); mapped != "" {
		add(mapped)
	}
	add(raw)
	bare := BaseModelName(raw)
	if mapped := canonicalBillingName(bare); mapped != "" {
		add(mapped)
	}
	add(bare)
	return out
}

// ResolveBillingModelName 返回首选的计费模型名（候选列表的第一项）。
func ResolveBillingModelName(name string) string {
	candidates := ResolveBillingModelCandidates(name)
	if len(candidates) == 0 {
		return ""
	}
	return candidates[0]
}

// ---------------------------------------------------------------------------
// 内置表达式表（billing-02 §5.1）
// ---------------------------------------------------------------------------

var (
	builtinMu    sync.RWMutex
	builtinExprs = map[string]string{}
)

// SetBuiltinBillingExpressions 安装内置表达式表（整表替换）。
//
// 【为什么默认是空表】billing-02 §5.1 只列出 4 个模型键（gpt-image-2 族与
// gpt-6-astra），没有给出价格数值；凭空填价会造成真实计费错误。本包提供
// 「内置表达式优先于默认价、但低于已配置传统价」的机制，表内容由配置/管理面
// 在装配时注入。任务 2 的优先级逻辑不依赖表内具体条目。
func SetBuiltinBillingExpressions(m map[string]string) {
	copyMap := make(map[string]string, len(m))
	for k, v := range m {
		copyMap[k] = v
	}
	builtinMu.Lock()
	builtinExprs = copyMap
	builtinMu.Unlock()
}

// BuiltinBillingExpression 返回模型的内置表达式与命中标记。
// 与 DB 查找一致，按候选顺序（规范名 → 原名 → 基名）匹配。
func BuiltinBillingExpression(model string) (string, bool) {
	builtinMu.RLock()
	defer builtinMu.RUnlock()
	for _, cand := range ResolveBillingModelCandidates(model) {
		if e, ok := builtinExprs[cand]; ok && strings.TrimSpace(e) != "" {
			return e, true
		}
	}
	return "", false
}

// ---------------------------------------------------------------------------
// 定价 spec 解析
// ---------------------------------------------------------------------------

// ParsedSpec 是一次定价 spec 解析的完整产物。
//
// 冻结接口 ParsePricing 只返回 PriceData；但表达式字符串与模式无法放入
// PriceData（契约没有对应字段），故额外提供本类型承载二者，供 Source 与
// 管理面（Task 7）消费。ParsePricing 等价于只取 Price 字段。
type ParsedSpec struct {
	Mode  contract.BillingMode
	Expr  string
	Price contract.PriceData
}

// pricingSpec 是 ParsePricing 接受的 JSON 形态。
//
// 数值字段用指针，以区分「键缺失」（nil）与「显式 0」。两种风格互斥：
//   - 旧倍率风格：model_ratio / completion_ratio / cache_ratio / create_cache_ratio，
//     按 billing-02 §3.2 的语义换算（1 = $2/1M）为积分绝对值；
//   - 绝对值风格：prompt_price / completion_price / cache_read_price /
//     cache_write_price（积分/1M，R9 的 DB 口径）。
type pricingSpec struct {
	Mode string `json:"mode"`
	Expr string `json:"expr"`

	FreeModel *bool `json:"free_model"`

	// 旧倍率风格（相对值）
	ModelRatio       *float64 `json:"model_ratio"`
	CompletionRatio  *float64 `json:"completion_ratio"`
	CacheRatio       *float64 `json:"cache_ratio"`
	CreateCacheRatio *float64 `json:"create_cache_ratio"`

	// 绝对值风格（积分/1M）
	PromptPrice     *float64 `json:"prompt_price"`
	CompletionPrice *float64 `json:"completion_price"`
	CacheReadPrice  *float64 `json:"cache_read_price"`
	CacheWritePrice *float64 `json:"cache_write_price"`

	// 缓存创建分档（积分/1M；键名沿用 PriceData 字段名）
	CacheCreation5mRatio *float64 `json:"cache_creation_5m_ratio"`
	CacheCreation1hRatio *float64 `json:"cache_creation_1h_ratio"`

	// 媒体乘数（相对输入价）
	ImageRatio           *float64 `json:"image_ratio"`
	AudioRatio           *float64 `json:"audio_ratio"`
	AudioCompletionRatio *float64 `json:"audio_completion_ratio"`

	// 按次价（积分/次）
	ModelPrice *float64 `json:"model_price"`
}

// 解析相关的哨兵错误，便于调用方（Task 7）映射 400。
var (
	// ErrInvalidSpec 表示 spec 不是合法 JSON 或字段取值非法。
	ErrInvalidSpec = errors.New("pricing: 定价 spec 非法")
	// ErrNilPrice 表示 Resolve 收到 nil PriceData。
	ErrNilPrice = errors.New("pricing: PriceData 为 nil")
	// ErrMissingExpression 表示 tiered_expr 模式缺少表达式。
	ErrMissingExpression = errors.New("pricing: tiered_expr 模式缺少表达式")
	// ErrUnexpectedExpression 表示非表达式模式却携带表达式。
	ErrUnexpectedExpression = errors.New("pricing: 非表达式模式不得携带表达式")
	// ErrInvalidMode 表示未知计价模式。
	ErrInvalidMode = errors.New("pricing: 未知计价模式")
	// ErrInvalidPrice 表示价格字段含 NaN/±Inf/负值。
	ErrInvalidPrice = errors.New("pricing: 价格字段必须为有限非负数")
)

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidSpec, fmt.Sprintf(format, args...))
}

func nonNegative(name string, v *float64) error {
	if v == nil {
		return nil
	}
	if math.IsNaN(*v) || math.IsInf(*v, 0) || *v < 0 {
		return fmt.Errorf("%w: %s=%v", ErrInvalidPrice, name, *v)
	}
	return nil
}

// ParsePricing 解析定价 spec 并返回 PriceData（冻结接口）。
//
// 空串等价于「全部默认」：Ratio 模式、默认输入价、媒体倍率 1。
// 需要表达式或模式时用 ParsePricingSpec。
func ParsePricing(spec string) (*contract.PriceData, error) {
	parsed, err := ParsePricingSpec(spec)
	if err != nil {
		return nil, err
	}
	price := parsed.Price
	return &price, nil
}

// ParsePricingSpec 解析定价 spec 并返回模式、表达式与 PriceData。
func ParsePricingSpec(spec string) (*ParsedSpec, error) {
	trimmed := strings.TrimSpace(spec)
	if trimmed == "" {
		price := defaultPriceData()
		return &ParsedSpec{Mode: contract.BillingModeRatio, Price: price}, nil
	}

	var raw pricingSpec
	dec := json.NewDecoder(strings.NewReader(trimmed))
	dec.DisallowUnknownFields() // 未知键=配置错误；静默忽略会让「打错的价格键」悄悄失效
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidSpec, err)
	}
	// 只允许恰好一个 JSON 值：`{...}{...}` 之类的尾随内容必须拒绝，
	// 否则第二份配置会被静默丢弃（定价配置静默丢失比报错危险得多）。
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, invalidf("JSON 之后存在多余内容")
	}

	// 全部数值字段：有限非负（billing-02 §3）。
	for name, v := range map[string]*float64{
		"model_ratio":             raw.ModelRatio,
		"completion_ratio":        raw.CompletionRatio,
		"cache_ratio":             raw.CacheRatio,
		"create_cache_ratio":      raw.CreateCacheRatio,
		"prompt_price":            raw.PromptPrice,
		"completion_price":        raw.CompletionPrice,
		"cache_read_price":        raw.CacheReadPrice,
		"cache_write_price":       raw.CacheWritePrice,
		"cache_creation_5m_ratio": raw.CacheCreation5mRatio,
		"cache_creation_1h_ratio": raw.CacheCreation1hRatio,
		"image_ratio":             raw.ImageRatio,
		"audio_ratio":             raw.AudioRatio,
		"audio_completion_ratio":  raw.AudioCompletionRatio,
		"model_price":             raw.ModelPrice,
	} {
		if err := nonNegative(name, v); err != nil {
			return nil, err
		}
	}

	legacyUsed := raw.ModelRatio != nil || raw.CompletionRatio != nil ||
		raw.CacheRatio != nil || raw.CreateCacheRatio != nil
	absoluteUsed := raw.PromptPrice != nil || raw.CompletionPrice != nil ||
		raw.CacheReadPrice != nil || raw.CacheWritePrice != nil
	if legacyUsed && absoluteUsed {
		return nil, invalidf("旧倍率风格与绝对值风格不得混用")
	}

	mode, modeSet, err := parseSpecMode(raw.Mode)
	if err != nil {
		return nil, err
	}
	if !modeSet {
		switch {
		case strings.TrimSpace(raw.Expr) != "":
			mode = contract.BillingModeTieredExpr
		case raw.ModelPrice != nil:
			mode = contract.BillingModePerCall
		default:
			mode = contract.BillingModeRatio
		}
	}

	if mode == contract.BillingModeTieredExpr {
		// 表达式是唯一价格配置（billing-02 §3.1）：表达式模式下混入数值
		// 价格字段属于配置歧义——静默丢弃会让管理员以为价列仍生效。
		// 按 fail-closed 方向直接拒绝（review M11）。
		for name, v := range map[string]*float64{
			"model_ratio":             raw.ModelRatio,
			"completion_ratio":        raw.CompletionRatio,
			"cache_ratio":             raw.CacheRatio,
			"create_cache_ratio":      raw.CreateCacheRatio,
			"prompt_price":            raw.PromptPrice,
			"completion_price":        raw.CompletionPrice,
			"cache_read_price":        raw.CacheReadPrice,
			"cache_write_price":       raw.CacheWritePrice,
			"cache_creation_5m_ratio": raw.CacheCreation5mRatio,
			"cache_creation_1h_ratio": raw.CacheCreation1hRatio,
			"image_ratio":             raw.ImageRatio,
			"audio_ratio":             raw.AudioRatio,
			"audio_completion_ratio":  raw.AudioCompletionRatio,
			"model_price":             raw.ModelPrice,
		} {
			if v != nil {
				return nil, invalidf("tiered_expr 模式不得混入数值价格字段 %s", name)
			}
		}

		e := strings.TrimSpace(raw.Expr)
		if e == "" {
			return nil, ErrMissingExpression
		}
		// 保存期即编译校验（billing-01 §2.3 / §6.4）：非法表达式绝不能
		// 进入快照，否则错误会推迟到预扣/结算热路径才暴露。
		ev, err := expr.Compile(e)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidSpec, err)
		}
		if err := validateExpressionIdentifiers(ev); err != nil {
			return nil, err
		}
		price := contract.PriceData{FreeModel: raw.FreeModel != nil && *raw.FreeModel}
		return &ParsedSpec{Mode: mode, Expr: e, Price: price}, nil
	}
	if strings.TrimSpace(raw.Expr) != "" {
		return nil, ErrUnexpectedExpression
	}
	if mode == contract.BillingModePerCall && raw.ModelPrice == nil {
		return nil, invalidf("per_call 模式必须提供 model_price")
	}

	qpu := float64(CurrentQuotaPerUnit())
	price := defaultPriceData()

	// 输入价：绝对值优先；旧倍率按 billing-02 §3.2（1 = $2/1M）换算。
	switch {
	case raw.PromptPrice != nil:
		price.ModelRatio = *raw.PromptPrice
	case raw.ModelRatio != nil:
		price.ModelRatio = *raw.ModelRatio * 2 * qpu
	}

	// 输出价：显式 0 转 -1 哨兵（见 explicitZeroRatio 注释）；显式正值
	// 直接保留；未设置保持 0，由 Resolve 按模型族兜底。
	if raw.CompletionPrice != nil {
		if *raw.CompletionPrice == 0 {
			price.CompletionRatio = explicitZeroRatio
		} else {
			price.CompletionRatio = *raw.CompletionPrice
		}
	} else if raw.CompletionRatio != nil {
		if *raw.CompletionRatio == 0 {
			price.CompletionRatio = explicitZeroRatio
		} else {
			price.CompletionRatio = price.ModelRatio * *raw.CompletionRatio
		}
	}

	// 缓存读价：显式则保留；未设置置 0 且 Set=false，由 Resolve 派生输入价 × 1.0。
	if raw.CacheReadPrice != nil {
		price.CacheRatio = *raw.CacheReadPrice
		price.CacheRatioSet = true
	} else if raw.CacheRatio != nil {
		price.CacheRatio = price.ModelRatio * *raw.CacheRatio
		price.CacheRatioSet = true
	}

	// 缓存写价：显式则保留；未设置由 Resolve 派生输入价 × 1.25。
	if raw.CacheWritePrice != nil {
		price.CacheCreationRatio = *raw.CacheWritePrice
		price.CacheCreationRatioSet = true
	} else if raw.CreateCacheRatio != nil {
		price.CacheCreationRatio = price.ModelRatio * *raw.CreateCacheRatio
		price.CacheCreationRatioSet = true
	}

	if raw.CacheCreation5mRatio != nil {
		price.CacheCreation5mRatio = *raw.CacheCreation5mRatio
		price.CacheCreationSet = true
	}
	if raw.CacheCreation1hRatio != nil {
		// 显式 0 用 -1 哨兵转发（契约无 1h Set 标记），Resolve 把它还原为 0。
		if *raw.CacheCreation1hRatio == 0 {
			price.CacheCreation1hRatio = explicitZeroRatio
		} else {
			price.CacheCreation1hRatio = *raw.CacheCreation1hRatio
		}
	}

	if raw.ImageRatio != nil {
		price.ImageRatio = *raw.ImageRatio
		price.ImageRatioSet = true
	}
	if raw.AudioRatio != nil {
		price.AudioRatio = *raw.AudioRatio
		price.AudioRatioSet = true
	}
	if raw.AudioCompletionRatio != nil {
		price.AudioCompletionRatio = *raw.AudioCompletionRatio
	}

	price.UsePrice = mode == contract.BillingModePerCall
	if raw.ModelPrice != nil {
		price.ModelPrice = *raw.ModelPrice
	}
	if raw.FreeModel != nil {
		price.FreeModel = *raw.FreeModel
	} else if !price.UsePrice {
		price.FreeModel = price.ModelRatio == 0 && price.CompletionRatio <= 0 &&
			price.CacheRatio == 0 && price.CacheCreationRatio == 0
	} else {
		price.FreeModel = price.ModelPrice == 0
	}

	if err := validatePriceData(&price); err != nil {
		return nil, err
	}
	return &ParsedSpec{Mode: mode, Price: price}, nil
}

// 表达式标识符白名单（保存期校验，billing-01 §2.1/§2.2、billing-00 §4.1）。
//
// 【为什么必须白名单】表达式来自管理端配置；若允许任意标识符，拼写错误
// （如把 cc1h 写成 cch1）会编译成「未绑定变量」而在运行期才失败，或更糟：
// 未来的变量扩展让旧表达式静默改变语义。白名单含：
//   - 数据变量：p/c/len/cr/cc/cc1h/img/img_cr/img_o/ai/ao/image_count；
//   - 函数名：billing-00 §4.1 的语言全集（当前 Task 1 只编译数学子集，
//     其余名字在编译期即被拒绝；此处列出是为未来扩展不误伤）。
//
// UsedVars 含函数名（billing-01 §4.1），所以先按函数名放行再按变量名判定。
var (
	allowedExpressionVars = map[string]struct{}{
		"p": {}, "c": {}, "len": {}, "cr": {}, "cc": {}, "cc1h": {},
		"img": {}, "img_cr": {}, "img_o": {}, "ai": {}, "ao": {}, "image_count": {},
	}
	allowedExpressionFuncs = map[string]struct{}{
		"min": {}, "max": {}, "abs": {}, "ceil": {}, "floor": {}, "round": {},
		"tier": {}, "fixed": {}, "param": {}, "header": {}, "has": {},
		"hour": {}, "minute": {}, "weekday": {}, "month": {}, "day": {}, "now": {},
		"u": {},
	}
)

// validateExpressionIdentifiers 拒绝白名单之外的数据变量/函数名。
func validateExpressionIdentifiers(ev *expr.Evaluator) error {
	for name := range ev.UsedVars() {
		if _, ok := allowedExpressionFuncs[name]; ok {
			continue
		}
		if _, ok := allowedExpressionVars[name]; ok {
			continue
		}
		return fmt.Errorf("%w: 表达式引用了未知标识符 %q", ErrInvalidSpec, name)
	}
	return nil
}

// validatePriceData 校验换算**之后**的价格字段有限非负。
//
// 入参校验只能保证原始值有限；`model_ratio × 2 × QPU` 等换算仍可能溢出
// 为 +Inf（如 model_ratio=1e308），必须在进入快照前拒绝。
// -1 只允许出现在两个「显式零哨兵」字段上（completion / 1h 缓存创建）。
func validatePriceData(p *contract.PriceData) error {
	fields := []struct {
		name          string
		v             float64
		allowSentinel bool
	}{
		{"model_ratio", p.ModelRatio, false},
		{"completion_ratio", p.CompletionRatio, true},
		{"cache_ratio", p.CacheRatio, false},
		{"cache_creation_ratio", p.CacheCreationRatio, false},
		{"cache_creation_5m_ratio", p.CacheCreation5mRatio, false},
		{"cache_creation_1h_ratio", p.CacheCreation1hRatio, true},
		{"image_ratio", p.ImageRatio, false},
		{"audio_ratio", p.AudioRatio, false},
		{"audio_completion_ratio", p.AudioCompletionRatio, false},
		{"model_price", p.ModelPrice, false},
	}
	for _, f := range fields {
		if f.allowSentinel && f.v == explicitZeroRatio {
			continue
		}
		if math.IsNaN(f.v) || math.IsInf(f.v, 0) || f.v < 0 {
			return fmt.Errorf("%w: 换算后的 %s=%v", ErrInvalidPrice, f.name, f.v)
		}
	}
	return nil
}

func parseSpecMode(s string) (contract.BillingMode, bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return 0, false, nil
	case "ratio", "per_token":
		return contract.BillingModeRatio, true, nil
	case "tiered_expr", "expression", "expr":
		return contract.BillingModeTieredExpr, true, nil
	case "per_call", "per-call", "per_request", "per-request":
		return contract.BillingModePerCall, true, nil
	default:
		return 0, false, fmt.Errorf("%w: %q", ErrInvalidMode, s)
	}
}

// defaultPriceData 返回全部默认的 PriceData。
//
// 默认输入价以积分绝对值给出；输出价与缓存读写价留 0（未设置）+
// Set=false，由 Resolve 用模型族倍率与缺省乘数派生（保证「未设置」与
// 「显式 0」通过 Set 标记/哨兵可分）。
func defaultPriceData() contract.PriceData {
	return contract.PriceData{
		ModelRatio: DefaultPromptPricePer1M(),
		// CompletionRatio 0 = 未设置 → Resolve 按模型族兜底；
		// CacheRatio/CacheCreationRatio 同理由 Set=false + 0 表达。
		ImageRatio:           DefaultImageRatio,
		AudioRatio:           DefaultAudioRatio,
		AudioCompletionRatio: DefaultAudioCompletionRatio,
	}
}

// ---------------------------------------------------------------------------
// 模式优先级（billing-02 §2.2 / §2.4）
// ---------------------------------------------------------------------------

// ResolveMode 实现 billing-02 §2.2 的优先级判定（早返回）：
//
//  1. 显式 tiered_expr → 表达式（显式模式最高优先，§2.4 行 1）；
//  2. 显式 ratio / per_call → 尊重显式模式（R9 的 mode 列永远有值）：
//     显式 ratio 不会被内置表达式抢走（§2.2(0)），显式 per_call 即按次；
//  3. 未显式时：已配置传统价（ModelRatio/ModelPrice 等）→ 按次或传统倍率
//     （「已配置旧价优先于内置表达式」，§2.4 行 2）；
//  4. 命中内置表达式 → 表达式；
//  5. 其余 → 传统倍率兜底。
//
// modeExplicit 让调用方区分「显式写了 ratio」与「未配置（零值也是 ratio）」；
// DB 的 model_pricing.mode 永远显式（NOT NULL），故 Source 传 true。
// price 为 nil 表示没有任何已配置价（按未配置处理，不 panic）。
func ResolveMode(price *contract.PriceData, model string, configured contract.BillingMode, modeExplicit bool) contract.BillingMode {
	if configured == contract.BillingModeTieredExpr {
		return contract.BillingModeTieredExpr
	}
	if modeExplicit {
		// 显式模式优先于「已配置旧价」与内置表达式（billing-02 §2.2(0)）。
		if configured == contract.BillingModePerCall {
			return contract.BillingModePerCall
		}
		return contract.BillingModeRatio
	}
	if hasConfiguredLegacyPrice(price) {
		if price.UsePrice {
			return contract.BillingModePerCall
		}
		return contract.BillingModeRatio
	}
	if _, ok := BuiltinBillingExpression(model); ok {
		return contract.BillingModeTieredExpr
	}
	if configured == contract.BillingModePerCall || (price != nil && price.UsePrice) {
		return contract.BillingModePerCall
	}
	return contract.BillingModeRatio
}

func hasConfiguredLegacyPrice(price *contract.PriceData) bool {
	if price == nil {
		return false
	}
	return price.UsePrice || price.ModelRatio != 0 || price.CompletionRatio != 0 ||
		price.CacheRatio != 0 || price.CacheCreationRatio != 0
}

// ---------------------------------------------------------------------------
// 快照产出
// ---------------------------------------------------------------------------

// Resolve 按模型与模式归一 PriceData 并产出 PriceSnapshot（冻结接口）。
//
// 本入口不携带表达式：表达式模式请用 ResolveExpression（Source 即如此）。
func Resolve(price *contract.PriceData, model string, mode contract.BillingMode) (contract.PriceSnapshot, error) {
	return ResolveExpression(price, model, mode, "")
}

// ResolveExpression 在 Resolve 的基础上冻结表达式与哈希。
//
// 归一化内容：
//   - Ratio 模式：按 Set 标记与显式零哨兵填入缓存读/写、1h 缓存创建与补全倍率默认值；
//   - 分组倍率：GroupSpecialRatio（组间覆盖）优先于 GroupRatio，缺省 1；
//   - CacheWriteMode 只读元信息由模型名与缓存写配置派生（billing-02 §6.3）。
//
// 【为什么需要 -1 哨兵】契约缺少 CompletionRatioSet 等标记；若把 0 一律当
// 未设置，就会吞掉管理员的「输出免费 / 1h 缓存免费」显式零配置，故用 -1
// 单独承载显式零，Resolve 在这一层还原。
func ResolveExpression(price *contract.PriceData, model string, mode contract.BillingMode, exprString string) (contract.PriceSnapshot, error) {
	if price == nil {
		return contract.PriceSnapshot{}, ErrNilPrice
	}
	if mode < contract.BillingModeRatio || mode > contract.BillingModePerCall {
		return contract.PriceSnapshot{}, fmt.Errorf("%w: %d", ErrInvalidMode, mode)
	}

	p := *price // 值拷贝：Resolve 不得改写调用方持有的快照源对象

	effective := mode
	if mode != contract.BillingModeTieredExpr && p.UsePrice {
		// ModelPrice 命中派生按次（billing-02 §2.4 行 3：价先于率）。
		effective = contract.BillingModePerCall
	}

	var hash string
	if effective == contract.BillingModeTieredExpr {
		e := strings.TrimSpace(exprString)
		if e == "" {
			return contract.PriceSnapshot{}, ErrMissingExpression
		}
		ev, err := expr.Compile(e)
		if err != nil {
			return contract.PriceSnapshot{}, fmt.Errorf("%w: %v", ErrInvalidSpec, err)
		}
		if err := validateExpressionIdentifiers(ev); err != nil {
			return contract.PriceSnapshot{}, err
		}
		sum := sha256.Sum256([]byte(e))
		hash = hex.EncodeToString(sum[:])
		exprString = e
	} else if strings.TrimSpace(exprString) != "" {
		return contract.PriceSnapshot{}, ErrUnexpectedExpression
	}

	if effective == contract.BillingModeRatio {
		applyRatioDefaults(&p, model)
	}
	if effective == contract.BillingModePerCall {
		p.UsePrice = true
	}

	groupRatio, err := groupRatioFromInfo(p.GroupRatioInfo)
	if err != nil {
		return contract.PriceSnapshot{}, err
	}

	return contract.PriceSnapshot{
		Mode:       effective,
		Price:      p,
		ExprString: exprString,
		ExprHash:   hash,
		GroupRatio: groupRatio,
		// 预扣上浮系数默认 1（G7-R05 改判，billing-00 §3.4）；≤0 由会话层拒绝。
		PreConsumeMultiplier: 1,
		CacheWriteMode:       deriveCacheWriteMode(model, p),
		// IsClaudeUsageSemantic 是上游协议事实，由会话层从 RelayContext 写入，
		// 定价层不得猜测（billing-00 §3.4）。
	}, nil
}

// applyRatioDefaults 填充 Ratio 模式的缺省值（显式零由 Set 标记/哨兵保护）。
func applyRatioDefaults(p *contract.PriceData, model string) {
	prompt := p.ModelRatio

	// 补全倍率：0 = 未设置 → 模型族硬编码兜底；-1 = 显式零 → 还原为 0；
	// 正值原样保留。显式零不能被兜底覆盖（billing-00 §3.2）。
	switch p.CompletionRatio {
	case explicitZeroRatio:
		p.CompletionRatio = 0
	case 0:
		p.CompletionRatio = CompletionPricePer1M(model, prompt, nil)
	}

	// 缓存读：未设置（0 且无 Set）→ 输入价 × DefaultCacheRatio；显式 0 保留。
	if !p.CacheRatioSet && p.CacheRatio == 0 {
		p.CacheRatio = prompt * DefaultCacheRatio
	}

	// 缓存创建基准：未设置 → 输入价 × 1.25；显式 0 保留。
	if !p.CacheCreationRatioSet && p.CacheCreationRatio == 0 {
		p.CacheCreationRatio = prompt * DefaultCacheCreationRatio
	}

	// 5m/通用 = 基准；显式 5m 值优先（CacheCreationSet）。
	if !p.CacheCreationSet && p.CacheCreation5mRatio == 0 {
		p.CacheCreation5mRatio = p.CacheCreationRatio
	}

	// 1h：显式值优先；-1 哨兵表示显式 0；未设置时仅 claude 名继承 ×1.6。
	switch {
	case p.CacheCreation1hRatio == explicitZeroRatio:
		p.CacheCreation1hRatio = 0
	case p.CacheCreation1hRatio != 0:
		// 显式绝对值，保留
	case modelHasClaude(model):
		p.CacheCreation1hRatio = p.CacheCreationRatio * claudeCacheCreation1hFactor
	default:
		p.CacheCreation1hRatio = p.CacheCreationRatio
	}

	// 媒体倍率：未设置（0 且无 Set）→ 1；显式 0 保留。
	if !p.ImageRatioSet && p.ImageRatio == 0 {
		p.ImageRatio = DefaultImageRatio
	}
	if !p.AudioRatioSet && p.AudioRatio == 0 {
		p.AudioRatio = DefaultAudioRatio
	}
	// 契约没有 AudioCompletionRatioSet：ParsePricing/Source 未设置时填 1，
	// 因此 0 在此处按显式零保留（不再二次兜底）。
}

// groupRatioFromInfo 取生效的分组倍率：组间覆盖（Special）优先，其次分组倍率。
//
// 缺省值 1 只用于「GroupRatioInfo 为零值」（调用方未提供分组信息）：
// billing-02 §4.7 规定分组倍率未命中按 1 计。但显式传入的 NaN/±Inf/负值
// 是配置错误，必须报错而不是静默归一为 1（否则错误倍率会静默按 1 计费）；
// 组间覆盖标记为 true 时其 Special 值同样必须是有限正数（review M7）。
func groupRatioFromInfo(info contract.GroupRatioInfo) (float64, error) {
	if info.HasSpecialRatio {
		special := info.GroupSpecialRatio
		if math.IsNaN(special) || math.IsInf(special, 0) || special <= 0 {
			return 0, fmt.Errorf("%w: 组间覆盖倍率 %v", ErrInvalidGroupRatio, special)
		}
		return special, nil
	}
	ratio := info.GroupRatio
	switch {
	case ratio == 0:
		// 零值 = 调用方未提供分组信息 → 按 billing-02 §4.7 的缺省 1。
		return 1, nil
	case math.IsNaN(ratio) || math.IsInf(ratio, 0) || ratio < 0:
		return 0, fmt.Errorf("%w: 分组倍率 %v", ErrInvalidGroupRatio, ratio)
	default:
		return ratio, nil
	}
}

// deriveCacheWriteMode 由模型名与缓存写配置派生只读元信息（billing-02 §6.3）。
func deriveCacheWriteMode(model string, p contract.PriceData) contract.CacheWriteMode {
	if modelHasClaude(model) {
		return contract.CacheWriteClaudeTTL
	}
	if p.CacheCreationRatioSet || p.CacheCreationSet {
		return contract.CacheWriteStandard
	}
	return contract.CacheWriteNone
}
