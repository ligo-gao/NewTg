//go:build integration

package billing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/shopspring/decimal"

	"newtg/gateway/internal/deploy/ionet"
)

// 本文件是 C2 / Task 5c 的集成验收（真 PG + fake provider）。
//
// 运行（隔离库，先应用全部迁移）：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_c2?sslmode=disable' \
//	  go test -tags=integration ./internal/deploy/billing/ -count=1 -v
//
// 覆盖：小时切分（跨整点/不足一小时）、重复运行不双计、部署终止闭合段、
// 重启补算、成本汇总、迁移 up/down。

func openBillingTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("NEWTG_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("NEWTG_TEST_DB_DSN 未设置，跳过集成测试")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("ping test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func resetBillingTables(t *testing.T, db *sql.DB) {
	t.Helper()
	var exists bool
	if err := db.QueryRow(`SELECT to_regclass('public.deployment_billing_segments') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatalf("check billing tables: %v", err)
	}
	if !exists {
		t.Fatalf("deployment_billing_segments 不存在：先对隔离库应用迁移（platform-04 §11.2）")
	}
	if _, err := db.Exec(`TRUNCATE deployment_cost_facts, deployment_billing_segments RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate billing tables: %v", err)
	}
}

func testClock(initial time.Time) (func() time.Time, func(time.Time)) {
	var mu sync.Mutex
	now := initial
	return func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			return now
		}, func(v time.Time) {
			mu.Lock()
			defer mu.Unlock()
			now = v
		}
}

func flatRateEstimate(deploymentID string, started *time.Time) SegmentEstimate {
	return SegmentEstimate{
		DeploymentID:             deploymentID,
		IdempotencyKey:           "test:" + deploymentID,
		SegmentKind:              SegmentKindCreate,
		Name:                     deploymentID,
		HardwareID:               1,
		LocationIDs:              []int64{1},
		GPUsPerContainer:         1,
		ReplicaCount:             1,
		PlannedSeconds:           7200,
		StartedAt:                started,
		QuoteUSD:                 decimal.NewFromFloat(7.2),
		UnitPriceUSDPerGPUSecond: decimal.NewFromFloat(0.001),
		PriceSource:              PriceSourceProviderQuote,
	}
}

func newTestLedger(t *testing.T, db *sql.DB, now func() time.Time, provider ProviderFunc, invoice InvoiceReader) *Ledger {
	t.Helper()
	ledger, err := NewLedger(LedgerConfig{
		DB:           db,
		Now:          now,
		QuotaPerUnit: func() int64 { return 100 },
		Provider:     provider,
		Invoice:      invoice,
	})
	if err != nil {
		t.Fatalf("NewLedger: %v", err)
	}
	return ledger
}

func factsOfType(t *testing.T, ledger *Ledger, segmentID, factType string) []CostFact {
	t.Helper()
	facts, err := ledger.Facts(context.Background(), segmentID)
	if err != nil {
		t.Fatalf("Facts: %v", err)
	}
	var out []CostFact
	for _, f := range facts {
		if f.FactType == factType {
			out = append(out, f)
		}
	}
	return out
}

// TestAccrualHourSplitAndIdempotency 覆盖小时切分、重复运行、重启补算与结算。
func TestAccrualHourSplitAndIdempotency(t *testing.T) {
	db := openBillingTestDB(t)
	resetBillingTables(t, db)
	start := time.Date(2026, 10, 20, 10, 30, 0, 0, time.UTC)
	now, setNow := testClock(start)
	ledger := newTestLedger(t, db, now, nil, nil)

	seg, created, err := ledger.Hold(context.Background(), flatRateEstimate("dep-hour", &start))
	if err != nil || !created {
		t.Fatalf("Hold = (%+v, %v, %v)", seg, created, err)
	}
	if seg.PreconsumedCredits != 720 {
		t.Fatalf("preconsumed = %d，want 720（7200s × 0.001 × 100）", seg.PreconsumedCredits)
	}

	// 11:10：只写已闭合的 10:30–11:00（1800s）。
	setNow(time.Date(2026, 10, 20, 11, 10, 0, 0, time.UTC))
	if n, err := ledger.Accrue(context.Background(), seg.SegmentID); err != nil || n != 1 {
		t.Fatalf("Accrue(11:10) = (%d, %v)，want (1, nil)", n, err)
	}
	// 同一时刻重复运行：不双计。
	if n, err := ledger.Accrue(context.Background(), seg.SegmentID); err != nil || n != 0 {
		t.Fatalf("Accrue repeat = (%d, %v)，want (0, nil)", n, err)
	}

	// 模拟进程重启：新 Ledger 在同一 DB 上补算缺失的 11:00–12:00。
	setNow(time.Date(2026, 10, 20, 12, 20, 0, 0, time.UTC))
	restarted := newTestLedger(t, db, now, nil, nil)
	if n, err := restarted.Accrue(context.Background(), seg.SegmentID); err != nil || n != 1 {
		t.Fatalf("restart Accrue(12:20) = (%d, %v)，want (1, nil)", n, err)
	}
	// 当前小时未闭合：不写。
	if n, err := restarted.Accrue(context.Background(), seg.SegmentID); err != nil || n != 0 {
		t.Fatalf("open-hour Accrue = (%d, %v)，want (0, nil)", n, err)
	}

	accruals := factsOfType(t, restarted, seg.SegmentID, FactAccrual)
	if len(accruals) != 2 {
		t.Fatalf("accrual facts = %d，want 2", len(accruals))
	}
	wantStart := []time.Time{
		time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 20, 11, 0, 0, 0, time.UTC),
	}
	wantSeconds := []int64{1800, 3600}
	for i, f := range accruals {
		if f.PeriodStart == nil || !f.PeriodStart.Equal(wantStart[i]) {
			t.Fatalf("accrual[%d].period_start = %v，want %v", i, f.PeriodStart, wantStart[i])
		}
		if f.GPUSeconds != wantSeconds[i] {
			t.Fatalf("accrual[%d].gpu_seconds = %d，want %d", i, f.GPUSeconds, wantSeconds[i])
		}
	}
	if accruals[0].EstimatedCredits != 180 || accruals[1].EstimatedCredits != 360 {
		t.Fatalf("accrual credits = [%d %d]，want [180 360]",
			accruals[0].EstimatedCredits, accruals[1].EstimatedCredits)
	}

	// 12:35 终止：补写 12:00–12:35 的开放小时，再写 settlement。
	// 7500s = planned 7200 + grace 300；确认成本 750，预扣 720，少补 30。
	setNow(time.Date(2026, 10, 20, 12, 35, 0, 0, time.UTC))
	delta, err := restarted.Settle(context.Background(), seg.SegmentID, GPUUsageFact{
		SegmentID:    seg.SegmentID,
		DeploymentID: seg.DeploymentID,
		GPUSeconds:   7500,
		Source:       UsageProviderExact,
	})
	if err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if delta != 30 {
		t.Fatalf("delta = %d，want 30", delta)
	}
	settled, err := restarted.Segment(context.Background(), seg.SegmentID)
	if err != nil {
		t.Fatalf("Segment: %v", err)
	}
	if settled.State != SegmentSettled || settled.SettledCredits == nil || *settled.SettledCredits != 750 {
		t.Fatalf("settled segment = %+v", settled)
	}
	// 重复结算 no-op，不新增事实。
	before := len(factsOfType(t, restarted, seg.SegmentID, FactSettlement))
	if _, err := restarted.Settle(context.Background(), seg.SegmentID, GPUUsageFact{
		SegmentID: seg.SegmentID, DeploymentID: seg.DeploymentID, GPUSeconds: 7500, Source: UsageProviderExact,
	}); err != nil {
		t.Fatalf("repeat Settle: %v", err)
	}
	if after := len(factsOfType(t, restarted, seg.SegmentID, FactSettlement)); after != before {
		t.Fatalf("settlement facts %d → %d，重复结算必须 no-op", before, after)
	}
	// 最终三条 accrual：1800 + 3600 + 2100 = 7500s。
	accruals = factsOfType(t, restarted, seg.SegmentID, FactAccrual)
	if len(accruals) != 3 {
		t.Fatalf("final accrual facts = %d，want 3", len(accruals))
	}
	var total int64
	for _, f := range accruals {
		total += f.GPUSeconds
	}
	if total != 7500 {
		t.Fatalf("accrued gpu_seconds = %d，want 7500", total)
	}

	totals, err := restarted.CostTotals(context.Background(), "dep-hour")
	if err != nil {
		t.Fatalf("CostTotals: %v", err)
	}
	if totals.PreconsumeCredits != 720 || totals.AccrualCredits != 750 ||
		totals.SettlementCredits != 750 || totals.ConfirmedCost() != 750 {
		t.Fatalf("cost totals = %+v", totals)
	}
}

// TestReconcileTerminationClosesSegment 覆盖「部署终止闭合 segment」：
// provider 终值（部署级累计秒）按段基线换算成段内 GPU·秒并结算。
func TestReconcileTerminationClosesSegment(t *testing.T) {
	db := openBillingTestDB(t)
	resetBillingTables(t, db)
	start := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	now, setNow := testClock(start.Add(45 * time.Minute))
	fake := &ionet.FakeProvider{
		DeploymentDetail: ionet.DeploymentDetail{
			ID:                   "dep-term",
			Status:               "terminated",
			StartedAt:            &start,
			FinishedAt:           timePtr(start.Add(40 * time.Minute)),
			AmountPaid:           7.2,
			GPUsPerContainer:     1,
			TotalContainers:      1,
			HardwareID:           1,
			ComputeMinutesServed: 40,
			Locations:            []ionet.DeploymentLocation{{ID: 1}},
		},
	}
	provider := func(context.Context) (ionet.DeploymentProvider, bool, error) { return fake, true, nil }
	ledger := newTestLedger(t, db, now, provider, nil)

	seg, created, err := ledger.Hold(context.Background(), flatRateEstimate("dep-term", &start))
	if err != nil || !created {
		t.Fatalf("Hold: %v %v", created, err)
	}
	state, err := ledger.Reconcile(context.Background(), seg.SegmentID)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if state != SegmentSettled {
		t.Fatalf("reconcile state = %q，want settled", state)
	}
	settlements := factsOfType(t, ledger, seg.SegmentID, FactSettlement)
	if len(settlements) != 1 {
		t.Fatalf("settlement facts = %d，want 1", len(settlements))
	}
	// 40 分钟 × 60s × 1 GPU × 1 replica = 2400 GPU·秒 → 240 积分。
	if settlements[0].GPUSeconds != 2400 || settlements[0].ConfirmedCredits == nil ||
		*settlements[0].ConfirmedCredits != 240 {
		t.Fatalf("settlement = %+v", settlements[0])
	}
	if settlements[0].UsageSource != UsageProviderExact {
		t.Fatalf("usage source = %q，want provider_exact", settlements[0].UsageSource)
	}
	// 重复对账 no-op。
	setNow(start.Add(2 * time.Hour))
	if state, err = ledger.Reconcile(context.Background(), seg.SegmentID); err != nil || state != SegmentSettled {
		t.Fatalf("repeat Reconcile = (%q, %v)", state, err)
	}
	if n := len(factsOfType(t, ledger, seg.SegmentID, FactSettlement)); n != 1 {
		t.Fatalf("settlement facts after repeat = %d，want 1", n)
	}
}

// TestReconcileProviderMissingHonoursWindow 覆盖 900s 放弃窗口：
// provider 可达且明确 404 时，窗口内保持状态，窗口后 abandoned + released。
func TestReconcileProviderMissingHonoursWindow(t *testing.T) {
	db := openBillingTestDB(t)
	resetBillingTables(t, db)
	start := time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC)
	now, setNow := testClock(start)
	fake := &ionet.FakeProvider{
		DeploymentDetailErr: &ionet.APIError{StatusCode: 404, Detail: "not found"},
	}
	provider := func(context.Context) (ionet.DeploymentProvider, bool, error) { return fake, true, nil }
	ledger := newTestLedger(t, db, now, provider, nil)

	seg, created, err := ledger.Hold(context.Background(), flatRateEstimate("dep-missing", nil))
	if err != nil || !created {
		t.Fatalf("Hold: %v %v", created, err)
	}
	if seg.State != SegmentPendingConfirm {
		t.Fatalf("state = %q，want pending_confirm", seg.State)
	}
	// 窗口内（100s）：保持 pending_confirm。
	setNow(start.Add(100 * time.Second))
	if state, err := ledger.Reconcile(context.Background(), seg.SegmentID); err != nil || state != SegmentPendingConfirm {
		t.Fatalf("Reconcile(100s) = (%q, %v)", state, err)
	}
	// 窗口后（901s）：abandoned 并写 released 事实。
	setNow(start.Add(901 * time.Second))
	if state, err := ledger.Reconcile(context.Background(), seg.SegmentID); err != nil || state != SegmentAbandoned {
		t.Fatalf("Reconcile(901s) = (%q, %v)", state, err)
	}
	released := factsOfType(t, ledger, seg.SegmentID, FactReleased)
	if len(released) != 1 || released[0].ConfirmedCredits == nil || *released[0].ConfirmedCredits != 0 {
		t.Fatalf("released facts = %+v", released)
	}
}

// fakeInvoiceReader 是账单接缝的测试替身（ionet FakeProvider 没有账单能力）。
type fakeInvoiceReader struct {
	invoice Invoice
	err     error
	calls   int
}

func (f *fakeInvoiceReader) Invoice(context.Context, string) (Invoice, error) {
	f.calls++
	return f.invoice, f.err
}

// TestInvoiceReconciliationAdjustsCost 覆盖成本汇总与发票差异（绝对重述）。
func TestInvoiceReconciliationAdjustsCost(t *testing.T) {
	db := openBillingTestDB(t)
	resetBillingTables(t, db)
	start := time.Date(2026, 10, 23, 7, 0, 0, 0, time.UTC)
	now, _ := testClock(start.Add(2 * time.Hour))
	invoice := &fakeInvoiceReader{invoice: Invoice{
		BillReference: "inv-001",
		AmountUSD:     decimal.NewFromFloat(8.64),
		PeriodStart:   timePtr(time.Date(2026, 10, 23, 7, 0, 0, 0, time.UTC)),
		PeriodEnd:     timePtr(time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)),
	}}
	ledger := newTestLedger(t, db, now, nil, invoice)
	seg, _, err := ledger.Hold(context.Background(), flatRateEstimate("dep-inv", &start))
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if _, err := ledger.Settle(context.Background(), seg.SegmentID, GPUUsageFact{
		SegmentID: seg.SegmentID, DeploymentID: seg.DeploymentID, GPUSeconds: 7200, Source: UsageProviderExact,
	}); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	delta, err := ledger.ReconcileInvoice(context.Background(), seg.SegmentID)
	if err != nil {
		t.Fatalf("ReconcileInvoice: %v", err)
	}
	if delta != 144 { // 8.64 USD × 100 = 864；已确认 720
		t.Fatalf("invoice delta = %d，want 144", delta)
	}
	totals, err := ledger.CostTotals(context.Background(), "dep-inv")
	if err != nil {
		t.Fatalf("CostTotals: %v", err)
	}
	if totals.AdjustmentCredits != 864 || totals.ConfirmedCost() != 864 {
		t.Fatalf("totals after adjustment = %+v（调整是绝对重述，不能与 settlement 相加）", totals)
	}
	// 同一账单周期重复对账：no-op。
	if delta, err := ledger.ReconcileInvoice(context.Background(), seg.SegmentID); err != nil || delta != 0 {
		t.Fatalf("repeat ReconcileInvoice = (%d, %v)", delta, err)
	}
	if n := len(factsOfType(t, ledger, seg.SegmentID, FactReconciliationAdjustment)); n != 1 {
		t.Fatalf("adjustment facts = %d，want 1", n)
	}
}

// TestWorkerDiscoversRunningDeployment 覆盖 worker 的「运行中部署 → 幂等段 →
// 周期记账」路径：同一 provider 快照重复扫描不会二次预扣/双计。
func TestWorkerDiscoversRunningDeployment(t *testing.T) {
	db := openBillingTestDB(t)
	resetBillingTables(t, db)
	start := time.Date(2026, 10, 24, 6, 0, 0, 0, time.UTC)
	now, _ := testClock(start.Add(2*time.Hour + 10*time.Minute))
	fake := &ionet.FakeProvider{
		Deployments: []ionet.DeploymentSummary{{ID: "dep-sync", Name: "dep-sync", Status: "running"}},
		DeploymentDetail: ionet.DeploymentDetail{
			ID:                      "dep-sync",
			Status:                  "running",
			StartedAt:               &start,
			AmountPaid:              7.2,
			GPUsPerContainer:        1,
			TotalContainers:         1,
			HardwareID:              1,
			ComputeMinutesServed:    120,
			ComputeMinutesRemaining: 120,
			Locations:               []ionet.DeploymentLocation{{ID: 1}},
		},
	}
	provider := func(context.Context) (ionet.DeploymentProvider, bool, error) { return fake, true, nil }
	ledger := newTestLedger(t, db, now, provider, nil)
	worker, err := NewWorker(WorkerDeps{Ledger: ledger, Provider: provider, Now: now})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	stats, err := worker.AccrueOnce(context.Background())
	if err != nil {
		t.Fatalf("AccrueOnce: %v", err)
	}
	if stats.DeploymentsSeen != 1 || stats.SegmentsCreated != 1 || stats.Errors != 0 {
		t.Fatalf("stats = %+v，want seen=1 created=1 errors=0", stats)
	}
	seg, err := ledger.ActiveSegmentForDeployment(context.Background(), "dep-sync")
	if err != nil {
		t.Fatalf("ActiveSegmentForDeployment: %v", err)
	}
	if seg.State != SegmentRunning || seg.StartedAt == nil || !seg.StartedAt.Equal(start) {
		t.Fatalf("segment = %+v", seg)
	}
	// 建段时已立即记账：2 个已闭合小时 + 1 条预扣。
	if n := len(factsOfType(t, ledger, seg.SegmentID, FactAccrual)); n != 2 {
		t.Fatalf("accrual facts = %d，want 2", n)
	}
	// 重复扫描：同一 Idempotency-Key / fact_key，不再预扣、不再双计。
	stats, err = worker.AccrueOnce(context.Background())
	if err != nil {
		t.Fatalf("repeat AccrueOnce: %v", err)
	}
	if stats.SegmentsCreated != 0 || stats.FactsWritten != 0 {
		t.Fatalf("repeat stats = %+v，want created=0 facts=0", stats)
	}
	if n := len(factsOfType(t, ledger, seg.SegmentID, FactAccrual)); n != 2 {
		t.Fatalf("accrual facts after repeat = %d，want 2", n)
	}
	pre := factsOfType(t, ledger, seg.SegmentID, FactPreconsume)
	if len(pre) != 1 {
		t.Fatalf("preconsume facts = %d，want 1", len(pre))
	}
}

// TestDeploymentBillingMigrationUpDown 在事务内验证成对迁移：
// down → up → up（幂等）→ down；最后 rollback 不影响已迁移的测试库。
func TestDeploymentBillingMigrationUpDown(t *testing.T) {
	db := openBillingTestDB(t)
	upSQL := readMigration(t, "20261020270000_deployment_billing.up.sql")
	downSQL := readMigration(t, "20261020270000_deployment_billing.down.sql")
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	exec := func(label, script string) {
		t.Helper()
		if _, err := tx.Exec(script); err != nil {
			t.Fatalf("%s: %v", label, err)
		}
	}
	exec("down (clean slate)", downSQL)
	exec("up", upSQL)
	exec("up twice is no-op", upSQL)

	assertColumns := func(want map[string]int) {
		t.Helper()
		for table, columns := range want {
			var n int
			if err := tx.QueryRow(`SELECT count(*) FROM information_schema.columns
				WHERE table_schema = current_schema() AND table_name = $1`, table).Scan(&n); err != nil {
				t.Fatalf("count columns %s: %v", table, err)
			}
			if n != columns {
				t.Fatalf("%s 列数 = %d，want %d（platform-01 §22.13）", table, n, columns)
			}
		}
	}
	assertColumns(map[string]int{
		"deployment_billing_segments": 24,
		"deployment_cost_facts":       19,
	})

	// 幂等键与索引必须真实存在。
	var indexes int
	if err := tx.QueryRow(`SELECT count(*) FROM pg_indexes
		WHERE schemaname = current_schema() AND indexname IN (
			'deployment_billing_segments_idempotency_uq',
			'deployment_billing_segments_deployment_state_idx',
			'deployment_billing_segments_heartbeat_idx',
			'deployment_cost_facts_fact_key_uq',
			'deployment_cost_facts_deployment_created_idx',
			'deployment_cost_facts_segment_type_idx')`).Scan(&indexes); err != nil {
		t.Fatalf("count indexes: %v", err)
	}
	if indexes != 6 {
		t.Fatalf("billings indexes = %d，want 6", indexes)
	}

	exec("down (reverse order)", downSQL)
	for _, table := range []string{"deployment_cost_facts", "deployment_billing_segments"} {
		var exists bool
		if err := tx.QueryRow(`SELECT to_regclass($1) IS NOT NULL`, "public."+table).Scan(&exists); err != nil {
			t.Fatalf("check %s: %v", table, err)
		}
		if exists {
			t.Fatalf("%s 仍存在，down 未清理", table)
		}
	}
}

func readMigration(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join("..", "..", "..", "migrations", name)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read migration %s: %v", path, err)
	}
	body := strings.TrimSpace(string(raw))
	if body == "" {
		t.Fatalf("migration %s is empty", name)
	}
	return body
}

func timePtr(v time.Time) *time.Time { return &v }

// TestProviderFromSettings 锁定冻结键名与解封行为（无需 DB）。
func TestProviderFromSettings(t *testing.T) {
	settings := fakeSettings{
		DeployEnabledKey: []byte(`true`),
		DeployAPIKeyKey:  []byte(`"sealed-key"`),
	}
	opener := fakeOpener{}
	provider, ok, err := ProviderFromSettings(settings, &opener, nil)(context.Background())
	if err != nil || !ok || provider == nil {
		t.Fatalf("ProviderFromSettings = (%v, %v, %v)", provider, ok, err)
	}
	if opener.last != "sealed-key" {
		t.Fatalf("sealed value = %q", opener.last)
	}
	settings[DeployEnabledKey] = []byte(`false`)
	if _, ok, err := ProviderFromSettings(settings, &opener, nil)(context.Background()); err != nil || ok {
		t.Fatalf("disabled = (%v, %v)", ok, err)
	}
}

type fakeSettings map[string]json.RawMessage

func (f fakeSettings) Get(key string) (json.RawMessage, bool) {
	v, ok := f[key]
	return v, ok
}

type fakeOpener struct{ last string }

func (f *fakeOpener) Open(sealed string) (string, error) {
	f.last = sealed
	if sealed == "" {
		return "", errors.New("empty")
	}
	return "api-key", nil
}
