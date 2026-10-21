package httpapi

// S4 支付令牌/自动续费端点（P-14 零回显、PUT 开关契约）。
//
// 用 fake 服务验证 HTTP 投影：响应体**绝不**包含 token_cipher（即使是假值），
// 只出现 brand/last4/exp；PUT auto-renew 把 path id 与 body 原样交给服务层。

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/rbac"
	"newtg/gateway/internal/store"
)

type fakePaymentMethodHandlerService struct {
	method  *string
	enabled bool
	deleted string
}

func (f *fakePaymentMethodHandlerService) CreatePaymentMethod(_ context.Context, userID int64,
	provider, token string) (store.PaymentMethod, error) {
	return store.PaymentMethod{
		ID: "22222222-2222-2222-2222-222222222222", UserID: userID,
		Provider: provider, TokenCipher: "TOP_SECRET_CIPHER_" + token,
		Brand: "visa", Last4: "4242", ExpMonth: 12, ExpYear: 2030,
		Status: "active", CreatedAt: time.Now().UTC(),
	}, nil
}

func (f *fakePaymentMethodHandlerService) ListPaymentMethods(context.Context, int64) ([]store.PaymentMethod, error) {
	return []store.PaymentMethod{{
		ID: "22222222-2222-2222-2222-222222222222", UserID: 7,
		Provider: "stripe", TokenCipher: "TOP_SECRET_CIPHER",
		Brand: "visa", Last4: "4242", ExpMonth: 12, ExpYear: 2030,
		Status: "active", CreatedAt: time.Now().UTC(),
	}}, nil
}

func (f *fakePaymentMethodHandlerService) DeletePaymentMethod(_ context.Context, _ int64, id string) ([]int64, error) {
	f.deleted = id
	return []int64{9}, nil
}

func (f *fakePaymentMethodHandlerService) SetSubscriptionAutoRenew(_ context.Context, userID, subscriptionID int64,
	enabled bool, paymentMethodID *string) (store.Subscription, error) {
	f.enabled = enabled
	f.method = paymentMethodID
	return store.Subscription{
		ID: subscriptionID, UserID: userID, AutoRenew: enabled,
		AutoRenewPaymentMethodID: paymentMethodID,
	}, nil
}

func newPaymentMethodTestEnv(t *testing.T) (*subscriptionTestEnv, *fakePaymentMethodHandlerService) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	payments := &fakePaymentMethodHandlerService{}
	handlers, err := NewSubscriptionHandlers(SubscriptionHandlerConfig{
		Repo: newFakeSubscriptionRepo(), DB: &sql.DB{}, Redis: rdb,
		Authz: rbac.NewAuthzMiddleware(), Audit: audit.New(&subAuditExecer{}),
		Logger: testLogger(), PaymentMethods: payments,
	})
	if err != nil {
		t.Fatalf("NewSubscriptionHandlers: %v", err)
	}
	mux := http.NewServeMux()
	handlers.Register(mux)
	return &subscriptionTestEnv{router: mux, rdb: rdb, mr: mr}, payments
}

// TestPaymentMethodResponsesNeverEchoToken 是 Step 4 的零回显验收。
func TestPaymentMethodResponsesNeverEchoToken(t *testing.T) {
	env, _ := newPaymentMethodTestEnv(t)

	list := env.do(t, endUser(7), http.MethodGet, "/api/payment-methods", nil, nil)
	if list.Code != http.StatusOK {
		t.Fatalf("list status = %d, body=%s", list.Code, list.Body.String())
	}
	body := list.Body.String()
	if strings.Contains(body, "TOP_SECRET_CIPHER") || strings.Contains(body, "token_cipher") {
		t.Fatalf("list response echoes the token cipher: %s", body)
	}
	var decoded struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(decoded.Items) != 1 || decoded.Items[0]["brand"] != "visa" ||
		decoded.Items[0]["last4"] != "4242" {
		t.Fatalf("items = %v, want brand/last4 projection", decoded.Items)
	}

	created := env.do(t, endUser(7), http.MethodPost, "/api/payment-methods",
		map[string]any{"provider": "stripe", "token": "pm_test_1"}, nil)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body=%s", created.Code, created.Body.String())
	}
	if strings.Contains(created.Body.String(), "TOP_SECRET_CIPHER") {
		t.Fatalf("create response echoes the token cipher: %s", created.Body.String())
	}
}

// TestPutSubscriptionAutoRenewPassesPreference：path id、enabled 与令牌 id
// 原样交给服务层；响应带 auto_renew 状态。
func TestPutSubscriptionAutoRenewPassesPreference(t *testing.T) {
	env, payments := newPaymentMethodTestEnv(t)
	const methodID = "33333333-3333-3333-3333-333333333333"
	rec := env.do(t, endUser(7), http.MethodPut, "/api/subscriptions/42/auto-renew",
		map[string]any{"enabled": true, "payment_method_id": methodID}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !payments.enabled || payments.method == nil || *payments.method != methodID {
		t.Fatalf("service got enabled=%v method=%v", payments.enabled, payments.method)
	}
	var view struct {
		SubscriptionID int64  `json:"subscription_id"`
		AutoRenew      bool   `json:"auto_renew"`
		MethodID       string `json:"auto_renew_payment_method_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if view.SubscriptionID != 42 || !view.AutoRenew || view.MethodID != methodID {
		t.Fatalf("view = %+v", view)
	}
}

// TestDeletePaymentMethodReturnsNoContent：删除成功 204，且不泄露被关闭的
// 订阅数量/密文（数量属于内部副作用，用户通过订阅列表看到开关变化）。
func TestDeletePaymentMethodReturnsNoContent(t *testing.T) {
	env, payments := newPaymentMethodTestEnv(t)
	const methodID = "44444444-4444-4444-4444-444444444444"
	rec := env.do(t, endUser(7), http.MethodDelete, "/api/payment-methods/"+methodID, nil, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if payments.deleted != methodID {
		t.Fatalf("deleted = %q, want %q", payments.deleted, methodID)
	}
}

// TestAutoRenewEndpointsFailClosedWithoutService：未装配支付服务时 503，
// 不静默保存/假装成功。
func TestAutoRenewEndpointsFailClosedWithoutService(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	handlers, err := NewSubscriptionHandlers(SubscriptionHandlerConfig{
		Repo: newFakeSubscriptionRepo(), DB: &sql.DB{}, Redis: rdb,
		Authz: rbac.NewAuthzMiddleware(), Audit: audit.New(&subAuditExecer{}),
		Logger: testLogger(),
	})
	if err != nil {
		t.Fatalf("NewSubscriptionHandlers: %v", err)
	}
	mux := http.NewServeMux()
	handlers.Register(mux)
	env := &subscriptionTestEnv{router: mux, rdb: rdb, mr: mr}
	rec := env.do(t, endUser(7), http.MethodPut, "/api/subscriptions/42/auto-renew",
		map[string]any{"enabled": true}, nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got == "" {
		t.Fatal("503 must advertise Retry-After")
	}
}
