package billing

import "errors"

// 订单状态机（modules-04 §3；裁决 P-1 / S2 R-29② / S3 R-30–R-33）。
//
// 枚举与数据库 CHECK 完全一致：pending / succeeded / failed / expired /
// cancelled / refund_required / refunded / refund_failed。
// `refund_required` 由 S2/R-29② 引入（迁移 20261021130000）；`refunded` 与
// `refund_failed` 由 S3 退款流程引入（迁移 20261022090000）。
// `paid` 是架构 spec 的历史别名，映射为 succeeded。
type OrderStatus string

const (
	// OrderPending 是唯一非终态：订单已创建、等待支付确认。
	OrderPending OrderStatus = "pending"
	// OrderSucceeded 表示支付已确认且余额已入账（paid 的映射目标）。
	OrderSucceeded OrderStatus = "succeeded"
	OrderFailed    OrderStatus = "failed"
	// OrderExpired 表示超过 30 分钟未支付被 worker 关闭。
	OrderExpired = "expired"
	// OrderCancelled 表示用户主动取消。
	OrderCancelled = "cancelled"
	// OrderRefundRequired 表示**钱已收、履约失败**（S2/R-29②：升降级支付成功后
	// 订阅已被并发取消/换套餐，迁移无法应用）。终态：不再接受回调重投，也不再
	// 自动迁移——留给退款流程（S3）人工处理。不是 pending，否则渠道会无限重投、
	// 每次 5xx，且永远不会成功。
	OrderRefundRequired OrderStatus = "refund_required"
	// OrderRefunded 表示退款已完成（S3/R-31）：钱已原路退回渠道或补回钱包，
	// 且与 refunded_amount / 钱包 reversal 流水同一事务落库。终态。
	OrderRefunded OrderStatus = "refunded"
	// OrderRefundFailed 表示渠道退款失败（S3/R-32），可重试。**不是终态**：
	// 重试成功后迁移到 refunded。与 failed 反向：failed 是「没收到钱」，
	// refund_failed 是「收到钱但没退成功」；把两者混用会让对账把已收的钱
	// 当成未收。
	OrderRefundFailed OrderStatus = "refund_failed"
)

var (
	// ErrInvalidOrderTransition 是所有非法迁移的哨兵错误，调用方可用
	// errors.Is 判定；具体 from/to 由 *OrderTransitionError 携带。
	ErrInvalidOrderTransition = errors.New("billing: invalid order transition")
)

// OrderTransitionError 描述一次被拒绝的状态迁移。为什么需要类型而不仅是
// fmt.Errorf：支付回调路径要区分「并发已迁移」（可幂等忽略）与「状态非法」
// （需要告警），调用方依赖 errors.As 取出两侧状态。
type OrderTransitionError struct {
	From OrderStatus
	To   OrderStatus
}

// Error 实现 error。
func (e *OrderTransitionError) Error() string {
	reason := "transition is not allowed"
	if !e.From.Valid() {
		reason = "unknown source status"
	} else if e.From.Terminal() {
		reason = "source state is terminal"
	} else if !e.To.Valid() {
		reason = "unknown target status"
	}
	return "billing: invalid order transition " + string(e.From) + " -> " + string(e.To) + ": " + reason
}

// Unwrap 让 errors.Is(err, ErrInvalidOrderTransition) 成立。
func (e *OrderTransitionError) Unwrap() error { return ErrInvalidOrderTransition }

// Valid 报告 s 是否是数据库 CHECK 允许的落库状态。
func (s OrderStatus) Valid() bool {
	switch s {
	case OrderPending, OrderSucceeded, OrderFailed, OrderExpired, OrderCancelled,
		OrderRefundRequired, OrderRefunded, OrderRefundFailed:
		return true
	default:
		return false
	}
}

// Terminal 报告 s 是否终态。终态不可再迁移——支付失败后的迟到回调、
// 超时关闭后的成功通知都必须被拒绝，否则会重复入账或改写对账事实。
//
// refund_failed 刻意**不是**终态：R-32 要求渠道退款失败可以重试。它是
// 「钱已收、退款尚未成功」的中间态，重试成功后才进入 refunded 终态。
func (s OrderStatus) Terminal() bool {
	switch s {
	case OrderSucceeded, OrderFailed, OrderExpired, OrderCancelled,
		OrderRefundRequired, OrderRefunded:
		return true
	default:
		return false
	}
}

// Transition 校验 from → to 是否合法，非法时返回 *OrderTransitionError。
//
// 合法迁移（白名单显式列出，避免依赖 Terminal 的实现细节）：
//   - pending → succeeded | failed | expired | cancelled（支付生命周期）；
//   - succeeded → refund_required（S2/R-29②：钱已收、履约失败）。
//     S2 的结算事务会在快照迁移失败时走这条迁移并落库；R-33 要求它显式
//     加白，而不是在业务代码里直接拼 SQL 绕过状态机；
//   - succeeded | refund_required | refund_failed → refunded（S3 退款完成）；
//   - succeeded | refund_required → refund_failed（S3 渠道退款失败，可重试）。
//
// 终态→任何状态（包括自身）都拒绝：数据库层迁移使用条件 UPDATE
// （WHERE id=? AND status IN (...)，本函数是同一契约的应用层表达，让调用方
// 让调用方在碰 DB 之前就得到可读错误；它不能替代 DB 条件。
func Transition(from, to OrderStatus) error {
	switch {
	case from == OrderPending && to == OrderSucceeded,
		from == OrderPending && to == OrderFailed,
		from == OrderPending && to == OrderExpired,
		from == OrderPending && to == OrderCancelled,
		// S2/R-29②：履约失败的「已付款 → 待退款」入口（R-33 显式加白）。
		from == OrderSucceeded && to == OrderRefundRequired,
		// S3/R-31：退款完成。refund_required 的订单同样是「钱已收」。
		from == OrderSucceeded && to == OrderRefunded,
		from == OrderRefundRequired && to == OrderRefunded,
		from == OrderRefundFailed && to == OrderRefunded,
		// S3/R-32：渠道退款失败可重试；refund_failed 不是终态。
		from == OrderSucceeded && to == OrderRefundFailed,
		from == OrderRefundRequired && to == OrderRefundFailed:
		return nil
	default:
		return &OrderTransitionError{From: from, To: to}
	}
}
