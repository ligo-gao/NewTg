// Package adapters 实现协议适配器层（relay-02）：两级分派表 + 各上游协议的
// `contract.Adapter` 实现。
//
// 两级分派（relay-02 §2.2）：
//  1. 渠道类型（contract.ChannelType）→ 适配器键（DispatchTable，契约 §3.3）；
//  2. 适配器键（AdapterKey）→ 适配器实例（adapterConstructors）。
//
// 设计纪律（relay-02 §2.3）：
//   - 回退与失败全部是声明式数据，不存在「map 未命中即默认 OpenAI」的隐式路径；
//   - `Lookup` 对 59 个具名类型全部返回条目（R-P6），可用性由 `Dispatch` 判定；
//   - 21（aiproxy_library）与 61（task_plugin）显式失败（ForbidFallback），
//     返回包装 `contract.ErrEndpointUnsupported` 的错误，永不返回 nil 适配器。
package adapters

import (
	"fmt"
	"net/http"

	"newtg/gateway/internal/contract"
)

// AdapterKey 是适配器注册键（契约 §3.3，等价基线的「API 类型」）。
// 别名到契约类型，保证两级分派使用的键类型只有一个出处。
type AdapterKey = contract.AdapterKey

// 分派键全集（relay-02 §2.5）。表中共 26 个键；brief 所述「25 个」为笔误，
// §2.5 实际列出 26 行（含 AKAIProxyLibrary）。
//
// 常量保持**无类型字符串**：`DispatchEntry.AdapterKey` 是 string，调用方需要
// 能直接与条目字段比较（`entry.AdapterKey == AKOpenAI`），不因类型转换而失配。
const (
	AKOpenAI          = "openai"           // OpenAI 兼容族（含 Azure/DashScope/paas-v4 派生）
	AKAnthropicNative = "anthropic_native" // Anthropic 原生 /v1/messages
	AKGeminiNative    = "gemini_native"    // Gemini 原生 :generateContent
	AKVertex          = "vertex"           // Vertex AI（Gemini/Claude 原生）
	AKPaLM            = "palm"             // Google PaLM 旧协议
	AKOllama          = "ollama"           // Ollama 自有协议
	AKBaidu           = "baidu"            // 百度千帆 v1
	AKZhipu           = "zhipu"            // 智谱 v3 model-api
	AKXunfei          = "xunfei"           // 讯飞 WebSocket
	AKTencent         = "tencent"          // 腾讯 TC3 + OpenAI 兼容
	AKAws             = "aws"              // AWS Bedrock
	AKCohere          = "cohere"           // Cohere 自有协议
	AKMiniMax         = "minimax"          // MiniMax 自有协议
	AKDify            = "dify"             // Dify 自有协议
	AKJina            = "jina"             // Jina 自有协议（rerank/embeddings）
	AKCloudflare      = "cloudflare"       // Cloudflare Workers AI
	AKMokaAI          = "mokaai"           // MokaAI 自有协议
	AKCoze            = "coze"             // Coze v3/chat
	AKReplicate       = "replicate"        // Replicate predictions
	AKCodex           = "codex"            // ChatGPT Subscription Codex
	AKAdvancedCustom  = "advanced_custom"  // 灵活集成（多协议转换路由表）
	AKSub2API         = "sub2api"          // 网关兼容
	AKNewAPI          = "newapi"           // 网关兼容（独立键）
	AKTask            = "task"             // 任务平台（JS 任务插件）
	AKTaskPlugin      = "task_plugin"      // 任务插件绑定（契约驱动）
	AKAIProxyLibrary  = "aiproxy_library"  // 遗留（无实现，显式失败）
)

// AdapterConstructor 构造一个适配器实例。
//
// 构造器无参数：它返回的是「该渠道类型绑定的适配器」。Azure 变体与 OpenAI
// 共用 AKOpenAI 键，但 `Type()` 必须返回各自渠道类型（契约 §3.1 Type 注释）。
type AdapterConstructor func() contract.Adapter

// DispatchTable 是渠道类型 → 分派入口的声明式数据（契约 §3.3）。
//
// 59 个具名类型逐行写出（R-03 的「59 named + 1 sentinel」中指类型全集；
// 哨兵 "" 不参与选择，因此不入表 —— relay-02 §2.3 允许省略或显式 ForbidFallback）。
var DispatchTable = map[contract.ChannelType]contract.DispatchEntry{
	// 基线序号 1–13
	contract.ChannelTypeOpenAI:         {AdapterKey: string(AKOpenAI)},
	contract.ChannelTypeMidjourney:     {AdapterKey: string(AKTask)},
	contract.ChannelTypeAzure:          {AdapterKey: string(AKOpenAI)}, // Azure 变体，键同为 OpenAI（relay-02 §2.2）
	contract.ChannelTypeOllama:         {AdapterKey: string(AKOllama)},
	contract.ChannelTypeMidjourneyPlus: {AdapterKey: string(AKTask)},
	// 6/9/10/12/13 是遗留 OpenAI 兼容类型：行为照搬基线，但回退必须逐类型显式写出
	// FallbackTo（relay-02 §2.3 关键约束），不得依赖 map 未命中的隐式默认。
	contract.ChannelTypeOpenAIMax: {AdapterKey: string(AKOpenAI), FallbackTo: string(AKOpenAI)},
	contract.ChannelTypeOhMyGPT:   {AdapterKey: string(AKOpenAI)},
	contract.ChannelTypeCustom:    {AdapterKey: string(AKOpenAI)},
	contract.ChannelTypeAILS:      {AdapterKey: string(AKOpenAI), FallbackTo: string(AKOpenAI)},
	contract.ChannelTypeAIProxy:   {AdapterKey: string(AKOpenAI), FallbackTo: string(AKOpenAI)},
	contract.ChannelTypePaLM:      {AdapterKey: string(AKPaLM)},
	contract.ChannelTypeAPI2GPT:   {AdapterKey: string(AKOpenAI), FallbackTo: string(AKOpenAI)},
	contract.ChannelTypeAIGC2D:    {AdapterKey: string(AKOpenAI), FallbackTo: string(AKOpenAI)},
	// 基线序号 14–27
	contract.ChannelTypeAnthropic:  {AdapterKey: string(AKAnthropicNative)},
	contract.ChannelTypeBaidu:      {AdapterKey: string(AKBaidu)},
	contract.ChannelTypeZhipu:      {AdapterKey: string(AKZhipu)},
	contract.ChannelTypeAli:        {AdapterKey: string(AKOpenAI)},
	contract.ChannelTypeXunfei:     {AdapterKey: string(AKXunfei)},
	contract.ChannelAi360:          {AdapterKey: string(AKOpenAI)},
	contract.ChannelTypeOpenRouter: {AdapterKey: string(AKOpenAI)},
	// 21：基线的 GetAdaptor 返回 nil（invalid api type）。新系统保留枚举并显式失败，
	// 不静默回落（U-10 裁决；relay-02 §2.3）。
	contract.ChannelTypeAIProxyLibrary: {AdapterKey: string(AKAIProxyLibrary), ForbidFallback: true},
	contract.ChannelTypeFastGPT:        {AdapterKey: string(AKOpenAI)},
	contract.ChannelTypeTencent:        {AdapterKey: string(AKTencent)},
	contract.ChannelTypeGemini:         {AdapterKey: string(AKGeminiNative)},
	// 25：R-01 裁决 —— moonshot → Anthropic 原生（relay-02 §5 行 25）。
	contract.ChannelTypeMoonshot:   {AdapterKey: string(AKAnthropicNative)},
	contract.ChannelTypeZhipuV4:    {AdapterKey: string(AKOpenAI)},
	contract.ChannelTypePerplexity: {AdapterKey: string(AKOpenAI)},
	// 基线序号 31–43（28/29/30/32 为空洞）
	contract.ChannelTypeLingYiWanWu: {AdapterKey: string(AKOpenAI)},
	contract.ChannelTypeAws:         {AdapterKey: string(AKAws)},
	contract.ChannelTypeCohere:      {AdapterKey: string(AKCohere)},
	contract.ChannelTypeMiniMax:     {AdapterKey: string(AKMiniMax)},
	contract.ChannelTypeSunoAPI:     {AdapterKey: string(AKTask)},
	contract.ChannelTypeDify:        {AdapterKey: string(AKDify)},
	contract.ChannelTypeJina:        {AdapterKey: string(AKJina)},
	contract.ChannelTypeCloudflare:  {AdapterKey: string(AKCloudflare)},
	contract.ChannelTypeSiliconFlow: {AdapterKey: string(AKOpenAI)},
	contract.ChannelTypeVertexAi:    {AdapterKey: string(AKVertex)},
	contract.ChannelTypeMistral:     {AdapterKey: string(AKOpenAI)},
	contract.ChannelTypeDeepSeek:    {AdapterKey: string(AKOpenAI)},
	contract.ChannelTypeMokaAI:      {AdapterKey: string(AKMokaAI)},
	// 基线序号 44–60
	contract.ChannelTypeVolcEngine:  {AdapterKey: string(AKOpenAI)},
	contract.ChannelTypeBaiduV2:     {AdapterKey: string(AKOpenAI)},
	contract.ChannelTypeXinference:  {AdapterKey: string(AKOpenAI)},
	contract.ChannelTypeXai:         {AdapterKey: string(AKOpenAI)},
	contract.ChannelTypeCoze:        {AdapterKey: string(AKCoze)},
	contract.ChannelTypeKling:       {AdapterKey: string(AKTask)},
	contract.ChannelTypeJimeng:      {AdapterKey: string(AKTask)},
	contract.ChannelTypeVidu:        {AdapterKey: string(AKTask)},
	contract.ChannelTypeSubmodel:    {AdapterKey: string(AKOpenAI)},
	contract.ChannelTypeDoubaoVideo: {AdapterKey: string(AKTask)},
	// 55：R-04 裁决 —— Sora 只有任务身份（RoleTask），不走 AKOpenAI。
	contract.ChannelTypeSora:           {AdapterKey: string(AKTask)},
	contract.ChannelTypeReplicate:      {AdapterKey: string(AKReplicate)},
	contract.ChannelTypeCodex:          {AdapterKey: string(AKCodex)},
	contract.ChannelTypeAdvancedCustom: {AdapterKey: string(AKAdvancedCustom)},
	contract.ChannelTypeSub2API:        {AdapterKey: string(AKSub2API)},
	contract.ChannelTypeNewAPI:         {AdapterKey: string(AKNewAPI)},
	// 61：禁止回退 OpenAI（照搬基线；relay-02 §2.3）。
	contract.ChannelTypeTaskPlugin: {AdapterKey: string(AKTaskPlugin), ForbidFallback: true},
	// 基线序号 62–63：advanced custom 预设，统一键。
	contract.ChannelTypeVLLM:   {AdapterKey: string(AKAdvancedCustom)},
	contract.ChannelTypeSGLang: {AdapterKey: string(AKAdvancedCustom)},
}

// adapterConstructors 是「已实现」的适配器键 → 工厂。
//
// 工厂接收请求的渠道类型：AKOpenAI 同时服务 openai 与 azure_openai，
// 但契约要求 `Type()` 返回各自渠道类型（Azure 变体返回 azure_openai）。
// 未出现在本表中的键 = 尚无实现；Dispatch 会按声明回退或显式失败。
var adapterConstructors = map[AdapterKey]func(contract.ChannelType) contract.Adapter{
	AKOpenAI: func(channelType contract.ChannelType) contract.Adapter {
		return &OpenAI{channelType: channelType}
	},
}

// keyRoles 声明每个适配器键的渠道身份（relay-02 §2.4、§2.5 末段）。
//
// 任务平台键只有 RoleTask；其余协议族是对话/多模态身份 RoleChat。
// 类型级双重身份（1/17/24/35/41/45）属于 §3.3 的类型级覆写，
// 不在本任务范围内（各类型适配器实现时补 CapPatch）。
var keyRoles = map[AdapterKey][]contract.ChannelRole{
	AKOpenAI:          {contract.RoleChat},
	AKAnthropicNative: {contract.RoleChat},
	AKGeminiNative:    {contract.RoleChat},
	AKVertex:          {contract.RoleChat},
	AKPaLM:            {contract.RoleChat},
	AKOllama:          {contract.RoleChat},
	AKBaidu:           {contract.RoleChat},
	AKZhipu:           {contract.RoleChat},
	AKXunfei:          {contract.RoleChat},
	AKTencent:         {contract.RoleChat},
	AKAws:             {contract.RoleChat},
	AKCohere:          {contract.RoleChat},
	AKMiniMax:         {contract.RoleChat},
	AKDify:            {contract.RoleChat},
	AKJina:            {contract.RoleChat},
	AKCloudflare:      {contract.RoleChat},
	AKMokaAI:          {contract.RoleChat},
	AKCoze:            {contract.RoleChat},
	AKReplicate:       {contract.RoleChat},
	AKCodex:           {contract.RoleChat},
	AKAdvancedCustom:  {contract.RoleChat},
	AKSub2API:         {contract.RoleChat},
	AKNewAPI:          {contract.RoleChat},
	AKTask:            {contract.RoleTask}, // R-04：纯任务身份
	AKTaskPlugin:      {contract.RoleTask},
	AKAIProxyLibrary:  {contract.RoleChat},
}

// lookupTable 是 Lookup 的数据源：59 个具名类型各有一个构造器。
// 无实现的键返回 declaredAdapter（能力可读、协议方法显式失败）。
var lookupTable = make(map[contract.ChannelType]AdapterConstructor, len(contract.ChannelTypes))

func init() {
	validateDispatchTable(DispatchTable)

	for channelType, entry := range DispatchTable {
		key := AdapterKey(entry.AdapterKey)
		if factory, ok := adapterConstructors[key]; ok {
			lookupTable[channelType] = func() contract.Adapter { return factory(channelType) }
			continue
		}
		// 显式失败条目：Lookup 仍命中（R-P6），但构造出的适配器只声明能力，
		// 任何协议调用都返回 ErrEndpointUnsupported；Dispatch 也不会把它当作可用实现。
		lookupTable[channelType] = func() contract.Adapter { return newDeclaredAdapter(channelType) }
	}
}

// validateDispatchTable 做启动期不变量校验（relay-02 §2.3、AC-B-08 fail-fast）。
// 违反即 panic：分派表是编译期常量数据，坏数据不应进入运行时。
func validateDispatchTable(table contract.DispatchTable) {
	if got, want := len(table), len(contract.ChannelTypes); got != want {
		panic(fmt.Sprintf("adapters: DispatchTable has %d entries, want %d named channel types", got, want))
	}
	if _, ok := table[contract.ChannelTypeUnknown]; ok {
		panic("adapters: sentinel ChannelTypeUnknown must not be dispatched")
	}
	for _, channelType := range contract.ChannelTypes {
		entry, ok := table[channelType]
		if !ok {
			panic(fmt.Sprintf("adapters: channel type %q has no dispatch entry", channelType))
		}
		if entry.AdapterKey == "" {
			panic(fmt.Sprintf("adapters: channel type %q has an empty AdapterKey", channelType))
		}
		if entry.ForbidFallback && entry.FallbackTo != "" {
			panic(fmt.Sprintf("adapters: channel type %q declares both ForbidFallback and FallbackTo", channelType))
		}
		if entry.FallbackTo != "" {
			// ③ FallbackTo 必须指向已注册键，否则是声明矛盾（relay-02 §2.3）。
			if _, ok := adapterConstructors[AdapterKey(entry.FallbackTo)]; !ok {
				panic(fmt.Sprintf("adapters: channel type %q falls back to unregistered key %q", channelType, entry.FallbackTo))
			}
		}
	}
}

// declaredAdapter 只声明能力，不实现协议。用于尚无适配器的键（含 21/61 的
// 显式失败条目）。Dispatch 永远不会把它当作可用实现返回。
type declaredAdapter struct {
	channelType contract.ChannelType
	roles       []contract.ChannelRole
}

func newDeclaredAdapter(channelType contract.ChannelType) *declaredAdapter {
	key := AdapterKey(DispatchTable[channelType].AdapterKey)
	return &declaredAdapter{channelType: channelType, roles: keyRoles[key]}
}

func (a *declaredAdapter) Type() contract.ChannelType { return a.channelType }

func (a *declaredAdapter) Caps() contract.Capabilities {
	return contract.Capabilities{Roles: append([]contract.ChannelRole(nil), a.roles...)}
}

func (a *declaredAdapter) unsupported() error {
	return fmt.Errorf("adapters: channel type %q has no adapter implementation: %w", string(a.channelType), ErrEndpointUnsupported)
}

func (a *declaredAdapter) BuildRequest(*contract.RelayContext, *contract.Request) (*http.Request, error) {
	return nil, a.unsupported()
}

func (a *declaredAdapter) ParseResponse(*contract.RelayContext, *http.Response) (*contract.Response, contract.Usage, error) {
	return nil, contract.Usage{}, a.unsupported()
}

func (a *declaredAdapter) ClassifyError(int, []byte) contract.ErrorClass {
	return contract.ErrEndpointUnsupported
}

var _ contract.Adapter = (*declaredAdapter)(nil)

// Lookup 返回渠道类型对应的适配器构造器（第一级分派数据）。
//
// R-P6：59 个具名类型全部命中，包括 21/61 的显式失败条目；哨兵与未知类型
// 未命中。是否可用由 Dispatch 判定 —— Lookup 只回答「表里有这一行」。
func Lookup(channelType contract.ChannelType) (AdapterConstructor, bool) {
	constructor, ok := lookupTable[channelType]
	return constructor, ok
}

// Dispatch 执行两级解析并返回可用适配器（relay-02 §2.3 的完整语义）：
//
//  1. 类型无条目（哨兵/未知）→ ErrEndpointUnsupported；
//  2. 键已实现 → 返回适配器实例；
//  3. 键未实现且 ForbidFallback → ErrEndpointUnsupported（不重试、不自动禁用）；
//  4. 键未实现、有 FallbackTo → 返回回退键的适配器；
//  5. 键未实现、无回退 → ErrEndpointUnsupported（禁止隐式默认 OpenAI）。
//
// 任何失败路径都返回 nil 适配器（R-P6：never a nil adapter）。
func Dispatch(channelType contract.ChannelType) (contract.Adapter, error) {
	entry, ok := DispatchTable[channelType]
	if !ok {
		return nil, fmt.Errorf("adapters: channel type %q has no dispatch entry: %w", string(channelType), ErrEndpointUnsupported)
	}
	key := AdapterKey(entry.AdapterKey)
	if factory, ok := adapterConstructors[key]; ok {
		return factory(channelType), nil
	}
	if entry.ForbidFallback {
		return nil, fmt.Errorf("adapters: channel type %q is explicitly unsupported (%s): %w",
			string(channelType), key, ErrEndpointUnsupported)
	}
	if entry.FallbackTo != "" {
		if factory, ok := adapterConstructors[AdapterKey(entry.FallbackTo)]; ok {
			return factory(channelType), nil
		}
		// 声明矛盾应在 validateDispatchTable 的启动期校验中被拦截；运行期仍显式失败。
		return nil, fmt.Errorf("adapters: fallback %q for channel type %q is not registered: %w",
			entry.FallbackTo, string(channelType), ErrEndpointUnsupported)
	}
	return nil, fmt.Errorf("adapters: channel type %q adapter %q is not implemented: %w",
		string(channelType), key, ErrEndpointUnsupported)
}
