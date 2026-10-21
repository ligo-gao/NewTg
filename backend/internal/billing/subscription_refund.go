package billing

// subscription_refund.go —— 订阅购买退款（W2/S3；R-30–R-34、P-13）。
//
// 业务边界：
//   - 只处理**订阅订单**（biz_kind = subscribe / renew / change）。充值订单
//     的退款不属于本端点；管理员若要对充值退款，走渠道后台或后续独立流程。
//   - R-35：只有「未消耗、active 且未过期」（amount_used == 0、status='active'
//     且 end_at > now）的订阅可以全额退款；已消耗/已过期/已取消一律
//     ErrSubscriptionInUse（HTTP 422）。
//   - R-38：自动退款（全额 + 取消订阅）只对 biz_kind='subscribe' 的 succeeded
//     订单开放。renew/change 的 succeeded 订单退款需要回退 end_at / plan，
//     属于独立的领域逻辑，本批返回 422 要求人工处理——否则会把用户此前
//     已付费、仍在使用的整条订阅一起作废。
//   - R-30：S2 产生的 orders.status='refund_required'（钱已收、履约失败）
//     必须可退。这类订单**不走 P-13**：履约根本没有发生，订阅是否已被消耗
//     与「退还这笔没交付的钱」无关。
//   - R-31：退款幂等。锚点是 orders 行锁 + 状态机：同一订单第二次调用看到
//     refunded 就直接返回既有结果，不再出账。钱包退款与 orders.refunded_amount
//     在同一事务；原路退款则先调渠道、成功后在同一事务写订单与流水事实。
//   - R-32：渠道退款失败 → refund_failed（可重试），不本地冲销、不二次扣减。
//     渠道请求自带幂等键（Stripe Idempotency-Key / 支付宝 out_request_no），
//     所以「渠道已退、本地落库失败」的重试不会退第二次。
//   - R-33：succeeded → refund_required 已在 Transition 显式加白（见 order.go）。
//   - R-34：退款事件 kind='refunded'，detail 只放 order_id / amount / reason。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"newtg/gateway/internal/store"
)

const (
	// refundReasonMaxRunes 与 orders.refund_reason varchar(200) 及
	// subscription_events.detail 的读侧口径一致（按字符而不是字节）。
	refundReasonMaxRunes = 200

	// stripeRefundIdempotencyPrefix / alipayRefundRequestPrefix 是渠道侧
	// 幂等键的固定前缀：同一订单永远得到同一个键，重试只会命中渠道已受理
	// 的那一笔退款（R-32 的「不二次出账」在渠道侧的落点）。
	stripeRefundIdempotencyPrefix = "refund-order-"
	alipayRefundRequestPrefix     = "refund-"
)

var (
	// ErrRefundNotSupported 表示该订单不属于本端点可退的范围（非订阅订单、
	// 状态不可退等），HTTP 层映射 409。
	ErrRefundNotSupported = errors.New("billing: order is not refundable")
	// ErrSubscriptionInUse 表示订阅已开始或已消耗（P-13），HTTP 层映射 422
	// subscription_in_use。
	ErrSubscriptionInUse = errors.New("billing: subscription is already started or used")
	// ErrInvalidRefundReason 表示退款原因非法（空/超长/控制字符），HTTP 400。
	ErrInvalidRefundReason = errors.New("billing: invalid refund reason")
	// ErrRefundManualOnly 表示该订单不能自动退款（R-38）：renew/change 的
	// succeeded 订单需要回退 end_at / plan，不能沿用「全额退款 + 取消整条
	// 订阅」的自动化路径。HTTP 层映射 422，由运营人工处理。
	ErrRefundManualOnly = errors.New("billing: order requires manual refund review")
	// ErrRefundProviderFailed 表示渠道退款失败，订单已置 refund_failed 可重试
	// （R-32）。HTTP 层映射 502；调用方可以用 errors.Is 继续识别底层渠道错误。
	ErrRefundProviderFailed = errors.New("billing: provider refund failed")
	// ErrRefundWalletLimit 表示回补会把钱包推过 2^53−1 上界（R-37/M-6）。
	// 独立哨兵让 HTTP 层给出 409 而不是笼统 500：这不是程序故障，而是
	// 「余额已接近上限，退款需要人工处理」的业务结果。
	ErrRefundWalletLimit = errors.New("billing: refund would exceed wallet limit")
)

// RefundInput 是一次管理员退款请求。
type RefundInput struct {
	OrderID     int64
	Reason      string
	ActorUserID int64
}

// RefundResult 是退款结果。
//
// Refunded=false 表示本次调用命中了幂等锚点（订单已经 refunded），没有再次
// 出账；调用方据此区分「新退款」与「重放」，但两者都应返回 200。
// Amount 是订单金额（分）；钱包退款回补的积分可能不同（按购买时的实际扣减
// 回补），两者刻意不混用同一字段。
type RefundResult struct {
	Order    *store.Order
	Refunded bool
	Amount   int64
}

// RefundRequest 是用户侧「申请退款」的持久化标记（只写请求，不动钱）。
type RefundRequest struct {
	OrderID     int64
	RequestedAt time.Time
	Reason      string
}

// RefundSubscription 执行一次管理员发起的订阅订单退款（R-30–R-34）。
//
// 流程按渠道分叉：
//   - wallet：不经过第三方，订单行锁 + 钱包回补 + 订单/订阅状态 + 事件
//     全部在一个事务内（R-31）。
//   - stripe/alipay：先在事务外调用渠道退款（带幂等键），成功后再用一个
//     事务写 refunded_amount / 取消订阅 / 追事件。渠道失败时把订单置
//     refund_failed，绝不改钱包（R-32）。
//   - epay：协议没有退款 API（与主动查询/关单相同的能力缺口），返回
//     ErrProviderActionUnsupported。钱需要运营在易支付后台原路退；订单保持
//     原状态便于人工对账，而不是伪造成 refund_failed 让重试永远失败。
func (s *TopupService) RefundSubscription(ctx context.Context, in RefundInput) (*RefundResult, error) {
	reason, err := normalizeRefundReason(in.Reason)
	if err != nil {
		return nil, err
	}
	if in.OrderID <= 0 || in.ActorUserID <= 0 {
		return nil, fmt.Errorf("%w: order id and actor must be positive", ErrRefundNotSupported)
	}

	order, err := s.GetOrder(ctx, in.OrderID)
	if err != nil {
		return nil, err
	}
	// 幂等锚点（R-31）：已退款的订单直接返回既有结果，不再调渠道/钱包。
	if OrderStatus(order.Status) == OrderRefunded {
		return &RefundResult{Order: order, Refunded: false, Amount: order.RefundedAmount}, nil
	}
	if !refundableOrderStatus(order.Status) {
		return nil, fmt.Errorf("%w: order %d is %s", ErrOrderStateConflict, order.ID, order.Status)
	}
	if !refundableBizKind(order.BizKind) {
		return nil, fmt.Errorf("%w: order %d has biz_kind %q", ErrRefundNotSupported, order.ID, order.BizKind)
	}
	if err := autoRefundAllowed(order); err != nil {
		return nil, err
	}

	// 第一次资格检查（乐观、无锁）。钱包路径会在事务内用行锁重做；渠道路径
	// 的最终落库不再重复 P-13——渠道已经退款成功后拒绝落库会造成「渠道已退、
	// 本地未退」的账实不符。
	if err := s.checkSubscriptionRefundEligible(ctx, s.db, order, s.now().UTC()); err != nil {
		return nil, err
	}

	// 0 元订单没有资金需要移动：免费套餐订单可能仍带 provider（创建时未走
	// 渠道），不能因为「渠道没有交易号」把它判成退款失败。直接走本地落库。
	if order.Amount == 0 || order.Provider == ProviderWallet {
		return s.refundWalletOrder(ctx, order.ID, reason, in.ActorUserID)
	}
	// R-37：调渠道之前先落「退款进行中」。渠道调用与本地事务之间没有原子性，
	// 这个标记让「渠道已退、本地事务失败/进程崩溃」的订单在订单中心一眼可见，
	// 而不是只剩一行日志。成功 finalize 会清空它，失败会改写成 provider_*。
	//
	// 渠道能力在落标记前判定：epay 没有退款 API，既不进「进行中」也不进
	// refund_failed（重试永远不会成功），订单保持原状态等人工处理。
	switch order.Provider {
	case ProviderStripe, ProviderAlipay:
	case ProviderEpay:
		return nil, fmt.Errorf("%w: %s has no refund API", ErrProviderActionUnsupported, ProviderEpay)
	default:
		return nil, fmt.Errorf("%w: provider %s", ErrProviderDisabled, order.Provider)
	}
	if err := s.markOrderRefundInProgress(ctx, order.ID); err != nil {
		return nil, err
	}
	if err := s.refundProviderPayment(ctx, order, reason); err != nil {
		// R-32：渠道退款失败 → refund_failed，可重试；不动订单金额/钱包。
		s.markOrderRefundFailed(ctx, order.ID, err)
		fresh, getErr := s.GetOrder(ctx, order.ID)
		if getErr == nil {
			// 并发退款可能已在本次渠道调用期间完成（另一个管理员抢先成功）。
			// 渠道幂等键保证没有第二笔退款，此时应按幂等成功返回而不是 502。
			if OrderStatus(fresh.Status) == OrderRefunded {
				return &RefundResult{Order: fresh, Refunded: false, Amount: fresh.RefundedAmount}, nil
			}
			order = fresh
		}
		return nil, fmt.Errorf("%w: %w", ErrRefundProviderFailed, err)
	}
	return s.finalizeProviderRefund(ctx, order.ID, reason, in.ActorUserID)
}

// RequestSubscriptionRefund 记录用户侧退款申请（S3 brief：只写请求，不直接
// 退款）。只有「未消耗、active 且未过期」的订阅可以申请（R-35）；管理员随后
// 通过 POST /api/orders/{id}/refund 审批并实际退款。
func (s *TopupService) RequestSubscriptionRefund(ctx context.Context,
	subscriptionID, userID int64, reason string) (*RefundRequest, error) {

	reason, err := normalizeRefundReason(reason)
	if err != nil {
		return nil, err
	}
	if subscriptionID <= 0 || userID <= 0 {
		return nil, store.ErrSubscriptionNotFound
	}
	var sub store.Subscription
	if err := s.db.WithContext(ctx).Where("id = ?", subscriptionID).First(&sub).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, store.ErrSubscriptionNotFound
		}
		return nil, fmt.Errorf("billing: load subscription %d: %w", subscriptionID, err)
	}
	// 归属校验放在服务层而不是只信 handler：退款申请会写资金相关订单行。
	if sub.UserID != userID {
		return nil, store.ErrSubscriptionNotFound
	}
	// R-35：申请退款同样只对「active、未消耗、未过期」的订阅开放。
	if err := subscriptionRefundEligible(&sub, s.now().UTC()); err != nil {
		return nil, err
	}
	if sub.OrderID == nil {
		return nil, fmt.Errorf("%w: subscription %d has no purchase order", ErrRefundNotSupported, sub.ID)
	}

	var (
		requestedAt  time.Time
		storedReason string
	)
	err = s.db.WithContext(ctx).Raw(`
		UPDATE orders
		SET refund_requested_at = COALESCE(refund_requested_at, now()),
		    refund_reason = COALESCE(refund_reason, ?),
		    updated_at = now()
		WHERE id = ? AND status = 'succeeded'
		RETURNING refund_requested_at, refund_reason`,
		reason, *sub.OrderID).Row().Scan(&requestedAt, &storedReason)
	if err != nil {
		// 条件 UPDATE 未命中的原因只有两种：订单已不是 succeeded（已退款/
		// 不可退）或并发写方先改了行。重新读取后给出可读错误；已请求过的
		// 订单返回既有标记（幂等，不因为重复点击报错）。
		var order store.Order
		if loadErr := s.db.WithContext(ctx).Where("id = ?", *sub.OrderID).First(&order).Error; loadErr != nil {
			if errors.Is(loadErr, gorm.ErrRecordNotFound) {
				return nil, fmt.Errorf("%w: order %d", ErrOrderNotFound, *sub.OrderID)
			}
			return nil, fmt.Errorf("billing: load order %d for refund request: %w", *sub.OrderID, loadErr)
		}
		if order.RefundRequestedAt != nil {
			existing := ""
			if order.RefundReason != nil {
				existing = *order.RefundReason
			}
			return &RefundRequest{
				OrderID: order.ID, RequestedAt: *order.RefundRequestedAt, Reason: existing,
			}, nil
		}
		if OrderStatus(order.Status) == OrderRefunded {
			return nil, fmt.Errorf("%w: order %d is already refunded", ErrRefundNotSupported, order.ID)
		}
		return nil, fmt.Errorf("%w: order %d is %s", ErrOrderStateConflict, order.ID, order.Status)
	}
	return &RefundRequest{OrderID: *sub.OrderID, RequestedAt: requestedAt, Reason: storedReason}, nil
}

// ---------------------------------------------------------------------------
// 资格判定
// ---------------------------------------------------------------------------

// refundableOrderStatus 是退款入口允许的订单状态：已付款、待人工退款、
// 退款失败可重试。其它状态（pending/failed/expired/cancelled/refunded）不可退。
func refundableOrderStatus(status string) bool {
	switch OrderStatus(status) {
	case OrderSucceeded, OrderRefundRequired, OrderRefundFailed:
		return true
	default:
		return false
	}
}

// refundableBizKind 限定本端点的业务范围：订阅购买/续费/升降级订单。
//
// 注意它只回答「是不是订阅订单」；**能否自动退款**还要看 autoRefundAllowed
// （R-38 把 succeeded 的 renew/change 排除在自动路径之外）。
func refundableBizKind(kind string) bool {
	switch kind {
	case store.OrderBizKindSubscribe, store.OrderBizKindRenew, store.OrderBizKindChange:
		return true
	default:
		return false
	}
}

// autoRefundAllowed 实现 R-38：自动退款（全额退款 + 取消整条订阅）只适用于
// biz_kind='subscribe' 的 succeeded 订单。
//
//   - renew succeeded：退款应回退 end_at（把续费周期减回去），而不是取消整条
//     订阅——用户此前的付费周期仍在履约。
//   - change succeeded：退款应回退 plan 变更，同样不能取消整条订阅。
//   - refund_required：S2 的「钱已收、履约失败」，必须可退（R-30），且按
//     R-36 不取消订阅。S4/R-43 之后 renew 也会产生 refund_required（同周期
//     晚到支付），同样必须可退。
//   - refund_failed：重试路径（R-32）。可能来自 succeeded（subscribe）或
//     refund_required（change/renew）；两种都必须能重试。R-46 明确 renew
//     的 refund_failed 不能因 biz_kind 被拒，否则渠道失败一次后永远无法
//     重试。
//
// **succeeded** 的 renew/change 仍返回 ErrRefundManualOnly（HTTP 422）：
// 已履约的续费/升降级要回退 end_at/plan，自动退款会误伤用户仍在使用的
// 订阅（R-38 不变）。
func autoRefundAllowed(order *store.Order) error {
	if OrderStatus(order.Status) == OrderSucceeded && order.BizKind != store.OrderBizKindSubscribe {
		return fmt.Errorf("%w: order %d biz_kind %s", ErrRefundManualOnly, order.ID, order.BizKind)
	}
	return nil
}

// checkSubscriptionRefundEligible 应用 R-35 的资格判定，并在需要时确认订阅
// 行存在。
//
// refund_required 订单是「钱已收、履约失败」：没有交付，就不存在「已消耗/
// 有效」的判定对象，因此跳过资格检查（R-30）。refund_failed 的来源有两种：
//   - 由 succeeded 进入：说明这是一笔已履约的购买，重试时仍要重新核对资格
//     （订阅可能在第一次失败后被消费）；
//   - 由 refund_required 进入：只可能来自 change（S2）或 renew（S4/R-43），
//     两者都是「钱已收、未履约」，没有可消耗的有效期，继续跳过。
func (s *TopupService) checkSubscriptionRefundEligible(ctx context.Context,
	db *gorm.DB, order *store.Order, now time.Time) error {

	status := OrderStatus(order.Status)
	if status == OrderRefundRequired {
		return nil
	}
	if status == OrderRefundFailed &&
		(order.BizKind == store.OrderBizKindChange || order.BizKind == store.OrderBizKindRenew) {
		return nil
	}

	sub, err := loadRefundSubscription(ctx, db, order, false)
	if err != nil {
		if errors.Is(err, store.ErrSubscriptionNotFound) {
			return fmt.Errorf("%w: order %d has no subscription to check", ErrRefundNotSupported, order.ID)
		}
		return err
	}
	if err := subscriptionRefundEligible(sub, now); err != nil {
		return err
	}
	return nil
}

// subscriptionRefundEligible 是 R-35 的唯一资格判定：
// `amount_used == 0` 且 `status == 'active'` 且未过期（`end_at > now`）。
//
// 【为什么不是 start_at > now】订阅只在结算时以**当前时刻**创建
// （subscription_purchase.go：start_at = now），系统没有预约生效的实例，
// renew/change 只改 end_at。因此「start_at 在未来」在生产不可达，按字面
// 条件会让任何购买完成后的 succeeded 订单永远退不了。R-35 裁决改为
// 「有效期内未消费即可退」，风险由「退款端点是 settings:manage 管理员动作、
// 用户侧只写申请标记」控制。
func subscriptionRefundEligible(sub *store.Subscription, now time.Time) error {
	if sub.Status != "active" || sub.AmountUsed != 0 || !sub.EndAt.After(now) {
		return fmt.Errorf("%w: subscription %d status=%s amount_used=%d end_at=%s",
			ErrSubscriptionInUse, sub.ID, sub.Status, sub.AmountUsed,
			sub.EndAt.UTC().Format(time.RFC3339))
	}
	return nil
}

// loadRefundSubscription 找到订单对应的订阅实例。
//
// subscribe 订单通过 subscriptions.order_id（UNIQUE）反查；renew/change 订单的
// biz_ref_id 就是实例 id。forUpdate=true 时对实例行加锁——钱包退款与 P-13
// 判定必须在同一行锁内完成，否则并发的预扣会改变 amount_used。
func loadRefundSubscription(ctx context.Context, db *gorm.DB, order *store.Order,
	forUpdate bool) (*store.Subscription, error) {

	query := db.WithContext(ctx)
	if forUpdate {
		query = query.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var sub store.Subscription
	var err error
	switch order.BizKind {
	case store.OrderBizKindSubscribe:
		err = query.Where("order_id = ?", order.ID).First(&sub).Error
	case store.OrderBizKindRenew, store.OrderBizKindChange:
		if order.BizRefID == nil {
			return nil, fmt.Errorf("%w: order %d has no biz_ref_id", store.ErrSubscriptionNotFound, order.ID)
		}
		err = query.Where("id = ?", *order.BizRefID).First(&sub).Error
	default:
		return nil, fmt.Errorf("%w: order %d has biz_kind %q", store.ErrSubscriptionNotFound, order.ID, order.BizKind)
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("%w: order %d", store.ErrSubscriptionNotFound, order.ID)
	}
	if err != nil {
		return nil, fmt.Errorf("billing: load subscription for order %d: %w", order.ID, err)
	}
	return &sub, nil
}

// ---------------------------------------------------------------------------
// 钱包退款（本地、不依赖渠道）
// ---------------------------------------------------------------------------

// refundWalletOrder 在单个事务内完成「订单 refunded + 钱包回补 + 订阅取消 +
// 退款事件」。
//
// 它同时承担 0 元订单（免费套餐）的退款：没有资金需要移动，也不存在渠道
// 交易号；walletRefundCredits 对 amount==0 直接返回 0，跳过流水写入。
//
// 回补积分取自原始 subscription_purchase 流水的**实际扣减额**，而不是按当前
// quota_per_unit 重算：QPU 是站点配置，购买后可能被调整，重算会在汇率边界
// 上多退/少退。原始流水同时是「哪些钱确实是这笔订单扣的」的唯一证据。
func (s *TopupService) refundWalletOrder(ctx context.Context,
	orderID int64, reason string, actorID int64) (*RefundResult, error) {

	var (
		refunded bool
		amount   int64
	)
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		order, err := lockOrderRow(ctx, tx, orderID)
		if err != nil {
			return err
		}
		if OrderStatus(order.Status) == OrderRefunded {
			// 幂等命中：事务外会重新读取订单并返回既有结果。
			return nil
		}
		if !refundableOrderStatus(order.Status) || !refundableBizKind(order.BizKind) {
			return fmt.Errorf("%w: order %d is %s", ErrOrderStateConflict, order.ID, order.Status)
		}
		sub, eligibilityErr := s.lockedRefundEligibility(ctx, tx, order, s.now().UTC())
		if eligibilityErr != nil {
			return eligibilityErr
		}
		// R-36：只有全额退还「已履约购买」时才收回权益；refund_required
		// 退的是 change 订单的差价，指向的是用户仍在使用的原订阅，绝不能取消。
		cancelSubscription := shouldCancelSubscriptionAfterRefund(order)

		credits, err := walletRefundCredits(ctx, tx, order)
		if err != nil {
			return err
		}
		moved, err := markOrderRefundedTx(tx, order.ID, OrderStatus(order.Status), reason)
		if err != nil {
			return err
		}
		if !moved {
			// 行已在锁外被并发退款（正常不会发生：本事务已 SELECT FOR UPDATE）：
			// 绝不再回补钱包/再动订阅；事务外重新读取即可。
			return nil
		}
		if credits > 0 {
			// type='reversal' 是「已入账资金的反向调整」；reference_type='order'
			// + reference_id 让同一订单至多一行（DB 部分唯一索引兜底重复退款）。
			if _, _, err := applyDeltaTxNote(ctx, tx, order.UserID, credits,
				"reversal", "confirmed", "order", strconv.FormatInt(order.ID, 10),
				truncateNote("subscription refund: "+reason)); err != nil {
				if errors.Is(err, ErrWalletLimit) {
					return fmt.Errorf("%w: order %d", ErrRefundWalletLimit, order.ID)
				}
				return fmt.Errorf("billing: refund wallet for order %d: %w", order.ID, err)
			}
		}
		if err := finalizeSubscriptionRefundTx(ctx, tx, order, sub, reason, cancelSubscription); err != nil {
			return err
		}
		amount = order.Amount
		refunded = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	fresh, err := s.GetOrder(ctx, orderID)
	if err != nil {
		return nil, err
	}
	if !refunded {
		// 幂等命中：Result.Amount 也返回既有退款额，而不是 0。
		amount = fresh.RefundedAmount
	}
	s.log.Info("subscription refund completed",
		"order_id", orderID, "amount", amount, "provider", fresh.Provider, "actor", actorID)
	return &RefundResult{Order: fresh, Refunded: refunded, Amount: amount}, nil
}

// walletRefundCredits 返回应回补的积分额（原始购买流水的扣减绝对值）。
//
// 0 元订单（免费套餐）没有扣款流水，返回 0；非 0 元订单必须恰好有一行
// subscription_purchase 流水——缺失或重复都说明数据损坏，fail closed，
// 绝不按订单金额临时换算出一笔"看起来合理"的退款。
func walletRefundCredits(ctx context.Context, tx *gorm.DB, order *store.Order) (int64, error) {
	if order.Amount == 0 {
		return 0, nil
	}
	var (
		total int64
		rows  int64
	)
	if err := tx.WithContext(ctx).Raw(`
		SELECT COALESCE(SUM(-amount), 0), COUNT(*)
		  FROM wallet_transactions
		 WHERE user_id = ? AND type = 'subscription_purchase'
		   AND reference_type = 'order' AND reference_id = ?`,
		order.UserID, strconv.FormatInt(order.ID, 10)).Row().Scan(&total, &rows); err != nil {
		return 0, fmt.Errorf("billing: sum wallet purchase for order %d: %w", order.ID, err)
	}
	if rows != 1 || total <= 0 {
		return 0, fmt.Errorf("billing: order %d expected exactly one wallet debit, got %d rows totaling %d",
			order.ID, rows, total)
	}
	return total, nil
}

// ---------------------------------------------------------------------------
// 渠道退款
// ---------------------------------------------------------------------------

// refundProviderPayment 按渠道调用退款 API。所有渠道错误都以错误返回；
// 成功时不会在该函数内写任何数据库状态。
func (s *TopupService) refundProviderPayment(ctx context.Context,
	order *store.Order, reason string) error {

	switch order.Provider {
	case ProviderStripe:
		return s.refundStripePayment(ctx, order)
	case ProviderAlipay:
		return s.refundAlipayTrade(ctx, order, reason)
	case ProviderEpay:
		// 易支付协议没有退款 API（与主动查询/关单并列的能力缺口）。
		return fmt.Errorf("%w: %s has no refund API", ErrProviderActionUnsupported, ProviderEpay)
	default:
		return fmt.Errorf("%w: provider %s", ErrProviderDisabled, order.Provider)
	}
}

// refundStripePayment 调用 POST /v1/refunds。
//
// Idempotency-Key 固定为 refund-order-<id>：同一订单的所有重试在 Stripe
// 侧收敛到同一笔退款。金额用订单金额（分），即创建 PaymentIntent 时的金额；
// Stripe 不允许退款额超过实收，渠道会用 4xx 拒绝（→ provider_rejected）。
func (s *TopupService) refundStripePayment(ctx context.Context, order *store.Order) error {
	if order.ProviderTxnID == nil || strings.TrimSpace(*order.ProviderTxnID) == "" {
		return fmt.Errorf("%w: order %d has no provider transaction id", ErrProviderUnavailable, order.ID)
	}
	secret, err := s.configSecret(ConfigStripeSecretKey)
	if err != nil {
		return err
	}
	form := url.Values{}
	form.Set("payment_intent", *order.ProviderTxnID)
	form.Set("amount", strconv.FormatInt(order.Amount, 10))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.stripeBaseURL+"/v1/refunds", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Idempotency-Key", fmt.Sprintf("%s%d", stripeRefundIdempotencyPrefix, order.ID))
	resp, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: stripe refund request", ErrProviderUnavailable)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("%w: read stripe refund response", ErrProviderUnavailable)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &providerHTTPError{provider: "stripe", status: resp.StatusCode}
	}
	var parsed struct {
		ID       string `json:"id"`
		Status   string `json:"status"`
		Amount   int64  `json:"amount"`
		Currency string `json:"currency"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || parsed.ID == "" {
		return fmt.Errorf("%w: parse stripe refund response", ErrProviderUnavailable)
	}
	// Stripe 的退款状态：pending / requires_action / succeeded / failed /
	// canceled。pending 与 requires_action 已被受理、不会重复扣款，按成功
	// 处理；failed/canceled 明确失败，置 refund_failed 等重试。
	switch parsed.Status {
	case "pending", "requires_action", "succeeded":
	case "failed", "canceled":
		return fmt.Errorf("%w: stripe refund %s is %s", ErrProviderUnavailable, parsed.ID, parsed.Status)
	default:
		return fmt.Errorf("%w: stripe refund %s has status %q", ErrProviderUnavailable, parsed.ID, parsed.Status)
	}
	// 金额核对：渠道返回的退款额必须与订单一致。不一致说明退款对象/参数
	// 错位，不能把本地订单标记成已全额退款。
	if parsed.Amount != 0 && parsed.Amount != order.Amount {
		return fmt.Errorf("%w: stripe refund %s amount %d != order amount %d",
			ErrProviderUnavailable, parsed.ID, parsed.Amount, order.Amount)
	}
	return nil
}

// refundAlipayTrade 调用 alipay.trade.refund。
//
// out_request_no 固定为 refund-<id>：支付宝按 (out_trade_no, out_request_no)
// 对退款请求幂等，重复请求返回同一笔退款结果（fund_change=N），这是 R-32
// 在渠道侧的锚点。只有 CNY 订单能退——支付宝按人民币收款，跨币种订单本来
// 就不可能由该渠道产生。
func (s *TopupService) refundAlipayTrade(ctx context.Context,
	order *store.Order, reason string) error {

	if order.ProviderTxnID == nil || strings.TrimSpace(*order.ProviderTxnID) == "" {
		return fmt.Errorf("%w: order %d has no provider transaction id", ErrProviderUnavailable, order.ID)
	}
	if order.Currency != AlipayCurrency {
		return fmt.Errorf("%w: alipay refund on %s order", ErrProviderUnavailable, order.Currency)
	}
	privateKey, err := s.configSecret(ConfigAlipayPrivateKey)
	if err != nil {
		return err
	}
	publicKey, err := s.configSecret(ConfigAlipayPublicKey)
	if err != nil {
		return err
	}
	biz := map[string]string{
		"out_trade_no":   *order.ProviderTxnID,
		"refund_amount":  centsToYuanString(order.Amount),
		"out_request_no": fmt.Sprintf("%s%d", alipayRefundRequestPrefix, order.ID),
	}
	if reason != "" {
		// 支付宝 refund_reason 上限 256 字符；复用已校验的 1–200 字符原因。
		biz["refund_reason"] = reason
	}
	raw, err := json.Marshal(biz)
	if err != nil {
		return err
	}
	node, err := s.alipayCall(ctx, "alipay.trade.refund", string(raw), privateKey, publicKey)
	if err != nil {
		return err
	}
	var parsed struct {
		Code       string `json:"code"`
		FundChange string `json:"fund_change"`
		RefundFee  string `json:"refund_fee"`
	}
	if err := json.Unmarshal(node, &parsed); err != nil {
		return fmt.Errorf("%w: parse alipay refund response", ErrProviderUnavailable)
	}
	if parsed.Code != "10000" {
		// 业务拒绝（参数/交易状态不允许退款）= 4xx 类别，重试不会成功；
		// 仍置 refund_failed 让人工介入，而不是把它藏成「渠道不可用」。
		return &providerHTTPError{provider: "alipay", status: http.StatusBadRequest}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 落库
// ---------------------------------------------------------------------------

// finalizeProviderRefund 在渠道退款成功后写本地事实。
//
// 渠道侧已经出账，本地这一步只做「记录 + 收回权益」：重新锁定订单，若并发
// 请求已经把它标记为 refunded 则直接返回（渠道幂等键保证没有第二笔退款）。
// 任何失败都会让调用方返回 500，但钱已在渠道退回——这是有意的偏置：宁可
// 让人工补账，也不能在渠道已退款后把订单留在 refund_failed 诱导二次退款。
func (s *TopupService) finalizeProviderRefund(ctx context.Context,
	orderID int64, reason string, actorID int64) (*RefundResult, error) {

	var (
		refunded bool
		amount   int64
	)
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		order, err := lockOrderRow(ctx, tx, orderID)
		if err != nil {
			return err
		}
		if OrderStatus(order.Status) == OrderRefunded {
			// 幂等命中：渠道幂等键保证没有第二笔退款。
			return nil
		}
		if !refundableOrderStatus(order.Status) || !refundableBizKind(order.BizKind) {
			return fmt.Errorf("%w: order %d is %s", ErrOrderStateConflict, order.ID, order.Status)
		}
		sub, err := loadRefundSubscription(ctx, tx, order, true)
		if err != nil && !errors.Is(err, store.ErrSubscriptionNotFound) {
			return err
		}
		if errors.Is(err, store.ErrSubscriptionNotFound) {
			// 渠道已经退款成功，本地绝不能因为「找不到实例」就拒绝落库——
			// 那会把钱已退回的订单留在 succeeded/refund_failed，诱导二次退款。
			// 缺实例时只告警，事件随之缺省。
			s.log.Warn("refunded order has no subscription; recording refund without event",
				"order_id", order.ID)
			sub = nil
		}
		cancelSubscription := shouldCancelSubscriptionAfterRefund(order)
		moved, err := markOrderRefundedTx(tx, order.ID, OrderStatus(order.Status), reason)
		if err != nil {
			return err
		}
		if !moved {
			// 并发请求已完成退款：不重复写订阅/事件；事务外重新读取。
			return nil
		}
		if err := finalizeSubscriptionRefundTx(ctx, tx, order, sub, reason, cancelSubscription); err != nil {
			return err
		}
		amount = order.Amount
		refunded = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	fresh, err := s.GetOrder(ctx, orderID)
	if err != nil {
		return nil, err
	}
	if !refunded {
		// 幂等命中：Result.Amount 也返回既有退款额，而不是 0。
		amount = fresh.RefundedAmount
	}
	s.log.Info("subscription refund completed",
		"order_id", orderID, "amount", amount, "provider", fresh.Provider, "actor", actorID)
	return &RefundResult{Order: fresh, Refunded: refunded, Amount: amount}, nil
}

// lockedRefundEligibility 是钱包事务内版本的资格判定：R-35 判定与实例行锁
// 一起完成，避免「刚通过检查、立刻被并发消费」的窗口。
func (s *TopupService) lockedRefundEligibility(ctx context.Context,
	tx *gorm.DB, order *store.Order, now time.Time) (*store.Subscription, error) {

	status := OrderStatus(order.Status)
	sub, err := loadRefundSubscription(ctx, tx, order, true)
	if err != nil {
		if errors.Is(err, store.ErrSubscriptionNotFound) && status == OrderRefundRequired {
			return nil, nil // 履约失败订单无需实例
		}
		return nil, err
	}
	if status == OrderRefundRequired {
		return sub, nil
	}
	if status == OrderRefundFailed && order.BizKind == store.OrderBizKindChange {
		return sub, nil
	}
	if err := subscriptionRefundEligible(sub, now); err != nil {
		return nil, err
	}
	return sub, nil
}

// lockOrderRow 用 SELECT ... FOR UPDATE 锁定订单行。
func lockOrderRow(ctx context.Context, tx *gorm.DB, orderID int64) (*store.Order, error) {
	var order store.Order
	err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ?", orderID).First(&order).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("%w: order %d", ErrOrderNotFound, orderID)
	}
	if err != nil {
		return nil, fmt.Errorf("billing: lock order %d: %w", orderID, err)
	}
	return &order, nil
}

// markOrderRefundedTx 把订单从任一可退状态条件迁移到 refunded。
//
// M-1：落库前显式过应用层状态机（Transition），让 order.go 的退款白名单
// 有真实调用者；条件 UPDATE 仍是并发闸门，两者表达同一契约。
//
// 返回值 moved=false 表示并发请求已经把订单置为 refunded（本事务没有写入）。
// refunded_amount 与状态同一条 UPDATE 写入，满足 R-31 的「同一事务」要求。
// M-3：这里刻意**不**写 refund_requested_at——那是「用户申请过」的事实，
// 只有 RequestSubscriptionRefund 能写；管理员直接退款不应伪装成用户申请。
func markOrderRefundedTx(tx *gorm.DB, orderID int64, from OrderStatus, reason string) (bool, error) {
	if err := Transition(from, OrderRefunded); err != nil {
		return false, err
	}
	res := tx.Exec(`
		UPDATE orders
		SET status = 'refunded', refunded_amount = amount,
		    refunded_at = now(), refund_reason = ?, failure_code = NULL,
		    updated_at = now()
		WHERE id = ? AND status IN ('succeeded', 'refund_required', 'refund_failed')`,
		reason, orderID)
	if res.Error != nil {
		return false, fmt.Errorf("billing: mark order %d refunded: %w", orderID, res.Error)
	}
	if res.RowsAffected > 0 {
		return true, nil
	}
	var status string
	if err := tx.Raw(`SELECT status FROM orders WHERE id = ?`, orderID).Row().Scan(&status); err != nil {
		return false, fmt.Errorf("billing: re-read order %d after refund: %w", orderID, err)
	}
	if status == string(OrderRefunded) {
		return false, nil
	}
	return false, fmt.Errorf("%w: order %d is %s", ErrOrderStateConflict, orderID, status)
}

// shouldCancelSubscriptionAfterRefund 决定退款成功后是否收回订阅权益（R-36/R-38）。
//
// R-38 已把 succeeded 的自动退款限定为 subscribe 订单，因此这里返回 true 的
// 只有「全额退还已履约购买」。refund_required 的 change 订单退的是**差价**，
// 其 biz_ref_id 指向用户仍在使用、仍在履约的原订阅；取消它等于把用户已经
// 付过钱、正在使用的服务一并作废。
func shouldCancelSubscriptionAfterRefund(order *store.Order) bool {
	switch OrderStatus(order.Status) {
	case OrderSucceeded, OrderRefundFailed:
		// 只有「已履约的购买（subscribe）」全额退款才收回权益并取消订阅。
		// refund_failed 可能来自 succeeded（subscribe）或 refund_required
		// （change/renew）：后者的钱对应一次未履约的变更/续费，取消整条
		// 订阅会误伤用户仍在使用的周期（R-36/R-46）。
		return order.BizKind == store.OrderBizKindSubscribe
	default:
		return false
	}
}

// markOrderRefundInProgress 在调用渠道前写入「退款进行中」失败码（R-37）。
//
// 它不改变订单状态（仍是 succeeded/refund_failed），只覆盖 failure_code；
// 成功 finalize 清空、失败改写为 provider_*。并发退款已成功时 affected=0，
// 上层会在渠道调用后通过幂等 finalize 收敛，绝不在这里报错阻断。
func (s *TopupService) markOrderRefundInProgress(ctx context.Context, orderID int64) error {
	res := s.db.WithContext(ctx).Exec(`
		UPDATE orders
		SET failure_code = 'refund_in_progress', updated_at = now()
		WHERE id = ? AND status IN ('succeeded', 'refund_required', 'refund_failed')`, orderID)
	if res.Error != nil {
		return fmt.Errorf("billing: mark order %d refund_in_progress: %w", orderID, res.Error)
	}
	return nil
}

// finalizeSubscriptionRefundTx 在退款成功的事务内追 refunded 事件（R-34），
// 并按 cancel 决定是否收回订阅权益（R-36）。
func finalizeSubscriptionRefundTx(ctx context.Context, tx *gorm.DB,
	order *store.Order, sub *store.Subscription, reason string, cancel bool) error {

	if sub != nil && cancel && sub.Status == "active" {
		// 全额退还未消费的有效期订阅：立即取消（不是周期末取消，因为钱已退）。
		res := tx.Exec(`
			UPDATE subscriptions
			SET status = 'cancelled', cancel_at_period_end = false, updated_at = now()
			WHERE id = ? AND status = 'active'`, sub.ID)
		if res.Error != nil {
			return fmt.Errorf("billing: cancel refunded subscription %d: %w", sub.ID, res.Error)
		}
	}
	if sub == nil {
		return nil
	}
	// R-34：detail 只允许 order_id / amount / reason（金额是订单分单位）。
	// 事件的 seq/行锁由 store.AppendSubscriptionEvent 统一处理。
	return store.AppendSubscriptionEvent(ctx, tx.Statement.ConnPool, sub.ID,
		store.SubscriptionEventRefunded, map[string]any{
			"order_id": order.ID,
			"amount":   order.Amount,
			"reason":   reason,
		})
}

// markOrderRefundFailed 把渠道退款失败写入订单（R-32）。
//
// 幂等/并发：条件 UPDATE 只从可退状态迁移；若并发请求已退款成功，这里
// affected=0 且不改变 refunded 终态。失败只记日志，不覆盖调用方要返回的
// 渠道错误——「本地没能记录失败」不应让管理员以为钱没退。
func (s *TopupService) markOrderRefundFailed(ctx context.Context, orderID int64, cause error) {
	res := s.db.WithContext(ctx).Exec(`
		UPDATE orders
		SET status = 'refund_failed', failure_code = ?, updated_at = now()
		WHERE id = ? AND status IN ('succeeded', 'refund_required', 'refund_failed')`,
		refundFailureCode(cause), orderID)
	if res.Error != nil {
		s.log.Error("mark order refund_failed failed", "order_id", orderID, "error", res.Error)
		return
	}
	if res.RowsAffected == 0 {
		s.log.Warn("order was not marked refund_failed (concurrent state change)",
			"order_id", orderID)
	}
}

// refundFailureCode 把渠道错误映射到 orders.failure_code 的冻结值集。
// 4xx（渠道明确拒绝：金额超实收、交易状态不允许）→ provider_rejected；
// 网络/5xx/响应不可解析 → provider_unavailable（可重试）。
func refundFailureCode(err error) string {
	var httpErr *providerHTTPError
	if errors.As(err, &httpErr) && httpErr.status >= 400 && httpErr.status < 500 {
		return "provider_rejected"
	}
	return "provider_unavailable"
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

// normalizeRefundReason 归一化并校验退款原因（1–200 字符、无控制字符）。
// 空白原因不可接受：R-34 要求事件里的 reason 能解释资金动作。
func normalizeRefundReason(raw string) (string, error) {
	reason := strings.TrimSpace(raw)
	if reason == "" || utf8.RuneCountInString(reason) > refundReasonMaxRunes ||
		hasControlChars(reason) {
		return "", fmt.Errorf("%w: reason must be 1-%d characters", ErrInvalidRefundReason, refundReasonMaxRunes)
	}
	return reason, nil
}

// truncateNote 让钱包流水备注不超过 200 字符（DB CHECK）。
func truncateNote(note string) string {
	runes := []rune(note)
	if len(runes) > 200 {
		return string(runes[:200])
	}
	return note
}
