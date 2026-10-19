package httpapi

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"sync"
	"time"

	"newtg/gateway/internal/plugin"
	"newtg/gateway/internal/store"
)

// fakeTaskPluginStore 是管理端点单测用的内存仓储。
//
// 刻意模拟真实仓储的可观察语义（幂等返回已有行、异 hash 冲突、激活互斥、
// 唯一版本禁删、乐观锁），但**不**试图替代 PostgreSQL 事务测试：真正的
// FOR UPDATE / 部分唯一索引 / 并发行为在 *integration_test.go 里对真库验证。
type fakeTaskPluginStore struct {
	mu       sync.Mutex
	plugins  map[string]store.TaskPlugin
	versions map[string][]store.TaskPluginVersion
	sources  []store.PluginMarketplaceSource
	nextID   int

	uploadCalls int
	listErr     error
}

func newFakeTaskPluginStore() *fakeTaskPluginStore {
	return &fakeTaskPluginStore{
		plugins:  map[string]store.TaskPlugin{},
		versions: map[string][]store.TaskPluginVersion{},
	}
}

func (f *fakeTaskPluginStore) List(context.Context) ([]store.TaskPlugin, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]store.TaskPlugin, 0, len(f.plugins))
	for _, p := range f.plugins {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (f *fakeTaskPluginStore) Get(_ context.Context, key string) (*store.TaskPlugin, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.plugins[key]
	if !ok {
		return nil, store.ErrTaskPluginNotFound
	}
	return &p, nil
}

func (f *fakeTaskPluginStore) ListVersions(_ context.Context, key string) ([]store.TaskPluginVersion, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.plugins[key]; !ok {
		return nil, store.ErrTaskPluginNotFound
	}
	out := append([]store.TaskPluginVersion(nil), f.versions[key]...)
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (f *fakeTaskPluginStore) GetVersion(_ context.Context, key, version string) (*store.TaskPluginVersion, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, v := range f.versions[key] {
		if v.Version == version {
			copied := v
			return &copied, nil
		}
	}
	if _, ok := f.plugins[key]; !ok {
		return nil, store.ErrTaskPluginNotFound
	}
	return nil, store.ErrTaskPluginVersionNotFound
}

func (f *fakeTaskPluginStore) UpsertBuiltinPlugins(_ context.Context, inputs []store.BuiltinPlugin) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, in := range inputs {
		p, ok := f.plugins[in.Key]
		if !ok {
			p = store.TaskPlugin{Key: in.Key, Name: in.Name, Icon: in.Icon, Source: "builtin",
				Builtin: true, Enabled: true, Description: in.Description, UpdatedAt: time.Now().UTC()}
		} else {
			p.Name, p.Icon, p.Description = in.Name, in.Icon, in.Description
			p.Source, p.Builtin = "builtin", true
		}
		f.plugins[in.Key] = p
	}
	return nil
}

func (f *fakeTaskPluginStore) Upload(_ context.Context, in store.TaskPluginUpload) (*store.TaskPlugin, *store.TaskPluginVersion, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uploadCalls++
	p, ok := f.plugins[in.Key]
	if !ok {
		now := time.Now().UTC()
		source, builtin := "upload", false
		if in.Builtin {
			source, builtin = "builtin", true
		}
		p = store.TaskPlugin{
			Key: in.Key, Name: in.Name, Icon: in.Icon, Source: source, Builtin: builtin,
			Enabled: true, Description: in.Description, CreatedAt: now, UpdatedAt: now,
		}
		f.plugins[in.Key] = p
	} else if in.Builtin && (!p.Builtin || p.Source != "builtin") {
		p.Builtin, p.Source = true, "builtin"
		f.plugins[in.Key] = p
	}
	for _, existing := range f.versions[in.Key] {
		if existing.Version != in.Version {
			continue
		}
		if existing.SourceHash != in.SourceHash {
			return nil, nil, false, store.ErrTaskPluginVersionConflict
		}
		return &p, &existing, false, nil
	}
	v := store.TaskPluginVersion{
		ID: "v" + strconv.Itoa(f.nextID), Key: in.Key, Version: in.Version, APIVersion: in.APIVersion,
		SourceText: in.SourceText, SourceHash: in.SourceHash, Icon: in.Icon,
		Enabled: true, Active: false, UploadedBy: in.UploadedBy, CreatedAt: time.Now().UTC(),
	}
	f.nextID++
	f.versions[in.Key] = append(f.versions[in.Key], v)
	return &p, &v, true, nil
}

func (f *fakeTaskPluginStore) Activate(_ context.Context, key, version string, expect *time.Time) (*store.TaskPlugin, *store.TaskPluginVersion, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.plugins[key]
	if !ok {
		return nil, nil, store.ErrTaskPluginNotFound
	}
	if expect != nil && !p.UpdatedAt.UTC().Equal(expect.UTC()) {
		return nil, nil, store.ErrTaskPluginStale
	}
	var target *store.TaskPluginVersion
	for i := range f.versions[key] {
		v := &f.versions[key][i]
		v.Active = v.Version == version
		if v.Active {
			v.Enabled = true
			target = v
		}
	}
	if target == nil {
		return nil, nil, store.ErrTaskPluginVersionNotFound
	}
	p.ActiveVersion = &version
	p.UpdatedAt = time.Now().UTC()
	f.plugins[key] = p
	return &p, target, nil
}

func (f *fakeTaskPluginStore) SetEnabled(_ context.Context, key string, enabled bool, expect *time.Time) (*store.TaskPlugin, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.plugins[key]
	if !ok {
		return nil, store.ErrTaskPluginNotFound
	}
	if expect != nil && !p.UpdatedAt.UTC().Equal(expect.UTC()) {
		return nil, store.ErrTaskPluginStale
	}
	p.Enabled = enabled
	p.UpdatedAt = time.Now().UTC()
	f.plugins[key] = p
	return &p, nil
}

func (f *fakeTaskPluginStore) DeleteVersion(_ context.Context, key, version string, expect *time.Time) (*store.TaskPlugin, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.plugins[key]
	if !ok {
		return nil, "", store.ErrTaskPluginNotFound
	}
	if expect != nil && !p.UpdatedAt.UTC().Equal(expect.UTC()) {
		return nil, "", store.ErrTaskPluginStale
	}
	versions := f.versions[key]
	if len(versions) <= 1 {
		return nil, "", store.ErrTaskPluginVersionRequired
	}
	var target *store.TaskPluginVersion
	remaining := make([]store.TaskPluginVersion, 0, len(versions)-1)
	for i := range versions {
		if versions[i].Version == version {
			v := versions[i]
			target = &v
			continue
		}
		remaining = append(remaining, versions[i])
	}
	if target == nil {
		return nil, "", store.ErrTaskPluginVersionNotFound
	}
	promoted := ""
	if target.Active {
		for _, v := range remaining {
			if v.Enabled {
				promoted = v.Version
				break
			}
		}
		if promoted == "" {
			return nil, "", store.ErrTaskPluginVersionRequired
		}
		for i := range remaining {
			remaining[i].Active = remaining[i].Version == promoted
		}
		p.ActiveVersion = &promoted
	}
	f.versions[key] = remaining
	p.UpdatedAt = time.Now().UTC()
	f.plugins[key] = p
	return &p, promoted, nil
}

func (f *fakeTaskPluginStore) ListMarketSources(context.Context) ([]store.PluginMarketplaceSource, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.PluginMarketplaceSource(nil), f.sources...), nil
}

func (f *fakeTaskPluginStore) GetMarketSource(_ context.Context, id string) (*store.PluginMarketplaceSource, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, source := range f.sources {
		if source.ID == id {
			copied := source
			return &copied, nil
		}
	}
	return nil, store.ErrMarketSourceNotFound
}

func (f *fakeTaskPluginStore) ReplaceMarketSources(_ context.Context, inputs []store.MarketSourceInput) ([]store.PluginMarketplaceSource, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	next := make([]store.PluginMarketplaceSource, 0, len(inputs))
	for _, in := range inputs {
		id := in.ID
		if id == "" {
			f.nextID++
			id = "s" + strconv.Itoa(f.nextID)
		}
		next = append(next, store.PluginMarketplaceSource{
			ID: id, Name: in.Name, URL: in.URL, Enabled: in.Enabled, LastCheckStatus: "pending",
		})
	}
	f.sources = next
	return append([]store.PluginMarketplaceSource(nil), next...), nil
}

func (f *fakeTaskPluginStore) UpdateMarketSourceCheck(_ context.Context, id, status, lastError string, at time.Time) (*store.PluginMarketplaceSource, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.sources {
		if f.sources[i].ID != id {
			continue
		}
		f.sources[i].LastCheckStatus = status
		f.sources[i].LastCheckAt = &at
		if lastError == "" {
			f.sources[i].LastError = nil
		} else {
			f.sources[i].LastError = &lastError
		}
		copied := f.sources[i]
		return &copied, nil
	}
	return nil, store.ErrMarketSourceNotFound
}

func (f *fakeTaskPluginStore) seed(p store.TaskPlugin, versions ...store.TaskPluginVersion) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.plugins[p.Key] = p
	f.versions[p.Key] = versions
}

// memoryVersionLoader 是 Manager 的单测 loader。
type memoryVersionLoader struct {
	active  []plugin.ActiveVersion
	byKey   map[string]plugin.ActiveVersion
	loadErr error
}

func (m *memoryVersionLoader) LoadActive(context.Context) ([]plugin.ActiveVersion, error) {
	if m.loadErr != nil {
		return nil, m.loadErr
	}
	return append([]plugin.ActiveVersion(nil), m.active...), nil
}

func (m *memoryVersionLoader) LoadVersion(_ context.Context, key, version string) (*plugin.ActiveVersion, error) {
	v, ok := m.byKey[key+"/"+version]
	if !ok {
		return nil, nil
	}
	return &v, nil
}

// errFakeStore 只用于构造 500 分支。
var errFakeStore = errors.New("fake store failure")
