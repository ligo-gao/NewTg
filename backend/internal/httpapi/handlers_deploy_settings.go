// 模型部署控制面的设置端点（modules-08 §3.20 / F-08-51）。
//
//	GET  /api/deployments/settings                     settings:manage
//	PUT  /api/deployments/settings                     settings:manage
//	POST /api/deployments/settings/test-connection     settings:manage
//
// 这是 PUT /api/settings?section=deploy 的**作用域化投影**：写路径复用同一
// settings.Service（同一注册表、同一 AES-256-GCM Sealer、同一广播、同一
// updated_at 乐观锁），绝不复用通用 settings 的明文/掩码视图来拼响应。
//
// 秘密面（§3.20.5）：
//   - 响应只有 has_secret 布尔位，永远没有 api_key 字段；
//   - 请求里留空 / "******" = 不更新；clear_api_key=true 才删除；
//   - 连接测试的候选密钥只在本次请求内存中使用，不写日志、不进审计、不回显。
package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/configcenter"
	"newtg/gateway/internal/deploy/ionet"
	"newtg/gateway/internal/rbac"
	"newtg/gateway/internal/settings"

	"github.com/redis/go-redis/v9"
)

const (
	// 两个设置键是冻结名（modules-08 §3.20.10），不得改成第二套别名。
	deployEnabledKey = "deploy.ionet.enabled"
	deployAPIKeyKey  = "deploy.ionet.api_key"

	// 连接测试按管理员限速：5 次/分钟（§3.20.7 建议值，brief 冻结）。
	// 固定窗口复用 rateLimit:v2 的 Lua 脚本（原子 INCR+PEXPIRE）。
	deployTestRateLimit  = 5
	deployTestRateWindow = time.Minute

	// 连接测试的探测超时与服务端出站超时一致（§3.20.7：30s）。
	deployTestTimeout = ionet.DefaultTimeout

	deployProviderName = "io.net"
)

// deployProblemType* 是 §3.20.2 错误码的 RFC 7807 type。
const (
	problemTypeDeployInvalidRequest  = "https://newtg.dev/problems/deploy_invalid_request"
	problemTypeDeploySecretMissing   = "https://newtg.dev/problems/deploy_secret_missing"
	problemTypeDeploySecretInvalid   = "https://newtg.dev/problems/deploy_secret_invalid"
	problemTypeDeployProviderError   = "https://newtg.dev/problems/deploy_provider_error"
	problemTypeDeployProviderTimeout = "https://newtg.dev/problems/deploy_provider_timeout"
	problemTypeDeployRateLimited     = "https://newtg.dev/problems/deploy_rate_limited"
)

var (
	deploySettingsReadAction   = rbac.AuthzAction{ResourceKind: "deploy_settings", ActionName: "read"}
	deploySettingsUpdateAction = rbac.AuthzAction{ResourceKind: "deploy_settings", ActionName: "update"}
	deploySettingsTestAction   = rbac.AuthzAction{ResourceKind: "deploy_settings", ActionName: "test"}
)

// DeploySecretStore 读取已存密钥的明文（仅供连接测试在内存中使用）。
// 单测可注入 fake，生产实现为 sqlDeploySecretStore。
type DeploySecretStore interface {
	StoredAPIKey(ctx context.Context) (string, bool, error)
}

// DeploySettingsHandlerConfig 是端点装配输入。
type DeploySettingsHandlerConfig struct {
	// Settings 必填：生产为 *settings.Service。
	Settings SettingsService
	// Secrets 是已存密钥读取面；nil 时由 DB+Sealer 构造。
	Secrets DeploySecretStore
	DB      *sql.DB
	Sealer  configcenter.SecretSealer
	// Provider 按生效密钥构造 provider 客户端；nil 时使用 ionet.NewClient。
	// 它是测试接缝，生产不注入（host 固定、管理员不可配置）。
	Provider func(apiKey string) ionet.DeploymentProvider
	Authz    *rbac.AuthzMiddleware
	Audit    *audit.Logger
	Redis    *redis.Client
	Logger   *slog.Logger
	// Now 是延迟测量/审计的时钟接缝；nil 时 time.Now。
	Now func() time.Time
}

// DeploySettingsHandlers 持有部署设置端点依赖，可并发使用。
type DeploySettingsHandlers struct {
	service  SettingsService
	secrets  DeploySecretStore
	provider func(apiKey string) ionet.DeploymentProvider
	authz    *rbac.AuthzMiddleware
	audit    *audit.Logger
	rdb      *redis.Client
	log      *slog.Logger
	now      func() time.Time
}

// NewDeploySettingsHandlers 构造端点。
func NewDeploySettingsHandlers(cfg DeploySettingsHandlerConfig) (*DeploySettingsHandlers, error) {
	if cfg.Settings == nil {
		return nil, errors.New("httpapi: DeploySettingsHandlerConfig.Settings is required")
	}
	authz := cfg.Authz
	if authz == nil {
		authz = rbac.NewAuthzMiddleware()
	}
	// 写侧字段白名单：未登记字段由 RBAC 层 403（security-02 §2.6.1）。
	authz.SetWritePolicy(deploySettingsUpdateAction, &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{
		"enabled":       nil,
		"api_key":       nil,
		"clear_api_key": nil,
		"updated_at":    nil,
	}})
	authz.SetWritePolicy(deploySettingsTestAction, &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{
		"api_key": nil,
	}})

	secrets := cfg.Secrets
	if secrets == nil {
		if cfg.DB != nil && cfg.Sealer != nil {
			secrets = &sqlDeploySecretStore{db: cfg.DB, sealer: cfg.Sealer}
		} else {
			// 没有读取面时连接测试只能使用候选密钥；缺省候选会得到
			// deploy_secret_missing 而不是 500（装配未接入日志库之类）。
			secrets = missingSecretStore{}
		}
	}
	provider := cfg.Provider
	if provider == nil {
		provider = func(apiKey string) ionet.DeploymentProvider {
			return ionet.NewClient(ionet.Config{APIKey: apiKey, Logger: cfg.Logger})
		}
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &DeploySettingsHandlers{
		service: cfg.Settings, secrets: secrets, provider: provider,
		authz: authz, audit: cfg.Audit, rdb: cfg.Redis, log: log, now: now,
	}, nil
}

// Register 把三个端点挂到受保护子树。路径是静态的，不参与 /api/deployments/{id}
// 的参数路由（T4 的静态子路径必须先于参数路由注册）。
func (h *DeploySettingsHandlers) Register(mux *http.ServeMux) {
	mux.Handle("GET /api/deployments/settings",
		h.authz.Handler(rbac.PermSettingsManage, deploySettingsReadAction)(http.HandlerFunc(h.handleGet)))
	mux.Handle("PUT /api/deployments/settings",
		h.authz.Handler(rbac.PermSettingsManage, deploySettingsUpdateAction)(http.HandlerFunc(h.handlePut)))
	mux.Handle("POST /api/deployments/settings/test-connection",
		h.authz.Handler(rbac.PermSettingsManage, deploySettingsTestAction)(http.HandlerFunc(h.handleTestConnection)))
}

// deploySettingsView 是 GET/PUT 的唯一响应投影。
//
// ★ 刻意不是 settings.SectionView：通用视图包含 secret 元数据与掩码值，
// 直接透出既会泄漏注册表结构，也容易在后续改动中把 value 带出去。
type deploySettingsView struct {
	Enabled    bool       `json:"enabled"`
	HasSecret  bool       `json:"has_secret"`
	CanConnect bool       `json:"can_connect"`
	UpdatedAt  *time.Time `json:"updated_at"`
}

// projectDeploySettings 从 section 视图派生作用域化投影。
func projectDeploySettings(view settings.SectionView) deploySettingsView {
	out := deploySettingsView{UpdatedAt: view.UpdatedAt}
	for _, item := range view.Items {
		switch item.Key {
		case deployEnabledKey:
			// 未落库时用注册表默认值；Value 是已存值。bool 断言的失败回退
			// default，避免脏数据让投影 panic 或伪造 true。
			out.Enabled = defaultBool(item)
		case deployAPIKeyKey:
			// 密钥行存在 ⇔ 视图里 value 非 nil（统一掩码值）。这是
			// has_secret 的唯一判据，绝不读明文。
			out.HasSecret = item.Value != nil
		}
	}
	return out
}

func defaultBool(item settings.Item) bool {
	if value, ok := item.Value.(bool); ok {
		return value
	}
	return item.Default == true
}

// refresh 重读 section（GET/PUT 响应的唯一真源），并更新 can_connect。
func (h *DeploySettingsHandlers) refresh(ctx context.Context) (deploySettingsView, error) {
	view, err := h.service.Get(ctx, "deploy")
	if err != nil {
		return deploySettingsView{}, err
	}
	out := projectDeploySettings(view)
	out.CanConnect = out.Enabled && out.HasSecret
	return out, nil
}

func (h *DeploySettingsHandlers) handleGet(w http.ResponseWriter, r *http.Request) {
	out, err := h.refresh(r.Context())
	if err != nil {
		h.writeSettingsError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

type deploySettingsUpdateRequest struct {
	// Enabled 必须显式提交：设置 Tab 保存的是整段快照，缺字段会被误读成
	// false（等于静默关闭能力），因此用指针区分「没传」与「传了 false」。
	Enabled     *bool      `json:"enabled"`
	APIKey      string     `json:"api_key"`
	ClearAPIKey bool       `json:"clear_api_key"`
	UpdatedAt   *time.Time `json:"updated_at"`
}

func (h *DeploySettingsHandlers) handlePut(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxAuthBodyBytes))
	if err != nil {
		writeProblemDetail(w, http.StatusBadRequest, problemTypeDeployInvalidRequest,
			"Invalid request", "unable to read request body")
		return
	}
	var req deploySettingsUpdateRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeProblemDetail(w, http.StatusBadRequest, problemTypeDeployInvalidRequest,
			"Invalid request", "invalid JSON body")
		return
	}
	if req.Enabled == nil {
		writeProblemDetail(w, http.StatusBadRequest, problemTypeDeployInvalidRequest,
			"Invalid request", "enabled is required")
		return
	}
	keyProvided := req.APIKey != "" && req.APIKey != settings.MaskedValue
	if keyProvided && req.ClearAPIKey {
		// 同时给出新值又要求清除是歧义请求：不猜测管理员意图。
		writeProblemDetail(w, http.StatusBadRequest, problemTypeDeployInvalidRequest,
			"Invalid request", "api_key and clear_api_key are mutually exclusive")
		return
	}

	values := map[string]json.RawMessage{
		deployEnabledKey: json.RawMessage(strconv.FormatBool(*req.Enabled)),
	}
	if req.ClearAPIKey {
		// null = CLEAR（删除行 → 回到「未配置」），与通用 settings 同一语义。
		values[deployAPIKeyKey] = json.RawMessage("null")
	} else if keyProvided {
		rawKey, err := json.Marshal(req.APIKey)
		if err != nil {
			writeProblemDetail(w, http.StatusInternalServerError, authProblemInternal,
				"Internal Server Error", "")
			return
		}
		values[deployAPIKeyKey] = rawKey
	}
	// 留空 / 掩码：不放入 values，service 层因此不会触碰已存密钥。

	// 作用域化 PUT 是前端 DangerConfirm 之后的「确认提交」路径（§3.20.8：
	// 关闭开关、清除密钥都在 UI 二次确认）。危险确认仍由通用 PUT 强制执行——
	// 直接 PUT /api/settings?section=deploy 不传 dangerous_confirmed 会被注册表
	// 拒绝，所以本端点不构成绕过。
	before, _ := h.refresh(r.Context())
	actor := actorID(r)
	result, err := h.service.Put(r.Context(), settings.UpdateInput{
		Section:            "deploy",
		Values:             values,
		ExpectUpdatedAt:    req.UpdatedAt,
		DangerousConfirmed: true,
		Actor:              actor,
	})
	if err != nil {
		h.writeSettingsError(w, err)
		return
	}
	after, err := h.refresh(r.Context())
	if err != nil {
		h.writeSettingsError(w, err)
		return
	}
	h.writeSettingsAudit(r, actor, before, after, result.Changed)
	writeJSON(w, http.StatusOK, after)
}

// handleTestConnection 执行只读探测。
func (h *DeploySettingsHandlers) handleTestConnection(w http.ResponseWriter, r *http.Request) {
	actor := actorID(r)
	if limited, retry := h.allowTestConnection(r.Context(), actor); limited {
		w.Header().Set("Retry-After", retrySeconds(retry))
		writeProblemDetail(w, http.StatusTooManyRequests, problemTypeDeployRateLimited,
			"Too many requests", "connection test is limited to 5 requests per minute")
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxAuthBodyBytes))
	if err != nil {
		writeProblemDetail(w, http.StatusBadRequest, problemTypeDeployInvalidRequest,
			"Invalid request", "unable to read request body")
		return
	}
	var req struct {
		APIKey string `json:"api_key"`
	}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeProblemDetail(w, http.StatusBadRequest, problemTypeDeployInvalidRequest,
				"Invalid request", "invalid JSON body")
			return
		}
	}
	candidate := req.APIKey
	if candidate == settings.MaskedValue {
		candidate = "" // 掩码 = 前端回显，不当作候选密钥
	}

	apiKey := candidate
	if apiKey == "" {
		stored, ok, err := h.secrets.StoredAPIKey(r.Context())
		if err != nil {
			// 解封失败通常意味着主密钥轮换/数据损坏；不能把错误细节（可能
			// 含密文）回显，按内部错误处理并记日志。
			h.log.Error("deploy: decrypt stored api key failed", "err", err)
			writeProblemDetail(w, http.StatusInternalServerError, authProblemInternal,
				"Internal Server Error", "")
			return
		}
		if !ok {
			h.writeConnectionAudit(r, actor, false, http.StatusUnprocessableEntity, 0, 0, 0, "deploy_secret_missing")
			writeProblemDetail(w, http.StatusUnprocessableEntity, problemTypeDeploySecretMissing,
				"Deployment secret is not configured",
				"configure deploy.ionet.api_key or provide a candidate api_key")
			return
		}
		apiKey = stored
	}

	ctx, cancel := context.WithTimeout(r.Context(), deployTestTimeout)
	defer cancel()
	started := h.now()
	info, callErr := h.provider(apiKey).TestConnection(ctx)
	latency := h.now().Sub(started)

	if callErr != nil {
		code, problemType, detail := mapDeployProviderError(callErr)
		h.log.Warn("deploy: connection test failed", "err", callErr, "code", code)
		status := codeToHTTPStatus(code)
		// provider 的 429 建议等待时间原样带回给管理端（§3.20.7）。
		var apiErr *ionet.APIError
		if errors.As(callErr, &apiErr) && apiErr.RetryAfter > 0 {
			w.Header().Set("Retry-After", retrySeconds(apiErr.RetryAfter))
		}
		h.writeConnectionAudit(r, actor, false, status, 0, 0, latency.Milliseconds(), code)
		writeProblemDetail(w, status, problemType, deployErrorTitle(code), detail)
		return
	}

	h.writeConnectionAudit(r, actor, true, http.StatusOK, info.HardwareCount, info.TotalAvailable, latency.Milliseconds(), "")
	writeJSON(w, http.StatusOK, struct {
		OK             bool  `json:"ok"`
		HardwareCount  int   `json:"hardware_count"`
		TotalAvailable int   `json:"total_available"`
		LatencyMS      int64 `json:"latency_ms"`
	}{OK: true, HardwareCount: info.HardwareCount, TotalAvailable: info.TotalAvailable, LatencyMS: latency.Milliseconds()})
}

// allowTestConnection 消耗管理员维度的固定窗口名额。
//
// 维度是管理员 ID 而不是 IP：设置页是已认证管理面，同一出口 NAT 后的多个
// 管理员不应共享桶。Redis 不可用时 fail-open——这是只读探测，不是认证边界。
func (h *DeploySettingsHandlers) allowTestConnection(ctx context.Context, actor int64) (bool, time.Duration) {
	if h.rdb == nil {
		return false, 0
	}
	key := RateLimitKeyPrefix + "DT:admin:" + strconv.FormatInt(actor, 10)
	limited, retry, err := allowRequestRate(ctx, h.rdb, key, deployTestRateLimit, deployTestRateWindow)
	if err != nil {
		h.log.Warn("deploy: rate limiter unavailable; allowing connection test", "err", err)
		return false, 0
	}
	return limited, retry
}

// mapDeployProviderError 把 provider 错误映射为 §3.20.2 的业务码。
//
// 401/403 必须映射为 422 deploy_secret_invalid 而不是静默 502：管理员需要
// 知道「密钥不对」而不是「上游挂了」。
func mapDeployProviderError(err error) (code, problemType, detail string) {
	if ionet.IsTimeout(err) {
		return "deploy_provider_timeout", problemTypeDeployProviderTimeout,
			"provider did not respond in time"
	}
	var apiErr *ionet.APIError
	if errors.As(err, &apiErr) {
		switch {
		case apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden:
			return "deploy_secret_invalid", problemTypeDeploySecretInvalid, "provider rejected the API key"
		case apiErr.StatusCode == http.StatusTooManyRequests:
			return "deploy_rate_limited", problemTypeDeployRateLimited, "provider rate limit reached"
		}
		// provider 的错误正文已由 ionet 截断/脱敏，可安全回显。
		return "deploy_provider_error", problemTypeDeployProviderError, apiErr.Detail
	}
	var invalid *ionet.InvalidResponseError
	if errors.As(err, &invalid) {
		return "deploy_provider_error", problemTypeDeployProviderError, "provider returned an unreadable response"
	}
	return "deploy_provider_error", problemTypeDeployProviderError, "provider request failed"
}

func codeToHTTPStatus(code string) int {
	switch code {
	case "deploy_secret_invalid", "deploy_secret_missing":
		return http.StatusUnprocessableEntity
	case "deploy_rate_limited":
		return http.StatusTooManyRequests
	case "deploy_provider_timeout":
		return http.StatusGatewayTimeout
	default:
		return http.StatusBadGateway
	}
}

func deployErrorTitle(code string) string {
	switch code {
	case "deploy_secret_invalid":
		return "Deployment secret is invalid"
	case "deploy_rate_limited":
		return "Provider rate limit reached"
	case "deploy_provider_timeout":
		return "Provider timeout"
	default:
		return "Provider error"
	}
}

// writeSettingsError 复用通用 settings 的错误映射，保持 409/400 文案一致。
func (h *DeploySettingsHandlers) writeSettingsError(w http.ResponseWriter, err error) {
	status, title, detail := settingsErrorFields(err)
	if status == http.StatusInternalServerError {
		h.log.Error("deploy: settings operation failed", "err", err)
	}
	var verr *settings.ValidationError
	if errors.As(err, &verr) && verr.Key != "" {
		writeJSONBytes(w, status, settingsProblemWithFields(status, title, detail, verr.Key))
		return
	}
	writeJSONBytes(w, status, settingsProblem(status, title, detail))
}

// writeSettingsAudit 记录 deploy.settings_update。
//
// detail 只含 provider/enabled/has_secret/changed_fields；before/after 也只有
// 两个布尔位。changed_fields 里会出现设置键名（不是值），密钥值绝不进入。
func (h *DeploySettingsHandlers) writeSettingsAudit(r *http.Request, actor int64,
	before, after deploySettingsView, changed []string) {
	if h.audit == nil {
		return
	}
	if changed == nil {
		changed = []string{}
	}
	detail, err := json.Marshal(map[string]any{
		"provider":       deployProviderName,
		"enabled":        after.Enabled,
		"has_secret":     after.HasSecret,
		"changed_fields": changed,
	})
	if err != nil {
		return
	}
	beforeJSON, _ := json.Marshal(map[string]any{
		"enabled": before.Enabled, "has_secret": before.HasSecret,
	})
	afterJSON, _ := json.Marshal(map[string]any{
		"enabled": after.Enabled, "has_secret": after.HasSecret,
	})
	_ = writeAuditEvent(r.Context(), h.audit, audit.Event{
		Action:     audit.ActionDeploySettingsUpdate,
		Actor:      actor,
		Method:     http.MethodPut,
		Route:      "/api/deployments/settings",
		StatusCode: http.StatusOK,
		IP:         clientIP(r),
		UserAgent:  r.UserAgent(),
		Detail:     detail,
		Before:     beforeJSON,
		After:      afterJSON,
	})
}

// writeConnectionAudit 记录 deploy.connection_test。
//
// 失败时 detail.result=failed + error_code；成功时记 result/hardware_count/
// total_available/latency_ms。候选密钥与已存密钥都不出现在任何字段里。
func (h *DeploySettingsHandlers) writeConnectionAudit(r *http.Request, actor int64,
	ok bool, status int, hardwareCount, totalAvailable int, latencyMS int64, errorCode string) {
	if h.audit == nil {
		return
	}
	result := "success"
	if !ok {
		result = "failed"
	}
	detail := map[string]any{
		"provider":        deployProviderName,
		"result":          result,
		"hardware_count":  hardwareCount,
		"total_available": totalAvailable,
		"latency_ms":      latencyMS,
	}
	if errorCode != "" {
		detail["error_code"] = errorCode
	}
	raw, err := json.Marshal(detail)
	if err != nil {
		return
	}
	_ = writeAuditEvent(r.Context(), h.audit, audit.Event{
		Action:     audit.ActionDeployConnectionTest,
		Actor:      actor,
		Method:     http.MethodPost,
		Route:      "/api/deployments/settings/test-connection",
		StatusCode: status,
		IP:         clientIP(r),
		UserAgent:  r.UserAgent(),
		Detail:     raw,
	})
}

// sqlDeploySecretStore 是生产实现：按设置键读取密文并用同一个 Sealer 解封。
//
// 为什么直接查 system_settings 而不是 settings.Service：服务层刻意不提供
// 「读取密钥明文」的 API（任何调用方都能把秘密读出来会扩大泄漏面）。这里
// 只允许连接测试在请求内存中短暂持有明文，且调用点单一。
type sqlDeploySecretStore struct {
	db     *sql.DB
	sealer configcenter.SecretSealer
}

func (s *sqlDeploySecretStore) StoredAPIKey(ctx context.Context) (string, bool, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT value FROM system_settings WHERE "key" = $1`, deployAPIKeyKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	var sealed string
	if err := json.Unmarshal(raw, &sealed); err != nil {
		return "", false, err
	}
	if sealed == "" {
		return "", false, nil
	}
	plain, err := s.sealer.Open(sealed)
	if err != nil {
		return "", false, err
	}
	return plain, plain != "", nil
}

// missingSecretStore 是「未接线读取面」的占位：恒报告未配置。
type missingSecretStore struct{}

func (missingSecretStore) StoredAPIKey(context.Context) (string, bool, error) {
	return "", false, nil
}
