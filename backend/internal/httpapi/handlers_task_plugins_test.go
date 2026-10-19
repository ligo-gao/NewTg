package httpapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
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

type pluginTestEnv struct {
	mux      *http.ServeMux
	store    *fakeTaskPluginStore
	manager  *plugin.Manager
	audit    *fakeAuditExec
	handlers *TaskPluginHandlers
}

func samplePluginSource(t *testing.T) string {
	t.Helper()
	sources, err := plugins.TaskPluginSources()
	if err != nil {
		t.Fatalf("TaskPluginSources: %v", err)
	}
	source, ok := sources["sample"]
	if !ok {
		t.Fatal("内置 sample 插件缺失")
	}
	return source
}

func sourceForKey(t *testing.T, key string) string {
	t.Helper()
	return strings.Replace(samplePluginSource(t), `key: "sample"`, `key: "`+key+`"`, 1)
}

func newPluginTestEnv(t *testing.T, manager *plugin.Manager) *pluginTestEnv {
	return newPluginTestEnvWithMarket(t, manager, nil)
}

func newPluginTestEnvWithMarket(t *testing.T, manager *plugin.Manager, marketClient *market.Client) *pluginTestEnv {
	t.Helper()
	store := newFakeTaskPluginStore()
	exec := &fakeAuditExec{}
	if manager == nil {
		sources, err := plugins.TaskPluginSources()
		if err != nil {
			t.Fatalf("TaskPluginSources: %v", err)
		}
		badKey := "bad-plugin"
		loader := &memoryVersionLoader{
			active: []plugin.ActiveVersion{
				{Key: "uploaded-x", Version: "2.0.0", SourceText: sourceForKey(t, "uploaded-x"),
					SourceHash: sha256HexForTest(sourceForKey(t, "uploaded-x")), Enabled: true, Active: true},
				{Key: badKey, Version: "1.0.0", SourceText: "this is not javascript",
					SourceHash: sha256HexForTest("this is not javascript"), Enabled: true, Active: true},
			},
			byKey: map[string]plugin.ActiveVersion{},
		}
		created, err := plugin.NewManager(sources, loader, plugin.Options{MasterEnabled: true})
		if err != nil {
			t.Fatalf("NewManager: %v", err)
		}
		if err := created.Reload(t.Context()); err != nil {
			t.Fatalf("Reload: %v", err)
		}
		manager = created
	}
	handlers, err := NewTaskPluginHandlers(TaskPluginHandlerConfig{
		Store:   store,
		Manager: manager,
		Authz:   rbac.NewAuthzMiddleware(),
		Audit:   audit.New(exec),
		Access:  newFakeUserAccess(),
		Market:  marketClient,
	})
	if err != nil {
		t.Fatalf("NewTaskPluginHandlers: %v", err)
	}
	mux := http.NewServeMux()
	handlers.Register(mux)
	return &pluginTestEnv{mux: mux, store: store, manager: manager, audit: exec, handlers: handlers}
}

// sha256HexForTest 直接计算字节哈希：测试 loader 里可能出现故意不可编译的
// 源（compile_failed 状态），不能先编译再取 hash。
func sha256HexForTest(source string) string {
	sum := sha256.Sum256([]byte(source))
	return hex.EncodeToString(sum[:])
}

func pluginAdmin() *auth.Principal {
	return &auth.Principal{UserID: 1, Role: rbac.RoleAdmin, Permissions: []string{rbac.PermSettingsManage}}
}

func pluginAdminNoWrite() *auth.Principal {
	return &auth.Principal{UserID: 2, Role: rbac.RoleAdmin, Permissions: []string{rbac.PermUsersRead}}
}

func pluginUser() *auth.Principal {
	return &auth.Principal{UserID: 3, Role: rbac.RoleUser}
}

func TestTaskPluginPermissionMatrix(t *testing.T) {
	env := newPluginTestEnv(t, nil)

	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodGet, "/api/plugin/task", "", pluginUser(), nil))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("普通用户读列表 = %d，期望 403", rr.Code)
	}

	// 裁决 R-M6D-16：读端点同样要求 settings:manage（受限 admin 不得读源码/诊断）。
	rr = httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodGet, "/api/plugin/task", "", pluginAdminNoWrite(), nil))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("无 settings:manage 的 admin 读列表 = %d，期望 403", rr.Code)
	}
	for _, path := range []string{
		"/api/plugin/task/runtime/status",
		"/api/plugin/task/marketplace",
		"/api/plugin/task/marketplace/sources",
		"/api/plugin/task/sample",
		"/api/plugin/task/sample/versions",
		"/api/plugin/task/sample/source",
	} {
		rr = httptest.NewRecorder()
		env.mux.ServeHTTP(rr, settingsRequest(http.MethodGet, path, "", pluginAdminNoWrite(), nil))
		if rr.Code != http.StatusForbidden {
			t.Fatalf("无 settings:manage 读 %s = %d，期望 403", path, rr.Code)
		}
	}

	rr = httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodGet, "/api/plugin/task", "", pluginAdmin(), nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("admin 读列表 = %d，期望 200（body=%s）", rr.Code, rr.Body.String())
	}

	// 上传：只有 admin 角色还不够，必须持有 settings:manage（或 FULL）。
	body := `{"source_text":` + mustJSON(t, samplePluginSource(t)) + `}`
	rr = httptest.NewRecorder()
	req := settingsRequest(http.MethodPost, "/api/plugin/task", body, pluginAdminNoWrite(), map[string]string{
		"Idempotency-Key": "upload-1",
	})
	env.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("缺 settings:manage 上传 = %d，期望 403（body=%s）", rr.Code, rr.Body.String())
	}
	if env.store.uploadCalls != 0 {
		t.Fatal("权限拒绝时不得调用存储写路径")
	}

	// 激活同样要求 settings:manage。
	rr = httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/plugin/task/sample/activate",
		`{"version":"1.0.0"}`, pluginAdminNoWrite(), nil))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("缺 settings:manage 激活 = %d，期望 403", rr.Code)
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}

func TestTaskPluginUploadSuccessAndIdempotency(t *testing.T) {
	env := newPluginTestEnv(t, nil)
	body := `{"source_text":` + mustJSON(t, samplePluginSource(t)) + `}`

	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/plugin/task", body, pluginAdmin(),
		map[string]string{"Idempotency-Key": "upload-a"}))
	if rr.Code != http.StatusCreated {
		t.Fatalf("首次上传 = %d，期望 201（body=%s）", rr.Code, rr.Body.String())
	}
	var first map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &first); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if first["created"] != true {
		t.Fatalf("created = %v，期望 true", first["created"])
	}
	if strings.Contains(rr.Body.String(), "buildSubmitRequest") {
		t.Fatal("上传响应不得包含源码正文")
	}

	// 同 (key,version) 同 hash 幂等：同 Idempotency-Key 直接重放首次响应。
	rr = httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/plugin/task", body, pluginAdmin(),
		map[string]string{"Idempotency-Key": "upload-a"}))
	if rr.Code != http.StatusCreated {
		t.Fatalf("同键重放 = %d，期望 201", rr.Code)
	}
	if rr.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatal("同键重放必须带 Idempotent-Replayed=true")
	}
	if env.store.uploadCalls != 1 {
		t.Fatalf("重放不得再次执行写路径，uploadCalls=%d", env.store.uploadCalls)
	}

	// 不同 Idempotency-Key、同内容：仓储返回已有行，HTTP 200。
	rr = httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/plugin/task", body, pluginAdmin(),
		map[string]string{"Idempotency-Key": "upload-b"}))
	if rr.Code != http.StatusOK {
		t.Fatalf("同内容再次上传 = %d，期望 200（body=%s）", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"created":false`) {
		t.Fatalf("幂等返回应标记 created=false：%s", rr.Body.String())
	}
}

func TestTaskPluginUploadCompileFailureWritesNothing(t *testing.T) {
	env := newPluginTestEnv(t, nil)
	body := `{"source_text":"globalThis.__NEWTG_PLUGIN__ = { key: \"x\" }"}`

	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/plugin/task", body, pluginAdmin(),
		map[string]string{"Idempotency-Key": "bad-source"}))
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("非法源 = %d，期望 422（body=%s）", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "plugin_compile_failed") {
		t.Fatalf("错误码应为 plugin_compile_failed：%s", rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"issues"`) {
		t.Fatalf("编译失败应返回结构化 issues：%s", rr.Body.String())
	}
	if env.store.uploadCalls != 0 {
		t.Fatalf("编译失败不得写入，uploadCalls=%d", env.store.uploadCalls)
	}
}

func TestTaskPluginUploadRequiresIdempotencyKey(t *testing.T) {
	env := newPluginTestEnv(t, nil)
	body := `{"source_text":"x"}`
	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/plugin/task", body, pluginAdmin(), nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("缺 Idempotency-Key = %d，期望 400", rr.Code)
	}
	if env.store.uploadCalls != 0 {
		t.Fatal("缺幂等键不得执行写路径")
	}
}

func TestTaskPluginListFiltersAndPagination(t *testing.T) {
	env := newPluginTestEnv(t, nil)
	now := time.Now().UTC()
	env.store.seed(store.TaskPlugin{Key: "sample", Name: "Sample", Source: "builtin", Builtin: true,
		Enabled: true, UpdatedAt: now})
	env.store.seed(store.TaskPlugin{Key: "uploaded-x", Name: "Uploaded X", Source: "upload",
		Enabled: false, UpdatedAt: now})
	env.store.seed(store.TaskPlugin{Key: "bad-plugin", Name: "Bad", Source: "upload",
		Enabled: true, UpdatedAt: now})
	env.store.seed(store.TaskPlugin{Key: "ghost", Name: "Ghost", Source: "upload",
		Enabled: true, UpdatedAt: now})

	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodGet,
		"/api/plugin/task?search=uploaded", "", pluginAdmin(), nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("search 列表 = %d（body=%s）", rr.Code, rr.Body.String())
	}
	var page struct {
		Items []struct {
			Key           string `json:"key"`
			RuntimeStatus string `json:"runtime_status"`
		} `json:"items"`
		Total    int `json:"total"`
		Page     int `json:"page"`
		PageSize int `json:"page_size"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].Key != "uploaded-x" {
		t.Fatalf("search 结果不符：%+v", page)
	}

	rr = httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodGet,
		"/api/plugin/task?runtime_status=not_registered&page_size=1", "", pluginAdmin(), nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("runtime 筛选 = %d", rr.Code)
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if page.Total != 1 || page.Items[0].Key != "ghost" {
		t.Fatalf("not_registered 筛选不符：%+v", page)
	}

	rr = httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodGet,
		"/api/plugin/task?p=2&page_size=2", "", pluginAdmin(), nil))
	if err := json.Unmarshal(rr.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if page.Total != 4 || page.Page != 2 || page.PageSize != 2 || len(page.Items) != 2 {
		t.Fatalf("分页不符：%+v", page)
	}

	// 默认排序：异常状态优先（compile_failed → not_registered → registered）。
	rr = httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodGet, "/api/plugin/task", "", pluginAdmin(), nil))
	if err := json.Unmarshal(rr.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if page.Items[0].Key != "bad-plugin" {
		t.Fatalf("默认排序首个应为 compile_failed 的 bad-plugin：%+v", page.Items)
	}
}

func TestTaskPluginRuntimeStatusFiveStates(t *testing.T) {
	env := newPluginTestEnv(t, nil)
	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodGet, "/api/plugin/task/runtime/status", "", pluginAdmin(), nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("runtime status = %d（body=%s）", rr.Code, rr.Body.String())
	}
	var status plugin.RuntimeStatus
	if err := json.Unmarshal(rr.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if status.Node == "" || status.Generation == 0 {
		t.Fatalf("runtime status 缺 node/generation：%+v", status)
	}
	states := map[plugin.PluginStatus]bool{}
	for _, p := range status.Plugins {
		states[p.Status] = true
	}
	if !states[plugin.StatusRegistered] {
		t.Fatalf("快照缺少 registered：%+v", status.Plugins)
	}
	if !states[plugin.StatusCompileFailed] {
		t.Fatalf("快照缺少 compile_failed：%+v", status.Plugins)
	}
}

func TestTaskPluginDetailAndSourceViewer(t *testing.T) {
	env := newPluginTestEnv(t, nil)
	env.store.seed(store.TaskPlugin{Key: "sample", Name: "Sample", Source: "builtin", Builtin: true,
		Enabled: true, ActiveVersion: strPtr("1.0.0"), UpdatedAt: time.Now().UTC()},
		store.TaskPluginVersion{ID: "1", Key: "sample", Version: "1.0.0", APIVersion: "v1",
			SourceText: samplePluginSource(t), SourceHash: "abc", Enabled: true, Active: true},
	)

	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodGet, "/api/plugin/task/sample", "", pluginAdmin(), nil))
	if rr.Code != http.StatusOK || strings.Contains(rr.Body.String(), "buildSubmitRequest") {
		t.Fatalf("默认详情不得含源码：%d %s", rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodGet, "/api/plugin/task/sample?include_source=1", "", pluginAdmin(), nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "buildSubmitRequest") {
		t.Fatalf("include_source=1 应返回源码：%d %s", rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodGet, "/api/plugin/task/sample/source?version=1.0.0", "", pluginAdmin(), nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "source_text") {
		t.Fatalf("源查看器 = %d %s", rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodGet, "/api/plugin/task/sample/source?version=9.9.9", "", pluginAdmin(), nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("不存在的版本 = %d，期望 404", rr.Code)
	}
}

func TestTaskPluginDryRunZeroSideEffectsAndOutboundRejection(t *testing.T) {
	env := newPluginTestEnv(t, nil)
	env.store.seed(store.TaskPlugin{Key: "sample", Name: "Sample", Source: "builtin", Builtin: true,
		Enabled: true, UpdatedAt: time.Now().UTC()})
	beforeUploads := env.store.uploadCalls

	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/plugin/task/sample/dryrun",
		`{"endpoint":"submit","payload":{"model":"sample-model","prompt":"hi"}}`, pluginAdmin(), nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("dryrun = %d（body=%s）", rr.Code, rr.Body.String())
	}
	if env.store.uploadCalls != beforeUploads {
		t.Fatal("dryrun 不得写库")
	}

	rr = httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/plugin/task/sample/dryrun",
		`{"endpoint":"submit","payload":{},"allow_outbound":true}`, pluginAdmin(), nil))
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("allow_outbound=true = %d，期望 422（body=%s）", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "dryrun_outbound_unavailable") {
		t.Fatalf("错误码应为 dryrun_outbound_unavailable：%s", rr.Body.String())
	}
}

func TestTaskPluginWritesRequireOptimisticLock(t *testing.T) {
	env := newPluginTestEnv(t, nil)
	stale := time.Now().Add(-time.Hour).UTC()
	env.store.seed(store.TaskPlugin{Key: "sample", Name: "Sample", Source: "builtin", Builtin: true,
		Enabled: true, UpdatedAt: time.Now().UTC()},
		store.TaskPluginVersion{ID: "1", Key: "sample", Version: "1.0.0", Enabled: true,
			Active: true, SourceText: samplePluginSource(t)},
	)
	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/plugin/task/sample/status",
		`{"enabled":false,"expect_updated_at":"`+stale.Format(time.RFC3339Nano)+`"}`, pluginAdmin(), nil))
	if rr.Code != http.StatusConflict {
		t.Fatalf("陈旧 expect_updated_at = %d，期望 409（body=%s）", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "plugin_updated_at_conflict") {
		t.Fatalf("错误码应为 plugin_updated_at_conflict：%s", rr.Body.String())
	}
}

func TestTaskPluginAuditRedactsSourceText(t *testing.T) {
	env := newPluginTestEnv(t, nil)
	body := `{"source_text":` + mustJSON(t, samplePluginSource(t)) + `}`
	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/plugin/task", body, pluginAdmin(),
		map[string]string{"Idempotency-Key": "audit-upload"}))
	if rr.Code != http.StatusCreated {
		t.Fatalf("上传 = %d（body=%s）", rr.Code, rr.Body.String())
	}
	calls := env.audit.snapshot()
	if len(calls) == 0 {
		t.Fatal("上传必须写审计")
	}
	detail := fmt.Sprint(calls[len(calls)-1][9])
	if !strings.Contains(detail, `"plugin.upload"`) ||
		!strings.Contains(detail, `"source_hash_prefix"`) {
		t.Fatalf("审计 detail 缺动作/哈希前缀：%s", detail)
	}
	if strings.Contains(detail, "buildSubmitRequest") || strings.Contains(detail, "globalThis") {
		t.Fatalf("审计 detail 不得包含源码正文：%s", detail)
	}
}

func TestTaskPluginUploadSizeLimit(t *testing.T) {
	env := newPluginTestEnv(t, nil)
	tooLarge := strings.Repeat("a", maxPluginUploadDefault+1)
	body := `{"source_text":` + mustJSON(t, tooLarge) + `}`
	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/plugin/task", body, pluginAdmin(),
		map[string]string{"Idempotency-Key": "too-large"}))
	if rr.Code != http.StatusUnprocessableEntity || !strings.Contains(rr.Body.String(), "plugin_source_too_large") {
		t.Fatalf("超限上传 = %d（body=%s），期望 422 plugin_source_too_large", rr.Code, rr.Body.String())
	}
	if env.store.uploadCalls != 0 {
		t.Fatal("超限上传不得写库")
	}
}

func TestTaskPluginRuntimeStatusAllFiveStates(t *testing.T) {
	factorySources := map[string]string{
		"sample":      samplePluginSource(t),
		"factory-ok":  sourceForKey(t, "factory-ok"),
		"factory-off": sourceForKey(t, "factory-off"),
	}
	loader := &memoryVersionLoader{
		active: []plugin.ActiveVersion{
			{Key: "sample", Version: "1.0.0", SourceText: sourceForKey(t, "sample"),
				SourceHash: sha256HexForTest(sourceForKey(t, "sample")), Enabled: true, Active: true},
			{Key: "bad-plugin", Version: "1.0.0", SourceText: "this is not javascript",
				SourceHash: sha256HexForTest("this is not javascript"), Enabled: true, Active: true},
		},
	}
	manager, err := plugin.NewManager(factorySources, loader, plugin.Options{
		MasterEnabled:       true,
		DisabledFactoryKeys: []string{"sample", "factory-off"},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := manager.Reload(t.Context()); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	env := newPluginTestEnv(t, manager)
	env.store.seed(store.TaskPlugin{Key: "ghost", Name: "Ghost", Source: "upload",
		Enabled: true, UpdatedAt: time.Now().UTC()})

	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodGet, "/api/plugin/task/runtime/status", "", pluginAdmin(), nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("runtime status = %d", rr.Code)
	}
	var status plugin.RuntimeStatus
	if err := json.Unmarshal(rr.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode: %v", err)
	}
	states := map[plugin.PluginStatus]bool{}
	for _, p := range status.Plugins {
		states[p.Status] = true
	}
	for _, want := range []plugin.PluginStatus{
		plugin.StatusRegistered, plugin.StatusDisabledFallback,
		plugin.StatusDisabled, plugin.StatusCompileFailed,
	} {
		if !states[want] {
			t.Fatalf("运行时快照缺状态 %s：%+v", want, status.Plugins)
		}
	}

	// not_registered 只可能出现在「DB 有行但注册表无层」的列表投影里。
	rr = httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodGet,
		"/api/plugin/task?runtime_status=not_registered", "", pluginAdmin(), nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"key":"ghost"`) {
		t.Fatalf("not_registered 列表 = %d %s", rr.Code, rr.Body.String())
	}
}

func TestTaskPluginUploadMultipart(t *testing.T) {
	env := newPluginTestEnv(t, nil)
	source := sourceForKey(t, "mp-upload")

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	part, err := writer.CreateFormFile("file", "plugin.js")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := part.Write([]byte(source)); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/plugin/task", &buf)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Idempotency-Key", "multipart-1")
	req = req.WithContext(auth.WithPrincipal(req.Context(), pluginAdmin()))
	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("multipart 上传 = %d（body=%s）", rr.Code, rr.Body.String())
	}

	// 2 MiB + 1 字节：在读取阶段被专用 MaxBytesReader/正文校验拒绝。
	var big bytes.Buffer
	bigWriter := multipart.NewWriter(&big)
	bigPart, _ := bigWriter.CreateFormFile("file", "plugin.js")
	_, _ = bigPart.Write([]byte(strings.Repeat("a", maxPluginUploadDefault+1)))
	_ = bigWriter.Close()
	req = httptest.NewRequest(http.MethodPost, "/api/plugin/task", &big)
	req.Header.Set("Content-Type", bigWriter.FormDataContentType())
	req.Header.Set("Idempotency-Key", "multipart-too-large")
	req = req.WithContext(auth.WithPrincipal(req.Context(), pluginAdmin()))
	rr = httptest.NewRecorder()
	env.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnprocessableEntity || !strings.Contains(rr.Body.String(), "plugin_source_too_large") {
		t.Fatalf("multipart 超限 = %d（body=%s）", rr.Code, rr.Body.String())
	}
}

func TestTaskPluginDryRunAuditRedactsPayload(t *testing.T) {
	env := newPluginTestEnv(t, nil)
	env.store.seed(store.TaskPlugin{Key: "sample", Name: "Sample", Source: "builtin", Builtin: true,
		Enabled: true, UpdatedAt: time.Now().UTC()})
	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/plugin/task/sample/dryrun",
		`{"endpoint":"submit","payload":{"prompt":"super-secret-prompt"}}`, pluginAdmin(), nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("dryrun = %d（body=%s）", rr.Code, rr.Body.String())
	}
	calls := env.audit.snapshot()
	if len(calls) == 0 {
		t.Fatal("dryrun 必须写审计")
	}
	detail := fmt.Sprint(calls[len(calls)-1][9])
	if strings.Contains(detail, "super-secret-prompt") || strings.Contains(detail, `"payload"`) {
		t.Fatalf("dryrun 审计不得包含样例请求体：%s", detail)
	}
	if !strings.Contains(detail, `"plugin.dryrun"`) || !strings.Contains(detail, `"plugin_key":"sample"`) {
		t.Fatalf("dryrun 审计缺动作/key：%s", detail)
	}
}
