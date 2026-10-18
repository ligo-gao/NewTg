package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/redis/go-redis/v9"
)

// 会话常量。数值是 M1 计划 R-06 的冻结值（modules-01 F-01-13）：
//   - 用户：滑动 7d / 绝对 30d / 并发 20
//   - 管理员：滑动 30min / 绝对 12h / 并发 5
//
// 管理员档更短，因为管理面权限更高、被窃取后的爆炸半径更大（security-01 §2.1、§3）。
const (
	// SessionIDBytes 是会话 id 的熵长度：256 bit，满足 ASVS 7.2.3 的 ≥128 bit。
	SessionIDBytes = 32
	// CSRFTokenBytes 是同步器令牌的熵长度（security-01 §3 冻结）。
	CSRFTokenBytes = 32

	userSlidingTimeout  = 7 * 24 * time.Hour
	userAbsoluteTimeout = 30 * 24 * time.Hour
	userMaxConcurrent   = 20

	adminSlidingTimeout  = 30 * time.Minute
	adminAbsoluteTimeout = 12 * time.Hour
	adminMaxConcurrent   = 5

	// sessionKeyPrefix 构造成 sess:{id}（security-01 §2.1 冻结的 Redis 键名）。
	sessionKeyPrefix = "sess:"
	// userSessionsKeyPrefix 是每用户活跃会话索引；成员为 sid，分数为最近活动时间。
	// 该索引让「并发上限驱逐」与「吊销用户全部会话」无需 SCAN 全库。
	userSessionsKeyPrefix = "sess:user:"

	// maxSessionUABytes 是写入 Redis 会话记录（并由会话列表原样返回）的 UA
	// 存储上限。User-Agent 没有协议层长度限制，且是攻击者完全可控的输入：
	// 不截断会把任意大的字符串写进 Redis 与审计副本。512 字节足以识别任何
	// 现实浏览器/设备；截断按 UTF-8 边界进行，绝不产出非法 UTF-8。
	maxSessionUABytes = 512

	// sessionRecordTimeout 限制异步 login_sessions 落库的时长。DB 卡死时
	// 没有上限的 goroutine 会在每次登录后堆积（连接池耗尽 → 雪崩）；5 秒
	// 足以覆盖正常写入，超时即放弃——该表只是审计副本，会话仍以 Redis 为准。
	sessionRecordTimeout = 5 * time.Second

	fieldUserID            = "user_id"
	fieldUA                = "ua"
	fieldIP                = "ip"
	fieldCreatedAt         = "created_at"
	fieldLastSeenAt        = "last_seen_at"
	fieldAbsoluteExpiresAt = "absolute_expires_at"
	fieldAuthLevel         = "auth_level"
)

// ErrSessionNotFound 表示会话不存在或已过期（滑动/绝对超时先到者）。
// 调用方应把它映射为未认证；Redis 故障则以普通 error 返回，绝不复用本哨兵（GC-7）。
var ErrSessionNotFound = errors.New("auth: session not found")

// SessionOpts 是建会话时由登录流程提供的上下文；Role 决定超时档。
type SessionOpts struct {
	UA   string
	IP   string
	Role string
}

// Session 是 Redis hash sess:{sid} 的内存投影。该结构只会返回给已通过校验的调用方，
// 不直接序列化给客户端（security-01 §2.1）。
type Session struct {
	UserID            int64
	UA                string
	IP                string
	CreatedAt         time.Time
	LastSeenAt        time.Time
	AbsoluteExpiresAt time.Time
	AuthLevel         string
	CSRFHash          string
}

// timeoutProfile 是一档会话策略。并发上限也随档位变化（R-06）。
type timeoutProfile struct {
	sliding       time.Duration
	absolute      time.Duration
	maxConcurrent int
}

// profileForRole 把 users.role 映射到超时档。未知/空角色按普通用户处理：
// 宁可给更长的会话也不因拼写错误把管理员误降为更严格档（后者会导致无法登录）。
func profileForRole(role string) timeoutProfile {
	if role == "admin" {
		return timeoutProfile{
			sliding:       adminSlidingTimeout,
			absolute:      adminAbsoluteTimeout,
			maxConcurrent: adminMaxConcurrent,
		}
	}
	return timeoutProfile{
		sliding:       userSlidingTimeout,
		absolute:      userAbsoluteTimeout,
		maxConcurrent: userMaxConcurrent,
	}
}

// SessionRecorder 是 login_sessions 落库的可选接缝。Redis 是会话的权威存储；
// PostgreSQL 行仅作审计/会话列表副本，且只允许收到 SHA-256(sid)（I11：明文 session id
// 绝不落第二个副本）。Task 4/6 用真实的 DB 实现注入；默认 nil 即 no-op。
type SessionRecorder interface {
	RecordSession(ctx context.Context, userID int64, sidHash string) error
}

// SessionManager 管理服务端不透明会话。SESSION_SECRET 不用于签名会话（会话内容不在
// Cookie 里），只作为 CSRF 令牌的 HMAC 密钥与审计侧标识哈希的密钥（R-01-05）。
type SessionManager struct {
	rdb    *redis.Client
	secret []byte
	// now 是可替换时钟，仅供测试跨过 7d/30d/12h 窗口；生产恒为 time.Now。
	now      func() time.Time
	recorder SessionRecorder
}

// NewSessionManager 构造会话管理器。secret 即 config.SessionSecret。
func NewSessionManager(rdb *redis.Client, secret []byte) *SessionManager {
	return &SessionManager{rdb: rdb, secret: secret, now: time.Now}
}

// SetSessionRecorder 注入 login_sessions 落库实现（默认 nil）。必须在开始服务前调用。
func (m *SessionManager) SetSessionRecorder(r SessionRecorder) {
	m.recorder = r
}

// Create 生成 256 bit CSPRNG 会话 id，写入 Redis hash，并按并发上限原子驱逐
// 最久未活动者。返回的 token 只应写入 HttpOnly 的 __Host-nkt_sess Cookie。
func (m *SessionManager) Create(ctx context.Context, userID int64, opts SessionOpts) (string, error) {
	sid, err := newSessionID()
	if err != nil {
		return "", err
	}

	now := m.now().UTC()
	prof := profileForRole(opts.Role)
	authLevel := opts.Role
	if authLevel == "" {
		authLevel = "user"
	}
	absoluteExpiresAt := now.Add(prof.absolute)

	// Redis TTL 只是垃圾回收：真正的失效判定在 Get/Touch 里按滑动与绝对上限双检查。
	// TTL 取两者先到者，避免键在滑动窗口过后仍长期滞留。
	ttl := minDuration(prof.sliding, prof.absolute)

	score := strconv.FormatInt(now.UnixMicro(), 10)
	_, err = createSessionScript.Run(ctx, m.rdb,
		[]string{sessionKey(sid), userSessionsKey(userID)},
		strconv.FormatInt(userID, 10),
		truncateUTF8Bytes(opts.UA, maxSessionUABytes),
		opts.IP,
		formatSessionTime(now),
		formatSessionTime(now),
		formatSessionTime(absoluteExpiresAt),
		authLevel,
		strconv.FormatInt(ttl.Milliseconds(), 10),
		score,
		strconv.Itoa(prof.maxConcurrent),
		sid,
		sessionKeyPrefix,
	).Result()
	if err != nil {
		return "", fmt.Errorf("auth: create session: %w", err)
	}

	m.recordSession(ctx, userID, HashSessionID(sid))
	return sid, nil
}

// Get 读取并校验会话。Redis 错误原样上抛（GC-7：调用方必须 fail-closed，不能当作匿名）；
// 只有「确认不存在/已过期」才返回 ErrSessionNotFound。
func (m *SessionManager) Get(ctx context.Context, sid string) (*Session, error) {
	if !validSessionID(sid) {
		return nil, ErrSessionNotFound
	}
	raw, err := m.rdb.HGetAll(ctx, sessionKey(sid)).Result()
	if err != nil {
		return nil, fmt.Errorf("auth: read session: %w", err)
	}
	if len(raw) == 0 {
		return nil, ErrSessionNotFound
	}
	s, err := sessionFromHash(raw)
	if err != nil {
		return nil, err
	}
	if s.expiredAt(m.now()) {
		// 逻辑过期（例如绝对上限到达）可能早于 Redis TTL 回收；顺手清理，
		// 清理失败不影响「已失效」这一确定性结论。
		_ = m.deleteSession(ctx, sid, s.UserID)
		return nil, ErrSessionNotFound
	}
	return s, nil
}

// Touch 刷新滑动窗口（last_seen_at）与 Redis TTL，并更新并发驱逐索引分数。
// 绝对上限是硬顶：接近 absolute_expires_at 时 TTL 只到绝对上限，不会延长会话。
func (m *SessionManager) Touch(ctx context.Context, sid string) error {
	if !validSessionID(sid) {
		return ErrSessionNotFound
	}
	s, err := m.Get(ctx, sid)
	if err != nil {
		return err
	}

	now := m.now().UTC()
	prof := profileForRole(s.AuthLevel)
	deadline := now.Add(prof.sliding)
	if s.AbsoluteExpiresAt.Before(deadline) {
		deadline = s.AbsoluteExpiresAt
	}
	ttl := deadline.Sub(now)
	if ttl <= 0 {
		_ = m.deleteSession(ctx, sid, s.UserID)
		return ErrSessionNotFound
	}

	ok, err := touchSessionScript.Run(ctx, m.rdb,
		[]string{sessionKey(sid), userSessionsKey(s.UserID)},
		formatSessionTime(now),
		strconv.FormatInt(ttl.Milliseconds(), 10),
		strconv.FormatInt(now.UnixMicro(), 10),
		sid,
	).Int64()
	if err != nil {
		return fmt.Errorf("auth: touch session: %w", err)
	}
	if ok == 0 {
		// 在 Get 与脚本之间被并发吊销/驱逐。
		return ErrSessionNotFound
	}
	return nil
}

// Revoke 删除单个会话（登出、踢出、并发驱逐之外的主动终止）。重复调用幂等。
func (m *SessionManager) Revoke(ctx context.Context, sid string) error {
	if !validSessionID(sid) {
		return nil
	}
	uid, err := m.rdb.HGet(ctx, sessionKey(sid), fieldUserID).Result()
	if errors.Is(err, redis.Nil) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("auth: revoke session: %w", err)
	}
	userID, err := strconv.ParseInt(uid, 10, 64)
	if err != nil {
		return fmt.Errorf("auth: corrupt session record: %w", err)
	}
	return m.deleteSession(ctx, sid, userID)
}

// RevokeUser 吊销 userID 的全部会话，keepCurrent 非空时保留当前会话。
// 用于改密/换绑/删用户/MFA 因子变更等场景（security-01 §2.1 生命周期状态机）。
func (m *SessionManager) RevokeUser(ctx context.Context, userID int64, keepCurrent string) error {
	if keepCurrent != "" && !validSessionID(keepCurrent) {
		// 非法 id 不可能是本用户会话；退化为全部吊销比拼进脚本更安全。
		keepCurrent = ""
	}
	_, err := revokeUserScript.Run(ctx, m.rdb,
		[]string{userSessionsKey(userID)},
		sessionKeyPrefix,
		keepCurrent,
	).Result()
	if err != nil {
		return fmt.Errorf("auth: revoke user sessions: %w", err)
	}
	return nil
}

// SessionInfo 是会话列表的只读投影（F-01-13 / F-01-27）。它刻意不含
// CSRFHash：列表是发给客户端的，任何令牌材料都不应出现在响应里。
//
// SID 是列表行与 DELETE /api/sessions/{sid} 之间的句柄。会话端点只向
// 已认证的本人返回该值，并始终以「sid 属于当前用户」为吊销前提（I-12）。
type SessionInfo struct {
	SID               string
	UA                string
	IP                string
	CreatedAt         time.Time
	LastSeenAt        time.Time
	AbsoluteExpiresAt time.Time
}

const (
	// maxListedSessions 是 F-01-13 冻结的列表上限。
	maxListedSessions = 50
	// sessionListScanLimit 限制单次列表读取的索引成员数。每用户活跃会话
	// 有并发上限（20/5），残留的过期成员按 last_seen 排序必然落在存活会话
	// 之后；扫 200 条足以保证「≤50 条存活会话」不会因残留被截断，同时避免
	// 一个长期高频登录的账号把无界 ZSET 全量拉进内存。
	sessionListScanLimit = 200
)

// ListSessions 返回 userID 当前有效（未滑动/绝对过期）的会话，按 last_seen_at
// 倒序，最多 maxListedSessions 条。
//
// 只读：不刷新 last_seen、不清理索引。Redis 错误原样上抛（调用方 fail-closed）；
// 键已按 TTL 回收的索引成员被跳过——这正是「过期会话不出现」的判定点。
// 归属校验是纵深防御：即使索引被写脏，也不会把他人会话列进本用户列表。
func (m *SessionManager) ListSessions(ctx context.Context, userID int64) ([]SessionInfo, error) {
	members, err := m.rdb.ZRevRangeWithScores(ctx, userSessionsKey(userID), 0, sessionListScanLimit-1).Result()
	if err != nil {
		return nil, fmt.Errorf("auth: list sessions: %w", err)
	}

	out := make([]SessionInfo, 0, min(len(members), maxListedSessions))
	now := m.now()

	// 一次 pipeline 取回全部会话 hash：列表端点可能要看 20 个活跃会话 + 若干
	// 残留成员，逐条 HGETALL 会把延迟累加成可感知的慢查询。
	pipe := m.rdb.Pipeline()
	cmds := make([]*redis.MapStringStringCmd, len(members))
	for i, member := range members {
		sid, ok := member.Member.(string)
		if !ok {
			continue // ZSET 成员总是字符串；类型异常按损坏记录跳过
		}
		cmds[i] = pipe.HGetAll(ctx, sessionKey(sid))
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, fmt.Errorf("auth: list sessions: %w", err)
	}

	for i, member := range members {
		if cmds[i] == nil {
			continue
		}
		sid := member.Member.(string)
		raw := cmds[i].Val()
		if len(raw) == 0 {
			continue // Redis TTL 已回收：索引成员残留，跳过（TC-F-01-13-02）
		}
		s, err := sessionFromHash(raw)
		if err != nil {
			return nil, fmt.Errorf("auth: list sessions: %w", err)
		}
		if s.UserID != userID || s.expiredAt(now) {
			continue
		}
		out = append(out, SessionInfo{
			SID:               sid,
			UA:                s.UA,
			IP:                s.IP,
			CreatedAt:         s.CreatedAt,
			LastSeenAt:        s.LastSeenAt,
			AbsoluteExpiresAt: s.AbsoluteExpiresAt,
		})
	}

	// 索引分数与 last_seen_at 由同一个 Lua 脚本原子写入，理论上已有序；
	// 显式再排序让「按 last_seen 倒序」不依赖分数编码细节。
	sort.Slice(out, func(i, j int) bool { return out[i].LastSeenAt.After(out[j].LastSeenAt) })
	if len(out) > maxListedSessions {
		out = out[:maxListedSessions]
	}
	return out, nil
}

// HashSessionID 返回写 login_sessions.session_id_hash 的 SHA-256 十六进制。
// 单独导出是为了让 DB 侧（Task 4/6）与 Redis 侧使用同一编码，避免哈希口径漂移。
func HashSessionID(sid string) string {
	sum := sha256.Sum256([]byte(sid))
	return hex.EncodeToString(sum[:])
}

// recordSession 异步落库。login_sessions 只是审计副本，失败不得让登录失败；
// context.WithoutCancel 防止请求返回后 ctx 被取消而丢写入，短超时防止 DB
// 卡住时每次登录泄漏一个 goroutine。
func (m *SessionManager) recordSession(ctx context.Context, userID int64, sidHash string) {
	rec := m.recorder
	if rec == nil {
		return
	}
	go func() {
		recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionRecordTimeout)
		defer cancel()
		_ = rec.RecordSession(recordCtx, userID, sidHash)
	}()
}

// deleteSession 原子删除会话 hash 与索引成员。
func (m *SessionManager) deleteSession(ctx context.Context, sid string, userID int64) error {
	_, err := revokeSessionScript.Run(ctx, m.rdb,
		[]string{sessionKey(sid), userSessionsKey(userID)},
		sid,
	).Result()
	if err != nil {
		return fmt.Errorf("auth: delete session: %w", err)
	}
	return nil
}

// expiredAt 实现「滑动与绝对两者先到者生效」（modules-01 F-01-13）。
func (s *Session) expiredAt(now time.Time) bool {
	if !now.Before(s.AbsoluteExpiresAt) {
		return true
	}
	prof := profileForRole(s.AuthLevel)
	return !now.Before(s.LastSeenAt.Add(prof.sliding))
}

func sessionKey(sid string) string { return sessionKeyPrefix + sid }

func userSessionsKey(userID int64) string {
	return userSessionsKeyPrefix + strconv.FormatInt(userID, 10)
}

// newSessionID 生成 base64url(CSPRNG(32 bytes))；CSPRNG 失败时返回错误（fail-closed）。
func newSessionID() (string, error) {
	buf := make([]byte, SessionIDBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("auth: generate session id: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// validSessionID 拒绝畸形 Cookie 值。除了尽早返回 not-found，这也防止攻击者把
// 任意字符串拼进 Redis 键名（例如伪造出 sess:user:{id} 这类索引键）。
func validSessionID(sid string) bool {
	if len(sid) != base64.RawURLEncoding.EncodedLen(SessionIDBytes) {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(sid)
	return err == nil && len(raw) == SessionIDBytes
}

func sessionFromHash(raw map[string]string) (*Session, error) {
	userID, err := strconv.ParseInt(raw[fieldUserID], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("auth: corrupt session record: user_id: %w", err)
	}
	createdAt, err := parseSessionTime(raw[fieldCreatedAt])
	if err != nil {
		return nil, fmt.Errorf("auth: corrupt session record: created_at: %w", err)
	}
	lastSeenAt, err := parseSessionTime(raw[fieldLastSeenAt])
	if err != nil {
		return nil, fmt.Errorf("auth: corrupt session record: last_seen_at: %w", err)
	}
	absoluteExpiresAt, err := parseSessionTime(raw[fieldAbsoluteExpiresAt])
	if err != nil {
		return nil, fmt.Errorf("auth: corrupt session record: absolute_expires_at: %w", err)
	}
	return &Session{
		UserID:            userID,
		UA:                raw[fieldUA],
		IP:                raw[fieldIP],
		CreatedAt:         createdAt,
		LastSeenAt:        lastSeenAt,
		AbsoluteExpiresAt: absoluteExpiresAt,
		AuthLevel:         raw[fieldAuthLevel],
		CSRFHash:          raw[fieldCSRFHash],
	}, nil
}

// 时间以 RFC3339Nano/UTC 存储，便于运维直读 Redis 时无时区歧义。
func formatSessionTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseSessionTime(v string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

// truncateUTF8Bytes 把 s 截断到至多 max 字节且保持合法 UTF-8。
//
// 先经 strings.ToValidUTF8 清洗头部/代理可能带入的 obs-text（HTTP 头允许
// 非 UTF-8 的 0x80–0xFF 字节），再从 max 处向前回退到 rune 起点。直接按
// 字节切会把多字节字符切成半个 rune，Redis 与 JSON 会话列表都会得到非法
// UTF-8。
func truncateUTF8Bytes(s string, max int) string {
	s = strings.ToValidUTF8(s, "")
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// createSessionScript 在一个 Lua 事务里完成「写会话 + 登记索引 + 驱逐」。
//
// 为什么必须原子：并发登录时若分多条命令执行，两个请求可能同时看到 ZCARD==cap
// 并各自驱逐，或刚建的会话在驱逐循环里被误删。Redis 单线程执行脚本，整段不可分割。
//
// 新会话的分数被抬到严格大于当前最大分数，保证即使测试/时钟回拨造成分数并列，
// 驱逐循环也绝不会选中刚创建的会话。
var createSessionScript = redis.NewScript(`
local top = redis.call('ZRANGE', KEYS[2], -1, -1, 'WITHSCORES')
local score = tonumber(ARGV[9])
if top[2] and tonumber(top[2]) >= score then
  score = tonumber(top[2]) + 1
end

redis.call('HSET', KEYS[1],
  'user_id', ARGV[1],
  'ua', ARGV[2],
  'ip', ARGV[3],
  'created_at', ARGV[4],
  'last_seen_at', ARGV[5],
  'absolute_expires_at', ARGV[6],
  'auth_level', ARGV[7])
redis.call('PEXPIRE', KEYS[1], ARGV[8])
redis.call('ZADD', KEYS[2], score, ARGV[11])

local cap = tonumber(ARGV[10])
while redis.call('ZCARD', KEYS[2]) > cap do
  local popped = redis.call('ZPOPMIN', KEYS[2], 1)
  local victim = popped[1]
  if not victim then break end
  redis.call('DEL', ARGV[12] .. victim)
end
return redis.call('ZCARD', KEYS[2])
`)

// touchSessionScript 原子刷新 last_seen_at、TTL 与索引分数；EXISTS 检查关闭
// 「Get 与写入之间被吊销」的竞态。
var touchSessionScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 0 then
  return 0
end
redis.call('HSET', KEYS[1], 'last_seen_at', ARGV[1])
redis.call('PEXPIRE', KEYS[1], ARGV[2])
redis.call('ZADD', KEYS[2], ARGV[3], ARGV[4])
return 1
`)

// revokeSessionScript 删除单个会话及其索引成员。
var revokeSessionScript = redis.NewScript(`
redis.call('DEL', KEYS[1])
redis.call('ZREM', KEYS[2], ARGV[1])
return 1
`)

// revokeUserScript 在一个脚本内遍历用户索引并删除除 keepCurrent 外的全部会话。
// 原子执行避免「遍历后、删除前新登录的会话漏网」的窗口。
var revokeUserScript = redis.NewScript(`
local members = redis.call('ZRANGE', KEYS[1], 0, -1)
local removed = 0
for _, sid in ipairs(members) do
  if sid ~= ARGV[2] then
    redis.call('DEL', ARGV[1] .. sid)
    redis.call('ZREM', KEYS[1], sid)
    removed = removed + 1
  end
end
return removed
`)
