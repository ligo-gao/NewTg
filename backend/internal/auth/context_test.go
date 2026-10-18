package auth

import (
	"context"
	"testing"
)

// TestPrincipalRoundTrip 覆盖 brief 的上下文契约：WithPrincipal 注入的身份必须能
// 由 PrincipalFrom 原样取回（含 Bearer 路径的 TokenID 指针）。
func TestPrincipalRoundTrip(t *testing.T) {
	tokenID := int64(9)
	want := &Principal{
		UserID:      42,
		Role:        "admin",
		Permissions: []string{"users:read", "settings:manage"},
		TokenID:     &tokenID,
	}

	ctx := WithPrincipal(context.Background(), want)
	got, ok := PrincipalFrom(ctx)
	if !ok {
		t.Fatal("PrincipalFrom() 未取到已注入的身份")
	}
	if got != want {
		t.Fatalf("PrincipalFrom() = %+v，期望同一个指针 %+v", got, want)
	}
	if got.TokenID == nil || *got.TokenID != tokenID {
		t.Errorf("TokenID = %v，期望指向 %d", got.TokenID, tokenID)
	}
}

// TestPrincipalFromMissing 断言未注入时返回 ok=false 且 nil：
// 调用方必须依据 ok 判定，而不是对 nil 解引用（否则未认证请求会 panic 成 500）。
func TestPrincipalFromMissing(t *testing.T) {
	got, ok := PrincipalFrom(context.Background())
	if ok || got != nil {
		t.Fatalf("PrincipalFrom(空上下文) = (%+v, %v)，期望 (nil, false)", got, ok)
	}
}

// TestWithPrincipalNilLeavesContextEmpty 防止把 nil 身份当作「已认证」注入：
// nil 必须是 no-op，PrincipalFrom 仍返回 ok=false。
func TestWithPrincipalNilLeavesContextEmpty(t *testing.T) {
	ctx := WithPrincipal(context.Background(), nil)
	if got, ok := PrincipalFrom(ctx); ok || got != nil {
		t.Fatalf("WithPrincipal(nil) 后 PrincipalFrom() = (%+v, %v)，期望 (nil, false)", got, ok)
	}
}

// TestHashTokenKey 冻结 Bearer 明文到 Task 12 key_hash 的编码口径：
// SHA-256 的小写十六进制，与 HashSessionID 保持同一种编码，避免两套哈希漂移。
func TestHashTokenKey(t *testing.T) {
	// 空串的 SHA-256 是公开测试向量，可发现「没真正哈希」的实现。
	const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got := HashTokenKey(""); got != emptySHA256 {
		t.Errorf("HashTokenKey(\"\") = %q，期望 %q", got, emptySHA256)
	}

	a := HashTokenKey("nkt-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	b := HashTokenKey("nkt-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	if len(a) != 64 {
		t.Errorf("HashTokenKey 长度 = %d，期望 SHA-256 十六进制的 64", len(a))
	}
	if a == b {
		t.Error("不同明文得到了相同哈希")
	}
}

// TestAuthCookieConstants 冻结 security-01 §3.1 的 Cookie/头名。
// 这些名字是前端与后端之间的线协议，改动必须是设计变更而非重构。
func TestAuthCookieConstants(t *testing.T) {
	if SessionCookieName != "__Host-nkt_sess" {
		t.Errorf("SessionCookieName = %q，期望 __Host-nkt_sess", SessionCookieName)
	}
	if CSRFCookieName != "__Host-nkt_csrf" {
		t.Errorf("CSRFCookieName = %q，期望 __Host-nkt_csrf", CSRFCookieName)
	}
	if CSRFHeaderName != "X-CSRF-Token" {
		t.Errorf("CSRFHeaderName = %q，期望 X-CSRF-Token", CSRFHeaderName)
	}
}
