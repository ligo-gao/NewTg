package channelhealth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"newtg/gateway/internal/contract"
)

// base_url 以 /v1 结尾时，relay（adapters.joinURL）会去掉重复的 /v1 前缀；
// 模型目录必须同口径，否则填 `https://host/v1` 的渠道能转发但拉不到模型。
func TestModelListerAcceptsBaseURLWithV1Suffix(t *testing.T) {
	cases := []struct {
		name string
		typ  contract.ChannelType
	}{
		{"openai", contract.ChannelTypeOpenAI},
		{"anthropic", contract.ChannelTypeAnthropic},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotPaths []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPaths = append(gotPaths, r.URL.Path)
				if r.URL.Path != "/v1/models" {
					http.NotFound(w, r)
					return
				}
				_, _ = w.Write([]byte(`{"data":[{"id":"m1"},{"id":"m2"}],"has_more":false}`))
			}))
			defer server.Close()

			for _, suffix := range []string{"/v1", "/v1/"} {
				gotPaths = nil
				models, err := NewModelLister().List(context.Background(), ModelListRequest{
					ChannelType: tc.typ,
					BaseURL:     server.URL + suffix,
					APIKey:      "sk-test",
				})
				if err != nil {
					t.Fatalf("base %q: List: %v", suffix, err)
				}
				if len(models) != 2 {
					t.Fatalf("base %q: models = %v", suffix, models)
				}
				if got := strings.Join(gotPaths, ","); got != "/v1/models" {
					t.Fatalf("base %q: requested paths = %q", suffix, got)
				}
			}
		})
	}
}

func TestModelListerOpenAICompatible(t *testing.T) {
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.URL.Path != "/v1/models" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-4o"},{"id":"gpt-4o"},{"id":"o3"}]}`))
	}))
	defer server.Close()

	lister := NewModelLister()
	models, err := lister.List(context.Background(), ModelListRequest{
		ChannelType: contract.ChannelTypeOpenAI,
		BaseURL:     server.URL,
		APIKey:      "sk-test",
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if gotAuth != "Bearer sk-test" {
		t.Fatalf("authorization = %q", gotAuth)
	}
	if len(models) != 2 || models[0] != "gpt-4o" || models[1] != "o3" {
		t.Fatalf("models = %v", models)
	}
}

func TestModelListerAnthropicPaginates(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("x-api-key") != "sk-ant" {
			t.Fatalf("missing x-api-key")
		}
		if r.Header.Get("anthropic-version") == "" {
			t.Fatalf("missing anthropic-version")
		}
		if r.URL.Path != "/v1/models" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		if r.URL.Query().Get("after_id") == "" {
			_, _ = w.Write([]byte(`{"data":[{"id":"claude-sonnet-4-5"}],"has_more":true,"last_id":"page-2"}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"claude-opus-4-1"}],"has_more":false}`))
	}))
	defer server.Close()

	lister := NewModelLister()
	models, err := lister.List(context.Background(), ModelListRequest{
		ChannelType: contract.ChannelTypeAnthropic,
		BaseURL:     server.URL,
		APIKey:      "sk-ant",
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if requests != 2 {
		t.Fatalf("requests = %d", requests)
	}
	if len(models) != 2 || models[1] != "claude-opus-4-1" {
		t.Fatalf("models = %v", models)
	}
}

func TestModelListerGeminiStripsPrefixAndPaginates(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("x-goog-api-key") != "gem-key" {
			t.Fatalf("missing x-goog-api-key")
		}
		if r.URL.Path != "/v1beta/models" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		if r.URL.Query().Get("pageToken") == "" {
			_, _ = w.Write([]byte(`{"models":[{"name":"models/gemini-2.5-pro"}],"nextPageToken":"p2"}`))
			return
		}
		_, _ = w.Write([]byte(`{"models":[{"name":"models/gemini-2.5-flash"}]}`))
	}))
	defer server.Close()

	lister := NewModelLister()
	models, err := lister.List(context.Background(), ModelListRequest{
		ChannelType: contract.ChannelTypeGemini,
		BaseURL:     server.URL,
		APIKey:      "gem-key",
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if requests != 2 {
		t.Fatalf("requests = %d", requests)
	}
	if len(models) != 2 || models[0] != "gemini-2.5-pro" || models[1] != "gemini-2.5-flash" {
		t.Fatalf("models = %v", models)
	}
}

// OpenAI 兼容目录里的 created 是 Unix 秒，转成 YYYY-MM-DD 作为「发布日期」；
// 上游没给 created 时保持空字符串，绝不猜测。
func TestListDetailedOpenAICompatibleMetadata(t *testing.T) {
	body := `{"data":[
		{"id":"gpt-4o","created":1715644800,"owned_by":"openai"},
		{"id":"text-embedding-3-small","created":1706140800},
		{"id":"no-created"}
	]}`
	infos, err := parseOpenAIModels([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(infos) != 3 {
		t.Fatalf("len = %d", len(infos))
	}
	if infos[0].ReleasedAt != "2024-05-14" {
		t.Fatalf("released_at = %q", infos[0].ReleasedAt)
	}
	if infos[1].ReleasedAt != "2024-01-25" {
		t.Fatalf("released_at = %q", infos[1].ReleasedAt)
	}
	if infos[2].ReleasedAt != "" {
		t.Fatalf("missing created should stay empty, got %q", infos[2].ReleasedAt)
	}
	for _, info := range infos {
		if info.Source != "openai_models" {
			t.Fatalf("source = %q", info.Source)
		}
	}
	// gpt-4o / no-created 没有用途线索 → 对话补全；text-embedding-3-small
	// 明确是 embedding 族 → 只给 embeddings，不并列 chat。
	wantEndpoints := map[string]string{
		"gpt-4o":                 "chat.completions",
		"text-embedding-3-small": "embeddings",
		"no-created":             "chat.completions",
	}
	for _, info := range infos {
		want, ok := wantEndpoints[info.ID]
		if !ok {
			t.Fatalf("unexpected model %q", info.ID)
		}
		if len(info.Endpoints) != 1 || info.Endpoints[0] != want {
			t.Fatalf("%s: endpoints = %v, want [%s]", info.ID, info.Endpoints, want)
		}
	}
}

// 端点建议只按模型 ID 里明确的用途线索分流：命中第一个族后只给该族，绝不
// 并列 chat；没有线索的对话模型（含 audio-preview）回落到 chat.completions。
func TestSuggestOpenAIEndpoints(t *testing.T) {
	cases := []struct {
		id   string
		want string
	}{
		{"text-embedding-3-small", "embeddings"},
		{"bge-reranker-v2-m3", "rerank"},
		{"whisper-1", "audio.transcriptions"},
		{"tts-1", "audio.speech"},
		{"dall-e-3", "images.generations"},
		{"gpt-image-1", "images.generations"},
		{"gpt-4o", "chat.completions"},
		{"gpt-4o-audio-preview", "chat.completions"},
		{"claude-3-5-sonnet", "chat.completions"},
		// 大小写不敏感。
		{"TEXT-EMBEDDING-3-LARGE", "embeddings"},
		{"WHISPER-1", "audio.transcriptions"},
	}
	for _, testCase := range cases {
		got := suggestOpenAIEndpoints(testCase.id)
		if len(got) != 1 || got[0] != testCase.want {
			t.Fatalf("suggestOpenAIEndpoints(%q) = %v, want [%s]", testCase.id, got, testCase.want)
		}
	}
}

// created 在不同上游可能是字符串、小数或毫秒时间戳。解析必须宽容：非数值
// 或区间外的值落空串，绝不让整份目录解析失败。
func TestParseOpenAIModelsToleratesCreatedShapes(t *testing.T) {
	body := `{"data":[
		{"id":"zero","created":0},
		{"id":"negative","created":-1},
		{"id":"millis","created":1715644800000},
		{"id":"below-range","created":946684799},
		{"id":"lower-bound","created":946684800},
		{"id":"upper-bound","created":4102444800},
		{"id":"string-seconds","created":"1715644800"},
		{"id":"string-garbage","created":"recent"},
		{"id":"float-seconds","created":1715644800.5},
		{"id":"object","created":{"unix":1715644800}}
	],"models":[
		{"name":"manifest-only","created":1715644800},
		{"id":"preferred-id","name":"ignored-name","created":"1715644800"}
	]}`
	infos, err := parseOpenAIModels([]byte(body))
	if err != nil {
		t.Fatalf("parse must tolerate odd created values: %v", err)
	}
	want := map[string]string{
		"zero":           "",
		"negative":       "",
		"millis":         "",
		"below-range":    "",
		"lower-bound":    "2000-01-01",
		"upper-bound":    "",
		"string-seconds": "2024-05-14",
		"string-garbage": "",
		"float-seconds":  "2024-05-14",
		"object":         "",
		"manifest-only":  "2024-05-14",
		"preferred-id":   "2024-05-14",
	}
	if len(infos) != len(want) {
		t.Fatalf("len = %d, want %d (%+v)", len(infos), len(want), infos)
	}
	for _, info := range infos {
		expected, ok := want[info.ID]
		if !ok {
			t.Fatalf("unexpected model %q", info.ID)
		}
		if info.ReleasedAt != expected {
			t.Fatalf("%s: released_at = %q, want %q", info.ID, info.ReleasedAt, expected)
		}
	}
}

func TestParseAnthropicModelMetadata(t *testing.T) {
	body := `{"data":[
		{"id":"claude-sonnet-4-5","display_name":"Claude Sonnet 4.5","created_at":"2025-09-29T00:00:00Z"},
		{"id":"claude-3-haiku-20240307"}
	],"has_more":false}`
	page, err := parseAnthropicModels([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if page.Data[0].DisplayName != "Claude Sonnet 4.5" {
		t.Fatalf("display_name = %q", page.Data[0].DisplayName)
	}
	infos := anthropicModelInfos(page)
	if infos[0].ReleasedAt != "2025-09-29" || infos[0].Source != "anthropic_models" {
		t.Fatalf("info = %+v", infos[0])
	}
	if infos[1].ReleasedAt != "" || infos[1].DisplayName != "" {
		t.Fatalf("missing fields must stay empty: %+v", infos[1])
	}
	if len(infos[0].Endpoints) != 1 || infos[0].Endpoints[0] != "messages" {
		t.Fatalf("endpoints = %v", infos[0].Endpoints)
	}
}

func TestParseGeminiModelMetadata(t *testing.T) {
	body := `{"models":[
		{"name":"models/gemini-2.5-pro","displayName":"Gemini 2.5 Pro","description":"Think deeply",
		 "inputTokenLimit":1048576,"outputTokenLimit":65536,
		 "supportedGenerationMethods":["generateContent","countTokens"]},
		{"name":"models/text-embedding-004","inputTokenLimit":2048,
		 "supportedGenerationMethods":["embedContent"]}
	]}`
	infos, err := parseGeminiModels([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if infos[0].ID != "gemini-2.5-pro" || infos[0].DisplayName != "Gemini 2.5 Pro" {
		t.Fatalf("info = %+v", infos[0])
	}
	if infos[0].ContextLength == nil || *infos[0].ContextLength != 1048576 ||
		infos[0].MaxOutput == nil || *infos[0].MaxOutput != 65536 {
		t.Fatalf("limits = %+v", infos[0])
	}
	if len(infos[0].Endpoints) != 1 || infos[0].Endpoints[0] != "generateContent" {
		t.Fatalf("endpoints = %v", infos[0].Endpoints)
	}
	// 只支持 embedContent 的模型拿不到公开端点枚举，留空由管理员填。
	if len(infos[1].Endpoints) != 0 {
		t.Fatalf("endpoints = %v", infos[1].Endpoints)
	}
	if infos[1].ContextLength == nil || *infos[1].ContextLength != 2048 {
		t.Fatalf("context = %+v", infos[1])
	}
}

// 流式生成与 generateContent 共用同一个公开端点：公开枚举里没有
// streamGenerateContent，只有支持流式的模型必须仍然推导出 generateContent。
func TestGeminiEndpointsMapsStreamMethod(t *testing.T) {
	endpoints := geminiEndpoints([]string{"streamGenerateContent"})
	if len(endpoints) != 1 || endpoints[0] != "generateContent" {
		t.Fatalf("endpoints = %v", endpoints)
	}
}

// 供应商建议只按协议族推导；OpenAI 兼容的其他上游同样建议 OpenAI，
// 其余协议族留空，绝不凭名字猜供应商。
func TestSuggestVendor(t *testing.T) {
	cases := map[contract.ChannelType]string{
		contract.ChannelTypeOpenAI:    "OpenAI",
		contract.ChannelTypeAnthropic: "Anthropic",
		contract.ChannelTypeGemini:    "Gemini",
		contract.ChannelTypeDeepSeek:  "",
	}
	for in, want := range cases {
		if got := SuggestVendor(in); got != want {
			t.Fatalf("SuggestVendor(%s) = %q, want %q", in, got, want)
		}
	}
}

func TestModelListerRejectsUnsupportedTypes(t *testing.T) {
	lister := NewModelLister()
	_, err := lister.List(context.Background(), ModelListRequest{
		ChannelType: contract.ChannelTypeTaskPlugin,
		BaseURL:     "https://example.com",
		APIKey:      "key",
	})
	if !errors.Is(err, ErrModelListUnsupported) {
		t.Fatalf("err = %v", err)
	}
}

func TestModelListerRequiresKeyAndAzureVersion(t *testing.T) {
	lister := NewModelLister()
	_, err := lister.List(context.Background(), ModelListRequest{
		ChannelType: contract.ChannelTypeOpenAI,
		BaseURL:     "https://api.openai.com",
	})
	if !errors.Is(err, ErrModelListConfig) {
		t.Fatalf("missing key err = %v", err)
	}

	_, err = lister.List(context.Background(), ModelListRequest{
		ChannelType: contract.ChannelTypeAzure,
		BaseURL:     "https://example.openai.azure.com",
		APIKey:      "key",
	})
	if !errors.Is(err, ErrModelListConfig) {
		t.Fatalf("azure err = %v", err)
	}
}
