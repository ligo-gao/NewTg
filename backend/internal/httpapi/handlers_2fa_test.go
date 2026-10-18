package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

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

func (r *fakeTwoFARepo) ConsumeBackupCode(_ context.Context, userID int64, matchedHash string, remaining []string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	u := r.users[userID]
	if u == nil || !u.TOTPEnabled {
		return false, nil
	}
	found := false
	for _, h := range u.BackupCodes {
		if h == matchedHash {
			found = true
			break
		}
	}
	if !found {
		// 条件 UPDATE 的 WHERE 不成立：并发已被消费或码已重生成。
		return false, nil
	}
	u.BackupCodes = append([]string(nil), remaining...)
	return true, nil
}

// ---------------------------------------------------------------------------
// 测试环境
// ---------------------------------------------------------------------------

type twoFATestEnv struct {
	t        *testing.T
	router   http.Handler
	repo     *fakeTwoFARepo
	rdb      *redis.Client
	mr       *miniredis.Miniredis
	sessions *auth.SessionManager
	handlers *TwoFAHandlers
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

	h, err := NewTwoFAHandlers(TwoFAConfig{
		Repo:          repo,
		Redis:         rdb,
		Sessions:      sessions,
		EncryptionKey: twoFATestEncryptionKey,
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
		t:        t,
		router:   root,
		repo:     repo,
		rdb:      rdb,
		mr:       mr,
		sessions: sessions,
		handlers: h,
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

// authedRequest 建一个带会话 Cookie、CSRF 头与同源 Origin 的请求。
func (e *twoFATestEnv) authedRequest(userID int64, method, path string, body any) *httptest.ResponseRecorder {
	e.t.Helper()
	sid, err := e.sessions.Create(context.Background(), userID, auth.SessionOpts{
		UA: "TwoFATest/1.0", IP: "192.0.2.20", Role: "user",
	})
	if err != nil {
		e.t.Fatalf("Create session: %v", err)
	}
	csrf := auth.NewCSRFToken()
	if err := e.sessions.IssueCSRF(context.Background(), sid, csrf); err != nil {
		e.t.Fatalf("IssueCSRF: %v", err)
	}

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

	// 旧码必须全部失效。
	for _, old := range oldCodes {
		ticket := env.issueTicket(user.ID)
		if rr := env.challenge(ticket, old, "backup"); rr.Code == http.StatusOK {
			t.Fatalf("旧备用码 %q 在重生成后仍然可用", old)
		}
	}

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
		name string
		body map[string]any
	}{
		{"缺 ticket", map[string]any{"code": twoFATestCode, "type": "totp"}},
		{"畸形 ticket", map[string]any{"ticket": "not-a-ticket", "code": twoFATestCode, "type": "totp"}},
		{"未知 type", map[string]any{"ticket": env.issueTicket(user.ID), "code": twoFATestCode, "type": "sms"}},
		{"空 body", nil},
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
		})
	}

	// 过期/不存在的 ticket 与畸形 ticket 同一文案（无存在性探测信道）。
	expired := env.issueTicket(user.ID)
	env.mr.Del(twoFATicketKeyPrefix + expired)
	rr := env.challenge(expired, twoFATestCode, "totp")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("过期 ticket status = %d，期望 400", rr.Code)
	}
	if title := decodeProblemTitle(t, rr); title != "Invalid or expired verification ticket" {
		t.Errorf("problem title = %q", title)
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
