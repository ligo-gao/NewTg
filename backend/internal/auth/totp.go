// TOTP 双因子与备用码原语（modules-01 F-01-08 / F-01-09 / F-01-10）。
//
// 冻结口径：
//
//   - TOTP：30 秒步进、6 位、HMAC-SHA1（RFC 6238 与 Google Authenticator 兼容），
//     接受窗口 ±0 步——只接受「当前步」的码，上一步/下一步一律拒绝（TC-A-09）。
//     时间由调用方以 now 参数注入：生产传服务端时间（NTP 同步的系统时钟），
//     测试传真值，绝不以客户端提交的时间为准。
//
//   - 备用码：8 组 XXXX-XXXX（4-4 位大写字母数字）；落库只存 argon2id 带盐哈希，
//     明文仅在生成响应中出现一次。单码熵约 41 bit，低于可安全使用快速哈希的
//     112 bit 门槛，故必须用慢哈希（security-03 §2.3 的 lookup secret 分档）。
//
//   - TOTP 种子加密：AES-256-GCM envelope（security-03 §2.4 冻结布局）：
//
//     envelope := version(1B) || key_id(1B) || alg_id(1B) || wrapped_dek_len(2B)
//     || wrapped_dek || nonce(12B) || ciphertext(n) || tag(16B)
//
//     每条记录随机生成 256-bit DEK，DEK 仅用于该条记录且被主密钥包裹；
//     因此「同一 DEK + nonce」不可能重复（§2.4 的 nonce 不变量）。
//     主密钥来自 CREDENTIAL_ENCRYPTION_KEY；解密失败一律返回统一错误、绝不降级。
package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const (
	// TOTPStepSeconds / TOTPDigits 是 modules-01 F-01-08 的冻结值。
	TOTPStepSeconds = 30
	TOTPDigits      = 6

	// totpSecretBytes 取 160 bit：RFC 4226 §4 要求共享密钥至少 128 bit，
	// 160 bit 与 Google Authenticator 的常见实现一致，base32 后为 32 字符。
	totpSecretBytes = 20

	// backupCodeAlphabet 是 XXXX-XXXX 的字符集（36 符号，大小写不敏感——
	// 服务端统一转大写）。禁止新增小写，否则同一码的两种写法会命中不同哈希。
	backupCodeAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	backupCodeSymbols  = 8
)

// sixDigitCodePattern 与服务端其它 6 位验证码保持同口径：纯 ASCII 数字。
var sixDigitCodePattern = regexp.MustCompile(`^[0-9]{6}$`)

// backupCodePattern 是 modules-01 F-01-09 冻结的字段正则。
var backupCodePattern = regexp.MustCompile(`^[A-Z0-9]{4}-[A-Z0-9]{4}$`)

// ValidateTOTPSecret 只接受服务端 GenerateTOTPSecret 冻结的形状：32 个 base32
// 字符（大小写不敏感，无填充），解码后恰为 20 字节。
//
// enable 端点必须在验证/落库前调用它：否则客户端可以提交任意自选字节串
// （甚至畸形 base32）当种子。只接受固定形状并不降低安全性——secret 由服务端
// setup 生成，客户端没有理由提交其它长度的种子。
func ValidateTOTPSecret(secret string) bool {
	s := strings.TrimSpace(secret)
	if len(s) != 32 {
		return false
	}
	key, ok := decodeTOTPSecret(s)
	return ok && len(key) == totpSecretBytes
}

// GenerateTOTPSecret 生成 base32（无填充）的 TOTP 共享密钥。
// 熵源是 crypto/rand（OS CSPRNG）；失败即返回错误，绝不退化为弱随机。
func GenerateTOTPSecret() (string, error) {
	buf := make([]byte, totpSecretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("auth: generate totp secret: %w", err)
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf), nil
}

// VerifyTOTP 校验 6 位 TOTP 码。窗口固定为 ±0 步（Skew=0），即只接受 now
// 所在 30 秒步的码；上一步与下一步的码都会失败（TC-A-09）。
//
// now 必须由服务端提供（生产为 NTP 同步的系统时钟）：客户端时钟偏差不得影响
// 校验结果（F-01-08 验收标准）。实现遵循 RFC 6238（HMAC-SHA1、动态截断），
// 与 Google Authenticator 的默认参数一致；畸形 secret/code 一律返回 false，
// 不 panic、不区分失败原因。比较使用 crypto/subtle 常量时间。
func VerifyTOTP(secret, code string, now time.Time) bool {
	code = strings.TrimSpace(code)
	if !sixDigitCodePattern.MatchString(code) {
		return false
	}
	// 1970 之前没有定义 TOTP 步进；负 Unix 秒转 uint64 会得到巨大计数器。
	if now.Unix() < 0 {
		return false
	}
	key, ok := decodeTOTPSecret(secret)
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare(
		[]byte(hotpCode(key, uint64(now.Unix()/TOTPStepSeconds))),
		[]byte(code),
	) == 1
}

// decodeTOTPSecret 按 RFC 4648 base32 解码共享密钥，容忍小写与缺失填充
// （认证器迁移/手输的常见形态）。空密钥或非法字母表直接拒绝。
func decodeTOTPSecret(secret string) ([]byte, bool) {
	s := strings.ToUpper(strings.TrimSpace(secret))
	if s == "" {
		return nil, false
	}
	if n := len(s) % 8; n != 0 {
		s += strings.Repeat("=", 8-n)
	}
	key, err := base32.StdEncoding.DecodeString(s)
	if err != nil || len(key) == 0 {
		return nil, false
	}
	return key, true
}

// hotpCode 实现 RFC 4226 §5.3 的动态截断，输出 6 位零填充十进制。
func hotpCode(key []byte, counter uint64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], counter)
	mac := hmac.New(sha1.New, key)
	_, _ = mac.Write(msg[:])
	sum := mac.Sum(nil)

	offset := sum[len(sum)-1] & 0x0f
	value := int64(sum[offset]&0x7f)<<24 |
		int64(sum[offset+1])<<16 |
		int64(sum[offset+2])<<8 |
		int64(sum[offset+3])
	return fmt.Sprintf("%06d", value%1_000_000)
}

// GenerateBackupCodes 生成 n 组互不相同的备用码，返回明文与 argon2id 哈希。
//
// 明文形如 "7K2M-9QXA"（仅在此次调用结果中出现，调用方必须一次性下发且不落库/
// 不写日志）。每码独立随机盐，因此同一码的两次哈希不同；哈希串复用
// HashPassword 的 PHC 形态（m=19456,t=2,p=1），与口令存储同一成本基线。
func GenerateBackupCodes(n int) (plain []string, hashes []string, err error) {
	if n < 0 {
		return nil, nil, fmt.Errorf("auth: backup code count must be non-negative, got %d", n)
	}
	plain = make([]string, 0, n)
	hashes = make([]string, 0, n)
	seen := make(map[string]struct{}, n)
	for len(plain) < n {
		code, err := generateBackupCode()
		if err != nil {
			return nil, nil, err
		}
		// 空间足够大（36^8）时重试实际不会发生；仍显式去重，保证「8 组互不相同」
		// 是生成器保证而不是概率保证。
		if _, dup := seen[code]; dup {
			continue
		}
		seen[code] = struct{}{}
		hash, err := HashPassword(code)
		if err != nil {
			return nil, nil, fmt.Errorf("auth: hash backup code: %w", err)
		}
		plain = append(plain, code)
		hashes = append(hashes, hash)
	}
	return plain, hashes, nil
}

// generateBackupCode 用拒绝采样从 36 符号表中取 8 个字符，避免 256%36 的取模偏差。
func generateBackupCode() (string, error) {
	// 256 不是 36 的整数倍：>= limit 的字节丢弃后重采，保证每个符号等概率。
	limit := byte(256 - 256%len(backupCodeAlphabet))
	symbols := make([]byte, 0, backupCodeSymbols)
	buf := make([]byte, 16)
	for len(symbols) < backupCodeSymbols {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("auth: generate backup code: %w", err)
		}
		for _, b := range buf {
			if b >= limit {
				continue
			}
			symbols = append(symbols, backupCodeAlphabet[int(b)%len(backupCodeAlphabet)])
			if len(symbols) == backupCodeSymbols {
				break
			}
		}
	}
	return string(symbols[:4]) + "-" + string(symbols[4:]), nil
}

// NormalizeBackupCode 把用户输入归一化为冻结格式：去首尾/内部空白、转大写，
// 缺连字符的 8 字符输入按 4-4 补上（前端粘贴自动补连字符的服务端等价物）。
// 归一化后仍不符合 XXXX-XXXX 的输入返回 ok=false——不做模糊匹配，避免把
// 输错的码静默重解释成另一个有效码。
func NormalizeBackupCode(raw string) (string, bool) {
	s := strings.ToUpper(strings.TrimSpace(raw))
	s = strings.ReplaceAll(s, " ", "")
	if len(s) == backupCodeSymbols {
		s = s[:4] + "-" + s[4:]
	}
	if !backupCodePattern.MatchString(s) {
		return "", false
	}
	return s, true
}

// VerifyBackupCode 在哈希数组中恒定时间地查找匹配项，返回命中的哈希。
//
// 不短路：即使已命中仍校验剩余条目，避免「匹配位置」通过响应时延泄露。
// 命中项由调用方用于条件 UPDATE 原子核销（见 httpapi.ConsumeBackupCode）。
func VerifyBackupCode(hashes []string, rawCode string) (matched string, ok bool) {
	code, ok := NormalizeBackupCode(rawCode)
	if !ok {
		return "", false
	}
	for _, hash := range hashes {
		if VerifyPassword(hash, code) && matched == "" {
			matched = hash
		}
	}
	return matched, matched != ""
}

// ---------------------------------------------------------------------------
// AES-256-GCM envelope（security-03 §2.4）
// ---------------------------------------------------------------------------

const (
	// 冻结常量（security-03 §3.2）。
	gcmKeyBytes   = 32
	gcmNonceBytes = 12
	gcmTagBytes   = 16

	envelopeVersion   byte = 1 // ENVELOPE_VERSION
	envelopeKeyID     byte = 1 // 环境变量主密钥只有一代；KMS 演进时递增
	envelopeAlgAESGCM byte = 1

	// 头部 = version(1) + key_id(1) + alg_id(1) + wrapped_dek_len(2)。
	envelopeHeaderBytes = 5
	// wrapped_dek = nonce(12) + DEK 密文(32) + tag(16)。
	wrappedDEKBytes = gcmNonceBytes + gcmKeyBytes + gcmTagBytes
	// 上限防御：本系统只加密短 TOTP 种子，超出即为伪造/损坏输入。
	maxEnvelopeBytes = 4 << 10
	// envelopeOverheadBytes 是固定开销：头部 + DEK 包裹体 + 主 nonce + tag。
	envelopeOverheadBytes = envelopeHeaderBytes + wrappedDEKBytes + gcmNonceBytes + gcmTagBytes
	// maxPlaintextBytes 与解密侧上限自洽：Encrypt 接受的最长明文封装后恰好
	// 不超过 maxEnvelopeBytes，不会出现「能加密但解不开」的边界。
	maxPlaintextBytes = maxEnvelopeBytes - envelopeOverheadBytes
)

// ErrSecretDecrypt 是解密失败的统一哨兵（security-03 §2.5）：调用方只能
// 判定「不可用」，不得据此区分密钥缺失、格式错误或 tag 不符。
var ErrSecretDecrypt = errors.New("auth: credential decryption failed")

// SecretCipher 用 CREDENTIAL_ENCRYPTION_KEY 派生主密钥，封装/解封装每条记录的
// 随机 DEK。零值不可用，必须经 NewSecretCipher 构造。
type SecretCipher struct {
	masterKey [gcmKeyBytes]byte
}

// NewSecretCipher 从部署密钥构造 cipher。
//
// 主密钥的编码优先级：32 字节 base64（标准或 raw）→ 64 位十六进制 → 其余按
// UTF-8 口令做 SHA-256 拉伸。最后一种兼容现有的纯文本 env 部署；生产建议直接
// 下发 256-bit 随机值的 base64，避免把低熵口令带进 KMS 之前的过渡期。
// 空密钥属于装配错误，必须 fail-fast。
func NewSecretCipher(masterKey string) (*SecretCipher, error) {
	raw := strings.TrimSpace(masterKey)
	if raw == "" {
		return nil, errors.New("auth: CREDENTIAL_ENCRYPTION_KEY is required for credential encryption")
	}
	c := &SecretCipher{}
	switch {
	case decodeInto(c.masterKey[:], raw, base64.StdEncoding.DecodeString):
	case decodeInto(c.masterKey[:], raw, base64.RawStdEncoding.DecodeString):
	case decodeInto(c.masterKey[:], raw, hex.DecodeString):
	default:
		c.masterKey = sha256.Sum256([]byte(raw))
	}
	return c, nil
}

// decodeInto 在 decode 成功且长度恰为 32 字节时写入 dst 并返回 true。
func decodeInto(dst []byte, encoded string, decode func(string) ([]byte, error)) bool {
	b, err := decode(encoded)
	if err != nil || len(b) != len(dst) {
		return false
	}
	copy(dst, b)
	return true
}

// Encrypt 把明文封装成 base64（无填充）的 envelope 文本。
// 每次调用生成新的 DEK 与两个独立 nonce；任何 CSPRNG 失败都返回错误。
func (c *SecretCipher) Encrypt(plaintext string) (string, error) {
	if len(plaintext) > maxPlaintextBytes {
		return "", fmt.Errorf("auth: plaintext too large for envelope (%d bytes)", len(plaintext))
	}
	dek := make([]byte, gcmKeyBytes)
	if _, err := rand.Read(dek); err != nil {
		return "", fmt.Errorf("auth: generate data encryption key: %w", err)
	}
	defer clear(dek) // 尽力清零（Go 无保证，security-03 §2.8 列为已知限制）

	wrapped, err := sealGCM(c.masterKey[:], dek)
	if err != nil {
		return "", fmt.Errorf("auth: wrap data encryption key: %w", err)
	}
	nonce := make([]byte, gcmNonceBytes)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("auth: generate envelope nonce: %w", err)
	}
	aead, err := newGCM(dek)
	if err != nil {
		return "", fmt.Errorf("auth: init envelope cipher: %w", err)
	}
	ciphertext := aead.Seal(nil, nonce, []byte(plaintext), nil)

	out := make([]byte, 0, envelopeHeaderBytes+len(wrapped)+len(nonce)+len(ciphertext))
	out = append(out, envelopeVersion, envelopeKeyID, envelopeAlgAESGCM)
	out = binary.BigEndian.AppendUint16(out, uint16(len(wrapped)))
	out = append(out, wrapped...)
	out = append(out, nonce...)
	out = append(out, ciphertext...)
	return base64.RawStdEncoding.EncodeToString(out), nil
}

// Decrypt 解封装 envelope。版本/key_id/算法未知、长度越界、tag 不符或 base64
// 非法一律返回 ErrSecretDecrypt，绝不返回部分明文（security-03 §2.5）。
func (c *SecretCipher) Decrypt(encoded string) (string, error) {
	raw, err := base64.RawStdEncoding.Strict().DecodeString(strings.TrimSpace(encoded))
	if err != nil || len(raw) > maxEnvelopeBytes || len(raw) < envelopeHeaderBytes+wrappedDEKBytes+gcmNonceBytes+gcmTagBytes {
		return "", ErrSecretDecrypt
	}
	if raw[0] != envelopeVersion || raw[1] != envelopeKeyID || raw[2] != envelopeAlgAESGCM {
		return "", ErrSecretDecrypt
	}
	wrappedLen := int(binary.BigEndian.Uint16(raw[3:5]))
	// DEK 包裹体长度是固定的：长度字段只允许精确匹配，防止解析器被构造输入
	// 引到错位的 nonce/ciphertext 边界。
	if wrappedLen != wrappedDEKBytes {
		return "", ErrSecretDecrypt
	}
	off := envelopeHeaderBytes
	if len(raw) < off+wrappedLen+gcmNonceBytes+gcmTagBytes {
		return "", ErrSecretDecrypt
	}
	wrapped := raw[off : off+wrappedLen]
	off += wrappedLen
	nonce := raw[off : off+gcmNonceBytes]
	off += gcmNonceBytes
	ciphertext := raw[off:]

	dek, err := openGCM(c.masterKey[:], wrapped)
	if err != nil {
		return "", ErrSecretDecrypt
	}
	defer clear(dek)

	aead, err := newGCM(dek)
	if err != nil {
		return "", ErrSecretDecrypt
	}
	plaintext, err := aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", ErrSecretDecrypt
	}
	defer clear(plaintext)
	return string(plaintext), nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// sealGCM 输出 nonce || ciphertext || tag，使包裹体自包含（nonce 不是秘密）。
func sealGCM(key, plaintext []byte) ([]byte, error) {
	aead, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcmNonceBytes)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return aead.Seal(nonce, nonce, plaintext, nil), nil
}

func openGCM(key, sealed []byte) ([]byte, error) {
	if len(sealed) < gcmNonceBytes+gcmTagBytes {
		return nil, ErrSecretDecrypt
	}
	aead, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce, ciphertext := sealed[:gcmNonceBytes], sealed[gcmNonceBytes:]
	return aead.Open(nil, nonce, ciphertext, nil)
}
