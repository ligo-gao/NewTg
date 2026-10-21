package relay

// W7/M4-a：执行层失败上报接缝的行为测试。
//
// 覆盖 brief 要求的「A 失败、B 成功仍上报 A」「pin 单次仍上报」「传输错误/
// 取消不上报」「body 有界」「autoBan 透传」。判定逻辑（是否真的禁用）在
// internal/channelhealth 的测试里；本文件只锁执行层的**事实完整性**。

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"newtg/gateway/internal/contract"
)

type attemptFailureRecord struct {
	channelID  int
	autoBan    bool
	class      contract.ErrorClass
	statusCode int
	body       string
}

type failureRecorder struct {
	mu      sync.Mutex
	records []attemptFailureRecord
}

func (r *failureRecorder) callback(_ context.Context, channelID int, autoBan bool,
	class contract.ErrorClass, statusCode int, body []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, attemptFailureRecord{
		channelID:  channelID,
		autoBan:    autoBan,
		class:      class,
		statusCode: statusCode,
		body:       string(body),
	})
}

func (r *failureRecorder) snapshot() []attemptFailureRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]attemptFailureRecord(nil), r.records...)
}

// TestExecuteAttemptsReportsAuthFailure：401 快失败路径必须上报本轮渠道，
// 且携带 autoBan/class/status/body。
func TestExecuteAttemptsReportsAuthFailure(t *testing.T) {
	log := &routeCallLog{}
	adapter := &routeTestAdapter{log: log, class: contract.ErrUpstreamAuth}
	recorder := &failureRecorder{}
	deps := routeTestDeps(
		[]contract.ChannelCandidate{{ChannelID: 11, Priority: 10}},
		map[int]ChannelAssignment{11: {ChannelID: 11, AutoBan: true}},
		log,
		func(int) (*http.Response, error) {
			return routeResponse(http.StatusUnauthorized, `{"error":"invalid api key"}`), nil
		},
		adapter, nil, 3,
	)
	deps.OnAttemptFailure = recorder.callback

	if _, err := ExecuteAttempts(context.Background(), routeTestContext(false), deps); err == nil {
		t.Fatal("401 应快速失败")
	}
	records := recorder.snapshot()
	if len(records) != 1 {
		t.Fatalf("上报次数 = %d, want 1", len(records))
	}
	got := records[0]
	if got.channelID != 11 || !got.autoBan || got.class != contract.ErrUpstreamAuth ||
		got.statusCode != http.StatusUnauthorized || got.body != `{"error":"invalid api key"}` {
		t.Fatalf("上报事实 = %+v", got)
	}
}

// TestExecuteAttemptsReportsFailedChannelBeforeRetrySuccess：候选 A 返回 500、
// 候选 B 返回 200 时，上报必须只包含 A，且整笔请求仍然成功。
//
// 这条用例锁住「接缝必须在每轮非 2xx 处，而不是最终错误处」——若只上报最终
// 错误，A 的失败会随 B 的成功完全丢失（变异点）。
func TestExecuteAttemptsReportsFailedChannelBeforeRetrySuccess(t *testing.T) {
	log := &routeCallLog{}
	adapter := &routeTestAdapter{log: log}
	recorder := &failureRecorder{}
	deps := routeTestDeps(
		[]contract.ChannelCandidate{{ChannelID: 1, Priority: 10}, {ChannelID: 2, Priority: 5}},
		map[int]ChannelAssignment{1: {ChannelID: 1, AutoBan: true}, 2: {ChannelID: 2, AutoBan: true}},
		log,
		func(channelID int) (*http.Response, error) {
			if channelID == 1 {
				return routeResponse(http.StatusInternalServerError, `{"error":"boom"}`), nil
			}
			return routeResponse(http.StatusOK, `{}`), nil
		},
		adapter, nil, 3,
	)
	deps.OnAttemptFailure = recorder.callback

	result, err := ExecuteAttempts(context.Background(), routeTestContext(false), deps)
	if err != nil {
		t.Fatalf("500 后应重试并成功: %v", err)
	}
	defer result.Response.Body.Close()
	records := recorder.snapshot()
	if len(records) != 1 {
		t.Fatalf("上报次数 = %d, want 1（只上报失败渠道 A）: %+v", len(records), records)
	}
	if records[0].channelID != 1 || records[0].statusCode != http.StatusInternalServerError {
		t.Fatalf("上报事实 = %+v, want channel=1 status=500", records[0])
	}
	if got := len(log.httpCalls); got != 2 {
		t.Fatalf("尝试次数 = %d, want 2", got)
	}
}

// TestExecuteAttemptsDoesNotReportSuccess：2xx 不上报。
func TestExecuteAttemptsDoesNotReportSuccess(t *testing.T) {
	log := &routeCallLog{}
	recorder := &failureRecorder{}
	deps := routeTestDeps(
		routeTestSingleCandidate(5),
		map[int]ChannelAssignment{5: {ChannelID: 5, AutoBan: true}},
		log,
		func(int) (*http.Response, error) { return routeResponse(http.StatusOK, `{}`), nil },
		&routeTestAdapter{log: log}, nil, 3,
	)
	deps.OnAttemptFailure = recorder.callback
	result, err := ExecuteAttempts(context.Background(), routeTestContext(false), deps)
	if err != nil {
		t.Fatalf("ExecuteAttempts: %v", err)
	}
	result.Response.Body.Close()
	if records := recorder.snapshot(); len(records) != 0 {
		t.Fatalf("2xx 不应上报: %+v", records)
	}
}

// TestExecuteAttemptsDoesNotReportTransportFailure：连接失败/传输错误不是
// 「上游对渠道表态」，不得上报（R-M4-01 C：传输层错误一律不禁用）。
func TestExecuteAttemptsDoesNotReportTransportFailure(t *testing.T) {
	log := &routeCallLog{}
	recorder := &failureRecorder{}
	deps := routeTestDeps(
		routeTestSingleCandidate(6),
		map[int]ChannelAssignment{6: {ChannelID: 6, AutoBan: true}},
		log,
		func(int) (*http.Response, error) { return nil, errors.New("dial tcp: connection refused") },
		&routeTestAdapter{log: log}, nil, 1,
	)
	deps.OnAttemptFailure = recorder.callback
	if _, err := ExecuteAttempts(context.Background(), routeTestContext(false), deps); err == nil {
		t.Fatal("传输错误应耗尽重试并失败")
	}
	if records := recorder.snapshot(); len(records) != 0 {
		t.Fatalf("传输错误不得上报: %+v", records)
	}
}

// TestExecuteAttemptsDoesNotReportCanceledContext：已取消的请求在选路前
// 返回 0 次尝试，不得上报。
func TestExecuteAttemptsDoesNotReportCanceledContext(t *testing.T) {
	log := &routeCallLog{}
	recorder := &failureRecorder{}
	deps := routeTestDeps(
		routeTestSingleCandidate(7),
		map[int]ChannelAssignment{7: {ChannelID: 7, AutoBan: true}},
		log,
		func(int) (*http.Response, error) {
			t.Fatal("已取消的请求不得发出上游调用")
			return nil, nil
		},
		&routeTestAdapter{log: log}, nil, 3,
	)
	deps.OnAttemptFailure = recorder.callback
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ExecuteAttempts(ctx, routeTestContext(false), deps); err == nil {
		t.Fatal("已取消的请求应失败")
	}
	if records := recorder.snapshot(); len(records) != 0 {
		t.Fatalf("ctx 取消不得上报: %+v", records)
	}
}

// TestExecuteAttemptsPinnedSingleAttemptReports：pin + single_attempt 的
// 单次尝试失败仍要上报 pin 渠道（不因提前停止而丢失事实）。
func TestExecuteAttemptsPinnedSingleAttemptReports(t *testing.T) {
	log := &routeCallLog{}
	adapter := &routeTestAdapter{log: log}
	recorder := &failureRecorder{}
	deps := routeTestDeps(
		[]contract.ChannelCandidate{{
			ChannelID:   8,
			Priority:    10,
			Constraints: contract.ChannelConstraints{PinChannelID: 8, RetryMode: contract.PinRetrySingleAttempt},
		}},
		map[int]ChannelAssignment{8: {ChannelID: 8, AutoBan: true}},
		log,
		func(int) (*http.Response, error) {
			return routeResponse(http.StatusBadGateway, `{"error":"bad gateway"}`), nil
		},
		adapter, nil, 3,
	)
	deps.OnAttemptFailure = recorder.callback
	if _, err := ExecuteAttempts(context.Background(), routeTestContext(false), deps); err == nil {
		t.Fatal("pin 单次尝试应失败")
	}
	records := recorder.snapshot()
	if len(records) != 1 || records[0].channelID != 8 || records[0].statusCode != http.StatusBadGateway {
		t.Fatalf("上报事实 = %+v, want channel=8 status=502", records)
	}
}

// TestExecuteAttemptsPropagatesAutoBanFalse：auto_ban=false 的渠道仍上报事实
// （判定层需要看到它），但 autoBan 字段必须为 false，让判定返回不禁用。
func TestExecuteAttemptsPropagatesAutoBanFalse(t *testing.T) {
	log := &routeCallLog{}
	recorder := &failureRecorder{}
	deps := routeTestDeps(
		routeTestSingleCandidate(9),
		map[int]ChannelAssignment{9: {ChannelID: 9, AutoBan: false}},
		log,
		func(int) (*http.Response, error) { return routeResponse(http.StatusForbidden, `{}`), nil },
		&routeTestAdapter{log: log, class: contract.ErrUpstreamAuth}, nil, 3,
	)
	deps.OnAttemptFailure = recorder.callback
	if _, err := ExecuteAttempts(context.Background(), routeTestContext(false), deps); err == nil {
		t.Fatal("403 应快速失败")
	}
	records := recorder.snapshot()
	if len(records) != 1 || records[0].autoBan {
		t.Fatalf("autoBan 透传错误: %+v", records)
	}
}

// TestExecuteAttemptsBoundsReportedBody：上报的 body 必须与执行层读取上限
// 一致（64 KiB），不得把超大错误体交给上层/日志（GC-6）。
func TestExecuteAttemptsBoundsReportedBody(t *testing.T) {
	log := &routeCallLog{}
	recorder := &failureRecorder{}
	oversized := make([]byte, maxErrorBodyBytes+1024)
	for i := range oversized {
		oversized[i] = 'x'
	}
	deps := routeTestDeps(
		routeTestSingleCandidate(10),
		map[int]ChannelAssignment{10: {ChannelID: 10, AutoBan: true}},
		log,
		func(int) (*http.Response, error) {
			return routeResponse(http.StatusInternalServerError, string(oversized)), nil
		},
		&routeTestAdapter{log: log}, nil, 1,
	)
	deps.OnAttemptFailure = recorder.callback
	_, _ = ExecuteAttempts(context.Background(), routeTestContext(false), deps)
	records := recorder.snapshot()
	if len(records) == 0 {
		t.Fatal("非 2xx 应上报")
	}
	if got := len(records[0].body); got != maxErrorBodyBytes {
		t.Fatalf("上报 body 长度 = %d, want %d", got, maxErrorBodyBytes)
	}
}
