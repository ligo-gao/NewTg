package billing

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"

	"newtg/gateway/internal/contract"
)

// NormalizeTokens 把上游用量归一成表达式可消费的 TokenParams（billing-00 §5）。
//
// 核心不变量：P / C 表示「**未被表达式单独定价的 token**」。归一化是**双向**的
// （billing-00 §5.2，基线最容易漏的一条）：
//
//   - GPT / OpenAI（PromptIncludesCache=true，prompt_tokens 已含全部子类）：
//     **减**——表达式引用了某个子类变量，才从 P（输入）/ C（输出）中扣除该子类；
//     未引用的子类留在 P/C 里按基础价计费。
//   - Claude / Anthropic（PromptIncludesCache=false，input_tokens 只含文本）：
//     **加**——未引用 cr 时必须把缓存读取加回 P。只做减法、漏掉这条加回，
//     缓存读段会「凭空消失」：P 里没有、又没有独立项，整段免费。
//
// 分支选择器只使用 M2 产出的关系布尔 Usage.PromptIncludesCache，不读取
// 语义字符串（billing-03 §4.3）：字符串来源会漂移，音频路径曾因硬编码
// isClaudeUsageSemantic=false 而漏掉加回（甲类修正 M-03-2）。
//
// snap 参数是契约形状的一部分（调用方传冻结快照），本函数刻意不读
// PriceSnapshot.IsClaudeUsageSemantic —— 上游格式事实以 Usage 的关系布尔为准。
func NormalizeTokens(usage contract.Usage, snap contract.PriceSnapshot, usedVars map[string]bool) contract.TokenParams {
	_ = snap // 见上：格式语义只认 Usage 关系布尔

	p := max(usage.Prompt, 0)
	c := max(usage.Completion, 0)

	// Claude 语义 = input_tokens 不含缓存；与 len 取值、img_cr 拆分共用同一布尔
	// （billing-00 §5.3/§5.4 明确复用，避免三处判定漂移）。
	claudeSemantic := !usage.PromptIncludesCache

	// 缓存写归一（billing-03 §4.4 / 决议 C4）：M2 保真传递 GPT 的
	// cache_write_tokens 与 Claude 的 cached_creation_tokens，取较大值防止
	// 同一份缓存写被两条协议口径重复计。两值皆缺时取 Usage.CacheWrite。
	cc := normalizedCacheWrite(usage)
	cc5m, cc1h := cc, int64(0)
	if claudeSemantic {
		// 5m / 1h 拆分只对 Claude 语义有意义（billing-03 §4.5）：非 Claude
		// 语义下 cc1h 恒为 0，cc 即通用缓存写。
		cc5m, cc1h = splitClaudeCacheCreation(cc, usage.Raw)
	}

	cr := max(usage.CacheRead, 0)
	img := max(usage.PromptDetail.Image, 0)
	ai := max(usage.PromptDetail.Audio, 0)
	imgO := max(usage.CompletionDetail.Image, 0)
	ao := max(usage.CompletionDetail.Audio, 0)

	// len 必须在任何扣除之前取值（billing-00 §5.3）：档位条件必须用 len，
	// 用 P 会让缓存命中把 P 变小、误判低档。Claude 的 input 只含文本，
	// 故上下文长度要把缓存读/缓存写加回来。
	var length int64
	if claudeSemantic {
		length = p + cr + cc5m + cc1h
	} else {
		length = p
	}

	// 图片缓存 img_cr 三段式（billing-00 §5.4）：三条全满足才拆分 ——
	// 引用 img_cr、非 Claude 语义、上游给出有效图片缓存明细。
	// 明细缺失/异常一律回退原聚合逻辑：imgCR 保持 0，缓存总量继续进 cr，
	// **不推测比例**（推测会把不确定的上游明细变成确定的计费差异）。
	imgCR := int64(0)
	if !claudeSemantic && usedVars["img_cr"] && usage.PromptDetail.CacheReadImage > 0 {
		cachedImage := usage.PromptDetail.CacheReadImage
		if validImageCacheSplit(p, cr, img, ai, usage.PromptDetail.Text, cachedImage) {
			imgCR = cachedImage
			cr -= imgCR  // 缓存总量移出图片缓存部分
			img -= imgCR // 图片输入移出缓存部分（缓存部分不按图片输入价计）
		}
	}

	if claudeSemantic {
		// Claude 分支只对 cr 做加回；cc/img/ai 在 Claude 用量里本就独立计数，
		// 不在 input_tokens 内，不能在此再减（billing-03 §4.6）。
		if !usedVars["cr"] {
			p += cr
		}
	} else {
		// 逐子类扣除表（billing-00 §5.2 / billing-03 §4.5）：未引用的子类不扣。
		if usedVars["cr"] {
			p -= cr
		}
		if usedVars["cc"] {
			p -= cc5m // cc 只对应 5m/通用档；1h 由 cc1h 单独扣
		}
		if usedVars["cc1h"] {
			p -= cc1h
		}
		if usedVars["img"] {
			p -= img
		}
		if usedVars["img_cr"] {
			p -= imgCR
		}
		if usedVars["ai"] {
			p -= ai
		}
		if usedVars["img_o"] {
			c -= imgO
		}
		if usedVars["ao"] {
			c -= ao
		}
	}

	// 下限钳 0：上游可能给出未调整前缀计数的子类，导致负余量；
	// 负 P/C 进入表达式会反转费用方向，必须钳制（billing-00 §5.2）。
	p = max(p, 0)
	c = max(c, 0)

	// ImageCount 的默认值 1 属于「变量契约」而非上游用量（契约 TokenParams
	// 注释）：会话在结算时若为图像请求，会用 UsageFact.ImageCount 覆盖它。
	return contract.TokenParams{
		P:          p,
		C:          c,
		Len:        length,
		CR:         cr,
		CC5m:       cc5m,
		CC1h:       cc1h,
		Img:        img,
		ImgCR:      imgCR,
		AI:         ai,
		ImgO:       imgO,
		AO:         ao,
		ImageCount: 1,
	}
}

// normalizedCacheWrite 返回归一后的缓存写 token 数：
// max(Usage.CacheWrite, Raw["cache_write_tokens"], Raw["cached_creation_tokens"])，
// 并钳 ≥ 0。Raw 取值失败按 0 参与（即取存在的那个）。
func normalizedCacheWrite(usage contract.Usage) int64 {
	cc := max(usage.CacheWrite, 0)
	for _, key := range []string{"cache_write_tokens", "cached_creation_tokens"} {
		if v, ok := rawInt64(usage.Raw, key); ok {
			cc = max(cc, v)
		}
	}
	return max(cc, 0)
}

// splitClaudeCacheCreation 把 Claude 的缓存写拆成 5m/通用与 1h 两档
// （billing-03 §4.5，取自 Raw 的 cache_creation.ephemeral_* 或扁平键）。
//
// 归一后的总量 cc 是上界：拆分结果必须落在 cc 内，否则说明上游明细与
// 总量不一致，按总量钳制（不把不一致放大成超计费）。
func splitClaudeCacheCreation(cc int64, raw map[string]any) (cc5m, cc1h int64) {
	v5, ok5 := rawInt64Any(raw,
		"cache_creation.ephemeral_5m_input_tokens",
		"ephemeral_5m_input_tokens")
	v1, ok1 := rawInt64Any(raw,
		"cache_creation.ephemeral_1h_input_tokens",
		"ephemeral_1h_input_tokens")
	if !ok5 && !ok1 {
		return cc, 0
	}

	cc1h = max(v1, 0)
	cc1h = min(cc1h, cc)
	if ok5 {
		cc5m = min(max(v5, 0), cc-cc1h)
	} else {
		// 只给了 1h 明细：剩余部分按 5m/通用档处理（与「缺失按 0 参与」一致）。
		cc5m = cc - cc1h
	}
	return cc5m, cc1h
}

// validImageCacheSplit 实现 img_cr 的三段式有效性校验（billing-00 §5.4 五项）：
//
//  1. cachedImage ≥ 0（调用方已保证 > 0）
//  2. cached ≥ cachedImage
//  3. image ≥ cachedImage
//  4. cached ≤ promptTokens
//  5. image ≤ promptTokens − (cached − cachedImage)
//
// 之后再对文本/图片/音频/非图片缓存子类逐个要求 0 ≤ x ≤ remaining 并递减，
// 保证子类互不重叠、总量守恒。任一不满足即回退原聚合逻辑。
func validImageCacheSplit(prompt, cached, image, audio, text, cachedImage int64) bool {
	if cachedImage < 0 || cached < cachedImage || image < cachedImage {
		return false
	}
	if cached > prompt {
		return false
	}
	if image > prompt-(cached-cachedImage) {
		return false
	}
	if text < 0 || audio < 0 {
		return false
	}
	remaining := prompt
	for _, x := range []int64{text, image - cachedImage, audio, cached - cachedImage} {
		if x < 0 || x > remaining {
			return false
		}
		remaining -= x
	}
	return true
}

// rawInt64Any 依次尝试多个键路径，返回第一个成功解析的值。
func rawInt64Any(raw map[string]any, paths ...string) (int64, bool) {
	for _, path := range paths {
		if v, ok := rawInt64(raw, path); ok {
			return v, true
		}
	}
	return 0, false
}

// rawInt64 从 Usage.Raw 中按点号路径取整数（如 "cache_creation.ephemeral_5m_input_tokens"）。
//
// 接受 int/int32/int64/float64/json.Number/string：M2 的 Raw 是 map[string]any，
// 不同协议解码器产出的数值类型不统一；计费侧不能依赖某一种具体类型。
// 非有限浮点（NaN/Inf）视为缺失 —— 把 NaN 静默转成整数会产生不可对账的金额。
func rawInt64(raw map[string]any, path string) (int64, bool) {
	if raw == nil {
		return 0, false
	}
	var cur any = raw
	for part := range strings.SplitSeq(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return 0, false
		}
		cur, ok = m[part]
		if !ok {
			return 0, false
		}
	}
	return anyToInt64(cur)
}

func anyToInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int32:
		return int64(n), true
	case int64:
		return n, true
	case uint:
		if uint64(n) > math.MaxInt64 {
			return math.MaxInt64, true
		}
		return int64(n), true
	case uint64:
		if n > math.MaxInt64 {
			return math.MaxInt64, true
		}
		return int64(n), true
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return 0, false
		}
		// 越界浮点直接钳到 int64 边界：token 数不可能到这个量级，
		// 钳制只是为了不让 Go 的 float→int 转换产生实现相关的回绕。
		if n >= math.MaxInt64 {
			return math.MaxInt64, true
		}
		if n <= math.MinInt64 {
			return math.MinInt64, true
		}
		return int64(n), true
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i, true
		}
		f, err := n.Float64()
		if err != nil {
			return 0, false
		}
		return anyToInt64(f)
	case string:
		if i, err := strconv.ParseInt(strings.TrimSpace(n), 10, 64); err == nil {
			return i, true
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		if err != nil {
			return 0, false
		}
		return anyToInt64(f)
	default:
		return 0, false
	}
}
