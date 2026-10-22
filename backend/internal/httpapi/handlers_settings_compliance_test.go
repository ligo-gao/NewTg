package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/rbac"
	"newtg/gateway/internal/store"
)

// fakePaymentComplianceStore 模拟 users.payment_compliance_confirmed_at 的
// 幂等语义：首次写入返回 newly=true，之后返回同一时刻且 newly=false。
type fakePaymentComplianceStore struct {
	mu          sync.Mutex
	confirmedAt time.Time
	statusErr   error
	confirmErr  error
	statusCalls int
	confirmCall int
}

func (f *fakePaymentComplianceStore) Status(_ context.Context, _ int64) (time.Time, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusCalls++
	if f.statusErr != nil {
		return time.Time{}, false, f.statusErr
	}
	if f.confirmedAt.IsZero() {
		return time.Time{}, false, nil
	}
	return f.confirmedAt, true, nil
}

func (f *fakePaymentComplianceStore) Confirm(_ context.Context, _ int64) (time.Time, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.confirmCall++
	if f.confirmErr != nil {
		return time.Time{}, false, f.confirmErr
	}
	if !f.confirmedAt.IsZero() {
		return f.confirmedAt, false, nil
	}
	f.confirmedAt = time.Now().UTC().Truncate(time.Second)
	return f.confirmedAt, true, nil
}

func paymentComplianceTestMux(
	t *testing.T,
	compliance PaymentComplianceStore,
	auditLogger *audit.Logger,
) *http.ServeMux {
	t.Helper()
	handlers, err := NewSettingsHandlers(SettingsHandlerConfig{
		Service:    &fakeSettingsService{},
		Authz:      rbac.NewAuthzMiddleware(),
		Audit:      auditLogger,
		Idem:       NewMemoryIdempotencyStore(),
		Compliance: compliance,
	})
	if err != nil {
		t.Fatalf("NewSettingsHandlers: %v", err)
	}
	mux := http.NewServeMux()
	handlers.Register(mux)
	return mux
}

func settingsManager() *auth.Principal {
	return &auth.Principal{
		UserID: 42, Role: rbac.RoleAdmin,
		Permissions: []string{rbac.PermSettingsManage},
	}
}

func TestPaymentComplianceGetStatus(t *testing.T) {
	repo := &fakePaymentComplianceStore{}
	mux := paymentComplianceTestMux(t, repo, nil)

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodGet, "/api/settings/payment-compliance", "", settingsManager(), nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET = %d，期望 200（body=%s）", rr.Code, rr.Body.String())
	}
	var body paymentComplianceResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Confirmed || body.ConfirmedAt != nil {
		t.Fatalf("首次确认前 = %+v，期望 confirmed=false/confirmed_at=null", body)
	}

	// 首次确认后再读：同一时刻、confirmed=true。
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/settings/payment-compliance",
		`{"confirmation":"我已了解支付合规责任"}`, settingsManager(), nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("POST = %d（body=%s）", rr.Code, rr.Body.String())
	}
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodGet, "/api/settings/payment-compliance", "", settingsManager(), nil))
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Confirmed || body.ConfirmedAt == nil {
		t.Fatalf("确认后 = %+v，期望 confirmed=true 且带时间", body)
	}
}

func TestPaymentComplianceRequiresSettingsManage(t *testing.T) {
	repo := &fakePaymentComplianceStore{}
	mux := paymentComplianceTestMux(t, repo, nil)
	user := &auth.Principal{UserID: 7, Role: rbac.RoleUser, Permissions: []string{"wallet:read"}}

	for _, method := range []string{http.MethodGet, http.MethodPost} {
		rr := httptest.NewRecorder()
		body := ""
		if method == http.MethodPost {
			body = `{"confirmation":"我已了解支付合规责任"}`
		}
		mux.ServeHTTP(rr, settingsRequest(method, "/api/settings/payment-compliance", body, user, nil))
		if rr.Code != http.StatusForbidden {
			t.Fatalf("%s = %d，期望 403（body=%s）", method, rr.Code, rr.Body.String())
		}
	}
	if repo.statusCalls != 0 || repo.confirmCall != 0 {
		t.Fatalf("无权限请求触达仓库：status=%d confirm=%d", repo.statusCalls, repo.confirmCall)
	}
}

func TestPaymentComplianceRejectsInexactText(t *testing.T) {
	exec := &fakeAuditExec{}
	repo := &fakePaymentComplianceStore{}
	mux := paymentComplianceTestMux(t, repo, audit.New(exec))

	for _, body := range []string{
		`{"confirmation":""}`,
		`{"confirmation":"我已了解支付合规责任 "}`, // 尾随空格不通过
		`{"confirmation":"我已了解支付合规责任。"}`, // 多余标点不通过
		`{"confirmation":"I understand payment compliance"}`,
		`{}`,
	} {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, settingsRequest(http.MethodPost,
			"/api/settings/payment-compliance", body, settingsManager(), nil))
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("body=%s status=%d，期望 400（resp=%s）", body, rr.Code, rr.Body.String())
		}
	}
	if repo.confirmCall != 0 {
		t.Fatalf("错误文本触达仓库 %d 次，期望 0", repo.confirmCall)
	}
	if calls := exec.snapshot(); len(calls) != 0 {
		t.Fatalf("错误文本写审计 %d 次，期望 0", len(calls))
	}
}

func TestPaymentComplianceConfirmAuditsWhoAndWhen(t *testing.T) {
	exec := &fakeAuditExec{}
	logger := audit.New(exec)
	logger.UsernameResolver = func(_ context.Context, userID int64) (string, error) {
		if userID != 42 {
			t.Fatalf("resolver userID = %d，期望 42", userID)
		}
		return "alice", nil
	}
	repo := &fakePaymentComplianceStore{}
	mux := paymentComplianceTestMux(t, repo, logger)

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/settings/payment-compliance",
		`{"confirmation":"我已了解支付合规责任"}`, settingsManager(), nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("POST = %d（body=%s）", rr.Code, rr.Body.String())
	}

	calls := exec.snapshot()
	if len(calls) != 1 {
		t.Fatalf("审计写入 %d 次，期望 1", len(calls))
	}
	args := calls[0]
	if args[0] != int64(42) {
		t.Fatalf("audit user_id = %v，期望 42", args[0])
	}
	if args[1] != "alice" {
		t.Fatalf("audit username = %v，期望 alice（who）", args[1])
	}
	if args[2] != "operation" {
		t.Fatalf("audit category = %v，期望 operation", args[2])
	}
	detail, _ := args[9].(string)
	if !strings.Contains(detail, "payment_compliance_confirmed") ||
		!strings.Contains(detail, "confirmed_at") {
		t.Fatalf("audit detail = %q，期望 event + confirmed_at（when）", detail)
	}
	if strings.Contains(detail, `"confirmation"`) {
		t.Fatalf("audit detail 不应记录输入原文: %q", detail)
	}
}

func TestPaymentComplianceConfirmIsIdempotent(t *testing.T) {
	exec := &fakeAuditExec{}
	repo := &fakePaymentComplianceStore{}
	mux := paymentComplianceTestMux(t, repo, audit.New(exec))
	body := `{"confirmation":"我已了解支付合规责任"}`

	var first, second paymentComplianceResponse
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/settings/payment-compliance", body, settingsManager(), nil))
	if err := json.Unmarshal(rr.Body.Bytes(), &first); err != nil {
		t.Fatalf("decode first: %v", err)
	}
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/settings/payment-compliance", body, settingsManager(), nil))
	if err := json.Unmarshal(rr.Body.Bytes(), &second); err != nil {
		t.Fatalf("decode second: %v", err)
	}

	if first.ConfirmedAt == nil || second.ConfirmedAt == nil || !first.ConfirmedAt.Equal(*second.ConfirmedAt) {
		t.Fatalf("两次确认时间不同: %+v vs %+v", first, second)
	}
	if calls := exec.snapshot(); len(calls) != 1 {
		t.Fatalf("重复确认写审计 %d 次，期望仅首次 1 次", len(calls))
	}
}

func TestPaymentComplianceUserMissingReturns404(t *testing.T) {
	repo := &fakePaymentComplianceStore{statusErr: store.ErrPaymentComplianceUserNotFound}
	mux := paymentComplianceTestMux(t, repo, nil)

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodGet, "/api/settings/payment-compliance", "", settingsManager(), nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("GET 缺失用户 = %d，期望 404（body=%s）", rr.Code, rr.Body.String())
	}
}

func TestPaymentComplianceConfirmStoreFailureIs500(t *testing.T) {
	repo := &fakePaymentComplianceStore{confirmErr: errors.New("db down")}
	mux := paymentComplianceTestMux(t, repo, nil)

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/settings/payment-compliance",
		`{"confirmation":"我已了解支付合规责任"}`, settingsManager(), nil))
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("仓库故障 = %d，期望 500（body=%s）", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "db down") {
		t.Fatalf("500 响应泄露内部错误: %s", rr.Body.String())
	}
}
