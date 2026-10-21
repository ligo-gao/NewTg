// 模型部署控制面的管理端点（modules-08 §3.20 / F-08-52～F-08-58）。
//
// 这是 provider 实时数据的**纯代理面**：NEW TG 不持久化部署/容器/日志，
// 不在本地建镜像副本（§3.20.4）。本文件只做：
//
//	鉴权（settings:manage，含只读）→ 禁用门 → provider 调用 → 白名单投影 → 审计
//
// 安全约束（§3.20.5 / §3.20.6）：
//   - 仓库密钥、秘密环境变量只在单次写请求的内存中透传，不落库、不回显、
//     不进审计（连秘密环境变量的键名也不记）；
//   - 镜像 URL 记录审计前剥离 userinfo 与查询串；
//   - provider 错误正文由 ionet 客户端截断并抹掉 API Key / 请求中的秘密值。
//
// 幂等（§3.20.7）：创建/更新/改名/续期/终止五类写操作必须携带
// Idempotency-Key；复用渠道/设置端点的 Claim → Complete/Release 存储，
// 作用域 = 用户 + 方法 + 完整 URI + 请求指纹 + key。写超时返回 504
// 「结果未知」，**绝不自动重试**。
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"newtg/gateway/internal/api/dto"
	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/auth"
	deploybilling "newtg/gateway/internal/deploy/billing"
	"newtg/gateway/internal/deploy/ionet"
	"newtg/gateway/internal/rbac"

	"github.com/redis/go-redis/v9"
)

const (
	// 全量搜索的冻结上限（R-M6D-14）：10 页 / 1000 条。
	// provider 没有 keyword 参数时，网关按页扫描；达到上限置
	// search_truncated=true，绝不用「当前页过滤」冒充全量结果。
	deployScanMaxPages   = 10
	deployScanMaxItems   = 1000
	deployScanPageSize   = 100
	deployListDefaultP   = 1
	deployListDefaultRaw = 20
	deployListMaxRaw     = 100

	deployLogsDefaultLimit = 100
	deployLogsMaxLimit     = 1000
)

// §3.20.2 错误码的 RFC 7807 type。
const (
	problemTypeDeployNotFound         = "https://newtg.dev/problems/deploy_not_found"
	problemTypeDeployDisabled         = "https://newtg.dev/problems/deploy_disabled"
	problemTypeDeployNameTaken        = "https://newtg.dev/problems/deploy_name_taken"
	problemTypeDeployInvalidState     = "https://newtg.dev/problems/deploy_invalid_state"
	problemTypeDeployPriceUnavailable = "https://newtg.dev/problems/deploy_price_unavailable"
	// problemTypeDeployQuoteUnavailable 是 P-21：provider 回 200 但没有可用报价
	// （缺报价字段/负金额/非 USD）时创建与续期都拒绝，不静默按 0 计。
	problemTypeDeployQuoteUnavailable = "https://newtg.dev/problems/deploy_quote_unavailable"
	// problemTypeDeployBillingFailed 是站点成本账写失败（预扣/结算）。P-19 口径：
	// 失败只回滚部署创建，不产生任何用户扣费。
	problemTypeDeployBillingFailed = "https://newtg.dev/problems/deploy_billing_failed"
)

// deployLedgerTimeout 是账本写操作的独立超时。
//
// 不复用 provider 调用上下文：provider 的 30s 预算可能已耗尽，而预扣/结算
// 是本地事务，必须拿到自己的预算；也不跟随请求取消，否则客户端断开会让
// 「已创建的部署」与「未落库的预扣」失配。
const deployLedgerTimeout = 10 * time.Second

var (
	deployListAction     = rbac.AuthzAction{ResourceKind: "deployments", ActionName: "read"}
	deployCreateAction   = rbac.AuthzAction{ResourceKind: "deployments", ActionName: "create"}
	deployUpdateAction   = rbac.AuthzAction{ResourceKind: "deployments", ActionName: "update"}
	deployRenameAction   = rbac.AuthzAction{ResourceKind: "deployments", ActionName: "rename"}
	deployExtendAction   = rbac.AuthzAction{ResourceKind: "deployments", ActionName: "extend"}
	deployDeleteAction   = rbac.AuthzAction{ResourceKind: "deployments", ActionName: "delete"}
	deployPriceAction    = rbac.AuthzAction{ResourceKind: "deployments", ActionName: "price_estimation"}
	deployReadProviderID = rbac.AuthzAction{ResourceKind: "deployments", ActionName: "read_provider"}
)

// DeployBillingLedger 是站点 GPU 成本账在管理面的最小接缝。
//
// 只暴露写路径真正需要的四个方法：创建/续期预扣（Hold）、创建失败回滚
// （Release）、终止收敛（SettleTerminated）与只读的段定位。它是
// `*deploybilling.Ledger` 的子集——**不得**在 httpapi 里另写一套记账。
type DeployBillingLedger interface {
	Hold(ctx context.Context, est deploybilling.SegmentEstimate) (deploybilling.Segment, bool, error)
	Release(ctx context.Context, segmentID, reason string) error
	SettleTerminated(ctx context.Context, obs deploybilling.TerminationObservation) (deploybilling.TerminationResult, error)
}

// DeployManagementHandlerConfig 是部署管理端点的装配输入。
type DeployManagementHandlerConfig struct {
	// Settings 必填：读取 section=deploy 的启用状态与 has_secret。
	Settings SettingsService
	// Secrets 读取已存密钥明文（只在请求内存中使用）。
	Secrets DeploySecretStore
	// Provider 按生效密钥构造 provider 客户端；nil 时使用固定 host 的 ionet。
	Provider func(apiKey string) ionet.DeploymentProvider
	// Billing 是站点成本账接缝。nil 表示该进程不做部署计费（只有测试装配
	// 会省略）；生产装配在 gateway 与 worker 都构造真实账本。
	Billing DeployBillingLedger
	Authz   *rbac.AuthzMiddleware
	Audit   *audit.Logger
	Redis   *redis.Client
	Idem    IdempotencyStore
	Logger  *slog.Logger
	Now     func() time.Time
}

// DeployManagementHandlers 持有部署管理端点依赖，可并发使用。
type DeployManagementHandlers struct {
	service  SettingsService
	secrets  DeploySecretStore
	provider func(apiKey string) ionet.DeploymentProvider
	billing  DeployBillingLedger
	authz    *rbac.AuthzMiddleware
	audit    *audit.Logger
	rdb      *redis.Client
	idem     IdempotencyStore
	log      *slog.Logger
	now      func() time.Time
}

// NewDeployManagementHandlers 构造端点。
func NewDeployManagementHandlers(cfg DeployManagementHandlerConfig) (*DeployManagementHandlers, error) {
	if cfg.Settings == nil {
		return nil, errors.New("httpapi: DeployManagementHandlerConfig.Settings is required")
	}
	authz := cfg.Authz
	if authz == nil {
		authz = rbac.NewAuthzMiddleware()
	}
	// 每个写动作都登记字段白名单：未登记的写动作会被 RBAC 层失败关闭
	// （security-02 §2.6.1 规则 6）。秘密字段作为不透明叶子登记——值不参与
	// 白名单判定，但仍必须出现在白名单里才允许透传。
	authz.SetWritePolicy(deployCreateAction, &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{
		"name": nil, "hardware_id": nil, "location_ids": nil,
		"gpus_per_container": nil, "replica_count": nil, "duration_hours": nil,
		"image_url": nil, "registry_username": nil, "registry_secret": nil,
		"entrypoint": nil, "args": nil, "traffic_port": nil,
		"env_variables": nil, "secret_env_variables": nil,
	}})
	authz.SetWritePolicy(deployUpdateAction, &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{
		"image_url": nil, "registry_username": nil, "registry_secret": nil,
		"clear_registry_secret": nil, "entrypoint": nil, "args": nil, "command": nil,
		"traffic_port": nil, "env_variables": nil, "secret_env_variables": nil,
		"clear_secret_env": nil,
	}})
	authz.SetWritePolicy(deployRenameAction, &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{
		"name": nil,
	}})
	authz.SetWritePolicy(deployExtendAction, &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{
		"duration_hours": nil,
	}})
	// DELETE 没有请求体，但仍要登记：未登记的写动作无条件 403。
	authz.SetWritePolicy(deployDeleteAction, &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{}})
	authz.SetWritePolicy(deployPriceAction, &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{
		"hardware_id": nil, "location_ids": nil, "gpus_per_container": nil,
		"replica_count": nil, "duration_hours": nil, "currency": nil,
		"duration_type": nil, "duration_qty": nil, "hardware_qty": nil,
	}})

	secrets := cfg.Secrets
	if secrets == nil {
		secrets = missingSecretStore{}
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
	idem := cfg.Idem
	if idem == nil {
		idem = NewMemoryIdempotencyStore()
	}
	return &DeployManagementHandlers{
		service: cfg.Settings, secrets: secrets, provider: provider, billing: cfg.Billing,
		authz: authz, audit: cfg.Audit, rdb: cfg.Redis, idem: idem,
		log: log, now: now,
	}, nil
}

// Register 挂载部署管理端点。静态子路径全部先于 {id} 注册；Go 1.22 ServeMux
// 本身按「字面量优先」匹配，但显式顺序让契约在阅读时也成立。
func (h *DeployManagementHandlers) Register(mux *http.ServeMux) {
	read := func(action rbac.AuthzAction, fn http.HandlerFunc) http.Handler {
		return h.authz.Handler(rbac.PermSettingsManage, action)(fn)
	}

	// 静态子路径（先注册）。
	mux.Handle("GET /api/deployments", read(deployListAction, h.handleList))
	mux.Handle("GET /api/deployments/hardware-types", read(deployReadProviderID, h.handleHardwareTypes))
	mux.Handle("GET /api/deployments/locations", read(deployReadProviderID, h.handleLocations))
	mux.Handle("GET /api/deployments/available-replicas", read(deployReadProviderID, h.handleAvailableReplicas))
	mux.Handle("POST /api/deployments/price-estimation", read(deployPriceAction, h.handlePriceEstimation))
	mux.Handle("GET /api/deployments/check-name", read(deployReadProviderID, h.handleCheckName))
	mux.Handle("POST /api/deployments", read(deployCreateAction, h.handleCreate))

	// 参数路径。
	mux.Handle("GET /api/deployments/{id}", read(deployReadProviderID, h.handleGet))
	mux.Handle("GET /api/deployments/{id}/containers", read(deployReadProviderID, h.handleContainers))
	mux.Handle("GET /api/deployments/{id}/containers/{container_id}", read(deployReadProviderID, h.handleContainer))
	mux.Handle("GET /api/deployments/{id}/logs", read(deployReadProviderID, h.handleLogs))
	mux.Handle("PATCH /api/deployments/{id}", read(deployUpdateAction, h.handleUpdate))
	mux.Handle("PATCH /api/deployments/{id}/name", read(deployRenameAction, h.handleRename))
	mux.Handle("POST /api/deployments/{id}/extend", read(deployExtendAction, h.handleExtend))
	mux.Handle("DELETE /api/deployments/{id}", read(deployDeleteAction, h.handleDelete))
}

// ---------- 禁用门与 provider 装配 ----------

// gate 执行启用/密钥门（§3.20.8）。设置读取与连接测试不经过这里。
//
// 返回的 apiKey 只在本次请求内存中使用；从不写入响应/日志/审计。
func (h *DeployManagementHandlers) gate(w http.ResponseWriter, r *http.Request) (string, bool) {
	view, err := h.service.Get(r.Context(), "deploy")
	if err != nil {
		h.log.Error("deploy: read settings failed", "err", err)
		writeProblemDetail(w, http.StatusInternalServerError, authProblemInternal,
			"Internal Server Error", "")
		return "", false
	}
	state := projectDeploySettings(view)
	if !state.Enabled {
		writeProblemDetail(w, http.StatusConflict, problemTypeDeployDisabled,
			"Model deployment is disabled",
			"enable deploy.ionet.enabled before using model deployment endpoints")
		return "", false
	}
	apiKey, ok, err := h.secrets.StoredAPIKey(r.Context())
	if err != nil {
		// 解封失败可能包含密文/主密钥细节，绝不回显。
		h.log.Error("deploy: decrypt stored api key failed", "err", err)
		writeProblemDetail(w, http.StatusInternalServerError, authProblemInternal,
			"Internal Server Error", "")
		return "", false
	}
	if !ok || apiKey == "" {
		writeProblemDetail(w, http.StatusUnprocessableEntity, problemTypeDeploySecretMissing,
			"Deployment secret is not configured",
			"configure deploy.ionet.api_key in deployment settings")
		return "", false
	}
	return apiKey, true
}

func (h *DeployManagementHandlers) client(apiKey string) ionet.DeploymentProvider {
	return h.provider(apiKey)
}

func (h *DeployManagementHandlers) callContext(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), ionet.DefaultTimeout)
}

// ---------- 错误映射 ----------

// writeProviderError 把 provider 错误映射成 §3.20.2 的业务码。
func (h *DeployManagementHandlers) writeProviderError(w http.ResponseWriter, r *http.Request, err error) {
	status, problemType, title, detail := classifyDeployProviderError(err)
	var apiErr *ionet.APIError
	if errors.As(err, &apiErr) && apiErr.RetryAfter > 0 {
		// provider 的 429 建议等待时间原样透传（§3.20.7）。
		w.Header().Set("Retry-After", retrySeconds(apiErr.RetryAfter))
	}
	if status >= http.StatusInternalServerError {
		h.log.Warn("deploy: provider request failed", "status", status, "err", err)
	}
	writeProblemDetail(w, status, problemType, title, detail)
}

// classifyDeployProviderError 是部署管理面的错误映射表。
//
// 401/403 → 422 deploy_secret_invalid（管理员需要知道「密钥不对」）；
// 404 → 404 deploy_not_found；409 → 409 deploy_invalid_state；
// 429 → 429 deploy_rate_limited；5xx/不可解析 → 502；超时 → 504。
func classifyDeployProviderError(err error) (status int, problemType, title, detail string) {
	if ionet.IsTimeout(err) {
		return http.StatusGatewayTimeout, problemTypeDeployProviderTimeout,
			"Provider timeout",
			"provider did not respond in time; the write result is unknown—refresh before retrying"
	}
	var apiErr *ionet.APIError
	if errors.As(err, &apiErr) {
		switch {
		case apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden:
			return http.StatusUnprocessableEntity, problemTypeDeploySecretInvalid,
				"Deployment secret is invalid", "provider rejected the API key"
		case apiErr.StatusCode == http.StatusNotFound:
			return http.StatusNotFound, problemTypeDeployNotFound,
				"Deployment not found", "the deployment or container does not exist at the provider"
		case apiErr.StatusCode == http.StatusConflict:
			return http.StatusConflict, problemTypeDeployInvalidState,
				"Deployment state does not allow this operation", apiErr.Detail
		case apiErr.StatusCode == http.StatusBadRequest || apiErr.StatusCode == http.StatusUnprocessableEntity:
			return http.StatusBadRequest, problemTypeDeployInvalidRequest,
				"Invalid request", apiErr.Detail
		case apiErr.StatusCode == http.StatusTooManyRequests:
			return http.StatusTooManyRequests, problemTypeDeployRateLimited,
				"Provider rate limit reached", "provider rate limit reached"
		default:
			return http.StatusBadGateway, problemTypeDeployProviderError, "Provider error", apiErr.Detail
		}
	}
	var invalid *ionet.InvalidResponseError
	if errors.As(err, &invalid) {
		return http.StatusBadGateway, problemTypeDeployProviderError,
			"Provider error", "provider returned an unreadable response"
	}
	return http.StatusBadGateway, problemTypeDeployProviderError, "Provider error", "provider request failed"
}

// ---------- 审计 ----------

// writeDeployAudit 写入 deploy.* 审计。
//
// detail 由调用方只填非秘密字段；本函数统一补 provider/result/request_id。
// before/after 只允许非秘密快照——秘密值与其键名不得传入。
func (h *DeployManagementHandlers) writeDeployAudit(r *http.Request, actor int64,
	action string, status int, ok bool, detail map[string]any, before, after any) {
	if h.audit == nil {
		return
	}
	if detail == nil {
		detail = map[string]any{}
	}
	detail["provider"] = deployProviderName
	result := "success"
	if !ok {
		result = "failed"
	}
	detail["result"] = result
	raw, err := json.Marshal(detail)
	if err != nil {
		return
	}
	event := audit.Event{
		Action:     action,
		Actor:      actor,
		Result:     result,
		Method:     r.Method,
		Route:      r.Pattern,
		StatusCode: status,
		IP:         clientIP(r),
		UserAgent:  r.UserAgent(),
		Detail:     raw,
	}
	if before != nil {
		event.Before, _ = json.Marshal(before)
	}
	if after != nil {
		event.After, _ = json.Marshal(after)
	}
	_ = writeAuditEvent(r.Context(), h.audit, event)
}

// sanitizeImageRef 在写入审计前剥离 userinfo、查询串与片段。
// 解析失败时返回空串——宁可审计里缺这个字段，也不原样记录可疑 URL。
func sanitizeImageRef(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

// ---------- 站点成本账接线（H1/H2） ----------

// ledgerContext 给账本写操作一个独立预算（见 deployLedgerTimeout）。
func (h *DeployManagementHandlers) ledgerContext(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(r.Context()), deployLedgerTimeout)
}

// deployClientRequestID 是 P-20 幂等键里的客户端请求标识：写操作必备的
// Idempotency-Key 头（serveWriteIdempotent 已强制非空）。
func deployClientRequestID(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get("Idempotency-Key"))
}

// deployCreateIdempotencyKey 是冻结的创建预扣键（P-20）。
//
// 不含部署 ID：预扣发生在 provider 返回部署 ID 之前（报价门与写入顺序见
// billing-03 §5.9 逐项约束 1），所以只能由用户 + 客户端请求标识定位同一
// 次创建。同一键重入由 Hold 的 idempotency_key 唯一约束返回既有段。
func deployCreateIdempotencyKey(userID int64, clientRequestID string) string {
	return fmt.Sprintf("deploy:create:%d:%s", userID, clientRequestID)
}

// deployExtendIdempotencyKey 是冻结的续期预扣键（P-20）。
//
// 续期是「同一时长的一次新段」：同部署同时长重入应命中同一段，不同时长
// 是不同购买，必须各自成段。
func deployExtendIdempotencyKey(deploymentID string, hours int) string {
	return fmt.Sprintf("deploy:extend:%s:%d", deploymentID, hours)
}

// deployQuote 把 provider 报价归一成计费报价（P-21）。
func deployQuote(price ionet.PriceEstimationResponse, name string, hardwareID int64,
	locationIDs []int64, gpus, replicas int, durationSeconds int64) deploybilling.Quote {
	quote := deploybilling.QuoteFromPrice(price.EstimatedCost.Decimal,
		price.EstimatedCostSet, price.EstimationValid, price.Currency)
	quote.Name = name
	quote.HardwareID = hardwareID
	quote.LocationIDs = locationIDs
	quote.GPUsPerContainer = gpus
	quote.ReplicaCount = replicas
	quote.DurationSeconds = durationSeconds
	return quote
}

func deployQuoteUnavailableBytes(w http.ResponseWriter, r *http.Request, verb string) (int, []byte) {
	return deployProblemBytes(w, r, http.StatusUnprocessableEntity, problemTypeDeployQuoteUnavailable,
		"Price quote unavailable",
		"provider did not return a usable USD quote; the deployment was not "+verb)
}

func int64Locations(ids []int) []int64 {
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		out = append(out, int64(id))
	}
	return out
}

// ---------- 幂等写包装 ----------

// serveWriteIdempotent 覆盖五类写操作。
//
// 与设置端点同构，但两点不同：
//  1. Idempotency-Key **必须**存在，缺失直接 400（§3.20.7），而不是退化为
//     非幂等执行；
//  2. 只有 2xx 会被 Complete 固化。504「结果未知」属于非 2xx → Release，
//     管理员必须刷新确认后再用新的 key 重试，不得自动重放。
func (h *DeployManagementHandlers) serveWriteIdempotent(w http.ResponseWriter, r *http.Request,
	build func() (int, []byte)) {
	actor := actorID(r)
	if limited, retry := h.allowDeployWrite(r.Context(), actor); limited {
		w.Header().Set("Retry-After", retrySeconds(retry))
		writeProblemDetail(w, http.StatusTooManyRequests, problemTypeDeployRateLimited,
			"Too many requests", "deployment write operations are limited to 5 requests per minute")
		return
	}

	headerKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if headerKey == "" {
		writeProblemDetail(w, http.StatusBadRequest, problemTypeDeployInvalidRequest,
			"Invalid request", "Idempotency-Key header is required for deployment write operations")
		return
	}

	bodyBytes, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxAuthBodyBytes))
	if err != nil {
		writeProblemDetail(w, http.StatusBadRequest, problemTypeDeployInvalidRequest,
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
	// 作用域包含请求指纹：同一个 key 被不同请求体复用时不会错误重放。
	key := fmt.Sprintf("deploy:%d:%s %s:%s:%s",
		userID, r.Method, r.URL.RequestURI(), fingerprint, headerKey)

	claim, err := h.idem.Claim(r.Context(), key, IdempotencyPendingTTL)
	if err != nil {
		// fail-open：幂等存储不可用不阻断管理写入；provider 侧重复风险由
		// 「不自动重试」与管理员的新 key 流程兜底。
		status, body := build()
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

	status, body := build()

	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), idempotencyFinalizeTimeout)
	defer cancel()
	if status >= http.StatusOK && status < http.StatusMultipleChoices && body != nil {
		if err := h.idem.Complete(ctx, key, claim.Token,
			IdempotentResponse{Status: status, Body: body}, IdempotencyTTL); err != nil {
			h.log.Error("deploy: idempotency complete failed", "err", err)
		}
	} else {
		if err := h.idem.Release(ctx, key, claim.Token); err != nil {
			h.log.Error("deploy: idempotency release failed", "err", err)
		}
	}
	// body 为 nil 表示 build 已经直接写好了响应（如禁用门/权限前置错误），
	// 不能再写第二遍。
	if body == nil {
		return
	}
	writeJSONBytes(w, status, body)
}

// allowDeployWrite 复用 T2 的连接测试管理员桶（5 次/分钟）。
// 维度是管理员 ID；Redis 不可用时 fail-open（provider 侧仍有其自身限流）。
func (h *DeployManagementHandlers) allowDeployWrite(ctx context.Context, actor int64) (bool, time.Duration) {
	if h.rdb == nil {
		return false, 0
	}
	key := RateLimitKeyPrefix + "DT:admin:" + strconv.FormatInt(actor, 10)
	limited, retry, err := allowRequestRate(ctx, h.rdb, key, deployTestRateLimit, deployTestRateWindow)
	if err != nil {
		h.log.Warn("deploy: rate limiter unavailable; allowing write", "err", err)
		return false, 0
	}
	return limited, retry
}

// ---------- 列表 ----------

type deployListItem struct {
	ID                      string     `json:"id"`
	Name                    string     `json:"name"`
	Status                  string     `json:"status"`
	CompletedPercent        float64    `json:"completed_percent"`
	GPUCount                int        `json:"gpu_count"`
	ReplicaCount            int        `json:"replica_count"`
	HardwareName            string     `json:"hardware_name"`
	BrandName               string     `json:"brand_name"`
	ComputeMinutesServed    int        `json:"compute_minutes_served"`
	ComputeMinutesRemaining int        `json:"compute_minutes_remaining"`
	CreatedAt               *time.Time `json:"created_at"`
}

type deployListResponse struct {
	Items           []deployListItem `json:"items"`
	Total           int              `json:"total"`
	Page            int              `json:"page"`
	PageSize        int              `json:"page_size"`
	StatusCounts    map[string]int64 `json:"status_counts"`
	SearchTruncated bool             `json:"search_truncated,omitempty"`
}

func (h *DeployManagementHandlers) handleList(w http.ResponseWriter, r *http.Request) {
	apiKey, ok := h.gate(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	page, valid := parseDeployInt(q.Get("p"), deployListDefaultP)
	if !valid || page < 1 {
		writeProblemDetail(w, http.StatusBadRequest, problemTypeDeployInvalidRequest,
			"Invalid request", "p must be a positive integer")
		return
	}
	pageSize, valid := parseDeployInt(q.Get("page_size"), deployListDefaultRaw)
	if !valid || pageSize < 1 {
		writeProblemDetail(w, http.StatusBadRequest, problemTypeDeployInvalidRequest,
			"Invalid request", "page_size must be a positive integer")
		return
	}
	// 超上限按上限截断，并在响应回显实际 page_size（§3.20.8 超限状态）。
	if pageSize > deployListMaxRaw {
		pageSize = deployListMaxRaw
	}
	statusFilter := strings.ToLower(strings.TrimSpace(q.Get("status")))
	keyword := strings.TrimSpace(q.Get("keyword"))

	ctx, cancel := h.callContext(r)
	defer cancel()
	provider := h.client(apiKey)
	scanned, total, truncated, err := scanDeployments(ctx, provider, keyword)
	if err != nil {
		h.writeProviderError(w, r, err)
		return
	}

	counts := map[string]int64{"all": int64(len(scanned))}
	for _, item := range scanned {
		counts[item.Status]++
	}
	filtered := make([]deployListItem, 0, len(scanned))
	for _, item := range scanned {
		if statusFilter != "" && item.Status != statusFilter {
			continue
		}
		filtered = append(filtered, deployListItem{
			ID: item.ID, Name: item.Name, Status: item.Status,
			CompletedPercent: item.CompletedPercent, GPUCount: item.GPUCount,
			ReplicaCount: item.ReplicaCount, HardwareName: item.HardwareName,
			BrandName:               item.BrandName,
			ComputeMinutesServed:    item.ComputeMinutesServed,
			ComputeMinutesRemaining: item.ComputeMinutesRemaining,
			CreatedAt:               deployTimePtr(item.CreatedAt),
		})
	}
	total = len(filtered)

	start := (page - 1) * pageSize
	if start > len(filtered) {
		start = len(filtered)
	}
	end := start + pageSize
	if end > len(filtered) {
		end = len(filtered)
	}
	writeJSON(w, http.StatusOK, deployListResponse{
		Items: filtered[start:end], Total: total, Page: page, PageSize: pageSize,
		StatusCounts: counts, SearchTruncated: truncated,
	})
}

// scanDeployments 按页扫描全量结果集（冻结上限 10 页 / 1000 条）。
//
// 为什么不在 provider 侧过滤：provider 没有 keyword 参数，且 status_counts
// 需要全量口径。扫描**不带 status 过滤**，否则计数只会剩下被选中的状态；
// status 过滤在扫描完成后进行。
func scanDeployments(ctx context.Context, provider ionet.DeploymentProvider,
	keyword string) ([]ionet.DeploymentSummary, int, bool, error) {
	var (
		raw      []ionet.DeploymentSummary
		total    int
		complete bool
	)
	for page := 1; page <= deployScanMaxPages; page++ {
		result, err := provider.ListDeployments(ctx, ionet.DeploymentListOptions{
			Page: page, PageSize: deployScanPageSize,
			SortBy: "created_at", SortOrder: "desc",
		})
		if err != nil {
			return nil, 0, false, err
		}
		if result.Total > 0 {
			total = result.Total
		}
		raw = append(raw, result.Items...)
		// 条数上限优先于页数上限：第 10 页可能一次带回超过剩余额度的条目。
		if len(raw) >= deployScanMaxItems {
			raw = raw[:deployScanMaxItems]
			break
		}
		// 明确的结束条件（这两个才是「已取全」）：
		//   1. 本页不足一页 → provider 已经没有下一页；
		//   2. provider 声明了 total 且本次页已覆盖它。
		// provider 不返回 total 时**不能**用本页条数推断总量（复核 HIGH-1），
		// 只能靠「不足一页」或扫到冻结上限来终止。
		if len(result.Items) < deployScanPageSize ||
			(total > 0 && page*deployScanPageSize >= total) {
			complete = true
			break
		}
	}
	if total == 0 {
		// total 未知时对外仍要给出一个可用的 total：用实际扫描量，并由
		// truncated 明确标记它是否只是窗口内计数。
		total = len(raw)
	}
	// keyword 过滤在完整扫描之后：截断判定必须基于原始条数，否则「关键词
	// 命中很少」会被误判成 provider 数据未取全。
	lowered := strings.ToLower(keyword)
	scanned := make([]ionet.DeploymentSummary, 0, len(raw))
	for _, item := range raw {
		if keyword != "" &&
			!strings.Contains(strings.ToLower(item.Name), lowered) &&
			!strings.Contains(strings.ToLower(item.ID), lowered) {
			continue
		}
		scanned = append(scanned, item)
	}
	// truncated 口径：
	//   - 扫描未走到明确终点（命中 10 页/1000 条上限，或 provider 既没给
	//     total 又一直返回满页）→ true，因为无法确认已取全；
	//   - provider 声明的 total 大于已扫描量 → true。
	truncated := !complete || total > len(raw)
	return scanned, total, truncated, nil
}

// ---------- 资源目录与价格 ----------

func (h *DeployManagementHandlers) handleHardwareTypes(w http.ResponseWriter, r *http.Request) {
	apiKey, ok := h.gate(w, r)
	if !ok {
		return
	}
	ctx, cancel := h.callContext(r)
	defer cancel()
	result, err := h.client(apiKey).ListHardware(ctx)
	if err != nil {
		h.writeProviderError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *DeployManagementHandlers) handleLocations(w http.ResponseWriter, r *http.Request) {
	apiKey, ok := h.gate(w, r)
	if !ok {
		return
	}
	ctx, cancel := h.callContext(r)
	defer cancel()
	result, err := h.client(apiKey).ListLocations(ctx)
	if err != nil {
		h.writeProviderError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *DeployManagementHandlers) handleAvailableReplicas(w http.ResponseWriter, r *http.Request) {
	apiKey, ok := h.gate(w, r)
	if !ok {
		return
	}
	hardwareID, valid := parseDeployInt(r.URL.Query().Get("hardware_id"), 0)
	if !valid || hardwareID <= 0 {
		writeProblemDetail(w, http.StatusBadRequest, problemTypeDeployInvalidRequest,
			"Invalid request", "hardware_id must be a positive integer")
		return
	}
	gpuCount, valid := parseDeployInt(r.URL.Query().Get("gpu_count"), 1)
	if !valid || gpuCount < 1 {
		writeProblemDetail(w, http.StatusBadRequest, problemTypeDeployInvalidRequest,
			"Invalid request", "gpu_count must be a positive integer")
		return
	}
	ctx, cancel := h.callContext(r)
	defer cancel()
	result, err := h.client(apiKey).AvailableReplicas(ctx, hardwareID, gpuCount)
	if err != nil {
		h.writeProviderError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

type deployPriceRequest struct {
	HardwareID       int    `json:"hardware_id"`
	LocationIDs      []int  `json:"location_ids"`
	GPUsPerContainer int    `json:"gpus_per_container"`
	ReplicaCount     int    `json:"replica_count"`
	DurationHours    int    `json:"duration_hours"`
	Currency         string `json:"currency"`
	DurationType     string `json:"duration_type"`
	DurationQty      int    `json:"duration_qty"`
	HardwareQty      int    `json:"hardware_qty"`
}

func (h *DeployManagementHandlers) handlePriceEstimation(w http.ResponseWriter, r *http.Request) {
	apiKey, ok := h.gate(w, r)
	if !ok {
		return
	}
	var req deployPriceRequest
	if err := decodeDeployJSON(r, &req); err != nil {
		writeProblemDetail(w, http.StatusBadRequest, problemTypeDeployInvalidRequest,
			"Invalid request", err.Error())
		return
	}
	if detail := validatePriceRequest(req); detail != "" {
		writeProblemDetail(w, http.StatusBadRequest, problemTypeDeployInvalidRequest,
			"Invalid request", detail)
		return
	}
	ctx, cancel := h.callContext(r)
	defer cancel()
	result, err := h.client(apiKey).EstimatePrice(ctx, ionet.PriceEstimationRequest{
		HardwareID: req.HardwareID, LocationIDs: req.LocationIDs,
		GPUsPerContainer: req.GPUsPerContainer, ReplicaCount: req.ReplicaCount,
		DurationHours: req.DurationHours, Currency: req.Currency,
		DurationType: req.DurationType, DurationQty: req.DurationQty,
		HardwareQty: req.HardwareQty,
	})
	if err != nil {
		h.writeProviderError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func validatePriceRequest(req deployPriceRequest) string {
	if req.HardwareID <= 0 {
		return "hardware_id must be a positive integer"
	}
	if len(req.LocationIDs) == 0 {
		return "location_ids must contain at least one id"
	}
	for _, id := range req.LocationIDs {
		if id <= 0 {
			return "location_ids must only contain positive integers"
		}
	}
	if req.GPUsPerContainer < 1 {
		return "gpus_per_container must be at least 1"
	}
	if req.ReplicaCount < 1 {
		return "replica_count must be at least 1"
	}
	if req.DurationHours < 1 {
		return "duration_hours must be at least 1"
	}
	return ""
}

func (h *DeployManagementHandlers) handleCheckName(w http.ResponseWriter, r *http.Request) {
	apiKey, ok := h.gate(w, r)
	if !ok {
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		writeProblemDetail(w, http.StatusBadRequest, problemTypeDeployInvalidRequest,
			"Invalid request", "name is required")
		return
	}
	ctx, cancel := h.callContext(r)
	defer cancel()
	available, err := h.client(apiKey).CheckName(ctx, name)
	if err != nil {
		h.writeProviderError(w, r, err)
		return
	}
	// 不可用是 200 结果而不是错误（§3.20.2）。
	writeJSON(w, http.StatusOK, struct {
		Name      string `json:"name"`
		Available bool   `json:"available"`
	}{Name: name, Available: available})
}

// ---------- 创建 ----------

type createDeploymentRequest struct {
	Name               string            `json:"name"`
	HardwareID         int               `json:"hardware_id"`
	LocationIDs        []int             `json:"location_ids"`
	GPUsPerContainer   int               `json:"gpus_per_container"`
	ReplicaCount       int               `json:"replica_count"`
	DurationHours      int               `json:"duration_hours"`
	ImageURL           string            `json:"image_url"`
	RegistryUsername   string            `json:"registry_username"`
	RegistrySecret     string            `json:"registry_secret"`
	Entrypoint         []string          `json:"entrypoint"`
	Args               []string          `json:"args"`
	TrafficPort        int               `json:"traffic_port"`
	EnvVariables       map[string]string `json:"env_variables"`
	SecretEnvVariables map[string]string `json:"secret_env_variables"`
}

func (h *DeployManagementHandlers) handleCreate(w http.ResponseWriter, r *http.Request) {
	h.serveWriteIdempotent(w, r, func() (int, []byte) {
		return h.createDeployment(w, r)
	})
}

func (h *DeployManagementHandlers) createDeployment(w http.ResponseWriter, r *http.Request) (int, []byte) {
	apiKey, ok := h.gate(w, r)
	if !ok {
		return 0, nil
	}
	actor := actorID(r)
	requestID := newRelayRequestID()
	var req createDeploymentRequest
	if err := decodeDeployJSON(r, &req); err != nil {
		return deployProblemBytes(w, r, http.StatusBadRequest, problemTypeDeployInvalidRequest,
			"Invalid request", err.Error())
	}
	if detail := validateCreateRequest(&req); detail != "" {
		return deployProblemBytes(w, r, http.StatusBadRequest, problemTypeDeployInvalidRequest,
			"Invalid request", detail)
	}

	ctx, cancel := h.callContext(r)
	defer cancel()
	provider := h.client(apiKey)

	// 名称查重（创建前一次）。
	available, err := provider.CheckName(ctx, req.Name)
	if err != nil {
		status, body := h.providerErrorBody(w, r, err)
		_, code := deployErrorCode(err)
		h.writeDeployAudit(r, actor, audit.ActionDeployCreate, status, false,
			createAuditDetail(req, "", requestID, code), nil, nil)
		return status, body
	}
	if !available {
		h.writeDeployAudit(r, actor, audit.ActionDeployCreate, http.StatusConflict,
			false, createAuditDetail(req, "", requestID, "deploy_name_taken"), nil, nil)
		return deployProblemBytes(w, r, http.StatusConflict, problemTypeDeployNameTaken,
			"Deployment name is already taken", "choose a different deployment name")
	}

	// 报价门（§3.20.3 / brief 语义 3）：报价失败不得发起 provider 写。
	price, err := provider.EstimatePrice(ctx, priceRequestFromCreate(req))
	if err != nil {
		h.writeDeployAudit(r, actor, audit.ActionDeployCreate, http.StatusUnprocessableEntity,
			false, createAuditDetail(req, "", requestID, "deploy_price_unavailable"), nil, nil)
		return deployProblemBytes(w, r, http.StatusUnprocessableEntity, problemTypeDeployPriceUnavailable,
			"Price estimation unavailable",
			"provider price estimation failed; no deployment was created")
	}
	// P-21：200 但没有可用报价（缺字段/负金额/非 USD）同样拒绝，绝不按 0 计。
	quote := deployQuote(price, req.Name, int64(req.HardwareID), int64Locations(req.LocationIDs),
		req.GPUsPerContainer, req.ReplicaCount, int64(req.DurationHours)*3600)
	if !quote.Available {
		h.writeDeployAudit(r, actor, audit.ActionDeployCreate, http.StatusUnprocessableEntity,
			false, createAuditDetail(req, "", requestID, "quote_unavailable"), nil, nil)
		return deployQuoteUnavailableBytes(w, r, "created")
	}

	// 提交前再查重一次（对照 §3.20.3：名称可能在两次检查之间被占用）。
	available, err = provider.CheckName(ctx, req.Name)
	if err != nil {
		status, body := h.providerErrorBody(w, r, err)
		_, code := deployErrorCode(err)
		h.writeDeployAudit(r, actor, audit.ActionDeployCreate, status, false,
			createAuditDetail(req, "", requestID, code), nil, nil)
		return status, body
	}
	if !available {
		h.writeDeployAudit(r, actor, audit.ActionDeployCreate, http.StatusConflict,
			false, createAuditDetail(req, "", requestID, "deploy_name_taken"), nil, nil)
		return deployProblemBytes(w, r, http.StatusConflict, problemTypeDeployNameTaken,
			"Deployment name is already taken", "choose a different deployment name")
	}

	action, err := provider.CreateDeployment(ctx, ionet.CreateDeploymentRequest{
		Name: req.Name, HardwareID: req.HardwareID, LocationIDs: req.LocationIDs,
		GPUsPerContainer: req.GPUsPerContainer, ReplicaCount: req.ReplicaCount,
		DurationHours: req.DurationHours, ImageURL: req.ImageURL,
		RegistryUsername: req.RegistryUsername, RegistrySecret: req.RegistrySecret,
		Entrypoint: req.Entrypoint, Args: req.Args, TrafficPort: req.TrafficPort,
		EnvVariables: req.EnvVariables, SecretEnvVariables: req.SecretEnvVariables,
	})
	if err != nil {
		if providerConflict(err) {
			// 创建阶段的 provider 409 只可能是名称已占用（状态冲突在创建时
			// 不存在）；按名称冲突映射，管理员才知道该改名而不是重试。
			h.writeDeployAudit(r, actor, audit.ActionDeployCreate, http.StatusConflict,
				false, createAuditDetail(req, "", requestID, "deploy_name_taken"), nil, nil)
			return deployProblemBytes(w, r, http.StatusConflict, problemTypeDeployNameTaken,
				"Deployment name is already taken", "choose a different deployment name")
		}
		status, body := h.providerErrorBody(w, r, err)
		_, code := deployErrorCode(err)
		h.writeDeployAudit(r, actor, audit.ActionDeployCreate, status,
			false, createAuditDetail(req, "", requestID, code), nil, nil)
		return status, body
	}

	// H1：创建成功后在站点成本账预扣（P-18：Hold 只出现在创建/延期两处）。
	//
	// 为什么预扣只能在 provider 返回部署 ID 之后：段以 deployment_id 为对账
	// 主键（worker/reconcile 都按它查 provider），预扣先于创建就只能记一个
	// 占位 ID，之后永远无法与真实部署对上。P-19 的补偿口径因此是「预扣失败
	// 回滚部署创建」——只撤销本次创建，不触碰任何用户钱包/订阅/令牌。
	if h.billing != nil {
		est := deploybilling.EstimateForCreate(quote, req.GPUsPerContainer*req.ReplicaCount)
		est.DeploymentID = action.DeploymentID
		est.IdempotencyKey = deployCreateIdempotencyKey(actor, deployClientRequestID(r))
		ledgerCtx, cancelLedger := h.ledgerContext(r)
		_, _, holdErr := h.billing.Hold(ledgerCtx, est)
		cancelLedger()
		if holdErr != nil {
			h.log.Error("deploy: site cost hold failed",
				"deployment_id", action.DeploymentID, "err", holdErr)
			h.rollbackDeploymentCreation(r, provider, action.DeploymentID)
			h.writeDeployAudit(r, actor, audit.ActionDeployCreate, http.StatusBadGateway,
				false, createAuditDetail(req, action.DeploymentID, requestID, "deploy_billing_hold_failed"), nil, nil)
			return deployProblemBytes(w, r, http.StatusBadGateway, problemTypeDeployBillingFailed,
				"Deployment billing unavailable",
				"site cost reservation failed; the deployment creation was rolled back")
		}
	}

	h.writeDeployAudit(r, actor, audit.ActionDeployCreate, http.StatusAccepted,
		true, createAuditDetail(req, action.DeploymentID, requestID, ""), nil, nil)
	return jsonBody(w, r, http.StatusAccepted, struct {
		DeploymentID string `json:"deployment_id"`
		Status       string `json:"status"`
	}{DeploymentID: action.DeploymentID, Status: action.Status})
}

// rollbackDeploymentCreation 是预扣失败时的补偿：撤销刚创建的 provider 部署。
//
// best-effort：provider 删除失败只记日志，不改变响应——此时站点成本账没有
// 落段（Hold 失败），部署可能残留，运维可通过列表发现；反过来的顺序（先
// 返回失败再删除）会丢失「哪个部署需要清理」的信息。绝不重试 provider 写。
func (h *DeployManagementHandlers) rollbackDeploymentCreation(r *http.Request,
	provider ionet.DeploymentProvider, deploymentID string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), ionet.DefaultTimeout)
	defer cancel()
	if _, err := provider.DeleteDeployment(ctx, deploymentID); err != nil {
		h.log.Warn("deploy: rollback of deployment creation failed",
			"deployment_id", deploymentID, "err", err)
	}
}

func validateCreateRequest(req *createDeploymentRequest) string {
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		return "name is required"
	}
	if req.HardwareID <= 0 {
		return "hardware_id must be a positive integer"
	}
	if len(req.LocationIDs) == 0 {
		return "location_ids must contain at least one id"
	}
	for _, id := range req.LocationIDs {
		if id <= 0 {
			return "location_ids must only contain positive integers"
		}
	}
	if req.GPUsPerContainer < 1 {
		return "gpus_per_container must be at least 1"
	}
	// 上界与计费乘数冻结值一致（billing-05 §4.4.4）：越过上界会让段快照
	// 失真，而创建已经写到 provider，Hold 才拒绝会触发一次无谓的回滚。
	if req.GPUsPerContainer > deploybilling.MaxDeploymentGPUsPerContainer {
		return fmt.Sprintf("gpus_per_container must be at most %d", deploybilling.MaxDeploymentGPUsPerContainer)
	}
	if req.ReplicaCount < 1 {
		return "replica_count must be at least 1"
	}
	if req.ReplicaCount > deploybilling.MaxDeploymentReplicas {
		return fmt.Sprintf("replica_count must be at most %d", deploybilling.MaxDeploymentReplicas)
	}
	if req.DurationHours < 1 {
		return "duration_hours must be at least 1"
	}
	if req.DurationHours > deploybilling.MaxDeploymentDurationHours {
		return fmt.Sprintf("duration_hours must be at most %d", deploybilling.MaxDeploymentDurationHours)
	}
	if req.ImageURL = strings.TrimSpace(req.ImageURL); req.ImageURL == "" {
		return "image_url is required"
	}
	if parsed, err := url.Parse(req.ImageURL); err != nil || parsed.Host == "" ||
		(parsed.Scheme != "https" && parsed.Scheme != "http") {
		return "image_url must be an absolute http(s) URL"
	}
	if req.TrafficPort < 0 || req.TrafficPort > 65535 {
		return "traffic_port must be between 1 and 65535"
	}
	return ""
}

func priceRequestFromCreate(req createDeploymentRequest) ionet.PriceEstimationRequest {
	return ionet.PriceEstimationRequest{
		HardwareID: req.HardwareID, LocationIDs: req.LocationIDs,
		GPUsPerContainer: req.GPUsPerContainer, ReplicaCount: req.ReplicaCount,
		DurationHours: req.DurationHours,
	}
}

// createAuditDetail 只放非秘密字段；image_ref 已剥离 userinfo/query。
// 秘密环境变量的键名与值、仓库密钥都不进入 detail。
func createAuditDetail(req createDeploymentRequest, deploymentID, requestID, errorCode string) map[string]any {
	detail := map[string]any{
		"deployment_id":      deploymentID,
		"name":               req.Name,
		"hardware_id":        req.HardwareID,
		"location_ids":       req.LocationIDs,
		"gpus_per_container": req.GPUsPerContainer,
		"replica_count":      req.ReplicaCount,
		"duration_hours":     req.DurationHours,
		"image_ref":          sanitizeImageRef(req.ImageURL),
		"request_id":         requestID,
	}
	if errorCode != "" {
		detail["error_code"] = errorCode
	}
	return detail
}

// ---------- 详情 / 容器 / 日志 ----------

type deployDetailResponse struct {
	ID                      string                     `json:"id"`
	Status                  string                     `json:"status"`
	CreatedAt               *time.Time                 `json:"created_at"`
	StartedAt               *time.Time                 `json:"started_at"`
	FinishedAt              *time.Time                 `json:"finished_at"`
	AmountPaid              float64                    `json:"amount_paid"`
	CompletedPercent        float64                    `json:"completed_percent"`
	TotalGPUs               int                        `json:"total_gpus"`
	GPUsPerContainer        int                        `json:"gpus_per_container"`
	TotalContainers         int                        `json:"total_containers"`
	HardwareID              int                        `json:"hardware_id"`
	HardwareName            string                     `json:"hardware_name"`
	BrandName               string                     `json:"brand_name"`
	Locations               []ionet.DeploymentLocation `json:"locations"`
	ComputeMinutesServed    int                        `json:"compute_minutes_served"`
	ComputeMinutesRemaining int                        `json:"compute_minutes_remaining"`
	ContainerConfig         deployContainerConfig      `json:"container_config"`
}

type deployContainerConfig struct {
	Entrypoint   []string       `json:"entrypoint"`
	EnvVariables map[string]any `json:"env_variables"`
	TrafficPort  int            `json:"traffic_port"`
	ImageURL     string         `json:"image_url"`
}

func (h *DeployManagementHandlers) handleGet(w http.ResponseWriter, r *http.Request) {
	apiKey, ok := h.gate(w, r)
	if !ok {
		return
	}
	ctx, cancel := h.callContext(r)
	defer cancel()
	detail, err := h.client(apiKey).GetDeployment(ctx, r.PathValue("id"))
	if err != nil {
		h.writeProviderError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, deployDetailResponse{
		ID: detail.ID, Status: detail.Status, CreatedAt: detail.CreatedAt,
		StartedAt: detail.StartedAt, FinishedAt: detail.FinishedAt,
		// Money 是精确 decimal；对外 DTO 仍是展示用的 float64。
		AmountPaid: detail.AmountPaid.InexactFloat64(), CompletedPercent: detail.CompletedPercent,
		TotalGPUs: detail.TotalGPUs, GPUsPerContainer: detail.GPUsPerContainer,
		TotalContainers: detail.TotalContainers, HardwareID: detail.HardwareID,
		HardwareName: detail.HardwareName, BrandName: detail.BrandName,
		Locations:               detail.Locations,
		ComputeMinutesServed:    detail.ComputeMinutesServed,
		ComputeMinutesRemaining: detail.ComputeMinutesRemaining,
		ContainerConfig: deployContainerConfig{
			Entrypoint:   detail.ContainerConfig.Entrypoint,
			EnvVariables: detail.ContainerConfig.EnvVariables,
			TrafficPort:  detail.ContainerConfig.TrafficPort,
			ImageURL:     detail.ContainerConfig.ImageURL,
		},
	})
}

type deployContainerView struct {
	DeviceID         string                 `json:"device_id"`
	ContainerID      string                 `json:"container_id"`
	Hardware         string                 `json:"hardware"`
	BrandName        string                 `json:"brand_name"`
	Status           string                 `json:"status"`
	GPUsPerContainer int                    `json:"gpus_per_container"`
	UptimePercent    int                    `json:"uptime_percent"`
	RuntimeSeconds   int64                  `json:"runtime_seconds"`
	PublicURL        string                 `json:"public_url"`
	CreatedAt        *time.Time             `json:"created_at"`
	Events           []ionet.ContainerEvent `json:"events"`
}

func (h *DeployManagementHandlers) handleContainers(w http.ResponseWriter, r *http.Request) {
	apiKey, ok := h.gate(w, r)
	if !ok {
		return
	}
	ctx, cancel := h.callContext(r)
	defer cancel()
	list, err := h.client(apiKey).ListContainers(ctx, r.PathValue("id"))
	if err != nil {
		h.writeProviderError(w, r, err)
		return
	}
	items := make([]deployContainerView, 0, len(list.Containers))
	for _, item := range list.Containers {
		items = append(items, h.containerView(item))
	}
	writeJSON(w, http.StatusOK, struct {
		Containers []deployContainerView `json:"containers"`
		Total      int                   `json:"total"`
	}{Containers: items, Total: list.Total})
}

func (h *DeployManagementHandlers) handleContainer(w http.ResponseWriter, r *http.Request) {
	apiKey, ok := h.gate(w, r)
	if !ok {
		return
	}
	containerID := strings.TrimSpace(r.PathValue("container_id"))
	if containerID == "" {
		writeProblemDetail(w, http.StatusBadRequest, problemTypeDeployInvalidRequest,
			"Invalid request", "container_id is required")
		return
	}
	ctx, cancel := h.callContext(r)
	defer cancel()
	item, err := h.client(apiKey).GetContainer(ctx, r.PathValue("id"), containerID)
	if err != nil {
		h.writeProviderError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, h.containerView(item))
}

// containerView 投影容器详情。runtime_seconds 由 created_at 推导（provider
// 不提供运行时长字段）；推导值仅用于展示，不参与计费。
func (h *DeployManagementHandlers) containerView(item ionet.Container) deployContainerView {
	var runtime int64
	if item.CreatedAt != nil {
		runtime = int64(h.now().Sub(*item.CreatedAt).Seconds())
		if runtime < 0 {
			runtime = 0
		}
	}
	events := item.Events
	if events == nil {
		events = []ionet.ContainerEvent{}
	}
	return deployContainerView{
		DeviceID: item.DeviceID, ContainerID: item.ContainerID,
		Hardware: item.Hardware, BrandName: item.BrandName, Status: item.Status,
		GPUsPerContainer: item.GPUsPerContainer, UptimePercent: item.UptimePercent,
		RuntimeSeconds: runtime, PublicURL: item.PublicURL,
		CreatedAt: item.CreatedAt, Events: events,
	}
}

func (h *DeployManagementHandlers) handleLogs(w http.ResponseWriter, r *http.Request) {
	apiKey, ok := h.gate(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	containerID := strings.TrimSpace(q.Get("container_id"))
	if containerID == "" {
		writeProblemDetail(w, http.StatusBadRequest, problemTypeDeployInvalidRequest,
			"Invalid request", "container_id is required")
		return
	}
	limit, valid := parseDeployInt(q.Get("limit"), deployLogsDefaultLimit)
	if !valid || limit < 1 {
		writeProblemDetail(w, http.StatusBadRequest, problemTypeDeployInvalidRequest,
			"Invalid request", "limit must be a positive integer")
		return
	}
	if limit > deployLogsMaxLimit {
		limit = deployLogsMaxLimit
	}
	opts := ionet.LogsOptions{
		ContainerID: containerID, Level: q.Get("level"), Stream: q.Get("stream"),
		PageToken: q.Get("page_token"), Limit: limit,
	}
	if raw := q.Get("start_time"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeProblemDetail(w, http.StatusBadRequest, problemTypeDeployInvalidRequest,
				"Invalid request", "start_time must be RFC3339")
			return
		}
		opts.StartTime = &parsed
	}
	if raw := q.Get("end_time"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeProblemDetail(w, http.StatusBadRequest, problemTypeDeployInvalidRequest,
				"Invalid request", "end_time must be RFC3339")
			return
		}
		opts.EndTime = &parsed
	}

	ctx, cancel := h.callContext(r)
	defer cancel()
	page, err := h.client(apiKey).GetLogs(ctx, r.PathValue("id"), opts)
	if err != nil {
		h.writeProviderError(w, r, err)
		return
	}
	lines := page.Lines
	if lines == nil {
		lines = []string{}
	}
	writeJSON(w, http.StatusOK, struct {
		Logs          []string `json:"logs"`
		NextPageToken string   `json:"next_page_token"`
		HasMore       bool     `json:"has_more"`
	}{Logs: lines, NextPageToken: page.NextPageToken, HasMore: page.HasMore})
}

// ---------- 更新 ----------

type updateDeploymentRequest struct {
	ImageURL            *string            `json:"image_url"`
	RegistryUsername    *string            `json:"registry_username"`
	RegistrySecret      *string            `json:"registry_secret"`
	ClearRegistrySecret bool               `json:"clear_registry_secret"`
	Entrypoint          *[]string          `json:"entrypoint"`
	Args                *[]string          `json:"args"`
	Command             *string            `json:"command"`
	TrafficPort         *int               `json:"traffic_port"`
	EnvVariables        *map[string]string `json:"env_variables"`
	SecretEnvVariables  *map[string]string `json:"secret_env_variables"`
	ClearSecretEnv      bool               `json:"clear_secret_env"`
}

func (h *DeployManagementHandlers) handleUpdate(w http.ResponseWriter, r *http.Request) {
	h.serveWriteIdempotent(w, r, func() (int, []byte) {
		return h.updateDeployment(w, r)
	})
}

func (h *DeployManagementHandlers) updateDeployment(w http.ResponseWriter, r *http.Request) (int, []byte) {
	apiKey, ok := h.gate(w, r)
	if !ok {
		return 0, nil
	}
	actor := actorID(r)
	requestID := newRelayRequestID()
	deploymentID := r.PathValue("id")

	var req updateDeploymentRequest
	if err := decodeDeployJSON(r, &req); err != nil {
		return deployProblemBytes(w, r, http.StatusBadRequest, problemTypeDeployInvalidRequest,
			"Invalid request", err.Error())
	}
	if detail := normalizeUpdateRequest(&req); detail != "" {
		return deployProblemBytes(w, r, http.StatusBadRequest, problemTypeDeployInvalidRequest,
			"Invalid request", detail)
	}

	ctx, cancel := h.callContext(r)
	defer cancel()
	provider := h.client(apiKey)

	// 前后快照只取非秘密字段（§3.20.6）。best-effort：读取失败不阻断更新。
	before, beforeOK := h.auditSnapshot(ctx, provider, deploymentID)

	action, err := provider.UpdateDeployment(ctx, deploymentID, ionet.UpdateDeploymentRequest{
		ImageURL: req.ImageURL, RegistryUsername: req.RegistryUsername,
		RegistrySecret: req.RegistrySecret, ClearRegistrySecret: req.ClearRegistrySecret,
		Entrypoint: req.Entrypoint, Args: req.Args, Command: req.Command,
		TrafficPort: req.TrafficPort, EnvVariables: req.EnvVariables,
		SecretEnvVariables: req.SecretEnvVariables, ClearSecretEnv: req.ClearSecretEnv,
	})
	if err != nil {
		status, body := h.providerErrorBody(w, r, err)
		_, code := deployErrorCode(err)
		h.writeDeployAudit(r, actor, audit.ActionDeployUpdate, status, false,
			updateAuditDetail(deploymentID, requestID, req, code), nil, nil)
		return status, body
	}
	after, _ := h.auditSnapshot(ctx, provider, deploymentID)
	if !beforeOK {
		before = nil
	}
	h.writeDeployAudit(r, actor, audit.ActionDeployUpdate, http.StatusOK, true,
		updateAuditDetail(deploymentID, requestID, req, ""), before, after)
	return jsonBody(w, r, http.StatusOK, struct {
		DeploymentID string `json:"deployment_id"`
		Status       string `json:"status"`
	}{DeploymentID: action.DeploymentID, Status: action.Status})
}

// normalizeUpdateRequest 处理「留空 = 不更新」与显式清除的互斥语义。
func normalizeUpdateRequest(req *updateDeploymentRequest) string {
	if req.RegistrySecret != nil && *req.RegistrySecret == "" {
		req.RegistrySecret = nil // 留空 = 不更新
	}
	if req.ClearRegistrySecret && req.RegistrySecret != nil {
		return "registry_secret and clear_registry_secret are mutually exclusive"
	}
	if req.SecretEnvVariables != nil && len(*req.SecretEnvVariables) == 0 {
		req.SecretEnvVariables = nil
	}
	if req.ClearSecretEnv && req.SecretEnvVariables != nil {
		return "secret_env_variables and clear_secret_env are mutually exclusive"
	}
	if req.ImageURL != nil {
		trimmed := strings.TrimSpace(*req.ImageURL)
		if trimmed == "" {
			return "image_url cannot be empty"
		}
		if parsed, err := url.Parse(trimmed); err != nil || parsed.Host == "" ||
			(parsed.Scheme != "https" && parsed.Scheme != "http") {
			return "image_url must be an absolute http(s) URL"
		}
		req.ImageURL = &trimmed
	}
	if req.TrafficPort != nil && (*req.TrafficPort < 1 || *req.TrafficPort > 65535) {
		return "traffic_port must be between 1 and 65535"
	}
	return ""
}

// updateAuditDetail 只记录字段名：秘密值/秘密环境变量键名/值都不进入 detail。
// 秘密面只留一个布尔事实 secret_changed。
func updateAuditDetail(deploymentID, requestID string, req updateDeploymentRequest, errorCode string) map[string]any {
	changed := []string{}
	if req.ImageURL != nil {
		changed = append(changed, "image_url")
	}
	if req.RegistryUsername != nil {
		changed = append(changed, "registry_username")
	}
	if req.Entrypoint != nil {
		changed = append(changed, "entrypoint")
	}
	if req.Args != nil {
		changed = append(changed, "args")
	}
	if req.Command != nil {
		changed = append(changed, "command")
	}
	if req.TrafficPort != nil {
		changed = append(changed, "traffic_port")
	}
	if req.EnvVariables != nil {
		changed = append(changed, "env_variables")
	}
	secretChanged := req.RegistrySecret != nil || req.ClearRegistrySecret ||
		req.SecretEnvVariables != nil || req.ClearSecretEnv
	detail := map[string]any{
		"deployment_id":  deploymentID,
		"changed_fields": changed,
		"secret_changed": secretChanged,
		"request_id":     requestID,
	}
	if errorCode != "" {
		detail["error_code"] = errorCode
	}
	return detail
}

// auditSnapshot 只保留非秘密字段；秘密环境变量不在 provider 详情响应里。
func (h *DeployManagementHandlers) auditSnapshot(ctx context.Context,
	provider ionet.DeploymentProvider, id string) (map[string]any, bool) {
	detail, err := provider.GetDeployment(ctx, id)
	if err != nil {
		return nil, false
	}
	return map[string]any{
		"image_url":     sanitizeImageRef(detail.ContainerConfig.ImageURL),
		"entrypoint":    detail.ContainerConfig.Entrypoint,
		"traffic_port":  detail.ContainerConfig.TrafficPort,
		"env_variables": detail.ContainerConfig.EnvVariables,
	}, true
}

// ---------- 改名 ----------

func (h *DeployManagementHandlers) handleRename(w http.ResponseWriter, r *http.Request) {
	h.serveWriteIdempotent(w, r, func() (int, []byte) {
		return h.renameDeployment(w, r)
	})
}

func (h *DeployManagementHandlers) renameDeployment(w http.ResponseWriter, r *http.Request) (int, []byte) {
	apiKey, ok := h.gate(w, r)
	if !ok {
		return 0, nil
	}
	actor := actorID(r)
	requestID := newRelayRequestID()
	deploymentID := r.PathValue("id")

	var req struct {
		Name string `json:"name"`
	}
	if err := decodeDeployJSON(r, &req); err != nil {
		return deployProblemBytes(w, r, http.StatusBadRequest, problemTypeDeployInvalidRequest,
			"Invalid request", err.Error())
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		return deployProblemBytes(w, r, http.StatusBadRequest, problemTypeDeployInvalidRequest,
			"Invalid request", "name is required")
	}

	ctx, cancel := h.callContext(r)
	defer cancel()
	provider := h.client(apiKey)

	// 服务端再次查重（§3.20.2 / F-08-57）。
	available, err := provider.CheckName(ctx, req.Name)
	if err != nil {
		status, body := h.providerErrorBody(w, r, err)
		_, code := deployErrorCode(err)
		h.writeDeployAudit(r, actor, audit.ActionDeployRename, status, false, map[string]any{
			"deployment_id": deploymentID, "new_name": req.Name,
			"request_id": requestID, "error_code": code,
		}, nil, nil)
		return status, body
	}
	if !available {
		h.writeDeployAudit(r, actor, audit.ActionDeployRename, http.StatusConflict, false, map[string]any{
			"deployment_id": deploymentID, "new_name": req.Name,
			"request_id": requestID, "error_code": "deploy_name_taken",
		}, nil, nil)
		return deployProblemBytes(w, r, http.StatusConflict, problemTypeDeployNameTaken,
			"Deployment name is already taken", "choose a different deployment name")
	}

	oldName := h.lookupDeploymentName(ctx, provider, deploymentID)
	action, err := provider.RenameDeployment(ctx, deploymentID, req.Name)
	if err != nil {
		if providerConflict(err) {
			// 改名的 409 语义是目标名称已被占用；其余冲突（状态不允许）
			// 仍走 deploy_invalid_state。
			h.writeDeployAudit(r, actor, audit.ActionDeployRename, http.StatusConflict, false, map[string]any{
				"deployment_id": deploymentID, "old_name": oldName, "new_name": req.Name,
				"request_id": requestID, "error_code": "deploy_name_taken",
			}, nil, nil)
			return deployProblemBytes(w, r, http.StatusConflict, problemTypeDeployNameTaken,
				"Deployment name is already taken", "choose a different deployment name")
		}
		status, body := h.providerErrorBody(w, r, err)
		_, code := deployErrorCode(err)
		h.writeDeployAudit(r, actor, audit.ActionDeployRename, status, false, map[string]any{
			"deployment_id": deploymentID, "old_name": oldName, "new_name": req.Name,
			"request_id": requestID, "error_code": code,
		}, nil, nil)
		return status, body
	}
	h.writeDeployAudit(r, actor, audit.ActionDeployRename, http.StatusOK, true, map[string]any{
		"deployment_id": deploymentID, "old_name": oldName, "new_name": req.Name,
		"request_id": requestID,
	}, map[string]string{"name": oldName}, map[string]string{"name": req.Name})
	return jsonBody(w, r, http.StatusOK, struct {
		DeploymentID string `json:"deployment_id"`
		Status       string `json:"status"`
		Name         string `json:"name"`
	}{DeploymentID: action.DeploymentID, Status: action.Status, Name: req.Name})
}

// lookupDeploymentName 为审计的 old_name 做 best-effort 查找：provider 详情
// 不返回名称，只能扫描列表（同一冻结上限）。查不到时留空，绝不因为审计
// 字段而阻断改名。
func (h *DeployManagementHandlers) lookupDeploymentName(ctx context.Context,
	provider ionet.DeploymentProvider, id string) string {
	for page := 1; page <= deployScanMaxPages; page++ {
		result, err := provider.ListDeployments(ctx, ionet.DeploymentListOptions{
			Page: page, PageSize: deployScanPageSize,
			SortBy: "created_at", SortOrder: "desc",
		})
		if err != nil {
			return ""
		}
		for _, item := range result.Items {
			if item.ID == id {
				return item.Name
			}
		}
		if len(result.Items) < deployScanPageSize {
			return ""
		}
	}
	return ""
}

// ---------- 续期 ----------

func (h *DeployManagementHandlers) handleExtend(w http.ResponseWriter, r *http.Request) {
	h.serveWriteIdempotent(w, r, func() (int, []byte) {
		return h.extendDeployment(w, r)
	})
}

func (h *DeployManagementHandlers) extendDeployment(w http.ResponseWriter, r *http.Request) (int, []byte) {
	apiKey, ok := h.gate(w, r)
	if !ok {
		return 0, nil
	}
	actor := actorID(r)
	requestID := newRelayRequestID()
	deploymentID := r.PathValue("id")

	var req struct {
		DurationHours int `json:"duration_hours"`
	}
	if err := decodeDeployJSON(r, &req); err != nil {
		return deployProblemBytes(w, r, http.StatusBadRequest, problemTypeDeployInvalidRequest,
			"Invalid request", err.Error())
	}
	if req.DurationHours < 1 {
		return deployProblemBytes(w, r, http.StatusBadRequest, problemTypeDeployInvalidRequest,
			"Invalid request", "duration_hours must be at least 1")
	}
	if req.DurationHours > deploybilling.MaxDeploymentDurationHours {
		return deployProblemBytes(w, r, http.StatusBadRequest, problemTypeDeployInvalidRequest,
			"Invalid request", fmt.Sprintf("duration_hours must be at most %d", deploybilling.MaxDeploymentDurationHours))
	}

	ctx, cancel := h.callContext(r)
	defer cancel()
	provider := h.client(apiKey)

	// 续期前必须取得报价（brief 语义 3）。硬件/地域/副本数来自 provider 详情。
	detail, err := provider.GetDeployment(ctx, deploymentID)
	if err != nil {
		status, body := h.providerErrorBody(w, r, err)
		_, code := deployErrorCode(err)
		h.writeDeployAudit(r, actor, audit.ActionDeployExtend, status, false,
			extendAuditDetail(deploymentID, req.DurationHours, 0, requestID, code), nil, nil)
		return status, body
	}
	locationIDs := make([]int, 0, len(detail.Locations))
	for _, location := range detail.Locations {
		locationIDs = append(locationIDs, location.ID)
	}
	replicas := detail.TotalContainers
	if replicas < 1 {
		replicas = 1
	}
	price, err := provider.EstimatePrice(ctx, ionet.PriceEstimationRequest{
		HardwareID: detail.HardwareID, LocationIDs: locationIDs,
		GPUsPerContainer: detail.GPUsPerContainer, ReplicaCount: replicas,
		DurationHours: req.DurationHours,
	})
	if err != nil {
		h.writeDeployAudit(r, actor, audit.ActionDeployExtend, http.StatusUnprocessableEntity,
			false, extendAuditDetail(deploymentID, req.DurationHours, 0, requestID,
				"deploy_price_unavailable"), nil, nil)
		return deployProblemBytes(w, r, http.StatusUnprocessableEntity, problemTypeDeployPriceUnavailable,
			"Price estimation unavailable",
			"provider price estimation failed; the deployment was not extended")
	}
	// P-21：续期同样要求可用报价，不得按 0 计。
	quote := deployQuote(price, detail.ID, int64(detail.HardwareID), int64Locations(locationIDs),
		detail.GPUsPerContainer, replicas, int64(req.DurationHours)*3600)
	if !quote.Available {
		h.writeDeployAudit(r, actor, audit.ActionDeployExtend, http.StatusUnprocessableEntity,
			false, extendAuditDetail(deploymentID, req.DurationHours, 0, requestID,
				"quote_unavailable"), nil, nil)
		return deployQuoteUnavailableBytes(w, r, "extended")
	}

	// H1：续期先预扣新段，再写 provider。
	//
	// 与创建不同，部署 ID 已知，所以可以严格按 billing-03 §5.9 逐项约束 1
	// 「预扣不建立则 provider 写不发起」执行；provider 续期失败时释放新段
	// （段从未运行 → released 事实，成本 0）。
	var extendSegmentID string
	if h.billing != nil {
		est := deploybilling.EstimateForExtend(quote, detail.GPUsPerContainer*replicas, h.now())
		est.DeploymentID = deploymentID
		est.IdempotencyKey = deployExtendIdempotencyKey(deploymentID, req.DurationHours)
		ledgerCtx, cancelLedger := h.ledgerContext(r)
		seg, _, holdErr := h.billing.Hold(ledgerCtx, est)
		cancelLedger()
		if holdErr != nil {
			h.log.Error("deploy: site cost hold failed on extend",
				"deployment_id", deploymentID, "err", holdErr)
			h.writeDeployAudit(r, actor, audit.ActionDeployExtend, http.StatusBadGateway,
				false, extendAuditDetail(deploymentID, req.DurationHours, 0, requestID,
					"deploy_billing_hold_failed"), nil, nil)
			return deployProblemBytes(w, r, http.StatusBadGateway, problemTypeDeployBillingFailed,
				"Deployment billing unavailable",
				"site cost reservation failed; the deployment was not extended")
		}
		extendSegmentID = seg.SegmentID
	}

	action, err := provider.ExtendDeployment(ctx, deploymentID, req.DurationHours)
	if err != nil {
		// provider 未续期成功 → 新段从未运行，释放全部预扣（不再 Hold）。
		if h.billing != nil && extendSegmentID != "" {
			ledgerCtx, cancelLedger := h.ledgerContext(r)
			if releaseErr := h.billing.Release(ledgerCtx, extendSegmentID, "extend_failed"); releaseErr != nil {
				h.log.Warn("deploy: release extend segment failed",
					"segment_id", extendSegmentID, "err", releaseErr)
			}
			cancelLedger()
		}
		status, body := h.providerErrorBody(w, r, err)
		_, code := deployErrorCode(err)
		h.writeDeployAudit(r, actor, audit.ActionDeployExtend, status, false,
			extendAuditDetail(deploymentID, req.DurationHours, price.EstimatedCost.InexactFloat64(), requestID, code), nil, nil)
		return status, body
	}

	// 续期生效：旧段在生效点结算关闭（billing-03 §5.9.2），新段从这一刻
	// 起算。排除刚预扣的新段——它不是「被续期关闭的旧段」。
	if h.billing != nil {
		ledgerCtx, cancelLedger := h.ledgerContext(r)
		_, settleErr := h.billing.SettleTerminated(ledgerCtx, deploybilling.TerminationObservation{
			DeploymentID:      deploymentID,
			ProviderStartedAt: detail.StartedAt,
			FinishedAt:        deployTimePtr(h.now().UTC()),
			ComputeSeconds:    int64(detail.ComputeMinutesServed) * 60,
			ExcludeSegmentIDs: []string{extendSegmentID},
		})
		cancelLedger()
		if settleErr != nil {
			// 续期已生效，响应不能失败：旧段留给 N16 对账收敛。
			h.log.Warn("deploy: settle previous segment after extend failed",
				"deployment_id", deploymentID, "err", settleErr)
		}
	}

	h.writeDeployAudit(r, actor, audit.ActionDeployExtend, http.StatusAccepted, true,
		extendAuditDetail(deploymentID, req.DurationHours, price.EstimatedCost.InexactFloat64(), requestID, ""), nil, nil)
	return jsonBody(w, r, http.StatusAccepted, struct {
		DeploymentID string `json:"deployment_id"`
		Status       string `json:"status"`
	}{DeploymentID: action.DeploymentID, Status: action.Status})
}

func extendAuditDetail(deploymentID string, hours int, estimated float64, requestID, errorCode string) map[string]any {
	detail := map[string]any{
		"deployment_id":      deploymentID,
		"added_hours":        hours,
		"estimated_cost_usd": estimated,
		"request_id":         requestID,
	}
	if errorCode != "" {
		detail["error_code"] = errorCode
	}
	return detail
}

// ---------- 终止 ----------

func (h *DeployManagementHandlers) handleDelete(w http.ResponseWriter, r *http.Request) {
	h.serveWriteIdempotent(w, r, func() (int, []byte) {
		return h.deleteDeployment(w, r)
	})
}

func (h *DeployManagementHandlers) deleteDeployment(w http.ResponseWriter, r *http.Request) (int, []byte) {
	apiKey, ok := h.gate(w, r)
	if !ok {
		return 0, nil
	}
	actor := actorID(r)
	requestID := newRelayRequestID()
	deploymentID := r.PathValue("id")

	ctx, cancel := h.callContext(r)
	defer cancel()
	provider := h.client(apiKey)
	name := h.lookupDeploymentName(ctx, provider, deploymentID)

	action, err := provider.DeleteDeployment(ctx, deploymentID)
	if err != nil {
		status, body := h.providerErrorBody(w, r, err)
		_, code := deployErrorCode(err)
		h.writeDeployAudit(r, actor, audit.ActionDeployDelete, status, false, map[string]any{
			"deployment_id": deploymentID, "name": name,
			"request_id": requestID, "error_code": code,
		}, nil, nil)
		return status, body
	}
	h.writeDeployAudit(r, actor, audit.ActionDeployDelete, http.StatusAccepted, true, map[string]any{
		"deployment_id": deploymentID, "name": name, "request_id": requestID,
	}, nil, nil)
	return jsonBody(w, r, http.StatusAccepted, struct {
		DeploymentID string `json:"deployment_id"`
		Status       string `json:"status"`
	}{DeploymentID: action.DeploymentID, Status: action.Status})
}

// ---------- 小工具 ----------

// providerConflict 判断 provider 是否返回 409。create/rename 的调用点把
// 409 解释为「名称已占用」；其他写操作仍由 classify 映射为 invalid_state。
func providerConflict(err error) bool {
	var apiErr *ionet.APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict
}

// deployErrorCode 返回 (problemType, 业务码)，用于审计 error_code。
func deployErrorCode(err error) (string, string) {
	status, problemType, _, _ := classifyDeployProviderError(err)
	switch problemType {
	case problemTypeDeploySecretInvalid:
		return problemType, "deploy_secret_invalid"
	case problemTypeDeployNotFound:
		return problemType, "deploy_not_found"
	case problemTypeDeployInvalidState:
		return problemType, "deploy_invalid_state"
	case problemTypeDeployRateLimited:
		return problemType, "deploy_rate_limited"
	case problemTypeDeployProviderTimeout:
		return problemType, "deploy_provider_timeout"
	case problemTypeDeployInvalidRequest:
		return problemType, "deploy_invalid_request"
	default:
		_ = status
		return problemType, "deploy_provider_error"
	}
}

// problemBody 生成 RFC 7807 响应体（不直接写 w）：幂等包装需要先拿到字节，
// 才能在 Complete 固化与写出之间保持「只写一次」。
func deployProblemBytes(_ http.ResponseWriter, _ *http.Request, status int, problemType, title, detail string) (int, []byte) {
	problem := dto.Problem{Type: problemType, Title: title, Status: status}
	if detail != "" {
		problem.Detail = &detail
	}
	body, err := json.Marshal(problem)
	if err != nil {
		return http.StatusInternalServerError, nil
	}
	return status, body
}

// providerErrorBody 是 problemBody 的 provider 错误版本；Retry-After 通过
// 响应头传递（头在 WriteHeader 之前设置仍然有效）。
func (h *DeployManagementHandlers) providerErrorBody(w http.ResponseWriter, r *http.Request, err error) (int, []byte) {
	status, problemType, title, detail := classifyDeployProviderError(err)
	var apiErr *ionet.APIError
	if errors.As(err, &apiErr) && apiErr.RetryAfter > 0 {
		w.Header().Set("Retry-After", retrySeconds(apiErr.RetryAfter))
	}
	if status >= http.StatusInternalServerError {
		h.log.Warn("deploy: provider request failed", "status", status, "err", err)
	}
	return deployProblemBytes(w, r, status, problemType, title, detail)
}

// jsonBody 编码响应体并返回字节（由幂等包装统一写出）。
func jsonBody(_ http.ResponseWriter, _ *http.Request, status int, payload any) (int, []byte) {
	body, err := json.Marshal(payload)
	if err != nil {
		return deployProblemBytes(nil, nil, http.StatusInternalServerError, authProblemInternal,
			"Internal Server Error", "")
	}
	return status, body
}

// decodeDeployJSON 读取并解码请求体（1 MiB 上限）。
func decodeDeployJSON(r *http.Request, out any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxAuthBodyBytes+1))
	if err != nil {
		return errors.New("unable to read request body")
	}
	if len(body) > maxAuthBodyBytes {
		return errors.New("request body is too large")
	}
	if len(body) == 0 {
		return errors.New("request body is required")
	}
	if err := json.Unmarshal(body, out); err != nil {
		return errors.New("invalid JSON body")
	}
	return nil
}

// parseDeployInt 解析正整数；空串返回默认值。非法输入返回 valid=false。
func parseDeployInt(raw string, def int) (int, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return def, true
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, false
	}
	return value, true
}

func deployTimePtr(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	return &value
}
