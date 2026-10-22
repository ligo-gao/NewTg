package billing

// W8/MJ-d 固定价任务族的计费验收（真 PG；未设 NEWTG_TEST_DB_DSN 则跳过）。
//
// 覆盖冻结口径：
//   - UsePrice 估算走严格按次价（model_pricing.mode=1 + prompt_price），
//     金额完全由配置价决定，与 token/张数无关（relay-05 §7.1/§7.3）；
//   - 缺价 fail-closed：PreConsume 返回 ErrNoPerCallPrice，余额/流水不动，
//     绝不回落到默认倍率价后按 0 结清；
//   - 终态 Settle 按预扣额（at-held）：成功不二次扣费、不按空用量退款；
//   - 免计费档位的显式零价快照不产生任何 hold/流水。
//
// 这里用带纳秒时间戳的唯一模型名，避免污染共享测试库的 model_pricing。

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"newtg/gateway/internal/billing/pricing"
	"newtg/gateway/internal/contract"
)

func mjUniqueModel(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// TestMJPerCallPriceIsChargedAtSubmitAndHeldToSettlement 覆盖「提交价 →
// 终态按预扣额结清」的完整资金路径。
func TestMJPerCallPriceIsChargedAtSubmitAndHeldToSettlement(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	src, err := pricing.NewSource(gormDB, billingTestConfig())
	if err != nil {
		t.Fatalf("pricing.NewSource: %v", err)
	}
	engine := NewEngine(gormDB, nil, nil, billingTestConfig(), src, nil, nil)
	ctx := context.Background()

	for _, price := range []int64{100, 250, 800} {
		price := price
		t.Run(fmt.Sprintf("price-%d", price), func(t *testing.T) {
			model := mjUniqueModel("mjd-price")
			t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM model_pricing WHERE model = $1`, model) })
			if _, err := db.Exec(
				`INSERT INTO model_pricing (model, mode, prompt_price, status) VALUES ($1, 1, $2, 1)`,
				model, price); err != nil {
				t.Fatalf("insert model_pricing: %v", err)
			}

			userID := createBillingTestUser(t, db, 1000)
			requestID := testRequestID("mjd-price")
			session := newBillingTestSession(engine, userID, requestID, nil)
			est := contract.QuotaEstimate{
				Model:         model,
				Group:         "default",
				UsePrice:      true,
				FixedPriceKey: model,
			}
			if err := session.PreConsume(est); err != nil {
				t.Fatalf("PreConsume: %v", err)
			}
			if got := session.Reserved(); got != price {
				t.Fatalf("Reserved = %d，want 动作价 %d", got, price)
			}
			if got := userBalance(t, db, userID); got != 1000-price {
				t.Fatalf("balance after PreConsume = %d，want %d", got, 1000-price)
			}

			// 终态成功：空 fact = 按预扣额结清，不重算、不多退少补。
			if err := engine.TerminateByRequestIDForToken(requestID, 0, contract.UsageFact{}); err != nil {
				t.Fatalf("TerminateByRequestIDForToken: %v", err)
			}
			if got := userBalance(t, db, userID); got != 1000-price {
				t.Fatalf("balance after settle = %d，want %d（按预扣额结清）", got, 1000-price)
			}
			st, err := engine.ledger.openHoldState(ctx, requestID)
			if err != nil {
				t.Fatalf("openHoldState: %v", err)
			}
			if st.Held != 0 || !st.Closed || st.ClosedStatus != "settled" {
				t.Fatalf("hold 未按动作价结清: %+v", st)
			}
			// 幂等：重复结算不改变余额。
			if err := engine.TerminateByRequestIDForToken(requestID, 0, contract.UsageFact{}); err != nil {
				t.Fatalf("idempotent settle: %v", err)
			}
			if got := userBalance(t, db, userID); got != 1000-price {
				t.Fatalf("idempotent settle changed balance: %d", got)
			}
		})
	}
}

// TestMJMissingPriceFailsClosed 断言缺价时提交面在触碰资金前失败。
//
// 变异点：把严格解析改回通用 Resolve（默认倍率价）→ 本用例会拿到
// PreConsume 成功而余额不变（估算 0），断言立刻红。
func TestMJMissingPriceFailsClosed(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	src, err := pricing.NewSource(gormDB, billingTestConfig())
	if err != nil {
		t.Fatalf("pricing.NewSource: %v", err)
	}
	engine := NewEngine(gormDB, nil, nil, billingTestConfig(), src, nil, nil)
	userID := createBillingTestUser(t, db, 1000)
	requestID := testRequestID("mjd-missing")
	model := mjUniqueModel("mjd-missing")

	session := newBillingTestSession(engine, userID, requestID, nil)
	err = session.PreConsume(contract.QuotaEstimate{
		Model: model, Group: "default", UsePrice: true, FixedPriceKey: model,
	})
	if !errors.Is(err, pricing.ErrNoPerCallPrice) {
		t.Fatalf("PreConsume err = %v，want pricing.ErrNoPerCallPrice", err)
	}
	if got := userBalance(t, db, userID); got != 1000 {
		t.Fatalf("缺价请求动了余额: %d，want 1000", got)
	}
	if rows := readWalletRows(t, db, requestID); len(rows) != 0 {
		t.Fatalf("缺价请求写了流水: %+v", rows)
	}
	if got := session.Reserved(); got != 0 {
		t.Fatalf("缺价请求 Reserved = %d，want 0", got)
	}
}

// TestMJFreeTierZeroSnapshotHasNoHold 覆盖免计费档位：显式零价快照预扣 0、
// 不写流水、不应答失败。金额 0 来自档位事实，而不是「查不到价」。
func TestMJFreeTierZeroSnapshotHasNoHold(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	engine := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)
	userID := createBillingTestUser(t, db, 1000)
	requestID := testRequestID("mjd-free")

	zero := &contract.PriceSnapshot{
		Mode:                 contract.BillingModePerCall,
		Price:                contract.PriceData{UsePrice: true, ModelPrice: 0, FreeModel: true},
		GroupRatio:           1,
		PreConsumeMultiplier: 1,
	}
	session := newBillingTestSession(engine, userID, requestID, zero)
	if err := session.PreConsume(contract.QuotaEstimate{
		Model: "mj_inpaint", Group: "default", UsePrice: true, FixedPriceKey: "mj_inpaint",
	}); err != nil {
		t.Fatalf("free PreConsume: %v", err)
	}
	if got := session.Reserved(); got != 0 {
		t.Fatalf("free Reserved = %d，want 0", got)
	}
	if got := userBalance(t, db, userID); got != 1000 {
		t.Fatalf("free action changed balance: %d", got)
	}
	if rows := readWalletRows(t, db, requestID); len(rows) != 0 {
		t.Fatalf("free action wrote wallet rows: %+v", rows)
	}
	if err := engine.TerminateByRequestIDForToken(requestID, 0, contract.UsageFact{}); err != nil {
		t.Fatalf("free settle: %v", err)
	}
}

// TestMJRatioSnapshotRejectedForUsePrice 断言 UsePrice 估算不能接受倍率快照。
//
// 调用方预置了错模式的快照时，按倍率公式 + 空用量会估算 0；必须在预扣前
// 拒绝，而不是「成功但免费」。
func TestMJRatioSnapshotRejectedForUsePrice(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	engine := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)
	userID := createBillingTestUser(t, db, 1000)
	requestID := testRequestID("mjd-badsnap")

	session := newBillingTestSession(engine, userID, requestID, ratioSnapshot(100, 0))
	err := session.PreConsume(contract.QuotaEstimate{
		Model: "mj_imagine", Group: "default", UsePrice: true, FixedPriceKey: "mj_imagine",
	})
	if !errors.Is(err, pricing.ErrNoPerCallPrice) {
		t.Fatalf("PreConsume err = %v，want ErrNoPerCallPrice", err)
	}
	if got := userBalance(t, db, userID); got != 1000 {
		t.Fatalf("bad snapshot changed balance: %d", got)
	}
}
