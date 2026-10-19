// Package plugins 承载编入二进制的任务插件源（relay-06 §2.1）。
//
// 目录约定：`tasks/<key>/plugin.js`，每个插件一个目录。本包只负责把源文本
// 交给运行时包（internal/plugin）注册工厂；编译、校验与注册都不在这里做，
// 以免「embed 包」与「运行时包」互相依赖。
//
// 为什么内置插件必须编入二进制：内置插件是构建期产物，启动时加载失败属于
// 不可恢复错误（relay-06 §2.1 要求「失败即 panic」，由调用方使用
// plugin.MustNewManager 实现）。从磁盘读内置插件会让部署缺失目录时静默降级。
package plugins

import (
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
)

//go:embed tasks/*/plugin.js
var taskPluginFS embed.FS

// TaskPluginSources 返回 `tasks/<key>/plugin.js` 的源文本表（key → source）。
//
// key 取目录名（不是 manifest 内的 key）：运行时会强制二者一致（不一致
// 视为构建期错误），这样目录布局就是 plugin key 的唯一事实源，避免
// 「文件名一个 key、meta 里另一个 key」的静默歧义。
func TaskPluginSources() (map[string]string, error) {
	entries, err := fs.ReadDir(taskPluginFS, "tasks")
	if err != nil {
		return nil, fmt.Errorf("plugins: 读取内置任务插件目录: %w", err)
	}
	out := make(map[string]string, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		file := path.Join("tasks", name, "plugin.js")
		source, err := taskPluginFS.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("plugins: 读取内置插件 %q: %w", name, err)
		}
		out[name] = string(source)
	}
	return out, nil
}

// TaskPluginKeys 返回内置插件 key 的升序列表（诊断与测试用）。
func TaskPluginKeys() ([]string, error) {
	sources, err := TaskPluginSources()
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(sources))
	for key := range sources {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys, nil
}

// SourceKey 从目录名推导 key；保留为函数以便未来支持嵌套目录时只改一处。
func SourceKey(dir string) string { return strings.TrimSpace(dir) }
