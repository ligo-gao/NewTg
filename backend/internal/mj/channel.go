package mj

import (
	"math/rand/v2"
	"strings"

	"newtg/gateway/internal/tasks"
)

// 渠道类型字面量（relay-05 §3.4；基线 ChannelTypeMidjourney=2 /
// ChannelTypeMidjourneyPlus=5）。
//
// 用字符串而不是 DB 的 smallint：领域层只做策略判定，类型比较必须可读且
// 与渠道配置的线值一致；int 映射留在 store 的扫描层，避免两个漂移点。
const (
	ChannelTypeMidjourney     = "midjourney"
	ChannelTypeMidjourneyPlus = "midjourney_plus"
)

// ChannelStatusEnabled 是「启用中」的渠道状态（store 的 status=1）。
//
// 人工禁用（2）与自动禁用（3）都不得被提交选中：前者是管理员显式决定，
// 后者说明上游已判不可用；把任务送进去只会失败并需要退款。
const ChannelStatusEnabled = 1

// ChannelCandidate 是渠道选择所需的最小投影（a2 从渠道仓储读取后传入）。
//
// 领域层不查 DB：把候选集作为入参，选择策略就可在单测里穷举优先级/权重/
// 类型边界，而不依赖真实渠道表。Credentials 刻意不在这里——选择与凭据
// 解密是两条路径，避免领域包持有明文凭据。
type ChannelCandidate struct {
	ID       int64
	Type     string
	Status   int
	Priority int
	Weight   uint

	// SupportedModels 是渠道原生支持的模型；ModelMapping 的键是客户端
	// 别名（store 的选择逻辑同样是「原生支持 或 映射键命中」）。
	SupportedModels []string
	ModelMapping    map[string]string
}

// supportsModel 报告渠道是否可服务该模型。
//
// 空模型一律不匹配：空串会命中「任意渠道」，让动作解析失败的请求落到
// 一个随机渠道上（并可能按错误模型计价），所以必须显式拒绝。
func (c ChannelCandidate) supportsModel(model string) bool {
	if strings.TrimSpace(model) == "" {
		return false
	}
	for _, supported := range c.SupportedModels {
		if supported == model {
			return true
		}
	}
	_, mapped := c.ModelMapping[model]
	return mapped
}

// EligibleChannels 过滤出可服务该动作的 MJ 渠道。
//
// 三条准入条件缺一不可：
//  1. status=1（启用中）；
//  2. 类型是 midjourney / midjourney_plus；Plus-only 端点只允许 plus；
//     其它类型（例如误配了 mj_* 模型的 OpenAI 渠道）绝不参与；
//  3. 渠道原生支持该模型或有同名映射键。
//
// 结果保持输入顺序，便于「同优先级内按渠道 id 稳定加权」。
func EligibleChannels(candidates []ChannelCandidate, model string, plusOnly bool) []ChannelCandidate {
	eligible := make([]ChannelCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.Status != ChannelStatusEnabled {
			continue
		}
		switch candidate.Type {
		case ChannelTypeMidjourney:
			if plusOnly {
				continue
			}
		case ChannelTypeMidjourneyPlus:
			// plus 渠道可以服务普通请求（Plus 是普通能力的超集）。
		default:
			continue
		}
		if !candidate.supportsModel(model) {
			continue
		}
		eligible = append(eligible, candidate)
	}
	return eligible
}

// SelectChannel 在候选集中选出一次提交的目标渠道。
//
// 语义与中继的渠道选择一致（relay-01 §5.3）：先取**最高优先级**（数值大者），
// 再在同优先级内按权重做加权随机；权重全为 0 时退化为均匀分布，而不是
// 除零。uint64N 是随机源接缝（生产传 rand.Uint64N，测试传确定函数），
// nil 时使用默认随机源。
//
// 没有可用渠道返回 ok=false，由 a2 映射为 no_available_channel（503）；
// 绝不返回零值候选让调用方误以为选中了渠道 0。
func SelectChannel(candidates []ChannelCandidate, model string, plusOnly bool, uint64N func(uint64) uint64) (ChannelCandidate, bool) {
	eligible := EligibleChannels(candidates, model, plusOnly)
	if len(eligible) == 0 {
		return ChannelCandidate{}, false
	}
	if uint64N == nil {
		uint64N = rand.Uint64N
	}

	maxPriority := eligible[0].Priority
	for _, candidate := range eligible[1:] {
		if candidate.Priority > maxPriority {
			maxPriority = candidate.Priority
		}
	}
	group := make([]ChannelCandidate, 0, len(eligible))
	for _, candidate := range eligible {
		if candidate.Priority == maxPriority {
			group = append(group, candidate)
		}
	}
	if len(group) == 1 {
		// 单候选不消耗随机源：让「唯一可用渠道」的结果可复现。
		return group[0], true
	}

	var total uint64
	for _, candidate := range group {
		total += uint64(candidate.Weight)
	}
	if total == 0 {
		return group[uint64N(uint64(len(group)))], true
	}
	r := uint64N(total)
	for _, candidate := range group {
		weight := uint64(candidate.Weight)
		if r < weight {
			return candidate, true
		}
		r -= weight
	}
	// 算术上不可达（r ∈ [0,total)）；保留末位兜底只为防御权重求和溢出，
	// 绝不让函数返回零值候选。
	return group[len(group)-1], true
}

// OriginTask 是动作类任务的原任务快照（a2 读取 tasks 行后传入）。
type OriginTask struct {
	// Exists 区分「任务不存在」与「存在但渠道/状态不满足」：两者都拒绝，
	// 但错误描述不同，便于客户端区分「id 错了」与「任务已不可操作」。
	Exists bool
	// ChannelID 是原任务执行渠道；<=0 视为缺失。
	ChannelID int64
	// ChannelEnabled 是该渠道当前是否启用（status=1）。
	ChannelEnabled bool
	// State 是原任务状态（用于 mj.action_check_success_enabled 校验）。
	State tasks.State
}

// LockPolicy 是渠道锁定校验的策略开关。
type LockPolicy struct {
	// RequireSuccess 对应 mj.action_check_success_enabled：开启时要求原任务
	// 已是 success。默认关闭（设计：开关关闭时不校验）。
	RequireSuccess bool
}

// ResolveLock 执行动作类任务的渠道三校验（relay-05 §4.1）。
//
// 1. 原任务存在，否则 get_channel_info_failed；
// 2. 原渠道启用，否则「该任务所属渠道已被禁用」；
// 3. 开关开启时原任务必须 SUCCESS，否则 action_task_not_success。
//
// 任何一条不满足都返回错误，调用方必须在 PreConsume 之前拒绝——否则会
// 出现「已扣费但任务无法提交」的资金损失。成功时返回锁定渠道 id。
func ResolveLock(origin OriginTask, policy LockPolicy) (int64, *Error) {
	if !origin.Exists || origin.ChannelID <= 0 {
		return 0, BadRequest(DescriptionOriginTaskMissing)
	}
	if !origin.ChannelEnabled {
		return 0, BadRequest(DescriptionChannelDisabled)
	}
	if policy.RequireSuccess && origin.State != tasks.StateSuccess {
		return 0, BadRequest(DescriptionTaskNotSuccess)
	}
	return origin.ChannelID, nil
}
