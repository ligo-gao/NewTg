package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
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
		ActionAuthLogin:          "login",
		ActionAuthLogout:         "login",
		ActionAuthRegister:       "login",
		ActionAuthPasswordChange: "security",
		ActionAuthPasswordReset:  "security",
		ActionAuth2FAEnable:      "security",
		ActionAuth2FADisable:     "security",
		ActionTokenCreate:        "operation",
		ActionTokenUpdate:        "operation",
		ActionTokenDelete:        "operation",
		ActionTokenResetKey:      "operation",
		ActionTokenEnable:        "operation",
		ActionTokenDisable:       "operation",
		ActionTokenBatchEnable:   "operation",
		ActionTokenBatchDisable:  "operation",
		ActionTokenBatchDelete:   "operation",
		ActionUserCreate:         "operation",
		ActionUserQuotaAdjust:    "operation",
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
