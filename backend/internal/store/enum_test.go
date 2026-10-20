package store

import (
	"errors"
	"testing"

	"newtg/gateway/internal/contract"
)

// TestBillingPreferenceMapping 钉死 users.billing_preference 文本与
// contract.BillingPreference 内部值的唯一对应关系。
//
// 这不是「再测一遍 switch」：contract 的 iota 顺序与 DB 文本顺序**不同**
// （subscription_first, subscription_only, wallet_only, wallet_first 对比
// subscription_first, wallet_first, subscription_only, wallet_only）。任何
// 按下标 / 类型转换的"简化"都会让 wallet_first 与 subscription_only 互换，
// 直接改变用户的扣费来源。表驱动用例把四组对应关系逐条锁死。
func TestBillingPreferenceMapping(t *testing.T) {
	cases := []struct {
		text  string
		value contract.BillingPreference
	}{
		{BillingPreferenceSubscriptionFirst, contract.PrefSubscriptionFirst},
		{BillingPreferenceWalletFirst, contract.PrefWalletFirst},
		{BillingPreferenceSubscriptionOnly, contract.PrefSubscriptionOnly},
		{BillingPreferenceWalletOnly, contract.PrefWalletOnly},
	}
	for _, c := range cases {
		c := c
		t.Run(c.text, func(t *testing.T) {
			gotText, err := BillingPreferenceText(c.value)
			if err != nil || gotText != c.text {
				t.Fatalf("BillingPreferenceText(%d) = %q/%v，want %q/nil",
					c.value, gotText, err, c.text)
			}
			gotValue, err := ParseBillingPreference(c.text)
			if err != nil || gotValue != c.value {
				t.Fatalf("ParseBillingPreference(%q) = %d/%v，want %d/nil",
					c.text, gotValue, err, c.value)
			}
		})
	}

	// 四值是双射：文本数与内部值数一致，且没有两个内部值映射到同一文本。
	seen := make(map[string]contract.BillingPreference, len(cases))
	for _, c := range cases {
		if prev, dup := seen[c.text]; dup {
			t.Fatalf("文本 %q 同时映射到 %d 与 %d", c.text, prev, c.value)
		}
		seen[c.text] = c.value
	}

	// 反例护栏：contract 的 iota 顺序确实与文本顺序不同。文本顺序第 1 位是
	// wallet_first，而 contract 的 1 是 subscription_only；文本顺序第 2 位是
	// subscription_only，而 contract 的 2 是 wallet_only。若将来有人把
	// contract 常量重排成「文本顺序」，这两条会让测试变红，提醒复核
	// 「禁止按下标转换」的注释与迁移口径，而不是静默改变语义。
	if contract.PrefWalletFirst == 1 || contract.PrefSubscriptionOnly == 2 {
		t.Fatal("contract.BillingPreference 的 iota 顺序与 DB 文本顺序意外一致：请复核映射与注释")
	}

	// 越界值必须报错而不是兜底归一（归一属于迁移/偏好设置层的职责）。
	if _, err := BillingPreferenceText(contract.BillingPreference(99)); !errors.Is(err, ErrBillingPreferenceInvalid) {
		t.Fatalf("越界枚举 err = %v，want ErrBillingPreferenceInvalid", err)
	}
	if _, err := ParseBillingPreference("bogus"); !errors.Is(err, ErrBillingPreferenceInvalid) {
		t.Fatalf("未知文本 err = %v，want ErrBillingPreferenceInvalid", err)
	}
}
