package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/deploy/ionet"
	"newtg/gateway/internal/rbac"
	"newtg/gateway/internal/settings"
)

// 本文件验收作用域化部署设置端点：
//   - 投影只有 enabled/has_secret/can_connect/updated_at，密钥零回显；
//   - PUT 是 section=deploy 的别名（同一 service、同一乐观锁、clear 语义）；
//   - 连接测试的候选密钥只在内存中使用，不写日志/审计/响应；
//   - provider 401/403 → 422，超时 → 504，限流 5 次/分钟。

type fakeDeploySecretStore struct {
	key string
	ok  bool
	err error
}

func (f fakeDeploySecretStore) StoredAPIKey(context.Context) (string, bool, error) {
	return f.key, f.ok, f.err
}

func sampleDeployView(enabled bool, hasSecret bool) settings.SectionView {
	var secretValue any
	if hasSecret {
		secretValue = settings.MaskedValue
	}
	updated := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	return settings.SectionView{
		Section:   "deploy",
		UpdatedAt: &updated,
		Items: []settings.Item{
			{
				Key: deployEnabledKey, Type: settings.TypeBool, Default: false,
				Value: enabled, Dangerous: true, DangerDirection: settings.DangerDirectionOff,
			},
			{
				Key: deployAPIKeyKey, Type: settings.TypeString, Default: "", Secret: true,
				Value: secretValue, Dangerous: true, DangerDirection: settings.DangerDirectionAny,
			},
		},
	}
}

type deployTestEnv struct {
	mux      *http.ServeMux
	service  *fakeSettingsService
	provider *ionet.FakeProvider
	secrets  *fakeDeploySecretStore
	audit    *fakeAuditExec
	logger   *audit.Logger
	mu       sync.Mutex
	usedKeys []string
}

func newDeployTestEnv(t *testing.T, rdb *redis.Client) *deployTestEnv {
	t.Helper()
	return newDeployTestEnvWithLogger(t, rdb, testLogger())
}

// newDeployTestEnvWithLogger 允许注入可捕获的 logger，用于「日志零回显」断言。
func newDeployTestEnvWithLogger(t *testing.T, rdb *redis.Client, logger *slog.Logger) *deployTestEnv {
	t.Helper()
	service := &fakeSettingsService{getView: sampleDeployView(false, false)}
	provider := &ionet.FakeProvider{}
	secrets := &fakeDeploySecretStore{}
	exec := &fakeAuditExec{}
	auditLogger := audit.New(exec)
	env := &deployTestEnv{
		service: service, provider: provider, secrets: secrets, audit: exec, logger: auditLogger,
	}
	handlers, err := NewDeploySettingsHandlers(DeploySettingsHandlerConfig{
		Settings: service,
		Secrets:  secrets,
		Provider: func(apiKey string) ionet.DeploymentProvider {
			env.mu.Lock()
			env.usedKeys = append(env.usedKeys, apiKey)
			env.mu.Unlock()
			return provider
		},
		Authz:  rbac.NewAuthzMiddleware(),
		Audit:  auditLogger,
		Redis:  rdb,
		Logger: logger,
	})
	if err != nil {
		t.Fatalf("NewDeploySettingsHandlers: %v", err)
	}
	mux := http.NewServeMux()
	handlers.Register(mux)
	env.mux = mux
	return env
}

func deployManager() *auth.Principal {
	return &auth.Principal{UserID: 1, Role: rbac.RoleAdmin, Permissions: []string{rbac.PermSettingsManage}}
}

func (e *deployTestEnv) keysUsed() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.usedKeys...)
}

func TestDeploySettingsGetProjectionHasNoSecret(t *testing.T) {
	env := newDeployTestEnv(t, nil)
	env.service.getView = sampleDeployView(true, true)

	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodGet, "/api/deployments/settings", "", deployManager(), nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d（body=%s）", rr.Code, rr.Body.String())
	}
	var body struct {
		Enabled    bool       `json:"enabled"`
		HasSecret  bool       `json:"has_secret"`
		CanConnect bool       `json:"can_connect"`
		UpdatedAt  *time.Time `json:"updated_at"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Enabled || !body.HasSecret || !body.CanConnect || body.UpdatedAt == nil {
		t.Fatalf("投影 = %+v", body)
	}
	// 零回显：即使 section 视图里带着掩码值，响应也不得有密钥字段或掩码。
	if strings.Contains(rr.Body.String(), settings.MaskedValue) ||
		strings.Contains(rr.Body.String(), "api_key") {
		t.Fatalf("响应泄漏了密钥面: %s", rr.Body.String())
	}

	// 关闭开关时 can_connect 必须为 false（即使已配置密钥）。
	env.service.getView = sampleDeployView(false, true)
	rr = httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodGet, "/api/deployments/settings", "", deployManager(), nil))
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if body.CanConnect {
		t.Fatalf("enabled=false 时 can_connect 应为 false: %+v", body)
	}
}

func TestDeploySettingsGetRequiresSettingsManage(t *testing.T) {
	env := newDeployTestEnv(t, nil)
	adminNoPerm := &auth.Principal{UserID: 2, Role: rbac.RoleAdmin, Permissions: []string{rbac.PermUsersRead}}
	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodGet, "/api/deployments/settings", "", adminNoPerm, nil))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("缺 settings:manage = %d，期望 403", rr.Code)
	}
}

func TestDeploySettingsPutIsScopedSectionAlias(t *testing.T) {
	env := newDeployTestEnv(t, nil)
	env.service.putResult = settings.UpdateResult{Section: "deploy", Values: map[string]any{}}
	env.service.getView = sampleDeployView(true, true)
	updated := time.Date(2026, 10, 19, 11, 0, 0, 0, time.UTC)

	body := `{"enabled":true,"api_key":"sk-live-123","updated_at":"2026-10-19T11:00:00Z"}`
	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodPut, "/api/deployments/settings", body, deployManager(), nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d（body=%s）", rr.Code, rr.Body.String())
	}

	in := env.service.lastInput
	if in.Section != "deploy" {
		t.Fatalf("section = %q", in.Section)
	}
	if !in.DangerousConfirmed {
		t.Fatal("作用域化 PUT 是 UI 确认后的提交路径，必须标记为已确认")
	}
	if in.ExpectUpdatedAt == nil || !in.ExpectUpdatedAt.Equal(updated) {
		t.Fatalf("updated_at 未传入乐观锁: %v", in.ExpectUpdatedAt)
	}
	if in.Actor != 1 {
		t.Fatalf("actor = %d", in.Actor)
	}
	if string(in.Values[deployEnabledKey]) != "true" {
		t.Fatalf("enabled = %s", in.Values[deployEnabledKey])
	}
	if string(in.Values[deployAPIKeyKey]) != `"sk-live-123"` {
		t.Fatalf("api_key = %s", in.Values[deployAPIKeyKey])
	}
}

func TestDeploySettingsPutSecretSemantics(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantKey  bool
		wantNull bool
		wantCode int
	}{
		{"留空不更新", `{"enabled":true,"api_key":""}`, false, false, http.StatusOK},
		{"掩码不更新", `{"enabled":true,"api_key":"******"}`, false, false, http.StatusOK},
		{"显式清除", `{"enabled":true,"clear_api_key":true}`, true, true, http.StatusOK},
		{"缺 enabled", `{"api_key":"x"}`, false, false, http.StatusBadRequest},
		{"清除与新值冲突", `{"enabled":true,"api_key":"x","clear_api_key":true}`, false, false, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newDeployTestEnv(t, nil)
			env.service.putResult = settings.UpdateResult{Section: "deploy", Values: map[string]any{}}
			rr := httptest.NewRecorder()
			env.mux.ServeHTTP(rr, settingsRequest(http.MethodPut, "/api/deployments/settings", tc.body, deployManager(), nil))
			if rr.Code != tc.wantCode {
				t.Fatalf("status = %d，期望 %d（body=%s）", rr.Code, tc.wantCode, rr.Body.String())
			}
			if tc.wantCode != http.StatusOK {
				if env.service.putCalls != 0 {
					t.Fatalf("非法请求不得调用服务层，实际 %d 次", env.service.putCalls)
				}
				return
			}
			raw, present := env.service.lastInput.Values[deployAPIKeyKey]
			if present != tc.wantKey {
				t.Fatalf("api_key 是否提交 = %v，期望 %v", present, tc.wantKey)
			}
			if tc.wantNull && string(raw) != "null" {
				t.Fatalf("clear 语义应为 null，实际 %s", raw)
			}
		})
	}
}

func TestDeploySettingsPutConflictMapping(t *testing.T) {
	env := newDeployTestEnv(t, nil)
	env.service.putErr = settings.ErrConflict
	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodPut, "/api/deployments/settings",
		`{"enabled":true}`, deployManager(), nil))
	if rr.Code != http.StatusConflict {
		t.Fatalf("陈旧 updated_at = %d，期望 409（body=%s）", rr.Code, rr.Body.String())
	}
}

func TestDeploySettingsPutAuditHasNoSecret(t *testing.T) {
	env := newDeployTestEnv(t, nil)
	env.service.putResult = settings.UpdateResult{Section: "deploy", Values: map[string]any{}}
	// Get 在 PUT 前后各调用一次：之前未配置，之后已启用且有密钥。
	env.service.getQueue = []settings.SectionView{
		sampleDeployView(false, false),
		sampleDeployView(true, true),
	}

	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodPut, "/api/deployments/settings",
		`{"enabled":true,"api_key":"sk-never-in-audit"}`, deployManager(), nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	calls := env.audit.snapshot()
	if len(calls) != 1 {
		t.Fatalf("审计次数 = %d，期望 1", len(calls))
	}
	args := calls[0]
	if args[2] != "operation" {
		t.Fatalf("category = %v", args[2])
	}
	detail, _ := args[9].(string)
	before, _ := args[10].(string)
	after, _ := args[11].(string)
	for name, payload := range map[string]string{"detail": detail, "before": before, "after": after} {
		if strings.Contains(payload, "sk-never-in-audit") {
			t.Fatalf("%s 泄漏密钥: %s", name, payload)
		}
	}
	if !strings.Contains(detail, `"has_secret":true`) || !strings.Contains(detail, `"provider":"io.net"`) {
		t.Fatalf("detail 缺少冻结字段: %s", detail)
	}
	if !strings.Contains(before, `"enabled":false`) || !strings.Contains(after, `"enabled":true`) {
		t.Fatalf("before/after 未记录 enabled 变化: %s / %s", before, after)
	}
}

func TestDeployConnectionTestMissingSecret(t *testing.T) {
	env := newDeployTestEnv(t, nil)
	env.secrets.ok = false

	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/deployments/settings/test-connection",
		`{}`, deployManager(), nil))
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d（body=%s）", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "deploy_secret_missing") {
		t.Fatalf("problem.type = %s", rr.Body.String())
	}
	if connection, _, _ := env.provider.Calls(); connection != 0 {
		t.Fatalf("缺密钥不得探测 provider，实际 %d 次", connection)
	}
	calls := env.audit.snapshot()
	if len(calls) != 1 {
		t.Fatalf("审计次数 = %d", len(calls))
	}
	if !strings.Contains(calls[0][9].(string), `"result":"failed"`) ||
		!strings.Contains(calls[0][9].(string), "deploy_secret_missing") {
		t.Fatalf("失败审计字段不全: %v", calls[0][9])
	}
}

func TestDeployConnectionTestUsesCandidateAndZeroEcho(t *testing.T) {
	env := newDeployTestEnv(t, nil)
	env.provider.Connection = ionet.ConnectionInfo{HardwareCount: 4, TotalAvailable: 9}

	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/deployments/settings/test-connection",
		`{"api_key":"sk-candidate-xyz"}`, deployManager(), nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d（body=%s）", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "sk-candidate-xyz") {
		t.Fatalf("候选密钥出现在响应: %s", rr.Body.String())
	}
	var body struct {
		OK             bool  `json:"ok"`
		HardwareCount  int   `json:"hardware_count"`
		TotalAvailable int   `json:"total_available"`
		LatencyMS      int64 `json:"latency_ms"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.OK || body.HardwareCount != 4 || body.TotalAvailable != 9 || body.LatencyMS < 0 {
		t.Fatalf("响应 = %+v", body)
	}
	if keys := env.keysUsed(); len(keys) != 1 || keys[0] != "sk-candidate-xyz" {
		t.Fatalf("provider 使用的密钥 = %v", keys)
	}
	calls := env.audit.snapshot()
	if len(calls) != 1 {
		t.Fatalf("审计次数 = %d", len(calls))
	}
	if strings.Contains(calls[0][9].(string), "sk-candidate-xyz") {
		t.Fatalf("候选密钥进入审计: %v", calls[0][9])
	}
	if !strings.Contains(calls[0][9].(string), `"result":"success"`) {
		t.Fatalf("成功审计缺少 result: %v", calls[0][9])
	}
}

func TestDeployConnectionTestUsesStoredSecret(t *testing.T) {
	env := newDeployTestEnv(t, nil)
	env.secrets.key = "stored-key"
	env.secrets.ok = true
	env.provider.Connection = ionet.ConnectionInfo{HardwareCount: 1, TotalAvailable: 1}

	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/deployments/settings/test-connection",
		`{}`, deployManager(), nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d（body=%s）", rr.Code, rr.Body.String())
	}
	if keys := env.keysUsed(); len(keys) != 1 || keys[0] != "stored-key" {
		t.Fatalf("provider 使用的已存密钥 = %v", keys)
	}
	if strings.Contains(rr.Body.String(), "stored-key") {
		t.Fatalf("已存密钥出现在响应")
	}
}

func TestDeployConnectionTestProviderErrors(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode int
		wantType string
	}{
		{"鉴权失败", &ionet.APIError{StatusCode: http.StatusUnauthorized, Detail: "bad key"}, http.StatusUnprocessableEntity, "deploy_secret_invalid"},
		{"禁止", &ionet.APIError{StatusCode: http.StatusForbidden, Detail: "no"}, http.StatusUnprocessableEntity, "deploy_secret_invalid"},
		{"超时", &timeoutError{}, http.StatusGatewayTimeout, "deploy_provider_timeout"},
		{"5xx", &ionet.APIError{StatusCode: http.StatusBadGateway, Detail: "down"}, http.StatusBadGateway, "deploy_provider_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newDeployTestEnv(t, nil)
			env.provider.ConnectionErr = tc.err
			rr := httptest.NewRecorder()
			env.mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/deployments/settings/test-connection",
				`{"api_key":"sk-x"}`, deployManager(), nil))
			if rr.Code != tc.wantCode {
				t.Fatalf("status = %d，期望 %d（body=%s）", rr.Code, tc.wantCode, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), tc.wantType) {
				t.Fatalf("problem.type 未包含 %q: %s", tc.wantType, rr.Body.String())
			}
		})
	}
}

// timeoutError 实现 net.Error 的 Timeout()，模拟 http.Client 超时。
type timeoutError struct{}

func (timeoutError) Error() string   { return "timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestDeployConnectionTestRateLimit(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })

	env := newDeployTestEnv(t, rdb)
	for i := 0; i < deployTestRateLimit; i++ {
		rr := httptest.NewRecorder()
		env.mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/deployments/settings/test-connection",
			`{"api_key":"sk-x"}`, deployManager(), nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("第 %d 次 = %d（body=%s）", i+1, rr.Code, rr.Body.String())
		}
	}
	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/deployments/settings/test-connection",
		`{"api_key":"sk-x"}`, deployManager(), nil))
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("第 6 次 = %d，期望 429（body=%s）", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Fatal("429 缺少 Retry-After")
	}
	if connection, _, _ := env.provider.Calls(); connection != deployTestRateLimit {
		t.Fatalf("provider 调用次数 = %d，期望 %d", connection, deployTestRateLimit)
	}
}

func TestDeployConnectionTestUnknownFieldRejected(t *testing.T) {
	env := newDeployTestEnv(t, nil)
	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/deployments/settings/test-connection",
		`{"api_key":"k","endpoint":"https://evil.example"}`, deployManager(), nil))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("未登记字段 = %d，期望 403（endpoint 不能被配置）", rr.Code)
	}
}

// provider 429 必须映射为 deploy_rate_limited 并把 Retry-After 带回管理端
// （§3.20.7：provider 429 → 429 + Retry-After）。
func TestDeployConnectionTestProviderRateLimit(t *testing.T) {
	env := newDeployTestEnv(t, nil)
	env.provider.ConnectionErr = &ionet.APIError{
		StatusCode: http.StatusTooManyRequests,
		Detail:     "provider throttle",
		RetryAfter: 2 * time.Second,
	}

	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/deployments/settings/test-connection",
		`{"api_key":"sk-x"}`, deployManager(), nil))
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d，期望 429（body=%s）", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "deploy_rate_limited") {
		t.Fatalf("problem.type = %s", rr.Body.String())
	}
	if got := rr.Header().Get("Retry-After"); got != "2" {
		t.Fatalf("Retry-After = %q，期望 provider 建议的 2", got)
	}
}

// 日志零回显：候选密钥与已存密钥都不得出现在任何日志行（成功与失败路径）。
func TestDeployConnectionTestNeverLogsSecrets(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// 失败路径：provider 返回已脱敏的 502（生产由 ionet 客户端完成脱敏）。
	failEnv := newDeployTestEnvWithLogger(t, nil, logger)
	failEnv.provider.ConnectionErr = &ionet.APIError{StatusCode: http.StatusBadGateway, Detail: "upstream failure"}
	rr := httptest.NewRecorder()
	failEnv.mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/deployments/settings/test-connection",
		`{"api_key":"sk-candidate-never-log"}`, deployManager(), nil))
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("失败路径 status = %d", rr.Code)
	}

	// 成功路径：使用已存密钥。
	okEnv := newDeployTestEnvWithLogger(t, nil, logger)
	okEnv.secrets.key = "sk-stored-never-log"
	okEnv.secrets.ok = true
	okEnv.provider.Connection = ionet.ConnectionInfo{HardwareCount: 1, TotalAvailable: 1}
	rr = httptest.NewRecorder()
	okEnv.mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/deployments/settings/test-connection",
		`{}`, deployManager(), nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("成功路径 status = %d", rr.Code)
	}

	logs := buf.String()
	for _, secret := range []string{"sk-candidate-never-log", "sk-stored-never-log"} {
		if strings.Contains(logs, secret) {
			t.Fatalf("日志泄漏密钥 %q：%s", secret, logs)
		}
	}
}

// Redis 故障时连接测试 fail-open：只读探测放行，并留下 Warn（不是静默）。
func TestDeployConnectionTestRateLimiterFailsOpen(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{
		Addr:         mr.Addr(),
		MaxRetries:   -1,
		DialTimeout:  50 * time.Millisecond,
		ReadTimeout:  50 * time.Millisecond,
		WriteTimeout: 50 * time.Millisecond,
	})
	t.Cleanup(func() { _ = rdb.Close() })
	mr.Close() // Redis 不可用

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	env := newDeployTestEnvWithLogger(t, rdb, logger)
	env.provider.Connection = ionet.ConnectionInfo{HardwareCount: 1}

	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/deployments/settings/test-connection",
		`{"api_key":"sk-x"}`, deployManager(), nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("Redis 故障应 fail-open，实际 %d（body=%s）", rr.Code, rr.Body.String())
	}
	if !strings.Contains(buf.String(), "rate limiter unavailable") {
		t.Fatalf("缺少限流降级 Warn: %s", buf.String())
	}
}
