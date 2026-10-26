package billing

import (
	"math"
	"sort"
	"strings"

	"newtg/gateway/internal/billing/pricing"
	"newtg/gateway/internal/contract"
)

// DiscountTarget 是折扣匹配的目标上下文（M3 Task 6）。
//
// 【为什么定义在本包而不是 contract】冻结契约只给了 contract.Discount
// {Rate, Target}，没有「折扣目标」类型（B0 §8 只冻结语义：不叠加、取最低、
// 全免靠 Target 非空判定）。Task 6 简报允许在本包定义并在报告中说明。
//
// 三个字段是**精确等值匹配**的候选值（非通配、非前缀）：
//   - Model   请求的模型名（折扣规则最具体的维度）
//   - Group   生效分组名
//   - Channel 渠道 id 的文本形式（契约与 DB 中渠道标识都是数值，这里用
//     字符串承载是为了让 Target 的编码对所有维度保持同一形状）
//
// 【Target 编码（本包冻结）】contract.Discount.Target 必须带作用域前缀：
//
//	"model:<模型名>"、"group:<分组名>"、"channel:<渠道 id>"、"global"
//
// 无前缀或不认识的前缀一律视为**未命中**（fail-closed）：把未知标识当成
// 任意作用域可能把别人的折扣应用到本请求上，宁可全额计费。
//
// 【R-57】`user:<id>` 作用域与有效期窗口（valid_from/valid_until）由**配置域**
// 在调用 ResolveDiscount 之前过滤：本函数只收到「已经按用户、时间、启用
// 状态筛过」的候选规则，因此不再做用户匹配。`user:` 前缀在这里**保持
// fail-closed**（未命中），绝不把它当通配符——否则任一用户的规则都会命中
// 所有人。配置层在预扣时解析一次并冻结到会话；结算侧复用同一份冻结折扣，
// 由 SettlementOutcome 携带 original_cost / applied_discount /
// discount_target 写入 usage_logs.other——折扣事实来自结算结果，不依赖
// RelayContext 字段。
type DiscountTarget struct {
	Model   string
	Group   string
	Channel string
}

// 折扣作用域（Target 前缀）。
const (
	discountScopeModel   = "model"
	discountScopeGroup   = "group"
	discountScopeChannel = "channel"
	discountScopeGlobal  = "global"
)

// 作用域具体度：数值越大越具体。平级（同 rate）时「更具体者胜」（C-12/D15）。
// 优先级冻结为 模型 > 分组 > 渠道 > 全局：模型规则只影响一个模型，分组规则
// 影响一组用户，渠道规则影响一条上游链路，全局规则影响所有流量。
const (
	discountRankGlobal  = iota // 0
	discountRankChannel        // 1
	discountRankGroup          // 2
	discountRankModel          // 3
)

// noDiscount 是「未命中任何规则」的规范返回值。
//
// billing-04 §2.1：快照为空 / 模型名为空时短路为 (rate=1, target="")。
// Target 为空是「未命中」的唯一判据（ApplyDiscount 依赖它区分
// 「未命中」与「命中 rate=0 的合法全免」）。
func noDiscount() contract.Discount { return contract.Discount{Rate: 1} }

// ResolveDiscount 从候选折扣中解析唯一生效折扣（billing-00 §8、billing-04 §2.2）。
//
// 规则（顺序即语义）：
//  1. 只保留**作用于本请求**的规则（Model/Group/Channel 精确等值，global 恒命中）；
//  2. 不叠加：rate 取**最低**（折扣力度最大者）；渠道规则声明的 rate 比全局
//     更紧（更低）时自然胜出，这就是 C-14/D14「更紧的渠道上限优先」在折扣
//     维度上的落点；
//  3. 平级（同 rate）：更具体者胜（C-12/D15），模型 > 分组 > 渠道 > 全局；
//  4. 同作用域同 rate：按目标标识升序取胜者，保证与快照遍历顺序无关
//     （billing-04 M-04-1 的确定性修正）。
//
// NaN/±Inf 的 rate 不参与比较：它们无法形成确定的全序，且让一个坏配置
// 静默变成「最低折扣」会全站白送。跳过即按剩余规则计费（安全方向）。
func ResolveDiscount(discounts []contract.Discount, target DiscountTarget) contract.Discount {
	best := noDiscount()
	bestRank := -1
	bestID := ""
	matched := false

	for _, d := range discounts {
		scope, id, ok := parseDiscountTarget(d.Target)
		if !ok {
			continue
		}
		if !discountApplies(scope, id, target) {
			continue
		}
		if math.IsNaN(d.Rate) || math.IsInf(d.Rate, 0) {
			continue
		}

		rank := discountScopeRank(scope)
		switch {
		case !matched:
		case d.Rate < best.Rate:
		case d.Rate == best.Rate && rank > bestRank:
		case d.Rate == best.Rate && rank == bestRank && id < bestID:
		default:
			continue
		}

		best = d
		bestRank = rank
		bestID = id
		matched = true
	}

	if !matched {
		return noDiscount()
	}
	return best
}

// parseDiscountTarget 解析 "scope:id" / "global" 形式的目标标识。
//
// 空串、缺 id 的带前缀串、未知前缀都返回 ok=false（未命中，fail-closed）。
// 作用域前缀大小写不敏感；标识本体保持原样（模型/分组名大小写敏感）。
func parseDiscountTarget(target string) (scope, id string, ok bool) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", "", false
	}
	if strings.EqualFold(target, discountScopeGlobal) {
		return discountScopeGlobal, "", true
	}
	prefix, rest, found := strings.Cut(target, ":")
	if !found {
		return "", "", false
	}
	scope = strings.ToLower(strings.TrimSpace(prefix))
	id = strings.TrimSpace(rest)
	if id == "" {
		return "", "", false
	}
	switch scope {
	case discountScopeModel, discountScopeGroup, discountScopeChannel:
		return scope, id, true
	default:
		return "", "", false
	}
}

// discountApplies 判断一条规则是否作用于目标请求。
func discountApplies(scope, id string, target DiscountTarget) bool {
	switch scope {
	case discountScopeGlobal:
		return true
	case discountScopeModel:
		return target.Model != "" && id == target.Model
	case discountScopeGroup:
		return target.Group != "" && id == target.Group
	case discountScopeChannel:
		return target.Channel != "" && id == target.Channel
	default:
		return false
	}
}

// discountScopeRank 返回作用域具体度。
func discountScopeRank(scope string) int {
	switch scope {
	case discountScopeModel:
		return discountRankModel
	case discountScopeGroup:
		return discountRankGroup
	case discountScopeChannel:
		return discountRankChannel
	default:
		return discountRankGlobal
	}
}

// ApplyDiscount 把折扣率作用到配额上（billing-00 §8、billing-04 §2.3）。
//
// 【顺序冻结】全免判定先于最小计费保底，顺序不可颠倒：
//   - 未命中（Target=="")：原样返回，等价于 rate=1。这是全免判据的关键——
//     rate 的零值也是 0，只看 rate 会把「未命中」误判成全免；
//   - quota<=0 或 rate>=1：原样返回；
//   - rate<0：归零（非法负率按完全让利处理，与基线 ApplyDiscount 一致）；
//   - rate==0：返回 0，且**不被**最小计费保底抬升（命中全免规则就是免费）；
//   - 0<rate<1：按 QuotaFromFloat 向零截断；结果恰为 0 时置 1（正配额
//     折为 0 → 最小计费保底，契约 §7.5）。
//
// 换算只经 pricing.QuotaFromFloat（billing-07 §2.4 禁止裸 float→int）。
// 有效域内 quota ≤ MaxQuota 且 rate<1，乘积不会越 int32 界，因此本签名
// （冻结为无 *QuotaClamp 返回值）不会丢失审计标记；NaN 单独按最小计费
// 1 处理（fail-closed，绝不把坏配置变成免费）。
func ApplyDiscount(quota int64, d contract.Discount) int64 {
	if d.Target == "" {
		return quota
	}
	if quota <= 0 {
		return quota
	}
	switch {
	case math.IsNaN(d.Rate):
		// rate 不是有限实数属于配置错误；按最小计费处理，不白送也不原价。
		return 1
	case d.Rate >= 1:
		return quota
	case d.Rate < 0:
		return 0
	case d.Rate == 0:
		// 全免：返回 0，且先于下面的最小计费保底判定。
		return 0
	}

	scaled := pricing.QuotaFromFloat(float64(quota) * d.Rate)
	if scaled <= 0 {
		return 1
	}
	return scaled
}

// SortDiscounts 返回按（作用域具体度降序、目标标识升序）稳定排序的副本。
//
// 【用途】配置快照构建时调用，使平级（同 rate）胜者在**任何遍历顺序**下
// 都确定（billing-04 M-04-1 / C-12 D15）。ResolveDiscount 自身已按
// 「最低 rate + 具体度 + 标识升序」选出唯一胜者，不依赖输入顺序；本函数
// 供需要稳定快照顺序的调用方（配置域）复用同一口径。未知作用域的规则
// 排在最后（它们不会生效）。
func SortDiscounts(discounts []contract.Discount) []contract.Discount {
	out := make([]contract.Discount, len(discounts))
	copy(out, discounts)
	sort.SliceStable(out, func(i, j int) bool {
		si, idi, oki := parseDiscountTarget(out[i].Target)
		sj, idj, okj := parseDiscountTarget(out[j].Target)
		ri, rj := -1, -1
		if oki {
			ri = discountScopeRank(si)
		}
		if okj {
			rj = discountScopeRank(sj)
		}
		if ri != rj {
			return ri > rj
		}
		if idi != idj {
			return idi < idj
		}
		return out[i].Target < out[j].Target
	})
	return out
}
