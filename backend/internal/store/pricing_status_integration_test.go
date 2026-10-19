//go:build integration

package store

// F4：ModelPricingStatus 的候选顺序验收（R-84）。
//
// 该查询必须与 pricing.DBSource.Resolve 的候选解析完全一致，否则「管理员停用
// 了别名/基名行」时用户请求会绕过 404：
//   1. 规范名（显式映射）优先；
//   2. 去修饰后的原名（精确名）；
//   3. 基名（-latest / -thinking 归一）兜底；
//   4. 全无行 → found=false（按默认价兜底，视为启用）。
//
// 运行：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_test?sslmode=disable' \
//	go test -tags=integration ./internal/store/ -run TestIntegrationModelPricingStatus -v

import (
	"context"
	"fmt"
	"testing"
	"time"

	"newtg/gateway/internal/billing/pricing"
)

func insertPricingStatusRow(t *testing.T, repo *SQLPricingRepo, model string, status int16) {
	t.Helper()
	if _, err := repo.db.Exec(
		`INSERT INTO model_pricing (model, mode, prompt_price, author, status)
		 VALUES ($1, 1, 100, 't9-test', $2)`, model, status); err != nil {
		t.Fatalf("insert model_pricing(%s): %v", model, err)
	}
}

func cleanupPricingStatusRows(t *testing.T, repo *SQLPricingRepo, models ...string) {
	t.Helper()
	t.Cleanup(func() {
		for _, model := range models {
			_, _ = repo.db.Exec(`DELETE FROM model_pricing WHERE model = $1`, model)
		}
	})
}

func TestIntegrationModelPricingStatusCandidateOrder(t *testing.T) {
	db := openTestDB(t)
	repo := NewPricingRepo(db)
	ctx := context.Background()

	t.Run("精确名优先于基名", func(t *testing.T) {
		base := fmt.Sprintf("exact-%d", time.Now().UnixNano())
		alias := base + "-latest"
		insertPricingStatusRow(t, repo, alias, 2) // 精确行停用
		insertPricingStatusRow(t, repo, base, 1)  // 基名启用
		cleanupPricingStatusRows(t, repo, alias, base)

		status, found, err := repo.ModelPricingStatus(ctx, alias)
		if err != nil {
			t.Fatalf("ModelPricingStatus(%s): %v", alias, err)
		}
		if !found || status != 2 {
			t.Fatalf("status = (%d, %v), want (2, true)（精确名必须先于基名）", status, found)
		}
	})

	t.Run("精确名无行时回退基名", func(t *testing.T) {
		base := fmt.Sprintf("based-%d", time.Now().UnixNano())
		insertPricingStatusRow(t, repo, base, 2)
		cleanupPricingStatusRows(t, repo, base)

		status, found, err := repo.ModelPricingStatus(ctx, base+"-latest")
		if err != nil {
			t.Fatalf("ModelPricingStatus: %v", err)
		}
		if !found || status != 2 {
			t.Fatalf("status = (%d, %v), want (2, true)（-latest 必须回退到基名行）", status, found)
		}
	})

	t.Run("规范名候选优先", func(t *testing.T) {
		suffix := fmt.Sprintf("%d", time.Now().UnixNano())
		alias := "alias-" + suffix
		canonical := "canonical-" + suffix
		pricing.SetCanonicalBillingModelNames(map[string]string{alias: canonical})
		t.Cleanup(func() { pricing.SetCanonicalBillingModelNames(map[string]string{}) })
		insertPricingStatusRow(t, repo, canonical, 2)
		cleanupPricingStatusRows(t, repo, canonical)

		status, found, err := repo.ModelPricingStatus(ctx, alias)
		if err != nil {
			t.Fatalf("ModelPricingStatus(%s): %v", alias, err)
		}
		if !found || status != 2 {
			t.Fatalf("status = (%d, %v), want (2, true)（规范名行必须被候选命中）", status, found)
		}
	})

	t.Run("无任何候选行视为启用", func(t *testing.T) {
		status, found, err := repo.ModelPricingStatus(ctx, fmt.Sprintf("missing-%d", time.Now().UnixNano()))
		if err != nil {
			t.Fatalf("ModelPricingStatus: %v", err)
		}
		if found {
			t.Fatalf("found = true (status %d), want false（无定价行按默认价兜底）", status)
		}
	})

	t.Run("候选优先级高于同时存在的低优先行", func(t *testing.T) {
		base := fmt.Sprintf("prio-%d", time.Now().UnixNano())
		alias := base + "-latest"
		insertPricingStatusRow(t, repo, alias, 1) // 精确行启用
		insertPricingStatusRow(t, repo, base, 2)  // 基名停用
		cleanupPricingStatusRows(t, repo, alias, base)

		status, found, err := repo.ModelPricingStatus(ctx, alias)
		if err != nil {
			t.Fatalf("ModelPricingStatus: %v", err)
		}
		if !found || status != 1 {
			t.Fatalf("status = (%d, %v), want (1, true)（存在精确行时不得读基名行）", status, found)
		}
	})
}
