// Package tasks 承载任务插件平台的数据面：持久化模型、状态机、提交执行器
// 与产物投影（relay-06 §6/§7/§10.2/§10.5）。
//
// 依赖方向：本包只依赖 contract / plugin / 标准库。HTTP 形状、认证、计费
// 会话装配都在 httpapi；反向依赖会形成环，也会把「凭据解密」带进数据面。
package tasks

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"time"

	"github.com/google/uuid"
)

// State 是 tasks.state 的持久化字面量（契约 §2.5 TaskState 7 态）。
//
// 用字符串而不是 int：DB 列是 text 且带 CHECK，枚举对账（store.EnumChecks）
// 与 SQL 诊断都直接可读；int 枚举需要在每层做一次映射，反而多一个漂移点。
type State string

const (
	StateNotStart   State = "not_start"
	StateSubmitted  State = "submitted"
	StateQueued     State = "queued"
	StateInProgress State = "in_progress"
	StateSuccess    State = "success"
	StateFailure    State = "failure"
	StateUnknown    State = "unknown"
)

// Terminal 报告是否为终态。终态恰好一次结算由 T6b 的 CAS 推进保证；
// 本函数只用于提交阶段的「即时终态」判定。
func (s State) Terminal() bool {
	return s == StateSuccess || s == StateFailure
}

// Valid 报告取值是否属于冻结的 7 态集合。
func (s State) Valid() bool {
	switch s {
	case StateNotStart, StateSubmitted, StateQueued,
		StateInProgress, StateSuccess, StateFailure, StateUnknown:
		return true
	}
	return false
}

// ParseState 把插件返回值 / 上游原始串映射到 7 态。
//
// 映射表刻意保守：只接受契约与设计逐字列出的词汇（relay-06 §3.7），
// 其余一律 unknown —— 设计明文要求「UNKNOWN 必须原样返回，不得默认成
// IN_PROGRESS」，把无法识别的串猜成进行中会让后台轮询无限重试。
// 少量同义串（RUNNING/SUCCEEDED/FAILED 等）来自厂商 API 的常见拼写，
// 映射到最近似语义；它们不是新的持久化值。
func ParseState(raw string) State {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "NOT_START", "NOTSTART", "PENDING":
		return StateNotStart
	case "SUBMITTED":
		return StateSubmitted
	case "QUEUED", "QUEUEING", "QUEUING":
		return StateQueued
	case "IN_PROGRESS", "INPROGRESS", "RUNNING", "PROCESSING":
		return StateInProgress
	case "SUCCESS", "SUCCEEDED", "COMPLETED", "DONE":
		return StateSuccess
	case "FAILURE", "FAILED", "ERROR", "CANCELLED", "CANCELED":
		return StateFailure
	default:
		return StateUnknown
	}
}

// PublicTaskIDPrefix 是冻结的对外任务前缀（relay-06 §7：task_<32 随机>）。
const PublicTaskIDPrefix = "task_"

// NewPublicTaskID 生成 `task_` + 32 位小写 hex（128 位熵）。
//
// 用 crypto/rand 而不是自增 id：public_task_id 会出现在客户端台账与 URL
// 查询参数里，可预测的 ID 会把「换一个 ID 就能猜他人任务」变成在线枚举。
// 唯一性由 tasks_public_task_id_uq 兜底。
func NewPublicTaskID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return PublicTaskIDPrefix + hex.EncodeToString(b[:]), nil
}

// Task 是 tasks 表的读写投影。JSON tag 只用于只读列表/查询；
// private_data 永远不出现（设计 §7.1：台账 Omit private_data）。
type Task struct {
	ID                int64      `json:"id"`
	PublicTaskID      string     `json:"task_id"`
	UpstreamTaskID    string     `json:"upstream_task_id,omitempty"`
	Family            string     `json:"family"`
	Action            string     `json:"action"`
	PluginKey         string     `json:"plugin_key"`
	PluginGeneration  int64      `json:"plugin_generation"`
	FetchMode         string     `json:"fetch_mode"`
	LockedChannelID   int64      `json:"locked_channel_id"`
	OriginTaskIDs     []string   `json:"origin_task_ids"`
	State             State      `json:"state"`
	RawState          string     `json:"raw_state"`
	ProgressPercent   int        `json:"progress_percent"`
	RetentionDeadline *time.Time `json:"retention_deadline,omitempty"`

	UserID     int64      `json:"user_id,omitempty"`
	Group      string     `json:"group"`
	ChannelID  int64      `json:"channel_id"`
	Quota      int64      `json:"quota"`
	FailReason string     `json:"fail_reason,omitempty"`
	SubmitTime *time.Time `json:"submit_time,omitempty"`
	StartTime  *time.Time `json:"start_time,omitempty"`
	FinishTime *time.Time `json:"finish_time,omitempty"`

	Properties  map[string]any `json:"properties,omitempty"`
	PrivateData map[string]any `json:"-"`
	// Data 只在上游响应快照/产物投影需要时读取；台账列表显式置 nil 以省略。
	Data map[string]any `json:"data,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ListFilter 是台账分页/筛选输入（self 与 admin 共用）。
type ListFilter struct {
	UserID   int64   // 0 = 不按用户过滤（仅 admin）
	States   []State // 空 = 不筛状态
	Page     int
	PageSize int
}

// ---------- 批次（T4 / P-04） ----------

// BatchPublicIDPrefix 是批次对外 id 前缀。DB 主键是 uuid（便于 join/索引），
// 对外 id 加前缀是为了让客户端一眼区分 batch 与 task，映射是纯前缀替换
// （与 resp_/task_ 同一口径），不做任何转义或哈希。
const BatchPublicIDPrefix = "batch_"

// 批次状态字面量（OpenAI Batch 状态机的只读子集）。
//
// 本实现只产出三种：创建即 in_progress、全部子任务终态后 completed（有成功）
// 或 failed（全失败）；validating/finalizing/cancelled 保留给将来（取消端点
// 当前 fail-closed，见 handlers_batches.go）。
const (
	BatchStatusInProgress = "in_progress"
	BatchStatusCompleted  = "completed"
	BatchStatusFailed     = "failed"
)

// Batch 是 task_batches 的读写投影。
//
// Total 是**提交行数**（含上游提交失败的子请求）；Completed/Failed 是读取时
// 由子任务状态重算的聚合值（见 Repo.GetBatchByID 的 why 注释），落库列只是
// 上次观测的快照，供将来后台扫描使用。
type Batch struct {
	ID        string      `json:"id"`
	UserID    int64       `json:"user_id,omitempty"`
	TokenID   int64       `json:"token_id,omitempty"`
	Status    string      `json:"status"`
	Total     int         `json:"total"`
	Completed int         `json:"completed"`
	Failed    int         `json:"failed"`
	CreatedAt time.Time   `json:"created_at"`
	UpdatedAt time.Time   `json:"updated_at"`
	Items     []BatchItem `json:"items,omitempty"`
}

// BatchItem 是批次内一条子任务的最小投影（custom_id 不在冻结表结构里，
// 因此只暴露任务 id 与状态；调用方按创建顺序自行对应输入行）。
type BatchItem struct {
	PublicTaskID    string `json:"task_id"`
	State           State  `json:"state"`
	ProgressPercent int    `json:"progress_percent"`
}

// NewBatchID 生成 uuid v4 批次主键（crypto/rand 由 google/uuid 提供）。
// 用随机 uuid 而不是自增：批次 id 会出现在客户端 URL 里，可预测 id 会变成
// 他人批次的枚举面。
func NewBatchID() (string, error) {
	id, err := uuid.NewRandom()
	if err != nil {
		return "", err
	}
	return id.String(), nil
}

// ParseBatchPublicID 把对外 id（batch_<uuid>）还原为 DB 主键。
//
// 只接受 uuid 形状：把任意路径段直接拼进 uuid 比较会让 PostgreSQL 抛
// invalid input syntax（500），也会把「畸形 id」与「不存在」分成两种响应。
// 校验失败与不存在因此同为 ok=false。
func ParseBatchPublicID(raw string) (string, bool) {
	trimmed := strings.TrimSpace(raw)
	if !strings.HasPrefix(trimmed, BatchPublicIDPrefix) {
		return "", false
	}
	id, err := uuid.Parse(trimmed[len(BatchPublicIDPrefix):])
	if err != nil {
		return "", false
	}
	return id.String(), true
}

// FormatBatchPublicID 把 DB 主键写成对外 id。
func FormatBatchPublicID(id string) string {
	return BatchPublicIDPrefix + id
}

// ArtifactView 是产物清单项（relay-06 §10.5：{key,type,mimeType?}）。
// key 是任务内稳定标识，供 content 端点定位；ContentURL 由宿主在响应装配时
// 注入（能力 URL），ProjectArtifacts 本身不签发 URL——数据投影与授权分离，
// 同一份 task.data 可以被不同基址/密钥的宿主复用。
type ArtifactView struct {
	Key        string `json:"key"`
	Type       string `json:"type"`
	MIMEType   string `json:"mime_type,omitempty"`
	ContentURL string `json:"content_url,omitempty"`
}
