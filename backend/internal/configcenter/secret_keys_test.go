package configcenter

import (
	"context"
	"errors"
	"testing"
	"time"
)

// 本文件验收 R-09 的 M1 落地：Turnstile secret_key 必须在密钥类清单中，
// 从而 Center.Save 会先密封再落库；公开键（site_key/enabled）不得登记。
// 复用 configcenter_test.go 的 fakeDB/fakeRedis/fakeSealer（同一包）。

// TestTurnstileSecretKeyRegistered：只有 secret_key 属密钥类。
func TestTurnstileSecretKeyRegistered(t *testing.T) {
	if _, ok := secretKeys["auth.turnstile.secret_key"]; !ok {
		t.Fatal("auth.turnstile.secret_key 必须登记进 secretKeys（R-09 / modules-08 F-08-07）")
	}
	for _, public := range []string{
		"auth.turnstile.enabled",
		"auth.turnstile.site_key",
	} {
		if _, ok := secretKeys[public]; ok {
			t.Fatalf("%q 是公开下发字段，不得登记为密钥类", public)
		}
	}
}

// TestPaymentSecretKeysRegistered：M5 充值渠道凭据必须密封落库；
// publishable_key 是公开值，不得登记。
func TestPaymentSecretKeysRegistered(t *testing.T) {
	for _, secret := range []string{
		"pay.stripe.secret_key",
		"pay.stripe.webhook_secret",
		"pay.alipay.private_key",
		"pay.alipay.public_key",
		"pay.epay.key",
	} {
		if _, ok := secretKeys[secret]; !ok {
			t.Fatalf("%q 必须登记进 secretKeys（security-03 / modules-08 §3.20）", secret)
		}
	}
	if _, ok := secretKeys["pay.stripe.publishable_key"]; ok {
		t.Fatal("pay.stripe.publishable_key 是公开下发字段，不得登记为密钥类")
	}
}

// TestSaveTurnstileSecretRequiresSealer：注册路径在无 Sealer 时拒绝写入，
// 且不触达 DB / PUBLISH。
func TestSaveTurnstileSecretRequiresSealer(t *testing.T) {
	db := &fakeDB{execResult: fakeResult{affected: 1}}
	rdb := &fakeRedis{}
	c := newCenter(db, rdb) // 使用包级默认 secretKeys

	err := c.Save(context.Background(), "auth.turnstile.secret_key", "cf-secret", nil)
	if !errors.Is(err, ErrSecretSealerRequired) {
		t.Fatalf("已注册密钥类键无 Sealer 应返回 ErrSecretSealerRequired，实际 %v", err)
	}
	if len(db.execCalls) != 0 || db.rowCalls != 0 || len(rdb.published) != 0 {
		t.Fatal("拒绝写入时不应触达 DB 或 PUBLISH")
	}
}

// TestSaveTurnstileSecretSealed：注册路径带 Sealer 时以密文落库。
func TestSaveTurnstileSecretSealed(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	db := &fakeDB{row: fakeRow{updatedAt: now}}
	rdb := &fakeRedis{}
	c := newCenter(db, rdb)
	c.Sealer = fakeSealer{prefix: "sealed:"}

	if err := c.Save(context.Background(), "auth.turnstile.secret_key", "cf-secret", nil); err != nil {
		t.Fatalf("Save 失败：%v", err)
	}
	if v, ok := c.Get("auth.turnstile.secret_key"); !ok || string(v) != `"sealed:cf-secret"` {
		t.Fatalf("落库值应为密文，实际 %s (ok=%v)", v, ok)
	}
}

// TestSaveUnregisteredTurnstileKeyPlaintext：未注册路径（公开键）无需 Sealer，
// 原样落库。注册/未注册两条路径的行为差是密钥清单生效的直接证据。
func TestSaveUnregisteredTurnstileKeyPlaintext(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	db := &fakeDB{row: fakeRow{updatedAt: now}}
	rdb := &fakeRedis{}
	c := newCenter(db, rdb) // 无 Sealer

	if err := c.Save(context.Background(), "auth.turnstile.site_key", "site-key", nil); err != nil {
		t.Fatalf("公开键 Save 不应要求 Sealer：%v", err)
	}
	if v, ok := c.Get("auth.turnstile.site_key"); !ok || string(v) != `"site-key"` {
		t.Fatalf("公开键应原样落库，实际 %s (ok=%v)", v, ok)
	}
}
