package billing

// S4b 收口用例（H1/M1/M2/M3/M4 + LOW）。
//
// 全部用例依赖真实主库（NEWTG_TEST_DB_DSN 未设置时报 Skip），并发用例另用
// miniredis 提供 requestID 结算锁；锁语义与生产一致（SETNX 不可重入）。

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/store"
)

// newSettlementTestEngine 构造带 miniredis 结算锁的引擎。
func newSettlementTestEngine(t *testing.T, funding SubscriptionFundingSource,
	sat Saturator) (*Engine, *sql.DB) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	gormDB, db := openBillingTestDB(t)
	e := NewEngine(gormDB, nil, rdb, billingTestConfig(), nil, nil, sat)
	e.SetSubscriptionFunding(funding)
	return e, db
}

// seedSubscriptionPreConsume 直接写入一条已预扣记录（模拟 Detach 后的任务）。
func seedSubscriptionPreConsume(funding *fakeSubscriptionFunding, requestID string,
	subscriptionID, userID, amount int64) {
	funding.mu.Lock()
	defer funding.mu.Unlock()
	funding.records[requestID] = store.SubscriptionPreConsume{
		RequestID: requestID, SubscriptionID: subscriptionID, Status: "consumed",
		PreConsumed: amount, CreatedAt: time.Now().UTC(),
	}
	funding.owners[requestID] = userID
	funding.used[subscriptionID] += amount
}

// H1 回归：RefundByRequestIDForToken 必须在持有结算锁后**再次读取**
// request_registry。用例用 recordHook 让退款在「识别订阅来源」后暂停，
// 期间让结算完整收尾；旧实现只复核预扣记录（仍为 consumed）会二次退款。
func TestRefundByRequestIDRechecksRegistryUnderLock(t *testing.T) {
	funding := newFakeSubscriptionFunding(contract.PrefSubscriptionFirst,
		subscriptionAccount(7, 1000, nil))
	gormDB, db := openBillingTestDB(t)
	e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)
	e.SetSubscriptionFunding(funding)
	userID := createBillingTestUser(t, db, 500)
	tokenID := createBillingTestToken(t, db, userID, int64(500))
	req := testUUID(t)
	seedSubscriptionPreConsume(funding, req, 7, userID, 100)
	if _, err := db.Exec(
		`UPDATE api_tokens SET remain_quota = remain_quota - 100 WHERE id = $1`, tokenID); err != nil {
		t.Fatalf("simulate token debit: %v", err)
	}

	entered := make(chan struct{})
	resume := make(chan struct{})
	funding.mu.Lock()
	funding.recordHook = func(_ string, call int) {
		if call == 1 {
			close(entered)
			<-resume
		}
	}
	funding.mu.Unlock()

	var (
		wg        sync.WaitGroup
		refundErr error
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		refundErr = e.RefundByRequestIDForToken(req, tokenID)
	}()
	<-entered // 退款已识别订阅来源，但尚未取锁

	if err := e.TerminateByRequestIDForToken(req, tokenID, contract.UsageFact{}); err != nil {
		t.Fatalf("TerminateByRequestIDForToken: %v", err)
	}
	close(resume)
	wg.Wait()

	if refundErr != nil {
		t.Fatalf("迟到的退款应幂等返回 nil，got %v", refundErr)
	}
	if got := funding.usedOf(7); got != 100 {
		t.Fatalf("amount_used = %d，want 100（不得在结算后退款）", got)
	}
	if state := funding.registry[req]; state != "settled" {
		t.Fatalf("registry = %q，want settled", state)
	}
	if got := funding.recordStatus(req); got != "consumed" {
		t.Fatalf("预扣记录 = %q，want consumed（不得被退款改写）", got)
	}
	if got := tokenRemain(t, db, tokenID).Int64; got != 400 {
		t.Fatalf("令牌剩余 = %d，want 400（不得回补已结算请求）", got)
	}
}

// H1 并发：两个 goroutine 同时对同一 requestID 结算/退款，任意交错后只能
// 发生其中一件。
func TestConcurrentTerminateAndRefundSubscription(t *testing.T) {
	for i := 0; i < 10; i++ {
		funding := newFakeSubscriptionFunding(contract.PrefSubscriptionFirst,
			subscriptionAccount(7, 1000, nil))
		e, c := newSettlementTestEngine(t, funding, nil)
		userID := createBillingTestUser(t, c, 500)
		tokenID := createBillingTestToken(t, c, userID, int64(500))
		req := testUUID(t)
		seedSubscriptionPreConsume(funding, req, 7, userID, 100)
		if _, err := c.Exec(
			`UPDATE api_tokens SET remain_quota = remain_quota - 100 WHERE id = $1`, tokenID); err != nil {
			t.Fatalf("simulate token debit: %v", err)
		}

		start := make(chan struct{})
		var (
			wg        sync.WaitGroup
			termErr   error
			refundErr error
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			termErr = e.TerminateByRequestIDForToken(req, tokenID, contract.UsageFact{})
		}()
		go func() {
			defer wg.Done()
			<-start
			refundErr = e.RefundByRequestIDForToken(req, tokenID)
		}()
		close(start)
		wg.Wait()

		for name, err := range map[string]error{"terminate": termErr, "refund": refundErr} {
			if err != nil && !errors.Is(err, ErrSettlementInProgress) {
				t.Fatalf("iteration %d: %s err = %v", i, name, err)
			}
		}

		used := funding.usedOf(7)
		state := funding.registry[req]
		status := funding.recordStatus(req)
		remain := tokenRemain(t, c, tokenID).Int64
		settledOK := used == 100 && state == "settled" && status == "consumed" && remain == 400
		refundedOK := used == 0 && state == "refunded" && status == "refunded" && remain == 500
		if !settledOK && !refundedOK {
			t.Fatalf("iteration %d: 交错结果不是单一终态：used=%d registry=%q record=%q token=%d",
				i, used, state, status, remain)
		}
	}
}

// M1：实时连接 Terminate 与恢复扫描并发时，也必须由同一把 requestID 结算锁
// 串行化——只能结算或只能退款，不得两者都发生。
func TestConcurrentRealtimeTerminateAndRecoveryScan(t *testing.T) {
	now := time.Now().UTC()
	for i := 0; i < 10; i++ {
		funding := newFakeSubscriptionFunding(contract.PrefSubscriptionFirst,
			subscriptionAccount(7, 1000, nil))
		e, c := newSettlementTestEngine(t, funding, nil)
		userID := createBillingTestUser(t, c, 500)
		tokenID := createBillingTestToken(t, c, userID, int64(500))
		req := testRequestID("s4b-race")
		s := newBillingTestSessionWithToken(e, userID, tokenID, req, perCallSnapshot(100))
		if err := s.PreConsume(contract.QuotaEstimate{Model: "m"}); err != nil {
			t.Fatalf("iteration %d: PreConsume: %v", i, err)
		}

		start := make(chan struct{})
		var (
			wg      sync.WaitGroup
			termErr error
			recErr  error
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			termErr = s.Terminate(contract.UsageFact{})
		}()
		go func() {
			defer wg.Done()
			<-start
			_, recErr = e.RecoverStaleSubscriptionPreConsumes(context.Background(), now, 10)
		}()
		close(start)
		wg.Wait()

		if termErr != nil && !errors.Is(termErr, ErrSettlementInProgress) &&
			!errors.Is(termErr, ErrRequestClosed) && !errors.Is(termErr, ErrSessionClosed) {
			t.Fatalf("iteration %d: Terminate err = %v", i, termErr)
		}
		if recErr != nil && !errors.Is(recErr, ErrSettlementInProgress) {
			t.Fatalf("iteration %d: recovery err = %v", i, recErr)
		}

		used := funding.usedOf(7)
		state := funding.registry[req]
		status := funding.recordStatus(req)
		settledOK := used == 100 && state == "settled" && status == "consumed" && s.settled
		refundedOK := used == 0 && state == "refunded" && status == "refunded"
		if !settledOK && !refundedOK {
			t.Fatalf("iteration %d: 实时结算与恢复扫描交错后出现混合状态：used=%d registry=%q record=%q",
				i, used, state, status)
		}
	}
}

// M2：外部执行者写入 registry 终态后，本会话不得再追加订阅消耗。
func TestSessionSubscriptionChargeIncrementStopsAfterTerminal(t *testing.T) {
	funding := newFakeSubscriptionFunding(contract.PrefSubscriptionFirst,
		subscriptionAccount(7, 1000, nil))
	gormDB, db := openBillingTestDB(t)
	e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)
	e.SetSubscriptionFunding(funding)
	userID := createBillingTestUser(t, db, 500)
	tokenID := createBillingTestToken(t, db, userID, int64(500))
	req := testRequestID("s4b-m2-terminal")
	s := newBillingTestSessionWithToken(e, userID, tokenID, req, perCallSnapshot(100))
	if err := s.PreConsume(contract.QuotaEstimate{Model: "m"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	// 另一个执行者（任务轮询/恢复扫描）已经退款收尾。
	if err := funding.MarkRequestRefunded(context.Background(), req, userID); err != nil {
		t.Fatalf("MarkRequestRefunded: %v", err)
	}

	err := s.ChargeIncrement(30, contract.UsageFact{Model: "m"})
	if !errors.Is(err, ErrRequestClosed) {
		t.Fatalf("ChargeIncrement err = %v，want ErrRequestClosed", err)
	}
	if got := funding.usedOf(7); got != 100 {
		t.Fatalf("amount_used = %d，want 100（终态后不得追加）", got)
	}
	if got := tokenRemain(t, db, tokenID).Int64; got != 400 {
		t.Fatalf("令牌剩余 = %d，want 400（终态后不得扣减）", got)
	}
	// markTerminalLocked 把会话置为「已在他处结案」：本会话不再持有任何可退
	// 金额，令牌由结案方按自己的路径处理（不得重复回补）。
	if s.consumed != 0 || s.tokenDeducted != 0 {
		t.Fatalf("会话累计 = consumed %d / tokenDeducted %d，want 0/0", s.consumed, s.tokenDeducted)
	}

	// 本地终态位：后续调用直接拒绝，不再查询 registry（短 TTL 缓存）。
	before := funding.registryCallCount()
	if err := s.ChargeIncrement(30, contract.UsageFact{Model: "m"}); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("二次 ChargeIncrement err = %v，want ErrSessionClosed", err)
	}
	if got := funding.registryCallCount(); got != before {
		t.Fatalf("终态后再次查询 registry %d 次，want 0（本地标记）", got-before)
	}
}

// M2 缓存节流：未观测到终态时，TTL 内的连续增量只查一次 registry。
func TestSessionSubscriptionChargeIncrementTerminalCheckThrottled(t *testing.T) {
	funding := newFakeSubscriptionFunding(contract.PrefSubscriptionFirst,
		subscriptionAccount(7, 1000, nil))
	gormDB, db := openBillingTestDB(t)
	e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)
	e.SetSubscriptionFunding(funding)
	userID := createBillingTestUser(t, db, 500)
	tokenID := createBillingTestToken(t, db, userID, int64(500))
	s := newBillingTestSessionWithToken(e, userID, tokenID,
		testRequestID("s4b-m2-ttl"), perCallSnapshot(100))
	if err := s.PreConsume(contract.QuotaEstimate{Model: "m"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if err := s.ChargeIncrement(10, contract.UsageFact{Model: "m"}); err != nil {
		t.Fatalf("ChargeIncrement 1: %v", err)
	}
	if err := s.ChargeIncrement(10, contract.UsageFact{Model: "m"}); err != nil {
		t.Fatalf("ChargeIncrement 2: %v", err)
	}
	if got := funding.registryCallCount(); got != 1 {
		t.Fatalf("registry 查询 = %d，want 1（TTL 内复用结果）", got)
	}
	if got := funding.usedOf(7); got != 120 {
		t.Fatalf("amount_used = %d，want 120", got)
	}
}

// M3：增量退款带 `amount_used >= consumed` 守卫。周期重置后旧周期的增量
// 回退不得侵蚀新周期用量（方向安全地少退）。
func TestSessionSubscriptionIncrementRefundGuardedAfterReset(t *testing.T) {
	funding := newFakeSubscriptionFunding(contract.PrefSubscriptionFirst,
		subscriptionAccount(7, 1000, nil))
	gormDB, db := openBillingTestDB(t)
	e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)
	e.SetSubscriptionFunding(funding)
	userID := createBillingTestUser(t, db, 500)
	tokenID := createBillingTestToken(t, db, userID, int64(500))
	req := testRequestID("s4b-m3-reset")
	s := newBillingTestSessionWithToken(e, userID, tokenID, req, perCallSnapshot(100))
	if err := s.PreConsume(contract.QuotaEstimate{Model: "m"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if err := s.ChargeIncrement(30, contract.UsageFact{Model: "m"}); err != nil {
		t.Fatalf("ChargeIncrement: %v", err)
	}
	// 周期重置把计数归零，新周期又用了 20。
	funding.setUsed(7, 20)

	if err := s.Refund(); err != nil {
		t.Fatalf("Refund: %v", err)
	}
	if got := funding.usedOf(7); got != 20 {
		t.Fatalf("amount_used = %d，want 20（旧周期退款不得侵蚀新周期用量）", got)
	}
	if got := funding.recordStatus(req); got != "refunded" {
		t.Fatalf("预扣记录 = %q，want refunded", got)
	}
	if got := tokenRemain(t, db, tokenID).Int64; got != 500 {
		t.Fatalf("令牌剩余 = %d，want 500（令牌回补不受订阅周期影响）", got)
	}
	if got := funding.usedOf(7); got < 0 {
		t.Fatalf("amount_used = %d，不得为负", got)
	}
}

// M4：订阅来源的任务带非空 fact 结算时，delta = cost − preConsume，
// 令牌额度同步调整，成功日志带偏好。
func TestTerminateByRequestIDSubscriptionTaskNonEmptyFact(t *testing.T) {
	funding := newFakeSubscriptionFunding(contract.PrefSubscriptionFirst,
		subscriptionAccount(7, 1000, nil))
	gormDB, db := openBillingTestDB(t)
	sat := &recordingSaturator{db: db}
	src := staticPricingSource{snap: *perCallSnapshot(100)}
	e := NewEngine(gormDB, nil, nil, billingTestConfig(), src, nil, sat)
	e.SetSubscriptionFunding(funding)
	userID := createBillingTestUser(t, db, 500)
	tokenID := createBillingTestToken(t, db, userID, int64(500))
	req := testUUID(t)
	seedSubscriptionPreConsume(funding, req, 7, userID, 50)
	if _, err := db.Exec(
		`UPDATE api_tokens SET remain_quota = remain_quota - 50 WHERE id = $1`, tokenID); err != nil {
		t.Fatalf("simulate token debit: %v", err)
	}

	if err := e.TerminateByRequestIDForToken(req, tokenID, contract.UsageFact{Model: "m"}); err != nil {
		t.Fatalf("TerminateByRequestIDForToken: %v", err)
	}
	if got := funding.usedOf(7); got != 100 {
		t.Fatalf("amount_used = %d，want 100（delta = 100 - 50）", got)
	}
	if state := funding.registry[req]; state != "settled" {
		t.Fatalf("registry = %q，want settled", state)
	}
	if got := tokenRemain(t, db, tokenID).Int64; got != 400 {
		t.Fatalf("令牌剩余 = %d，want 400（补扣 50）", got)
	}
	if sat.called != 1 {
		t.Fatalf("Saturate 调用 = %d，want 1", sat.called)
	}
	if out := sat.outcome; out.Cost != 100 || out.SubscriptionPostDelta != 50 ||
		out.SubscriptionPreConsumed != 50 || out.BillingPreference != "subscription_first" {
		t.Fatalf("结算 outcome = %+v，字段不符（需 delta=50/cost=100/偏好）", out)
	}
}

// M4：增量已落账但 DebitToken 失败——订阅侧增量保留（已交付用量不得白送），
// 请求失败，令牌不再扣。
func TestSessionSubscriptionChargeIncrementTokenDebitFailure(t *testing.T) {
	funding := newFakeSubscriptionFunding(contract.PrefSubscriptionFirst,
		subscriptionAccount(7, 1000, nil))
	gormDB, db := openBillingTestDB(t)
	e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)
	e.SetSubscriptionFunding(funding)
	userID := createBillingTestUser(t, db, 500)
	// 令牌只有 100：预扣 100 后余额为 0，增量 30 的 DebitToken 必然失败。
	tokenID := createBillingTestToken(t, db, userID, int64(100))
	s := newBillingTestSessionWithToken(e, userID, tokenID,
		testRequestID("s4b-m4-debit"), perCallSnapshot(100))
	if err := s.PreConsume(contract.QuotaEstimate{Model: "m"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}

	err := s.ChargeIncrement(30, contract.UsageFact{Model: "m"})
	if err == nil {
		t.Fatal("ChargeIncrement err = nil，want DebitToken 失败")
	}
	if got := funding.usedOf(7); got != 130 {
		t.Fatalf("amount_used = %d，want 130（增量已落账且不可回滚）", got)
	}
	if got := tokenRemain(t, db, tokenID).Int64; got != 0 {
		t.Fatalf("令牌剩余 = %d，want 0（扣减失败）", got)
	}
	if s.consumed != 30 || s.tokenDeducted != 100 {
		t.Fatalf("会话累计 = consumed %d / tokenDeducted %d，want 30/100",
			s.consumed, s.tokenDeducted)
	}
	if s.settled || s.refunded {
		t.Fatal("增量令牌扣减失败不得终结会话，由调用方按请求失败处理")
	}
}

// LOW：MarkRequestSettled 短暂失败按退避重试，最终成功仍写 settled。
func TestSubscriptionSettlementRegistryRetries(t *testing.T) {
	funding := newFakeSubscriptionFunding(contract.PrefSubscriptionFirst,
		subscriptionAccount(7, 1000, nil))
	funding.markSettledFailures = 2
	gormDB, db := openBillingTestDB(t)
	e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)
	e.SetSubscriptionFunding(funding)
	userID := createBillingTestUser(t, db, 500)
	tokenID := createBillingTestToken(t, db, userID, int64(500))
	req := testRequestID("s4b-registry-retry")
	s := newBillingTestSessionWithToken(e, userID, tokenID, req, perCallSnapshot(100))
	if err := s.PreConsume(contract.QuotaEstimate{Model: "m"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if err := s.Terminate(contract.UsageFact{Model: "m"}); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if state := funding.registry[req]; state != "settled" {
		t.Fatalf("registry = %q，want settled（第 3 次尝试应成功）", state)
	}
	funding.mu.Lock()
	calls := funding.markSettledCalls
	funding.mu.Unlock()
	if calls != 3 {
		t.Fatalf("MarkRequestSettled 调用 = %d，want 3", calls)
	}
}
