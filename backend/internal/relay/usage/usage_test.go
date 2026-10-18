package usage

import (
	"bytes"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"newtg/gateway/internal/contract"
)

// ----------------------------------------------------------------------------
// ExtractUsage：三源优先级（R-05：原生 > 注入 > 响应头，首个非空来源胜出，
// 绝不累加）
// ----------------------------------------------------------------------------

func TestExtractUsageNativeOnly(t *testing.T) {
	resp := &contract.Response{
		Model: "gpt-4o",
		Raw: []byte(`{
			"id": "chatcmpl-1",
			"model": "gpt-4o",
			"usage": {
				"prompt_tokens": 100,
				"completion_tokens": 20,
				"total_tokens": 120,
				"prompt_tokens_details": {"cached_tokens": 30},
				"completion_tokens_details": {"reasoning_tokens": 7}
			}
		}`),
	}

	fact := ExtractUsage(resp, nil, nil)
	if fact == nil {
		t.Fatal("ExtractUsage returned nil fact")
	}
	if fact.Model != "gpt-4o" {
		t.Fatalf("Model = %q, want gpt-4o", fact.Model)
	}
	got := fact.Usage
	if got.Prompt != 100 || got.Completion != 20 || got.Total != 120 {
		t.Fatalf("Prompt/Completion/Total = %d/%d/%d, want 100/20/120",
			got.Prompt, got.Completion, got.Total)
	}
	if got.CacheRead != 30 || got.Reasoning != 7 {
		t.Fatalf("CacheRead/Reasoning = %d/%d, want 30/7", got.CacheRead, got.Reasoning)
	}
	if got.Source != contract.UsageUpstreamExact {
		t.Fatalf("Source = %v, want UsageUpstreamExact", got.Source)
	}
	// GPT 格式的关系布尔必须显式为 true（relay-03 §6.1 规则 5/6）。
	if !got.PromptIncludesCache || !got.ReasoningIncludedInCompletion {
		t.Fatalf("relation booleans = %v/%v, want true/true",
			got.PromptIncludesCache, got.ReasoningIncludedInCompletion)
	}
	if got.Raw == nil {
		t.Fatal("native usage Raw 未保留（relay-03 §11.3 要求原文对账）")
	}
}

func TestExtractUsageNativeWinsOverInjectedAndHeader(t *testing.T) {
	resp := &contract.Response{
		Model: "gpt-4o",
		Raw:   []byte(`{"usage":{"prompt_tokens":10,"completion_tokens":4,"total_tokens":14}}`),
	}
	injected := &contract.Usage{Prompt: 999, Completion: 999, Total: 1998, Source: contract.UsageUpstreamPartial}
	headers := http.Header{}
	headers.Set("x-model-response-usage", `{"prompt_tokens":777,"completion_tokens":777,"total_tokens":1554}`)

	got := ExtractUsage(resp, headers, injected).Usage
	if got.Prompt != 10 || got.Completion != 4 || got.Total != 14 {
		t.Fatalf("Prompt/Completion/Total = %d/%d/%d, want native 10/4/14 (不得累加)",
			got.Prompt, got.Completion, got.Total)
	}
	if got.Source != contract.UsageUpstreamExact {
		t.Fatalf("Source = %v, want UsageUpstreamExact", got.Source)
	}
}

func TestExtractUsageInjectedWhenNativeAbsent(t *testing.T) {
	cases := map[string]*contract.Response{
		"nil resp":      nil,
		"empty raw":     {Model: "m"},
		"no usage key":  {Model: "m", Raw: []byte(`{"id":"x"}`)},
		"usage null":    {Model: "m", Raw: []byte(`{"usage":null}`)},
		"usage empty":   {Model: "m", Raw: []byte(`{"usage":{}}`)},
		"invalid usage": {Model: "m", Raw: []byte(`{"usage":"not-an-object"}`)},
		"invalid raw":   {Model: "m", Raw: []byte(`not-json`)},
	}
	injected := &contract.Usage{
		Prompt: 40, Completion: 2, Total: 42,
		Source: contract.UsageUpstreamPartial,
	}
	for name, resp := range cases {
		t.Run(name, func(t *testing.T) {
			got := ExtractUsage(resp, nil, injected).Usage
			if got.Prompt != 40 || got.Completion != 2 || got.Total != 42 {
				t.Fatalf("usage = %d/%d/%d, want injected 40/2/42",
					got.Prompt, got.Completion, got.Total)
			}
			if got.Source != contract.UsageUpstreamPartial {
				t.Fatalf("Source = %v, want UsageUpstreamPartial", got.Source)
			}
		})
	}
}

func TestExtractUsageZeroInjectedFallsThroughToHeader(t *testing.T) {
	headers := http.Header{}
	headers.Set("x-model-response-usage", `{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}`)

	got := ExtractUsage(&contract.Response{Model: "m"}, headers, &contract.Usage{}).Usage
	if got.Prompt != 3 || got.Completion != 1 || got.Total != 4 {
		t.Fatalf("usage = %d/%d/%d, want header 3/1/4", got.Prompt, got.Completion, got.Total)
	}
}

func TestExtractUsageNegativeInjectedClamped(t *testing.T) {
	injected := &contract.Usage{Prompt: -10, Completion: -2, Total: -12, CacheRead: -3}
	got := ExtractUsage(&contract.Response{Model: "m"}, nil, injected).Usage
	if got.Prompt != 0 || got.Completion != 0 || got.Total != 0 || got.CacheRead != 0 {
		t.Fatalf("negative injected not clamped to 0: %+v", got)
	}
}

func TestExtractUsageHeaderOnly(t *testing.T) {
	headers := http.Header{}
	headers.Set("x-model-response-usage", `{"prompt_tokens":12,"completion_tokens":34,"total_tokens":46}`)

	fact := ExtractUsage(&contract.Response{Model: "m"}, headers, nil)
	got := fact.Usage
	if got.Prompt != 12 || got.Completion != 34 || got.Total != 46 {
		t.Fatalf("usage = %d/%d/%d, want 12/34/46", got.Prompt, got.Completion, got.Total)
	}
	if got.Source != contract.UsageUpstreamExact {
		t.Fatalf("Source = %v, want UsageUpstreamExact (上游报告的头部用量)", got.Source)
	}
	// 头部不是标准协议：关系布尔保守默认 false（搞反方向会成倍错算费用）。
	if got.PromptIncludesCache || got.ReasoningIncludedInCompletion {
		t.Fatalf("relation booleans = %v/%v, want false/false",
			got.PromptIncludesCache, got.ReasoningIncludedInCompletion)
	}
	if got.Raw == nil {
		t.Fatal("header usage Raw 未保留（对账与审计）")
	}
}

func TestExtractUsageHeaderAliasesInputOutput(t *testing.T) {
	headers := http.Header{}
	headers.Set("x-model-response-usage", `{"input_tokens":11,"output_tokens":5}`)

	got := ExtractUsage(&contract.Response{Model: "m"}, headers, nil).Usage
	if got.Prompt != 11 || got.Completion != 5 {
		t.Fatalf("aliases = %d/%d, want 11/5", got.Prompt, got.Completion)
	}
	if got.Total != 16 {
		t.Fatalf("Total = %d, want derived 16", got.Total)
	}
}

func TestExtractUsageHeaderPromptAliasPrecedence(t *testing.T) {
	headers := http.Header{}
	headers.Set("x-model-response-usage", `{"prompt_tokens":1,"input_tokens":99,"completion_tokens":2,"output_tokens":99}`)

	got := ExtractUsage(nil, headers, nil).Usage
	if got.Prompt != 1 || got.Completion != 2 {
		t.Fatalf("usage = %d/%d, want prompt_tokens/completion_tokens 1/2", got.Prompt, got.Completion)
	}
}

func TestExtractUsageHeaderClampsNegative(t *testing.T) {
	headers := http.Header{}
	headers.Set("x-model-response-usage", `{"prompt_tokens":-5,"completion_tokens":7,"total_tokens":-1}`)

	got := ExtractUsage(nil, headers, nil).Usage
	if got.Prompt != 0 || got.Completion != 7 || got.Total != 0 {
		t.Fatalf("usage = %d/%d/%d, want clamped 0/7/0", got.Prompt, got.Completion, got.Total)
	}
}

func TestExtractUsageHeaderRelationBooleansOverride(t *testing.T) {
	headers := http.Header{}
	headers.Set("x-model-response-usage",
		`{"prompt_tokens":1,"completion_tokens":1,"prompt_includes_cache":true,"reasoning_included_in_completion":true}`)

	got := ExtractUsage(nil, headers, nil).Usage
	if !got.PromptIncludesCache || !got.ReasoningIncludedInCompletion {
		t.Fatalf("relation booleans = %v/%v, want true/true (头部显式声明)",
			got.PromptIncludesCache, got.ReasoningIncludedInCompletion)
	}
}

func TestExtractUsageHeaderInvalidOrEmptyFallsThrough(t *testing.T) {
	cases := map[string]string{
		"empty string": "",
		"invalid json": `{`,
		"empty object": `{}`,
		"no numeric":   `{"prompt_includes_cache":true}`,
	}
	injected := &contract.Usage{Prompt: 8, Completion: 1, Total: 9}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			headers := http.Header{}
			headers.Set("x-model-response-usage", value)
			got := ExtractUsage(nil, headers, injected).Usage
			if got.Prompt != 8 || got.Completion != 1 {
				t.Fatalf("usage = %d/%d, want injected fallback 8/1", got.Prompt, got.Completion)
			}
		})
	}
}

func TestExtractUsageNoSourceReturnsEstimatedZero(t *testing.T) {
	fact := ExtractUsage(&contract.Response{Model: "m"}, nil, nil)
	if fact.Model != "m" {
		t.Fatalf("Model = %q, want m", fact.Model)
	}
	got := fact.Usage
	if got.Source != contract.UsageEstimated {
		t.Fatalf("Source = %v, want UsageEstimated", got.Source)
	}
	if got.Prompt != 0 || got.Completion != 0 || got.Total != 0 {
		t.Fatalf("zero fallback not zero: %d/%d/%d", got.Prompt, got.Completion, got.Total)
	}
	if got.Raw != nil {
		t.Fatalf("zero fallback Raw = %v, want nil", got.Raw)
	}
}

func TestExtractUsageNilRespEmptyRawSafety(t *testing.T) {
	for name, resp := range map[string]*contract.Response{
		"nil":       nil,
		"empty raw": {},
	} {
		t.Run(name, func(t *testing.T) {
			fact := ExtractUsage(resp, nil, nil)
			if fact == nil {
				t.Fatal("ExtractUsage returned nil fact")
			}
			if fact.Model != "" {
				t.Fatalf("Model = %q, want empty", fact.Model)
			}
			if fact.Usage.Source != contract.UsageEstimated {
				t.Fatalf("Source = %v, want UsageEstimated", fact.Usage.Source)
			}
		})
	}
}

func TestExtractUsageFactDefaults(t *testing.T) {
	fact := ExtractUsage(&contract.Response{
		Model: "m",
		Raw:   []byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`),
	}, nil, nil)

	if fact.ImageCount != 0 || fact.LatencyMS != 0 || fact.Attempts != 0 {
		t.Fatalf("ImageCount/LatencyMS/Attempts = %d/%d/%d, want 0/0/0",
			fact.ImageCount, fact.LatencyMS, fact.Attempts)
	}
	if fact.FirstTokenMS != nil {
		t.Fatalf("FirstTokenMS = %v, want nil（非流式）", *fact.FirstTokenMS)
	}
	if fact.Stream {
		t.Fatal("Stream = true, want false（本函数产出的是非流式事实）")
	}
}

// TestUsageNumericFieldsAreInt64 固化 GC-9 / billing-00 §7.1–§7.2：
// 配额与用量一律整数 int64，金额链路禁止 float64。
func TestUsageNumericFieldsAreInt64(t *testing.T) {
	numeric := map[string]bool{
		"Prompt": true, "Completion": true, "Total": true,
		"CacheRead": true, "CacheWrite": true, "Reasoning": true,
		"AudioSeconds": true, "Reserved": true,
		"Image": true, "Audio": true, "Text": true, "CacheReadImage": true,
	}
	types := []reflect.Type{
		reflect.TypeOf(contract.Usage{}),
		reflect.TypeOf(contract.UsageDetail{}),
	}
	checked := 0
	for _, typ := range types {
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			if !numeric[field.Name] {
				continue
			}
			checked++
			if field.Type.Kind() != reflect.Int64 {
				t.Errorf("%s.%s 的类型是 %v，必须为 int64（GC-9）", typ.Name(), field.Name, field.Type)
			}
		}
	}
	if checked == 0 {
		t.Fatal("没有检查到任何数值字段，测试失去意义")
	}
}

// ----------------------------------------------------------------------------
// InjectIncludeUsage：R-P14 / relay-03 §2.3 / R-02
// ----------------------------------------------------------------------------

func TestInjectIncludeUsageStreamMergesStreamOptions(t *testing.T) {
	original := []byte(`{"model":"gpt-4o","stream":true,"temperature":0.2,"stream_options":{"foo":1},"logprobs":true}`)
	req := &contract.Request{
		Model:  "gpt-4o",
		Stream: true,
		Raw:    append(contract.ProtocolBody(nil), original...),
	}

	got := InjectIncludeUsage(req, true)
	if got == nil {
		t.Fatal("InjectIncludeUsage returned nil")
	}

	var obj map[string]any
	if err := json.Unmarshal(got.Raw, &obj); err != nil {
		t.Fatalf("re-encoded Raw 不是合法 JSON: %v", err)
	}
	options, ok := obj["stream_options"].(map[string]any)
	if !ok {
		t.Fatalf("stream_options = %#v, want object", obj["stream_options"])
	}
	if options["include_usage"] != true {
		t.Fatalf("include_usage = %#v, want true", options["include_usage"])
	}
	if options["foo"] != float64(1) {
		t.Fatalf("existing stream_options key foo = %#v, want preserved 1", options["foo"])
	}
	// 未建模的 Raw 字段必须保留。
	if obj["temperature"] != 0.2 || obj["logprobs"] != true || obj["model"] != "gpt-4o" {
		t.Fatalf("unmodeled Raw fields lost: %#v", obj)
	}
	// R-P14：非流式语义下不得出现顶层 usage（真实 OpenAI 会 400）。
	if _, exists := obj["usage"]; exists {
		t.Fatal("stream body 不得注入顶层 usage")
	}
	// 入参不得被修改。
	if !bytes.Equal(req.Raw, original) {
		t.Fatalf("input Raw mutated: %s", req.Raw)
	}
}

func TestInjectIncludeUsageStreamEmptyRawUnchanged(t *testing.T) {
	for name, raw := range map[string]contract.ProtocolBody{
		"nil raw":   nil,
		"empty raw": {},
	} {
		t.Run(name, func(t *testing.T) {
			req := &contract.Request{Stream: true, Raw: raw}
			got := InjectIncludeUsage(req, true)
			if len(got.Raw) != 0 {
				t.Fatalf("Raw = %s, want unchanged empty", got.Raw)
			}
		})
	}
}

func TestInjectIncludeUsageNonStreamUnchanged(t *testing.T) {
	original := []byte(`{"model":"m","stream":false}`)
	req := &contract.Request{Model: "m", Stream: false, Raw: append(contract.ProtocolBody(nil), original...)}

	got := InjectIncludeUsage(req, true)
	if !bytes.Equal(got.Raw, original) {
		t.Fatalf("non-stream Raw = %s, want unchanged %s（R-P14）", got.Raw, original)
	}
	var obj map[string]any
	if err := json.Unmarshal(got.Raw, &obj); err != nil {
		t.Fatal(err)
	}
	if _, exists := obj["usage"]; exists {
		t.Fatal("R-P14：非流式请求不得注入顶层 usage（真实 OpenAI 会以未知参数 400 拒绝）")
	}
}

func TestInjectIncludeUsageWithUsageFalseUnchanged(t *testing.T) {
	original := []byte(`{"model":"m","stream":true,"stream_options":{"include_usage":false}}`)
	req := &contract.Request{Model: "m", Stream: true, Raw: append(contract.ProtocolBody(nil), original...)}

	got := InjectIncludeUsage(req, false)
	if !bytes.Equal(got.Raw, original) {
		t.Fatalf("Raw = %s, want unchanged %s（客户端的显式选择不得擦除）", got.Raw, original)
	}
}

func TestInjectIncludeUsageNilRequest(t *testing.T) {
	if got := InjectIncludeUsage(nil, true); got != nil {
		t.Fatalf("InjectIncludeUsage(nil) = %#v, want nil", got)
	}
}

func TestInjectIncludeUsageDeepCopiesRequest(t *testing.T) {
	maxTokens := 5
	temperature := 0.5
	topP := 0.9
	seed := 2
	req := &contract.Request{
		Model:  "m",
		Stream: true,
		Messages: []contract.Message{{
			Role: contract.RoleUser,
			Content: []contract.ContentPart{{
				Kind:    contract.PartText,
				Text:    "hi",
				Payload: []byte{1, 2},
				Meta:    map[string]any{"k": "v"},
			}},
			Extensions: map[string]any{"e": map[string]any{"x": 1}},
		}},
		Tools:          []contract.Tool{{Name: "t", Schema: map[string]any{"type": "object"}}},
		ResponseFormat: &contract.ResponseFormat{Kind: contract.ResponseFormatJSONObject, Schema: map[string]any{"a": 1}},
		MaxTokens:      &maxTokens,
		Temperature:    &temperature,
		TopP:           &topP,
		Stop:           []string{"END"},
		Seed:           &seed,
		RawFormat:      contract.RelayFormat("openai"),
		Raw:            []byte(`{"model":"m","stream":true}`),
	}

	got := InjectIncludeUsage(req, true)
	if got == req {
		t.Fatal("返回值与入参是同一指针，未做拷贝")
	}

	// 深度修改副本的每个可变结构，入参必须保持不变。
	got.Messages[0].Content[0].Text = "changed"
	got.Messages[0].Content[0].Payload[0] = 9
	got.Messages[0].Content[0].Meta["k"] = "changed"
	got.Messages[0].Extensions["e"].(map[string]any)["x"] = 2
	got.Tools[0].Schema["type"] = "array"
	got.ResponseFormat.Schema["a"] = 2
	*got.MaxTokens = 6
	*got.Temperature = 0.1
	*got.TopP = 0.1
	got.Stop[0] = "STOP"
	*got.Seed = 3
	got.Raw[0] = 'X'

	if req.Messages[0].Content[0].Text != "hi" ||
		req.Messages[0].Content[0].Payload[0] != 1 ||
		req.Messages[0].Content[0].Meta["k"] != "v" ||
		req.Messages[0].Extensions["e"].(map[string]any)["x"] != 1 ||
		req.Tools[0].Schema["type"] != "object" ||
		req.ResponseFormat.Schema["a"] != 1 ||
		*req.MaxTokens != 5 || *req.Temperature != 0.5 || *req.TopP != 0.9 ||
		req.Stop[0] != "END" || *req.Seed != 2 ||
		req.Raw[0] != '{' {
		t.Fatalf("入参被返回值共享的可变结构修改污染: %+v", req)
	}
}
