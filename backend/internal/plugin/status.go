package plugin

import (
	"sort"
	"time"
)

// RuntimeStatus 是 `GET /api/plugin/task/runtime/status` 的节点本地投影
// （relay-06 §2.5；t3-brief 冻结约定 7）。
//
// 注意 `generation` 是**节点本地**单调代际，跨节点诊断以 DB revision 为准；
// 因此 Revision 也随响应下发，避免运维把两个节点的 generation 直接比较。
type RuntimeStatus struct {
	Node        string                `json:"node"`
	Generation  uint64                `json:"generation"`
	Revision    string                `json:"revision"`
	ReloadedAt  time.Time             `json:"reloaded_at"`
	ReloadError string                `json:"reload_error,omitempty"`
	Plugins     []PluginRuntimeStatus `json:"plugins"`
}

// PluginRuntimeStatus 是逐插件的编译与路由错误投影（F-08-36）。
type PluginRuntimeStatus struct {
	Key     string       `json:"key"`
	Status  PluginStatus `json:"status"`
	Version string       `json:"version,omitempty"`
	Layer   Layer        `json:"layer,omitempty"`
	Errors  Issues       `json:"errors,omitempty"`
}

// Status 返回当前快照的运行时状态。
//
// 逐插件按 key 升序，保证响应稳定（前端按副本分组展示时不会抖动）。
func (m *Manager) Status() RuntimeStatus {
	m.mu.Lock()
	node := m.node
	reloadedAt := m.reloadedAt
	reloadErr := m.reloadErr
	m.mu.Unlock()

	snap := m.Snapshot()
	status := RuntimeStatus{
		Node:        node,
		Generation:  snap.Generation(),
		Revision:    snap.Revision(),
		ReloadedAt:  reloadedAt,
		ReloadError: reloadErr,
		Plugins:     make([]PluginRuntimeStatus, 0, len(snap.entries)),
	}
	for _, key := range snap.Keys() {
		entry := snap.entries[key]
		status.Plugins = append(status.Plugins, PluginRuntimeStatus{
			Key:     entry.Key,
			Status:  entry.Status,
			Version: entry.Version,
			Layer:   entry.Layer,
			Errors:  append(Issues(nil), entry.Errors...),
		})
	}
	sort.SliceStable(status.Plugins, func(i, j int) bool { return status.Plugins[i].Key < status.Plugins[j].Key })
	return status
}
