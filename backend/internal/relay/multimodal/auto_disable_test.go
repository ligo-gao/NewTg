package multimodal

// W7/M4-a：多模态非 realtime 端点与 chat 共用 ExecuteAttempts，因此自动禁用
// 失败上报也必须在这里生效（调研 §5.1 的可达路径）。本用例只验证 dispatcher
// 把 Options.OnAttemptFailure 传进 RouteDeps，判定逻辑不在本包。

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/relay"
	"newtg/gateway/internal/relay/adapters"
)

func TestMultimodalReportsAttemptFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid api key"}`))
	}))
	t.Cleanup(server.Close)

	billing := &fakeBilling{}
	var failures []attemptFailureRecord
	dispatcher := NewDispatcher(Options{
		Candidates: func(context.Context, *contract.RelayContext) ([]contract.ChannelCandidate, error) {
			return []contract.ChannelCandidate{{ChannelID: 4}}, nil
		},
		ResolveChannel: func(context.Context, *contract.RelayContext, int) (relay.ChannelAssignment, error) {
			return relay.ChannelAssignment{
				ChannelID:     4,
				ChannelType:   contract.ChannelTypeOpenAI,
				APIBase:       server.URL,
				Credential:    "test-credential",
				UpstreamModel: "upstream-model",
				AutoBan:       true,
			}, nil
		},
		Dispatch: adapters.Dispatch,
		Billing:  func(*contract.RelayContext) contract.BillingSession { return billing },
		Client:   server.Client(),
		OnAttemptFailure: func(_ context.Context, channelID int, autoBan bool,
			class contract.ErrorClass, statusCode int, body []byte) {
			failures = append(failures, attemptFailureRecord{
				channelID: channelID, autoBan: autoBan, class: class,
				statusCode: statusCode, body: string(body),
			})
		},
		WriteError: func(w http.ResponseWriter, requestID string, err error) {
			writeOpenAIError(w, http.StatusBadGateway, requestID, "upstream_error", "upstream request failed")
		},
	})

	body := []byte(`{"model":"m","prompt":"p"}`)
	request := httptest.NewRequest(http.MethodPost, "/v1/images/generations", bytes.NewReader(body))
	recorder := httptest.NewRecorder()
	dispatcher.Handle(recorder, request, &contract.RelayContext{
		RequestID:   "req-m4a",
		OriginModel: "m",
		Request:     &contract.Request{Model: "m", Raw: body},
	}, contract.EndpointImageGenerate)

	if len(failures) != 1 {
		t.Fatalf("上报次数 = %d, want 1: %+v", len(failures), failures)
	}
	got := failures[0]
	if got.channelID != 4 || !got.autoBan || got.class != contract.ErrUpstreamAuth ||
		got.statusCode != http.StatusUnauthorized {
		t.Fatalf("上报事实 = %+v", got)
	}
	if got.body != `{"error":"invalid api key"}` {
		t.Fatalf("body = %q", got.body)
	}
}

type attemptFailureRecord struct {
	channelID  int
	autoBan    bool
	class      contract.ErrorClass
	statusCode int
	body       string
}
