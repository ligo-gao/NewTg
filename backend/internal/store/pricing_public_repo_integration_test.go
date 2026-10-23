//go:build integration

package store

// M6c Task 7 公开定价仓储的数据库级验收：真实 PostgreSQL。
//
// 覆盖单测无法证明的真相：
//   - 可见集合（status=1 且 billing_expr IS NULL）由 SQL 执行；
//   - group_pricing 的 JOIN 只带出「源模型可见」的覆盖价；
//   - 版本号由三张表派生，并且在定价变更后改变；
//   - LoadPublicCounts 与目录可见集合同口径。
//
// 运行（隔离库，先 migrate up）：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_test_verify_t7?sslmode=disable' \
//	  go test -tags=integration ./internal/store/ -run TestPublicPricingRepo -v
//
// 测试只插入/删除自己命名的行，不做 DROP/TRUNCATE（共享库不清理其他包的数据）。

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func openPublicPricingTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("NEWTG_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("NEWTG_TEST_DB_DSN 未设置，跳过集成测试")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		t.Fatalf("ping test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestPublicPricingRepoVisibleSetAndVersion(t *testing.T) {
	db := openPublicPricingTestDB(t)
	repo := NewPublicPricingRepo(db)
	ctx := context.Background()

	suffix := time.Now().UnixNano()
	visible := fmt.Sprintf("t7-pub-visible-%d", suffix)
	disabled := fmt.Sprintf("t7-pub-disabled-%d", suffix)
	expression := fmt.Sprintf("t7-pub-expr-%d", suffix)
	group := fmt.Sprintf("t7g%d", suffix%1_000_000_000)

	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM group_pricing WHERE model IN ($1, $2)`, visible, disabled)
		_, _ = db.Exec(`DELETE FROM model_pricing WHERE model IN ($1, $2, $3)`,
			visible, disabled, expression)
		_, _ = db.Exec(`DELETE FROM user_groups WHERE name = $1`, group)
	})

	// 分组必须先于覆盖价（FK），模型覆盖价引用逻辑模型名。
	if _, err := db.Exec(`INSERT INTO user_groups (name, ratio, user_selectable, status)
		VALUES ($1, 120, true, 1)`, group); err != nil {
		t.Fatalf("insert user_groups: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO model_pricing
		(model, mode, prompt_price, completion_price, cache_read_price, cache_write_price, author, status)
		VALUES ($1, 0, 250, 900, 25, 50, 't7', 1)`, visible); err != nil {
		t.Fatalf("insert visible model: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO model_pricing
		(model, mode, prompt_price, author, status) VALUES ($1, 0, 10, 't7', 2)`, disabled); err != nil {
		t.Fatalf("insert disabled model: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO model_pricing
		(model, mode, billing_expr, author, status) VALUES ($1, 0, '{"mode":"tiered_expr"}', 't7', 1)`,
		expression); err != nil {
		t.Fatalf("insert expression model: %v", err)
	}
	for _, model := range []string{visible, disabled} {
		if _, err := db.Exec(`INSERT INTO group_pricing
			("group", model, prompt_price, completion_price, cache_read_price, cache_write_price, status)
			VALUES ($1, $2, 300, 1000, 30, 60, 1)`, group, model); err != nil {
			t.Fatalf("insert group_pricing(%s): %v", model, err)
		}
	}

	first, err := repo.LoadPublicPricingCatalog(ctx)
	if err != nil {
		t.Fatalf("LoadPublicPricingCatalog: %v", err)
	}
	if first.Version == "" || len(first.Version) != 16 {
		t.Fatalf("版本号格式错误: %q", first.Version)
	}

	byModel := make(map[string]PublicModelPricing, len(first.Models))
	for _, model := range first.Models {
		byModel[model.Model] = model
	}
	if row, ok := byModel[visible]; !ok || row.PromptPrice == nil || *row.PromptPrice != 250 {
		t.Fatalf("可见模型缺失或价格错误: %+v", row)
	}
	if _, ok := byModel[disabled]; ok {
		t.Fatal("停用模型不得出现在公示集合")
	}
	if _, ok := byModel[expression]; ok {
		t.Fatal("表达式计价模型不得出现在公示集合（MVP 排除）")
	}

	// 覆盖价只带出可见源模型的那些。
	overrideByModel := make(map[string]PublicGroupPricing)
	for _, override := range first.Overrides {
		overrideByModel[override.Model] = override
	}
	if _, ok := overrideByModel[visible]; !ok {
		t.Fatal("可见模型的覆盖价缺失")
	}
	if _, ok := overrideByModel[disabled]; ok {
		t.Fatal("停用模型的覆盖价不得带出（JOIN 源模型可见性）")
	}

	// 计数与目录同口径。
	counts, err := repo.LoadPublicCounts(ctx)
	if err != nil {
		t.Fatalf("LoadPublicCounts: %v", err)
	}
	if counts.Models != int64(len(first.Models)) {
		t.Fatalf("models 计数 %d != 可见集合 %d", counts.Models, len(first.Models))
	}
	var upstreams int64
	if err := db.QueryRowContext(ctx,
		`SELECT count(DISTINCT type) FROM channels WHERE status = 1`).Scan(&upstreams); err != nil {
		t.Fatalf("direct upstream count: %v", err)
	}
	if counts.Upstreams != upstreams {
		t.Fatalf("upstreams 计数 %d != 直接查询 %d", counts.Upstreams, upstreams)
	}

	// 改价后版本必须改变（ETag 失效链路）。
	if _, err := db.Exec(`UPDATE model_pricing SET prompt_price = 251, updated_at = now() WHERE model = $1`,
		visible); err != nil {
		t.Fatalf("update visible model: %v", err)
	}
	second, err := repo.LoadPublicPricingCatalog(ctx)
	if err != nil {
		t.Fatalf("reload catalog: %v", err)
	}
	if second.Version == first.Version {
		t.Fatalf("定价变更后版本号未改变（仍为 %s）", first.Version)
	}

	// 快照内生成时间不为零。
	if second.GeneratedAt.IsZero() {
		t.Fatal("GeneratedAt 为零值")
	}
}

// TestPublicPricingRepoStrategyFieldAndVersion 冻结 Task 6 的两条真相：
//  1. strategy 列随可见集合返回（handler 据此生成 pricing_schedule）；
//  2. 只改策略内容也必须改变版本号——否则公开页的 If-None-Match 会继续
//     返回旧时段表。
func TestPublicPricingRepoStrategyFieldAndVersion(t *testing.T) {
	db := openPublicPricingTestDB(t)
	repo := NewPublicPricingRepo(db)
	ctx := context.Background()

	model := fmt.Sprintf("t6-pub-strategy-%d", time.Now().UnixNano())
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM model_pricing WHERE model = $1`, model) })

	if _, err := db.Exec(`INSERT INTO model_pricing (model, mode, prompt_price, author, status)
		VALUES ($1, 0, 250, 't6', 1)`, model); err != nil {
		t.Fatalf("insert model_pricing: %v", err)
	}
	baseline, err := repo.LoadPublicPricingCatalog(ctx)
	if err != nil {
		t.Fatalf("baseline Load: %v", err)
	}

	var strategy []byte
	for _, row := range baseline.Models {
		if row.Model == model {
			strategy = row.Strategy
		}
	}
	if len(strategy) != 0 {
		t.Fatalf("未配置策略时 Strategy 应为 nil: %s", strategy)
	}

	if _, err := db.Exec(`UPDATE model_pricing
		SET strategy = '{"version":1,"time":{"enabled":true,"timezone":"Asia/Shanghai","holiday_policy":"none","default_multiplier":0.5,"windows":[]}}'::jsonb
		WHERE model = $1`, model); err != nil {
		t.Fatalf("set strategy: %v", err)
	}
	afterStrategy, err := repo.LoadPublicPricingCatalog(ctx)
	if err != nil {
		t.Fatalf("Load after strategy: %v", err)
	}
	if afterStrategy.Version == baseline.Version {
		t.Fatal("策略变更后版本号未改变")
	}
	found := false
	for _, row := range afterStrategy.Models {
		if row.Model == model {
			found = true
			if len(row.Strategy) == 0 {
				t.Fatal("strategy 列未随快照返回")
			}
		}
	}
	if !found {
		t.Fatal("模型不在公开快照中")
	}
}

// TestPublicPricingRepoMetadataSnapshot 冻结 D1-22 的两条真相：
//  1. model_meta（含 status=2）与 vendors 与定价三表在同一快照返回；
//  2. 两表的编辑都会改变版本号——否则公开页的 If-None-Match 会持续 304。
func TestPublicPricingRepoMetadataSnapshot(t *testing.T) {
	db := openPublicPricingTestDB(t)
	repo := NewPublicPricingRepo(db)
	ctx := context.Background()

	suffix := time.Now().UnixNano()
	model := fmt.Sprintf("t7-meta-model-%d", suffix)
	vendorName := fmt.Sprintf("t7-meta-vendor-%d", suffix)
	vendorValue := fmt.Sprintf("t7-meta-priced-vendor-%d", suffix)

	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM model_meta WHERE model_name = $1`, model)
		_, _ = db.Exec(`DELETE FROM vendors WHERE name = $1`, vendorName)
		_, _ = db.Exec(`DELETE FROM model_pricing WHERE model = $1`, model)
	})

	if _, err := db.Exec(`INSERT INTO model_pricing (model, mode, prompt_price, vendor, author, status)
		VALUES ($1, 0, 100, $2, 't7', 1)`, model, vendorValue); err != nil {
		t.Fatalf("insert model_pricing: %v", err)
	}
	baseline, err := repo.LoadPublicPricingCatalog(ctx)
	if err != nil {
		t.Fatalf("baseline Load: %v", err)
	}

	var vendorID int64
	if err := db.QueryRowContext(ctx,
		`INSERT INTO vendors (name) VALUES ($1) RETURNING id`, vendorName).Scan(&vendorID); err != nil {
		t.Fatalf("insert vendor: %v", err)
	}
	afterVendor, err := repo.LoadPublicPricingCatalog(ctx)
	if err != nil {
		t.Fatalf("Load after vendor: %v", err)
	}
	if afterVendor.Version == baseline.Version {
		t.Fatal("vendors 变更后版本号未改变（D1-22）")
	}
	foundVendor := false
	for _, vendor := range afterVendor.Vendors {
		if vendor.ID == vendorID && vendor.Name == vendorName && vendor.Status == 1 {
			foundVendor = true
		}
	}
	if !foundVendor {
		t.Fatalf("vendors 快照未包含新供应商: %+v", afterVendor.Vendors)
	}

	// 停用档案也必须被加载：D1-13 依赖它判断「精确命中停用档案」。
	if _, err := db.Exec(`INSERT INTO model_meta
		(model_name, display_name, vendor_id, capabilities, context_length, status, name_rule)
		VALUES ($1, 'Hidden Meta', $2, '["text"]'::jsonb, 128000, 2, 0)`,
		model, vendorID); err != nil {
		t.Fatalf("insert model_meta: %v", err)
	}
	afterMeta, err := repo.LoadPublicPricingCatalog(ctx)
	if err != nil {
		t.Fatalf("Load after meta: %v", err)
	}
	if afterMeta.Version == afterVendor.Version {
		t.Fatal("model_meta 变更后版本号未改变（D1-22）")
	}
	foundMeta := false
	for _, rec := range afterMeta.Meta {
		if rec.ModelName == model {
			foundMeta = true
			if rec.Status != 2 || rec.VendorID == nil || *rec.VendorID != vendorID {
				t.Fatalf("model_meta 投影错误: %+v", rec)
			}
		}
	}
	if !foundMeta {
		t.Fatalf("model_meta 快照未包含停用档案: %+v", afterMeta.Meta)
	}

	// model_pricing.vendor 作为 D1-16 的第二级回退也必须随定价快照返回。
	var pricedVendor *string
	for _, row := range afterMeta.Models {
		if row.Model == model {
			pricedVendor = row.Vendor
		}
	}
	if pricedVendor == nil || *pricedVendor != vendorValue {
		t.Fatalf("model_pricing.vendor 未随快照返回: %v", pricedVendor)
	}

	// 编辑档案同样必须改变版本（不只是新增行）。
	if _, err := db.Exec(`UPDATE model_meta SET display_name = 'Edited', updated_at = now()
		WHERE model_name = $1`, model); err != nil {
		t.Fatalf("update model_meta: %v", err)
	}
	afterEdit, err := repo.LoadPublicPricingCatalog(ctx)
	if err != nil {
		t.Fatalf("Load after meta edit: %v", err)
	}
	if afterEdit.Version == afterMeta.Version {
		t.Fatal("model_meta 编辑后版本号未改变（D1-22）")
	}
}
