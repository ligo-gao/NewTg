package httpapi

// 时段定价策略的管理面验收（计划 Task 5）：
//   - 候选并集（catalog / channel / pricing 去重）与 priced 标记；
//   - 保存期校验：strategy 与 billing_expr 互斥、未开放字段拒绝、字段路径；
//   - 策略试算：高峰/闲时的生效倍率、单价与总价；
//   - 详情返回完整策略，列表只返回摘要；显式 null 清除策略。
//
// 依赖内存假仓储与 miniredis，不触碰外部 PG（SQL 的并集/版本由集成测试覆盖）。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"newtg/gateway/internal/pricingstrategy"
	"newtg/gateway/internal/store"
)

// timeStrategyFixture 是覆盖工作日两个高峰窗口的合法策略草稿。
func timeStrategyFixture() map[string]any {
	return map[string]any{
		"version": 1,
		"time": map[string]any{
			"enabled":            true,
			"timezone":           "Asia/Shanghai",
			"holiday_policy":     "none",
			"default_multiplier": 0.5,
			"windows": []any{
				map[string]any{
					"name": "上午高峰", "days": []int{1, 2, 3, 4, 5},
					"start": "09:00", "end": "12:00", "multiplier": 1.0,
				},
				map[string]any{
					"name": "下午高峰", "days": []int{1, 2, 3, 4, 5},
					"start": "14:00", "end": "18:00", "multiplier": 1.0,
				},
			},
		},
	}
}

// fixedHolidayLookup 是节假日查表的固定答案。
type fixedHolidayLookup struct {
	holiday pricingstrategy.Holiday
	found   bool
	err     error
}

func (f fixedHolidayLookup) LookupHoliday(context.Context, string, time.Time) (pricingstrategy.Holiday, bool, error) {
	return f.holiday, f.found, f.err
}

func TestPricingCandidatesUnionAndDedupe(t *testing.T) {
	env := newPricingTestEnv(t)
	displayName := "GPT-5 Turbo"
	env.repo.catalogModels["openai/gpt-5"] = store.PricingCandidate{
		DisplayName: &displayName, ContextLength: publicInt64Ptr(128000),
	}
	env.repo.channelModels["channel-only"] = true
	// 三个来源同时命中的模型必须在结果里只出现一次。
	env.repo.catalogModels["all/sources"] = store.PricingCandidate{ContextLength: publicInt64Ptr(32000)}
	env.repo.channelModels["all/sources"] = true
	env.repo.models["all/sources"] = &store.ModelPricingRecord{
		ID: 7, Model: "all/sources", Mode: 1, Status: 1,
		PromptPrice: publicInt64Ptr(200), Author: "test", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	env.repo.channelModels["channel-only"] = true

	rr := env.request(fullPricingAdmin(), http.MethodGet, "/api/model-pricing/candidates", nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d（body=%q）", rr.Code, rr.Body.String())
	}
	var list pricingCandidateListView
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("候选响应非法: %v（body=%q）", err, rr.Body.String())
	}
	byModel := make(map[string]pricingCandidateView, len(list.Items))
	for _, item := range list.Items {
		if _, dup := byModel[item.Model]; dup {
			t.Fatalf("候选 %q 重复出现", item.Model)
		}
		byModel[item.Model] = item
	}

	catalogOnly, ok := byModel["openai/gpt-5"]
	if !ok {
		t.Fatalf("目录来源的模型缺失: %+v", list.Items)
	}
	if catalogOnly.Priced || catalogOnly.DisplayName == nil || *catalogOnly.DisplayName != displayName {
		t.Fatalf("目录候选字段错误: %+v", catalogOnly)
	}
	if len(catalogOnly.Sources) != 1 || catalogOnly.Sources[0] != store.PricingCandidateSourceCatalog {
		t.Fatalf("目录候选 sources = %v", catalogOnly.Sources)
	}

	channelOnly, ok := byModel["channel-only"]
	if !ok || len(channelOnly.Sources) != 1 || channelOnly.Sources[0] != store.PricingCandidateSourceChannel {
		t.Fatalf("渠道候选错误: %+v", channelOnly)
	}

	all, ok := byModel["all/sources"]
	if !ok {
		t.Fatal("三来源模型缺失")
	}
	if !all.Priced || all.Mode != "per_request" {
		t.Fatalf("已定价候选的 priced/mode 错误: %+v", all)
	}
	if len(all.Sources) != 3 ||
		all.Sources[0] != store.PricingCandidateSourceCatalog ||
		all.Sources[1] != store.PricingCandidateSourceChannel ||
		all.Sources[2] != store.PricingCandidateSourcePricing {
		t.Fatalf("三来源 sources = %v", all.Sources)
	}

	// 搜索是大小写不敏感子串。
	rr = env.request(fullPricingAdmin(), http.MethodGet, "/api/model-pricing/candidates?keyword=GPT", nil, nil)
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("搜索响应非法: %v", err)
	}
	if len(list.Items) != 1 || list.Items[0].Model != "openai/gpt-5" {
		t.Fatalf("keyword=GPT 结果 = %+v", list.Items)
	}
}

func TestPricingCreateRejectsStrategyWithExpression(t *testing.T) {
	env := newPricingTestEnv(t)
	rr := env.request(fullPricingAdmin(), http.MethodPost, "/api/model-pricing", map[string]any{
		"model":        "gpt-strategy-expr",
		"billing_expr": "p",
		"strategy":     timeStrategyFixture(),
	}, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d，期望 400（body=%q）", rr.Code, rr.Body.String())
	}
	problem := decodePricingProblem(t, rr)
	fields, _ := problem["fields"].([]any)
	if len(fields) != 2 {
		t.Fatalf("problem.fields = %v，期望 strategy 与 billing_expr", problem["fields"])
	}
	if _, ok := env.repo.models["gpt-strategy-expr"]; ok {
		t.Fatal("互斥配置不得落库")
	}
}

func TestPricingCreateRejectsUnsupportedStrategyFields(t *testing.T) {
	env := newPricingTestEnv(t)

	cases := []struct {
		name   string
		mutate func(map[string]any)
		field  string
	}{
		{
			name:   "tiers",
			mutate: func(s map[string]any) { s["tiers"] = []any{} },
			field:  "strategy.tiers",
		},
		{
			name: "bad timezone",
			mutate: func(s map[string]any) {
				s["time"].(map[string]any)["timezone"] = "Mars/Olympus"
			},
			field: "strategy.time.timezone",
		},
		{
			name: "bad window clock",
			mutate: func(s map[string]any) {
				window := s["time"].(map[string]any)["windows"].([]any)[0].(map[string]any)
				window["start"] = "9:00"
			},
			field: "strategy.time.windows[0].start",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			strategy := timeStrategyFixture()
			tc.mutate(strategy)
			rr := env.request(fullPricingAdmin(), http.MethodPost, "/api/model-pricing", map[string]any{
				"model": "gpt-bad-strategy", "prompt_price": 100, "strategy": strategy,
			}, nil)
			if rr.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d，期望 422（body=%q）", rr.Code, rr.Body.String())
			}
			problem := decodePricingProblem(t, rr)
			fields, _ := problem["fields"].([]any)
			if len(fields) != 1 || fields[0] != tc.field {
				t.Fatalf("problem.fields = %v，期望 [%s]", problem["fields"], tc.field)
			}
			if _, ok := env.repo.models["gpt-bad-strategy"]; ok {
				t.Fatal("非法策略不得落库")
			}
		})
	}
}

func TestPricingPreviewStrategyPeakVsOffPeak(t *testing.T) {
	env := newPricingTestEnv(t)
	body := map[string]any{
		"model": "deepseek-flash",
		"mode":  "per_token",
		"prices": map[string]any{
			"prompt_price": 200, "completion_price": 800,
			"cache_read_price": 4, "cache_write_price": 250,
		},
		"strategy":          timeStrategyFixture(),
		"input_tokens":      128000,
		"output_tokens":     4000,
		"cache_read_tokens": 64000,
	}

	peak := previewStrategy(t, env, body, "2026-10-27T10:00:00+08:00")
	if peak.Match.Window != "上午高峰" || peak.Match.Multiplier != 1 {
		t.Fatalf("高峰命中错误: %+v", peak.Match)
	}
	if peak.Total.Quota <= 0 {
		t.Fatalf("高峰总价必须为正: %+v", peak.Total)
	}
	if peak.EffectiveRates.PromptPrice == nil || *peak.EffectiveRates.PromptPrice != 200 ||
		peak.EffectiveRates.CompletionPrice == nil || *peak.EffectiveRates.CompletionPrice != 800 ||
		peak.EffectiveRates.CacheReadPrice == nil || *peak.EffectiveRates.CacheReadPrice != 4 {
		t.Fatalf("高峰生效单价错误: %+v", peak.EffectiveRates)
	}
	if peak.Schedule == nil || peak.Schedule.Kind != "peak_offpeak" || len(peak.Schedule.Windows) != 2 {
		t.Fatalf("schedule 投影错误: %+v", peak.Schedule)
	}

	offPeak := previewStrategy(t, env, body, "2026-10-27T21:00:00+08:00")
	if offPeak.Match.Window != "" || offPeak.Match.Multiplier != 0.5 {
		t.Fatalf("闲时命中错误: %+v", offPeak.Match)
	}
	if offPeak.EffectiveRates.PromptPrice == nil || *offPeak.EffectiveRates.PromptPrice != 100 ||
		offPeak.EffectiveRates.CacheWritePrice == nil || *offPeak.EffectiveRates.CacheWritePrice != 125 {
		t.Fatalf("闲时生效单价错误: %+v", offPeak.EffectiveRates)
	}
	// 两档各自四舍五入到整数积分，因此允许 ±1 的取整差；倍率的 2:1 关系
	// 已由生效单价精确断言，这里只验证总价方向。
	if diff := peak.Total.Quota - 2*offPeak.Total.Quota; diff < -2 || diff > 2 {
		t.Fatalf("高峰/闲时总价应约为 2:1，得到 %d / %d", peak.Total.Quota, offPeak.Total.Quota)
	}
}

func TestPricingPreviewStrategyHolidayOffDay(t *testing.T) {
	lookup := fixedHolidayLookup{
		holiday: pricingstrategy.Holiday{Name: "国庆节", OffDay: true},
		found:   true,
	}
	env := newPricingTestEnvWithLookup(t, lookup)
	strategy := timeStrategyFixture()
	strategy["time"].(map[string]any)["holiday_policy"] = "cn_offpeak"
	body := map[string]any{
		"model":        "deepseek-flash",
		"mode":         "per_token",
		"prices":       map[string]any{"prompt_price": 200, "completion_price": 800},
		"strategy":     strategy,
		"input_tokens": 1_000_000,
	}

	// 2026-10-01 是周四，10:00 本应命中「上午高峰」，法定假日必须全天低谷。
	view := previewStrategy(t, env, body, "2026-10-01T10:00:00+08:00")
	if view.Match.Multiplier != 0.5 || view.Match.Window != "" {
		t.Fatalf("法定假日应按默认（低谷）倍率: %+v", view.Match)
	}
	if !view.Match.HolidayOffDay || view.Match.HolidayName != "国庆节" {
		t.Fatalf("节假日事实错误: %+v", view.Match)
	}
	if view.EffectiveRates.PromptPrice == nil || *view.EffectiveRates.PromptPrice != 100 {
		t.Fatalf("假日生效单价错误: %+v", view.EffectiveRates)
	}
}

func TestPricingPreviewStrategyDisabledSchedule(t *testing.T) {
	env := newPricingTestEnv(t)
	view := previewStrategy(t, env, map[string]any{
		"mode":         "per_token",
		"prices":       map[string]any{"prompt_price": 100},
		"input_tokens": 1000,
	}, "2026-10-27T10:00:00+08:00")
	if view.Match.Multiplier != 1 || view.Schedule != nil {
		t.Fatalf("无策略时应为倍率 1 + schedule null: %+v", view)
	}
}

func TestPricingStrategyDetailAndClear(t *testing.T) {
	env := newPricingTestEnv(t)
	rr := env.request(fullPricingAdmin(), http.MethodPost, "/api/model-pricing", map[string]any{
		"model": "deepseek-flash", "prompt_price": 200, "completion_price": 800,
		"strategy": timeStrategyFixture(),
	}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status = %d（body=%q）", rr.Code, rr.Body.String())
	}
	created := decodePricingDetail(t, rr)
	if created["has_strategy"] != true || created["strategy_kind"] != "peak_offpeak" {
		t.Fatalf("创建响应摘要错误: %+v", created)
	}
	strategy, _ := created["strategy"].(map[string]any)
	if strategy == nil || strategy["version"] != float64(1) {
		t.Fatalf("创建响应未返回规范化策略: %+v", created["strategy"])
	}

	// 列表只返回摘要，不返回完整策略。
	rr = env.request(fullPricingAdmin(), http.MethodGet, "/api/model-pricing", nil, nil)
	var list map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("列表 JSON 非法: %v", err)
	}
	items, _ := list["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items = %+v", list["items"])
	}
	item, _ := items[0].(map[string]any)
	if _, present := item["strategy"]; present {
		t.Fatal("列表不得返回完整 strategy")
	}
	if item["has_strategy"] != true || item["strategy_kind"] != "peak_offpeak" {
		t.Fatalf("列表摘要错误: %+v", item)
	}

	// GET 单条返回完整策略（编辑工作台回填）。
	rr = env.request(fullPricingAdmin(), http.MethodGet, "/api/model-pricing/deepseek-flash", nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("get status = %d（body=%q）", rr.Code, rr.Body.String())
	}
	detail := decodePricingDetail(t, rr)
	if detail["strategy"] == nil {
		t.Fatalf("详情缺少 strategy: %+v", detail)
	}

	// 显式 null 清除策略。
	updatedAt := detail["updated_at"]
	rr = env.request(fullPricingAdmin(), http.MethodPatch, "/api/model-pricing/deepseek-flash", map[string]any{
		"updated_at": updatedAt,
		"strategy":   nil,
	}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("clear status = %d（body=%q）", rr.Code, rr.Body.String())
	}
	cleared := decodePricingDetail(t, rr)
	if cleared["has_strategy"] != false || cleared["strategy_kind"] != "flat" || cleared["strategy"] != nil {
		t.Fatalf("清除后摘要错误: %+v", cleared)
	}
}

func TestPricingGetMissingIsNotFound(t *testing.T) {
	env := newPricingTestEnv(t)
	rr := env.request(fullPricingAdmin(), http.MethodGet, "/api/model-pricing/nope", nil, nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d，期望 404", rr.Code)
	}
}

// previewStrategy 调用试算端点并解码响应；非 200 直接失败。
func previewStrategy(t *testing.T, env *pricingTestEnv, body map[string]any, at string) pricingStrategyPreviewView {
	t.Helper()
	request := make(map[string]any, len(body)+1)
	for key, value := range body {
		request[key] = value
	}
	request["at"] = at
	rr := env.request(fullPricingAdmin(), http.MethodPost, "/api/model-pricing:preview-strategy", request, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("preview status = %d（body=%q）", rr.Code, rr.Body.String())
	}
	var view pricingStrategyPreviewView
	if err := json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
		t.Fatalf("preview JSON 非法: %v（body=%q）", err, rr.Body.String())
	}
	return view
}

func decodePricingDetail(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var detail map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &detail); err != nil {
		t.Fatalf("detail JSON 非法: %v（body=%q）", err, rr.Body.String())
	}
	return detail
}
