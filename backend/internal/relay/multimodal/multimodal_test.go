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
	"net"
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
)

// ---------- fake billing ----------

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
		_, _ = w.Write([]byte(`{"data":[{"url":"a"},{"revised_prompt":"skip"},{"b64_json":"b"}]}`))
	})
	recorder := httptest.NewRecorder()
	env.handle(t, contract.EndpointImageGenerate, []byte(`{"model":"m","prompt":"p","n":3,"stream":true}`), "application/json", recorder)

	body := recorder.Body.String()
	if got := strings.Count(body, `"type":"image_generation.completed"`); got != 2 {
		t.Fatalf("completed events = %d, want 2; body=%s", got, body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("missing [DONE]; body=%s", body)
	}
	_, _, facts, _ := env.billing.snapshot()
	if len(facts) != 1 || facts[0].ImageCount != 2 || !facts[0].Stream {
		t.Fatalf("settled fact = %+v, want ImageCount=2 Stream=true", facts)
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

// ---------- realtime skeleton ----------

func TestRealtimeDialUpstream(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buffer := make([]byte, 4096)
		_, _ = conn.Read(buffer)
		_, _ = conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n"))
		time.Sleep(50 * time.Millisecond)
	}()
	adapter := &endpointAdapter{channelType: contract.ChannelTypeOpenAI, defaultBase: "http://" + listener.Addr().String(), plan: &RequestPlan{endpoint: contract.EndpointRealtime}}
	conn, err := adapter.DialUpstream(&contract.RelayContext{Credential: "k"})
	if err != nil {
		t.Fatalf("DialUpstream: %v", err)
	}
	_ = conn.Close()
}

func TestRealtimeRejectsNonUpgrade(t *testing.T) {
	env := newTestEnv(t, contract.ChannelTypeOpenAI, func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called")
	})
	request := httptest.NewRequest(http.MethodGet, "/v1/realtime", nil)
	recorder := httptest.NewRecorder()
	env.dispatcher.Handle(recorder, request, &contract.RelayContext{RequestID: "r", OriginModel: "m", Request: &contract.Request{Model: "m"}}, contract.EndpointRealtime)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
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

// TestTranscriptionDurationSaturatesWithAudit 覆盖 relay-04 §9 的 MaxAudioSeconds：
// 上游测得 7200s 必须饱和到 3600s，并按饱和值重算音频 token，同时留下
// QuotaClamp 审计标记（契约 §12 不变量 9）。
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
