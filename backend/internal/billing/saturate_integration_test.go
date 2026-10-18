//go:build integration

package billing

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// 本文件是 usage_logs 落库与月分区的集成验收（真 PG 日志库）。
//
// 日志库与主库分离：取 NEWTG_TEST_DB_DSN 并把 dbname 换成 newtg_logs
// （计划冻结的测试手法，与 internal/store 的日志 schema 测试一致）。
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_test?sslmode=disable' \
//	  go test -tags=integration ./internal/billing/ -count=1

// openLogsTestDB 连接日志库（newtg_logs）。
func openLogsTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("NEWTG_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("NEWTG_TEST_DB_DSN 未设置，跳过计费日志集成测试")
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Scheme == "" {
		t.Fatalf("解析 NEWTG_TEST_DB_DSN 失败: %v", err)
	}
	u.Path = "/newtg_logs"
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatalf("open logs db: %v", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		t.Fatalf("ping logs db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	requireUsageLogsSchema(t, db)
	return db
}

// requireUsageLogsSchema 确认日志库已执行迁移（否则测试无法区分「代码错」与
// 「环境没迁移」）。
func requireUsageLogsSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	var reg sql.NullString
	if err := db.QueryRow(`SELECT to_regclass('usage_logs')::text`).Scan(&reg); err != nil {
		t.Fatalf("check usage_logs: %v", err)
	}
	if !reg.Valid {
		t.Fatal("日志库缺少 usage_logs，请先执行日志库迁移（migrations-logs）")
	}
}

// newTestUUID 生成一个 v4 UUID（request_id 列是 uuid 类型）。
func newTestUUID(t *testing.T) string {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// cleanupUsageLog 删除测试期间写入的日志行（按 request_id）。
func cleanupUsageLog(t *testing.T, db *sql.DB, requestID string) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM usage_logs WHERE request_id = $1::uuid`, requestID)
	})
}

// countUsageLog 返回该 request_id 在分区父表上的行数。
func countUsageLog(t *testing.T, db *sql.DB, requestID string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT count(*) FROM usage_logs WHERE request_id = $1::uuid`, requestID).Scan(&n); err != nil {
		t.Fatalf("count usage log: %v", err)
	}
	return n
}

// TestSaturateIntegrationWritesColumns 验证 25 列直接落库后的值。
func TestSaturateIntegrationWritesColumns(t *testing.T) {
	db := openLogsTestDB(t)
	rc := testRelayContext()
	rc.RequestID = newTestUUID(t)
	cleanupUsageLog(t, db, rc.RequestID)

	outcome := SettlementOutcome{
		Cost: 321, PreDeducted: 500, Refunded: 179, Status: UsageLogStatusSuccess,
	}
	l := NewDBLogger(db)
	if err := l.Saturate(context.Background(), testUsageFact(), rc, outcome); err != nil {
		t.Fatalf("Saturate: %v", err)
	}

	var (
		requestID, model, group                   string
		errorClass, errorMessage                  sql.NullString
		tokenID, channelID                        sql.NullInt64
		tokenName, upstream, retryChain           sql.NullString
		userID                                    int64
		prompt, completion, cacheRead, cacheWrite int
		preDeducted, cost, refunded               int64
		latency, firstToken                       sql.NullInt64
		stream                                    bool
		status                                    int16
		other                                     []byte
		createdAt                                 time.Time
	)
	err := db.QueryRow(`
		SELECT request_id::text, user_id, token_id, token_name, channel_id, upstream,
		       model, "group", prompt_tokens, completion_tokens, cache_read_tokens,
		       cache_write_tokens, pre_deducted, cost, refunded, latency_ms,
		       first_token_ms, stream, status, error_class, error_message,
		       retry_chain, other::text, created_at
		  FROM usage_logs WHERE request_id = $1::uuid`, rc.RequestID).
		Scan(&requestID, &userID, &tokenID, &tokenName, &channelID, &upstream,
			&model, &group, &prompt, &completion, &cacheRead, &cacheWrite,
			&preDeducted, &cost, &refunded, &latency, &firstToken, &stream,
			&status, &errorClass, &errorMessage, &retryChain, &other, &createdAt)
	if err != nil {
		t.Fatalf("read usage log row: %v", err)
	}

	if requestID != rc.RequestID {
		t.Fatalf("request_id = %s, want %s", requestID, rc.RequestID)
	}
	if userID != int64(rc.UserID) {
		t.Fatalf("user_id = %d, want %d", userID, rc.UserID)
	}
	if !tokenID.Valid || tokenID.Int64 != int64(rc.TokenID) {
		t.Fatalf("token_id = %+v", tokenID)
	}
	if !channelID.Valid || channelID.Int64 != int64(rc.ChannelID) {
		t.Fatalf("channel_id = %+v", channelID)
	}
	// M3 无数据源的列必须为 NULL，不得用占位值污染统计。
	if tokenName.Valid || upstream.Valid || retryChain.Valid {
		t.Fatalf("token_name/upstream/retry_chain 应为 NULL: %+v %+v %+v", tokenName, upstream, retryChain)
	}
	if model != "gpt-4o" || group != "vip" {
		t.Fatalf("model/group = %s/%s", model, group)
	}
	if prompt != 1000 || completion != 200 || cacheRead != 300 || cacheWrite != 40 {
		t.Fatalf("tokens = %d/%d/%d/%d", prompt, completion, cacheRead, cacheWrite)
	}
	if preDeducted != 500 || cost != 321 || refunded != 179 {
		t.Fatalf("amounts = %d/%d/%d", preDeducted, cost, refunded)
	}
	if !latency.Valid || latency.Int64 != 4567 {
		t.Fatalf("latency_ms = %+v", latency)
	}
	if !firstToken.Valid || firstToken.Int64 != 123 {
		t.Fatalf("first_token_ms = %+v", firstToken)
	}
	if !stream || status != UsageLogStatusSuccess {
		t.Fatalf("stream/status = %v/%d", stream, status)
	}
	if errorClass.Valid || errorMessage.Valid {
		t.Fatalf("success 行不应有错误字段: %+v %+v", errorClass, errorMessage)
	}
	if createdAt.IsZero() {
		t.Fatal("created_at 未由数据库生成")
	}

	var otherMap map[string]any
	if err := json.Unmarshal(other, &otherMap); err != nil {
		t.Fatalf("other 不是 JSON: %v (%s)", err, other)
	}
	admin, ok := otherMap["admin_info"].(map[string]any)
	if !ok {
		t.Fatalf("other.admin_info = %v", otherMap["admin_info"])
	}
	if _, ok := admin["quota_saturation"]; !ok {
		t.Fatalf("other.admin_info.quota_saturation 缺失: %s", other)
	}
	assertNoEstimatedKeys(t, otherMap, "other")
}

// TestSaturateIntegrationIdempotent 验证重复写同一 request_id 只有一行，
// 且 success 不覆盖已有的 refunded 终态（R-25）。
func TestSaturateIntegrationIdempotent(t *testing.T) {
	db := openLogsTestDB(t)
	rc := testRelayContext()
	rc.RequestID = newTestUUID(t)
	cleanupUsageLog(t, db, rc.RequestID)
	l := NewDBLogger(db)
	ctx := context.Background()

	success := SettlementOutcome{Cost: 100, PreDeducted: 100, Status: UsageLogStatusSuccess}
	for i := 0; i < 2; i++ {
		if err := l.Saturate(ctx, testUsageFact(), rc, success); err != nil {
			t.Fatalf("Saturate(success #%d): %v", i, err)
		}
	}
	if n := countUsageLog(t, db, rc.RequestID); n != 1 {
		t.Fatalf("success 重复写入后行数 = %d, want 1", n)
	}

	// worker 在 Saturate 与主库结算之间判定悬挂并退款：success 行收敛为 refunded。
	refunded := SettlementOutcome{PreDeducted: 100, Refunded: 100, Status: UsageLogStatusRefunded}
	if err := l.Saturate(ctx, nil, rc, refunded); err != nil {
		t.Fatalf("Saturate(refunded): %v", err)
	}
	var status int16
	var cost, refundedAmt int64
	if err := db.QueryRow(`SELECT status, cost, refunded FROM usage_logs WHERE request_id = $1::uuid`,
		rc.RequestID).Scan(&status, &cost, &refundedAmt); err != nil {
		t.Fatalf("read converged row: %v", err)
	}
	if status != UsageLogStatusRefunded || cost != 0 || refundedAmt != 100 {
		t.Fatalf("converged row = status %d cost %d refunded %d", status, cost, refundedAmt)
	}

	// 终态之后迟到的 success 绝不能把 refunded 降级。
	if err := l.Saturate(ctx, testUsageFact(), rc, success); err != nil {
		t.Fatalf("Saturate(success after refunded): %v", err)
	}
	if err := db.QueryRow(`SELECT status FROM usage_logs WHERE request_id = $1::uuid`,
		rc.RequestID).Scan(&status); err != nil {
		t.Fatalf("read after late success: %v", err)
	}
	if status != UsageLogStatusRefunded {
		t.Fatalf("late success 覆盖了 refunded: status = %d", status)
	}
	if n := countUsageLog(t, db, rc.RequestID); n != 1 {
		t.Fatalf("行数 = %d, want 1", n)
	}
}

// TestSaturateIntegrationConcurrentConverges 验证并发写（success 与 refunded
// 交错）收敛为恰一行 refunded（R-25 的并发面）。
func TestSaturateIntegrationConcurrentConverges(t *testing.T) {
	db := openLogsTestDB(t)
	rc := testRelayContext()
	rc.RequestID = newTestUUID(t)
	cleanupUsageLog(t, db, rc.RequestID)
	l := NewDBLogger(db)

	const writers = 8
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			outcome := SettlementOutcome{
				Cost: 10, PreDeducted: 10, Status: UsageLogStatusSuccess,
			}
			if i%2 == 1 {
				outcome = SettlementOutcome{
					PreDeducted: 10, Refunded: 10, Status: UsageLogStatusRefunded,
				}
			}
			if err := l.Saturate(context.Background(), testUsageFact(), rc, outcome); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Saturate: %v", err)
	}

	if n := countUsageLog(t, db, rc.RequestID); n != 1 {
		t.Fatalf("并发写入后行数 = %d, want 1", n)
	}
	var status int16
	if err := db.QueryRow(`SELECT status FROM usage_logs WHERE request_id = $1::uuid`,
		rc.RequestID).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != UsageLogStatusRefunded {
		t.Fatalf("最终 status = %d, want refunded（refunded 优先且单向）", status)
	}
}

// TestSaturateIntegrationPartitionRouting 验证写入落到当月分区。
func TestSaturateIntegrationPartitionRouting(t *testing.T) {
	db := openLogsTestDB(t)
	now := time.Now().UTC()
	if err := EnsureUsagePartitions(context.Background(), db, now); err != nil {
		t.Fatalf("EnsureUsagePartitions: %v", err)
	}

	rc := testRelayContext()
	rc.RequestID = newTestUUID(t)
	cleanupUsageLog(t, db, rc.RequestID)
	l := NewDBLogger(db)
	if err := l.Saturate(context.Background(), testUsageFact(), rc,
		SettlementOutcome{Status: UsageLogStatusSuccess}); err != nil {
		t.Fatalf("Saturate: %v", err)
	}

	var partition string
	var createdAt time.Time
	if err := db.QueryRow(
		`SELECT tableoid::regclass::text, created_at FROM usage_logs WHERE request_id = $1::uuid`,
		rc.RequestID).Scan(&partition, &createdAt); err != nil {
		t.Fatalf("read tableoid: %v", err)
	}
	// 用行自身的 created_at 反推期望分区，避免测试恰逢月末的边界抖动。
	want := usagePartitionName(usageMonthStart(createdAt))
	if partition != want {
		t.Fatalf("分区路由 = %s, want %s", partition, want)
	}
}

// TestPartitionIntegrationEnsure 验证幂等创建当月+下月、无 DEFAULT 分区、
// 以及失败（连接已关闭）返回 error。
func TestPartitionIntegrationEnsure(t *testing.T) {
	db := openLogsTestDB(t)
	ctx := context.Background()

	// 使用远期固定月份，避免与真实流量/迁移预建分区冲突。
	now := time.Date(2099, 3, 15, 8, 0, 0, 0, time.UTC)
	names := []string{
		"usage_logs_209903",
		"usage_logs_209904",
	}
	preExisting := make([]bool, len(names))
	for i, name := range names {
		if err := db.QueryRow(`SELECT to_regclass($1) IS NOT NULL`, name).Scan(&preExisting[i]); err != nil {
			t.Fatalf("check %s: %v", name, err)
		}
	}
	t.Cleanup(func() {
		for i, name := range names {
			if !preExisting[i] {
				_, _ = db.Exec(`DROP TABLE IF EXISTS ` + quoteUsagePartitionIdent(name))
			}
		}
	})

	for i := 0; i < 2; i++ {
		if err := EnsureUsagePartitions(ctx, db, now); err != nil {
			t.Fatalf("EnsureUsagePartitions #%d: %v", i, err)
		}
	}

	for _, name := range names {
		var exists bool
		if err := db.QueryRow(`SELECT to_regclass($1) IS NOT NULL`, name).Scan(&exists); err != nil {
			t.Fatalf("check %s: %v", name, err)
		}
		if !exists {
			t.Fatalf("分区 %s 未创建", name)
		}
		var isPartition bool
		if err := db.QueryRow(`
			SELECT c.relispartition FROM pg_class c WHERE c.relname = $1`, name).Scan(&isPartition); err != nil {
			t.Fatalf("relispartition %s: %v", name, err)
		}
		if !isPartition {
			t.Fatalf("%s 不是 usage_logs 的分区", name)
		}
	}

	// 不得存在 DEFAULT 分区（裁决 V11）。
	var defaults int
	if err := db.QueryRow(`
		SELECT count(*) FROM pg_inherits i
		JOIN pg_class c ON c.oid = i.inhrelid
		WHERE i.inhparent = 'usage_logs'::regclass
		  AND pg_get_expr(c.relpartbound, c.oid) = 'DEFAULT'`).Scan(&defaults); err != nil {
		t.Fatalf("query default partition: %v", err)
	}
	if defaults != 0 {
		t.Fatalf("usage_logs 存在 %d 个 DEFAULT 分区，must be 0", defaults)
	}

	// 失败路径：数据库不可用时必须返回 error，不得静默成功。
	closed, err := sql.Open("pgx", mustLogsDSN(t))
	if err != nil {
		t.Fatalf("open closed db: %v", err)
	}
	_ = closed.Close()
	if err := EnsureUsagePartitions(ctx, closed, now); err == nil {
		t.Fatal("EnsureUsagePartitions(closed db) = nil, want error")
	}
}

// mustLogsDSN 返回指向 newtg_logs 的 DSN（测试失败路径用）。
func mustLogsDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("NEWTG_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("NEWTG_TEST_DB_DSN 未设置，跳过集成测试")
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	u.Path = "/newtg_logs"
	return u.String()
}

// TestSaturateIntegrationNoConflictingPartitionKeys 保证测试上下文里
// buildUsageLogOther 的 model 参数与 rc 一致时的映射标记行为（回归：
// 上游模型相同不得写 is_model_mapped）。
func TestSaturateIntegrationNoConflictingPartitionKeys(t *testing.T) {
	rc := testRelayContext()
	rc.UpstreamModel = rc.OriginModel
	other := buildUsageLogOther(rc, rc.OriginModel, sql.NullInt64{})
	if _, ok := other["upstream_model_name"]; ok {
		t.Fatal("上游模型与请求模型相同，不应写 upstream_model_name")
	}
	if _, ok := other["is_model_mapped"]; ok {
		t.Fatal("上游模型与请求模型相同，不应写 is_model_mapped")
	}
}
