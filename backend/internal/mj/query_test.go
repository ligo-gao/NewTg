package mj

// W8/MJ-b 查询客户端与 DTO 投影的单测。
//
// 覆盖：fetch 的固定路径/凭据/单次请求、结构化解析、非 2xx/坏 JSON/缺
// result 的错误折叠、image-seed 的原样透传与超限、DataDelta 的增量语义、
// TaskDTO 的 raw_state 保真与内部字段不泄露。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"newtg/gateway/internal/tasks"
)

func TestQueryClientFetchesUpstreamTask(t *testing.T) {
	var calls int32
	var method, path, secret string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		method, path, secret = r.Method, r.URL.Path, r.Header.Get(upstreamSecretHeader)
		_, _ = w.Write([]byte(`{"code":1,"description":"ok","result":{` +
			`"status":"IN_PROGRESS","progress":"45%","imageUrl":"https://cdn.example/x.png",` +
			`"videoUrl":"https://cdn.example/v.mp4","videoUrls":["https://cdn.example/v1.mp4",7],` +
			`"buttons":[{"customId":"MJ::JOB::upsample::1::u"}],` +
			`"properties":{"numberOfQueues":2},"failReason":"","promptEn":"a cat","action":"IMAGINE"}}`))
	}))
	defer server.Close()

	client := NewSubmitClient(upstreamClient(server))
	result, mjErr := client.Query(context.Background(), SubmitConfig{
		BaseURL: server.URL + "/", APIKey: "channel-key",
	}, "mj_1")
	if mjErr != nil {
		t.Fatalf("Query: %v", mjErr)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("upstream calls = %d, want 1", got)
	}
	if method != http.MethodGet || path != "/mj/task/mj_1/fetch" {
		t.Fatalf("upstream request = %s %s, want GET /mj/task/mj_1/fetch", method, path)
	}
	if secret != "channel-key" {
		t.Fatalf("mj-api-secret = %q, want channel credential", secret)
	}
	if result.Status != "IN_PROGRESS" || result.Progress != "45%" {
		t.Fatalf("status/progress = %q/%q", result.Status, result.Progress)
	}
	if result.ImageURL != "https://cdn.example/x.png" || result.VideoURL != "https://cdn.example/v.mp4" {
		t.Fatalf("product urls = %q/%q", result.ImageURL, result.VideoURL)
	}
	if len(result.VideoURLs) != 2 || result.VideoURLs[0] != "https://cdn.example/v1.mp4" || result.VideoURLs[1] != "7" {
		t.Fatalf("videoUrls = %v", result.VideoURLs)
	}
	if len(result.Buttons) != 1 || len(result.Properties) != 1 {
		t.Fatalf("buttons/properties = %v/%v", result.Buttons, result.Properties)
	}
	if result.PromptEn != "a cat" || result.Action != "IMAGINE" {
		t.Fatalf("promptEn/action = %q/%q", result.PromptEn, result.Action)
	}
}

func TestQueryClientFoldsErrorsWithoutLeakingBody(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		wantErr string
	}{
		{"http 500", http.StatusInternalServerError, `{"code":1,"result":{"status":"SUCCESS"}}`, DescriptionUpstreamError},
		{"logical error code", http.StatusOK, `{"code":4,"description":"task not found at internal-host"}`, DescriptionUpstreamError},
		{"bad json", http.StatusOK, `<html>proxy error</html>`, DescriptionUpstreamBadResponse},
		{"missing result", http.StatusOK, `{"code":1}`, DescriptionUpstreamBadResponse},
		{"result not object", http.StatusOK, `{"code":1,"result":"x"}`, DescriptionUpstreamBadResponse},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			_, mjErr := NewSubmitClient(upstreamClient(server)).Query(context.Background(),
				SubmitConfig{BaseURL: server.URL, APIKey: "k"}, "mj_1")
			if mjErr == nil {
				t.Fatalf("expected error")
			}
			if mjErr.Description != tc.wantErr {
				t.Fatalf("description = %q, want %q", mjErr.Description, tc.wantErr)
			}
			// 上游正文（含内部地址）不得进入错误描述。
			if strings.Contains(mjErr.Description, "internal-host") || strings.Contains(mjErr.Description, "proxy error") {
				t.Fatalf("error leaks upstream body: %q", mjErr.Description)
			}
		})
	}
}

func TestQueryClientRejectsInvalidIDWithoutUpstreamCall(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		atomic.AddInt32(&calls, 1)
	}))
	defer server.Close()
	_, mjErr := NewSubmitClient(upstreamClient(server)).Query(context.Background(),
		SubmitConfig{BaseURL: server.URL, APIKey: "k"}, "../etc/passwd")
	if mjErr == nil || mjErr.Description != DescriptionInvalidID {
		t.Fatalf("invalid id error = %+v", mjErr)
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatalf("invalid id must not reach upstream")
	}
}

func TestQueryClientDoesNotFollowRedirects(t *testing.T) {
	var redirected int32
	second := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		atomic.AddInt32(&redirected, 1)
	}))
	defer second.Close()
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, &http.Request{}, second.URL+"/steal", http.StatusFound)
	}))
	defer first.Close()
	_, mjErr := NewSubmitClient(upstreamClient(first)).Query(context.Background(),
		SubmitConfig{BaseURL: first.URL, APIKey: "k"}, "mj_1")
	if mjErr == nil {
		t.Fatalf("redirect must be a failure (禁重定向)")
	}
	if atomic.LoadInt32(&redirected) != 0 {
		t.Fatalf("redirect target was contacted")
	}
}

func TestImageSeedPassesThroughUpstreamBody(t *testing.T) {
	var method, path, secret string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path, secret = r.Method, r.URL.Path, r.Header.Get(upstreamSecretHeader)
		_, _ = w.Write([]byte(`{"code":1,"description":"","result":["seed-value"]}`))
	}))
	defer server.Close()
	body, mjErr := NewSubmitClient(upstreamClient(server)).ImageSeed(context.Background(),
		SubmitConfig{BaseURL: server.URL, APIKey: "channel-key"}, "mj_1")
	if mjErr != nil {
		t.Fatalf("ImageSeed: %v", mjErr)
	}
	if method != http.MethodPost || path != "/mj/task/mj_1/image-seed" || secret != "channel-key" {
		t.Fatalf("upstream request = %s %s secret=%q", method, path, secret)
	}
	if string(body) != `{"code":1,"description":"","result":["seed-value"]}` {
		t.Fatalf("body must pass through unchanged: %s", body)
	}
}

func TestImageSeedRejectsNonJSONAndHTTPErrors(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"html", http.StatusOK, `<html>bad</html>`},
		{"http 502", http.StatusBadGateway, `{"code":1}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			if _, mjErr := NewSubmitClient(upstreamClient(server)).ImageSeed(context.Background(),
				SubmitConfig{BaseURL: server.URL, APIKey: "k"}, "mj_1"); mjErr == nil {
				t.Fatalf("expected error")
			}
		})
	}
}

func TestQueryResultDataDeltaSkipsEmptyValues(t *testing.T) {
	delta := (&QueryResult{
		ImageURL: "https://cdn/x.png",
		Status:   "IN_PROGRESS", // 状态不是 data 字段
	}).DataDelta()
	if delta[DataImageURL] != "https://cdn/x.png" {
		t.Fatalf("image url missing: %v", delta)
	}
	for _, key := range []string{DataVideoURL, DataVideoURLs, DataButtons, DataProperties, DataPromptEn, DataDescription} {
		if _, ok := delta[key]; ok {
			t.Fatalf("empty value %q must not be written (would overwrite previous snapshot)", key)
		}
	}
}

func TestTaskDTOProjectsLineProtocolAndHidesInternals(t *testing.T) {
	submit := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	task := &tasks.Task{
		PublicTaskID:    "task_internal",
		UpstreamTaskID:  "mj_1",
		Family:          tasks.FamilyMJ,
		Action:          "IMAGINE",
		State:           tasks.StateFailure,
		RawState:        "CANCEL",
		ProgressPercent: 37,
		FailReason:      "cancelled by user",
		SubmitTime:      &submit,
		Properties:      map[string]any{PropertyPrompt: "a cat"},
		PrivateData:     map[string]any{PrivateRequestID: "req_secret"},
		Data: map[string]any{
			DataImageURL:    "https://cdn/x.png",
			DataVideoURLs:   []any{"https://cdn/v1.mp4"},
			DataButtons:     []any{map[string]any{"customId": "MJ::x"}},
			DataProperties:  map[string]any{"numberOfQueues": float64(2)},
			DataDescription: "upstream description",
		},
	}
	dto := TaskDTO(task)
	if dto["status"] != "CANCEL" {
		t.Fatalf("status must preserve raw_state verbatim: %v", dto["status"])
	}
	if dto["progress"] != "37%" {
		t.Fatalf("progress projection = %v", dto["progress"])
	}
	if dto["id"] != "mj_1" || dto["prompt"] != "a cat" || dto["failReason"] != "cancelled by user" {
		t.Fatalf("basic projection = %v", dto)
	}
	if dto["imageUrl"] != "https://cdn/x.png" {
		t.Fatalf("imageUrl = %v", dto["imageUrl"])
	}
	if urls, ok := dto["videoUrls"].([]string); !ok || len(urls) != 1 {
		t.Fatalf("videoUrls = %#v, want []string", dto["videoUrls"])
	}
	if dto["submitTime"] != submit.UnixMilli() {
		t.Fatalf("submitTime = %v, want %d", dto["submitTime"], submit.UnixMilli())
	}
	// 内部列绝不出现（public_task_id / private_data / family 等）。
	for _, key := range []string{"task_id", "public_task_id", "private_data", "family", "plugin_key", "user_id"} {
		if _, ok := dto[key]; ok {
			t.Fatalf("DTO leaks internal field %q: %v", key, dto)
		}
	}
	if strings.Contains(strings.Join(mapKeys(dto), ","), "req_secret") {
		t.Fatalf("DTO leaked request id")
	}
}

func mapKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
