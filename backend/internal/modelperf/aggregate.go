// Package modelperf 负责模型性能测速的聚合与定时调度。
//
// 聚合口径与手工测速共用（success_rate 分母是请求轮数；分位数只取成功且
// 非 nil 的样本，nearest-rank；TPS/输出 token 只对成功样本求平均；无样本
// 为 nil 而不是 0），因此从 httpapi 中抽出，避免两套口径漂移。
package modelperf

import (
	"sort"

	"newtg/gateway/internal/channelhealth"
	"newtg/gateway/internal/store"
)

// Aggregate 把逐次观测投影为汇总事实 + 明细行。
//
// runs 是请求的探测次数（成功率的固定分母）；成功/样本口径按
// PerfRunResult.Success 判定，失败 run 即使携带了部分字段也不进入聚合。
// createdBy 为 nil 表示定时任务等非人工触发，落库时 created_by 为空。
func Aggregate(ch *store.Channel, model string, runs int,
	results []channelhealth.PerfRunResult, createdBy *int64) (store.ModelPerformanceTest, []store.ModelPerformanceRun) {

	test := store.ModelPerformanceTest{
		ChannelID: ch.ID,
		Model:     model,
		Provider:  ch.Type,
		Runs:      runs,
		CreatedBy: createdBy,
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
