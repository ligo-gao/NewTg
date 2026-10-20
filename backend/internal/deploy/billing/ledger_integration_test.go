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
			AmountPaid:           ionet.Money{Decimal: decimal.RequireFromString("7.2")},
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
			AmountPaid:              ionet.Money{Decimal: decimal.RequireFromString("7.2")},
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

// estimateFor 复制 flatRateEstimate 并换幂等键（同部署多段）。
func estimateFor(deploymentID, key string, started *time.Time) SegmentEstimate {
	est := flatRateEstimate(deploymentID, started)
	est.IdempotencyKey = key
	return est
}

func terminatedDetail(id string, started, finished time.Time, computeMinutes int, amountPaid string) ionet.DeploymentDetail {
	return ionet.DeploymentDetail{
		ID:                   id,
		Status:               "terminated",
		StartedAt:            &started,
		FinishedAt:           &finished,
		AmountPaid:           ionet.Money{Decimal: decimal.RequireFromString(amountPaid)},
		GPUsPerContainer:     1,
		TotalContainers:      1,
		HardwareID:           1,
		ComputeMinutesServed: computeMinutes,
		Locations:            []ionet.DeploymentLocation{{ID: 1}},
	}
}

// TestSettleCrossSegmentBaselineAndRegression 覆盖 M1/M6：
//   - 跨段基线相减（960 → 1560 确认 600，而不是把累计值整段重复计入）；
//   - provider 累计回退（1320 < 1560）时改走内部记账，不得记 0 且标 provider_exact。
func TestSettleCrossSegmentBaselineAndRegression(t *testing.T) {
	db := openBillingTestDB(t)
	resetBillingTables(t, db)
	start1 := time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC)
	now, setNow := testClock(start1.Add(30 * time.Minute))
	fake := &ionet.FakeProvider{DeploymentDetail: terminatedDetail(
		"dep-cross", start1, start1.Add(20*time.Minute), 16, "9.6")}
	provider := func(context.Context) (ionet.DeploymentProvider, bool, error) { return fake, true, nil }
	ledger := newTestLedger(t, db, now, provider, nil)

	seg1, _, err := ledger.Hold(context.Background(), estimateFor("dep-cross", "seg1", &start1))
	if err != nil {
		t.Fatalf("Hold seg1: %v", err)
	}
	if state, err := ledger.Reconcile(context.Background(), seg1.SegmentID); err != nil || state != SegmentSettled {
		t.Fatalf("Reconcile seg1 = (%q, %v)", state, err)
	}
	s1 := factsOfType(t, ledger, seg1.SegmentID, FactSettlement)
	if len(s1) != 1 || s1[0].GPUSeconds != 960 || s1[0].UsageSource != UsageProviderExact {
		t.Fatalf("seg1 settlement = %+v", s1)
	}

	start2 := start1.Add(time.Hour)
	setNow(start2.Add(30 * time.Minute))
	fake.DeploymentDetail = terminatedDetail("dep-cross", start2, start2.Add(26*time.Minute), 26, "9.6")
	seg2, _, err := ledger.Hold(context.Background(), estimateFor("dep-cross", "seg2", &start2))
	if err != nil {
		t.Fatalf("Hold seg2: %v", err)
	}
	if state, err := ledger.Reconcile(context.Background(), seg2.SegmentID); err != nil || state != SegmentSettled {
		t.Fatalf("Reconcile seg2 = (%q, %v)", state, err)
	}
	s2 := factsOfType(t, ledger, seg2.SegmentID, FactSettlement)
	if len(s2) != 1 || s2[0].GPUSeconds != 600 {
		t.Fatalf("seg2 settlement = %+v，want 600s（1560 − 960）", s2)
	}
	if s2[0].ProviderCumulativeSeconds == nil || *s2[0].ProviderCumulativeSeconds != 1560 {
		t.Fatalf("seg2 cumulative = %v，want 1560", s2[0].ProviderCumulativeSeconds)
	}

	// 第三段：provider 累计回退到 1320 < 1560。
	start3 := start2.Add(time.Hour)
	setNow(start3.Add(30 * time.Minute))
	fake.DeploymentDetail = terminatedDetail("dep-cross", start3, start3.Add(10*time.Minute), 22, "9.6")
	seg3, _, err := ledger.Hold(context.Background(), estimateFor("dep-cross", "seg3", &start3))
	if err != nil {
		t.Fatalf("Hold seg3: %v", err)
	}
	if state, err := ledger.Reconcile(context.Background(), seg3.SegmentID); err != nil || state != SegmentSettled {
		t.Fatalf("Reconcile seg3 = (%q, %v)", state, err)
	}
	s3 := factsOfType(t, ledger, seg3.SegmentID, FactSettlement)
	if len(s3) != 1 {
		t.Fatalf("seg3 settlements = %d", len(s3))
	}
	if s3[0].UsageSource != UsageInternalAccounting {
		t.Fatalf("seg3 usage_source = %q，want internal_accounting（不得标 provider_exact）", s3[0].UsageSource)
	}
	// 内部记账窗口 = started_at → finished_at = 10 分钟 = 600 GPU·秒。
	if s3[0].GPUSeconds != 600 {
		t.Fatalf("seg3 gpu_seconds = %d，want 600（不得记 0）", s3[0].GPUSeconds)
	}
	if s3[0].ProviderCumulativeSeconds != nil {
		t.Fatalf("回退段不得写入 provider 累计基线: %v", *s3[0].ProviderCumulativeSeconds)
	}
}

// TestSettleZeroComputeSecondsUsesInternalAccounting 覆盖 M6：
// provider 累计为 0（不可用）时按内部记账结算，不得当作免费。
func TestSettleZeroComputeSecondsUsesInternalAccounting(t *testing.T) {
	db := openBillingTestDB(t)
	resetBillingTables(t, db)
	start := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
	now, _ := testClock(start.Add(45 * time.Minute))
	fake := &ionet.FakeProvider{DeploymentDetail: terminatedDetail(
		"dep-zero", start, start.Add(30*time.Minute), 0, "7.2")}
	provider := func(context.Context) (ionet.DeploymentProvider, bool, error) { return fake, true, nil }
	ledger := newTestLedger(t, db, now, provider, nil)

	seg, _, err := ledger.Hold(context.Background(), flatRateEstimate("dep-zero", &start))
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if state, err := ledger.Reconcile(context.Background(), seg.SegmentID); err != nil || state != SegmentSettled {
		t.Fatalf("Reconcile = (%q, %v)", state, err)
	}
	settlements := factsOfType(t, ledger, seg.SegmentID, FactSettlement)
	if len(settlements) != 1 || settlements[0].GPUSeconds != 1800 ||
		settlements[0].UsageSource != UsageInternalAccounting {
		t.Fatalf("settlement = %+v，want 1800s / internal_accounting", settlements)
	}
}

// TestInvoiceCorrectionWritesNewAdjustment 覆盖 M5/M6：
// 同一周期更正发票必须写新调整（键含账单引用 + 序号），重复读同一张发票 no-op。
func TestInvoiceCorrectionWritesNewAdjustment(t *testing.T) {
	db := openBillingTestDB(t)
	resetBillingTables(t, db)
	start := time.Date(2026, 10, 27, 7, 0, 0, 0, time.UTC)
	now, _ := testClock(start.Add(2 * time.Hour))
	periodStart := start
	periodEnd := start.Add(2 * time.Hour)
	reader := &fakeInvoiceReader{invoice: Invoice{
		BillReference: "inv-001", AmountUSD: decimal.RequireFromString("8.64"),
		PeriodStart: &periodStart, PeriodEnd: &periodEnd,
	}}
	ledger := newTestLedger(t, db, now, nil, reader)
	seg, _, err := ledger.Hold(context.Background(), flatRateEstimate("dep-inv2", &start))
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if _, err := ledger.Settle(context.Background(), seg.SegmentID, GPUUsageFact{
		SegmentID: seg.SegmentID, DeploymentID: seg.DeploymentID, GPUSeconds: 7200,
		Source: UsageProviderExact,
	}); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if delta, err := ledger.ReconcileInvoice(context.Background(), seg.SegmentID); err != nil || delta != 144 {
		t.Fatalf("第一张发票 = (%d, %v)，want 144", delta, err)
	}
	// 同一张发票重复读：0 差额，不追加事实。
	if delta, err := ledger.ReconcileInvoice(context.Background(), seg.SegmentID); err != nil || delta != 0 {
		t.Fatalf("重复读发票 = (%d, %v)，want (0, nil)", delta, err)
	}
	if n := len(factsOfType(t, ledger, seg.SegmentID, FactReconciliationAdjustment)); n != 1 {
		t.Fatalf("adjustment facts = %d，want 1", n)
	}
	// 更正发票：同周期、新引用、新金额 → 新调整事实，不得静默丢弃。
	reader.invoice = Invoice{
		BillReference: "inv-001-rev", AmountUSD: decimal.RequireFromString("9.00"),
		PeriodStart: &periodStart, PeriodEnd: &periodEnd,
	}
	if delta, err := ledger.ReconcileInvoice(context.Background(), seg.SegmentID); err != nil || delta != 36 {
		t.Fatalf("更正发票 = (%d, %v)，want 36", delta, err)
	}
	facts := factsOfType(t, ledger, seg.SegmentID, FactReconciliationAdjustment)
	if len(facts) != 2 {
		t.Fatalf("adjustment facts = %d，want 2（更正不得覆盖/静默丢弃）", len(facts))
	}
	if facts[0].FactKey == facts[1].FactKey {
		t.Fatalf("更正发票复用了 fact_key: %q", facts[0].FactKey)
	}
	// 再读更正发票：0 差额。
	if delta, err := ledger.ReconcileInvoice(context.Background(), seg.SegmentID); err != nil || delta != 0 {
		t.Fatalf("重复读更正发票 = (%d, %v)", delta, err)
	}
	totals, err := ledger.CostTotals(context.Background(), "dep-inv2")
	if err != nil || totals.ConfirmedCost() != 900 {
		t.Fatalf("totals = %+v, %v，want confirmed 900", totals, err)
	}
}

// TestInvoiceNilPeriodAndMultiplePeriods 覆盖 M5：PeriodStart 为 nil 时逐次
// 调整不共用键；多个账单周期的调整相加而不是互相覆盖。
func TestInvoiceNilPeriodAndMultiplePeriods(t *testing.T) {
	db := openBillingTestDB(t)
	resetBillingTables(t, db)
	start := time.Date(2026, 10, 28, 6, 0, 0, 0, time.UTC)
	now, _ := testClock(start.Add(2 * time.Hour))
	reader := &fakeInvoiceReader{}
	ledger := newTestLedger(t, db, now, nil, reader)
	seg, _, err := ledger.Hold(context.Background(), flatRateEstimate("dep-inv3", &start))
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if _, err := ledger.Settle(context.Background(), seg.SegmentID, GPUUsageFact{
		SegmentID: seg.SegmentID, DeploymentID: seg.DeploymentID, GPUSeconds: 7200,
		Source: UsageProviderExact,
	}); err != nil {
		t.Fatalf("Settle: %v", err)
	}

	// nil 周期：两次不同引用必须各自落键。
	reader.invoice = Invoice{BillReference: "nil-1", AmountUSD: decimal.RequireFromString("8.00")}
	if delta, err := ledger.ReconcileInvoice(context.Background(), seg.SegmentID); err != nil || delta != 80 {
		t.Fatalf("nil 周期#1 = (%d, %v)，want 80", delta, err)
	}
	reader.invoice = Invoice{BillReference: "nil-2", AmountUSD: decimal.RequireFromString("8.50")}
	if delta, err := ledger.ReconcileInvoice(context.Background(), seg.SegmentID); err != nil || delta != 50 {
		t.Fatalf("nil 周期#2 = (%d, %v)，want 50", delta, err)
	}
	if n := len(factsOfType(t, ledger, seg.SegmentID, FactReconciliationAdjustment)); n != 2 {
		t.Fatalf("nil 周期 adjustment facts = %d，want 2（不得共用键）", n)
	}
	// 同一金额再读：0 差额。
	if delta, err := ledger.ReconcileInvoice(context.Background(), seg.SegmentID); err != nil || delta != 0 {
		t.Fatalf("nil 周期重复读 = (%d, %v)", delta, err)
	}

	// 两个不同账单周期：两条调整相加（900 + 100 = 1000），而不是后者覆盖前者。
	p1s, p1e := start, start.Add(time.Hour)
	p2s, p2e := start.Add(time.Hour), start.Add(2*time.Hour)
	reader.invoice = Invoice{
		BillReference: "cycle-1", AmountUSD: decimal.RequireFromString("9.00"),
		PeriodStart: &p1s, PeriodEnd: &p1e,
	}
	if _, err := ledger.ReconcileInvoice(context.Background(), seg.SegmentID); err != nil {
		t.Fatalf("cycle-1: %v", err)
	}
	reader.invoice = Invoice{
		BillReference: "cycle-2", AmountUSD: decimal.RequireFromString("1.00"),
		PeriodStart: &p2s, PeriodEnd: &p2e,
	}
	if delta, err := ledger.ReconcileInvoice(context.Background(), seg.SegmentID); err != nil || delta != 100 {
		t.Fatalf("cycle-2 delta = (%d, %v)，want 100", delta, err)
	}
	totals, err := ledger.CostTotals(context.Background(), "dep-inv3")
	if err != nil {
		t.Fatalf("CostTotals: %v", err)
	}
	// nil 周期两条（最后一条 850）+ cycle-1（900）+ cycle-2（100）= 1850。
	if totals.ConfirmedCost() != 1850 {
		t.Fatalf("confirmed cost = %d，want 1850（多周期/多引用相加）", totals.ConfirmedCost())
	}
}

// TestWorkerAccruesAllSegmentsAcrossPages 覆盖 M4：批大小小于段数时仍处理全部段。
func TestWorkerAccruesAllSegmentsAcrossPages(t *testing.T) {
	db := openBillingTestDB(t)
	resetBillingTables(t, db)
	start := time.Date(2026, 10, 29, 8, 0, 0, 0, time.UTC)
	now, _ := testClock(start.Add(2*time.Hour + 10*time.Minute))
	ledger := newTestLedger(t, db, now, nil, nil)
	segments := make([]Segment, 0, 5)
	for i := 0; i < 5; i++ {
		// 相同 started_at，靠 segment_id 做 keyset 兜底。
		est := estimateFor("dep-page", "page-seg-"+string(rune('a'+i)), &start)
		seg, _, err := ledger.Hold(context.Background(), est)
		if err != nil {
			t.Fatalf("Hold %d: %v", i, err)
		}
		segments = append(segments, seg)
	}
	provider := func(context.Context) (ionet.DeploymentProvider, bool, error) { return nil, false, nil }
	worker, err := NewWorker(WorkerDeps{Ledger: ledger, Provider: provider, Now: now, BatchSize: 2})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	stats, err := worker.AccrueOnce(context.Background())
	if err != nil {
		t.Fatalf("AccrueOnce: %v", err)
	}
	if stats.SegmentsAccrued != 5 {
		t.Fatalf("segments accrued = %d，want 5（不能只扫批大小内的段）", stats.SegmentsAccrued)
	}
	for _, seg := range segments {
		if n := len(factsOfType(t, ledger, seg.SegmentID, FactAccrual)); n != 2 {
			t.Fatalf("segment %s accrual facts = %d，want 2", seg.SegmentID, n)
		}
	}
}

// TestReconcileAuditCarriesRetryAfter 覆盖 LOW：provider 错误审计带
// error_code 与 retry_after_seconds。
func TestReconcileAuditCarriesRetryAfter(t *testing.T) {
	db := openBillingTestDB(t)
	resetBillingTables(t, db)
	start := time.Date(2026, 10, 30, 8, 0, 0, 0, time.UTC)
	now, _ := testClock(start.Add(10 * time.Minute))
	fake := &ionet.FakeProvider{
		DeploymentDetailErr: &ionet.APIError{StatusCode: 429, RetryAfter: 3 * time.Second, Detail: "slow down"},
	}
	provider := func(context.Context) (ionet.DeploymentProvider, bool, error) { return fake, true, nil }
	ledger := newTestLedger(t, db, now, provider, nil)
	var audited map[string]any
	ledger.audit = func(_ context.Context, action string, detail map[string]any) error {
		if action == AuditActionReconcile {
			audited = detail
		}
		return nil
	}
	seg, _, err := ledger.Hold(context.Background(), flatRateEstimate("dep-audit", &start))
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if _, err := ledger.Reconcile(context.Background(), seg.SegmentID); err == nil {
		t.Fatal("期望 provider 429 错误")
	}
	if audited == nil {
		t.Fatal("未写对账审计")
	}
	if audited["error_code"] != "deploy_rate_limited" {
		t.Fatalf("error_code = %v", audited["error_code"])
	}
	if audited["retry_after_seconds"] != int64(3) {
		t.Fatalf("retry_after_seconds = %v，want 3", audited["retry_after_seconds"])
	}
}

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
