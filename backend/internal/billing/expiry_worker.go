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
// 顺序刻意是「先尽力关单、再条件 UPDATE」：F-04-07 要求支付宝订单先调
// alipay.trade.close；关单失败也必须置 expired（异常流程原文），所以
// closeProviderOrder 只告警，绝不中断后面的 DB 迁移。
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
		Order("id").Find(&orders).Error
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
		// 渠道关闭是 best-effort：Topup 为 nil 的纯 DB 单测直接跳过。
		if topup != nil {
			topup.closeProviderOrder(ctx, order)
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

// AlipayTradePaid 报告查询状态是否表示资金已到账。TRADE_FINISHED 是交易
// 完结态（不可退款），与 TRADE_SUCCESS 一样必须入账——否则回调丢失且
// 交易已完结的订单会永远停在 pending。
func AlipayTradePaid(status string) bool {
	return status == "TRADE_SUCCESS" || status == "TRADE_FINISHED"
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
		Order("id").Find(&orders).Error
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
