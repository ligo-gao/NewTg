package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

// TestCSRFIssueVerify 覆盖 brief (e)：签发后正确 token 通过、错误/空 token 拒绝；
// 存储值是 HMAC-SHA256(SESSION_SECRET, token) 而非 token 明文。
func TestCSRFIssueVerify(t *testing.T) {
	ctx := context.Background()
	m, _, _ := newTestSessionManager(t)
	sid := mustCreateSession(t, m, 42, "user")

	token := NewCSRFToken()
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != CSRFTokenBytes {
		t.Fatalf("CSRF token 必须是 %d 字节的 base64url：len(raw)=%d err=%v", CSRFTokenBytes, len(raw), err)
	}

	if err := m.IssueCSRF(ctx, sid, token); err != nil {
		t.Fatalf("IssueCSRF 失败: %v", err)
	}
	if !m.VerifyCSRF(ctx, sid, token) {
		t.Error("正确 token 的 VerifyCSRF 应为 true")
	}
	if m.VerifyCSRF(ctx, sid, "wrong-token") {
		t.Error("错误 token 的 VerifyCSRF 应为 false")
	}
	if m.VerifyCSRF(ctx, sid, "") {
		t.Error("空 token 的 VerifyCSRF 应为 false")
	}
	if m.VerifyCSRF(ctx, "", token) {
		t.Error("空 sid 的 VerifyCSRF 应为 false")
	}
	unknown := base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("x", SessionIDBytes)))
	if m.VerifyCSRF(ctx, unknown, token) {
		t.Error("不存在会话的 VerifyCSRF 应为 false（fail-closed）")
	}

	// 服务端只存 HMAC：取 Redis 字段与独立计算的 HMAC 比对。
	stored, err := m.rdb.HGet(ctx, sessionKey(sid), fieldCSRFHash).Result()
	if err != nil {
		t.Fatalf("读取 csrf_hash 失败: %v", err)
	}
	mac := hmac.New(sha256.New, []byte(testSessionSecret))
	_, _ = mac.Write([]byte(token))
	want := hex.EncodeToString(mac.Sum(nil))
	if stored != want {
		t.Errorf("csrf_hash = %q，期望 HMAC-SHA256(SESSION_SECRET, token) = %q", stored, want)
	}
	if stored == token || strings.Contains(stored, token) {
		t.Error("csrf_hash 不得包含 token 明文")
	}
}

// TestCSRFRotation 覆盖改密/角色变更时的轮换语义：IssueCSRF 是轮换原语，
// 新 token 生效后旧 token 立即失效；轮换目标会话不存在时返回 not-found。
func TestCSRFRotation(t *testing.T) {
	ctx := context.Background()
	m, _, _ := newTestSessionManager(t)
	sid := mustCreateSession(t, m, 42, "user")

	tokenA := NewCSRFToken()
	tokenB := NewCSRFToken()
	if tokenA == tokenB {
		t.Fatal("两次生成的 CSRF token 不应相同")
	}
	if err := m.IssueCSRF(ctx, sid, tokenA); err != nil {
		t.Fatalf("签发 tokenA 失败: %v", err)
	}
	if err := m.IssueCSRF(ctx, sid, tokenB); err != nil {
		t.Fatalf("轮换 tokenB 失败: %v", err)
	}
	if m.VerifyCSRF(ctx, sid, tokenA) {
		t.Error("轮换后旧 token 必须失效")
	}
	if !m.VerifyCSRF(ctx, sid, tokenB) {
		t.Error("轮换后新 token 必须通过")
	}

	unknown := base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("y", SessionIDBytes)))
	if err := m.IssueCSRF(ctx, unknown, NewCSRFToken()); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("对不存在会话 IssueCSRF = %v，期望 ErrSessionNotFound", err)
	}
	if err := m.IssueCSRF(ctx, sid, ""); err == nil {
		t.Error("空 token IssueCSRF 必须报错")
	}
}

// TestCSRFTokensUnique 验证令牌来自 CSPRNG：100 次生成无重复、长度符合 256 bit。
func TestCSRFTokensUnique(t *testing.T) {
	seen := make(map[string]bool, 100)
	for i := 0; i < 100; i++ {
		tok := NewCSRFToken()
		if got := base64.RawURLEncoding.EncodedLen(CSRFTokenBytes); len(tok) != got {
			t.Fatalf("token 长度 = %d，期望 %d", len(tok), got)
		}
		if seen[tok] {
			t.Fatalf("第 %d 次生成出现重复 token", i)
		}
		seen[tok] = true
	}
}

// TestCSRFRedisDownFailsClosed 覆盖 I12：VerifyCSRF 是 bool 签名，Redis 不可用时
// 必须返回 false（拒绝），IssueCSRF 必须返回错误。
func TestCSRFRedisDownFailsClosed(t *testing.T) {
	ctx := context.Background()
	m, mr, _ := newTestSessionManager(t)
	sid := mustCreateSession(t, m, 42, "user")
	token := NewCSRFToken()
	if err := m.IssueCSRF(ctx, sid, token); err != nil {
		t.Fatalf("IssueCSRF 失败: %v", err)
	}

	mr.Close()

	if m.VerifyCSRF(ctx, sid, token) {
		t.Fatal("Redis 不可用时 VerifyCSRF 必须拒绝（false）")
	}
	if err := m.IssueCSRF(ctx, sid, NewCSRFToken()); err == nil {
		t.Fatal("Redis 不可用时 IssueCSRF 必须返回错误")
	}
}
