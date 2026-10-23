package httpapi

// 公开目录的时段表（计划 Task 6）：
//   - time.enabled=true 时列表与详情返回 pricing_schedule；
//   - 无策略 / time.enabled=false / 策略损坏时为 JSON null（旧响应形状）；
//   - 不存在的窗口不参与（windows 恒为数组）。

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"newtg/gateway/internal/store"
)

func scheduleFixtureCatalog(strategy []byte) store.PublicPricingCatalog {
	return store.PublicPricingCatalog{
		Models: []store.PublicModelPricing{
			{
				Model: "deepseek/flash", Mode: 0,
				PromptPrice: publicInt64Ptr(200), CompletionPrice: publicInt64Ptr(800),
				Strategy: strategy,
			},
			{Model: "plain/model", Mode: 0, PromptPrice: publicInt64Ptr(100)},
		},
		Version: "abcdefabcdefabcd",
	}
}

func TestPublicPricingSchedulePresentWhenEnabled(t *testing.T) {
	repo := &fakePublicPricingRepo{catalog: scheduleFixtureCatalog(publicJSON(timeStrategyFixture()))}
	env := newPublicTestEnv(t, nil, repo, nil)

	_, body, rr := env.decodeList(t, "/api/pricing?page_size=100", nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d（body=%s）", rr.Code, rr.Body.String())
	}
	schedules := make(map[string]*PricingScheduleView, len(body.Items))
	for _, card := range body.Items {
		schedules[card.ModelID] = card.PricingSchedule
	}
	schedule := schedules["deepseek/flash"]
	if schedule == nil {
		t.Fatalf("启用时段的模型缺少 pricing_schedule: %+v", body.Items)
	}
	if schedule.Kind != "peak_offpeak" || schedule.Timezone != "Asia/Shanghai" ||
		schedule.DefaultMultiplier != 0.5 || schedule.HolidayPolicy != "none" ||
		len(schedule.Windows) != 2 {
		t.Fatalf("pricing_schedule 投影错误: %+v", schedule)
	}
	if schedule.Windows[0].Name != "上午高峰" || schedule.Windows[0].Start != "09:00" ||
		len(schedule.Windows[0].Days) != 5 {
		t.Fatalf("窗口投影错误: %+v", schedule.Windows[0])
	}
	if schedules["plain/model"] != nil {
		t.Fatal("无策略模型不得返回 pricing_schedule")
	}

	// 详情同样返回时段表（前端详情页展示完整时段）。
	detail := env.request(http.MethodGet, "/api/pricing/"+url.PathEscape("deepseek/flash"), nil, nil)
	if detail.Code != http.StatusOK {
		t.Fatalf("detail status = %d（body=%s）", detail.Code, detail.Body.String())
	}
	var decoded PublicModelDetail
	if err := json.Unmarshal(detail.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("detail JSON 非法: %v", err)
	}
	if decoded.PricingSchedule == nil || decoded.PricingSchedule.Kind != "peak_offpeak" {
		t.Fatalf("详情缺少 pricing_schedule: %+v", decoded.PricingSchedule)
	}
}

func TestPublicPricingScheduleNullWhenDisabled(t *testing.T) {
	disabled, err := json.Marshal(map[string]any{
		"version": 1,
		"time": map[string]any{
			"enabled": false, "timezone": "Asia/Shanghai",
			"holiday_policy": "none", "default_multiplier": 0,
			"windows": []any{},
		},
	})
	if err != nil {
		t.Fatalf("marshal disabled strategy: %v", err)
	}
	repo := &fakePublicPricingRepo{catalog: scheduleFixtureCatalog(disabled)}
	env := newPublicTestEnv(t, nil, repo, nil)

	_, body, rr := env.decodeList(t, "/api/pricing?page_size=100", nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d（body=%s）", rr.Code, rr.Body.String())
	}
	for _, card := range body.Items {
		if card.PricingSchedule != nil {
			t.Fatalf("time.enabled=false 时必须为 null: %+v", card.PricingSchedule)
		}
	}
	// 键必须存在且是 null（不是省略）——旧客户端按固定字段解析。
	var raw struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		t.Fatalf("raw JSON 非法: %v", err)
	}
	for _, item := range raw.Items {
		value, ok := item["pricing_schedule"]
		if !ok || string(value) != "null" {
			t.Fatalf("pricing_schedule 必须是显式 null: %s", value)
		}
	}
}

func TestPublicPricingScheduleInvalidStrategyDegradesToNull(t *testing.T) {
	repo := &fakePublicPricingRepo{catalog: scheduleFixtureCatalog([]byte(`{"version":2}`))}
	env := newPublicTestEnv(t, nil, repo, nil)

	_, body, rr := env.decodeList(t, "/api/pricing?page_size=100", nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("脏策略不得让目录 500：status = %d（body=%s）", rr.Code, rr.Body.String())
	}
	for _, card := range body.Items {
		if card.ModelID == "deepseek/flash" && card.PricingSchedule != nil {
			t.Fatalf("非法策略必须降级为 null: %+v", card.PricingSchedule)
		}
	}
}
