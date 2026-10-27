// Package settingread 提供从配置中心读取标量设置的最小工具。
//
// 这些函数原先以未导出的形式住在 internal/platform，供任务轮询与保留策略
// 共用；perfsettings 也需要同一套「缺键 / JSON null / 脏值 → 回落默认」的
// 语义，故抽出成叶子包，避免 platform 与 perfsettings 互相依赖。
//
// 约定：
//   - 缺键与 nil reader 静默回落，不告警（首次部署的正常状态）；
//   - JSON null、类型不符、负值属于脏数据，回落并告警，便于运维定位；
//   - 日志统一带 `worker:` 前缀，与既有日志断言保持一致。
package settingread

import (
	"encoding/json"
	"log/slog"
)

// Reader 是配置中心读取面的最小接口（生产 *configcenter.Center）。
// 抽出接口让三态映射可以在不依赖 PG/Redis 的单测里验证（R-W9B-08）。
type Reader interface {
	Get(key string) (json.RawMessage, bool)
}

// Present 报告 key 是否在配置中心里落过库。值是否可用由 Bool/Int 判断：
// JSON null 也算「存在」，这样 Settings 的 Source 能区分「从未保存」与
// 「保存了脏值」。
func Present(reader Reader, key string) bool {
	if reader == nil {
		return false
	}
	_, ok := reader.Get(key)
	return ok
}

// Bool 读取布尔设置；缺省/非布尔/JSON null 一律回落 fallback。
//
// 与 Int 的指针对齐：null 解到 bool 会得到 false 且无错误，若不显式区分，
// 脏行会把「默认关」的键读成关闭、也可能把默认开的键误关；所以这里显式
// warn，便于发现脏数据。
func Bool(reader Reader, key string, fallback bool, log *slog.Logger) bool {
	if reader == nil {
		return fallback
	}
	raw, ok := reader.Get(key)
	if !ok {
		return fallback
	}
	var value *bool
	if err := json.Unmarshal(raw, &value); err != nil || value == nil {
		log.Warn("worker: setting is not a valid boolean; falling back to the fail-safe default",
			"key", key, "raw", string(raw), "fallback", fallback)
		return fallback
	}
	return *value
}

// Int 读取一个非负整数设置。
//
// 返回 ok=false 覆盖四种情况：未配置、JSON null、解析失败、负值。调用方
// 因此只需区分「有合法值」与「回落默认」；具体的告警在这里发出，便于运维
// 定位脏行。
//
// 为什么用 *int64：json.Unmarshal 把 `null` 解到 int64 时返回 nil error 且
// 保持零值，会被误读成「显式 0 = 禁用」——这是 fail-open（静默关掉兜底）。
// 解到指针后 null → nil 指针，明确落入「回落默认 + warn」。
func Int(reader Reader, key string, log *slog.Logger) (int64, bool) {
	if reader == nil {
		return 0, false
	}
	raw, ok := reader.Get(key)
	if !ok {
		return 0, false
	}
	var value *int64
	if err := json.Unmarshal(raw, &value); err != nil {
		log.Warn("worker: setting is not a valid integer; falling back to the design default",
			"key", key, "raw", string(raw), "error", err)
		return 0, false
	}
	if value == nil {
		log.Warn("worker: setting is null; falling back to the design default",
			"key", key, "raw", string(raw))
		return 0, false
	}
	if *value < 0 {
		// 设计只冻结了 `<=0 禁用`：0 是唯一的禁用形式。负值是配置错误，
		// 不是另一种禁用写法（见 platform.taskPollLimits 的注释）。
		log.Warn("worker: setting is negative; falling back to the design default",
			"key", key, "value", *value)
		return 0, false
	}
	return *value, true
}
