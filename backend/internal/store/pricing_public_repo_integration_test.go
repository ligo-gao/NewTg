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
	channelName := fmt.Sprintf("t7-pub-ch-%d", suffix)

	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM channels WHERE name = $1`, channelName)
		_, _ = db.Exec(`DELETE FROM group_pricing WHERE model IN ($1, $2)`, visible, disabled)
		_, _ = db.Exec(`DELETE FROM model_pricing WHERE model IN ($1, $2, $3)`,
			visible, disabled, expression)
		_, _ = db.Exec(`DELETE FROM user_groups WHERE name = $1`, group)
	})

	// 公示集合现在要求「至少一个 status=1 的渠道支持该模型」，因此可见模型
	// 必须配一个启用渠道；否则它会被渠道可用性谓词过滤掉。
	if _, err := db.Exec(`INSERT INTO channels
		(name, type, credentials, supported_models, status)
		VALUES ($1, 'openai', $2, jsonb_build_array($3::text), 1)`,
		channelName, []byte{0}, visible); err != nil {
		t.Fatalf("insert channels: %v", err)
	}

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
	channelName := fmt.Sprintf("t6-pub-strategy-ch-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM channels WHERE name = $1`, channelName)
		_, _ = db.Exec(`DELETE FROM model_pricing WHERE model = $1`, model)
	})

	if _, err := db.Exec(`INSERT INTO model_pricing (model, mode, prompt_price, author, status)
		VALUES ($1, 0, 250, 't6', 1)`, model); err != nil {
		t.Fatalf("insert model_pricing: %v", err)
	}
	// 公开目录要求模型仍有可用渠道，否则快照里看不到它（F-4）。
	if _, err := db.Exec(`INSERT INTO channels
		(name, type, credentials, supported_models, status)
		VALUES ($1, 'openai', $2, jsonb_build_array($3::text), 1)`,
		channelName, []byte{0}, model); err != nil {
		t.Fatalf("insert channel: %v", err)
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
	channelName := fmt.Sprintf("t7-meta-ch-%d", suffix)

	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM channels WHERE name = $1`, channelName)
		_, _ = db.Exec(`DELETE FROM model_meta WHERE model_name = $1`, model)
		_, _ = db.Exec(`DELETE FROM vendors WHERE name = $1`, vendorName)
		_, _ = db.Exec(`DELETE FROM model_pricing WHERE model = $1`, model)
	})

	if _, err := db.Exec(`INSERT INTO model_pricing (model, mode, prompt_price, vendor, author, status)
		VALUES ($1, 0, 100, $2, 't7', 1)`, model, vendorValue); err != nil {
		t.Fatalf("insert model_pricing: %v", err)
	}
	// 有可用渠道才会进入公开快照（F-4），否则 Models 断言会落空。
	if _, err := db.Exec(`INSERT INTO channels
		(name, type, credentials, supported_models, status)
		VALUES ($1, 'openai', $2, jsonb_build_array($3::text), 1)`,
		channelName, []byte{0}, model); err != nil {
		t.Fatalf("insert channel: %v", err)
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

// TestListPublicChatModelsIntegration 冻结计划 Task 3 的探测集合口径：
// 可见集合（status=1 且无表达式）∩ 对话能力。
//   - 有 chat 端点 ⇒ 收入；
//   - 无档案 ⇒ 保守收入（Review Focus 4：不得静默丢弃）；
//   - 只有 embeddings 端点 ⇒ 剔除；
//   - 停用档案 exact 命中 ⇒ 剔除（D1-13 与公开目录同规则）。
func TestListPublicChatModelsIntegration(t *testing.T) {
	db := openPublicPricingTestDB(t)
	repo := NewPublicPricingRepo(db)
	ctx := context.Background()

	suffix := time.Now().UnixNano()
	chat := fmt.Sprintf("t7-chat-%d", suffix)
	embedding := fmt.Sprintf("t7-embed-%d", suffix)
	noMeta := fmt.Sprintf("t7-nometa-%d", suffix)
	hidden := fmt.Sprintf("t7-hidden-%d", suffix)
	channelName := fmt.Sprintf("t7-chat-ch-%d", suffix)

	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM channels WHERE name = $1`, channelName)
		_, _ = db.Exec(`DELETE FROM model_meta WHERE model_name IN ($1, $2, $3)`, chat, embedding, hidden)
		_, _ = db.Exec(`DELETE FROM model_pricing WHERE model IN ($1, $2, $3, $4)`,
			chat, embedding, noMeta, hidden)
	})

	for _, model := range []string{chat, embedding, noMeta, hidden} {
		if _, err := db.Exec(`INSERT INTO model_pricing (model, mode, prompt_price, author, status)
			VALUES ($1, 0, 100, 't7', 1)`, model); err != nil {
			t.Fatalf("insert model_pricing(%s): %v", model, err)
		}
	}
	// 四个模型都给可用渠道：本用例验证的是「端点/档案」过滤，渠道可用性必须
	// 保持全真，否则剔除原因会被渠道条件掩盖（F-4）。
	if _, err := db.Exec(`INSERT INTO channels
		(name, type, credentials, supported_models, status)
		VALUES ($1, 'openai', $2,
			jsonb_build_array($3::text, $4::text, $5::text, $6::text), 1)`,
		channelName, []byte{0}, chat, embedding, noMeta, hidden); err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	for _, row := range []struct {
		name      string
		endpoints string
		status    int16
	}{
		{chat, `{"/v1/chat/completions":["POST"]}`, 1},
		{embedding, `{"/v1/embeddings":["POST"]}`, 1},
		{hidden, `{"/v1/chat/completions":["POST"]}`, 2},
	} {
		if _, err := db.Exec(`INSERT INTO model_meta (model_name, endpoints, status, name_rule)
			VALUES ($1, $2::jsonb, $3, 0)`, row.name, row.endpoints, row.status); err != nil {
			t.Fatalf("insert model_meta(%s): %v", row.name, err)
		}
	}

	got, err := repo.ListPublicChatModels(ctx)
	if err != nil {
		t.Fatalf("ListPublicChatModels: %v", err)
	}
	set := make(map[string]bool, len(got))
	for _, name := range got {
		set[name] = true
	}
	if !set[chat] || !set[noMeta] {
		t.Fatalf("chat models must include %s and %s: %v", chat, noMeta, got)
	}
	if set[embedding] || set[hidden] {
		t.Fatalf("non-chat / disabled-profile models leaked in: %v", got)
	}
}

// TestPublicPricingRepoChannelAvailability 冻结 F-4 的目录可见性口径：
//   - 目录与计数只包含「至少一个 status=1 渠道能服务」的模型，命中路径覆盖
//     supported_models 与 model_mapping 两支（与路由 candidatesQuery 同源）；
//   - 手动禁用（2）与自动禁用（3）都会隐藏模型，重新启用即恢复；
//   - 渠道状态变化必须改变 publicPricingVersion，否则客户端会拿旧 ETag 命中
//     304，继续展示已被隐藏的模型。
func TestPublicPricingRepoChannelAvailability(t *testing.T) {
	db := openPublicPricingTestDB(t)
	repo := NewPublicPricingRepo(db)
	ctx := context.Background()

	suffix := time.Now().UnixNano()
	supported := fmt.Sprintf("t7-avail-supported-%d", suffix)
	mapped := fmt.Sprintf("t7-avail-mapped-%d", suffix)
	channelName := fmt.Sprintf("t7-avail-ch-%d", suffix)

	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM channels WHERE name = $1`, channelName)
		_, _ = db.Exec(`DELETE FROM model_pricing WHERE model IN ($1, $2)`, supported, mapped)
	})

	for _, model := range []string{supported, mapped} {
		if _, err := db.Exec(`INSERT INTO model_pricing (model, mode, prompt_price, author, status)
			VALUES ($1, 0, 100, 't7', 1)`, model); err != nil {
			t.Fatalf("insert model_pricing(%s): %v", model, err)
		}
	}
	// supported_models 命中 supported；model_mapping 的同名键命中 mapped。
	if _, err := db.Exec(`INSERT INTO channels
		(name, type, credentials, supported_models, model_mapping, status)
		VALUES ($1, 'openai', $2, jsonb_build_array($3::text), $4::jsonb, 1)`,
		channelName, []byte{0}, supported,
		fmt.Sprintf(`{"%s":"upstream-real"}`, mapped)); err != nil {
		t.Fatalf("insert channel: %v", err)
	}

	visibleSet := func(catalog PublicPricingCatalog) map[string]bool {
		set := make(map[string]bool, len(catalog.Models))
		for _, rec := range catalog.Models {
			set[rec.Model] = true
		}
		return set
	}

	before, err := repo.LoadPublicPricingCatalog(ctx)
	if err != nil {
		t.Fatalf("LoadPublicPricingCatalog(before): %v", err)
	}
	beforeCounts, err := repo.LoadPublicCounts(ctx)
	if err != nil {
		t.Fatalf("LoadPublicCounts(before): %v", err)
	}
	set := visibleSet(before)
	if !set[supported] || !set[mapped] {
		t.Fatalf("supported_models 与 model_mapping 两条命中路径都应可见: supported=%v mapped=%v",
			set[supported], set[mapped])
	}
	if beforeCounts.Models != int64(len(before.Models)) {
		t.Fatalf("计数与目录不同口径: counts=%d items=%d", beforeCounts.Models, len(before.Models))
	}
	for _, model := range []string{supported, mapped} {
		ok, err := repo.HasRoutableChannel(ctx, model)
		if err != nil || !ok {
			t.Fatalf("HasRoutableChannel(%s) = %v, %v；期望 true", model, ok, err)
		}
	}
	if ok, err := repo.HasRoutableChannel(ctx, fmt.Sprintf("t7-avail-absent-%d", suffix)); err != nil || ok {
		t.Fatalf("没有任何渠道的模型不应可路由: ok=%v err=%v", ok, err)
	}

	// 手动禁用：目录与计数同步收缩，版本必须变化（否则 304 会继续回放旧目录）。
	if _, err := db.Exec(`UPDATE channels SET status = 2 WHERE name = $1`, channelName); err != nil {
		t.Fatalf("disable channel: %v", err)
	}
	disabled, err := repo.LoadPublicPricingCatalog(ctx)
	if err != nil {
		t.Fatalf("LoadPublicPricingCatalog(disabled): %v", err)
	}
	disabledCounts, err := repo.LoadPublicCounts(ctx)
	if err != nil {
		t.Fatalf("LoadPublicCounts(disabled): %v", err)
	}
	set = visibleSet(disabled)
	if set[supported] || set[mapped] {
		t.Fatalf("渠道禁用后模型仍出现在目录: supported=%v mapped=%v", set[supported], set[mapped])
	}
	if disabled.Version == before.Version {
		t.Fatal("渠道禁用后 publicPricingVersion 未改变")
	}
	if disabledCounts.Models != beforeCounts.Models-2 {
		t.Fatalf("渠道禁用后模型计数 = %d，期望 %d", disabledCounts.Models, beforeCounts.Models-2)
	}
	if ok, err := repo.HasRoutableChannel(ctx, supported); err != nil || ok {
		t.Fatalf("禁用后 HasRoutableChannel = %v, %v；期望 false", ok, err)
	}

	// 自动禁用（status=3）同样不可用：三态里只有 1 是可调度。
	if _, err := db.Exec(`UPDATE channels SET status = 3 WHERE name = $1`, channelName); err != nil {
		t.Fatalf("auto-disable channel: %v", err)
	}
	autoDisabled, err := repo.LoadPublicPricingCatalog(ctx)
	if err != nil {
		t.Fatalf("LoadPublicPricingCatalog(auto-disabled): %v", err)
	}
	if visibleSet(autoDisabled)[supported] {
		t.Fatal("自动禁用后模型仍出现在目录")
	}
	if autoDisabled.Version == disabled.Version {
		t.Fatal("自动禁用后 publicPricingVersion 未改变")
	}

	// 重新启用：目录、计数与版本都恢复可见性。
	if _, err := db.Exec(`UPDATE channels SET status = 1 WHERE name = $1`, channelName); err != nil {
		t.Fatalf("re-enable channel: %v", err)
	}
	restored, err := repo.LoadPublicPricingCatalog(ctx)
	if err != nil {
		t.Fatalf("LoadPublicPricingCatalog(restored): %v", err)
	}
	restoredCounts, err := repo.LoadPublicCounts(ctx)
	if err != nil {
		t.Fatalf("LoadPublicCounts(restored): %v", err)
	}
	set = visibleSet(restored)
	if !set[supported] || !set[mapped] {
		t.Fatalf("重新启用后模型未恢复: supported=%v mapped=%v", set[supported], set[mapped])
	}
	if restoredCounts.Models != beforeCounts.Models {
		t.Fatalf("重新启用后模型计数 = %d，期望 %d", restoredCounts.Models, beforeCounts.Models)
	}
	if restored.Version == autoDisabled.Version {
		t.Fatal("重新启用后 publicPricingVersion 未改变")
	}
}
