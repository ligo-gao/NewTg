package billing

// W9 计费 P0（B1 + B3）的验收用例。
//
// B1（R-W9B-01）：订阅结算的 delta 与 request_registry 终态 CAS 必须在**同一个
// PG 事务**内提交；registry 写失败必须连同 delta 一起回滚，重复/并发结算只能
// 生效一次，迟到退款看到终态后 no-op。
//
// B3（R-W9B-03）：钱包退款 / 订阅退款与令牌回补必须在同一事务内提交；令牌
// UPDATE 失败时整笔退款回滚，不得出现「钱已退、令牌少补」。
//
// 全部用例走真实 PostgreSQL（NEWTG_TEST_DB_DSN 未设置时 Skip）；故障注入用
// 真实 DB 触发器，确保验证的是事务边界而不是测试替身。
// 账号/套餐/订阅/预扣/registry 行都在 t.Cleanup 中删除。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/store"
)

// w9bCreateSubscription 创建用户可见的套餐 + 活跃订阅（真实仓储）。
func w9bCreateSubscription(t *testing.T, db *sql.DB, repo *store.SQLSubscriptionRepo,
	userID, total int64) store.Subscription {
	t.Helper()
	ctx := context.Background()
	plan, err := repo.CreatePlan(ctx, store.SubscriptionPlanInput{
		PlanKey:      fmt.Sprintf("w9b-%d", time.Now().UnixNano()),
		Title:        "W9B",
		PeriodDays:   30,
		PriceAmount:  100,
		Currency:     "CNY",
		GrantCredits: total,
	})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	now := time.Now().UTC()
	sub, err := repo.CreateSubscription(ctx, store.SubscriptionCreateInput{
		UserID:            userID,
		PlanID:            plan.ID,
		PlanTitleSnapshot: plan.Title,
		StartAt:           now.Add(-time.Hour),
		EndAt:             now.Add(time.Hour),
		AmountTotal:       total,
		AmountUsed:        0,
		Currency:          plan.Currency,
		PricePaidAmount:   plan.PriceAmount,
		PeriodDays:        plan.PeriodDays,
		GraceDays:         plan.GraceDays,
	})
	if err != nil {
		t.Fatalf("CreateSubscription: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM subscription_pre_consume_records WHERE subscription_id = $1`, sub.ID)
		_, _ = db.Exec(`DELETE FROM subscriptions WHERE id = $1`, sub.ID)
		_, _ = db.Exec(`DELETE FROM subscription_plans WHERE id = $1`, plan.ID)
	})
	return sub
}

// w9bNewRealEngine 构造生产装配（真实 store 适配器 + 无 Redis 结算锁）。
//
// 不传 Redis 是刻意的：并发用例要证明 **数据库终态 CAS** 本身就能保证恰好
// 一次结算，而不是依赖 Redis SETNX。生产上两者叠加（锁收窄窗口、CAS 兜底）。
func w9bNewRealEngine(t *testing.T) (*Engine, *sql.DB, *store.SQLSubscriptionRepo) {
	t.Helper()
	gormDB, db := openBillingTestDB(t)
	repo := store.NewSubscriptionRepo(db)
	e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)
	e.SetSubscriptionFunding(NewStoreSubscriptionFunding(repo, db))
	return e, db, repo
}

// w9bSeedSubscriptionPreConsume 模拟一次订阅预扣：预扣记录 + amount_used +
// 令牌已扣（与 chargeSubscriptionLocked 的「先令牌、后订阅」一致）。
func w9bSeedSubscriptionPreConsume(t *testing.T, db *sql.DB, repo *store.SQLSubscriptionRepo,
	requestID string, subscriptionID, tokenID, amount int64) {
	t.Helper()
	if _, _, err := repo.PreConsumeUserSubscription(context.Background(), requestID, subscriptionID, amount); err != nil {
		t.Fatalf("PreConsumeUserSubscription: %v", err)
	}
	if _, err := db.Exec(
		`UPDATE api_tokens SET remain_quota = remain_quota - $2 WHERE id = $1`, tokenID, amount); err != nil {
		t.Fatalf("debit token: %v", err)
	}
}

// w9bAmountUsed 读取订阅已用额度。
func w9bAmountUsed(t *testing.T, db *sql.DB, subscriptionID int64) int64 {
	t.Helper()
	var used int64
	if err := db.QueryRow(`SELECT amount_used FROM subscriptions WHERE id = $1`, subscriptionID).
		Scan(&used); err != nil {
		t.Fatalf("read amount_used: %v", err)
	}
	return used
}

// w9bRegistryState 读取 request_registry.state（行缺失返回空串）。
func w9bRegistryState(t *testing.T, db *sql.DB, requestID string) string {
	t.Helper()
	var state string
	err := db.QueryRow(`SELECT state FROM request_registry WHERE request_id = $1`, requestID).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return ""
	}
	if err != nil {
		t.Fatalf("read registry state: %v", err)
	}
	return state
}

// w9bPreConsumeStatus 读取预扣记录状态。
func w9bPreConsumeStatus(t *testing.T, db *sql.DB, requestID string) string {
	t.Helper()
	var status string
	if err := db.QueryRow(
		`SELECT status FROM subscription_pre_consume_records WHERE request_id = $1`, requestID).
		Scan(&status); err != nil {
		t.Fatalf("read pre-consume status: %v", err)
	}
	return status
}

// w9bAgePreConsumeAndLease 把预扣记录与在飞租约一起回拨，模拟「请求已超过
// 兜底窗口且进程已崩溃」（R-W9B-02：两个条件必须同时满足，否则扫描会正确
// 地把持续续租的长请求当成在飞并跳过）。
func w9bAgePreConsumeAndLease(t *testing.T, db *sql.DB, requestID string) {
	t.Helper()
	if _, err := db.Exec(
		`UPDATE subscription_pre_consume_records SET created_at = now() - interval '1 hour'
		  WHERE request_id = $1`, requestID); err != nil {
		t.Fatalf("age pre-consume: %v", err)
	}
	if _, err := db.Exec(
		`UPDATE request_registry SET heartbeat_at = now() - interval '1 hour'
		  WHERE request_id = $1`, requestID); err != nil {
		t.Fatalf("age inflight lease: %v", err)
	}
}

// w9bCleanupRequest 删除本用例写入的 registry / 预扣 / 钱包流水。
func w9bCleanupRequest(t *testing.T, db *sql.DB, requestID string) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM request_registry WHERE request_id = $1`, requestID)
		_, _ = db.Exec(`DELETE FROM subscription_pre_consume_records WHERE request_id = $1`, requestID)
		_, _ = db.Exec(`DELETE FROM wallet_transactions WHERE reference_id = $1`, requestID)
	})
}

// w9bInstallRegistryFailure 在该 requestID 的 request_registry INSERT/UPDATE
// 前抛出异常，用来证明 registry 写失败会连同 delta 一起回滚。
func w9bInstallRegistryFailure(t *testing.T, db *sql.DB, requestID string) {
	t.Helper()
	w9bInstallFailureFunc(t, db)
	if _, err := db.Exec(fmt.Sprintf(`
		CREATE TRIGGER w9b_fail_registry_trg
		BEFORE INSERT OR UPDATE ON request_registry
		FOR EACH ROW WHEN (NEW.request_id = '%s'::uuid)
		EXECUTE FUNCTION w9b_raise_failure()`, requestID)); err != nil {
		t.Fatalf("install registry failure trigger: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DROP TRIGGER IF EXISTS w9b_fail_registry_trg ON request_registry`)
	})
}

// w9bInstallTokenFailure 在该 token 的 api_tokens UPDATE 前抛出异常，用来
// 证明令牌回补失败会连同钱包/订阅退款一起回滚（B3）。
func w9bInstallTokenFailure(t *testing.T, db *sql.DB, tokenID int64) {
	t.Helper()
	w9bInstallFailureFunc(t, db)
	if _, err := db.Exec(fmt.Sprintf(`
		CREATE TRIGGER w9b_fail_token_trg
		BEFORE UPDATE ON api_tokens
		FOR EACH ROW WHEN (NEW.id = %d)
		EXECUTE FUNCTION w9b_raise_failure()`, tokenID)); err != nil {
		t.Fatalf("install token failure trigger: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DROP TRIGGER IF EXISTS w9b_fail_token_trg ON api_tokens`)
	})
}

// w9bBlockSubscriptionRow 在独立连接上持有订阅行锁，让并发用例的两个执行者
// 都先通过 registry 预检、再一起阻塞在资金事务的行锁上。这样「终态 CAS」才是
// 唯一的仲裁者；去掉 CAS 时第二个事务会成功提交第二次 delta，用例必红。
func w9bBlockSubscriptionRow(t *testing.T, db *sql.DB, subscriptionID int64) (release func()) {
	t.Helper()
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("blocker conn: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `BEGIN`); err != nil {
		t.Fatalf("blocker begin: %v", err)
	}
	if _, err := conn.ExecContext(ctx,
		`SELECT id FROM subscriptions WHERE id = $1 FOR UPDATE`, subscriptionID); err != nil {
		t.Fatalf("blocker lock: %v", err)
	}
	return func() {
		_, _ = conn.ExecContext(context.Background(), `COMMIT`)
		_ = conn.Close()
	}
}

// w9bWaitForLockWaiters 等待恰好 want 个后台会话阻塞在行锁上（最多 5s）。
func w9bWaitForLockWaiters(t *testing.T, db *sql.DB, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := db.QueryRow(`
			SELECT count(*) FROM pg_stat_activity
			 WHERE datname = current_database()
			   AND wait_event_type = 'Lock'
			   AND state = 'active'`).Scan(&n); err != nil {
			t.Fatalf("count lock waiters: %v", err)
		}
		if n >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待 %d 个行锁等待者超时", want)
}

func w9bInstallFailureFunc(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`
		CREATE OR REPLACE FUNCTION w9b_raise_failure() RETURNS trigger AS $$
		BEGIN
			RAISE EXCEPTION 'w9b injected failure on %', TG_TABLE_NAME;
		END;
		$$ LANGUAGE plpgsql`); err != nil {
		t.Fatalf("create failure function: %v", err)
	}
}

// TestW9BSettlementRegistryFailureRollsBackDelta 覆盖 B1 的核心不变式：
// registry 写失败时 delta 不得单独提交。旧实现先提交 delta、再尽力写
// registry（失败只告警），会留下「已扣订阅、无终态」并被兜底扫描二次退款。
func TestW9BSettlementRegistryFailureRollsBackDelta(t *testing.T) {
	e, db, repo := w9bNewRealEngine(t)
	userID := createBillingTestUser(t, db, 500)
	tokenID := createBillingTestToken(t, db, userID, int64(500))
	sub := w9bCreateSubscription(t, db, repo, userID, 1000)
	req := testUUID(t)
	w9bCleanupRequest(t, db, req)
	w9bSeedSubscriptionPreConsume(t, db, repo, req, sub.ID, tokenID, 100)

	w9bInstallRegistryFailure(t, db, req)

	err := e.TerminateByRequestIDForTokenCost(req, tokenID, 60, contract.UsageFact{Model: "m"})
	if err == nil {
		t.Fatal("registry 写失败时 Terminate = nil，want error（delta 必须随事务回滚）")
	}
	if got := w9bAmountUsed(t, db, sub.ID); got != 100 {
		t.Fatalf("失败后 amount_used = %d，want 100（delta 不得单独提交）", got)
	}
	if got := w9bPreConsumeStatus(t, db, req); got != "consumed" {
		t.Fatalf("失败后预扣记录 = %q，want consumed（保留给兜底扫描）", got)
	}
	if got := w9bRegistryState(t, db, req); got != "in_flight" {
		t.Fatalf("失败后 registry = %q，want in_flight（R-W9B-02 预扣租约仍在，不得伪造终态）", got)
	}
	if got := tokenRemain(t, db, tokenID).Int64; got != 400 {
		t.Fatalf("失败后令牌剩余 = %d，want 400（结算未成功不得调整令牌）", got)
	}

	// 故障解除后重放必须完整成功——证明失败路径没有留下部分状态。
	if _, err := db.Exec(`DROP TRIGGER IF EXISTS w9b_fail_registry_trg ON request_registry`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}
	if err := e.TerminateByRequestIDForTokenCost(req, tokenID, 60, contract.UsageFact{Model: "m"}); err != nil {
		t.Fatalf("重放 Terminate: %v", err)
	}
	if got := w9bAmountUsed(t, db, sub.ID); got != 60 {
		t.Fatalf("重放后 amount_used = %d，want 60", got)
	}
	if got := w9bRegistryState(t, db, req); got != "settled" {
		t.Fatalf("重放后 registry = %q，want settled", got)
	}
	if got := tokenRemain(t, db, tokenID).Int64; got != 440 {
		t.Fatalf("重放后令牌剩余 = %d，want 440（回补 40）", got)
	}
}

// TestW9BConcurrentTerminatesSettleExactlyOnce 覆盖 B1 的恰好一次结算：
// 两个执行者（无 Redis 锁）同时结算同一 requestID，终态 CAS 必须让其中一个
// 整笔回滚，delta 与令牌差额都只落一次。
func TestW9BConcurrentTerminatesSettleExactlyOnce(t *testing.T) {
	for i := 0; i < 5; i++ {
		e, db, repo := w9bNewRealEngine(t)
		userID := createBillingTestUser(t, db, 500)
		tokenID := createBillingTestToken(t, db, userID, int64(500))
		sub := w9bCreateSubscription(t, db, repo, userID, 1000)
		req := testUUID(t)
		w9bCleanupRequest(t, db, req)
		w9bSeedSubscriptionPreConsume(t, db, repo, req, sub.ID, tokenID, 100)

		release := w9bBlockSubscriptionRow(t, db, sub.ID)
		start := make(chan struct{})
		errs := make([]error, 2)
		var wg sync.WaitGroup
		for j := 0; j < 2; j++ {
			wg.Add(1)
			go func(j int) {
				defer wg.Done()
				<-start
				errs[j] = e.TerminateByRequestIDForTokenCost(req, tokenID, 60, contract.UsageFact{Model: "m"})
			}(j)
		}
		close(start)
		w9bWaitForLockWaiters(t, db, 2)
		release()
		wg.Wait()

		for j, err := range errs {
			if err != nil {
				t.Fatalf("iteration %d: 并发结算 %d err = %v，want nil（幂等收敛）", i, j, err)
			}
		}
		if got := w9bAmountUsed(t, db, sub.ID); got != 60 {
			t.Fatalf("iteration %d: amount_used = %d，want 60（delta 只能落一次）", i, got)
		}
		if got := w9bRegistryState(t, db, req); got != "settled" {
			t.Fatalf("iteration %d: registry = %q，want settled", i, got)
		}
		if got := w9bPreConsumeStatus(t, db, req); got != "consumed" {
			t.Fatalf("iteration %d: 预扣记录 = %q，want consumed", i, got)
		}
		if got := tokenRemain(t, db, tokenID).Int64; got != 440 {
			t.Fatalf("iteration %d: 令牌剩余 = %d，want 440（差额只调整一次）", i, got)
		}
	}
}

// TestW9BConcurrentTerminateAndRecoveryExactlyOnce 覆盖 B1 的结算/退款并发：
// 实时结算与兜底扫描任意交错，只能得到单一终态。
func TestW9BConcurrentTerminateAndRecoveryExactlyOnce(t *testing.T) {
	now := time.Now().UTC()
	for i := 0; i < 5; i++ {
		e, db, repo := w9bNewRealEngine(t)
		userID := createBillingTestUser(t, db, 500)
		tokenID := createBillingTestToken(t, db, userID, int64(500))
		sub := w9bCreateSubscription(t, db, repo, userID, 1000)
		req := testUUID(t)
		w9bCleanupRequest(t, db, req)
		w9bSeedSubscriptionPreConsume(t, db, repo, req, sub.ID, tokenID, 100)
		// 让扫描看到这条预扣：记录年龄超过窗口，且租约心跳也过期
		// （R-W9B-02 的两个条件必须同时成立）。
		w9bAgePreConsumeAndLease(t, db, req)

		release := w9bBlockSubscriptionRow(t, db, sub.ID)
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
			termErr = e.TerminateByRequestIDForTokenCost(req, tokenID, 60, contract.UsageFact{Model: "m"})
		}()
		go func() {
			defer wg.Done()
			<-start
			_, recErr = e.RecoverStaleSubscriptionPreConsumes(context.Background(), now, 10)
		}()
		close(start)
		w9bWaitForLockWaiters(t, db, 2)
		release()
		wg.Wait()

		if termErr != nil {
			t.Fatalf("iteration %d: Terminate err = %v，want nil", i, termErr)
		}
		if recErr != nil {
			t.Fatalf("iteration %d: recovery err = %v，want nil", i, recErr)
		}

		used := w9bAmountUsed(t, db, sub.ID)
		state := w9bRegistryState(t, db, req)
		status := w9bPreConsumeStatus(t, db, req)
		settledOK := used == 60 && state == "settled" && status == "consumed"
		refundedOK := used == 0 && state == "refunded" && status == "refunded"
		if !settledOK && !refundedOK {
			t.Fatalf("iteration %d: 交错后出现混合状态：used=%d registry=%q record=%q",
				i, used, state, status)
		}
	}
}

// TestW9BLateRefundAfterSettleNoop 覆盖 B1 的迟到回调：结算成功后的退款入口
// 必须看到终态并 no-op，不得回退 amount_used 或重复回补令牌。
func TestW9BLateRefundAfterSettleNoop(t *testing.T) {
	e, db, repo := w9bNewRealEngine(t)
	userID := createBillingTestUser(t, db, 500)
	tokenID := createBillingTestToken(t, db, userID, int64(500))
	sub := w9bCreateSubscription(t, db, repo, userID, 1000)
	req := testUUID(t)
	w9bCleanupRequest(t, db, req)
	w9bSeedSubscriptionPreConsume(t, db, repo, req, sub.ID, tokenID, 100)

	if err := e.TerminateByRequestIDForTokenCost(req, tokenID, 60, contract.UsageFact{Model: "m"}); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if err := e.RefundByRequestIDForToken(req, tokenID); err != nil {
		t.Fatalf("结算后退款应幂等返回 nil: %v", err)
	}
	if got := w9bAmountUsed(t, db, sub.ID); got != 60 {
		t.Fatalf("迟到的退款改变了 amount_used = %d，want 60", got)
	}
	if got := w9bPreConsumeStatus(t, db, req); got != "consumed" {
		t.Fatalf("迟到的退款把预扣改为 %q，want consumed", got)
	}
	if got := w9bRegistryState(t, db, req); got != "settled" {
		t.Fatalf("迟到的退款把 registry 改为 %q，want settled（终态互斥）", got)
	}
	if got := tokenRemain(t, db, tokenID).Int64; got != 440 {
		t.Fatalf("迟到的退款把令牌改为 %d，want 440（不得重复回补）", got)
	}
}

// TestW9BLateTerminateAfterRefundNoop 覆盖 B1 的迟到结算方向：兜底扫描已把
// 预扣退款（registry refunded）后，任务轮询/重启重放的 Terminate 必须 no-op，
// 不得再落 delta（多扣）或把记录改回 consumed。
func TestW9BLateTerminateAfterRefundNoop(t *testing.T) {
	e, db, repo := w9bNewRealEngine(t)
	userID := createBillingTestUser(t, db, 500)
	tokenID := createBillingTestToken(t, db, userID, int64(500))
	sub := w9bCreateSubscription(t, db, repo, userID, 1000)
	req := testUUID(t)
	w9bCleanupRequest(t, db, req)
	w9bSeedSubscriptionPreConsume(t, db, repo, req, sub.ID, tokenID, 100)
	// 预扣超过窗口且租约心跳已过期（R-W9B-02），扫描才会接管。
	w9bAgePreConsumeAndLease(t, db, req)

	recovered, err := e.RecoverStaleSubscriptionPreConsumes(context.Background(), time.Now().UTC(), 10)
	if err != nil || recovered != 1 {
		t.Fatalf("recovery = %d/%v，want 1/nil", recovered, err)
	}
	if got := w9bRegistryState(t, db, req); got != "refunded" {
		t.Fatalf("扫描后 registry = %q，want refunded", got)
	}

	// 迟到的任务成功回调：预扣已退款，不得再结算。
	if err := e.TerminateByRequestIDForTokenCost(req, tokenID, 60, contract.UsageFact{Model: "m"}); err != nil {
		t.Fatalf("迟到 Terminate 应幂等返回 nil: %v", err)
	}
	if got := w9bAmountUsed(t, db, sub.ID); got != 0 {
		t.Fatalf("迟到 Terminate 后 amount_used = %d，want 0（不得二次扣费）", got)
	}
	if got := w9bPreConsumeStatus(t, db, req); got != "refunded" {
		t.Fatalf("迟到 Terminate 把预扣改为 %q，want refunded", got)
	}
	if got := w9bRegistryState(t, db, req); got != "refunded" {
		t.Fatalf("迟到 Terminate 把 registry 改为 %q，want refunded", got)
	}
	if got := tokenRemain(t, db, tokenID).Int64; got != 400 {
		t.Fatalf("迟到 Terminate 调整了令牌 = %d，want 400", got)
	}
}

// TestW9BWalletRefundTokenCreditAtomic 覆盖 B3 的钱包路径：令牌回补失败时
// 钱包退款必须整笔回滚（hold 仍开放、余额不变），故障解除后可完整重放。
func TestW9BWalletRefundTokenCreditAtomic(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)
	userID := createBillingTestUser(t, db, 500)
	tokenID := createBillingTestToken(t, db, userID, int64(500))
	req := testRequestID("w9b-wallet-atomic")
	w9bCleanupRequest(t, db, req)

	s := newBillingTestSessionWithToken(e, userID, tokenID, req, perCallSnapshot(100))
	if err := s.PreConsume(contract.QuotaEstimate{Model: "m"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if err := s.ChargeIncrement(30, contract.UsageFact{Model: "m"}); err != nil {
		t.Fatalf("ChargeIncrement: %v", err)
	}
	if got := userBalance(t, db, userID); got != 370 {
		t.Fatalf("退款前余额 = %d，want 370（预扣 100 + 增量 30）", got)
	}
	if got := tokenRemain(t, db, tokenID).Int64; got != 370 {
		t.Fatalf("退款前令牌 = %d，want 370", got)
	}

	w9bInstallTokenFailure(t, db, tokenID)

	if err := s.Refund(); err == nil {
		t.Fatal("令牌回补失败时 Refund = nil，want error（钱包退款必须回滚）")
	}
	if got := userBalance(t, db, userID); got != 370 {
		t.Fatalf("回滚后余额 = %d，want 370（钱包不得单独退款）", got)
	}
	if got := tokenRemain(t, db, tokenID).Int64; got != 370 {
		t.Fatalf("回滚后令牌 = %d，want 370", got)
	}
	if rows := readWalletRows(t, db, req); !w9bHasHoldOnly(rows) {
		t.Fatalf("回滚后 hold 行 = %+v，want consume_pre 仍为 hold 且无 release", rows)
	}
	if s.refunded {
		t.Fatal("回滚后会话 refunded = true，want false（可重试）")
	}

	// 故障解除后重放：钱包与令牌各只退一次。
	if _, err := db.Exec(`DROP TRIGGER IF EXISTS w9b_fail_token_trg ON api_tokens`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}
	if err := s.Refund(); err != nil {
		t.Fatalf("重放 Refund: %v", err)
	}
	if got := userBalance(t, db, userID); got != 500 {
		t.Fatalf("重放后余额 = %d，want 500", got)
	}
	if got := tokenRemain(t, db, tokenID).Int64; got != 500 {
		t.Fatalf("重放后令牌 = %d，want 500", got)
	}
	rows := readWalletRows(t, db, req)
	if n := w9bCountRelease(rows); n != 1 {
		t.Fatalf("重放后钱包流水 = %+v，release 行 = %d，want 恰好 1（幂等）", rows, n)
	}
}

// w9bHasHoldOnly 判断钱包流水里仍有未关闭的预扣行、且没有 release 行。
func w9bHasHoldOnly(rows []walletRow) bool {
	hold, release := false, false
	for _, row := range rows {
		switch {
		case row.Type == "consume_pre" && row.Status == "hold":
			hold = true
		case row.Type == "release":
			release = true
		}
	}
	return hold && !release
}

// w9bCountRelease 返回钱包流水中的 release 行数（release 部分唯一索引保证 <=1）。
func w9bCountRelease(rows []walletRow) int {
	n := 0
	for _, row := range rows {
		if row.Type == "release" {
			n++
		}
	}
	return n
}

// TestW9BSubscriptionRefundTokenCreditAtomic 覆盖 B3 的订阅路径：令牌回补
// 失败时 amount_used / 预扣状态 / registry 全部回滚，故障解除后可完整重放。
func TestW9BSubscriptionRefundTokenCreditAtomic(t *testing.T) {
	e, db, repo := w9bNewRealEngine(t)
	userID := createBillingTestUser(t, db, 500)
	tokenID := createBillingTestToken(t, db, userID, int64(500))
	sub := w9bCreateSubscription(t, db, repo, userID, 1000)
	req := testUUID(t)
	w9bCleanupRequest(t, db, req)

	s := newBillingTestSessionWithToken(e, userID, tokenID, req, perCallSnapshot(100))
	if err := s.PreConsume(contract.QuotaEstimate{Model: "m"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if err := s.ChargeIncrement(30, contract.UsageFact{Model: "m"}); err != nil {
		t.Fatalf("ChargeIncrement: %v", err)
	}
	if got := w9bAmountUsed(t, db, sub.ID); got != 130 {
		t.Fatalf("退款前 amount_used = %d，want 130", got)
	}
	if got := tokenRemain(t, db, tokenID).Int64; got != 370 {
		t.Fatalf("退款前令牌 = %d，want 370", got)
	}

	w9bInstallTokenFailure(t, db, tokenID)

	if err := s.Refund(); err == nil {
		t.Fatal("令牌回补失败时 Refund = nil，want error（订阅退款必须回滚）")
	}
	if got := w9bAmountUsed(t, db, sub.ID); got != 130 {
		t.Fatalf("回滚后 amount_used = %d，want 130（含增量）", got)
	}
	if got := w9bPreConsumeStatus(t, db, req); got != "consumed" {
		t.Fatalf("回滚后预扣记录 = %q，want consumed", got)
	}
	if got := w9bRegistryState(t, db, req); got != "in_flight" {
		t.Fatalf("回滚后 registry = %q，want in_flight（预扣租约仍在，不得伪造终态）", got)
	}
	if got := tokenRemain(t, db, tokenID).Int64; got != 370 {
		t.Fatalf("回滚后令牌 = %d，want 370", got)
	}

	// 故障解除后重放：订阅额度、预扣、registry、令牌一次收敛。
	if _, err := db.Exec(`DROP TRIGGER IF EXISTS w9b_fail_token_trg ON api_tokens`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}
	if err := s.Refund(); err != nil {
		t.Fatalf("重放 Refund: %v", err)
	}
	if got := w9bAmountUsed(t, db, sub.ID); got != 0 {
		t.Fatalf("重放后 amount_used = %d，want 0", got)
	}
	if got := w9bPreConsumeStatus(t, db, req); got != "refunded" {
		t.Fatalf("重放后预扣记录 = %q，want refunded", got)
	}
	if got := w9bRegistryState(t, db, req); got != "refunded" {
		t.Fatalf("重放后 registry = %q，want refunded", got)
	}
	if got := tokenRemain(t, db, tokenID).Int64; got != 500 {
		t.Fatalf("重放后令牌 = %d，want 500", got)
	}

	// 重复 Refund 幂等：不得二次回补。
	if err := s.Refund(); err != nil {
		t.Fatalf("重复 Refund: %v", err)
	}
	if got := tokenRemain(t, db, tokenID).Int64; got != 500 {
		t.Fatalf("重复退款后令牌 = %d，want 500（只回补一次）", got)
	}
	if got := w9bAmountUsed(t, db, sub.ID); got != 0 {
		t.Fatalf("重复退款后 amount_used = %d，want 0", got)
	}
}
