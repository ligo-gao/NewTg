package usage

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"newtg/gateway/internal/billing/pricing"
	"newtg/gateway/internal/contract"
)

// ----------------------------------------------------------------------------
// (a) Anthropic 原生 usage（非流式整包 + 流式 message_start / message_delta）
// ----------------------------------------------------------------------------

func TestUsageMultimodalAnthropicNative(t *testing.T) {
	resp := &contract.Response{
		Model: "claude-sonnet-4-5",
		Raw: []byte(`{
			"id":"msg_1","type":"message","model":"claude-sonnet-4-5",
			"usage":{
				"input_tokens":100,
				"output_tokens":20,
				"cache_creation_input_tokens":10,
				"cache_read_input_tokens":30
			}
		}`),
	}

	fact := ExtractUsageMultimodal(resp, nil, nil, contract.ChannelTypeAnthropic)
	if fact == nil {
		t.Fatal("nil fact")
	}
	if fact.Model != "claude-sonnet-4-5" {
		t.Fatalf("Model = %q", fact.Model)
	}
	got := fact.Usage
	if got.Prompt != 100 || got.Completion != 20 {
		t.Fatalf("Prompt/Completion = %d/%d, want 100/20", got.Prompt, got.Completion)
	}
	if got.CacheRead != 30 || got.CacheWrite != 10 {
		t.Fatalf("CacheRead/CacheWrite = %d/%d, want 30/10", got.CacheRead, got.CacheWrite)
	}
	if got.Total != 160 {
		t.Fatalf("Total = %d, want 160（input+output+缓存读写）", got.Total)
	}
	// Claude 的 input_tokens 不含缓存 → 关系布尔必须为 false（方向错了会成倍错算）。
	if got.PromptIncludesCache {
		t.Fatal("PromptIncludesCache = true, want false（Claude 语义）")
	}
	if !got.ReasoningIncludedInCompletion {
		t.Fatal("ReasoningIncludedInCompletion = false, want true（thinking 计入 output）")
	}
	if got.Source != contract.UsageUpstreamExact {
		t.Fatalf("Source = %v, want upstream_exact", got.Source)
	}
	// Raw 必须保留 Claude 原键（含 0 值也是协议事实），供 M3 usage_semantic 判定。
	for _, key := range []string{"input_tokens", "output_tokens", "cache_read_input_tokens", "cache_creation_input_tokens"} {
		if _, ok := got.Raw[key]; !ok {
			t.Fatalf("Raw 缺少 Claude 键 %q: %#v", key, got.Raw)
		}
	}
}

func TestUsageMultimodalAnthropicStreamEvents(t *testing.T) {
	t.Run("message_start 内嵌 message.usage", func(t *testing.T) {
		resp := &contract.Response{
			Model: "claude-sonnet-4-5",
			Raw: []byte(`{
				"type":"message_start",
				"message":{"id":"msg_1","usage":{"input_tokens":10,"cache_read_input_tokens":2}}
			}`),
		}
		got := ExtractUsageMultimodal(resp, nil, nil, contract.ChannelTypeAnthropic).Usage
		if got.Prompt != 10 || got.CacheRead != 2 {
			t.Fatalf("Prompt/CacheRead = %d/%d, want 10/2", got.Prompt, got.CacheRead)
		}
		if got.PromptIncludesCache {
			t.Fatal("PromptIncludesCache = true, want false")
		}
	})

	t.Run("message_delta 顶层 usage 只有 output_tokens", func(t *testing.T) {
		resp := &contract.Response{
			Model: "claude-sonnet-4-5",
			Raw:   []byte(`{"type":"message_delta","usage":{"output_tokens":7}}`),
		}
		got := ExtractUsageMultimodal(resp, nil, nil, contract.ChannelTypeAnthropic).Usage
		if got.Completion != 7 {
			t.Fatalf("Completion = %d, want 7", got.Completion)
		}
		if got.Total != 7 {
			t.Fatalf("Total = %d, want 7", got.Total)
		}
		if got.Source != contract.UsageUpstreamExact {
			t.Fatalf("Source = %v, want upstream_exact", got.Source)
		}
	})
}

// ----------------------------------------------------------------------------
// (b) Gemini 原生 usage（增量形状 + 明细拆分）
// ----------------------------------------------------------------------------

func TestUsageMultimodalGeminiNative(t *testing.T) {
	resp := &contract.Response{
		Model: "gemini-3-pro",
		Raw: []byte(`{
			"candidates":[{"content":{"parts":[{"text":"hi"}]}}],
			"usageMetadata":{
				"promptTokenCount":100,
				"toolUsePromptTokenCount":5,
				"candidatesTokenCount":20,
				"thoughtsTokenCount":3,
				"cachedContentTokenCount":40,
				"totalTokenCount":128,
				"promptTokensDetails":[
					{"modality":"IMAGE","tokenCount":50},
					{"modality":"AUDIO","tokenCount":10}
				],
				"cacheTokensDetails":[{"modality":"IMAGE","tokenCount":20}],
				"candidatesTokensDetails":[{"modality":"IMAGE","tokenCount":4}]
			}
		}`),
	}

	got := ExtractUsageMultimodal(resp, nil, nil, contract.ChannelTypeGemini).Usage
	if got.Prompt != 105 || got.Completion != 23 {
		t.Fatalf("Prompt/Completion = %d/%d, want 105/23（含 toolUse/thoughts）", got.Prompt, got.Completion)
	}
	if got.CacheRead != 40 || got.Reasoning != 3 || got.Total != 128 {
		t.Fatalf("CR/RZ/Total = %d/%d/%d, want 40/3/128", got.CacheRead, got.Reasoning, got.Total)
	}
	// Gemini 的 promptTokenCount 不含缓存 → 关系布尔 false；thoughts 已计入 completion → true。
	if got.PromptIncludesCache {
		t.Fatal("PromptIncludesCache = true, want false（Gemini 语义）")
	}
	if !got.ReasoningIncludedInCompletion {
		t.Fatal("ReasoningIncludedInCompletion = false, want true（thoughts 已计入 Completion）")
	}
	if got.PromptDetail.Image != 50 || got.PromptDetail.Audio != 10 {
		t.Fatalf("PromptDetail.Image/Audio = %d/%d, want 50/10", got.PromptDetail.Image, got.PromptDetail.Audio)
	}
	if got.PromptDetail.Text != 45 {
		t.Fatalf("PromptDetail.Text = %d, want 45（105-50-10）", got.PromptDetail.Text)
	}
	if got.PromptDetail.CacheReadImage != 20 {
		t.Fatalf("PromptDetail.CacheReadImage = %d, want 20", got.PromptDetail.CacheReadImage)
	}
	if got.CompletionDetail.Image != 4 {
		t.Fatalf("CompletionDetail.Image = %d, want 4", got.CompletionDetail.Image)
	}
}

// ----------------------------------------------------------------------------
// (c) 三源优先级保持不变（GC-3）
// ----------------------------------------------------------------------------

func TestUsageMultimodalThreeSourcePriority(t *testing.T) {
	headers := http.Header{}
	headers.Set("x-model-response-usage", `{"prompt_tokens":777,"completion_tokens":777,"total_tokens":1554}`)

	t.Run("原生 > 注入 > 头", func(t *testing.T) {
		resp := &contract.Response{
			Model: "claude-sonnet-4-5",
			Raw:   []byte(`{"usage":{"input_tokens":10,"output_tokens":4}}`),
		}
		injected := &contract.Usage{Prompt: 999, Completion: 999, Source: contract.UsageUpstreamPartial}
		got := ExtractUsageMultimodal(resp, headers, injected, contract.ChannelTypeAnthropic).Usage
		if got.Prompt != 10 || got.Completion != 4 {
			t.Fatalf("usage = %d/%d, want native 10/4（不得累加/降级）", got.Prompt, got.Completion)
		}
	})

	t.Run("原生为空时用注入", func(t *testing.T) {
		resp := &contract.Response{Model: "m", Raw: []byte(`{"usage":{}}`)}
		injected := &contract.Usage{Prompt: 40, Completion: 2, Source: contract.UsageUpstreamPartial}
		got := ExtractUsageMultimodal(resp, headers, injected, contract.ChannelTypeOpenAI).Usage
		if got.Prompt != 40 || got.Completion != 2 {
			t.Fatalf("usage = %d/%d, want injected 40/2", got.Prompt, got.Completion)
		}
		if got.Source != contract.UsageUpstreamPartial {
			t.Fatalf("Source = %v, want upstream_partial", got.Source)
		}
	})

	t.Run("注入为零时用响应头", func(t *testing.T) {
		got := ExtractUsageMultimodal(&contract.Response{Model: "m"}, headers, &contract.Usage{}, contract.ChannelTypeOpenAI).Usage
		if got.Prompt != 777 || got.Completion != 777 {
			t.Fatalf("usage = %d/%d, want header 777/777", got.Prompt, got.Completion)
		}
		if got.Source != contract.UsageUpstreamExact {
			t.Fatalf("Source = %v, want upstream_exact", got.Source)
		}
	})
}

// ----------------------------------------------------------------------------
// Raw 键保留：OpenAI / Gemini 不得被误判为 Claude
// ----------------------------------------------------------------------------

func TestUsageMultimodalOpenAINotClaude(t *testing.T) {
	resp := &contract.Response{
		Model: "gpt-4o",
		Raw: []byte(`{"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,
			"prompt_tokens_details":{"cached_tokens":30,"image_tokens":50}}}`),
	}
	got := ExtractUsageMultimodal(resp, nil, nil, contract.ChannelTypeOpenAI)
	if !got.Usage.PromptIncludesCache || !got.Usage.ReasoningIncludedInCompletion {
		t.Fatalf("OpenAI 关系布尔 = %v/%v, want true/true",
			got.Usage.PromptIncludesCache, got.Usage.ReasoningIncludedInCompletion)
	}
	if _, ok := got.Usage.Raw["prompt_tokens"]; !ok {
		t.Fatalf("Raw 未保留 OpenAI 键: %#v", got.Usage.Raw)
	}
	if _, ok := got.Usage.Raw["cache_read_input_tokens"]; ok {
		t.Fatal("OpenAI Raw 被伪造 Claude 键（会被 M3 误判 usage_semantic=anthropic）")
	}
	// 图像理解：prompt_tokens_details.image_tokens 是 prompt 的子集。
	if got.Usage.PromptDetail.Image != 50 {
		t.Fatalf("PromptDetail.Image = %d, want 50", got.Usage.PromptDetail.Image)
	}
}

func TestUsageMultimodalGeminiNotClaude(t *testing.T) {
	resp := &contract.Response{
		Model: "gemini-3-pro",
		Raw:   []byte(`{"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":2,"totalTokenCount":12}}`),
	}
	got := ExtractUsageMultimodal(resp, nil, nil, contract.ChannelTypeGemini).Usage
	if _, ok := got.Raw["promptTokenCount"]; !ok {
		t.Fatalf("Raw 未保留 Gemini 键: %#v", got.Raw)
	}
	if _, ok := got.Raw["input_tokens"]; ok {
		t.Fatal("Gemini Raw 含 input_tokens（会被误判为 Claude）")
	}
	if got.Prompt != 10 || got.Completion != 2 || got.Total != 12 {
		t.Fatalf("usage = %d/%d/%d, want 10/2/12", got.Prompt, got.Completion, got.Total)
	}
}

// ----------------------------------------------------------------------------
// (d) 图片张数：只按 payload 推导（relay-04 §8.1）
// ----------------------------------------------------------------------------

func TestUsageMultimodalImageCountForms(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want int
	}{
		{"对象形 url", `{"data":{"url":"https://x/a.png"}}`, 1},
		{"对象形 b64", `{"data":{"b64_json":"QUJD"}}`, 1},
		{"对象形无 payload", `{"data":{"revised_prompt":"x"}}`, 0},
		{"数组形", `{"data":[{"url":"u1"},{"b64_json":"b2"}]}`, 1},
		{"拆分形只计一次", `{"data":[{"url":"u1"},{"b64_json":"b1"},{"url":"u2"},{"b64_json":"b2"}]}`, 2},
		{"空数组", `{"data":[]}`, 0},
		{"非图片响应无 data 键", `{"id":"x"}`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := &contract.Response{Model: "gpt-image-2", Raw: []byte(tc.raw)}
			got := ExtractUsageMultimodal(resp, nil, nil, contract.ChannelTypeOpenAI)
			if got.ImageCount != tc.want {
				t.Fatalf("ImageCount = %d, want %d", got.ImageCount, tc.want)
			}
			if got.Usage.Source == contract.UsageReservedDefault {
				t.Fatalf("合法/非图片 payload 不应标 reserved_default")
			}
		})
	}
}

func TestUsageMultimodalGeminiInlineImageCount(t *testing.T) {
	resp := &contract.Response{
		Model: "gemini-3-pro-image",
		Raw: []byte(`{
			"candidates":[{"content":{"parts":[
				{"text":"here"},
				{"inlineData":{"mimeType":"image/png","data":"QUJD"}},
				{"inline_data":{"data":"REVG"}}
			]}}],
			"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":2}
		}`),
	}
	got := ExtractUsageMultimodal(resp, nil, nil, contract.ChannelTypeGemini)
	if got.ImageCount != 2 {
		t.Fatalf("ImageCount = %d, want 2（只计带 payload 的内联图片）", got.ImageCount)
	}
	if got.Usage.Source == contract.UsageReservedDefault {
		t.Fatal("合法内联图片不应标 reserved_default")
	}

	// 文本对话（无 inlineData 键）不得被当成图片语义。
	textResp := &contract.Response{
		Model: "gemini-3-pro",
		Raw:   []byte(`{"candidates":[{"content":{"parts":[{"text":"hi"}]}}],"usageMetadata":{"promptTokenCount":1}}`),
	}
	if got := ExtractUsageMultimodal(textResp, nil, nil, contract.ChannelTypeGemini); got.ImageCount != 0 {
		t.Fatalf("文本对话 ImageCount = %d, want 0", got.ImageCount)
	}
}

func TestUsageMultimodalImageCountInvalidKeepsReservation(t *testing.T) {
	for _, raw := range []string{
		`{"data":123}`,
		`{"data":"oops"}`,
		`{"data":[{"url":123}]}`,
	} {
		resp := &contract.Response{Model: "gpt-image-2", Raw: []byte(raw)}
		got := ExtractUsageMultimodal(resp, nil, nil, contract.ChannelTypeOpenAI)
		if got.Usage.Source != contract.UsageReservedDefault {
			t.Fatalf("raw=%s: Source = %v, want reserved_default（保持预留量）", raw, got.Usage.Source)
		}
		if got.ImageCount != 0 {
			t.Fatalf("raw=%s: ImageCount = %d, want 0（不得凭空造数）", raw, got.ImageCount)
		}
	}
}

// ----------------------------------------------------------------------------
// (d) 音频：时长 → token 经 pricing 系数（禁止硬编码）
// ----------------------------------------------------------------------------

func TestUsageMultimodalAudioConversion(t *testing.T) {
	t.Run("转写 duration=90s", func(t *testing.T) {
		resp := &contract.Response{
			Model: "whisper-1",
			Raw:   []byte(`{"text":"hello","duration":90}`),
		}
		got := ExtractUsageMultimodal(resp, nil, nil, contract.ChannelTypeOpenAI).Usage
		if got.AudioSeconds != 90 {
			t.Fatalf("AudioSeconds = %d, want 90", got.AudioSeconds)
		}
		want := pricing.AudioTokensFromSeconds(90)
		if got.PromptDetail.Audio != want {
			t.Fatalf("PromptDetail.Audio = %d, want pricing 系数换算值 %d", got.PromptDetail.Audio, want)
		}
		// 1 分钟 = 1000 token 的唯一真源在 pricing：90s 必须等于 1500。
		if want != int64(90.0/60.0*float64(pricing.AudioTokensPerMinute())) {
			t.Fatalf("pricing 系数漂移：90s = %d", want)
		}
		if got.PromptDetail.Audio != 1500 {
			t.Fatalf("90s 音频 token = %d, want 1500", got.PromptDetail.Audio)
		}
	})

	t.Run("不足 1 秒向上取整", func(t *testing.T) {
		resp := &contract.Response{Model: "whisper-1", Raw: []byte(`{"text":"hi","duration":1.2}`)}
		got := ExtractUsageMultimodal(resp, nil, nil, contract.ChannelTypeOpenAI).Usage
		if got.AudioSeconds != 2 {
			t.Fatalf("AudioSeconds = %d, want ceil(1.2)=2", got.AudioSeconds)
		}
		if got.PromptDetail.Audio != pricing.AudioTokensFromSeconds(1.2) {
			t.Fatalf("PromptDetail.Audio = %d, want %d",
				got.PromptDetail.Audio, pricing.AudioTokensFromSeconds(1.2))
		}
	})

	t.Run("TTS 二进制流输出音频", func(t *testing.T) {
		resp := &contract.Response{
			Model:        "tts-1",
			BinaryStream: strings.NewReader("audio-bytes"),
		}
		injected := &contract.Usage{AudioSeconds: 60, Source: contract.UsageUpstreamPartial}
		got := ExtractUsageMultimodal(resp, nil, injected, contract.ChannelTypeOpenAI).Usage
		if got.AudioSeconds != 60 {
			t.Fatalf("AudioSeconds = %d, want 60", got.AudioSeconds)
		}
		if got.CompletionDetail.Audio != pricing.AudioTokensPerMinute() {
			t.Fatalf("CompletionDetail.Audio = %d, want %d（1 分钟）",
				got.CompletionDetail.Audio, pricing.AudioTokensPerMinute())
		}
		if got.PromptDetail.Audio != 0 {
			t.Fatalf("TTS 不应写入输入音频维度：%d", got.PromptDetail.Audio)
		}
	})
}

// ----------------------------------------------------------------------------
// 嵌入 / 重排：按 prompt tokens
// ----------------------------------------------------------------------------

func TestUsageMultimodalEmbeddingAndRerank(t *testing.T) {
	t.Run("嵌入 prompt_tokens", func(t *testing.T) {
		resp := &contract.Response{
			Model: "text-embedding-3-large",
			Raw:   []byte(`{"data":[{"embedding":[0.1]}],"usage":{"prompt_tokens":12,"total_tokens":12}}`),
		}
		got := ExtractUsageMultimodal(resp, nil, nil, contract.ChannelTypeOpenAI)
		if got.Usage.Prompt != 12 || got.Usage.Total != 12 {
			t.Fatalf("Prompt/Total = %d/%d, want 12/12", got.Usage.Prompt, got.Usage.Total)
		}
		// 嵌入响应有 data 键但没有 url/b64_json → 张数 0（不影响文本计费）。
		if got.ImageCount != 0 {
			t.Fatalf("ImageCount = %d, want 0", got.ImageCount)
		}
	})

	t.Run("重排只有 total_tokens → prompt", func(t *testing.T) {
		resp := &contract.Response{
			Model: "rerank-2",
			Raw:   []byte(`{"results":[],"usage":{"total_tokens":34}}`),
		}
		got := ExtractUsageMultimodal(resp, nil, nil, contract.ChannelTypeJina).Usage
		if got.Prompt != 34 || got.Completion != 0 {
			t.Fatalf("Prompt/Completion = %d/%d, want 34/0（重排按 prompt）", got.Prompt, got.Completion)
		}
	})

	t.Run("Cohere billed_units", func(t *testing.T) {
		resp := &contract.Response{
			Model: "command-r",
			Raw:   []byte(`{"meta":{"billed_units":{"input_tokens":8,"output_tokens":3}}}`),
		}
		got := ExtractUsageMultimodal(resp, nil, nil, contract.ChannelTypeCohere).Usage
		if got.Prompt != 8 || got.Completion != 3 {
			t.Fatalf("Prompt/Completion = %d/%d, want 8/3", got.Prompt, got.Completion)
		}
	})
}

// ----------------------------------------------------------------------------
// (e) 二进制流中断：终结器只执行一次且错误上抛
// ----------------------------------------------------------------------------

func TestUsageMultimodalOnStreamCloseOnce(t *testing.T) {
	errTerm := errors.New("terminate failed")
	var calls atomic.Int32
	terminator := func() error {
		calls.Add(1)
		return errTerm
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := OnStreamClose(ctx, terminator); !errors.Is(err, errTerm) {
		t.Fatalf("OnStreamClose error = %v, want %v", err, errTerm)
	}
	// 同一关闭周期的重复调用不得再次执行终结器。
	if err := OnStreamClose(ctx, terminator); !errors.Is(err, errTerm) {
		t.Fatalf("第二次调用 error = %v, want %v（错误应上抛且不丢失）", err, errTerm)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("terminator calls = %d, want 1", got)
	}

	// 并发关闭路径（写失败 goroutine + 客户端断开回调）同样只执行一次。
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	var calls2 atomic.Int32
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := OnStreamClose(ctx2, func() error {
				calls2.Add(1)
				return nil
			}); err != nil {
				t.Errorf("OnStreamClose: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if got := calls2.Load(); got != 1 {
		t.Fatalf("并发 terminator calls = %d, want 1", got)
	}

	// 不同上下文互不干扰。
	ctx3, cancel3 := context.WithCancel(context.Background())
	defer cancel3()
	var calls3 atomic.Int32
	_ = OnStreamClose(ctx3, func() error { calls3.Add(1); return nil })
	if calls3.Load() != 1 {
		t.Fatalf("独立上下文 terminator calls = %d, want 1", calls3.Load())
	}

	// 无可区分生命周期（Background/nil）时直接执行，不共享全局守卫。
	var calls4 atomic.Int32
	_ = OnStreamClose(context.Background(), func() error { calls4.Add(1); return nil })
	_ = OnStreamClose(nil, func() error { calls4.Add(1); return nil })
	if calls4.Load() != 2 {
		t.Fatalf("Background/nil terminator calls = %d, want 2", calls4.Load())
	}

	// nil 终结器安全 no-op。
	if err := OnStreamClose(ctx, nil); err != nil {
		t.Fatalf("nil terminator error = %v", err)
	}
}

// ----------------------------------------------------------------------------
// 安全默认：未知 kind / nil 响应不 panic
// ----------------------------------------------------------------------------

func TestUsageMultimodalUnknownKindSafe(t *testing.T) {
	got := ExtractUsageMultimodal(nil, nil, nil, contract.ChannelType("no-such-channel"))
	if got == nil {
		t.Fatal("nil fact")
	}
	if got.Usage.Source != contract.UsageEstimated {
		t.Fatalf("Source = %v, want estimated", got.Usage.Source)
	}
	// OpenAI 族的安全默认关系布尔（即便没有 usage 也是协议属性）。
	if !got.Usage.PromptIncludesCache || !got.Usage.ReasoningIncludedInCompletion {
		t.Fatalf("默认关系布尔 = %v/%v, want true/true",
			got.Usage.PromptIncludesCache, got.Usage.ReasoningIncludedInCompletion)
	}
}
