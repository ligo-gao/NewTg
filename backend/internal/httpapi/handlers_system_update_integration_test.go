//go:build integration

package httpapi

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/egress"
	"newtg/gateway/internal/rbac"
	"newtg/gateway/internal/settings"
	"newtg/gateway/internal/updatecheck"
)

// 本文件覆盖 G-M6C-20 的真实依赖面：设置键写库校验、SSRF 拒绝、缓存共享、
// MFA 覆盖率聚合与权限边界。
//
// 运行（临时隔离 PG16 + Redis）：
//
//	NEWTG_TEST_DB_DSN='postgres://postgres:postgres@localhost:<port>/newtg?sslmode=disable' \
//	NEWTG_TEST_REDIS_ADDR=localhost:<port> \
//	go test -tags=integration ./internal/httpapi/ -run TestSystemStatusUpdateCheckIntegration -v

func flushUpdateCheckKeys(t *testing.T, rdb *redis.Client) {
	t.Helper()
	for _, key := range []string{
		"newtg:update_check:latest",
		"newtg:update_check:lock",
		"newtg:update_check:forced_at",
	} {
		if err := rdb.Del(context.Background(), key).Err(); err != nil {
			t.Fatalf("flush %s: %v", key, err)
		}
	}
}

func TestSystemStatusUpdateCheckSettingsIntegration(t *testing.T) {
	db, _ := openAuthIntegration(t)
	service, err := settings.NewService(settings.ServiceConfig{DB: db})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	ctx := context.Background()
	cleanup := func() {
		_, _ = db.Exec(`DELETE FROM system_settings WHERE key LIKE 'site.update_check.%' OR key = 'update.channel'`)
	}
	cleanup()
	t.Cleanup(cleanup)

	put := func(section string, values map[string]any, confirmed bool) (settings.UpdateResult, error) {
		raw := make(map[string]json.RawMessage, len(values))
		for key, value := range values {
			encoded, marshalErr := json.Marshal(value)
			if marshalErr != nil {
				t.Fatalf("marshal %s: %v", key, marshalErr)
			}
			raw[key] = encoded
		}
		return service.Put(ctx, settings.UpdateInput{
			Section:            section,
			Values:             raw,
			DangerousConfirmed: confirmed,
		})
	}

	// 默认读：未落库的键回 value=null，默认值来自注册表（关闭/无源）。
	view, err := service.Get(ctx, "site.update_check")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defaults := map[string]any{}
	for _, item := range view.Items {
		defaults[item.Key] = item.Default
	}
	if defaults["site.update_check.enabled"] != false ||
		defaults["site.update_check.source_url"] != "" {
		t.Fatalf("默认值 = %+v，期望关闭且无源", defaults)
	}

	// http 源在未开内网豁免时被拒绝（§3.15 F-08-42 主流程 7）。
	if _, err := put("site.update_check", map[string]any{
		"site.update_check.enabled":    true,
		"site.update_check.source_url": "http://10.0.0.5/latest.json",
	}, false); err == nil {
		t.Fatal("http 源在未开 allow_private_address 时必须被拒绝")
	}

	// 开启功能级豁免（危险项需确认）后同一 http 源可保存。
	first, err := put("site.update_check", map[string]any{
		"site.update_check.enabled":               true,
		"site.update_check.source_url":            "http://10.0.0.5/latest.json",
		"site.update_check.allow_private_address": true,
	}, true)
	if err != nil {
		t.Fatalf("带豁免的 http 源应保存成功: %v", err)
	}
	if len(first.Changed) != 3 {
		t.Fatalf("changed = %v", first.Changed)
	}

	// 区间外 TTL 与非法通道必须被拒绝。
	if _, err := put("site.update_check", map[string]any{
		"site.update_check.interval_seconds": 60,
	}, false); err == nil {
		t.Fatal("interval_seconds=60 应被拒绝（范围 300–86400）")
	}
	if _, err := put("update", map[string]any{"update.channel": "nightly"}, false); err == nil {
		t.Fatal("非法发布通道应被拒绝")
	}
	if _, err := put("update", map[string]any{"update.channel": "beta"}, false); err != nil {
		t.Fatalf("beta 通道应保存成功: %v", err)
	}
}

func TestUpdateCheckSSRFIntegration(t *testing.T) {
	_, rdb := openAuthIntegration(t)
	flushUpdateCheckKeys(t, rdb)
	reader := updateTestConfig{}
	reader.set(t, updatecheck.KeyEnabled, true)
	// 私网字面量地址：默认策略下必须被拒，且不产生任何出站。
	reader.set(t, updatecheck.KeySourceURL, "http://10.99.0.7/updates/latest.json")
	reader.set(t, "sec.ssrf.mode", "basic")
	reader.set(t, "sec.ssrf.domain_mode", "all")

	checker, err := updatecheck.New(updatecheck.Config{
		Reader: reader, Redis: rdb, CurrentVersion: "1.0.0",
	})
	if err != nil {
		t.Fatalf("updatecheck.New: %v", err)
	}
	mux := http.NewServeMux()
	NewUpdateCheckHandlers(UpdateCheckHandlerConfig{Checker: checker}).Register(mux)

	admin := &auth.Principal{UserID: 1, Role: rbac.RoleAdmin}
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodGet, "/api/system/update-check", "", admin, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d（body=%s）", rr.Code, rr.Body.String())
	}
	var body updatecheck.Result
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("JSON 非法: %v", err)
	}
	if body.State != updatecheck.StateError || body.ErrorKind != updatecheck.ErrorSSRFBlocked {
		t.Fatalf("body = %+v，期望 ssrf_blocked", body)
	}
	if strings.Contains(rr.Body.String(), "10.99.0.7") {
		t.Fatal("响应不得回显更新源地址")
	}

	user := &auth.Principal{UserID: 2, Role: rbac.RoleUser}
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodGet, "/api/system/update-check", "", user, nil))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("普通用户 = %d，期望 403", rr.Code)
	}
}

func TestUpdateCheckCacheSharedIntegration(t *testing.T) {
	_, rdb := openAuthIntegration(t)
	flushUpdateCheckKeys(t, rdb)
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"latest":"1.1.0"}`))
	}))
	t.Cleanup(server.Close)

	reader := updateTestConfig{}
	reader.set(t, updatecheck.KeyEnabled, true)
	reader.set(t, updatecheck.KeySourceURL, server.URL)
	reader.set(t, "sec.ssrf.mode", "off")
	checker, err := updatecheck.New(updatecheck.Config{
		Reader: reader, Redis: rdb, CurrentVersion: "1.0.0",
		ClientFactory: func(egress.Policy) *http.Client { return &http.Client{Timeout: 5 * time.Second} },
	})
	if err != nil {
		t.Fatalf("updatecheck.New: %v", err)
	}
	for i := 0; i < 2; i++ {
		result, err := checker.Check(context.Background(), false)
		if err != nil {
			t.Fatalf("Check #%d: %v", i, err)
		}
		if result.State != updatecheck.StateUpdateAvailable {
			t.Fatalf("result = %+v", result)
		}
		if i == 1 && (!result.Cached || result.CacheState != "fresh") {
			t.Fatalf("第二次读取应命中缓存: %+v", result)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("源站被请求 %d 次，期望 1（Redis 缓存共享）", hits.Load())
	}
}

func TestSystemStatusMFAIntegration(t *testing.T) {
	db, rdb := openAuthIntegration(t)
	ctx := context.Background()

	suffix := uniqueAuthSuffix()
	usernames := []string{"mfa_u1_" + suffix, "mfa_u2_" + suffix, "mfa_a1_" + suffix, "mfa_a2_" + suffix}
	cleanup := func() {
		_, _ = db.Exec(`DELETE FROM users WHERE username = ANY($1)`, usernames)
	}
	cleanup()
	t.Cleanup(cleanup)

	rows := []struct {
		username string
		role     string
		status   string
		totp     bool
	}{
		{usernames[0], "user", "enabled", true},
		{usernames[1], "user", "enabled", false},
		{usernames[2], "admin", "enabled", true},
		{usernames[3], "admin", "enabled", false},
	}
	for _, row := range rows {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO users (username, email, password_hash, role, status, totp_enabled)
			VALUES ($1, $2, 'x', $3, $4, $5)`,
			row.username, row.username+"@example.test", row.role, row.status, row.totp); err != nil {
			t.Fatalf("seed user: %v", err)
		}
	}

	handler := NewSystemStatusHandlers(SystemStatusConfig{
		DB: db, Redis: rdb, InstanceID: "gateway-mfa", Role: "gateway", StartedAt: time.Now(),
	})
	mux := http.NewServeMux()
	handler.Register(mux)
	admin := &auth.Principal{UserID: 1, Role: rbac.RoleAdmin}
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodGet, "/api/system/status", "", admin, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d（body=%s）", rr.Code, rr.Body.String())
	}
	var body struct {
		MFA struct {
			Available           bool    `json:"available"`
			TotpEnabledUsers    int64   `json:"totp_enabled_users"`
			TotalUsers          int64   `json:"total_users"`
			MFACoveragePct      float64 `json:"mfa_coverage_pct"`
			TotpEnabledAdmins   int64   `json:"totp_enabled_admins"`
			TotalAdmins         int64   `json:"total_admins"`
			AdminMFACoveragePct float64 `json:"admin_mfa_coverage_pct"`
		} `json:"mfa"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("JSON 非法: %v", err)
	}
	if !body.MFA.Available {
		t.Fatal("有 DB 时 mfa.available 应为 true")
	}
	// 设计口径：全局计数包含管理员（status=enabled 的全部账号）。
	// 集成测试库可能已有其他用例留下的账号，因此以数据库当前计数为真值，
	// 而不是假定只有本用例插入的 4 行。
	var wantTotal, wantTotp, wantAdmins, wantAdminTotp int64
	if err := db.QueryRowContext(ctx, `
		SELECT
			count(*) FILTER (WHERE status = 'enabled'),
			count(*) FILTER (WHERE status = 'enabled' AND totp_enabled),
			count(*) FILTER (WHERE status = 'enabled' AND role = 'admin'),
			count(*) FILTER (WHERE status = 'enabled' AND role = 'admin' AND totp_enabled)
		FROM users`).Scan(&wantTotal, &wantTotp, &wantAdmins, &wantAdminTotp); err != nil {
		t.Fatalf("read expected MFA counts: %v", err)
	}
	wantPct := 0.0
	if wantTotal > 0 {
		wantPct = math.Round(float64(wantTotp)/float64(wantTotal)*1000) / 10
	}
	wantAdminPct := 0.0
	if wantAdmins > 0 {
		wantAdminPct = math.Round(float64(wantAdminTotp)/float64(wantAdmins)*1000) / 10
	}
	if body.MFA.TotalUsers != wantTotal || body.MFA.TotpEnabledUsers != wantTotp || body.MFA.MFACoveragePct != wantPct {
		t.Fatalf("全局口径 = %+v，期望 %d/%d = %.1f", body.MFA, wantTotp, wantTotal, wantPct)
	}
	if body.MFA.TotalAdmins != wantAdmins || body.MFA.TotpEnabledAdmins != wantAdminTotp || body.MFA.AdminMFACoveragePct != wantAdminPct {
		t.Fatalf("管理面口径 = %+v，期望 %d/%d = %.1f", body.MFA, wantAdminTotp, wantAdmins, wantAdminPct)
	}
}
