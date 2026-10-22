//go:build integration

package platform

// W13-EMAIL 装配验收：生产 newGatewayAPI 路径必须注入事务性邮件适配器，
// 并且三个 handler 接缝实际走到该适配器与配置中心的 notify.smtp.* 快照。
//
// I-2 复核修复：原用例只断言 assembly.EmailSender != nil，删掉
// platform/api.go 的 GatewayDeps.EmailSender 注入后仍然绿（nil 发信器与
// 未配置同形，都 503）。现在在设置中心写入指向**进程内 fake SMTP** 的
// notify.smtp.*，断言 email-code / password-reset / notification-test
// 三个端点各自真正投递一封；删除注入会让「实际收到一封」变红。
//
// 运行（测试库按任务约定自建 newtg_dom_w13e / newtg_dom_w13e_logs）：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_w13e?sslmode=disable' \
//	NEWTG_TEST_LOGS_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_w13e_logs?sslmode=disable' \
//	NEWTG_TEST_REDIS_ADDR=localhost:56379 \
//	go test -tags=integration -p 1 ./internal/platform/ -run TestIntegrationGatewayEmailAssembly -v

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/config"
	"newtg/gateway/internal/configcenter"
	"newtg/gateway/internal/httpapi"
)

func TestIntegrationGatewayEmailAssembly(t *testing.T) {
	dsn := os.Getenv("NEWTG_TEST_DB_DSN")
	logsDSN := os.Getenv("NEWTG_TEST_LOGS_DSN")
	redisAddr := os.Getenv("NEWTG_TEST_REDIS_ADDR")
	if dsn == "" || logsDSN == "" || redisAddr == "" {
		t.Skip("NEWTG_TEST_DB_DSN / NEWTG_TEST_LOGS_DSN / NEWTG_TEST_REDIS_ADDR 未设置，跳过邮件装配集成测试")
	}

	mainDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open main db: %v", err)
	}
	t.Cleanup(func() { _ = mainDB.Close() })
	logsDB, err := sql.Open("pgx", logsDSN)
	if err != nil {
		t.Fatalf("open logs db: %v", err)
	}
	t.Cleanup(func() { _ = logsDB.Close() })
	if err := mainDB.Ping(); err != nil {
		t.Fatalf("ping main db: %v", err)
	}
	if err := logsDB.Ping(); err != nil {
		t.Fatalf("ping logs db: %v", err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: redisAddr})
	t.Cleanup(func() { _ = rdb.Close() })
	ctx := context.Background()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Fatalf("ping redis: %v", err)
	}

	cfg := &config.Config{
		SessionSecret:           "w13-email-session-secret",
		CredentialEncryptionKey: "w13-email-credential-key",
		PublicBaseURL:           "http://localhost:3000",
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	assembly, err := newGatewayAPI(ctx, cfg, mainDB, logsDB, rdb, logger)
	if err != nil {
		t.Fatalf("newGatewayAPI: %v", err)
	}
	if assembly == nil || assembly.Handler == nil {
		t.Fatal("assembly/handler = nil, want the management router")
	}
	t.Cleanup(assembly.AttemptFailureReporterStop)

	// 装配断言：适配器实例存在（I-2 的正向投递断言才是真正的注入证明）。
	if assembly.EmailSender == nil {
		t.Fatal("assembly.EmailSender = nil, want the transactional email adapter")
	}
	center, ok := assembly.Config.(*configcenter.Center)
	if !ok {
		t.Fatalf("assembly.Config 类型 = %T, want *configcenter.Center（集成测试需要写设置中心）", assembly.Config)
	}

	// 同一隔离库会被重复运行：先清空 SMTP 配置，保证基线用例可重复；
	// 结束后再清空，避免把 fake 端口留给下一次运行。
	clearSMTPSettings(t, ctx, center)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		clearSMTPSettings(t, cleanupCtx, center)
	})

	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	// 集成 Redis 是跨用例/跨运行共享的；验证码与重置的 IP 桶按真实
	// RemoteAddr 计数，固定 IP 会让同一隔离库的第二次运行撞上 10 次/小时桶。
	// 每次运行取一个文档段（198.51.100.0/24）内的独立地址。
	probeIP := "198.51.100." + strconv.FormatInt(time.Now().UnixNano()%250+1, 10) + ":12345"
	post := func(path, body string) (*httptest.ResponseRecorder, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = probeIP
		rec := httptest.NewRecorder()
		assembly.Handler.ServeHTTP(rec, req)
		var problem map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &problem)
		return rec, problem
	}

	// 负向基线（未配置 SMTP）：验证码与密码重置必须 fail-closed 503。
	// 注意：删掉 api.go 的注入同样得到 503，因此本用例本身不能证明注入；
	// 下面「配置后实际投递」才是 I-2 的变异敏感断言。
	rec, problem := post("/api/auth/email-code",
		`{"email":"w13-`+suffix+`@example.com","purpose":"register"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured email-code = %d, want 503（body=%q）", rec.Code, rec.Body.String())
	}
	if got, _ := problem["type"].(string); !strings.HasSuffix(got, "/email_unavailable") {
		t.Fatalf("unconfigured email-code problem.type = %v, want email_unavailable", problem["type"])
	}
	rec, problem = post("/api/auth/password-reset/request",
		`{"email":"nobody-`+suffix+`@example.com"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured password-reset = %d, want 503（body=%q）", rec.Code, rec.Body.String())
	}
	if got, _ := problem["type"].(string); !strings.HasSuffix(got, "/email_unavailable") {
		t.Fatalf("unconfigured password-reset problem.type = %v, want email_unavailable", problem["type"])
	}

	// ---- I-2：设置中心指向进程内 fake SMTP，三个接缝都要真实投递 ----
	smtp := newInProcessSMTPServer(t)
	configureSMTP(t, ctx, center, smtp)

	// (a) email-code：同步投递。
	email := "w13-" + suffix + "@example.com"
	rec, _ = post("/api/auth/email-code",
		`{"email":"`+email+`","purpose":"register"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("configured email-code = %d, want 204（body=%q）", rec.Code, rec.Body.String())
	}
	if msg := smtp.next(t); !strings.Contains(msg, "Your verification code is: ") {
		t.Fatalf("email-code 投递内容不符：%q", msg)
	}

	// (b) password-reset：需要真实已注册邮箱（未注册时会静默 204 不发信）。
	userID := createEmailIntegrationUser(t, mainDB, email)
	rec, _ = post("/api/auth/password-reset/request", `{"email":"`+email+`"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("configured password-reset = %d, want 204（body=%q）", rec.Code, rec.Body.String())
	}
	if msg := smtp.next(t); !strings.Contains(msg, "reset-password?token=") {
		t.Fatalf("password-reset 投递内容不符：%q", msg)
	}

	// (c) notification-test：受保护端点，用与装配同一个 SESSION_SECRET/Redis
	// 构造会话与 CSRF（等价于真实登录后的会话状态）。
	sessions := auth.NewSessionManager(rdb, []byte(cfg.SessionSecret))
	sid, err := sessions.Create(ctx, userID, auth.SessionOpts{
		UA: "W13EmailIntegration/1.0", IP: "203.0.113.20", Role: "user",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	t.Cleanup(func() { _ = sessions.Revoke(context.Background(), sid) })
	csrf := auth.NewCSRFToken()
	if err := sessions.IssueCSRF(ctx, sid, csrf); err != nil {
		t.Fatalf("issue csrf: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/notification-preferences/test",
		strings.NewReader(`{"channel":"email","target":"`+email+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(auth.CSRFHeaderName, csrf)
	req.Header.Set("Origin", cfg.PublicBaseURL)
	req.RemoteAddr = probeIP
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: sid})
	rec = httptest.NewRecorder()
	assembly.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("configured notification-test = %d, want 200（body=%q）", rec.Code, rec.Body.String())
	}
	if msg := smtp.next(t); !strings.Contains(msg, "test email from your gateway") {
		t.Fatalf("notification-test 投递内容不符：%q", msg)
	}
}

// clearSMTPSettings 把 notify.smtp 的关键键写空，使「未配置」基线可重复。
// username 也清空：notify 层在 from_email 为空时回退 username（G7-R41），
// 残留 username 会让干净库意外变成「已配置」。
func clearSMTPSettings(t *testing.T, ctx context.Context, center *configcenter.Center) {
	t.Helper()
	for _, key := range []string{
		"notify.smtp.host", "notify.smtp.username", "notify.smtp.from_email",
	} {
		if err := center.Save(ctx, key, "", nil); err != nil {
			t.Fatalf("clear %s: %v", key, err)
		}
	}
}

// configureSMTP 把设置中心指向进程内 fake SMTP。
func configureSMTP(t *testing.T, ctx context.Context, center *configcenter.Center, smtp *inProcessSMTPServer) {
	t.Helper()
	host, portText, err := net.SplitHostPort(smtp.ln.Addr().String())
	if err != nil {
		t.Fatalf("split fake smtp addr: %v", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("parse fake smtp port: %v", err)
	}
	for key, value := range map[string]any{
		"notify.smtp.host":       host,
		"notify.smtp.port":       port,
		"notify.smtp.encryption": "none",
		"notify.smtp.from_email": "noreply@example.com",
	} {
		if err := center.Save(ctx, key, value, nil); err != nil {
			t.Fatalf("save %s: %v", key, err)
		}
	}
}

// createEmailIntegrationUser 建一个带邮箱的真实用户（与注册同一写路径）。
func createEmailIntegrationUser(t *testing.T, db *sql.DB, email string) int64 {
	t.Helper()
	hash, err := auth.HashPassword("integration-correct-horse")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	created, err := httpapi.NewSQLAuthRepo(db).CreateUser(context.Background(), httpapi.CreateUserInput{
		Username:      "w13e_" + strconv.FormatInt(time.Now().UnixNano(), 36),
		Email:         email,
		PasswordHash:  hash,
		EmailVerified: true,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() { cleanupIntegrationUser(t, db, created.ID) })
	return created.ID
}

// ---------------------------------------------------------------------------
// 进程内 fake SMTP（只实现本用例需要的最小会话）
// ---------------------------------------------------------------------------

type inProcessSMTPServer struct {
	ln   net.Listener
	msgs chan string
	wg   sync.WaitGroup
}

func newInProcessSMTPServer(t *testing.T) *inProcessSMTPServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen fake smtp: %v", err)
	}
	s := &inProcessSMTPServer{ln: ln, msgs: make(chan string, 16)}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
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
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		s.wg.Wait()
	})
	return s
}

func (s *inProcessSMTPServer) handle(conn net.Conn) {
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

// next 取下一封已接收邮件（3s 超时；密码重置走异步队列）。
func (s *inProcessSMTPServer) next(t *testing.T) string {
	t.Helper()
	select {
	case msg := <-s.msgs:
		return msg
	case <-time.After(3 * time.Second):
		t.Fatal("3s 内未收到邮件")
		return ""
	}
}
