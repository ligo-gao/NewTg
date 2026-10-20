//go:build integration

package httpapi

// D1b 目录端点的数据库级验收：真实 PostgreSQL + 真实 store.SQLModelCatalogRepo。
// 覆盖单元假实现无法证明的真相：
//   - vendors 的 lower(name) 唯一索引（大小写不同 → 409）；
//   - model_meta.updated_at 乐观锁在 SQL 层生效；
//   - 引用保护统计停用模型；
//   - 派生字段（square_state / configured_channel_count / enable_groups）与
//     channels.supported_models、user_groups 的真实数据对齐。
//
// 运行：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_d1?sslmode=disable' \
//	  go test -tags=integration ./internal/httpapi/ -run TestModelCatalogIntegration -v

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

	_ "github.com/jackc/pgx/v5/stdlib"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/rbac"
	"newtg/gateway/internal/store"
)

type catalogIntegrationEnv struct {
	db       *sql.DB
	repo     *store.SQLModelCatalogRepo
	handlers *ModelCatalogHandlers
	router   http.Handler
	clock    time.Time
}

func newCatalogIntegrationEnv(t *testing.T) *catalogIntegrationEnv {
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

	repo := store.NewModelCatalogRepo(db)
	handlers, err := NewModelCatalogHandlers(ModelCatalogHandlerConfig{
		Repo: repo, Redis: rdb, Authz: rbac.NewAuthzMiddleware(), Logger: testLogger(),
		Derived: NewSQLCatalogDerived(db),
	})
	if err != nil {
		t.Fatalf("NewModelCatalogHandlers: %v", err)
	}
	mux := http.NewServeMux()
	handlers.RegisterModelCatalogRoutes(mux, nil)
	return &catalogIntegrationEnv{db: db, repo: repo, handlers: handlers, router: mux, clock: time.Now()}
}

func (e *catalogIntegrationEnv) request(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
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
	p := &auth.Principal{UserID: 77, Role: rbac.RoleAdmin}
	req = req.WithContext(auth.WithPrincipal(req.Context(), p))
	return serve(e.router, req)
}

func catalogIntegrationName(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

func TestModelCatalogIntegrationVendorUniquenessAndReferenceProtection(t *testing.T) {
	env := newCatalogIntegrationEnv(t)
	name := catalogIntegrationName("vnd")
	rr := env.request(t, http.MethodPost, "/api/vendors", map[string]any{"name": name})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create vendor = %d（body=%q）", rr.Code, rr.Body.String())
	}
	vendor := decodeVendorView(t, rr)
	t.Cleanup(func() {
		_, _ = env.db.Exec(`DELETE FROM model_meta WHERE vendor_id = $1`, vendor.ID)
		_, _ = env.db.Exec(`DELETE FROM vendors WHERE id = $1`, vendor.ID)
	})

	// 大小写重名 → 409（lower(name) 唯一索引）。
	rr = env.request(t, http.MethodPost, "/api/vendors", map[string]any{"name": stringsUpper(name)})
	if rr.Code != http.StatusConflict {
		t.Fatalf("大小写重名 = %d，期望 409（body=%q）", rr.Code, rr.Body.String())
	}

	// 建一个停用模型指向该供应商，删除必须 409 + reference_count=1
	// （停用模型同样算引用，D1-07）。
	modelName := catalogIntegrationName("ref-model")
	rr = env.request(t, http.MethodPost, "/api/models", map[string]any{
		"model_name": modelName, "vendor_id": vendor.ID, "status": 2,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create model = %d（body=%q）", rr.Code, rr.Body.String())
	}
	model := decodeModelView(t, rr)
	t.Cleanup(func() {
		_, _ = env.db.Exec(`DELETE FROM model_meta WHERE id = $1`, model.ID)
	})

	rr = env.request(t, http.MethodDelete, fmt.Sprintf("/api/vendors/%d", vendor.ID), nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("引用中删除 = %d，期望 409（body=%q）", rr.Code, rr.Body.String())
	}
	problem := decodeCatalogProblem(t, rr)
	if problem["reference_count"] != float64(1) {
		t.Fatalf("reference_count = %v，期望 1", problem["reference_count"])
	}

	// 更新乐观锁：错误 version → 409；正确 → 200 且版本变化。
	rr = env.request(t, http.MethodPut, fmt.Sprintf("/api/vendors/%d", vendor.ID),
		map[string]any{"name": name, "version": "stale"})
	if rr.Code != http.StatusConflict {
		t.Fatalf("错误 version = %d，期望 409", rr.Code)
	}
	rr = env.request(t, http.MethodPut, fmt.Sprintf("/api/vendors/%d", vendor.ID),
		map[string]any{"name": name + "-x", "version": vendor.Version})
	if rr.Code != http.StatusOK {
		t.Fatalf("update = %d（body=%q）", rr.Code, rr.Body.String())
	}
}

func stringsUpper(value string) string {
	upper := make([]byte, len(value))
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		upper[i] = c
	}
	return string(upper)
}

func TestModelCatalogIntegrationDerivedFields(t *testing.T) {
	env := newCatalogIntegrationEnv(t)

	// 两个启用分组：partial 场景需要「只覆盖部分用户分组」。
	groupA := catalogIntegrationName("grp-a")
	groupB := catalogIntegrationName("grp-b")
	insertGroup := func(name string) {
		var id int64
		if err := env.db.QueryRow(
			`INSERT INTO user_groups (name, status) VALUES ($1, 1) RETURNING id`, name).Scan(&id); err != nil {
			t.Fatalf("insert user group: %v", err)
		}
		t.Cleanup(func() { _, _ = env.db.Exec(`DELETE FROM user_groups WHERE id = $1`, id) })
	}
	insertGroup(groupA)
	insertGroup(groupB)

	insertChannel := func(modelsJSON, groupTags string, status int16) int64 {
		var groupArg any
		if groupTags != "" {
			groupArg = groupTags
		}
		var id int64
		if err := env.db.QueryRow(`
			INSERT INTO channels (name, type, credentials, supported_models, group_tags, model_mapping,
				weight, priority, status, auto_ban)
			VALUES ($1, 'openai', $2, $3::jsonb, $4::jsonb, '{}'::jsonb, 1, 10, $5, true)
			RETURNING id`,
			catalogIntegrationName("ch"), []byte{0}, modelsJSON, groupArg, status).Scan(&id); err != nil {
			t.Fatalf("insert channel: %v", err)
		}
		t.Cleanup(func() { _, _ = env.db.Exec(`DELETE FROM channels WHERE id = $1`, id) })
		return id
	}

	prefix := catalogIntegrationName("derive")
	allName := prefix + "-all"
	partialName := prefix + "-partial"
	offName := prefix + "-off"
	// NULL group_tags = 全部分组 → visible；精确匹配。
	insertChannel(fmt.Sprintf(`["%s"]`, allName), "", 1)
	// 只覆盖 groupA → partial；前缀规则。
	insertChannel(fmt.Sprintf(`["%s-suffix"]`, partialName), fmt.Sprintf(`["%s"]`, groupA), 1)
	// 停用渠道不计入。
	insertChannel(fmt.Sprintf(`["%s"]`, offName), "", 2)

	mustCreate := func(body map[string]any) modelMetaView {
		rr := env.request(t, http.MethodPost, "/api/models", body)
		if rr.Code != http.StatusCreated {
			t.Fatalf("create model = %d（body=%q）", rr.Code, rr.Body.String())
		}
		view := decodeModelView(t, rr)
		t.Cleanup(func() { _, _ = env.db.Exec(`DELETE FROM model_meta WHERE id = $1`, view.ID) })
		return view
	}
	mustCreate(map[string]any{"model_name": allName})
	mustCreate(map[string]any{"model_name": partialName, "name_rule": 1})
	offView := mustCreate(map[string]any{"model_name": offName})

	// 详情：全分组渠道 → visible，计数 1，enable_groups = 全部启用分组。
	rr := env.request(t, http.MethodGet, fmt.Sprintf("/api/models/%d", findModelID(t, env, allName)), nil)
	view := decodeModelView(t, rr)
	if view.SquareState != squareStateVisible {
		t.Fatalf("all → square_state = %q，期望 visible（body=%q）", view.SquareState, rr.Body.String())
	}
	if view.ConfiguredChannelCount != 1 {
		t.Fatalf("all → configured_channel_count = %d，期望 1", view.ConfiguredChannelCount)
	}
	if !catalogContainsString(view.EnableGroups, groupA) || !catalogContainsString(view.EnableGroups, groupB) {
		t.Fatalf("all → enable_groups = %v，期望包含两个分组", view.EnableGroups)
	}

	// 前缀规则 + 只覆盖 groupA → partial。
	rr = env.request(t, http.MethodGet, fmt.Sprintf("/api/models/%d", findModelID(t, env, partialName)), nil)
	view = decodeModelView(t, rr)
	if view.SquareState != squareStatePartial {
		t.Fatalf("partial → square_state = %q，期望 partial（body=%q）", view.SquareState, rr.Body.String())
	}
	if len(view.EnableGroups) != 1 || view.EnableGroups[0] != groupA {
		t.Fatalf("partial → enable_groups = %v", view.EnableGroups)
	}

	// 只有停用渠道 → unavailable（且计数 0）。
	if offView.SquareState != squareStateUnavailable || offView.ConfiguredChannelCount != 0 {
		t.Fatalf("off → %+v", offView)
	}

	// status=2 的档案恒 hidden。updated_at 必须取真实行值（PG 是微秒精度，
	// time.Now() 的纳秒会与它不相等而误判 409）。
	rr = env.request(t, http.MethodGet, fmt.Sprintf("/api/models/%d", findModelID(t, env, allName)), nil)
	current := decodeModelView(t, rr)
	rr = env.request(t, http.MethodPut, fmt.Sprintf("/api/models/%d", current.ID),
		map[string]any{"status": 2, "updated_at": current.UpdatedAt.Format(time.RFC3339Nano)})
	if rr.Code != http.StatusOK {
		t.Fatalf("update status = %d（body=%q）", rr.Code, rr.Body.String())
	}
	view = decodeModelView(t, rr)
	if view.SquareState != squareStateHidden {
		t.Fatalf("status=2 → square_state = %q，期望 hidden", view.SquareState)
	}

	// 搜索走真实 SQL。
	rr = env.request(t, http.MethodGet, "/api/models/search?keyword="+url.QueryEscape(prefix)+"&limit=10", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("search = %d", rr.Code)
	}
	var search modelSearchListView
	if err := json.Unmarshal(rr.Body.Bytes(), &search); err != nil {
		t.Fatalf("search JSON: %v", err)
	}
	if len(search.Items) != 3 {
		t.Fatalf("search 命中 = %d，期望 3", len(search.Items))
	}
}

func findModelID(t *testing.T, env *catalogIntegrationEnv, name string) int64 {
	t.Helper()
	var id int64
	if err := env.db.QueryRow(`SELECT id FROM model_meta WHERE model_name = $1`, name).Scan(&id); err != nil {
		t.Fatalf("find model %s: %v", name, err)
	}
	return id
}

func catalogContainsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func TestModelCatalogIntegrationBatchDeleteChannelProtection(t *testing.T) {
	env := newCatalogIntegrationEnv(t)
	name := catalogIntegrationName("batch")
	rr := env.request(t, http.MethodPost, "/api/models", map[string]any{"model_name": name})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create model = %d", rr.Code)
	}
	view := decodeModelView(t, rr)
	t.Cleanup(func() { _, _ = env.db.Exec(`DELETE FROM model_meta WHERE id = $1`, view.ID) })

	// 渠道只剩这一个模型：remove_from_channels=true 必须 422 + channel_ids。
	var channelID int64
	if err := env.db.QueryRow(`
		INSERT INTO channels (name, type, credentials, supported_models, model_mapping,
			weight, priority, status, auto_ban)
		VALUES ($1, 'openai', $2, $3::jsonb, '{}'::jsonb, 1, 10, 1, true)
		RETURNING id`,
		catalogIntegrationName("ch"), []byte{0}, fmt.Sprintf(`["%s"]`, name)).Scan(&channelID); err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	t.Cleanup(func() { _, _ = env.db.Exec(`DELETE FROM channels WHERE id = $1`, channelID) })

	rr = env.request(t, http.MethodPost, "/api/models/batch-delete", map[string]any{
		"ids": []int64{view.ID}, "remove_from_channels": true,
	})
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("渠道变空 = %d，期望 422（body=%q）", rr.Code, rr.Body.String())
	}
	problem := decodeCatalogProblem(t, rr)
	ids, ok := problem["channel_ids"].([]any)
	if !ok || len(ids) != 1 || ids[0] != float64(channelID) {
		t.Fatalf("channel_ids = %v，期望 [%d]", problem["channel_ids"], channelID)
	}
	// 模型仍在（整体回滚）。
	if _, err := env.db.Exec(`SELECT 1 FROM model_meta WHERE id = $1`, view.ID); err != nil {
		t.Fatalf("rollback check: %v", err)
	}
}

// TestModelCatalogIntegrationSyncAdvisoryLock 证明同步事务拿的是同一个
// pg_advisory_xact_lock：后到者阻塞到先到者提交，随后基于新状态（已存在的
// 供应商）走 update，而不是照旧快照重复 insert。
func TestModelCatalogIntegrationSyncAdvisoryLock(t *testing.T) {
	env := newCatalogIntegrationEnv(t)
	vendorName := catalogIntegrationName("lock-vendor")
	modelName := catalogIntegrationName("lock-model")
	raw := syncManifestFor(t,
		[]map[string]any{{"model_name": modelName}},
		[]map[string]any{{"name": vendorName, "description": "upstream"}},
	)
	env.handlers.syncSource = func(context.Context) ([]byte, error) { return raw, nil }
	version := integrationPreviewSourceVersion(t, env)
	t.Cleanup(func() {
		_, _ = env.db.Exec(`DELETE FROM model_meta WHERE model_name = $1`, modelName)
		_, _ = env.db.Exec(`DELETE FROM vendors WHERE lower(name) = lower($1)`, vendorName)
	})

	ctx := context.Background()
	conn, err := env.db.Conn(ctx)
	if err != nil {
		t.Fatalf("acquire conn: %v", err)
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		_ = conn.Close()
		t.Fatalf("begin holder tx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtext('newtg.model_sync')::bigint)`); err != nil {
		t.Fatalf("hold lock: %v", err)
	}
	// 持锁期间先写入供应商：后到者恢复后应看到它并走 update。
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO vendors (name, description, status) VALUES ($1, 'holder', 1)`, vendorName); err != nil {
		t.Fatalf("insert holder vendor: %v", err)
	}

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- env.request(t, http.MethodPost, "/api/models/sync", map[string]any{
			"source_version": version,
			"models":         []any{map[string]any{"model_name": modelName}},
			"vendors":        []any{map[string]any{"name": vendorName}},
		})
	}()
	select {
	case <-done:
		t.Fatal("后到者未被咨询锁阻塞")
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit holder tx: %v", err)
	}
	_ = conn.Close()

	rr := <-done
	if rr.Code != http.StatusOK {
		t.Fatalf("apply = %d（body=%q）", rr.Code, rr.Body.String())
	}
	var applied syncApplyResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &applied)
	// 后到者看到已存在的供应商 → update，不撞唯一约束。
	if len(applied.UpdatedVendors) != 1 || len(applied.CreatedVendors) != 0 {
		t.Fatalf("应基于新状态更新供应商：%+v", applied)
	}
	var description string
	if err := env.db.QueryRow(`SELECT description FROM vendors WHERE lower(name) = lower($1)`, vendorName).
		Scan(&description); err != nil {
		t.Fatalf("read vendor: %v", err)
	}
	if description != "upstream" {
		t.Fatalf("description = %q，期望 upstream", description)
	}
}

// TestModelCatalogIntegrationSyncRollback 证明真实 SQL 事务整体回滚：
// 供应商已写入、模型校验失败时，供应商行不能残留。
func TestModelCatalogIntegrationSyncRollback(t *testing.T) {
	env := newCatalogIntegrationEnv(t)
	vendorName := catalogIntegrationName("rollback-vendor")
	modelName := catalogIntegrationName("rollback-model")
	raw := syncManifestFor(t,
		[]map[string]any{{"model_name": modelName, "capabilities": []string{"not-a-capability"}}},
		[]map[string]any{{"name": vendorName}},
	)
	env.handlers.syncSource = func(context.Context) ([]byte, error) { return raw, nil }
	version := integrationPreviewSourceVersion(t, env)
	t.Cleanup(func() {
		_, _ = env.db.Exec(`DELETE FROM model_meta WHERE model_name = $1`, modelName)
		_, _ = env.db.Exec(`DELETE FROM vendors WHERE lower(name) = lower($1)`, vendorName)
	})

	rr := env.request(t, http.MethodPost, "/api/models/sync", map[string]any{
		"source_version": version,
		"models":         []any{map[string]any{"model_name": modelName}},
		"vendors":        []any{map[string]any{"name": vendorName}},
	})
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("apply = %d，期望 422（body=%q）", rr.Code, rr.Body.String())
	}
	var count int64
	if err := env.db.QueryRow(`SELECT count(*) FROM vendors WHERE lower(name) = lower($1)`, vendorName).
		Scan(&count); err != nil {
		t.Fatalf("count vendor: %v", err)
	}
	if count != 0 {
		t.Fatalf("事务未回滚：供应商残留 %d 行", count)
	}
}

// TestModelCatalogIntegrationDeleteVersioned 证明删除的乐观锁在 SQL 层生效：
// 读取之后另一连接提交更新，删除命中 0 行 → 409，行保留。
func TestModelCatalogIntegrationDeleteVersioned(t *testing.T) {
	env := newCatalogIntegrationEnv(t)
	name := catalogIntegrationName("del-race")
	rr := env.request(t, http.MethodPost, "/api/models", map[string]any{"model_name": name})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create = %d（body=%q）", rr.Code, rr.Body.String())
	}
	view := decodeModelView(t, rr)
	t.Cleanup(func() { _, _ = env.db.Exec(`DELETE FROM model_meta WHERE id = $1`, view.ID) })

	// 模拟「读后、删前」另一连接的已提交更新。
	if _, err := env.db.Exec(
		`UPDATE model_meta SET description = 'concurrent', updated_at = now() WHERE id = $1`, view.ID); err != nil {
		t.Fatalf("concurrent update: %v", err)
	}

	rr = env.request(t, http.MethodDelete,
		fmt.Sprintf("/api/models/%d?updated_at=%s", view.ID,
			url.QueryEscape(view.UpdatedAt.Format(time.RFC3339Nano))), nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("删除 = %d，期望 409（body=%q）", rr.Code, rr.Body.String())
	}
	var count int64
	if err := env.db.QueryRow(`SELECT count(*) FROM model_meta WHERE id = $1`, view.ID).Scan(&count); err != nil {
		t.Fatalf("count model: %v", err)
	}
	if count != 1 {
		t.Fatalf("409 删除不应删行，剩余 %d", count)
	}
}

// TestModelCatalogIntegrationSearchStatusPushdown 证明 status 在 SQL 的 LIMIT
// 之前生效：前 100 条全部启用时，仍能返回排序靠后的停用匹配。
func TestModelCatalogIntegrationSearchStatusPushdown(t *testing.T) {
	env := newCatalogIntegrationEnv(t)
	prefix := catalogIntegrationName("sp")
	disabled := int16(2)
	for i := 0; i < 100; i++ {
		if _, err := env.repo.CreateModelMeta(context.Background(), store.ModelMetaInput{
			ModelName: fmt.Sprintf("%s-a%03d", prefix, i),
		}); err != nil {
			t.Fatalf("create enabled model: %v", err)
		}
	}
	if _, err := env.repo.CreateModelMeta(context.Background(), store.ModelMetaInput{
		ModelName: prefix + "-z", Status: &disabled,
	}); err != nil {
		t.Fatalf("create disabled model: %v", err)
	}
	t.Cleanup(func() {
		_, _ = env.db.Exec(`DELETE FROM model_meta WHERE model_name LIKE $1`, prefix+"%")
	})

	rr := env.request(t, http.MethodGet,
		"/api/models/search?keyword="+url.QueryEscape(prefix)+"&status=2&limit=100", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("search = %d（body=%q）", rr.Code, rr.Body.String())
	}
	var list modelSearchListView
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("search JSON: %v", err)
	}
	if len(list.Items) != 1 || list.Items[0].ModelName != prefix+"-z" {
		t.Fatalf("status 下推结果 = %+v，期望仅 %s-z", list.Items, prefix)
	}
}

func integrationPreviewSourceVersion(t *testing.T, env *catalogIntegrationEnv) string {
	t.Helper()
	rr := env.request(t, http.MethodGet, "/api/models/sync/preview", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("preview = %d（body=%q）", rr.Code, rr.Body.String())
	}
	var preview syncPreviewResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &preview); err != nil {
		t.Fatalf("preview JSON: %v", err)
	}
	return preview.SourceVersion
}
