// GET /api/home/stats —— 首页统计数据条（modules-09 F-09-04；R-PUB-08）。
//
// 端点契约（M6c Task 7 冻结）：
//
//	200 {"upstreams":42,"models":130,"endpoints":3,"routing_dims":6}
//
// 口径：
//   - upstreams  = COUNT(DISTINCT channels.type) WHERE status=1（真实计数）；
//   - models     = 与 /api/pricing 完全相同的可见集合计数（status=1 且无表达式）；
//   - endpoints  = 冻结常量 3；
//   - routing_dims = 冻结常量 6。
//
// 0 如实返回（前端按 F-09-04 在 0 时隐藏区块）；失败绝不返回 200 空对象——
// 用空对象伪装「暂无数据」会让前端把一次数据库故障缓存 24h 并隐藏统计条。
package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/store"
)

const (
	// publicEndpointCount 是兼容协议端点目录的冻结大小（R-PUB-08）。
	//
	// 【来源与冻结理由】modules-09 F-09-04 的数据字段表 + B3 契约汇总：
	// 公开页承诺的「兼容端点」是三个协议族入口——
	//   openai.chat/completions（/v1/chat/completions）
	//   anthropic.messages（/v1/messages）
	//   gemini.generateContent（/v1beta/models/{model}:generateContent）
	// 【为什么不数 DB】调度端点不是数据库里的行，而是中继代码注册的路由；
	// 没有任何表能作为真源。若去数「当前挂载的 HTTP 路由」，一个只服务内部
	// 调用的端点（如 /v1/models）或未来新增的辅助端点都会污染对外承诺的数字，
	// 而且这个数字会随部署形态漂移。它是对外契约的一部分，因此冻结为常量。
	publicEndpointCount int64 = 3

	// publicRoutingDims 是调度控制维度的冻结大小（R-PUB-08）。
	//
	// 【来源与冻结理由】modules-09 F-09-04 的字段说明列出的调度维度：
	//   priority（优先级）/ weight（权重）/ group（分组）/
	//   model-allowlist（模型白名单）/ affinity（亲和）/ retry（重试）
	// 合计 6 项。
	// 【为什么不数 DB】这些维度是调度器与配置注册表的实现特征，不是目录数据：
	// rl.affinity / rl.retry 等设置键本身随波次增减，把「注册表里有多少键」
	// 当成维度数会让一次内部重构改变公开页的宣传口径。冻结常量与端点数是同一
	// 类「对外承诺」，只能随产品能力明确变更而修改。
	publicRoutingDims int64 = 6

	// homeStatsCacheTTL 是服务端缓存窗口（R-PUB-06：/api/home/stats 5 分钟）。
	// 与响应头 max-age=300 同值：客户端与服务端看到同一份陈旧上界。
	homeStatsCacheTTL = 5 * time.Minute

	// homeStatsLoadTimeout 约束缓存装载的 DB 查询。用独立的、不随请求取消的
	// context：首个请求断连不应让所有等待同一缓存的请求一起失败。
	homeStatsLoadTimeout = 5 * time.Second
)

// HomeStatsHandlerConfig 是首页统计端点的装配输入。
type HomeStatsHandlerConfig struct {
	Repo   store.PublicPricingRepo
	Redis  *redis.Client
	Logger *slog.Logger
	// Now 是时钟接缝（缓存过期测试）；nil 时使用 time.Now。
	Now func() time.Time
	// CacheTTL 是缓存窗口接缝；0 时使用 homeStatsCacheTTL。
	CacheTTL time.Duration
}

// HomeStatsHandlers 持有首页统计端点依赖，可并发使用。
type HomeStatsHandlers struct {
	repo store.PublicPricingRepo
	rdb  *redis.Client
	log  *slog.Logger
	now  func() time.Time
	ttl  time.Duration

	// mu 同时保护 cached/loadedAt 并充当单飞闸门：缓存过期后第一个请求在
	// 持锁期间查询，其余请求排队后直接复用结果。公开页的突发流量下，若不加
	// 这把锁，一次过期会让所有并发请求同时打到数据库（缓存击穿）。
	mu       sync.Mutex
	cached   *store.PublicCounts
	loadedAt time.Time
}

// NewHomeStatsHandlers 构造首页统计端点。Repo 必填。
func NewHomeStatsHandlers(cfg HomeStatsHandlerConfig) (*HomeStatsHandlers, error) {
	if cfg.Repo == nil {
		return nil, errHomeStatsRepoRequired
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	ttl := cfg.CacheTTL
	if ttl <= 0 {
		ttl = homeStatsCacheTTL
	}
	return &HomeStatsHandlers{repo: cfg.Repo, rdb: cfg.Redis, log: logger, now: now, ttl: ttl}, nil
}

// RegisterHomeStatsRoutes 注册公开统计端点（精确模式，绕过 SessionAuth）。
func (h *HomeStatsHandlers) RegisterHomeStatsRoutes(rt *Router) {
	rt.HandlePublic("GET /api/home/stats", http.HandlerFunc(h.handleHomeStats))
}

// HomeStatsView 是响应体。字段是整数（不是 int64 字符串）：统计条是计数，
// 4 项都远小于 2^53，不存在前端精度问题；与 ModelCard 的金额字段区别对待。
type HomeStatsView struct {
	Upstreams   int64 `json:"upstreams"`
	Models      int64 `json:"models"`
	Endpoints   int64 `json:"endpoints"`
	RoutingDims int64 `json:"routing_dims"`
}

func (h *HomeStatsHandlers) handleHomeStats(w http.ResponseWriter, r *http.Request) {
	// B8 公开只读桶：60 次/分钟/IP+path（security-07 §2.1 冻结值）。
	// 公开面 fail-open（§2.3）：限流器故障时放行并告警，不因 Redis 故障
	// 让首页统计整体不可用。
	if !h.allowPublicRead(w, r) {
		return
	}

	counts, err := h.loadCounts(r.Context())
	if err != nil {
		h.log.Error("home stats: load counts failed", "error", err)
		// 500 problem，绝不 200 空对象（brief 冻结的失败语义）。
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
		return
	}

	// 服务端缓存与客户端缓存同窗口；0 值也缓存——「暂无数据」是合法状态，
	// 反复查库不会让它变出模型。
	w.Header().Set("Cache-Control", "public, max-age=300")
	writeJSON(w, http.StatusOK, HomeStatsView{
		Upstreams:   counts.Upstreams,
		Models:      counts.Models,
		Endpoints:   publicEndpointCount,
		RoutingDims: publicRoutingDims,
	})
}

// allowPublicRead 消耗 B8 桶；超限时写 429 并返回 false。
func (h *HomeStatsHandlers) allowPublicRead(w http.ResponseWriter, r *http.Request) bool {
	if h.rdb == nil {
		return true
	}
	over, retry, err := allowRequestRate(r.Context(), h.rdb,
		PublicReadRateLimitKey(clientIP(r), r.URL.Path), PublicReadRateLimit, PublicReadRateWindow)
	if err != nil {
		h.log.Warn("home stats rate limiter unavailable; failing open", "error", err)
		return true
	}
	if over {
		writeRateLimited(w, retry, "Too many requests")
		return false
	}
	return true
}

// loadCounts 返回缓存值；过期时在持锁期间重新装载。
func (h *HomeStatsHandlers) loadCounts(ctx context.Context) (store.PublicCounts, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.cached != nil && h.now().Sub(h.loadedAt) < h.ttl {
		return *h.cached, nil
	}
	// 不继承请求取消：一旦开始装载，就让窗口内的所有排队请求共享这次结果；
	// 首个请求断连不能把其余请求变成 500。超时仍由独立上限兜底。
	loadCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), homeStatsLoadTimeout)
	defer cancel()
	counts, err := h.repo.LoadPublicCounts(loadCtx)
	if err != nil {
		return store.PublicCounts{}, err
	}
	h.cached = &counts
	h.loadedAt = h.now()
	return counts, nil
}
