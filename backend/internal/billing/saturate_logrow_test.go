package billing

import (
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
