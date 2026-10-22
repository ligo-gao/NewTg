package httpapi

// W8/MJ-d 提交幂等键与渠道在飞限制的单元验收（全内存）。
//
// 真实 PG 上的唯一约束/并发/窗口语义在 tasks/idempotency_integration_test.go；
// 这里验证 HTTP 编排：什么时候 claim、什么时候回放/409、失败后是否释放键、
// 以及限制器拒绝发生在预扣之前。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"newtg/gateway/internal/mj"
	"newtg/gateway/internal/tasks"
)

// fakeIdemRecord 是幂等表行的内存镜像。
type fakeIdemRecord struct {
	hash         []byte
	status       string // in_flight / completed
	publicTaskID string
	response     []byte
	claimedAt    time.Time
	expiresAt    time.Time
}

type fakeMJIdempotency struct {
	mu   sync.Mutex
	rows map[string]*fakeIdemRecord
}

func newFakeMJIdempotency() *fakeMJIdempotency {
	return &fakeMJIdempotency{rows: map[string]*fakeIdemRecord{}}
}

func idemRowKey(userID int64, key string) string {
	return fmt.Sprintf("%d\x00%s", userID, key)
}

func (f *fakeMJIdempotency) seed(userID int64, key string, rec fakeIdemRecord) {
	f.mu.Lock()
	defer f.mu.Unlock()
	copied := rec
	copied.hash = append([]byte(nil), rec.hash...)
	copied.response = append([]byte(nil), rec.response...)
	f.rows[idemRowKey(userID, key)] = &copied
}

func (f *fakeMJIdempotency) ClaimSubmitIdempotency(_ context.Context,
	c tasks.SubmitIdempotencyClaim) (tasks.SubmitIdempotencyState, *tasks.SubmitIdempotencyRecord, error) {

	f.mu.Lock()
	defer f.mu.Unlock()
	now := c.Now
	if now.IsZero() {
		now = time.Now()
	}
	r := f.rows[idemRowKey(c.UserID, c.Key)]
	if r == nil || !r.expiresAt.After(now) {
		f.rows[idemRowKey(c.UserID, c.Key)] = &fakeIdemRecord{
			hash:      append([]byte(nil), c.RequestHash...),
			status:    "in_flight",
			claimedAt: now,
			expiresAt: now.Add(tasks.SubmitIdempotencyWindow),
		}
		return tasks.SubmitIdempotencyClaimed, nil, nil
	}
	if !bytes.Equal(r.hash, c.RequestHash) {
		return tasks.SubmitIdempotencyConflictBody, nil, nil
	}
	switch r.status {
	case "completed":
		return tasks.SubmitIdempotencyReplay, &tasks.SubmitIdempotencyRecord{
			PublicTaskID: r.publicTaskID,
			ResponseBody: append([]byte(nil), r.response...),
		}, nil
	case "in_flight":
		if now.Sub(r.claimedAt) >= tasks.SubmitIdempotencyInflightTTL {
			r.hash = append([]byte(nil), c.RequestHash...)
			r.claimedAt = now
			r.expiresAt = now.Add(tasks.SubmitIdempotencyWindow)
			return tasks.SubmitIdempotencyClaimed, nil, nil
		}
		return tasks.SubmitIdempotencyInFlight, nil, nil
	}
	return 0, nil, nil
}

func (f *fakeMJIdempotency) CompleteSubmitIdempotency(_ context.Context, userID int64,
	key string, hash []byte, publicTaskID string, response []byte) error {

	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.rows[idemRowKey(userID, key)]
	if r == nil || !bytes.Equal(r.hash, hash) || r.status != "in_flight" {
		return tasks.ErrIdempotencyClaimLost
	}
	r.status = "completed"
	r.publicTaskID = publicTaskID
	r.response = append([]byte(nil), response...)
	return nil
}

func (f *fakeMJIdempotency) AbandonSubmitIdempotency(_ context.Context, userID int64,
	key string, hash []byte) error {

	f.mu.Lock()
	defer f.mu.Unlock()
	if r := f.rows[idemRowKey(userID, key)]; r != nil &&
		bytes.Equal(r.hash, hash) && r.status == "in_flight" {
		delete(f.rows, idemRowKey(userID, key))
	}
	return nil
}

func (f *fakeMJIdempotency) completedRecord(userID int64, key string) *fakeIdemRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.rows[idemRowKey(userID, key)]
	if r == nil {
		return nil
	}
	copied := *r
	return &copied
}

// newIdemTestEnv 构造一个带幂等存储的提交环境。
func newIdemTestEnv(t *testing.T, upstreamResponse string) (*mjTestEnv, *fakeMJIdempotency, *mjUpstream) {
	t.Helper()
	store := newFakeMJStore()
	upstream := newMJUpstream(t, upstreamResponse)
	sealer := NewSecretSealer("mj-handler-test-key")
	normal, cand := sealedMJChannel(t, sealer, 10, mj.ChannelTypeMidjourney,
		upstream.server.URL, mjAllModels(), 10)
	channels := &fakeMJChannels{
		channels:   map[int64]*tasks.ChannelTarget{10: normal},
		candidates: []tasks.MJChannelCandidate{cand},
	}
	idem := newFakeMJIdempotency()
	env := newMJTestEnvFull(t, store, channels, mjConfigured(nil), upstream, nil, idem, nil)
	return env, idem, upstream
}

func requestWithIdemKey(t *testing.T, body, key, bearer string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mj/submit/imagine", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	req.Header.Set("Authorization", "Bearer "+bearer)
	return req
}

func TestMJIdempotencyReplaysFirstResult(t *testing.T) {
	env, idem, upstream := newIdemTestEnv(t, `{"code":1,"description":"ok","result":"mj_first"}`)

	first := serveMJ(t, env, requestWithIdemKey(t, `{"prompt":"cat"}`, "same-key", "nkt-mj-user-42"))
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d (body=%s)", first.Code, first.Body.String())
	}
	if calls := upstream.calls(); calls != 1 {
		t.Fatalf("upstream calls = %d，want 1", calls)
	}
	if got := env.recorderCount(); got != 1 {
		t.Fatalf("billing sessions = %d，want 1", got)
	}
	rec := idem.completedRecord(42, "same-key")
	if rec == nil || rec.status != "completed" || rec.publicTaskID == "" {
		t.Fatalf("idempotency row not completed: %+v", rec)
	}

	// 同键同体：原样回放首次结果，不再提交上游、不再建会话。
	replay := serveMJ(t, env, requestWithIdemKey(t, `{"prompt":"cat"}`, "same-key", "nkt-mj-user-42"))
	if replay.Code != http.StatusOK {
		t.Fatalf("replay status = %d (body=%s)", replay.Code, replay.Body.String())
	}
	if replay.Body.String() != first.Body.String() {
		t.Fatalf("replay body = %s，want 首次响应 %s", replay.Body.String(), first.Body.String())
	}
	if calls := upstream.calls(); calls != 1 {
		t.Fatalf("replay 触发了上游: calls = %d", calls)
	}
	if got := env.recorderCount(); got != 1 {
		t.Fatalf("replay 建了新的计费会话: %d", got)
	}

	// 同键不同体：409，且不动上游/资金。
	conflict := serveMJ(t, env, requestWithIdemKey(t, `{"prompt":"dog"}`, "same-key", "nkt-mj-user-42"))
	if conflict.Code != http.StatusConflict {
		t.Fatalf("conflict status = %d，want 409 (body=%s)", conflict.Code, conflict.Body.String())
	}
	body := decodeMJEnvelope(t, conflict)
	if body["description"] != mj.DescriptionIdempotencyKeyReused {
		t.Fatalf("conflict description = %v", body["description"])
	}
	if calls := upstream.calls(); calls != 1 {
		t.Fatalf("conflict 触发了上游: calls = %d", calls)
	}
	if got := env.recorderCount(); got != 1 {
		t.Fatalf("conflict 建了新的计费会话: %d", got)
	}
}

func TestMJIdempotencyFailureReleasesKeyForRetry(t *testing.T) {
	env, idem, upstream := newIdemTestEnv(t, `{"code":24,"description":"sensitive"}`)

	failed := serveMJ(t, env, requestWithIdemKey(t, `{"prompt":"bad"}`, "retry-key", "nkt-mj-user-42"))
	if failed.Code != http.StatusBadRequest {
		t.Fatalf("failed status = %d (body=%s)", failed.Code, failed.Body.String())
	}
	if rec := idem.completedRecord(42, "retry-key"); rec != nil {
		t.Fatalf("失败后仍留下幂等记录: %+v（同键重试会被误判 409）", rec)
	}
	first := env.lastRecorder(t)
	if countCall(first.ReserveCalls(), "refund") != 1 {
		t.Fatalf("failed submit billing calls = %v，want 一次退款", first.ReserveCalls())
	}

	// 同一个键重试（上限已恢复）必须能重新执行，而不是 409。
	upstream.mu.Lock()
	upstream.response = `{"code":1,"description":"ok","result":"mj_retry_ok"}`
	upstream.mu.Unlock()
	retry := serveMJ(t, env, requestWithIdemKey(t, `{"prompt":"bad"}`, "retry-key", "nkt-mj-user-42"))
	if retry.Code != http.StatusOK {
		t.Fatalf("retry status = %d (body=%s)", retry.Code, retry.Body.String())
	}
	if calls := upstream.calls(); calls != 2 {
		t.Fatalf("upstream calls = %d，want 2（失败一次 + 重试一次）", calls)
	}
	if got := env.recorderCount(); got != 2 {
		t.Fatalf("billing sessions = %d，want 2", got)
	}
	body := decodeMJEnvelope(t, retry)
	if body["result"] != "mj_retry_ok" {
		t.Fatalf("retry result = %v", body["result"])
	}
}

func TestMJIdempotencyCrossUserIsolation(t *testing.T) {
	env, _, upstream := newIdemTestEnv(t, `{"code":1,"description":"ok","result":"mj_cross"}`)

	first := serveMJ(t, env, requestWithIdemKey(t, `{"prompt":"cat"}`, "shared-key", "nkt-mj-user-42"))
	if first.Code != http.StatusOK {
		t.Fatalf("user42 status = %d (body=%s)", first.Code, first.Body.String())
	}
	// 用户 43 使用同一键：独立记录，必须真正提交而不是回放 42 的结果。
	req := requestWithIdemKey(t, `{"prompt":"cat"}`, "shared-key", "nkt-mj-user-43")
	rec := httptest.NewRecorder()
	env.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("user43 status = %d (body=%s)", rec.Code, rec.Body.String())
	}
	if calls := upstream.calls(); calls != 2 {
		t.Fatalf("upstream calls = %d，want 2（跨用户不共享幂等记录）", calls)
	}
	if rows := env.store.insertedRows(); len(rows) != 2 {
		t.Fatalf("task rows = %d，want 2", len(rows))
	}
}

func TestMJIdempotencyWindowAndInflight(t *testing.T) {
	env, idem, upstream := newIdemTestEnv(t, `{"code":1,"description":"ok","result":"mj_window"}`)
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

	// 24h 过期：旧完成记录不参与重放。
	body := []byte(`{"prompt":"cat"}`)
	idem.seed(42, "expired-key", fakeIdemRecord{
		hash:         mj.SubmitRequestHash(mj.EndpointImagine, body),
		status:       "completed",
		publicTaskID: "task_old",
		response:     []byte(`{"code":1,"description":"old","result":"mj_old"}`),
		claimedAt:    now.Add(-25 * time.Hour),
		expiresAt:    now.Add(-time.Hour),
	})
	expired := serveMJ(t, env, requestWithIdemKey(t, string(body), "expired-key", "nkt-mj-user-42"))
	if expired.Code != http.StatusOK || decodeMJEnvelope(t, expired)["result"] != "mj_window" {
		t.Fatalf("过期键没有重新执行: status=%d body=%s", expired.Code, expired.Body.String())
	}
	if calls := upstream.calls(); calls != 1 {
		t.Fatalf("过期键 upstream calls = %d，want 1", calls)
	}

	// 仍在租约内的 in_flight：409，不重复提交。
	idem.seed(42, "inflight-key", fakeIdemRecord{
		hash:      mj.SubmitRequestHash(mj.EndpointImagine, body),
		status:    "in_flight",
		claimedAt: now.Add(-time.Second),
		expiresAt: now.Add(24 * time.Hour),
	})
	inflight := serveMJ(t, env, requestWithIdemKey(t, string(body), "inflight-key", "nkt-mj-user-42"))
	if inflight.Code != http.StatusConflict {
		t.Fatalf("inflight status = %d，want 409 (body=%s)", inflight.Code, inflight.Body.String())
	}
	if got := decodeMJEnvelope(t, inflight)["description"]; got != mj.DescriptionIdempotencyInProgress {
		t.Fatalf("inflight description = %v", got)
	}
	if calls := upstream.calls(); calls != 1 {
		t.Fatalf("inflight upstream calls = %d，want 1", calls)
	}
}

func TestMJChannelLimiterRejectsBeforeBilling(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	var calls atomic.Int64
	blocking := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		_, _ = w.Write([]byte(`{"code":1,"description":"ok","result":"mj_limited"}`))
	}))
	// 注册顺序即清理顺序（LIFO）：先注册 Close，后注册 releaseAll，
	// 保证先放行在飞 handler 再 Close（失败路径 t.Fatal 也不挂住）。
	t.Cleanup(blocking.Close)
	// 失败路径（t.Fatal）也必须先放行上游：httptest.Server.Close 会等待所有
	// 在飞 handler 结束，否则变异验证会挂住而不是报红。
	t.Cleanup(releaseAll)

	store := newFakeMJStore()
	sealer := NewSecretSealer("mj-handler-test-key")
	normal, cand := sealedMJChannel(t, sealer, 10, mj.ChannelTypeMidjourney,
		blocking.URL, mjAllModels(), 10)
	channels := &fakeMJChannels{
		channels:   map[int64]*tasks.ChannelTarget{10: normal},
		candidates: []tasks.MJChannelCandidate{cand},
	}
	config := mjConfigured(map[string]json.RawMessage{
		"mj.channel_max_inflight": mjIntSetting(1),
	})
	env := newMJTestEnvFull(t, store, channels, config, nil, nil, nil, newMJInFlightLimiter())

	// 第一个请求占住渠道槽并阻塞在上游。
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- serveMJ(t, env, newMJRequest(t, http.MethodPost, "/mj/submit/imagine", `{"prompt":"cat"}`))
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("第一个请求没有到达上游")
	}

	// 第二个请求同一渠道：429，未预扣、未出站。
	// 放在 goroutine + 超时里：限制器一旦失效，这个请求会真的出站并阻塞在
	// 上游 release 上——用超时把它变成明确失败，而不是挂住整个测试进程。
	secondCh := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		secondCh <- serveMJ(t, env, newMJRequest(t, http.MethodPost, "/mj/submit/imagine", `{"prompt":"dog"}`))
	}()
	var second *httptest.ResponseRecorder
	select {
	case second = <-secondCh:
	case <-time.After(2 * time.Second):
		t.Fatal("超限请求没有被拒绝：渠道在飞限制器失效（请求已出站并阻塞）")
	}
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("超限 status = %d，want 429 (body=%s)", second.Code, second.Body.String())
	}
	if got := decodeMJEnvelope(t, second)["description"]; got != mj.DescriptionChannelBusy {
		t.Fatalf("超限 description = %v", got)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("超限请求触发了上游: calls = %d", got)
	}
	if got := env.recorderCount(); got != 1 {
		t.Fatalf("超限请求建了计费会话: %d（应在 PreConsume 之前拒绝）", got)
	}

	releaseAll()
	first := <-done
	if first.Code != http.StatusOK {
		t.Fatalf("首个请求 status = %d (body=%s)", first.Code, first.Body.String())
	}

	// 槽释放后，第三个请求恢复。
	third := serveMJ(t, env, newMJRequest(t, http.MethodPost, "/mj/submit/imagine", `{"prompt":"bird"}`))
	if third.Code != http.StatusOK {
		t.Fatalf("槽释放后 status = %d (body=%s)", third.Code, third.Body.String())
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d，want 2", got)
	}
}

func TestMJInFlightLimiterIsPerChannel(t *testing.T) {
	limiter := newMJInFlightLimiter()
	release1, ok := limiter.TryAcquire(1, 1)
	if !ok {
		t.Fatal("channel 1 first acquire failed")
	}
	if _, ok := limiter.TryAcquire(1, 1); ok {
		t.Fatal("channel 1 second acquire should fail at limit 1")
	}
	// 不同渠道各有独立计数。
	release2, ok := limiter.TryAcquire(2, 1)
	if !ok {
		t.Fatal("channel 2 must not be blocked by channel 1")
	}
	release1()
	release1() // 幂等释放：重复 defer 不能把计数减成负数
	if got := limiter.inflightCount(1); got != 0 {
		t.Fatalf("channel 1 inflight = %d，want 0", got)
	}
	if release, ok := limiter.TryAcquire(1, 1); !ok {
		t.Fatal("channel 1 should be acquirable after release")
	} else {
		release()
	}
	release2()
}
