package relay

import (
	"sort"

	"newtg/gateway/internal/contract"
)

const (
	// defaultRetries 是 relay.max_retries 缺省时的重试次数（不含首试）。
	// 取值由 GC-5 / C-9/D11 冻结为 3。
	defaultRetries = 3
	// minRetries / maxRetries 是重试次数的闭区间。
	// 下限 1 保证至少有一次渠道转移机会；上限 5 防止上游整体故障时
	// 单个请求把连接与并发槽拖成无界占用（GC-5 / C-9/D11）。
	minRetries = 1
	maxRetries = 5
)

// ClampRetries 把配置的重试次数钳制到 [1,5]。
//
// 为什么是钳制而不是报错：relay.max_retries 是运维可调的故障转移预算，
// 配置越界（0、负数、超大值）不应让请求直接失败，而应落到安全区间——
// 下限 1 避免「0 次重试」把可恢复的上游瞬断变成用户可见错误，
// 上限 5 避免重试预算无界放大（GC-5 / C-9/D11）。
func ClampRetries(n int) int {
	if n < minRetries {
		return minRetries
	}
	if n > maxRetries {
		return maxRetries
	}
	return n
}

// BuildAttemptPlan 用默认重试次数构建尝试计划。
func BuildAttemptPlan(rc *contract.RelayContext, candidates []contract.ChannelCandidate) *contract.AttemptPlan {
	return BuildAttemptPlanWithRetries(rc, candidates, defaultRetries)
}

// BuildAttemptPlanWithRetries 构建一次请求的渠道尝试计划：分组过滤 → 排序 → 按渠道去重。
//
// 排序口径（控制器裁决 R-P3，覆盖任务书字面的「priority 升」）：
//   - priority 降序：数值越大越优先；contract.AttemptPlan.Candidates 的注释已冻结
//     「按优先级降序」，DB 索引也是 (status, priority DESC)。
//   - 同 priority 内 weight 降序，且使用**稳定**排序：排序键完全相同的候选保持
//     输入顺序，保证同一份索引数据每次构建出的计划一致（可复现的重试顺序）。
//   - 此处**不做加权随机**：relay-01 §5.3 的同优先级加权随机是「在目标优先级内挑
//     一个渠道」的选择动作，属于路由/选择层；AttemptPlan 只给出确定的候选次序，
//     供重试索引（relay-01 §6.4）稳定映射。
//
// 去重口径：按 ChannelID 保留排序后的第一次出现。排序保证高 priority/weight 的
// 条目先出现，因此留下的是排序键最优的那条；同一渠道在一次请求内只应被尝试一次，
// 否则重复候选会白白消耗重试预算。
//
// 分组口径：rc.UsingGroup 非空时，只保留 Group 为空（组无关渠道）或与 UsingGroup
// 相同的候选——分组是权限与计费边界（relay-01 §5.5），重试不得越过分组。
// rc 为 nil 时不做分组过滤（仅供内部/测试直调用，生产路径应由 Task 1 保证 rc 非 nil）。
//
// 输入切片只读：候选常直接来自进程内路由索引缓存（relay-01 §5.1），原地排序会污染
// 其他并发请求看到的顺序，故先复制再操作。
func BuildAttemptPlanWithRetries(rc *contract.RelayContext, candidates []contract.ChannelCandidate, retries int) *contract.AttemptPlan {
	usingGroup := ""
	if rc != nil {
		usingGroup = rc.UsingGroup
	}

	// 复制 + 分组过滤。空 Group 表示渠道与分组无关，任何分组都可用。
	filtered := make([]contract.ChannelCandidate, 0, len(candidates))
	for _, c := range candidates {
		if usingGroup != "" && c.Group != "" && c.Group != usingGroup {
			continue
		}
		filtered = append(filtered, c)
	}

	// R-P3：priority 降序 → weight 降序；同键稳定。
	sort.SliceStable(filtered, func(i, j int) bool {
		if filtered[i].Priority != filtered[j].Priority {
			return filtered[i].Priority > filtered[j].Priority
		}
		return filtered[i].Weight > filtered[j].Weight
	})

	// 去重：排序后的第一次出现即最优排序键。
	kept := make([]contract.ChannelCandidate, 0, len(filtered))
	seen := make(map[int]struct{}, len(filtered))
	for _, c := range filtered {
		if _, dup := seen[c.ChannelID]; dup {
			continue
		}
		seen[c.ChannelID] = struct{}{}
		kept = append(kept, c)
	}

	// CrossGroup 判据（控制器裁决）：仅当存活候选中存在「非空且不同于 UsingGroup」
	// 的分组时才置 true。指定 UsingGroup 时上面的过滤已剔除异组候选，故恒为 false；
	// 未指定 UsingGroup 时计划天然可能跨组，标记为事实供路由层判断。
	// M2 不做 auto 分组轮换（属 M3），该字段在 M2 不改变重试行为。
	crossGroup := false
	for _, c := range kept {
		if c.Group != "" && c.Group != usingGroup {
			crossGroup = true
			break
		}
	}

	return &contract.AttemptPlan{
		Candidates: kept,
		// relay-01 §6.1：真正尝试次数 = RetryTimes + 1（含首试）。
		MaxAttempts: ClampRetries(retries) + 1,
		CrossGroup:  crossGroup,
	}
}

// Next 返回计划中第 attemptIndex 个候选（已按 R-P3 排序）。索引由路由层维护并递增。
//
// 越界（含负数、空计划、nil 计划）返回零值与 false，**不做隐式钳制**：
// relay-01 §6.4 的「越界取最低优先级」是路由层把 priority 重试索引映射到
// 去重后优先级数组的规则（target ← priorities[min(retry, len-1)]），作用对象是
// **优先级**而非候选序号。若在这里静默钳制到最后一个候选，会把「重试预算耗尽」
// 伪装成「仍有候选可试」，调用方就无法区分两种失败路径（§6.5）。
func Next(p *contract.AttemptPlan, attemptIndex int) (contract.ChannelCandidate, bool) {
	if p == nil || attemptIndex < 0 || attemptIndex >= len(p.Candidates) {
		return contract.ChannelCandidate{}, false
	}
	return p.Candidates[attemptIndex], true
}
