//go:build integration

package store

// 公开性能接口的批量读面（模型库）日志库集成验收。
//
// 运行：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_test?sslmode=disable' \
//	  go test -tags=integration ./internal/store/ -run 'Batch|LatestByModels' -v
//
// 未设置 DSN 时 Skip；DSN 的 dbname 会被 openLogsTestDB 替换为日志库。

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
)

// batchIntPtr 返回 int 的指针，供用例构造可空列。
func batchIntPtr(v int) *int { return &v }

// insertPublicMetricsUsageLog 写一条 usage_logs 测试行；nil 延迟/首字写 NULL。
func insertPublicMetricsUsageLog(
	t *testing.T,
	db *sql.DB,
	model string,
	at time.Time,
	status int16,
	latencyMS, firstTokenMS *int,
	completionTokens int,
) {
	t.Helper()
	ensureUsageLogsPartition(t, db, at)
	if _, err := db.Exec(`
		INSERT INTO usage_logs
			(request_id, user_id, channel_id, model, status,
			 latency_ms, first_token_ms, completion_tokens, created_at)
		VALUES ($1, 1, 1, $2, $3, $4, $5, $6, $7)`,
		uuid.NewString(), model, status, latencyMS, firstTokenMS, completionTokens, at.UTC(),
	); err != nil {
		t.Fatalf("insert usage log for %s: %v", model, err)
	}
}

// TestPublicModelMetricsRepoBatch 验证批量聚合与逐模型口径一致：
//   - 同一窗口内两个模型各 2 行，恰好返回 2 项；
//   - 每项的计数/平均与单模型方法一致；
//   - 未入参的模型不出现，窗口外的行不计入；
//   - 批量行不带 per-group 拆分（Groups 必须为 nil）。
func TestPublicModelMetricsRepoBatch(t *testing.T) {
	db := openLogsTestDB(t)
	repo := NewPublicModelMetricsRepo(db)
	ctx := context.Background()

	models := []string{"openai/gpt-5", "vendor:embed", "other/model"}
	// 清理可能残留的同名模型行，保证聚合口径确定。
	cleanup := func() {
		if _, err := db.Exec(`DELETE FROM usage_logs WHERE model = ANY($1::text[])`, models); err != nil {
			t.Errorf("cleanup usage logs: %v", err)
		}
	}
	cleanup()
	t.Cleanup(cleanup)

	end := time.Now().UTC().Truncate(time.Minute)
	start := end.Add(-2 * time.Hour)

	// gpt-5：2 行窗口内（1 成功 + 1 失败），1 行窗口外。
	insertPublicMetricsUsageLog(t, db, "openai/gpt-5", start.Add(10*time.Minute), 1, batchIntPtr(100), batchIntPtr(40), 10)
	insertPublicMetricsUsageLog(t, db, "openai/gpt-5", start.Add(20*time.Minute), 2, batchIntPtr(300), nil, 20)
	insertPublicMetricsUsageLog(t, db, "openai/gpt-5", start.Add(-1*time.Hour), 1, batchIntPtr(900), batchIntPtr(900), 99)
	// vendor:embed：2 行窗口内，全部成功。
	insertPublicMetricsUsageLog(t, db, "vendor:embed", start.Add(15*time.Minute), 1, batchIntPtr(200), batchIntPtr(60), 30)
	insertPublicMetricsUsageLog(t, db, "vendor:embed", start.Add(25*time.Minute), 1, batchIntPtr(400), batchIntPtr(80), 40)
	// other/model：窗口内一行，但不在入参里，必须不出现。
	insertPublicMetricsUsageLog(t, db, "other/model", start.Add(5*time.Minute), 1, batchIntPtr(50), batchIntPtr(5), 1)

	got, err := repo.LoadPublicModelMetricsBatch(ctx, []string{"openai/gpt-5", "vendor:embed"}, start, end)
	if err != nil {
		t.Fatalf("LoadPublicModelMetricsBatch: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("batch len = %d, want 2 (%+v)", len(got), got)
	}
	if _, ok := got["other/model"]; ok {
		t.Fatalf("模型不在入参中却被返回: %+v", got["other/model"])
	}

	gpt5, ok := got["openai/gpt-5"]
	if !ok {
		t.Fatalf("missing openai/gpt-5 in batch result")
	}
	single, err := repo.LoadPublicModelMetrics(ctx, "openai/gpt-5", start, end)
	if err != nil {
		t.Fatalf("LoadPublicModelMetrics: %v", err)
	}
	if gpt5.SampleSize != single.SampleSize || gpt5.SampleSize != 2 {
		t.Fatalf("gpt-5 sample_size = %d, single = %d, want 2", gpt5.SampleSize, single.SampleSize)
	}
	if gpt5.SuccessCount != single.SuccessCount || gpt5.SuccessCount != 1 {
		t.Fatalf("gpt-5 success_count = %d, single = %d, want 1", gpt5.SuccessCount, single.SuccessCount)
	}
	if gpt5.AvgTTFTMs == nil || *gpt5.AvgTTFTMs != 40 {
		t.Fatalf("gpt-5 avg_ttft_ms = %v, want 40 (single=%v)", gpt5.AvgTTFTMs, single.AvgTTFTMs)
	}
	if single.AvgTTFTMs == nil || *gpt5.AvgTTFTMs != *single.AvgTTFTMs {
		t.Fatalf("gpt-5 avg_ttft_ms = %v, single = %v", gpt5.AvgTTFTMs, single.AvgTTFTMs)
	}
	if gpt5.AvgLatencyMs == nil || *gpt5.AvgLatencyMs != 200 {
		t.Fatalf("gpt-5 avg_latency_ms = %v, want 200", gpt5.AvgLatencyMs)
	}
	if gpt5.Groups != nil {
		t.Fatalf("batch row must not carry per-group split: %+v", gpt5.Groups)
	}

	embed, ok := got["vendor:embed"]
	if !ok {
		t.Fatalf("missing vendor:embed in batch result")
	}
	if embed.SampleSize != 2 || embed.SuccessCount != 2 {
		t.Fatalf("vendor:embed = %+v, want sample=2 success=2", embed)
	}
	if embed.AvgTTFTMs == nil || *embed.AvgTTFTMs != 70 {
		t.Fatalf("vendor:embed avg_ttft_ms = %v, want 70", embed.AvgTTFTMs)
	}
	if embed.Groups != nil {
		t.Fatalf("batch row must not carry per-group split: %+v", embed.Groups)
	}

	// 空入参：返回空 map，且不查库。
	empty, err := repo.LoadPublicModelMetricsBatch(ctx, nil, start, end)
	if err != nil {
		t.Fatalf("empty batch: %v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Fatalf("empty batch = %+v, want empty map", empty)
	}
}
