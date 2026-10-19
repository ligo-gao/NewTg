package plugin

import (
	"context"
	"fmt"
	"testing"
	"time"

	"newtg/gateway/plugins"
)

// TestLoadTenPluginsUnderOneSecond 覆盖 t3-brief 要求 6。
//
// 用 sample 源派生 10 个 key 的插件模拟内置插件规模（真实 10 平台源由后续
// 任务补齐）。预算 1s 是加载期门禁：30s 轮询每次 Reload 复用 factory 编译
// 缓存，因此启动成本只付一次。
func TestLoadTenPluginsUnderOneSecond(t *testing.T) {
	sources := make(map[string]string, 10)
	for idx := 0; idx < 10; idx++ {
		key := fmt.Sprintf("sample%d", idx)
		sources[key] = fixture{Key: key}.Source()
	}
	start := time.Now()
	m := MustNewManager(sources, nil, Options{MasterEnabled: true})
	if err := m.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > time.Second {
		t.Fatalf("加载 10 个插件耗时 %v，超过 1s 上限", elapsed)
	}
	registered := 0
	for _, key := range m.Snapshot().Keys() {
		if m.Snapshot().Lookup(key).Status == StatusRegistered {
			registered++
		}
	}
	if registered != 10 {
		t.Fatalf("注册数=%d want 10", registered)
	}

	// 第二次 Reload 命中 factory 缓存 + 内容未变，应显著更快且不递增代际。
	generation := m.Snapshot().Generation()
	if err := m.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if m.Snapshot().Generation() != generation {
		t.Fatalf("未变化的重载不应递增 generation")
	}
}

// TestEmbeddedSampleRegisteredAndDryRun 覆盖要求 2 的端到端链路：
// 编译 → T/D 校验 → 注册 → registered → dryrun。
func TestEmbeddedSampleRegisteredAndDryRun(t *testing.T) {
	sources, err := plugins.TaskPluginSources()
	if err != nil {
		t.Fatalf("读取内置插件: %v", err)
	}
	m := MustNewManager(sources, nil, Options{MasterEnabled: true, Node: "node-a"})
	if err := m.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	status := m.Status()
	if status.Node != "node-a" || status.Generation == 0 {
		t.Fatalf("运行时状态不符: %+v", status)
	}
	var sample *PluginRuntimeStatus
	for idx := range status.Plugins {
		if status.Plugins[idx].Key == "sample" {
			sample = &status.Plugins[idx]
		}
	}
	if sample == nil || sample.Status != StatusRegistered || sample.Version != "1.0.0" {
		t.Fatalf("sample 未注册: %+v", status.Plugins)
	}

	result, err := m.DryRun(context.Background(), DryRunRequest{
		Key:      "sample",
		Endpoint: "submit",
		Payload:  map[string]any{"model": "sample-model", "baseUrl": "https://example.invalid"},
	})
	if err != nil {
		t.Fatalf("dryrun: %v", err)
	}
	if result.Result.(map[string]any)["method"] != "POST" {
		t.Fatalf("dryrun 结果不符: %+v", result.Result)
	}
}
