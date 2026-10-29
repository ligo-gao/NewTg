// POST /api/settings/test/smtp —— SMTP 配置的分阶段连通性探测。
//
// 语义：
//   - 会话 + CSRF + settings:manage（由 Register 的鉴权链承担）；
//   - 配置取自**已保存**快照；host 或发件地址为空 → 422
//     `smtp_not_configured`；
//   - 只走到 MAIL FROM 为止，**不进入 DATA、不投递任何邮件**；
//   - 3 次/分钟/管理员（Redis 固定窗口，故障 fail-open——只读探测）；
//   - 阶段结论用固定枚举 + SMTP 数字码表达，绝不透传服务器原文：SMTP
//     的 5xx 文本常回显地址，原文进日志即 PII；
//   - 审计 settings.test_smtp，detail 仅 host/port/ok/stage/reason/
//     smtp_code，不含账号、发件地址或密码。

package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/notify"
)

const (
	// smtpTestRateLimit / Window：与 Webhook 测试同口径的管理员限流。
	smtpTestRateLimit  = 3
	smtpTestRateWindow = time.Minute
	// smtpTestRateMark 沿用两位桶标记；维度是管理员 ID。
	smtpTestRateMark = "ST"

	// smtpProblemNotConfigured 与「探测失败」区分：前者是管理员能就地补齐
	// 的配置缺失（422），后者是探测结果（200 + ok=false）。
	smtpProblemNotConfigured = "https://newtg.dev/problems/smtp_not_configured"
)

// SMTPProber 是 SMTP 配置探测的最小能力集（生产为 *notify.SMTPSender）。
//
// Configured 只看 host + 发件地址，与真实投递同口径；Target 供审计使用，
// 只暴露主机/端口，不暴露账号与发件地址。
type SMTPProber interface {
	Configured(ctx context.Context) bool
	Probe(ctx context.Context) (notify.SMTPProbeResult, error)
	Target() (host string, port int)
}

// smtpTestStep 是 wire 契约的一个阶段。字段名与 notify.SMTPProbeStep
// 保持一致，但独立定义：契约变更必须显式改这里，而不是被内部结构带去。
type smtpTestStep struct {
	Stage    string `json:"stage"`
	Status   string `json:"status"`
	Reason   string `json:"reason,omitempty"`
	SMTPCode int    `json:"smtp_code,omitempty"`
}

// smtpTestResponse 是探测结果。steps 按执行顺序返回，未执行的阶段不出现。
type smtpTestResponse struct {
	OK    bool           `json:"ok"`
	Steps []smtpTestStep `json:"steps"`
}

func (h *SettingsHandlers) handleTestSMTP(w http.ResponseWriter, r *http.Request) {
	if h.smtp == nil {
		// 装配缺失不是 422：管理员无法通过改配置修复它。
		h.log.Error("settings: smtp prober is not configured")
		writeProblemDetail(w, http.StatusServiceUnavailable, authProblemInternal,
			"Service unavailable", "")
		return
	}

	principal, _ := auth.PrincipalFrom(r.Context())
	actor := int64(0)
	if principal != nil {
		actor = principal.UserID
	}
	if h.allowSMTPTest(w, r, actor) {
		return
	}

	if !h.smtp.Configured(r.Context()) {
		h.writeSMTPTestAudit(r, actor, smtpTestResponse{OK: false}, http.StatusUnprocessableEntity)
		writeProblemDetail(w, http.StatusUnprocessableEntity, smtpProblemNotConfigured,
			"SMTP is not configured",
			"configure notify.smtp.host and notify.smtp.from_email before testing")
		return
	}

	result, err := h.smtp.Probe(r.Context())
	if err != nil {
		// 密钥解封失败等属于服务端配置/密钥问题：只记日志，响应固定文案。
		h.log.Error("settings: smtp probe failed", "err", err)
		h.writeSMTPTestAudit(r, actor, smtpTestResponse{OK: false}, http.StatusInternalServerError)
		writeProblemDetail(w, http.StatusInternalServerError, authProblemInternal,
			"Internal Server Error", "")
		return
	}

	payload := smtpTestResponse{OK: result.OK, Steps: make([]smtpTestStep, 0, len(result.Steps))}
	for _, step := range result.Steps {
		payload.Steps = append(payload.Steps, smtpTestStep{
			Stage: step.Stage, Status: step.Status, Reason: step.Reason, SMTPCode: step.SMTPCode,
		})
	}
	h.writeSMTPTestAudit(r, actor, payload, http.StatusOK)
	writeJSON(w, http.StatusOK, payload)
}

// allowSMTPTest 消耗管理员维度的固定窗口名额；返回 true 表示已写出 429。
// Redis 故障 fail-open，与 Webhook 测试同一策略：这是已认证的管理员探测，
// 不是认证或计费边界。
func (h *SettingsHandlers) allowSMTPTest(w http.ResponseWriter, r *http.Request, actor int64) bool {
	if h.rdb == nil {
		return false
	}
	key := RateLimitKeyPrefix + smtpTestRateMark + ":admin:" + strconv.FormatInt(actor, 10)
	limited, retry, err := allowRequestRate(r.Context(), h.rdb, key, smtpTestRateLimit, smtpTestRateWindow)
	if err != nil {
		h.log.Warn("settings: smtp test rate limiter unavailable; allowing request", "err", err)
		return false
	}
	if !limited {
		return false
	}
	writeRateLimited(w, retry, "Too many requests")
	return true
}

// writeSMTPTestAudit 写 settings.test_smtp。
//
// detail 只有 host/port（管理员自填的探测目标）、ok、最后一个阶段与其
// 分类/数字码。账号、发件地址与密码绝不进入审计。
func (h *SettingsHandlers) writeSMTPTestAudit(
	r *http.Request, actor int64, result smtpTestResponse, httpStatus int,
) {
	if h.audit == nil {
		return
	}
	host, port := h.smtp.Target()
	detailMap := map[string]any{
		"host": host,
		"port": port,
		"ok":   result.OK,
	}
	if len(result.Steps) > 0 {
		last := result.Steps[len(result.Steps)-1]
		detailMap["stage"] = last.Stage
		if last.Reason != "" {
			detailMap["reason"] = last.Reason
		}
		if last.SMTPCode != 0 {
			detailMap["smtp_code"] = last.SMTPCode
		}
	}
	detail, err := json.Marshal(detailMap)
	if err != nil {
		return
	}
	event := audit.Event{
		Action:     audit.ActionSettingsTestSMTP,
		Actor:      actor,
		Method:     http.MethodPost,
		Route:      "/api/settings/test/smtp",
		StatusCode: httpStatus,
		IP:         clientIP(r),
		UserAgent:  r.UserAgent(),
		Detail:     detail,
	}
	_ = writeAuditEvent(r.Context(), h.audit, event)
}
