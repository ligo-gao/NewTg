//go:build integration

package httpapi

// W8/MJ-d 的端到端验收：真实 PostgreSQL（任务行/幂等表/账本）+ 真实计费引擎
// + httptest 假上游。与 handlers_mj_integration_test.go 的 fake 会话不同，
// 这里验证「动作价 → 预扣金额 → 账本 hold → 幂等重放不重复扣费」真正串起来。
//
// 运行：NEWTG_TEST_DB_DSN=...（已应用全部迁移）go test -tags=integration ./internal/httpapi/

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/billing"
	"newtg/gateway/internal/billing/pricing"
	"newtg/gateway/internal/mj"
	"newtg/gateway/internal/tasks"
)

// mjDConfig 是计费引擎的最小配置读取器。
type mjDConfig map[string]json.RawMessage

func (m mjDConfig) Get(key string) (json.RawMessage, bool) {
	v, ok := m[key]
	return v, ok
}

func openMJDDeps(t *testing.T) (*sql.DB, *gorm.DB, *tasks.Repo, *billing.Engine) {
	t.Helper()
	db := openMJIntegrationDB(t)
	// 复用同一连接池，避免 gorm 单独再开一个。
	gormDB, err := gorm.Open(postgres.New(postgres.Config{Conn: db}), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		t.Fatalf("open gorm: %v", err)
	}
	cfg := mjDConfig{
		billing.ConfigPreConsumeRatio: json.RawMessage("1.0"),
		billing.ConfigTrustQuotaUSD:   json.RawMessage("0"),
	}
	source, err := pricing.NewSource(gormDB, cfg)
	if err != nil {
		t.Fatalf("pricing.NewSource: %v", err)
	}
	engine := billing.NewEngine(gormDB, nil, nil, cfg, source, nil, nil)
	return db, gormDB, tasks.NewRepo(db), engine
}

// ensureMJUser 建默认分组、用户与不限额令牌，返回 (userID, tokenID, key)。
func ensureMJUser(t *testing.T, db *sql.DB, balance int64) (int64, int64, string) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO user_groups (name, ratio, user_selectable, is_system, status)
		VALUES ('default', 100, true, true, 1) ON CONFLICT (name) DO NOTHING`); err != nil {
		t.Fatalf("ensure default group: %v", err)
	}
	uniq := time.Now().UnixNano()
	userName := fmt.Sprintf("mjd%d", uniq%1_000_000)
	var userID int64
	if err := db.QueryRow(`
		INSERT INTO users (username, password_hash, balance, "group")
		VALUES ($1, 'x', $2, 'default') RETURNING id`, userName, balance).Scan(&userID); err != nil {
		t.Fatalf("create user: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM users WHERE id = $1`, userID) })

	key := fmt.Sprintf("nkt-mjd-%d", uniq)
	var tokenID int64
	if err := db.QueryRow(`
		INSERT INTO api_tokens (user_id, name, key_hash, "group", remain_quota)
		VALUES ($1, 'mjd', $2, 'default', NULL) RETURNING id`,
		userID, auth.HashTokenKey(key)).Scan(&tokenID); err != nil {
		t.Fatalf("create token: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM api_tokens WHERE id = $1`, tokenID) })
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM wallet_transactions WHERE user_id = $1`, userID)
		_, _ = db.Exec(`DELETE FROM task_submit_idempotency WHERE user_id = $1`, userID)
		_, _ = db.Exec(`DELETE FROM tasks WHERE user_id = $1`, userID)
	})
	return userID, tokenID, key
}

func userBalanceMJD(t *testing.T, db *sql.DB, userID int64) int64 {
	t.Helper()
	var balance int64
	if err := db.QueryRow(`SELECT balance FROM users WHERE id = $1`, userID).Scan(&balance); err != nil {
		t.Fatalf("read balance: %v", err)
	}
	return balance
}

func countMJDRows(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	return n
}

func TestMJDActionPriceAndIdempotencyOnRealDB(t *testing.T) {
	db, _, repo, engine := openMJDDeps(t)
	sealer := NewSecretSealer("mj-d-integration-key")

	const price = int64(7)
	_, _ = db.Exec(`DELETE FROM model_pricing WHERE model = 'mj_imagine'`)
	if _, err := db.Exec(
		`INSERT INTO model_pricing (model, mode, prompt_price, status) VALUES ('mj_imagine', 1, $1, 1)`,
		price); err != nil {
		t.Fatalf("insert mj_imagine price: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM model_pricing WHERE model = 'mj_imagine'`) })
	// 缺价动作（describe）必须没有定价行。
	_, _ = db.Exec(`DELETE FROM model_pricing WHERE model = 'mj_describe'`)

	userID, tokenID, key := ensureMJUser(t, db, 10_000)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":1,"description":"ok","result":"mj_d_1"}`))
	}))
	defer upstream.Close()
	channelID := insertMJIntegrationChannel(t, db, sealer,
		fmt.Sprintf("mj-d-int-%d", time.Now().UnixNano()), mj.ChannelTypeMidjourney,
		upstream.URL, mjAllModels())
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM channels WHERE id = $1`, channelID) })

	tokens := mjTokenLookup{
		auth.HashTokenKey(key): &auth.TokenMeta{
			ID: tokenID, UserID: userID, Status: "enabled", Group: "default", UnlimitedQuota: true,
		},
	}
	handler := NewMJHandler(MJHandlerConfig{
		Store: repo, Channels: repo, Sealer: sealer, Tokens: tokens,
		Billing:     engine.NewSession,
		Config:      mjConfigured(nil),
		ImageSigner: mj.NewImageSigner("mj-d-integration-key"),
		Idempotency: repo,
		Uint64N:     func(uint64) uint64 { return 0 },
	})
	mux := http.NewServeMux()
	handler.Register(mux, RelayAuth(tokens, fakeTaskAccess{}, nil))

	submit := func(body, idemKey string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/mj/submit/imagine", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+key)
		if idemKey != "" {
			req.Header.Set("Idempotency-Key", idemKey)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	body := `{"prompt":"a cat"}`
	first := submit(body, "mjd-key-1")
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d (body=%s)", first.Code, first.Body.String())
	}
	if got := userBalanceMJD(t, db, userID); got != 10_000-price {
		t.Fatalf("balance after submit = %d，want %d（动作价）", got, 10_000-price)
	}
	if got := countMJDRows(t, db,
		`SELECT count(*) FROM wallet_transactions WHERE user_id = $1 AND type = 'consume_pre' AND status = 'hold'`,
		userID); got != 1 {
		t.Fatalf("hold rows = %d，want 1", got)
	}
	row, err := repo.GetByUserUpstreamID(context.Background(), userID, "mj_d_1")
	if err != nil {
		t.Fatalf("task row missing: %v", err)
	}
	if row.Quota != price || row.Family != tasks.FamilyMJ {
		t.Fatalf("task row = family=%s quota=%d，want mj/%d", row.Family, row.Quota, price)
	}

	// 同键同体：回放首次结果，余额/任务行/流水都不变。
	replay := submit(body, "mjd-key-1")
	if replay.Code != http.StatusOK || replay.Body.String() != first.Body.String() {
		t.Fatalf("replay = %d %s，want 首次 %d %s",
			replay.Code, replay.Body.String(), first.Code, first.Body.String())
	}
	if got := userBalanceMJD(t, db, userID); got != 10_000-price {
		t.Fatalf("replay 重复扣费: balance = %d", got)
	}
	if got := countMJDRows(t, db, `SELECT count(*) FROM tasks WHERE user_id = $1`, userID); got != 1 {
		t.Fatalf("replay 建了重复任务行: %d", got)
	}

	// 同键不同体：409，不动资金。
	conflict := submit(`{"prompt":"a dog"}`, "mjd-key-1")
	if conflict.Code != http.StatusConflict {
		t.Fatalf("conflict status = %d (body=%s)", conflict.Code, conflict.Body.String())
	}
	if got := userBalanceMJD(t, db, userID); got != 10_000-price {
		t.Fatalf("conflict 动了余额: %d", got)
	}

	// 缺价动作：400 task_pricing_unavailable，余额不变、无新 hold。
	missing := submitWithPath(t, mux, key, "/mj/submit/describe", `{"base64Array":["aGk="]}`)
	if missing.Code != http.StatusBadRequest {
		t.Fatalf("missing price status = %d (body=%s)", missing.Code, missing.Body.String())
	}
	var env map[string]any
	_ = json.Unmarshal(missing.Body.Bytes(), &env)
	if env["description"] != mj.DescriptionPricingUnavailable {
		t.Fatalf("missing price description = %v", env["description"])
	}
	if got := userBalanceMJD(t, db, userID); got != 10_000-price {
		t.Fatalf("缺价请求动了余额: %d", got)
	}
}

// TestMJDFreeTierOnRealDB 验证免计费档位在真实账本上不产生 hold/流水。
func TestMJDFreeTierOnRealDB(t *testing.T) {
	db, _, repo, engine := openMJDDeps(t)
	sealer := NewSecretSealer("mj-d-integration-key")
	userID, tokenID, key := ensureMJUser(t, db, 10_000)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":1,"description":"ok","result":"mj_d_free"}`))
	}))
	defer upstream.Close()
	channelID := insertMJIntegrationChannel(t, db, sealer,
		fmt.Sprintf("mj-d-free-%d", time.Now().UnixNano()), mj.ChannelTypeMidjourney,
		upstream.URL, mjAllModels())
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM channels WHERE id = $1`, channelID) })

	// 动作类端点需要原任务行。
	origin := mj.TaskRow(mj.Submission{
		PublicTaskID:   "task_mjd_origin",
		UpstreamTaskID: "mj_d_origin",
		Action:         mj.ActionImagine,
		UserID:         userID,
		TokenID:        tokenID,
		Group:          "default",
		ChannelID:      channelID,
		State:          tasks.StateSuccess,
		Quota:          0,
	})
	if err := repo.Insert(context.Background(), origin); err != nil {
		t.Fatalf("insert origin task: %v", err)
	}

	tokens := mjTokenLookup{
		auth.HashTokenKey(key): &auth.TokenMeta{
			ID: tokenID, UserID: userID, Status: "enabled", Group: "default", UnlimitedQuota: true,
		},
	}
	handler := NewMJHandler(MJHandlerConfig{
		Store: repo, Channels: repo, Sealer: sealer, Tokens: tokens,
		Billing:     engine.NewSession,
		Config:      mjConfigured(nil),
		ImageSigner: mj.NewImageSigner("mj-d-integration-key"),
		Idempotency: repo,
		Uint64N:     func(uint64) uint64 { return 0 },
	})
	mux := http.NewServeMux()
	handler.Register(mux, RelayAuth(tokens, fakeTaskAccess{}, nil))

	rec := submitWithPath(t, mux, key, "/mj/submit/change",
		`{"taskId":"mj_d_origin","action":"INPAINT","index":1}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("inpaint status = %d (body=%s)", rec.Code, rec.Body.String())
	}
	if got := userBalanceMJD(t, db, userID); got != 10_000 {
		t.Fatalf("免计费动作扣了费: balance = %d", got)
	}
	if got := countMJDRows(t, db,
		`SELECT count(*) FROM wallet_transactions WHERE user_id = $1`, userID); got != 0 {
		t.Fatalf("免计费动作写了流水: %d", got)
	}
}

func submitWithPath(t *testing.T, mux *http.ServeMux, key, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}
