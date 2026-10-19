package billing

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"gorm.io/gorm"

	"newtg/gateway/internal/store"
)

// 本文件是 M5 的订单过期回收与支付宝轮询兜底循环（modules-04 F-04-06 /
// F-04-07；裁决 P-1/P-5/P-20）。
//
// 边界（必须与兑换码明确区分）：
//   - 兑换码**没有**过期 worker：`expired` 是读时虚拟态
//     （platform-01 R11：status='unused' AND expires_at <= now()），绝不落库。
//     本循环只处理 orders，扫描不会触碰 redemption_codes（P-5）。
//   - 订单过期不新增 expires_at 列：到期由 created_at + OrderExpiry（30min）
//     派生，worker 与读路径共享同一真源（platform-01 §10）。
//   - 状态迁移只走条件 UPDATE `WHERE id=? AND status='pending'`：终态订单
//     永远不被改写，重复扫描天然幂等（P-1）。
//
// 失败语义（与 M3 的 worker 一致）：单轮扫描失败只记录并在下一轮重试；
// 渠道查询/关单失败绝不终止循环。循环由 ctx 取消优雅退出（返回 nil）。

const (
	// OrderExpiryScanInterval 是订单超时扫描周期（F-04-07：worker 每分钟）。
	// 冻结值而不是配置项：30 分钟有效期与分钟级回收是一组对账参数，
	// 运行期改动会让「过期」在进程间出现两个真源。
	OrderExpiryScanInterval = 60 * time.Second

	// AlipayPollInterval 是支付宝主动查询周期（F-04-06 主流程 4：每 2 分钟）。
	// 通知是主路径，轮询只是回调丢失兜底，因此不需要更密的扫描。
	AlipayPollInterval = 2 * time.Minute

	// AlipayPollMinAge 只轮询创建超过 5 分钟的订单（F-04-06 主流程 4）。
	// 更年轻的交易可能仍在用户扫码过程中，查询只会得到 WAIT_BUYER_PAY，
	// 白白放大渠道请求量与限流风险。
	AlipayPollMinAge = 5 * time.Minute

	// ExpiryScanBatchSize 是单轮扫描的确定性上限（订单过期与支付宝轮询
	// 共用）。为什么必须限量：两条扫描串行跑在同一 goroutine，积压或渠道
	// 变慢时单轮可能超过 60s/2min 的契约周期；time.Ticker 会静默丢 tick，
	// 周期就失去保证。LIMIT + ORDER BY(created_at, id) 让每轮工作量有界，
	// 未处理的行由下一轮继续（最旧优先，不会跳过或饿死新行以外的行）。
	ExpiryScanBatchSize = 200
)

// ExpiryWorkerDeps 是订单过期循环的依赖与可调参数。
//
// 零值字段使用冻结默认值：
//   - Now 零值 → time.Now；Logger 零值 → slog.Default()；
//   - OrderInterval 零值 → OrderExpiryScanInterval；
//   - AlipayInterval 零值 → AlipayPollInterval；
//   - OrderExpiry 零值 → OrderExpiry（30min）；AlipayMinAge 零值 → 5min；
//   - OrderScan / AlipayPoll 零值 → 绑定 DB/Topup 的生产扫描（此时两者必填）。
//
// 为什么不直接传 *config.Config：本循环的两个周期与 30 分钟有效期是契约
// 冻结值，不是环境变量；运行期配置会破坏「何时过期」的单一真源。
type ExpiryWorkerDeps struct {
	DB    *gorm.DB
	Topup *TopupService

	Logger *slog.Logger
	Now    func() time.Time

	OrderInterval  time.Duration
	AlipayInterval time.Duration
	OrderExpiry    time.Duration
	AlipayMinAge   time.Duration

	// OrderScan / AlipayPoll 是装配单测的接缝：注入 fake 后不需要 DB 或
	// 真实渠道即可验证「启动即扫 + 周期扫描 + ctx 取消退出」的接线。
	OrderScan  func(ctx context.Context, now time.Time) (int, error)
	AlipayPoll func(ctx context.Context, now time.Time) error
}

// RunExpiryLoop 运行订单过期回收与支付宝轮询，直到 ctx 取消。
//
// 启动时立即各扫一轮，之后按各自周期（默认 60s / 2min）运行。两个扫描
// 在同一 goroutine 的 select 中串行执行：单副本 worker（R-04）下没有并发
// 收益，串行还避免了「过期扫描与轮询同时命中同一订单」的窗口。
// 单轮错误只记录，不影响另一条扫描或下一轮。
func RunExpiryLoop(ctx context.Context, deps ExpiryWorkerDeps) error {
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	orderInterval := deps.OrderInterval
	if orderInterval <= 0 {
		orderInterval = OrderExpiryScanInterval
	}
	alipayInterval := deps.AlipayInterval
	if alipayInterval <= 0 {
		alipayInterval = AlipayPollInterval
	}
	expiry := deps.OrderExpiry
	if expiry <= 0 {
		expiry = OrderExpiry
	}
	minAge := deps.AlipayMinAge
	if minAge <= 0 {
		minAge = AlipayPollMinAge
	}

	orderScan := deps.OrderScan
	if orderScan == nil {
		if deps.DB == nil || deps.Topup == nil {
			return errors.New("billing: expiry loop requires DB and TopupService for the order scan")
		}
		orderScan = func(ctx context.Context, now time.Time) (int, error) {
			return ExpirePendingOrders(ctx, deps.DB, deps.Topup, now, expiry)
		}
	}
	alipayPoll := deps.AlipayPoll
	if alipayPoll == nil {
		if deps.DB == nil || deps.Topup == nil {
			return errors.New("billing: expiry loop requires DB and TopupService for the alipay poll")
		}
		alipayPoll = func(ctx context.Context, now time.Time) error {
			return PollPendingAlipayOrders(ctx, deps.DB, deps.Topup, now, minAge)
		}
	}

	// 已取消的上下文不再启动扫描：停机竞态下不产生新的 DB/渠道负载。
	if ctx.Err() != nil {
		return nil
	}

	scanOrders := func() {
		expired, err := orderScan(ctx, now())
		if err != nil {
			// DB 故障不能当成「没有过期订单」；下一轮重试。
			log.Error("order expiry scan failed", "error", err)
			return
		}
		if expired > 0 {
			log.Info("orders expired", "count", expired)
		}
	}
	pollAlipay := func() {
		if err := alipayPoll(ctx, now()); err != nil {
			// 渠道不可用是常态（超时/限流）；记录后继续下一轮。
			log.Warn("alipay order poll failed", "error", err)
		}
	}

	// 启动即扫：重启后不必等一个完整周期才回收积压订单。
	scanOrders()
	pollAlipay()

	orderTicker := time.NewTicker(orderInterval)
	defer orderTicker.Stop()
	alipayTicker := time.NewTicker(alipayInterval)
	defer alipayTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			// 优雅停机不是错误：与 M3 的 RunWorkerLoop 返回 nil 一致。
			return nil
		case <-orderTicker.C:
			scanOrders()
		case <-alipayTicker.C:
			pollAlipay()
		}
	}
}

// ExpirePendingOrders 扫描一轮超时 pending 订单并逐单迁移为 expired。
//
// 返回本次实际迁移的数量（affected>0 的订单数）。查询失败返回 error；
// 单个订单的关单失败或条件 UPDATE 落败只记录、不影响同轮其它订单。
//
// 每轮最多处理 ExpiryScanBatchSize 条（最旧优先），积压由后续轮次继续。
// 顺序是「支付宝先做最终收款核实 → 尽力关单 → 条件 UPDATE」：核实确认
// 已付款的订单走入账而不是过期；关单失败仍必须置 expired（F-04-07
// 异常流程原文），所以 closeProviderOrder 只告警，绝不中断 DB 迁移。
func ExpirePendingOrders(ctx context.Context, db *gorm.DB, topup *TopupService,
	now time.Time, expiry time.Duration) (int, error) {

	if db == nil {
		return 0, errors.New("billing: order expiry requires a database")
	}
	if expiry <= 0 {
		expiry = OrderExpiry
	}
	cutoff := now.Add(-expiry)

	var orders []store.Order
	err := db.WithContext(ctx).
		Where("status = ? AND created_at < ?", string(OrderPending), cutoff).
		Order("created_at").Order("id").
		Limit(ExpiryScanBatchSize).
		Find(&orders).Error
	if err != nil {
		return 0, err
	}

	expired := 0
	log := slog.Default()
	if topup != nil {
		log = topup.log
	}
	for i := range orders {
		order := &orders[i]

		// 资金安全（复审 Critical #3）：到期前对渠道订单做最后一次核实。
		// 用户可能在最后一个轮询间隙或渠道故障期间完成支付；一旦置 expired，
		// 入账与人工完成也都拒绝非 pending 订单——就会「扣款不到账且无补救
		// 路径」。支付宝与 Stripe 都必须先问渠道：
		//   paid/succeeded → 走同一幂等入账，绝不置 expired；
		//   已关闭/canceled → 置 expired；
		//   未付款/可取消 → 正常过期（先 best-effort 关单）；
		//   查询错误/processing → 本轮保持 pending，下一轮重试（fail-safe
		//   方向是钱）。
		disposition := expireAfterClose
		if topup != nil {
			switch order.Provider {
			case ProviderAlipay:
				disposition = topup.alipayExpiryDisposition(ctx, order)
			case ProviderStripe:
				disposition = topup.stripeExpiryDisposition(ctx, order)
			}
		}
		switch disposition {
		case keepPending:
			continue
		case expireAfterClose:
			// 渠道关闭是 best-effort：关闭失败也只告警，不阻塞 DB 迁移。
			if topup != nil {
				topup.closeProviderOrder(ctx, order)
			}
		case expireWithoutClose:
			// TRADE_CLOSED：渠道侧已是关闭态，重复 close 只会产生噪声。
		}

		res := db.WithContext(ctx).Exec(
			`UPDATE orders SET status = 'expired', updated_at = now()
			 WHERE id = ? AND status = 'pending'`, order.ID)
		if res.Error != nil {
			// 单行失败不阻断整轮：下一轮会重新命中该 pending 订单。
			log.Error("mark order expired failed", "order_id", order.ID, "error", res.Error)
			continue
		}
		// affected=0 说明并发回调/取消已先行迁移；终态不可改写（P-1），
		// 静默跳过而不是把已成功的订单降级为 expired。
		if res.RowsAffected > 0 {
			expired++
		}
	}
	return expired, nil
}

// expiryDisposition 是「到期时如何处理该订单」的三态判定结果。
type expiryDisposition int

const (
	// expireAfterClose：未付款/未关闭的订单——先 best-effort 关单再置 expired。
	expireAfterClose expiryDisposition = iota
	// expireWithoutClose：渠道已明确 TRADE_CLOSED——直接置 expired。
	expireWithoutClose
	// keepPending：金额已支付但入账未完成，或渠道状态不可判定——本轮不动，
	// 绝不能用「过期」切断唯一的资金补救路径。
	keepPending
)

// alipayExpiryDisposition 在订单到期前做最后一次渠道核实（复审 Important #1）。
//
// 这不是「正常轮询」的重复：轮询只覆盖 created_at 在 5–30 分钟窗口的订单，
// 而付款可能恰好发生在最后一个轮询 tick 与 30 分钟扫描之间，或发生在渠道
// 查询故障期间。到期扫描是最后一道防线，必须在写 expired 前确认渠道确实
// 未收款。
func (s *TopupService) alipayExpiryDisposition(ctx context.Context, order *store.Order) expiryDisposition {
	if order.ProviderTxnID == nil || strings.TrimSpace(*order.ProviderTxnID) == "" {
		// 没有渠道单号就无法核实；宁可保持 pending 等人工排查，也不在
		// 可能已扣款的情况下关闭订单。
		s.log.Warn("expiry skipped: alipay order has no provider txn id", "order_id", order.ID)
		return keepPending
	}
	txn := *order.ProviderTxnID
	trade, err := s.QueryAlipayTrade(ctx, txn)
	if err != nil {
		// 渠道故障：不确认收款就不能关闭（fail-safe 方向是钱）。
		// 下一轮扫描会重试；若渠道恢复后确认已付，则走同一入账路径。
		s.log.Warn("alipay expiry pre-check failed; keeping order pending",
			"order_id", order.ID, "error", err)
		return keepPending
	}
	switch {
	case AlipayTradePaid(trade.TradeStatus):
		cents, err := yuanToCents(trade.TotalAmount)
		if err != nil {
			// 已付款但金额不可解析：保持 pending 等待人工核对，
			// 绝不按订单金额猜测入账，也绝不置 expired。
			s.log.Error("alipay paid order has unparsable amount; keeping pending",
				"order_id", order.ID, "error", err)
			return keepPending
		}
		raw := marshalWhitelisted(map[string]any{
			"provider":     ProviderAlipay,
			"source":       "expiry_precheck",
			"out_trade_no": txn,
			"trade_status": trade.TradeStatus,
			"total_amount": trade.TotalAmount,
		})
		// 与 webhook/轮询同一条幂等入账路径：条件 UPDATE + ApplyTx。
		// 即使 settle 失败（如并发已入账/瞬时 DB 错误）也不置 expired：
		// 钱的路径优先，下一轮扫描会再次核实。
		if err := s.settleOrder(ctx, order, cents, AlipayCurrency, raw); err != nil {
			s.log.Error("alipay expiry pre-check settle failed; keeping order pending",
				"order_id", order.ID, "error", err)
		}
		return keepPending
	case trade.TradeStatus == "TRADE_CLOSED":
		return expireWithoutClose
	default:
		// WAIT_BUYER_PAY 等：渠道确认未收款，可以安全过期。
		return expireAfterClose
	}
}

// AlipayTradePaid 报告查询状态是否表示资金已到账。TRADE_FINISHED 是交易
// 完结态（不可退款），与 TRADE_SUCCESS 一样必须入账——否则回调丢失且
// 交易已完结的订单会永远停在 pending。
func AlipayTradePaid(status string) bool {
	return status == "TRADE_SUCCESS" || status == "TRADE_FINISHED"
}

// stripeExpiryDisposition 在 Stripe 订单到期前查询 PaymentIntent。
//
// Stripe 的 succeeded 回调可能丢投或晚于 30 分钟：用户实际已付款，但
// webhook 没到。worker 若直接 cancel + expired，后续回调、主动核实与
// 人工完成都会因「非 pending」拒绝，形成永久扣款不到账。因此：
//   - succeeded → settleOrder（与 webhook 同一条幂等入账路径），keepPending；
//   - processing / 查询错误 → keepPending：钱可能正在清算，过期不可逆；
//   - requires_payment_method / canceled / requires_confirmation →
//     best-effort cancel 后过期（资金确定未到）；
//   - 其它未知状态（requires_action/requires_capture 等）→ keepPending，
//     宁可等人工核对也不切断资金路径。
func (s *TopupService) stripeExpiryDisposition(ctx context.Context, order *store.Order) expiryDisposition {
	if order.ProviderTxnID == nil || strings.TrimSpace(*order.ProviderTxnID) == "" {
		// 没有 PI 号无法核实；宁可保持 pending 等人工排查，也不在
		// 可能已扣款的情况下关闭订单。
		s.log.Warn("expiry skipped: stripe order has no provider txn id", "order_id", order.ID)
		return keepPending
	}
	txn := *order.ProviderTxnID
	intent, err := s.QueryStripePaymentIntent(ctx, txn)
	if err != nil {
		// 渠道故障：不确认收款就不能关闭（fail-safe 方向是钱）。
		// 下一轮扫描会重试；若渠道恢复后确认已付，则走同一入账路径。
		s.log.Warn("stripe expiry pre-check failed; keeping order pending",
			"order_id", order.ID, "error", err)
		return keepPending
	}
	switch intent.Status {
	case "succeeded":
		amount := intent.AmountReceived
		if amount == 0 {
			amount = intent.Amount
		}
		raw := marshalWhitelisted(map[string]any{
			"provider":        ProviderStripe,
			"source":          "expiry_precheck",
			"provider_txn_id": intent.ID,
			"status":          intent.Status,
			"amount":          amount,
			"amount_received": intent.AmountReceived,
			"currency":        strings.ToUpper(intent.Currency),
		})
		// 与 webhook/verify 同一条幂等入账路径：条件 UPDATE + ApplyTx。
		// 即使 settle 失败（并发已入账/瞬时 DB 错误）也不置 expired：
		// 钱的路径优先，下一轮扫描会再次核实。
		if err := s.settleOrder(ctx, order, amount, strings.ToUpper(intent.Currency), raw); err != nil {
			s.log.Error("stripe expiry pre-check settle failed; keeping order pending",
				"order_id", order.ID, "error", err)
		}
		return keepPending
	case "processing":
		// 清算中：可能马上 succeeded，绝不能过期。
		s.log.Info("stripe payment intent still processing; keeping order pending",
			"order_id", order.ID)
		return keepPending
	case "requires_payment_method", "canceled", "requires_confirmation":
		// 资金确定未到账（或交易已取消），可以安全关单并过期。
		return expireAfterClose
	default:
		s.log.Warn("stripe payment intent in unrecognized state; keeping order pending",
			"order_id", order.ID, "status", intent.Status)
		return keepPending
	}
}

// PollPendingAlipayOrders 扫描一轮创建超过 minAge 的 pending 支付宝订单并
// 主动查询渠道（F-04-06 主流程 4，P-20）。
//
// 分支：
//   - 已付款 → settleOrder（与 webhook/verify 完全相同的幂等入账路径）；
//   - TRADE_CLOSED → 条件 UPDATE 为 expired；
//   - 仍待支付 → 保持 pending，下一轮再查；
//   - 渠道查询失败 → 记录并继续下一个订单，绝不终止循环。
func PollPendingAlipayOrders(ctx context.Context, db *gorm.DB, topup *TopupService,
	now time.Time, minAge time.Duration) error {

	if db == nil {
		return errors.New("billing: alipay poll requires a database")
	}
	if topup == nil {
		return errors.New("billing: alipay poll requires a topup service")
	}
	if minAge <= 0 {
		minAge = AlipayPollMinAge
	}

	// 沙箱交易没有真实资金，查询结果同样是 TRADE_SUCCESS。通知路径已经
	// 显式拒绝沙箱入账（security-06）；轮询如果放行就等于绕过该闸门。
	if topup.configBool(ConfigAlipaySandbox, false) {
		topup.log.Debug("alipay polling skipped: sandbox mode")
		return nil
	}

	cutoff := now.Add(-minAge)
	var orders []store.Order
	err := db.WithContext(ctx).
		Where("status = ? AND provider = ? AND created_at < ?",
			string(OrderPending), ProviderAlipay, cutoff).
		Order("created_at").Order("id").
		Limit(ExpiryScanBatchSize).
		Find(&orders).Error
	if err != nil {
		return err
	}

	for i := range orders {
		order := &orders[i]
		if order.ProviderTxnID == nil || strings.TrimSpace(*order.ProviderTxnID) == "" {
			// precreate 前分配的单号理论上在 pending 订单上必然存在；
			// 缺失说明历史数据异常，记录后跳过（不能拿空单号查询）。
			topup.log.Warn("pending alipay order has no provider txn id", "order_id", order.ID)
			continue
		}
		txn := *order.ProviderTxnID
		trade, err := topup.QueryAlipayTrade(ctx, txn)
		if err != nil {
			// 渠道超时/限流/签名不可用：只影响本单，本轮其余订单继续。
			topup.log.Warn("alipay trade query failed", "order_id", order.ID, "error", err)
			continue
		}

		switch {
		case AlipayTradePaid(trade.TradeStatus):
			cents, err := yuanToCents(trade.TotalAmount)
			if err != nil {
				topup.log.Warn("alipay poll returned unparsable amount",
					"order_id", order.ID, "error", err)
				continue
			}
			raw := marshalWhitelisted(map[string]any{
				"provider":     ProviderAlipay,
				"source":       "poll",
				"out_trade_no": txn,
				"trade_status": trade.TradeStatus,
				"total_amount": trade.TotalAmount,
			})
			// settleOrder 内部用条件 UPDATE + ApplyTx：与 webhook 并发时
			// 只有一方能入账，另一方回读成功状态，绝不重复加余额。
			if err := topup.settleOrder(ctx, order, cents, AlipayCurrency, raw); err != nil {
				// 金额不符会走 failed；并发/配置错误只记录，下一轮重试。
				topup.log.Error("alipay poll settle failed",
					"order_id", order.ID, "error", err)
			}
		case trade.TradeStatus == "TRADE_CLOSED":
			res := db.WithContext(ctx).Exec(
				`UPDATE orders SET status = 'expired', updated_at = now()
				 WHERE id = ? AND status = 'pending'`, order.ID)
			if res.Error != nil {
				topup.log.Error("mark closed alipay order expired failed",
					"order_id", order.ID, "error", res.Error)
				continue
			}
			if res.RowsAffected > 0 {
				topup.log.Info("alipay order closed at provider", "order_id", order.ID)
			}
		default:
			// WAIT_BUYER_PAY 等中间态：保持 pending，等通知或下一轮轮询。
		}
	}
	return nil
}
