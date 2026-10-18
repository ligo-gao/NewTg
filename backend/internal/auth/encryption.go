package auth

// 登录密码前端加密（F-01-25 / modules-01 §3.14）。
//
// 分层：
//   - 纯密码学：RSA-OAEP(SHA-256) 与 v2 信封（OAEP 包裹的 AES-256-GCM 一次性
//     密钥）的加解密、kid 推导；
//   - 防降级判定：DecodePasswordPayload 是登录 handler 唯一入口；
//   - 双槽密钥治理：LoginKeyManager 负责首次生成、轮换与 previous 槽保留窗口。
//
// 反 oracle 口径：任何解密/格式/kid/防降级失败都返回同一个 ErrPasswordPayload，
// 错误串固定、不带内部原因；Reason 字段只供指标/日志分类，调用方不得据此
// 改变公开响应（modules-01 §3.14 I-9）。
//
// 本文件不依赖数据库：持久化由 LoginKeyStore 接缝注入（生产实现见
// httpapi.SQLLoginKeyStore）。

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	// LoginKeyBits 是服务端 RSA 密钥的最小位数（modules-01 §3.14：RSA ≥2048）。
	LoginKeyBits = 2048
	// LoginKeyIDBytes 是 kid 截取的 SPKI 摘要字节数：sha256(SPKI DER)[:16]。
	LoginKeyIDBytes = 16
	// PreviousLoginKeyRetention 是旧槽保留窗口：必须大于前端公钥缓存 5 分钟
	// （I-7 冻结为 ≥15 分钟）。
	PreviousLoginKeyRetention = 15 * time.Minute

	passwordEnvelopeV2 = "v2"
	// passwordV2OAEPLabel 把 v2 的密钥包裹与其它 OAEP 用途域隔离。
	passwordV2OAEPLabel = "password-v2"
	// passwordV2AADPrefix 与 kid 拼接成 AAD，把密文绑定到具体密钥槽。
	passwordV2AADPrefix = "password-v2:"

	// maxPasswordPlaintextBytes 是 PWDMaxLen 个字符的最坏 UTF-8 字节数。
	// 与参照侧一致：超过它的密文直接按畸形输入拒绝。
	maxPasswordPlaintextBytes = PWDMaxLen * utf8.UTFMax
	// maxEncryptedPayloadBytes 是载荷长度上限，防御超大输入拖垮 RSA/GCM。
	maxEncryptedPayloadBytes = 4096

	passwordNonceBytes  = 12
	passwordAESKeyBytes = 32
	passwordTagBytes    = 16
)

// 内部原因码。只用于指标/日志分类；所有原因对外必须是同一个错误
// （modules-01 §3.14 反 oracle 约束）。
const (
	PasswordReasonEncryptionRequired = "encryption_required"
	PasswordReasonEncryptionDisabled = "encryption_disabled"
	PasswordReasonMissingEncrypted   = "missing_encrypted_password"
	PasswordReasonMissingPassword    = "missing_password"
	PasswordReasonMissingKeyID       = "missing_key_id"
	PasswordReasonUnknownKeyID       = "unknown_key_id"
	PasswordReasonDecryptFailed      = "decrypt_failed"
)

var (
	// ErrPasswordPayload 是所有密码载荷失败的统一哨兵。公开响应必须与
	// 「用户名或密码不正确」完全一致。
	ErrPasswordPayload = errors.New("auth: password payload rejected")
	// ErrUnknownLoginKey 表示 kid 不在 active/previous 双槽或已过保留窗口。
	ErrUnknownLoginKey = errors.New("auth: unknown login encryption key")
)

// PasswordPayloadError 携带内部原因码，但 Error() 永远返回统一文案，
// 避免日志或误返回把原因码泄露给调用方。
type PasswordPayloadError struct {
	Reason string
}

func (e *PasswordPayloadError) Error() string { return ErrPasswordPayload.Error() }

// Unwrap 让 errors.Is(err, ErrPasswordPayload) 对所有原因成立。
func (e *PasswordPayloadError) Unwrap() error { return ErrPasswordPayload }

func newPasswordPayloadError(reason string) *PasswordPayloadError {
	return &PasswordPayloadError{Reason: reason}
}

// GeneratedLoginKey 是首次生成/轮换的产物。PrivateKeyPEM 是 PKCS#8 PEM，
// 调用方必须在落库前用 CREDENTIAL_ENCRYPTION_KEY 加密（LoginKeyStore 的
// PrivateKeyEnc 就是这个密文）。
type GeneratedLoginKey struct {
	Kid           string
	PublicKeyPEM  string
	PrivateKeyPEM string
}

// GenerateLoginKey 生成 RSA 密钥对并推导 kid。
func GenerateLoginKey(bits int) (*GeneratedLoginKey, error) {
	if bits < LoginKeyBits {
		return nil, fmt.Errorf("auth: RSA key must be at least %d bits", LoginKeyBits)
	}
	privateKey, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		return nil, fmt.Errorf("auth: generate login key: %w", err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return nil, fmt.Errorf("auth: marshal login private key: %w", err)
	}
	kid, publicPEM, err := loginKeyPublicParts(&privateKey.PublicKey)
	if err != nil {
		return nil, err
	}
	return &GeneratedLoginKey{
		Kid:          kid,
		PublicKeyPEM: publicPEM,
		PrivateKeyPEM: string(pem.EncodeToMemory(&pem.Block{
			Type:  "PRIVATE KEY",
			Bytes: privateDER,
		})),
	}, nil
}

// LoginKeyID 计算 kid = sha256(SPKI DER)[:16] 的十六进制（32 字符）。
func LoginKeyID(publicKey *rsa.PublicKey) (string, error) {
	kid, _, err := loginKeyPublicParts(publicKey)
	return kid, err
}

func loginKeyPublicParts(publicKey *rsa.PublicKey) (kid, publicPEM string, err error) {
	if publicKey == nil {
		return "", "", errors.New("auth: nil login public key")
	}
	spkiDER, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		return "", "", fmt.Errorf("auth: marshal login public key: %w", err)
	}
	digest := sha256.Sum256(spkiDER)
	kid = hex.EncodeToString(digest[:LoginKeyIDBytes])
	publicPEM = string(pem.EncodeToMemory(&pem.Block{
		Type:  "PUBLIC KEY",
		Bytes: spkiDER,
	}))
	return kid, publicPEM, nil
}

// ParseLoginPrivateKeyPEM 解析并校验 PKCS#8 RSA 私钥。
func ParseLoginPrivateKeyPEM(privateKeyPEM string) (*rsa.PrivateKey, error) {
	block, rest := pem.Decode([]byte(privateKeyPEM))
	if block == nil || block.Type != "PRIVATE KEY" || strings.TrimSpace(string(rest)) != "" {
		return nil, errors.New("auth: login private key is not valid PKCS#8 PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("auth: parse login private key: %w", err)
	}
	privateKey, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("auth: login private key is not RSA")
	}
	if privateKey.N == nil || privateKey.N.BitLen() < LoginKeyBits {
		return nil, fmt.Errorf("auth: login private key must be at least %d bits", LoginKeyBits)
	}
	if err := privateKey.Validate(); err != nil {
		return nil, fmt.Errorf("auth: validate login private key: %w", err)
	}
	privateKey.Precompute()
	return privateKey, nil
}

// ParseLoginPublicKeyPEM 解析 SPKI PEM 公钥。
func ParseLoginPublicKeyPEM(publicKeyPEM string) (*rsa.PublicKey, error) {
	block, rest := pem.Decode([]byte(publicKeyPEM))
	if block == nil || block.Type != "PUBLIC KEY" || strings.TrimSpace(string(rest)) != "" {
		return nil, errors.New("auth: login public key is not valid SPKI PEM")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("auth: parse login public key: %w", err)
	}
	publicKey, ok := parsed.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("auth: login public key is not RSA")
	}
	if publicKey.N == nil || publicKey.N.BitLen() < LoginKeyBits {
		return nil, fmt.Errorf("auth: login public key must be at least %d bits", LoginKeyBits)
	}
	return publicKey, nil
}

// EncryptPasswordV2 生成 v2 信封：
//
//	v2.<b64(wrappedKey)>.<b64(nonce)>.<b64(ciphertext)>
//
// wrappedKey = RSA-OAEP(SHA-256, label "password-v2")(一次性 AES-256 密钥)；
// ciphertext = AES-256-GCM(password, AAD "password-v2:"+kid)。
// 编码与参照侧一致使用标准 base64（含填充），便于前端共用实现。
func EncryptPasswordV2(publicKey *rsa.PublicKey, kid, password string) (string, error) {
	if publicKey == nil || kid == "" {
		return "", errors.New("auth: public key and kid are required")
	}
	if !utf8.ValidString(password) || password == "" {
		return "", errors.New("auth: invalid password plaintext")
	}
	if len(password) > maxPasswordPlaintextBytes {
		return "", errors.New("auth: password plaintext too large")
	}

	dataKey := make([]byte, passwordAESKeyBytes)
	if _, err := rand.Read(dataKey); err != nil {
		return "", fmt.Errorf("auth: generate password data key: %w", err)
	}
	defer clear(dataKey) // 尽力清零（Go 无保证，security-03 §2.8 列为已知限制）

	wrappedKey, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, publicKey,
		dataKey, []byte(passwordV2OAEPLabel))
	if err != nil {
		return "", fmt.Errorf("auth: wrap password data key: %w", err)
	}
	block, err := aes.NewCipher(dataKey)
	if err != nil {
		return "", fmt.Errorf("auth: password aes: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("auth: password gcm: %w", err)
	}
	nonce := make([]byte, passwordNonceBytes)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("auth: generate password nonce: %w", err)
	}
	ciphertext := gcm.Seal(nil, nonce, []byte(password), []byte(passwordV2AADPrefix+kid))

	return strings.Join([]string{
		passwordEnvelopeV2,
		base64.StdEncoding.EncodeToString(wrappedKey),
		base64.StdEncoding.EncodeToString(nonce),
		base64.StdEncoding.EncodeToString(ciphertext),
	}, "."), nil
}

// EncryptPasswordLegacy 生成旧版裸 RSA-OAEP(SHA-256) 密文（标准 base64）。
// 仅用于兼容与测试；受 RSA 单块容量限制，长/多字节口令必须走 v2。
func EncryptPasswordLegacy(publicKey *rsa.PublicKey, password string) (string, error) {
	if publicKey == nil || password == "" {
		return "", errors.New("auth: public key and password are required")
	}
	ciphertext, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, publicKey, []byte(password), nil)
	if err != nil {
		return "", fmt.Errorf("auth: encrypt legacy password: %w", err)
	}
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

// DecryptPasswordPayload 按格式解密单槽载荷。所有失败都返回
// *PasswordPayloadError（Reason=decrypt_failed），调用方只应使用 errors.Is。
func DecryptPasswordPayload(payload, kid string, privateKey *rsa.PrivateKey) (string, error) {
	if privateKey == nil || kid == "" || payload == "" {
		return "", newPasswordPayloadError(PasswordReasonDecryptFailed)
	}
	if len(payload) > maxEncryptedPayloadBytes {
		return "", newPasswordPayloadError(PasswordReasonDecryptFailed)
	}

	var plaintext []byte
	var err error
	if strings.HasPrefix(payload, passwordEnvelopeV2+".") {
		plaintext, err = decryptPasswordV2(payload, kid, privateKey)
	} else {
		plaintext, err = decryptPasswordLegacy(payload, privateKey)
	}
	if err != nil {
		return "", newPasswordPayloadError(PasswordReasonDecryptFailed)
	}
	if len(plaintext) == 0 || !utf8.Valid(plaintext) {
		return "", newPasswordPayloadError(PasswordReasonDecryptFailed)
	}
	return string(plaintext), nil
}

func decryptPasswordV2(payload, kid string, privateKey *rsa.PrivateKey) ([]byte, error) {
	parts := strings.Split(payload, ".")
	if len(parts) != 4 || parts[0] != passwordEnvelopeV2 {
		return nil, ErrPasswordPayload
	}
	wrappedKey, err := base64.StdEncoding.Strict().DecodeString(parts[1])
	if err != nil || len(wrappedKey) != privateKey.Size() {
		return nil, ErrPasswordPayload
	}
	nonce, err := base64.StdEncoding.Strict().DecodeString(parts[2])
	if err != nil || len(nonce) != passwordNonceBytes {
		return nil, ErrPasswordPayload
	}
	ciphertext, err := base64.StdEncoding.Strict().DecodeString(parts[3])
	if err != nil || len(ciphertext) <= passwordTagBytes ||
		len(ciphertext) > maxPasswordPlaintextBytes+passwordTagBytes {
		return nil, ErrPasswordPayload
	}
	dataKey, err := rsa.DecryptOAEP(sha256.New(), rand.Reader, privateKey,
		wrappedKey, []byte(passwordV2OAEPLabel))
	if err != nil || len(dataKey) != passwordAESKeyBytes {
		return nil, ErrPasswordPayload
	}
	defer clear(dataKey)
	block, err := aes.NewCipher(dataKey)
	if err != nil {
		return nil, ErrPasswordPayload
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, ErrPasswordPayload
	}
	return gcm.Open(nil, nonce, ciphertext, []byte(passwordV2AADPrefix+kid))
}

func decryptPasswordLegacy(payload string, privateKey *rsa.PrivateKey) ([]byte, error) {
	// 旧格式参照侧用非 Strict 解码，保留这一兼容口径。
	ciphertext, err := base64.StdEncoding.DecodeString(payload)
	if err != nil || len(ciphertext) != privateKey.Size() {
		return nil, ErrPasswordPayload
	}
	return rsa.DecryptOAEP(sha256.New(), rand.Reader, privateKey, ciphertext, nil)
}

// PasswordKeyLookup 按 kid 返回 active 或 previous 槽的私钥。
// 未知 kid 必须返回错误（内容不重要，上层统一映射）。
type PasswordKeyLookup func(ctx context.Context, kid string) (*rsa.PrivateKey, error)

// DecodePasswordPayload 是登录 handler 的唯一集成点：
//
//	password, err := auth.DecodePasswordPayload(ctx, enabled, lookup,
//	    req.Password, req.PasswordEncrypted, req.EncryptionKeyID)
//	if err != nil {
//	    writeUnauthorized(w) // 与错误口令逐字同形（反 oracle）
//	    return
//	}
//
// 防降级（I-9）：
//   - enabled=true：只接受密文；出现明文 password 一律拒绝（encryption_required），
//     即使同时带了密文也不回落到明文；
//   - enabled=false：只接受明文；出现密文一律拒绝（encryption_disabled），
//     不得静默退回明文路径。
func DecodePasswordPayload(ctx context.Context, enabled bool, lookup PasswordKeyLookup,
	plainPassword, encryptedPayload, encryptionKeyID string) (string, error) {

	hasPlain := plainPassword != ""
	hasEncrypted := encryptedPayload != ""

	switch {
	case !enabled && hasEncrypted:
		return "", newPasswordPayloadError(PasswordReasonEncryptionDisabled)
	case enabled && hasPlain:
		return "", newPasswordPayloadError(PasswordReasonEncryptionRequired)
	case enabled && !hasEncrypted:
		return "", newPasswordPayloadError(PasswordReasonMissingEncrypted)
	case !enabled && !hasPlain:
		return "", newPasswordPayloadError(PasswordReasonMissingPassword)
	case !enabled:
		return plainPassword, nil
	}

	if encryptionKeyID == "" {
		return "", newPasswordPayloadError(PasswordReasonMissingKeyID)
	}
	if lookup == nil {
		return "", newPasswordPayloadError(PasswordReasonUnknownKeyID)
	}
	privateKey, err := lookup(ctx, encryptionKeyID)
	if err != nil || privateKey == nil {
		return "", newPasswordPayloadError(PasswordReasonUnknownKeyID)
	}
	return DecryptPasswordPayload(encryptedPayload, encryptionKeyID, privateKey)
}

// ---------------------------------------------------------------------------
// 双槽密钥治理（I-7）
// ---------------------------------------------------------------------------

// StoredLoginKey 是 login_encryption_keys 一行的投影。
// PrivateKeyEnc 是 CREDENTIAL_ENCRYPTION_KEY（AES-256-GCM）封装的 PKCS#8 PEM。
type StoredLoginKey struct {
	ID            int64
	Active        bool
	PublicKeyPEM  string
	PrivateKeyEnc []byte
	Kid           string
	CreatedAt     time.Time
}

// LoginKeyStore 是密钥槽的持久化接缝。生产实现必须保证「至多一个 active」
// （DB 部分唯一索引）与 RotateLoginKey 的事务性。
type LoginKeyStore interface {
	ListLoginKeys(ctx context.Context) ([]StoredLoginKey, error)
	InsertLoginKey(ctx context.Context, key StoredLoginKey) (int64, error)
	RotateLoginKey(ctx context.Context, oldID int64, key StoredLoginKey) error
	// DeleteLoginKey 删除非 active 槽；返回是否实际删除了行。并发轮换/清理
	// 可能让目标行已不存在——那是 no-op 而不是删除，清理计数不得把它算进去。
	DeleteLoginKey(ctx context.Context, id int64) (bool, error)
}

// LoginKey 是已解密可用的密钥槽。
type LoginKey struct {
	ID           int64
	Kid          string
	PublicKeyPEM string
	PrivateKey   *rsa.PrivateKey
	CreatedAt    time.Time
}

// LoginKeyManager 负责首次生成、公钥下发、按 kid 解密与轮换。
// 可并发使用；bootstrap 在多副本间靠 DB 唯一约束收敛。
type LoginKeyManager struct {
	store     LoginKeyStore
	cipher    *SecretCipher
	bits      int
	retention time.Duration
	now       func() time.Time

	bootstrapMu sync.Mutex
}

// NewLoginKeyManager 构造管理器。store/cipher 为 nil 属装配错误，调用会报错。
func NewLoginKeyManager(store LoginKeyStore, cipher *SecretCipher) *LoginKeyManager {
	return &LoginKeyManager{
		store:     store,
		cipher:    cipher,
		bits:      LoginKeyBits,
		retention: PreviousLoginKeyRetention,
		now:       time.Now,
	}
}

// PublicKey 返回当前 active 槽的 kid 与 SPKI PEM；无槽时先生成（首次使用 bootstrap）。
func (m *LoginKeyManager) PublicKey(ctx context.Context) (kid, publicKeyPEM string, err error) {
	key, err := m.EnsureActive(ctx)
	if err != nil {
		return "", "", err
	}
	return key.Kid, key.PublicKeyPEM, nil
}

// EnsureActive 返回 active 槽；不存在时生成并持久化。并发/多副本竞争由
// 存储层唯一约束裁决，败者重读胜者的槽，保证全站同一把 active 钥匙。
func (m *LoginKeyManager) EnsureActive(ctx context.Context) (*LoginKey, error) {
	if m.store == nil || m.cipher == nil {
		return nil, errors.New("auth: login key manager is not configured")
	}
	rows, err := m.store.ListLoginKeys(ctx)
	if err != nil {
		return nil, fmt.Errorf("auth: list login keys: %w", err)
	}
	if active := findActiveLoginKey(rows); active != nil {
		return m.openStoredKey(*active)
	}

	m.bootstrapMu.Lock()
	defer m.bootstrapMu.Unlock()

	// 拿到进程内锁后重新读取：同副本的并发请求不应重复生成。
	rows, err = m.store.ListLoginKeys(ctx)
	if err != nil {
		return nil, fmt.Errorf("auth: list login keys: %w", err)
	}
	if active := findActiveLoginKey(rows); active != nil {
		return m.openStoredKey(*active)
	}

	generated, err := GenerateLoginKey(m.bits)
	if err != nil {
		return nil, err
	}
	sealed, err := m.cipher.Encrypt(generated.PrivateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("auth: seal login private key: %w", err)
	}
	row := StoredLoginKey{
		Active:        true,
		PublicKeyPEM:  generated.PublicKeyPEM,
		PrivateKeyEnc: []byte(sealed),
		Kid:           generated.Kid,
		CreatedAt:     m.now(),
	}
	id, err := m.store.InsertLoginKey(ctx, row)
	if err != nil {
		// 另一副本可能已抢到唯一 active 槽；重读它而不是把启动失败放大成故障。
		rows, listErr := m.store.ListLoginKeys(ctx)
		if listErr == nil {
			if active := findActiveLoginKey(rows); active != nil {
				return m.openStoredKey(*active)
			}
		}
		return nil, fmt.Errorf("auth: persist login key: %w", err)
	}
	row.ID = id
	return m.openStoredKey(row)
}

// Lookup 实现 PasswordKeyLookup：active 槽始终可用；previous 槽只在
// 「新 active 创建时间 + 15 分钟」窗口内可用（I-7）。
func (m *LoginKeyManager) Lookup(ctx context.Context, kid string) (*rsa.PrivateKey, error) {
	if m == nil || m.store == nil || m.cipher == nil {
		// 装配缺失必须返回错误而不是解引用 nil（调用方会 fail-closed）。
		return nil, errors.New("auth: login key manager is not configured")
	}
	if kid == "" {
		return nil, ErrUnknownLoginKey
	}
	rows, err := m.rowsWithActive(ctx)
	if err != nil {
		return nil, err
	}
	active := findActiveLoginKey(rows)
	if active == nil {
		return nil, ErrUnknownLoginKey
	}
	activeKey, err := m.openStoredKey(*active)
	if err != nil {
		return nil, err
	}
	if active.Kid == kid {
		return activeKey.PrivateKey, nil
	}

	// previous 槽的保留窗口以「新 active 的创建时间」为轮换锚点：
	// 轮换在同一事务内把旧槽降级并写入新 active，故二者时间一致。
	if m.now().After(active.CreatedAt.Add(m.retention)) {
		return nil, ErrUnknownLoginKey
	}
	for _, row := range rows {
		if row.Active || row.Kid != kid {
			continue
		}
		key, err := m.openStoredKey(row)
		if err != nil {
			// 损坏的 previous 槽等同未知 kid：不得因数据异常泄露差异。
			return nil, ErrUnknownLoginKey
		}
		return key.PrivateKey, nil
	}
	return nil, ErrUnknownLoginKey
}

// Rotate 轮换密钥：旧 active 降为 previous（至少保留 retention），新槽成为
// active。返回新 kid。存储实现必须在单事务内完成降级+插入。
func (m *LoginKeyManager) Rotate(ctx context.Context) (string, error) {
	if m.store == nil || m.cipher == nil {
		return "", errors.New("auth: login key manager is not configured")
	}
	rows, err := m.rowsWithActive(ctx)
	if err != nil {
		return "", err
	}
	active := findActiveLoginKey(rows)
	if active == nil {
		return "", errors.New("auth: no active login key to rotate")
	}
	// 已过窗口的旧槽先清理。窗口内的旧槽保留：正常运维轮换间隔远大于
	// 15 分钟，库里通常只有 active+previous 两槽；连续快速轮换会短暂保留
	// 多个 previous，它们与唯一 previous 一样按最新 active 的窗口统一失效。
	if _, err := m.pruneRows(ctx, rows, active); err != nil {
		return "", err
	}

	generated, err := GenerateLoginKey(m.bits)
	if err != nil {
		return "", err
	}
	sealed, err := m.cipher.Encrypt(generated.PrivateKeyPEM)
	if err != nil {
		return "", fmt.Errorf("auth: seal login private key: %w", err)
	}
	row := StoredLoginKey{
		Active:        true,
		PublicKeyPEM:  generated.PublicKeyPEM,
		PrivateKeyEnc: []byte(sealed),
		Kid:           generated.Kid,
		CreatedAt:     m.now(),
	}
	if err := m.store.RotateLoginKey(ctx, active.ID, row); err != nil {
		return "", fmt.Errorf("auth: rotate login key: %w", err)
	}
	return generated.Kid, nil
}

// PrunePrevious 清理已过保留窗口的 previous 槽，返回删除行数。
func (m *LoginKeyManager) PrunePrevious(ctx context.Context) (int, error) {
	if m.store == nil {
		return 0, errors.New("auth: login key manager is not configured")
	}
	rows, err := m.store.ListLoginKeys(ctx)
	if err != nil {
		return 0, fmt.Errorf("auth: list login keys: %w", err)
	}
	active := findActiveLoginKey(rows)
	if active == nil {
		return 0, nil
	}
	return m.pruneRows(ctx, rows, active)
}

func (m *LoginKeyManager) pruneRows(ctx context.Context, rows []StoredLoginKey, active *StoredLoginKey) (int, error) {
	if !m.now().After(active.CreatedAt.Add(m.retention)) {
		return 0, nil
	}
	deleted := 0
	for _, row := range rows {
		if row.Active {
			continue
		}
		removed, err := m.store.DeleteLoginKey(ctx, row.ID)
		if err != nil {
			return deleted, fmt.Errorf("auth: delete login key %d: %w", row.ID, err)
		}
		if removed {
			// 只有真正删除的行才计入返回值：并发清理已删掉的槽是 no-op，
			// 把它计为「删除了 N 行」会让调用方误判清理成效。
			deleted++
		}
	}
	return deleted, nil
}

// rowsWithActive 返回保证含 active 槽的行集合（必要时先 bootstrap）。
func (m *LoginKeyManager) rowsWithActive(ctx context.Context) ([]StoredLoginKey, error) {
	rows, err := m.store.ListLoginKeys(ctx)
	if err != nil {
		return nil, fmt.Errorf("auth: list login keys: %w", err)
	}
	if findActiveLoginKey(rows) != nil {
		return rows, nil
	}
	if _, err := m.EnsureActive(ctx); err != nil {
		return nil, err
	}
	rows, err = m.store.ListLoginKeys(ctx)
	if err != nil {
		return nil, fmt.Errorf("auth: list login keys: %w", err)
	}
	return rows, nil
}

func findActiveLoginKey(rows []StoredLoginKey) *StoredLoginKey {
	for i := range rows {
		if rows[i].Active {
			return &rows[i]
		}
	}
	return nil
}

// openStoredKey 解密私钥并做完整性校验：kid 与公钥必须由同一把私钥推导，
// 否则说明行损坏或被篡改，必须 fail-closed。
func (m *LoginKeyManager) openStoredKey(row StoredLoginKey) (*LoginKey, error) {
	if m.cipher == nil {
		return nil, errors.New("auth: login key manager is not configured")
	}
	privatePEM, err := m.cipher.Decrypt(string(row.PrivateKeyEnc))
	if err != nil {
		return nil, fmt.Errorf("auth: open login private key: %w", err)
	}
	privateKey, err := ParseLoginPrivateKeyPEM(privatePEM)
	if err != nil {
		return nil, err
	}
	computedKid, computedPEM, err := loginKeyPublicParts(&privateKey.PublicKey)
	if err != nil {
		return nil, err
	}
	if row.Kid != computedKid {
		return nil, errors.New("auth: login key kid does not match private key")
	}
	publicKey, err := ParseLoginPublicKeyPEM(row.PublicKeyPEM)
	if err != nil {
		return nil, err
	}
	if publicKey.N.Cmp(privateKey.N) != 0 || publicKey.E != privateKey.E {
		return nil, errors.New("auth: login public key does not match private key")
	}
	return &LoginKey{
		ID:           row.ID,
		Kid:          computedKid,
		PublicKeyPEM: computedPEM,
		PrivateKey:   privateKey,
		CreatedAt:    row.CreatedAt,
	}, nil
}
