// Package channelhealth 的上游模型目录客户端。
//
// 只覆盖 NEWTG 已实现的三类协议适配器（OpenAI 兼容、Anthropic 原生、
// Gemini 原生）。其余渠道类型没有可用的中转适配器，去拉模型列表也没有
// 意义，因此显式返回 ErrModelListUnsupported，绝不猜测路径。
package channelhealth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/relay/adapters"
)

// ErrModelListUnsupported 表示该渠道类型没有可用的模型目录协议。
var ErrModelListUnsupported = errors.New("channel models: unsupported channel type")

// ErrModelListConfig 表示缺少拉取模型所需的基础配置（如 base_url/key）。
var ErrModelListConfig = errors.New("channel models: invalid configuration")

// ModelListRequest 是拉取上游模型列表所需的最小配置。
type ModelListRequest struct {
	ChannelType contract.ChannelType
	BaseURL     string
	APIKey      string
	APIVersion  string
}

// ModelInfo 是上游模型目录里的一条模型事实。所有字段只来自上游声明，
// 不做推断：上游没给的保持零值（ReleasedAt 为空串、指针为 nil），由调用方
// 决定如何展示与合并。
type ModelInfo struct {
	ID            string
	DisplayName   string
	Description   string
	ContextLength *int64
	MaxOutput     *int64
	ReleasedAt    string   // "YYYY-MM-DD"；上游没给就是 ""
	Endpoints     []string // 公开端点枚举：chat.completions / messages / embeddings / ... / generateContent
	Source        string   // "openai_models" | "anthropic_models" | "gemini_models"
}

// 目录来源，与上游协议一一对应。
const (
	modelSourceOpenAI    = "openai_models"
	modelSourceAnthropic = "anthropic_models"
	modelSourceGemini    = "gemini_models"
)

// 协议默认公开端点。OpenAI 兼容目录不区分模型用途，统一按对话补全处理；
// 更细的端点拆分留到后续任务。
const (
	endpointChatCompletions = "chat.completions"
	endpointMessages        = "messages"
	endpointGenerateContent = "generateContent"
)

// Gemini 的流式生成方法。公开端点枚举里只有 generateContent，流式由同一
// 端点的 SSE 承载，因此映射到 generateContent。
const geminiStreamMethod = "streamGenerateContent"

// ModelLister 调用上游的模型目录接口。HTTPS 与 HTTP 都允许（渠道 base_url
// 本身支持自建网关），但沿用测试探针的“不跟随重定向”策略，避免凭据被
// 重定向到其他主机。
type ModelLister struct {
	client *http.Client
}

func NewModelLister() *ModelLister {
	return &ModelLister{
		client: &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

const (
	modelListMaxBody  = 4 << 20
	modelListMaxItems = 5000
	modelListMaxPages = 10
)

// List 返回上游声明的模型 ID（已去重、保持上游顺序）。
func (l *ModelLister) List(ctx context.Context, req ModelListRequest) ([]string, error) {
	infos, err := l.ListDetailed(ctx, req)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(infos))
	for _, info := range infos {
		ids = append(ids, info.ID)
	}
	return ids, nil
}

// ListDetailed 返回上游声明的模型及其可信元数据（已按 ID 去重、保持上游
// 顺序）。
func (l *ModelLister) ListDetailed(ctx context.Context, req ModelListRequest) ([]ModelInfo, error) {
	key := strings.TrimSpace(req.APIKey)
	if key == "" {
		return nil, fmt.Errorf("%w: api key is required", ErrModelListConfig)
	}

	table := adapters.DispatchTable()
	entry, ok := table[req.ChannelType]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrModelListUnsupported, req.ChannelType)
	}

	switch adapters.AdapterKey(entry.AdapterKey) {
	case "openai":
		base, err := resolveBaseURL(req)
		if err != nil {
			return nil, err
		}
		if req.ChannelType == contract.ChannelTypeAzure {
			return l.listAzure(ctx, req, base, key)
		}
		return l.listOpenAICompatible(ctx, req, base, key)
	case "anthropic_native":
		base, err := resolveBaseURL(req)
		if err != nil {
			return nil, err
		}
		return l.listAnthropic(ctx, req, base, key)
	case "gemini_native":
		base, err := resolveBaseURL(req)
		if err != nil {
			return nil, err
		}
		return l.listGemini(ctx, base, key)
	default:
		return nil, fmt.Errorf("%w: %s (adapter %s)", ErrModelListUnsupported, req.ChannelType, entry.AdapterKey)
	}
}

// resolveBaseURL 使用请求里的 base_url；为空时按类型取协议默认值。
// 需要必填 base_url 的类型（Azure 等）保持失败，不回退到公共地址。
func resolveBaseURL(req ModelListRequest) (string, error) {
	base := strings.TrimRight(strings.TrimSpace(req.BaseURL), "/")
	if base != "" {
		parsed, err := url.Parse(base)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return "", fmt.Errorf("%w: base_url must be a valid http/https URL", ErrModelListConfig)
		}
		return base, nil
	}

	if def, ok := defaultBaseURLs[req.ChannelType]; ok && def != "" {
		return def, nil
	}
	return "", fmt.Errorf("%w: base_url is required for %s", ErrModelListConfig, req.ChannelType)
}

// defaultBaseURLs 与 relay 适配器的默认基址保持一致（relay-02 §5）。
var defaultBaseURLs = map[contract.ChannelType]string{
	contract.ChannelTypeOpenAI:      "https://api.openai.com",
	contract.ChannelTypeOpenAIMax:   "https://api.openaimax.com",
	contract.ChannelTypeOhMyGPT:     "https://api.ohmygpt.com",
	contract.ChannelTypeAILS:        "https://api.caipacity.com",
	contract.ChannelTypeAIProxy:     "https://api.aiproxy.io",
	contract.ChannelTypeAPI2GPT:     "https://api.api2gpt.com",
	contract.ChannelTypeAIGC2D:      "https://api.aigc2d.com",
	contract.ChannelTypeAli:         "https://dashscope.aliyuncs.com",
	contract.ChannelAi360:           "https://api.360.cn",
	contract.ChannelTypeOpenRouter:  "https://openrouter.ai/api",
	contract.ChannelTypeFastGPT:     "https://fastgpt.run/api/openapi",
	contract.ChannelTypeZhipuV4:     "https://open.bigmodel.cn",
	contract.ChannelTypePerplexity:  "https://api.perplexity.ai",
	contract.ChannelTypeLingYiWanWu: "https://api.lingyiwanwu.com",
	contract.ChannelTypeSiliconFlow: "https://api.siliconflow.cn",
	contract.ChannelTypeMistral:     "https://api.mistral.ai",
	contract.ChannelTypeDeepSeek:    "https://api.deepseek.com",
	contract.ChannelTypeBaiduV2:     "https://qianfan.baidubce.com",
	contract.ChannelTypeXai:         "https://api.x.ai",
	contract.ChannelTypeSubmodel:    "https://llm.submodel.ai",
	contract.ChannelTypeAnthropic:   "https://api.anthropic.com",
	contract.ChannelTypeMoonshot:    "https://api.moonshot.cn",
	contract.ChannelTypeGemini:      "https://generativelanguage.googleapis.com",
}

func (l *ModelLister) listAzure(
	ctx context.Context,
	req ModelListRequest,
	base, key string,
) ([]ModelInfo, error) {
	if strings.TrimSpace(req.APIVersion) == "" {
		return nil, fmt.Errorf("%w: api_version is required for azure", ErrModelListConfig)
	}
	path := "/openai/models?api-version=" + url.QueryEscape(strings.TrimSpace(req.APIVersion))
	headers := http.Header{}
	headers.Set("api-key", key)
	body, err := l.get(ctx, base+path, headers)
	if err != nil {
		return nil, err
	}
	return parseOpenAIModels(body)
}

func (l *ModelLister) listOpenAICompatible(
	ctx context.Context,
	req ModelListRequest,
	base, key string,
) ([]ModelInfo, error) {
	if req.ChannelType == contract.ChannelTypeCustom {
		return nil, fmt.Errorf("%w: custom base URLs cannot be used to discover models", ErrModelListUnsupported)
	}

	path := "/v1/models"
	switch req.ChannelType {
	case contract.ChannelTypeAli:
		path = "/compatible-mode/v1/models"
	case contract.ChannelTypeZhipuV4:
		path = "/api/paas/v4/models"
	case contract.ChannelTypeVolcEngine:
		path = "/api/v3/models"
	case contract.ChannelTypeBaiduV2:
		path = "/v2/models"
	case contract.ChannelTypePerplexity:
		path = "/models"
	}

	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+key)
	headers.Set("Accept", "application/json")
	requestURL, err := joinModelPath(base, path, nil)
	if err != nil {
		return nil, err
	}
	body, err := l.get(ctx, requestURL, headers)
	if err != nil {
		return nil, err
	}
	return parseOpenAIModels(body)
}

// joinModelPath 拼接 base_url 与模型目录路径，沿用 relay 适配器的尾部 /v1
// 去重规则（adapters.joinURL / relay-02 §7.2）：base 以 /v1 结尾时不再重复
// path 的 /v1。这样 `https://host` 与 `https://host/v1` 两种写法都能取到模型，
// 与转发行为保持一致。
func joinModelPath(base, path string, query url.Values) (string, error) {
	parsed, err := url.Parse(base)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("%w: base_url must be a valid http/https URL", ErrModelListConfig)
	}
	basePath := strings.TrimRight(parsed.Path, "/")
	if strings.HasSuffix(basePath, "/v1") && strings.HasPrefix(path, "/v1/") {
		path = strings.TrimPrefix(path, "/v1")
	}
	parsed.Path = basePath + path
	if query != nil {
		parsed.RawQuery = query.Encode()
	}
	return parsed.String(), nil
}

func (l *ModelLister) listAnthropic(
	ctx context.Context,
	req ModelListRequest,
	base, key string,
) ([]ModelInfo, error) {
	prefix := ""
	if req.ChannelType == contract.ChannelTypeMoonshot && !strings.HasSuffix(base, "/anthropic") {
		prefix = "/anthropic"
	}

	headers := http.Header{}
	headers.Set("x-api-key", key)
	headers.Set("anthropic-version", "2023-06-01")
	headers.Set("Accept", "application/json")

	infos := make([]ModelInfo, 0, 64)
	afterID := ""
	for page := 0; page < modelListMaxPages; page++ {
		query := url.Values{}
		query.Set("limit", "1000")
		if afterID != "" {
			query.Set("after_id", afterID)
		}
		requestURL, err := joinModelPath(base, prefix+"/v1/models", query)
		if err != nil {
			return nil, err
		}
		body, err := l.get(ctx, requestURL, headers)
		if err != nil {
			return nil, err
		}
		payload, err := parseAnthropicModels(body)
		if err != nil {
			return nil, err
		}
		infos = append(infos, anthropicModelInfos(payload)...)
		if !payload.HasMore || payload.LastID == "" {
			break
		}
		afterID = payload.LastID
	}
	return normalizeModelInfos(infos), nil
}

// anthropicModelsPage 是 Anthropic /v1/models 的一页响应。
type anthropicModelsPage struct {
	Data []struct {
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
		CreatedAt   string `json:"created_at"`
	} `json:"data"`
	HasMore bool   `json:"has_more"`
	LastID  string `json:"last_id"`
}

func parseAnthropicModels(body []byte) (anthropicModelsPage, error) {
	var page anthropicModelsPage
	if err := json.Unmarshal(body, &page); err != nil {
		return anthropicModelsPage{}, fmt.Errorf("channel models: invalid anthropic response: %w", err)
	}
	return page, nil
}

// anthropicModelInfos 把一页 Anthropic 目录映射成 ModelInfo。Anthropic 原生
// 模型只走 messages 端点；上游没给的字段保持零值。
func anthropicModelInfos(page anthropicModelsPage) []ModelInfo {
	infos := make([]ModelInfo, 0, len(page.Data))
	for _, item := range page.Data {
		infos = append(infos, ModelInfo{
			ID:          item.ID,
			DisplayName: item.DisplayName,
			ReleasedAt:  dateFromRFC3339(item.CreatedAt),
			Endpoints:   []string{endpointMessages},
			Source:      modelSourceAnthropic,
		})
	}
	return infos
}

func (l *ModelLister) listGemini(
	ctx context.Context,
	base, key string,
) ([]ModelInfo, error) {
	if !strings.HasSuffix(base, "/v1beta") && !strings.HasSuffix(base, "/v1") {
		base += "/v1beta"
	}
	headers := http.Header{}
	headers.Set("x-goog-api-key", key)
	headers.Set("Accept", "application/json")

	infos := make([]ModelInfo, 0, 64)
	pageToken := ""
	for page := 0; page < modelListMaxPages; page++ {
		path := base + "/models?pageSize=1000"
		if pageToken != "" {
			path += "&pageToken=" + url.QueryEscape(pageToken)
		}
		body, err := l.get(ctx, path, headers)
		if err != nil {
			return nil, err
		}
		parsedPage, err := parseGeminiModelsPage(body)
		if err != nil {
			return nil, err
		}
		infos = append(infos, geminiModelInfos(parsedPage.Models)...)
		if parsedPage.NextPageToken == "" {
			break
		}
		pageToken = parsedPage.NextPageToken
	}
	return normalizeModelInfos(infos), nil
}

// geminiModelsPage 是 Gemini /v1beta/models 的一页响应。nextPageToken 只用于
// 分页，不进 ModelInfo。
type geminiModelsPage struct {
	Models        []geminiModel `json:"models"`
	NextPageToken string        `json:"nextPageToken"`
}

type geminiModel struct {
	Name                       string   `json:"name"`
	DisplayName                string   `json:"displayName"`
	Description                string   `json:"description"`
	InputTokenLimit            *int64   `json:"inputTokenLimit"`
	OutputTokenLimit           *int64   `json:"outputTokenLimit"`
	SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
}

func parseGeminiModelsPage(body []byte) (geminiModelsPage, error) {
	var page geminiModelsPage
	if err := json.Unmarshal(body, &page); err != nil {
		return geminiModelsPage{}, fmt.Errorf("channel models: invalid gemini response: %w", err)
	}
	return page, nil
}

// parseGeminiModels 返回该页的模型事实（ID 去掉 models/ 前缀）。
func parseGeminiModels(body []byte) ([]ModelInfo, error) {
	page, err := parseGeminiModelsPage(body)
	if err != nil {
		return nil, err
	}
	return geminiModelInfos(page.Models), nil
}

// geminiModelInfos 只映射上游声明过的字段：token 上限缺省时保持 nil，
// 端点由 supportedGenerationMethods 决定。
func geminiModelInfos(models []geminiModel) []ModelInfo {
	infos := make([]ModelInfo, 0, len(models))
	for _, item := range models {
		infos = append(infos, ModelInfo{
			ID:            strings.TrimPrefix(item.Name, "models/"),
			DisplayName:   item.DisplayName,
			Description:   item.Description,
			ContextLength: item.InputTokenLimit,
			MaxOutput:     item.OutputTokenLimit,
			Endpoints:     geminiEndpoints(item.SupportedGenerationMethods),
			Source:        modelSourceGemini,
		})
	}
	return infos
}

// geminiEndpoints 把 Gemini 的生成方法映射到公开端点枚举。公开枚举里的
// /v1/embeddings 是 OpenAI 形状，Gemini 原生没有该端点，因此 embedContent
// 刻意不映射——只支持 embedContent 的模型端点留空，由管理员填。
func geminiEndpoints(methods []string) []string {
	for _, method := range methods {
		if method == endpointGenerateContent || method == geminiStreamMethod {
			return []string{endpointGenerateContent}
		}
	}
	return []string{}
}

func (l *ModelLister) get(ctx context.Context, requestURL string, headers http.Header) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return nil, fmt.Errorf("channel models: build request: %w", err)
	}
	for name, values := range headers {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	response, err := l.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("channel models: upstream request failed: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("channel models: upstream returned status %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, modelListMaxBody))
	if err != nil {
		return nil, fmt.Errorf("channel models: read upstream response: %w", err)
	}
	return body, nil
}

func parseOpenAIModels(body []byte) ([]ModelInfo, error) {
	var payload struct {
		Data []struct {
			ID      string `json:"id"`
			Created any    `json:"created"`
		} `json:"data"`
		Models []struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Created any    `json:"created"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("channel models: invalid models response: %w", err)
	}
	infos := make([]ModelInfo, 0, len(payload.Data)+len(payload.Models))
	for _, item := range payload.Data {
		infos = append(infos, newOpenAIModelInfo(item.ID, item.Created))
	}
	for _, item := range payload.Models {
		id := item.ID
		if id == "" {
			id = item.Name
		}
		infos = append(infos, newOpenAIModelInfo(id, item.Created))
	}
	normalized := normalizeModelInfos(infos)
	if len(normalized) == 0 {
		return nil, errors.New("channel models: upstream returned no models")
	}
	return normalized, nil
}

// newOpenAIModelInfo 把一条 OpenAI 兼容目录条目转成 ModelInfo。该协议只
// 提供 id 与 created，其余字段留零值。
func newOpenAIModelInfo(id string, created any) ModelInfo {
	return ModelInfo{
		ID:         id,
		ReleasedAt: createdDate(created),
		Endpoints:  []string{endpointChatCompletions},
		Source:     modelSourceOpenAI,
	}
}

// 可信的发布日期区间：2000-01-01T00:00:00Z（含）到 2100-01-01T00:00:00Z
// （不含）。区间外的值多半是毫秒时间戳或垃圾数据，宁可留空也不显示 5 位年份。
var (
	modelDateMin = time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC)
	modelDateMax = time.Date(2100, time.January, 1, 0, 0, 0, 0, time.UTC)
)

func dateFromUnixSeconds(sec int64) string {
	if sec <= 0 {
		return ""
	}
	moment := time.Unix(sec, 0).UTC()
	if moment.Before(modelDateMin) || !moment.Before(modelDateMax) {
		return ""
	}
	return moment.Format("2006-01-02")
}

// createdDate 宽容读取上游的 created：接受 JSON 数字（含小数）、数值字符串
// 以及解码后的整数，其余形态（null、布尔、对象、非数值字符串）一律当作
// 「上游没给」返回空串，不让整份目录解析失败。
func createdDate(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		number, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		if err != nil {
			return ""
		}
		return dateFromUnixFloat(number)
	case json.Number:
		number, err := typed.Float64()
		if err != nil {
			return ""
		}
		return dateFromUnixFloat(number)
	case float64:
		return dateFromUnixFloat(typed)
	case float32:
		return dateFromUnixFloat(float64(typed))
	case int:
		return dateFromUnixSeconds(int64(typed))
	case int32:
		return dateFromUnixSeconds(int64(typed))
	case int64:
		return dateFromUnixSeconds(typed)
	default:
		return ""
	}
}

// dateFromUnixFloat 先把小数截断到秒；先按区间过滤再转 int64，避免超大浮点
// 的整数转换溢出。毫秒时间戳落在区间外，返回空串。
func dateFromUnixFloat(sec float64) string {
	if math.IsNaN(sec) || math.IsInf(sec, 0) {
		return ""
	}
	if sec < float64(modelDateMin.Unix()) || sec >= float64(modelDateMax.Unix()) {
		return ""
	}
	return dateFromUnixSeconds(int64(sec))
}

// dateFromRFC3339 把上游的 RFC3339 时间戳（如 Anthropic 的 created_at）转成
// "YYYY-MM-DD"；解析失败返回空串。
func dateFromRFC3339(value string) string {
	parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(value))
	if err != nil {
		return ""
	}
	return parsed.UTC().Format("2006-01-02")
}

// SuggestVendor 只按协议族推导建议的供应商名：只有 OpenAI / Anthropic /
// Gemini 三种协议有明确对应的供应商。其余渠道（含 OpenAI 兼容的第三方上游）
// 留空，由管理员在向导里手填——绝不凭渠道名或模型 ID 猜供应商。
func SuggestVendor(t contract.ChannelType) string {
	switch t {
	case contract.ChannelTypeOpenAI:
		return "OpenAI"
	case contract.ChannelTypeAnthropic:
		return "Anthropic"
	case contract.ChannelTypeGemini:
		return "Gemini"
	default:
		return ""
	}
}

func normalizeModelInfos(infos []ModelInfo) []ModelInfo {
	out := make([]ModelInfo, 0, len(infos))
	seen := make(map[string]struct{}, len(infos))
	for _, info := range infos {
		info.ID = strings.TrimSpace(info.ID)
		if info.ID == "" {
			continue
		}
		if _, ok := seen[info.ID]; ok {
			continue
		}
		seen[info.ID] = struct{}{}
		out = append(out, info)
		if len(out) >= modelListMaxItems {
			break
		}
	}
	return out
}
