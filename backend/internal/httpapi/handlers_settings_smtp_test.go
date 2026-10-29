package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/notify"
	"newtg/gateway/internal/rbac"
)

// POST /api/settings/test/smtp 的单元验收：权限、结果映射、配置缺失、
// 内部错误脱敏与管理员限流。真实 SMTP 探测在 notify 包覆盖。

type fakeSMTPProber struct {
	configured bool
	result     notify.SMTPProbeResult
	err        error
	calls      int
}

func (f *fakeSMTPProber) Configured(context.Context) bool { return f.configured }

func (f *fakeSMTPProber) Target() (string, int) { return "smtp.example.test", 465 }

func (f *fakeSMTPProber) Probe(context.Context) (notify.SMTPProbeResult, error) {
	f.calls++
	return f.result, f.err
}

func smtpTestMux(t *testing.T, prober SMTPProber, auditLogger *audit.Logger, rdb *redis.Client) *http.ServeMux {
	t.Helper()
	handlers, err := NewSettingsHandlers(SettingsHandlerConfig{
		Service: &fakeSettingsService{},
		Authz:   rbac.NewAuthzMiddleware(),
		Audit:   auditLogger,
		Idem:    NewMemoryIdempotencyStore(),
		SMTP:    prober,
		Redis:   rdb,
	})
	if err != nil {
		t.Fatalf("NewSettingsHandlers: %v", err)
	}
	mux := http.NewServeMux()
	handlers.Register(mux)
	return mux
}

func TestSMTPTestEndpointResultMapping(t *testing.T) {
	steps := []notify.SMTPProbeStep{
		{Stage: notify.SMTPProbeStageDNS, Status: notify.SMTPProbeStatusPassed},
		{Stage: notify.SMTPProbeStageConnect, Status: notify.SMTPProbeStatusPassed},
		{Stage: notify.SMTPProbeStageTLS, Status: notify.SMTPProbeStatusSkipped},
		{Stage: notify.SMTPProbeStageGreeting, Status: notify.SMTPProbeStatusPassed},
		{Stage: notify.SMTPProbeStageAuth, Status: notify.SMTPProbeStatusFailed,
			Reason: notify.SMTPProbeReasonAuthFailed, SMTPCode: 535},
	}
	prober := &fakeSMTPProber{configured: true, result: notify.SMTPProbeResult{OK: false, Steps: steps}}
	mux := smtpTestMux(t, prober, nil, nil)

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/settings/test/smtp", "", webhookTestPrincipal(), nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d，期望 200（body=%s）", rr.Code, rr.Body.String())
	}
	var body smtpTestResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.OK {
		t.Fatal("探测失败时 ok 必须为 false")
	}
	if len(body.Steps) != len(steps) {
		t.Fatalf("steps 长度 = %d，期望 %d", len(body.Steps), len(steps))
	}
	last := body.Steps[len(body.Steps)-1]
	if last.Stage != notify.SMTPProbeStageAuth || last.Status != notify.SMTPProbeStatusFailed ||
		last.Reason != notify.SMTPProbeReasonAuthFailed || last.SMTPCode != 535 {
		t.Fatalf("最后一个阶段 = %+v", last)
	}
	if prober.calls != 1 {
		t.Fatalf("Probe 调用次数 = %d", prober.calls)
	}
}

func TestSMTPTestEndpointNotConfigured(t *testing.T) {
	prober := &fakeSMTPProber{configured: false}
	mux := smtpTestMux(t, prober, nil, nil)

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/settings/test/smtp", "", webhookTestPrincipal(), nil))
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d，期望 422（body=%s）", rr.Code, rr.Body.String())
	}
	var problem struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if problem.Type != smtpProblemNotConfigured {
		t.Fatalf("problem.type = %q，期望 %q", problem.Type, smtpProblemNotConfigured)
	}
	if prober.calls != 0 {
		t.Fatal("未配置时不应发起探测")
	}
}

func TestSMTPTestEndpointInternalError(t *testing.T) {
	prober := &fakeSMTPProber{configured: true, err: errors.New("open smtp password: internal detail")}
	mux := smtpTestMux(t, prober, nil, nil)

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/settings/test/smtp", "", webhookTestPrincipal(), nil))
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d，期望 500（body=%s）", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "internal detail") {
		t.Fatalf("响应泄露内部错误：%s", rr.Body.String())
	}
}

func TestSMTPTestEndpointRequiresSettingsManage(t *testing.T) {
	prober := &fakeSMTPProber{configured: true}
	mux := smtpTestMux(t, prober, nil, nil)

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/settings/test/smtp", "",
		&auth.Principal{UserID: 7, Role: rbac.RoleUser}, nil))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("普通用户 = %d，期望 403", rr.Code)
	}

	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/settings/test/smtp", "",
		&auth.Principal{UserID: 8, Role: rbac.RoleAdmin, Permissions: []string{rbac.PermUsersRead}}, nil))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("缺 settings:manage = %d，期望 403", rr.Code)
	}
	if prober.calls != 0 {
		t.Fatal("被拒绝的请求不应触发探测")
	}
}

func TestSMTPTestEndpointWithoutProber(t *testing.T) {
	mux := smtpTestMux(t, nil, nil, nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/settings/test/smtp", "", webhookTestPrincipal(), nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("未装配探测接缝 = %d，期望 503", rr.Code)
	}
}

func TestSMTPTestEndpointRateLimit(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })

	prober := &fakeSMTPProber{configured: true}
	mux := smtpTestMux(t, prober, nil, rdb)

	for i := 0; i < smtpTestRateLimit; i++ {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/settings/test/smtp", "", webhookTestPrincipal(), nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("第 %d 次 status = %d，期望 200（body=%s）", i+1, rr.Code, rr.Body.String())
		}
	}
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/settings/test/smtp", "", webhookTestPrincipal(), nil))
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("超限 status = %d，期望 429", rr.Code)
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Fatal("429 必须带 Retry-After")
	}
	if prober.calls != smtpTestRateLimit {
		t.Fatalf("超限后不应再探测，实际调用 %d 次", prober.calls)
	}
}

func TestSMTPTestAuditCarriesStageNotSecrets(t *testing.T) {
	capture := &captureExec{}
	logger := audit.New(capture)
	prober := &fakeSMTPProber{
		configured: true,
		result: notify.SMTPProbeResult{OK: false, Steps: []notify.SMTPProbeStep{{
			Stage: notify.SMTPProbeStageAuth, Status: notify.SMTPProbeStatusFailed,
			Reason: notify.SMTPProbeReasonAuthFailed, SMTPCode: 535,
		}}},
	}
	mux := smtpTestMux(t, prober, logger, nil)

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/settings/test/smtp", "", webhookTestPrincipal(), nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d，期望 200", rr.Code)
	}

	calls := capture.snapshot()
	if len(calls) == 0 {
		t.Fatal("应写入一条审计")
	}
	args := calls[0]
	if len(args) < 10 {
		t.Fatalf("审计参数不足：%+v", args)
	}
	// sanitizeDetailJSON 已把 action 注入 detail，并返回字符串形态。
	text := fmt.Sprint(args[9])
	for _, want := range []string{`"action":"settings.test_smtp"`, `"stage":"auth"`, `"smtp_code":535`} {
		if !strings.Contains(text, want) {
			t.Fatalf("审计 detail 缺少 %s：%s", want, text)
		}
	}
	for _, forbidden := range []string{"password", "bot@example.com", "smtp.qiye"} {
		if strings.Contains(strings.ToLower(text), forbidden) {
			t.Fatalf("审计 detail 泄露 %q：%s", forbidden, text)
		}
	}
}
