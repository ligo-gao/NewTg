package notify

import (
	"context"
	"encoding/json"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

// mapConfig 是 ConfigReader 的测试实现（键 → JSON 值）。
type mapConfig map[string]any

func (m mapConfig) Get(key string) (json.RawMessage, bool) {
	value, ok := m[key]
	if !ok {
		return nil, false
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, false
	}
	return raw, true
}

func TestSMTPFallbacksUsernameAndSiteTitle(t *testing.T) {
	sender := NewSMTPSender(mapConfig{
		"notify.smtp.host":       "smtp.example.com",
		"notify.smtp.username":   "bot@example.com",
		"notify.smtp.from_email": "",
		"notify.smtp.from_name":  "",
		"site.title":             "Example Gateway",
	}, nil)

	// G7-R41：host + username 即已配置（from_email 回退 username）。
	if !sender.Configured(context.Background()) {
		t.Fatal("host+username 应被视为已配置")
	}
	settings, err := sender.settings()
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	if settings.FromEmail != "bot@example.com" {
		t.Fatalf("from_email 应回退 username，得到 %q", settings.FromEmail)
	}
	if settings.FromName != "Example Gateway" {
		t.Fatalf("from_name 应回退 site.title，得到 %q", settings.FromName)
	}

	// 显式 from_email 优先于 username。
	sender = NewSMTPSender(mapConfig{
		"notify.smtp.host":       "smtp.example.com",
		"notify.smtp.username":   "bot@example.com",
		"notify.smtp.from_email": "noreply@example.com",
	}, nil)
	settings, _ = sender.settings()
	if settings.FromEmail != "noreply@example.com" {
		t.Fatalf("显式 from_email 不应被 username 覆盖，得到 %q", settings.FromEmail)
	}
}

func TestSMTPHeaderSanitisation(t *testing.T) {
	message := buildMessage(smtpSettings{
		FromEmail: "noreply@example.com",
		FromName:  "Gateway",
	}, "user@example.com", "Hello\r\nBcc: attacker@example.com", "body", "<id@example.com>")
	if strings.Contains(message, "\r\nBcc:") {
		t.Fatalf("主题中的 CRLF 必须被脱敏，实际消息：%q", message)
	}
	for _, line := range strings.Split(message, "\r\n") {
		if strings.HasPrefix(line, "Bcc:") {
			t.Fatal("不得注入 Bcc 头")
		}
	}
	// 收件人里的换行同样不能分裂出新头。
	message = buildMessage(smtpSettings{FromEmail: "noreply@example.com"},
		"user@example.com\r\nBcc: attacker@example.com", "subject", "body", "<id@example.com>")
	if strings.Contains(message, "\r\nBcc:") {
		t.Fatal("收件人中的 CRLF 必须被剥离")
	}
}

func TestSMTPConversationDeadline(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	// 接受连接后完全不说话：没有 SetDeadline 时 smtp.NewClient 会在读
	// greeting 上永久阻塞。
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			time.Sleep(3 * time.Second)
			_ = conn.Close()
		}
	}()
	host, portText, _ := net.SplitHostPort(listener.Addr().String())
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("parse listener port: %v", err)
	}
	sender := NewSMTPSender(mapConfig{
		"notify.smtp.host":       host,
		"notify.smtp.port":       port,
		"notify.smtp.encryption": "none",
		"notify.smtp.from_email": "noreply@example.com",
	}, nil)
	sender.DialTimeout = 200 * time.Millisecond

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := sender.Send(ctx, "user@example.com", "subject", "body"); err == nil {
		t.Fatal("对停滞服务器应返回超时错误")
	}
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Fatalf("SMTP 会话应在 deadline 内失败，实际耗时 %v", elapsed)
	}
}
