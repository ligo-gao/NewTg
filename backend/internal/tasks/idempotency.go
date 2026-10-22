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

import (
	"bytes"
	"context"
	"database/sql"
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
// 典型原因是执行超过租约后被接管；此时绝不能覆写接管者的结果。
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

// ClaimSubmitIdempotency 尝试为 (user_id, key) 建立或读取去重记录。
//
// 返回 Claimed 时调用方必须在成功/失败后分别调用 Complete/Abandon；返回
// Replay 时直接回放 Record 并结束；ConflictBody/InFlight 由 HTTP 层映射 409。
func (r *Repo) ClaimSubmitIdempotency(ctx context.Context, c SubmitIdempotencyClaim) (SubmitIdempotencyState, *SubmitIdempotencyRecord, error) {
	if r == nil || r.db == nil {
		return 0, nil, errors.New("tasks: repository is not configured")
	}
	if c.UserID <= 0 {
		return 0, nil, errors.New("tasks: idempotency claim requires user id")
	}
	if c.Key == "" || len(c.Key) > 128 {
		return 0, nil, fmt.Errorf("tasks: idempotency key length %d", len(c.Key))
	}
	if len(c.RequestHash) != 32 {
		return 0, nil, fmt.Errorf("tasks: idempotency request hash length %d，want 32", len(c.RequestHash))
	}
	now := c.Now
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	expires := now.Add(SubmitIdempotencyWindow)

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("tasks: begin idempotency claim: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `
		INSERT INTO task_submit_idempotency
			(user_id, idem_key, request_hash, endpoint, status, claimed_at, created_at, expires_at)
		VALUES ($1,$2,$3,$4,'in_flight',$5,$5,$6)
		ON CONFLICT (user_id, idem_key) DO NOTHING`,
		c.UserID, c.Key, c.RequestHash, c.Endpoint, now, expires)
	if err != nil {
		return 0, nil, fmt.Errorf("tasks: insert idempotency claim: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return 0, nil, fmt.Errorf("tasks: idempotency claim rows: %w", err)
	} else if n == 1 {
		if err := tx.Commit(); err != nil {
			return 0, nil, fmt.Errorf("tasks: commit idempotency claim: %w", err)
		}
		return SubmitIdempotencyClaimed, nil, nil
	}

	// 已有行：锁住它再判定，避免两个并发请求同时看到「过期/in_flight」后
	// 都尝试接管（FOR UPDATE 让第二个等待第一个的更新结果）。
	var (
		hash      []byte
		status    string
		publicID  sql.NullString
		response  []byte
		claimedAt time.Time
		expiresAt time.Time
	)
	err = tx.QueryRowContext(ctx, `
		SELECT request_hash, status, public_task_id, response_body, claimed_at, expires_at
		FROM task_submit_idempotency
		WHERE user_id = $1 AND idem_key = $2
		FOR UPDATE`, c.UserID, c.Key).
		Scan(&hash, &status, &publicID, &response, &claimedAt, &expiresAt)
	if err != nil {
		return 0, nil, fmt.Errorf("tasks: read idempotency claim: %w", err)
	}

	// 过期：整行覆写为新 claim（保留行，窗口从本次重新计算）。
	if !expiresAt.After(now) {
		if err := takeoverIdempotency(ctx, tx, c, now, expires); err != nil {
			return 0, nil, err
		}
		if err := tx.Commit(); err != nil {
			return 0, nil, fmt.Errorf("tasks: commit idempotency takeover: %w", err)
		}
		return SubmitIdempotencyClaimed, nil, nil
	}
	if !bytes.Equal(hash, c.RequestHash) {
		return SubmitIdempotencyConflictBody, nil, nil
	}
	switch status {
	case "completed":
		record := &SubmitIdempotencyRecord{
			PublicTaskID: publicID.String,
			ResponseBody: append([]byte(nil), response...),
		}
		return SubmitIdempotencyReplay, record, nil
	case "in_flight":
		if now.Sub(claimedAt.UTC()) >= SubmitIdempotencyInflightTTL {
			// 执行者崩溃（租约过期）→ 接管；上游可能已受理，但失去幂等性
			// 比让客户端在 24h 内永远拿不到结果更糟，且新提交仍受 code 21
			// 同用户复用保护。
			if err := takeoverIdempotency(ctx, tx, c, now, expires); err != nil {
				return 0, nil, err
			}
			if err := tx.Commit(); err != nil {
				return 0, nil, fmt.Errorf("tasks: commit idempotency takeover: %w", err)
			}
			return SubmitIdempotencyClaimed, nil, nil
		}
		return SubmitIdempotencyInFlight, nil, nil
	default:
		return 0, nil, fmt.Errorf("tasks: idempotency row has unknown status %q", status)
	}
}

func takeoverIdempotency(ctx context.Context, tx *sql.Tx, c SubmitIdempotencyClaim,
	now, expires time.Time) error {

	if _, err := tx.ExecContext(ctx, `
		UPDATE task_submit_idempotency
		SET request_hash = $3, endpoint = $4, status = 'in_flight',
		    public_task_id = NULL, response_body = NULL,
		    claimed_at = $5, created_at = $5, expires_at = $6
		WHERE user_id = $1 AND idem_key = $2`,
		c.UserID, c.Key, c.RequestHash, c.Endpoint, now, expires); err != nil {
		return fmt.Errorf("tasks: takeover idempotency claim: %w", err)
	}
	return nil
}

// CompleteSubmitIdempotency 记录首次结果，使后续同键重放直接命中。
//
// 必须匹配 request_hash：若执行超时被接管，旧执行者不得覆写新执行者的
// 结果（返回 ErrIdempotencyClaimLost，由调用方只记日志）。
func (r *Repo) CompleteSubmitIdempotency(ctx context.Context, userID int64, key string,
	requestHash []byte, publicTaskID string, response []byte) error {

	if r == nil || r.db == nil {
		return errors.New("tasks: repository is not configured")
	}
	if len(response) == 0 || !json.Valid(response) {
		return errors.New("tasks: idempotency response must be non-empty valid JSON")
	}
	res, err := r.db.ExecContext(ctx, `
		UPDATE task_submit_idempotency
		SET status = 'completed', public_task_id = $4, response_body = $5
		WHERE user_id = $1 AND idem_key = $2 AND request_hash = $3 AND status = 'in_flight'`,
		userID, key, requestHash, publicTaskID, response)
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
// （hash + in_flight），不碰接管者/已完成行。
func (r *Repo) AbandonSubmitIdempotency(ctx context.Context, userID int64, key string, requestHash []byte) error {
	if r == nil || r.db == nil {
		return errors.New("tasks: repository is not configured")
	}
	if _, err := r.db.ExecContext(ctx, `
		DELETE FROM task_submit_idempotency
		WHERE user_id = $1 AND idem_key = $2 AND request_hash = $3 AND status = 'in_flight'`,
		userID, key, requestHash); err != nil {
		return fmt.Errorf("tasks: abandon idempotency claim: %w", err)
	}
	return nil
}
