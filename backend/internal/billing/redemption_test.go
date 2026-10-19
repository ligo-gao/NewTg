package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"newtg/gateway/internal/contract"
)

// 本文件是 M5 Task 2（兑换码 CRUD + 防双花核销）的计费侧验收。
//
// 集成测试需要真实 PostgreSQL（未设置 NEWTG_TEST_DB_DSN 时跳过，与
// ledger_test.go 同手法）：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_test?sslmode=disable' \
//	go test ./internal/billing/ -run 'TestRedemption|TestRedeem' -v
//
// 覆盖口径（P-4/P-5/P-6/P-15/GC）：
//   - 归一化 trim → 大写 → I/L→1、O→0 → SHA-256 小写 hex；
//   - 26 位 Crockford Base32、只存哈希、明文仅生成响应出现一次；
//   - 条件 UPDATE 防双花（20 并发仅 1 成功、恰一行 topup 流水）；
//   - 过期边界 expires_at == now 视为过期（虚拟态不落库）；
//   - topup_group_ratio 应用与 ≤0 / 溢出拒绝（不得放大余额）；
//   - 入账失败时整个事务回滚，兑换码保持 unused。

var redemptionTestSeq atomic.Int64

// redemptionTestConfig 返回确定性配置：1 元 = 100 积分，vip 倍率 1.2。
func redemptionTestConfig(extra string) ConfigReader {
	cfg := mapConfig{
		ConfigQuotaPerUnit: json.RawMessage("100"),
	}
	if extra != "" {
		cfg[ConfigTopupGroupRatio] = json.RawMessage(extra)
	}
	return cfg
}

// insertRedemptionCode 直接写一行兑换码（生产路径只写哈希，测试同样）。
func insertRedemptionCode(t *testing.T, db *gorm.DB, plaintext string, credits int64,
	status string, expiresAt *time.Time) int64 {
	t.Helper()
	var id int64
	err := db.Raw(`INSERT INTO redemption_codes
		(code_hash, credits, status, batch_name, created_by, expires_at)
		VALUES (?, ?, ?, 'test-batch', 1, ?)
		RETURNING id`,
		HashRedemptionCode(NormalizeRedemptionCode(plaintext)), credits, status, expiresAt).
		Row().Scan(&id)
	if err != nil {
		t.Fatalf("insert redemption code: %v", err)
	}
	return id
}

func TestRedemptionNormalizeAndHash(t *testing.T) {
	// trim → 大写 → Crockford 别名（I/L→1、O→0）。
	cases := map[string]string{
		"  abcd0123  ":           "ABCD0123",
		"iIlLoO":                 "111100",
		"ABCDEFGHJKMNPQRSTVWXYZ": "ABCDEFGHJKMNPQRSTVWXYZ",
	}
	for in, want := range cases {
		if got := NormalizeRedemptionCode(in); got != want {
			t.Errorf("NormalizeRedemptionCode(%q) = %q, want %q", in, got, want)
		}
	}
	hash := HashRedemptionCode("ABCD0123")
	sum := sha256.Sum256([]byte("ABCD0123"))
	if hash != hex.EncodeToString(sum[:]) {
		t.Fatalf("HashRedemptionCode = %q, want SHA-256 lowercase hex %q", hash, hex.EncodeToString(sum[:]))
	}
	if strings.ToLower(hash) != hash {
		t.Fatalf("hash 必须是小写 hex: %q", hash)
	}
}

func TestGenerateRedemptionCodes(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	svc := NewRedemptionService(gormDB, NewWalletFunding(gormDB), redemptionTestConfig(""))
	expires := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)

	batchName := fmt.Sprintf("批次-%d", time.Now().UnixNano()%1_000_000)
	codes, err := svc.GenerateCodes(context.Background(), &batchName, 1_000, 5, &expires, 7)
	if err != nil {
		t.Fatalf("GenerateCodes: %v", err)
	}
	if len(codes) != 5 {
		t.Fatalf("codes = %d, want 5", len(codes))
	}
	seen := make(map[string]bool)
	for _, code := range codes {
		if len(code) != RedemptionCodeLength {
			t.Fatalf("code %q 长度 = %d, want %d", code, len(code), RedemptionCodeLength)
		}
		for _, r := range code {
			if !strings.ContainsRune(RedemptionAlphabet, r) {
				t.Fatalf("code %q 含非 Crockford 字符 %q", code, r)
			}
		}
		if seen[code] {
			t.Fatalf("批次内出现重复码 %q", code)
		}
		seen[code] = true

		// 库中只存哈希，且哈希可由归一化明文重算。
		var hash string
		if err := db.QueryRow(`SELECT code_hash FROM redemption_codes WHERE code_hash = $1`,
			HashRedemptionCode(NormalizeRedemptionCode(code))).Scan(&hash); err != nil {
			t.Fatalf("读取 code_hash: %v", err)
		}
		if strings.Contains(hash, code) {
			t.Fatalf("code_hash 泄露明文: %q", hash)
		}
	}

	// 清理：按批次前缀删除。
	if _, err := db.Exec(`DELETE FROM redemption_codes WHERE batch_name LIKE '批次-%'`); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
}

func TestGenerateRedemptionCodesValidation(t *testing.T) {
	gormDB, _ := openBillingTestDB(t)
	svc := NewRedemptionService(gormDB, NewWalletFunding(gormDB), nil)
	future := time.Now().Add(time.Hour)
	past := time.Now().Add(-time.Minute)
	batch := ptrString("batch")

	cases := []struct {
		name    string
		credits int64
		count   int
		expires *time.Time
		batch   *string
	}{
		{"count=0", 100, 0, nil, batch},
		{"count=101", 100, 101, nil, batch},
		{"credits=0", 0, 1, nil, batch},
		{"credits<0", -1, 1, nil, batch},
		{"expired", 100, 1, &past, batch},
		{"missing batch", 100, 1, &future, nil},
	}
	for _, tc := range cases {
		if _, err := svc.GenerateCodes(context.Background(), tc.batch, tc.credits, tc.count, tc.expires, 1); err == nil {
			t.Errorf("%s: GenerateCodes = nil, want error", tc.name)
		}
	}
}

// TestGenerateRedemptionCodesRetriesCollision 证明碰撞重试在 PostgreSQL 上
// 真的可用：23505 会中止整个事务，因此实现必须用 ON CONFLICT DO NOTHING
// 把冲突降级为 0 行影响。注入已知碰撞序列强制第一次命中。
func TestGenerateRedemptionCodesRetriesCollision(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	svc := NewRedemptionService(gormDB, NewWalletFunding(gormDB), nil)

	// 注入值只用 Crockford 字母表字符，保证与生成路径的哈希口径一致。
	colliding := "C011" + strings.Repeat("A", 22)
	collidingID := insertRedemptionCode(t, gormDB, colliding, 999, "unused", nil)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM redemption_codes WHERE id = $1`, collidingID) })

	unique := "B2C4" + strings.Repeat("B", 22)
	calls := 0
	svc.generateCode = func() (string, error) {
		calls++
		if calls == 1 {
			return colliding, nil
		}
		return unique, nil
	}

	batch := fmt.Sprintf("col-%d", time.Now().UnixNano()%1_000_000_000)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM redemption_codes WHERE batch_name = $1`, batch) })

	codes, err := svc.GenerateCodes(context.Background(), &batch, 100, 1, nil, 1)
	if err != nil {
		t.Fatalf("GenerateCodes with collision: %v", err)
	}
	if calls != 2 {
		t.Fatalf("generator calls = %d, want 2（第一次碰撞后必须重试）", calls)
	}
	if len(codes) != 1 || codes[0] != unique {
		t.Fatalf("codes = %v, want [%s]", codes, unique)
	}
	var stored int
	if err := db.QueryRow(`SELECT count(*) FROM redemption_codes WHERE code_hash = $1`,
		HashRedemptionCode(unique)).Scan(&stored); err != nil {
		t.Fatalf("read retried code: %v", err)
	}
	if stored != 1 {
		t.Fatalf("重试后的码落库 %d 行, want 1", stored)
	}
}

func TestRedeemCreditsAndRatio(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	group := "vip-" + uniqueFundingSuffix()
	if _, err := db.Exec(`INSERT INTO user_groups (name, ratio, user_selectable, is_system, status)
		VALUES ($1, 120, true, false, 1)`, group); err != nil {
		t.Fatalf("create group: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM user_groups WHERE name = $1`, group) })

	userID := createBillingTestUser(t, db, 0)
	if _, err := db.Exec(`UPDATE users SET "group" = $1 WHERE id = $2`, group, userID); err != nil {
		t.Fatalf("assign group: %v", err)
	}

	svc := NewRedemptionService(gormDB, NewWalletFunding(gormDB),
		redemptionTestConfig(fmt.Sprintf(`{"%s":1.2}`, group)))
	plain := "TEST" + strings.Repeat("A", 22)
	codeID := insertRedemptionCode(t, gormDB, plain, 10_000, "unused", nil)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM redemption_codes WHERE id = $1`, codeID) })

	balanceAfter, err := svc.Redeem(context.Background(), userID, "  "+strings.ToLower(plain)+" ")
	if err != nil {
		t.Fatalf("Redeem: %v", err)
	}
	// floor(10000 × 100 / 100 × 1.2) = 12000。
	if balanceAfter != 12_000 {
		t.Fatalf("balanceAfter = %d, want 12000", balanceAfter)
	}
	if got := userBalance(t, db, userID); got != 12_000 {
		t.Fatalf("users.balance = %d, want 12000", got)
	}
	rows := readFundingRows(t, db, userID)
	if len(rows) != 1 {
		t.Fatalf("wallet rows = %d, want 1", len(rows))
	}
	row := rows[0]
	if row.Type != CreditTypeTopup || row.Amount != 12_000 || row.Status != "confirmed" {
		t.Fatalf("wallet row = %+v, want topup/12000/confirmed", row)
	}
	if row.ReferenceType != "redemption" || row.ReferenceID != fmt.Sprint(codeID) {
		t.Fatalf("reference = %s/%s, want redemption/%d", row.ReferenceType, row.ReferenceID, codeID)
	}

	var status string
	var usedBy *int64
	if err := db.QueryRow(`SELECT status, used_by FROM redemption_codes WHERE id = $1`, codeID).
		Scan(&status, &usedBy); err != nil {
		t.Fatalf("read code: %v", err)
	}
	if status != "used" || usedBy == nil || *usedBy != userID {
		t.Fatalf("code = %s used_by=%v, want used/%d", status, usedBy, userID)
	}
}

func TestRedeemConcurrentOnlyOnce(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 0)
	svc := NewRedemptionService(gormDB, NewWalletFunding(gormDB), redemptionTestConfig(""))

	plain := "CONC" + strings.Repeat("B", 22)
	codeID := insertRedemptionCode(t, gormDB, plain, 500, "unused", nil)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM redemption_codes WHERE id = $1`, codeID) })

	const n = 20
	var wg sync.WaitGroup
	var success atomic.Int32
	errs := make(chan error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := svc.Redeem(context.Background(), userID, plain); err == nil {
				success.Add(1)
			} else {
				errs <- err
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)

	if got := success.Load(); got != 1 {
		t.Fatalf("并发成功次数 = %d, want 1", got)
	}
	for err := range errs {
		if !errors.Is(err, ErrRedemptionCodeUsed) {
			t.Fatalf("失败原因 = %v, want ErrRedemptionCodeUsed", err)
		}
	}
	if got := userBalance(t, db, userID); got != 500 {
		t.Fatalf("balance = %d, want 500（只入账一次）", got)
	}
	if rows := readFundingRows(t, db, userID); len(rows) != 1 {
		t.Fatalf("wallet rows = %d, want 1", len(rows))
	}
}

func TestRedeemReasonMapping(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	svc := NewRedemptionService(gormDB, NewWalletFunding(gormDB), redemptionTestConfig(""))
	userID := createBillingTestUser(t, db, 0)
	ctx := context.Background()

	past := time.Now().Add(-time.Minute)
	usedID := insertRedemptionCode(t, gormDB, "USED"+strings.Repeat("C", 22), 100, "unused", nil)
	if _, err := db.Exec(`UPDATE redemption_codes SET status='used', used_by=$2, used_at=now() WHERE id=$1`, usedID, userID); err != nil {
		t.Fatalf("mark used: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM redemption_codes WHERE id = $1`, usedID) })

	disabledID := insertRedemptionCode(t, gormDB, "DSBL"+strings.Repeat("D", 22), 100, "disabled", nil)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM redemption_codes WHERE id = $1`, disabledID) })

	expiredID := insertRedemptionCode(t, gormDB, "EXPR"+strings.Repeat("E", 22), 100, "unused", &past)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM redemption_codes WHERE id = $1`, expiredID) })

	// 恰好等于当前时刻也视为过期（条件 expires_at > now()）。
	boundary := time.Now()
	boundaryID := insertRedemptionCode(t, gormDB, "BNDR"+strings.Repeat("F", 22), 100, "unused", &boundary)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM redemption_codes WHERE id = $1`, boundaryID) })

	cases := []struct {
		name string
		code string
		want error
	}{
		{"invalid", "ZZZZ" + strings.Repeat("Z", 22), ErrRedemptionCodeInvalid},
		{"used", "USED" + strings.Repeat("C", 22), ErrRedemptionCodeUsed},
		{"disabled", "DSBL" + strings.Repeat("D", 22), ErrRedemptionCodeDisabled},
		{"expired", "EXPR" + strings.Repeat("E", 22), ErrRedemptionCodeExpired},
		{"boundary", "BNDR" + strings.Repeat("F", 22), ErrRedemptionCodeExpired},
	}
	for _, tc := range cases {
		_, err := svc.Redeem(ctx, userID, tc.code)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
	if got := userBalance(t, db, userID); got != 0 {
		t.Fatalf("拒绝路径不得入账, balance = %d", got)
	}
	if rows := readFundingRows(t, db, userID); len(rows) != 0 {
		t.Fatalf("拒绝路径不得写流水, rows = %d", len(rows))
	}
}

func TestRedeemRatioRejected(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 0)
	ctx := context.Background()

	for _, raw := range []string{`{"default":0}`, `{"default":-1.5}`} {
		svc := NewRedemptionService(gormDB, NewWalletFunding(gormDB), redemptionTestConfig(raw))
		plain := "RAT" + uniqueFundingSuffix()
		plain = plain + strings.Repeat("G", RedemptionCodeLength-len(plain))
		id := insertRedemptionCode(t, gormDB, plain, 1_000, "unused", nil)
		if _, err := svc.Redeem(ctx, userID, plain); !errors.Is(err, ErrInvalidTopupRatio) {
			t.Errorf("ratio %s: err = %v, want ErrInvalidTopupRatio", raw, err)
		}
		var status string
		if err := db.QueryRow(`SELECT status FROM redemption_codes WHERE id=$1`, id).Scan(&status); err != nil {
			t.Fatalf("read status: %v", err)
		}
		if status != "unused" {
			t.Fatalf("ratio %s: code status = %s, want unused（事务必须回滚）", raw, status)
		}
		_, _ = db.Exec(`DELETE FROM redemption_codes WHERE id = $1`, id)
	}

	// 极小倍率 + 小面额：向下取整为 0 也必须拒绝（P-6 非正结果）。
	tiny := NewRedemptionService(gormDB, NewWalletFunding(gormDB), redemptionTestConfig(`{"default":0.000001}`))
	tinyPlain := "TINY" + strings.Repeat("K", 22)
	tinyID := insertRedemptionCode(t, gormDB, tinyPlain, 1, "unused", nil)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM redemption_codes WHERE id = $1`, tinyID) })
	if _, err := tiny.Redeem(ctx, userID, tinyPlain); !errors.Is(err, ErrInvalidFundingCredit) {
		t.Fatalf("tiny ratio err = %v, want ErrInvalidFundingCredit", err)
	}
	var tinyStatus string
	if err := db.QueryRow(`SELECT status FROM redemption_codes WHERE id=$1`, tinyID).Scan(&tinyStatus); err != nil {
		t.Fatalf("read tiny status: %v", err)
	}
	if tinyStatus != "unused" {
		t.Fatalf("tiny ratio 后 code status = %s, want unused", tinyStatus)
	}

	// 溢出面额：QPU=1,000,000 时 credits × 10000 必须被拒绝（不静默钳制）。
	svc := NewRedemptionService(gormDB, NewWalletFunding(gormDB), mapConfig{
		ConfigQuotaPerUnit: json.RawMessage("1000000"),
	})
	plain := "OVFL" + strings.Repeat("H", 22)
	id := insertRedemptionCode(t, gormDB, plain, math.MaxInt64/10_000+1, "unused", nil)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM redemption_codes WHERE id = $1`, id) })
	if _, err := svc.Redeem(ctx, userID, plain); !errors.Is(err, ErrCreditOverflow) {
		t.Fatalf("overflow err = %v, want ErrCreditOverflow", err)
	}
	if got := userBalance(t, db, userID); got != 0 {
		t.Fatalf("overflow 后 balance = %d, want 0", got)
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM redemption_codes WHERE id=$1`, id).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "unused" {
		t.Fatalf("overflow 后 code status = %s, want unused", status)
	}
}

// TestRedeemCreditFailureRollsBack 钉死 P-15：条件 UPDATE 与入账在同一事务，
// 入账失败（钱包上界溢出）时兑换码必须回到 unused，绝不能出现「码已核销、钱没到」。
func TestRedeemCreditFailureRollsBack(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, contract.MaxWalletQuota)
	svc := NewRedemptionService(gormDB, NewWalletFunding(gormDB), redemptionTestConfig(""))

	plain := "ROLL" + strings.Repeat("J", 22)
	id := insertRedemptionCode(t, gormDB, plain, 100, "unused", nil)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM redemption_codes WHERE id = $1`, id) })

	if _, err := svc.Redeem(context.Background(), userID, plain); !errors.Is(err, ErrWalletLimit) {
		t.Fatalf("err = %v, want ErrWalletLimit", err)
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM redemption_codes WHERE id=$1`, id).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "unused" {
		t.Fatalf("code status = %s, want unused（入账失败必须整体回滚）", status)
	}
	if rows := readFundingRows(t, db, userID); len(rows) != 0 {
		t.Fatalf("wallet rows = %d, want 0", len(rows))
	}
}
