package httpapi

// Turnstile 接入点的「handler 内调用」形态。
//
// 设计冻结顺序是「限流（外层）→ Turnstile（内层）→ 业务 handler」
// （modules-01 §3.15）。M1 的登录/注册/发码/找回密码速率桶都在 handler 内部，
// 若把 Turnstile 作为路由中间件包在 handler 外，就会先于限流执行——攻击者
// 可以用无效人机令牌打满 Cloudflare 调用而不触发任何本地限流。因此本文件把
// auth.TurnstileMiddleware 包装成一个可在速率桶之后显式调用的 gate。
//
// 令牌通过查询参数注入：handler 此时已经解析/消费过请求体，gate 不再读取
// 或重放 Body；auth.TurnstileMiddleware 会优先从查询参数取 turnstile_token，
// 语义（一次性、接入点绑定、remoteip、fail-closed）与中间件完全一致。
//
// 已存在的 4 个接入点在这里接线：
//   - auth.TurnstileEndpointLogin          → POST /api/auth/login
//   - auth.TurnstileEndpointRegister       → POST /api/auth/register
//   - auth.TurnstileEndpointVerification   → POST /api/auth/email-code
//   - auth.TurnstileEndpointResetPassword  → POST /api/auth/password-reset/request
//
// 第 5 个接入点 auth.TurnstileEndpointCheckin（POST /api/checkin）在 M1 尚未
// 实现：checkin 端点与入账逻辑属 modules-03 F-03-12，等该端点落地后按同一
// 模式在 handler 内速率桶之后接入即可。

import (
	"log/slog"
	"net/http"

	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/auth"
)

// turnstileGates 按接入点持有一个已配置的 auth.TurnstileMiddleware。配置
// 快照通过 Settings 函数按请求读取，保证开关与密钥轮换即时生效。
type turnstileGates struct {
	byEndpoint map[string]*turnstileGate
}

type turnstileGate struct {
	middleware func(http.Handler) http.Handler
}

// newTurnstileGates 为四个已存在的接入点构造 gate。rdb 为 nil 时开启态会
// fail-closed（auth.TurnstileMiddleware 的重放守卫拒绝），与中间件契约一致。
func newTurnstileGates(settings func() auth.TurnstileSettings, rdb *redis.Client,
	verifyURL string, client *http.Client, log *slog.Logger) *turnstileGates {

	endpoints := []string{
		auth.TurnstileEndpointLogin,
		auth.TurnstileEndpointRegister,
		auth.TurnstileEndpointVerification,
		auth.TurnstileEndpointResetPassword,
	}
	gates := &turnstileGates{byEndpoint: make(map[string]*turnstileGate, len(endpoints))}
	for _, endpoint := range endpoints {
		gates.byEndpoint[endpoint] = &turnstileGate{middleware: auth.TurnstileMiddleware(
			auth.TurnstileMiddlewareConfig{
				Endpoint:   endpoint,
				Settings:   settings,
				VerifyURL:  verifyURL,
				HTTPClient: client,
				Redis:      rdb,
				Logger:     log,
			})}
	}
	return gates
}

// check 运行一次人机校验；返回 false 表示响应已由中间件写出（403/400/503），
// 调用方必须立即 return。nil gate 视为未启用（M1 允许未配置 Turnstile）。
func (g *turnstileGates) check(endpoint string, w http.ResponseWriter,
	r *http.Request, token string) bool {

	if g == nil {
		return true
	}
	gate := g.byEndpoint[endpoint]
	if gate == nil {
		return true
	}
	clone := r.Clone(r.Context())
	query := clone.URL.Query()
	query.Set(auth.TurnstileTokenParam, token)
	clone.URL.RawQuery = query.Encode()
	// handler 已消费 Body；重放一个空 Body，避免 gate 二次读取造成阻塞。
	clone.Body = http.NoBody
	clone.ContentLength = 0

	allowed := false
	gate.middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		allowed = true
	})).ServeHTTP(w, clone)
	return allowed
}

// checkTurnstile 是 authHandlers 的薄封装。
func (h *authHandlers) checkTurnstile(w http.ResponseWriter, r *http.Request,
	endpoint, token string) bool {

	return h.turnstile.check(endpoint, w, r, token)
}
