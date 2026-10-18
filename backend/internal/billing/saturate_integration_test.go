//go:build integration

package billing

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"newtg/gateway/internal/contract"
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
	// R-59：已交付用量的 ChargeIncrement 消耗（30）保留为 cost，退回的 hold=100。
	refunded := SettlementOutcome{
		PreDeducted: 100, Refunded: 100, RetainedCost: 30,
		Status: UsageLogStatusRefunded, ErrorClass: "worker_refund", ErrorMessage: "abandoned",
	}
	if err := l.Saturate(ctx, nil, rc, refunded); err != nil {
		t.Fatalf("Saturate(refunded): %v", err)
	}
	var status int16
	var cost, refundedAmt int64
	var errorClass string
	if err := db.QueryRow(
		`SELECT status, cost, refunded, COALESCE(error_class, '') FROM usage_logs WHERE request_id = $1::uuid`,
		rc.RequestID).Scan(&status, &cost, &refundedAmt, &errorClass); err != nil {
		t.Fatalf("read converged row: %v", err)
	}
	if status != UsageLogStatusRefunded || cost != 30 || refundedAmt != 100 {
		t.Fatalf("converged row = status %d cost %d refunded %d（want 3/30/100）",
			status, cost, refundedAmt)
	}
	if errorClass != "worker_refund" {
		t.Fatalf("converged error_class = %q, want worker_refund（收敛需记录退款原因）", errorClass)
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

	for i, name := range names {
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
		// 断言精确的 [月初, 次月初) 边界，而不只是「分区存在」。
		var bound string
		if err := db.QueryRow(`
			SELECT pg_get_expr(c.relpartbound, c.oid) FROM pg_class c WHERE c.relname = $1`,
			name).Scan(&bound); err != nil {
			t.Fatalf("relpartbound %s: %v", name, err)
		}
		wantBounds := []string{
			"FOR VALUES FROM ('2099-03-01 00:00:00+00') TO ('2099-04-01 00:00:00+00')",
			"FOR VALUES FROM ('2099-04-01 00:00:00+00') TO ('2099-05-01 00:00:00+00')",
		}
		if bound != wantBounds[i] {
			t.Fatalf("%s 边界 = %q, want %q", name, bound, wantBounds[i])
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

// TestSaturateIntegrationAppRoleConvergence 是 N1 的关键回归：非属主角色在
// **应用角色授权**下执行真实的 refunded 收敛。此前授权只有三列，而收敛
// UPDATE 还写 error_class/error_message —— 属主/超级用户测试发现不了，
// 生产 newtg_app 会 permission denied。
func TestSaturateIntegrationAppRoleConvergence(t *testing.T) {
	admin := openLogsTestDB(t)
	ctx := context.Background()
	requestID := newTestUUID(t)
	cleanupUsageLog(t, admin, requestID)

	rc := testRelayContext()
	rc.RequestID = requestID
	// 属主先写一行 success，模拟「Terminate 已落日志、结算前被 worker 回收」。
	if err := NewDBLogger(admin).Saturate(ctx, testUsageFact(), rc, SettlementOutcome{
		Cost: 100, PreDeducted: 100, Status: UsageLogStatusSuccess,
	}); err != nil {
		t.Fatalf("owner Saturate(success): %v", err)
	}

	role := fmt.Sprintf("t6_app_%d", time.Now().UnixNano())
	dsn := logsDSNForUser(t, role, "t6probe")
	if _, err := admin.Exec(fmt.Sprintf("CREATE ROLE %s LOGIN PASSWORD 't6probe'", role)); err != nil {
		t.Skipf("无法创建临时角色（需要 CREATEROLE），跳过应用角色授权路径: %v", err)
	}
	var db *sql.DB
	t.Cleanup(func() {
		if db != nil {
			_ = db.Close()
		}
		// 先清掉该角色在当前库的授权/属主依赖，再删角色。两个错误都必须
		// 暴露：静默吞掉 DROP 失败会让共享测试库持续泄漏 login role。
		if _, err := admin.Exec("DROP OWNED BY " + role + " CASCADE"); err != nil {
			t.Errorf("DROP OWNED BY %s: %v", role, err)
		}
		if _, err := admin.Exec("DROP ROLE " + role); err != nil {
			t.Errorf("DROP ROLE %s: %v", role, err)
		}
	})
	// 精确复刻应用角色在日志库的授权：CONNECT/USAGE/SELECT + 五列 UPDATE。
	// 故意不授 INSERT/其它列 UPDATE，验证最小权限。
	for _, stmt := range []string{
		"GRANT CONNECT ON DATABASE newtg_logs TO " + role,
		"GRANT USAGE ON SCHEMA public TO " + role,
		"GRANT SELECT ON usage_logs TO " + role,
		"GRANT UPDATE (status, cost, refunded, error_class, error_message) ON usage_logs TO " + role,
	} {
		if _, err := admin.Exec(stmt); err != nil {
			t.Fatalf("grant to temp role: %v (%s)", err, stmt)
		}
	}

	var err error
	db, err = sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open app-role db: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("ping app-role db: %v", err)
	}

	// 走完整 DBLogger 收敛路径（咨询锁 + 查重 + UPDATE），而不是裸 SQL。
	if err := NewDBLogger(db).Saturate(ctx, nil, rc, SettlementOutcome{
		PreDeducted: 100, Refunded: 100, RetainedCost: 30,
		Status: UsageLogStatusRefunded, ErrorClass: "worker_refund", ErrorMessage: "abandoned",
	}); err != nil {
		t.Fatalf("app-role convergence = %v, want nil（R-60 授权必须覆盖全部被写列）", err)
	}

	var status int16
	var cost, refunded int64
	var errorClass string
	if err := admin.QueryRow(
		`SELECT status, cost, refunded, COALESCE(error_class, '') FROM usage_logs WHERE request_id = $1::uuid`,
		requestID).Scan(&status, &cost, &refunded, &errorClass); err != nil {
		t.Fatalf("read converged row: %v", err)
	}
	if status != UsageLogStatusRefunded || cost != 30 || refunded != 100 || errorClass != "worker_refund" {
		t.Fatalf("converged row = status %d cost %d refunded %d error_class %q",
			status, cost, refunded, errorClass)
	}

	// 最小权限：授权之外的列必须更新失败。
	if _, err := db.Exec(`UPDATE usage_logs SET model = 'tampered' WHERE request_id = $1::uuid`,
		requestID); err == nil {
		t.Fatal("未授权的列更新应失败（五列授权之外不得可写）")
	}

	// 红/绿：把授权降回旧的三列，收敛必须失败（证明本测试能捕获 N1）。
	if _, err := admin.Exec("REVOKE UPDATE ON usage_logs FROM " + role); err != nil {
		t.Fatalf("revoke update: %v", err)
	}
	if _, err := admin.Exec(
		"GRANT UPDATE (status, cost, refunded) ON usage_logs TO " + role); err != nil {
		t.Fatalf("grant three columns: %v", err)
	}
	err = NewDBLogger(db).Saturate(ctx, nil, rc, SettlementOutcome{
		PreDeducted: 100, Refunded: 100, RetainedCost: 30,
		Status: UsageLogStatusRefunded, ErrorClass: "worker_refund", ErrorMessage: "abandoned",
	})
	if err == nil {
		t.Fatal("三列授权下收敛应失败（error_class/error_message 不可写）")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "permission denied") {
		t.Fatalf("三列授权下的错误应包含 permission denied: %v", err)
	}
}

// TestPartitionIntegrationDDLUnavailable 验证 R-56 的「运行期身份无 DDL」路径：
// 非属主且无 schema CREATE 的连接必须在尝试任何 DDL 之前得到可识别的
// *PartitionDDLUnavailableError（worker 据此记 warn，而不是反复报错）。
func TestPartitionIntegrationDDLUnavailable(t *testing.T) {
	admin := openLogsTestDB(t)
	ctx := context.Background()
	role := fmt.Sprintf("t6_noddl_%d", time.Now().UnixNano())
	dsn := logsDSNForUser(t, role, "t6probe")

	if _, err := admin.Exec(fmt.Sprintf("CREATE ROLE %s LOGIN PASSWORD 't6probe'", role)); err != nil {
		t.Skipf("无法创建临时角色（需要 CREATEROLE），跳过 DDL 权限路径: %v", err)
	}
	var db *sql.DB
	t.Cleanup(func() {
		if db != nil {
			_ = db.Close()
		}
		// 与 app-role 测试同口径：DROP OWNED 清授权，再 DROP ROLE；错误不吞。
		if _, err := admin.Exec("DROP OWNED BY " + role + " CASCADE"); err != nil {
			t.Errorf("DROP OWNED BY %s: %v", role, err)
		}
		if _, err := admin.Exec("DROP ROLE " + role); err != nil {
			t.Errorf("DROP ROLE %s: %v", role, err)
		}
	})
	for _, stmt := range []string{
		"GRANT CONNECT ON DATABASE newtg_logs TO " + role,
		"GRANT USAGE ON SCHEMA public TO " + role,
		"GRANT SELECT ON usage_logs TO " + role,
	} {
		if _, err := admin.Exec(stmt); err != nil {
			t.Fatalf("grant to temp role: %v (%s)", err, stmt)
		}
	}

	var err error
	db, err = sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open temp role db: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("ping temp role db: %v", err)
	}

	now := time.Date(2099, 3, 15, 8, 0, 0, 0, time.UTC)
	err = EnsureUsagePartitions(ctx, db, now)
	var ddlErr *PartitionDDLUnavailableError
	if !errors.As(err, &ddlErr) {
		t.Fatalf("EnsureUsagePartitions = %v, want *PartitionDDLUnavailableError", err)
	}
	if ddlErr.SchemaCreate || ddlErr.OwnsParent {
		t.Fatalf("权限探测 = %+v, want schema_create=false owns_parent=false", ddlErr)
	}
	if !strings.Contains(err.Error(), "migrate credentials") {
		t.Fatalf("错误信息缺少运维指引: %v", err)
	}
	// 未授权身份绝不能创建出分区。
	var exists bool
	if err := admin.QueryRow(`SELECT to_regclass('usage_logs_209903') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatalf("check partition: %v", err)
	}
	if exists {
		t.Fatal("无 DDL 权限的连接不应创建出分区")
	}
}

// TestSaturateIntegrationRefundedRetainedCost 验证 R-59 的落库：
// refunded 保留的 ChargeIncrement 消耗写 cost，纯退款仍为 0。
func TestSaturateIntegrationRefundedRetainedCost(t *testing.T) {
	db := openLogsTestDB(t)
	l := NewDBLogger(db)
	ctx := context.Background()

	retained := testRelayContext()
	retained.RequestID = newTestUUID(t)
	cleanupUsageLog(t, db, retained.RequestID)
	if err := l.Saturate(ctx, nil, retained, SettlementOutcome{
		PreDeducted: 300, Refunded: 300, RetainedCost: 50, Status: UsageLogStatusRefunded,
	}); err != nil {
		t.Fatalf("Saturate(retained): %v", err)
	}
	var cost, refunded int64
	var status int16
	if err := db.QueryRow(`SELECT status, cost, refunded FROM usage_logs WHERE request_id = $1::uuid`,
		retained.RequestID).Scan(&status, &cost, &refunded); err != nil {
		t.Fatalf("read retained row: %v", err)
	}
	if status != UsageLogStatusRefunded || cost != 50 || refunded != 300 {
		t.Fatalf("retained row = status %d cost %d refunded %d, want 3/50/300", status, cost, refunded)
	}

	plain := testRelayContext()
	plain.RequestID = newTestUUID(t)
	cleanupUsageLog(t, db, plain.RequestID)
	if err := l.Saturate(ctx, nil, plain, SettlementOutcome{
		PreDeducted: 300, Refunded: 300, Status: UsageLogStatusRefunded,
	}); err != nil {
		t.Fatalf("Saturate(plain): %v", err)
	}
	if err := db.QueryRow(`SELECT cost FROM usage_logs WHERE request_id = $1::uuid`,
		plain.RequestID).Scan(&cost); err != nil {
		t.Fatalf("read plain row: %v", err)
	}
	if cost != 0 {
		t.Fatalf("plain refunded cost = %d, want 0", cost)
	}
}

// TestSaturateIntegrationNULPath 回归 I2：带 NUL 的请求路径曾使日志 INSERT
// 以 SQLSTATE 22P05 失败，进而阻塞结算。other 的字符串值必须被清洗。
func TestSaturateIntegrationNULPath(t *testing.T) {
	db := openLogsTestDB(t)
	rc := testRelayContext()
	rc.RequestID = newTestUUID(t)
	rc.RequestPath = "/v1/chat\x00/completions"
	rc.UpstreamModel = "gpt\x00-4o-upstream"
	cleanupUsageLog(t, db, rc.RequestID)

	l := NewDBLogger(db)
	if err := l.Saturate(context.Background(), testUsageFact(), rc,
		SettlementOutcome{Status: UsageLogStatusSuccess}); err != nil {
		t.Fatalf("Saturate(NUL path) = %v, want nil（清洗后应可落库）", err)
	}
	var path string
	if err := db.QueryRow(`SELECT other->>'request_path' FROM usage_logs WHERE request_id = $1::uuid`,
		rc.RequestID).Scan(&path); err != nil {
		t.Fatalf("read other.request_path: %v", err)
	}
	if strings.ContainsRune(path, '\x00') {
		t.Fatalf("request_path 仍含 NUL: %q", path)
	}
}

// TestSaturateIntegrationLockCanonicalizesUUID 回归 I1：同一个 UUID 的大小写/
// 花括号拼写必须共享同一把咨询锁，否则并发写会插入两行（评审探针 99/100）。
func TestSaturateIntegrationLockCanonicalizesUUID(t *testing.T) {
	db := openLogsTestDB(t)
	l := NewDBLogger(db)

	for i := 0; i < 10; i++ {
		canonical := newTestUUID(t)
		cleanupUsageLog(t, db, canonical)
		spellings := []string{strings.ToUpper(canonical), "{" + canonical + "}"}
		outcome := SettlementOutcome{Cost: 1, Status: UsageLogStatusSuccess}

		start := make(chan struct{})
		var wg sync.WaitGroup
		errs := make(chan error, len(spellings))
		for _, spelling := range spellings {
			wg.Add(1)
			go func(spelling string) {
				defer wg.Done()
				<-start
				rc := &contract.RelayContext{RequestID: spelling, UserID: 1}
				if err := l.Saturate(context.Background(), testUsageFact(), rc, outcome); err != nil {
					errs <- err
				}
			}(spelling)
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("iteration %d: Saturate: %v", i, err)
		}
		if n := countUsageLog(t, db, canonical); n != 1 {
			t.Fatalf("iteration %d: 行数 = %d, want 1（UUID 拼写必须共享同一把锁）", i, n)
		}
	}
}

// logsDSNForUser 返回以指定角色/口令连接 newtg_logs 的 DSN。
func logsDSNForUser(t *testing.T, user, password string) string {
	t.Helper()
	u, err := url.Parse(mustLogsDSN(t))
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	u.User = url.UserPassword(user, password)
	return u.String()
}
