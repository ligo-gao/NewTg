package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/channelhealth"
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/store"
)

type fakeChannelModelLister struct {
	models []string
	err    error
	calls  int
	got    channelhealth.ModelListRequest
}

func (f *fakeChannelModelLister) ListDetailed(_ context.Context, req channelhealth.ModelListRequest) ([]channelhealth.ModelInfo, error) {
	f.calls++
	f.got = req
	if f.err != nil {
		return nil, f.err
	}
	out := make([]channelhealth.ModelInfo, 0, len(f.models))
	for _, id := range f.models {
		out = append(out, channelhealth.ModelInfo{ID: id, Source: "openai_models"})
	}
	return out, nil
}

func TestFetchChannelModelsUsesFormValues(t *testing.T) {
	lister := &fakeChannelModelLister{models: []string{"gpt-4o", "o3"}}
	env := newChannelTestEnv(t, ChannelHandlerOptions{ModelLister: lister})

	rr := serveWithPrincipal(t, env.mux,
		jsonReq(t, http.MethodPost, "/api/channels/fetch-models", map[string]any{
			"type":     "openai",
			"base_url": "https://upstream.example.com",
			"credentials": map[string]any{
				"api_key": "sk-form",
			},
		}),
		channelAdminPrincipal(),
	)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Models []string `json:"models"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Models) != 2 || body.Models[1] != "o3" {
		t.Fatalf("models = %v", body.Models)
	}
	if lister.got.ChannelType != contract.ChannelTypeOpenAI ||
		lister.got.BaseURL != "https://upstream.example.com" ||
		lister.got.APIKey != "sk-form" {
		t.Fatalf("lister request = %+v", lister.got)
	}
}

func TestFetchChannelModelsEditUsesStoredCredentials(t *testing.T) {
	lister := &fakeChannelModelLister{models: []string{"claude-sonnet-4-5"}}
	env := newChannelTestEnv(t, ChannelHandlerOptions{ModelLister: lister})
	baseURL := "https://api.anthropic.com"
	ch := seedChannel(t, env, func(c *store.Channel) {
		c.Type = string(contract.ChannelTypeAnthropic)
		c.BaseURL = &baseURL
		c.Credentials = mustSealCredentials(t, env.sealer, ChannelCredentials{APIKey: "sk-stored"})
	})

	rr := serveWithPrincipal(t, env.mux,
		jsonReq(t, http.MethodPost, "/api/channels/fetch-models", map[string]any{
			"channel_id": ch.ID,
		}),
		channelAdminPrincipal(),
	)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	if lister.got.ChannelType != contract.ChannelTypeAnthropic ||
		lister.got.BaseURL != baseURL ||
		lister.got.APIKey != "sk-stored" {
		t.Fatalf("lister request = %+v", lister.got)
	}
}

func TestFetchChannelModelsRequiresKey(t *testing.T) {
	env := newChannelTestEnv(t, ChannelHandlerOptions{ModelLister: &fakeChannelModelLister{}})
	rr := serveWithPrincipal(t, env.mux,
		jsonReq(t, http.MethodPost, "/api/channels/fetch-models", map[string]any{
			"type":     "openai",
			"base_url": "https://upstream.example.com",
		}),
		channelAdminPrincipal(),
	)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d", rr.Code)
	}
}

func TestFetchChannelModelsRequiresSensitivePermission(t *testing.T) {
	env := newChannelTestEnv(t, ChannelHandlerOptions{ModelLister: &fakeChannelModelLister{}})
	principal := &auth.Principal{UserID: testRegularID, Role: "user"}
	rr := serveWithPrincipal(t, env.mux,
		jsonReq(t, http.MethodPost, "/api/channels/fetch-models", map[string]any{
			"type":        "openai",
			"base_url":    "https://upstream.example.com",
			"credentials": map[string]any{"api_key": "sk-x"},
		}),
		principal,
	)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d", rr.Code)
	}
}

func TestFetchChannelModelsUnsupportedTypeIs400(t *testing.T) {
	lister := &fakeChannelModelLister{err: channelhealth.ErrModelListUnsupported}
	env := newChannelTestEnv(t, ChannelHandlerOptions{ModelLister: lister})
	rr := serveWithPrincipal(t, env.mux,
		jsonReq(t, http.MethodPost, "/api/channels/fetch-models", map[string]any{
			"type":        "task_plugin",
			"base_url":    "https://upstream.example.com",
			"credentials": map[string]any{"api_key": "sk-x"},
		}),
		channelAdminPrincipal(),
	)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestFetchChannelModelsUpstreamFailureIs502(t *testing.T) {
	lister := &fakeChannelModelLister{err: errors.New("upstream returned status 401")}
	env := newChannelTestEnv(t, ChannelHandlerOptions{ModelLister: lister})
	rr := serveWithPrincipal(t, env.mux,
		jsonReq(t, http.MethodPost, "/api/channels/fetch-models", map[string]any{
			"type":        "openai",
			"base_url":    "https://upstream.example.com",
			"credentials": map[string]any{"api_key": "sk-x"},
		}),
		channelAdminPrincipal(),
	)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

// 新响应在保留 models 的同时附加 entries；旧客户端只读 models，形状不变。
func TestFetchChannelModelsReturnsEntries(t *testing.T) {
	lister := &fakeChannelModelLister{models: []string{"gemini-2.5-pro"}}
	env := newChannelTestEnv(t, ChannelHandlerOptions{ModelLister: lister})
	rr := serveWithPrincipal(t, env.mux,
		jsonReq(t, http.MethodPost, "/api/channels/fetch-models", map[string]any{
			"type":        "gemini",
			"credentials": map[string]any{"api_key": "sk-form"},
		}),
		channelAdminPrincipal(),
	)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Models  []string            `json:"models"`
		Entries []channelModelEntry `json:"entries"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Models) != 1 || body.Models[0] != "gemini-2.5-pro" {
		t.Fatalf("models = %v", body.Models)
	}
	if len(body.Entries) != 1 || body.Entries[0].ID != "gemini-2.5-pro" ||
		body.Entries[0].VendorSuggestion != "Gemini" {
		t.Fatalf("entries = %+v", body.Entries)
	}
}

// entries 是「照抄上游事实」的投影：display_name / description 按 model_meta
// 的长度上限截断（按 rune，不切坏多字节字符），端点为空时必须是 [] 而不是 null。
func TestBuildChannelModelEntriesTruncatesAndKeepsSlices(t *testing.T) {
	contextLength := int64(1048576)
	maxOutput := int64(65536)
	infos := []channelhealth.ModelInfo{{
		ID:            "gemini-2.5-pro",
		DisplayName:   strings.Repeat("好", 70),
		Description:   strings.Repeat("d", 600),
		ContextLength: &contextLength,
		MaxOutput:     &maxOutput,
		ReleasedAt:    "2025-06-17",
		Endpoints:     []string{"generateContent"},
		Source:        "gemini_models",
	}}
	entries := buildChannelModelEntries(infos, contract.ChannelTypeGemini)
	if len(entries) != 1 {
		t.Fatalf("entries = %+v", entries)
	}
	entry := entries[0]
	if got := utf8.RuneCountInString(entry.DisplayName); got != 64 {
		t.Fatalf("display_name runes = %d, want 64", got)
	}
	if got := utf8.RuneCountInString(entry.Description); got != 512 {
		t.Fatalf("description runes = %d, want 512", got)
	}
	if entry.ContextLength == nil || *entry.ContextLength != contextLength ||
		entry.MaxOutput == nil || *entry.MaxOutput != maxOutput {
		t.Fatalf("limits = %+v", entry)
	}
	if entry.ReleasedAt != "2025-06-17" || entry.VendorSuggestion != "Gemini" ||
		entry.FactsSource != "gemini_models" {
		t.Fatalf("entry = %+v", entry)
	}

	empty := buildChannelModelEntries([]channelhealth.ModelInfo{{
		ID:     "text-embedding-004",
		Source: "gemini_models",
	}}, contract.ChannelTypeDeepSeek)
	if empty == nil || len(empty) != 1 {
		t.Fatalf("entries must be non-nil: %+v", empty)
	}
	if empty[0].Endpoints == nil {
		t.Fatalf("endpoints must be an empty slice, got nil")
	}
	if empty[0].VendorSuggestion != "" {
		t.Fatalf("vendor_suggestion = %q, want empty", empty[0].VendorSuggestion)
	}
	raw, err := json.Marshal(empty[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"endpoints":[]`) {
		t.Fatalf("endpoints must serialize as []: %s", raw)
	}
	if got := buildChannelModelEntries(nil, contract.ChannelTypeOpenAI); got == nil {
		t.Fatalf("entries must be non-nil for empty input")
	}
}
