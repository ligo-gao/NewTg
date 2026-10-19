package plugin

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestDryRunSubmitUsesInjectedContext(t *testing.T) {
	m := newManager(t, nil, Options{MasterEnabled: true})
	if err := m.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	result, err := m.DryRun(context.Background(), DryRunRequest{
		Key:      "sample",
		Endpoint: "submit",
		Payload:  map[string]any{"model": "sample-model", "prompt": "hello", "baseUrl": "https://upstream.invalid"},
	})
	if err != nil {
		t.Fatalf("dryrun: %v", err)
	}
	body, ok := result.Result.(map[string]any)
	if !ok {
		t.Fatalf("结果应为对象: %T", result.Result)
	}
	if body["method"] != "POST" || !strings.HasPrefix(body["url"].(string), "https://upstream.invalid") {
		t.Fatalf("请求描述符不符: %+v", body)
	}
	if result.Version != "1.0.0" || result.Endpoint != "submit" {
		t.Fatalf("结果元数据不符: %+v", result)
	}
}

func TestDryRunQueryAndResult(t *testing.T) {
	m := newManager(t, nil, Options{MasterEnabled: true})
	if err := m.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	query, err := m.DryRun(context.Background(), DryRunRequest{
		Key:      "sample",
		Endpoint: "query",
		Payload:  map[string]any{"taskId": "abc", "baseUrl": "https://upstream.invalid"},
	})
	if err != nil {
		t.Fatalf("dryrun query: %v", err)
	}
	if !strings.HasSuffix(query.Result.(map[string]any)["url"].(string), "/abc") {
		t.Fatalf("query URL 不符: %+v", query.Result)
	}

	result, err := m.DryRun(context.Background(), DryRunRequest{
		Key:      "sample",
		Endpoint: "result",
		Payload: map[string]any{
			"ctx":  map[string]any{"model": "sample-model"},
			"body": `{"status":"running"}`,
		},
	})
	if err != nil {
		t.Fatalf("dryrun result: %v", err)
	}
	if result.Result.(map[string]any)["state"] != "SUCCESS" {
		t.Fatalf("parseTaskResult 返回值不符: %+v", result.Result)
	}
}

func TestDryRunUnknownKey(t *testing.T) {
	m := newManager(t, nil, Options{MasterEnabled: true})
	if err := m.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	_, err := m.DryRun(context.Background(), DryRunRequest{Key: "ghost"})
	issues, ok := err.(Issues)
	if !ok || !issues.hasRule("dryrun_unavailable") {
		t.Fatalf("未知 key 应拒绝: %v", err)
	}
}

func TestDryRunSpecificVersion(t *testing.T) {
	v2 := fixture{Version: "2.0.0", Exports: map[string]string{
		"buildSubmitRequest": `function (ctx) { return { url: "https://example.invalid/v2", method: "POST" }; }`,
	}}.Source()
	v2row := activeVersion("sample", "2.0.0", v2)
	// 2.0.0 尚未激活：既避免「同 key 两个 active」的歧义，
	// 也让 dryrun 的「按版本试运行」有独立意义。
	v2row.Active = false
	loader := newFakeLoader(activeVersion("sample", "1.0.0", fixture{}.Source()), v2row)
	m := newManager(t, loader, Options{MasterEnabled: true})
	if err := m.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	result, err := m.DryRun(context.Background(), DryRunRequest{
		Key: "sample", Version: "2.0.0", Endpoint: "submit", Payload: map[string]any{},
	})
	if err != nil {
		t.Fatalf("dryrun: %v", err)
	}
	if result.Version != "2.0.0" || result.Result.(map[string]any)["url"] != "https://example.invalid/v2" {
		t.Fatalf("指定版本的 dryrun 未生效: %+v", result)
	}
}

func TestDryRunAppliesValidation(t *testing.T) {
	bad := fixture{Version: "2.0.0", UsageSchema: `{
    seconds: { type: "number", unit: "second", description: { en: "Video generation price", zh: "视频生成单价" } }
  }`}.Source()
	badRow := activeVersion("sample", "2.0.0", bad)
	badRow.Active = false
	loader := newFakeLoader(activeVersion("sample", "1.0.0", fixture{}.Source()), badRow)
	m := newManager(t, loader, Options{MasterEnabled: true})
	if err := m.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	_, err := m.DryRun(context.Background(), DryRunRequest{Key: "sample", Version: "2.0.0"})
	issues, ok := err.(Issues)
	if !ok || !issues.hasRule("D5") {
		t.Fatalf("dryrun 必须走同一校验（D5）: %v", err)
	}
}

func TestDryRunTimeout(t *testing.T) {
	slow := fixture{Exports: map[string]string{
		"buildSubmitRequest": `function (ctx) { while (true) {} }`,
	}}.Source()
	m := MustNewManager(map[string]string{"sample": slow}, nil, Options{
		MasterEnabled: true,
		Timeout:       100 * time.Millisecond,
	})
	if err := m.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	start := time.Now()
	_, err := m.DryRun(context.Background(), DryRunRequest{Key: "sample", Endpoint: "submit", Payload: map[string]any{}})
	if err == nil {
		t.Fatalf("期望超时")
	}
	issues, ok := err.(Issues)
	if !ok || !issues.hasRule("timeout") {
		t.Fatalf("期望 timeout 规则: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("超时未及时中断: %v", elapsed)
	}
}
