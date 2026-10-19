package multimodal

import (
	"encoding/json"

	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/relay/usage"
)

// ============================================================================
// 嵌入 / 重排端点族（relay-04 §1、§2、§8.3、§8.4、§9）
// ============================================================================

const (
	// MaxEmbeddingBatch 是 input 数组的元素数上界（甲类修正）。
	MaxEmbeddingBatch = 2048
	// MaxEmbeddingInputBytes 是单条输入文本的字节上界。
	MaxEmbeddingInputBytes = 1 << 20
	// MaxEmbeddingDimensions 是 dimensions 的上界（甲类修正）。
	MaxEmbeddingDimensions = 8192

	// MaxRerankDocuments 是 documents 元素数上界（甲类修正）。
	MaxRerankDocuments = 1000
	// MaxRerankDocBytes 是单文档字节上界。
	MaxRerankDocBytes = 1 << 20
	// MaxRerankTopN / MaxRerankChunkPerDoc / MaxRerankOverlapTokens 是
	// 三个用户可控数值的上界（甲类修正：基线全无上界）。
	MaxRerankTopN          = 1000
	MaxRerankChunkPerDoc   = 512
	MaxRerankOverlapTokens = 4096
)

// Embeddings 实现 contract.EmbeddingAdapter。
//
// 上游响应原样解析：data[] 的 index/embedding 投影到契约结构；用量走
// usage.ExtractUsageMultimodal 的三源口径（§8.4：上游 usage 优先）。
// 请求与响应都保留原始体，归一化/透传路径不重编码。
func (a *endpointAdapter) Embeddings(rc *contract.RelayContext, req *contract.Request) (*contract.EmbeddingResponse, contract.Usage, error) {
	resp, body, err := a.exchange(rc, req)
	if err != nil {
		return nil, contract.Usage{}, err
	}
	var payload struct {
		Model string `json:"model"`
		Data  []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, contract.Usage{}, classified(contract.ErrUpstreamServer, "multimodal: invalid embeddings response")
	}
	result := &contract.EmbeddingResponse{Model: payload.Model, Data: make([]contract.EmbeddingDatum, 0, len(payload.Data))}
	for _, item := range payload.Data {
		result.Data = append(result.Data, contract.EmbeddingDatum{Index: item.Index, Vector: item.Embedding})
	}
	fact := usage.ExtractUsageMultimodal(&contract.Response{Raw: body}, resp.Header, nil, a.channelType)
	result.Usage = fact.Usage
	return result, fact.Usage, nil
}

// Rerank 实现 contract.RerankAdapter。
//
// `usage.total_tokens → prompt` 的映射由 T3 的 parseOpenAIUsageObject 统一
// 处理（§8.3）；本方法不重复实现 token 口径。
func (a *endpointAdapter) Rerank(rc *contract.RelayContext, req *contract.Request) (*contract.RerankResponse, contract.Usage, error) {
	resp, body, err := a.exchange(rc, req)
	if err != nil {
		return nil, contract.Usage{}, err
	}
	var payload struct {
		Model   string `json:"model"`
		Results []struct {
			Index          int     `json:"index"`
			RelevanceScore float64 `json:"relevance_score"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, contract.Usage{}, classified(contract.ErrUpstreamServer, "multimodal: invalid rerank response")
	}
	result := &contract.RerankResponse{Model: payload.Model, Results: make([]contract.RerankResult, 0, len(payload.Results))}
	for _, item := range payload.Results {
		result.Results = append(result.Results, contract.RerankResult{Index: item.Index, Score: item.RelevanceScore})
	}
	fact := usage.ExtractUsageMultimodal(&contract.Response{Raw: body}, resp.Header, nil, a.channelType)
	result.Usage = fact.Usage
	return result, fact.Usage, nil
}
