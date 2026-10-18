package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/configcenter"
)

// 本文件是 Task 13 的 HTTP 面单元验收：公钥端点（F-01-25）与「解密失败 /
// 明文被拒 / kid 不匹配 / 密码错误」的响应不可区分性（反 oracle）。
// 全部依赖内存实现，不触碰 PG（GC-8）。

// memLoginKeyStore 是 auth.LoginKeyStore 的内存实现，模拟 DB 的
// 「至多一个 active」约束（部分唯一索引），供管理器与端点测试共用。
type memLoginKeyStore struct {
	mu     sync.Mutex
	nextID int64
	rows   []auth.StoredLoginKey
}

func (s *memLoginKeyStore) ListLoginKeys(context.Context) ([]auth.StoredLoginKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]auth.StoredLoginKey, len(s.rows))
	copy(out, s.rows)
	return out, nil
}

func (s *memLoginKeyStore) InsertLoginKey(_ context.Context, key auth.StoredLoginKey) (int64, error) {
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

func (s *memLoginKeyStore) RotateLoginKey(_ context.Context, oldID int64, key auth.StoredLoginKey) error {
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
			s.rows = append(s.rows[:i], s.rows[i+1:]...)
			return nil
		}
	}
	return nil
}

func newTestPasswordEncryption(t *testing.T, enabled bool) (*PasswordEncryption, *memLoginKeyStore) {
	t.Helper()
	cfg := mapConfig{}
	if enabled {
		cfg["auth.login_password_encryption.enabled"] = configBool(true)
	}
	store := &memLoginKeyStore{}
	pe, err := NewPasswordEncryption(PasswordEncryptionConfig{
		Store:         store,
		Config:        cfg,
		EncryptionKey: "task-13-httpapi-test-master-key",
	})
	if err != nil {
		t.Fatalf("NewPasswordEncryption: %v", err)
	}
	return pe, store
}

type encryptionKeyJSON struct {
	Enabled   bool   `json:"enabled"`
	Kid       string `json:"kid"`
	PublicKey string `json:"public_key"`
}

func getEncryptionKey(t *testing.T, pe *PasswordEncryption) (*httptest.ResponseRecorder, encryptionKeyJSON) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/auth/login/encryption-key", nil)
	rr := httptest.NewRecorder()
	pe.HandleKey(rr, req)
	var body encryptionKeyJSON
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON：%q", rr.Body.String())
	}
	return rr, body
}

// TestLoginEncryptionKeyEndpointDisabled：关闭时只返回 {enabled:false}，
// 且必须逐字节命中冻结 JSON——Task 15 客户端按 `enabled` 字段判断加密是否
// 关闭，`{}` 会被误判为「未禁用」而继续走加密登录。
func TestLoginEncryptionKeyEndpointDisabled(t *testing.T) {
	pe, store := newTestPasswordEncryption(t, false)
	req := httptest.NewRequest(http.MethodGet, "/api/auth/login/encryption-key", nil)
	rr := httptest.NewRecorder()
	pe.HandleKey(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", rr.Code)
	}
	if got, want := rr.Body.String(), "{\"enabled\":false}\n"; got != want {
		t.Fatalf("关闭响应体必须逐字节为 %q，实际 %q", want, got)
	}
	if cc := rr.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q，期望 no-store（DisableCache）", cc)
	}
	rows, _ := store.ListLoginKeys(context.Background())
	if len(rows) != 0 {
		t.Fatalf("关闭时不得生成密钥，实际 %d 行", len(rows))
	}
}

// TestLoginEncryptionKeyEndpointBootstraps：开启时首次调用生成 active 槽，
// 返回 {kid, public_key}，重复调用复用同一把钥匙。
func TestLoginEncryptionKeyEndpointBootstraps(t *testing.T) {
	pe, store := newTestPasswordEncryption(t, true)
	rr, body := getEncryptionKey(t, pe)

	if rr.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", rr.Code)
	}
	if body.Enabled {
		t.Fatal("启用态响应不应带 enabled 字段（约定为 {kid, public_key}）")
	}
	if strings.Contains(rr.Body.String(), "\"enabled\"") {
		t.Fatalf("启用态响应不得出现 enabled 字段：%s", rr.Body.String())
	}
	if len(body.Kid) != 32 {
		t.Fatalf("kid = %q，期望 32 位十六进制", body.Kid)
	}
	pub, err := auth.ParseLoginPublicKeyPEM(body.PublicKey)
	if err != nil {
		t.Fatalf("public_key 不是 SPKI PEM：%v", err)
	}
	if pub.N.BitLen() < 2048 {
		t.Fatalf("RSA 位数 = %d，要求 ≥2048", pub.N.BitLen())
	}
	if cc := rr.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q，期望 no-store", cc)
	}

	_, body2 := getEncryptionKey(t, pe)
	if body2.Kid != body.Kid || body2.PublicKey != body.PublicKey {
		t.Fatal("重复调用应复用同一 active 槽")
	}
	rows, _ := store.ListLoginKeys(context.Background())
	if len(rows) != 1 || !rows[0].Active {
		t.Fatalf("应恰有 1 个 active 槽：%#v", rows)
	}
}

// TestDecodeLoginPasswordV2AndLegacy：HTTP 装配层同时接受 v2 与旧格式。
func TestDecodeLoginPasswordV2AndLegacy(t *testing.T) {
	pe, _ := newTestPasswordEncryption(t, true)
	_, body := getEncryptionKey(t, pe)
	pub, err := auth.ParseLoginPublicKeyPEM(body.PublicKey)
	if err != nil {
		t.Fatalf("ParseLoginPublicKeyPEM: %v", err)
	}
	ctx := context.Background()

	v2, err := auth.EncryptPasswordV2(pub, body.Kid, "CorrectHorse9!")
	if err != nil {
		t.Fatalf("EncryptPasswordV2: %v", err)
	}
	if got, err := pe.DecodeLoginPassword(ctx, "", v2, body.Kid); err != nil || got != "CorrectHorse9!" {
		t.Fatalf("v2 解密 = (%q, %v)", got, err)
	}

	legacy, err := auth.EncryptPasswordLegacy(pub, "Legacy-8!pass")
	if err != nil {
		t.Fatalf("EncryptPasswordLegacy: %v", err)
	}
	if got, err := pe.DecodeLoginPassword(ctx, "", legacy, body.Kid); err != nil || got != "Legacy-8!pass" {
		t.Fatalf("旧格式解密 = (%q, %v)", got, err)
	}
}

// TestLoginPayloadFailuresAreIndistinguishable 是本任务最关键的反 oracle
// 验收：解密失败、AAD 篡改、kid 不匹配、开启时明文被拒、关闭时密文被拒，
// 以及真正的密码错误，必须产生逐字节相同的 401 响应。
func TestLoginPayloadFailuresAreIndistinguishable(t *testing.T) {
	enabled, store := newTestPasswordEncryption(t, true)
	_, body := getEncryptionKey(t, enabled)
	pub, err := auth.ParseLoginPublicKeyPEM(body.PublicKey)
	if err != nil {
		t.Fatalf("ParseLoginPublicKeyPEM: %v", err)
	}
	disabled, _ := newTestPasswordEncryption(t, false)

	validPayload, err := auth.EncryptPasswordV2(pub, body.Kid, "CorrectHorse9!")
	if err != nil {
		t.Fatalf("EncryptPasswordV2: %v", err)
	}
	wrongPasswordPayload, err := auth.EncryptPasswordV2(pub, body.Kid, "WrongHorse9!")
	if err != nil {
		t.Fatalf("EncryptPasswordV2: %v", err)
	}
	// AAD 绑定 kid：用别的 kid 加密再按真实 kid 解密必定认证失败。
	otherKid := strings.Repeat("0", 32)
	aadTampered, err := auth.EncryptPasswordV2(pub, otherKid, "CorrectHorse9!")
	if err != nil {
		t.Fatalf("EncryptPasswordV2: %v", err)
	}

	hash := authTestPasswordHash()

	// probe 复刻 wiring 文档给出的集成片段：解密/防降级失败时把口令置空
	// （空口令永不匹配），继续走与密码错误完全相同的校验与 401 出口。
	probe := func(plain, encrypted, kid string, pe *PasswordEncryption) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		password, err := pe.DecodeLoginPassword(context.Background(), plain, encrypted, kid)
		if err != nil {
			password = ""
		}
		if !auth.VerifyPassword(hash, password) {
			writeUnauthorized(rr)
			return rr
		}
		rr.WriteHeader(http.StatusOK)
		return rr
	}

	cases := []struct {
		name      string
		plain     string
		encrypted string
		kid       string
		pe        *PasswordEncryption
	}{
		{name: "真实密码错误", encrypted: wrongPasswordPayload, kid: body.Kid, pe: enabled},
		{name: "AAD 篡改", encrypted: aadTampered, kid: body.Kid, pe: enabled},
		{name: "kid 不匹配", encrypted: validPayload, kid: otherKid, pe: enabled},
		{name: "开启时提交明文", plain: "CorrectHorse9!", pe: enabled},
		{name: "关闭时提交密文", encrypted: validPayload, kid: body.Kid, pe: disabled},
		{name: "密文缺 kid", encrypted: validPayload, pe: enabled},
	}

	baseline := probe("", cases[0].encrypted, cases[0].kid, cases[0].pe)
	if baseline.Code != http.StatusUnauthorized {
		t.Fatalf("基线状态码 = %d，期望 401", baseline.Code)
	}
	baselineBody := baseline.Body.String()
	baselineType := baseline.Header().Get("Content-Type")

	for _, tc := range cases[1:] {
		rr := probe(tc.plain, tc.encrypted, tc.kid, tc.pe)
		if rr.Code != baseline.Code {
			t.Fatalf("%s 状态码 = %d，期望与密码错误同为 %d", tc.name, rr.Code, baseline.Code)
		}
		if rr.Body.String() != baselineBody {
			t.Fatalf("%s 响应体与密码错误不同：%q vs %q", tc.name, rr.Body.String(), baselineBody)
		}
		if ct := rr.Header().Get("Content-Type"); ct != baselineType {
			t.Fatalf("%s Content-Type = %q，期望 %q", tc.name, ct, baselineType)
		}
	}

	// 对照组：同一条路径下正确口令必须成功，证明上面的 401 不是因为
	// 装配整体坏了（避免「全部不可区分地失败」的假阳性）。
	if rr := probe("", validPayload, body.Kid, enabled); rr.Code != http.StatusOK {
		t.Fatalf("正确口令状态码 = %d，期望 200", rr.Code)
	}
	if rows, _ := store.ListLoginKeys(context.Background()); len(rows) != 1 {
		t.Fatalf("端点 bootstrap 后应恰有 1 个槽，实际 %d", len(rows))
	}
}

// TestTurnstileSettingsProvider 验证 configcenter 读取面 + SecretSealer：
// secret 必须经 Open 解封；解封失败时返回空串（fail-closed），绝不把
// 密文当明文 secret 使用；已开启但未装配 Sealer 属启动期装配错误。
func TestTurnstileSettingsProvider(t *testing.T) {
	cfg := mapConfig{
		"auth.turnstile.enabled":    configBool(true),
		"auth.turnstile.site_key":   configString("site-key"),
		"auth.turnstile.secret_key": configString("sealed:secret-key"),
	}
	sealer := fakeSealer{prefix: "sealed:"}
	read, err := TurnstileSettingsProvider(cfg, sealer, nil)
	if err != nil {
		t.Fatalf("TurnstileSettingsProvider: %v", err)
	}
	got := read()
	if !got.Enabled || got.SiteKey != "site-key" || got.SecretKey != "secret-key" {
		t.Fatalf("设置读取 = %#v", got)
	}

	// 解封失败：返回空 secret（中间件会判为配置错误并拒绝）。
	broken, err := TurnstileSettingsProvider(cfg, fakeSealer{prefix: "other:"}, nil)
	if err != nil {
		t.Fatalf("TurnstileSettingsProvider(broken sealer): %v", err)
	}
	if got := broken(); got.SecretKey != "" {
		t.Fatalf("解封失败必须返回空 secret（fail-closed），实际 %q", got.SecretKey)
	}

	// 已开启但 sealer 为 nil：启动失败，绝不带病运行。
	if _, err := TurnstileSettingsProvider(cfg, nil, nil); err == nil {
		t.Fatal("已开启 Turnstile 且 sealer 为 nil 时必须返回装配错误")
	}

	// 未开启时允许没有 sealer；后续运行期开启会由设置函数 fail-closed。
	offCfg := mapConfig{
		"auth.turnstile.enabled":    configBool(false),
		"auth.turnstile.secret_key": configString("sealed:secret-key"),
	}
	offRead, err := TurnstileSettingsProvider(offCfg, nil, nil)
	if err != nil {
		t.Fatalf("未开启时不应要求 Sealer：%v", err)
	}
	if off := offRead(); off.Enabled || off.SecretKey != "" {
		t.Fatalf("未开启且无 Sealer 时不得解出 secret：%#v", off)
	}
}

// fakeSealer 是 configcenter.SecretSealer 的测试替身。
type fakeSealer struct{ prefix string }

func (s fakeSealer) Seal(plain string) (string, error) { return s.prefix + plain, nil }

func (s fakeSealer) Open(sealed string) (string, error) {
	if !strings.HasPrefix(sealed, s.prefix) {
		return "", errors.New("fakeSealer: 前缀不匹配")
	}
	return strings.TrimPrefix(sealed, s.prefix), nil
}

var _ configcenter.SecretSealer = fakeSealer{}
