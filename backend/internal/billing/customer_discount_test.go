package billing

// T3 预扣阶段客户折扣的验收用例（spec §5.2/§5.4）。
//
// 覆盖三件事：
//   - 解析缝只解析一次，并把用户/候选模型/冻结时间原样交给仓储；
//   - 折扣作用在最小计费保底**之后**、资金来源选择**之前**；
//   - 查询失败、未命中、全免三者的资金语义（失败原价、全免零预扣）。
//
// 与既有 billing 集成用例一致：未设置 NEWTG_TEST_DB_DSN 时跳过。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"

	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/store"
)

// fakeDiscountStore 是 DiscountStore 的内存实现：可返回命中行/未命中/错误，
// 并记录每次调用收到的用户、候选模型与时刻。
type fakeDiscountStore struct {
	discount store.CustomerDiscount
	found    bool
	err      error

	calls     int
	gotUserID int64
	gotModels []string
	gotAt     time.Time
}

func (f *fakeDiscountStore) Resolve(_ context.Context, userID int64, models []string, at time.Time) (store.CustomerDiscount, bool, error) {
	f.calls++
	f.gotUserID = userID
	f.gotModels = append([]string(nil), models...)
	f.gotAt = at
	return f.discount, f.found, f.err
}

// customerDiscount 构造一条命中规则；ratePercent 是实收百分比（90 = 9 折）。
func customerDiscount(ratePercent int16, targetType, targetID string) store.CustomerDiscount {
	return store.CustomerDiscount{
		ModelName:   "m",
		TargetType:  targetType,
		TargetID:    targetID,
		RatePercent: ratePercent,
		Enabled:     true,
	}
}

// discountTestConfig 返回确定性配置：预扣比例 1.0、关闭信任旁路，并显式
// 设定预扣下限（用于验证「先保底、后打折」的顺序）。
func discountTestConfig(minHold int64) ConfigReader {
	return mapConfig{
		ConfigPreConsumeRatio:    json.RawMessage("1.0"),
		ConfigTrustQuotaUSD:      json.RawMessage("0"),
		ConfigMinPreConsumeQuota: json.RawMessage(jsonNumber(minHold)),
	}
}

// jsonNumber 把整数渲染成 JSON 数字文本（避免依赖 fmt 的格式差别）。
func jsonNumber(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// newDiscountTestEngine 打开真实账本库并装配折扣解析缝。
func newDiscountTestEngine(t *testing.T, cfg ConfigReader, ds DiscountStore) (*Engine, *sql.DB) {
	t.Helper()
	gormDB, db := openBillingTestDB(t)
	if cfg == nil {
		cfg = billingTestConfig()
	}
	e := NewEngine(gormDB, nil, nil, cfg, nil, nil, nil)
	if ds != nil {
		e.SetDiscountStore(ds)
	}
	return e, db
}

// TestPreConsumeAppliesDiscountAfterFloor 验证顺序冻结：先保底、再打折。
//
// 估算 10、保底 10：若顺序颠倒（先折 10×0.5=5 再保底）结果是 10；正确顺序
// 下保底不改变 10，再折半得到 5。
func TestPreConsumeAppliesDiscountAfterFloor(t *testing.T) {
	ds := &fakeDiscountStore{discount: customerDiscount(50, "user", "7"), found: true}
	e, db := newDiscountTestEngine(t, discountTestConfig(10), ds)
	userID := createBillingTestUser(t, db, 500)
	s := newBillingTestSession(e, userID, testRequestID("t3-floor"), perCallSnapshot(10))

	if err := s.PreConsume(contract.QuotaEstimate{Model: "m"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if got := s.Reserved(); got != 5 {
		t.Fatalf("Reserved = %d, want 5（保底 10 之后 × 0.5，而不是先折后被保底抬到 10）", got)
	}
	if got := userBalance(t, db, userID); got != 495 {
		t.Fatalf("balance = %d, want 495（hold 5）", got)
	}
}

// TestPreConsumeZeroRateHoldsNothing 验证全免规则零预扣、来源为钱包。
func TestPreConsumeZeroRateHoldsNothing(t *testing.T) {
	ds := &fakeDiscountStore{discount: customerDiscount(0, "user", "7"), found: true}
	e, db := newDiscountTestEngine(t, nil, ds)
	userID := createBillingTestUser(t, db, 500)
	s := newBillingTestSession(e, userID, testRequestID("t3-zero"), perCallSnapshot(100))

	if err := s.PreConsume(contract.QuotaEstimate{Model: "m"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if got := s.Reserved(); got != 0 {
		t.Fatalf("Reserved = %d, want 0（全免不得预扣）", got)
	}
	if s.fundingSource != contract.FundingWallet {
		t.Fatalf("fundingSource = %v, want wallet", s.fundingSource)
	}
	if got := userBalance(t, db, userID); got != 500 {
		t.Fatalf("balance = %d, want 500（全免不得动余额）", got)
	}
	if rows := readWalletRows(t, db, s.requestID); len(rows) != 0 {
		t.Fatalf("钱包流水 = %d 条, want 0（全免不产生 hold）", len(rows))
	}
}

// TestPreConsumeZeroRateSkipsSubscription 验证全免短路在资金来源选择之前：
// 订阅偏好下也不得调用订阅预扣（chargeSubscriptionLocked 会把 0 抬成 1）。
func TestPreConsumeZeroRateSkipsSubscription(t *testing.T) {
	ds := &fakeDiscountStore{discount: customerDiscount(0, "user", "7"), found: true}
	funding := newFakeSubscriptionFunding(contract.PrefSubscriptionFirst, subscriptionAccount(7, 1000, nil))
	e, db := newDiscountTestEngine(t, nil, ds)
	e.SetSubscriptionFunding(funding)
	userID := createBillingTestUser(t, db, 500)
	s := newBillingTestSession(e, userID, testRequestID("t3-zero-sub"), perCallSnapshot(100))

	if err := s.PreConsume(contract.QuotaEstimate{Model: "m"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if got := funding.calls(); got != 0 {
		t.Fatalf("订阅预扣调用 = %d 次, want 0（全免必须跳过订阅）", got)
	}
	if s.fundingSource != contract.FundingWallet {
		t.Fatalf("fundingSource = %v, want wallet", s.fundingSource)
	}
	if got := s.Reserved(); got != 0 {
		t.Fatalf("Reserved = %d, want 0", got)
	}
}

// TestPreConsumeDiscountQueryFailureBillsListPrice 验证解析故障绝不阻断请求、
// 也绝不误免：按原价预算预扣，且折扣保持零值（Target 为空）。
func TestPreConsumeDiscountQueryFailureBillsListPrice(t *testing.T) {
	ds := &fakeDiscountStore{err: errors.New("fake: discounts unavailable")}
	e, db := newDiscountTestEngine(t, nil, ds)
	userID := createBillingTestUser(t, db, 500)
	s := newBillingTestSession(e, userID, testRequestID("t3-err"), perCallSnapshot(100))

	if err := s.PreConsume(contract.QuotaEstimate{Model: "m"}); err != nil {
		t.Fatalf("PreConsume: %v, want nil（折扣查询失败不得让请求失败）", err)
	}
	if got := s.Reserved(); got != 100 {
		t.Fatalf("Reserved = %d, want 100（原价预算）", got)
	}
	if s.discount.Target != "" || s.discount.Rate != 0 {
		t.Fatalf("discount = %+v, want 零值（未命中）", s.discount)
	}
	if got := userBalance(t, db, userID); got != 400 {
		t.Fatalf("balance = %d, want 400", got)
	}
}

// TestPreConsumeUsesFrozenRequestTime 验证有效期判定使用会话冻结的
// requestTime，而不是解析时刻的 time.Now()。
func TestPreConsumeUsesFrozenRequestTime(t *testing.T) {
	ds := &fakeDiscountStore{found: false}
	e, db := newDiscountTestEngine(t, nil, ds)
	userID := createBillingTestUser(t, db, 500)
	s := newBillingTestSession(e, userID, testRequestID("t3-time"), perCallSnapshot(100))
	frozen := time.Date(2026, 10, 1, 8, 30, 0, 0, time.UTC)
	s.requestTime = frozen

	if err := s.PreConsume(contract.QuotaEstimate{Model: "m"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if !ds.gotAt.Equal(frozen) {
		t.Fatalf("Resolve at = %v, want 冻结的 requestTime %v", ds.gotAt, frozen)
	}
	if ds.gotUserID != userID {
		t.Fatalf("Resolve userID = %d, want %d", ds.gotUserID, userID)
	}
}

// TestPreConsumeExpandsModelCandidates 验证模型名先展开为候选列表再交给仓储：
// 仓储按真实模型名匹配，而不是带 @channel 修饰的请求原名。
func TestPreConsumeExpandsModelCandidates(t *testing.T) {
	ds := &fakeDiscountStore{found: false}
	e, db := newDiscountTestEngine(t, nil, ds)
	userID := createBillingTestUser(t, db, 500)
	s := newBillingTestSession(e, userID, testRequestID("t3-candidates"), perCallSnapshot(100))

	if err := s.PreConsume(contract.QuotaEstimate{Model: "claude-sonnet-5-5@thinking"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	found := false
	for _, candidate := range ds.gotModels {
		if candidate == "claude-sonnet-5-5" {
			found = true
		}
		if candidate == "claude-sonnet-5-5@thinking" {
			t.Fatalf("候选列表不应包含带修饰符的原名: %v", ds.gotModels)
		}
	}
	if !found {
		t.Fatalf("候选列表 = %v, want 含规范名 claude-sonnet-5-5", ds.gotModels)
	}
}

// TestSessionFrozenDiscountSemantics 覆盖冻结值入口与生效判据：
// 零值 Rate + 空 Target 是「未命中」而不是全免；NaN 因无法比较而不适用。
// 结算侧（后续任务）通过 applyFrozenDiscount 复用预扣冻结的折扣。
func TestSessionFrozenDiscountSemantics(t *testing.T) {
	s := &Session{}
	if s.discountApplies() {
		t.Fatal("零值折扣不应生效（Target 为空 = 未命中，不是全免）")
	}
	s.applyFrozenDiscount(0, "user:7")
	if !s.discountResolved {
		t.Fatal("applyFrozenDiscount 必须置 discountResolved")
	}
	if !s.discountApplies() {
		t.Fatal("Target 非空且 Rate == 0 必须被判定为全免生效")
	}
	s.applyFrozenDiscount(math.NaN(), "user:7")
	if s.discountApplies() {
		t.Fatal("NaN rate 不得生效（fail-closed 到原价）")
	}
	s.applyFrozenDiscount(1, "user:7")
	if s.discountApplies() {
		t.Fatal("rate == 1 不得生效")
	}
}

// TestLedgerDiscountFacts 覆盖结算侧「原价事实」的推导口径（R1）：
//   - 已知原价直接使用；未知时按 net/rate 反推；
//   - 全免（rate 0）不写原价；
//   - 未命中/无折扣/NaN 一律返回零值三元组。
func TestLedgerDiscountFacts(t *testing.T) {
	tests := []struct {
		name     string
		d        contract.Discount
		rawCost  int64
		netCost  int64
		rawKnown bool
		wantRaw  int64
		wantRate float64
		wantTgt  string
	}{
		{
			name: "已知原价", d: contract.Discount{Rate: 0.9, Target: "user:7"},
			rawCost: 100, netCost: 90, rawKnown: true,
			wantRaw: 100, wantRate: 0.9, wantTgt: "user:7",
		},
		{
			name: "未知原价按 net/rate 反推", d: contract.Discount{Rate: 0.9, Target: "user:7"},
			netCost: 90, rawKnown: false,
			wantRaw: 100, wantRate: 0.9, wantTgt: "user:7",
		},
		{
			name: "全免不写原价", d: contract.Discount{Rate: 0, Target: "user:7"},
			rawCost: 100, netCost: 0, rawKnown: true,
			wantRaw: 0, wantRate: 0, wantTgt: "user:7",
		},
		{
			name: "原价规则等于未命中", d: contract.Discount{Rate: 1, Target: "user:7"},
			rawCost: 100, netCost: 100, rawKnown: true,
			wantRaw: 0, wantRate: 0, wantTgt: "",
		},
		{
			name: "空目标", d: contract.Discount{Rate: 0.9, Target: ""},
			rawCost: 100, netCost: 90, rawKnown: true,
			wantRaw: 0, wantRate: 0, wantTgt: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw, rate, target := discountFacts(tc.d, tc.rawCost, tc.netCost, tc.rawKnown)
			if raw != tc.wantRaw || rate != tc.wantRate || target != tc.wantTgt {
				t.Fatalf("discountFacts = (%d, %v, %q), want (%d, %v, %q)",
					raw, rate, target, tc.wantRaw, tc.wantRate, tc.wantTgt)
			}
			s := &Session{discount: tc.d}
			raw, rate, target = s.ledgerDiscount(tc.rawCost, tc.netCost, tc.rawKnown)
			if raw != tc.wantRaw || rate != tc.wantRate || target != tc.wantTgt {
				t.Fatalf("ledgerDiscount = (%d, %v, %q), want (%d, %v, %q)",
					raw, rate, target, tc.wantRaw, tc.wantRate, tc.wantTgt)
			}
		})
	}
}
