// Package httpapi 装配管理面 HTTP 处理链（路由、handler、鉴权/CSRF 中间件）。
package httpapi

import (
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"newtg/gateway/internal/api/dto"
	"newtg/gateway/internal/auth"
)

// TokenLookup 是 auth.TokenLookup 的别名，使本包签名与 Task 4 契约逐字一致；
// 类型本身仍定义在 auth，避免 tokenstore → httpapi 的 import cycle。
type TokenLookup = auth.TokenLookup

// RFC 7807 type URI 采用全库既有的 https://newtg.dev/problems/<slug> 约定
// （modules-03 的 insufficient-balance、modules-09 的 model-not-found 同口径）。
// unauthorized 未在设计里冻结 slug，故取最中性的名字；前端只依赖 401 状态码。
const (
	problemTypeUnauthorized = "https://newtg.dev/problems/unauthorized"
	problemTypeCSRFFailed   = "https://newtg.dev/problems/csrf_failed"
	// password_change_required：临时口令（must_change_password=true）登录后的
	// 受限会话访问白名单外端点。前端据此强制跳转改密页（modules-01 F-01-03）。
	problemTypePasswordChangeRequired = "https://newtg.dev/problems/password_change_required"
)

// SessionAuth 是 Cookie 会话 / Bearer 令牌双路径鉴权中间件。
//
// 双路径（security-01 §2.2、platform-03 §10.1）：
//   - `__Host-nkt_sess` Cookie：服务端不透明会话，Redis 是权威存储；
//   - `Authorization: Bearer nkt-…`：API 令牌，查询前先做 SHA-256。
//
// 失败一律返回同一个 401 problem+json，不区分「没带凭据 / Cookie 过期 / 令牌状态
// 非法 / Redis 故障」。原因有二：任何差异化都会成为凭据有效性与会话存在性的探测
// 信道；Redis 故障必须 fail-closed（security-01 I12、GC-7），绝不能降级为匿名。
//
// 信任边界（Role）：会话记录里的 auth_level 来自登录时写入的 SessionOpts.Role，
// 该值必须由 Task 6 从 users 表（可信 DB 行）读取；SessionOpts.Role 绝不可取自
// 请求体/请求头。本中间件只消费已签发的服务端会话，不重新信任任何客户端角色声明。
//
// 强制改密（must_change_password）：受限状态按用户记录在 Redis
// （auth.SessionManager.PasswordChangeRequired），而不是写进会话 hash。原因是
// 2FA challenge 等其它会话签发路径无需各自接线即可继承限制。Cookie 与 Bearer
// 两条路径都会检查（管理员重置口令只吊销会话、不吊销 nkt- 令牌），受限身份仅
// 放行登出/自读/改密/2FA 端点，其余一律 403 password_change_required；标记读取
// 失败时 fail-closed（401，与 Get/Touch 的 Redis 故障口径一致）。
func SessionAuth(m *auth.SessionManager, tokenStore TokenLookup) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Cookie 优先：只要浏览器带了会话 Cookie，就按 CSRF 受保护的 Cookie
			// 路径处理。若因同时带 Authorization 而切到 Bearer 路径，就等于给了
			// 一个绕过 CSRF 的旁路；宁可对混用请求 fail-closed。
			if sid, ok := sessionID(r); ok {
				sess, err := m.Get(r.Context(), sid)
				if err != nil {
					writeUnauthorized(w)
					return
				}
				restricted, err := m.PasswordChangeRequired(r.Context(), sess.UserID)
				if err != nil {
					// 无法确认受限状态时绝不放行（GC-7）。
					writeUnauthorized(w)
					return
				}
				if restricted && !passwordChangeAllowed(r) {
					// 在 Touch 之前拒绝：被拦截的请求不应延长会话寿命。
					writePasswordChangeRequired(w)
					return
				}
				// 滑动超时依赖每次活跃请求刷新 last_seen_at；不刷新则 7 天滑动窗
				// 会退化成「创建后 7 天必失效」（security-01 I3）。
				// Touch 失败（Redis 故障或 Get 与 Touch 之间被并发吊销）必须
				// fail-closed 返回 401：会话状态无法确认时绝不放行（GC-7）。
				if err := m.Touch(r.Context(), sid); err != nil {
					writeUnauthorized(w)
					return
				}
				p := &auth.Principal{UserID: sess.UserID, Role: sess.AuthLevel}
				next.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), p)))
				return
			}

			// Bearer 路径没有 Redis 会话可滑动，不调用 Touch：令牌状态由
			// Resolve 的权威存储判定（relay-01 §3.3）。
			raw, ok := bearerCredential(r)
			if !ok || tokenStore == nil {
				writeUnauthorized(w)
				return
			}
			meta, err := tokenStore.Resolve(r.Context(), auth.HashTokenKey(raw))
			if err != nil || !tokenUsable(meta, time.Now()) {
				writeUnauthorized(w)
				return
			}
			// 强制改密同样约束 Bearer 路径：管理员重置口令只吊销会话、不吊销
			// nkt- 令牌；若此处不检查，旧令牌仍可访问全部管理端点，违背
			// 「改密完成前不得访问其他受保护接口」（modules-01 F-01-03）。
			restricted, err := m.PasswordChangeRequired(r.Context(), meta.UserID)
			if err != nil {
				writeUnauthorized(w)
				return
			}
			if restricted && !passwordChangeAllowed(r) {
				writePasswordChangeRequired(w)
				return
			}
			// Role/Permissions 不从令牌推导：令牌没有角色语义，管理权限必须由
			// 会话身份 + RBAC 判定；TokenID 非 nil 让中继/审计区分调用来源。
			tokenID := meta.ID
			p := &auth.Principal{UserID: meta.UserID, TokenID: &tokenID}
			next.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), p)))
		})
	}
}

// CSRF 校验 Cookie 会话路径非安全方法的同步器令牌（security-01 §2.2）。
//
// 只在「请求携带会话 Cookie」时生效：
//   - Bearer 请求豁免——Authorization 头不能被跨站表单/简单请求携带，
//     不存在 CSRF 风险（security-01 §2.2、relay-01 §3.1 的凭证载体分层）；
//   - 无 Cookie 的未认证请求交给 SessionAuth 先返回 401，本中间件不越权改写其语义。
//
// Redis 不可用时 VerifyCSRF 返回 false（I12 fail-closed），统一落 403 csrf_failed，
// 与 design §2.8「CSRF 校验失败」的客户端处理（刷新令牌后重试一次）一致。
func CSRF(m *auth.SessionManager) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isSafeMethod(r.Method) {
				next.ServeHTTP(w, r)
				return
			}
			sid, ok := sessionID(r)
			if !ok {
				next.ServeHTTP(w, r)
				return
			}
			if !m.VerifyCSRF(r.Context(), sid, r.Header.Get(auth.CSRFHeaderName)) {
				writeCSRFFailed(w)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// OriginRefererCheck 是 CSRF 的纵深防御：Cookie 路径的非安全方法必须来自同源。
//
// 触发条件与 CSRF 相同（带会话 Cookie 的非安全方法）；Bearer/服务端客户端不带
// Cookie，不受此限制，否则会误伤正常的 API 自动化调用。
//
// 缺 Origin 与 Referer 时拒绝：浏览器对非安全请求必发 Origin（Fetch 规范），
// 缺失意味着请求不是浏览器发起的同源写操作，而它带着会话 Cookie——这正是需要
// fail-closed 的形态。允许清单来自 PUBLIC_BASE_URL（可为多个，如主机名分离部署）；
// 清单为空时退化为「与请求 Host 同源」，保证本地开发（PUBLIC_BASE_URL 缺省为空）
// 仍受保护。清单里的非法条目被忽略而非放行（fail-closed）。
func OriginRefererCheck(allowedOrigins []string) func(http.Handler) http.Handler {
	allowed := make(map[string]struct{}, len(allowedOrigins))
	for _, raw := range allowedOrigins {
		if origin, _, ok := normalizeOrigin(raw); ok {
			allowed[origin] = struct{}{}
		}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isSafeMethod(r.Method) || !hasSessionCookie(r) {
				next.ServeHTTP(w, r)
				return
			}

			// Origin 存在但非法时不得回退到 Referer：两者都可由客户端控制，
			// 用「坏 Origin + 好 Referer」回退等于给绕过留缝。
			raw := r.Header.Get("Origin")
			if raw == "" {
				raw = r.Header.Get("Referer")
			}
			origin, host, ok := normalizeOrigin(raw)
			if !ok {
				writeCSRFFailed(w)
				return
			}
			if len(allowed) > 0 {
				if _, ok := allowed[origin]; !ok {
					writeCSRFFailed(w)
					return
				}
			} else if host != normalizeRequestHost(r.Host) {
				writeCSRFFailed(w)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// tokenUsable 实现 relay-01 §3.3 的令牌状态判定。Resolve 可能已做同类校验；
// 这里重做一次是廉价的双保险，且让 TokenMeta 的字段语义有唯一消费点。
func tokenUsable(meta *auth.TokenMeta, now time.Time) bool {
	if meta == nil || meta.Status != "enabled" {
		return false
	}
	if meta.ExpiresAt != nil && !now.Before(*meta.ExpiresAt) {
		return false
	}
	return meta.UnlimitedQuota || meta.RemainQuota > 0
}

// sessionID 返回会话 Cookie 的值；不存在时 ok=false。
func sessionID(r *http.Request) (string, bool) {
	c, err := r.Cookie(auth.SessionCookieName)
	if err != nil {
		return "", false
	}
	return c.Value, true
}

// passwordChangeAllowed 是受限会话（must_change_password=true）的端点白名单。
//
// 只保留用户完成改密所必需的最小集合（modules-01 F-01-03「改密完成前不得访问
// 其他受保护页面/接口」）：
//   - POST /api/logout   退出账号（public-safe，正常不会经过本中间件，仍显式放行）；
//   - GET  /api/me       读取当前身份（账户页自读；改密状态由登录响应驱动）；
//   - GET/PUT /api/self* 账户自读与语言等设置；
//   - POST /api/password 改密本身；
//   - POST /api/2fa/{setup,enable,disable,backup-codes/regenerate}
//     改密前后管理二次验证因子（显式枚举，避免未来新增子路由被自动放行）。
//
// 方法也参与判定：例如 POST /api/me 不在白名单内，防止未来给同名路径加写操作
// 时被顺带放行。
func passwordChangeAllowed(r *http.Request) bool {
	path := r.URL.Path
	switch {
	case path == "/api/logout":
		return r.Method == http.MethodPost
	case path == "/api/me":
		return r.Method == http.MethodGet
	case path == "/api/self" || strings.HasPrefix(path, "/api/self/"):
		return r.Method == http.MethodGet || r.Method == http.MethodPut
	case path == "/api/password":
		return r.Method == http.MethodPost
	case r.Method == http.MethodPost &&
		(path == "/api/2fa/setup" || path == "/api/2fa/enable" ||
			path == "/api/2fa/disable" || path == "/api/2fa/backup-codes/regenerate"):
		return true
	default:
		return false
	}
}

func hasSessionCookie(r *http.Request) bool {
	_, ok := sessionID(r)
	return ok
}

// bearerCredential 提取 `Authorization: Bearer nkt-…` 的明文令牌。
//
// 管理面只接受 nkt- 形态：legacy sk- 是 relay 面的兼容载体（relay-01 §3.2），
// 不是管理 API 凭据。保留原始明文（含 nkt- 前缀）交给 HashTokenKey，
// 与 Task 12 的 key_hash=SHA256(plain) 口径一致。
func bearerCredential(r *http.Request) (string, bool) {
	parts := strings.SplitN(r.Header.Get("Authorization"), " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", false
	}
	raw := strings.TrimSpace(parts[1])
	if !strings.HasPrefix(raw, "nkt-") || len(raw) == len("nkt-") {
		return "", false
	}
	return raw, true
}

// isSafeMethod 按 RFC 9110 的安全方法集合判定（GET/HEAD/OPTIONS/TRACE）。
// 用白名单而不是只枚举 POST/PUT/PATCH/DELETE：未来出现自定义方法时默认按
// 非安全处理（fail-closed），不会因漏列而绕过 CSRF。
func isSafeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	default:
		return false
	}
}

// writeUnauthorized 是所有认证失败的唯一出口：统一文案，无路径/原因差异。
func writeUnauthorized(w http.ResponseWriter) {
	writeProblem(w, http.StatusUnauthorized, problemTypeUnauthorized, "Unauthorized")
}

// writeCSRFFailed 同时服务 CSRF 与 Origin 纵深检查：不向客户端暴露是哪一层拒绝，
// 前端只需按 security-08 §2.6 的统一处理重取令牌。
func writeCSRFFailed(w http.ResponseWriter) {
	writeProblem(w, http.StatusForbidden, problemTypeCSRFFailed, "CSRF validation failed")
}

// writePasswordChangeRequired 是受限会话的唯一拒绝出口：前端据此强制跳转
// /security 改密页，而不是把用户当成未登录或权限不足。
func writePasswordChangeRequired(w http.ResponseWriter) {
	writeProblem(w, http.StatusForbidden, problemTypePasswordChangeRequired, "Password change required")
}

// writeProblem 输出无 detail 的 RFC 7807 Problem（GC-6）。
func writeProblem(w http.ResponseWriter, status int, problemType, title string) {
	writeProblemDetail(w, status, problemType, title, "")
}

// writeProblemDetail 输出带可选 detail 的 RFC 7807 Problem。字段校验类错误
// 需要 detail 说明具体规则；共享同一出口保证 Content-Type 与状态码口径一致。
func writeProblemDetail(w http.ResponseWriter, status int, problemType, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	problem := dto.Problem{Type: problemType, Title: title, Status: status}
	if detail != "" {
		problem.Detail = &detail
	}
	// 响应已开始写出；编码失败（如客户端断开）无法再改状态码，忽略即可。
	_ = json.NewEncoder(w).Encode(problem)
}

// normalizeOrigin 把 Origin/Referer/允许清单归一化为 scheme://host。
// 返回的 host 单独供「无白名单时与请求 Host 比对」使用。
// 路径被忽略（Referer 天然带路径）；默认端口按 scheme 剥离，避免
// `https://a` 与 `https://a:443` 被误判为不同源。
func normalizeOrigin(raw string) (origin, host string, ok bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil {
		return "", "", false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", "", false
	}
	host = strings.ToLower(u.Host)
	if h, p, err := net.SplitHostPort(host); err == nil {
		switch {
		case scheme == "https" && p == "443":
			host = h
		case scheme == "http" && p == "80":
			host = h
		default:
			host = net.JoinHostPort(h, p)
		}
	}
	return scheme + "://" + host, host, true
}

// normalizeRequestHost 归一化请求 Host（scheme 未知，80/443 都视为默认端口）。
func normalizeRequestHost(hostport string) string {
	hostport = strings.ToLower(strings.TrimSpace(hostport))
	h, p, err := net.SplitHostPort(hostport)
	if err != nil {
		return hostport
	}
	if p == "80" || p == "443" {
		return h
	}
	return net.JoinHostPort(h, p)
}
