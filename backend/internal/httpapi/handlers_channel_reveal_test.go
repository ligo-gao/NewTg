package httpapi

// W11：渠道凭据受控揭示（POST /api/channels/{id}/credentials/reveal）的单元
// 验收。设计权威：security-03 §2.10、modules-06 F-06-03。
//
// 纪律（R-P10）：Redis 用 miniredis、审计用假 Execer、渠道存储用内存 fake；
// 本文件只约束 HTTP 契约与安全不变量，真实 PG/日志库由 integration 用例覆盖。

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/store"
)

const (
	revealPassword    = "RevealCorrect9!"
	revealTOTPAdminID = int64(3)
	revealTOTPSecret  = "JBSWY3DPEHPK3PXP"
	revealTOTPCode    = "123456"
)

var revealPasswordHash = func() string {
	h, err := auth.HashPassword(revealPassword)
	if err != nil {
		panic(err)
	}
	return h
}()

// revealAuditExec 捕获审计 INSERT 的实参（列序与 audit.Logger 冻结契约一致）。
type revealAuditExec struct {
	mu    sync.Mutex
	calls [][]any
}

func (e *revealAuditExec) ExecContext(_ context.Context, _ string, args ...any) (sql.Result, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, append([]any(nil), args...))
	return revealAuditResult(1), nil
}

func (e *revealAuditExec) snapshot() [][]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([][]any, len(e.calls))
	for i, call := range e.calls {
		out[i] = append([]any(nil), call...)
	}
	return out
}

type revealAuditResult int64

func (r revealAuditResult) LastInsertId() (int64, error) { return 0, nil }
func (r revealAuditResult) RowsAffected() (int64, error) { return int64(r), nil }

// revealReauthSource 是 ReauthUserSource 的内存实现。
type revealReauthSource struct {
	mu        sync.Mutex
	materials map[int64]*auth.ReauthMaterial
}

func (s *revealReauthSource) ReauthMaterial(_ context.Context, userID int64) (*auth.ReauthMaterial, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.materials[userID], nil
}

type revealTestEnv struct {
	*channelTestEnv
	mr      *miniredis.Miniredis
	rdb     *redis.Client
	auditEx *revealAuditExec
	gate    *auth.ReauthGate
}

func newRevealTestEnv(t *testing.T) *revealTestEnv {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })

	source := &revealReauthSource{materials: map[int64]*auth.ReauthMaterial{
		testAdminID: {
			PasswordHash: revealPasswordHash,
			TOTPEnabled:  false,
		},
		revealTOTPAdminID: {
			PasswordHash: revealPasswordHash,
			TOTPEnabled:  true,
			TOTPSecret:   revealTOTPSecret,
		},
	}}
	// 校验器是注入接缝：只接受固定码，生产装配用 auth.VerifyTOTP。
	gate := auth.NewReauthGate(rdb, source, func(secret, code string, _ time.Time) bool {
		return secret == revealTOTPSecret && code == revealTOTPCode
	})
	auditEx := &revealAuditExec{}
	env := newChannelTestEnv(t, ChannelHandlerOptions{
		Idempotency: NewMemoryIdempotencyStore(),
		Redis:       rdb,
		Reauth:      gate,
		Audit:       audit.New(auditEx),
	})
	return &revealTestEnv{channelTestEnv: env, mr: mr, rdb: rdb, auditEx: auditEx, gate: gate}
}

func (e *revealTestEnv) reveal(t *testing.T, rawID string, body any, p *auth.Principal) *httptest.ResponseRecorder {
	t.Helper()
	req := jsonReq(t, http.MethodPost,
		"/api/channels/"+rawID+"/credentials/reveal", body)
	return serveWithPrincipal(t, e.mux, req, p)
}

// revealAuditRows 返回指定动作的审计行（detail 已解码）。
func (e *revealTestEnv) revealAuditRows(t *testing.T, action string) []map[string]any {
	t.Helper()
	rows := make([]map[string]any, 0, 4)
	for _, args := range e.auditEx.snapshot() {
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
		m["_category"] = args[2]
		m["_username"] = args[1]
		m["_actor"] = args[0]
		rows = append(rows, m)
	}
	return rows
}

// assertNoPlaintextAnywhere 断言审计实参与响应体都不含凭据明文。
func assertNoPlaintextAnywhere(t *testing.T, secret string, rr *httptest.ResponseRecorder, calls [][]any) {
	t.Helper()
	if rr != nil && strings.Contains(rr.Body.String(), secret) {
		t.Fatalf("响应体包含凭据明文：%q", rr.Body.String())
	}
	for _, call := range calls {
		for _, arg := range call {
			if s, ok := arg.(string); ok && strings.Contains(s, secret) {
				t.Fatalf("审计实参包含凭据明文：%q", s)
			}
		}
	}
}

func seedRevealChannel(t *testing.T, env *revealTestEnv) *store.Channel {
	t.Helper()
	return seedChannel(t, env.channelTestEnv, func(ch *store.Channel) {
		ch.Credentials = mustSealCredentials(t, env.sealer, ChannelCredentials{APIKey: "sk-reveal-secret-1234"})
		ch.Type = string(contract.ChannelTypeOpenAI)
	})
}

func TestChannelRevealRequiresManageSensitive(t *testing.T) {
	env := newRevealTestEnv(t)
	seedRevealChannel(t, env)

	rr := env.reveal(t, "1", map[string]any{
		"reason": "rotate check", "password": revealPassword,
	}, channelAdminPrincipal("channels:read", "channels:manage"))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("无 manage_sensitive 状态码 = %d，期望 403（body=%s）", rr.Code, rr.Body.String())
	}
	assertNoPlaintextAnywhere(t, "sk-reveal-secret-1234", rr, env.auditEx.snapshot())
}

func TestChannelRevealRequiresStepUp(t *testing.T) {
	env := newRevealTestEnv(t)
	seedRevealChannel(t, env)

	cases := []struct {
		name   string
		body   map[string]any
		status int
	}{
		{
			name:   "missing_both_factors",
			body:   map[string]any{"reason": "check"},
			status: http.StatusUnprocessableEntity,
		},
		{
			name:   "both_factors",
			body:   map[string]any{"reason": "check", "password": revealPassword, "totp_code": revealTOTPCode},
			status: http.StatusUnprocessableEntity,
		},
		{
			name:   "wrong_password",
			body:   map[string]any{"reason": "check", "password": "WrongPassword1!"},
			status: http.StatusForbidden,
		},
		{
			name:   "blank_reason",
			body:   map[string]any{"reason": "   ", "password": revealPassword},
			status: http.StatusUnprocessableEntity,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := env.reveal(t, "1", tc.body, channelAdminPrincipal("*"))
			if rr.Code != tc.status {
				t.Fatalf("状态码 = %d，期望 %d（body=%s）", rr.Code, tc.status, rr.Body.String())
			}
			assertNoPlaintextAnywhere(t, "sk-reveal-secret-1234", rr, env.auditEx.snapshot())
			if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
				t.Fatalf("Content-Type = %q，期望 problem+json", ct)
			}
		})
	}

	// 失败尝试必须落审计且不含明文；成功响应绝不能出现。
	rows := env.revealAuditRows(t, audit.ActionChannelCredentialsReveal)
	failed := 0
	for _, row := range rows {
		if row["_result"] == "failed" {
			failed++
		}
		if _, leaked := row["api_key"]; leaked {
			t.Fatalf("揭审计 detail 含 api_key 字段：%v", row)
		}
	}
	if failed < 4 {
		t.Fatalf("失败审计行数 = %d，期望 >= 4", failed)
	}
}

func TestChannelRevealPasswordSuccess(t *testing.T) {
	env := newRevealTestEnv(t)
	seedRevealChannel(t, env)

	rr := env.reveal(t, "1", map[string]any{
		"reason": "migration verification", "password": revealPassword,
	}, channelAdminPrincipal("*"))
	if rr.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（body=%s）", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q，期望 no-store", got)
	}
	if got := rr.Header().Get("Pragma"); got != "no-cache" {
		t.Fatalf("Pragma = %q，期望 no-cache", got)
	}
	if got := rr.Header().Get("ETag"); got != "" {
		t.Fatalf("揭示响应不得带 ETag，实际 %q", got)
	}
	// 设计：响应体就是凭据对象本身（与 credentials_masked 同一形状）。
	var body ChannelCredentials
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if body.APIKey != "sk-reveal-secret-1234" {
		t.Fatalf("响应 = %+v，期望完整 api_key", body)
	}

	// 揭示是只读端点：只允许 Get，绝不写业务库。
	if env.store.updateCalls != 0 || env.store.createCalls != 0 || env.store.deleteCalls != 0 {
		t.Fatalf("揭示端点写库了：create=%d update=%d delete=%d",
			env.store.createCalls, env.store.updateCalls, env.store.deleteCalls)
	}
	if env.store.getCalls == 0 {
		t.Fatal("揭示端点未读取渠道")
	}

	// 步进认证只授权本次请求：不得写再认证窗口。
	if env.mr.Exists("reauth:1") {
		t.Fatal("揭示成功后不得写 reauth 窗口（只授权本次请求）")
	}
	// 没有窗口可复用：紧接着的空因子请求仍是 422。
	again := env.reveal(t, "1", map[string]any{"reason": "again"}, channelAdminPrincipal("*"))
	if again.Code != http.StatusUnprocessableEntity {
		t.Fatalf("无因子第二次状态码 = %d，期望 422（无免认证窗口）", again.Code)
	}

	// 成功审计：category=operation、result=success、只含字段名与 reason。
	rows := env.revealAuditRows(t, audit.ActionChannelCredentialsReveal)
	found := false
	for _, row := range rows {
		if row["_result"] != "success" {
			continue
		}
		found = true
		if row["_category"] != "operation" {
			t.Fatalf("审计 category = %v，期望 operation", row["_category"])
		}
		if row["channel_id"] != float64(1) || row["reason"] != "migration verification" {
			t.Fatalf("审计 detail = %v，期望 channel_id/reason", row)
		}
		fields, _ := row["revealed_fields"].([]any)
		if len(fields) != 1 || fields[0] != "api_key" {
			t.Fatalf("revealed_fields = %v，期望 [api_key]", row["revealed_fields"])
		}
		assertNoPlaintextAnywhere(t, "sk-reveal-secret-1234", nil, env.auditEx.snapshot())
	}
	if !found {
		t.Fatalf("缺少成功审计行：%v", rows)
	}
}

func TestChannelRevealTOTPFactorMustMatchState(t *testing.T) {
	env := newRevealTestEnv(t)
	seedRevealChannel(t, env)
	p := &auth.Principal{UserID: revealTOTPAdminID, Role: "admin", Permissions: []string{"*"}}

	// 已启用 TOTP 的账号给密码 → 422 字段级错误（不是 403，也不是解密）。
	rr := env.reveal(t, "1", map[string]any{"reason": "check", "password": revealPassword}, p)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("MFA 账号给密码状态码 = %d，期望 422（body=%s）", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"totp_code"`) {
		t.Fatalf("422 错误未定位到 totp_code：%s", rr.Body.String())
	}
	assertNoPlaintextAnywhere(t, "sk-reveal-secret-1234", rr, env.auditEx.snapshot())

	// 错误 TOTP → 403（因子失败）。
	rr = env.reveal(t, "1", map[string]any{"reason": "check", "totp_code": "000000"}, p)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("错误 TOTP 状态码 = %d，期望 403（body=%s）", rr.Code, rr.Body.String())
	}

	// 正确 TOTP → 200。
	rr = env.reveal(t, "1", map[string]any{"reason": "check", "totp_code": revealTOTPCode}, p)
	if rr.Code != http.StatusOK {
		t.Fatalf("正确 TOTP 状态码 = %d，期望 200（body=%s）", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "sk-reveal-secret-1234") {
		t.Fatalf("成功响应缺少凭据：%s", rr.Body.String())
	}
}

func TestChannelRevealRateLimitedPerAdmin(t *testing.T) {
	env := newRevealTestEnv(t)
	seedRevealChannel(t, env)
	p := channelAdminPrincipal("*")

	for i := 0; i < ChannelRevealRateLimit; i++ {
		rr := env.reveal(t, "1", map[string]any{"reason": "check", "password": revealPassword}, p)
		if rr.Code != http.StatusOK {
			t.Fatalf("第 %d 次状态码 = %d，期望 200（body=%s）", i+1, rr.Code, rr.Body.String())
		}
	}
	rr := env.reveal(t, "1", map[string]any{"reason": "check", "password": revealPassword}, p)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("第 N+1 次状态码 = %d，期望 429（body=%s）", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Fatal("429 必须带 Retry-After")
	}
	rows := env.revealAuditRows(t, audit.ActionChannelCredentialsReveal)
	limited := false
	for _, row := range rows {
		if row["failure"] == "rate_limited" {
			limited = true
		}
	}
	if !limited {
		t.Fatalf("缺少 rate_limited 审计行：%v", rows)
	}
}

func TestChannelRevealNotFoundAndInvalidID(t *testing.T) {
	env := newRevealTestEnv(t)
	seedRevealChannel(t, env)
	p := channelAdminPrincipal("*")

	for _, id := range []string{"9999", "abc", "0", "-1"} {
		rr := env.reveal(t, id, map[string]any{"reason": "check", "password": revealPassword}, p)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("id=%q 状态码 = %d，期望 404（body=%s）", id, rr.Code, rr.Body.String())
		}
		assertNoPlaintextAnywhere(t, "sk-reveal-secret-1234", rr, env.auditEx.snapshot())
	}
}

// TestChannelWriteAuditCoversCRUD 覆盖 O-1 缺口：create/update/delete 都要落
// 审计，且 detail 只含 id/字段名/布尔，绝不含凭据值。
func TestChannelWriteAuditCoversCRUD(t *testing.T) {
	env := newRevealTestEnv(t)
	p := channelAdminPrincipal("*")

	createBody := map[string]any{
		"name":             "audit-channel",
		"type":             "openai",
		"credentials":      map[string]any{"api_key": "sk-created-secret-9999"},
		"supported_models": []string{"gpt-4o"},
	}
	rr := serveWithPrincipal(t, env.mux,
		jsonReq(t, http.MethodPost, "/api/channels", createBody), p)
	if rr.Code != http.StatusCreated {
		t.Fatalf("创建状态码 = %d，期望 201（body=%s）", rr.Code, rr.Body.String())
	}
	var created ChannelResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("创建响应不是合法 JSON: %v", err)
	}

	patchBody := map[string]any{
		"expected_version": created.Version,
		"remark":           "audited",
	}
	rr = serveWithPrincipal(t, env.mux,
		jsonReq(t, http.MethodPatch, "/api/channels/"+strconv.FormatInt(created.ID, 10), patchBody), p)
	if rr.Code != http.StatusOK {
		t.Fatalf("更新状态码 = %d，期望 200（body=%s）", rr.Code, rr.Body.String())
	}

	rr = serveWithPrincipal(t, env.mux,
		httptest.NewRequest(http.MethodDelete, "/api/channels/"+strconv.FormatInt(created.ID, 10), nil), p)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("删除状态码 = %d，期望 204（body=%s）", rr.Code, rr.Body.String())
	}

	// 三个动作各一条成功审计。
	for _, action := range []string{
		audit.ActionChannelCreate, audit.ActionChannelUpdate, audit.ActionChannelDelete,
	} {
		rows := env.revealAuditRows(t, action)
		if len(rows) != 1 {
			t.Fatalf("%s 审计行数 = %d，期望 1（rows=%v）", action, len(rows), rows)
		}
		if rows[0]["_result"] != "success" || rows[0]["channel_id"] != float64(created.ID) {
			t.Fatalf("%s 审计 = %v，期望 success + channel_id", action, rows[0])
		}
		if rows[0]["_actor"] != int64(testAdminID) {
			t.Fatalf("%s 审计 actor = %v，期望真实管理员 %d", action, rows[0]["_actor"], testAdminID)
		}
		if _, leaked := rows[0]["credentials"]; leaked {
			t.Fatalf("%s 审计 detail 含 credentials 字段：%v", action, rows[0])
		}
	}
	updateRows := env.revealAuditRows(t, audit.ActionChannelUpdate)
	fields, _ := updateRows[0]["fields"].([]any)
	if len(fields) != 1 || fields[0] != "remark" {
		t.Fatalf("update fields = %v，期望 [remark]", updateRows[0]["fields"])
	}
	if updateRows[0]["credentials_written"] != false {
		t.Fatalf("update credentials_written = %v，期望 false", updateRows[0]["credentials_written"])
	}
	// 全量审计实参都不得含明文。
	assertNoPlaintextAnywhere(t, "sk-created-secret-9999", nil, env.auditEx.snapshot())
}
