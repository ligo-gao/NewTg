//go:build integration

package billing

// W9-B2（R-W9B-02 / G-DOM-35①）的验收用例：订阅预扣的在飞租约、续租、
// 兜底扫描谓词与终态清理。
//
// 全部用例走真实 PostgreSQL（NEWTG_TEST_DB_DSN 未设置时 Skip）；隔离库由
// 调用方指定（例如 newtg_dom_w9l）。用例构造缩短的窗口/TTL 配置，避免
// 真的等待 15 分钟。
//
// 覆盖：
//   - 持续续租的长任务/长流跑过窗口 → 不被退款，完成后按预扣额结算；
//   - 无续租/进程崩溃 → 租约过期后被兜底退款（方向安全）；
//   - 续租延长租约；扫描与续租并发 → 只产生合法终态；
//   - 任务轮询续租覆盖单轮 > TTL（计费侧的续租入口）；
//   - 钱包路径既有租约/退款行为回归（TestSessionLeaseLifecycle，同库运行）；
//   - B1 复核证据缺口：令牌回补的 user_id 绑定、退款 CAS 未命中整笔回滚。

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"sync"
	"testing"
	"time"

	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/store"
)

// w9lConfig 构造缩短窗口/TTL 的计费配置（R-W9B-02 要求两组参数分开）。
func w9lConfig(windowSeconds, leaseTTLSeconds int64) mapConfig {
	return mapConfig{
		ConfigPreConsumeRatio:                     json.RawMessage("1.0"),
		ConfigTrustQuotaUSD:                       json.RawMessage("0"),
		ConfigSubscriptionRecoveryWindowSeconds:   json.RawMessage(strconv.FormatInt(windowSeconds, 10)),
		ConfigSubscriptionInflightLeaseTTLSeconds: json.RawMessage(strconv.FormatInt(leaseTTLSeconds, 10)),
	}
}

// w9lNewEngine 构造真实仓储 + 缩短参数的计费引擎。
func w9lNewEngine(t *testing.T, cfg mapConfig) (*Engine, *sql.DB, *store.SQLSubscriptionRepo) {
	t.Helper()
	gormDB, db := openBillingTestDB(t)
	repo := store.NewSubscriptionRepo(db)
	e := NewEngine(gormDB, nil, nil, cfg, nil, nil, nil)
	e.SetSubscriptionFunding(NewStoreSubscriptionFunding(repo, db))
	// 流式会话心跳：测试用毫秒级间隔驱动真实续租循环（钱包心跳旋钮不参与）。
	e.subHeartbeat = 20 * time.Millisecond
	return e, db, repo
}

// w9lSeed 创建一个订阅用户的预扣（订阅 + 令牌侧扣减），并注册清理。
func w9lSeed(t *testing.T, db *sql.DB, repo *store.SQLSubscriptionRepo,
	amount int64) (userID, tokenID int64, sub store.Subscription, requestID string) {
	t.Helper()
	userID = createBillingTestUser(t, db, 500)
	tokenID = createBillingTestToken(t, db, userID, int64(500))
	sub = w9bCreateSubscription(t, db, repo, userID, 1000)
	requestID = testUUID(t)
	w9bCleanupRequest(t, db, requestID)
	w9bSeedSubscriptionPreConsume(t, db, repo, requestID, sub.ID, tokenID, amount)
	return userID, tokenID, sub, requestID
}

// w9lBackdatePreConsume 把预扣记录的创建时间回拨（模拟请求已超过窗口）。
func w9lBackdatePreConsume(t *testing.T, db *sql.DB, requestID string, at time.Time) {
	t.Helper()
	if _, err := db.Exec(`UPDATE subscription_pre_consume_records SET created_at = $2 WHERE request_id = $1`,
		requestID, at); err != nil {
		t.Fatalf("backdate pre-consume: %v", err)
	}
}

// w9lBackdateLease 把 in-flight 租约心跳回拨（模拟进程已崩溃）。
func w9lBackdateLease(t *testing.T, db *sql.DB, requestID string, at time.Time) {
	t.Helper()
	if _, err := db.Exec(`UPDATE request_registry SET heartbeat_at = $2 WHERE request_id = $1`,
		requestID, at); err != nil {
		t.Fatalf("backdate lease: %v", err)
	}
}

// w9lLeaseHeartbeat 读取租约心跳时间。
func w9lLeaseHeartbeat(t *testing.T, db *sql.DB, requestID string) time.Time {
	t.Helper()
	var at time.Time
	if err := db.QueryRow(`SELECT heartbeat_at FROM request_registry WHERE request_id = $1`,
		requestID).Scan(&at); err != nil {
		t.Fatalf("read lease heartbeat: %v", err)
	}
	return at
}

// TestW9LLongRunningRequestKeptAliveByRenewal 覆盖 G-DOM-35 的核心回归：
// 请求年龄超过兜底窗口，但只要租约持续续租就不被退款；完成后按预扣额结算。
func TestW9LLongRunningRequestKeptAliveByRenewal(t *testing.T) {
	window, leaseTTL := int64(2), int64(1)
	e, db, repo := w9lNewEngine(t, w9lConfig(window, leaseTTL))
	userID, tokenID, sub, req := w9lSeed(t, db, repo, 100)

	// 预扣记录已经超过窗口（模拟任务/长流已经跑过 900s 的等价场景）。
	w9lBackdatePreConsume(t, db, req, time.Now().UTC().Add(-10*time.Second))

	// 持续续租：跨越窗口，扫描每轮都必须返回 0。这里直接调用计费适配器的
	// 续租入口（流式会话心跳与任务轮询器走的是同一个方法）。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := e.subscriptionFunding.RenewInflightLease(context.Background(), []string{req}); err != nil {
			t.Fatalf("renew lease: %v", err)
		}
		recovered, err := e.RecoverStaleSubscriptionPreConsumes(context.Background(), time.Now().UTC(), 10)
		if err != nil {
			t.Fatalf("recover while renewed: %v", err)
		}
		if recovered != 0 {
			t.Fatalf("持续续租期间 recovered = %d，want 0（长请求不得被退款）", recovered)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if got := w9bAmountUsed(t, db, sub.ID); got != 100 {
		t.Fatalf("续租期间 amount_used = %d，want 100（预扣未退）", got)
	}
	if got := w9bRegistryState(t, db, req); got != "in_flight" {
		t.Fatalf("续租期间 registry = %q，want in_flight", got)
	}

	// 完成：按预扣额结算（空 fact + 冻结成本 = 预扣额）。
	if err := e.TerminateByRequestIDForTokenCost(req, tokenID, 100, contract.UsageFact{Model: "m"}); err != nil {
		t.Fatalf("Terminate after long run: %v", err)
	}
	if got := w9bAmountUsed(t, db, sub.ID); got != 100 {
		t.Fatalf("结算后 amount_used = %d，want 100（按预扣额收费，不得免费）", got)
	}
	if got := w9bRegistryState(t, db, req); got != "settled" {
		t.Fatalf("结算后 registry = %q，want settled（终态清理）", got)
	}
	// 终态后扫描不得再退款。
	if _, err := e.subscriptionFunding.RenewInflightLease(context.Background(), []string{req}); err != nil {
		t.Fatalf("renew after settle: %v", err)
	}
	recovered, err := e.RecoverStaleSubscriptionPreConsumes(context.Background(), time.Now().UTC(), 10)
	if err != nil || recovered != 0 {
		t.Fatalf("结算后扫描 = %d/%v，want 0/nil", recovered, err)
	}
	if got := w9bAmountUsed(t, db, sub.ID); got != 100 {
		t.Fatalf("结算后扫描改变了 amount_used = %d，want 100", got)
	}
	_ = userID
}

// TestW9LSubscriptionSessionHeartbeatRenewsLease 覆盖流式会话的心跳接线：
// 订阅会话 PreConsume 成功后自动启动续租循环；只要会话在跑，租约就保持
// 新鲜、扫描不退款，Terminate 后置终态。
func TestW9LSubscriptionSessionHeartbeatRenewsLease(t *testing.T) {
	e, db, repo := w9lNewEngine(t, w9lConfig(2, 1))
	userID := createBillingTestUser(t, db, 500)
	tokenID := createBillingTestToken(t, db, userID, int64(500))
	sub := w9bCreateSubscription(t, db, repo, userID, 1000)
	if _, err := db.Exec(`UPDATE users SET billing_preference = 'subscription_first' WHERE id = $1`, userID); err != nil {
		t.Fatalf("set billing preference: %v", err)
	}

	req := testUUID(t)
	w9bCleanupRequest(t, db, req)
	s := newBillingTestSessionWithToken(e, userID, tokenID, req, perCallSnapshot(100))
	if err := s.PreConsume(contract.QuotaEstimate{Model: "m"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	t.Cleanup(func() {
		// 用例失败时也要停掉后台心跳，避免 goroutine 泄漏到后续用例。
		_ = s.Refund()
	})
	if got := w9bAmountUsed(t, db, sub.ID); got != 100 {
		t.Fatalf("预扣后 amount_used = %d，want 100", got)
	}
	if got := w9bRegistryState(t, db, req); got != "in_flight" {
		t.Fatalf("预扣后 registry = %q，want in_flight", got)
	}

	// 把心跳回拨到租约过期界之外，等待会话心跳真实续租。
	stale := time.Now().UTC().Add(-5 * time.Second)
	w9lBackdateLease(t, db, req, stale)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if w9lLeaseHeartbeat(t, db, req).After(stale.Add(time.Second)) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if hb := w9lLeaseHeartbeat(t, db, req); !hb.After(stale.Add(time.Second)) {
		t.Fatalf("会话心跳未续租：heartbeat = %v（初始 %v）", hb, stale)
	}

	// 请求超过窗口，但会话仍在跑：扫描必须跳过。
	w9lBackdatePreConsume(t, db, req, time.Now().UTC().Add(-10*time.Second))
	recovered, err := e.RecoverStaleSubscriptionPreConsumes(context.Background(), time.Now().UTC(), 10)
	if err != nil || recovered != 0 {
		t.Fatalf("会话存活期间扫描 = %d/%v，want 0/nil", recovered, err)
	}

	// 结束：订阅按预扣额结算，registry 置 settled，心跳停止。
	if err := s.Terminate(contract.UsageFact{}); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if got := w9bAmountUsed(t, db, sub.ID); got != 100 {
		t.Fatalf("结算后 amount_used = %d，want 100", got)
	}
	if got := w9bRegistryState(t, db, req); got != "settled" {
		t.Fatalf("结算后 registry = %q，want settled", got)
	}
	// 幂等：重复 Terminate 不改变任何资金状态。
	if err := s.Terminate(contract.UsageFact{}); err != nil {
		t.Fatalf("重复 Terminate: %v", err)
	}
	if got := w9bAmountUsed(t, db, sub.ID); got != 100 {
		t.Fatalf("重复 Terminate 后 amount_used = %d，want 100", got)
	}
}

// TestW9LCrashedRequestRefundedAfterLeaseExpiry 覆盖真正的崩溃：没有续租，
// 租约在 TTL 后过期，扫描只退一次（方向安全：订阅退回，令牌保持已扣）。
func TestW9LCrashedRequestRefundedAfterLeaseExpiry(t *testing.T) {
	e, db, repo := w9lNewEngine(t, w9lConfig(2, 1))
	_, tokenID, sub, req := w9lSeed(t, db, repo, 100)

	now := time.Now().UTC()
	w9lBackdatePreConsume(t, db, req, now.Add(-10*time.Second))
	w9lBackdateLease(t, db, req, now.Add(-5*time.Second)) // 心跳停满多个 TTL

	recovered, err := e.RecoverStaleSubscriptionPreConsumes(context.Background(), now, 10)
	if err != nil || recovered != 1 {
		t.Fatalf("崩溃恢复 = %d/%v，want 1/nil", recovered, err)
	}
	if got := w9bAmountUsed(t, db, sub.ID); got != 0 {
		t.Fatalf("崩溃退款后 amount_used = %d，want 0", got)
	}
	if got := w9bRegistryState(t, db, req); got != "refunded" {
		t.Fatalf("崩溃退款后 registry = %q，want refunded", got)
	}
	if got := w9bPreConsumeStatus(t, db, req); got != "refunded" {
		t.Fatalf("崩溃退款后预扣记录 = %q，want refunded", got)
	}
	// 方向安全：扫描无法回补令牌（预扣表不存 tokenID），令牌保持已扣。
	if got := tokenRemain(t, db, tokenID).Int64; got != 400 {
		t.Fatalf("崩溃退款后令牌 = %d，want 400（少退方向，不得多送）", got)
	}

	// 重复扫描幂等：不得二次退款。
	recovered, err = e.RecoverStaleSubscriptionPreConsumes(context.Background(), now, 10)
	if err != nil || recovered != 0 {
		t.Fatalf("重复扫描 = %d/%v，want 0/nil", recovered, err)
	}
}

// TestW9LLeaseGuardRejectsLiveLease 覆盖扫描与续租的线性化：退款事务内的
// 租约守卫在心跳新鲜时拒绝整笔退款；心跳过期后才允许退款。
func TestW9LLeaseGuardRejectsLiveLease(t *testing.T) {
	_, db, repo := w9lNewEngine(t, w9lConfig(2, 1))
	_, tokenID, sub, req := w9lSeed(t, db, repo, 100)

	now := time.Now().UTC()
	w9lBackdatePreConsume(t, db, req, now.Add(-10*time.Second))

	// 心跳新鲜（预扣事务写入 now）：守卫必须拒绝，且不改任何资金状态。
	res, err := repo.RefundSubscriptionRequest(context.Background(), store.SubscriptionRefund{
		RequestID:          req,
		UserID:             1,
		TokenID:            tokenID,
		TokenAmount:        100,
		LeaseExpiredBefore: now.Add(-time.Second),
	})
	if err != nil {
		t.Fatalf("guarded refund (live lease): %v", err)
	}
	if res.Applied || res.Terminal != "in_flight" {
		t.Fatalf("live lease 退款结果 = %+v，want Applied=false/Terminal=in_flight", res)
	}
	if got := w9bAmountUsed(t, db, sub.ID); got != 100 {
		t.Fatalf("live lease 被退款后 amount_used = %d，want 100", got)
	}
	if got := w9bPreConsumeStatus(t, db, req); got != "consumed" {
		t.Fatalf("live lease 被退款后预扣 = %q，want consumed", got)
	}
	if got := w9bRegistryState(t, db, req); got != "in_flight" {
		t.Fatalf("live lease 被退款后 registry = %q，want in_flight（不得复活/退款）", got)
	}
	if got := tokenRemain(t, db, tokenID).Int64; got != 400 {
		t.Fatalf("live lease 被退款后令牌 = %d，want 400", got)
	}

	// 心跳过期后同一调用必须成功（整笔退款 + registry refunded）。
	w9lBackdateLease(t, db, req, now.Add(-5*time.Second))
	res, err = repo.RefundSubscriptionRequest(context.Background(), store.SubscriptionRefund{
		RequestID:          req,
		UserID:             1,
		TokenID:            tokenID,
		TokenAmount:        100,
		LeaseExpiredBefore: now.Add(-time.Second),
	})
	if err != nil {
		t.Fatalf("guarded refund (expired lease): %v", err)
	}
	if !res.Applied {
		t.Fatalf("expired lease 退款 = %+v，want Applied=true", res)
	}
	if got := w9bAmountUsed(t, db, sub.ID); got != 0 {
		t.Fatalf("expired lease 退款后 amount_used = %d，want 0", got)
	}
	if got := w9bRegistryState(t, db, req); got != "refunded" {
		t.Fatalf("expired lease 退款后 registry = %q，want refunded", got)
	}
}

// TestW9LRenewalExtendsLease 覆盖「续租延长租约」：过期边缘的租约被续租后
// 心跳前移，不再满足过期条件。
func TestW9LRenewalExtendsLease(t *testing.T) {
	e, db, repo := w9lNewEngine(t, w9lConfig(2, 1))
	_, _, _, req := w9lSeed(t, db, repo, 100)

	// 先把心跳放到 TTL 之外，再续租。
	w9lBackdateLease(t, db, req, time.Now().UTC().Add(-5*time.Second))
	before := w9lLeaseHeartbeat(t, db, req)
	renewed, err := e.subscriptionFunding.RenewInflightLease(context.Background(), []string{req})
	if err != nil || renewed != 1 {
		t.Fatalf("RenewInflightLease = %d/%v，want 1/nil", renewed, err)
	}
	after := w9lLeaseHeartbeat(t, db, req)
	if !after.After(before) {
		t.Fatalf("续租后心跳 = %v，want 晚于 %v", after, before)
	}
	if time.Since(after) > time.Minute {
		t.Fatalf("续租后心跳 = %v，want 接近 now", after)
	}

	// 终态行的续租是 no-op（不能复活已结案请求）。
	if _, err := db.Exec(`UPDATE request_registry SET state = 'refunded' WHERE request_id = $1`, req); err != nil {
		t.Fatalf("mark refunded: %v", err)
	}
	renewed, err = e.subscriptionFunding.RenewInflightLease(context.Background(), []string{req})
	if err != nil || renewed != 0 {
		t.Fatalf("终态后续租 = %d/%v，want 0/nil（不得复活）", renewed, err)
	}
	if got := w9bRegistryState(t, db, req); got != "refunded" {
		t.Fatalf("终态后续租把 registry 改为 %q，want refunded", got)
	}
}

// TestW9LScanAndRenewalConcurrent 覆盖扫描与续租并发：只有合法终态，
// 不会出现「已退款又结算」或「重复退款」。
func TestW9LScanAndRenewalConcurrent(t *testing.T) {
	for i := 0; i < 5; i++ {
		e, db, repo := w9lNewEngine(t, w9lConfig(2, 1))
		_, tokenID, sub, req := w9lSeed(t, db, repo, 100)
		now := time.Now().UTC()
		w9lBackdatePreConsume(t, db, req, now.Add(-10*time.Second))
		w9lBackdateLease(t, db, req, now.Add(-5*time.Second))

		start := make(chan struct{})
		var wg sync.WaitGroup
		var recovered int
		var scanErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			// 续租循环：与扫描在租约行上竞争。
			for j := 0; j < 5; j++ {
				_, _ = e.subscriptionFunding.RenewInflightLease(context.Background(), []string{req})
				time.Sleep(2 * time.Millisecond)
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			recovered, scanErr = e.RecoverStaleSubscriptionPreConsumes(context.Background(), time.Now().UTC(), 10)
		}()
		close(start)
		wg.Wait()
		if scanErr != nil {
			t.Fatalf("iteration %d: scan err = %v", i, scanErr)
		}
		if recovered > 1 {
			t.Fatalf("iteration %d: recovered = %d，want <= 1", i, recovered)
		}

		state := w9bRegistryState(t, db, req)
		used := w9bAmountUsed(t, db, sub.ID)
		switch {
		case state == "refunded" && used == 0:
			// 退款先落地：迟到的结算必须 no-op（B1 终态互斥）。
			if err := e.TerminateByRequestIDForTokenCost(req, tokenID, 100, contract.UsageFact{Model: "m"}); err != nil {
				t.Fatalf("iteration %d: late terminate err = %v", i, err)
			}
			if got := w9bAmountUsed(t, db, sub.ID); got != 0 {
				t.Fatalf("iteration %d: 已退款又被结算，amount_used = %d", i, got)
			}
			if got := w9bRegistryState(t, db, req); got != "refunded" {
				t.Fatalf("iteration %d: 已退款后 registry = %q，want refunded", i, got)
			}
		case used == 100:
			// 未退款（续租获胜）：registry 必须仍是在飞或已结算，不能是退款。
			if state != "in_flight" {
				t.Fatalf("iteration %d: used=100 但 registry = %q，want in_flight", i, state)
			}
			if err := e.TerminateByRequestIDForTokenCost(req, tokenID, 100, contract.UsageFact{Model: "m"}); err != nil {
				t.Fatalf("iteration %d: terminate err = %v", i, err)
			}
			if got := w9bRegistryState(t, db, req); got != "settled" {
				t.Fatalf("iteration %d: 结算后 registry = %q，want settled", i, got)
			}
		default:
			t.Fatalf("iteration %d: 混合状态 used=%d registry=%q（非法终态）", i, used, state)
		}
	}
}

// TestW9LTaskPollerRenewalCoversLongRound 覆盖任务轮询的业务前提：单轮时长
// 超过租约 TTL 时，逐任务续租仍能保住活跃任务（任务入口在 tasks 包有单元
// 用例；这里验证它依赖的计费续租入口在真实 PG 上生效）。
func TestW9LTaskPollerRenewalCoversLongRound(t *testing.T) {
	e, db, repo := w9lNewEngine(t, w9lConfig(2, 1))
	_, _, sub, req := w9lSeed(t, db, repo, 100)

	// 模拟 Detach 后的任务：会话不再续租，轮询器在「单轮」中每 300ms
	// 续租一次（远大于 1s TTL 的轮转间隔在实际中不会发生，这里压缩时间）。
	w9lBackdatePreConsume(t, db, req, time.Now().UTC().Add(-10*time.Second))
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(300 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				_, _ = e.subscriptionFunding.RenewInflightLease(context.Background(), []string{req})
			}
		}
	}()

	// 2.5s > 2s 窗口且远大于 1s TTL：若只续租一次（轮批开头），后续扫描
	// 会把任务当崩溃退款；逐任务续租则必须一直存活。
	deadline := time.Now().Add(2500 * time.Millisecond)
	for time.Now().Before(deadline) {
		time.Sleep(400 * time.Millisecond)
		recovered, err := e.RecoverStaleSubscriptionPreConsumes(context.Background(), time.Now().UTC(), 10)
		if err != nil || recovered != 0 {
			close(stop)
			<-done
			t.Fatalf("任务轮询续租期间扫描 = %d/%v，want 0/nil", recovered, err)
		}
	}
	close(stop)
	<-done

	if got := w9bAmountUsed(t, db, sub.ID); got != 100 {
		t.Fatalf("终态前 amount_used = %d，want 100（续租期间不得退款）", got)
	}
	if got := w9bRegistryState(t, db, req); got != "in_flight" {
		t.Fatalf("终态前 registry = %q，want in_flight", got)
	}
}

// TestW9LTokenCreditRequiresUserBinding 补齐 B1 复核证据缺口①：
// creditTokenQuotaTx 的 UPDATE 必须绑定 user_id。令牌属于他人时回补 0 行，
// 订阅退款仍然完成（事务不因此失败）。
func TestW9LTokenCreditRequiresUserBinding(t *testing.T) {
	_, db, repo := w9lNewEngine(t, w9lConfig(2, 1))
	ownerID, ownerToken, _, req := w9lSeed(t, db, repo, 100)
	otherID := createBillingTestUser(t, db, 500)

	// 用「另一个用户」的 userID 调退款：令牌 UPDATE 命中 0 行。
	res, err := repo.RefundSubscriptionRequest(context.Background(), store.SubscriptionRefund{
		RequestID:   req,
		UserID:      otherID,
		TokenID:     ownerToken,
		TokenAmount: 100,
	})
	if err != nil {
		t.Fatalf("refund with mismatched user: %v", err)
	}
	if !res.Applied {
		t.Fatalf("退款 = %+v，want Applied=true（订阅退款不受令牌归属影响）", res)
	}
	if got := tokenRemain(t, db, ownerToken).Int64; got != 400 {
		t.Fatalf("他人令牌剩余 = %d，want 400（不得跨用户回补）", got)
	}
	_ = ownerID
}

// TestW9LRefundRegistryCASMissRollsBackEverything 补齐 B1 复核证据缺口②：
// 直接调仓储（绕过上层预检），registry 已 settled 时退款 CAS 未命中，
// 整笔事务回滚——amount_used、预扣状态、令牌都不变。
func TestW9LRefundRegistryCASMissRollsBackEverything(t *testing.T) {
	_, db, repo := w9lNewEngine(t, w9lConfig(2, 1))
	_, tokenID, sub, req := w9lSeed(t, db, repo, 100)

	// 模拟另一个执行者已结算：registry.settled（预扣记录仍是 consumed）。
	if _, err := db.Exec(`UPDATE request_registry SET state = 'settled', settled_at = now()
		WHERE request_id = $1`, req); err != nil {
		t.Fatalf("mark settled: %v", err)
	}

	res, err := repo.RefundSubscriptionRequest(context.Background(), store.SubscriptionRefund{
		RequestID:   req,
		UserID:      1,
		TokenID:     tokenID,
		TokenAmount: 100,
	})
	if err != nil {
		t.Fatalf("refund after settle: %v", err)
	}
	if res.Applied || res.Terminal != "settled" {
		t.Fatalf("CAS 未命中结果 = %+v，want Applied=false/Terminal=settled", res)
	}
	if got := w9bAmountUsed(t, db, sub.ID); got != 100 {
		t.Fatalf("CAS 未命中后 amount_used = %d，want 100（整笔回滚）", got)
	}
	if got := w9bPreConsumeStatus(t, db, req); got != "consumed" {
		t.Fatalf("CAS 未命中后预扣 = %q，want consumed（整笔回滚）", got)
	}
	if got := w9bRegistryState(t, db, req); got != "settled" {
		t.Fatalf("CAS 未命中后 registry = %q，want settled（不得降级）", got)
	}
	if got := tokenRemain(t, db, tokenID).Int64; got != 400 {
		t.Fatalf("CAS 未命中后令牌 = %d，want 400（不得回补）", got)
	}
}
