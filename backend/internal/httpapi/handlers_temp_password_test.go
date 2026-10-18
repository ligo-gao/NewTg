package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/rbac"
)

// Task 14 评审 Important 5：管理员临时口令的 24h TTL 必须由登录路径消费。
// 标记由 reset-password 写入、登录校验、改密成功后删除（F-07-12）。

func TestUsersTempPasswordLoginTTL(t *testing.T) {
	env := newAuthTestEnv(t, nil, authTestOptions{})
	hash, err := auth.HashPassword("TempPassw0rd!234")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	user := env.repo.add(UserRecord{
		Username:           "tempuser",
		PasswordHash:       hash,
		MustChangePassword: true,
	})
	key := tempPasswordKeyPrefix + strconv.FormatInt(user.ID, 10)

	// 标记缺失（过期/被清理）→ 临时口令立即失效；不能无限期放行。
	rr := env.login("tempuser", "TempPassw0rd!234")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("无 TTL 标记登录 status = %d，期望 401（body=%q）", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "temp_password_expired") {
		t.Fatalf("缺少 temp_password_expired problem：%q", rr.Body.String())
	}

	// 标记有效 → 允许登录，并携带 must_change_password=true 驱动前端强制改密。
	if err := env.rdb.Set(context.Background(), key, "1", tempPasswordTTL).Err(); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	rr = env.login("tempuser", "TempPassw0rd!234")
	if rr.Code != http.StatusOK {
		t.Fatalf("有效 TTL 登录 status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	var resp authSessionResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("login JSON: %v", err)
	}
	if !resp.User.MustChangePassword {
		t.Fatal("登录响应缺少 must_change_password=true")
	}

	// 口令错误时不泄露 TTL 状态（与普通 invalid_credentials 同形）。
	_ = env.rdb.Del(context.Background(), key).Err()
	rr = env.login("tempuser", "definitely-wrong")
	if rr.Code != http.StatusUnauthorized || strings.Contains(rr.Body.String(), "temp_password_expired") {
		t.Fatalf("错误口令响应泄漏 TTL 状态：status=%d body=%q", rr.Code, rr.Body.String())
	}
}

func TestUsersChangePasswordClearsTempMarker(t *testing.T) {
	env := newPasswordTestEnv(t)
	hash, err := auth.HashPassword("TempPassw0rd!234")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	user := env.repo.add(UserRecord{
		Username:           "tempuser",
		PasswordHash:       hash,
		MustChangePassword: true,
	})
	ctx := context.Background()
	key := tempPasswordKeyPrefix + strconv.FormatInt(user.ID, 10)
	if err := env.rdb.Set(ctx, key, "1", tempPasswordTTL).Err(); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	sid, err := env.sessions.Create(ctx, user.ID, auth.SessionOpts{
		UA: "Test/1.0", IP: "127.0.0.1", Role: rbac.RoleUser,
	})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}
	csrf := auth.NewCSRFToken()
	if err := env.sessions.IssueCSRF(ctx, sid, csrf); err != nil {
		t.Fatalf("IssueCSRF: %v", err)
	}

	rr := env.request(http.MethodPost, "/api/password",
		map[string]any{
			"current_password": "TempPassw0rd!234",
			"new_password":     "NewPassw0rd!234",
		},
		func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: sid})
			r.Header.Set(auth.CSRFHeaderName, csrf)
			r.Header.Set("Origin", passwordTestBaseURL)
			r.RemoteAddr = "127.0.0.1:4321"
		})
	if rr.Code != http.StatusNoContent {
		t.Fatalf("改密 status = %d，期望 204（body=%q）", rr.Code, rr.Body.String())
	}
	if env.mr.Exists(key) {
		t.Fatal("用户自选新口令后临时口令标记未删除")
	}
}

// 防回归：临时口令响应必须禁止缓存（只展示一次）。
func TestUsersResetPasswordNoStore(t *testing.T) {
	env := newAdminTestEnv(t)
	user := env.repo.addUser(AdminUserRecord{Username: "alice"})
	rr := env.request(fullAdmin(), http.MethodPost, fmt.Sprintf("/api/users/%d/reset-password", user.ID), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("reset-password status = %d（body=%q）", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q，期望 no-store", got)
	}
}
