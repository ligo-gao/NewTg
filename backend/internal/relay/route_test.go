package relay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/relay/adapters"
	"newtg/gateway/internal/relay/billingseam"
)

// routeTestChannelHeader 让假 RoundTripper 知道本次请求打到了哪个渠道。
// 渠道身份只能由执行层写入 RelayContext（relay-01 §7.4），故 BuildRequest
// 从 rc.ChannelID 取值写进请求头；测试据此断言「尝试顺序」与「轮次内渠道组」。
const routeTestChannelHeader = "X-Test-Channel-ID"

// routeCallLog 记录两类调用：BuildRequest 的次数（每轮构造上游请求）与真实
// 传输次数（每次 client.Do 进 Transport）。分开记录才能区分「构造失败」与
// 「发出后失败」两种消耗重试预算的方式。
type routeCallLog struct {
	buildCalls []int
	httpCalls  []int
}

// routeTestAdapter 是 contract.Adapter 的最小测试替身。
// ClassifyError 按 R-P13 的常见状态给出分类，但路由层的重试决策**只依据状态码**，
// 因此分类只用于断言错误链上的 RelayError，不影响是否重试。
type routeTestAdapter struct {
	log      *routeCallLog
	buildErr error
	parsed   *contract.Response
	usage    contract.Usage
	parseErr error
	class    contract.ErrorClass
}

func (a *routeTestAdapter) Type() contract.ChannelType { return contract.ChannelTypeOpenAI }

func (a *routeTestAdapter) Caps() contract.Capabilities { return contract.Capabilities{} }

func (a *routeTestAdapter) BuildRequest(rc *contract.RelayContext, _ *contract.Request) (*http.Request, error) {
	if a.log != nil {
		a.log.buildCalls = append(a.log.buildCalls, rc.ChannelID)
	}
	if a.buildErr != nil {
		return nil, a.buildErr
	}
	req, err := http.NewRequest(http.MethodPost, "https://upstream.invalid/v1/chat/completions", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set(routeTestChannelHeader, strconv.Itoa(rc.ChannelID))
	return req, nil
}

func (a *routeTestAdapter) ParseResponse(*contract.RelayContext, *http.Response) (*contract.Response, contract.Usage, error) {
	if a.parseErr != nil {
		return nil, contract.Usage{}, a.parseErr
	}
	if a.parsed == nil {
		return &contract.Response{ID: "route-test-response"}, a.usage, nil
	}
	return a.parsed, a.usage, nil
}

func (a *routeTestAdapter) ClassifyError(status int, _ []byte) contract.ErrorClass {
	if a.class != contract.ErrUnknown {
		return a.class
	}
	switch {
	case status == http.StatusTooManyRequests:
		return contract.ErrUpstreamRateLimit
	case status >= 500:
		return contract.ErrUpstreamServer
	default:
		return contract.ErrUnknown
	}
}

// routeRoundTripper 把「按渠道生成的响应」注入 http.Client，测试因此不需要
// 起真实上游；同时保留 client.Do 的完整错误包装（*url.Error 等），
// 用于验证 GC-6：传输错误文本里有 URL，路由层不得原样透出。
type routeRoundTripper func(*http.Request) (*http.Response, error)

func (f routeRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func routeResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// routeTestDeps 组装注入式依赖（R-P4）：候选来源、渠道解析、分派、客户端与计费桩。
func routeTestDeps(
	candidates []contract.ChannelCandidate,
	resolve map[int]ChannelAssignment,
	log *routeCallLog,
	respond func(channelID int) (*http.Response, error),
	adapter contract.Adapter,
	billing contract.BillingSession,
	retries int,
) RouteDeps {
	return RouteDeps{
		Candidates: func(context.Context, *contract.RelayContext) ([]contract.ChannelCandidate, error) {
			return candidates, nil
		},
		ResolveChannel: func(_ context.Context, channelID int) (ChannelAssignment, error) {
			assignment, ok := resolve[channelID]
			if !ok {
				return ChannelAssignment{}, fmt.Errorf("test: channel %d not resolvable", channelID)
			}
			return assignment, nil
		},
		Dispatch: func(contract.ChannelType) (contract.Adapter, error) { return adapter, nil },
		Billing:  billing,
		Client: &http.Client{Transport: routeRoundTripper(func(r *http.Request) (*http.Response, error) {
			channelID, _ := strconv.Atoi(r.Header.Get(routeTestChannelHeader))
			log.httpCalls = append(log.httpCalls, channelID)
			return respond(channelID)
		})},
		MaxRetries: retries,
	}
}

func routeTestContext(streaming bool) *contract.RelayContext {
	return &contract.RelayContext{
		RequestID:   "req-route-test",
		OriginModel: "client-model",
		Streaming:   streaming,
		Request:     &contract.Request{Model: "client-model", Stream: streaming},
		UsingGroup:  "default",
	}
}

func routeTestAssignment(channelID int) ChannelAssignment {
	return ChannelAssignment{
		ChannelID:     channelID,
		ChannelType:   contract.ChannelTypeOpenAI,
		APIBase:       "https://api.secret.example",
		Credential:    "sk-route-test-credential",
		UpstreamModel: "upstream-model",
		GroupRatio:    1.5,
		CreatedTime:   time.Unix(1700000000, 0),
	}
}

func routeTestEstimate(maxTokens int) *contract.QuotaEstimate {
	return &contract.QuotaEstimate{Model: "client-model", Group: "default", MaxTokens: &maxTokens}
}

// recordingBilling 包住 Task 6 的 Stub，额外记录每个生命周期调用：
// GC-7 要求失败时必须 Terminate 且携带空 UsageFact，Stub 只暴露状态位，
// 记录不到传参，故测试在接缝上加一层观察。
type recordingBilling struct {
	*billingseam.Stub
	preConsumeCalls int
	terminateFacts  []contract.UsageFact
}

func (b *recordingBilling) PreConsume(est contract.QuotaEstimate) error {
	b.preConsumeCalls++
	return b.Stub.PreConsume(est)
}

func (b *recordingBilling) Terminate(fact contract.UsageFact) error {
	b.terminateFacts = append(b.terminateFacts, fact)
	return b.Stub.Terminate(fact)
}

func routeTestSingleCandidate(id int) []contract.ChannelCandidate {
	return []contract.ChannelCandidate{{ChannelID: id, Priority: 10}}
}

// TestRouteSingleSuccess：单一渠道成功——一次构造、一次传输、渠道组整组写入，
// Route 负责 ParseResponse + 正常 Terminate（ExecuteAttempts 在成功时不终结）。
func TestRouteSingleSuccess(t *testing.T) {
	log := &routeCallLog{}
	adapter := &routeTestAdapter{log: log, usage: contract.Usage{Prompt: 10, Completion: 5, Total: 15}}
	billing := &recordingBilling{Stub: billingseam.NewStub()}
	deps := routeTestDeps(
		routeTestSingleCandidate(7),
		map[int]ChannelAssignment{7: routeTestAssignment(7)},
		log,
		func(int) (*http.Response, error) { return routeResponse(http.StatusOK, `{}`), nil },
		adapter,
		billing,
		3,
	)
	rc := routeTestContext(false)
	rc.QuotaEstimate = routeTestEstimate(100)

	resp, err := Route(context.Background(), rc, deps)
	if err != nil {
		t.Fatalf("Route() error = %v", err)
	}
	if resp == nil || resp.ID != "route-test-response" {
		t.Fatalf("Route() response = %#v，期望假适配器的解析结果", resp)
	}
	if len(log.buildCalls) != 1 || log.buildCalls[0] != 7 {
		t.Fatalf("BuildRequest 渠道序列 = %v，期望 [7]", log.buildCalls)
	}
	if len(log.httpCalls) != 1 || log.httpCalls[0] != 7 {
		t.Fatalf("上游传输渠道序列 = %v，期望 [7]", log.httpCalls)
	}
	// relay-01 §7.4：执行层每轮整组写渠道组（含 CreatedTime/GroupRatio）。
	if rc.ChannelID != 7 || rc.ChannelType != contract.ChannelTypeOpenAI ||
		rc.APIBase != "https://api.secret.example" || rc.Credential != "sk-route-test-credential" ||
		rc.UpstreamModel != "upstream-model" || rc.GroupRatio != 1.5 ||
		!rc.CreatedTime.Equal(time.Unix(1700000000, 0)) {
		t.Fatalf("渠道组未整组写入：%#v", rc)
	}
	if rc.AttemptIndex != 0 {
		t.Fatalf("AttemptIndex = %d，期望 0", rc.AttemptIndex)
	}
	if billing.preConsumeCalls != 1 {
		t.Fatalf("PreConsume 调用 %d 次，期望 1", billing.preConsumeCalls)
	}
	if !billing.Terminated() {
		t.Fatal("Route 成功后应调用 Terminate 结算最终事实")
	}
	if billing.Sessions() != 0 || billing.Reserved() != 0 {
		t.Fatalf("Terminate 后 sessions=%d reserved=%d，期望 0/0", billing.Sessions(), billing.Reserved())
	}
	if len(billing.terminateFacts) != 1 {
		t.Fatalf("Terminate 调用 %d 次，期望 1", len(billing.terminateFacts))
	}
	fact := billing.terminateFacts[0]
	if fact.Model != "client-model" || fact.Usage.Prompt != 10 || fact.Usage.Completion != 5 ||
		fact.Attempts != 1 || fact.Stream || fact.FirstTokenMS != nil {
		t.Fatalf("Terminate 用量事实 = %#v，期望客户端模型/client-model、usage(10,5)、attempts=1、非流式", fact)
	}
}

// TestRouteRetryOn500ThenSuccess：5xx 可重试（R-P13），换下一个候选并成功；
// 成功时 ExecuteAttempts 不得 Terminate（最终事实由调用方结算，R-P4）。
func TestRouteRetryOn500ThenSuccess(t *testing.T) {
	log := &routeCallLog{}
	adapter := &routeTestAdapter{log: log}
	billing := &recordingBilling{Stub: billingseam.NewStub()}
	deps := routeTestDeps(
		[]contract.ChannelCandidate{{ChannelID: 1, Priority: 10}, {ChannelID: 2, Priority: 5}},
		map[int]ChannelAssignment{1: routeTestAssignment(1), 2: routeTestAssignment(2)},
		log,
		func(channelID int) (*http.Response, error) {
			if channelID == 1 {
				return routeResponse(http.StatusInternalServerError, "boom"), nil
			}
			return routeResponse(http.StatusOK, `{}`), nil
		},
		adapter,
		billing,
		3,
	)
	rc := routeTestContext(false)

	result, err := ExecuteAttempts(context.Background(), rc, deps)
	if err != nil {
		t.Fatalf("ExecuteAttempts() error = %v", err)
	}
	defer result.Response.Body.Close()
	if result.Candidate.ChannelID != 2 || result.Response.StatusCode != http.StatusOK {
		t.Fatalf("成功结果 = candidate %d / status %d，期望渠道 2 / 200", result.Candidate.ChannelID, result.Response.StatusCode)
	}
	if got := fmt.Sprint(log.httpCalls); got != "[1 2]" {
		t.Fatalf("尝试顺序 = %v，期望 [1 2]", log.httpCalls)
	}
	if rc.AttemptIndex != 1 {
		t.Fatalf("AttemptIndex = %d，期望 1（第二次尝试成功）", rc.AttemptIndex)
	}
	if billing.Terminated() {
		t.Fatal("成功路径不得在 ExecuteAttempts 内 Terminate（调用方持有最终事实）")
	}
	if billing.Sessions() != 1 {
		t.Fatalf("成功路径 sessions=%d，期望 1（会话仍未终结）", billing.Sessions())
	}
}

// TestRouteFastFail400：400 属固定不重试集合（R-P13），只允许 1 次尝试并释放预算。
func TestRouteFastFail400(t *testing.T) {
	log := &routeCallLog{}
	adapter := &routeTestAdapter{log: log}
	billing := &recordingBilling{Stub: billingseam.NewStub()}
	deps := routeTestDeps(
		[]contract.ChannelCandidate{{ChannelID: 1, Priority: 10}, {ChannelID: 2, Priority: 5}},
		map[int]ChannelAssignment{1: routeTestAssignment(1), 2: routeTestAssignment(2)},
		log,
		func(int) (*http.Response, error) { return routeResponse(http.StatusBadRequest, "bad request"), nil },
		adapter,
		billing,
		3,
	)
	rc := routeTestContext(false)
	rc.QuotaEstimate = routeTestEstimate(100)

	_, err := ExecuteAttempts(context.Background(), rc, deps)
	if err == nil {
		t.Fatal("400 应快速失败")
	}
	var fast *FastFailError
	if !errors.As(err, &fast) {
		t.Fatalf("错误 = %T %v，期望 *FastFailError", err, err)
	}
	if len(log.httpCalls) != 1 {
		t.Fatalf("尝试次数 = %d，期望 1（400 不重试）", len(log.httpCalls))
	}
	if !billing.Terminated() || billing.Sessions() != 0 || billing.Reserved() != 0 {
		t.Fatalf("快速失败后应释放预算：terminated=%v sessions=%d reserved=%d",
			billing.Terminated(), billing.Sessions(), billing.Reserved())
	}
}

// TestRouteRetryBudgetExhausted：候选耗尽返回聚合 AttemptsError，且次数只由
// MaxRetries 钳制决定（单候选时 §6.4 的索引钳制会重复打同一个候选）。
func TestRouteRetryBudgetExhausted(t *testing.T) {
	log := &routeCallLog{}
	adapter := &routeTestAdapter{log: log}
	billing := &recordingBilling{Stub: billingseam.NewStub()}
	deps := routeTestDeps(
		routeTestSingleCandidate(7),
		map[int]ChannelAssignment{7: routeTestAssignment(7)},
		log,
		func(int) (*http.Response, error) { return routeResponse(http.StatusInternalServerError, "boom"), nil },
		adapter,
		billing,
		1, // 1 次重试 → 2 次总尝试
	)
	rc := routeTestContext(false)
	rc.QuotaEstimate = routeTestEstimate(100)

	_, err := ExecuteAttempts(context.Background(), rc, deps)
	var attemptsErr *AttemptsError
	if !errors.As(err, &attemptsErr) {
		t.Fatalf("错误 = %T %v，期望 *AttemptsError", err, err)
	}
	if attemptsErr.Attempts != 2 {
		t.Fatalf("Attempts = %d，期望 2", attemptsErr.Attempts)
	}
	if len(log.httpCalls) != 2 {
		t.Fatalf("上游尝试 %d 次，期望 2", len(log.httpCalls))
	}
	if !strings.Contains(err.Error(), "2") {
		t.Fatalf("错误文本 %q 未包含尝试次数", err.Error())
	}
	if !billing.Terminated() || billing.Sessions() != 0 || billing.Reserved() != 0 {
		t.Fatalf("耗尽后应释放预算：terminated=%v sessions=%d reserved=%d",
			billing.Terminated(), billing.Sessions(), billing.Reserved())
	}
}

// TestRouteCandidateIndexClamp 冻结 §6.4 的优先级数组钳制照搬：索引越过候选数后
// 落在最后一个候选上；因此「尝试轮数 < 优先级数」时低优先级候选可能永不被触达。
func TestRouteCandidateIndexClamp(t *testing.T) {
	log := &routeCallLog{}
	adapter := &routeTestAdapter{log: log}
	deps := routeTestDeps(
		[]contract.ChannelCandidate{{ChannelID: 1, Priority: 10}, {ChannelID: 2, Priority: 5}},
		map[int]ChannelAssignment{1: routeTestAssignment(1), 2: routeTestAssignment(2)},
		log,
		func(int) (*http.Response, error) { return routeResponse(http.StatusInternalServerError, "boom"), nil },
		adapter,
		nil,
		3, // 4 次尝试，但只有 2 个候选
	)

	_, err := ExecuteAttempts(context.Background(), routeTestContext(false), deps)
	var attemptsErr *AttemptsError
	if !errors.As(err, &attemptsErr) || attemptsErr.Attempts != 4 {
		t.Fatalf("错误 = %v，期望 AttemptsError{Attempts:4}", err)
	}
	if got := fmt.Sprint(log.httpCalls); got != "[1 2 2 2]" {
		t.Fatalf("尝试顺序 = %v，期望 [1 2 2 2]（越界钳制到末位候选）", log.httpCalls)
	}
}

// TestRoutePinSelected：pin 约束来自 rc.AttemptPlan（BuildRelayContext 已做非管理员
// 403，路由层不得再解析凭据），执行层把 pin 候选提到首位；且不得原地改写调用方切片。
func TestRoutePinSelected(t *testing.T) {
	log := &routeCallLog{}
	adapter := &routeTestAdapter{log: log}
	provided := &contract.AttemptPlan{Candidates: []contract.ChannelCandidate{
		{ChannelID: 1, Priority: 10},
		{ChannelID: 2, Priority: 5, Constraints: contract.ChannelConstraints{PinChannelID: 2}},
	}}
	rc := routeTestContext(false)
	rc.AttemptPlan = provided

	deps := routeTestDeps(
		nil, // 已有计划时不得回落到候选来源
		map[int]ChannelAssignment{1: routeTestAssignment(1), 2: routeTestAssignment(2)},
		log,
		func(channelID int) (*http.Response, error) {
			if channelID != 2 {
				return routeResponse(http.StatusInternalServerError, "wrong channel"), nil
			}
			return routeResponse(http.StatusOK, `{}`), nil
		},
		adapter,
		nil,
		3,
	)

	result, err := ExecuteAttempts(context.Background(), rc, deps)
	if err != nil {
		t.Fatalf("ExecuteAttempts() error = %v", err)
	}
	defer result.Response.Body.Close()
	if result.Candidate.ChannelID != 2 {
		t.Fatalf("pin 命中 = %d，期望 2", result.Candidate.ChannelID)
	}
	if got := fmt.Sprint(log.httpCalls); got != "[2]" {
		t.Fatalf("尝试序列 = %v，期望 [2]（pin 优先）", log.httpCalls)
	}
	if rc.AttemptPlan == nil || rc.AttemptPlan.Candidates[0].ChannelID != 2 {
		t.Fatalf("生效计划首位 = %#v，期望 pin 渠道 2", rc.AttemptPlan)
	}
	if provided.Candidates[0].ChannelID != 1 {
		t.Fatal("不得原地改写调用方提供的候选切片（应先复制再重排）")
	}
}

// TestRoutePinAbsent：pin 指定了不存在的渠道 → ErrChannelNotFound 快失败
// （HTTP-404 语义）；此刻尚未 PreConsume，不得留下计费会话。
func TestRoutePinAbsent(t *testing.T) {
	log := &routeCallLog{}
	adapter := &routeTestAdapter{log: log}
	billing := &recordingBilling{Stub: billingseam.NewStub()}
	rc := routeTestContext(false)
	rc.AttemptPlan = &contract.AttemptPlan{Candidates: []contract.ChannelCandidate{
		{ChannelID: 1, Priority: 10, Constraints: contract.ChannelConstraints{PinChannelID: 99}},
	}}

	candidatesCalled := false
	deps := RouteDeps{
		Candidates: func(context.Context, *contract.RelayContext) ([]contract.ChannelCandidate, error) {
			candidatesCalled = true
			return routeTestSingleCandidate(1), nil
		},
		ResolveChannel: func(context.Context, int) (ChannelAssignment, error) {
			t.Fatal("pin 缺失时不得解析渠道")
			return ChannelAssignment{}, nil
		},
		Dispatch:   func(contract.ChannelType) (contract.Adapter, error) { return adapter, nil },
		Billing:    billing,
		Client:     &http.Client{Transport: routeRoundTripper(func(*http.Request) (*http.Response, error) { return nil, errors.New("unexpected") })},
		MaxRetries: 3,
	}

	_, err := ExecuteAttempts(context.Background(), rc, deps)
	if !errors.Is(err, ErrChannelNotFound) {
		t.Fatalf("错误 = %v，期望包装 ErrChannelNotFound", err)
	}
	var fast *FastFailError
	if !errors.As(err, &fast) {
		t.Fatalf("错误 = %T，期望 *FastFailError（404 快失败语义）", err)
	}
	if candidatesCalled {
		t.Fatal("已有计划时不得调用候选来源")
	}
	if len(log.httpCalls) != 0 {
		t.Fatalf("pin 缺失时发出了 %d 次上游请求，期望 0", len(log.httpCalls))
	}
	if billing.preConsumeCalls != 0 || billing.Sessions() != 0 {
		t.Fatalf("pin 校验应先于 PreConsume：preConsume=%d sessions=%d", billing.preConsumeCalls, billing.Sessions())
	}
}

// TestRouteNoRetryStatuses：R-P13 的固定不重试集合（除 400 外）逐一验证：
// 401/403/422（4xx 鉴权/参数）与 408/504/524（超时类）都只允许 1 次尝试。
func TestRouteNoRetryStatuses(t *testing.T) {
	for _, status := range []int{
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusRequestTimeout,
		http.StatusUnprocessableEntity,
		http.StatusGatewayTimeout,
		524, // Cloudflare 超时；net/http 无常量
	} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			log := &routeCallLog{}
			adapter := &routeTestAdapter{log: log}
			billing := &recordingBilling{Stub: billingseam.NewStub()}
			deps := routeTestDeps(
				[]contract.ChannelCandidate{{ChannelID: 1, Priority: 10}, {ChannelID: 2, Priority: 5}},
				map[int]ChannelAssignment{1: routeTestAssignment(1), 2: routeTestAssignment(2)},
				log,
				func(int) (*http.Response, error) { return routeResponse(status, "upstream rejected"), nil },
				adapter,
				billing,
				3,
			)

			_, err := ExecuteAttempts(context.Background(), routeTestContext(false), deps)
			var fast *FastFailError
			if !errors.As(err, &fast) {
				t.Fatalf("状态 %d 的错误 = %T %v，期望 *FastFailError", status, err, err)
			}
			if len(log.httpCalls) != 1 {
				t.Fatalf("状态 %d 尝试 %d 次，期望 1（不重试）", status, len(log.httpCalls))
			}
			if !billing.Terminated() {
				t.Fatalf("状态 %d 快速失败后未 Terminate（GC-7）", status)
			}
		})
	}
}

// TestRouteRetry429：429 是固定集合的反例——必须重试并可在下一候选成功。
func TestRouteRetry429(t *testing.T) {
	log := &routeCallLog{}
	adapter := &routeTestAdapter{log: log}
	deps := routeTestDeps(
		[]contract.ChannelCandidate{{ChannelID: 1, Priority: 10}, {ChannelID: 2, Priority: 5}},
		map[int]ChannelAssignment{1: routeTestAssignment(1), 2: routeTestAssignment(2)},
		log,
		func(channelID int) (*http.Response, error) {
			if channelID == 1 {
				return routeResponse(http.StatusTooManyRequests, "slow down"), nil
			}
			return routeResponse(http.StatusOK, `{}`), nil
		},
		adapter,
		nil,
		3,
	)

	result, err := ExecuteAttempts(context.Background(), routeTestContext(false), deps)
	if err != nil {
		t.Fatalf("429 后应重试并成功，实际错误 = %v", err)
	}
	defer result.Response.Body.Close()
	if got := fmt.Sprint(log.httpCalls); got != "[1 2]" {
		t.Fatalf("尝试序列 = %v，期望 [1 2]", log.httpCalls)
	}
}

// TestRouteTransportTimeoutReleasesBudget：传输层超时属可重试故障（R-P13），
// 耗尽后必须 Terminate 释放预留（GC-7：stub 的 sessions/reserved 归零），
// 且 GC-6 要求错误文本不得带出 url.Error 里的上游地址。
func TestRouteTransportTimeoutReleasesBudget(t *testing.T) {
	log := &routeCallLog{}
	adapter := &routeTestAdapter{log: log}
	billing := &recordingBilling{Stub: billingseam.NewStubWithLimit(1000)}
	timeoutErr := &url.Error{
		Op:  "Post",
		URL: "https://api.secret.example/v1/chat/completions",
		Err: &netTimeoutError{},
	}
	deps := routeTestDeps(
		routeTestSingleCandidate(7),
		map[int]ChannelAssignment{7: routeTestAssignment(7)},
		log,
		func(int) (*http.Response, error) { return nil, timeoutErr },
		adapter,
		billing,
		1, // 2 次尝试后耗尽
	)
	rc := routeTestContext(false)
	rc.QuotaEstimate = routeTestEstimate(100)

	_, err := ExecuteAttempts(context.Background(), rc, deps)
	var attemptsErr *AttemptsError
	if !errors.As(err, &attemptsErr) {
		t.Fatalf("错误 = %T %v，期望 *AttemptsError", err, err)
	}
	if attemptsErr.Attempts != 2 {
		t.Fatalf("Attempts = %d，期望 2（传输超时可重试）", attemptsErr.Attempts)
	}
	var retryable *RetryableError
	if !errors.As(err, &retryable) {
		t.Fatalf("错误链上应有 *RetryableError，实际 %v", err)
	}
	if !billing.Terminated() {
		t.Fatal("传输失败耗尽后未 Terminate（GC-7）")
	}
	if billing.Sessions() != 0 || billing.Reserved() != 0 {
		t.Fatalf("终止后 sessions=%d reserved=%d，期望 0/0", billing.Sessions(), billing.Reserved())
	}
	if strings.Contains(err.Error(), "api.secret.example") {
		t.Fatalf("错误文本泄露了上游地址：%q", err.Error())
	}
}

// netTimeoutError 实现 net.Error 的 Timeout，模拟传输层响应头超时。
type netTimeoutError struct{}

func (e *netTimeoutError) Error() string { return "i/o timeout" }
func (e *netTimeoutError) Timeout() bool { return true }

// TestRouteContextCancellation：客户端断开/取消必须立即停止（不再消耗重试预算）
// 并释放预算；错误链保留 context.Canceled 供上层区分「取消」与「上游失败」。
func TestRouteContextCancellation(t *testing.T) {
	log := &routeCallLog{}
	adapter := &routeTestAdapter{log: log}
	billing := &recordingBilling{Stub: billingseam.NewStubWithLimit(1000)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	deps := routeTestDeps(
		routeTestSingleCandidate(7),
		map[int]ChannelAssignment{7: routeTestAssignment(7)},
		log,
		func(int) (*http.Response, error) {
			cancel()
			return nil, ctx.Err()
		},
		adapter,
		billing,
		5,
	)
	rc := routeTestContext(false)
	rc.QuotaEstimate = routeTestEstimate(100)

	_, err := ExecuteAttempts(ctx, rc, deps)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("错误 = %v，期望 errors.Is(..., context.Canceled)", err)
	}
	if len(log.httpCalls) != 1 {
		t.Fatalf("取消后尝试 %d 次，期望 1（立即停止）", len(log.httpCalls))
	}
	if !billing.Terminated() || billing.Sessions() != 0 || billing.Reserved() != 0 {
		t.Fatalf("取消后应释放预算：terminated=%v sessions=%d reserved=%d",
			billing.Terminated(), billing.Sessions(), billing.Reserved())
	}
}

// TestRouteNoSecretsInError 冻结 GC-6：构造错误、传输错误与上游响应体三处的
// APIBase/URL/凭据都不得出现在对外错误文本中（只允许状态码与分类）。
func TestRouteNoSecretsInError(t *testing.T) {
	const secretBase = "https://api.secret.example"
	const secretCredential = "sk-route-test-credential"

	t.Run("build error", func(t *testing.T) {
		log := &routeCallLog{}
		adapter := &routeTestAdapter{
			log:      log,
			buildErr: fmt.Errorf("dial %s with credential %s failed", secretBase, secretCredential),
		}
		deps := routeTestDeps(
			routeTestSingleCandidate(7),
			map[int]ChannelAssignment{7: routeTestAssignment(7)},
			log,
			func(int) (*http.Response, error) { return routeResponse(http.StatusOK, `{}`), nil },
			adapter,
			nil,
			1,
		)
		_, err := ExecuteAttempts(context.Background(), routeTestContext(false), deps)
		if err == nil {
			t.Fatal("构造失败应返回错误")
		}
		if strings.Contains(err.Error(), "api.secret.example") || strings.Contains(err.Error(), secretCredential) {
			t.Fatalf("构造错误泄露敏感信息：%q", err.Error())
		}
	})

	t.Run("transport error", func(t *testing.T) {
		log := &routeCallLog{}
		adapter := &routeTestAdapter{log: log}
		deps := routeTestDeps(
			routeTestSingleCandidate(7),
			map[int]ChannelAssignment{7: routeTestAssignment(7)},
			log,
			func(int) (*http.Response, error) {
				return nil, &url.Error{Op: "Post", URL: secretBase + "/v1/chat/completions", Err: errors.New(secretCredential)}
			},
			adapter,
			nil,
			1,
		)
		_, err := ExecuteAttempts(context.Background(), routeTestContext(false), deps)
		if err == nil {
			t.Fatal("传输失败应返回错误")
		}
		if strings.Contains(err.Error(), "api.secret.example") || strings.Contains(err.Error(), secretCredential) {
			t.Fatalf("传输错误泄露敏感信息：%q", err.Error())
		}
	})

	t.Run("upstream body", func(t *testing.T) {
		log := &routeCallLog{}
		adapter := &routeTestAdapter{log: log}
		deps := routeTestDeps(
			routeTestSingleCandidate(7),
			map[int]ChannelAssignment{7: routeTestAssignment(7)},
			log,
			func(int) (*http.Response, error) {
				return routeResponse(http.StatusInternalServerError, `{"echo":"`+secretCredential+` from `+secretBase+`"}`), nil
			},
			adapter,
			nil,
			0,
		)
		_, err := ExecuteAttempts(context.Background(), routeTestContext(false), deps)
		if err == nil {
			t.Fatal("5xx 耗尽应返回错误")
		}
		if strings.Contains(err.Error(), "api.secret.example") || strings.Contains(err.Error(), secretCredential) {
			t.Fatalf("错误文本带出了上游响应体：%q", err.Error())
		}
	})
}

// TestRouteStreamingRejected 冻结 R-P7：Route 只做非流式；流式调用方必须先用
// ExecuteAttempts 拿到原始 2xx 响应，再交给流式层。
func TestRouteStreamingRejected(t *testing.T) {
	// 先验证 ExecuteAttempts 对流式请求照常返回原始响应（不解析）。
	log := &routeCallLog{}
	adapter := &routeTestAdapter{log: log}
	deps := routeTestDeps(
		routeTestSingleCandidate(7),
		map[int]ChannelAssignment{7: routeTestAssignment(7)},
		log,
		func(int) (*http.Response, error) { return routeResponse(http.StatusOK, "data: {}\n\n"), nil },
		adapter,
		nil,
		0,
	)
	result, err := ExecuteAttempts(context.Background(), routeTestContext(true), deps)
	if err != nil {
		t.Fatalf("ExecuteAttempts 对流式请求应返回原始响应，实际错误 = %v", err)
	}
	defer result.Response.Body.Close()
	if result.Response.StatusCode != http.StatusOK {
		t.Fatalf("原始响应状态 = %d，期望 200", result.Response.StatusCode)
	}

	// 再验证 Route 对 rc.Streaming 明确拒绝，且不产生任何上游调用/预扣。
	log2 := &routeCallLog{}
	adapter2 := &routeTestAdapter{log: log2}
	billing := &recordingBilling{Stub: billingseam.NewStub()}
	deps2 := routeTestDeps(
		routeTestSingleCandidate(7),
		map[int]ChannelAssignment{7: routeTestAssignment(7)},
		log2,
		func(int) (*http.Response, error) { return routeResponse(http.StatusOK, `{}`), nil },
		adapter2,
		billing,
		3,
	)
	_, err = Route(context.Background(), routeTestContext(true), deps2)
	if !errors.Is(err, ErrStreamingInRoute) {
		t.Fatalf("Route 对流式请求的错误 = %v，期望 ErrStreamingInRoute", err)
	}
	if len(log2.httpCalls) != 0 || billing.preConsumeCalls != 0 {
		t.Fatalf("流式误用 Route 不应触达上游/计费：http=%d preConsume=%d", len(log2.httpCalls), billing.preConsumeCalls)
	}
}

// TestRoutePreConsumeFailure：预扣不足返回 FastFailError，且保留 402 语义
// （errors.Is 命中桩哨兵）；未开启会话，不需要也不需要调用 Terminate。
func TestRoutePreConsumeFailure(t *testing.T) {
	log := &routeCallLog{}
	adapter := &routeTestAdapter{log: log}
	billing := &recordingBilling{Stub: billingseam.NewStubWithLimit(10)}
	deps := routeTestDeps(
		routeTestSingleCandidate(7),
		map[int]ChannelAssignment{7: routeTestAssignment(7)},
		log,
		func(int) (*http.Response, error) { return routeResponse(http.StatusOK, `{}`), nil },
		adapter,
		billing,
		3,
	)
	rc := routeTestContext(false)
	rc.QuotaEstimate = routeTestEstimate(100) // 超过上限 10 → 402

	_, err := ExecuteAttempts(context.Background(), rc, deps)
	var fast *FastFailError
	if !errors.As(err, &fast) {
		t.Fatalf("错误 = %T %v，期望 *FastFailError", err, err)
	}
	if !errors.Is(err, billingseam.ErrInsufficientQuota) {
		t.Fatalf("错误 %v 未保留 402 语义（billingseam.ErrInsufficientQuota）", err)
	}
	if len(log.httpCalls) != 0 {
		t.Fatalf("预扣失败后仍发出 %d 次上游请求", len(log.httpCalls))
	}
	if billing.Sessions() != 0 || billing.Reserved() != 0 {
		t.Fatalf("预扣失败不应留下会话：sessions=%d reserved=%d", billing.Sessions(), billing.Reserved())
	}
}

// TestRouteParseFailureReleasesBudget：上游 2xx 但响应解析失败时，Route 必须用
// 空 UsageFact 终结会话（GC-7：ExecuteAttempts 成功后由 Route 承担终结责任）。
func TestRouteParseFailureReleasesBudget(t *testing.T) {
	log := &routeCallLog{}
	adapter := &routeTestAdapter{log: log, parseErr: errors.New("invalid JSON")}
	billing := &recordingBilling{Stub: billingseam.NewStub()}
	deps := routeTestDeps(
		routeTestSingleCandidate(7),
		map[int]ChannelAssignment{7: routeTestAssignment(7)},
		log,
		func(int) (*http.Response, error) { return routeResponse(http.StatusOK, `{`), nil },
		adapter,
		billing,
		3,
	)
	rc := routeTestContext(false)
	rc.QuotaEstimate = routeTestEstimate(100)

	_, err := Route(context.Background(), rc, deps)
	if err == nil {
		t.Fatal("解析失败应返回错误")
	}
	if !billing.Terminated() || billing.Sessions() != 0 || billing.Reserved() != 0 {
		t.Fatalf("解析失败后应释放预算：terminated=%v sessions=%d reserved=%d",
			billing.Terminated(), billing.Sessions(), billing.Reserved())
	}
	if len(billing.terminateFacts) != 1 {
		t.Fatalf("Terminate 调用 %d 次，期望 1", len(billing.terminateFacts))
	}
	if fact := billing.terminateFacts[0]; !reflect.DeepEqual(fact, contract.UsageFact{}) {
		t.Fatalf("失败终结的事实 = %#v，期望空 UsageFact（GC-7）", fact)
	}
}

// TestRouteMaxRetriesZeroMeansDefault 冻结控制器裁决（2026-10-18）：
// deps.MaxRetries 的 Go 零值无法区分「未设置」与「显式 0」，故 0 一律解析为
// GC-5 默认 3（总尝试 4 次）；想要最小预算的调用方显式传 1（总尝试 2 次）。
// 同时覆盖钳制上界（6 → 5 次重试 → 6 次尝试）。
func TestRouteMaxRetriesZeroMeansDefault(t *testing.T) {
	cases := []struct {
		retries  int
		attempts int
	}{
		{retries: 0, attempts: 4}, // 未设置 → 默认 3 次重试 + 首试
		{retries: 1, attempts: 2}, // 显式最小预算
		{retries: 6, attempts: 6}, // 钳制到 5 次重试 + 首试
	}
	for _, tc := range cases {
		t.Run(strconv.Itoa(tc.retries), func(t *testing.T) {
			log := &routeCallLog{}
			adapter := &routeTestAdapter{log: log}
			deps := routeTestDeps(
				routeTestSingleCandidate(7),
				map[int]ChannelAssignment{7: routeTestAssignment(7)},
				log,
				func(int) (*http.Response, error) { return routeResponse(http.StatusInternalServerError, "boom"), nil },
				adapter,
				nil,
				tc.retries,
			)
			_, err := ExecuteAttempts(context.Background(), routeTestContext(false), deps)
			var attemptsErr *AttemptsError
			if !errors.As(err, &attemptsErr) {
				t.Fatalf("错误 = %T %v，期望 *AttemptsError", err, err)
			}
			if attemptsErr.Attempts != tc.attempts {
				t.Fatalf("MaxRetries=%d → Attempts=%d，期望 %d", tc.retries, attemptsErr.Attempts, tc.attempts)
			}
			if len(log.httpCalls) != tc.attempts {
				t.Fatalf("MaxRetries=%d → 上游调用 %d 次，期望 %d", tc.retries, len(log.httpCalls), tc.attempts)
			}
		})
	}
}

// TestRouteDispatchFailureFastFails：分派失败按契约 §4 的 ErrEndpointUnsupported
// 「不重试」快失败；safeError 必须保留 errors.Is 链，且不触达上游、释放预算。
func TestRouteDispatchFailureFastFails(t *testing.T) {
	log := &routeCallLog{}
	adapter := &routeTestAdapter{log: log}
	billing := &recordingBilling{Stub: billingseam.NewStub()}
	deps := routeTestDeps(
		routeTestSingleCandidate(7),
		map[int]ChannelAssignment{7: routeTestAssignment(7)},
		log,
		func(int) (*http.Response, error) {
			t.Fatal("分派失败时不得发出上游请求")
			return nil, errors.New("unreachable")
		},
		adapter,
		billing,
		3,
	)
	deps.Dispatch = func(contract.ChannelType) (contract.Adapter, error) {
		return nil, adapters.ErrEndpointUnsupported
	}
	rc := routeTestContext(false)
	rc.QuotaEstimate = routeTestEstimate(100)

	_, err := ExecuteAttempts(context.Background(), rc, deps)
	var fast *FastFailError
	if !errors.As(err, &fast) {
		t.Fatalf("错误 = %T %v，期望 *FastFailError", err, err)
	}
	if !errors.Is(err, adapters.ErrEndpointUnsupported) {
		t.Fatalf("安全包装丢失了 errors.Is 链：%v", err)
	}
	if len(log.httpCalls) != 0 {
		t.Fatalf("分派失败后发出了 %d 次上游请求", len(log.httpCalls))
	}
	if !billing.Terminated() || billing.Sessions() != 0 || billing.Reserved() != 0 {
		t.Fatalf("分派失败后应释放预算：terminated=%v sessions=%d reserved=%d",
			billing.Terminated(), billing.Sessions(), billing.Reserved())
	}
}

// TestRouteBuildRequestClassification 冻结构造期错误的分类：
// ErrRetryable 类可重试（耗尽后 AttemptsError），参数/配置类与无分类错误快失败。
// 两类都不应触达上游传输。
func TestRouteBuildRequestClassification(t *testing.T) {
	t.Run("retryable class", func(t *testing.T) {
		log := &routeCallLog{}
		adapter := &routeTestAdapter{
			log:      log,
			buildErr: &adapters.Error{Class: contract.ErrRetryable, Err: errors.New("transient")},
		}
		billing := &recordingBilling{Stub: billingseam.NewStub()}
		deps := routeTestDeps(
			routeTestSingleCandidate(7),
			map[int]ChannelAssignment{7: routeTestAssignment(7)},
			log,
			func(int) (*http.Response, error) {
				t.Fatal("构造失败时不得发出上游请求")
				return nil, errors.New("unreachable")
			},
			adapter,
			billing,
			1, // 2 次尝试
		)
		_, err := ExecuteAttempts(context.Background(), routeTestContext(false), deps)
		var attemptsErr *AttemptsError
		if !errors.As(err, &attemptsErr) || attemptsErr.Attempts != 2 {
			t.Fatalf("错误 = %v，期望 AttemptsError{Attempts:2}", err)
		}
		var retryable *RetryableError
		if !errors.As(err, &retryable) {
			t.Fatalf("错误链上应有 *RetryableError：%v", err)
		}
		if !billing.Terminated() {
			t.Fatal("重试耗尽后未 Terminate（GC-7）")
		}
	})

	t.Run("fast-fail class", func(t *testing.T) {
		log := &routeCallLog{}
		adapter := &routeTestAdapter{
			log:      log,
			buildErr: &adapters.Error{Class: contract.ErrParamInvalid, Err: errors.New("bad param")},
		}
		billing := &recordingBilling{Stub: billingseam.NewStub()}
		deps := routeTestDeps(
			routeTestSingleCandidate(7),
			map[int]ChannelAssignment{7: routeTestAssignment(7)},
			log,
			func(int) (*http.Response, error) {
				t.Fatal("构造失败时不得发出上游请求")
				return nil, errors.New("unreachable")
			},
			adapter,
			billing,
			3,
		)
		_, err := ExecuteAttempts(context.Background(), routeTestContext(false), deps)
		var fast *FastFailError
		if !errors.As(err, &fast) {
			t.Fatalf("错误 = %T %v，期望 *FastFailError", err, err)
		}
		if len(log.buildCalls) != 1 {
			t.Fatalf("参数错误尝试 %d 次，期望 1（不重试）", len(log.buildCalls))
		}
		if !billing.Terminated() {
			t.Fatal("快失败后未 Terminate（GC-7）")
		}
	})

	t.Run("unclassified", func(t *testing.T) {
		log := &routeCallLog{}
		adapter := &routeTestAdapter{log: log, buildErr: errors.New("boom")}
		deps := routeTestDeps(
			routeTestSingleCandidate(7),
			map[int]ChannelAssignment{7: routeTestAssignment(7)},
			log,
			func(int) (*http.Response, error) {
				t.Fatal("构造失败时不得发出上游请求")
				return nil, errors.New("unreachable")
			},
			adapter,
			nil,
			3,
		)
		_, err := ExecuteAttempts(context.Background(), routeTestContext(false), deps)
		var fast *FastFailError
		if !errors.As(err, &fast) {
			t.Fatalf("无分类构造错误 = %T %v，期望 *FastFailError", err, err)
		}
		if len(log.buildCalls) != 1 {
			t.Fatalf("无分类构造错误尝试 %d 次，期望 1", len(log.buildCalls))
		}
	})
}

// TestRouteResolveChannelFailure 覆盖渠道解析失败的两条语义：
//  1. 换下一个候选继续（解析失败消耗一轮预算）；
//  2. 失败轮的渠道组必须清空，不能把上一轮的 APIBase/Credential 留在上下文里。
func TestRouteResolveChannelFailure(t *testing.T) {
	t.Run("fail over to next candidate", func(t *testing.T) {
		log := &routeCallLog{}
		adapter := &routeTestAdapter{log: log}
		// 只解析渠道 2；渠道 1 的缺失由 routeTestDeps 的解析器报错。
		deps := routeTestDeps(
			[]contract.ChannelCandidate{{ChannelID: 1, Priority: 10}, {ChannelID: 2, Priority: 5}},
			map[int]ChannelAssignment{2: routeTestAssignment(2)},
			log,
			func(int) (*http.Response, error) { return routeResponse(http.StatusOK, `{}`), nil },
			adapter,
			nil,
			3,
		)
		result, err := ExecuteAttempts(context.Background(), routeTestContext(false), deps)
		if err != nil {
			t.Fatalf("解析失败应换下一个候选，实际错误 = %v", err)
		}
		defer result.Response.Body.Close()
		if result.Candidate.ChannelID != 2 {
			t.Fatalf("成功候选 = %d，期望 2", result.Candidate.ChannelID)
		}
		if got := fmt.Sprint(log.buildCalls); got != "[2]" {
			t.Fatalf("BuildRequest 序列 = %v，期望 [2]（渠道 1 解析失败不应构造请求）", log.buildCalls)
		}
	})

	t.Run("clears stale channel group", func(t *testing.T) {
		log := &routeCallLog{}
		adapter := &routeTestAdapter{log: log}
		billing := &recordingBilling{Stub: billingseam.NewStub()}
		// 第 1 轮：渠道 1 解析成功→500；第 2 轮：渠道 2 解析失败→清空渠道组。
		deps := routeTestDeps(
			[]contract.ChannelCandidate{{ChannelID: 1, Priority: 10}, {ChannelID: 2, Priority: 5}},
			map[int]ChannelAssignment{1: routeTestAssignment(1)},
			log,
			func(int) (*http.Response, error) { return routeResponse(http.StatusInternalServerError, "boom"), nil },
			adapter,
			billing,
			1, // 2 次尝试
		)
		rc := routeTestContext(false)
		rc.QuotaEstimate = routeTestEstimate(100)

		_, err := ExecuteAttempts(context.Background(), rc, deps)
		var attemptsErr *AttemptsError
		if !errors.As(err, &attemptsErr) || attemptsErr.Attempts != 2 {
			t.Fatalf("错误 = %v，期望 AttemptsError{Attempts:2}", err)
		}
		if rc.ChannelID != 2 {
			t.Fatalf("失败轮的 ChannelID = %d，期望 2（候选 ID 仍应保留）", rc.ChannelID)
		}
		if rc.APIBase != "" || rc.Credential != "" || rc.UpstreamModel != "" ||
			rc.GroupRatio != 0 || rc.MultiKeyIndex != 0 || !rc.CreatedTime.IsZero() ||
			rc.ChannelType != contract.ChannelTypeUnknown {
			t.Fatalf("失败轮残留了上一轮的渠道组：%#v", rc)
		}
		if !billing.Terminated() {
			t.Fatal("解析失败耗尽后未 Terminate（GC-7）")
		}
	})
}

// TestRoutePinSingleAttempt：relay-01 §6.2 的「固定渠道单次尝试」——pin 候选
// 携带 PinRetrySingleAttempt 时，首轮失败即停止，不得换到其他候选。
func TestRoutePinSingleAttempt(t *testing.T) {
	log := &routeCallLog{}
	adapter := &routeTestAdapter{log: log}
	billing := &recordingBilling{Stub: billingseam.NewStub()}
	rc := routeTestContext(false)
	rc.AttemptPlan = &contract.AttemptPlan{Candidates: []contract.ChannelCandidate{
		{ChannelID: 1, Priority: 1},
		{ChannelID: 2, Priority: 10, Constraints: contract.ChannelConstraints{
			PinChannelID: 2,
			RetryMode:    contract.PinRetrySingleAttempt,
		}},
	}}
	deps := routeTestDeps(
		nil,
		map[int]ChannelAssignment{1: routeTestAssignment(1), 2: routeTestAssignment(2)},
		log,
		func(channelID int) (*http.Response, error) {
			if channelID == 2 {
				return routeResponse(http.StatusInternalServerError, "boom"), nil
			}
			t.Fatal("pin single_attempt 不得换渠道重试")
			return routeResponse(http.StatusOK, `{}`), nil
		},
		adapter,
		billing,
		3,
	)

	_, err := ExecuteAttempts(context.Background(), rc, deps)
	var attemptsErr *AttemptsError
	if !errors.As(err, &attemptsErr) {
		t.Fatalf("错误 = %T %v，期望 *AttemptsError", err, err)
	}
	if attemptsErr.Attempts != 1 {
		t.Fatalf("Attempts = %d，期望 1（pin single_attempt）", attemptsErr.Attempts)
	}
	if got := fmt.Sprint(log.httpCalls); got != "[2]" {
		t.Fatalf("尝试序列 = %v，期望 [2]", log.httpCalls)
	}
	if !billing.Terminated() || billing.Sessions() != 0 {
		t.Fatalf("单次尝试失败后应释放预算：terminated=%v sessions=%d", billing.Terminated(), billing.Sessions())
	}
}

// TestRouteFixedNoRetryWinsOverChannelErrorClass 冻结控制器裁决（2026-10-18）：
// M2 有意偏离 relay-01 §6.3 第 1 步——即便适配器把 400 归为 ErrChannelError
// （§6.3 本应「最先判定 = 强制可重试」），R-P13 的固定不重试集合仍然优先。
// 分类本身保留在 rc.LastError.IsChannelErr 里，供 M3 的自动禁用使用。
func TestRouteFixedNoRetryWinsOverChannelErrorClass(t *testing.T) {
	log := &routeCallLog{}
	adapter := &routeTestAdapter{log: log, class: contract.ErrChannelError}
	billing := &recordingBilling{Stub: billingseam.NewStub()}
	deps := routeTestDeps(
		[]contract.ChannelCandidate{{ChannelID: 1, Priority: 10}, {ChannelID: 2, Priority: 5}},
		map[int]ChannelAssignment{1: routeTestAssignment(1), 2: routeTestAssignment(2)},
		log,
		func(int) (*http.Response, error) { return routeResponse(http.StatusBadRequest, "channel error"), nil },
		adapter,
		billing,
		3,
	)
	rc := routeTestContext(false)

	_, err := ExecuteAttempts(context.Background(), rc, deps)
	var fast *FastFailError
	if !errors.As(err, &fast) {
		t.Fatalf("错误 = %T %v，期望 *FastFailError（固定集合优先）", err, err)
	}
	if len(log.httpCalls) != 1 {
		t.Fatalf("400 被归为 ErrChannelError 时尝试 %d 次，期望 1（M2 固定集合优先）", len(log.httpCalls))
	}
	if rc.LastError == nil || !rc.LastError.IsChannelErr || !rc.LastError.SkipRetry {
		t.Fatalf("rc.LastError = %#v，期望 IsChannelErr=true 且 SkipRetry=true", rc.LastError)
	}
	if !billing.Terminated() {
		t.Fatal("快失败后未 Terminate（GC-7）")
	}
}
