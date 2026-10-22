package httpapi

// W15-C 签到端点的 HTTP 单元验收（不触真 PG）：关闭态零副作用、响应形状、
// 同日重放 409、钱包上界 409、限流先于 Turnstile、Turnstile 三态与月历校验。
//
// 真实事务/并发/回滚在 billing/checkin_test.go 与 handlers_checkin_integration_test.go。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/billing"
)

type fakeCheckinRunner struct {
	enabled  bool
	result   billing.CheckinResult
	err      error
	calendar billing.CheckinCalendar
	calErr   error
	calls    int
	lastUID  int64
	lastMon  string
}

func (f *fakeCheckinRunner) Enabled() bool { return f.enabled }

func (f *fakeCheckinRunner) CheckIn(_ context.Context, userID int64) (billing.CheckinResult, error) {
	f.calls++
	f.lastUID = userID
	return f.result, f.err
}

func (f *fakeCheckinRunner) Calendar(_ context.Context, userID int64, month string) (billing.CheckinCalendar, error) {
	f.calls++
	f.lastUID = userID
	f.lastMon = month
	return f.calendar, f.calErr
}

type checkinHarness struct {
	mux *http.ServeMux
	rdb *redis.Client
	mr  *miniredis.Miniredis
}

// newCheckinHarness 挂载签到端点并注入固定 Principal。userID<=0 时不注入
// 身份，用于验证 401。
func newCheckinHarness(t *testing.T, svc CheckinRunner, gates *turnstileGates, userID int64) *checkinHarness {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })

	h, err := NewCheckinHandlers(CheckinHandlerConfig{
		Service:   svc,
		Redis:     rdb,
		Turnstile: gates,
		Logger:    testLogger(),
	})
	if err != nil {
		t.Fatalf("NewCheckinHandlers: %v", err)
	}
	mux := http.NewServeMux()
	var wrap func(http.Handler) http.Handler
	if userID > 0 {
		wrap = func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ctx := auth.WithPrincipal(r.Context(), &auth.Principal{UserID: userID, Role: "user"})
				next.ServeHTTP(w, r.WithContext(ctx))
			})
		}
	}
	h.RegisterCheckinRoutes(mux, wrap)
	return &checkinHarness{mux: mux, rdb: rdb, mr: mr}
}

func checkinRequest(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	} else {
		reader = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func decodeCheckinProblemType(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	var problem struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &problem); err != nil {
		t.Fatalf("problem JSON: %v (body=%q)", err, rr.Body.String())
	}
	return problem.Type
}

func TestCheckinDisabledReturns403WithZeroSideEffects(t *testing.T) {
	svc := &fakeCheckinRunner{enabled: false}
	h := newCheckinHarness(t, svc, nil, 42)

	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/checkin"},
		{http.MethodGet, "/api/checkin/calendar"},
	} {
		rr := checkinRequest(t, h.mux, tc.method, tc.path, "")
		if rr.Code != http.StatusForbidden {
			t.Fatalf("%s %s = %d, want 403 (body=%q)", tc.method, tc.path, rr.Code, rr.Body.String())
		}
		if got := decodeCheckinProblemType(t, rr); got != problemTypeCheckinDisabled {
			t.Fatalf("%s type=%s, want %s", tc.path, got, problemTypeCheckinDisabled)
		}
	}
	if svc.calls != 0 {
		t.Fatalf("关闭态调用了服务 %d 次，want 0", svc.calls)
	}
	// 零副作用：限流键都没有创建（关闭判定在限流之前）。
	if keys := h.mr.Keys(); len(keys) != 0 {
		t.Fatalf("关闭态产生 Redis 键: %v", keys)
	}
}

func TestCheckinSuccessResponseShape(t *testing.T) {
	svc := &fakeCheckinRunner{
		enabled: true,
		result: billing.CheckinResult{
			Date: "2026-10-22", Awarded: 37, Streak: 3, MonthTotal: 80,
			TotalEarned: 120, BalanceAfter: 1120,
		},
	}
	h := newCheckinHarness(t, svc, nil, 42)
	rr := checkinRequest(t, h.mux, http.MethodPost, "/api/checkin", `{}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (body=%q)", rr.Code, rr.Body.String())
	}
	var body struct {
		Date           string `json:"date"`
		Awarded        int64  `json:"awarded"`
		Streak         int32  `json:"streak"`
		MonthTotal     int64  `json:"month_total"`
		TotalEarned    int64  `json:"total_earned"`
		BalanceAfter   int64  `json:"balance_after"`
		CheckedInToday bool   `json:"checked_in_today"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Date != "2026-10-22" || body.Awarded != 37 || body.Streak != 3 ||
		body.MonthTotal != 80 || body.TotalEarned != 120 || body.BalanceAfter != 1120 ||
		!body.CheckedInToday {
		t.Fatalf("body=%+v", body)
	}
	if svc.lastUID != 42 {
		t.Fatalf("user_id=%d, want 会话 Principal 的 42（接口不接受 user_id 参数）", svc.lastUID)
	}
}

func TestCheckinSameDayReplayReturns409(t *testing.T) {
	svc := &fakeCheckinRunner{
		enabled: true,
		result:  billing.CheckinResult{AlreadyCheckedIn: true, Date: "2026-10-22", Awarded: 5},
	}
	h := newCheckinHarness(t, svc, nil, 42)
	rr := checkinRequest(t, h.mux, http.MethodPost, "/api/checkin", `{}`)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status=%d, want 409 (body=%q)", rr.Code, rr.Body.String())
	}
	if got := decodeCheckinProblemType(t, rr); got != problemTypeCheckinAlreadyDone {
		t.Fatalf("type=%s, want %s", got, problemTypeCheckinAlreadyDone)
	}
}

func TestCheckinWalletLimitReturns409(t *testing.T) {
	svc := &fakeCheckinRunner{enabled: true, err: billing.ErrWalletLimit}
	h := newCheckinHarness(t, svc, nil, 42)
	rr := checkinRequest(t, h.mux, http.MethodPost, "/api/checkin", `{}`)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status=%d, want 409 (body=%q)", rr.Code, rr.Body.String())
	}
	if got := decodeCheckinProblemType(t, rr); got != problemTypeWalletLimitExceeded {
		t.Fatalf("type=%s, want %s", got, problemTypeWalletLimitExceeded)
	}
}

func TestCheckinCalendarValidationAndShape(t *testing.T) {
	svc := &fakeCheckinRunner{
		enabled: true,
		calendar: billing.CheckinCalendar{
			Month: "2026-10", Today: "2026-10-22",
			Days:           []billing.CheckinDay{{Date: "2026-10-21", Awarded: 5}},
			Streak:         2,
			MonthTotal:     9,
			TotalEarned:    99,
			CheckedInToday: true,
		},
	}
	h := newCheckinHarness(t, svc, nil, 42)

	if rr := checkinRequest(t, h.mux, http.MethodGet, "/api/checkin/calendar?month=2026-13", ""); rr.Code != http.StatusBadRequest {
		t.Fatalf("非法月份 = %d, want 400 (body=%q)", rr.Code, rr.Body.String())
	}
	if rr := checkinRequest(t, h.mux, http.MethodGet, "/api/checkin/calendar?month=2026-1", ""); rr.Code != http.StatusBadRequest {
		t.Fatalf("两位以下月份 = %d, want 400", rr.Code)
	}

	rr := checkinRequest(t, h.mux, http.MethodGet, "/api/checkin/calendar?month=2026-10", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (body=%q)", rr.Code, rr.Body.String())
	}
	if svc.lastMon != "2026-10" {
		t.Fatalf("month=%q, want 2026-10", svc.lastMon)
	}
	var body struct {
		Month string `json:"month"`
		Days  []struct {
			Date    string `json:"date"`
			Awarded int64  `json:"awarded"`
		} `json:"days"`
		CheckedInToday bool `json:"checked_in_today"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Month != "2026-10" || len(body.Days) != 1 || body.Days[0].Date != "2026-10-21" || !body.CheckedInToday {
		t.Fatalf("body=%+v", body)
	}

	// 空月返回 []（不是 null），前端才能区分「无记录」与「未知」。
	svc.calendar = billing.CheckinCalendar{Month: "2026-11", Today: "2026-10-22"}
	rr = checkinRequest(t, h.mux, http.MethodGet, "/api/checkin/calendar?month=2026-11", "")
	if !strings.Contains(rr.Body.String(), `"days":[]`) {
		t.Fatalf("空月必须返回空数组: %s", rr.Body.String())
	}
}

func TestCheckinUnauthenticatedReturns401(t *testing.T) {
	svc := &fakeCheckinRunner{enabled: true}
	h := newCheckinHarness(t, svc, nil, 0)
	if rr := checkinRequest(t, h.mux, http.MethodPost, "/api/checkin", `{}`); rr.Code != http.StatusUnauthorized {
		t.Fatalf("POST = %d, want 401", rr.Code)
	}
	if rr := checkinRequest(t, h.mux, http.MethodGet, "/api/checkin/calendar", ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("GET = %d, want 401", rr.Code)
	}
	if svc.calls != 0 {
		t.Fatalf("未认证请求调用了服务 %d 次", svc.calls)
	}
}

// ---------------------------------------------------------------------------
// Turnstile 三态 + 限流顺序
// ---------------------------------------------------------------------------

type turnstileStub struct {
	server *httptest.Server
	calls  atomic.Int64
}

func newTurnstileStub(t *testing.T, success bool) *turnstileStub {
	t.Helper()
	stub := &turnstileStub{}
	stub.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stub.calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"success":%t}`, success)
	}))
	t.Cleanup(stub.server.Close)
	return stub
}

func enabledTurnstileGates(verifyURL string, rdb *redis.Client) *turnstileGates {
	return newTurnstileGates(func() auth.TurnstileSettings {
		return auth.TurnstileSettings{Enabled: true, SiteKey: "site-key", SecretKey: "secret-key"}
	}, rdb, verifyURL, nil, testLogger())
}

func TestCheckinTurnstileMissingTokenRejected(t *testing.T) {
	stub := newTurnstileStub(t, true)
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })

	svc := &fakeCheckinRunner{enabled: true, result: billing.CheckinResult{Date: "2026-10-22"}}
	h := newCheckinHarness(t, svc, enabledTurnstileGates(stub.server.URL, rdb), 42)
	rr := checkinRequest(t, h.mux, http.MethodPost, "/api/checkin", `{}`)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403 (body=%q)", rr.Code, rr.Body.String())
	}
	if stub.calls.Load() != 0 {
		t.Fatalf("缺令牌时 siteverify calls=%d, want 0", stub.calls.Load())
	}
	if svc.calls != 0 {
		t.Fatalf("缺令牌时调用了签到服务 %d 次", svc.calls)
	}
}

func TestCheckinTurnstileFailureRejected(t *testing.T) {
	stub := newTurnstileStub(t, false)
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })

	svc := &fakeCheckinRunner{enabled: true, result: billing.CheckinResult{Date: "2026-10-22"}}
	h := newCheckinHarness(t, svc, enabledTurnstileGates(stub.server.URL, rdb), 42)
	rr := checkinRequest(t, h.mux, http.MethodPost, "/api/checkin",
		`{"turnstile_token":"bad-token"}`)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403 (body=%q)", rr.Code, rr.Body.String())
	}
	if stub.calls.Load() != 1 {
		t.Fatalf("siteverify calls=%d, want 1", stub.calls.Load())
	}
	if svc.calls != 0 {
		t.Fatalf("人机校验失败时调用了签到服务 %d 次", svc.calls)
	}
}

func TestCheckinTurnstileSuccessPasses(t *testing.T) {
	stub := newTurnstileStub(t, true)
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })

	svc := &fakeCheckinRunner{enabled: true, result: billing.CheckinResult{Date: "2026-10-22", Awarded: 5}}
	h := newCheckinHarness(t, svc, enabledTurnstileGates(stub.server.URL, rdb), 42)
	rr := checkinRequest(t, h.mux, http.MethodPost, "/api/checkin",
		`{"turnstile_token":"good-token"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (body=%q)", rr.Code, rr.Body.String())
	}
	if stub.calls.Load() != 1 || svc.calls != 1 {
		t.Fatalf("siteverify=%d service=%d, want 1/1", stub.calls.Load(), svc.calls)
	}
}

func TestCheckinRateLimitRunsBeforeTurnstile(t *testing.T) {
	stub := newTurnstileStub(t, true)
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })

	svc := &fakeCheckinRunner{enabled: true, result: billing.CheckinResult{Date: "2026-10-22"}}
	h := newCheckinHarness(t, svc, enabledTurnstileGates(stub.server.URL, rdb), 42)

	for i := 0; i < checkinRateLimit; i++ {
		body := fmt.Sprintf(`{"turnstile_token":"tok-%d"}`, i)
		if rr := checkinRequest(t, h.mux, http.MethodPost, "/api/checkin", body); rr.Code != http.StatusOK {
			t.Fatalf("第 %d 次 = %d, want 200 (body=%q)", i+1, rr.Code, rr.Body.String())
		}
	}
	if stub.calls.Load() != checkinRateLimit {
		t.Fatalf("siteverify calls=%d, want %d", stub.calls.Load(), checkinRateLimit)
	}
	rr := checkinRequest(t, h.mux, http.MethodPost, "/api/checkin", `{"turnstile_token":"tok-over"}`)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("超限 = %d, want 429 (body=%q)", rr.Code, rr.Body.String())
	}
	if stub.calls.Load() != checkinRateLimit {
		t.Fatalf("被限流后 siteverify calls=%d, want %d（限流必须先于 Turnstile）",
			stub.calls.Load(), checkinRateLimit)
	}
	if svc.calls != checkinRateLimit {
		t.Fatalf("service calls=%d, want %d", svc.calls, checkinRateLimit)
	}
}

func TestCheckinRejectsOversizedTurnstileToken(t *testing.T) {
	svc := &fakeCheckinRunner{enabled: true, result: billing.CheckinResult{Date: "2026-10-22"}}
	h := newCheckinHarness(t, svc, nil, 42)
	body := `{"turnstile_token":"` + strings.Repeat("x", checkinMaxTurnstileToken+1) + `"}`
	rr := checkinRequest(t, h.mux, http.MethodPost, "/api/checkin", body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("超长令牌 = %d, want 400 (body=%q)", rr.Code, rr.Body.String())
	}
	if svc.calls != 0 {
		t.Fatalf("超长令牌不应进入签到服务（calls=%d）", svc.calls)
	}
}

func TestCheckinDeletedUserMapsTo401(t *testing.T) {
	svc := &fakeCheckinRunner{enabled: true, err: billing.ErrUserNotFound}
	h := newCheckinHarness(t, svc, nil, 42)
	rr := checkinRequest(t, h.mux, http.MethodPost, "/api/checkin", `{}`)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("已删除用户 = %d, want 401 (body=%q)", rr.Code, rr.Body.String())
	}
}
