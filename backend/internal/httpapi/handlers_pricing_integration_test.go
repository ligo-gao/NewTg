//go:build integration

package httpapi

// M3 Task 7 的数据库级验收：真实 PostgreSQL + 真实 store.SQLPricingRepo。
// 覆盖单元测试无法证明的真相：
//   - model_pricing / group_pricing 的 UNIQUE 与 FK 行为；
//   - updated_at 乐观锁在 SQL 层（WHERE updated_at = $n）确实生效；
//   - 删除模型时覆盖价同事务级联清理；
//   - 保存的表达式行能被 pricing.DBSource 解析（管理面与计费读取同一口径）。
//
// 运行：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_test?sslmode=disable' \
//	  go test -tags=integration ./internal/httpapi/ -run TestPricingIntegration -v
//
// Redis 用 miniredis（幂等缓存），因此只依赖 DSN。未设置 DSN 时跳过。

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	_ "github.com/jackc/pgx/v5/stdlib"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/billing/pricing"
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/rbac"
	"newtg/gateway/internal/store"
)

type pricingIntegrationEnv struct {
	db     *sql.DB
	repo   *store.SQLPricingRepo
	router http.Handler
}

func newPricingIntegrationEnv(t *testing.T) *pricingIntegrationEnv {
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

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })

	repo := store.NewPricingRepo(db)
	handlers, err := NewPricingHandlers(PricingHandlerConfig{
		Repo: repo, Redis: rdb, Authz: rbac.NewAuthzMiddleware(), Logger: testLogger(),
	})
	if err != nil {
		t.Fatalf("NewPricingHandlers: %v", err)
	}
	mux := http.NewServeMux()
	handlers.RegisterPricingRoutes(mux, nil)
	return &pricingIntegrationEnv{db: db, repo: repo, router: mux}
}

func (e *pricingIntegrationEnv) request(t *testing.T, method, path string, body any, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader = http.NoBody
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	principal := &auth.Principal{UserID: 900077, Role: rbac.RoleAdmin}
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
	return serve(e.router, req)
}

func pricingIntegrationModel(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("it-pricing-%d", time.Now().UnixNano())
}

func cleanupPricingIntegration(t *testing.T, db *sql.DB, model string) {
	t.Helper()
	if _, err := db.Exec(`DELETE FROM group_pricing WHERE model = $1`, model); err != nil {
		t.Errorf("cleanup group_pricing(%s): %v", model, err)
	}
	if _, err := db.Exec(`DELETE FROM model_pricing WHERE model = $1`, model); err != nil {
		t.Errorf("cleanup model_pricing(%s): %v", model, err)
	}
}

// TestPricingIntegrationOptimisticLockAndCascade 覆盖 SQL 乐观锁、唯一约束、
// 分组覆盖价与级联删除。
func TestPricingIntegrationOptimisticLockAndCascade(t *testing.T) {
	env := newPricingIntegrationEnv(t)
	model := pricingIntegrationModel(t)
	t.Cleanup(func() { cleanupPricingIntegration(t, env.db, model) })

	rr := env.request(t, http.MethodPost, "/api/model-pricing", map[string]any{
		"model": model, "mode": "per_token", "prompt_price": 250,
		"completion_price": 1000, "cache_read_price": 25, "cache_write_price": 300,
		"author": "openai", "vendor": "OpenAI",
	}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create = %d（body=%q）", rr.Code, rr.Body.String())
	}
	view := decodeModelPricingView(t, rr)
	if view.PromptPrice == nil || *view.PromptPrice != 250 {
		t.Fatalf("view.prompt_price = %v", view.PromptPrice)
	}

	// DB 真实落库（含 R9 的三值语义）。
	var (
		prompt     *int64
		completion *int64
		author     string
	)
	if err := env.db.QueryRow(`SELECT prompt_price, completion_price, author FROM model_pricing WHERE model = $1`, model).
		Scan(&prompt, &completion, &author); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if prompt == nil || *prompt != 250 || completion == nil || *completion != 1000 || author != "openai" {
		t.Fatalf("DB row = %v/%v/%s", prompt, completion, author)
	}

	// 重复 model → 409（UNIQUE 约束）。
	rr = env.request(t, http.MethodPost, "/api/model-pricing", map[string]any{"model": model}, nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("duplicate = %d，期望 409（body=%q）", rr.Code, rr.Body.String())
	}

	// 陈旧 updated_at → SQL WHERE 未命中 → 409。
	stale := view.UpdatedAt.Add(-time.Second)
	rr = env.request(t, http.MethodPatch, "/api/model-pricing/"+model, map[string]any{
		"updated_at": stale.Format(time.RFC3339Nano), "prompt_price": 999,
	}, nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("stale patch = %d，期望 409（body=%q）", rr.Code, rr.Body.String())
	}
	// 正确版本 → 200，且 updated_at 前移。
	rr = env.request(t, http.MethodPatch, "/api/model-pricing/"+model, map[string]any{
		"updated_at": view.UpdatedAt.Format(time.RFC3339Nano), "prompt_price": 500,
	}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("patch = %d（body=%q）", rr.Code, rr.Body.String())
	}
	view = decodeModelPricingView(t, rr)
	if view.PromptPrice == nil || *view.PromptPrice != 500 || !view.UpdatedAt.After(stale) {
		t.Fatalf("patched view = %+v", view)
	}

	// 分组覆盖价：创建 / 唯一 / 级联。
	rr = env.request(t, http.MethodPost, "/api/group-pricing", map[string]any{
		"group": "default", "model": model, "prompt_price": 400,
	}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("override create = %d（body=%q）", rr.Code, rr.Body.String())
	}
	var override groupPricingView
	if err := json.Unmarshal(rr.Body.Bytes(), &override); err != nil {
		t.Fatalf("override JSON: %v", err)
	}
	if override.Mode != "per_token" || override.SourceStatus != 1 {
		t.Fatalf("override = %+v，期望带源模型模式/状态", override)
	}
	rr = env.request(t, http.MethodPost, "/api/group-pricing", map[string]any{
		"group": "default", "model": model,
	}, nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("duplicate override = %d，期望 409", rr.Code)
	}

	// 停用 → 再启用。
	rr = env.request(t, http.MethodPost, "/api/model-pricing/"+model+":disable", map[string]any{
		"updated_at": view.UpdatedAt.Format(time.RFC3339Nano),
	}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("disable = %d（body=%q）", rr.Code, rr.Body.String())
	}
	view = decodeModelPricingView(t, rr)
	if view.Status != 2 {
		t.Fatalf("status = %d，期望 2", view.Status)
	}

	// 删除模型：同事务清理覆盖价。
	rr = env.request(t, http.MethodDelete, "/api/model-pricing/"+model+"?updated_at="+url.QueryEscape(view.UpdatedAt.Format(time.RFC3339Nano)), nil, nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete = %d（body=%q）", rr.Code, rr.Body.String())
	}
	var count int
	if err := env.db.QueryRow(`SELECT count(*) FROM group_pricing WHERE model = $1`, model).Scan(&count); err != nil {
		t.Fatalf("count overrides: %v", err)
	}
	if count != 0 {
		t.Fatalf("删除模型后仍有 %d 条覆盖价", count)
	}
	if err := env.db.QueryRow(`SELECT count(*) FROM model_pricing WHERE model = $1`, model).Scan(&count); err != nil {
		t.Fatalf("count model: %v", err)
	}
	if count != 0 {
		t.Fatalf("删除后 model_pricing 仍有 %d 行", count)
	}
}

// TestPricingIntegrationExpressionReachesSource 证明管理面保存的表达式与
// pricing.DBSource 的读取口径一致（同一 DB 行 → 同一 PriceSnapshot）。
func TestPricingIntegrationExpressionReachesSource(t *testing.T) {
	env := newPricingIntegrationEnv(t)
	model := pricingIntegrationModel(t)
	t.Cleanup(func() { cleanupPricingIntegration(t, env.db, model) })

	rr := env.request(t, http.MethodPost, "/api/model-pricing", map[string]any{
		"model": model, "billing_expr": "p * 0.001 + c * 0.002",
	}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create expression = %d（body=%q）", rr.Code, rr.Body.String())
	}
	view := decodeModelPricingView(t, rr)
	if view.Mode != pricingModeTieredExpr {
		t.Fatalf("mode = %s，期望 tiered_expr", view.Mode)
	}

	gormDB, err := gorm.Open(postgres.New(postgres.Config{Conn: env.db}), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm open: %v", err)
	}
	source, err := pricing.NewSource(gormDB, nil)
	if err != nil {
		t.Fatalf("pricing.NewSource: %v", err)
	}
	snapshot, err := source.Resolve(context.Background(), model, "default")
	if err != nil {
		t.Fatalf("source.Resolve: %v", err)
	}
	if snapshot.Mode != contract.BillingModeTieredExpr || snapshot.ExprString != "p * 0.001 + c * 0.002" {
		t.Fatalf("snapshot = %+v，期望表达式模式", snapshot)
	}
	quota, err := pricing.ComputeQuota(snapshot, contract.TokenParams{
		P: 1_000_000, C: 0, ImageCount: pricing.ImageCountUnset,
	}, nil)
	if err != nil {
		t.Fatalf("ComputeQuota: %v", err)
	}
	// 1M × 0.001 = 1000 USD/1M → 1000/1e6 × 100 = 0.1 → 取整 0 → 最小计费 1。
	if quota != 1 {
		t.Fatalf("quota = %d，期望 1（最小计费）", quota)
	}
}

// TestPricingIntegrationListPagination 在真实 SQL 上验证 offset 分页与筛选。
func TestPricingIntegrationListPagination(t *testing.T) {
	env := newPricingIntegrationEnv(t)
	prefix := fmt.Sprintf("it-page-%d-", time.Now().UnixNano())
	var models []string
	for i := 0; i < 5; i++ {
		model := fmt.Sprintf("%s%d", prefix, i)
		models = append(models, model)
		status := 1
		if i%2 == 0 {
			status = 2
		}
		rr := env.request(t, http.MethodPost, "/api/model-pricing", map[string]any{
			"model": model, "prompt_price": i * 10, "status": status,
		}, nil)
		if rr.Code != http.StatusCreated {
			t.Fatalf("seed %s = %d（body=%q）", model, rr.Code, rr.Body.String())
		}
	}
	t.Cleanup(func() {
		for _, model := range models {
			cleanupPricingIntegration(t, env.db, model)
		}
	})

	rr := env.request(t, http.MethodGet, "/api/model-pricing?keyword="+prefix+"&p=1&page_size=2&sort=prompt_price", nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list = %d（body=%q）", rr.Code, rr.Body.String())
	}
	list := decodeModelPricingList(t, rr)
	if list.Total != 5 || len(list.Items) != 2 || list.Page != 1 || list.PageSize != 2 {
		t.Fatalf("list = %+v", list)
	}
	if list.Items[0].PromptPrice == nil || *list.Items[0].PromptPrice != 0 {
		t.Fatalf("prompt_price 升序首项 = %v", list.Items[0].PromptPrice)
	}
	rr = env.request(t, http.MethodGet, "/api/model-pricing?keyword="+prefix+"&status=disabled", nil, nil)
	list = decodeModelPricingList(t, rr)
	if list.Total != 3 { // i=0,2,4 被置为停用
		t.Fatalf("status 筛选 total = %d，期望 3", list.Total)
	}
}

// TestPricingIntegrationGroupOverrideGuards 证明 R-69 的保存期拦截在真实 SQL
// 上生效：表达式源与 per_request 源的无效覆盖价字段被 422 拒绝。
func TestPricingIntegrationGroupOverrideGuards(t *testing.T) {
	env := newPricingIntegrationEnv(t)
	exprModel := pricingIntegrationModel(t) + "-expr"
	perCallModel := pricingIntegrationModel(t) + "-call"
	t.Cleanup(func() {
		cleanupPricingIntegration(t, env.db, exprModel)
		cleanupPricingIntegration(t, env.db, perCallModel)
	})

	rr := env.request(t, http.MethodPost, "/api/model-pricing",
		map[string]any{"model": exprModel, "billing_expr": "p * 0.001"}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("expr model = %d（body=%q）", rr.Code, rr.Body.String())
	}
	rr = env.request(t, http.MethodPost, "/api/group-pricing",
		map[string]any{"group": "default", "model": exprModel, "prompt_price": 1}, nil)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expr override = %d，期望 422（body=%q）", rr.Code, rr.Body.String())
	}
	assertProblemFields(t, rr, "model")

	rr = env.request(t, http.MethodPost, "/api/model-pricing",
		map[string]any{"model": perCallModel, "mode": "per_request", "prompt_price": 10}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("percall model = %d（body=%q）", rr.Code, rr.Body.String())
	}
	rr = env.request(t, http.MethodPost, "/api/group-pricing",
		map[string]any{"group": "default", "model": perCallModel, "completion_price": 5}, nil)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("percall completion override = %d，期望 422（body=%q）", rr.Code, rr.Body.String())
	}
	assertProblemFields(t, rr, "completion_price", "cache_read_price", "cache_write_price")
	rr = env.request(t, http.MethodPost, "/api/group-pricing",
		map[string]any{"group": "default", "model": perCallModel, "prompt_price": 3}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("percall prompt-only override = %d（body=%q）", rr.Code, rr.Body.String())
	}
}

// TestPricingIntegrationOverrideCreateVsModelDeleteRace 是 A3-F1 的确定性竞态
// 验收：删除事务先持有 model_pricing 行锁（模拟已开始但未提交的删除），
// 并发创建覆盖价必须在 FOR SHARE 上阻塞；删除提交后创建看到 0 行而拒绝，
// 不得留下孤儿覆盖价。
func TestPricingIntegrationOverrideCreateVsModelDeleteRace(t *testing.T) {
	env := newPricingIntegrationEnv(t)
	model := pricingIntegrationModel(t)
	t.Cleanup(func() { cleanupPricingIntegration(t, env.db, model) })

	rr := env.request(t, http.MethodPost, "/api/model-pricing",
		map[string]any{"model": model, "prompt_price": 100}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed model = %d（body=%q）", rr.Code, rr.Body.String())
	}

	ctx := context.Background()
	lockTx, err := env.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin lock tx: %v", err)
	}
	defer func() { _ = lockTx.Rollback() }()
	var one int
	if err := lockTx.QueryRowContext(ctx,
		`SELECT 1 FROM model_pricing WHERE model = $1 FOR UPDATE`, model).Scan(&one); err != nil {
		t.Fatalf("lock source row: %v", err)
	}

	type result struct{ err error }
	done := make(chan result, 1)
	go func() {
		_, createErr := env.repo.CreateGroupPricing(ctx, store.GroupPricingInput{
			Group: "default", Model: model, PromptPrice: 1, Status: 1,
		})
		done <- result{err: createErr}
	}()

	// 创建请求应阻塞在 FOR SHARE（删除事务持有排他行锁）。
	select {
	case got := <-done:
		t.Fatalf("创建在删除提交前返回：%v（期望被行锁阻塞）", got.err)
	case <-time.After(100 * time.Millisecond):
	}

	if _, err := lockTx.ExecContext(ctx, `DELETE FROM model_pricing WHERE model = $1`, model); err != nil {
		t.Fatalf("delete source in lock tx: %v", err)
	}
	if err := lockTx.Commit(); err != nil {
		t.Fatalf("commit lock tx: %v", err)
	}
	got := <-done
	if got.err != store.ErrPricingInvalidModel {
		t.Fatalf("并发创建 err = %v，期望 ErrPricingInvalidModel", got.err)
	}
	var count int
	if err := env.db.QueryRow(`SELECT count(*) FROM group_pricing WHERE model = $1`, model).Scan(&count); err != nil {
		t.Fatalf("count overrides: %v", err)
	}
	if count != 0 {
		t.Fatalf("竞态留下 %d 条孤儿覆盖价", count)
	}
}
