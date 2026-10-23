package httpapi

import (
	"context"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"newtg/gateway/internal/channelhealth"
	"newtg/gateway/internal/store"
)

// 本文件实现渠道测速管理端点（W16 模型性能测试）：
//
//	POST /api/channels/{id}/performance-tests   channels:manage + 渠道测试限流桶
//	GET  /api/channels/{id}/performance-tests   channels:read
//
// POST 通过 Performer 接缝执行一次真实流式探测批次（生产实现
// channelhealth.PerfRunner），把逐次观测与汇总**同一事务**落库后返回落库结果；
// 任何一步失败都不返回未经持久化的聚合值（「先返回后写库」会让管理台看到
// 已消失的测试）。
//
// 聚合口径（与 platform-05 §3.5 一致）：
//   - success_rate = success_count / runs；
//   - TTFT/延迟分位数只取**成功且非 nil** 的样本，nearest-rank（store.Percentile）；
//   - TPS/输出 token 均值只对成功且非 nil 的样本求平均；
//   - 无样本 → null（不是 0），前端据此显示「—」。
//
// GC-6：响应与错误体只含白名单数字/枚举；上游 URL、凭据、错误文本永不回显。

const (
	// channelPerfDefaultRuns 是请求省略 runs 时的探测次数。
	channelPerfDefaultRuns = 3
	// channelPerfMinRuns / channelPerfMaxRuns 是管理面单次测速的边界。
	// 与 channelhealth.PerfMaxRuns(10) 不同：手工测速按 5 次封顶，避免一次
	// 点击消耗过多上游配额；真实实现仍会自行钳制。
	channelPerfMinRuns = 1
	channelPerfMaxRuns = 5
	// channelPerfDefaultListLimit / channelPerfMaxListLimit 是历史查询边界。
	channelPerfDefaultListLimit = 20
	channelPerfMaxListLimit     = 100
	// problemTypeChannelPerformanceUnavailable 表示 Performer 或存储未装配。
	problemTypeChannelPerformanceUnavailable = "https://newtg.dev/problems/channel_performance_unavailable"
)

// ChannelPerformer 是测速执行接缝。生产实现是 (*channelhealth.PerfRunner).Run；
// 测试注入确定性样本，避免依赖真实上游。
type ChannelPerformer func(ctx context.Context, ch *store.Channel, model string, runs int) []channelhealth.PerfRunResult

type channelPerformanceTestRequest struct {
	Model string `json:"model"`
	// Runs 省略（0）时取默认 3；显式值必须落在 1..5。
	Runs int `json:"runs"`
}

// performanceTestResponse 是 POST/GET 的稳定响应投影。可空聚合字段显式输出
// null（不使用 omitempty）：前端需要区分「无样本」与「字段缺失」。
type performanceTestResponse struct {
	ID              int64    `json:"id"`
	ChannelID       int64    `json:"channel_id"`
	Model           string   `json:"model"`
	Provider        string   `json:"provider"`
	Runs            int      `json:"runs"`
	SuccessCount    int      `json:"success_count"`
	SuccessRate     float64  `json:"success_rate"`
	TTFTMSP50       *int     `json:"ttft_ms_p50"`
	TTFTMSP95       *int     `json:"ttft_ms_p95"`
	LatencyMSP50    *int     `json:"latency_ms_p50"`
	LatencyMSP95    *int     `json:"latency_ms_p95"`
	TPSAvg          *float64 `json:"tps_avg"`
	OutputTokensAvg *float64 `json:"output_tokens_avg"`
	CreatedAt       string   `json:"created_at"`
}

type performanceTestListResponse struct {
	Items []performanceTestResponse `json:"items"`
}

// handleCreatePerformanceTest 执行一次测速并落库。
func (h *ChannelHandler) handleCreatePerformanceTest(w http.ResponseWriter, r *http.Request) {
	if !h.authorize(w, r, PermChannelsManage) {
		return
	}
	id, ok := channelIDFromRequest(r)
	if !ok {
		writeAPIError(w, channelNotFoundError())
		return
	}
	// 与连通性测试共用「10 次/分钟/管理员」桶（modules-08 F-08-16）：两者都是
	// 会真实消耗上游配额的手工探针。Redis 不可用时 fail-open（只记 WARN）——
	// 测速不是敏感数据出口，不能让 Redis 抖动把管理动作整体打成 503。
	if h.rdb != nil {
		over, retryAfter, err := allowRequestRate(r.Context(), h.rdb,
			ChannelTestRateLimitKey(actorID(r)), ChannelTestRateLimit, ChannelTestRateWindow)
		if err != nil {
			h.log.Warn("channel performance rate limit check failed; allowing request (fail-open)", "error", err)
		} else if over {
			writeRateLimited(w, retryAfter, "Too many channel performance test requests")
			return
		}
	} else {
		h.log.Warn("channel performance rate limit store is not configured; allowing request (fail-open)")
	}

	var req channelPerformanceTestRequest
	if err := decodeChannelJSON(r, &req); err != nil {
		writeAPIError(w, invalidRequestError("request body is not valid JSON"))
		return
	}
	ch, err := h.channels.Get(r.Context(), id)
	if err != nil {
		writeAPIError(w, storeErrorResponse(err))
		return
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		writeAPIError(w, validationError("model", "model is required"))
		return
	}
	if !slices.Contains(decodeStringSlice(ch.SupportedModels), model) {
		writeAPIError(w, validationError("model", "model must be one of supported_models"))
		return
	}
	runs := req.Runs
	if runs == 0 {
		runs = channelPerfDefaultRuns
	}
	if runs < channelPerfMinRuns || runs > channelPerfMaxRuns {
		writeAPIError(w, validationError("runs", "runs must be between 1 and 5"))
		return
	}
	if h.performer == nil || h.performance == nil {
		// 未装配就是不可用：绝不返回伪造的 0 样本结果（R-P10）。
		writeAPIError(w, &apiError{
			Status: http.StatusServiceUnavailable,
			Type:   problemTypeChannelPerformanceUnavailable,
			Title:  "Channel performance test unavailable",
		})
		return
	}

	results := h.performer(r.Context(), ch, model, runs)
	test, runRows := aggregatePerformanceTest(ch, model, runs, results, actorID(r))
	persisted, err := h.performance.Create(r.Context(), test, runRows)
	if err != nil {
		// 落库失败不返回聚合值：调用方必须能区分「测过并保存」与「测过但丢了」。
		h.log.Error("persist channel performance test failed",
			"channel_id", id, "model", model, "error", err)
		writeAPIError(w, internalAPIError())
		return
	}
	writeChannelJSON(w, http.StatusOK, performanceResponseFrom(persisted))
}

// handleListPerformanceTests 返回某渠道最近的历史测试（新→旧）。
func (h *ChannelHandler) handleListPerformanceTests(w http.ResponseWriter, r *http.Request) {
	if !h.authorize(w, r, PermChannelsRead) {
		return
	}
	id, ok := channelIDFromRequest(r)
	if !ok {
		writeAPIError(w, channelNotFoundError())
		return
	}
	query := r.URL.Query()
	model := strings.TrimSpace(query.Get("model"))
	limit := channelPerfDefaultListLimit
	if raw := strings.TrimSpace(query.Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > channelPerfMaxListLimit {
			writeAPIError(w, validationError("limit", "limit must be between 1 and 100"))
			return
		}
		limit = parsed
	}
	// 渠道不存在必须 404，且先于存储查询（不把 404 伪装成空列表）。
	if _, err := h.channels.Get(r.Context(), id); err != nil {
		writeAPIError(w, storeErrorResponse(err))
		return
	}
	if h.performance == nil {
		writeAPIError(w, &apiError{
			Status: http.StatusServiceUnavailable,
			Type:   problemTypeChannelPerformanceUnavailable,
			Title:  "Channel performance test unavailable",
		})
		return
	}
	items, err := h.performance.ListRecent(r.Context(), id, model, limit)
	if err != nil {
		h.log.Error("list channel performance tests failed", "channel_id", id, "error", err)
		writeAPIError(w, internalAPIError())
		return
	}
	resp := performanceTestListResponse{Items: make([]performanceTestResponse, 0, len(items))}
	for _, item := range items {
		resp.Items = append(resp.Items, performanceResponseFrom(item))
	}
	writeChannelJSON(w, http.StatusOK, resp)
}

// aggregatePerformanceTest 把逐次观测投影为汇总事实 + 明细行。
//
// runs 是请求的探测次数（成功率的固定分母）；成功/样本口径按
// PerfRunResult.Success 判定，失败 run 即使携带了部分字段也不进入聚合。
func aggregatePerformanceTest(ch *store.Channel, model string, runs int,
	results []channelhealth.PerfRunResult, createdBy int64) (store.ModelPerformanceTest, []store.ModelPerformanceRun) {

	test := store.ModelPerformanceTest{
		ChannelID: ch.ID,
		Model:     model,
		Provider:  ch.Type,
		Runs:      runs,
		CreatedBy: &createdBy,
	}
	runRows := make([]store.ModelPerformanceRun, 0, len(results))
	var ttft, latency, tokens []int
	var tps []float64

	for i, result := range results {
		runRows = append(runRows, store.ModelPerformanceRun{
			RunIndex:     i,
			Success:      result.Success,
			TTFTMS:       result.TTFTMS,
			LatencyMS:    result.LatencyMS,
			OutputTokens: result.OutputTokens,
			TPS:          result.TPS,
			ErrorClass:   result.ErrorClass,
		})
		if !result.Success {
			continue
		}
		test.SuccessCount++
		if result.TTFTMS != nil {
			ttft = append(ttft, *result.TTFTMS)
		}
		if result.LatencyMS != nil {
			latency = append(latency, *result.LatencyMS)
		}
		if result.OutputTokens != nil {
			tokens = append(tokens, *result.OutputTokens)
		}
		if result.TPS != nil {
			tps = append(tps, *result.TPS)
		}
	}
	if runs > 0 {
		test.SuccessRate = float64(test.SuccessCount) / float64(runs)
	}
	test.TTFTMSP50 = percentileOf(ttft, 0.50)
	test.TTFTMSP95 = percentileOf(ttft, 0.95)
	test.LatencyMSP50 = percentileOf(latency, 0.50)
	test.LatencyMSP95 = percentileOf(latency, 0.95)
	test.TPSAvg = averageFloat(tps)
	test.OutputTokensAvg = averageInt(tokens)
	return test, runRows
}

// percentileOf 对样本排序后取 nearest-rank 分位数；无样本返回 nil。
func percentileOf(samples []int, p float64) *int {
	if len(samples) == 0 {
		return nil
	}
	sorted := append([]int(nil), samples...)
	sort.Ints(sorted)
	value, ok := store.Percentile(sorted, p)
	if !ok {
		return nil
	}
	return &value
}

func averageFloat(samples []float64) *float64 {
	if len(samples) == 0 {
		return nil
	}
	var sum float64
	for _, sample := range samples {
		sum += sample
	}
	avg := sum / float64(len(samples))
	return &avg
}

func averageInt(samples []int) *float64 {
	if len(samples) == 0 {
		return nil
	}
	var sum int
	for _, sample := range samples {
		sum += sample
	}
	avg := float64(sum) / float64(len(samples))
	return &avg
}

func performanceResponseFrom(test store.ModelPerformanceTest) performanceTestResponse {
	createdAt := ""
	if !test.CreatedAt.IsZero() {
		createdAt = test.CreatedAt.UTC().Format(time.RFC3339)
	}
	return performanceTestResponse{
		ID:              test.ID,
		ChannelID:       test.ChannelID,
		Model:           test.Model,
		Provider:        test.Provider,
		Runs:            test.Runs,
		SuccessCount:    test.SuccessCount,
		SuccessRate:     test.SuccessRate,
		TTFTMSP50:       test.TTFTMSP50,
		TTFTMSP95:       test.TTFTMSP95,
		LatencyMSP50:    test.LatencyMSP50,
		LatencyMSP95:    test.LatencyMSP95,
		TPSAvg:          test.TPSAvg,
		OutputTokensAvg: test.OutputTokensAvg,
		CreatedAt:       createdAt,
	}
}
