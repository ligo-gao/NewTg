package auth

import (
	"context"
	"crypto/rsa"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// 本文件是 Task 13 的加密侧单元验收（F-01-25）。全部在进程内完成：
// RSA 密钥由 crypto/rand 生成，不访问网络、不依赖 PG/Redis（GC-8）。

func mustGenerateLoginKey(t *testing.T) (kid string, privatePEM string) {
	t.Helper()
	gen, err := GenerateLoginKey(LoginKeyBits)
	if err != nil {
		t.Fatalf("GenerateLoginKey: %v", err)
	}
	if len(gen.Kid) != 32 {
		t.Fatalf("kid 长度 = %d，期望 32 位十六进制（sha256(SPKI)[:16]）", len(gen.Kid))
	}
	if !strings.HasPrefix(gen.PublicKeyPEM, "-----BEGIN PUBLIC KEY-----") {
		t.Fatalf("公钥不是 SPKI PEM：%q", gen.PublicKeyPEM[:min(32, len(gen.PublicKeyPEM))])
	}
	if !strings.HasPrefix(gen.PrivateKeyPEM, "-----BEGIN PRIVATE KEY-----") {
		t.Fatalf("私钥不是 PKCS#8 PEM")
	}
	return gen.Kid, gen.PrivateKeyPEM
}

// TestEncryptionV2EnvelopeRoundTrip 覆盖 TC-F-01-25-01：v2 信封往返，
// 且口令含长 Unicode（字节数超过 2048-bit RSA 单块上限，正是 v2 的存在理由）。
func TestEncryptionV2EnvelopeRoundTrip(t *testing.T) {
	kid, privatePEM := mustGenerateLoginKey(t)
	priv, err := ParseLoginPrivateKeyPEM(privatePEM)
	if err != nil {
		t.Fatalf("ParseLoginPrivateKeyPEM: %v", err)
	}
	password := "p@ss-密码-🔐-" + strings.Repeat("é", 100)
	if len(password) <= 190 {
		t.Fatalf("测试口令只有 %d 字节，无法证明 v2 绕开 RSA 单块上限", len(password))
	}

	payload, err := EncryptPasswordV2(&priv.PublicKey, kid, password)
	if err != nil {
		t.Fatalf("EncryptPasswordV2: %v", err)
	}
	parts := strings.Split(payload, ".")
	if len(parts) != 4 || parts[0] != "v2" {
		t.Fatalf("v2 载荷格式 = %q，期望 v2.<wrappedKey>.<nonce>.<ct>", payload)
	}

	got, err := DecryptPasswordPayload(payload, kid, priv)
	if err != nil {
		t.Fatalf("DecryptPasswordPayload: %v", err)
	}
	if got != password {
		t.Fatalf("解密结果不一致: got %q want %q", got, password)
	}

	// 走导出的集成辅助（enabled=true 的唯一合法形态是密文）。
	lookup := func(context.Context, string) (*rsa.PrivateKey, error) { return priv, nil }
	got, err = DecodePasswordPayload(context.Background(), true, lookup, "", payload, kid)
	if err != nil || got != password {
		t.Fatalf("DecodePasswordPayload = (%q, %v)，期望口令还原", got, err)
	}
}

// TestEncryptionLegacyOAEPRoundTrip 覆盖 TC-F-01-25-02：旧版裸 RSA-OAEP(SHA-256)
// 仍可解密，保证前端灰度期两种格式并存。
func TestEncryptionLegacyOAEPRoundTrip(t *testing.T) {
	kid, privatePEM := mustGenerateLoginKey(t)
	priv, err := ParseLoginPrivateKeyPEM(privatePEM)
	if err != nil {
		t.Fatalf("ParseLoginPrivateKeyPEM: %v", err)
	}
	const password = "Legacy-8!pass"

	payload, err := EncryptPasswordLegacy(&priv.PublicKey, password)
	if err != nil {
		t.Fatalf("EncryptPasswordLegacy: %v", err)
	}
	if strings.HasPrefix(payload, "v2.") {
		t.Fatal("旧格式不得带 v2 前缀")
	}
	got, err := DecryptPasswordPayload(payload, kid, priv)
	if err != nil || got != password {
		t.Fatalf("旧格式解密 = (%q, %v)", got, err)
	}
}

// TestEncryptionAADTamperFailsGenerically 覆盖 TC-F-01-25-03 的密码学面：
// AAD 绑定了 kid，任何 AAD/密文篡改都只能得到同一个 ErrPasswordPayload，
// 错误串不得携带可区分的内部原因。
func TestEncryptionAADTamperFailsGenerically(t *testing.T) {
	kid, privatePEM := mustGenerateLoginKey(t)
	priv, err := ParseLoginPrivateKeyPEM(privatePEM)
	if err != nil {
		t.Fatalf("ParseLoginPrivateKeyPEM: %v", err)
	}

	// 用错误的 kid 作为 AAD 加密，再按真实 kid 解密：GCM 必须拒绝。
	otherKid := strings.Repeat("ab", 16)
	payload, err := EncryptPasswordV2(&priv.PublicKey, otherKid, "CorrectHorse9!")
	if err != nil {
		t.Fatalf("EncryptPasswordV2: %v", err)
	}
	_, errAAD := DecryptPasswordPayload(payload, kid, priv)
	if !errors.Is(errAAD, ErrPasswordPayload) {
		t.Fatalf("AAD 篡改必须返回 ErrPasswordPayload，实际 %v", errAAD)
	}

	// 翻转密文最后一个字节，错误串必须与 AAD 篡改逐字相同。
	tampered := tamperLastByte(t, payload)
	_, errCT := DecryptPasswordPayload(tampered, kid, priv)
	if !errors.Is(errCT, ErrPasswordPayload) {
		t.Fatalf("密文篡改必须返回 ErrPasswordPayload，实际 %v", errCT)
	}
	if errAAD.Error() != errCT.Error() {
		t.Fatalf("AAD 篡改与密文篡改错误串不同：%q vs %q", errAAD, errCT)
	}
	if pe, ok := errAAD.(*PasswordPayloadError); !ok || pe.Reason != PasswordReasonDecryptFailed {
		t.Fatalf("内部原因码应为 %q，实际 %#v", PasswordReasonDecryptFailed, errAAD)
	}
}

// TestDecodePasswordPayloadUnknownKID 覆盖 TC-F-01-25-04：kid 不在双槽内时，
// 与解密失败共用同一个公开错误与同一个内部泛化原因。
func TestDecodePasswordPayloadUnknownKID(t *testing.T) {
	lookup := func(context.Context, string) (*rsa.PrivateKey, error) {
		return nil, ErrUnknownLoginKey
	}
	_, err := DecodePasswordPayload(context.Background(), true, lookup, "", "v2.AAAA.BBBB.CCCC", "00112233445566778899aabbccddeeff")
	if !errors.Is(err, ErrPasswordPayload) {
		t.Fatalf("kid 不匹配必须返回 ErrPasswordPayload，实际 %v", err)
	}
	if pe := err.(*PasswordPayloadError); pe.Reason != PasswordReasonUnknownKeyID {
		t.Fatalf("内部原因码 = %q，期望 %q", pe.Reason, PasswordReasonUnknownKeyID)
	}
}

// TestDecodePasswordPayloadAntiDowngrade 覆盖 TC-F-01-25-06 的双向防降级：
// 开启时拒明文（encryption_required），关闭时拒密文；两侧都不得静默回落。
func TestDecodePasswordPayloadAntiDowngrade(t *testing.T) {
	noLookup := func(context.Context, string) (*rsa.PrivateKey, error) {
		t.Fatal("防降级拒绝路径不得触发密钥查询")
		return nil, nil
	}
	cases := []struct {
		name      string
		enabled   bool
		plain     string
		encrypted string
		kid       string
		wantErr   string
		wantOK    string
	}{
		{
			name:    "开启时明文被拒",
			enabled: true, plain: "CorrectHorse9!",
			wantErr: PasswordReasonEncryptionRequired,
		},
		{
			name:    "开启时明文+密文同时出现仍拒明文",
			enabled: true, plain: "CorrectHorse9!", encrypted: "v2.a.b.c", kid: "k",
			wantErr: PasswordReasonEncryptionRequired,
		},
		{
			name:    "开启时两个字段都缺失",
			enabled: true,
			wantErr: PasswordReasonMissingEncrypted,
		},
		{
			name:    "关闭时密文被拒",
			enabled: false, encrypted: "v2.a.b.c", kid: "k",
			wantErr: PasswordReasonEncryptionDisabled,
		},
		{
			name:    "关闭时两个字段都缺失",
			enabled: false,
			wantErr: PasswordReasonMissingPassword,
		},
		{
			name:    "关闭时明文可用",
			enabled: false, plain: "CorrectHorse9!",
			wantOK: "CorrectHorse9!",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DecodePasswordPayload(context.Background(), tc.enabled, noLookup,
				tc.plain, tc.encrypted, tc.kid)
			if tc.wantErr != "" {
				if !errors.Is(err, ErrPasswordPayload) {
					t.Fatalf("期望 ErrPasswordPayload，实际 (%q, %v)", got, err)
				}
				if pe := err.(*PasswordPayloadError); pe.Reason != tc.wantErr {
					t.Fatalf("内部原因码 = %q，期望 %q", pe.Reason, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.wantOK {
				t.Fatalf("DecodePasswordPayload = (%q, %v)，期望 %q", got, err, tc.wantOK)
			}
		})
	}
}

// memLoginKeyStore 是 LoginKeyStore 的内存实现，并模拟 DB 的
// 「至多一个 active」部分唯一索引与事务性轮换，供管理器单测使用。
type memLoginKeyStore struct {
	mu     sync.Mutex
	nextID int64
	rows   []StoredLoginKey
}

func (s *memLoginKeyStore) ListLoginKeys(context.Context) ([]StoredLoginKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]StoredLoginKey, len(s.rows))
	copy(out, s.rows)
	return out, nil
}

func (s *memLoginKeyStore) InsertLoginKey(_ context.Context, key StoredLoginKey) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if key.Active {
		for _, row := range s.rows {
			if row.Active {
				return 0, errors.New("memstore: active 槽已存在")
			}
		}
	}
	s.nextID++
	key.ID = s.nextID
	s.rows = append(s.rows, key)
	return key.ID, nil
}

func (s *memLoginKeyStore) RotateLoginKey(_ context.Context, oldID int64, key StoredLoginKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	found := false
	for i := range s.rows {
		if s.rows[i].ID == oldID && s.rows[i].Active {
			s.rows[i].Active = false
			found = true
			break
		}
	}
	if !found {
		return errors.New("memstore: 当前 active 槽不存在")
	}
	for _, row := range s.rows {
		if row.Active {
			return errors.New("memstore: active 槽已存在")
		}
	}
	s.nextID++
	key.ID = s.nextID
	s.rows = append(s.rows, key)
	return nil
}

func (s *memLoginKeyStore) DeleteLoginKey(_ context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.rows {
		if s.rows[i].ID == id {
			if s.rows[i].Active {
				return errors.New("memstore: 不得删除 active 槽")
			}
			s.rows = append(s.rows[:i], s.rows[i+1:]...)
			return nil
		}
	}
	return nil
}

func (s *memLoginKeyStore) snapshot() []StoredLoginKey {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]StoredLoginKey, len(s.rows))
	copy(out, s.rows)
	return out
}

func mustTestCipher(t *testing.T) *SecretCipher {
	t.Helper()
	c, err := NewSecretCipher("task-13-unit-test-master-key")
	if err != nil {
		t.Fatalf("NewSecretCipher: %v", err)
	}
	return c
}

// TestLoginKeyManagerBootstrapRotationAndPreviousSlot 覆盖轮换双槽（I-7）：
// 首次使用生成 active；轮换后旧槽仍可解密 ≥15 分钟；超窗后旧 kid 失配。
func TestLoginKeyManagerBootstrapRotationAndPreviousSlot(t *testing.T) {
	store := &memLoginKeyStore{}
	mgr := NewLoginKeyManager(store, mustTestCipher(t))
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	mgr.now = func() time.Time { return now }

	ctx := context.Background()
	kid1, pubPEM1, err := mgr.PublicKey(ctx)
	if err != nil {
		t.Fatalf("PublicKey bootstrap: %v", err)
	}
	if got := len(store.snapshot()); got != 1 {
		t.Fatalf("首次使用应生成 1 个 active 槽，实际 %d", got)
	}
	if kid1b, _, _ := mgr.PublicKey(ctx); kid1b != kid1 {
		t.Fatalf("bootstrap 不幂等：%q != %q", kid1b, kid1)
	}

	// 用第一代公钥加密一份 v2 载荷，轮换后必须还能解。
	pub1, err := ParseLoginPublicKeyPEM(pubPEM1)
	if err != nil {
		t.Fatalf("ParseLoginPublicKeyPEM: %v", err)
	}
	payload, err := EncryptPasswordV2(pub1, kid1, "CorrectHorse9!")
	if err != nil {
		t.Fatalf("EncryptPasswordV2: %v", err)
	}

	kid2, err := mgr.Rotate(ctx)
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if kid2 == kid1 {
		t.Fatal("轮换后 kid 不得相同")
	}
	rows := store.snapshot()
	if len(rows) != 2 {
		t.Fatalf("轮换后应有 active+previous 两槽，实际 %d", len(rows))
	}

	got, err := DecodePasswordPayload(ctx, true, mgr.Lookup, "", payload, kid1)
	if err != nil || got != "CorrectHorse9!" {
		t.Fatalf("previous 槽解密失败：(%q, %v)", got, err)
	}

	// 前端公钥缓存 5 分钟；15 分钟窗口内旧槽必须持续可用。
	now = now.Add(PreviousLoginKeyRetention)
	if _, err := mgr.Lookup(ctx, kid1); err != nil {
		t.Fatalf("15 分钟窗口边界内 previous 槽不应失效：%v", err)
	}

	// 超过窗口后旧 kid 必须失配（与未知 kid 同形）。
	now = now.Add(time.Second)
	if _, err := mgr.Lookup(ctx, kid1); !errors.Is(err, ErrUnknownLoginKey) {
		t.Fatalf("超窗后 previous kid 应失配，实际 %v", err)
	}

	// 新 active 不受影响。
	if _, err := mgr.Lookup(ctx, kid2); err != nil {
		t.Fatalf("active 槽应始终可用：%v", err)
	}

	pruned, err := mgr.PrunePrevious(ctx)
	if err != nil || pruned != 1 {
		t.Fatalf("PrunePrevious = (%d, %v)，期望清理 1 个旧槽", pruned, err)
	}
	if rows := store.snapshot(); len(rows) != 1 || !rows[0].Active {
		t.Fatalf("清理后应只剩 active 槽：%#v", rows)
	}
}

// TestLoginKeyManagerConcurrentBootstrap 模拟多副本同时首次启动：
// 唯一 active 约束的败者必须重读胜者的槽，而不是各生成一把钥匙。
func TestLoginKeyManagerConcurrentBootstrap(t *testing.T) {
	store := &memLoginKeyStore{}
	cipher := mustTestCipher(t)
	ctx := context.Background()

	const replicas = 6
	kids := make([]string, replicas)
	errs := make([]error, replicas)
	var wg sync.WaitGroup
	for i := 0; i < replicas; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			mgr := NewLoginKeyManager(store, cipher)
			kids[i], _, errs[i] = mgr.PublicKey(ctx)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("副本 %d bootstrap: %v", i, err)
		}
		if kids[i] != kids[0] {
			t.Fatalf("副本 %d 得到不同 kid：%q vs %q", i, kids[i], kids[0])
		}
	}
	rows := store.snapshot()
	if len(rows) != 1 || !rows[0].Active {
		t.Fatalf("并发 bootstrap 后应恰有 1 个 active 槽：%#v", rows)
	}
}

// tamperLastByte 只改 base64 密文段的最后一个字符，保持格式合法，
// 以证明失败来自 GCM 认证而不是解析提前拒绝。
func tamperLastByte(t *testing.T, payload string) string {
	t.Helper()
	parts := strings.Split(payload, ".")
	if len(parts) != 4 {
		t.Fatalf("载荷格式异常：%q", payload)
	}
	ct := []byte(parts[3])
	if ct[len(ct)-1] == 'A' {
		ct[len(ct)-1] = 'B'
	} else {
		ct[len(ct)-1] = 'A'
	}
	parts[3] = string(ct)
	return strings.Join(parts, ".")
}
