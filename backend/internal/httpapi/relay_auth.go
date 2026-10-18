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

// relayErrorEnvelope 是 /v1 路径唯一的错误形状（不得使用 RFC7807）。
// code 字段保留但允许空串：令牌/数据库类错误按 §3.6 不传 code。
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
			raw, ok := relayCredentialFromRequest(r)
			if !ok {
				writeRelayError(w, http.StatusUnauthorized, "", "", "invalid token")
				return
			}
			key := normalizeRelayTokenKey(raw)
			if key == "" || tokenStore == nil {
				writeRelayError(w, http.StatusUnauthorized, "", "", "invalid token")
				return
			}
			// 兼容载体归一化：relay.BuildRelayContext 只重新解析 Authorization /
			// x-api-key 两种规范载体（relay-01 §3.1）。?key= / x-goog-api-key
			// 在这里收敛进 Authorization，保证鉴权与组装两次解析指向同一条令牌。
			// 查询参数绝不能进入日志/审计（§3.1 的日志纪律）。
			r.Header.Set("Authorization", "Bearer "+raw)

			meta, err := tokenStore.Resolve(r.Context(), auth.HashTokenKey(key))
			if err != nil || !tokenUsable(meta, time.Now()) {
				writeRelayError(w, http.StatusUnauthorized, "", "", "invalid token")
				return
			}
			if access == nil {
				// 无法回源用户状态时 fail-closed：不能把零值 Role 当普通用户放行。
				writeRelayError(w, http.StatusInternalServerError, "", "database_error", "user access unavailable")
				return
			}
			role, _, status, err := access.UserAccess(r.Context(), meta.UserID)
			if err != nil {
				// 已删除用户 → 401（身份失效），与坏令牌同语义；其余存储故障 500。
				if errors.Is(err, store.ErrUserNotFound) {
					writeRelayError(w, http.StatusUnauthorized, "", "", "invalid token")
					return
				}
				// 与 relay-01 §2 阶段 1 的 500 database_error 对齐；不暴露存储错误文本。
				writeRelayError(w, http.StatusInternalServerError, "", "database_error", "user access unavailable")
				return
			}
			if status != "enabled" {
				// §3.4：用户被封禁 → 403 auth_user_banned。
				writeRelayError(w, http.StatusForbidden, "", "auth_user_banned", "user is not enabled")
				return
			}
			// §3.4 IP 白名单：allow_ips 非空时客户端 IP 必须在列表中。
			// 解析失败/不在列表一律 403 access_denied（不区分原因）。
			if !tokenIPAllowed(r, meta.AllowIPs, trustedProxies) {
				writeRelayError(w, http.StatusForbidden, "", "access_denied", "client IP is not allowed for this token")
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
//   - x-api-key：仅 /v1/messages 或模型路径生效。
//   - x-goog-api-key：同上（Gemini 兼容载体；M2 端点族只接 chat/models，
//     但 messages 路径的载体规则在这里一并固化，供 relay-04 复用）。
//   - 查询参数 key：仅模型路径（/v1/models、/v1beta/models*、
//     /v1beta/openai/models*）生效；这是兼容载体，进入日志/埋点前必须剥离
//     （本函数不写日志，调用方也不得记录原始查询串）。
func relayCredentialFromRequest(r *http.Request) (string, bool) {
	if v := strings.TrimSpace(r.Header.Get("Authorization")); v != "" {
		if i := strings.IndexByte(v, ' '); i >= 0 {
			if strings.EqualFold(v[:i], "Bearer") {
				if raw := strings.TrimSpace(v[i+1:]); raw != "" {
					return raw, true
				}
			}
		} else {
			return v, true
		}
	}

	path := ""
	if r.URL != nil {
		path = r.URL.Path
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
