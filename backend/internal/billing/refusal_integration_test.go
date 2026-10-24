//go:build integration

package billing

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"newtg/gateway/internal/contract"
)

// 本文件是中继前拒绝行（RecordRefusal）的真 PG 日志库验收。
//
//	NEWTG_TEST_LOGS_DSN='postgres://newtg:newtg@localhost:55432/newtg_logs?sslmode=disable' \
//	  go test -tags=integration ./internal/billing/ -run Refusal -count=1
//
// 未设置 NEWTG_TEST_LOGS_DSN 时回退到 NEWTG_TEST_DB_DSN 派生的 newtg_logs，
// 与 saturate_integration_test.go 的既有手法一致（见 mustLogsDSN）。

// openRefusalLogsTestDB 连接日志库（优先 NEWTG_TEST_LOGS_DSN）。
func openRefusalLogsTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("NEWTG_TEST_LOGS_DSN")
	if dsn == "" {
		dsn = mustLogsDSN(t)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open refusal logs db: %v", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		t.Fatalf("ping refusal logs db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	requireUsageLogsSchema(t, db)
	return db
}

// TestRefusalIntegrationIdempotentAndConverges 验证两件事：
//  1. 同一 request_id 写两次（不同 stage）只落一行，证据保留第一次；
//  2. refused 行可被后续 refunded 收敛改写（单向幂等），不能停在 refused。
func TestRefusalIntegrationIdempotentAndConverges(t *testing.T) {
	db := openRefusalLogsTestDB(t)
	ctx := context.Background()
	requestID := newTestUUID(t)
	cleanupUsageLog(t, db, requestID)
	logger := NewDBLogger(db)

	tokenID := int64(11)
	first := Refusal{
		RequestID:    requestID,
		UserID:       7,
		TokenID:      &tokenID,
		Model:        "gpt-4o",
		Endpoint:     "chat_completions",
		Stage:        RefusalStageAuth,
		ErrorClass:   "unauthorized",
		ErrorMessage: "网关在调用上游前拒绝了请求",
		Structure:    "valid",
	}
	if err := logger.RecordRefusal(ctx, first); err != nil {
		t.Fatalf("RecordRefusal(first): %v", err)
	}

	// 第二次换 stage/endpoint/structure：必须幂等命中，保留第一次的证据。
	second := first
	second.Stage = RefusalStageQuota
	second.Endpoint = "messages"
	second.Structure = "invalid"
	if err := logger.RecordRefusal(ctx, second); err != nil {
		t.Fatalf("RecordRefusal(second): %v", err)
	}

	if n := countUsageLog(t, db, requestID); n != 1 {
		t.Fatalf("重复写入后行数 = %d, want 1", n)
	}

	var (
		status                    int16
		preDeducted, cost, refund int64
		rowTokenID                sql.NullInt64
		retryChain                sql.NullString
		other                     []byte
		userID                    int64
		model                     string
	)
	if err := db.QueryRow(`
		SELECT status, pre_deducted, cost, refunded, token_id, retry_chain,
		       other::text, user_id, model
		  FROM usage_logs WHERE request_id = $1::uuid`, requestID).
		Scan(&status, &preDeducted, &cost, &refund, &rowTokenID, &retryChain,
			&other, &userID, &model); err != nil {
		t.Fatalf("read refusal row: %v", err)
	}
	if status != UsageLogStatusRefused {
		t.Fatalf("status = %d, want refused(%d)", status, UsageLogStatusRefused)
	}
	if preDeducted != 0 || cost != 0 || refund != 0 {
		t.Fatalf("amounts = %d/%d/%d, want 0/0/0", preDeducted, cost, refund)
	}
	if retryChain.Valid {
		t.Fatalf("retry_chain = %+v, want NULL（未到达上游）", retryChain)
	}
	if !rowTokenID.Valid || rowTokenID.Int64 != tokenID {
		t.Fatalf("token_id = %+v, want %d", rowTokenID, tokenID)
	}
	if userID != first.UserID || model != first.Model {
		t.Fatalf("user/model = %d/%s, want %d/%s", userID, model, first.UserID, first.Model)
	}

	var otherMap map[string]any
	if err := json.Unmarshal(other, &otherMap); err != nil {
		t.Fatalf("other 不是 JSON: %v (%s)", err, other)
	}
	admin, ok := otherMap["admin_info"].(map[string]any)
	if !ok {
		t.Fatalf("other.admin_info = %T, want object: %s", otherMap["admin_info"], other)
	}
	triage, ok := admin["triage"].(map[string]any)
	if !ok {
		t.Fatalf("admin_info.triage = %T, want object: %s", admin["triage"], other)
	}
	if got := triage["stage"]; got != RefusalStageAuth {
		t.Fatalf("triage.stage = %v, want %s（第一次写入必须胜出）", got, RefusalStageAuth)
	}
	if got := triage["endpoint"]; got != "chat_completions" {
		t.Fatalf("triage.endpoint = %v, want chat_completions", got)
	}
	if got := triage["request_structure"]; got != "valid" {
		t.Fatalf("triage.request_structure = %v, want valid", got)
	}

	// 单向幂等：worker 的 refunded 收敛必须改写 refused 行，不能停在 refused。
	rc := &contract.RelayContext{RequestID: requestID, UserID: 7}
	if err := logger.Saturate(ctx, nil, rc, SettlementOutcome{
		Status:       UsageLogStatusRefunded,
		ErrorClass:   "worker_refund",
		ErrorMessage: "abandoned",
	}); err != nil {
		t.Fatalf("Saturate(refunded): %v", err)
	}
	if err := db.QueryRow(`SELECT status FROM usage_logs WHERE request_id = $1::uuid`,
		requestID).Scan(&status); err != nil {
		t.Fatalf("read converged status: %v", err)
	}
	if status != UsageLogStatusRefunded {
		t.Fatalf("converged status = %d, want refunded(%d)（refused 不得成为终态）",
			status, UsageLogStatusRefunded)
	}
	if n := countUsageLog(t, db, requestID); n != 1 {
		t.Fatalf("收敛后行数 = %d, want 1", n)
	}
}

// TestRefusalIntegrationSharesAdvisoryLockWithSaturate 证明「拒绝行与结算行
// 共用同一把 request_id 作用域顾问锁」不是注释里的约定，而是可观察的行为。
//
// 手法：由测试事务手动持有该 request_id 的锁（与两条生产写入路径同一个
// SQL 表达式），再并发调用 RecordRefusal 与 Saturate——两者都必须阻塞；
// 释放后两者都完成，且该 request_id 仍只有一行。若任何一方换了锁键，
// 它会在持有期间直接穿过并写入第二行，本测试失败。
func TestRefusalIntegrationSharesAdvisoryLockWithSaturate(t *testing.T) {
	db := openRefusalLogsTestDB(t)
	ctx := context.Background()
	requestID := newTestUUID(t)
	cleanupUsageLog(t, db, requestID)
	logger := NewDBLogger(db)

	holder, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin holder tx: %v", err)
	}
	defer func() { _ = holder.Rollback() }()
	if err := lockUsageLogRequestID(ctx, holder, requestID); err != nil {
		t.Fatalf("hold advisory lock: %v", err)
	}

	refusalDone := make(chan error, 1)
	go func() {
		waitCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		refusalDone <- logger.RecordRefusal(waitCtx, Refusal{
			RequestID: requestID, UserID: 7, Stage: RefusalStageAuth,
		})
	}()

	satDone := make(chan error, 1)
	go func() {
		waitCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		rc := &contract.RelayContext{RequestID: requestID, UserID: 7}
		satDone <- logger.Saturate(waitCtx, nil, rc, SettlementOutcome{
			Status: UsageLogStatusRefunded, ErrorClass: "worker_refund", ErrorMessage: "abandoned",
		})
	}()

	// 锁被测试事务持有时，两条写入路径都不得完成。
	select {
	case err := <-refusalDone:
		t.Fatalf("RecordRefusal 没有等待顾问锁就返回: %v", err)
	case err := <-satDone:
		t.Fatalf("Saturate 没有等待顾问锁就返回: %v", err)
	case <-time.After(400 * time.Millisecond):
	}

	if err := holder.Commit(); err != nil {
		t.Fatalf("release advisory lock: %v", err)
	}
	for name, done := range map[string]chan error{"RecordRefusal": refusalDone, "Saturate": satDone} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("%s after release: %v", name, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s did not finish after the lock was released", name)
		}
	}

	if n := countUsageLog(t, db, requestID); n != 1 {
		t.Fatalf("行数 = %d, want 1（同一 request_id 的两条写入路径必须串行）", n)
	}
}
