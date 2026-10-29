package notify

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeSMTPConfig 控制脚本化 SMTP 服务器的行为；零值是一台「什么都能过」
// 的服务器，便于逐条只打开一个失败面。
type fakeSMTPConfig struct {
	// advertiseAuth 控制 EHLO 是否通告 AUTH（false 时客户端应报
	// auth_unsupported，而不是拿 535）。
	advertiseAuth bool
	// advertiseStartTLS 控制 EHLO 是否通告 STARTTLS。
	advertiseStartTLS bool
	// authFail 让 AUTH 返回 535。
	authFail bool
	// mailFromFail 让 MAIL FROM 返回 550。
	mailFromFail bool
}

// fakeSMTPServer 是 net.Listen 上的最小 SMTP 会话端。它只实现探针需要
// 的 EHLO/AUTH/MAIL FROM/QUIT，不解析 DATA——探针契约就是「绝不进入
// DATA」。
type fakeSMTPServer struct {
	listener net.Listener
	cfg      fakeSMTPConfig
}

func startFakeSMTP(t *testing.T, cfg fakeSMTPConfig) *fakeSMTPServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := &fakeSMTPServer{listener: listener, cfg: cfg}
	go server.serve()
	t.Cleanup(func() { _ = listener.Close() })
	return server
}

func (s *fakeSMTPServer) hostPort(t *testing.T) (string, int) {
	t.Helper()
	host, portText, err := net.SplitHostPort(s.listener.Addr().String())
	if err != nil {
		t.Fatalf("split listener addr: %v", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("parse listener port: %v", err)
	}
	return host, port
}

func (s *fakeSMTPServer) serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *fakeSMTPServer) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	reader := bufio.NewReader(conn)
	_, _ = fmt.Fprint(conn, "220 fake ESMTP\r\n")
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		command := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(command, "EHLO"), strings.HasPrefix(command, "HELO"):
			extensions := []string{"250-fake"}
			if s.cfg.advertiseStartTLS {
				extensions = append(extensions, "250-STARTTLS")
			}
			if s.cfg.advertiseAuth {
				extensions = append(extensions, "250-AUTH PLAIN")
			}
			extensions = append(extensions, "250 OK")
			_, _ = fmt.Fprint(conn, strings.Join(extensions, "\r\n")+"\r\n")
		case strings.HasPrefix(command, "AUTH"):
			if s.cfg.authFail {
				_, _ = fmt.Fprint(conn, "535 5.7.8 Authentication credentials invalid\r\n")
			} else {
				_, _ = fmt.Fprint(conn, "235 2.7.0 Authentication successful\r\n")
			}
		case strings.HasPrefix(command, "MAIL FROM"):
			if s.cfg.mailFromFail {
				_, _ = fmt.Fprint(conn, "550 5.7.1 Sender address rejected\r\n")
			} else {
				_, _ = fmt.Fprint(conn, "250 2.1.0 Ok\r\n")
			}
		case strings.HasPrefix(command, "QUIT"):
			_, _ = fmt.Fprint(conn, "221 Bye\r\n")
			return
		default:
			_, _ = fmt.Fprint(conn, "250 OK\r\n")
		}
	}
}

// probeConfig 用一台假服务器构造发送器；encryption 固定为 none，除非
// 用例显式覆盖。
func probeConfig(port int, overrides map[string]any) mapConfig {
	config := mapConfig{
		"notify.smtp.host":       "127.0.0.1",
		"notify.smtp.port":       port,
		"notify.smtp.encryption": "none",
		"notify.smtp.username":   "bot@example.com",
		"notify.smtp.from_email": "bot@example.com",
	}
	for key, value := range overrides {
		config[key] = value
	}
	return config
}

func stepOf(t *testing.T, result SMTPProbeResult, stage string) SMTPProbeStep {
	t.Helper()
	for _, step := range result.Steps {
		if step.Stage == stage {
			return step
		}
	}
	t.Fatalf("结果中没有阶段 %q，steps=%+v", stage, result.Steps)
	return SMTPProbeStep{}
}

func TestSMTPProbeSuccess(t *testing.T) {
	server := startFakeSMTP(t, fakeSMTPConfig{advertiseAuth: true})
	host, port := server.hostPort(t)
	sender := NewSMTPSender(probeConfig(port, map[string]any{"notify.smtp.host": host}), nil)

	result, err := sender.Probe(context.Background())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !result.OK {
		t.Fatalf("期望探测成功，实际 %+v", result.Steps)
	}
	want := []struct{ stage, status string }{
		{"dns", "passed"},
		{"connect", "passed"},
		{"tls", "skipped"},
		{"greeting", "passed"},
		{"auth", "passed"},
		{"mail_from", "passed"},
	}
	for _, item := range want {
		step := stepOf(t, result, item.stage)
		if step.Status != item.status {
			t.Fatalf("阶段 %s 期望 %s，实际 %+v", item.stage, item.status, step)
		}
	}
}

func TestSMTPProbeAuthRejected(t *testing.T) {
	server := startFakeSMTP(t, fakeSMTPConfig{advertiseAuth: true, authFail: true})
	host, port := server.hostPort(t)
	sender := NewSMTPSender(probeConfig(port, map[string]any{"notify.smtp.host": host}), nil)

	result, err := sender.Probe(context.Background())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if result.OK {
		t.Fatal("认证失败时 ok 必须为 false")
	}
	auth := stepOf(t, result, "auth")
	if auth.Status != "failed" || auth.Reason != "auth_failed" || auth.SMTPCode != 535 {
		t.Fatalf("auth 阶段应为 failed/auth_failed/535，实际 %+v", auth)
	}
	// 失败后不得继续下一步：MAIL FROM 不能出现在结果里。
	for _, step := range result.Steps {
		if step.Stage == "mail_from" {
			t.Fatal("认证失败后不应执行 MAIL FROM")
		}
	}
}

func TestSMTPProbeMailFromRejected(t *testing.T) {
	server := startFakeSMTP(t, fakeSMTPConfig{advertiseAuth: true, mailFromFail: true})
	host, port := server.hostPort(t)
	sender := NewSMTPSender(probeConfig(port, map[string]any{"notify.smtp.host": host}), nil)

	result, err := sender.Probe(context.Background())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if result.OK {
		t.Fatal("发件人被拒时 ok 必须为 false")
	}
	mail := stepOf(t, result, "mail_from")
	if mail.Status != "failed" || mail.Reason != "mail_from_rejected" || mail.SMTPCode != 550 {
		t.Fatalf("mail_from 阶段应为 failed/mail_from_rejected/550，实际 %+v", mail)
	}
}

func TestSMTPProbeConnectRefused(t *testing.T) {
	// 先占一个端口再释放，保证该端口上没有监听者。
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	_, portText, _ := net.SplitHostPort(listener.Addr().String())
	port, _ := strconv.Atoi(portText)
	_ = listener.Close()

	sender := NewSMTPSender(probeConfig(port, nil), nil)
	sender.ProbeTimeout = 300 * time.Millisecond

	result, err := sender.Probe(context.Background())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if result.OK {
		t.Fatal("端口拒绝连接时 ok 必须为 false")
	}
	connect := stepOf(t, result, "connect")
	if connect.Status != "failed" {
		t.Fatalf("connect 阶段应失败，实际 %+v", connect)
	}
	if connect.Reason != "connect_refused" && connect.Reason != "connect_timeout" {
		t.Fatalf("connect 失败原因应为 connect_refused/connect_timeout，实际 %q", connect.Reason)
	}
}

func TestSMTPProbeStartTLSUnsupported(t *testing.T) {
	server := startFakeSMTP(t, fakeSMTPConfig{advertiseAuth: true})
	host, port := server.hostPort(t)
	sender := NewSMTPSender(probeConfig(port, map[string]any{
		"notify.smtp.host":       host,
		"notify.smtp.encryption": "starttls",
	}), nil)

	result, err := sender.Probe(context.Background())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	tlsStep := stepOf(t, result, "tls")
	if tlsStep.Status != "failed" || tlsStep.Reason != "starttls_unsupported" {
		t.Fatalf("tls 阶段应为 failed/starttls_unsupported，实际 %+v", tlsStep)
	}
}

func TestSMTPProbeSSLAgainstPlaintext(t *testing.T) {
	server := startFakeSMTP(t, fakeSMTPConfig{advertiseAuth: true})
	host, port := server.hostPort(t)
	sender := NewSMTPSender(probeConfig(port, map[string]any{
		"notify.smtp.host":       host,
		"notify.smtp.encryption": "ssl",
	}), nil)
	sender.ProbeTimeout = 500 * time.Millisecond

	result, err := sender.Probe(context.Background())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	tlsStep := stepOf(t, result, "tls")
	if tlsStep.Status != "failed" {
		t.Fatalf("明文服务器做 SSL 握手应失败，实际 %+v", tlsStep)
	}
	if tlsStep.Reason != "tls_failed" && tlsStep.Reason != "tls_cert_invalid" {
		t.Fatalf("tls 失败原因应为 tls_failed/tls_cert_invalid，实际 %q", tlsStep.Reason)
	}
}

func TestSMTPProbeSkipsAuthWithoutCredentials(t *testing.T) {
	server := startFakeSMTP(t, fakeSMTPConfig{})
	host, port := server.hostPort(t)
	sender := NewSMTPSender(probeConfig(port, map[string]any{
		"notify.smtp.host":     host,
		"notify.smtp.username": "",
	}), nil)

	result, err := sender.Probe(context.Background())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !result.OK {
		t.Fatalf("无凭证时应跳过认证并成功，实际 %+v", result.Steps)
	}
	auth := stepOf(t, result, "auth")
	if auth.Status != "skipped" {
		t.Fatalf("auth 阶段应为 skipped，实际 %+v", auth)
	}
}

func TestSMTPProbeIncompleteConfig(t *testing.T) {
	sender := NewSMTPSender(mapConfig{"notify.smtp.host": ""}, nil)
	result, err := sender.Probe(context.Background())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if result.OK || len(result.Steps) == 0 {
		t.Fatalf("配置不完整应直接失败并给出步骤，实际 %+v", result)
	}
	if result.Steps[0].Reason != "config_incomplete" {
		t.Fatalf("第一个阶段应为 config_incomplete，实际 %+v", result.Steps[0])
	}
}
