package mj

import (
	"regexp"
	"strconv"

	"newtg/gateway/internal/tasks"
)

// MjId 是上游 midjourney-proxy 返回的任务 id（= tasks.upstream_task_id）。
//
// 【为什么 MjId 不是唯一键（R-MJ-05）】上游同一个 MjId 可能被不同用户各
// 拿到一次（code 21「任务已存在」）。归属键是 (user_id, upstream_task_id)：
// 查询/复用只在同用户内匹配，图片代理按候选 owner 逐个验签。把 MjId 当
// 全局唯一会让第二个用户在 code 21 时无法落库，或被迫跨用户复用同一行。
const (
	// MaxMjIDLen 与 R-MJ-16 的路径 id 规则一致：1–128 个安全字符。
	MaxMjIDLen = 128
	// MaxOwnerCandidates 是图片代理按 MjId 取候选 owner 的上限。
	//
	// 正常情况下同一 MjId 的候选数 = 用户数（通常 1）。上限防止异常数据
	// （例如上游把同一 id 发给大量用户）让图片代理退化成一次全表扫描 +
	// 逐候选 HMAC 验证的 DoS 面。
	MaxOwnerCandidates = 100
)

// mjIDPattern 是 R-MJ-16 冻结的 id 字符集：字母/数字/下划线/连字符，1–128。
//
// 这个字符集同时用于路径 id（fetch/list/image-seed）与 body 里的 taskId：
// 它们都会被拼进上游 URL 或 SQL 参数，限制字符集是避免路径穿越/日志注入
// 类型问题的第一道防线（真正的防护仍是参数化查询与 URL 构造）。
//
// 长度用 MaxMjIDLen 拼进表达式（而不是在正则里再写一次 128）：两处独立
// 的字面量会在有人调上界时漂移，边界测试也就失去意义。
var mjIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,` + strconv.Itoa(MaxMjIDLen) + `}$`)

// ValidMjID 报告 s 是否符合 MjId 形状。空串不是合法 id。
func ValidMjID(s string) bool {
	return mjIDPattern.MatchString(s)
}

// ParseMjID 校验并返回路径/请求中的 MjId。
//
// 不做 TrimSpace：路径参数里的前后空格是客户端构造错误，静默修剪会让
// 「不存在的 id」与「形似存在的 id」产生同一响应，反而掩盖问题。
func ParseMjID(raw string) (string, *Error) {
	if !ValidMjID(raw) {
		return "", BadRequest(DescriptionInvalidID)
	}
	return raw, nil
}

// ReuseKey 是 code 21 复用查找的归属键（R-MJ-05）。
//
// 字段顺序/命名直接对应 (user_id, upstream_task_id) 复合索引，a2 用它查
// 已存在行；不要把它与计费幂等键 RequestID 混用（relay-00 §5.4）。
type ReuseKey struct {
	UserID         int64
	UpstreamTaskID string
}

// NewReuseKey 构造复用键并校验 MjId 形状。
func NewReuseKey(userID int64, mjID string) (ReuseKey, *Error) {
	if userID <= 0 {
		return ReuseKey{}, BadRequest(DescriptionInvalidRequest)
	}
	if !ValidMjID(mjID) {
		return ReuseKey{}, BadRequest(DescriptionInvalidID)
	}
	return ReuseKey{UserID: userID, UpstreamTaskID: mjID}, nil
}

// ReusableTask 报告 existing 是否是同一用户、同一 MjId 的可复用任务行。
//
// 只做归属判定，不判状态：code 21 可能已出结果（success）也可能仍在跑
// （in_progress），两种都应返回已有任务而不是新建行。family 必须是 mj：
// 插件任务即便巧合有同名 upstream id 也不得被 MJ 复用。
func ReusableTask(existing *tasks.Task, key ReuseKey) bool {
	if existing == nil {
		return false
	}
	return existing.Family == tasks.FamilyMJ &&
		existing.UserID == key.UserID &&
		existing.UpstreamTaskID == key.UpstreamTaskID
}

// LimitCandidates 把候选切片截断到 MaxOwnerCandidates。
//
// 泛型是为了同时服务「任务行候选」与「图片代理候选」两种投影；调用方
// 应先按 id 排序再传入，保证截断结果是确定的（否则每次请求可能验证
// 不同的候选集合，行为不可复现）。
func LimitCandidates[T any](candidates []T) []T {
	if len(candidates) <= MaxOwnerCandidates {
		return candidates
	}
	return candidates[:MaxOwnerCandidates]
}

// ParseListIDs 校验 list-by-condition 的 ids 数组（R-MJ-16）。
//
// 空数组合法（设计：空数组返回 []）。每个元素必须是合法 MjId；总数上限
// 100 防止一次查询把解析/DB 成本放大。结果保持输入顺序并保留重复项——
// 去重会改变客户端按输入对位的语义。
func ParseListIDs(ids []string) ([]string, *Error) {
	if len(ids) > MaxListIDs {
		return nil, BadRequest(DescriptionTooManyIDs)
	}
	out := make([]string, len(ids))
	for i, id := range ids {
		if !ValidMjID(id) {
			return nil, BadRequest(DescriptionInvalidID)
		}
		out[i] = id
	}
	return out, nil
}
