// 本文件实现 security-07 §2.1/§2.2.1 的认证面请求速率桶：
//
//	B1 登录      5 次/分钟/IP+identifier
//	B3 注册      3 次/分钟/IP
//	B8 公开状态  60 次/分钟/IP+path（冻结值）
//
// 算法是固定窗口 Lua 脚本（原子 INCR+EXPIRE），与基线 rateLimit:v2 命名空间
// 一致。固定窗口在窗口交界允许约 2× 突刺，这是 security-07 §2.2.1 明确接受
// 的语义（照搬基线、不改算法）。
//
// W7/M5-a 起，脚本的单一实现位于 internal/ratelimit；本文件的 allowRequestRate
// 只是保留原签名的薄委托，供既有认证面/公开面/管理面调用点继续使用。
//
// 键命名（冻结前缀 rateLimit:v2:ip:<mark>:<clientIP>）：
//   - B3 只有 IP 维，恰好落在冻结形状上；
//   - B1/B8 的维度分别是 IP+identifier / IP+path，因此在冻结前缀后追加第二维
//     （identifier 先 SHA-256，避免用户名/邮箱明文进入 Redis 快照）。
//     这是对冻结键形的扩展，已登记在任务报告。
//
// 失败策略（security-07 §2.3）：登录/注册 fail-closed（503 + Retry-After），
// 公开只读面 fail-open（放行，由调用方记录告警）。
package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/ratelimit"
)

const (
	// RateLimitKeyPrefix 是 security-07 §2.2.2 冻结的 IP 维命名空间。
	RateLimitKeyPrefix = "rateLimit:v2:ip:"

	// 桶标记沿用基线的两位大写风格（GW/GA/CT/EV）。
	LoginRateMark      = "LI"
	RegisterRateMark   = "RG"
	PublicReadRateMark = "PB"
	SetupRateMark      = "ST"

	// B1：登录请求速率（modules-08 F-08-16 唯一真源）。
	LoginRateLimit  = 5
	LoginRateWindow = time.Minute

	// B3：注册请求速率（modules-08 F-08-16）。
	RegisterRateLimit  = 3
	RegisterRateWindow = time.Minute

	// B8：公开只读面（status/pricing/rankings/live-feed）60 次/分钟/IP+path。
	PublicReadRateLimit  = 60
	PublicReadRateWindow = time.Minute

	// B13：/api/setup 的保守匿名桶（控制端裁决）。设计未分配桶，但初始化前
	// 每个请求都付一次 argon2 成本，且未初始化窗口允许任何人抢先建首管；
	// 10 次/分钟/IP 只拦住自动化轰炸，不影响人工安装向导的正常重试。
	SetupRateLimit  = 10
	SetupRateWindow = time.Minute

	// B14：渠道凭据揭示（security-03 §2.10 / modules-06 F-06-03）。设计冻结
	// 「按管理员 10 次/小时」，维度是**管理员**而不是 IP（同一管理员换 IP
	// 不应刷新配额）。超限 429 + Retry-After，计数只在服务端维护。
	ChannelRevealRateMark   = "CR"
	ChannelRevealRateLimit  = 10
	ChannelRevealRateWindow = time.Hour
)

// SetupRateLimitKey 构造 B13 桶键：rateLimit:v2:ip:ST:<ip>。
func SetupRateLimitKey(ip string) string {
	return RateLimitKeyPrefix + SetupRateMark + ":" + ip
}

// LoginRateLimitKey 构造 B1 桶键：rateLimit:v2:ip:LI:<ip>:<sha256(identifier)>。
// identifier 先小写归一，使大小写变体共享同一桶。
func LoginRateLimitKey(ip, identifier string) string {
	return RateLimitKeyPrefix + LoginRateMark + ":" + ip + ":" +
		sha256Hex(strings.ToLower(strings.TrimSpace(identifier)))
}

// RegisterRateLimitKey 构造 B3 桶键：rateLimit:v2:ip:RG:<ip>（冻结形状）。
func RegisterRateLimitKey(ip string) string {
	return RateLimitKeyPrefix + RegisterRateMark + ":" + ip
}

// PublicReadRateLimitKey 构造 B8 桶键：rateLimit:v2:ip:PB:<ip>:<path>。
func PublicReadRateLimitKey(ip, path string) string {
	return RateLimitKeyPrefix + PublicReadRateMark + ":" + ip + ":" + path
}

// ChannelRevealRateLimitKey 构造 B14 桶键：rateLimit:v2:user:CR:<userID>。
//
// 冻结前缀 RateLimitKeyPrefix 是 IP 维命名空间；凭据揭示按**管理员**计数
// （security-03 §2.10），因此显式使用 user 维段而不是把 userID 塞进 ip 段——
// 键必须自描述，运维直读 Redis 时不能把用户 id 误读成地址。这是对冻结键形
// 的扩展，与 B1/B8 的第二维扩展同一处理方式。
func ChannelRevealRateLimitKey(userID int64) string {
	return "rateLimit:v2:user:" + ChannelRevealRateMark + ":" + strconv.FormatInt(userID, 10)
}

// allowRequestRate 消耗固定窗口中的一个名额。
//
// 单一实现已迁入 internal/ratelimit（R-M5-01）；本函数保留原签名，让
// 认证面/公开面/管理面的既有调用点与各自的 fail 策略保持不变。
// 返回 over=true 表示本次请求超限，retryAfter 为当前窗口剩余时间。
// Redis 错误原样上抛，由调用方按各面的 fail 策略处理。
//
// 注意：这些既有桶的阈值都 >0；ratelimit.Allow 对 limit<=0 采用「不限」
// 语义（M5 的 0=不限 裁决），不改变任何现有桶的行为。
func allowRequestRate(ctx context.Context, rdb *redis.Client, key string, limit int, window time.Duration) (bool, time.Duration, error) {
	decision, err := ratelimit.New(rdb).Allow(ctx, ratelimit.Bucket{
		Name:   "httpapi",
		Limit:  limit,
		Window: window,
		// FailMode 只回填到 Decision，不参与 Allow 的返回值；各调用点的
		// fail-open/fail-closed 仍由它们自己决定（见本文件头注释）。
	}, key, 1)
	if err != nil {
		return false, 0, err
	}
	return !decision.Allowed, decision.RetryAfter, nil
}

// allowRequestRate 是 authHandlers 的便捷形态（复用同一 Redis 客户端）。
func (h *authHandlers) allowRequestRate(ctx context.Context, key string, limit int, window time.Duration) (bool, time.Duration, error) {
	return allowRequestRate(ctx, h.rdb, key, limit, window)
}

// writeRateLimited 输出 429 + Retry-After（modules-00 §429 文案口径）。
func writeRateLimited(w http.ResponseWriter, retry time.Duration, title string) {
	w.Header().Set("Retry-After", retrySeconds(retry))
	writeProblem(w, http.StatusTooManyRequests, authProblemTooManyRequests, title)
}
