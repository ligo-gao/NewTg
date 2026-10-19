package httpapi

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/rbac"
)

// 本文件是 Task 9 的单元验收：会话管理 / GET|PATCH /api/me / /api/self(/setting)
// / POST /api/verify-email。全部依赖都是内存假实现（miniredis + fakeSelfRepo），
// 未打 integration 标签时不得触碰外部 PG/Redis（GC-8）。
//
// 端点契约（Task 9 brief + controller rulings）：
//   - GET    /api/sessions                  ≤50 条，按 last_seen 倒序，标当前会话；
//   - DELETE /api/sessions/{sid}            他人/未知 sid → 404（不泄露存在性）；
//                                           当前 sid → 400；
//   - POST   /api/sessions/revoke-others    保留当前会话；
//   - GET    /api/me                        零敏感字段；
//   - PATCH  /api/me                        仅 display_name，其它字段 403 field_not_writable；
//   - GET    /api/self / PUT /api/self/setting  乐观锁（updated_at 不匹配 → 409）；
//   - POST   /api/verify-email              {email_code} → 置 email_verified_at。
//
// 会话端点只接受浏览器会话（Cookie），API 令牌（Bearer）一律 403（F-01-27 I-13/
// TC-F-01-27-05）：令牌是中继平面凭证，不能读取或吊销控制台会话。

const sessionTestUserID int64 = 7

// fakeSelfRepo 是 SelfRepo 的内存实现，版本号用单调递增的假时钟模拟
// users.updated_at 的乐观锁语义。
type fakeSelfRepo struct {
	mu      sync.Mutex
	users   map[int64]*SelfRecord
	updates []SelfPatch
	clock   time.Time
	err     error
	// verifyBonuses 记录每次 MarkEmailVerified 收到的补发额（B6 断言）。
	verifyBonuses []int64
	// verifyGranted 覆盖「流水实际插入」结果（nil = signupBonus>0 即成功）；
	// false 模拟「已发放过 → ON CONFLICT 0 行」，用于断言名额归还。
	verifyGranted *bool
}

func newFakeSelfRepo() *fakeSelfRepo {
	return &fakeSelfRepo{
		users: map[int64]*SelfRecord{},
		clock: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC),
	}
}

func (r *fakeSelfRepo) tick() time.Time {
	r.clock = r.clock.Add(time.Second)
	return r.clock
}

func (r *fakeSelfRepo) add(userID int64, rec SelfRecord) *SelfRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	if rec.Role == "" {
		rec.Role = rbac.RoleUser
	}
	if rec.Status == "" {
		rec.Status = "enabled"
	}
	if rec.Language == "" {
		rec.Language = "en"
	}
	rec.RecordIPLog = true
	rec.UpdatedAt = r.tick()
	copied := rec
	r.users[userID] = &copied
	return &copied
}

// bump 模拟另一个并发写请求提交成功：updated_at 前进，旧令牌随即失效。
func (r *fakeSelfRepo) bump(userID int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.users[userID].UpdatedAt = r.tick()
}

func (r *fakeSelfRepo) GetSelf(_ context.Context, userID int64) (SelfRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return SelfRecord{}, r.err
	}
	rec, ok := r.users[userID]
	if !ok {
		return SelfRecord{}, ErrSelfNotFound
	}
	return *rec, nil
}

func (r *fakeSelfRepo) UpdateDisplayName(_ context.Context, userID int64, displayName *string) (SelfRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return SelfRecord{}, r.err
	}
	rec, ok := r.users[userID]
	if !ok {
		return SelfRecord{}, ErrSelfNotFound
	}
	rec.DisplayName = displayName
	rec.UpdatedAt = r.tick()
	return *rec, nil
}

func (r *fakeSelfRepo) UpdateSettings(_ context.Context, userID int64, patch SelfPatch, expectUpdatedAt time.Time) (SelfRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return SelfRecord{}, r.err
	}
	rec, ok := r.users[userID]
	if !ok {
		return SelfRecord{}, ErrSelfNotFound
	}
	if !rec.UpdatedAt.Equal(expectUpdatedAt) {
		return SelfRecord{}, ErrSelfConflict
	}
	if patch.Language != nil {
		rec.Language = *patch.Language
	}
	if patch.SidebarModules != nil {
		if string(*patch.SidebarModules) == "null" {
			rec.SidebarModules = nil
		} else {
			rec.SidebarModules = append(json.RawMessage(nil), (*patch.SidebarModules)...)
		}
	}
	if patch.RecordIPLog != nil {
		rec.RecordIPLog = *patch.RecordIPLog
	}
	rec.UpdatedAt = r.tick()
	r.updates = append(r.updates, patch)
	return *rec, nil
}

func (r *fakeSelfRepo) MarkEmailVerified(_ context.Context, userID int64, signupBonus int64) (time.Time, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return time.Time{}, false, r.err
	}
	rec, ok := r.users[userID]
	if !ok {
		return time.Time{}, false, ErrSelfNotFound
	}
	r.verifyBonuses = append(r.verifyBonuses, signupBonus)
	if rec.EmailVerifiedAt == nil {
		now := r.tick()
		rec.EmailVerifiedAt = &now
	}
	granted := signupBonus > 0
	if r.verifyGranted != nil {
		granted = *r.verifyGranted && signupBonus > 0
	}
	return *rec.EmailVerifiedAt, granted, nil
}

// unverifyEmail 把账号退回未验证状态，用于断言「验证码一次性」：如果第二次
// 请求仍命中「已验证」短路，复用码的断言就是空转。
func (r *fakeSelfRepo) unverifyEmail(userID int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.users[userID].EmailVerifiedAt = nil
}

// sessionTestEnv 直接装配「Protect 保护链 + 本任务注册钩子」，不经过
// NewRouter：Task 9 只拥有 handlers_session.go，路由装配由并行任务完成。
type sessionTestEnv struct {
	t        *testing.T
	sessions *auth.SessionManager
	mr       *miniredis.Miniredis
	rdb      *redis.Client
	repo     *fakeSelfRepo
	tokens   *fakeTokenLookup
	router   http.Handler
	handlers *SessionHandlers
}

func newSessionTestEnv(t *testing.T) *sessionTestEnv {
	return newSessionTestEnvWithConfig(t, nil)
}

// newSessionTestEnvWithConfig 允许用例注入配置中心快照（B6 的捐赠开关）。
func newSessionTestEnvWithConfig(t *testing.T, cfg mapConfig) *sessionTestEnv {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })

	sessions := auth.NewSessionManager(rdb, []byte(testSessionSecret))
	repo := newFakeSelfRepo()
	tokens := &fakeTokenLookup{metas: map[string]*auth.TokenMeta{}}

	h, err := NewSessionHandlers(SessionHandlerConfig{
		Repo:          repo,
		Sessions:      sessions,
		Redis:         rdb,
		SessionSecret: []byte(testSessionSecret),
		Config:        cfg,
	})
	if err != nil {
		t.Fatalf("NewSessionHandlers: %v", err)
	}

	sub := http.NewServeMux()
	h.RegisterSessionRoutes(sub, nil)

	outer := http.NewServeMux()
	// 与 routes.go 相同：/api/ 子树整体经过 SessionAuth → CSRF → OriginRefererCheck。
	outer.Handle("/api/", Protect(sessions, tokens, "")(sub))

	return &sessionTestEnv{
		t:        t,
		sessions: sessions,
		mr:       mr,
		rdb:      rdb,
		repo:     repo,
		tokens:   tokens,
		router:   outer,
		handlers: h,
	}
}

// createSession 建一个已签发 CSRF 的会话，返回 (sid, csrf)。
func (e *sessionTestEnv) createSession(userID int64, role, ua, ip string) (string, string) {
	e.t.Helper()
	sid, err := e.sessions.Create(context.Background(), userID, auth.SessionOpts{
		UA: ua, IP: ip, Role: role,
	})
	if err != nil {
		e.t.Fatalf("Create session: %v", err)
	}
	csrf := auth.NewCSRFToken()
	if err := e.sessions.IssueCSRF(context.Background(), sid, csrf); err != nil {
		e.t.Fatalf("IssueCSRF: %v", err)
	}
	return sid, csrf
}

// request 发起一次请求。sid 非空时携带会话 Cookie；非安全方法还需要 csrf
// 与同源 Origin，否则会被保护链拦在业务 handler 之前。
func (e *sessionTestEnv) request(method, path string, body any, sid, csrf string) *httptest.ResponseRecorder {
	e.t.Helper()
	var reader io.Reader = http.NoBody
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
	if sid != "" {
		req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: sid})
	}
	if csrf != "" {
		// 浏览器同时携带 CSRF Cookie 与头；测试不做这一步会让 GET /api/me
		// 把「Cookie 缺失」误判为需要补发并轮换令牌，后续写请求全部 403。
		req.AddCookie(&http.Cookie{Name: auth.CSRFCookieName, Value: csrf})
		req.Header.Set(auth.CSRFHeaderName, csrf)
	}
	req.Header.Set("Origin", "http://example.com")
	return serve(e.router, req)
}

func (e *sessionTestEnv) bearerRequest(method, path, token string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	return serve(e.router, req)
}

func (e *sessionTestEnv) mustGetSelf(t *testing.T, sid, csrf string) selfView {
	t.Helper()
	rr := e.request(http.MethodGet, "/api/self", nil, sid, csrf)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/self status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	var view selfView
	if err := json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
		t.Fatalf("GET /api/self JSON 非法: %v（body=%q）", err, rr.Body.String())
	}
	return view
}

// ---------------------------------------------------------------------------
// (a) TC-F-01-13-01 多设备列表；(b) TC-F-01-13-02 过期会话不列
// ---------------------------------------------------------------------------

func TestSessionListMultiDevice(t *testing.T) {
	env := newSessionTestEnv(t)
	env.repo.add(sessionTestUserID, SelfRecord{Username: "alice", Email: strPtr("alice@example.com")})

	first, firstCSRF := env.createSession(sessionTestUserID, rbac.RoleUser, "Desktop/Chrome", "198.51.100.1")
	second, _ := env.createSession(sessionTestUserID, rbac.RoleUser, "Mobile/Safari", "198.51.100.2")
	third, _ := env.createSession(sessionTestUserID, rbac.RoleUser, "Tablet/Firefox", "198.51.100.3")

	rr := env.request(http.MethodGet, "/api/sessions", nil, first, firstCSRF)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/sessions status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	var resp sessionListView
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("会话列表 JSON 非法: %v（body=%q）", err, rr.Body.String())
	}
	if len(resp.Sessions) != 3 {
		t.Fatalf("会话数 = %d，期望 3（body=%q）", len(resp.Sessions), rr.Body.String())
	}

	byID := map[string]sessionView{}
	for _, s := range resp.Sessions {
		byID[s.ID] = s
	}
	for _, sid := range []string{first, second, third} {
		if _, ok := byID[sid]; !ok {
			t.Errorf("列表缺少会话 %s", sid)
		}
	}
	if !byID[first].Current {
		t.Error("当前会话未标记 current=true")
	}
	if byID[second].Current || byID[third].Current {
		t.Error("只有当前会话可以标记 current=true")
	}
	if byID[second].UserAgent != "Mobile/Safari" || byID[second].IP != "198.51.100.2" {
		t.Errorf("UA/IP 投影错误: %+v", byID[second])
	}
	if byID[second].CreatedAt.IsZero() || byID[second].LastSeenAt.IsZero() {
		t.Errorf("时间字段不得为零值: %+v", byID[second])
	}

	// last_seen 倒序：SessionAuth 的 Touch 会把当前会话刷新到最新，因此它排第一。
	for i := 1; i < len(resp.Sessions); i++ {
		if resp.Sessions[i].LastSeenAt.After(resp.Sessions[i-1].LastSeenAt) {
			t.Errorf("列表未按 last_seen 倒序: %v > %v",
				resp.Sessions[i].LastSeenAt, resp.Sessions[i-1].LastSeenAt)
		}
	}
	if resp.Sessions[0].ID != first {
		t.Errorf("列表首项 = %s，期望被 Touch 的当前会话 %s", resp.Sessions[0].ID, first)
	}
}

func TestSessionListSkipsDeadRecords(t *testing.T) {
	env := newSessionTestEnv(t)
	env.repo.add(sessionTestUserID, SelfRecord{Username: "alice"})

	live, csrf := env.createSession(sessionTestUserID, rbac.RoleUser, "Live/1.0", "203.0.113.1")
	dead, _ := env.createSession(sessionTestUserID, rbac.RoleUser, "Dead/1.0", "203.0.113.2")

	// 模拟 Redis TTL 到期：sess:{id} 被回收，但 ZSET 索引成员残留。
	// ListSessions 必须跳过这类成员（TC-F-01-13-02）。
	env.mr.Del("sess:" + dead)
	if members, err := env.mr.ZMembers("sess:user:" + strconv.FormatInt(sessionTestUserID, 10)); err != nil || len(members) != 2 {
		t.Fatalf("测试前置失败：索引成员 = %v（err=%v），期望残留 2 条", members, err)
	}

	rr := env.request(http.MethodGet, "/api/sessions", nil, live, csrf)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	var resp sessionListView
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("JSON 非法: %v", err)
	}
	if len(resp.Sessions) != 1 || resp.Sessions[0].ID != live {
		t.Fatalf("列表 = %+v，期望只含存活会话 %s", resp.Sessions, live)
	}

	// 直接单测 ListSessions 的投影，避免端点层掩盖顺序/字段问题。
	infos, err := env.sessions.ListSessions(context.Background(), sessionTestUserID)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(infos) != 1 || infos[0].SID != live {
		t.Fatalf("ListSessions = %+v，期望只含 %s", infos, live)
	}
	if infos[0].UA != "Live/1.0" || infos[0].IP != "203.0.113.1" {
		t.Errorf("SessionInfo 字段错误: %+v", infos[0])
	}
}

// TestListSessionsCapsAt50 冻结 F-01-13 的列表上限。正常路径下并发上限
// （20/5）已保证活跃会话远少于 50，这里直接写 Redis 索引模拟「残留成员 +
// 上限兜底」，确保 ListSessions 自己也会截断。
func TestListSessionsCapsAt50(t *testing.T) {
	env := newSessionTestEnv(t)
	const userID int64 = 77
	now := time.Now().UTC()
	for i := 0; i < 60; i++ {
		sid := randomSessionID(t)
		seen := now.Add(-time.Duration(i) * time.Minute)
		if err := env.rdb.HSet(context.Background(), "sess:"+sid, map[string]any{
			"user_id":             strconv.FormatInt(userID, 10),
			"ua":                  "CapTest/1.0",
			"ip":                  "192.0.2.1",
			"created_at":          seen.Format(time.RFC3339Nano),
			"last_seen_at":        seen.Format(time.RFC3339Nano),
			"absolute_expires_at": now.Add(24 * time.Hour).Format(time.RFC3339Nano),
			"auth_level":          rbac.RoleUser,
		}).Err(); err != nil {
			t.Fatalf("HSet: %v", err)
		}
		if err := env.rdb.ZAdd(context.Background(), "sess:user:"+strconv.FormatInt(userID, 10),
			redis.Z{Score: float64(seen.UnixMicro()), Member: sid}).Err(); err != nil {
			t.Fatalf("ZAdd: %v", err)
		}
	}

	infos, err := env.sessions.ListSessions(context.Background(), userID)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(infos) != 50 {
		t.Fatalf("列表长度 = %d，期望上限 50", len(infos))
	}
	for i := 1; i < len(infos); i++ {
		if infos[i].LastSeenAt.After(infos[i-1].LastSeenAt) {
			t.Fatalf("列表未按 last_seen 倒序: 第 %d 项 %v > 第 %d 项 %v",
				i, infos[i].LastSeenAt, i-1, infos[i-1].LastSeenAt)
		}
	}
}

// ---------------------------------------------------------------------------
// (c) TC-F-01-14-01 / TC-F-01-27-03 踢出与越权 404
// ---------------------------------------------------------------------------

func TestSessionDeleteRules(t *testing.T) {
	env := newSessionTestEnv(t)
	env.repo.add(sessionTestUserID, SelfRecord{Username: "alice"})
	env.repo.add(8, SelfRecord{Username: "bob"})

	attacker, csrf := env.createSession(sessionTestUserID, rbac.RoleUser, "Attacker/1.0", "203.0.113.7")
	victim, _ := env.createSession(8, rbac.RoleUser, "Victim/1.0", "203.0.113.8")

	// 他人 sid 与完全未知的 sid 必须同形 404（不泄露存在性）。
	other := env.request(http.MethodDelete, "/api/sessions/"+victim, nil, attacker, csrf)
	unknown := env.request(http.MethodDelete, "/api/sessions/"+randomSessionID(t), nil, attacker, csrf)
	if other.Code != http.StatusNotFound || unknown.Code != http.StatusNotFound {
		t.Fatalf("越权/未知 sid status = %d/%d，期望 404/404（body=%q / %q）",
			other.Code, unknown.Code, other.Body.String(), unknown.Body.String())
	}
	if other.Body.String() != unknown.Body.String() {
		t.Fatalf("他人 sid 与未知 sid 响应不同，构成会话存在性信道:\nother=%q\nunknown=%q",
			other.Body.String(), unknown.Body.String())
	}
	if p := decodeProblem(t, other); p.Type != "https://newtg.dev/problems/not_found" {
		t.Errorf("problem.type = %q，期望 not_found", p.Type)
	}
	if _, err := env.sessions.Get(context.Background(), victim); err != nil {
		t.Fatalf("越权删除不得影响受害者会话: %v", err)
	}

	// 当前会话不可通过本端点踢出（应使用登出）。
	self := env.request(http.MethodDelete, "/api/sessions/"+attacker, nil, attacker, csrf)
	if self.Code != http.StatusBadRequest {
		t.Fatalf("踢当前会话 status = %d，期望 400（body=%q）", self.Code, self.Body.String())
	}
	if p := decodeProblem(t, self); p.Type != authProblemBadRequest {
		t.Errorf("踢当前会话 problem.type = %q，期望 bad_request", p.Type)
	}

	// 同一用户的其它会话：204，且被踢会话随即失效（TC-F-01-14-01）。
	second, _ := env.createSession(sessionTestUserID, rbac.RoleUser, "Second/1.0", "203.0.113.9")
	kick := env.request(http.MethodDelete, "/api/sessions/"+second, nil, attacker, csrf)
	if kick.Code != http.StatusNoContent {
		t.Fatalf("踢自己其它会话 status = %d，期望 204（body=%q）", kick.Code, kick.Body.String())
	}
	if _, err := env.sessions.Get(context.Background(), second); !errors.Is(err, auth.ErrSessionNotFound) {
		t.Fatalf("被踢会话 Get = %v，期望 ErrSessionNotFound", err)
	}
	if _, err := env.sessions.Get(context.Background(), attacker); err != nil {
		t.Fatalf("踢出其它会话不得影响当前会话: %v", err)
	}
}

// ---------------------------------------------------------------------------
// (d) TC-F-01-27-01 revoke-others 保留当前
// ---------------------------------------------------------------------------

func TestSessionRevokeOthersKeepsCurrent(t *testing.T) {
	env := newSessionTestEnv(t)
	env.repo.add(sessionTestUserID, SelfRecord{Username: "alice"})

	current, csrf := env.createSession(sessionTestUserID, rbac.RoleUser, "Current/1.0", "203.0.113.1")
	other1, _ := env.createSession(sessionTestUserID, rbac.RoleUser, "Other/1.0", "203.0.113.2")
	other2, _ := env.createSession(sessionTestUserID, rbac.RoleUser, "Other/2.0", "203.0.113.3")
	// 别人的会话必须保留（RevokeUser 只作用于本人索引）。
	foreign, _ := env.createSession(8, rbac.RoleUser, "Foreign/1.0", "203.0.113.4")

	rr := env.request(http.MethodPost, "/api/sessions/revoke-others", nil, current, csrf)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("revoke-others status = %d，期望 204（body=%q）", rr.Code, rr.Body.String())
	}
	if _, err := env.sessions.Get(context.Background(), current); err != nil {
		t.Fatalf("当前会话必须保留: %v", err)
	}
	for _, sid := range []string{other1, other2} {
		if _, err := env.sessions.Get(context.Background(), sid); !errors.Is(err, auth.ErrSessionNotFound) {
			t.Errorf("其它会话 %s Get = %v，期望 ErrSessionNotFound", sid, err)
		}
	}
	if _, err := env.sessions.Get(context.Background(), foreign); err != nil {
		t.Errorf("不得吊销其他用户的会话: %v", err)
	}
}

// ---------------------------------------------------------------------------
// (e) GET /api/me 零敏感字段；PATCH 未知字段 403 field_not_writable
// ---------------------------------------------------------------------------

func TestMeGetAndPatch(t *testing.T) {
	env := newSessionTestEnv(t)
	env.repo.add(sessionTestUserID, SelfRecord{
		Username:    "alice",
		Email:       strPtr("alice@example.com"),
		Role:        rbac.RoleUser,
		Status:      "enabled",
		AvatarURL:   strPtr("https://cdn.example.com/a.png"),
		DisplayName: strPtr("Alice"),
	})
	sid, csrf := env.createSession(sessionTestUserID, rbac.RoleUser, "Test/1.0", "203.0.113.1")

	rr := env.request(http.MethodGet, "/api/me", nil, sid, csrf)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/me status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	var me meView
	if err := json.Unmarshal(rr.Body.Bytes(), &me); err != nil {
		t.Fatalf("GET /api/me JSON 非法: %v（body=%q）", err, rr.Body.String())
	}
	if me.Username != "alice" || me.Role != rbac.RoleUser || me.Status != "enabled" {
		t.Errorf("/api/me 投影错误: %+v", me)
	}
	if me.Email == nil || *me.Email != "alice@example.com" {
		t.Errorf("email 未回传: %+v", me.Email)
	}
	if me.AvatarURL == nil || *me.AvatarURL != "https://cdn.example.com/a.png" {
		t.Errorf("avatar_url 未回传: %+v", me.AvatarURL)
	}
	if me.DisplayName == nil || *me.DisplayName != "Alice" {
		t.Errorf("display_name 未回传: %+v", me.DisplayName)
	}
	// M6 R-17: role=user 的 permissions 必须是非 nil 空集合（JSON []），
	// 而不是泄露 admin_permissions 原始列或序列化成 null。
	if me.Permissions == nil || len(me.Permissions) != 0 {
		t.Errorf("user permissions = %#v，期望空集合", me.Permissions)
	}
	for _, forbidden := range []string{"password", "totp", "backup", "key_hash", "admin_permissions", "hash"} {
		if strings.Contains(strings.ToLower(rr.Body.String()), forbidden) {
			t.Errorf("/api/me 响应包含敏感字段 %q: %s", forbidden, rr.Body.String())
		}
	}

	// 合法 PATCH：只写 display_name，并返回更新后的投影。
	patch := env.request(http.MethodPatch, "/api/me", map[string]any{"display_name": "Alice Cooper"}, sid, csrf)
	if patch.Code != http.StatusOK {
		t.Fatalf("PATCH /api/me status = %d，期望 200（body=%q）", patch.Code, patch.Body.String())
	}
	stored, _ := env.repo.GetSelf(context.Background(), sessionTestUserID)
	if stored.DisplayName == nil || *stored.DisplayName != "Alice Cooper" {
		t.Errorf("display_name 未落库: %+v", stored.DisplayName)
	}

	// 未知字段/只读字段必须 403 field_not_writable，绝不静默丢弃（ruling 3）。
	for _, body := range []map[string]any{
		{"display_name": "x", "role": "admin"},
		{"display_name": "x", "email": "evil@example.com"},
		{"display_name": "x", "avatar_url": "https://evil.example/x.png"},
		{"display_name": "x", "username": "root"},
		{"display_name": "x", "totp_enabled": true},
		{"nonsense": 1},
	} {
		bad := env.request(http.MethodPatch, "/api/me", body, sid, csrf)
		if bad.Code != http.StatusForbidden {
			t.Errorf("body=%v status = %d，期望 403（body=%q）", body, bad.Code, bad.Body.String())
			continue
		}
		if p := decodeProblem(t, bad); p.Type != "https://newtg.dev/problems/field_not_writable" {
			t.Errorf("body=%v problem.type = %q，期望 field_not_writable", body, p.Type)
		}
	}
	stored, _ = env.repo.GetSelf(context.Background(), sessionTestUserID)
	if stored.DisplayName == nil || *stored.DisplayName != "Alice Cooper" {
		t.Errorf("403 请求不得产生部分写入: %+v", stored.DisplayName)
	}

	// 超长（>32 runes）与缺失 display_name。
	if rr := env.request(http.MethodPatch, "/api/me",
		map[string]any{"display_name": strings.Repeat("a", 33)}, sid, csrf); rr.Code != http.StatusBadRequest {
		t.Errorf("33 字符 display_name status = %d，期望 400", rr.Code)
	}
	if rr := env.request(http.MethodPatch, "/api/me", map[string]any{}, sid, csrf); rr.Code != http.StatusBadRequest {
		t.Errorf("空 PATCH status = %d，期望 400", rr.Code)
	}

	// null/空串表示清除 display_name（前端回退显示 username）。
	if rr := env.request(http.MethodPatch, "/api/me", map[string]any{"display_name": nil}, sid, csrf); rr.Code != http.StatusOK {
		t.Fatalf("清除 display_name status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	stored, _ = env.repo.GetSelf(context.Background(), sessionTestUserID)
	if stored.DisplayName != nil {
		t.Errorf("display_name 清除后应为 nil，实际 %+v", stored.DisplayName)
	}

	// 未认证不得访问。
	if rr := env.request(http.MethodGet, "/api/me", nil, "", ""); rr.Code != http.StatusUnauthorized {
		t.Errorf("未认证 GET /api/me status = %d，期望 401", rr.Code)
	}
}

// TestMePermissionsEffectiveSet 覆盖 M6 R-17 的 permissions 归一：
// user → []；admin 显式键 → 原样；SQL NULL / ["*"] → ["*"]；显式 [] → []。
func TestMePermissionsEffectiveSet(t *testing.T) {
	for _, tc := range []struct {
		name        string
		role        string
		permissions []string
		want        []string
	}{
		{"user", rbac.RoleUser, nil, []string{}},
		{"logs-read-only-admin", rbac.RoleAdmin, []string{"logs:read"}, []string{"logs:read"}},
		{"full-null-admin", rbac.RoleAdmin, nil, []string{"*"}},
		{"full-star-admin", rbac.RoleAdmin, []string{"*"}, []string{"*"}},
		{"explicitly-empty-admin", rbac.RoleAdmin, []string{}, []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newSessionTestEnv(t)
			env.repo.add(sessionTestUserID, SelfRecord{
				Username: "actor",
				Role:     tc.role,
				Status:   "enabled",
			})
			env.handlers.access = staticPermissions{perms: tc.permissions}
			sid, csrf := env.createSession(sessionTestUserID, tc.role, "Test/1.0", "203.0.113.1")

			rr := env.request(http.MethodGet, "/api/me", nil, sid, csrf)
			if rr.Code != http.StatusOK {
				t.Fatalf("GET /api/me status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
			}
			var me meView
			if err := json.Unmarshal(rr.Body.Bytes(), &me); err != nil {
				t.Fatalf("GET /api/me JSON 非法: %v（body=%q）", err, rr.Body.String())
			}
			if !reflect.DeepEqual(me.Permissions, tc.want) {
				t.Errorf("permissions = %#v，期望 %#v", me.Permissions, tc.want)
			}
			if strings.Contains(rr.Body.String(), `"permissions":null`) {
				t.Errorf("permissions 不得为 null: %s", rr.Body.String())
			}
		})
	}
}

// ---------------------------------------------------------------------------
// (f) PUT /api/self/setting 乐观锁 409；(g) 语言非法值 400
// ---------------------------------------------------------------------------

func TestSelfSettingOptimisticLock(t *testing.T) {
	env := newSessionTestEnv(t)
	env.repo.add(sessionTestUserID, SelfRecord{Username: "alice"})
	sid, csrf := env.createSession(sessionTestUserID, rbac.RoleUser, "Test/1.0", "203.0.113.1")

	view := env.mustGetSelf(t, sid, csrf)
	if view.Language != "en" || !view.RecordIPLog {
		t.Fatalf("GET /api/self 默认值错误: %+v", view)
	}
	if view.UpdatedAt.IsZero() {
		t.Fatal("GET /api/self 必须回传 updated_at 作为乐观锁令牌")
	}

	ok := env.request(http.MethodPut, "/api/self/setting", map[string]any{
		"language":   "zh-CN",
		"updated_at": view.UpdatedAt.Format(time.RFC3339Nano),
	}, sid, csrf)
	if ok.Code != http.StatusOK {
		t.Fatalf("PUT /api/self/setting status = %d，期望 200（body=%q）", ok.Code, ok.Body.String())
	}
	var updated selfView
	if err := json.Unmarshal(ok.Body.Bytes(), &updated); err != nil {
		t.Fatalf("PUT 响应 JSON 非法: %v", err)
	}
	if updated.Language != "zh-CN" {
		t.Errorf("language = %q，期望 zh-CN", updated.Language)
	}
	if !updated.UpdatedAt.After(view.UpdatedAt) {
		t.Errorf("updated_at 未前进: %v → %v", view.UpdatedAt, updated.UpdatedAt)
	}

	// 模拟并发写：另一个请求先提交，调用方手里的旧令牌必须 409。
	env.repo.bump(sessionTestUserID)
	stale := env.request(http.MethodPut, "/api/self/setting", map[string]any{
		"language":   "fr",
		"updated_at": updated.UpdatedAt.Format(time.RFC3339Nano),
	}, sid, csrf)
	if stale.Code != http.StatusConflict {
		t.Fatalf("过期 updated_at status = %d，期望 409（body=%q）", stale.Code, stale.Body.String())
	}
	if p := decodeProblem(t, stale); p.Type != authProblemConflict {
		t.Errorf("409 problem.type = %q，期望 conflict", p.Type)
	}
	stored, _ := env.repo.GetSelf(context.Background(), sessionTestUserID)
	if stored.Language != "zh-CN" {
		t.Errorf("冲突请求不得落库，language = %q", stored.Language)
	}

	// updated_at 缺失/非法 → 400（乐观锁令牌不可省略）。
	current := env.mustGetSelf(t, sid, csrf)
	for _, body := range []map[string]any{
		{"language": "fr"},
		{"language": "fr", "updated_at": "not-a-timestamp"},
		{"updated_at": current.UpdatedAt.Format(time.RFC3339Nano)}, // 没有任何设置键
	} {
		rr := env.request(http.MethodPut, "/api/self/setting", body, sid, csrf)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("body=%v status = %d，期望 400（body=%q）", body, rr.Code, rr.Body.String())
		}
	}
}

func TestSelfSettingValidationAndSparseSidebar(t *testing.T) {
	env := newSessionTestEnv(t)
	env.repo.add(sessionTestUserID, SelfRecord{Username: "alice"})
	sid, csrf := env.createSession(sessionTestUserID, rbac.RoleUser, "Test/1.0", "203.0.113.1")

	view := env.mustGetSelf(t, sid, csrf)
	token := view.UpdatedAt.Format(time.RFC3339Nano)

	// (g) 语言必须是 canonical 7 语之一；非法值 400 且不落库。
	bad := env.request(http.MethodPut, "/api/self/setting", map[string]any{
		"language": "de", "updated_at": token,
	}, sid, csrf)
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("非法语言 status = %d，期望 400（body=%q）", bad.Code, bad.Body.String())
	}
	stored, _ := env.repo.GetSelf(context.Background(), sessionTestUserID)
	if stored.Language != "en" {
		t.Errorf("非法语言不得落库: %q", stored.Language)
	}

	// 稀疏 sidebar_modules：只落显式改过的键，值必须是 bool。
	okSidebar := env.request(http.MethodPut, "/api/self/setting", map[string]any{
		"sidebar_modules": map[string]any{"billing": false, "channels": true},
		"updated_at":      token,
	}, sid, csrf)
	if okSidebar.Code != http.StatusOK {
		t.Fatalf("sidebar_modules status = %d，期望 200（body=%q）", okSidebar.Code, okSidebar.Body.String())
	}
	var updated selfView
	if err := json.Unmarshal(okSidebar.Body.Bytes(), &updated); err != nil {
		t.Fatalf("JSON 非法: %v", err)
	}
	var sidebar map[string]bool
	if err := json.Unmarshal(updated.SidebarModules, &sidebar); err != nil {
		t.Fatalf("sidebar_modules 不是 JSON 对象: %v（raw=%s）", err, updated.SidebarModules)
	}
	if !reflect.DeepEqual(sidebar, map[string]bool{"billing": false, "channels": true}) {
		t.Errorf("sidebar_modules = %v，期望稀疏覆盖 {billing:false, channels:true}", sidebar)
	}

	token = updated.UpdatedAt.Format(time.RFC3339Nano)
	for _, badSidebar := range []any{
		map[string]any{"billing": "no"},
		map[string]any{"billing": nil}, // null 不得静默变成 false
		[]string{"billing"},
		"billing",
		42,
	} {
		rr := env.request(http.MethodPut, "/api/self/setting", map[string]any{
			"sidebar_modules": badSidebar, "updated_at": token,
		}, sid, csrf)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("sidebar_modules=%v status = %d，期望 400（body=%q）", badSidebar, rr.Code, rr.Body.String())
		}
	}

	// record_ip_log 与「清空覆盖」。
	token = updated.UpdatedAt.Format(time.RFC3339Nano)
	privacy := env.request(http.MethodPut, "/api/self/setting", map[string]any{
		"record_ip_log": false, "updated_at": token,
	}, sid, csrf)
	if privacy.Code != http.StatusOK {
		t.Fatalf("record_ip_log status = %d，期望 200（body=%q）", privacy.Code, privacy.Body.String())
	}
	if err := json.Unmarshal(privacy.Body.Bytes(), &updated); err != nil {
		t.Fatalf("JSON 非法: %v", err)
	}
	if updated.RecordIPLog {
		t.Error("record_ip_log 未更新为 false")
	}

	token = updated.UpdatedAt.Format(time.RFC3339Nano)
	clear := env.request(http.MethodPut, "/api/self/setting", map[string]any{
		"sidebar_modules": nil, "updated_at": token,
	}, sid, csrf)
	if clear.Code != http.StatusOK {
		t.Fatalf("清空 sidebar_modules status = %d，期望 200（body=%q）", clear.Code, clear.Body.String())
	}
	stored, _ = env.repo.GetSelf(context.Background(), sessionTestUserID)
	if stored.SidebarModules != nil {
		t.Errorf("sidebar_modules 清空后应为 NULL，实际 %s", stored.SidebarModules)
	}
	// GET /api/self 对 NULL 回传 JSON null（不是字符串 "null"）。
	if !strings.Contains(clear.Body.String(), `"sidebar_modules":null`) {
		t.Errorf("清空后响应应含 \"sidebar_modules\":null，实际 %s", clear.Body.String())
	}

	// 未知字段走 rbac 字段层 → 403 field_not_writable。
	unknown := env.request(http.MethodPut, "/api/self/setting", map[string]any{
		"language": "fr", "updated_at": token, "role": "admin",
	}, sid, csrf)
	if unknown.Code != http.StatusForbidden {
		t.Fatalf("未知字段 status = %d，期望 403（body=%q）", unknown.Code, unknown.Body.String())
	}
	if p := decodeProblem(t, unknown); p.Type != "https://newtg.dev/problems/field_not_writable" {
		t.Errorf("problem.type = %q，期望 field_not_writable", p.Type)
	}
}

// ---------------------------------------------------------------------------
// POST /api/verify-email
// ---------------------------------------------------------------------------

func TestVerifyEmail(t *testing.T) {
	env := newSessionTestEnv(t)
	env.repo.add(sessionTestUserID, SelfRecord{Username: "alice", Email: strPtr("alice@example.com")})
	sid, csrf := env.createSession(sessionTestUserID, rbac.RoleUser, "Test/1.0", "203.0.113.1")

	store := newEmailCodeStore(env.rdb, []byte(testSessionSecret))
	code, err := store.issue(context.Background(), "alice@example.com", "verify")
	if err != nil {
		t.Fatalf("issue code: %v", err)
	}

	wrong := env.request(http.MethodPost, "/api/verify-email", map[string]any{"email_code": "000000"}, sid, csrf)
	if wrong.Code != http.StatusBadRequest {
		t.Fatalf("错误验证码 status = %d，期望 400（body=%q）", wrong.Code, wrong.Body.String())
	}

	ok := env.request(http.MethodPost, "/api/verify-email", map[string]any{"email_code": code}, sid, csrf)
	if ok.Code != http.StatusOK {
		t.Fatalf("验证码 status = %d，期望 200（body=%q）", ok.Code, ok.Body.String())
	}
	var resp struct {
		EmailVerified bool `json:"email_verified"`
	}
	if err := json.Unmarshal(ok.Body.Bytes(), &resp); err != nil || !resp.EmailVerified {
		t.Fatalf("verify-email 响应 = %q（err=%v），期望 email_verified=true", ok.Body.String(), err)
	}
	stored, _ := env.repo.GetSelf(context.Background(), sessionTestUserID)
	if stored.EmailVerifiedAt == nil {
		t.Fatal("email_verified_at 未写入")
	}

	// 验证码一次性：把账号退回未验证状态，让第二次请求真正走「消费验证码」
	// 分支（否则会命中上面的已验证短路，断言空转），同一码必须 400。
	env.repo.unverifyEmail(sessionTestUserID)
	reuse := env.request(http.MethodPost, "/api/verify-email", map[string]any{"email_code": code}, sid, csrf)
	if reuse.Code != http.StatusBadRequest {
		t.Fatalf("复用已验证验证码 status = %d，期望 400（body=%q）", reuse.Code, reuse.Body.String())
	}
}

// TestVerifyEmailGrantsPendingSignupBonus 覆盖 B6：requires_verified_email
// 开启时，首次验证把当期配置的赠送额交给仓储在同一事务内补发；开关关闭时
// 不补发（注册路径已发放）。
func TestVerifyEmailGrantsPendingSignupBonus(t *testing.T) {
	env := newSessionTestEnvWithConfig(t, mapConfig{
		"billing.signup_grant_requires_verified_email": configBool(true),
		"billing.signup_grant_credits":                 configInt(750),
	})
	env.repo.add(sessionTestUserID, SelfRecord{Username: "alice", Email: strPtr("alice@example.com")})
	sid, csrf := env.createSession(sessionTestUserID, rbac.RoleUser, "Test/1.0", "203.0.113.1")
	store := newEmailCodeStore(env.rdb, []byte(testSessionSecret))
	code, err := store.issue(context.Background(), "alice@example.com", "verify")
	if err != nil {
		t.Fatalf("issue code: %v", err)
	}
	rr := env.request(http.MethodPost, "/api/verify-email", map[string]any{"email_code": code}, sid, csrf)
	if rr.Code != http.StatusOK {
		t.Fatalf("verify status = %d（body=%q）", rr.Code, rr.Body.String())
	}
	env.repo.mu.Lock()
	bonuses := append([]int64(nil), env.repo.verifyBonuses...)
	env.repo.mu.Unlock()
	if len(bonuses) != 1 || bonuses[0] != 750 {
		t.Fatalf("补发额调用 = %v，期望 [750]", bonuses)
	}

	// 开关关闭：注册路径已经发放过，验证不得再次发放。
	off := newSessionTestEnvWithConfig(t, mapConfig{
		"billing.signup_grant_requires_verified_email": configBool(false),
		"billing.signup_grant_credits":                 configInt(750),
	})
	off.repo.add(sessionTestUserID, SelfRecord{Username: "bob", Email: strPtr("bob@example.com")})
	offSID, offCSRF := off.createSession(sessionTestUserID, rbac.RoleUser, "Test/1.0", "203.0.113.2")
	offStore := newEmailCodeStore(off.rdb, []byte(testSessionSecret))
	offCode, err := offStore.issue(context.Background(), "bob@example.com", "verify")
	if err != nil {
		t.Fatalf("issue code: %v", err)
	}
	if rr := off.request(http.MethodPost, "/api/verify-email", map[string]any{"email_code": offCode}, offSID, offCSRF); rr.Code != http.StatusOK {
		t.Fatalf("verify status = %d（body=%q）", rr.Code, rr.Body.String())
	}
	off.repo.mu.Lock()
	offBonuses := append([]int64(nil), off.repo.verifyBonuses...)
	off.repo.mu.Unlock()
	if len(offBonuses) != 1 || offBonuses[0] != 0 {
		t.Fatalf("开关关闭时补发额调用 = %v，期望 [0]", offBonuses)
	}
}

// ---------------------------------------------------------------------------
// 凭证平面：API 令牌不得访问会话端点（TC-F-01-27-05）
// ---------------------------------------------------------------------------

func TestSessionEndpointsRejectAPIToken(t *testing.T) {
	env := newSessionTestEnv(t)
	env.repo.add(sessionTestUserID, SelfRecord{Username: "alice"})

	const plain = "nkt-testtoken1234567890"
	env.tokens.metas[auth.HashTokenKey(plain)] = &auth.TokenMeta{
		ID: 1, UserID: sessionTestUserID, Status: "enabled", UnlimitedQuota: true,
	}

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/sessions"},
		{http.MethodDelete, "/api/sessions/" + randomSessionID(t)},
		{http.MethodPost, "/api/sessions/revoke-others"},
	} {
		rr := env.bearerRequest(tc.method, tc.path, plain)
		if rr.Code != http.StatusForbidden {
			t.Errorf("%s %s status = %d，期望 403（body=%q）", tc.method, tc.path, rr.Code, rr.Body.String())
		}
	}

	// 对照：/api/me 允许 Bearer（令牌属于同一用户，且不涉及会话凭证）。
	me := env.bearerRequest(http.MethodGet, "/api/me", plain)
	if me.Code != http.StatusOK {
		t.Fatalf("Bearer GET /api/me status = %d，期望 200（body=%q）", me.Code, me.Body.String())
	}
}

// ---------------------------------------------------------------------------
// 注册钩子与路由存在性
// ---------------------------------------------------------------------------

// TestSessionRouteRegistrationUsesWrap 冻结注册钩子的契约：所有端点都必须
// 经过调用方传入的 wrap（生产为 SessionAuth/Protect 链）。
func TestSessionRouteRegistrationUsesWrap(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	sessions := auth.NewSessionManager(rdb, []byte(testSessionSecret))

	h, err := NewSessionHandlers(SessionHandlerConfig{
		Repo:     newFakeSelfRepo(),
		Sessions: sessions,
		Redis:    rdb,
	})
	if err != nil {
		t.Fatalf("NewSessionHandlers: %v", err)
	}
	mux := http.NewServeMux()
	h.RegisterSessionRoutes(mux, SessionAuth(sessions, nil))

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/sessions"},
		{http.MethodDelete, "/api/sessions/abc"},
		{http.MethodPost, "/api/sessions/revoke-others"},
		{http.MethodGet, "/api/me"},
		{http.MethodPatch, "/api/me"},
		{http.MethodGet, "/api/self"},
		{http.MethodPut, "/api/self/setting"},
		{http.MethodPost, "/api/verify-email"},
	} {
		rr := serve(mux, httptest.NewRequest(tc.method, tc.path, nil))
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("%s %s status = %d，期望 401（wrap 未生效或路由未注册）",
				tc.method, tc.path, rr.Code)
		}
	}
}

func TestNewSessionHandlersRequiresDependencies(t *testing.T) {
	if _, err := NewSessionHandlers(SessionHandlerConfig{}); err == nil {
		t.Fatal("缺少 Sessions 必须返回装配错误")
	}
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	sessions := auth.NewSessionManager(rdb, []byte(testSessionSecret))
	if _, err := NewSessionHandlers(SessionHandlerConfig{Sessions: sessions}); err == nil {
		t.Fatal("缺少 Repo/DB 必须返回装配错误")
	}
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

func strPtr(v string) *string { return &v }

// randomSessionID 生成格式合法（base64url 编码 32 字节）但必然不存在的 sid。
func randomSessionID(t *testing.T) string {
	t.Helper()
	buf := make([]byte, auth.SessionIDBytes)
	if _, err := cryptorand.Read(buf); err != nil {
		t.Fatalf("crypto/rand: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

// TestMeGetReissuesMissingCSRFCookie 覆盖 B7：Cookie 会话仍在但 __Host-nkt_csrf
// 丢失时，GET /api/me 补发令牌；已有 Cookie 时不得轮换（否则并发写请求会拿
// 旧令牌 403）。
func TestMeGetReissuesMissingCSRFCookie(t *testing.T) {
	env := newSessionTestEnv(t)
	env.repo.add(sessionTestUserID, SelfRecord{Username: "alice", Email: strPtr("alice@example.com")})
	sid, _ := env.createSession(sessionTestUserID, rbac.RoleUser, "Test/1.0", "203.0.113.1")

	rr := env.request(http.MethodGet, "/api/me", nil, sid, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/me status = %d（body=%q）", rr.Code, rr.Body.String())
	}
	reissued := cookieByName(rr, auth.CSRFCookieName)
	if reissued == nil || reissued.Value == "" {
		t.Fatal("缺失 CSRF Cookie 时 GET /api/me 必须补发")
	}
	if reissued.Path != "/" || !reissued.Secure || reissued.HttpOnly {
		t.Fatalf("补发的 CSRF Cookie 属性错误: %+v", reissued)
	}
	// 服务端必须真的接受新令牌（不是只写了个随机 Cookie）。
	if !env.sessions.VerifyCSRF(context.Background(), sid, reissued.Value) {
		t.Fatal("补发的 CSRF 令牌不可用")
	}

	// 已有 Cookie：不得轮换（响应不出现新的 Set-Cookie）。
	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: sid})
	req.AddCookie(&http.Cookie{Name: auth.CSRFCookieName, Value: reissued.Value})
	req.Header.Set("Origin", "http://example.com")
	rr = serve(env.router, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/me status = %d（body=%q）", rr.Code, rr.Body.String())
	}
	if c := cookieByName(rr, auth.CSRFCookieName); c != nil {
		t.Fatalf("已有 CSRF Cookie 时不得轮换，收到 %+v", c)
	}
}

// verifyEmailGrantEnv 构造启用「验证后补发」的环境，并返回用户/会话/验证码。
func verifyEmailGrantEnv(t *testing.T, repo *fakeSelfRepo) (*sessionTestEnv, string, string, string) {
	t.Helper()
	env := newSessionTestEnvWithConfig(t, mapConfig{
		"billing.signup_grant_requires_verified_email": configBool(true),
		"billing.signup_grant_credits":                 configInt(500),
	})
	if repo != nil {
		env.repo = repo
		// 重新装配 handler，使注入的 repo 生效（NewSessionHandlers 已持有旧指针）。
		h, err := NewSessionHandlers(SessionHandlerConfig{
			Repo: repo, Sessions: env.sessions, Redis: env.rdb,
			SessionSecret: []byte(testSessionSecret),
			Config:        env.handlers.config, Logger: testLogger(),
		})
		if err != nil {
			t.Fatalf("NewSessionHandlers: %v", err)
		}
		sub := http.NewServeMux()
		h.RegisterSessionRoutes(sub, nil)
		outer := http.NewServeMux()
		outer.Handle("/api/", Protect(env.sessions, env.tokens, "")(sub))
		env.router = outer
		env.handlers = h
	}
	env.repo.add(sessionTestUserID, SelfRecord{Username: "alice", Email: strPtr("alice@example.com")})
	sid, csrf := env.createSession(sessionTestUserID, rbac.RoleUser, "Test/1.0", "203.0.113.1")
	store := newEmailCodeStore(env.rdb, []byte(testSessionSecret))
	code, err := store.issue(context.Background(), "alice@example.com", "verify")
	if err != nil {
		t.Fatalf("issue code: %v", err)
	}
	return env, sid, csrf, code
}

// TestVerifyEmailSignupGrantCapSkipsButVerifies：同 IP 名额已满 → 仍完成验证，
// 不写流水，且不改变名额计数。
func TestVerifyEmailSignupGrantCapSkipsButVerifies(t *testing.T) {
	env, sid, csrf, code := verifyEmailGrantEnv(t, nil)
	key := signupGrantSlotKey("192.0.2.1")
	if err := env.rdb.Set(context.Background(), key, strconv.Itoa(signupGrantIPLimit), signupGrantIPWindow).Err(); err != nil {
		t.Fatalf("seed cap: %v", err)
	}

	rr := env.request(http.MethodPost, "/api/verify-email", map[string]any{"email_code": code}, sid, csrf)
	if rr.Code != http.StatusOK {
		t.Fatalf("verify status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	env.repo.mu.Lock()
	bonuses := append([]int64(nil), env.repo.verifyBonuses...)
	env.repo.mu.Unlock()
	if len(bonuses) != 1 || bonuses[0] != 0 {
		t.Fatalf("补发额 = %v，期望 [0]（cap 命中跳过）", bonuses)
	}
	if got := env.rdb.Get(context.Background(), key).Val(); got != strconv.Itoa(signupGrantIPLimit) {
		t.Fatalf("名额计数 = %q，期望保持 %d", got, signupGrantIPLimit)
	}
}

// TestVerifyEmailSignupGrantReleasedWhenNotInserted：流水未实际插入（已发放）
// 时必须归还名额，重复验证不得消耗额度。
func TestVerifyEmailSignupGrantReleasedWhenNotInserted(t *testing.T) {
	repo := newFakeSelfRepo()
	notGranted := false
	repo.verifyGranted = &notGranted
	env, sid, csrf, code := verifyEmailGrantEnv(t, repo)
	key := signupGrantSlotKey("192.0.2.1")

	rr := env.request(http.MethodPost, "/api/verify-email", map[string]any{"email_code": code}, sid, csrf)
	if rr.Code != http.StatusOK {
		t.Fatalf("verify status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	if got := env.rdb.Get(context.Background(), key).Val(); got != "0" {
		t.Fatalf("名额计数 = %q，期望 0（预占后归还）", got)
	}

	// 已发放且已发布场景：再次验证命中「已验证」短路，不再预占名额。
	replay := env.request(http.MethodPost, "/api/verify-email", map[string]any{"email_code": code}, sid, csrf)
	if replay.Code != http.StatusOK {
		t.Fatalf("重复 verify status = %d，期望 200（body=%q）", replay.Code, replay.Body.String())
	}
	if got := env.rdb.Get(context.Background(), key).Val(); got != "0" {
		t.Fatalf("重复验证后名额计数 = %q，期望 0", got)
	}
}

// TestVerifyEmailSignupGrantCounterUnavailable：领赠计数不可用时跳过补发，
// 但邮箱验证仍然成功（可用性优先于赠送）。
func TestVerifyEmailSignupGrantCounterUnavailable(t *testing.T) {
	env, sid, csrf, code := verifyEmailGrantEnv(t, nil)
	broken := redis.NewClient(&redis.Options{
		Addr: "127.0.0.1:1", MaxRetries: -1, DialTimeout: 50 * time.Millisecond,
	})
	t.Cleanup(func() { _ = broken.Close() })
	env.handlers.rdb = broken

	rr := env.request(http.MethodPost, "/api/verify-email", map[string]any{"email_code": code}, sid, csrf)
	if rr.Code != http.StatusOK {
		t.Fatalf("verify status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	env.repo.mu.Lock()
	bonuses := append([]int64(nil), env.repo.verifyBonuses...)
	env.repo.mu.Unlock()
	if len(bonuses) != 1 || bonuses[0] != 0 {
		t.Fatalf("补发额 = %v，期望 [0]（计数不可用跳过）", bonuses)
	}
	if stored, _ := env.repo.GetSelf(context.Background(), sessionTestUserID); stored.EmailVerifiedAt == nil {
		t.Fatal("计数不可用时仍必须置 email_verified_at")
	}
}

// TestReleaseSignupGrantSlotDetached 覆盖 D2 终验 Minor 2/3：客户端断连
// （ctx 已取消）仍必须归还名额；键不存在时裸 DECR 不得创建 -1。
func TestReleaseSignupGrantSlotDetached(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	ctx := context.Background()

	// (1) 已取消的请求 ctx：detached 包装仍能 DECR。
	ip := "203.0.113.5"
	if err := rdb.Set(ctx, signupGrantSlotKey(ip), "1", time.Hour).Err(); err != nil {
		t.Fatalf("seed slot: %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := releaseSignupGrantSlotDetached(canceled, rdb, ip); err != nil {
		t.Fatalf("detached release: %v", err)
	}
	if got := rdb.Get(ctx, signupGrantSlotKey(ip)).Val(); got != "0" {
		t.Fatalf("取消 ctx 后名额 = %q，期望 0（仍已归还）", got)
	}

	// (2) 键不存在：不得创建 -1。
	missing := "203.0.113.6"
	if err := releaseSignupGrantForIP(ctx, rdb, missing); err != nil {
		t.Fatalf("release missing key: %v", err)
	}
	if mr.Exists(signupGrantSlotKey(missing)) {
		t.Fatalf("缺失键被 DECR 创建为 %q，期望不存在",
			rdb.Get(ctx, signupGrantSlotKey(missing)).Val())
	}
}
