package notify

// N8 worker 的纯函数验收：事件映射、重试时刻、失败文案脱敏与幂等锚。
// 真实 PG 的行为（SKIP LOCKED/暂停/重启）在 *_integration_test.go。

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestWebhookEventForCategory(t *testing.T) {
	cases := map[string]string{
		"billing.balance_low":     "balance_low",
		"billing.quota_exhausted": "balance_low",
		"billing.payment_success": "payment_success",
		"system.announcement":     "announcement",
		"auth.welcome":            "user_signup",
	}
	for category, want := range cases {
		got, ok := WebhookEventForCategory(category)
		if !ok || got != want {
			t.Errorf("WebhookEventForCategory(%q) = %q,%v，期望 %q,true", category, got, ok, want)
		}
	}
	// 没有 Webhook 订阅面的 category 必须 fail-closed，不能猜一个事件名。
	for _, category := range []string{"billing.usage_digest", "security.alert", "unknown"} {
		if got, ok := WebhookEventForCategory(category); ok {
			t.Errorf("WebhookEventForCategory(%q) = %q,true，期望 not ok", category, got)
		}
	}
}

func TestRetryAtForAttempt(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{1, 1 * time.Minute},
		{2, 5 * time.Minute},
		{3, 25 * time.Minute},
	}
	for _, tc := range cases {
		got := retryAtForAttempt(now, tc.attempt)
		if got == nil {
			t.Fatalf("attempt %d 应有重试时刻", tc.attempt)
		}
		if !got.Equal(now.Add(tc.want)) {
			t.Errorf("attempt %d = %s，期望 %s", tc.attempt, got, now.Add(tc.want))
		}
	}
	// 第 4 次失败是终态：没有重试时刻。
	if got := retryAtForAttempt(now, webhookMaxAttempts); got != nil {
		t.Fatalf("第 %d 次失败后仍安排重试：%s", webhookMaxAttempts, got)
	}
	if got := retryAtForAttempt(now, 0); got != nil {
		t.Fatalf("attempt 0 不应有重试时刻：%s", got)
	}
}

func TestWebhookFailureReasonIsSanitized(t *testing.T) {
	secretURL := "https://hooks.example.com/endpoint?token=super-secret-value"
	cases := []struct {
		name   string
		result WebhookResult
		err    error
		want   string
	}{
		{"blocked", WebhookResult{}, fmt.Errorf("%w: %s", ErrWebhookBlockedTarget, secretURL), "webhook target is not allowed"},
		{"transport", WebhookResult{}, fmt.Errorf("%w: %s", ErrWebhookTransport, secretURL), "webhook request failed"},
		{"not_configured", WebhookResult{}, ErrWebhookNotConfigured, "webhook is not configured"},
		{"unknown", WebhookResult{}, errors.New(secretURL + " response body"), "webhook delivery failed"},
		{"http_status", WebhookResult{StatusCode: 503, OK: false}, nil, "endpoint returned http status 503"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := webhookFailureReason(tc.result, tc.err)
			if got != tc.want {
				t.Fatalf("reason = %q，期望 %q", got, tc.want)
			}
			for _, leak := range []string{"super-secret-value", "hooks.example.com", "response body"} {
				if strings.Contains(got, leak) {
					t.Fatalf("reason 泄露 %q: %q", leak, got)
				}
			}
		})
	}
}

func TestWebhookEventIDUsesDedupeKey(t *testing.T) {
	if got := webhookEventID(Delivery{ID: 9, DedupeKey: "balance:7:20261020"}); got != "balance:7:20261020" {
		t.Fatalf("eventID = %q", got)
	}
	if got := webhookEventID(Delivery{ID: 9}); got != "delivery:9" {
		t.Fatalf("缺 dedupe_key 时 eventID = %q，期望 delivery:9", got)
	}
}
