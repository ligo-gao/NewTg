package httpapi

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// 本文件实现渠道凭据的落库封装（GC-8）与展示掩码（modules-06 §1.5）。
//
// 安全边界：
//   - 明文只在本包内存中出现，绝不写日志、绝不进 RFC7807 detail（GC-6）；
//   - Open 的错误一律归一为 ErrCredentialSealInvalid，不携带明文片段；
//   - nonce 每次 Seal 重新随机（GCM 复用 nonce 会灾难性失去保密性）。

// credentialSealPrefix 是密文版本前缀。留版本位是为了未来主密钥轮换/算法升级
// 时能识别旧格式；当前只有 v1（AES-256-GCM，nonce 前置）。
const credentialSealPrefix = "v1:"

// ErrCredentialSealInvalid 覆盖「前缀/编码/认证标签」全部失败：
// 调用方无法区分原因，也不应该区分（避免成为密文有效性探测信道）。
var ErrCredentialSealInvalid = errors.New("httpapi: invalid sealed credential")

// SecretSealer 是 AES-256-GCM 封口器。零值不可用，必须经 NewSecretSealer。
type SecretSealer struct {
	aead cipher.AEAD
}

// NewSecretSealer 按 CREDENTIAL_ENCRYPTION_KEY 派生主密钥（GC-8）：
//
//	原始值 base64 解码后恰好 32 字节 → 直接作为 AES-256 密钥；
//	否则 → SHA-256(原始值) 作为密钥。
//
// 为什么允许两种形态：部署环境常直接放随机串（非 base64 的 32 字节文本），
// 强求解码会让配置期无故失败；而 base64 形态便于运维生成/轮换 32 字节密钥。
// 两种形态都落到 32 字节，不会出现 AES 密钥长度错误。
func NewSecretSealer(rawKey string) *SecretSealer {
	key := deriveSealerKey(rawKey)
	block, err := aes.NewCipher(key)
	if err != nil {
		// 32 字节密钥不可能失败；若发生属编程错误，直接 panic 而非静默降级。
		panic(fmt.Sprintf("httpapi: init credential cipher: %v", err))
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		panic(fmt.Sprintf("httpapi: init credential GCM: %v", err))
	}
	return &SecretSealer{aead: aead}
}

func deriveSealerKey(rawKey string) []byte {
	// 先试带填充、再试无填充的标准 base64；只有恰好 32 字节才认。
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding} {
		if decoded, err := enc.DecodeString(rawKey); err == nil && len(decoded) == 32 {
			return decoded
		}
	}
	sum := sha256.Sum256([]byte(rawKey))
	return sum[:]
}

// Seal 返回 "v1:" + base64(nonce || ciphertext)。
// nonce 是 GCM 的标准 12 字节；认证标签已包含在 ciphertext 尾部。
func (s *SecretSealer) Seal(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("httpapi: generate credential nonce: %w", err)
	}
	body := s.aead.Seal(nil, nonce, plaintext, nil)
	buf := make([]byte, 0, len(nonce)+len(body))
	buf = append(buf, nonce...)
	buf = append(buf, body...)
	return []byte(credentialSealPrefix + base64.StdEncoding.EncodeToString(buf)), nil
}

// Open 解开 Seal 的输出；任何失败都返回 ErrCredentialSealInvalid。
func (s *SecretSealer) Open(sealed []byte) ([]byte, error) {
	str := string(sealed)
	if !strings.HasPrefix(str, credentialSealPrefix) {
		return nil, ErrCredentialSealInvalid
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(str, credentialSealPrefix))
	if err != nil {
		return nil, ErrCredentialSealInvalid
	}
	ns := s.aead.NonceSize()
	if len(raw) < ns+s.aead.Overhead() {
		return nil, ErrCredentialSealInvalid
	}
	plaintext, err := s.aead.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		return nil, ErrCredentialSealInvalid
	}
	return plaintext, nil
}

// ChannelCredentials 是 channels.credentials 解密后的 JSON 形状。
//
// deployment / api_version 只对 azure_openai 有意义（relay-02 §6、modules-06
// F-06-03）；其他类型写入时会被剥离，避免把无关字段当密钥长期保存。
type ChannelCredentials struct {
	APIKey     string `json:"api_key"`
	Deployment string `json:"deployment,omitempty"`
	APIVersion string `json:"api_version,omitempty"`
}

// ChannelCredentialsMasked 是详情接口的凭据掩码（modules-06 §1.5 第 2 条）：
// api_key 掩码；deployment/api_version 非密钥，不脱敏。
type ChannelCredentialsMasked struct {
	APIKey     string `json:"api_key"`
	Deployment string `json:"deployment,omitempty"`
	APIVersion string `json:"api_version,omitempty"`
}

// MaskAPIKey 实现 modules-06 §1.5：
//
//	长度 ≥ 7：前 3 位 + "…" + 后 4 位（如 sk-…a1b2）；
//	长度 < 7：仅 "…" + 后 4 位（不足 4 位则显示全部）；
//	空串：空串（未设置，不伪造掩码）。
//
// 用 Unicode 省略号 "…" 而不是三个点，与设计文案一致。
func MaskAPIKey(key string) string {
	if key == "" {
		return ""
	}
	if len(key) < 7 {
		return "…" + lastN(key, 4)
	}
	return key[:3] + "…" + key[len(key)-4:]
}

func lastN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// maskCredentials 把明文凭据投影为掩码视图；调用方必须持有解密后的明文。
func maskCredentials(creds ChannelCredentials) ChannelCredentialsMasked {
	return ChannelCredentialsMasked{
		APIKey:     MaskAPIKey(creds.APIKey),
		Deployment: creds.Deployment,
		APIVersion: creds.APIVersion,
	}
}
