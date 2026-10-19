package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/store"
)

// GET /api/status 的 usable_groups（M6c Task 13 冻结形状）：
//   - 匿名请求不读用户分组、不 401、usable_groups 为空数组；
//   - 有效会话下发 user_selectable/当前分组的分组与模型清单；
//   - 查询失败降级为空数组，绝不让公开状态页 5xx；
//   - 响应仍为 Cache-Control: no-store。
type fakeUsableGroupLookup struct {
	groups []store.UsableGroup
	err    error
	calls  int
	userID int64
}

func (f *fakeUsableGroupLookup) UsableGroupsForUser(_ context.Context, userID int64) ([]store.UsableGroup, error) {
	f.calls++
	f.userID = userID
	if f.err != nil {
		return nil, f.err
	}
	return f.groups, nil
}

type statusUsableGroupsBody struct {
	UsableGroups []struct {
		Name        string   `json:"name"`
		Description string   `json:"description"`
		Ratio       int32    `json:"ratio"`
		Models      []string `json:"models"`
	} `json:"usable_groups"`
}

func decodeUsableGroups(t *testing.T, raw []byte) statusUsableGroupsBody {
	t.Helper()
	var body statusUsableGroupsBody
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode status: %v (body=%s)", err, raw)
	}
	return body
}

func TestStatusUsableGroupsAnonymousIsEmptyAndDoesNotReadGroups(t *testing.T) {
	lookup := &fakeUsableGroupLookup{
		groups: []store.UsableGroup{{Name: "default", Models: []string{"gpt-5"}}},
	}
	env := newAuthTestEnv(t, mapConfig{}, authTestOptions{userGroups: lookup})

	rr := env.request(http.MethodGet, "/api/status", nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if lookup.calls != 0 {
		t.Fatalf("匿名请求读取了用户分组（calls=%d），必须为零", lookup.calls)
	}
	body := decodeUsableGroups(t, rr.Body.Bytes())
	if body.UsableGroups == nil {
		t.Fatal("usable_groups 必须是空数组而不是缺失/null")
	}
	if len(body.UsableGroups) != 0 {
		t.Fatalf("匿名 usable_groups = %+v, want []", body.UsableGroups)
	}
	if got := rr.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
}

func TestStatusUsableGroupsForSession(t *testing.T) {
	lookup := &fakeUsableGroupLookup{groups: []store.UsableGroup{
		{Name: "default", Description: "默认分组", Ratio: 100, Models: []string{"gpt-5", "gpt-5-mini"}},
		{Name: "vip", Description: "VIP", Ratio: 120, Models: nil},
	}}
	env := newAuthTestEnv(t, mapConfig{}, authTestOptions{userGroups: lookup})

	sid, err := env.sessions.Create(context.Background(), 42,
		auth.SessionOpts{Role: "user", UA: "test", IP: "203.0.113.9"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	rr := env.request(http.MethodGet, "/api/status", nil, func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: sid})
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	if lookup.calls != 1 || lookup.userID != 42 {
		t.Fatalf("lookup calls=%d userID=%d, want 1/42", lookup.calls, lookup.userID)
	}
	body := decodeUsableGroups(t, rr.Body.Bytes())
	if len(body.UsableGroups) != 2 {
		t.Fatalf("usable_groups = %+v, want 2", body.UsableGroups)
	}
	first := body.UsableGroups[0]
	if first.Name != "default" || first.Description != "默认分组" || first.Ratio != 100 ||
		len(first.Models) != 2 || first.Models[0] != "gpt-5" {
		t.Fatalf("first group = %+v", first)
	}
	// nil 模型清单必须编码为 []（前端用它区分「无可用模型」与「未知」）。
	if body.UsableGroups[1].Models == nil || len(body.UsableGroups[1].Models) != 0 {
		t.Fatalf("empty group models = %#v, want []", body.UsableGroups[1].Models)
	}
	if got := rr.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
}

func TestStatusUsableGroupsLookupFailureDegradesToEmpty(t *testing.T) {
	lookup := &fakeUsableGroupLookup{err: errors.New("pq: dsn postgres://secret@host/db")}
	env := newAuthTestEnv(t, mapConfig{}, authTestOptions{userGroups: lookup})

	sid, err := env.sessions.Create(context.Background(), 43, auth.SessionOpts{Role: "user"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	rr := env.request(http.MethodGet, "/api/status", nil, func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: sid})
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200（个性化字段失败不得让公开状态页失败）", rr.Code)
	}
	body := decodeUsableGroups(t, rr.Body.Bytes())
	if len(body.UsableGroups) != 0 {
		t.Fatalf("usable_groups = %+v, want []", body.UsableGroups)
	}
	if body := rr.Body.String(); strings.Contains(body, "postgres://") || strings.Contains(body, "secret@host") {
		t.Fatalf("响应泄漏存储细节: %s", body)
	}
}
