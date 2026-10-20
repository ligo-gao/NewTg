//go:build integration

package billing

// D6b live_feed 投递的数据库级验收：真实 logs DB + miniredis。
//
// 证明三件单测无法完整证明的事：
//   - 事件在 usage_logs 事务**提交之后**才出现（插入成功才投递）；
//   - 幂等命中（同一 request_id 重试）不重复投递；
//   - Redis 不可用时 Saturate 仍返回 nil 且日志行照常落库（发布失败不阻塞
//     计费）。
//
// 运行：
//
//	NEWTG_TEST_LOGS_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_d6_logs?sslmode=disable' \
//	  go test -tags=integration ./internal/billing/ -run TestSaturatePublishesLiveFeed -v

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	_ "github.com/jackc/pgx/v5/stdlib"

	"newtg/gateway/internal/contract"
)

func openFeedLogsDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("NEWTG_TEST_LOGS_DSN")
	if dsn == "" {
		t.Skip("NEWTG_TEST_LOGS_DSN 未设置，跳过 live_feed 投递集成测试")
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
	var reg sql.NullString
	if err := db.QueryRow(`SELECT to_regclass('usage_logs')::text`).Scan(&reg); err != nil {
		t.Fatalf("check usage_logs: %v", err)
	}
	if !reg.Valid {
		t.Fatal("日志库缺少 usage_logs，请先执行日志库迁移（migrations-logs）")
	}
	return db
}

func TestSaturatePublishesLiveFeedAfterCommit(t *testing.T) {
	db := openFeedLogsDB(t)
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })

	ctx := context.Background()
	pubsub := rdb.Subscribe(ctx, FeedChannel)
	t.Cleanup(func() { _ = pubsub.Close() })
	if _, err := pubsub.Receive(ctx); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	writer := NewDBLogger(db).WithFeed(NewRedisFeedPublisher(rdb), logger)

	rc := testRelayContext()
	rc.RequestID = newTestUUID(t)
	rc.ChannelType = contract.ChannelTypeAnthropic
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM usage_logs WHERE request_id = $1::uuid`, rc.RequestID)
	})
	outcome := SettlementOutcome{Cost: 100, PreDeducted: 100, RetainedCost: 0, Status: UsageLogStatusSuccess}
	fact := testUsageFact()

	if err := writer.Saturate(ctx, fact, rc, outcome); err != nil {
		t.Fatalf("Saturate: %v", err)
	}

	// 日志行已提交。
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM usage_logs WHERE request_id = $1::uuid`,
		rc.RequestID).Scan(&count); err != nil {
		t.Fatalf("count row: %v", err)
	}
	if count != 1 {
		t.Fatalf("usage_logs rows = %d, want 1", count)
	}

	// list 里有一条事件，字段来自已落库投影（1000/200/4567）。
	items, err := rdb.LRange(ctx, FeedListKey, 0, -1).Result()
	if err != nil {
		t.Fatalf("lrange: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("feed list = %d events, want 1", len(items))
	}
	var event FeedEvent
	if err := json.Unmarshal([]byte(items[0]), &event); err != nil {
		t.Fatalf("decode event: %v", err)
	}
	if event.Model != "gpt-4o" || event.Upstream != "anthropic" ||
		event.TokensIn != 1000 || event.TokensOut != 200 ||
		event.LatencyMS != 4567 || event.Status != "ok" {
		t.Fatalf("event = %+v", event)
	}

	// 频道也收到了同一份事件。
	select {
	case msg := <-pubsub.Channel():
		var published FeedEvent
		if err := json.Unmarshal([]byte(msg.Payload), &published); err != nil {
			t.Fatalf("decode published: %v", err)
		}
		if published.ID != event.ID {
			t.Fatalf("published id = %q, want %q", published.ID, event.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pub/sub 未收到事件")
	}

	// 同一 request_id 重试：幂等命中，不重复投递。
	if err := writer.Saturate(ctx, fact, rc, outcome); err != nil {
		t.Fatalf("idempotent Saturate: %v", err)
	}
	if got := rdb.LLen(ctx, FeedListKey).Val(); got != 1 {
		t.Fatalf("idempotent replay published again: list=%d", got)
	}
	select {
	case msg := <-pubsub.Channel():
		t.Fatalf("幂等重试不应再次广播: %s", msg.Payload)
	case <-time.After(50 * time.Millisecond):
	}

	// Redis 故障：新请求照常落库，Saturate 返回 nil（发布失败不阻塞计费）。
	mr.Close()
	rc2 := testRelayContext()
	rc2.RequestID = newTestUUID(t)
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM usage_logs WHERE request_id = $1::uuid`, rc2.RequestID)
	})
	if err := writer.Saturate(ctx, fact, rc2, outcome); err != nil {
		t.Fatalf("Saturate with redis down must not fail billing: %v", err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM usage_logs WHERE request_id = $1::uuid`,
		rc2.RequestID).Scan(&count); err != nil {
		t.Fatalf("count second row: %v", err)
	}
	if count != 1 {
		t.Fatalf("usage_logs rows after redis failure = %d, want 1", count)
	}
}
