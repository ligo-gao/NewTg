package auth

// Turnstile 人机校验（F-01-26 / modules-01 §3.15）。
//
// 冻结口径：
//   - fail-closed：校验服务不可达、返回非 200、响应不可解析、重放守卫不可用，
//     一律拒绝，绝不因为「拿不准」放行（I-10）；
//   - 令牌一次性且绑定接入点：成功校验后把 sha256(token) 以 SET NX 写入 Redis，
//     同一 token 在任何接入点再次出现都拒绝（I-11）；
//   - siteverify 必须携带 remoteip，经内部 clientip 解析器取值（TRUSTED_PROXIES
//     为空时只取连接地址，不信任 X-Forwarded-For；配置 edge 网段时取真实客户端）；
//   - 5 个接入点名称冻结为常量，中间件顺序由装配方保证：限流在外、Turnstile 在内。
//
// 本中间件只依赖 net/http 与 Redis，不依赖 httpapi（避免 import cycle），
// 错误响应直接按全库 RFC 7807 约定写出。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/api/dto"
	"newtg/gateway/internal/clientip"
)

const (
	// TurnstileVerifyURL 是 Cloudflare 官方校验端点。
	TurnstileVerifyURL = "https://challenges.cloudflare.com/turnstile/v0/siteverify"
	// TurnstileTokenParam 是请求体/查询参数中的令牌字段名（modules-01 §3.15）。
	TurnstileTokenParam = "turnstile_token"
	// TurnstileTokenTTL 是重放守卫的保留时长：覆盖 Cloudflare 令牌 300s 有效期
	// 后仍留有余量，防止过期令牌被重新提交。
	TurnstileTokenTTL = 10 * time.Minute

	// 5 个冻结接入点（modules-01 §3.15 表格）。
	TurnstileEndpointVerification  = "verification"
	TurnstileEndpointResetPassword = "reset_password"
	TurnstileEndpointRegister      = "register"
	TurnstileEndpointLogin         = "login"
	TurnstileEndpointCheckin       = "checkin"

	turnstileReplayPrefix = "turnstile:used:"
	defaultTurnstileBody  = 1 << 20

	problemTurnstileFailed    = "https://newtg.dev/problems/turnstile_challenge_failed"
	problemServiceUnavailable = "https://newtg.dev/problems/service_unavailable"
	problemBadRequest         = "https://newtg.dev/problems/bad_request"
)

// TurnstileSettings 是 auth.turnstile.* 的运行时快照。
// SecretKey 由配置中心经 SecretSealer.Open 解封后传入；SiteKey 仍参与
// 「开启但未配置」的配置错误判定（modules-01 TC-F-01-26-04）。
type TurnstileSettings struct {
	Enabled   bool
	SiteKey   string
	SecretKey string
}

// TurnstileMiddlewareConfig 是装配输入。Settings 按请求调用，保证开关与
// 密钥轮换在 configcenter 广播后立即生效。
type TurnstileMiddlewareConfig struct {
	// Endpoint 是 5 个接入点常量之一；空值按 "unknown" 处理（仍绑定重放键）。
	Endpoint string
	// Settings 返回当前配置；nil 视为关闭。
	Settings func() TurnstileSettings
	// VerifyURL 覆盖 siteverify 端点（测试注入 httptest 桩）。
	VerifyURL string
	// HTTPClient 可选；默认 5s 超时的客户端（fail-closed 必须有界）。
	HTTPClient *http.Client
	// Redis 是重放守卫；开启时 nil 表示无法保证一次性，必须 fail-closed。
	Redis *redis.Client
	// Logger 可选；默认 slog.Default()。
	Logger *slog.Logger
	// MaxBodyBytes 是可缓存的 JSON 请求体上限（默认 1 MiB）。
	MaxBodyBytes int64
}

// TurnstileMiddleware 返回接入点专用的中间件。装配顺序（外层 → 内层）：
// 限流 → Turnstile → 业务 handler（modules-01 §3.15 表格）。
func TurnstileMiddleware(cfg TurnstileMiddlewareConfig) func(http.Handler) http.Handler {
	endpoint := strings.TrimSpace(cfg.Endpoint)
	if endpoint == "" {
		endpoint = "unknown"
	}
	verifyURL := strings.TrimSpace(cfg.VerifyURL)
	if verifyURL == "" {
		verifyURL = TurnstileVerifyURL
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	if cfg.Settings == nil {
		// 与 nil Redis 的 fail-closed 不同，缺少设置函数只能视为「未开启」；
		// 但这属于装配疏漏，构造时告警一次，避免静默失去人机校验。
		log.Warn("turnstile middleware constructed without settings provider; middleware disabled")
	}
	maxBody := cfg.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = defaultTurnstileBody
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			settings := TurnstileSettings{}
			if cfg.Settings != nil {
				settings = cfg.Settings()
			}
			// 关闭时防刷完全依赖外层限流；此处不得读取/消费请求体。
			if !settings.Enabled {
				next.ServeHTTP(w, r)
				return
			}

			// 配置错误不静默放行（TC-F-01-26-04）。site/secret 任一缺失都拒绝。
			if strings.TrimSpace(settings.SiteKey) == "" || strings.TrimSpace(settings.SecretKey) == "" {
				log.Error("turnstile enabled but site/secret key is missing",
					"endpoint", endpoint, "reason", "missing_configuration")
				writeTurnstileProblem(w, http.StatusForbidden, problemTurnstileFailed,
					"Human verification failed. Please try again.", "")
				return
			}

			token, err := turnstileTokenFromRequest(r, maxBody)
			if err != nil {
				writeTurnstileProblem(w, http.StatusBadRequest, problemBadRequest,
					"Invalid request body", "")
				return
			}
			if token == "" {
				log.Warn("turnstile token missing", "endpoint", endpoint, "reason", "missing_token")
				writeTurnstileProblem(w, http.StatusForbidden, problemTurnstileFailed,
					"Human verification failed. Please try again.", "")
				return
			}

			remoteIP := turnstileRemoteIP(r)
			// 先向 Cloudflare 校验，成功后再占用一次性名额：校验服务故障时
			// 不消耗用户的合法令牌，用户重试仍可用。
			ok, err := verifyTurnstileToken(r.Context(), client, verifyURL,
				settings.SecretKey, token, remoteIP)
			if err != nil {
				log.Error("turnstile verification unavailable",
					"endpoint", endpoint, "reason", "verify_unavailable", "error", err)
				writeTurnstileProblem(w, http.StatusServiceUnavailable, problemServiceUnavailable,
					"Human verification is temporarily unavailable", "5")
				return
			}
			if !ok {
				log.Warn("turnstile challenge failed", "endpoint", endpoint, "reason", "challenge_failed")
				writeTurnstileProblem(w, http.StatusForbidden, problemTurnstileFailed,
					"Human verification failed. Please try again.", "")
				return
			}

			claimed, err := claimTurnstileToken(r.Context(), cfg.Redis, token, endpoint, remoteIP)
			if err != nil {
				// 一次性属性无法落库时必须拒绝，不能退化成可重放（I-11）。
				log.Error("turnstile replay guard unavailable",
					"endpoint", endpoint, "reason", "replay_guard_unavailable", "error", err)
				writeTurnstileProblem(w, http.StatusServiceUnavailable, problemServiceUnavailable,
					"Human verification is temporarily unavailable", "5")
				return
			}
			if !claimed {
				log.Warn("turnstile token replayed", "endpoint", endpoint, "reason", "replayed_token")
				writeTurnstileProblem(w, http.StatusForbidden, problemTurnstileFailed,
					"Human verification failed. Please try again.", "")
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// turnstileTokenFromRequest 依次从查询参数与 JSON 请求体读取 turnstile_token。
// 请求体被完整缓存后还原，下游 handler 必须仍能按原样解析。
func turnstileTokenFromRequest(r *http.Request, maxBody int64) (string, error) {
	if token := strings.TrimSpace(r.URL.Query().Get(TurnstileTokenParam)); token != "" {
		return token, nil
	}
	if r.Body == nil || r.Method == http.MethodGet || r.Method == http.MethodHead {
		return "", nil
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		return "", fmt.Errorf("turnstile: read body: %w", err)
	}
	if int64(len(raw)) > maxBody {
		return "", errors.New("turnstile: request body too large")
	}
	// 无论是否找到令牌都还原请求体：找不到时由本中间件拒绝，但保持
	// 「中间件不改变下游可见输入」的契约，便于排障与测试。
	r.Body = io.NopCloser(bytes.NewReader(raw))
	r.ContentLength = int64(len(raw))
	if len(bytes.TrimSpace(raw)) == 0 {
		return "", nil
	}
	var payload struct {
		TurnstileToken string `json:"turnstile_token"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		// 非 JSON 请求体：视为没有令牌；格式错误由业务 handler 报 400。
		return "", nil
	}
	return strings.TrimSpace(payload.TurnstileToken), nil
}

// turnstileRemoteIP 经 internal/clientip 解析（与限流/风控同一信任边界）。
// TRUSTED_PROXIES 为空时只取连接地址；X-Forwarded-For 只有在直连 peer 属于
// 可信代理时才被采信，避免攻击者自选 siteverify 的 remoteip 绑定值。
func turnstileRemoteIP(r *http.Request) string {
	return clientip.ClientIP(r)
}

// verifyTurnstileToken 调用 siteverify。任何非 200/网络错误/解析失败都
// 作为 error 返回，由调用方统一映射 503（fail-closed）。
func verifyTurnstileToken(ctx context.Context, client *http.Client, verifyURL,
	secret, token, remoteIP string) (bool, error) {

	form := url.Values{
		"secret":   {secret},
		"response": {token},
	}
	if remoteIP != "" {
		form.Set("remoteip", remoteIP)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, verifyURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	// 响应体只需读取一次；关闭失败不影响已读到的结论。
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return false, err
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("turnstile: siteverify status %d", resp.StatusCode)
	}
	var out struct {
		Success bool `json:"success"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return false, fmt.Errorf("turnstile: decode siteverify response: %w", err)
	}
	return out.Success, nil
}

// claimTurnstileToken 以 SET NX 原子占用 token。键只存 sha256 摘要，
// 不把令牌原文（短时凭据）落进 Redis。
func claimTurnstileToken(ctx context.Context, rdb *redis.Client,
	token, endpoint, remoteIP string) (bool, error) {

	if rdb == nil {
		return false, errors.New("turnstile: redis replay guard is not configured")
	}
	digest := sha256.Sum256([]byte(token))
	key := turnstileReplayPrefix + hex.EncodeToString(digest[:])
	return rdb.SetNX(ctx, key, endpoint+"|"+remoteIP, TurnstileTokenTTL).Result()
}

// writeTurnstileProblem 输出全库统一的 RFC 7807 Problem（GC-6）。
// 复用 dto.Problem，避免与 httpapi.writeProblem 的字段形状各自漂移。
func writeTurnstileProblem(w http.ResponseWriter, status int, problemType, title, retryAfter string) {
	if retryAfter != "" {
		w.Header().Set("Retry-After", retryAfter)
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(dto.Problem{
		Type:   problemType,
		Title:  title,
		Status: status,
	})
}
