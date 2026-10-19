package plugin

import (
	"context"
	"strings"
	"testing"
	"time"
)

// fakeLoader 是 VersionLoader 的内存实现。
type fakeLoader struct {
	versions []ActiveVersion
	byKey    map[string]ActiveVersion
	err      error
}

func newFakeLoader(versions ...ActiveVersion) *fakeLoader {
	loader := &fakeLoader{versions: versions, byKey: map[string]ActiveVersion{}}
	for _, version := range versions {
		loader.byKey[version.Key+"/"+version.Version] = version
	}
	return loader
}

func (f *fakeLoader) LoadActive(context.Context) ([]ActiveVersion, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.versions, nil
}

func (f *fakeLoader) LoadVersion(_ context.Context, key, version string) (*ActiveVersion, error) {
	if f.err != nil {
		return nil, f.err
	}
	found, ok := f.byKey[key+"/"+version]
	if !ok {
		return nil, nil
	}
	return &found, nil
}

func activeVersion(key, version, source string) ActiveVersion {
	return ActiveVersion{
		Key:        key,
		Version:    version,
		SourceText: source,
		SourceHash: sourceHash(source),
		Enabled:    true,
		Active:     true,
		UpdatedAt:  time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC),
	}
}

func newManager(t *testing.T, loader VersionLoader, opts Options) *Manager {
	t.Helper()
	return MustNewManager(map[string]string{"sample": fixture{}.Source()}, loader, opts)
}

func TestRegistryFactoryOnly(t *testing.T) {
	m := newManager(t, nil, Options{MasterEnabled: true})
	if err := m.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	entry := m.Snapshot().Lookup("sample")
	if entry.Status != StatusRegistered || entry.Layer != LayerFactory || entry.Version != "1.0.0" {
		t.Fatalf("factory 层应为 registered: %+v", entry)
	}
	if unknown := m.Snapshot().Lookup("nope"); unknown.Status != StatusNotRegistered {
		t.Fatalf("未知 key 应为 not_registered: %+v", unknown)
	}
}

func TestRegistryOverrideWinsOverFactory(t *testing.T) {
	override := fixture{Version: "2.0.0", EnDesc: "Uploaded task plugin"}.Source()
	m := newManager(t, newFakeLoader(activeVersion("sample", "2.0.0", override)), Options{MasterEnabled: true})
	if err := m.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	entry := m.Snapshot().Lookup("sample")
	if entry.Status != StatusRegistered || entry.Layer != LayerOverride || entry.Version != "2.0.0" {
		t.Fatalf("覆盖层应优先: %+v", entry)
	}
}

func TestRegistryBrokenOverrideFallsBackToFactory(t *testing.T) {
	broken := fixture{Version: "2.0.0", Append: "function ("}.Source()
	m := newManager(t, newFakeLoader(activeVersion("sample", "2.0.0", broken)), Options{MasterEnabled: true})
	if err := m.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	entry := m.Snapshot().Lookup("sample")
	if entry.Status != StatusRegistered || entry.Layer != LayerFactory || entry.Version != "1.0.0" {
		t.Fatalf("覆盖层失败应回退 factory: %+v", entry)
	}
	if !entry.Errors.hasRule("override_compile_failed") {
		t.Fatalf("回退必须留下诊断: %v", entry.Errors)
	}
}

func TestRegistryFactoryDisabledWithOverrideIsFallback(t *testing.T) {
	override := fixture{Version: "2.0.0"}.Source()
	m := newManager(t, newFakeLoader(activeVersion("sample", "2.0.0", override)), Options{
		MasterEnabled:       true,
		DisabledFactoryKeys: []string{"sample"},
	})
	if err := m.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	entry := m.Snapshot().Lookup("sample")
	if entry.Status != StatusDisabledFallback || entry.Layer != LayerOverride {
		t.Fatalf("内置禁用且有覆盖层应为 disabled_fallback: %+v", entry)
	}
	if _, ok := m.Snapshot().Plugin("sample"); !ok {
		t.Fatalf("disabled_fallback 的插件必须可执行")
	}
}

func TestRegistryFactoryDisabledWithoutOverrideIsDisabled(t *testing.T) {
	m := newManager(t, nil, Options{MasterEnabled: true, DisabledFactoryKeys: []string{"sample"}})
	if err := m.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	entry := m.Snapshot().Lookup("sample")
	if entry.Status != StatusDisabled || entry.Plugin != nil {
		t.Fatalf("内置禁用且无覆盖层应为 disabled: %+v", entry)
	}
}

func TestRegistryFactoryDisabledBrokenOverrideIsCompileFailed(t *testing.T) {
	broken := fixture{Version: "2.0.0", Append: "function ("}.Source()
	m := newManager(t, newFakeLoader(activeVersion("sample", "2.0.0", broken)), Options{
		MasterEnabled:       true,
		DisabledFactoryKeys: []string{"sample"},
	})
	if err := m.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	entry := m.Snapshot().Lookup("sample")
	if entry.Status != StatusCompileFailed || entry.Plugin != nil {
		t.Fatalf("内置禁用且覆盖层失败应为 compile_failed: %+v", entry)
	}
}

func TestRegistryMasterDisabled(t *testing.T) {
	m := newManager(t, nil, Options{MasterEnabled: false})
	if err := m.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	entry := m.Snapshot().Lookup("sample")
	if entry.Status != StatusDisabled {
		t.Fatalf("总开关关闭应为 disabled: %+v", entry)
	}
}

func TestRegistryRouteConflict(t *testing.T) {
	first := fixture{Key: "sample", Routes: routeJS("POST", "/v1/:id/submit", "submit", "buildSubmitRequest", "parseSubmitResponse")}.Source()
	second := fixture{Key: "sample2", Routes: routeJS("POST", "/v1/:task_id/submit", "submit", "buildSubmitRequest", "parseSubmitResponse")}.Source()
	m := MustNewManager(map[string]string{"sample": first, "sample2": second}, nil, Options{MasterEnabled: true})
	if err := m.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	// key 升序先到先得：sample 保留路由，sample2 整插件失败。
	if entry := m.Snapshot().Lookup("sample"); entry.Status != StatusRegistered {
		t.Fatalf("先注册者应保留: %+v", entry)
	}
	entry := m.Snapshot().Lookup("sample2")
	if entry.Status != StatusCompileFailed || !entry.Errors.hasRule("route_conflict") {
		t.Fatalf("路由冲突应使后到插件 compile_failed: %+v", entry)
	}
	if owners := m.Snapshot().RouteOwners(); owners["POST /v1/{}/submit"] != "sample" {
		t.Fatalf("路由索引不应残留失败插件: %v", owners)
	}
}

func TestRegistryChannelTypeConflict(t *testing.T) {
	first := fixture{Key: "sample", ChannelTypes: "[17]"}.Source()
	second := fixture{Key: "sample2", ChannelTypes: "[17]"}.Source()
	m := MustNewManager(map[string]string{"sample": first, "sample2": second}, nil, Options{MasterEnabled: true})
	if err := m.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	entry := m.Snapshot().Lookup("sample2")
	if entry.Status != StatusCompileFailed || !entry.Errors.hasRule("channel_type_conflict") {
		t.Fatalf("渠道类型跨插件复用应失败: %+v", entry)
	}
}

func TestRegistryStatusVocabulary(t *testing.T) {
	valid := map[PluginStatus]bool{
		StatusRegistered: true, StatusDisabled: true, StatusDisabledFallback: true,
		StatusNotRegistered: true, StatusCompileFailed: true,
	}
	seen := map[PluginStatus]bool{}
	override := fixture{Version: "2.0.0"}.Source()
	m := MustNewManager(map[string]string{
		"sample":  fixture{}.Source(),
		"sample2": fixture{Key: "sample2"}.Source(),
		"sample3": fixture{Key: "sample3"}.Source(),
		"sample4": fixture{Key: "sample4"}.Source(),
	}, newFakeLoader(
		activeVersion("sample", "2.0.0", override),
		activeVersion("sample3", "2.0.0", fixture{Key: "sample3", Version: "2.0.0", Append: "function ("}.Source()),
		activeVersion("sample4", "2.0.0", fixture{Key: "sample4", Version: "2.0.0"}.Source()),
	), Options{MasterEnabled: true, DisabledFactoryKeys: []string{"sample2", "sample3", "sample4"}})
	if err := m.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	status := m.Status()
	for _, plugin := range status.Plugins {
		if !valid[plugin.Status] {
			t.Fatalf("非法状态词 %q", plugin.Status)
		}
		seen[plugin.Status] = true
	}
	for _, want := range []PluginStatus{StatusRegistered, StatusDisabledFallback, StatusCompileFailed} {
		if !seen[want] {
			t.Fatalf("缺少状态 %q: %+v", want, status.Plugins)
		}
	}
	// not_registered 不来自快照条目，而是 Lookup 的合成值。
	if got := m.Snapshot().Lookup("ghost").Status; got != StatusNotRegistered || !valid[got] {
		t.Fatalf("Lookup(ghost)=%q", got)
	}
}

func TestRegistryHashMismatchIsFailClosed(t *testing.T) {
	version := activeVersion("sample", "2.0.0", fixture{Version: "2.0.0"}.Source())
	version.SourceHash = strings.Repeat("0", 64)
	m := newManager(t, newFakeLoader(version), Options{MasterEnabled: true})
	if err := m.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	entry := m.Snapshot().Lookup("sample")
	// 覆盖层 hash 不符 → 不执行；factory 可用 → 回退 factory。
	if entry.Layer != LayerFactory || entry.Status != StatusRegistered {
		t.Fatalf("hash 不符的覆盖层不得执行: %+v", entry)
	}
	if !entry.Errors.hasRule("source_hash_mismatch") && !entry.Errors.hasRule("override_compile_failed") {
		t.Fatalf("应留下 hash 不符诊断: %v", entry.Errors)
	}
}
