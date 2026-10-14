package contract

// ============================================================================
// §3.6 渠道类型枚举（v1.1 · 补遗 10）
//
// 来源：`02-协议适配器层-设计.md` §5 的 60 行主表。本文件是该枚举的**唯一出处**
//（`02` 只保留规格表并引用此处，不再重复声明类型）。
//
// ⚠️ 一处转写修正：基线序号 19 的显示名为「360」，**不能作为 Go 标识符**
//（标识符不可数字开头），故常量名用 `ChannelAi360`（显示名仍为 "360"）。
//
// 其余 59 个常量名与 `02` §5 表逐行一致（含 26 = `ChannelTypeZhipu_v4`）。
//
// ⚠️ 待裁决（见 99 §5 未闭合项）：`06` §1.3 声明 DB 层 `channels.type` 为 **text**，
//   而本枚举为 **int**（沿用基线数值）。二者不一致 —— 要么 DB 列改整型，
//   要么本枚举改 string slug。**本处按 02 原样保留 int**，不擅自改表示法。
// ============================================================================

// ChannelType 是渠道类型标识。
//
// 数值沿用基线的编号（0–63，其中 28/29/30/32 为空洞）——**仅作追溯对齐**，
// 不作为对外契约；对外与 DB 存储的表示法待裁决（见文件头第 3 条）。
type ChannelType int

const (
	ChannelTypeUnknown        ChannelType = 0  // 哨兵，不选择
	ChannelTypeOpenAI         ChannelType = 1  // OpenAI 兼容
	ChannelTypeMidjourney     ChannelType = 2  // 任务平台（绘图中继）
	ChannelTypeAzure          ChannelType = 3  // OpenAI 兼容（Azure 变体）
	ChannelTypeOllama         ChannelType = 4  // Ollama 自有协议
	ChannelTypeMidjourneyPlus ChannelType = 5  // 任务平台
	ChannelTypeOpenAIMax      ChannelType = 6  // 遗留
	ChannelTypeOhMyGPT        ChannelType = 7  // OpenAI 兼容
	ChannelTypeCustom         ChannelType = 8  // OpenAI 兼容（Deprecated）
	ChannelTypeAILS           ChannelType = 9  // 遗留
	ChannelTypeAIProxy        ChannelType = 10 // 遗留
	ChannelTypePaLM           ChannelType = 11 // Google PaLM 旧协议
	ChannelTypeAPI2GPT        ChannelType = 12 // 遗留
	ChannelTypeAIGC2D         ChannelType = 13 // 遗留
	ChannelTypeAnthropic      ChannelType = 14 // Anthropic 原生
	ChannelTypeBaidu          ChannelType = 15 // 自有协议
	ChannelTypeZhipu          ChannelType = 16 // 自有协议
	ChannelTypeAli            ChannelType = 17 // 自有协议（兼任务）
	ChannelTypeXunfei         ChannelType = 18 // 自有协议
	ChannelAi360              ChannelType = 19 // 自有协议（显示名「360」；不可作 Go 标识符，故改名）
	ChannelTypeOpenRouter     ChannelType = 20 // OpenAI 兼容
	ChannelTypeAIProxyLibrary ChannelType = 21 // 未注册（GetAdaptor 返回 nil）
	ChannelTypeFastGPT        ChannelType = 22 // OpenAI 兼容
	ChannelTypeTencent        ChannelType = 23 // 自有协议（凭据同字段双协议）
	ChannelTypeGemini         ChannelType = 24 // Gemini 原生（兼任务）
	ChannelTypeMoonshot       ChannelType = 25 // Anthropic 原生
	ChannelTypeZhipuV4        ChannelType = 26 // 自有协议（paas/v4；对应 02 表的 ChannelTypeZhipu_v4）
	ChannelTypePerplexity     ChannelType = 27 // OpenAI 兼容
	ChannelTypeLingYiWanWu    ChannelType = 31 // OpenAI 兼容
	ChannelTypeAws            ChannelType = 33 // 自有协议（Bedrock）
	ChannelTypeCohere         ChannelType = 34 // 自有非 OpenAI 形状
	ChannelTypeMiniMax        ChannelType = 35 // 自有协议（兼任务）
	ChannelTypeSunoAPI        ChannelType = 36 // 任务平台
	ChannelTypeDify           ChannelType = 37 // 自有协议
	ChannelTypeJina           ChannelType = 38 // 自有协议
	ChannelTypeCloudflare     ChannelType = 39 // 自有协议
	ChannelTypeSiliconFlow    ChannelType = 40 // OpenAI 兼容
	ChannelTypeVertexAi       ChannelType = 41 // Gemini/Claude 原生（凭据条件切换；兼任务）
	ChannelTypeMistral        ChannelType = 42 // OpenAI 兼容
	ChannelTypeDeepSeek       ChannelType = 43 // OpenAI 兼容
	ChannelTypeMokaAI         ChannelType = 44 // OpenAI 兼容
	ChannelTypeVolcEngine     ChannelType = 45 // OpenAI 兼容（兼任务）
	ChannelTypeBaiduV2        ChannelType = 46 // OpenAI 兼容
	ChannelTypeXinference     ChannelType = 47 // OpenAI 兼容
	ChannelTypeXai            ChannelType = 48 // OpenAI 兼容
	ChannelTypeCoze           ChannelType = 49 // 自有协议
	ChannelTypeKling          ChannelType = 50 // 任务平台
	ChannelTypeJimeng         ChannelType = 51 // 任务平台
	ChannelTypeVidu           ChannelType = 52 // 任务平台
	ChannelTypeSubmodel       ChannelType = 53 // OpenAI 兼容
	ChannelTypeDoubaoVideo    ChannelType = 54 // 任务平台
	ChannelTypeSora           ChannelType = 55 // 任务平台（兼对话）
	ChannelTypeReplicate      ChannelType = 56 // 自有协议
	ChannelTypeCodex          ChannelType = 57 // OAuth JSON 凭据
	ChannelTypeAdvancedCustom ChannelType = 58 // 灵活集成
	ChannelTypeSub2API        ChannelType = 59 // 网关兼容
	ChannelTypeNewAPI         ChannelType = 60 // 网关兼容（对端产品名，按 AGENTS.md 保持原样）
	ChannelTypeTaskPlugin     ChannelType = 61 // 任务插件协议（禁止回退）
	ChannelTypeVLLM           ChannelType = 62 // 灵活集成
	ChannelTypeSGLang         ChannelType = 63 // 灵活集成
)

// channelTypeNames 是 ChannelType → 显示名的映射，供日志、UI 与 60 类型白名单校验使用。
//
// 注：`ChannelTypeUnknown(0)` 与空洞值（28/29/30/32）不在表内 —— 前者是哨兵、
// 后者在基线中即无定义；查表未命中即视为非法值。
var channelTypeNames = map[ChannelType]string{
	ChannelTypeOpenAI:         "openai",
	ChannelTypeMidjourney:     "midjourney",
	ChannelTypeAzure:          "azure",
	ChannelTypeOllama:         "ollama",
	ChannelTypeMidjourneyPlus: "midjourney_plus",
	ChannelTypeOpenAIMax:      "openai_max",
	ChannelTypeOhMyGPT:        "ohmygpt",
	ChannelTypeCustom:         "custom",
	ChannelTypeAILS:           "ails",
	ChannelTypeAIProxy:        "aiproxy",
	ChannelTypePaLM:           "palm",
	ChannelTypeAPI2GPT:        "api2gpt",
	ChannelTypeAIGC2D:         "aigc2d",
	ChannelTypeAnthropic:      "anthropic",
	ChannelTypeBaidu:          "baidu",
	ChannelTypeZhipu:          "zhipu",
	ChannelTypeAli:            "ali",
	ChannelTypeXunfei:         "xunfei",
	ChannelAi360:              "360",
	ChannelTypeOpenRouter:     "openrouter",
	ChannelTypeAIProxyLibrary: "aiproxy_library",
	ChannelTypeFastGPT:        "fastgpt",
	ChannelTypeTencent:        "tencent",
	ChannelTypeGemini:         "gemini",
	ChannelTypeMoonshot:       "moonshot",
	ChannelTypeZhipuV4:        "zhipu_v4",
	ChannelTypePerplexity:     "perplexity",
	ChannelTypeLingYiWanWu:    "lingyiwanwu",
	ChannelTypeAws:            "aws",
	ChannelTypeCohere:         "cohere",
	ChannelTypeMiniMax:        "minimax",
	ChannelTypeSunoAPI:        "sunoapi",
	ChannelTypeDify:           "dify",
	ChannelTypeJina:           "jina",
	ChannelTypeCloudflare:     "cloudflare",
	ChannelTypeSiliconFlow:    "siliconflow",
	ChannelTypeVertexAi:       "vertex_ai",
	ChannelTypeMistral:        "mistral",
	ChannelTypeDeepSeek:       "deepseek",
	ChannelTypeMokaAI:         "mokaai",
	ChannelTypeVolcEngine:     "volcengine",
	ChannelTypeBaiduV2:        "baidu_v2",
	ChannelTypeXinference:     "xinference",
	ChannelTypeXai:            "xai",
	ChannelTypeCoze:           "coze",
	ChannelTypeKling:          "kling",
	ChannelTypeJimeng:         "jimeng",
	ChannelTypeVidu:           "vidu",
	ChannelTypeSubmodel:       "submodel",
	ChannelTypeDoubaoVideo:    "doubao_video",
	ChannelTypeSora:           "sora",
	ChannelTypeReplicate:      "replicate",
	ChannelTypeCodex:          "codex",
	ChannelTypeAdvancedCustom: "advanced_custom",
	ChannelTypeSub2API:        "sub2api",
	ChannelTypeNewAPI:         "newapi",
	ChannelTypeTaskPlugin:     "task_plugin",
	ChannelTypeVLLM:           "vllm",
	ChannelTypeSGLang:         "sglang",
}

// ChannelTypeName 返回渠道类型的显示名。第二个返回值为 false 表示非法值
// （哨兵 0、空洞 28/29/30/32、或超出 0–63 范围）。
func ChannelTypeName(t ChannelType) (string, bool) {
	name, ok := channelTypeNames[t]
	return name, ok
}
