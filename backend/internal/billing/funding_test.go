package billing

import (
	"context"
	"database/sql"
	"math"
	"strconv"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"newtg/gateway/internal/store"
)

// 本文件覆盖 M5 Task 1（裁决 P-1/P-3/P-4/P-6/P-14）：
//   - 金额（最小货币单位=分）→ 积分的换算与上下限钳制；
//   - FundingSource 默认实现「余额 + 恰一行流水」；
//   - redemption_codes.code_hash / orders.idempotency_key 的唯一约束。
//
// 集成用例与 ledger_test.go 同一手法：未设置 NEWTG_TEST_DB_DSN 时跳过。

func mustDecimal(t *testing.T, s string) decimal.Decimal {
	t.Helper()
	d, err := decimal.NewFromString(s)
	if err != nil {
		t.Fatalf("parse decimal %q: %v", s, err)
	}
	return d
}

// TestFundingComputeCredits 钉死 P-06 的定价公式：
// credits = floor(amount_cents × quota_per_unit / 100 × ratio)。
func TestFundingComputeCredits(t *testing.T) {
	cases := []struct {
		name         string
		amountCents  int64
		quotaPerUnit int64
		ratio        string
		want         int64
	}{
		// 默认口径：1 元 = 100 积分。¥10 = 1000 分 → 1000 积分。
		{"¥10 默认倍率", 1_000, 100, "1", 1_000},
		// ¥100 未配置分组（回退 1.0）→ 10,000 积分（F-04-01 示例）。
		{"¥100 默认倍率", 10_000, 100, "1", 10_000},
		// pay.topup_group_ratio 配 {vip: 1.2} → 12,000 积分（F-04-01 示例）。
		{"¥100 vip 1.2", 10_000, 100, "1.2", 12_000},
		// 向下取整：133 分 × 1.5 = 199.5 → 199。
		{"向下取整", 133, 100, "1.5", 199},
		// quota_per_unit 可配置：N=200 时 ¥1 = 200 积分。
		{"自定义配额粒度", 100, 200, "1", 200},
		// 单笔硬上限恰好合法：¥100,000 = 10,000,000 分 → 10,000,000 积分。
		{"硬上限金额", 10_000_000, 100, "1", 10_000_000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ComputeCredits(tc.amountCents, tc.quotaPerUnit, mustDecimal(t, tc.ratio))
			if err != nil {
				t.Fatalf("ComputeCredits(%d, %d, %s): %v", tc.amountCents, tc.quotaPerUnit, tc.ratio, err)
			}
			if got != tc.want {
				t.Fatalf("credits = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestFundingComputeCreditsRejects 覆盖非法输入：金额/粒度非正、倍率非正、
// 结果溢出 int64。所有非法输入都必须显式报错而不是静默钳制。
func TestFundingComputeCreditsRejects(t *testing.T) {
	one := decimal.NewFromInt(1)
	for _, tc := range []struct {
		name         string
		amountCents  int64
		quotaPerUnit int64
		ratio        decimal.Decimal
	}{
		{"金额为 0", 0, 100, one},
		{"金额为负", -1, 100, one},
		{"配额粒度为 0", 100, 0, one},
		{"配额粒度为负", 100, -100, one},
		{"倍率为 0", 100, 100, decimal.Zero},
		{"倍率为负", 100, 100, decimal.NewFromInt(-1)},
		{"结果溢出 int64", math.MaxInt64, 200, one},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ComputeCredits(tc.amountCents, tc.quotaPerUnit, tc.ratio); err == nil {
				t.Fatalf("ComputeCredits(%d, %d, %s) = nil error, want rejection",
					tc.amountCents, tc.quotaPerUnit, tc.ratio)
			}
		})
	}
}

// TestFundingValidateTopupAmount 覆盖全局最低 / 渠道最低 / 单笔硬上限：
// 实际下限 = max(全局最低, 渠道最低)；越界一律拒绝（F-04-01 异常流程）。
func TestFundingValidateTopupAmount(t *testing.T) {
	const (
		globalMin = int64(1_000)      // pay.min_topup_cny 默认 10 元
		provider  = int64(2_000)      // pay.stripe.min_topup_cny 示例 20 元
		max       = int64(10_000_000) // 单笔硬上限 ¥100,000
	)

	ok := []struct {
		name        string
		amountCents int64
		providerMin int64
	}{
		{"恰好等于全局最低", globalMin, 0},
		{"高于全局最低", 5_000, 0},
		{"恰好等于渠道最低", provider, provider},
		{"恰好等于硬上限", max, provider},
	}
	for _, tc := range ok {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateTopupAmount(tc.amountCents, globalMin, tc.providerMin, max); err != nil {
				t.Fatalf("ValidateTopupAmount(%d): %v", tc.amountCents, err)
			}
		})
	}

	bad := []struct {
		name        string
		amountCents int64
		providerMin int64
	}{
		{"低于全局最低", globalMin - 1, 0},
		{"低于渠道最低", provider - 1, provider},
		{"超过硬上限", max + 1, provider},
		{"金额为 0", 0, 0},
		{"金额为负", -100, 0},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateTopupAmount(tc.amountCents, globalMin, tc.providerMin, max); err == nil {
				t.Fatalf("ValidateTopupAmount(%d) = nil, want error", tc.amountCents)
			}
		})
	}

	// 渠道最低高于硬上限属于配置错误：即使金额在硬上限内也不可通过。
	if err := ValidateTopupAmount(max, globalMin, max+1, max); err == nil {
		t.Fatal("渠道最低高于硬上限时应拒绝")
	}
}

// TestFundingTopupGroupRatio 覆盖 pay.topup_group_ratio 的 jsonb 解析：
// 未配置分组回退 1.0；ratio ≤ 0 / 非有限值 / 非数字一律视为配置非法。
func TestFundingTopupGroupRatio(t *testing.T) {
	ratios, err := ParseTopupGroupRatios([]byte(`{"vip":1.2,"svip":2}`))
	if err != nil {
		t.Fatalf("ParseTopupGroupRatios: %v", err)
	}
	if got, err := GroupRatio(ratios, "vip"); err != nil || !got.Equal(decimal.RequireFromString("1.2")) {
		t.Fatalf("vip ratio = %s, %v; want 1.2", got, err)
	}
	if got, err := GroupRatio(ratios, "svip"); err != nil || !got.Equal(decimal.NewFromInt(2)) {
		t.Fatalf("svip ratio = %s, %v; want 2", got, err)
	}
	// 未配置分组回退 1.0（不报错、不阻断充值）。
	if got, err := GroupRatio(ratios, "default"); err != nil || !got.Equal(decimal.NewFromInt(1)) {
		t.Fatalf("default ratio = %s, %v; want 1", got, err)
	}
	// 空配置（默认 {}）同样回退 1.0。
	if ratios, err := ParseTopupGroupRatios([]byte(`{}`)); err != nil || len(ratios) != 0 {
		t.Fatalf("parse {} = %v, %v; want empty map", ratios, err)
	}
	if got, err := GroupRatio(nil, "vip"); err != nil || !got.Equal(decimal.NewFromInt(1)) {
		t.Fatalf("nil map ratio = %s, %v; want 1", got, err)
	}

	for _, raw := range []string{
		`{"vip":0}`,
		`{"vip":-1}`,
		`{"vip":"NaN"}`,
		`{"vip":"Infinity"}`,
		`{"vip":"not-a-number"}`,
		`not-json`,
	} {
		if _, err := ParseTopupGroupRatios([]byte(raw)); err == nil {
			t.Errorf("ParseTopupGroupRatios(%s) = nil, want error", raw)
		}
	}
}

// TestFundingApply 覆盖 FundingSource 默认实现：入账正确、恰一行流水、
// type/reference_type 按 P-3 落库（order / redemption，绝不写渠道名）。
func TestFundingApply(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 0)
	funding := NewWalletFunding(gormDB)
	ctx := context.Background()

	balanceAfter, txID, err := funding.Apply(ctx, Credit{
		UserID:        userID,
		Amount:        1_000,
		Type:          CreditTypeTopup,
		ReferenceType: "order",
		ReferenceID:   "42",
	})
	if err != nil {
		t.Fatalf("Apply(order): %v", err)
	}
	if balanceAfter != 1_000 {
		t.Fatalf("balanceAfter = %d, want 1000", balanceAfter)
	}
	if txID == "" {
		t.Fatal("txID 为空，充值路径需要流水 id 做对账锚点")
	}

	rows := readFundingRows(t, db, userID)
	if len(rows) != 1 {
		t.Fatalf("wallet rows = %d, want 1（每笔入账恰一行）", len(rows))
	}
	want := fundingRow{ID: rows[0].ID, Type: "topup", Amount: 1_000, BalanceAfter: 1_000,
		Status: "confirmed", ReferenceType: "order", ReferenceID: "42"}
	if rows[0] != want {
		t.Fatalf("row = %+v, want %+v", rows[0], want)
	}
	if got := strconv.FormatInt(rows[0].ID, 10); got != txID {
		t.Fatalf("txID = %s, want wallet_transactions.id %s", txID, got)
	}

	// 兑换码路径：reference_type='redemption' + 兑换码 id（P-3）。
	balanceAfter, _, err = funding.Apply(ctx, Credit{
		UserID:        userID,
		Amount:        500,
		Type:          CreditTypeTopup,
		ReferenceType: "redemption",
		ReferenceID:   "7",
	})
	if err != nil {
		t.Fatalf("Apply(redemption): %v", err)
	}
	if balanceAfter != 1_500 {
		t.Fatalf("balanceAfter = %d, want 1500", balanceAfter)
	}
	rows = readFundingRows(t, db, userID)
	if len(rows) != 2 {
		t.Fatalf("wallet rows = %d, want 2", len(rows))
	}
	if rows[1].ReferenceType != "redemption" || rows[1].ReferenceID != "7" {
		t.Fatalf("row1 = %+v, want reference redemption/7", rows[1])
	}
}

// TestFundingApplyRejects 覆盖 FundingSource 的应用层校验：非正金额、
// 非法流水类型、渠道名作 reference_type、缺引用一律拒绝且不落任何行。
func TestFundingApplyRejects(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 100)
	funding := NewWalletFunding(gormDB)
	ctx := context.Background()

	bad := []Credit{
		{UserID: userID, Amount: 0, Type: CreditTypeTopup, ReferenceType: "order", ReferenceID: "1"},
		{UserID: userID, Amount: -10, Type: CreditTypeTopup, ReferenceType: "order", ReferenceID: "1"},
		{UserID: userID, Amount: 10, Type: "consume", ReferenceType: "order", ReferenceID: "1"},
		// P-3：支付渠道名绝不作为 wallet_transactions.reference_type。
		{UserID: userID, Amount: 10, Type: CreditTypeTopup, ReferenceType: "stripe", ReferenceID: "1"},
		{UserID: userID, Amount: 10, Type: CreditTypeTopup, ReferenceType: "alipay", ReferenceID: "1"},
		{UserID: userID, Amount: 10, Type: CreditTypeTopup, ReferenceType: "order", ReferenceID: ""},
		{UserID: 0, Amount: 10, Type: CreditTypeTopup, ReferenceType: "order", ReferenceID: "1"},
	}
	for i, c := range bad {
		if _, _, err := funding.Apply(ctx, c); err == nil {
			t.Errorf("case %d Apply(%+v) = nil, want error", i, c)
		}
	}
	if got := userBalance(t, db, userID); got != 100 {
		t.Fatalf("balance = %d, want 100（拒绝路径不得改动余额）", got)
	}
	if rows := readFundingRows(t, db, userID); len(rows) != 0 {
		t.Fatalf("rows = %d, want 0", len(rows))
	}
}

// TestFundingDataModelUniqueness 钉死 P-4/P-14 的数据模型口径：
// redemption_codes 只有 code_hash 且 UNIQUE；orders.idempotency_key UNIQUE；
// 结构体字段与 platform-01 §9/§10 一一对应（含可空列的三值语义）。
func TestFundingDataModelUniqueness(t *testing.T) {
	gormDB, db := openBillingTestDB(t)

	code := store.RedemptionCode{
		CodeHash:  "sha256-" + uniqueFundingSuffix(),
		Credits:   1_000,
		Status:    "unused",
		BatchName: ptrString("批次-20261019"),
		CreatedBy: 1,
	}
	if err := gormDB.Create(&code).Error; err != nil {
		t.Fatalf("create redemption code: %v", err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM redemption_codes WHERE id = $1`, code.ID) })
	if code.ID == 0 {
		t.Fatal("redemption code id 未回填")
	}

	dup := code
	dup.ID = 0
	dup.BatchName = nil
	if err := gormDB.Create(&dup).Error; err == nil {
		t.Fatal("重复 code_hash 插入成功，UNIQUE 约束缺失")
	}

	// 可空列必须能落 NULL（used_by/used_at/expires_at 三值语义）。
	var usedBy sql.NullInt64
	var usedAt, expiresAt sql.NullTime
	if err := db.QueryRow(`SELECT used_by, used_at, expires_at FROM redemption_codes WHERE id = $1`, code.ID).
		Scan(&usedBy, &usedAt, &expiresAt); err != nil {
		t.Fatalf("read redemption code: %v", err)
	}
	if usedBy.Valid || usedAt.Valid || expiresAt.Valid {
		t.Fatalf("可空列应为 NULL, got used_by=%v used_at=%v expires_at=%v", usedBy, usedAt, expiresAt)
	}

	key := "idem-" + uniqueFundingSuffix()
	order := store.Order{
		UserID:         1,
		Provider:       "stripe",
		Amount:         10_000,
		Currency:       "CNY",
		Status:         "pending",
		IdempotencyKey: key,
	}
	if err := gormDB.Create(&order).Error; err != nil {
		t.Fatalf("create order: %v", err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM orders WHERE id = $1`, order.ID) })
	if order.ID == 0 {
		t.Fatal("order id 未回填")
	}

	dupOrder := order
	dupOrder.ID = 0
	dupOrder.ProviderTxnID = nil
	if err := gormDB.Create(&dupOrder).Error; err == nil {
		t.Fatal("重复 idempotency_key 插入成功，UNIQUE 约束缺失")
	}

	// 默认值由数据库补齐：credited_credits=0、manual_completed=false、可空列为 NULL。
	var credited int64
	var manual bool
	var providerTxn sql.NullString
	var paidAt sql.NullTime
	if err := db.QueryRow(
		`SELECT credited_credits, manual_completed, provider_txn_id, paid_at
		 FROM orders WHERE id = $1`, order.ID).
		Scan(&credited, &manual, &providerTxn, &paidAt); err != nil {
		t.Fatalf("read order: %v", err)
	}
	if credited != 0 || manual || providerTxn.Valid || paidAt.Valid {
		t.Fatalf("order defaults = credited=%d manual=%v txn=%v paid=%v, want 0/false/NULL/NULL",
			credited, manual, providerTxn, paidAt)
	}
}

// ---------------------------------------------------------------------------
// 测试辅助
// ---------------------------------------------------------------------------

type fundingRow struct {
	ID            int64
	Type          string
	Amount        int64
	BalanceAfter  int64
	Status        string
	ReferenceType string
	ReferenceID   string
}

// readFundingRows 返回某用户全部充值/兑换流水（按 id 升序）。
func readFundingRows(t *testing.T, db *sql.DB, userID int64) []fundingRow {
	t.Helper()
	rows, err := db.Query(
		`SELECT id, type, amount, balance_after, status,
		        COALESCE(reference_type, ''), COALESCE(reference_id, '')
		 FROM wallet_transactions WHERE user_id = $1 ORDER BY id`, userID)
	if err != nil {
		t.Fatalf("read wallet rows: %v", err)
	}
	defer rows.Close()
	var out []fundingRow
	for rows.Next() {
		var r fundingRow
		if err := rows.Scan(&r.ID, &r.Type, &r.Amount, &r.BalanceAfter, &r.Status,
			&r.ReferenceType, &r.ReferenceID); err != nil {
			t.Fatalf("scan wallet row: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate wallet rows: %v", err)
	}
	return out
}

var fundingSeq int64

// uniqueFundingSuffix 生成进程内唯一后缀，避免共享测试库上的 UNIQUE 冲突。
func uniqueFundingSuffix() string {
	fundingSeq++
	return strconv.FormatInt(time.Now().UnixNano(), 10) + "-" + strconv.FormatInt(fundingSeq, 10)
}

func ptrString(s string) *string { return &s }
