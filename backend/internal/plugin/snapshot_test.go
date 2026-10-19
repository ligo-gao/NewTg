package plugin

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSnapshotGenerationOnlyAdvancesOnChange(t *testing.T) {
	loader := newFakeLoader(activeVersion("sample", "1.0.0", fixture{}.Source()))
	m := newManager(t, loader, Options{MasterEnabled: true})

	if err := m.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	first := m.Snapshot().Generation()
	if first == 0 {
		t.Fatalf("首次发布 generation 必须非零")
	}

	// 内容未变：30s 轮询重载不得递增代际，否则在途请求的 generation 会漂移。
	if err := m.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := m.Snapshot().Generation(); got != first {
		t.Fatalf("内容未变不应递增 generation: %d -> %d", first, got)
	}

	// 配置变化（禁用内置）→ 新代际。
	m.SetDisabledFactoryKeys([]string{"sample"})
	if err := m.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := m.Snapshot().Generation(); got != first+1 {
		t.Fatalf("配置变化应递增 generation: %d -> %d", first, got)
	}
}

func TestSnapshotRevisionIsMaxUpdatedAt(t *testing.T) {
	older := activeVersion("a", "1.0.0", fixture{Key: "a"}.Source())
	newer := activeVersion("b", "1.0.0", fixture{Key: "b"}.Source())
	newer.UpdatedAt = time.Date(2026, 10, 19, 18, 30, 0, 0, time.UTC)
	m := MustNewManager(map[string]string{}, newFakeLoader(older, newer), Options{MasterEnabled: true})
	if err := m.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	want := newer.UpdatedAt.Format(time.RFC3339Nano)
	if got := m.Snapshot().Revision(); got != want {
		t.Fatalf("revision=%q want %q", got, want)
	}
	if status := m.Status(); status.Revision != want {
		t.Fatalf("runtime/status 必须暴露 revision: %+v", status)
	}
}

func TestSnapshotReloadErrorKeepsPrevious(t *testing.T) {
	loader := newFakeLoader(activeVersion("sample", "1.0.0", fixture{}.Source()))
	m := newManager(t, loader, Options{MasterEnabled: true})
	if err := m.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	before := m.Snapshot()

	loader.err = errors.New("db down")
	if err := m.Reload(context.Background()); err == nil {
		t.Fatalf("期望 reload 失败")
	}
	if m.Snapshot() != before {
		t.Fatalf("加载失败必须保留上一份快照（fail-closed）")
	}
	if status := m.Status(); status.ReloadError == "" {
		t.Fatalf("runtime/status 必须暴露 reload_error")
	}

	loader.err = nil
	if err := m.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if status := m.Status(); status.ReloadError != "" {
		t.Fatalf("成功重载后 reload_error 应清空: %q", status.ReloadError)
	}
}

func TestSnapshotBeforeFirstReloadIsEmptyNotNil(t *testing.T) {
	m := newManager(t, nil, Options{MasterEnabled: true})
	snap := m.Snapshot()
	if snap == nil || len(snap.Keys()) != 0 {
		t.Fatalf("首次 Reload 前应返回空快照")
	}
	if entry := snap.Lookup("sample"); entry.Status != StatusNotRegistered {
		t.Fatalf("空快照 Lookup 应为 not_registered: %+v", entry)
	}
}
