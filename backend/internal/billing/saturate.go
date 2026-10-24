package billing

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/billing/pricing"
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/perfmetrics"
)

// PerfMetricsRecorder 是模型性能采集的写入接缝（G-M6C-13）。
//
// 定义在 billing 侧而不是直接依赖 *perfmetrics.Collector，是为了让单测可以
// 注入计数 fake；生产由 platform 注入采集器，PERF_METRICS_ENABLED=false 时
// 保持 nil（热路径只多一次 nil 判断，零额外分配）。
type PerfMetricsRecorder interface {
	Observe(perfmetrics.Observation)
}

// DBLogger 是 usage_logs 的落库实现（Task 3 的 billing.Saturator）。
//
// 日志库独立于主库（DB_LOGS_DSN）：本类型只持有日志库句柄，**不做也不宣称
// 任何跨库事务**。写入失败返回 error，由 Terminate 决定「不结算、保留 hold」
// 或由 worker 重试（见 engine.go 中 Saturator 的注释）。
type DBLogger struct {
	logsDB *sql.DB
	// feed 是公开首页 live_feed 的投递接缝（modules-09 F-09-02）。nil 表示
	// 未接线（单测/未配置 Redis），Saturate 跳过投递但计费语义不变。
	feed FeedPublisher
	// log 只用于投递失败的告警；nil 时回退 slog.Default。
	log *slog.Logger
	// queue 是 W10-N2 的批处理生产者接缝。nil = 保持同步逐条写入
	// （`log.batch_enabled` 默认 false）；非 nil = 先 XADD，失败时同步回退。
	queue UsageLogQueue
	// perf 是模型性能采集接缝（platform-05 §3.5）。nil = 未启用/未装配，
	// Saturate 只多一次 nil 判断，不产生任何采样开销。
	perf PerfMetricsRecorder
	// now 是事件 created_at 的时钟接缝；零值使用 time.Now。
	now func() time.Time
}

// NewDBLogger 构造日志落库器。logsDB 为 nil 时 Saturate 返回错误而不是 panic。
func NewDBLogger(logsDB *sql.DB) *DBLogger { return &DBLogger{logsDB: logsDB} }

// WithFeed 挂接 live_feed 事件投递器并返回自身（链式装配）。
//
// 【为什么放在 DBLogger 而不是 Engine】投递必须发生在 usage_logs 事务
// **提交之后**：只有提交成功才代表这次调用已进入公共可展示的账本。把接缝
// 放在写入方（DBLogger）能天然保证顺序，且发布失败只记日志、绝不回滚计费
// ——公开页的一次 Redis 抖动不应让用户被重复扣费或请求失败。
func (l *DBLogger) WithFeed(feed FeedPublisher, logger *slog.Logger) *DBLogger {
	l.feed = feed
	l.log = logger
	return l
}

// WithBatchQueue 挂接 W10-N2 的 Redis Stream 生产者并返回自身（链式装配）。
//
// 【冻结边界 R-W10-02】挂接后 `Saturate` 先尝试 XADD：成功即视为日志已受理
// （不再同步写库）；**失败必须回退到同步写入**，绝不因为 Redis 抖动丢日志。
// 回退后的同步路径与未挂接时逐字节一致。
//
// 默认关闭（`log.batch_enabled=false`）：批处理是可选加速面，先保持既有
// 同步语义上线，再按部署开关切换、可随时回退。
func (l *DBLogger) WithBatchQueue(queue UsageLogQueue, logger *slog.Logger) *DBLogger {
	l.queue = queue
	if logger != nil {
		l.log = logger
	}
	return l
}

// WithPerfMetrics 挂接模型性能采集器（G-M6C-13）并返回自身。
//
// 【为什么挂在 DBLogger 而不是中继 handler】Saturate 是每个请求最终结果的
// 唯一落库点，且已经完成状态/模型/延迟/TTFT/token 的归一化。在这里采样
// 与 usage_logs 的口径天然一致，也不会因重试链/流式路径各自采样造成双计。
func (l *DBLogger) WithPerfMetrics(recorder PerfMetricsRecorder) *DBLogger {
	l.perf = recorder
	return l
}

// logger 返回告警用的 logger（零值回退 slog.Default）。
func (l *DBLogger) logger() *slog.Logger {
	if l != nil && l.log != nil {
		return l.log
	}
	return slog.Default()
}

// 公开首页 live_feed 的 Redis 键（modules-09 F-09-02 / A6-#4）。
//
// 【list 与频道同名】list 供轮询降级（LPUSH 写入、LRANGE 读取），Pub/Sub
// 频道供 SSE 扇出；两者用同一个名字让「最近事件」与「实时事件」不会因为
// 拼写不同而分叉。SSE 订阅侧只读频道，轮询侧只读 list。
const (
	// FeedListKey 是 live_feed 最近事件的 Redis list。
	FeedListKey = "newtg:feed:calls"
	// FeedChannel 是 live_feed 实时广播频道（与 list 同名）。
	FeedChannel = FeedListKey
	// FeedListMax 是 list 保留的最近事件数（环形缓冲上界）。
	FeedListMax = 200

	// FeedWindowKeyPrefix 是聚合条分钟桶的键前缀（F-09-02 聚合条）。
	// 每个桶是一对键：`...:<minute>:calls` 与 `...:<minute>:cost`。
	FeedWindowKeyPrefix = "newtg:feed:win:"
	// FeedWindowMinutes 是聚合窗口按分钟桶计的长度。
	// 【冻结值】F-09-02 定义「近 60 秒」；A6 收口冻结为单个当前分钟桶。
	// 投递写入「投递（结算）时刻」所在的分钟桶（不读事件 TS），读取只汇总该桶，因此读数是
	// 0–60s 的近似：窗口实际覆盖 [当前分钟起点, now]，最长 60 秒。跨分钟
	// 的瞬间读数会从整分钟跌到新桶的 0——这是近似语义的固有代价，换来
	// 读取端一次 MGET、无需维护滑动窗口状态。
	FeedWindowMinutes = 1
	// FeedWindowBucketTTL 是分钟桶的保留时长（冻结 10 分钟）。
	// 只读当前桶，仍保留 10 分钟：跨分钟边界后旧桶需要短暂存活（覆盖
	// 时钟偏移、迟到的读取与同一分钟内的重复续期），过期桶永远不会被
	// 汇总，保留时长不改变读数口径。
	FeedWindowBucketTTL = 10 * time.Minute
)

// FeedWindow 是首页聚合条读数（modules-09 F-09-02）。
//
// 【整数积分】CostCredits 取结算事实里的整数积分（SettlementOutcome.Cost），
// 不做货币换算、不暴露任何单条事件费用（裁决 A-1）。逐行事件保持无费用，
// 费用只在这个平台级聚合读数里出现。
type FeedWindow struct {
	Calls       int64 `json:"calls"`
	CostCredits int64 `json:"cost_credits"`
}

// FeedEvent 是投递到公开 live_feed 的单条脱敏事件（F-09-02 数据字段表）。
//
// 【脱敏红线】字段是白名单，绝不包含 user_id / username / email / IP / UA /
// token / channel_id / channel.name / base_url / prompt / response /
// request_id 原文。upstream 只到协议族（openai/anthropic/gemini/unknown），
// id 是 request_id 的不可逆短哈希，客户端只把它当列表 key。
type FeedEvent struct {
	ID        string `json:"id"`
	TS        string `json:"ts"`
	Model     string `json:"model"`
	Upstream  string `json:"upstream"`
	TokensIn  int    `json:"tokens_in"`
	TokensOut int    `json:"tokens_out"`
	LatencyMS int64  `json:"latency_ms"`
	Status    string `json:"status"`
	// CostCredits 是本次调用的整数积分消耗，只用于投递侧维护聚合条
	// （F-09-02 `window.cost_credits`）。`json:"-"` 是脱敏红线的一部分：
	// 逐行事件按裁决 A-1 绝不下发任何费用字段，代价是它也不会进入
	// Pub/Sub 帧——SSE 只传事件，聚合条只由轮询端点返回。
	CostCredits int64 `json:"-"`
}

// FeedWindowMinute 返回时间点所属的分钟桶编号（Unix 分钟）。
func FeedWindowMinute(now time.Time) int64 { return now.Unix() / 60 }

// FeedWindowCallsKey 返回某分钟桶的 calls 键。
func FeedWindowCallsKey(minute int64) string {
	return fmt.Sprintf("%s%d:calls", FeedWindowKeyPrefix, minute)
}

// FeedWindowCostKey 返回某分钟桶的 cost 键。
func FeedWindowCostKey(minute int64) string {
	return fmt.Sprintf("%s%d:cost", FeedWindowKeyPrefix, minute)
}

// ReadFeedWindow 汇总当前分钟桶的 calls/cost（近 60 秒近似，F-09-02）。
//
// 【一次往返】calls/cost 两个键用一条 MGET 读取：聚合条每个轮询请求都要
// 读，逐键 GET 会把公开页的 Redis 往返翻倍。
//
// 【坏桶按 0】桶值只可能由 INCR/INCRBY 产生；解析失败（手工写入/版本升级）
// 时跳过该桶而不是返回错误——聚合条是展示面读数，不应因为一个坏数字让
// 整个公开列表变成 5xx。
func ReadFeedWindow(ctx context.Context, rdb *redis.Client, now time.Time) (FeedWindow, error) {
	if rdb == nil {
		return FeedWindow{}, errors.New("billing: live feed window has no redis client")
	}
	current := FeedWindowMinute(now)
	keys := make([]string, 0, 2*FeedWindowMinutes)
	for i := int64(0); i < FeedWindowMinutes; i++ {
		minute := current - i
		keys = append(keys, FeedWindowCallsKey(minute), FeedWindowCostKey(minute))
	}
	values, err := rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return FeedWindow{}, fmt.Errorf("billing: read live feed window: %w", err)
	}
	var window FeedWindow
	for i, value := range values {
		if value == nil {
			continue
		}
		text, ok := value.(string)
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(text, 10, 64)
		if err != nil || n < 0 {
			continue
		}
		if i%2 == 0 {
			window.Calls += n
		} else {
			window.CostCredits += n
		}
	}
	return window, nil
}

// FeedPublisher 是 live_feed 事件投递接缝（测试可注入失败实现）。
type FeedPublisher interface {
	PublishCall(ctx context.Context, event FeedEvent) error
}

// RedisFeedPublisher 用 LPUSH + LTRIM + PUBLISH 投递事件，并维护聚合条分钟桶。
type RedisFeedPublisher struct {
	rdb *redis.Client
	// now 是聚合条分钟桶的时钟接缝；零值时使用 time.Now。
	// 抽出来是为了单测能固定「哪一分钟」而不在分钟边界上抖动。
	now func() time.Time
}

// NewRedisFeedPublisher 构造 Redis 投递器。rdb 为 nil 时 PublishCall 直接返回
// 错误（由 DBLogger 记录并吞掉），不影响计费。
func NewRedisFeedPublisher(rdb *redis.Client) *RedisFeedPublisher {
	return &RedisFeedPublisher{rdb: rdb, now: time.Now}
}

// PublishCall 原子写入 list（保留最近 FeedListMax 条）、广播到频道，并累加
// 当前分钟桶的聚合读数。
//
// 用 TxPipeline（MULTI/EXEC）让 LPUSH/LTRIM、PUBLISH 与 INCR 不出现中间态：
// 订阅侧看到的事件与 list/聚合桶来自同一次投递。
//
// 【聚合口径（A6 冻结）】calls 对**所有**事件 +1（成功与失败都算一次调用）；
// cost 只对 status=ok 的事件累加 event.CostCredits（整数积分）。失败事件
// 不计费用，逐行事件本身也从不携带费用（裁决 A-1）。
//
// 【TTL】两个桶都设 FeedWindowBucketTTL。EXPIRE（秒）每次都续期是安全的：
// 同一分钟桶只会被该分钟的事件写入，重复续期不会延长到超过「最后一次
// 写入 + 10 分钟」；读取端只汇总当前分钟桶，不会读到过期桶的脏值。
func (p *RedisFeedPublisher) PublishCall(ctx context.Context, event FeedEvent) error {
	if p == nil || p.rdb == nil {
		return errors.New("billing: live feed publisher has no redis client")
	}
	payload, err := json.Marshal(event)
	if err != nil {
		// 固定结构体的 Marshal 不会失败；保底避免把 nil 发出去。
		return fmt.Errorf("billing: marshal live feed event: %w", err)
	}
	now := time.Now
	if p.now != nil {
		now = p.now
	}
	// 只取一次分钟：两次调用 now() 可能跨分钟，让 calls 与 cost 落到不同桶。
	minute := FeedWindowMinute(now())
	callsKey := FeedWindowCallsKey(minute)
	pipe := p.rdb.TxPipeline()
	pipe.LPush(ctx, FeedListKey, payload)
	pipe.LTrim(ctx, FeedListKey, 0, FeedListMax-1)
	pipe.Publish(ctx, FeedChannel, payload)
	pipe.Incr(ctx, callsKey)
	pipe.Expire(ctx, callsKey, FeedWindowBucketTTL)
	if event.Status == "ok" && event.CostCredits > 0 {
		costKey := FeedWindowCostKey(minute)
		pipe.IncrBy(ctx, costKey, event.CostCredits)
		pipe.Expire(ctx, costKey, FeedWindowBucketTTL)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("billing: publish live feed event: %w", err)
	}
	return nil
}

// 编译期断言：DBLogger 必须实现结算日志接缝。
var _ Saturator = (*DBLogger)(nil)

// usageLogReservedPublicKeys 是 public 区禁止出现的键（billing-07 §7.1）。
//
// 三个分区名本身 + 四个遗留敏感键。本文件只写自己构造的键，这里仍做一次
// 防御性拦截：写入方一旦被复用/扩展，也不会把特权或渠道敏感信息平铺到
// 普通用户可见的 public 区。
var usageLogReservedPublicKeys = map[string]struct{}{
	"admin_info":    {},
	"root_info":     {},
	"audit_info":    {},
	"channel_id":    {},
	"channel_name":  {},
	"channel_type":  {},
	"reject_reason": {},
}

// usageLogRow 是一条 usage_logs 行的写投影（id/created_at 由数据库生成）。
//
// token_name/upstream 在 M3 没有可靠数据源：RelayContext 不携带令牌名与
// 上游显示名。两列可空，因此显式写 NULL 而不是编造占位值（platform-01 §12）。
// retry_chain 由执行层的 Attempts 投影而来（buildRetryChain）；没有尝试
// （未到达上游）时为 NULL。
type usageLogRow struct {
	requestID        string
	userID           int64
	tokenID          sql.NullInt64
	channelID        sql.NullInt64
	model            string
	group            sql.NullString
	promptTokens     int
	completionTokens int
	cacheReadTokens  int
	cacheWriteTokens int
	preDeducted      int64
	cost             int64
	refunded         int64
	latencyMS        sql.NullInt64
	firstTokenMS     sql.NullInt64
	stream           bool
	status           int16
	errorClass       sql.NullString
	errorMessage     sql.NullString
	retryChain       sql.NullString
	other            string
}

// Saturate 写入一条 usage_logs 记录（platform-01 §12 的 25 列）。
//
// 【顺序】Terminate 固定「先 Saturate、后主库结算」（有 hold 路径）；本方法
// 因此必须幂等、可重试。无 hold 的补扣路径反过来「先扣款、后写日志」，
// 失败时钱已收、日志缺失，属已登记的残余风险（R-38）。
//
// 【幂等与跨库竞态 R-25】usage_logs 的 request_id 只有普通索引（R5），
// 没有唯一约束，重复 INSERT 不会被数据库拦住。写终态前先在事务内加
// request_id 作用域的**事务级咨询锁**并读取已有行：
//   - 已有终态行且本次不是 refunded → 直接跳过（幂等命中；绝不把 refunded
//     覆盖成 success）；
//   - 本次是 refunded（worker 兜底退款）→ 允许把已有行**单向收敛**为
//     refunded、cost=0（billing-06 §5.3 第 6 步），refunded 金额只增不减；
//   - 没有任何行 → 插入。
//
// 咨询锁随事务提交/回滚自动释放；所有写 usage_logs 的执行者（Terminate 与
// T5 worker 的退款占位）都必须经本方法写，才能共享同一把锁。
func (l *DBLogger) Saturate(ctx context.Context, fact *contract.UsageFact,
	rc *contract.RelayContext, outcome SettlementOutcome) error {

	if l == nil || l.logsDB == nil {
		return errors.New("billing: DBLogger has no logs database")
	}
	row, err := buildUsageLogRow(fact, rc, outcome)
	if err != nil {
		return err
	}
	// W10-N2：批处理模式先入 Stream。XADD 成功 = 已受理，直接返回，消费侧
	// 负责落库与 live_feed；失败则**同步回退**下面的既有写入路径。
	//
	// 两种模式对调用方的语义相同：返回 nil 表示「日志已受理」。有 hold 的
	// 结算路径仍然是「先日志、后结算」，无 hold 的补扣路径仍然是「先资金、
	// 后日志」——批处理没有改变结算金额或顺序。
	if l.queue != nil && l.enqueueUsageLog(ctx, row, fact != nil, rc) == nil {
		// 批处理路径在 Saturate 返回前已受理；消费者只落库、不再回调
		// Saturate，因此这里采样恰好一次。worker 的退款占位 fact=nil 不采样。
		if fact != nil {
			l.observePerf(row)
		}
		return nil
	}
	inserted, err := l.writeUsageLog(ctx, row)
	if err != nil {
		return fmt.Errorf("billing: saturate usage log for request %s: %w", row.requestID, err)
	}
	// 只有**真正插入新行**的调用才采样/进入 live_feed：幂等命中（重试/重放）
	// 与 worker 的退款收敛都不代表一条新调用。fact == nil 是 worker 的退款
	// 占位，没有可采样的用量事实。
	if inserted && fact != nil {
		l.observePerf(row)
		if l.feed != nil {
			l.publishFeed(row, rc, outcome)
		}
	}
	return nil
}

// observePerf 把已归一化的 usage_logs 行投影为一次性能采样。
//
// 口径（platform-05 §3.5.1）：
//   - 只采完成时三态 success/error/refused；refunded 是结算后的异步事实，
//     进入采样会迫使管道回填，因此直接跳过；
//   - generation 秒数由 latency/TTFT 派生，不额外采集字段；
//   - 非流式请求不携带 TTFT（FirstTokenMS 保持 nil）。
func (l *DBLogger) observePerf(row usageLogRow) {
	if l == nil || l.perf == nil {
		return
	}
	var status perfmetrics.Status
	switch row.status {
	case UsageLogStatusSuccess:
		status = perfmetrics.StatusSuccess
	case UsageLogStatusError:
		status = perfmetrics.StatusError
	case UsageLogStatusRefused:
		status = perfmetrics.StatusRefused
	default:
		// refunded/未知状态不进入采样。
		return
	}
	now := time.Now
	if l.now != nil {
		now = l.now
	}
	observation := perfmetrics.Observation{
		Model:            row.model,
		Status:           status,
		Stream:           row.stream,
		CompletionTokens: int64(row.completionTokens),
		ObservedAt:       now().UTC(),
	}
	if row.latencyMS.Valid {
		latency := int(row.latencyMS.Int64)
		observation.LatencyMS = &latency
	}
	if row.firstTokenMS.Valid {
		firstToken := int(row.firstTokenMS.Int64)
		observation.FirstTokenMS = &firstToken
	}
	l.perf.Observe(observation)
}

// enqueueUsageLog 把一行日志事实投递到 Stream。返回错误时调用方必须同步回退。
//
// 独立短超时：结算路径不应被 Redis 的 3s 默认读超时拖住。超时即按失败处理
// （回退方向安全：日志仍然落库）。若超时但其实 XADD 已生效，消费侧与同步
// 写者会通过同一把 request_id 顾问锁 + 行存在性检查收敛，不会写出两行。
func (l *DBLogger) enqueueUsageLog(ctx context.Context, row usageLogRow,
	feedEligible bool, rc *contract.RelayContext) error {

	now := l.now
	if now == nil {
		now = time.Now
	}
	upstream := "unknown"
	if rc != nil {
		upstream = feedUpstreamFamily(rc.ChannelType)
	}
	// C1：事件只携带规范化后的 request_id。无法规范化的 ID 不入队——
	// 回退同步路径后 `$1::uuid` 会以与既有行为一致的方式失败（不结算），
	// 而不是把一条永远无法落库的消息塞进 Stream。
	requestID, err := normalizeUsageLogRequestID(row.requestID)
	if err != nil {
		l.logger().Warn("billing: usage log request_id is not a valid UUID; not enqueueing",
			"request_id", row.requestID, "error", err)
		return err
	}
	row.requestID = requestID
	event := eventFromUsageLogRow(row, now(), feedEligible, upstream)

	enqueueCtx, cancel := context.WithTimeout(ctx, usageLogEnqueueTimeout)
	defer cancel()
	if err := l.queue.Enqueue(enqueueCtx, event); err != nil {
		// 只记录并回退：XADD 失败不是请求失败，也不是结算失败。
		l.logger().Warn("billing: usage log stream enqueue failed; falling back to synchronous write",
			"request_id", row.requestID, "status", row.status, "error", err)
		return err
	}
	return nil
}

// publishFeed 以独立短超时投递脱敏事件。
//
// 【为什么不继承请求 ctx】Saturate 在请求收尾阶段调用，客户端断连会取消
// 请求上下文；事件仍应尽力投递（否则高并发下的断连会让公开页「断流」）。
// 用独立 Background + 2s 上限，既不被请求取消，也不会在 Redis 故障时挂住
// 结算协程。
func (l *DBLogger) publishFeed(row usageLogRow, rc *contract.RelayContext, outcome SettlementOutcome) {
	event := buildFeedEvent(row, rc, outcome, time.Now().UTC())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := l.feed.PublishCall(ctx, event); err != nil {
		logger := l.log
		if logger == nil {
			logger = slog.Default()
		}
		logger.Warn("billing: publish live feed event failed; billing unaffected",
			"request_id", row.requestID, "error", err)
	}
}

// buildFeedEvent 把已落库的行投影为公开事件（F-09-02 字段表）。
//
// 所有字段都来自 row（已清洗/截断/钳制的日志投影）而不是原始 fact/rc：
// 这样公开面与 usage_logs 的口径天然一致，也避免把渠道名/上游名/用户标识
// 带到响应里。token 数只取 prompt+completion，与排行榜口径相同。
func buildFeedEvent(row usageLogRow, rc *contract.RelayContext, outcome SettlementOutcome,
	now time.Time) FeedEvent {

	status := "error"
	if outcome.Status == UsageLogStatusSuccess {
		status = "ok"
	}
	latency := int64(0)
	if row.latencyMS.Valid {
		latency = row.latencyMS.Int64
	}
	upstream := "unknown"
	if rc != nil {
		upstream = feedUpstreamFamily(rc.ChannelType)
	}
	// 只有成功事件计入聚合条费用；refused/error/refunded 的 cost 在日志
	// 语义里已被强制为 0（或属于退回金额），不能计入「近期消耗」。
	costCredits := int64(0)
	if outcome.Status == UsageLogStatusSuccess {
		costCredits = outcome.Cost
	}
	return FeedEvent{
		ID:          feedEventID(row.requestID),
		TS:          now.Format(time.RFC3339),
		Model:       row.model,
		Upstream:    upstream,
		TokensIn:    row.promptTokens,
		TokensOut:   row.completionTokens,
		LatencyMS:   latency,
		Status:      status,
		CostCredits: costCredits,
	}
}

// feedEventID 返回 request_id 的 8 位十六进制短哈希。
//
// 不直接下发 request_id：它是跨系统关联键，公开页只需要一个稳定的列表 key。
// 哈希不可逆且定长，避免把内部标识原样暴露。
func feedEventID(requestID string) string {
	sum := sha256.Sum256([]byte(requestID))
	return hex.EncodeToString(sum[:4])
}

// feedUpstreamFamily 把渠道类型映射为公开的协议族。
//
// 口径与 relay/usage 的 channelFamily 一致（anthropic/moonshot 为
// Anthropic 原生，gemini/vertex_ai 为 Gemini 原生，其余按 OpenAI 兼容处理）；
// 没有渠道来源（空 slug）时返回 unknown，不用「openai」掩盖未知。
func feedUpstreamFamily(kind contract.ChannelType) string {
	switch kind {
	case contract.ChannelTypeUnknown:
		return "unknown"
	case contract.ChannelTypeAnthropic, contract.ChannelTypeMoonshot:
		return "anthropic"
	case contract.ChannelTypeGemini, contract.ChannelTypeVertexAi:
		return "gemini"
	default:
		return "openai"
	}
}

// buildUsageLogRow 把用量事实/请求上下文/结算结果投影成一行日志。
//
// 校验失败返回 error（调用方据此不结算）：负金额、未知 status、缺 requestID
// 都会破坏日志与账本的对账语义，不能静默落库。
func buildUsageLogRow(fact *contract.UsageFact,
	rc *contract.RelayContext, outcome SettlementOutcome) (usageLogRow, error) {

	if rc == nil || strings.TrimSpace(rc.RequestID) == "" {
		return usageLogRow{}, fmt.Errorf("%w: usage log requires a request id", ErrMissingRequestID)
	}
	if outcome.Status < UsageLogStatusRefused || outcome.Status > UsageLogStatusRefunded {
		return usageLogRow{}, fmt.Errorf("billing: invalid usage log status %d", outcome.Status)
	}
	if outcome.Cost < 0 || outcome.PreDeducted < 0 || outcome.Refunded < 0 ||
		outcome.RetainedCost < 0 {
		return usageLogRow{}, fmt.Errorf(
			"billing: negative usage log amounts (cost=%d pre_deducted=%d refunded=%d retained=%d)",
			outcome.Cost, outcome.PreDeducted, outcome.Refunded, outcome.RetainedCost)
	}

	// cost 语义（platform-01 §12 + R-39/R-59）：
	//   - success  → Cost（实际应扣）；
	//   - refunded → RetainedCost：崩溃兜底只退剩余 hold，已交付用量经
	//     ChargeIncrement 扣掉的增量不随崩溃退回（R-39），日志必须与钱包
	//     一致（cost=保留的消耗，refunded=退回的 hold）；0 = 纯全额退款。
	//     注意 billing-06 §5.3 写的是「refunded → cost=0」，那是未考虑
	//     ChargeIncrement 的字面表述，以 R-39/R-59 为准（已登记文档冲突）。
	//   - refused / error → 强制 0（没有保留消费）。
	cost := outcome.Cost
	switch outcome.Status {
	case UsageLogStatusSuccess:
		// 保留 outcome.Cost。
	case UsageLogStatusRefunded:
		cost = outcome.RetainedCost
	default:
		cost = 0
	}

	// 流式标志优先级：UsageFact.Stream 是结算事实的权威值；只有 worker 的
	// 退款占位（fact==nil，没有 usage 事实）才回退到 rc.Streaming。
	stream := rc.Streaming
	latency := sql.NullInt64{}
	firstToken := sql.NullInt64{}
	if fact != nil {
		stream = fact.Stream
		// PG 的 int 是 32 位；用量事实来自上游/适配器，直接写入越界值会让
		// 整条日志插入失败。与 token 列同一口径钳到 int32（负数视为未知）。
		if fact.LatencyMS >= 0 {
			latency = sql.NullInt64{Int64: int64(clampTokenCount(int64(fact.LatencyMS))), Valid: true}
		}
		// 非流式按契约不得有 TTFT（platform-01 §12：非流式为 null）。
		if stream && fact.FirstTokenMS != nil && *fact.FirstTokenMS >= 0 {
			firstToken = sql.NullInt64{Int64: int64(clampTokenCount(int64(*fact.FirstTokenMS))), Valid: true}
		}
	}

	model := ""
	if fact != nil {
		model = fact.Model
	}
	if model == "" {
		model = rc.OriginModel
	}
	if model == "" {
		model = rc.UpstreamModel
	}
	// varchar(128)/varchar(32)：超长模型名/分组名若原样写入会让整条日志
	// 插入失败，从而阻塞结算。日志列是展示/统计用途，截断是安全方向。
	model = truncateLogText(model, 128)

	group := rc.UsingGroup
	if group == "" {
		group = rc.UserGroup
	}

	// retry_chain 是排障证据：逐轮上游尝试的投影。空 Attempts 必须写 NULL
	// （nullString 把空串视为未设置），不能写空数组冒充「查过但没有记录」。
	retryChain, err := buildRetryChain(rc)
	if err != nil {
		return usageLogRow{}, err
	}

	row := usageLogRow{
		requestID:        strings.TrimSpace(rc.RequestID),
		userID:           int64(rc.UserID),
		tokenID:          nullInt64(int64(rc.TokenID)),
		channelID:        nullInt64(int64(rc.ChannelID)),
		model:            model,
		group:            nullString(truncateLogText(group, 32)),
		promptTokens:     clampTokenCount(int64(usageField(fact, func(u *contract.Usage) int64 { return u.Prompt }))),
		completionTokens: clampTokenCount(int64(usageField(fact, func(u *contract.Usage) int64 { return u.Completion }))),
		cacheReadTokens:  clampTokenCount(int64(usageField(fact, func(u *contract.Usage) int64 { return u.CacheRead }))),
		cacheWriteTokens: clampTokenCount(int64(usageField(fact, func(u *contract.Usage) int64 { return u.CacheWrite }))),
		preDeducted:      outcome.PreDeducted,
		cost:             cost,
		refunded:         outcome.Refunded,
		latencyMS:        latency,
		firstTokenMS:     firstToken,
		stream:           stream,
		status:           outcome.Status,
		errorClass:       nullString(truncateLogText(outcome.ErrorClass, 255)),
		errorMessage:     nullString(truncateLogText(outcome.ErrorMessage, 500)),
		retryChain:       nullString(retryChain),
	}

	other, err := json.Marshal(buildUsageLogOther(rc, model, firstToken, outcome))
	if err != nil {
		// other 只由本文件构造（字符串/数字/嵌套 map），Marshal 失败只可能
		// 来自 QuotaClamp.Original 之类的非有限值；ClampAuditMap 已做 JSON
		// 安全化，此分支是最后一道防线，不把半成品日志写进库。
		return usageLogRow{}, fmt.Errorf("billing: marshal usage log other: %w", err)
	}
	row.other = string(other)
	return row, nil
}

// retryChainStep 是 retry_chain 数组元素的固定形状（键名即落库契约）。
//
// 只承载执行层真实持有的事实：attempt 序号、渠道 id、上游族、错误归类、
// 耗时与上游状态码。channel_name / upstream_request_id 没有可靠来源，不写
// （modules-05 §1.3 的元素级投影允许缺键）。
//
// `decision`（受控 6 值路由决策）同样不写：AttemptRecord 目前不带路由层的
// PolicyDecision，从序号推断「同优先级/降优先级」会把网关从未观测到的事实
// 写进审计证据。等 AttemptRecord 携带真实决策后再加回该键（用户可见标签）。
type retryChainStep struct {
	Attempt        string `json:"attempt"`
	ChannelID      int    `json:"channel_id"`
	Upstream       string `json:"upstream"`
	ErrorClass     string `json:"error_class"`
	LatencyMS      int    `json:"latency_ms"`
	UpstreamStatus int    `json:"upstream_status"`
}

// buildRetryChain 把执行层证据投影为 usage_logs.retry_chain 的 JSON 文本。
//
// 空/nil Attempts 返回 ""（调用方写 NULL）：没有到达上游的请求不应伪造一条
// 空链路，否则读取端无法区分「没有尝试」与「尝试记录丢失」。
//
// upstream 取请求级 rc.ChannelType（结算时最终解析出的渠道族），与
// admin_info.triage.channel_type 同源；重试可能换渠道类型，但 AttemptRecord
// 的 ChannelType 与请求级字段在结算时通常一致，按契约只写请求级标签。
func buildRetryChain(rc *contract.RelayContext) (string, error) {
	if rc == nil || len(rc.Attempts) == 0 {
		return "", nil
	}
	// 上界与生产端（relay.RecordAttempt）共用 contract.MaxAttemptRecords：
	// 日志投影不依赖上游包的实现细节，过量输入直接挡在 jsonb 写入之前
	// （jsonb 体积与读取端渲染成本都被这一处钳住）。
	attempts := rc.Attempts
	if len(attempts) > contract.MaxAttemptRecords {
		attempts = attempts[:contract.MaxAttemptRecords]
	}
	upstream := string(rc.ChannelType)
	steps := make([]retryChainStep, len(attempts))
	for i, attempt := range attempts {
		steps[i] = retryChainStep{
			Attempt:        fmt.Sprintf("%d/%d", i+1, len(attempts)),
			ChannelID:      attempt.ChannelID,
			Upstream:       upstream,
			ErrorClass:     attempt.ErrorClass.String(),
			LatencyMS:      attempt.LatencyMS,
			UpstreamStatus: attempt.UpstreamStatus,
		}
	}
	encoded, err := json.Marshal(steps)
	if err != nil {
		// 固定结构体的 Marshal 不会失败（无 NaN/chan/func）；保底不写半成品。
		return "", fmt.Errorf("billing: marshal usage log retry chain: %w", err)
	}
	return string(encoded), nil
}

// usageField 安全读取 Usage 的某个 int64 字段（fact 为 nil 时返回 0）。
func usageField(fact *contract.UsageFact, pick func(*contract.Usage) int64) int64 {
	if fact == nil {
		return 0
	}
	return pick(&fact.Usage)
}

// buildUsageLogOther 构造 usage_logs.other 的四分区投影（billing-07 §6/§7）。
//
// 本任务只写有可靠来源的键，不臆造字段：
//   - public 顶层平铺：request_path / frt / upstream_model_name /
//     is_model_mapped / applied_discount / billing_source /
//     usage_semantic / billing_mode / expr_b64；
//   - 订阅来源成功日志额外写 billing_preference 与 subscription_* 快照
//     （billing-08 §5.3）：所有值来自结算结果，不做二次查询；
//   - admin_info：quota_saturation（4 键 op/kind/original/clamped）与
//     triage（排障证据：endpoint / channel_type / upstream_status / attempts /
//     request_structure / conversion）。两者都只进 admin_info，普通用户视图
//     会整体剥离该分区（billing-07 §7.2）。
//
// **禁止 estimated_* 键**（billing-00 §11、billing-07 §6.9）：估算值在结果
// 缺失时复用同名键，不引入新前缀。本函数不写任何 estimated_ 前缀键。
//
// 【已知契约缺口】contract.RelayContext 只有 Discount float64，没有
// DiscountTarget 字段（billing-99 R-5）。因此日志只能记录 0<rate<1 的
// applied_discount；rate=0 的合法全免与「未命中」在上下文里无法区分，
// discount_target 也无处取值。该缺口登记在 task-6-report，待契约补字段后
// 由本函数补写（billing-04 §2.6 要求的两个 public 键）。
func buildUsageLogOther(rc *contract.RelayContext,
	model string, firstToken sql.NullInt64, outcome SettlementOutcome) map[string]any {

	other := make(map[string]any, 14)

	if rc.RequestPath != "" {
		setPublicKey(other, "request_path", rc.RequestPath)
	}
	if firstToken.Valid {
		setPublicKey(other, "frt", firstToken.Int64)
	}
	// 上游模型与请求模型不同才写映射标记（billing-07 §6.5）。
	if rc.UpstreamModel != "" && rc.UpstreamModel != model {
		setPublicKey(other, "upstream_model_name", rc.UpstreamModel)
		setPublicKey(other, "is_model_mapped", true)
	}
	// 资金来源来自调用方冻结的选择（S4a 起订阅失败退款会传 subscription）；
	// 不能从本函数的上下文猜测，否则同一请求的退款日志会误记为钱包。
	billingSource := "wallet"
	if outcome.FundingSource == contract.FundingSubscription {
		billingSource = "subscription"
	}
	setPublicKey(other, "billing_source", billingSource)

	if outcome.FundingSource == contract.FundingSubscription {
		// 订阅来源成功/退款日志的专用键（billing-08 §5.3）。零值不写：
		// 空 fact 的按预扣额结算没有 plan 快照时才可能缺失，宁缺勿造。
		if outcome.BillingPreference != "" {
			setPublicKey(other, "billing_preference", outcome.BillingPreference)
		}
		if outcome.SubscriptionID > 0 {
			setPublicKey(other, "subscription_id", outcome.SubscriptionID)
		}
		if outcome.SubscriptionPlanID > 0 {
			setPublicKey(other, "subscription_plan_id", outcome.SubscriptionPlanID)
		}
		if outcome.SubscriptionPlanTitle != "" {
			setPublicKey(other, "subscription_plan_title", outcome.SubscriptionPlanTitle)
		}
		if outcome.SubscriptionTotal > 0 {
			setPublicKey(other, "subscription_total", outcome.SubscriptionTotal)
			setPublicKey(other, "subscription_used", outcome.SubscriptionUsed)
			remain := outcome.SubscriptionTotal - outcome.SubscriptionUsed
			if remain < 0 {
				remain = 0
			}
			setPublicKey(other, "subscription_remain", remain)
		}
		if outcome.SubscriptionPreConsumed > 0 {
			setPublicKey(other, "subscription_pre_consumed", outcome.SubscriptionPreConsumed)
		}
		if outcome.SubscriptionPostDelta != 0 {
			setPublicKey(other, "subscription_post_delta", outcome.SubscriptionPostDelta)
		}
		// 「本次请求订阅消耗」只在成功结算时有意义；退款/失败日志不写消费。
		if outcome.Status == UsageLogStatusSuccess && outcome.Cost > 0 {
			setPublicKey(other, "subscription_consumed", outcome.Cost)
		}
		// 订阅计费不扣钱包（billing-07 §6.4：固定 0），显式写出让对账方
		// 不必依赖「键缺失 = 0」的隐含约定。
		setPublicKey(other, "wallet_quota_deducted", int64(0))
	}

	// 折扣日志：0<rate<1 才可判定为「命中且未全免」；rate=0 与零值无法
	// 区分（见上方契约缺口注释），不在本函数猜测。
	if rc.Discount > 0 && rc.Discount < 1 && !math.IsNaN(rc.Discount) {
		setPublicKey(other, "applied_discount", rc.Discount)
	}

	if snap := rc.PriceSnapshot; snap != nil {
		if snap.Mode == contract.BillingModeTieredExpr && snap.ExprString != "" {
			setPublicKey(other, "billing_mode", "tiered_expr")
			setPublicKey(other, "expr_b64", base64.StdEncoding.EncodeToString([]byte(snap.ExprString)))
		}
		if snap.IsClaudeUsageSemantic {
			setPublicKey(other, "usage_semantic", "anthropic")
		}
		// 时段计价的可审计事实（spec §7）：TimeMultiplier=0 表示该模型没有
		// 启用时段策略，键不写，日志与旧版逐字段一致。
		if snap.TimeMultiplier > 0 {
			setPublicKey(other, "time_multiplier", snap.TimeMultiplier)
			if snap.TimeWindow != "" {
				setPublicKey(other, "time_window", snap.TimeWindow)
			}
			if snap.HolidayName != "" {
				setPublicKey(other, "holiday_name", snap.HolidayName)
			}
		}
	}

	adminInfo := make(map[string]any, 2)
	if rc.QuotaClamp != nil {
		// 钳制标记必须只出现在 admin_info：普通用户视图会整体剥离该分区
		// （billing-07 §7.2），这使「管理员可见、非管理员不可见」成为结构
		// 性质而不是读取侧的额外判断。
		adminInfo["quota_saturation"] = pricing.ClampAuditMap(rc.QuotaClamp)
	}
	adminInfo["triage"] = buildTriageInfo(rc)
	other["admin_info"] = adminInfo

	return other
}

// maxTriageConversionSteps 是 admin_info.triage.conversion 保留的步数上界。
//
// 转换链是诊断线索而不是审计流水：排障只需要知道「是否发生转换、哪一段」，
// 4 步足以覆盖"入站 → 中间 IR → 上游"的常见路径，也避免把异常长链写进
// 每次日志（体积与读取端渲染成本）。
const maxTriageConversionSteps = 4

// triageLabelMaxRunes 是 conversion 中 stage/from/to 的字符串截断上界。
//
// 与 truncateLogText 同一口径（按 rune 截断、去 NUL）：这些值最终进 jsonb，
// 未清洗的超长/非法 UTF-8 文本会让整条日志插入失败并阻塞结算。
const triageLabelMaxRunes = 64

// buildTriageInfo 构造 admin_info.triage 的排障证据投影。
//
// 只写有可靠来源的事实，且全部是有界值（枚举、状态码、计数）：
//   - endpoint：contract.EndpointLabel（chat_completions|messages|responses|other）；
//   - channel_type：请求级渠道族（与 retry_chain[].upstream 同源）；
//   - upstream_status：最近一次**拿到的**上游响应状态码。末尾几轮可能是
//     网络错误/超时（AttemptRecord.UpstreamStatus==0），它们不应覆盖之前
//     真正拿到的响应；一次都没有拿到时整键省略（0 不是合法的 HTTP 状态）；
//   - attempts：执行层记录的尝试次数（已由 relay 截到 8 条）；
//   - request_structure：valid|invalid|""（空 = 未校验），空串也写，读取端
//     据此区分「校验通过/不通过/未校验」三态；
//   - conversion：最多 4 步，标签截断 64 rune；无转换时写空数组。
//
// 本函数不写任何自由文本（上游响应体、prompt、渠道名/凭据），符合 GC-6。
func buildTriageInfo(rc *contract.RelayContext) map[string]any {
	if rc == nil {
		rc = &contract.RelayContext{}
	}
	upstreamStatus := 0
	for _, attempt := range rc.Attempts {
		if attempt.UpstreamStatus != 0 {
			upstreamStatus = attempt.UpstreamStatus
		}
	}
	triage := make(map[string]any, 6)
	triage["endpoint"] = contract.EndpointLabel(rc.RequestPath)
	triage["channel_type"] = string(rc.ChannelType)
	if upstreamStatus != 0 {
		triage["upstream_status"] = upstreamStatus
	}
	triage["attempts"] = len(rc.Attempts)
	triage["request_structure"] = truncateLogText(rc.RequestStructure, triageLabelMaxRunes)
	triage["conversion"] = buildTriageConversion(rc.ConversionChain)
	return triage
}

// buildTriageConversion 把转换链投影为 {stage, from, to} 的有界数组。
//
// 连续重复的 (stage, from, to) 只保留一步：执行层每轮 BuildRequest 都会追加
// 请求方向的转换，同一渠道上的重试因此会产生完全相同的相邻条目。去重只针对
// 相邻重复，不同 stage（例如之后的 protocol_convert 响应回写）不会互相吞掉。
func buildTriageConversion(chain []contract.ConversionStepEntry) []map[string]any {
	// 非 nil 空切片：jsonb 写 [] 而不是 null，读取端不必区分两种「没有转换」。
	steps := make([]map[string]any, 0, min(len(chain), maxTriageConversionSteps))
	var prevStage, prevFrom, prevTo string
	for _, entry := range chain {
		stage := truncateLogText(entry.Stage, triageLabelMaxRunes)
		from := truncateLogText(string(entry.From), triageLabelMaxRunes)
		to := truncateLogText(string(entry.To), triageLabelMaxRunes)
		if len(steps) > 0 && stage == prevStage && from == prevFrom && to == prevTo {
			continue
		}
		if len(steps) == maxTriageConversionSteps {
			break
		}
		steps = append(steps, map[string]any{
			"stage": stage,
			"from":  from,
			"to":    to,
		})
		prevStage, prevFrom, prevTo = stage, from, to
	}
	return steps
}

// setPublicKey 写入 public 顶层键，拒绝保留键（billing-07 §7.1）。
func setPublicKey(dst map[string]any, key string, value any) {
	if _, reserved := usageLogReservedPublicKeys[key]; reserved {
		return
	}
	// 字符串值统一清洗：PostgreSQL 的 jsonb 不接受 \u0000（SQLSTATE 22P05），
	// 一个带 `%00` 的请求路径就能让整条日志插入失败并阻塞结算。truncateLogText
	// 同时去掉 NUL 并修复非法 UTF-8；max=0 表示不做长度截断。
	if s, ok := value.(string); ok {
		value = truncateLogText(s, 0)
	}
	dst[key] = value
}

// writeUsageLog 在单个日志库事务内完成「加锁 → 查重 → 插入/收敛」。
// 返回 inserted=true 仅表示本次**新插入**了一行终态日志（幂等命中与退款
// 收敛都返回 false），供 live_feed 判断是否应投递一次新调用。
//
// 为什么必须用事务级咨询锁：request_id 无唯一约束，两个执行者（Terminate
// 与 worker 退款）并发写时，单纯 SELECT-then-INSERT 在 READ COMMITTED 下
// 互相看不到对方未提交的行，会写出两行。锁按 request_id 的哈希串行化
// 同一请求的写者；不同请求之间只有极小概率的哈希碰撞（仅多一次串行）。
func (l *DBLogger) writeUsageLog(ctx context.Context, row usageLogRow) (bool, error) {
	tx, err := l.logsDB.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin usage log tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := lockUsageLogRequestID(ctx, tx, row.requestID); err != nil {
		return false, err
	}

	existing, err := existingUsageLogStatuses(ctx, tx, row.requestID)
	if err != nil {
		return false, err
	}
	if len(existing) > 0 {
		if row.status != UsageLogStatusRefunded {
			// 已有终态 → 幂等命中。尤其不得把 refunded 覆盖成 success
			// （R-25：worker 在 Saturate 与主库结算之间已判定悬挂并退款）。
			return false, tx.Commit()
		}
		// 单向收敛到 refunded：billing-06 §5.3 第 6 步要求「若存在日志行，
		// 置 status=refunded」；cost 取本次结算的保留消耗（R-39/R-59：已交付
		// 用量的 ChargeIncrement 不随 hold 退回，纯退款时为 0）。refunded
		// 金额取调用方新值与既有值的较大者，重复退款不会把已记录金额改小。
		// error_class/error_message 仅在本次提供时覆盖（worker 的退款原因），
		// 保留原有的错误归类信息。
		if _, err := tx.ExecContext(ctx, `
			UPDATE usage_logs
			   SET status = $2,
			       cost = $3,
			       refunded = GREATEST(COALESCE(refunded, 0), $4),
			       error_class = COALESCE(NULLIF($5, ''), error_class),
			       error_message = COALESCE(NULLIF($6, ''), error_message)
			 WHERE request_id = $1::uuid`,
			row.requestID, row.status, row.cost, row.refunded,
			row.errorClass.String, row.errorMessage.String); err != nil {
			return false, fmt.Errorf("converge usage log to refunded: %w", err)
		}
		return false, tx.Commit()
	}

	if _, err := tx.ExecContext(ctx, usageLogInsertSQL, usageLogInsertArgs(row)...); err != nil {
		return false, fmt.Errorf("insert usage log: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// lockUsageLogRequestID 取 request_id 作用域的事务级咨询锁。
//
// 所有写 usage_logs 的执行者（Saturate/writeUsageLog 与 RecordRefusal）都
// 必须经本函数加锁，才能让同一请求的写者共享同一把锁。
//
// 【I1】先 ::uuid 规范化再取文本：UUID 的合法输入拼写不止一种
// （大小写、带花括号 `{...}`），直接对原始文本做哈希会让同一个
// requestID 落到两把不同的锁上，进而并发写出两行日志。PG 的
// uuid→text 输出固定为小写无花括号形式，保证所有拼写共享同一把锁。
func lockUsageLogRequestID(ctx context.Context, tx *sql.Tx, requestID string) error {
	if _, err := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended(($1::uuid)::text, 0))`, requestID); err != nil {
		return fmt.Errorf("lock usage log request id: %w", err)
	}
	return nil
}

// usageLogInsertSQL 显式列出 platform-01 §12 的 25 列。
//
// id 由 IDENTITY 生成，created_at 由数据库 now() 生成；token_name/upstream
// 仍无可靠来源，显式 NULL。列清单保持与迁移逐列同名，便于后续补数据源时
// 按列增补，而不是改成 map/反射写入。
//
// retry_chain 从 NULL 常量改为 $20::jsonb（排障证据），因此 now 的 other
// 顺延为 $21::jsonb；共 21 个绑定参数（token_name/upstream 不占位）。
const usageLogInsertSQL = `
INSERT INTO usage_logs (
    request_id, user_id, token_id, token_name, channel_id, upstream,
    model, "group", prompt_tokens, completion_tokens, cache_read_tokens,
    cache_write_tokens, pre_deducted, cost, refunded, latency_ms,
    first_token_ms, stream, status, error_class, error_message,
    retry_chain, other)
VALUES ($1::uuid, $2, $3, NULL, $4, NULL,
        $5, $6, $7, $8, $9,
        $10, $11, $12, $13, $14,
        $15, $16, $17, $18, $19,
        $20::jsonb, $21::jsonb)`

// usageLogInsertArgs 按 usageLogInsertSQL 的 $1..$21 顺序展开绑定参数。
//
// 抽出来供两条写路径共用：Saturate 的同步写入与 Task 3 的中继前拒绝行
// （RecordRefusal）。参数顺序是跨任务契约，改动必须同步改 SQL 占位符。
func usageLogInsertArgs(row usageLogRow) []any {
	return []any{
		row.requestID, row.userID, row.tokenID, row.channelID, row.model,
		row.group, row.promptTokens, row.completionTokens, row.cacheReadTokens,
		row.cacheWriteTokens, row.preDeducted, row.cost, row.refunded,
		row.latencyMS, row.firstTokenMS, row.stream, row.status,
		row.errorClass, row.errorMessage, row.retryChain, row.other,
	}
}

// existingUsageLogStatuses 读取该 request_id 已存在的日志状态。
//
// FOR UPDATE 锁住已有行，配合事务级咨询锁构成「单行状态机」的写侧屏障。
// 读取后立即关闭 rows，避免 pgx 连接在事务内被结果集占用。
func existingUsageLogStatuses(ctx context.Context, tx *sql.Tx, requestID string) ([]int16, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT status FROM usage_logs WHERE request_id = $1::uuid
		 ORDER BY created_at, id FOR UPDATE`, requestID)
	if err != nil {
		return nil, fmt.Errorf("query existing usage log: %w", err)
	}
	defer rows.Close()

	var statuses []int16
	for rows.Next() {
		var s int16
		if err := rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("scan existing usage log status: %w", err)
		}
		statuses = append(statuses, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate existing usage log rows: %w", err)
	}
	return statuses, nil
}

// nullInt64 把 0 视为 NULL（users/tokens/channels 的 id 由 IDENTITY 从 1 起，
// 零值表示「未绑定」而不是一个合法主键）。
func nullInt64(v int64) sql.NullInt64 {
	if v == 0 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: v, Valid: true}
}

// nullString 把空串视为 NULL（可空文本列，空值不渲染）。
func nullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// clampTokenCount 把上游 token 数钳到 PG int 域并拒绝负值。
//
// 这里不是配额换算（quota 换算只走 pricing），但 PG 的 int 是 32 位：
// 直接把 int64 转 int 在 32 位宿主上会回绕。token 数不可为负，负数按 0
// 处理（上游解析怪癖不污染统计）。
func clampTokenCount(v int64) int {
	if v <= 0 {
		return 0
	}
	if v > math.MaxInt32 {
		return math.MaxInt32
	}
	return int(v)
}

// truncateLogText 清洗并截断日志文本列。
//
// 两件事：
//   - 去掉 NUL：PostgreSQL 的 text/varchar 不接受 \u0000，否则整条日志
//     插入失败并阻塞结算；
//   - 按 **rune** 截断：按字节截断会把多字节 UTF-8 切成非法序列，同样
//     导致被数据库拒绝。max<=0 表示不截断。
func truncateLogText(s string, max int) string {
	if s == "" {
		return ""
	}
	s = strings.ReplaceAll(s, "\x00", "")
	s = strings.ToValidUTF8(s, "�")
	if max <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}
