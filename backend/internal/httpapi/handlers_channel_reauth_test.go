package httpapi

// R-11：渠道目标变更的步进再认证（补 M4/M5 的凭据外带链）。
//
// 纪律与 reveal 用例一致：Redis 用 miniredis、审计用假 Execer、渠道存储用
// 内存 fake；本文件只约束 HTTP 契约与安全不变量。
//
// 受保护的目标变更（security-04 §2.4）：
//   - base_url：决定已存密钥发往哪里；
//   - upstream_account_id：换成另一套上游凭据；
//   - allow_private_network：允许把凭据发往内网（R-01 的 opt-in）。
//
// 门是 auth.ReauthGate.Authorize：5 分钟窗口内可免重复提交因子，窗口外必须
// 现场提供口令（+ 已启用时的 TOTP）。

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/rbac"
	"newtg/gateway/internal/store"
)

// newChannelReauthEnv 在 newChannelTestEnv 之上接入真实 ReauthGate（miniredis）
// 与捕获式审计。opts 中未设置的 Redis/Reauth/Audit 由本函数补齐；调用方显式
// 传入的字段保持不变（例如 ModelLister）。
func newChannelReauthEnv(t *testing.T, opts ChannelHandlerOptions) (*channelTestEnv, *auth.ReauthGate, *revealAuditExec) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })

	source := &revealReauthSource{materials: map[int64]*auth.ReauthMaterial{
		testAdminID: {PasswordHash: revealPasswordHash},
	}}
	gate := auth.NewReauthGate(rdb, source, func(string, string, time.Time) bool { return false })
	auditEx := &revealAuditExec{}
	opts.Redis = rdb
	opts.Reauth = gate
	opts.Audit = audit.New(auditEx)
	return newChannelTestEnv(t, opts), gate, auditEx
}

// openChannelReauthWindow 通过一次真实口令校验打开 5 分钟再认证窗口。
func openChannelReauthWindow(t *testing.T, gate *auth.ReauthGate) {
	t.Helper()
	if err := gate.Authorize(context.Background(), testAdminID, auth.ReauthFactor{Password: revealPassword}); err != nil {
		t.Fatalf("打开再认证窗口失败: %v", err)
	}
}

// channelAuditRows 返回指定动作的审计行（detail 已解码），列序与
// audit.Logger 冻结契约一致（与 revealAuditRows 同口径，但接受独立 exporter）。
func channelAuditRows(t *testing.T, ex *revealAuditExec, action string) []map[string]any {
	t.Helper()
	rows := make([]map[string]any, 0, 4)
	for _, args := range ex.snapshot() {
		detail, _ := args[9].(string)
		var m map[string]any
		if err := json.Unmarshal([]byte(detail), &m); err != nil {
			t.Fatalf("审计 detail 不是合法 JSON: %v (%q)", err, detail)
		}
		if m["action"] != action {
			continue
		}
		m["_result"] = args[8]
		m["_status"] = args[7]
		m["_actor"] = args[0]
		rows = append(rows, m)
	}
	return rows
}

func sensitiveManagePrincipal() *auth.Principal {
	return channelAdminPrincipal(rbac.PermChannelsManage, rbac.PermChannelsManageSensitive)
}

func patchChannelBody(seeded *store.Channel, extra map[string]any) map[string]any {
	body := map[string]any{
		"expected_version": seeded.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
	for k, v := range extra {
		body[k] = v
	}
	return body
}

// 未提交因子：403 reauth_required、零写库、一条 reauth_failed 审计。
func TestChannelPatchBaseURLChangeRequiresStepUpAuth(t *testing.T) {
	env, _, auditEx := newChannelReauthEnv(t, ChannelHandlerOptions{})
	baseURL := "https://api.example.com"
	seeded := seedChannel(t, env, func(c *store.Channel) {
		c.BaseURL = &baseURL
		c.AllowPrivateNetwork = false
	})

	rr := serveWithPrincipal(t, env.mux,
		jsonReq(t, http.MethodPatch, "/api/channels/1",
			patchChannelBody(seeded, map[string]any{"base_url": "https://attacker.example.com"})),
		sensitiveManagePrincipal())

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d，期望 403（body=%s）", rr.Code, rr.Body.String())
	}
	if got := decodeProblemType(t, rr); got != problemTypeReauthRequired {
		t.Fatalf("problem type = %q，期望 %q", got, problemTypeReauthRequired)
	}
	if env.store.updateCalls != 0 {
		t.Fatalf("再认证未过仍写库（updateCalls=%d）", env.store.updateCalls)
	}
	rows := channelAuditRows(t, auditEx, audit.ActionChannelReauthFailed)
	if len(rows) != 1 {
		t.Fatalf("reauth_failed 审计行数 = %d，期望 1（rows=%v）", len(rows), rows)
	}
	if rows[0]["_result"] != "failed" || rows[0]["target"] != "base_url" {
		t.Fatalf("reauth_failed 审计 = %v，期望 failed + target=base_url", rows[0])
	}
	if _, leaked := rows[0]["password"]; leaked {
		t.Fatalf("审计 detail 含 password 字段：%v", rows[0])
	}
	if rows[0]["_actor"] != int64(testAdminID) {
		t.Fatalf("审计 actor = %v，期望 %d", rows[0]["_actor"], testAdminID)
	}
}

// 错误口令：403，且不得写库。
func TestChannelPatchBaseURLChangeRejectsWrongPassword(t *testing.T) {
	env, _, _ := newChannelReauthEnv(t, ChannelHandlerOptions{})
	baseURL := "https://api.example.com"
	seeded := seedChannel(t, env, func(c *store.Channel) { c.BaseURL = &baseURL })

	rr := serveWithPrincipal(t, env.mux,
		jsonReq(t, http.MethodPatch, "/api/channels/1",
			patchChannelBody(seeded, map[string]any{
				"base_url": "https://attacker.example.com",
				"password": "wrong-password",
			})),
		sensitiveManagePrincipal())

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d，期望 403（body=%s）", rr.Code, rr.Body.String())
	}
	if env.store.updateCalls != 0 {
		t.Fatalf("口令错误仍写库（updateCalls=%d）", env.store.updateCalls)
	}
}

// 正确口令：200，地址落库，一条 target_change 成功审计。
func TestChannelPatchBaseURLChangeWithPasswordSucceeds(t *testing.T) {
	env, _, auditEx := newChannelReauthEnv(t, ChannelHandlerOptions{})
	baseURL := "https://api.example.com"
	seeded := seedChannel(t, env, func(c *store.Channel) { c.BaseURL = &baseURL })

	rr := serveWithPrincipal(t, env.mux,
		jsonReq(t, http.MethodPatch, "/api/channels/1",
			patchChannelBody(seeded, map[string]any{
				"base_url": "https://next.example.com",
				"password": revealPassword,
			})),
		sensitiveManagePrincipal())

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d，期望 200（body=%s）", rr.Code, rr.Body.String())
	}
	if got := env.store.items[1].BaseURL; got == nil || *got != "https://next.example.com" {
		t.Fatalf("落库 base_url = %v，期望 https://next.example.com", got)
	}
	rows := channelAuditRows(t, auditEx, audit.ActionChannelTargetChange)
	if len(rows) != 1 || rows[0]["_result"] != "success" || rows[0]["target"] != "base_url" {
		t.Fatalf("target_change 审计 = %v，期望 1 条 success/base_url", rows)
	}
}

// 同一再认证窗口内重复改地址不再要求口令（Authorize 的 5 分钟窗口语义）。
func TestChannelPatchBaseURLChangeReusesReauthWindow(t *testing.T) {
	env, gate, _ := newChannelReauthEnv(t, ChannelHandlerOptions{})
	baseURL := "https://api.example.com"
	seeded := seedChannel(t, env, func(c *store.Channel) { c.BaseURL = &baseURL })
	openChannelReauthWindow(t, gate)

	rr := serveWithPrincipal(t, env.mux,
		jsonReq(t, http.MethodPatch, "/api/channels/1",
			patchChannelBody(seeded, map[string]any{"base_url": "https://next.example.com"})),
		sensitiveManagePrincipal())

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d，期望 200（窗口内免因子；body=%s）", rr.Code, rr.Body.String())
	}
}

// upstream_account_id 变更同样受保护。
func TestChannelPatchUpstreamAccountChangeRequiresStepUpAuth(t *testing.T) {
	env, gate, _ := newChannelReauthEnv(t, ChannelHandlerOptions{})
	baseURL := "https://api.example.com"
	seeded := seedChannel(t, env, func(c *store.Channel) { c.BaseURL = &baseURL })

	rr := serveWithPrincipal(t, env.mux,
		jsonReq(t, http.MethodPatch, "/api/channels/1",
			patchChannelBody(seeded, map[string]any{"upstream_account_id": 42})),
		channelAdminPrincipal(rbac.PermChannelsManage))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d，期望 403（body=%s）", rr.Code, rr.Body.String())
	}
	if env.store.updateCalls != 0 {
		t.Fatalf("再认证未过仍写库（updateCalls=%d）", env.store.updateCalls)
	}

	openChannelReauthWindow(t, gate)
	rr = serveWithPrincipal(t, env.mux,
		jsonReq(t, http.MethodPatch, "/api/channels/1",
			patchChannelBody(seeded, map[string]any{"upstream_account_id": 42})),
		channelAdminPrincipal(rbac.PermChannelsManage))
	if rr.Code != http.StatusOK {
		t.Fatalf("窗口内 status = %d，期望 200（body=%s）", rr.Code, rr.Body.String())
	}
	if got := env.store.items[1].UpstreamAccountID; got == nil || *got != 42 {
		t.Fatalf("落库 upstream_account_id = %v，期望 42", got)
	}
}

// allow_private_network 改值（R-01 opt-in）同样受步进再认证保护。
func TestChannelPatchAllowPrivateNetworkChangeRequiresStepUpAuth(t *testing.T) {
	env, gate, _ := newChannelReauthEnv(t, ChannelHandlerOptions{})
	baseURL := "https://api.example.com"
	seeded := seedChannel(t, env, func(c *store.Channel) {
		c.BaseURL = &baseURL
		c.AllowPrivateNetwork = false
	})

	rr := serveWithPrincipal(t, env.mux,
		jsonReq(t, http.MethodPatch, "/api/channels/1",
			patchChannelBody(seeded, map[string]any{"allow_private_network": true})),
		sensitiveManagePrincipal())
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d，期望 403（body=%s）", rr.Code, rr.Body.String())
	}

	openChannelReauthWindow(t, gate)
	rr = serveWithPrincipal(t, env.mux,
		jsonReq(t, http.MethodPatch, "/api/channels/1",
			patchChannelBody(seeded, map[string]any{"allow_private_network": true})),
		sensitiveManagePrincipal())
	if rr.Code != http.StatusOK {
		t.Fatalf("窗口内 status = %d，期望 200（body=%s）", rr.Code, rr.Body.String())
	}
	if !env.store.items[1].AllowPrivateNetwork {
		t.Fatal("落库 allow_private_network 仍为 false")
	}
}

// 只改非目标字段（备注）不受影响，即使没有任何再认证门。
func TestChannelPatchNonTargetFieldsSkipStepUpAuth(t *testing.T) {
	env := newChannelTestEnv(t, ChannelHandlerOptions{})
	baseURL := "https://api.example.com"
	seeded := seedChannel(t, env, func(c *store.Channel) { c.BaseURL = &baseURL })

	rr := serveWithPrincipal(t, env.mux,
		jsonReq(t, http.MethodPatch, "/api/channels/1",
			patchChannelBody(seeded, map[string]any{"remark": "ops note"})),
		channelAdminPrincipal(rbac.PermChannelsManage))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d，期望 200（body=%s）", rr.Code, rr.Body.String())
	}
}

// 门缺失时 fail-closed：503，绝不退化成「仅权限」。
func TestChannelPatchTargetChangeWithoutReauthGateFailsClosed(t *testing.T) {
	env := newChannelTestEnv(t, ChannelHandlerOptions{})
	baseURL := "https://api.example.com"
	seeded := seedChannel(t, env, func(c *store.Channel) { c.BaseURL = &baseURL })

	rr := serveWithPrincipal(t, env.mux,
		jsonReq(t, http.MethodPatch, "/api/channels/1",
			patchChannelBody(seeded, map[string]any{"base_url": "https://next.example.com"})),
		sensitiveManagePrincipal())
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d，期望 503（body=%s）", rr.Code, rr.Body.String())
	}
	if env.store.updateCalls != 0 {
		t.Fatalf("门缺失仍写库（updateCalls=%d）", env.store.updateCalls)
	}
}

// fetch-models 复用库里密钥：仅当 base_url 一致**且**再认证窗口内。
func TestFetchChannelModelsStoredKeyReuseRequiresReauthWindow(t *testing.T) {
	lister := &fakeChannelModelLister{models: []string{"claude-sonnet-4-5"}}
	env, gate, auditEx := newChannelReauthEnv(t, ChannelHandlerOptions{ModelLister: lister})
	baseURL := "https://api.anthropic.com"
	ch := seedChannel(t, env, func(c *store.Channel) {
		c.BaseURL = &baseURL
		c.Credentials = mustSealCredentials(t, env.sealer, ChannelCredentials{APIKey: "sk-stored"})
	})

	body := map[string]any{"channel_id": ch.ID}

	rr := serveWithPrincipal(t, env.mux,
		jsonReq(t, http.MethodPost, "/api/channels/fetch-models", body),
		sensitiveManagePrincipal())
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("窗口外 status = %d，期望 422（必须显式提供 api_key；body=%s）", rr.Code, rr.Body.String())
	}
	if lister.calls != 0 {
		t.Fatalf("窗口外仍发起探测 %d 次：存量密钥不得复用", lister.calls)
	}

	openChannelReauthWindow(t, gate)
	rr = serveWithPrincipal(t, env.mux,
		jsonReq(t, http.MethodPost, "/api/channels/fetch-models", body),
		sensitiveManagePrincipal())
	if rr.Code != http.StatusOK {
		t.Fatalf("窗口内 status = %d，期望 200（body=%s）", rr.Code, rr.Body.String())
	}
	if lister.got.APIKey != "sk-stored" {
		t.Fatalf("lister api_key = %q，期望复用 sk-stored", lister.got.APIKey)
	}
	rows := channelAuditRows(t, auditEx, audit.ActionChannelCredentialsReuse)
	if len(rows) != 1 || rows[0]["_result"] != "success" || rows[0]["channel_id"] != float64(ch.ID) {
		t.Fatalf("credentials_reuse 审计 = %v，期望 1 条 success + channel_id", rows)
	}
}
