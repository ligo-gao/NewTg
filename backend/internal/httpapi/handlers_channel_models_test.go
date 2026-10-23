package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

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

func (f *fakeChannelModelLister) List(_ context.Context, req channelhealth.ModelListRequest) ([]string, error) {
	f.calls++
	f.got = req
	return f.models, f.err
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
