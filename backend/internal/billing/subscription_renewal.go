package billing

// 订阅手动续费（P2a；billing-08 §3.3 方案甲）。
//
// 续费**复用同一实例**：支付成功后推进 subscriptions.end_at，不新建行。
// 取方案甲的理由见 billing-08 §3.3——会话在 PreConsume 时冻结
// subscriptionId，若续费换行会让同一在飞请求的结算落到不同实例。
//
// 本文件与 subscription_purchase.go 的分工：
//   - 下单：落 `orders(biz_kind='renew', biz_ref_id=subscriptions.id)`，
//     金额/周期/额度取**下单瞬间**的套餐行（R-D5-03），快照写入
//     orders.channel_metadata 的独占命名空间 `{"renewal":{...}}`；
//     渠道支付单复用 topup.go 的 startProviderPayment（同一支付 owner）。
//   - 结算：settleOrder 的 renew 分支，同一事务内订单 pending→succeeded
//     并条件扩展实例；任何一步失败整体回滚，订单保持 pending。
//
// 幂等：
//   - orders.idempotency_key UNIQUE（重复 POST 返回同一订单）；
//   - orders.provider_txn_id UNIQUE（重复回调找回同一订单）；
//   - 结算只由「订单 pending→succeeded」这一次转换触发（WHERE status='pending'
//     + RowsAffected），并发/重投回调不会二次推进 end_at。

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
	// ErrRenewalSubscriptionNotFound：续费目标实例不存在，或不属于当前用户。
	// 两者统一 404：不能因为「存在但属于别人」暴露 ID 存在性。
	ErrRenewalSubscriptionNotFound = errors.New("billing: subscription to renew is not found")
	// ErrSubscriptionNotRenewable：实例处于 cancelled 终态（或并发进入终态），
	// 不能续费（409）。
	ErrSubscriptionNotRenewable = errors.New("billing: subscription cannot be renewed")
	// ErrRenewalSnapshot：续费订单缺少可用快照或快照与订单不一致。正常写路径
	// 不会出现；出现时保持 pending，绝不按当前套餐臆测合同条件。
	ErrRenewalSnapshot = errors.New("billing: renewal order snapshot is missing or invalid")
)

// renewalOrderMetadata 是 orders.channel_metadata 中的续费命名空间。
//
// 与 purchase 的 `subscription` 键分开：两种订单的 biz_ref_id 语义不同
// （plan_id vs subscriptions.id），共用一个键会让解码方必须靠 biz_kind
// 猜测字段含义。独立键让两边的校验各自封闭，也允许共享 jsonb 列的其它
// 写方继续追加自己的键。
type renewalOrderMetadata struct {
	Renewal *subscriptionOrderSnapshot `json:"renewal"`
}

// CreateSubscriptionRenewalOrder 创建手动续费订单。
//
// 入参只有 subscriptionID 与 provider：金额/币种/周期/额度一律从实例当前
// 绑定的套餐行读取，handler 不参与定价。仅 active/expired 可续费；cancelled
// 返回 ErrSubscriptionNotRenewable（409）。
//
// 0 元套餐不调用渠道：同一事务内直接创建 succeeded 订单并扩展实例（与 S3
// 即时路径同构）。
func (s *TopupService) CreateSubscriptionRenewalOrder(ctx context.Context,
	userID, subscriptionID int64, provider, idempotencyKey string) (*TopupOrder, error) {

	if userID <= 0 {
		return nil, fmt.Errorf("%w: user id must be > 0", ErrInvalidFundingCredit)
	}
	if subscriptionID <= 0 {
		return nil, fmt.Errorf("%w: subscription id must be > 0", ErrRenewalSubscriptionNotFound)
	}
	// 大小写/空白归一必须早于幂等查重（同 CreateSubscriptionOrder）。
	provider = strings.ToLower(strings.TrimSpace(provider))
	key := strings.TrimSpace(idempotencyKey)
	if key == "" || utf8.RuneCountInString(key) > maxIdempotencyKeyRunes || hasControlChars(key) {
		return nil, ErrInvalidIdempotency
	}

	// 幂等查重早于套餐/渠道/状态校验：运营在两次重试之间停用套餐时，
	// 重试仍须拿到首次订单（与购买/充值的 P-2 口径一致）。
	if existing, err := s.findOrderByIdempotencyKey(ctx, key); err != nil {
		return nil, err
	} else if existing != nil {
		if existing.UserID != userID ||
			existing.BizKind != store.OrderBizKindRenew ||
			existing.BizRefID == nil || *existing.BizRefID != subscriptionID ||
			existing.Provider != provider {
			return nil, ErrIdempotencyConflict
		}
		return s.replayOrder(existing, userID)
	}

	if provider != ProviderStripe && provider != ProviderAlipay && provider != ProviderEpay {
		return nil, ErrInvalidProvider
	}

	order := &store.Order{
		UserID:         userID,
		Provider:       provider,
		Status:         string(OrderPending),
		BizKind:        store.OrderBizKindRenew,
		BizRefID:       &subscriptionID,
		IdempotencyKey: key,
	}
	var snapshot subscriptionOrderSnapshot
	immediate := false
	err := s.subscriptionTx(ctx, func(tx *gorm.DB) error {
		planID, err := loadRenewableSubscriptionTx(tx, userID, subscriptionID)
		if err != nil {
			return err
		}
		plan, err := loadSubscriptionPlanForShare(tx, planID)
		if err != nil {
			return err
		}
		snapshot = plan
		snapshot.SubscriptionID = subscriptionID
		order.Amount = plan.PriceAmount
		order.Currency = plan.Currency

		// 付费续费在插入订单前确认渠道可用（金额下限口径与购买一致）；
		// 0 元套餐是显式例外（完全免费，不经渠道）。
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

		metadata, err := json.Marshal(renewalOrderMetadata{Renewal: &snapshot})
		if err != nil {
			return fmt.Errorf("billing: marshal renewal order snapshot: %w", err)
		}
		order.ChannelMetadata = metadata

		if plan.PriceAmount == 0 {
			immediate = true
			order.Status = string(OrderSucceeded)
			err = tx.Raw(`
				INSERT INTO orders (user_id, provider, amount, currency, status,
					idempotency_key, biz_kind, biz_ref_id, channel_metadata,
					credited_credits, paid_at)
				VALUES (?, ?, 0, ?, 'succeeded', ?, 'renew', ?, ?, 0, now())
				RETURNING id, created_at, updated_at, paid_at`,
				userID, provider, plan.Currency, key, subscriptionID, metadata).
				Row().Scan(&order.ID, &order.CreatedAt, &order.UpdatedAt, &order.PaidAt)
			if err != nil {
				return fmt.Errorf("billing: create free renewal order: %w", err)
			}
			// 同一事务内扩展实例：订单 succeeded 与 end_at 推进原子出现。
			_, err = store.RenewSubscription(ctx, tx.Statement.ConnPool,
				renewalExtendInput(userID, snapshot, order.Amount, s.now().UTC()))
			return err
		}

		err = tx.Raw(`
			INSERT INTO orders (user_id, provider, amount, currency, status,
				idempotency_key, biz_kind, biz_ref_id, channel_metadata)
			VALUES (?, ?, ?, ?, 'pending', ?, 'renew', ?, ?)
			RETURNING id, created_at, updated_at`,
			userID, provider, plan.PriceAmount, plan.Currency, key, subscriptionID, metadata).
			Row().Scan(&order.ID, &order.CreatedAt, &order.UpdatedAt)
		if err != nil {
			return fmt.Errorf("billing: create renewal order: %w", err)
		}
		return nil
	})
	if err != nil {
		if isUniqueViolation(err) {
			// 并发同 key：后到者回查首次订单。
			if existing, findErr := s.findOrderByIdempotencyKey(ctx, key); findErr == nil && existing != nil {
				if existing.UserID == userID &&
					existing.BizKind == store.OrderBizKindRenew &&
					existing.BizRefID != nil && *existing.BizRefID == subscriptionID &&
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

// loadRenewableSubscriptionTx 读取续费目标并做归属/状态守卫，返回其 plan_id。
//
// 放在事务内、套餐行锁之前：先确认实例归当前用户，再谈绑定套餐。cancelled
// 是唯一不可续费的终态（expired 允许续费后恢复 active）。不锁实例行：
// 下单不修改实例，真正的迁移守卫在结算的条件 UPDATE（status IN active,
// expired）；这里只拒绝「下单时已确定不能续费」的请求。
func loadRenewableSubscriptionTx(tx *gorm.DB, userID, subscriptionID int64) (int64, error) {
	var (
		ownerID int64
		planID  int64
		status  string
	)
	err := tx.Raw(`SELECT user_id, plan_id, status FROM subscriptions WHERE id = ?`,
		subscriptionID).Row().Scan(&ownerID, &planID, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("%w: subscription %d", ErrRenewalSubscriptionNotFound, subscriptionID)
	}
	if err != nil {
		return 0, fmt.Errorf("billing: load subscription %d for renewal: %w", subscriptionID, err)
	}
	if ownerID != userID {
		// 归属不符与不存在同口径：不泄露他人订阅 ID 的存在性。
		return 0, fmt.Errorf("%w: subscription %d", ErrRenewalSubscriptionNotFound, subscriptionID)
	}
	if status != "active" && status != "expired" {
		return 0, fmt.Errorf("%w: subscription %d is %s",
			ErrSubscriptionNotRenewable, subscriptionID, status)
	}
	return planID, nil
}

// renewalExtendInput 把下单快照翻译为 store 的条件扩展入参。
//
// price 用订单金额而不是快照价格：decode 已保证二者相等，这里以订单行为准，
// 避免以后有人只改其中一侧时静默漂移。
func renewalExtendInput(userID int64, snap subscriptionOrderSnapshot,
	price int64, now time.Time) store.RenewalExtendInput {

	var scope []byte
	if len(snap.ModelScope) > 0 {
		scope = snap.ModelScope
	}
	return store.RenewalExtendInput{
		SubscriptionID:      snap.SubscriptionID,
		UserID:              userID,
		PlanID:              snap.PlanID,
		PlanTitleSnapshot:   snap.PlanTitle,
		PeriodDays:          snap.PeriodDays,
		GrantCredits:        snap.GrantCredits,
		ResetOnRenew:        snap.ResetOnRenew,
		AllowWalletOverflow: snap.AllowWalletOverflow,
		ModelScope:          scope,
		ConcurrencyLimit:    snap.ConcurrencyLimit,
		GraceDays:           snap.GraceDays,
		Currency:            snap.Currency,
		PricePaidAmount:     price,
		Now:                 now,
	}
}

// settleSubscriptionRenewal 是 renew 订单的入账入口（settleOrder 分支）。
//
// 与充值分支的关键差异：不计算积分、不写 wallet_transactions；订单成功
// 与实例扩展在同一事务内，任一步失败整体回滚，订单保持 pending 等重投/
// 人工处理。重复回调在订单已 succeeded 时由 settleOrder 提前返回，绝不会
// 二次推进 end_at。
func (s *TopupService) settleSubscriptionRenewal(ctx context.Context, order *store.Order,
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

	snap, err := decodeRenewalSnapshot(order)
	if err != nil {
		return err
	}
	now := s.now().UTC()
	extended := false
	err = s.subscriptionTx(ctx, func(tx *gorm.DB) error {
		res := tx.Exec(`
			UPDATE orders
			SET status = 'succeeded', paid_at = now(), raw_callback = ?,
			    credited_credits = 0, provider_amount = ?, provider_currency = ?,
			    currency_scale = ?, updated_at = now()
			WHERE id = ? AND status = 'pending'`,
			raw, providerAmount, providerCurrency, currencyScale(order.Currency), order.ID)
		if res.Error != nil {
			return fmt.Errorf("billing: settle renewal order %d: %w", order.ID, res.Error)
		}
		if res.RowsAffected == 0 {
			// 并发回调/轮询已先行迁移：同一事务内回查，已成功即幂等返回。
			// 这里**不得**再扩展实例——扩展只属于那一次 pending→succeeded。
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
		if _, err := store.RenewSubscription(ctx, tx.Statement.ConnPool,
			renewalExtendInput(order.UserID, snap, order.Amount, now)); err != nil {
			return err
		}
		extended = true
		return nil
	})
	if err != nil {
		return err
	}
	if extended {
		s.log.Info("subscription renewed",
			"order_id", order.ID, "subscription_id", snap.SubscriptionID,
			"plan_id", snap.PlanID, "user_id", order.UserID,
			"period_days", snap.PeriodDays, "amount_total", snap.GrantCredits)
	}
	return s.refreshOrder(ctx, order)
}

// decodeRenewalSnapshot 从 orders.channel_metadata 的 `renewal` 命名空间
// 还原下单快照，并与订单行交叉校验。
//
// 只读本命名空间：其它键（渠道原始字段、subscription 购买快照等）既不
// 影响解析，也不会被这里读成续费字段。缺少该键一律视为无可用快照——不
// 兼容扁平/旧格式，绝不按当前套餐重放（R-D5-03）。
func decodeRenewalSnapshot(order *store.Order) (subscriptionOrderSnapshot, error) {
	if order.BizRefID == nil {
		return subscriptionOrderSnapshot{}, fmt.Errorf("%w: order %d has no biz_ref_id",
			ErrRenewalSnapshot, order.ID)
	}
	if len(order.ChannelMetadata) == 0 {
		return subscriptionOrderSnapshot{}, fmt.Errorf("%w: order %d has no channel_metadata",
			ErrRenewalSnapshot, order.ID)
	}
	var meta renewalOrderMetadata
	if err := json.Unmarshal(order.ChannelMetadata, &meta); err != nil {
		return subscriptionOrderSnapshot{}, fmt.Errorf("%w: order %d: %v", ErrRenewalSnapshot, order.ID, err)
	}
	if meta.Renewal == nil {
		return subscriptionOrderSnapshot{}, fmt.Errorf("%w: order %d has no renewal namespace",
			ErrRenewalSnapshot, order.ID)
	}
	snap := *meta.Renewal
	if snap.SubscriptionID != *order.BizRefID {
		return snap, fmt.Errorf("%w: order %d snapshot targets subscription %d, want %d",
			ErrRenewalSnapshot, order.ID, snap.SubscriptionID, *order.BizRefID)
	}
	if snap.PlanID <= 0 || strings.TrimSpace(snap.PlanTitle) == "" ||
		snap.PeriodDays < 1 || snap.GrantCredits < 0 || snap.PriceAmount < 0 ||
		snap.GraceDays < 0 || snap.Currency == "" {
		return snap, fmt.Errorf("%w: order %d snapshot is incomplete", ErrRenewalSnapshot, order.ID)
	}
	if snap.PriceAmount != order.Amount || snap.Currency != order.Currency {
		return snap, fmt.Errorf("%w: order %d snapshot differs from order amount/currency",
			ErrRenewalSnapshot, order.ID)
	}
	return snap, nil
}
