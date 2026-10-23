//go:build integration

package store

// Task 5 的数据库级验收：候选并集的 SQL 真相。
//
// 单测的假仓储只能证明 handler 的映射；这里证明四个来源在 SQL 层真的合并、
// 去重，并且 channel/model_mapping 的键都参与。运行：
//
//	NEWTG_TEST_DB_DSN='postgres://.../newtg_test?sslmode=disable' \
//	  go test -tags=integration ./internal/store/ -run TestPricingCandidates -v
//
// 未设置 DSN 时跳过；只插入/删除本测试命名的行。

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestPricingCandidatesUnion(t *testing.T) {
	db := openPublicPricingTestDB(t)
	repo := NewPricingRepo(db)
	ctx := context.Background()

	suffix := time.Now().UnixNano()
	tag := fmt.Sprintf("t5cand%d", suffix)
	catalog := tag + "-catalog"
	channelOnly := tag + "-channel"
	mappingOnly := tag + "-mapping"
	pricedOnly := tag + "-priced"
	all := tag + "-all"
	vendorName := tag + "-vendor"
	channelName := tag + "-ch"

	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM model_pricing WHERE model IN ($1, $2)`, pricedOnly, all)
		_, _ = db.Exec(`DELETE FROM model_meta WHERE model_name IN ($1, $2)`, catalog, all)
		_, _ = db.Exec(`DELETE FROM channels WHERE name = $1`, channelName)
		_, _ = db.Exec(`DELETE FROM vendors WHERE name = $1`, vendorName)
	})

	var vendorID int64
	if err := db.QueryRowContext(ctx,
		`INSERT INTO vendors (name) VALUES ($1) RETURNING id`, vendorName).Scan(&vendorID); err != nil {
		t.Fatalf("insert vendor: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO model_meta (model_name, display_name, vendor_id, context_length, status, name_rule)
		VALUES ($1, $2, $3, 128000, 1, 0)`,
		catalog, "Catalog Display", vendorID); err != nil {
		t.Fatalf("insert model_meta: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO channels (name, type, credentials, supported_models, model_mapping, status)
		VALUES ($1, 'openai', $2, $3::jsonb, $4::jsonb, 1)`,
		channelName, []byte{0},
		fmt.Sprintf(`["%s","%s"]`, channelOnly, all),
		fmt.Sprintf(`{"%s":"upstream-real"}`, mappingOnly)); err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO model_pricing (model, mode, prompt_price, author, status)
		VALUES ($1, 0, 100, 't5', 1)`, pricedOnly); err != nil {
		t.Fatalf("insert priced model: %v", err)
	}
	// 三来源同时命中：model_meta + channel + model_pricing。
	if _, err := db.ExecContext(ctx, `
		INSERT INTO model_meta (model_name, status, name_rule) VALUES ($1, 1, 0)`, all); err != nil {
		t.Fatalf("insert all-sources meta: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO model_pricing (model, mode, prompt_price, author, status)
		VALUES ($1, 1, 300, 't5', 1)`, all); err != nil {
		t.Fatalf("insert all-sources pricing: %v", err)
	}
	// 让 all 同时出现在渠道的 supported_models。
	if _, err := db.ExecContext(ctx,
		`UPDATE channels SET supported_models = supported_models || to_jsonb($1::text)
		 WHERE name = $2`, all, channelName); err != nil {
		t.Fatalf("extend channel models: %v", err)
	}

	items, err := repo.ListPricingCandidates(ctx, PricingCandidateFilter{Keyword: tag, Limit: 50})
	if err != nil {
		t.Fatalf("ListPricingCandidates: %v", err)
	}
	byModel := make(map[string]PricingCandidate, len(items))
	for _, item := range items {
		if _, dup := byModel[item.Model]; dup {
			t.Fatalf("候选 %q 重复", item.Model)
		}
		byModel[item.Model] = item
	}

	assertSources := func(model string, want ...string) PricingCandidate {
		t.Helper()
		item, ok := byModel[model]
		if !ok {
			t.Fatalf("候选 %q 缺失（items=%+v）", model, items)
		}
		if len(item.Sources) != len(want) {
			t.Fatalf("%s sources = %v，期望 %v", model, item.Sources, want)
		}
		for i := range want {
			if item.Sources[i] != want[i] {
				t.Fatalf("%s sources = %v，期望 %v", model, item.Sources, want)
			}
		}
		return item
	}

	catalogItem := assertSources(catalog, PricingCandidateSourceCatalog)
	if catalogItem.DisplayName == nil || *catalogItem.DisplayName != "Catalog Display" {
		t.Fatalf("catalog display_name = %v", catalogItem.DisplayName)
	}
	if catalogItem.Vendor == nil || *catalogItem.Vendor != vendorName {
		t.Fatalf("catalog vendor = %v", catalogItem.Vendor)
	}
	if catalogItem.ContextLength == nil || *catalogItem.ContextLength != 128000 {
		t.Fatalf("catalog context_length = %v", catalogItem.ContextLength)
	}
	if catalogItem.Priced || catalogItem.Mode != "per_token" {
		t.Fatalf("未定价 catalog 行 = %+v", catalogItem)
	}

	assertSources(channelOnly, PricingCandidateSourceChannel)
	assertSources(mappingOnly, PricingCandidateSourceChannel)

	pricedItem := assertSources(pricedOnly, PricingCandidateSourcePricing)
	if !pricedItem.Priced || pricedItem.Mode != "per_token" {
		t.Fatalf("priced 行 = %+v", pricedItem)
	}

	allItem := assertSources(all,
		PricingCandidateSourceCatalog, PricingCandidateSourceChannel, PricingCandidateSourcePricing)
	if !allItem.Priced || allItem.Mode != "per_request" {
		t.Fatalf("三来源行 = %+v", allItem)
	}

	// 关键字过滤：只命中目标模型名。
	filtered, err := repo.ListPricingCandidates(ctx, PricingCandidateFilter{Keyword: channelOnly, Limit: 10})
	if err != nil {
		t.Fatalf("keyword search: %v", err)
	}
	if len(filtered) != 1 || filtered[0].Model != channelOnly {
		t.Fatalf("keyword 结果 = %+v", filtered)
	}
}
