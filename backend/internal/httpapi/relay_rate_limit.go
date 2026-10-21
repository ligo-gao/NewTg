// 本文件实现 W7/M5-b：中继准入限流（B11 → B10 → B12）与 429/500 错误出口。
//
// 设计权威：security-07 §2.2.2（键/维度/短路）、relay-01 §4（429/500 形状）、
// modules-08 F-08-16（四个 rl 键的默认值与范围）。执行点由 R-M5-04 冻结：
// 每个入口在 BuildRelayContext + 分组解析之后、pin/白名单/routeDeps/预扣/
// 上游之前——被拒绝的请求因此零预扣、零 usage_logs、零上游调用。
package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/ratelimit"
)

// relayRateLimitWindow 是固定窗口长度（security-07 §2.2.2：1 分钟，无窗口
// 时长设置键）。1 分钟是唯一冻结值，不得做成可配置项。
const relayRateLimitWindow = time.Minute

// RelayRateLimiter 是中继准入限流的唯一接缝。
//
// 生产注入 *ratelimit.Limiter（Redis 固定窗口，跨副本共享计数）；测试注入
// 同一实现指向 miniredis。这里用窄接口而不是具体类型，是为了让「未装配」
// （nil）显式表达：单元测试与降级装配不该为了跑通转发而被迫准备 Redis。
//
// nil 表示**跳过限流**，与装配完成后的「配置全 0」不是同一件事：后者仍会
// 逐桶求值（只是阈值 0 不访问 Redis）。nil 只允许出现在测试/降级装配里；
// 生产装配由 platform.newRelayRateLimiter 保证非 nil——缺失 Redis 会让启动
// 失败，而不是静默把三档桶全部关掉。
type RelayRateLimiter interface {
	Allow(ctx context.Context, bucket ratelimit.Bucket, key string, cost int) (ratelimit.Decision, error)
}

// relayRateLimitRejection 描述一次被限流接缝拒绝的请求。
//
// 只承载固定文案与状态码：调用方按 Status 写 OpenAI 错误信封，不在这里直接
// 写响应——入口 handler 才知道本次请求的 requestID（X-Request-ID 一致性）。
type relayRateLimitRejection struct {
	Status     int
	Code       string
	Message    string
	RetryAfter time.Duration
	// Bucket 只用于日志（B11/B10/B12），不进入响应体。
	Bucket string
	Limit  int
}

// checkRelayRateLimit 按冻结顺序求值三个桶，任一超限立即返回拒绝。
//
// 顺序（security-07 §2.2.2；R-M5-05 短路）：B11 per-token → B10 per-user
// （含 group_overrides，取 rc.UsingGroup）→ B12 per-user global。短路是有意的：
// 已被拒绝的请求不再自增后续桶，Redis 往返也最少；对客户端可见行为无影响。
//
// 四项边界语义：
//   - 0 = 不限（modules-08 F-08-16）：阈值 0 的桶由 ratelimit.Allow 短路，
//     不访问 Redis、不建键；
//   - /pg 跳过 B11（R-M5-08）：会话身份没有令牌，principal.TokenID == nil；
//   - 管理员不豁免（R-M5-13）：本函数不看 Role，任何身份都走同一套桶；
//   - 后端故障 fail-closed（R-M5-10）：任一桶返回 error → 500
//     rate_limit_check_failed，绝不降级放行（放行的代价是上游配额与真实资金）。
func checkRelayRateLimit(
	ctx context.Context,
	limiter RelayRateLimiter,
	config ConfigReader,
	rc *contract.RelayContext,
	principal *auth.Principal,
) *relayRateLimitRejection {
	if limiter == nil || principal == nil || rc == nil {
		return nil
	}
	// 每次请求解析阈值：配置中心保证 ≤1s 生效（security-07 §2.2.3 第 3 条），
	// 且计数不随阈值变更重置——计数只在 Redis，配置只影响本次比较的上限。
	settings := ratelimit.Resolve(config, slog.Default())

	type bucketStep struct {
		name  string
		limit int
		key   string
	}
	steps := make([]bucketStep, 0, 3)
	// B11 先判：令牌是最窄的维度，per-token 超限不应继续消耗用户维度的名额。
	if principal.TokenID != nil {
		steps = append(steps, bucketStep{
			name:  ratelimit.BucketToken,
			limit: settings.TokenLimit(),
			key:   ratelimit.TokenKey(*principal.TokenID),
		})
	}
	steps = append(steps,
		// B10：阈值可被 rl.group_overrides 按分组覆盖（R-M5-06）。取 rc.UsingGroup
		// 而不是 principal 的令牌字面分组：auto 令牌在 BuildRelayContext 里已解析
		// 为首组，/pg 在 applyPlaygroundSession 里已回源用户分组。
		bucketStep{
			name:  ratelimit.BucketUserModel,
			limit: settings.UserModelLimit(rc.UsingGroup),
			key:   ratelimit.UserKey(principal.UserID),
		},
		// B12：中继全站桶，不被分组覆盖。
		bucketStep{
			name:  ratelimit.BucketUserGlobal,
			limit: settings.UserGlobalLimit(),
			key:   ratelimit.UserGlobalKey(principal.UserID),
		},
	)

	logArgs := func(step bucketStep) []any {
		args := []any{
			"bucket", step.name,
			"user_id", principal.UserID,
			"group", rc.UsingGroup,
			"limit", step.limit,
			"request_id", rc.RequestID,
		}
		if principal.TokenID != nil {
			args = append(args, "token_id", *principal.TokenID)
		}
		return args
	}

	for _, step := range steps {
		decision, err := limiter.Allow(ctx, ratelimit.Bucket{
			Name:   step.name,
			Limit:  step.limit,
			Window: relayRateLimitWindow,
			// FailMode 只由 ratelimit 回填到 Decision；中继面的策略固定
			// fail-closed（R-M5-10），见下方 err 分支。
			FailMode: ratelimit.FailClosed,
		}, step.key, 1)
		if err != nil {
			// fail-closed：Redis/限流器故障不是「没有超限」，而是「无法判断是否
			// 超限」。放行会把上游配额与真实资金暴露给一个不受控的入口；按冻结
			// 契约返回 500（不是 429/503），客户端不会把它误读成「重试即可」。
			slog.Error("relay rate limit check failed; denying request (fail-closed)",
				append(logArgs(step), "error", err)...)
			return &relayRateLimitRejection{
				Status:  http.StatusInternalServerError,
				Code:    "rate_limit_check_failed",
				Message: "rate limit check failed",
				Bucket:  step.name,
				Limit:   step.limit,
			}
		}
		if decision.Allowed {
			continue
		}
		// Retry-After 与文案取同一个整秒值（Decision.RetryAfter 已向上取整，
		// retrySeconds 再取整是幂等的），保证头与正文不出现「差 1 秒」。
		seconds := retrySeconds(decision.RetryAfter)
		slog.Info("relay rate limit exceeded",
			append(logArgs(step), "retry_after_s", seconds, "status_code", http.StatusTooManyRequests)...)
		return &relayRateLimitRejection{
			Status:     http.StatusTooManyRequests,
			Code:       "rate_limit_exceeded",
			Message:    "rate limit exceeded; retry in " + seconds + "s",
			RetryAfter: decision.RetryAfter,
			Bucket:     step.name,
			Limit:      step.limit,
		}
	}
	return nil
}

// writeRelayRateLimitError 写中继信封的 429/500。
//
// requestID 必须传 rc.RequestID（而不是 handler 局部的另一个 ID）：429 的日志
// 用同一个值记录，计费幂等键也来自它。两个 ID 并存会让客户端报障时只能对上
// 响应或日志之一（M5-b 复核 F3）。
//
// Retry-After 只在 429 上出现：它是「何时可重试」的客户端信号，500 是限流器
// 故障（R-M5-10 明确不设该头，避免客户端把内部故障当限流重试）。
// 响应形状沿用 writeRelayError（OpenAI 信封 + X-Request-ID，relay-01 §3.6），
// 不使用 /api 的 problem+json。
func writeRelayRateLimitError(w http.ResponseWriter, requestID string, rejection *relayRateLimitRejection) {
	if rejection == nil {
		return
	}
	if rejection.Status == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", retrySeconds(rejection.RetryAfter))
	}
	writeRelayError(w, rejection.Status, requestID, rejection.Code, rejection.Message)
}
