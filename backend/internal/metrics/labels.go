package metrics

import "sync"

// guardLimit 是「允许但需控制」标签（model/channel_id/group）的唯一值上限。
//
// 冻结口径 platform-05 §3.3（业主裁决 U-05-5）：唯一值超过 5000 后归并为
// other。护栏放在**采集侧**（写标签之前），因为一旦把第 5001 个值交给
// prometheus client，child 会永久留在内存与 /metrics 输出里，事后无法回收。
const guardLimit = 5000

// maxLabelValueLen 是动态标签值的硬长度上限。
//
// 超长值一律归并为 other：否则一个超大 model 字符串（用户可控输入）会先
// 被存入 seen 集合再写进标签，既不安全也无界。128 字节足以覆盖现实模型名。
const maxLabelValueLen = 128

// labelGuard 用「已见集合 + 上限」把动态标签的基数钳在固定范围。
//
// 为什么不用 Prometheus 的 relabel：relabel 发生在抓取端/TSDB，服务进程仍
// 会为每个新值创建 child（内存增长、/metrics 膨胀），无法阻止指标本身成为
// 故障源。此处按维度各自持有 Guard，互不影响。
type labelGuard struct {
	mu    sync.Mutex
	limit int
	seen  map[string]struct{}
}

func newLabelGuard(limit int) *labelGuard {
	if limit <= 0 {
		limit = guardLimit
	}
	return &labelGuard{limit: limit, seen: make(map[string]struct{})}
}

// Value 返回可用于标签的值：
//   - 空串 → unknown（避免空标签与「未设置」混淆）；
//   - 超长 → other（用户输入护栏，见 maxLabelValueLen）；
//   - 已见/未满 → 原值；未满时登记；
//   - 已满且新值 → other（不登记，other 不再占用配额）。
func (g *labelGuard) Value(v string) string {
	if v == "" {
		return "unknown"
	}
	if len(v) > maxLabelValueLen {
		return "other"
	}
	if v == "other" || v == "unknown" {
		return v
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.seen[v]; ok {
		return v
	}
	if len(g.seen) >= g.limit {
		return "other"
	}
	g.seen[v] = struct{}{}
	return v
}

// size 返回当前已登记的唯一条数（测试与护栏断言用）。
func (g *labelGuard) size() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.seen)
}
