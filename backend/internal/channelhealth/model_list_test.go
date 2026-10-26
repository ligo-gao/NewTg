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
		if len(info.Endpoints) != 1 || info.Endpoints[0] != "chat.completions" {
			t.Fatalf("endpoints = %v", info.Endpoints)
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
