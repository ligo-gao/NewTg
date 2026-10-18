package adapters

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"newtg/gateway/internal/contract"
)

func TestOpenAICaps(t *testing.T) {
	caps := NewOpenAI().Caps()
	if _, ok := caps.Endpoints[contract.EndpointChat]; !ok {
		t.Fatal("Endpoints 缺少 chat")
	}
	if _, ok := caps.Endpoints[contract.EndpointModels]; !ok {
		t.Fatal("Endpoints 缺少 models")
	}
	if !caps.Stream || !caps.StreamUsage || !caps.ModelList {
		t.Fatalf("Stream/StreamUsage/ModelList = %v/%v/%v, want true/true/true",
			caps.Stream, caps.StreamUsage, caps.ModelList)
	}
	if caps.DefaultBaseURL != "https://api.openai.com" {
		t.Fatalf("DefaultBaseURL = %q", caps.DefaultBaseURL)
	}
	if caps.CreditShape != contract.CreditSingleKey {
		t.Fatalf("CreditShape = %v, want CreditSingleKey", caps.CreditShape)
	}
	if len(caps.Roles) != 1 || caps.Roles[0] != contract.RoleChat {
		t.Fatalf("Roles = %v, want [RoleChat]", caps.Roles)
	}
}

func TestOpenAIBuildRequestOpenAI(t *testing.T) {
	rc := &contract.RelayContext{
		ChannelType:   contract.ChannelTypeOpenAI,
		APIBase:       "", // 空 → 默认 https://api.openai.com
		Credential:    "sk-test-secret",
		UpstreamModel: "gpt-4o-upstream",
	}
	req := &contract.Request{
		Model:  "gpt-4o",
		Stream: true,
		Messages: []contract.Message{{
			Role:    contract.RoleUser,
			Content: []contract.ContentPart{{Kind: contract.PartText, Text: "hi"}},
		}},
	}
	httpReq, err := NewOpenAI().BuildRequest(rc, req)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if httpReq.Method != http.MethodPost {
		t.Fatalf("method = %q, want POST", httpReq.Method)
	}
	if got, want := httpReq.URL.String(), "https://api.openai.com/v1/chat/completions"; got != want {
		t.Fatalf("URL = %q, want %q", got, want)
	}
	if got := httpReq.Header.Get("Authorization"); got != "Bearer sk-test-secret" {
		t.Fatalf("Authorization = %q, want Bearer <key>", got)
	}
	if got := httpReq.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := httpReq.Header.Get("Accept"); got != "text/event-stream" {
		t.Fatalf("Accept = %q, want text/event-stream（流式）", got)
	}
	if httpReq.ContentLength <= 0 || httpReq.GetBody == nil {
		t.Fatal("请求体必须可重放（ContentLength/GetBody，契约 §4.1 幂等要求）")
	}
	body, err := io.ReadAll(httpReq.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	s := string(body)
	for _, want := range []string{`"model":"gpt-4o-upstream"`, `"stream":true`, `"role":"user"`, `"content":"hi"`} {
		if !strings.Contains(s, want) {
			t.Fatalf("body 缺少 %s：%s", want, s)
		}
	}

	// 幂等：同一输入两次构造结果等价。
	second, err := NewOpenAI().BuildRequest(rc, req)
	if err != nil {
		t.Fatalf("second BuildRequest: %v", err)
	}
	secondBody, _ := io.ReadAll(second.Body)
	if string(secondBody) != s {
		t.Fatal("BuildRequest 不幂等：两次调用请求体不一致")
	}
	if second.URL.String() != httpReq.URL.String() {
		t.Fatal("BuildRequest 不幂等：两次调用 URL 不一致")
	}
}

func TestOpenAIBuildRequestAvoidsDoubleV1(t *testing.T) {
	rc := &contract.RelayContext{
		ChannelType: contract.ChannelTypeOpenAI,
		APIBase:     "https://proxy.example.com/v1/",
		Credential:  "sk-x",
	}
	req := &contract.Request{Model: "m", Messages: []contract.Message{{Role: contract.RoleUser, Content: []contract.ContentPart{{Kind: contract.PartText, Text: "hi"}}}}}
	httpReq, err := NewOpenAI().BuildRequest(rc, req)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if got, want := httpReq.URL.Path, "/v1/chat/completions"; got != want {
		t.Fatalf("path = %q, want %q（base 已含 /v1 时不得重复拼接，relay-02 §7.2）", got, want)
	}
}

func TestOpenAIBuildRequestAzure(t *testing.T) {
	after := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	before := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name       string
		created    time.Time
		upstream   string
		wantDeploy string
	}{
		{name: "keeps dot after 2025-05-10", created: after, upstream: "gpt-4o.v1", wantDeploy: "gpt-4o.v1"},
		{name: "removes dot before 2025-05-10", created: before, upstream: "gpt-4o.v1", wantDeploy: "gpt-4ov1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rc := &contract.RelayContext{
				ChannelType:   contract.ChannelTypeAzure,
				APIBase:       "https://my-resource.openai.azure.com",
				Credential:    "azure-key-secret",
				UpstreamModel: tc.upstream,
				CreatedTime:   tc.created,
			}
			req := &contract.Request{Model: "gpt-4o", Messages: []contract.Message{{Role: contract.RoleUser, Content: []contract.ContentPart{{Kind: contract.PartText, Text: "hi"}}}}}
			httpReq, err := NewOpenAI().BuildRequest(rc, req)
			if err != nil {
				t.Fatalf("BuildRequest: %v", err)
			}
			want := "https://my-resource.openai.azure.com/openai/deployments/" + tc.wantDeploy +
				"/chat/completions?api-version=2025-04-01-preview"
			if got := httpReq.URL.String(); got != want {
				t.Fatalf("URL = %q, want %q", got, want)
			}
			if got := httpReq.Header.Get("api-key"); got != "azure-key-secret" {
				t.Fatalf("api-key = %q", got)
			}
			if got := httpReq.Header.Get("Authorization"); got != "" {
				t.Fatalf("Azure 不得使用 Authorization（got %q）", got)
			}
		})
	}
}

func TestOpenAIBuildRequestAzureRequiresBaseURL(t *testing.T) {
	_, err := NewOpenAI().BuildRequest(&contract.RelayContext{
		ChannelType: contract.ChannelTypeAzure,
		Credential:  "azure-key",
	}, &contract.Request{Model: "m"})
	if err == nil {
		t.Fatal("Azure 缺 base_url 必须失败（relay-02 §7.1 必填）")
	}
	if !errors.Is(err, ErrChannelConfig) {
		t.Fatalf("error = %v, want ErrChannelConfig-class", err)
	}
}

// TestOpenAIErrorsDoNotLeakBaseURLOrCredential 覆盖 GC-6：错误串不得含上游 base_url / 凭据。
func TestOpenAIErrorsDoNotLeakBaseURLOrCredential(t *testing.T) {
	secretBase := "https://user:super-secret@bad host.example/"
	secretKey := "sk-super-secret-key"
	a := NewOpenAI()

	_, err := a.BuildRequest(&contract.RelayContext{
		ChannelType: contract.ChannelTypeOpenAI,
		APIBase:     secretBase,
		Credential:  secretKey,
	}, &contract.Request{Model: "m"})
	if err == nil {
		t.Fatal("非法 base URL 必须失败")
	}
	assertNoSecrets(t, err, secretBase, secretKey, "super-secret", "user:")

	_, err = a.BuildRequest(&contract.RelayContext{
		ChannelType: contract.ChannelTypeOpenAI,
		Credential:  "",
	}, &contract.Request{Model: "m"})
	if err == nil {
		t.Fatal("空凭据必须失败")
	}
	if !errors.Is(err, ErrChannelConfig) {
		t.Fatalf("error = %v, want ErrChannelConfig", err)
	}

	// 上游错误响应体可能回显敏感信息，ParseResponse 的错误不得携带响应体。
	resp := &http.Response{
		StatusCode: http.StatusUnauthorized,
		Body:       io.NopCloser(strings.NewReader(`{"error":"key ` + secretKey + ` rejected at ` + secretBase + `"}`)),
	}
	_, _, err = a.ParseResponse(&contract.RelayContext{}, resp)
	if err == nil {
		t.Fatal("401 响应必须返回错误")
	}
	assertNoSecrets(t, err, secretBase, secretKey, "super-secret")
	if class, ok := ClassOf(err); !ok || class != contract.ErrUpstreamAuth {
		t.Fatalf("401 错误分类 = %v/%v, want ErrUpstreamAuth", class, ok)
	}
}

func assertNoSecrets(t *testing.T, err error, needles ...string) {
	t.Helper()
	msg := err.Error()
	for _, n := range needles {
		if n != "" && strings.Contains(msg, n) {
			t.Fatalf("错误串泄露敏感信息 %q：%s", n, msg)
		}
	}
}

func TestOpenAIParseResponse(t *testing.T) {
	body := `{
	  "id": "chatcmpl-1", "model": "gpt-4o",
	  "choices": [{"index": 0, "message": {"role": "assistant", "content": "hello"}, "finish_reason": "stop"}],
	  "usage": {"prompt_tokens": 10, "completion_tokens": 4, "total_tokens": 14,
	    "prompt_tokens_details": {"cached_tokens": 2},
	    "completion_tokens_details": {"reasoning_tokens": 1}}
	}`
	resp, usage, err := NewOpenAI().ParseResponse(&contract.RelayContext{}, &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
	})
	if err != nil {
		t.Fatalf("ParseResponse: %v", err)
	}
	if resp.ID != "chatcmpl-1" || resp.Model != "gpt-4o" || len(resp.Choices) != 1 {
		t.Fatalf("response = %+v", resp)
	}
	if resp.Choices[0].Message.Content[0].Text != "hello" || resp.FinishReason != "stop" {
		t.Fatalf("choice = %+v", resp.Choices[0])
	}
	if string(resp.Raw) != body {
		t.Fatal("Response.Raw 必须是上游原始响应体")
	}
	if usage.Prompt != 10 || usage.Completion != 4 || usage.Total != 14 {
		t.Fatalf("usage = %+v", usage)
	}
	if !usage.PromptIncludesCache || !usage.ReasoningIncludedInCompletion {
		t.Fatalf("关系布尔错误：%+v", usage)
	}
	if usage.CacheRead != 2 || usage.Reasoning != 1 {
		t.Fatalf("cache/reasoning = %+v", usage)
	}
}

func TestOpenAIClassifyError(t *testing.T) {
	cases := []struct {
		status int
		want   contract.ErrorClass
	}{
		{http.StatusBadRequest, contract.ErrParamInvalid},
		{http.StatusUnprocessableEntity, contract.ErrParamInvalid},
		{http.StatusUnauthorized, contract.ErrUpstreamAuth},
		{http.StatusForbidden, contract.ErrUpstreamAuth},
		{http.StatusNotFound, contract.ErrParamInvalid},
		{http.StatusTooManyRequests, contract.ErrUpstreamRateLimit},
		{http.StatusInternalServerError, contract.ErrUpstreamServer},
		{http.StatusBadGateway, contract.ErrUpstreamServer},
		{http.StatusServiceUnavailable, contract.ErrUpstreamServer},
		{http.StatusContinue, contract.ErrRetryable},
		{199, contract.ErrRetryable},
		{http.StatusMultipleChoices, contract.ErrRetryable},
		{399, contract.ErrRetryable},
		{http.StatusOK, contract.ErrUnknown},
	}
	a := NewOpenAI()
	for _, tc := range cases {
		if got := a.ClassifyError(tc.status, []byte("boom")); got != tc.want {
			t.Errorf("ClassifyError(%d) = %v, want %v", tc.status, got, tc.want)
		}
	}
}

func TestOpenAIStreamEvents(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"id":"c1","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"}}]}`,
		"",
		`data: {"id":"c1","model":"gpt-4o","choices":[{"index":0,"delta":{"content":"lo"}}]}`,
		"",
		`data: {"id":"c1","model":"gpt-4o","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"lookup","arguments":"{\"q\":"}}]}}]}`,
		"",
		`data: {"id":"c1","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		"",
		`data: {"id":"c1","model":"gpt-4o","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":3,"total_tokens":13}}`,
		"",
		`data: [DONE]`,
		"",
	}, "\n")

	var events []contract.StreamEvent
	for ev := range NewOpenAI().StreamEvents(&contract.RelayContext{}, bytes.NewBufferString(stream)) {
		events = append(events, ev)
	}

	var deltas []string
	var reasoning []string
	var toolDeltas []contract.ToolCallDelta
	finishCount, usageCount := 0, 0
	for _, ev := range events {
		switch ev.Kind {
		case contract.EventDelta:
			if ev.Delta != "" {
				deltas = append(deltas, ev.Delta)
			}
			if ev.Reasoning != "" {
				reasoning = append(reasoning, ev.Reasoning)
			}
			if ev.ToolDelta != nil {
				toolDeltas = append(toolDeltas, *ev.ToolDelta)
			}
		case contract.EventFinish:
			finishCount++
			if ev.Model != "gpt-4o" {
				t.Fatalf("finish model = %q", ev.Model)
			}
		case contract.EventUsage:
			usageCount++
			if ev.Usage == nil || ev.Usage.Prompt != 10 || ev.Usage.Completion != 3 {
				t.Fatalf("usage event = %+v", ev.Usage)
			}
		case contract.EventError:
			t.Fatalf("unexpected error event: %s", ev.Raw)
		}
	}
	if strings.Join(deltas, "") != "Hello" {
		t.Fatalf("deltas = %v, want Hello", deltas)
	}
	if len(reasoning) != 0 {
		t.Fatalf("unexpected reasoning deltas: %v", reasoning)
	}
	if len(toolDeltas) != 1 || toolDeltas[0].Index != 0 || toolDeltas[0].ID != "call_1" ||
		toolDeltas[0].Name != "lookup" || toolDeltas[0].ArgumentsFragment != `{"q":` {
		t.Fatalf("tool deltas = %+v", toolDeltas)
	}
	if finishCount != 1 {
		t.Fatalf("finish events = %d, want 1（[DONE] 不得重复发 finish）", finishCount)
	}
	if usageCount != 1 {
		t.Fatalf("usage events = %d, want 1", usageCount)
	}
}

func TestOpenAIStreamEventsMultiLineData(t *testing.T) {
	stream := "data: {\"id\":\"c1\",\"model\":\"m\",\n" +
		"data: \"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: [DONE]\n\n"
	var got []contract.StreamEvent
	for ev := range NewOpenAI().StreamEvents(&contract.RelayContext{}, bytes.NewBufferString(stream)) {
		got = append(got, ev)
	}
	var text string
	for _, ev := range got {
		if ev.Kind == contract.EventDelta {
			text += ev.Delta
		}
	}
	if text != "hi" {
		t.Fatalf("multi-line data 未聚合：text=%q events=%+v", text, got)
	}
}

func TestOpenAIStreamEventsReasoningAndError(t *testing.T) {
	stream := "data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"think\"}}]}\n\n" +
		"data: {\"error\":{\"message\":\"boom\"}}\n\n" +
		"data: [DONE]\n\n"
	var sawReasoning, sawError bool
	for ev := range NewOpenAI().StreamEvents(&contract.RelayContext{}, bytes.NewBufferString(stream)) {
		switch ev.Kind {
		case contract.EventDelta:
			if ev.Reasoning == "think" {
				sawReasoning = true
			}
		case contract.EventError:
			sawError = true
			if strings.Contains(string(ev.Raw), "boom") == false {
				t.Fatal("错误事件应保留原始帧（Raw）")
			}
		}
	}
	if !sawReasoning || !sawError {
		t.Fatalf("reasoning=%v error=%v", sawReasoning, sawError)
	}
}

// TestOpenAIStreamEventsDoneWithoutFinishChunk 断言仅有 [DONE] 时也产出 finish 事件。
func TestOpenAIStreamEventsDoneWithoutFinishChunk(t *testing.T) {
	stream := "data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"}}]}\n\n" +
		"data: [DONE]\n\n"
	var finish int
	for ev := range NewOpenAI().StreamEvents(&contract.RelayContext{}, bytes.NewBufferString(stream)) {
		if ev.Kind == contract.EventFinish {
			finish++
		}
	}
	if finish != 1 {
		t.Fatalf("finish events = %d, want 1", finish)
	}
}
