//go:build integration

package store

// 本文件钉住 usage_logs 对账聚合的计数口径（上游用量对账设计 §7.1）。
//
// 背景：items 表有 CHECK (local_refunded_requests <= local_requests)，而
// InsertItems 把同一账号的全部明细放在**一个事务**里；只要有一行退款数大于
// 请求数，整次对账就会 failed。旧实现把 requests 过滤成 status = 1，导致
// 「只有退款的一天」requests = 0 < refunded = 1，CHECK 永远不可满足。
//
// 运行（日志库 DSN 独立，未设置时跳过；绝不回落到其它默认库）：
//
//	NEWTG_TEST_LOGS_DSN='postgres://newtg:<pw>@127.0.0.1:55432/newtg_it_logs?sslmode=disable' \
//	  go test -tags=integration -count=1 ./internal/store/ -run 'TestUsageAggregate' -v
//
// 日期从 time.Now() 推导而不是写死：日志迁移只预建了执行时刻起 12 个月的分区，
// 固定日期跨月后会因分区不存在而失败。每行 request_id 用 uuid 唯一命名，
// t.Cleanup 按 id 删除；日志库与其它包的集成测试并行共享，必须可重入。

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	// pgx/v5 stdlib 驱动与生产一致。
	_ "github.com/jackc/pgx/v5/stdlib"
)

// openUsageAggregateTestLogsDB 只读 NEWTG_TEST_LOGS_DSN：聚合测试的断言
// 依赖日志库 schema，静默回落到别的库等于没跑。
func openUsageAggregateTestLogsDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("NEWTG_TEST_LOGS_DSN")
	if dsn == "" {
		t.Skip("NEWTG_TEST_LOGS_DSN 未设置，跳过 usage_logs 聚合集成测试")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open logs db: %v", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		t.Fatalf("ping logs db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// aggregateLogRow 描述一条待插入的 usage_logs 行；零值即 NULL/默认。
type aggregateLogRow struct {
	model      string
	status     int16
	accountID  *int64
	channelID  *int64
	source     *int16
	semantic   *int16
	prompt     int
	cacheRead  int
	completion int
	cost       int64
	attempts   int
}

// TestUsageAggregateCountsEveryStatus 覆盖退款、失败、未知语义与归属四条口径。
func TestUsageAggregateCountsEveryStatus(t *testing.T) {
	logsDB := openUsageAggregateTestLogsDB(t)
	repo := NewUsageAggregateRepo(logsDB)
	ctx := context.Background()

	// accountID / channelID 用纳秒时间戳取唯一值：日志库与其它包并行共享，
	// 不能依赖「库里没有别人的行」。suffix 让模型名同样唯一。
	base := time.Now().UnixNano()
	accountID := base
	boundChannel := base + 1
	unboundChannel := base + 2
	suffix := uuid.NewString()[:8]

	now := time.Now().UTC()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	// 当天中午落在当月既有分区内；半开窗口 [day, day+24h) 恰好覆盖它。
	at := day.Add(12 * time.Hour)

	requestIDs := make([]string, 0, 8)
	insert := func(row aggregateLogRow) {
		t.Helper()
		requestID := uuid.NewString()
		_, err := logsDB.Exec(`
			INSERT INTO usage_logs (request_id, user_id, channel_id, model,
				prompt_tokens, completion_tokens, cache_read_tokens, cost, status,
				upstream_model, upstream_account_id, usage_source, usage_semantic,
				attempts, created_at)
			VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`,
			requestID, int64(1), row.channelID, row.model,
			row.prompt, row.completion, row.cacheRead, row.cost, row.status,
			row.model, row.accountID, row.source, row.semantic, row.attempts, at)
		if err != nil {
			t.Fatalf("insert usage_log %s: %v", row.model, err)
		}
		requestIDs = append(requestIDs, requestID)
	}
	t.Cleanup(func() {
		for _, requestID := range requestIDs {
			if _, err := logsDB.Exec(`DELETE FROM usage_logs WHERE request_id = $1::uuid`, requestID); err != nil {
				t.Errorf("cleanup usage_log %s: %v", requestID, err)
			}
		}
	})

	semanticPromptIncludesCache := int16(0)
	sourceExact := int16(0)
	sourceReserved := int16(3)

	refundedOnly := "agg-" + suffix + "-refunded-only"
	mixed := "agg-" + suffix + "-mixed"
	failed := "agg-" + suffix + "-failed"
	unknown := "agg-" + suffix + "-unknown"
	attributed := "agg-" + suffix + "-attributed"

	// (1) 只有退款的一天：requests 必须等于 refunded（旧查询这里是 0 < 1）。
	insert(aggregateLogRow{
		model: refundedOnly, status: 3, accountID: &accountID,
		semantic: &semanticPromptIncludesCache, prompt: 100, attempts: 1,
	})

	// (2) 两条退款 + 一条成功：requests = 3、refunded = 2（旧查询得到 1 < 2）。
	for i := 0; i < 2; i++ {
		insert(aggregateLogRow{
			model: mixed, status: 3, accountID: &accountID,
			semantic: &semanticPromptIncludesCache, prompt: 100, attempts: 1,
		})
	}
	insert(aggregateLogRow{
		model: mixed, status: 1, accountID: &accountID,
		semantic: &semanticPromptIncludesCache, prompt: 100, attempts: 1,
	})

	// (3) status = 2（error）且 token 非零：token 仍要汇总，请求数仍要计入。
	//     usage_source = 3（reserved_default）钉住 estimated 的 IN (2, 3)：
	//     设计 §7.1 字面的 = 2 会把这一行漏掉。
	insert(aggregateLogRow{
		model: failed, status: 2, accountID: &accountID,
		source: &sourceReserved, semantic: &semanticPromptIncludesCache,
		prompt: 100, cacheRead: 40, completion: 10, attempts: 1,
	})

	// (4) usage_semantic IS NULL：hit/output 仍计入，miss 为 0（不可知），
	//     unknown_requests = 1。
	insert(aggregateLogRow{
		model: unknown, status: 1, accountID: &accountID,
		source: &sourceExact, prompt: 100, cacheRead: 40, completion: 10, attempts: 1,
	})

	// (5) 归属：直连账号 + legacy 绑定渠道都算，legacy 未绑定渠道不算。
	insert(aggregateLogRow{
		model: attributed, status: 1, accountID: &accountID,
		source: &sourceExact, semantic: &semanticPromptIncludesCache, attempts: 1,
	})
	insert(aggregateLogRow{
		model: attributed, status: 1, channelID: &boundChannel,
		source: &sourceExact, semantic: &semanticPromptIncludesCache, attempts: 1,
	})
	insert(aggregateLogRow{
		model: attributed, status: 1, channelID: &unboundChannel,
		source: &sourceExact, semantic: &semanticPromptIncludesCache, attempts: 1,
	})

	rows, err := repo.Aggregate(ctx, accountID, []int64{boundChannel}, "UTC", day, day.AddDate(0, 0, 1))
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	byModel := make(map[string]LocalUsage, len(rows))
	for _, row := range rows {
		byModel[row.Model] = row
	}
	if len(rows) != 5 {
		t.Fatalf("aggregate rows = %d (%+v), want 5 (one per model)", len(rows), rows)
	}

	// (6) CHECK 依赖的不变量：任何一行都必须 refunded <= requests。
	for _, row := range rows {
		if row.RefundedRequests > row.Requests {
			t.Fatalf("model %q: refunded_requests = %d > requests = %d（items 表 CHECK 会拒绝整批）",
				row.Model, row.RefundedRequests, row.Requests)
		}
	}

	if got := byModel[refundedOnly]; got.Requests != 1 || got.RefundedRequests != 1 {
		t.Fatalf("%s: requests/refunded = %d/%d, want 1/1", refundedOnly, got.Requests, got.RefundedRequests)
	}
	if got := byModel[mixed]; got.Requests != 3 || got.RefundedRequests != 2 {
		t.Fatalf("%s: requests/refunded = %d/%d, want 3/2", mixed, got.Requests, got.RefundedRequests)
	}
	if got := byModel[failed]; got.Requests != 1 || got.EstimatedRequests != 1 ||
		got.CacheHitTokens != 40 || got.CacheMissTokens != 60 || got.OutputTokens != 10 {
		t.Fatalf("%s: requests/estimated/hit/miss/output = %d/%d/%d/%d/%d, want 1/1/40/60/10",
			failed, got.Requests, got.EstimatedRequests, got.CacheHitTokens, got.CacheMissTokens, got.OutputTokens)
	}
	if got := byModel[unknown]; got.Requests != 1 || got.UnknownRequests != 1 ||
		got.CacheHitTokens != 40 || got.CacheMissTokens != 0 || got.OutputTokens != 10 {
		t.Fatalf("%s: requests/unknown/hit/miss/output = %d/%d/%d/%d/%d, want 1/1/40/0/10",
			unknown, got.Requests, got.UnknownRequests, got.CacheHitTokens, got.CacheMissTokens, got.OutputTokens)
	}
	if got := byModel[attributed]; got.Requests != 2 {
		t.Fatalf("%s: requests = %d, want 2（未绑定渠道的行不得计入）", attributed, got.Requests)
	}
}
