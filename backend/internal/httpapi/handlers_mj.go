// Package httpapi 的绘图任务族（family='mj'）提交端点（W8/MJ-a2）。
//
// 端点面（relay-05 §1.1 的 12 条提交端点，全部挂在根 mux 的 /mj/*）：
//
//	POST /mj/submit/imagine | change | simple-change | action | modal |
//	     shorten | describe | blend | edits | video | upload-discord-images
//	POST /mj/insight-face/swap
//
// 本文件只编排「归一化 → 限流 → 选/锁渠道 → 预扣 → 上游提交 → 落库 →
// 交接」这一条提交路径。查询族（fetch/list/image-seed）与图片代理分别在
// MJ-b/MJ-c；MJ-a2 的未实现路径统一返回 MJ 错误信封（400 unknown_endpoint）。
//
// 【计费时机（R-MJ-01，冻结）】提交时 PreConsume hold，任务行落库后 Detach
// 保留 hold；正常任务在轮询终态时结算/退款（MJ-b/d），upload-discord-images
// 的 inline 终态行在提交时按 hold 结清（R-MJ-14）。这与设计原文「提交时即
// Terminate」不同：引擎冻结了「Terminate 后 Refund 是 no-op」，因此把终态
// 结算延后，用户侧余额在提交时冻结、失败时恢复的行为不变。
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/billing"
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/egress"
	"newtg/gateway/internal/mj"
	"newtg/gateway/internal/relay"
	"newtg/gateway/internal/relay/billingseam"
	"newtg/gateway/internal/tasks"
)

const (
	// maxMJBodyBytes 是 /mj/* 提交体上限（R-MJ-16 冻结 32MiB）。
	//
	// 与领域层的数组/单项/prompt 上界配合：body 总量限制内存，领域上界限制
	// 解析/后续渠道选择的放大面。超限一律 400 固定文案（不是 413）——MJ
	// 客户端只解析 {code,description}，给 413 会让它把请求当成上游故障。
	maxMJBodyBytes = 32 << 20

	// mjSuccessDescription 是成功受理体的固定 description。
	//
	// 不透传上游 description：上游文本可能携带内部地址/账号信息，而客户端
	// 只关心 code/result。成功语义由 code=1 表达。
	mjSuccessDescription = "Submit success"

	// mjResultPersistTimeout 是「上游已受理后落库」的独立时限。
	//
	// 用 context.WithoutCancel：客户端断开不能让已受理的上游任务丢失落库与
	// 结算锚点（R-16 同口径）。上限防止 DB 卡死把请求 goroutine 永久占住。
	mjResultPersistTimeout = 10 * time.Second

	// maxMJListBodyBytes 是 list-by-condition 的请求体上限。
	//
	// R-MJ-16 已冻结 ids ≤ 100、单个 id ≤ 128 字符，最坏也就十几 KiB；
	// 64KiB 给 JSON 包装留足余量，同时把解析成本钉死在一个小常量上。
	maxMJListBodyBytes = 64 << 10
)

// MJTaskStore 是 MJ 提交/查询路径的任务行存储接缝（生产 *tasks.Repo）。
type MJTaskStore interface {
	Insert(ctx context.Context, t *tasks.Task) error
	GetByUserUpstreamID(ctx context.Context, userID int64, upstreamID string) (*tasks.Task, error)
	// ListByUserUpstreamIDs 是 list-by-condition 的批量归属读取（family=mj）。
	ListByUserUpstreamIDs(ctx context.Context, userID int64, ids []string, limit int) ([]*tasks.Task, error)
	// ListOwnersByUpstreamID 是图片代理按 MjId 取候选 owner 的全局读取
	// （MJ-c）。验签前不知道 owner，只能取出候选行逐个复算 HMAC（R-MJ-05）。
	ListOwnersByUpstreamID(ctx context.Context, upstreamID string, limit int) ([]*tasks.Task, error)
	// UpdateBilling 在 inline 上传结算后写 billing_final / settlement_pending。
	UpdateBilling(ctx context.Context, id int64, quota *int64, private map[string]any) error
}

var _ MJTaskStore = (*tasks.Repo)(nil)

// MJChannelSource 是 MJ 渠道选择/读取接缝（生产 *tasks.Repo）。
type MJChannelSource interface {
	GetChannel(ctx context.Context, id int64) (*tasks.ChannelTarget, error)
	MJChannelCandidates(ctx context.Context, model, group string) ([]tasks.MJChannelCandidate, error)
}

var _ MJChannelSource = (*tasks.Repo)(nil)

// MJSubmitter 是上游提交执行接缝（生产 *mj.SubmitClient）。
//
// 抽出接口只为测试「上游已受理后客户端断开」这类时序：真实客户端绑定
// r.Context()，无法在返回成功的同时让请求上下文已取消。
type MJSubmitter interface {
	Submit(ctx context.Context, cfg mj.SubmitConfig, endpoint mj.Endpoint,
		normalized *mj.NormalizedSubmit) (*mj.SubmitResponse, *mj.Error)
}

var _ MJSubmitter = (*mj.SubmitClient)(nil)

// MJQuerier 是查询族的上游接缝（生产 *mj.SubmitClient）。
//
// 与提交分开成接口：提交测试可以注入只实现 Submit 的假客户端而不被迫
// 伪造 query/image-seed；生产两者是同一个客户端（固定路径/凭据/禁重定向）。
type MJQuerier interface {
	Query(ctx context.Context, cfg mj.SubmitConfig, mjID string) (*mj.QueryResult, *mj.Error)
	ImageSeed(ctx context.Context, cfg mj.SubmitConfig, mjID string) ([]byte, *mj.Error)
}

var _ MJQuerier = (*mj.SubmitClient)(nil)

// MJHandlerConfig 是 MJ 提交 handler 的装配输入。
type MJHandlerConfig struct {
	Store    MJTaskStore
	Channels MJChannelSource
	// Sealer 解开渠道凭据（GC-8）。必填：没有它只能拒绝提交，绝不回落明文。
	Sealer *SecretSealer
	// Tokens 供 relay.BuildRelayContext 回源令牌分组（Bearer 路径）。
	Tokens auth.TokenLookup
	// Billing 为每次提交构造计费会话；nil 时使用 billingseam.NewStub（测试）。
	Billing func(*contract.RelayContext) contract.BillingSession
	// Config 读取 mj.* 设置；nil 时全部按 fail-closed 默认（mj.enabled=false）。
	Config ConfigReader
	// RateLimiter 是 W7/M5-b 的提交面准入限流接缝（R-M5-07）：MJ 提交是
	// 独立路由，必须显式接入同一组 B10/B11/B12 桶，不能因为不走 /v1/tasks
	// 就绕过任务提交的限流口径。
	RateLimiter RelayRateLimiter
	// Client 是上游提交执行器；nil 时构造禁重定向的默认客户端。
	Client *mj.SubmitClient
	// Submitter 是上游提交测试接缝；非 nil 时优先于 Client。
	Submitter MJSubmitter
	// Querier 是查询族（fetch 的上游侧不使用；image-seed 使用）与轮询驱动
	// 的测试接缝。nil 时复用 Client（*mj.SubmitClient 同时实现两者）。
	Querier MJQuerier
	// ImageSigner 签发/校验 /mj/image/{id} 的签名 URL（W8/MJ-c）。必填：
	// 没有它图片代理无法验签，注册一个永远 404 的端点比不注册更危险。
	ImageSigner *mj.ImageSigner
	// ImageGuard 是图片出站守卫的测试接缝（httptest 只能监听 127.0.0.1，
	// 需要 AllowLoopback/自定义解析器）。nil 时按 sec.ssrf.* 构造，且构造
	// 结果永远至少 basic + DomainAll（见 mjImagePolicy）。
	//
	// 守卫与 http.Client 在装配期构造一次并复用：按图新建 Transport 会让
	// 每张图片重新建连（连接池失效），把 CDN 延迟放大到每个产物请求。
	// 代价是 sec.ssrf.* 变更需要重启生效——与设置中心「安全策略变更走
	// 重启」的运维口径一致。
	ImageGuard *egress.Guard
	// Audit 是图片签名/归属失败的 best-effort 安全审计（security-02
	// §2.4：category=security 的 mj_image_sig_rejected）。nil 时跳过。
	Audit  *audit.Logger
	Logger *slog.Logger
	// Now / Uint64N 是测试接缝：时间与加权随机源。
	Now     func() time.Time
	Uint64N func(uint64) uint64
}

// MJHandler 持有提交依赖，可并发使用。
type MJHandler struct {
	store       MJTaskStore
	channels    MJChannelSource
	sealer      *SecretSealer
	tokens      auth.TokenLookup
	billing     func(*contract.RelayContext) contract.BillingSession
	config      ConfigReader
	rateLimiter RelayRateLimiter
	client      MJSubmitter
	querier     MJQuerier
	imageSigner *mj.ImageSigner
	// imageGuard 只做无 DNS 的形状校验（scheme/port/userinfo/域名）；
	// imageClient 是装配期构造的出站客户端（禁重定向），连接时由
	// Guard.DialContext 做地址校验与反 rebinding。
	imageGuard  *egress.Guard
	imageClient *http.Client
	audit       *audit.Logger
	log         *slog.Logger
	now         func() time.Time
	uint64N     func(uint64) uint64
}

// NewMJHandler 构造 handler。Store/Channels/Sealer/Tokens 是硬依赖：
// 缺任一都不存在安全降级（会把配置错误伪装成用户可见失败或绕过归属校验）。
func NewMJHandler(cfg MJHandlerConfig) *MJHandler {
	if cfg.Store == nil || cfg.Channels == nil || cfg.Sealer == nil || cfg.Tokens == nil || cfg.ImageSigner == nil {
		panic("httpapi: NewMJHandler requires store, channel source, sealer, token lookup and image signer")
	}
	billingFactory := cfg.Billing
	if billingFactory == nil {
		billingFactory = func(*contract.RelayContext) contract.BillingSession { return billingseam.NewStub() }
	}
	var client MJSubmitter = cfg.Submitter
	if client == nil {
		if cfg.Client != nil {
			client = cfg.Client
		} else {
			client = mj.NewSubmitClient(nil)
		}
	}
	// 查询接缝优先取显式 Querier；否则若提交客户端本身就是完整实现
	// （*mj.SubmitClient/测试 fake），复用它——固定凭据/超时/禁重定向只有
	// 一份实现。最后才构造默认客户端。
	querier := cfg.Querier
	if querier == nil {
		if q, ok := client.(MJQuerier); ok {
			querier = q
		} else {
			querier = mj.NewSubmitClient(nil)
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
	uint64N := cfg.Uint64N
	if uint64N == nil {
		// 保持 nil，SelectChannel 内部使用 rand.Uint64N；这里只在测试注入。
		uint64N = nil
	}
	// 图片出站守卫与客户端在装配期构造一次。ImageGuard 是测试接缝；
	// 生产按 sec.ssrf.* 构造，且 mjImagePolicy 永远至少 basic + DomainAll。
	imageGuard := cfg.ImageGuard
	if imageGuard == nil {
		imageGuard = egress.NewGuard(mjImagePolicy(cfg.Config))
	}
	imageClient := imageGuard.NewClient(egress.ClientOptions{Timeout: mjImageFetchTimeout})

	return &MJHandler{
		store:       cfg.Store,
		channels:    cfg.Channels,
		sealer:      cfg.Sealer,
		tokens:      cfg.Tokens,
		billing:     billingFactory,
		config:      cfg.Config,
		rateLimiter: cfg.RateLimiter,
		client:      client,
		querier:     querier,
		imageSigner: cfg.ImageSigner,
		imageGuard:  imageGuard,
		imageClient: imageClient,
		audit:       cfg.Audit,
		log:         log,
		now:         now,
		uint64N:     uint64N,
	}
}

// Register 把 12 条提交端点与 3 条查询端点挂到根 mux，并复用中继令牌鉴权链。
//
// 鉴权失败/未知端点也必须返回 MJ 信封（R-MJ-17）：RelayAuth 原生写的是
// OpenAI 错误体，因此这里用 mjEnvelopeAuth 包一层，把 <400 的响应原样放行、
// 把鉴权拒绝改写成 {code,description}。
//
// 查询族（relay-05 §1.2）不受 `mj.enabled` 追溯（设计 §1 注）：开关关闭
// 不应隐藏历史任务与产物，因此只做令牌鉴权与归属校验。
//
// 未知 /mj 路径注册为兜底（不是 ServeMux 的裸 404）：MJ 客户端只认一种
// 信封，裸 404 会让它把「路径拼错」当成网络故障。
func (h *MJHandler) Register(mux *http.ServeMux, authMiddleware func(http.Handler) http.Handler) {
	if mux == nil || authMiddleware == nil {
		panic("httpapi: MJHandler.Register requires mux and auth middleware")
	}
	auth := mjEnvelopeAuth(authMiddleware)
	for _, endpoint := range mj.Endpoints() {
		endpoint := endpoint
		mux.Handle("POST "+endpoint.Path(), auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h.handleSubmit(w, r, endpoint)
		})))
	}
	// 查询族：更具体的模式优先于 `/mj/` 兜底，注册顺序不影响匹配。
	mux.Handle("GET /mj/task/{id}/fetch", auth(http.HandlerFunc(h.handleTaskFetch)))
	mux.Handle("POST /mj/task/list-by-condition", auth(http.HandlerFunc(h.handleTaskList)))
	mux.Handle("POST /mj/task/{id}/image-seed", auth(http.HandlerFunc(h.handleImageSeed)))
	// 图片代理（W8/MJ-c）：**不挂 RelayAuth**。设计把该端点排除在令牌鉴权
	// 之外（relay-05 §1.3/§8、security-02 §2.4：不采纳强制鉴权）——签名 URL
	// 就是能力凭证，浏览器 `<img src>` 不会携带 Bearer。签名与归属校验在
	// handler 内完成，失败一律 404。
	mux.Handle("GET /mj/image/{id}", http.HandlerFunc(h.handleMJImage))

	unknown := auth(http.HandlerFunc(h.handleUnknown))
	mux.Handle("/mj/", unknown)
	mux.Handle("/mj", unknown)
}

// handleUnknown 是所有未实现/拼错的 /mj 路径的统一出口。
//
// 400 而不是 404：R-MJ-17 要求 /mj/* 的失败只有一种形状，且「未知端点」是
// 客户端构造错误（code 4），不是资源不存在。MJ-b/MJ-c 接入时用更具体的
// 模式覆盖各自路径（ServeMux 具体模式优先，不会与本兜底冲突）。
func (h *MJHandler) handleUnknown(w http.ResponseWriter, _ *http.Request) {
	writeMJError(w, mj.BadRequest(mj.DescriptionUnknownEndpoint))
}

// handleSubmit 是 12 条提交端点的唯一编排入口。
//
// 顺序即语义（与 relay-05 §4 的生命周期一致）：
//
//	门控 → 解析/归一化 → 鉴权上下文 → 限流 → 渠道/锁定 → 预扣 →
//	上游提交 → code 21 复用 → 落库 → Detach（upload 为提交时结清）
//
// 预扣之前的任何拒绝都不动资金；预扣之后的任何失败都走 defer 全额退款，
// 且提交失败不落任务行（R-MJ-13）。
func (h *MJHandler) handleSubmit(w http.ResponseWriter, r *http.Request, endpoint mj.Endpoint) {
	// 总开关最先判：mj.enabled=false 时不解析、不鉴权上下文、不预扣、不出站
	//（relay-05 §1）。门控本身需要先于解析，避免为一个已关闭的功能做
	// base64/数组上界校验与 JSON 解码。
	if !h.settingBool("mj.enabled", false) {
		writeMJError(w, mj.Forbidden(mj.DescriptionDisabled))
		return
	}

	principal, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		writeMJError(w, &mj.Error{Code: mj.CodeUnknownError, Description: mj.DescriptionInvalidToken, Status: http.StatusUnauthorized})
		return
	}

	req, mjErr := decodeMJSubmitRequest(w, r)
	if mjErr != nil {
		writeMJError(w, mjErr)
		return
	}
	// Idempotency-Key 目前只做形状/上界校验；完整的 (user,key) 去重表排
	// MJ-d（R-MJ-06 第二步）。现在拒绝畸形值是为了避免它在将来被静默接受。
	if _, _, keyErr := mj.ParseIdempotencyKey(r.Header.Get(mj.IdempotencyKeyHeader)); keyErr != nil {
		writeMJError(w, keyErr)
		return
	}

	normalized, mjErr := mj.Normalize(endpoint, req, h.normalizeOptions())
	if mjErr != nil {
		writeMJError(w, mjErr)
		return
	}
	if !tokenModelAllowed(principal.ModelLimits, normalized.Model) {
		writeMJError(w, mj.Forbidden(mj.DescriptionForbidden))
		return
	}

	rc, err := relay.BuildRelayContext(r.Context(), principal, r, h.tokens)
	if err != nil {
		status, _, _ := mapRelayContextError(err)
		writeMJError(w, mjContextError(status))
		return
	}
	// requestID 与计费会话对齐（与 /v1/tasks 同口径）：客户端报障时能用同一个
	// ID 定位上游提交、任务行与资金流水。
	requestID := newRelayRequestID()
	if requestID != "" {
		rc.RequestID = requestID
	}
	relay.WithRequest(rc, &contract.Request{Model: normalized.Model},
		contract.RelayFormat("midjourney"), r.URL.Path)

	// 准入限流：独立路由必须显式接缝，位置在渠道选择/预扣之前——被拒绝的
	// 提交不选渠道、不预扣、不触达上游。
	if rejection := checkRelayRateLimit(r.Context(), h.rateLimiter, h.config, rc, principal); rejection != nil {
		writeMJError(w, mjRateLimitError(rejection.Status))
		return
	}

	channel, lockedChannelID, originIDs, mjErr := h.resolveChannel(r.Context(), rc, principal, normalized)
	if mjErr != nil {
		writeMJError(w, mjErr)
		return
	}
	apiKey, err := h.decryptChannelCredential(channel.Credentials)
	if err != nil {
		h.log.Error("mj: channel credential is not usable", "channel_id", channel.ID, "error", err)
		writeMJError(w, &mj.Error{Code: mj.CodeUnknownError, Description: mj.DescriptionChannelMisconfigured, Status: http.StatusInternalServerError})
		return
	}
	if strings.TrimSpace(channel.BaseURL) == "" {
		writeMJError(w, &mj.Error{Code: mj.CodeUnknownError, Description: mj.DescriptionChannelMisconfigured, Status: http.StatusInternalServerError})
		return
	}

	// 预扣：固定动作价走 model_pricing(mode=1) 的 mj_<action> 行（R-MJ-01）。
	// 估算在渠道选择之后：分组倍率来自本次选中的用户分组，且无渠道时不必
	// 触碰资金。PreConsume 失败不落行。
	estimate := &contract.QuotaEstimate{
		Model:    normalized.Model,
		Group:    rc.UsingGroup,
		UsePrice: true,
		// FixedPriceKey 只写进 usage.Raw 供审计；金额由 model_pricing 决定
		//（实现里没有消费方，不要依赖它定价）。
		FixedPriceKey: normalized.Model,
	}
	relay.WithQuotaEstimate(rc, estimate)
	session := h.billing(rc)
	if session == nil {
		session = billingseam.NewStub()
	}
	if err := session.PreConsume(*estimate); err != nil {
		if errors.Is(err, billing.ErrInsufficientQuota) {
			writeMJError(w, mj.QuotaNotEnough())
			return
		}
		h.log.Warn("mj: pre-consume failed", "request_id", rc.RequestID, "model", normalized.Model, "error", err)
		writeMJError(w, mj.PricingUnavailable())
		return
	}
	committed := false
	settled := false
	defer func() {
		if !committed && !settled {
			// 预扣成功后的任何失败都必须全额退款（Refund 幂等）。
			if err := session.Refund(); err != nil {
				h.log.Warn("mj: refund after failed submit did not complete",
					"request_id", rc.RequestID, "error", err)
			}
		}
	}()

	// 单次尝试，不重试（relay-05 §6.2）。
	resp, mjErr := h.client.Submit(r.Context(), mj.SubmitConfig{
		BaseURL: channel.BaseURL,
		APIKey:  apiKey,
	}, endpoint, normalized)
	if mjErr != nil {
		writeMJError(w, mjErr)
		return
	}
	if !mj.CodeAccepted(resp.Code) {
		// 上游受理失败：不落任务行（R-MJ-13），defer 全额退款。客户端看到
		// 上游原始 code + 固定 description（不转发上游文本/地址）。
		writeMJError(w, mj.UpstreamError(resp.Code))
		return
	}

	held := session.Reserved()

	// upload-discord-images 是数组形结果、无 MjId：生成 inline 终态行并在
	// 提交时按 hold 结清（R-MJ-14），不参与轮询。
	if endpoint == mj.EndpointUploadDiscord {
		// 空数组不是「成功上传 0 张」：客户端拿不到任何附件 URL，却已被
		// 计入一次固定价。按上游坏响应失败退款（M5）。
		if len(resp.Results) == 0 {
			writeMJError(w, &mj.Error{Code: mj.CodeUnknownError, Description: mj.DescriptionUpstreamBadResponse, Status: http.StatusBadGateway})
			return
		}
		committed, settled = h.finishUpload(w, r, session, rc, principal, channel, resp, held)
		return
	}

	// 其余端点必须有单值 MjId；否则无法轮询/查询，按上游坏响应失败退款。
	// 同时按 R-MJ-16 的 id 形状校验：上游结果会被原样回给客户端并写入
	// tasks.upstream_task_id，未校验的任意长度/字符集会把 DB 列与客户端
	// 变成上游坏数据的放大器（也避免路径注入类问题）。
	if strings.TrimSpace(resp.Result) == "" || !mj.ValidMjID(resp.Result) {
		writeMJError(w, &mj.Error{Code: mj.CodeUnknownError, Description: mj.DescriptionUpstreamBadResponse, Status: http.StatusBadGateway})
		return
	}

	// code 21「任务已存在」：同用户已有该 MjId 行则复用（R-MJ-05/R-MJ-06
	// 第一步）。复用不落新行、不重复计费：hold 全额退回，响应返回已有 id。
	// 跨用户绝不复用——同用户查询命中不了时会走正常落行路径。
	if resp.Code == mj.CodeTaskExists {
		existing, lookupErr := h.store.GetByUserUpstreamID(r.Context(), principal.UserID, resp.Result)
		if lookupErr != nil && !errors.Is(lookupErr, tasks.ErrTaskNotFound) {
			h.log.Error("mj: reuse lookup failed", "request_id", rc.RequestID, "error", lookupErr)
			writeMJError(w, &mj.Error{Code: mj.CodeUnknownError, Description: mj.DescriptionInternalError, Status: http.StatusInternalServerError})
			return
		}
		if lookupErr == nil {
			key, keyErr := mj.NewReuseKey(principal.UserID, resp.Result)
			if keyErr == nil && mj.ReusableTask(existing, key) {
				if err := session.Refund(); err != nil {
					h.log.Warn("mj: refund for reused task did not complete",
						"request_id", rc.RequestID, "error", err)
					// 不置 settled：defer 会按幂等语义再试一次。若仍失败，hold
					// 留在账本上由人工/补偿处理；不能把「任务确实已存在」伪装
					// 成提交失败让客户端重试（重试只会再命中 code 21）。
				} else {
					settled = true
				}
				writeMJSuccess(w, resp.Result)
				return
			}
		}
	}

	row, mjErr := h.buildTaskRow(principal, rc, channel, normalized, resp, held, lockedChannelID, originIDs)
	if mjErr != nil {
		writeMJError(w, mjErr)
		return
	}
	// 上游已受理：落库/交接必须在客户端断连后仍完成（context.WithoutCancel）。
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), mjResultPersistTimeout)
	defer cancel()
	if err := h.store.Insert(persistCtx, row); err != nil {
		h.log.Error("mj: persist task failed after upstream accepted",
			"request_id", rc.RequestID, "error", err)
		writeMJError(w, &mj.Error{Code: mj.CodeUnknownError, Description: mj.DescriptionInternalError, Status: http.StatusInternalServerError})
		return
	}
	committed = true

	// I1：上游受理体可能直接带终态（SUCCESS/FAILURE/CANCEL）。这类行
	// 不会再进入 ListPendingByFamily，也不带补偿扫描认领的 pending 标志；
	// 如果不在提交路径就地结算，hold 会永久冻结。因此终态行与 inline 上传
	// 走同一套「Detach → Terminate/Refund → billing_final/pending」。
	if row.State.Terminal() {
		settled = h.settleTerminalAtHeld(persistCtx, session, rc, row)
		writeMJSuccess(w, resp.Result)
		return
	}

	// 异步交接：停止心跳、移除在飞租约，保留 hold 到轮询终态（R-MJ-01）。
	if handoff, ok := session.(contract.BillingHandoff); ok {
		if err := handoff.Detach(rc.RequestID); err != nil {
			// 任务行已落库，无法回滚；心跳未停会让 worker 延迟兜底（安全
			// 方向），任务行仍有 request_id 可人工收敛。
			h.log.Warn("mj: billing detach failed after task insert",
				"request_id", rc.RequestID, "error", err)
		}
	}

	writeMJSuccess(w, resp.Result)
}

// settleTerminalAtHeld 在提交路径结算一个**终态**任务行。
//
// 资金语义与插件族的终态结算一致（R-MJ-01）：
//   - SUCCESS → 按预扣额结清（Terminate by request id）；
//   - FAILURE/CANCEL → 全额退款（Refund by request id）。
//
// 顺序固定为 Detach → 结算：Detach 只停心跳/移除租约、保留 hold，不会把
// 会话结案；反过来先结算再 Detach 会让心跳在已结案会话上继续续租。
//
// 结算失败不伪装成成功：写 settlement_pending/refund_pending 保留预扣，
// 由 MJ 侧补偿扫描（MJ-b 接线后）或人工按 request_id 收敛。返回 true 表示
// 资金已收敛（调用方的 defer 不再退款）。
func (h *MJHandler) settleTerminalAtHeld(ctx context.Context, session contract.BillingSession,
	rc *contract.RelayContext, row *tasks.Task) bool {

	if row == nil {
		return false
	}
	handoff, hasHandoff := session.(contract.BillingHandoff)
	if hasHandoff {
		if err := handoff.Detach(rc.RequestID); err != nil {
			h.log.Warn("mj: billing detach before terminal settlement failed",
				"request_id", rc.RequestID, "error", err)
		}
	}

	if row.State == tasks.StateSuccess {
		var err error
		if hasHandoff {
			err = handoff.TerminateByRequestID(rc.RequestID, contract.UsageFact{})
		} else {
			err = session.Terminate(contract.UsageFact{})
		}
		if err != nil {
			h.log.Warn("mj: terminal success settlement did not complete; keeping hold",
				"request_id", rc.RequestID, "error", err)
			h.markBilling(ctx, row.ID, nil, map[string]any{"settlement_pending": true})
			return false
		}
		h.markBilling(ctx, row.ID, nil, map[string]any{"billing_final": true})
		return true
	}

	var err error
	if hasHandoff {
		err = handoff.RefundByRequestID(rc.RequestID)
	} else {
		err = session.Refund()
	}
	if err != nil {
		h.log.Warn("mj: terminal failure refund did not complete; keeping hold",
			"request_id", rc.RequestID, "error", err)
		h.markBilling(ctx, row.ID, nil, map[string]any{"refund_pending": true})
		return false
	}
	// 退款后行上的 quota 归零（与插件族失败退款同口径），避免台账把
	// 已退回的 hold 记成已消费。
	zero := int64(0)
	h.markBilling(ctx, row.ID, &zero, map[string]any{"billing_final": true})
	return true
}

// finishUpload 写 upload-discord-images 的 inline 终态行并在提交时结清。
//
// 返回 (committed, settled)：调用方用它们决定 defer 是否还要退款。顺序固定
// 为「先落行，再结算」：结算后引擎不允许 Refund（Terminate 后 Refund 是
// no-op），如果先结算再落行失败就会把已扣的钱变成孤儿。落行先于结算则最坏
// 情况是「行已存在、hold 仍冻结」，可用 request_id 收敛。
func (h *MJHandler) finishUpload(w http.ResponseWriter, r *http.Request,
	session contract.BillingSession, rc *contract.RelayContext, principal *auth.Principal,
	channel *tasks.ChannelTarget, resp *mj.SubmitResponse,
	held int64) (committed, settled bool) {

	now := h.now().UTC()
	publicID, err := tasks.NewPublicTaskID()
	if err != nil {
		writeMJError(w, &mj.Error{Code: mj.CodeUnknownError, Description: mj.DescriptionInternalError, Status: http.StatusInternalServerError})
		return false, false
	}
	tokenID := int64(0)
	if principal.TokenID != nil {
		tokenID = *principal.TokenID
	}
	row := mj.UploadInlineTask(mj.UploadSubmission{
		PublicTaskID:      publicID,
		UserID:            principal.UserID,
		TokenID:           tokenID,
		Group:             rc.UsingGroup,
		ChannelID:         channel.ID,
		Quota:             held,
		SubmitTime:        now,
		RequestID:         rc.RequestID,
		UploadURLs:        append([]string(nil), resp.Results...),
		RetentionDeadline: h.retentionDeadline(now),
	})

	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), mjResultPersistTimeout)
	defer cancel()
	if err := h.store.Insert(persistCtx, row); err != nil {
		h.log.Error("mj: persist inline upload task failed after upstream accepted",
			"request_id", rc.RequestID, "error", err)
		writeMJError(w, &mj.Error{Code: mj.CodeUnknownError, Description: mj.DescriptionInternalError, Status: http.StatusInternalServerError})
		return false, false
	}

	// 按预扣额结清：无任务表达式，固定动作价不存在「多退少补」。
	// 与普通终态提交共用同一条 Detach → Terminate 路径（I1）。
	settledOK := h.settleTerminalAtHeld(persistCtx, session, rc, row)

	// 结算失败时保留预扣并写 settlement_pending。注意（I2）：当前 MJ settler
	// 尚未接线，没有 MJ 侧补偿扫描会自动重试；该标志供 MJ-b 接入后的
	// `ListReconcilable(FamilyMJ)` 与人工按 request_id 收敛使用，不能据此
	// 宣称「一定会被自动发现」。
	writeMJSuccessResults(w, resp.Results)
	return true, settledOK
}

// buildTaskRow 把一次已受理的提交映射为 family='mj' 的任务行。
func (h *MJHandler) buildTaskRow(principal *auth.Principal, rc *contract.RelayContext,
	channel *tasks.ChannelTarget, normalized *mj.NormalizedSubmit, resp *mj.SubmitResponse,
	held, lockedChannelID int64, originIDs []string) (*tasks.Task, *mj.Error) {

	publicID, err := tasks.NewPublicTaskID()
	if err != nil {
		return nil, &mj.Error{Code: mj.CodeUnknownError, Description: mj.DescriptionInternalError, Status: http.StatusInternalServerError}
	}
	tokenID := int64(0)
	if principal.TokenID != nil {
		tokenID = *principal.TokenID
	}
	now := h.now().UTC()
	state, rawState, progress := submitState(resp)
	prompt := ""
	if normalized.Prompt != nil {
		prompt = *normalized.Prompt
	}
	code := resp.Code
	data := map[string]any{}
	if resp.Properties != nil {
		// properties 是上游受理体快照（队列数/实例 id 等），不含 base64。
		data["properties"] = resp.Properties
	}
	return mj.TaskRow(mj.Submission{
		PublicTaskID:    publicID,
		UpstreamTaskID:  resp.Result,
		Action:          normalized.Action,
		UserID:          principal.UserID,
		TokenID:         tokenID,
		Group:           rc.UsingGroup,
		ChannelID:       channel.ID,
		LockedChannelID: lockedChannelID,
		OriginTaskIDs:   originIDs,
		Prompt:          prompt,
		State:           state,
		RawState:        rawState,
		ProgressPercent: progress,
		Quota:           held,
		SubmitTime:      now,
		RequestID:       rc.RequestID,
		Code:            &code,
		Data:            data,
	}), nil
}

// submitState 把上游受理码/状态快照映射为任务状态。
//
// 设计 §4.2：1/21/22/23 都是「已受理」；22/23 是排队态。code 21 若附带
// SUCCESS 状态则直接落 success（任务可能已经出结果）。未知/缺失状态一律
// submitted，让后台轮询接管，绝不猜成终态（猜 failure 会触发退款）。
func submitState(resp *mj.SubmitResponse) (tasks.State, string, int) {
	if resp == nil {
		return tasks.StateSubmitted, "", 0
	}
	if resp.Status != "" {
		status := mj.NormalizeStatus(resp.Status, resp.Progress)
		return status.State, status.RawState, status.ProgressPercent
	}
	switch resp.Code {
	case mj.CodeQueued, mj.CodeQueueFull:
		return tasks.StateQueued, "", 0
	}
	return tasks.StateSubmitted, "", 0
}

// resolveChannel 选择目标渠道；动作类端点按原任务锁定（relay-05 §4.1）。
//
// 返回的 lockedChannelID/originIDs 只用于任务行：锁定任务是「执行渠道 =
// 原渠道」，正常任务是「加权选中的渠道」。所有拒绝都发生在 PreConsume 之前。
func (h *MJHandler) resolveChannel(ctx context.Context, rc *contract.RelayContext,
	principal *auth.Principal, normalized *mj.NormalizedSubmit) (*tasks.ChannelTarget, int64, []string, *mj.Error) {

	if normalized.LockOrigin {
		origin, err := h.store.GetByUserUpstreamID(ctx, principal.UserID, normalized.TaskID)
		if err != nil {
			if errors.Is(err, tasks.ErrTaskNotFound) {
				return nil, 0, nil, mj.BadRequest(mj.DescriptionOriginTaskMissing)
			}
			h.log.Error("mj: read origin task failed", "user_id", principal.UserID, "error", err)
			return nil, 0, nil, &mj.Error{Code: mj.CodeUnknownError, Description: mj.DescriptionInternalError, Status: http.StatusInternalServerError}
		}
		channel, err := h.channels.GetChannel(ctx, origin.ChannelID)
		if err != nil {
			if errors.Is(err, tasks.ErrChannelNotFound) {
				return nil, 0, nil, mj.BadRequest(mj.DescriptionOriginTaskMissing)
			}
			h.log.Error("mj: read origin channel failed", "channel_id", origin.ChannelID, "error", err)
			return nil, 0, nil, &mj.Error{Code: mj.CodeUnknownError, Description: mj.DescriptionInternalError, Status: http.StatusInternalServerError}
		}
		lockedID, lockErr := mj.ResolveLock(mj.OriginTask{
			Exists:         true,
			ChannelID:      origin.ChannelID,
			ChannelEnabled: channel.Status == mj.ChannelStatusEnabled,
			State:          origin.State,
		}, mj.LockPolicy{RequireSuccess: h.settingBool("mj.action_check_success_enabled", false)})
		if lockErr != nil {
			return nil, 0, nil, lockErr
		}
		// 渠道类型漂移：原渠道可能被管理员改成非 MJ 类型。继续用它会
		// 把 MJ 提交发给一个不理解该协议的上游（并按 MJ 模型计价）。
		if channel.Type != mj.ChannelTypeMidjourney && channel.Type != mj.ChannelTypeMidjourneyPlus {
			return nil, 0, nil, mj.BadRequest(mj.DescriptionChannelDisabled)
		}
		if normalized.PlusOnly && channel.Type != mj.ChannelTypeMidjourneyPlus {
			return nil, 0, nil, mj.Forbidden(mj.DescriptionPlusChannelRequired)
		}
		return channel, lockedID, []string{normalized.TaskID}, nil
	}

	rawCandidates, err := h.channels.MJChannelCandidates(ctx, normalized.Model, rc.UsingGroup)
	if err != nil {
		h.log.Error("mj: channel candidates failed", "model", normalized.Model, "error", err)
		return nil, 0, nil, &mj.Error{Code: mj.CodeUnknownError, Description: mj.DescriptionInternalError, Status: http.StatusInternalServerError}
	}
	candidates := make([]mj.ChannelCandidate, 0, len(rawCandidates))
	for _, raw := range rawCandidates {
		candidates = append(candidates, mj.ChannelCandidate{
			ID: raw.ID, Type: raw.Type, Status: raw.Status,
			Priority: raw.Priority, Weight: raw.Weight,
			SupportedModels: raw.SupportedModels, ModelMapping: raw.ModelMapping,
		})
	}
	chosen, ok := mj.SelectChannel(candidates, normalized.Model, normalized.PlusOnly, h.uint64N)
	if !ok {
		// Plus-only 端点在没有 plus 渠道、但有普通 MJ 渠道时是**能力拒绝**
		//（403），不是「没有可用渠道」（503）：前者运营可修（加 plus 渠道），
		// 后者是暂时性容量问题。
		if normalized.PlusOnly && len(mj.EligibleChannels(candidates, normalized.Model, false)) > 0 {
			return nil, 0, nil, mj.Forbidden(mj.DescriptionPlusChannelRequired)
		}
		return nil, 0, nil, mj.ServiceUnavailable(mj.DescriptionNoChannel)
	}
	channel, err := h.channels.GetChannel(ctx, chosen.ID)
	if err != nil {
		if errors.Is(err, tasks.ErrChannelNotFound) {
			return nil, 0, nil, mj.ServiceUnavailable(mj.DescriptionNoChannel)
		}
		h.log.Error("mj: read selected channel failed", "channel_id", chosen.ID, "error", err)
		return nil, 0, nil, &mj.Error{Code: mj.CodeUnknownError, Description: mj.DescriptionInternalError, Status: http.StatusInternalServerError}
	}
	// 选择与读取之间的禁用竞态：选中后被管理员禁用时拒绝（不扣费）。
	if channel.Status != mj.ChannelStatusEnabled {
		return nil, 0, nil, mj.ServiceUnavailable(mj.DescriptionNoChannel)
	}
	return channel, 0, nil, nil
}

// decryptChannelCredential 解开渠道凭据并取 api_key（明文只活在本请求内存）。
//
// 提交/查询/轮询三个入口共用同一个解密实现（decryptChannelCredentialWith）：
// 凭据格式与失败语义只应有一份，否则某条路径会悄悄接受另一种明文形态。
func (h *MJHandler) decryptChannelCredential(sealed []byte) (string, error) {
	return decryptChannelCredentialWith(h.sealer, sealed)
}

// decryptChannelCredentialWith 是渠道凭据解密的唯一实现。
func decryptChannelCredentialWith(sealer *SecretSealer, sealed []byte) (string, error) {
	if len(sealed) == 0 {
		return "", nil
	}
	if sealer == nil {
		return "", errors.New("httpapi: channel credential sealer is not configured")
	}
	plain, err := sealer.Open(sealed)
	if err != nil {
		return "", err
	}
	var creds ChannelCredentials
	if err := json.Unmarshal(plain, &creds); err != nil {
		return "", err
	}
	return creds.APIKey, nil
}

// normalizeOptions 从 mj.* 设置读取透传开关；读失败一律按关闭（fail-closed）。
func (h *MJHandler) normalizeOptions() mj.Options {
	return mj.Options{
		ForwardAccountFilter: h.settingBool("mj.account_filter_enabled", false),
		ForwardNotifyHook:    h.settingBool("mj.notify_enabled", false),
		ClearModeFlags:       h.settingBool("mj.mode_clear_enabled", false),
	}
}

// settingBool 读取布尔设置；缺失/类型错误/无配置中心一律回落。
func (h *MJHandler) settingBool(key string, fallback bool) bool {
	if h.config == nil {
		return fallback
	}
	raw, ok := h.config.Get(key)
	if !ok {
		return fallback
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return fallback
	}
	return value
}

// retentionDeadline 计算 inline 终态行的保留截止时间。
//
// mj.retention_terminal_days：0 = 不清理（返回 nil）；缺失/非法 → 设计默认
// 30 天（system-config-mapping #70）。上限由注册表约束，这里再钳一次，
// 防止配置中心里的历史脏行把 time.Duration 乘溢出。
func (h *MJHandler) retentionDeadline(now time.Time) *time.Time {
	days := int64(30)
	if h.config != nil {
		if raw, ok := h.config.Get("mj.retention_terminal_days"); ok {
			var parsed *int64
			if err := json.Unmarshal(raw, &parsed); err == nil && parsed != nil && *parsed >= 0 && *parsed <= 3650 {
				days = *parsed
			}
		}
	}
	return MJRetentionDeadline(now, days)
}

// MJRetentionDeadline 是保留截止的唯一计算实现（提交路径与轮询终态共用）。
//
// days=0 → nil（不自动清理）；负值/超界已在调用方归一化为默认值。
func MJRetentionDeadline(now time.Time, days int64) *time.Time {
	if days <= 0 {
		return nil
	}
	if days > 3650 {
		days = 3650
	}
	deadline := now.Add(time.Duration(days) * 24 * time.Hour)
	return &deadline
}

// markBilling 更新任务行的结算旗标；失败只告警（行已存在，request_id 可收敛）。
func (h *MJHandler) markBilling(ctx context.Context, id int64, quota *int64, flags map[string]any) {
	if err := h.store.UpdateBilling(ctx, id, quota, flags); err != nil {
		h.log.Warn("mj: update billing flags failed", "task_id", id, "error", err)
	}
}

// decodeMJSubmitRequest 解码有界 JSON 对象。
func decodeMJSubmitRequest(w http.ResponseWriter, r *http.Request) (*mj.SubmitRequest, *mj.Error) {
	if r.Body == nil {
		return nil, mj.BadRequest(mj.DescriptionInvalidRequest)
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxMJBodyBytes)
	var req mj.SubmitRequest
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return nil, mj.BadRequest(mj.DescriptionBodyTooLarge)
		}
		if errors.Is(err, io.EOF) {
			return nil, mj.BadRequest(mj.DescriptionInvalidRequest)
		}
		return nil, mj.BadRequest(mj.DescriptionInvalidRequest)
	}
	return &req, nil
}

// mjContextError 把 BuildRelayContext 的状态码折叠为 MJ 错误信封。
func mjContextError(status int) *mj.Error {
	switch status {
	case http.StatusBadRequest:
		// ErrRequestMissing 等「请求本身不可用」是客户端错误（M4），不能
		// 折叠成 500 让客户端以为网关故障并重试。
		return mj.BadRequest(mj.DescriptionInvalidRequest)
	case http.StatusUnauthorized:
		return &mj.Error{Code: mj.CodeUnknownError, Description: mj.DescriptionInvalidToken, Status: http.StatusUnauthorized}
	case http.StatusForbidden:
		return &mj.Error{Code: mj.CodeUnknownError, Description: mj.DescriptionForbidden, Status: http.StatusForbidden}
	default:
		return &mj.Error{Code: mj.CodeUnknownError, Description: mj.DescriptionInternalError, Status: status}
	}
}

// mjRateLimitError 把准入限流拒绝折叠为 MJ 信封。
func mjRateLimitError(status int) *mj.Error {
	if status == http.StatusTooManyRequests {
		return &mj.Error{Code: mj.CodeUnknownError, Description: mj.DescriptionRateLimited, Status: http.StatusTooManyRequests}
	}
	return &mj.Error{Code: mj.CodeUnknownError, Description: mj.DescriptionInternalError, Status: http.StatusInternalServerError}
}

// writeMJError 输出 R-MJ-17 错误信封（只有 code/description）。
//
// 非 *mj.Error 一律 internal_error：底层错误可能含 DSN、上游地址或用户输入，
// 绝不能把 err.Error() 写进 description。
func writeMJError(w http.ResponseWriter, err error) {
	mjErr, ok := err.(*mj.Error)
	status := http.StatusInternalServerError
	if ok && mjErr != nil {
		status = mjErr.HTTPStatus()
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(mj.NewFailure(err))
}

// writeMJSuccess 输出单值成功信封。
func writeMJSuccess(w http.ResponseWriter, result string) {
	writeMJJSON(w, mj.NewSuccess(mjSuccessDescription, result))
}

// writeMJSuccessResults 输出数组形成功信封（upload-discord-images）。
func writeMJSuccessResults(w http.ResponseWriter, results []string) {
	if results == nil {
		// 成功受理体恒有 result 字段；nil 序列化成 null 会破坏客户端契约。
		results = []string{}
	}
	writeMJJSON(w, mj.NewSuccess(mjSuccessDescription, results))
}

func writeMJJSON(w http.ResponseWriter, envelope mj.Envelope) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(envelope)
}

// mjEnvelopeAuth 把 RelayAuth 的 OpenAI 错误体改写成 MJ 信封（R-MJ-17）。
//
// 做法：把内层中间件写出的响应先缓冲。鉴权通过时内层会调用 next，缓冲里
// 是业务响应，原样回放；鉴权失败时缓冲里是 OpenAI 错误体，丢弃并按状态码
// 生成 {code,description}。提交端点没有流式响应，缓冲不会破坏语义。
//
// 不重写 RelayAuth 本身：令牌/用户状态/IP 白名单的判定顺序必须与 /v1 完全
// 一致，复制一份实现会让两条入口的鉴权语义漂移。
func mjEnvelopeAuth(inner func(http.Handler) http.Handler) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// passed 必须是**请求级**状态（C1）：把它放在构造函数闭包里会让
			// 同一路由的首个成功请求永久置位，之后所有鉴权失败都错写成
			// OpenAI 信封；并发请求还会产生数据竞争。因此每个请求构造一次
			// 内层链，passed 只属于本次 ServeHTTP。
			passed := false
			wrapped := inner(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				passed = true
				next.ServeHTTP(w, r)
			}))
			buf := &mjBufferedResponseWriter{header: make(http.Header)}
			wrapped.ServeHTTP(buf, r)
			if passed {
				copyResponse(w, buf)
				return
			}
			status := buf.status
			if status == 0 {
				status = http.StatusInternalServerError
			}
			writeMJError(w, mjAuthError(status))
		})
	}
}

// mjBufferedResponseWriter 是 mjEnvelopeAuth 的最小缓冲实现。
type mjBufferedResponseWriter struct {
	header http.Header
	status int
	body   strings.Builder
}

func (b *mjBufferedResponseWriter) Header() http.Header { return b.header }

func (b *mjBufferedResponseWriter) WriteHeader(status int) {
	if b.status == 0 {
		b.status = status
	}
}

func (b *mjBufferedResponseWriter) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	return b.body.Write(p)
}

func copyResponse(w http.ResponseWriter, buf *mjBufferedResponseWriter) {
	for key, values := range buf.header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	status := buf.status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = io.WriteString(w, buf.body.String())
}

// mjAuthError 把鉴权中间件的 HTTP 状态折叠为固定 MJ 错误。
func mjAuthError(status int) *mj.Error {
	switch status {
	case http.StatusUnauthorized:
		return &mj.Error{Code: mj.CodeUnknownError, Description: mj.DescriptionInvalidToken, Status: http.StatusUnauthorized}
	case http.StatusForbidden:
		return &mj.Error{Code: mj.CodeUnknownError, Description: mj.DescriptionForbidden, Status: http.StatusForbidden}
	default:
		return &mj.Error{Code: mj.CodeUnknownError, Description: mj.DescriptionInternalError, Status: http.StatusInternalServerError}
	}
}
