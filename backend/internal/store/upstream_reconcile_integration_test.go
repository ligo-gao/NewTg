//go:build integration

package store

// 本文件是上游对账四个仓储的集成测试（连真 PostgreSQL，主库 + 日志库）。
//
// 运行（仓库自带的测试实例）：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_test?sslmode=disable' \
//	NEWTG_TEST_LOGS_DSN='postgres://newtg:newtg@localhost:55432/newtg_logs?sslmode=disable' \
//	  go test -tags=integration ./internal/store -run 'Upstream|Snapshot|Reconcile|Aggregate' -v
//
// DSN 未设置时跳过；表结构由 migrations 负责，测试只插入唯一前缀的行并在
// 结束时按 id / request_id 清理，不依赖库中既有数据。

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	// pgx/v5 stdlib 驱动与生产一致。
	_ "github.com/jackc/pgx/v5/stdlib"
)

// openUpstreamReconcileTestDB 连接主库；NEWTG_TEST_DB_DSN 优先，兼容 TEST_DB_DSN。
func openUpstreamReconcileTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("NEWTG_TEST_DB_DSN")
	if dsn == "" {
		dsn = os.Getenv("TEST_DB_DSN")
	}
	if dsn == "" {
		t.Skip("NEWTG_TEST_DB_DSN/TEST_DB_DSN 未设置，跳过上游对账集成测试")
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

// openUpstreamReconcileLogsDB 连接日志库：优先 NEWTG_TEST_LOGS_DSN，
// 否则沿用既有测试手法把主库 DSN 的 dbname 换成 newtg_logs。
func openUpstreamReconcileLogsDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("NEWTG_TEST_LOGS_DSN")
	if dsn == "" {
		mainDSN := os.Getenv("NEWTG_TEST_DB_DSN")
		if mainDSN == "" {
			t.Skip("NEWTG_TEST_LOGS_DSN / NEWTG_TEST_DB_DSN 未设置，跳过日志库聚合测试")
		}
		u, err := url.Parse(mainDSN)
		if err != nil || u.Scheme == "" {
			t.Fatalf("解析 NEWTG_TEST_DB_DSN 失败: %v", err)
		}
		u.Path = "/newtg_logs"
		dsn = u.String()
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open logs db: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("ping logs db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// createReconcileAccount 创建一个唯一命名的账号并登记清理。
func createReconcileAccount(t *testing.T, repo *UpstreamAccountRepo, suffix string) *UpstreamAccount {
	t.Helper()
	a, err := repo.Create(context.Background(), UpstreamAccount{
		Name:     "it-upstream-" + suffix,
		Provider: "deepseek",
		Timezone: "Asia/Shanghai",
		Note:     "integration test",
	})
	if err != nil {
		t.Fatalf("create upstream account: %v", err)
	}
	t.Cleanup(func() {
		if _, err := repo.db.Exec(`DELETE FROM upstream_accounts WHERE id = $1`, a.ID); err != nil {
			t.Errorf("cleanup upstream account %d: %v", a.ID, err)
		}
	})
	return a
}

// insertReconcileChannel 插入一个绑定账号的渠道并登记清理（清理先于账号删除）。
func insertReconcileChannel(t *testing.T, db *sql.DB, name string, accountID int64) int64 {
	t.Helper()
	var id int64
	err := db.QueryRow(`
		INSERT INTO channels (name, type, credentials, supported_models, model_mapping,
			weight, priority, status, auto_ban, upstream_account_id)
		VALUES ($1, 'openai', $2, '["it-model"]'::jsonb, '{}'::jsonb, 1, 10, 1, true, $3)
		RETURNING id`, name, []byte{0}, accountID).Scan(&id)
	if err != nil {
		t.Fatalf("insert channel %s: %v", name, err)
	}
	t.Cleanup(func() {
		if _, err := db.Exec(`DELETE FROM channels WHERE id = $1`, id); err != nil {
			t.Errorf("cleanup channel %d: %v", id, err)
		}
	})
	return id
}

// TestUpstreamAccountCRUD 覆盖 create → list（状态筛选 + 分页）→ update（乐观锁）
// → delete（有渠道绑定时被 FK 拒绝）。
func TestUpstreamAccountCRUD(t *testing.T) {
	db := openUpstreamReconcileTestDB(t)
	repo := NewUpstreamAccountRepo(db)
	ctx := context.Background()
	suffix := uuid.NewString()[:8]

	active := createReconcileAccount(t, repo, suffix+"-a")
	if active.Status != 1 || active.Currency != "CNY" {
		t.Fatalf("create defaults = status %d currency %q, want 1/CNY", active.Status, active.Currency)
	}
	disabled, err := repo.Create(ctx, UpstreamAccount{
		Name: "it-upstream-" + suffix + "-b", Provider: "kimi", Status: 2,
	})
	if err != nil {
		t.Fatalf("create disabled account: %v", err)
	}
	t.Cleanup(func() {
		if _, err := db.Exec(`DELETE FROM upstream_accounts WHERE id = $1`, disabled.ID); err != nil {
			t.Errorf("cleanup disabled account %d: %v", disabled.ID, err)
		}
	})

	if _, err := repo.Create(ctx, UpstreamAccount{Name: active.Name, Provider: "x"}); !errors.Is(err, ErrNameConflict) {
		t.Fatalf("duplicate name err = %v, want ErrNameConflict", err)
	}

	got, err := repo.Get(ctx, active.ID)
	if err != nil || got.Name != active.Name {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	if _, err := repo.Get(ctx, 1<<62); !errors.Is(err, ErrUpstreamAccountNotFound) {
		t.Fatalf("Get missing err = %v, want ErrUpstreamAccountNotFound", err)
	}

	all, total, err := repo.List(ctx, nil, 1, 100)
	if err != nil {
		t.Fatalf("List all: %v", err)
	}
	if total < 2 || !containsAccountID(all, active.ID) || !containsAccountID(all, disabled.ID) {
		t.Fatalf("List all = %d rows (total %d), missing created accounts", len(all), total)
	}

	activeStatus := int16(1)
	onlyActive, _, err := repo.List(ctx, &activeStatus, 1, 100)
	if err != nil {
		t.Fatalf("List active: %v", err)
	}
	if !containsAccountID(onlyActive, active.ID) || containsAccountID(onlyActive, disabled.ID) {
		t.Fatalf("status filter = %+v, want only active", onlyActive)
	}

	page1, total1, err := repo.List(ctx, nil, 1, 1)
	if err != nil {
		t.Fatalf("List page 1: %v", err)
	}
	page2, _, err := repo.List(ctx, nil, 2, 1)
	if err != nil {
		t.Fatalf("List page 2: %v", err)
	}
	if len(page1) != 1 || len(page2) != 1 || page1[0].ID == page2[0].ID || total1 != total {
		t.Fatalf("paging broken: page1=%+v page2=%+v total=%d/%d", page1, page2, total1, total)
	}

	consoleURL := "https://console.example.com"
	newName := active.Name + "-renamed"
	updated, err := repo.Update(ctx, active.ID, UpstreamAccountPatch{
		ExpectedUpdatedAt: active.UpdatedAt,
		Name:              &newName,
		ConsoleURLSet:     true,
		ConsoleURL:        &consoleURL,
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Name != newName || updated.ConsoleURL == nil || *updated.ConsoleURL != consoleURL {
		t.Fatalf("Update result = %+v", updated)
	}

	// 同一个旧版本再写一次 → 乐观锁失配。
	if _, err := repo.Update(ctx, active.ID, UpstreamAccountPatch{
		ExpectedUpdatedAt: active.UpdatedAt, Note: reconcileStringPtr("stale"),
	}); !errors.Is(err, ErrUpstreamAccountConflict) {
		t.Fatalf("stale update err = %v, want ErrUpstreamAccountConflict", err)
	}

	// Set=true 且指针为 nil 要清空 console_url（与 PATCH 的缺省区分）。
	cleared, err := repo.Update(ctx, active.ID, UpstreamAccountPatch{
		ExpectedUpdatedAt: updated.UpdatedAt, ConsoleURLSet: true, ConsoleURL: nil,
	})
	if err != nil {
		t.Fatalf("clear console_url: %v", err)
	}
	if cleared.ConsoleURL != nil {
		t.Fatalf("console_url = %v, want nil", *cleared.ConsoleURL)
	}

	if _, err := repo.Update(ctx, 1<<62, UpstreamAccountPatch{
		ExpectedUpdatedAt: time.Now(), Note: reconcileStringPtr("nope"),
	}); !errors.Is(err, ErrUpstreamAccountNotFound) {
		t.Fatalf("update missing err = %v, want ErrUpstreamAccountNotFound", err)
	}

	channelID := insertReconcileChannel(t, db, "it-upstream-"+suffix+"-ch", active.ID)
	ids, err := repo.ChannelIDs(ctx, active.ID)
	if err != nil || len(ids) != 1 || ids[0] != channelID {
		t.Fatalf("ChannelIDs = %v, %v; want [%d]", ids, err, channelID)
	}

	// 渠道仍绑定 → ON DELETE RESTRICT（23503）→ ErrUpstreamAccountInUse。
	if err := repo.Delete(ctx, active.ID); !errors.Is(err, ErrUpstreamAccountInUse) {
		t.Fatalf("delete bound account err = %v, want ErrUpstreamAccountInUse", err)
	}

	if _, err := db.Exec(`DELETE FROM channels WHERE id = $1`, channelID); err != nil {
		t.Fatalf("unbind channel: %v", err)
	}
	if err := repo.Delete(ctx, active.ID); err != nil {
		t.Fatalf("delete account: %v", err)
	}
	if _, err := repo.Get(ctx, active.ID); !errors.Is(err, ErrUpstreamAccountNotFound) {
		t.Fatalf("Get deleted err = %v, want ErrUpstreamAccountNotFound", err)
	}
	if err := repo.Delete(ctx, active.ID); !errors.Is(err, ErrUpstreamAccountNotFound) {
		t.Fatalf("delete again err = %v, want ErrUpstreamAccountNotFound", err)
	}
}

// TestSnapshotUpsertIsIdempotent 覆盖同键重复导入：库里只留一行，后写值胜出。
func TestSnapshotUpsertIsIdempotent(t *testing.T) {
	db := openUpstreamReconcileTestDB(t)
	accounts := NewUpstreamAccountRepo(db)
	repo := NewUpstreamSnapshotRepo(db)
	ctx := context.Background()
	suffix := uuid.NewString()[:8]
	account := createReconcileAccount(t, accounts, suffix)
	t.Cleanup(func() {
		if _, err := db.Exec(`DELETE FROM upstream_usage_snapshots WHERE account_id = $1`, account.ID); err != nil {
			t.Errorf("cleanup snapshots for account %d: %v", account.ID, err)
		}
	})

	day := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	first, second := int64(10), int64(20)

	n, err := repo.Upsert(ctx, []UpstreamSnapshot{{
		AccountID: account.ID, UsageDate: day, UpstreamModel: "m",
		CacheHitTokens: &first, Currency: "CNY", Source: "manual", Note: "first",
	}})
	if err != nil || n != 1 {
		t.Fatalf("first upsert = %d, %v; want 1, nil", n, err)
	}

	n, err = repo.Upsert(ctx, []UpstreamSnapshot{{
		AccountID: account.ID, UsageDate: day, UpstreamModel: "m",
		CacheHitTokens: &second, Currency: "CNY", Source: "csv", Note: "second",
	}})
	if err != nil || n != 1 {
		t.Fatalf("second upsert = %d, %v; want 1, nil", n, err)
	}

	rows, err := repo.ListRange(ctx, account.ID, day, day)
	if err != nil {
		t.Fatalf("ListRange: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows after two upserts = %d, want 1", len(rows))
	}
	if rows[0].CacheHitTokens == nil || *rows[0].CacheHitTokens != second || rows[0].Note != "second" || rows[0].Source != "csv" {
		t.Fatalf("row = %+v, want second values to win", rows[0])
	}

	// 同一次调用里的重复键必须先折叠，否则 PostgreSQL 报
	// "ON CONFLICT DO UPDATE command cannot affect row a second time"。
	a, bVal := int64(1), int64(2)
	n, err = repo.Upsert(ctx, []UpstreamSnapshot{
		{AccountID: account.ID, UsageDate: day, UpstreamModel: "m2", CacheHitTokens: &a},
		{AccountID: account.ID, UsageDate: day, UpstreamModel: "m2", CacheHitTokens: &bVal},
	})
	if err != nil || n != 1 {
		t.Fatalf("in-batch dedupe = %d, %v; want 1, nil", n, err)
	}
	rows, err = repo.ListRange(ctx, account.ID, day, day)
	if err != nil {
		t.Fatalf("ListRange after batch dedupe: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	for _, row := range rows {
		if row.UpstreamModel == "m2" && (row.CacheHitTokens == nil || *row.CacheHitTokens != bVal) {
			t.Fatalf("m2 row = %+v, want last value 2", row)
		}
	}
}

// TestInsertItemsRejectsDuplicateModelDay 覆盖唯一约束与明细读写/汇总路径。
func TestInsertItemsRejectsDuplicateModelDay(t *testing.T) {
	db := openUpstreamReconcileTestDB(t)
	accounts := NewUpstreamAccountRepo(db)
	repo := NewReconcileRepo(db)
	ctx := context.Background()
	suffix := uuid.NewString()[:8]
	account := createReconcileAccount(t, accounts, suffix)

	day := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	runID, err := repo.InsertRun(ctx, ReconcileRun{
		AccountID:            account.ID,
		WindowStart:          day,
		WindowEnd:            day.AddDate(0, 0, 1),
		TriggeredBy:          "manual",
		TokenToleranceBP:     100,
		AmountToleranceMinor: 100,
		QuotaPerUnit:         500000,
		Timezone:             "Asia/Shanghai",
	})
	if err != nil {
		t.Fatalf("InsertRun: %v", err)
	}
	t.Cleanup(func() {
		if _, err := db.Exec(`DELETE FROM upstream_reconciliation_runs WHERE id = $1`, runID); err != nil {
			t.Errorf("cleanup run %d: %v", runID, err)
		}
	})

	duplicate := []ReconcileItem{
		{RunID: runID, AccountID: account.ID, UsageDate: day, UpstreamModel: "m", Severity: "ok"},
		{RunID: runID, AccountID: account.ID, UsageDate: day, UpstreamModel: "m", Severity: "warn"},
	}
	if err := repo.InsertItems(ctx, duplicate); !errors.Is(err, ErrDuplicateItem) {
		t.Fatalf("duplicate insert err = %v, want ErrDuplicateItem", err)
	}

	revenue := int64(500)
	item := ReconcileItem{
		RunID: runID, AccountID: account.ID, UsageDate: day, UpstreamModel: "m-ok",
		LocalRequests: 10, LocalCacheHitTokens: 40, LocalCacheMissTokens: 60,
		LocalOutputTokens: 10, LocalCostCredits: 1000,
		RevenueMinor: &revenue, Severity: "warn", Note: "seed",
	}
	if err := repo.InsertItems(ctx, []ReconcileItem{item}); err != nil {
		t.Fatalf("InsertItems: %v", err)
	}

	items, total, err := repo.ListItems(ctx, runID, "", "", 1, 20)
	if err != nil || total != 1 || len(items) != 1 {
		t.Fatalf("ListItems = %d rows (total %d), %v", len(items), total, err)
	}
	if items[0].RevenueMinor == nil || *items[0].RevenueMinor != revenue || items[0].Status != "open" {
		t.Fatalf("item = %+v, want open with revenue 500", items[0])
	}
	// 只查 count 会漏掉「::date 加错参数」这类串列错误：模型名被当成日期时，
	// Postgres 要么直接报错，要么把可解析的模型名写进 usage_date。两个键值
	// 都必须原样往返。
	if items[0].UpstreamModel != item.UpstreamModel {
		t.Fatalf("upstream_model = %q, want %q", items[0].UpstreamModel, item.UpstreamModel)
	}
	if !items[0].UsageDate.Equal(item.UsageDate) {
		t.Fatalf("usage_date = %s, want %s", items[0].UsageDate.Format("2006-01-02"),
			item.UsageDate.Format("2006-01-02"))
	}
	if warnItems, _, err := repo.ListItems(ctx, runID, "warn", "", 1, 20); err != nil || len(warnItems) != 1 {
		t.Fatalf("severity filter = %d rows, %v; want 1", len(warnItems), err)
	}
	if alertItems, _, err := repo.ListItems(ctx, runID, "alert", "", 1, 20); err != nil || len(alertItems) != 0 {
		t.Fatalf("alert filter = %d rows, %v; want 0", len(alertItems), err)
	}

	if err := repo.AckItem(ctx, items[0].ID, "acknowledged", "checked"); err != nil {
		t.Fatalf("AckItem: %v", err)
	}
	acked, err := repo.GetItem(ctx, items[0].ID)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if acked.Status != "acknowledged" || acked.Note != "checked" {
		t.Fatalf("acked item = %+v", acked)
	}
	if err := repo.AckItem(ctx, 1<<62, "acknowledged", ""); !errors.Is(err, ErrItemNotFound) {
		t.Fatalf("ack missing err = %v, want ErrItemNotFound", err)
	}

	runs, runsTotal, err := repo.ListRuns(ctx, &account.ID, 1, 20)
	if err != nil || runsTotal != 1 || len(runs) != 1 || runs[0].WindowEnd.Sub(runs[0].WindowStart) != 24*time.Hour {
		t.Fatalf("ListRuns = %+v total %d, %v", runs, runsTotal, err)
	}
	if err := repo.FinishRun(ctx, runID, "succeeded", ""); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	if err := repo.FinishRun(ctx, 1<<62, "succeeded", ""); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("finish missing err = %v, want ErrRunNotFound", err)
	}

	days, err := repo.Summary(ctx, runID)
	if err != nil || len(days) != 1 {
		t.Fatalf("Summary = %+v, %v", days, err)
	}
	if days[0].ItemCount != 1 || days[0].WarnCount != 1 || days[0].AlertCount != 0 {
		t.Fatalf("summary day = %+v", days[0])
	}
	if days[0].RevenueMinor == nil || *days[0].RevenueMinor != revenue {
		t.Fatalf("summary revenue = %v, want %d", days[0].RevenueMinor, revenue)
	}
	if days[0].DeltaTokenBP != nil {
		t.Fatalf("delta_token_bp = %d, want nil (no snapshot dims)", *days[0].DeltaTokenBP)
	}
}

// TestAggregateAttributesLegacyRowsByChannel 覆盖日志库聚合：账号归属与
// 「无 upstream_account_id 时按 channel 归属」的 legacy 兜底。
func TestAggregateAttributesLegacyRowsByChannel(t *testing.T) {
	logsDB := openUpstreamReconcileLogsDB(t)
	repo := NewUsageAggregateRepo(logsDB)
	ctx := context.Background()

	now := time.Now().UTC()
	insert := func(channelID int64, accountID *int64, semantic, source int16, prompt, cacheRead, completion, cost, attempts int64) string {
		t.Helper()
		requestID := uuid.NewString()
		_, err := logsDB.Exec(`
			INSERT INTO usage_logs (request_id, user_id, channel_id, model,
				prompt_tokens, completion_tokens, cache_read_tokens, cost, status,
				upstream_model, upstream_account_id, usage_source, usage_semantic, attempts)
			VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, 1, $9, $10, $11, $12, $13)`,
			requestID, int64(1), channelID, "client-model",
			prompt, completion, cacheRead, cost,
			"upstream-M", accountID, source, semantic, attempts)
		if err != nil {
			t.Fatalf("insert usage_log: %v", err)
		}
		return requestID
	}

	accountID := int64(7)
	ids := []string{
		insert(50, &accountID, 0, 2, 100, 40, 10, 5, 1), // 直接归属账号 7
		insert(99, nil, 1, 1, 200, 20, 20, 7, 2),        // legacy：按渠道 99 归属
		insert(100, nil, 1, 1, 300, 0, 30, 11, 1),       // legacy：渠道 100，不属于账号 7
	}
	t.Cleanup(func() {
		for _, id := range ids {
			if _, err := logsDB.Exec(`DELETE FROM usage_logs WHERE request_id = $1::uuid`, id); err != nil {
				t.Errorf("cleanup usage_log %s: %v", id, err)
			}
		}
	})

	start, end := now.Add(-time.Hour), now.Add(time.Hour)
	rows, err := repo.Aggregate(ctx, accountID, []int64{99}, "Asia/Shanghai", start, end)
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d (%+v), want 1 (channel 100 must be excluded)", len(rows), rows)
	}
	got := rows[0]
	if got.Model != "upstream-M" {
		t.Fatalf("model = %q, want upstream-M", got.Model)
	}
	want := LocalUsage{
		UsageDate: got.UsageDate, Model: "upstream-M",
		Requests: 2, EstimatedRequests: 1, RefundedRequests: 0,
		CacheHitTokens: 60, CacheMissTokens: 260, OutputTokens: 30,
		WalletCredits: 0, SubscriptionCredits: 0, CostCredits: 12,
		UnknownRequests: 0, MultiAttemptRequests: 1,
	}
	if got != want {
		t.Fatalf("aggregate = %+v, want %+v", got, want)
	}

	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	localNow := now.In(loc)
	if y, m, d := got.UsageDate.Date(); y != localNow.Year() || m != localNow.Month() || d != localNow.Day() {
		t.Fatalf("usage_date = %s, want local date %s", got.UsageDate, localNow.Format("2006-01-02"))
	}

	latest, err := repo.LatestCreatedAt(ctx)
	if err != nil {
		t.Fatalf("LatestCreatedAt: %v", err)
	}
	if latest.Before(now.Add(-2*time.Minute)) || latest.After(now.Add(time.Minute)) {
		t.Fatalf("LatestCreatedAt = %s, want near %s", latest, now)
	}
}

func containsAccountID(items []UpstreamAccount, id int64) bool {
	for _, item := range items {
		if item.ID == id {
			return true
		}
	}
	return false
}

func reconcileStringPtr(s string) *string { return &s }
