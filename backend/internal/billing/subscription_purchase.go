package billing

// 订阅购买（S3；billing-08 §3.2/§3.8/§6.2，任务书 A1 冻结裁决 1–7）。
//
// 本文件只做两件事：
//  1. 把购买请求落成 `orders(biz_kind='subscribe', biz_ref_id=plan_id)`，
//     渠道支付单复用 topup.go 的 startProviderPayment（同一套 Stripe/
//     支付宝协议，不引入第二个支付 owner）。
//  2. 在 settleOrder 的 subscribe 分支里，同一事务内把订单置 succeeded 并
//     创建 subscriptions 实例；**跳过 ComputeCredits 与钱包入账/流水**。
//
// 快照纪律（R-D5-03）：套餐派生的列必须取「下单瞬间」的条件，而不是
// 回调到达时重新读套餐——管理员可能在等待支付期间改价/改额度。因此下单
// 时把套餐快照写入 orders.channel_metadata 的**独占命名空间**
// `{"subscription":{...}}`（jsonb，不含密钥），结算时只读该键。
// channel_metadata 是订单表的共享列：其它写方（渠道原始字段、后续批次）
// 可能在同一个对象里追加自己的键，因此写入与读取都不得假设对象整体形状，
// 也不得整对象覆写。删除保护（DeletePlan 统计 pending/succeeded subscribe 订单）保证
// 回调前套餐不会被删；本文件对套餐行取 FOR SHARE，与 DeletePlan 的
// FOR UPDATE 互斥，消除「下单与删除」的窗口。
//
// 幂等三重：
//   - orders.idempotency_key UNIQUE（重复 POST 返回同一订单）；
//   - orders.provider_txn_id UNIQUE（重复回调找回同一订单）；
//   - subscriptions.order_id UNIQUE（同一订单只建一条实例的最终防线）。
// 重复回调在订单已 succeeded 时直接返回，不进入订阅插入。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"

	"newtg/gateway/internal/store"
)

var (
	// ErrSubscriptionPlanUnavailable：套餐不存在或已停用（购买侧 409）。
	// 套餐被物理删除也归入这一族：对前端表现都是「这个套餐不能买了」。
	ErrSubscriptionPlanUnavailable = errors.New("billing: subscription plan is not found or disabled")
	// ErrSubscriptionPlanInvalid：套餐行自身字段非法（价格/周期/币种），
	// 属于配置损坏而不是用户输入错误（购买侧 422）。
	ErrSubscriptionPlanInvalid = errors.New("billing: subscription plan fields are invalid")
	// ErrSubscriptionSnapshot：订单缺少可用的下单快照。正常写路径不会出现；
	// 出现时保持 pending 交人工处理，绝不按当前套餐臆测合同条件。
	ErrSubscriptionSnapshot = errors.New("billing: subscription order snapshot is missing or invalid")
)

// subscriptionOrderSnapshot 是下单瞬间的套餐快照。落库时包在
// subscriptionOrderMetadata 的 `subscription` 键下，而不是直接作为
// channel_metadata 的顶层对象。
//
// 字段与 subscriptions 的快照列一一对应（billing-08 §6.1 R-D5-03）。
// `plan_id` 单独保存，用于与 orders.biz_ref_id 交叉校验，防止元数据被
// 误写到另一张订单上。
type subscriptionOrderSnapshot struct {
	// SubscriptionID 只出现在续费订单（P2a）的 `renewal` 命名空间里：
	// 用于与 orders.biz_ref_id（= subscriptions.id）交叉校验。购买订单
	// 的 `subscription` 命名空间没有该键（omitempty），解码后为 0。
	SubscriptionID      int64           `json:"subscription_id,omitempty"`
	PlanID              int64           `json:"plan_id"`
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
}

// subscriptionOrderMetadata 是 orders.channel_metadata 中的订阅命名空间。
//
// 【为什么不把快照直接放顶层】orders.channel_metadata 是共享 jsonb 列：
// 充值订单与未来的写方都可能在同一对象里放自己的键。快照放在独占子键
// `subscription` 下，读取时只解这一个键——其它写方追加字段不会破坏快照，
// 订阅侧也不会（也没有机会）整对象覆写别人的数据。
type subscriptionOrderMetadata struct {
	Subscription *subscriptionOrderSnapshot `json:"subscription"`
}

// CreateSubscriptionOrder 创建订阅购买订单。
//
// 入参只有 plan_id 与 provider：金额/币种/周期/额度一律从套餐行读取，
// handler 不参与定价（GC / I-F-07 的唯一真源原则）。
//
// 0 元套餐（price_amount=0）不调用渠道：同一事务内直接创建 succeeded 订单
// 与 subscriptions 实例，返回 Replayed=false 且无 client_secret/qr_code
// （任务书冻结裁决 4）。
func (s *TopupService) CreateSubscriptionOrder(ctx context.Context,
	userID, planID int64, provider, idempotencyKey string) (*TopupOrder, error) {

	if userID <= 0 {
		return nil, fmt.Errorf("%w: user id must be > 0", ErrInvalidFundingCredit)
	}
	if planID <= 0 {
		return nil, fmt.Errorf("%w: plan id must be > 0", ErrSubscriptionPlanUnavailable)
	}
	// 大小写/空白归一必须早于幂等查重：否则同一 key 的 "Stripe" 重试会被
	// 误判成「另一份订单」而 409。
	provider = strings.ToLower(strings.TrimSpace(provider))
	key := strings.TrimSpace(idempotencyKey)
	if key == "" || utf8.RuneCountInString(key) > maxIdempotencyKeyRunes || hasControlChars(key) {
		return nil, ErrInvalidIdempotency
	}

	// 幂等查重必须早于套餐/渠道校验：运营在两次重试之间停用套餐或关闭渠道
	// 时，重试仍须拿到首次订单（与 CreateTopupOrder 的 P-2 同口径）。
	if existing, err := s.findOrderByIdempotencyKey(ctx, key); err != nil {
		return nil, err
	} else if existing != nil {
		if existing.UserID != userID ||
			existing.BizKind != store.OrderBizKindSubscribe ||
			existing.BizRefID == nil || *existing.BizRefID != planID ||
			existing.Provider != provider {
			return nil, ErrIdempotencyConflict
		}
		return s.replayOrder(existing, userID)
	}

	if provider != ProviderStripe && provider != ProviderAlipay {
		return nil, ErrInvalidProvider
	}

	order := &store.Order{
		UserID:         userID,
		Provider:       provider,
		Status:         string(OrderPending),
		BizKind:        store.OrderBizKindSubscribe,
		BizRefID:       &planID,
		IdempotencyKey: key,
	}
	var snapshot subscriptionOrderSnapshot
	immediate := false
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		plan, err := loadSubscriptionPlanForShare(tx, planID)
		if err != nil {
			return err
		}
		snapshot = plan
		order.Amount = plan.PriceAmount
		order.Currency = plan.Currency

		// 0 元套餐不经过渠道；付费套餐在插入订单前确认渠道可用，
		// 避免「先留下 pending 订单再 503」（与 CreateTopupOrder 一致）。
		// 套餐价还必须达到该渠道的最低下单金额：渠道会在 precreate 阶段
		// 拒绝过小金额，那时订单已落库、只能标 failed；这里提前 422，
		// 不产生任何订单。0 元套餐是显式例外（完全免费，不经渠道）。
		if plan.PriceAmount > 0 {
			providerMin, err := s.providerReady(provider)
			if err != nil {
				return err
			}
			if plan.PriceAmount < providerMin {
				return fmt.Errorf("%w: plan %d price %d cents below provider %s minimum %d cents",
					ErrInvalidTopupAmount, planID, plan.PriceAmount, provider, providerMin)
			}
		}

		metadata, err := json.Marshal(subscriptionOrderMetadata{Subscription: &snapshot})
		if err != nil {
			return fmt.Errorf("billing: marshal subscription order snapshot: %w", err)
		}
		order.ChannelMetadata = metadata

		if plan.PriceAmount == 0 {
			immediate = true
			order.Status = string(OrderSucceeded)
			err = tx.Raw(`
				INSERT INTO orders (user_id, provider, amount, currency, status,
					idempotency_key, biz_kind, biz_ref_id, channel_metadata,
					credited_credits, paid_at)
				VALUES (?, ?, 0, ?, 'succeeded', ?, 'subscribe', ?, ?, 0, now())
				RETURNING id, created_at, updated_at, paid_at`,
				userID, provider, plan.Currency, key, planID, metadata).
				Row().Scan(&order.ID, &order.CreatedAt, &order.UpdatedAt, &order.PaidAt)
			if err != nil {
				return fmt.Errorf("billing: create free subscription order: %w", err)
			}
			// 同一事务内建实例：订单 succeeded 与订阅实例原子出现。
			return insertSubscriptionTx(tx, order, snapshot, s.now().UTC())
		}

		err = tx.Raw(`
			INSERT INTO orders (user_id, provider, amount, currency, status,
				idempotency_key, biz_kind, biz_ref_id, channel_metadata)
			VALUES (?, ?, ?, ?, 'pending', ?, 'subscribe', ?, ?)
			RETURNING id, created_at, updated_at`,
			userID, provider, plan.PriceAmount, plan.Currency, key, planID, metadata).
			Row().Scan(&order.ID, &order.CreatedAt, &order.UpdatedAt)
		if err != nil {
			return fmt.Errorf("billing: create subscription order: %w", err)
		}
		return nil
	})
	if err != nil {
		if isUniqueViolation(err) {
			// 并发同 key：后到者回查首次订单。回查失败说明冲突来自
			// provider_txn_id（插入阶段不可能），按内部错误处理。
			if existing, findErr := s.findOrderByIdempotencyKey(ctx, key); findErr == nil && existing != nil {
				if existing.UserID == userID &&
					existing.BizKind == store.OrderBizKindSubscribe &&
					existing.BizRefID != nil && *existing.BizRefID == planID &&
					existing.Provider == provider {
					return s.replayOrder(existing, userID)
				}
				return nil, ErrIdempotencyConflict
			}
		}
		return nil, err
	}
	if immediate {
		return &TopupOrder{Order: order}, nil
	}
	return s.startProviderPayment(ctx, order)
}

// loadSubscriptionPlanForShare 读取并锁定套餐行，返回下单快照。
//
// `FOR SHARE` 与 store.DeletePlan 的 `FOR UPDATE` 互斥：套餐被删除的
// 竞态在这里表现为「等锁后查不到行」，而不是写入悬挂 plan_id 的订单。
func loadSubscriptionPlanForShare(tx *gorm.DB, planID int64) (subscriptionOrderSnapshot, error) {
	var (
		snap        subscriptionOrderSnapshot
		modelScope  []byte
		concurrency sql.NullInt64
		status      string
	)
	err := tx.Raw(`
		SELECT title, period_days, price_amount, currency, grant_credits,
		       reset_on_renew, allow_wallet_overflow, model_scope,
		       concurrency_limit, grace_days, status
		FROM subscription_plans WHERE id = ? FOR SHARE`, planID).Row().
		Scan(&snap.PlanTitle, &snap.PeriodDays, &snap.PriceAmount, &snap.Currency,
			&snap.GrantCredits, &snap.ResetOnRenew, &snap.AllowWalletOverflow,
			&modelScope, &concurrency, &snap.GraceDays, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return snap, fmt.Errorf("%w: plan %d", ErrSubscriptionPlanUnavailable, planID)
	}
	if err != nil {
		return snap, fmt.Errorf("billing: load subscription plan %d: %w", planID, err)
	}
	if status != "enabled" {
		return snap, fmt.Errorf("%w: plan %d is %s", ErrSubscriptionPlanUnavailable, planID, status)
	}

	snap.PlanID = planID
	snap.Currency = strings.ToUpper(strings.TrimSpace(snap.Currency))
	// S1 只支持 CNY（R-D5-07）。套餐 CHECK 已限制币种，这里再守一次：
	// 迁移/手工数据异常时宁可 422，也不能把非人民币金额当人民币入账。
	if snap.Currency != AlipayCurrency {
		return snap, fmt.Errorf("%w: plan %d currency %q", ErrSubscriptionPlanInvalid, planID, snap.Currency)
	}
	if snap.PriceAmount < 0 || snap.PeriodDays < 1 || snap.GrantCredits < 0 || snap.GraceDays < 0 {
		return snap, fmt.Errorf("%w: plan %d has out-of-range period/price/credits", ErrSubscriptionPlanInvalid, planID)
	}
	if strings.TrimSpace(snap.PlanTitle) == "" {
		return snap, fmt.Errorf("%w: plan %d has empty title", ErrSubscriptionPlanInvalid, planID)
	}
	if snap.PriceAmount > MaxTopupCents {
		return snap, fmt.Errorf("%w: plan %d price exceeds max top-up", ErrSubscriptionPlanInvalid, planID)
	}
	if len(modelScope) > 0 && string(modelScope) != "null" {
		snap.ModelScope = json.RawMessage(modelScope)
	}
	if concurrency.Valid {
		value := int32(concurrency.Int64)
		snap.ConcurrencyLimit = &value
	}
	return snap, nil
}

// insertSubscriptionTx 在调用方的 GORM 事务内插入 subscriptions 行。
//
// 不复用 store.SQLSubscriptionRepo.CreateSubscription：后者自开
// database/sql 事务，无法与订单状态更新原子提交，会留下「订单成功但没建
// 实例」的窗口。快照列全部来自下单时冻结的 snapshot，而不是当前套餐。
func insertSubscriptionTx(tx *gorm.DB, order *store.Order,
	snap subscriptionOrderSnapshot, startAt time.Time) error {

	startAt = startAt.UTC()
	endAt := startAt.AddDate(0, 0, int(snap.PeriodDays))
	var nextResetAt any
	if snap.ResetOnRenew {
		nextResetAt = endAt
	}
	var modelScope any
	if len(snap.ModelScope) > 0 {
		modelScope = string(snap.ModelScope)
	}
	var concurrency any
	if snap.ConcurrencyLimit != nil {
		concurrency = *snap.ConcurrencyLimit
	}

	err := tx.Exec(`
		INSERT INTO subscriptions
			(user_id, plan_id, plan_title_snapshot, status, start_at, end_at,
			 next_reset_at, amount_total, amount_used, allow_wallet_overflow,
			 auto_renew, cancel_at_period_end, order_id, currency,
			 price_paid_amount, period_days, reset_on_renew, grace_days,
			 model_scope, concurrency_limit)
		VALUES (?, ?, ?, 'active', ?, ?, ?, ?, 0, ?, false, false, ?, ?, ?, ?, ?, ?, ?, ?)`,
		order.UserID, snap.PlanID, snap.PlanTitle, startAt, endAt, nextResetAt,
		snap.GrantCredits, snap.AllowWalletOverflow, order.ID, snap.Currency,
		order.Amount, snap.PeriodDays, snap.ResetOnRenew, snap.GraceDays,
		modelScope, concurrency).Error
	if err != nil {
		return fmt.Errorf("billing: create subscription for order %d: %w", order.ID, err)
	}
	return nil
}

// settleSubscriptionOrder 是 subscribe 订单的入账入口。
//
// 与充值分支的关键差异：
//   - 不调用 ComputeCredits / 分组倍率 / userGroup；
//   - 不调用 funding.ApplyTx（wallet_transactions 零新增）；
//   - 条件 UPDATE 与 subscriptions INSERT 在同一事务内，任一步失败整体回滚，
//     订单保持 pending 等重投/人工处理（任务书冻结裁决 3/6）。
func (s *TopupService) settleSubscriptionOrder(ctx context.Context, order *store.Order,
	providerAmount int64, providerCurrency string, raw []byte) error {

	// 与充值分支同口径：订单币种是大写 ISO-4217，渠道回调可能是小写。
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

	snap, err := decodeSubscriptionSnapshot(order)
	if err != nil {
		return err
	}
	startAt := s.now().UTC()
	created := false
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res := tx.Exec(`
			UPDATE orders
			SET status = 'succeeded', paid_at = now(), raw_callback = ?,
			    credited_credits = 0, provider_amount = ?, provider_currency = ?,
			    currency_scale = ?, updated_at = now()
			WHERE id = ? AND status = 'pending'`,
			raw, providerAmount, providerCurrency, currencyScale(order.Currency), order.ID)
		if res.Error != nil {
			return fmt.Errorf("billing: settle subscription order %d: %w", order.ID, res.Error)
		}
		if res.RowsAffected == 0 {
			// 并发回调/轮询已先行迁移：同一事务内回查，已成功即幂等返回。
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
		if err := insertSubscriptionTx(tx, order, snap, startAt); err != nil {
			return err
		}
		created = true
		return nil
	})
	if err != nil {
		return err
	}
	if created {
		s.log.Info("subscription activated",
			"order_id", order.ID, "plan_id", snap.PlanID,
			"user_id", order.UserID, "period_days", snap.PeriodDays,
			"amount_total", snap.GrantCredits)
	}
	return s.refreshOrder(ctx, order)
}

// decodeSubscriptionSnapshot 从 orders.channel_metadata 的 `subscription`
// 命名空间还原下单快照。
//
// 只读本命名空间：同一 jsonb 对象里的其它键（渠道原始字段、其它子系统
// 的元数据）既不影响解析，也不会被这里读成快照字段。缺少该键的对象
// 一律视为「没有可用的下单快照」（包括历史上的扁平顶层格式）——不猜测、
// 不兼容：按当前套餐重放会把管理员在下单后的编辑施加到用户已付款的
// 合同上（R-D5-03 明确禁止）。
func decodeSubscriptionSnapshot(order *store.Order) (subscriptionOrderSnapshot, error) {
	if order.BizRefID == nil {
		return subscriptionOrderSnapshot{}, fmt.Errorf("%w: order %d has no biz_ref_id",
			ErrSubscriptionSnapshot, order.ID)
	}
	if len(order.ChannelMetadata) == 0 {
		return subscriptionOrderSnapshot{}, fmt.Errorf("%w: order %d has no channel_metadata",
			ErrSubscriptionSnapshot, order.ID)
	}
	var meta subscriptionOrderMetadata
	if err := json.Unmarshal(order.ChannelMetadata, &meta); err != nil {
		return subscriptionOrderSnapshot{}, fmt.Errorf("%w: order %d: %v", ErrSubscriptionSnapshot, order.ID, err)
	}
	if meta.Subscription == nil {
		return subscriptionOrderSnapshot{}, fmt.Errorf("%w: order %d has no subscription namespace",
			ErrSubscriptionSnapshot, order.ID)
	}
	snap := *meta.Subscription
	if snap.PlanID != *order.BizRefID || strings.TrimSpace(snap.PlanTitle) == "" ||
		snap.PeriodDays < 1 || snap.GrantCredits < 0 || snap.PriceAmount < 0 ||
		snap.Currency == "" {
		return snap, fmt.Errorf("%w: order %d snapshot is incomplete", ErrSubscriptionSnapshot, order.ID)
	}
	// 金额/币种必须与订单行一致：快照与订单不一致说明数据被篡改或写错。
	if snap.PriceAmount != order.Amount || snap.Currency != order.Currency {
		return snap, fmt.Errorf("%w: order %d snapshot differs from order amount/currency",
			ErrSubscriptionSnapshot, order.ID)
	}
	return snap, nil
}
