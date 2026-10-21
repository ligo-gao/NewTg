//go:build integration

package channelhealth

// W7/M4-a：Reporter 的真实 PostgreSQL 端到端验收。
//
// 运行：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_m4?sslmode=disable' \
//	NEWTG_TEST_LOGS_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_m4_logs?sslmode=disable' \
//	  go test -tags=integration -p 1 ./internal/channelhealth/ -run TestReporterIntegration -v
//
// 覆盖 brief 的「审计与通知落库/直投」「失败不影响主响应」「并发只迁移一次」
// 「manual_disabled 不被覆盖」「全局开关/auto_ban/纯 5xx 不触发」。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/notify"
	"newtg/gateway/internal/store"
)

func openReporterTestDB(t *testing.T, env string) *sql.DB {
	t.Helper()
	dsn := os.Getenv(env)
	if dsn == "" {
		t.Skip(env + " 未设置，跳过自动禁用集成测试")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open %s: %v", env, err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("ping %s: %v", env, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

type integrationFixture struct {
	db        *sql.DB
	logsDB    *sql.DB
	repo      *store.ChannelRepo
	audit     *audit.Logger
	notifySvc *notify.Service
	auditFrom int64
}

func newIntegrationFixture(t *testing.T) *integrationFixture {
	t.Helper()
	db := openReporterTestDB(t, "NEWTG_TEST_DB_DSN")
	logsDB := openReporterTestDB(t, "NEWTG_TEST_LOGS_DSN")

	notifyStore, err := notify.NewStore(db)
	if err != nil {
		t.Fatalf("notify.NewStore: %v", err)
	}
	notifySvc, err := notify.NewService(notify.ServiceConfig{Store: notifyStore})
	if err != nil {
		t.Fatalf("notify.NewService: %v", err)
	}
	if err := notifySvc.EnsureSeeded(context.Background()); err != nil {
		t.Fatalf("seed notification templates: %v", err)
	}

	var auditFrom int64
	if err := logsDB.QueryRow(`SELECT COALESCE(max(id), 0) FROM audit_logs`).Scan(&auditFrom); err != nil {
		t.Fatalf("read audit max id: %v", err)
	}
	return &integrationFixture{
		db:        db,
		logsDB:    logsDB,
		repo:      store.NewChannelRepo(db),
		audit:     audit.New(logsDB),
		notifySvc: notifySvc,
		auditFrom: auditFrom,
	}
}

// integrationSeq 让同一次运行内的渠道名/用户名互不冲突（并避开上轮遗留行）。
var integrationSeq atomic.Int64

func uniqueSuffix() string {
	return strconv.FormatInt(time.Now().UnixNano()%1_000_000_000, 10) + "-" +
		strconv.FormatInt(integrationSeq.Add(1), 10)
}

func (f *integrationFixture) insertChannel(t *testing.T, prefix string, status int16, autoBan bool) int64 {
	t.Helper()
	name := prefix + "-" + uniqueSuffix()
	var id int64
	err := f.db.QueryRow(`
		INSERT INTO channels (name, type, credentials, supported_models, model_mapping,
			weight, priority, status, auto_ban)
		VALUES ($1, 'openai', $2, '["gpt-test"]'::jsonb, '{}'::jsonb, 1, 0, $3, $4)
		RETURNING id`, name, []byte{0}, status, autoBan).Scan(&id)
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM channels WHERE id = $1`, id) })
	return id
}

// insertUser 插入一个指定角色/语言的用户；用户名带运行内序号避免唯一冲突。
func (f *integrationFixture) insertUser(t *testing.T, role, language string) int64 {
	t.Helper()
	username := "m4arep" + strings.ReplaceAll(uniqueSuffix(), "-", "")
	var id int64
	err := f.db.QueryRow(`
		INSERT INTO users (username, role, status, language)
		VALUES ($1, $2, 'enabled', $3)
		RETURNING id`, username, role, language).Scan(&id)
	if err != nil {
		t.Fatalf("insert user %s: %v", username, err)
	}
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM users WHERE id = $1`, id) })
	return id
}

func (f *integrationFixture) channelRow(t *testing.T, id int64) (status int16, reason sql.NullString) {
	t.Helper()
	err := f.db.QueryRow(`SELECT status, auto_disabled_reason FROM channels WHERE id = $1`, id).
		Scan(&status, &reason)
	if err != nil {
		t.Fatalf("read channel %d: %v", id, err)
	}
	return status, reason
}

func (f *integrationFixture) auditRows(t *testing.T, channelID int64) []struct {
	UserID   int64
	Username string
	Category string
	Detail   string
	Before   string
	After    string
} {
	t.Helper()
	rows, err := f.logsDB.Query(`
		SELECT user_id, username, category, COALESCE(detail::text, ''),
		       COALESCE(before::text, ''), COALESCE(after::text, '')
		FROM audit_logs
		WHERE id > $1 AND detail->>'action' = $2
		ORDER BY id`, f.auditFrom, audit.ActionChannelAutoDisable)
	if err != nil {
		t.Fatalf("query audit rows: %v", err)
	}
	defer rows.Close()
	var out []struct {
		UserID   int64
		Username string
		Category string
		Detail   string
		Before   string
		After    string
	}
	for rows.Next() {
		var row struct {
			UserID   int64
			Username string
			Category string
			Detail   string
			Before   string
			After    string
		}
		if err := rows.Scan(&row.UserID, &row.Username, &row.Category, &row.Detail, &row.Before, &row.After); err != nil {
			t.Fatalf("scan audit row: %v", err)
		}
		var detail map[string]any
		if err := json.Unmarshal([]byte(row.Detail), &detail); err != nil {
			t.Fatalf("decode audit detail: %v", err)
		}
		if detail["channel_id"] == float64(channelID) {
			out = append(out, row)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate audit rows: %v", err)
	}
	return out
}

func (f *integrationFixture) deliveryRows(t *testing.T, channelID int64) []struct {
	UserID  int64
	Locale  string
	Payload string
	Dedupe  string
} {
	t.Helper()
	rows, err := f.db.Query(`
		SELECT recipient_user_id, locale, COALESCE(payload::text, ''), dedupe_key
		FROM notification_deliveries
		WHERE channel = 'inapp' AND category = 'ops.channel_auto_disabled'
		  AND dedupe_key LIKE $1
		ORDER BY recipient_user_id`,
		fmt.Sprintf("channel_auto_disabled:%d:%%", channelID))
	if err != nil {
		t.Fatalf("query deliveries: %v", err)
	}
	defer rows.Close()
	var out []struct {
		UserID  int64
		Locale  string
		Payload string
		Dedupe  string
	}
	for rows.Next() {
		var row struct {
			UserID  int64
			Locale  string
			Payload string
			Dedupe  string
		}
		if err := rows.Scan(&row.UserID, &row.Locale, &row.Payload, &row.Dedupe); err != nil {
			t.Fatalf("scan delivery: %v", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate deliveries: %v", err)
	}
	return out
}

func (f *integrationFixture) cleanupDeliveries(t *testing.T, channelID int64) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM notification_deliveries
			WHERE channel = 'inapp' AND category = 'ops.channel_auto_disabled'
			  AND dedupe_key LIKE $1`, fmt.Sprintf("channel_auto_disabled:%d:%%", channelID))
	})
}

func integrationReporter(t *testing.T, f *integrationFixture, enabled bool) *Reporter {
	t.Helper()
	reporter, err := NewReporter(ReporterConfig{
		Store:       f.repo,
		Config:      mapSettings{KeyAutoDisableEnabled: json.RawMessage(fmt.Sprintf("%t", enabled))},
		Audit:       f.audit,
		Notifier:    f.notifySvc,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		ChannelsURL: "/admin/channels",
	})
	if err != nil {
		t.Fatalf("NewReporter: %v", err)
	}
	return reporter
}

// TestReporterIntegrationAuthDisable：401 → status=3 + reason + 审计 + 每个
// 管理员一条本地化站内通知（普通用户没有）。
func TestReporterIntegrationAuthDisable(t *testing.T) {
	f := newIntegrationFixture(t)
	channelID := f.insertChannel(t, "m4a-auth", 1, true)
	f.cleanupDeliveries(t, channelID)
	adminZH := f.insertUser(t, "admin", "zh-CN")
	adminEN := f.insertUser(t, "admin", "en")
	user := f.insertUser(t, "user", "zh-CN")

	reporter := integrationReporter(t, f, true)
	reporter.ReportAttemptFailure(context.Background(), int(channelID), true,
		contract.ErrUpstreamAuth, 401, []byte(`{"error":"invalid api key"}`))
	if processed := reporter.RunOnce(context.Background()); processed != 1 {
		t.Fatalf("RunOnce processed = %d, want 1", processed)
	}
	if got := reporter.Applied(); got != 1 {
		t.Fatalf("Applied = %d, want 1", got)
	}

	status, reason := f.channelRow(t, channelID)
	if status != 3 || !reason.Valid || reason.String != ReasonAuthError {
		t.Fatalf("channel = status %d reason %v, want 3/%s", status, reason, ReasonAuthError)
	}

	audits := f.auditRows(t, channelID)
	if len(audits) != 1 {
		t.Fatalf("审计行数 = %d, want 1: %+v", len(audits), audits)
	}
	if audits[0].UserID != 0 || audits[0].Username != "system" || audits[0].Category != "operation" {
		t.Fatalf("审计 actor = %d/%q/%q, want 0/system/operation（R-M4-10）",
			audits[0].UserID, audits[0].Username, audits[0].Category)
	}
	if strings.Contains(audits[0].Detail, "invalid api key") ||
		strings.Contains(audits[0].Detail, "http") {
		t.Fatalf("审计 detail 泄漏响应体/URL: %s", audits[0].Detail)
	}

	deliveries := f.deliveryRows(t, channelID)
	if len(deliveries) != 2 {
		t.Fatalf("站内投递行数 = %d, want 2（仅管理员）: %+v", len(deliveries), deliveries)
	}
	byUser := make(map[int64]string, len(deliveries))
	for _, delivery := range deliveries {
		if delivery.UserID == user {
			t.Fatalf("普通用户收到了运维通知: %+v", delivery)
		}
		byUser[delivery.UserID] = delivery.Payload
	}
	if payload, ok := byUser[adminZH]; !ok || !strings.Contains(payload, "上游鉴权错误") ||
		!strings.Contains(payload, "渠道已自动禁用") {
		t.Fatalf("zh-CN 通知 payload = %q", payload)
	}
	if payload, ok := byUser[adminEN]; !ok || !strings.Contains(payload, "Upstream authentication error") ||
		!strings.Contains(payload, "Channel auto-disabled") {
		t.Fatalf("en 通知 payload = %q", payload)
	}

	// 读取面直投：管理员能在通知中心看到（title/body 来自 payload）。
	items, total, err := f.notifySvc.ListUserNotifications(context.Background(), adminZH, 1, 10)
	if err != nil {
		t.Fatalf("ListUserNotifications: %v", err)
	}
	if total != 1 || len(items) != 1 {
		t.Fatalf("通知中心 total/items = %d/%d, want 1/1", total, len(items))
	}
	if items[0].Title != "渠道已自动禁用" || !strings.Contains(items[0].Body, "上游鉴权错误") {
		t.Fatalf("通知中心 item = %+v", items[0])
	}
	if items[0].LinkURL != "/admin/channels" {
		t.Fatalf("link_url = %q", items[0].LinkURL)
	}
}

// TestReporterIntegrationForbiddenTriggers：403 与 401 同属 ErrUpstreamAuth，
// 同样迁移为 channel_auth_error（R-M4-01 C 明确包含 403）。
func TestReporterIntegrationForbiddenTriggers(t *testing.T) {
	f := newIntegrationFixture(t)
	id := f.insertChannel(t, "m4a-403", 1, true)
	f.cleanupDeliveries(t, id)
	reporter := integrationReporter(t, f, true)
	reporter.ReportAttemptFailure(context.Background(), int(id), true,
		contract.ErrUpstreamAuth, 403, []byte(`{"error":"forbidden"}`))
	reporter.RunOnce(context.Background())
	if status, reason := f.channelRow(t, id); status != 3 || !reason.Valid || reason.String != ReasonAuthError {
		t.Fatalf("403 渠道 = status %d reason %v, want 3/%s", status, reason, ReasonAuthError)
	}
	if audits := f.auditRows(t, id); len(audits) != 1 {
		t.Fatalf("403 审计行数 = %d, want 1", len(audits))
	}
}

// TestReporterIntegrationKeywordAndPlain5xx：关键词触发 → channel_keyword；
// 纯 5xx 不上报（状态保持不变、无副作用）。
func TestReporterIntegrationKeywordAndPlain5xx(t *testing.T) {
	f := newIntegrationFixture(t)
	reporter := integrationReporter(t, f, true)

	keywordID := f.insertChannel(t, "m4a-keyword", 1, true)
	f.cleanupDeliveries(t, keywordID)
	reporter.ReportAttemptFailure(context.Background(), int(keywordID), true,
		contract.ErrUpstreamServer, 500, []byte("Your credit balance is too low"))
	reporter.RunOnce(context.Background())
	if status, reason := f.channelRow(t, keywordID); status != 3 || !reason.Valid || reason.String != ReasonKeyword {
		t.Fatalf("关键词渠道 = status %d reason %v, want 3/%s", status, reason, ReasonKeyword)
	}

	plainID := f.insertChannel(t, "m4a-5xx", 1, true)
	reporter.ReportAttemptFailure(context.Background(), int(plainID), true,
		contract.ErrUpstreamServer, 500, []byte(`{"error":"internal server error"}`))
	if processed := reporter.RunOnce(context.Background()); processed != 0 {
		t.Fatalf("纯 5xx 入队条数 = %d, want 0", processed)
	}
	if status, reason := f.channelRow(t, plainID); status != 1 || reason.Valid {
		t.Fatalf("纯 5xx 渠道 = status %d reason %v, want 1/NULL", status, reason)
	}
}

// TestReporterIntegrationSwitchOffAndAutoBanFalse：全局开关关闭或 auto_ban=false
// 时，即使 401 也不迁移、不写审计。
func TestReporterIntegrationSwitchOffAndAutoBanFalse(t *testing.T) {
	f := newIntegrationFixture(t)

	offID := f.insertChannel(t, "m4a-off", 1, true)
	offReporter := integrationReporter(t, f, false)
	offReporter.ReportAttemptFailure(context.Background(), int(offID), true, contract.ErrUpstreamAuth, 401, nil)
	offReporter.RunOnce(context.Background())
	if status, _ := f.channelRow(t, offID); status != 1 {
		t.Fatalf("开关关闭时 status = %d, want 1", status)
	}

	autoBanID := f.insertChannel(t, "m4a-noban", 1, false)
	onReporter := integrationReporter(t, f, true)
	onReporter.ReportAttemptFailure(context.Background(), int(autoBanID), false, contract.ErrUpstreamAuth, 401, nil)
	onReporter.RunOnce(context.Background())
	if status, _ := f.channelRow(t, autoBanID); status != 1 {
		t.Fatalf("auto_ban=false 时 status = %d, want 1", status)
	}
	if len(f.auditRows(t, offID)) != 0 || len(f.auditRows(t, autoBanID)) != 0 {
		t.Fatal("未迁移的渠道不得产生审计")
	}
}

// TestReporterIntegrationManualDisabledUnchanged：manual_disabled 不被自动禁用
// 覆盖，也不产生审计/通知。
func TestReporterIntegrationManualDisabledUnchanged(t *testing.T) {
	f := newIntegrationFixture(t)
	id := f.insertChannel(t, "m4a-manual", 2, true)
	reporter := integrationReporter(t, f, true)
	reporter.ReportAttemptFailure(context.Background(), int(id), true, contract.ErrUpstreamAuth, 403, nil)
	reporter.RunOnce(context.Background())
	if status, reason := f.channelRow(t, id); status != 2 || reason.Valid {
		t.Fatalf("manual_disabled 被改写: status %d reason %v", status, reason)
	}
	if len(f.auditRows(t, id)) != 0 {
		t.Fatal("manual_disabled 不得产生 channel.auto_disable 审计")
	}
}

// TestReporterIntegrationConcurrentSingleTransition：并发上报同一渠道只产生
// 一条审计、每个管理员一条通知（条件更新 + 冷却 + 通知 dedupe 三层收敛）。
func TestReporterIntegrationConcurrentSingleTransition(t *testing.T) {
	f := newIntegrationFixture(t)
	id := f.insertChannel(t, "m4a-conc", 1, true)
	f.cleanupDeliveries(t, id)
	f.insertUser(t, "admin", "en")
	reporter := integrationReporter(t, f, true)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reporter.ReportAttemptFailure(context.Background(), int(id), true,
				contract.ErrUpstreamAuth, 401, nil)
		}()
	}
	wg.Wait()
	// 冷却窗口内只有一条入队；即使入队多条，条件更新也只迁移一次。
	reporter.RunOnce(context.Background())
	if status, reason := f.channelRow(t, id); status != 3 || !reason.Valid || reason.String != ReasonAuthError {
		t.Fatalf("channel = status %d reason %v", status, reason)
	}
	if audits := f.auditRows(t, id); len(audits) != 1 {
		t.Fatalf("审计行数 = %d, want 1（同一迁移只写一条）", len(audits))
	}
	deliveries := f.deliveryRows(t, id)
	if len(deliveries) != 1 {
		t.Fatalf("投递行数 = %d, want 1（每个管理员一条）", len(deliveries))
	}
}
