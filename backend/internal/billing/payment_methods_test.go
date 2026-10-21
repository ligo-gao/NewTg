package billing

// S4 支付令牌服务单测（P-14）：Stripe 回查 + AES-GCM 封装 + 零回显契约。
//
// 服务层断言「落库的是密文、不是明文」与「展示字段来自 Stripe 而非客户端」；
// handler 层的零回显由 handlers_payment_methods_test.go 断言。
// 不依赖 PostgreSQL：fake repo 捕获写入值。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"newtg/gateway/internal/store"
)

type fakePaymentMethodRepo struct {
	created []store.PaymentMethodInput
	rows    []store.PaymentMethod
	deleted []string
	marked  []string
}

func (f *fakePaymentMethodRepo) Create(_ context.Context,
	in store.PaymentMethodInput) (store.PaymentMethod, error) {
	f.created = append(f.created, in)
	return store.PaymentMethod{
		ID: "22222222-2222-2222-2222-222222222222", UserID: in.UserID,
		Provider: in.Provider, TokenCipher: in.TokenCipher, Brand: in.Brand,
		Last4: in.Last4, ExpMonth: in.ExpMonth, ExpYear: in.ExpYear,
		Status: "active", CreatedAt: time.Now().UTC(),
	}, nil
}

func (f *fakePaymentMethodRepo) ListByUser(context.Context, int64) ([]store.PaymentMethod, error) {
	return f.rows, nil
}

func (f *fakePaymentMethodRepo) GetForUser(context.Context, int64, string) (store.PaymentMethod, error) {
	return store.PaymentMethod{}, store.ErrPaymentMethodNotFound
}

func (f *fakePaymentMethodRepo) Delete(_ context.Context, _ int64, id string) ([]int64, error) {
	f.deleted = append(f.deleted, id)
	return []int64{9}, nil
}

func (f *fakePaymentMethodRepo) MarkInvalid(_ context.Context, _ int64, id string) error {
	f.marked = append(f.marked, id)
	return nil
}

type fakeAutoRenewPrefs struct {
	enabled bool
	method  *string
	called  bool
}

func (f *fakeAutoRenewPrefs) SetSubscriptionAutoRenew(_ context.Context, userID, subscriptionID int64,
	enabled bool, method *string) (store.Subscription, error) {
	f.called = true
	f.enabled = enabled
	f.method = method
	return store.Subscription{
		ID: subscriptionID, UserID: userID, AutoRenew: enabled,
		AutoRenewPaymentMethodID: method,
	}, nil
}

func stripePaymentMethodServer(t *testing.T, pmID string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/payment_methods/"+pmID {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer ") {
			t.Errorf("missing bearer auth, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"` + pmID + `","type":"card",
			"card":{"brand":"visa","last4":"4242","exp_month":12,"exp_year":2030}
		}`))
	}))
}

// TestPaymentMethodTokenIsSealedNotStoredPlain 覆盖 Step 4：令牌 AES-GCM 加密
// 落库，展示字段来自 Stripe 回查（客户端无法伪造）。
func TestPaymentMethodTokenIsSealedNotStoredPlain(t *testing.T) {
	const pmID = "pm_test_123"
	stripe := stripePaymentMethodServer(t, pmID)
	defer stripe.Close()

	repo := &fakePaymentMethodRepo{}
	svc, err := NewPaymentMethodService(PaymentMethodServiceConfig{
		Repo: repo, Subs: &fakeAutoRenewPrefs{}, Sealer: testTokenSealer{},
		Config: mapConfig{
			ConfigStripeSecretKey: json.RawMessage(`"sealed:sk_test_123"`),
		},
		StripeBaseURL: stripe.URL,
	})
	if err != nil {
		t.Fatalf("NewPaymentMethodService: %v", err)
	}
	created, err := svc.CreatePaymentMethod(context.Background(), 7, "stripe", pmID)
	if err != nil {
		t.Fatalf("CreatePaymentMethod: %v", err)
	}
	if len(repo.created) != 1 {
		t.Fatalf("repo writes = %d, want 1", len(repo.created))
	}
	stored := repo.created[0]
	if stored.TokenCipher != "sealed:"+pmID {
		t.Fatalf("token cipher = %q, want the sealer output", stored.TokenCipher)
	}
	if stored.Brand != "visa" || stored.Last4 != "4242" ||
		stored.ExpMonth != 12 || stored.ExpYear != 2030 {
		t.Fatalf("metadata = %+v, want values from Stripe lookup", stored)
	}
	if created.TokenCipher == pmID {
		t.Fatal("service returned plaintext token")
	}
}

// TestPaymentMethodRejectsUnknownOrUnsupportedToken：Stripe 不认识/非卡令牌
// 一律 422，不落库；非 stripe 渠道 400。
func TestPaymentMethodRejectsUnknownOrUnsupportedToken(t *testing.T) {
	stripe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer stripe.Close()

	repo := &fakePaymentMethodRepo{}
	svc, err := NewPaymentMethodService(PaymentMethodServiceConfig{
		Repo: repo, Subs: &fakeAutoRenewPrefs{}, Sealer: testTokenSealer{},
		Config: mapConfig{
			ConfigStripeSecretKey: json.RawMessage(`"sealed:sk_test_123"`),
		},
		StripeBaseURL: stripe.URL,
	})
	if err != nil {
		t.Fatalf("NewPaymentMethodService: %v", err)
	}
	if _, err := svc.CreatePaymentMethod(context.Background(), 7, "stripe", "pm_missing_1"); !errors.Is(err, ErrInvalidPaymentToken) {
		t.Fatalf("missing token err = %v, want ErrInvalidPaymentToken", err)
	}
	if _, err := svc.CreatePaymentMethod(context.Background(), 7, "alipay", "pm_x_1"); !errors.Is(err, ErrInvalidProvider) {
		t.Fatalf("provider err = %v, want ErrInvalidProvider", err)
	}
	if _, err := svc.CreatePaymentMethod(context.Background(), 7, "stripe", "../../etc/passwd"); !errors.Is(err, ErrInvalidPaymentToken) {
		t.Fatalf("path traversal err = %v, want ErrInvalidPaymentToken", err)
	}
	if len(repo.created) != 0 {
		t.Fatalf("repo writes = %d, want 0", len(repo.created))
	}
}

// TestSetSubscriptionAutoRenewValidatesID：令牌 id 形态非法时在服务层拒绝，
// 不把任意文本带进 SQL。
func TestSetSubscriptionAutoRenewValidatesID(t *testing.T) {
	prefs := &fakeAutoRenewPrefs{}
	svc, err := NewPaymentMethodService(PaymentMethodServiceConfig{
		Repo: &fakePaymentMethodRepo{}, Subs: prefs, Sealer: testTokenSealer{},
	})
	if err != nil {
		t.Fatalf("NewPaymentMethodService: %v", err)
	}
	bad := "not-a-uuid"
	if _, err := svc.SetSubscriptionAutoRenew(context.Background(), 7, 5, true, &bad); !errors.Is(err, store.ErrAutoRenewPaymentMethodInvalid) {
		t.Fatalf("err = %v, want ErrAutoRenewPaymentMethodInvalid", err)
	}
	if prefs.called {
		t.Fatal("store must not be called with an invalid payment method id")
	}
}
