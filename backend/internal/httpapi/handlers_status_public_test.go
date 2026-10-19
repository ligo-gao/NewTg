package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// GET /api/status 的公开设置投影（modules-08 §1.4.1）：
// site/nav/billing/pay 由注册表 public:true 键自动聚合，且密钥类永不出现。
func TestPublicStatusSettingsProjection(t *testing.T) {
	cfg := mapConfig{
		"site.title":               configString("GatewayX"),
		"site.server_address":      configString("https://gw.example.com/"),
		"site.seo.description":     configString("desc"),
		"nav.pricing.enabled":      configBool(false),
		"nav.console.require_auth": configBool(true),
		"billing.quota_per_unit":   configInt(200),
		"pay.online_enabled":       configBool(false),
		"auth.passkey.enabled":     configBool(true),
		// 密钥类即使被错误写入配置中心，也不得进公开面。
		"auth.turnstile.secret_key": configString("should-never-leak"),
	}
	env := newAuthTestEnv(t, cfg, authTestOptions{})
	rr := env.request(http.MethodGet, "/api/status", nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d（body=%s）", rr.Code, rr.Body.String())
	}
	if body := rr.Body.String(); strings.Contains(body, "should-never-leak") || strings.Contains(body, "secret_key") {
		t.Fatalf("公开状态泄漏密钥类字段: %s", body)
	}

	var body struct {
		Auth struct {
			Passkey struct {
				Enabled bool `json:"enabled"`
			} `json:"passkey"`
		} `json:"auth"`
		Site struct {
			Title         string `json:"title"`
			ServerAddress string `json:"server_address"`
			Seo           struct {
				Description string `json:"description"`
			} `json:"seo"`
		} `json:"site"`
		Nav struct {
			Pricing struct {
				Enabled bool `json:"enabled"`
			} `json:"pricing"`
			Console struct {
				RequireAuth bool `json:"require_auth"`
			} `json:"console"`
		} `json:"nav"`
		Billing struct {
			QuotaPerUnit int64 `json:"quota_per_unit"`
		} `json:"billing"`
		Pay struct {
			OnlineEnabled bool `json:"online_enabled"`
		} `json:"pay"`
		USDExchangeRate     float64 `json:"usd_exchange_rate"`
		CreditDisplaySymbol string  `json:"credit_display_symbol"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("JSON 非法: %v", err)
	}
	if body.Site.Title != "GatewayX" || body.Site.Seo.Description != "desc" {
		t.Fatalf("site 投影错误: %+v", body.Site)
	}
	if body.Nav.Pricing.Enabled || !body.Nav.Console.RequireAuth {
		t.Fatalf("nav 投影错误: %+v", body.Nav)
	}
	if body.Billing.QuotaPerUnit != 200 {
		t.Fatalf("billing.quota_per_unit = %d", body.Billing.QuotaPerUnit)
	}
	if body.Pay.OnlineEnabled {
		t.Fatal("pay.online_enabled 未按配置下发")
	}
	if !body.Auth.Passkey.Enabled {
		t.Fatal("auth.passkey.enabled 未按配置下发")
	}
	// §1.4.1 派生字段：汇率常量 7.1，显示符号恒等于 billing.currency_symbol。
	if body.USDExchangeRate != 7.1 {
		t.Fatalf("usd_exchange_rate = %v，期望 7.1", body.USDExchangeRate)
	}
	if body.CreditDisplaySymbol != "¥" {
		t.Fatalf("credit_display_symbol = %q，期望 ¥", body.CreditDisplaySymbol)
	}
}

func TestPublicStatusSettingsDefaults(t *testing.T) {
	env := newAuthTestEnv(t, nil, authTestOptions{})
	rr := env.request(http.MethodGet, "/api/status", nil, nil)
	var body struct {
		Site struct {
			Title     string `json:"title"`
			Timezone  string `json:"timezone"`
			HomeBlock struct {
				Hero bool `json:"hero"`
			} `json:"home_blocks"`
		} `json:"site"`
		Nav struct {
			Home struct {
				Enabled bool `json:"enabled"`
			} `json:"home"`
		} `json:"nav"`
		Billing struct {
			DisplayUnit string `json:"display_unit"`
		} `json:"billing"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("JSON 非法: %v", err)
	}
	if body.Site.Title != "NewTG" || body.Site.Timezone != "Asia/Shanghai" {
		t.Fatalf("site 默认值错误: %+v", body.Site)
	}
	if !body.Site.HomeBlock.Hero {
		t.Fatal("home_blocks.hero 默认应为 true")
	}
	if !body.Nav.Home.Enabled {
		t.Fatal("nav.home.enabled 默认应为 true")
	}
	if body.Billing.DisplayUnit != "credits" {
		t.Fatalf("billing.display_unit 默认 = %q", body.Billing.DisplayUnit)
	}
}
