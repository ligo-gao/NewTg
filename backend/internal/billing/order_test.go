package billing

import (
	"errors"
	"testing"
)

// TestOrderTransition 钉死订单状态机（P-1 + S2/R-29② + S3/R-31/R-32）：
//   - pending 可以迁移到四个支付结果态；
//   - 退款是终态 succeeded/refund_required 的**唯一**合法出口（refunded /
//     refund_failed），refund_failed 还可以重试到 refunded；
//   - 其余终态（failed/expired/cancelled/refunded）一律不可再迁移。
func TestOrderTransition(t *testing.T) {
	valid := [][2]OrderStatus{
		{OrderPending, OrderSucceeded},
		{OrderPending, OrderFailed},
		{OrderPending, OrderExpired},
		{OrderPending, OrderCancelled},
		// S2/R-29②：履约失败入口（R-33 显式加白）。
		{OrderSucceeded, OrderRefundRequired},
		// S3/R-31：退款完成。
		{OrderSucceeded, OrderRefunded},
		{OrderRefundRequired, OrderRefunded},
		{OrderRefundFailed, OrderRefunded},
		// S3/R-32：渠道退款失败可重试。
		{OrderSucceeded, OrderRefundFailed},
		{OrderRefundRequired, OrderRefundFailed},
	}
	for _, tc := range valid {
		if err := Transition(tc[0], tc[1]); err != nil {
			t.Errorf("Transition(%s, %s) = %v, want nil", tc[0], tc[1], err)
		}
	}

	// 真正不可迁移的终态：failed/expired/cancelled/refunded。
	for _, from := range []OrderStatus{OrderFailed, OrderExpired, OrderCancelled, OrderRefunded} {
		for _, to := range []OrderStatus{OrderPending, OrderSucceeded, OrderFailed,
			OrderExpired, OrderCancelled, OrderRefundRequired, OrderRefunded, OrderRefundFailed} {
			err := Transition(from, to)
			if err == nil {
				t.Errorf("Transition(%s, %s) = nil, want terminal-state error", from, to)
				continue
			}
			if !errors.Is(err, ErrInvalidOrderTransition) {
				t.Errorf("Transition(%s, %s) err = %v, want ErrInvalidOrderTransition", from, to, err)
			}
			var te *OrderTransitionError
			if !errors.As(err, &te) {
				t.Errorf("Transition(%s, %s) err type = %T, want *OrderTransitionError", from, to, err)
			}
		}
	}

	// succeeded 只允许退款出口：不能回到 pending，也不能直接变成 failed。
	for _, to := range []OrderStatus{OrderPending, OrderFailed, OrderExpired, OrderCancelled} {
		if err := Transition(OrderSucceeded, to); !errors.Is(err, ErrInvalidOrderTransition) {
			t.Errorf("Transition(succeeded, %s) err = %v, want ErrInvalidOrderTransition", to, err)
		}
	}
	// refund_failed 只能重试到 refunded，不能伪装成 succeeded/refund_required。
	for _, to := range []OrderStatus{OrderPending, OrderSucceeded, OrderRefundRequired} {
		if err := Transition(OrderRefundFailed, to); !errors.Is(err, ErrInvalidOrderTransition) {
			t.Errorf("Transition(refund_failed, %s) err = %v, want ErrInvalidOrderTransition", to, err)
		}
	}

	// pending→pending 与未知状态都不是合法迁移。
	if err := Transition(OrderPending, OrderPending); !errors.Is(err, ErrInvalidOrderTransition) {
		t.Errorf("Transition(pending, pending) err = %v, want ErrInvalidOrderTransition", err)
	}
	if err := Transition(OrderStatus("paid"), OrderSucceeded); !errors.Is(err, ErrInvalidOrderTransition) {
		t.Errorf("Transition(paid, succeeded) err = %v, want ErrInvalidOrderTransition", err)
	}
	// 架构 spec 的历史拼写同样非法：refunded→paid 跨过未知枚举，必须是 409。
	if err := Transition(OrderStatus("refunded"), OrderStatus("paid")); !errors.Is(err, ErrInvalidOrderTransition) {
		t.Errorf("Transition(refunded, paid) err = %v, want ErrInvalidOrderTransition", err)
	}
	// expired→succeeded 是被状态机拒绝的典型迟到回调。
	if err := Transition(OrderExpired, OrderSucceeded); !errors.Is(err, ErrInvalidOrderTransition) {
		t.Errorf("Transition(expired, succeeded) err = %v, want ErrInvalidOrderTransition", err)
	}
}

// TestOrderStatusValid 校验枚举合法性与终态判定。
func TestOrderStatusValid(t *testing.T) {
	for _, s := range []OrderStatus{OrderPending, OrderSucceeded, OrderFailed, OrderExpired,
		OrderCancelled, OrderRefundRequired, OrderRefunded, OrderRefundFailed} {
		if !s.Valid() {
			t.Errorf("%s.Valid() = false, want true", s)
		}
	}
	if OrderStatus("paid").Valid() || OrderStatus("refunding").Valid() {
		t.Error("paid/refunding 不应是合法落库状态（paid 映射为 succeeded）")
	}
	if OrderPending.Terminal() {
		t.Error("pending.Terminal() = true, want false")
	}
	for _, s := range []OrderStatus{OrderSucceeded, OrderFailed, OrderExpired, OrderCancelled,
		OrderRefundRequired, OrderRefunded} {
		if !s.Terminal() {
			t.Errorf("%s.Terminal() = false, want true", s)
		}
	}
	// R-32：refund_failed 可重试，不是终态。
	if OrderRefundFailed.Terminal() {
		t.Error("refund_failed.Terminal() = true, want false（R-32 可重试）")
	}
}
