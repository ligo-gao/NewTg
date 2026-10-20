//go:build integration

package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/notify"
	"newtg/gateway/internal/rbac"
)

// 测试 Webhook 端点的集成验收（D2-14）：真实 Redis 限流、真实发送器
// （loopback 测试服务器）与审计零密钥。运行依赖同其它集成测试。

type webhookStaticConfig map[string]any

func (m webhookStaticConfig) Get(key string) (json.RawMessage, bool) {
	value, ok := m[key]
	if !ok {
		return nil, false
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, false
	}
	return raw, true
}

type webhookTestSealer struct{}

func (webhookTestSealer) Open(sealed string) (string, error) {
	return strings.TrimPrefix(sealed, "sealed:"), nil
}

func (webhookTestSealer) Seal(plain string) (string, error) { return "sealed:" + plain, nil }

func newWebhookEndpointEnv(t *testing.T) (*sql.DB, *redis.Client, *http.ServeMux) {
	t.Helper()
	logsDSN := os.Getenv("NEWTG_TEST_LOGS_DSN")
	redisAddr := os.Getenv("NEWTG_TEST_REDIS_ADDR")
	if logsDSN == "" || redisAddr == "" {
		t.Skip("NEWTG_TEST_LOGS_DSN / NEWTG_TEST_REDIS_ADDR 未设置，跳过集成测试")
	}
	logsDB, err := sql.Open("pgx", logsDSN)
	if err != nil {
		t.Fatalf("open logs db: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := logsDB.PingContext(ctx); err != nil {
		t.Fatalf("ping logs db: %v", err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: redisAddr, MaxRetries: -1})
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Fatalf("ping redis: %v", err)
	}
	t.Cleanup(func() {
		_, _ = logsDB.Exec(`DELETE FROM audit_logs WHERE user_id = $1`, int64(424242))
		_ = rdb.Close()
		_ = logsDB.Close()
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	sender := notify.NewWebhookSender(webhookStaticConfig{
		notify.KeyWebhookEnabled: true,
		// query 里的 token 必须不出现在审计/错误里（只记 host）。
		notify.KeyWebhookURL:    server.URL + "/hook?token=super-secret-value",
		notify.KeyWebhookSecret: "sealed:s3cret",
		notify.KeyWebhookEvents: []string{"balance_low"},
	}, webhookTestSealer{})
	// httptest 只能监听 127.0.0.1；仅测试装配放开守卫。
	sender.AllowPrivateTargets = true
	sender.Now = func() time.Time { return time.Unix(1700000000, 0) }

	handlers, err := NewSettingsHandlers(SettingsHandlerConfig{
		Service: &fakeSettingsService{},
		Authz:   rbac.NewAuthzMiddleware(),
		Audit:   audit.New(logsDB),
		Idem:    NewMemoryIdempotencyStore(),
		Webhook: sender,
		Redis:   rdb,
	})
	if err != nil {
		t.Fatalf("NewSettingsHandlers: %v", err)
	}
	mux := http.NewServeMux()
	handlers.Register(mux)
	return logsDB, rdb, mux
}

func TestIntegrationWebhookTestEndpoint(t *testing.T) {
	logsDB, rdb, mux := newWebhookEndpointEnv(t)
	actor := int64(424242)
	principal := &auth.Principal{UserID: actor, Role: rbac.RoleAdmin, Permissions: []string{rbac.PermSettingsManage}}
	post := func() *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/settings/test/webhook", "", principal, nil))
		return rr
	}
	resetBucket := func() {
		_ = rdb.Del(context.Background(), RateLimitKeyPrefix+webhookTestRateMark+":admin:424242").Err()
	}

	t.Run("ok_and_audit_has_no_secret", func(t *testing.T) {
		resetBucket()
		rr := post()
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d，期望 200（body=%s）", rr.Code, rr.Body.String())
		}
		var body webhookTestResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !body.OK || body.StatusCode != http.StatusOK {
			t.Fatalf("body = %+v", body)
		}

		var raw []byte
		if err := logsDB.QueryRow(`
			SELECT detail FROM audit_logs
			WHERE user_id = $1 AND route = '/api/settings/test/webhook'
			ORDER BY id DESC LIMIT 1`, actor).Scan(&raw); err != nil {
			t.Fatalf("read audit: %v", err)
		}
		var detail map[string]any
		if err := json.Unmarshal(raw, &detail); err != nil {
			t.Fatalf("decode detail: %v", err)
		}
		// detail 只允许 event/status_code/ok/host 四项（writer 会额外注入 action）。
		allowed := map[string]bool{"event": true, "status_code": true, "ok": true, "host": true, "action": true}
		for key := range detail {
			if !allowed[key] {
				t.Fatalf("审计 detail 出现未允许字段 %q: %s", key, string(raw))
			}
		}
		if detail["event"] != webhookTestEvent {
			t.Fatalf("event = %v", detail["event"])
		}
		if detail["host"] != "127.0.0.1" {
			t.Fatalf("host = %v，期望仅主机名", detail["host"])
		}
		for _, leak := range []string{"super-secret-value", "token=", "/hook", "s3cret"} {
			if strings.Contains(string(raw), leak) {
				t.Fatalf("审计泄露 %q: %s", leak, string(raw))
			}
		}
	})

	t.Run("rate_limit_4th_429", func(t *testing.T) {
		resetBucket()
		for i := 0; i < webhookTestRateLimit; i++ {
			if rr := post(); rr.Code != http.StatusOK {
				t.Fatalf("第 %d 次 = %d，期望 200", i+1, rr.Code)
			}
		}
		rr := post()
		if rr.Code != http.StatusTooManyRequests {
			t.Fatalf("第 4 次 = %d，期望 429", rr.Code)
		}
		if rr.Header().Get("Retry-After") == "" {
			t.Fatal("429 缺少 Retry-After")
		}
	})

	t.Run("timeout_is_504", func(t *testing.T) {
		resetBucket()
		block := make(chan struct{})
		slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-block
		}))
		defer func() {
			close(block)
			slow.Close()
		}()
		sender := notify.NewWebhookSender(webhookStaticConfig{
			notify.KeyWebhookEnabled: true,
			notify.KeyWebhookURL:     slow.URL,
			notify.KeyWebhookSecret:  "sealed:s3cret",
			notify.KeyWebhookEvents:  []string{"balance_low"},
		}, webhookTestSealer{})
		sender.AllowPrivateTargets = true
		sender.Timeout = 50 * time.Millisecond
		handlers, err := NewSettingsHandlers(SettingsHandlerConfig{
			Service: &fakeSettingsService{},
			Authz:   rbac.NewAuthzMiddleware(),
			Webhook: sender,
			Redis:   rdb,
		})
		if err != nil {
			t.Fatalf("NewSettingsHandlers: %v", err)
		}
		mux := http.NewServeMux()
		handlers.Register(mux)
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/settings/test/webhook", "", principal, nil))
		if rr.Code != http.StatusGatewayTimeout {
			t.Fatalf("status = %d，期望 504（body=%s）", rr.Code, rr.Body.String())
		}
		if strings.Contains(rr.Body.String(), slow.URL) {
			t.Fatalf("504 泄露目标 URL：%s", rr.Body.String())
		}
	})

	t.Run("unconfigured_is_422", func(t *testing.T) {
		resetBucket()
		sender := notify.NewWebhookSender(webhookStaticConfig{}, webhookTestSealer{})
		handlers, err := NewSettingsHandlers(SettingsHandlerConfig{
			Service: &fakeSettingsService{},
			Authz:   rbac.NewAuthzMiddleware(),
			Webhook: sender,
			Redis:   rdb,
		})
		if err != nil {
			t.Fatalf("NewSettingsHandlers: %v", err)
		}
		mux := http.NewServeMux()
		handlers.Register(mux)
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/settings/test/webhook", "", principal, nil))
		if rr.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d，期望 422", rr.Code)
		}
	})
}
