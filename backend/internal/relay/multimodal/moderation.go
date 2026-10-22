package multimodal

import (
	"encoding/json"
	"math"
)

// ============================================================================
// 审核端点族（POST /v1/moderations，relay-04 §5、§9）
//
// 本文件只承载审核族独有的冻结常量与本地估算口径；请求校验在 httpapi
// 入口层完成，分派/计费与嵌入、重排共用同一条 ExecuteAttempts 路径。
// ============================================================================

const (
	// DefaultModerationModel 是缺省审核模型（relay-04 §5.2：「缺省 model 为
	// 审核默认模型」）。
	//
	// 设计只冻结了「有缺省值」这一语义，没有冻结字面量。基线两处记载互相
	// 冲突（modules-基线-06 §1 写 omni-moderation-latest、§5 写
	// text-moderation-latest）；本实现取当前 OpenAI 官方审核模型
	// omni-moderation-latest。回写设计时需要把这个字面量落定。
	DefaultModerationModel = "omni-moderation-latest"

	// MaxModerationInputBytes 是审核 `input` 的字节上界（relay-04 §9 冻结
	// 1 MiB）。`input` 是用户可控量，必须先于预扣拒绝（billing.md 的
	// 「计费乘数一律前置上界」）。
	MaxModerationInputBytes = 1 << 20

	// MaxTokensLimit 是 max_tokens 族的冻结上界（relay-04 §9 = math.MaxInt32/2）。
	// 审核请求若携带 max_tokens/max_completion_tokens/min_tokens/
	// max_output_tokens，按此上界拒绝；它同样属于会被乘进配额的数值。
	MaxTokensLimit = math.MaxInt32 / 2
)

// estimateModerationText 汇总审核请求 `input` 中字符串的字节数。
//
// 只统计 string 与 string[] 两种标准形态：审核的 multimodal input 数组里
// 可能出现 image_url 等二进制/URL 元素，按对象序列化后的字节计入会高估
// token，因此忽略非字符串元素（与 estimateEmbeddingText 同一口径）。
func estimateModerationText(body []byte) int {
	var payload struct {
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return 0
	}
	return sumJSONStrings(payload.Input)
}
