package channelhealth

// W7/M4-b：共享探测的单元验收（不连真实上游，全部用 httptest）。
//
// 覆盖 brief §3 的探测口径：三族 adapter 的请求构造/凭据位置、15s 超时
// （测试注入短超时）、无 adapter 跳过、凭据解封失败跳过、错误体 64 KiB
// 上界。真实出站策略（transport）由 httpapi.NewChannelProbeClient 提供，
// 这里用 httptest.Client 复用其 TLS/连接配置。

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/store"
)

// fakeOpener 返回固定明文或错误，模拟 httpapi.SecretSealer。
type fakeOpener struct {
	plaintext []byte
	err       error
}

func (f fakeOpener) Open([]byte) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.plaintext, nil
}

func sealerOpener(t *testing.T, apiKey string) fakeOpener {
	t.Helper()
	raw, err := json.Marshal(map[string]string{"api_key": apiKey})
	if err != nil {
		t.Fatalf("marshal credentials: %v", err)
	}
	return fakeOpener{plaintext: raw}
}

func newTestProber(t *testing.T, opener CredentialOpener, client *http.Client, timeout time.Duration) *Prober {
	t.Helper()
	prober, err := NewProber(ProberConfig{Opener: opener, Client: client, Timeout: timeout})
	if err != nil {
		t.Fatalf("NewProber: %v", err)
	}
	return prober
}

func probeChannel(typ string, baseURL string, mapping string) *store.Channel {
	return &store.Channel{
		ID:           1,
		Type:         typ,
		BaseURL:      &baseURL,
		Credentials:  []byte("sealed"),
		ModelMapping: []byte(mapping),
		CreatedAt:    time.Now().UTC(),
	}
}

// TestProberRequestShapes 覆盖三族适配器的路径、凭据头与模型映射。
func TestProberRequestShapes(t *testing.T) {
	cases := []struct {
		name       string
		channelTyp contract.ChannelType
		wantPath   string
		wantHeader [2]string // name, value
	}{
		{
			name:       "openai",
			channelTyp: contract.ChannelTypeOpenAI,
			wantPath:   "/v1/chat/completions",
			wantHeader: [2]string{"Authorization", "Bearer sk-probe"},
		},
		{
			name:       "anthropic",
			channelTyp: contract.ChannelTypeAnthropic,
			wantPath:   "/v1/messages",
			wantHeader: [2]string{"x-api-key", "sk-probe"},
		},
		{
			name:       "gemini",
			channelTyp: contract.ChannelTypeGemini,
			wantPath:   "/v1beta/models/mapped-model:generateContent",
			wantHeader: [2]string{"x-goog-api-key", "sk-probe"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var (
				mu      sync.Mutex
				gotPath string
				gotHdr  string
				gotBody map[string]any
			)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				gotPath = r.URL.Path
				if tc.channelTyp == contract.ChannelTypeGemini {
					gotPath = r.URL.EscapedPath()
				}
				gotHdr = r.Header.Get(tc.wantHeader[0])
				body, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(body, &gotBody)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{}`)
			}))
			defer upstream.Close()

			prober := newTestProber(t, sealerOpener(t, "sk-probe"), upstream.Client(), 0)
			result := prober.Probe(context.Background(),
				probeChannel(string(tc.channelTyp), upstream.URL, `{"client-model":"mapped-model"}`),
				"client-model")
			if !result.Success || result.TransportClass != "" || result.Unsupported {
				t.Fatalf("探测结果 = %+v, want success", result)
			}
			if gotPath != tc.wantPath {
				t.Fatalf("path = %q, want %q", gotPath, tc.wantPath)
			}
			if gotHdr != tc.wantHeader[1] {
				t.Fatalf("%s = %q, want %q", tc.wantHeader[0], gotHdr, tc.wantHeader[1])
			}
			// 模型映射必须生效：OpenAI/Anthropic 写在请求体，Gemini 写在路径
			// （wantPath 已断言 mapped-model）。
			if tc.channelTyp != contract.ChannelTypeGemini {
				if model, _ := gotBody["model"].(string); model != "mapped-model" {
					t.Fatalf("上游 model = %v, want mapped-model（body=%v）", gotBody["model"], gotBody)
				}
			}
		})
	}
}

// TestProberClassifiesUpstreamFailures：401/403 → ErrUpstreamAuth；5xx/429
// 保留 body 供关键词匹配；非 2xx 不返回 Success。
func TestProberClassifiesUpstreamFailures(t *testing.T) {
	cases := []struct {
		status int
		class  contract.ErrorClass
		body   string
	}{
		{http.StatusUnauthorized, contract.ErrUpstreamAuth, `{"error":"invalid api key"}`},
		{http.StatusForbidden, contract.ErrUpstreamAuth, `{"error":"forbidden"}`},
		{http.StatusInternalServerError, contract.ErrUpstreamServer, `{"error":"boom"}`},
		{http.StatusTooManyRequests, contract.ErrUpstreamRateLimit, "Your credit balance is too low"},
	}
	for _, tc := range cases {
		t.Run(tc.body, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer upstream.Close()

			prober := newTestProber(t, sealerOpener(t, "sk-probe"), upstream.Client(), 0)
			result := prober.Probe(context.Background(),
				probeChannel(string(contract.ChannelTypeOpenAI), upstream.URL, `{}`), "gpt-test")
			if result.Success {
				t.Fatalf("status %d 不应是 success", tc.status)
			}
			if result.StatusCode != tc.status || result.ErrorClass != tc.class {
				t.Fatalf("status/class = %d/%d, want %d/%d",
					result.StatusCode, result.ErrorClass, tc.status, tc.class)
			}
			if string(result.Body) != tc.body {
				t.Fatalf("body = %q, want %q", result.Body, tc.body)
			}
		})
	}
}

// TestProberSkipsUnsupportedAndInvalidCredentials：无 adapter / 凭据非法都
// 是「无法探测」，调用方据此跳过（R-M4-06），绝不判失败。
func TestProberSkipsUnsupportedAndInvalidCredentials(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("unsupported/credential-invalid 渠道不得发起出站请求")
	}))
	defer upstream.Close()

	t.Run("unsupported", func(t *testing.T) {
		// aws 在分派表里有条目但没有已实现 adapter，且无回退（relay-02 §2.3）。
		prober := newTestProber(t, sealerOpener(t, "sk-probe"), upstream.Client(), 0)
		result := prober.Probe(context.Background(),
			probeChannel(string(contract.ChannelTypeAws), upstream.URL, `{}`), "gpt-test")
		if !result.Unsupported || result.Success {
			t.Fatalf("result = %+v, want Unsupported", result)
		}
	})

	t.Run("opener-error", func(t *testing.T) {
		prober := newTestProber(t, fakeOpener{err: errors.New("bad key")}, upstream.Client(), 0)
		result := prober.Probe(context.Background(),
			probeChannel(string(contract.ChannelTypeOpenAI), upstream.URL, `{}`), "gpt-test")
		if !result.CredentialInvalid {
			t.Fatalf("result = %+v, want CredentialInvalid", result)
		}
	})

	t.Run("empty-api-key", func(t *testing.T) {
		prober := newTestProber(t, sealerOpener(t, ""), upstream.Client(), 0)
		result := prober.Probe(context.Background(),
			probeChannel(string(contract.ChannelTypeOpenAI), upstream.URL, `{}`), "gpt-test")
		if !result.CredentialInvalid {
			t.Fatalf("result = %+v, want CredentialInvalid", result)
		}
	})

	t.Run("nil-channel", func(t *testing.T) {
		prober := newTestProber(t, sealerOpener(t, "sk-probe"), upstream.Client(), 0)
		if result := prober.Probe(context.Background(), nil, "gpt-test"); !result.CredentialInvalid {
			t.Fatalf("result = %+v, want CredentialInvalid", result)
		}
	})
}

// TestProberTimeoutIsTransportFailure：超过单次探测上限只记传输层 timeout，
// 不是「渠道故障」——判定层据此不禁用（R-M4-01 C / R-M4-14）。
func TestProberTimeoutIsTransportFailure(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer func() {
		close(release)
		upstream.Close()
	}()

	prober := newTestProber(t, sealerOpener(t, "sk-probe"), upstream.Client(), 50*time.Millisecond)
	start := time.Now()
	result := prober.Probe(context.Background(),
		probeChannel(string(contract.ChannelTypeOpenAI), upstream.URL, `{}`), "gpt-test")
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("探测未按注入的 50ms 超时返回：%v", elapsed)
	}
	if result.TransportClass != TransportTimeout {
		t.Fatalf("transport class = %q, want timeout", result.TransportClass)
	}
}

// TestProberBoundsErrorBody：响应体只读 64 KiB（关键词匹配的上界），且超长
// 体不会导致探测本身失败。
func TestProberBoundsErrorBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, strings.Repeat("a", ProbeMaxErrorBodyBytes*3))
	}))
	defer upstream.Close()

	prober := newTestProber(t, sealerOpener(t, "sk-probe"), upstream.Client(), 0)
	result := prober.Probe(context.Background(),
		probeChannel(string(contract.ChannelTypeOpenAI), upstream.URL, `{}`), "gpt-test")
	if len(result.Body) != ProbeMaxErrorBodyBytes {
		t.Fatalf("body 长度 = %d, want %d", len(result.Body), ProbeMaxErrorBodyBytes)
	}
	if result.ErrorClass != contract.ErrUpstreamServer {
		t.Fatalf("class = %d, want ErrUpstreamServer", result.ErrorClass)
	}
}
