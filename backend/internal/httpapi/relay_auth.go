package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"newtg/gateway/internal/auth"
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
//
// 401 的 code 为空串（§3.6：令牌/数据库类错误不传 code）；存储故障同样 401，
// 因为区分「Redis/DB 故障」与「坏令牌」会泄露令牌是否存在（security-01 I12
// fail-closed 的同一姿势）。用户状态查询失败则是 500 database_error：它发生在
// 令牌已确认之后，不构成令牌探测信道。
func RelayAuth(tokenStore auth.TokenLookup, access UserAccessStore) func(http.Handler) http.Handler {
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
				// 与 relay-01 §2 阶段 1 的 500 database_error 对齐；不暴露存储错误文本。
				writeRelayError(w, http.StatusInternalServerError, "", "database_error", "user access unavailable")
				return
			}
			if status != "enabled" {
				// §3.4：用户被封禁 → 403 auth_user_banned。
				writeRelayError(w, http.StatusForbidden, "", "auth_user_banned", "user is not enabled")
				return
			}

			tokenID := meta.ID
			p := &auth.Principal{UserID: meta.UserID, TokenID: &tokenID, Role: role}
			next.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), p)))
		})
	}
}

// relayCredentialFromRequest 提取中继请求的令牌明文（relay-01 §3.1 的规范载体）。
//
// 规则与 relay.context 的 credentialFromRequest 逐字一致：
//   - Authorization 头永远生效：剥离 Bearer 前缀；无空格的裸 key 直用；
//     Basic/自定义方案（带空格但非 Bearer）不构成令牌凭据，继续尝试兼容载体。
//   - x-api-key 只在路径含 /v1/messages 或 /v1/models 时生效。
//
// ?key= / x-goog-api-key / mj-api-secret / WS 子协议等兼容载体属于 relay-04/07
// 的端点族，本阶段只接 chat/completions 与 models。
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
	if strings.Contains(path, "/v1/messages") || strings.Contains(path, "/v1/models") {
		if v := strings.TrimSpace(r.Header.Get("x-api-key")); v != "" {
			return v, true
		}
	}
	return "", false
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
