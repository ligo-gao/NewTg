package httpapi

// 网关侧渠道在飞并发限制（W8/MJ-d）。
//
// 【设计边界（relay-05 §5）】Discord 账号池、账号级并发、排队与重分配全部
// 在上游 midjourney-proxy 内；网关**不维护账号清单**，也**不做** per-account
// 并发。本限制器只保护「同一渠道的上游提交端点」：避免单个副本在突发流量下
// 对同一 base URL 无界并发（连接/上游队列被打满），并给调用方一个明确的
// 429 而不是无限等待。
//
// 【跨副本语义】进程内计数器只约束单副本。设计把网关视为无状态集群、上游
// 代理持有真实队列；跨副本的全局上限需要 Redis 在飞租约（当前未设计）。
// 这里登记为已知边界：N 个副本时最坏并发 ≈ N × mj.channel_max_inflight。
//
// 【为什么是提交并发而不是任务并发】任务在提交成功后由上游异步执行，网关
// 不再持有执行槽；对「已受理任务数」设上限会把异步任务族变成同步队列，
// 与设计「网关不做 per-MJ 并发队列」冲突。

import "sync"

// MJChannelLimiter 是渠道在飞计数的接缝（测试可注入确定性实现）。
//
// limit <= 0 表示调用方已确认「不限」，实现应放行；配置注册表保证生产值
// 落在 [1,1000]，因此生产不会走到不限分支。
type MJChannelLimiter interface {
	TryAcquire(channelID int64, limit int) (release func(), ok bool)
}

// mjDefaultChannelMaxInflight 是缺失设置时的单渠道在飞上限。
//
// 取 8 与仓内既有的可控并发口径一致（relay-06 插件运行时并发 8、
// billing-02 上游同步并发 8）。这是网关侧的保护值，不是上游账号池容量；
// 运营可用 mj.channel_max_inflight 调整。设计未冻结具体数值，已在 W8/MJ-d
// 报告中登记为待回写项。
const mjDefaultChannelMaxInflight = 8

// mjMaxChannelMaxInflight 与设置注册表的上限一致（防御脏配置）。
const mjMaxChannelMaxInflight = 1000

// mjInFlightLimiter 是进程内的按渠道计数器。
type mjInFlightLimiter struct {
	mu       sync.Mutex
	inflight map[int64]int
}

func newMJInFlightLimiter() *mjInFlightLimiter {
	return &mjInFlightLimiter{inflight: make(map[int64]int)}
}

// TryAcquire 尝试占用一个渠道槽。失败立即返回（不排队）：
//
//   - 排队会把请求 goroutine 和上游 60s 超时叠在一起，放大尾延迟；
//   - 设计允许「排队或 429」，429 让客户端显式退避，且不占用资金
//     （调用点在 PreConsume 之前，被拒请求不计费）。
func (l *mjInFlightLimiter) TryAcquire(channelID int64, limit int) (func(), bool) {
	if l == nil {
		return func() {}, true
	}
	if limit <= 0 {
		return func() {}, true
	}
	if limit > mjMaxChannelMaxInflight {
		limit = mjMaxChannelMaxInflight
	}
	l.mu.Lock()
	if l.inflight[channelID] >= limit {
		l.mu.Unlock()
		return nil, false
	}
	l.inflight[channelID]++
	l.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			if n := l.inflight[channelID]; n <= 1 {
				delete(l.inflight, channelID)
			} else {
				l.inflight[channelID] = n - 1
			}
		})
	}, true
}

// inflightCount 是观测/测试读取口（并发安全）。
func (l *mjInFlightLimiter) inflightCount(channelID int64) int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inflight[channelID]
}
