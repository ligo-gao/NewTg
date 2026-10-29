package channelhealth

// 流式性能探测（PerfRunner）：通过既有渠道实测真实流式请求的
// TTFT / 总延迟 / 输出 token / TPS。
//
// 与 Probe 的关系：
//   - 凭据解封、适配器分派、BaseURL/模型映射、错误分类全部复用同一套代码
//     （probe.go），不新增第二套出站策略；
//   - Probe 只测到响应头（健康判定），PerfRunner 消费完整 SSE 流（性能评估），
//     因此两者的超时语义不同：perf 的 timeout 覆盖「建连 + 整段生成」。
//
// 纪律：
//   - 不估算 token：只采信上游 usage.completion_tokens，缺席即 nil；
//   - 不记录/透出错误体：非 2xx 的 body 只按 64 KiB 有界读取并交给
//     adapter.ClassifyError，绝不进日志或结果；
//   - 单次 run 失败不影响同批其余 run（性能探测要的是样本，不是一次判定）。

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/egress"
	"newtg/gateway/internal/relay/adapters"
	"newtg/gateway/internal/relay/ir"
	"newtg/gateway/internal/store"
)

const (
	// PerfMaxRuns 是单次 Run 的批上界；多于 10 次对手工性能探测没有意义，
	// 只会放大上游配额消耗。
	PerfMaxRuns = 10
	// PerfMinRuns 是批下界：runs<=0 时退化为单次探测。
	PerfMinRuns = 1
	// PerfMaxTokens 是性能探测请求的输出上界：足够形成可测的生成时长，
	// 又不至于在真实上游上烧掉大量配额。
	PerfMaxTokens = 128
	// PerfPrompt 是固定的单条 user 消息（短回答，跨模型可比）。
	PerfPrompt = "Reply with one short sentence about the weather."
)

// 稳定错误类名（写进 PerfRunResult.ErrorClass，调用方无需解析错误文本）。
//
// 本地失败：
//   - PerfClassCredential：凭据解封失败或 api_key 缺失；
//   - PerfClassBuild：适配器无法构造请求（多为渠道配置错误）；
//   - PerfClassUnsupported：渠道类型无可用 adapter（与 Probe 的跳过语义一致）。
//
// 上游失败：
//   - PerfClassUpstream：非 2xx 且适配器分类未命中具体档位时的兜底；
//   - PerfClassUpstreamAuth / PerfClassUpstreamRateLimit / PerfClassUpstreamServer /
//     PerfClassParamInvalid / PerfClassChannelConfig / PerfClassContentFilter /
//     PerfClassEndpointUnsupported：adapter.ClassifyError 的稳定投影；
//   - PerfClassUpstreamError：2xx 流中出现 EventError（协议级流错误）。
//
// 传输层失败复用 probe.go 的 TransportTimeout / TransportNetwork（"timeout"/"network"）。
const (
	PerfClassCredential          = "credential"
	PerfClassBuild               = "build"
	PerfClassUnsupported         = "unsupported"
	PerfClassUpstream            = "upstream"
	PerfClassUpstreamError       = "upstream_error"
	PerfClassUpstreamAuth        = "upstream_auth"
	PerfClassUpstreamRateLimit   = "upstream_rate_limit"
	PerfClassUpstreamServer      = "upstream_server"
	PerfClassParamInvalid        = "param_invalid"
	PerfClassChannelConfig       = "channel_config"
	PerfClassContentFilter       = "content_filter"
	PerfClassEndpointUnsupported = "endpoint_unsupported"
)

// PerfRunResult 是一次流式性能 run 的非敏感结果。
//
// 指针字段表达「未知」而不是 0：
//   - TTFTMS：首个非空 delta（正文或推理）到达时间；无 delta 或失败在首 token
//     之前时为 nil；
//   - LatencyMS：请求开始到流终结（迭代器耗尽或事件错误）的总耗时；
//   - OutputTokens：上游 usage.completion_tokens（>0 时）；上游未上报时为 nil；
//   - TPS：OutputTokens / (LatencyMS/1000)；任一未知或延迟为 0 时为 nil。
type PerfRunResult struct {
	Success      bool
	TTFTMS       *int
	LatencyMS    *int
	OutputTokens *int
	TPS          *float64
	ErrorClass   string
}

// PerfRunner 执行流式性能探测。并发安全：不持有可变状态，每个 run 独立超时。
type PerfRunner struct {
	opener  CredentialOpener
	client  *http.Client
	timeout time.Duration
}

// NewPerfRunner 构造性能探测器，复用 ProberConfig（opener + client + timeout）。
//
// 超时口径与 NewProber 一致：Timeout<=0 时取 ProbeTimeout（15s）；正值按调用方
// 指定（手工性能测试通常传 30s，覆盖建连 + 整段生成）。缺少依赖是装配错误。
func NewPerfRunner(cfg ProberConfig) (*PerfRunner, error) {
	if cfg.Opener == nil {
		return nil, errors.New("channelhealth: perf runner requires a credential opener")
	}
	if cfg.Client == nil {
		return nil, errors.New("channelhealth: perf runner requires an http client")
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = ProbeTimeout
	}
	return &PerfRunner{opener: cfg.Opener, client: cfg.Client, timeout: timeout}, nil
}

// Run 顺序执行 runs 次性能探测，每次一个独立结果。
//
// runs 被钳制到 [PerfMinRuns, PerfMaxRuns]；单次失败只体现在该次的
// PerfRunResult.ErrorClass，不中止批次。
func (p *PerfRunner) Run(ctx context.Context, ch *store.Channel, model string, runs int) []PerfRunResult {
	if runs < PerfMinRuns {
		runs = PerfMinRuns
	}
	if runs > PerfMaxRuns {
		runs = PerfMaxRuns
	}
	results := make([]PerfRunResult, 0, runs)
	for i := 0; i < runs; i++ {
		results = append(results, p.run(ctx, ch, model))
	}
	return results
}

// run 执行一次流式探测。
func (p *PerfRunner) run(ctx context.Context, ch *store.Channel, model string) PerfRunResult {
	if ch == nil {
		return PerfRunResult{ErrorClass: PerfClassCredential}
	}
	// 凭据解封与形状校验：与 Probe 完全相同，失败属本地配置问题。
	plaintext, err := p.opener.Open(ch.Credentials)
	if err != nil {
		return PerfRunResult{ErrorClass: PerfClassCredential}
	}
	var creds probeCredentials
	if err := json.Unmarshal(plaintext, &creds); err != nil || creds.APIKey == "" {
		return PerfRunResult{ErrorClass: PerfClassCredential}
	}

	channelType := contract.ChannelType(ch.Type)
	adapter, err := adapters.Dispatch(channelType)
	if err != nil {
		return PerfRunResult{ErrorClass: PerfClassUnsupported}
	}
	streamer, ok := adapter.(contract.StreamAdapter)
	if !ok {
		// Dispatch 目前只返回三族流式适配器；显式失败优于对 nil 调方法。
		return PerfRunResult{ErrorClass: PerfClassUnsupported}
	}

	apiBase := ""
	if ch.BaseURL != nil {
		apiBase = *ch.BaseURL
	}
	maxTokens := PerfMaxTokens
	request := &contract.Request{
		Model:  model,
		Stream: true,
		Messages: []contract.Message{{
			Role:    contract.RoleUser,
			Content: []contract.ContentPart{{Kind: contract.PartText, Text: PerfPrompt}},
		}},
		MaxTokens: &maxTokens,
		RawFormat: ir.FormatOpenAIChat,
		// OpenAI 兼容上游只有显式请求 include_usage 才会在流末回报 usage；
		// 没有它就无法得到 OutputTokens/TPS。Raw 仅是 OpenAI 请求体的基底，
		// 原生协议适配器（Anthropic/Gemini）不读取它。
		Raw: contract.ProtocolBody(`{"stream_options":{"include_usage":true}}`),
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
		// 构造错误可能回显 URL（GC-6）→ 只保留稳定分类。
		return PerfRunResult{ErrorClass: PerfClassBuild}
	}

	runCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	// R-01：与体检/目录同口径，用同一份渠道级私网 opt-in。
	if ch.AllowPrivateNetwork {
		runCtx = egress.WithAllowPrivateNetwork(runCtx)
	}

	start := time.Now()
	resp, err := p.client.Do(httpReq.WithContext(runCtx))
	if err != nil {
		// *url.Error 文本含完整 URL/凭据 → 只保留 timeout/network。
		return PerfRunResult{ErrorClass: TransportClass(err)}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		// 错误体只用于分类：有界读取 64 KiB，绝不写日志/结果（GC-6）。
		body, _ := io.ReadAll(io.LimitReader(resp.Body, ProbeMaxErrorBodyBytes))
		class := adapter.ClassifyError(resp.StatusCode, body)
		return PerfRunResult{ErrorClass: perfUpstreamClass(class)}
	}

	// StreamEvents 需要 io.ReadWriter；SSE 适配器只读，写侧是空实现。
	rw := perfStreamReadWriter{Reader: resp.Body}
	var (
		ttft         *int
		lastUsage    *contract.Usage
		failedClass  string
		streamEvents = streamer.StreamEvents(rc, rw)
	)
	for event := range streamEvents {
		switch event.Kind {
		case contract.EventDelta:
			// TTFT 只认第一个「有内容」的增量：空 delta / 纯 tool_call 不算首 token。
			if ttft == nil && (event.Delta != "" || event.Reasoning != "") {
				ms := int(time.Since(start).Milliseconds())
				ttft = &ms
			}
		case contract.EventUsage:
			if event.Usage != nil {
				usage := *event.Usage // 拷贝：事件缓冲可能复用
				lastUsage = &usage
			}
		case contract.EventError:
			failedClass = PerfClassUpstreamError
			// 流内超时/取消只会表现为读取错误；用 runCtx 的终态还原稳定分类。
			if ctxErr := runCtx.Err(); ctxErr != nil {
				failedClass = TransportClass(ctxErr)
			}
		}
		if failedClass != "" {
			break
		}
	}

	latencyMS := int(time.Since(start).Milliseconds())
	result := PerfRunResult{TTFTMS: ttft, LatencyMS: &latencyMS, ErrorClass: failedClass}
	if failedClass != "" {
		return result
	}

	result.Success = true
	if lastUsage != nil && lastUsage.Completion > 0 {
		outputTokens := int(lastUsage.Completion)
		result.OutputTokens = &outputTokens
		if latencyMS > 0 {
			tps := float64(outputTokens) / (float64(latencyMS) / 1000)
			result.TPS = &tps
		}
	}
	return result
}

// perfStreamReadWriter 把响应体适配为 StreamEvents 需要的 io.ReadWriter。
// SSE 是单向流：适配器只读取，写侧接受并丢弃（WebSocket 形态不走本探测器）。
type perfStreamReadWriter struct {
	io.Reader
}

func (perfStreamReadWriter) Write(p []byte) (int, error) { return len(p), nil }

// perfUpstreamClass 把契约错误分类投影为稳定字符串。
// 未命中的分类统一回落 PerfClassUpstream，避免把枚举数值泄漏给调用方。
func perfUpstreamClass(class contract.ErrorClass) string {
	switch class {
	case contract.ErrUpstreamAuth:
		return PerfClassUpstreamAuth
	case contract.ErrUpstreamRateLimit:
		return PerfClassUpstreamRateLimit
	case contract.ErrUpstreamServer:
		return PerfClassUpstreamServer
	case contract.ErrParamInvalid:
		return PerfClassParamInvalid
	case contract.ErrChannelConfig:
		return PerfClassChannelConfig
	case contract.ErrContentFilter:
		return PerfClassContentFilter
	case contract.ErrEndpointUnsupported:
		return PerfClassEndpointUnsupported
	default:
		return PerfClassUpstream
	}
}
