package channelhealth

// 流式性能探测的单元验收（不连真实上游，全部用 httptest）。
//
// 口径：
//   - TTFT = 请求开始到第一个非空 delta（正文或推理）的毫秒数；
//   - Latency = 请求开始到流结束（迭代器耗尽/出错）的总毫秒数；
//   - OutputTokens 只来自上游上报的 usage.completion_tokens，绝不估算；
//   - 每个 run 独立超时，单个失败不终止整批。
//
// 时间断言只用范围/关系（>0、Latency >= TTFT），避免毫秒级抖动造成假红。

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"newtg/gateway/internal/contract"
)

func newTestPerfRunner(t *testing.T, opener CredentialOpener, client *http.Client, timeout time.Duration) *PerfRunner {
	t.Helper()
	runner, err := NewPerfRunner(ProberConfig{Opener: opener, Client: client, Timeout: timeout})
	if err != nil {
		t.Fatalf("NewPerfRunner: %v", err)
	}
	return runner
}

// sseUpstream 写一段 OpenAI Chat Completions 的 SSE 流：
// 两个正文增量 + 可选 usage 末块 + [DONE]。
func sseUpstream(t *testing.T, withUsage bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("httptest.ResponseWriter 不支持 Flush")
			return
		}
		// 先发响应头再延迟首块：确保 TTFT 是「到首个 token」的时间，
		// 同时让毫秒截断不会把 TTFT 压成 0。
		flusher.Flush()
		time.Sleep(20 * time.Millisecond)
		_, _ = io.WriteString(w, "data: {\"id\":\"1\",\"model\":\"mapped-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Sunny\"}}]}\n\n")
		flusher.Flush()
		_, _ = io.WriteString(w, "data: {\"id\":\"1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\" skies\"}}]}\n\n")
		flusher.Flush()
		if withUsage {
			_, _ = io.WriteString(w, "data: {\"id\":\"1\",\"choices\":[],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":7,\"total_tokens\":16}}\n\n")
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
}

// TestPerfRunOpenAISuccess：完整 SSE（delta + usage）→ Success、TTFT、
// Latency、OutputTokens、TPS 全部有效，且请求形状符合性能探测口径。
func TestPerfRunOpenAISuccess(t *testing.T) {
	const prompt = "Reply with one short sentence about the weather."
	type captured struct {
		method string
		path   string
		auth   string
		body   map[string]any
	}
	got := make(chan captured, 1)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		got <- captured{method: r.Method, path: r.URL.Path, auth: r.Header.Get("Authorization"), body: body}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		flusher.Flush()
		time.Sleep(20 * time.Millisecond)
		_, _ = io.WriteString(w, "data: {\"id\":\"1\",\"model\":\"mapped-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Sunny\"}}]}\n\n")
		flusher.Flush()
		_, _ = io.WriteString(w, "data: {\"id\":\"1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\" skies\"}}]}\n\n")
		flusher.Flush()
		_, _ = io.WriteString(w, "data: {\"id\":\"1\",\"choices\":[],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":7,\"total_tokens\":16}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer upstream.Close()

	runner := newTestPerfRunner(t, sealerOpener(t, "sk-perf"), upstream.Client(), 5*time.Second)
	results := runner.Run(context.Background(),
		probeChannel(string(contract.ChannelTypeOpenAI), upstream.URL, `{"client-model":"mapped-model"}`),
		"client-model", 1)
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	result := results[0]
	if !result.Success || result.ErrorClass != "" {
		t.Fatalf("result = %+v, want success", result)
	}
	if result.TTFTMS == nil || *result.TTFTMS <= 0 {
		t.Fatalf("TTFTMS = %v, want > 0", result.TTFTMS)
	}
	if result.LatencyMS == nil || *result.LatencyMS < *result.TTFTMS {
		t.Fatalf("LatencyMS = %v, want >= TTFT %d", result.LatencyMS, *result.TTFTMS)
	}
	if result.OutputTokens == nil || *result.OutputTokens != 7 {
		t.Fatalf("OutputTokens = %v, want 7", result.OutputTokens)
	}
	if result.TPS == nil || *result.TPS <= 0 {
		t.Fatalf("TPS = %v, want > 0", result.TPS)
	}

	request := <-got
	if request.method != http.MethodPost || request.path != "/v1/chat/completions" {
		t.Fatalf("request = %s %s, want POST /v1/chat/completions", request.method, request.path)
	}
	if request.auth != "Bearer sk-perf" {
		t.Fatalf("Authorization = %q, want Bearer sk-perf", request.auth)
	}
	if request.body["stream"] != true {
		t.Fatalf("stream = %v, want true", request.body["stream"])
	}
	if request.body["max_tokens"] != float64(128) {
		t.Fatalf("max_tokens = %v, want 128", request.body["max_tokens"])
	}
	if request.body["model"] != "mapped-model" {
		t.Fatalf("model = %v, want mapped-model", request.body["model"])
	}
	streamOptions, _ := request.body["stream_options"].(map[string]any)
	if streamOptions["include_usage"] != true {
		t.Fatalf("stream_options = %v, want include_usage=true", request.body["stream_options"])
	}
	messages, _ := request.body["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("messages = %v, want exactly one", request.body["messages"])
	}
	message, _ := messages[0].(map[string]any)
	if message["role"] != string(contract.RoleUser) || message["content"] != prompt {
		t.Fatalf("message = %v, want user prompt %q", message, prompt)
	}
}

// TestPerfRunUpstreamErrorClass：非 2xx 用适配器分类，返回 failed + 稳定类名。
func TestPerfRunUpstreamErrorClass(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{http.StatusUnauthorized, PerfClassUpstreamAuth},
		{http.StatusInternalServerError, PerfClassUpstreamServer},
		{http.StatusTooManyRequests, PerfClassUpstreamRateLimit},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, `{"error":"upstream said no"}`)
			}))
			defer upstream.Close()

			runner := newTestPerfRunner(t, sealerOpener(t, "sk-perf"), upstream.Client(), time.Second)
			result := runner.Run(context.Background(),
				probeChannel(string(contract.ChannelTypeOpenAI), upstream.URL, `{}`), "gpt-test", 1)[0]
			if result.Success {
				t.Fatalf("status %d 不应是 success: %+v", tc.status, result)
			}
			if result.ErrorClass != tc.want {
				t.Fatalf("ErrorClass = %q, want %q", result.ErrorClass, tc.want)
			}
			if result.TTFTMS != nil || result.OutputTokens != nil || result.TPS != nil {
				t.Fatalf("失败结果不应携带流指标: %+v", result)
			}
		})
	}
}

// TestPerfRunTimeoutAndNetwork：传输层失败分为 timeout / network。
func TestPerfRunTimeoutAndNetwork(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		release := make(chan struct{})
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-release
			w.WriteHeader(http.StatusOK)
		}))
		defer upstream.Close()
		defer close(release)

		runner := newTestPerfRunner(t, sealerOpener(t, "sk-perf"), upstream.Client(), 50*time.Millisecond)
		result := runner.Run(context.Background(),
			probeChannel(string(contract.ChannelTypeOpenAI), upstream.URL, `{}`), "gpt-test", 1)[0]
		if result.Success || result.ErrorClass != TransportTimeout {
			t.Fatalf("result = %+v, want timeout failure", result)
		}
	})

	t.Run("network", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		client := upstream.Client()
		closedURL := upstream.URL
		upstream.Close() // 端口关闭后连接被拒绝 → network

		runner := newTestPerfRunner(t, sealerOpener(t, "sk-perf"), client, time.Second)
		result := runner.Run(context.Background(),
			probeChannel(string(contract.ChannelTypeOpenAI), closedURL, `{}`), "gpt-test", 1)[0]
		if result.Success || result.ErrorClass != TransportNetwork {
			t.Fatalf("result = %+v, want network failure", result)
		}
	})
}

// TestPerfRunWithoutUsage：上游不报 usage → Success/TTFT/Latency 有效，
// OutputTokens/TPS 为 nil（绝不按字符数伪造）。
func TestPerfRunWithoutUsage(t *testing.T) {
	upstream := sseUpstream(t, false)
	defer upstream.Close()

	runner := newTestPerfRunner(t, sealerOpener(t, "sk-perf"), upstream.Client(), 5*time.Second)
	result := runner.Run(context.Background(),
		probeChannel(string(contract.ChannelTypeOpenAI), upstream.URL, `{}`), "gpt-test", 1)[0]
	if !result.Success {
		t.Fatalf("result = %+v, want success", result)
	}
	if result.TTFTMS == nil || *result.TTFTMS <= 0 {
		t.Fatalf("TTFTMS = %v, want > 0", result.TTFTMS)
	}
	if result.LatencyMS == nil || *result.LatencyMS < *result.TTFTMS {
		t.Fatalf("LatencyMS = %v, want >= TTFT", result.LatencyMS)
	}
	if result.OutputTokens != nil || result.TPS != nil {
		t.Fatalf("OutputTokens/TPS = %v/%v, want nil/nil", result.OutputTokens, result.TPS)
	}
}

// TestPerfRunBatchesRuns：一次 Run(n) 产生 n 个独立结果，单次失败不终止整批；
// runs 被钳制在 1..10。
func TestPerfRunBatchesRuns(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	runner := newTestPerfRunner(t, sealerOpener(t, "sk-perf"), upstream.Client(), 5*time.Second)
	channel := probeChannel(string(contract.ChannelTypeOpenAI), upstream.URL, `{}`)

	results := runner.Run(context.Background(), channel, "gpt-test", 3)
	if len(results) != 3 {
		t.Fatalf("results = %d, want 3", len(results))
	}
	for i, result := range results {
		if !result.Success {
			t.Fatalf("run %d = %+v, want success", i, result)
		}
	}
	if got := requests.Load(); got != 3 {
		t.Fatalf("upstream requests = %d, want 3", got)
	}

	// 无 adapter 的渠道：精确构造 runs 个本地失败，不发起请求。
	unsupported := probeChannel(string(contract.ChannelTypeAws), "http://127.0.0.1:1", `{}`)
	if got := len(runner.Run(context.Background(), unsupported, "m", 0)); got != 1 {
		t.Fatalf("runs=0 → %d results, want 1", got)
	}
	if got := len(runner.Run(context.Background(), unsupported, "m", 20)); got != 10 {
		t.Fatalf("runs=20 → %d results, want 10", got)
	}
}

// TestPerfRunLocalFailureClasses：凭据/构造/分派失败用稳定类名，且不发请求。
func TestPerfRunLocalFailureClasses(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("本地失败不得发起出站请求")
	}))
	defer upstream.Close()

	t.Run("credential", func(t *testing.T) {
		runner := newTestPerfRunner(t, fakeOpener{err: errTestOpener}, upstream.Client(), time.Second)
		result := runner.Run(context.Background(),
			probeChannel(string(contract.ChannelTypeOpenAI), upstream.URL, `{}`), "gpt-test", 1)[0]
		if result.Success || result.ErrorClass != PerfClassCredential {
			t.Fatalf("result = %+v, want %s", result, PerfClassCredential)
		}
	})

	t.Run("unsupported", func(t *testing.T) {
		runner := newTestPerfRunner(t, sealerOpener(t, "sk-perf"), upstream.Client(), time.Second)
		result := runner.Run(context.Background(),
			probeChannel(string(contract.ChannelTypeAws), upstream.URL, `{}`), "gpt-test", 1)[0]
		if result.Success || result.ErrorClass != PerfClassUnsupported {
			t.Fatalf("result = %+v, want %s", result, PerfClassUnsupported)
		}
	})

	t.Run("build", func(t *testing.T) {
		runner := newTestPerfRunner(t, sealerOpener(t, "sk-perf"), upstream.Client(), time.Second)
		// custom 渠道必须提供 base_url；缺失时 BuildRequest 拒绝（本地配置错误）。
		channel := probeChannel(string(contract.ChannelTypeCustom), "", `{}`)
		result := runner.Run(context.Background(), channel, "gpt-test", 1)[0]
		if result.Success || result.ErrorClass != PerfClassBuild {
			t.Fatalf("result = %+v, want %s", result, PerfClassBuild)
		}
	})
}

var errTestOpener = errors.New("bad credential")
