// POST /api/settings/test/webhook —— 发送测试 Webhook 事件（modules-08 F-08-20 / D2-14）。
//
// 语义：
//   - 会话 + CSRF + settings:manage（由 Register 的鉴权链承担）；
//   - 配置取自**已保存**快照；URL/secret 未配置 → 422；
//   - 不受 enabled=false 影响（测试目的就是验证未启用配置）；
//   - 3 次/分钟/管理员（Redis 固定窗口，故障 fail-open——只读探测）；
//   - 已收到 HTTP 响应即 200 `{ok,status_code}`，非 2xx 时 ok=false；
//     连接失败/超时 → 504 固定文案；SSRF 守卫拒绝 → 422 固定文案；
//   - **不写 notification_deliveries**（测试事件不是业务投递）；
//   - 审计 settings.test_webhook，detail 仅 event/status_code/ok/host。

package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/notify"
)

const (
	// webhookTestRateLimit / Window：D2-14 冻结的测试频率。
	webhookTestRateLimit  = 3
	webhookTestRateWindow = time.Minute
	// webhookTestRateMark 沿用基线两位桶标记风格；维度是管理员 ID，
	// 同一 NAT 后的多个管理员不共享桶（与 deploy 连接测试同口径）。
	webhookTestRateMark = "WT"
	// webhookTestEvent 是样例事件名。它**不是事件契约**：业务 payload 的
	// 字段与事件名归模块 01/03/04，这里只用于连通性与签名验证。
	webhookTestEvent = "test"

	// 两个 422 必须可区分（D2-14 复核 M1）：未配置是管理员能就地补齐的
	// 状态，目标被守卫拒绝则是 URL 本身不可用；前端据此给「去配置」还是
	// 「换目标/加白名单」的不同动作。合并成 validation_failed 会让 UI 只能
	// 展示一句含糊的通用文案。
	webhookProblemNotConfigured = "https://newtg.dev/problems/webhook_not_configured"
	webhookProblemTargetBlocked = "https://newtg.dev/problems/webhook_target_blocked"
)

// webhookTestPayload 是固定样例字节。刻意用常量而不是运行时 Marshal：
// 接收方可以用同一串固定字节做签名回归，不受字段顺序影响。
var webhookTestPayload = []byte(`{"event":"test","message":"NewTG webhook test event","webhook_test":true}`)

type webhookTestResponse struct {
	OK         bool `json:"ok"`
	StatusCode int  `json:"status_code"`
}

func (h *SettingsHandlers) handleTestWebhook(w http.ResponseWriter, r *http.Request) {
	if h.webhook == nil {
		// 装配缺失不是 422：管理员无法通过改配置修复它。
		h.log.Error("settings: webhook test sender is not configured")
		writeProblemDetail(w, http.StatusServiceUnavailable, authProblemInternal,
			"Service unavailable", "")
		return
	}

	principal, _ := auth.PrincipalFrom(r.Context())
	actor := int64(0)
	if principal != nil {
		actor = principal.UserID
	}
	if h.allowWebhookTest(w, r, actor) {
		return
	}

	result, err := h.webhook.SendTest(r.Context(), webhookTestPayload)
	if err != nil {
		status, problemType, title, detail := webhookTestErrorFields(err)
		if status == http.StatusInternalServerError {
			// 解封失败等内部错误只记服务端日志；响应固定文案。
			h.log.Error("settings: webhook test failed", "err", err)
		}
		h.writeWebhookTestAudit(r, actor, webhookTestResponse{OK: false, StatusCode: 0}, status)
		writeProblemDetail(w, status, problemType, title, detail)
		return
	}

	payload := webhookTestResponse{OK: result.OK, StatusCode: result.StatusCode}
	h.writeWebhookTestAudit(r, actor, payload, http.StatusOK)
	writeJSON(w, http.StatusOK, payload)
}

// allowWebhookTest 消耗管理员维度的固定窗口名额；返回 true 表示已写出 429。
//
// Redis 故障 fail-open：这是已认证的管理员探测，不是认证/计费边界；
// 与 deploy 连接测试、只读排障面同一 fail 策略。
func (h *SettingsHandlers) allowWebhookTest(w http.ResponseWriter, r *http.Request, actor int64) bool {
	if h.rdb == nil {
		return false
	}
	key := RateLimitKeyPrefix + webhookTestRateMark + ":admin:" + strconv.FormatInt(actor, 10)
	limited, retry, err := allowRequestRate(r.Context(), h.rdb, key, webhookTestRateLimit, webhookTestRateWindow)
	if err != nil {
		h.log.Warn("settings: webhook test rate limiter unavailable; allowing request", "err", err)
		return false
	}
	if !limited {
		return false
	}
	writeRateLimited(w, retry, "Too many requests")
	return true
}

// webhookTestErrorFields 把发送错误映射为状态码、problem type 与固定文案。
//
// 分类刻意只到「未配置 / 目标被守卫拒绝 / 传输失败 / 内部错误」四档：
// 任何更细的上游错误（DNS、TLS、连接重置、响应体）都可能携带端点内部信息。
// 前两档同为 422，但 problem type 不同：前者可补齐配置后重试，后者必须
// 更换目标或调整白名单，前端需要据此给出不同动作。
func webhookTestErrorFields(err error) (status int, problemType, title, detail string) {
	switch {
	case errors.Is(err, notify.ErrWebhookNotConfigured):
		return http.StatusUnprocessableEntity, webhookProblemNotConfigured,
			"Webhook is not configured",
			"configure notify.webhook.url and notify.webhook.secret before testing"
	case errors.Is(err, notify.ErrWebhookBlockedTarget):
		return http.StatusUnprocessableEntity, webhookProblemTargetBlocked,
			"Webhook target is not allowed",
			"the configured URL is not an allowed outbound target"
	case errors.Is(err, notify.ErrWebhookTransport):
		return http.StatusGatewayTimeout, problemTypeGatewayTimeout,
			"Webhook request failed",
			"the endpoint did not return an HTTP response in time"
	default:
		return http.StatusInternalServerError, authProblemInternal,
			"Internal Server Error", ""
	}
}

// writeWebhookTestAudit 写 settings.test_webhook。
//
// detail 是 D2-14 冻结的四个字段：event/status_code/ok/host。host 只取
// 主机名（不含 path/query/userinfo），密钥与响应体绝不进入审计。
func (h *SettingsHandlers) writeWebhookTestAudit(r *http.Request, actor int64, result webhookTestResponse, httpStatus int) {
	if h.audit == nil {
		return
	}
	detail, err := json.Marshal(map[string]any{
		"event":       webhookTestEvent,
		"status_code": result.StatusCode,
		"ok":          result.OK,
		"host":        h.webhook.TargetHost(),
	})
	if err != nil {
		return
	}
	event := audit.Event{
		Action:     audit.ActionSettingsTestWebhook,
		Actor:      actor,
		Method:     http.MethodPost,
		Route:      "/api/settings/test/webhook",
		StatusCode: httpStatus,
		IP:         clientIP(r),
		UserAgent:  r.UserAgent(),
		Detail:     detail,
	}
	_ = writeAuditEvent(r.Context(), h.audit, event)
}
