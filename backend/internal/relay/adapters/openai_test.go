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
	if caps.BaseURLMode != contract.BaseURLDefault {
		t.Fatalf("BaseURLMode = %v, want BaseURLDefault（openai 有默认基址）", caps.BaseURLMode)
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

// TestOpenAIBuildRequestRequiredBaseURLTypes 覆盖 relay-02 §7.1 的必填基址集合：
// 缺失时不得静默落到 https://api.openai.com（会把凭据发往错误上游）。
func TestOpenAIBuildRequestRequiredBaseURLTypes(t *testing.T) {
	for _, ct := range []contract.ChannelType{
		contract.ChannelTypeAzure,
		contract.ChannelTypeCustom,
		contract.ChannelTypeSunoAPI,
		contract.ChannelTypeVolcEngine,
		contract.ChannelTypeDoubaoVideo,
		contract.ChannelTypeNewAPI,
		contract.ChannelTypeTaskPlugin,
		contract.ChannelTypeVLLM,
		contract.ChannelTypeSGLang,
	} {
		t.Run(string(ct), func(t *testing.T) {
			_, err := NewOpenAI().BuildRequest(&contract.RelayContext{
				ChannelType: ct,
				Credential:  "secret-key",
				// APIBase 为空
			}, &contract.Request{Model: "m"})
			if err == nil {
				t.Fatalf("%q 缺 base_url 必须失败", ct)
			}
			if !errors.Is(err, ErrChannelConfig) {
				t.Fatalf("%q error = %v, want ErrChannelConfig-class", ct, err)
			}
		})
	}
}

// TestOpenAIBuildRequestCustomFullURL 覆盖类型 8 的整 URL 语义：
// 替换 {model} 后直接作为完整 URL，绝不追加 /v1/chat/completions。
func TestOpenAIBuildRequestCustomFullURL(t *testing.T) {
	rc := &contract.RelayContext{
		ChannelType:   contract.ChannelTypeCustom,
		APIBase:       "https://custom.example/v2/generate?key=public&model={model}",
		Credential:    "custom-key",
		UpstreamModel: "mapped-model.v1",
	}
	req := &contract.Request{Model: "client-model", Messages: []contract.Message{{Role: contract.RoleUser, Content: []contract.ContentPart{{Kind: contract.PartText, Text: "hi"}}}}}
	httpReq, err := NewOpenAI().BuildRequest(rc, req)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	want := "https://custom.example/v2/generate?key=public&model=mapped-model.v1"
	if got := httpReq.URL.String(); got != want {
		t.Fatalf("URL = %q, want %q（整 URL，不追加 /v1/chat/completions）", got, want)
	}
	if !strings.Contains(httpReq.URL.Path, "/v2/generate") {
		t.Fatalf("path = %q，不得被改写为 chat/completions", httpReq.URL.Path)
	}
	if got := httpReq.Header.Get("Authorization"); got != "Bearer custom-key" {
		t.Fatalf("Authorization = %q", got)
	}
}

// TestOpenAIBuildRequestCustomModelEscaped 断言 {model} 的模型名经过转义，
// 防止模型名中的 ? / # / ../ 改写完整 URL。
func TestOpenAIBuildRequestCustomModelEscaped(t *testing.T) {
	rc := &contract.RelayContext{
		ChannelType:   contract.ChannelTypeCustom,
		APIBase:       "https://custom.example/api/{model}",
		Credential:    "k",
		UpstreamModel: "evil?x=1#frag",
	}
	httpReq, err := NewOpenAI().BuildRequest(rc, &contract.Request{Model: "m"})
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if strings.Contains(httpReq.URL.RawQuery, "x=1") || httpReq.URL.Fragment != "" {
		t.Fatalf("模型名改写了 URL 结构：%s", httpReq.URL.String())
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
		{http.StatusRequestTimeout, contract.ErrUnknown}, // relay-01 §6.2：408 属不重试固定集合
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

// TestOpenAIStreamEventsPreservesRawFieldsAndSkipsEmptyData 覆盖：
// ① event:/id:/data: 行保留在事件 Raw 中（转码/审计需要原始事件体）；
// ② 空 data 事件（`data:` 心跳）被跳过，不得产生伪 JSON 错误。
func TestOpenAIStreamEventsPreservesRawFieldsAndSkipsEmptyData(t *testing.T) {
	stream := "event: message\nid: 42\ndata: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data:\n\n" +
		"data: [DONE]\n\n"
	var delta *contract.StreamEvent
	for ev := range NewOpenAI().StreamEvents(&contract.RelayContext{}, bytes.NewBufferString(stream)) {
		ev := ev
		switch ev.Kind {
		case contract.EventError:
			t.Fatalf("空 data 不得产生错误事件：%s", ev.Raw)
		case contract.EventDelta:
			if ev.Delta == "hi" {
				delta = &ev
			}
		}
	}
	if delta == nil {
		t.Fatal("缺少内容 delta 事件")
	}
	raw := string(delta.Raw)
	for _, want := range []string{"event: message", "id: 42", "data: "} {
		if !strings.Contains(raw, want) {
			t.Fatalf("事件 Raw 未保留原始行 %q：%q", want, raw)
		}
	}
}

// ============================================================================
// final-review finding 3：逐类型默认基址/路径（relay-02 §5/§7.1）
// ============================================================================

// TestOpenAIDefaultBaseURLPerType 冻结逐类型默认基址与 Chat 路径：
// 共享 OpenAI 适配器绝不能把非 openai 类型一律发往 api.openai.com。
func TestOpenAIDefaultBaseURLPerType(t *testing.T) {
	cases := []struct {
		channelType contract.ChannelType
		apiBase     string // 空 = 用默认值
		wantURL     string
	}{
		{contract.ChannelTypeOpenAI, "", "https://api.openai.com/v1/chat/completions"},
		{contract.ChannelTypeOpenAIMax, "", "https://api.openaimax.com/v1/chat/completions"},
		{contract.ChannelTypeOhMyGPT, "", "https://api.ohmygpt.com/v1/chat/completions"},
		{contract.ChannelTypeAILS, "", "https://api.caipacity.com/v1/chat/completions"},
		{contract.ChannelTypeAIProxy, "", "https://api.aiproxy.io/v1/chat/completions"},
		{contract.ChannelTypeAPI2GPT, "", "https://api.api2gpt.com/v1/chat/completions"},
		{contract.ChannelTypeAIGC2D, "", "https://api.aigc2d.com/v1/chat/completions"},
		{contract.ChannelAi360, "", "https://api.360.cn/v1/chat/completions"},
		{contract.ChannelTypeOpenRouter, "", "https://openrouter.ai/api/v1/chat/completions"},
		{contract.ChannelTypeFastGPT, "", "https://fastgpt.run/api/openapi/v1/chat/completions"},
		{contract.ChannelTypeZhipuV4, "", "https://open.bigmodel.cn/api/paas/v4/chat/completions"},
		{contract.ChannelTypePerplexity, "", "https://api.perplexity.ai/chat/completions"},
		{contract.ChannelTypeLingYiWanWu, "", "https://api.lingyiwanwu.com/v1/chat/completions"},
		{contract.ChannelTypeSiliconFlow, "", "https://api.siliconflow.cn/v1/chat/completions"},
		{contract.ChannelTypeMistral, "", "https://api.mistral.ai/v1/chat/completions"},
		{contract.ChannelTypeDeepSeek, "", "https://api.deepseek.com/v1/chat/completions"},
		// volcengine 必填 base_url，但路径固定 /api/v3（relay-02 §5 行 45）。
		{contract.ChannelTypeVolcEngine, "https://ark.cn-beijing.volces.com", "https://ark.cn-beijing.volces.com/api/v3/chat/completions"},
		{contract.ChannelTypeBaiduV2, "", "https://qianfan.baidubce.com/v2/chat/completions"},
		{contract.ChannelTypeXai, "", "https://api.x.ai/v1/chat/completions"},
		{contract.ChannelTypeSubmodel, "", "https://llm.submodel.ai/v1/chat/completions"},
		{contract.ChannelTypeAli, "", "https://dashscope.aliyuncs.com/compatible-mode/v1/chat/completions"},
		// 渠道覆盖优先于默认值。
		{contract.ChannelTypeOpenRouter, "https://proxy.example/v1", "https://proxy.example/v1/chat/completions"},
	}
	for _, tc := range cases {
		t.Run(string(tc.channelType)+"/"+tc.wantURL, func(t *testing.T) {
			adapter := &OpenAI{channelType: tc.channelType}
			httpReq, err := adapter.BuildRequest(&contract.RelayContext{
				ChannelType: tc.channelType,
				APIBase:     tc.apiBase,
				Credential:  "sk-test",
			}, &contract.Request{Model: "m"})
			if err != nil {
				t.Fatalf("BuildRequest: %v", err)
			}
			if got := httpReq.URL.String(); got != tc.wantURL {
				t.Fatalf("URL = %q，期望 %q（relay-02 §5/§7.1 逐类型默认值）", got, tc.wantURL)
			}
		})
	}
}

// TestOpenAICapsDefaultBaseURLPerType 断言能力声明与 BuildRequest 使用同一张表：
// 渠道表单据此展示默认基址；BaseURLMode 必须与 DefaultBaseURL 一致
// （空默认 = BaseURLRequired，有默认 = BaseURLDefault）。
func TestOpenAICapsDefaultBaseURLPerType(t *testing.T) {
	cases := []struct {
		channelType contract.ChannelType
		wantBaseURL string
		wantMode    contract.BaseURLMode
	}{
		{contract.ChannelTypeOpenAI, "https://api.openai.com", contract.BaseURLDefault},
		{contract.ChannelTypeOpenRouter, "https://openrouter.ai/api", contract.BaseURLDefault},
		{contract.ChannelTypeZhipuV4, "https://open.bigmodel.cn", contract.BaseURLDefault},
		{contract.ChannelTypeDeepSeek, "https://api.deepseek.com", contract.BaseURLDefault},
		{contract.ChannelTypeXinference, "", contract.BaseURLRequired}, // 默认空：必须由渠道配置
		{contract.ChannelTypeAzure, "", contract.BaseURLRequired},      // 必填
		{contract.ChannelTypeCustom, "", contract.BaseURLRequired},     // 必填整 URL
		{contract.ChannelTypeVolcEngine, "", contract.BaseURLRequired}, // 必填
	}
	for _, tc := range cases {
		adapter := &OpenAI{channelType: tc.channelType}
		caps := adapter.Caps()
		if caps.DefaultBaseURL != tc.wantBaseURL {
			t.Errorf("%q Caps().DefaultBaseURL = %q，期望 %q", tc.channelType, caps.DefaultBaseURL, tc.wantBaseURL)
		}
		if caps.BaseURLMode != tc.wantMode {
			t.Errorf("%q Caps().BaseURLMode = %v，期望 %v（空默认基址必须声明 BaseURLRequired）",
				tc.channelType, caps.BaseURLMode, tc.wantMode)
		}
	}
}

// TestOpenAIBuildRequestDefaultEmptyBaseURLFails 冻结「默认空 = 不可用」：
// xinference（§5 行 47 默认空）与未知类型都不得静默回落到 OpenAI。
func TestOpenAIBuildRequestDefaultEmptyBaseURLFails(t *testing.T) {
	cases := []struct {
		name        string
		adapter     *OpenAI
		channelType contract.ChannelType
	}{
		{"xinference", &OpenAI{channelType: contract.ChannelTypeXinference}, contract.ChannelTypeXinference},
		{"unknown", &OpenAI{channelType: contract.ChannelTypeUnknown}, contract.ChannelTypeUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.adapter.BuildRequest(&contract.RelayContext{
				ChannelType: tc.channelType,
				Credential:  "secret-key",
				// APIBase 为空
			}, &contract.Request{Model: "m"})
			if err == nil {
				t.Fatal("默认基址为空的类型必须返回 ErrChannelConfig")
			}
			if !errors.Is(err, ErrChannelConfig) {
				t.Fatalf("error = %v，期望 ErrChannelConfig-class", err)
			}
		})
	}
}

// TestOpenAIBuildRequestCustomQueryModelEscaped 冻结 finding 6：
// {model} 位于 query 时必须用 QueryEscape，`a&admin=1` 不能注入额外参数。
func TestOpenAIBuildRequestCustomQueryModelEscaped(t *testing.T) {
	rc := &contract.RelayContext{
		ChannelType:   contract.ChannelTypeCustom,
		APIBase:       "https://custom.example/v2/generate?key=public&model={model}",
		Credential:    "k",
		UpstreamModel: "a&admin=1",
	}
	httpReq, err := NewOpenAI().BuildRequest(rc, &contract.Request{Model: "m"})
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if got := httpReq.URL.Query().Get("admin"); got != "" {
		t.Fatalf("模型名注入了 admin 参数：%q（URL=%s）", got, httpReq.URL.String())
	}
	if got := httpReq.URL.Query().Get("model"); got != "a&admin=1" {
		t.Fatalf("model 参数 = %q，期望解码回 a&admin=1", got)
	}
	if !strings.Contains(httpReq.URL.RawQuery, "model=a%26admin%3D1") {
		t.Fatalf("query 转义不完整：%s", httpReq.URL.RawQuery)
	}
}

// TestOpenAIBuildRequestCustomPathModelEscaped 覆盖占位符位于路径时仍用
// PathEscape，且不得把 `?`/`#` 带成 URL 结构。
func TestOpenAIBuildRequestCustomPathModelEscaped(t *testing.T) {
	rc := &contract.RelayContext{
		ChannelType:   contract.ChannelTypeCustom,
		APIBase:       "https://custom.example/api/{model}",
		Credential:    "k",
		UpstreamModel: "a&admin=1",
	}
	httpReq, err := NewOpenAI().BuildRequest(rc, &contract.Request{Model: "m"})
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if httpReq.URL.Query().Get("admin") != "" || httpReq.URL.Fragment != "" {
		t.Fatalf("路径占位符改写了 URL 结构：%s", httpReq.URL.String())
	}
	if !strings.Contains(httpReq.URL.EscapedPath(), "a&admin=1") {
		t.Fatalf("路径模型名 = %q，期望保留 a&admin=1", httpReq.URL.EscapedPath())
	}
}

// TestOpenAIBuildRequestCustomAuthorityPlaceholderRejected 冻结 security-04 §2.4：
// `{model}` 替换不得改变主机。authority 没有安全的转义机制，而模型名是客户端
// 可控输入，允许替换就等于允许客户端选择 host/port（例如 `10.0.0.5:8080`）。
// 因此只要模板的 authority 出现占位符，整个渠道配置即 ErrChannelConfig——
// 即使当前模型名本身「安全」也不放行（否则安全性取决于请求内容）。
func TestOpenAIBuildRequestCustomAuthorityPlaceholderRejected(t *testing.T) {
	cases := []struct {
		name    string
		baseURL string
		model   string
	}{
		{"subdomain template", "https://{model}.example.com/v1/chat/completions", "gpt-4o"},
		{"host template", "https://{model}/v1/chat/completions", "gpt-4o"},
		{"encoded placeholder", "https://%7Bmodel%7D.example.com/v1/chat/completions", "gpt-4o"},
		{"client-selected host:port", "https://{model}/v1/chat/completions", "10.0.0.5:8080"},
		{"userinfo template", "https://user@{model}/v1/chat/completions", "evil.example"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rc := &contract.RelayContext{
				ChannelType:   contract.ChannelTypeCustom,
				APIBase:       tc.baseURL,
				Credential:    "k",
				UpstreamModel: tc.model,
			}
			_, err := NewOpenAI().BuildRequest(rc, &contract.Request{Model: "m"})
			if err == nil {
				t.Fatal("authority 占位符必须返回 ErrChannelConfig")
			}
			if class, ok := ClassOf(err); !ok || class != contract.ErrChannelConfig {
				t.Fatalf("error class = %v/%v，期望 ErrChannelConfig", class, ok)
			}
			if strings.Contains(err.Error(), tc.baseURL) || strings.Contains(err.Error(), tc.model) {
				t.Fatalf("错误信息泄露 base_url/模型名（GC-6）：%v", err)
			}
		})
	}

	// 固定主机时 path/query 占位符仍然可用：拒绝范围仅限 authority。
	rc := &contract.RelayContext{
		ChannelType:   contract.ChannelTypeCustom,
		APIBase:       "https://custom.example/v1/{model}?alias={model}",
		Credential:    "k",
		UpstreamModel: "gpt-4o",
	}
	httpReq, err := NewOpenAI().BuildRequest(rc, &contract.Request{Model: "m"})
	if err != nil {
		t.Fatalf("固定主机的 path/query 占位符不应被拒绝：%v", err)
	}
	if httpReq.URL.Host != "custom.example" {
		t.Fatalf("host = %q，期望 custom.example", httpReq.URL.Host)
	}
	if !strings.Contains(httpReq.URL.EscapedPath(), "gpt-4o") ||
		httpReq.URL.Query().Get("alias") != "gpt-4o" {
		t.Fatalf("path/query 占位符未被替换：%s", httpReq.URL.String())
	}
}

// TestOpenAIBuildRequestCustomFragmentPlaceholder 冻结 fragment 占位符的
// 单次转义：PathEscape 的结果必须写入 RawFragment，否则 URL.String 会按
// fragment 规则把 `%` 再转义一次（%20 → %2520），模型名被双重编码。
func TestOpenAIBuildRequestCustomFragmentPlaceholder(t *testing.T) {
	rc := &contract.RelayContext{
		ChannelType:   contract.ChannelTypeCustom,
		APIBase:       "https://custom.example/v1/generate#frag={model}",
		Credential:    "k",
		UpstreamModel: "a b#c%",
	}
	httpReq, err := NewOpenAI().BuildRequest(rc, &contract.Request{Model: "m"})
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if got := httpReq.URL.EscapedFragment(); got != "frag=a%20b%23c%25" {
		t.Fatalf("EscapedFragment = %q，期望单次转义 frag=a%%20b%%23c%%25（double-escape 会得到 %%2520）", got)
	}
	if got := httpReq.URL.Fragment; got != "frag=a b#c%" {
		t.Fatalf("Fragment = %q，期望解码回 frag=a b#c%%", got)
	}
	if got := httpReq.URL.String(); strings.Contains(got, "%2520") || strings.Contains(got, "%2523") {
		t.Fatalf("fragment 被二次转义：%s", got)
	}
}

// TestReadUpstreamBodyCap 冻结 finding 9 的有界读取：恰好等于上限成功，
// 超过上限按 ErrUpstreamServer 拒绝（绝不截断后当正常响应解析）。
func TestReadUpstreamBodyCap(t *testing.T) {
	const limit = 8
	body, err := readUpstreamBody(strings.NewReader("12345678"), limit)
	if err != nil {
		t.Fatalf("恰好等于上限应成功：%v", err)
	}
	if string(body) != "12345678" {
		t.Fatalf("body = %q", body)
	}

	_, err = readUpstreamBody(strings.NewReader("123456789"), limit)
	if err == nil {
		t.Fatal("超限响应必须报错")
	}
	if class, ok := ClassOf(err); !ok || class != contract.ErrUpstreamServer {
		t.Fatalf("超限错误分类 = %v/%v，期望 ErrUpstreamServer", class, ok)
	}
}

// TestOpenAIStreamEventsCapsSingleLineRead 冻结 P1-8：单条不带换行的超长行
// 必须在读取过程中就地截停，不能先 ReadString 把整行读进内存再检查——恶意
// 或被攻陷的上游只要持续发送不带 '\n' 的数据，网关内存就按流长度增长。
func TestOpenAIStreamEventsCapsSingleLineRead(t *testing.T) {
	// 4MB 的惰性数据源：旧实现会把它全部读进内存后才触发上限检查。
	rw := &endlessLineReader{remaining: 4 << 20}
	var events []contract.StreamEvent
	for ev := range openAIStreamEvents(rw, 8) {
		events = append(events, ev)
	}
	if len(events) != 1 || events[0].Kind != contract.EventError || events[0].Raw != nil {
		t.Fatalf("超长单行必须交付唯一的硬错误事件：%+v", events)
	}
	// 读取量必须是常数级（一个 sseInitialBuffer 分片），而不是整条流。
	if rw.read > 4*sseInitialBuffer {
		t.Fatalf("为 8 字节的事件上限读了 %d 字节：单行上限必须边读边判", rw.read)
	}
}

// endlessLineReader 惰性产生 'x'，永不换行；remaining 让它最终 EOF，避免
// 旧实现下测试无限挂起。
type endlessLineReader struct {
	remaining int64
	read      int64
}

func (r *endlessLineReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	for i := range p {
		p[i] = 'x'
	}
	r.remaining -= int64(len(p))
	r.read += int64(len(p))
	return len(p), nil
}

func (r *endlessLineReader) Write(p []byte) (int, error) { return len(p), nil }

// TestOpenAIStreamEventsEventCapIsHardError 冻结 finding 5：事件字节上限触发时
// 必须产出 Raw == nil 的硬错误事件（bridgeStream 据此终止且不伪造 [DONE]），
// 绝不能带 Raw 被当成软错误。
func TestOpenAIStreamEventsEventCapIsHardError(t *testing.T) {
	stream := "data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"too long\"}}]}\n\n"
	var events []contract.StreamEvent
	for ev := range openAIStreamEvents(bytes.NewBufferString(stream), 8) {
		events = append(events, ev)
	}
	if len(events) != 1 {
		t.Fatalf("事件数 = %d，期望 1（超限即终止）：%+v", len(events), events)
	}
	if events[0].Kind != contract.EventError {
		t.Fatalf("事件类型 = %v，期望 EventError", events[0].Kind)
	}
	if events[0].Raw != nil {
		t.Fatalf("超限事件携带 Raw=%q，必须为 nil（硬错误形状）", events[0].Raw)
	}
}
