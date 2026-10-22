package httpapi

// W13-EMAIL 验收：事务性邮件适配器 + 三个端点（验证码 / 密码重置 / 通知测试）
// 的 fail-closed 行为。用进程内 fake SMTP 服务器跑真实 notify.SMTPSender，
// 而不是在适配器层打桩——这样「SMTP 未配置 → 503 / 配置后确实发一封」
// 走的是生产代码路径。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/notify"
)

// ---------------------------------------------------------------------------
// 进程内 fake SMTP 服务器
// ---------------------------------------------------------------------------

type fakeSMTPServer struct {
	ln   net.Listener
	msgs chan string
	// rejectRCPT 非空时对 RCPT TO 返回该行。用于复现上游回显（如
	// `550 5.1.1 <victim@example.com>: ...`），验证错误链与日志不泄露地址。
	rejectRCPT string
	// conns 统计 TCP 连接数：未配置时不仅要「没有邮件」，还不得发起连接。
	conns atomic.Int64
	wg    sync.WaitGroup
}

func newFakeSMTPServer(t *testing.T) *fakeSMTPServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen fake smtp: %v", err)
	}
	s := &fakeSMTPServer{ln: ln, msgs: make(chan string, 32)}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.serve()
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		s.wg.Wait()
	})
	return s
}

func (s *fakeSMTPServer) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.conns.Add(1)
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handle(conn)
		}()
	}
}

// handle 实现 SMTP 会话的最小子集：greeting / EHLO / MAIL / RCPT / DATA / QUIT。
func (s *fakeSMTPServer) handle(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	writer := bufio.NewWriter(conn)
	write := func(line string) {
		_, _ = writer.WriteString(line + "\r\n")
		_ = writer.Flush()
	}
	write("220 fake ESMTP")
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		command := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(command, "EHLO"), strings.HasPrefix(command, "HELO"):
			_, _ = writer.WriteString("250-fake\r\n250 OK\r\n")
			_ = writer.Flush()
		case strings.HasPrefix(command, "MAIL FROM"):
			write("250 OK")
		case strings.HasPrefix(command, "RCPT TO"):
			if s.rejectRCPT != "" {
				// 550 的文本故意包含收件地址：生产 SMTP 常见回显。
				write(s.rejectRCPT)
				continue
			}
			write("250 OK")
		case command == "DATA":
			write("354 End data with <CR><LF>.<CR><LF>")
			var body strings.Builder
			for {
				part, err := reader.ReadString('\n')
				if err != nil {
					return
				}
				if part == ".\r\n" {
					break
				}
				body.WriteString(part)
			}
			select {
			case s.msgs <- body.String():
			default:
			}
			write("250 OK")
		case command == "QUIT":
			write("221 Bye")
			return
		default:
			write("250 OK")
		}
	}
}

// next 取下一封已接收邮件（2s 超时）。
func (s *fakeSMTPServer) next(t *testing.T) string {
	t.Helper()
	select {
	case msg := <-s.msgs:
		return msg
	case <-time.After(2 * time.Second):
		t.Fatal("2s 内未收到邮件")
		return ""
	}
}

func (s *fakeSMTPServer) assertSilent(t *testing.T) {
	t.Helper()
	select {
	case msg := <-s.msgs:
		t.Fatalf("未配置 SMTP 时不应有任何投递，实际收到：%q", msg)
	case <-time.After(100 * time.Millisecond):
	}
	if n := s.conns.Load(); n != 0 {
		t.Fatalf("未配置 SMTP 时不应建立任何连接，实际 %d 次", n)
	}
}

// smtpConfigFor 生成指向 fake 服务器的 notify.smtp.* 快照。
func smtpConfigFor(t *testing.T, server *fakeSMTPServer) mapConfig {
	t.Helper()
	host, portText, err := net.SplitHostPort(server.ln.Addr().String())
	if err != nil {
		t.Fatalf("split fake smtp addr: %v", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("parse fake smtp port: %v", err)
	}
	return mapConfig{
		"notify.smtp.host":       configString(host),
		"notify.smtp.port":       json.RawMessage(strconv.Itoa(port)),
		"notify.smtp.encryption": configString("none"),
		"notify.smtp.from_email": configString("noreply@example.com"),
	}
}

func unconfiguredMailer() SMTPMailer {
	return notify.NewSMTPSender(mapConfig{}, nil)
}

func configuredMailer(t *testing.T, server *fakeSMTPServer) SMTPMailer {
	t.Helper()
	return notify.NewSMTPSender(smtpConfigFor(t, server), nil)
}

// ---------------------------------------------------------------------------
// 适配器层：三种邮件各发一次、内容正确、未配置零投递
// ---------------------------------------------------------------------------

func TestTransactionalEmailSenderSendsEachKind(t *testing.T) {
	server := newFakeSMTPServer(t)
	sender := NewTransactionalEmailSender(configuredMailer(t, server))
	ctx := context.Background()

	if !sender.Configured(ctx) {
		t.Fatal("host+from_email 已配置，Configured 应为 true")
	}

	if err := sender.SendEmailCode(ctx, "user@example.com", "register", "123456"); err != nil {
		t.Fatalf("SendEmailCode: %v", err)
	}
	codeMsg := server.next(t)
	if !strings.Contains(codeMsg, "123456") {
		t.Fatalf("验证码邮件未包含验证码：%q", codeMsg)
	}
	if !strings.Contains(codeMsg, "complete your registration") {
		t.Fatalf("验证码邮件未按用途渲染：%q", codeMsg)
	}

	const resetURL = "https://app.example.com/reset-password?token=reset-token-abc"
	if err := sender.SendPasswordReset(ctx, "user@example.com", resetURL); err != nil {
		t.Fatalf("SendPasswordReset: %v", err)
	}
	resetMsg := server.next(t)
	if !strings.Contains(resetMsg, resetURL) {
		t.Fatalf("重置邮件未包含正确链接：%q", resetMsg)
	}

	if err := sender.SendTestNotification(ctx, "email", "admin@example.com"); err != nil {
		t.Fatalf("SendTestNotification(email): %v", err)
	}
	testMsg := server.next(t)
	if !strings.Contains(testMsg, "test email") {
		t.Fatalf("测试通知正文不符：%q", testMsg)
	}

	// 非 email 渠道本部署没有出口：必须是可识别的 503 哨兵错误。
	if err := sender.SendTestNotification(ctx, "webhook", "https://example.com/hook"); !errors.Is(err, ErrNotificationChannelUnavailable) {
		t.Fatalf("webhook 测试通知 err = %v，期望 ErrNotificationChannelUnavailable", err)
	}
}

func TestTransactionalEmailSenderUnconfiguredFailsClosed(t *testing.T) {
	sender := NewTransactionalEmailSender(unconfiguredMailer())
	ctx := context.Background()

	if sender.Configured(ctx) {
		t.Fatal("空 SMTP 配置下 Configured 必须为 false")
	}
	if err := sender.SendEmailCode(ctx, "user@example.com", "register", "123456"); !errors.Is(err, ErrEmailUnavailable) {
		t.Fatalf("SendEmailCode err = %v，期望 ErrEmailUnavailable", err)
	}
	if err := sender.SendPasswordReset(ctx, "user@example.com", "https://x/reset?token=t"); !errors.Is(err, ErrEmailUnavailable) {
		t.Fatalf("SendPasswordReset err = %v，期望 ErrEmailUnavailable", err)
	}
	if err := sender.SendTestNotification(ctx, "email", "admin@example.com"); !errors.Is(err, ErrEmailUnavailable) {
		t.Fatalf("SendTestNotification err = %v，期望 ErrEmailUnavailable", err)
	}
}

// ---------------------------------------------------------------------------
// 端点层：生产装配（真实 SMTPSender + 配置中心快照）
// ---------------------------------------------------------------------------

type emailEndpointEnv struct {
	t        *testing.T
	router   *Router
	repo     *pwdTestRepo
	rdb      *redis.Client
	sessions *auth.SessionManager
	logs     *syncBuffer
}

// syncBuffer 是可并发读写的日志接收器。密码重置邮件在 worker goroutine
// 里异步投递并记日志，测试读取必须加锁（-race 下否则是数据竞争）。
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitForLog 等待日志出现 marker（异步 worker 用）。
func waitForLog(t *testing.T, logs *syncBuffer, marker string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(logs.String(), marker) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("3s 内日志未出现 %q：%q", marker, logs.String())
}

// logLineContaining 返回第一条包含 marker 的日志行（slog TextHandler 一行一条）。
// 用于把断言限定到某一次事件，避免共享 buffer 里其它事件的同类字段误满足。
func logLineContaining(logs, marker string) string {
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, marker) {
			return line
		}
	}
	return ""
}

// newEmailEndpointEnv 按生产形态装配 Router：EmailSender 是事务性邮件适配器，
// Config 是配置中心快照的测试替身，日志写入可断言的 buffer。
func newEmailEndpointEnv(t *testing.T, mailer SMTPMailer, config mapConfig) *emailEndpointEnv {
	t.Helper()
	if config == nil {
		config = mapConfig{}
	}
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })

	repo := newPwdTestRepo()
	sessions := auth.NewSessionManager(rdb, []byte(testSessionSecret))
	logs := &syncBuffer{}
	router, err := NewRouter(RouterConfig{
		Redis:         rdb,
		Sessions:      sessions,
		Repo:          repo,
		Config:        config,
		SessionSecret: []byte(testSessionSecret),
		PublicBaseURL: "https://app.example.com",
		EmailSender:   NewTransactionalEmailSender(mailer),
		Logger:        slog.New(slog.NewTextHandler(logs, nil)),
	})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	if err := router.RegisterPasswordRoutes("https://app.example.com", audit.New(&pwdAuditExec{})); err != nil {
		t.Fatalf("RegisterPasswordRoutes: %v", err)
	}
	return &emailEndpointEnv{t: t, router: router, repo: repo, rdb: rdb, sessions: sessions, logs: logs}
}

func (e *emailEndpointEnv) post(path string, body any) *httptest.ResponseRecorder {
	e.t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		e.t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

func assertProblemType(t *testing.T, rec *httptest.ResponseRecorder, wantSlug string) {
	t.Helper()
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503（body=%q）", rec.Code, rec.Body.String())
	}
	var problem struct {
		Type   string `json:"type"`
		Status int    `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
		t.Fatalf("problem body 不是 JSON: %v（%q）", err, rec.Body.String())
	}
	if !strings.HasSuffix(problem.Type, wantSlug) {
		t.Fatalf("problem type = %q, want suffix %q", problem.Type, wantSlug)
	}
	if problem.Status != http.StatusServiceUnavailable {
		t.Fatalf("problem.status = %d, want 503", problem.Status)
	}
	if !strings.Contains(rec.Body.String(), wantSlug) {
		t.Fatalf("响应体缺少固定错误码 %q: %q", wantSlug, rec.Body.String())
	}
}

// TestEmailCodeEndpointFailsClosedWithoutSMTP：SMTP 未配置时验证码端点 503，
// 且不生成/投递任何码（旧口径是 204 假成功）。
func TestEmailCodeEndpointFailsClosedWithoutSMTP(t *testing.T) {
	env := newEmailEndpointEnv(t, unconfiguredMailer(), nil)
	rec := env.post("/api/auth/email-code",
		map[string]any{"email": "user@example.com", "purpose": "register"})
	assertProblemType(t, rec, "email_unavailable")
	if !strings.Contains(env.logs.String(), "email delivery is not configured") {
		t.Fatalf("应记录配置缺失日志，实际：%q", env.logs.String())
	}
}

// TestPasswordResetFailsClosedWithoutSMTP：已注册与未注册邮箱都必须得到
// 完全相同的 503（email_unavailable），不泄露账号存在性。
func TestPasswordResetFailsClosedWithoutSMTP(t *testing.T) {
	env := newEmailEndpointEnv(t, unconfiguredMailer(), nil)
	env.repo.add(UserRecord{Username: "alice", Email: "alice@example.com", PasswordHash: "x"})

	known := env.post("/api/auth/password-reset/request", map[string]any{"email": "alice@example.com"})
	unknown := env.post("/api/auth/password-reset/request", map[string]any{"email": "nobody@example.com"})
	assertProblemType(t, known, "email_unavailable")
	assertProblemType(t, unknown, "email_unavailable")
	if known.Body.String() != unknown.Body.String() {
		t.Fatalf("已注册/未注册响应体不同，构成枚举信道：\nknown=%q\nunknown=%q",
			known.Body.String(), unknown.Body.String())
	}
}

// TestRegistrationFailsClosedWhenEmailVerificationRequired：邮箱验证开启且
// 发信不可用时，注册明确失败，而不是回「验证码无效」的误导性 400。
func TestRegistrationFailsClosedWhenEmailVerificationRequired(t *testing.T) {
	env := newEmailEndpointEnv(t, unconfiguredMailer(),
		mapConfig{"auth.email_verification_required": configBool(true)})
	rec := env.post("/api/auth/register",
		registerPayload("alice", "alice@example.com", "N7x!qPz2wR", "123456"))
	assertProblemType(t, rec, "email_unavailable")
	env.repo.mu.Lock()
	created := len(env.repo.users)
	env.repo.mu.Unlock()
	if created != 0 {
		t.Fatalf("注册应被拒绝，实际创建了 %d 个用户", created)
	}
}

// TestTransactionalEmailsDeliveredAndNotLogged：配置正确时三个端点走通，
// 邮件包含正确码/链接，日志不含明文凭据。
func TestTransactionalEmailsDeliveredAndNotLogged(t *testing.T) {
	server := newFakeSMTPServer(t)
	env := newEmailEndpointEnv(t, configuredMailer(t, server), nil)
	env.repo.add(UserRecord{Username: "alice", Email: "alice@example.com", PasswordHash: "x"})

	// 验证码：同步投递。
	rec := env.post("/api/auth/email-code",
		map[string]any{"email": "alice@example.com", "purpose": "register"})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("email-code = %d, want 204（body=%q）", rec.Code, rec.Body.String())
	}
	codeMsg := server.next(t)
	code := extractVerificationCode(t, codeMsg)
	if code == "" {
		t.Fatalf("验证码邮件中没有 6 位码：%q", codeMsg)
	}

	// 密码重置：异步投递。
	rec = env.post("/api/auth/password-reset/request", map[string]any{"email": "alice@example.com"})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("password-reset = %d, want 204（body=%q）", rec.Code, rec.Body.String())
	}
	resetMsg := server.next(t)
	token := extractResetToken(t, resetMsg)
	if token == "" {
		t.Fatalf("重置邮件中没有 token：%q", resetMsg)
	}

	// 日志只能有类别/错误，不能出现验证码或 token/链接。
	logged := env.logs.String()
	if strings.Contains(logged, code) {
		t.Fatalf("日志泄露验证码 %q：%s", code, logged)
	}
	if strings.Contains(logged, token) {
		t.Fatalf("日志泄露重置 token %q：%s", token, logged)
	}
}

func extractVerificationCode(t *testing.T, message string) string {
	t.Helper()
	const marker = "Your verification code is: "
	idx := strings.Index(message, marker)
	if idx < 0 {
		return ""
	}
	rest := message[idx+len(marker):]
	if len(rest) < 6 {
		return ""
	}
	code := rest[:6]
	if _, err := strconv.Atoi(code); err != nil {
		return ""
	}
	return code
}

func extractResetToken(t *testing.T, message string) string {
	t.Helper()
	const marker = "token="
	idx := strings.Index(message, marker)
	if idx < 0 {
		return ""
	}
	rest := message[idx+len(marker):]
	end := strings.IndexAny(rest, " \r\n")
	if end < 0 {
		end = len(rest)
	}
	return rest[:end]
}

// TestNotificationTestEndpointFailsClosedWithoutSMTP：通知测试 email 渠道在
// SMTP 未配置时 503 + email_unavailable，且不消耗测试配额。
func TestNotificationTestEndpointFailsClosedWithoutSMTP(t *testing.T) {
	env := newAccountTestEnv(t)
	user := env.addUser("alice", "alice@example.com", false)
	sid, csrf := env.sessionWithCSRF(user.ID)
	env.handler.sender = NewTransactionalEmailSender(unconfiguredMailer())

	for i := 0; i < 5; i++ {
		rec := env.authedRequest(http.MethodPost, "/api/notification-preferences/test",
			map[string]any{"channel": "email"}, sid, csrf)
		assertProblemType(t, rec, "email_unavailable")
	}
}

// ---------------------------------------------------------------------------
// 复核修复（W13-EMAIL with-fixes）：I-1 / M-1 … M-5
// ---------------------------------------------------------------------------

// TestTransactionalEmailSenderUnconfiguredSendsNothing（M-2）：未配置时三个
// 接缝都必须返回固定错误，且底层 SMTP 收到 0 次连接——不是「连接后失败」。
func TestTransactionalEmailSenderUnconfiguredSendsNothing(t *testing.T) {
	server := newFakeSMTPServer(t)
	cfg := smtpConfigFor(t, server)
	// 删掉发件地址：host 仍指向 fake，但 Configured 必须为 false。
	delete(cfg, "notify.smtp.from_email")
	sender := NewTransactionalEmailSender(notify.NewSMTPSender(cfg, nil))
	ctx := context.Background()

	if sender.Configured(ctx) {
		t.Fatal("缺 from_email 时 Configured 必须为 false")
	}
	if err := sender.SendEmailCode(ctx, "user@example.com", "register", "123456"); !errors.Is(err, ErrEmailUnavailable) {
		t.Fatalf("SendEmailCode err = %v，期望 ErrEmailUnavailable", err)
	}
	if err := sender.SendPasswordReset(ctx, "user@example.com", "https://x/reset?token=t"); !errors.Is(err, ErrEmailUnavailable) {
		t.Fatalf("SendPasswordReset err = %v，期望 ErrEmailUnavailable", err)
	}
	if err := sender.SendTestNotification(ctx, "email", "admin@example.com"); !errors.Is(err, ErrEmailUnavailable) {
		t.Fatalf("SendTestNotification err = %v，期望 ErrEmailUnavailable", err)
	}
	server.assertSilent(t)
}

// TestEmailDeliveryConfiguredFailsClosedWithoutProbe（M-5）：未实现
// Configured 的发送器不得被当作可用（旧实现 fail-open 返回 true）。
func TestEmailDeliveryConfiguredFailsClosedWithoutProbe(t *testing.T) {
	type bareSender struct{}
	if emailDeliveryConfigured(context.Background(), bareSender{}) {
		t.Fatal("未实现 Configured 的发送器必须按未配置处理（fail-closed）")
	}
	if emailDeliveryConfigured(context.Background(), nil) {
		t.Fatal("nil 发送器必须按未配置处理")
	}
	if !emailDeliveryConfigured(context.Background(), NewTransactionalEmailSender(configuredMailer(t, newFakeSMTPServer(t)))) {
		t.Fatal("已配置的 TransactionalEmailSender 必须判定为可用")
	}
}

// TestTransactionalEmailSenderNormalizesDeliveryErrors（Important-1）：适配器
// 边界必须把底层 SMTP 错误归一化为唯一哨兵 ErrEmailDeliveryFailed。用例直接
// 断言 `send` 的返回值，而不是 handler 日志——把 `send` 改回 `%w` 透传时，
// errors.Is 与「文本不含地址/550/@」会同时变红，安全不变量因此被钉住。
func TestTransactionalEmailSenderNormalizesDeliveryErrors(t *testing.T) {
	const victim = "victim@example.com"
	server := newFakeSMTPServer(t)
	server.rejectRCPT = "550 5.1.1 <" + victim + ">: Recipient address rejected"
	sender := NewTransactionalEmailSender(configuredMailer(t, server))
	ctx := context.Background()

	configured := []struct {
		name string
		send func() error
	}{
		{"SendEmailCode", func() error {
			return sender.SendEmailCode(ctx, victim, "register", "123456")
		}},
		{"SendPasswordReset", func() error {
			return sender.SendPasswordReset(ctx, victim, "https://app.example.com/reset-password?token=t")
		}},
		{"SendTestNotification", func() error {
			return sender.SendTestNotification(ctx, "email", victim)
		}},
	}
	for _, tc := range configured {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.send()
			if err == nil {
				t.Fatal("RCPT 被 550 拒绝，必须返回错误")
			}
			if !errors.Is(err, ErrEmailDeliveryFailed) {
				t.Fatalf("err = %v，期望 errors.Is(ErrEmailDeliveryFailed)", err)
			}
			// 固定分类必须原样返回：任何包装（哪怕只是加常量前缀）都可能
			// 在后续演进中重新夹带上游回显，这里把边界钉死。
			if err.Error() != ErrEmailDeliveryFailed.Error() {
				t.Fatalf("错误文本未被归一化：%q", err.Error())
			}
			for _, banned := range []string{victim, "@", "550", "Recipient address rejected"} {
				if strings.Contains(err.Error(), banned) {
					t.Fatalf("错误文本泄露 %q：%q", banned, err.Error())
				}
			}
		})
	}

	// 未配置路径必须仍是 ErrEmailUnavailable，且绝不能同时被归到
	// delivery_failed：503（配置缺失，重试无用）与 502（可重试）是客户端/告警
	// 依赖的两个不同分类，不能混。
	unconfigured := NewTransactionalEmailSender(unconfiguredMailer())
	notConfigured := []struct {
		name string
		send func() error
	}{
		{"SendEmailCode", func() error {
			return unconfigured.SendEmailCode(ctx, victim, "register", "123456")
		}},
		{"SendPasswordReset", func() error {
			return unconfigured.SendPasswordReset(ctx, victim, "https://x/reset?token=t")
		}},
		{"SendTestNotification", func() error {
			return unconfigured.SendTestNotification(ctx, "email", victim)
		}},
	}
	for _, tc := range notConfigured {
		t.Run(tc.name+"_unconfigured", func(t *testing.T) {
			err := tc.send()
			if !errors.Is(err, ErrEmailUnavailable) {
				t.Fatalf("err = %v，期望 ErrEmailUnavailable", err)
			}
			if errors.Is(err, ErrEmailDeliveryFailed) {
				t.Fatal("未配置路径不得被归为 ErrEmailDeliveryFailed")
			}
			if strings.Contains(err.Error(), "@") {
				t.Fatalf("未配置错误文本泄露地址：%q", err.Error())
			}
		})
	}
}

// TestEmailErrorClass（Minor-2）：三种固定分类都必须被断言；任意未知错误只能
// 落到 delivery_failed，绝不能落入 not_configured / channel_unavailable。
func TestEmailErrorClass(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"not_configured", ErrEmailUnavailable, "not_configured"},
		{"not_configured_wrapped", fmt.Errorf("probe: %w", ErrEmailUnavailable), "not_configured"},
		{"channel_unavailable", ErrNotificationChannelUnavailable, "channel_unavailable"},
		{"channel_unavailable_wrapped", fmt.Errorf("probe: %w", ErrNotificationChannelUnavailable), "channel_unavailable"},
		{"delivery_failed", ErrEmailDeliveryFailed, "delivery_failed"},
		{"arbitrary_error", errors.New("550 5.1.1 <victim@example.com>"), "delivery_failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := emailErrorClass(tc.err); got != tc.want {
				t.Fatalf("emailErrorClass(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// TestEmailCodeFailClosedBeforeIssue（M-1）：fail-closed 必须发生在
// codes.issue **之前**。旧用例只断言 Redis 键不存在，而失败分支的
// `codes.discard` 会把生成后留下的键同步删掉，所以把可用性检查移到 issue
// 之后时旧用例仍然绿——它只证明了「不留残余码」。这里用可注入接缝计数
// issue 调用次数，直接钉住顺序。
func TestEmailCodeFailClosedBeforeIssue(t *testing.T) {
	env := newEmailEndpointEnv(t, unconfiguredMailer(), nil)
	const email = "user@example.com"

	var issues atomic.Int64
	env.router.handlers.issueEmailCode = func(ctx context.Context, email, purpose string) (string, error) {
		issues.Add(1)
		return env.router.handlers.codes.issue(ctx, email, purpose)
	}

	rec := env.post("/api/auth/email-code",
		map[string]any{"email": email, "purpose": "register"})
	assertProblemType(t, rec, "email_unavailable")

	if n := issues.Load(); n != 0 {
		t.Fatalf("未配置时必须在 issue 之前返回，实际 issue 调用 %d 次", n)
	}
	// 辅助断言：即使实现将来改成「生成后失败也保留」，也不得留下无人收得到的码。
	if n, err := env.rdb.Exists(context.Background(), "email_code:register:"+email).Result(); err != nil || n != 0 {
		t.Fatalf("未配置时不得生成验证码：exists=%d err=%v", n, err)
	}
}

// TestEmailCodeFailsClosedBeforeCooldownAndQuota（M-4）：邮件可用性检查在
// 冷却键与 IP 桶之前；未配置期间不得消耗 60s 冷却/配额。
func TestEmailCodeFailsClosedBeforeCooldownAndQuota(t *testing.T) {
	env := newEmailEndpointEnv(t, unconfiguredMailer(), nil)
	const email = "user@example.com"
	ctx := context.Background()

	rec := env.post("/api/auth/email-code",
		map[string]any{"email": email, "purpose": "register"})
	assertProblemType(t, rec, "email_unavailable")

	if n, err := env.rdb.Exists(ctx, "email_code:cooldown:"+sha256Hex(email)).Result(); err != nil || n != 0 {
		t.Fatalf("未配置时不得写冷却键：exists=%d err=%v", n, err)
	}
	keys, err := env.rdb.Keys(ctx, "email_code:ip:*").Result()
	if err != nil {
		t.Fatalf("list ip keys: %v", err)
	}
	if len(keys) != 0 {
		t.Fatalf("未配置时不得消耗 IP 桶，实际键：%v", keys)
	}
}

// TestNotificationTestChannelUnavailableBeforeQuota（M-3）：本部署不支持的
// 渠道在配额 INCR 之前就 503，且分类是 channel_unavailable（不是 email_unavailable）；
// nil 发信器只影响 email 渠道。
func TestNotificationTestChannelUnavailableBeforeQuota(t *testing.T) {
	env := newAccountTestEnv(t)
	user := env.addUser("alice", "alice@example.com", false)
	sid, csrf := env.sessionWithCSRF(user.ID)
	ctx := context.Background()
	quotaKey := notificationTestKeyPrefix + strconv.FormatInt(user.ID, 10)

	env.handler.sender = NewTransactionalEmailSender(unconfiguredMailer())
	for channel, target := range map[string]string{
		"webhook": "https://example.com/hook",
		"bark":    "https://example.com/bark",
		"gotify":  "https://example.com/gotify",
	} {
		rec := env.authedRequest(http.MethodPost, "/api/notification-preferences/test",
			map[string]any{"channel": channel, "target": target}, sid, csrf)
		assertProblemType(t, rec, "notification_channel_unavailable")
		if n, err := env.rdb.Exists(ctx, quotaKey).Result(); err != nil || n != 0 {
			t.Fatalf("%s 不应消耗配额：exists=%d err=%v", channel, n, err)
		}
	}

	// nil 发信器：非 email 渠道仍是自己的分类，email 才是 email_unavailable。
	env.handler.sender = nil
	rec := env.authedRequest(http.MethodPost, "/api/notification-preferences/test",
		map[string]any{"channel": "webhook", "target": "https://example.com/hook"}, sid, csrf)
	assertProblemType(t, rec, "notification_channel_unavailable")
	rec = env.authedRequest(http.MethodPost, "/api/notification-preferences/test",
		map[string]any{"channel": "email"}, sid, csrf)
	assertProblemType(t, rec, "email_unavailable")

	if n, err := env.rdb.Exists(ctx, quotaKey).Result(); err != nil || n != 0 {
		t.Fatalf("不可用渠道不得消耗配额：exists=%d err=%v", n, err)
	}
}

// TestEmailDeliveryErrorsDoNotLeakRecipient（I-1）：SMTP RCPT 回显含收件
// 地址时，email-code / password-reset / notification-test 的日志与响应体
// 都不得出现该地址或任何 `@`。
func TestEmailDeliveryErrorsDoNotLeakRecipient(t *testing.T) {
	const victim = "victim@example.com"

	// --- email-code + password-reset ---
	server := newFakeSMTPServer(t)
	server.rejectRCPT = "550 5.1.1 <" + victim + ">: Recipient address rejected"
	env := newEmailEndpointEnv(t, configuredMailer(t, server), nil)
	env.repo.add(UserRecord{Username: "victim", Email: victim, PasswordHash: "x"})

	rec := env.post("/api/auth/email-code",
		map[string]any{"email": victim, "purpose": "register"})
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("email-code = %d, want 502（body=%q）", rec.Code, rec.Body.String())
	}
	assertNoRecipientLeak(t, "email-code response", rec.Body.String())
	waitForLog(t, env.logs, "send email code failed")
	assertNoRecipientLeak(t, "email-code log", env.logs.String())

	// 密码重置异步投递；worker 只记固定分类 + 计数。
	rec = env.post("/api/auth/password-reset/request", map[string]any{"email": victim})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("password-reset = %d, want 204（body=%q）", rec.Code, rec.Body.String())
	}
	waitForLog(t, env.logs, "password reset email delivery failed")
	// Minor-4：异步 worker 行必须带固定 error_class（与另外两个端点同口径）。
	// 按行匹配，避免同一 buffer 里 email-code 的同类日志误满足断言。
	resetLine := logLineContaining(env.logs.String(), "password reset email delivery failed")
	if !strings.Contains(resetLine, "error_class=delivery_failed") {
		t.Fatalf("password-reset worker 日志缺少固定分类：%q", resetLine)
	}
	assertNoRecipientLeak(t, "password-reset log", env.logs.String())

	// --- notification-test ---
	acct := newAccountTestEnv(t)
	user := acct.addUser("victim", victim, false)
	sid, csrf := acct.sessionWithCSRF(user.ID)
	logBuf := &syncBuffer{}
	acct.handler.log = slog.New(slog.NewTextHandler(logBuf, nil))
	acct.handler.sender = NewTransactionalEmailSender(configuredMailer(t, server))

	rec = acct.authedRequest(http.MethodPost, "/api/notification-preferences/test",
		map[string]any{"channel": "email", "target": victim}, sid, csrf)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("notification-test = %d, want 502（body=%q）", rec.Code, rec.Body.String())
	}
	assertNoRecipientLeak(t, "notification-test response", rec.Body.String())
	assertNoRecipientLeak(t, "notification-test log", logBuf.String())
}

func assertNoRecipientLeak(t *testing.T, where, text string) {
	t.Helper()
	if strings.Contains(text, "victim@example.com") {
		t.Fatalf("%s 泄露收件地址：%q", where, text)
	}
	if strings.Contains(text, "@") {
		t.Fatalf("%s 出现任意 @（不得回显地址）：%q", where, text)
	}
}
