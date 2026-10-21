package mj

// W8/MJ-a2 上游提交客户端的线协议验收。
//
// 覆盖：固定路径/`mj-api-secret` 头/60s 单次尝试、字段指针语义（任务类补
// action/index/taskId、simple-change 不发 taskId）、code/result 的 number/string/
// 数组三态解析、响应上限与坏响应/重定向失败路径。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func imagineNormalized(prompt string) *NormalizedSubmit {
	return &NormalizedSubmit{
		Endpoint: EndpointImagine,
		Action:   ActionImagine,
		Model:    "mj_imagine",
		Prompt:   ptr(prompt),
	}
}

func TestBuildSubmitBodyPointerSemantics(t *testing.T) {
	t.Run("imagine omits gateway-only fields", func(t *testing.T) {
		body, err := buildSubmitBody(EndpointImagine, imagineNormalized("a cat"))
		if err != nil {
			t.Fatalf("buildSubmitBody: %v", err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(body, &decoded); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if decoded["prompt"] != "a cat" {
			t.Fatalf("prompt = %v", decoded["prompt"])
		}
		for _, forbidden := range []string{"accountFilter", "notifyHook", "action", "taskId", "index"} {
			if _, ok := decoded[forbidden]; ok {
				t.Fatalf("field %q must not be sent by default: %s", forbidden, body)
			}
		}
	})

	t.Run("change sends action and index", func(t *testing.T) {
		n := &NormalizedSubmit{Endpoint: EndpointChange, Action: ActionUpscale, Model: "mj_upscale",
			TaskID: "mj_1", Index: 2}
		body, err := buildSubmitBody(EndpointChange, n)
		if err != nil {
			t.Fatalf("buildSubmitBody: %v", err)
		}
		var decoded map[string]any
		_ = json.Unmarshal(body, &decoded)
		if decoded["action"] != "UPSCALE" || decoded["index"] != float64(2) || decoded["taskId"] != "mj_1" {
			t.Fatalf("change body = %s", body)
		}
	})

	t.Run("video forces VIDEO", func(t *testing.T) {
		n := &NormalizedSubmit{Endpoint: EndpointVideo, Action: ActionVideo, Model: "mj_video", TaskID: "mj_1"}
		body, _ := buildSubmitBody(EndpointVideo, n)
		var decoded map[string]any
		_ = json.Unmarshal(body, &decoded)
		if decoded["action"] != "VIDEO" || decoded["taskId"] != "mj_1" {
			t.Fatalf("video body = %s", body)
		}
	})

	t.Run("simple-change sends content without taskId", func(t *testing.T) {
		n := &NormalizedSubmit{Endpoint: EndpointSimpleChange, Action: ActionUpscale, Model: "mj_upscale",
			TaskID: "mj_1", Content: ptr("mj_1 u1")}
		body, _ := buildSubmitBody(EndpointSimpleChange, n)
		var decoded map[string]any
		_ = json.Unmarshal(body, &decoded)
		if decoded["content"] != "mj_1 u1" {
			t.Fatalf("content = %v", decoded["content"])
		}
		if _, ok := decoded["taskId"]; ok {
			t.Fatalf("simple-change must not send a separate taskId: %s", body)
		}
	})

	t.Run("forwards accountFilter only when normalized keeps it", func(t *testing.T) {
		n := imagineNormalized("x")
		n.AccountFilter = map[string]any{"instanceId": "abc"}
		body, _ := buildSubmitBody(EndpointImagine, n)
		var decoded map[string]any
		_ = json.Unmarshal(body, &decoded)
		if _, ok := decoded["accountFilter"]; !ok {
			t.Fatalf("forwarded accountFilter missing: %s", body)
		}
	})
}

func TestSubmitSendsPathHeaderAndParsesResponse(t *testing.T) {
	var gotPath, gotSecret, gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotSecret = r.Header.Get("mj-api-secret")
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":"1","description":"ok","result":"mj_up_1","properties":{"status":"SUCCESS","progress":"100%","numberOfQueues":2}}`))
	}))
	defer server.Close()

	client := NewSubmitClient(nil)
	resp, err := client.Submit(context.Background(), SubmitConfig{BaseURL: server.URL, APIKey: "channel-key"},
		EndpointImagine, imagineNormalized("a cat"))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if gotPath != "/mj/submit/imagine" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotSecret != "channel-key" {
		t.Fatalf("mj-api-secret = %q", gotSecret)
	}
	if !strings.Contains(gotBody, `"prompt":"a cat"`) {
		t.Fatalf("body = %s", gotBody)
	}
	if resp.Code != 1 || resp.Result != "mj_up_1" {
		t.Fatalf("response = %+v", resp)
	}
	if resp.Status != "SUCCESS" || resp.Progress != "100%" {
		t.Fatalf("properties status/progress not parsed: %+v", resp)
	}
}

func TestSubmitParsesArrayResultForUpload(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":1,"result":["https://cdn/1.png","https://cdn/2.png"]}`))
	}))
	defer server.Close()

	client := NewSubmitClient(nil)
	n := &NormalizedSubmit{Endpoint: EndpointUploadDiscord, Action: ActionUpload, Model: "mj_upload",
		Base64Array: []string{"aGk="}}
	resp, err := client.Submit(context.Background(), SubmitConfig{BaseURL: server.URL, APIKey: "k"},
		EndpointUploadDiscord, n)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if resp.Result != "https://cdn/1.png" || len(resp.Results) != 2 {
		t.Fatalf("array result = %+v", resp)
	}
}

func TestSubmitRejectsBadUpstreamResponses(t *testing.T) {
	t.Run("oversized response", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(strings.Repeat("x", MaxSubmitResponseBytes+1)))
		}))
		defer server.Close()
		client := NewSubmitClient(nil)
		_, err := client.Submit(context.Background(), SubmitConfig{BaseURL: server.URL}, EndpointImagine, imagineNormalized("x"))
		if err == nil || err.Description != DescriptionUpstreamResponseTooLarge {
			t.Fatalf("error = %+v, want %s", err, DescriptionUpstreamResponseTooLarge)
		}
	})

	t.Run("non-JSON upstream error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("<html>internal address 10.0.0.1</html>"))
		}))
		defer server.Close()
		client := NewSubmitClient(nil)
		_, err := client.Submit(context.Background(), SubmitConfig{BaseURL: server.URL}, EndpointImagine, imagineNormalized("x"))
		if err == nil || err.Description != DescriptionUpstreamError {
			t.Fatalf("error = %+v, want %s", err, DescriptionUpstreamError)
		}
	})

	t.Run("HTTP failure with accepted code", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"code":1,"result":"mj_up_1"}`))
		}))
		defer server.Close()
		client := NewSubmitClient(nil)
		_, err := client.Submit(context.Background(), SubmitConfig{BaseURL: server.URL}, EndpointImagine, imagineNormalized("x"))
		if err == nil || err.Description != DescriptionUpstreamError {
			t.Fatalf("error = %+v, want %s", err, DescriptionUpstreamError)
		}
	})

	t.Run("unsupported result element", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"code":1,"result":["ok",{"bad":true}]}`))
		}))
		defer server.Close()
		client := NewSubmitClient(nil)
		_, err := client.Submit(context.Background(), SubmitConfig{BaseURL: server.URL}, EndpointUploadDiscord,
			&NormalizedSubmit{Endpoint: EndpointUploadDiscord, Action: ActionUpload, Model: "mj_upload", Base64Array: []string{"aGk="}})
		if err == nil || err.Description != DescriptionUpstreamBadResponse {
			t.Fatalf("error = %+v, want %s", err, DescriptionUpstreamBadResponse)
		}
	})

	t.Run("missing code", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"description":"no code"}`))
		}))
		defer server.Close()
		client := NewSubmitClient(nil)
		_, err := client.Submit(context.Background(), SubmitConfig{BaseURL: server.URL}, EndpointImagine, imagineNormalized("x"))
		if err == nil || err.Description != DescriptionUpstreamBadResponse {
			t.Fatalf("error = %+v, want %s", err, DescriptionUpstreamBadResponse)
		}
	})
}

func TestSubmitDoesNotFollowRedirects(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatalf("redirect target must not be requested")
	}))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer server.Close()

	client := NewSubmitClient(nil)
	_, err := client.Submit(context.Background(), SubmitConfig{BaseURL: server.URL}, EndpointImagine, imagineNormalized("x"))
	if err == nil || err.Description != DescriptionUpstreamError {
		t.Fatalf("error = %+v, want %s", err, DescriptionUpstreamError)
	}
}
