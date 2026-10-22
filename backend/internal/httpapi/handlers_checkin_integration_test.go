//go:build integration

package httpapi

// W15-C 签到端点的集成验收：真实 PostgreSQL（主库）+ 真实 Redis。
//
// 运行：
//
//	NEWTG_TEST_DB_DSN=postgres://newtg:newtg@localhost:55432/newtg_dom_w15c?sslmode=disable \
//	NEWTG_TEST_LOGS_DSN=postgres://newtg:newtg@localhost:55432/newtg_dom_w15c_logs?sslmode=disable \
//	NEWTG_TEST_REDIS_ADDR=localhost:56379 \
//	  go test -tags=integration -p 1 ./internal/httpapi -run TestCheckinIntegration -v
//
// 覆盖：并发两请求只一行 checkins / 一条流水 / 余额只加一次；同日第三次 409；
// 站点时区跨日可再签；钱包上界 409 且签到行随事务回滚（不部分入账）。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/billing"
	"newtg/gateway/internal/contract"
)

type checkinIntegrationClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *checkinIntegrationClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *checkinIntegrationClock) Set(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}

// openCheckinIntegration 打开隔离测试库与真实 Redis；缺环境变量时跳过。
func openCheckinIntegration(t *testing.T) (*gorm.DB, *sql.DB, *redis.Client) {
	t.Helper()
	dsn := os.Getenv("NEWTG_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("NEWTG_TEST_DB_DSN 未设置，跳过签到集成测试")
	}
	addr := os.Getenv("NEWTG_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("NEWTG_TEST_REDIS_ADDR 未设置，跳过签到集成测试")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping test db: %v", err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr, MaxRetries: -1})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Fatalf("ping redis: %v", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })

	gormDB, err := gorm.Open(postgres.New(postgres.Config{Conn: db}), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		t.Fatalf("open gorm: %v", err)
	}
	return gormDB, db, rdb
}

// createCheckinIntegrationUser 建一个余额可控的用户；签到/流水随用户清理。
func createCheckinIntegrationUser(t *testing.T, db *sql.DB, balance int64) int64 {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO user_groups (name, ratio, user_selectable, is_system, status)
		 VALUES ('default', 100, true, true, 1) ON CONFLICT (name) DO NOTHING`); err != nil {
		t.Fatalf("ensure default group: %v", err)
	}
	username := fmt.Sprintf("w15c%d", time.Now().UnixNano())
	var id int64
	if err := db.QueryRow(
		`INSERT INTO users (username, password_hash, balance, "group")
		 VALUES ($1, 'x', $2, 'default') RETURNING id`, username, balance).Scan(&id); err != nil {
		t.Fatalf("create user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM checkins WHERE user_id = $1`, id)
		_, _ = db.Exec(`DELETE FROM wallet_transactions WHERE user_id = $1`, id)
		_, _ = db.Exec(`DELETE FROM users WHERE id = $1`, id)
	})
	return id
}

func newCheckinIntegrationHarness(t *testing.T, gormDB *gorm.DB, rdb *redis.Client,
	userID int64, cfg mapConfig, clock *checkinIntegrationClock, loc *time.Location) *http.ServeMux {

	t.Helper()
	svc, err := billing.NewCheckinService(billing.CheckinServiceConfig{
		DB:       gormDB,
		Config:   cfg,
		Logger:   testLogger(),
		Now:      clock.Now,
		Location: func() *time.Location { return loc },
	})
	if err != nil {
		t.Fatalf("NewCheckinService: %v", err)
	}
	h, err := NewCheckinHandlers(CheckinHandlerConfig{
		Service: svc,
		Redis:   rdb,
		Logger:  testLogger(),
	})
	if err != nil {
		t.Fatalf("NewCheckinHandlers: %v", err)
	}
	mux := http.NewServeMux()
	h.RegisterCheckinRoutes(mux, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := auth.WithPrincipal(r.Context(), &auth.Principal{UserID: userID, Role: "user"})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	})
	return mux
}

func checkinIntegrationPost(t *testing.T, mux http.Handler) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/checkin", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	return rr
}

func checkinIntegrationCounts(t *testing.T, db *sql.DB, userID int64) (checkins, ledger int64, balance int64) {
	t.Helper()
	if err := db.QueryRow(`SELECT count(*) FROM checkins WHERE user_id = $1`, userID).Scan(&checkins); err != nil {
		t.Fatalf("count checkins: %v", err)
	}
	if err := db.QueryRow(
		`SELECT count(*) FROM wallet_transactions WHERE user_id = $1 AND type = 'checkin_bonus'`,
		userID).Scan(&ledger); err != nil {
		t.Fatalf("count ledger: %v", err)
	}
	if err := db.QueryRow(`SELECT balance FROM users WHERE id = $1`, userID).Scan(&balance); err != nil {
		t.Fatalf("read balance: %v", err)
	}
	return checkins, ledger, balance
}

// checkinTestEnvConfig 是集成测试用的签到三键配置。
func checkinTestEnvConfig(enabled bool, minQuota, maxQuota int64) mapConfig {
	return mapConfig{
		"checkin.enabled":   json.RawMessage(fmt.Sprintf("%t", enabled)),
		"checkin.min_quota": json.RawMessage(fmt.Sprintf("%d", minQuota)),
		"checkin.max_quota": json.RawMessage(fmt.Sprintf("%d", maxQuota)),
	}
}

func TestCheckinIntegrationConcurrentCreditsOnce(t *testing.T) {
	gormDB, db, rdb := openCheckinIntegration(t)
	clock := &checkinIntegrationClock{now: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)}
	userID := createCheckinIntegrationUser(t, db, 0)
	mux := newCheckinIntegrationHarness(t, gormDB, rdb, userID,
		checkinTestEnvConfig(true, 10, 10), clock, time.UTC)

	// 并发两请求：唯一约束使恰好一次成功，另一请求得到同日 409。
	var (
		wg                sync.WaitGroup
		mu                sync.Mutex
		okCount, conflict int
	)
	start := make(chan struct{})
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			rr := checkinIntegrationPost(t, mux)
			mu.Lock()
			defer mu.Unlock()
			switch rr.Code {
			case http.StatusOK:
				okCount++
			case http.StatusConflict:
				conflict++
			default:
				t.Errorf("status=%d body=%q", rr.Code, rr.Body.String())
			}
		}()
	}
	close(start)
	wg.Wait()
	if okCount != 1 || conflict != 1 {
		t.Fatalf("ok=%d conflict=%d, want 1/1", okCount, conflict)
	}
	checkins, ledger, balance := checkinIntegrationCounts(t, db, userID)
	if checkins != 1 || ledger != 1 || balance != 10 {
		t.Fatalf("checkins=%d ledger=%d balance=%d, want 1/1/10", checkins, ledger, balance)
	}

	// 同日第三次同样 409，流水与余额不增。
	rr := checkinIntegrationPost(t, mux)
	if rr.Code != http.StatusConflict {
		t.Fatalf("同日重放 = %d, want 409 (body=%q)", rr.Code, rr.Body.String())
	}
	if got := decodeCheckinProblemType(t, rr); got != problemTypeCheckinAlreadyDone {
		t.Fatalf("type=%s, want %s", got, problemTypeCheckinAlreadyDone)
	}
	checkins, ledger, balance = checkinIntegrationCounts(t, db, userID)
	if checkins != 1 || ledger != 1 || balance != 10 {
		t.Fatalf("重放后 checkins=%d ledger=%d balance=%d", checkins, ledger, balance)
	}
}

func TestCheckinIntegrationWalletLimitRollsBack(t *testing.T) {
	gormDB, db, rdb := openCheckinIntegration(t)
	clock := &checkinIntegrationClock{now: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)}
	userID := createCheckinIntegrationUser(t, db, contract.MaxWalletQuota)
	mux := newCheckinIntegrationHarness(t, gormDB, rdb, userID,
		checkinTestEnvConfig(true, 10, 10), clock, time.UTC)

	rr := checkinIntegrationPost(t, mux)
	if rr.Code != http.StatusConflict {
		t.Fatalf("钱包上界 = %d, want 409 (body=%q)", rr.Code, rr.Body.String())
	}
	if got := decodeCheckinProblemType(t, rr); got != problemTypeWalletLimitExceeded {
		t.Fatalf("type=%s, want %s", got, problemTypeWalletLimitExceeded)
	}
	// 账本失败 → 整笔事务回滚：签到行也不能落库（不部分入账）。
	checkins, ledger, balance := checkinIntegrationCounts(t, db, userID)
	if checkins != 0 || ledger != 0 || balance != contract.MaxWalletQuota {
		t.Fatalf("回滚不完整: checkins=%d ledger=%d balance=%d", checkins, ledger, balance)
	}
}

func TestCheckinIntegrationCrossSiteTimezoneDay(t *testing.T) {
	gormDB, db, rdb := openCheckinIntegration(t)
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Skip("Asia/Shanghai 不可用")
	}
	// 23:59 本地 = 15:59Z；两分钟后 00:01 本地 = 次日 16:01Z。
	clock := &checkinIntegrationClock{now: time.Date(2026, 10, 22, 15, 59, 0, 0, time.UTC)}
	userID := createCheckinIntegrationUser(t, db, 0)
	mux := newCheckinIntegrationHarness(t, gormDB, rdb, userID,
		checkinTestEnvConfig(true, 5, 5), clock, loc)

	first := checkinIntegrationPost(t, mux)
	if first.Code != http.StatusOK {
		t.Fatalf("首次 = %d, want 200 (body=%q)", first.Code, first.Body.String())
	}
	var firstBody struct {
		Date string `json:"date"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &firstBody); err != nil || firstBody.Date != "2026-10-22" {
		t.Fatalf("first body=%s (err=%v), want date 2026-10-22", first.Body.String(), err)
	}

	clock.Set(time.Date(2026, 10, 22, 16, 1, 0, 0, time.UTC))
	second := checkinIntegrationPost(t, mux)
	if second.Code != http.StatusOK {
		t.Fatalf("跨日后 = %d, want 200 (body=%q)", second.Code, second.Body.String())
	}
	var secondBody struct {
		Date    string `json:"date"`
		Streak  int32  `json:"streak"`
		Awarded int64  `json:"awarded"`
	}
	if err := json.Unmarshal(second.Body.Bytes(), &secondBody); err != nil {
		t.Fatalf("decode second: %v", err)
	}
	if secondBody.Date != "2026-10-23" || secondBody.Streak != 2 || secondBody.Awarded != 5 {
		t.Fatalf("second body=%+v", secondBody)
	}
	checkins, ledger, balance := checkinIntegrationCounts(t, db, userID)
	if checkins != 2 || ledger != 2 || balance != 10 {
		t.Fatalf("跨日 checkins=%d ledger=%d balance=%d, want 2/2/10", checkins, ledger, balance)
	}
}
