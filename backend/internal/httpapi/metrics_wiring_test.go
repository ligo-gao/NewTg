package httpapi

// W10-METRICS 接线级用例：走真实 RelayHandler.routeDeps → relay.ObserveAttempt
// → internal/metrics，证明 C-1（成功不计错误）与 I-2（流式异常记 refunded）
// 不是仅在指标包内部自洽。

import (
	"net/http"
	"net/http/httptest"
	"testing"

	dto "github.com/prometheus/client_model/go"

	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/metrics"
)

// relayMetricCounter 返回符合标签子集的 counter 之和（全局 registry）。
func relayMetricCounter(t *testing.T, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := metrics.Registry().Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, fam := range families {
		if fam.GetName() != name {
			continue
		}
		total := 0.0
		for _, m := range fam.Metric {
			if metricHasLabels(m, labels) {
				total += m.GetCounter().GetValue()
			}
		}
		return total
	}
	return 0
}

func metricHasLabels(m *dto.Metric, labels map[string]string) bool {
	for wantName, wantValue := range labels {
		found := false
		for _, pair := range m.Label {
			if pair.GetName() == wantName {
				found = pair.GetValue() == wantValue
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// TestW10MetricsSuccessAttemptDoesNotCountError：C-1 接线回归。
func TestW10MetricsSuccessAttemptDoesNotCountError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","created":1,"model":"gpt-test",` +
			`"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()

	env := newRelayTestEnv(t, upstream, nil)
	labels := map[string]string{"endpoint": "chat_completions"}
	errorsBefore := relayMetricCounter(t, "newtg_relay_errors_total", labels)
	successBefore := relayMetricCounter(t, "newtg_relay_requests_total",
		map[string]string{"endpoint": "chat_completions", "status": "success"})

	rec := env.doAuthed(t, http.MethodPost, "/v1/chat/completions",
		`{"model":"gpt-test","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	if delta := relayMetricCounter(t, "newtg_relay_errors_total", labels) - errorsBefore; delta != 0 {
		t.Fatalf("errors_total grew by %v on a successful attempt (C-1 regression)", delta)
	}
	if delta := relayMetricCounter(t, "newtg_relay_requests_total",
		map[string]string{"endpoint": "chat_completions", "status": "success"}) - successBefore; delta != 1 {
		t.Fatalf("success status delta = %v, want 1", delta)
	}
}

// TestW10MetricsStreamAbnormalRecordsRefunded：I-2 接线回归。
// 硬错误（Raw 为空）在 HTTP 200 之后发生；最终 status 必须是 refunded，
// 不能因为响应头已写 200 就记 success。
func TestW10MetricsStreamAbnormalRecordsRefunded(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	adapter := &scriptedStreamAdapter{
		url: upstream.URL + "/v1/chat/completions",
		events: []contract.StreamEvent{
			{Kind: contract.EventDelta, Delta: "part"},
			{Kind: contract.EventError}, // 硬错误：终止并退款
		},
	}
	env := newRelayTestEnv(t, upstream, nil)
	env.handler.dispatch = func(contract.ChannelType) (contract.Adapter, error) { return adapter, nil }

	refundedBefore := relayMetricCounter(t, "newtg_relay_requests_total",
		map[string]string{"endpoint": "chat_completions", "status": "refunded"})
	env.doAuthed(t, http.MethodPost, "/v1/chat/completions",
		`{"model":"gpt-test","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	if delta := relayMetricCounter(t, "newtg_relay_requests_total",
		map[string]string{"endpoint": "chat_completions", "status": "refunded"}) - refundedBefore; delta != 1 {
		t.Fatalf("stream abnormal status delta = %v, want 1 (refunded)", delta)
	}
}
