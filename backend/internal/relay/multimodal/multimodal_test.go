package multimodal

// M4 Task 4 的行为测试：端点适配器、图像张数信任顺序、单次结算（R-02）、
// 流式/二进制直传、客户端断连退款、能力拒绝与实时骨架。
//
// 纪律：不连真 DB/Redis/上游；渠道/计费全部是内存 fake，HTTP 上游用
// httptest.Server。

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/relay"
	"newtg/gateway/internal/relay/adapters"
	"newtg/gateway/internal/relay/ir"
	"newtg/gateway/internal/triage"
)

// ---------- fake billing ----------

var testPNGBytes = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1A, '\n'}

type fakeBilling struct {
	mu         sync.Mutex
	preCalls   int
	estimates  []contract.QuotaEstimate
	terminated []contract.UsageFact
	refunds    int
	reserved   int64
}

var _ contract.BillingSession = (*fakeBilling)(nil)

func (f *fakeBilling) PreConsume(est contract.QuotaEstimate) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.preCalls++
	f.estimates = append(f.estimates, est)
	return nil
}
func (f *fakeBilling) Reserve(delta int64) error { return nil }
func (f *fakeBilling) ChargeIncrement(int64, contract.UsageFact) error {
	return nil
}
func (f *fakeBilling) Terminate(fact contract.UsageFact) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.terminated = append(f.terminated, fact)
	return nil
}
func (f *fakeBilling) Refund() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refunds++
	return nil
}
func (f *fakeBilling) Reserved() int64               { return f.reserved }
func (f *fakeBilling) NeedsRefund() bool             { return false }
func (f *fakeBilling) RecoverAbandoned(string) error { return nil }

func (f *fakeBilling) snapshot() (pre, refunds int, facts []contract.UsageFact, estimates []contract.QuotaEstimate) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.preCalls, f.refunds, append([]contract.UsageFact(nil), f.terminated...), append([]contract.QuotaEstimate(nil), f.estimates...)
}

// ---------- fake channel plumbing ----------

type testEnv struct {
	dispatcher *Dispatcher
	billing    *fakeBilling
	upstream   *httptest.Server
	channel    contract.ChannelType
}

func newTestEnv(t *testing.T, channelType contract.ChannelType, handler http.HandlerFunc) *testEnv {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	billing := &fakeBilling{}
	env := &testEnv{upstream: server, billing: billing, channel: channelType}
	env.dispatcher = NewDispatcher(Options{
		Candidates: func(context.Context, *contract.RelayContext) ([]contract.ChannelCandidate, error) {
			return []contract.ChannelCandidate{{ChannelID: 1}}, nil
		},
		ResolveChannel: func(context.Context, *contract.RelayContext, int) (relay.ChannelAssignment, error) {
			return relay.ChannelAssignment{
				ChannelID:     1,
				ChannelType:   channelType,
				APIBase:       server.URL,
				Credential:    "test-credential",
				UpstreamModel: "upstream-model",
			}, nil
		},
		Dispatch: adapters.Dispatch,
		Billing:  func(*contract.RelayContext) contract.BillingSession { return billing },
		Client:   server.Client(),
		WriteError: func(w http.ResponseWriter, requestID string, err error) {
			writeOpenAIError(w, http.StatusBadGateway, requestID, "upstream_error", "upstream request failed")
		},
	})
	return env
}

func (e *testEnv) handle(t *testing.T, endpoint contract.Endpoint, body []byte, contentType string, w http.ResponseWriter) (*contract.RelayContext, *httptest.ResponseRecorder) {
	t.Helper()
	path := "/v1/test"
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	rc := &contract.RelayContext{
		RequestID:   "req-test",
		OriginModel: "test-model",
		Request:     &contract.Request{Model: "test-model", Raw: body},
	}
	recorder, _ := w.(*httptest.ResponseRecorder)
	e.dispatcher.Handle(w, request, rc, endpoint)
	return rc, recorder
}

// ---------- image adapter ----------

func imageAdapterForTest() *endpointAdapter {
	return &endpointAdapter{channelType: contract.ChannelTypeOpenAI, plan: &RequestPlan{endpoint: contract.EndpointImageGenerate}}
}

func TestCountImagesFourForms(t *testing.T) {
	adapter := imageAdapterForTest()
	cases := []struct {
		name string
		body string
		want int
		ok   bool
	}{
		{"object-url", `{"data":{"url":"https://x/1.png"}}`, 1, true},
		{"object-empty", `{"data":{"revised_prompt":"x"}}`, 0, true},
		{"array-max", `{"data":[{"url":"a"},{"url":"b"},{"b64_json":"c"}]}`, 2, true},
		{"split", `{"data":[{"url":"a"},{"b64_json":"a"}]}`, 1, true},
		{"no-payload-item", `{"data":[{"url":"a"},{"revised_prompt":"x"}]}`, 1, true},
		{"no-data", `{"object":"list"}`, 0, false},
		{"malformed-item", `{"data":[{"url":123}]}`, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := adapter.CountImages([]byte(tc.body))
			if ok != tc.ok || (ok && got != tc.want) {
				t.Fatalf("CountImages = (%d,%v), want (%d,%v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestImageUsagePrefersUpstreamCount(t *testing.T) {
	adapter := imageAdapterForTest()
	body := `{"usage":{"image_count":2,"output_image_count":9},"data":[{"url":"a"},{"url":"b"},{"url":"c"}]}`
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(body))}
	count, ok := adapter.ImageUsage(nil, resp)
	if !ok || count != 2 {
		t.Fatalf("ImageUsage = (%d,%v), want (2,true)", count, ok)
	}
	// body 必须被复原，供后续 payload 推导/透传使用。
	restored, _ := io.ReadAll(resp.Body)
	if string(restored) != body {
		t.Fatalf("body not restored: %q", restored)
	}
}

// ---------- image settlement ----------

func TestImageActualUsageSingleSettlement(t *testing.T) {
	env := newTestEnv(t, contract.ChannelTypeOpenAI, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/images/generations" {
			t.Errorf("upstream path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated) // 上游 201 视为成功（relay-04 §4.3）
		_, _ = w.Write([]byte(`{"created":1,"data":[{"url":"a"},{"url":"b"}]}`))
	})
	body := []byte(`{"model":"m","prompt":"p","n":5}`)
	recorder := httptest.NewRecorder()
	env.handle(t, contract.EndpointImageGenerate, body, "application/json", recorder)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", recorder.Code, recorder.Body.String())
	}
	pre, refunds, facts, estimates := env.billing.snapshot()
	if pre != 1 {
		t.Fatalf("PreConsume calls = %d, want 1", pre)
	}
	if refunds != 0 {
		t.Fatalf("Refund calls = %d, want 0", refunds)
	}
	if len(facts) != 1 {
		t.Fatalf("Terminate calls = %d, want 1 (single settlement)", len(facts))
	}
	// R-02：按实际 payload 张数结算，而不是请求的 n=5。
	if facts[0].ImageCount != 2 {
		t.Fatalf("settled ImageCount = %d, want 2", facts[0].ImageCount)
	}
	if len(estimates) != 1 || estimates[0].ImageCount != 5 {
		t.Fatalf("PreConsume estimate = %+v, want ImageCount=5 (placeholder)", estimates)
	}
}

func TestImageUnparseableCountKeepsReservation(t *testing.T) {
	env := newTestEnv(t, contract.ChannelTypeOpenAI, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"url":123}]}`))
	})
	recorder := httptest.NewRecorder()
	env.handle(t, contract.EndpointImageGenerate, []byte(`{"model":"m","prompt":"p","n":3}`), "application/json", recorder)
	_, _, facts, _ := env.billing.snapshot()
	if len(facts) != 1 {
		t.Fatalf("Terminate calls = %d, want 1", len(facts))
	}
	if facts[0].Usage.Source != contract.UsageReservedDefault {
		t.Fatalf("Source = %v, want reserved_default", facts[0].Usage.Source)
	}
}

func TestImageJSONConvertedToSSE(t *testing.T) {
	env := newTestEnv(t, contract.ChannelTypeOpenAI, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// 拆分形（url 条目 + b64 条目）同一张图只计一次（§8.1.1 max）。
		_, _ = w.Write([]byte(`{"created":123,"usage":{"total_tokens":7},"data":[{"url":"a"},{"revised_prompt":"skip"},{"b64_json":"b"}]}`))
	})
	recorder := httptest.NewRecorder()
	env.handle(t, contract.EndpointImageGenerate, []byte(`{"model":"m","prompt":"p","n":3,"stream":true}`), "application/json", recorder)

	body := recorder.Body.String()
	events := strings.Split(body, "\n\n")
	var completed map[string]any
	for _, frame := range events {
		payload := strings.TrimPrefix(strings.TrimSpace(frame), "data: ")
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			t.Fatalf("invalid event %q: %v", payload, err)
		}
		if event["type"] == "image_generation.completed" {
			if completed != nil {
				t.Fatalf("split form must emit exactly one completed event: %s", body)
			}
			completed = event
		}
	}
	if completed == nil {
		t.Fatalf("missing completed event; body=%s", body)
	}
	// F1：字段必须平铺在顶层（url/created_at/usage），不能塞进 data。
	if completed["url"] != "a" {
		t.Fatalf("completed event url = %#v, want top-level \"a\" (flattened)", completed["url"])
	}
	if _, nested := completed["data"]; nested {
		t.Fatalf("completed event must not nest payload under data: %#v", completed)
	}
	if completed["created_at"] != float64(123) {
		t.Fatalf("created_at = %#v, want 123", completed["created_at"])
	}
	usage, _ := completed["usage"].(map[string]any)
	if usage["total_tokens"] != float64(7) {
		t.Fatalf("event usage = %#v, want total_tokens=7", completed["usage"])
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("missing [DONE]; body=%s", body)
	}
	_, _, facts, _ := env.billing.snapshot()
	if len(facts) != 1 || facts[0].ImageCount != 1 || !facts[0].Stream {
		t.Fatalf("settled fact = %+v, want ImageCount=1 Stream=true", facts)
	}
}

func TestImageRealSSEForwarded(t *testing.T) {
	env := newTestEnv(t, contract.ChannelTypeOpenAI, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"type\":\"image_generation.partial_image\"}\n\n")
		flusher.Flush()
		_, _ = io.WriteString(w, "data: {\"type\":\"image_generation.completed\"}\n\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"image_generation.completed\"}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})
	recorder := httptest.NewRecorder()
	env.handle(t, contract.EndpointImageGenerate, []byte(`{"model":"m","prompt":"p","stream":true}`), "application/json", recorder)
	body := recorder.Body.String()
	if !strings.Contains(body, "image_generation.partial_image") {
		t.Fatalf("upstream events not forwarded: %s", body)
	}
	_, _, facts, _ := env.billing.snapshot()
	if len(facts) != 1 || facts[0].ImageCount != 2 {
		t.Fatalf("settled fact = %+v, want ImageCount=2", facts)
	}
}

// ---------- embeddings / rerank ----------

func TestEmbeddingsPassThroughAndUsage(t *testing.T) {
	env := newTestEnv(t, contract.ChannelTypeOpenAI, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			t.Errorf("upstream path = %q", r.URL.Path)
		}
		// F14：给上游加可观测延迟，断言 JSON 路径填充了 LatencyMS。
		time.Sleep(15 * time.Millisecond)
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if payload["model"] != "upstream-model" {
			t.Errorf("model mapping failed: %+v", payload["model"])
		}
		_, _ = w.Write([]byte(`{"object":"list","data":[{"index":0,"embedding":[0.1,0.2]}],"usage":{"prompt_tokens":7,"total_tokens":7}}`))
	})
	body := []byte(`{"model":"client-model","input":["a","b","c"]}`)
	request := httptest.NewRequest(http.MethodPost, "/v1/embeddings", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	rc := &contract.RelayContext{
		RequestID:     "req-emb",
		OriginModel:   "client-model",
		UpstreamModel: "upstream-model",
		Request:       &contract.Request{Model: "client-model", Raw: body},
	}
	recorder := httptest.NewRecorder()
	env.dispatcher.Handle(recorder, request, rc, contract.EndpointEmbeddings)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"embedding":[0.1,0.2]`) {
		t.Fatalf("response not passed through: %s", recorder.Body.String())
	}
	_, _, facts, _ := env.billing.snapshot()
	if len(facts) != 1 || facts[0].Usage.Prompt != 7 {
		t.Fatalf("settled fact = %+v, want Prompt=7", facts)
	}
	if facts[0].LatencyMS < 10 {
		t.Fatalf("LatencyMS = %d, want >= 10 (F14)", facts[0].LatencyMS)
	}
}

func TestRerankTotalTokensMapsToPrompt(t *testing.T) {
	env := newTestEnv(t, contract.ChannelTypeOpenAI, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/rerank" {
			t.Errorf("upstream path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"results":[{"index":0,"relevance_score":0.9}],"usage":{"total_tokens":11}}`))
	})
	recorder := httptest.NewRecorder()
	env.handle(t, contract.EndpointRerank, []byte(`{"model":"m","query":"q","documents":["d1","d2"]}`), "application/json", recorder)
	_, _, facts, _ := env.billing.snapshot()
	if len(facts) != 1 || facts[0].Usage.Prompt != 11 {
		t.Fatalf("settled fact = %+v, want Prompt=11 (total_tokens→prompt)", facts)
	}
}

// ---------- audio ----------

func wavFixture(seconds float64) []byte {
	const byteRate = 8000
	dataSize := int(seconds * byteRate)
	var buf bytes.Buffer
	buf.WriteString("RIFF")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(36+dataSize))
	buf.WriteString("WAVEfmt ")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(16))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1)) // PCM
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1)) // mono
	_ = binary.Write(&buf, binary.LittleEndian, uint32(8000))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(byteRate))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(8))
	buf.WriteString("data")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(dataSize))
	buf.Write(make([]byte, dataSize))
	return buf.Bytes()
}

func TestAudioDurationWAVAndPCM(t *testing.T) {
	if seconds, ok := AudioDuration(wavFixture(2), "wav"); !ok || seconds < 1.99 || seconds > 2.01 {
		t.Fatalf("wav duration = (%v,%v), want ~2", seconds, ok)
	}
	pcm := make([]byte, pcmSampleRate*pcmBytesPerSample*3) // 3 秒
	if seconds, ok := AudioDuration(pcm, "pcm"); !ok || seconds < 2.99 || seconds > 3.01 {
		t.Fatalf("pcm duration = (%v,%v), want ~3", seconds, ok)
	}
}

func TestAudioSpeechStreamsAndSettlesDuration(t *testing.T) {
	audio := wavFixture(60) // 60 秒 → 1000 audio tokens
	env := newTestEnv(t, contract.ChannelTypeOpenAI, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/speech" {
			t.Errorf("upstream path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write(audio)
	})
	recorder := httptest.NewRecorder()
	env.handle(t, contract.EndpointAudioSpeech, []byte(`{"model":"tts","input":"hi","response_format":"wav"}`), "application/json", recorder)

	if !bytes.Equal(recorder.Body.Bytes(), audio) {
		t.Fatalf("binary response not passed through (got %d bytes, want %d)", recorder.Body.Len(), len(audio))
	}
	_, _, facts, _ := env.billing.snapshot()
	if len(facts) != 1 {
		t.Fatalf("Terminate calls = %d, want 1", len(facts))
	}
	if facts[0].Usage.AudioSeconds != 60 || facts[0].Usage.CompletionDetail.Audio != 1000 {
		t.Fatalf("audio usage = %+v, want AudioSeconds=60 AudioTokens=1000", facts[0].Usage)
	}
}

func TestAudioSpeechFallbackPerKB(t *testing.T) {
	payload := bytes.Repeat([]byte{0x00}, 4096) // 未知格式 → 每 KB ≈ 1 token
	env := newTestEnv(t, contract.ChannelTypeOpenAI, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(payload)
	})
	recorder := httptest.NewRecorder()
	env.handle(t, contract.EndpointAudioSpeech, []byte(`{"model":"tts","input":"hi","response_format":"opus"}`), "application/json", recorder)
	_, _, facts, _ := env.billing.snapshot()
	if len(facts) != 1 || facts[0].Usage.CompletionDetail.Audio != 4 {
		t.Fatalf("audio fallback usage = %+v, want 4 tokens", facts)
	}
}

func TestTranscriptionPrefersUpstreamDuration(t *testing.T) {
	env := newTestEnv(t, contract.ChannelTypeOpenAI, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"text":"hello","duration":60}`))
	})
	ct, body := multipartFixture(t, "file", "audio.mp3", append([]byte("ID3\x04\x00\x00\x00\x00\x00\x00"), bytes.Repeat([]byte("a"), 64)...), map[string]string{"model": "whisper-1"})
	recorder := httptest.NewRecorder()
	env.handle(t, contract.EndpointAudioTranscribe, body, ct, recorder)
	pre, _, facts, estimates := env.billing.snapshot()
	if pre != 1 {
		t.Fatalf("PreConsume calls = %d", pre)
	}
	_ = estimates
	if len(facts) != 1 || facts[0].Usage.PromptDetail.Audio != 1000 {
		t.Fatalf("transcription usage = %+v, want 1000 audio tokens", facts)
	}
}

// ---------- failure / refund paths ----------

type failingWriter struct{ header http.Header }

func (f *failingWriter) Header() http.Header       { return f.header }
func (f *failingWriter) WriteHeader(int)           {}
func (f *failingWriter) Write([]byte) (int, error) { return 0, errors.New("client gone") }

func TestClientDisconnectRefundsOnce(t *testing.T) {
	env := newTestEnv(t, contract.ChannelTypeOpenAI, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write(wavFixture(1))
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", bytes.NewReader([]byte(`{"model":"tts","input":"hi","response_format":"wav"}`)))
	request.Header.Set("Content-Type", "application/json")
	rc := &contract.RelayContext{RequestID: "req-x", OriginModel: "tts", Request: &contract.Request{Model: "tts", Raw: []byte(`{"model":"tts","input":"hi"}`)}}
	env.dispatcher.Handle(&failingWriter{header: http.Header{}}, request, rc, contract.EndpointAudioSpeech)
	_, refunds, facts, _ := env.billing.snapshot()
	if refunds != 1 || len(facts) != 0 {
		t.Fatalf("refunds=%d terminates=%d, want refund exactly once", refunds, len(facts))
	}
}

func TestUnsupportedChannelRejectedWithoutUpstream(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	defer server.Close()
	billing := &fakeBilling{}
	dispatcher := NewDispatcher(Options{
		Candidates: func(context.Context, *contract.RelayContext) ([]contract.ChannelCandidate, error) {
			return []contract.ChannelCandidate{{ChannelID: 1}}, nil
		},
		ResolveChannel: func(context.Context, *contract.RelayContext, int) (relay.ChannelAssignment, error) {
			return relay.ChannelAssignment{ChannelID: 1, ChannelType: contract.ChannelTypeMiniMax, APIBase: server.URL, Credential: "k"}, nil
		},
		Dispatch: adapters.Dispatch,
		Billing:  func(*contract.RelayContext) contract.BillingSession { return billing },
		Client:   server.Client(),
		Supports: func(contract.ChannelType) bool { return false },
		WriteError: func(w http.ResponseWriter, requestID string, err error) {
			writeOpenAIError(w, http.StatusBadRequest, requestID, "endpoint_not_supported", "unsupported")
		},
	})
	body := []byte(`{"model":"m","prompt":"p"}`)
	request := httptest.NewRequest(http.MethodPost, "/v1/images/generations", bytes.NewReader(body))
	recorder := httptest.NewRecorder()
	dispatcher.Handle(recorder, request, &contract.RelayContext{RequestID: "r", OriginModel: "m", Request: &contract.Request{Model: "m", Raw: body}}, contract.EndpointImageGenerate)
	if calls != 0 {
		t.Fatalf("upstream calls = %d, want 0 (capability rejection before BuildRequest)", calls)
	}
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
	// PreConsume 已发生（执行层在循环前预扣），失败后必须 Refund。
	_, refunds, facts, _ := billing.snapshot()
	if refunds != 1 || len(facts) != 0 {
		t.Fatalf("refunds=%d terminates=%d, want refund once", refunds, len(facts))
	}
}

func TestLargeBinaryResponseIsCopiedInChunks(t *testing.T) {
	payload := bytes.Repeat([]byte{0x42}, 4<<20)
	env := newTestEnv(t, contract.ChannelTypeOpenAI, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write(payload)
	})
	recorder := httptest.NewRecorder()
	env.handle(t, contract.EndpointAudioSpeech, []byte(`{"model":"tts","input":"hi","response_format":"wav"}`), "application/json", recorder)
	if recorder.Body.Len() != len(payload) {
		t.Fatalf("response bytes = %d, want %d", recorder.Body.Len(), len(payload))
	}
}

// multipartFixture 构造仅含一个文件与若干文本字段的 multipart 请求体。
func multipartFixture(t *testing.T, field, filename string, content []byte, fields map[string]string) (string, []byte) {
	t.Helper()
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	for name, value := range fields {
		if err := writer.WriteField(name, value); err != nil {
			t.Fatal(err)
		}
	}
	part, err := writer.CreateFormFile(field, filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return writer.FormDataContentType(), buf.Bytes()
}

func TestTranscriptionDurationSaturatesWithAudit(t *testing.T) {
	env := newTestEnv(t, contract.ChannelTypeOpenAI, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"text":"hello","duration":7200}`))
	})
	ct, body := multipartFixture(t, "file", "audio.mp3", append([]byte("ID3\x04\x00\x00\x00\x00\x00\x00"), bytes.Repeat([]byte("a"), 64)...), map[string]string{"model": "whisper-1"})
	request := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", bytes.NewReader(body))
	request.Header.Set("Content-Type", ct)
	rc := &contract.RelayContext{
		RequestID:   "req-clamp",
		OriginModel: "whisper-1",
		Request:     &contract.Request{Model: "whisper-1", Raw: body},
	}
	recorder := httptest.NewRecorder()
	env.dispatcher.Handle(recorder, request, rc, contract.EndpointAudioTranscribe)
	_, _, facts, _ := env.billing.snapshot()
	if len(facts) != 1 {
		t.Fatalf("Terminate calls = %d, want 1", len(facts))
	}
	if facts[0].Usage.AudioSeconds != MaxAudioSeconds {
		t.Fatalf("AudioSeconds = %d, want %d", facts[0].Usage.AudioSeconds, MaxAudioSeconds)
	}
	if rc.QuotaClamp == nil || rc.QuotaClamp.Kind != contract.ClampOverflow || rc.QuotaClamp.Clamped != MaxAudioSeconds {
		t.Fatalf("missing quota clamp audit marker: %+v", rc.QuotaClamp)
	}
}

// limitedWriter 只允许前 allow 次 Write 成功，之后模拟客户端断开。
type limitedWriter struct {
	header http.Header
	buf    bytes.Buffer
	allow  int
	writes int
}

func (w *limitedWriter) Header() http.Header { return w.header }
func (w *limitedWriter) WriteHeader(int)     {}
func (w *limitedWriter) Write(p []byte) (int, error) {
	if w.writes >= w.allow {
		return 0, errors.New("client gone")
	}
	w.writes++
	return w.buf.Write(p)
}

// TestImageStreamBillingR15 固化 R-15 / relay-04 §8.1.3：
//   - 正常结束：信任 completed（允许低于请求 n）；
//   - 客户端中断：费用不低于请求 n（max(completed, n)，不整笔退款）；
//   - completed 超过请求 n：按实际更高张数计。
func TestImageStreamBillingR15(t *testing.T) {
	cases := []struct {
		name      string
		requested int
		items     int
		abort     bool
		want      int
	}{
		{"normal-lower-than-requested", 5, 2, false, 2},
		{"abort-not-below-requested", 3, 1, true, 3},
		{"completed-higher-than-requested", 1, 3, false, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t, contract.ChannelTypeOpenAI, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				items := make([]string, 0, tc.items)
				for i := 0; i < tc.items; i++ {
					items = append(items, `{"url":"https://x/`+string(rune('a'+i))+`.png"}`)
				}
				_, _ = w.Write([]byte(`{"data":[` + strings.Join(items, ",") + `]}`))
			})
			body := []byte(`{"model":"m","prompt":"p","n":` + strconv.Itoa(tc.requested) + `,"stream":true}`)
			request := httptest.NewRequest(http.MethodPost, "/v1/images/generations", bytes.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			rc := &contract.RelayContext{RequestID: "r15", OriginModel: "m", Request: &contract.Request{Model: "m", Raw: body}}

			var writer http.ResponseWriter
			if tc.abort {
				// 第一次写事件成功，随后 [DONE] 写入失败 → 中断。
				writer = &limitedWriter{header: http.Header{}, allow: 1}
			} else {
				writer = httptest.NewRecorder()
			}
			env.dispatcher.Handle(writer, request, rc, contract.EndpointImageGenerate)

			_, refunds, facts, _ := env.billing.snapshot()
			if refunds != 0 {
				t.Fatalf("Refund calls = %d, want 0 (image streams settle, not refund)", refunds)
			}
			if len(facts) != 1 {
				t.Fatalf("Terminate calls = %d, want 1", len(facts))
			}
			if facts[0].ImageCount != tc.want {
				t.Fatalf("ImageCount = %d, want %d", facts[0].ImageCount, tc.want)
			}
		})
	}
}

// ---------- 评审修复（F1–F15） ----------

// errorAfterReader 先返回数据，随后返回非 EOF 错误（模拟上游连接重置）。
type errorAfterReader struct {
	data []byte
	done bool
}

func (r *errorAfterReader) Read(p []byte) (int, error) {
	if !r.done && len(r.data) > 0 {
		n := copy(p, r.data)
		r.data = r.data[n:]
		if len(r.data) == 0 {
			r.done = true
		}
		return n, nil
	}
	return 0, errors.New("connection reset by peer")
}

// TestAudioDurationMP3UsesTotalSize（F1）：MP3 按字节率估算，流式直传只有
// 64 KiB 前缀；必须使用总字节数，否则大文件系统性少计。
func TestAudioDurationMP3UsesTotalSize(t *testing.T) {
	prefix := append([]byte("ID3\x04\x00\x00\x00\x00\x00\x00"), 0xFF, 0xFB, 0x90, 0x64)
	prefix = append(prefix, bytes.Repeat([]byte{0}, 64<<10)...)
	total := int64(1 << 20)
	seconds, ok := AudioDurationWithSize(prefix, total, "mp3")
	if !ok {
		t.Fatal("mp3 duration not detected")
	}
	want := float64(total-10) * 8 / 128000
	if seconds < want-2 || seconds > want+2 {
		t.Fatalf("mp3 duration = %v, want ≈%v (must use total size)", seconds, want)
	}
}

// TestAudioSpeechMP3LargeFileDuration 是 F1 的端到端回归：1 MiB MP3 的
// 结算时长必须按总字节数而非 64 KiB 前缀。
func TestAudioSpeechMP3LargeFileDuration(t *testing.T) {
	audio := append([]byte("ID3\x04\x00\x00\x00\x00\x00\x00"), 0xFF, 0xFB, 0x90, 0x64)
	audio = append(audio, bytes.Repeat([]byte{0}, (1<<20)-len(audio))...)
	env := newTestEnv(t, contract.ChannelTypeOpenAI, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write(audio)
	})
	recorder := httptest.NewRecorder()
	env.handle(t, contract.EndpointAudioSpeech, []byte(`{"model":"tts","input":"hi","response_format":"mp3"}`), "application/json", recorder)
	_, _, facts, _ := env.billing.snapshot()
	if len(facts) != 1 {
		t.Fatalf("Terminate calls = %d", len(facts))
	}
	want := int64((len(audio)-10)*8/128000) + 1 // ceil
	if facts[0].Usage.AudioSeconds < want-1 || facts[0].Usage.AudioSeconds > want+1 {
		t.Fatalf("AudioSeconds = %d, want ≈%d (total-size based)", facts[0].Usage.AudioSeconds, want)
	}
}

// TestTranscriptionFallsBackToUploadDuration（F2）：上游省略 usage/duration
// 时用预扣阶段测得的上传文件头时长（60s WAV）结算，不得按 0。
func TestTranscriptionFallsBackToUploadDuration(t *testing.T) {
	env := newTestEnv(t, contract.ChannelTypeOpenAI, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"text":"hello"}`))
	})
	ct, body := multipartFixture(t, "file", "audio.wav", wavFixture(60), map[string]string{"model": "whisper-1"})
	recorder := httptest.NewRecorder()
	env.handle(t, contract.EndpointAudioTranscribe, body, ct, recorder)
	_, _, facts, estimates := env.billing.snapshot()
	if len(facts) != 1 {
		t.Fatalf("Terminate calls = %d", len(facts))
	}
	if len(estimates) != 1 || estimates[0].AudioSeconds != 60 {
		t.Fatalf("pre-consume estimate = %+v, want AudioSeconds=60", estimates)
	}
	if facts[0].Usage.AudioSeconds != 60 || facts[0].Usage.PromptDetail.Audio != 1000 {
		t.Fatalf("settled usage = %+v, want AudioSeconds=60 / 1000 audio tokens", facts[0].Usage)
	}
}

// TestImageStreamReadErrorIsAbnormal（F3）：非 EOF 的读取错误必须按中断
// 结算 max(completed, n)，且不得伪造 [DONE]。
func TestImageStreamReadErrorIsAbnormal(t *testing.T) {
	env := newTestEnv(t, contract.ChannelTypeOpenAI, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
	})
	frame := "data: {\"type\":\"image_generation.completed\"}\n\n"
	body := io.MultiReader(strings.NewReader(frame), &errorAfterReader{})
	result := &relay.AttemptResult{
		Response: &http.Response{Body: io.NopCloser(body), Header: http.Header{"Content-Type": []string{"text/event-stream"}}},
		Adapter:  &endpointAdapter{channelType: contract.ChannelTypeOpenAI, plan: &RequestPlan{endpoint: contract.EndpointImageGenerate}},
	}
	plan := &RequestPlan{endpoint: contract.EndpointImageGenerate, estimate: contract.QuotaEstimate{ImageCount: 3}, started: time.Now()}
	rc := &contract.RelayContext{RequestID: "r", OriginModel: "m"}
	settlement := &settlement{billing: env.billing}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	env.dispatcher.serveImageStream(recorder, request, rc, plan, result, settlement, true)

	if strings.Contains(recorder.Body.String(), "[DONE]") {
		t.Fatalf("forged [DONE] on upstream read error: %s", recorder.Body.String())
	}
	_, refunds, facts, _ := env.billing.snapshot()
	if refunds != 0 || len(facts) != 1 || facts[0].ImageCount != 3 {
		t.Fatalf("refunds=%d facts=%+v, want ImageCount=3 (max(completed,n))", refunds, facts)
	}
}

// TestImageStreamMalformedPayloadKeepsReservation（F5）：类型非法的 payload
// 必须回退 reserved_default，不能按 0 张静默收费。
func TestImageStreamMalformedPayloadKeepsReservation(t *testing.T) {
	env := newTestEnv(t, contract.ChannelTypeOpenAI, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"url":123}]}`))
	})
	recorder := httptest.NewRecorder()
	env.handle(t, contract.EndpointImageGenerate, []byte(`{"model":"m","prompt":"p","n":2,"stream":true}`), "application/json", recorder)
	_, _, facts, _ := env.billing.snapshot()
	if len(facts) != 1 || facts[0].Usage.Source != contract.UsageReservedDefault {
		t.Fatalf("settled fact = %+v, want reserved_default", facts)
	}
}

// TestImageStreamArrayURLCount（F4 补充）：数组形按 url 条目数计，拆分形
// 只计一次（在 TestImageJSONConvertedToSSE 中固化）。
func TestImageStreamArrayURLCount(t *testing.T) {
	env := newTestEnv(t, contract.ChannelTypeOpenAI, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"url":"a"},{"url":"b"}]}`))
	})
	recorder := httptest.NewRecorder()
	env.handle(t, contract.EndpointImageGenerate, []byte(`{"model":"m","prompt":"p","n":5,"stream":true}`), "application/json", recorder)
	_, _, facts, _ := env.billing.snapshot()
	if len(facts) != 1 || facts[0].ImageCount != 2 {
		t.Fatalf("settled fact = %+v, want ImageCount=2", facts)
	}
}

// TestUpstreamOmittedUsageUsesLocalEstimate（F7）：嵌入/重排无 usage 时按
// 请求文本做本地估算（estimated），而不是 0。
func TestUpstreamOmittedUsageUsesLocalEstimate(t *testing.T) {
	env := newTestEnv(t, contract.ChannelTypeOpenAI, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[]}`))
	})
	recorder := httptest.NewRecorder()
	env.handle(t, contract.EndpointEmbeddings, []byte(`{"model":"m","input":["hello world"]}`), "application/json", recorder)
	_, _, facts, _ := env.billing.snapshot()
	if len(facts) != 1 || facts[0].Usage.Prompt <= 0 || facts[0].Usage.Source != contract.UsageEstimated {
		t.Fatalf("embedding fact = %+v, want estimated Prompt>0", facts)
	}

	env2 := newTestEnv(t, contract.ChannelTypeOpenAI, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results":[]}`))
	})
	recorder2 := httptest.NewRecorder()
	env2.handle(t, contract.EndpointRerank, []byte(`{"model":"m","query":"q","documents":["doc one","doc two"]}`), "application/json", recorder2)
	_, _, facts2, _ := env2.billing.snapshot()
	if len(facts2) != 1 || facts2[0].Usage.Prompt <= 0 || facts2[0].Usage.Source != contract.UsageEstimated {
		t.Fatalf("rerank fact = %+v, want estimated Prompt>0", facts2)
	}
}

// TestAdapterRevalidatesImageBounds（F9）：适配器本地必须拒绝越界的 n
// （防御性二次校验，CCR-9）。
func TestAdapterRevalidatesImageBounds(t *testing.T) {
	adapter := &endpointAdapter{
		channelType: contract.ChannelTypeOpenAI,
		defaultBase: "http://example.invalid",
		base:        adapters.NewOpenAI(),
		plan: &RequestPlan{
			endpoint:    contract.EndpointImageGenerate,
			contentType: "application/json",
			rawBody:     []byte(`{"model":"m","prompt":"p","n":999}`),
		},
	}
	_, err := adapter.BuildRequest(&contract.RelayContext{Credential: "k"}, &contract.Request{Model: "m", Raw: adapter.plan.rawBody})
	class, ok := adapters.ClassOf(err)
	if !ok || class != contract.ErrParamInvalid {
		t.Fatalf("BuildRequest error = %v (class=%v), want ErrParamInvalid", err, class)
	}
}

// TestMultipartModelMapping（F10）：multipart 的 model 字段必须映射为上游
// 模型名。
func TestMultipartModelMapping(t *testing.T) {
	var seenModel string
	env := newTestEnv(t, contract.ChannelTypeOpenAI, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(1 << 20)
		seenModel = r.FormValue("model")
		_, _ = w.Write([]byte(`{"data":[{"url":"a"}]}`))
	})
	ct, body := multipartFixture(t, "image", "image.png", append(append([]byte(nil), testPNGBytes...), []byte("x")...), map[string]string{"model": "client-model", "prompt": "p", "n": "1"})
	recorder := httptest.NewRecorder()
	env.handle(t, contract.EndpointImageEdit, body, ct, recorder)
	if seenModel != "upstream-model" {
		t.Fatalf("upstream model = %q, want upstream-model", seenModel)
	}
}

// TestAzureExcludedFromMultimodalMatrix（F11）：Azure 的部署名/api-version
// 路径规则尚未对齐，必须显式拒绝而不是发出错误路径。
func TestAzureExcludedFromMultimodalMatrix(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	defer server.Close()
	billing := &fakeBilling{}
	dispatcher := NewDispatcher(Options{
		Candidates: func(context.Context, *contract.RelayContext) ([]contract.ChannelCandidate, error) {
			return []contract.ChannelCandidate{{ChannelID: 1}}, nil
		},
		ResolveChannel: func(context.Context, *contract.RelayContext, int) (relay.ChannelAssignment, error) {
			return relay.ChannelAssignment{ChannelID: 1, ChannelType: contract.ChannelTypeAzure, APIBase: server.URL, Credential: "k"}, nil
		},
		Dispatch: adapters.Dispatch,
		Billing:  func(*contract.RelayContext) contract.BillingSession { return billing },
		Client:   server.Client(),
		WriteError: func(w http.ResponseWriter, requestID string, err error) {
			writeOpenAIError(w, http.StatusBadRequest, requestID, "endpoint_not_supported", "unsupported")
		},
	})
	body := []byte(`{"model":"m","prompt":"p"}`)
	request := httptest.NewRequest(http.MethodPost, "/v1/images/generations", bytes.NewReader(body))
	recorder := httptest.NewRecorder()
	dispatcher.Handle(recorder, request, &contract.RelayContext{RequestID: "r", OriginModel: "m", Request: &contract.Request{Model: "m", Raw: body}}, contract.EndpointImageGenerate)
	if calls != 0 || recorder.Code != http.StatusBadRequest {
		t.Fatalf("calls=%d status=%d, want 0/400", calls, recorder.Code)
	}
}

// TestGeminiEmbeddingPath（F8/R-11）：Gemini 语义嵌入路径复用客户端路径，
// 使用 x-goog-api-key 鉴权，响应原样回写。
func TestGeminiEmbeddingPath(t *testing.T) {
	var seenPath, seenKey string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenKey = r.Header.Get("x-goog-api-key")
		_, _ = w.Write([]byte(`{"embedding":{"values":[0.1,0.2]}}`))
	}))
	defer server.Close()
	billing := &fakeBilling{}
	dispatcher := NewDispatcher(Options{
		Candidates: func(context.Context, *contract.RelayContext) ([]contract.ChannelCandidate, error) {
			return []contract.ChannelCandidate{{ChannelID: 1}}, nil
		},
		ResolveChannel: func(context.Context, *contract.RelayContext, int) (relay.ChannelAssignment, error) {
			return relay.ChannelAssignment{ChannelID: 1, ChannelType: contract.ChannelTypeGemini, APIBase: server.URL, Credential: "g-key"}, nil
		},
		Dispatch: adapters.Dispatch,
		Billing:  func(*contract.RelayContext) contract.BillingSession { return billing },
		Client:   server.Client(),
		WriteError: func(w http.ResponseWriter, requestID string, err error) {
			writeOpenAIError(w, http.StatusBadGateway, requestID, "upstream_error", "failed")
		},
	})
	body := []byte(`{"content":{"parts":[{"text":"hello"}]}}`)
	path := "/v1beta/models/text-embedding-004:embedContent"
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	rc := &contract.RelayContext{RequestID: "gem", OriginModel: "text-embedding-004", RequestPath: path, Request: &contract.Request{Model: "text-embedding-004", Raw: body}}
	recorder := httptest.NewRecorder()
	dispatcher.Handle(recorder, request, rc, contract.EndpointEmbeddings)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "embedding") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if seenPath != path || seenKey != "g-key" {
		t.Fatalf("upstream path=%q key=%q", seenPath, seenKey)
	}
	_, _, facts, _ := billing.snapshot()
	if len(facts) != 1 || facts[0].Usage.Prompt <= 0 {
		t.Fatalf("gemini embedding fact = %+v, want estimated Prompt>0", facts)
	}
}

// TestGeminiNativeEmbeddingNoFalseConversion 钉死 NF-1：Gemini 原生嵌入路径
// （:embedContent / :batchEmbedContents）的 body 原样透传，入站协议必须如实
// 标成 Gemini，不得记录 `openai → gemini_generate_content` 的请求转换，
// 4xx 也就不会误判成网关责任的 GW-01。
//
// 对照组 `/v1/engines/{m}/embeddings`：入站是 OpenAI input，适配器确实把它
// 翻译成 Gemini content.parts，因此必须仍记录一步转换（4xx → GW-01）。
func TestGeminiNativeEmbeddingNoFalseConversion(t *testing.T) {
	drive := func(t *testing.T, path string, body []byte) (*contract.RelayContext, bool) {
		t.Helper()
		hit := false
		env := newTestEnv(t, contract.ChannelTypeGemini, func(w http.ResponseWriter, _ *http.Request) {
			hit = true
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":400,"message":"bad"}}`))
		})
		request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		rc := &contract.RelayContext{
			RequestID:   "gemini-embed",
			OriginModel: "text-embedding-004",
			RequestPath: path,
			Request:     &contract.Request{Model: "text-embedding-004", Raw: body},
		}
		env.dispatcher.Handle(httptest.NewRecorder(), request, rc, contract.EndpointEmbeddings)
		return rc, hit
	}

	t.Run("native embedContent records no request step", func(t *testing.T) {
		body := []byte(`{"content":{"parts":[{"text":"hello"}]}}`)
		rc, hit := drive(t, "/v1beta/models/text-embedding-004:embedContent", body)
		if !hit {
			t.Fatal("上游未被调用：测试没有走到请求转换生产者接缝")
		}
		if rc.Request == nil || len(rc.ConversionChain) != 0 {
			t.Fatalf("ConversionChain = %+v, want no request step（body 未被重编码）", rc.ConversionChain)
		}
		if rc.Request.RawFormat != ir.FormatGeminiGenerateContent {
			t.Fatalf("Request.RawFormat = %#v, want gemini native", rc.Request)
		}
		verdict := triage.Classify(triage.Evidence{
			UpstreamStatus:   400,
			Attempts:         1,
			ConversionSteps:  len(rc.ConversionChain),
			RequestStructure: "valid",
		})
		if verdict.Rule == "GW-01" || verdict.Owner != triage.OwnerUnknown {
			t.Fatalf("verdict = %+v, 原生嵌入的 4xx 不得判 GW-01", verdict)
		}
	})

	t.Run("engines embedding still records the translation", func(t *testing.T) {
		body := []byte(`{"input":"hello"}`)
		rc, hit := drive(t, "/v1/engines/text-embedding-004/embeddings", body)
		if !hit {
			t.Fatal("上游未被调用：engines 路径没有走到请求转换生产者接缝")
		}
		if len(rc.ConversionChain) != 1 {
			t.Fatalf("ConversionChain = %+v, want exactly one request step", rc.ConversionChain)
		}
		step := rc.ConversionChain[0]
		if step.Stage != "request" || step.From != ir.FormatOpenAIChat || step.To != ir.FormatGeminiGenerateContent {
			t.Fatalf("request step = %+v, want request openai→gemini_generate_content", step)
		}
		verdict := triage.Classify(triage.Evidence{
			UpstreamStatus:   400,
			Attempts:         1,
			ConversionSteps:  len(rc.ConversionChain),
			RouteFrom:        string(step.From),
			RouteTo:          string(step.To),
			RequestStructure: "valid",
		})
		if verdict.Rule != "GW-01" || verdict.Owner != triage.OwnerGateway {
			t.Fatalf("verdict = %+v, engines 形态的真实转换 + 4xx 应判 GW-01", verdict)
		}
	})
}

// signalingWriter 在首次 Write 时关闭 first，用于断言二进制响应是分块
// 直传而不是整段缓冲后再写（F13）。
type signalingWriter struct {
	inner http.ResponseWriter
	once  sync.Once
	first chan struct{}
}

func (w *signalingWriter) Header() http.Header    { return w.inner.Header() }
func (w *signalingWriter) WriteHeader(status int) { w.inner.WriteHeader(status) }
func (w *signalingWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.first) })
	return w.inner.Write(p)
}

func TestBinaryStreamIsCopiedInChunks(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write(bytes.Repeat([]byte{1}, 32<<10))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-release
		_, _ = w.Write(bytes.Repeat([]byte{2}, 32<<10))
	}))
	defer upstream.Close()
	billing := &fakeBilling{}
	dispatcher := NewDispatcher(Options{
		Candidates: func(context.Context, *contract.RelayContext) ([]contract.ChannelCandidate, error) {
			return []contract.ChannelCandidate{{ChannelID: 1}}, nil
		},
		ResolveChannel: func(context.Context, *contract.RelayContext, int) (relay.ChannelAssignment, error) {
			return relay.ChannelAssignment{ChannelID: 1, ChannelType: contract.ChannelTypeOpenAI, APIBase: upstream.URL, Credential: "k"}, nil
		},
		Dispatch: adapters.Dispatch,
		Billing:  func(*contract.RelayContext) contract.BillingSession { return billing },
		Client:   upstream.Client(),
		WriteError: func(w http.ResponseWriter, requestID string, err error) {
			writeOpenAIError(w, http.StatusBadGateway, requestID, "upstream_error", "failed")
		},
	})
	body := []byte(`{"model":"tts","input":"hi","response_format":"wav"}`)
	request := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	rc := &contract.RelayContext{RequestID: "chunk", OriginModel: "tts", Request: &contract.Request{Model: "tts", Raw: body}}
	recorder := httptest.NewRecorder()
	first := make(chan struct{})
	w := &signalingWriter{inner: recorder, first: first}
	done := make(chan struct{})
	go func() {
		dispatcher.Handle(w, request, rc, contract.EndpointAudioSpeech)
		close(done)
	}()
	select {
	case <-first:
		// 第一块已写出，而上游仍阻塞在 release：证明没有整段缓冲。
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("first chunk was not written before upstream completed (buffered)")
	}
	close(release)
	<-done
	if recorder.Body.Len() != 64<<10 {
		t.Fatalf("response bytes = %d, want %d", recorder.Body.Len(), 64<<10)
	}
}

// ---------- 复审残余 N1–N6 ----------

// TestTranscriptionPartialUsageAddsAudio（N1）：上游 usage 只有文本 token
// 时，音频维度仍需用测得的上传时长补齐。
func TestTranscriptionPartialUsageAddsAudio(t *testing.T) {
	env := newTestEnv(t, contract.ChannelTypeOpenAI, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"text":"hi","usage":{"prompt_tokens":5}}`))
	})
	ct, body := multipartFixture(t, "file", "audio.wav", wavFixture(60), map[string]string{"model": "whisper-1"})
	recorder := httptest.NewRecorder()
	env.handle(t, contract.EndpointAudioTranscribe, body, ct, recorder)
	_, _, facts, _ := env.billing.snapshot()
	if len(facts) != 1 {
		t.Fatalf("Terminate calls = %d", len(facts))
	}
	usage := facts[0].Usage
	if usage.Prompt != 5 || usage.AudioSeconds != 60 || usage.PromptDetail.Audio != 1000 {
		t.Fatalf("usage = %+v, want Prompt=5 AudioSeconds=60 AudioTokens=1000", usage)
	}
	if usage.Source != contract.UsageEstimated {
		t.Fatalf("Source = %v, want estimated (mixed upstream text + measured audio)", usage.Source)
	}
}

// TestGeminiEnginesPathTranslates（N2）：/v1/engines/{model}/embeddings 的
// 多输入必须翻译为 :batchEmbedContents 的 requests[]（每个输入一个 embedding），
// 并映射 dimensions → outputDimensionality。
func TestGeminiEnginesPathTranslates(t *testing.T) {
	var (
		seenPath string
		seenBody map[string]any
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&seenBody)
		_, _ = w.Write([]byte(`{"embeddings":[{"values":[0.1]},{"values":[0.2]}]}`))
	}))
	defer server.Close()
	billing := &fakeBilling{}
	dispatcher := NewDispatcher(Options{
		Candidates: func(context.Context, *contract.RelayContext) ([]contract.ChannelCandidate, error) {
			return []contract.ChannelCandidate{{ChannelID: 1}}, nil
		},
		ResolveChannel: func(context.Context, *contract.RelayContext, int) (relay.ChannelAssignment, error) {
			return relay.ChannelAssignment{ChannelID: 1, ChannelType: contract.ChannelTypeGemini, APIBase: server.URL, Credential: "g"}, nil
		},
		Dispatch: adapters.Dispatch,
		Billing:  func(*contract.RelayContext) contract.BillingSession { return billing },
		Client:   server.Client(),
		WriteError: func(w http.ResponseWriter, requestID string, err error) {
			writeOpenAIError(w, http.StatusBadGateway, requestID, "upstream_error", "failed")
		},
	})
	body := []byte(`{"input":["one","two"],"dimensions":256}`)
	path := "/v1/engines/text-embedding-004/embeddings"
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	rc := &contract.RelayContext{RequestID: "eng", OriginModel: "text-embedding-004", RequestPath: path, Request: &contract.Request{Model: "text-embedding-004", Raw: body}}
	recorder := httptest.NewRecorder()
	dispatcher.Handle(recorder, request, rc, contract.EndpointEmbeddings)

	if seenPath != "/v1beta/models/text-embedding-004:batchEmbedContents" {
		t.Fatalf("upstream path = %q", seenPath)
	}
	requests, _ := seenBody["requests"].([]any)
	if len(requests) != 2 {
		t.Fatalf("requests = %#v, want 2", seenBody["requests"])
	}
	first, _ := requests[0].(map[string]any)
	if first["outputDimensionality"] != float64(256) {
		t.Fatalf("outputDimensionality = %#v, want 256", first["outputDimensionality"])
	}
	if _, exists := seenBody["model"]; exists {
		t.Fatalf("translated batch body must not carry a top-level model: %#v", seenBody)
	}
	// 无 usage 时用 OpenAI 形状的 input 做本地估算（不是 Gemini parts）。
	_, _, facts, _ := billing.snapshot()
	if len(facts) != 1 || facts[0].Usage.Prompt <= 0 {
		t.Fatalf("engines estimate = %+v, want Prompt>0 from input", facts)
	}
}

// TestGeminiEnginesSingleInput：单输入 → :embedContent + content.parts。
func TestGeminiEnginesSingleInput(t *testing.T) {
	var (
		seenPath string
		seenBody map[string]any
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&seenBody)
		_, _ = w.Write([]byte(`{"embedding":{"values":[0.1]}}`))
	}))
	defer server.Close()
	billing := &fakeBilling{}
	dispatcher := NewDispatcher(Options{
		Candidates: func(context.Context, *contract.RelayContext) ([]contract.ChannelCandidate, error) {
			return []contract.ChannelCandidate{{ChannelID: 1}}, nil
		},
		ResolveChannel: func(context.Context, *contract.RelayContext, int) (relay.ChannelAssignment, error) {
			return relay.ChannelAssignment{ChannelID: 1, ChannelType: contract.ChannelTypeGemini, APIBase: server.URL, Credential: "g"}, nil
		},
		Dispatch: adapters.Dispatch,
		Billing:  func(*contract.RelayContext) contract.BillingSession { return billing },
		Client:   server.Client(),
		WriteError: func(w http.ResponseWriter, requestID string, err error) {
			writeOpenAIError(w, http.StatusBadGateway, requestID, "upstream_error", "failed")
		},
	})
	body := []byte(`{"input":"one"}`)
	path := "/v1/engines/text-embedding-004/embeddings"
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	rc := &contract.RelayContext{RequestID: "eng", OriginModel: "text-embedding-004", RequestPath: path, Request: &contract.Request{Model: "text-embedding-004", Raw: body}}
	dispatcher.Handle(httptest.NewRecorder(), request, rc, contract.EndpointEmbeddings)
	if seenPath != "/v1beta/models/text-embedding-004:embedContent" {
		t.Fatalf("upstream path = %q", seenPath)
	}
	content, _ := seenBody["content"].(map[string]any)
	parts, _ := content["parts"].([]any)
	if len(parts) != 1 {
		t.Fatalf("content.parts = %#v, want 1", content)
	}
}

// TestJoinBaseURLDedupesVersion（N3）：base 与客户端路径的任意版本段对
// （/v1、/v1beta、/v1alpha 等）都不得拼出双版本。
func TestJoinBaseURLDedupesVersion(t *testing.T) {
	cases := []struct {
		base, path, want string
	}{
		{"https://x/v1beta", "/v1beta/models/m:embedContent", "https://x/v1beta/models/m:embedContent"},
		{"https://x/v1", "/v1beta/models/m:embedContent", "https://x/v1/models/m:embedContent"},
		{"https://x/v1alpha", "/v1/models/m:embedContent", "https://x/v1alpha/models/m:embedContent"},
		{"https://x/v1beta1", "/v1beta/models/m:embedContent", "https://x/v1beta1/models/m:embedContent"},
		{"https://x", "/v1beta/models/m:embedContent", "https://x/v1beta/models/m:embedContent"},
	}
	for _, tc := range cases {
		got, err := joinBaseURL(tc.base, tc.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Fatalf("joinBaseURL(%q,%q) = %q, want %q", tc.base, tc.path, got, tc.want)
		}
	}
}

// TestParseMultipartLargeField（N4）：70 KiB 的 parameters 是入口允许的合法
// 字段，dispatcher 二次解析不得截断或拒绝。
func TestParseMultipartLargeField(t *testing.T) {
	parameters := `{"n":2,"pad":"` + strings.Repeat("a", 70<<10) + `"}`
	ct, body := multipartFixture(t, "image", "image.png", append(append([]byte(nil), testPNGBytes...), 'x'), map[string]string{
		"model": "m", "prompt": "p", "parameters": parameters,
	})
	parsed, err := parseMultipart(body, ct, false)
	if err != nil {
		t.Fatalf("parseMultipart: %v", err)
	}
	if len(parsed.fields["parameters"]) != len(parameters) {
		t.Fatalf("parameters length = %d, want %d (truncated)", len(parsed.fields["parameters"]), len(parameters))
	}
}

// ---------- 整分支终审 F1–F9 ----------

// TestEndpointChannelMatrix（F2）：端点×渠道类型白名单必须拒绝 relay-04
// 中标注为「否」的 OpenAI 兼容类型（deepseek/mistral 等），并放行「是」组。
func TestEndpointChannelMatrix(t *testing.T) {
	type testCase struct {
		name     string
		channel  contract.ChannelType
		endpoint contract.Endpoint
		body     func() ([]byte, string)
		support  bool
	}
	jsonBody := func(raw string) func() ([]byte, string) {
		return func() ([]byte, string) { return []byte(raw), "application/json" }
	}
	cases := []testCase{
		{"embeddings-deepseek-no", contract.ChannelTypeDeepSeek, contract.EndpointEmbeddings, jsonBody(`{"model":"m","input":"x"}`), false},
		{"embeddings-mistral-no", contract.ChannelTypeMistral, contract.EndpointEmbeddings, jsonBody(`{"model":"m","input":"x"}`), false},
		{"embeddings-openai-yes", contract.ChannelTypeOpenAI, contract.EndpointEmbeddings, jsonBody(`{"model":"m","input":"x"}`), true},
		{"embeddings-ali-yes", contract.ChannelTypeAli, contract.EndpointEmbeddings, jsonBody(`{"model":"m","input":"x"}`), true},
		{"embeddings-siliconflow-yes", contract.ChannelTypeSiliconFlow, contract.EndpointEmbeddings, jsonBody(`{"model":"m","input":"x"}`), true},
		// R-W18-11：能力真源是 Caps().Endpoints。Gemini 只在专用语义路径上
		// 声明嵌入；普通 /v1/embeddings 的 OpenAI 形状仍必须拒绝。
		{"embeddings-gemini-generic-no", contract.ChannelTypeGemini, contract.EndpointEmbeddings, jsonBody(`{"model":"m","input":"x"}`), false},
		// NewAPI 没有适配器实现（declaredAdapter 不声明任何端点）。
		{"embeddings-newapi-no", contract.ChannelTypeNewAPI, contract.EndpointEmbeddings, jsonBody(`{"model":"m","input":"x"}`), false},
		{"rerank-deepseek-no", contract.ChannelTypeDeepSeek, contract.EndpointRerank, jsonBody(`{"model":"m","query":"q","documents":["d"]}`), false},
		{"rerank-openai-yes", contract.ChannelTypeOpenAI, contract.EndpointRerank, jsonBody(`{"model":"m","query":"q","documents":["d"]}`), true},
		{"rerank-xinference-yes", contract.ChannelTypeXinference, contract.EndpointRerank, jsonBody(`{"model":"m","query":"q","documents":["d"]}`), true},
		{"image-deepseek-no", contract.ChannelTypeDeepSeek, contract.EndpointImageGenerate, jsonBody(`{"model":"m","prompt":"p"}`), false},
		{"image-xai-yes", contract.ChannelTypeXai, contract.EndpointImageGenerate, jsonBody(`{"model":"m","prompt":"p"}`), true},
		{"speech-volcengine-yes", contract.ChannelTypeVolcEngine, contract.EndpointAudioSpeech, jsonBody(`{"model":"m","input":"x"}`), true},
		{"speech-deepseek-no", contract.ChannelTypeDeepSeek, contract.EndpointAudioSpeech, jsonBody(`{"model":"m","input":"x"}`), false},
		// R-W18-14：审核只声明给 OpenAI 渠道（没有跨厂商等价形态）。
		{"moderations-openai-yes", contract.ChannelTypeOpenAI, contract.EndpointModerations, jsonBody(`{"model":"m","input":"x"}`), true},
		{"moderations-deepseek-no", contract.ChannelTypeDeepSeek, contract.EndpointModerations, jsonBody(`{"model":"m","input":"x"}`), false},
		{"moderations-siliconflow-no", contract.ChannelTypeSiliconFlow, contract.EndpointModerations, jsonBody(`{"model":"m","input":"x"}`), false},
		{"transcribe-volcengine-no", contract.ChannelTypeVolcEngine, contract.EndpointAudioTranscribe, func() ([]byte, string) {
			ct, body := multipartFixture(t, "file", "a.mp3", append([]byte("ID3\x04\x00\x00\x00\x00\x00\x00"), bytes.Repeat([]byte("a"), 32)...), map[string]string{"model": "m"})
			return body, ct
		}, false},
		{"transcribe-siliconflow-yes", contract.ChannelTypeSiliconFlow, contract.EndpointAudioTranscribe, func() ([]byte, string) {
			ct, body := multipartFixture(t, "file", "a.mp3", append([]byte("ID3\x04\x00\x00\x00\x00\x00\x00"), bytes.Repeat([]byte("a"), 32)...), map[string]string{"model": "m"})
			return body, ct
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			env := newTestEnv(t, tc.channel, func(w http.ResponseWriter, r *http.Request) {
				calls++
				_, _ = w.Write([]byte(`{"data":[{"url":"a"}]}`))
			})
			body, ct := tc.body()
			recorder := httptest.NewRecorder()
			env.handle(t, tc.endpoint, body, ct, recorder)
			_, refunds, facts, _ := env.billing.snapshot()
			if tc.support {
				if calls != 1 || len(facts) != 1 || refunds != 0 {
					t.Fatalf("supported: calls=%d facts=%d refunds=%d, want 1/1/0", calls, len(facts), refunds)
				}
			} else {
				if calls != 0 || len(facts) != 0 || refunds != 1 {
					t.Fatalf("unsupported: calls=%d facts=%d refunds=%d, want 0/0/1", calls, len(facts), refunds)
				}
			}
		})
	}
}

// TestModerationSettlesOnceWithLocalEstimate（R-W18-14 / relay-04 §5）：
// 审核请求经同一条 ExecuteAttempts/结算接缝；上游未给 usage 时按 input
// 字节本地估算一次，响应原样回写。
func TestModerationSettlesOnceWithLocalEstimate(t *testing.T) {
	upstreamBody := `{"id":"modr-1","model":"upstream-model","results":[{"flagged":false}]}`
	env := newTestEnv(t, contract.ChannelTypeOpenAI, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/moderations" {
			t.Errorf("path = %q, want /v1/moderations", r.URL.Path)
		}
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if payload["model"] != "upstream-model" {
			t.Errorf("upstream model = %v, want upstream-model（缺省模型必须写回 body）", payload["model"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, upstreamBody)
	})
	recorder := httptest.NewRecorder()
	env.handle(t, contract.EndpointModerations, []byte(`{"input":"hello world"}`), "application/json", recorder)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"id":"modr-1"`) {
		t.Fatalf("status=%d body=%s, want 200 passthrough", recorder.Code, recorder.Body.String())
	}
	pre, refunds, facts, _ := env.billing.snapshot()
	if pre != 1 || refunds != 0 || len(facts) != 1 {
		t.Fatalf("pre=%d refunds=%d facts=%d, want 1/0/1（计费一次）", pre, refunds, len(facts))
	}
	if facts[0].Usage.Source != contract.UsageEstimated || facts[0].Usage.Prompt <= 0 {
		t.Fatalf("usage = %+v, want estimated prompt tokens", facts[0].Usage)
	}
}

// TestModerationUnsupportedChannelRejected（R-W18-11/R-W18-14）：Caps 不含
// moderations 的渠道必须在 BuildRequest 前被 ErrEndpointUnsupported 拒绝，
// 零上游、零结算事实、退款一次。
func TestModerationUnsupportedChannelRejected(t *testing.T) {
	calls := 0
	env := newTestEnv(t, contract.ChannelTypeDeepSeek, func(w http.ResponseWriter, r *http.Request) {
		calls++
	})
	recorder := httptest.NewRecorder()
	env.handle(t, contract.EndpointModerations, []byte(`{"input":"hello"}`), "application/json", recorder)
	if calls != 0 {
		t.Fatalf("upstream calls = %d, want 0", calls)
	}
	_, refunds, facts, _ := env.billing.snapshot()
	if refunds != 1 || len(facts) != 0 {
		t.Fatalf("refunds=%d facts=%d, want refund once / no fact", refunds, len(facts))
	}
	if recorder.Code == http.StatusOK {
		t.Fatalf("unsupported channel must not return 200; body=%s", recorder.Body.String())
	}
}

// TestAACNotParsedAsMP3（F3）：ADTS AAC 帧头（layer=00）不得按 MP3 解析。
func TestAACNotParsedAsMP3(t *testing.T) {
	aac := []byte{0xFF, 0xF1, 0x50, 0x80, 0x00, 0x1F, 0xFC}
	aac = append(aac, bytes.Repeat([]byte{0}, 100)...)
	if seconds, ok := AudioDuration(aac, "aac"); ok {
		t.Fatalf("AAC parsed as %v seconds, want ok=false (fallback path)", seconds)
	}
}

// TestGeminiModelMapping（F4）：Gemini 语义路径上的模型必须应用渠道映射。
func TestGeminiModelMapping(t *testing.T) {
	var seenPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		_, _ = w.Write([]byte(`{"embedding":{"values":[0.1]}}`))
	}))
	defer server.Close()
	billing := &fakeBilling{}
	dispatcher := NewDispatcher(Options{
		Candidates: func(context.Context, *contract.RelayContext) ([]contract.ChannelCandidate, error) {
			return []contract.ChannelCandidate{{ChannelID: 1}}, nil
		},
		ResolveChannel: func(context.Context, *contract.RelayContext, int) (relay.ChannelAssignment, error) {
			return relay.ChannelAssignment{ChannelID: 1, ChannelType: contract.ChannelTypeGemini, APIBase: server.URL, Credential: "g", UpstreamModel: "upstream-embed"}, nil
		},
		Dispatch: adapters.Dispatch,
		Billing:  func(*contract.RelayContext) contract.BillingSession { return billing },
		Client:   server.Client(),
		WriteError: func(w http.ResponseWriter, requestID string, err error) {
			writeOpenAIError(w, http.StatusBadGateway, requestID, "upstream_error", "failed")
		},
	})
	body := []byte(`{"content":{"parts":[{"text":"hi"}]}}`)
	path := "/v1beta/models/client-embed:embedContent"
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	rc := &contract.RelayContext{RequestID: "map", OriginModel: "client-embed", RequestPath: path, Request: &contract.Request{Model: "client-embed", Raw: body}}
	dispatcher.Handle(httptest.NewRecorder(), request, rc, contract.EndpointEmbeddings)
	if seenPath != "/v1beta/models/upstream-embed:embedContent" {
		t.Fatalf("upstream path = %q, want mapped model", seenPath)
	}
}

// TestImageNIntegralFloat（F5）：入口接受的 `n:2.0` 不得在分派阶段 400。
func TestImageNIntegralFloat(t *testing.T) {
	env := newTestEnv(t, contract.ChannelTypeOpenAI, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"url":"a"},{"url":"b"}]}`))
	})
	recorder := httptest.NewRecorder()
	env.handle(t, contract.EndpointImageGenerate, []byte(`{"model":"m","prompt":"p","n":2.0}`), "application/json", recorder)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	_, _, facts, estimates := env.billing.snapshot()
	if len(estimates) != 1 || estimates[0].ImageCount != 2 {
		t.Fatalf("estimate = %+v, want ImageCount=2", estimates)
	}
	if len(facts) != 1 || facts[0].ImageCount != 2 {
		t.Fatalf("fact = %+v, want ImageCount=2", facts)
	}
}

// TestImageTokenFloor（F9）：上游零 token 的图像 usage 需置 1。
func TestImageTokenFloor(t *testing.T) {
	env := newTestEnv(t, contract.ChannelTypeOpenAI, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"url":"a"}]}`))
	})
	recorder := httptest.NewRecorder()
	env.handle(t, contract.EndpointImageGenerate, []byte(`{"model":"m","prompt":"p"}`), "application/json", recorder)
	_, _, facts, _ := env.billing.snapshot()
	if len(facts) != 1 {
		t.Fatalf("Terminate calls = %d", len(facts))
	}
	if facts[0].Usage.Prompt != 1 || facts[0].Usage.Total != 1 {
		t.Fatalf("usage = %+v, want Prompt=Total=1 (token floor)", facts[0].Usage)
	}
}

// TestStreamInactivityWatchdog（F7）：上游停滞时看门狗必须关闭连接并退款，
// 不得无限挂起。
func TestStreamInactivityWatchdog(t *testing.T) {
	stalled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/wav")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		select {
		case <-r.Context().Done():
		case <-stalled:
		}
	}))
	defer upstream.Close()
	defer close(stalled)
	billing := &fakeBilling{}
	dispatcher := NewDispatcher(Options{
		Candidates: func(context.Context, *contract.RelayContext) ([]contract.ChannelCandidate, error) {
			return []contract.ChannelCandidate{{ChannelID: 1}}, nil
		},
		ResolveChannel: func(context.Context, *contract.RelayContext, int) (relay.ChannelAssignment, error) {
			return relay.ChannelAssignment{ChannelID: 1, ChannelType: contract.ChannelTypeOpenAI, APIBase: upstream.URL, Credential: "k"}, nil
		},
		Dispatch:                adapters.Dispatch,
		Billing:                 func(*contract.RelayContext) contract.BillingSession { return billing },
		Client:                  upstream.Client(),
		StreamInactivityTimeout: 50 * time.Millisecond,
		WriteError: func(w http.ResponseWriter, requestID string, err error) {
			writeOpenAIError(w, http.StatusBadGateway, requestID, "upstream_error", "failed")
		},
	})
	body := []byte(`{"model":"tts","input":"hi"}`)
	request := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	rc := &contract.RelayContext{RequestID: "watch", OriginModel: "tts", Request: &contract.Request{Model: "tts", Raw: body}}
	start := time.Now()
	done := make(chan struct{})
	go func() {
		dispatcher.Handle(httptest.NewRecorder(), request, rc, contract.EndpointAudioSpeech)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stalled upstream was not interrupted by the inactivity watchdog")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("watchdog took %v, want ~50ms", elapsed)
	}
	_, refunds, facts, _ := billing.snapshot()
	if refunds != 1 || len(facts) != 0 {
		t.Fatalf("refunds=%d facts=%d, want refund once", refunds, len(facts))
	}
}

// TestID3PrefixedAAC（N3）：带 ID3 标签的 ADTS AAC 同样不得按 MP3 解析。
func TestID3PrefixedAAC(t *testing.T) {
	aac := append([]byte("ID3\x04\x00\x00\x00\x00\x00\x00"), 0xFF, 0xF1, 0x50, 0x80)
	aac = append(aac, bytes.Repeat([]byte{0}, 200)...)
	for _, hint := range []string{"", "aac"} {
		if seconds, ok := AudioDurationWithSize(aac, int64(len(aac)), hint); ok {
			t.Fatalf("hint %q: ID3-prefixed AAC parsed as %v seconds, want ok=false", hint, seconds)
		}
	}
}
