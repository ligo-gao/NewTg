// 本文件实现演示站（只读模式）的执行面：管理面只读守卫 + 中继两层日额度。
//
// 设计权威：docs/modules-08-系统管理与设置-子功能设计.md §3.17（F-08-46）、
// §3.17.2（N-33 定案 · 2026-10-17 · 裁决 G7-R48）。
//
// 三条硬约束在这里落成代码：
//  1. 守卫在服务端、deny-by-default（§3.17.1 Q1）：前端隐藏按钮不算守卫。
//     开关 site.demo.enabled 只是入口，真正拦截发生在 /api/* 的最外层，
//     绕过前端直接 POST 仍得 403。
//  2. 三分放行集（Q2）：① 会话/认证类写放行（否则无人能登录/登出/找回
//     密码）；② 管理/配置/账务/用户/令牌/兑换/日志类一律拒绝；③ 第三方
//     支付回调按来源识别放行（它们不由访客会话发起）。中继（/v1、/mj、
//     /pg 与 /v1/tasks）不在守卫内，由本文件的额度约束管理。
//  3. 拒绝形状统一（Q3）：403 + 固定 problem type + 固定文案，绝不静默成功
//     或 500；被拒不写审计，只累加 demo_write_rejected_total（Q6）。
package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	platformconfig "newtg/gateway/internal/config"
	"newtg/gateway/internal/metrics"
	"newtg/gateway/internal/ratelimit"
	"newtg/gateway/internal/settings"
)

const (
	// 设置键（唯一真源是 settings 注册表；名称按 §3.17.2 的
	// `site.demo.relay_quota.*` 冻结。任务简报里的 `site.demo.relay_ip_daily`
	// 只是举例，本实现以设计文档的键名为准）。
	keyDemoEnabled          = "site.demo.enabled"
	keyDemoRelayPerIPDaily  = "site.demo.relay_quota.per_ip_daily"
	keyDemoRelayGlobalDaily = "site.demo.relay_quota.global_daily"

	// problemTypeDemoReadOnly 是 Q3 冻结的固定错误码（RFC 7807 type URI）。
	// 与 csrf_failed / password_change_required 同属 /api/* 的 problem 面。
	problemTypeDemoReadOnly = "https://newtg.dev/problems/demo_read_only"

	// 中继拒绝码沿用 writeRelayError 的 code 字段（OpenAI 信封）。
	demoRelayDisabledCode         = "demo_relay_disabled"
	demoRelayQuotaExceededCode    = "demo_relay_quota_exceeded"
	demoRelayQuotaUnavailableCode = "demo_relay_quota_unavailable"

	// 演示站中继日额度的默认值（§3.17.2 定案的实测数值）。缺键时回落这里；
	// 注册表仍是唯一真源，这里只是「注册表未演进」时的兜底，不构成第二份
	// 可编辑默认值。
	demoRelayPerIPDailyDefault  = 20
	demoRelayGlobalDailyDefault = 2000
)

// demoQuotaNow 便于测试固定「当前时间」；生产恒为 time.Now。
var demoQuotaNow = time.Now

// demoWriteAllowedPaths 是 Q2①（会话/认证类）与 Q2③（第三方回调）的
// 精确路径白名单。只有非安全方法需要查它；GET/HEAD 一律直接放行。
//
// 为什么不放行 POST /api/auth/register：注册是 Q1 deny-by-default 的一部分
// （§3.17.2「换号无法绕过」）——演示站访客只能使用安装向导预置的账号。
var demoWriteAllowedPaths = map[string]struct{}{
	// ① 会话/认证：登录（含 2FA 第二步）、登出、邮箱验证码、找回密码。
	// 少任何一个都会把访客锁在门外（§3.17.1 Q2 的裁决理由）。
	"/api/auth/login":                   {},
	"/api/auth/2fa/challenge":           {},
	"/api/logout":                       {},
	"/api/auth/email-code":              {},
	"/api/auth/password-reset/request":  {},
	"/api/auth/password-reset/validate": {},
	"/api/auth/password-reset/confirm":  {},
	// ③ 第三方回调：支付渠道服务器发起，不带访客会话；拒绝它们会让演示站
	// 的充值回调（若有）全部失败。验签与限流仍由 handler 自己执行。
	"/api/webhooks/stripe": {},
	"/api/webhooks/alipay": {},
	"/api/webhooks/epay":   {},
}

// demoWriteRouteFamilies 是 demo_write_rejected_total{route_family} 的
// 闭集。标签只能来自服务端白名单：原始路径由客户端控制，直接当标签会
// 让 /api/<garbage> 产生无界基数（指标包 §3.3 的护栏同理）。
var demoWriteRouteFamilies = map[string]struct{}{
	"auth": {}, "logout": {}, "sessions": {}, "me": {}, "self": {},
	"account": {}, "email": {}, "verify-email": {}, "2fa": {}, "password": {},
	"users": {}, "groups": {}, "settings": {}, "tokens": {}, "token": {},
	"orders": {}, "topup": {}, "wallet": {}, "redemption": {},
	"subscriptions": {}, "subscription-plans": {}, "subscription": {},
	"payment-methods": {}, "billing-preference": {},
	"logs": {}, "usage": {}, "audit": {},
	"announcements": {}, "notifications": {}, "notification-templates": {},
	"notification-preferences": {},
	"channels":                 {}, "model-pricing": {}, "group-pricing": {}, "model-catalog": {},
	"plugin": {}, "task": {}, "tasks": {}, "performance": {}, "deployments": {},
	"webhooks": {}, "pricing": {}, "setup": {}, "status": {}, "geoip": {},
	"home": {}, "rankings": {},
}

// DemoReadOnlyGuard 返回管理面只读守卫（§3.17.1 Q1/Q2/Q3）。
//
// 位置：包住整个 /api/* mux（routes.go 中 RequestIDMiddleware → noStoreAPI →
// 本守卫 → mux），因此在 SessionAuth/CSRF 之前生效——被拒的写请求不会先
// 经过会话校验，也不会触发任何业务副作用；这也意味着「未登录的写请求」
// 在演示站下得到 403 而不是 401，与 Q3 的统一拒绝形状一致。
//
// 关闭时（默认）只做一次配置读取后原样透传，行为与未接线完全相同。
func DemoReadOnlyGuard(config ConfigReader) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// 守卫只管 /api/*；Router 本身也只挂 /api/*，前缀判断用于防御
			// 未来在根 mux 挂静态资源等非管理面路径。
			if !strings.HasPrefix(r.URL.Path, "/api/") || !demoModeEnabled(config) {
				next.ServeHTTP(w, r)
				return
			}
			// 安全方法放行（§3.17.1 Q1：只拦 POST/PUT/PATCH/DELETE，
			// 不误伤 GET/HEAD；OPTIONS/TRACE 同属 RFC 9110 安全方法）。
			if isSafeMethod(r.Method) {
				next.ServeHTTP(w, r)
				return
			}
			if _, ok := demoWriteAllowedPaths[r.URL.Path]; ok {
				next.ServeHTTP(w, r)
				return
			}
			// Q6：被拒写入不写审计，只累加指标。
			metrics.ObserveDemoWriteRejected(demoWriteRouteFamily(r.URL.Path))
			writeProblemDetail(w, http.StatusForbidden, problemTypeDemoReadOnly,
				"Demo site is read-only", "演示站为只读：写操作已被服务端拒绝")
		})
	}
}

// demoModeEnabled 读取**有效**演示模式（设置值 + NEWTG_DEMO_ENABLED 覆盖）。
//
// 委托给 platformconfig.DemoEnabled：守卫、中继额度与计费豁免必须共用同一
// 实现（控制端裁决 I2），否则会出现「守卫按 env 关闭、额度仍按设置扣费」。
// 缺键/脏值一律按关闭处理：注册表默认是 false（§3.17 F-08-46 数据字段）。
func demoModeEnabled(config ConfigReader) bool {
	return platformconfig.DemoEnabled(config)
}

// demoWriteRouteFamily 把被拒路径归并为有限标签集；未知路径记 other。
func demoWriteRouteFamily(path string) string {
	rest := strings.TrimPrefix(path, "/api/")
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	if _, ok := demoWriteRouteFamilies[rest]; ok {
		return rest
	}
	return "other"
}

// demoRelayQuotaLimits 解析两层日额度。
//
// 与 ratelimit.Resolve 同一口径：缺键回落注册表默认；负值/JSON null/非整数
// 视为脏值 → 回落默认并告警。0 必须原样保留——它是设计里的「关闭中继」
// 语义（§3.17.2 fail-closed），不能被默认值覆盖。
func demoRelayQuotaLimits(config ConfigReader) (perIP, global int) {
	perIP = int(demoIntSetting(config, keyDemoRelayPerIPDaily, demoRelayPerIPDailyDefault))
	global = int(demoIntSetting(config, keyDemoRelayGlobalDaily, demoRelayGlobalDailyDefault))
	return perIP, global
}

func demoIntSetting(config ConfigReader, key string, fallback int64) int64 {
	// 注册表默认优先于本地兜底：默认值只写一处（settings 包），这里只在
	// 注册表尚未演进时使用本地常量，避免两个可编辑真源。
	if setting, ok := settings.Lookup(key); ok {
		if value, ok := settings.DefaultValue(setting).(int64); ok {
			fallback = value
		}
	}
	if config == nil {
		return fallback
	}
	raw, ok := config.Get(key)
	if !ok {
		return fallback
	}
	var value *int64
	if err := json.Unmarshal(raw, &value); err != nil || value == nil {
		slog.Warn("demo relay quota: invalid setting; falling back to default",
			"key", key, "fallback", fallback)
		return fallback
	}
	if *value < 0 {
		slog.Warn("demo relay quota: negative setting; falling back to default",
			"key", key, "value", *value, "fallback", fallback)
		return fallback
	}
	return *value
}

// checkDemoRelayQuota 在演示站开启时消耗两层日额度（§3.17.2）。
//
// 顺序：先每 IP、后全站。每 IP 被拒绝时不再自增全站计数器（该请求根本
// 不会被服务，不应计入「全站已服务次数」）；全站计数器是保险丝，任何
// 后续 IP 都会继续撞上它。
//
// 被拒绝时返回 429 + Retry-After（§3.17.2「任一层耗尽即 429」），并累加
// demo_relay_quota_exceeded_total{scope}。两个上限任一为 0 ⇒ 403 整体关闭
// （保留定案前的 fail-closed 行为）；Redis 故障 ⇒ 503，绝不放开为无限量。
//
// 调用点固定在 checkRelayRateLimit 的最前面：入口在 BuildRelayContext 与
// 分组解析之后，因此被拒请求零预扣、零 usage_logs、零上游调用（R-M5-04）。
func checkDemoRelayQuota(
	ctx context.Context,
	limiter RelayRateLimiter,
	config ConfigReader,
	requestID string,
	clientIP string,
) *relayRateLimitRejection {
	if !demoModeEnabled(config) {
		return nil
	}
	perIP, global := demoRelayQuotaLimits(config)
	if perIP <= 0 || global <= 0 {
		// 配置级 fail-closed：0 不是「不限」，而是「中继整体关闭」。
		// 与 ratelimit.Allow 的 0=不限 语义刻意相反——这里 0 是安全阀。
		return &relayRateLimitRejection{
			Status:  http.StatusForbidden,
			Code:    demoRelayDisabledCode,
			Message: "demo relay is disabled",
		}
	}
	if limiter == nil {
		// 生产装配缺 Redis 时 newRelayRateLimiter 会让启动失败；走到这里
		// 说明是降级/测试装配。演示站不能因此变成「无限量」，fail-closed。
		slog.Error("demo relay quota limiter is not configured; denying request (fail-closed)",
			"request_id", requestID)
		return demoQuotaUnavailable()
	}

	date, window := demoQuotaWindow(demoQuotaNow(), demoQuotaLocation(config))
	if ip := strings.TrimSpace(clientIP); ip != "" {
		clientIP = ip
	} else {
		// 地址不可解析时不能共享一个「空 IP」桶把所有人都限死，也不能
		// 跳过限额；把空值哈希成固定桶是保守折中（与 clientip 的
		// fail-closed 取值一致：该请求仍受全站保险丝约束）。
		clientIP = "unknown"
	}

	steps := []struct {
		scope string
		limit int
		key   string
	}{
		// 键形状逐字冻结（§3.17.2）：demo:relay:{yyyymmdd}:ip:{sha256(ip)}。
		// IP 只以哈希出现，不落明文（security-09 T-3 类数据纪律）。
		{"ip", perIP, "demo:relay:" + date + ":ip:" + demoIPHash(clientIP)},
		{"global", global, "demo:relay:" + date + ":global"},
	}

	for _, step := range steps {
		decision, err := limiter.Allow(ctx, ratelimit.Bucket{
			Name:     "demo_" + step.scope,
			Limit:    step.limit,
			Window:   window,
			FailMode: ratelimit.FailClosed,
		}, step.key, 1)
		if err != nil {
			// 配额读数失败不是「没有超限」。演示站的真实上游成本必须有上限，
			// 故 503 拒绝（任务冻结口径；中继自身 B10–B12 故障仍是 500）。
			slog.Error("demo relay quota check failed; denying request (fail-closed)",
				"scope", step.scope, "request_id", requestID, "error", err)
			return demoQuotaUnavailable()
		}
		if decision.Allowed {
			continue
		}
		metrics.ObserveDemoRelayQuotaExceeded(step.scope)
		seconds := retrySeconds(decision.RetryAfter)
		message := "daily demo relay quota for this IP is exhausted; retry in " + seconds + "s"
		if step.scope == "global" {
			message = "global daily demo relay quota is exhausted; retry in " + seconds + "s"
		}
		slog.Info("demo relay quota exceeded",
			"scope", step.scope, "limit", step.limit,
			"retry_after_s", seconds, "status_code", http.StatusTooManyRequests,
			"request_id", requestID)
		return &relayRateLimitRejection{
			Status:     http.StatusTooManyRequests,
			Code:       demoRelayQuotaExceededCode,
			Message:    message,
			RetryAfter: decision.RetryAfter,
		}
	}
	return nil
}

func demoQuotaUnavailable() *relayRateLimitRejection {
	return &relayRateLimitRejection{
		Status:  http.StatusServiceUnavailable,
		Code:    demoRelayQuotaUnavailableCode,
		Message: "demo relay quota check is unavailable",
	}
}

// demoQuotaWindow 返回站点时区下的当日键与「到次日 00:00」的剩余窗口。
//
// 用日期分段 + 首次自增设 TTL 的双保险实现「跨零点自动归零、无定时任务」
// （§3.17.2）。窗口只影响首个请求设置的 TTL；日期键本身已保证次日换桶，
// 所以即使 TTL 因时钟偏移略长也不会把昨天的计数带进今天。
func demoQuotaWindow(now time.Time, loc *time.Location) (string, time.Duration) {
	if loc == nil {
		loc = time.UTC
	}
	local := now.In(loc)
	next := time.Date(local.Year(), local.Month(), local.Day()+1, 0, 0, 0, 0, loc)
	window := next.Sub(now)
	if window <= 0 {
		// 理论上不可达（next 严格晚于 now）；兜底避免 PEXPIRE 0 立刻删键。
		window = time.Minute
	}
	return local.Format("20060102"), window
}

// demoQuotaLocation 读取站点时区（site.timezone）。
//
// 回退顺序：设置值 → 注册表默认值（仅当设置为空时使用）→ 以上任一不是
// 合法 IANA 名称时回落 UTC。注意非法名称**不会**再回落注册表默认：设置
// 已经给出一个（错的）值，再猜一个默认时区只会让「当日键」静默漂移。
func demoQuotaLocation(config ConfigReader) *time.Location {
	return siteLocation(config, nil)
}

// CheckinSiteLocation 是 W15-C 签到「自然日」判定复用的站点时区解析
// （导出版本，附带非法时区告警）。与 demoQuotaLocation 共用同一实现，
// 避免两处对「今天」得出不同答案（I-19）。
func CheckinSiteLocation(config ConfigReader, log *slog.Logger) *time.Location {
	return siteLocation(config, log)
}

func siteLocation(config ConfigReader, log *slog.Logger) *time.Location {
	name := ""
	if config != nil {
		if raw, ok := config.Get("site.timezone"); ok {
			var value string
			if json.Unmarshal(raw, &value) == nil {
				name = strings.TrimSpace(value)
			}
		}
	}
	if name == "" {
		if setting, ok := settings.Lookup("site.timezone"); ok {
			if value, ok := settings.DefaultValue(setting).(string); ok {
				name = strings.TrimSpace(value)
			}
		}
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		if log != nil {
			log.Warn("site.timezone is not a valid IANA name; falling back to UTC",
				"timezone", name, "error", err)
		}
		return time.UTC
	}
	return loc
}

// demoIPHash 返回 IP 的 SHA-256 十六进制（Redis 键里不落明文地址）。
func demoIPHash(ip string) string {
	sum := sha256.Sum256([]byte(ip))
	return hex.EncodeToString(sum[:])
}
