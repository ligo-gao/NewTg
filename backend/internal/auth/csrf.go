package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/redis/go-redis/v9"
)

// fieldCSRFHash 是会话 hash 中同步器令牌的派生值字段（security-01 §2.2）。
const fieldCSRFHash = "csrf_hash"

// errEmptyCSRFToken 表示调用方试图签发空令牌；正常路径只有 NewCSRFToken 的输出。
var errEmptyCSRFToken = errors.New("auth: csrf token must not be empty")

// NewCSRFToken 生成 base64url(CSPRNG(32 bytes)) 的 CSRF 令牌（ASVS 3.5.1）。
// CSPRNG 失败属于灾难性状态：panic 让请求以 500 fail-closed，而不是返回可预测令牌。
func NewCSRFToken() string {
	buf := make([]byte, CSRFTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		panic("auth: crypto/rand failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

// IssueCSRF 把 HMAC-SHA256(SESSION_SECRET, token) 写入会话 hash。它同时是轮换原语：
// 改密、再认证、2FA 状态变更、角色/权限变更后重新调用即可让旧令牌立即失效
// （security-01 §2.2「刷新时机」冻结）。
//
// 为什么存 HMAC 而不是明文：Redis 快照/备份泄露时，攻击者拿到的派生值无法反推出
// 可提交的令牌；比对时服务端从请求头重算派生值即可（同步器令牌形态）。
func (m *SessionManager) IssueCSRF(ctx context.Context, sid, token string) error {
	if !validSessionID(sid) {
		return ErrSessionNotFound
	}
	if token == "" {
		return errEmptyCSRFToken
	}
	ok, err := issueCSRFScript.Run(ctx, m.rdb,
		[]string{sessionKey(sid)},
		m.csrfHash(token),
	).Int64()
	if err != nil {
		return fmt.Errorf("auth: issue csrf token: %w", err)
	}
	if ok == 0 {
		// EXISTS 检查在脚本内完成，避免给已吊销会话凭空创建只有 csrf_hash 的僵尸键。
		return ErrSessionNotFound
	}
	return nil
}

// VerifyCSRF 以常量时间比对请求头令牌与服务端派生值。
// Redis 不可用时返回 false（I12 fail-closed）；调用方应返回 403 csrf_failed。
//
// 说明：会话本身的有效性由鉴权中间件先行校验（Get），本方法只回答「令牌是否匹配」。
func (m *SessionManager) VerifyCSRF(ctx context.Context, sid, headerToken string) bool {
	if !validSessionID(sid) || headerToken == "" {
		return false
	}
	stored, err := m.rdb.HGet(ctx, sessionKey(sid), fieldCSRFHash).Result()
	if err != nil || stored == "" {
		return false
	}
	// hmac.Equal 对等长切片做常量时间比较；两侧都是 64 字符十六进制，
	// 不会因前缀匹配提前返回（security-01 §2.2 要求常量时间）。
	return hmac.Equal([]byte(m.csrfHash(headerToken)), []byte(stored))
}

// csrfHash 返回 HMAC-SHA256(SESSION_SECRET, token) 的小写十六进制。
func (m *SessionManager) csrfHash(token string) string {
	mac := hmac.New(sha256.New, m.secret)
	_, _ = mac.Write([]byte(token))
	return hex.EncodeToString(mac.Sum(nil))
}

// issueCSRFScript 仅在会话存在时写入派生值；HSET 不改动既有 TTL，
// 因此轮换令牌不会顺带延长会话寿命。
var issueCSRFScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 0 then
  return 0
end
redis.call('HSET', KEYS[1], 'csrf_hash', ARGV[1])
return 1
`)
