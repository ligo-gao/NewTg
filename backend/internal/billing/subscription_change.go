package billing

// 订阅升级/降级（S2；billing-08 §3.6、任务书 P-10/P-11/P-12、R-23/R-24/R-25）。
//
// 设计要点：
//   - **不新建实例**：升降级把同一 subscriptions 行迁移到新套餐（plan_id 与
//     全部快照列改写），id 与 start_at 不变（P-12）。理由与续费方案甲相同：
//     会话在 PreConsume 时冻结 subscriptionId，换行会让在飞请求的结算落到
//     另一个实例上；同时「已用量」是这条实例的账，换行等于凭空核销。
//   - **按剩余价值折算**（P-11）：剩余价值 = round(price_paid_amount ×
//     剩余秒 / 周期总秒)；应补 = max(0, 新套餐价 − 剩余价值)。升级补差价即
//     立即生效；降级不退款，差额按新套餐单价折成**时长**延长 end_at。
//   - **同币种、同周期**（P-10）：数据模型里套餐只有 period_days（没有
//     month/year 单位列），因此「同一周期单位」落为「相同 period_days」——
//     不同天数的周期之间没有同一个「周期总秒」分母，折算公式不成立。
//     币种不同同理（金额不可加），一律 422 subscription_cycle_mismatch。
//   - **支付通道复用 S3**：应补 > 0 时落 `orders(biz_kind='change',
//     biz_ref_id=subscriptions.id)`，渠道支付单复用 topup.go 的
//     startProviderPayment，结算走 settleOrder 的 change 分支；应补 = 0 时
//     同一事务内直接创建 succeeded 订单并改写实例（与购买/续费的 0 元路径同构）。
//
// 幂等：orders.idempotency_key UNIQUE（重复 POST 返回同一订单）；结算只由
// 「订单 pending→succeeded」这一次转换触发，重复回调不会二次改写实例。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"

	"newtg/gateway/internal/store"
)

var (
	// ErrChangeSubscriptionNotFound：升降级目标实例不存在，或不属于当前用户。
	// 两者统一 404：不能因为「存在但属于别人」暴露 ID 存在性。
	ErrChangeSubscriptionNotFound = errors.New("billing: subscription to change is not found")
	// ErrSubscriptionNotChangeable：实例已不在 active（cancelled 终态，或并发
	// 进入终态/到期），不能升降级（409）。
	ErrSubscriptionNotChangeable = errors.New("billing: subscription cannot be changed")
	// ErrSubscriptionCycleMismatch：新旧套餐币种或周期天数不同（P-10）。
	// 422：折算公式需要同一个周期总秒与同一币种，跨周期/跨币种无法定义。
	ErrSubscriptionCycleMismatch = errors.New("billing: subscription plans use different currency or cycle")
	// ErrChangePlanUnchanged：目标套餐就是当前套餐。重复提交同一请求必须在
	// 校验阶段被拒（409），而不是「再折算一次」把用户额度反复重算。
	ErrChangePlanUnchanged = errors.New("billing: target plan is already the current plan")
	// ErrChangeSnapshot：升降级订单缺少可用快照，或快照与订单/实例不一致。
	// 正常写路径不会出现；出现时保持 pending，绝不按当前套餐臆测合同条件。
	ErrChangeSnapshot = errors.New("billing: subscription change order snapshot is missing or invalid")
	// ErrChangeOrderChanged：结算时实例已不在下单时的旧套餐上（并发升降级/
	// 续费/取消）。订单保持 pending 等人工核对，绝不叠加到别人的合同上。
	ErrChangeOrderChanged = errors.New("billing: subscription changed since the change order was created")
)

// 一天的秒数。周期总秒按 period_days × 86400 计算（不是 end_at − start_at）：
// 降级延长过的实例 end_at 会超出原始周期，用「日历跨度」做分母会让同一套餐
// 在不同实例上得到不同的折算单价。
const subscriptionDaySeconds = int64(24 * 60 * 60)

// ChangePlanInput 是 POST /api/subscriptions/{id}/change 的服务入参。
//
// Provider 与续费同口径：只接受 stripe/alipay/epay（wallet 是余额购买专属，
// 这里没有对应的扣款分支）。应补 = 0 时渠道不参与履约，但订单行仍需要
// provider 判别列（orders_biz_kind_chk 之外，provider 是 NOT NULL 的受控词表）。
type ChangePlanInput struct {
	UserID         int64
	SubscriptionID int64
	NewPlanID      int64
	Provider       string
	IdempotencyKey string
}

// ChangePlanResult 是升降级的服务出参。
//
// 0 元路径（含降级折算）Order 为已 succeeded 的订单，NewEndAt 是实际写入的
// end_at；付费路径先返回 pending 订单与支付载荷，NewEndAt 是下单瞬间的折算
// 结果（真实写入时刻在支付成功时重新求值，见 settleSubscriptionChange）。
type ChangePlanResult struct {
	OrderID      int64
	Order        *store.Order
	Replayed     bool
	ClientSecret string
	QRCode       string
	PayURL       string
	FormFields   map[string]string
	// PriceDelta 是本次应补差价（分，≥0）；CreditsDelta 是新旧周期总额度之差；
	// EventKind 是 upgraded / downgraded。
	PriceDelta   int64
	CreditsDelta int64
	EventKind    string
	NewEndAt     time.Time
	// FromPlanID / NewPlanID 供 handler 回显（不落订单行的普通列）。
	FromPlanID int64
	NewPlanID  int64
}

// subscriptionChangeSnapshot 是 change 订单的冻结快照，落在
// orders.channel_metadata 的 `change` 命名空间里（与 `subscription` /
// `renewal` 并列，互不干扰；共享 jsonb 列的任何写方都只能追加自己的键）。
//
// 【为什么冻结折算结果】应补差价与折算时长在下单瞬间就写进订单：结算到达时
// 套餐可能已被改价/停用，用户已付款的合同条件不得随之变化（R-D5-03）。
type subscriptionChangeSnapshot struct {
	SubscriptionID      int64           `json:"subscription_id"`
	FromPlanID          int64           `json:"from_plan_id"`
	ToPlanID            int64           `json:"to_plan_id"`
	PlanTitle           string          `json:"plan_title"`
	PeriodDays          int32           `json:"period_days"`
	GrantCredits        int64           `json:"grant_credits"`
	ResetOnRenew        bool            `json:"reset_on_renew"`
	AllowWalletOverflow bool            `json:"allow_wallet_overflow"`
	ModelScope          json.RawMessage `json:"model_scope,omitempty"`
	ConcurrencyLimit    *int32          `json:"concurrency_limit,omitempty"`
	GraceDays           int32           `json:"grace_days"`
	Currency            string          `json:"currency"`
	PriceAmount         int64           `json:"price_amount"`
	// PriceDelta 是应补差价（= 订单金额）；RemainingValue 是折算用的旧套餐
	// 剩余价值（仅供排障，不参与任何后续计算）。
	PriceDelta     int64 `json:"price_delta"`
	RemainingValue int64 `json:"remaining_value"`
	// DurationSeconds 是折算后的新周期时长（秒），结算时以「支付成功时刻 +
	// DurationSeconds」求 end_at——等待支付的时间不应计入用户周期。
	DurationSeconds int64     `json:"duration_seconds"`
	CreditsDelta    int64     `json:"credits_delta"`
	EventKind       string    `json:"event_kind"`
	EndAt           time.Time `json:"end_at"`
}

// subscriptionChangeMetadata 是 orders.channel_metadata 中的升降级命名空间。
type subscriptionChangeMetadata struct {
	Change *subscriptionChangeSnapshot `json:"change"`
}

// subscriptionRemainingValue 计算实例在 now 时刻的剩余价值（分，≥0）。
//
// 公式（P-11 冻结）：剩余价值 = round(price_paid_amount × 剩余秒 / 周期总秒)。
//   - 剩余秒向下取整到秒（P-11 的取整要求）：end_at 已过则为 0，绝不出现负数；
//   - round 是四舍五入到整数分（decimal.Round(0) 半值远离零；被乘除的数都是
//     非负数，等价于半值向上）；
//   - 分母用 period_days × 86400 而不是 end_at − start_at：降级延长过的实例
//     日历跨度更长，用跨度做分母会让同一套餐的单价随实例历史漂移。
func subscriptionRemainingValue(pricePaid int64, periodDays int32, endAt, now time.Time) int64 {
	if pricePaid <= 0 || periodDays < 1 {
		return 0
	}
	remainingSeconds := int64(endAt.Sub(now) / time.Second)
	if remainingSeconds <= 0 {
		return 0
	}
	totalSeconds := int64(periodDays) * subscriptionDaySeconds
	value := decimal.NewFromInt(pricePaid).
		Mul(decimal.NewFromInt(remainingSeconds)).
		Div(decimal.NewFromInt(totalSeconds))
	return value.Round(0).IntPart()
}

// subscriptionChangeAmounts 把「剩余价值 + 新套餐价」换算为
// (应补差价, 新周期时长秒)。
//
// 语义（P-11）：
//   - 应补 = max(0, 新套餐价 − 剩余价值)：任何路径都不产生负额，降级不退款；
//   - 应补 > 0（升级或剩余价值不足）：用户补足到新套餐全价 ⇒ 得到一个完整
//     新周期（时长 = 新套餐 period_days）；
//   - 应补 = 0（剩余价值 ≥ 新套餐价）：差值是用户**已付**的钱，不退还，
//     按新套餐单价折成时长 —— 时长 = floor(剩余价值 × 新周期秒 / 新套餐价)。
//     剩余价值恰好等于新套餐价时正好是一个完整周期，两种情形在边界上连续；
//   - 新套餐价 = 0（免费套餐）：没有单价可折算，直接给一个完整免费周期；
//     实例已有更长的剩余时间时由 SQL 的 GREATEST(end_at, …) 保住，不缩短。
func subscriptionChangeAmounts(remainValue, newPrice int64, newPeriodDays int32) (int64, int64) {
	periodSeconds := int64(newPeriodDays) * subscriptionDaySeconds
	if newPrice <= 0 {
		return 0, periodSeconds
	}
	if remainValue >= newPrice {
		seconds := decimal.NewFromInt(remainValue).
			Mul(decimal.NewFromInt(periodSeconds)).
			Div(decimal.NewFromInt(newPrice))
		return 0, seconds.Floor().IntPart() // 时长取整向下到秒（P-11 冻结）
	}
	return newPrice - remainValue, periodSeconds
}

// ChangePlan 创建订阅升级/降级请求。
//
// 入参只有新套餐 id 与 provider：金额/币种/周期/额度一律从套餐行读取，
// handler 不参与定价（与购买/续费同一原则）。
func (s *TopupService) ChangePlan(ctx context.Context, in ChangePlanInput) (ChangePlanResult, error) {
	if in.UserID <= 0 {
		return ChangePlanResult{}, fmt.Errorf("%w: user id must be > 0", ErrInvalidFundingCredit)
	}
	if in.SubscriptionID <= 0 {
		return ChangePlanResult{}, fmt.Errorf("%w: subscription id must be > 0", ErrChangeSubscriptionNotFound)
	}
	if in.NewPlanID <= 0 {
		return ChangePlanResult{}, fmt.Errorf("%w: plan id must be > 0", ErrSubscriptionPlanUnavailable)
	}
	// 大小写/空白归一必须早于幂等查重（同购买/续费：否则 "Stripe" 的重试会被
	// 误判成另一份订单而 409）。
	provider := strings.ToLower(strings.TrimSpace(in.Provider))
	key := strings.TrimSpace(in.IdempotencyKey)
	if key == "" || utf8.RuneCountInString(key) > maxIdempotencyKeyRunes || hasControlChars(key) {
		return ChangePlanResult{}, ErrInvalidIdempotency
	}

	// 幂等查重早于套餐/渠道/状态校验：运营在两次重试之间停用套餐或关渠道时，
	// 重试仍须拿到首次订单（与购买 P-2、续费同一口径）。
	if existing, err := s.findOrderByIdempotencyKey(ctx, key); err != nil {
		return ChangePlanResult{}, err
	} else if existing != nil {
		if existing.UserID != in.UserID ||
			existing.BizKind != store.OrderBizKindChange ||
			existing.BizRefID == nil || *existing.BizRefID != in.SubscriptionID ||
			existing.Provider != provider {
			return ChangePlanResult{}, ErrIdempotencyConflict
		}
		snap, err := decodeSubscriptionChangeSnapshot(existing)
		if err != nil {
			return ChangePlanResult{}, err
		}
		// 同一个 key 不能改口到另一个目标套餐：那是两份合同，必须换 key。
		if snap.ToPlanID != in.NewPlanID {
			return ChangePlanResult{}, ErrIdempotencyConflict
		}
		result := ChangePlanResult{
			OrderID: existing.ID, Order: existing, Replayed: true,
			PriceDelta: snap.PriceDelta, CreditsDelta: snap.CreditsDelta,
			EventKind: snap.EventKind, FromPlanID: snap.FromPlanID, NewPlanID: snap.ToPlanID,
			NewEndAt: replayEndAt(existing, snap),
		}
		return result, nil
	}

	if provider != ProviderStripe && provider != ProviderAlipay && provider != ProviderEpay {
		return ChangePlanResult{}, ErrInvalidProvider
	}

	var (
		order     store.Order
		snapshot  subscriptionChangeSnapshot
		immediate bool
	)
	err := s.subscriptionTx(ctx, func(tx *gorm.DB) error {
		// R-25：先对实例行加锁，再读余额与已用。锁在事务内，任何并发预扣/
		// 结算/退款都会阻塞到本事务结束，折算基准（剩余价值 + 已用量）因而
		// 是同一个一致快照。
		sub, err := store.LockSubscriptionForChange(ctx, tx.Statement.ConnPool,
			in.UserID, in.SubscriptionID)
		if err != nil {
			return mapSubscriptionChangeStoreError(err, in.SubscriptionID)
		}
		if sub.PlanID == in.NewPlanID {
			return fmt.Errorf("%w: subscription %d already on plan %d",
				ErrChangePlanUnchanged, in.SubscriptionID, in.NewPlanID)
		}
		plan, err := loadSubscriptionPlanForShare(tx, in.NewPlanID)
		if err != nil {
			return err
		}
		// P-10：同币种、同周期天数。两者任一不同都无法用同一个折算基准，
		// 必须 422 而不是「按天凑合算」。
		if plan.Currency != sub.Currency || plan.PeriodDays != sub.PeriodDays {
			return fmt.Errorf("%w: subscription %d is %s/%d days, plan %d is %s/%d days",
				ErrSubscriptionCycleMismatch, in.SubscriptionID, sub.Currency, sub.PeriodDays,
				plan.PlanID, plan.Currency, plan.PeriodDays)
		}

		now := s.now().UTC()
		remainValue := subscriptionRemainingValue(sub.PricePaidAmount, sub.PeriodDays, sub.EndAt, now)
		priceDelta, durationSeconds := subscriptionChangeAmounts(remainValue, plan.PriceAmount, plan.PeriodDays)
		// 事件 kind 按**套餐方向**判定（新价 > 旧价 = 升级），而不是按本次是否
		// 付了钱：周期末的降级也可能需要补足差价（剩余价值已接近 0），那时仍
		// 是一次 downgraded，而不是「升级后立刻降级」的语义。
		eventKind := store.SubscriptionEventDowngraded
		if plan.PriceAmount > sub.PricePaidAmount {
			eventKind = store.SubscriptionEventUpgraded
		}
		// 折算只会延长到期；GREATEST 在这里先求一次值（而不是只交给 SQL），
		// 是为了让快照里冻结的 end_at 与最终写入值一致——换成免费套餐等
		// 没有单价可折算的情形下，旧周期的剩余时间必须保住。
		endAt := now.Add(time.Duration(durationSeconds) * time.Second)
		if sub.EndAt.UTC().After(endAt) {
			endAt = sub.EndAt.UTC()
		}
		snapshot = subscriptionChangeSnapshot{
			SubscriptionID:      in.SubscriptionID,
			FromPlanID:          sub.PlanID,
			ToPlanID:            plan.PlanID,
			PlanTitle:           plan.PlanTitle,
			PeriodDays:          plan.PeriodDays,
			GrantCredits:        plan.GrantCredits,
			ResetOnRenew:        plan.ResetOnRenew,
			AllowWalletOverflow: plan.AllowWalletOverflow,
			ModelScope:          plan.ModelScope,
			ConcurrencyLimit:    plan.ConcurrencyLimit,
			GraceDays:           plan.GraceDays,
			Currency:            plan.Currency,
			PriceAmount:         plan.PriceAmount,
			PriceDelta:          priceDelta,
			RemainingValue:      remainValue,
			DurationSeconds:     durationSeconds,
			// credits_delta 是「本周期总额度」的变化量（新 grant − 旧 total），
			// 不是用户可用余额的变化（已用量会被钳制，见 P-12）。
			CreditsDelta: plan.GrantCredits - sub.AmountTotal,
			EventKind:    eventKind,
			EndAt:        endAt,
		}

		order = store.Order{
			UserID:         in.UserID,
			Provider:       provider,
			Status:         string(OrderPending),
			BizKind:        store.OrderBizKindChange,
			BizRefID:       &in.SubscriptionID,
			IdempotencyKey: key,
			Amount:         priceDelta,
			Currency:       plan.Currency,
		}
		if priceDelta > 0 {
			// 付费差价在插入订单前确认渠道可用与最低金额（与购买/续费同口径），
			// 避免「先留下 pending 订单再 503」。
			providerMin, err := s.providerReady(provider)
			if err != nil {
				return err
			}
			if priceDelta < providerMin {
				return fmt.Errorf("%w: change delta %d cents below provider %s minimum %d cents",
					ErrInvalidTopupAmount, priceDelta, provider, providerMin)
			}
		}

		metadata, err := json.Marshal(subscriptionChangeMetadata{Change: &snapshot})
		if err != nil {
			return fmt.Errorf("billing: marshal subscription change snapshot: %w", err)
		}
		order.ChannelMetadata = metadata

		if priceDelta == 0 {
			// 0 元差价（升级到更便宜/免费套餐，或剩余价值已覆盖新价）：不经过
			// 渠道，同一事务内创建 succeeded 订单并改写实例 + 写事件。
			immediate = true
			order.Status = string(OrderSucceeded)
			err = tx.Raw(`
				INSERT INTO orders (user_id, provider, amount, currency, status,
					idempotency_key, biz_kind, biz_ref_id, channel_metadata,
					credited_credits, paid_at)
				VALUES (?, ?, 0, ?, 'succeeded', ?, 'change', ?, ?, 0, now())
				RETURNING id, created_at, updated_at, paid_at`,
				in.UserID, provider, plan.Currency, key, in.SubscriptionID, metadata).
				Row().Scan(&order.ID, &order.CreatedAt, &order.UpdatedAt, &order.PaidAt)
			if err != nil {
				return fmt.Errorf("billing: create free change order: %w", err)
			}
			_, err = store.ChangeSubscriptionPlan(ctx, tx.Statement.ConnPool,
				changePlanInputFromSnapshot(in.UserID, snapshot, snapshot.EndAt))
			return err
		}

		err = tx.Raw(`
			INSERT INTO orders (user_id, provider, amount, currency, status,
				idempotency_key, biz_kind, biz_ref_id, channel_metadata)
			VALUES (?, ?, ?, ?, 'pending', ?, 'change', ?, ?)
			RETURNING id, created_at, updated_at`,
			in.UserID, provider, priceDelta, plan.Currency, key, in.SubscriptionID, metadata).
			Row().Scan(&order.ID, &order.CreatedAt, &order.UpdatedAt)
		if err != nil {
			return fmt.Errorf("billing: create change order: %w", err)
		}
		return nil
	})
	if err != nil {
		if isUniqueViolation(err) {
			// 并发同 key：后到者回查首次订单。
			if existing, findErr := s.findOrderByIdempotencyKey(ctx, key); findErr == nil && existing != nil {
				if existing.UserID == in.UserID &&
					existing.BizKind == store.OrderBizKindChange &&
					existing.BizRefID != nil && *existing.BizRefID == in.SubscriptionID &&
					existing.Provider == provider {
					if snap, snapErr := decodeSubscriptionChangeSnapshot(existing); snapErr == nil &&
						snap.ToPlanID == in.NewPlanID {
						return ChangePlanResult{
							OrderID: existing.ID, Order: existing, Replayed: true,
							PriceDelta: snap.PriceDelta, CreditsDelta: snap.CreditsDelta,
							EventKind: snap.EventKind, FromPlanID: snap.FromPlanID, NewPlanID: snap.ToPlanID,
							NewEndAt: replayEndAt(existing, snap),
						}, nil
					}
				}
				return ChangePlanResult{}, ErrIdempotencyConflict
			}
		}
		return ChangePlanResult{}, err
	}

	result := ChangePlanResult{
		OrderID: order.ID, Order: &order, PriceDelta: snapshot.PriceDelta,
		CreditsDelta: snapshot.CreditsDelta, EventKind: snapshot.EventKind,
		FromPlanID: snapshot.FromPlanID, NewPlanID: snapshot.ToPlanID,
	}
	if immediate {
		result.NewEndAt = snapshot.EndAt
		return result, nil
	}
	topup, err := s.startProviderPayment(ctx, &order)
	if err != nil {
		return ChangePlanResult{}, err
	}
	result.Order = topup.Order
	result.ClientSecret = topup.ClientSecret
	result.QRCode = topup.QRCode
	result.PayURL = topup.PayURL
	result.FormFields = topup.FormFields
	result.NewEndAt = snapshot.EndAt
	return result, nil
}

// replayEndAt 只在 0 元（即时生效）订单上给回放调用方 end_at：那种订单的
// 快照 end_at 就是实际写入值。付费订单的真实 end_at 在支付成功时才以
// 「支付成功时刻 + 冻结时长」求值，回放下单瞬间的估值会误导。
func replayEndAt(order *store.Order, snap subscriptionChangeSnapshot) time.Time {
	if order.Status != string(OrderSucceeded) || order.Amount != 0 {
		return time.Time{}
	}
	return snap.EndAt
}

// mapSubscriptionChangeStoreError 把 store 的实例守卫错误翻译成计费语义。
//
// store 统一用 ErrSubscriptionNotFound / ErrSubscriptionNotActive 表达原因，
// billing 侧需要更细的哨兵（404 vs 409）供 handler 映射。
func mapSubscriptionChangeStoreError(err error, subscriptionID int64) error {
	switch {
	case errors.Is(err, store.ErrSubscriptionNotFound):
		return fmt.Errorf("%w: subscription %d", ErrChangeSubscriptionNotFound, subscriptionID)
	case errors.Is(err, store.ErrSubscriptionNotActive):
		return fmt.Errorf("%w: %v", ErrSubscriptionNotChangeable, err)
	}
	return err
}

// changePlanInputFromSnapshot 把订单快照翻译为 store 的条件改写入参。
//
// endAt 由调用方给出：即时路径用下单瞬间的折算结果；付费路径用「支付成功
// 时刻 + 冻结时长」，等待支付的时间不计入用户周期。
func changePlanInputFromSnapshot(userID int64, snap subscriptionChangeSnapshot,
	endAt time.Time) store.SubscriptionChangeInput {

	var scope []byte
	if len(snap.ModelScope) > 0 {
		scope = snap.ModelScope
	}
	return store.SubscriptionChangeInput{
		SubscriptionID:      snap.SubscriptionID,
		UserID:              userID,
		FromPlanID:          snap.FromPlanID,
		PlanID:              snap.ToPlanID,
		PlanTitleSnapshot:   snap.PlanTitle,
		PeriodDays:          snap.PeriodDays,
		GrantCredits:        snap.GrantCredits,
		ResetOnRenew:        snap.ResetOnRenew,
		AllowWalletOverflow: snap.AllowWalletOverflow,
		ModelScope:          scope,
		ConcurrencyLimit:    snap.ConcurrencyLimit,
		GraceDays:           snap.GraceDays,
		Currency:            snap.Currency,
		PricePaidAmount:     snap.PriceAmount,
		EndAt:               endAt,
		EventKind:           snap.EventKind,
		PriceDelta:          snap.PriceDelta,
		CreditsDelta:        snap.CreditsDelta,
	}
}

// settleSubscriptionChange 是 change 订单的入账入口（settleOrder 分支）。
//
// 与充值分支的差异：不计算积分、不写 wallet_transactions；订单成功与实例
// 改写在同一事务内，任一步失败整体回滚，订单保持 pending 等重投/人工处理。
// 重复回调在订单已 succeeded 时由 settleOrder 提前返回，绝不会二次改写。
func (s *TopupService) settleSubscriptionChange(ctx context.Context, order *store.Order,
	providerAmount int64, providerCurrency string, raw []byte) error {

	providerCurrency = strings.ToUpper(strings.TrimSpace(providerCurrency))
	if providerCurrency != "" && providerCurrency != order.Currency {
		return s.failMismatch(ctx, order, "currency_mismatch", raw, map[string]any{
			"expected_currency": order.Currency,
			"got_currency":      providerCurrency,
		}, ErrCurrencyMismatch)
	}
	if providerAmount != order.Amount {
		return s.failMismatch(ctx, order, "amount_mismatch", raw, map[string]any{
			"expected_amount": order.Amount,
			"got_amount":      providerAmount,
		}, ErrAmountMismatch)
	}

	snap, err := decodeSubscriptionChangeSnapshot(order)
	if err != nil {
		return err
	}
	now := s.now().UTC()
	changed := false
	err = s.subscriptionTx(ctx, func(tx *gorm.DB) error {
		res := tx.Exec(`
			UPDATE orders
			SET status = 'succeeded', paid_at = now(), raw_callback = ?,
			    credited_credits = 0, provider_amount = ?, provider_currency = ?,
			    currency_scale = ?, updated_at = now()
			WHERE id = ? AND status = 'pending'`,
			raw, providerAmount, providerCurrency, currencyScale(order.Currency), order.ID)
		if res.Error != nil {
			return fmt.Errorf("billing: settle change order %d: %w", order.ID, res.Error)
		}
		if res.RowsAffected == 0 {
			// 并发回调/轮询已先行迁移：已成功即幂等返回，**不再**改写实例。
			var status string
			if err := tx.Raw(`SELECT status FROM orders WHERE id = ?`, order.ID).
				Row().Scan(&status); err != nil {
				return fmt.Errorf("billing: re-read order %d: %w", order.ID, err)
			}
			if status == string(OrderSucceeded) {
				return nil
			}
			return fmt.Errorf("%w: order %d is %s", ErrOrderStateConflict, order.ID, status)
		}
		endAt := now.Add(time.Duration(snap.DurationSeconds) * time.Second)
		if _, err := store.ChangeSubscriptionPlan(ctx, tx.Statement.ConnPool,
			changePlanInputFromSnapshot(order.UserID, snap, endAt)); err != nil {
			switch {
			case errors.Is(err, store.ErrSubscriptionNotFound),
				errors.Is(err, store.ErrSubscriptionNotActive):
				// 实例在下单后被取消/到期/换了套餐：整笔回滚，订单留 pending。
				return fmt.Errorf("%w: %v", ErrChangeOrderChanged, err)
			}
			return err
		}
		changed = true
		return nil
	})
	if err != nil {
		return err
	}
	if changed {
		s.log.Info("subscription plan changed",
			"order_id", order.ID, "subscription_id", snap.SubscriptionID,
			"from_plan_id", snap.FromPlanID, "to_plan_id", snap.ToPlanID,
			"user_id", order.UserID, "event", snap.EventKind,
			"price_delta", snap.PriceDelta, "duration_seconds", snap.DurationSeconds)
	}
	return s.refreshOrder(ctx, order)
}

// decodeSubscriptionChangeSnapshot 从 orders.channel_metadata 的 `change`
// 命名空间还原下单快照，并与订单行交叉校验。
//
// 只读本命名空间：`subscription`（购买）/`renewal`（续费）/渠道原始字段既不
// 影响解析，也不会被这里读成升降级字段。缺少该键一律视为无可用快照——
// 不兼容扁平/旧格式，绝不按当前套餐重放（R-D5-03）。
func decodeSubscriptionChangeSnapshot(order *store.Order) (subscriptionChangeSnapshot, error) {
	if order.BizRefID == nil {
		return subscriptionChangeSnapshot{}, fmt.Errorf("%w: order %d has no biz_ref_id",
			ErrChangeSnapshot, order.ID)
	}
	if len(order.ChannelMetadata) == 0 {
		return subscriptionChangeSnapshot{}, fmt.Errorf("%w: order %d has no channel_metadata",
			ErrChangeSnapshot, order.ID)
	}
	var meta subscriptionChangeMetadata
	if err := json.Unmarshal(order.ChannelMetadata, &meta); err != nil {
		return subscriptionChangeSnapshot{}, fmt.Errorf("%w: order %d: %v", ErrChangeSnapshot, order.ID, err)
	}
	if meta.Change == nil {
		return subscriptionChangeSnapshot{}, fmt.Errorf("%w: order %d has no change namespace",
			ErrChangeSnapshot, order.ID)
	}
	snap := *meta.Change
	if snap.SubscriptionID != *order.BizRefID {
		return snap, fmt.Errorf("%w: order %d snapshot targets subscription %d, want %d",
			ErrChangeSnapshot, order.ID, snap.SubscriptionID, *order.BizRefID)
	}
	if snap.FromPlanID <= 0 || snap.ToPlanID <= 0 || snap.FromPlanID == snap.ToPlanID ||
		strings.TrimSpace(snap.PlanTitle) == "" || snap.PeriodDays < 1 ||
		snap.GrantCredits < 0 || snap.GraceDays < 0 || snap.PriceAmount < 0 ||
		snap.PriceDelta < 0 || snap.RemainingValue < 0 || snap.DurationSeconds <= 0 ||
		snap.EndAt.IsZero() || strings.TrimSpace(snap.Currency) == "" {
		return snap, fmt.Errorf("%w: order %d snapshot is incomplete", ErrChangeSnapshot, order.ID)
	}
	if snap.EventKind != store.SubscriptionEventUpgraded &&
		snap.EventKind != store.SubscriptionEventDowngraded {
		return snap, fmt.Errorf("%w: order %d snapshot kind %q", ErrChangeSnapshot, order.ID, snap.EventKind)
	}
	// 订单金额就是应补差价：二者不一致说明快照被篡改或写错。
	if snap.PriceDelta != order.Amount || snap.Currency != order.Currency {
		return snap, fmt.Errorf("%w: order %d snapshot differs from order amount/currency",
			ErrChangeSnapshot, order.ID)
	}
	return snap, nil
}
