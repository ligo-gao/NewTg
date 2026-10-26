package billing

// 客户折扣的预扣侧接缝与冻结逻辑（spec §5.2 / §5.4）。
//
// 本文件只负责「预扣时解析一次、冻结到会话」：解析结果写进 Session.discount，
// 由 PreConsume 在最小计费保底之后、选择资金来源之前应用一次。结算侧的
// 原价/实收口径（ledgerDiscount 的消费方）属于后续任务，本文件只提供冻结
// 值的读取入口。
//
// 三条不可动摇的口径：
//   - 未命中、装配缺失、查询失败一律按原价（零值 Discount，Target 为空）；
//   - 查询失败绝不向上返回错误——折扣是让利，不是请求的前置条件；
//   - 全免只能由「Target 非空且 Rate == 0」表达，零值 Rate 不是全免。

import (
	"context"
	"log/slog"
	"math"
	"time"

	"newtg/gateway/internal/billing/pricing"
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/store"
)

// DiscountStore 是折扣规则解析缝（spec §5.2）。*store.CustomerDiscountRepo 是
// 生产实现；单测注入内存 fake。
//
// models 是 pricing.ResolveBillingModelCandidates 展开后的候选名；at 是会话
// 冻结的 requestTime（有效期判定不得用 time.Now()）。
type DiscountStore interface {
	Resolve(ctx context.Context, userID int64, models []string, at time.Time) (store.CustomerDiscount, bool, error)
}

// SetDiscountStore 装配折扣解析源（spec §5.2）。
//
// 与 SetRequestPricingStore 同一约定：装配期调用一次，构造后不再修改——
// 请求路径的读取不加锁，依赖「装配后只读」这一前提。未装配时预扣一律原价。
func (e *Engine) SetDiscountStore(s DiscountStore) {
	e.discountStore = s
}

// resolveDiscountLocked 解析并冻结本请求的折扣（调用方持有 s.mu）。
//
// 每请求只解析一次：discountResolved 一旦置位就直接返回，保证预扣与
// （后续任务的）结算读到同一份冻结值，也不会因为重试而叠加。
func (s *Session) resolveDiscountLocked(ctx context.Context) {
	if s.discountResolved {
		return
	}
	defer func() { s.discountResolved = true }()

	if s.engine == nil || s.engine.discountStore == nil || s.userID == 0 || s.model == "" {
		// 装配缺失/身份或模型未知：没有可解析的上下文，按原价处理。
		return
	}
	models := pricing.ResolveBillingModelCandidates(s.model)
	d, found, err := s.engine.discountStore.Resolve(ctx, s.userID, models, s.requestTime)
	if err != nil {
		// 折扣是让利：查询失败不得让请求失败，也不得把零值当全免。
		slog.Warn("billing: resolve customer discount failed; billing at list price",
			"request_id", s.requestID, "model", s.model, "user_id", s.userID, "error", err)
		return
	}
	if !found {
		return
	}
	s.discount = contract.Discount{Rate: d.Rate(), Target: d.Target()}
}

// applyFrozenDiscount 直接写入已冻结的折扣值并标记已解析。
//
// 供异步/handoff 重建会话时复用预扣阶段冻结的折扣：重建后的会话没有
// 解析上下文（也没有必要重新查询），必须原样复用而不是按当前规则重算，
// 否则同一请求的预扣与结算可能命中不同折扣。
func (s *Session) applyFrozenDiscount(rate float64, target string) {
	s.discount = contract.Discount{Rate: rate, Target: target}
	s.discountResolved = true
}

// discountApplies 判断冻结折扣是否实际生效。
//
// 判据是 Target 非空且 Rate < 1：零值 Rate 配合空 Target 表示「未命中」，
// 绝不是全免；NaN 无法比较，显式视为不适用（fail-closed 到原价）。
func (s *Session) discountApplies() bool {
	return s.discount.Target != "" && !math.IsNaN(s.discount.Rate) && s.discount.Rate < 1
}

// discountFacts 推导结算日志需要的「原价成本 + 生效折扣」（R1）。
//
// 返回零值三元组表示「没有可记录的折扣」：未命中、rate >= 1、NaN 都不写。
// 全免（rate == 0）只返回目标、不写原价：0 是让利结果而不是原价事实，
// 用 0 反推原价会得到无穷大或 0，都不真实。
func discountFacts(d contract.Discount, rawCost, netCost int64, rawKnown bool) (int64, float64, string) {
	if d.Target == "" || math.IsNaN(d.Rate) || d.Rate >= 1 {
		return 0, 0, ""
	}
	if d.Rate == 0 {
		return 0, 0, d.Target
	}
	original := int64(0)
	if rawKnown {
		original = rawCost
	} else if netCost > 0 {
		// 没有原价事实时按实收/折扣率反推（四舍五入到整数积分）。
		original = int64(math.Round(float64(netCost) / d.Rate))
	}
	return original, d.Rate, d.Target
}

// ledgerDiscount 是本会话冻结折扣的 discountFacts 读取入口（调用方持有所需
// 状态；冻结后只读，不额外加锁）。
func (s *Session) ledgerDiscount(rawCost, netCost int64, rawKnown bool) (int64, float64, string) {
	return discountFacts(s.discount, rawCost, netCost, rawKnown)
}
