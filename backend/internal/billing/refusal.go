package billing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// 中继前拒绝的阶段词表（冻结为五种）。
//
// 这些拒绝发生在网关调用上游之前（鉴权、请求结构、配额/限流、模型准入），
// 因此没有 UsageFact、没有 Attempts、也没有结算金额。字段值必须与
// admin_info.triage.stage 的读取端（triage.Evidence.RejectStage）逐一对应，
// 不允许把未经校验的自由文本写进日志。
const (
	RefusalStageQuota            = "quota"
	RefusalStageRateLimit        = "rate_limit"
	RefusalStageAuth             = "auth"
	RefusalStageRequestStructure = "request_structure"
	RefusalStageModelAdmission   = "model_admission"
)

// refusalStages 是上述五种阶段的集合，供入参校验使用。
var refusalStages = map[string]struct{}{
	RefusalStageQuota:            {},
	RefusalStageRateLimit:        {},
	RefusalStageAuth:             {},
	RefusalStageRequestStructure: {},
	RefusalStageModelAdmission:   {},
}

// validRefusalStage 报告 stage 是否属于冻结词表。
func validRefusalStage(stage string) bool {
	_, ok := refusalStages[stage]
	return ok
}

// refusalStructures 是 Structure 的冻结取值："" 表示「尚未校验」，
// valid / invalid 是真实解析结论。与 triage.Evidence.RequestStructure 及
// OpenAPI 的 TriageEvidence.request_structure 枚举逐一对应。
var refusalStructures = map[string]struct{}{
	"":        {},
	"valid":   {},
	"invalid": {},
}

// validRefusalStructure 报告 structure 是否属于冻结词表。
func validRefusalStructure(structure string) bool {
	_, ok := refusalStructures[structure]
	return ok
}

// Refusal 是一条「未到达上游即被拒绝」的日志投影。
//
// 与 usageLogRow 的区别：这里没有用量事实（token 数/延迟/流式都不存在），
// 金额一律写 0，retry_chain 写 NULL。唯一可写的排障证据是
// other.admin_info.triage 里的 stage / endpoint / request_structure。
//
// 字段类型与鉴权/落库层保持一致：UserID 为 int64（auth.Principal.UserID、
// usageLogRow.userID），TokenID 为 *int64（auth.Principal.TokenID），nil 表示
// 会话路径没有令牌，必须与「令牌 0」区分开。
type Refusal struct {
	RequestID    string // 必须是合法 UUID（32 位十六进制或带连字符两种拼写都接受）
	UserID       int64  // 与 auth.Principal.UserID、usageLogRow.userID 一致
	TokenID      *int64 // 与 auth.Principal.TokenID 一致；nil 写 NULL（会话路径没有令牌）
	Model        string // 解析失败时为空字符串，不得因此报错
	Endpoint     string // contract.EndpointLabel(rc.RequestPath) 的返回值
	Stage        string // quota | rate_limit | auth | request_structure | model_admission
	ErrorClass   string // usage_logs.error_class 的既有词表
	ErrorMessage string // 固定文案，绝不含上游 URL / 凭据 / 响应体
	Structure    string // "valid" | "invalid" | "" —— 写入 admin_info.triage.request_structure
}

// RecordRefusal 写入一条 status=refused 的 usage_logs 行。
//
// 本方法是 Task 3b 的落库接缝：调用方在返回拒绝响应之前调用，任何错误都只
// 记录日志、继续返回原始 HTTP 响应（拒绝行的缺失不应改变对用户的答复）。
//
// 【幂等】与 Saturate 共用 request_id 作用域的事务级咨询锁：先加锁再查重，
// 只要该 request_id 已有任意一行（不论状态），直接返回 nil 而不插入。这保证
// 同一请求的重复拒绝、或拒绝后迟到的结算写入，都不会产生第二行。
// 拒绝行本身是「可被收敛」的起点：后续 worker 的 refunded 写入仍可把它
// 单向收敛为 refunded（见 writeUsageLog 的既有收敛分支）。
func (l *DBLogger) RecordRefusal(ctx context.Context, in Refusal) error {
	if strings.TrimSpace(in.RequestID) == "" {
		return errors.New("billing: refusal requires a request id")
	}
	if in.Stage == "" {
		return errors.New("billing: refusal requires a stage")
	}
	// 在落库前校验冻结词表：未经校验的 stage 会静默进入
	// admin_info.triage，污染 triage.Evidence.RejectStage 的归因。
	if !validRefusalStage(in.Stage) {
		return fmt.Errorf("billing: unknown refusal stage %q", in.Stage)
	}
	// 同理校验 Structure：写错的取值会静默进入 admin_info.triage，
	// 读取端只会把它当成「未校验」，管理员看到的排除依据因此消失。
	if !validRefusalStructure(in.Structure) {
		return fmt.Errorf("billing: unknown refusal structure %q", in.Structure)
	}
	// 调用方把任何错误当作「日志失败但继续服务」，因此这里绝不 panic：
	// 未装配日志库时返回错误而不是解引用 nil。
	if l == nil || l.logsDB == nil {
		return errors.New("billing: DBLogger has no logs database")
	}

	row, err := buildRefusalRow(in)
	if err != nil {
		return err
	}

	tx, err := l.logsDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin refusal usage log tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 与 writeUsageLog 共用同一把锁与同一套 UUID 规范化，避免拒绝行与结算行
	// 并发时各写一行（usage_logs.request_id 没有唯一约束，R-25）。
	if err := lockUsageLogRequestID(ctx, tx, row.requestID); err != nil {
		return err
	}
	existing, err := existingUsageLogStatuses(ctx, tx, row.requestID)
	if err != nil {
		return err
	}
	if len(existing) > 0 {
		// 任意状态即视为已记录：拒绝行不覆盖、不收敛既有行。
		return tx.Commit()
	}

	if _, err := tx.ExecContext(ctx, usageLogInsertSQL, usageLogInsertArgs(row)...); err != nil {
		return fmt.Errorf("insert refusal usage log: %w", err)
	}
	return tx.Commit()
}

// buildRefusalRow 把 Refusal 投影为 usage_logs 行。
//
// 金额字段全部为零值（拒绝发生在计费之前），stream 为 false，
// retry_chain 保持 sql.NullString 零值（写 NULL：从未到达上游，不得伪造
// 一条空链路）。model 允许为空；其余自由文本按 varchar 列宽截断。
func buildRefusalRow(in Refusal) (usageLogRow, error) {
	other, err := buildRefusalOther(in)
	if err != nil {
		return usageLogRow{}, err
	}
	row := usageLogRow{
		requestID:    strings.TrimSpace(in.RequestID),
		userID:       in.UserID,
		model:        truncateLogText(in.Model, 128),
		status:       UsageLogStatusRefused,
		errorClass:   nullString(truncateLogText(in.ErrorClass, 255)),
		errorMessage: nullString(truncateLogText(in.ErrorMessage, 500)),
		other:        other,
	}
	// nil 表示没有令牌；非 nil（包括指向 0）原样写值，保留「令牌 0」与
	// 「无令牌」的区分。不能复用 nullInt64——它会把 0 折叠成 NULL。
	if in.TokenID != nil {
		row.tokenID = sql.NullInt64{Int64: *in.TokenID, Valid: true}
	}
	return row, nil
}

// buildRefusalOther 构造拒绝行的 other JSON。
//
// 只写 admin_info.triage（管理员可见分区，普通用户视图整体剥离）：
// {"stage": ..., "endpoint": ...} 固定存在，request_structure 在空串时省略
// （"" 表示尚未校验，省略键与 buildTriageInfo 里显式写空串的既有口径不同：
// 这里的省略是任务契约的一部分）。所有值经过 truncateLogText 清洗 NUL/非法
// UTF-8，任何输入都不会让 jsonb 插入失败（SQLSTATE 22P05）。
func buildRefusalOther(in Refusal) (string, error) {
	triage := make(map[string]any, 3)
	triage["stage"] = truncateLogText(in.Stage, triageLabelMaxRunes)
	triage["endpoint"] = truncateLogText(in.Endpoint, triageLabelMaxRunes)
	if in.Structure != "" {
		triage["request_structure"] = truncateLogText(in.Structure, triageLabelMaxRunes)
	}
	encoded, err := json.Marshal(map[string]any{
		"admin_info": map[string]any{"triage": triage},
	})
	if err != nil {
		// 只含字符串与嵌套 map，Marshal 实际不会失败；保留错误分支避免把
		// 半成品写进日志。
		return "", fmt.Errorf("billing: marshal refusal usage log other: %w", err)
	}
	return string(encoded), nil
}
