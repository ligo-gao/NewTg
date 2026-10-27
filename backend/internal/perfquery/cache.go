package perfquery

import (
	"fmt"
	"sync"
	"time"
)

// cacheEntry 是一次成功查询的结果与其绝对过期时刻。
type cacheEntry struct {
	samples []Sample
	expires time.Time
}

// ttlCache 是进程内 TTL 缓存：互斥锁 + 每项 deadline。
//
// 刻意不起后台清理 goroutine：条目数被查询名与时间桶压得很小（21 条 ×
// 活跃窗口），惰性剪枝足够，也避免缓存成为测试里无法回收的 goroutine 源。
type ttlCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	now     func() time.Time
	entries map[string]cacheEntry
}

func newTTLCache(ttl time.Duration, now func() time.Time) *ttlCache {
	return &ttlCache{
		ttl:     ttl,
		now:     now,
		entries: make(map[string]cacheEntry),
	}
}

// cacheKey 的 end 必须是调用方已截到 CacheTTL 桶边界的值。键里带 start/step：
// 同一查询名在不同时间窗或步长下结果不同，只有完全同参才可复用。
func cacheKey(name string, start, end time.Time, step time.Duration) string {
	return fmt.Sprintf("%s|%d|%d|%s", name, start.Unix(), end.Unix(), step)
}

// get 命中时返回样本副本：调用方（overview 组装）可能改写切片，不能让它
// 把缓存变成可变的共享状态。
func (c *ttlCache) get(key string) ([]Sample, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneLocked()
	e, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	return cloneSamples(e.samples), true
}

func (c *ttlCache) put(key string, samples []Sample) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneLocked()
	c.entries[key] = cacheEntry{
		samples: cloneSamples(samples),
		expires: c.now().Add(c.ttl),
	}
}

// pruneLocked 在每次读写时清掉到期条目。map 迭代时 delete 是安全的。
func (c *ttlCache) pruneLocked() {
	now := c.now()
	for key, e := range c.entries {
		if !now.Before(e.expires) {
			delete(c.entries, key)
		}
	}
}

func cloneSamples(in []Sample) []Sample {
	if in == nil {
		return nil
	}
	out := make([]Sample, len(in))
	copy(out, in)
	return out
}
