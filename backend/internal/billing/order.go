package billing

import "errors"

// 订单状态机（modules-04 §3；裁决 P-1）。
//
// 枚举与数据库 CHECK 完全一致：pending / succeeded / failed / expired /
// cancelled / refund_required（后者由 S2/R-29② 引入，迁移 20261021130000）。
// `paid` 是架构 spec 的历史别名，映射为 succeeded；`refunded` 仍为未来扩展
// 预留（购买退款属 S3），故这里刻意不定义常量——任何尝试写入 refunded 的
// 代码都应在编译期就暴露。
type OrderStatus string

const (
	// OrderPending 是唯一非终态：订单已创建、等待支付确认。
	OrderPending OrderStatus = "pending"
	// OrderSucceeded 表示支付已确认且余额已入账（paid 的映射目标）。
	OrderSucceeded OrderStatus = "succeeded"
	OrderFailed    OrderStatus = "failed"
	// OrderExpired 表示超过 30 分钟未支付被 worker 关闭。
	OrderExpired OrderStatus = "expired"
	// OrderCancelled 表示用户主动取消。
	OrderCancelled OrderStatus = "cancelled"
	// OrderRefundRequired 表示**钱已收、履约失败**（S2/R-29②：升降级支付成功后
	// 订阅已被并发取消/换套餐，迁移无法应用）。终态：不再接受回调重投，也不再
	// 自动迁移——留给退款流程（S3）人工处理。不是 pending，否则渠道会无限重投、
	// 每次 5xx，且永远不会成功。
	OrderRefundRequired OrderStatus = "refund_required"
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
		OrderRefundRequired:
		return true
	default:
		return false
	}
}

// Terminal 报告 s 是否终态。终态不可再迁移——支付失败后的迟到回调、
// 超时关闭后的成功通知都必须被拒绝，否则会重复入账或改写对账事实。
func (s OrderStatus) Terminal() bool {
	switch s {
	case OrderSucceeded, OrderFailed, OrderExpired, OrderCancelled, OrderRefundRequired:
		return true
	default:
		return false
	}
}

// Transition 校验 from → to 是否合法，非法时返回 *OrderTransitionError。
//
// 唯一合法迁移：pending → succeeded | failed | expired | cancelled。
// 终态→任何状态（包括自身）都拒绝：数据库层迁移使用条件 UPDATE
// （WHERE id=? AND status='pending'），本函数是同一契约的应用层表达，
// 让调用方在碰 DB 之前就得到可读错误。
//
// 【不在此表的迁移】`succeeded → refund_required`（S2/R-29②：订阅订单支付
// 成功但履约失败）**刻意不放进 Transition 白名单**：它只发生在订阅结算事务
// 内部（`settleSubscriptionChange` 用条件 UPDATE `WHERE id=? AND status=
// 'succeeded'` 直接落库），没有第二个调用方，也就没有需要预校验的路径。
// 退款流程（S3）如果要用本函数表达该迁移，必须显式把这一对加进开关——
// 在此之前它会被拒绝（fail closed），这是有意的：任何新的「已付款 →
// 待退款」入口都应当先被复核。
func Transition(from, to OrderStatus) error {
	// 显式列出四个合法目标，避免依赖 Terminal() 的实现细节。
	switch {
	case from == OrderPending && to == OrderSucceeded,
		from == OrderPending && to == OrderFailed,
		from == OrderPending && to == OrderExpired,
		from == OrderPending && to == OrderCancelled:
		return nil
	default:
		return &OrderTransitionError{From: from, To: to}
	}
}
