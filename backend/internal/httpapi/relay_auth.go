package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/store"
)

// 本文件实现 /v1 中继路径的令牌鉴权中间件（relay-01 §3 / §3.6）。
//
// 与 SessionAuth 的分工：
//   - SessionAuth 服务 /api/* 管理面（Cookie 会话或 nkt- Bearer，注入的角色来自会话）；
//   - RelayAuth 只服务 /v1/* 中继面：令牌是唯一身份来源，角色/状态必须回源
//     users 表（令牌本身没有角色语义，relay-01 §3.4 的「用户被封禁」判定）。
//
// 凭证载体（relay-01 §3.1）：Authorization 头总是生效；x-api-key 仅在
// /v1/messages 与 /v1/models 路径作为兼容载体生效。归一化规则必须与
// relay.BuildRelayContext 内部的 credentialFromRequest/normalizeTokenCredential
// 完全一致——否则鉴权阶段查到的令牌与组装阶段重新解析的 key 不同，
// BuildRelayContext 会以 ErrTokenInvalid 失败（两次解析必须指向同一行）。

// relayErrorType 是 relay-01 §3.6 冻结的 OpenAI 兼容错误信封 type 字面量。
const relayErrorType = "new_api_error"

// relayErrorEnvelope 是 OpenAI 形端点的错误形状（不得使用 RFC7807）。
// code 字段保留但允许空串：令牌/数据库类错误按 §3.6 不传 code。
//
// R-W18-04：Anthropic Messages 族（/v1/messages、/v1/messages/count_tokens）
// 不使用本形状，改用 claudeErrorEnvelope；两种信封在同一端点内不得混用。
type relayErrorEnvelope struct {
	Error relayErrorBody `json:"error"`
}

type relayErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

// newRelayRequestID 生成与 relay.BuildRelayContext 同格式的请求 ID：
// 16 字节 crypto/rand 的小写十六进制（128 位熵，不可预测）。
//
// 鉴权/路由前置失败时还没有 RelayContext，仍需要一个 requestId 供客户端报障
// 与日志串联（relay-01 §3.6 的 requestId 包装）。
func newRelayRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 熵源异常时不能退化成可预测 ID；返回空串，调用方仍会写 X-Request-ID
		// 之外的固定错误体（宁可缺 ID 也不伪造一个可碰撞的值）。
		return ""
	}
	return hex.EncodeToString(b[:])
}

// writeRelayError 输出 OpenAI 兼容错误信封（relay-01 §3.6）。
//
// GC-6：message 必须由调用方给出**固定文案**；绝不放上游 URL、凭据、SQL 文本
// 或适配器原始错误。requestId 统一追加到 message（§3.6 的 SetMessage 语义），
// 同时写 X-Request-ID 头，兼容只读头的客户端。
func writeRelayError(w http.ResponseWriter, status int, requestID, code, message string) {
	if requestID == "" {
		requestID = newRelayRequestID()
	}
	if requestID != "" {
		w.Header().Set("X-Request-ID", requestID)
		message = message + " (requestId: " + requestID + ")"
	}
	// /v1 永远 application/json；RFC7807 只属于 /api/*（relay-01 §3.6）。
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(relayErrorEnvelope{Error: relayErrorBody{
		Message: message,
		Type:    relayErrorType,
		Code:    code,
	}})
}

// relayErrorWriter 是「按入站协议写出错误」的统一签名。
//
// 抽出函数类型是为了让同一段校验代码（RelayAuth、限流拒绝、敏感词拦截）
// 不按协议复制：调用方只在**入口**决定一次写出器，之后每个错误出口都用它。
type relayErrorWriter func(w http.ResponseWriter, status int, requestID, code, message string)

// writeRelayErrorForRequest 按请求路径选择错误信封（R-W18-04）。
//
// 为什么按路径而不是按端点 handler 选：鉴权中间件在 handler 之前执行，
// 它手里只有 *http.Request；若让中间件固定写 OpenAI 形，/v1/messages 的
// 401/403/500 就会与 handler 内的 Claude 形混用——这正是裁决禁止的
// 「同一端点内形状混用」。
//
// 覆盖范围只限 Anthropic Messages 路径（含 /v1/messages/count_tokens）；
// 其它 /v1 端点（chat 等）保持 OpenAI 形，不得回归。
func writeRelayErrorForRequest(r *http.Request) relayErrorWriter {
	if r != nil && r.URL != nil && isAnthropicMessagesPath(r.URL.Path) {
		return writeClaudeError
	}
	return writeRelayError
}

// isAnthropicMessagesPath 报告路径是否属于 Claude 原生端点族。
//
// 前缀匹配覆盖 count_tokens（/v1/messages/count_tokens）；精确匹配覆盖
// 无尾斜杠的 messages 本体。其它以 /v1/messages 开头的形态（如未来子端点）
// 同属 Anthropic 协议面，必须使用 Claude 信封。
func isAnthropicMessagesPath(path string) bool {
	return path == "/v1/messages" || strings.HasPrefix(path, "/v1/messages/")
}

// Claude 错误信封的类型字面量（Anthropic Messages API 公开规范）。
//
// 顶层固定 `{"type":"error","error":{...}}`：Claude SDK 按这个形状解析，
// 收到 OpenAI 的 `{"error":{...,"code":...}}` 会当成未知结构。
const (
	claudeErrorTopLevel = "error"

	claudeErrInvalidRequest  = "invalid_request_error"
	claudeErrAuthentication  = "authentication_error"
	claudeErrPermission      = "permission_error"
	claudeErrNotFound        = "not_found_error"
	claudeErrRequestTooLarge = "request_too_large"
	claudeErrRateLimit       = "rate_limit_error"
	claudeErrAPI             = "api_error"
)

// claudeErrorEnvelope 是 /v1/messages 族的唯一错误形状（R-W18-04）。
//
// 刻意**没有** code 字段：Claude 客户端只认 type/message，混入 OpenAI 的
// code 会让两种信封在同一响应里并存。机器可读语义由 HTTP 状态码 +
// error.type 共同表达（relay-01 §3.6 的「入站协议本地信封」）。
type claudeErrorEnvelope struct {
	Type  string          `json:"type"`
	Error claudeErrorBody `json:"error"`
}

type claudeErrorBody struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// claudeErrorTypeForStatus 把 HTTP 状态码映射为 Claude 的 error.type。
//
// 映射依据 R-W18-04：
//   - 400/422 → invalid_request_error；
//   - 401 → authentication_error；403 → permission_error；
//   - 404 → not_found_error；413 → request_too_large；
//   - 429 → rate_limit_error；
//   - 402（余额/额度不足）Anthropic 没有对应类型，冻结为
//     invalid_request_error + 固定 message（"insufficient quota"），
//     状态码保持 402 不降级——客户端仍能按状态码区分「该充值」；
//   - 其余 5xx → api_error（503 同样用 api_error：Claude 的
//     overloaded_error 只用于上游过载，网关没有该语义）。
//
// 未知 4xx 落到 invalid_request_error：宁可让客户端按参数错误处理，
// 也不要给一个它不认识的 type。
func claudeErrorTypeForStatus(status int) string {
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return claudeErrInvalidRequest
	case http.StatusUnauthorized:
		return claudeErrAuthentication
	case http.StatusPaymentRequired:
		return claudeErrInvalidRequest
	case http.StatusForbidden:
		return claudeErrPermission
	case http.StatusNotFound:
		return claudeErrNotFound
	case http.StatusRequestEntityTooLarge:
		return claudeErrRequestTooLarge
	case http.StatusTooManyRequests:
		return claudeErrRateLimit
	}
	if status >= 500 {
		return claudeErrAPI
	}
	if status >= 400 {
		return claudeErrInvalidRequest
	}
	return claudeErrAPI
}

// writeClaudeError 输出 Claude 形错误信封（R-W18-04 / relay-01 §3.6）。
//
// 与 writeRelayError 保持一致的三条语义（裁决要求不得改变）：
//   - 状态码原样写出（429 的 Retry-After 由调用方在此之前设置）；
//   - requestId 追加到 message 末尾，同时写 X-Request-ID 头；
//   - Content-Type 恒为 application/json。
//
// code 参数保留是为了与 relayErrorWriter 签名统一，但**不写入响应**：
// Claude 信封没有 code 槽位；调用方的 code 仍用于日志/诊断（本函数不记日志）。
func writeClaudeError(w http.ResponseWriter, status int, requestID, _ string, message string) {
	if requestID == "" {
		requestID = newRelayRequestID()
	}
	if requestID != "" {
		w.Header().Set("X-Request-ID", requestID)
		message = message + " (requestId: " + requestID + ")"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(claudeErrorEnvelope{
		Type: claudeErrorTopLevel,
		Error: claudeErrorBody{
			Type:    claudeErrorTypeForStatus(status),
			Message: message,
		},
	})
}

// PlaygroundSessionAuth 是 /pg/* 的会话鉴权中间件（relay-01 §1.5）。
//
// 与 SessionAuth 的差别：
//   - 只接受浏览器会话 Cookie，**拒绝任何 Authorization 头**——即使同时携带
//     有效会话也拒绝。参照侧「登录会话可调、access token 不可调」是 Playground
//     的冻结契约（TC-A-26）；如果允许令牌与会话并存，调用来源就无法在审计/
//     计费归因上唯一确定（token_id 必须为 NULL）。
//   - 失败形状用 OpenAI 错误信封（/pg 是协议面，不是管理面）。401 不带 code，
//     与 /v1 的令牌错误同口径；Bearer 场景用 403 + session_required 明确告知
//     调用方换用会话 Cookie。
//
// Touch 与 SessionAuth 一致：Playground 是用户活跃操作，应当滑动续期。
func PlaygroundSessionAuth(sessions *auth.SessionManager) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// 先拒绝令牌载体：任何非空 Authorization 都不属于会话平面。
			// 不解析 key 形态（Bearer/裸 key 都拒绝），避免成为「这个令牌是否
			// 存在」的探测信道。
			if strings.TrimSpace(r.Header.Get("Authorization")) != "" {
				writeRelayError(w, http.StatusForbidden, "", "session_required",
					"the playground only accepts a browser session, not an API token")
				return
			}
			if sessions == nil {
				writeRelayError(w, http.StatusInternalServerError, "", "internal_error", "session store is unavailable")
				return
			}
			sid, ok := sessionID(r)
			if !ok || strings.TrimSpace(sid) == "" {
				writeRelayError(w, http.StatusUnauthorized, "", "", "invalid session")
				return
			}
			sess, err := sessions.Get(r.Context(), sid)
			if err != nil || sess == nil || sess.UserID == 0 {
				writeRelayError(w, http.StatusUnauthorized, "", "", "invalid session")
				return
			}
			// 强制改密的会话不得使用 Playground：与 SessionAuth 的 fail-closed
			// 语义一致（无法确认受限状态时也拒绝）。
			restricted, err := sessions.PasswordChangeRequired(r.Context(), sess.UserID)
			if err != nil {
				writeRelayError(w, http.StatusUnauthorized, "", "", "invalid session")
				return
			}
			if restricted {
				writeRelayError(w, http.StatusForbidden, "", "password_change_required",
					"password change is required before using the playground")
				return
			}
			if err := sessions.Touch(r.Context(), sid); err != nil {
				// 会话可能在 Get 与 Touch 之间被吊销；状态无法确认时 fail-closed。
				writeRelayError(w, http.StatusUnauthorized, "", "", "invalid session")
				return
			}
			// TokenID 保持 nil：这是「会话身份」的唯一标记，计费侧据此跳过
			// 令牌额度并让 usage_logs.token_id 落 NULL。
			p := &auth.Principal{UserID: sess.UserID, Role: sess.AuthLevel}
			next.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), p)))
		})
	}
}

// RelayAuth 是 /v1 中继路径的令牌鉴权中间件。
//
// 判定顺序（relay-01 §3.2–§3.4，顺序即语义）：
//  1. 提取凭证载体；缺失/形态非法 → 401（不区分原因，避免成为令牌存在性探测信道）。
//  2. 归一化 key 后先 SHA-256 再查存储（明文绝不入 SQL/日志）。
//  3. 令牌状态（enabled / 未过期 / 额度）→ 不合格 401。
//  4. 用户状态（users.status）→ 非 enabled 403 auth_user_banned。
//  5. 令牌 IP 白名单（allow_ips）→ 不匹配 403 access_denied（finding 6）。
//
// 401 的 code 为空串（§3.6：令牌/数据库类错误不传 code）；存储故障同样 401，
// 因为区分「Redis/DB 故障」与「坏令牌」会泄露令牌是否存在（security-01 I12
// fail-closed 的同一姿势）。用户状态查询失败则是 500 database_error：它发生在
// 令牌已确认之后，不构成令牌探测信道；用户已被删除（ErrUserNotFound）是身份
// 失效，映射 401（finding 9），不再塌缩成 500。
//
// trustedProxies 是 TRUSTED_PROXIES 配置解析后的前缀集合。M2 客户端 IP 策略：
//   - 默认只信 RemoteAddr（直连地址）；
//   - 仅当 TRUSTED_PROXIES 非空且直连对端 ∈ 白名单时，才解析 X-Forwarded-For，
//     并从右向左跳过受信代理取第一个非受信地址（标准代理链语义）。
//     未配置时 XFF 完全被忽略——否则任意客户端可伪造白名单 IP。
func RelayAuth(tokenStore auth.TokenLookup, access UserAccessStore, trustedProxies []netip.Prefix) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// R-W18-04：鉴权失败发生在 handler 之前，写出器必须在这里就按
			// 入站协议选定——否则 /v1/messages 的 401/403/500 会退回 OpenAI 形。
			writeErr := writeRelayErrorForRequest(r)
			raw, ok := relayCredentialFromRequest(r)
			if !ok {
				writeErr(w, http.StatusUnauthorized, "", "", "invalid token")
				return
			}
			key := normalizeRelayTokenKey(raw)
			if key == "" || tokenStore == nil {
				writeErr(w, http.StatusUnauthorized, "", "", "invalid token")
				return
			}
			// 兼容载体归一化：relay.BuildRelayContext 只重新解析 Authorization /
			// x-api-key 两种规范载体（relay-01 §3.1）。?key= / x-goog-api-key
			// 在这里收敛进 Authorization，保证鉴权与组装两次解析指向同一条令牌。
			// 查询参数绝不能进入日志/审计（§3.1 的日志纪律）。
			r.Header.Set("Authorization", "Bearer "+raw)

			meta, err := tokenStore.Resolve(r.Context(), auth.HashTokenKey(key))
			if err != nil || !tokenUsable(meta, time.Now()) {
				writeErr(w, http.StatusUnauthorized, "", "", "invalid token")
				return
			}
			if access == nil {
				// 无法回源用户状态时 fail-closed：不能把零值 Role 当普通用户放行。
				writeErr(w, http.StatusInternalServerError, "", "database_error", "user access unavailable")
				return
			}
			role, _, status, err := access.UserAccess(r.Context(), meta.UserID)
			if err != nil {
				// 已删除用户 → 401（身份失效），与坏令牌同语义；其余存储故障 500。
				if errors.Is(err, store.ErrUserNotFound) {
					writeErr(w, http.StatusUnauthorized, "", "", "invalid token")
					return
				}
				// 与 relay-01 §2 阶段 1 的 500 database_error 对齐；不暴露存储错误文本。
				writeErr(w, http.StatusInternalServerError, "", "database_error", "user access unavailable")
				return
			}
			if status != "enabled" {
				// §3.4：用户被封禁 → 403 auth_user_banned。
				writeErr(w, http.StatusForbidden, "", "auth_user_banned", "user is not enabled")
				return
			}
			// §3.4 IP 白名单：allow_ips 非空时客户端 IP 必须在列表中。
			// 解析失败/不在列表一律 403 access_denied（不区分原因）。
			if !tokenIPAllowed(r, meta.AllowIPs, trustedProxies) {
				writeErr(w, http.StatusForbidden, "", "access_denied", "client IP is not allowed for this token")
				return
			}

			tokenID := meta.ID
			p := &auth.Principal{
				UserID:      meta.UserID,
				TokenID:     &tokenID,
				Role:        role,
				ModelLimits: append([]string(nil), meta.ModelLimits...),
			}
			next.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), p)))
		})
	}
}

// relayCredentialFromRequest 提取中继请求的令牌明文（relay-01 §3.1 的载体表）。
//
// 优先级（顺序即语义）：
//   - Authorization 头总是第一优先，且总是生效：剥离 Bearer 前缀；无空格的
//     裸 key 直用；Basic/自定义方案（带空格但非 Bearer）不构成令牌凭据，
//     继续尝试兼容载体。
//   - mj-api-secret：仅 MJ 路径（`/mj/*` 或 `/<seg>/mj/*`）且 Authorization
//     key 为空或等于哨兵串 `midjourney-proxy` 时生效（relay-01 §3.1）。
//     这是 midjourney-proxy 生态客户端的既有载体；哨兵串本身不是令牌。
//   - x-api-key：仅 /v1/messages 或模型路径生效。
//   - x-goog-api-key：同上（Gemini 兼容载体；M2 端点族只接 chat/models，
//     但 messages 路径的载体规则在这里一并固化，供 relay-04 复用）。
//   - 查询参数 key：仅模型路径（/v1/models、/v1beta/models*、
//     /v1beta/openai/models*）生效；这是兼容载体，进入日志/埋点前必须剥离
//     （本函数不写日志，调用方也不得记录原始查询串）。
func relayCredentialFromRequest(r *http.Request) (string, bool) {
	path := ""
	if r.URL != nil {
		path = r.URL.Path
	}

	authRaw, authOK := authorizationCredential(r)
	if authOK && !(isMJPath(path) && authRaw == mjProxySentinel) {
		return authRaw, true
	}
	// MJ 客户端兼容载体：哨兵 Authorization 或完全缺失时才看该头。
	// 非 MJ 路径一律忽略——否则普通 /v1 调用也能用这个头绕过 Bearer 语义。
	if isMJPath(path) {
		if v := strings.TrimSpace(r.Header.Get(mjAPISecretHeader)); v != "" {
			return v, true
		}
	}
	if authOK {
		// 哨兵串在没有 mj-api-secret 时仍是「无效 key」而不是「无凭据」：
		// 让下游统一走 401 invalid token，不把哨兵当成合法令牌去查库。
		return authRaw, true
	}

	if isMessagesOrModelsPath(path) {
		if v := strings.TrimSpace(r.Header.Get("x-api-key")); v != "" {
			return v, true
		}
		if v := strings.TrimSpace(r.Header.Get("x-goog-api-key")); v != "" {
			return v, true
		}
	}
	if isModelsPath(path) {
		if r.URL != nil {
			if v := strings.TrimSpace(r.URL.Query().Get("key")); v != "" {
				return v, true
			}
		}
	}
	return "", false
}

// authorizationCredential 只解析 Authorization 头（Bearer/裸 key）。
//
// 抽出来供 MJ 载体判定复用：需要先知道 Authorization 是否构成 key、以及它
// 是否只是哨兵串，才能决定是否回落到 mj-api-secret。
func authorizationCredential(r *http.Request) (string, bool) {
	v := strings.TrimSpace(r.Header.Get("Authorization"))
	if v == "" {
		return "", false
	}
	if i := strings.IndexByte(v, ' '); i >= 0 {
		if strings.EqualFold(v[:i], "Bearer") {
			if raw := strings.TrimSpace(v[i+1:]); raw != "" {
				return raw, true
			}
		}
		return "", false
	}
	return v, true
}

const (
	// mjAPISecretHeader 是 MJ 客户端可用的令牌载体（relay-01 §3.1）。
	mjAPISecretHeader = "mj-api-secret"
	// mjProxySentinel 是 midjourney-proxy 客户端在 Authorization 里放置的
	// 占位串：真实的网关令牌在同一请求的 mj-api-secret 头里。
	mjProxySentinel = "midjourney-proxy"
)

// isMJPath 报告路径是否属于 MJ 端点族。
//
// 生产路径在 mux 之前已把 `/<seg>/mj/*` 重写为 `/mj/*`；这里同时识别别名
// 形态，使 handler 单测/未来绕过重写的装配也能正确选择载体。保留首段
// （api/v1/pg/healthz/readyz）下的 `/mj` 不是 MJ 族——它们是别的处理面，
// 不能因为第二段叫 mj 就接受 MJ 的令牌载体。
func isMJPath(path string) bool {
	segments := strings.Split(strings.Trim(path, "/"), "/")
	if len(segments) == 0 || segments[0] == "" {
		return false
	}
	if segments[0] == "mj" {
		return true
	}
	if len(segments) >= 2 && segments[1] == "mj" {
		switch segments[0] {
		case "api", "v1", "pg", "healthz", "readyz":
			return false
		}
		return true
	}
	return false
}

// isMessagesOrModelsPath 覆盖 x-api-key / x-goog-api-key 的生效路径
// （relay-01 §3.1：路径含 /v1/messages 或 /v1/models）。
func isMessagesOrModelsPath(path string) bool {
	return strings.Contains(path, "/v1/messages") || isModelsPath(path)
}

// isModelsPath 覆盖查询参数 key 的生效路径：
// /v1/models、/v1beta/models*、/v1beta/openai/models*。
func isModelsPath(path string) bool {
	return strings.Contains(path, "/v1/models") ||
		strings.HasPrefix(path, "/v1beta/models") ||
		strings.HasPrefix(path, "/v1beta/openai/models")
}

// parseTrustedProxies 解析 TRUSTED_PROXIES（逗号分隔的 IP 或 CIDR）。
//
// 非法条目被忽略而不是放宽：宁可少信任一个代理（退回 RemoteAddr），
// 也不把无法解析的字符串当成「信任所有」。空配置返回 nil。
func parseTrustedProxies(raw string) []netip.Prefix {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	out := make([]netip.Prefix, 0, 4)
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if prefix, err := netip.ParsePrefix(part); err == nil {
			out = append(out, prefix.Masked())
			continue
		}
		if addr, err := netip.ParseAddr(part); err == nil {
			out = append(out, netip.PrefixFrom(addr.Unmap(), addr.Unmap().BitLen()))
		}
	}
	return out
}

// tokenIPAllowed 判定客户端 IP 是否满足令牌的 allow_ips 白名单。
//
// 空白名单 = 不限（platform-01 §2：NULL = 不限）。白名单元素支持单 IP 与 CIDR；
// 无法解析的条目被忽略（fail-closed：不给它放行任何地址）。客户端地址无法解析
// 或不在列表中一律返回 false（relay-01 §3.4 的 403）。
func tokenIPAllowed(r *http.Request, allowIPs []string, trustedProxies []netip.Prefix) bool {
	if len(allowIPs) == 0 {
		return true
	}
	client, ok := requestClientAddr(r, trustedProxies)
	if !ok {
		return false
	}
	client = client.Unmap()
	for _, entry := range allowIPs {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if prefix, err := netip.ParsePrefix(entry); err == nil {
			if prefix.Masked().Contains(client) {
				return true
			}
			continue
		}
		if addr, err := netip.ParseAddr(entry); err == nil && addr.Unmap() == client {
			return true
		}
	}
	return false
}

// requestClientAddr 解析请求的客户端 IP（M2 策略见 RelayAuth 注释）：
// 默认 RemoteAddr；仅当直连对端属于 trustedProxies 时才读取 X-Forwarded-For，
// 并从右向左跳过受信代理。任何一跳解析失败都返回 ok=false（fail-closed）。
func requestClientAddr(r *http.Request, trustedProxies []netip.Prefix) (netip.Addr, bool) {
	peer, ok := parseAddrPort(r.RemoteAddr)
	if !ok {
		return netip.Addr{}, false
	}
	if len(trustedProxies) == 0 || !addrInPrefixes(peer, trustedProxies) {
		return peer, true
	}
	hops := forwardedForHops(r)
	for i := len(hops) - 1; i >= 0; i-- {
		addr, err := netip.ParseAddr(hops[i])
		if err != nil {
			return netip.Addr{}, false
		}
		addr = addr.Unmap()
		if !addrInPrefixes(addr, trustedProxies) {
			return addr, true
		}
	}
	if len(hops) > 0 {
		// 全部跳都是受信代理：最左一跳即最初的客户端。
		addr, err := netip.ParseAddr(hops[0])
		if err != nil {
			return netip.Addr{}, false
		}
		return addr.Unmap(), true
	}
	return peer, true
}

// forwardedForHops 展开全部 X-Forwarded-For 头（多值头 + 逗号分隔）。
func forwardedForHops(r *http.Request) []string {
	values := r.Header.Values("X-Forwarded-For")
	hops := make([]string, 0, len(values))
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			if part = strings.TrimSpace(part); part != "" {
				hops = append(hops, part)
			}
		}
	}
	return hops
}

// parseAddrPort 解析 RemoteAddr（host:port）；裸 IP 也接受（测试/Unix socket）。
func parseAddrPort(raw string) (netip.Addr, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return netip.Addr{}, false
	}
	if addrPort, err := netip.ParseAddrPort(raw); err == nil {
		return addrPort.Addr().Unmap(), true
	}
	if addr, err := netip.ParseAddr(raw); err == nil {
		return addr.Unmap(), true
	}
	return netip.Addr{}, false
}

func addrInPrefixes(addr netip.Addr, prefixes []netip.Prefix) bool {
	addr = addr.Unmap()
	for _, prefix := range prefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// normalizeRelayTokenKey 实现 relay-01 §3.2 的令牌 key 归一化：
//
//  1. nkt-<48> 整段不切分（48 位 Base62 body 不含 '-'）；
//  2. legacy sk-<key>[-<channelId>] 按首个 '-' 截断得到查询 key；
//     末段纯数字时表示指定渠道（pin），从查询 key 中剥离；
//  3. 裸 key 直用。
//
// 必须与 relay 包的同名（未导出）逻辑保持一致：鉴权阶段与
// BuildRelayContext 的重解析必须命中同一 key_hash。
func normalizeRelayTokenKey(raw string) string {
	switch {
	case strings.HasPrefix(raw, "nkt-"):
		return raw
	case strings.HasPrefix(raw, "sk-"):
		body := strings.TrimPrefix(raw, "sk-")
		if body == "" {
			return raw
		}
		parts := strings.Split(body, "-")
		return "sk-" + parts[0]
	default:
		return raw
	}
}
