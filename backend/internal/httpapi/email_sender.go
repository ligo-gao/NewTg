package httpapi

// W13-EMAIL：事务性邮件发送器的生产适配器。
//
// 三个 handler 侧接缝（EmailCodeSender / PasswordResetSender /
// NotificationTestSender）共用同一份 SMTP 配置（配置中心 notify.smtp.*
// 快照）与同一个出口，避免「验证码能发、重置不能发」这类半装配状态。
//
// fail-closed 是冻结口径：SMTP 未配置时 Configured 返回 false，端点必须
// 503（problem type email_unavailable），绝不能沿用 M1 的 204 假成功——
// 用户会以为验证码已发出而永远收不到。M1 的 nil 发信器同样按「未配置」处理。
//
// 日志/错误链只写邮件类别，绝不带验证码、重置链接或收件地址：前两者是
// 可直接使用的凭据，地址是 PII。

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"newtg/gateway/internal/notify"
)

// SMTPMailer 是 notify.SMTPSender 的最小方法集（便于测试注入 fake）。
type SMTPMailer interface {
	// Configured 报告当前快照下 SMTP 是否可投递（host + 发件地址）。
	Configured(ctx context.Context) bool
	// Send 投递纯文本邮件并返回 Message-ID（退信关联用）。
	Send(ctx context.Context, to, subject, body string) (string, error)
}

// authProblemEmailUnavailable 是「事务性邮件通道未配置」的 RFC 7807 type。
// 冻结固定 slug：客户端与运维告警按它区分「配置缺失（503，重试无用）」与
// 「投递失败（502，可重试）」。
const authProblemEmailUnavailable = "https://newtg.dev/problems/email_unavailable"

var (
	// ErrEmailUnavailable 表示 SMTP 未配置或配置不可用：事务性邮件无法投递。
	// handler 据此返回 503 + email_unavailable，而不是 502（重试无用）。
	ErrEmailUnavailable = errors.New("httpapi: email delivery is not configured")
	// ErrNotificationChannelUnavailable 表示本部署没有该通知渠道的测试出口。
	// 当前只装配 SMTP（email）；webhook/bark/gotify 属模块 08 的后续装配。
	ErrNotificationChannelUnavailable = errors.New("httpapi: notification channel is not available")
)

// TransactionalEmailSender 实现三个事务性邮件接缝，可并发使用。
type TransactionalEmailSender struct {
	mailer SMTPMailer
}

// NewTransactionalEmailSender 用同一个 SMTP 出口构造全部事务性邮件接缝。
// mailer 为 nil 时 Configured 恒 false（等价未配置，端点 fail-closed）。
func NewTransactionalEmailSender(mailer SMTPMailer) *TransactionalEmailSender {
	return &TransactionalEmailSender{mailer: mailer}
}

// Configured 供 handler 在生成验证码/写队列/消耗配额前判断能否投递。
// 每次调用都读配置中心快照：管理员保存 SMTP 后立即生效，无需重启。
func (s *TransactionalEmailSender) Configured(ctx context.Context) bool {
	return s != nil && s.mailer != nil && s.mailer.Configured(ctx)
}

// SendEmailCode 投递一次性验证码。正文只含验证码与用途说明。
func (s *TransactionalEmailSender) SendEmailCode(ctx context.Context, email, purpose, code string) error {
	subject, body := emailCodeContent(purpose, code)
	return s.send(ctx, email, subject, body)
}

// SendPasswordReset 投递一次性重置链接。正文只含链接（token 在 query 中）。
func (s *TransactionalEmailSender) SendPasswordReset(ctx context.Context, email, resetURL string) error {
	subject := "Reset your password"
	body := fmt.Sprintf(
		"We received a request to reset the password for this account.\n\n"+
			"Open the link below to choose a new password:\n\n%s\n\n"+
			"The link expires in %d minutes and can be used once.\n"+
			"If you did not request this, you can ignore this email.\n",
		resetURL, int(passwordResetTokenTTL/time.Minute))
	return s.send(ctx, email, subject, body)
}

// SendTestNotification 实现通知测试接缝。本部署只装配 SMTP，因此仅支持
// email 渠道；其它渠道返回 ErrNotificationChannelUnavailable 由端点 503。
func (s *TransactionalEmailSender) SendTestNotification(ctx context.Context, channel, target string) error {
	if channel != "email" {
		return fmt.Errorf("%w: %s", ErrNotificationChannelUnavailable, channel)
	}
	return s.send(ctx, target, "Test notification",
		"This is a test email from your gateway. No action is required.\n")
}

// send 是三个接缝的唯一出口：先按快照判断可用性，再投递。
func (s *TransactionalEmailSender) send(ctx context.Context, to, subject, body string) error {
	if !s.Configured(ctx) {
		return ErrEmailUnavailable
	}
	if _, err := s.mailer.Send(ctx, to, subject, body); err != nil {
		if errors.Is(err, notify.ErrSMTPNotConfigured) {
			// 配置在检查与发送之间被清空：仍按不可用处理，而不是 502。
			return ErrEmailUnavailable
		}
		// 不包装正文/收件地址：错误链只保留 SMTP 侧原因。
		return fmt.Errorf("httpapi: send transactional email: %w", err)
	}
	return nil
}

// emailCodeContent 按用途生成主题与纯文本正文。正文不含用户名、邮箱、
// IP 等其它信息，最小化泄露面。
func emailCodeContent(purpose, code string) (string, string) {
	action := "verify your email address"
	switch purpose {
	case "register":
		action = "complete your registration"
	case "reset":
		action = "reset your password"
	case "bind":
		action = "bind this email address"
	}
	body := fmt.Sprintf(
		"Your verification code is: %s\n\n"+
			"Use this code to %s. It expires in %d minutes.\n"+
			"If you did not request this, you can ignore this email.\n",
		code, action, int(emailCodeTTL/time.Minute))
	return "Your verification code", body
}

// emailDeliveryConfigured 是 handler 侧的可用性判断：nil 发信器等价未配置
// （fail-closed，M1 的「nil = 204」口径已废止）；实现 Configured 的发送器
// 按配置中心实时快照判断；未实现 Configured 的发送器（测试假实现/自建实现）
// 视为可用，避免把测试缝隙误判为故障。
func emailDeliveryConfigured(ctx context.Context, sender any) bool {
	if sender == nil {
		return false
	}
	if checker, ok := sender.(interface{ Configured(context.Context) bool }); ok {
		return checker.Configured(ctx)
	}
	return true
}

// writeEmailUnavailable 是三个事务性邮件端点的统一 fail-closed 出口。
// 不设置 Retry-After：这是配置缺失而不是瞬时故障，立即重试没有意义。
func writeEmailUnavailable(w http.ResponseWriter) {
	writeProblem(w, http.StatusServiceUnavailable, authProblemEmailUnavailable,
		"Email delivery is not configured")
}
