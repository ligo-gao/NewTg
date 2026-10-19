package billing

import "errors"

// 订单状态机（modules-04 §3；裁决 P-1）。
//
// 枚举与数据库 CHECK 完全一致：pending / succeeded / failed / expired /
// cancelled。`paid` 是架构 spec 的历史别名，映射为 succeeded；`refunded`
// 仅为未来扩展预留，MVP 不启用（不退款），故这里刻意不定义常量——
// 任何尝试写入 refunded 的代码都应在编译期就暴露。
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
	case OrderPending, OrderSucceeded, OrderFailed, OrderExpired, OrderCancelled:
		return true
	default:
		return false
	}
}

// Terminal 报告 s 是否终态。终态不可再迁移——支付失败后的迟到回调、
// 超时关闭后的成功通知都必须被拒绝，否则会重复入账或改写对账事实。
func (s OrderStatus) Terminal() bool {
	switch s {
	case OrderSucceeded, OrderFailed, OrderExpired, OrderCancelled:
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
