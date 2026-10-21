//go:build integration

package httpapi

// W7/M3 钱包流水读取面集成测试（modules-03 F-03-02/F-03-03/F-03-04）。
//
// 运行（隔离库，先 CREATE DATABASE + migrate；绝不复用共享 newtg_test）：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_m3?sslmode=disable' \
//	NEWTG_TEST_LOGS_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_m3_logs?sslmode=disable' \
//	  go test -tags=integration -p 1 ./internal/httpapi/ -run TestWalletTransactions -v
//
// 用例 → 设计/裁决映射见 .superpowers/sdd/2026-10-21-剩余功能域开发计划/
// task-M3-report.md 首节。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/rbac"
)

// walletTxnFixedNow 是所有时间窗用例的固定时钟：默认窗口断言不依赖机器时间。
var walletTxnFixedNow = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

type walletTxnEnvOptions struct {
	// logDBOverride 为 true 时把 LogDB 置 nil（可控接缝：验证 usage 降级）；
	// 不通过关闭共享日志库制造故障。
	logDBOverride bool
	redis         *redis.Client
	wrap          func(http.Handler) http.Handler
}

type walletTxnEnv struct {
	db      *sql.DB
	logsDB  *sql.DB
	handler http.Handler
	now     time.Time
}

func newWalletTxnEnv(t *testing.T, opts walletTxnEnvOptions) *walletTxnEnv {
	t.Helper()
	mainDSN := os.Getenv("NEWTG_TEST_DB_DSN")
	logsDSN := os.Getenv("NEWTG_TEST_LOGS_DSN")
	if mainDSN == "" || logsDSN == "" {
		t.Skip("NEWTG_TEST_DB_DSN / NEWTG_TEST_LOGS_DSN 未设置，跳过钱包流水集成测试")
	}
	mainDB, err := sql.Open("pgx", mainDSN)
	if err != nil {
		t.Fatalf("open main db: %v", err)
	}
	logsDB, err := sql.Open("pgx", logsDSN)
	if err != nil {
		_ = mainDB.Close()
		t.Fatalf("open logs db: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := mainDB.PingContext(ctx); err != nil {
		_ = mainDB.Close()
		_ = logsDB.Close()
		t.Fatalf("ping main db: %v", err)
	}
	if err := logsDB.PingContext(ctx); err != nil {
		_ = mainDB.Close()
		_ = logsDB.Close()
		t.Fatalf("ping logs db: %v", err)
	}
	t.Cleanup(func() {
		_ = mainDB.Close()
		_ = logsDB.Close()
	})
	if _, err := mainDB.Exec(
		`INSERT INTO user_groups (name, ratio, user_selectable, is_system, status)
		 VALUES ('default', 100, true, true, 1) ON CONFLICT (name) DO NOTHING`); err != nil {
		t.Fatalf("ensure default group: %v", err)
	}

	var logDB *sql.DB = logsDB
	if opts.logDBOverride {
		logDB = nil
	}
	h, err := NewWalletHandlers(WalletHandlerConfig{
		DB:     mainDB,
		LogDB:  logDB,
		Config: nil,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Redis:  opts.redis,
		Now:    func() time.Time { return walletTxnFixedNow },
	})
	if err != nil {
		t.Fatalf("NewWalletHandlers: %v", err)
	}
	mux := http.NewServeMux()
	h.RegisterWalletRoutes(mux, opts.wrap)
	return &walletTxnEnv{db: mainDB, logsDB: logsDB, handler: mux, now: walletTxnFixedNow}
}

func walletTxnUser(t *testing.T, db *sql.DB, balance int64) int64 {
	t.Helper()
	name := fmt.Sprintf("wtx%d", time.Now().UnixNano())
	var id int64
	if err := db.QueryRow(
		`INSERT INTO users (username, balance, "group") VALUES ($1, $2, 'default') RETURNING id`,
		name, balance,
	).Scan(&id); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	t.Cleanup(func() {
		for _, stmt := range []string{
			`DELETE FROM wallet_transactions WHERE user_id = $1`,
			`DELETE FROM orders WHERE user_id = $1`,
			`DELETE FROM redemption_codes WHERE used_by = $1 OR created_by = $1`,
			`DELETE FROM api_tokens WHERE user_id = $1`,
			`DELETE FROM users WHERE id = $1`,
		} {
			if _, err := db.Exec(stmt, id); err != nil {
				t.Logf("cleanup %q: %v", stmt, err)
			}
		}
	})
	return id
}

// walletTxnSeed 是一行流水的测试种子；空串表示 SQL NULL。
type walletTxnSeed struct {
	Type         string
	Status       string
	Amount       int64
	BalanceAfter int64
	RefType      string
	RefID        string
	Note         string
	CreatedAt    time.Time
}

func walletTxnInsert(t *testing.T, db *sql.DB, userID int64, seed walletTxnSeed) int64 {
	t.Helper()
	if seed.Status == "" {
		seed.Status = "confirmed"
	}
	if seed.CreatedAt.IsZero() {
		seed.CreatedAt = walletTxnFixedNow.Add(-time.Hour)
	}
	var id int64
	if err := db.QueryRow(`
		INSERT INTO wallet_transactions
			(user_id, type, amount, balance_after, status, reference_type, reference_id, note, created_at)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), NULLIF($7, ''), NULLIF($8, ''), $9)
		RETURNING id`,
		userID, seed.Type, seed.Amount, seed.BalanceAfter, seed.Status,
		seed.RefType, seed.RefID, seed.Note, seed.CreatedAt,
	).Scan(&id); err != nil {
		t.Fatalf("insert wallet transaction: %v", err)
	}
	return id
}

func walletTxnRequest(
	t *testing.T, handler http.Handler, userID int64, path string, query url.Values,
) *httptest.ResponseRecorder {
	t.Helper()
	target := path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if userID > 0 {
		req = req.WithContext(auth.WithPrincipal(context.Background(), &auth.Principal{
			UserID: userID,
			Role:   "user",
		}))
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func walletTxnDecodePage(t *testing.T, rec *httptest.ResponseRecorder) walletTransactionPage {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var page walletTransactionPage
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode page: %v (body=%s)", err, rec.Body.String())
	}
	return page
}

func walletTxnDecodeDetail(t *testing.T, rec *httptest.ResponseRecorder) walletTransactionDetailView {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var detail walletTransactionDetailView
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode detail: %v (body=%s)", err, rec.Body.String())
	}
	return detail
}

func walletTxnIDs(page walletTransactionPage) []int64 {
	ids := make([]int64, 0, len(page.Items))
	for _, item := range page.Items {
		ids = append(ids, item.ID)
	}
	return ids
}

// --- 认证 -----------------------------------------------------------------

func TestWalletTransactionsRequireAuthentication(t *testing.T) {
	env := newWalletTxnEnv(t, walletTxnEnvOptions{})

	for _, path := range []string{"/api/wallet/transactions", "/api/wallet/transactions/1"} {
		rec := walletTxnRequest(t, env.handler, 0, path, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s status = %d, want 401", path, rec.Code)
		}
	}
}

// TestWalletTransactionsSessionAndBearerRead 用生产 Protect 链证明会话与
// Bearer 两种身份都能读；未认证 401。identity 只来自 Principal（R-M3-01 安全口径）。
func TestWalletTransactionsSessionAndBearerRead(t *testing.T) {
	env := newWalletTxnEnv(t, walletTxnEnvOptions{})
	userID := walletTxnUser(t, env.db, 1000)
	walletTxnInsert(t, env.db, userID, walletTxnSeed{Type: "topup", Amount: 1000, BalanceAfter: 1000})

	sessions, _ := newTestSessions(t)
	routerMux := http.NewServeMux()
	wrap := Protect(sessions, walletTxnTokenLookup{db: env.db}, "https://app.example.com")
	env.handler = wrap(routerMux)
	// 重新注册到受保护子树（RegisterWalletRoutes 会再挂一次到同一 mux）。
	h, err := NewWalletHandlers(WalletHandlerConfig{
		DB: env.db, LogDB: env.logsDB,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:    func() time.Time { return walletTxnFixedNow },
	})
	if err != nil {
		t.Fatalf("NewWalletHandlers: %v", err)
	}
	h.RegisterWalletRoutes(routerMux, nil)

	// 1) 会话 Cookie（GET 不触发 CSRF）。
	sid := mustSession(t, sessions, userID, "user")
	req := httptest.NewRequest(http.MethodGet, "/api/wallet/transactions", nil)
	req.Host = "app.example.com"
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: sid})
	rec := httptest.NewRecorder()
	env.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("session status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	// 2) Bearer 令牌。
	plain := "nkt-m3-integration-token"
	if _, err := env.db.Exec(`
		INSERT INTO api_tokens (user_id, name, key_hash, "group")
		VALUES ($1, 'm3', $2, 'default')`, userID, auth.HashTokenKey(plain)); err != nil {
		t.Fatalf("insert api token: %v", err)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/wallet/transactions", nil)
	req.Header.Set("Authorization", "Bearer "+plain)
	rec = httptest.NewRecorder()
	env.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("bearer status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	// 3) 无凭据 → 401。
	rec = serve(env.handler, httptest.NewRequest(http.MethodGet, "/api/wallet/transactions", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d, want 401", rec.Code)
	}
}

// walletTxnTokenLookup 是 Bearer 路径的最小 SQL 实现（与生产 tokenstore
// 的读取面一致）。
type walletTxnTokenLookup struct{ db *sql.DB }

func (l walletTxnTokenLookup) Resolve(ctx context.Context, keyHash string) (*auth.TokenMeta, error) {
	var (
		meta   auth.TokenMeta
		expiry sql.NullTime
		remain sql.NullInt64
	)
	err := l.db.QueryRowContext(ctx, `
		SELECT id, user_id, status, "group", expires_at, remain_quota
		FROM api_tokens WHERE key_hash = $1`, keyHash).
		Scan(&meta.ID, &meta.UserID, &meta.Status, &meta.Group, &expiry, &remain)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if expiry.Valid {
		meta.ExpiresAt = &expiry.Time
	}
	meta.UnlimitedQuota = !remain.Valid
	meta.RemainQuota = remain.Int64
	return &meta, nil
}

// --- 列表筛选 -------------------------------------------------------------

func TestWalletTransactionsListFilters(t *testing.T) {
	env := newWalletTxnEnv(t, walletTxnEnvOptions{})
	userID := walletTxnUser(t, env.db, 1_000_000)

	allTypes := []string{
		"topup", "redemption", "consume_pre", "consume", "release",
		"reversal", "admin_adjustment", "bonus", "affiliate_transfer",
		"checkin_bonus", "subscription_purchase",
	}
	for i, typ := range allTypes {
		note := ""
		if typ == "admin_adjustment" {
			note = "admin adjustment: add idem:" + strings.Repeat("a", 64)
		}
		walletTxnInsert(t, env.db, userID, walletTxnSeed{
			Type: typ, Amount: int64(-(i + 1)), BalanceAfter: 0, Note: note,
			CreatedAt: walletTxnFixedNow.Add(-time.Duration(i+1) * time.Minute),
		})
	}

	t.Run("type covers all 11 DB values", func(t *testing.T) {
		for _, typ := range allTypes {
			page := walletTxnDecodePage(t, walletTxnRequest(t, env.handler, userID,
				"/api/wallet/transactions", url.Values{"type": {typ}}))
			if page.Total != 1 || len(page.Items) != 1 {
				t.Fatalf("type=%s total=%d items=%d, want 1/1", typ, page.Total, len(page.Items))
			}
			if page.Items[0].Type != typ {
				t.Fatalf("type=%s returned %q", typ, page.Items[0].Type)
			}
		}
	})

	statusUser := walletTxnUser(t, env.db, 1_000_000)
	for i, status := range []string{"hold", "settled", "released", "expired", "confirmed"} {
		walletTxnInsert(t, env.db, statusUser, walletTxnSeed{
			Type: "consume_pre", Status: status, Amount: int64(-(i + 1)), BalanceAfter: 0,
			CreatedAt: walletTxnFixedNow.Add(-time.Duration(i+1) * time.Minute),
		})
	}
	t.Run("status covers all 5 values", func(t *testing.T) {
		for _, status := range []string{"hold", "settled", "released", "expired", "confirmed"} {
			page := walletTxnDecodePage(t, walletTxnRequest(t, env.handler, statusUser,
				"/api/wallet/transactions", url.Values{"status": {status}}))
			if page.Total != 1 || page.Items[0].Status != status {
				t.Fatalf("status=%s total=%d item.status=%q", status, page.Total,
					firstStatus(page))
			}
		}
	})

	dirUser := walletTxnUser(t, env.db, 1_000_000)
	walletTxnInsert(t, env.db, dirUser, walletTxnSeed{Type: "topup", Amount: 500, BalanceAfter: 500})
	walletTxnInsert(t, env.db, dirUser, walletTxnSeed{Type: "consume", Amount: -300, BalanceAfter: 200})
	t.Run("dir maps to amount sign", func(t *testing.T) {
		in := walletTxnDecodePage(t, walletTxnRequest(t, env.handler, dirUser,
			"/api/wallet/transactions", url.Values{"dir": {"in"}}))
		if in.Total != 1 || in.Items[0].Amount != 500 {
			t.Fatalf("dir=in total=%d items=%+v", in.Total, in.Items)
		}
		out := walletTxnDecodePage(t, walletTxnRequest(t, env.handler, dirUser,
			"/api/wallet/transactions", url.Values{"dir": {"out"}}))
		if out.Total != 1 || out.Items[0].Amount != -300 {
			t.Fatalf("dir=out total=%d items=%+v", out.Total, out.Items)
		}
	})

	t.Run("default window is 30 days, left-closed right-open", func(t *testing.T) {
		windowUser := walletTxnUser(t, env.db, 1_000_000)
		from := walletTxnFixedNow.Add(-walletDefaultWindow)
		walletTxnInsert(t, env.db, windowUser, walletTxnSeed{
			Type: "bonus", Amount: 1, BalanceAfter: 1, CreatedAt: from}) // 恰在左边界 → 命中
		walletTxnInsert(t, env.db, windowUser, walletTxnSeed{
			Type: "bonus", Amount: 2, BalanceAfter: 2, CreatedAt: from.Add(-time.Second)}) // 31 天前 → 不命中
		walletTxnInsert(t, env.db, windowUser, walletTxnSeed{
			Type: "bonus", Amount: 3, BalanceAfter: 3, CreatedAt: walletTxnFixedNow}) // to 右开 → 不命中
		walletTxnInsert(t, env.db, windowUser, walletTxnSeed{
			Type: "bonus", Amount: 4, BalanceAfter: 4, CreatedAt: walletTxnFixedNow.Add(-time.Second)})
		page := walletTxnDecodePage(t, walletTxnRequest(t, env.handler, windowUser,
			"/api/wallet/transactions", nil))
		if page.Total != 2 {
			t.Fatalf("default window total=%d, want 2 (items=%+v)", page.Total, page.Items)
		}
	})

	t.Run("explicit from/to narrow the window", func(t *testing.T) {
		windowUser := walletTxnUser(t, env.db, 1_000_000)
		walletTxnInsert(t, env.db, windowUser, walletTxnSeed{
			Type: "bonus", Amount: 1, BalanceAfter: 1, CreatedAt: walletTxnFixedNow.Add(-2 * time.Hour)})
		walletTxnInsert(t, env.db, windowUser, walletTxnSeed{
			Type: "bonus", Amount: 2, BalanceAfter: 2, CreatedAt: walletTxnFixedNow.Add(-time.Hour)})
		q := url.Values{
			"from": {walletTxnFixedNow.Add(-90 * time.Minute).Format(time.RFC3339)},
			"to":   {walletTxnFixedNow.Add(-30 * time.Minute).Format(time.RFC3339)},
		}
		page := walletTxnDecodePage(t, walletTxnRequest(t, env.handler, windowUser,
			"/api/wallet/transactions", q))
		if page.Total != 1 || page.Items[0].Amount != 2 {
			t.Fatalf("window total=%d items=%+v, want only the newer row", page.Total, page.Items)
		}
	})

	// 实现期解释：只给 to 时窗口为 [to−30d, to)（相对 to 回退，而不是
	// 字面默认 from=now−30d 造成 from > to）。
	t.Run("only to uses a 30-day window ending at to", func(t *testing.T) {
		windowUser := walletTxnUser(t, env.db, 1_000_000)
		to := walletTxnFixedNow.Add(-time.Hour)
		walletTxnInsert(t, env.db, windowUser, walletTxnSeed{
			Type: "bonus", Amount: 1, BalanceAfter: 1, CreatedAt: to.Add(-31 * 24 * time.Hour)}) // 窗外
		walletTxnInsert(t, env.db, windowUser, walletTxnSeed{
			Type: "bonus", Amount: 2, BalanceAfter: 2, CreatedAt: to.Add(-29 * 24 * time.Hour)}) // 窗内
		walletTxnInsert(t, env.db, windowUser, walletTxnSeed{
			Type: "bonus", Amount: 3, BalanceAfter: 3, CreatedAt: to.Add(-time.Second)}) // 窗内（右开前一刻）
		walletTxnInsert(t, env.db, windowUser, walletTxnSeed{
			Type: "bonus", Amount: 4, BalanceAfter: 4, CreatedAt: to}) // to 右开 → 窗外
		page := walletTxnDecodePage(t, walletTxnRequest(t, env.handler, windowUser,
			"/api/wallet/transactions",
			url.Values{"to": {to.Format(time.RFC3339)}}))
		if page.Total != 2 {
			t.Fatalf("only-to total=%d, want 2 (items=%+v)", page.Total, page.Items)
		}
	})

	// 跨度恰 90 天必须允许（上限是 ≤90 天，不是 <90）。
	t.Run("exactly 90 days is allowed", func(t *testing.T) {
		windowUser := walletTxnUser(t, env.db, 1_000_000)
		from := walletTxnFixedNow.Add(-walletMaxWindow)
		walletTxnInsert(t, env.db, windowUser, walletTxnSeed{
			Type: "bonus", Amount: 1, BalanceAfter: 1, CreatedAt: from.Add(-time.Second)}) // 窗外
		walletTxnInsert(t, env.db, windowUser, walletTxnSeed{
			Type: "bonus", Amount: 2, BalanceAfter: 2, CreatedAt: from.Add(time.Minute)})
		rec := walletTxnRequest(t, env.handler, windowUser, "/api/wallet/transactions",
			url.Values{
				"from": {from.Format(time.RFC3339)},
				"to":   {walletTxnFixedNow.Format(time.RFC3339)},
			})
		page := walletTxnDecodePage(t, rec)
		if page.Total != 1 || page.Items[0].Amount != 2 {
			t.Fatalf("90-day span total=%d items=%+v, want 1/amount 2", page.Total, page.Items)
		}
	})

	t.Run("min/max apply to ABS(amount), inclusive", func(t *testing.T) {
		amountUser := walletTxnUser(t, env.db, 1_000_000)
		for _, amount := range []int64{100, -100, 500, -500, 1000, -1000} {
			walletTxnInsert(t, env.db, amountUser, walletTxnSeed{
				Type: "consume", Amount: amount, BalanceAfter: 0})
		}
		page := walletTxnDecodePage(t, walletTxnRequest(t, env.handler, amountUser,
			"/api/wallet/transactions", url.Values{"min": {"500"}, "max": {"1000"}}))
		if page.Total != 4 {
			t.Fatalf("ABS range total=%d, want 4 (items=%+v)", page.Total, page.Items)
		}
		for _, item := range page.Items {
			abs := item.Amount
			if abs < 0 {
				abs = -abs
			}
			if abs < 500 || abs > 1000 {
				t.Fatalf("amount %d outside [500,1000]", item.Amount)
			}
		}
		// 边界相等可用（闭区间）。
		exact := walletTxnDecodePage(t, walletTxnRequest(t, env.handler, amountUser,
			"/api/wallet/transactions", url.Values{"min": {"100"}, "max": {"100"}}))
		if exact.Total != 2 {
			t.Fatalf("closed boundary total=%d, want 2", exact.Total)
		}
	})

	t.Run("ref matches non-UUID exactly", func(t *testing.T) {
		refUser := walletTxnUser(t, env.db, 1_000_000)
		walletTxnInsert(t, env.db, refUser, walletTxnSeed{
			Type: "topup", Amount: 1000, BalanceAfter: 1000, RefType: "order", RefID: "424242"})
		hit := walletTxnDecodePage(t, walletTxnRequest(t, env.handler, refUser,
			"/api/wallet/transactions", url.Values{"ref": {"424242"}}))
		if hit.Total != 1 {
			t.Fatalf("exact ref total=%d, want 1", hit.Total)
		}
		miss := walletTxnDecodePage(t, walletTxnRequest(t, env.handler, refUser,
			"/api/wallet/transactions", url.Values{"ref": {"42424"}}))
		if miss.Total != 0 {
			t.Fatalf("partial ref total=%d, want 0", miss.Total)
		}
	})

	t.Run("empty result is [] not null", func(t *testing.T) {
		emptyUser := walletTxnUser(t, env.db, 1_000_000)
		rec := walletTxnRequest(t, env.handler, emptyUser, "/api/wallet/transactions", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), `"items":[]`) {
			t.Fatalf("items must serialise as []: %s", rec.Body.String())
		}
	})
}

func firstStatus(page walletTransactionPage) string {
	if len(page.Items) == 0 {
		return ""
	}
	return page.Items[0].Status
}

// TestWalletTransactionsRefUUIDNormalization 覆盖 R-M3-07：同一 requestID 的
// 32-hex 与带连字符拼写都必须命中；普通关联号保持精确匹配。
func TestWalletTransactionsRefUUIDNormalization(t *testing.T) {
	env := newWalletTxnEnv(t, walletTxnEnvOptions{})
	userID := walletTxnUser(t, env.db, 1_000_000)
	compact := "9f3a8d2e1b2c4d5e8f90112233445566"
	hyphenated := "9f3a8d2e-1b2c-4d5e-8f90-112233445566"
	walletTxnInsert(t, env.db, userID, walletTxnSeed{
		Type: "consume_pre", Status: "hold", Amount: -12000, BalanceAfter: 0,
		RefType: "usage_log", RefID: compact})

	for _, ref := range []string{compact, hyphenated, strings.ToUpper(hyphenated)} {
		page := walletTxnDecodePage(t, walletTxnRequest(t, env.handler, userID,
			"/api/wallet/transactions", url.Values{"ref": {ref}}))
		if page.Total != 1 {
			t.Fatalf("ref=%q total=%d, want 1", ref, page.Total)
		}
	}
}

// --- 分页 -----------------------------------------------------------------

// TestWalletTransactionsPaginationTieBreaker 用多行完全相同的 created_at
// 验证 ORDER BY created_at DESC, id DESC 的稳定 tie-breaker：跨页无重复、
// 无遗漏；p 越界夹到最后一页（变异 B 必须让本用例红）。
func TestWalletTransactionsPaginationTieBreaker(t *testing.T) {
	env := newWalletTxnEnv(t, walletTxnEnvOptions{})
	userID := walletTxnUser(t, env.db, 1_000_000)
	sameInstant := walletTxnFixedNow.Add(-time.Hour)
	inserted := make([]int64, 0, 5)
	for i := 0; i < 5; i++ {
		inserted = append(inserted, walletTxnInsert(t, env.db, userID, walletTxnSeed{
			Type: "bonus", Amount: int64(i + 1), BalanceAfter: int64(i + 1), CreatedAt: sameInstant,
		}))
	}
	// 期望顺序：id 倒序。
	want := []int64{inserted[4], inserted[3], inserted[2], inserted[1], inserted[0]}

	var got []int64
	seen := map[int64]bool{}
	for pageNo := 1; pageNo <= 3; pageNo++ {
		page := walletTxnDecodePage(t, walletTxnRequest(t, env.handler, userID,
			"/api/wallet/transactions", url.Values{
				"p": {fmt.Sprintf("%d", pageNo)}, "page_size": {"2"},
			}))
		if page.Page != pageNo {
			t.Fatalf("page=%d, want %d", page.Page, pageNo)
		}
		for _, id := range walletTxnIDs(page) {
			if seen[id] {
				t.Fatalf("id %d repeated across pages", id)
			}
			seen[id] = true
			got = append(got, id)
		}
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("page order = %v, want %v", got, want)
	}

	// p 越界 → 最后一页，且返回同一批行。
	last := walletTxnDecodePage(t, walletTxnRequest(t, env.handler, userID,
		"/api/wallet/transactions", url.Values{"p": {"99"}, "page_size": {"2"}}))
	if last.Page != 3 {
		t.Fatalf("out-of-range page = %d, want 3", last.Page)
	}
	if fmt.Sprint(walletTxnIDs(last)) != fmt.Sprint(want[4:]) {
		t.Fatalf("clamped last page ids = %v, want %v", walletTxnIDs(last), want[4:])
	}
}

// --- IDOR 与 404 ----------------------------------------------------------

func TestWalletTransactionsDetailIDOR(t *testing.T) {
	env := newWalletTxnEnv(t, walletTxnEnvOptions{})
	alice := walletTxnUser(t, env.db, 1_000_000)
	bob := walletTxnUser(t, env.db, 1_000_000)
	aliceRow := walletTxnInsert(t, env.db, alice, walletTxnSeed{Type: "bonus", Amount: 1, BalanceAfter: 1})
	bobRow := walletTxnInsert(t, env.db, bob, walletTxnSeed{Type: "bonus", Amount: 2, BalanceAfter: 2})

	own := walletTxnRequest(t, env.handler, alice,
		fmt.Sprintf("/api/wallet/transactions/%d", aliceRow), nil)
	if own.Code != http.StatusOK {
		t.Fatalf("own detail status = %d, want 200", own.Code)
	}
	other := walletTxnRequest(t, env.handler, alice,
		fmt.Sprintf("/api/wallet/transactions/%d", bobRow), nil)
	if other.Code != http.StatusNotFound {
		t.Fatalf("other's detail status = %d, want 404 (IDOR)", other.Code)
	}
	for _, raw := range []string{"abc", "0", "-1", "999999999"} {
		rec := walletTxnRequest(t, env.handler, alice, "/api/wallet/transactions/"+raw, nil)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("id=%q status = %d, want 404", raw, rec.Code)
		}
	}

	// 接口形状没有 user_id：传了也忽略，仍只返回自己的行。
	page := walletTxnDecodePage(t, walletTxnRequest(t, env.handler, alice,
		"/api/wallet/transactions", url.Values{"user_id": {fmt.Sprintf("%d", bob)}}))
	if page.Total != 1 || page.Items[0].ID != aliceRow {
		t.Fatalf("user_id param leaked another user's rows: %+v", page.Items)
	}
}

// --- 详情上下文 -----------------------------------------------------------

func TestWalletTransactionsDetailUsageContextAndSegments(t *testing.T) {
	env := newWalletTxnEnv(t, walletTxnEnvOptions{})
	userID := walletTxnUser(t, env.db, 1_000_000)

	insertUsage := func(requestID string, user int64, cost, pre, refunded int64) {
		t.Helper()
		if _, err := env.logsDB.Exec(`
			INSERT INTO usage_logs (request_id, user_id, model, status,
				prompt_tokens, completion_tokens, pre_deducted, cost, refunded)
			VALUES ($1::uuid, $2, 'claude-sonnet-4.5', 1, 1204, 388, $3, $4, $5)`,
			requestID, user, pre, cost, refunded); err != nil {
			t.Fatalf("insert usage log: %v", err)
		}
		t.Cleanup(func() {
			_, _ = env.logsDB.Exec(`DELETE FROM usage_logs WHERE request_id = $1::uuid`, requestID)
		})
	}

	t.Run("held/charged/refunded partial refund", func(t *testing.T) {
		requestID := "11111111111111111111111111111111"
		insertUsage(requestID, userID, 9240, 12000, 2760)
		preID := walletTxnInsert(t, env.db, userID, walletTxnSeed{
			Type: "consume_pre", Status: "settled", Amount: -12000, BalanceAfter: 988000,
			RefType: "usage_log", RefID: requestID, Note: "billing:settle",
			CreatedAt: walletTxnFixedNow.Add(-2 * time.Hour)})
		walletTxnInsert(t, env.db, userID, walletTxnSeed{
			Type: "release", Amount: 2760, BalanceAfter: 990760,
			RefType: "usage_log", RefID: requestID, Note: "billing:settle",
			CreatedAt: walletTxnFixedNow.Add(-2*time.Hour + time.Second)})

		detail := walletTxnDecodeDetail(t, walletTxnRequest(t, env.handler, userID,
			fmt.Sprintf("/api/wallet/transactions/%d", preID), nil))
		if detail.Usage == nil {
			t.Fatalf("usage context missing")
		}
		if detail.Usage.Model != "claude-sonnet-4.5" ||
			detail.Usage.PromptTokens != 1204 || detail.Usage.CompletionTokens != 388 ||
			detail.Usage.Cost != 9240 {
			t.Fatalf("usage = %+v", detail.Usage)
		}
		if detail.Usage.PreDeducted == nil || *detail.Usage.PreDeducted != 12000 {
			t.Fatalf("usage.pre_deducted = %v, want 12000", detail.Usage.PreDeducted)
		}
		if detail.Usage.Refunded == nil || *detail.Usage.Refunded != 2760 {
			t.Fatalf("usage.refunded = %v, want 2760", detail.Usage.Refunded)
		}
		if detail.Segments == nil {
			t.Fatalf("segments missing")
		}
		if *detail.Segments != (walletTransactionSegmentsView{Held: 12000, Charged: 9240, Refunded: 2760}) {
			t.Fatalf("segments = %+v, want 12000/9240/2760", *detail.Segments)
		}
		// billing:* 内部标记不得出现在用户侧 note。
		if detail.Note != nil {
			t.Fatalf("note leaked for consume_pre: %q", *detail.Note)
		}
	})

	t.Run("failed request full refund", func(t *testing.T) {
		requestID := "22222222222222222222222222222222"
		insertUsage(requestID, userID, 0, 12000, 12000)
		preID := walletTxnInsert(t, env.db, userID, walletTxnSeed{
			Type: "consume_pre", Status: "released", Amount: -12000, BalanceAfter: 988000,
			RefType: "usage_log", RefID: requestID, Note: "billing:refund"})
		walletTxnInsert(t, env.db, userID, walletTxnSeed{
			Type: "release", Amount: 12000, BalanceAfter: 1_000_000,
			RefType: "usage_log", RefID: requestID, Note: "billing:refund"})
		detail := walletTxnDecodeDetail(t, walletTxnRequest(t, env.handler, userID,
			fmt.Sprintf("/api/wallet/transactions/%d", preID), nil))
		if *detail.Segments != (walletTransactionSegmentsView{Held: 12000, Charged: 0, Refunded: 12000}) {
			t.Fatalf("segments = %+v, want 12000/0/12000", *detail.Segments)
		}
	})

	t.Run("under-estimate top-up", func(t *testing.T) {
		requestID := "33333333333333333333333333333333"
		insertUsage(requestID, userID, 12500, 12000, 0)
		preID := walletTxnInsert(t, env.db, userID, walletTxnSeed{
			Type: "consume_pre", Status: "settled", Amount: -12000, BalanceAfter: 988000,
			RefType: "usage_log", RefID: requestID})
		walletTxnInsert(t, env.db, userID, walletTxnSeed{
			Type: "consume", Amount: -500, BalanceAfter: 987500,
			RefType: "usage_log", RefID: requestID, Note: "billing:settle"})
		detail := walletTxnDecodeDetail(t, walletTxnRequest(t, env.handler, userID,
			fmt.Sprintf("/api/wallet/transactions/%d", preID), nil))
		if *detail.Segments != (walletTransactionSegmentsView{Held: 12000, Charged: 12500, Refunded: 0}) {
			t.Fatalf("segments = %+v, want 12000/12500/0", *detail.Segments)
		}
	})

	t.Run("no hold cross-group charge", func(t *testing.T) {
		requestID := "44444444444444444444444444444444"
		insertUsage(requestID, userID, 1000, 0, 0)
		consumeID := walletTxnInsert(t, env.db, userID, walletTxnSeed{
			Type: "consume", Amount: -1000, BalanceAfter: 999000,
			RefType: "usage_log", RefID: requestID})
		detail := walletTxnDecodeDetail(t, walletTxnRequest(t, env.handler, userID,
			fmt.Sprintf("/api/wallet/transactions/%d", consumeID), nil))
		if *detail.Segments != (walletTransactionSegmentsView{Held: 0, Charged: 1000, Refunded: 0}) {
			t.Fatalf("segments = %+v, want 0/1000/0", *detail.Segments)
		}
	})

	t.Run("hyphenated reference_id still finds the log", func(t *testing.T) {
		requestID := "55555555-5555-4555-8555-555555555555"
		insertUsage(requestID, userID, 100, 100, 0)
		rowID := walletTxnInsert(t, env.db, userID, walletTxnSeed{
			Type: "consume_pre", Status: "hold", Amount: -100, BalanceAfter: 999900,
			RefType: "usage_log", RefID: requestID})
		detail := walletTxnDecodeDetail(t, walletTxnRequest(t, env.handler, userID,
			fmt.Sprintf("/api/wallet/transactions/%d", rowID), nil))
		if detail.Usage == nil || detail.Usage.RequestID != requestID {
			t.Fatalf("usage for hyphenated request id = %+v", detail.Usage)
		}
	})

	t.Run("log row owned by another user is invisible", func(t *testing.T) {
		other := walletTxnUser(t, env.db, 1000)
		requestID := "66666666666666666666666666666666"
		insertUsage(requestID, other, 100, 100, 0)
		rowID := walletTxnInsert(t, env.db, userID, walletTxnSeed{
			Type: "consume_pre", Status: "hold", Amount: -100, BalanceAfter: 999900,
			RefType: "usage_log", RefID: requestID})
		detail := walletTxnDecodeDetail(t, walletTxnRequest(t, env.handler, userID,
			fmt.Sprintf("/api/wallet/transactions/%d", rowID), nil))
		if detail.Usage != nil {
			t.Fatalf("usage from another user leaked: %+v", detail.Usage)
		}
	})
}

func TestWalletTransactionsDetailOrderRedemptionAdjustmentReversal(t *testing.T) {
	env := newWalletTxnEnv(t, walletTxnEnvOptions{})
	userID := walletTxnUser(t, env.db, 1_000_000)

	insertOrder := func(bizKind string) int64 {
		t.Helper()
		var id int64
		key := fmt.Sprintf("m3-order-%d", time.Now().UnixNano())
		if err := env.db.QueryRow(`
			INSERT INTO orders (user_id, provider, amount, currency, status,
				idempotency_key, biz_kind, biz_ref_id, credited_credits)
			VALUES ($1, 'stripe', 5000, 'CNY', 'succeeded', $2, $3, 1, 5000)
			RETURNING id`, userID, key, bizKind).Scan(&id); err != nil {
			t.Fatalf("insert order: %v", err)
		}
		return id
	}

	t.Run("order context", func(t *testing.T) {
		orderID := insertOrder("topup")
		rowID := walletTxnInsert(t, env.db, userID, walletTxnSeed{
			Type: "topup", Amount: 5000, BalanceAfter: 1_005_000,
			RefType: "order", RefID: fmt.Sprintf("%d", orderID)})
		detail := walletTxnDecodeDetail(t, walletTxnRequest(t, env.handler, userID,
			fmt.Sprintf("/api/wallet/transactions/%d", rowID), nil))
		if detail.Order == nil {
			t.Fatalf("order context missing")
		}
		if detail.Order.ID != orderID || detail.Order.Status != "succeeded" ||
			detail.Order.Amount != 5000 || detail.Order.Currency != "CNY" ||
			detail.Order.Provider != "stripe" || detail.Order.BizKind != "topup" {
			t.Fatalf("order = %+v", detail.Order)
		}
	})

	t.Run("redemption context never exposes code_hash", func(t *testing.T) {
		codeHash := fmt.Sprintf("m3hash-%d", time.Now().UnixNano())
		var codeID int64
		if err := env.db.QueryRow(`
			INSERT INTO redemption_codes (code_hash, credits, status, batch_name, created_by, used_by, used_at)
			VALUES ($1, 1000, 'used', 'campaign-a', $2, $2, now()) RETURNING id`,
			codeHash, userID).Scan(&codeID); err != nil {
			t.Fatalf("insert redemption code: %v", err)
		}
		rowID := walletTxnInsert(t, env.db, userID, walletTxnSeed{
			Type: "redemption", Amount: 1000, BalanceAfter: 1_001_000,
			RefType: "redemption", RefID: fmt.Sprintf("%d", codeID)})
		rec := walletTxnRequest(t, env.handler, userID,
			fmt.Sprintf("/api/wallet/transactions/%d", rowID), nil)
		detail := walletTxnDecodeDetail(t, rec)
		if detail.Redemption == nil {
			t.Fatalf("redemption context missing")
		}
		if detail.Redemption.ID != codeID || detail.Redemption.Credits != 1000 ||
			detail.Redemption.BatchName == nil || *detail.Redemption.BatchName != "campaign-a" {
			t.Fatalf("redemption = %+v", detail.Redemption)
		}
		if strings.Contains(rec.Body.String(), codeHash) {
			t.Fatalf("code_hash leaked: %s", rec.Body.String())
		}
	})

	t.Run("admin_adjustment strips the idempotency fingerprint", func(t *testing.T) {
		fingerprint := strings.Repeat("c", 64)
		rowID := walletTxnInsert(t, env.db, userID, walletTxnSeed{
			Type: "admin_adjustment", Amount: 1234, BalanceAfter: 1_001_234,
			RefType: "admin_op", RefID: "idem-key",
			Note: "admin adjustment: subtract idem:" + fingerprint})
		rec := walletTxnRequest(t, env.handler, userID,
			fmt.Sprintf("/api/wallet/transactions/%d", rowID), nil)
		detail := walletTxnDecodeDetail(t, rec)
		if detail.Note != nil {
			t.Fatalf("machine note leaked: %q", *detail.Note)
		}
		if detail.Adjustment == nil || detail.Adjustment.Mode != "subtract" {
			t.Fatalf("adjustment = %+v, want mode subtract", detail.Adjustment)
		}
		if strings.Contains(rec.Body.String(), "idem:") ||
			strings.Contains(rec.Body.String(), fingerprint) {
			t.Fatalf("fingerprint leaked: %s", rec.Body.String())
		}
	})

	// 非机器格式的 note（未来 module 07 会写入人类理由）必须原样返回；
	// 解析不出 mode 时 adjustment 为 null，不能伪造成 add/subtract。
	t.Run("admin human note is returned as-is", func(t *testing.T) {
		note := "goodwill credit for the outage on 2026-10-18"
		rowID := walletTxnInsert(t, env.db, userID, walletTxnSeed{
			Type: "admin_adjustment", Amount: 500, BalanceAfter: 1_000_500,
			RefType: "admin_op", RefID: "human-key", Note: note})
		rec := walletTxnRequest(t, env.handler, userID,
			fmt.Sprintf("/api/wallet/transactions/%d", rowID), nil)
		detail := walletTxnDecodeDetail(t, rec)
		if detail.Note == nil || *detail.Note != note {
			t.Fatalf("note = %v, want %q", detail.Note, note)
		}
		if detail.Adjustment != nil {
			t.Fatalf("adjustment = %+v, want null for an unparseable human note", detail.Adjustment)
		}
		if strings.Contains(rec.Body.String(), "idem:") {
			t.Fatalf("fingerprint marker leaked: %s", rec.Body.String())
		}
	})

	t.Run("reversal corrects the same order's wallet purchase", func(t *testing.T) {
		orderID := insertOrder("subscribe")
		purchaseID := walletTxnInsert(t, env.db, userID, walletTxnSeed{
			Type: "subscription_purchase", Amount: -2000, BalanceAfter: 998000,
			RefType: "order", RefID: fmt.Sprintf("%d", orderID)})
		reversalID := walletTxnInsert(t, env.db, userID, walletTxnSeed{
			Type: "reversal", Amount: 2000, BalanceAfter: 1_000_000,
			RefType: "order", RefID: fmt.Sprintf("%d", orderID),
			Note: "subscription refund: user request"})
		detail := walletTxnDecodeDetail(t, walletTxnRequest(t, env.handler, userID,
			fmt.Sprintf("/api/wallet/transactions/%d", reversalID), nil))
		if detail.CorrectsTransactionID == nil || *detail.CorrectsTransactionID != purchaseID {
			t.Fatalf("corrects_transaction_id = %v, want %d", detail.CorrectsTransactionID, purchaseID)
		}
		// reversal 的 note 是运营原因（非 admin 机器格式），不展示也不泄露。
		if detail.Note != nil {
			t.Fatalf("reversal note leaked: %q", *detail.Note)
		}
	})

	t.Run("reversal without a wallet purchase has no link", func(t *testing.T) {
		orderID := insertOrder("subscribe")
		reversalID := walletTxnInsert(t, env.db, userID, walletTxnSeed{
			Type: "reversal", Amount: 2000, BalanceAfter: 1_000_000,
			RefType: "order", RefID: fmt.Sprintf("%d", orderID)})
		detail := walletTxnDecodeDetail(t, walletTxnRequest(t, env.handler, userID,
			fmt.Sprintf("/api/wallet/transactions/%d", reversalID), nil))
		if detail.CorrectsTransactionID != nil {
			t.Fatalf("corrects_transaction_id = %d, want null", *detail.CorrectsTransactionID)
		}
	})
}

// TestWalletTransactionsDetailDegradesWhenLogDBUnavailable 用可控接缝
// （LogDB=nil）验证日志库不可用时资金行仍 200、usage=null；不关闭共享库。
func TestWalletTransactionsDetailDegradesWhenLogDBUnavailable(t *testing.T) {
	env := newWalletTxnEnv(t, walletTxnEnvOptions{logDBOverride: true})
	userID := walletTxnUser(t, env.db, 1_000_000)
	requestID := "77777777777777777777777777777777"
	rowID := walletTxnInsert(t, env.db, userID, walletTxnSeed{
		Type: "consume_pre", Status: "hold", Amount: -500, BalanceAfter: 999500,
		RefType: "usage_log", RefID: requestID})
	detail := walletTxnDecodeDetail(t, walletTxnRequest(t, env.handler, userID,
		fmt.Sprintf("/api/wallet/transactions/%d", rowID), nil))
	if detail.Usage != nil {
		t.Fatalf("usage = %+v, want null when log db is unavailable", detail.Usage)
	}
	// 主库里的三段金额仍应可用。
	if detail.Segments == nil || detail.Segments.Held != 500 {
		t.Fatalf("segments = %+v, want held=500 from the main db", detail.Segments)
	}
}

// --- 422 ------------------------------------------------------------------

func TestWalletTransactionsValidation(t *testing.T) {
	env := newWalletTxnEnv(t, walletTxnEnvOptions{})
	userID := walletTxnUser(t, env.db, 1000)
	now := walletTxnFixedNow.Format(time.RFC3339)

	cases := []struct {
		name  string
		query url.Values
	}{
		{"unknown type", url.Values{"type": {"bogus"}}},
		{"unknown status", url.Values{"status": {"bogus"}}},
		{"unknown dir", url.Values{"dir": {"sideways"}}},
		{"p zero", url.Values{"p": {"0"}}},
		{"p not a number", url.Values{"p": {"abc"}}},
		{"page_size zero", url.Values{"page_size": {"0"}}},
		{"page_size 101", url.Values{"page_size": {"101"}}},
		{"from after to", url.Values{"from": {now}, "to": {walletTxnFixedNow.Add(-time.Hour).Format(time.RFC3339)}}},
		{"span 91 days", url.Values{
			"from": {walletTxnFixedNow.Add(-91 * 24 * time.Hour).Format(time.RFC3339)},
			"to":   {now},
		}},
		{"min greater than max", url.Values{"min": {"500"}, "max": {"100"}}},
		{"negative min", url.Values{"min": {"-1"}}},
		{"non-integer max", url.Values{"max": {"1.5"}}},
		{"bad timestamp", url.Values{"from": {"2026-10-01"}}},
		{"ref too long", url.Values{"ref": {strings.Repeat("x", 65)}}},
		// Postgres 文本协议拒绝 NUL（22021）；非法 UTF-8/控制字符同样不可
		// 能是合法关联号。R-M3-02：一律 422，不能漏成 500。
		{"ref with NUL", url.Values{"ref": {"a\x00b"}}},
		{"ref with invalid utf8", url.Values{"ref": {string([]byte{0xff, 0xfe})}}},
		{"ref with control character", url.Values{"ref": {"a\x01b"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := walletTxnRequest(t, env.handler, userID, "/api/wallet/transactions", tc.query)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422 (body=%s)", rec.Code, rec.Body.String())
			}
		})
	}
}

// --- 429 ------------------------------------------------------------------

func TestWalletTransactionsRateLimit(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	env := newWalletTxnEnv(t, walletTxnEnvOptions{redis: rdb})
	userID := walletTxnUser(t, env.db, 1000)
	otherID := walletTxnUser(t, env.db, 1000)

	for i := 0; i < WalletReadRateLimit; i++ {
		rec := walletTxnRequest(t, env.handler, userID, "/api/wallet/transactions", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d status = %d, want 200", i+1, rec.Code)
		}
	}
	blocked := walletTxnRequest(t, env.handler, userID, "/api/wallet/transactions", nil)
	if blocked.Code != http.StatusTooManyRequests {
		t.Fatalf("61st status = %d, want 429", blocked.Code)
	}
	if blocked.Header().Get("Retry-After") == "" {
		t.Fatal("429 must carry Retry-After")
	}
	// 列表与详情共用同一用户桶。
	detail := walletTxnRequest(t, env.handler, userID, "/api/wallet/transactions/1", nil)
	if detail.Code != http.StatusTooManyRequests {
		t.Fatalf("detail after limit status = %d, want 429 (shared bucket)", detail.Code)
	}
	// 另一个用户不受影响。
	if rec := walletTxnRequest(t, env.handler, otherID, "/api/wallet/transactions", nil); rec.Code != http.StatusOK {
		t.Fatalf("other user status = %d, want 200", rec.Code)
	}
}

// TestWalletTransactionsRateLimitFailsOpen：Redis 不可用时只读面放行
// （R-M3-11 / security-07 §2.3 的 fail-open 口径）。
func TestWalletTransactionsRateLimitFailsOpen(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{
		Addr:         mr.Addr(),
		MaxRetries:   -1,
		DialTimeout:  50 * time.Millisecond,
		ReadTimeout:  50 * time.Millisecond,
		WriteTimeout: 50 * time.Millisecond,
	})
	t.Cleanup(func() { _ = rdb.Close() })
	mr.Close() // 连接拒绝，模拟 Redis 故障
	env := newWalletTxnEnv(t, walletTxnEnvOptions{redis: rdb})
	userID := walletTxnUser(t, env.db, 1000)
	rec := walletTxnRequest(t, env.handler, userID, "/api/wallet/transactions", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 when redis is down (fail-open)", rec.Code)
	}
}

// TestWalletTransactionsAssemblyRoutes 端到端验证生产装配（BuildGatewayRouter
// 的 assembly.go 分支）确实把两个流水端点挂进受保护链：会话 Cookie → 认证链
// → handler → 真实 PG。只测路由注册与身份传递，不复测查询细节。
func TestWalletTransactionsAssemblyRoutes(t *testing.T) {
	env := newWalletTxnEnv(t, walletTxnEnvOptions{})
	userID := walletTxnUser(t, env.db, 1_000_000)
	rowID := walletTxnInsert(t, env.db, userID, walletTxnSeed{
		Type: "bonus", Amount: 7, BalanceAfter: 1_000_007})

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	sessions := auth.NewSessionManager(rdb, []byte(testSessionSecret))

	walletHandlers, err := NewWalletHandlers(WalletHandlerConfig{
		DB:     env.db,
		LogDB:  env.logsDB,
		Logger: testLogger(),
		Now:    func() time.Time { return walletTxnFixedNow },
	})
	if err != nil {
		t.Fatalf("NewWalletHandlers: %v", err)
	}
	rt, err := BuildGatewayRouter(GatewayDeps{
		DB:                      env.db,
		LogDB:                   env.logsDB,
		Redis:                   rdb,
		Sessions:                sessions,
		Config:                  mapConfig{},
		Audit:                   audit.New(env.logsDB),
		Authz:                   rbac.NewAuthzMiddleware(),
		TokenService:            &fakeTokenService{},
		SessionSecret:           []byte(testSessionSecret),
		CredentialEncryptionKey: "m3-assembly-integration-key",
		PublicBaseURL:           "http://localhost:3000",
		Logger:                  testLogger(),
		WalletHandlers:          walletHandlers,
	})
	if err != nil {
		t.Fatalf("BuildGatewayRouter: %v", err)
	}

	sid := mustSession(t, sessions, userID, "user")
	req := httptest.NewRequest(http.MethodGet, "/api/wallet/transactions", nil)
	req.Host = "app.example.com"
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: sid})
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	page := walletTxnDecodePage(t, rec)
	if page.Total != 1 || page.Items[0].ID != rowID {
		t.Fatalf("list via assembly = %+v, want row %d", page, rowID)
	}

	detailReq := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/api/wallet/transactions/%d", rowID), nil)
	detailReq.Host = "app.example.com"
	detailReq.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: sid})
	detailRec := httptest.NewRecorder()
	rt.ServeHTTP(detailRec, detailReq)
	if detailRec.Code != http.StatusOK {
		t.Fatalf("detail status = %d, want 200 (body=%s)", detailRec.Code, detailRec.Body.String())
	}
	detail := walletTxnDecodeDetail(t, detailRec)
	if detail.ID != rowID {
		t.Fatalf("detail id = %d, want %d", detail.ID, rowID)
	}

	// 未认证请求走同一装配链 → 401（证明端点没有绕过 SessionAuth）。
	anon := httptest.NewRecorder()
	rt.ServeHTTP(anon, httptest.NewRequest(http.MethodGet, "/api/wallet/transactions", nil))
	if anon.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d, want 401", anon.Code)
	}
}
