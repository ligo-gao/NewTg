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
// 冻结/累计消费聚合与 health 口径。需要真实 PostgreSQL（主库 + 日志库）：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_test?sslmode=disable' \
//	NEWTG_TEST_LOGS_DSN='postgres://newtg:newtg@localhost:55432/newtg_logs?sslmode=disable' \
//	go test ./internal/httpapi/ -run TestWalletSummary -v

type walletTestConfig map[string]json.RawMessage

func (m walletTestConfig) Get(key string) (json.RawMessage, bool) {
	v, ok := m[key]
	return v, ok
}

// newWalletTestHandler 返回（主库、日志库、路由）。
//
// usage_logs 按 schema 契约只存在于日志库（主库表集合断言
// TestMainSchemaTableCount 会拒绝它），因此 LogDB 必须来自
// NEWTG_TEST_LOGS_DSN，且测试建表也只能落在日志库连接上。
func newWalletTestHandler(t *testing.T, cfg ConfigReader) (*sql.DB, *sql.DB, http.Handler) {
	t.Helper()
	mainDSN := os.Getenv("NEWTG_TEST_DB_DSN")
	logsDSN := os.Getenv("NEWTG_TEST_LOGS_DSN")
	if mainDSN == "" {
		t.Skip("NEWTG_TEST_DB_DSN 未设置，跳过钱包摘要集成测试")
	}
	if logsDSN == "" {
		t.Skip("NEWTG_TEST_LOGS_DSN 未设置，跳过钱包摘要集成测试")
	}
	mainDB, err := sql.Open("pgx", mainDSN)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	if err := mainDB.Ping(); err != nil {
		_ = mainDB.Close()
		t.Fatalf("ping test db: %v", err)
	}
	logsDB, err := sql.Open("pgx", logsDSN)
	if err != nil {
		_ = mainDB.Close()
		t.Fatalf("open logs db: %v", err)
	}
	if err := logsDB.Ping(); err != nil {
		_ = mainDB.Close()
		_ = logsDB.Close()
		t.Fatalf("ping logs db: %v", err)
	}
	// 先登记关闭，后登记残留清理：t.Cleanup 后进先出，保证清理时连接仍可用。
	t.Cleanup(func() {
		_ = mainDB.Close()
		_ = logsDB.Close()
	})
	dropMainUsageLogsArtifact(t, mainDB)

	// 最小列集足以让钱包聚合查询（user_id/cost/created_at）工作；
	// 日志库若已跑过 migrations-logs，这句 CREATE IF NOT EXISTS 是 no-op。
	if _, err := logsDB.Exec(`CREATE TABLE IF NOT EXISTS usage_logs (
		id bigint GENERATED ALWAYS AS IDENTITY,
		user_id bigint NOT NULL,
		cost bigint NOT NULL DEFAULT 0,
		created_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		t.Fatalf("create usage_logs in logs db: %v", err)
	}

	h, err := NewWalletHandlers(WalletHandlerConfig{
		DB:     mainDB,
		LogDB:  logsDB,
		Config: cfg,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewWalletHandlers: %v", err)
	}
	mux := http.NewServeMux()
	h.RegisterWalletRoutes(mux, nil)
	return mainDB, logsDB, mux
}

// dropMainUsageLogsArtifact 在用例结束时删除主测试库里的 usage_logs 残留表。
//
// 为什么需要：usage_logs 按契约只属于日志库；历史版本的集成测试误把
// NEWTG_TEST_DB_DSN 同时当作 LogDB 并在此建表，残留表会让主库 schema
// 断言（internal/store.TestMainSchemaTableCount）失败。该表是测试产物、
// 不是业务 schema，因此无条件清掉。
//
// 为什么要坐 advisory 事务锁：go test 会并行运行不同包（httpapi、store），
// 它们共享同一个 newtg_test 库；锁把「检查并删除」串行化，避免并发包
// 对同一残留表的删除互相干扰。锁随事务提交/回滚自动释放。
func dropMainUsageLogsArtifact(t *testing.T, mainDB *sql.DB) {
	t.Helper()
	t.Cleanup(func() {
		tx, err := mainDB.Begin()
		if err != nil {
			t.Logf("cleanup main usage_logs: begin: %v", err)
			return
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.Exec(`SELECT pg_advisory_xact_lock(hashtext('newtg_test.usage_logs')::bigint)`); err != nil {
			t.Logf("cleanup main usage_logs: advisory lock: %v", err)
			return
		}
		if _, err := tx.Exec(`DROP TABLE IF EXISTS usage_logs CASCADE`); err != nil {
			t.Logf("cleanup main usage_logs: drop: %v", err)
			return
		}
		if err := tx.Commit(); err != nil {
			t.Logf("cleanup main usage_logs: commit: %v", err)
		}
	})
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

func walletTestUsage(t *testing.T, logsDB *sql.DB, userID, cost int64) {
	t.Helper()
	// 日志库跑过 migrations-logs 时，usage_logs 是带 NOT NULL request_id /
	// model / status 的分区表；空库自举时只有最小列集。按实际列集选择插入
	// 形态，以免测试对日志库 schema 状态做硬假设。
	var migrated bool
	if err := logsDB.QueryRow(`SELECT EXISTS (
		SELECT 1 FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'usage_logs'
		  AND column_name = 'request_id')`).Scan(&migrated); err != nil {
		t.Fatalf("inspect usage_logs schema: %v", err)
	}
	var err error
	if migrated {
		_, err = logsDB.Exec(
			`INSERT INTO usage_logs (request_id, user_id, model, status, cost)
			 VALUES (gen_random_uuid(), $1, 'wallet-summary-test', 1, $2)`, userID, cost)
	} else {
		_, err = logsDB.Exec(
			`INSERT INTO usage_logs (user_id, cost) VALUES ($1, $2)`, userID, cost)
	}
	if err != nil {
		t.Fatalf("insert usage log: %v", err)
	}
	t.Cleanup(func() {
		_, _ = logsDB.Exec(`DELETE FROM usage_logs WHERE user_id = $1`, userID)
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
	_, _, handler := newWalletTestHandler(t, nil)

	rec, _ := doWalletSummary(t, handler, 0)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestWalletSummaryReadsOnlySelfAndAggregates(t *testing.T) {
	cfg := walletTestConfig{
		ConfigBalanceAlertThreshold: json.RawMessage("5000"),
	}
	db, logsDB, handler := newWalletTestHandler(t, cfg)

	alice := walletTestUser(t, db, 1500)
	bob := walletTestUser(t, db, 99999)
	walletTestLedger(t, db, alice, "consume_pre", "hold", -300)
	walletTestLedger(t, db, alice, "consume", "confirmed", -700)
	walletTestUsage(t, logsDB, alice, 700)

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
	db, _, handler := newWalletTestHandler(t, nil)

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
