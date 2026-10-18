package auth

import (
	"encoding/base32"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"
)

// 本文件是 Task 7 的领域原语验收：TOTP（RFC 6238/4226）、备用码生成/校验、
// AES-256-GCM envelope。全部为纯内存测试，不依赖外部 PG/Redis（GC-8）。

const backupCodeHashPrefix = "$argon2id$v=19$m=19456,t=2,p=1$"

// TestTOTPRFC6238Vectors 用 RFC 6238 附录 B 的 SHA-1 测试密钥与时间点验证
// 6 位截断实现（RFC 向量为 8 位，取末 6 位即 Google Authenticator 口径）。
func TestTOTPRFC6238Vectors(t *testing.T) {
	secret := base32.StdEncoding.
		WithPadding(base32.NoPadding).
		EncodeToString([]byte("12345678901234567890"))
	if secret != "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ" {
		t.Fatalf("测试密钥 base32 = %q，与 RFC 6238 附录 B 不符", secret)
	}

	vectors := []struct {
		unix int64
		code string
	}{
		{59, "287082"},
		{1111111109, "081804"},
		{1111111111, "050471"},
		{1234567890, "005924"},
		{2000000000, "279037"},
		{20000000000, "353130"},
	}
	for _, v := range vectors {
		now := time.Unix(v.unix, 0)
		if !VerifyTOTP(secret, v.code, now) {
			t.Errorf("VerifyTOTP(T=%d, %s) = false，期望 true", v.unix, v.code)
		}
		if VerifyTOTP(secret, "000000", now) && v.code != "000000" {
			t.Errorf("VerifyTOTP(T=%d, 000000) = true，期望 false", v.unix)
		}
	}
}

// TestTOTPRejectsAdjacentSteps 覆盖 TC-A-09：±0 窗口下上一步/下一步的码必须
// 被拒绝，同一步内的时间点都接受。
func TestTOTPRejectsAdjacentSteps(t *testing.T) {
	secret, err := GenerateTOTPSecret()
	if err != nil {
		t.Fatalf("GenerateTOTPSecret: %v", err)
	}
	key, ok := decodeTOTPSecret(secret)
	if !ok {
		t.Fatal("生成的 secret 无法解码")
	}

	// 固定在一个步进窗口的中间，避免跨步边界造成的偶发失败。
	step := int64(1_700_000_000 / TOTPStepSeconds)
	now := time.Unix(step*TOTPStepSeconds+TOTPStepSeconds/2, 0)

	current := hotpCode(key, uint64(step))
	previous := hotpCode(key, uint64(step-1))
	next := hotpCode(key, uint64(step+1))

	if !VerifyTOTP(secret, current, now) {
		t.Fatal("当前步的码应被接受")
	}
	if VerifyTOTP(secret, previous, now) {
		t.Fatal("上一步的码必须被拒绝（±0 窗口，TC-A-09）")
	}
	if VerifyTOTP(secret, next, now) {
		t.Fatal("下一步的码必须被拒绝（±0 窗口，TC-A-09）")
	}

	// 同一步的起点与终点仍是「当前步」；跨过 step+30s 后原码立即失效。
	stepStart := time.Unix(step*TOTPStepSeconds, 0)
	stepEnd := time.Unix(step*TOTPStepSeconds+TOTPStepSeconds-1, 0)
	if !VerifyTOTP(secret, current, stepStart) || !VerifyTOTP(secret, current, stepEnd) {
		t.Fatal("当前步内任意时刻应接受同一步的码")
	}
	if VerifyTOTP(secret, current, stepEnd.Add(time.Second)) {
		t.Fatal("进入下一步后，上一步的码必须失效")
	}
}

// TestTOTPRejectsMalformedInput 冻结「不可信输入不 panic、不误放行」。
func TestTOTPRejectsMalformedInput(t *testing.T) {
	secret, err := GenerateTOTPSecret()
	if err != nil {
		t.Fatalf("GenerateTOTPSecret: %v", err)
	}
	key, _ := decodeTOTPSecret(secret)
	now := time.Unix(1_700_000_015, 0)
	valid := hotpCode(key, uint64(now.Unix()/TOTPStepSeconds))

	if !VerifyTOTP(secret, " "+valid+" ", now) {
		t.Error("code 两侧空白应被容忍")
	}
	for _, code := range []string{"", "12345", "1234567", "abcdef", "12345a", "１２３４５６"} {
		if VerifyTOTP(secret, code, now) {
			t.Errorf("畸形 code %q 不得通过", code)
		}
	}
	for _, badSecret := range []string{"", "   ", "not-base32!!", "1"} {
		if VerifyTOTP(badSecret, valid, now) {
			t.Errorf("畸形 secret %q 不得通过", badSecret)
		}
	}
	// 小写/缺填充的 secret 仍可校验（认证器迁移的常见形态）。
	lower := strings.ToLower(secret)
	if !VerifyTOTP(lower, valid, now) {
		t.Error("小写 secret 应被接受（base32 大小写不敏感）")
	}
}

// TestGenerateTOTPSecret 冻结 160-bit 熵与 base32 无填充形态。
func TestGenerateTOTPSecret(t *testing.T) {
	a, err := GenerateTOTPSecret()
	if err != nil {
		t.Fatalf("GenerateTOTPSecret: %v", err)
	}
	b, err := GenerateTOTPSecret()
	if err != nil {
		t.Fatalf("GenerateTOTPSecret: %v", err)
	}
	if a == b {
		t.Fatal("两次生成的 secret 不得相同")
	}
	if got := len(a); got != 32 {
		t.Fatalf("secret 长度 = %d，期望 32（20 字节 base32）", got)
	}
	raw, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(a)
	if err != nil || len(raw) != totpSecretBytes {
		t.Fatalf("secret 不是 20 字节 base32：err=%v len=%d", err, len(raw))
	}
}

// TestGenerateBackupCodes 覆盖 brief (b) 的格式/唯一性/argon2id+盐断言。
func TestGenerateBackupCodes(t *testing.T) {
	plain, hashes, err := GenerateBackupCodes(3)
	if err != nil {
		t.Fatalf("GenerateBackupCodes: %v", err)
	}
	if len(plain) != 3 || len(hashes) != 3 {
		t.Fatalf("len(plain)=%d len(hashes)=%d，期望 3/3", len(plain), len(hashes))
	}

	seen := map[string]bool{}
	for i, code := range plain {
		if !backupCodePattern.MatchString(code) {
			t.Errorf("备用码 %q 不符合 XXXX-XXXX", code)
		}
		if seen[code] {
			t.Errorf("备用码 %q 重复", code)
		}
		seen[code] = true
		if !strings.HasPrefix(hashes[i], backupCodeHashPrefix) {
			t.Errorf("哈希 %q 不是冻结的 argon2id 参数形态", hashes[i])
		}
		// 每次哈希独立随机盐：同一明文的另一次哈希必须不同。
		again, err := HashPassword(code)
		if err != nil {
			t.Fatalf("HashPassword: %v", err)
		}
		if again == hashes[i] {
			t.Errorf("备用码 %q 的两次哈希相同，盐未随机化", code)
		}
	}

	// 命中时返回对应的哈希；未命中返回 false。
	matched, ok := VerifyBackupCode(hashes, plain[0])
	if !ok || matched != hashes[0] {
		t.Fatalf("VerifyBackupCode(%q) = (%q, %v)，期望命中 hashes[0]", plain[0], matched, ok)
	}
	if _, ok := VerifyBackupCode(hashes, "ZZZZ-ZZZZ"); ok && !seen["ZZZZ-ZZZZ"] {
		t.Fatal("不存在的备用码不得通过")
	}
	// 归一化：小写与缺连字符都指向同一个码。
	if _, ok := VerifyBackupCode(hashes, strings.ToLower(plain[0])); !ok {
		t.Error("小写备用码应被接受")
	}
	if _, ok := VerifyBackupCode(hashes, strings.ReplaceAll(plain[0], "-", "")); !ok {
		t.Error("无连字符的 8 字符备用码应被接受")
	}
	if _, ok := VerifyBackupCode(hashes, "ABC"); ok {
		t.Error("长度不符的备用码不得通过")
	}
}

func TestGenerateBackupCodesZeroAndNegative(t *testing.T) {
	plain, hashes, err := GenerateBackupCodes(0)
	if err != nil || len(plain) != 0 || len(hashes) != 0 {
		t.Fatalf("GenerateBackupCodes(0) = (%v, %v, %v)，期望空结果", plain, hashes, err)
	}
	if _, _, err := GenerateBackupCodes(-1); err == nil {
		t.Fatal("负数 n 必须返回错误")
	}
}

// TestNormalizeBackupCode 冻结服务端归一化口径（与前端自动大写/补连字符等价）。
func TestNormalizeBackupCode(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"7K2M-9QXA", "7K2M-9QXA", true},
		{"7k2m-9qxa", "7K2M-9QXA", true},
		{" 7k2m9qxa ", "7K2M-9QXA", true},
		{"7K2M 9QXA", "7K2M-9QXA", true},
		{"7K2M-9QX", "", false},
		{"7K2M-9QXA-", "", false},
		{"7K2M_9QXA", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		got, ok := NormalizeBackupCode(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("NormalizeBackupCode(%q) = (%q, %v)，期望 (%q, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// ---------------------------------------------------------------------------
// AES-256-GCM envelope
// ---------------------------------------------------------------------------

const testCredentialKey = "task7-test-credential-encryption-key"

func mustCipher(t *testing.T, key string) *SecretCipher {
	t.Helper()
	c, err := NewSecretCipher(key)
	if err != nil {
		t.Fatalf("NewSecretCipher: %v", err)
	}
	return c
}

func TestSecretCipherRoundTrip(t *testing.T) {
	c := mustCipher(t, testCredentialKey)
	const secret = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"

	enc, err := c.Encrypt(secret)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if strings.Contains(enc, secret) {
		t.Fatal("密文中不得出现明文")
	}
	got, err := c.Decrypt(enc)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if got != secret {
		t.Fatalf("Decrypt = %q，期望 %q", got, secret)
	}

	// 每条记录新 DEK + 新 nonce：同一明文的两次封装必须不同。
	enc2, err := c.Encrypt(secret)
	if err != nil {
		t.Fatalf("Encrypt(2): %v", err)
	}
	if enc == enc2 {
		t.Fatal("同一明文的两次封装不得相同（DEK/nonce 复用）")
	}

	// 空明文也应可往返（envelope 不假设非空）。
	empty, err := c.Encrypt("")
	if err != nil {
		t.Fatalf("Encrypt(empty): %v", err)
	}
	if got, err := c.Decrypt(empty); err != nil || got != "" {
		t.Fatalf("空明文往返 = (%q, %v)，期望空串", got, err)
	}
}

// TestSecretCipherEnvelopeLayout 钉死 security-03 §2.4 的冻结布局。
func TestSecretCipherEnvelopeLayout(t *testing.T) {
	c := mustCipher(t, testCredentialKey)
	enc, err := c.Encrypt("hello")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	raw, err := base64.RawStdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatalf("envelope 不是 raw base64: %v", err)
	}
	if len(raw) < envelopeHeaderBytes+wrappedDEKBytes+gcmNonceBytes+gcmTagBytes {
		t.Fatalf("envelope 长度 = %d，过短", len(raw))
	}
	if raw[0] != envelopeVersion || raw[1] != envelopeKeyID || raw[2] != envelopeAlgAESGCM {
		t.Fatalf("头部 = %v，期望 version/key_id/alg = %d/%d/%d",
			raw[:3], envelopeVersion, envelopeKeyID, envelopeAlgAESGCM)
	}
	if got := int(raw[3])<<8 | int(raw[4]); got != wrappedDEKBytes {
		t.Fatalf("wrapped_dek_len = %d，期望 %d", got, wrappedDEKBytes)
	}
}

func TestSecretCipherTamperFails(t *testing.T) {
	c := mustCipher(t, testCredentialKey)
	enc, err := c.Encrypt("GEZDGNBVGY3TQOJQ")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	raw, _ := base64.RawStdEncoding.DecodeString(enc)

	// 对头部（version/key_id/alg/长度）、DEK 包裹体、nonce、密文与 tag 的每个
	// 区域各翻一位：全部必须返回 ErrSecretDecrypt，绝不部分解密。
	for name, offset := range map[string]int{
		"version":     0,
		"key_id":      1,
		"alg_id":      2,
		"wrapped_dek": envelopeHeaderBytes + 3,
		"dek_nonce":   envelopeHeaderBytes + 1,
		"nonce":       envelopeHeaderBytes + wrappedDEKBytes + 1,
		"ciphertext":  envelopeHeaderBytes + wrappedDEKBytes + gcmNonceBytes,
		"tag":         len(raw) - 1,
	} {
		t.Run(name, func(t *testing.T) {
			tampered := append([]byte(nil), raw...)
			tampered[offset] ^= 0x01
			if _, err := c.Decrypt(base64.RawStdEncoding.EncodeToString(tampered)); err == nil {
				t.Fatalf("翻转 %s 后仍解密成功", name)
			}
		})
	}

	for name, bad := range map[string]string{
		"not-base64": "!!!not-base64!!!",
		"truncated":  base64.RawStdEncoding.EncodeToString(raw[:10]),
		"empty":      "",
	} {
		if _, err := c.Decrypt(bad); err == nil {
			t.Errorf("%s envelope 不得解密成功", name)
		}
	}

	// 换一把主密钥（等价于 key_id 指向错误版本）。
	other := mustCipher(t, "another-credential-key")
	if _, err := other.Decrypt(enc); err == nil {
		t.Fatal("错误主密钥不得解出明文")
	}
}

func TestNewSecretCipherKeyEncodings(t *testing.T) {
	if _, err := NewSecretCipher(""); err == nil {
		t.Fatal("空 CREDENTIAL_ENCRYPTION_KEY 必须 fail-fast")
	}

	key := make([]byte, gcmKeyBytes)
	for i := range key {
		key[i] = byte(i)
	}
	for name, encoded := range map[string]string{
		"base64":     base64.StdEncoding.EncodeToString(key),
		"base64-raw": base64.RawStdEncoding.EncodeToString(key),
		"hex":        fmt.Sprintf("%x", key),
		"passphrase": "plain-text-deployment-secret",
	} {
		t.Run(name, func(t *testing.T) {
			c := mustCipher(t, encoded)
			enc, err := c.Encrypt("round-trip")
			if err != nil {
				t.Fatalf("Encrypt: %v", err)
			}
			if got, err := c.Decrypt(enc); err != nil || got != "round-trip" {
				t.Fatalf("Decrypt = (%q, %v)", got, err)
			}
		})
	}
}
