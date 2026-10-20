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
	preConsumeCalls int
	markAttempts    int
	// markFailures 是前 N 次退款调用返回错误（验证 3 次退避重试）。
	markFailures int
}

func newFakeSubscriptionFunding(pref contract.BillingPreference, subs ...SubscriptionAccount) *fakeSubscriptionFunding {
	return &fakeSubscriptionFunding{
		preference: pref,
		subs:       subs,
		used:       map[int64]int64{},
		records:    map[string]store.SubscriptionPreConsume{},
	}
}

func (f *fakeSubscriptionFunding) BillingPreference(context.Context, int64) (contract.BillingPreference, error) {
	if f.prefErr != nil {
		return 0, f.prefErr
	}
	return f.preference, nil
}

func (f *fakeSubscriptionFunding) ListActiveSubscriptions(context.Context, int64, time.Time) ([]SubscriptionAccount, error) {
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
	if used := f.used[rec.SubscriptionID] - rec.PreConsumed; used > 0 {
		f.used[rec.SubscriptionID] = used
	} else {
		f.used[rec.SubscriptionID] = 0
	}
	return nil
}

func (f *fakeSubscriptionFunding) usedOf(id int64) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.used[id]
}

func (f *fakeSubscriptionFunding) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.preConsumeCalls
}

// subscriptionAccount 构造一个活跃、额度充足的订阅候选。
func subscriptionAccount(id, total int64, scope []byte) SubscriptionAccount {
	return SubscriptionAccount{
		ID: id, EndAt: time.Now().Add(time.Hour), AmountTotal: total,
		ModelScope: scope,
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
// 从订阅扣，钱包与令牌（无令牌）都不动，Session 冻结来源信息。
func TestSessionSubscriptionFirstUsesSubscription(t *testing.T) {
	funding := newFakeSubscriptionFunding(contract.PrefSubscriptionFirst, subscriptionAccount(7, 1000, nil))
	e, db := newSubscriptionEngine(t, funding)
	userID := createBillingTestUser(t, db, 500)
	tokenID := createBillingTestToken(t, db, userID, int64(500))
	s := newBillingTestSessionWithToken(e, userID, 0, testRequestID("s4a-sub"), perCallSnapshot(100))

	if err := s.PreConsume(contract.QuotaEstimate{Model: "m"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if s.fundingSource != contract.FundingSubscription || s.subscriptionID != 7 || s.preConsumed != 100 {
		t.Fatalf("冻结来源 = %v/%d/%d，want subscription/7/100",
			s.fundingSource, s.subscriptionID, s.preConsumed)
	}
	if got := funding.usedOf(7); got != 100 {
		t.Fatalf("订阅 amount_used = %d，want 100", got)
	}
	if got := userBalance(t, db, userID); got != 500 {
		t.Fatalf("钱包余额 = %d，want 500（订阅来源不得扣钱包）", got)
	}
	_ = tokenID
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
