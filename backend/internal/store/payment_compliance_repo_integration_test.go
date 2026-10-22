//go:build integration

package store

import (
	"context"
	"testing"
	"time"
)

// TestPaymentComplianceRepo 验证真实 PG 上的幂等确认语义与首次时刻稳定性。
//
//	NEWTG_TEST_DB_DSN='postgres://...' go test -tags=integration ./internal/store/ -run TestPaymentComplianceRepo -v
func TestPaymentComplianceRepo(t *testing.T) {
	db := openTestDB(t)
	repo := NewPaymentComplianceRepo(db)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	suffix := randomSuffix(t)
	var userID int64
	err := db.QueryRowContext(ctx, `
		INSERT INTO users (username, role, status, "group", language)
		VALUES ($1, 'admin', 'enabled', 'default', 'en')
		RETURNING id`, "pc"+suffix).Scan(&userID)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM users WHERE id = $1`, userID)
	})

	at, confirmed, err := repo.Status(ctx, userID)
	if err != nil {
		t.Fatalf("Status before confirm: %v", err)
	}
	if confirmed || !at.IsZero() {
		t.Fatalf("确认前 Status = (%v,%v)，期望 (zero,false)", at, confirmed)
	}

	first, newly, err := repo.Confirm(ctx, userID)
	if err != nil {
		t.Fatalf("first Confirm: %v", err)
	}
	if !newly || first.IsZero() {
		t.Fatalf("first Confirm = (%v,newly=%v)，期望非零且 newly=true", first, newly)
	}

	second, newly, err := repo.Confirm(ctx, userID)
	if err != nil {
		t.Fatalf("second Confirm: %v", err)
	}
	if newly {
		t.Fatalf("second Confirm newly=true，期望幂等 false")
	}
	if !second.Equal(first) {
		t.Fatalf("重复确认改写了首次时刻: %v vs %v", first, second)
	}

	stored, confirmed, err := repo.Status(ctx, userID)
	if err != nil {
		t.Fatalf("Status after confirm: %v", err)
	}
	if !confirmed || !stored.Equal(first) {
		t.Fatalf("Status after confirm = (%v,%v)，期望 (%v,true)", stored, confirmed, first)
	}
}
