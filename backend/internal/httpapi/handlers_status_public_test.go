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
		"nav.models.enabled":       configBool(false),
		"nav.models.require_auth":  configBool(false),
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
			Models struct {
				Enabled     bool `json:"enabled"`
				RequireAuth bool `json:"require_auth"`
			} `json:"models"`
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
	// 值来自配置投影（注册表默认 enabled=true），而非注册表默认。
	if body.Nav.Models.Enabled || body.Nav.Models.RequireAuth {
		t.Fatalf("nav.models 投影错误: %+v", body.Nav.Models)
	}
	// 字段必须真实下发：注册表默认 true，缺失会被零值 false 掩盖。
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		t.Fatalf("JSON 非法: %v", err)
	}
	var navRaw map[string]json.RawMessage
	if err := json.Unmarshal(raw["nav"], &navRaw); err != nil {
		t.Fatalf("nav 非法: %v", err)
	}
	if _, ok := navRaw["models"]; !ok {
		t.Fatal("nav.models 未下发")
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
			Models struct {
				Enabled     bool `json:"enabled"`
				RequireAuth bool `json:"require_auth"`
			} `json:"models"`
		} `json:"nav"`
		Billing struct {
			DisplayUnit string `json:"display_unit"`
		} `json:"billing"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("JSON 非法: %v", err)
	}
	if body.Site.Title != "NEWTG" || body.Site.Timezone != "Asia/Shanghai" {
		t.Fatalf("site 默认值错误: %+v", body.Site)
	}
	if !body.Site.HomeBlock.Hero {
		t.Fatal("home_blocks.hero 默认应为 true")
	}
	if !body.Nav.Home.Enabled {
		t.Fatal("nav.home.enabled 默认应为 true")
	}
	if !body.Nav.Models.Enabled {
		t.Fatal("nav.models.enabled 默认应为 true")
	}
	if body.Nav.Models.RequireAuth {
		t.Fatal("nav.models.require_auth 默认应为 false")
	}
	if body.Billing.DisplayUnit != "credits" {
		t.Fatalf("billing.display_unit 默认 = %q", body.Billing.DisplayUnit)
	}
}

// R-PUB-07：home_custom_html 仅在 home_custom_enabled=true 时下发。
func TestPublicStatusHomeCustomHTMLConditional(t *testing.T) {
	// 默认（disabled）：字段完全不出现，而不是空串。
	env := newAuthTestEnv(t, mapConfig{
		"site.home_custom_html": configString("<section>custom</section>"),
	}, authTestOptions{})
	rr := env.request(http.MethodGet, "/api/status", nil, nil)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		t.Fatalf("JSON 非法: %v", err)
	}
	var site map[string]json.RawMessage
	if err := json.Unmarshal(raw["site"], &site); err != nil {
		t.Fatalf("site 非法: %v", err)
	}
	if _, ok := site["home_custom_html"]; ok {
		t.Fatal("home_custom_enabled=false 时不得下发 home_custom_html")
	}
	if enabled, _ := site["home_custom_enabled"]; string(enabled) != "false" {
		t.Fatalf("home_custom_enabled = %s", enabled)
	}

	// enabled=true：字段下发且内容一致。
	enabledEnv := newAuthTestEnv(t, mapConfig{
		"site.home_custom_enabled": configBool(true),
		"site.home_custom_html":    configString("<section>custom</section>"),
	}, authTestOptions{})
	rr = enabledEnv.request(http.MethodGet, "/api/status", nil, nil)
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		t.Fatalf("JSON 非法: %v", err)
	}
	if err := json.Unmarshal(raw["site"], &site); err != nil {
		t.Fatalf("site 非法: %v", err)
	}
	var html string
	if err := json.Unmarshal(site["home_custom_html"], &html); err != nil {
		t.Fatalf("home_custom_html 缺失或类型错误: %v", err)
	}
	if html != "<section>custom</section>" {
		t.Fatalf("home_custom_html = %q", html)
	}
}

// R-PUB-09：nav.docs.require_auth / nav.about.require_auth 默认 false 并可配置。
func TestPublicStatusNavAuthKeys(t *testing.T) {
	env := newAuthTestEnv(t, nil, authTestOptions{})
	rr := env.request(http.MethodGet, "/api/status", nil, nil)
	var body struct {
		Nav struct {
			Docs struct {
				Enabled     bool `json:"enabled"`
				RequireAuth bool `json:"require_auth"`
			} `json:"docs"`
			About struct {
				Enabled     bool `json:"enabled"`
				RequireAuth bool `json:"require_auth"`
			} `json:"about"`
		} `json:"nav"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("JSON 非法: %v", err)
	}
	if body.Nav.Docs.RequireAuth || body.Nav.About.RequireAuth {
		t.Fatalf("两个新键默认应为 false: %+v", body.Nav)
	}
	if !body.Nav.Docs.Enabled || !body.Nav.About.Enabled {
		t.Fatalf("enabled 默认应为 true: %+v", body.Nav)
	}

	enabled := newAuthTestEnv(t, mapConfig{
		"nav.docs.require_auth":  configBool(true),
		"nav.about.require_auth": configBool(true),
	}, authTestOptions{})
	rr = enabled.request(http.MethodGet, "/api/status", nil, nil)
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("JSON 非法: %v", err)
	}
	if !body.Nav.Docs.RequireAuth || !body.Nav.About.RequireAuth {
		t.Fatalf("配置值未投影到 nav: %+v", body.Nav)
	}
	// R-PUB-06：status 仍保持 no-store（设置即时生效）。
	if cc := rr.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q，期望 no-store", cc)
	}
}
