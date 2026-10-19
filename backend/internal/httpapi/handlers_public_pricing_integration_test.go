//go:build integration

package httpapi

// M6c Task 7 公开读面的数据库级验收：真实 PostgreSQL + SQLPublicPricingRepo
// + 真实会话（软鉴权），验证仅靠内存假实现无法证明的真相：
//   - 可见集合（停用 / 表达式）在 HTTP 层确实被排除；
//   - 登录用户的 group_price 来自 users."group" 与 group_pricing 覆盖价；
//   - /api/home/stats 的 models 与目录可见集合同口径。
//
// 运行（隔离库）：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_test_verify_t7?sslmode=disable' \
//	  go test -tags=integration ./internal/httpapi/ -run TestPublicPricingIntegration -v
//
// 只插入/删除本测试命名的行，不做 DROP/TRUNCATE。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	_ "github.com/jackc/pgx/v5/stdlib"

	"newtg/gateway/internal/auth"
)

func TestPublicPricingIntegrationVisibleSetAndSoftAuth(t *testing.T) {
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

	suffix := time.Now().UnixNano()
	visible := fmt.Sprintf("t7-api-visible-%d", suffix)
	disabled := fmt.Sprintf("t7-api-disabled-%d", suffix)
	expression := fmt.Sprintf("t7-api-expr-%d", suffix)
	group := fmt.Sprintf("t7ag%d", suffix%1_000_000_000)
	username := fmt.Sprintf("t7user%d", suffix%1_000_000_000)

	if _, err := db.Exec(`INSERT INTO user_groups (name, ratio, user_selectable, status)
		VALUES ($1, 150, true, 1)`, group); err != nil {
		t.Fatalf("insert user_groups: %v", err)
	}
	var userID int64
	if err := db.QueryRow(`INSERT INTO users (username, password_hash, role, status, "group")
		VALUES ($1, 'x', 'user', 'enabled', $2) RETURNING id`, username, group).Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	for _, row := range []struct {
		model  string
		status int16
	}{{visible, 1}, {disabled, 2}} {
		if _, err := db.Exec(`INSERT INTO model_pricing
			(model, mode, prompt_price, completion_price, cache_read_price, cache_write_price, author, status)
			VALUES ($1, 0, 250, 900, 25, 50, 't7', $2)`, row.model, row.status); err != nil {
			t.Fatalf("insert model_pricing(%s): %v", row.model, err)
		}
		if _, err := db.Exec(`INSERT INTO group_pricing
			("group", model, prompt_price, completion_price, cache_read_price, cache_write_price, status)
			VALUES ($1, $2, 400, 1200, 40, 80, 1)`, group, row.model); err != nil {
			t.Fatalf("insert group_pricing(%s): %v", row.model, err)
		}
	}
	if _, err := db.Exec(`INSERT INTO model_pricing
		(model, mode, billing_expr, author, status)
		VALUES ($1, 0, '{"mode":"tiered_expr"}', 't7', 1)`, expression); err != nil {
		t.Fatalf("insert expression model: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM group_pricing WHERE model IN ($1, $2)`, visible, disabled)
		_, _ = db.Exec(`DELETE FROM model_pricing WHERE model IN ($1, $2, $3)`, visible, disabled, expression)
		_, _ = db.Exec(`DELETE FROM users WHERE id = $1`, userID)
		_, _ = db.Exec(`DELETE FROM user_groups WHERE name = $1`, group)
	})

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	sessions := auth.NewSessionManager(rdb, []byte(testSessionSecret))
	rt, err := NewRouter(RouterConfig{
		DB: db, Redis: rdb, Sessions: sessions, Config: mapConfig{},
		SessionSecret: []byte(testSessionSecret),
	})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}

	// 匿名：可见模型出现，停用/表达式不出现。
	rr := serve(rt, httptest.NewRequest(http.MethodGet, "/api/pricing?page_size=100", http.NoBody))
	if rr.Code != http.StatusOK {
		t.Fatalf("anonymous pricing = %d（body=%s）", rr.Code, rr.Body.String())
	}
	var list PublicPricingList
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	seen := map[string]PublicModelCard{}
	for _, item := range list.Items {
		seen[item.ModelID] = item
	}
	card, ok := seen[visible]
	if !ok {
		t.Fatalf("可见模型 %s 缺失", visible)
	}
	if _, ok := seen[disabled]; ok {
		t.Fatal("停用模型不得出现在公示页")
	}
	if _, ok := seen[expression]; ok {
		t.Fatal("表达式计价模型不得出现在公示页")
	}
	if card.GroupPrice != nil {
		t.Fatalf("匿名且 expose=false 不应出现 group_price: %+v", card.GroupPrice)
	}

	// 登录用户：软鉴权解析 users."group" 并应用覆盖价。
	sid, err := sessions.Create(ctx, userID, auth.SessionOpts{Role: "user"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/pricing?page_size=100", http.NoBody)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: sid})
	rr = serve(rt, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("logged-in pricing = %d（body=%s）", rr.Code, rr.Body.String())
	}
	list = PublicPricingList{}
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	seen = map[string]PublicModelCard{}
	for _, item := range list.Items {
		seen[item.ModelID] = item
	}
	logged := seen[visible]
	if logged.GroupPrice == nil || logged.GroupPrice.Source != "override" ||
		logged.GroupPrice.Prompt == nil || *logged.GroupPrice.Prompt != "400" ||
		logged.GroupPrice.Group != group {
		t.Fatalf("登录用户覆盖价错误: %+v", logged.GroupPrice)
	}

	// 首页统计的 models 与目录可见集合同口径（同一请求无筛选，Total 即可见总数）。
	var total int64
	if _, err := fmt.Sscanf(list.Total, "%d", &total); err != nil {
		t.Fatalf("parse total %q: %v", list.Total, err)
	}
	stats := serve(rt, httptest.NewRequest(http.MethodGet, "/api/home/stats", http.NoBody))
	var view HomeStatsView
	if err := json.Unmarshal(stats.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode stats: %v", err)
	}
	if view.Models != total {
		t.Fatalf("stats.models=%d != pricing.total=%d", view.Models, total)
	}
}
