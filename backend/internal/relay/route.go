package relay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/relay/adapters"
	"newtg/gateway/internal/relay/usage"
)

// ============================================================================
// 路由分派与重试（relay-01 §5.4–§5.7 / §6.1–§6.5；控制器裁决 R-P4 / R-P13 / R-P7）
//
// 本文件只做「选谁、试几次、失败怎么归类」，不解析请求体、不产出 IR、不实现流式
// 桥接（归一化/流式分别归 Task 8 与 relay-03）。依赖全部由 RouteDeps 注入：
// candidate 来源、渠道解析、适配器分派、HTTP 客户端与计费会话，因此本包不依赖
// store/config（depguard 的 relay-boundary），测试也不需要真实上游。
// ============================================================================

const (
	// maxErrorBodyBytes 是错误响应体的读取上限（64 KiB）。读取只为把 body 交给
	// adapter.ClassifyError（部分协议按 body 判别错误类），绝不进入错误文本/日志；
	// 有界读取同时避免恶意上游用超大错误体拖垮中继（GC-6）。
	maxErrorBodyBytes = 64 << 10

	// 默认客户端超时取 relay-01 §11.1 的冻结默认值。Client.Timeout 保持 0
	// （整体读取默认禁用）：流式/长响应不能被一个总时长掐断，响应头等待由
	// Transport.ResponseHeaderTimeout 负责。
	routeResponseHeaderTimeout = 1800 * time.Second
	routeIdleConnTimeout       = 90 * time.Second
	routeDialTimeout           = 30 * time.Second
	routeTLSHandshakeTimeout   = 10 * time.Second
	routeMaxIdleConns          = 500
	routeMaxIdleConnsPerHost   = 100
	routeExpectContinueTimeout = 1 * time.Second
)

var (
	// ErrChannelNotFound 表示 pin（legacy sk-<key>-<channelId> 后缀）指定的渠道
	// 不在候选集内。控制器裁决要求按 HTTP-404 语义快失败；非管理员使用 pin 的
	// 403 已由 BuildRelayContext 在执行到本层之前拒绝（R-P2a、relay-01 §3.5），
	// 本层绝不调用 ParseChannelPin，否则会绕过那道校验。
	ErrChannelNotFound = errors.New("relay: 指定渠道不存在")

	// ErrNoAvailableChannel 表示候选来源为空（或分组过滤后为空）。
	// 对应 relay-01 §2 阶段 3 的 503 get_channel_failed（SkipRetry）。
	ErrNoAvailableChannel = errors.New("relay: 无可用渠道候选")

	// ErrStreamingInRoute 表示调用方把流式请求交给了 Route。R-P7：Route 只负责
	// 非流式路径；流式调用方应先 ExecuteAttempts 拿到原始 2xx 响应，再走
	// relay/stream 的事件桥接。
	ErrStreamingInRoute = errors.New("relay: 流式请求必须使用 ExecuteAttempts 而非 Route")

	// ErrGroupRatioMissing 表示当前要尝试的分组没有配置倍率
	// （user_groups 缺失或 status != 1）。R-64 / I-25：倍率取不到必须显式
	// 失败（HTTP 403 group_ratio_missing），**绝不**按默认 1 计费。
	ErrGroupRatioMissing = errors.New("relay: 分组倍率未配置")

	// ErrGroupCheckFailed 表示分组配置校验的存储查询失败。fail-closed：
	// 查询失败既不能当作「已配置」放行，也不能当作「未配置」误导排障——
	// 端点层映射 500 database_error（不落 502 上游故障）。
	ErrGroupCheckFailed = errors.New("relay: 分组配置校验失败")

	errRelayContextMissing = errors.New("relay: 缺少中继上下文")
	errDependencyMissing   = errors.New("relay: 路由依赖缺失")
)

const (
	// defaultCrossGroupRetryMaxGroups 是 R-63 的跨组上限默认值（站点键
	// relay.cross_group_retry_max_groups 缺省/非法时使用）。
	defaultCrossGroupRetryMaxGroups = 3
	// MaxCrossGroupRetryGroups 是 R-63 的硬上限：无论配置多大，单请求最多
	// 尝试 20 个分组，防止 auto_groups 把上游调用量线性放大。
	MaxCrossGroupRetryGroups = 20
)

// RetryableError 标记一次**可重试**的尝试失败（上游瞬时故障/限流/5xx）。
//
// 它只描述单次尝试；重试预算耗尽后由 AttemptsError 聚合。Unwrap 保留底层
// 错误链，调用方仍可 errors.Is/As 到底层分类（如 context.Canceled）。
type RetryableError struct{ Err error }

func (e *RetryableError) Error() string {
	if e == nil || e.Err == nil {
		return "relay: 可重试的路由错误"
	}
	return e.Err.Error()
}

func (e *RetryableError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// FastFailError 标记**不得换渠道重试**的失败（R-P13 固定集合、渠道配置/分派
// 失败、预扣失败等）。快速失败直接上抛，不聚合为 AttemptsError，避免把
// 「不可重试」伪装成「重试耗尽」。
type FastFailError struct{ Err error }

func (e *FastFailError) Error() string {
	if e == nil || e.Err == nil {
		return "relay: 快速失败"
	}
	return e.Err.Error()
}

func (e *FastFailError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// AttemptsError 是重试预算耗尽后的聚合错误。
//
// Attempts 是实际消耗的尝试轮数（含首试；每轮对应一次候选选择，即使失败发生在
// 渠道解析/构造请求阶段也算一轮），Last 是最后一次尝试的错误。Error() 必须包含
// 次数：端点层据此区分「一次就失败」与「重试耗尽」，日志也需要该计数做故障归因。
//
// Groups 是 group=auto 请求实际进入过的分组序列（按尝试顺序，含无候选的空组）。
// R-66 要求重试耗尽时把序列追加到 error.message（"; groups tried: g1, g2"）；
// 具体分组令牌该字段保持 nil，错误体与 M1 逐字节一致。
type AttemptsError struct {
	Attempts int
	Last     error
	Groups   []string
}

func (e *AttemptsError) Error() string {
	if e == nil {
		return "relay: 尝试耗尽"
	}
	if e.Last == nil {
		return fmt.Sprintf("relay: %d 次尝试均失败", e.Attempts)
	}
	return fmt.Sprintf("relay: %d 次尝试均失败；最后一次: %v", e.Attempts, e.Last)
}

func (e *AttemptsError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Last
}

// UpstreamStatusError 是上游非 2xx 响应的**安全摘要**：只含状态码与契约错误类。
//
// 导出理由：契约 §4 的 RelayError 不是 error，而 §9.3 的「ErrorClass → 客户端
// 状态码」映射归端点层（Task 8）。FastFailError/AttemptsError 按任务书只暴露
// Err 字段，端点层需要 errors.As 到本类型才能拿到上游状态码。
// GC-6：文本与字段都不含 URL、凭据或响应体。
type UpstreamStatusError struct {
	StatusCode int
	Class      contract.ErrorClass
}

func (e *UpstreamStatusError) Error() string {
	return fmt.Sprintf("relay: 上游返回 HTTP %d（class=%d）", e.StatusCode, int(e.Class))
}

// RouteDeps 是路由/执行层的显式依赖（控制器裁决 R-P4）。
//
// 之所以不读全局单例：候选来源与渠道解析在 M2 尚无 store（本包被 depguard
// 禁止依赖持久化），且 GC-7 的预算释放必须能在测试里用 billingseam 桩断言。
type RouteDeps struct {
	// Candidates 返回原始候选（未排序）。当 rc.AttemptPlan 已由上游路由层写好时
	// 不会被调用——已有计划代表调用方已经完成选路（含 pin 约束）。
	Candidates func(ctx context.Context, rc *contract.RelayContext) ([]contract.ChannelCandidate, error)

	// ResolveChannel 按渠道 ID 取本轮渠道配置（基址/凭据/上游模型/倍率/建渠道时点）。
	//
	// 返回类型复用 context.go 的 ChannelAssignment（比 R-P4 草案多 ChannelID 与
	// MultiKeyIndex）：它是 WithChannel 的既有入参，另立同名类型会与「每轮整组写
	// 渠道组」（relay-01 §7.4）冲突。ChannelID 以候选为准，执行层每轮覆盖，
	// 防止解析结果与候选 ID 漂移。
	ResolveChannel func(ctx context.Context, channelID int) (ChannelAssignment, error)

	// Dispatch 按渠道类型取适配器；nil 时使用 adapters.Dispatch（两级分派表）。
	Dispatch func(contract.ChannelType) (contract.Adapter, error)

	// Billing 是计费会话；nil 表示不接入计费（仅测试/降级）。
	// 有值时：循环前 PreConsume 一次；最终失败/取消/panic 时 Refund 一次（GC-7/R-85）。
	Billing contract.BillingSession

	// Client 是上游 HTTP 客户端；nil 时使用按 relay-01 §11.1 冻结默认值构造的
	// 包级客户端（不跟随重定向、ResponseHeaderTimeout 1800s）。
	Client *http.Client

	// MaxRetries 是重试次数（不含首试）：0 取默认 3，随后钳制到 [1,5]
	// （GC-5 / C-9/D11）。总尝试次数 = 钳制后的值 + 1（relay-01 §6.1）。
	MaxRetries int

	// MaxAutoGroups 是 R-63 的跨组尝试分组数上限（装配处已解析并钳制到
	// [1,20]）。0 表示未设置 → 默认 3。它只约束**分组数**，组内重试预算
	// 仍由 MaxRetries 独立决定（R-63：组内预算不合并）。
	MaxAutoGroups int

	// GroupConfigured 严格校验某分组已配置且启用（R-64 / I-25：
	// user_groups WHERE name=? AND status=1）。每组首次尝试前调用；返回
	// false → ErrGroupRatioMissing（403），返回 error → ErrGroupCheckFailed
	// （500 fail-closed）。nil 表示跳过校验（仅测试/无计费降级装配）。
	GroupConfigured func(ctx context.Context, group string) (bool, error)
}

// GroupSequence 返回本次请求的**有效分组序列**（R-61/R-62/R-63/R-68）。
//
// 首元素 = rc.UsingGroup（鉴权层由 AutoGroups[0] 写入）。仅当 CrossGroupRetry
// 为真且令牌带 AutoGroups 时追加后续组：顺序 = 提交顺序（I-22），重复组按首现
// 去重，空串与 "auto" 哨兵被跳过（它们不可能是真实分组）。组数受 maxGroups
// 与 R-63 的 20 硬上限约束；开关关闭时只返回首组——路由层不得自行放大范围。
//
// 端点层（/v1/messages 方向预检，R-68）与执行层共用本函数，保证「预检认为
// 可达的分组」与「执行层真正会尝试的分组」是同一序列。
func GroupSequence(rc *contract.RelayContext, maxGroups int) []string {
	if rc == nil {
		return nil
	}
	limit := maxGroups
	if limit <= 0 {
		limit = defaultCrossGroupRetryMaxGroups
	}
	if limit > MaxCrossGroupRetryGroups {
		limit = MaxCrossGroupRetryGroups
	}
	// 首元素永远是 UsingGroup（即使为空：只有测试/内部直调会这样，
	// store.Candidates 对空分组按「不过滤」处理）。直调方只填 AutoGroups
	// 时以首项为准（R-61：UsingGroup 的初值 = AutoGroups[0]）；自动分组
	// 序列只在 CrossGroupRetry 开启时追加。
	first := rc.UsingGroup
	if first == "" && len(rc.AutoGroups) > 0 {
		first = rc.AutoGroups[0]
	}
	groups := make([]string, 0, limit)
	groups = append(groups, first)
	if !rc.CrossGroupRetry || len(rc.AutoGroups) == 0 {
		return groups
	}
	for _, group := range rc.AutoGroups {
		if group == "" || group == groupAuto {
			continue
		}
		duplicate := false
		for _, seen := range groups {
			if seen == group {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		if len(groups) >= limit {
			break
		}
		groups = append(groups, group)
	}
	return groups
}

// AttemptResult 是一次成功尝试的原始产物。
//
// 保留原始 *http.Response 与适配器，是为了让流式路径（R-P7 / Task 8）能直接
// 消费 2xx 响应体；Route 则在内部把它解析为 contract.Response。
type AttemptResult struct {
	Response  *http.Response
	Adapter   contract.Adapter
	Candidate contract.ChannelCandidate
	Channel   ChannelAssignment
}

// defaultRouteClient 是生产路径的默认上游客户端。
//
// 单例而不是每次请求新建：Transport 持有连接池，按请求重建会退回短连接，
// 且 §11.1 的连接池上限（500 / 每主机 100）只有在长期复用时才有意义。
var defaultRouteClient = newDefaultRouteClient()

func newDefaultRouteClient() *http.Client {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   routeDialTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          routeMaxIdleConns,
		MaxIdleConnsPerHost:   routeMaxIdleConnsPerHost,
		IdleConnTimeout:       routeIdleConnTimeout,
		TLSHandshakeTimeout:   routeTLSHandshakeTimeout,
		ExpectContinueTimeout: routeExpectContinueTimeout,
		ResponseHeaderTimeout: routeResponseHeaderTimeout,
	}
	return &http.Client{
		Transport: transport,
		// relay-01 §6.3 的重试区间把 3xx 当作上游异常（重定向已禁用）：
		// 不跟随跳转，把 3xx 原样交给状态分类。
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		// Client.Timeout 保持 0：整体读取默认禁用（§11.1），长响应由调用方
		// 的 context 与块间看门狗（流式路径）控制。
	}
}

// ExecuteAttempts 按 AttemptPlan 逐轮执行上游请求，直到：
//   - 拿到 2xx：返回原始响应 + 适配器（不解析、不终结计费会话）；
//   - 命中快失败：返回 *FastFailError；
//   - 上下文取消：立即停止并返回包裹 context 错误的 *AttemptsError；
//   - 预算耗尽：返回 *AttemptsError。
//
// 计费生命周期：PreConsume 成功之后的所有失败/取消/panic 出口都会 Refund 一次
// （GC-7/R-85：失败整笔不收费；Terminate(零事实) 会让按次模型照常扣费）；
// PreConsume 之前的失败（计划/pin/依赖校验/分组倍率校验，或请求在预扣前已取消）
// 没有开启会话，因此**不调用** Refund。
//
// 跨组重试（R-62，W7/M2）：rc.CrossGroupRetry 且令牌带 AutoGroups 时按
// GroupSequence 逐组尝试；每组拥有独立的 RetryTimes+1 预算（组内预算不合并），
// 组内耗尽后换下一组并重新调用 deps.Candidates 构计划（不复用旧候选）。换组是
// WithUsingGroup 的唯一新改写点；pin 只在首组解析、不跨组。空候选集的分组视为
// 耗尽并继续换组（TC-F-02-13-01），全部组都为空时以 ErrNoAvailableChannel 收尾。
//
// 职责边界（控制器裁决 R-P4）：候选排序、每轮渠道组整组写入、BuildRequest + Do、
// HTTP 错误分类、重试/快失败判定、循环前一次性 PreConsume、最终失败 Terminate
// 都在这里。成功时**不** Terminate——最终 UsageFact 只有解析响应后才存在，
// 由 Route（或流式路径的调用方）负责结算。
func ExecuteAttempts(ctx context.Context, rc *contract.RelayContext, deps RouteDeps) (*AttemptResult, error) {
	if rc == nil {
		return nil, &FastFailError{Err: errRelayContextMissing}
	}
	// 已取消的请求不值得选路/预扣：直接以 0 次尝试上抛，避免产生任何计费副作用。
	if err := ctx.Err(); err != nil {
		return nil, &AttemptsError{Attempts: 0, Last: err}
	}
	if deps.ResolveChannel == nil {
		return nil, &FastFailError{Err: fmt.Errorf("%w: ResolveChannel", errDependencyMissing)}
	}

	// 重试预算是**单一来源**：先解析「0 = 未设置 → GC-5 默认 3」，再钳制 [1,5]。
	// 计划构建与执行循环共用同一个值，避免两处 default 分叉。
	// 控制器裁决（2026-10-18）：Go 零值无法区分「未设置」与「显式 0」，故 deps 里的
	// 0 一律视为未设置、落到默认 3；想要最小重试预算的调用方必须显式传 1。
	retries := deps.MaxRetries
	if retries == 0 {
		retries = defaultRetries
	}
	maxAttempts := ClampRetries(retries) + 1

	// —— 0. 有效分组序列（R-62/R-63/R-68）——
	groups := GroupSequence(rc, deps.MaxAutoGroups)
	// 只有 auto 令牌（AutoGroups 非空）才记录已尝试分组并附加 R-66 序列；
	// 具体分组令牌的失败体必须与 M1 逐字节一致（M2 回归项）。
	recordGroups := len(rc.AutoGroups) > 0
	crossGroup := rc.CrossGroupRetry && recordGroups && len(groups) > 1
	if len(groups) > 0 && rc.UsingGroup != groups[0] {
		// 防御：鉴权层已保证 UsingGroup == AutoGroups[0]（R-61）。直接构造
		// RelayContext 的调用方（测试/内部）只写 AutoGroups 时在此对齐，否则
		// 候选查询会落到错误分组；同一处也同步预扣估算的分组（尚未预扣）。
		WithUsingGroup(rc, groups[0])
		if rc.QuotaEstimate != nil && rc.QuotaEstimate.Group != "" {
			rc.QuotaEstimate.Group = groups[0]
		}
	}

	// —— 1. 首组计划（已有计划优先，代表上游路由层已完成选路）——
	var firstPlan *contract.AttemptPlan
	if rc.AttemptPlan != nil {
		firstPlan = rc.AttemptPlan
	} else {
		plan, err := fetchAttemptPlan(ctx, rc, deps, retries)
		if err != nil {
			return nil, err
		}
		firstPlan = plan
	}
	ordered, pinned, pinSingleAttempt, err := preferPinned(firstPlan.Candidates)
	if err != nil {
		return nil, err
	}
	// R-62：pin 只在首个分组内解析；带 pin 的请求不跨组（即使 RetryMode 不是
	// single_attempt，耗尽组内预算后也停在首组）。
	effective := &contract.AttemptPlan{
		Candidates:  ordered,
		MaxAttempts: maxAttempts,
		CrossGroup:  firstPlan.CrossGroup,
	}
	priorities := attemptPriorities(ordered)
	sawCandidates := len(ordered) > 0
	// 路由组（relay-01 §7.3）：把本轮真正执行的计划写回上下文供审计/后续层读取。
	WithRoute(rc, effective, 0, nil)

	// GC-7：预扣之后的所有失败出口都必须释放预算。Refund 自身幂等（契约 §6.1），
	// 但本函数仍用本地标志保证恰好一次：显式失败出口立即释放；defer 只兜底
	// 「没有走显式释放且没有成功」的出口——panic 不经过任何 return，只能靠 defer。
	// 修复 load-bearing finding：此前 releaseBudget 只挂在显式 error return 上，
	// PreConsume 之后任意环节 panic 会永久泄漏预留。
	//
	// 【R-85：释放必须走 Refund，不是 Terminate(零事实)】失败/取消/panic 是
	// 「整笔不收费」语义。Terminate 会按真实计价求值：按次（per_call）模型与
	// 常量表达式模型即使 UsageFact 全零也照常扣费，并把日志写成 success——
	// 这正是 T9 复审复现的「失败请求照常扣 150 积分」。Refund 全额退回
	// hold 与 ChargeIncrement 增量，并把 usage_logs 收敛为 refunded。
	//
	// preConsumed 守卫：PreConsume 之前的失败（含 R-64 的首组倍率校验、空候选
	// 计划）没有开启会话，契约 §6.1 要求不调用 Refund。
	preConsumed := false
	released := false
	releaseBudget := func() {
		if released || deps.Billing == nil || !preConsumed {
			return
		}
		released = true
		_ = deps.Billing.Refund()
	}
	// succeeded 在唯一成功出口（2xx）返回前置位：成功时预算由调用方结算
	// （Route / 流式端点），defer 不得释放。
	succeeded := false
	defer func() {
		if !succeeded {
			releaseBudget()
		}
	}()

	dispatch := deps.Dispatch
	if dispatch == nil {
		dispatch = adapters.Dispatch
	}
	client := deps.Client
	if client == nil {
		client = defaultRouteClient
	}

	attempts := 0 // 全局尝试序号（R-62 补充口径：AttemptIndex 反映整笔请求）
	var lastErr error
	var tried []string // 已进入的分组序列（仅 auto 请求写入 AttemptsError.Groups）
	stopAll := false

	for groupIdx := 0; groupIdx < len(groups); groupIdx++ {
		group := groups[groupIdx]
		if groupIdx > 0 {
			// 客户端断开后不再切换分组：换组会继续消耗上游与预算。
			if err := ctx.Err(); err != nil {
				lastErr = err
				stopAll = true
				break
			}
			// R-62：换组是路由层对 UsingGroup 的唯一新改写点（relay-01 §7.1）。
			WithUsingGroup(rc, group)
			// 首组无候选而提前换组时预扣尚未发生：估算分组必须跟着换，
			// 否则 PreConsume 会按旧组倍率冻结 hold（R-64/R-65 的资损面）。
			if !preConsumed && rc.QuotaEstimate != nil && rc.QuotaEstimate.Group != "" {
				rc.QuotaEstimate.Group = group
			}
			// R-62：每次换组后重新查询候选构计划，不得复用旧候选。
			plan, err := fetchAttemptPlan(ctx, rc, deps, retries)
			if err != nil {
				releaseBudget()
				return nil, err
			}
			ordered, _, _, err = preferPinned(plan.Candidates)
			if err != nil {
				releaseBudget()
				return nil, err
			}
			effective = &contract.AttemptPlan{
				Candidates:  ordered,
				MaxAttempts: maxAttempts,
				CrossGroup:  plan.CrossGroup,
			}
			priorities = attemptPriorities(ordered)
			WithRoute(rc, effective, attempts, nil)
			if len(ordered) > 0 {
				sawCandidates = true
			}
		}

		if len(ordered) == 0 {
			// R-62：空候选集的分组视为耗尽并继续换组（TC-F-02-13-01：
			// g1 无可用渠道 → g2）。只有**尚未产生任何错误**时才把
			// ErrNoAvailableChannel 记为最终错误；否则保留最后一次真实失败
			// （modules-02 §2.10：达到上限返回最后一次失败的原始状态码）。
			if recordGroups {
				tried = append(tried, group)
			}
			if lastErr == nil {
				lastErr = ErrNoAvailableChannel
			}
			continue
		}

		// R-64 / I-25：每组首次尝试前严格校验倍率已配置且启用。首组校验
		// 先于 PreConsume；后续组在切换时校验。未被尝试的组缺失不阻塞。
		// 仅对 auto 令牌生效：具体分组令牌保持 M1 行为（M2 回归项）。
		if recordGroups && deps.GroupConfigured != nil {
			configured, checkErr := deps.GroupConfigured(ctx, group)
			if checkErr != nil {
				releaseBudget()
				// GC-6：存储错误可能带 DSN/SQL，只经 safeWrapf 暴露固定描述。
				return nil, &FastFailError{Err: fmt.Errorf("%w: %w",
					ErrGroupCheckFailed, safeWrapf(checkErr, "relay: 分组配置校验失败"))}
			}
			if !configured {
				releaseBudget()
				return nil, &FastFailError{Err: fmt.Errorf("%w: %s", ErrGroupRatioMissing, group)}
			}
		}

		// —— 3. 循环前一次性预扣（relay-01 §6.1；契约 §6.1）——
		// nil 估算按零值传递：接缝不做定价（GC-3），零值表示「调用方未提供估算」，
		// 由真实计费实现（M3）决定如何处理，路由层不臆造金额。
		if !preConsumed && deps.Billing != nil {
			estimate := contract.QuotaEstimate{}
			if rc.QuotaEstimate != nil {
				estimate = *rc.QuotaEstimate
			}
			if err := deps.Billing.PreConsume(estimate); err != nil {
				// PreConsume 失败不开启会话（契约 §6.1），无需 Terminate；
				// 快失败保留 errors.Is 链上的 402 语义（billingseam.ErrInsufficientQuota）。
				// GC-6：计费/存储错误可能带连接串等敏感信息，经 safeWrapf 只暴露固定描述
				// （safeError 的 Unwrap 仍保留完整错误链，故 errors.Is 不受影响）。
				return nil, &FastFailError{Err: safeWrapf(err, "relay: 预扣费失败")}
			}
			preConsumed = true
		}
		if recordGroups {
			tried = append(tried, group)
		}

		// 组内预算 = ClampRetries(RetryTimes)+1（R-62/R-63：组内预算不合并）。
		for i := 0; i < maxAttempts; i++ {
			// 每次换轮前检查取消：客户端断开后不再消耗重试预算。
			if err := ctx.Err(); err != nil {
				lastErr = err
				stopAll = true
				break
			}

			// 每轮选谁（relay-01 §5.2–§5.3 / §6.4）：
			//   - pin 的首轮固定选 pin 候选（§5.4 A 步），不受加权随机影响；
			//   - 其余轮次先按 §6.4 把尝试索引映射到目标优先级，再在该优先级内
			//     按权重加权随机（SelectWithinPriority，全零权重退化为均匀）。
			// 修复 final-review finding 1：原实现直接 Next(i) 确定性取候选，导致同
			// 优先级里权重最高的渠道吃掉全部流量，权重语义形同虚设。
			//
			// R-62 补充口径：组内优先级映射用**组内序号 i**；WithRoute 的
			// AttemptIndex 用**全局尝试序号**，让 usage_logs/UsageFact.Attempts
			// 反映整笔请求实际打过的上游次数（跨组后不再从 0 重计）。
			var candidate contract.ChannelCandidate
			if pinned && groupIdx == 0 && i == 0 {
				candidate = ordered[0]
			} else {
				target := priorities[min(i, len(priorities)-1)]
				candidate = SelectWithinPriority(candidatesAtPriority(ordered, target), selectionUint64N)
			}
			attemptIndex := attempts
			attempts++
			WithRoute(rc, effective, attemptIndex, nil)

			// 每轮整组解析并写入渠道组（relay-01 §7.4）：WithChannel 覆盖全部字段，
			// 上一轮的 Credential/UpstreamModel/MultiKeyIndex 不会残留。
			assignment, resolveErr := deps.ResolveChannel(ctx, candidate.ChannelID)
			if resolveErr != nil {
				if err := ctx.Err(); err != nil {
					lastErr = err
					stopAll = true
					break
				}
				// 本轮解析失败时先把渠道组清空（只留候选 ID）：否则上一轮的
				// APIBase/Credential/UpstreamModel 会滞留在上下文里，被审计或日志
				// 误记到本轮（relay-01 §7.4 的「整组写入」语义要求不残留）。
				WithChannel(rc, ChannelAssignment{ChannelID: candidate.ChannelID})
				// 渠道解析失败（索引陈旧/渠道已删）是「这一轮不可用」：换下一个候选。
				// 走重试预算而不是无限扫描，避免坏索引把单请求拖成线性放大。
				lastErr = &RetryableError{Err: safeWrapf(resolveErr, "relay: 渠道 %d 解析失败", candidate.ChannelID)}
				WithRoute(rc, effective, attemptIndex, &contract.RelayError{
					Class:     contract.ErrUnknown,
					Code:      "channel:resolve_failed",
					SkipRetry: pinSingleAttempt,
					Message:   "渠道解析失败",
				})
				if pinSingleAttempt {
					stopAll = true
					break
				}
				continue
			}
			// ChannelID 以候选为准（ResolveChannel 的返回可能不含 ID）；ChannelType
			// 来自解析结果，分派与 BuildRequest 共用同一个值。
			assignment.ChannelID = candidate.ChannelID
			WithChannel(rc, assignment)

			adapter, dispatchErr := dispatch(assignment.ChannelType)
			if dispatchErr != nil {
				// 契约 §4 冻结 ErrEndpointUnsupported「不重试、不自动禁用」；
				// 换渠道重试与该分类冲突，故整体快失败（错误文本经安全包装，GC-6）。
				// R-62：适配器分派失败同时是**不换组**的硬停止。
				releaseBudget()
				return nil, &FastFailError{Err: safeWrapf(dispatchErr, "relay: 渠道 %d 的适配器分派失败", candidate.ChannelID)}
			}

			httpReq, buildErr := adapter.BuildRequest(rc, rc.Request)
			if buildErr != nil {
				if err := ctx.Err(); err != nil {
					lastErr = err
					stopAll = true
					break
				}
				class, _ := adapters.ClassOf(buildErr)
				WithRoute(rc, effective, attemptIndex, &contract.RelayError{
					Class:     class,
					Code:      "channel:build_request_failed",
					SkipRetry: !retryableErrorClass(class),
					Message:   "上游请求构造失败",
				})
				if retryableErrorClass(class) {
					lastErr = &RetryableError{Err: safeWrapf(buildErr, "relay: 构造上游请求失败（渠道 %d）", candidate.ChannelID)}
					if pinSingleAttempt {
						stopAll = true
						break
					}
					continue
				}
				// 无分类的构造错误视为编程/配置错误：重试不会改变结果。
				releaseBudget()
				return nil, &FastFailError{Err: safeWrapf(buildErr, "relay: 构造上游请求失败（渠道 %d）", candidate.ChannelID)}
			}

			// 适配器拿不到 ctx（契约 §3.1 的 BuildRequest 无 ctx 参数），执行层负责
			// 把本轮 context 绑到请求上；否则客户端取消无法中止上游调用。
			httpReq = httpReq.WithContext(ctx)

			resp, doErr := client.Do(httpReq)
			if doErr != nil {
				// 上下文取消/超时优先判定：停止循环并释放预算（R-P13）。
				if err := ctx.Err(); err != nil {
					lastErr = err
					stopAll = true
					break
				}
				// 其余传输层错误（连接失败、响应头超时等）按 R-P13 一律可重试。
				// GC-6：url.Error 的文本含完整 URL，故只保留安全摘要，不渲染底层文本。
				lastErr = &RetryableError{Err: safeWrapf(doErr, "relay: 上游传输失败（渠道 %d）", candidate.ChannelID)}
				WithRoute(rc, effective, attemptIndex, &contract.RelayError{
					Class:     contract.ErrRetryable,
					Code:      "do_request_failed",
					SkipRetry: pinSingleAttempt,
					Message:   "上游传输失败",
				})
				if pinSingleAttempt {
					stopAll = true
					break
				}
				continue
			}

			if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
				succeeded = true // 交由调用方结算；defer 不得释放
				return &AttemptResult{
					Response:  resp,
					Adapter:   adapter,
					Candidate: candidate,
					Channel:   assignment,
				}, nil
			}

			// 非 2xx：有界读取错误体只为分类（adapter.ClassifyError 可能需要它），
			// 不进入错误文本（GC-6），随后关闭以复用/释放连接。
			body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
			_ = resp.Body.Close()
			class := adapter.ClassifyError(resp.StatusCode, body)
			statusErr := &UpstreamStatusError{StatusCode: resp.StatusCode, Class: class}
			WithRoute(rc, effective, attemptIndex, &contract.RelayError{
				Class:        class,
				StatusCode:   resp.StatusCode,
				SkipRetry:    !retryableStatus(resp.StatusCode),
				IsChannelErr: class == contract.ErrChannelError,
				Message:      statusErr.Error(),
			})

			if !retryableStatus(resp.StatusCode) {
				// R-P13 固定集合：400/401/403/422、408/504/524 绝不换渠道重试，
				// 同时是 R-62 的**不换组**硬停止（非可重试状态码）。
				releaseBudget()
				return nil, &FastFailError{Err: statusErr}
			}
			lastErr = &RetryableError{Err: statusErr}
			if pinSingleAttempt {
				// relay-01 §6.2：pin.RetryMode == single_attempt 时固定渠道只试一次。
				stopAll = true
				break
			}
		}

		// 组内预算耗尽或硬停止：只有「可继续」时才进入下一个分组。
		//   - stopAll：取消/pin 单次尝试等硬停止；
		//   - !crossGroup：开关关闭或序列只有一组（R-62/R-63）；
		//   - pinned：pin 只在首组解析，不跨组（R-62）。
		if stopAll || !crossGroup || pinned {
			break
		}
	}

	// GC-7 / relay-01 §2 阶段 15：预算耗尽或取消，释放预留后返回聚合错误。
	releaseBudget()
	if lastErr == nil {
		lastErr = ErrNoAvailableChannel // 防御：循环未产生错误就退出说明上游计划为空
	}
	if !sawCandidates && !recordGroups {
		// 具体分组令牌（非 auto）：保持 M1 的 FastFail/503 逐字节语义。
		return nil, &FastFailError{Err: ErrNoAvailableChannel}
	}
	var groupsTried []string
	if recordGroups {
		groupsTried = tried
	}
	return nil, &AttemptsError{Attempts: attempts, Last: lastErr, Groups: groupsTried}
}

// fetchAttemptPlan 查询候选并构建一个分组的尝试计划。
//
// 候选查询失败是选路失败（阶段 3，SkipRetry），不能当作上游瞬时故障重试；
// GC-6：store 类错误可能带 DSN/主机名/凭据，必须经 safeWrapf 只暴露固定描述。
func fetchAttemptPlan(ctx context.Context, rc *contract.RelayContext, deps RouteDeps, retries int) (*contract.AttemptPlan, error) {
	if deps.Candidates == nil {
		return nil, &FastFailError{Err: fmt.Errorf("%w: Candidates", errDependencyMissing)}
	}
	candidates, err := deps.Candidates(ctx, rc)
	if err != nil {
		return nil, &FastFailError{Err: safeWrapf(err, "relay: 候选渠道查询失败")}
	}
	return BuildAttemptPlanWithRetries(rc, candidates, retries), nil
}

// Route 执行一次**非流式**中继：ExecuteAttempts → ParseResponse → usage.ExtractUsage
// → Terminate(fact)（成功）；解析失败/panic → Refund()（R-85）。
//
// 用量事实统一由 usage.ExtractUsage 产出（R-05 三源优先级：原生 usage > 注入 >
// `x-model-response-usage` 头），本函数是它在**非流式路径**的生产调用方；
// 流式路径的跨块累计与头部兜底属于**端点层**职责（流式路径不经过 Route）。
//
// R-P7：Route 明确拒绝 rc.Streaming。流式调用方必须使用 ExecuteAttempts 拿到
// 原始 2xx *http.Response，再交给 relay/stream 的事件桥接（流式 usage 提取归
// Task 7）；在 Route 内解析 SSE 会把两条路径的失败语义混在一起。
func Route(ctx context.Context, rc *contract.RelayContext, deps RouteDeps) (*contract.Response, error) {
	if rc == nil {
		return nil, &FastFailError{Err: errRelayContextMissing}
	}
	if rc.Streaming {
		return nil, &FastFailError{Err: ErrStreamingInRoute}
	}
	start := time.Now()

	result, err := ExecuteAttempts(ctx, rc, deps)
	if err != nil {
		return nil, err
	}
	// ExecuteAttempts 成功时把响应体所有权交给调用方；Route 解析完即关闭。
	defer func() { _ = result.Response.Body.Close() }()

	// GC-7 panic 安全（final-review finding 10）：PreConsume 之后会话已开启，
	// 任何退出路径（含 ParseResponse/事实构建里的 panic）都必须恰好终结一次。
	// 用与 httpapi.bridgeStream 相同的 finalized 幂等标志 + defer 兜底：
	//   - 成功解析并拿到最终事实 → Terminate(finalFact)；
	//   - 解析失败 / panic 等失败出口 → Refund()（R-85：失败整笔不收费，
	//     绝不能把零值事实交给 Terminate 按按次价/常量表达式照常扣费）。
	var (
		finalized bool
		finalFact contract.UsageFact
		settle    bool
	)
	finish := func(refund bool) {
		if finalized || deps.Billing == nil {
			return
		}
		finalized = true
		// relay-01 §2 阶段 14：结算失败记日志、不阻断响应。本层没有 logger，
		// 故丢弃错误；响应已完整解析，客户端不应因结算失败拿不到内容。
		if refund {
			_ = deps.Billing.Refund()
			return
		}
		_ = deps.Billing.Terminate(finalFact)
	}
	defer func() { finish(!settle) }()

	response, adapterUsage, parseErr := result.Adapter.ParseResponse(rc, result.Response)
	if parseErr != nil {
		// 上游 2xx 但响应不可用：会话已开启，defer 以 Refund 释放预算（GC-7/R-85）。
		// 这里不把解析失败标成 FastFailError：它不是 4xx 判定（契约 §4），
		// 用安全包装保留适配器错误链供端点层读取分类（§9.3）。
		return nil, safeWrapf(parseErr, "relay: 解析上游响应失败（渠道 %d）", rc.ChannelID)
	}

	// 用量提取按渠道族分派（M4 T4 接线）：OpenAI 形状与历史行为一致，
	// Anthropic/Gemini 原生渠道走 T3 的 ExtractUsageMultimodal——否则
	// `input_tokens`/`usageMetadata` 会被 OpenAI 解析器读成零值，且缓存/推理
	// 的关系布尔方向可能被误判（成倍错算费用）。
	fact := usage.ExtractUsageMultimodal(response, result.Response.Header, nil, rc.ChannelType)
	if fact.Usage.Source == contract.UsageEstimated {
		// 三源全部缺席时 ExtractUsage 只给出零值 + estimated；但关系布尔是
		// **协议属性**而非用量数字（relay-03 §6.2 row 1）：OpenAI 格式的 prompt
		// 已含缓存读、completion 已含推理 token，必须从适配器归一结果保留为
		// true。否则「无 usage 的上游」会让计费侧把 CacheRead/Reasoning 当独立
		// 维度重复加价——这是对适配器原行为的回归。
		fact.Usage.PromptIncludesCache = adapterUsage.PromptIncludesCache
		fact.Usage.ReasoningIncludedInCompletion = adapterUsage.ReasoningIncludedInCompletion
	}
	// 计费按**客户端请求模型**取价（billing-02 §「OriginModelName 为起点」）；
	// 上游声明的模型名保留在 Response.Model 供对账（契约 §2.3），不进计费键。
	if rc.OriginModel != "" {
		fact.Model = rc.OriginModel
	} else if response != nil && fact.Model == "" {
		fact.Model = response.Model
	}
	fact.LatencyMS = int(time.Since(start).Milliseconds())
	fact.Stream = false
	fact.Attempts = rc.AttemptIndex + 1
	// FirstTokenMS 保持 nil：非流式没有首 token 时刻（契约 §6.1 · 决议 B6）。
	finalFact = *fact
	settle = true // defer 走 Terminate；失败/panic 出口保持 false → Refund
	return response, nil
}

// preferPinned 把 pin 候选提到首位（控制器裁决：prefer a candidate with that ID）。
//
// 只做重排、不扩大候选集：pin 不在候选里说明它被下层过滤（禁用/分组/过滤器）或
// 根本不存在，返回 ErrChannelNotFound 快失败（HTTP-404 语义）。绝不在这里按
// pin 去数据库另取渠道，否则会绕过 relay-01 §5.4 D 步的过滤兜底。
//
// 返回的 pinned 表示候选中确实带 pin 约束（ordered[0] 即 pin 候选）；执行层的
// 首轮据此跳过加权随机，直接命中 pin（§5.4 A 步）。singleAttempt 是
// RetryMode == single_attempt（§6.2）。
//
// 返回的切片要么是输入本身（无 pin 或 pin 已在首位），要么是新副本：
// 输入常来自路由索引缓存，原地交换会污染其他并发请求（不修改输入切片）。
func preferPinned(candidates []contract.ChannelCandidate) (ordered []contract.ChannelCandidate, pinned bool, singleAttempt bool, err error) {
	pinID := 0
	for _, candidate := range candidates {
		if candidate.Constraints.PinChannelID != 0 {
			pinID = candidate.Constraints.PinChannelID
			break
		}
	}
	if pinID == 0 {
		return candidates, false, false, nil
	}

	index := -1
	for i, candidate := range candidates {
		if candidate.ChannelID == pinID {
			index = i
			break
		}
	}
	if index < 0 {
		return nil, false, false, &FastFailError{Err: fmt.Errorf("%w: channel %d", ErrChannelNotFound, pinID)}
	}

	singleAttempt = candidates[index].Constraints.RetryMode == contract.PinRetrySingleAttempt
	if index == 0 {
		return candidates, true, singleAttempt, nil
	}
	ordered = make([]contract.ChannelCandidate, 0, len(candidates))
	ordered = append(ordered, candidates[index])
	ordered = append(ordered, candidates[:index]...)
	ordered = append(ordered, candidates[index+1:]...)
	return ordered, true, singleAttempt, nil
}

// retryableStatus 实现 relay-01 §6.3 的三态判定（第 3–5 步）并把 R-P13 的
// 固定不重试集合覆盖在其上：
//
//   - 400 / 401 / 403 / 422 不重试（brief 的「4xx 快失败」经控制器收敛到这四个）；
//   - 408 / 504 / 524 不重试（§6.2 的固定集合；不得被「超时」措辞误判为可重试）；
//   - 429 与 5xx（除 504/524）可重试；
//   - 其余状态落在 §6.3 的重试区间（100–199、300–399、401–407、409–499、
//     500–503、505–523、525–599）内，一律可重试。
//
// 注意 404 在基线的 401–407 区间内，属于可重试——这是照搬的乙类怪癖；
// adapter.ClassifyError 可能把 404 归为 ErrParamInvalid，但 R-P13 冻结的重试
// 决策以状态集合为准，分类只用于错误映射/自动禁用。
//
// 【M2 有意偏离 relay-01 §6.3 第 1 步（控制器裁决，2026-10-18，已记台账）】
// §6.3 规定 ErrChannelError 是「最先判定」的可重试通道，即便状态在固定集合内
// 也要重试。M2 明确让 R-P13 的固定集合**优先于 adapter 分类**：400/401/403/422/
// 408/504/524 即使被适配器归为 ErrChannelError 也一律快失败。M3 若恢复 §6.3
// 第 1 步，必须同步修改本函数与对应测试，不能只改分类分支。
func retryableStatus(status int) bool {
	switch status {
	case http.StatusBadRequest, // 400
		http.StatusUnauthorized,        // 401
		http.StatusForbidden,           // 403
		http.StatusRequestTimeout,      // 408
		http.StatusUnprocessableEntity, // 422
		http.StatusGatewayTimeout,      // 504
		524:                            // Cloudflare 超时（net/http 无常量）
		return false
	default:
		return true
	}
}

// retryableErrorClass 把契约 §4 的错误类映射为重试决策（仅用于**非 HTTP**错误，
// 如 BuildRequest 失败）。§6.3 第 1 步：ErrChannelError 强制可重试。
// 其余类别（参数/鉴权/配置/内容审核/额度/端点不支持）一律不重试。
func retryableErrorClass(class contract.ErrorClass) bool {
	switch class {
	case contract.ErrRetryable,
		contract.ErrUpstreamRateLimit,
		contract.ErrUpstreamServer,
		contract.ErrChannelError:
		return true
	default:
		return false
	}
}

// safeError 是 GC-6 的安全包装：Error() 只渲染调用方给出的固定描述，
// 底层错误仅通过 Unwrap 参与 errors.Is/As，不参与文本。
//
// 必要性：Go 的 *url.Error 形如 `Post "https://host/path": dial tcp ...`，
// 直接把传输错误拼进上层错误就会把 APIBase 写进日志/响应。适配器虽然也
// 承诺错误文本不含 URL/凭据，但执行层是最后一道出口，必须自己兜底。
type safeError struct {
	message string
	cause   error
}

func (e *safeError) Error() string { return e.message }

func (e *safeError) Unwrap() error { return e.cause }

func safeWrapf(cause error, format string, args ...any) error {
	return &safeError{message: fmt.Sprintf(format, args...), cause: cause}
}
