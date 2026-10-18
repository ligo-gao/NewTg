package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

// execCall 捕获一次 INSERT 的实参，便于逐列断言。
// 实参顺序是 audit.Logger 与测试之间的冻结契约（见 audit.go 的 INSERT）。
type execCall struct {
	query string
	args  []any
}

type fakeExec struct {
	calls []execCall
	err   error
}

func (f *fakeExec) ExecContext(_ context.Context, query string, args ...any) (sql.Result, error) {
	f.calls = append(f.calls, execCall{query: query, args: args})
	if f.err != nil {
		return nil, f.err
	}
	return fakeResult(1), nil
}

type fakeResult int64

func (r fakeResult) LastInsertId() (int64, error) { return 0, nil }
func (r fakeResult) RowsAffected() (int64, error) { return int64(r), nil }

func decodeArg(t *testing.T, arg any) map[string]any {
	t.Helper()
	switch v := arg.(type) {
	case nil:
		return nil
	case string:
		var m map[string]any
		if err := json.Unmarshal([]byte(v), &m); err != nil {
			t.Fatalf("审计 jsonb 参数不是合法 JSON: %v (%q)", err, v)
		}
		return m
	case []byte:
		var m map[string]any
		if err := json.Unmarshal(v, &m); err != nil {
			t.Fatalf("审计 jsonb 参数不是合法 JSON: %v (%q)", err, v)
		}
		return m
	default:
		t.Fatalf("审计 jsonb 参数类型 = %T，期望 string/[]byte/nil", arg)
		return nil
	}
}

// TestAuditLogWritesBeforeAfterAndRedactsSecrets 覆盖 brief (h)：
// before/after diff 落列正确，detail 中的口令/密钥材料被脱敏，审计行绝不带明文。
func TestAuditLogWritesBeforeAfterAndRedactsSecrets(t *testing.T) {
	exec := &fakeExec{}
	l := New(exec)

	detail := json.RawMessage(`{"token_id":7,"password":"hunter2","key":"nkt-verysecret","nested":{"api_key":"sk-live-123","ok":"keep"}}`)
	before := json.RawMessage(`{"status":"enabled","name":"prod"}`)
	after := json.RawMessage(`{"status":"disabled","name":"prod"}`)

	err := l.LogEvent(context.Background(), Event{
		Action:   ActionTokenDisable,
		Actor:    42,
		Username: "alice",
		Detail:   detail,
		Before:   before,
		After:    after,
	})
	if err != nil {
		t.Fatalf("LogEvent: %v", err)
	}
	if len(exec.calls) != 1 {
		t.Fatalf("INSERT 次数 = %d，期望 1", len(exec.calls))
	}
	call := exec.calls[0]
	if !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(call.query)), "INSERT INTO AUDIT_LOGS") {
		t.Fatalf("审计必须 append-only INSERT，实际 SQL = %q", call.query)
	}
	if len(call.args) != 12 {
		t.Fatalf("实参数 = %d，期望 12", len(call.args))
	}

	// 列顺序：user_id, username, category, ip, user_agent, method, route, status_code, result, detail, before, after
	if got := call.args[0]; got != int64(42) {
		t.Errorf("user_id = %v，期望 42", got)
	}
	if got := call.args[1]; got != "alice" {
		t.Errorf("username = %v，期望 alice", got)
	}
	if got := call.args[2]; got != "operation" {
		t.Errorf("category = %v，期望 operation（token.* 属操作审计）", got)
	}
	if got := call.args[8]; got != "success" {
		t.Errorf("result = %v，期望 success", got)
	}

	gotDetail := decodeArg(t, call.args[9])
	if gotDetail == nil {
		t.Fatal("detail 不得为 NULL")
	}
	rawDetail, _ := json.Marshal(gotDetail)
	if strings.Contains(string(rawDetail), "hunter2") || strings.Contains(string(rawDetail), "nkt-verysecret") || strings.Contains(string(rawDetail), "sk-live-123") {
		t.Fatalf("detail 泄露了密钥/口令材料: %s", rawDetail)
	}
	if gotDetail["password"] != redacted || gotDetail["key"] != redacted {
		t.Errorf("敏感字段未替换为 %q: %#v", redacted, gotDetail)
	}
	nested, ok := gotDetail["nested"].(map[string]any)
	if !ok || nested["api_key"] != redacted {
		t.Errorf("嵌套敏感字段未脱敏: %#v", gotDetail["nested"])
	}
	if nested["ok"] != "keep" {
		t.Errorf("非敏感字段被误删: %#v", nested)
	}

	gotBefore := decodeArg(t, call.args[10])
	gotAfter := decodeArg(t, call.args[11])
	if gotBefore["status"] != "enabled" || gotAfter["status"] != "disabled" {
		t.Errorf("before/after diff 错误：before=%#v after=%#v", gotBefore, gotAfter)
	}
}

// TestAuditRejectsUnknownAction 冻结动作枚举：未登记的动作名不得落库。
func TestAuditRejectsUnknownAction(t *testing.T) {
	exec := &fakeExec{}
	l := New(exec)
	err := l.LogEvent(context.Background(), Event{Action: "token.hack", Actor: 1})
	if !errors.Is(err, ErrUnknownAction) {
		t.Fatalf("未知动作 err = %v，期望 ErrUnknownAction", err)
	}
	if len(exec.calls) != 0 {
		t.Fatal("未知动作不得写审计行")
	}
}

// TestAuditActionCategories 逐类断言冻结动作到 category 的映射。
func TestAuditActionCategories(t *testing.T) {
	cases := map[string]string{
		ActionAuthLogin:                    "login",
		ActionAuthLogout:                   "login",
		ActionAuthRegister:                 "login",
		ActionAuthPasswordChange:           "security",
		ActionAuthPasswordReset:            "security",
		ActionAuth2FAEnable:                "security",
		ActionAuth2FADisable:               "security",
		ActionAuth2FABackupCodesRegenerate: "security",
		ActionTokenCreate:                  "operation",
		ActionTokenUpdate:                  "operation",
		ActionTokenDelete:                  "operation",
		ActionTokenResetKey:                "operation",
		ActionTokenEnable:                  "operation",
		ActionTokenDisable:                 "operation",
		ActionTokenBatchEnable:             "operation",
		ActionTokenBatchDisable:            "operation",
		ActionTokenBatchDelete:             "operation",
		ActionUserCreate:                   "operation",
		ActionUserQuotaAdjust:              "operation",
		ActionSystemInitialized:            "operation",
		// RBAC 拒绝并入既有 security 类（security-09 §2.7 设计 B），不新增类别。
		ActionAuthzDenied: "security",
	}
	for action, want := range cases {
		got, ok := Category(action)
		if !ok {
			t.Fatalf("动作 %q 未登记", action)
		}
		if got != want {
			t.Errorf("动作 %q category = %q，期望 %q", action, got, want)
		}
	}
}

// TestAuditRejectsInvalidJSON：非法 JSON 是编程错误，必须拒绝而不是写入半截数据。
func TestAuditRejectsInvalidJSON(t *testing.T) {
	l := New(&fakeExec{})
	if err := l.LogEvent(context.Background(), Event{Action: ActionTokenCreate, Actor: 1, Detail: json.RawMessage(`{`)}); err == nil {
		t.Fatal("非法 JSON detail 应返回错误")
	}
}

// TestAuditPropagatesWriteError：审计写失败必须把错误交给调用方（由调用方决定降级）。
func TestAuditPropagatesWriteError(t *testing.T) {
	want := errors.New("logs db down")
	l := New(&fakeExec{err: want})
	if err := l.LogEvent(context.Background(), Event{Action: ActionTokenCreate, Actor: 1}); !errors.Is(err, want) {
		t.Fatalf("err = %v，期望透传 %v", err, want)
	}
}

// TestAuditPackageLogFrozenSignature 覆盖接口冻结的包级 audit.Log 形态。
func TestAuditPackageLogFrozenSignature(t *testing.T) {
	exec := &fakeExec{}
	SetDefault(New(exec))
	t.Cleanup(func() { SetDefault(nil) })

	if err := Log(context.Background(), ActionTokenCreate, 9, json.RawMessage(`{"token_id":3}`)); err != nil {
		t.Fatalf("包级 Log: %v", err)
	}
	if len(exec.calls) != 1 {
		t.Fatalf("包级 Log 未写入审计行")
	}
	if got := exec.calls[0].args[1]; got != "user#9" {
		t.Errorf("无用户名快照时 username = %v，期望 user#9", got)
	}
}

// TestAuditPackageLogUnconfigured：未安装默认 logger 时明确报错，不静默丢弃审计。
func TestAuditPackageLogUnconfigured(t *testing.T) {
	SetDefault(nil)
	if err := Log(context.Background(), ActionTokenCreate, 1, nil); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v，期望 ErrNotConfigured", err)
	}
}

// TestAuditRedactsCompoundCredentialKeys 覆盖 D1-5：复合凭据键（access_token、
// refresh_token、id_token、bearer_token、token_hash 等）归一化后是独立组合词，
// 不会被通用 "token" 命中；带空白的键名也必须脱敏，否则空白就能绕过匹配。
// 计数键 backup_codes_count 归一化后不在集合内，必须保留数值（modules-05 §7.4）。
func TestAuditRedactsCompoundCredentialKeys(t *testing.T) {
	exec := &fakeExec{}
	l := New(exec)
	detail := json.RawMessage(`{
		"access_token":"oauth-secret","refresh_token":"refresh-secret","id_token":"id-secret",
		"bearer_token":"bearer-secret","token_hash":"hash-secret",
		"client_secret":"client-secret","private_key":"pem-secret",
		" Access_Token ":"padded-secret",
		"backup_codes_count":7,"backup_codes":["one","two"]
	}`)
	if err := l.LogEvent(context.Background(), Event{Action: ActionAuth2FAEnable, Actor: 1, Detail: detail}); err != nil {
		t.Fatalf("LogEvent: %v", err)
	}
	got := decodeArg(t, exec.calls[0].args[9])

	for _, key := range []string{
		"access_token", "refresh_token", "id_token", "bearer_token",
		"token_hash", "client_secret", "private_key", " Access_Token ",
	} {
		if got[key] != redacted {
			t.Errorf("%q = %#v，期望 %q", key, got[key], redacted)
		}
	}
	if got["backup_codes"] != redacted {
		t.Errorf("backup_codes = %#v，期望仍强制脱敏", got["backup_codes"])
	}
	if got["backup_codes_count"] != float64(7) {
		t.Errorf("backup_codes_count = %#v，期望保留数值 7（计数不是凭据）", got["backup_codes_count"])
	}
	raw, _ := json.Marshal(got)
	for _, secret := range []string{
		"oauth-secret", "refresh-secret", "id-secret", "bearer-secret",
		"hash-secret", "client-secret", "pem-secret", "padded-secret",
	} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("detail 泄露了复合凭据 %q: %s", secret, raw)
		}
	}
}

// TestAuditWriterInjectsDetailAction 覆盖裁决 R-43：detail.action 恒等于冻结
// 动作枚举，由 writer 注入；调用侧手写的动作名必须被覆盖而不是并存。
func TestAuditWriterInjectsDetailAction(t *testing.T) {
	t.Run("对象详情覆盖调用侧 action", func(t *testing.T) {
		exec := &fakeExec{}
		l := New(exec)
		if err := l.LogEvent(context.Background(), Event{
			Action: ActionTokenBatchEnable,
			Actor:  1,
			Detail: json.RawMessage(`{"action":"enable","token_id":7}`),
		}); err != nil {
			t.Fatalf("LogEvent: %v", err)
		}
		got := decodeArg(t, exec.calls[0].args[9])
		if got["action"] != ActionTokenBatchEnable {
			t.Errorf("detail.action = %v，期望冻结动作 %q", got["action"], ActionTokenBatchEnable)
		}
		if got["token_id"] != float64(7) {
			t.Errorf("其余键未保留: %#v", got)
		}
	})

	t.Run("无详情生成仅含 action 的对象", func(t *testing.T) {
		exec := &fakeExec{}
		l := New(exec)
		if err := l.LogEvent(context.Background(), Event{Action: ActionTokenCreate, Actor: 1}); err != nil {
			t.Fatalf("LogEvent: %v", err)
		}
		got := decodeArg(t, exec.calls[0].args[9])
		if len(got) != 1 || got["action"] != ActionTokenCreate {
			t.Errorf("detail = %#v，期望仅含 action=%q", got, ActionTokenCreate)
		}
	})

	t.Run("非对象详情原样保留", func(t *testing.T) {
		exec := &fakeExec{}
		l := New(exec)
		if err := l.LogEvent(context.Background(), Event{
			Action: ActionTokenCreate,
			Actor:  1,
			Detail: json.RawMessage(`["a","b"]`),
		}); err != nil {
			t.Fatalf("LogEvent: %v", err)
		}
		raw, ok := exec.calls[0].args[9].(string)
		if !ok {
			t.Fatalf("detail 参数类型 = %T，期望 string", exec.calls[0].args[9])
		}
		if raw != `["a","b"]` {
			t.Errorf("非对象 detail 被改写: %s", raw)
		}
	})
}

// TestAuditTruncatesUserAgentAtUTF8Boundary 覆盖 D1-11：UA 在写入侧按
// 512 字节上限截断，且不得把多字节字符切成非法 UTF-8。
func TestAuditTruncatesUserAgentAtUTF8Boundary(t *testing.T) {
	write := func(t *testing.T, ua string) string {
		t.Helper()
		exec := &fakeExec{}
		l := New(exec)
		if err := l.LogEvent(context.Background(), Event{Action: ActionTokenCreate, Actor: 1, UserAgent: ua}); err != nil {
			t.Fatalf("LogEvent: %v", err)
		}
		got, ok := exec.calls[0].args[4].(string)
		if !ok {
			t.Fatalf("user_agent 参数类型 = %T，期望 string", exec.calls[0].args[4])
		}
		if len(got) > 512 {
			t.Fatalf("user_agent 长度 = %d，期望 ≤512", len(got))
		}
		if !utf8.ValidString(got) {
			t.Fatalf("user_agent 被截断成非法 UTF-8：%q", got)
		}
		return got
	}

	// 511 ASCII + 4 字节 emoji：第 512 字节落在 emoji 内部，必须回退到 511。
	long := strings.Repeat("a", 511) + "👍" + strings.Repeat("b", 10)
	if got := write(t, long); got != strings.Repeat("a", 511) {
		t.Fatalf("跨 rune 边界截断 = %q，期望 511 个 a", got)
	}

	// 恰好 512 字节且以完整多字节字符结尾：原样保留。
	exact := strings.Repeat("a", 510) + "é"
	if got := write(t, exact); got != exact {
		t.Fatalf("恰好 512 字节的 UA 被改写（len=%d）", len(got))
	}

	// 纯 ASCII 超限：截到 512。
	ascii := strings.Repeat("x", 600)
	if got := write(t, ascii); got != strings.Repeat("x", 512) {
		t.Fatalf("ASCII UA 截断长度 = %d，期望 512", len(got))
	}
}
