package mj

import (
	"strconv"
	"strings"

	"newtg/gateway/internal/tasks"
)

// 本文件是 MJ 上游状态/进度 → 统一 7 态任务状态的归一化（R-MJ-15）。
//
// 【为什么不直接复用 tasks.ParseState】插件的 ParseState 把 MODAL 当
// unknown、CANCEL 当 failure；但 MJ 上游的 MODAL 是「等待用户输入」的
// **非终态**，映射成 unknown 会让它在台账/轮询里长期停留在一个无法推进
// 的档位。MJ 需要自己的映射表；持久化值仍是同一套 7 态，两族共用列与
// 状态机，不新增枚举。

// Status 是一次上游状态快照的归一化结果。
//
// RawState 必须逐字保留上游原文：对外线协议（fetch/list DTO）回给客户端的
// 是原文，枚举只用于内部判定（relay-05 §3.1 甲类-1）。把原文丢掉会让
// 客户端从 "SUCCESS" 变成 "success"，属于对外可观察变更。
type Status struct {
	State           tasks.State
	RawState        string
	ProgressPercent int
}

// NormalizeStatus 把上游 status/progress 字符串归一化。
//
// progress 接受 "45%" 或 "45"，非法值归零（0%）而不是报错：状态归一化
// 是轮询路径，不能因为上游多给一个字符就丢掉整轮观测；真正决定终态的
// 是 state。
func NormalizeStatus(rawState, rawProgress string) Status {
	return Status{
		State:           MapState(rawState),
		RawState:        strings.TrimSpace(rawState),
		ProgressPercent: ParseProgress(rawProgress),
	}
}

// MapState 是 R-MJ-15 的冻结映射表：
//
//	NOT_START                 → not_start
//	SUBMITTED                 → submitted
//	QUEUED                    → queued
//	MODAL / IN_PROGRESS       → in_progress   （MODAL 在等待用户输入）
//	SUCCESS                   → success
//	FAILURE / CANCEL          → failure
//	其它                       → unknown
//
// 未知状态必须落到 unknown 且**不累计失败**：把无法识别的串猜成 failure
// 会触发退款，而退款是对「结果可能已产出」的直接资金损失。
func MapState(raw string) tasks.State {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "NOT_START":
		return tasks.StateNotStart
	case "SUBMITTED":
		return tasks.StateSubmitted
	case "QUEUED":
		return tasks.StateQueued
	case "MODAL", "IN_PROGRESS":
		return tasks.StateInProgress
	case "SUCCESS":
		return tasks.StateSuccess
	case "FAILURE", "CANCEL":
		return tasks.StateFailure
	}
	return tasks.StateUnknown
}

// IsFailure 报告归一化状态是否应触发失败处理（累计失败/退款）。
//
// 只有 failure 为真；unknown 显式为假。轮询侧的失败计数必须调用本函数，
// 而不是 `state != success`——后者会把 unknown 当成失败（R-MJ-15）。
func IsFailure(state tasks.State) bool {
	return state == tasks.StateFailure
}

// ParseProgress 把上游进度串解析为 0–100 的整数。
//
// 接受 "45%" / "45"；空、非数字、负值 → 0；超过 100 → 100（钳制）。
// 上游偶尔给 "100.0%" 这类带小数的形态，这里按前缀整数解析，失败归零。
func ParseProgress(raw string) int {
	s := strings.TrimSpace(raw)
	s = strings.TrimSuffix(s, "%")
	if s == "" {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	if n < 0 {
		return 0
	}
	if n > 100 {
		return 100
	}
	return n
}

// ProjectProgress 把内部数值进度投影为基线线协议形态 "N%".
//
// 先钳制再格式化：progress_percent 有 DB CHECK 0–100，但调用方可能传入
// 任意 int（上游/测试接缝），投影函数必须自己保证输出合法。
func ProjectProgress(percent int) string {
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	return strconv.Itoa(percent) + "%"
}

// ProjectStatus 生成客户端可见的 status 字段（relay-05 §3.1）。
//
// 规则：
//   - 有上游原始状态 → 逐字回传 raw（这是「上游回填」路径，线值不变）；
//   - 没有 raw（网关自身判定的状态）→ not_start 投影为空串、success/failure
//     投影为大写 SUCCESS/FAILURE，与基线网关侧写入值逐一相同；
//   - 其余（unknown 且无 raw）→ 空串，绝不把内部小写字面量泄露给客户端。
//
// 注意 raw 优先：CANCEL 经 MapState 变成内部 failure，但客户端应看到
// "CANCEL"（上游原文），而不是被改写成 "FAILURE"。
func ProjectStatus(state tasks.State, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw != "" {
		return raw
	}
	switch state {
	case tasks.StateNotStart:
		return ""
	case tasks.StateSuccess:
		return "SUCCESS"
	case tasks.StateFailure:
		return "FAILURE"
	}
	return ""
}
