package store

// 公开端点枚举的派生（D1-19）。放在 store 而不是 httpapi：公开定价仓储
// （SQLPublicPricingRepo）也要按同一份枚举出公开模型列表，映射若留在 http
// 层，store → httpapi 就会形成反向依赖。

import (
	"encoding/json"
)

// PublicEndpointMapping 是 D1-19 冻结的「档案端点路径 → 公开端点枚举」。
// 未知路径忽略：公开面只暴露契约里存在的协议族，不把管理员的私有路径
// 透传成 facet 值。
//
// 顺序即公开枚举顺序：facet 与卡片输出都依赖它，契约测试亦按此核对。
var PublicEndpointMapping = []struct {
	Path  string
	Value string
}{
	{"/v1/chat/completions", "chat.completions"},
	{"/v1/messages", "messages"},
	{"/v1/embeddings", "embeddings"},
	{"/v1/images/generations", "images.generations"},
	{"/v1/audio/speech", "audio.speech"},
	{"/v1/audio/transcriptions", "audio.transcriptions"},
	{"/v1/rerank", "rerank"},
	{"/v1beta/models/{model}:generateContent", "generateContent"},
}

// DeriveEndpointTypes 把 endpoints 对象（`{"/path":["METHOD"]}`）映射为
// 去重、按 PublicEndpointMapping 固定顺序排列的枚举。对象存在即算命中：
// 方法属于路由实现细节，公开筛选维度只有协议端点。
//
// 恒返回非 nil：降级契约要求空数组而不是 null。
func DeriveEndpointTypes(raw []byte) []string {
	out := make([]string, 0, len(PublicEndpointMapping))
	if len(raw) == 0 {
		return out
	}
	var paths map[string]json.RawMessage
	if err := json.Unmarshal(raw, &paths); err != nil {
		return out
	}
	for _, entry := range PublicEndpointMapping {
		if _, ok := paths[entry.Path]; ok {
			out = append(out, entry.Value)
		}
	}
	return out
}
