package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"newtg/gateway/internal/auth"
)

// 钱包摘要端点（modules-03 §F-03-01）验收：未认证 401、只读本人数据、
// 冻结/累计消费聚合与 health 口径。需要真实 PostgreSQL：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_test?sslmode=disable' \
//	go test ./internal/httpapi/ -run TestWalletSummary -v

type walletTestConfig map[string]json.RawMessage

func (m walletTestConfig) Get(key string) (json.RawMessage, bool) {
	v, ok := m[key]
	return v, ok
}

func newWalletTestHandler(t *testing.T, cfg ConfigReader) (*sql.DB, http.Handler) {
	t.Helper()
	dsn := os.Getenv("NEWTG_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("NEWTG_TEST_DB_DSN 未设置，跳过钱包摘要集成测试")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		t.Fatalf("ping test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// usage_logs normally lives in the separate logs DB; this integration test
	// uses the same DSN and a minimal table to exercise the aggregate query.
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS usage_logs (
		id bigint GENERATED ALWAYS AS IDENTITY,
		user_id bigint NOT NULL,
		cost bigint NOT NULL DEFAULT 0,
		created_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		t.Fatalf("create usage_logs: %v", err)
	}

	h, err := NewWalletHandlers(WalletHandlerConfig{
		DB:     db,
		LogDB:  db,
		Config: cfg,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewWalletHandlers: %v", err)
	}
	mux := http.NewServeMux()
	h.RegisterWalletRoutes(mux, nil)
	return db, mux
}

func walletTestUser(t *testing.T, db *sql.DB, balance int64) int64 {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO user_groups (name, ratio, user_selectable, is_system, status)
		 VALUES ('default', 100, true, true, 1) ON CONFLICT (name) DO NOTHING`); err != nil {
		t.Fatalf("ensure default group: %v", err)
	}
	name := fmt.Sprintf("wlt%d", time.Now().UnixNano()%1_000_000_000_000)
	var id int64
	if err := db.QueryRow(
		`INSERT INTO users (username, balance, "group") VALUES ($1, $2, 'default') RETURNING id`,
		name, balance,
	).Scan(&id); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM wallet_transactions WHERE user_id = $1`, id)
		_, _ = db.Exec(`DELETE FROM users WHERE id = $1`, id)
	})
	return id
}

func walletTestLedger(t *testing.T, db *sql.DB, userID int64, kind, status string, amount int64) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO wallet_transactions (user_id, type, amount, balance_after, status)
		 VALUES ($1, $2, $3, 0, $4)`,
		userID, kind, amount, status,
	); err != nil {
		t.Fatalf("insert wallet transaction: %v", err)
	}
}

func walletTestUsage(t *testing.T, db *sql.DB, userID, cost int64) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO usage_logs (user_id, cost) VALUES ($1, $2)`, userID, cost,
	); err != nil {
		t.Fatalf("insert usage log: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM usage_logs WHERE user_id = $1`, userID)
	})
}

func doWalletSummary(t *testing.T, handler http.Handler, userID int64) (*httptest.ResponseRecorder, walletSummaryView) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/wallet/summary", nil)
	if userID > 0 {
		req = req.WithContext(auth.WithPrincipal(context.Background(), &auth.Principal{
			UserID: userID,
			Role:   "user",
		}))
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var view walletSummaryView
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
			t.Fatalf("decode summary: %v (body=%s)", err, rec.Body.String())
		}
	}
	return rec, view
}

func TestWalletSummaryRequiresAuthentication(t *testing.T) {
	_, handler := newWalletTestHandler(t, nil)

	rec, _ := doWalletSummary(t, handler, 0)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestWalletSummaryReadsOnlySelfAndAggregates(t *testing.T) {
	cfg := walletTestConfig{
		ConfigBalanceAlertThreshold: json.RawMessage("5000"),
	}
	db, handler := newWalletTestHandler(t, cfg)

	alice := walletTestUser(t, db, 1500)
	bob := walletTestUser(t, db, 99999)
	walletTestLedger(t, db, alice, "consume_pre", "hold", -300)
	walletTestLedger(t, db, alice, "consume", "confirmed", -700)
	walletTestUsage(t, db, alice, 700)

	rec, aliceView := doWalletSummary(t, handler, alice)
	if rec.Code != http.StatusOK {
		t.Fatalf("alice status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if aliceView.Balance != 1500 {
		t.Errorf("alice balance = %d, want 1500", aliceView.Balance)
	}
	if aliceView.HeldCredits != 300 {
		t.Errorf("held_credits = %d, want 300", aliceView.HeldCredits)
	}
	if aliceView.TotalConsumed != 700 {
		t.Errorf("total_consumed = %d, want 700", aliceView.TotalConsumed)
	}
	if aliceView.TotalRequests != 1 {
		t.Errorf("total_requests = %d, want 1", aliceView.TotalRequests)
	}
	if aliceView.Threshold != 5000 {
		t.Errorf("threshold = %d, want 5000", aliceView.Threshold)
	}
	if aliceView.Health != "low" {
		t.Errorf("health = %q, want low", aliceView.Health)
	}
	if aliceView.Group != "default" {
		t.Errorf("group = %q, want default", aliceView.Group)
	}

	// The endpoint has no user-id parameter: a different session gets its own row.
	_, bobView := doWalletSummary(t, handler, bob)
	if bobView.Balance != 99999 {
		t.Errorf("bob balance = %d, want 99999 (self-only read)", bobView.Balance)
	}
	if bobView.Health != "healthy" {
		t.Errorf("bob health = %q, want healthy", bobView.Health)
	}
}

func TestWalletSummaryHealthUsesFrozenThresholdDefault(t *testing.T) {
	db, handler := newWalletTestHandler(t, nil)

	// Missing config key → default 1000; balance 999 is low, 0 is depleted.
	low := walletTestUser(t, db, 999)
	empty := walletTestUser(t, db, 0)

	_, lowView := doWalletSummary(t, handler, low)
	if lowView.Threshold != defaultBalanceAlertThreshold {
		t.Errorf("threshold = %d, want %d", lowView.Threshold, defaultBalanceAlertThreshold)
	}
	if lowView.Health != "low" {
		t.Errorf("health = %q, want low", lowView.Health)
	}

	_, emptyView := doWalletSummary(t, handler, empty)
	if emptyView.Health != "depleted" {
		t.Errorf("health = %q, want depleted", emptyView.Health)
	}
}
