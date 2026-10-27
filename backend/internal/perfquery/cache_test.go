package perfquery

import (
	"testing"
	"time"
)

func TestTTLCacheHitAndExpire(t *testing.T) {
	now := time.Unix(1761000000, 0)
	cache := newTTLCache(15*time.Second, func() time.Time { return now })

	got := []Sample{{At: now, Value: 1.25}}
	cache.put("k", got)

	if samples, ok := cache.get("k"); !ok {
		t.Fatal("缓存应立即命中")
	} else if len(samples) != 1 || samples[0].Value != 1.25 {
		t.Fatalf("缓存内容 = %v", samples)
	}

	now = now.Add(14 * time.Second)
	if _, ok := cache.get("k"); !ok {
		t.Fatal("TTL 内应命中")
	}

	now = now.Add(time.Second) // 恰好到达 TTL 边界
	if _, ok := cache.get("k"); ok {
		t.Fatal("TTL 边界后应未命中")
	}
	if len(cache.entries) != 0 {
		t.Fatalf("过期条目应被惰性剪枝，剩余 %d", len(cache.entries))
	}
}

func TestTTLCacheReturnsCopy(t *testing.T) {
	now := time.Unix(1761000000, 0)
	cache := newTTLCache(time.Minute, func() time.Time { return now })
	cache.put("k", []Sample{{At: now, Value: 7}})

	first, ok := cache.get("k")
	if !ok {
		t.Fatal("未命中")
	}
	first[0].Value = 99

	second, ok := cache.get("k")
	if !ok {
		t.Fatal("未命中")
	}
	if second[0].Value != 7 {
		t.Fatalf("调用方改写返回值污染了缓存: %v", second[0].Value)
	}
}

func TestTTLCacheConcurrentAccess(t *testing.T) {
	cache := newTTLCache(time.Minute, time.Now)
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 100; j++ {
				cache.put("k", []Sample{{Value: 1}})
				cache.get("k")
			}
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
}

func TestCacheKeyDistinguishesInputs(t *testing.T) {
	end := time.Unix(1761000180, 0)
	start := end.Add(-time.Hour)
	base := cacheKey(NameGatewayRPS, start, end, time.Minute)

	if got := cacheKey(NameGatewayRPS, start, end, time.Minute); got != base {
		t.Fatalf("相同输入键不同: %q vs %q", got, base)
	}
	cases := map[string]string{
		"name":  cacheKey(NameGatewayP95, start, end, time.Minute),
		"start": cacheKey(NameGatewayRPS, start.Add(time.Minute), end, time.Minute),
		"end":   cacheKey(NameGatewayRPS, start, end.Add(time.Minute), time.Minute),
		"step":  cacheKey(NameGatewayRPS, start, end, 2*time.Minute),
	}
	for field, key := range cases {
		if key == base {
			t.Errorf("%s 不同的查询生成了相同缓存键", field)
		}
	}
}

// TestCacheKeyUsesTruncatedEnd 演练 QueryRange 的键口径：同一 TTL 桶内的
// 不同 end 视为同一次查询，跨桶则分开。
func TestCacheKeyUsesTruncatedEnd(t *testing.T) {
	ttl := 15 * time.Second
	start := time.Unix(1761000000, 0)
	endA := time.Unix(1761000005, 0).Truncate(ttl)
	endB := time.Unix(1761000010, 0).Truncate(ttl)
	endC := time.Unix(1761000015, 0).Truncate(ttl)

	if cacheKey(NameGatewayRPS, start, endA, time.Minute) != cacheKey(NameGatewayRPS, start, endB, time.Minute) {
		t.Error("同一桶内的 end 应共享缓存键")
	}
	if cacheKey(NameGatewayRPS, start, endA, time.Minute) == cacheKey(NameGatewayRPS, start, endC, time.Minute) {
		t.Error("跨桶的 end 不应共享缓存键")
	}
}
