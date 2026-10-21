package mj

import (
	"time"

	"newtg/gateway/internal/tasks"
)

// tasks.fetch_mode 的族内取值（与迁移 CHECK {poll,callback,inline} 逐字一致）。
const (
	// FetchModePoll 是 MJ 的默认取回方式：后台轮询上游。
	FetchModePoll = "poll"
	// FetchModeInline 只用于 upload-discord-images 的即时终态行（R-MJ-14）。
	FetchModeInline = "inline"
)

// MJ 附加字段的 jsonb 键名。
//
// R-MJ-03：两族共用 tasks 表，MJ 专属字段一律落 data/properties，不新增
// 宽列。分工是安全边界而不是审美：
//   - properties 会出现在 /api/task/self 的投影里（tasks/model.go:119），
//     只允许放轻量元数据（prompt）；
//   - data 不在台账投影（repo.go 的台账查询显式置 nil），适合放上游快照、
//     产物 URL、按钮与上传结果。
//
// 任何 base64/蒙版/凭据都不得写入这两列（设计 §3.3 与安全约束）。
const (
	// DataCode/DataDescription 是上游受理码与描述的审计快照，由 TaskRow
	// 从 Submission.Code/Description 写入。
	DataCode        = "code"
	DataDescription = "description"
	// DataUploadURLs 只用于 upload-discord-images 的 inline 终态行。
	DataUploadURLs = "upload_urls"

	// PropertyPrompt 是 prompt 在 properties 里的键。
	PropertyPrompt = "prompt"

	// PrivateRequestID 是计费幂等锚点（契约 §5.4；settleTerminal 依赖它）。
	// 它与客户端提交幂等键（Idempotency-Key）**不是同一个东西**，不得混用。
	PrivateRequestID = "request_id"
	// PrivateTokenID 是提交所用令牌，供结算/退款按 (request_id, token_id)
	// 定位令牌额度（handlers_tasks.go 同口径）。
	PrivateTokenID = "token_id"
)

// Submission 是提交成功后构造任务行的输入。
type Submission struct {
	PublicTaskID    string
	UpstreamTaskID  string
	Action          Action
	UserID          int64
	TokenID         int64
	Group           string
	ChannelID       int64
	LockedChannelID int64
	OriginTaskIDs   []string
	Prompt          string
	State           tasks.State
	RawState        string
	ProgressPercent int
	Quota           int64
	SubmitTime      time.Time
	RequestID       string
	// Code/Description 是上游受理码与描述（只进 data，不建宽列）。
	Code        *int
	Description string

	// RetentionDeadline 只在 a2 写终态行时使用；提交行通常为空。
	RetentionDeadline *time.Time

	// Data / Properties 是调用方追加的上游快照与轻量元数据；
	// TaskRow 会把 Code/Description 与 prompt 分别合并进 data/properties，
	// 调用方不得放入二进制载荷。
	Data       map[string]any
	Properties map[string]any
}

// TaskRow 把一次受理映射为 family='mj' 的任务行。
//
// 冻结字段（R-MJ-03）：
//   - Family=FamilyMJ，PluginKey=""，PluginGeneration=0（族内无插件绑定）；
//   - UpstreamTaskID 即 MjId（对外 id），PublicTaskID 只是内部唯一 id；
//   - FetchMode=poll（上传端点走 UploadInlineTask，不经过这里）。
//
// ProgressPercent 钳制到 0–100：tasks 列有 CHECK，越界会让整条插入失败，
// 而进度只是展示值，钳制比拒绝提交更安全。
func TaskRow(s Submission) *tasks.Task {
	data := mergeData(s.Data)
	if s.Code != nil {
		data[DataCode] = *s.Code
	}
	if s.Description != "" {
		data[DataDescription] = s.Description
	}
	task := &tasks.Task{
		PublicTaskID:      s.PublicTaskID,
		UpstreamTaskID:    s.UpstreamTaskID,
		Family:            tasks.FamilyMJ,
		Action:            string(s.Action),
		PluginKey:         "",
		PluginGeneration:  0,
		FetchMode:         FetchModePoll,
		LockedChannelID:   s.LockedChannelID,
		OriginTaskIDs:     append([]string(nil), s.OriginTaskIDs...),
		State:             s.State,
		RawState:          s.RawState,
		ProgressPercent:   clampPercent(s.ProgressPercent),
		RetentionDeadline: s.RetentionDeadline,
		UserID:            s.UserID,
		Group:             s.Group,
		ChannelID:         s.ChannelID,
		Quota:             s.Quota,
		Properties:        mergePrompt(s.Properties, s.Prompt),
		Data:              data,
		PrivateData:       privateData(s.RequestID, s.TokenID),
	}
	if !s.SubmitTime.IsZero() {
		submitTime := s.SubmitTime
		task.SubmitTime = &submitTime
	}
	return task
}

// UploadSubmission 是 upload-discord-images 的输入。
type UploadSubmission struct {
	PublicTaskID      string
	UserID            int64
	TokenID           int64
	Group             string
	ChannelID         int64
	Prompt            string
	Quota             int64
	SubmitTime        time.Time
	RequestID         string
	UploadURLs        []string
	RetentionDeadline *time.Time
	Data              map[string]any
	Properties        map[string]any
}

// UploadInlineTask 构造上传端点的 inline 终态行（R-MJ-14）。
//
// 上传没有上游任务 id（上游返回的是 Discord 附件 URL 数组），因此：
//   - upstream_task_id 为空串（不伪造 MjId——基线伪造 MjId 会让 fetch 查不到
//     真任务、让图片代理指向错误对象）；
//   - state=success、progress=100，提交即终态（fetch_mode=inline），
//     不参与轮询；
//   - data.upload_urls 保存附件 URL 列表，供审计/回查。
//
// 计费由 a2 在提交路径完成；本函数只表达行语义，不动资金。
func UploadInlineTask(s UploadSubmission) *tasks.Task {
	urls := append([]string{}, s.UploadURLs...)
	data := mergeData(s.Data)
	data[DataUploadURLs] = urls

	task := &tasks.Task{
		PublicTaskID:      s.PublicTaskID,
		UpstreamTaskID:    "",
		Family:            tasks.FamilyMJ,
		Action:            string(ActionUpload),
		PluginKey:         "",
		PluginGeneration:  0,
		FetchMode:         FetchModeInline,
		State:             tasks.StateSuccess,
		RawState:          "",
		ProgressPercent:   100,
		RetentionDeadline: s.RetentionDeadline,
		UserID:            s.UserID,
		Group:             s.Group,
		ChannelID:         s.ChannelID,
		Quota:             s.Quota,
		Properties:        mergePrompt(s.Properties, s.Prompt),
		Data:              data,
		PrivateData:       privateData(s.RequestID, s.TokenID),
	}
	if !s.SubmitTime.IsZero() {
		submitTime := s.SubmitTime
		task.SubmitTime = &submitTime
	}
	return task
}

// mergePrompt 复制调用方 properties 并写入 prompt。
//
// 复制而不是原地改：Submission.Properties 可能来自上游快照，原地修改会把
// 同一张 map 共享给调用方（并发下变成数据竞争）。
func mergePrompt(extra map[string]any, prompt string) map[string]any {
	merged := make(map[string]any, len(extra)+1)
	for key, value := range extra {
		merged[key] = value
	}
	if prompt != "" {
		merged[PropertyPrompt] = prompt
	}
	return merged
}

// mergeData 浅拷贝调用方 data。
//
// 不在这里写任何占位键：code/description 由 TaskRow 按 Submission 的
// 显式字段写入，upload_urls 由 UploadInlineTask 写入。共享 map 会被调用方
// 后续修改（并发下是数据竞争），因此必须复制。
func mergeData(extra map[string]any) map[string]any {
	merged := make(map[string]any, len(extra))
	for key, value := range extra {
		merged[key] = value
	}
	return merged
}

// privateData 组装结算锚点；空值不落键（与 tasks.Repo 的 {} 默认一致）。
func privateData(requestID string, tokenID int64) map[string]any {
	private := make(map[string]any, 2)
	if requestID != "" {
		private[PrivateRequestID] = requestID
	}
	if tokenID != 0 {
		private[PrivateTokenID] = tokenID
	}
	return private
}

// clampPercent 把进度钳制到 0–100（tasks.progress_percent 的 CHECK 边界）。
func clampPercent(percent int) int {
	if percent < 0 {
		return 0
	}
	if percent > 100 {
		return 100
	}
	return percent
}
