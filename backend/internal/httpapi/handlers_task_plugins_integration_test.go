//go:build integration

package httpapi

// 插件管理端点的数据库级验收：上传/激活/删除/乐观锁经过真实仓储与运行时
// Manager；市场安装的 hash 校验在真实库上证明「零写入」。
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_m6d_t5?sslmode=disable' \
//	  go test -tags=integration ./internal/httpapi/ -run TestTaskPluginIntegration -v

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/plugin"
	"newtg/gateway/internal/plugin/market"
	"newtg/gateway/internal/rbac"
	"newtg/gateway/internal/store"
	"newtg/gateway/plugins"
)

type pluginIntegrationEnv struct {
	mux     *http.ServeMux
	repo    *store.TaskPluginRepo
	manager *plugin.Manager
	db      *sql.DB
}

func newPluginIntegrationEnv(t *testing.T, marketClient *market.Client) *pluginIntegrationEnv {
	t.Helper()
	db := openDeployTestDB(t)
	ctx := t.Context()
	for _, query := range []string{
		`DELETE FROM task_plugin_versions`,
		`DELETE FROM task_plugins`,
		`DELETE FROM plugin_marketplace_sources`,
	} {
		if _, err := db.ExecContext(ctx, query); err != nil {
			t.Fatalf("clean: %v", err)
		}
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM task_plugin_versions`)
		_, _ = db.ExecContext(context.Background(), `DELETE FROM task_plugins`)
		_, _ = db.ExecContext(context.Background(), `DELETE FROM plugin_marketplace_sources`)
	})

	repo := store.NewTaskPluginRepo(db)
	sources, err := plugins.TaskPluginSources()
	if err != nil {
		t.Fatalf("TaskPluginSources: %v", err)
	}
	manager, err := plugin.NewManager(sources, plugin.NewSQLVersionLoader(db), plugin.Options{MasterEnabled: true})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := manager.Reload(ctx); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	exec := &fakeAuditExec{}
	handlers, err := NewTaskPluginHandlers(TaskPluginHandlerConfig{
		Store: repo, Manager: manager, Authz: rbac.NewAuthzMiddleware(),
		Access: newFakeUserAccess(), Audit: audit.New(exec), Market: marketClient,
		Bindings: store.NewChannelRepo(db),
	})
	if err != nil {
		t.Fatalf("NewTaskPluginHandlers: %v", err)
	}
	mux := http.NewServeMux()
	handlers.Register(mux)
	return &pluginIntegrationEnv{mux: mux, repo: repo, manager: manager, db: db}
}

func TestTaskPluginIntegrationUploadActivateDelete(t *testing.T) {
	env := newPluginIntegrationEnv(t, nil)
	source := samplePluginSource(t)
	body := `{"source_text":` + mustJSON(t, source) + `}`

	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/plugin/task", body, pluginAdmin(),
		map[string]string{"Idempotency-Key": "it-upload-1"}))
	if rr.Code != http.StatusCreated {
		t.Fatalf("上传 = %d（body=%s）", rr.Code, rr.Body.String())
	}
	var upload struct {
		Plugin struct {
			UpdatedAt time.Time `json:"updated_at"`
		} `json:"plugin"`
		Version struct {
			Version string `json:"version"`
		} `json:"version"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &upload); err != nil {
		t.Fatalf("decode upload: %v", err)
	}
	if upload.Version.Version == "" {
		t.Fatal("上传响应缺版本")
	}

	// 唯一版本禁删（S4）。
	rr = httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodDelete,
		"/api/plugin/task/sample/versions/"+upload.Version.Version, `{}`, pluginAdmin(), nil))
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "plugin_version_required") {
		t.Fatalf("删除唯一版本 = %d（body=%s），期望 409 plugin_version_required", rr.Code, rr.Body.String())
	}

	// 激活：expect_updated_at 来自上传响应。
	activateBody := `{"version":` + mustJSON(t, upload.Version.Version) + `,"expect_updated_at":` +
		mustJSON(t, upload.Plugin.UpdatedAt.UTC().Format(time.RFC3339Nano)) + `}`
	rr = httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/plugin/task/sample/activate",
		activateBody, pluginAdmin(), nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("激活 = %d（body=%s）", rr.Code, rr.Body.String())
	}
	var activated struct {
		Plugin struct {
			ActiveVersion *string   `json:"active_version"`
			UpdatedAt     time.Time `json:"updated_at"`
		} `json:"plugin"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &activated); err != nil {
		t.Fatalf("decode activate: %v", err)
	}
	if activated.Plugin.ActiveVersion == nil || *activated.Plugin.ActiveVersion != upload.Version.Version {
		t.Fatalf("激活后镜像 active_version = %v", activated.Plugin.ActiveVersion)
	}

	// 陈旧 expect_updated_at → 409。
	stale := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	rr = httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/plugin/task/sample/status",
		`{"enabled":false,"expect_updated_at":`+mustJSON(t, stale)+`}`, pluginAdmin(), nil))
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "plugin_updated_at_conflict") {
		t.Fatalf("陈旧乐观锁 = %d（body=%s），期望 409", rr.Code, rr.Body.String())
	}

	// 正确版本值切启用状态，响应 bound_channels=0（绑定列未落地）。
	statusBody := `{"enabled":false,"expect_updated_at":` +
		mustJSON(t, activated.Plugin.UpdatedAt.UTC().Format(time.RFC3339Nano)) + `}`
	rr = httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/plugin/task/sample/status",
		statusBody, pluginAdmin(), nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"bound_channels":0`) {
		t.Fatalf("禁用 = %d（body=%s）", rr.Code, rr.Body.String())
	}
}

func TestTaskPluginIntegrationMarketHashMismatchZeroWrite(t *testing.T) {
	source := sourceForKey(t, "market-it")
	sum := sha256.Sum256([]byte("tampered"))
	marketServer := newMarketTestServer(t, "market-it", "1.0.0", source, hex.EncodeToString(sum[:]))
	env := newPluginIntegrationEnv(t, marketServer.client(t))
	if _, err := env.repo.ReplaceMarketSources(t.Context(), []store.MarketSourceInput{
		{Name: "IT market", URL: "https://market.example/index.json", Enabled: true},
	}); err != nil {
		t.Fatalf("seed source: %v", err)
	}
	sources, err := env.repo.ListMarketSources(t.Context())
	if err != nil || len(sources) != 1 {
		t.Fatalf("list sources = %+v, %v", sources, err)
	}

	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/plugin/task/marketplace/install",
		`{"source_id":"`+sources[0].ID+`","key":"market-it","version":"1.0.0"}`, pluginAdmin(), nil))
	if rr.Code != http.StatusUnprocessableEntity || !strings.Contains(rr.Body.String(), "plugin_hash_mismatch") {
		t.Fatalf("hash 不符 = %d（body=%s）", rr.Code, rr.Body.String())
	}
	pluginsList, err := env.repo.List(t.Context())
	if err != nil {
		t.Fatalf("list plugins: %v", err)
	}
	for _, p := range pluginsList {
		if p.Key == "market-it" {
			t.Fatalf("hash 不符不得创建主表行：%+v", p)
		}
	}
}

func TestTaskPluginIntegrationBuiltinRows(t *testing.T) {
	env := newPluginIntegrationEnv(t, nil)
	ctx := t.Context()
	if err := env.repo.UpsertBuiltinPlugins(ctx, []store.BuiltinPlugin{{
		Key: "sample", Name: "Sample", Description: strPtrIntegration("builtin sample"),
	}}); err != nil {
		t.Fatalf("UpsertBuiltinPlugins: %v", err)
	}

	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodGet, "/api/plugin/task", "", pluginAdmin(), nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"key":"sample"`) ||
		!strings.Contains(rr.Body.String(), `"runtime_status":"registered"`) {
		t.Fatalf("内置插件应出现在列表且 registered：%d %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"version":"1.0.0"`) {
		t.Fatalf("内置插件列表应带运行时生效版本：%s", rr.Body.String())
	}

	rr = httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodGet, "/api/plugin/task/sample", "", pluginAdmin(), nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("内置插件详情 = %d（body=%s）", rr.Code, rr.Body.String())
	}

	// 上传内置 key 的覆盖层：主表必须保留 builtin/source=builtin，版本行不激活。
	source := samplePluginSource(t)
	rr = httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/plugin/task",
		`{"source_text":`+mustJSON(t, source)+`}`, pluginAdmin(),
		map[string]string{"Idempotency-Key": "builtin-override"}))
	if rr.Code != http.StatusCreated {
		t.Fatalf("上传内置 key = %d（body=%s）", rr.Code, rr.Body.String())
	}
	var upload struct {
		Plugin struct {
			Source    string    `json:"source"`
			Builtin   bool      `json:"builtin"`
			UpdatedAt time.Time `json:"updated_at"`
		} `json:"plugin"`
		Version struct {
			Active bool `json:"active"`
		} `json:"version"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &upload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if upload.Plugin.Source != "builtin" || !upload.Plugin.Builtin {
		t.Fatalf("上传内置 key 后主表来源被改写：%+v", upload.Plugin)
	}
	if upload.Version.Active {
		t.Fatal("上传不得自动激活")
	}

	// 停用内置 key：B-11 enabled=false 必须让 factory 层不再注册。
	statusBody := `{"enabled":false,"expect_updated_at":` +
		mustJSON(t, upload.Plugin.UpdatedAt.UTC().Format(time.RFC3339Nano)) + `}`
	rr = httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/plugin/task/sample/status",
		statusBody, pluginAdmin(), nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("停用内置 key = %d（body=%s）", rr.Code, rr.Body.String())
	}
	rr = httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodGet, "/api/plugin/task/runtime/status", "", pluginAdmin(), nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("runtime status = %d", rr.Code)
	}
	var aggregate plugin.RuntimeAggregate
	if err := json.Unmarshal(rr.Body.Bytes(), &aggregate); err != nil {
		t.Fatalf("decode runtime: %v", err)
	}
	if len(aggregate.Instances) != 1 {
		t.Fatalf("未装配 Redis 协调器时应只返回本地副本：%+v", aggregate)
	}
	for _, p := range aggregate.Instances[0].Plugins {
		if p.Key == "sample" && p.Status != plugin.StatusDisabled {
			t.Fatalf("停用后内置 key 状态 = %s，期望 disabled：%+v", p.Status, aggregate.Instances[0].Plugins)
		}
	}
}

func strPtrIntegration(value string) *string { return &value }

// TestTaskPluginIntegrationBoundChannels 覆盖 M6d Task 7a 的真实计数：
// 单绑/多绑命中、停用渠道排除、绑定选项只列已注册插件。
func TestTaskPluginIntegrationBoundChannels(t *testing.T) {
	env := newPluginIntegrationEnv(t, nil)
	ctx := t.Context()
	suffix := time.Now().UnixNano()

	insert := func(role string, status int16, pluginKey any, extend string) int64 {
		t.Helper()
		var id int64
		err := env.db.QueryRowContext(ctx, `INSERT INTO channels
			(name, type, credentials, supported_models, status, task_plugin_key, task_extend_plugin_keys)
			VALUES ($1, 'task_plugin', $2, '["video-x"]'::jsonb, $3, $4, $5::jsonb) RETURNING id`,
			"it-plugin-bind-"+role+"-"+strconv.FormatInt(suffix, 10), []byte("ciphertext"), status, pluginKey, extend).Scan(&id)
		if err != nil {
			t.Fatalf("insert channel %s: %v", role, err)
		}
		return id
	}
	ids := []int64{
		insert("single", 1, "sample", `[]`),
		insert("multi", 1, nil, `["sample"]`),
		insert("disabled", 2, "sample", `[]`),
	}
	t.Cleanup(func() {
		for _, id := range ids {
			_, _ = env.db.ExecContext(context.Background(), `DELETE FROM channels WHERE id = $1`, id)
		}
	})
	if err := env.repo.UpsertBuiltinPlugins(ctx, []store.BuiltinPlugin{
		{Key: "sample", Name: "Sample"},
	}); err != nil {
		t.Fatalf("seed plugin rows: %v", err)
	}

	// 绑定选项：channels:read 可读，按真实计数投影。
	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodGet, "/api/task_plugin_options", "",
		&auth.Principal{UserID: 9, Role: rbac.RoleAdmin, Permissions: []string{rbac.PermChannelsRead}}, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("绑定选项 = %d（body=%s）", rr.Code, rr.Body.String())
	}
	var options struct {
		Items []struct {
			Key          string `json:"key"`
			ChannelCount int    `json:"channel_count"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &options); err != nil {
		t.Fatalf("decode options: %v", err)
	}
	found := false
	for _, item := range options.Items {
		if item.Key == "sample" {
			found = true
			if item.ChannelCount != 2 {
				t.Fatalf("sample channel_count = %d，期望 2（停用渠道不计）", item.ChannelCount)
			}
		}
	}
	if !found {
		t.Fatalf("绑定选项缺少 sample：%+v", options.Items)
	}

	// 插件列表 bound_channels 真实计数。
	rr = httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodGet, "/api/plugin/task", "", pluginAdmin(), nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("插件列表 = %d（body=%s）", rr.Code, rr.Body.String())
	}
	var list struct {
		Items []struct {
			Key           string `json:"key"`
			BoundChannels int    `json:"bound_channels"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	for _, item := range list.Items {
		if item.Key == "sample" && item.BoundChannels != 2 {
			t.Fatalf("列表 sample bound_channels = %d，期望 2", item.BoundChannels)
		}
	}

	// 停用预检：bound_channels 与列表一致。
	rr = httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/plugin/task/sample/status",
		`{"enabled":false}`, pluginAdmin(), nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("停用 sample = %d（body=%s）", rr.Code, rr.Body.String())
	}
	var status struct {
		BoundChannels int `json:"bound_channels"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if status.BoundChannels != 2 {
		t.Fatalf("停用响应 bound_channels = %d，期望 2", status.BoundChannels)
	}
}
