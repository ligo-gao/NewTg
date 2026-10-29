package notify

// SMTP 配置探测（「测试连接」端点 notify.SMTPProber 的生产实现）。
//
// 与 Send 的区别是**只走握手、不进入 DATA**：管理员改完 notify.smtp.* 后
// 最想知道的是「卡在哪一步」，而 Send 的对外错误被刻意压成单一分类
// （ERR_EMAIL_DELIVERY_FAILED），因为 SMTP 服务器会在错误里回显收件地址。
// 探针因此把每一步单独执行、单独分类，且只回传固定枚举与数字码，绝不
// 透传服务器原文。
//
// 阶段按执行顺序追加（而不是固定顺序）：STARTTLS 必须先 EHLO 才能协商，
// 所以 tls 步在 greeting 之后；encryption=none 时 tls 记为 skipped。
// 前端按返回数组渲染，未执行的阶段不出现在结果里。

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/smtp"
	"net/textproto"
	"strconv"
	"syscall"
	"time"
)

// 探针阶段名（冻结枚举，前端按 stage 映射文案）。
const (
	SMTPProbeStageDNS      = "dns"
	SMTPProbeStageConnect  = "connect"
	SMTPProbeStageTLS      = "tls"
	SMTPProbeStageGreeting = "greeting"
	SMTPProbeStageAuth     = "auth"
	SMTPProbeStageMailFrom = "mail_from"
)

// 探针阶段状态。
const (
	SMTPProbeStatusPassed  = "passed"
	SMTPProbeStatusFailed  = "failed"
	SMTPProbeStatusSkipped = "skipped"
)

// 失败的固定分类（Reason）。服务器原文与地址绝不进入该字段。
const (
	SMTPProbeReasonConfigIncomplete    = "config_incomplete"
	SMTPProbeReasonDNSNotFound         = "dns_not_found"
	SMTPProbeReasonDNSError            = "dns_error"
	SMTPProbeReasonConnectTimeout      = "connect_timeout"
	SMTPProbeReasonConnectRefused      = "connect_refused"
	SMTPProbeReasonConnectFailed       = "connect_failed"
	SMTPProbeReasonTLSCertInvalid      = "tls_cert_invalid"
	SMTPProbeReasonTLSFailed           = "tls_failed"
	SMTPProbeReasonStartTLSUnsupported = "starttls_unsupported"
	SMTPProbeReasonGreetingFailed      = "greeting_failed"
	SMTPProbeReasonAuthFailed          = "auth_failed"
	SMTPProbeReasonAuthUnsupported     = "auth_unsupported"
	SMTPProbeReasonMailFromRejected    = "mail_from_rejected"
)

// SMTPProbeStep 是一个阶段的结论。Reason/SMTPCode 仅在 failed 时有意义；
// SMTPCode 是服务器返回的数字码（如 535/550），不含文本。
type SMTPProbeStep struct {
	Stage    string `json:"stage"`
	Status   string `json:"status"`
	Reason   string `json:"reason,omitempty"`
	SMTPCode int    `json:"smtp_code,omitempty"`
}

// SMTPProbeResult 是一次探测的完整结论。OK 等价于「走到了 MAIL FROM 且
// 服务器接受」。
type SMTPProbeResult struct {
	OK    bool            `json:"ok"`
	Steps []SMTPProbeStep `json:"steps"`
}

// smtpProbeDefaultTimeout 是单个网络阶段的超时。探针是交互式动作，
// 5 秒足够覆盖 DNS/TCP/TLS/AUTH；超时即判失败，避免管理端等到 HTTP 层
// 超时后拿到一条含糊的错误。
const smtpProbeDefaultTimeout = 5 * time.Second

// Probe 依次执行 DNS → TCP → TLS → EHLO → AUTH → MAIL FROM，任一步失败
// 立即停止。返回 error 仅表示配置读取失败（如密钥解封失败），投递层面的
// 失败都体现在 Steps 里。
func (s *SMTPSender) Probe(ctx context.Context) (SMTPProbeResult, error) {
	settings, err := s.settings()
	if err != nil {
		return SMTPProbeResult{}, err
	}
	timeout := s.ProbeTimeout
	if timeout <= 0 {
		timeout = smtpProbeDefaultTimeout
	}

	result := SMTPProbeResult{}
	pass := func(stage string) {
		result.Steps = append(result.Steps, SMTPProbeStep{Stage: stage, Status: SMTPProbeStatusPassed})
	}
	skip := func(stage string) {
		result.Steps = append(result.Steps, SMTPProbeStep{Stage: stage, Status: SMTPProbeStatusSkipped})
	}
	fail := func(stage, reason string, code int) (SMTPProbeResult, error) {
		result.Steps = append(result.Steps, SMTPProbeStep{
			Stage: stage, Status: SMTPProbeStatusFailed, Reason: reason, SMTPCode: code,
		})
		return result, nil
	}

	// 配置不完整时没有可探测的对象；仍返回一个 failed 步，让前端不必为
	// 「空结果」再解释一遍。HTTP 层通常已在 Configured 处拦成 422。
	if settings.Host == "" || settings.FromEmail == "" {
		return fail(SMTPProbeStageDNS, SMTPProbeReasonConfigIncomplete, 0)
	}

	lookupCtx, cancelLookup := context.WithTimeout(ctx, timeout)
	_, err = net.DefaultResolver.LookupHost(lookupCtx, settings.Host)
	cancelLookup()
	if err != nil {
		reason := SMTPProbeReasonDNSError
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
			reason = SMTPProbeReasonDNSNotFound
		}
		return fail(SMTPProbeStageDNS, reason, 0)
	}
	pass(SMTPProbeStageDNS)

	address := net.JoinHostPort(settings.Host, strconv.Itoa(settings.Port))
	dialer := &net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return fail(SMTPProbeStageConnect, classifySMTPDialError(err), 0)
	}
	defer func() { _ = conn.Close() }()
	// 与 Send 同口径：一次 deadline 覆盖整段会话，服务器接受连接后不再
	// 说话时探针会在 timeout 内失败，而不是挂到 HTTP 层超时。
	_ = conn.SetDeadline(time.Now().Add(timeout))
	pass(SMTPProbeStageConnect)

	tlsConfig := &tls.Config{
		ServerName:         settings.Host,
		InsecureSkipVerify: settings.InsecureSkipVerify,
		MinVersion:         tls.VersionTLS12,
	}

	switch settings.Encryption {
	case "ssl":
		tlsConn := tls.Client(conn, tlsConfig)
		handshakeCtx, cancelHandshake := context.WithTimeout(ctx, timeout)
		err := tlsConn.HandshakeContext(handshakeCtx)
		cancelHandshake()
		if err != nil {
			return fail(SMTPProbeStageTLS, classifySMTPTLSError(err), 0)
		}
		conn = tlsConn
		pass(SMTPProbeStageTLS)
	case "starttls":
		// STARTTLS 需要先 EHLO；它的 tls 步在 greeting 之后追加。
	default:
		skip(SMTPProbeStageTLS)
	}

	client, err := smtp.NewClient(conn, settings.Host)
	if err != nil {
		return fail(SMTPProbeStageGreeting, SMTPProbeReasonGreetingFailed, 0)
	}
	defer func() { _ = client.Close() }()
	pass(SMTPProbeStageGreeting)

	if settings.Encryption == "starttls" {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return fail(SMTPProbeStageTLS, SMTPProbeReasonStartTLSUnsupported, 0)
		}
		if err := client.StartTLS(tlsConfig); err != nil {
			return fail(SMTPProbeStageTLS, classifySMTPTLSError(err), 0)
		}
		pass(SMTPProbeStageTLS)
	}

	if settings.Username == "" && !settings.ForceAuthLogin {
		// 与 Send 完全一致：没有凭证且不强制 AUTH 时不做认证。
		skip(SMTPProbeStageAuth)
	} else {
		if ok, _ := client.Extension("AUTH"); !ok {
			return fail(SMTPProbeStageAuth, SMTPProbeReasonAuthUnsupported, 0)
		}
		auth := smtp.PlainAuth("", settings.Username, settings.Password, settings.Host)
		if err := client.Auth(auth); err != nil {
			var smtpErr *textproto.Error
			code := 0
			if errors.As(err, &smtpErr) {
				code = smtpErr.Code
			}
			return fail(SMTPProbeStageAuth, SMTPProbeReasonAuthFailed, code)
		}
		pass(SMTPProbeStageAuth)
	}

	if err := client.Mail(settings.FromEmail); err != nil {
		var smtpErr *textproto.Error
		code := 0
		if errors.As(err, &smtpErr) {
			code = smtpErr.Code
		}
		return fail(SMTPProbeStageMailFrom, SMTPProbeReasonMailFromRejected, code)
	}
	pass(SMTPProbeStageMailFrom)

	result.OK = true
	// 礼貌收尾；QUIT 失败不影响已经得出的结论。
	_ = client.Quit()
	return result, nil
}

// classifySMTPDialError 把拨号错误映射到固定分类。超时与拒绝是两种完全
// 不同的处置（前者查防火墙，后者查端口/服务），因此分开。
func classifySMTPDialError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return SMTPProbeReasonConnectTimeout
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return SMTPProbeReasonConnectTimeout
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return SMTPProbeReasonConnectRefused
	}
	return SMTPProbeReasonConnectFailed
}

// classifySMTPTLSError 区分「证书不被信任」与其它握手失败：前者通常意味着
// 证书过期/域名不匹配，后者意味着对端根本不是 TLS（端口选错）。
func classifySMTPTLSError(err error) string {
	var verification *tls.CertificateVerificationError
	if errors.As(err, &verification) {
		return SMTPProbeReasonTLSCertInvalid
	}
	var unknownAuthority x509.UnknownAuthorityError
	if errors.As(err, &unknownAuthority) {
		return SMTPProbeReasonTLSCertInvalid
	}
	var hostname x509.HostnameError
	if errors.As(err, &hostname) {
		return SMTPProbeReasonTLSCertInvalid
	}
	return SMTPProbeReasonTLSFailed
}
