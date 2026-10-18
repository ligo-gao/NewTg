package auth

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// testSessionSecret 是单测夹具密钥；HMAC 的派生结果只在测试内断言。
const testSessionSecret = "test-session-secret-0123456789abcdef"

// fakeClock 让滑动/绝对超时测试以毫秒级完成——生产路径始终是 time.Now。
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// newTestSessionManager 用 miniredis 替换真 Redis；不依赖外部服务。
func newTestSessionManager(t *testing.T) (*SessionManager, *miniredis.Miniredis, *fakeClock) {
	t.Helper()
	mr := miniredis.RunT(t)
	// MaxRetries=-1 关闭重试：Redis-down 用例只需验证错误上抛，不必等默认退避。
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	// RunT 的 cleanup 先注册，故先关客户端、再关 miniredis。
	t.Cleanup(func() { _ = rdb.Close() })

	clock := &fakeClock{t: time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)}
	m := NewSessionManager(rdb, []byte(testSessionSecret))
	m.now = clock.Now
	return m, mr, clock
}

func mustCreateSession(t *testing.T, m *SessionManager, userID int64, role string) string {
	t.Helper()
	sid, err := m.Create(context.Background(), userID, SessionOpts{
		UA:   "TestAgent/1.0",
		IP:   "203.0.113.9",
		Role: role,
	})
	if err != nil {
		t.Fatalf("Create(user=%d, role=%s) 失败: %v", userID, role, err)
	}
	return sid
}

func mustGetSession(t *testing.T, m *SessionManager, sid string) *Session {
	t.Helper()
	s, err := m.Get(context.Background(), sid)
	if err != nil {
		t.Fatalf("Get(%q) 失败: %v", sid, err)
	}
	return s
}

// TestSessionCreateGetRoundTrip 覆盖 brief (a)：Create→Get 往返，字段与超时档一致，
// 且 Cookie 承载的 session id 是 32 字节 CSPRNG 的 base64url（无填充）。
func TestSessionCreateGetRoundTrip(t *testing.T) {
	ctx := context.Background()
	m, mr, clock := newTestSessionManager(t)
	now := clock.Now()

	sid := mustCreateSession(t, m, 42, "user")

	raw, err := base64.RawURLEncoding.DecodeString(sid)
	if err != nil || len(raw) != SessionIDBytes {
		t.Fatalf("session id 必须是 %d 字节的 base64url：len(raw)=%d err=%v", SessionIDBytes, len(raw), err)
	}
	if strings.ContainsAny(sid, "=+/") {
		t.Fatalf("session id 必须是无填充 URL-safe base64：%q", sid)
	}
	if !mr.Exists(sessionKey(sid)) {
		t.Fatalf("Redis 中缺少会话键 %q", sessionKey(sid))
	}

	s := mustGetSession(t, m, sid)
	if s.UserID != 42 {
		t.Errorf("UserID = %d，期望 42", s.UserID)
	}
	if s.UA != "TestAgent/1.0" || s.IP != "203.0.113.9" {
		t.Errorf("UA/IP = %q/%q，期望 TestAgent/1.0/203.0.113.9", s.UA, s.IP)
	}
	if s.AuthLevel != "user" {
		t.Errorf("AuthLevel = %q，期望 user", s.AuthLevel)
	}
	if !s.CreatedAt.Equal(now) || !s.LastSeenAt.Equal(now) {
		t.Errorf("CreatedAt/LastSeenAt = %v/%v，期望 %v", s.CreatedAt, s.LastSeenAt, now)
	}
	if !s.AbsoluteExpiresAt.Equal(now.Add(userAbsoluteTimeout)) {
		t.Errorf("AbsoluteExpiresAt = %v，期望 %v", s.AbsoluteExpiresAt, now.Add(userAbsoluteTimeout))
	}
	if s.CSRFHash != "" {
		t.Errorf("未签发 CSRF 时会话不应带 csrf_hash，得到 %q", s.CSRFHash)
	}

	// 合法格式但不存在的 sid；以及畸形 sid（不得拼进 Redis 键，直接 not-found）。
	unknown := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0xAB}, SessionIDBytes))
	if _, err := m.Get(ctx, unknown); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("Get(未知 sid) = %v，期望 ErrSessionNotFound", err)
	}
	if _, err := m.Get(ctx, "not-a-session-id"); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("Get(畸形 sid) = %v，期望 ErrSessionNotFound", err)
	}
}

// TestSessionCreateTruncatesUA 覆盖 A2：攻击者可控的 UA 在写入 Redis 前按
// UTF-8 边界截断到 512 字节；多字节字符不得被切成非法 UTF-8。
func TestSessionCreateTruncatesUA(t *testing.T) {
	ctx := context.Background()
	m, _, _ := newTestSessionManager(t)

	ascii := strings.Repeat("A", 4096)
	sid, err := m.Create(ctx, 7, SessionOpts{UA: ascii, IP: "203.0.113.9", Role: "user"})
	if err != nil {
		t.Fatalf("Create(long ascii UA): %v", err)
	}
	if got := mustGetSession(t, m, sid).UA; got != strings.Repeat("A", maxSessionUABytes) {
		t.Fatalf("ASCII UA 截断结果长度 = %d，期望 %d", len(got), maxSessionUABytes)
	}

	multi := strings.Repeat("用户", 300) // 1800 字节；512 落在第 171 个 rune 中间
	sid, err = m.Create(ctx, 7, SessionOpts{UA: multi, IP: "203.0.113.9", Role: "user"})
	if err != nil {
		t.Fatalf("Create(multibyte UA): %v", err)
	}
	got := mustGetSession(t, m, sid).UA
	if len(got) > maxSessionUABytes {
		t.Fatalf("多字节 UA 长度 = %d，超过上限 %d", len(got), maxSessionUABytes)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("多字节 UA 截断后不是合法 UTF-8: %q", got)
	}
	if !strings.HasPrefix(multi, got) {
		t.Fatalf("截断必须保留原前缀：got=%q", got)
	}
	if len(got) != maxSessionUABytes-2 {
		t.Fatalf("多字节截断应回退到 %d 字节（3 字节 rune 边界），实际 %d", maxSessionUABytes-2, len(got))
	}

	// obs-text（HTTP 头允许的非 UTF-8 字节）不得原样落库。
	sid, err = m.Create(ctx, 7, SessionOpts{UA: "\xff\xfe" + strings.Repeat("B", 600), IP: "203.0.113.9", Role: "user"})
	if err != nil {
		t.Fatalf("Create(invalid UTF-8 UA): %v", err)
	}
	if got := mustGetSession(t, m, sid).UA; !utf8.ValidString(got) || strings.ContainsRune(got, '�') {
		t.Fatalf("非法 UTF-8 输入清洗后仍不合法: %q", got)
	}
}

// TestSessionSlidingTouch 覆盖 brief (b) 的滑动部分：Touch 刷新 last_seen_at（含
// 并发驱逐索引 ZSET 的分数），7 天无活动后 Get 返回 not-found 并清理键。
func TestSessionSlidingTouch(t *testing.T) {
	ctx := context.Background()
	m, mr, clock := newTestSessionManager(t)

	sid := mustCreateSession(t, m, 42, "user")

	clock.Advance(time.Hour)
	if err := m.Touch(ctx, sid); err != nil {
		t.Fatalf("Touch 失败: %v", err)
	}
	s := mustGetSession(t, m, sid)
	if !s.LastSeenAt.Equal(clock.Now()) {
		t.Errorf("Touch 后 LastSeenAt = %v，期望 %v", s.LastSeenAt, clock.Now())
	}
	score, err := m.rdb.ZScore(ctx, userSessionsKey(42), sid).Result()
	if err != nil {
		t.Fatalf("读取并发驱逐索引分数失败: %v", err)
	}
	// 分数用 UnixMicro 存储（float64 可精确表示，避免 UnixNano 超过 2^53 丢精度）。
	if want := float64(clock.Now().UnixMicro()); score != want {
		t.Errorf("Touch 后 ZSET 分数 = %v，期望 %v", score, want)
	}

	// 距最后一次活动 7d-1m：仍有效；再滑 2 分钟越过滑动窗口：失效。
	clock.Advance(userSlidingTimeout - time.Minute)
	mustGetSession(t, m, sid)

	clock.Advance(2 * time.Minute)
	if _, err := m.Get(ctx, sid); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("超出滑动窗口后 Get = %v，期望 ErrSessionNotFound", err)
	}
	if mr.Exists(sessionKey(sid)) {
		t.Error("过期会话的 Redis 键应被清理")
	}
	if n := m.rdb.ZCard(ctx, userSessionsKey(42)).Val(); n != 0 {
		t.Errorf("过期会话应同时从驱逐索引移除，ZCARD = %d", n)
	}
}

// TestSessionAbsoluteExpiry 覆盖 brief (b) 的绝对部分：持续活动（滑动窗口不断刷新）
// 也不能越过 created_at + 30d；越过后 Get/Touch 均返回 not-found。
func TestSessionAbsoluteExpiry(t *testing.T) {
	ctx := context.Background()
	m, _, clock := newTestSessionManager(t)

	sid := mustCreateSession(t, m, 42, "user")

	// 每天 Touch 一次，滑动窗口永远新鲜，只有绝对上限能终止会话。
	for i := 0; i < 29; i++ {
		clock.Advance(24 * time.Hour)
		if err := m.Touch(ctx, sid); err != nil {
			t.Fatalf("第 %d 天 Touch 失败: %v", i+1, err)
		}
	}
	clock.Advance(24*time.Hour - time.Minute) // 距 created_at 29d23h59m
	if err := m.Touch(ctx, sid); err != nil {
		t.Fatalf("绝对上限前 1 分钟 Touch 应成功: %v", err)
	}

	clock.Advance(2 * time.Minute) // 越过 30d 绝对上限；距上次活动仅 3 分钟
	if _, err := m.Get(ctx, sid); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("越过绝对上限后 Get = %v，期望 ErrSessionNotFound", err)
	}
	if err := m.Touch(ctx, sid); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("越过绝对上限后 Touch = %v，期望 ErrSessionNotFound", err)
	}
}

// TestSessionAdminTimeoutProfile 覆盖 brief (c)：role 选择超时档。管理员 30 分钟滑动
// 窗口先于用户 7 天档生效；且 12 小时绝对上限压过刚刷新的滑动窗口（先到者失效）。
func TestSessionAdminTimeoutProfile(t *testing.T) {
	ctx := context.Background()
	m, _, clock := newTestSessionManager(t)

	adminSID := mustCreateSession(t, m, 7, "admin")
	userSID := mustCreateSession(t, m, 7, "user")

	clock.Advance(adminSlidingTimeout - time.Minute) // 29 分钟无活动
	mustGetSession(t, m, adminSID)
	mustGetSession(t, m, userSID)

	clock.Advance(2 * time.Minute) // 31 分钟无活动
	if _, err := m.Get(ctx, adminSID); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("管理员 31 分钟无活动 Get = %v，期望 ErrSessionNotFound（30min 档先于用户 7d 档）", err)
	}
	if _, err := m.Get(ctx, userSID); err != nil {
		t.Errorf("用户会话在 31 分钟无活动时不应失效: %v", err)
	}

	// 绝对上限：管理员 12h 到达后，即使 2 分钟前刚 Touch 过也必须失效。
	// 为使滑动窗口始终新鲜，每 29 分钟 Touch 一次（少于 30min 滑动档）。
	admin2 := mustCreateSession(t, m, 8, "admin")
	for elapsed := time.Duration(0); elapsed < adminAbsoluteTimeout-time.Minute; elapsed += 29 * time.Minute {
		step := 29 * time.Minute
		if remaining := adminAbsoluteTimeout - time.Minute - elapsed; remaining < step {
			step = remaining
		}
		clock.Advance(step)
		if err := m.Touch(ctx, admin2); err != nil {
			t.Fatalf("绝对上限内第 %v 分钟 Touch 应成功: %v", elapsed.Minutes(), err)
		}
	}
	clock.Advance(2 * time.Minute)
	if _, err := m.Get(ctx, admin2); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("管理员越过 12h 绝对上限后 Get = %v，期望 ErrSessionNotFound", err)
	}
}

// TestSessionConcurrencyEviction 覆盖 brief (d)：并发达上限时原子驱逐最久未活动者。
// 用户 21 个会话 → 留 20；管理员 6 个 → 留 5。
func TestSessionConcurrencyEviction(t *testing.T) {
	ctx := context.Background()

	t.Run("user 21→20 oldest activity evicted", func(t *testing.T) {
		m, _, clock := newTestSessionManager(t)

		sids := make([]string, 0, userMaxConcurrent)
		for i := 0; i < userMaxConcurrent; i++ {
			sids = append(sids, mustCreateSession(t, m, 42, "user"))
			clock.Advance(time.Second)
		}

		// 刷新第 1 个会话：它不再是最久未活动者，驱逐应落在第 2 个。
		if err := m.Touch(ctx, sids[0]); err != nil {
			t.Fatalf("Touch 失败: %v", err)
		}
		clock.Advance(time.Second)
		newest := mustCreateSession(t, m, 42, "user") // 第 21 个

		if _, err := m.Get(ctx, sids[1]); !errors.Is(err, ErrSessionNotFound) {
			t.Errorf("第 2 个会话（最久未活动）应被驱逐，Get = %v", err)
		}
		if _, err := m.Get(ctx, sids[0]); err != nil {
			t.Errorf("刚 Touch 过的会话不应被驱逐: %v", err)
		}
		for i := 2; i < userMaxConcurrent; i++ {
			if _, err := m.Get(ctx, sids[i]); err != nil {
				t.Errorf("会话 %d 不应被驱逐: %v", i, err)
			}
		}
		if _, err := m.Get(ctx, newest); err != nil {
			t.Errorf("新建的第 21 个会话必须存活: %v", err)
		}
		if n := m.rdb.ZCard(ctx, userSessionsKey(42)).Val(); n != int64(userMaxConcurrent) {
			t.Errorf("驱逐后索引 ZCARD = %d，期望 %d", n, userMaxConcurrent)
		}
	})

	t.Run("admin 6→5", func(t *testing.T) {
		m, _, clock := newTestSessionManager(t)

		sids := make([]string, 0, adminMaxConcurrent)
		for i := 0; i < adminMaxConcurrent; i++ {
			sids = append(sids, mustCreateSession(t, m, 9, "admin"))
			clock.Advance(time.Second)
		}
		clock.Advance(time.Second)
		mustCreateSession(t, m, 9, "admin") // 第 6 个

		if _, err := m.Get(ctx, sids[0]); !errors.Is(err, ErrSessionNotFound) {
			t.Errorf("管理员第 1 个会话应被驱逐，Get = %v", err)
		}
		if n := m.rdb.ZCard(ctx, userSessionsKey(9)).Val(); n != int64(adminMaxConcurrent) {
			t.Errorf("驱逐后索引 ZCARD = %d，期望 %d", n, adminMaxConcurrent)
		}
	})
}

// TestSessionRevoke 覆盖登出语义：Revoke 删除会话与索引成员，重复调用幂等。
func TestSessionRevoke(t *testing.T) {
	ctx := context.Background()
	m, mr, _ := newTestSessionManager(t)

	sid := mustCreateSession(t, m, 42, "user")
	if err := m.Revoke(ctx, sid); err != nil {
		t.Fatalf("Revoke 失败: %v", err)
	}
	if _, err := m.Get(ctx, sid); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("Revoke 后 Get = %v，期望 ErrSessionNotFound", err)
	}
	if mr.Exists(sessionKey(sid)) {
		t.Error("Revoke 后 Redis 会话键应被删除")
	}
	if n := m.rdb.ZCard(ctx, userSessionsKey(42)).Val(); n != 0 {
		t.Errorf("Revoke 后索引 ZCARD = %d，期望 0", n)
	}
	if err := m.Revoke(ctx, sid); err != nil {
		t.Errorf("重复 Revoke 应幂等返回 nil，得到 %v", err)
	}
}

// TestSessionRevokeUserKeepCurrent 覆盖 brief (f)：吊销该用户除 keepCurrent 外的
// 全部会话；其他用户不受影响；keepCurrent 为空表示全部吊销。
func TestSessionRevokeUserKeepCurrent(t *testing.T) {
	ctx := context.Background()
	m, _, _ := newTestSessionManager(t)

	keep := mustCreateSession(t, m, 42, "user")
	drop1 := mustCreateSession(t, m, 42, "user")
	drop2 := mustCreateSession(t, m, 42, "user")
	otherUser := mustCreateSession(t, m, 43, "user")

	if err := m.RevokeUser(ctx, 42, keep); err != nil {
		t.Fatalf("RevokeUser(keep) 失败: %v", err)
	}
	if _, err := m.Get(ctx, keep); err != nil {
		t.Errorf("keepCurrent 会话不应被吊销: %v", err)
	}
	for _, sid := range []string{drop1, drop2} {
		if _, err := m.Get(ctx, sid); !errors.Is(err, ErrSessionNotFound) {
			t.Errorf("会话 %q 应被吊销，Get = %v", sid, err)
		}
	}
	if _, err := m.Get(ctx, otherUser); err != nil {
		t.Errorf("其他用户的会话不应被吊销: %v", err)
	}
	if n := m.rdb.ZCard(ctx, userSessionsKey(42)).Val(); n != 1 {
		t.Errorf("保留 1 个会话后索引 ZCARD = %d，期望 1", n)
	}
	if n := m.rdb.ZCard(ctx, userSessionsKey(43)).Val(); n != 1 {
		t.Errorf("其他用户索引 ZCARD = %d，期望 1", n)
	}

	// keepCurrent 为空 → 吊销全部。
	if err := m.RevokeUser(ctx, 42, ""); err != nil {
		t.Fatalf("RevokeUser(全部) 失败: %v", err)
	}
	if _, err := m.Get(ctx, keep); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("keepCurrent 为空时应吊销全部会话，Get = %v", err)
	}
	if n := m.rdb.ZCard(ctx, userSessionsKey(42)).Val(); n != 0 {
		t.Errorf("全部吊销后索引 ZCARD = %d，期望 0", n)
	}
}

// recordedSession 是 recorder 接缝的观测值。
type recordedSession struct {
	userID  int64
	sidHash string
}

type channelRecorder struct {
	calls chan recordedSession
	err   error
}

func (r *channelRecorder) RecordSession(_ context.Context, userID int64, sidHash string) error {
	r.calls <- recordedSession{userID: userID, sidHash: sidHash}
	return r.err
}

// TestSessionRecorderStoresHashOnly 覆盖「SQL 侧绝不落 session id 明文」：
// recorder 收到的是 SHA-256(sid) 的十六进制，不是 sid 本身；异步落库失败不影响建会话。
func TestSessionRecorderStoresHashOnly(t *testing.T) {
	ctx := context.Background()
	m, _, _ := newTestSessionManager(t)

	rec := &channelRecorder{calls: make(chan recordedSession, 1)}
	m.SetSessionRecorder(rec)

	sid := mustCreateSession(t, m, 7, "user")

	select {
	case got := <-rec.calls:
		if got.userID != 7 {
			t.Errorf("recorder userID = %d，期望 7", got.userID)
		}
		if got.sidHash == sid || strings.Contains(got.sidHash, sid) {
			t.Error("recorder 不得收到 session id 明文")
		}
		if want := HashSessionID(sid); got.sidHash != want {
			t.Errorf("recorder sidHash = %q，期望 SHA-256 十六进制 %q", got.sidHash, want)
		}
		if len(got.sidHash) != 64 {
			t.Errorf("SHA-256 十六进制长度 = %d，期望 64", len(got.sidHash))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("异步落库 2s 内未触发")
	}

	// recorder 返回错误是次要信息：login_sessions 只是审计副本，Redis 才是会话权威，
	// 因此 Create 仍须成功。
	failing := &channelRecorder{calls: make(chan recordedSession, 1), err: errors.New("db down")}
	m.SetSessionRecorder(failing)
	if _, err := m.Create(ctx, 7, SessionOpts{Role: "user"}); err != nil {
		t.Fatalf("落库失败不应让 Create 失败: %v", err)
	}
	select {
	case <-failing.calls:
	case <-time.After(2 * time.Second):
		t.Fatal("recorder 未被调用")
	}
}

// TestSessionRedisDownFailsClosed 覆盖 GC-7 / I12：Redis 不可用时 Get 返回错误，
// 且绝不能把错误伪装成 not-found（调用方据此 fail-closed，而非当成匿名会话）。
func TestSessionRedisDownFailsClosed(t *testing.T) {
	ctx := context.Background()
	m, mr, _ := newTestSessionManager(t)

	sid := mustCreateSession(t, m, 42, "user")
	mr.Close() // 模拟 Redis 故障

	if _, err := m.Get(ctx, sid); err == nil {
		t.Fatal("Redis 不可用时 Get 必须返回错误")
	} else if errors.Is(err, ErrSessionNotFound) {
		t.Fatal("Redis 不可用不得伪装成 ErrSessionNotFound（会诱导调用方降级为匿名）")
	}
	if err := m.Touch(ctx, sid); err == nil {
		t.Fatal("Redis 不可用时 Touch 必须返回错误")
	}
	if err := m.Revoke(ctx, sid); err == nil {
		t.Fatal("Redis 不可用时 Revoke 必须返回错误")
	}
	if err := m.RevokeUser(ctx, 42, ""); err == nil {
		t.Fatal("Redis 不可用时 RevokeUser 必须返回错误")
	}
	if _, err := m.Create(ctx, 42, SessionOpts{Role: "user"}); err == nil {
		t.Fatal("Redis 不可用时 Create 必须返回错误")
	}
}

// deadlineRecorder 记录异步落库收到的 context deadline。
type deadlineRecorder struct{ deadlines chan time.Time }

func (r *deadlineRecorder) RecordSession(ctx context.Context, _ int64, _ string) error {
	if deadline, ok := ctx.Deadline(); ok {
		r.deadlines <- deadline
	}
	return nil
}

// TestSessionRecorderHasTimeout 覆盖 A6：异步落库必须带有限 deadline，
// 否则 DB 卡住时每次登录都会泄漏一个永久阻塞的 goroutine。
func TestSessionRecorderHasTimeout(t *testing.T) {
	m, _, _ := newTestSessionManager(t)
	rec := &deadlineRecorder{deadlines: make(chan time.Time, 1)}
	m.SetSessionRecorder(rec)

	before := time.Now()
	_ = mustCreateSession(t, m, 7, "user")
	select {
	case deadline := <-rec.deadlines:
		if deadline.IsZero() {
			t.Fatal("recordSession 的 context 没有 deadline")
		}
		if deadline.Before(before) || deadline.After(before.Add(sessionRecordTimeout+5*time.Second)) {
			t.Fatalf("deadline = %v，期望约 %v 后（sessionRecordTimeout）", deadline, sessionRecordTimeout)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("recorder 未被调用")
	}
}
