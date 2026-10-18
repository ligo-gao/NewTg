package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/auth"
)

// 本文件是 Task 7 的 HTTP 层单元验收，覆盖 brief (a)–(f)：
//
//	(a) ±0 窗口——领域层在 auth/totp_test.go 断言；此处用 RFC 6238 已知向量
//	    驱动端点，保证 handler 传入的是服务端注入时钟。
//	(b) 备用码一次性、并发同一码恰一成功、只存 argon2id 哈希。
//	(c) enable 验证失败不落库（TC-F-01-10-02）。
//	(d) disable 需要口令（TC-F-01-10-03）。
//	(e) regenerate 后旧码全部失效（TC-F-01-10-04）。
//	(f) challenge 5 次失败作废 ticket（TC-F-01-08-02）。
//
// 全部依赖用 miniredis + fakeTwoFARepo；未打 integration 标签时不触碰外部服务（GC-8）。

const (
	twoFATestPassword = "CorrectHorse9!"
	// RFC 6238 附录 B 的 SHA-1 测试密钥（ASCII "12345678901234567890" 的 base32）。
	twoFATestSecret = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	// T=59s 时该密钥的 6 位码（RFC 向量 94287082 的末 6 位）。
	twoFATestCode = "287082"
	// 测试用凭据加密主密钥；生产来自 CREDENTIAL_ENCRYPTION_KEY。
	twoFATestEncryptionKey = "task7-handler-test-encryption-key"
)

var (
	twoFATestNow = time.Unix(59, 0)

	// backupCodeShape 与 modules-01 F-01-09 的字段正则同口径。
	backupCodeShape = regexp.MustCompile(`^[A-Z0-9]{4}-[A-Z0-9]{4}$`)

	twoFATestPasswordHash = sync.OnceValue(func() string {
		h, err := auth.HashPassword(twoFATestPassword)
		if err != nil {
			panic(err)
		}
		return h
	})
)

func readFullRand(buf []byte) (int, error) { return rand.Read(buf) }

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// fakeTwoFARepo 是 TwoFARepo 的内存实现。ConsumeBackupCode 的临界区模拟
// 「条件 UPDATE」：只有数组中仍含 matchedHash 时才替换，并发下恰一成功。
type fakeTwoFARepo struct {
	mu             sync.Mutex
	users          map[int64]*TwoFAUser
	nextID         int64
	enableAttempts int
}

func newFakeTwoFARepo() *fakeTwoFARepo {
	return &fakeTwoFARepo{users: map[int64]*TwoFAUser{}}
}

// add 登记一个用户；返回的副本供断言（仓储内部仍持有权威状态）。
func (r *fakeTwoFARepo) add(u TwoFAUser) *TwoFAUser {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	u.ID = r.nextID
	if u.Role == "" {
		u.Role = "user"
	}
	if u.Status == "" {
		u.Status = "enabled"
	}
	if u.BackupCodes == nil {
		u.BackupCodes = []string{}
	}
	r.users[u.ID] = cloneTwoFAUser(&u)
	return cloneTwoFAUser(&u)
}

func (r *fakeTwoFARepo) get(userID int64) *TwoFAUser {
	r.mu.Lock()
	defer r.mu.Unlock()
	return cloneTwoFAUser(r.users[userID])
}

func cloneTwoFAUser(u *TwoFAUser) *TwoFAUser {
	if u == nil {
		return nil
	}
	cp := *u
	cp.BackupCodes = append([]string(nil), u.BackupCodes...)
	return &cp
}

func (r *fakeTwoFARepo) FindUserByID(_ context.Context, userID int64) (*TwoFAUser, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return cloneTwoFAUser(r.users[userID]), nil
}

func (r *fakeTwoFARepo) EnableTwoFA(_ context.Context, userID int64, secretEnc string, backupHashes []string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.enableAttempts++
	u := r.users[userID]
	if u == nil || u.TOTPEnabled {
		return false, nil
	}
	u.TOTPSecretEnc = secretEnc
	u.BackupCodes = append([]string(nil), backupHashes...)
	u.TOTPEnabled = true
	return true, nil
}

func (r *fakeTwoFARepo) DisableTwoFA(_ context.Context, userID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if u := r.users[userID]; u != nil {
		u.TOTPSecretEnc = ""
		u.BackupCodes = nil
		u.TOTPEnabled = false
	}
	return nil
}

func (r *fakeTwoFARepo) ReplaceBackupCodes(_ context.Context, userID int64, hashes []string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	u := r.users[userID]
	if u == nil || !u.TOTPEnabled {
		return false, nil
	}
	u.BackupCodes = append([]string(nil), hashes...)
	return true, nil
}

func (r *fakeTwoFARepo) ConsumeBackupCode(_ context.Context, userID int64, matchedHash string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	u := r.users[userID]
	if u == nil || !u.TOTPEnabled {
		return false, nil
	}
	// 模拟 SQL 的 `backup_codes - matchedHash`：对**当前值**增量删除，
	// 不是用调用方预读快照覆盖（那会复现已修复的丢失更新）。
	found := false
	next := make([]string, 0, len(u.BackupCodes))
	for _, h := range u.BackupCodes {
		if h == matchedHash {
			found = true
			continue
		}
		next = append(next, h)
	}
	if !found {
		// 条件 UPDATE 的 WHERE 不成立：并发已被消费或码已重生成。
		return false, nil
	}
	u.BackupCodes = next
	return true, nil
}

// fakeTwoFAAuditExec 捕获 audit.Logger 的 INSERT 实参；实参顺序与
// audit.insertSQL 的列顺序一致（审计包测试文件冻结同一契约）。
type fakeTwoFAAuditExec struct {
	mu    sync.Mutex
	calls [][]any
}

func (f *fakeTwoFAAuditExec) ExecContext(_ context.Context, _ string, args ...any) (sql.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]any(nil), args...))
	return nil, nil
}

func (f *fakeTwoFAAuditExec) snapshot() [][]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]any, len(f.calls))
	copy(out, f.calls)
	return out
}

// ---------------------------------------------------------------------------
// 测试环境
// ---------------------------------------------------------------------------

type twoFATestEnv struct {
	t         *testing.T
	router    http.Handler
	repo      *fakeTwoFARepo
	rdb       *redis.Client
	mr        *miniredis.Miniredis
	sessions  *auth.SessionManager
	handlers  *TwoFAHandlers
	auditExec *fakeTwoFAAuditExec
}

// newTwoFATestEnv 按 routes.go 的结构装配：challenge 挂在公开 mux，
// 其余四个端点挂在 SessionAuth → CSRF → OriginReferer 链之后。
func newTwoFATestEnv(t *testing.T) *twoFATestEnv {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	sessions := auth.NewSessionManager(rdb, []byte(testSessionSecret))
	repo := newFakeTwoFARepo()
	auditExec := &fakeTwoFAAuditExec{}

	h, err := NewTwoFAHandlers(TwoFAConfig{
		Repo:          repo,
		Redis:         rdb,
		Sessions:      sessions,
		EncryptionKey: twoFATestEncryptionKey,
		Audit:         audit.New(auditExec),
		Now:           func() time.Time { return twoFATestNow },
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewTwoFAHandlers: %v", err)
	}

	protected := http.NewServeMux()
	root := http.NewServeMux()
	Register2FARoutes(root, protected, h)
	// 与 routes.go 同形：公开端点精确注册；其余 /api/* 受保护。
	root.Handle("/api/", Protect(sessions, nil, "")(protected))

	return &twoFATestEnv{
		t:         t,
		router:    root,
		repo:      repo,
		rdb:       rdb,
		mr:        mr,
		sessions:  sessions,
		handlers:  h,
		auditExec: auditExec,
	}
}

func (e *twoFATestEnv) request(method, path string, body any) *httptest.ResponseRecorder {
	e.t.Helper()
	var reader io.Reader = bytes.NewReader(nil)
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			e.t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return serve(e.router, req)
}

// newSession 建一个正式会话并签发 CSRF 令牌，返回两个凭据供 requestAs 使用。
func (e *twoFATestEnv) newSession(userID int64) (sid, csrf string) {
	e.t.Helper()
	sid, err := e.sessions.Create(context.Background(), userID, auth.SessionOpts{
		UA: "TwoFATest/1.0", IP: "192.0.2.20", Role: "user",
	})
	if err != nil {
		e.t.Fatalf("Create session: %v", err)
	}
	csrf = auth.NewCSRFToken()
	if err := e.sessions.IssueCSRF(context.Background(), sid, csrf); err != nil {
		e.t.Fatalf("IssueCSRF: %v", err)
	}
	return sid, csrf
}

// requestAs 用给定的会话/CSRF 凭据发起受保护请求。
func (e *twoFATestEnv) requestAs(method, path string, body any, sid, csrf string) *httptest.ResponseRecorder {
	e.t.Helper()
	var reader io.Reader = bytes.NewReader(nil)
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			e.t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(auth.CSRFHeaderName, csrf)
	req.Header.Set("Origin", "http://"+req.Host)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: sid})
	return serve(e.router, req)
}

// authedRequest 建一个带会话 Cookie、CSRF 头与同源 Origin 的请求。
func (e *twoFATestEnv) authedRequest(userID int64, method, path string, body any) *httptest.ResponseRecorder {
	sid, csrf := e.newSession(userID)
	return e.requestAs(method, path, body, sid, csrf)
}

// issueTicket 复刻 handlers_auth.go 的 createPending2FA 键格式（该函数私有于
// 另一个 handler 结构，测试按契约直接写 Redis）。
func (e *twoFATestEnv) issueTicket(userID int64) string {
	e.t.Helper()
	buf := make([]byte, 32)
	if _, err := readFullRand(buf); err != nil {
		e.t.Fatalf("generate ticket: %v", err)
	}
	ticket := base64.RawURLEncoding.EncodeToString(buf)
	key := twoFATicketKeyPrefix + ticket
	if err := e.rdb.Set(context.Background(), key, strconv.FormatInt(userID, 10), pending2FATTL).Err(); err != nil {
		e.t.Fatalf("store ticket: %v", err)
	}
	return ticket
}

func (e *twoFATestEnv) challenge(ticket, code, typ string) *httptest.ResponseRecorder {
	return e.request(http.MethodPost, "/api/auth/2fa/challenge", map[string]any{
		"ticket": ticket,
		"code":   code,
		"type":   typ,
	})
}

func (e *twoFATestEnv) ticketExists(ticket string) bool {
	n, err := e.rdb.Exists(context.Background(), twoFATicketKeyPrefix+ticket).Result()
	if err != nil {
		e.t.Fatalf("redis exists: %v", err)
	}
	return n == 1
}

// enableUser 走完整 enable 端点，返回明文备用码。
func (e *twoFATestEnv) enableUser(user *TwoFAUser) []string {
	e.t.Helper()
	rr := e.authedRequest(user.ID, http.MethodPost, "/api/2fa/enable", map[string]any{
		"secret": twoFATestSecret,
		"code":   twoFATestCode,
	})
	if rr.Code != http.StatusOK {
		e.t.Fatalf("enable status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	var resp twoFABackupCodesResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		e.t.Fatalf("decode enable response: %v", err)
	}
	return resp.BackupCodes
}

func decodeProblemTitle(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	var p problemBody
	if err := json.Unmarshal(rr.Body.Bytes(), &p); err != nil {
		t.Fatalf("problem 体不是合法 JSON: %v（body=%q）", err, rr.Body.String())
	}
	return p.Title
}

func twoFACookie(rr *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rr.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// (a) setup：secret + otpauth URI，不落库
// ---------------------------------------------------------------------------

func Test2FASetupReturnsSecretAndQR(t *testing.T) {
	env := newTwoFATestEnv(t)
	user := env.repo.add(TwoFAUser{
		Username: "alice", Email: "alice@example.com", PasswordHash: twoFATestPasswordHash(),
	})

	rr := env.authedRequest(user.ID, http.MethodPost, "/api/2fa/setup", map[string]any{})
	if rr.Code != http.StatusOK {
		t.Fatalf("setup status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	var resp twoFASetupResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode setup response: %v", err)
	}
	if resp.Secret == "" {
		t.Fatal("setup 必须返回 secret")
	}
	if len(resp.Secret) != 32 {
		t.Fatalf("secret 长度 = %d，期望 32", len(resp.Secret))
	}
	if want := "otpauth://totp/NewTG:alice?"; len(resp.OTPAuthURL) < len(want) || resp.OTPAuthURL[:len(want)] != want {
		t.Fatalf("otpauth_url = %q，期望以 %q 开头", resp.OTPAuthURL, want)
	}
	if !bytes.Contains([]byte(resp.OTPAuthURL), []byte("secret="+resp.Secret)) {
		t.Error("otpauth_url 必须携带同一个 secret")
	}
	// setup 不落库：状态保持未启用。
	if got := env.repo.get(user.ID); got.TOTPEnabled || got.TOTPSecretEnc != "" {
		t.Fatalf("setup 后状态 = %+v，期望未启用且无种子", got)
	}

	// 未认证请求必须被中间件拒绝。
	if rr := env.request(http.MethodPost, "/api/2fa/setup", map[string]any{}); rr.Code != http.StatusUnauthorized {
		t.Fatalf("未认证 setup status = %d，期望 401", rr.Code)
	}
}

// ---------------------------------------------------------------------------
// (c) enable：码错误不落库；正确码原子落库并返回 8 组备用码
// ---------------------------------------------------------------------------

func Test2FAEnableRejectsWrongCodeWithoutPersisting(t *testing.T) {
	env := newTwoFATestEnv(t)
	user := env.repo.add(TwoFAUser{Username: "bob", PasswordHash: twoFATestPasswordHash()})

	rr := env.authedRequest(user.ID, http.MethodPost, "/api/2fa/enable", map[string]any{
		"secret": twoFATestSecret,
		"code":   "000000", // T=59s 的有效码是 287082
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("错误码 enable status = %d，期望 400（body=%q）", rr.Code, rr.Body.String())
	}
	if title := decodeProblemTitle(t, rr); title != "Invalid verification code" {
		t.Errorf("problem title = %q", title)
	}
	if env.repo.enableAttempts != 0 {
		t.Fatalf("验证失败仍调用了 EnableTwoFA（%d 次）", env.repo.enableAttempts)
	}
	if got := env.repo.get(user.ID); got.TOTPEnabled || got.TOTPSecretEnc != "" || len(got.BackupCodes) != 0 {
		t.Fatalf("验证失败后用户状态 = %+v，期望完全未变", got)
	}
}

func Test2FAEnablePersistsEncryptedSecretAndHashes(t *testing.T) {
	env := newTwoFATestEnv(t)
	user := env.repo.add(TwoFAUser{Username: "carol", PasswordHash: twoFATestPasswordHash()})

	codes := env.enableUser(user)
	if len(codes) != 8 {
		t.Fatalf("备用码数量 = %d，期望 8", len(codes))
	}
	for _, code := range codes {
		if !backupCodeShape.MatchString(code) {
			t.Errorf("备用码 %q 不符合 XXXX-XXXX", code)
		}
	}

	got := env.repo.get(user.ID)
	if !got.TOTPEnabled {
		t.Fatal("enable 后 totp_enabled 应为 true")
	}
	if got.TOTPSecretEnc == "" || got.TOTPSecretEnc == twoFATestSecret {
		t.Fatalf("落库 secret = %q，期望 envelope 密文而非明文", got.TOTPSecretEnc)
	}
	if _, err := env.handlers.cipher.Decrypt(got.TOTPSecretEnc); err != nil {
		t.Fatalf("落库密文无法用同一密钥解密: %v", err)
	}
	if len(got.BackupCodes) != 8 {
		t.Fatalf("落库备用码哈希数 = %d，期望 8", len(got.BackupCodes))
	}
	for i, h := range got.BackupCodes {
		if !bytes.HasPrefix([]byte(h), []byte("$argon2id$")) {
			t.Errorf("备用码哈希[%d] = %q，期望 argon2id PHC", i, h)
		}
		if containsString(codes, h) {
			t.Errorf("备用码明文不得落库：%q", h)
		}
	}

	// 重复启用必须 409，且不得覆盖已落库状态。
	rr := env.authedRequest(user.ID, http.MethodPost, "/api/2fa/enable", map[string]any{
		"secret": twoFATestSecret, "code": twoFATestCode,
	})
	if rr.Code != http.StatusConflict {
		t.Fatalf("重复 enable status = %d，期望 409", rr.Code)
	}
}

// ---------------------------------------------------------------------------
// (f) challenge：TOTP 成功建会话；5 次失败作废 ticket
// ---------------------------------------------------------------------------

func Test2FAChallengeTOTPSuccessCreatesSession(t *testing.T) {
	env := newTwoFATestEnv(t)
	user := env.repo.add(TwoFAUser{Username: "dave", PasswordHash: twoFATestPasswordHash()})
	env.enableUser(user)

	ticket := env.issueTicket(user.ID)
	rr := env.challenge(ticket, twoFATestCode, "totp")
	if rr.Code != http.StatusOK {
		t.Fatalf("challenge status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	var resp authSessionResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode challenge response: %v", err)
	}
	if resp.User.ID != user.ID || resp.User.Username != "dave" {
		t.Fatalf("challenge user = %+v，期望 id=%d/username=dave", resp.User, user.ID)
	}
	if twoFACookie(rr, auth.SessionCookieName) == nil {
		t.Fatal("challenge 成功必须下发会话 Cookie")
	}
	if twoFACookie(rr, auth.CSRFCookieName) == nil {
		t.Fatal("challenge 成功必须下发 CSRF Cookie")
	}
	if env.ticketExists(ticket) {
		t.Fatal("challenge 成功后 ticket 必须被消费")
	}

	// 同一步进窗口内复用同一个码：必须拒绝（F-01-08 异常流程）。
	replayTicket := env.issueTicket(user.ID)
	rr = env.challenge(replayTicket, twoFATestCode, "totp")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("重复 TOTP status = %d，期望 400（body=%q）", rr.Code, rr.Body.String())
	}
}

func Test2FAChallengeFiveFailuresInvalidateTicket(t *testing.T) {
	env := newTwoFATestEnv(t)
	user := env.repo.add(TwoFAUser{Username: "erin", PasswordHash: twoFATestPasswordHash()})
	env.enableUser(user)
	ticket := env.issueTicket(user.ID)

	for i := 1; i <= 4; i++ {
		rr := env.challenge(ticket, "000000", "totp")
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("第 %d 次失败 status = %d，期望 400（body=%q）", i, rr.Code, rr.Body.String())
		}
		if !env.ticketExists(ticket) {
			t.Fatalf("第 %d 次失败后 ticket 不应被作废", i)
		}
		if c := twoFACookie(rr, auth.SessionCookieName); c != nil {
			t.Fatalf("第 %d 次失败不得下发会话 Cookie", i)
		}
	}

	rr := env.challenge(ticket, "000000", "totp")
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("第 5 次失败 status = %d，期望 429（body=%q）", rr.Code, rr.Body.String())
	}
	if env.ticketExists(ticket) {
		t.Fatal("第 5 次失败后 ticket 必须被作废")
	}

	// ticket 已不存在：后续请求按「无效/过期」400，而不是继续 429。
	if rr := env.challenge(ticket, twoFATestCode, "totp"); rr.Code != http.StatusBadRequest {
		t.Fatalf("作废 ticket 的后续请求 status = %d，期望 400", rr.Code)
	}
}

// ---------------------------------------------------------------------------
// (d) disable：必须口令确认
// ---------------------------------------------------------------------------

func Test2FADisableRequiresPassword(t *testing.T) {
	env := newTwoFATestEnv(t)
	user := env.repo.add(TwoFAUser{Username: "frank", PasswordHash: twoFATestPasswordHash()})
	env.enableUser(user)

	for name, body := range map[string]map[string]any{
		"错误口令": {"password": "wrong-password"},
		"空口令":  {"password": ""},
		"缺字段":  {},
	} {
		t.Run(name, func(t *testing.T) {
			rr := env.authedRequest(user.ID, http.MethodPost, "/api/2fa/disable", body)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d，期望 400（body=%q）", rr.Code, rr.Body.String())
			}
			if got := env.repo.get(user.ID); !got.TOTPEnabled {
				t.Fatal("口令确认失败不得禁用 2FA")
			}
		})
	}

	rr := env.authedRequest(user.ID, http.MethodPost, "/api/2fa/disable",
		map[string]any{"password": twoFATestPassword})
	if rr.Code != http.StatusOK {
		t.Fatalf("正确口令 disable status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	var resp twoFADisableResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode disable response: %v", err)
	}
	if resp.TOTPEnabled {
		t.Error("disable 响应 totp_enabled 应为 false")
	}
	if got := env.repo.get(user.ID); got.TOTPEnabled || got.TOTPSecretEnc != "" || len(got.BackupCodes) != 0 {
		t.Fatalf("disable 后状态 = %+v，期望全部清空", got)
	}
}

// ---------------------------------------------------------------------------
// (e) regenerate：旧码全部失效
// ---------------------------------------------------------------------------

func Test2FARegenerateInvalidatesAllOldCodes(t *testing.T) {
	env := newTwoFATestEnv(t)
	user := env.repo.add(TwoFAUser{Username: "grace", PasswordHash: twoFATestPasswordHash()})
	oldCodes := env.enableUser(user)

	// 错误口令不得改变备用码。
	if rr := env.authedRequest(user.ID, http.MethodPost, "/api/2fa/backup-codes/regenerate",
		map[string]any{"password": "wrong-password"}); rr.Code != http.StatusBadRequest {
		t.Fatalf("错误口令 regenerate status = %d，期望 400", rr.Code)
	}

	rr := env.authedRequest(user.ID, http.MethodPost, "/api/2fa/backup-codes/regenerate",
		map[string]any{"password": twoFATestPassword})
	if rr.Code != http.StatusOK {
		t.Fatalf("regenerate status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	var resp twoFABackupCodesResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode regenerate response: %v", err)
	}
	if len(resp.BackupCodes) != 8 {
		t.Fatalf("新备用码数量 = %d，期望 8", len(resp.BackupCodes))
	}
	for _, old := range oldCodes {
		if containsString(resp.BackupCodes, old) {
			// 随机碰撞概率 ~8/36^8；若发生，重生成语义仍以哈希替换为准。
			t.Logf("新旧备用码出现随机碰撞：%q", old)
		}
	}

	// 旧码必须全部失效。8 次错误会同时把登录失败桶打到锁定（5 次即锁），
	// 这本身是预期行为；解锁后再验证新码。
	for _, old := range oldCodes {
		ticket := env.issueTicket(user.ID)
		if rr := env.challenge(ticket, old, "backup"); rr.Code == http.StatusOK {
			t.Fatalf("旧备用码 %q 在重生成后仍然可用", old)
		}
	}
	env.mr.FastForward(LoginLockDuration + time.Second)

	// 新码必须可用（只验证第一组，避免把 8 组全部核销）。
	ticket := env.issueTicket(user.ID)
	if rr := env.challenge(ticket, resp.BackupCodes[0], "backup"); rr.Code != http.StatusOK {
		t.Fatalf("新备用码 challenge status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
}

// ---------------------------------------------------------------------------
// (b) 备用码：一次性 + 并发同一码恰一成功
// ---------------------------------------------------------------------------

func Test2FABackupCodeSingleUse(t *testing.T) {
	env := newTwoFATestEnv(t)
	user := env.repo.add(TwoFAUser{Username: "heidi", PasswordHash: twoFATestPasswordHash()})
	codes := env.enableUser(user)

	ticket := env.issueTicket(user.ID)
	if rr := env.challenge(ticket, codes[0], "backup"); rr.Code != http.StatusOK {
		t.Fatalf("首次备用码 status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}

	// 同一码第二次使用（新的合法 ticket）→ 400。
	ticket2 := env.issueTicket(user.ID)
	rr := env.challenge(ticket2, codes[0], "backup")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("重复备用码 status = %d，期望 400（body=%q）", rr.Code, rr.Body.String())
	}
	if title := decodeProblemTitle(t, rr); title != "Incorrect or already-used backup code" {
		t.Errorf("problem title = %q", title)
	}

	// 恰好移除一组哈希；其余 7 组保留。
	got := env.repo.get(user.ID)
	if len(got.BackupCodes) != 7 {
		t.Fatalf("核销后剩余哈希 = %d，期望 7", len(got.BackupCodes))
	}
}

func Test2FABackupCodeConcurrentSameCodeOnlyOneSucceeds(t *testing.T) {
	env := newTwoFATestEnv(t)
	user := env.repo.add(TwoFAUser{Username: "ivan", PasswordHash: twoFATestPasswordHash()})
	codes := env.enableUser(user)

	const workers = 6
	tickets := make([]string, workers)
	for i := range tickets {
		tickets[i] = env.issueTicket(user.ID)
	}

	var wg sync.WaitGroup
	statuses := make([]int, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			statuses[i] = env.challenge(tickets[i], codes[0], "backup").Code
		}(i)
	}
	wg.Wait()

	successes := 0
	for i, code := range statuses {
		switch code {
		case http.StatusOK:
			successes++
		case http.StatusBadRequest:
		default:
			t.Errorf("worker %d status = %d，期望 200 或 400", i, code)
		}
	}
	if successes != 1 {
		t.Fatalf("并发同一备用码成功次数 = %d，期望恰 1（statuses=%v）", successes, statuses)
	}
	if got := env.repo.get(user.ID); len(got.BackupCodes) != 7 {
		t.Fatalf("并发核销后剩余哈希 = %d，期望 7", len(got.BackupCodes))
	}
}

// Test2FAChallengeBackupRequiresEnabled 覆盖「禁用后旧码不可用」的防御面。
func Test2FAChallengeBackupRequiresEnabled(t *testing.T) {
	env := newTwoFATestEnv(t)
	user := env.repo.add(TwoFAUser{Username: "judy", PasswordHash: twoFATestPasswordHash()})
	codes := env.enableUser(user)

	if rr := env.authedRequest(user.ID, http.MethodPost, "/api/2fa/disable",
		map[string]any{"password": twoFATestPassword}); rr.Code != http.StatusOK {
		t.Fatalf("disable status = %d", rr.Code)
	}

	ticket := env.issueTicket(user.ID)
	rr := env.challenge(ticket, codes[0], "backup")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("禁用后旧备用码 status = %d，期望 400（body=%q）", rr.Code, rr.Body.String())
	}
	if twoFACookie(rr, auth.SessionCookieName) != nil {
		t.Fatal("禁用后旧备用码不得建立会话")
	}
}

// ---------------------------------------------------------------------------
// 输入校验
// ---------------------------------------------------------------------------

func Test2FAChallengeInputValidation(t *testing.T) {
	env := newTwoFATestEnv(t)
	user := env.repo.add(TwoFAUser{Username: "karl", PasswordHash: twoFATestPasswordHash()})
	env.enableUser(user)

	cases := []struct {
		name     string
		body     map[string]any
		wantType string
	}{
		{"缺 ticket", map[string]any{"code": twoFATestCode, "type": "totp"}, problemTypeTwoFATicketInvalid},
		{"畸形 ticket", map[string]any{"ticket": "not-a-ticket", "code": twoFATestCode, "type": "totp"}, problemTypeTwoFATicketInvalid},
		{"未知 type", map[string]any{"ticket": env.issueTicket(user.ID), "code": twoFATestCode, "type": "sms"}, authProblemBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := env.request(http.MethodPost, "/api/auth/2fa/challenge", tc.body)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d，期望 400（body=%q）", rr.Code, rr.Body.String())
			}
			if twoFACookie(rr, auth.SessionCookieName) != nil {
				t.Fatal("非法输入不得建立会话")
			}
			if p := decodeProblem(t, rr); p.Type != tc.wantType {
				t.Fatalf("problem.type = %q，期望 %q（body=%q）", p.Type, tc.wantType, rr.Body.String())
			}
		})
	}

	// 真·空 body（http.NoBody）：请求体解析失败，与 ticket 状态无关。
	emptyReq := httptest.NewRequest(http.MethodPost, "/api/auth/2fa/challenge", http.NoBody)
	emptyRR := serve(env.router, emptyReq)
	if emptyRR.Code != http.StatusBadRequest {
		t.Fatalf("空 body status = %d，期望 400（body=%q）", emptyRR.Code, emptyRR.Body.String())
	}
	if p := decodeProblem(t, emptyRR); p.Type != authProblemBadRequest {
		t.Fatalf("空 body problem.type = %q，期望 %q", p.Type, authProblemBadRequest)
	}

	// 过期/不存在的 ticket 与畸形 ticket 同一 type 与文案（无存在性探测信道）。
	expired := env.issueTicket(user.ID)
	env.mr.Del(twoFATicketKeyPrefix + expired)
	rr := env.challenge(expired, twoFATestCode, "totp")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("过期 ticket status = %d，期望 400", rr.Code)
	}
	if p := decodeProblem(t, rr); p.Type != problemTypeTwoFATicketInvalid ||
		p.Title != "Invalid or expired verification ticket" {
		t.Fatalf("过期 ticket problem = %+v", p)
	}

	// 已消费 ticket：同一 type（前端据此提示重新登录）。
	consumed := env.issueTicket(user.ID)
	ok, err := env.handlers.consumeTicket(context.Background(), consumed, strconv.FormatInt(user.ID, 10))
	if err != nil || !ok {
		t.Fatalf("consumeTicket = (%v, %v)，期望成功", ok, err)
	}
	rr = env.challenge(consumed, twoFATestCode, "totp")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("已消费 ticket status = %d，期望 400", rr.Code)
	}
	if p := decodeProblem(t, rr); p.Type != problemTypeTwoFATicketInvalid {
		t.Fatalf("已消费 ticket problem.type = %q，期望 %q", p.Type, problemTypeTwoFATicketInvalid)
	}

	// 错误 TOTP / 备用码仍是 bad_request（可就地重试，不是 ticket 失效）。
	for _, typ := range []string{"totp", "backup"} {
		rr := env.challenge(env.issueTicket(user.ID), "000000", typ)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("错误 %s 码 status = %d，期望 400", typ, rr.Code)
		}
		if p := decodeProblem(t, rr); p.Type != authProblemBadRequest {
			t.Fatalf("错误 %s 码 problem.type = %q，期望 %q", typ, p.Type, authProblemBadRequest)
		}
	}
}

// TestNewTwoFAHandlersRequiresDependencies 冻结装配期 fail-fast。
func TestNewTwoFAHandlersRequiresDependencies(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	sessions := auth.NewSessionManager(rdb, []byte(testSessionSecret))
	repo := newFakeTwoFARepo()

	cases := map[string]TwoFAConfig{
		"缺 Redis":    {Repo: repo, Sessions: sessions, EncryptionKey: "k"},
		"缺 Sessions": {Repo: repo, Redis: rdb, EncryptionKey: "k"},
		"缺 Repo/DB":  {Redis: rdb, Sessions: sessions, EncryptionKey: "k"},
		"缺密钥":        {Repo: repo, Redis: rdb, Sessions: sessions},
	}
	for name, cfg := range cases {
		if _, err := NewTwoFAHandlers(cfg); err == nil {
			t.Errorf("%s：NewTwoFAHandlers 应返回错误", name)
		}
	}
}

// TestRegister2FARoutesChallengeIsPublic 证明 challenge 不经过会话中间件：
// 无 Cookie 时返回业务 400（而不是 401）。
func TestRegister2FARoutesChallengeIsPublic(t *testing.T) {
	env := newTwoFATestEnv(t)
	rr := env.request(http.MethodPost, "/api/auth/2fa/challenge", map[string]any{
		"ticket": "invalid", "code": "000000", "type": "totp",
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("公开 challenge status = %d，期望 400（body=%q）", rr.Code, rr.Body.String())
	}
}

// ---------------------------------------------------------------------------
// 评审追加：状态守卫、限流、会话吊销/CSRF 轮换、审计、真实 ticket
// ---------------------------------------------------------------------------

// Test2FASetupRejectsEnabledAndDisabledAccounts：已启用 409；禁用账号即使
// 会话有效也不得调用任何 2FA 端点（四个端点共用 principalUser）。
func Test2FASetupRejectsEnabledAndDisabledAccounts(t *testing.T) {
	env := newTwoFATestEnv(t)

	enabled := env.repo.add(TwoFAUser{
		Username: "olga", PasswordHash: twoFATestPasswordHash(), TOTPEnabled: true,
	})
	if rr := env.authedRequest(enabled.ID, http.MethodPost, "/api/2fa/setup", map[string]any{}); rr.Code != http.StatusConflict {
		t.Fatalf("已启用 setup status = %d，期望 409（body=%q）", rr.Code, rr.Body.String())
	}

	disabled := env.repo.add(TwoFAUser{
		Username: "paul", PasswordHash: twoFATestPasswordHash(), Status: "disabled",
	})
	for name, path := range map[string]string{
		"setup":   "/api/2fa/setup",
		"disable": "/api/2fa/disable",
	} {
		t.Run(name, func(t *testing.T) {
			rr := env.authedRequest(disabled.ID, http.MethodPost, path, map[string]any{"password": twoFATestPassword})
			if rr.Code != http.StatusForbidden {
				t.Fatalf("禁用账号 status = %d，期望 403（body=%q）", rr.Code, rr.Body.String())
			}
		})
	}
}

// Test2FAEnableRejectsInvalidSecretShapesAndOAuthOnly 覆盖评审 Important 5：
// enable 只接受服务端冻结形状（32 个 base32 字符 / 20 字节），且 OAuth-only
// 用户不得启用（否则无法用口令确认来关闭）。
func Test2FAEnableRejectsInvalidSecretShapesAndOAuthOnly(t *testing.T) {
	env := newTwoFATestEnv(t)
	user := env.repo.add(TwoFAUser{Username: "quinn", PasswordHash: twoFATestPasswordHash()})

	for _, secret := range []string{
		"",
		"short",
		strings.Repeat("A", 31),
		strings.Repeat("A", 33),
		strings.Repeat("A", 30) + "!!",
		strings.Repeat("1", 32), // '1' 不是 RFC 4648 base32 字母表
	} {
		rr := env.authedRequest(user.ID, http.MethodPost, "/api/2fa/enable",
			map[string]any{"secret": secret, "code": twoFATestCode})
		if rr.Code != http.StatusBadRequest {
			t.Errorf("secret=%q status = %d，期望 400（body=%q）", secret, rr.Code, rr.Body.String())
		}
	}
	if env.repo.enableAttempts != 0 {
		t.Fatalf("非法 secret 仍触发落库（%d 次）", env.repo.enableAttempts)
	}

	oauth := env.repo.add(TwoFAUser{Username: "rita"}) // password_hash 为空
	rr := env.authedRequest(oauth.ID, http.MethodPost, "/api/2fa/enable",
		map[string]any{"secret": twoFATestSecret, "code": twoFATestCode})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("OAuth-only enable status = %d，期望 403（body=%q）", rr.Code, rr.Body.String())
	}
	if got := env.repo.get(oauth.ID); got.TOTPEnabled {
		t.Fatal("OAuth-only 用户不得启用 2FA")
	}
}

// Test2FAChallengeRateLimitedAfterFailures 覆盖评审 Important 1：挑战与登录
// 共用失败锁定桶；连续错码后即使提交正确码也返回 429，解锁后恢复。
func Test2FAChallengeRateLimitedAfterFailures(t *testing.T) {
	env := newTwoFATestEnv(t)
	user := env.repo.add(TwoFAUser{Username: "sam", PasswordHash: twoFATestPasswordHash()})
	env.enableUser(user)

	for i := 0; i < LoginMaxFailures; i++ {
		// 每次用新 ticket，把失败计数限定在登录桶（ticket 计数每次为 1）。
		if rr := env.challenge(env.issueTicket(user.ID), "000000", "totp"); rr.Code != http.StatusBadRequest {
			t.Fatalf("第 %d 次错码 status = %d，期望 400（body=%q）", i+1, rr.Code, rr.Body.String())
		}
	}
	if rr := env.challenge(env.issueTicket(user.ID), twoFATestCode, "totp"); rr.Code != http.StatusTooManyRequests {
		t.Fatalf("锁定后正确码 status = %d，期望 429（body=%q）", rr.Code, rr.Body.String())
	}

	env.mr.FastForward(LoginLockDuration + time.Second)
	if rr := env.challenge(env.issueTicket(user.ID), twoFATestCode, "totp"); rr.Code != http.StatusOK {
		t.Fatalf("解锁后正确码 status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
}

// Test2FADisablePasswordConfirmRateLimited：disable/regenerate 的口令确认
// 同样走登录桶，5 次错误口令后正确口令也被拒绝（429），且状态未被修改。
func Test2FADisablePasswordConfirmRateLimited(t *testing.T) {
	env := newTwoFATestEnv(t)
	user := env.repo.add(TwoFAUser{Username: "uma", PasswordHash: twoFATestPasswordHash()})
	env.enableUser(user)

	for i := 0; i < LoginMaxFailures; i++ {
		rr := env.authedRequest(user.ID, http.MethodPost, "/api/2fa/disable",
			map[string]any{"password": "wrong-password"})
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("第 %d 次错误口令 status = %d，期望 400（body=%q）", i+1, rr.Code, rr.Body.String())
		}
	}
	rr := env.authedRequest(user.ID, http.MethodPost, "/api/2fa/disable",
		map[string]any{"password": twoFATestPassword})
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("锁定后正确口令 status = %d，期望 429（body=%q）", rr.Code, rr.Body.String())
	}
	if got := env.repo.get(user.ID); !got.TOTPEnabled {
		t.Fatal("锁定期间不得禁用 2FA")
	}
}

// Test2FAStateChangeRevokesOtherSessionsAndRotatesCSRF 覆盖 I13 与 TC-A-04：
// enable/disable/regenerate 成功后吊销该用户其它会话、保留当前会话，并轮换
// CSRF 令牌（旧令牌立即失效）。
func Test2FAStateChangeRevokesOtherSessionsAndRotatesCSRF(t *testing.T) {
	ctx := context.Background()

	assertRevokedAndRotated := func(t *testing.T, env *twoFATestEnv, rr *httptest.ResponseRecorder, curSID, oldCSRF string) {
		t.Helper()
		newCSRF := ""
		if c := twoFACookie(rr, auth.CSRFCookieName); c != nil {
			newCSRF = c.Value
		}
		if newCSRF == "" || newCSRF == oldCSRF {
			t.Fatalf("2FA 变更后 CSRF 未轮换（cookie=%q）", newCSRF)
		}
		if env.sessions.VerifyCSRF(ctx, curSID, oldCSRF) {
			t.Fatal("旧 CSRF 令牌在轮换后仍然有效")
		}
		if !env.sessions.VerifyCSRF(ctx, curSID, newCSRF) {
			t.Fatal("新 CSRF 令牌无效")
		}
	}

	t.Run("enable", func(t *testing.T) {
		env := newTwoFATestEnv(t)
		user := env.repo.add(TwoFAUser{Username: "vera", PasswordHash: twoFATestPasswordHash()})
		curSID, curCSRF := env.newSession(user.ID)
		otherSID, _ := env.newSession(user.ID)

		rr := env.requestAs(http.MethodPost, "/api/2fa/enable", map[string]any{
			"secret": twoFATestSecret, "code": twoFATestCode,
		}, curSID, curCSRF)
		if rr.Code != http.StatusOK {
			t.Fatalf("enable status = %d（body=%q）", rr.Code, rr.Body.String())
		}
		if _, err := env.sessions.Get(ctx, otherSID); !errors.Is(err, auth.ErrSessionNotFound) {
			t.Fatalf("其它会话未被吊销：err=%v", err)
		}
		if _, err := env.sessions.Get(ctx, curSID); err != nil {
			t.Fatalf("当前会话被误吊销：%v", err)
		}
		assertRevokedAndRotated(t, env, rr, curSID, curCSRF)
	})

	t.Run("disable", func(t *testing.T) {
		env := newTwoFATestEnv(t)
		user := env.repo.add(TwoFAUser{Username: "wendy", PasswordHash: twoFATestPasswordHash()})
		env.enableUser(user)
		curSID, curCSRF := env.newSession(user.ID)
		otherSID, _ := env.newSession(user.ID)

		rr := env.requestAs(http.MethodPost, "/api/2fa/disable",
			map[string]any{"password": twoFATestPassword}, curSID, curCSRF)
		if rr.Code != http.StatusOK {
			t.Fatalf("disable status = %d（body=%q）", rr.Code, rr.Body.String())
		}
		if _, err := env.sessions.Get(ctx, otherSID); !errors.Is(err, auth.ErrSessionNotFound) {
			t.Fatalf("其它会话未被吊销：err=%v", err)
		}
		if _, err := env.sessions.Get(ctx, curSID); err != nil {
			t.Fatalf("当前会话被误吊销：%v", err)
		}
		assertRevokedAndRotated(t, env, rr, curSID, curCSRF)
	})

	t.Run("regenerate", func(t *testing.T) {
		env := newTwoFATestEnv(t)
		user := env.repo.add(TwoFAUser{Username: "xena", PasswordHash: twoFATestPasswordHash()})
		env.enableUser(user)
		curSID, curCSRF := env.newSession(user.ID)
		otherSID, _ := env.newSession(user.ID)

		rr := env.requestAs(http.MethodPost, "/api/2fa/backup-codes/regenerate",
			map[string]any{"password": twoFATestPassword}, curSID, curCSRF)
		if rr.Code != http.StatusOK {
			t.Fatalf("regenerate status = %d（body=%q）", rr.Code, rr.Body.String())
		}
		if _, err := env.sessions.Get(ctx, otherSID); !errors.Is(err, auth.ErrSessionNotFound) {
			t.Fatalf("其它会话未被吊销：err=%v", err)
		}
		if _, err := env.sessions.Get(ctx, curSID); err != nil {
			t.Fatalf("当前会话被误吊销：%v", err)
		}
		assertRevokedAndRotated(t, env, rr, curSID, curCSRF)
	})
}

func decodeTwoFAAuditDetail(t *testing.T, arg any) map[string]any {
	t.Helper()
	var raw []byte
	switch v := arg.(type) {
	case []byte:
		raw = v
	case string:
		raw = []byte(v)
	default:
		t.Fatalf("审计 detail 参数类型 = %T，期望 string/[]byte", arg)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("审计 detail 不是合法 JSON: %v（%q）", err, raw)
	}
	return m
}

// Test2FAAuditEvents 覆盖评审 Important 4：三种状态变更各写一条 security 审计。
func Test2FAAuditEvents(t *testing.T) {
	env := newTwoFATestEnv(t)
	user := env.repo.add(TwoFAUser{Username: "tina", PasswordHash: twoFATestPasswordHash()})

	env.enableUser(user)
	if rr := env.authedRequest(user.ID, http.MethodPost, "/api/2fa/disable",
		map[string]any{"password": twoFATestPassword}); rr.Code != http.StatusOK {
		t.Fatalf("disable status = %d（body=%q）", rr.Code, rr.Body.String())
	}
	env.enableUser(user)
	if rr := env.authedRequest(user.ID, http.MethodPost, "/api/2fa/backup-codes/regenerate",
		map[string]any{"password": twoFATestPassword}); rr.Code != http.StatusOK {
		t.Fatalf("regenerate status = %d（body=%q）", rr.Code, rr.Body.String())
	}

	calls := env.auditExec.snapshot()
	wantLabels := []string{"2fa_enabled", "2fa_disabled", "2fa_enabled", "backup_codes_regenerated"}
	wantActions := []string{
		audit.ActionAuth2FAEnable, audit.ActionAuth2FADisable,
		audit.ActionAuth2FAEnable, audit.ActionAuth2FABackupCodesRegenerate,
	}
	if len(calls) != len(wantLabels) {
		t.Fatalf("审计行数 = %d，期望 %d", len(calls), len(wantLabels))
	}
	for i, call := range calls {
		// 列序：user_id, username, category, ..., detail(索引 9)...
		if got := call[1]; got != "tina" {
			t.Errorf("第 %d 行 username = %v，期望 tina", i, got)
		}
		if got := call[2]; got != "security" {
			t.Errorf("第 %d 行 category = %v，期望 security", i, got)
		}
		detail := decodeTwoFAAuditDetail(t, call[9])
		// detail.action 由 writer 统一注入冻结动作名；人读标签在 detail.label。
		if detail["action"] != wantActions[i] {
			t.Errorf("第 %d 行 detail.action = %v，期望 %s", i, detail["action"], wantActions[i])
		}
		if detail["label"] != wantLabels[i] {
			t.Errorf("第 %d 行 detail.label = %v，期望 %s", i, detail["label"], wantLabels[i])
		}
	}
}

// Test2FAChallengeWithRealPendingTicket 用 handlers_auth.go 的真实
// createPending2FA（而不是测试自建键）签发 ticket，再走 challenge：
// 验证两个任务之间的键格式/值类型契约没有漂移。
func Test2FAChallengeWithRealPendingTicket(t *testing.T) {
	env := newTwoFATestEnv(t)
	user := env.repo.add(TwoFAUser{Username: "yara", PasswordHash: twoFATestPasswordHash()})
	env.enableUser(user)

	ah := newAuthHandlers(nil, env.sessions, env.rdb, nil, []byte(testSessionSecret),
		nil, auth.CompromisedPasswordChecker{}, nil)
	ticket, err := ah.createPending2FA(context.Background(), user.ID)
	if err != nil {
		t.Fatalf("createPending2FA: %v", err)
	}
	if rr := env.challenge(ticket, twoFATestCode, "totp"); rr.Code != http.StatusOK {
		t.Fatalf("真实 ticket challenge status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	if env.ticketExists(ticket) {
		t.Fatal("真实 ticket 在成功后应被消费")
	}
}

// Test2FAChallengeConsumesTicketOnce 覆盖 Minor：同一 ticket 的并发提交
// 只能派生一个会话（Lua 原子消费）。两个码不同，验证阶段都会成功。
func Test2FAChallengeConsumesTicketOnce(t *testing.T) {
	env := newTwoFATestEnv(t)
	user := env.repo.add(TwoFAUser{Username: "zoe", PasswordHash: twoFATestPasswordHash()})
	codes := env.enableUser(user)
	ticket := env.issueTicket(user.ID)

	statuses := make([]int, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			statuses[i] = env.challenge(ticket, codes[i], "backup").Code
		}(i)
	}
	wg.Wait()

	successes := 0
	for _, status := range statuses {
		switch status {
		case http.StatusOK:
			successes++
		case http.StatusBadRequest:
		default:
			t.Fatalf("并发 ticket status = %d，期望 200 或 400", status)
		}
	}
	if successes != 1 {
		t.Fatalf("同一 ticket 并发提交成功次数 = %d，期望恰 1（statuses=%v）", successes, statuses)
	}
}
