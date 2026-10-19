package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"newtg/gateway/internal/billing/pricing"
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/relay/ir"
)

// ============================================================================
// 多模态 / 原生协议 usage 提取（M4 · Task 3）
//
// 权威：
//   - relay-03 §6.2 规格表 row 4（Anthropic）、row 7（Gemini）、row 1（OpenAI）；
//     §6.1 归一化总则（非负、子类关系布尔）；§11.2 UsageSource 赋值。
//   - relay-04 §0.4（计费方式三分类）、§4.7（图片按张数）、§8.1（张数只按
//     payload 推导）、§8.2（音频时长 → token）。
//   - GC-3：三源优先级与 M2 一致，**不因多模态而改变**。
//   - GC-4：图片/音频折算复用 pricing 的 per-unit 系数，禁止硬编码（音频
//     「1 分钟 = 1000 token」的唯一真源见 pricing.AudioTokensFromSeconds）。
//
// 【为什么单独一个入口而不是改 ExtractUsage】M2 的 ExtractUsage 把原生来源
// 固定为 OpenAI `usage` 形状（ir.ParseOpenAIUsage）。Anthropic 的
// `input_tokens` 与 Gemini 的 `usageMetadata` 若走同一条路径会被解析成零值或
// 被误判为 GPT 语义（PromptIncludesCache 方向搞反 → 缓存段成倍错算）。
// 保留 M2 入口不动，新增本入口按渠道族分派，路由层可逐步切换。
// ============================================================================

// ExtractUsageMultimodal 按渠道族提取一份多模态/原生协议用量事实。
//
// 三源优先级（GC-3 / R-05）与 ExtractUsage 完全一致：原生 usage >
// include_usage 注入 > `x-model-response-usage` 头；首个**非空**来源胜出，
// 绝不叠加。差别只在「原生 usage 长什么样」：
//   - Anthropic / Moonshot（Claude 协议）：input_tokens / output_tokens /
//     cache_read_input_tokens / cache_creation_input_tokens；流式
//     `message_start.message.usage` 与 `message_delta.usage` 都支持。
//   - Gemini / Vertex：usageMetadata（promptTokenCount / candidatesTokenCount /
//     cachedContentTokenCount / thoughtsTokenCount / 明细数组）。
//   - 其余（OpenAI 兼容）：沿用 ir.ParseOpenAIUsage 的字段路径。
//
// 返回的 UsageFact 只填 Model / Usage / ImageCount；Latency/FirstToken/Stream/
// Attempts 仍由调用方填写（契约 §6.1）。未知 kind 走 OpenAI 形状的安全默认，
// 不 panic。
//
// 图片张数按 relay-04 §8.1 只从响应 payload 推导；解析异常时把 Source 置为
// UsageReservedDefault 并保持 ImageCount 零值——计费会话据此保持请求预留量
// （billing/session.go adoptImageCount），绝不凭空造数。
func ExtractUsageMultimodal(resp *contract.Response, headers http.Header,
	injected *contract.Usage, kind contract.ChannelType) *contract.UsageFact {

	fact := &contract.UsageFact{}
	if resp != nil {
		fact.Model = resp.Model
	}

	// 优先级 1：原生 usage（按渠道族/协议形状解析）。
	if native := nativeUsageMultimodal(resp, kind); native != nil {
		fact.Usage = *native
		finalizeMultimodal(fact, resp, kind)
		return fact
	}

	// 优先级 2：调用方注入的合成用量（include_usage 流式累计）。
	if injected != nil {
		clamped := clampUsage(*injected)
		if !usageIsZero(clamped) {
			fact.Usage = clamped
			finalizeMultimodal(fact, resp, kind)
			return fact
		}
	}

	// 优先级 3：非标准响应头。
	if fromHeader, ok := headerUsage(headers); ok {
		fact.Usage = fromHeader
		finalizeMultimodal(fact, resp, kind)
		return fact
	}

	// 三源全缺席：零值 + estimated（与 M2 ExtractUsage 同一语义）。
	fact.Usage = contract.Usage{Source: contract.UsageEstimated}
	finalizeMultimodal(fact, resp, kind)
	return fact
}

// finalizeMultimodal 补两类与来源无关的事实：
//  1. 协议关系布尔的安全默认（仅当没有任何原生解析结果时）；
//  2. 多模态折算：图片张数（payload 推导）与音频时长 → token。
//
// 顺序不能反：图片解析异常会把 Source 改成 reserved_default，若先做音频
// 再覆盖 Source 会把「保持预留量」的语义丢掉。
func finalizeMultimodal(fact *contract.UsageFact, resp *contract.Response, kind contract.ChannelType) {
	if fact.Usage.Source == contract.UsageEstimated && channelFamily(kind) == familyOpenAI {
		// 关系布尔是**协议属性**而非用量数字（relay-03 §6.2 row 1）：OpenAI
		// 格式即使没有 usage，prompt 也含缓存、completion 也含推理。Claude/
		// Gemini 的 false 语义已在各自解析器里显式赋值，这里只兜 OpenAI。
		fact.Usage.PromptIncludesCache = true
		fact.Usage.ReasoningIncludedInCompletion = true
	}

	applyImageCount(fact, resp)
	applyAudioConversion(&fact.Usage, resp)
}

// ============================================================================
// 原生协议分派
// ============================================================================

type protocolFamily int

const (
	familyOpenAI protocolFamily = iota
	familyClaude
	familyGemini
)

// channelFamily 把渠道类型映射到 usage 形状族。
//
// Moonshot 走 Anthropic 原生协议（channels.go 的注释），Vertex 既可能承载
// Gemini 也可能承载 Claude；两者都归入 Gemini 族后仍由 nativeUsageMultimodal
// 的 payload 形状嗅探纠正（usageMetadata 优先、Claude 缓存键次之），因此
// 不会因为渠道名而错误解释另一协议的键。
func channelFamily(kind contract.ChannelType) protocolFamily {
	switch kind {
	case contract.ChannelTypeAnthropic, contract.ChannelTypeMoonshot:
		return familyClaude
	case contract.ChannelTypeGemini, contract.ChannelTypeVertexAi:
		return familyGemini
	default:
		return familyOpenAI
	}
}

// nativeUsageMultimodal 从 resp.Raw 中找原生 usage 对象并解析。
//
// 查找顺序（与各协议流式事件的实际形状对应）：
//  1. `usageMetadata` —— Gemini 原生（含非流式整包与每个 SSE 块）；
//  2. `message.usage` —— Anthropic `message_start`；
//  3. `usage` —— Anthropic `message_delta` / OpenAI 末块；
//  4. `meta.billed_units` —— Cohere 重排/生成。
//
// 「存在且非空」判定与 M2 nativeUsage 一致：空对象/类型非法视为缺席，
// 让后续来源继续参与优先级（R-05：首个**非空**来源胜出）。
func nativeUsageMultimodal(resp *contract.Response, kind contract.ChannelType) *contract.Usage {
	if resp == nil || len(bytes.TrimSpace(resp.Raw)) == 0 {
		return nil
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(resp.Raw, &envelope); err != nil {
		// 不是 JSON 对象：可能是二进制/纯文本响应，没有原生 usage。
		return nil
	}
	family := channelFamily(kind)

	if raw, ok := envelope["usageMetadata"]; ok {
		if parsed := parseGeminiUsage(raw); parsed != nil {
			return parsed
		}
	}

	if raw, ok := envelope["message"]; ok {
		var message struct {
			Usage json.RawMessage `json:"usage"`
		}
		if json.Unmarshal(raw, &message) == nil {
			if parsed := parseUsageObject(message.Usage, family); parsed != nil {
				return parsed
			}
		}
	}

	if raw, ok := envelope["usage"]; ok {
		if parsed := parseUsageObject(raw, family); parsed != nil {
			return parsed
		}
	}

	if raw, ok := envelope["meta"]; ok {
		if parsed := parseCohereBilledUnits(raw); parsed != nil {
			return parsed
		}
	}
	return nil
}

// parseUsageObject 按 payload 形状（而非仅渠道类型）选择解析器。
//
// 【为什么形状优先】Vertex 渠道可承载 Claude 或 Gemini；部分 OpenAI 兼容网关
// 会透传 Anthropic 的 usage。以键集合判定可避免「渠道类型对、payload 形状
// 不对」时的静默零值；渠道族只作为无歧义键（只剩 input/output）时的提示。
func parseUsageObject(raw json.RawMessage, family protocolFamily) *contract.Usage {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &object); err != nil || len(object) == 0 {
		// 空对象 / 类型非法：没有可采信的上游事实，继续下一个来源。
		return nil
	}

	if hasAnyKey(object, "promptTokenCount", "candidatesTokenCount") {
		return parseGeminiUsage(trimmed)
	}
	// OpenAI 形状优先于渠道族：Anthropic 兼容网关/跨协议转发可能把
	// prompt_tokens 透传到 Claude 渠道，此时按 payload 而不是渠道名解释，
	// 避免「渠道对、形状不对」被解析成零值。
	if hasAnyKey(object, "prompt_tokens", "completion_tokens") {
		return parseOpenAIUsageObject(trimmed, false)
	}

	// input_tokens / output_tokens 是**歧义形状**：Responses 风格与 Claude
	// 都用这两个键。判定顺序：
	//  1. 出现 input_tokens_details / output_tokens_details → OpenAI Responses；
	//  2. 出现 Claude 缓存键 → Claude（任何渠道族）；
	//  3. 渠道族为 Claude → Claude；
	//  4. 渠道族为 Gemini/Vertex 且只有 input/output（无 OpenAI 明细）→
	//     Vertex 上的 Anthropic 模型（family 提示，relay-03 §6.2 row 6）；
	//  5. 其余（OpenAI/未知）→ Responses 风格。
	if hasAnyKey(object, "input_tokens", "output_tokens") {
		if hasAnyKey(object, "input_tokens_details", "output_tokens_details") {
			return parseResponsesUsage(trimmed)
		}
		if family == familyClaude || looksClaudeUsage(object) ||
			(family == familyGemini && !hasAnyKey(object, "prompt_tokens", "completion_tokens")) {
			return parseClaudeUsage(trimmed)
		}
		return parseResponsesUsage(trimmed)
	}

	if family == familyClaude || looksClaudeUsage(object) {
		return parseClaudeUsage(trimmed)
	}
	// 只带 total_tokens 的重排/嵌入形状：允许 total → prompt 兜底；
	// 其它未识别对象不应伪造用量（见 parseOpenAIUsageObject）。
	return parseOpenAIUsageObject(trimmed, !hasAnyKey(object,
		"prompt_tokens", "completion_tokens", "input_tokens", "output_tokens"))
}

// hasAnyKey 判断顶层对象是否含任一指定键（键存在即为协议事实，值可为 0）。
func hasAnyKey(object map[string]json.RawMessage, keys ...string) bool {
	for _, key := range keys {
		if _, ok := object[key]; ok {
			return true
		}
	}
	return false
}

// looksClaudeUsage 识别 Anthropic 专有缓存键。
//
// 只做键存在性判定：Claude 在无缓存命中时也可能发 `cache_read_input_tokens: 0`，
// 键本身就是协议事实（billing/session.go hasClaudeUsageKeys 同口径）。
// 该判定同时服务 M3 usage_semantic 的**正向** Claude 指示：Raw 必须原样保留
// 这些键，Gemini/OpenAI 的 Raw 不得伪造它们。
func looksClaudeUsage(object map[string]json.RawMessage) bool {
	for _, key := range []string{
		"cache_read_input_tokens",
		"cache_creation_input_tokens",
		"cache_creation",
	} {
		if _, ok := object[key]; ok {
			return true
		}
	}
	return false
}

// parseClaudeUsage 解析 Anthropic usage（relay-03 §6.2 row 4）。
//
// 【F7 · 委托 ir.ParseAnthropicUsage】Claude 口径已在 T2 的 `ir` 包实现
// （含 `cache_creation.ephemeral_5m_input_tokens` / `ephemeral_1h_input_tokens`
// 的拆分回填、Total=prompt+completion、Raw 原键保留）。usage 层再写一份会
// 与适配器漂移——缓存写少算的后果是真实少计费，故这里只做「是否存在」
// 判定并原样复用 ir 的结果。
//
// 返回 nil 表示上游没有可采信的 Claude 事实（空对象/非法形状），让三源
// 优先级的后续来源继续参与；Source=UsageEstimated 是 ir 的「无事实」哨兵。
func parseClaudeUsage(raw json.RawMessage) *contract.Usage {
	parsed := ir.ParseAnthropicUsage(raw)
	if parsed.Source == contract.UsageEstimated {
		return nil
	}
	return &parsed
}

// geminiModalityDetail 是 Gemini `*TokensDetails[]` 的元素。
type geminiModalityDetail struct {
	Modality   string `json:"modality"`
	TokenCount int64  `json:"tokenCount"`
}

// parseGeminiUsage 解析 Gemini usageMetadata（relay-03 §6.2 row 7）。
//
// 关键口径（与 OpenAI 相反，错一处就会重复/遗漏计费）：
//   - Prompt = promptTokenCount + toolUsePromptTokenCount；
//   - Completion = candidatesTokenCount + thoughtsTokenCount；
//   - CacheRead = cachedContentTokenCount（独立维度，PromptIncludesCache=false）；
//   - Reasoning = thoughtsTokenCount，且 ReasoningIncludedInCompletion=true
//     （thoughts 已计入上面的 Completion）。
//   - Raw 保留 Gemini 原键，**不得**改写成 OpenAI/Claude 键——否则 M3 的
//     usage_semantic 会把 Gemini 误判为 anthropic（虚假审计值）。
//
// 子类明细来自 promptTokensDetails / cacheTokensDetails / candidatesTokensDetails：
// 图像/音频 token 是 prompt/completion 的**子集**，只做拆分不改总量。
//
// 【F10 登记】`CompletionDetail.Image` 与 `PromptDetail.CacheReadImage` 是
// 本入口比 ir.ParseGeminiUsage 更细的只读拆分（后者目前只带 Audio/Text）。
// 二者不进入总量、只服务表达式子类变量；若后续把 Gemini 解析统一到 ir，
// 需先在 ir 侧补这两个字段再删除本实现，避免静默丢失明细。
func parseGeminiUsage(raw json.RawMessage) *contract.Usage {
	var payload struct {
		PromptTokenCount        *int64 `json:"promptTokenCount"`
		ToolUsePromptTokenCount *int64 `json:"toolUsePromptTokenCount"`
		CandidatesTokenCount    *int64 `json:"candidatesTokenCount"`
		TotalTokenCount         *int64 `json:"totalTokenCount"`
		ThoughtsTokenCount      *int64 `json:"thoughtsTokenCount"`
		CachedContentTokenCount *int64 `json:"cachedContentTokenCount"`

		PromptTokensDetails     []geminiModalityDetail `json:"promptTokensDetails"`
		CacheTokensDetails      []geminiModalityDetail `json:"cacheTokensDetails"`
		CandidatesTokensDetails []geminiModalityDetail `json:"candidatesTokensDetails"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil
	}
	if payload.PromptTokenCount == nil && payload.CandidatesTokenCount == nil &&
		payload.TotalTokenCount == nil && payload.CachedContentTokenCount == nil &&
		payload.ThoughtsTokenCount == nil && payload.ToolUsePromptTokenCount == nil {
		return nil
	}

	prompt := nonNeg(derefInt64(payload.PromptTokenCount)) + nonNeg(derefInt64(payload.ToolUsePromptTokenCount))
	completion := nonNeg(derefInt64(payload.CandidatesTokenCount)) + nonNeg(derefInt64(payload.ThoughtsTokenCount))
	cacheRead := nonNeg(derefInt64(payload.CachedContentTokenCount))
	reasoning := nonNeg(derefInt64(payload.ThoughtsTokenCount))
	total := nonNeg(derefInt64(payload.TotalTokenCount))
	if total == 0 {
		// totalTokenCount 缺席时派生；它是日志/对账维度，不进入计费数学。
		total = prompt + completion
	}

	promptImage, promptAudio := modalityTokens(payload.PromptTokensDetails)
	completionImage, completionAudio := modalityTokens(payload.CandidatesTokensDetails)
	cachedImage, _ := modalityTokens(payload.CacheTokensDetails)

	var rawMap map[string]any
	_ = json.Unmarshal(raw, &rawMap)

	return &contract.Usage{
		Prompt:                        prompt,
		Completion:                    completion,
		Total:                         total,
		PromptIncludesCache:           false,
		ReasoningIncludedInCompletion: true,
		CacheRead:                     cacheRead,
		Reasoning:                     reasoning,
		PromptDetail: contract.UsageDetail{
			Image:          promptImage,
			Audio:          promptAudio,
			Text:           nonNeg(prompt - promptImage - promptAudio),
			CacheRead:      cacheRead,
			CacheReadImage: cachedImage,
		},
		CompletionDetail: contract.UsageDetail{
			Image: completionImage,
			Audio: completionAudio,
			Text:  nonNeg(completion - completionAudio),
		},
		Source: contract.UsageUpstreamExact,
		Raw:    rawMap,
	}
}

// modalityTokens 汇总某组 Gemini 明细中的 IMAGE / AUDIO token。
// modality 大小写不敏感（上游可能返回 IMAGE / image）。
func modalityTokens(details []geminiModalityDetail) (image, audio int64) {
	for _, detail := range details {
		count := nonNeg(detail.TokenCount)
		switch strings.ToUpper(strings.TrimSpace(detail.Modality)) {
		case "IMAGE":
			image += count
		case "AUDIO":
			audio += count
		}
	}
	return image, audio
}

// parseOpenAIUsageObject 解析 OpenAI Chat/Completions 形状的 usage。
//
// 复用 ir.ParseOpenAIUsage 保证与非流式整包、流式末块走同一套字段路径
// （relay-03 §6.2 row 1），并补多模态端点特有的口径：
//   - 上游只带图像/音频明细时，ir 已把它们填进 PromptDetail/CompletionDetail；
//   - totalTokens 不在此处当 prompt（见 allowTotalFallback）。
//
// allowTotalFallback 只在「对象没有任何 prompt/completion/input/output 键」
// 时为 true（重排/嵌入的 `{"total_tokens":N}` 形状，relay-04 §8.3）。
// 【F2】以前无条件把 total 当 prompt，会把 Responses/Claude 形状中真实存在的
// 数字覆盖成 total，属于凭空造数；现在由调用方按键集合显式授权。
//
// 没有任何可识别 token 键时返回 nil：不生成「全零 upstream_exact」把
// 注入/响应头来源挡掉（F3）。
func parseOpenAIUsageObject(raw json.RawMessage, allowTotalFallback bool) *contract.Usage {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil
	}
	if !hasAnyKey(object, "prompt_tokens", "completion_tokens", "total_tokens") {
		return nil
	}
	parsed := ir.ParseOpenAIUsage(raw)
	if parsed.Source == contract.UsageEstimated {
		return nil
	}
	if allowTotalFallback && parsed.Prompt == 0 && parsed.Completion == 0 && parsed.Total > 0 {
		parsed.Prompt = parsed.Total
		parsed.PromptDetail.Text = parsed.Total
	}
	return &parsed
}

// parseResponsesUsage 解析 OpenAI Responses 风格 usage（relay-03 §6.2 row 2）。
//
// Responses 的键与 Chat 不同：input_tokens / output_tokens / total_tokens，
// 明细在 input_tokens_details.cached_tokens / cache_write_tokens 与
// output_tokens_details.reasoning_tokens。ir.ParseOpenAIUsage 只认 Chat 的
// prompt_tokens 系列，直接喂给它会在 input/output 形状上产出全零（F2/F3），
// 因此在 usage 层按 row 2 的字段路径补齐。
//
// 关系布尔按 GPT 格式：input_tokens 已含缓存读（PromptIncludesCache=true），
// output_tokens 已含 reasoning（ReasoningIncludedInCompletion=true）。
func parseResponsesUsage(raw json.RawMessage) *contract.Usage {
	var payload struct {
		InputTokens  *int64 `json:"input_tokens"`
		OutputTokens *int64 `json:"output_tokens"`
		TotalTokens  *int64 `json:"total_tokens"`
		InputDetails *struct {
			CachedTokens     int64 `json:"cached_tokens"`
			CacheWriteTokens int64 `json:"cache_write_tokens"`
			ImageTokens      int64 `json:"image_tokens"`
			AudioTokens      int64 `json:"audio_tokens"`
		} `json:"input_tokens_details"`
		OutputDetails *struct {
			ReasoningTokens int64 `json:"reasoning_tokens"`
			AudioTokens     int64 `json:"audio_tokens"`
		} `json:"output_tokens_details"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil
	}
	if payload.InputTokens == nil && payload.OutputTokens == nil && payload.TotalTokens == nil {
		return nil
	}

	prompt := nonNeg(derefInt64(payload.InputTokens))
	completion := nonNeg(derefInt64(payload.OutputTokens))
	total := nonNeg(derefInt64(payload.TotalTokens))
	if total == 0 && prompt+completion > 0 {
		total = prompt + completion
	}

	var cacheRead, cacheWrite, promptImage, promptAudio int64
	if details := payload.InputDetails; details != nil {
		cacheRead = nonNeg(details.CachedTokens)
		cacheWrite = nonNeg(details.CacheWriteTokens)
		promptImage = nonNeg(details.ImageTokens)
		promptAudio = nonNeg(details.AudioTokens)
	}
	var reasoning, completionAudio int64
	if details := payload.OutputDetails; details != nil {
		reasoning = nonNeg(details.ReasoningTokens)
		completionAudio = nonNeg(details.AudioTokens)
	}

	var rawMap map[string]any
	_ = json.Unmarshal(raw, &rawMap)

	return &contract.Usage{
		Prompt:                        prompt,
		Completion:                    completion,
		Total:                         total,
		PromptIncludesCache:           true,
		ReasoningIncludedInCompletion: true,
		CacheRead:                     cacheRead,
		CacheWrite:                    cacheWrite,
		Reasoning:                     reasoning,
		PromptDetail: contract.UsageDetail{
			Image:      promptImage,
			Audio:      promptAudio,
			Text:       nonNeg(prompt - promptImage - promptAudio),
			CacheRead:  cacheRead,
			CacheWrite: cacheWrite,
		},
		CompletionDetail: contract.UsageDetail{
			Audio: completionAudio,
			Text:  nonNeg(completion - completionAudio),
		},
		Source: contract.UsageUpstreamExact,
		Raw:    rawMap,
	}
}

// parseCohereBilledUnits 解析 Cohere 的 `meta.billed_units`（relay-03 §6.2 row 9）。
//
// Cohere 不返回 prompt/completion 键，而是 billed_units.input_tokens /
// output_tokens；Total 不设（与规格表一致）。这是重排/生成的文本 token 口径。
func parseCohereBilledUnits(raw json.RawMessage) *contract.Usage {
	var meta struct {
		BilledUnits struct {
			InputTokens  *int64 `json:"input_tokens"`
			OutputTokens *int64 `json:"output_tokens"`
		} `json:"billed_units"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil
	}
	if meta.BilledUnits.InputTokens == nil && meta.BilledUnits.OutputTokens == nil {
		return nil
	}
	prompt := nonNeg(derefInt64(meta.BilledUnits.InputTokens))
	completion := nonNeg(derefInt64(meta.BilledUnits.OutputTokens))

	var rawMap map[string]any
	_ = json.Unmarshal(raw, &rawMap)

	return &contract.Usage{
		Prompt:                        prompt,
		Completion:                    completion,
		PromptIncludesCache:           true,
		ReasoningIncludedInCompletion: true,
		PromptDetail:                  contract.UsageDetail{Text: prompt},
		CompletionDetail:              contract.UsageDetail{Text: completion},
		Source:                        contract.UsageUpstreamExact,
		Raw:                           rawMap,
	}
}

// ============================================================================
// 多模态折算（GC-4）
// ============================================================================

const (
	// maxImageN 是单次图片请求的可计费张数上界。
	//
	// relay-04 §8.1.2 要求「超出 MaxImageN」按异常回退（保持预留量）；
	// 128 与契约/会话层的 MaxImageN 同值。该常量在 M2 会话层、图片端点
	// 校验与这里各有一份（跨包冻结值），改动时需三处同步——本任务不改契约，
	// 故用本地只读副本并在此注明来源，避免在计费路径硬编码匿名数字。
	maxImageN = 128

	// maxAudioSeconds 是单条音频响应的时长上界（秒）。
	//
	// relay-04 §8.2.2 规定 duration_seconds 是用户/上游可控量，必须饱和
	// 处理（超过 1 小时按 1 小时计），防止构造超长时长把 token 数放大到
	// 溢出或异常高计费。换算前先在此钳制，再交给 pricing 的数学换算。
	maxAudioSeconds = 3600
)

// applyImageCount 按 relay-04 §8.1 从响应 payload 推导图像张数。
//
// 只认 payload（url / b64_json），**绝不数数组长度或 `data.#`**。四形态：
//   - 对象形：有非空 url 或 b64_json → 1，否则 0；
//   - 数组形：max(含非空 url 的条目数, 含非空 b64_json 的条目数)（拆分形只计一次）；
//   - 无 payload 条目（仅 revised_prompt）→ 0；
//   - 解析异常 → ok=false：Source=reserved_default、ImageCount 保持零值，
//     计费会话据此保持请求预留量（§8.1.2，不凭空造数）。
//
// 只有顶层出现 `data` 键才视为图像响应：对话/嵌入响应没有该键，不会因为
// 本函数被误标成 reserved_default（那会让正常请求退化为「保持预扣」）。
func applyImageCount(fact *contract.UsageFact, resp *contract.Response) {
	if resp == nil || len(bytes.TrimSpace(resp.Raw)) == 0 {
		return
	}
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(resp.Raw, &envelope); err != nil {
		return
	}
	trimmed := bytes.TrimSpace(envelope.Data)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		// 非 OpenAI 图片形态：Gemini 的图片在 candidates[].content.parts[].inlineData。
		// 只有真正出现 inlineData 键才进入图片语义，普通文本对话不会被误标。
		if count, present, ok := geminiInlineImageCount(resp.Raw); present {
			if !ok || count > maxImageN {
				fact.Usage.Source = contract.UsageReservedDefault
				return
			}
			fact.ImageCount = count
		}
		return
	}
	count, ok := countImagePayload(trimmed)
	if !ok || count > maxImageN {
		// 解析异常/越界：保持请求预留量（relay-04 §8.1.2，F4）。
		fact.Usage.Source = contract.UsageReservedDefault
		return
	}
	fact.ImageCount = count
}

// countImagePayload 对 `data` 的 JSON 值按四形态计数。
func countImagePayload(raw json.RawMessage) (int, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return 0, false
	}
	switch trimmed[0] {
	case '{':
		var item struct {
			URL     json.RawMessage `json:"url"`
			B64JSON json.RawMessage `json:"b64_json"`
		}
		if err := json.Unmarshal(trimmed, &item); err != nil {
			return 0, false
		}
		urlPresent, urlValid := imagePayloadField(item.URL)
		b64Present, b64Valid := imagePayloadField(item.B64JSON)
		if !urlValid || !b64Valid {
			return 0, false
		}
		if urlPresent || b64Present {
			return 1, true
		}
		return 0, true
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return 0, false
		}
		urlCount, b64Count := 0, 0
		for _, rawItem := range items {
			var item struct {
				URL     json.RawMessage `json:"url"`
				B64JSON json.RawMessage `json:"b64_json"`
			}
			if err := json.Unmarshal(rawItem, &item); err != nil {
				// 任一条目非法 → 整个计数不可信（§8.1.2 异常回退）。
				return 0, false
			}
			urlPresent, urlValid := imagePayloadField(item.URL)
			b64Present, b64Valid := imagePayloadField(item.B64JSON)
			if !urlValid || !b64Valid {
				return 0, false
			}
			if urlPresent {
				urlCount++
			}
			if b64Present {
				b64Count++
			}
		}
		// 拆分形（同一张图分列 url + b64_json）只计一次：取 max 而非求和。
		return max(urlCount, b64Count), true
	default:
		// null/字符串/数字等非图像形态：不可信。
		return 0, false
	}
}

// geminiInlineImageCount 统计 Gemini 响应里带 payload 的内联图片数量。
//
// Gemini 图片生成把图片放在 `candidates[].content.parts[].inlineData.data`
// （base64；部分兼容层用 snake_case `inline_data`）。这里同样**只按 payload
// 计数**：只有 inlineData 的 data 为非空字符串才算一张；inlineData 键存在但
// data 类型非法 → ok=false（§8.1.2 保持预留量）。present=false 表示响应根本
// 没有 inlineData 键（普通文本对话），不改变图像语义。
func geminiInlineImageCount(raw []byte) (count int, present bool, ok bool) {
	var envelope struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					InlineData *struct {
						Data json.RawMessage `json:"data"`
					} `json:"inlineData"`
					InlineDataSnake *struct {
						Data json.RawMessage `json:"data"`
					} `json:"inline_data"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return 0, false, false
	}
	for _, candidate := range envelope.Candidates {
		for _, part := range candidate.Content.Parts {
			inline := part.InlineData
			if inline == nil {
				inline = part.InlineDataSnake
			}
			if inline == nil {
				continue
			}
			present = true
			hasPayload, valid := imagePayloadField(inline.Data)
			if !valid {
				return 0, true, false
			}
			if hasPayload {
				count++
			}
		}
	}
	return count, present, true
}

// imagePayloadField 判断 url/b64_json 字段是否承载了**非空字符串** payload。
// 返回 (present, valid)：
//   - 键缺失或 null → (false, true)：无 payload 条目，计 0（§8.1.1）；
//   - JSON 字符串 → (非空, true)；
//   - 其它类型（数字/对象/数组）→ (false, false)：payload 与计数不可信，
//     走 §8.1.2 的异常回退，绝不按 0 静默放行。
func imagePayloadField(raw json.RawMessage) (present, valid bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return false, true
	}
	if trimmed[0] != '"' {
		return false, false
	}
	var value string
	if err := json.Unmarshal(trimmed, &value); err != nil {
		return false, false
	}
	return strings.TrimSpace(value) != "", true
}

// applyAudioConversion 把音频时长填成可结算的音频 token。
//
// 两条路径：
//   - 上游明确给出音频 token（prompt/completion details）→ 保留，不覆盖；
//   - 只有测得时长（转写响应的 `duration`，或调用方注入的 AudioSeconds）→
//     经 pricing.AudioTokensFromSeconds 换算（1 分钟 = 1000 token，唯一真源）。
//
// 输入/输出方向：二进制直传响应（TTS）是输出音频 → CompletionDetail.Audio；
// 其余（转写/翻译 JSON）是输入音频 → PromptDetail.Audio。AudioSeconds 本身
// 是测量字段，始终保留（CCR-5：中继只报量，不取整）。
func applyAudioConversion(usage *contract.Usage, resp *contract.Response) {
	if duration, ok := responseAudioDuration(resp); ok && usage.AudioSeconds == 0 {
		usage.AudioSeconds = pricing.AudioSecondsFromDuration(duration)
	}
	// relay-04 §8.2.2：duration_seconds 是用户/上游可控量，必须在换算前饱和
	// 到 1 小时。超过上界按上界计（不是丢弃），并保留测量字段语义。
	if usage.AudioSeconds > maxAudioSeconds {
		usage.AudioSeconds = maxAudioSeconds
	}
	if usage.AudioSeconds <= 0 {
		return
	}
	tokens := pricing.AudioTokensFromSeconds(float64(usage.AudioSeconds))
	if tokens <= 0 {
		return
	}
	if isBinaryResponse(resp) {
		if usage.CompletionDetail.Audio == 0 {
			usage.CompletionDetail.Audio = tokens
		}
		return
	}
	if usage.PromptDetail.Audio == 0 {
		usage.PromptDetail.Audio = tokens
	}
}

// responseAudioDuration 读取转写/翻译响应顶层的 `duration`（秒）。
// 也接受 `duration_seconds`（部分兼容网关的别名）。
func responseAudioDuration(resp *contract.Response) (float64, bool) {
	if resp == nil || len(bytes.TrimSpace(resp.Raw)) == 0 {
		return 0, false
	}
	var payload struct {
		Duration        *float64 `json:"duration"`
		DurationSeconds *float64 `json:"duration_seconds"`
	}
	if err := json.Unmarshal(resp.Raw, &payload); err != nil {
		return 0, false
	}
	value := payload.Duration
	if value == nil {
		value = payload.DurationSeconds
	}
	if value == nil || math.IsNaN(*value) || math.IsInf(*value, 0) || *value <= 0 {
		return 0, false
	}
	return *value, true
}

// isBinaryResponse 判断响应是否为二进制直传载体（TTS 等）。
func isBinaryResponse(resp *contract.Response) bool {
	return resp != nil && (len(resp.Binary) > 0 || resp.BinaryStream != nil)
}

func derefInt64(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

// ============================================================================
// 二进制流终结器（Review Focus 3 / R-85）
// ============================================================================

const (
	// streamCloseGuardRetention 是一次性终结器守卫的保留时间。
	//
	// 【为什么需要保留】客户端断开与写失败可能由两个 goroutine 同时触发
	// 关闭路径；守卫必须在**整个关闭周期**内可见，才能保证只执行一次。
	// 但请求结束后继续持有 context 会让每个请求的上下文树无法回收，故只保留
	// 很短时间并在后续调用中顺手清扫（有界内存：约等于该窗口内的二进制流数）。
	streamCloseGuardRetention = 30 * time.Second
	// streamCloseGuardSweepEvery 控制清扫频率，避免每次都遍历整个表。
	streamCloseGuardSweepEvery = 256
)

// streamCloseGuard 把 terminator 收敛为「每个上下文只执行一次」。
type streamCloseGuard struct {
	once   sync.Once
	err    error
	doneAt atomic.Int64 // UnixNano；0 表示尚未执行
}

var (
	// streamCloseGuards 以 ctx.Done() 通道为键：通道是每个请求唯一的关闭信号，
	// 且不持有整个 context 值树（只持有通道本身），显著降低保留成本。
	streamCloseGuards sync.Map // <-chan struct{} -> *streamCloseGuard
	streamCloseCalls  atomic.Uint64
)

// OnStreamClose 执行二进制流（图片/音频直传）的终结器，保证每个流生命周期
// **只执行一次**，并把终结错误上抛。
//
// 使用方（Task 4）按 R-85 语义传入 `Refund` 或 `Terminate(fact)`：
//   - 客户端断开 / 写入失败 → Refund（整笔不收费）；
//   - 正常结束 → Terminate（结算实际用量）。
//
// 幂等性由 ctx.Done() 绑定的一次性守卫提供：同一 ctx 上的并发/重复调用
// （例如写失败 goroutine 与连接关闭回调同时触发）只会执行一次终结器，
// 后续调用返回首次执行的错误。ctx 为 nil 或无可区分的生命周期
// （Background/TODO）时直接执行一次——这类全局上下文不能当作共享键，
// 否则不同流的终结器会互相吞掉。
//
// 【F8 登记 · 条件性 once】「只执行一次」的保证范围是**同一个 ctx 的关闭
// 周期**（守卫保留窗口内，见 streamCloseGuardRetention）；窗口外再次调用
// 会重新执行终结器。这是有意的取舍：无限保留 ctx 会让每个请求的上下文树
// 无法回收；而 Refund/Terminate 在契约里本身幂等，窗口外重入不会重复扣费。
func OnStreamClose(ctx context.Context, terminator func() error) error {
	if terminator == nil {
		return nil
	}
	if ctx == nil {
		return terminator()
	}
	done := ctx.Done()
	if done == nil {
		return terminator()
	}

	entry, _ := streamCloseGuards.LoadOrStore(done, &streamCloseGuard{})
	guard := entry.(*streamCloseGuard)
	guard.once.Do(func() {
		guard.err = terminator()
		guard.doneAt.Store(time.Now().UnixNano())
	})

	if streamCloseCalls.Add(1)%streamCloseGuardSweepEvery == 0 {
		sweepStreamCloseGuards(time.Now())
	}
	return guard.err
}

// sweepStreamCloseGuards 清理已完成且超过保留期的守卫，防止表无限增长。
// 只删除「已执行过」的条目；未执行的守卫即使上下文已结束也必须保留到
// 第一次调用（否则重复调用会重新执行终结器）。
func sweepStreamCloseGuards(now time.Time) {
	cutoff := now.Add(-streamCloseGuardRetention).UnixNano()
	streamCloseGuards.Range(func(key, value any) bool {
		guard := value.(*streamCloseGuard)
		if doneAt := guard.doneAt.Load(); doneAt != 0 && doneAt < cutoff {
			streamCloseGuards.Delete(key)
		}
		return true
	})
}
