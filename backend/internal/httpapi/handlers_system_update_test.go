package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/egress"
	"newtg/gateway/internal/rbac"
	"newtg/gateway/internal/updatecheck"
)

type updateTestConfig map[string]json.RawMessage

func (m updateTestConfig) Get(key string) (json.RawMessage, bool) {
	value, ok := m[key]
	return value, ok
}

func (m updateTestConfig) set(t *testing.T, key string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal %s: %v", key, err)
	}
	m[key] = raw
}

func newUpdateCheckMux(t *testing.T, reader updateTestConfig, version string) *http.ServeMux {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	checker, err := updatecheck.New(updatecheck.Config{
		Reader:         reader,
		Redis:          rdb,
		CurrentVersion: version,
		ClientFactory: func(egress.Policy) *http.Client {
			return &http.Client{}
		},
	})
	if err != nil {
		t.Fatalf("updatecheck.New: %v", err)
	}
	mux := http.NewServeMux()
	NewUpdateCheckHandlers(UpdateCheckHandlerConfig{Checker: checker}).Register(mux)
	return mux
}

func TestUpdateCheckEndpointAdminOnly(t *testing.T) {
	reader := updateTestConfig{}
	mux := newUpdateCheckMux(t, reader, "1.0.0")

	user := &auth.Principal{UserID: 7, Role: rbac.RoleUser}
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodGet, "/api/system/update-check", "", user, nil))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("普通用户 = %d，期望 403", rr.Code)
	}

	admin := &auth.Principal{UserID: 1, Role: rbac.RoleAdmin}
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodGet, "/api/system/update-check", "", admin, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("admin GET = %d（body=%s）", rr.Code, rr.Body.String())
	}
	var body updatecheck.Result
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("JSON 非法: %v", err)
	}
	if body.Enabled || body.State != updatecheck.StateDisabled {
		t.Fatalf("默认应为 disabled: %+v", body)
	}
}

func TestUpdateCheckEndpointMapsRateLimitTo429(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"latest":"1.1.0"}`))
	}))
	t.Cleanup(server.Close)

	reader := updateTestConfig{}
	reader.set(t, updatecheck.KeyEnabled, true)
	reader.set(t, updatecheck.KeySourceURL, server.URL)
	reader.set(t, "sec.ssrf.mode", "off")
	mux := newUpdateCheckMux(t, reader, "1.0.0")

	admin := &auth.Principal{UserID: 1, Role: rbac.RoleAdmin}
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/system/update-check", "", admin, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("首次立即检查 = %d（body=%s）", rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/system/update-check", "", admin, nil))
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("60s 内二次检查 = %d，期望 429", rr.Code)
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Fatal("429 必须带 Retry-After")
	}
}

func TestUpdateCheckEndpointWithoutChecker(t *testing.T) {
	mux := http.NewServeMux()
	NewUpdateCheckHandlers(UpdateCheckHandlerConfig{}).Register(mux)
	admin := &auth.Principal{UserID: 1, Role: rbac.RoleAdmin}
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodGet, "/api/system/update-check", "", admin, nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("未装配 checker = %d，期望 503", rr.Code)
	}
}
