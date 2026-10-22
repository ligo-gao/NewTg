package tasks

// 任务提交幂等键的 SQL 仓储（W8/MJ-d，R-MJ-06 第二步）。
//
// 语义（relay-00 §5.4 + relay-05 §6）：
//   - 客户端可选的 Idempotency-Key 以 (user_id, key) 唯一；
//   - 请求哈希覆盖「端点 + 原始请求体」：同键不同体是客户端错误（409），
//     同键同体重放返回**首次结果**（不重复提交上游、不重复计费）；
//   - 窗口 24h；过期后同键视为新请求，允许整体覆写这一行；
//   - 与计费幂等键 request_id 严格分离：本表从不参与资金幂等，只做提交去重。
//
// 【为什么用 claim 行而不是「成功后再插入」】没有预占，两个并发的同键请求
// 会各自通过查重、各自提交上游、各自预扣——事后再插入唯一键只能报冲突，
// 两条真实任务与两笔资金已经发生。唯一约束必须命中在副作用之前。
//
// 【claim_token 的用途（复核 I2）】租约接管后，新旧执行者的 request_hash
// 可能完全相同；只按 (user,key,hash,in_flight) 收尾会让旧执行者覆写/删除
// 接管者的 claim。每次 claim（含接管）生成并轮换独立 token，Complete/
// Abandon 必须同时匹配 token——同体接管后旧执行者的收尾一律失败。
//
// 【清理】expires_at 只为「过期后可覆写」与将来的清理作业服务；本任务
// 不启动清理 worker（登记并入 W10 清理批次）。未被再次使用的键会长期保留
// 一行，24h 窗口由读取判定而非物理删除。

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const (
	// SubmitIdempotencyWindow 是幂等键的记录窗口（relay-05 §6 冻结 24h）。
	SubmitIdempotencyWindow = 24 * time.Hour

	// SubmitIdempotencyInflightTTL 是「执行中」claim 的租约。
	//
	// 必须大于一次提交的最坏同步耗时（提交 60s + 落库结算 10s，relay-05
	// §6.1），否则一个仍在执行的上游提交会被第二个同键请求抢走，造成重复
	// 建任务。租约过期的唯一原因是执行者崩溃，此时允许接管比永久 409 更
	// 安全（崩溃窗口内的上游请求最多重复一次，但客户端不会拿到互相矛盾的
	// 两个 in-flight 错误直到 24h 窗口结束）。
	SubmitIdempotencyInflightTTL = 2 * time.Minute

	// MaxSubmitIdempotencyKey 是幂等键的字节上界。
	//
	// 必须与 HTTP 冻结常量 mj.MaxIdempotencyKey（1KiB）一致——两套上界会让
	// 129–1024 字节的合法键在 claim 时变成 500（复核 I1）。mj 包测试断言
	// 两者相等，防止单边漂移。
	MaxSubmitIdempotencyKey = 1 << 10
)

// SubmitIdempotencyState 是 Claim 的结果分类。
type SubmitIdempotencyState int

const (
	// SubmitIdempotencyClaimed：本次请求获得执行权（新键、过期键或崩溃接管）。
	SubmitIdempotencyClaimed SubmitIdempotencyState = iota
	// SubmitIdempotencyReplay：同键同体的已完成请求，返回首次结果。
	SubmitIdempotencyReplay
	// SubmitIdempotencyConflictBody：同键但请求体/端点不同 → 409。
	SubmitIdempotencyConflictBody
	// SubmitIdempotencyInFlight：同键同体的另一个执行者仍在运行 → 409。
	SubmitIdempotencyInFlight
)

// ErrIdempotencyClaimLost 表示完成/放弃时本执行者已不再持有该键。
//
// 典型原因是执行超过租约后被接管；此时绝不能覆写/删除接管者的结果。
var ErrIdempotencyClaimLost = errors.New("tasks: idempotency claim lost")

// SubmitIdempotencyClaim 是一次 claim 的输入。
type SubmitIdempotencyClaim struct {
	UserID      int64
	Key         string
	RequestHash []byte
	Endpoint    string
	// Now 是测试接缝；零值 → time.Now。
	Now time.Time
}

// SubmitIdempotencyRecord 是重放所需的最小记录。
type SubmitIdempotencyRecord struct {
	PublicTaskID string
	// ResponseBody 是首次响应的完整 {code,description,result} JSON。
	ResponseBody []byte
}

// SubmitIdempotencyRow 是判定所需的既有行投影（纯逻辑输入）。
type SubmitIdempotencyRow struct {
	RequestHash []byte
	Status      string
	ClaimedAt   time.Time
	ExpiresAt   time.Time
}

// EvaluateSubmitIdempotency 是 claim 判定的**唯一实现**。
//
// 仓储与 HTTP 单测的 fake 都调用它：判定规则（哈希优先于执行中、过期/
// 租约接管）只有一份，任一单边实现都不会掩盖「忽略请求哈希」这类变异
// （复核 M4）。返回 takeover=true 表示调用方应轮换 claim_token 并覆写行。
func EvaluateSubmitIdempotency(row *SubmitIdempotencyRow, requestHash []byte,
	now time.Time) (state SubmitIdempotencyState, takeover bool, err error) {

	if row == nil {
		return SubmitIdempotencyClaimed, false, nil
	}
	if !row.ExpiresAt.After(now) {
		return SubmitIdempotencyClaimed, true, nil
	}
	if !bytes.Equal(row.RequestHash, requestHash) {
		return SubmitIdempotencyConflictBody, false, nil
	}
	switch row.Status {
	case "completed":
		return SubmitIdempotencyReplay, false, nil
	case "in_flight":
		if now.Sub(row.ClaimedAt.UTC()) >= SubmitIdempotencyInflightTTL {
			return SubmitIdempotencyClaimed, true, nil
		}
		return SubmitIdempotencyInFlight, false, nil
	default:
		return 0, false, fmt.Errorf("tasks: idempotency row has unknown status %q", row.Status)
	}
}

// newIdempotencyClaimToken 生成 32 个十六进制字符的 claim token。
func newIdempotencyClaimToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("tasks: generate idempotency claim token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// ClaimSubmitIdempotency 尝试为 (user_id, key) 建立或读取去重记录。
//
// 返回 Claimed 时附带 claimToken，调用方必须用同一 token 调用
// Complete/Abandon；返回 Replay 时直接回放 Record 并结束；
// ConflictBody/InFlight 由 HTTP 层映射 409。
func (r *Repo) ClaimSubmitIdempotency(ctx context.Context, c SubmitIdempotencyClaim) (SubmitIdempotencyState, *SubmitIdempotencyRecord, string, error) {
	if r == nil || r.db == nil {
		return 0, nil, "", errors.New("tasks: repository is not configured")
	}
	if c.UserID <= 0 {
		return 0, nil, "", errors.New("tasks: idempotency claim requires user id")
	}
	if c.Key == "" || len(c.Key) > MaxSubmitIdempotencyKey {
		return 0, nil, "", fmt.Errorf("tasks: idempotency key length %d (1–%d)",
			len(c.Key), MaxSubmitIdempotencyKey)
	}
	if len(c.RequestHash) != 32 {
		return 0, nil, "", fmt.Errorf("tasks: idempotency request hash length %d，want 32", len(c.RequestHash))
	}
	now := c.Now
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	expires := now.Add(SubmitIdempotencyWindow)

	token, err := newIdempotencyClaimToken()
	if err != nil {
		return 0, nil, "", err
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, nil, "", fmt.Errorf("tasks: begin idempotency claim: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `
		INSERT INTO task_submit_idempotency
			(user_id, idem_key, request_hash, endpoint, status, claim_token, claimed_at, created_at, expires_at)
		VALUES ($1,$2,$3,$4,'in_flight',$5,$6,$6,$7)
		ON CONFLICT (user_id, idem_key) DO NOTHING`,
		c.UserID, c.Key, c.RequestHash, c.Endpoint, token, now, expires)
	if err != nil {
		return 0, nil, "", fmt.Errorf("tasks: insert idempotency claim: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return 0, nil, "", fmt.Errorf("tasks: idempotency claim rows: %w", err)
	} else if n == 1 {
		if err := tx.Commit(); err != nil {
			return 0, nil, "", fmt.Errorf("tasks: commit idempotency claim: %w", err)
		}
		return SubmitIdempotencyClaimed, nil, token, nil
	}

	// 已有行：锁住它再判定，避免两个并发请求同时看到「过期/in_flight」后
	// 都尝试接管（FOR UPDATE 让第二个等待第一个的更新结果）。
	var (
		row      SubmitIdempotencyRow
		publicID sql.NullString
		response []byte
	)
	err = tx.QueryRowContext(ctx, `
		SELECT request_hash, status, public_task_id, response_body, claimed_at, expires_at
		FROM task_submit_idempotency
		WHERE user_id = $1 AND idem_key = $2
		FOR UPDATE`, c.UserID, c.Key).
		Scan(&row.RequestHash, &row.Status, &publicID, &response, &row.ClaimedAt, &row.ExpiresAt)
	if err != nil {
		return 0, nil, "", fmt.Errorf("tasks: read idempotency claim: %w", err)
	}

	state, takeover, err := EvaluateSubmitIdempotency(&row, c.RequestHash, now)
	if err != nil {
		return 0, nil, "", err
	}
	if takeover {
		if err := takeoverIdempotency(ctx, tx, c, token, now, expires); err != nil {
			return 0, nil, "", err
		}
		if err := tx.Commit(); err != nil {
			return 0, nil, "", fmt.Errorf("tasks: commit idempotency takeover: %w", err)
		}
		return SubmitIdempotencyClaimed, nil, token, nil
	}
	if state == SubmitIdempotencyReplay {
		record := &SubmitIdempotencyRecord{
			PublicTaskID: publicID.String,
			ResponseBody: append([]byte(nil), response...),
		}
		return SubmitIdempotencyReplay, record, "", nil
	}
	return state, nil, "", nil
}

func takeoverIdempotency(ctx context.Context, tx *sql.Tx, c SubmitIdempotencyClaim,
	token string, now, expires time.Time) error {

	if _, err := tx.ExecContext(ctx, `
		UPDATE task_submit_idempotency
		SET request_hash = $3, endpoint = $4, status = 'in_flight',
		    public_task_id = NULL, response_body = NULL, claim_token = $5,
		    claimed_at = $6, created_at = $6, expires_at = $7
		WHERE user_id = $1 AND idem_key = $2`,
		c.UserID, c.Key, c.RequestHash, c.Endpoint, token, now, expires); err != nil {
		return fmt.Errorf("tasks: takeover idempotency claim: %w", err)
	}
	return nil
}

// CompleteSubmitIdempotency 记录首次结果，使后续同键重放直接命中。
//
// 必须匹配 request_hash 与 claim_token：若执行超时被接管，旧执行者不得
// 覆写新执行者的结果（返回 ErrIdempotencyClaimLost，由调用方只记日志）。
func (r *Repo) CompleteSubmitIdempotency(ctx context.Context, userID int64, key string,
	requestHash []byte, claimToken, publicTaskID string, response []byte) error {

	if r == nil || r.db == nil {
		return errors.New("tasks: repository is not configured")
	}
	if claimToken == "" {
		return errors.New("tasks: idempotency complete requires claim token")
	}
	if len(response) == 0 || !json.Valid(response) {
		return errors.New("tasks: idempotency response must be non-empty valid JSON")
	}
	res, err := r.db.ExecContext(ctx, `
		UPDATE task_submit_idempotency
		SET status = 'completed', public_task_id = $5, response_body = $6
		WHERE user_id = $1 AND idem_key = $2 AND request_hash = $3
		  AND claim_token = $4 AND status = 'in_flight'`,
		userID, key, requestHash, claimToken, publicTaskID, response)
	if err != nil {
		return fmt.Errorf("tasks: complete idempotency claim: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("tasks: complete idempotency rows: %w", err)
	}
	if n == 0 {
		return ErrIdempotencyClaimLost
	}
	return nil
}

// AbandonSubmitIdempotency 在提交失败时不留下「执行中」占位。
//
// 失败不产生任务/资金，同键重试必须能重新尝试；保留占位会让客户端在租约
// 内收到 409 in_progress，掩盖「其实什么都没发生」。只删除本执行者的行
// （hash + token + in_flight），不碰接管者/已完成行。
func (r *Repo) AbandonSubmitIdempotency(ctx context.Context, userID int64, key string,
	requestHash []byte, claimToken string) error {

	if r == nil || r.db == nil {
		return errors.New("tasks: repository is not configured")
	}
	if claimToken == "" {
		return errors.New("tasks: idempotency abandon requires claim token")
	}
	if _, err := r.db.ExecContext(ctx, `
		DELETE FROM task_submit_idempotency
		WHERE user_id = $1 AND idem_key = $2 AND request_hash = $3
		  AND claim_token = $4 AND status = 'in_flight'`,
		userID, key, requestHash, claimToken); err != nil {
		return fmt.Errorf("tasks: abandon idempotency claim: %w", err)
	}
	return nil
}
