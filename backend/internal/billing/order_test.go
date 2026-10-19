package billing

import (
	"errors"
	"testing"
)

// TestOrderTransition 钉死 M5 §3 的五态订单状态机（裁决 P-1）：
// 只有 pending 可以迁移，四个终态一律不可再迁移。
func TestOrderTransition(t *testing.T) {
	valid := [][2]OrderStatus{
		{OrderPending, OrderSucceeded},
		{OrderPending, OrderFailed},
		{OrderPending, OrderExpired},
		{OrderPending, OrderCancelled},
	}
	for _, tc := range valid {
		if err := Transition(tc[0], tc[1]); err != nil {
			t.Errorf("Transition(%s, %s) = %v, want nil", tc[0], tc[1], err)
		}
	}

	terminal := []OrderStatus{OrderSucceeded, OrderFailed, OrderExpired, OrderCancelled}
	for _, from := range terminal {
		for _, to := range []OrderStatus{OrderPending, OrderSucceeded, OrderFailed, OrderExpired, OrderCancelled} {
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

	// pending→pending 与未知状态都不是合法迁移。
	if err := Transition(OrderPending, OrderPending); !errors.Is(err, ErrInvalidOrderTransition) {
		t.Errorf("Transition(pending, pending) err = %v, want ErrInvalidOrderTransition", err)
	}
	if err := Transition(OrderStatus("paid"), OrderSucceeded); !errors.Is(err, ErrInvalidOrderTransition) {
		t.Errorf("Transition(paid, succeeded) err = %v, want ErrInvalidOrderTransition", err)
	}
	// 架构 spec 的历史拼写同样非法：refunded→paid 既跨过未启用的枚举，
	// 也跨过终态，必须是 409 而不是被静默归一化。
	if err := Transition(OrderStatus("refunded"), OrderStatus("paid")); !errors.Is(err, ErrInvalidOrderTransition) {
		t.Errorf("Transition(refunded, paid) err = %v, want ErrInvalidOrderTransition", err)
	}
	// expired→paid/succeeded 是被状态机拒绝的典型迟到回调。
	if err := Transition(OrderExpired, OrderSucceeded); !errors.Is(err, ErrInvalidOrderTransition) {
		t.Errorf("Transition(expired, succeeded) err = %v, want ErrInvalidOrderTransition", err)
	}
}

// TestOrderStatusValid 校验枚举合法性与终态判定。
func TestOrderStatusValid(t *testing.T) {
	for _, s := range []OrderStatus{OrderPending, OrderSucceeded, OrderFailed, OrderExpired, OrderCancelled} {
		if !s.Valid() {
			t.Errorf("%s.Valid() = false, want true", s)
		}
	}
	if OrderStatus("paid").Valid() || OrderStatus("refunded").Valid() {
		t.Error("paid/refunded 不应是合法落库状态（P-1：paid 映射为 succeeded，refunded 预留不启用）")
	}
	if OrderPending.Terminal() {
		t.Error("pending.Terminal() = true, want false")
	}
	for _, s := range []OrderStatus{OrderSucceeded, OrderFailed, OrderExpired, OrderCancelled} {
		if !s.Terminal() {
			t.Errorf("%s.Terminal() = false, want true", s)
		}
	}
}
