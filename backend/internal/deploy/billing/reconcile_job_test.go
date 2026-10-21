//go:build integration

package billing

import (
	"context"
	"net/http"
	"testing"
	"time"

	"newtg/gateway/internal/deploy/ionet"
)

// 本文件验收 W3 H2：终止收敛（管理面确认删除）与部署级对账。
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_w3?sslmode=disable' \
//	  go test -tags=integration -p 1 ./internal/deploy/billing/ -count=1

// H2 Step 1：终止按实际用量结算，剩余预扣释放；重复收敛不重复记账。
func TestSettleTerminatedSettlesUsageAndReleasesRemainder(t *testing.T) {
	db := openBillingTestDB(t)
	resetBillingTables(t, db)
	start := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	now, setNow := testClock(start)
	ledger := newTestLedger(t, db, now, nil, nil)

	seg, created, err := ledger.Hold(context.Background(), flatRateEstimate("dep-term", &start))
	if err != nil || !created {
		t.Fatalf("Hold = (%+v, %v, %v)", seg, created, err)
	}
	if seg.PreconsumedCredits != 720 {
		t.Fatalf("preconsumed = %d，want 720", seg.PreconsumedCredits)
	}

	// 运行 1800s 后被删除：确认成本 = 1800 × 0.001 × 100 = 180。
	setNow(start.Add(30 * time.Minute))
	result, err := ledger.SettleTerminated(context.Background(), TerminationObservation{
		DeploymentID:   "dep-term",
		ComputeSeconds: 1800,
	})
	if err != nil {
		t.Fatalf("SettleTerminated: %v", err)
	}
	if result.SegmentsSettled != 1 || result.SegmentsReleased != 0 {
		t.Fatalf("result = %+v", result)
	}
	if result.ConfirmedCredits != 180 {
		t.Fatalf("confirmed credits = %d，want 180", result.ConfirmedCredits)
	}

	settled, err := ledger.Segment(context.Background(), seg.SegmentID)
	if err != nil {
		t.Fatalf("Segment: %v", err)
	}
	if settled.State != SegmentSettled || settled.SettledCredits == nil || *settled.SettledCredits != 180 {
		t.Fatalf("段未按用量结算: state=%s settled=%v", settled.State, settled.SettledCredits)
	}
	settlements := factsOfType(t, ledger, seg.SegmentID, FactSettlement)
	if len(settlements) != 1 {
		t.Fatalf("settlement 事实 = %d，want 1", len(settlements))
	}
	released := factsOfType(t, ledger, seg.SegmentID, FactReleased)
	if len(released) != 0 {
		t.Fatalf("已运行段不得再写 released 事实: %d", len(released))
	}

	// 重复收敛：终态短路，事实不重复。
	again, err := ledger.SettleTerminated(context.Background(), TerminationObservation{
		DeploymentID: "dep-term", ComputeSeconds: 1800,
	})
	if err != nil {
		t.Fatalf("repeat SettleTerminated: %v", err)
	}
	if again.SegmentsSettled != 0 {
		t.Fatalf("重复收敛不得再结算: %+v", again)
	}
	if got := len(factsOfType(t, ledger, seg.SegmentID, FactSettlement)); got != 1 {
		t.Fatalf("重复收敛后 settlement 事实 = %d，want 1", got)
	}
}

// H2 Step 1（从未运行）：pending 段在终止时释放全部预扣。
func TestSettleTerminatedReleasesNeverStartedSegment(t *testing.T) {
	db := openBillingTestDB(t)
	resetBillingTables(t, db)
	start := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	now, setNow := testClock(start)
	ledger := newTestLedger(t, db, now, nil, nil)

	// StartedAt=nil → pending_confirm（provider 从未确认启动）。
	seg, _, err := ledger.Hold(context.Background(), flatRateEstimate("dep-never", nil))
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	setNow(start.Add(10 * time.Minute))
	result, err := ledger.SettleTerminated(context.Background(), TerminationObservation{
		DeploymentID: "dep-never",
	})
	if err != nil {
		t.Fatalf("SettleTerminated: %v", err)
	}
	if result.SegmentsReleased != 1 || result.SegmentsSettled != 0 {
		t.Fatalf("result = %+v", result)
	}
	released := factsOfType(t, ledger, seg.SegmentID, FactReleased)
	if len(released) != 1 || released[0].ConfirmedCredits == nil || *released[0].ConfirmedCredits != 0 {
		t.Fatalf("released 事实 = %+v", released)
	}
}

// H2 Step 2：provider 已删除但段仍 open → 对账任务关闭段且不重复记账。
func TestReconcileDeploymentClosesDeletedDeploymentOnce(t *testing.T) {
	db := openBillingTestDB(t)
	resetBillingTables(t, db)
	start := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	now, setNow := testClock(start)
	// provider 明确 404（部署已删除）：这是对账窗口内唯一会推进到放弃的信号。
	provider := &ionet.FakeProvider{
		DeploymentDetailErr: &ionet.APIError{StatusCode: http.StatusNotFound},
	}
	ledger := newTestLedger(t, db, now, func(context.Context) (ionet.DeploymentProvider, bool, error) {
		return provider, true, nil
	}, nil)

	seg, _, err := ledger.Hold(context.Background(), flatRateEstimate("dep-gone", nil))
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}

	// 窗口内（900s 内）：保持 pending_confirm，不放弃。
	setNow(start.Add(5 * time.Minute))
	states, err := ledger.ReconcileDeployment(context.Background(), "dep-gone")
	if err != nil {
		t.Fatalf("ReconcileDeployment(window): %v", err)
	}
	if len(states) != 1 || states[0] != SegmentPendingConfirm {
		t.Fatalf("窗口内状态 = %v，want pending_confirm", states)
	}

	// 窗口后：provider 404 → 关闭段并释放，成本 0。
	setNow(start.Add(16 * time.Minute))
	states, err = ledger.ReconcileDeployment(context.Background(), "dep-gone")
	if err != nil {
		t.Fatalf("ReconcileDeployment: %v", err)
	}
	if len(states) != 1 || states[0] != SegmentAbandoned {
		t.Fatalf("窗口后状态 = %v，want abandoned", states)
	}
	released := factsOfType(t, ledger, seg.SegmentID, FactReleased)
	if len(released) != 1 {
		t.Fatalf("released 事实 = %d，want 1", len(released))
	}
	if got := len(factsOfType(t, ledger, seg.SegmentID, FactSettlement)); got != 0 {
		t.Fatalf("未证实成本不得结算: settlement 事实 = %d", got)
	}

	// 重复对账：终态短路，不重复记账。
	states, err = ledger.ReconcileDeployment(context.Background(), "dep-gone")
	if err != nil {
		t.Fatalf("repeat ReconcileDeployment: %v", err)
	}
	if len(states) != 0 {
		t.Fatalf("终态段不应再出现在未终态集合: %v", states)
	}
	if got := len(factsOfType(t, ledger, seg.SegmentID, FactReleased)); got != 1 {
		t.Fatalf("重复对账后 released 事实 = %d，want 1", got)
	}
}
