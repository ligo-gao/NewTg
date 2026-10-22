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
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
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
	wg   sync.WaitGroup
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
		case strings.HasPrefix(command, "MAIL FROM"), strings.HasPrefix(command, "RCPT TO"):
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
	logs     *bytes.Buffer
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
	logs := &bytes.Buffer{}
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
