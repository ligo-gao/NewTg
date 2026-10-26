package httpapi

// 「渠道探测补全模型档案」向导的请求体契约。
//
// 前端 buildCreateBody / buildUpdateBody（Task 4）按 model_meta 契约拼请求体；
// 本测试不复用 store 结构体，而是直接提交与前端等价的原始 JSON 对象，钉住
// 「向导产出的形状必须被后端接受」。两条负例（knowledge_cutoff 的月份形态、
// endpoints 用公开枚举名当键）正是前后端最容易漂的地方——一旦前端写错，
// 这里先失败，而不是等管理员在界面上收到 422。

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strconv"
	"testing"
	"time"
)

// wizardCreateBody 与 frontend buildCreateBody 对一行完整事实的输出一一对应。
func wizardCreateBody() map[string]any {
	return map[string]any{
		"model_name":   "gemini-2.5-pro",
		"display_name": "Gemini 2.5 Pro",
		"description":  "Frontier multimodal model",
		"tags":         []string{"vision", "reasoning"},
		"endpoints": map[string]any{
			"/v1/chat/completions":                   []string{"POST"},
			"/v1beta/models/{model}:generateContent": []string{"POST"},
		},
		"capabilities": []string{"text", "vision", "function_calling"},
		"modalities":   []string{"text", "image"},
		"parameters": []map[string]any{
			{
				"name": "temperature", "type": "number", "required": false,
				"default": "0.7", "description": "Sampling temperature",
			},
			{"name": "stream", "type": "boolean", "required": false},
		},
		"context_length":   1048576,
		"max_output":       65536,
		"knowledge_cutoff": "2025-06",
		"released_at":      "2025-06-17",
		"vendor_id":        nil,
	}
}

func mustUnmarshalJSON[T any](t *testing.T, raw json.RawMessage) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("回读 JSON 非法: %v（raw=%s）", err, raw)
	}
	return out
}

func TestWizardCreateBodyIsAccepted(t *testing.T) {
	env := newCatalogTestEnv(t)
	rr := env.request(catalogFullAdmin(), http.MethodPost, "/api/models", wizardCreateBody(), nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create = %d（body=%q）", rr.Code, rr.Body.String())
	}
	created := decodeModelView(t, rr)

	// 再 GET 一次，断言的是「真正落库并读回」的字段，而不是创建响应的临时值。
	rr = env.request(catalogFullAdmin(), http.MethodGet, "/api/models/"+strconv.FormatInt(created.ID, 10), nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("get = %d（body=%q）", rr.Code, rr.Body.String())
	}
	view := decodeModelView(t, rr)

	if view.ModelName != "gemini-2.5-pro" {
		t.Fatalf("model_name = %q", view.ModelName)
	}
	if view.DisplayName == nil || *view.DisplayName != "Gemini 2.5 Pro" {
		t.Fatalf("display_name = %v", view.DisplayName)
	}
	if view.Description == nil || *view.Description != "Frontier multimodal model" {
		t.Fatalf("description = %v", view.Description)
	}
	if tags := mustUnmarshalJSON[[]string](t, view.Tags); !reflect.DeepEqual(tags, []string{"vision", "reasoning"}) {
		t.Fatalf("tags = %v", tags)
	}
	wantEndpoints := map[string][]string{
		"/v1/chat/completions":                   {"POST"},
		"/v1beta/models/{model}:generateContent": {"POST"},
	}
	if endpoints := mustUnmarshalJSON[map[string][]string](t, view.Endpoints); !reflect.DeepEqual(endpoints, wantEndpoints) {
		t.Fatalf("endpoints = %v", endpoints)
	}
	if capabilities := mustUnmarshalJSON[[]string](t, view.Capabilities); !reflect.DeepEqual(
		capabilities, []string{"text", "vision", "function_calling"}) {
		t.Fatalf("capabilities = %v", capabilities)
	}
	if modalities := mustUnmarshalJSON[[]string](t, view.Modalities); !reflect.DeepEqual(
		modalities, []string{"text", "image"}) {
		t.Fatalf("modalities = %v", modalities)
	}
	parameters := mustUnmarshalJSON[[]map[string]any](t, view.Parameters)
	if len(parameters) != 2 {
		t.Fatalf("parameters = %v", parameters)
	}
	if parameters[0]["name"] != "temperature" || parameters[0]["type"] != "number" ||
		parameters[0]["required"] != false || parameters[0]["default"] != "0.7" ||
		parameters[0]["description"] != "Sampling temperature" {
		t.Fatalf("parameters[0] = %v", parameters[0])
	}
	if parameters[1]["name"] != "stream" || parameters[1]["required"] != false {
		t.Fatalf("parameters[1] = %v", parameters[1])
	}
	if view.ContextLength == nil || *view.ContextLength != 1048576 {
		t.Fatalf("context_length = %v", view.ContextLength)
	}
	if view.MaxOutput == nil || *view.MaxOutput != 65536 {
		t.Fatalf("max_output = %v", view.MaxOutput)
	}
	if view.KnowledgeCutoff == nil || *view.KnowledgeCutoff != "2025-06" {
		t.Fatalf("knowledge_cutoff = %v", view.KnowledgeCutoff)
	}
	if view.ReleasedAt == nil || *view.ReleasedAt != "2025-06-17" {
		t.Fatalf("released_at = %v", view.ReleasedAt)
	}
	if view.VendorID != nil {
		t.Fatalf("vendor_id = %v, want null", view.VendorID)
	}
}

func TestWizardUpdateBodyIsAcceptedAndEnforcesOptimisticLock(t *testing.T) {
	env := newCatalogTestEnv(t)
	created := createTestModel(t, env, "gpt-4o")

	body := map[string]any{
		"updated_at":       created.UpdatedAt.Format(time.RFC3339Nano),
		"display_name":     "GPT-4o",
		"description":      "desc",
		"icon":             nil,
		"vendor_id":        nil,
		"tags":             []string{"vision"},
		"endpoints":        map[string]any{"/v1/chat/completions": []string{"POST"}},
		"capabilities":     []string{"text", "vision"},
		"modalities":       []string{"text", "image"},
		"parameters":       []map[string]any{{"name": "stream", "type": "boolean", "required": false, "default": "true"}},
		"context_length":   128000,
		"max_output":       16384,
		"knowledge_cutoff": "2025-06",
		"released_at":      "2025-06-17",
		"status":           1,
		"sync_official":    true,
		"name_rule":        0,
	}

	path := "/api/models/" + strconv.FormatInt(created.ID, 10)
	rr := env.request(catalogFullAdmin(), http.MethodPut, path, body, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("update = %d（body=%q）", rr.Code, rr.Body.String())
	}
	view := decodeModelView(t, rr)
	if view.DisplayName == nil || *view.DisplayName != "GPT-4o" ||
		view.ContextLength == nil || *view.ContextLength != 128000 ||
		view.KnowledgeCutoff == nil || *view.KnowledgeCutoff != "2025-06" {
		t.Fatalf("updated view = %+v", view)
	}
	if parameters := mustUnmarshalJSON[[]map[string]any](t, view.Parameters); len(parameters) != 1 ||
		parameters[0]["default"] != "true" || parameters[0]["required"] != false {
		t.Fatalf("parameters = %v", parameters)
	}

	// 同一份体再发一次：updated_at 已被第一次更新推进，必须 409 而不是覆盖。
	rr = env.request(catalogFullAdmin(), http.MethodPut, path, body, nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("stale update = %d，期望 409（body=%q）", rr.Code, rr.Body.String())
	}
}

func TestWizardBodyValidationRejectsDriftingShapes(t *testing.T) {
	cases := []struct {
		name  string
		body  map[string]any
		field string
	}{
		{
			name: "knowledge_cutoff must be YYYY-MM, not a full date",
			body: map[string]any{
				"model_name":       "bad-cutoff",
				"knowledge_cutoff": "2025-06-01",
			},
			field: "knowledge_cutoff",
		},
		{
			name: "endpoints keys must be paths, not public enum names",
			body: map[string]any{
				"model_name": "bad-endpoints",
				"endpoints":  map[string]any{"chat.completions": []string{"POST"}},
			},
			field: "endpoints",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			env := newCatalogTestEnv(t)
			rr := env.request(catalogFullAdmin(), http.MethodPost, "/api/models", testCase.body, nil)
			if rr.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d，期望 422（body=%q）", rr.Code, rr.Body.String())
			}
			problem := decodeCatalogProblem(t, rr)
			rawFields, ok := problem["fields"].([]any)
			if !ok {
				t.Fatalf("problem.fields 缺失: %v", problem)
			}
			for _, raw := range rawFields {
				if raw == testCase.field {
					return
				}
			}
			t.Fatalf("problem.fields = %v，期望包含 %q", rawFields, testCase.field)
		})
	}
}
