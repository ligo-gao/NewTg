package httpapi

// W8/MJ-d 提交重试/超时矩阵的单元验收（全内存 + 假上游）。
//
// 设计冻结（relay-05 §6.2、契约 §4.7）：绘图提交**单次尝试、不重试**。
// 本文件把矩阵钉死在「无论错误是否可重试，网关只出站一次、失败全额退款」：
//
//   - 传输超时（可重试类）→ 502，单次上游请求，hold 全额退款；
//   - 上游 code 30（分组饱和，客户端可退避重试）→ 429，同样单次；
//   - 上游 code 24（敏感词，不可重试）→ 400，同样单次。
//
// 「客户端用同一个幂等键重试」是另一条路径，见 handlers_mj_idempotency_test.go：
// 失败会 Abandon 键，重试重新执行；成功则回放首次结果，不重复建任务/扣费。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/mj"
	"newtg/gateway/internal/tasks"
)

// newMJRetryMux 构造只挂提交面的 handler（固定单渠道）。
func newMJRetryMux(t *testing.T, upstreamURL string, client *mj.SubmitClient,
	recorder **taskBillingRecorder) (*http.ServeMux, *fakeMJStore) {

	t.Helper()
	store := newFakeMJStore()
	sealer := NewSecretSealer("mj-handler-test-key")
	tokens, _, _ := newMJTokens()
	normal, cand := sealedMJChannel(t, sealer, 10, mj.ChannelTypeMidjourney,
		upstreamURL, mjAllModels(), 10)
	channels := &fakeMJChannels{
		channels:   map[int64]*tasks.ChannelTarget{10: normal},
		candidates: []tasks.MJChannelCandidate{cand},
	}
	handler := NewMJHandler(MJHandlerConfig{
		Store:    store,
		Channels: channels,
		Sealer:   sealer,
		Tokens:   tokens,
		Billing: func(*contract.RelayContext) contract.BillingSession {
			b := newTaskBillingRecorder()
			b.hold = 50
			*recorder = b
			return b
		},
		Config:      mjConfigured(nil),
		ImageSigner: mj.NewImageSigner("mj-handler-test-key"),
		Client:      client,
		Now:         func() time.Time { return time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC) },
		Uint64N:     func(uint64) uint64 { return 0 },
	})
	mux := http.NewServeMux()
	handler.Register(mux, RelayAuth(tokens, fakeTaskAccess{}, nil))
	return mux, store
}

func serveRetry(t *testing.T, mux http.Handler) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mj/submit/imagine", strings.NewReader(`{"prompt":"cat"}`))
	req.Header.Set("Authorization", "Bearer nkt-mj-user-42")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestMJSubmitMatrixSingleAttemptAndRefund(t *testing.T) {
	cases := []struct {
		name       string
		respond    func(w http.ResponseWriter)
		wantStatus int
		wantDesc   string
		client     *mj.SubmitClient
	}{
		{
			name: "transport timeout is retryable but gateway does not retry",
			respond: func(w http.ResponseWriter) {
				time.Sleep(150 * time.Millisecond)
				_, _ = w.Write([]byte(`{"code":1,"result":"late"}`))
			},
			wantStatus: http.StatusBadGateway,
			wantDesc:   mj.DescriptionUpstreamError,
			// 注入 20ms 客户端超时模拟上游卡死；SubmitClient 内部还会套
			// 60s 冻结超时，短的那个先生效。
			client: mj.NewSubmitClient(&http.Client{Timeout: 20 * time.Millisecond}),
		},
		{
			name: "code 30 group saturated -> 429 single attempt",
			respond: func(w http.ResponseWriter) {
				_, _ = w.Write([]byte(`{"code":30,"description":"busy"}`))
			},
			wantStatus: http.StatusTooManyRequests,
			wantDesc:   mj.DescriptionGroupSaturated,
			client:     mj.NewSubmitClient(nil),
		},
		{
			name: "code 24 sensitive word -> 400 single attempt",
			respond: func(w http.ResponseWriter) {
				_, _ = w.Write([]byte(`{"code":24,"description":"sensitive"}`))
			},
			wantStatus: http.StatusBadRequest,
			wantDesc:   mj.DescriptionSensitiveWord,
			client:     mj.NewSubmitClient(nil),
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				tc.respond(w)
			}))
			defer srv.Close()

			var recorder *taskBillingRecorder
			mux, store := newMJRetryMux(t, srv.URL, tc.client, &recorder)
			rec := serveRetry(t, mux)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d，want %d (body=%s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if rows := store.insertedRows(); len(rows) != 0 {
				t.Fatalf("失败提交落了 %d 条任务行（R-MJ-13：不落行）", len(rows))
			}
			if got := decodeMJEnvelope(t, rec)["description"]; got != tc.wantDesc {
				t.Fatalf("description = %v，want %s", got, tc.wantDesc)
			}
			if got := calls.Load(); got != 1 {
				t.Fatalf("upstream calls = %d，want 1（提交不重试）", got)
			}
			if recorder == nil {
				t.Fatal("billing session was not constructed")
			}
			billingCalls := recorder.ReserveCalls()
			if countCall(billingCalls, "preconsume") != 1 || countCall(billingCalls, "refund") != 1 {
				t.Fatalf("billing calls = %v，want 1 preconsume + 1 refund", billingCalls)
			}
			if countCall(billingCalls, "detach") != 0 {
				t.Fatalf("失败提交不得 detach: %v", billingCalls)
			}
		})
	}
}

// TestMJTimeoutCeilingsAreFrozen 断言冻结的超时常量（relay-05 §6.1）。
//
// 变异点：有人把 60/30/15 改成插件族的 30s/1440m 或统一值 → 本用例红。
func TestMJTimeoutCeilingsAreFrozen(t *testing.T) {
	if mj.SubmitTimeout != 60*time.Second {
		t.Fatalf("SubmitTimeout = %v，want 60s", mj.SubmitTimeout)
	}
	if mj.ImageSeedTimeout != 30*time.Second {
		t.Fatalf("ImageSeedTimeout = %v，want 30s", mj.ImageSeedTimeout)
	}
	if mj.QueryTimeout != 15*time.Second {
		t.Fatalf("QueryTimeout = %v，want 15s", mj.QueryTimeout)
	}
	if tasks.DefaultMJTaskTimeout != time.Hour {
		t.Fatalf("MJ 卡住兜底 = %v，want 1h（与插件 1440m 不同族）", tasks.DefaultMJTaskTimeout)
	}
}
