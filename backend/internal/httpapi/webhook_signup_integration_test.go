//go:build integration

package httpapi

// B1b：注册成功的 user_signup Webhook 投递验收（真实 PostgreSQL，Redis 用
// miniredis）。运行：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_b1b?sslmode=disable' \
//	  go test -tags=integration ./internal/httpapi/ -run TestIntegrationSignupWebhook -v
//
// 覆盖：注册成功入队一次、重复注册（409）不重复入队、未订阅不入队、
// payload 字段与最小脱敏（无口令）。

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/notify"
)

type signupWebhookSubscription struct {
	events map[string]bool
}

func (s signupWebhookSubscription) Subscribed(event string) bool { return s.events[event] }

func openSignupWebhookDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("NEWTG_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("NEWTG_TEST_DB_DSN 未设置，跳过注册 Webhook 集成测试")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("ping test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// newSignupWebhookRouter 用真实 SQLAuthRepo + miniredis + 真实投递生产者装配
// 管理面；其它配置取默认值。
func newSignupWebhookRouter(t *testing.T, db *sql.DB,
	subscription notify.WebhookEventSubscription) *Router {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })

	store, err := notify.NewStore(db)
	if err != nil {
		t.Fatalf("notify.NewStore: %v", err)
	}
	producer, err := notify.NewWebhookEventProducer(store, subscription, nil)
	if err != nil {
		t.Fatalf("notify.NewWebhookEventProducer: %v", err)
	}
	rt, err := NewRouter(RouterConfig{
		Redis:         rdb,
		Repo:          NewSQLAuthRepo(db),
		Config:        mapConfig{},
		SessionSecret: []byte(testSessionSecret),
		SignupWebhook: producer,
	})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	return rt
}

func postRegister(t *testing.T, rt *Router, username, email string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(registerPayload(username, email, "N7x!qPz2wR", ""))
	if err != nil {
		t.Fatalf("marshal register body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/auth/register", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return serve(rt, req)
}

func signupDeliveryCount(t *testing.T, db *sql.DB, userID int64) int {
	t.Helper()
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM notification_deliveries
		WHERE channel = 'webhook' AND dedupe_key = $1`,
		fmt.Sprintf("user:%d:signup", userID)).Scan(&count); err != nil {
		t.Fatalf("count signup deliveries: %v", err)
	}
	return count
}

func TestIntegrationSignupWebhookEnqueuesOnce(t *testing.T) {
	db := openSignupWebhookDB(t)
	ensureDefaultGroup(t, db)
	rt := newSignupWebhookRouter(t, db, signupWebhookSubscription{
		events: map[string]bool{"user_signup": true},
	})

	suffix := "b1b_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	username := suffix
	email := suffix + "@example.com"
	rr := postRegister(t, rt, username, email)
	if rr.Code != http.StatusCreated {
		t.Fatalf("register status = %d（body=%q）", rr.Code, rr.Body.String())
	}

	var userID int64
	if err := db.QueryRow(`SELECT id FROM users WHERE username = $1`, username).Scan(&userID); err != nil {
		t.Fatalf("read registered user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM notification_deliveries WHERE dedupe_key = $1`,
			fmt.Sprintf("user:%d:signup", userID))
		cleanupAuthUser(t, db, userID)
	})

	if got := signupDeliveryCount(t, db, userID); got != 1 {
		t.Fatalf("signup deliveries = %d, want 1", got)
	}

	var (
		category, channel, dedupe, status string
		payload                           []byte
	)
	if err := db.QueryRow(`SELECT category, channel, dedupe_key, status, payload
		FROM notification_deliveries WHERE dedupe_key = $1`,
		fmt.Sprintf("user:%d:signup", userID)).
		Scan(&category, &channel, &dedupe, &status, &payload); err != nil {
		t.Fatalf("read signup delivery: %v", err)
	}
	if category != "auth.welcome" || channel != "webhook" || status != "queued" {
		t.Fatalf("category/channel/status = %s/%s/%s", category, channel, status)
	}
	if dedupe != fmt.Sprintf("user:%d:signup", userID) {
		t.Fatalf("dedupe_key = %q", dedupe)
	}

	var body struct {
		Event     string `json:"event"`
		UserID    int64  `json:"user_id"`
		Username  string `json:"username"`
		CreatedAt string `json:"created_at"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatalf("payload JSON: %v（%s）", err, payload)
	}
	if body.Event != "user_signup" || body.UserID != userID || body.Username != username {
		t.Fatalf("payload = %+v", body)
	}
	if _, err := time.Parse(time.RFC3339, body.CreatedAt); err != nil {
		t.Fatalf("created_at %q is not RFC3339: %v", body.CreatedAt, err)
	}
	lower := strings.ToLower(string(payload))
	for _, forbidden := range []string{"password", "secret", "token", "email_code"} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("payload leaks %q: %s", forbidden, payload)
		}
	}

	// 重复注册被 409 拒绝，不会产生第二条投递。
	duplicate := postRegister(t, rt, username, email)
	if duplicate.Code != http.StatusConflict {
		t.Fatalf("duplicate register status = %d, want 409", duplicate.Code)
	}
	if got := signupDeliveryCount(t, db, userID); got != 1 {
		t.Fatalf("after duplicate signup deliveries = %d, want 1", got)
	}
}

func TestIntegrationSignupWebhookSkipsUnsubscribed(t *testing.T) {
	db := openSignupWebhookDB(t)
	ensureDefaultGroup(t, db)
	rt := newSignupWebhookRouter(t, db, signupWebhookSubscription{
		events: map[string]bool{"payment_success": true},
	})

	suffix := "b1b_nosub_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	rr := postRegister(t, rt, suffix, suffix+"@example.com")
	if rr.Code != http.StatusCreated {
		t.Fatalf("register status = %d（body=%q）", rr.Code, rr.Body.String())
	}

	var userID int64
	if err := db.QueryRow(`SELECT id FROM users WHERE username = $1`, suffix).Scan(&userID); err != nil {
		t.Fatalf("read registered user: %v", err)
	}
	t.Cleanup(func() { cleanupAuthUser(t, db, userID) })

	if got := signupDeliveryCount(t, db, userID); got != 0 {
		t.Fatalf("unsubscribed signup wrote %d deliveries, want 0", got)
	}
}
