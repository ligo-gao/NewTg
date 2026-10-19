package tasks

import (
	"strings"

	"newtg/gateway/internal/plugin"
)

// 本文件是只读投影：产物清单与 usage 事实都从持久化的 task.data 派生，
// 不建第二份真相（relay-06 §10.5 / §7.1）。

// ProjectArtifacts 从 task.data 投影产物清单。
//
// 数据形状约定（插件把 listArtifacts 的结果写进 taskData 的 `artifacts`）：
//
//	{"artifacts": [{"key": "v1", "type": "video", "mimeType": "video/mp4"}]}
//
// 兼容常见的 `id` / `kind` / `mime_type` 拼写；缺失 key 时用稳定序号
// `artifact_<i>`，保证 T6b 的 content 端点在同一份 data 上得到同一组 key。
// 重复 key 只保留第一项：产物 key 是任务内标识，重复会让 content 路由产生
// 二义。
func ProjectArtifacts(data map[string]any) []ArtifactView {
	raw, ok := data["artifacts"].([]any)
	if !ok || len(raw) == 0 {
		return nil
	}
	out := make([]ArtifactView, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for i, item := range raw {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		view, ok := projectArtifactItem(i, obj)
		if !ok {
			continue
		}
		if _, dup := seen[view.Key]; dup {
			continue
		}
		seen[view.Key] = struct{}{}
		out = append(out, view)
	}
	return out
}

// projectArtifactItem 读取单项产物；缺 key 或 type 时返回 ok=false。
//
// 设计把产物 key 定为任务内稳定标识，content 端点靠它定位。猜一个
// `artifact_<i>` 或默认 `file` 会把「插件写错字段」伪装成合法产物，
// 用户点开只会得到错误内容；因此宁可跳过并在类型上留下空洞。
// （诊断由调用方按 items 数量与 task.data 对比发现，T6b 补结构化诊断。）
func projectArtifactItem(_ int, obj map[string]any) (ArtifactView, bool) {
	key := toString(obj["key"])
	if key == "" {
		key = toString(obj["id"])
	}
	kind := toString(obj["type"])
	if kind == "" {
		kind = toString(obj["kind"])
	}
	if key == "" || kind == "" {
		return ArtifactView{}, false
	}
	mime := toString(obj["mimeType"])
	if mime == "" {
		mime = toString(obj["mime_type"])
	}
	return ArtifactView{Key: key, Type: kind, MIMEType: mime}, true
}

// DeclaredUsageFields 返回插件 usageSchema 声明的字段名集合。
//
// 键 = 插件声明的字段名（不是单位枚举）；usageProfiles 里按模型分化的
// schema 也并入同一集合——读取侧只关心「这个字段名是否由插件声明」，
// 具体模型的档位差异属于计价侧。
func DeclaredUsageFields(m *plugin.Manifest) map[string]struct{} {
	out := make(map[string]struct{})
	if m == nil {
		return out
	}
	for name := range m.UsageSchema {
		out[name] = struct{}{}
	}
	for _, profile := range m.UsageProfiles {
		for name := range profile.Schema {
			out[name] = struct{}{}
		}
	}
	return out
}

// ProjectUsage 从 task.data.usage 投影用量事实。
//
// 设计约束（brief / relay-06 §9.3）：只返回插件声明字段的**数量**，
// 绝不返回额度/价格；未声明字段一律丢弃，避免插件把内部成本字段透给客户端。
// 值只接受数值与字符串（UsageFactValue 的两种承载），布尔/对象等形状忽略。
func ProjectUsage(data map[string]any, declared map[string]struct{}) map[string]any {
	raw, ok := data["usage"].(map[string]any)
	if !ok || len(raw) == 0 || len(declared) == 0 {
		return nil
	}
	out := make(map[string]any, len(raw))
	for name, value := range raw {
		if _, ok := declared[name]; !ok {
			continue
		}
		switch typed := value.(type) {
		case float64:
			out[name] = typed
		case string:
			out[name] = typed
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// NormalizeStateFilter 把查询串里的状态值解析为合法状态集合。
// 空串返回 (nil, true)；非法值返回 ok=false，由 handler 映射 400。
func NormalizeStateFilter(raw string) ([]State, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, true
	}
	parts := strings.Split(raw, ",")
	out := make([]State, 0, len(parts))
	for _, part := range parts {
		st := State(strings.ToLower(strings.TrimSpace(part)))
		if !st.Valid() {
			return nil, false
		}
		out = append(out, st)
	}
	return out, true
}
