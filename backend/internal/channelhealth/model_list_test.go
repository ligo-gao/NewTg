package channelhealth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"newtg/gateway/internal/contract"
)

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
