// 系统设置读写端点（modules-08 §1.3 / §1.4）。
//
//	GET /api/settings?section=<section>   admin（只读）
//	PUT /api/settings?section=<section>   settings:manage（写）
//
// 写路径：section 整体校验 → 单事务写 system_settings → configcenter 广播 →
// audit_logs（before/after 脱敏）；支持 Idempotency-Key 与 expect_updated_at
// 乐观锁（409）。密钥类值只以 "******" 出现在响应与审计中。
package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/notify"
	"newtg/gateway/internal/rbac"
	"newtg/gateway/internal/settings"
)

// SettingsService 是设置服务的测试接缝（生产为 *settings.Service）。
type SettingsService interface {
	Get(ctx context.Context, section string) (settings.SectionView, error)
	Put(ctx context.Context, in settings.UpdateInput) (settings.UpdateResult, error)
}

// SettingsHandlerConfig 是设置端点的装配输入。
type SettingsHandlerConfig struct {
	Service SettingsService
	Authz   *rbac.AuthzMiddleware
	Audit   *audit.Logger
	// Idem 是幂等存储；nil 时退化为进程内实现（单副本）。
	Idem   IdempotencyStore
	Logger *slog.Logger
	// Webhook 是 POST /api/settings/test/webhook 的发送器接缝
	// （生产 *notify.WebhookSender）。nil 时端点返回 503。
	Webhook WebhookTester
	// Redis 是测试 Webhook 的管理员限流桶；nil 时 fail-open（只读探测口径）。
	Redis *redis.Client
}

// WebhookTester 是测试发送的最小能力集（生产为 *notify.WebhookSender）。
//
// Configured 只看 URL+secret，不看 enabled：测试目的就是验证未启用的配置
// （D2-14）。TargetHost 只返回主机名，供审计 detail 使用。
type WebhookTester interface {
	Configured(ctx context.Context) bool
	SendTest(ctx context.Context, payload []byte) (notify.WebhookResult, error)
	TargetHost() string
}

// SettingsHandlers 持有设置端点依赖，可并发使用。
type SettingsHandlers struct {
	service SettingsService
	authz   *rbac.AuthzMiddleware
	// authzConfigured 记录调用方是否显式注入 RBAC：GET 的读门上，显式注入
	// 走权限判定（与 OpenAPI 的 settings:manage 一致），未注入的单测装配退回
	// 「仅管理员角色」的只读门。
	authzConfigured bool
	audit           *audit.Logger
	idem            IdempotencyStore
	log             *slog.Logger
	webhook         WebhookTester
	rdb             *redis.Client
}

var settingsUpdateAction = rbac.AuthzAction{ResourceKind: "settings", ActionName: "update"}

// settingsReadAction 是 GET /api/settings 的读动作。读动作不登记字段策略
// （RBAC 只对写方法的未登记动作失败关闭），但需要独立登记项以便与写动作区分。
var settingsReadAction = rbac.AuthzAction{ResourceKind: "settings", ActionName: "read"}

// settingsTestWebhookAction 是测试 Webhook 的字段级写策略键。端点本身没有
// 请求体（配置从已保存快照读取），空 Fields 仍必须登记：RBAC 对任何写方法
// 的未登记动作都失败关闭（security-02 §2.6.1 规则 6）。
var settingsTestWebhookAction = rbac.AuthzAction{ResourceKind: "settings", ActionName: "test_webhook"}

// settingsResumeWebhookAction 是恢复 Webhook 推送的写策略键。端点没有请求体，
// 与 test_webhook 一样登记空字段集（RBAC 对未登记写动作失败关闭）。
var settingsResumeWebhookAction = rbac.AuthzAction{ResourceKind: "settings", ActionName: "resume_webhook"}

// NewSettingsHandlers 构造设置端点。
func NewSettingsHandlers(cfg SettingsHandlerConfig) (*SettingsHandlers, error) {
	if cfg.Service == nil {
		return nil, errors.New("httpapi: SettingsHandlerConfig.Service is required")
	}
	authz := cfg.Authz
	if authz == nil {
		authz = rbac.NewAuthzMiddleware()
	}
	// 写侧字段白名单：section/values/expect_updated_at。values 是「键→任意 JSON」
	// 的不透明叶子，键的合法性由注册表在服务层校验（未知键 400）。
	authz.SetWritePolicy(settingsUpdateAction, &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{
		"section":           nil,
		"values":            nil,
		"expect_updated_at": nil,
		// 危险项二次确认：前端确认后置 true。它是确认证据而不是设置值，
		// 服务层把它映射为 UpdateInput.DangerousConfirmed。
		"dangerous_confirmed": nil,
	}})
	idem := cfg.Idem
	if idem == nil {
		idem = NewMemoryIdempotencyStore()
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	authz.SetWritePolicy(settingsTestWebhookAction, &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{}})
	authz.SetWritePolicy(settingsResumeWebhookAction, &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{}})
	return &SettingsHandlers{
		service: cfg.Service, authz: authz, authzConfigured: cfg.Authz != nil,
		audit: cfg.Audit, idem: idem, log: log,
		webhook: cfg.Webhook, rdb: cfg.Redis,
	}, nil
}

// Register 把端点挂到受保护子树。
func (h *SettingsHandlers) Register(mux *http.ServeMux) {
	// R-26：读设置与写设置同一权限门（settings:manage），与契约一致——
	// 「能改设置」的权限自然覆盖「能看设置」，受限管理员不得读取整段配置
	// （其中含支付/通知等敏感面）。
	getSettings := http.Handler(http.HandlerFunc(h.handleGet))
	if h.authzConfigured {
		getSettings = h.authz.Handler(rbac.PermSettingsManage, settingsReadAction)(getSettings)
	} else {
		// 未注入 RBAC 的单测装配：保留仅管理员角色的只读门。
		getSettings = requireAdminRole(getSettings)
	}
	mux.Handle("GET /api/settings", getSettings)
	mux.Handle("PUT /api/settings",
		h.authz.Handler(rbac.PermSettingsManage, settingsUpdateAction)(http.HandlerFunc(h.handlePut)))
	// 发送测试事件（F-08-20）：写语义（有出站副作用），权限 settings:manage；
	// 审计动作独立于 settings.update，便于区分「改配置」与「探测端点」。
	mux.Handle("POST /api/settings/test/webhook",
		h.authz.Handler(rbac.PermSettingsManage, settingsTestWebhookAction)(http.HandlerFunc(h.handleTestWebhook)))
	// 恢复自动暂停的 Webhook 推送（F-08-20）。与测试端点同为 settings:manage：
	// 它改变的是出站投递状态，不是普通设置值，但仍属于设置管理面。
	mux.Handle("POST /api/settings/notify-webhook:resume",
		h.authz.Handler(rbac.PermSettingsManage, settingsResumeWebhookAction)(http.HandlerFunc(h.handleResumeWebhook)))
}

func (h *SettingsHandlers) handleGet(w http.ResponseWriter, r *http.Request) {
	section := strings.TrimSpace(r.URL.Query().Get("section"))
	if section == "" {
		writeProblemDetail(w, http.StatusBadRequest, authProblemBadRequest,
			"Invalid request", "section is required")
		return
	}
	view, err := h.service.Get(r.Context(), section)
	if err != nil {
		status, title, detail := settingsErrorFields(err)
		if status == http.StatusInternalServerError {
			h.log.Error("settings: read failed", "section", section, "err", err)
		}
		writeProblemDetail(w, status, settingsProblemType(status), title, detail)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

type settingsUpdateRequest struct {
	Section            string                     `json:"section"`
	Values             map[string]json.RawMessage `json:"values"`
	ExpectUpdatedAt    *time.Time                 `json:"expect_updated_at"`
	DangerousConfirmed bool                       `json:"dangerous_confirmed"`
}

type settingsUpdateResponse struct {
	Section   string         `json:"section"`
	UpdatedAt *time.Time     `json:"updated_at"`
	Values    map[string]any `json:"values"`
}

func (h *SettingsHandlers) handlePut(w http.ResponseWriter, r *http.Request) {
	h.serveIdempotent(w, r, func() (int, []byte, bool) {
		section := strings.TrimSpace(r.URL.Query().Get("section"))
		if section == "" {
			status := http.StatusBadRequest
			return status, settingsProblem(status, "Invalid request", "section is required"), false
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxAuthBodyBytes))
		if err != nil {
			status := http.StatusBadRequest
			return status, settingsProblem(status, "Invalid request", "unable to read request body"), false
		}
		var req settingsUpdateRequest
		if err := json.Unmarshal(body, &req); err != nil {
			status := http.StatusBadRequest
			return status, settingsProblem(status, "Invalid request", "invalid JSON body"), false
		}
		if req.Section != "" && req.Section != section {
			status := http.StatusBadRequest
			return status, settingsProblem(status, "Invalid request",
				"body section does not match query section"), false
		}
		// 契约要求 values 必填（空对象 {} 合法，表示本次不改任何键）：
		// 缺失/null 不能静默当成空更新成功。
		if req.Values == nil {
			status := http.StatusBadRequest
			return status, settingsProblem(status, "Invalid request", "values is required"), false
		}

		principal, _ := auth.PrincipalFrom(r.Context())
		actor := int64(0)
		if principal != nil {
			actor = principal.UserID
		}
		result, err := h.service.Put(r.Context(), settings.UpdateInput{
			Section:            section,
			Values:             req.Values,
			ExpectUpdatedAt:    req.ExpectUpdatedAt,
			DangerousConfirmed: req.DangerousConfirmed,
			Actor:              actor,
		})
		if err != nil {
			status, title, detail := settingsErrorFields(err)
			if status == http.StatusInternalServerError {
				h.log.Error("settings: update failed", "section", section, "err", err)
			}
			var verr *settings.ValidationError
			if errors.As(err, &verr) && verr.Key != "" {
				return status, settingsProblemWithFields(status, title, detail, verr.Key), false
			}
			return status, settingsProblem(status, title, detail), false
		}

		// 审计在提交之后、响应之前写：设置写是最高敏感操作，必须可追溯。
		// 审计失败不覆盖已成功的业务（writeAuditEvent 自带 3s 上限）。
		h.writeUpdateAudit(r, section, actor, result)

		payload, err := json.Marshal(settingsUpdateResponse{
			Section:   result.Section,
			UpdatedAt: result.UpdatedAt,
			Values:    result.Values,
		})
		if err != nil {
			status := http.StatusInternalServerError
			return status, settingsProblem(status, "Internal Server Error", ""), false
		}
		return http.StatusOK, payload, true
	})
}

func (h *SettingsHandlers) writeUpdateAudit(r *http.Request, section string, actor int64, result settings.UpdateResult) {
	if h.audit == nil {
		return
	}
	detail, err := json.Marshal(map[string]any{
		"section": section,
		"changed": result.Changed,
	})
	if err != nil {
		return
	}
	before, _ := json.Marshal(result.Before)
	after, _ := json.Marshal(result.After)
	event := audit.Event{
		Action:     audit.ActionSettingsUpdate,
		Actor:      actor,
		Method:     http.MethodPut,
		Route:      "/api/settings",
		StatusCode: http.StatusOK,
		IP:         clientIP(r),
		UserAgent:  r.UserAgent(),
		Detail:     detail,
		Before:     before,
		After:      after,
	}
	_ = writeAuditEvent(r.Context(), h.audit, event)
}

// settingsErrorFields 把服务层错误映射成状态码与文案。
func settingsErrorFields(err error) (status int, title, detail string) {
	switch {
	case errors.Is(err, settings.ErrConflict):
		return http.StatusConflict, "Settings were modified by someone else", "refresh and retry"
	case errors.Is(err, settings.ErrUnknownSection):
		return http.StatusBadRequest, "Invalid request", "unknown section"
	case errors.Is(err, settings.ErrSecretSealerRequired):
		return http.StatusInternalServerError, "Internal Server Error", ""
	default:
		var locked *settings.LockedError
		if errors.As(err, &locked) {
			// F-08-03 TC-04：锁定键（nav.console.require_auth）被改时 422。
			return http.StatusUnprocessableEntity, "Setting is read-only", locked.Error()
		}
		var verr *settings.ValidationError
		if errors.As(err, &verr) {
			return http.StatusBadRequest, "Invalid settings", verr.Error()
		}
		return http.StatusInternalServerError, "Internal Server Error", ""
	}
}

// problemTypeGatewayTimeout 用于「上游未在时限内回应」的 504（目前是测试
// Webhook 的传输超时）。不能落到 internal_error：504 是可重试的上游故障，
// 而 internal_error 会引导前端当作服务端缺陷处理。
const problemTypeGatewayTimeout = "https://newtg.dev/problems/gateway_timeout"

func settingsProblemType(status int) string {
	switch status {
	case http.StatusBadRequest:
		return authProblemBadRequest
	case http.StatusConflict:
		return authProblemConflict
	case http.StatusUnprocessableEntity:
		return problemTypeValidationFailed
	case http.StatusGatewayTimeout:
		return problemTypeGatewayTimeout
	default:
		return authProblemInternal
	}
}

func settingsProblem(status int, title, detail string) []byte {
	problem := map[string]any{"type": settingsProblemType(status), "title": title, "status": status}
	if detail != "" {
		problem["detail"] = detail
	}
	raw, _ := json.Marshal(problem)
	return raw
}

// settingsProblemWithFields 在 Problem 里附带 fields 扩展（rfc7807 之外的
// 既有管理面约定）：前端据此把错误落到具体字段而不是整段错误条。
func settingsProblemWithFields(status int, title, detail, field string) []byte {
	problem := map[string]any{
		"type":   settingsProblemType(status),
		"title":  title,
		"status": status,
		"fields": []string{field},
	}
	if detail != "" {
		problem["detail"] = detail
	}
	raw, _ := json.Marshal(problem)
	return raw
}

// requireAdminRole 是「GET admin」的只读门：只要求 role=admin，不要求
// settings:manage（modules-07 把读权限与写权限分开；写侧另有 settings:manage）。
func requireAdminRole(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := auth.PrincipalFrom(r.Context())
		if !ok || p == nil || p.UserID == 0 {
			writeUnauthorized(w)
			return
		}
		if p.Role != rbac.RoleAdmin {
			writeForbidden(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// serveIdempotent 与渠道端点同构：Claim → 执行 → Complete/Release。
// 作用域键绑定「用户 + 方法 + 完整 URI（含 section）+ 请求指纹」：同一个
// Idempotency-Key 被不同 section/不同请求体复用时不会被错误重放。
func (h *SettingsHandlers) serveIdempotent(w http.ResponseWriter, r *http.Request, build func() (int, []byte, bool)) {
	headerKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if headerKey == "" {
		status, body, _ := build()
		writeJSONBytes(w, status, body)
		return
	}

	bodyBytes, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxAuthBodyBytes))
	if err != nil {
		writeProblemDetail(w, http.StatusBadRequest, authProblemBadRequest,
			"Invalid request", "unable to read request body")
		return
	}
	r.Body = io.NopCloser(strings.NewReader(string(bodyBytes)))
	fingerprint := settingsFingerprint(bodyBytes)

	principal, _ := auth.PrincipalFrom(r.Context())
	userID := int64(0)
	if principal != nil {
		userID = principal.UserID
	}
	key := fmt.Sprintf("settings:%d:%s %s:%s:%s",
		userID, r.Method, r.URL.RequestURI(), fingerprint, headerKey)

	claim, err := h.idem.Claim(r.Context(), key, IdempotencyPendingTTL)
	if err != nil {
		// fail-open：幂等存储不可用不阻断管理写入（与渠道端点同口径）。
		status, body, _ := build()
		writeJSONBytes(w, status, body)
		return
	}

	switch claim.Status {
	case IdempotencyClaimReplay:
		w.Header().Set("Idempotent-Replayed", "true")
		writeJSONBytes(w, claim.Response.Status, claim.Response.Body)
		return
	case IdempotencyClaimInFlight:
		w.Header().Set("Retry-After", "1")
		writeProblemDetail(w, http.StatusConflict, problemTypeIdempotencyInProgress,
			"Idempotency key is in progress",
			"another request with this Idempotency-Key is still being processed; retry shortly")
		return
	}

	status, body, cacheable := build()

	// finalize 一律使用 WithoutCancel + 5s 上限：客户端断开不能把 pending
	// 标记留到 TTL，也不能让 Redis 故障拖住响应。
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), idempotencyFinalizeTimeout)
	defer cancel()
	if cacheable && status >= http.StatusOK && status < http.StatusMultipleChoices {
		if err := h.idem.Complete(ctx, key, claim.Token,
			IdempotentResponse{Status: status, Body: body}, IdempotencyTTL); err != nil {
			h.log.Error("httpapi: settings idempotency complete failed", "err", err)
		}
	} else {
		// 非 2xx 不固化，允许客户端修正参数后用同一个 key 重试。
		if err := h.idem.Release(ctx, key, claim.Token); err != nil {
			h.log.Error("httpapi: settings idempotency release failed", "err", err)
		}
	}
	writeJSONBytes(w, status, body)
}

func settingsFingerprint(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:16])
}
