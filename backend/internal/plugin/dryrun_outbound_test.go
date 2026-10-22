package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"newtg/gateway/internal/egress"
)

func dryRunManager(t *testing.T, exports map[string]string) *Manager {
	t.Helper()
	source := fixture{Exports: exports}.Source()
	manager := MustNewManager(map[string]string{"sample": source}, nil, Options{MasterEnabled: true})
	if err := manager.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	return manager
}

func TestDryRunAllowOutboundExecutesGuardedRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"id":"probe"}`))
	}))
	defer srv.Close()

	manager := dryRunManager(t, map[string]string{
		"buildSubmitRequest": `function (ctx) {
      return { url: ctx.baseUrl + "/probe?token=SUPERSECRET", method: "GET" };
    }`,
	})
	policy := egress.Policy{
		Mode: egress.ModeBasic, DomainMode: egress.DomainAllowlist,
		ConnectTimeout: time.Second, AllowLoopback: true, AllowAnyPort: true,
	}
	result, err := manager.DryRun(context.Background(), DryRunRequest{
		Key: "sample", Endpoint: "submit", AllowOutbound: true,
		Payload:      map[string]any{"baseUrl": srv.URL},
		egressPolicy: &policy,
	})
	if err != nil {
		t.Fatalf("dryrun: %v", err)
	}
	if !result.Outbound {
		t.Fatalf("result must be marked as outbound: %#v", result)
	}
	out, ok := result.Result.(map[string]any)
	if !ok {
		t.Fatalf("unexpected result: %#v", result.Result)
	}
	if out["kind"] != "outbound" || out["statusCode"] != http.StatusOK {
		t.Fatalf("unexpected outbound result: %#v", out)
	}
	encoded, _ := json.Marshal(out)
	if bytes.Contains(encoded, []byte("SUPERSECRET")) {
		t.Fatalf("dryrun result must not echo URL query secrets: %s", encoded)
	}
	if bytes.Contains(encoded, []byte("/probe")) {
		t.Fatalf("dryrun result must not echo request URL: %s", encoded)
	}
	body, _ := out["body"].(map[string]any)
	if body["id"] != "probe" {
		t.Fatalf("response body not decoded: %#v", out)
	}
}

func TestDryRunAllowOutboundRejectsPrivateTarget(t *testing.T) {
	manager := dryRunManager(t, map[string]string{
		"buildSubmitRequest": `function (ctx) { return { url: ctx.baseUrl + "/probe", method: "GET" }; }`,
	})
	_, err := manager.DryRun(context.Background(), DryRunRequest{
		Key: "sample", Endpoint: "submit", AllowOutbound: true,
		Payload: map[string]any{"baseUrl": "http://127.0.0.1:9"},
	})
	if err == nil {
		t.Fatal("private target must be rejected")
	}
	issues, ok := err.(Issues)
	if !ok || !issues.hasRule("dryrun_outbound_rejected") {
		t.Fatalf("want dryrun_outbound_rejected, got %v", err)
	}
}

func TestDryRunAllowOutboundOnNonDescriptorReturnsStructuredResult(t *testing.T) {
	manager := dryRunManager(t, map[string]string{
		"parseSubmitResponse": `function () { return { taskId: "t1" }; }`,
	})
	_, err := manager.DryRun(context.Background(), DryRunRequest{
		Key: "sample", Endpoint: "submit_response", AllowOutbound: true,
		Payload: map[string]any{"statusCode": 200, "body": "{}"},
	})
	if err == nil {
		t.Fatal("non-descriptor endpoint must not silently 'execute' outbound")
	}
	issues, ok := err.(Issues)
	if !ok || !issues.hasRule("dryrun_outbound_unsupported") {
		t.Fatalf("want dryrun_outbound_unsupported, got %v", err)
	}
	if strings.Contains(err.Error(), "dryrun_outbound_unavailable") {
		t.Fatalf("unavailable error must be gone: %v", err)
	}
}

func TestDryRunAllowOutboundRejectsFilePlaceholders(t *testing.T) {
	manager := dryRunManager(t, map[string]string{
		"buildSubmitRequest": `function (ctx) {
      return { url: ctx.baseUrl + "/probe", method: "POST", body: { f: { __fileRef: "request_file:image", encoding: "base64" } } };
    }`,
	})
	policy := egress.Policy{
		Mode: egress.ModeBasic, DomainMode: egress.DomainAllowlist,
		ConnectTimeout: time.Second, AllowLoopback: true, AllowAnyPort: true,
	}
	_, err := manager.DryRun(context.Background(), DryRunRequest{
		Key: "sample", Endpoint: "submit", AllowOutbound: true,
		Payload: map[string]any{"baseUrl": "http://127.0.0.1:9"}, egressPolicy: &policy,
	})
	if err == nil {
		t.Fatal("file placeholders must be rejected when dryrun has no files")
	}
	issues, ok := err.(Issues)
	if !ok || !issues.hasRule("dryrun_outbound_unsupported") {
		t.Fatalf("want dryrun_outbound_unsupported, got %v", err)
	}
}
