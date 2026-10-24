//go:build integration

package httpapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"newtg/gateway/internal/billing"
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/rbac"
	"newtg/gateway/internal/relay"
	"newtg/gateway/internal/relay/adapters"
	"newtg/gateway/internal/relay/ir"
)

// TestIntegrationTriageConversionEvidenceFromRoute 证明请求方向的转换证据由
// **生产路径**产生（final-review Major 1）：
//
//  1. 真实 relay.Route 用 Anthropic 适配器把 OpenAI 请求改写后发往一个返回
//     400 的假上游——请求失败，crossproto.Forward 永远不会执行；
//  2. rc.ConversionChain 必须已经带有 "request" 步；
//  3. 经结算写入 usage_logs 后，triage 端点必须返回非空 conversion 且判 GW-01。
//
// 若移除 route 中的 appendRequestConversion，第 2/3 步都会失败（conversion
// 为空 → UNKNOWN），因此本用例钉住的是生产者，而不是手工 seed 的 other JSON。
func TestIntegrationTriageConversionEvidenceFromRoute(t *testing.T) {
	env := newUsageTestEnv(t)
	userID, _ := ensureUsageTestUser(t, env, "conv")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// 非可重试 4xx：Route 在首轮即快失败，但 BuildRequest 已经完成。
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`)
	}))
	defer upstream.Close()

	req, err := ir.ParseOpenAIRequest([]byte(
		`{"model":"claude-3-5-sonnet","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("ParseOpenAIRequest: %v", err)
	}

	requestID := newRequestID(t)
	rc := &contract.RelayContext{
		RequestID:   requestID,
		UserID:      int(userID),
		OriginModel: "claude-3-5-sonnet",
		RequestPath: "/v1/chat/completions",
		ChannelType: contract.ChannelTypeAnthropic,
		Request:     req,
		UsingGroup:  "default",
	}
	deps := relay.RouteDeps{
		Candidates: func(context.Context, *contract.RelayContext) ([]contract.ChannelCandidate, error) {
			return []contract.ChannelCandidate{{ChannelID: 91, Priority: 10}}, nil
		},
		ResolveChannel: func(context.Context, int) (relay.ChannelAssignment, error) {
			return relay.ChannelAssignment{
				ChannelID:   91,
				ChannelType: contract.ChannelTypeAnthropic,
				APIBase:     upstream.URL,
				Credential:  "sk-test-credential",
				GroupRatio:  1,
			}, nil
		},
		Dispatch: adapters.Dispatch,
		Client:   upstream.Client(),
	}

	if _, err := relay.Route(context.Background(), rc, deps); err == nil {
		t.Fatal("400 上游必须让 Route 返回错误")
	}
	if len(rc.ConversionChain) != 1 {
		t.Fatalf("rc.ConversionChain = %+v, want exactly one request step from the producer", rc.ConversionChain)
	}
	step := rc.ConversionChain[0]
	if step.Stage != "request" || step.From != ir.FormatOpenAIChat || step.To != ir.FormatAnthropicMessages {
		t.Fatalf("request step = %+v, want request openai→anthropic_messages", step)
	}

	// 与 billing.Session.Refund 相同的落库投影：error_class 取自 rc.LastError。
	errorClass, errorMessage := "", ""
	if rc.LastError != nil {
		errorClass = rc.LastError.Class.String()
		errorMessage = rc.LastError.Message
	}
	outcome := billing.SettlementOutcome{
		Status:       billing.UsageLogStatusRefunded,
		ErrorClass:   errorClass,
		ErrorMessage: errorMessage,
	}
	if err := billing.NewDBLogger(env.logsDB).Saturate(context.Background(), nil, rc, outcome); err != nil {
		t.Fatalf("Saturate: %v", err)
	}
	env.reqIDs = append(env.reqIDs, requestID)

	rec, body := env.triage(t, requestID, usageAdminPrincipal(1, rbac.PermLogsRead))
	if rec.Code != http.StatusOK || !body.Found || body.Evidence == nil || body.Verdict == nil {
		t.Fatalf("triage = %d found=%v body=%s", rec.Code, body.Found, rec.Body.String())
	}
	if len(body.Evidence.Conversion) == 0 {
		t.Fatalf("evidence.conversion 为空：生产路径没有记录请求方向转换（%s）", rec.Body.String())
	}
	if got := body.Evidence.Conversion[0]; got["stage"] != "request" {
		t.Fatalf("conversion[0] = %v, want stage request", got)
	}
	if body.Evidence.RouteFrom != "openai" || body.Evidence.RouteTo != "anthropic_messages" {
		t.Fatalf("route = %q→%q, want openai→anthropic_messages", body.Evidence.RouteFrom, body.Evidence.RouteTo)
	}
	if body.Evidence.ChannelType != "anthropic" {
		t.Fatalf("channel_type = %q, want anthropic", body.Evidence.ChannelType)
	}
	if body.Verdict.Owner != "gateway" || body.Verdict.Rule != "GW-01" || body.Verdict.Sub != "protocol_conversion" {
		t.Fatalf("verdict = %+v, want gateway/GW-01/protocol_conversion", *body.Verdict)
	}
}
