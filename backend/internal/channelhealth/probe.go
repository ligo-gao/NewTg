package channelhealth

// W7/M4-b：健康体检与手工连通性测试的**共享探测**（R-M4-02/06/14）。
//
// 本文件是 modules-06 F-06-06 探测器从 httpapi 抽出的可复用实现：
//   - 请求构造、凭据解封、适配器分派、模型映射与状态码分类全部与中继/
//     手工测试同一份代码（不新增第二套出站策略）；
//   - 只测量到**响应头**的耗时（R-M4-02 的阈值作用面）；
//   - 无 adapter 的渠道类型返回 Unsupported，由调用方整轮跳过（R-M4-06），
//     绝不写入 last_test_*、绝不判失败；
//   - 错误体按 64 KiB 截断并只用于关键词匹配，绝不出现在日志/错误文案里
//     （GC-6）。

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"time"

	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/relay/adapters"
	"newtg/gateway/internal/relay/ir"
	"newtg/gateway/internal/store"
)

const (
	// ProbeTimeout 是健康 worker 单次探测上限（R-M4-14 冻结 15s）。
	//
	// 为什么不是手工测试的 30s：100 渠道 × 并发 5 × 30s 的最坏单轮约 10 分钟，
	// 与 modules-06 F-06-12 的 5 分钟验收冲突；15s 让最坏单轮落在 5 分钟内。
	ProbeTimeout = 15 * time.Second
	// ProbeMaxErrorBodyBytes 与执行层错误体读取上限一致（relay/route.go）。
	// 关键词匹配与分类只看前 64 KiB（R-M4-12 的有界复杂度）。
	ProbeMaxErrorBodyBytes = 64 << 10
)

// 传输层失败的稳定分类（不携带 *url.Error 的 URL/凭据文本，GC-6）。
const (
	// TransportTimeout：context deadline 或 net.Error.Timeout()。
	TransportTimeout = "timeout"
	// TransportNetwork：连接拒绝/DNS/TLS/取消等其余传输失败。
	TransportNetwork = "network"
)

// CredentialOpener 解开 channels.credentials 的密文（生产 *httpapi.SecretSealer）。
//
// 用接口而不是具体类型：channelhealth 不能反向依赖 httpapi（httpapi 会
// 依赖本包做探测器薄适配），同时把「凭据解封」的密钥材料留在装配层。
type CredentialOpener interface {
	Open(sealed []byte) ([]byte, error)
}

// ProbeResult 是一次探测的完整、非敏感结果。
//
// 字段分组表达互斥的终态：
//   - Success=true：2xx，LatencyMS 有效；
//   - TransportClass != ""：传输层失败（timeout/network）；
//   - Unsupported=true：渠道类型无 adapter（R-M4-06，跳过）；
//   - CredentialInvalid/BuildFailed：本地配置问题（跳过，不判渠道故障）；
//   - 其余：上游非 2xx，StatusCode/ErrorClass/Body 有效。
type ProbeResult struct {
	LatencyMS  int
	StatusCode int
	ErrorClass contract.ErrorClass
	// Body 是上游错误体（≤64 KiB）。只进关键词匹配，绝不写日志/审计。
	Body    []byte
	Success bool

	TransportClass string
	Unsupported    bool
	// CredentialInvalid 表示解封失败/凭据形状非法（GC-8）。
	CredentialInvalid bool
	// BuildFailed 表示适配器无法构造请求（多为本地配置错误）。
	BuildFailed bool
}

// probeCredentials 只是 channels.credentials 明文 JSON 的最小投影。
//
// 复用与 httpapi.ChannelCredentials 相同的字段名；这里只读取 api_key——
// deployment/api_version 仅 azure 变体使用，而 azure 走同一 OpenAI 适配器
// 时 BuildRequest 会按渠道类型自行补默认值（relay-02 §7.4）。
type probeCredentials struct {
	APIKey string `json:"api_key"`
}

// ProberConfig 是 Prober 的显式依赖。
type ProberConfig struct {
	// Opener 必填：渠道凭据的 AES-GCM 解封面。
	Opener CredentialOpener
	// Client 必填：与中继/手工测试同一出站 transport（调用方注入
	// httpapi.NewChannelProbeClient）；本包不自建第二套客户端策略。
	Client *http.Client
	// Timeout 覆盖单次探测上限；<=0 使用 ProbeTimeout（体检 15s）。
	// 手工连通性测试传入 30s（R-M4-14 的例外）。
	Timeout time.Duration
}

// Prober 执行单次探测。并发安全：不持有可变状态，可被 worker 池复用。
type Prober struct {
	opener  CredentialOpener
	client  *http.Client
	timeout time.Duration
}

// NewProber 构造探测器。缺少 Opener/Client 是装配错误：静默降级会变成
// 「永远失败」或「不经既有出站策略」，必须显式失败。
func NewProber(cfg ProberConfig) (*Prober, error) {
	if cfg.Opener == nil {
		return nil, errors.New("channelhealth: prober requires a credential opener")
	}
	if cfg.Client == nil {
		return nil, errors.New("channelhealth: prober requires an http client")
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = ProbeTimeout
	}
	return &Prober{opener: cfg.Opener, client: cfg.Client, timeout: timeout}, nil
}

// Probe 构造并发送一次最小非流式请求（单条 user "ping"，max_tokens=1）。
//
// 与手工连通性测试完全相同：路径/头/模型映射都由 adapter.BuildRequest 决定，
// 因此三种 adapter 族（openai/anthropic/gemini）的探测语义一致。
// 不返回 error：所有失败都编码在 ProbeResult 的互斥字段里，调用方无需解析
// 错误文本（GC-6 的最后一道出口）。
func (p *Prober) Probe(ctx context.Context, ch *store.Channel, model string) ProbeResult {
	if ch == nil {
		return ProbeResult{CredentialInvalid: true}
	}
	plaintext, err := p.opener.Open(ch.Credentials)
	if err != nil {
		return ProbeResult{CredentialInvalid: true}
	}
	var creds probeCredentials
	if err := json.Unmarshal(plaintext, &creds); err != nil || creds.APIKey == "" {
		return ProbeResult{CredentialInvalid: true}
	}

	channelType := contract.ChannelType(ch.Type)
	adapter, err := adapters.Dispatch(channelType)
	if err != nil {
		// Dispatch 的所有失败都是 ErrEndpointUnsupported；无 adapter 的类型
		// 整轮跳过，不改 last_test_*（R-M4-06）。
		return ProbeResult{Unsupported: true}
	}
	apiBase := ""
	if ch.BaseURL != nil {
		apiBase = *ch.BaseURL
	}

	maxTokens := 1
	request := &contract.Request{
		Model:  model,
		Stream: false,
		Messages: []contract.Message{{
			Role:    contract.RoleUser,
			Content: []contract.ContentPart{{Kind: contract.PartText, Text: "ping"}},
		}},
		MaxTokens: &maxTokens,
		RawFormat: ir.FormatOpenAIChat,
	}
	rc := &contract.RelayContext{
		ChannelType:   channelType,
		APIBase:       apiBase,
		Credential:    creds.APIKey,
		UpstreamModel: upstreamModelFor(ch.ModelMapping, model),
		CreatedTime:   ch.CreatedAt,
	}
	httpReq, err := adapter.BuildRequest(rc, request)
	if err != nil {
		// 构造错误可能回显 URL（GC-6）→ 只保留稳定分类，不保留文本。
		return ProbeResult{BuildFailed: true}
	}

	probeCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	start := time.Now()
	resp, err := p.client.Do(httpReq.WithContext(probeCtx))
	if err != nil {
		// *url.Error 的文本含完整 URL → 绝不透出（GC-6）。
		return ProbeResult{TransportClass: TransportClass(err)}
	}
	// 只测到响应头的时间：生成式响应体可能持续数十秒，R-M4-02 冻结的阈值
	// 语义是「上游是否及时开始响应」，不是整段生成耗时。
	latency := int(time.Since(start).Milliseconds())
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, ProbeMaxErrorBodyBytes))
	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		return ProbeResult{LatencyMS: latency, StatusCode: resp.StatusCode, Success: true}
	}
	return ProbeResult{
		LatencyMS:  latency,
		StatusCode: resp.StatusCode,
		ErrorClass: adapter.ClassifyError(resp.StatusCode, body),
		Body:       body,
	}
}

// TransportClass 把传输层错误分为 timeout/network（与手工测试的五值
// error_class 同口径）。
func TransportClass(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return TransportTimeout
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return TransportTimeout
	}
	return TransportNetwork
}

// upstreamModelFor 返回 model_mapping 中「客户端模型 → 上游模型」的映射值；
// 未配置/不是字符串时返回空串，适配器回落到客户端模型名（relay-02 §8.1）。
func upstreamModelFor(raw []byte, model string) string {
	if len(raw) == 0 || model == "" {
		return ""
	}
	var mapping map[string]string
	if err := json.Unmarshal(raw, &mapping); err != nil {
		return ""
	}
	return mapping[model]
}
