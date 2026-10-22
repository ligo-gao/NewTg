// Package httpapi 的任务数据面端点（M6d Task 6a）。
//
// 端点面（relay-06 §10.2 / §10.4）：
//
//	POST /v1/tasks/{key}                    提交任务（Bearer 令牌）
//	GET  /v1/tasks/{key}?task_id=           查询任务（Bearer 令牌）
//	GET  /v1/tasks/{key}/artifacts?task_id= 列产物（Bearer 令牌）
//	GET|HEAD /v1/tasks/{key}/artifacts/{artifact_key}/content?task_id=&access=
//	                                        取产物内容（HMAC 能力 URL，无需 Bearer）
//	GET  /v1/responses/{id}                 Responses 产物回放（resp_<x> → task_<x>，T3 / P-02）
//	POST /v1/videos                          OpenAI Videos 提交 / 查询 / 内容（T4 / P-04）
//	GET  /v1/videos/{id}、/v1/videos/{id}/content
//	POST /v1/batches                         OpenAI Batches（T4 / P-04；父批次只聚合）
//	GET  /v1/batches/{id}、POST /v1/batches/{id}/cancel
//	GET  /api/task/self                     本人任务台账（会话）
//	GET  /api/task                          管理员台账（logs:read）
//
// /v1 端点复用 RelayAuth 中间件（由 RelayHandler 注入同一个 authMiddleware），
// /api 端点挂在管理面受保护链上。计费只经 contract.BillingSession 接缝：
// 本文件不实现定价/结算，T6b 接管轮询与终态结算。
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/billing"
	"newtg/gateway/internal/billing/pricing"
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/plugin"
	"newtg/gateway/internal/rbac"
	"newtg/gateway/internal/relay"
	"newtg/gateway/internal/relay/billingseam"
	"newtg/gateway/internal/sensitive"
	"newtg/gateway/internal/tasks"
)

const (
	// maxTaskSubmitBytes 是任务提交请求体上限。任务请求通常只含 prompt/参数，
	// 4MiB 足以覆盖长提示与参考图 URL 列表；文件上传走后续任务，不在本任务。
	maxTaskSubmitBytes = 4 << 20

	// 台账分页默认值与 channels/usage-logs 同口径。
	tasksDefaultPageSize = 20
	tasksMaxPageSize     = 100

	// 管理面问题类型：任务台账的 403/500 用固定 type，便于前端按类别处理。
	problemTypeTasksForbidden = "https://newtg.dev/problems/forbidden"
	problemTypeTasksDatabase  = "https://newtg.dev/problems/database_error"
)

// TaskRepository 是任务数据面的存储接缝（生产 *tasks.Repo）。
type TaskRepository interface {
	Insert(ctx context.Context, t *tasks.Task) error
	GetByPublicID(ctx context.Context, userID int64, publicID, pluginKey string) (*tasks.Task, error)
	// GetByPublicIDAny 是能力式产物读取（global access ticket 即授权）。
	GetByPublicIDAny(ctx context.Context, publicID string) (*tasks.Task, error)
	ListByUser(ctx context.Context, userID int64, f tasks.ListFilter) ([]tasks.Task, int, error)
	ListAdmin(ctx context.Context, f tasks.ListFilter) ([]tasks.Task, int, error)
}

var _ TaskRepository = (*tasks.Repo)(nil)

// TaskChannelSource 是按插件绑定选渠道的读取面（生产 *tasks.Repo）。
type TaskChannelSource interface {
	SelectChannel(ctx context.Context, pluginKey, model, group string) (*tasks.ChannelTarget, error)
	// GetChannel 按 ID 读取（产物内容代理用；不按 status 过滤，见 Repo 注释）。
	GetChannel(ctx context.Context, id int64) (*tasks.ChannelTarget, error)
}

var _ TaskChannelSource = (*tasks.Repo)(nil)

// TasksHandlerConfig 是任务端点的装配输入。
type TasksHandlerConfig struct {
	Store    TaskRepository
	Channels TaskChannelSource
	Plugins  *plugin.Manager
	// Batches 是批次聚合的存储面（T4 / P-04）。nil 时若 Store 同时实现了
	// BatchRepository 就复用它（生产 tasks.Repo 两者同源）；两者都缺失时
	// /v1/batches* 返回 503，绝不把「没有存储」伪装成空批次。
	Batches BatchRepository
	// Sealer 解开渠道凭据（GC-8）。必填：没有它就只能拒绝提交，绝不回落明文。
	Sealer *SecretSealer
	// Tokens 是 relay.BuildRelayContext 回源令牌分组的接缝（Bearer 路径）。
	Tokens auth.TokenLookup
	// Billing 为每次提交构造计费会话；nil 时使用 billingseam.NewStub（测试）。
	Billing func(*contract.RelayContext) contract.BillingSession
	// Client 是出站客户端；nil 时构造禁重定向的默认客户端。
	Client *http.Client
	// ArtifactSigner 签发/校验产物能力 URL（M6d T6b）。必填：没有它就无法
	// 安全地授予「持票取产物」的能力，静默降级成无鉴权会直接泄露产物。
	ArtifactSigner *tasks.ArtifactSigner
	// PublicBaseURL 是能力 URL 的基址（可空 → 相对路径）。
	PublicBaseURL string
	// AllowPrivateUpstream 只允许测试注入 httptest（127.0.0.1）时置 true。
	// 生产装配恒为 false；它只跳过字面量私网地址检查，不关闭白名单/重定向/大小限制。
	AllowPrivateUpstream bool
	// BillingSetting 读取任务计价表达式配置（T5）。nil 表示未配置插件覆盖，
	// 解析链只回退到模型级表达式。
	BillingSetting func() pricing.BillingSetting
	// ModelExpr 读取**映射模型**的 billing_mode/表达式（T5 解析链第 4 级）。
	// clientModel 的第 3 级直接用提交期冻结的 rc.PriceSnapshot，不必再查。
	// 查询失败按「没有第 4 级」处理（记录日志），不阻断任务提交。
	ModelExpr func(ctx context.Context, model string) (mode, expr string, ok bool, err error)
	// RateLimiter / Config 是 W7/M5-b 的准入限流接缝与阈值来源（R-M5-07：
	// 只有**提交**面计入，查询/轮询/产物面不调用）。RateLimiter 为 nil 时
	// 跳过（仅测试/无 Redis 的降级装配）。
	RateLimiter RelayRateLimiter
	Config      ConfigReader
}

// TasksHandler 持有任务端点依赖，可并发使用。
type TasksHandler struct {
	store        TaskRepository
	batches      BatchRepository
	channels     TaskChannelSource
	plugins      *plugin.Manager
	sealer       *SecretSealer
	tokens       auth.TokenLookup
	billing      func(*contract.RelayContext) contract.BillingSession
	client       *http.Client
	signer       *tasks.ArtifactSigner
	publicBase   string
	allowPrivate bool
	// billingSetting/modelExpr 是 T5 任务计价表达式的配置读取缝。
	billingSetting func() pricing.BillingSetting
	modelExpr      func(ctx context.Context, model string) (mode, expr string, ok bool, err error)
	// rateLimiter/config 是 W7/M5-b 的提交面准入限流缝（R-M5-07）。
	rateLimiter RelayRateLimiter
	config      ConfigReader
	// sensitive 是 sec.sensitive.* 的解析缓存（F-08-18 / W14）；零值可用。
	sensitive *sensitive.Cache
}

// NewTasksHandler 构造 handler。Store/Channels/Plugins/Sealer/Signer 是硬依赖：
// 缺任一都不存在「安全降级」，只会把配置错误伪装成用户可见的失败。
func NewTasksHandler(cfg TasksHandlerConfig) *TasksHandler {
	if cfg.Store == nil || cfg.Channels == nil || cfg.Plugins == nil || cfg.Sealer == nil || cfg.ArtifactSigner == nil {
		panic("httpapi: NewTasksHandler requires store, channel source, plugin manager, sealer and artifact signer")
	}
	billing := cfg.Billing
	if billing == nil {
		billing = func(*contract.RelayContext) contract.BillingSession { return billingseam.NewStub() }
	}
	client := cfg.Client
	if client == nil {
		client = tasks.NewOutboundClient()
	}
	// 批次存储默认复用同一仓储实例：生产装配只注入 Store（tasks.Repo 同时
	// 实现两个接口），不要为了批次再建第二套连接面/仓储（R-07）。
	batches := cfg.Batches
	if batches == nil {
		if shared, ok := cfg.Store.(BatchRepository); ok {
			batches = shared
		}
	}
	return &TasksHandler{
		store:          cfg.Store,
		batches:        batches,
		channels:       cfg.Channels,
		plugins:        cfg.Plugins,
		sealer:         cfg.Sealer,
		tokens:         cfg.Tokens,
		billing:        billing,
		client:         client,
		signer:         cfg.ArtifactSigner,
		publicBase:     strings.TrimRight(strings.TrimSpace(cfg.PublicBaseURL), "/"),
		allowPrivate:   cfg.AllowPrivateUpstream,
		billingSetting: cfg.BillingSetting,
		modelExpr:      cfg.ModelExpr,
		rateLimiter:    cfg.RateLimiter,
		config:         cfg.Config,
		sensitive:      &sensitive.Cache{},
	}
}

// RegisterRelay 把 /v1/tasks* 挂到 mux，复用中继的 Bearer 鉴权链。
//
// content 端点**刻意不挂 Bearer 中间件**：它设计为能力式 URL（relay-06
// §10.5），客户端用它下载产物时通常没有令牌上下文（浏览器直链/播放器）。
// 授权完全由 access 票据承担；票据绑定 pluginKey/taskID/artifactKey。
func (h *TasksHandler) RegisterRelay(mux *http.ServeMux, authMiddleware func(http.Handler) http.Handler) {
	mux.Handle("POST /v1/tasks/{key}", authMiddleware(http.HandlerFunc(h.handleSubmit)))
	mux.Handle("GET /v1/tasks/{key}", authMiddleware(http.HandlerFunc(h.handleGet)))
	mux.Handle("GET /v1/tasks/{key}/artifacts", authMiddleware(http.HandlerFunc(h.handleArtifacts)))
	mux.HandleFunc("GET /v1/tasks/{key}/artifacts/{artifact_key}/content", h.handleContent)
	// T3 / P-02：Responses 产物回放。与 POST /v1/responses 同一条 RelayAuth 链，
	// 但在任务数据面上注册：响应体由 task.data 的产物投影装配，中继面既没有
	// 任务存储也没有产物签名器，放过去只会让两份实现分叉。
	mux.Handle("GET /v1/responses/{id}", authMiddleware(http.HandlerFunc(h.handleResponsesGet)))
	// T4 / P-04：/v1/videos 与 /v1/batches 都是 tasks 数据面的**纯内建**薄封装
	// （R-10：只登记 builtinRouteShapes，处理器不查插件路由索引）。
	mux.Handle("POST /v1/videos", authMiddleware(http.HandlerFunc(h.handleVideosSubmit)))
	mux.Handle("GET /v1/videos/{id}", authMiddleware(http.HandlerFunc(h.handleVideosGet)))
	mux.Handle("GET /v1/videos/{id}/content", authMiddleware(http.HandlerFunc(h.handleVideosContent)))
	mux.Handle("POST /v1/batches", authMiddleware(http.HandlerFunc(h.handleBatchesSubmit)))
	mux.Handle("GET /v1/batches/{id}", authMiddleware(http.HandlerFunc(h.handleBatchesGet)))
	mux.Handle("POST /v1/batches/{id}/cancel", authMiddleware(http.HandlerFunc(h.handleBatchesCancel)))
}

// RegisterAdmin 把台账端点挂到管理面受保护链。
func (h *TasksHandler) RegisterAdmin(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/task/self", h.handleSelfList)
	mux.HandleFunc("GET /api/task", h.handleAdminList)
}

// ---------- /v1：提交 ----------

type taskSubmitRequest struct {
	Model string `json:"model"`
}

func (h *TasksHandler) handleSubmit(w http.ResponseWriter, r *http.Request) {
	requestID := newRelayRequestID()
	principal, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		writeRelayError(w, http.StatusUnauthorized, requestID, "", "invalid token")
		return
	}
	key := strings.TrimSpace(r.PathValue("key"))
	if key == "" || len(key) > plugin.MaxKeyLen {
		writeRelayError(w, http.StatusNotFound, requestID, "model_not_found", "unknown task plugin")
		return
	}

	// 插件准入（brief 步骤 2）：只接受 registered / disabled_fallback。
	// 未注册与已禁用用不同状态码，让调用方能区分「没有这个插件」与
	// 「插件被管理员关闭」；两者都不产生任何计费或持久化副作用。
	snapshot := h.plugins.Snapshot()
	entry := snapshot.Lookup(key)
	if entry.Plugin == nil {
		if entry.Status == plugin.StatusNotRegistered {
			writeRelayError(w, http.StatusNotFound, requestID, "model_not_found", "unknown task plugin")
			return
		}
		writeRelayError(w, http.StatusConflict, requestID, "plugin_unavailable", "task plugin is not available")
		return
	}
	compiled := entry.Plugin

	// 请求体：有界读取 + JSON 对象。model 必填且必须是非空字符串。
	r.Body = http.MaxBytesReader(w, r.Body, maxTaskSubmitBytes)
	var body map[string]any
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&body); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeRelayError(w, http.StatusRequestEntityTooLarge, requestID, "payload_too_large", "request body is too large")
			return
		}
		writeRelayError(w, http.StatusBadRequest, requestID, "invalid_request_error", "request body must be a JSON object")
		return
	}
	model := strings.TrimSpace(toStringValue(body["model"]))
	if model == "" {
		writeRelayError(w, http.StatusBadRequest, requestID, "invalid_request_error", "model is required")
		return
	}
	// action 是族内动作标识（解释权归插件）；列宽 40，超长必须在落库前拒绝，
	// 否则会变成 500 的 CHECK 违约而不是可读的 400。
	action := strings.TrimSpace(toStringValue(body["action"]))
	if len(action) > 40 {
		writeRelayError(w, http.StatusBadRequest, requestID, "invalid_request_error", "action is too long")
		return
	}
	if !tokenModelAllowed(principal.ModelLimits, model) {
		writeRelayError(w, http.StatusForbidden, requestID, "distributor_token_model_forbidden",
			"this token is not allowed to access the requested model")
		return
	}
	taskDuration, ok := taskDurationSeconds(body["seconds"])
	if !ok {
		writeRelayError(w, http.StatusBadRequest, requestID, "invalid_request_error",
			"seconds must be a positive number no greater than the task duration limit")
		return
	}

	// 复用中继上下文组装：RequestID / UserID / TokenID / 分组来自同一条
	// 鉴权链，pin 的「非管理员拒绝」也在这里执行（R-P2a）。
	//
	// 错误映射必须与 /v1 中继面同口径（W7/M2 复核 Minor-1）：R-61 的
	// auto_groups_not_enabled、R-P2a 的 pin 403 不能被压成 401，否则同一
	// 令牌在不同端点会得到互相矛盾的鉴权语义。令牌类错误仍按 §3.6 返回
	// 401（mapRelayContextError 对 ErrTokenInvalid/ErrPrincipalMissing 不传 code）。
	rc, err := relay.BuildRelayContext(r.Context(), principal, r, h.tokens)
	if err != nil {
		status, code, message := mapRelayContextError(err)
		writeRelayError(w, status, requestID, code, message)
		return
	}
	// 计费幂等键与响应里的 requestId 对齐：客户端报障时同一个 ID 能同时定位
	// 失败响应与计费会话（BuildRelayContext 生成的是另一个随机值）。
	if requestID != "" {
		rc.RequestID = requestID
	}
	relay.WithRequest(rc, &contract.Request{Model: model}, "openai", r.URL.Path)

	// F-08-18（W14）：任务/视频提交体里的用户文本在渠道选择、PreConsume 与
	// 出站之前过滤。视频入口（/v1/videos）先解析 model 再委托到本函数，因此
	// 两个端点只有这一处实现，不会出现「一个端点过滤、另一个漏掉」。
	if h.rejectSensitiveTaskBody(w, requestID, int64(rc.UserID), body) {
		return
	}

	// W7/M5-b 准入限流（R-M5-07 / R-M5-04）：任务**提交**是本 handler 的唯一
	// 限流点（videos / batches / 原生路由都汇合到这里）。位置在渠道选择与
	// 预扣之前：被拒绝的提交不建任务行、不预扣、不触达上游。
	//
	// 与 chat/messages 的细微差异（登记）：令牌模型白名单在本函数开头、
	// BuildRelayContext 之前执行，因此白名单 403 不消耗配额；chat/messages
	// 的白名单在限流之后，白名单 403 会消耗配额。这是把限流放在
	// BuildRelayContext 之后（rc.UsingGroup 可用）的必然结果，不是两套语义。
	if rejection := checkRelayRateLimit(r.Context(), h.rateLimiter, h.config, rc, principal, clientIP(r)); rejection != nil {
		writeRelayRateLimitError(w, rc.RequestID, rejection)
		return
	}

	// 渠道候选（brief 步骤 3）：status=1 + 绑定插件 key + 支持模型，
	// 按 priority/weight 取一个。无候选 → 404，发生在任何计费之前。
	channel, err := h.channels.SelectChannel(r.Context(), key, model, rc.UsingGroup)
	if err != nil {
		if errors.Is(err, tasks.ErrChannelNotFound) {
			writeRelayError(w, http.StatusNotFound, requestID, "model_not_found",
				"no task channel is available for the requested model")
			return
		}
		writeRelayError(w, http.StatusInternalServerError, requestID, "database_error",
			"failed to select a task channel")
		return
	}

	// 渠道凭据解密（GC-8）：明文只活在本次请求的内存里，绝不落库/日志/响应。
	apiKey, err := h.decryptChannelCredential(channel.Credentials)
	if err != nil {
		writeRelayError(w, http.StatusInternalServerError, requestID, "channel_credential_invalid",
			"task channel credential is not usable")
		return
	}
	if strings.TrimSpace(channel.BaseURL) == "" {
		// 基址缺失是渠道配置错误，不是用户参数错误；不退回插件 meta.baseUrl——
		// 那会让「渠道配置漏填」静默走一个可能不受管理员控制的地址。
		writeRelayError(w, http.StatusInternalServerError, requestID, "channel_misconfigured",
			"task channel base url is not configured")
		return
	}

	// 计费预扣（brief 步骤 5）：估算进入 RelayContext，PreConsume 失败即 402
	// 且不落任务行。定价/快照解析由计费引擎负责，本端点不读价格字段。
	estimate := &contract.QuotaEstimate{
		Model:        model,
		Group:        rc.UsingGroup,
		TaskDuration: taskDuration,
	}
	relay.WithQuotaEstimate(rc, estimate)
	session := h.billing(rc)
	if session == nil {
		session = billingseam.NewStub()
	}
	if err := session.PreConsume(*estimate); err != nil {
		// M1：定价键缺失/表达式错误不是余额不足。只有计费引擎明确判定额度
		// 不足才映射 402；其余价格/配置错误映射 400 model_price_error，避免
		// 客户端把「管理员没配价」误读成「充值就能用」。
		if errors.Is(err, billing.ErrInsufficientQuota) {
			writeRelayError(w, http.StatusPaymentRequired, requestID, "insufficient_quota",
				"insufficient quota for this task")
			return
		}
		writeRelayError(w, http.StatusBadRequest, requestID, "model_price_error",
			"task pricing is not available for the requested model")
		return
	}
	committed := false
	// settled 表示资金已按本请求的终态收敛（退款或结算成功）。它只用于
	// defer 的「是否还需要退款」，与任务的 state 不是同一概念。
	settled := false
	defer func() {
		if !committed && !settled {
			// 预扣成功后的任何失败都必须全额退款；Refund 幂等（契约 §6.1）。
			if err := session.Refund(); err != nil {
				slog.Warn("tasks: refund after failed submit did not complete",
					"request_id", rc.RequestID, "error", err)
			}
		}
	}()

	// 任务计价表达式（T5 / P-08）：在预扣之后、出站提交之前解析并**冻结**。
	//   - 放在预扣之后：rc.PriceSnapshot 已由 PreConsume 写入（模型级解析链
	//     第 3 级的输入），避免重复解析定价；
	//   - 放在出站之前：插件覆盖键为空/非法是配置错误，此时还没有产生上游
	//     任务，直接 400 由 defer 全额退款，比落库后再失败更安全。
	// 没有任何表达式时 frozen 为 nil，结算沿用「按预扣额结清」。
	frozenInfo, freezeErr := h.freezeTaskBilling(r.Context(), rc, key, model,
		upstreamModelName(channel, model), compiled.Manifest())
	if freezeErr != nil {
		slog.Warn("tasks: task billing expression is not usable",
			"request_id", rc.RequestID, "plugin", key, "model", model, "error", freezeErr)
		writeRelayError(w, http.StatusBadRequest, requestID, "model_price_error",
			"task pricing expression is invalid")
		return
	}
	frozen := frozenInfo.snapshot

	// 出站提交（brief 步骤 6/7）：插件构建 → 宿主连接 → 插件解析。
	result, err := tasks.Submit(r.Context(), h.client, tasks.SubmitInput{
		Plugin:       compiled,
		Channel:      *channel,
		Model:        model,
		Body:         body,
		BaseURL:      channel.BaseURL,
		APIKey:       apiKey,
		AllowPrivate: h.allowPrivate,
	})
	if err != nil {
		// 提交失败不落任务行（brief 步骤 8 的冻结选择）：没有 upstream_task_id
		// 的行既不能轮询也不能结算，只会成为孤儿。失败事实由 usage_logs 与
		// 退款流水承载。
		writeRelayError(w, http.StatusBadGateway, requestID, "upstream_error",
			"task submission failed upstream")
		return
	}

	publicID, err := tasks.NewPublicTaskID()
	if err != nil {
		writeRelayError(w, http.StatusInternalServerError, requestID, "internal_error",
			"failed to allocate a task id")
		return
	}

	// 双屏障的「向上差额预留」：当前没有已定价的目标额度，预留增量为 0
	//（幂等 no-op），持久化的 quota 就是本次预扣量。带定价的差额上浮归
	// T6b；**错误不能吞**：非零预留适配后这里的失败必须可见（M4）。
	if err := session.Reserve(0); err != nil {
		slog.Warn("tasks: reserve after submit failed", "request_id", rc.RequestID, "error", err)
		writeRelayError(w, http.StatusInternalServerError, requestID, "billing_error",
			"failed to reserve quota for the task")
		return
	}
	quota := session.Reserved()

	// 即时终态的资金收敛（HIGH-2 / T5）：
	//   - FAILURE：设计 §9.5「即时失败 settle 为 0」→ 全额退款，quota 落 0；
	//   - SUCCESS：有冻结表达式则求值后差额结算；无表达式则按预扣额结清。
	//   求值/资金失败都保留预扣并置 settlement_pending（P-07）。
	refundPending := false
	settlementPending := false
	switch {
	case result.State == tasks.StateFailure:
		if err := session.Refund(); err != nil {
			// 退款失败不能伪装成已退：持久化 refund_pending 供补偿扫描发现
			//（M3），并保留预扣金额，避免账实不符。
			refundPending = true
			slog.Warn("tasks: refund for immediate failure did not complete",
				"request_id", rc.RequestID, "error", err)
		} else {
			settled = true
			quota = 0
		}
	case result.State == tasks.StateSuccess:
		// 即时 SUCCESS 的两种口径（T5）：
		//   - 有冻结表达式：求值真实成本后差额结算；
		//   - 无表达式：沿用 T5 之前的「按预扣额终结」（空 fact，差额 0）。
		//
		// 顺序必须是 Detach → Terminate：Terminate 走独立的 ByRequestID
		// 会话，不会停掉本请求的心跳 goroutine。
		done, finalQuota := h.settleImmediateSuccess(session, rc.RequestID, frozen,
			result.Data, compiled.Manifest(), quota)
		if done {
			settled = true
			quota = finalQuota
		} else {
			settlementPending = true
		}
	}

	now := time.Now().UTC()
	fetchMode := "poll"
	if result.Immediate {
		// 即时终态不再需要轮询取回；fetch_mode 记录事实而不是默认值（LOW）。
		fetchMode = "inline"
	}
	task := &tasks.Task{
		PublicTaskID:     publicID,
		UpstreamTaskID:   result.UpstreamTaskID,
		Family:           "plugin",
		Action:           action,
		PluginKey:        key,
		PluginGeneration: tasks.PluginGeneration(compiled),
		FetchMode:        fetchMode,
		LockedChannelID:  0,
		OriginTaskIDs:    []string{},
		State:            result.State,
		RawState:         result.RawState,
		ProgressPercent:  result.Progress,
		UserID:           principal.UserID,
		Group:            rc.UsingGroup,
		ChannelID:        channel.ID,
		Quota:            quota,
		FailReason:       "",
		SubmitTime:       &now,
		Properties: map[string]any{
			"OriginModelName":   model,
			"UpstreamModelName": upstreamModelName(channel, model),
			"Input":             body,
		},
		PrivateData: map[string]any{
			// 执行溯源（设计 §7）+ 资金来源。request_id 是 T6b 在进程外
			// 结算/退款的唯一锚点；凭据/明文 key 绝不进入此列。
			"request_id":     rc.RequestID,
			"request_path":   rc.RequestPath,
			"billing_source": "wallet",
			"plugin": map[string]any{
				"key":         key,
				"name":        compiled.Manifest().Name,
				"version":     entry.Version,
				"api_version": compiled.Manifest().APIVersion,
				"generation":  tasks.PluginGeneration(compiled),
			},
		},
		Data: result.Data,
	}
	if principal.TokenID != nil {
		task.PrivateData["token_id"] = *principal.TokenID
	}
	if frozen != nil {
		// T5 / P-08：计价快照在提交期冻结，结算（含补偿扫描/重启后的
		// worker）只读这一份，不重新解析配置。只进 private_data：台账投影
		// 永远不返回（设计 §7.1）。
		task.PrivateData[tasks.PrivateTieredSnapshot] = frozen
	}
	if frozenInfo.skip != nil {
		// 复核 I2：模型级 tiered 表达式不是任务表达式（不引用 u()）而被
		// 显式跳过。落标记 + 原因，避免与「从未配置」不可区分。
		task.PrivateData[tasks.PrivateTaskExprSkip] = frozenInfo.skip
	}
	if refundPending {
		task.PrivateData["refund_pending"] = true
	}
	if settlementPending {
		task.PrivateData["settlement_pending"] = true
	}
	if settled {
		// 即时终态已封账（inline success/failure）：与轮询终态同一审计口径。
		task.PrivateData["billing_final"] = true
	}
	if result.Immediate {
		// 即时终态：不再轮询，定格完成时间。
		task.FinishTime = &now
	}

	if err := h.store.Insert(r.Context(), task); err != nil {
		writeRelayError(w, http.StatusInternalServerError, requestID, "internal_error",
			"failed to persist the task")
		return
	}
	committed = true

	// 异步交接（HIGH-1）：任务行落库成功后，凡仍有预扣 hold 的路径都必须
	// 停止心跳并移除在飞租约，否则 goroutine 泄漏、worker 永远看不到悬挂，
	// 重启后还会被 RecoverAbandoned 当成废弃请求全额退款（免费服务）。
	// 已 refund/terminate 的路径没有 hold，无需交接。
	if !settled {
		if handoff, ok := session.(contract.BillingHandoff); ok {
			if err := handoff.Detach(rc.RequestID); err != nil {
				// 任务行已落库，无法回滚；心跳未停会让 worker 延迟兜底（安全方向）。
				// 记录告警供运维发现，任务行本身仍有 request_id 可人工收敛。
				slog.Warn("tasks: billing detach failed after task insert",
					"request_id", rc.RequestID, "error", err)
			}
		}
	}

	writeJSON(w, http.StatusAccepted, taskSubmitResponse{
		PublicTaskID:   task.PublicTaskID,
		State:          task.State,
		UpstreamTaskID: task.UpstreamTaskID,
	})
}

type taskSubmitResponse struct {
	PublicTaskID   string      `json:"public_task_id"`
	State          tasks.State `json:"state"`
	UpstreamTaskID string      `json:"upstream_task_id,omitempty"`
}

// ---------- /v1：查询 / 产物 ----------

type taskQueryResponse struct {
	TaskID          string               `json:"task_id"`
	State           tasks.State          `json:"state"`
	RawState        string               `json:"raw_state"`
	ProgressPercent int                  `json:"progress_percent"`
	Usage           map[string]any       `json:"usage,omitempty"`
	Artifacts       []tasks.ArtifactView `json:"artifacts,omitempty"`
	FailReason      string               `json:"fail_reason,omitempty"`
}

func (h *TasksHandler) handleGet(w http.ResponseWriter, r *http.Request) {
	requestID := newRelayRequestID()
	task, key, ok := h.lookupOwnedTask(w, r, requestID)
	if !ok {
		return
	}
	resp := taskQueryResponse{
		TaskID:          task.PublicTaskID,
		State:           task.State,
		RawState:        task.RawState,
		ProgressPercent: task.ProgressPercent,
		Artifacts:       h.signArtifacts(r, key, task, tasks.ProjectArtifacts(task.Data)),
		FailReason:      task.FailReason,
	}
	// usage 只投影插件声明字段；插件已被卸载时连 schema 都不可信，宁可不返回。
	if compiled, ok := h.plugins.Snapshot().Plugin(key); ok {
		resp.Usage = tasks.ProjectUsage(task.Data, tasks.DeclaredUsageFields(compiled.Manifest()))
	}
	writeJSON(w, http.StatusOK, resp)
}

type artifactsResponse struct {
	Items []tasks.ArtifactView `json:"items"`
}

func (h *TasksHandler) handleArtifacts(w http.ResponseWriter, r *http.Request) {
	requestID := newRelayRequestID()
	task, _, ok := h.lookupOwnedTask(w, r, requestID)
	if !ok {
		return
	}
	items := h.signArtifacts(r, r.PathValue("key"), task, tasks.ProjectArtifacts(task.Data))
	if items == nil {
		items = []tasks.ArtifactView{}
	}
	writeJSON(w, http.StatusOK, artifactsResponse{Items: items})
}

// signArtifacts 为产物清单注入宿主签发的 content_url。
//
// 数据投影（ProjectArtifacts）与授权（签名）分开：同一份 task.data 可以被
// 不同基址/密钥的宿主复用，而签名只属于当前宿主实例。
func (h *TasksHandler) signArtifacts(r *http.Request, pluginKey string, task *tasks.Task, items []tasks.ArtifactView) []tasks.ArtifactView {
	if len(items) == 0 {
		return items
	}
	base := h.contentBase(r)
	now := time.Now()
	for i := range items {
		// 非法 key（含 `/` 等）不签发 URL：客户端拿到一个必然 400 的能力
		// 地址比没有地址更糟。合法性由 content 端点同口径校验。
		if !tasks.ValidArtifactKey(items[i].Key) {
			continue
		}
		items[i].ContentURL = h.signer.ContentURL(base, pluginKey, task, items[i].Key, now)
	}
	return items
}

// contentBase 返回能力 URL 的基址。
//
// 优先显式 PUBLIC_BASE_URL（部署方声明的规范站点地址）；否则用本次请求的
// scheme+Host。不读取 X-Forwarded-*：那需要可信代理配置，猜错会把能力 URL
// 指向攻击者可控的主机。
func (h *TasksHandler) contentBase(r *http.Request) string {
	if h.publicBase != "" {
		return h.publicBase
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// handleContent 代理产物内容（能力式 URL，relay-06 §10.5）。
//
// 鉴权顺序刻意是「先验票、后查库」：签名无效直接 403，不消耗 DB 查询，
// 也让 task_id 枚举在签名之外没有额外侧信道。签名覆盖 pluginKey/taskID/
// artifactKey，票据不能跨任务或跨插件重放。
func (h *TasksHandler) handleContent(w http.ResponseWriter, r *http.Request) {
	requestID := newRelayRequestID()
	key := strings.TrimSpace(r.PathValue("key"))
	artifactKey := strings.TrimSpace(r.PathValue("artifact_key"))
	publicID := strings.TrimSpace(r.URL.Query().Get("task_id"))
	access := r.URL.Query().Get("access")
	if key == "" || artifactKey == "" || publicID == "" || access == "" {
		writeRelayError(w, http.StatusBadRequest, requestID, "invalid_request_error",
			"task_id and access are required")
		return
	}
	if !tasks.ValidArtifactKey(artifactKey) {
		// 冻结字符集 [A-Za-z0-9._-]{1,128}：含 `/` 等直接 400，避免路径
		// 解码歧义与跨段路由构造。
		writeRelayError(w, http.StatusBadRequest, requestID, "invalid_request_error",
			"invalid artifact_key")
		return
	}
	if !h.signer.Verify(key, publicID, artifactKey, access, time.Now()) {
		writeRelayError(w, http.StatusForbidden, requestID, "invalid_access",
			"invalid or expired artifact access ticket")
		return
	}
	task, err := h.store.GetByPublicIDAny(r.Context(), publicID)
	if err != nil {
		if errors.Is(err, tasks.ErrTaskNotFound) {
			writeRelayError(w, http.StatusNotFound, requestID, "task_not_found", "task not found")
			return
		}
		writeRelayError(w, http.StatusInternalServerError, requestID, "database_error", "failed to read the task")
		return
	}
	// 票据绑定 pluginKey，但任务行才是「这个任务属于哪个插件」的事实；
	// 两者不一致按不存在处理（不区分原因，避免跨插件探测）。
	if task.PluginKey != key {
		writeRelayError(w, http.StatusNotFound, requestID, "artifact_not_found", "artifact not found")
		return
	}
	artifact, ok := findArtifact(tasks.ProjectArtifacts(task.Data), artifactKey)
	if !ok {
		writeRelayError(w, http.StatusNotFound, requestID, "artifact_not_found", "artifact not found")
		return
	}
	// 代理段与 /v1/videos/{id}/content（T4）共用：后者不经过能力票据，
	// 授权由 Bearer 归属校验完成，取产物的动作完全相同。
	h.serveTaskArtifact(w, r, requestID, task, artifact)
}

// serveTaskArtifact 把 task.data 投影出的一项产物从上游代理给客户端。
//
// 调用方负责授权（能力票据或 Bearer 归属）；本函数只按任务行上的
// plugin_key/channel 事实取回内容，绝不接受调用方另行指定的插件或渠道。
func (h *TasksHandler) serveTaskArtifact(w http.ResponseWriter, r *http.Request, requestID string, task *tasks.Task, artifact tasks.ArtifactView) {
	key := task.PluginKey
	compiled, ok := h.plugins.Snapshot().Plugin(key)
	if !ok {
		writeRelayError(w, http.StatusServiceUnavailable, requestID, "plugin_unavailable", "task plugin is not available")
		return
	}
	channelID := task.LockedChannelID
	if channelID == 0 {
		channelID = task.ChannelID
	}
	channel, err := h.channels.GetChannel(r.Context(), channelID)
	if err != nil {
		writeRelayError(w, http.StatusBadGateway, requestID, "upstream_error", "task channel is not available")
		return
	}
	apiKey, err := h.decryptChannelCredential(channel.Credentials)
	if err != nil {
		writeRelayError(w, http.StatusInternalServerError, requestID, "channel_credential_invalid",
			"task channel credential is not usable")
		return
	}
	if strings.TrimSpace(channel.BaseURL) == "" {
		writeRelayError(w, http.StatusBadGateway, requestID, "channel_misconfigured",
			"task channel base url is not configured")
		return
	}
	model := toStringValue(task.Properties["OriginModelName"])
	upstreamModel := toStringValue(task.Properties["UpstreamModelName"])
	if upstreamModel == "" {
		upstreamModel = model
	}
	stream, err := tasks.FetchContent(r.Context(), h.client, tasks.ContentInput{
		Plugin:         compiled,
		Channel:        *channel,
		Model:          model,
		UpstreamModel:  upstreamModel,
		UpstreamTaskID: task.UpstreamTaskID,
		ArtifactKey:    artifact.Key,
		BaseURL:        channel.BaseURL,
		APIKey:         apiKey,
		Method:         r.Method,
		AllowPrivate:   h.allowPrivate,
	})
	if err != nil {
		// access 票据绝不能进日志：这里记录的入站 URL 必须脱敏。
		slog.Warn("tasks: artifact content proxy failed",
			"task_id", task.PublicTaskID, "artifact", artifact.Key,
			"url", tasks.RedactAccess(r.URL.String()), "error", err)
		writeRelayError(w, http.StatusBadGateway, requestID, "upstream_error",
			"failed to fetch artifact content")
		return
	}
	defer stream.Body.Close()
	if stream.StatusCode < 200 || stream.StatusCode > 299 {
		writeRelayError(w, http.StatusBadGateway, requestID, "upstream_error",
			"upstream rejected the artifact content request")
		return
	}
	contentType := sanitizeContentType(stream.Headers["Content-Type"])
	if contentType == "" {
		contentType = artifact.MIMEType
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	if length := stream.Headers["Content-Length"]; length != "" {
		if n, err := strconv.ParseInt(length, 10, 64); err == nil && n >= 0 && n <= tasks.MaxContentBytes {
			w.Header().Set("Content-Length", strconv.FormatInt(n, 10))
		}
	}
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	buf := make([]byte, 32<<10)
	if _, err := io.CopyBuffer(w, stream.Body, buf); err != nil {
		// 头已发出：无法改状态码，只能中断连接（不完整的流不能被当成完整产物）。
		slog.Warn("tasks: artifact content stream interrupted",
			"task_id", task.PublicTaskID, "artifact", artifact.Key, "error", err)
	}
}

func findArtifact(items []tasks.ArtifactView, key string) (tasks.ArtifactView, bool) {
	for _, item := range items {
		if item.Key == key {
			return item, true
		}
	}
	return tasks.ArtifactView{}, false
}

// sanitizeContentType 只接受不含控制字符的 Content-Type。
//
// 响应头由上游提供；含 CR/LF 的值会被 Go 的写入器拒绝，但提前归一能给出
// 确定的兜底类型，而不是让整个响应失败。
func sanitizeContentType(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 128 {
		return ""
	}
	for _, r := range raw {
		if r < 0x20 || r == 0x7f {
			return ""
		}
	}
	return raw
}

// lookupOwnedTask 完成查询的公共前置：鉴权身份、task_id 参数、归属读取。
//
// 不存在 / 不属于本人 / plugin_key 不匹配 一律 404：任何更细的区分都会
// 让 public_task_id 变成跨用户探测信道。
func (h *TasksHandler) lookupOwnedTask(w http.ResponseWriter, r *http.Request, requestID string) (*tasks.Task, string, bool) {
	principal, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		writeRelayError(w, http.StatusUnauthorized, requestID, "", "invalid token")
		return nil, "", false
	}
	key := strings.TrimSpace(r.PathValue("key"))
	publicID := strings.TrimSpace(r.URL.Query().Get("task_id"))
	if key == "" || publicID == "" {
		writeRelayError(w, http.StatusBadRequest, requestID, "invalid_request_error", "task_id is required")
		return nil, "", false
	}
	task, err := h.store.GetByPublicID(r.Context(), principal.UserID, publicID, key)
	if err != nil {
		if errors.Is(err, tasks.ErrTaskNotFound) {
			writeRelayError(w, http.StatusNotFound, requestID, "task_not_found", "task not found")
			return nil, "", false
		}
		writeRelayError(w, http.StatusInternalServerError, requestID, "database_error", "failed to read the task")
		return nil, "", false
	}
	return task, key, true
}

// ---------- GET /v1/responses/{id}（T3 / P-02） ----------

const (
	// responsesIDPrefix 是 OpenAI Responses 的对外 id 前缀。任务行的 public id
	// 用 task_ 前缀；两者共享同一段随机量，因此映射是纯前缀替换。
	responsesIDPrefix = "resp_"
	// responsesObjectType 是 Responses 对象的 object 字段冻结值。
	responsesObjectType = "response"
	// responsesProtocolName 是插件声明「接管 Responses 协议」时使用的键
	// （manifest meta.protocols.openai_responses，relay-06 §3.1）。
	responsesProtocolName = "openai_responses"
)

// resolvetaskFromResponseID 把 Responses 的 response_id 映射为 tasks.public_task_id。
//
// 只做前缀替换、不做解码：resp_ 与 task_ 后的随机量来自同一个
// tasks.NewPublicTaskID，任何转义/哈希都会破坏一一对应（P-02）。
// 非 resp_ 前缀（含空串与裸 "resp_"）一律 ok=false：不把任意路径段当作
// task id 去查库，端点因此不会变成任务 id 的存在性 oracle。
func resolvetaskFromResponseID(raw string) (taskID string, ok bool) {
	id := strings.TrimSpace(raw)
	if !strings.HasPrefix(id, responsesIDPrefix) {
		return "", false
	}
	suffix := id[len(responsesIDPrefix):]
	if suffix == "" {
		return "", false
	}
	return tasks.PublicTaskIDPrefix + suffix, true
}

// responsesObject 是 GET /v1/responses/{id} 的成功响应体。
//
// 字段集合对齐 OpenAI Responses 对象的只读子集：网关不持有 Responses 对话
// 历史（基线-09 §1.5），能如实给出的只有由任务行派生的部分（id/状态/模型/
// 产物）。刻意不填 output_text / usage 等需要上游原始响应的字段——凭空构造
// 会让客户端按不存在的语义解析。
type responsesObject struct {
	ID        string                `json:"id"`
	Object    string                `json:"object"`
	CreatedAt int64                 `json:"created_at"`
	Status    string                `json:"status"`
	Model     string                `json:"model,omitempty"`
	Output    []responsesOutputItem `json:"output"`
}

// responsesOutputItem 是 output[] 的一项：一个可检索产物 + 能力 URL。
//
// Type 原样取插件声明的产物类别（video / image / audio …），不映射到 OpenAI
// 的内建 output item 类型（message / image_generation_call …）：宿主没有足够
// 信息做这个映射，臆造类型会让客户端按错误的 schema 解析内容。
type responsesOutputItem struct {
	Type     string `json:"type"`
	ID       string `json:"id"`
	Status   string `json:"status"`
	MIMEType string `json:"mime_type,omitempty"`
	URL      string `json:"url"`
}

// handleResponsesGet 回放插件任务的产物（T3 / P-02）。
//
// 服务范围（冻结）：只服务**插件任务产物**——`resp_<x>` 映射为 `task_<x>`，
// 任务归属必须是当前令牌的 user_id，插件必须仍在装且声明
// `openai_responses` 协议，且任务至少有一个可检索产物。任一条件不满足都
// 返回 404，不区分原因：resp_<32 hex> 是高熵 id，但「存在但无权」与「不存在」
// 若给出不同状态码（403 vs 404），仍然构成存在性 oracle。
//
// 为什么先查 RouteIndex（R-10 / pluginDispatchedBuiltinShapes）：本形状登记为
// 「处理器先查插件索引」的内建路由。插件声明 GET /v1/responses/{id} 的 query
// 路由时必须由插件接管；否则内建回放会成为插件路由的静默黑洞（注册成功但
// 永远不可达）。
func (h *TasksHandler) handleResponsesGet(w http.ResponseWriter, r *http.Request) {
	requestID := newRelayRequestID()

	if match, ok := h.plugins.Snapshot().Routes().Match(r.Method, r.URL.Path); ok {
		h.DispatchNativeRoute(w, r, match)
		return
	}

	principal, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		writeRelayError(w, http.StatusUnauthorized, requestID, "", "invalid token")
		return
	}
	taskID, ok := resolvetaskFromResponseID(r.PathValue("id"))
	if !ok {
		writeResponsesNotFound(w, requestID)
		return
	}
	task, err := h.store.GetByPublicIDAny(r.Context(), taskID)
	if err != nil {
		if errors.Is(err, tasks.ErrTaskNotFound) {
			writeResponsesNotFound(w, requestID)
			return
		}
		writeRelayError(w, http.StatusInternalServerError, requestID, "database_error", "failed to read the task")
		return
	}
	// 归属不符 → 404（P-02：不是 403）：403 会把「这个 id 存在、只是不是你的」
	// 告诉调用方，使 resp_ 空间变成可枚举的存在性 oracle。
	if task.UserID != principal.UserID {
		writeResponsesNotFound(w, requestID)
		return
	}
	// 只有声明 openai_responses 协议的插件任务才由本端点服务；插件已卸载或
	// 从未声明协议 = 该 Responses 对象不存在（404，而不是 503/400）。
	compiled, ok := h.plugins.Snapshot().Plugin(task.PluginKey)
	if !ok || !pluginClaimsResponsesProtocol(compiled.Manifest()) {
		writeResponsesNotFound(w, requestID)
		return
	}
	// 保留期已过 = 产物不可检索（relay-06 §7 的 retention_deadline 语义）。
	// 用 !After(now) 而不是 Before(now)：恰好到期的那一毫秒也算过期。
	if task.RetentionDeadline != nil && !task.RetentionDeadline.After(time.Now()) {
		writeResponsesNotFound(w, requestID)
		return
	}

	artifacts := h.signArtifacts(r, task.PluginKey, task, tasks.ProjectArtifacts(task.Data))
	output := make([]responsesOutputItem, 0, len(artifacts))
	for _, artifact := range artifacts {
		// 非法 artifact key 不签发 content_url（见 signArtifacts）；没有能力 URL
		// 的产物不可检索，不能出现在回放里。
		if artifact.ContentURL == "" {
			continue
		}
		output = append(output, responsesOutputItem{
			Type:     artifact.Type,
			ID:       artifact.Key,
			Status:   "completed",
			MIMEType: artifact.MIMEType,
			URL:      artifact.ContentURL,
		})
	}
	// 产物为空 = 这个 Responses 对象没有可回放的内容（还在生成、失败、或
	// retainResult=false 已丢弃）。按 P-02 一律 404，而不是返回一个空 output
	// 的 200——后者会让客户端以为检索成功但拿不到任何产物。
	if len(output) == 0 {
		writeResponsesNotFound(w, requestID)
		return
	}

	writeJSON(w, http.StatusOK, responsesObject{
		ID:        responsesIDPrefix + strings.TrimPrefix(task.PublicTaskID, tasks.PublicTaskIDPrefix),
		Object:    responsesObjectType,
		CreatedAt: responsesCreatedAt(task),
		Status:    responsesStatusForState(task.State),
		Model:     toStringValue(task.Properties["OriginModelName"]),
		Output:    output,
	})
}

// responsesCreatedAt 返回 Responses 对象的 created_at（Unix 秒）。
//
// 优先 submit_time（提交时刻就是 Response 对象的诞生时刻）；历史行可能没有，
// 回退 created_at（行写入时刻），再回退 0 —— 绝不用 now()，那会让同一个对象
// 每次检索都换一个 created_at。
func responsesCreatedAt(task *tasks.Task) int64 {
	if task == nil {
		return 0
	}
	if task.SubmitTime != nil {
		return task.SubmitTime.Unix()
	}
	if !task.CreatedAt.IsZero() {
		return task.CreatedAt.Unix()
	}
	return 0
}

// responsesStatusForState 把任务的 7 态映射为 Responses 的 status 字面量。
//
// UNKNOWN 映射为 incomplete 而不是 in_progress：设计明确禁止把无法识别的
// 上游状态默认成进行中（会让客户端无限轮询），而 Responses 没有 unknown。
func responsesStatusForState(state tasks.State) string {
	switch state {
	case tasks.StateSuccess:
		return "completed"
	case tasks.StateFailure:
		return "failed"
	case tasks.StateInProgress:
		return "in_progress"
	case tasks.StateUnknown:
		return "incomplete"
	default:
		// not_start / submitted / queued
		return "queued"
	}
}

// pluginClaimsResponsesProtocol 判定插件 manifest 是否声明了
// `meta.protocols.openai_responses`（relay-06 §3.1）。
func pluginClaimsResponsesProtocol(m *plugin.Manifest) bool {
	if m == nil {
		return false
	}
	_, ok := m.Protocols[responsesProtocolName]
	return ok
}

// writeResponsesNotFound 是 Responses 端点唯一的「不存在」出口：固定文案、
// 不区分原因（见 handleResponsesGet 的归属注释）。
func writeResponsesNotFound(w http.ResponseWriter, requestID string) {
	writeRelayError(w, http.StatusNotFound, requestID, "not_found", "response not found")
}

// ---------- /api：台账 ----------

type taskListResponse struct {
	Items    []tasks.Task `json:"items"`
	Total    int          `json:"total"`
	Page     int          `json:"page"`
	PageSize int          `json:"page_size"`
}

func (h *TasksHandler) handleSelfList(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, problemTypeUnauthorized, "unauthorized")
		return
	}
	page, pageSize := tasksPageParams(r)
	states, ok := tasks.NormalizeStateFilter(r.URL.Query().Get("state"))
	if !ok {
		writeProblemDetail(w, http.StatusBadRequest, problemTypeInvalidRequest, "invalid_request", "unknown state filter")
		return
	}
	// R-MJ-18：两族共用 tasks 表，台账默认展示全部；family 可选收窄。
	// 非法族名是客户端构造错误（不是「没有任务」），必须显式 400。
	family, ok := taskFamilyFilter(r)
	if !ok {
		writeProblemDetail(w, http.StatusBadRequest, problemTypeInvalidRequest, "invalid_request", "unknown family filter")
		return
	}
	items, total, err := h.store.ListByUser(r.Context(), principal.UserID, tasks.ListFilter{
		States: states, Family: family, Page: page, PageSize: pageSize,
	})
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, problemTypeTasksDatabase, "database_error")
		return
	}
	// self 台账不暴露 user_id（本人恒等）；列表不含 data / private_data。
	for i := range items {
		items[i].UserID = 0
	}
	writeJSON(w, http.StatusOK, taskListResponse{Items: items, Total: total, Page: page, PageSize: pageSize})
}

func (h *TasksHandler) handleAdminList(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, problemTypeUnauthorized, "unauthorized")
		return
	}
	// 管理员可见性沿用 logs:read 口径：role=admin 且持有 logs:read（或 FULL）。
	// 缺权限返回 403，而不是「静默收窄成本人」——管理端的过滤参数与权限
	// 语义必须一致，收窄会掩盖越权尝试。
	if principal.Role != rbac.RoleAdmin || !rbac.HasPermission(*principal, rbac.PermLogsRead) {
		writeProblem(w, http.StatusForbidden, problemTypeTasksForbidden, "permission_denied")
		return
	}
	page, pageSize := tasksPageParams(r)
	states, ok := tasks.NormalizeStateFilter(r.URL.Query().Get("state"))
	if !ok {
		writeProblemDetail(w, http.StatusBadRequest, problemTypeInvalidRequest, "invalid_request", "unknown state filter")
		return
	}
	var userID int64
	if raw := strings.TrimSpace(r.URL.Query().Get("user_id")); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed <= 0 {
			writeProblemDetail(w, http.StatusBadRequest, problemTypeInvalidRequest, "invalid_request", "user_id must be a positive integer")
			return
		}
		userID = parsed
	}
	items, total, err := h.store.ListAdmin(r.Context(), tasks.ListFilter{
		UserID: userID, States: states, Page: page, PageSize: pageSize,
	})
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, problemTypeTasksDatabase, "database_error")
		return
	}
	writeJSON(w, http.StatusOK, taskListResponse{Items: items, Total: total, Page: page, PageSize: pageSize})
}

// ---------- 工具 ----------

// decryptChannelCredential 解开 channels.credentials 并取出 api_key。
// 空密文表示渠道未配置凭据：允许（auth=none 的插件存在），返回空串。
func (h *TasksHandler) decryptChannelCredential(sealed []byte) (string, error) {
	if len(sealed) == 0 {
		return "", nil
	}
	plain, err := h.sealer.Open(sealed)
	if err != nil {
		return "", err
	}
	var creds ChannelCredentials
	if err := json.Unmarshal(plain, &creds); err != nil {
		return "", err
	}
	return creds.APIKey, nil
}

// upstreamModelName 返回渠道 model_mapping 映射后的模型名（缺省回退原名）。
func upstreamModelName(ch *tasks.ChannelTarget, model string) string {
	if ch != nil {
		if mapped, ok := ch.ModelMapping[model]; ok && mapped != "" {
			return mapped
		}
	}
	return model
}

// frozenTaskBilling 是 freezeTaskBilling 的结果。
//
// snapshot 与 skip 互斥：有任务表达式时冻结快照；模型级表达式被显式跳过
// 时写 skip 标记（复核 I2：跳过必须可审计，不能与「未配置」混淆）。
type frozenTaskBilling struct {
	snapshot map[string]any
	skip     map[string]any
}

// freezeTaskBilling 按 P-06 的四级顺序解析任务计价表达式并冻结。
//
// 输入构造：
//   - 第 1/2 级：设置中心的 plugin_billing_expr（插件覆盖）；
//   - 第 3 级：客户端模型的 tiered 表达式——直接取 PreConsume 已冻结的
//     rc.PriceSnapshot，既不重复解析定价，也保证与预扣用的同一份配置；
//   - 第 4 级：映射模型只能回源读（本进程没有它的快照）。
//
// 失败口径（复核 I2）：
//   - 插件覆盖键存在但不可用（空/非法）→ error：必须 fail-closed 拒绝提交，
//     不能静默回退到其它价格来源（R-37）；
//   - 模型级表达式含 u( 却不可编译 → error（显式任务覆盖写坏了）；
//   - 模型级表达式不含 u( → skip 标记，按 held 结算（token 语义表达式）。
//
// 返回的 snapshot 携带表达式引用的字段单位与 QuotaPerUnit（复核 I1/M1）。
func (h *TasksHandler) freezeTaskBilling(ctx context.Context, rc *contract.RelayContext,
	pluginKey, clientModel, mappedModel string, m *plugin.Manifest) (frozenTaskBilling, error) {
	setting := pricing.BillingSetting{}
	if h.billingSetting != nil {
		setting = h.billingSetting()
	}
	setting.PluginBillingExpr = cloneStringMap(setting.PluginBillingExpr)
	setting.BillingMode = cloneStringMap(setting.BillingMode)
	setting.BillingExpr = cloneStringMap(setting.BillingExpr)

	if rc != nil && rc.PriceSnapshot != nil &&
		rc.PriceSnapshot.Mode == contract.BillingModeTieredExpr &&
		strings.TrimSpace(rc.PriceSnapshot.ExprString) != "" {
		setting.BillingMode[clientModel] = "tiered_expr"
		setting.BillingExpr[clientModel] = rc.PriceSnapshot.ExprString
	}

	if mappedModel != "" && mappedModel != clientModel && h.modelExpr != nil {
		mode, expr, ok, err := h.modelExpr(ctx, mappedModel)
		if err != nil {
			// 查询失败不等于「没有表达式」：记日志后按第 4 级缺失处理。
			// 任务仍可提交，差额结算失败时由 P-07 保留预扣，方向安全。
			slog.Warn("tasks: read mapped model expression failed",
				"model", mappedModel, "error", err)
		} else if ok {
			setting.BillingMode[mappedModel] = mode
			setting.BillingExpr[mappedModel] = expr
		}
	}

	res, err := pricing.ResolveTaskExprDetailed(setting, pluginKey, clientModel, mappedModel)
	if err != nil {
		return frozenTaskBilling{}, err
	}
	if res.Expr == nil {
		if res.SkipReason == "" {
			return frozenTaskBilling{}, nil
		}
		slog.Info("tasks: model tiered expression is not a task expression; settling at held",
			"plugin", pluginKey, "model", res.SkipModel, "reason", res.SkipReason)
		return frozenTaskBilling{skip: map[string]any{
			"source": string(pricing.ExprSourceHeldSkip),
			"model":  res.SkipModel,
			"reason": res.SkipReason,
		}}, nil
	}
	group := 1.0
	if rc != nil && rc.PriceSnapshot != nil && rc.PriceSnapshot.GroupRatio > 0 {
		group = rc.PriceSnapshot.GroupRatio
	}
	return frozenTaskBilling{
		snapshot: tasks.FreezeTieredSnapshot(res.Expr, res.Source, group, m),
	}, nil
}

// settleImmediateSuccess 收敛「上游即时成功」的资金（T5 实时结算路径）。
//
// 返回 settled=false 时调用方保留预扣并置 settlement_pending（P-07）；
// finalQuota 是应写入任务行的最终额度（按预扣额结清时保持 heldQuota）。
func (h *TasksHandler) settleImmediateSuccess(session contract.BillingSession, requestID string,
	frozen map[string]any, data map[string]any, m *plugin.Manifest, heldQuota int64) (bool, int64) {
	handoff, ok := session.(contract.BillingHandoff)
	if !ok {
		// 无交接面（测试桩/旧会话）：保留预扣并标 pending，由补偿扫描处理。
		return false, 0
	}
	if err := handoff.Detach(requestID); err != nil {
		// 交接失败不阻断资金收敛；最坏情况是心跳延迟被 worker 兜底
		//（安全方向），任务行仍有 request_id 可人工收敛。
		slog.Warn("tasks: billing detach failed before inline settlement",
			"request_id", requestID, "error", err)
	}

	credits, configured, evalErr := tasks.EvalTaskCost(frozen, data, m)
	if !configured {
		// 无任务表达式：沿用「按预扣额结清」，金额不变。
		if err := handoff.TerminateByRequestID(requestID, contract.UsageFact{}); err != nil {
			slog.Warn("tasks: terminate-at-held for immediate success did not complete",
				"request_id", requestID, "error", err)
			return false, 0
		}
		return true, heldQuota
	}
	if evalErr != nil {
		// 表达式求值失败：保留预扣（不退款），交由补偿扫描重试。
		slog.Warn("tasks: evaluate task expression for immediate success failed; keeping hold",
			"request_id", requestID, "error", evalErr)
		return false, 0
	}
	costSettler, ok := session.(interface {
		TerminateByRequestIDCost(requestID string, cost int64, fact contract.UsageFact) error
	})
	if !ok {
		// 没有成本结算接缝时不能退回「按预扣额结清」：配置了表达式却按
		// hold 结算会静默多收/少收。保留预扣等补偿路径（它会用冻结快照）。
		slog.Warn("tasks: billing session does not support cost settlement; keeping hold",
			"request_id", requestID)
		return false, 0
	}
	if err := costSettler.TerminateByRequestIDCost(requestID, credits, contract.UsageFact{}); err != nil {
		slog.Warn("tasks: expression settlement for immediate success did not complete",
			"request_id", requestID, "credits", credits, "error", err)
		return false, 0
	}
	return true, credits
}

// cloneStringMap 复制配置 map，避免调用方共享的表被本次解析污染。
func cloneStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// taskDurationSeconds 读取请求体里的 seconds（任务时长）。
//
// 上界用契约的 MaxRequestTaskDurationSeconds（3600s）：任何会成为计费乘数的
// 用户可控量在进入计费前必须带上界（.agents/rules/billing.md）。缺失返回 0
// （表示无时长维度）；存在但不是正数或越界则返回 ok=false，由调用方 400。
func taskDurationSeconds(v any) (int, bool) {
	if v == nil {
		return 0, true
	}
	var num float64
	switch typed := v.(type) {
	case float64:
		num = typed
	case int:
		num = float64(typed)
	case json.Number:
		parsed, err := typed.Float64()
		if err != nil {
			return 0, false
		}
		num = parsed
	default:
		return 0, false
	}
	if num <= 0 || num > contract.MaxRequestTaskDurationSeconds {
		return 0, false
	}
	if num != float64(int(num)) {
		return 0, false
	}
	return int(num), true
}

// taskFamilyFilter 解析 /api/task/self 的 family 过滤（R-MJ-18）。
//
// 空值 = 不筛（保持历史行为：台账默认展示两族）；只有冻结集合
// {plugin,mj} 合法。拼错的族名返回 ok=false，由 handler 显式 400——静默
// 返回空集会让「过滤参数写错」看起来像「今天没有任务」。
func taskFamilyFilter(r *http.Request) (string, bool) {
	family := strings.TrimSpace(r.URL.Query().Get("family"))
	if family == "" {
		return "", true
	}
	if !tasks.ValidFamily(family) {
		return "", false
	}
	return family, true
}

func tasksPageParams(r *http.Request) (int, int) {
	page, _ := strconv.Atoi(r.URL.Query().Get("p"))
	pageSize, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = tasksDefaultPageSize
	}
	if pageSize > tasksMaxPageSize {
		pageSize = tasksMaxPageSize
	}
	return page, pageSize
}

// toStringValue 把 JSON 标量转成字符串（model 等必填字段用）。
func toStringValue(v any) string {
	switch typed := v.(type) {
	case string:
		return typed
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	default:
		return ""
	}
}
