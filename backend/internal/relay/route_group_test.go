package relay

// W7/M2 路由层用例：group=auto 的分组序列、跨组重试、组内独立预算、
// R-63 上限、R-64 倍率存在性校验与 R-66 分组序列（裁决编号见 m2-brief）。
//
// 这些用例只依赖注入式 RouteDeps（R-P4）：候选按 rc.UsingGroup 分组返回，
// 上游由 routeRoundTripper 仿真，因此能精确断言「哪一组、第几次」被调用。

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"testing"

	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/relay/adapters"
	"newtg/gateway/internal/relay/billingseam"
)

// groupCandidateSource 按分组返回候选，并记录执行层实际查询过的分组序列。
type groupCandidateSource struct {
	byGroup map[string][]contract.ChannelCandidate
	queried []string
}

func (s *groupCandidateSource) candidates(_ context.Context, rc *contract.RelayContext) ([]contract.ChannelCandidate, error) {
	s.queried = append(s.queried, rc.UsingGroup)
	return append([]contract.ChannelCandidate(nil), s.byGroup[rc.UsingGroup]...), nil
}

// groupTestDeps 组装跨组用例的依赖：所有渠道共用同一个假适配器，
// 响应由 respond(channelID) 决定。
func groupTestDeps(
	source *groupCandidateSource,
	log *routeCallLog,
	respond func(channelID int) (*http.Response, error),
	billing contract.BillingSession,
	retries, maxGroups int,
	configured func(group string) (bool, error),
) RouteDeps {
	assignments := make(map[int]ChannelAssignment)
	for _, candidates := range source.byGroup {
		for _, candidate := range candidates {
			assignments[candidate.ChannelID] = routeTestAssignment(candidate.ChannelID)
		}
	}
	adapter := &routeTestAdapter{log: log}
	deps := RouteDeps{
		Candidates: source.candidates,
		ResolveChannel: func(_ context.Context, channelID int) (ChannelAssignment, error) {
			assignment, ok := assignments[channelID]
			if !ok {
				return ChannelAssignment{}, fmt.Errorf("test: channel %d not resolvable", channelID)
			}
			return assignment, nil
		},
		Dispatch:      func(contract.ChannelType) (contract.Adapter, error) { return adapter, nil },
		Billing:       billing,
		MaxRetries:    retries,
		MaxAutoGroups: maxGroups,
		Client: &http.Client{Transport: routeRoundTripper(func(r *http.Request) (*http.Response, error) {
			channelID, _ := strconv.Atoi(r.Header.Get(routeTestChannelHeader))
			log.httpCalls = append(log.httpCalls, channelID)
			return respond(channelID)
		})},
	}
	if configured != nil {
		deps.GroupConfigured = func(_ context.Context, group string) (bool, error) {
			return configured(group)
		}
	}
	return deps
}

// autoContext 构造 group=auto 的中继上下文（UsingGroup = AutoGroups[0]）。
func autoContext(groups ...string) *contract.RelayContext {
	rc := routeTestContext(false)
	rc.AutoGroups = append([]string(nil), groups...)
	rc.CrossGroupRetry = true
	if len(groups) > 0 {
		rc.UsingGroup = groups[0]
	}
	return rc
}

func groupCandidates(id int, priority int) []contract.ChannelCandidate {
	return []contract.ChannelCandidate{{ChannelID: id, Priority: priority, Weight: 1, Group: ""}}
}

func failingOrOK(failing map[int]bool) func(int) (*http.Response, error) {
	return func(channelID int) (*http.Response, error) {
		if failing[channelID] {
			return routeResponse(http.StatusInternalServerError, "boom"), nil
		}
		return routeResponse(http.StatusOK, `{}`), nil
	}
}

// TestCrossGroupRetryFallsThroughToNextGroup 覆盖 R-62 与 TC-F-02-14-01：
// g1 全部渠道 500、g2 可用 → 成功；尝试序列 = g1 组内预算，g2 一次。
func TestCrossGroupRetryFallsThroughToNextGroup(t *testing.T) {
	log := &routeCallLog{}
	billing := &recordingBilling{Stub: billingseam.NewStub()}
	source := &groupCandidateSource{byGroup: map[string][]contract.ChannelCandidate{
		"g1": {groupCandidates(1, 10)[0], groupCandidates(2, 5)[0]},
		"g2": groupCandidates(3, 10),
	}}
	deps := groupTestDeps(source, log, failingOrOK(map[int]bool{1: true, 2: true}), billing, 1, 3, nil)
	rc := autoContext("g1", "g2")

	result, err := ExecuteAttempts(context.Background(), rc, deps)
	if err != nil {
		t.Fatalf("ExecuteAttempts() error = %v", err)
	}
	defer result.Response.Body.Close()
	if result.Candidate.ChannelID != 3 {
		t.Fatalf("成功渠道 = %d，期望 3（g2）", result.Candidate.ChannelID)
	}
	if got := fmt.Sprint(log.httpCalls); got != "[1 2 3]" {
		t.Fatalf("上游尝试序列 = %v，期望 [1 2 3]（g1×2 → g2×1）", got)
	}
	if got := fmt.Sprint(source.queried); got != "[g1 g2]" {
		t.Fatalf("候选查询分组 = %v，期望 [g1 g2]（换组必须重新查候选）", got)
	}
	// R-62 补充口径：AttemptIndex 用全局尝试序号（0,1,2），最后一轮成功。
	if rc.AttemptIndex != 2 {
		t.Fatalf("AttemptIndex = %d，期望 2（整笔请求的第 3 次尝试）", rc.AttemptIndex)
	}
	if rc.UsingGroup != "g2" {
		t.Fatalf("UsingGroup = %q，期望 g2（结算按最终组）", rc.UsingGroup)
	}
	if billing.preConsumeCalls != 1 {
		t.Fatalf("PreConsume = %d 次，期望 1（整笔请求只预扣一次）", billing.preConsumeCalls)
	}
	if billing.refundCalls != 0 {
		t.Fatalf("Refund = %d 次，期望 0（成功路径由调用方结算）", billing.refundCalls)
	}
}

// TestCrossGroupRetryDisabledStopsAtFirstGroup 覆盖 TC-F-02-14-02：
// 开关关闭时即使 g1 耗尽也不得触达 g2。
func TestCrossGroupRetryDisabledStopsAtFirstGroup(t *testing.T) {
	log := &routeCallLog{}
	billing := &recordingBilling{Stub: billingseam.NewStub()}
	source := &groupCandidateSource{byGroup: map[string][]contract.ChannelCandidate{
		"g1": {groupCandidates(1, 10)[0], groupCandidates(2, 5)[0]},
		"g2": groupCandidates(3, 10),
	}}
	deps := groupTestDeps(source, log, failingOrOK(map[int]bool{1: true, 2: true}), billing, 1, 3, nil)
	rc := autoContext("g1", "g2")
	rc.CrossGroupRetry = false

	_, err := ExecuteAttempts(context.Background(), rc, deps)
	if err == nil {
		t.Fatal("g1 全部失败且开关关闭时应返回错误")
	}
	var attempts *AttemptsError
	if !errors.As(err, &attempts) {
		t.Fatalf("错误 = %T %v，期望 *AttemptsError", err, err)
	}
	if got := fmt.Sprint(log.httpCalls); got != "[1 2]" {
		t.Fatalf("上游尝试序列 = %v，期望 [1 2]（g2 调用数必须为 0）", got)
	}
	if got := fmt.Sprint(attempts.Groups); got != "[g1]" {
		t.Fatalf("AttemptsError.Groups = %v，期望 [g1]", got)
	}
	if billing.refundCalls != 1 {
		t.Fatalf("Refund = %d 次，期望 1（失败整笔不收费）", billing.refundCalls)
	}
}

// TestCrossGroupEmptyGroupFallsThrough 覆盖 TC-F-02-13-01 / R-62：
// g1 无可用渠道 → 视为耗尽并继续换组到 g2。
func TestCrossGroupEmptyGroupFallsThrough(t *testing.T) {
	log := &routeCallLog{}
	billing := &recordingBilling{Stub: billingseam.NewStub()}
	source := &groupCandidateSource{byGroup: map[string][]contract.ChannelCandidate{
		"g1": nil,
		"g2": groupCandidates(3, 10),
	}}
	deps := groupTestDeps(source, log, failingOrOK(nil), billing, 1, 3, nil)
	rc := autoContext("g1", "g2")

	result, err := ExecuteAttempts(context.Background(), rc, deps)
	if err != nil {
		t.Fatalf("ExecuteAttempts() error = %v", err)
	}
	defer result.Response.Body.Close()
	if got := fmt.Sprint(log.httpCalls); got != "[3]" {
		t.Fatalf("上游尝试序列 = %v，期望 [3]（g1 空集不产生上游调用）", got)
	}
	if rc.UsingGroup != "g2" {
		t.Fatalf("UsingGroup = %q，期望 g2", rc.UsingGroup)
	}
	if billing.preConsumeCalls != 1 {
		t.Fatalf("PreConsume = %d 次，期望 1", billing.preConsumeCalls)
	}
}

// TestCrossGroupPerGroupBudget 覆盖 R-62/R-63 与用例 8：
// retries=1 → 每组恰好 2 次；g1 用满 2 次后才切 g2（若实现成全局预算，
// g2 永远不会被调用）。
func TestCrossGroupPerGroupBudget(t *testing.T) {
	log := &routeCallLog{}
	billing := &recordingBilling{Stub: billingseam.NewStub()}
	source := &groupCandidateSource{byGroup: map[string][]contract.ChannelCandidate{
		"g1": {groupCandidates(1, 10)[0], groupCandidates(2, 5)[0]},
		"g2": groupCandidates(3, 10),
	}}
	deps := groupTestDeps(source, log, failingOrOK(map[int]bool{1: true, 2: true}), billing, 1, 3, nil)
	rc := autoContext("g1", "g2")

	if _, err := ExecuteAttempts(context.Background(), rc, deps); err != nil {
		t.Fatalf("ExecuteAttempts() error = %v", err)
	}
	if got := fmt.Sprint(log.httpCalls); got != "[1 2 3]" {
		t.Fatalf("上游尝试序列 = %v，期望 [1 2 3]（组内预算独立重置）", got)
	}
}

// TestCrossGroupRetryLimit 覆盖 R-63 与用例 9：
// auto=[g1..g4]、上限 3、全部 500 → 仅 g1..g3 被调用，g4 的 transport=0，
// AttemptsError.Groups = [g1 g2 g3]（R-66 的 message 由端点层拼接）。
func TestCrossGroupRetryLimit(t *testing.T) {
	log := &routeCallLog{}
	billing := &recordingBilling{Stub: billingseam.NewStub()}
	source := &groupCandidateSource{byGroup: map[string][]contract.ChannelCandidate{
		"g1": groupCandidates(1, 10),
		"g2": groupCandidates(2, 10),
		"g3": groupCandidates(3, 10),
		"g4": groupCandidates(4, 10),
	}}
	deps := groupTestDeps(source, log, failingOrOK(map[int]bool{1: true, 2: true, 3: true, 4: true}), billing, 1, 3, nil)
	rc := autoContext("g1", "g2", "g3", "g4")

	_, err := ExecuteAttempts(context.Background(), rc, deps)
	var attempts *AttemptsError
	if !errors.As(err, &attempts) {
		t.Fatalf("错误 = %T %v，期望 *AttemptsError", err, err)
	}
	for _, channelID := range log.httpCalls {
		if channelID == 4 {
			t.Fatalf("g4 被调用（尝试序列 %v），R-63 上限必须阻止第 4 个分组", log.httpCalls)
		}
	}
	if got := fmt.Sprint(attempts.Groups); got != "[g1 g2 g3]" {
		t.Fatalf("AttemptsError.Groups = %v，期望 [g1 g2 g3]", got)
	}
	if attempts.Attempts != 6 {
		t.Fatalf("Attempts = %d，期望 6（3 组 × 组内预算 2）", attempts.Attempts)
	}
}

// TestCrossGroupHardStopOn400 覆盖 R-62 硬停止：400 不换渠道、不换组。
func TestCrossGroupHardStopOn400(t *testing.T) {
	log := &routeCallLog{}
	billing := &recordingBilling{Stub: billingseam.NewStub()}
	source := &groupCandidateSource{byGroup: map[string][]contract.ChannelCandidate{
		"g1": groupCandidates(1, 10),
		"g2": groupCandidates(2, 10),
	}}
	deps := groupTestDeps(source, log, func(int) (*http.Response, error) {
		return routeResponse(http.StatusBadRequest, "bad request"), nil
	}, billing, 3, 3, nil)
	rc := autoContext("g1", "g2")

	_, err := ExecuteAttempts(context.Background(), rc, deps)
	var fast *FastFailError
	if !errors.As(err, &fast) {
		t.Fatalf("错误 = %T %v，期望 *FastFailError（400 固定不重试）", err, err)
	}
	if got := fmt.Sprint(log.httpCalls); got != "[1]" {
		t.Fatalf("上游尝试序列 = %v，期望 [1]（g2 调用数必须为 0）", got)
	}
}

// TestPinDoesNotCrossGroups 覆盖 R-62：pin 只在首组解析，单次尝试后不得换组。
func TestPinDoesNotCrossGroups(t *testing.T) {
	log := &routeCallLog{}
	billing := &recordingBilling{Stub: billingseam.NewStub()}
	pinned := groupCandidates(1, 10)
	pinned[0].Constraints.PinChannelID = 1
	pinned[0].Constraints.RetryMode = contract.PinRetrySingleAttempt
	source := &groupCandidateSource{byGroup: map[string][]contract.ChannelCandidate{
		"g1": pinned,
		"g2": groupCandidates(2, 10),
	}}
	deps := groupTestDeps(source, log, failingOrOK(map[int]bool{1: true}), billing, 3, 3, nil)
	rc := autoContext("g1", "g2")

	if _, err := ExecuteAttempts(context.Background(), rc, deps); err == nil {
		t.Fatal("pin 单次尝试失败后应返回错误")
	}
	if got := fmt.Sprint(log.httpCalls); got != "[1]" {
		t.Fatalf("上游尝试序列 = %v，期望 [1]（pin 不跨组）", got)
	}
}

// TestGroupRatioMissingBeforePreConsume 覆盖 R-64 / I-25：
// 首组倍率缺失必须在 PreConsume 之前 403 语义快失败——零预扣、零上游。
func TestGroupRatioMissingBeforePreConsume(t *testing.T) {
	log := &routeCallLog{}
	billing := &recordingBilling{Stub: billingseam.NewStub()}
	source := &groupCandidateSource{byGroup: map[string][]contract.ChannelCandidate{
		"g1": groupCandidates(1, 10),
	}}
	deps := groupTestDeps(source, log, failingOrOK(nil), billing, 1, 3, func(group string) (bool, error) {
		return false, nil
	})
	rc := autoContext("g1")

	_, err := ExecuteAttempts(context.Background(), rc, deps)
	if !errors.Is(err, ErrGroupRatioMissing) {
		t.Fatalf("错误 = %v，期望 ErrGroupRatioMissing", err)
	}
	var fast *FastFailError
	if !errors.As(err, &fast) {
		t.Fatalf("错误 = %T，期望 *FastFailError", err)
	}
	if billing.preConsumeCalls != 0 || billing.refundCalls != 0 {
		t.Fatalf("PreConsume=%d Refund=%d，期望 0/0（预扣前 fail-closed）",
			billing.preConsumeCalls, billing.refundCalls)
	}
	if len(log.httpCalls) != 0 {
		t.Fatalf("上游调用 = %v，期望 0（零上游）", log.httpCalls)
	}
}

// TestGroupRatioMissingOnSwitchRefunds 覆盖用例 12 的路由层半边：
// 切换目标组倍率缺失 → 403 语义快失败，且已发生的预扣全额退回。
func TestGroupRatioMissingOnSwitchRefunds(t *testing.T) {
	log := &routeCallLog{}
	billing := &recordingBilling{Stub: billingseam.NewStub()}
	source := &groupCandidateSource{byGroup: map[string][]contract.ChannelCandidate{
		"g1": groupCandidates(1, 10),
		"g2": groupCandidates(2, 10),
	}}
	deps := groupTestDeps(source, log, failingOrOK(map[int]bool{1: true}), billing, 1, 3, func(group string) (bool, error) {
		return group == "g1", nil
	})
	rc := autoContext("g1", "g2")

	_, err := ExecuteAttempts(context.Background(), rc, deps)
	if !errors.Is(err, ErrGroupRatioMissing) {
		t.Fatalf("错误 = %v，期望 ErrGroupRatioMissing", err)
	}
	if billing.preConsumeCalls != 1 {
		t.Fatalf("PreConsume = %d 次，期望 1（首组已预扣）", billing.preConsumeCalls)
	}
	if billing.refundCalls != 1 {
		t.Fatalf("Refund = %d 次，期望 1（预扣全额退回）", billing.refundCalls)
	}
	for _, channelID := range log.httpCalls {
		if channelID == 2 {
			t.Fatalf("g2 上游被调用（序列 %v），R-64 必须在触达上游前拒绝", log.httpCalls)
		}
	}
}

// TestGroupConfiguredStorageErrorFailsClosed 覆盖补充口径 #7：
// 存储查询失败 → 500 语义（ErrGroupCheckFailed），不得当作 false/true 继续。
func TestGroupConfiguredStorageErrorFailsClosed(t *testing.T) {
	log := &routeCallLog{}
	billing := &recordingBilling{Stub: billingseam.NewStub()}
	source := &groupCandidateSource{byGroup: map[string][]contract.ChannelCandidate{
		"g1": groupCandidates(1, 10),
	}}
	deps := groupTestDeps(source, log, failingOrOK(nil), billing, 1, 3, func(group string) (bool, error) {
		return false, errors.New("db down")
	})
	rc := autoContext("g1")

	_, err := ExecuteAttempts(context.Background(), rc, deps)
	if !errors.Is(err, ErrGroupCheckFailed) {
		t.Fatalf("错误 = %v，期望 ErrGroupCheckFailed", err)
	}
	if billing.preConsumeCalls != 0 || len(log.httpCalls) != 0 {
		t.Fatalf("PreConsume=%d upstream=%v，期望 0/空（fail-closed）",
			billing.preConsumeCalls, log.httpCalls)
	}
}

// TestGroupSequence 冻结有效分组序列的边界（R-61/R-63/R-68）。
func TestGroupSequence(t *testing.T) {
	t.Run("开关关闭只返回首组", func(t *testing.T) {
		rc := autoContext("g1", "g2")
		rc.CrossGroupRetry = false
		if got := fmt.Sprint(GroupSequence(rc, 3)); got != "[g1]" {
			t.Fatalf("GroupSequence = %v，期望 [g1]", got)
		}
	})
	t.Run("上限截断", func(t *testing.T) {
		rc := autoContext("g1", "g2", "g3")
		if got := fmt.Sprint(GroupSequence(rc, 2)); got != "[g1 g2]" {
			t.Fatalf("GroupSequence = %v，期望 [g1 g2]", got)
		}
	})
	t.Run("非法上限回退默认 3", func(t *testing.T) {
		rc := autoContext("g1", "g2", "g3", "g4")
		if got := fmt.Sprint(GroupSequence(rc, 0)); got != "[g1 g2 g3]" {
			t.Fatalf("GroupSequence = %v，期望默认上限 3", got)
		}
		if got := fmt.Sprint(GroupSequence(rc, 99)); got != "[g1 g2 g3 g4]" {
			t.Fatalf("GroupSequence(99) = %v，期望 4 个分组（序列长度封顶）", got)
		}
	})
	t.Run("去重且跳过空值与 auto 哨兵", func(t *testing.T) {
		rc := autoContext("g1")
		rc.AutoGroups = []string{"g1", "", "auto", "g2", "g1", "g3"}
		if got := fmt.Sprint(GroupSequence(rc, 10)); got != "[g1 g2 g3]" {
			t.Fatalf("GroupSequence = %v，期望 [g1 g2 g3]", got)
		}
	})
	t.Run("直调方只填 AutoGroups 时首组取首项", func(t *testing.T) {
		rc := routeTestContext(false)
		rc.UsingGroup = ""
		rc.AutoGroups = []string{"g1", "g2"}
		rc.CrossGroupRetry = true
		if got := fmt.Sprint(GroupSequence(rc, 3)); got != "[g1 g2]" {
			t.Fatalf("GroupSequence = %v，期望 [g1 g2]", got)
		}
	})
}

// TestCrossGroupUsesGroupLocalPriorityIndex 覆盖 R-62 复核 Minor-3：
// 切到 g2 后，组内首轮的 priority 映射必须用**组内序号**（i=0 → 最高
// priority），不得沿用全局尝试序号——否则 g2 会从最低优先级开始。
func TestCrossGroupUsesGroupLocalPriorityIndex(t *testing.T) {
	log := &routeCallLog{}
	billing := &recordingBilling{Stub: billingseam.NewStub()}
	source := &groupCandidateSource{byGroup: map[string][]contract.ChannelCandidate{
		"g1": groupCandidates(1, 10),
		"g2": {groupCandidates(2, 10)[0], groupCandidates(3, 5)[0]}, // 2 最高、3 次之
	}}
	deps := groupTestDeps(source, log, failingOrOK(map[int]bool{1: true}), billing, 1, 3, nil)
	rc := autoContext("g1", "g2")

	result, err := ExecuteAttempts(context.Background(), rc, deps)
	if err != nil {
		t.Fatalf("ExecuteAttempts() error = %v", err)
	}
	defer result.Response.Body.Close()
	if result.Candidate.ChannelID != 2 {
		t.Fatalf("g2 首轮选中渠道 %d，期望 2（组内最高 priority）", result.Candidate.ChannelID)
	}
	if got := fmt.Sprint(log.httpCalls); got != "[1 1 2]" {
		t.Fatalf("上游尝试序列 = %v，期望 [1 1 2]（g2 必须从最高 priority 开始）", got)
	}
}

// TestCrossGroupContextCancelBetweenGroups 覆盖 R-62 复核 Minor-4(a)：
// 组间 ctx 取消后不得再切组、不得触达 g2 上游，预扣恰好退款一次。
func TestCrossGroupContextCancelBetweenGroups(t *testing.T) {
	log := &routeCallLog{}
	billing := &recordingBilling{Stub: billingseam.NewStub()}
	source := &groupCandidateSource{byGroup: map[string][]contract.ChannelCandidate{
		"g1": groupCandidates(1, 10),
		"g2": groupCandidates(2, 10),
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g1Calls := 0
	deps := groupTestDeps(source, log, func(channelID int) (*http.Response, error) {
		if channelID == 1 {
			g1Calls++
			if g1Calls == 2 {
				// 组内预算（retries=1 → 2 次）刚好耗尽时取消：外层换组
				// 前的 ctx 检查必须拦住 g2。
				cancel()
			}
		}
		return routeResponse(http.StatusInternalServerError, "boom"), nil
	}, billing, 1, 3, nil)
	rc := autoContext("g1", "g2")

	_, err := ExecuteAttempts(ctx, rc, deps)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("错误 = %v，期望 context.Canceled", err)
	}
	if got := fmt.Sprint(log.httpCalls); got != "[1 1]" {
		t.Fatalf("上游尝试序列 = %v，期望 [1 1]（取消后不得触达 g2）", got)
	}
	if billing.preConsumeCalls != 1 || billing.refundCalls != 1 {
		t.Fatalf("PreConsume=%d Refund=%d，期望 1/1（预扣恰好退款一次）",
			billing.preConsumeCalls, billing.refundCalls)
	}
	var attempts *AttemptsError
	if !errors.As(err, &attempts) {
		t.Fatalf("错误 = %T %v，期望 *AttemptsError", err, err)
	}
	if fmt.Sprint(attempts.Groups) != "[g1]" {
		t.Fatalf("AttemptsError.Groups = %v，期望 [g1]（g2 未被尝试）", attempts.Groups)
	}
}

// TestCrossGroupDispatchFailureFastFails 覆盖 R-62 复核 Minor-4(b)：
// 适配器分派失败（ErrEndpointUnsupported 族）是硬停止——直接 FastFail、
// 不换组、不触达上游。
func TestCrossGroupDispatchFailureFastFails(t *testing.T) {
	log := &routeCallLog{}
	billing := &recordingBilling{Stub: billingseam.NewStub()}
	source := &groupCandidateSource{byGroup: map[string][]contract.ChannelCandidate{
		"g1": groupCandidates(1, 10),
		"g2": groupCandidates(2, 10),
	}}
	deps := groupTestDeps(source, log, failingOrOK(nil), billing, 1, 3, nil)
	deps.Dispatch = func(contract.ChannelType) (contract.Adapter, error) {
		return nil, adapters.ErrEndpointUnsupported
	}
	rc := autoContext("g1", "g2")

	_, err := ExecuteAttempts(context.Background(), rc, deps)
	var fast *FastFailError
	if !errors.As(err, &fast) {
		t.Fatalf("错误 = %T %v，期望 *FastFailError", err, err)
	}
	if !errors.Is(err, adapters.ErrEndpointUnsupported) {
		t.Fatalf("错误链丢失 ErrEndpointUnsupported：%v", err)
	}
	if len(log.httpCalls) != 0 {
		t.Fatalf("分派失败不得触达上游，实际调用 %v", log.httpCalls)
	}
	if billing.preConsumeCalls != 1 || billing.refundCalls != 1 {
		t.Fatalf("PreConsume=%d Refund=%d，期望 1/1", billing.preConsumeCalls, billing.refundCalls)
	}
}

// TestSpecificGroupUnchanged 覆盖用例 14：具体分组（非 auto）令牌即使
// 数据上带 CrossGroupRetry=true，也不得产生分组序列或换组。
func TestSpecificGroupUnchanged(t *testing.T) {
	log := &routeCallLog{}
	billing := &recordingBilling{Stub: billingseam.NewStub()}
	source := &groupCandidateSource{byGroup: map[string][]contract.ChannelCandidate{
		"default": groupCandidates(1, 10),
		"g2":      groupCandidates(2, 10),
	}}
	deps := groupTestDeps(source, log, failingOrOK(map[int]bool{1: true}), billing, 1, 3, nil)
	rc := routeTestContext(false)
	rc.CrossGroupRetry = true // 无 AutoGroups：I-23 的写入期耦合被绕过的防御用例

	_, err := ExecuteAttempts(context.Background(), rc, deps)
	var attempts *AttemptsError
	if !errors.As(err, &attempts) {
		t.Fatalf("错误 = %T %v，期望 *AttemptsError", err, err)
	}
	if attempts.Groups != nil {
		t.Fatalf("Groups = %v，期望 nil（具体分组不产生 R-66 序列）", attempts.Groups)
	}
	if got := fmt.Sprint(log.httpCalls); got != "[1 1]" {
		t.Fatalf("上游尝试序列 = %v，期望 [1 1]（无换组）", got)
	}
}
