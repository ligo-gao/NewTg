package auth

import (
	"context"
	"errors"
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
// 注入的校验器必须是唯一验证入口（Task 7 落地后由装配层注入 auth.VerifyTOTP）。
func TestReauthGateTOTP(t *testing.T) {
	const secret = "JBSWY3DPEHPK3PXP"
	var seenSecret, seenCode string
	verify := func(s, code string, _ time.Time) bool {
		seenSecret, seenCode = s, code
		return s == secret && code == "123456"
	}
	source := &fakeReauthSource{materials: map[int64]*ReauthMaterial{
		9: {PasswordHash: reauthTestPasswordHash, TOTPEnabled: true, TOTPSecret: secret},
	}}
	gate, mr := newReauthTestGate(t, source, verify)
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

// TestReauthGateWindowExpiry 覆盖「5 分钟窗口自最近一次成功再认证起算」：
// 窗口过期后空因子失效；再次成功会重新计时（刷新窗口）。
func TestReauthGateWindowExpiry(t *testing.T) {
	source := &fakeReauthSource{materials: map[int64]*ReauthMaterial{
		11: {PasswordHash: reauthTestPasswordHash},
	}}
	gate, mr := newReauthTestGate(t, source, nil)
	ctx := context.Background()

	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	gate.setClock(func() time.Time { return now })

	if err := gate.Authorize(ctx, 11, ReauthFactor{Password: reauthTestPassword}); err != nil {
		t.Fatalf("首次再认证 err = %v", err)
	}

	// 窗口内（4:59）仍然有效。
	now = now.Add(ReauthWindow - time.Second)
	if err := gate.Authorize(ctx, 11, ReauthFactor{}); err != nil {
		t.Fatalf("窗口内 err = %v，期望 nil", err)
	}

	// 越过 5 分钟后，Redis TTL 已到期（miniredis 需手动推进）。
	mr.FastForward(ReauthWindow + time.Second)
	if err := gate.Authorize(ctx, 11, ReauthFactor{}); !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("窗口过期 err = %v，期望 ErrReauthRequired", err)
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
