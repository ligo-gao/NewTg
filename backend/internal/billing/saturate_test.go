package billing

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"newtg/gateway/internal/contract"
)

// testUsageFact 返回一份完整用量事实（流式、带 TTFT）。
func testUsageFact() *contract.UsageFact {
	ttft := 123
	return &contract.UsageFact{
		Model: "gpt-4o",
		Usage: contract.Usage{
			Prompt:     1000,
			Completion: 200,
			CacheRead:  300,
			CacheWrite: 40,
		},
		LatencyMS:    4567,
		FirstTokenMS: &ttft,
		Stream:       true,
	}
}

// testRelayContext 返回一份带渠道/分组/钳制/折扣的请求上下文。
func testRelayContext() *contract.RelayContext {
	return &contract.RelayContext{
		RequestID:     "0b6f5f3e-5f4e-4d3c-8b2a-1f0e9d8c7b6a",
		UserID:        7,
		TokenID:       11,
		UsingGroup:    "vip",
		UserGroup:     "default",
		OriginModel:   "gpt-4o",
		UpstreamModel: "gpt-4o-2024-08-06",
		RequestPath:   "/v1/chat/completions",
		ChannelID:     42,
		Discount:      0.8,
		QuotaClamp: &contract.QuotaClamp{
			Op: "QuotaFromFloatChecked", Kind: contract.ClampOverflow,
			Original: 1e12, Clamped: contract.MaxQuota,
		},
		PriceSnapshot: &contract.PriceSnapshot{
			Mode:                  contract.BillingModeTieredExpr,
			ExprString:            `p * 0.001`,
			IsClaudeUsageSemantic: true,
		},
	}
}

// TestSaturateRowProjection 验证 25 列投影的字段来源与 NULL 语义。
func TestSaturateRowProjection(t *testing.T) {
	row, err := buildUsageLogRow(testUsageFact(), testRelayContext(), SettlementOutcome{
		Cost: 321, PreDeducted: 500, Refunded: 179,
		Status: UsageLogStatusSuccess,
	})
	if err != nil {
		t.Fatalf("buildUsageLogRow: %v", err)
	}

	if row.requestID != "0b6f5f3e-5f4e-4d3c-8b2a-1f0e9d8c7b6a" {
		t.Fatalf("requestID = %q", row.requestID)
	}
	if row.userID != 7 {
		t.Fatalf("userID = %d, want 7", row.userID)
	}
	if !row.tokenID.Valid || row.tokenID.Int64 != 11 {
		t.Fatalf("tokenID = %+v, want 11", row.tokenID)
	}
	if !row.channelID.Valid || row.channelID.Int64 != 42 {
		t.Fatalf("channelID = %+v, want 42", row.channelID)
	}
	if row.model != "gpt-4o" {
		t.Fatalf("model = %q, want gpt-4o", row.model)
	}
	if !row.group.Valid || row.group.String != "vip" {
		t.Fatalf("group = %+v, want vip", row.group)
	}
	if row.promptTokens != 1000 || row.completionTokens != 200 ||
		row.cacheReadTokens != 300 || row.cacheWriteTokens != 40 {
		t.Fatalf("tokens = %d/%d/%d/%d", row.promptTokens, row.completionTokens,
			row.cacheReadTokens, row.cacheWriteTokens)
	}
	if row.preDeducted != 500 || row.cost != 321 || row.refunded != 179 {
		t.Fatalf("amounts = pre %d cost %d refunded %d", row.preDeducted, row.cost, row.refunded)
	}
	if !row.latencyMS.Valid || row.latencyMS.Int64 != 4567 {
		t.Fatalf("latencyMS = %+v", row.latencyMS)
	}
	if !row.firstTokenMS.Valid || row.firstTokenMS.Int64 != 123 {
		t.Fatalf("firstTokenMS = %+v, want 123", row.firstTokenMS)
	}
	if !row.stream {
		t.Fatal("stream = false, want true")
	}
	if row.status != UsageLogStatusSuccess {
		t.Fatalf("status = %d", row.status)
	}
}

// TestSaturateRowNullsAndForcing 验证可空列/强制归零语义。
func TestSaturateRowNullsAndForcing(t *testing.T) {
	ttft := 10
	fact := &contract.UsageFact{Model: "m", FirstTokenMS: &ttft, Stream: false}
	rc := &contract.RelayContext{RequestID: "11111111-1111-4111-8111-111111111111"}

	row, err := buildUsageLogRow(fact, rc, SettlementOutcome{
		Cost:   99, // error 状态必须强制为 0
		Status: UsageLogStatusError, ErrorClass: "upstream_server",
		ErrorMessage: "boom",
	})
	if err != nil {
		t.Fatalf("buildUsageLogRow: %v", err)
	}
	if row.cost != 0 {
		t.Fatalf("error status cost = %d, want 0（非 success 的 cost 一律归零）", row.cost)
	}
	if row.firstTokenMS.Valid {
		t.Fatalf("非流式 firstTokenMS = %+v, want NULL", row.firstTokenMS)
	}
	if row.tokenID.Valid || row.channelID.Valid {
		t.Fatalf("token/channel 应为 NULL: %+v %+v", row.tokenID, row.channelID)
	}
	if row.group.Valid {
		t.Fatalf("空分组应为 NULL: %+v", row.group)
	}
	if !row.errorClass.Valid || row.errorClass.String != "upstream_server" {
		t.Fatalf("errorClass = %+v", row.errorClass)
	}
	if !row.errorMessage.Valid || row.errorMessage.String != "boom" {
		t.Fatalf("errorMessage = %+v", row.errorMessage)
	}

	// refunded 状态同样强制 cost=0（worker 兜底退款）。
	row, err = buildUsageLogRow(nil, rc, SettlementOutcome{
		Cost: 55, PreDeducted: 55, Refunded: 55, Status: UsageLogStatusRefunded,
	})
	if err != nil {
		t.Fatalf("buildUsageLogRow(refunded): %v", err)
	}
	if row.cost != 0 {
		t.Fatalf("refunded status cost = %d, want 0", row.cost)
	}
}

// TestSaturateRejectsInvalidInput 验证缺 requestID/非法 status/负金额被拒绝。
func TestSaturateRejectsInvalidInput(t *testing.T) {
	l := NewDBLogger(nil)
	ctx := context.Background()

	if err := l.Saturate(ctx, testUsageFact(), nil, SettlementOutcome{}); err == nil {
		t.Fatal("Saturate(nil rc) = nil, want error")
	}
	if err := l.Saturate(ctx, testUsageFact(), &contract.RelayContext{}, SettlementOutcome{}); err == nil {
		t.Fatal("Saturate(empty request id) = nil, want error")
	}

	rc := testRelayContext()
	tests := []struct {
		name    string
		outcome SettlementOutcome
	}{
		{"status 4", SettlementOutcome{Status: 4}},
		{"status -1", SettlementOutcome{Status: -1}},
		{"negative cost", SettlementOutcome{Status: UsageLogStatusSuccess, Cost: -1}},
		{"negative pre_deducted", SettlementOutcome{Status: UsageLogStatusSuccess, PreDeducted: -1}},
		{"negative refunded", SettlementOutcome{Status: UsageLogStatusSuccess, Refunded: -1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := buildUsageLogRow(testUsageFact(), rc, tc.outcome); err == nil {
				t.Fatalf("buildUsageLogRow(%+v) = nil, want error", tc.outcome)
			}
		})
	}
}

// TestSaturateNilLogsDB 验证未装配日志库时 fail-closed。
func TestSaturateNilLogsDB(t *testing.T) {
	l := NewDBLogger(nil)
	err := l.Saturate(context.Background(), testUsageFact(), testRelayContext(),
		SettlementOutcome{Status: UsageLogStatusSuccess})
	if err == nil {
		t.Fatal("Saturate(nil logsDB) = nil, want error")
	}
}

// TestSaturateOtherPartitions 验证 other 的分区与键集约束：
// quota_saturation 只出现在 admin_info；public 顶层无保留/特权键；
// 全 JSON 不含 estimated_* 前缀（billing-00 §11 / billing-07 §6.9/§7）。
func TestSaturateOtherPartitions(t *testing.T) {
	row, err := buildUsageLogRow(testUsageFact(), testRelayContext(), SettlementOutcome{
		Cost: 10, Status: UsageLogStatusSuccess,
	})
	if err != nil {
		t.Fatalf("buildUsageLogRow: %v", err)
	}

	var other map[string]any
	if err := json.Unmarshal([]byte(row.other), &other); err != nil {
		t.Fatalf("other is not JSON: %v (%s)", err, row.other)
	}

	// quota_saturation 必须在 admin_info 内，且是 4 键审计 map。
	if _, ok := other["quota_saturation"]; ok {
		t.Fatal("quota_saturation 不得出现在 public 顶层")
	}
	admin, ok := other["admin_info"].(map[string]any)
	if !ok {
		t.Fatalf("admin_info = %T, want object（other=%s）", other["admin_info"], row.other)
	}
	qs, ok := admin["quota_saturation"].(map[string]any)
	if !ok {
		t.Fatalf("admin_info.quota_saturation = %T, want object", admin["quota_saturation"])
	}
	wantKeys := []string{"op", "kind", "original", "clamped"}
	for _, k := range wantKeys {
		if _, ok := qs[k]; !ok {
			t.Fatalf("quota_saturation 缺键 %q: %v", k, qs)
		}
	}
	if len(qs) != len(wantKeys) {
		t.Fatalf("quota_saturation keys = %d, want %d", len(qs), len(wantKeys))
	}

	// public 保留键一律不得出现（admin_info 是合法分区容器，单独保留；
	// root_info/audit_info 本任务不写）。
	legacyKeys := []string{"channel_id", "channel_name", "channel_type", "reject_reason"}
	for _, k := range legacyKeys {
		if _, ok := other[k]; ok {
			t.Fatalf("public 顶层出现遗留敏感键 %q", k)
		}
	}
	if _, ok := other["root_info"]; ok {
		t.Fatal("M3 不写 root_info 分区")
	}
	if _, ok := other["audit_info"]; ok {
		t.Fatal("M3 不写 audit_info 分区")
	}

	// 折扣/路径/映射/资金来源等 public 键。
	if got := other["applied_discount"]; got != 0.8 {
		t.Fatalf("applied_discount = %v, want 0.8", got)
	}
	if got := other["request_path"]; got != "/v1/chat/completions" {
		t.Fatalf("request_path = %v", got)
	}
	if got := other["upstream_model_name"]; got != "gpt-4o-2024-08-06" {
		t.Fatalf("upstream_model_name = %v", got)
	}
	if got := other["is_model_mapped"]; got != true {
		t.Fatalf("is_model_mapped = %v", got)
	}
	if got := other["billing_source"]; got != "wallet" {
		t.Fatalf("billing_source = %v", got)
	}
	if got := other["frt"]; got != float64(123) {
		t.Fatalf("frt = %v, want 123", got)
	}
	if got := other["billing_mode"]; got != "tiered_expr" {
		t.Fatalf("billing_mode = %v", got)
	}
	if got := other["usage_semantic"]; got != "anthropic" {
		t.Fatalf("usage_semantic = %v", got)
	}

	assertNoEstimatedKeys(t, other, "other")
}

// TestSaturateOtherNoDiscountForZeroRate 钉死已知契约缺口的行为：
// RelayContext 只有 Discount float64，rate=0 与未命中无法区分，故只记录
// 0<rate<1；任何情况下都不得凭空写出 discount_target。
func TestSaturateOtherNoDiscountForZeroRate(t *testing.T) {
	rc := testRelayContext()
	rc.Discount = 0
	other := buildUsageLogOther(rc, "gpt-4o", sql.NullInt64{})
	if _, ok := other["applied_discount"]; ok {
		t.Fatal("rate=0 不得凭零值写入 applied_discount")
	}
	if _, ok := other["discount_target"]; ok {
		t.Fatal("契约无 DiscountTarget 字段，不得凭空写入 discount_target")
	}

	rc.Discount = 0.75
	other = buildUsageLogOther(rc, "gpt-4o", sql.NullInt64{})
	if got := other["applied_discount"]; got != 0.75 {
		t.Fatalf("applied_discount = %v, want 0.75", got)
	}
}

// TestSaturateTextSanitization 验证 NUL/非法 UTF-8/超长文本不会让日志插入失败。
func TestSaturateTextSanitization(t *testing.T) {
	if got := truncateLogText("a\x00b", 0); got != "ab" {
		t.Fatalf("NUL 未移除: %q", got)
	}
	long := strings.Repeat("错", 600)
	if got := truncateLogText(long, 500); len([]rune(got)) != 500 {
		t.Fatalf("truncate runes = %d, want 500", len([]rune(got)))
	}
	if got := truncateLogText("ok", 0); got != "ok" {
		t.Fatalf("max=0 不应截断: %q", got)
	}
}

// assertNoEstimatedKeys 递归断言 JSON 中不存在 estimated_ 前缀的键。
func assertNoEstimatedKeys(t *testing.T, v any, path string) {
	t.Helper()
	switch node := v.(type) {
	case map[string]any:
		for k, child := range node {
			if strings.HasPrefix(k, "estimated_") {
				t.Fatalf("%s.%s: 禁止写入 estimated_* 键", path, k)
			}
			assertNoEstimatedKeys(t, child, path+"."+k)
		}
	case []any:
		for _, child := range node {
			assertNoEstimatedKeys(t, child, path+"[]")
		}
	}
}
