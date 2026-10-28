package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
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
//	POST /v1/messages           Anthropic 原生入口（W7/M1）：Anthropic 上游逐字节
//	                            透传；OpenAI/Gemini 上游经 IR → Anthropic 编码
//	                            （W18-B · R-W18-03）；三类之外的上游 501
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

// channelBatchGetter 是 RelayChannelStore 的**可选**扩展：批量读取渠道行。
//
// 接口是冻结的，不能为新优化往里加方法；用可选接口 + 类型断言既能拿到
// 生产实现（*store.ChannelRepo）的批量 SQL，又不破坏任何其它实现（测试
// fake、插件侧替身）——没有实现时退回逐条 Get，语义完全一致。
type channelBatchGetter interface {
	GetMany(ctx context.Context, ids []int64) (map[int64]*store.Channel, error)
}

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

// RefusalRecorder 是「未到达上游即被拒绝」的落库接缝（Task 3b）。
//
// 生产实现是 *billing.DBLogger；nil 表示未装配日志库（探针-only / 测试装配），
// 此时所有拒绝记录都是 no-op，HTTP 行为与接线前逐字节一致。
type RefusalRecorder interface {
	RecordRefusal(ctx context.Context, in billing.Refusal) error
}

var _ RefusalRecorder = (*billing.DBLogger)(nil)

// modelPricingStatusDisabled 是 model_pricing.status 的停用值（modules-06
// §3.7 状态机：1 启用 / 2 停用；迁移的 CHECK 约束只允许这两个值）。
const modelPricingStatusDisabled int16 = 2

// RelayHandlerOptions 是 platform 装配与测试的注入点。
type RelayHandlerOptions struct {
	// Channels 是候选/渠道/模型列表的数据源（必填）。
	Channels RelayChannelStore
	// Tokens 是令牌解析接缝（auth.TokenLookup；生产为带缓存的
	// *tokenstore.Store——见 platform 的 relayTokenLookup，P2-2）。
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
	// Refusals 是中继前拒绝（鉴权/请求结构/模型准入/配额/限流）的落库接缝
	// （Task 3b）；nil 时 no-op。
	Refusals RefusalRecorder
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
	// refusals 是 Task 3b 的中继前拒绝落库接缝；nil 时记录为 no-op。
	refusals RefusalRecorder
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
		refusals:         opts.Refusals,
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
			// M4-a：多模态端点也经 ExecuteAttempts，失败渠道必须
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
// 为什么读渠道类型而不是让 store 查询时过滤：Candidates 的存储面
// （RelayChannelStore）是冻结的，且协议族映射的唯一真源在 adapters 分派表；
// 在路由层做这一层过滤可以避免把「渠道类型 → 协议」的知识泄漏进 store。
//
// P2-2：读的是「候选这一批」而不是逐条点查——存储实现支持 GetMany 时一次
// SQL 取回（候选通常个位数，但每个候选一次往返在高峰期就是 N 倍放大器）；
// 否则（测试替身等）退回逐条 Get。批量调用本身失败时也退回逐条，让 pin
// 缺失/渠道已删的判定保持既有语义，而不是把基础设施故障说成「协议不支持」。
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
	load := candidateChannelLoader(ctx, channels, candidates)
	filtered := make([]contract.ChannelCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		ch, err := load(int64(candidate.ChannelID))
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

// candidateChannelLoader 返回按 id 取渠道的函数：优先使用存储的批量读取，
// 不支持或批量失败时逐条 Get。缺行语义由调用方处理（返回 store.ErrChannelNotFound），
// 与逐条 Get 完全一致。
func candidateChannelLoader(ctx context.Context, channels RelayChannelStore,
	candidates []contract.ChannelCandidate) func(int64) (*store.Channel, error) {

	batch, ok := channels.(channelBatchGetter)
	if !ok || len(candidates) == 0 {
		return func(id int64) (*store.Channel, error) { return channels.Get(ctx, id) }
	}
	ids := make([]int64, 0, len(candidates))
	for _, candidate := range candidates {
		ids = append(ids, int64(candidate.ChannelID))
	}
	loaded, err := batch.GetMany(ctx, ids)
	if err != nil {
		// 批量读取失败：退回逐条，保持「pin 缺失上抛、普通候选跳过」的判定。
		return func(id int64) (*store.Channel, error) { return channels.Get(ctx, id) }
	}
	return func(id int64) (*store.Channel, error) {
		if ch, ok := loaded[id]; ok {
			return ch, nil
		}
		return nil, store.ErrChannelNotFound
	}
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
	// 上游账号归属随渠道整组投影；NULL = 未归属，以 0 表示。
	// 后续由执行层复制进 RelayContext，再在产出 UsageFact 时冻结。
	accountID := int64(0)
	if ch.UpstreamAccountID != nil {
		accountID = *ch.UpstreamAccountID
	}
	return relay.ChannelAssignment{
		ChannelID:         channelID,
		ChannelType:       contract.ChannelType(ch.Type),
		APIBase:           apiBase,
		Credential:        creds.APIKey,
		UpstreamModel:     modelMappingTarget(ch.ModelMapping, rc.OriginModel),
		UpstreamAccountID: accountID,
		GroupRatio:        1.0,
		CreatedTime:       ch.CreatedAt,
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
		h.logRelayFailure(r, rc, err)
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
		// 用户付不起这笔账：不交付服务。hold 留在账本由补偿扫描处理；
		// 这里绝不能 Refund（上游已经执行完了）。
		if commitErr := settlement.commit(); commitErr != nil {
			// Important-2：资金已提交、hold 已关闭，只有日志写失败时钱其实
			// 已经收上来了——返回 5xx 只会让客户端重试并二次扣费。
			postCharge := errors.Is(commitErr, billing.ErrPostChargeLogFailed)
			if !postCharge {
				status = "error"
			}
			h.reportSettlementFailure(rc, commitErr, postCharge)
			if !postCharge {
				writeRelayMappedError(w, rc.RequestID, commitErr)
				return
			}
		}
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
		h.logRelayFailure(r, rc, err)
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
	finalize := func(terminate func() error) error {
		if terminated || billing == nil {
			return nil
		}
		terminated = true
		return terminate()
	}
	terminate := func(fact contract.UsageFact) {
		if err := finalize(func() error { return billing.Terminate(fact) }); err != nil {
			// 流式响应已经写出：无法改 402，只能留下结算失败证据。
			h.reportSettlementFailure(rc, err, true)
		}
	}
	// R-85：失败/取消/panic 是整笔不收费语义。真实引擎的 Refund 会全额退回
	// hold 与增量，并把 usage_logs 收敛为 refunded；Terminate(零事实) 会让
	// 按次/常量表达式模型对失败流照常扣费并记 success。
	refund := func() {
		_ = finalize(func() error { return billing.Refund() })
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
		// 流式路径不保留上游响应 id；归属维度照常冻结。
		UpstreamModel:     rc.UpstreamModel,
		UpstreamAccountID: rc.UpstreamAccountID,
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

// anthropicEvent 写一个 Anthropic SSE 事件帧：`event: <name>\ndata: <json>\n\n`。
//
// 与 OpenAI 的纯 data 帧不同，Anthropic 客户端按 event 名分派；data 必须是
// 单行 JSON（encoding/json 的输出不含裸换行），因此无需再拆 data 行。
func (s *relaySSEWriter) anthropicEvent(name string, data []byte) bool {
	frame := make([]byte, 0, len(name)+len(data)+16)
	frame = append(frame, "event: "...)
	frame = append(frame, name...)
	frame = append(frame, '\n', 'd', 'a', 't', 'a', ':', ' ')
	frame = append(frame, data...)
	frame = append(frame, '\n', '\n')
	return s.writeFrame(frame)
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

// ---------- GET /v1/models 族 ----------
//
// 模型列表/检索按请求头分流（relay-04 §10.1）：同一份启用模型清单要能以
// OpenAI / Anthropic / Gemini 三种 SDK 期望的形状返回，否则「一个 key 接
// 三家 SDK」在这些辅助端点上就断了。非计费端点（R-W18-10），不消耗限流桶。
//
// 错误信封固定为 OpenAI 形：R-W18-04 只给 `/v1/messages` 留 Claude 形
// （W18-A 面），模型列表不因请求头改变错误形状，避免同一路径出现两套
// 客户端解析分支。

// modelListShape 是模型列表的三种对外形状。
type modelListShape int

const (
	modelShapeOpenAI modelListShape = iota
	modelShapeAnthropic
	modelShapeGemini
)

// modelListShapeFor 按请求头/查询参数选择列表形状（relay-04 §10.1）：
//   - `x-api-key` + `anthropic-version` → Anthropic；
//   - `x-goog-api-key` 或 `?key=` → Gemini（`?key=` 是 Gemini 兼容载体）；
//   - 否则 OpenAI。
func modelListShapeFor(r *http.Request) modelListShape {
	if strings.TrimSpace(r.Header.Get("x-api-key")) != "" &&
		strings.TrimSpace(r.Header.Get("anthropic-version")) != "" {
		return modelShapeAnthropic
	}
	if strings.TrimSpace(r.Header.Get("x-goog-api-key")) != "" {
		return modelShapeGemini
	}
	if r.URL != nil && strings.TrimSpace(r.URL.Query().Get("key")) != "" {
		return modelShapeGemini
	}
	return modelShapeOpenAI
}

// modelShapeForRetrieval 是 `GET /v1/models/{model...}` 的分流：Anthropic 头 →
// Anthropic，否则 OpenAI（relay-04 §10.1 只给这两支，不含 Gemini）。
func modelShapeForRetrieval(r *http.Request) modelListShape {
	if strings.TrimSpace(r.Header.Get("x-api-key")) != "" &&
		strings.TrimSpace(r.Header.Get("anthropic-version")) != "" {
		return modelShapeAnthropic
	}
	return modelShapeOpenAI
}

type openAIModelObject struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`
}

type anthropicModelObject struct {
	Type        string `json:"type"`
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	CreatedAt   string `json:"created_at"`
}

type geminiModelObject struct {
	Name                       string   `json:"name"`
	DisplayName                string   `json:"displayName"`
	SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
}

// modelFallbackCreatedAt 是 Anthropic 模型对象的 created_at 占位值。
//
// 网关只聚合渠道配置里的模型名，没有「模型创建时间」这一事实。Anthropic
// 的 Models API 形状要求该字段存在；用固定 epoch 而不是 time.Now()，保证
// 同一份配置在任意副本/任意时刻返回相同字节（便于缓存与漂移测试）。
const modelFallbackCreatedAt = "1970-01-01T00:00:00Z"

func (h *RelayHandler) handleModels(w http.ResponseWriter, r *http.Request) {
	switch modelListShapeFor(r) {
	case modelShapeAnthropic:
		h.writeAnthropicModels(w, r)
	case modelShapeGemini:
		h.writeGeminiModels(w, r)
	default:
		h.writeOpenAIModels(w, r)
	}
}

// handleOpenAIModels 固定 OpenAI 形状（`/v1beta/openai/models`，relay-04 §10.1）。
func (h *RelayHandler) handleOpenAIModels(w http.ResponseWriter, r *http.Request) {
	h.writeOpenAIModels(w, r)
}

// handleModel 是 `GET /v1/models/{model...}`：Anthropic 头 → Anthropic 形状，
// 否则 OpenAI 形状。
func (h *RelayHandler) handleModel(w http.ResponseWriter, r *http.Request) {
	model := r.PathValue("model")
	models, requestID, ok := h.loadEnabledModels(w, r)
	if !ok {
		return
	}
	if !containsModel(models, model) {
		writeRelayError(w, http.StatusNotFound, requestID, "model_not_found",
			"the requested model is not available")
		return
	}
	if modelShapeForRetrieval(r) == modelShapeAnthropic {
		writeJSONResponse(w, requestID, anthropicModelObject{
			Type:        "model",
			ID:          model,
			DisplayName: model,
			CreatedAt:   modelFallbackCreatedAt,
		})
		return
	}
	writeJSONResponse(w, requestID, openAIModelObject{ID: model, Object: "model", OwnedBy: "newtg"})
}

// handleBetaModels 是 `GET /v1beta/models`：Gemini 形状（relay-04 §10.1）。
func (h *RelayHandler) handleBetaModels(w http.ResponseWriter, r *http.Request) {
	h.writeGeminiModels(w, r)
}

// handleBetaModel 是 `GET /v1beta/models/{model...}`：Gemini 形状。
func (h *RelayHandler) handleBetaModel(w http.ResponseWriter, r *http.Request) {
	model := r.PathValue("model")
	models, requestID, ok := h.loadEnabledModels(w, r)
	if !ok {
		return
	}
	if !containsModel(models, model) {
		writeRelayError(w, http.StatusNotFound, requestID, "model_not_found",
			"the requested model is not available")
		return
	}
	writeJSONResponse(w, requestID, geminiModelFor(model))
}

func (h *RelayHandler) writeOpenAIModels(w http.ResponseWriter, r *http.Request) {
	models, requestID, ok := h.loadEnabledModels(w, r)
	if !ok {
		return
	}
	payload := struct {
		Object string              `json:"object"`
		Data   []openAIModelObject `json:"data"`
	}{Object: "list", Data: make([]openAIModelObject, 0, len(models))}
	for _, m := range models {
		payload.Data = append(payload.Data, openAIModelObject{ID: m, Object: "model", OwnedBy: "newtg"})
	}
	writeJSONResponse(w, requestID, payload)
}

func (h *RelayHandler) writeAnthropicModels(w http.ResponseWriter, r *http.Request) {
	models, requestID, ok := h.loadEnabledModels(w, r)
	if !ok {
		return
	}
	payload := struct {
		Data    []anthropicModelObject `json:"data"`
		HasMore bool                   `json:"has_more"`
		FirstID *string                `json:"first_id"`
		LastID  *string                `json:"last_id"`
	}{Data: make([]anthropicModelObject, 0, len(models))}
	for _, m := range models {
		payload.Data = append(payload.Data, anthropicModelObject{
			Type:        "model",
			ID:          m,
			DisplayName: m,
			CreatedAt:   modelFallbackCreatedAt,
		})
	}
	if len(models) > 0 {
		first, last := models[0], models[len(models)-1]
		payload.FirstID, payload.LastID = &first, &last
	}
	writeJSONResponse(w, requestID, payload)
}

func (h *RelayHandler) writeGeminiModels(w http.ResponseWriter, r *http.Request) {
	models, requestID, ok := h.loadEnabledModels(w, r)
	if !ok {
		return
	}
	payload := struct {
		Models []geminiModelObject `json:"models"`
	}{Models: make([]geminiModelObject, 0, len(models))}
	for _, m := range models {
		payload.Models = append(payload.Models, geminiModelFor(m))
	}
	writeJSONResponse(w, requestID, payload)
}

// geminiModelFor 把网关模型名投影为 Gemini Models API 对象。
//
// Gemini 的 `name` 必须是 `models/<id>` 形式。`supportedGenerationMethods`
// 只声明网关**确实会转发**的动作：`POST /v1beta/models/{model}:embedContent`
// 与 `:batchEmbedContents`（后者不是 SDK 的 generation method，不列入）。
// 声明 `generateContent`/`streamGenerateContent` 会让 SDK 去调用一条本网关
// 未注册的路径——那是能力谎报（R-W18-11 的反面）。
func geminiModelFor(model string) geminiModelObject {
	return geminiModelObject{
		Name:                       "models/" + model,
		DisplayName:                model,
		SupportedGenerationMethods: []string{"embedContent"},
	}
}

func (h *RelayHandler) loadEnabledModels(w http.ResponseWriter, r *http.Request) ([]string, string, bool) {
	requestID := newRelayRequestID()
	models, err := h.channels.EnabledModels(r.Context())
	if err != nil {
		// GC-6：存储错误可能含 SQL/DSN，只回固定文案。
		writeRelayError(w, http.StatusInternalServerError, requestID, "database_error",
			"failed to load model list")
		return nil, requestID, false
	}
	return models, requestID, true
}

func writeJSONResponse(w http.ResponseWriter, requestID string, payload any) {
	w.Header().Set("Content-Type", "application/json")
	if requestID != "" {
		w.Header().Set("X-Request-ID", requestID)
	}
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(payload)
}

func containsModel(models []string, model string) bool {
	for _, candidate := range models {
		if candidate == model {
			return true
		}
	}
	return false
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

// settleFailureReason 把结算错误收敛成有限的指标/日志枚举。
//
// GC-6：标签与日志字段绝不能直接来自 err.Error()。
func settleFailureReason(err error) string {
	var quotaErr *billing.QuotaError
	switch {
	case errors.As(err, &quotaErr), errors.Is(err, billing.ErrInsufficientQuota):
		return "insufficient_quota"
	case errors.Is(err, billing.ErrRequestClosed):
		return "request_closed"
	case errors.Is(err, billing.ErrSettlementInProgress):
		return "settlement_in_progress"
	case errors.Is(err, billing.ErrPostChargeLogFailed):
		// 资金已提交、仅日志写失败：响应仍会交付，但欠一条 usage_logs。
		return "log_write_failed"
	default:
		return "internal"
	}
}

// reportSettlementFailure 记录一次「上游已执行、结算失败」。
//
// delivered 区分非流式（false：402 阻断交付）与流式（true：只能事后
// 靠指标/日志发现欠费请求）。
func (h *RelayHandler) reportSettlementFailure(rc *contract.RelayContext, err error, delivered bool) {
	reason := settleFailureReason(err)
	if rc != nil {
		metrics.ObserveSettleFailure(rc.OriginModel, rc.UsingGroup, reason)
	}
	if h == nil || h.log == nil || rc == nil {
		return
	}
	args := []any{
		"request_id", rc.RequestID,
		"endpoint", relay.EndpointLabel(rc),
		"model", rc.OriginModel,
		"group", rc.UsingGroup,
		"error_class", reason,
		"delivered", delivered,
	}
	var quotaErr *billing.QuotaError
	if errors.As(err, &quotaErr) {
		args = append(args, "required", quotaErr.Required, "available", quotaErr.Available)
	}
	h.log.Error("billing settlement failed", args...)
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

// relayFailureDiagnostics 从错误链提取可安全记录的上游诊断事实。
//
// GC-6：只返回枚举与数字，不返回 err.Error()——适配器/url.Error 的文本可能
// 含 base_url 或凭据；上游响应体已在执行层丢弃，这里也不能补回来。
func relayFailureDiagnostics(err error) (class string, upstreamStatus, attempts int) {
	var attemptsErr *relay.AttemptsError
	if errors.As(err, &attemptsErr) {
		attempts = attemptsErr.Attempts
	}
	var upstream *relay.UpstreamStatusError
	if errors.As(err, &upstream) {
		if attempts == 0 {
			attempts = 1
		}
		return relay.ErrorClassName(upstream.Class), upstream.StatusCode, attempts
	}
	return "", 0, attempts
}

// logRelayFailure 记录一次中继最终失败的结构化诊断行。
//
// 为什么必须在这里打：客户端只收到固定文案 + requestId（GC-6），失败请求
// 也不一定写进 usage_logs（没有开启计费会话的失败没有日志行）。缺了这一行，
// 用户报 requestId 时无法知道是哪个渠道、模型、上游状态码，排障只能靠猜。
//
// r 只用于 Task 3b 的拒绝落库（身份/端点与「不随客户端断开取消」的 ctx）；
// 传 nil 时诊断日志照常写，拒绝记录跳过。
func (h *RelayHandler) logRelayFailure(r *http.Request, rc *contract.RelayContext, err error) {
	if h == nil || h.log == nil || rc == nil {
		return
	}
	status, code, message := mapRelayError(err)
	class, upstreamStatus, attempts := relayFailureDiagnostics(err)
	h.log.Warn("relay request failed",
		"request_id", rc.RequestID,
		"endpoint", relay.EndpointLabel(rc),
		"model", rc.OriginModel,
		"upstream_model", rc.UpstreamModel,
		"channel_id", rc.ChannelID,
		"channel_type", string(rc.ChannelType),
		"stream", rc.Streaming,
		"status", status,
		"error_code", code,
		"error_class", class,
		"upstream_status", upstreamStatus,
		"attempts", attempts,
	)
	// Task 3b：402/429 且**从未到达上游**才是「中继前拒绝」，补一条 refused 行。
	// 「未到达上游」的主判据是执行层没有记录任何 Attempt（len(rc.Attempts) != 0）。
	// upstreamStatus != 0 是纵深防御、当前不承重：上游 429 会被执行层聚合/映射
	// 成 502（见 mapRelayError），根本进不了下面的 402/429 分支，因此该条件今天
	// 不会决定任何结果；保留它只为防止将来错误映射改动后，上游状态以 429 形态
	// 重新到达这里而被误记成入口限流。
	if r == nil || h.refusals == nil || len(rc.Attempts) != 0 || upstreamStatus != 0 {
		return
	}
	stage := ""
	switch status {
	case http.StatusPaymentRequired:
		stage = billing.RefusalStageQuota
	case http.StatusTooManyRequests:
		stage = billing.RefusalStageRateLimit
	}
	if stage == "" {
		return
	}
	// 计费类错误不是 *UpstreamStatusError，relayFailureDiagnostics 因此返回空
	// 分类；但 mapRelayError 已经给出了同一套 error_class 词表里的 code
	//（insufficient_quota 等）。落库沿用 code 而不是留 NULL——拒绝行本来就
	// 没有上游状态码，分类是管理员唯一能看到的成因标签。
	errorClass := class
	if errorClass == "" {
		errorClass = code
	}
	h.recordRefusal(r, billing.Refusal{
		RequestID:    rc.RequestID,
		UserID:       int64(rc.UserID),
		TokenID:      refusalTokenID(rc.TokenID),
		Model:        rc.OriginModel,
		Endpoint:     contract.EndpointLabel(rc.RequestPath),
		Stage:        stage,
		ErrorClass:   errorClass,
		ErrorMessage: message,
		Structure:    rc.RequestStructure,
	})
}

// 中继前拒绝的阶段词表由 billing 包持有（billing.RefusalStage*）：写入方与
// 校验方必须是同一份定义，否则一边改名会让 RecordRefusal 静默拒收，拒绝行
// 直接丢失。这里不再重复声明。

// refusalTokenID 把 RelayContext 的 int 令牌语义（0 = 会话路径没有令牌）
// 转成 Refusal 的 *int64（nil = 无令牌）。不能对 0 取地址：那会把「无令牌」
// 写成「令牌 0」，两者在 usage_logs 里是可区分的。
func refusalTokenID(tokenID int) *int64 {
	if tokenID <= 0 {
		return nil
	}
	id := int64(tokenID)
	return &id
}

// recordRefusal 写入一条「未到达上游即被拒绝」的 usage_logs 行（Task 3b）。
//
// 所有拒绝点的唯一出口：调用方只给出 stage 与本点已知的事实，身份与端点标签
// 在这里补齐。落库失败只记 WARN 并吞掉——落库永不阻断请求，原始 HTTP 响应
// 仍由调用方照常写出。
func (h *RelayHandler) recordRefusal(r *http.Request, in billing.Refusal) {
	if h == nil {
		return
	}
	// 调用方没给身份时才回源 Principal。fail-closed 401 的站点本就没有
	// Principal：UserID 保持 0、TokenID 保持 nil（「无法说明是谁」是诚实
	// 结果，绝不能猜一个用户）。
	if principal, ok := auth.PrincipalFrom(r.Context()); ok {
		if in.UserID == 0 {
			in.UserID = principal.UserID
		}
		if in.TokenID == nil {
			in.TokenID = principal.TokenID
		}
	}
	recordPreRelayRefusal(h.refusals, h.log, r, in)
}

// recordPreRelayRefusal 是拒绝行的唯一落库出口（handler 与鉴权中间件共用）。
//
// 落库永不阻断请求：即使注入的实现 panic，也必须让调用方继续写原始 HTTP
// 响应（否则客户端会拿到断连而不是那条 4xx）。recover 只记 WARN。
// WithoutCancel：客户端断开不得取消落库；2s 上限避免拒绝行的写入拖住响应
// （超时即放弃，错误仍只记日志）。
func recordPreRelayRefusal(recorder RefusalRecorder, log *slog.Logger, r *http.Request, in billing.Refusal) {
	if recorder == nil || r == nil {
		return
	}
	defer func() {
		if recovered := recover(); recovered != nil && log != nil {
			log.Warn("relay refusal recorder panicked",
				"request_id", in.RequestID, "stage", in.Stage, "panic", recovered)
		}
	}()
	if in.Endpoint == "" && r.URL != nil {
		in.Endpoint = contract.EndpointLabel(r.URL.Path)
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Second)
	defer cancel()
	if err := recorder.RecordRefusal(ctx, in); err != nil && log != nil {
		log.Warn("relay refusal not recorded",
			"request_id", in.RequestID, "stage", in.Stage, "error", err)
	}
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
