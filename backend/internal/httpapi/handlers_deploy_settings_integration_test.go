//go:build integration

package httpapi

// 部署设置端点的数据库级验收：真实 PostgreSQL + settings.Service + Sealer。
// 覆盖单测无法证明的部分：密钥以密文落库、GET 零回显、clear 删除行、
// updated_at 乐观锁在 SQL 层生效，以及「通用 settings 写仍受 dangerous 约束」
// ——作用域化端点不是绕过通道。
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_m6d_t2?sslmode=disable' \
//	  go test -tags=integration ./internal/httpapi/ -run TestDeploySettingsIntegration -v

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/configcenter"
	"newtg/gateway/internal/deploy/ionet"
	"newtg/gateway/internal/rbac"
	"newtg/gateway/internal/settings"
)

// deployIntegrationSealer 是可观测的假 Sealer：落库值带 fixed prefix。
type deployIntegrationSealer struct{ prefix string }

func (s deployIntegrationSealer) Seal(plain string) (string, error) { return s.prefix + plain, nil }
func (s deployIntegrationSealer) Open(sealed string) (string, error) {
	if !strings.HasPrefix(sealed, s.prefix) {
		return "", errors.New("invalid sealed value")
	}
	return strings.TrimPrefix(sealed, s.prefix), nil
}

var _ configcenter.SecretSealer = deployIntegrationSealer{}

func openDeployTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("NEWTG_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("NEWTG_TEST_DB_DSN 未设置，跳过集成测试")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		t.Fatalf("ping test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// snapshotDeployKeys 保存并按需清理 deploy section 的两行，测试结束恢复。
// 只触碰 deploy.ionet.*；共享库里的其它键一行不动。
func snapshotDeployKeys(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	for _, key := range []string{deployEnabledKey, deployAPIKeyKey} {
		var (
			value     []byte
			updatedBy sql.NullInt64
			updatedAt time.Time
		)
		err := db.QueryRowContext(ctx,
			`SELECT value, updated_by, updated_at FROM system_settings WHERE "key" = $1`, key).
			Scan(&value, &updatedBy, &updatedAt)
		exists := err == nil
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("snapshot %q: %v", key, err)
		}
		savedValue := append([]byte(nil), value...)
		t.Cleanup(func() {
			if exists {
				_, _ = db.ExecContext(context.Background(), `
					INSERT INTO system_settings ("key", value, updated_by, updated_at)
					VALUES ($1, $2, $3, $4)
					ON CONFLICT ("key") DO UPDATE
					SET value = EXCLUDED.value, updated_by = EXCLUDED.updated_by, updated_at = EXCLUDED.updated_at`,
					key, savedValue, deployNullableInt(updatedBy.Int64), updatedAt)
				return
			}
			_, _ = db.ExecContext(context.Background(),
				`DELETE FROM system_settings WHERE "key" = $1`, key)
		})
		if _, err := db.ExecContext(ctx,
			`DELETE FROM system_settings WHERE "key" = $1`, key); err != nil {
			t.Fatalf("clean %q: %v", key, err)
		}
	}
}

func deployNullableInt(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

type deployIntegrationEnv struct {
	db       *sql.DB
	svc      *settings.Service
	mux      *http.ServeMux
	provider *ionet.FakeProvider
	usedKeys []string
}

func newDeployIntegrationEnv(t *testing.T) *deployIntegrationEnv {
	t.Helper()
	db := openDeployTestDB(t)
	snapshotDeployKeys(t, db)

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })

	sealer := deployIntegrationSealer{prefix: "sealed:"}
	svc, err := settings.NewService(settings.ServiceConfig{DB: db, Sealer: sealer})
	if err != nil {
		t.Fatalf("settings.NewService: %v", err)
	}
	env := &deployIntegrationEnv{db: db, svc: svc, provider: &ionet.FakeProvider{}}
	handlers, err := NewDeploySettingsHandlers(DeploySettingsHandlerConfig{
		Settings: svc,
		DB:       db,
		Sealer:   sealer,
		Provider: func(apiKey string) ionet.DeploymentProvider {
			env.usedKeys = append(env.usedKeys, apiKey)
			return env.provider
		},
		Authz:  rbac.NewAuthzMiddleware(),
		Redis:  rdb,
		Logger: testLogger(),
	})
	if err != nil {
		t.Fatalf("NewDeploySettingsHandlers: %v", err)
	}
	mux := http.NewServeMux()
	handlers.Register(mux)
	env.mux = mux
	return env
}

func (e *deployIntegrationEnv) do(t *testing.T, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, bytes.NewReader([]byte(body)))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	principal := &auth.Principal{UserID: 1, Role: rbac.RoleAdmin, Permissions: []string{rbac.PermSettingsManage}}
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
	rr := httptest.NewRecorder()
	e.mux.ServeHTTP(rr, req)
	return rr
}

func (e *deployIntegrationEnv) storedValue(t *testing.T, key string) (string, bool) {
	t.Helper()
	raw, ok := e.storedRaw(t, key)
	if !ok {
		return "", false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("%q 不是 JSON string: %s", key, raw)
	}
	return value, true
}

func (e *deployIntegrationEnv) storedBool(t *testing.T, key string) (bool, bool) {
	t.Helper()
	raw, ok := e.storedRaw(t, key)
	if !ok {
		return false, false
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("%q 不是 JSON bool: %s", key, raw)
	}
	return value, true
}

func (e *deployIntegrationEnv) storedRaw(t *testing.T, key string) ([]byte, bool) {
	t.Helper()
	var raw []byte
	err := e.db.QueryRow(`SELECT value FROM system_settings WHERE "key" = $1`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false
	}
	if err != nil {
		t.Fatalf("read %q: %v", key, err)
	}
	return raw, true
}

func TestDeploySettingsIntegrationRoundTrip(t *testing.T) {
	env := newDeployIntegrationEnv(t)

	// 初始：未配置、无版本。
	rr := env.do(t, http.MethodGet, "/api/deployments/settings", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("初始 GET = %d（body=%s）", rr.Code, rr.Body.String())
	}
	var initial struct {
		Enabled    bool       `json:"enabled"`
		HasSecret  bool       `json:"has_secret"`
		CanConnect bool       `json:"can_connect"`
		UpdatedAt  *time.Time `json:"updated_at"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &initial); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if initial.Enabled || initial.HasSecret || initial.CanConnect || initial.UpdatedAt != nil {
		t.Fatalf("初始投影 = %+v", initial)
	}

	// 保存开关 + 密钥：响应无密钥明文，库里是密文。
	rr = env.do(t, http.MethodPut, "/api/deployments/settings",
		`{"enabled":true,"api_key":"sk-integration","updated_at":null}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT = %d（body=%s）", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "sk-integration") || strings.Contains(rr.Body.String(), "sealed:") {
		t.Fatalf("PUT 响应泄漏密钥面: %s", rr.Body.String())
	}
	var after struct {
		Enabled    bool       `json:"enabled"`
		HasSecret  bool       `json:"has_secret"`
		CanConnect bool       `json:"can_connect"`
		UpdatedAt  *time.Time `json:"updated_at"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &after); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !after.Enabled || !after.HasSecret || !after.CanConnect || after.UpdatedAt == nil {
		t.Fatalf("保存后投影 = %+v", after)
	}
	stored, ok := env.storedValue(t, deployAPIKeyKey)
	if !ok || stored != "sealed:sk-integration" {
		t.Fatalf("落库值 = %q_exists=%v，期望密文", stored, ok)
	}

	// 留空不更新：api_key="" 时密文保持不变，但 enabled 可以改。
	rr = env.do(t, http.MethodPut, "/api/deployments/settings",
		`{"enabled":false,"api_key":"","updated_at":"`+after.UpdatedAt.UTC().Format(time.RFC3339Nano)+`"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("留空 PUT = %d（body=%s）", rr.Code, rr.Body.String())
	}
	stored, _ = env.storedValue(t, deployAPIKeyKey)
	if stored != "sealed:sk-integration" {
		t.Fatalf("留空覆盖了密钥: %q", stored)
	}

	// 连接测试：使用已存密钥（解密后交给 provider），响应零回显。
	env.provider.Connection = ionet.ConnectionInfo{HardwareCount: 3, TotalAvailable: 6}
	rr = env.do(t, http.MethodPost, "/api/deployments/settings/test-connection", `{}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("test-connection = %d（body=%s）", rr.Code, rr.Body.String())
	}
	if len(env.usedKeys) != 1 || env.usedKeys[0] != "sk-integration" {
		t.Fatalf("provider 使用的密钥 = %v", env.usedKeys)
	}
	if strings.Contains(rr.Body.String(), "sk-integration") {
		t.Fatalf("连接测试响应泄漏密钥: %s", rr.Body.String())
	}
}

func TestDeploySettingsIntegrationOptimisticLockAndClear(t *testing.T) {
	env := newDeployIntegrationEnv(t)
	first := env.do(t, http.MethodPut, "/api/deployments/settings",
		`{"enabled":true,"api_key":"sk-lock"}`)
	if first.Code != http.StatusOK {
		t.Fatalf("PUT = %d（body=%s）", first.Code, first.Body.String())
	}
	var body struct {
		UpdatedAt *time.Time `json:"updated_at"`
	}
	_ = json.Unmarshal(first.Body.Bytes(), &body)

	// 陈旧版本 → 409，且不写入。
	stale := body.UpdatedAt.Add(-time.Second).UTC().Format(time.RFC3339Nano)
	rr := env.do(t, http.MethodPut, "/api/deployments/settings",
		`{"enabled":false,"updated_at":"`+stale+`"}`)
	if rr.Code != http.StatusConflict {
		t.Fatalf("陈旧 updated_at = %d，期望 409（body=%s）", rr.Code, rr.Body.String())
	}
	if value, ok := env.storedBool(t, deployEnabledKey); !ok || !value {
		t.Fatalf("冲突请求不应写入，enabled = %v exists=%v", value, ok)
	}

	// 显式清除：删除行，回到「未配置」。
	current := body.UpdatedAt.UTC().Format(time.RFC3339Nano)
	rr = env.do(t, http.MethodPut, "/api/deployments/settings",
		`{"enabled":true,"clear_api_key":true,"updated_at":"`+current+`"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("clear PUT = %d（body=%s）", rr.Code, rr.Body.String())
	}
	var cleared struct {
		HasSecret bool `json:"has_secret"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &cleared)
	if cleared.HasSecret {
		t.Fatal("清除后 has_secret 应为 false")
	}
	if _, ok := env.storedValue(t, deployAPIKeyKey); ok {
		t.Fatal("clear_api_key 后仍存在密钥行")
	}

	// 未配置且无候选 → 422 deploy_secret_missing（不探测 provider）。
	rr = env.do(t, http.MethodPost, "/api/deployments/settings/test-connection", `{}`)
	if rr.Code != http.StatusUnprocessableEntity || !strings.Contains(rr.Body.String(), "deploy_secret_missing") {
		t.Fatalf("缺密钥 test-connection = %d（body=%s）", rr.Code, rr.Body.String())
	}
}

// 通用 settings 写不得绕过危险确认：直接改 deploy section 的关闭型开关，
// 不带 dangerous_confirmed 必须被拒（作用域化端点是确认后的路径，不是漏洞）。
func TestDeploySettingsIntegrationGenericPutStillRequiresConfirmation(t *testing.T) {
	env := newDeployIntegrationEnv(t)
	ctx := context.Background()

	on, err := env.svc.Put(ctx, settings.UpdateInput{
		Section: "deploy",
		Values:  map[string]json.RawMessage{deployEnabledKey: json.RawMessage(`true`)},
	})
	if err != nil {
		t.Fatalf("开启（安全方向）应允许: %v", err)
	}

	if _, err := env.svc.Put(ctx, settings.UpdateInput{
		Section:         "deploy",
		Values:          map[string]json.RawMessage{deployEnabledKey: json.RawMessage(`false`)},
		ExpectUpdatedAt: on.UpdatedAt,
	}); err == nil {
		t.Fatal("通用 PUT 关闭 deploy.ionet.enabled 未确认时必须被拒")
	} else {
		var verr *settings.ValidationError
		if !errors.As(err, &verr) {
			t.Fatalf("错误类型 = %T，期望 ValidationError: %v", err, err)
		}
	}
	if value, ok := env.storedBool(t, deployEnabledKey); !ok || !value {
		t.Fatalf("被拒写入改变了 enabled: %v exists=%v", value, ok)
	}

	// 密钥替换同样受 any 方向约束。
	if _, err := env.svc.Put(ctx, settings.UpdateInput{
		Section: "deploy",
		Values:  map[string]json.RawMessage{deployAPIKeyKey: json.RawMessage(`"sk-raw"`)},
	}); err == nil {
		t.Fatal("通用 PUT 写入密钥未确认时必须被拒")
	}
}
