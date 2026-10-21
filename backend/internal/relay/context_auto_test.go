package relay

// W7/M2 鉴权层用例：group=auto 的 R-61 解析（UsingGroup/AutoGroups/
// CrossGroupRetry）与具体分组/会话路径的零值语义。

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"newtg/gateway/internal/auth"
)

func autoTokenMeta(groups []string, retry bool) *auth.TokenMeta {
	return &auth.TokenMeta{
		ID:              9,
		UserID:          42,
		Status:          "enabled",
		Group:           "auto",
		AutoGroups:      groups,
		CrossGroupRetry: retry,
		UnlimitedQuota:  true,
	}
}

func autoBearerRequest() *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-x"}`))
	req.Header.Set("Authorization", "Bearer "+testNKT)
	return req
}

func TestBuildRelayContextAutoGroups(t *testing.T) {
	tokenID := int64(9)
	principal := &auth.Principal{UserID: 42, Role: "user", TokenID: &tokenID}

	t.Run("auto 解析首组并携带有序全表", func(t *testing.T) {
		store := &stubTokenStore{metas: map[string]*auth.TokenMeta{
			auth.HashTokenKey(testNKT): autoTokenMeta([]string{"g1", "g2"}, true),
		}}
		rc, err := BuildRelayContext(context.Background(), principal, autoBearerRequest(), store)
		if err != nil {
			t.Fatalf("BuildRelayContext() error = %v", err)
		}
		if rc.UsingGroup != "g1" {
			t.Fatalf("UsingGroup = %q，期望 AutoGroups[0]=g1（R-61）", rc.UsingGroup)
		}
		// 补充口径 #3：UserGroup 保持令牌分组的字面值，不改成 UsingGroup。
		if rc.UserGroup != "auto" {
			t.Fatalf("UserGroup = %q，期望字面 auto", rc.UserGroup)
		}
		if len(rc.AutoGroups) != 2 || rc.AutoGroups[0] != "g1" || rc.AutoGroups[1] != "g2" {
			t.Fatalf("AutoGroups = %v，期望 [g1 g2]", rc.AutoGroups)
		}
		if !rc.CrossGroupRetry {
			t.Fatal("CrossGroupRetry 未从令牌投影带入")
		}
	})

	t.Run("空列表 fail-closed 且不回退用户分组", func(t *testing.T) {
		store := &stubTokenStore{metas: map[string]*auth.TokenMeta{
			auth.HashTokenKey(testNKT): autoTokenMeta(nil, true),
		}}
		rc, err := BuildRelayContext(context.Background(), principal, autoBearerRequest(), store)
		if !errors.Is(err, ErrAutoGroupsNotEnabled) {
			t.Fatalf("error = %v，期望 ErrAutoGroupsNotEnabled", err)
		}
		if rc != nil {
			t.Fatalf("失败时必须返回 nil 上下文，实际 %#v", rc)
		}
	})

	t.Run("具体分组忽略越界的 auto 字段", func(t *testing.T) {
		meta := &auth.TokenMeta{
			ID: 9, UserID: 42, Status: "enabled", Group: "vip",
			AutoGroups: []string{"g1"}, CrossGroupRetry: true, UnlimitedQuota: true,
		}
		store := &stubTokenStore{metas: map[string]*auth.TokenMeta{
			auth.HashTokenKey(testNKT): meta,
		}}
		rc, err := BuildRelayContext(context.Background(), principal, autoBearerRequest(), store)
		if err != nil {
			t.Fatalf("BuildRelayContext() error = %v", err)
		}
		if rc.UsingGroup != "vip" || rc.UserGroup != "vip" {
			t.Fatalf("分组 = %q/%q，期望 vip/vip", rc.UsingGroup, rc.UserGroup)
		}
		if len(rc.AutoGroups) != 0 || rc.CrossGroupRetry {
			t.Fatalf("具体分组不得携带 auto 语义：AutoGroups=%v Retry=%v",
				rc.AutoGroups, rc.CrossGroupRetry)
		}
	})

	t.Run("会话路径无自动分组语义", func(t *testing.T) {
		store := &stubTokenStore{}
		rc, err := BuildRelayContext(context.Background(),
			&auth.Principal{UserID: 42, Role: "user"}, autoBearerRequest(), store)
		if err != nil {
			t.Fatalf("BuildRelayContext() error = %v", err)
		}
		if len(rc.AutoGroups) != 0 || rc.CrossGroupRetry || rc.UsingGroup != "" {
			t.Fatalf("会话路径字段 = UsingGroup:%q Auto:%v Retry:%v，期望全零",
				rc.UsingGroup, rc.AutoGroups, rc.CrossGroupRetry)
		}
		if len(store.calls) != 0 {
			t.Fatalf("会话路径不得查询令牌存储，实际调用 %d 次", len(store.calls))
		}
	})
}
