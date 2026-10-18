package pricing

import (
	"encoding/json"
	"errors"
	"testing"

	"newtg/gateway/internal/contract"
)

// fakeConfig 是 ConfigReader 的内存实现。
type fakeConfig map[string]json.RawMessage

func (f fakeConfig) Get(key string) (json.RawMessage, bool) {
	v, ok := f[key]
	return v, ok
}

func TestReadQuotaPerUnit(t *testing.T) {
	cases := []struct {
		name    string
		cfg     ConfigReader
		want    int64
		wantErr bool
	}{
		{"nil 配置用默认", nil, DefaultQuotaPerUnit, false},
		{"缺键用默认", fakeConfig{}, DefaultQuotaPerUnit, false},
		{"合法值", fakeConfig{quotaPerUnitKey: json.RawMessage(`250`)}, 250, false},
		{"零越界", fakeConfig{quotaPerUnitKey: json.RawMessage(`0`)}, 0, true},
		{"负值越界", fakeConfig{quotaPerUnitKey: json.RawMessage(`-1`)}, 0, true},
		{"超上界", fakeConfig{quotaPerUnitKey: json.RawMessage(`1000001`)}, 0, true},
		{"非整数", fakeConfig{quotaPerUnitKey: json.RawMessage(`100.5`)}, 0, true},
		{"字符串", fakeConfig{quotaPerUnitKey: json.RawMessage(`"100"`)}, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := readQuotaPerUnit(c.cfg)
			if (err != nil) != c.wantErr {
				t.Fatalf("err=%v，wantErr=%v", err, c.wantErr)
			}
			if !c.wantErr && got != c.want {
				t.Fatalf("got %d，期望 %d", got, c.want)
			}
		})
	}
}

func TestNewSourceRequiresDB(t *testing.T) {
	if _, err := NewSource(nil, nil); err == nil {
		t.Fatal("nil DB 应报错")
	}
}

func TestPriceFromModelRowRatio(t *testing.T) {
	i64 := func(v int64) *int64 { return &v }
	row := modelPricingRow{
		Model:           "m",
		Mode:            0,
		PromptPrice:     i64(500),
		CompletionPrice: i64(1500),
		CacheReadPrice:  i64(100),
		CacheWritePrice: i64(1250),
		Status:          1,
	}
	price, mode, expr, err := priceFromModelRow(row, "m")
	if err != nil {
		t.Fatalf("映射: %v", err)
	}
	if mode != contract.BillingModeRatio || expr != "" {
		t.Fatalf("mode=%v expr=%q", mode, expr)
	}
	if price.ModelRatio != 500 || price.CompletionRatio != 1500 ||
		price.CacheRatio != 100 || !price.CacheRatioSet ||
		price.CacheCreationRatio != 1250 || !price.CacheCreationRatioSet {
		t.Fatalf("价列映射错误: %+v", price)
	}
}

func TestPriceFromModelRowNullSemantics(t *testing.T) {
	// completion NULL → 0（未设置，由 Resolve 兜底）；cache 两列 NULL → Set=false。
	row := modelPricingRow{Model: "m", Mode: 0, PromptPrice: ptr(int64(1000))}
	price, _, _, err := priceFromModelRow(row, "m")
	if err != nil {
		t.Fatal(err)
	}
	if price.CompletionRatio != 0 {
		t.Fatalf("NULL completion 应为 0（未设置），got %v", price.CompletionRatio)
	}
	if price.CacheRatioSet || price.CacheCreationRatioSet {
		t.Fatalf("NULL cache 不应带 Set 标记: %+v", price)
	}

	// 显式 0 必须保留（-1 哨兵；Resolve 还原为 0）。
	row = modelPricingRow{
		Model: "m", Mode: 0, PromptPrice: ptr(int64(0)),
		CompletionPrice: ptr(int64(0)), CacheReadPrice: ptr(int64(0)),
		CacheWritePrice: ptr(int64(0)),
	}
	price, _, _, err = priceFromModelRow(row, "m")
	if err != nil {
		t.Fatal(err)
	}
	if price.CompletionRatio != explicitZeroRatio || !price.CacheRatioSet || !price.CacheCreationRatioSet {
		t.Fatalf("显式零被改写: %+v", price)
	}
	if !price.FreeModel {
		t.Fatal("四项全 0 应标 FreeModel")
	}
}

func TestPriceFromModelRowPerCallAndExpr(t *testing.T) {
	// per_request：prompt_price = 每次请求价。
	row := modelPricingRow{Model: "m", Mode: 1, PromptPrice: ptr(int64(150))}
	price, mode, expr, err := priceFromModelRow(row, "m")
	if err != nil || mode != contract.BillingModePerCall || expr != "" {
		t.Fatalf("按次映射: mode=%v expr=%q err=%v", mode, expr, err)
	}
	if !price.UsePrice || price.ModelPrice != 150 {
		t.Fatalf("按次价错误: %+v", price)
	}

	// per_request 且 prompt_price NULL → 兜底价。
	row = modelPricingRow{Model: "m", Mode: 1}
	price, _, _, err = priceFromModelRow(row, "m")
	if err != nil {
		t.Fatal(err)
	}
	if price.ModelPrice != DefaultPerCallPrice() {
		t.Fatalf("NULL 按次价应兜底 %v，got %v", DefaultPerCallPrice(), price.ModelPrice)
	}

	// billing_expr 非 NULL → 表达式模式（即使 mode=0）。
	e := " p*2 "
	row = modelPricingRow{Model: "m", Mode: 0, BillingExpr: &e}
	_, mode, expr, err = priceFromModelRow(row, "m")
	if err != nil || mode != contract.BillingModeTieredExpr || expr != "p*2" {
		t.Fatalf("表达式行映射: mode=%v expr=%q err=%v", mode, expr, err)
	}

	// 非法 mode。
	row = modelPricingRow{Model: "m", Mode: 7}
	if _, _, _, err := priceFromModelRow(row, "m"); err == nil {
		t.Fatal("非法 mode 应报错")
	}
}

func TestPriceFromModelRowRejectsInexactPrice(t *testing.T) {
	row := modelPricingRow{Model: "m", Mode: 0, PromptPrice: ptr(maxExactFloatInt + 1)}
	if _, _, _, err := priceFromModelRow(row, "m"); !errors.Is(err, ErrInvalidPrice) {
		t.Fatalf("超出 float64 精确整数域应拒绝，got %v", err)
	}
}

func TestApplyGroupOverride(t *testing.T) {
	price := contract.PriceData{ModelRatio: 500, CompletionRatio: 1500}
	row := groupPricingRow{PromptPrice: 100, CompletionPrice: 200, CacheReadPrice: 50, CacheWritePrice: 60}
	if err := applyGroupOverride(&price, contract.BillingModeRatio, row); err != nil {
		t.Fatal(err)
	}
	if price.ModelRatio != 100 || price.CompletionRatio != 200 ||
		price.CacheRatio != 50 || price.CacheCreationRatio != 60 ||
		!price.CacheRatioSet || !price.CacheCreationRatioSet {
		t.Fatalf("覆盖价映射错误: %+v", price)
	}

	perCall := contract.PriceData{UsePrice: true, ModelPrice: 150}
	if err := applyGroupOverride(&perCall, contract.BillingModePerCall, row); err != nil {
		t.Fatal(err)
	}
	if perCall.ModelPrice != 100 || !perCall.UsePrice {
		t.Fatalf("按次覆盖价错误: %+v", perCall)
	}

	if err := applyGroupOverride(&price, contract.BillingModeTieredExpr, row); err == nil {
		t.Fatal("表达式模式不应接受价列覆盖")
	}
}

func ptr[T any](v T) *T { return &v }
