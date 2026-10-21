package auth

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// 本文件是 Task 8 全量再认证门（security-01 §2.6/§3.1、GC-10）的单元验收：
// REAUTH_WINDOW=5min，因子=当前密码 + 已启用 MFA 时的 TOTP，失败必须返回
// ErrReauthRequired 且不留下任何「已再认证」记录（TC-A-21 的「不落库」在
// 门这一层的语义就是窗口键不得被写入）。

const reauthTestPassword = "CorrectHorse9!"

var reauthTestPasswordHash = func() string {
	h, err := HashPassword(reauthTestPassword)
	if err != nil {
		panic(err)
	}
	return h
}()

// fakeReauthSource 是 ReauthUserSource 的内存实现；nil material 表示用户不存在。
type fakeReauthSource struct {
	materials map[int64]*ReauthMaterial
	err       error
	calls     int
}

func (f *fakeReauthSource) ReauthMaterial(_ context.Context, userID int64) (*ReauthMaterial, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.materials[userID], nil
}

func newReauthTestGate(t *testing.T, source ReauthUserSource, verify TOTPVerifier) (*ReauthGate, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	return NewReauthGate(rdb, source, verify), mr
}

// TestReauthGatePasswordOnly 覆盖未启用 MFA 的账号：正确密码通过并开启窗口，
// 错误密码/空密码一律 ErrReauthRequired。
func TestReauthGatePasswordOnly(t *testing.T) {
	source := &fakeReauthSource{materials: map[int64]*ReauthMaterial{
		7: {PasswordHash: reauthTestPasswordHash},
	}}
	gate, mr := newReauthTestGate(t, source, nil)
	ctx := context.Background()

	// 窗口外且无因子：必须拒绝，不能把「从未再认证」当成已认证。
	if err := gate.Authorize(ctx, 7, ReauthFactor{}); !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("空因子且无窗口 err = %v，期望 ErrReauthRequired", err)
	}

	if err := gate.Authorize(ctx, 7, ReauthFactor{Password: "wrong-password"}); !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("错误密码 err = %v，期望 ErrReauthRequired", err)
	}
	if mr.Exists("reauth:7") {
		t.Fatal("失败的再认证不得写入窗口键（TC-A-21 不落库）")
	}

	if err := gate.Authorize(ctx, 7, ReauthFactor{Password: reauthTestPassword}); err != nil {
		t.Fatalf("正确密码 err = %v，期望 nil", err)
	}
	if ttl := mr.TTL("reauth:7"); ttl != ReauthWindow {
		t.Fatalf("窗口 TTL = %v，期望 %v", ttl, ReauthWindow)
	}

	// 窗口内空因子直接通过：这是「自最近一次成功再认证起算」的语义。
	if err := gate.Authorize(ctx, 7, ReauthFactor{}); err != nil {
		t.Fatalf("窗口内空因子 err = %v，期望 nil", err)
	}

	// 未知用户没有任何口令材料，必须拒绝。
	if err := gate.Authorize(ctx, 404, ReauthFactor{Password: "anything"}); !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("未知用户 err = %v，期望 ErrReauthRequired", err)
	}
}

// TestReauthGateTOTP 覆盖已启用 MFA：必须同时提供正确 TOTP，缺失或错误都拒绝；
// 注入的校验器必须是唯一验证入口（Task 7 落地后由装配层注入 auth.VerifyTOTP），
// 且必须收到服务端注入的时钟。
func TestReauthGateTOTP(t *testing.T) {
	const secret = "JBSWY3DPEHPK3PXP"
	fixedNow := time.Date(2026, 10, 18, 12, 30, 0, 0, time.UTC)
	var seenSecret, seenCode string
	var seenNow time.Time
	verify := func(s, code string, now time.Time) bool {
		seenSecret, seenCode, seenNow = s, code, now
		return s == secret && code == "123456"
	}
	source := &fakeReauthSource{materials: map[int64]*ReauthMaterial{
		9: {PasswordHash: reauthTestPasswordHash, TOTPEnabled: true, TOTPSecret: secret},
	}}
	gate, mr := newReauthTestGate(t, source, verify)
	gate.setClock(func() time.Time { return fixedNow })
	ctx := context.Background()

	if err := gate.Authorize(ctx, 9, ReauthFactor{Password: reauthTestPassword}); !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("缺 TOTP err = %v，期望 ErrReauthRequired", err)
	}
	if err := gate.Authorize(ctx, 9, ReauthFactor{Password: reauthTestPassword, TOTPCode: "000000"}); !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("错误 TOTP err = %v，期望 ErrReauthRequired", err)
	}
	if mr.Exists("reauth:9") {
		t.Fatal("缺/错 TOTP 不得写入窗口键")
	}

	if err := gate.Authorize(ctx, 9, ReauthFactor{Password: reauthTestPassword, TOTPCode: "123456"}); err != nil {
		t.Fatalf("正确密码+TOTP err = %v，期望 nil", err)
	}
	if seenSecret != secret || seenCode != "123456" {
		t.Fatalf("校验器收到 secret=%q code=%q，期望 %q/123456", seenSecret, seenCode, secret)
	}
	if !seenNow.Equal(fixedNow) {
		t.Fatalf("校验器收到时钟 = %v，期望注入的 %v", seenNow, fixedNow)
	}
}

// TestReauthGateTOTPWithoutVerifierFailsClosed：已启用 MFA 但装配层忘了注入
// 校验器时必须拒绝，绝不能退化成「只验密码」。
func TestReauthGateTOTPWithoutVerifierFailsClosed(t *testing.T) {
	source := &fakeReauthSource{materials: map[int64]*ReauthMaterial{
		10: {PasswordHash: reauthTestPasswordHash, TOTPEnabled: true, TOTPSecret: "JBSWY3DPEHPK3PXP"},
	}}
	gate, _ := newReauthTestGate(t, source, nil)

	err := gate.Authorize(context.Background(), 10,
		ReauthFactor{Password: reauthTestPassword, TOTPCode: "123456"})
	if !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("未注入 TOTP 校验器 err = %v，期望 ErrReauthRequired（fail-closed）", err)
	}
}

// TestReauthGateWindowExpiry 覆盖「5 分钟窗口自最近一次成功再认证起算」的
// 边界：4:59 仍有效、5:01 失效、再次成功刷新 TTL。窗口过期以 Redis TTL 为
// 权威（不是进程时钟），因此用 miniredis 推进真实 TTL。
func TestReauthGateWindowExpiry(t *testing.T) {
	source := &fakeReauthSource{materials: map[int64]*ReauthMaterial{
		11: {PasswordHash: reauthTestPasswordHash},
	}}
	gate, mr := newReauthTestGate(t, source, nil)
	ctx := context.Background()
	if err := gate.Authorize(ctx, 11, ReauthFactor{Password: reauthTestPassword}); err != nil {
		t.Fatalf("首次再认证 err = %v", err)
	}

	// 4:59：窗口仍在。
	mr.FastForward(ReauthWindow - time.Second)
	if err := gate.Authorize(ctx, 11, ReauthFactor{}); err != nil {
		t.Fatalf("4:59 窗口内 err = %v，期望 nil", err)
	}

	// 再前进 2 秒越过 5 分钟：窗口失效。
	mr.FastForward(2 * time.Second)
	if err := gate.Authorize(ctx, 11, ReauthFactor{}); !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("5:01 窗口外 err = %v，期望 ErrReauthRequired", err)
	}

	// 重新认证成功后窗口重新计时。
	if err := gate.Authorize(ctx, 11, ReauthFactor{Password: reauthTestPassword}); err != nil {
		t.Fatalf("二次再认证 err = %v", err)
	}
	if ttl := mr.TTL("reauth:11"); ttl != ReauthWindow {
		t.Fatalf("刷新后的窗口 TTL = %v，期望 %v", ttl, ReauthWindow)
	}
}

// TestReauthGatePasswordlessUserFailsClosed：OAuth-only 用户没有口令，不能
// 用空口令通过再认证。
func TestReauthGatePasswordlessUserFailsClosed(t *testing.T) {
	source := &fakeReauthSource{materials: map[int64]*ReauthMaterial{
		12: {PasswordHash: ""},
	}}
	gate, mr := newReauthTestGate(t, source, nil)

	if err := gate.Authorize(context.Background(), 12, ReauthFactor{Password: ""}); !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("无口令用户 err = %v，期望 ErrReauthRequired", err)
	}
	if mr.Exists("reauth:12") {
		t.Fatal("无口令用户不得获得再认证窗口")
	}
}

// TestReauthGateSourceErrorFailsClosed：存储故障必须以普通 error 上抛（调用方
// 应映射 503），绝不能伪装成已认证或 ErrReauthRequired 之外的放行。
func TestReauthGateSourceErrorFailsClosed(t *testing.T) {
	source := &fakeReauthSource{err: errors.New("db down")}
	gate, _ := newReauthTestGate(t, source, nil)

	err := gate.Authorize(context.Background(), 13, ReauthFactor{Password: "whatever"})
	if err == nil || errors.Is(err, ErrReauthRequired) {
		t.Fatalf("存储故障 err = %v，期望普通错误（非 ErrReauthRequired）", err)
	}
}

// TestReauthGateRedisFailureFailsClosed：窗口写入失败时不得返回 nil——否则
// 调用方会以为再认证已生效。
func TestReauthGateRedisFailureFailsClosed(t *testing.T) {
	source := &fakeReauthSource{materials: map[int64]*ReauthMaterial{
		14: {PasswordHash: reauthTestPasswordHash},
	}}
	gate, mr := newReauthTestGate(t, source, nil)
	mr.Close()

	err := gate.Authorize(context.Background(), 14, ReauthFactor{Password: reauthTestPassword})
	if err == nil {
		t.Fatal("Redis 故障时 Authorize 不得返回 nil（fail-closed）")
	}
}

// TestReauthClearWindow：改密/重置后必须能清除既有窗口，避免旧凭证换来的
// 认证继续授权后续敏感操作。
func TestReauthClearWindow(t *testing.T) {
	source := &fakeReauthSource{materials: map[int64]*ReauthMaterial{
		15: {PasswordHash: reauthTestPasswordHash},
	}}
	gate, mr := newReauthTestGate(t, source, nil)
	ctx := context.Background()

	if err := gate.Authorize(ctx, 15, ReauthFactor{Password: reauthTestPassword}); err != nil {
		t.Fatalf("再认证 err = %v", err)
	}
	if err := gate.Clear(ctx, 15); err != nil {
		t.Fatalf("Clear err = %v", err)
	}
	if mr.Exists("reauth:15") {
		t.Fatal("Clear 后窗口键仍存在")
	}
	if err := gate.Authorize(ctx, 15, ReauthFactor{}); !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("清除后空因子 err = %v，期望 ErrReauthRequired", err)
	}
}

// TestReauthGateFailureLockout：连续 5 次失败后锁定 15 分钟，锁定期间即使
// 提交正确因子也必须 429（*ReauthLockedError）；锁定到期后恢复。
func TestReauthGateFailureLockout(t *testing.T) {
	source := &fakeReauthSource{materials: map[int64]*ReauthMaterial{
		21: {PasswordHash: reauthTestPasswordHash},
	}}
	gate, mr := newReauthTestGate(t, source, nil)
	ctx := context.Background()

	for i := 0; i < ReauthMaxFailures; i++ {
		err := gate.Authorize(ctx, 21, ReauthFactor{Password: "wrong"})
		if !errors.Is(err, ErrReauthRequired) {
			t.Fatalf("第 %d 次失败 err = %v，期望 ErrReauthRequired", i+1, err)
		}
	}

	err := gate.Authorize(ctx, 21, ReauthFactor{Password: reauthTestPassword})
	var locked *ReauthLockedError
	if !errors.As(err, &locked) {
		t.Fatalf("锁定后正确密码 err = %v，期望 *ReauthLockedError", err)
	}
	if !errors.Is(err, ErrReauthLocked) {
		t.Fatalf("errors.Is(err, ErrReauthLocked) = false（err=%v）", err)
	}
	if locked.RetryAfter <= 0 || locked.RetryAfter > ReauthLockDuration {
		t.Fatalf("RetryAfter = %v，期望 (0, %v]", locked.RetryAfter, ReauthLockDuration)
	}
	if mr.Exists("reauth:21") {
		t.Fatal("锁定期间不得写入再认证窗口")
	}

	// 锁定到期后恢复。
	mr.FastForward(ReauthLockDuration + time.Second)
	if err := gate.Authorize(ctx, 21, ReauthFactor{Password: reauthTestPassword}); err != nil {
		t.Fatalf("锁定到期后 err = %v，期望 nil", err)
	}
}

// TestReauthGateSuccessResetsFailures：成功再认证清零连续失败计数，
// 4 次失败 + 成功 + 4 次失败不应锁定（第 5 次连续失败才触发锁定）。
func TestReauthGateSuccessResetsFailures(t *testing.T) {
	source := &fakeReauthSource{materials: map[int64]*ReauthMaterial{
		22: {PasswordHash: reauthTestPasswordHash},
	}}
	gate, _ := newReauthTestGate(t, source, nil)
	ctx := context.Background()

	for i := 0; i < ReauthMaxFailures-1; i++ {
		if err := gate.Authorize(ctx, 22, ReauthFactor{Password: "wrong"}); !errors.Is(err, ErrReauthRequired) {
			t.Fatalf("前置失败 err = %v", err)
		}
	}
	if err := gate.Authorize(ctx, 22, ReauthFactor{Password: reauthTestPassword}); err != nil {
		t.Fatalf("成功再认证 err = %v", err)
	}
	for i := 0; i < ReauthMaxFailures-1; i++ {
		err := gate.Authorize(ctx, 22, ReauthFactor{Password: "wrong"})
		if !errors.Is(err, ErrReauthRequired) {
			t.Fatalf("复位后第 %d 次失败 err = %v（应仍是 ErrReauthRequired，未锁定）", i+1, err)
		}
	}
	// 第 5 次连续失败后应进入锁定。
	if err := gate.Authorize(ctx, 22, ReauthFactor{Password: "wrong"}); !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("第 5 次连续失败 err = %v，期望 ErrReauthRequired", err)
	}
	var locked *ReauthLockedError
	if err := gate.Authorize(ctx, 22, ReauthFactor{Password: reauthTestPassword}); !errors.As(err, &locked) {
		t.Fatalf("第 5 次失败后 err = %v，期望 *ReauthLockedError", err)
	}
}

// TestReauthGateLimiterErrorFailsClosed：限流器故障不得放行。
func TestReauthGateLimiterErrorFailsClosed(t *testing.T) {
	source := &fakeReauthSource{materials: map[int64]*ReauthMaterial{
		23: {PasswordHash: reauthTestPasswordHash},
	}}
	gate, _ := newReauthTestGate(t, source, nil)
	gate.SetLimiter(reauthErrorLimiter{err: errors.New("limiter down")})

	err := gate.Authorize(context.Background(), 23, ReauthFactor{Password: reauthTestPassword})
	if err == nil || errors.Is(err, ErrReauthRequired) {
		t.Fatalf("限流器故障 err = %v，期望普通错误（fail-closed）", err)
	}
}

type reauthErrorLimiter struct{ err error }

func (l reauthErrorLimiter) Check(context.Context, int64) error { return l.err }
func (l reauthErrorLimiter) Record(context.Context, int64, bool) error {
	return l.err
}

// TestRedisReauthLimiterHealsMissingTTL 覆盖 A4：计数器存在但无 TTL（旧版本/
// 人工写入的异常数据）时，Check 必须重写完整锁定窗口，而不是只返回锁定却
// 让键永不过期（事实上的永久锁定）。
func TestRedisReauthLimiterHealsMissingTTL(t *testing.T) {
	ctx := context.Background()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })

	// 直接写入无过期时间的失败计数：PTTL 返回 -1。
	key := reauthFailureKey(31)
	if err := rdb.Set(ctx, key, strconv.Itoa(ReauthMaxFailures), 0).Err(); err != nil {
		t.Fatalf("seed counter: %v", err)
	}

	limiter := NewRedisReauthLimiter(rdb)
	err := limiter.Check(ctx, 31)
	var locked *ReauthLockedError
	if !errors.As(err, &locked) {
		t.Fatalf("Check = %v，期望 *ReauthLockedError", err)
	}
	if locked.RetryAfter != ReauthLockDuration {
		t.Fatalf("RetryAfter = %v，期望完整窗口 %v", locked.RetryAfter, ReauthLockDuration)
	}
	ttl, err := rdb.PTTL(ctx, key).Result()
	if err != nil {
		t.Fatalf("PTTL after heal: %v", err)
	}
	if ttl <= 0 || ttl > ReauthLockDuration {
		t.Fatalf("修复后的 TTL = %v，期望 (0, %v]", ttl, ReauthLockDuration)
	}

	// 自愈必须是真的：窗口过去后锁定解除。
	mr.FastForward(ReauthLockDuration + time.Second)
	if err := limiter.Check(ctx, 31); err != nil {
		t.Fatalf("窗口过期后 Check = %v，期望 nil（不得永久锁定）", err)
	}
}

// ---------------------------------------------------------------------------
// W11：AuthorizeOnce（security-03 §2.10 步进认证）
// ---------------------------------------------------------------------------

// TestAuthorizeOncePasswordOnlyDoesNotOpenWindow：未启用 MFA 的账号只接受
// password；成功后**不得**写 reauth:<userID> 窗口键（步进认证只授权本次请求）。
func TestAuthorizeOncePasswordOnlyDoesNotOpenWindow(t *testing.T) {
	source := &fakeReauthSource{materials: map[int64]*ReauthMaterial{
		41: {PasswordHash: reauthTestPasswordHash},
	}}
	gate, mr := newReauthTestGate(t, source, nil)
	ctx := context.Background()

	// 空因子不能沿用任何窗口（与 Authorize 的关键差异）。
	if err := gate.AuthorizeOnce(ctx, 41, ReauthFactor{}); !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("空因子 err = %v，期望 ErrReauthRequired", err)
	}
	if err := gate.AuthorizeOnce(ctx, 41, ReauthFactor{Password: "wrong"}); !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("错误密码 err = %v，期望 ErrReauthRequired", err)
	}
	if mr.Exists("reauth:41") {
		t.Fatal("失败的步进认证不得写入窗口键")
	}
	if err := gate.AuthorizeOnce(ctx, 41, ReauthFactor{Password: reauthTestPassword}); err != nil {
		t.Fatalf("正确密码 err = %v，期望 nil", err)
	}
	if mr.Exists("reauth:41") {
		t.Fatal("AuthorizeOnce 成功后不得写再认证窗口（只授权本次请求）")
	}
	// 再次空因子仍然拒绝：证明没有可复用的免认证窗口。
	if err := gate.AuthorizeOnce(ctx, 41, ReauthFactor{}); !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("成功后空因子 err = %v，期望 ErrReauthRequired（无窗口）", err)
	}
}

// TestAuthorizeOnceTOTPShape：已启用 MFA 时只接受 totp_code；给密码/同时给
// 两种因子都是 *ReauthFactorShapeError（422 字段级），且不计数失败锁定。
func TestAuthorizeOnceTOTPShape(t *testing.T) {
	const secret = "JBSWY3DPEHPK3PXP"
	source := &fakeReauthSource{materials: map[int64]*ReauthMaterial{
		42: {PasswordHash: reauthTestPasswordHash, TOTPEnabled: true, TOTPSecret: secret},
	}}
	gate, mr := newReauthTestGate(t, source, func(s, code string, _ time.Time) bool {
		return s == secret && code == "123456"
	})
	ctx := context.Background()

	for _, factor := range []ReauthFactor{
		{Password: reauthTestPassword},
		{Password: reauthTestPassword, TOTPCode: "123456"},
	} {
		err := gate.AuthorizeOnce(ctx, 42, factor)
		var shape *ReauthFactorShapeError
		if !errors.As(err, &shape) || shape.Required != "totp_code" {
			t.Fatalf("factor=%+v err = %v，期望 Required=totp_code 的 shape 错误", factor, err)
		}
	}
	// 形态错误不计数：三次形态错误后仍能正常通过。
	for i := 0; i < 3; i++ {
		_ = gate.AuthorizeOnce(ctx, 42, ReauthFactor{Password: reauthTestPassword})
	}
	if err := gate.AuthorizeOnce(ctx, 42, ReauthFactor{TOTPCode: "123456"}); err != nil {
		t.Fatalf("正确 TOTP err = %v，期望 nil（形态错误不得触发锁定）", err)
	}
	if mr.Exists("reauth:42") {
		t.Fatal("TOTP 步进认证成功后不得写再认证窗口")
	}
	// 错误 TOTP 是真正的因子失败 → ErrReauthRequired。
	if err := gate.AuthorizeOnce(ctx, 42, ReauthFactor{TOTPCode: "000000"}); !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("错误 TOTP err = %v，期望 ErrReauthRequired", err)
	}
}

// TestAuthorizeOncePasswordAccountRejectsTOTP：未启用 MFA 时给 totp_code 是
// 形态错误（Required=password），不能退化成密码校验。
func TestAuthorizeOncePasswordAccountRejectsTOTP(t *testing.T) {
	source := &fakeReauthSource{materials: map[int64]*ReauthMaterial{
		43: {PasswordHash: reauthTestPasswordHash},
	}}
	gate, _ := newReauthTestGate(t, source, nil)

	var shape *ReauthFactorShapeError
	err := gate.AuthorizeOnce(context.Background(), 43, ReauthFactor{TOTPCode: "123456"})
	if !errors.As(err, &shape) || shape.Required != "password" {
		t.Fatalf("err = %v，期望 Required=password 的 shape 错误", err)
	}
}

// TestAuthorizeOnceFailureLockShared：连续 5 次错误因子后锁定，与 Authorize
// 共用同一 reauth_fail 桶。
func TestAuthorizeOnceFailureLockShared(t *testing.T) {
	source := &fakeReauthSource{materials: map[int64]*ReauthMaterial{
		44: {PasswordHash: reauthTestPasswordHash},
	}}
	gate, _ := newReauthTestGate(t, source, nil)
	ctx := context.Background()

	for i := 0; i < ReauthMaxFailures; i++ {
		if err := gate.AuthorizeOnce(ctx, 44, ReauthFactor{Password: "wrong"}); !errors.Is(err, ErrReauthRequired) {
			t.Fatalf("第 %d 次错误 err = %v，期望 ErrReauthRequired", i+1, err)
		}
	}
	var locked *ReauthLockedError
	if err := gate.AuthorizeOnce(ctx, 44, ReauthFactor{Password: reauthTestPassword}); !errors.As(err, &locked) {
		t.Fatalf("锁定后 err = %v，期望 *ReauthLockedError（即使因子正确）", err)
	}
}

// TestAuthorizeOnceTOTPWithoutVerifierFailsClosed：已启用 MFA 但未注入校验器
// 时必须拒绝，绝不能把「无法验证 TOTP」当成通过。
func TestAuthorizeOnceTOTPWithoutVerifierFailsClosed(t *testing.T) {
	source := &fakeReauthSource{materials: map[int64]*ReauthMaterial{
		45: {PasswordHash: reauthTestPasswordHash, TOTPEnabled: true, TOTPSecret: "JBSWY3DPEHPK3PXP"},
	}}
	gate, _ := newReauthTestGate(t, source, nil)

	if err := gate.AuthorizeOnce(context.Background(), 45, ReauthFactor{TOTPCode: "123456"}); !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("未注入 TOTP 校验器 err = %v，期望 ErrReauthRequired（fail-closed）", err)
	}
}
