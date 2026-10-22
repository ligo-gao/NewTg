//go:build integration

package tasks

// W8/MJ-d 提交幂等键表的真实 PostgreSQL 验收（R-MJ-06 第二步）。
//
// 覆盖：
//   - 首次 claim → 执行权；并发同键只有一个执行者；
//   - 同键同体 Complete 后重放返回首次结果（含 upload 的数组形 JSON）；
//   - 同键不同体 → 冲突；跨用户同键互不影响；
//   - 失败 Abandon 后同键可重试；24h 过期与崩溃租约接管；
//   - claim_token 归属：同体接管后旧执行者的 Complete/Abandon 均失败（I2）；
//   - 键上界：128/129/200/1024 字节均可写入，1025 被仓储拒绝（I1）。
//
// 运行：NEWTG_TEST_DB_DSN=postgres://... go test -tags=integration ./internal/tasks/

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func idemHash(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

func newIdemFixture(t *testing.T) (*Repo, context.Context, string) {
	t.Helper()
	db := openTaskTestDB(t)
	repo := NewRepo(db)
	prefix := fmt.Sprintf("mjd-idem-%d-", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM task_submit_idempotency WHERE idem_key LIKE $1`, prefix+"%")
	})
	return repo, context.Background(), prefix
}

func claimIdem(t *testing.T, repo *Repo, ctx context.Context, userID int64,
	key, body string, now time.Time) (SubmitIdempotencyState, *SubmitIdempotencyRecord, string) {

	t.Helper()
	state, rec, token, err := repo.ClaimSubmitIdempotency(ctx, SubmitIdempotencyClaim{
		UserID: userID, Key: key, RequestHash: idemHash(body),
		Endpoint: "submit/imagine", Now: now,
	})
	if err != nil {
		t.Fatalf("ClaimSubmitIdempotency(%s): %v", key, err)
	}
	return state, rec, token
}

func TestSubmitIdempotencyClaimReplayAndConflict(t *testing.T) {
	repo, ctx, prefix := newIdemFixture(t)
	userID := int64(910001)
	key := prefix + "k1"
	now := time.Now().UTC()

	state, rec, token := claimIdem(t, repo, ctx, userID, key, `{"prompt":"cat"}`, now)
	if state != SubmitIdempotencyClaimed || rec != nil || token == "" {
		t.Fatalf("首次 claim = (%v,%v,token=%q)，want Claimed/nil/非空 token", state, rec, token)
	}
	// 同键同体、首次仍在执行 → InFlight（不重复提交上游）。
	if state, _, _ := claimIdem(t, repo, ctx, userID, key, `{"prompt":"cat"}`, now); state != SubmitIdempotencyInFlight {
		t.Fatalf("执行中重放 = %v，want InFlight", state)
	}
	// 同键不同体 → 冲突（必须先于 InFlight 判定）。
	if state, _, _ := claimIdem(t, repo, ctx, userID, key, `{"prompt":"dog"}`, now); state != SubmitIdempotencyConflictBody {
		t.Fatalf("不同体重放 = %v，want ConflictBody", state)
	}
	// 跨用户同键是独立记录。
	if state, _, _ := claimIdem(t, repo, ctx, userID+1, key, `{"prompt":"dog"}`, now); state != SubmitIdempotencyClaimed {
		t.Fatalf("跨用户同键 = %v，want Claimed", state)
	}

	response := []byte(`{"code":1,"description":"Submit success","result":["https://cdn/x.png"]}`)
	if err := repo.CompleteSubmitIdempotency(ctx, userID, key, idemHash(`{"prompt":"cat"}`),
		token, "task_public_1", response); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	state, rec, _ = claimIdem(t, repo, ctx, userID, key, `{"prompt":"cat"}`, now)
	if state != SubmitIdempotencyReplay || rec == nil {
		t.Fatalf("完成后的重放 = (%v,%v)，want Replay+record", state, rec)
	}
	if rec.PublicTaskID != "task_public_1" || string(rec.ResponseBody) != string(response) {
		t.Fatalf("replay record = %+v，want public id/首次响应原样", rec)
	}
	// 已完成行不再被当作 in_flight；不同体仍然冲突。
	if state, _, _ := claimIdem(t, repo, ctx, userID, key, `{"prompt":"dog"}`, now); state != SubmitIdempotencyConflictBody {
		t.Fatalf("已完成行不同体 = %v，want ConflictBody", state)
	}
}

func TestSubmitIdempotencyAbandonAllowsRetry(t *testing.T) {
	repo, ctx, prefix := newIdemFixture(t)
	userID := int64(910002)
	key := prefix + "k-abandon"
	now := time.Now().UTC()

	state, _, token := claimIdem(t, repo, ctx, userID, key, `{"prompt":"cat"}`, now)
	if state != SubmitIdempotencyClaimed {
		t.Fatalf("claim = %v，want Claimed", state)
	}
	if err := repo.AbandonSubmitIdempotency(ctx, userID, key, idemHash(`{"prompt":"cat"}`), token); err != nil {
		t.Fatalf("Abandon: %v", err)
	}
	// 失败后同键重试必须能重新拿到执行权（不留下 409 in_progress）。
	if state, _, _ := claimIdem(t, repo, ctx, userID, key, `{"prompt":"cat"}`, now); state != SubmitIdempotencyClaimed {
		t.Fatalf("Abandon 后重试 = %v，want Claimed", state)
	}
}

func TestSubmitIdempotencyWindowAndLeaseTakeover(t *testing.T) {
	repo, ctx, prefix := newIdemFixture(t)
	userID := int64(910003)

	// 24h 过期：旧窗口的完成结果不再用于重放，同键可整体覆写。
	expiredKey := prefix + "k-expired"
	old := time.Now().UTC().Add(-25 * time.Hour)
	state, _, oldToken := claimIdem(t, repo, ctx, userID, expiredKey, `{"prompt":"old"}`, old)
	if state != SubmitIdempotencyClaimed {
		t.Fatalf("old claim = %v", state)
	}
	if err := repo.CompleteSubmitIdempotency(ctx, userID, expiredKey,
		idemHash(`{"prompt":"old"}`), oldToken, "task_old",
		[]byte(`{"code":1,"description":"ok","result":"mj_old"}`)); err != nil {
		t.Fatalf("complete old: %v", err)
	}
	now := time.Now().UTC()
	if state, _, _ := claimIdem(t, repo, ctx, userID, expiredKey, `{"prompt":"new"}`, now); state != SubmitIdempotencyClaimed {
		t.Fatalf("过期键 = %v，want Claimed（窗口已过）", state)
	}

	// 崩溃租约：in_flight 超过 2 分钟可被接管，未超时则拒绝。
	leaseKey := prefix + "k-lease"
	if state, _, _ := claimIdem(t, repo, ctx, userID, leaseKey, `{"prompt":"cat"}`,
		now.Add(-SubmitIdempotencyInflightTTL-time.Second)); state != SubmitIdempotencyClaimed {
		t.Fatalf("lease claim = %v", state)
	}
	if state, _, _ := claimIdem(t, repo, ctx, userID, leaseKey, `{"prompt":"cat"}`, now); state != SubmitIdempotencyClaimed {
		t.Fatalf("过期租约接管 = %v，want Claimed", state)
	}
	if state, _, _ := claimIdem(t, repo, ctx, userID, leaseKey, `{"prompt":"cat"}`, now); state != SubmitIdempotencyInFlight {
		t.Fatalf("新租约内第二次 = %v，want InFlight", state)
	}
}

// TestSubmitIdempotencyTakeoverRevokesOldClaim 是 I2 的回归：
// 同体接管后旧执行者（旧 token）的 Complete 与 Abandon 都必须失败，
// 不能覆写或删除接管者的 claim。
func TestSubmitIdempotencyTakeoverRevokesOldClaim(t *testing.T) {
	repo, ctx, prefix := newIdemFixture(t)
	userID := int64(910006)
	key := prefix + "k-takeover"
	now := time.Now().UTC().UTC()
	body := `{"prompt":"same-body"}`

	// 旧执行者：claim 在 3 分钟前（超过 2 分钟租约）。
	state, _, oldToken := claimIdem(t, repo, ctx, userID, key, body,
		now.Add(-SubmitIdempotencyInflightTTL-time.Minute))
	if state != SubmitIdempotencyClaimed {
		t.Fatalf("old claim = %v", state)
	}
	// 新执行者用同体接管：拿到的 token 必须不同。
	state, _, newToken := claimIdem(t, repo, ctx, userID, key, body, now)
	if state != SubmitIdempotencyClaimed || newToken == "" {
		t.Fatalf("takeover = (%v, token=%q)", state, newToken)
	}
	if newToken == oldToken {
		t.Fatalf("接管没有轮换 claim_token（old=new=%q）", oldToken)
	}
	// 旧执行者 Complete 不得覆写。
	if err := repo.CompleteSubmitIdempotency(ctx, userID, key, idemHash(body),
		oldToken, "task_stale", []byte(`{"code":1,"description":"stale","result":"stale"}`)); err != ErrIdempotencyClaimLost {
		t.Fatalf("旧 token Complete err = %v，want ErrIdempotencyClaimLost", err)
	}
	// 旧执行者 Abandon 不得删除接管者的行。
	if err := repo.AbandonSubmitIdempotency(ctx, userID, key, idemHash(body), oldToken); err != nil {
		t.Fatalf("old Abandon should be a no-op: %v", err)
	}
	if state, _, _ := claimIdem(t, repo, ctx, userID, key, body, now); state != SubmitIdempotencyInFlight {
		t.Fatalf("旧 Abandon 删除了接管者 claim: state=%v", state)
	}
	// 新执行者可以正常收尾。
	if err := repo.CompleteSubmitIdempotency(ctx, userID, key, idemHash(body),
		newToken, "task_new", []byte(`{"code":1,"description":"ok","result":"mj_new"}`)); err != nil {
		t.Fatalf("新 token Complete: %v", err)
	}
}

// TestSubmitIdempotencyKeyBounds 是 I1 的回归：HTTP 冻结 1KiB，DB 必须一致。
func TestSubmitIdempotencyKeyBounds(t *testing.T) {
	repo, ctx, prefix := newIdemFixture(t)
	userID := int64(910007)
	now := time.Now().UTC()

	for _, n := range []int{128, 129, 200, 1024} {
		key := prefix + strings.Repeat("k", n-len(prefix))
		if len(key) != n {
			t.Fatalf("key length = %d，want %d", len(key), n)
		}
		state, _, token := claimIdem(t, repo, ctx, userID, key, `{"prompt":"cat"}`, now)
		if state != SubmitIdempotencyClaimed || token == "" {
			t.Fatalf("%d 字节键 claim = (%v, token=%q)", n, state, token)
		}
	}
	// 超过冻结上界：仓储必须显式拒绝（HTTP 层更早已 400）。
	tooLong := prefix + strings.Repeat("k", MaxSubmitIdempotencyKey)
	if _, _, _, err := repo.ClaimSubmitIdempotency(ctx, SubmitIdempotencyClaim{
		UserID: userID, Key: tooLong, RequestHash: idemHash("x"), Endpoint: "submit/imagine", Now: now,
	}); err == nil {
		t.Fatalf("1025 字节键被仓储接受")
	}
}

func TestSubmitIdempotencyConcurrentClaimsHaveOneWinner(t *testing.T) {
	repo, ctx, prefix := newIdemFixture(t)
	userID := int64(910004)
	key := prefix + "k-race"
	now := time.Now().UTC()

	const goroutines = 8
	start := make(chan struct{})
	states := make(chan SubmitIdempotencyState, goroutines)
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			state, _, _, err := repo.ClaimSubmitIdempotency(ctx, SubmitIdempotencyClaim{
				UserID: userID, Key: key, RequestHash: idemHash(`{"prompt":"cat"}`),
				Endpoint: "submit/imagine", Now: now,
			})
			if err != nil {
				t.Errorf("concurrent claim: %v", err)
				return
			}
			states <- state
		}()
	}
	close(start)
	wg.Wait()
	close(states)

	claimed, inflight := 0, 0
	for state := range states {
		switch state {
		case SubmitIdempotencyClaimed:
			claimed++
		case SubmitIdempotencyInFlight:
			inflight++
		default:
			t.Fatalf("unexpected state %v", state)
		}
	}
	if claimed != 1 || inflight != goroutines-1 {
		t.Fatalf("claimed=%d inflight=%d，want 1/%d（同键只能有一个执行者）",
			claimed, inflight, goroutines-1)
	}
}

func TestSubmitIdempotencyCompleteRejectsLostClaim(t *testing.T) {
	repo, ctx, prefix := newIdemFixture(t)
	userID := int64(910005)
	key := prefix + "k-lost"
	now := time.Now().UTC()
	state, _, token := claimIdem(t, repo, ctx, userID, key, `{"prompt":"first"}`, now)
	if state != SubmitIdempotencyClaimed {
		t.Fatalf("claim = %v", state)
	}
	// 旧执行者的 hash 不再匹配（被接管后不得覆写新结果）。
	err := repo.CompleteSubmitIdempotency(ctx, userID, key, idemHash(`{"prompt":"other"}`),
		token, "task_stale", []byte(`{"code":1,"description":"ok","result":"stale"}`))
	if err != ErrIdempotencyClaimLost {
		t.Fatalf("stale Complete err = %v，want ErrIdempotencyClaimLost", err)
	}
}
