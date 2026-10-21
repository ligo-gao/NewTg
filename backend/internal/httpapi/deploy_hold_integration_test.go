//go:build integration

package httpapi

// W3 H1/H2 的数据库级验收：真实 PostgreSQL + 真实 DeploymentBillingLedger +
// fake provider。单测只能证明「接线被调用」，这里证明**真的落段/真的释放**，
// 并且复用生产账本（不在 httpapi 里另写一套记账）。
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_w3?sslmode=disable' \
//	  go test -tags=integration -p 1 ./internal/httpapi/ -run TestDeployBilling -v

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/deploy/billing"
	"newtg/gateway/internal/deploy/ionet"
	"newtg/gateway/internal/rbac"
)

// alwaysClaimIdemStore 每次 Claim 都视为抢到执行权：让同一 Idempotency-Key 的
// 两次请求都真正执行到 handler，从而验证**账本层**的幂等（而不是 HTTP 层重放
// 把第二次请求挡在门外）。
type alwaysClaimIdemStore struct {
	mu   sync.Mutex
	next int
}

func (s *alwaysClaimIdemStore) Claim(context.Context, string, time.Duration) (IdempotencyClaim, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	return IdempotencyClaim{Status: IdempotencyClaimAcquired, Token: "tok-" + time.Duration(s.next).String()}, nil
}
func (s *alwaysClaimIdemStore) Complete(context.Context, string, string, IdempotentResponse, time.Duration) error {
	return nil
}
func (s *alwaysClaimIdemStore) Release(context.Context, string, string) error { return nil }

type deployBillingEnv struct {
	mux      *http.ServeMux
	provider *ionet.FakeProvider
	db       *sql.DB
	ledger   *billing.Ledger
	audit    *fakeAuditExec
}

func newDeployBillingEnv(t *testing.T, now func() time.Time, idem IdempotencyStore) *deployBillingEnv {
	t.Helper()
	db := openDeployTestDB(t)
	if _, err := db.Exec(`TRUNCATE deployment_cost_facts, deployment_billing_segments RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate billing tables: %v", err)
	}
	ledger, err := billing.NewLedger(billing.LedgerConfig{
		DB:           db,
		Now:          now,
		QuotaPerUnit: func() int64 { return 100 },
	})
	if err != nil {
		t.Fatalf("NewLedger: %v", err)
	}
	service := &fakeSettingsService{getView: sampleDeployView(true, true)}
	provider := &ionet.FakeProvider{NameAvailable: true}
	secrets := &fakeDeploySecretStore{key: "stored-key", ok: true}
	exec := &fakeAuditExec{}
	handlers, err := NewDeployManagementHandlers(DeployManagementHandlerConfig{
		Settings: service,
		Secrets:  secrets,
		Provider: func(string) ionet.DeploymentProvider { return provider },
		Billing:  ledger,
		Authz:    rbac.NewAuthzMiddleware(),
		Audit:    audit.New(exec),
		Idem:     idem,
		Logger:   testLogger(),
		Now:      now,
	})
	if err != nil {
		t.Fatalf("NewDeployManagementHandlers: %v", err)
	}
	mux := http.NewServeMux()
	handlers.Register(mux)
	return &deployBillingEnv{mux: mux, provider: provider, db: db, ledger: ledger, audit: exec}
}

func (e *deployBillingEnv) do(method, target, body string, headers map[string]string) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	e.mux.ServeHTTP(rr, settingsRequest(method, target, body, deployManager(), headers))
	return rr
}

func (e *deployBillingEnv) segments(t *testing.T) []billing.Segment {
	t.Helper()
	rows, err := e.db.Query(`SELECT segment_id, deployment_id, segment_kind, idempotency_key, state,
		preconsumed_credits, settled_credits FROM deployment_billing_segments ORDER BY created_at`)
	if err != nil {
		t.Fatalf("query segments: %v", err)
	}
	defer rows.Close()
	var out []billing.Segment
	for rows.Next() {
		var seg billing.Segment
		if err := rows.Scan(&seg.SegmentID, &seg.DeploymentID, &seg.SegmentKind,
			&seg.IdempotencyKey, &seg.State, &seg.PreconsumedCredits, &seg.SettledCredits); err != nil {
			t.Fatalf("scan segment: %v", err)
		}
		out = append(out, seg)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("segments rows: %v", err)
	}
	return out
}

func (e *deployBillingEnv) factCount(t *testing.T, segmentID, factType string) int {
	t.Helper()
	var n int
	if err := e.db.QueryRow(`SELECT count(*) FROM deployment_cost_facts
		WHERE segment_id = $1 AND fact_type = $2`, segmentID, factType).Scan(&n); err != nil {
		t.Fatalf("count facts: %v", err)
	}
	return n
}

func deployBillingClock(initial time.Time) (func() time.Time, func(time.Time)) {
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

func billingCreateBody() string {
	return `{"name":"my-deploy","hardware_id":7,"location_ids":[1,2],"gpus_per_container":1,` +
		`"replica_count":2,"duration_hours":24,"image_url":"https://registry.example/img:tag"}`
}

func availablePrice() ionet.PriceEstimationResponse {
	return ionet.PriceEstimationResponse{
		EstimatedCost:    ionet.Money{Decimal: decimal.RequireFromString("10")},
		EstimatedCostSet: true, EstimationValid: true, Currency: "USDC",
	}
}

// H1 Step 1：创建成功产生 exactly 1 个 hold 段。
func TestDeployBillingCreateHoldsExactlyOneSegment(t *testing.T) {
	start := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	now, _ := deployBillingClock(start)
	env := newDeployBillingEnv(t, now, NewMemoryIdempotencyStore())
	env.provider.Price = availablePrice()
	env.provider.CreateResult = ionet.DeploymentAction{DeploymentID: "dep-new", Status: "requested"}

	rr := env.do(http.MethodPost, "/api/deployments", billingCreateBody(), deployWriteHeaders())
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d（body=%s）", rr.Code, rr.Body.String())
	}
	segs := env.segments(t)
	if len(segs) != 1 {
		t.Fatalf("段数 = %d，want 1", len(segs))
	}
	seg := segs[0]
	if seg.DeploymentID != "dep-new" || seg.SegmentKind != billing.SegmentKindCreate {
		t.Fatalf("段 = %+v", seg)
	}
	if seg.IdempotencyKey != "deploy:create:1:idem-key-1" {
		t.Fatalf("P-20 幂等键 = %q", seg.IdempotencyKey)
	}
	// 创建后 provider 尚未确认开始：段处于待确认，等对账转 running。
	if seg.State != billing.SegmentPendingConfirm {
		t.Fatalf("state = %s，want pending_confirm", seg.State)
	}
	if seg.PreconsumedCredits <= 0 {
		t.Fatalf("预扣必须为正: %d", seg.PreconsumedCredits)
	}
	if env.factCount(t, seg.SegmentID, billing.FactPreconsume) != 1 {
		t.Fatal("必须有且仅有一条 preconsume 事实")
	}
}

// H1 Step 1（失败路径）：provider 创建失败 → 零段（没有可释放的预扣）。
func TestDeployBillingCreateProviderFailureLeavesNoSegment(t *testing.T) {
	start := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	now, _ := deployBillingClock(start)
	env := newDeployBillingEnv(t, now, NewMemoryIdempotencyStore())
	env.provider.Price = availablePrice()
	env.provider.CreateErr = &ionet.APIError{StatusCode: http.StatusInternalServerError}

	rr := env.do(http.MethodPost, "/api/deployments", billingCreateBody(), deployWriteHeaders())
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d，期望 502（body=%s）", rr.Code, rr.Body.String())
	}
	if segs := env.segments(t); len(segs) != 0 {
		t.Fatalf("provider 创建失败不得留下段: %+v", segs)
	}
}

// H1 Step 2：同 clientRequestID 重放不产生第二段（账本层幂等）。
func TestDeployBillingCreateReplaySameClientRequestIDHoldsOnce(t *testing.T) {
	start := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	now, _ := deployBillingClock(start)
	env := newDeployBillingEnv(t, now, &alwaysClaimIdemStore{})
	env.provider.Price = availablePrice()
	env.provider.CreateResult = ionet.DeploymentAction{DeploymentID: "dep-new", Status: "requested"}

	for i := 0; i < 2; i++ {
		rr := env.do(http.MethodPost, "/api/deployments", billingCreateBody(), deployWriteHeaders())
		if rr.Code != http.StatusAccepted {
			t.Fatalf("第 %d 次 status = %d（body=%s）", i+1, rr.Code, rr.Body.String())
		}
	}
	segs := env.segments(t)
	if len(segs) != 1 {
		t.Fatalf("同键重放后段数 = %d，want 1", len(segs))
	}
	if env.factCount(t, segs[0].SegmentID, billing.FactPreconsume) != 1 {
		t.Fatal("同键重放不得重复预扣")
	}
}

// H1 Step 3：quote.Available=false → 422 quote_unavailable 且零段。
func TestDeployBillingCreateQuoteUnavailableRejects(t *testing.T) {
	start := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	now, _ := deployBillingClock(start)
	env := newDeployBillingEnv(t, now, NewMemoryIdempotencyStore())
	// 200 但没有报价字段：不是「免费」，必须拒绝。
	env.provider.Price = ionet.PriceEstimationResponse{EstimationValid: true, Currency: "USDC"}

	rr := env.do(http.MethodPost, "/api/deployments", billingCreateBody(), deployWriteHeaders())
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d，期望 422（body=%s）", rr.Code, rr.Body.String())
	}
	if got := decodeDeployProblem(t, rr)["type"]; got != problemTypeDeployQuoteUnavailable {
		t.Fatalf("problem.type = %v", got)
	}
	if env.provider.CreateCalls != 0 {
		t.Fatal("报价不可用不得发起 provider 写")
	}
	if segs := env.segments(t); len(segs) != 0 {
		t.Fatalf("报价不可用不得落段: %+v", segs)
	}
}

// H1：续期 provider 失败 → 新段释放（released 事实、成本 0），旧段不动。
func TestDeployBillingExtendProviderFailureReleasesSegment(t *testing.T) {
	start := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	now, _ := deployBillingClock(start)
	env := newDeployBillingEnv(t, now, NewMemoryIdempotencyStore())
	env.provider.Price = availablePrice()
	env.provider.DeploymentDetail = ionet.DeploymentDetail{
		ID: "dep-1", HardwareID: 7, GPUsPerContainer: 1, TotalContainers: 2,
		Locations: []ionet.DeploymentLocation{{ID: 1}},
	}
	env.provider.ExtendErr = &ionet.APIError{StatusCode: http.StatusInternalServerError}

	rr := env.do(http.MethodPost, "/api/deployments/dep-1/extend", `{"duration_hours":12}`, deployWriteHeaders())
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d（body=%s）", rr.Code, rr.Body.String())
	}
	segs := env.segments(t)
	if len(segs) != 1 {
		t.Fatalf("段数 = %d，want 1", len(segs))
	}
	if segs[0].State != billing.SegmentAbandoned {
		t.Fatalf("续期失败段 state = %s，want abandoned", segs[0].State)
	}
	if env.factCount(t, segs[0].SegmentID, billing.FactReleased) != 1 {
		t.Fatal("续期失败必须写 released 事实")
	}
	if env.factCount(t, segs[0].SegmentID, billing.FactSettlement) != 0 {
		t.Fatal("从未运行的段不得结算")
	}
}

// H2：终止按实际用量结算，剩余预扣释放；重复删除不重复记账。
func TestDeployBillingTerminationSettlesAndReleases(t *testing.T) {
	start := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	now, setNow := deployBillingClock(start)
	env := newDeployBillingEnv(t, now, NewMemoryIdempotencyStore())
	env.provider.Price = availablePrice()
	env.provider.CreateResult = ionet.DeploymentAction{DeploymentID: "dep-run", Status: "requested"}
	env.provider.DeleteResult = ionet.DeploymentAction{DeploymentID: "dep-run", Status: "termination requested"}

	if rr := env.do(http.MethodPost, "/api/deployments", billingCreateBody(), deployWriteHeaders()); rr.Code != http.StatusAccepted {
		t.Fatalf("create status = %d（body=%s）", rr.Code, rr.Body.String())
	}
	segs := env.segments(t)
	if len(segs) != 1 {
		t.Fatalf("段数 = %d", len(segs))
	}
	segmentID := segs[0].SegmentID

	// 对账把段从 pending_confirm 推进到 running（provider 已开始）。
	setNow(start.Add(5 * time.Minute))
	env.provider.DeploymentDetail = ionet.DeploymentDetail{
		ID: "dep-run", Status: "running", HardwareID: 7, GPUsPerContainer: 1, TotalContainers: 2,
		StartedAt:            deployTimePtr(start),
		Locations:            []ionet.DeploymentLocation{{ID: 1}},
		ComputeMinutesServed: 0,
	}
	if _, err := env.ledger.ReconcileDeployment(context.Background(), "dep-run"); err != nil {
		t.Fatalf("ReconcileDeployment: %v", err)
	}

	// 运行 30 分钟后删除：provider 报告 30 分钟用量 → 按用量结算。
	setNow(start.Add(30 * time.Minute))
	env.provider.DeploymentDetail.ComputeMinutesServed = 30
	if rr := env.do(http.MethodDelete, "/api/deployments/dep-run", "", deployWriteHeaders()); rr.Code != http.StatusAccepted {
		t.Fatalf("delete status = %d（body=%s）", rr.Code, rr.Body.String())
	}

	settled := env.segments(t)[0]
	if settled.State != billing.SegmentSettled {
		t.Fatalf("终止后 state = %s，want settled", settled.State)
	}
	if settled.SettledCredits == nil || *settled.SettledCredits <= 0 {
		t.Fatalf("确认成本必须为正: %v", settled.SettledCredits)
	}
	if *settled.SettledCredits >= settled.PreconsumedCredits {
		t.Fatalf("提前终止应释放剩余预扣: confirmed=%d preconsumed=%d",
			*settled.SettledCredits, settled.PreconsumedCredits)
	}
	if env.factCount(t, segmentID, billing.FactSettlement) != 1 {
		t.Fatal("必须恰有一条 settlement 事实")
	}

	// 重复收敛（另一轮删除/对账）：终态短路，不重复记账。
	if _, err := env.ledger.SettleTerminated(context.Background(), billing.TerminationObservation{
		DeploymentID: "dep-run", ComputeSeconds: 1800,
	}); err != nil {
		t.Fatalf("repeat SettleTerminated: %v", err)
	}
	if env.factCount(t, segmentID, billing.FactSettlement) != 1 {
		t.Fatal("重复收敛不得重复写 settlement")
	}
}
