package adapters

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"newtg/gateway/internal/contract"
)

func geminiContext(credential string) *contract.RelayContext {
	return &contract.RelayContext{
		ChannelID:   24,
		ChannelType: contract.ChannelTypeGemini,
		Credential:  credential,
		OriginModel: "gemini-2.5-flash",
	}
}

func geminiRequest(stream bool) *contract.Request {
	return &contract.Request{
		Model:  "gemini-2.5-flash",
		Stream: stream,
		Messages: []contract.Message{{
			Role:    contract.RoleUser,
			Content: []contract.ContentPart{{Kind: contract.PartText, Text: "hi"}},
		}},
	}
}

func TestGeminiBuildRequestURLAndHeaders(t *testing.T) {
	adapter := NewGeminiAdapter(contract.ChannelTypeGemini)
	httpReq, err := adapter.BuildRequest(geminiContext("goog-secret"), geminiRequest(false))
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	want := "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:generateContent"
	if httpReq.URL.String() != want {
		t.Fatalf("URL = %q, want %q", httpReq.URL.String(), want)
	}
	if got := httpReq.Header.Get("x-goog-api-key"); got != "goog-secret" {
		t.Fatalf("x-goog-api-key = %q", got)
	}
	if got := httpReq.Header.Get("Authorization"); got != "" {
		t.Fatalf("Gemini 不得使用 Authorization，得到 %q", got)
	}
	if got := httpReq.Header.Get("Accept"); got != "application/json" {
		t.Fatalf("非流式 Accept = %q", got)
	}

	// 流式：URL 后缀 + alt=sse，且请求体不携带 stream 字段。
	httpReq, err = adapter.BuildRequest(geminiContext("goog-secret"), geminiRequest(true))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(httpReq.URL.Path, ":streamGenerateContent") || httpReq.URL.Query().Get("alt") != "sse" {
		t.Fatalf("流式 URL = %q", httpReq.URL.String())
	}
	if got := httpReq.Header.Get("Accept"); got != "text/event-stream" {
		t.Fatalf("流式 Accept = %q", got)
	}
	body, _ := io.ReadAll(httpReq.Body)
	var built map[string]any
	if err := json.Unmarshal(body, &built); err != nil {
		t.Fatal(err)
	}
	if _, ok := built["stream"]; ok {
		t.Fatalf("Gemini 请求体不得包含 stream：%s", body)
	}
	if _, ok := built["model"]; ok {
		t.Fatalf("Gemini 请求体不得包含 model：%s", body)
	}
}

func TestGeminiBaseURLVersionDedup(t *testing.T) {
	adapter := NewGeminiAdapter(contract.ChannelTypeGemini)
	rc := geminiContext("k")
	rc.APIBase = "https://proxy.example.com/v1beta"
	httpReq, err := adapter.BuildRequest(rc, geminiRequest(false))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(httpReq.URL.Path, "/v1beta/v1beta") {
		t.Fatalf("版本段重复：%q", httpReq.URL.Path)
	}
	if httpReq.URL.Path != "/v1beta/models/gemini-2.5-flash:generateContent" {
		t.Fatalf("URL path = %q", httpReq.URL.Path)
	}
}

func TestGeminiCredentialNotLeakedInErrors(t *testing.T) {
	adapter := NewGeminiAdapter(contract.ChannelTypeGemini)
	const secret = "goog-supersecret"
	rc := geminiContext(secret)
	rc.APIBase = "http://[::bad-host"
	if _, err := adapter.BuildRequest(rc, geminiRequest(false)); err == nil {
		t.Fatal("非法 base URL 必须报错")
	} else if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "bad-host") {
		t.Fatalf("GC-6：错误文本不得包含凭据或 base URL：%v", err)
	}

	resp := &http.Response{
		StatusCode: http.StatusUnauthorized,
		Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"bad key ` + secret + `"}}`)),
	}
	_, _, err := adapter.ParseResponse(rc, resp)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("GC-6：401 错误文本不得泄露凭据：%v", err)
	}
	if class, _ := ClassOf(err); class != contract.ErrUpstreamAuth {
		t.Fatalf("401 分类 = %v", class)
	}
}

func TestGeminiParseResponse(t *testing.T) {
	adapter := NewGeminiAdapter(contract.ChannelTypeGemini)
	body := `{
	  "responseId":"resp_1","modelVersion":"gemini-2.5-flash",
	  "candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"hello"}]},"finishReason":"STOP"}],
	  "usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":4,"totalTokenCount":7,"cachedContentTokenCount":2}
	}`
	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}
	response, usage, err := adapter.ParseResponse(geminiContext("k"), resp)
	if err != nil {
		t.Fatalf("ParseResponse: %v", err)
	}
	if response.FinishReason != "stop" || response.Choices[0].Message.Content[0].Text != "hello" {
		t.Fatalf("response = %+v", response)
	}
	if usage.CacheRead != 2 || usage.PromptIncludesCache {
		t.Fatalf("usage = %+v", usage)
	}
	if _, ok := usage.Raw["cachedContentTokenCount"]; !ok {
		t.Fatalf("usage.Raw 必须保留 Gemini 原键：%v", usage.Raw)
	}
}

func TestGeminiClassifyError(t *testing.T) {
	adapter := NewGeminiAdapter(contract.ChannelTypeGemini)
	cases := map[int]contract.ErrorClass{
		400: contract.ErrParamInvalid,
		401: contract.ErrUpstreamAuth,
		404: contract.ErrParamInvalid,
		429: contract.ErrUpstreamRateLimit,
		500: contract.ErrUpstreamServer,
		503: contract.ErrUpstreamServer,
	}
	for status, want := range cases {
		if got := adapter.ClassifyError(status, nil); got != want {
			t.Fatalf("status %d: got %v, want %v", status, got, want)
		}
	}
}

func TestGeminiStreamEvents(t *testing.T) {
	adapter := NewGeminiAdapter(contract.ChannelTypeGemini)
	sse := strings.Join([]string{
		`data: {"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"he"}]}}],"usageMetadata":{"promptTokenCount":10,"totalTokenCount":10}}`,
		"",
		`data: {"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"llo"},{"thought":true,"text":"thinking"}]}}]}`,
		"",
		`data: {"candidates":[{"index":0,"content":{"role":"model","parts":[{"functionCall":{"name":"lookup","args":{"q":"x"}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"thoughtsTokenCount":2,"totalTokenCount":15,"cachedContentTokenCount":3}}`,
		"",
	}, "\n")
	events := collectStreamEvents(adapter.(contract.StreamAdapter).StreamEvents(geminiContext("k"), bytes.NewBufferString(sse)))

	var text, reasoning strings.Builder
	var toolDeltas []contract.ToolCallDelta
	var usages []contract.Usage
	finishCount := 0
	for _, event := range events {
		switch event.Kind {
		case contract.EventDelta:
			text.WriteString(event.Delta)
			reasoning.WriteString(event.Reasoning)
			if event.ToolDelta != nil {
				toolDeltas = append(toolDeltas, *event.ToolDelta)
			}
		case contract.EventUsage:
			if event.Usage != nil {
				usages = append(usages, *event.Usage)
			}
		case contract.EventFinish:
			finishCount++
		}
	}
	if text.String() != "hello" {
		t.Fatalf("文本增量 = %q", text.String())
	}
	if reasoning.String() != "thinking" {
		t.Fatalf("思考增量 = %q", reasoning.String())
	}
	if len(toolDeltas) != 1 || toolDeltas[0].Name != "lookup" || !strings.Contains(toolDeltas[0].ArgumentsFragment, `"q"`) {
		t.Fatalf("工具增量 = %+v", toolDeltas)
	}
	if finishCount != 1 {
		t.Fatalf("finish 事件 = %d, want 1", finishCount)
	}
	if len(usages) != 2 {
		t.Fatalf("usage 事件 = %d, want 2", len(usages))
	}
	last := usages[len(usages)-1]
	if last.Prompt != 10 || last.Completion != 7 || last.Total != 15 || last.CacheRead != 3 || last.Reasoning != 2 {
		t.Fatalf("跨块累计 usage = %+v（Completion 应为 candidates+thoughts）", last)
	}
	if last.PromptIncludesCache || !last.ReasoningIncludedInCompletion {
		t.Fatalf("关系布尔错误：%+v", last)
	}
	if _, ok := last.Raw["cachedContentTokenCount"]; !ok {
		t.Fatalf("流式累计 Raw 必须保留 cachedContentTokenCount：%v", last.Raw)
	}
}

func TestGeminiStreamErrorAndRawParts(t *testing.T) {
	adapter := NewGeminiAdapter(contract.ChannelTypeGemini)
	sse := strings.Join([]string{
		`data: {"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"aGk="}}]}}]}`,
		"",
		`data: {"error":{"code":500,"message":"boom","status":"INTERNAL"}}`,
		"",
	}, "\n")
	events := collectStreamEvents(adapter.(contract.StreamAdapter).StreamEvents(geminiContext("k"), bytes.NewBufferString(sse)))
	var rawCount, errorCount int
	for _, event := range events {
		switch event.Kind {
		case contract.EventRaw:
			rawCount++
		case contract.EventError:
			errorCount++
		}
	}
	if rawCount != 1 {
		t.Fatalf("无法表达的图片 part 必须以 EventRaw 保留，得到 %d", rawCount)
	}
	if errorCount != 1 {
		t.Fatalf("error 块必须映射为 EventError，得到 %d", errorCount)
	}
}

func TestGeminiRegistryDispatch(t *testing.T) {
	adapter, err := Dispatch(contract.ChannelTypeGemini)
	if err != nil {
		t.Fatalf("Dispatch(gemini): %v", err)
	}
	gemini, ok := adapter.(*Gemini)
	if !ok {
		t.Fatalf("Dispatch(gemini) = %T, want *Gemini（R-13 原生适配器）", adapter)
	}
	if gemini.Type() != contract.ChannelTypeGemini {
		t.Fatalf("Type = %q", gemini.Type())
	}
	if _, ok := adapter.(contract.StreamAdapter); !ok {
		t.Fatal("Gemini 适配器必须实现 StreamAdapter")
	}
	caps := adapter.Caps()
	if !hasRole(caps.Roles, contract.RoleChat) {
		t.Fatalf("gemini 必须有 RoleChat：%+v", caps.Roles)
	}
	for _, endpoint := range []contract.Endpoint{contract.EndpointChat, contract.EndpointGenerateContent} {
		if _, ok := caps.Endpoints[endpoint]; !ok {
			t.Fatalf("gemini Caps 缺少端点 %q", endpoint)
		}
	}
	if caps.StreamUsage || !caps.StreamAggregatesUsage {
		t.Fatalf("Gemini 流式能力声明错误：%+v", caps)
	}
	if caps.DefaultBaseURL != geminiDefaultBaseURL {
		t.Fatalf("DefaultBaseURL = %q", caps.DefaultBaseURL)
	}
}
