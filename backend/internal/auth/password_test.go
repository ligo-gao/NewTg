// Package auth 的测试：口令哈希、校验、长度策略与泄露筛查。
//
// 依据：security-03 §3.2（ARGON2ID_PARAMS m=19456KiB/t=2/p=1）、
// security-01 §2.3（纯长度策略、离线 top-3000 + 上下文字词表、可选 HIBP k-匿名）、
// M1 计划 R-05（keyLen=32 本计划冻结）与 GC-9（口令只存 argon2id 哈希）。
package auth

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"

	"golang.org/x/crypto/argon2"
)

func TestHashVerify(t *testing.T) {
	const plain = "correct horse battery staple"

	hash, err := HashPassword(plain)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !VerifyPassword(hash, plain) {
		t.Fatal("VerifyPassword rejected the correct password")
	}
	if VerifyPassword(hash, plain+"x") {
		t.Fatal("VerifyPassword accepted a wrong password")
	}
	if VerifyPassword(hash, "Correct horse battery staple") {
		t.Fatal("VerifyPassword must not normalize case")
	}
	if VerifyPassword(hash, "正确 horse battery staple") {
		t.Fatal("VerifyPassword accepted a different password")
	}

	// PHC 自描述格式必须与安全设计逐字一致，参数变化要显式发生（TC-C-10）。
	parts := strings.Split(hash, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" ||
		parts[2] != "v=19" || parts[3] != "m=19456,t=2,p=1" {
		t.Fatalf("unexpected PHC layout: %q", hash)
	}
	salt, err := base64.RawStdEncoding.Strict().DecodeString(parts[4])
	if err != nil {
		t.Fatalf("salt is not unpadded standard base64: %v", err)
	}
	if len(salt) != 16 {
		t.Fatalf("salt length = %d, want 16 bytes (128-bit, security-03 MIN_ENTROPY_BITS)", len(salt))
	}
	key, err := base64.RawStdEncoding.Strict().DecodeString(parts[5])
	if err != nil {
		t.Fatalf("hash is not unpadded standard base64: %v", err)
	}
	if len(key) != 32 {
		t.Fatalf("key length = %d, want 32 bytes (plan R-05)", len(key))
	}

	// 同一口令两次哈希必须使用独立随机盐，否则相同口令会得到相同哈希。
	hash2, err := HashPassword(plain)
	if err != nil {
		t.Fatalf("HashPassword (second): %v", err)
	}
	if hash == hash2 {
		t.Fatal("two hashes of the same password are identical; salt is not random")
	}
	parts2 := strings.Split(hash2, "$")
	if len(parts2) != 6 || parts2[4] == parts[4] {
		t.Fatal("two hashes of the same password use the same salt")
	}
	if !VerifyPassword(hash2, plain) {
		t.Fatal("VerifyPassword rejected the second hash of the same password")
	}
}

// TestHashVerifyNoTruncation 守住 security-01 §2.3「不截断」：bcrypt 的 72 字节
// 截断缺陷不得因换用 argon2id 又混入；多字节口令也必须按完整字节序列校验。
func TestHashVerifyNoTruncation(t *testing.T) {
	prefix := strings.Repeat("A", 72)
	first := prefix + "-first-suffix"
	second := prefix + "-second-suffix"

	hashFirst, err := HashPassword(first)
	if err != nil {
		t.Fatalf("HashPassword(first): %v", err)
	}
	hashSecond, err := HashPassword(second)
	if err != nil {
		t.Fatalf("HashPassword(second): %v", err)
	}
	if !VerifyPassword(hashFirst, first) || !VerifyPassword(hashSecond, second) {
		t.Fatal("passwords longer than 72 bytes must verify in full")
	}
	if VerifyPassword(hashFirst, second) || VerifyPassword(hashSecond, first) {
		t.Fatal("passwords sharing the first 72 bytes must not be interchangeable")
	}

	unicodePwd := strings.Repeat("口令", 60) + "终"
	hashUnicode, err := HashPassword(unicodePwd)
	if err != nil {
		t.Fatalf("HashPassword(unicode): %v", err)
	}
	if !VerifyPassword(hashUnicode, unicodePwd) {
		t.Fatal("multibyte password failed to verify")
	}
	if VerifyPassword(hashUnicode, unicodePwd+"x") {
		t.Fatal("multibyte password with an extra suffix verified")
	}
}

// TestVerifyConstantTime 覆盖伪造/畸形哈希：实现必须在进入 argon2 前完成
// 结构校验与参数上界检查（argon2 对 t=0/p=0 会 panic，上界缺失则可被
// 伪造的 m 值拖入巨型内存分配）；任何输入都不得 panic，只能返回 false。
func TestVerifyConstantTime(t *testing.T) {
	valid, err := HashPassword("valid password 123")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	parts := strings.Split(valid, "$")
	if len(parts) != 6 {
		t.Fatalf("unexpected PHC layout: %q", valid)
	}
	key, err := base64.RawStdEncoding.Strict().DecodeString(parts[5])
	if err != nil {
		t.Fatalf("decode key: %v", err)
	}
	flipped := append([]byte(nil), key...)
	flipped[0] ^= 0xff

	salt16 := base64.RawStdEncoding.EncodeToString(make([]byte, 16))
	key32 := base64.RawStdEncoding.EncodeToString(make([]byte, 32))

	cases := map[string]string{
		"empty":                  "",
		"plain text":             "not-a-phc-hash",
		"wrong algorithm":        "$argon2i$v=19$m=19456,t=2,p=1$" + parts[4] + "$" + parts[5],
		"wrong version":          "$argon2id$v=18$m=19456,t=2,p=1$" + parts[4] + "$" + parts[5],
		"missing key":            "$argon2id$v=19$m=19456,t=2,p=1$" + parts[4] + "$",
		"empty salt":             "$argon2id$v=19$m=19456,t=2,p=1$$" + parts[5],
		"missing field":          "$argon2id$v=19$m=19456,t=2$" + parts[4] + "$" + parts[5],
		"unknown field":          "$argon2id$v=19$m=19456,t=2,p=1,x=9$" + parts[4] + "$" + parts[5],
		"rounds zero":            "$argon2id$v=19$m=19456,t=0,p=1$" + parts[4] + "$" + parts[5],
		"parallelism zero":       "$argon2id$v=19$m=19456,t=2,p=0$" + parts[4] + "$" + parts[5],
		"memory below minimum":   "$argon2id$v=19$m=1,t=2,p=1$" + parts[4] + "$" + parts[5],
		"memory over ceiling":    "$argon2id$v=19$m=4294967295,t=2,p=1$" + parts[4] + "$" + parts[5],
		"threads over ceiling":   "$argon2id$v=19$m=19456,t=2,p=255$" + parts[4] + "$" + parts[5],
		"padded base64":          "$argon2id$v=19$m=19456,t=2,p=1$" + salt16 + "==$" + key32 + "==",
		"invalid base64 salt":    "$argon2id$v=19$m=19456,t=2,p=1$not base64!$" + parts[5],
		"truncated":              strings.Join(parts[:5], "$"),
		"extra section":          valid + "$extra",
		"key too short":          "$argon2id$v=19$m=19456,t=2,p=1$" + parts[4] + "$" + base64.RawStdEncoding.EncodeToString(key[:8]),
		"tampered key":           "$argon2id$v=19$m=19456,t=2,p=1$" + parts[4] + "$" + base64.RawStdEncoding.EncodeToString(flipped),
		"unrelated valid-shaped": "$argon2id$v=19$m=19456,t=2,p=1$" + salt16 + "$" + key32,
	}
	for name, forged := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("VerifyPassword panicked on malformed hash: %v", r)
				}
			}()
			if VerifyPassword(forged, "valid password 123") {
				t.Fatalf("VerifyPassword accepted malformed hash %q", forged)
			}
		})
	}
}

func TestCompromised(t *testing.T) {
	var zero CompromisedPasswordChecker

	hits := []string{
		"password",   // top-3000 第 1 位
		"PASSWORD",   // 大小写不敏感
		"123456",     // top-3000
		"qwerty",     // top-3000
		"1234-5678",  // 去分隔符后命中 12345678
		"p@ssw0rd",   // leet 折叠后命中 password
		"NewTG2026!", // 内置产品名词（security-01 §2.3）
		"n3wtg-2026", // 产品名的 leet/追加数字变体
		"mynewtgpass",
	}
	for _, plain := range hits {
		if !zero.Check(plain) {
			t.Errorf("Check(%q) = false, want offline-list hit", plain)
		}
	}

	misses := []string{
		"correct-horse-battery-staple-42",
		"Tr0ub4dor&3-correct",
	}
	for _, plain := range misses {
		if zero.Check(plain) {
			t.Errorf("Check(%q) = true, want no hit", plain)
		}
	}

	// 站点级上下文词（site.title、部署域名等由调用方注入）。
	siteChecker := NewCompromisedPasswordChecker("acme", "example")
	if !siteChecker.Check("ILoveACME99") {
		t.Error("configured context word was not detected")
	}
	if siteChecker.Check("correct-horse-battery-staple-42") {
		t.Error("unrelated password hit the configured context list")
	}

	// 用户名是每用户上下文，只能在调用点提供。
	if !zero.CheckWithContext("alice-loves-cats-42", "alice") {
		t.Error("per-call context word (username) was not detected")
	}
	if zero.CheckWithContext("alice-loves-cats-42") {
		t.Error("password hit without its username context")
	}
}

func TestOfflineListSize(t *testing.T) {
	// ASVS 6.2.4 / security-01 §2.3：随版本发布的离线表必须 ≥ top-3000。
	entries := 0
	for _, line := range strings.Split(commonPasswordsRaw, "\n") {
		if strings.TrimSpace(line) != "" {
			entries++
		}
	}
	if entries < 3000 {
		t.Fatalf("offline password list has %d entries, want >= 3000", entries)
	}
}

// TestVerifyRejectsBelowBaselineParams 守住参数下限：M1 没有历史哈希，低于
// 冻结基线（m=19456, t=2, p=1, keyLen=32，R-05）的哈希不可能是本系统产出的，
// 只可能被伪造或降级。这里用正确的派生值构造“本可通过校验”的哈希，证明拒绝
// 发生在策略层，而不是碰巧摘要不匹配。
func TestVerifyRejectsBelowBaselineParams(t *testing.T) {
	const plain = "valid password 123"
	salt := make([]byte, 16)
	saltB64 := base64.RawStdEncoding.EncodeToString(salt)

	encode := func(rounds, memory uint32, keyLen uint32) string {
		key := argon2.IDKey([]byte(plain), salt, rounds, memory, 1, keyLen)
		return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=1$%s$%s",
			memory, rounds, saltB64, base64.RawStdEncoding.EncodeToString(key))
	}

	// 下限用字面量而不是生成常量：若未来调高 argon2Time/argon2MemoryKiB，
	// 「低于生成常量」的旧哈希仍是合法历史数据，必须继续可校验。这里钉死
	// 真正冻结的基线 19456/2/32。
	cases := map[string]string{
		"rounds below baseline": encode(2-1, 19456, 32),
		"memory below baseline": encode(2, 19456-1, 32),
		"key below baseline":    encode(2, 19456, 32/2),
	}
	for name, hash := range cases {
		t.Run(name, func(t *testing.T) {
			if VerifyPassword(hash, plain) {
				t.Fatalf("VerifyPassword accepted below-baseline hash %q", hash)
			}
		})
	}
}

// TestVerifyAcceptsHigherCostHash 证明下限冻结的动机：PHC 自描述允许未来
// 调高生成成本后，旧实现（本函数）仍按哈希记录的参数完成校验，而不是把
// 全部旧哈希当成伪造拒绝。
func TestVerifyAcceptsHigherCostHash(t *testing.T) {
	const plain = "valid password 456"
	salt := make([]byte, 16)
	key := argon2.IDKey([]byte(plain), salt, 3, 19456*2, 2, 32)
	hash := fmt.Sprintf("$argon2id$v=19$m=%d,t=3,p=2$%s$%s",
		19456*2, base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key))
	if !VerifyPassword(hash, plain) {
		t.Fatal("VerifyPassword rejected a higher-cost PHC hash within the verify window")
	}
}

// TestCompromisedSymbolOnlyHIBP：纯符号口令（8 位，纯长度策略下合法）归一化
// 后为空，但启用 HIBP 时仍必须查询泄露库——归一化 guard 不能吞掉外部查询
// （security-01 TC-A-08）。
func TestCompromisedSymbolOnlyHIBP(t *testing.T) {
	const plain = "!!!!!!!!"
	if !ValidatePasswordLength(plain) {
		t.Fatal("test fixture must satisfy the pure-length policy")
	}

	sum := sha1.Sum([]byte(plain))
	full := strings.ToUpper(hex.EncodeToString(sum[:]))
	var gotPrefix string
	checker := CompromisedPasswordChecker{
		PwnedRange: func(prefix string) (string, error) {
			gotPrefix = prefix
			return full[5:] + ":9\r\n", nil
		},
	}
	if !checker.Check(plain) {
		t.Fatal("symbol-only password skipped the HIBP lookup")
	}
	if gotPrefix != full[:5] {
		t.Fatalf("HIBP prefix = %q, want %q", gotPrefix, full[:5])
	}
}

func TestCompromisedHIBP(t *testing.T) {
	const plain = "hibp-probe-passphrase"
	sum := sha1.Sum([]byte(plain))
	full := strings.ToUpper(hex.EncodeToString(sum[:]))

	var gotPrefix string
	checker := CompromisedPasswordChecker{
		PwnedRange: func(prefix string) (string, error) {
			gotPrefix = prefix
			// 真 HIBP Range 响应：本地只需匹配后缀，永不发送完整口令。
			return "00112233445566778899AABBCCDDEEFF0011:2\r\n" + full[5:] + ":7\r\n", nil
		},
	}
	if !checker.Check(plain) {
		t.Fatal("HIBP range response with matching suffix was not detected")
	}
	if gotPrefix != full[:5] {
		t.Fatalf("HIBP prefix = %q, want %q", gotPrefix, full[:5])
	}
	if len(gotPrefix) != 5 {
		t.Fatalf("HIBP prefix length = %d, want exactly 5 hex chars (k-anonymity)", len(gotPrefix))
	}

	checker.PwnedRange = func(string) (string, error) {
		return "00112233445566778899AABBCCDDEEFF0011:2\r\n", nil
	}
	if checker.Check(plain) {
		t.Fatal("HIBP response without the suffix reported a hit")
	}

	// 外部查询是可选增强（security-01 §2.3）：故障时回落到离线表，不能把
	// 网络错误当成命中而拒绝全部口令。
	checker.PwnedRange = func(string) (string, error) {
		return "", errors.New("hibp unavailable")
	}
	if checker.Check(plain) {
		t.Fatal("HIBP error must fall back to the offline list, not report a hit")
	}

	// 离线表命中时不得再发起外部查询（隐私与性能）。
	called := false
	checker.PwnedRange = func(string) (string, error) {
		called = true
		return "", nil
	}
	if !checker.Check("password") {
		t.Fatal("offline top-3000 hit was lost")
	}
	if called {
		t.Fatal("offline hit still queried HIBP")
	}
}

func TestPolicy(t *testing.T) {
	// 常量值本身也是冻结契约（F-01-01 / security-01 §2.3）。
	if PWDMinLen != 8 || PWDMaxLen != 128 {
		t.Fatalf("PWDMinLen/PWDMaxLen = %d/%d, want 8/128", PWDMinLen, PWDMaxLen)
	}

	cases := []struct {
		name string
		pwd  string
		want bool
	}{
		{"7 chars", "1234567", false},
		{"8 digits", "12345678", true}, // TC-A-07：纯数字但 ≥8 位必须接受
		{"8 lowercase", "aaaaaaaa", true},
		{"8 symbols", "!!!!!!!!", true}, // 不做字符组成限制
		{"128 chars", strings.Repeat("a", 128), true},
		{"129 chars", strings.Repeat("a", 129), false},
		{"empty", "", false},
		{"8 CJK runes", strings.Repeat("密", 8), true},
		{"129 CJK runes", strings.Repeat("密", 129), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ValidatePasswordLength(tc.pwd); got != tc.want {
				t.Fatalf("ValidatePasswordLength(%q) = %v, want %v", tc.pwd, got, tc.want)
			}
		})
	}
}
