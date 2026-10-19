package billing

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestOrderVerifyRefreshesConcurrentLoser 覆盖「并发核实落败」路径：
// 第二个 VerifyOrder 在内存里看到 pending，但数据库已被第一个请求（或
// webhook）迁移为 succeeded。settleOrder 的条件 UPDATE affected=0 后
// 必须重读数据库，否则返回 paid=true 却带有 status=pending / credits=0。
func TestOrderVerifyRefreshesConcurrentLoser(t *testing.T) {
	gormDB, sqlDB := openBillingTestDB(t)
	userID := createBillingTestUser(t, sqlDB, 0)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/payment_intents/pi_loser" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"pi_loser","amount":1000,"amount_received":1000,"currency":"cny","status":"succeeded"}`)
	}))
	t.Cleanup(server.Close)
	svc := newStripeTopupService(t, gormDB, server.URL)

	var orderID int64
	if err := sqlDB.QueryRow(`
		INSERT INTO orders (user_id, provider, amount, currency, status, idempotency_key, provider_txn_id)
		VALUES ($1, 'stripe', 1000, 'CNY', 'pending', $2, 'pi_loser')
		RETURNING id`,
		userID, fmt.Sprintf("loser-%d", time.Now().UnixNano())).Scan(&orderID); err != nil {
		t.Fatalf("insert order: %v", err)
	}
	t.Cleanup(func() { _, _ = sqlDB.Exec(`DELETE FROM orders WHERE id = $1`, orderID) })

	ctx := context.Background()
	original, err := svc.GetOrder(ctx, orderID)
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	stale := *original // 模拟并发请求在入账前读到的快照

	first, err := svc.VerifyOrder(ctx, original)
	if err != nil {
		t.Fatalf("first verify: %v", err)
	}
	if !first.Paid || original.Status != string(OrderSucceeded) || original.PaidAt == nil {
		t.Fatalf("first verify result = paid %v, order %+v", first.Paid, original)
	}

	// 第二个请求携带过期的 pending 快照：DB 已 succeeded，settle 落败。
	second, err := svc.VerifyOrder(ctx, &stale)
	if err != nil {
		t.Fatalf("second verify: %v", err)
	}
	if !second.Paid {
		t.Fatalf("second verify paid = false, want true")
	}
	if stale.Status != string(OrderSucceeded) {
		t.Fatalf("second verify order status = %s, want succeeded (stale in-memory view)", stale.Status)
	}
	if stale.PaidAt == nil || !stale.PaidAt.Equal(*original.PaidAt) {
		t.Fatalf("second verify paid_at = %v, want %v", stale.PaidAt, original.PaidAt)
	}
	if stale.CreditedCredits != 1000 {
		t.Fatalf("second verify credited_credits = %d, want 1000", stale.CreditedCredits)
	}

	var walletRows int64
	if err := sqlDB.QueryRow(`SELECT count(*) FROM wallet_transactions
		WHERE user_id = $1 AND type = 'topup' AND reference_id = $2`,
		userID, fmt.Sprintf("%d", orderID)).Scan(&walletRows); err != nil {
		t.Fatalf("count wallet: %v", err)
	}
	if walletRows != 1 {
		t.Fatalf("wallet rows = %d, want exactly 1", walletRows)
	}
}
