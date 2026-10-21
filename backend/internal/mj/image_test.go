package mj

// W8/MJ-c 图片签名 URL 的单元验收。
//
// 覆盖 R-MJ-09 的冻结口径：域前缀/密钥派生、TTL 300s、参数 sig/exp、
// 常量时间比较、绑定 mj_id‖owner‖exp 的防篡改，以及 data.image_url 投影。

import (
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"newtg/gateway/internal/tasks"
)

func fixedImageSigner() (*ImageSigner, time.Time) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	return NewImageSigner("mj-image-test-secret"), now
}

func TestImageSignerRoundTripAndTTLBoundary(t *testing.T) {
	signer, now := fixedImageSigner()
	sig, exp := signer.Sign("mj_123", 42, now)
	if sig == "" || exp == 0 {
		t.Fatalf("Sign returned empty ticket: sig=%q exp=%d", sig, exp)
	}
	if want := now.Unix() + int64(ImageURLTTL/time.Second); exp != want {
		t.Fatalf("exp = %d, want %d (TTL 300s)", exp, want)
	}
	if int64(ImageURLTTL/time.Second) != 300 {
		t.Fatalf("ImageURLTTL = %s, want 300s (R-MJ-09 frozen)", ImageURLTTL)
	}

	// 边界：exp 当秒仍有效，exp+1 秒过期。
	if !signer.Verify("mj_123", 42, exp, sig, now) {
		t.Fatal("fresh ticket must verify")
	}
	if !signer.Verify("mj_123", 42, exp, sig, now.Add(ImageURLTTL)) {
		t.Fatal("ticket must remain valid at the exact exp second")
	}
	if signer.Verify("mj_123", 42, exp, sig, now.Add(ImageURLTTL+time.Second)) {
		t.Fatal("ticket must expire one second after exp")
	}
}

func TestImageSignerBindsAllFields(t *testing.T) {
	signer, now := fixedImageSigner()
	sig, exp := signer.Sign("mj_123", 42, now)

	cases := []struct {
		name  string
		mjID  string
		owner int64
		exp   int64
		sig   string
	}{
		{"wrong owner", "mj_123", 43, exp, sig},
		{"wrong mj id", "mj_124", 42, exp, sig},
		{"wrong exp", "mj_123", 42, exp + 1, sig},
		{"tampered sig", "mj_123", 42, exp, flipLastChar(sig)},
		{"garbage sig", "mj_123", 42, exp, "not-base64!"},
		{"empty sig", "mj_123", 42, exp, ""},
		{"zero owner", "mj_123", 0, exp, sig},
		{"invalid mj id", "bad id", 42, exp, sig},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if signer.Verify(tc.mjID, tc.owner, tc.exp, tc.sig, now) {
				t.Fatalf("ticket must not verify: %+v", tc)
			}
		})
	}
}

func TestImageSignerDifferentSecretCannotVerify(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	issuer := NewImageSigner("issuer-secret")
	verifier := NewImageSigner("other-secret")
	sig, exp := issuer.Sign("mj_1", 7, now)
	if verifier.Verify("mj_1", 7, exp, sig, now) {
		t.Fatal("a ticket signed with another key must not verify")
	}
}

// TestImageSignerUsesConstantTimeCompare 是源码级守卫：签名比较必须走
// hmac.Equal，不能被改成 ==/bytes.Equal（后者会按首个差异字节提前返回）。
func TestImageSignerUsesConstantTimeCompare(t *testing.T) {
	raw, err := os.ReadFile("image.go")
	if err != nil {
		t.Fatalf("read image.go: %v", err)
	}
	source := string(raw)
	if !strings.Contains(source, "hmac.Equal(") {
		t.Fatal("image signature verification must use hmac.Equal (constant time)")
	}
	if strings.Contains(source, "bytes.Equal(") {
		t.Fatal("image signature verification must not use bytes.Equal")
	}
}

func TestParseImageTicket(t *testing.T) {
	valid := url.Values{"sig": {"abc"}, "exp": {"1700000000"}}
	if exp, sig, ok := ParseImageTicket(valid); !ok || exp != 1700000000 || sig != "abc" {
		t.Fatalf("valid ticket parsed as (%d,%q,%v)", exp, sig, ok)
	}
	cases := []struct {
		name  string
		query url.Values
	}{
		{"nil", nil},
		{"missing sig", url.Values{"exp": {"1700000000"}}},
		{"missing exp", url.Values{"sig": {"abc"}}},
		{"non numeric exp", url.Values{"sig": {"abc"}, "exp": {"soon"}}},
		{"zero exp", url.Values{"sig": {"abc"}, "exp": {"0"}}},
		{"negative exp", url.Values{"sig": {"abc"}, "exp": {"-5"}}},
		{"blank sig", url.Values{"sig": {"  "}, "exp": {"1700000000"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, ok := ParseImageTicket(tc.query); ok {
				t.Fatalf("ticket must be rejected: %v", tc.query)
			}
		})
	}
}

func TestImageURLProjection(t *testing.T) {
	cases := []struct {
		name string
		task *tasks.Task
		want string
		ok   bool
	}{
		{"nil task", nil, "", false},
		{"nil data", &tasks.Task{}, "", false},
		{"missing key", &tasks.Task{Data: map[string]any{}}, "", false},
		{"non string", &tasks.Task{Data: map[string]any{DataImageURL: 123}}, "", false},
		{"blank", &tasks.Task{Data: map[string]any{DataImageURL: "   "}}, "", false},
		{"valid", &tasks.Task{Data: map[string]any{DataImageURL: " https://cdn/x.png "}}, "https://cdn/x.png", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ImageURL(tc.task)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("ImageURL = (%q,%v), want (%q,%v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// flipLastChar 改掉签名最后一个字符，构造同长度但内容不同的票据。
func flipLastChar(sig string) string {
	if sig == "" {
		return "x"
	}
	last := sig[len(sig)-1]
	replacement := byte('A')
	if last == 'A' {
		replacement = 'B'
	}
	return sig[:len(sig)-1] + string(replacement)
}
