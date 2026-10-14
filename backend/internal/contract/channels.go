package contract

import "slices"

// ============================================================================
// §3.6 渠道类型枚举（v1.1 · 补遗 10/11）
//
// 来源：`02-协议适配器层-设计.md` §5 的 60 行规格表。本文件是该枚举的**唯一出处**
//（`02` 只保留规格表并引用此处）。
//
// **表示法（v1.1 · 补遗 11）**：`ChannelType` 为 **string slug**，不是整型。依据：
//   - `06` §1.3 声明 DB 层 `channels.type` 为 **text**（应用层做 60 类型白名单校验）。
//     应用类型与存储形态一致，以免无谓的整型↔文本转换层。
//   - 新系统「全新开始，不迁移数据」（架构 spec §0）——**没有保留基线数值的理由**。
//   - 数值属于基线实现的表面；净室重写不复用其标识符与编号体系。
//
// 基线的编号（0–63，含 28/29/30/32 空洞）**仅作为稳定排序与追溯线索**保留在注释中，
// 不再参与运行时语义。
//
// ⚠️ 一处转写修正：基线序号 19 的显示名为「360」，**不能作为 Go 标识符**
//（标识符不可数字开头），故常量名用 `ChannelAi360`；其 slug 值仍为 "360"。
// 其余 59 个常量名与 `02` §5 表逐行一致。
// slug 取值以架构 spec §2.1 的措辞为准（如 `azure_openai` 而非 `azure`，前者语义更明确）。
// ============================================================================

// ChannelType 是渠道类型标识（**string slug**，见文件头）。
//
// 零值为 ""，对应 `ChannelTypeUnknown` —— 即「未设置/无效」，不是可用的渠道类型。
type ChannelType string

// 全部具名渠道类型。注释中的「基线序号」仅供追溯，不参与运行时语义。
const (
	ChannelTypeUnknown ChannelType = "" // 哨兵：未设置 / 无效，不参与选择

	ChannelTypeOpenAI         ChannelType = "openai"          // 基线序号 1  —— OpenAI 兼容
	ChannelTypeMidjourney     ChannelType = "midjourney"      // 基线序号 2  —— 任务平台（绘图中继）
	ChannelTypeAzure          ChannelType = "azure_openai"    // 基线序号 3  —— OpenAI 兼容（Azure 变体）
	ChannelTypeOllama         ChannelType = "ollama"          // 基线序号 4  —— Ollama 自有协议
	ChannelTypeMidjourneyPlus ChannelType = "midjourney_plus" // 基线序号 5  —— 任务平台
	ChannelTypeOpenAIMax      ChannelType = "openai_max"      // 基线序号 6  —— 遗留
	ChannelTypeOhMyGPT        ChannelType = "ohmygpt"         // 基线序号 7  —— OpenAI 兼容
	ChannelTypeCustom         ChannelType = "custom"          // 基线序号 8  —— OpenAI 兼容（Deprecated）
	ChannelTypeAILS           ChannelType = "ails"            // 基线序号 9  —— 遗留
	ChannelTypeAIProxy        ChannelType = "aiproxy"         // 基线序号 10 —— 遗留
	ChannelTypePaLM           ChannelType = "palm"            // 基线序号 11 —— Google PaLM 旧协议
	ChannelTypeAPI2GPT        ChannelType = "api2gpt"         // 基线序号 12 —— 遗留
	ChannelTypeAIGC2D         ChannelType = "aigc2d"          // 基线序号 13 —— 遗留
	ChannelTypeAnthropic      ChannelType = "anthropic"       // 基线序号 14 —— Anthropic 原生
	ChannelTypeBaidu          ChannelType = "baidu"           // 基线序号 15 —— 自有协议
	ChannelTypeZhipu          ChannelType = "zhipu"           // 基线序号 16 —— 自有协议
	ChannelTypeAli            ChannelType = "ali"             // 基线序号 17 —— 自有协议（兼任务）
	ChannelTypeXunfei         ChannelType = "xunfei"          // 基线序号 18 —— 自有协议
	ChannelAi360              ChannelType = "360"             // 基线序号 19 —— 自有协议（常量名受标识符限制）
	ChannelTypeOpenRouter     ChannelType = "openrouter"      // 基线序号 20 —— OpenAI 兼容
	ChannelTypeAIProxyLibrary ChannelType = "aiproxy_library" // 基线序号 21 —— 未注册（无适配器）
	ChannelTypeFastGPT        ChannelType = "fastgpt"         // 基线序号 22 —— OpenAI 兼容
	ChannelTypeTencent        ChannelType = "tencent"         // 基线序号 23 —— 自有协议（凭据同字段双协议）
	ChannelTypeGemini         ChannelType = "gemini"          // 基线序号 24 —— Gemini 原生（兼任务）
	ChannelTypeMoonshot       ChannelType = "moonshot"        // 基线序号 25 —— Anthropic 原生
	ChannelTypeZhipuV4        ChannelType = "zhipu_v4"        // 基线序号 26 —— 自有协议（paas/v4）
	ChannelTypePerplexity     ChannelType = "perplexity"      // 基线序号 27 —— OpenAI 兼容
	ChannelTypeLingYiWanWu    ChannelType = "lingyiwanwu"     // 基线序号 31 —— OpenAI 兼容
	ChannelTypeAws            ChannelType = "aws"             // 基线序号 33 —— 自有协议（Bedrock）
	ChannelTypeCohere         ChannelType = "cohere"          // 基线序号 34 —— 自有非 OpenAI 形状
	ChannelTypeMiniMax        ChannelType = "minimax"         // 基线序号 35 —— 自有协议（兼任务）
	ChannelTypeSunoAPI        ChannelType = "sunoapi"         // 基线序号 36 —— 任务平台
	ChannelTypeDify           ChannelType = "dify"            // 基线序号 37 —— 自有协议
	ChannelTypeJina           ChannelType = "jina"            // 基线序号 38 —— 自有协议
	ChannelTypeCloudflare     ChannelType = "cloudflare"      // 基线序号 39 —— 自有协议
	ChannelTypeSiliconFlow    ChannelType = "siliconflow"     // 基线序号 40 —— OpenAI 兼容
	ChannelTypeVertexAi       ChannelType = "vertex_ai"       // 基线序号 41 —— Gemini/Claude 原生（凭据条件切换；兼任务）
	ChannelTypeMistral        ChannelType = "mistral"         // 基线序号 42 —— OpenAI 兼容
	ChannelTypeDeepSeek       ChannelType = "deepseek"        // 基线序号 43 —— OpenAI 兼容
	ChannelTypeMokaAI         ChannelType = "mokaai"          // 基线序号 44 —— OpenAI 兼容
	ChannelTypeVolcEngine     ChannelType = "volcengine"      // 基线序号 45 —— OpenAI 兼容（兼任务）
	ChannelTypeBaiduV2        ChannelType = "baidu_v2"        // 基线序号 46 —— OpenAI 兼容
	ChannelTypeXinference     ChannelType = "xinference"      // 基线序号 47 —— OpenAI 兼容
	ChannelTypeXai            ChannelType = "xai"             // 基线序号 48 —— OpenAI 兼容
	ChannelTypeCoze           ChannelType = "coze"            // 基线序号 49 —— 自有协议
	ChannelTypeKling          ChannelType = "kling"           // 基线序号 50 —— 任务平台
	ChannelTypeJimeng         ChannelType = "jimeng"          // 基线序号 51 —— 任务平台
	ChannelTypeVidu           ChannelType = "vidu"            // 基线序号 52 —— 任务平台
	ChannelTypeSubmodel       ChannelType = "submodel"        // 基线序号 53 —— OpenAI 兼容
	ChannelTypeDoubaoVideo    ChannelType = "doubao_video"    // 基线序号 54 —— 任务平台
	ChannelTypeSora           ChannelType = "sora"            // 基线序号 55 —— 任务平台（兼对话）
	ChannelTypeReplicate      ChannelType = "replicate"       // 基线序号 56 —— 自有协议
	ChannelTypeCodex          ChannelType = "codex"           // 基线序号 57 —— OAuth JSON 凭据
	ChannelTypeAdvancedCustom ChannelType = "advanced_custom" // 基线序号 58 —— 灵活集成
	ChannelTypeSub2API        ChannelType = "sub2api"         // 基线序号 59 —— 网关兼容
	ChannelTypeNewAPI         ChannelType = "newapi"          // 基线序号 60 —— 网关兼容（对端产品名，保持原样）
	ChannelTypeTaskPlugin     ChannelType = "task_plugin"     // 基线序号 61 —— 任务插件协议（禁止回退）
	ChannelTypeVLLM           ChannelType = "vllm"            // 基线序号 62 —— 灵活集成
	ChannelTypeSGLang         ChannelType = "sglang"          // 基线序号 63 —— 灵活集成
)

// ChannelTypes 是全部具名渠道类型（**不含**哨兵 `ChannelTypeUnknown`）的**有序清单**。
//
// 顺序沿用基线的编号顺序 —— 仅作**稳定排序**（选择器渲染、日志分组），
// 不再承载数值语义。白名单校验与选择器数据源均取自此清单。
var ChannelTypes = []ChannelType{
	ChannelTypeOpenAI,
	ChannelTypeMidjourney,
	ChannelTypeAzure,
	ChannelTypeOllama,
	ChannelTypeMidjourneyPlus,
	ChannelTypeOpenAIMax,
	ChannelTypeOhMyGPT,
	ChannelTypeCustom,
	ChannelTypeAILS,
	ChannelTypeAIProxy,
	ChannelTypePaLM,
	ChannelTypeAPI2GPT,
	ChannelTypeAIGC2D,
	ChannelTypeAnthropic,
	ChannelTypeBaidu,
	ChannelTypeZhipu,
	ChannelTypeAli,
	ChannelTypeXunfei,
	ChannelAi360,
	ChannelTypeOpenRouter,
	ChannelTypeAIProxyLibrary,
	ChannelTypeFastGPT,
	ChannelTypeTencent,
	ChannelTypeGemini,
	ChannelTypeMoonshot,
	ChannelTypeZhipuV4,
	ChannelTypePerplexity,
	ChannelTypeLingYiWanWu,
	ChannelTypeAws,
	ChannelTypeCohere,
	ChannelTypeMiniMax,
	ChannelTypeSunoAPI,
	ChannelTypeDify,
	ChannelTypeJina,
	ChannelTypeCloudflare,
	ChannelTypeSiliconFlow,
	ChannelTypeVertexAi,
	ChannelTypeMistral,
	ChannelTypeDeepSeek,
	ChannelTypeMokaAI,
	ChannelTypeVolcEngine,
	ChannelTypeBaiduV2,
	ChannelTypeXinference,
	ChannelTypeXai,
	ChannelTypeCoze,
	ChannelTypeKling,
	ChannelTypeJimeng,
	ChannelTypeVidu,
	ChannelTypeSubmodel,
	ChannelTypeDoubaoVideo,
	ChannelTypeSora,
	ChannelTypeReplicate,
	ChannelTypeCodex,
	ChannelTypeAdvancedCustom,
	ChannelTypeSub2API,
	ChannelTypeNewAPI,
	ChannelTypeTaskPlugin,
	ChannelTypeVLLM,
	ChannelTypeSGLang,
}

// Valid 报告 t 是否是已定义的具名渠道类型之一（哨兵与未知值返回 false）。
//
// 这是「60 类型白名单校验」的实现（`06` §1.3）：DB 为 text 列，
// 应用层在写入与路由前必须经此校验。
func (t ChannelType) Valid() bool {
	return slices.Contains(ChannelTypes, t)
}
