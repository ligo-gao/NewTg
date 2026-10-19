package notify

// SMTPSender 是测试发送的生产出口（F-08-19 的配置键 notify.smtp.*）。
//
// 配置读取面用 configcenter 的 ConfigReader（快照 Get），密钥类
// notify.smtp.password 落库是 AES-256-GCM 密文，必须经 SecretOpener 解封。
// 实现刻意保持「每封邮件重新读快照」：配置中心已做进程内快照，读它没有
// 数据库成本，却能让管理员保存 SMTP 后立即测试，而不必重启进程。

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// ConfigReader 与 configcenter.Center 的读取方法集一致（避免反向依赖）。
type ConfigReader interface {
	Get(key string) (json.RawMessage, bool)
}

// SecretOpener 解封密钥类配置（与 configcenter.SecretSealer 的方法集一致）。
type SecretOpener interface {
	Open(sealed string) (string, error)
}

// SMTPSender 可并发使用。
type SMTPSender struct {
	cfg    ConfigReader
	sealer SecretOpener
	// DialTimeout 是连接/读写超时；零值用 10s。
	DialTimeout time.Duration
}

// NewSMTPSender 构造发送器。cfg 为 nil 时 Configured 恒 false（等价未配置）。
func NewSMTPSender(cfg ConfigReader, sealer SecretOpener) *SMTPSender {
	return &SMTPSender{cfg: cfg, sealer: sealer}
}

type smtpSettings struct {
	Host               string
	Port               int
	Encryption         string
	Username           string
	Password           string
	FromName           string
	FromEmail          string
	ForceAuthLogin     bool
	InsecureSkipVerify bool
}

// Configured 实现 Sender：host 与 from_email 是 SMTP 可用的最小依赖集
// （端口/加密有默认值，用户名可按服务器要求留空）。
func (s *SMTPSender) Configured(_ context.Context) bool {
	settings, err := s.settings()
	if err != nil {
		return false
	}
	return settings.Host != "" && settings.FromEmail != ""
}

// Send 投递一封纯文本邮件，返回生成的 Message-ID（供退信关联）。
func (s *SMTPSender) Send(ctx context.Context, to, subject, body string) (string, error) {
	settings, err := s.settings()
	if err != nil {
		return "", err
	}
	if settings.Host == "" || settings.FromEmail == "" {
		return "", ErrSMTPNotConfigured
	}
	timeout := s.DialTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	address := net.JoinHostPort(settings.Host, strconv.Itoa(settings.Port))

	dialer := &net.Dialer{Timeout: timeout}
	var conn net.Conn
	if settings.Encryption == "ssl" {
		conn, err = tls.DialWithDialer(dialer, "tcp", address, &tls.Config{
			ServerName:         settings.Host,
			InsecureSkipVerify: settings.InsecureSkipVerify, // 管理员显式关闭校验时才跳过
			MinVersion:         tls.VersionTLS12,
		})
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", address)
	}
	if err != nil {
		return "", fmt.Errorf("notify: smtp dial: %w", err)
	}
	// 交给 smtp.Client 后由它关闭；失败路径上也要保证不泄漏连接。
	client, err := smtp.NewClient(conn, settings.Host)
	if err != nil {
		_ = conn.Close()
		return "", fmt.Errorf("notify: smtp greeting: %w", err)
	}
	defer func() { _ = client.Close() }()

	if settings.Encryption == "starttls" {
		if err := client.StartTLS(&tls.Config{
			ServerName:         settings.Host,
			InsecureSkipVerify: settings.InsecureSkipVerify,
			MinVersion:         tls.VersionTLS12,
		}); err != nil {
			return "", fmt.Errorf("notify: smtp starttls: %w", err)
		}
	}
	if settings.Username != "" || settings.ForceAuthLogin {
		auth := smtp.PlainAuth("", settings.Username, settings.Password, settings.Host)
		if err := client.Auth(auth); err != nil {
			return "", fmt.Errorf("notify: smtp auth: %w", err)
		}
	}
	if err := client.Mail(settings.FromEmail); err != nil {
		return "", fmt.Errorf("notify: smtp mail from: %w", err)
	}
	if err := client.Rcpt(strings.TrimSpace(to)); err != nil {
		return "", fmt.Errorf("notify: smtp rcpt: %w", err)
	}
	writer, err := client.Data()
	if err != nil {
		return "", fmt.Errorf("notify: smtp data: %w", err)
	}
	messageID := newMessageID(settings.FromEmail)
	if _, err := writer.Write([]byte(buildMessage(settings, to, subject, body, messageID))); err != nil {
		_ = writer.Close()
		return "", fmt.Errorf("notify: smtp write: %w", err)
	}
	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("notify: smtp finish: %w", err)
	}
	if err := client.Quit(); err != nil {
		return "", fmt.Errorf("notify: smtp quit: %w", err)
	}
	return messageID, nil
}

// buildMessage 生成 RFC 5322 纯文本邮件。主题用 RFC 2047 编码，中文不会
// 变成乱码；正文声明 UTF-8 8bit。
func buildMessage(settings smtpSettings, to, subject, body, messageID string) string {
	from := settings.FromEmail
	if settings.FromName != "" {
		from = mime.QEncoding.Encode("utf-8", settings.FromName) + " <" + settings.FromEmail + ">"
	}
	// 头注入防护：CR/LF 绝不允许进入主题或收件人（换行会让后续内容变成新头）。
	subject = strings.ReplaceAll(strings.ReplaceAll(subject, "\r", " "), "\n", " ")
	to = strings.ReplaceAll(strings.ReplaceAll(to, "\r", ""), "\n", "")
	var builder strings.Builder
	builder.WriteString("From: " + from + "\r\n")
	builder.WriteString("To: " + to + "\r\n")
	builder.WriteString("Subject: " + mime.QEncoding.Encode("utf-8", subject) + "\r\n")
	builder.WriteString("Date: " + time.Now().UTC().Format(time.RFC1123Z) + "\r\n")
	builder.WriteString("Message-ID: " + messageID + "\r\n")
	builder.WriteString("MIME-Version: 1.0\r\n")
	builder.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	builder.WriteString("Content-Transfer-Encoding: 8bit\r\n")
	builder.WriteString("\r\n")
	builder.WriteString(body)
	return builder.String()
}

func newMessageID(fromEmail string) string {
	domain := "localhost"
	if at := strings.LastIndex(fromEmail, "@"); at >= 0 && at+1 < len(fromEmail) {
		domain = fromEmail[at+1:]
	}
	random := make([]byte, 12)
	if _, err := rand.Read(random); err != nil {
		// 熵源不可用时仍要发出邮件；用时间戳保证唯一性，Message-ID 只用于
		// 退信关联，不承担安全职责。
		return fmt.Sprintf("<%d@%s>", time.Now().UnixNano(), domain)
	}
	return "<" + hex.EncodeToString(random) + "@" + domain + ">"
}

// settings 读取 notify.smtp 段。JSON 解码失败或密钥解封失败返回错误，
// Configured 因此为 false（fail-closed：坏配置等价未配置）。
func (s *SMTPSender) settings() (smtpSettings, error) {
	settings := smtpSettings{Port: 465, Encryption: "ssl"}
	if s.cfg == nil {
		return settings, nil
	}
	settings.Host = s.stringValue("notify.smtp.host")
	if port := s.intValue("notify.smtp.port"); port > 0 && port <= 65535 {
		settings.Port = int(port)
	}
	if encryption := s.stringValue("notify.smtp.encryption"); encryption != "" {
		settings.Encryption = encryption
	}
	settings.Username = s.stringValue("notify.smtp.username")
	settings.FromName = s.stringValue("notify.smtp.from_name")
	settings.FromEmail = s.stringValue("notify.smtp.from_email")
	settings.ForceAuthLogin = s.boolValue("notify.smtp.force_auth_login")
	settings.InsecureSkipVerify = s.boolValue("notify.smtp.insecure_skip_verify")

	sealed := s.stringValue("notify.smtp.password")
	if sealed != "" {
		if s.sealer == nil {
			return settings, errors.New("notify: smtp password is sealed but no SecretOpener is configured")
		}
		plain, err := s.sealer.Open(sealed)
		if err != nil {
			return settings, fmt.Errorf("notify: open smtp password: %w", err)
		}
		settings.Password = plain
	}
	return settings, nil
}

func (s *SMTPSender) raw(key string) (json.RawMessage, bool) {
	if s.cfg == nil {
		return nil, false
	}
	return s.cfg.Get(key)
}

func (s *SMTPSender) stringValue(key string) string {
	raw, ok := s.raw(key)
	if !ok {
		return ""
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}
	return value
}

func (s *SMTPSender) intValue(key string) int64 {
	raw, ok := s.raw(key)
	if !ok {
		return 0
	}
	var value int64
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0
	}
	return value
}

func (s *SMTPSender) boolValue(key string) bool {
	raw, ok := s.raw(key)
	if !ok {
		return false
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return false
	}
	return value
}
