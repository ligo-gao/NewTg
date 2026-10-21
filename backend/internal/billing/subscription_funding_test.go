package billing

// S4a 订阅资金来源的验收用例（billing-04 §3/§4、billing-08 §4）。
//
// 两层：
//   - 假仓储（本文件 fake）验证四偏好矩阵、model_scope、trusted 顺序与
//     令牌回滚，不依赖订阅表；
//   - 生产适配器 + 真 PG 验证幂等/退款（store 层事务语义由
//     store/subscription_repo_integration_test.go 覆盖）。
//
// 与既有 billing 测试一致：未设置 NEWTG_TEST_DB_DSN 时 t.Skip。

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/store"
)

// testUUID 生成 RFC 4122 v4 UUID（subscription_pre_consume_records.request_id
// 是 PG uuid 列；billing 的 testRequestID 是 varchar 语义，不能用于真实仓储）。
func testUUID(t *testing.T) string {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("crypto/rand: %v", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// fakeSubscriptionFunding 是 SubscriptionFundingSource 的内存实现。
// 语义对齐 store：同 requestId 幂等、已退款再预扣报错、上界守卫。
type fakeSubscriptionFunding struct {
	preference contract.BillingPreference
	subs       []SubscriptionAccount
	listErr    error
	prefErr    error

	mu              sync.Mutex
	used            map[int64]int64
	records         map[string]store.SubscriptionPreConsume
	owners          map[string]int64
	preConsumeCalls int
	attempts        []int64
	markAttempts    int
	prefCalls       int
	listCalls       int
	// markFailures 是前 N 次退款调用返回错误（验证 3 次退避重试）。
	markFailures int
	// postDeltaCalls 记录结算增量；postDeltaErr 非 nil 时直接返回。
	postDeltaCalls []subscriptionDeltaCall
	postDeltaErr   error
	// settleErr 模拟「delta + registry 同事务」整体失败（B1 回滚用例）。
	settleErr    error
	listStaleErr error
	registry     map[string]string
	// registryCalls / registryHook 供并发用例精确控制「锁外读 registry」与
	// 「锁内复核」的交错；hook 在释放 f.mu 后调用，避免与其它方法死锁。
	registryCalls int
	registryHook  func(requestID string, call int)
	// recordCalls / recordHook 同理，控制 RefundByRequestIDForToken 在取锁前
	// 「识别订阅来源」与取锁后「第二道复核」之间的交错（H1 回归用例）。
	recordCalls int
	recordHook  func(requestID string, call int)
}

// subscriptionDeltaCall 是一次结算增量调用（断言用）。
type subscriptionDeltaCall struct {
	subscriptionID int64
	delta          int64
}

func newFakeSubscriptionFunding(pref contract.BillingPreference, subs ...SubscriptionAccount) *fakeSubscriptionFunding {
	return &fakeSubscriptionFunding{
		preference: pref,
		subs:       subs,
		used:       map[int64]int64{},
		records:    map[string]store.SubscriptionPreConsume{},
		owners:     map[string]int64{},
		registry:   map[string]string{},
	}
}

func (f *fakeSubscriptionFunding) BillingPreference(context.Context, int64) (contract.BillingPreference, error) {
	f.mu.Lock()
	f.prefCalls++
	f.mu.Unlock()
	if f.prefErr != nil {
		return 0, f.prefErr
	}
	return f.preference, nil
}

func (f *fakeSubscriptionFunding) ListActiveSubscriptions(context.Context, int64, time.Time) ([]SubscriptionAccount, error) {
	f.mu.Lock()
	f.listCalls++
	f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]SubscriptionAccount(nil), f.subs...), nil
}

func (f *fakeSubscriptionFunding) PreConsumeUserSubscription(_ context.Context,
	requestID string, subscriptionID, amount int64) (store.SubscriptionPreConsume, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.preConsumeCalls++
	f.attempts = append(f.attempts, subscriptionID)
	if rec, ok := f.records[requestID]; ok {
		if rec.Status == "refunded" {
			return store.SubscriptionPreConsume{}, false, store.ErrPreConsumeRefunded
		}
		return rec, false, nil
	}
	for _, sub := range f.subs {
		if sub.ID != subscriptionID {
			continue
		}
		if sub.AmountTotal > 0 && f.used[sub.ID]+amount > sub.AmountTotal {
			return store.SubscriptionPreConsume{}, false, store.ErrSubscriptionQuotaInsufficient
		}
	}
	rec := store.SubscriptionPreConsume{
		RequestID: requestID, SubscriptionID: subscriptionID,
		Status: "consumed", PreConsumed: amount,
	}
	f.records[requestID] = rec
	f.used[subscriptionID] += amount
	return rec, true, nil
}

func (f *fakeSubscriptionFunding) MarkPreConsumeRefunded(_ context.Context, requestID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.markAttempts++
	if f.markAttempts <= f.markFailures {
		return errors.New("fake: transient refund failure")
	}
	rec, ok := f.records[requestID]
	if !ok {
		return store.ErrPreConsumeNotFound
	}
	if rec.Status == "refunded" {
		return nil
	}
	rec.Status = "refunded"
	f.records[requestID] = rec
	// 镜像 store 的 `amount_used >= pre_consumed` 守卫（G-DOM-32）：周期
	// 重置后不命中时保持新周期用量不变，绝不扣成负数。
	if f.used[rec.SubscriptionID] >= rec.PreConsumed {
		f.used[rec.SubscriptionID] -= rec.PreConsumed
	}
	return nil
}

func (f *fakeSubscriptionFunding) PreConsumeRecord(_ context.Context, requestID string) (store.SubscriptionPreConsume, int64, error) {
	f.mu.Lock()
	f.recordCalls++
	call := f.recordCalls
	hook := f.recordHook
	rec, ok := f.records[requestID]
	owner := f.owners[requestID]
	f.mu.Unlock()
	if hook != nil {
		hook(requestID, call)
	}
	if !ok {
		return store.SubscriptionPreConsume{}, 0, store.ErrPreConsumeNotFound
	}
	return rec, owner, nil
}

// PostConsumeUserSubscriptionDelta 按 store 语义实现：下界 0、上界拒绝、
// delta=0 仍返回快照。
func (f *fakeSubscriptionFunding) PostConsumeUserSubscriptionDelta(_ context.Context,
	subscriptionID, delta int64) (SubscriptionSettlement, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.postDeltaCalls = append(f.postDeltaCalls, subscriptionDeltaCall{subscriptionID, delta})
	if f.postDeltaErr != nil {
		return SubscriptionSettlement{}, f.postDeltaErr
	}
	return f.applyDeltaLocked(subscriptionID, delta)
}

// RefundSubscriptionIncrement 镜像 store 的 `amount_used >= amount` 守卫（M3）。
func (f *fakeSubscriptionFunding) RefundSubscriptionIncrement(_ context.Context,
	subscriptionID, amount int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if amount <= 0 || f.used[subscriptionID] < amount {
		return false, nil
	}
	f.used[subscriptionID] -= amount
	return true, nil
}

// SettleSubscriptionDelta 镜像 B1 的终态 CAS：registry 已有终态时 applied=false
// 且不改额度；settleErr 模拟整个事务失败（不回滚模拟，因为假仓储没有事务）。
func (f *fakeSubscriptionFunding) SettleSubscriptionDelta(_ context.Context,
	requestID string, _ int64, subscriptionID, delta int64) (SubscriptionSettlement, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.postDeltaCalls = append(f.postDeltaCalls, subscriptionDeltaCall{subscriptionID, delta})
	if f.settleErr != nil {
		return SubscriptionSettlement{}, false, f.settleErr
	}
	if st := f.registry[requestID]; st == "settled" || st == "refunded" {
		return SubscriptionSettlement{}, false, nil
	}
	settlement, err := f.applyDeltaLocked(subscriptionID, delta)
	if err != nil {
		return SubscriptionSettlement{}, false, err
	}
	f.registry[requestID] = "settled"
	return settlement, true, nil
}

// RefundSubscriptionRequest 镜像 B3 的原子退款语义（假仓储不覆盖事务/令牌；
// TokenCredited=false 让会话走 best-effort CreditToken，保持旧测试口径）。
func (f *fakeSubscriptionFunding) RefundSubscriptionRequest(_ context.Context,
	in store.SubscriptionRefund) (store.SubscriptionRefundResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.markAttempts++
	if f.markAttempts <= f.markFailures {
		return store.SubscriptionRefundResult{}, errors.New("fake: transient refund failure")
	}
	rec, ok := f.records[in.RequestID]
	if !ok {
		return store.SubscriptionRefundResult{}, store.ErrPreConsumeNotFound
	}
	if rec.Status == "refunded" {
		return store.SubscriptionRefundResult{Terminal: "refunded"}, nil
	}
	if f.registry[in.RequestID] == "settled" {
		return store.SubscriptionRefundResult{Terminal: "settled"}, nil
	}
	result := store.SubscriptionRefundResult{Applied: true, TokenCredited: false}
	if in.Consumed > 0 && f.used[rec.SubscriptionID] >= in.Consumed {
		f.used[rec.SubscriptionID] -= in.Consumed
		result.IncrementReversed = true
	}
	if f.used[rec.SubscriptionID] >= rec.PreConsumed {
		f.used[rec.SubscriptionID] -= rec.PreConsumed
	}
	rec.Status = "refunded"
	f.records[in.RequestID] = rec
	f.registry[in.RequestID] = "refunded"
	return result, nil
}

// applyDeltaLocked 是 PostConsume 与 Settle 共享的额度增量语义。
func (f *fakeSubscriptionFunding) applyDeltaLocked(subscriptionID, delta int64) (SubscriptionSettlement, error) {
	var sub *SubscriptionAccount
	for i := range f.subs {
		if f.subs[i].ID == subscriptionID {
			sub = &f.subs[i]
			break
		}
	}
	if sub == nil {
		return SubscriptionSettlement{}, store.ErrSubscriptionNotFound
	}
	used := f.used[subscriptionID] + delta
	if used < 0 {
		used = 0
	}
	if sub.AmountTotal > 0 && used > sub.AmountTotal {
		return SubscriptionSettlement{}, store.ErrSubscriptionSettleExceeded
	}
	f.used[subscriptionID] = used
	return SubscriptionSettlement{
		PlanID:      sub.PlanID,
		PlanTitle:   sub.PlanTitle,
		AmountTotal: sub.AmountTotal,
		AmountUsed:  used,
	}, nil
}

// ListStalePreConsumes 返回 consumed 且 created_at 早于 before 的记录。
func (f *fakeSubscriptionFunding) ListStalePreConsumes(_ context.Context,
	before time.Time, limit int) ([]store.SubscriptionPreConsume, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listStaleErr != nil {
		return nil, f.listStaleErr
	}
	out := make([]store.SubscriptionPreConsume, 0, len(f.records))
	for _, rec := range f.records {
		if rec.Status != "consumed" || !rec.CreatedAt.Before(before) {
			continue
		}
		out = append(out, rec)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// deltas 返回已记录的结算增量调用。
func (f *fakeSubscriptionFunding) deltas() []subscriptionDeltaCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]subscriptionDeltaCall(nil), f.postDeltaCalls...)
}

// RequestRegistryState 返回内存中的结算终态（缺失 = 空串）。
//
// registryHook 在释放 f.mu 之后调用，供并发用例在「读到未结算」与「锁内
// 复核」之间注入确定性交错。
func (f *fakeSubscriptionFunding) RequestRegistryState(_ context.Context, requestID string) (string, error) {
	f.mu.Lock()
	f.registryCalls++
	call := f.registryCalls
	hook := f.registryHook
	state := f.registry[requestID]
	f.mu.Unlock()
	if hook != nil {
		hook(requestID, call)
	}
	return state, nil
}

// MarkRequestSettled 幂等置 settled；已 refunded 不降级（审计接缝，B1 后
// 正确性由 SettleSubscriptionDelta 的同事务 CAS 保证）。
func (f *fakeSubscriptionFunding) MarkRequestSettled(_ context.Context, requestID string, _ int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.registry[requestID] == "refunded" {
		return nil
	}
	f.registry[requestID] = "settled"
	return nil
}

// MarkRequestRefunded 幂等置 refunded；已 settled 不降级。
func (f *fakeSubscriptionFunding) MarkRequestRefunded(_ context.Context, requestID string, _ int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.registry[requestID] == "settled" {
		return nil
	}
	f.registry[requestID] = "refunded"
	return nil
}

func (f *fakeSubscriptionFunding) usedOf(id int64) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.used[id]
}

// setUsed 直接改写订阅已用量（模拟周期重置/新周期使用）。
func (f *fakeSubscriptionFunding) setUsed(id, value int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.used[id] = value
}

// registryCallCount 返回 RequestRegistryState 的调用次数（M2 缓存断言）。
func (f *fakeSubscriptionFunding) registryCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.registryCalls
}

// recordStatus 返回预扣记录状态。
func (f *fakeSubscriptionFunding) recordStatus(requestID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.records[requestID].Status
}

func (f *fakeSubscriptionFunding) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.preConsumeCalls
}

func (f *fakeSubscriptionFunding) attemptedIDs() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.attempts...)
}

func (f *fakeSubscriptionFunding) resolveCounts() (pref, list int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.prefCalls, f.listCalls
}

// subscriptionAccount 构造一个活跃、额度充足的订阅候选。
func subscriptionAccount(id, total int64, scope []byte) SubscriptionAccount {
	return SubscriptionAccount{
		ID: id, EndAt: time.Now().Add(time.Hour), AmountTotal: total,
		PlanID: 100 + id, PlanTitle: "Plan", ModelScope: scope,
	}
}

func newSubscriptionEngine(t *testing.T, funding SubscriptionFundingSource) (*Engine, *sql.DB) {
	t.Helper()
	gormDB, db := openBillingTestDB(t)
	e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)
	e.SetSubscriptionFunding(funding)
	return e, db
}

// TestSessionSubscriptionFirstUsesSubscription 覆盖：有活跃订阅且额度足时
// 从订阅扣，钱包不动；令牌按同一金额真实扣减（S4a 的「先令牌、后订阅」）。
func TestSessionSubscriptionFirstUsesSubscription(t *testing.T) {
	funding := newFakeSubscriptionFunding(contract.PrefSubscriptionFirst, subscriptionAccount(7, 1000, nil))
	e, db := newSubscriptionEngine(t, funding)
	userID := createBillingTestUser(t, db, 500)
	tokenID := createBillingTestToken(t, db, userID, int64(500))
	s := newBillingTestSessionWithToken(e, userID, tokenID, testRequestID("s4a-sub"), perCallSnapshot(100))

	if err := s.PreConsume(contract.QuotaEstimate{Model: "m"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if s.fundingSource != contract.FundingSubscription || s.subscriptionID != 7 || s.preConsumed != 100 {
		t.Fatalf("冻结来源 = %v/%d/%d，want subscription/7/100",
			s.fundingSource, s.subscriptionID, s.preConsumed)
	}
	if s.tokenDeducted != 100 {
		t.Fatalf("tokenDeducted = %d，want 100", s.tokenDeducted)
	}
	if got := funding.usedOf(7); got != 100 {
		t.Fatalf("订阅 amount_used = %d，want 100", got)
	}
	if got := userBalance(t, db, userID); got != 500 {
		t.Fatalf("钱包余额 = %d，want 500（订阅来源不得扣钱包）", got)
	}
	if got := tokenRemain(t, db, tokenID).Int64; got != 400 {
		t.Fatalf("令牌剩余 = %d，want 400（订阅预扣先扣令牌额度）", got)
	}
}

// TestSessionSubscriptionFirstFallsBackOnlyWithOverflow 覆盖溢出开关的
// 不对称性：开关关 → 402 且不扣钱包；开关开 → 回落钱包。
func TestSessionSubscriptionFirstFallsBackOnlyWithOverflow(t *testing.T) {
	tests := []struct {
		name        string
		overflow    bool
		wantErr     bool
		wantBalance int64
	}{
		{name: "订阅耗尽+禁止溢出：不回退", overflow: false, wantErr: true, wantBalance: 500},
		{name: "订阅耗尽+允许溢出：回退钱包", overflow: true, wantErr: false, wantBalance: 400},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// 订阅总额 50 < 预算 100：单实例额度不足。
			sub := subscriptionAccount(7, 50, nil)
			sub.AllowWalletOverflow = tc.overflow
			funding := newFakeSubscriptionFunding(contract.PrefSubscriptionFirst, sub)
			e, db := newSubscriptionEngine(t, funding)
			userID := createBillingTestUser(t, db, 500)
			s := newBillingTestSession(e, userID, testRequestID("s4a-overflow"), perCallSnapshot(100))

			err := s.PreConsume(contract.QuotaEstimate{Model: "m"})
			if tc.wantErr {
				if !errors.Is(err, ErrInsufficientQuota) {
					t.Fatalf("err = %v，want ErrInsufficientQuota", err)
				}
				if s.preConsumed != 0 {
					t.Fatalf("失败后 preConsumed = %d，want 0", s.preConsumed)
				}
			} else if err != nil {
				t.Fatalf("PreConsume: %v", err)
			}
			if got := userBalance(t, db, userID); got != tc.wantBalance {
				t.Fatalf("钱包余额 = %d，want %d", got, tc.wantBalance)
			}
		})
	}
}

// TestSessionSubscriptionOnlyNeverFallsBack 覆盖 subscription_only：
// 无候选或耗尽都返回 402，不扣钱包。
func TestSessionSubscriptionOnlyNeverFallsBack(t *testing.T) {
	tests := []struct {
		name string
		subs []SubscriptionAccount
	}{
		{name: "无可用订阅"},
		{name: "订阅额度不足", subs: []SubscriptionAccount{subscriptionAccount(7, 50, nil)}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			funding := newFakeSubscriptionFunding(contract.PrefSubscriptionOnly, tc.subs...)
			e, db := newSubscriptionEngine(t, funding)
			userID := createBillingTestUser(t, db, 500)
			s := newBillingTestSession(e, userID, testRequestID("s4a-subonly"), perCallSnapshot(100))

			err := s.PreConsume(contract.QuotaEstimate{Model: "m"})
			if !errors.Is(err, ErrInsufficientQuota) {
				t.Fatalf("err = %v，want ErrInsufficientQuota", err)
			}
			if got := userBalance(t, db, userID); got != 500 {
				t.Fatalf("钱包余额 = %d，want 500（不得回退钱包）", got)
			}
		})
	}
}

// TestSessionWalletFirstFallsBackOnInsufficientOnly 覆盖 wallet_first：
// 钱包额度不足时回退订阅；非额度错误（用户行不存在）绝不回退。
func TestSessionWalletFirstFallsBackOnInsufficientOnly(t *testing.T) {
	t.Run("钱包不足回退订阅", func(t *testing.T) {
		funding := newFakeSubscriptionFunding(contract.PrefWalletFirst, subscriptionAccount(7, 1000, nil))
		e, db := newSubscriptionEngine(t, funding)
		userID := createBillingTestUser(t, db, 50)
		s := newBillingTestSession(e, userID, testRequestID("s4a-walletfirst"), perCallSnapshot(100))

		if err := s.PreConsume(contract.QuotaEstimate{Model: "m"}); err != nil {
			t.Fatalf("PreConsume: %v", err)
		}
		if s.fundingSource != contract.FundingSubscription || s.subscriptionID != 7 {
			t.Fatalf("来源 = %v/%d，want subscription/7", s.fundingSource, s.subscriptionID)
		}
		if got := funding.usedOf(7); got != 100 {
			t.Fatalf("订阅 amount_used = %d，want 100", got)
		}
		if got := userBalance(t, db, userID); got != 50 {
			t.Fatalf("钱包余额 = %d，want 50", got)
		}
	})

	t.Run("钱包非额度错误不回退", func(t *testing.T) {
		funding := newFakeSubscriptionFunding(contract.PrefWalletFirst, subscriptionAccount(7, 1000, nil))
		e, db := newSubscriptionEngine(t, funding)
		userID := createBillingTestUser(t, db, 500)
		// 删除用户行：钱包更新命中 0 行，classifyBalanceMiss 返回
		// ErrUserNotFound（非额度错误）。回退订阅会掩盖该数据不一致。
		if _, err := db.Exec(`DELETE FROM users WHERE id = $1`, userID); err != nil {
			t.Fatalf("delete user: %v", err)
		}
		s := newBillingTestSession(e, userID, testRequestID("s4a-walletdb"), perCallSnapshot(100))

		err := s.PreConsume(contract.QuotaEstimate{Model: "m"})
		if !errors.Is(err, ErrUserNotFound) {
			t.Fatalf("err = %v，want ErrUserNotFound", err)
		}
		if funding.calls() != 0 {
			t.Fatalf("订阅预扣调用 = %d，want 0（非额度错误不得回退）", funding.calls())
		}
	})
}

// TestSessionSubscriptionTokenRollback 覆盖「先令牌、后订阅」的补偿：
// 订阅预扣失败必须把令牌额度原额退回，钱包与订阅额度都不变。
func TestSessionSubscriptionTokenRollback(t *testing.T) {
	funding := newFakeSubscriptionFunding(contract.PrefSubscriptionOnly, subscriptionAccount(7, 50, nil))
	e, db := newSubscriptionEngine(t, funding)
	userID := createBillingTestUser(t, db, 500)
	tokenID := createBillingTestToken(t, db, userID, int64(300))
	s := newBillingTestSessionWithToken(e, userID, tokenID, testRequestID("s4a-tokenrb"), perCallSnapshot(100))

	err := s.PreConsume(contract.QuotaEstimate{Model: "m"})
	if !errors.Is(err, ErrInsufficientQuota) {
		t.Fatalf("err = %v，want ErrInsufficientQuota", err)
	}
	if got := tokenRemain(t, db, tokenID).Int64; got != 300 {
		t.Fatalf("令牌剩余 = %d，want 300（失败必须回滚令牌）", got)
	}
	if got := funding.usedOf(7); got != 0 {
		t.Fatalf("订阅 amount_used = %d，want 0", got)
	}
	if got := userBalance(t, db, userID); got != 500 {
		t.Fatalf("钱包余额 = %d，want 500", got)
	}
}

// TestSessionSubscriptionModelScope 覆盖 model_scope 的三种形态：
// NULL=全部、[]=无模型、精确/前缀匹配。不匹配的订阅不作为候选。
func TestSessionSubscriptionModelScope(t *testing.T) {
	tests := []struct {
		name    string
		scope   []byte
		wantSub bool
	}{
		{name: "NULL 全部模型", scope: nil, wantSub: true},
		{name: "空数组无模型", scope: []byte(`[]`), wantSub: false},
		{name: "精确匹配", scope: []byte(`["m"]`), wantSub: true},
		{name: "前缀匹配", scope: []byte(`["openai/*"]`), wantSub: false},
		{name: "不含当前模型", scope: []byte(`["other/*"]`), wantSub: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			funding := newFakeSubscriptionFunding(contract.PrefSubscriptionFirst,
				subscriptionAccount(7, 1000, tc.scope))
			// M1：有活跃订阅但 scope 不匹配时按订阅失败处理，只有允许溢出才
			// 回退钱包。这里显式开溢出，让本用例专注 scope 过滤本身。
			funding.subs[0].AllowWalletOverflow = true
			e, db := newSubscriptionEngine(t, funding)
			userID := createBillingTestUser(t, db, 500)
			// s.model 取自 est.Model = "m"。
			s := newBillingTestSession(e, userID, testRequestID("s4a-scope"), perCallSnapshot(100))

			if err := s.PreConsume(contract.QuotaEstimate{Model: "m"}); err != nil {
				t.Fatalf("PreConsume: %v", err)
			}
			if tc.wantSub {
				if s.fundingSource != contract.FundingSubscription {
					t.Fatalf("来源 = %v，want subscription", s.fundingSource)
				}
			} else {
				if s.fundingSource != contract.FundingWallet {
					t.Fatalf("来源 = %v，want wallet（不匹配的订阅不可用）", s.fundingSource)
				}
				if got := userBalance(t, db, userID); got != 400 {
					t.Fatalf("钱包余额 = %d，want 400", got)
				}
			}
		})
	}
}

// TestModelScopePrefixMatching 是 modelScopeAllows 的纯函数用例，覆盖
// 注释冻结的两种形态，避免只通过计费路径间接验证。
func TestModelScopePrefixMatching(t *testing.T) {
	tests := []struct {
		scope []byte
		model string
		want  bool
	}{
		{nil, "anything", true},
		{[]byte(`[]`), "anything", false},
		{[]byte(`["openai/gpt-4o"]`), "openai/gpt-4o", true},
		{[]byte(`["openai/*"]`), "openai/gpt-4o", true},
		{[]byte(`["openai/*"]`), "azure/gpt-4o", false},
		{[]byte(`["*"]`), "openai/gpt-4o", false}, // 只认末尾 "/*"，不认裸 "*"
	}
	for _, tc := range tests {
		got, err := modelScopeAllows(tc.scope, tc.model)
		if err != nil {
			t.Fatalf("modelScopeAllows(%s, %q): %v", tc.scope, tc.model, err)
		}
		if got != tc.want {
			t.Fatalf("modelScopeAllows(%s, %q) = %v，want %v", tc.scope, tc.model, got, tc.want)
		}
	}
	if _, err := modelScopeAllows([]byte(`{bad`), "m"); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("非法 JSON err = %v，want ErrInvalidConfig", err)
	}
}

// TestSessionSubscriptionNeverTrusted 覆盖 trusted 与来源选择的正交性：
// 钱包侧命中旁路时，订阅主来源仍必须真实扣订阅、不得置 trusted。
func TestSessionSubscriptionNeverTrusted(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	funding := newFakeSubscriptionFunding(contract.PrefSubscriptionFirst, subscriptionAccount(7, 1000, nil))
	// 阈值 0.0001 USD × 默认 QPU 500000 = 50 积分；用户余额 500 远超阈值，
	// 若走钱包会命中旁路。订阅来源必须无视该旁路。
	e := NewEngine(gormDB, nil, nil, mapConfig{
		ConfigPreConsumeRatio: json.RawMessage("1.0"),
		ConfigTrustQuotaUSD:   json.RawMessage("0.0001"),
	}, nil, nil, nil)
	e.SetSubscriptionFunding(funding)
	userID := createBillingTestUser(t, db, 500)
	s := newBillingTestSession(e, userID, testRequestID("s4a-trusted"), perCallSnapshot(100))

	if err := s.PreConsume(contract.QuotaEstimate{Model: "m"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if s.trusted {
		t.Fatal("trusted = true，want false（订阅来源永不旁路）")
	}
	if s.fundingSource != contract.FundingSubscription {
		t.Fatalf("来源 = %v，want subscription", s.fundingSource)
	}
	if got := funding.usedOf(7); got != 100 {
		t.Fatalf("订阅 amount_used = %d，want 100（必须真实扣订阅）", got)
	}
	if got := userBalance(t, db, userID); got != 500 {
		t.Fatalf("钱包余额 = %d，want 500", got)
	}
}

// TestSessionSubscriptionIdempotentReplay 覆盖同 requestId 的重复会话：
// 订阅侧幂等返回既有记录，本会话刚扣的令牌必须原额退回（不双扣）。
func TestSessionSubscriptionIdempotentReplay(t *testing.T) {
	funding := newFakeSubscriptionFunding(contract.PrefSubscriptionOnly, subscriptionAccount(7, 1000, nil))
	e, db := newSubscriptionEngine(t, funding)
	userID := createBillingTestUser(t, db, 500)
	tokenID := createBillingTestToken(t, db, userID, int64(300))
	req := testRequestID("s4a-replay")

	first := newBillingTestSessionWithToken(e, userID, tokenID, req, perCallSnapshot(100))
	if err := first.PreConsume(contract.QuotaEstimate{Model: "m"}); err != nil {
		t.Fatalf("首次 PreConsume: %v", err)
	}
	second := newBillingTestSessionWithToken(e, userID, tokenID, req, perCallSnapshot(100))
	if err := second.PreConsume(contract.QuotaEstimate{Model: "m"}); err != nil {
		t.Fatalf("幂等重放 PreConsume: %v", err)
	}
	if got := funding.usedOf(7); got != 100 {
		t.Fatalf("订阅 amount_used = %d，want 100（同 requestId 不双扣）", got)
	}
	if got := tokenRemain(t, db, tokenID).Int64; got != 200 {
		t.Fatalf("令牌剩余 = %d，want 200（重放不得再扣令牌）", got)
	}
	// 回放会话不得退款：记录属于首执行者（LOW）。
	if second.NeedsRefund() {
		t.Fatal("回放会话 NeedsRefund = true，want false")
	}
	if err := second.Refund(); err != nil {
		t.Fatalf("回放会话 Refund: %v", err)
	}
	if funding.markAttempts != 0 {
		t.Fatalf("回放会话触发订阅退款 %d 次，want 0", funding.markAttempts)
	}
	if got := funding.usedOf(7); got != 100 {
		t.Fatalf("回放会话退款后 amount_used = %d，want 100", got)
	}
}

// TestSessionSubscriptionRefundRetries 覆盖订阅退款：前两次失败后第 3 次
// 成功（最多 3 次、退避 200/400ms），令牌回补、amount_used 回退；重复
// 调用幂等。
func TestSessionSubscriptionRefundRetries(t *testing.T) {
	funding := newFakeSubscriptionFunding(contract.PrefSubscriptionOnly, subscriptionAccount(7, 1000, nil))
	funding.markFailures = 2
	e, db := newSubscriptionEngine(t, funding)
	userID := createBillingTestUser(t, db, 500)
	tokenID := createBillingTestToken(t, db, userID, int64(300))
	s := newBillingTestSessionWithToken(e, userID, tokenID, testRequestID("s4a-refund"), perCallSnapshot(100))

	if err := s.PreConsume(contract.QuotaEstimate{Model: "m"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if got := tokenRemain(t, db, tokenID).Int64; got != 200 {
		t.Fatalf("预扣后令牌剩余 = %d，want 200", got)
	}
	if err := s.Refund(); err != nil {
		t.Fatalf("Refund: %v", err)
	}
	if funding.markAttempts != 3 {
		t.Fatalf("退款尝试 = %d，want 3（退避重试）", funding.markAttempts)
	}
	if got := funding.usedOf(7); got != 0 {
		t.Fatalf("退款后 amount_used = %d，want 0", got)
	}
	if got := tokenRemain(t, db, tokenID).Int64; got != 300 {
		t.Fatalf("退款后令牌剩余 = %d，want 300", got)
	}
	if got := userBalance(t, db, userID); got != 500 {
		t.Fatalf("钱包余额 = %d，want 500（订阅退款不动钱包）", got)
	}
	if s.NeedsRefund() {
		t.Fatal("Refund 后 NeedsRefund = true，want false")
	}
	// 重复退款：会话已 refunded，早返回；不得再调用仓储退第二次。
	if err := s.Refund(); err != nil {
		t.Fatalf("重复 Refund: %v", err)
	}
	if funding.markAttempts != 3 {
		t.Fatalf("重复退款尝试 = %d，want 3（幂等早返回）", funding.markAttempts)
	}
}

// TestStoreSubscriptionFundingRealRepository 用真 PG + 生产适配器验证：
// 偏好读取、单实例预扣、退款幂等与「已退款再预扣报错」在同一条链路上
// 成立（store 层事务语义由 store 包的集成用例重复覆盖）。
func TestStoreSubscriptionFundingRealRepository(t *testing.T) {
	_, db := openBillingTestDB(t)
	repo := store.NewSubscriptionRepo(db)
	funding := NewStoreSubscriptionFunding(repo, db)
	ctx := context.Background()
	userID := createBillingTestUser(t, db, 500)

	plan, err := repo.CreatePlan(ctx, store.SubscriptionPlanInput{
		PlanKey:      "s4a-adapter-" + time.Now().Format("150405.000000000"),
		Title:        "S4a",
		PeriodDays:   30,
		Currency:     "CNY",
		GrantCredits: 1000,
	})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM subscription_plans WHERE id = $1`, plan.ID) })
	now := time.Now().UTC()
	sub, err := repo.CreateSubscription(ctx, store.SubscriptionCreateInput{
		UserID: userID, PlanID: plan.ID, PlanTitleSnapshot: plan.Title,
		StartAt: now.Add(-time.Hour), EndAt: now.Add(time.Hour),
		AmountTotal: plan.GrantCredits, AmountUsed: 0,
		AllowWalletOverflow: plan.AllowWalletOverflow, Currency: plan.Currency,
		PricePaidAmount: plan.PriceAmount, PeriodDays: plan.PeriodDays,
		ResetOnRenew: plan.ResetOnRenew, GraceDays: plan.GraceDays,
	})
	if err != nil {
		t.Fatalf("CreateSubscription: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM subscription_pre_consume_records WHERE subscription_id = $1`, sub.ID)
		_, _ = db.Exec(`DELETE FROM subscriptions WHERE id = $1`, sub.ID)
	})

	// users.billing_preference 的 DB 默认值是 subscription_first。
	pref, err := funding.BillingPreference(ctx, userID)
	if err != nil {
		t.Fatalf("BillingPreference: %v", err)
	}
	if pref != contract.PrefSubscriptionFirst {
		t.Fatalf("默认偏好 = %v，want subscription_first", pref)
	}
	if _, err := db.Exec(`UPDATE users SET billing_preference = 'wallet_first' WHERE id = $1`, userID); err != nil {
		t.Fatalf("update preference: %v", err)
	}
	if pref, err = funding.BillingPreference(ctx, userID); err != nil || pref != contract.PrefWalletFirst {
		t.Fatalf("偏好 = %v/%v，want wallet_first/nil", pref, err)
	}

	req := testUUID(t)
	rec, created, err := funding.PreConsumeUserSubscription(ctx, req, sub.ID, 100)
	if err != nil || !created || rec.SubscriptionID != sub.ID || rec.PreConsumed != 100 {
		t.Fatalf("预扣 = %+v/%v/%v", rec, created, err)
	}
	if err := funding.MarkPreConsumeRefunded(ctx, req); err != nil {
		t.Fatalf("退款: %v", err)
	}
	if err := funding.MarkPreConsumeRefunded(ctx, req); err != nil {
		t.Fatalf("重复退款应幂等: %v", err)
	}
	if _, _, err := funding.PreConsumeUserSubscription(ctx, req, sub.ID, 100); !errors.Is(err, store.ErrPreConsumeRefunded) {
		t.Fatalf("已退款再预扣 err = %v，want ErrPreConsumeRefunded", err)
	}
	var used int64
	if err := db.QueryRow(`SELECT amount_used FROM subscriptions WHERE id = $1`, sub.ID).Scan(&used); err != nil {
		t.Fatalf("read amount_used: %v", err)
	}
	if used != 0 {
		t.Fatalf("退款后 amount_used = %d，want 0", used)
	}

	// R-W9B-01：退款事务本身写 registry refunded（不再是 Session 的第二条语句）。
	if state, err := funding.RequestRegistryState(ctx, req); err != nil || state != "refunded" {
		t.Fatalf("退款后 registry state = %q/%v，want refunded/nil", state, err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM request_registry WHERE request_id = $1`, req) })

	// S4b：结算增量适配器（ChargeIncrement 用）。它不得改写终态。
	settle, err := funding.PostConsumeUserSubscriptionDelta(ctx, sub.ID, 50)
	if err != nil {
		t.Fatalf("PostConsumeUserSubscriptionDelta: %v", err)
	}
	if settle.PlanID != plan.ID || settle.PlanTitle != plan.Title ||
		settle.AmountTotal != plan.GrantCredits || settle.AmountUsed != 50 {
		t.Fatalf("结算快照 = %+v，字段不符", settle)
	}
	if state, err := funding.RequestRegistryState(ctx, req); err != nil || state != "refunded" {
		t.Fatalf("增量结算改写了终态 = %q/%v，want refunded", state, err)
	}

	// settled 审计接缝（R-W9B-11）与终态互斥：用一个全新 requestId。
	settleReq := testUUID(t)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM request_registry WHERE request_id = $1`, settleReq) })
	if state, err := funding.RequestRegistryState(ctx, settleReq); err != nil || state != "" {
		t.Fatalf("初始 registry state = %q/%v，want 空/nil", state, err)
	}
	if err := funding.MarkRequestSettled(ctx, settleReq, userID); err != nil {
		t.Fatalf("MarkRequestSettled: %v", err)
	}
	if err := funding.MarkRequestSettled(ctx, settleReq, userID); err != nil {
		t.Fatalf("重复 MarkRequestSettled 应幂等: %v", err)
	}
	if state, err := funding.RequestRegistryState(ctx, settleReq); err != nil || state != "settled" {
		t.Fatalf("registry state = %q/%v，want settled/nil", state, err)
	}
	// 已 refunded 的请求不得被审计接缝升级为 settled。
	if err := funding.MarkRequestSettled(ctx, req, userID); err != nil {
		t.Fatalf("refunded 后 MarkRequestSettled: %v", err)
	}
	if state, _ := funding.RequestRegistryState(ctx, req); state != "refunded" {
		t.Fatalf("refunded 被改写为 %q，want refunded", state)
	}
	// settled 后 MarkRequestRefunded 也不得降级。
	if err := funding.MarkRequestRefunded(ctx, settleReq, userID); err != nil {
		t.Fatalf("settled 后 MarkRequestRefunded: %v", err)
	}
	if state, _ := funding.RequestRegistryState(ctx, settleReq); state != "settled" {
		t.Fatalf("settled 后被改写为 %q，want settled", state)
	}
}

// TestSessionSubscriptionFirstModelScopeMismatch 覆盖 M1：存在活跃订阅但
// model_scope 全部不匹配时，按「订阅失败」处理——只有允许溢出的实例才回退
// 钱包，否则 402；不能因为「没有候选」就静默改扣钱包。
func TestSessionSubscriptionFirstModelScopeMismatch(t *testing.T) {
	tests := []struct {
		name        string
		overflow    bool
		wantErr     bool
		wantBalance int64
	}{
		{name: "不匹配+禁止溢出：402", overflow: false, wantErr: true, wantBalance: 500},
		{name: "不匹配+允许溢出：回退钱包", overflow: true, wantErr: false, wantBalance: 400},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sub := subscriptionAccount(7, 1000, []byte(`["other/*"]`))
			sub.AllowWalletOverflow = tc.overflow
			funding := newFakeSubscriptionFunding(contract.PrefSubscriptionFirst, sub)
			e, db := newSubscriptionEngine(t, funding)
			userID := createBillingTestUser(t, db, 500)
			s := newBillingTestSession(e, userID, testRequestID("s4a-scope"), perCallSnapshot(100))

			err := s.PreConsume(contract.QuotaEstimate{Model: "m"})
			if tc.wantErr {
				if !errors.Is(err, ErrInsufficientQuota) {
					t.Fatalf("err = %v，want ErrInsufficientQuota", err)
				}
				if s.fundingSource != contract.FundingWallet {
					t.Fatalf("来源 = %v，want wallet（未扣款）", s.fundingSource)
				}
			} else if err != nil {
				t.Fatalf("PreConsume: %v", err)
			}
			if tc.wantErr && s.preConsumed != 0 {
				t.Fatalf("失败后 preConsumed = %d，want 0", s.preConsumed)
			}
			if got := userBalance(t, db, userID); got != tc.wantBalance {
				t.Fatalf("钱包余额 = %d，want %d", got, tc.wantBalance)
			}
		})
	}
}

// TestSessionWalletOnlySkipsSubscriptionList 覆盖 LOW：wallet_only 不应查询
// 活跃订阅（资金方向与订阅列表无关）。这里让列表查询直接报错，证明它未被调用。
func TestSessionWalletOnlySkipsSubscriptionList(t *testing.T) {
	funding := newFakeSubscriptionFunding(contract.PrefWalletOnly, subscriptionAccount(7, 1000, nil))
	funding.listErr = errors.New("list must not be called for wallet_only")
	e, db := newSubscriptionEngine(t, funding)
	userID := createBillingTestUser(t, db, 500)
	s := newBillingTestSession(e, userID, testRequestID("s4a-walletonly"), perCallSnapshot(100))

	if err := s.PreConsume(contract.QuotaEstimate{Model: "m"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if _, list := funding.resolveCounts(); list != 0 {
		t.Fatalf("wallet_only 查询订阅列表 %d 次，want 0", list)
	}
	if got := userBalance(t, db, userID); got != 400 {
		t.Fatalf("钱包余额 = %d，want 400", got)
	}
}

// TestSessionFundingPlanResolvedOnce 覆盖 M6：偏好与候选列表每请求只解析
// 一次，预扣/结算/退款复用同一份冻结快照。
func TestSessionFundingPlanResolvedOnce(t *testing.T) {
	funding := newFakeSubscriptionFunding(contract.PrefSubscriptionFirst, subscriptionAccount(7, 1000, nil))
	e, db := newSubscriptionEngine(t, funding)
	userID := createBillingTestUser(t, db, 500)
	s := newBillingTestSession(e, userID, testRequestID("s4a-once"), perCallSnapshot(100))

	if err := s.PreConsume(contract.QuotaEstimate{Model: "m"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	s.mu.Lock()
	_, err := s.resolveFundingPlanLocked(context.Background())
	s.mu.Unlock()
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	pref, list := funding.resolveCounts()
	if pref != 1 || list != 1 {
		t.Fatalf("解析次数 pref=%d list=%d，want 1/1（每请求只解析一次）", pref, list)
	}
}

// TestSessionSubscriptionCandidatesEndAtOrderAndContinue 覆盖 M6：多候选按
// end_at 升序尝试；单实例额度不足时 continue 到下一个，顺序不受仓储返回
// 顺序影响。
func TestSessionSubscriptionCandidatesEndAtOrderAndContinue(t *testing.T) {
	early := subscriptionAccount(11, 50, nil) // 更早到期但额度不足
	early.EndAt = time.Now().Add(time.Hour)
	late := subscriptionAccount(12, 1000, nil)
	late.EndAt = time.Now().Add(2 * time.Hour)
	// 故意逆序传入，验证 plan 内部按 end_at 重排。
	funding := newFakeSubscriptionFunding(contract.PrefSubscriptionOnly, late, early)
	e, db := newSubscriptionEngine(t, funding)
	userID := createBillingTestUser(t, db, 500)
	s := newBillingTestSession(e, userID, testRequestID("s4a-order"), perCallSnapshot(100))

	if err := s.PreConsume(contract.QuotaEstimate{Model: "m"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if got, want := funding.attemptedIDs(), []int64{11, 12}; len(got) != len(want) ||
		got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("尝试顺序 = %v，want %v（end_at 升序 + 不足继续）", got, want)
	}
	if got := funding.usedOf(11); got != 0 {
		t.Fatalf("不足候选 amount_used = %d，want 0", got)
	}
	if got := funding.usedOf(12); got != 100 {
		t.Fatalf("可用候选 amount_used = %d，want 100", got)
	}
	if s.subscriptionID != 12 {
		t.Fatalf("冻结订阅 = %d，want 12", s.subscriptionID)
	}
}

// TestSessionSubscriptionRefundAllAttemptsFail 覆盖 M6：3 次退款全部失败时
// 返回错误、不标记 refunded，资金/令牌保持已扣（可重试）。
func TestSessionSubscriptionRefundAllAttemptsFail(t *testing.T) {
	funding := newFakeSubscriptionFunding(contract.PrefSubscriptionOnly, subscriptionAccount(7, 1000, nil))
	funding.markFailures = 3
	e, db := newSubscriptionEngine(t, funding)
	userID := createBillingTestUser(t, db, 500)
	tokenID := createBillingTestToken(t, db, userID, int64(300))
	s := newBillingTestSessionWithToken(e, userID, tokenID, testRequestID("s4a-refund-fail"), perCallSnapshot(100))

	if err := s.PreConsume(contract.QuotaEstimate{Model: "m"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if err := s.Refund(); err == nil {
		t.Fatal("Refund = nil，want 3 次失败后的错误")
	}
	if funding.markAttempts != 3 {
		t.Fatalf("退款尝试 = %d，want 3", funding.markAttempts)
	}
	if got := funding.usedOf(7); got != 100 {
		t.Fatalf("退款失败后 amount_used = %d，want 100", got)
	}
	if got := tokenRemain(t, db, tokenID).Int64; got != 200 {
		t.Fatalf("退款失败后令牌剩余 = %d，want 200", got)
	}
	if got := userBalance(t, db, userID); got != 500 {
		t.Fatalf("钱包余额 = %d，want 500", got)
	}
	if !s.NeedsRefund() {
		t.Fatal("退款失败后 NeedsRefund = false，want true（可重试）")
	}
}

// TestSessionSubscriptionGuardsNeverTouchWallet 覆盖订阅来源的资金隔离：
// Reserve 仍 no-op（订阅不支持上浮），ChargeIncrement 立即结算订阅并同步
// **令牌**额度（S4b 冻结语义），但任何路径都不得触碰钱包。
func TestSessionSubscriptionGuardsNeverTouchWallet(t *testing.T) {
	funding := newFakeSubscriptionFunding(contract.PrefSubscriptionOnly, subscriptionAccount(7, 1000, nil))
	e, db := newSubscriptionEngine(t, funding)
	userID := createBillingTestUser(t, db, 500)
	tokenID := createBillingTestToken(t, db, userID, int64(300))
	s := newBillingTestSessionWithToken(e, userID, tokenID, testRequestID("s4a-guards"), perCallSnapshot(100))

	if err := s.PreConsume(contract.QuotaEstimate{Model: "m"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if err := s.Reserve(50); err != nil {
		t.Fatalf("Reserve on subscription: %v", err)
	}
	if err := s.ChargeIncrement(30, contract.UsageFact{Model: "m"}); err != nil {
		t.Fatalf("ChargeIncrement on subscription: %v", err)
	}
	if got := s.Reserved(); got != 100 {
		t.Fatalf("Reserve 改变了订阅预留: %d，want 100", got)
	}
	if got := userBalance(t, db, userID); got != 500 {
		t.Fatalf("钱包余额 = %d，want 500（订阅 guard 不得扣钱包）", got)
	}
	// S4b：实时增量立即追加订阅额度并同步扣令牌（预扣 100 + 增量 30）。
	if got := tokenRemain(t, db, tokenID).Int64; got != 170 {
		t.Fatalf("令牌剩余 = %d，want 170（增量必须同步扣令牌）", got)
	}
	if got := funding.usedOf(7); got != 130 {
		t.Fatalf("订阅 amount_used = %d，want 130（增量立即入账）", got)
	}
	if err := s.Terminate(contract.UsageFact{}); err != nil {
		t.Fatalf("Terminate on subscription: %v", err)
	}
	if got := userBalance(t, db, userID); got != 500 {
		t.Fatalf("Terminate 后钱包余额 = %d，want 500", got)
	}
	// 空 fact 成本 = 预扣 + 增量 = 130，delta=0，令牌不再变化。
	if got := tokenRemain(t, db, tokenID).Int64; got != 170 {
		t.Fatalf("Terminate 后令牌剩余 = %d，want 170", got)
	}
	if !s.settled {
		t.Fatal("Terminate 后 settled = false")
	}
}

// TestSessionSubscriptionZeroBudgetPreConsumesOne 走真实 PreConsume 路径
// （M6）：估算价 0 但非免费模型时，订阅预扣量置 1、令牌同步扣 1。
func TestSessionSubscriptionZeroBudgetPreConsume(t *testing.T) {
	funding := newFakeSubscriptionFunding(contract.PrefSubscriptionOnly, subscriptionAccount(7, 1000, nil))
	e, db := newSubscriptionEngine(t, funding)
	userID := createBillingTestUser(t, db, 500)
	tokenID := createBillingTestToken(t, db, userID, int64(300))
	// ratio 定价为正价，但估算 PromptTokens=0 → 预算 0（不是免费模型）。
	s := newBillingTestSessionWithToken(e, userID, tokenID, testRequestID("s4a-zero-pre"), ratioSnapshot(100, 0))

	if err := s.PreConsume(contract.QuotaEstimate{Model: "m"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if got := funding.usedOf(7); got != 1 {
		t.Fatalf("订阅 amount_used = %d，want 1（零预算置 1）", got)
	}
	if s.preConsumed != 1 || s.tokenDeducted != 1 {
		t.Fatalf("preConsumed/tokenDeducted = %d/%d，want 1/1", s.preConsumed, s.tokenDeducted)
	}
	if got := tokenRemain(t, db, tokenID).Int64; got != 299 {
		t.Fatalf("令牌剩余 = %d，want 299", got)
	}
	if got := userBalance(t, db, userID); got != 500 {
		t.Fatalf("钱包余额 = %d，want 500", got)
	}
}

// TestSessionSubscriptionZeroBudgetPreConsumesOne 覆盖订阅预扣量构造：
// amount <= 0 时置 1（订阅预扣要求 amount > 0）。
func TestSessionSubscriptionZeroBudgetPreConsumesOne(t *testing.T) {
	funding := newFakeSubscriptionFunding(contract.PrefSubscriptionOnly, subscriptionAccount(7, 1000, nil))
	e, db := newSubscriptionEngine(t, funding)
	userID := createBillingTestUser(t, db, 500)
	s := newBillingTestSession(e, userID, testRequestID("s4a-zero"), perCallSnapshot(100))
	s.model = "m"

	if err := s.chargeSubscriptionLocked(context.Background(), 0, []SubscriptionAccount{subscriptionAccount(7, 1000, nil)}); err != nil {
		t.Fatalf("chargeSubscriptionLocked: %v", err)
	}
	if got := funding.usedOf(7); got != 1 {
		t.Fatalf("订阅 amount_used = %d，want 1（零预算置 1）", got)
	}
	if s.preConsumed != 1 {
		t.Fatalf("preConsumed = %d，want 1", s.preConsumed)
	}
}
