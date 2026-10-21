package mj

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// TestSuccessEnvelopeShape 钉死成功信封 {code:1, description, result}。
func TestSuccessEnvelopeShape(t *testing.T) {
	envelope := NewSuccess("success", "mj-id-1")
	if envelope.Code != CodeSuccess {
		t.Fatalf("成功 code = %d，期望 1", envelope.Code)
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(fields) != 3 {
		t.Fatalf("成功信封字段数 = %d，期望 3: %s", len(fields), raw)
	}
	for _, key := range []string{"code", "description", "result"} {
		if _, ok := fields[key]; !ok {
			t.Fatalf("成功信封缺少 %q: %s", key, raw)
		}
	}
	if fields["result"] != "mj-id-1" {
		t.Fatalf("result = %v", fields["result"])
	}

	// 空数组结果也必须以数组出现（上传端点的 result:[]），不能字段缺失。
	upload := NewSuccess("success", []string{})
	raw, err = json.Marshal(upload)
	if err != nil {
		t.Fatalf("marshal upload: %v", err)
	}
	fields = nil
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("unmarshal upload: %v", err)
	}
	result, ok := fields["result"].([]any)
	if !ok || len(result) != 0 {
		t.Fatalf("上传 result 必须是空数组，got %s", raw)
	}
}

// TestErrorMarshalJSONWireOnly 钉死 *Error 直接序列化也只有线协议两字段。
//
// R-MJ-17 不允许 Status/内部错误链出现在任何响应里；Error 的导出字段
// （Code/Description/Status）若走默认序列化会输出 PascalCase + status，
// 因此必须有显式 MarshalJSON。删掉它本用例必须变红。
func TestErrorMarshalJSONWireOnly(t *testing.T) {
	raw, err := json.Marshal(BadRequest(DescriptionUnknownAction))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(fields) != 2 {
		t.Fatalf("*Error 序列化字段数 = %d，期望 2: %s", len(fields), raw)
	}
	if _, ok := fields["status"]; ok {
		t.Fatalf("*Error 泄露了 Status: %s", raw)
	}
	if _, ok := fields["Status"]; ok {
		t.Fatalf("*Error 使用了默认字段名 Status: %s", raw)
	}
	if _, ok := fields["Code"]; ok {
		t.Fatalf("*Error 使用了默认字段名 Code: %s", raw)
	}
	if fields["code"] != float64(CodeRequestError) || fields["description"] != DescriptionUnknownAction {
		t.Fatalf("线码/描述错误: %s", raw)
	}
}

// TestSuccessEnvelopeAlwaysHasResult 成功信封的 result 字段恒存在。
func TestSuccessEnvelopeAlwaysHasResult(t *testing.T) {
	raw, err := json.Marshal(NewSuccess("ok", nil))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(fields) != 3 {
		t.Fatalf("nil result 成功信封字段数 = %d，期望 3: %s", len(fields), raw)
	}
	result, ok := fields["result"].([]any)
	if !ok || len(result) != 0 {
		t.Fatalf("nil result 必须折叠为空数组: %s", raw)
	}
}

// TestFailureEnvelopeShape 钉死错误信封只有 {code, description}。
func TestFailureEnvelopeShape(t *testing.T) {
	envelope := NewFailure(BadRequest(DescriptionUnknownAction))
	if envelope.Code != CodeRequestError {
		t.Fatalf("参数错误 code = %d，期望 4", envelope.Code)
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(fields) != 2 {
		t.Fatalf("错误信封字段数 = %d，期望 2: %s", len(fields), raw)
	}
	if _, ok := fields["result"]; ok {
		t.Fatalf("错误信封不得含 result: %s", raw)
	}
	if _, ok := fields["status"]; ok {
		t.Fatalf("错误信封不得含内部 Status: %s", raw)
	}
	if fields["description"] != DescriptionUnknownAction {
		t.Fatalf("description = %v", fields["description"])
	}
}

// TestFailureDoesNotLeakInternalError 非领域错误一律折叠为 internal_error。
func TestFailureDoesNotLeakInternalError(t *testing.T) {
	secret := "postgres://user:password@host/db"
	envelope := NewFailure(errors.New(secret))
	if envelope.Code != CodeUnknownError || envelope.Description != DescriptionInternalError {
		t.Fatalf("非领域错误 = %+v，期望 code=5/internal_error", envelope)
	}
	raw, _ := json.Marshal(envelope)
	if strings.Contains(string(raw), secret) || strings.Contains(string(raw), "password") {
		t.Fatalf("错误信封泄露了内部错误: %s", raw)
	}
}

// TestErrorHelpers 钉死错误码与建议 HTTP 状态。
func TestErrorHelpers(t *testing.T) {
	if got := BadRequest(DescriptionInvalidRequest); got.Code != CodeRequestError || got.HTTPStatus() != 400 {
		t.Fatalf("BadRequest = %+v", got)
	}
	if got := Forbidden(DescriptionUnknownAction); got.Code != CodeRequestError || got.HTTPStatus() != 403 {
		t.Fatalf("Forbidden = %+v", got)
	}
	if got := ServiceUnavailable(DescriptionNoChannel); got.Code != CodeUnknownError || got.HTTPStatus() != 503 {
		t.Fatalf("ServiceUnavailable = %+v", got)
	}
	var nilErr *Error
	if nilErr.HTTPStatus() != 400 {
		t.Fatalf("nil error 必须安全返回 400")
	}
	if nilErr.Error() == "" {
		t.Fatalf("nil error 的字符串不应为空（日志可读）")
	}
}

// TestUpstreamCodeClassification 钉死上游返回码归类与客户端重编码。
func TestUpstreamCodeClassification(t *testing.T) {
	accepted := []int{CodeSuccess, CodeTaskExists, CodeQueued, CodeQueueFull}
	for _, code := range accepted {
		if !CodeAccepted(code) {
			t.Fatalf("code %d 应视为已受理", code)
		}
		if got := ClientCode(code); got != CodeSuccess {
			t.Fatalf("ClientCode(%d) = %d，期望 1", code, got)
		}
	}
	for _, code := range []int{CodeSensitiveWord, CodeNoAccount, CodeGroupSaturated, 999, 0} {
		if CodeAccepted(code) {
			t.Fatalf("code %d 不得视为已受理", code)
		}
		if got := ClientCode(code); got != code {
			t.Fatalf("ClientCode(%d) = %d，非受理码应原样透传", code, got)
		}
	}
}
