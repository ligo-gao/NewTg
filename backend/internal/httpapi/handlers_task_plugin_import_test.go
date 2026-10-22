package httpapi

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"newtg/gateway/internal/plugin/market"
)

// importEnv 在标准插件测试环境上挂载 URL 导入端点。
//
// 生产装配在 routes.go 统一挂载；单测直接构造 mux，因此显式补这一次
// 注册，避免所有既有用例都被导入路由影响。
func importEnv(t *testing.T, marketClient *market.Client) *pluginTestEnv {
	t.Helper()
	env := newPluginTestEnvWithMarket(t, nil, marketClient)
	env.handlers.RegisterTaskPluginImport(env.mux)
	return env
}

func importRequest(body string, idempotencyKey string) *http.Request {
	headers := map[string]string{}
	if idempotencyKey != "" {
		headers["Idempotency-Key"] = idempotencyKey
	}
	return settingsRequest(http.MethodPost, "/api/plugin/task:import", body, pluginAdmin(), headers)
}

func TestTaskPluginImportFromURL(t *testing.T) {
	source := sourceForKey(t, "market-one")
	marketServer := newMarketTestServer(t, "market-one", "1.0.0", source, "")
	env := importEnv(t, marketServer.client(t))

	body := `{"url":"https://market.example/plugin.js","sha256":"` + marketServer.sourceHash + `"}`
	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, importRequest(body, "import-1"))
	if rr.Code != http.StatusCreated {
		t.Fatalf("URL 导入 = %d，期望 201（body=%s）", rr.Code, rr.Body.String())
	}
	var created struct {
		Created bool `json:"created"`
		Version struct {
			Version string `json:"version"`
			Active  bool   `json:"active"`
		} `json:"version"`
		Plugin struct {
			Key    string `json:"key"`
			Source string `json:"source"`
		} `json:"plugin"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !created.Created || created.Plugin.Key != "market-one" || created.Version.Version != "1.0.0" {
		t.Fatalf("导入响应不符：%+v", created)
	}
	if created.Version.Active {
		t.Fatal("URL 导入默认不得激活（F-08-30）")
	}
	if created.Plugin.Source != "upload" {
		t.Fatalf("导入来源 = %q，期望 upload", created.Plugin.Source)
	}

	// 同 Idempotency-Key 重放首次响应，不产生第二次入库。
	rr = httptest.NewRecorder()
	env.mux.ServeHTTP(rr, importRequest(body, "import-1"))
	if rr.Code != http.StatusCreated {
		t.Fatalf("同键重放 = %d，期望 201（body=%s）", rr.Code, rr.Body.String())
	}
	if env.store.uploadCalls != 1 {
		t.Fatalf("同键重放不得再次入库，uploadCalls=%d", env.store.uploadCalls)
	}
}

func TestTaskPluginImportHashOptional(t *testing.T) {
	source := sourceForKey(t, "market-one")
	marketServer := newMarketTestServer(t, "market-one", "1.0.0", source, "")
	env := importEnv(t, marketServer.client(t))

	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, importRequest(`{"url":"https://market.example/plugin.js"}`, "import-no-hash"))
	if rr.Code != http.StatusCreated {
		t.Fatalf("不带 sha256 导入 = %d，期望 201（body=%s）", rr.Code, rr.Body.String())
	}
}

func TestTaskPluginImportHashMismatchWritesNothing(t *testing.T) {
	source := sourceForKey(t, "market-one")
	marketServer := newMarketTestServer(t, "market-one", "1.0.0", source, "")
	env := importEnv(t, marketServer.client(t))

	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, importRequest(
		`{"url":"https://market.example/plugin.js","sha256":"`+strings.Repeat("0", 64)+`"}`,
		"import-bad-hash"))
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("hash 不符 = %d，期望 422（body=%s）", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "plugin_hash_mismatch") {
		t.Fatalf("错误码应为 plugin_hash_mismatch：%s", rr.Body.String())
	}
	if env.store.uploadCalls != 0 {
		t.Fatalf("hash 不符必须零写入，uploadCalls=%d", env.store.uploadCalls)
	}
}

func TestTaskPluginImportRejectsInsecureAndRedirect(t *testing.T) {
	t.Run("http URL", func(t *testing.T) {
		env := importEnv(t, nil)
		rr := httptest.NewRecorder()
		env.mux.ServeHTTP(rr, importRequest(
			`{"url":"http://market.example/plugin.js"}`, "import-http"))
		if rr.Code != http.StatusUnprocessableEntity ||
			!strings.Contains(rr.Body.String(), "plugin_source_url_rejected") {
			t.Fatalf("http URL = %d（body=%s），期望 422 plugin_source_url_rejected",
				rr.Code, rr.Body.String())
		}
		if env.store.uploadCalls != 0 {
			t.Fatal("URL 被拒时必须零写入")
		}
	})

	t.Run("redirect is not followed", func(t *testing.T) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://elsewhere.example/plugin.js", http.StatusFound)
		}))
		t.Cleanup(server.Close)
		env := importEnv(t, marketClientForServer(t, server))
		rr := httptest.NewRecorder()
		env.mux.ServeHTTP(rr, importRequest(
			`{"url":"https://market.example/plugin.js"}`, "import-redirect"))
		if rr.Code != http.StatusUnprocessableEntity ||
			!strings.Contains(rr.Body.String(), "plugin_source_url_rejected") {
			t.Fatalf("重定向 = %d（body=%s），期望 422 plugin_source_url_rejected",
				rr.Code, rr.Body.String())
		}
		if env.store.uploadCalls != 0 {
			t.Fatal("重定向必须零写入")
		}
	})
}

func TestTaskPluginImportHonorsUploadSizeLimit(t *testing.T) {
	source := strings.Repeat("a", 1025)
	marketServer := newMarketTestServer(t, "market-one", "1.0.0", source, "")
	env := importEnv(t, marketServer.client(t))
	env.handlers.config = mapConfig{"plugin.upload.max_bytes": json.RawMessage(`1024`)}

	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, importRequest(`{"url":"https://market.example/plugin.js"}`, "import-big"))
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("超限导入 = %d，期望 422（body=%s）", rr.Code, rr.Body.String())
	}
	if env.store.uploadCalls != 0 {
		t.Fatalf("超限必须零写入，uploadCalls=%d", env.store.uploadCalls)
	}
}

func TestTaskPluginImportRequiresSettingsManage(t *testing.T) {
	env := importEnv(t, nil)
	rr := httptest.NewRecorder()
	req := settingsRequest(http.MethodPost, "/api/plugin/task:import",
		`{"url":"https://market.example/plugin.js"}`, pluginAdminNoWrite(),
		map[string]string{"Idempotency-Key": "import-forbidden"})
	env.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("缺 settings:manage = %d，期望 403（body=%s）", rr.Code, rr.Body.String())
	}
}

// marketClientForServer 构造一个把任意主机重写到给定 httptest TLS 监听的
// 客户端（保留 https 校验与「3xx 不跟随」语义）。
func marketClientForServer(t *testing.T, server *httptest.Server) *market.Client {
	t.Helper()
	base, ok := server.Client().Transport.(*http.Transport)
	if !ok {
		t.Fatal("httptest client transport is not *http.Transport")
	}
	transport := base.Clone()
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec
	address := server.Listener.Addr().String()
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	return market.NewClientWithHTTP(&http.Client{Transport: transport})
}
