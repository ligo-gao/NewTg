package httpapi

// 登录前端加密（F-01-25）的 HTTP 装配：公钥端点、密钥槽持久化与登录解密接缝。
//
// ---------------------------------------------------------------------------
// 登录端点集成（handlers_auth.go 属 Task 6；本任务不直接修改，由 routes/wiring
// owner 应用下面这组改动）：
//
//  1. loginRequest 增加两个字段：
//       PasswordEncrypted string `json:"password_encrypted"`
//       EncryptionKeyID   string `json:"encryption_key_id"`
//
//  2. authHandlers 增加字段 passwordEncryption *PasswordEncryption，并由 NewRouter
//     注入（NewPasswordEncryption 需要 cfg.Config / cfg.DB / cfg.CredentialEncryptionKey）。
//
//  3. handleLogin 在 readAuthJSON 之后、查用户/锁定预检查之前插入一行：
//       password, decErr := h.passwordEncryption.DecodeLoginPassword(ctx,
//           req.Password, req.PasswordEncrypted, req.EncryptionKeyID)
//       if decErr != nil {
//           // 反 oracle：解密失败 / kid 不匹配 / 明文被拒不得提前 return——
//           // 那样会把「解密失败」与「密码错误」在限流计次和 argon2 时序上区分开。
//           // 空口令不满足 8–128 长度策略、永不匹配，继续走正常失败路径即可。
//           password = ""
//       }
//     随后把后续所有 req.Password 用法替换为 password（含 dummy verify 与
//     verifyPassword）。这样解密失败与密码错误在响应、计次、时序上完全同形。
//     ★ 开关关闭时若 Config 为 nil，Enabled() 返回 false（默认值），行为与现状一致。
//
//  4. 路由注册（公开 mux）：
//       RegisterLoginEncryptionRoutes(rt.mux, passwordEncryption)
//     并把 Turnstile 设置交给中间件：
//       settings, err := TurnstileSettingsProvider(cfg.Config, sealer, cfg.Logger)
//       if err != nil { return nil, err } // 已开启但缺 SecretSealer = 装配错误
//       turnstile := auth.TurnstileMiddleware(auth.TurnstileMiddlewareConfig{
//           Endpoint: auth.TurnstileEndpointLogin, Settings: settings,
//           Redis: cfg.Redis, Logger: cfg.Logger,
//       })
//     中间件顺序必须是「限流（外层）→ Turnstile（内层）→ 业务 handler」。
//
// 授权依据：modules-01 §3.14 / §3.15；I-7 双槽、I-9 双向防降级、反 oracle。
// ---------------------------------------------------------------------------

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/configcenter"
)

const (
	// loginPasswordEncryptionEnabledKey 是开关键（modules-08 配置注册表）。
	loginPasswordEncryptionEnabledKey = "auth.login_password_encryption.enabled"
	turnstileEnabledKey               = "auth.turnstile.enabled"
	turnstileSiteKeyKey               = "auth.turnstile.site_key"
	turnstileSecretKeyKey             = "auth.turnstile.secret_key"

	// problemLoginEncryptionKeyUnavailable 对应 modules-01 埋点表里的同名原因。
	problemLoginEncryptionKeyUnavailable = "https://newtg.dev/problems/login_encryption_key_unavailable"
)

// ---------------------------------------------------------------------------
// 密钥槽持久化（login_encryption_keys，Task 1 迁移）
// ---------------------------------------------------------------------------

// SQLLoginKeyStore 是 auth.LoginKeyStore 的 PostgreSQL 实现。
// 「至多一个 active」由部分唯一索引 login_encryption_keys_single_active_uq 兜底；
// 轮换在单事务内完成降级 + 插入，避免出现零 active 的窗口。
type SQLLoginKeyStore struct {
	db *sql.DB
}

// NewSQLLoginKeyStore 构造密钥槽仓储。
func NewSQLLoginKeyStore(db *sql.DB) *SQLLoginKeyStore { return &SQLLoginKeyStore{db: db} }

// ListLoginKeys 返回全部密钥槽（active 在前），供双槽查找。
func (s *SQLLoginKeyStore) ListLoginKeys(ctx context.Context) ([]auth.StoredLoginKey, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, active, public_key_pem, private_key_enc, kid, created_at
		FROM login_encryption_keys
		ORDER BY active DESC, created_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("httpapi: list login keys: %w", err)
	}
	// 只读查询；关闭失败不影响已读取的结果。
	defer func() { _ = rows.Close() }()

	var out []auth.StoredLoginKey
	for rows.Next() {
		var key auth.StoredLoginKey
		if err := rows.Scan(&key.ID, &key.Active, &key.PublicKeyPEM,
			&key.PrivateKeyEnc, &key.Kid, &key.CreatedAt); err != nil {
			return nil, fmt.Errorf("httpapi: scan login key: %w", err)
		}
		out = append(out, key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("httpapi: list login keys rows: %w", err)
	}
	return out, nil
}

// InsertLoginKey 插入新槽。created_at 显式写入管理器的时钟值，使多副本
// 对轮换窗口的判定基于同一时间来源。
func (s *SQLLoginKeyStore) InsertLoginKey(ctx context.Context, key auth.StoredLoginKey) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO login_encryption_keys
			(active, public_key_pem, private_key_enc, kid, created_at)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id`,
		key.Active, key.PublicKeyPEM, key.PrivateKeyEnc, key.Kid, key.CreatedAt).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("httpapi: insert login key: %w", err)
	}
	return id, nil
}

// RotateLoginKey 在单事务内把 oldID 降为 previous 并插入新 active。
// 若 oldID 已不是 active（并发轮换），更新 0 行 → 整体回滚，由调用方报错。
func (s *SQLLoginKeyStore) RotateLoginKey(ctx context.Context, oldID int64, key auth.StoredLoginKey) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("httpapi: begin rotate login key: %w", err)
	}
	// 主错误优先：成功路径显式 Commit；失败路径 Rollback 的错误无需覆盖主错误。
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `
		UPDATE login_encryption_keys SET active = false
		WHERE id = $1 AND active`, oldID)
	if err != nil {
		return fmt.Errorf("httpapi: demote login key: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("httpapi: demote login key rows affected: %w", err)
	}
	if affected != 1 {
		return fmt.Errorf("httpapi: rotate login key: slot %d is no longer active", oldID)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO login_encryption_keys
			(active, public_key_pem, private_key_enc, kid, created_at)
		VALUES ($1, $2, $3, $4, $5)`,
		key.Active, key.PublicKeyPEM, key.PrivateKeyEnc, key.Kid, key.CreatedAt); err != nil {
		return fmt.Errorf("httpapi: insert rotated login key: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("httpapi: commit rotate login key: %w", err)
	}
	return nil
}

// DeleteLoginKey 只允许删除非 active 槽，防止误删当前密钥。返回是否真的
// 删除了行：RowsAffected=0（并发清理已删/槽仍是 active）是 no-op，清理计数
// 不得把它算成删除。
func (s *SQLLoginKeyStore) DeleteLoginKey(ctx context.Context, id int64) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM login_encryption_keys WHERE id = $1 AND NOT active`, id)
	if err != nil {
		return false, fmt.Errorf("httpapi: delete login key %d: %w", id, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("httpapi: delete login key %d rows affected: %w", id, err)
	}
	return affected == 1, nil
}

// ---------------------------------------------------------------------------
// 登录解密接缝
// ---------------------------------------------------------------------------

// PasswordEncryptionConfig 是解密接缝的装配输入。Store 与 DB 二选一
// （单测注入内存 Store），EncryptionKey 即 CREDENTIAL_ENCRYPTION_KEY。
type PasswordEncryptionConfig struct {
	DB            *sql.DB
	Store         auth.LoginKeyStore
	Config        ConfigReader
	EncryptionKey string
	Logger        *slog.Logger
}

// PasswordEncryption 绑定配置中心开关与双槽密钥管理器。
// 零值不可用，必须经 NewPasswordEncryption 构造。
type PasswordEncryption struct {
	manager *auth.LoginKeyManager
	config  ConfigReader
	log     *slog.Logger
}

// NewPasswordEncryption 构造解密接缝。密钥加密器缺失/非法属于装配错误：
// 私钥绝不能以明文落库，必须让进程启动失败。
func NewPasswordEncryption(cfg PasswordEncryptionConfig) (*PasswordEncryption, error) {
	store := cfg.Store
	if store == nil {
		if cfg.DB == nil {
			return nil, fmt.Errorf("httpapi: PasswordEncryptionConfig.Store or DB is required")
		}
		store = NewSQLLoginKeyStore(cfg.DB)
	}
	cipher, err := auth.NewSecretCipher(cfg.EncryptionKey)
	if err != nil {
		return nil, err
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &PasswordEncryption{
		manager: auth.NewLoginKeyManager(store, cipher),
		config:  cfg.Config,
		log:     log,
	}, nil
}

// Enabled 读取 auth.login_password_encryption.enabled（默认 false）。
func (p *PasswordEncryption) Enabled() bool {
	return readConfigBool(p.config, loginPasswordEncryptionEnabledKey, false)
}

// DecodeLoginPassword 是登录 handler 的一行集成点，语义见
// auth.DecodePasswordPayload：所有错误都必须映射为与密码错误相同的 401。
func (p *PasswordEncryption) DecodeLoginPassword(ctx context.Context,
	plainPassword, encryptedPayload, encryptionKeyID string) (string, error) {

	return auth.DecodePasswordPayload(ctx, p.Enabled(), p.manager.Lookup,
		plainPassword, encryptedPayload, encryptionKeyID)
}

// HandleKey 是 GET /api/auth/login/encryption-key。关闭时返回
// {enabled:false}；开启时返回 {kid, public_key}，首次调用生成 active 槽。
// 响应禁止任何中间层缓存（modules-01 §3.14 DisableCache）。
func (p *PasswordEncryption) HandleKey(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")

	if !p.Enabled() {
		// 关闭态必须序列化出 enabled:false（不能用 omitempty 省略成 {}）：
		// Task 15 客户端以该字段判定是否走加密登录，缺失字段会被误判。
		disabled := false
		writeJSON(w, http.StatusOK, loginEncryptionKeyResponse{Enabled: &disabled})
		return
	}
	kid, publicKey, err := p.manager.PublicKey(r.Context())
	if err != nil {
		p.log.Error("load login encryption key failed", "error", err)
		// 取公钥失败不得回落到明文：客户端会显示「登录失败，请重试」。
		w.Header().Set("Retry-After", "5")
		writeProblem(w, http.StatusServiceUnavailable, problemLoginEncryptionKeyUnavailable,
			"Login encryption key is temporarily unavailable")
		return
	}
	writeJSON(w, http.StatusOK, loginEncryptionKeyResponse{
		Kid:       kid,
		PublicKey: publicKey,
	})
}

// loginEncryptionKeyResponse 的字段按 modules-01 §3.14 冻结：
// 启用 = {kid, public_key}；关闭 = {enabled:false}。
// Enabled 用指针 + omitempty：启用时字段整体缺席，关闭时显式输出 false。
type loginEncryptionKeyResponse struct {
	Enabled   *bool  `json:"enabled,omitempty"`
	Kid       string `json:"kid,omitempty"`
	PublicKey string `json:"public_key,omitempty"`
}

// LoginEncryptionRouteRegistrar 是 RegisterLoginEncryptionRoutes 需要的最小
// mux 面（*http.ServeMux 满足）。
type LoginEncryptionRouteRegistrar interface {
	Handle(pattern string, handler http.Handler)
}

// RegisterLoginEncryptionRoutes 把公钥端点挂到公开 mux（不在 SessionAuth 链内）。
func RegisterLoginEncryptionRoutes(public LoginEncryptionRouteRegistrar, h *PasswordEncryption) {
	if public == nil || h == nil {
		return
	}
	public.Handle("GET /api/auth/login/encryption-key", http.HandlerFunc(h.HandleKey))
}

// ---------------------------------------------------------------------------
// Turnstile 配置读取
// ---------------------------------------------------------------------------

// TurnstileSettingsProvider 把 configcenter 的读取面适配为 Turnstile
// 中间件的按请求设置函数。
//
// 密钥治理（security-03 §2.4 / modules-08 F-08-07）：
//   - auth.turnstile.secret_key 已在 configcenter 的 secretKeys 清单注册，
//     Center.Save 会先 AES-256-GCM 密封再落库；
//   - 若启动时配置为「已开启」却没有 SecretSealer，构造直接失败（返回 error），
//     不允许带着无法解封的密钥启动；
//   - 运行期才打开开关且 sealer 仍为 nil 时，按「解封失败」处理并返回空
//     secret，由中间件 fail-closed 拒绝，绝不把密文当明文 secret 使用；
//   - 解封失败日志按状态变化限频（进入失败态记一次，恢复后重置），避免
//     每个请求刷一条 Error。
func TurnstileSettingsProvider(config ConfigReader, sealer configcenter.SecretSealer,
	log *slog.Logger) (func() auth.TurnstileSettings, error) {

	if log == nil {
		log = slog.Default()
	}
	if sealer == nil && readConfigBool(config, turnstileEnabledKey, false) {
		return nil, errors.New("httpapi: auth.turnstile.enabled requires a SecretSealer " +
			"(auth.turnstile.secret_key must be sealed)")
	}

	var (
		logMu   sync.Mutex
		failing bool
	)
	reportFailure := func(reason string, err error) {
		logMu.Lock()
		defer logMu.Unlock()
		if failing {
			return
		}
		failing = true
		log.Error("turnstile secret_key unavailable", "reason", reason, "error", err)
	}
	reportOK := func() {
		logMu.Lock()
		failing = false
		logMu.Unlock()
	}

	return func() auth.TurnstileSettings {
		settings := auth.TurnstileSettings{
			Enabled: readConfigBool(config, turnstileEnabledKey, false),
			SiteKey: readConfigString(config, turnstileSiteKeyKey),
		}
		secret := readConfigString(config, turnstileSecretKeyKey)
		if secret == "" {
			reportOK()
			return settings
		}
		if sealer == nil {
			if settings.Enabled {
				reportFailure("secret_sealer_missing",
					errors.New("auth.turnstile.secret_key is sealed but no SecretSealer is configured"))
			}
			return settings
		}
		plain, err := sealer.Open(secret)
		if err != nil {
			if settings.Enabled {
				reportFailure("secret_unseal_failed", err)
			}
			return settings
		}
		reportOK()
		settings.SecretKey = plain
		return settings
	}, nil
}

// ---------------------------------------------------------------------------
// 配置读取辅助
// ---------------------------------------------------------------------------

// readConfigBool 与 authHandlers.boolConfig 同语义，但作为包级函数供
// 尚未持有 authHandlers 的装配代码使用。
func readConfigBool(config ConfigReader, key string, def bool) bool {
	if config == nil {
		return def
	}
	raw, ok := config.Get(key)
	if !ok {
		return def
	}
	var v bool
	if err := json.Unmarshal(raw, &v); err != nil {
		return def
	}
	return v
}

func readConfigString(config ConfigReader, key string) string {
	if config == nil {
		return ""
	}
	raw, ok := config.Get(key)
	if !ok {
		return ""
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	return strings.TrimSpace(v)
}
