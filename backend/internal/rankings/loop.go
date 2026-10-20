package rankings

// 排行榜快照 worker（D6a；R-D6-08/R-D6-09、billing-06 §8）。
//
// 每个周期一个独立循环：
//   1. 读 nav.rankings.enabled，关闭时跳过（页面不可达时不必消耗日志库扫描）；
//   2. 用 Redis SET NX PX 抢 period 级租约（TTL=刷新间隔），抢不到说明
//      另一副本正在刷，直接跳过；
//   3. 聚合当前/上一窗口 → 单事务写快照；
//   4. 成功后 DEL 公开缓存并 PUBLISH 刷新事件。
//
// 【为什么锁 TTL = 刷新间隔】公开数据的可接受陈旧度就是刷新间隔；锁一旦
// 因进程崩溃滞留，最多一个间隔后自动释放，不会永久阻塞该 period。运行
// 时间正常时一次聚合远小于间隔，租约不会在提交前过期。
//
// 【为什么抢不到锁不是错误】多副本部署下这是预期路径；把它记成告警会让
// 每个副本每轮都报警。只有 Redis 本身不可用才是 Warn。

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// 公开缓存与刷新事件（modules-09 §C1.7）。D6b 的 handler 用同一组常量，
// 避免 worker 与读取面拼出不同的键。
const (
	cacheKeyPrefix = "rankings:public:"
	// RefreshedChannel 是快照刷新广播频道；payload 为 {period, computed_at}。
	RefreshedChannel = "newtg:rankings.refreshed"
	lockKeyPrefix    = "rankings:lock:"
)

// CacheKey 返回 period 的公开响应缓存键（完整 DTO JSON 的 Redis key）。
func CacheKey(period Period) string { return cacheKeyPrefix + string(period) }

// LockKey 返回 period 的刷新租约键（导出仅为可观测性/测试断言）。
func LockKey(period Period) string { return lockKeyPrefix + string(period) }

// Deps 是 worker 的运行依赖。
type Deps struct {
	// MainDB 写快照与实体槽位，并从 model_pricing/model_meta 补元数据。
	MainDB *sql.DB
	// LogsDB 只读 usage_logs（与主库分治，platform-03 §3.4）。
	LogsDB *sql.DB
	// Redis 提供 period 级租约、公开缓存失效与刷新广播。
	Redis *redis.Client
	// Config 是站点设置快照（site.timezone / site.rankings_min_samples /
	// nav.rankings.enabled）。
	Config ConfigReader
	// Logger 为空时使用 slog.Default。
	Logger *slog.Logger
	// Now 为空时使用 time.Now（测试注入固定时刻，窗口断言才可复现）。
	Now func() time.Time
}

func (d Deps) normalized() (Deps, error) {
	if d.MainDB == nil {
		return Deps{}, fmt.Errorf("rankings: MainDB is required")
	}
	if d.LogsDB == nil {
		return Deps{}, fmt.Errorf("rankings: LogsDB is required")
	}
	if d.Redis == nil {
		return Deps{}, fmt.Errorf("rankings: Redis is required")
	}
	if d.Config == nil {
		return Deps{}, fmt.Errorf("rankings: Config is required")
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	return d, nil
}

// RunLoop 阻塞运行四个周期的刷新循环，直到 ctx 结束。
//
// 调用方（platform.runWorker）把它放进独立 goroutine 并用 WaitGroup 等待；
// 本函数自身也等待所有 period goroutine 退出后才返回，保证 ctx 取消后
// 没有残留的聚合 SQL 在连接池关闭后继续跑。
func RunLoop(ctx context.Context, deps Deps) error {
	deps, err := deps.normalized()
	if err != nil {
		return err
	}
	deps.Logger.Info("rankings worker started",
		"periods", len(allPeriods),
	)

	var wg sync.WaitGroup
	for _, period := range allPeriods {
		wg.Add(1)
		go func(p Period) {
			defer wg.Done()
			runPeriod(ctx, deps, p)
		}(period)
	}
	wg.Wait()
	deps.Logger.Info("rankings worker stopped")
	return nil
}

// runPeriod 是单周期的 ticker 循环。
func runPeriod(ctx context.Context, deps Deps, period Period) {
	// 启动即刷一次：冷启动、重启或刚打开 nav.rankings 时不必等一个完整间隔。
	// 多副本同时启动的竞争由 Redis 租约收敛为一次实际执行。
	refreshPeriod(ctx, deps, period)

	ticker := time.NewTicker(period.RefreshInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			refreshPeriod(ctx, deps, period)
		}
	}
}

// refreshPeriod 执行一轮「抢锁 → 聚合 → 写快照 → 失效缓存」。
// 错误只记录不返回：下一轮 tick 会重试，单轮失败不应终止 worker。
func refreshPeriod(ctx context.Context, deps Deps, period Period) {
	if ctx.Err() != nil {
		return
	}
	if !RankingsEnabled(deps.Config) {
		deps.Logger.Debug("rankings: 导航开关关闭，跳过刷新", "period", period)
		return
	}

	token, err := newLockToken()
	if err != nil {
		deps.Logger.Error("rankings: 生成租约令牌失败", "period", period, "error", err)
		return
	}
	key := LockKey(period)
	ttl := period.RefreshInterval()
	acquired, err := deps.Redis.SetNX(ctx, key, token, ttl).Result()
	if err != nil {
		if ctx.Err() == nil {
			deps.Logger.Warn("rankings: 获取刷新租约失败", "period", period, "error", err)
		}
		return
	}
	if !acquired {
		deps.Logger.Debug("rankings: 其他副本持有租约，跳过本轮", "period", period)
		return
	}
	// 释放租约用「比较后删除」：TTL 恰好在提交前过期、另一个副本已抢到时，
	// 直接 DEL 会误删别人的锁。ctx 已取消时用独立短超时，保证停机也能释放。
	defer releaseLock(deps, key, token)

	now := deps.Now()
	loc := ResolveLocation(deps.Config, deps.Logger)
	minSamples := ResolveMinSamples(deps.Config, deps.Logger)

	agg, err := aggregatePeriod(ctx, deps.MainDB, deps.LogsDB, period, now, loc, minSamples)
	if err != nil {
		if ctx.Err() == nil {
			deps.Logger.Warn("rankings: 聚合失败", "period", period, "error", err)
		}
		return
	}

	runID, err := writeSnapshot(ctx, deps.MainDB, snapshotInput{
		Period:          period,
		Window:          agg.Window,
		SnapshotAt:      now.UTC(),
		SampleThreshold: agg.Threshold,
		Models:          agg.Models,
		Providers:       agg.Providers,
		Timeseries:      agg.Timeseries,
	})
	if err != nil {
		if ctx.Err() == nil {
			deps.Logger.Warn("rankings: 写入快照失败", "period", period, "error", err)
		}
		return
	}

	deps.Logger.Info("rankings: 快照已刷新",
		"period", period,
		"run_id", runID,
		"models", len(agg.Models),
		"window_start", agg.Window.Start,
		"window_end", agg.Window.End,
		"granularity", period.Granularity(),
	)

	// 快照已提交后才失效缓存：先失效会让读取面在事务提交前回源到旧 run。
	invalidateCache(ctx, deps, period, now.UTC())
}

// releaseLock 用 Lua 做 compare-and-delete。
var releaseLockScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("DEL", KEYS[1])
end
return 0`)

func releaseLock(deps Deps, key, token string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := releaseLockScript.Run(ctx, deps.Redis, []string{key}, token).Err(); err != nil {
		deps.Logger.Warn("rankings: 释放刷新租约失败（TTL 会自动回收）", "key", key, "error", err)
	}
}

// invalidateCache 删除公开响应缓存并广播刷新事件（R-D6-09）。
//
// 写快照成功即代表公开数据已更新；缓存失效失败只影响新鲜度（TTL 兜底），
// 不应把已成功的快照回报为失败。
func invalidateCache(ctx context.Context, deps Deps, period Period, snapshotAt time.Time) {
	cacheCtx := ctx
	var cancel context.CancelFunc
	if cacheCtx.Err() != nil {
		cacheCtx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
	}
	if err := deps.Redis.Del(cacheCtx, CacheKey(period)).Err(); err != nil {
		deps.Logger.Warn("rankings: 删除公开缓存失败", "period", period, "error", err)
	}
	payload, err := json.Marshal(map[string]string{
		"period":      string(period),
		"computed_at": snapshotAt.Format(time.RFC3339Nano),
	})
	if err != nil {
		// 固定结构体的 Marshal 不会失败；保底分支避免把 nil payload 发出去。
		return
	}
	if err := deps.Redis.Publish(cacheCtx, RefreshedChannel, payload).Err(); err != nil {
		deps.Logger.Warn("rankings: 广播刷新事件失败", "period", period, "error", err)
	}
}

// newLockToken 生成租约持有者令牌（跨副本唯一，避免 compare-and-delete 误判）。
func newLockToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
