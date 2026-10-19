package httpapi

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"newtg/gateway/internal/plugin/market"
	"newtg/gateway/internal/store"
)

// marketTestServer 提供 /index.json 与 /plugin.js；host 用公开域名，
// transport 把连接重写到 httptest TLS 监听地址——这样最小 SSRF 守卫
// （拒绝环回字面量）不会把测试自己挡在门外。
type marketTestServer struct {
	server     *httptest.Server
	indexJSON  string
	sourceJS   string
	sourceHash string
}

func newMarketTestServer(t *testing.T, key, version, source string, indexHashOverride string) *marketTestServer {
	t.Helper()
	sum := sha256.Sum256([]byte(source))
	hash := hex.EncodeToString(sum[:])
	if indexHashOverride != "" {
		hash = indexHashOverride
	}
	index := map[string]any{
		"schema_version": 1,
		"plugins": []map[string]any{{
			"key": key, "name": "Market " + key, "description": "from marketplace",
			"icon_url": "",
			"versions": []map[string]any{{
				"version": version, "api_version": "v1",
				"source_url": "https://market.example/plugin.js", "source_hash": hash,
			}},
		}},
	}
	raw, err := json.Marshal(index)
	if err != nil {
		t.Fatalf("marshal index: %v", err)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/index.json":
			_, _ = w.Write(raw)
		case "/plugin.js":
			_, _ = w.Write([]byte(source))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return &marketTestServer{
		server: server, indexJSON: string(raw), sourceJS: source, sourceHash: hash,
	}
}

func (m *marketTestServer) client(t *testing.T) *market.Client {
	t.Helper()
	base, ok := m.server.Client().Transport.(*http.Transport)
	if !ok {
		t.Fatal("httptest client transport is not *http.Transport")
	}
	transport := base.Clone()
	// httptest 的证书只对 127.0.0.1 生效；这里显式跳过校验，测试关注
	// URL 守卫与 hash 校验，而不是本地 TLS 信任链。
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec
	address := m.server.Listener.Addr().String()
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	return &market.Client{HTTP: &http.Client{Transport: transport}}
}

func seedMarketSource(env *pluginTestEnv, id, url string) {
	env.store.mu.Lock()
	defer env.store.mu.Unlock()
	env.store.sources = append(env.store.sources, store.PluginMarketplaceSource{
		ID: id, Name: "Test market", URL: url, Enabled: true, LastCheckStatus: "pending",
	})
}

func TestMarketplaceSourcesRejectHTTP(t *testing.T) {
	env := newPluginTestEnv(t, nil)
	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodPut, "/api/plugin/task/marketplace/sources",
		`{"sources":[{"name":"insecure","url":"http://market.example/index.json","enabled":true}]}`,
		pluginAdmin(), nil))
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("http 源 = %d，期望 422（body=%s）", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "plugin_source_url_rejected") {
		t.Fatalf("错误码应为 plugin_source_url_rejected：%s", rr.Body.String())
	}
	if len(env.store.sources) != 0 {
		t.Fatal("被拒绝的源不得落库")
	}
}

func TestMarketplaceSourcesCRUDAndCheck(t *testing.T) {
	source := sourceForKey(t, "market-one")
	marketServer := newMarketTestServer(t, "market-one", "1.0.0", source, "")
	env := newPluginTestEnvWithMarket(t, nil, marketServer.client(t))

	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodPut, "/api/plugin/task/marketplace/sources",
		`{"sources":[{"name":"Main","url":"https://market.example/index.json","enabled":true}]}`,
		pluginAdmin(), nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("保存市场源 = %d（body=%s）", rr.Code, rr.Body.String())
	}
	var saved struct {
		Items []struct {
			ID              string `json:"id"`
			LastCheckStatus string `json:"last_check_status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &saved); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(saved.Items) != 1 || saved.Items[0].LastCheckStatus != "ok" {
		t.Fatalf("保存后应立即试拉并写回 ok：%+v", saved)
	}

	rr = httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodGet, "/api/plugin/task/marketplace/sources",
		"", pluginAdmin(), nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "Main") {
		t.Fatalf("读取市场源 = %d %s", rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodPost,
		"/api/plugin/task/marketplace/sources/"+saved.Items[0].ID+"/check", "", pluginAdmin(), nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"last_check_status":"ok"`) {
		t.Fatalf("显式试拉 = %d %s", rr.Code, rr.Body.String())
	}
}

func TestMarketplaceIndexAggregation(t *testing.T) {
	source := sourceForKey(t, "market-one")
	marketServer := newMarketTestServer(t, "market-one", "1.0.0", source, "")
	env := newPluginTestEnvWithMarket(t, nil, marketServer.client(t))
	seedMarketSource(env, "s1", "https://market.example/index.json")

	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodGet,
		"/api/plugin/task/marketplace?refresh=1", "", pluginAdmin(), nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("聚合索引 = %d（body=%s）", rr.Code, rr.Body.String())
	}
	var view struct {
		SchemaVersion int `json:"schema_version"`
		Plugins       []struct {
			Key           string `json:"key"`
			Installed     bool   `json:"installed"`
			LatestVersion string `json:"latest_version"`
			Versions      []struct {
				Version   string `json:"version"`
				Installed bool   `json:"installed"`
			} `json:"versions"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if view.SchemaVersion != 1 || len(view.Plugins) != 1 || view.Plugins[0].Key != "market-one" {
		t.Fatalf("聚合结果不符：%+v", view)
	}
	if view.Plugins[0].Installed || view.Plugins[0].LatestVersion != "1.0.0" {
		t.Fatalf("未安装插件标记错误：%+v", view.Plugins[0])
	}
}

func TestMarketplaceInstallHashMismatchWritesNothing(t *testing.T) {
	source := sourceForKey(t, "market-one")
	other := sha256.Sum256([]byte("tampered"))
	marketServer := newMarketTestServer(t, "market-one", "1.0.0", source, hex.EncodeToString(other[:]))
	env := newPluginTestEnvWithMarket(t, nil, marketServer.client(t))
	seedMarketSource(env, "s1", "https://market.example/index.json")

	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/plugin/task/marketplace/install",
		`{"source_id":"s1","key":"market-one","version":"1.0.0"}`, pluginAdmin(), nil))
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("hash 不符安装 = %d，期望 422（body=%s）", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "plugin_hash_mismatch") {
		t.Fatalf("错误码应为 plugin_hash_mismatch：%s", rr.Body.String())
	}
	if env.store.uploadCalls != 0 {
		t.Fatalf("hash 不符必须零写入，uploadCalls=%d", env.store.uploadCalls)
	}
}

func TestMarketplaceInstallSuccessDefaultsToInactive(t *testing.T) {
	source := sourceForKey(t, "market-one")
	marketServer := newMarketTestServer(t, "market-one", "1.0.0", source, "")
	env := newPluginTestEnvWithMarket(t, nil, marketServer.client(t))
	seedMarketSource(env, "s1", "https://market.example/index.json")

	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/plugin/task/marketplace/install",
		`{"source_id":"s1","key":"market-one","version":"1.0.0"}`, pluginAdmin(), nil))
	if rr.Code != http.StatusCreated {
		t.Fatalf("安装 = %d，期望 201（body=%s）", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"activated":false`) {
		t.Fatalf("默认不得自动激活：%s", rr.Body.String())
	}
	versions, _ := env.store.ListVersions(t.Context(), "market-one")
	if len(versions) != 1 || versions[0].Active {
		t.Fatalf("安装后应有一个非 active 版本：%+v", versions)
	}

	// 显式 activate=true 才会切换 active。
	rr = httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/plugin/task/marketplace/install",
		`{"source_id":"s1","key":"market-one","version":"1.0.0","activate":true}`, pluginAdmin(), nil))
	if rr.Code != http.StatusCreated && rr.Code != http.StatusOK {
		t.Fatalf("带激活安装 = %d（body=%s）", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"activated":true`) {
		t.Fatalf("activate=true 应返回 activated=true：%s", rr.Body.String())
	}
}

func TestMarketplacePackageMismatchRejected(t *testing.T) {
	// 索引声明 different 版本，但包体 manifest 是 sample 的 1.0.0；key 正确。
	source := sourceForKey(t, "market-one")
	marketServer := newMarketTestServer(t, "market-one", "9.9.9", source, "")
	env := newPluginTestEnvWithMarket(t, nil, marketServer.client(t))
	seedMarketSource(env, "s1", "https://market.example/index.json")

	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/plugin/task/marketplace/install",
		`{"source_id":"s1","key":"market-one","version":"9.9.9"}`, pluginAdmin(), nil))
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("包体版本不符 = %d，期望 422（body=%s）", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "plugin_package_mismatch") {
		t.Fatalf("错误码应为 plugin_package_mismatch：%s", rr.Body.String())
	}
	if env.store.uploadCalls != 0 {
		t.Fatalf("版本不符必须零写入，uploadCalls=%d", env.store.uploadCalls)
	}
}

func TestMarketplaceSourcesDuplicateURLRejected(t *testing.T) {
	env := newPluginTestEnv(t, nil)
	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodPut, "/api/plugin/task/marketplace/sources",
		`{"sources":[{"name":"A","url":"https://market.example/index.json","enabled":true},`+
			`{"name":"B","url":"https://market.example/index.json","enabled":true}]}`,
		pluginAdmin(), nil))
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "plugin_market_source_conflict") {
		t.Fatalf("重复 URL = %d（body=%s），期望 409", rr.Code, rr.Body.String())
	}
	if len(env.store.sources) != 0 {
		t.Fatal("重复 URL 不得落库")
	}
}

func TestMarketplaceDisabledBlocksMarketEndpoints(t *testing.T) {
	env := newPluginTestEnv(t, nil)
	env.handlers.config = mapConfig{"plugin.marketplace.enabled": configBool(false)}

	for _, tc := range []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodGet, "/api/plugin/task/marketplace", ""},
		{http.MethodPut, "/api/plugin/task/marketplace/sources", `{"sources":[]}`},
		{http.MethodPost, "/api/plugin/task/marketplace/install",
			`{"source_id":"s1","key":"x","version":"1.0.0"}`},
		{http.MethodPost, "/api/plugin/task/marketplace/sources/s1/check", ""},
	} {
		rr := httptest.NewRecorder()
		env.mux.ServeHTTP(rr, settingsRequest(tc.method, tc.path, tc.body, pluginAdmin(), nil))
		if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "plugin_marketplace_disabled") {
			t.Fatalf("%s %s = %d（body=%s），期望 409 plugin_marketplace_disabled",
				tc.method, tc.path, rr.Code, rr.Body.String())
		}
	}
}
