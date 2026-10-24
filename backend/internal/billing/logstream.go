package billing

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/metrics"
)

// 本文件是 W10-N2 的日志批处理链路（billing-06 §5.3 / R-W10-02、R-W10-11）：
// 生产侧把一条 usage_logs 事实 `XADD` 到 Redis Stream，worker 角色的单消费组
// 批量落库。
//
// 【冻结的 durability 边界（R-W10-02）】
//   - `XADD` 成功 = 日志已受理；生产侧直接返回 nil，不再同步写日志库；
//   - `XADD` 失败 → **同步回退**现有 `DBLogger.writeUsageLog`，绝不丢日志；
//   - 消费者只有在 INSERT/收敛事务**提交之后**才 `XACK`；
//   - 未 ack 的 pending 在 60s 空闲后被 `XAUTOCLAIM` 重投。
//
// 【为什么 created_at 必须由事件携带】usage_logs 按 created_at 月分区。若批量
// 落库继续用数据库 `DEFAULT now()`，日志时间会漂移成冲刷时刻：分区归属、排行
// 榜、latency 报表与保留期全部偏移。事件里的 created_at 是生产侧的结算时刻，
// 消费侧显式写入（见 usageLogBatchInsertSQL）。
//
// 【refunded 单向收敛】退款事件与成功事件同流，异步后可能先于 success 落库。
// 1) 已有行 + refunded → 收敛为 refunded（refunded 金额 GREATEST 只增不减）；
// 2) 已有行 + success → 跳过，**绝不把 refunded 回退成 success**；
// 3) 没有任何行 → 插入。批内先插 success 再收敛 refunded，保留 success 事件
// 里的 token/model 细节（worker 的退款占位 fact=nil，没有这些细节）。

const (
	// UsageLogStreamKey 是 usage_logs 的 Redis Stream 键。所有副本共用。
	UsageLogStreamKey = "newtg:usage_logs"
	// UsageLogStreamGroup 是消费组名。冻结为单消费组（R-W10-11）：多消费组会
	// 让同一条日志被重复消费，去重虽然兜底但会放大写放大。
	UsageLogStreamGroup = "worker-logs"

	// UsageLogStreamMaxLen 是 Stream 的近似长度上限（MAXLEN ~100000）。
	// Approx 修剪按 macro node 整块删除，实际长度可能略超上限；冻结这个量级
	// 是为了给 Redis 内存一个硬边界，而不是精确计数。
	//
	// 【已知丢失边界】修剪不区分「已消费」与「未消费」：若 worker 长时间缺位，
	// 积压超过上限后最旧的事件会被 Redis 丢弃。这是 R-W10-02「XADD 成功即
	// 受理」的已知代价，靠 newtg_worker_usage_logs_stream_lag（阈值 10000，
	// 远低于上限）在真正丢日志之前告警。
	UsageLogStreamMaxLen = 100000

	// UsageLogStreamClaimIdle 是 pending 条目被认定「消费者已失联」的空闲阈值。
	// 消费者崩溃后，存活副本最多等这么久就会 XAUTOCLAIM 重投。
	UsageLogStreamClaimIdle = 60 * time.Second

	// UsageLogStreamLagThreshold 是积压告警阈值（A13 冻结 10000）。超过时消费
	// 循环额外打一条 warn；指标 newtg_worker_usage_logs_stream_lag 本身总是上报。
	UsageLogStreamLagThreshold = 10000

	// UsageLogStreamBatchSize 是单次读取/消费的最大条数（设计中「每 5 秒或
	// 500 条」的批量口径）。
	UsageLogStreamBatchSize = 500
	// UsageLogStreamBlock 是 XREADGROUP 的阻塞时长。凑满 500 会立即返回并继续
	// 下一批，不会固定等满 5 秒，因而突发时有额外余量。
	UsageLogStreamBlock = 5 * time.Second

	// UsageLogDedupKeyPrefix 是应用层幂等键前缀。键里带 status：success 与
	// refunded 是同一请求的两个不同事实，不能互相吞掉。
	UsageLogDedupKeyPrefix = "newtg:log_dedup:"
	// UsageLogDedupTTL 覆盖「消费者崩溃 → 60s 后重投」的最坏路径，同时避免
	// 幂等键无限增长。24h 远大于任何合理的重投窗口。
	UsageLogDedupTTL = 24 * time.Hour

	// usageLogEnqueueTimeout 是单次 XADD 的上限。Redis 故障时 go-redis 的默认
	// ReadTimeout 是 3s，直接挂在结算路径上会拖慢请求；超时就按失败处理并
	// 同步回退（回退方向是安全的：日志一定落库）。
	usageLogEnqueueTimeout = time.Second

	// usageLogStreamProcessTimeout 是单批落库的上限。处理上下文刻意**不**继承
	// 消费循环的取消信号：停机时已经取出的这一批要冲刷完并 XACK，否则会退回
	// pending 重投（不丢，只是多一次写）。
	usageLogStreamProcessTimeout = 30 * time.Second

	// usageLogStreamErrorBackoff 是 Redis/DB 故障后的退避，避免空转放大故障。
	usageLogStreamErrorBackoff = time.Second
)

// UsageLogEvent 是一条 usage_logs 事实在 Stream 上的传输表示。
//
// 字段与 usageLogRow 一一对应，额外携带两个 feed 字段：
//   - FeedEligible 对应生产侧 `fact != nil`：只有真实调用（不是 worker 的
//     退款占位）才进入公开 live_feed；
//   - FeedUpstream 是渠道协议族的脱敏标签，消费侧没有原始 RelayContext，
//     只能由生产侧算好随事件带过来。
type UsageLogEvent struct {
	RequestID        string    `json:"request_id"`
	CreatedAt        time.Time `json:"created_at"`
	UserID           int64     `json:"user_id"`
	TokenID          *int64    `json:"token_id,omitempty"`
	ChannelID        *int64    `json:"channel_id,omitempty"`
	Model            string    `json:"model"`
	Group            *string   `json:"group,omitempty"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	CacheReadTokens  int       `json:"cache_read_tokens"`
	CacheWriteTokens int       `json:"cache_write_tokens"`
	PreDeducted      int64     `json:"pre_deducted"`
	Cost             int64     `json:"cost"`
	Refunded         int64     `json:"refunded"`
	LatencyMS        *int64    `json:"latency_ms,omitempty"`
	FirstTokenMS     *int64    `json:"first_token_ms,omitempty"`
	Stream           bool      `json:"stream"`
	Status           int16     `json:"status"`
	ErrorClass       *string   `json:"error_class,omitempty"`
	ErrorMessage     *string   `json:"error_message,omitempty"`
	// RetryChain 是排障证据的 JSON 文本（与 usageLogRow.retryChain 同一投影）。
	// 新增字段对旧消费者向后兼容：缺席即 NULL，与 W10-N2 上线前的行为一致。
	RetryChain *string `json:"retry_chain,omitempty"`
	Other      string  `json:"other"`

	FeedEligible bool   `json:"feed_eligible,omitempty"`
	FeedUpstream string `json:"feed_upstream,omitempty"`
}

// UsageLogQueue 是生产侧的事件队列接缝（测试可注入失败实现）。
type UsageLogQueue interface {
	Enqueue(ctx context.Context, event UsageLogEvent) error
}

// RedisUsageLogQueue 用 XADD 把事件写入 Stream。
type RedisUsageLogQueue struct {
	rdb *redis.Client
	// maxLen 是 MAXLEN 的测试接缝；零值使用 UsageLogStreamMaxLen。
	// 生产不提供配置面：这是 Stream 契约参数，不是运营旋钮。
	maxLen int64
}

// NewRedisUsageLogQueue 构造 Stream 生产者。rdb 为 nil 时 Enqueue 返回错误，
// 由 DBLogger 回退同步写入而不是 panic。
func NewRedisUsageLogQueue(rdb *redis.Client) *RedisUsageLogQueue {
	return &RedisUsageLogQueue{rdb: rdb}
}

// Enqueue 写入一条事件。返回 nil 即代表「已受理」（R-W10-02）。
func (q *RedisUsageLogQueue) Enqueue(ctx context.Context, event UsageLogEvent) error {
	if q == nil || q.rdb == nil {
		return errors.New("billing: usage log queue has no redis client")
	}
	payload, err := json.Marshal(event)
	if err != nil {
		// 固定结构体的 Marshal 不会失败；保底避免把半成品发出去。
		return fmt.Errorf("billing: marshal usage log event: %w", err)
	}
	return q.rdb.XAdd(ctx, q.addArgs(payload)).Err()
}

// addArgs 构造 XADD 参数。抽成独立函数便于单测断言 MAXLEN 契约，不需要真 Redis。
func (q *RedisUsageLogQueue) addArgs(payload []byte) *redis.XAddArgs {
	maxLen := q.maxLen
	if maxLen <= 0 {
		maxLen = UsageLogStreamMaxLen
	}
	return &redis.XAddArgs{
		Stream: UsageLogStreamKey,
		// Approx=true 生成 `MAXLEN ~ N`：允许 Redis 按 macro node 整块修剪。
		MaxLen: maxLen,
		Approx: true,
		Values: []any{"event", payload},
	}
}

// eventFromUsageLogRow 把已校验的行投影成事件。
//
// eventTime 使用生产侧时钟（结算时刻），不是消费侧的冲刷时刻。
func eventFromUsageLogRow(row usageLogRow, eventTime time.Time,
	feedEligible bool, feedUpstream string) UsageLogEvent {

	return UsageLogEvent{
		RequestID:        row.requestID,
		CreatedAt:        eventTime.UTC(),
		UserID:           row.userID,
		TokenID:          ptrNullInt64(row.tokenID),
		ChannelID:        ptrNullInt64(row.channelID),
		Model:            row.model,
		Group:            ptrNullString(row.group),
		PromptTokens:     row.promptTokens,
		CompletionTokens: row.completionTokens,
		CacheReadTokens:  row.cacheReadTokens,
		CacheWriteTokens: row.cacheWriteTokens,
		PreDeducted:      row.preDeducted,
		Cost:             row.cost,
		Refunded:         row.refunded,
		LatencyMS:        ptrNullInt64(row.latencyMS),
		FirstTokenMS:     ptrNullInt64(row.firstTokenMS),
		Stream:           row.stream,
		Status:           row.status,
		ErrorClass:       ptrNullString(row.errorClass),
		ErrorMessage:     ptrNullString(row.errorMessage),
		RetryChain:       ptrNullString(row.retryChain),
		Other:            row.other,
		FeedEligible:     feedEligible,
		FeedUpstream:     feedUpstream,
	}
}

// usageLogRow 把事件还原成写库投影。created_at 不参与（显式列单独传递）。
func (e UsageLogEvent) usageLogRow() usageLogRow {
	return usageLogRow{
		requestID:        e.RequestID,
		userID:           e.UserID,
		tokenID:          nullInt64FromPtr(e.TokenID),
		channelID:        nullInt64FromPtr(e.ChannelID),
		model:            e.Model,
		group:            nullStringFromPtr(e.Group),
		promptTokens:     e.PromptTokens,
		completionTokens: e.CompletionTokens,
		cacheReadTokens:  e.CacheReadTokens,
		cacheWriteTokens: e.CacheWriteTokens,
		preDeducted:      e.PreDeducted,
		cost:             e.Cost,
		refunded:         e.Refunded,
		latencyMS:        nullInt64FromPtr(e.LatencyMS),
		firstTokenMS:     nullInt64FromPtr(e.FirstTokenMS),
		stream:           e.Stream,
		status:           e.Status,
		errorClass:       nullStringFromPtr(e.ErrorClass),
		errorMessage:     nullStringFromPtr(e.ErrorMessage),
		retryChain:       nullStringFromPtr(e.RetryChain),
		other:            e.Other,
	}
}

// errInvalidUsageLogRequestID 表示事件里的 request_id 不是可接受的 UUID 形态。//
// 【C1】生产侧的 request_id 可能是 32 位无横线 hex，而 PG 的
// `request_id::text` 输出是带横线的小写标准形态。若各处理环节用不同形态做
// map 键，跨批去重与退款收敛会全部失配（success 后再来 refund 会插第二行）。
// 消费边界必须先把 request_id 规范化成唯一形态，所有 map/SQL 只用这个形态。
var errInvalidUsageLogRequestID = errors.New("billing: usage log request_id is not a valid UUID")

// errUsageLogStreamAck 区分「INSERT 失败」与「INSERT 已提交但 ack 失败」。
// 两者的重投语义相同（不丢日志），但运维告警文案必须不同：后者不是写库故障。
var errUsageLogStreamAck = errors.New("billing: acknowledge usage log stream entries")

// normalizeUsageLogRequestID 把 UUID 的各种合法输入形态规范化为
// PG `uuid::text` 的标准形态：小写、带横线、无花括号。
//
// 接受：32 位 hex（生产 ID 的实际形态）、标准 36 位形态、大写、`{...}`。
// 拒绝其它一切输入（消费侧按毒消息处理，生产侧回退同步写入以保持
// 「无法落库就不结算」的既有语义）。
func normalizeUsageLogRequestID(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if len(s) >= 2 && s[0] == '{' && s[len(s)-1] == '}' {
		s = s[1 : len(s)-1]
	}
	s = strings.ReplaceAll(s, "-", "")
	if len(s) != 32 {
		return "", errInvalidUsageLogRequestID
	}
	rawBytes, err := hex.DecodeString(s)
	if err != nil {
		return "", errInvalidUsageLogRequestID
	}
	s = strings.ToLower(hex.EncodeToString(rawBytes))
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		s[0:8], s[8:12], s[12:16], s[16:20], s[20:32]), nil
}

// dedupKey 返回一条非退款事件的幂等键。
//
// **refunded 不参与键空间**：退款金额可能被后续更大的退款更新（GREATEST），
// 用键吞掉第二条退款会丢掉金额收敛。退款更新本身是幂等的，重复执行安全。
func dedupKey(requestID string, status int16) string {
	return fmt.Sprintf("%s%s:%d", UsageLogDedupKeyPrefix, strings.TrimSpace(requestID), status)
}

// usageLogBatchEntry 是批内按 request_id 归并后的一个写意图。
//
// insert 是「若库里没有行则插入」的事件（优先非退款事件）；refund 是「若库里
// 已有行则单向收敛」的退款事件。两者可以同时存在（同批既有 success 又有
// refunded），此时先插 success 再收敛，保留用量细节。
type usageLogBatchEntry struct {
	requestID string
	insert    *UsageLogEvent
	refund    *UsageLogEvent
}

// reduceUsageLogBatch 做批内去重与归并。
//
// 顺序语义：
//   - 非退款事件：同一 request_id 只保留**第一条**（请求只有一个终态；
//     重复通常是重投或 ambiguous XADD 后的双发）；
//   - 退款事件：保留 refunded 金额最大的一条（金额只增不减），金额相同取更晚的；
//   - 结果按 request_id 排序，让顾问锁获取顺序确定，避免多消费副本死锁。
func reduceUsageLogBatch(events []UsageLogEvent) []usageLogBatchEntry {
	byID := make(map[string]*usageLogBatchEntry, len(events))
	order := make([]string, 0, len(events))
	for i := range events {
		ev := events[i]
		rid := strings.TrimSpace(ev.RequestID)
		if rid == "" {
			continue
		}
		ev.RequestID = rid
		entry, ok := byID[rid]
		if !ok {
			entry = &usageLogBatchEntry{requestID: rid}
			byID[rid] = entry
			order = append(order, rid)
		}
		if ev.Status == UsageLogStatusRefunded {
			if entry.refund == nil ||
				ev.Refunded > entry.refund.Refunded ||
				(ev.Refunded == entry.refund.Refunded && ev.CreatedAt.After(entry.refund.CreatedAt)) {
				copied := ev
				entry.refund = &copied
			}
			continue
		}
		if entry.insert == nil {
			copied := ev
			entry.insert = &copied
		}
	}
	// 没有 insert 也没有 refund 的条目不会出现（每条事件至少落入一侧）。
	sort.Strings(order)
	out := make([]usageLogBatchEntry, 0, len(order))
	for _, rid := range order {
		out = append(out, *byID[rid])
	}
	return out
}

// usageLogConsumer 是 Stream 消费组的执行体。
type usageLogConsumer struct {
	redis     *redis.Client
	logsDB    *sql.DB
	log       *slog.Logger
	now       func() time.Time
	consumer  string
	feed      FeedPublisher
	claimIdle time.Duration
	batchSize int64
	block     time.Duration
	dedupTTL  time.Duration
	// lagThreshold 是积压告警阈值（测试接缝；零值使用冻结常量）。
	lagThreshold int64
	// processTimeout 是单批处理上限（零值使用冻结常量）。
	processTimeout time.Duration
}

// UsageLogStreamDeps 是消费循环的依赖与测试接缝。零值使用冻结默认：
//   - Now → time.Now；Logger → slog.Default()；
//   - ClaimIdle → UsageLogStreamClaimIdle；BatchSize → UsageLogStreamBatchSize；
//   - Block → UsageLogStreamBlock；DedupTTL → UsageLogDedupTTL。
//
// Consumer 为空时用 "worker"（单消费组内仍允许重名，Redis 只按名字记 pending；
// 生产装配传入 node-instance 便于排障）。
type UsageLogStreamDeps struct {
	Redis     *redis.Client
	LogsDB    *sql.DB
	Logger    *slog.Logger
	Now       func() time.Time
	Consumer  string
	Feed      FeedPublisher
	ClaimIdle time.Duration
	BatchSize int64
	Block     time.Duration
	DedupTTL  time.Duration
}

func newUsageLogConsumer(deps UsageLogStreamDeps) (*usageLogConsumer, error) {
	if deps.Redis == nil {
		return nil, errors.New("billing: usage log stream requires Redis")
	}
	if deps.LogsDB == nil {
		return nil, errors.New("billing: usage log stream requires the logs database")
	}
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	consumer := strings.TrimSpace(deps.Consumer)
	if consumer == "" {
		consumer = "worker"
	}
	claimIdle := deps.ClaimIdle
	if claimIdle <= 0 {
		claimIdle = UsageLogStreamClaimIdle
	}
	batchSize := deps.BatchSize
	if batchSize <= 0 {
		batchSize = UsageLogStreamBatchSize
	}
	block := deps.Block
	if block <= 0 {
		block = UsageLogStreamBlock
	}
	dedupTTL := deps.DedupTTL
	if dedupTTL <= 0 {
		dedupTTL = UsageLogDedupTTL
	}
	return &usageLogConsumer{
		redis:          deps.Redis,
		logsDB:         deps.LogsDB,
		log:            log,
		now:            now,
		consumer:       consumer,
		feed:           deps.Feed,
		claimIdle:      claimIdle,
		batchSize:      batchSize,
		block:          block,
		dedupTTL:       dedupTTL,
		lagThreshold:   UsageLogStreamLagThreshold,
		processTimeout: usageLogStreamProcessTimeout,
	}, nil
}

// RunUsageLogStreamLoop 运行消费循环直到 ctx 取消。返回 nil 表示正常停机。
//
// 【启动】确保消费组存在。用 "0" 建组：升级/重启时 Stream 里已受理但未消费
// 的事件必须被读到，不能从 "$" 起跳把它们留在 lag 外。幂等性由 DB 存在性
// 检查与 Redis 去重键共同保证。
//
// 【失败语义】Redis/DB 故障只记录并在下一轮重试；循环不退出（与计费兜底
// 循环一致）。未 ack 的消息留在 pending，60s 后 XAUTOCLAIM 重投。
func RunUsageLogStreamLoop(ctx context.Context, deps UsageLogStreamDeps) error {
	c, err := newUsageLogConsumer(deps)
	if err != nil {
		return err
	}
	if ctx.Err() != nil {
		return nil
	}
	c.log.Info("usage log stream consumer started",
		"stream", UsageLogStreamKey,
		"group", UsageLogStreamGroup,
		"consumer", c.consumer,
		"batch_size", c.batchSize,
		"claim_idle", c.claimIdle,
	)

	if err := c.ensureGroup(ctx); err != nil {
		// Redis 不可用：不阻止 worker 上线（其它循环同样容忍 Redis 抖动），
		// 但必须留下明确日志——此时 Stream 只进不出，lag 会持续上涨。
		c.log.Error("usage log stream: create consumer group failed; will retry",
			"error", err)
	}

	for {
		if ctx.Err() != nil {
			return nil
		}
		messages, err := c.readOnce(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			c.log.Error("usage log stream: read failed; retrying",
				"error", err, "consumer", c.consumer)
			c.reportLag(ctx)
			sleepWithContext(ctx, usageLogStreamErrorBackoff)
			continue
		}
		if len(messages) == 0 {
			c.reportLag(ctx)
			continue
		}
		if err := c.consume(ctx, messages); err != nil {
			// 不 ack：消息留在 pending，由 XAUTOCLAIM 在 60s 后重投。
			if errors.Is(err, errUsageLogStreamAck) {
				// INSERT 已提交、只是 ack 失败：重投会被去重/存在性检查收敛。
				c.log.Error("usage log stream: ack failed after commit; entries may be redelivered",
					"error", err, "count", len(messages), "consumer", c.consumer)
			} else {
				c.log.Error("usage log stream: batch insert failed; messages stay pending",
					"error", err, "count", len(messages), "consumer", c.consumer)
			}
			c.reportLag(ctx)
			sleepWithContext(ctx, usageLogStreamErrorBackoff)
			continue
		}
		c.reportLag(ctx)
	}
}

// ensureGroup 幂等创建消费组（BUSYGROUP 视为成功）。
func (c *usageLogConsumer) ensureGroup(ctx context.Context) error {
	err := c.redis.XGroupCreateMkStream(ctx, UsageLogStreamKey, UsageLogStreamGroup, "0").Err()
	if err == nil || strings.Contains(err.Error(), "BUSYGROUP") {
		return nil
	}
	return err
}

// readOnce 先领取超时 pending，再读取未被投递的新消息。
//
// 单轮最多处理两页（一页 claim + 一页 new），把 CPU 让给 ack 之后的指标上报
// 与停机检查；积压会由循环的下一轮继续消化。
func (c *usageLogConsumer) readOnce(ctx context.Context) ([]redis.XMessage, error) {
	claimed, _, err := c.redis.XAutoClaim(ctx, &redis.XAutoClaimArgs{
		Stream:   UsageLogStreamKey,
		Group:    UsageLogStreamGroup,
		Consumer: c.consumer,
		MinIdle:  c.claimIdle,
		Start:    "0-0",
		Count:    c.batchSize,
	}).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		// 消费组尚不存在（启动时 Redis 抖动）——补建后本轮返回空。
		if strings.Contains(err.Error(), "NOGROUP") {
			if gerr := c.ensureGroup(ctx); gerr != nil {
				return nil, gerr
			}
			return nil, nil
		}
		return nil, fmt.Errorf("claim pending usage log events: %w", err)
	}
	if len(claimed) > 0 {
		return claimed, nil
	}

	streams, err := c.redis.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    UsageLogStreamGroup,
		Consumer: c.consumer,
		Streams:  []string{UsageLogStreamKey, ">"},
		Count:    c.batchSize,
		Block:    c.block,
	}).Result()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		if strings.Contains(err.Error(), "NOGROUP") {
			if gerr := c.ensureGroup(ctx); gerr != nil {
				return nil, gerr
			}
			return nil, nil
		}
		return nil, fmt.Errorf("read usage log events: %w", err)
	}
	var out []redis.XMessage
	for _, stream := range streams {
		out = append(out, stream.Messages...)
	}
	return out, nil
}

// consume 处理一批消息：解析 → 去重 → 事务落库 → 发布 feed → XACK。
//
// 顺序是冻结契约：**先提交 INSERT，后 XACK**。任何一步失败都不 ack。
func (c *usageLogConsumer) consume(ctx context.Context, messages []redis.XMessage) error {
	start := time.Now()
	processCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.processTimeout)
	defer cancel()

	events := c.parseMessages(messages)

	// Redis 侧幂等快查（非退款事件）。命中就跳过，省掉一次 DB 事务；
	// 查不到也不能证明没写过，DB 存在性检查才是正确性兜底。
	events = c.dropDedupedEvents(processCtx, events)

	entries := reduceUsageLogBatch(events)
	result, err := c.writeBatch(processCtx, entries)
	if err != nil {
		metrics.ObserveWorkerTask("log_flush", "failed", time.Since(start))
		return err
	}

	// 提交成功后才置幂等键；键写失败只影响性能，不影响正确性（DB 兜底）。
	c.markDeduped(processCtx, events)

	ids := make([]string, 0, len(messages))
	for _, msg := range messages {
		// 解析失败的消息也 ack：坏消息无法通过重投变好，留在 pending 会
		// 永久卡住整条流（head-of-line block）。它已经被单独记录。
		ids = append(ids, msg.ID)
	}
	if len(ids) > 0 {
		if err := c.redis.XAck(processCtx, UsageLogStreamKey, UsageLogStreamGroup, ids...).Err(); err != nil {
			// ack 失败：INSERT 已提交，消息可能被重投；去重键与 DB 存在性
			// 检查会把重投收敛成幂等命中，不会产生第二行。
			return fmt.Errorf("%w: %v", errUsageLogStreamAck, err)
		}
	}

	// Minor-1：live_feed 放在 XACK 之后。投递是展示面副作用，Redis 半故障
	// 时不应在 ack 之前阻塞消费循环（最坏每批多等 2s × 条目数）。
	// 只有**真正插入新行**才投递，与同步路径语义一致：幂等命中与退款收敛
	// 都不是一次新的公开调用。
	c.publishFeed(result.inserted)

	metrics.ObserveWorkerTask("log_flush", "succeeded", time.Since(start))
	return nil
}

// parseMessages 解析 Stream 消息，返回可处理事件列表。
//
// 解析失败的消息由调用方随整批一起 ack：坏消息无法通过重投变好，留在
// pending 会永久卡住整条流（head-of-line block）。失败原因在这里单独记录。
func (c *usageLogConsumer) parseMessages(messages []redis.XMessage) []UsageLogEvent {
	events := make([]UsageLogEvent, 0, len(messages))
	for _, msg := range messages {
		raw, ok := msg.Values["event"]
		if !ok {
			c.log.Error("usage log stream: message has no event field; acknowledging to avoid head-of-line block",
				"message_id", msg.ID)
			continue
		}
		var payload []byte
		switch v := raw.(type) {
		case string:
			payload = []byte(v)
		case []byte:
			payload = v
		default:
			c.log.Error("usage log stream: event field has unexpected type; acknowledging",
				"message_id", msg.ID, "type", fmt.Sprintf("%T", raw))
			continue
		}
		var event UsageLogEvent
		if err := json.Unmarshal(payload, &event); err != nil {
			c.log.Error("usage log stream: malformed event; acknowledging",
				"message_id", msg.ID, "error", err)
			continue
		}
		if strings.TrimSpace(event.RequestID) == "" {
			c.log.Warn("usage log stream: event missing request_id; acknowledging as poison",
				"message_id", msg.ID)
			continue
		}
		// C1：统一规范化后再进入去重/落库。DB 的 request_id::text 是标准
		// 小写带横线形态；生产 ID 可能是 32 位无横线 hex，不统一就会让
		// 跨批去重和退款收敛全部失配。
		normalized, err := normalizeUsageLogRequestID(event.RequestID)
		if err != nil {
			// I2：非法 request_id 永远无法通过 `$1::uuid`，留在 pending 会
			// 每轮重试并永久阻塞整条流（head-of-line block）。按毒消息 ack。
			c.log.Warn("usage log stream: event request_id is not a valid UUID; acknowledging as poison",
				"message_id", msg.ID, "request_id", event.RequestID)
			continue
		}
		event.RequestID = normalized
		events = append(events, event)
	}
	return events
}

// dropDedupedEvents 用 pipeline 查询幂等键，过滤已处理过的非退款事件。
//
// Redis 故障时**放行**（返回原列表）：宁可多一次 DB 存在性检查，也不能因为
// 幂等键不可用而丢日志。
func (c *usageLogConsumer) dropDedupedEvents(ctx context.Context, events []UsageLogEvent) []UsageLogEvent {
	if len(events) == 0 {
		return events
	}
	keys := make([]string, 0, len(events))
	indexByKey := make(map[string]int, len(events))
	for i, ev := range events {
		if ev.Status == UsageLogStatusRefunded {
			continue
		}
		key := dedupKey(ev.RequestID, ev.Status)
		if _, seen := indexByKey[key]; seen {
			continue
		}
		indexByKey[key] = i
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		return events
	}
	pipe := c.redis.Pipeline()
	cmds := make([]*redis.IntCmd, len(keys))
	for i, key := range keys {
		cmds[i] = pipe.Exists(ctx, key)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		c.log.Warn("usage log stream: dedup lookup failed; proceeding without the fast path",
			"error", err, "consumer", c.consumer)
		return events
	}
	drop := make(map[int]struct{}, len(keys))
	for i, cmd := range cmds {
		if cmd.Val() > 0 {
			drop[indexByKey[keys[i]]] = struct{}{}
		}
	}
	if len(drop) == 0 {
		return events
	}
	out := make([]UsageLogEvent, 0, len(events)-len(drop))
	for i, ev := range events {
		if _, ok := drop[i]; !ok {
			out = append(out, ev)
		}
	}
	return out
}

// markDeduped 在落库成功后写入幂等键。失败只记录：重复事件仍会被 DB 检查拦住。
func (c *usageLogConsumer) markDeduped(ctx context.Context, events []UsageLogEvent) {
	if len(events) == 0 {
		return
	}
	seen := make(map[string]struct{}, len(events))
	pipe := c.redis.Pipeline()
	n := 0
	for _, ev := range events {
		if ev.Status == UsageLogStatusRefunded {
			// 退款不置键：金额收敛必须允许后续更大的退款事件再次执行。
			continue
		}
		key := dedupKey(ev.RequestID, ev.Status)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		pipe.SetNX(ctx, key, "1", c.dedupTTL)
		n++
	}
	if n == 0 {
		return
	}
	if _, err := pipe.Exec(ctx); err != nil {
		c.log.Warn("usage log stream: failed to write dedup keys; correctness relies on the database",
			"error", err, "consumer", c.consumer)
	}
}

// publishFeed 在批量提交后投递 live_feed 事件。
//
// 与同步路径一样使用独立短超时：投递是展示面副作用，失败只告警，绝不因为
// Redis 抖动回滚已提交的日志或阻塞 XACK。
func (c *usageLogConsumer) publishFeed(events []UsageLogEvent) {
	if c.feed == nil || len(events) == 0 {
		return
	}
	for _, ev := range events {
		if !ev.FeedEligible {
			continue
		}
		feedEvent := FeedEvent{
			ID:          feedEventID(ev.RequestID),
			TS:          c.now().UTC().Format(time.RFC3339),
			Model:       ev.Model,
			Upstream:    ev.FeedUpstream,
			TokensIn:    ev.PromptTokens,
			TokensOut:   ev.CompletionTokens,
			Status:      "error",
			CostCredits: 0,
		}
		if ev.LatencyMS != nil {
			feedEvent.LatencyMS = *ev.LatencyMS
		}
		if ev.Status == UsageLogStatusSuccess {
			feedEvent.Status = "ok"
			feedEvent.CostCredits = ev.Cost
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		if err := c.feed.PublishCall(ctx, feedEvent); err != nil {
			c.log.Warn("usage log stream: publish live feed event failed; log persisted",
				"request_id", ev.RequestID, "error", err)
		}
		cancel()
	}
}

// reportLag 上报 Stream 积压（未投递 + pending 未 ack）。
//
// 【Minor-5 已知运维缺口】worker 完全缺位时本循环不运行，指标不会被更新，
// 看板会保留最后一次值（甚至看起来正常）。告警规则必须叠加 `absent()` 或
// `time() - newtg_worker_task_last_success_seconds{task_type="log_flush"}`，
// 不能只依赖 lag 数值本身。
func (c *usageLogConsumer) reportLag(ctx context.Context) {
	groups, err := c.redis.XInfoGroups(ctx, UsageLogStreamKey).Result()
	if err != nil {
		return
	}
	for _, g := range groups {
		if g.Name != UsageLogStreamGroup {
			continue
		}
		lag := streamLagTotal(g.Pending, g.Lag)
		metrics.SetWorkerStreamLag(float64(lag))
		if lag > c.lagThreshold {
			c.log.Warn("usage log stream backlog exceeds the alert threshold",
				"lag", lag, "threshold", c.lagThreshold,
				"consumer", c.consumer)
		}
		return
	}
}

// streamLagTotal 合并「未投递给消费组」与「已投递未 ack」两类积压。
//
// XINFO GROUPS 的 Lag 在无法判定时为 -1（例如 stream 被裁剪过）：此时只用
// Pending 兜底，绝不把 -1 当成「负积压」上报（A13 会把负数误判为正常）。
func streamLagTotal(pending, lag int64) int64 {
	total := pending
	if lag > 0 {
		total += lag
	}
	return total
}

func sleepWithContext(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// ---------- 批量落库 ----------

// usageLogBatchResult 汇总一次批量提交的实际写入。
type usageLogBatchResult struct {
	// inserted 是真正**新插入**的事件（用于 live_feed 判断）。
	inserted []UsageLogEvent
	// converged 是被收敛为 refunded 的已有行数（诊断用）。
	converged int
}

// writeBatch 在单个日志库事务内完成「顾问锁 → 查重 → 批量 INSERT → 退款收敛」。
//
// 为什么仍然使用 request_id 作用域的事务级顾问锁：同步回退写者与消费者都
// 可能在 ambiguous XADD（超时但实际已入队）后写同一请求。锁必须与
// writeUsageLog 完全一致，两条路径才共享同一个写屏障。批内按 request_id
// 排序加锁，避免多消费副本之间形成环。
func (c *usageLogConsumer) writeBatch(ctx context.Context, entries []usageLogBatchEntry) (usageLogBatchResult, error) {
	var result usageLogBatchResult
	if len(entries) == 0 {
		return result, nil
	}
	tx, err := c.logsDB.BeginTx(ctx, nil)
	if err != nil {
		return result, fmt.Errorf("begin usage log batch tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	requestIDs := make([]string, 0, len(entries))
	for _, entry := range entries {
		requestIDs = append(requestIDs, entry.requestID)
	}
	if err := lockUsageLogRequestIDs(ctx, tx, requestIDs); err != nil {
		return result, err
	}

	existing, err := existingUsageLogRequests(ctx, tx, requestIDs)
	if err != nil {
		return result, err
	}

	inserts := make([]UsageLogEvent, 0, len(entries))
	converges := make([]UsageLogEvent, 0, len(entries))
	for _, entry := range entries {
		exists := existing[entry.requestID]
		if !exists {
			// 没有行：优先插入非退款事件（保留 token/model 细节）；只有退款
			// 事件时直接插入退款行（worker 的崩溃兜底占位）。
			if entry.insert != nil {
				inserts = append(inserts, *entry.insert)
			} else if entry.refund != nil {
				inserts = append(inserts, *entry.refund)
			}
			if entry.insert != nil && entry.refund != nil {
				converges = append(converges, *entry.refund)
			}
			continue
		}
		// 已有终态行：success/refused/error 一律幂等跳过（绝不回退 refunded）；
		// 只有退款事件允许单向收敛。
		if entry.refund != nil {
			converges = append(converges, *entry.refund)
		}
	}

	if len(inserts) > 0 {
		if err := insertUsageLogBatch(ctx, tx, inserts); err != nil {
			return result, err
		}
		result.inserted = inserts
	}
	if len(converges) > 0 {
		for _, ev := range converges {
			if err := convergeUsageLogToRefunded(ctx, tx, ev); err != nil {
				return result, err
			}
		}
		result.converged = len(converges)
	}
	if err := tx.Commit(); err != nil {
		return result, fmt.Errorf("commit usage log batch: %w", err)
	}
	return result, nil
}

// lockUsageLogRequestIDs 按排序后的 request_id 逐个获取事务级顾问锁。
//
// 单条 SQL 的排序不保证函数求值顺序（volatile 函数 + ORDER BY），因此这里
// 用 MATERIALIZED CTE 固定 VALUES 的产生顺序；仍保留 Go 侧排序作为第二道
// 保证。锁表达式与 writeUsageLog 完全一致（UUID 规范化后哈希）。
func lockUsageLogRequestIDs(ctx context.Context, tx *sql.Tx, requestIDs []string) error {
	if len(requestIDs) == 0 {
		return nil
	}
	sorted := append([]string(nil), requestIDs...)
	sort.Strings(sorted)
	placeholders := make([]string, len(sorted))
	args := make([]any, len(sorted))
	for i, rid := range sorted {
		placeholders[i] = fmt.Sprintf("($%d::uuid)", i+1)
		args[i] = rid
	}
	query := fmt.Sprintf(`
WITH ids AS MATERIALIZED (
    SELECT id FROM (VALUES %s) AS t(id)
)
SELECT pg_advisory_xact_lock(hashtextextended(id::text, 0)) FROM ids`,
		strings.Join(placeholders, ", "))
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("lock usage log request ids: %w", err)
	}
	return nil
}

// existingUsageLogRequests 一次性读取批内 request_id 的已存在行。
//
// IN 列表（而非数组参数）沿用既有 SQL 风格；FOR UPDATE 锁住已有行，配合
// 顾问锁构成与 writeUsageLog 相同的「单行状态机」写侧屏障。
func existingUsageLogRequests(ctx context.Context, tx *sql.Tx, requestIDs []string) (map[string]bool, error) {
	out := make(map[string]bool, len(requestIDs))
	if len(requestIDs) == 0 {
		return out, nil
	}
	placeholders := make([]string, len(requestIDs))
	args := make([]any, len(requestIDs))
	for i, rid := range requestIDs {
		placeholders[i] = fmt.Sprintf("$%d::uuid", i+1)
		args[i] = rid
	}
	query := fmt.Sprintf(
		`SELECT request_id::text FROM usage_logs WHERE request_id IN (%s) FOR UPDATE`,
		strings.Join(placeholders, ", "))
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query existing usage log batch: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var rid string
		if err := rows.Scan(&rid); err != nil {
			return nil, fmt.Errorf("scan existing usage log batch: %w", err)
		}
		out[rid] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate existing usage log batch: %w", err)
	}
	return out, nil
}

// usageLogBatchInsertSQL 与同步路径的列清单逐列一致，唯一差别是显式写入
// created_at（事件时刻），从而把每条日志落到正确的月分区。
const usageLogBatchInsertSQL = `
INSERT INTO usage_logs (
    request_id, user_id, token_id, token_name, channel_id, upstream,
    model, "group", prompt_tokens, completion_tokens, cache_read_tokens,
    cache_write_tokens, pre_deducted, cost, refunded, latency_ms,
    first_token_ms, stream, status, error_class, error_message,
    retry_chain, other, created_at)`

// insertUsageLogBatch 用一条多行 INSERT 写入整批新行。
//
// 为什么必须一条语句：分区路由与 WAL 写入都是按语句聚合的，逐行 INSERT 会
// 退化成与同步路径一样的事务内多次往返，批量写入的意义就没了。
//
// 参数投影**复用同步路径的 usageLogInsertArgs**（$1..$21），每行只在末尾追加
// created_at（$22）：列语义只有一处定义，批量路径不会把 retry_chain 之类的
// 新列悄悄写成 NULL。token_name/upstream 仍是 SQL 常量 NULL，不占参数。
func insertUsageLogBatch(ctx context.Context, tx *sql.Tx, events []UsageLogEvent) error {
	if len(events) == 0 {
		return nil
	}
	// 每行 24 列，其中 token_name/upstream 显式 NULL；绑定参数 = 共享的 21 个
	// 列参数 + 1 个 created_at。
	const paramsPerRow = 22
	rows := make([]string, len(events))
	args := make([]any, 0, paramsPerRow*len(events))
	for i := range events {
		ev := events[i]
		base := i * paramsPerRow
		rows[i] = fmt.Sprintf(`($%d::uuid, $%d, $%d, NULL, $%d, NULL,
        $%d, $%d, $%d, $%d, $%d,
        $%d, $%d, $%d, $%d, $%d,
        $%d, $%d, $%d, $%d, $%d,
        $%d::jsonb, $%d::jsonb, $%d)`,
			base+1, base+2, base+3, base+4,
			base+5, base+6, base+7, base+8, base+9,
			base+10, base+11, base+12, base+13, base+14,
			base+15, base+16, base+17, base+18, base+19,
			base+20, base+21, base+22)
		args = append(args, usageLogInsertArgs(ev.usageLogRow())...)
		args = append(args, ev.CreatedAt.UTC())
	}
	query := usageLogBatchInsertSQL + "\nVALUES " + strings.Join(rows, ",\n")
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("insert usage log batch: %w", err)
	}
	return nil
}

// convergeUsageLogToRefunded 把已有行单向收敛为 refunded。
//
// 与 writeUsageLog 的 UPDATE 保持逐字段一致：refunded 金额 GREATEST 只增
// 不减，error_* 仅在本次提供时覆盖。**绝不**允许 refunded → success。
//
// 【Minor-2】`cost` 与同步路径一致使用本次事件的 cost 无条件覆盖，因此
// 「只增不减」只对 `refunded` 金额成立：一条更早的退款事件重投时可能把
// cost 改成更小的值。这与同步 `Saturate` 的行为一致（不是批处理引入的
// 新语义），已登记为待统一项。
func convergeUsageLogToRefunded(ctx context.Context, tx *sql.Tx, ev UsageLogEvent) error {
	if _, err := tx.ExecContext(ctx, `
		UPDATE usage_logs
		   SET status = $2,
		       cost = $3,
		       refunded = GREATEST(COALESCE(refunded, 0), $4),
		       error_class = COALESCE(NULLIF($5, ''), error_class),
		       error_message = COALESCE(NULLIF($6, ''), error_message)
		 WHERE request_id = $1::uuid`,
		ev.RequestID, UsageLogStatusRefunded, ev.Cost, ev.Refunded,
		nullStringFromPtr(ev.ErrorClass).String,
		nullStringFromPtr(ev.ErrorMessage).String); err != nil {
		return fmt.Errorf("converge usage log to refunded: %w", err)
	}
	return nil
}

// ---------- Null 类型转换 ----------

func ptrNullInt64(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	out := v.Int64
	return &out
}

func nullInt64FromPtr(v *int64) sql.NullInt64 {
	if v == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *v, Valid: true}
}

func ptrNullString(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	out := v.String
	return &out
}

func nullStringFromPtr(v *string) sql.NullString {
	if v == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: *v, Valid: true}
}
