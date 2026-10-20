package httpapi

// 首页 live_feed 的公开后端（D6b；modules-09 F-09-02 / A6-#4）。
//
// 两个读面共享同一份 Redis 数据：
//   - GET /api/home/live-feed         轮询降级，LRANGE list 最近 N 条；
//   - GET /api/home/live-feed/stream  SSE，订阅 Pub/Sub 频道实时扇出。
//
// 【为什么 SSE 走 Redis Pub/Sub 而不是每个访客直连上游】架构 §1.3 要求任意
// 副本可服务任意请求：事件由计费写入方 PUBLISH 到 Redis，各 gateway 副本
// 各自订阅后扇出给本副本的 SSE 连接。每个访客只占一条本地连接，上游/数据库
// 不随访客数增长。
//
// 【单 IP 并发 ≤5（冻结值）】B9 连接占用面：用 Redis ZSET 做带过期分数的
// 租约，超限返回 429 让前端降级轮询。租约随心跳续期，进程崩溃时由分数过期
// 自动回收，不会永久占住名额。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/billing"
)

const (
	// LiveFeedMaxConnections 是单 IP SSE 并发上限（modules-08 F-08-16 真源；
	// security-07 B9 冻结值）。不是「每秒连接数」——SSE 是长连接。
	LiveFeedMaxConnections = 5

	// LiveFeedHeartbeatInterval 是 SSE 保活注释间隔（F-09-02：15s）。
	LiveFeedHeartbeatInterval = 15 * time.Second

	// liveFeedLeaseTTL 是租约的有效期。取心跳的 4 倍：客户端/进程异常时
	// 最多 60s 释放名额；正常连接每 15s 续期一次，不会误回收。
	liveFeedLeaseTTL = 60 * time.Second

	// 轮询 limit 的默认值与上限（公开页固定取 20；上限防止一次抓取过大）。
	liveFeedDefaultLimit = 20
	liveFeedMaxLimit     = 100

	// liveFeedLeaseKeyPrefix 是单 IP 租约 ZSET 的键前缀。
	liveFeedLeaseKeyPrefix = "newtg:feed:sse:"

	// liveFeedReleaseTimeout 限制释放脚本的耗时（请求已结束，不能被 Redis
	// 抖动挂住连接协程）。
	liveFeedReleaseTimeout = 2 * time.Second

	// problemTypeLiveFeedDisabled 是 live_feed 关闭时的 404 type。
	problemTypeLiveFeedDisabled = "https://newtg.dev/problems/not_found"

	// liveFeedSubscribeAckTimeout 是「建立订阅 → 收到 SUBSCRIBE ack」的
	// 等待上限。必须在写 200 之前完成：一旦进入流式响应，订阅失败的
	// 事件就只能表现为静默断流，客户端无法区分「暂无调用」与「后端故障」。
	liveFeedSubscribeAckTimeout = 3 * time.Second

	// liveFeedSubscriberBuffer 是单条 SSE 连接的本地扇出队列长度。
	// 慢客户端（不读 socket）填满队列后丢事件而不是阻塞整个进程的扇出；
	// SSE 是「最近事件」语义，丢一帧比拖垮其他连接可接受。
	liveFeedSubscriberBuffer = 64
)

// LiveFeedHandlerConfig 是 live_feed 端点的装配输入。
type LiveFeedHandlerConfig struct {
	Redis  *redis.Client
	Config ConfigReader
	Logger *slog.Logger
	// Now 是时钟接缝（租约分数断言）；nil 时使用 time.Now。
	Now func() time.Time
	// HeartbeatInterval / LeaseTTL 是测试接缝；零值时使用上面的冻结常量。
	HeartbeatInterval time.Duration
	LeaseTTL          time.Duration
	// MaxConnections 是测试接缝；零值时为 LiveFeedMaxConnections。
	MaxConnections int
}

// LiveFeedHandlers 持有 live_feed 端点依赖，可并发使用。
//
// 【进程级单一订阅】所有 SSE 连接共享一条 Redis Pub/Sub 订阅，由后台
// 协程把消息广播到各连接的本地 channel。每连接各开一条 Pub/Sub 会让
// 订阅数随访客数增长（Redis 侧每个 SUBSCRIBE 都要维护状态，且各连接
// 各自重连会放大故障抖动）；进程级订阅把 Redis 侧成本固定为副本数。
type LiveFeedHandlers struct {
	rdb       *redis.Client
	config    ConfigReader
	log       *slog.Logger
	now       func() time.Time
	heartbeat time.Duration
	leaseTTL  time.Duration
	maxConns  int

	// subMu 保护订阅生命周期与 subscribers。扇出发送与注销关闭都在锁内
	// 完成，避免「向已关闭 channel 发送」的竞态。
	subMu       sync.Mutex
	pubsub      *redis.PubSub
	subscribers map[chan []byte]struct{}
}

// NewLiveFeedHandlers 构造 live_feed 端点。Redis 必填：没有 Redis 就没有
// 事件源，不能用一个进程内假缓冲冒充「真实调用证据」。
func NewLiveFeedHandlers(cfg LiveFeedHandlerConfig) (*LiveFeedHandlers, error) {
	if cfg.Redis == nil {
		return nil, fmt.Errorf("httpapi: live feed requires redis")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	heartbeat := cfg.HeartbeatInterval
	if heartbeat <= 0 {
		heartbeat = LiveFeedHeartbeatInterval
	}
	leaseTTL := cfg.LeaseTTL
	if leaseTTL <= 0 {
		leaseTTL = liveFeedLeaseTTL
	}
	maxConns := cfg.MaxConnections
	if maxConns <= 0 {
		maxConns = LiveFeedMaxConnections
	}
	return &LiveFeedHandlers{
		rdb: cfg.Redis, config: cfg.Config, log: logger, now: now,
		heartbeat: heartbeat, leaseTTL: leaseTTL, maxConns: maxConns,
		subscribers: make(map[chan []byte]struct{}),
	}, nil
}

// RegisterLiveFeedRoutes 注册公开 live_feed 端点（精确模式，绕过 SessionAuth）。
func (h *LiveFeedHandlers) RegisterLiveFeedRoutes(rt *Router) {
	rt.HandlePublic("GET /api/home/live-feed", http.HandlerFunc(h.handleList))
	rt.HandlePublic("GET /api/home/live-feed/stream", http.HandlerFunc(h.handleStream))
}

// LiveFeedListResponse 是轮询降级响应。
//
// events 直接透传 Redis 里的 JSON 对象（json.RawMessage），不二次投影：
// 事件在写入侧已完成脱敏，读取侧再 Marshal 一次只会引入字段漂移。
//
// window 是 F-09-02 的聚合条（近 60 秒 = 当前分钟桶近似，A6 冻结语义）：
// calls 计所有事件，cost_credits 只计成功事件的整数积分。逐行事件仍然
// 不含任何费用字段（裁决 A-1）——费用只在这里以平台级聚合出现。
type LiveFeedListResponse struct {
	Events []json.RawMessage  `json:"events"`
	Window billing.FeedWindow `json:"window"`
}

// homeBlockEnabled 读取 site.home_blocks.live_feed；缺省/损坏按注册表默认
// true 处理，只有显式 false 才关闭端点（F-09-02 前置条件）。
func (h *LiveFeedHandlers) homeBlockEnabled() bool {
	if h.config == nil {
		return true
	}
	raw, ok := h.config.Get("site.home_blocks")
	if !ok {
		return true
	}
	var blocks map[string]any
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return true
	}
	value, ok := blocks["live_feed"].(bool)
	if !ok {
		return true
	}
	return value
}

// allowPublicRead 消耗 B8 桶（60 次/分钟/IP+path，fail-open）。
func (h *LiveFeedHandlers) allowPublicRead(w http.ResponseWriter, r *http.Request) bool {
	over, retry, err := allowRequestRate(r.Context(), h.rdb,
		PublicReadRateLimitKey(clientIP(r), r.URL.Path), PublicReadRateLimit, PublicReadRateWindow)
	if err != nil {
		h.log.Warn("live feed rate limiter unavailable; failing open", "error", err)
		return true
	}
	if over {
		writeRateLimited(w, retry, "Too many requests")
		return false
	}
	return true
}

func (h *LiveFeedHandlers) handleList(w http.ResponseWriter, r *http.Request) {
	if !h.homeBlockEnabled() {
		writeProblem(w, http.StatusNotFound, problemTypeLiveFeedDisabled, "Not Found")
		return
	}
	if !h.allowPublicRead(w, r) {
		return
	}
	limit, ok := parseLiveFeedLimit(r.URL.Query().Get("limit"))
	if !ok {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid limit")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	raw, err := h.rdb.LRange(ctx, billing.FeedListKey, 0, int64(limit-1)).Result()
	if err != nil {
		// list 是唯一数据源；Redis 不可用时不能返回 200 空数组（那会让前端
		// 把故障渲染成「暂无近期调用」并缓存）。
		h.log.Error("live feed: read list failed", "error", err)
		w.Header().Set("Retry-After", "5")
		writeProblem(w, http.StatusServiceUnavailable, authProblemServiceUnavailable,
			"Live feed is temporarily unavailable")
		return
	}
	events := make([]json.RawMessage, 0, len(raw))
	for _, item := range raw {
		payload := []byte(item)
		// 只透传合法 JSON：Redis 是共享基础设施，防御性拒绝损坏条目，
		// 避免把非法 body 拼进响应或 SSE 帧。
		if json.Valid(payload) {
			events = append(events, json.RawMessage(payload))
		}
	}

	// 聚合条与事件列表共享 Redis，但失败口径不同：list 是唯一内容源，
	// 读失败即 503（不能把故障伪装成空列表）；window 是补充读数，读失败
	// 时按冻结语义降级为 0/0 并照常返回事件，不让一个辅助统计放大故障面。
	window, windowErr := billing.ReadFeedWindow(ctx, h.rdb, h.now())
	if windowErr != nil {
		h.log.Warn("live feed: read window failed; returning zero aggregate", "error", windowErr)
		window = billing.FeedWindow{}
	}

	// 实时调用列表不得被任何中间层缓存（F-09-02）。
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, LiveFeedListResponse{Events: events, Window: window})
}

// parseLiveFeedLimit 解析 limit：缺省 20；非整数或 <1 → 400；>100 截到 100。
//
// 上限是防御性上限而不是错误：前端固定传 20，公开面不应因为多要几条就
// 得到 400；截断同时限制单次响应体大小。
func parseLiveFeedLimit(raw string) (int, bool) {
	if raw == "" {
		return liveFeedDefaultLimit, true
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 {
		return 0, false
	}
	if value > liveFeedMaxLimit {
		value = liveFeedMaxLimit
	}
	return value, true
}

// ---------------------------------------------------------------------------
// SSE
// ---------------------------------------------------------------------------

// acquireLiveFeedLeaseScript 原子完成「清理过期租约 → 计数 → 占用名额」。
//
// 分数是租约到期时刻（ms）。先删 score < now 的成员，使崩溃进程的名额
// 在一个 TTL 后自动回收；计数与 ZADD 必须同一原子单元，否则并发连接会
// 同时读到 n=4 并各自插入，突破 5 条上限。
var acquireLiveFeedLeaseScript = redis.NewScript(`
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', ARGV[1])
local n = redis.call('ZCARD', KEYS[1])
if n >= tonumber(ARGV[2]) then
  return 0
end
redis.call('ZADD', KEYS[1], ARGV[3], ARGV[4])
redis.call('PEXPIRE', KEYS[1], ARGV[5])
return 1
`)

// renewLiveFeedLeaseScript 延长本连接的名额；返回 0 表示租约已丢失。
//
// 只有成员仍在 ZSET 里才续期：键被淘汰/手动清理后继续服务会让并发计数
// 失真，此时主动断开、让客户端重连并重新抢名额。
var renewLiveFeedLeaseScript = redis.NewScript(`
if redis.call('ZSCORE', KEYS[1], ARGV[1]) == false then
  return 0
end
redis.call('ZADD', KEYS[1], ARGV[2], ARGV[1])
redis.call('PEXPIRE', KEYS[1], ARGV[3])
return 1
`)

// LiveFeedLeaseKey 返回单 IP 的租约键（导出便于测试断言）。
func LiveFeedLeaseKey(ip string) string {
	return liveFeedLeaseKeyPrefix + ip
}

// acquireLiveFeedLease 尝试占用一个并发名额；false 表示已达上限。
func (h *LiveFeedHandlers) acquireLiveFeedLease(ctx context.Context, ip, member string) (bool, error) {
	now := h.now().UnixMilli()
	expireAt := now + h.leaseTTL.Milliseconds()
	result, err := acquireLiveFeedLeaseScript.Run(ctx, h.rdb,
		[]string{LiveFeedLeaseKey(ip)},
		strconv.FormatInt(now, 10),
		strconv.Itoa(h.maxConns),
		strconv.FormatInt(expireAt, 10),
		member,
		strconv.FormatInt(h.leaseTTL.Milliseconds(), 10),
	).Int()
	if err != nil {
		return false, fmt.Errorf("httpapi: acquire live feed lease: %w", err)
	}
	return result == 1, nil
}

// renewLiveFeedLease 续期；返回 false 表示名额已丢失。
func (h *LiveFeedHandlers) renewLiveFeedLease(ctx context.Context, ip, member string) bool {
	now := h.now().UnixMilli()
	expireAt := now + h.leaseTTL.Milliseconds()
	result, err := renewLiveFeedLeaseScript.Run(ctx, h.rdb,
		[]string{LiveFeedLeaseKey(ip)},
		member,
		strconv.FormatInt(expireAt, 10),
		strconv.FormatInt(h.leaseTTL.Milliseconds(), 10),
	).Int()
	if err != nil {
		h.log.Warn("live feed: renew lease failed", "error", err)
		return true // 网络抖动不立即断流；租约 TTL 仍兜底
	}
	return result == 1
}

// releaseLiveFeedLease 释放名额（ZREM）。失败只影响计数回收时点，TTL 会兜底。
func (h *LiveFeedHandlers) releaseLiveFeedLease(ip, member string) {
	ctx, cancel := context.WithTimeout(context.Background(), liveFeedReleaseTimeout)
	defer cancel()
	if err := h.rdb.ZRem(ctx, LiveFeedLeaseKey(ip), member).Err(); err != nil {
		h.log.Warn("live feed: release lease failed; TTL will reclaim", "error", err)
	}
}

// newLiveFeedConnID 生成跨副本唯一的连接成员 id。
func newLiveFeedConnID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("httpapi: generate live feed connection id: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// ensureFeedSubscription 保证本进程已建立共享订阅，且订阅已收到 Redis 的
// SUBSCRIBE ack。返回 nil 表示「此后发布的事件一定会进入本地扇出」，因此
// 调用方可以安全地写 200；返回错误时连接必须 503，绝不能用空流冒充在线。
//
// 【为什么要等 ack】go-redis 的 Subscribe 只是异步发出命令并立即返回对象；
// 真正的订阅生效在 ack 之后。若在 ack 前收到的事件已经发布，SSE 会永久
// 缺失它（Pub/Sub 不补发）。等待 ack 把「订阅就绪」变成可断言的时序点，
// 也是单测消除 flake 的依据。
func (h *LiveFeedHandlers) ensureFeedSubscription(ctx context.Context) error {
	// 整个握手（含等待 ack）都在锁内完成：并发首连必须等同一个 ack，
	// 不能在订阅尚未生效时就返回 nil 去写 200。
	h.subMu.Lock()
	defer h.subMu.Unlock()
	if h.pubsub != nil {
		return nil
	}

	// 用 Background：订阅是进程级资源，不能随首个访客断开而被取消。
	ps := h.rdb.Subscribe(context.Background(), billing.FeedChannel)

	ackCtx, cancel := context.WithTimeout(ctx, liveFeedSubscribeAckTimeout)
	defer cancel()
	for {
		raw, err := ps.Receive(ackCtx)
		if err != nil {
			_ = ps.Close()
			return fmt.Errorf("httpapi: subscribe live feed channel: %w", err)
		}
		// Receive 可能先返回订阅确认（*redis.Subscription），也可能返回期间
		// 到达的消息；只有确认才是「已生效」的证据。
		if sub, ok := raw.(*redis.Subscription); ok && sub.Kind == "subscribe" {
			break
		}
	}

	h.pubsub = ps
	go h.fanoutFeed(ps)
	return nil
}

// detachPubSub 仅在字段仍指向 ps 时清空，避免误清掉重连后建立的新订阅。
func (h *LiveFeedHandlers) detachPubSub(ps *redis.PubSub) {
	h.subMu.Lock()
	if h.pubsub == ps {
		h.pubsub = nil
	}
	h.subMu.Unlock()
}

// fanoutFeed 读取共享订阅并把事件广播给本进程的 SSE 连接。
//
// 订阅异常退出时清空 pubsub，让下一条连接重新建立订阅；否则端会一直
// 认为「已有订阅」却再也收不到事件。Close 造成的 ErrClosed 同样走清理，
// 不重复告警。
func (h *LiveFeedHandlers) fanoutFeed(ps *redis.PubSub) {
	defer func() {
		h.detachPubSub(ps)
		_ = ps.Close()
	}()
	for {
		msg, err := ps.ReceiveMessage(context.Background())
		if err != nil {
			if errors.Is(err, redis.ErrClosed) {
				return
			}
			// 连接抖动由 go-redis 自动重连；短暂退避避免错误风暴打满日志。
			h.log.Warn("live feed: shared subscription receive failed", "error", err)
			time.Sleep(50 * time.Millisecond)
			continue
		}
		payload := []byte(msg.Payload)
		h.subMu.Lock()
		for ch := range h.subscribers {
			select {
			case ch <- payload:
			default:
				// 慢客户端队列已满：丢弃这一帧，不阻塞其他连接。
			}
		}
		h.subMu.Unlock()
	}
}

// registerSubscriber 注册一条本地扇出队列；调用方退出时必须 unregister。
func (h *LiveFeedHandlers) registerSubscriber() chan []byte {
	ch := make(chan []byte, liveFeedSubscriberBuffer)
	h.subMu.Lock()
	h.subscribers[ch] = struct{}{}
	h.subMu.Unlock()
	return ch
}

// unregisterSubscriber 注销并关闭队列。关闭在锁内进行，与 fanoutFeed 的
// 锁内发送互斥，因此不会出现向已关闭 channel 发送的 panic。
func (h *LiveFeedHandlers) unregisterSubscriber(ch chan []byte) {
	h.subMu.Lock()
	if _, ok := h.subscribers[ch]; ok {
		delete(h.subscribers, ch)
		close(ch)
	}
	h.subMu.Unlock()
}

// Close 释放进程级订阅并关闭所有本地队列（测试/进程优雅退出使用）。
// 之后若有新连接，会按需重新建立订阅。
func (h *LiveFeedHandlers) Close() error {
	h.subMu.Lock()
	ps := h.pubsub
	h.pubsub = nil
	for ch := range h.subscribers {
		delete(h.subscribers, ch)
		close(ch)
	}
	h.subMu.Unlock()
	// 锁外关闭：fanoutFeed 退出时需要拿 subMu 清理字段。
	if ps != nil {
		return ps.Close()
	}
	return nil
}

func (h *LiveFeedHandlers) handleStream(w http.ResponseWriter, r *http.Request) {
	if !h.homeBlockEnabled() {
		writeProblem(w, http.StatusNotFound, problemTypeLiveFeedDisabled, "Not Found")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
		return
	}
	member, err := newLiveFeedConnID()
	if err != nil {
		h.log.Error("live feed: connection id failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
		return
	}

	ip := clientIP(r)
	acquired, err := h.acquireLiveFeedLease(r.Context(), ip, member)
	if err != nil {
		// 并发上限依赖 Redis；无法判定时必须拒绝新连接（fail-closed），
		// 让客户端降级轮询，而不是在故障期间放开连接数。
		h.log.Error("live feed: lease acquisition failed", "error", err)
		w.Header().Set("Retry-After", "5")
		writeProblem(w, http.StatusServiceUnavailable, authProblemServiceUnavailable,
			"Live feed is temporarily unavailable")
		return
	}
	if !acquired {
		// 超限拒绝新连接（B9）；Retry-After 给一个短间隔，前端按 F-09-02
		// 退避重连并同时降级为轮询。
		writeRateLimited(w, 5*time.Second, "Too many live feed connections")
		return
	}
	defer h.releaseLiveFeedLease(ip, member)

	// 先确认进程级订阅已生效（含 SUBSCRIBE ack）再写响应头：订阅失败时
	// 还能返回 503；一旦进入流式响应，任何错误都只能表现为断流。
	ackCtx, ackCancel := context.WithTimeout(r.Context(), liveFeedSubscribeAckTimeout)
	err = h.ensureFeedSubscription(ackCtx)
	ackCancel()
	if err != nil {
		h.log.Error("live feed: subscribe failed", "error", err)
		w.Header().Set("Retry-After", "5")
		writeProblem(w, http.StatusServiceUnavailable, authProblemServiceUnavailable,
			"Live feed is temporarily unavailable")
		return
	}
	events := h.registerSubscriber()
	defer h.unregisterSubscriber(events)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	// 关闭代理层缓冲：SSE 需要逐帧到达浏览器（Caddy 的压缩已在边缘配置
	// 禁用；X-Accel-Buffering 兜住 nginx 形态的部署）。
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ticker := time.NewTicker(h.heartbeat)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case payload, ok := <-events:
			if !ok {
				// 进程关闭（Close）会关闭本地队列，连接随之结束。
				return
			}
			if !json.Valid(payload) {
				// 损坏的 Redis 条目不能拼成 SSE data（换行会破坏帧结构）。
				h.log.Warn("live feed: skip invalid event payload")
				continue
			}
			frame := make([]byte, 0, len(payload)+20)
			frame = append(frame, "event: call\ndata: "...)
			frame = append(frame, payload...)
			frame = append(frame, '\n', '\n')
			if _, err := w.Write(frame); err != nil {
				return
			}
			flusher.Flush()
		case <-ticker.C:
			// 心跳同时续租；租约丢失（键被淘汰）时主动断开，由客户端重连。
			if !h.renewLiveFeedLease(r.Context(), ip, member) {
				return
			}
			if _, err := w.Write([]byte(": heartbeat\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
