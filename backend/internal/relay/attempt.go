package relay

import (
	"math/rand/v2"
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
//   - 此处**不做加权随机**：BuildAttemptPlan 只给出确定的候选次序（供计划审计与
//     pin 重排），同优先级内的加权随机由执行层每轮调用 SelectWithinPriority 完成
//     （relay-01 §5.3；见 ExecuteAttempts）。
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
//
// 保留本函数供需要「按固定次序」消费计划的调用方（审计/诊断）使用；执行层的
// 每轮选择走 attemptPriorities + SelectWithinPriority（relay-01 §5.2–§5.3）。
func Next(p *contract.AttemptPlan, attemptIndex int) (contract.ChannelCandidate, bool) {
	if p == nil || attemptIndex < 0 || attemptIndex >= len(p.Candidates) {
		return contract.ChannelCandidate{}, false
	}
	return p.Candidates[attemptIndex], true
}

// selectionUint64N 是加权随机选择的生产随机源。
//
// 用 math/rand/v2 的**全局**源而不是自建 *rand.Rand：全局函数并发安全且自动
// 播种，而 *rand.Rand 被多个并发请求共享时必须额外加锁，锁竞争会出现在每个
// 中继请求的热路径上（relay-01 §5.3 的加权随机每轮尝试都要采样一次）。
// 声明成包级变量是为了给测试留一个替换接缝（见 SelectWithinPriority）。
var selectionUint64N = rand.Uint64N

// attemptPriorities 返回候选列表中去重后的优先级序列（保持首次出现顺序）。
//
// 调用方传入的列表已按 R-P3 排序（优先级降序；pin 候选被提到首位），因此
// 无 pin 时结果就是 relay-01 §6.4 需要的「去重后的降序优先级数组」。有 pin 时
// 首元素是 pin 的优先级，其余仍按降序——pin 是加在选路之上的显式约束，优先级
// 降序在 pin 之后继续成立（relay-01 §5.4 A 步）。
func attemptPriorities(candidates []contract.ChannelCandidate) []int {
	seen := make(map[int]struct{}, len(candidates))
	priorities := make([]int, 0, len(candidates))
	for _, candidate := range candidates {
		if _, ok := seen[candidate.Priority]; ok {
			continue
		}
		seen[candidate.Priority] = struct{}{}
		priorities = append(priorities, candidate.Priority)
	}
	return priorities
}

// candidatesAtPriority 返回 priority 相同的候选，保持输入顺序。
func candidatesAtPriority(candidates []contract.ChannelCandidate, priority int) []contract.ChannelCandidate {
	group := make([]contract.ChannelCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.Priority == priority {
			group = append(group, candidate)
		}
	}
	return group
}

// SelectWithinPriority 在**同一优先级内**按权重做加权随机（relay-01 §5.3，
// 基线-03 §4.2；控制器冻结算法，final-review finding 1）。
//
// 算法与边界（对应基线 §10-3 的甲类修正，绝不除零/panic）：
//   - 单候选直接返回（不采样——避免无意义的随机源消耗，也让 pin 单候选路径确定）；
//   - S = Σw_i == 0（全零或缺失权重）→ 均匀随机，等效每个候选 100/n；
//   - 否则把 [0,S) 上的均匀整数按权重区间切分，落在哪个区间返回哪个候选
//     （即 eff_i = w_i/S 的概率）。
//
// uint64N 是随机源接缝（生产为 selectionUint64N = rand.Uint64N）。用 uint64
// 而不是 int 累加权重：Weight 是 uint，64 位平台上多候选求和不会在 int 上
// 提前溢出；uint64N(0) 永远不会被调用（总权重为 0 时走均匀分支）。
func SelectWithinPriority(candidates []contract.ChannelCandidate, uint64N func(uint64) uint64) contract.ChannelCandidate {
	if len(candidates) == 0 {
		return contract.ChannelCandidate{}
	}
	if len(candidates) == 1 {
		return candidates[0]
	}

	var total uint64
	for _, candidate := range candidates {
		total += uint64(candidate.Weight)
	}
	if total == 0 {
		// 甲类防御：全零权重退化为均匀分布，而不是 rand.Uint64N(0) 的 panic。
		return candidates[uint64N(uint64(len(candidates)))]
	}

	r := uint64N(total)
	for _, candidate := range candidates {
		weight := uint64(candidate.Weight)
		if r < weight {
			return candidate
		}
		r -= weight
	}
	// 算术上不可达（r ∈ [0,total) 且 Σweight == total）；保留末位兜底只为
	// 防御权重求和的极端溢出，绝不让函数返回零值候选。
	return candidates[len(candidates)-1]
}
