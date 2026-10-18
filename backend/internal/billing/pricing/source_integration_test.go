//go:build integration

package pricing

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"newtg/gateway/internal/contract"
)

// 本文件是数据库定价源的集成验收（真 PG，不设 NEWTG_TEST_DB_DSN 则跳过）：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_test?sslmode=disable' \
//	go test -tags=integration ./internal/billing/pricing/ -count=1
//
// 依赖已应用 M0 init 迁移（model_pricing / group_pricing / user_groups）。
// 所有写入使用带纳秒时间戳的唯一名字并在 Cleanup 删除，避免污染共享测试库。

func openTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("NEWTG_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("NEWTG_TEST_DB_DSN 未设置，跳过集成测试")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		// 集成测试不关心 GORM 的 SQL 日志，静默避免输出噪声。
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("打开测试库: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("取 sql.DB: %v", err)
	}
	pingCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(pingCtx); err != nil {
		t.Fatalf("ping 测试库: %v", err)
	}
	return db
}

// testPrefix 生成每次运行唯一的模型/分组名前缀。
func testPrefix(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("m3t2-%d", time.Now().UnixNano())
}

func cleanupPrefix(t *testing.T, db *gorm.DB, prefix string) {
	t.Helper()
	t.Cleanup(func() {
		// 用 LIKE 前缀匹配一次清掉本用例的全部行；删除失败只记录，
		// 不掩盖测试主断言（测试库可能已被并行会话持有）。
		if err := db.Exec(`DELETE FROM group_pricing WHERE model LIKE ?`, prefix+"%").Error; err != nil {
			t.Logf("cleanup group_pricing: %v", err)
		}
		if err := db.Exec(`DELETE FROM model_pricing WHERE model LIKE ?`, prefix+"%").Error; err != nil {
			t.Logf("cleanup model_pricing: %v", err)
		}
		if err := db.Exec(`DELETE FROM user_groups WHERE name LIKE ?`, prefix+"%").Error; err != nil {
			t.Logf("cleanup user_groups: %v", err)
		}
	})
}

func insertModelRow(t *testing.T, db *gorm.DB, model string, mode int16, expr *string,
	prompt, completion, cacheRead, cacheWrite *int64) {
	t.Helper()
	var exprVal, promptVal, completionVal, cacheReadVal, cacheWriteVal any
	if expr != nil {
		exprVal = *expr
	}
	if prompt != nil {
		promptVal = *prompt
	}
	if completion != nil {
		completionVal = *completion
	}
	if cacheRead != nil {
		cacheReadVal = *cacheRead
	}
	if cacheWrite != nil {
		cacheWriteVal = *cacheWrite
	}
	err := db.Exec(`
		INSERT INTO model_pricing
			(model, mode, billing_expr, prompt_price, completion_price, cache_read_price, cache_write_price, status)
		VALUES (?, ?, ?, ?, ?, ?, ?, 1)`,
		model, mode, exprVal, promptVal, completionVal, cacheReadVal, cacheWriteVal).Error
	if err != nil {
		t.Fatalf("插入 model_pricing(%s): %v", model, err)
	}
}

func insertDisabledModelRow(t *testing.T, db *gorm.DB, model string, prompt int64) {
	t.Helper()
	err := db.Exec(`
		INSERT INTO model_pricing (model, mode, prompt_price, completion_price, status)
		VALUES (?, 0, ?, ?, 2)`, model, prompt, prompt).Error
	if err != nil {
		t.Fatalf("插入停用 model_pricing(%s): %v", model, err)
	}
}

func insertGroup(t *testing.T, db *gorm.DB, name string, ratio int) {
	t.Helper()
	if err := db.Exec(`INSERT INTO user_groups (name, ratio, status) VALUES (?, ?, 1)`, name, ratio).Error; err != nil {
		t.Fatalf("插入 user_groups(%s): %v", name, err)
	}
}

func insertGroupPricing(t *testing.T, db *gorm.DB, group, model string, prompt, completion, cacheRead, cacheWrite int64) {
	t.Helper()
	err := db.Exec(`
		INSERT INTO group_pricing
			("group", model, prompt_price, completion_price, cache_read_price, cache_write_price, status)
		VALUES (?, ?, ?, ?, ?, ?, 1)`,
		group, model, prompt, completion, cacheRead, cacheWrite).Error
	if err != nil {
		t.Fatalf("插入 group_pricing(%s,%s): %v", group, model, err)
	}
}

func newTestSource(t *testing.T, db *gorm.DB) *DBSource {
	t.Helper()
	src, err := NewSource(db, nil)
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	return src
}

// TestIntegrationSourceModelGroupOverrideLayers 覆盖「模型价 → 分组倍率 →
// 分组覆盖价」三层与未知模型回落。
func TestIntegrationSourceModelGroupOverrideLayers(t *testing.T) {
	db := openTestDB(t)
	prefix := testPrefix(t)
	cleanupPrefix(t, db, prefix)

	model := prefix + "-ratio"
	group := prefix + "-vip"
	insertModelRow(t, db, model, 0, nil, ptr(int64(500)), ptr(int64(1500)), ptr(int64(100)), ptr(int64(1250)))
	insertGroup(t, db, group, 120)

	ctx := context.Background()

	// 第一层：模型价 + default 分组（倍率 1）。
	src := newTestSource(t, db)
	snap, err := src.Resolve(ctx, model, "")
	if err != nil {
		t.Fatalf("Resolve(default): %v", err)
	}
	if snap.Mode != contract.BillingModeRatio || snap.GroupRatio != 1 {
		t.Fatalf("默认分组快照错误: mode=%v group=%v", snap.Mode, snap.GroupRatio)
	}
	if snap.Price.ModelRatio != 500 || snap.Price.CompletionRatio != 1500 {
		t.Fatalf("模型价映射错误: %+v", snap.Price)
	}
	q, err := ComputeQuota(snap, contract.TokenParams{P: 1000, C: 2000}, nil)
	if err != nil || q != 4 {
		t.Fatalf("默认分组 quota=%d err=%v，期望 4", q, err)
	}

	// 第二层：分组倍率 120 = 1.2x。
	src = newTestSource(t, db) // 新实例避开 30s 缓存
	snap, err = src.Resolve(ctx, model, group)
	if err != nil {
		t.Fatalf("Resolve(group): %v", err)
	}
	if snap.GroupRatio != 1.2 {
		t.Fatalf("分组倍率=%v，期望 1.2", snap.GroupRatio)
	}
	if snap.Price.GroupRatioInfo.Group != group || snap.Price.GroupRatioInfo.GroupRatio != 1.2 {
		t.Fatalf("GroupRatioInfo 错误: %+v", snap.Price.GroupRatioInfo)
	}
	q, err = ComputeQuota(snap, contract.TokenParams{P: 1_000_000, C: 1_000_000}, nil)
	if err != nil || q != 2400 {
		t.Fatalf("分组倍率 quota=%d err=%v，期望 2400", q, err)
	}

	// 第三层：分组覆盖价替代「模型价 × 分组倍率」。
	insertGroupPricing(t, db, group, model, 100, 200, 50, 60)
	src = newTestSource(t, db)
	snap, err = src.Resolve(ctx, model, group)
	if err != nil {
		t.Fatalf("Resolve(override): %v", err)
	}
	if snap.GroupRatio != 1 {
		t.Fatalf("覆盖价生效时倍率应为 1（覆盖价是最终绝对价），got %v", snap.GroupRatio)
	}
	info := snap.Price.GroupRatioInfo
	if !info.HasSpecialRatio || info.GroupSpecialRatio != 1 || info.GroupRatio != 1.2 {
		t.Fatalf("覆盖价的 GroupRatioInfo 错误: %+v", info)
	}
	if snap.Price.ModelRatio != 100 || snap.Price.CompletionRatio != 200 ||
		snap.Price.CacheRatio != 50 || snap.Price.CacheCreationRatio != 60 {
		t.Fatalf("覆盖价映射错误: %+v", snap.Price)
	}
	q, err = ComputeQuota(snap, contract.TokenParams{P: 1_000_000, C: 1_000_000}, nil)
	if err != nil || q != 300 {
		t.Fatalf("覆盖价 quota=%d err=%v，期望 300", q, err)
	}
}

// TestIntegrationSourceUnknownAndDisabled 覆盖未知模型/停用模型回落默认价
// （brief 冻结：未知模型必须回落默认价，不得静默免费）。
func TestIntegrationSourceUnknownAndDisabled(t *testing.T) {
	db := openTestDB(t)
	prefix := testPrefix(t)
	cleanupPrefix(t, db, prefix)

	SetBuiltinBillingExpressions(nil)
	t.Cleanup(func() { SetBuiltinBillingExpressions(nil) })

	src := newTestSource(t, db)
	ctx := context.Background()

	unknown := prefix + "-unknown"
	snap, err := src.Resolve(ctx, unknown, "default")
	if err != nil {
		t.Fatalf("Resolve(unknown): %v", err)
	}
	wantPrompt := DefaultPromptPricePer1M()
	if snap.Mode != contract.BillingModeRatio || snap.Price.ModelRatio != wantPrompt {
		t.Fatalf("未知模型应回落默认价: mode=%v ratio=%v want=%v", snap.Mode, snap.Price.ModelRatio, wantPrompt)
	}
	if snap.Price.CompletionRatio != wantPrompt {
		t.Fatalf("未知模型补全应为默认族 1×输入价，got %v", snap.Price.CompletionRatio)
	}
	q, err := ComputeQuota(snap, contract.TokenParams{P: 1_000_000}, nil)
	if err != nil || q != int64(wantPrompt) {
		t.Fatalf("默认价 quota=%d err=%v，期望 %d", q, err, int64(wantPrompt))
	}

	// 停用行：同样回落默认价（路由层另行 404，本层不得按行内价格计费）。
	disabled := prefix + "-disabled"
	insertDisabledModelRow(t, db, disabled, 1)
	src = newTestSource(t, db)
	snap, err = src.Resolve(ctx, disabled, "default")
	if err != nil {
		t.Fatalf("Resolve(disabled): %v", err)
	}
	if snap.Price.ModelRatio != wantPrompt {
		t.Fatalf("停用模型应回落默认价，got %v", snap.Price.ModelRatio)
	}
}

func TestIntegrationSourcePerCallAndExpression(t *testing.T) {
	db := openTestDB(t)
	prefix := testPrefix(t)
	cleanupPrefix(t, db, prefix)
	restore := setQPU(t, 100)
	t.Cleanup(restore)

	ctx := context.Background()

	perCallModel := prefix + "-percall"
	insertModelRow(t, db, perCallModel, 1, nil, ptr(int64(150)), nil, nil, nil)
	src := newTestSource(t, db)
	snap, err := src.Resolve(ctx, perCallModel, "default")
	if err != nil {
		t.Fatalf("Resolve(percall): %v", err)
	}
	if snap.Mode != contract.BillingModePerCall || !snap.Price.UsePrice || snap.Price.ModelPrice != 150 {
		t.Fatalf("按次快照错误: mode=%v price=%+v", snap.Mode, snap.Price)
	}
	if q, err := ComputeQuota(snap, contract.TokenParams{}, nil); err != nil || q != 150 {
		t.Fatalf("按次 quota=%d err=%v，期望 150", q, err)
	}

	exprModel := prefix + "-expr"
	insertModelRow(t, db, exprModel, 0, ptr("p*2"), nil, nil, nil, nil)
	src = newTestSource(t, db)
	snap, err = src.Resolve(ctx, exprModel, "default")
	if err != nil {
		t.Fatalf("Resolve(expr): %v", err)
	}
	if snap.Mode != contract.BillingModeTieredExpr || snap.ExprString != "p*2" {
		t.Fatalf("表达式快照错误: mode=%v expr=%q", snap.Mode, snap.ExprString)
	}
	if len(snap.ExprHash) != 64 || strings.Trim(snap.ExprHash, "0123456789abcdef") != "" {
		t.Fatalf("ExprHash 应为 sha256 hex，got %q", snap.ExprHash)
	}
	if q, err := ComputeQuota(snap, contract.TokenParams{P: 1_000_000}, nil); err != nil || q != 200 {
		t.Fatalf("表达式 quota=%d err=%v，期望 200", q, err)
	}
}

// TestIntegrationSourceNullCompletionFamily 覆盖 R9 的 NULL 语义：
// completion_price NULL → 按模型族硬编码倍率兜底；显式 0 → 保留免费。
func TestIntegrationSourceNullCompletionFamily(t *testing.T) {
	db := openTestDB(t)
	prefix := testPrefix(t)
	cleanupPrefix(t, db, prefix)

	// 名字以 claude-sonnet-4 开头，命中 locked 族（补全 5x）与 claude 1h 系数。
	// 该名字不以 testPrefix 开头，需单独清理。
	model := "claude-sonnet-4-m3t2-" + strings.TrimPrefix(prefix, "m3t2-")
	t.Cleanup(func() {
		if err := db.Exec(`DELETE FROM model_pricing WHERE model = ?`, model).Error; err != nil {
			t.Logf("cleanup %s: %v", model, err)
		}
	})
	insertModelRow(t, db, model, 0, nil, ptr(int64(1000)), nil, nil, nil)

	src := newTestSource(t, db)
	snap, err := src.Resolve(context.Background(), model, "default")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if snap.Price.CompletionRatio != 5000 {
		t.Fatalf("NULL 补全应兜底 5×输入价=5000，got %v", snap.Price.CompletionRatio)
	}
	if snap.Price.CacheRatio != 1000 {
		t.Fatalf("NULL 缓存读应兜底 1×输入价=1000，got %v", snap.Price.CacheRatio)
	}
	if snap.Price.CacheCreationRatio != 1250 {
		t.Fatalf("NULL 缓存写应兜底 1.25×输入价=1250，got %v", snap.Price.CacheCreationRatio)
	}
	if snap.Price.CacheCreation1hRatio != 2000 {
		t.Fatalf("claude 1h 应为 1.6×基准=2000，got %v", snap.Price.CacheCreation1hRatio)
	}
}

func TestIntegrationSourceExplicitZeroAndFreeInput(t *testing.T) {
	db := openTestDB(t)
	prefix := testPrefix(t)
	cleanupPrefix(t, db, prefix)

	// 输入价显式 0、输出价 >0：必须只收输出费。
	model := prefix + "-zeroprompt"
	insertModelRow(t, db, model, 0, nil, ptr(int64(0)), ptr(int64(500)), ptr(int64(0)), ptr(int64(0)))
	src := newTestSource(t, db)
	snap, err := src.Resolve(context.Background(), model, "default")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if snap.Price.ModelRatio != 0 || snap.Price.CompletionRatio != 500 {
		t.Fatalf("显式零被覆盖: %+v", snap.Price)
	}
	if snap.Price.CacheCreation5mRatio != 0 || snap.Price.CacheCreation1hRatio != 0 {
		t.Fatalf("显式零缓存写应保留: %+v", snap.Price)
	}
	q, err := ComputeQuota(snap, contract.TokenParams{P: 1_000_000, C: 1_000_000}, nil)
	if err != nil || q != 500 {
		t.Fatalf("免费输入 quota=%d err=%v，期望 500", q, err)
	}
}

func TestIntegrationSourceQueryFailureReturnsError(t *testing.T) {
	db := openTestDB(t)
	src := newTestSource(t, db)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 取消后查询必然失败；不得静默回退免费价。

	_, err := src.Resolve(ctx, "m3t2-never-cached-query-failure", "default")
	if err == nil {
		t.Fatal("查询失败必须返回 error")
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatal("查询失败不应被当作「模型不存在」")
	}
}

func TestIntegrationSourceCacheAndInvalidate(t *testing.T) {
	db := openTestDB(t)
	prefix := testPrefix(t)
	cleanupPrefix(t, db, prefix)

	model := prefix + "-cache"
	insertModelRow(t, db, model, 0, nil, ptr(int64(100)), ptr(int64(100)), ptr(int64(100)), ptr(int64(100)))
	src := newTestSource(t, db)
	ctx := context.Background()

	snap, err := src.Resolve(ctx, model, "default")
	if err != nil || snap.Price.ModelRatio != 100 {
		t.Fatalf("首次 Resolve: %+v err=%v", snap.Price, err)
	}

	// 更新 DB 后缓存仍生效（30s TTL 的保守上界）。
	if err := db.Exec(`UPDATE model_pricing SET prompt_price = 200 WHERE model = ?`, model).Error; err != nil {
		t.Fatal(err)
	}
	snap, err = src.Resolve(ctx, model, "default")
	if err != nil || snap.Price.ModelRatio != 100 {
		t.Fatalf("缓存期内应沿用旧值: %v err=%v", snap.Price.ModelRatio, err)
	}

	// Invalidate 后立即生效。
	src.Invalidate()
	snap, err = src.Resolve(ctx, model, "default")
	if err != nil || snap.Price.ModelRatio != 200 {
		t.Fatalf("Invalidate 后应读新值: %v err=%v", snap.Price.ModelRatio, err)
	}
}

// TestIntegrationSourceConcurrentResolve 是缓存并发安全的冒烟用例
// （配合 -race 更有意义；默认 go test 也能发现 map 并发写 panic）。
func TestIntegrationSourceConcurrentResolve(t *testing.T) {
	db := openTestDB(t)
	prefix := testPrefix(t)
	cleanupPrefix(t, db, prefix)

	model := prefix + "-concurrent"
	insertModelRow(t, db, model, 0, nil, ptr(int64(500)), ptr(int64(1500)), ptr(int64(100)), ptr(int64(1250)))
	src := newTestSource(t, db)

	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if _, err := src.Resolve(context.Background(), model, "default"); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("并发 Resolve 失败: %v", err)
	}
}
