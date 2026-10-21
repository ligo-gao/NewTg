package mj

import (
	"encoding/json"
	"strconv"
	"time"

	"newtg/gateway/internal/tasks"
)

// 本文件把任务行投影为 MJ 查询族的对外 DTO（relay-05 §1.2）。
//
// 投影与数据面分离的理由：fetch/list 只能返回「客户端可见」的字段集合，
// 而 tasks.Task 同时携带 family/plugin_key/private_data 等内部列。直接把
// Task 序列化出去会泄露归属与结算锚点；DTO 由这里显式白名单构造，新增
// 内部列不会自动出现在 MJ 响应里。
//
// `data` 里的上游快照（产物 URL / 按钮 / 上游 properties）只在这里投影；
// /api/task/self 的管理台账不含 data（repo 的投影显式省略）。

// DataDelta 把一轮上游查询结果转成 data 的**增量**。
//
// 只写非空字段：tasks 的 ApplyPoll 用 jsonb `||` 合并，空值若也写入会把
// 上一轮已有的产物 URL/按钮冲掉（上游某些轮次只返回 progress）。
func (r *QueryResult) DataDelta() map[string]any {
	if r == nil {
		return nil
	}
	delta := map[string]any{}
	if r.ImageURL != "" {
		delta[DataImageURL] = r.ImageURL
	}
	if r.VideoURL != "" {
		delta[DataVideoURL] = r.VideoURL
	}
	if len(r.VideoURLs) > 0 {
		delta[DataVideoURLs] = append([]string(nil), r.VideoURLs...)
	}
	if len(r.Buttons) > 0 {
		delta[DataButtons] = r.Buttons
	}
	if len(r.Properties) > 0 {
		delta[DataProperties] = r.Properties
	}
	if r.PromptEn != "" {
		delta[DataPromptEn] = r.PromptEn
	}
	if r.Description != "" {
		delta[DataDescription] = r.Description
	}
	return delta
}

// TaskDTO 生成 fetch（单条）与 list-by-condition（数组元素）共用的任务 DTO。
//
// 字段名沿用基线线协议；状态/进度按 ProjectStatus/ProjectProgress 投影，
// 因此上游原始状态逐字回传（CANCEL 不会被改写成 FAILURE），网关侧终态仍是
// 大写的 SUCCESS/FAILURE（relay-05 §3.1）。
//
// nil task 返回 nil：调用方（批量查询）用 nil 表示「跳过该 id」，绝不能把
// 一个空对象塞进结果数组让客户端误以为任务存在。
func TaskDTO(task *tasks.Task) map[string]any {
	if task == nil {
		return nil
	}
	data := task.Data
	if data == nil {
		data = map[string]any{}
	}
	properties := task.Properties
	if properties == nil {
		properties = map[string]any{}
	}
	prompt, _ := properties[PropertyPrompt].(string)
	out := map[string]any{
		"id":       task.UpstreamTaskID,
		"action":   task.Action,
		"prompt":   prompt,
		"promptEn": stringValue(data[DataPromptEn]),
		// description 是上游对任务的描述；同样属于审计快照，只走 DTO。
		"description": stringValue(data[DataDescription]),
		"status":      ProjectStatus(task.State, task.RawState),
		"progress":    ProjectProgress(task.ProgressPercent),
		"imageUrl":    stringValue(data[DataImageURL]),
		"videoUrl":    stringValue(data[DataVideoURL]),
		"videoUrls":   stringList(data[DataVideoURLs]),
		"buttons":     anyList(data[DataButtons]),
		"properties":  anyMap(data[DataProperties]),
		"failReason":  task.FailReason,
		// 时间戳沿用基线的 epoch 毫秒；缺失为 0，不省略字段。
		"submitTime": epochMillis(task.SubmitTime),
		"startTime":  epochMillis(task.StartTime),
		"finishTime": epochMillis(task.FinishTime),
	}
	return out
}

// stringValue 把 jsonb 解出的标量转成字符串。
//
// jsonb 数字统一解成 float64（repo decodeMap）；这里不再猜测业务含义，
// 只把字符串原样返回、数字按十进制格式化，其余类型返回空串。
func stringValue(v any) string {
	switch typed := v.(type) {
	case string:
		return typed
	case float64:
		// jsonb 数字统一解成 float64；-1 精度按最短十进制表示，避免 1e+06。
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case json.Number:
		return typed.String()
	}
	return ""
}

// stringList 把 jsonb 数组规范化为 []string。
//
// 永远返回非 nil 切片：DTO 的 videoUrls 必须是 [] 而不是 null（客户端按
// 数组迭代，null 会让部分 SDK 抛错）。
func stringList(v any) []string {
	out := []string{}
	switch typed := v.(type) {
	case []string:
		out = append(out, typed...)
	case []any:
		for _, item := range typed {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

// anyList 把 jsonb 数组投影为 []any；非数组/null 返回空数组。
func anyList(v any) []any {
	if items, ok := v.([]any); ok && items != nil {
		return items
	}
	return []any{}
}

// anyMap 把 jsonb 对象投影为 map；非对象/null 返回空对象。
func anyMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok && m != nil {
		return m
	}
	return map[string]any{}
}

// epochMillis 把时间转成 Unix 毫秒；nil/零值返回 0。
func epochMillis(t *time.Time) int64 {
	if t == nil || t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}
