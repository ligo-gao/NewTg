package billing

import (
	"strings"
	"testing"

	"newtg/gateway/internal/contract"
)

// TestBuildUsageLogRowProjectsReconcileDimensions 验证上游对账维度按写时
// 事实固化进行投影（usage_logs 迁移 20261028090500）。
func TestBuildUsageLogRowProjectsReconcileDimensions(t *testing.T) {
	rc := &contract.RelayContext{RequestID: "req-1", UpstreamModel: "deepseek-v4-pro", UpstreamAccountID: 7}
	fact := &contract.UsageFact{
		Model: "deepseek-v4-pro", Attempts: 2, ResponseID: "resp_abc",
		Usage: contract.Usage{
			Prompt: 100, Completion: 40, CacheRead: 60, Reasoning: 12,
			PromptIncludesCache: true, Source: contract.UsageUpstreamExact,
		},
	}
	row, err := buildUsageLogRow(fact, rc, SettlementOutcome{Status: UsageLogStatusSuccess})
	if err != nil {
		t.Fatalf("buildUsageLogRow: %v", err)
	}
	if !row.usageSource.Valid || row.usageSource.Int16 != 0 {
		t.Fatalf("usage_source = %+v, want 0 (upstream_exact)", row.usageSource)
	}
	if !row.usageSemantic.Valid || row.usageSemantic.Int16 != 0 {
		t.Fatalf("usage_semantic = %+v, want 0 (prompt includes cache)", row.usageSemantic)
	}
	if row.reasoningTokens != 12 {
		t.Fatalf("reasoning_tokens = %d, want 12", row.reasoningTokens)
	}
	if row.upstreamModel.String != "deepseek-v4-pro" || !row.upstreamModel.Valid {
		t.Fatalf("upstream_model = %+v", row.upstreamModel)
	}
	if !row.upstreamAccountID.Valid || row.upstreamAccountID.Int64 != 7 {
		t.Fatalf("upstream_account_id = %+v", row.upstreamAccountID)
	}
	if row.responseID.String != "resp_abc" {
		t.Fatalf("response_id = %+v", row.responseID)
	}
	if row.attempts != 2 {
		t.Fatalf("attempts = %d, want 2", row.attempts)
	}
}

func TestBuildUsageLogRowLeavesUnknownAttributionNull(t *testing.T) {
	rc := &contract.RelayContext{RequestID: "req-2"}
	fact := &contract.UsageFact{Model: "m", Usage: contract.Usage{Source: contract.UsageEstimated}}
	row, err := buildUsageLogRow(fact, rc, SettlementOutcome{Status: UsageLogStatusSuccess})
	if err != nil {
		t.Fatalf("buildUsageLogRow: %v", err)
	}
	if row.upstreamModel.Valid || row.upstreamAccountID.Valid || row.responseID.Valid {
		t.Fatalf("unattributed fields must be NULL, got %+v %+v %+v",
			row.upstreamModel, row.upstreamAccountID, row.responseID)
	}
	if row.attempts != 1 {
		t.Fatalf("attempts = %d, want default 1", row.attempts)
	}
}

// TestBuildUsageLogRowPrefersFactAttributionOverRelayContext 验证归属以事实为准：
// 同一请求同时带上事实与 rc 的两套上游归属时，日志必须落事实值。这是「写时
// 固化」的核心护栏——重试可能改写 rc，但已产出的事实不得被回写（设计 §4.6）。
func TestBuildUsageLogRowPrefersFactAttributionOverRelayContext(t *testing.T) {
	rc := &contract.RelayContext{
		RequestID: "req-3", UpstreamModel: "rc-model", UpstreamAccountID: 9,
	}
	fact := &contract.UsageFact{
		Model: "m", ResponseID: "resp_fact",
		UpstreamModel: "fact-model", UpstreamAccountID: 42,
		Usage: contract.Usage{Source: contract.UsageUpstreamExact},
	}
	row, err := buildUsageLogRow(fact, rc, SettlementOutcome{Status: UsageLogStatusSuccess})
	if err != nil {
		t.Fatalf("buildUsageLogRow: %v", err)
	}
	if !row.upstreamModel.Valid || row.upstreamModel.String != "fact-model" {
		t.Fatalf("upstream_model = %+v, want fact value %q", row.upstreamModel, "fact-model")
	}
	if !row.upstreamAccountID.Valid || row.upstreamAccountID.Int64 != 42 {
		t.Fatalf("upstream_account_id = %+v, want fact value 42", row.upstreamAccountID)
	}
	if !row.responseID.Valid || row.responseID.String != "resp_fact" {
		t.Fatalf("response_id = %+v, want fact value %q", row.responseID, "resp_fact")
	}
}

// TestBuildUsageLogRowClampsNegativeReasoningTokens 验证上游解析出的负数
// reasoning_tokens 不会写入 PG（负数按 0，与 token 列同口径），行仍然有效。
func TestBuildUsageLogRowClampsNegativeReasoningTokens(t *testing.T) {
	rc := &contract.RelayContext{RequestID: "req-4"}
	fact := &contract.UsageFact{
		Model: "m",
		Usage: contract.Usage{Reasoning: -5, Source: contract.UsageUpstreamExact},
	}
	row, err := buildUsageLogRow(fact, rc, SettlementOutcome{Status: UsageLogStatusSuccess})
	if err != nil {
		t.Fatalf("buildUsageLogRow: %v", err)
	}
	if row.reasoningTokens != 0 {
		t.Fatalf("reasoning_tokens = %d, want 0", row.reasoningTokens)
	}
	if !row.usageSource.Valid || row.status != UsageLogStatusSuccess {
		t.Fatalf("row should still be valid: usage_source=%+v status=%d",
			row.usageSource, row.status)
	}
}

// TestBuildUsageLogRowTruncatesResponseIDTo128 验证 response_id 与其余文本列
// 一样按 varchar(128) 的宽度截断（迁移 20261028090500），避免超长上游 id
// 让整条日志插入失败并阻塞结算。
func TestBuildUsageLogRowTruncatesResponseIDTo128(t *testing.T) {
	long := strings.Repeat("响", 200)
	rc := &contract.RelayContext{RequestID: "req-5"}
	fact := &contract.UsageFact{
		Model: "m", ResponseID: long,
		Usage: contract.Usage{Source: contract.UsageUpstreamExact},
	}
	row, err := buildUsageLogRow(fact, rc, SettlementOutcome{Status: UsageLogStatusSuccess})
	if err != nil {
		t.Fatalf("buildUsageLogRow: %v", err)
	}
	if !row.responseID.Valid {
		t.Fatalf("response_id = %+v, want valid", row.responseID)
	}
	if got := len([]rune(row.responseID.String)); got != 128 {
		t.Fatalf("response_id runes = %d, want 128", got)
	}
	if want := strings.Repeat("响", 128); row.responseID.String != want {
		t.Fatalf("response_id = %q, want first 128 runes", row.responseID.String)
	}
}
