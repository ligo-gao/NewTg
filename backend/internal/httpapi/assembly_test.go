package httpapi

// 路由装配（BuildGatewayRouter）的单元验收：用内存 Repo/假审计/内存 Redis
// 证明全部 M1 端点已挂载、公开/受保护边界正确，以及装配期 fail-fast。
// 真实 PG/Redis 的端到端验收见 internal/platform 的集成测试（build tag）。

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/rbac"
)

// fakeLoginKeyStore 是最小 LoginKeyStore（仅列表读取）。
type fakeLoginKeyStore struct {
	mu   sync.Mutex
	rows []auth.StoredLoginKey
}

func (s *fakeLoginKeyStore) ListLoginKeys(context.Context) ([]auth.StoredLoginKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]auth.StoredLoginKey(nil), s.rows...), nil
}

func (s *fakeLoginKeyStore) InsertLoginKey(_ context.Context, key auth.StoredLoginKey) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows = append(s.rows, key)
	return int64(len(s.rows)), nil
}

func (s *fakeLoginKeyStore) RotateLoginKey(context.Context, int64, auth.StoredLoginKey) error {
	return nil
}

func (s *fakeLoginKeyStore) DeleteLoginKey(context.Context, int64) error { return nil }

// gatewayDepsForTest 构造一套可运行的内存装配。DB/LogDB 为 nil，全部 Repo
// 缝隙由既有测试假实现提供。
func gatewayDepsForTest(t *testing.T) (GatewayDeps, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })

	repo := newPwdTestRepo()
	adminRepo := newFakeAdminRepo()
	sessions := auth.NewSessionManager(rdb, []byte(testSessionSecret))

	loginEncryption, err := NewPasswordEncryption(PasswordEncryptionConfig{
		Store:         &fakeLoginKeyStore{},
		Config:        mapConfig{},
		EncryptionKey: "assembly-test-credential-key",
		Logger:        testLogger(),
	})
	if err != nil {
		t.Fatalf("NewPasswordEncryption: %v", err)
	}

	deps := GatewayDeps{
		Redis:                   rdb,
		Sessions:                sessions,
		Config:                  mapConfig{},
		Audit:                   audit.New(&captureExec{}),
		Authz:                   rbac.NewAuthzMiddleware(),
		TokenService:            &fakeTokenService{},
		SessionSecret:           []byte(testSessionSecret),
		CredentialEncryptionKey: "assembly-test-credential-key",
		PublicBaseURL:           "http://localhost:3000",
		EmailSender:             &resetTestSender{},
		Logger:                  testLogger(),
		Repo:                    repo,
		SetupStore:              newFakeSetupStore(),
		SelfRepo:                newFakeSelfRepo(),
		AccountRepo:             newAccountTestRepo(),
		AccountAnonymizer:       &accountTestAnonymizer{},
		AccountReauth: auth.NewReauthGate(rdb, auth.ReauthMaterialFunc(
			func(context.Context, int64) (*auth.ReauthMaterial, error) {
				return nil, auth.ErrReauthRequired
			}), nil),
		TwoFARepo:       newFakeTwoFARepo(),
		AdminUserRepo:   adminRepo,
		AdminGroupRepo:  adminRepo,
		LoginEncryption: loginEncryption,
	}
	return deps, rdb
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestBuildGatewayRouterMountsM1Routes 冻结 M1 的路由表与鉴权边界。
func TestBuildGatewayRouterMountsM1Routes(t *testing.T) {
	deps, _ := gatewayDepsForTest(t)
	rt, err := BuildGatewayRouter(deps)
	if err != nil {
		t.Fatalf("BuildGatewayRouter: %v", err)
	}

	public := []struct {
		method string
		path   string
		body   string
		want   int
	}{
		// 公开只读/认证端点：无会话可依赖。
		{http.MethodGet, "/api/status", "", http.StatusOK},
		{http.MethodGet, "/api/auth/login/encryption-key", "", http.StatusOK},
		{http.MethodPost, "/api/auth/login", `{"identifier":""}`, http.StatusBadRequest},
		{http.MethodPost, "/api/setup", `{}`, http.StatusBadRequest},
		{http.MethodPost, "/api/auth/password-reset/request", `{}`, http.StatusBadRequest},
		{http.MethodPost, "/api/auth/2fa/challenge", `{}`, http.StatusBadRequest},
		// usage 走公共精确路由：handler 自己解析 Bearer。
		{http.MethodGet, "/api/usage/token", "", http.StatusUnauthorized},
	}
	for _, tc := range public {
		rr := doRawRequest(rt, tc.method, tc.path, tc.body, nil)
		if rr.Code != tc.want {
			t.Errorf("%s %s = %d, want %d (body=%q)", tc.method, tc.path, rr.Code, tc.want, rr.Body.String())
		}
	}

	protected := []struct {
		method string
		path   string
	}{
		// 令牌 CRUD
		{http.MethodGet, "/api/tokens"},
		{http.MethodPost, "/api/tokens"},
		{http.MethodGet, "/api/token/auto-groups"},
		// 会话/自助
		{http.MethodGet, "/api/sessions"},
		{http.MethodGet, "/api/me"},
		{http.MethodGet, "/api/self"},
		// 账号生命周期
		{http.MethodDelete, "/api/account"},
		{http.MethodPut, "/api/email"},
		{http.MethodGet, "/api/notification-preferences"},
		// 密码
		{http.MethodPost, "/api/password"},
		// 2FA
		{http.MethodPost, "/api/2fa/setup"},
		// 用户/分组管理
		{http.MethodGet, "/api/users"},
		{http.MethodGet, "/api/groups"},
	}
	for _, tc := range protected {
		rr := doRawRequest(rt, tc.method, tc.path, "", nil)
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("%s %s = %d, want 401 (body=%q)", tc.method, tc.path, rr.Code, rr.Body.String())
			continue
		}
		if !strings.Contains(rr.Body.String(), problemTypeUnauthorized) {
			t.Errorf("%s %s problem.type 不是 unauthorized: %q", tc.method, tc.path, rr.Body.String())
		}
	}
}

// doRawRequest 发请求（可带 Cookie/Header 注入）。
func doRawRequest(h http.Handler, method, path, body string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	} else {
		reader = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if mutate != nil {
		mutate(req)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// TestBuildGatewayRouterFailsFast 覆盖装配期必须拒绝的状态：
// 无审计、无 RBAC、无权限装载器、无 PUBLIC_BASE_URL、缺日志库。
func TestBuildGatewayRouterFailsFast(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*GatewayDeps)
		want   string
	}{
		{"缺审计", func(d *GatewayDeps) { d.Audit = nil }, "Audit"},
		{"审计未绑定日志库", func(d *GatewayDeps) { d.Audit = audit.New(nil) }, "Audit"},
		{"缺 RBAC", func(d *GatewayDeps) { d.Authz = nil }, "Authz"},
		{"缺权限装载器", func(d *GatewayDeps) {
			d.Repo = &authRepoWithoutLoader{}
			d.Permissions = nil
		}, "Permissions"},
		{"有发信器但无基址", func(d *GatewayDeps) {
			d.PublicBaseURL = ""
			d.Config = mapConfig{}
			d.EmailSender = &resetTestSender{}
		}, "PUBLIC_BASE_URL"},
		{"缺令牌服务", func(d *GatewayDeps) { d.TokenService = nil }, "TokenService"},
		{"缺日志库与匿名化器", func(d *GatewayDeps) { d.AccountAnonymizer = nil; d.LogDB = nil }, "LogDB"},
		{"缺自助 Repo", func(d *GatewayDeps) { d.SelfRepo = nil }, "SelfRepo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps, _ := gatewayDepsForTest(t)
			tc.mutate(&deps)
			_, err := BuildGatewayRouter(deps)
			if err == nil {
				t.Fatal("BuildGatewayRouter 应返回装配错误")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want contains %q", err, tc.want)
			}
		})
	}
}

// authRepoWithoutLoader 只实现 AuthRepo，不实现 rbac.PermissionLoader：
// 用来证明「Repo 不是权限来源时必须显式给 Permissions」。
type authRepoWithoutLoader struct{}

func (authRepoWithoutLoader) FindUserByIdentifier(context.Context, string) (*UserRecord, error) {
	return nil, nil
}
func (authRepoWithoutLoader) UsernameOrEmailTaken(context.Context, string, string) (bool, error) {
	return false, nil
}
func (authRepoWithoutLoader) CreateUser(context.Context, CreateUserInput) (CreatedUser, error) {
	return CreatedUser{}, nil
}

// TestGatewayBaseURLFallback 覆盖 platform-03 §3.1 #20 的生效基址回退：
// PUBLIC_BASE_URL → site.server_address → 「相关功能禁用」（不是启动失败）。
func TestGatewayBaseURLFallback(t *testing.T) {
	t.Run("无 env 且 DB 无值：启动成功但重置投递禁用", func(t *testing.T) {
		deps, _ := gatewayDepsForTest(t)
		deps.PublicBaseURL = ""
		deps.EmailSender = nil // M1 无 SMTP：投递通道本身不存在
		rt, err := BuildGatewayRouter(deps)
		if err != nil {
			t.Fatalf("无基址且无发信器时 gateway 必须能启动: %v", err)
		}

		rr := doRawRequest(rt, http.MethodPost, "/api/auth/password-reset/request",
			`{"email":"nobody@example.com"}`, nil)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("reset request = %d, want 204（统一成功态，不投递）body=%q", rr.Code, rr.Body.String())
		}
		// 改密端点仍挂载：无会话应为 401，而不是 404。
		rr = doRawRequest(rt, http.MethodPost, "/api/password", `{}`, nil)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("POST /api/password = %d, want 401（端点必须存在）", rr.Code)
		}
	})

	t.Run("env 为空时回退到 site.server_address", func(t *testing.T) {
		deps, _ := gatewayDepsForTest(t)
		deps.PublicBaseURL = ""
		sender := &resetTestSender{}
		deps.EmailSender = sender
		deps.Config = mapConfig{setupServerAddressKey: configString("https://gateway.example.com")}
		repo := deps.Repo.(*pwdTestRepo)
		repo.add(UserRecord{Username: "resetuser", Email: "reset@example.com", PasswordHash: "unused"})

		rt, err := BuildGatewayRouter(deps)
		if err != nil {
			t.Fatalf("BuildGatewayRouter: %v", err)
		}
		rr := doRawRequest(rt, http.MethodPost, "/api/auth/password-reset/request",
			`{"email":"reset@example.com"}`, nil)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("reset request = %d, want 204 (body=%q)", rr.Code, rr.Body.String())
		}

		deadline := time.Now().Add(2 * time.Second)
		for sender.count() < 1 {
			if !time.Now().Before(deadline) {
				t.Fatal("2s 内未投递重置邮件")
			}
			time.Sleep(5 * time.Millisecond)
		}
		if link := sender.last(t).ResetURL; !strings.HasPrefix(link, "https://gateway.example.com/reset-password?token=") {
			t.Fatalf("重置链接 = %q, want 使用 site.server_address", link)
		}
	})
}

// TestLoginEncryptionFieldsDecode 覆盖 F-01-25 的登录字段接线：
//   - 开关关闭：明文可用，只带密文 → 与口令错误同形的 401；
//   - 开关开启：正确密文可登录，明文被拒绝（I-9 防降级）。
func TestLoginEncryptionFieldsDecode(t *testing.T) {
	password := "correct-horse-battery"

	t.Run("关闭时明文路径不变、密文载荷被拒", func(t *testing.T) {
		deps, _ := gatewayDepsForTest(t)
		repo := deps.Repo.(*pwdTestRepo)
		hash, err := auth.HashPassword(password)
		if err != nil {
			t.Fatalf("HashPassword: %v", err)
		}
		repo.add(UserRecord{Username: "plainuser", Email: "plain@example.com", PasswordHash: hash})

		pe, err := NewPasswordEncryption(PasswordEncryptionConfig{
			Store:         &fakeLoginKeyStore{},
			Config:        mapConfig{"auth.login_password_encryption.enabled": configBool(false)},
			EncryptionKey: "assembly-test-credential-key",
		})
		if err != nil {
			t.Fatalf("NewPasswordEncryption: %v", err)
		}
		deps.LoginEncryption = pe
		rt, err := BuildGatewayRouter(deps)
		if err != nil {
			t.Fatalf("BuildGatewayRouter: %v", err)
		}

		rr := doRawRequest(rt, http.MethodPost, "/api/auth/login",
			`{"identifier":"plainuser","password":"`+password+`"}`, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("明文登录 = %d, want 200 (body=%q)", rr.Code, rr.Body.String())
		}

		rr = doRawRequest(rt, http.MethodPost, "/api/auth/login",
			`{"identifier":"plainuser","password_encrypted":"AA==","encryption_key_id":"kid"}`, nil)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("关闭时密文登录 = %d, want 401 (body=%q)", rr.Code, rr.Body.String())
		}
	})

	t.Run("开启时正确密文可登录、明文被拒", func(t *testing.T) {
		key, err := auth.GenerateLoginKey(auth.LoginKeyBits)
		if err != nil {
			t.Fatalf("GenerateLoginKey: %v", err)
		}
		cipher, err := auth.NewSecretCipher("assembly-test-credential-key")
		if err != nil {
			t.Fatalf("NewSecretCipher: %v", err)
		}
		privateEnc, err := cipher.Encrypt(key.PrivateKeyPEM)
		if err != nil {
			t.Fatalf("seal private key: %v", err)
		}
		store := &fakeLoginKeyStore{rows: []auth.StoredLoginKey{{
			ID: 1, Active: true, PublicKeyPEM: key.PublicKeyPEM,
			PrivateKeyEnc: []byte(privateEnc), Kid: key.Kid, CreatedAt: time.Now(),
		}}}
		pe, err := NewPasswordEncryption(PasswordEncryptionConfig{
			Store:         store,
			Config:        mapConfig{"auth.login_password_encryption.enabled": configBool(true)},
			EncryptionKey: "assembly-test-credential-key",
		})
		if err != nil {
			t.Fatalf("NewPasswordEncryption: %v", err)
		}

		deps, _ := gatewayDepsForTest(t)
		deps.LoginEncryption = pe
		repo := deps.Repo.(*pwdTestRepo)
		hash, err := auth.HashPassword(password)
		if err != nil {
			t.Fatalf("HashPassword: %v", err)
		}
		repo.add(UserRecord{Username: "encuser", Email: "enc@example.com", PasswordHash: hash})

		rt, err := BuildGatewayRouter(deps)
		if err != nil {
			t.Fatalf("BuildGatewayRouter: %v", err)
		}

		pub, err := auth.ParseLoginPublicKeyPEM(key.PublicKeyPEM)
		if err != nil {
			t.Fatalf("ParseLoginPublicKeyPEM: %v", err)
		}
		payload, err := auth.EncryptPasswordV2(pub, key.Kid, password)
		if err != nil {
			t.Fatalf("EncryptPasswordV2: %v", err)
		}
		body := `{"identifier":"encuser","password_encrypted":"` + payload +
			`","encryption_key_id":"` + key.Kid + `"}`
		rr := doRawRequest(rt, http.MethodPost, "/api/auth/login", body, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("密文登录 = %d, want 200 (body=%q)", rr.Code, rr.Body.String())
		}

		// I-9：开关开启时明文不得静默降级。
		rr = doRawRequest(rt, http.MethodPost, "/api/auth/login",
			`{"identifier":"encuser","password":"`+password+`"}`, nil)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("开启时明文登录 = %d, want 401 (body=%q)", rr.Code, rr.Body.String())
		}
	})
}
