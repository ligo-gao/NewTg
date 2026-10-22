package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/billing"
	"newtg/gateway/internal/billing/expr"
	"newtg/gateway/internal/billing/pricing"
	"newtg/gateway/internal/channelhealth"
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/metrics"
	"newtg/gateway/internal/plugin"
	"newtg/gateway/internal/relay"
	"newtg/gateway/internal/relay/adapters"
	"newtg/gateway/internal/relay/billingseam"
	"newtg/gateway/internal/relay/crossproto"
	"newtg/gateway/internal/relay/ir"
	"newtg/gateway/internal/relay/multimodal"
	"newtg/gateway/internal/relay/stream"
	"newtg/gateway/internal/relay/usage"
	"newtg/gateway/internal/sensitive"
	"newtg/gateway/internal/store"
	"newtg/gateway/internal/tokenstore"
)

// 本文件是 Task 8 phase 8B 的 /v1 中继端点：
//
//	POST /v1/chat/completions   非流式走 relay.Route；流式走 ExecuteAttempts + SSE 桥接
//	GET  /v1/models             启用渠道 supported_models 去重并集
//	POST /v1/responses          插件协议/原生路由优先，未命中返回 501（T2 / P-01 / R-W18-07）
//	POST /v1/responses/compact  本波不提供（R-W18-12）：不注册 → 404
//	POST /v1/messages           Anthropic 原生入口（W7/M1）：仅路由到 Anthropic
//	                            协议族渠道，其余方向 fail-closed 501（R-59）
//
// 边界（relay-01 §2 阶段序）：鉴权在中间件（relay_auth.go）已完成；本文件负责
// 请求体上限与 IR 归一化（阶段 4）、RelayContext 组装（阶段 5）、路由/执行
// （阶段 7–13）、响应/错误映射（阶段 13–14）。计费只经由 contract.BillingSession
// 接缝（M2 是 billingseam.Stub，GC-3：不实现定价）。
//
// 安全不变量（GC-6）：所有错误只含固定文案 + requestId；上游 URL、渠道凭据、
// 适配器原始错误文本绝不出现在响应体或日志。上游请求体的 32MiB 上限与
// relay-01 §11.1 的超时值是冻结常量。

const (
	// maxRelayBodyBytes 是 /v1/chat/completions 的请求体上限（32MiB）。
	// 超限返回 413（OpenAI 信封），而不是把超大 body 读进内存。
	maxRelayBodyBytes = 32 << 20

	// relayMaxRetries 是 M2 的重试次数（不含首试）。GC-5 / C-9/D11 冻结默认值 3；
	// relay.ExecuteAttempts 会再钳制到 [1,5]。
	relayMaxRetries = 3

	// relay-01 §11.1 冻结的上游客户端超时。这些值同时存在于 relay 包的未导出
	// 常量里；端点层再写一份是任务书要求（请求级注入客户端时的唯一配置点）。
	// 整体读取（Client.Timeout）保持 0：流式/长响应不能被总时长掐断，
	// 响应头等待由 Transport.ResponseHeaderTimeout 负责。
	relayResponseHeaderTimeout = 1800 * time.Second
	relayIdleConnTimeout       = 90 * time.Second
	relayDialTimeout           = 30 * time.Second
	relayTLSHandshakeTimeout   = 10 * time.Second
	relayMaxIdleConns          = 500
	relayMaxIdleConnsPerHost   = 100
	relayExpectContinueTimeout = 1 * time.Second

	// channelTestTimeout 是渠道连通性测试的单次上限（modules-06 F-06-06 的 30s）。
	// 体检单次上限是 channelhealth.ProbeTimeout（15s，R-M4-14）；两者由
	// TestChannelTestTimeoutsFrozen 锁定，防止误合并。
	channelTestTimeout = 30 * time.Second

	// maxSoftStreamErrors 是软流错误的记录上限（relay-03 §4.3/§10.1：上限 20 条）。
	// 第 21 条软错误按硬错误处理：继续吞错会让一个持续损坏的上游把连接
	// 拖成永不结束的「成功」流。
	maxSoftStreamErrors = 20

	// streamInactivityTimeout 是块间不活动看门狗的默认值（relay-03 §5.4/§10.1
	// 冻结的 300s）。上游在等待期或传输期静默超过该值即中断流并按 timeout 结算。
	// 测试通过 RelayHandlerOptions.StreamInactivityTimeout 注入短值。
	streamInactivityTimeout = 300 * time.Second

	// streamPingInterval 是 SSE 保活注释帧 `: PING\n\n` 的间隔（relay-03 §3.3：
	// 契约 §10.1 冻结的唯一值 10s）。等待期与传输期共用同一个 ticker。
	streamPingInterval = 10 * time.Second
)

var (
	// errRelayBodyTooLarge 表示请求体超过 maxRelayBodyBytes（映射 413）。
	errRelayBodyTooLarge = errors.New("httpapi: relay request body too large")
	// errRelayCredentialMissing / errRelayCredentialInvalid 是渠道凭据解封失败的
	// 统一出口：文本不含密文、base_url 或底层错误（GC-6/GC-8）。
	errRelayCredentialMissing = errors.New("httpapi: channel credential is not configured")
	errRelayCredentialInvalid = errors.New("httpapi: channel credential cannot be opened")
)

// RelayChannelStore 是中继端点需要的渠道存储面；生产实现是 *store.ChannelRepo，
// 测试注入内存 fake（与 phase 8A 的 ChannelStore 分离：这里只需要读路径）。
type RelayChannelStore interface {
	Candidates(ctx context.Context, model, group string) ([]contract.ChannelCandidate, error)
	Get(ctx context.Context, id int64) (*store.Channel, error)
	EnabledModels(ctx context.Context) ([]string, error)
}

var _ RelayChannelStore = (*store.ChannelRepo)(nil)

// RelayModelStatusStore 是派发前的模型定价状态只读查询面（R-84 /
// modules-06 TC-F-06-13-01）。生产实现是 *store.SQLPricingRepo。
//
// 返回 (status, found, err)：found=false 表示 model_pricing 无该模型的行，
// 按默认价兜底、视为启用；status=2 表示管理员已停用该模型，用户请求必须
// 在进入上游之前返回 404。
type RelayModelStatusStore interface {
	ModelPricingStatus(ctx context.Context, model string) (status int16, found bool, err error)
}

var _ RelayModelStatusStore = (*store.SQLPricingRepo)(nil)

// AttemptFailureReporter 接收中继每轮「非 2xx 响应」的失败事实（W7/M4-a）。
//
// 生产实现是 *channelhealth.Reporter：先做纯判定（全局开关/auto_ban/触发
// 集合），命中后非阻塞入队，由后台 goroutine 执行条件更新 + 审计 + 站内通知。
// 实现必须非阻塞；契约要求的触发集合与默认关闭语义由实现方负责。
type AttemptFailureReporter interface {
	ReportAttemptFailure(ctx context.Context, channelID int, autoBan bool,
		class contract.ErrorClass, statusCode int, body []byte)
}

// modelPricingStatusDisabled 是 model_pricing.status 的停用值（modules-06
// §3.7 状态机：1 启用 / 2 停用；迁移的 CHECK 约束只允许这两个值）。
const modelPricingStatusDisabled int16 = 2

// RelayHandlerOptions 是 platform 装配与测试的注入点。
type RelayHandlerOptions struct {
	// Channels 是候选/渠道/模型列表的数据源（必填）。
	Channels RelayChannelStore
	// Tokens 是令牌解析接缝（auth.TokenLookup；生产为 *store.TokenRepo）。
	Tokens auth.TokenLookup
	// Access 回源 users.role/status（生产为 *store.TokenRepo）。
	Access UserAccessStore
	// Sealer 解开 channels.credentials 的 AES-GCM 密文（GC-8，必填）。
	Sealer *SecretSealer
	// Billing 为每个请求构造计费会话；nil 时使用 billingseam.NewStub（M2 桩，GC-3）。
	// 每请求新建是契约要求（真实 Session 与 billingseam.Stub 都是单会话实例）。
	//
	// 【T9 签名】入参是该请求的 RelayContext：真实引擎的 NewSession 需要它把
	// RequestID/UserID/TokenID/UsingGroup/PriceSnapshot 绑定到会话（契约 §6.1）。
	Billing func(*contract.RelayContext) contract.BillingSession
	// ModelStatus 是派发前的模型停用检查（R-84）；nil 时跳过（测试/降级）。
	// 生产由 platform 注入 store.NewPricingRepo(DB)。
	ModelStatus RelayModelStatusStore
	// Client 是上游 HTTP 客户端；nil 时按 relay-01 §11.1 超时构造。
	// 测试用 httptest.Server.Client() 注入，以复用其 TLS/连接配置。
	Client *http.Client
	// Dispatch 是渠道类型 → 适配器分派；nil 时使用 adapters.Dispatch。
	// 测试注入 fake StreamAdapter 以覆盖 soft/hard 流错误等上游难以复现的路径。
	Dispatch func(contract.ChannelType) (contract.Adapter, error)
	// TrustedProxies 是 TRUSTED_PROXIES 原文（逗号分隔 IP/CIDR）。为空时
	// RelayAuth 只信 RemoteAddr，忽略 X-Forwarded-For（M2 策略见 relay_auth.go）。
	TrustedProxies string
	// StreamInactivityTimeout 覆盖流式看门狗的默认 300s；<=0 使用默认值。
	// 仅测试注入短值，生产不得调小（relay-03 §10.1 冻结值）。
	StreamInactivityTimeout time.Duration
	// Multimodal 是 M4 多模态端点族的分派器（Task 4 注入）。
	// nil 时这些端点返回 501（仅 Task 1 骨架态）；T4 装配真实实现后不得再为 nil。
	Multimodal MultimodalDispatcher
	// Config 是跨协议转发开关的读取面（configcenter.Center 实现 ConfigReader）。
	// nil 时按默认 false 处理（GC-8 / R-03）：功能必须显式启用，绝无隐式开启。
	// 同一读取面也供 sec.sensitive.*（F-08-18）使用。
	Config ConfigReader
	// Logger 是安全事件日志出口；nil 时用 slog.Default。命中事件只记
	// security.sensitive_word_hit（不落命中词明文，F-08-18）。
	Logger *slog.Logger
	// UserGroups 是 /pg/* 会话中继的用户分组查询面（Task 13）：会话身份没有
	// 令牌分组，必须回源 users."group" 才能选渠道；同时用可用分组模型清单
	// 在计费前把「模型不可用」判成 403。nil 时 /pg 路径不可用（会话请求
	// 会以 500 失败关闭），生产装配必须传入。
	UserGroups PlaygroundUserGroups
	// NativeRoutes 是插件原生路由的执行面（T6c-a）；nil 时 /v1/ 兜底仍会
	// 挂载，但只做 404/405 判定——未知路径的 OpenAI 信封不依赖它。
	NativeRoutes NativeRouteDispatcher
	// GroupConfigured 是 R-64/I-25 的分组倍率存在性校验：严格查询
	// user_groups WHERE name=? AND status=1，**不得**复用 pricing 的
	// 「未命中缓存 1.0」路径。nil 时跳过校验（仅测试/无计费降级装配）；
	// 生产由 platform 注入 store.SQLPricingRepo.GroupConfigured。
	GroupConfigured func(ctx context.Context, group string) (bool, error)
	// AttemptFailureReporter 是 M4-a 的自动禁用判定接缝（生产
	// *channelhealth.Reporter）。nil 时不上报（测试/降级装配）。
	AttemptFailureReporter AttemptFailureReporter
	// RateLimiter 是中继准入限流接缝（W7/M5-b）；生产由 platform 注入一个
	// 复用现有 Redis 客户端的 *ratelimit.Limiter。nil 表示未装配（仅测试/
	// 无 Redis 的降级装配），此时跳过限流、行为与 M5 之前一致。
	RateLimiter RelayRateLimiter
}

// NativeRouteDispatcher 是插件原生路由在 HTTP 面的唯一接缝（生产实现
// *TasksHandler）。索引与执行必须来自同一份插件快照：分开注入两次会让
// 「匹配到的 Route」与「实际执行它的插件版本」分属不同 generation。
type NativeRouteDispatcher interface {
	// NativeRoutes 返回当前插件快照的只读路由索引。
	NativeRoutes() plugin.RouteIndex
	// DispatchNativeRoute 执行一次已命中的原生路由；鉴权由调用方先完成。
	DispatchNativeRoute(w http.ResponseWriter, r *http.Request, match plugin.RouteMatch)
}

// PlaygroundUserGroups 是 /pg/*（Playground 会话中继）需要的用户分组读取面。
// 生产实现是 *store.UserGroupsRepo；测试可注入内存实现。
type PlaygroundUserGroups interface {
	// UserGroup 返回启用用户的当前分组；用户不存在/停用返回 store.ErrUserNotFound。
	UserGroup(ctx context.Context, userID int64) (string, error)
	// UsableGroupsForUser 返回可用分组与各自已定价、有启用渠道的模型清单，
	// 与 GET /api/status 的 usable_groups 同口径。
	UsableGroupsForUser(ctx context.Context, userID int64) ([]store.UsableGroup, error)
}

// RelayHandler 持有 /v1 端点依赖。字段不导出，装配面只有 RelayHandlerOptions。
type RelayHandler struct {
	channels         RelayChannelStore
	tokens           auth.TokenLookup
	access           UserAccessStore
	sealer           *SecretSealer
	billing          func(*contract.RelayContext) contract.BillingSession
	modelStatus      RelayModelStatusStore
	client           *http.Client
	dispatch         func(contract.ChannelType) (contract.Adapter, error)
	trustedProxies   []netip.Prefix
	streamInactivity time.Duration
	multimodal       MultimodalDispatcher
	config           ConfigReader
	// sensitive 是 sec.sensitive.* 的解析缓存（F-08-18）；零值可用。
	// 每个请求从 h.config 取当前快照，配置变更后缓存按原始值哈希自然失效。
	sensitive       *sensitive.Cache
	log             *slog.Logger
	userGroups      PlaygroundUserGroups
	nativeRoutes    NativeRouteDispatcher
	groupConfigured func(ctx context.Context, group string) (bool, error)
	// attemptReporter 是 M4-a 的自动禁用上报接缝；nil 时执行层回调为 nil。
	attemptReporter AttemptFailureReporter
	// rateLimiter 是 W7/M5-b 的准入限流接缝；nil 时三档桶全部跳过。
	rateLimiter RelayRateLimiter
	// maxAutoGroups 是 R-63 的跨组上限（装配处解析并钳制 [1,20]）。
	maxAutoGroups int
	// playground 是 /pg/* 的内部转发表（路径已重写为 /v1/*，不含 RelayAuth；
	// 会话鉴权由 RegisterPlayground 的外层链完成）。
	playground *http.ServeMux
}

// NewRelayHandler 构造中继端点 handler。
//
// 渠道存储与密钥封装是硬依赖：静默降级（空候选/空密钥）会把「配置错误」
// 伪装成「用户请求失败」，因此直接 panic（与 phase 8A NewChannelHandler 同口径）。
func NewRelayHandler(opts RelayHandlerOptions) *RelayHandler {
	if opts.Channels == nil {
		panic("httpapi: NewRelayHandler requires a channel store")
	}
	if opts.Sealer == nil {
		panic("httpapi: NewRelayHandler requires a credential sealer (GC-8)")
	}
	client := opts.Client
	if client == nil {
		client = newRelayHTTPClient()
	}
	billing := opts.Billing
	if billing == nil {
		billing = func(*contract.RelayContext) contract.BillingSession { return billingseam.NewStub() }
	}
	dispatch := opts.Dispatch
	if dispatch == nil {
		dispatch = adapters.Dispatch
	}
	inactivity := opts.StreamInactivityTimeout
	if inactivity <= 0 {
		inactivity = streamInactivityTimeout
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	h := &RelayHandler{
		channels:         opts.Channels,
		tokens:           opts.Tokens,
		access:           opts.Access,
		sealer:           opts.Sealer,
		billing:          billing,
		modelStatus:      opts.ModelStatus,
		client:           client,
		dispatch:         dispatch,
		trustedProxies:   parseTrustedProxies(opts.TrustedProxies),
		streamInactivity: inactivity,
		multimodal:       opts.Multimodal,
		config:           opts.Config,
		sensitive:        &sensitive.Cache{},
		log:              logger,
		userGroups:       opts.UserGroups,
		nativeRoutes:     opts.NativeRoutes,
		groupConfigured:  opts.GroupConfigured,
		attemptReporter:  opts.AttemptFailureReporter,
		rateLimiter:      opts.RateLimiter,
		// R-63：站点键 relay.cross_group_retry_max_groups（默认 3，非法回退
		// 默认，钳制 [1,20]）。在这里解析一次，请求热路径只读字段。
		maxAutoGroups: resolveCrossGroupRetryMaxGroups(opts.Config),
	}
	// M4 T4：生产装配默认注入真实多模态分派器（T1 复审 F2/F4/F5 与 R-02 的
	// 落点）。显式传入的 Multimodal 优先（测试可替换）；nil 不再表示 501——
	// 多模态端点与 chat 共用同一套候选/渠道/计费/客户端接缝。
	if h.multimodal == nil {
		h.multimodal = multimodal.NewDispatcher(multimodal.Options{
			Candidates: func(ctx context.Context, rc *contract.RelayContext) ([]contract.ChannelCandidate, error) {
				return h.channels.Candidates(ctx, rc.OriginModel, rc.UsingGroup)
			},
			ResolveChannel: func(ctx context.Context, rc *contract.RelayContext, channelID int) (relay.ChannelAssignment, error) {
				return h.resolveChannel(ctx, rc, channelID)
			},
			Dispatch:   h.dispatch,
			Billing:    h.billing,
			Client:     h.client,
			MaxRetries: relayMaxRetries,
			WriteError: writeRelayMappedError,
			// 与 chat 同一套 R-63/R-64 语义：多模态端点同样可能携带 auto 令牌。
			MaxAutoGroups:   h.maxAutoGroups,
			GroupConfigured: h.groupConfigured,
			// M4-a：多模态非 realtime 端点也经 ExecuteAttempts，失败渠道必须
			// 进入同一条自动禁用上报路径（调研 §5.1 的实际可达路径）。
			OnAttemptFailure: h.reportAttemptFailure,
		})
	}
	return h
}

// resolveCrossGroupRetryMaxGroups 解析 R-63 的跨组尝试上限。
//
// 缺省 3；非整数/≤0 回退默认（配置损坏不得放大或关闭预算）；最后钳制到
// [1,20]。组数上限只约束**尝试的分组数**，组内重试预算由 relayMaxRetries
// 独立决定，不合并。
func resolveCrossGroupRetryMaxGroups(config ConfigReader) int {
	limit := tokenstore.DefaultCrossGroupRetryMaxGroups
	if value, ok := configIntValue(config, tokenstore.KeyCrossGroupRetryMax); ok && value > 0 {
		limit = value
	}
	if limit < 1 {
		limit = tokenstore.DefaultCrossGroupRetryMaxGroups
	}
	if limit > relay.MaxCrossGroupRetryGroups {
		limit = relay.MaxCrossGroupRetryGroups
	}
	return limit
}

// configIntValue 读取整数配置，同时接受 JSON number 与数字字符串。
//
// 与 tokenstore.configIntFrom 同口径（同一站点键不能因读取方不同而有两种
// 解释）；非整数/缺失返回 ok=false，由调用方回退默认值。
func configIntValue(config ConfigReader, key string) (int, bool) {
	if value, ok := readConfigInt64(config, key); ok {
		return int(value), true
	}
	raw := readConfigString(config, key)
	if raw == "" {
		return 0, false
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil {
		return 0, false
	}
	return parsed, true
}

// AuthMiddleware 返回 /v1 令牌鉴权中间件。
//
// 单独导出是为了让同一进程内的其它 /v1 端点族（M6d 的 /v1/tasks*）复用
// **同一个** RelayAuth 实例与同一份 trustedProxies 解析结果：复制一份配置
// 会让两条路径在代理信任策略上悄悄分叉。
func (h *RelayHandler) AuthMiddleware() func(http.Handler) http.Handler {
	return RelayAuth(h.tokens, h.access, h.trustedProxies)
}

// Register 把 /v1 路由挂到 mux；鉴权中间件只包裹这些端点（管理面不经过它）。
//
// 多模态端点族（M4 T1）复用同一个 RelayAuth 中间件：统一入口要求所有 /v1
// 端点先鉴权再归一化（relay-04 §0.2）。GET /v1/models 只在此处注册一次
// （R-09），多模态注册函数不得重复注册它。
func (h *RelayHandler) Register(mux *http.ServeMux) {
	authMiddleware := h.AuthMiddleware()
	mux.Handle("POST /v1/chat/completions", authMiddleware(http.HandlerFunc(h.handleChatCompletions)))
	mux.Handle("GET /v1/models", authMiddleware(http.HandlerFunc(h.handleModels)))
	// T2 / P-01：OpenAI Responses 创建端点——插件原生路由优先，未命中返回
	// 501（R-13；Responses 透传/转码另立 T2b，通用中继不承接）。
	// 精确模式注册（不是 /v1/ 子树），与 T1 的兜底不重叠：ServeMux 让精确模式
	// 胜出，兜底只接收没有其它路由认领的 /v1 请求。
	mux.Handle("POST /v1/responses", authMiddleware(http.HandlerFunc(h.handleResponses)))
	// R-W18-12：`POST /v1/responses/compact` 本波不提供，**刻意不注册**。
	// 不注册的路径会落到 /v1 兜底并返回 404；保留 501 半死桩会让客户端误以为
	// 「改天就能用」，也违反设计「不提供 = 404」的诚实失败口径。
	// W7/M1：Anthropic Messages 原生入口。与 chat 共用同一个 RelayAuth 中间件
	// 与同一个计费接缝（h.billing）；鉴权载体 x-api-key 的归一化已在
	// relay_auth.go / relay.credentialFromRequest 中按路径固化。
	mux.Handle("POST /v1/messages", authMiddleware(http.HandlerFunc(h.handleMessages)))

	// M4 T1 的多模态路由骨架：dispatcher 由 Task 4 注入；同时把 TokenLookup
	// 交给多模态 handler 以复用 BuildRelayContext 的分组回源路径。
	RegisterMultimodalRoutes(mux, authMiddleware, NewMultimodalHandler(MultimodalHandlerOptions{
		Tokens:      h.tokens,
		ModelStatus: h.modelStatus,
		Dispatcher:  h.multimodal,
		Groups:      h.userGroups,
		// W7/M5-b：多模态族与 chat 共用同一个限流接缝与同一份配置快照。
		RateLimiter: h.rateLimiter,
		Config:      h.config,
	}))

	// T6c-a：插件原生路由（manifest routes[]）的 /v1 兜底。
	//
	// 用无方法的子树模式 `/v1/`：更具体的精确模式（POST /v1/chat/completions、
	// GET /v1/models、POST /v1/tasks/{key} 等）按 ServeMux 优先级胜出，兜底只
	// 接收「没有其它路由认领」的 /v1 请求。它不是通用中继回退：命中插件路由
	// 走任务数据面，未命中一律 404 OpenAI 信封（P-05），绝不进入 chat 管线。
	mux.Handle(nativeRouteFallbackPattern, h.nativeRouteFallback(authMiddleware, mux))
}

// nativeRouteFallbackPattern 是原生路由兜底的 ServeMux 模式。
const nativeRouteFallbackPattern = "/v1/"

// nativeRouteFallback 构建 /v1/ 兜底 handler。
//
// 鉴权只包裹**命中**的原生路由（与 /v1/tasks 同一条 RelayAuth 链）：未知
// 路径保持「无令牌也 404」的既有可见性——兜底上线之前根 mux 对未知 /v1
// 就是 404，能力探测面不应因为加了兜底而变成 401/404 二值 oracle。
//
// 405 语义必须由这里补回：Go 的 ServeMux 只有在「路径匹配某个模式但方法不
// 匹配」时才返回 405，而 `/v1/` 匹配所有方法，若不显式探测，POST /v1/realtime
// 会从 405 退化成 404。探测只查路由表（mux.Handler），不执行任何 handler。
func (h *RelayHandler) nativeRouteFallback(authMiddleware func(http.Handler) http.Handler, mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := newRelayRequestID()
		if h.nativeRoutes != nil {
			if match, ok := h.nativeRoutes.NativeRoutes().Match(r.Method, r.URL.Path); ok {
				// 每次命中现建一个只用一次的 handler 链：中间件本身无状态，
				// 包住 match 比把 RouteMatch 塞进 context 更直接、更易审计。
				authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					h.nativeRoutes.DispatchNativeRoute(w, r, match)
				})).ServeHTTP(w, r)
				return
			}
		}
		// R-W18-12：明确不提供的 Responses 形状必须 404，而不是被下面的
		// 405 探测变成「方法不允许」。GET /v1/responses（WebSocket）与
		// POST /v1/responses/compact 在本波没有 handler，路径存在（POST
		// /v1/responses 已注册）会让探测返回 405——那会把「没提供」误报成
		// 「换个方法就行」，因此在这里先收敛为 404。
		if responsesEndpointUnprovided(r.Method, r.URL.Path) {
			writeRelayError(w, http.StatusNotFound, requestID, "invalid_request_error", "unknown endpoint")
			return
		}
		if allowed := methodMismatchAllowed(mux, r); len(allowed) > 0 {
			w.Header().Set("Allow", strings.Join(allowed, ", "))
			writeRelayError(w, http.StatusMethodNotAllowed, requestID, "method_not_allowed",
				"method not allowed for this endpoint")
			return
		}
		writeRelayError(w, http.StatusNotFound, requestID, "invalid_request_error", "unknown endpoint")
	})
}

// responsesEndpointUnprovided 判定是否为 R-W18-12 明确不提供的端点。
//
//   - `GET /v1/responses`：WebSocket 升级，本波不做（无状态集群下需要连接
//     粘滞与排空语义），不注册路由 → 404；
//   - `POST /v1/responses/compact`：独立计费口径的压缩端点，本波不做 →
//     不注册路由 → 404（501 半死桩已删除）。
//
// 只有 GET /v1/responses 收敛为 404：其它方法（PUT/DELETE…）走既有 405
// 探测（POST /v1/responses 已注册，Allow 必须如实列出 POST）。compact 的
// 路径不存在，任何方法都应 404。
//
// 只匹配精确路径（尾斜杠归一）；`/v1/responses/{id}` 属于已提供的检索面，
// 不得被这条规则误伤。
func responsesEndpointUnprovided(method, path string) bool {
	normalized := strings.TrimSuffix(path, "/")
	switch normalized {
	case "/v1/responses/compact":
		return true
	case "/v1/responses":
		return strings.EqualFold(method, http.MethodGet)
	}
	return false
}

// nativeProbeMethods 是方法不匹配探测的候选集合。
//
// 覆盖 /v1 面已注册的全部方法族（含 OPTIONS，供将来的 CORS 端点）；HEAD 单独
// 探测是因为 Go 的 GET 模式同时匹配 HEAD，不探测就无法在 Allow 里体现它。
var nativeProbeMethods = []string{
	http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
	http.MethodPatch, http.MethodDelete, http.MethodOptions,
}

// methodMismatchAllowed 返回「此路径存在、但当前方法不匹配」时允许的方法。
//
// 返回空切片表示路径本身没有注册过（真正的 404）。探测通过 mux.Handler
// 只做路由求解，不会执行 handler，也不消耗 body（请求未动，浅拷贝仅改 Method）。
func methodMismatchAllowed(mux *http.ServeMux, r *http.Request) []string {
	if mux == nil {
		return nil
	}
	allowed := make([]string, 0, len(nativeProbeMethods))
	for _, method := range nativeProbeMethods {
		if method == r.Method {
			continue
		}
		probe := new(http.Request)
		*probe = *r
		probe.Method = method
		_, pattern := mux.Handler(probe)
		if pattern == "" || pattern == nativeRouteFallbackPattern {
			continue
		}
		allowed = append(allowed, method)
	}
	return allowed
}

// Handler 返回挂好路由的独立 mux（测试与嵌入式装配用）。
func (h *RelayHandler) Handler() http.Handler {
	mux := http.NewServeMux()
	h.Register(mux)
	return mux
}

// ---------- /pg/*（Playground 会话中继，relay-01 §1.5） ----------

// RegisterPlayground 把 /pg/* 挂到根 mux。
//
// 与 Register 的区别只在鉴权链：/pg 只接受浏览器会话（PlaygroundSessionAuth），
// 不接受 API 令牌；通过后把路径重写到 /v1/*，交给**同一批**转发函数
// （handleChatCompletions / handleModels）。本函数刻意不复制路由、计费、
// 流式桥接逻辑：任何 /v1 侧的修复都会自动作用于 /pg。
//
// 挂载面按 relay-01 §1.5 收敛为**对话族**：只注册 `POST /pg/chat/completions`
// 与 `GET /pg/models`，其余 `/pg/*`（含 /v1 的多模态族 images/edits/
// embeddings/rerank/audio/engines/v1beta/realtime）一律 404 OpenAI 信封。
// 不把多模态族一并暴露的理由：设计只承诺对话族，且 OpenAPI 只登记这两条；
// 多挂的路径既无契约也无 UI 消费方，等于把未评审的计费面开给浏览器会话。
//
// 会话写路径沿用 /api/* 的 CSRF + OriginRefererCheck（Task 13 brief §3）：
// Playground 的 POST 是浏览器写操作，必须与其它会话写请求同口径。
func (h *RelayHandler) RegisterPlayground(mux *http.ServeMux, sessions *auth.SessionManager, publicBaseURL string) {
	if mux == nil || sessions == nil {
		panic("httpapi: RegisterPlayground requires a mux and a session manager")
	}
	// 内部转发表只注册对话族两条路由；会话鉴权已在最外层完成，故这里直接
	// 注册裸 handler，不再引入 RelayAuth 的令牌链。未注册的 /pg/* 由
	// handlePlayground 的 pattern=="" 分支统一回 404。
	inner := http.NewServeMux()
	inner.Handle("POST /v1/chat/completions", http.HandlerFunc(h.handleChatCompletions))
	inner.Handle("GET /v1/models", http.HandlerFunc(h.handleModels))
	h.playground = inner

	chain := PlaygroundSessionAuth(sessions)(
		CSRF(sessions)(
			OriginRefererCheck(parseAllowedOrigins(publicBaseURL))(
				http.HandlerFunc(h.handlePlayground))))
	mux.Handle("/pg/", chain)
	// 精确 /pg（无尾斜杠）也要落到同一 404 语义，而不是根 mux 的纯文本 404。
	mux.Handle("/pg", chain)
}

// handlePlayground 把 /pg 前缀重写为 /v1 后分派给内部转发表。
//
// 未支持的 /pg/* 返回 404 + OpenAI 错误信封（/pg 是 OpenAI 协议面，
// 不使用管理面的 RFC 7807 problem）。
func (h *RelayHandler) handlePlayground(w http.ResponseWriter, r *http.Request) {
	suffix := strings.TrimPrefix(r.URL.Path, "/pg")
	if suffix == "" || !strings.HasPrefix(suffix, "/") {
		writeRelayError(w, http.StatusNotFound, "", "not_found", "unsupported playground endpoint")
		return
	}
	// Clone 而不是原地改写：外层中间件/日志仍持有原始请求，且 RequestURI
	// 必须清空——它在服务端请求里是只读的权威字段，留着 /pg 会让下游
	// 任何基于 RequestURI 的判断看到未重写路径。
	rewritten := r.Clone(r.Context())
	urlCopy := *r.URL
	urlCopy.Path = "/v1" + suffix
	urlCopy.RawPath = ""
	rewritten.URL = &urlCopy
	rewritten.RequestURI = ""

	target, pattern := h.playground.Handler(rewritten)
	if pattern == "" {
		writeRelayError(w, http.StatusNotFound, "", "not_found", "unsupported playground endpoint")
		return
	}
	target.ServeHTTP(w, rewritten)
}

// applyPlaygroundSession 为会话身份补分组并执行模型准入。
//
// 返回 (status, code, message, ok)；ok=true 时 rc 已完成身份补齐。
// 判定顺序即语义：身份/用户状态（401/500）→ 分组回源 → 模型准入（403，
// 发生在任何 PreConsume 之前，因此不产生资金变动或 usage_logs 行）。
//
// 模型准入只认**当前分组**的模型清单：会话请求没有分组选择器，路由也用
// rc.UsingGroup，若按「任一可用分组」放行会出现「页面允许、路由必失败」
// 的两套答案。默认模型校验（PUT /api/self/setting）则按 brief 冻结的
// 「全部可用分组」口径，两者差异是有意的。
func applyPlaygroundSession(
	ctx context.Context,
	groups PlaygroundUserGroups,
	principal *auth.Principal,
	rc *contract.RelayContext,
	model string,
) (int, string, string, bool) {
	if principal == nil || principal.TokenID != nil {
		return 0, "", "", true
	}
	if groups == nil {
		// 生产装配必须提供；缺失时 fail-closed，绝不把空分组当「全部可用」。
		return http.StatusInternalServerError, "database_error", "user group lookup is unavailable", false
	}
	group, err := groups.UserGroup(ctx, principal.UserID)
	if err != nil {
		if errors.Is(err, store.ErrUserNotFound) {
			// 会话指向已删除/停用用户：身份失效，与坏令牌同语义（401）。
			return http.StatusUnauthorized, "", "invalid token", false
		}
		return http.StatusInternalServerError, "database_error", "failed to resolve user group", false
	}
	relay.WithIdentity(rc, rc.RequestID, rc.UserID, rc.TokenID, group, group)

	available, err := groups.UsableGroupsForUser(ctx, principal.UserID)
	if err != nil {
		return http.StatusInternalServerError, "database_error", "failed to resolve available models", false
	}
	for _, candidate := range available {
		if candidate.Name != group {
			continue
		}
		for _, allowed := range candidate.Models {
			if allowed == model {
				return 0, "", "", true
			}
		}
	}
	return http.StatusForbidden, "model_not_available",
		"the requested model is not available in the current group", false
}

// ---------- POST /v1/chat/completions ----------

// rejectSensitiveContent 在 IR 解析后、PreConsume/上游之前执行 F-08-18 过滤。
//
// 返回 true 表示已写出 400 响应，调用方必须立即 return。设计口径
// （modules-08 F-08-18）：命中 → 400 + 错误码 content_blocked，**不透露命中词**。
// 因此错误文案固定、日志也不含命中词正文（只记事件名/作用面/请求 id）。
//
// 计费时序是硬约束：本函数必须在 routeDeps（其 Billing 字段会创建计费会话）
// 与 relay.Route 之前调用；block 路径因此零 PreConsume、零上游请求。
// action=log 时只记 warn 日志并放行（设计「记录日志放行」）。
func (h *RelayHandler) rejectSensitiveContent(req *contract.Request, w http.ResponseWriter,
	requestID string, userID int) bool {
	cfg := h.sensitiveConfig()
	if !cfg.Active() {
		return false
	}
	result := cfg.CheckRequest(req)
	if !result.Matched {
		return false
	}
	h.logSensitiveHit(requestID, userID, result.Surface, cfg.Action)
	if cfg.Action != sensitive.ActionBlock {
		return false
	}
	// 固定文案与 W14 的新入口共用同一个常量/写出函数，避免两处文案漂移。
	writeSensitiveBlocked(w, requestID)
	return true
}

func (h *RelayHandler) sensitiveConfig() sensitive.Config {
	return sensitiveConfigFor(h.sensitive, h.config)
}

// logSensitiveHit 写 security.sensitive_word_hit（warn，F-08-18）。
//
// 设计要求「不落明文命中词」：这里刻意只记事件名、作用面、动作与请求/用户
// id，不携带任何文本片段。requestID 同时作为 message 之外的独立字段，便于
// 运行日志按请求关联。
func (h *RelayHandler) logSensitiveHit(requestID string, userID int,
	surface sensitive.Surface, action sensitive.Action) {
	logSensitiveHitEvent(h.log, action, surface, requestID, int64(userID))
}

func (h *RelayHandler) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	requestID := newRelayRequestID()

	// 跨协议转发（M4 T5 / GC-8）：协议头只在这里解析一次，随后贯穿选路
	// （targetProtocol）与响应回写。判定顺序是冻结口径：
	//   1. 开关关闭且携带头 → 403（无论头值是否合法）：关闭状态不得静默
	//      降级为 OpenAI（Review Focus 5），也不透露「只是拼错了」；
	//   2. 开关开启但头值非法 → 400；
	//   3. 未携带头 → targetProtocol 为空，后续选路与 M2 完全一致。
	// 鉴权在中间件已完成，因此未认证请求永远先得到 401，不会被本分支抢答。
	targetProtocol, hasProtocolHeader, protocolErr := crossproto.TargetFromHeader(r.Header.Get(crossproto.HeaderProtocol))
	if hasProtocolHeader && !readConfigBool(h.config, crossproto.ConfigKeyEnabled, false) {
		writeRelayError(w, http.StatusForbidden, requestID, "cross_protocol_forward_disabled",
			"cross-protocol forwarding is not enabled")
		return
	}
	if protocolErr != nil {
		writeRelayError(w, http.StatusBadRequest, requestID, "invalid_protocol",
			"unsupported target protocol (expected openai, anthropic, or gemini)")
		return
	}

	body, err := readRelayBody(r)
	if err != nil {
		if errors.Is(err, errRelayBodyTooLarge) {
			writeRelayError(w, http.StatusRequestEntityTooLarge, requestID, "request_too_large",
				"request body exceeds the 32 MiB limit")
			return
		}
		writeRelayError(w, http.StatusBadRequest, requestID, "invalid_request", "failed to read request body")
		return
	}
	// 复原 body：relay.BuildRelayContext 的 peekStreaming 需要读请求体判定 stream
	// （它会把已读前缀拼回）。之后归一化层仍以 IR 的 Stream 为权威值（§7.2）。
	r.Body = io.NopCloser(bytes.NewReader(body))

	parsed, err := ir.ParseOpenAIRequest(body)
	if err != nil {
		writeRelayError(w, http.StatusBadRequest, requestID, "invalid_request", "invalid OpenAI request body")
		return
	}
	// finding 14（OpenAI stream bridge 丢弃 choice 身份）：stream=true 时
	// 下游 SSE 桥接只编码 index=0 的增量，n>1 会静默丢失其余候选。
	// 与其返回一个语义不完整的流，不如在入口 400 拒绝并明确说明。
	if parsed.Stream && requestChoiceCount(body) > 1 {
		writeRelayError(w, http.StatusBadRequest, requestID, "invalid_request",
			"stream=true with n>1 is not supported: the streaming bridge cannot preserve choice identity")
		return
	}

	principal, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		// 只有中间件未挂载才会到这里；fail-closed，绝不把零值身份当匿名用户。
		writeRelayError(w, http.StatusUnauthorized, requestID, "", "invalid token")
		return
	}
	rc, err := relay.BuildRelayContext(r.Context(), principal, r, h.tokens)
	if err != nil {
		status, code, message := mapRelayContextError(err)
		writeRelayError(w, status, requestID, code, message)
		return
	}
	// 归一化层是 Request/OriginModel/Streaming 的唯一写入方（relay-01 §7.2）。
	// 必须在 BuildRelayContext 之后写：它会先用 peekStreaming 填一个尽力值。
	relay.WithRequest(rc, parsed, ir.FormatOpenAIChat, r.URL.Path)

	// F-08-18：在 IR 归一化之后、PreConsume/上游之前过滤（见函数注释）。
	// 放在 BuildRelayContext 之后是为了日志能带上 user_id；此时尚未创建
	// 计费会话，block 路径仍然零计费。
	if h.rejectSensitiveContent(parsed, w, requestID, rc.UserID) {
		return
	}

	// /pg/* 会话路径（relay-01 §1.5）：BuildRelayContext 不查令牌，因此
	// UsingGroup 为空；必须回源 users."group" 再写身份组，否则候选查询会
	// 变成「所有分组」并把定价倍率落到错误分组。
	// 同一分支在计费前执行模型准入：模型不在用户当前分组的可用清单 →
	// 403 且不落任何计费/日志（modules-02 §2.15 入口 6 与 TC-F-02-19-06）。
	if principal.TokenID == nil {
		status, code, message, ok := applyPlaygroundSession(r.Context(), h.userGroups, principal, rc, parsed.Model)
		if !ok {
			writeRelayError(w, status, requestID, code, message)
			return
		}
	}

	// W7/M5-b 准入限流（R-M5-04）：位置固定在这里——BuildRelayContext 与分组
	// 解析之后（/pg 的 UsingGroup 此时才可用），pin/白名单/routeDeps/PreConsume
	// 与上游之前。被拒绝的请求因此零预扣、零 usage_logs、零上游调用；流式与
	// 非流式在分流前只计一次（不按 SSE 帧计）。
	if rejection := checkRelayRateLimit(r.Context(), h.rateLimiter, h.config, rc, principal, clientIP(r)); rejection != nil {
		writeRelayRateLimitError(w, rc.RequestID, rejection)
		return
	}

	// finding 1（relay-01 §3.5/§5.4-A）：pin 必须在 BuildRelayContext **之后**
	// 解析——后者已经完成了「非管理员使用 pin → 403」的统一校验（R-P2a）。
	// 提前解析会让会话路径的 pin 绕过那道校验。
	pinID, hasPin := relay.ParseChannelPin(r)

	// finding 6（relay-01 §3.4）：令牌模型白名单在路由阶段执行；pin 渠道
	// 优先级高于白名单（§3.4 注：pin 命中时跳过白名单）。非管理员 pin
	// 已在 BuildRelayContext 返回 403，因此这里 hasPin 等价于管理员 pin。
	if !hasPin && !tokenModelAllowed(principal.ModelLimits, parsed.Model) {
		writeRelayError(w, http.StatusForbidden, requestID, "distributor_token_model_forbidden",
			"this token is not allowed to access the requested model")
		return
	}

	// R-84 / modules-06 TC-F-06-13-01：管理员停用（status=2）的定价模型不得
	// 被用户请求命中，必须在派发（含 PreConsume）之前 404。无定价行的模型
	// 按默认价兜底，仍视为启用（found=false 放行）。
	//
	// 为什么不交给 pricing.DBSource：它对停用行刻意回退默认价（保留管理员
	// 事后重新启用的能力），不会拒绝请求；「停用即不可调用」是路由层语义。
	if h.modelStatus != nil {
		status, found, statusErr := h.modelStatus.ModelPricingStatus(r.Context(), parsed.Model)
		if statusErr != nil {
			// GC-6：存储错误可能含 DSN/SQL，只回固定文案。
			writeRelayError(w, http.StatusInternalServerError, requestID, "database_error",
				"failed to check model availability")
			return
		}
		if found && status == modelPricingStatusDisabled {
			writeRelayError(w, http.StatusNotFound, requestID, "model_not_found",
				"the requested model is not available")
			return
		}
	}

	// finding 13：预扣估算必须进入 RelayContext，否则 PreConsume 只能看到
	// 零值（MaxTokens=nil），402 判定与 Terminate 预算释放都成了空转。
	// PromptTokens 暂为 0：本地 token 估算器（relay-03 §7）尚未落地，
	// 调用方不得把 0 误读成「prompt 免费」——M3 估算器接入后在此填充。
	relay.WithQuotaEstimate(rc, &contract.QuotaEstimate{
		Model:     parsed.Model,
		Group:     rc.UsingGroup,
		MaxTokens: parsed.MaxTokens,
	})

	deps := h.routeDeps(rc, pinID, hasPin, targetProtocol)
	if parsed.Stream {
		h.serveStream(w, r, rc, deps)
		return
	}
	// finding 11（relay-03 §2.3）：非流式请求不得向上游发送 stream_options /
	// include_usage——OpenAI 会以未知参数拒绝，且该选项只对流式有意义。
	// 归一化层保留原始 Raw 供透传，故在非流式出口显式剥离。
	if cleaned := stripStreamOptions(rc.Request); cleaned != rc.Request {
		rc.Request = cleaned
	}
	h.serveJSON(w, r, rc, deps)
}

// ---------- POST /v1/responses ----------

// handleResponses 是 OpenAI Responses 创建端点（T2 / P-01 / R-W18-07）。
//
// 分发语义（冻结）：
//  1. 插件绑定优先：命中 protocols.openai_responses.supports=create 派生
//     的路由，或 manifest routes[]（方法 + 路径精确匹配）即转任务插件提交；
//     创建出口按 Responses 对象/SSE 渲染（handlers_responses.go）；
//  2. 未命中一律 fail-closed（501，OpenAI 错误信封）：通用中继**不**承接
//     Responses。
//
// 为什么插件命中后必须 return：任务提交路径已在 handleSubmit 内部完成一次
// PreConsume。若继续进入通用管线，同一请求会产生第二次预扣（两个计费会话、
// 两条用量事实）——「计费只发生一次」是 P-01 的硬约束。
//
// 为什么回退是 501 而不是「尽力转发」（R-13）：通用中继是 Chat 管线——
// adapters.OpenAI.BuildRequest 固定打上游 /chat/completions，且 IR 编码不
// 透传 openai_responses 的 Raw，Responses 的 input/instructions 会全部丢失。
// 若放行，客户端会拿到 200 + chat.completion（或 chat chunk SSE）并按与该
// 输入无关的 usage 被结算——「静默给错答案」比明确报错更糟，且会产生真实
// 扣费。因此这里在**任何计费/上游调用之前**拒绝，零副作用。
//
// 错误形状（W18-C 裁决）：create/retrieve 的错误统一用 /v1 的 OpenAI 信封
// （writeRelayError），不再使用 problem+json——同一端点的成功与失败不能是
// 两套协议。501 仍是「端点形态不支持」的状态码，只是信封换了。
//
// 鉴权已由 Register 的 RelayAuth 中间件完成（与 /v1/chat/completions、
// /v1/tasks 同一条链）：未认证请求先得到 401，不会看到本分支的 501。
func (h *RelayHandler) handleResponses(w http.ResponseWriter, r *http.Request) {
	requestID := newRelayRequestID()

	// R-03：复用 T1 的只读路由索引，不另建匹配逻辑。每次请求都取当前快照，
	// 让已停用的插件路由立即失效；索引未命中才进入 fail-closed 分支。
	// 协议派生绑定已由 plugin.registerRoutes 写进同一索引（R-W18-07），
	// 因此这里不需要再读 manifest.protocols。
	if h.nativeRoutes != nil {
		if match, ok := h.nativeRoutes.NativeRoutes().Match(r.Method, r.URL.Path); ok {
			h.nativeRoutes.DispatchNativeRoute(w, r, match)
			return
		}
	}

	// R-13：Responses 的通用回退已撤销。请求体刻意不读：不做任何解析/校验/
	// 计费，直接给出可操作的错误。
	writeRelayError(w, http.StatusNotImplemented, requestID, "endpoint_not_supported",
		"the generic relay cannot execute /v1/responses; declare protocols.openai_responses in an installed task plugin")
}

// problemTypeEndpointNotSupported 是「中继无法表达该端点形态」的 problem
// type URI（目前仅 /v1/messages 的异构上游方向门禁使用）。
//
// 沿用仓库既有的 `https://newtg.dev/problems/` 命名空间（与
// handlers_public_pricing.go 的 not-implemented 同源），不另造第二套命名；
// 值为 URI 是因为 Problem.type 在 OpenAPI 里声明为 format: uri。
const problemTypeEndpointNotSupported = "https://newtg.dev/problems/endpoint-not-supported"

// ---------- POST /v1/messages（Anthropic Messages 原生入口，W7/M1） ----------

// handleMessages 把 Claude 原生客户端（含 Claude Code）的 `/v1/messages`
// 接入中继管线。鉴权复用 Register 里的**同一个** RelayAuth（x-api-key 载体
// 已按路径在 relay_auth.go 归一化），计费复用**同一条** contract.BillingSession
// 接缝（h.billing），不另建计费路径。
//
// 方向矩阵（R-59，判定落在 relay_messages_test.go）：
//   - 入站 Anthropic → 上游 Anthropic 协议族渠道（type 14 anthropic、
//     type 25 moonshot，后者是 Anthropic 兼容协议）：**可转码**。
//     请求经 IR 重新编码（system/messages/max_tokens/tools/stop_sequences 都在
//     IR 建模范围内，Raw 作为未建模字段的透传基底）；响应与 SSE 是上游原生
//     帧，对同协议的 Claude 客户端逐字节正确。
//   - 入站 Anthropic → OpenAI / Gemini 原生渠道：**fail-closed 501**。
//
// 为什么 OpenAI/Gemini 上游不能「尽力转码」（R-59 与 R-13 同一口径）：请求
// 方向可以经 IR 表达，但**响应方向没有 IR → Anthropic 的编码器**——
// ir.FromIR 只产出 OpenAI Chat 形状，bridgeStream 也只渲染
// chat.completion.chunk。放行会让 Claude 客户端收到一份 OpenAI 形状的 200
// （或 OpenAI SSE），语义与它声明的协议无关，却按真实 usage 计费。这正是
// 「看起来正常但内容错误」，比明确报错更糟。等 ir 包补齐 Anthropic 响应
// 编码器后再放开（登记在 M1 报告）。
//
// 判定时点：方向由「请求模型 → 候选渠道」决定，必须先读到 model。请求体读入
// 之后、PreConsume 与任何上游调用之前完成判定：常规路径在构造 routeDeps 之前
// 就返回 501（连计费会话都不创建）；pin 指向异构渠道的 TOCTOU 兜底路径会先
// 构造一个无副作用的会话，但仍在 PreConsume 之前被拒绝。两条路径都零计费、
// 零上游请求。
func (h *RelayHandler) handleMessages(w http.ResponseWriter, r *http.Request) {
	requestID := newRelayRequestID()

	body, err := readRelayBody(r)
	if err != nil {
		if errors.Is(err, errRelayBodyTooLarge) {
			writeRelayError(w, http.StatusRequestEntityTooLarge, requestID, "request_too_large",
				"request body exceeds the 32 MiB limit")
			return
		}
		writeRelayError(w, http.StatusBadRequest, requestID, "invalid_request", "failed to read request body")
		return
	}
	// 复原 body：BuildRelayContext 的 peekStreaming 需要读请求体（stream 标志）。
	r.Body = io.NopCloser(bytes.NewReader(body))

	parsed, err := ir.ParseAnthropicRequest(body)
	if err != nil {
		// GC-6：解析器文本可能回显用户输入片段，对客户端只给固定文案。
		writeRelayError(w, http.StatusBadRequest, requestID, "invalid_request", "invalid Anthropic request body")
		return
	}
	// Anthropic Messages 把 max_tokens 列为**必填**。IR 解析器对缺省保持 nil，
	// 编码器（ir.BuildAnthropicRequestBody）会为跨协议调用补 8192——但那条
	// 默认路径只为「OpenAI 客户端省略 max_tokens」服务，不能让原生入口继承：
	// 缺省时 QuotaEstimate.MaxTokens 为 nil，免费模型的防滥用 hold 会退化为 0
	// （billing/session.go 的 preConsumeFreeModelLocked：MaxTokens 缺失/≤0 →
	// hold 0 → 不写流水），等于让免费模型的高成本请求零预扣（F-08-12）。
	// 因此在这里、PreConsume 与任何上游调用之前显式拒绝缺失、0 与负数。
	// 负数本会被 ir.validateScalars 拦住（同样是 400），这里统一口径。
	if parsed.MaxTokens == nil || *parsed.MaxTokens <= 0 {
		writeRelayError(w, http.StatusBadRequest, requestID, "invalid_request",
			"max_tokens is required and must be a positive integer")
		return
	}

	principal, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		// 只有中间件未挂载才会到这里；fail-closed，绝不把零值身份当匿名用户。
		writeRelayError(w, http.StatusUnauthorized, requestID, "", "invalid token")
		return
	}
	rc, err := relay.BuildRelayContext(r.Context(), principal, r, h.tokens)
	if err != nil {
		status, code, message := mapRelayContextError(err)
		writeRelayError(w, status, requestID, code, message)
		return
	}
	// 归一化层是 Request/OriginModel/Streaming 的唯一写入方（relay-01 §7.2）。
	relay.WithRequest(rc, parsed, ir.FormatAnthropicMessages, r.URL.Path)

	// F-08-18：与 chat 同一时点（IR 之后、PreConsume/上游之前）。/v1/messages
	// 的 system 已由解析器放进 Messages[0]（RoleSystem），因此同一个
	// CheckRequest 同时覆盖 prompt 与 system 两个作用面。
	if h.rejectSensitiveContent(parsed, w, requestID, rc.UserID) {
		return
	}

	// W7/M5-b 准入限流（R-M5-04）：敏感词过滤之后、pin/白名单/停用检查/预扣
	// 之前。原生入口没有 /pg 分支，因此不存在 TokenID==nil 的会话路径
	// （B11 恒参与）。
	if rejection := checkRelayRateLimit(r.Context(), h.rateLimiter, h.config, rc, principal, clientIP(r)); rejection != nil {
		writeRelayRateLimitError(w, rc.RequestID, rejection)
		return
	}

	// pin 必须在 BuildRelayContext 之后解析（R-P2a：非管理员 pin 已在那里
	// 统一拒绝）。/v1/messages 不挂 /pg 会话平面（RegisterPlayground 只暴露
	// 对话族），因此没有 TokenID==nil 的分组回源分支。
	pinID, hasPin := relay.ParseChannelPin(r)
	if !hasPin && !tokenModelAllowed(principal.ModelLimits, parsed.Model) {
		writeRelayError(w, http.StatusForbidden, requestID, "distributor_token_model_forbidden",
			"this token is not allowed to access the requested model")
		return
	}

	// R-84：停用模型必须在派发与预扣之前 404（与 chat 同口径）。
	if h.modelStatus != nil {
		status, found, statusErr := h.modelStatus.ModelPricingStatus(r.Context(), parsed.Model)
		if statusErr != nil {
			writeRelayError(w, http.StatusInternalServerError, requestID, "database_error",
				"failed to check model availability")
			return
		}
		if found && status == modelPricingStatusDisabled {
			writeRelayError(w, http.StatusNotFound, requestID, "model_not_found",
				"the requested model is not available")
			return
		}
	}

	// 与 chat 完全同一份预扣估算（finding 13）。准确口径：MaxTokens 只是
	// QuotaEstimate 的输入之一——付费 token 模型的预扣仍由 estimateUsage
	// 决定（当前为 0，不把 MaxTokens 换算成 Completion），它直接影响的是
	// **免费模型**的防滥用 hold（MaxTokens × multiplier）。上面的入口校验
	// 保证这里永远是非零的合法值。
	relay.WithQuotaEstimate(rc, &contract.QuotaEstimate{
		Model:     parsed.Model,
		Group:     rc.UsingGroup,
		MaxTokens: parsed.MaxTokens,
	})

	// 方向约束：只允许 Anthropic 协议族渠道（见上方方向矩阵）。
	//
	// 这里在构造 routeDeps（其 Billing 字段会新建计费会话）之前先判定方向：
	// 常规 501 分支因此既不创建计费会话、也不 PreConsume、更不触达上游。
	// 候选为空时不在这里下结论——那属于「没有可用渠道」（503），交给执行层
	// 按既有口径映射。
	//
	// TOCTOU 兜底：预检通过后，routeDeps 的候选过滤仍可能拒绝（典型是 pin
	// 指向 OpenAI 渠道、同时存在其它 Anthropic 候选）。那条路径会先由
	// routeDeps 构造一个计费会话，随后在 PreConsume 之前被过滤拒绝——资金
	// 副作用为 0：PreConsume/Terminate 从不发生；非流式兜底路径会对这个
	// 未预扣的会话调用一次 no-op Refund（真实引擎在没有 hold 时直接返回
	// nil，不写流水），流式路径则不调用。审计计数器的 Refund 可能为 1，
	// 但它不产生任何资金变动。
	// R-68：方向预检必须按**有效分组序列**判定——首组 + 开关开启时的后续组
	// （受 R-63 上限约束）。首组无 Anthropic、后续组有且开关开启时不得 501，
	// 交给执行层换组；只有所有有候选的组都不支持 Anthropic 才 501。
	if checked, supported := h.messagesDirectionCovered(r.Context(), rc); checked && !supported {
		h.writeMessagesMappedError(w, requestID, crossproto.ErrUpstreamProtocolUnsupported)
		return
	}

	deps := h.routeDeps(rc, pinID, hasPin, crossproto.ProtocolAnthropic)
	if parsed.Stream {
		h.serveNativeStream(w, r, rc, deps)
		return
	}
	h.serveNativeJSON(w, r, rc, deps)
}

// messagesDirectionCovered 按 R-68 的**有效分组序列**做方向预检。
//
// 返回 checked=false 表示有效序列中没有任何组存在候选（无法下方向结论，
// 交给执行层按 ErrNoAvailableChannel/换组处理）；checked=true 且
// supported=false 才表示「端点方向不支持」→ 501。这里与执行层共用
// relay.GroupSequence，避免预检与真实换组范围漂移。
func (h *RelayHandler) messagesDirectionCovered(ctx context.Context, rc *contract.RelayContext) (checked, supported bool) {
	for _, group := range relay.GroupSequence(rc, h.maxAutoGroups) {
		candidates, err := h.channels.Candidates(ctx, rc.OriginModel, group)
		if err != nil || len(candidates) == 0 {
			// 与既有单组口径一致：查询失败/空组不下方向结论，交给执行层。
			continue
		}
		checked = true
		if h.messagesDirectionSupported(ctx, candidates) {
			return true, true
		}
	}
	return checked, false
}

// messagesDirectionSupported 判定候选集里是否存在 Anthropic 协议族渠道。
//
// 渠道类型 → 协议族的映射只来自 adapters.DispatchTable（crossproto 的
// channelType 映射），不在这里另抄类型清单：新增 Anthropic 兼容渠道时方向
// 判定必须自动跟随分派表。Get 失败的候选按「不可用」跳过（与执行层换渠道
// 同口径）；真正的错误留给执行层映射。
func (h *RelayHandler) messagesDirectionSupported(ctx context.Context, candidates []contract.ChannelCandidate) bool {
	for _, candidate := range candidates {
		ch, err := h.channels.Get(ctx, int64(candidate.ChannelID))
		if err != nil {
			continue
		}
		if crossproto.ChannelTypeSupportsProtocol(contract.ChannelType(ch.Type), crossproto.ProtocolAnthropic) {
			return true
		}
	}
	return false
}

// writeMessagesMappedError 是 /v1/messages 的错误出口。
//
// 唯一的差异化映射是「没有 Anthropic 协议族渠道」：这是**端点的方向不支持**
// （决定是否放行的是协议族能力，不是这一次请求的参数），按 R-59 返回 501
// problem+json，与 /v1/responses 的 fail-closed 共用同一个 problem type。
// 资金副作用为 0：常规路径甚至不创建计费会话；pin 触发的 TOCTOU 兜底路径
// 只创建一个会话，PreConsume/Terminate 从不发生，非流式兜底会对未预扣会话
// 调用一次 no-op Refund（真实引擎无 hold 时直接返回 nil，不写流水）。其余
// 错误沿用 /v1 的统一 OpenAI 信封（RelayAuth 的 401/403 也是该形状，保持
// 同一条鉴权链的响应一致性）。
func (h *RelayHandler) writeMessagesMappedError(w http.ResponseWriter, requestID string, err error) {
	if errors.Is(err, crossproto.ErrUpstreamProtocolUnsupported) {
		if requestID != "" {
			w.Header().Set("X-Request-ID", requestID)
		}
		writeProblemDetail(w, http.StatusNotImplemented, problemTypeEndpointNotSupported,
			"Messages endpoint requires an Anthropic-protocol channel",
			"the generic relay can only answer /v1/messages through Anthropic/Moonshot channels; "+
				"this request performs no billing and no upstream call")
		return
	}
	writeRelayMappedError(w, requestID, err)
}

// serveNativeJSON 是非流式 /v1/messages 的执行与回写。
//
// 与 serveJSON 的差异只有一处：**不做响应方向跨协议转换**。方向约束已把
// 上游限定为 Anthropic 协议族，因此 resp.Raw 就是 Anthropic 客户端期望的
// 原生 JSON——逐字节透传（与 M2 的 OpenAI→OpenAI 透传同口径），而不是把
// 上游原生体交给 ir.FromIR（那会产出 chat.completion，客户端语义错误）。
// 计费仍由 relay.Route 在自己的 defer 里完成；这里包一层 deferredSettlement
// 只是复用 chat 的「编码/写失败 → 不收费」语义。
func (h *RelayHandler) serveNativeJSON(w http.ResponseWriter, r *http.Request, rc *contract.RelayContext, deps relay.RouteDeps) {
	var settlement *deferredSettlement
	if deps.Billing != nil {
		settlement = newDeferredSettlement(deps.Billing)
		deps.Billing = settlement
		defer settlement.abort()
	}
	resp, err := relay.Route(r.Context(), rc, deps)
	if err != nil {
		h.writeMessagesMappedError(w, rc.RequestID, err)
		return
	}
	// 防御性方向校验：过滤保证只会命中 Anthropic 协议族，但重试/计划复用
	// 不得让异构协议体悄悄透传（R-59）。未知协议也在这里拒绝。
	if protocol, known := crossproto.ProtocolOfChannelType(rc.ChannelType); !known || protocol != crossproto.ProtocolAnthropic {
		writeRelayError(w, http.StatusBadGateway, rc.RequestID, "upstream_error",
			"upstream protocol does not match this endpoint")
		return
	}
	if resp == nil || len(resp.Raw) == 0 {
		// 原生响应体缺失：宁可 502 也不回一份形状未知的 JSON。
		writeRelayError(w, http.StatusBadGateway, rc.RequestID, "upstream_error",
			"upstream returned an empty response")
		return
	}
	if settlement != nil {
		_ = settlement.commit()
	}
	w.Header().Set("Content-Type", "application/json")
	if rc.RequestID != "" {
		w.Header().Set("X-Request-ID", rc.RequestID)
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(resp.Raw)
}

// serveNativeStream 是流式 /v1/messages 的执行与桥接。
//
// 与 serveStream 的差异：客户端协议是 Anthropic，因此**不**走 OpenAI 的
// chat.completion.chunk 渲染，也不注入 stream_options.include_usage，
// 更不写 `data: [DONE]`（Anthropic SSE 以 `message_stop` 收尾）。
func (h *RelayHandler) serveNativeStream(w http.ResponseWriter, r *http.Request, rc *contract.RelayContext, deps relay.RouteDeps) {
	start := time.Now()
	result, err := relay.ExecuteAttempts(r.Context(), rc, deps)
	if err != nil {
		h.writeMessagesMappedError(w, rc.RequestID, err)
		return
	}
	defer func() { _ = result.Response.Body.Close() }()

	streamAdapter, ok := result.Adapter.(contract.StreamAdapter)
	if !ok {
		// 预扣已成功（拿到 2xx），失败出口必须退款（R-85）。
		if deps.Billing != nil {
			_ = deps.Billing.Refund()
		}
		writeRelayError(w, http.StatusBadGateway, rc.RequestID, "stream_unsupported",
			"upstream adapter does not support streaming")
		return
	}
	// 与 serveNativeJSON 同一道方向兜底（R-59）。
	if protocol, known := crossproto.ProtocolOfChannelType(rc.ChannelType); !known || protocol != crossproto.ProtocolAnthropic {
		if deps.Billing != nil {
			_ = deps.Billing.Refund()
		}
		writeRelayError(w, http.StatusBadGateway, rc.RequestID, "upstream_error",
			"upstream protocol does not match this endpoint")
		return
	}

	stream.SetSSEHeaders(w.Header())
	if rc.RequestID != "" {
		w.Header().Set("X-Request-ID", rc.RequestID)
	}
	sse := newRelaySSEWriter(w)
	w.WriteHeader(http.StatusOK)
	sse.flush()

	h.pipeNativeAnthropicStream(r, rc, result.Response.Body, result.Response.Header, streamAdapter, deps.Billing, sse, start)
}

// pipeNativeAnthropicStream 把 Anthropic 上游的 SSE 字节逐字节转给客户端，
// 同时用适配器的 StreamEvents 解析**同一份字节**做用量提取与终结计费。
//
// 为什么不是「直接 io.Copy」：计费需要 usage 事实，而 Chat 口径的桥接会把
// 事件重编码成 OpenAI 形状。这里用 io.TeeReader 把上游字节一分为二
// （客户端写出一份、适配器解析一份），内容零改写；适配器只作为**用量与结束
// 信号**的来源。这是同协议透传，不是「碰运气」：字节本来就是该协议合法的
// 响应流（与 M2 的 OpenAI 原始帧透传同一判据）。
//
// 异常语义与 bridgeStream 一致：客户端断开/上游读取失败/看门狗超时一律
// Refund（R-85），不伪造 `message_stop` 或任何收尾帧（relay-03 §5.2/§10.3）。
//
// 与 bridgeStream 的两处口径差异，均因字节级透传没有协议级分帧：
//   - SendCount 按空行边界计 SSE 帧（见 countFrames），ping 帧也计入；
//   - OutcomePartial 的判据是「已写出任何帧」，无法像语义桥接那样只数内容帧。
//     一个只发 ping 后中断的流会被记为 partial 而非 failed——两者都是异常
//     结束并全额退款，只影响审计字段。
func (h *RelayHandler) pipeNativeAnthropicStream(
	r *http.Request,
	rc *contract.RelayContext,
	upstream io.ReadCloser,
	upstreamHeader http.Header,
	adapter contract.StreamAdapter,
	billing contract.BillingSession,
	sse *relaySSEWriter,
	start time.Time,
) {
	var (
		accumulated  contract.Usage
		sawUsage     bool
		received     int
		sent         int
		softErrors   int
		firstTokenMS *int
		finished     bool
		upstreamFail bool
		clientGone   bool
		timedOut     atomic.Bool
	)

	var closeOnce sync.Once
	closeUpstream := func() { closeOnce.Do(func() { _ = upstream.Close() }) }

	terminated := false
	finalize := func(terminate func() error) {
		if terminated || billing == nil {
			return
		}
		terminated = true
		_ = terminate()
	}
	terminate := func(fact contract.UsageFact) {
		finalize(func() error { return billing.Terminate(fact) })
	}
	refund := func() {
		finalize(func() error { return billing.Refund() })
	}
	// GC-7 兜底：任何未走显式终结的出口（含 panic）都必须释放预留。
	defer refund()

	// 看门狗与 bridgeStream 同口径：块间静默超过 streamInactivity 即判超时，
	// 关闭上游 body 解除阻塞读。
	inactivity := h.streamInactivity
	if inactivity <= 0 {
		inactivity = streamInactivityTimeout
	}
	watchdog := time.AfterFunc(inactivity, func() {
		timedOut.Store(true)
		closeUpstream()
	})
	defer watchdog.Stop()
	touch := func() {
		if !timedOut.Load() {
			watchdog.Reset(inactivity)
		}
	}

	defer func() {
		if rec := recover(); rec != nil {
			watchdog.Stop()
			relay.WithStream(rc, &contract.StreamStatus{
				Phase:     contract.PhaseTerminated,
				EndReason: contract.EndPanic,
				Outcome:   relayStreamOutcome(sent),
				RecvCount: received,
				SendCount: sent,
			}, sent, received, firstTokenMS)
			refund()
		}
	}()

	// teeWriter 是 io.TeeReader 的写端：原始字节块直接作为客户端帧写出。
	// 写失败即客户端断开，返回错误会让 StreamEvents 的读取中止。
	//
	// SendCount 的口径是**SSE 帧数**（与 chat 桥接每个 EventRaw/合成帧记一次
	// 同义），不是写次数：tee 拿到的是上游字节块，可能含多帧、也可能在帧中间
	// 被截断。lineEmpty 跨块保留，帧边界落在两次 Read 之间也能正确计数。
	lineEmpty := false
	countFrames := func(p []byte) {
		for _, b := range p {
			switch b {
			case '\n':
				if lineEmpty {
					sent++
				}
				lineEmpty = true
			case '\r':
				// CRLF 的 \r 不改变行状态，由随后的 \n 收行。
			default:
				lineEmpty = false
			}
		}
	}
	teeWriter := writerFunc(func(p []byte) (int, error) {
		if len(p) == 0 {
			return 0, nil
		}
		if !sse.writeFrame(p) {
			clientGone = true
			return 0, io.ErrClosedPipe
		}
		countFrames(p)
		if firstTokenMS == nil {
			ms := int(time.Since(start).Milliseconds())
			firstTokenMS = &ms
		}
		return len(p), nil
	})

	rw := struct {
		io.Reader
		io.Writer
	}{Reader: io.TeeReader(upstream, teeWriter), Writer: io.Discard}

	adapter.StreamEvents(rc, rw)(func(ev contract.StreamEvent) bool {
		if r.Context().Err() != nil {
			clientGone = true
			return false
		}
		if clientGone {
			return false
		}
		if timedOut.Load() {
			upstreamFail = true
			return false
		}
		touch()
		received++

		switch ev.Kind {
		case contract.EventUsage:
			if ev.Usage != nil {
				mergeUsage(&accumulated, *ev.Usage)
				sawUsage = true
			}
		case contract.EventFinish:
			// 结束帧本身已由 tee 写出（Anthropic 的 message_stop）；这里只记录
			// 「上游正常收尾」以便 EndReason 判定。
			finished = true
		case contract.EventError:
			// 软错误（坏 JSON 块/上游 error 事件）原样到达客户端后继续；
			// 硬错误（Raw 为空：读取失败/超限）或软错误超限则终止。
			if len(ev.Raw) > 0 && softErrors < maxSoftStreamErrors {
				softErrors++
				return true
			}
			upstreamFail = true
			return false
		}
		return true
	})

	// 头部兜底（R-05 优先级 3）：事件流没给出任何非零用量时，读上游
	// `x-model-response-usage` 头；仍没有则由估计值兜底（Source=estimated）。
	if !sawUsage || !usageHasFact(accumulated) {
		extracted := usage.ExtractUsageMultimodal(&contract.Response{}, upstreamHeader, nil, rc.ChannelType)
		if extracted.Usage.Source == contract.UsageUpstreamExact {
			accumulated = extracted.Usage
			sawUsage = true
		}
	}

	abnormal := clientGone || upstreamFail || timedOut.Load()
	endReason := contract.EndUpstreamDone
	outcome := contract.OutcomeSuccess
	switch {
	case clientGone:
		endReason = contract.EndClientAbort
		outcome = relayStreamOutcome(sent)
	case timedOut.Load():
		endReason = contract.EndStreamTimeout
		outcome = relayStreamOutcome(sent)
	case upstreamFail:
		endReason = contract.EndUpstreamError
		outcome = relayStreamOutcome(sent)
	case !finished:
		endReason = contract.EndUpstreamEOF
		outcome = relayStreamOutcome(sent)
	}
	relay.WithStream(rc, &contract.StreamStatus{
		Phase:     contract.PhaseTerminated,
		EndReason: endReason,
		Outcome:   outcome,
		RecvCount: received,
		SendCount: sent,
	}, sent, received, firstTokenMS)

	if abnormal {
		refund()
		return
	}

	fact := contract.UsageFact{
		Model:        rc.OriginModel,
		Usage:        accumulated,
		LatencyMS:    int(time.Since(start).Milliseconds()),
		FirstTokenMS: firstTokenMS,
		Stream:       true,
		Attempts:     rc.AttemptIndex + 1,
	}
	if !usageHasFact(accumulated) {
		fact.Usage = estimatedUsageFor(rc.ChannelType)
	}
	terminate(fact)
}

// writerFunc 把函数适配为 io.Writer（io.TeeReader 的写端）。
type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// requestChoiceCount 读取 OpenAI 请求体的 n（缺省/非法为 1）。
// 只用于 finding 14 的「stream + n>1」拒绝，不进入 IR（契约未建模 n）。
//
// 用 json.Number/float64 而不是 *int：`2.0`、`2e0` 是合法 JSON 数字且语义
// 等于 2；解码到 int 会失败并静默回落成 1，从而绕过 stream+n>1 的入口拒绝
// （二轮复审 finding 4）。超出 float64 范围的合法数字（如 1e999）按 +Inf
// 处理——语义上必然 >1，同样拒绝。
func requestChoiceCount(body []byte) float64 {
	var payload struct {
		N *json.Number `json:"n"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || payload.N == nil {
		return 1
	}
	n, err := payload.N.Float64()
	if err != nil {
		return math.Inf(1)
	}
	if n < 1 {
		return 1
	}
	return n
}

// stripStreamOptions 返回剥离顶层 stream_options 的请求副本。
//
// 非流式路径专用（relay-03 §2.3「不支持该选项时不流式时清空」）。
// 入参不被修改：Raw 是透传基底，可能仍被其它路径引用。
func stripStreamOptions(req *contract.Request) *contract.Request {
	if req == nil || len(req.Raw) == 0 {
		return req
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(req.Raw, &obj); err != nil || obj == nil {
		return req
	}
	if _, ok := obj["stream_options"]; !ok {
		return req
	}
	delete(obj, "stream_options")
	raw, err := json.Marshal(obj)
	if err != nil {
		return req
	}
	out := *req
	out.Stream = false
	out.Raw = raw
	return &out
}

// tokenModelAllowed 判定请求模型是否满足令牌模型白名单（relay-01 §3.4）。
// 空白名单 = 不限；大小写精确匹配（模型名是协议标识符，不做归一化）。
func tokenModelAllowed(modelLimits []string, model string) bool {
	if len(modelLimits) == 0 {
		return true
	}
	for _, allowed := range modelLimits {
		if allowed == model {
			return true
		}
	}
	return false
}

// readRelayBody 读取并限幅请求体。多读 1 字节即可区分「恰好 32MiB」与「超限」。
func readRelayBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRelayBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxRelayBodyBytes {
		return nil, errRelayBodyTooLarge
	}
	return body, nil
}

// routeDeps 组装 relay.RouteDeps（控制器裁决 R-P4：依赖全部显式注入）。
//
// pinID/hasPin 来自 relay.ParseChannelPin（handleChatCompletions 在
// BuildRelayContext 成功后解析，finding 1）。pin 只写进候选约束，不写进
// RelayContext（R-P2a）；若 pin 目标不在候选集内，返回 relay.ErrChannelNotFound，
// 由 ExecuteAttempts 在 PreConsume **之前**快失败（HTTP 404）。
func (h *RelayHandler) routeDeps(rc *contract.RelayContext, pinID int, hasPin bool, targetProtocol crossproto.Protocol) relay.RouteDeps {
	// M4-a：唯一同时拥有 channelID/autoBan/class/status/body 的接缝在
	// 执行层；端点层只负责把生产 reporter 透传进去。reporter 为 nil 时保持
	// 回调 nil，执行层完全跳过判定（测试/降级装配零开销）。
	var onAttemptFailure relay.AttemptFailureFunc
	if h.attemptReporter != nil {
		onAttemptFailure = h.reportAttemptFailure
	}
	return relay.RouteDeps{
		Candidates: func(ctx context.Context, rc *contract.RelayContext) ([]contract.ChannelCandidate, error) {
			// 候选按**客户端请求模型**与令牌分组过滤（relay-01 §5；store 侧
			// 只返回 status=1 且 supported_models 命中该模型的行）。
			candidates, err := h.channels.Candidates(ctx, rc.OriginModel, rc.UsingGroup)
			if err != nil {
				return nil, err
			}
			// R-62/R-68：auto 请求某个分组过滤后为空时，若后面还有未尝试的
			// 分组，返回空候选让执行层换组，而不是把「本组不支持目标协议/无
			// 渠道」误报成端点不支持。hasLaterGroups 每轮按当前 UsingGroup 计算。
			allowEmpty := h.hasLaterGroup(rc)
			if !hasPin {
				return filterCandidatesByProtocol(ctx, h.channels, candidates, targetProtocol, allowEmpty)
			}
			pinned := false
			for i := range candidates {
				candidates[i].Constraints.PinChannelID = pinID
				candidates[i].Constraints.RetryMode = contract.PinRetrySingleAttempt
				if candidates[i].ChannelID == pinID {
					pinned = true
				}
			}
			if !pinned {
				// pin 不在候选集：被过滤（禁用/分组/模型）或根本不存在。
				// 绝不在这里按 pin 另取渠道，否则会绕过 §5.4 D 步的过滤兜底。
				return nil, fmt.Errorf("%w: channel %d", relay.ErrChannelNotFound, pinID)
			}
			return filterCandidatesByProtocol(ctx, h.channels, candidates, targetProtocol, allowEmpty)
		},
		ResolveChannel: func(ctx context.Context, channelID int) (relay.ChannelAssignment, error) {
			return h.resolveChannel(ctx, rc, channelID)
		},
		Dispatch:         h.dispatch,
		Billing:          h.billing(rc),
		Client:           h.client,
		MaxRetries:       relayMaxRetries,
		MaxAutoGroups:    h.maxAutoGroups,
		GroupConfigured:  h.groupConfigured,
		OnAttemptFailure: onAttemptFailure,
		// W10-METRICS：执行层产出事实，端点层负责收敛为指标标签。
		// 观察函数内部 recover/护栏，绝不向执行层返回错误或 panic。
		ObserveAttempt: func(ev relay.AttemptEvent) {
			// 成功尝试不带 error_class：ErrUnknown 是枚举零值，映射成
			// "unknown" 会让每次成功都递增 errors_total（A3 恒 100%）。
			className := ""
			if ev.Result != "ok" {
				className = relay.ErrorClassName(ev.ErrorClass)
			}
			metrics.ObserveRelayAttempt(ev.Endpoint, ev.Model, ev.Stream, ev.ChannelID,
				ev.ChannelType, ev.FromType, ev.Result,
				className, ev.AttemptIndex, ev.Duration)
		},
	}
}

// reportAttemptFailure 把执行层的失败事实转交 reporter（M4-a）。
//
// 单独一层方法而不是直接把接口方法塞进 RouteDeps：nil reporter 必须退化为
// nil 回调，让执行层完全跳过判定（测试/降级装配不引入额外开销）；同时把
// 「reporter 可热替换」限制在 handler 内部，RouteDeps 的签名保持简单。
func (h *RelayHandler) reportAttemptFailure(ctx context.Context, channelID int, autoBan bool,
	class contract.ErrorClass, statusCode int, body []byte) {
	if h == nil || h.attemptReporter == nil {
		return
	}
	h.attemptReporter.ReportAttemptFailure(ctx, channelID, autoBan, class, statusCode, body)
}

// hasLaterGroup 报告当前分组之后是否还有未尝试的有效分组（R-62/R-63）。
// 端点层的协议过滤与方向预检共用 relay.GroupSequence，保证「这里认为可以
// 换组」与「执行层真正会换组」是同一序列。
func (h *RelayHandler) hasLaterGroup(rc *contract.RelayContext) bool {
	groups := relay.GroupSequence(rc, h.maxAutoGroups)
	for i, group := range groups {
		if group == rc.UsingGroup {
			return i < len(groups)-1
		}
	}
	return false
}

// filterCandidatesByProtocol 按目标上游协议过滤候选（跨协议转发）。
//
// targetProtocol 为空 = 未携带协议头：原样返回，保证 M2 选路行为逐字节不变
// （只有候选切片本身，未排序、未改写）。
//
// allowEmpty 是 R-62/R-68 的跨组语义：auto 请求当前分组过滤后为空、但序列中
// 还有后续分组时，返回空候选让执行层换组，而不是报 ErrUpstreamProtocolUnsupported
// 把整笔请求快失败。只有当前分组是最后一个可尝试分组时才保持原有的显式失败。
//
// 过滤必须在 PreConsume 之前完成（本函数位于 Candidates 回调内，执行层在
// 预扣前调用）：目标协议没有可用渠道时直接显式失败，不产生任何计费副作用。
//
// 为什么逐个 Get 渠道类型而不是让 store 查询时过滤：Candidates 的存储面
// （RelayChannelStore）是冻结的，且协议族映射的唯一真源在 adapters 分派表；
// 在路由层做这一层过滤可以避免把「渠道类型 → 协议」的知识泄漏进 store。
// 候选规模是单个模型 + 分组，通常个位数，额外点查询有界。
func filterCandidatesByProtocol(
	ctx context.Context,
	channels RelayChannelStore,
	candidates []contract.ChannelCandidate,
	targetProtocol crossproto.Protocol,
	allowEmpty bool,
) ([]contract.ChannelCandidate, error) {
	if targetProtocol == "" {
		return candidates, nil
	}
	filtered := make([]contract.ChannelCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		ch, err := channels.Get(ctx, int64(candidate.ChannelID))
		if err != nil {
			// F4/N1：非 pin 候选的解析失败（索引陈旧/渠道已删）不应拖垮
			// 整个请求——与执行层「这一轮不可用就换下一个候选」同口径，
			// 跳过即可。pin 候选是客户端的显式约束：解析失败必须上抛，
			// 否则会静默丢掉 pin（后续会以「目标协议无渠道」伪装成 400）。
			if isPinnedCandidate(candidate) {
				return nil, err
			}
			continue
		}
		if crossproto.ChannelTypeSupportsProtocol(contract.ChannelType(ch.Type), targetProtocol) {
			filtered = append(filtered, candidate)
			continue
		}
		if isPinnedCandidate(candidate) {
			// pin 渠道本身不属于目标协议：这是「目标协议无可用渠道」的
			// 特例。不能把带 pin 约束的候选集交给执行层——那时 pin 渠道
			// 已被过滤掉，preferPinned 会报 404 channel_not_found，把
			// 「协议不支持」误报成「渠道不存在」（F6）。
			return nil, crossproto.ErrUpstreamProtocolUnsupported
		}
	}
	if len(filtered) == 0 {
		if allowEmpty {
			// R-62/R-68：本组没有目标协议渠道，但序列里还有下一组。
			// 空计划让执行层把本组视为耗尽并换组；若最终全部为空，
			// 会以 ErrNoAvailableChannel / 最后一组的显式失败收尾。
			return nil, nil
		}
		// 显式失败而不是回退到任意协议：客户端声明了目标协议，静默换协议
		// 会让它按错误的响应语义解析（Review Focus 5 / GC-8）。
		return nil, crossproto.ErrUpstreamProtocolUnsupported
	}
	return filtered, nil
}

// isPinnedCandidate 判定候选是否就是 pin 指向的那一个渠道（N1）。
//
// routeDeps 在 pin 路径会把 PinChannelID 写到**所有**候选上（执行层据此
// 知道「本轮计划带 pin 约束」），因此 `PinChannelID != 0` 对每个候选都成立，
// 不能用作「这是 pin 候选」的判据。真正的 pin 候选必须同时满足
// ChannelID == PinChannelID；其余候选只是同批次的普通候选，协议不匹配或
// 解析失败时应按普通候选处理（过滤/跳过），而不是让整个请求 fail-fast。
func isPinnedCandidate(candidate contract.ChannelCandidate) bool {
	pinID := candidate.Constraints.PinChannelID
	return pinID != 0 && candidate.ChannelID == pinID
}

// resolveChannel 把 store.Channel 投影为执行层需要的整组渠道字段（relay-01 §7.4）。
//
// GroupRatio 在 M2 固定 1.0：分组倍率属于计费定价（M3），中继只持有与透传
// （GC-3）；填 1.0 而不是 0，避免下游把零值误读成「免费」。
func (h *RelayHandler) resolveChannel(ctx context.Context, rc *contract.RelayContext, channelID int) (relay.ChannelAssignment, error) {
	ch, err := h.channels.Get(ctx, int64(channelID))
	if err != nil {
		return relay.ChannelAssignment{}, err
	}
	creds, err := h.openChannelCredentials(ch)
	if err != nil {
		return relay.ChannelAssignment{}, err
	}
	apiBase := ""
	if ch.BaseURL != nil {
		apiBase = *ch.BaseURL
	}
	return relay.ChannelAssignment{
		ChannelID:     channelID,
		ChannelType:   contract.ChannelType(ch.Type),
		APIBase:       apiBase,
		Credential:    creds.APIKey,
		UpstreamModel: modelMappingTarget(ch.ModelMapping, rc.OriginModel),
		GroupRatio:    1.0,
		CreatedTime:   ch.CreatedAt,
		// M4-a：渠道级自动禁用开关随整组投影进执行层。每次 Get 都是当前
		// 行值，因此管理员在请求进行中关闭 auto_ban 最多影响下一轮解析。
		AutoBan: ch.AutoBan,
	}, nil
}

// openChannelCredentials 解封渠道凭据。失败统一返回不含细节的哨兵（GC-6/GC-8）。
func (h *RelayHandler) openChannelCredentials(ch *store.Channel) (ChannelCredentials, error) {
	if ch == nil || len(ch.Credentials) == 0 {
		return ChannelCredentials{}, errRelayCredentialMissing
	}
	plaintext, err := h.sealer.Open(ch.Credentials)
	if err != nil {
		return ChannelCredentials{}, errRelayCredentialInvalid
	}
	var creds ChannelCredentials
	if err := json.Unmarshal(plaintext, &creds); err != nil || creds.APIKey == "" {
		return ChannelCredentials{}, errRelayCredentialInvalid
	}
	return creds, nil
}

// modelMappingTarget 返回 model_mapping 中「客户端模型 → 上游模型」的映射值；
// 未配置/不是字符串时返回空串，适配器会回落到客户端模型名（relay-02 §8.1）。
func modelMappingTarget(raw []byte, model string) string {
	if len(raw) == 0 || model == "" {
		return ""
	}
	var mapping map[string]string
	if err := json.Unmarshal(raw, &mapping); err != nil {
		return ""
	}
	return mapping[model]
}

// ---------- 非流式 ----------

// deferredSettlement 把 Route 的成功结算延后到响应编码完成之后（F3）。
//
// 为什么需要：Route 在解析完上游响应后立即 Terminate；但跨协议转换
// （ir.FromIR）发生在 Route 返回之后。如果转换失败，客户端收到 502 却已经
// 被扣费——「没有交付服务却收费」是 fail-open 的计费漏洞。
//
// 该包装把 Terminate 的事实暂存，由端点层在 Forward 成功后 commit；
// 转换失败/写响应前 panic 时由 defer abort 触发真正的 Refund（此时内层
// 会话尚未 Terminate，退款有效——直接对已 Terminate 的会话调用 Refund
// 在真实引擎里是 no-op）。失败路径（Route 内部 Refund）保持委托语义。
type deferredSettlement struct {
	inner contract.BillingSession

	mu        sync.Mutex
	fact      *contract.UsageFact
	finalized bool
}

var _ contract.BillingSession = (*deferredSettlement)(nil)

func newDeferredSettlement(inner contract.BillingSession) *deferredSettlement {
	return &deferredSettlement{inner: inner}
}

func (d *deferredSettlement) PreConsume(est contract.QuotaEstimate) error {
	return d.inner.PreConsume(est)
}
func (d *deferredSettlement) Reserve(delta int64) error { return d.inner.Reserve(delta) }
func (d *deferredSettlement) ChargeIncrement(delta int64, fact contract.UsageFact) error {
	return d.inner.ChargeIncrement(delta, fact)
}
func (d *deferredSettlement) Reserved() int64   { return d.inner.Reserved() }
func (d *deferredSettlement) NeedsRefund() bool { return d.inner.NeedsRefund() }
func (d *deferredSettlement) RecoverAbandoned(requestID string) error {
	return d.inner.RecoverAbandoned(requestID)
}

func (d *deferredSettlement) Terminate(fact contract.UsageFact) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.finalized {
		return nil
	}
	copied := fact
	d.fact = &copied
	return nil
}

func (d *deferredSettlement) Refund() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.finalized {
		return nil
	}
	d.finalized = true
	d.fact = nil
	return d.inner.Refund()
}

// commit 在响应成功编码后落地结算。
func (d *deferredSettlement) commit() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.finalized {
		return nil
	}
	d.finalized = true
	if d.fact == nil {
		// Route 成功却没有给出事实（不应发生）：退款而不是零值结算。
		return d.inner.Refund()
	}
	return d.inner.Terminate(*d.fact)
}

// abort 是 defer 兜底：未 commit 的会话一律退款。
func (d *deferredSettlement) abort() { _ = d.Refund() }

func (h *RelayHandler) serveJSON(w http.ResponseWriter, r *http.Request, rc *contract.RelayContext, deps relay.RouteDeps) {
	// W10-METRICS：最终结果与端到端延迟。status 先按 success，所有错误出口
	// 在返回前置 error；观察发生在 defer，不影响响应写出。
	start := time.Now()
	status := "success"
	defer func() {
		metrics.ObserveRelayRequest(relay.EndpointLabel(rc), rc.OriginModel, false, status, time.Since(start))
	}()

	var settlement *deferredSettlement
	if deps.Billing != nil {
		settlement = newDeferredSettlement(deps.Billing)
		deps.Billing = settlement
		// 转换失败/编码 panic 都不得留下已扣费但未交付的请求（F3）。
		defer settlement.abort()
	}
	resp, err := relay.Route(r.Context(), rc, deps)
	if err != nil {
		status = "error"
		writeRelayMappedError(w, rc.RequestID, err)
		return
	}

	// 响应方向跨协议回写（GC-2）：上游是 Anthropic/Gemini 原生协议时，
	// resp.Raw 是上游原始 JSON，绝不能直接透传给 OpenAI 兼容客户端。
	// 目标协议取**实际命中渠道**的类型（而不是协议头）：重试可能换渠道，
	// 头只约束协议族，最终响应格式由最后一个成功渠道决定；二者在正常路径
	// 上一致（选路阶段已按头过滤），这里以执行结果为权威。
	upstreamProtocol, known := crossproto.ProtocolOfChannelType(rc.ChannelType)
	if !known {
		// 未实现的协议族不会走到这里（分派会先失败）；防御性按 OpenAI 透传。
		upstreamProtocol = crossproto.ProtocolOpenAI
	}
	resp, err = crossproto.Forward(r.Context(), rc, string(upstreamProtocol), resp)
	if err != nil {
		// F3：客户端拿不到响应 → 不收费。defer 的 abort 会在内层会话尚未
		// Terminate 时真正退款（提交前 finalized 仍为 false）。
		status = "error"
		writeRelayError(w, http.StatusBadGateway, rc.RequestID, "upstream_error",
			"failed to encode the upstream response")
		return
	}
	if settlement != nil {
		// 结算失败与 Route 同口径：不阻断已编码的响应。
		_ = settlement.commit()
	}

	w.Header().Set("Content-Type", "application/json")
	if rc.RequestID != "" {
		w.Header().Set("X-Request-ID", rc.RequestID)
	}
	w.WriteHeader(http.StatusOK)
	if resp != nil && len(resp.Raw) > 0 {
		// 上游原始响应体逐字节透传（relay-03 的非流式路径不重编码）。
		_, _ = w.Write(resp.Raw)
		return
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// ---------- 流式 ----------

// includeUsageRequested 判定是否向上游索要并向下游合成 include_usage（relay-03 §2.3）：
//   - stream_options.include_usage 缺省 → true（OpenAI Chat 的默认语义）；
//   - 显式 true/false → 尊重客户端选择（客户端显式 false 优先于默认值）。
//
// 只看归一化请求的 Raw：ir.ParseOpenAIRequest 已把它保存为完整原始体。
func includeUsageRequested(req *contract.Request) bool {
	if req == nil || len(req.Raw) == 0 {
		return true
	}
	var payload struct {
		StreamOptions *struct {
			IncludeUsage *bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	if err := json.Unmarshal(req.Raw, &payload); err != nil {
		return true
	}
	if payload.StreamOptions == nil || payload.StreamOptions.IncludeUsage == nil {
		return true
	}
	return *payload.StreamOptions.IncludeUsage
}

// serveStream 执行流式中继（relay-01 §2 阶段 7–14；relay-03 §3–§5）。
//
// 与 Route 的分工（R-P7）：流式路径先 ExecuteAttempts 拿原始 2xx 响应，再交给
// 适配器的 StreamEvents 事件流；ExecuteAttempts 在失败出口已 Terminate（GC-7），
// 因此本函数只在**拿到 2xx 之后**负责终结计费会话，且恰好一次。
func (h *RelayHandler) serveStream(w http.ResponseWriter, r *http.Request, rc *contract.RelayContext, deps relay.RouteDeps) {
	// 指标计时覆盖完整请求（含 body 解析/注入）；usage 的 LatencyMS 仍以
	// ExecuteAttempts 前的 start 为基准（保持 M2 口径不变，M-10）。
	requestStart := time.Now()
	status := "success"
	defer func() {
		metrics.ObserveRelayRequest(relay.EndpointLabel(rc), rc.OriginModel, true, status, time.Since(requestStart))
		// TTFT 只在流式路径且确实拿到首块时存在（非流式为 nil）。
		if rc.FirstTokenMS != nil {
			metrics.ObserveRelayFirstToken(relay.EndpointLabel(rc), rc.OriginModel,
				time.Duration(*rc.FirstTokenMS)*time.Millisecond)
		}
	}()

	// relay-03 §2.3：include_usage 默认 true，但客户端显式 false 优先。
	// 仅当需要时才注入 stream_options.include_usage=true（Raw 的其它字段保留）；
	// 客户端显式 false 时原样透传，不注入、也不合成末块 usage。
	includeUsage := includeUsageRequested(rc.Request)
	injected := usage.InjectIncludeUsage(rc.Request, includeUsage)
	if injected == nil {
		status = "error"
		writeRelayError(w, http.StatusBadRequest, rc.RequestID, "invalid_request", "invalid request body")
		return
	}
	// R-P14：流式路径强制 stream=true（即使上游请求体里残留 stream=false）。
	injected.Stream = true
	rc.Request = injected
	rc.Streaming = true

	// start 覆盖连上游在内的完整请求时长：LatencyMS / FirstTokenMS 都相对
	// 客户端请求开始计（与 relay.Route 的 start 口径一致）。
	start := time.Now()
	result, err := relay.ExecuteAttempts(r.Context(), rc, deps)
	if err != nil {
		status = "error"
		writeRelayMappedError(w, rc.RequestID, err)
		return
	}
	defer func() { _ = result.Response.Body.Close() }()

	streamAdapter, ok := result.Adapter.(contract.StreamAdapter)
	if !ok {
		// 预扣已成功（ExecuteAttempts 返回 2xx），必须释放预算再拒绝。
		// R-85：这是失败出口，走 Refund（Terminate(零事实) 会让按次模型扣费）。
		if deps.Billing != nil {
			_ = deps.Billing.Refund()
		}
		status = "error"
		writeRelayError(w, http.StatusBadGateway, rc.RequestID, "stream_unsupported",
			"upstream adapter does not support streaming")
		return
	}

	// 必须在 WriteHeader 之前设置响应头（relay-03 §3.1）。
	stream.SetSSEHeaders(w.Header())
	if rc.RequestID != "" {
		w.Header().Set("X-Request-ID", rc.RequestID)
	}
	sse := newRelaySSEWriter(w)
	w.WriteHeader(http.StatusOK)
	sse.flush()

	status = h.bridgeStream(r, rc, result.Response.Body, result.Response.Header, streamAdapter, deps.Billing, sse, start, includeUsage)
}

// forwardRawStreamEvent 判定适配器的原始 SSE 帧是否可以逐字节转发给客户端。
//
// 客户端入口固定 OpenAI 兼容，因此只有「上游协议族 = OpenAI」时原始帧才与
// 客户端协议同形。Anthropic/Gemini 的 EventRaw 是原生帧，混入 OpenAI SSE
// 会让客户端收到无法解析的 `event:`/原生 JSON（N2）。未知渠道类型按不可
// 转发处理（fail-closed）：宁可少一帧诊断信息，也不泄漏异种协议载荷。
func forwardRawStreamEvent(channelType contract.ChannelType) bool {
	protocol, ok := crossproto.ProtocolOfChannelType(channelType)
	return ok && protocol == crossproto.ProtocolOpenAI
}

// bridgeStream 消费适配器的 StreamEvent 序列并写入客户端。
//
// 语义（relay-03 §3–§9、relay-01 §7.6）：
//   - EventRaw：原始 SSE 帧逐字节转发（补足帧结束空行，帧内容不改写）；
//   - EventDelta/ToolDelta/Reasoning：重编码为 OpenAI chat.completion.chunk；
//   - EventUsage：非零合并累计（不擦除已有维度）；
//   - 若整个流都没有 EventUsage/finish usage，回落到上游 `x-model-response-usage`
//     响应头（R-05 优先级 3，二轮复审 finding 5）；
//   - EventFinish：[DONE] 之前写出 include_usage 末块；整个响应对客户端只写一次
//     data: [DONE]；
//   - EventError：软错误记录后继续；硬错误只终止流，绝不伪造 [DONE]（GC-6）。
//
// 健壮性（relay-03 §5.4/§10.1，M2 final review finding 4）：
//   - 看门狗：块间不活动超过 streamInactivity（默认 300s）→ EndReason=timeout，
//     关闭上游 body 以解除阻塞读；
//   - 心跳：每 10s 写 `: PING\n\n` 注释帧（等待期与传输期共用），写失败按客户端
//     断开处理并关闭上游；
//   - panic 恢复：EndReason=EndPanic，进程不崩；
//   - 计费（GC-7/R-85）：2xx 之后会话已开启；正常出口 Terminate(final fact)，
//     任何异常出口（断开/上游错误/超时/panic）一律 Refund；terminated 守卫
//     保证恰好一次。
//
// 异常结束（客户端断开/上游错误/超时/panic）不写收尾 usage 块与 [DONE]：
// relay-03 §5.2/§10.3 要求由连接终止表达异常，伪造正常结束标记会误导客户端。
//
// 返回值是 W10-METRICS 的最终状态：正常结算为 success，异常退款为 refunded。
// 调用方（serveStream）据此写 relay_requests_total；不能只看「HTTP 已 200」
// 就记 success（SSE 的失败发生在 200 之后）。
func (h *RelayHandler) bridgeStream(
	r *http.Request,
	rc *contract.RelayContext,
	upstream io.ReadCloser,
	upstreamHeader http.Header,
	adapter contract.StreamAdapter,
	billing contract.BillingSession,
	sse *relaySSEWriter,
	start time.Time,
	includeUsage bool,
) (streamStatus string) {
	streamStatus = "success"
	chunkID := "chatcmpl-newtg"
	if rc.RequestID != "" {
		chunkID = "chatcmpl-" + rc.RequestID
	}
	created := start.Unix()
	model := rc.OriginModel

	var (
		accumulated  contract.Usage
		sawUsage     bool
		received     int
		sent         int
		contentSent  int // 仅内容帧（raw/delta）：OutcomePartial 的判据（契约 §7.2）
		softErrors   int
		firstTokenMS *int
		finished     bool
		upstreamFail bool
		clientGone   bool
	)
	// 看门狗与 ping 协程会并发写这两个标志；事件循环只读。
	var timedOut atomic.Bool
	var pingFailed atomic.Bool

	// 上游 body 可能被看门狗、ping 失败与 serveStream 的 defer 多处关闭：
	// 统一经 sync.Once 收敛，避免并发 Close 的竞争。
	var closeUpstreamOnce sync.Once
	closeUpstream := func() { closeUpstreamOnce.Do(func() { _ = upstream.Close() }) }

	terminated := false
	finalize := func(terminate func() error) {
		if terminated || billing == nil {
			return
		}
		terminated = true
		_ = terminate()
	}
	terminate := func(fact contract.UsageFact) {
		finalize(func() error { return billing.Terminate(fact) })
	}
	// R-85：失败/取消/panic 是整笔不收费语义。真实引擎的 Refund 会全额退回
	// hold 与增量，并把 usage_logs 收敛为 refunded；Terminate(零事实) 会让
	// 按次/常量表达式模型对失败流照常扣费并记 success。
	refund := func() {
		finalize(func() error { return billing.Refund() })
	}
	// GC-7 兜底：任何未走显式终结的出口（含 panic）也必须释放预留。
	// 失败语义优先（R-85）：兜底一律退款，不结算。
	defer refund()

	// —— 看门狗（块间不活动超时）——
	inactivity := h.streamInactivity
	if inactivity <= 0 {
		inactivity = streamInactivityTimeout
	}
	watchdog := time.AfterFunc(inactivity, func() {
		timedOut.Store(true)
		// 关闭 body 让阻塞中的上游 Read 立即返回错误，事件循环才能退出。
		closeUpstream()
	})
	defer watchdog.Stop()
	touch := func() {
		// timedOut 已置位时不再续期：避免把已判超时的流又「救活」。
		if !timedOut.Load() {
			watchdog.Reset(inactivity)
		}
	}

	// —— SSE 心跳（等待首字节与流式传输期都发送）——
	// 写端与事件循环共享 relaySSEWriter 的互斥锁，帧不会交错。
	pingStop := make(chan struct{})
	pingDone := make(chan struct{})
	go func() {
		defer close(pingDone)
		ticker := time.NewTicker(streamPingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-pingStop:
				return
			case <-r.Context().Done():
				return
			case <-ticker.C:
				if !sse.ping() {
					// 写失败 = 客户端已断开；置位并关闭上游解除阻塞读。
					pingFailed.Store(true)
					closeUpstream()
					return
				}
			}
		}
	}()
	var stopPingOnce sync.Once
	stopPing := func() {
		stopPingOnce.Do(func() {
			close(pingStop)
			<-pingDone
		})
	}
	defer stopPing()

	// —— panic 恢复 ——
	// 注册在兜底 refund 之后（LIFO：本 defer 先执行），显式写入 EndPanic
	// 并退款；兜底 refund 随后因 terminated 守卫成为 no-op。
	defer func() {
		if rec := recover(); rec != nil {
			stopPing()
			watchdog.Stop()
			relay.WithStream(rc, &contract.StreamStatus{
				Phase:     contract.PhaseTerminated,
				EndReason: contract.EndPanic,
				Outcome:   relayStreamOutcome(contentSent),
				RecvCount: received,
				SendCount: sent,
			}, sent, received, firstTokenMS)
			// R-85：panic 是失败出口，整笔退款；不构造/上报用量事实
			// （abnormal 的「部分用量」不再参与计费）。
			refund()
			streamStatus = "refunded"
		}
	}()

	writeData := func(payload []byte) bool {
		if !sse.data(payload) {
			clientGone = true
			return false
		}
		sent++
		return true
	}
	recordFirstToken := func() {
		if firstTokenMS != nil {
			return
		}
		ms := int(time.Since(start).Milliseconds())
		firstTokenMS = &ms
	}

	// StreamEvents 接受 io.ReadWriter（契约 §3.1 v1.1 决议 D3：SSE 与 WS 共用）。
	// SSE 是单向流，写端用 io.Discard 占位。
	rw := struct {
		io.Reader
		io.Writer
	}{Reader: upstream, Writer: io.Discard}

	adapter.StreamEvents(rc, rw)(func(ev contract.StreamEvent) bool {
		// 异常优先判定：客户端断开 / ping 写失败 / 看门狗超时。
		// 即使适配器把取消包装成 EventError，也按对应异常处理，而不是上游错误。
		if r.Context().Err() != nil {
			clientGone = true
			return false
		}
		if pingFailed.Load() {
			clientGone = true
			return false
		}
		if timedOut.Load() {
			upstreamFail = true
			return false
		}
		touch()
		received++

		switch ev.Kind {
		case contract.EventRaw:
			if len(ev.Raw) == 0 {
				return true
			}
			// N2 / F2（T2 复审交接）：原生适配器用 EventRaw 承载无法语义化的
			// 载荷（Anthropic signature_delta/redacted_thinking/未知块、
			// Gemini inlineData 等）。当前客户端协议固定为 OpenAI：
			//   - 纯元数据帧（签名/引用）OpenAI 无字段可放 → 丢弃；
			//   - 内容载荷无法用 OpenAI SSE 表达 → 按连接级失败终止（不写
			//     [DONE]、退款），绝不逐字节透传或静默丢内容。
			if !forwardRawStreamEvent(adapter.Type()) {
				upstreamProtocol, known := crossproto.ProtocolOfChannelType(adapter.Type())
				if !known {
					// 未知协议无法判定载荷语义：fail-closed。
					upstreamProtocol = ""
				}
				if crossproto.ClassifyNativeRawEvent(upstreamProtocol, ev.Raw) == crossproto.RawEventUnsupported {
					rc.Diagnostics = append(rc.Diagnostics, contract.DiagnosticEntry{
						Stage:   "cross_protocol_stream",
						Message: "native raw event cannot be represented in an OpenAI stream",
					})
					upstreamFail = true
					return false
				}
				return true
			}
			if !sse.raw(ev.Raw) {
				clientGone = true
				return false
			}
			sent++
			contentSent++
			return true

		case contract.EventDelta:
			recordFirstToken()
			payload, err := marshalRelayDeltaChunk(chunkID, created, model, ev)
			if err != nil {
				upstreamFail = true
				return false
			}
			if !writeData(payload) {
				return false
			}
			contentSent++
			return true

		case contract.EventUsage:
			if ev.Usage != nil {
				mergeUsage(&accumulated, *ev.Usage)
				sawUsage = true
			}
			return true

		case contract.EventFinish:
			// finish_reason 是客户端可见语义（stop/length/tool_calls），不能只记布尔：
			// 从原始帧解析后写一个 delta 为空、finish_reason 非空的 chunk。
			// ev.Raw 可能是 [DONE] 哨兵帧（适配器在未见 finish_reason 时补发），
			// 因此必须解析而不是盲目转发。
			finished = true
			reason, finishUsage := parseRelayFinishEvent(ev.Raw)
			if finishUsage != nil {
				mergeUsage(&accumulated, *finishUsage)
				sawUsage = true
			}
			if reason != "" {
				payload, err := marshalRelayFinishChunk(chunkID, created, model, reason)
				if err != nil {
					upstreamFail = true
					return false
				}
				if !writeData(payload) {
					return false
				}
			}
			return true

		case contract.EventError:
			// relay-03 §4.3/§10.1：EventError 是**软错误**（坏 JSON 块/上游 error
			// 事件），记录后继续；上限 20 条，第 21 条按硬错误处理。
			// relay-03 §5.2/§10.3：异常结束不得向客户端注入错误对象——因此硬错误
			// （Raw 为空：读取失败/事件超限/空 reader）只终止流，不写错误帧。
			if len(ev.Raw) > 0 && softErrors < maxSoftStreamErrors {
				softErrors++
				return true
			}
			upstreamFail = true
			return false

		case contract.EventPing:
			// 心跳不是客户端可见事件；保活由传输层/SSE 注释帧负责（relay-03 §3.3）。
			return true

		default:
			return true
		}
	})

	// 停止心跳后再写收尾帧，避免 [DONE] 之后仍出现 ping。
	stopPing()
	if pingFailed.Load() {
		clientGone = true
	}

	// 头部兜底（R-05 优先级 3，二轮复审 finding 5）：整个流从未出现
	// EventUsage/finish usage 时，上游可能只在 `x-model-response-usage` 头里
	// 报告用量。先尝试头部；只有头部也没有可用数字时才在下方标记 estimated，
	// 绝不能把「头部给了精确值」的情况降级成本地估算。
	//
	// 判定条件是**非零事实**而非「是否出现过 usage 事件」（re-review fix 1）：
	// 上游发 `usage: {}` 或全零 usage 帧时 sawUsage 已为 true，但累计用量没有
	// 任何数字——此时头部才是唯一的真实来源，不能被事件占位符挡住。反过来，
	// 只要累计出任何非零维度，就绝不拿头部覆盖（三源互斥，R-05）。
	if !sawUsage || !usageHasFact(accumulated) {
		extracted := usage.ExtractUsageMultimodal(&contract.Response{}, upstreamHeader, nil, rc.ChannelType)
		if extracted.Usage.Source == contract.UsageUpstreamExact {
			accumulated = extracted.Usage
			sawUsage = true
		}
	}

	// 收尾帧只在正常/EOF 路径写；异常结束（客户端断开、上游错误、看门狗超时、
	// panic）不写 usage 块与 [DONE]（relay-03 §5.2/§10.3）。
	abnormal := clientGone || upstreamFail || timedOut.Load()
	if !abnormal {
		// 仅当 include_usage 开启时合成末块 usage（客户端显式 false 时不伪造）。
		if includeUsage {
			if payload, err := marshalRelayUsageChunk(chunkID, created, model, accumulated); err == nil {
				if sse.data(payload) {
					sent++
				}
			}
		}
		// relay-03 §3.2：统一结束标记且整个响应只出现一次。
		if sse.done() {
			sent++
		}
	}

	endReason := contract.EndUpstreamDone
	outcome := contract.OutcomeSuccess
	switch {
	case clientGone:
		endReason = contract.EndClientAbort
		outcome = relayStreamOutcome(contentSent)
	case timedOut.Load():
		endReason = contract.EndStreamTimeout
		outcome = relayStreamOutcome(contentSent)
	case upstreamFail:
		endReason = contract.EndUpstreamError
		outcome = relayStreamOutcome(contentSent)
	case !finished:
		endReason = contract.EndUpstreamEOF
		outcome = relayStreamOutcome(contentSent)
	}
	relay.WithStream(rc, &contract.StreamStatus{
		Phase:     contract.PhaseTerminated,
		EndReason: endReason,
		Outcome:   outcome,
		RecvCount: received,
		SendCount: sent,
	}, sent, received, firstTokenMS)

	// R-85：异常结束（客户端断开、上游错误、看门狗超时、panic）整笔退款。
	// 不再为失败流构造/上报用量事实——契约的 Refund() 没有事实入参，
	// 而把零值/部分事实交给 Terminate 会让按次模型照常扣费并记 success。
	if abnormal {
		refund()
		streamStatus = "refunded"
		return
	}

	fact := contract.UsageFact{
		Model:        model,
		Usage:        accumulated,
		LatencyMS:    int(time.Since(start).Milliseconds()),
		FirstTokenMS: firstTokenMS,
		Stream:       true,
		Attempts:     rc.AttemptIndex + 1,
	}
	if !usageHasFact(accumulated) {
		// 上游没有给出任何非零用量（包括空 `usage: {}` / 全零帧，且无头部）：
		// 标记 estimated，绝不把零值谎报成 upstream_exact（零值 Source 的
		// iota 是 UsageUpstreamExact）。关系布尔按**渠道协议族**补齐：
		// OpenAI 的 prompt 已含缓存、completion 已含推理；Claude/Gemini 是
		// 独立维度，不能沿用 OpenAI 的 true（否则缓存段被重复加价）。
		fact.Usage = estimatedUsageFor(rc.ChannelType)
	}
	terminate(fact)
	return
}

// estimatedUsageFor 为「无上游用量、需要本地估算」的事实补上**渠道协议族**
// 语义的关系布尔。
//
// 关系布尔是协议属性而非用量数字（relay-03 §6.2 row 1）：OpenAI 的
// prompt_tokens 已含 cached_tokens、completion_tokens 已含 reasoning_tokens；
// Claude/Gemini 的缓存与思考是独立维度。M4 接线后原生渠道也走本辅助函数，
// 不能再无条件按 OpenAI 置 true（那会让计费侧把缓存段重复加价）。
func estimatedUsageFor(kind contract.ChannelType) contract.Usage {
	return usage.ExtractUsageMultimodal(&contract.Response{}, nil, nil, kind).Usage
}

// relaySSEWriter 是 SSE 帧的唯一写出口。
//
// 为什么需要互斥：流式事件循环（主协程）与 10s ping ticker（独立协程）都会
// 写 http.ResponseWriter；net/http 不保证 ResponseWriter 并发写安全，帧必须
// 串行化（relay-03 §3.4 的「单写者」要求）。failed 置位后所有后续写直接失败，
// 避免客户端断开后反复写坏连接。
type relaySSEWriter struct {
	mu      sync.Mutex
	w       http.ResponseWriter
	flusher http.Flusher
	failed  bool
}

func newRelaySSEWriter(w http.ResponseWriter) *relaySSEWriter {
	flusher, _ := w.(http.Flusher)
	return &relaySSEWriter{w: w, flusher: flusher}
}

func (s *relaySSEWriter) flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

// writeFrame 写一个完整帧并立即 Flush。
func (s *relaySSEWriter) writeFrame(frame []byte) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failed {
		return false
	}
	if _, err := s.w.Write(frame); err != nil {
		s.failed = true
		return false
	}
	if s.flusher != nil {
		s.flusher.Flush()
	}
	return true
}

// data 写 `data: <payload>\n\n`（relay-03 §3.2 OpenAI 帧形态）。
func (s *relaySSEWriter) data(payload []byte) bool {
	frame := make([]byte, 0, len(payload)+8)
	frame = append(frame, "data: "...)
	frame = append(frame, payload...)
	frame = append(frame, '\n', '\n')
	return s.writeFrame(frame)
}

// raw 逐字节转发原始 SSE 事件（R-P2）。适配器的 Raw 是「事件块原文」
// （行间 \n，无尾部空行），因此这里只补足帧结束空行，不改写内容。
func (s *relaySSEWriter) raw(raw []byte) bool {
	frame := append([]byte(nil), raw...)
	switch {
	case bytes.HasSuffix(frame, []byte("\n\n")):
	case bytes.HasSuffix(frame, []byte("\n")):
		frame = append(frame, '\n')
	default:
		frame = append(frame, '\n', '\n')
	}
	return s.writeFrame(frame)
}

// done 写唯一的统一终止标记（relay-03 §3.2）。
func (s *relaySSEWriter) done() bool {
	return s.writeFrame([]byte("data: [DONE]\n\n"))
}

// ping 写 SSE 注释帧 `: PING\n\n`（relay-03 §3.3，10s 间隔）。
func (s *relaySSEWriter) ping() bool {
	return s.writeFrame([]byte(": PING\n\n"))
}

// relayStreamOutcome 按**内容帧**数判定非正常结束的结果（契约 §7.2：
// OutcomePartial = 「有部分内容但未正常终结」）。合成帧（usage/[DONE]）
// 不算内容，否则空流 EOF 会被误报为 partial。
func relayStreamOutcome(contentSent int) contract.ResponseOutcome {
	if contentSent > 0 {
		return contract.OutcomePartial
	}
	return contract.OutcomeFailed
}

// ---------- GET /v1/models ----------

func (h *RelayHandler) handleModels(w http.ResponseWriter, r *http.Request) {
	requestID := newRelayRequestID()
	models, err := h.channels.EnabledModels(r.Context())
	if err != nil {
		// GC-6：存储错误可能含 SQL/DSN，只回固定文案。
		writeRelayError(w, http.StatusInternalServerError, requestID, "database_error",
			"failed to load model list")
		return
	}
	type modelObject struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	}
	payload := struct {
		Object string        `json:"object"`
		Data   []modelObject `json:"data"`
	}{Object: "list", Data: make([]modelObject, 0, len(models))}
	for _, m := range models {
		payload.Data = append(payload.Data, modelObject{ID: m, Object: "model", OwnedBy: "newtg"})
	}
	w.Header().Set("Content-Type", "application/json")
	if requestID != "" {
		w.Header().Set("X-Request-ID", requestID)
	}
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(payload)
}

// ---------- 错误映射 ----------

// mapRelayContextError 把 BuildRelayContext 的哨兵映射为 /v1 状态码/信封。
func mapRelayContextError(err error) (int, string, string) {
	switch {
	case errors.Is(err, relay.ErrTokenInvalid), errors.Is(err, relay.ErrPrincipalMissing):
		// 401 不带 code：令牌类错误不区分原因（relay-01 §3.6）。
		return http.StatusUnauthorized, "", "invalid token"
	case errors.Is(err, relay.ErrPinForbidden):
		return http.StatusForbidden, "channel_pin_forbidden", "channel pin is not allowed for this user"
	case errors.Is(err, relay.ErrAutoGroupsNotEnabled):
		// R-61 / I-20：历史空列表 auto 令牌的请求期 fail-closed；绝不回退
		// 用户分组，因此这里不给任何「换个分组再试」的暗示。
		return http.StatusForbidden, "auto_groups_not_enabled",
			"auto groups are not enabled for this token"
	case errors.Is(err, relay.ErrIdentityOutOfRange):
		return http.StatusInternalServerError, "internal_error", "internal error"
	case errors.Is(err, relay.ErrRequestMissing):
		return http.StatusBadRequest, "invalid_request", "invalid request"
	default:
		return http.StatusInternalServerError, "internal_error", "internal error"
	}
}

// writeRelayMappedError 映射错误并写 /v1 响应。
//
// ErrSettlementInProgress 额外带 Retry-After（R-45/R-86）：同一 requestID 的
// 结算被另一执行者持锁是**可短暂重试**的并发冲突，状态码 409 + 退避提示
// 比把它伪装成 502 上游故障更准确。值为 1 秒：结算锁 TTL 30s、锁内只有
// 一个有界 DB 事务（10s），1s 足以跨过一次正常冲突窗口。
func writeRelayMappedError(w http.ResponseWriter, requestID string, err error) {
	status, code, message := mapRelayError(err)
	if status == http.StatusConflict && errors.Is(err, billing.ErrSettlementInProgress) {
		w.Header().Set("Retry-After", "1")
	}
	writeRelayError(w, status, requestID, code, message)
}

// mapRelayError 把路由/执行错误映射为客户端状态码（relay-01 §9.3）。
//
// 顺序即语义：计费类错误（402/400/409）先于 FastFail 判定——预扣/结算错误
// 被 FastFailError/safeError 包装，但契约语义必须保留（R-86：定价/钳制类
// 不得落 502；R-45：结算冲突 409）。FastFail 与 AttemptsError 的区分决定
// 「一次失败」与「重试耗尽」的展示。所有消息只含固定文案/次数/状态码，
// 绝不含上游 URL/凭据。
func mapRelayError(err error) (int, string, string) {
	// 额度不足（钱包或令牌额度）：真实引擎返回 *billing.QuotaError（Unwrap 到
	// billing.ErrInsufficientQuota）；M2 桩返回 *billingseam.InsufficientQuotaError。
	// 两者都在此统一为 402 Payment Required（契约 §4.4；N7 的令牌/钱包文案统一）。
	var quota *billing.QuotaError
	var stubQuota *billingseam.InsufficientQuotaError
	if errors.As(err, &quota) || errors.As(err, &stubQuota) ||
		errors.Is(err, billing.ErrInsufficientQuota) || errors.Is(err, billingseam.ErrInsufficientQuota) {
		return http.StatusPaymentRequired, "insufficient_quota", "insufficient quota"
	}

	// 定价/配置/表达式失败：400 model_price_error（relay-01 §2 阶段 6；N6）。
	if isModelPriceError(err) {
		return http.StatusBadRequest, "model_price_error", "pricing configuration rejected the request"
	}

	// 请求已在别处结案（结算/退款/worker 兜底）：fail-closed 的 409，不重试。
	if errors.Is(err, billing.ErrRequestClosed) {
		return http.StatusConflict, "request_closed", "the request has already been settled"
	}
	// 并发结算冲突：409 + Retry-After（R-45/R-86），不重试。
	if errors.Is(err, billing.ErrSettlementInProgress) {
		return http.StatusConflict, "settlement_in_progress", "settlement for this request is in progress"
	}
	// 重复预扣超出预算（C-10/D12）：请求级 400，不重试。
	if errors.Is(err, billing.ErrBudgetExceeded) {
		return http.StatusBadRequest, "invalid_request", "reservation exceeds the estimated budget"
	}

	var fastFail *relay.FastFailError
	if errors.As(err, &fastFail) {
		switch {
		case errors.Is(err, relay.ErrTokenInvalid):
			return http.StatusUnauthorized, "", "invalid token"
		case errors.Is(err, relay.ErrPinForbidden):
			return http.StatusForbidden, "channel_pin_forbidden", "channel pin is not allowed for this user"
		case errors.Is(err, relay.ErrNoAvailableChannel):
			// relay-01 §2 阶段 3：503 get_channel_failed（SkipRetry）。
			return http.StatusServiceUnavailable, "model_not_found", "no available channel for the requested model"
		case errors.Is(err, relay.ErrChannelNotFound):
			return http.StatusNotFound, "channel_not_found", "the pinned channel is not available"
		case errors.Is(err, relay.ErrGroupRatioMissing):
			// R-64 / I-25：倍率缺失必须显式失败，绝不按默认 1 计费。
			// 这是请求侧配置错误（403），不得落 502 上游故障。
			return http.StatusForbidden, "group_ratio_missing",
				"the group ratio is not configured for the selected group"
		case errors.Is(err, relay.ErrGroupCheckFailed):
			// 存储故障 fail-closed：既不能当已配置放行，也不能伪装成 403。
			return http.StatusInternalServerError, "database_error",
				"failed to verify the group configuration"
		case errors.Is(err, crossproto.ErrUpstreamProtocolUnsupported):
			// 跨协议转发：目标协议没有匹配渠道 = 上游能力不支持，非重试类
			// 错误（mapRelayError 走的 FastFail 分支已保证不重试）。
			return http.StatusBadRequest, "endpoint_not_supported",
				"no available channel supports the requested upstream protocol"
		}
		var upstream *relay.UpstreamStatusError
		if errors.As(err, &upstream) {
			return relayUpstreamStatus(upstream)
		}
		if errors.Is(err, adapters.ErrEndpointUnsupported) {
			return http.StatusBadRequest, "endpoint_not_supported", "the channel does not support this endpoint"
		}
		// 适配器分派/构造失败等：不暴露底层文本（可能含 base_url 解析细节）。
		return http.StatusBadGateway, "upstream_error", "upstream request failed"
	}

	var attempts *relay.AttemptsError
	if errors.As(err, &attempts) {
		// R-66：重试耗尽时在既有 error.message 追加已尝试分组序列
		// （不新增字段、不改 OpenAPI）。具体分组令牌的 Groups 为 nil，
		// 错误体与 M1 逐字节一致。
		suffix := ""
		if len(attempts.Groups) > 0 {
			suffix = "; groups tried: " + strings.Join(attempts.Groups, ", ")
		}
		if errors.Is(err, relay.ErrNoAvailableChannel) {
			// R-62 空候选收尾：仍是 503 get_channel_failed 语义（与具体分组
			// 的 FastFail 分支同码），但带上已尝试分组便于自助排查。
			return http.StatusServiceUnavailable, "model_not_found",
				"no available channel for the requested model" + suffix
		}
		// 任务书：AttemptsError → 502，并把尝试次数写进 message。
		return http.StatusBadGateway, "upstream_error",
			fmt.Sprintf("upstream request failed after %d attempt(s)", attempts.Attempts) + suffix
	}

	return http.StatusBadGateway, "upstream_error", "upstream request failed"
}

// isModelPriceError 判定「定价配置/表达式错误」族（N6：400 model_price_error）。
//
// 覆盖三类来源：
//   - 会话层：billing.ErrPriceError（预扣前已有 QuotaClamp）、billing.ErrInvalidConfig；
//   - 定价数学：pricing 的 spec/price/参数/表达式哨兵与 *pricing.ClampError；
//   - 表达式求值：*expr.EvalError（语法/未知变量/除零/溢出/未知函数）。
//
// 这些错误都表示「本次请求的定价配置不可用」，属于请求侧 400，不是上游
// 故障；落 502 会误导运维去查上游与网络。
func isModelPriceError(err error) bool {
	if errors.Is(err, billing.ErrPriceError) || errors.Is(err, billing.ErrInvalidConfig) {
		return true
	}
	var clamp *pricing.ClampError
	if errors.As(err, &clamp) {
		return true
	}
	var evalErr *expr.EvalError
	if errors.As(err, &evalErr) {
		return true
	}
	for _, sentinel := range []error{
		pricing.ErrInvalidSpec,
		pricing.ErrNilPrice,
		pricing.ErrMissingExpression,
		pricing.ErrUnexpectedExpression,
		pricing.ErrInvalidMode,
		pricing.ErrInvalidPrice,
		pricing.ErrInvalidParams,
		pricing.ErrInvalidGroupRatio,
		pricing.ErrNegativeExpression,
		pricing.ErrNoExpression,
	} {
		if errors.Is(err, sentinel) {
			return true
		}
	}
	return false
}

// relayUpstreamStatus 实现 §9.3 的 ErrorClass → 客户端状态码映射。
func relayUpstreamStatus(upstream *relay.UpstreamStatusError) (int, string, string) {
	if upstream == nil {
		return http.StatusBadGateway, "upstream_error", "upstream request failed"
	}
	switch upstream.Class {
	case contract.ErrParamInvalid:
		switch upstream.StatusCode {
		case http.StatusBadRequest, http.StatusNotFound, http.StatusUnprocessableEntity:
			return upstream.StatusCode, "invalid_request",
				fmt.Sprintf("upstream returned status %d", upstream.StatusCode)
		}
		return http.StatusBadRequest, "invalid_request",
			fmt.Sprintf("upstream returned status %d", upstream.StatusCode)
	case contract.ErrUpstreamAuth:
		return http.StatusUnauthorized, "upstream_auth_failed",
			fmt.Sprintf("upstream returned status %d", upstream.StatusCode)
	case contract.ErrUpstreamRateLimit:
		return http.StatusTooManyRequests, "rate_limit_exceeded",
			fmt.Sprintf("upstream returned status %d", upstream.StatusCode)
	case contract.ErrContentFilter:
		return http.StatusBadRequest, "content_filter", "request was rejected by the upstream content filter"
	case contract.ErrChannelConfig:
		return http.StatusServiceUnavailable, "channel_config_error", "channel is misconfigured"
	case contract.ErrUpstreamServer:
		return http.StatusBadGateway, "upstream_error",
			fmt.Sprintf("upstream returned status %d", upstream.StatusCode)
	default:
		return http.StatusBadGateway, "upstream_error",
			fmt.Sprintf("upstream returned status %d", upstream.StatusCode)
	}
}

// ---------- OpenAI chunk 编码 ----------

type openAIChunk struct {
	ID      string              `json:"id"`
	Object  string              `json:"object"`
	Created int64               `json:"created"`
	Model   string              `json:"model"`
	Choices []openAIChunkChoice `json:"choices"`
	Usage   *openAIChunkUsage   `json:"usage,omitempty"`
}

type openAIChunkChoice struct {
	Index        int              `json:"index"`
	Delta        openAIChunkDelta `json:"delta"`
	FinishReason *string          `json:"finish_reason"`
}

type openAIChunkDelta struct {
	Content          string                `json:"content,omitempty"`
	ReasoningContent string                `json:"reasoning_content,omitempty"`
	ToolCalls        []openAIChunkToolCall `json:"tool_calls,omitempty"`
}

type openAIChunkToolCall struct {
	Index    int                     `json:"index"`
	ID       string                  `json:"id,omitempty"`
	Type     string                  `json:"type,omitempty"`
	Function openAIChunkToolFunction `json:"function"`
}

type openAIChunkToolFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type openAIChunkUsage struct {
	PromptTokens            int64                     `json:"prompt_tokens"`
	CompletionTokens        int64                     `json:"completion_tokens"`
	TotalTokens             int64                     `json:"total_tokens"`
	PromptTokensDetails     *openAIPromptTokenDetails `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *openAICompletionDetails  `json:"completion_tokens_details,omitempty"`
}

type openAIPromptTokenDetails struct {
	CachedTokens     int64 `json:"cached_tokens,omitempty"`
	CacheWriteTokens int64 `json:"cache_write_tokens,omitempty"`
}

type openAICompletionDetails struct {
	ReasoningTokens int64 `json:"reasoning_tokens,omitempty"`
}

// marshalRelayDeltaChunk 把协议无关的 delta 事件编码为 OpenAI chat.completion.chunk。
// 文本走 content，推理走 reasoning_content（DeepSeek 兼容），工具增量走 tool_calls。
func marshalRelayDeltaChunk(id string, created int64, fallbackModel string, ev contract.StreamEvent) ([]byte, error) {
	model := ev.Model
	if model == "" {
		model = fallbackModel
	}
	delta := openAIChunkDelta{Content: ev.Delta, ReasoningContent: ev.Reasoning}
	if ev.ToolDelta != nil {
		delta.ToolCalls = []openAIChunkToolCall{{
			Index: ev.ToolDelta.Index,
			ID:    ev.ToolDelta.ID,
			Type:  "function",
			Function: openAIChunkToolFunction{
				Name:      ev.ToolDelta.Name,
				Arguments: ev.ToolDelta.ArgumentsFragment,
			},
		}}
	}
	return json.Marshal(openAIChunk{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   model,
		Choices: []openAIChunkChoice{{Index: 0, Delta: delta, FinishReason: nil}},
	})
}

// marshalRelayUsageChunk 编码 include_usage 末块：choices 为空数组、只带 usage。
func marshalRelayUsageChunk(id string, created int64, model string, u contract.Usage) ([]byte, error) {
	// H2：OpenAI 的 prompt_tokens 语义**包含**缓存读/写；原生渠道
	// （Claude/Gemini）的 input_tokens/promptTokenCount 不含缓存（
	// PromptIncludesCache=false）。客户端收到的 include_usage 末块必须是
	// OpenAI 语义，否则会出现 cached_tokens > prompt_tokens 的自相矛盾
	// （客户端/监控据此误判）。非流式路径已在 crossproto 做同样归一。
	promptTokens := u.Prompt
	if !u.PromptIncludesCache {
		promptTokens += u.CacheRead + u.CacheWrite
	}
	totalTokens := u.Total
	if totalTokens < promptTokens+u.Completion {
		// Total 是日志维度；至少覆盖 prompt+completion，避免上游只给
		// 部分维度时出现 total < prompt 的异常值。
		totalTokens = promptTokens + u.Completion
	}
	usagePayload := &openAIChunkUsage{
		PromptTokens:     promptTokens,
		CompletionTokens: u.Completion,
		TotalTokens:      totalTokens,
	}
	if u.CacheRead != 0 || u.CacheWrite != 0 {
		usagePayload.PromptTokensDetails = &openAIPromptTokenDetails{
			CachedTokens:     u.CacheRead,
			CacheWriteTokens: u.CacheWrite,
		}
	}
	if u.Reasoning != 0 {
		usagePayload.CompletionTokensDetails = &openAICompletionDetails{ReasoningTokens: u.Reasoning}
	}
	return json.Marshal(openAIChunk{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   model,
		Choices: []openAIChunkChoice{},
		Usage:   usagePayload,
	})
}

// marshalRelayFinishChunk 编码带 finish_reason 的终止 chunk（delta 为空）。
// OpenAI 客户端据此判断 stop / length / tool_calls；缺失会让上层误以为流被截断。
func marshalRelayFinishChunk(id string, created int64, model, reason string) ([]byte, error) {
	finish := reason
	return json.Marshal(openAIChunk{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   model,
		Choices: []openAIChunkChoice{{
			Index:        0,
			Delta:        openAIChunkDelta{},
			FinishReason: &finish,
		}},
	})
}

// parseRelayFinishEvent 从 EventFinish 的原始 SSE 帧中提取 finish_reason 与 usage。
//
// EventFinish 的 Raw 有两种来源（见 adapters/openai_stream.go）：
//   - 上游 finish_reason 块的原文（可能同一块还带 usage）；
//   - 上游只发 [DONE] 时，适配器补发的 EventFinish（Raw = "data: [DONE]"）。
//
// 后者必须识别并忽略，绝不能当 JSON 解析或原样转发。返回空 reason 表示
// 「没有可用的 finish_reason」；usage 仅在帧内确有 usage 对象时非 nil。
func parseRelayFinishEvent(raw []byte) (string, *contract.Usage) {
	payload := sseDataPayload(raw)
	if payload == "" || payload == "[DONE]" {
		return "", nil
	}
	var chunk struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage         json.RawMessage `json:"usage"`
		UsageMetadata json.RawMessage `json:"usageMetadata"`
	}
	if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
		return "", nil
	}
	reason := ""
	if len(chunk.Choices) > 0 {
		reason = chunk.Choices[0].FinishReason
	}
	if reason == "" {
		// 原生协议（Anthropic/Gemini）的 EventFinish.Raw 是上游原始事件帧，
		// 结束原因不在 OpenAI 的 choices[].finish_reason 里。不在此处归一，
		// 原生流会「丢 finish_reason」，客户端据空值误判为被截断。
		reason = nativeFinishReason([]byte(payload))
	}
	var usage *contract.Usage
	usageRaw := bytes.TrimSpace(chunk.Usage)
	if len(usageRaw) > 0 && string(usageRaw) != "null" {
		// 原生 Anthropic message_delta 的 usage 形状是 input/output_tokens，
		// 用 OpenAI 解析器会得到零值（或把 total 误映射成 prompt）。按键
		// 形状选择解析器，与 T3 的「形状优先」口径一致。
		var parsed contract.Usage
		if bytes.Contains(usageRaw, []byte("input_tokens")) || bytes.Contains(usageRaw, []byte("output_tokens")) {
			parsed = ir.ParseAnthropicUsage(chunk.Usage)
		} else {
			parsed = ir.ParseOpenAIUsage(chunk.Usage)
		}
		usage = &parsed
	} else if meta := bytes.TrimSpace(chunk.UsageMetadata); len(meta) > 0 && string(meta) != "null" {
		parsed := ir.ParseGeminiUsage(chunk.UsageMetadata)
		usage = &parsed
	}
	return reason, usage
}

// nativeFinishReason 把 Anthropic / Gemini 的原始结束原因映射为 OpenAI 的
// finish_reason 语义（relay-03 的客户端契约是 OpenAI 形态）。
//
// 无法映射的值返回空串而不是臆造：调用方（bridgeStream）在 reason 为空时
// 不写 finish chunk——宁可让客户端按 EOF 处理，也不伪造 stop。
func nativeFinishReason(payload []byte) string {
	var anthropic struct {
		Type  string `json:"type"`
		Delta struct {
			StopReason string `json:"stop_reason"`
		} `json:"delta"`
	}
	if err := json.Unmarshal(payload, &anthropic); err == nil && anthropic.Delta.StopReason != "" {
		switch anthropic.Delta.StopReason {
		case "end_turn", "stop_sequence", "pause_turn":
			return "stop"
		case "max_tokens":
			return "length"
		case "tool_use":
			return "tool_calls"
		case "refusal":
			return "content_filter"
		default:
			return ""
		}
	}
	var gemini struct {
		Candidates []struct {
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(payload, &gemini); err == nil {
		for _, candidate := range gemini.Candidates {
			switch strings.ToUpper(strings.TrimSpace(candidate.FinishReason)) {
			case "":
				continue
			case "STOP":
				return "stop"
			case "MAX_TOKENS":
				return "length"
			case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII":
				return "content_filter"
			case "MALFORMED_FUNCTION_CALL", "UNEXPECTED_TOOL_CALL":
				return "tool_calls"
			default:
				return ""
			}
		}
	}
	return ""
}

// sseDataPayload 合并一个 SSE 事件块内的多条 data: 行（\n 连接）。
func sseDataPayload(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	lines := make([]string, 0, 2)
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.HasPrefix(line, "data:") {
			lines = append(lines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// mergeUsage 把流式分块用量累计到 dst：**非零才覆盖，绝不擦除已有维度**
// （多块/多候选上游可能只在前几块给出非零值；relay-03 §6）。
func mergeUsage(dst *contract.Usage, src contract.Usage) {
	if src.Prompt != 0 {
		dst.Prompt = src.Prompt
	}
	if src.Completion != 0 {
		dst.Completion = src.Completion
	}
	if src.Total != 0 {
		dst.Total = src.Total
	}
	if src.CacheRead != 0 {
		dst.CacheRead = src.CacheRead
	}
	if src.CacheWrite != 0 {
		dst.CacheWrite = src.CacheWrite
	}
	if src.Reasoning != 0 {
		dst.Reasoning = src.Reasoning
	}
	if src.AudioSeconds != 0 {
		dst.AudioSeconds = src.AudioSeconds
	}
	if src.Reserved != 0 {
		dst.Reserved = src.Reserved
	}
	dst.PromptIncludesCache = dst.PromptIncludesCache || src.PromptIncludesCache
	dst.ReasoningIncludedInCompletion = dst.ReasoningIncludedInCompletion || src.ReasoningIncludedInCompletion
	dst.PromptDetail = mergeUsageDetail(dst.PromptDetail, src.PromptDetail)
	dst.CompletionDetail = mergeUsageDetail(dst.CompletionDetail, src.CompletionDetail)
	if src.Raw != nil {
		dst.Raw = src.Raw
	}
	if usageHasFact(src) {
		dst.Source = src.Source
	}
}

func mergeUsageDetail(dst, src contract.UsageDetail) contract.UsageDetail {
	if src.Image != 0 {
		dst.Image = src.Image
	}
	if src.Audio != 0 {
		dst.Audio = src.Audio
	}
	if src.Text != 0 {
		dst.Text = src.Text
	}
	if src.CacheRead != 0 {
		dst.CacheRead = src.CacheRead
	}
	if src.CacheWrite != 0 {
		dst.CacheWrite = src.CacheWrite
	}
	if src.CacheReadImage != 0 {
		dst.CacheReadImage = src.CacheReadImage
	}
	return dst
}

func usageHasFact(u contract.Usage) bool {
	return u.Prompt != 0 || u.Completion != 0 || u.Total != 0 ||
		u.CacheRead != 0 || u.CacheWrite != 0 || u.Reasoning != 0 ||
		u.AudioSeconds != 0 || u.Reserved != 0 ||
		u.PromptDetail != (contract.UsageDetail{}) ||
		u.CompletionDetail != (contract.UsageDetail{})
}

// ---------- 上游 HTTP 客户端 ----------

// newRelayHTTPClient 返回 /v1 中继使用的上游客户端（relay-01 §11.1）。
//
// 在 handler 构造期创建一次并跨请求复用：Transport 持有连接池，
// §11.1 的 500 / 每主机 100 上限只有在长期复用时才有意义（与 relay 包
// defaultRouteClient 同口径）。测试通过 RelayHandlerOptions.Client 注入。
func newRelayHTTPClient() *http.Client {
	return &http.Client{
		Transport: newRelayTransport(),
		// relay-01 §6.3：3xx 视为上游异常，不跟随重定向（避免凭据被转发）。
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		// Client.Timeout 保持 0：整体读取默认禁用（§11.1）。
	}
}

func newRelayTransport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   relayDialTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          relayMaxIdleConns,
		MaxIdleConnsPerHost:   relayMaxIdleConnsPerHost,
		IdleConnTimeout:       relayIdleConnTimeout,
		TLSHandshakeTimeout:   relayTLSHandshakeTimeout,
		ExpectContinueTimeout: relayExpectContinueTimeout,
		ResponseHeaderTimeout: relayResponseHeaderTimeout,
	}
}

// ---------- 渠道连通性测试（真实实现） ----------

// channelTestUpstreamError 是连通性测试的上游非 2xx 摘要。Error() 只含状态码：
// base_url/凭据/上游 body 绝不进入错误文本（GC-6），handler 也整体丢弃该错误。
type channelTestUpstreamError struct {
	StatusCode int
	Class      contract.ErrorClass
	// Body 是 ≤64 KiB 的上游错误体，仅供 M4-b 的自动禁用判定做关键词匹配。
	// 它**不参与** Error()，也不会被日志/响应回显——持有它的是 handler 与
	// channelhealth 判定函数（GC-6 的边界在这里是字段级而不是进程级）。
	Body []byte
}

func (e *channelTestUpstreamError) Error() string {
	// Class 是契约枚举（int），不含任何上游数据；带上是为排障提供归因，
	// 同时避免字段只写不读（handler 仍整体丢弃错误文本，GC-6）。
	return fmt.Sprintf("httpapi: channel test: upstream returned status %d (class %d)",
		e.StatusCode, int(e.Class))
}

// modules-06 F-06-06 冻结的连通性失败类别：只允许这五个字面量。
// 前端按该枚举做文案/引导，任何新词都会落到未定义分支，故不得扩展。
const (
	channelTestClassAuth    = "auth"    // 上游鉴权失败（401/403）
	channelTestClassTimeout = "timeout" // 请求超时（context deadline / net timeout）
	channelTestClassServer  = "server"  // 上游 5xx 或其它非 auth/param 状态；不可分类错误也归此档
	channelTestClassNetwork = "network" // 连接/DNS 失败
	channelTestClassParam   = "param"   // 请求参数非法（含内容审核）
)

// channelTestTransportError 是连通性测试的传输层失败摘要。
// Class 只由 channelTestTransportClass 写入（timeout/network），
// Error() 不含 URL/凭据（GC-6）；handler 最终仍整体丢弃该错误文本。
type channelTestTransportError struct{ Class string }

func (e *channelTestTransportError) Error() string {
	return "httpapi: channel test: upstream request failed (" + e.Class + ")"
}

// channelTestTransportClass 区分超时与一般网络失败：
//   - context deadline 或 net.Error.Timeout() → timeout；
//   - 其余（连接拒绝、DNS、TLS 握手失败、客户端取消）→ network。
//
// 实现委托 channelhealth.TransportClass：手工测试与健康体检必须对同一
// 传输错误给出同一分类（R-M4-01 的「传输层错误不禁用」依赖它）。
func channelTestTransportClass(err error) string {
	return channelhealth.TransportClass(err)
}

// channelTestErrorClass 把连通性测试错误归一为 modules-06 F-06-06 的
// **五值枚举**：auth | timeout | server | network | param。
//
// 映射口径：
//   - auth：上游 401/403（契约 ErrUpstreamAuth）；
//   - param：请求/内容校验类（ErrParamInvalid、ErrContentFilter）；
//   - timeout / network：由 channelTestTransportClass 在传输层写入；
//   - server：上游 5xx、限流、渠道配置等其余状态。
//
// 【不可分类 → server】选择保守档：server 不会把「上游不可用」误报成客户端
// 需要改参数的 param 或重新配置凭据的 auth，避免把排障方向指向错误一侧。
// 返回值本身不含状态码/URL/凭据，绝不回显底层错误文本（GC-6）。
func channelTestErrorClass(err error) string {
	var transport *channelTestTransportError
	if errors.As(err, &transport) {
		switch transport.Class {
		case channelTestClassTimeout, channelTestClassNetwork:
			return transport.Class
		default:
			return channelTestClassServer
		}
	}
	var upstream *channelTestUpstreamError
	if errors.As(err, &upstream) {
		switch upstream.Class {
		case contract.ErrUpstreamAuth:
			return channelTestClassAuth
		case contract.ErrParamInvalid, contract.ErrContentFilter:
			return channelTestClassParam
		default:
			// 5xx / 限流 / 渠道配置 / 未知状态：server。
			return channelTestClassServer
		}
	}
	return channelTestClassServer
}

// NewChannelProbeClient 返回健康体检使用的上游客户端。
//
// 与中继/手工测试共用 newRelayTransport（relay-01 §11.1 的连接池与超时策略），
// 不新增第二套出站路径；整体超时由每次探测的 ctx 控制（体检 15s，R-M4-14），
// 因此这里不设置 Client.Timeout（长连接池需要跨探测复用）。
func NewChannelProbeClient() *http.Client {
	return &http.Client{
		Transport: newRelayTransport(),
		// 3xx 视为上游异常，不跟随重定向（避免凭据被转发到第三方）。
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// NewChannelConnectivityTester 构造 modules-06 F-06-06 的真实连通性测试：
// 解封凭据 → 适配器构造一个最小非流式请求（单条 user "ping"，max_tokens=1）
// → 30s 内发送 → 非 2xx 按 adapter.ClassifyError 归类并返回非敏感错误。
//
// W7/M4-b 起它是 channelhealth.Prober 的**薄适配**：请求构造、凭据解封与
// 状态码分类只有一份实现，手工测试与健康体检共享（R-M4-15）。本函数只做
// 「ProbeResult → 既有五值 error_class 错误」的映射，保持 phase 8A 的
// ChannelTester 签名不变。
//
// 返回值是 phase 8A 的 ChannelTester 签名（latencyMS, error）。
func NewChannelConnectivityTester(sealer *SecretSealer, client *http.Client) ChannelTester {
	if client == nil {
		client = &http.Client{
			Timeout:       channelTestTimeout,
			Transport:     newRelayTransport(),
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	prober, err := channelhealth.NewProber(channelhealth.ProberConfig{
		Opener:  sealer,
		Client:  client,
		Timeout: channelTestTimeout, // 手工测试沿用 30s（R-M4-14 的例外）
	})
	return func(ctx context.Context, ch *store.Channel, model string) (int, error) {
		if ch == nil {
			return 0, errors.New("httpapi: channel test: channel is nil")
		}
		if sealer == nil || err != nil {
			return 0, errors.New("httpapi: channel test: credential sealer is not configured")
		}
		result := prober.Probe(ctx, ch, model)
		switch {
		case result.Unsupported:
			// 适配器分派错误只含渠道类型；仍用固定文案（GC-6 的最后一道出口）。
			return 0, errors.New("httpapi: channel test: no adapter for this channel type")
		case result.CredentialInvalid:
			return 0, errRelayCredentialInvalid
		case result.BuildFailed:
			// 构造错误可能回显 URL（GC-6）→ 固定文案。
			return 0, errors.New("httpapi: channel test: build upstream request failed")
		case result.TransportClass != "":
			// *url.Error 的文本含完整 URL → 绝不透出（GC-6）；只保留
			// timeout/network 这一层泛化归因，供 F-06-06 的 error_class 使用。
			return 0, &channelTestTransportError{Class: result.TransportClass}
		case !result.Success:
			return 0, &channelTestUpstreamError{
				StatusCode: result.StatusCode,
				Class:      result.ErrorClass,
				Body:       result.Body,
			}
		default:
			return result.LatencyMS, nil
		}
	}
}
