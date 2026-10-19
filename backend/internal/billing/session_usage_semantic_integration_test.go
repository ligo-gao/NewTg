//go:build integration

package billing

// T9 usage_semantic 生产写者的集成验收：真实主库 + 日志库 + DBLogger。
//
// 覆盖：
//   - 入站协议事实（RawFormat=claude）在会话解析快照时写入
//     PriceSnapshot.IsClaudeUsageSemantic，Saturator 落
//     usage_logs.other.usage_semantic=anthropic；
//   - OpenAI 语义（PromptIncludesCache=true）不写该键。

import (
	"encoding/json"
	"testing"

	"newtg/gateway/internal/contract"
)

// TestIntegrationSessionUsageSemanticClaude 验证 Claude 入站协议会把
// usage_semantic=anthropic 写进 usage_logs.other（真实日志库）。
func TestIntegrationSessionUsageSemanticClaude(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	logsDB := openLogsTestDB(t) // 与 saturate_integration_test.go 共用（integration tag）
	userID := createBillingTestUser(t, db, 500)

	req := newTestUUID(t)
	cleanupUsageLog(t, logsDB, req)

	e := NewEngine(gormDB, logsDB, nil, billingTestConfig(), nil, nil, NewDBLogger(logsDB))
	rc := &contract.RelayContext{
		RequestID:   req,
		UserID:      int(userID),
		OriginModel: "claude-test-model",
		UsingGroup:  "default",
		RawFormat:   contract.RelayFormat("claude"),
	}
	s := e.NewSession(rc).(*Session)
	// 传统倍率：1M prompt tokens × 100 = 100 积分；比例价不依赖 usage 语义，
	// 本用例只关心 fact → 快照 → 日志的 usage_semantic 传播。
	rc.PriceSnapshot = ratioSnapshot(100, 0)

	est := contract.QuotaEstimate{Model: "claude-test-model", PromptTokens: 1_000_000}
	if err := s.PreConsume(est); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	fact := contract.UsageFact{
		Model: "claude-test-model",
		Usage: contract.Usage{
			Prompt:              1_000_000,
			PromptIncludesCache: false,
			Source:              contract.UsageUpstreamExact,
		},
	}
	if err := s.Terminate(fact); err != nil {
		t.Fatalf("Terminate: %v", err)
	}

	var raw []byte
	if err := logsDB.QueryRow(
		`SELECT other::text FROM usage_logs WHERE request_id = $1::uuid`, req).Scan(&raw); err != nil {
		t.Fatalf("read usage log other: %v", err)
	}
	var other map[string]any
	if err := json.Unmarshal(raw, &other); err != nil {
		t.Fatalf("decode other: %v", err)
	}
	if got := other["usage_semantic"]; got != "anthropic" {
		t.Fatalf("usage_semantic = %v, want anthropic（Claude 入站语义）", got)
	}
}

// TestIntegrationSessionUsageSemanticOpenAI 验证 OpenAI 语义不写 usage_semantic。
func TestIntegrationSessionUsageSemanticOpenAI(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	logsDB := openLogsTestDB(t)
	userID := createBillingTestUser(t, db, 500)

	req := newTestUUID(t)
	cleanupUsageLog(t, logsDB, req)

	e := NewEngine(gormDB, logsDB, nil, billingTestConfig(), nil, nil, NewDBLogger(logsDB))
	rc := &contract.RelayContext{
		RequestID:   req,
		UserID:      int(userID),
		OriginModel: "openai-test-model",
		UsingGroup:  "default",
		RawFormat:   contract.RelayFormat("openai"),
	}
	s := e.NewSession(rc).(*Session)
	rc.PriceSnapshot = ratioSnapshot(100, 0)

	est := contract.QuotaEstimate{Model: "openai-test-model", PromptTokens: 1_000_000}
	if err := s.PreConsume(est); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	fact := contract.UsageFact{
		Model: "openai-test-model",
		Usage: contract.Usage{
			Prompt:              1_000_000,
			PromptIncludesCache: true,
			Source:              contract.UsageUpstreamExact,
		},
	}
	if err := s.Terminate(fact); err != nil {
		t.Fatalf("Terminate: %v", err)
	}

	var raw []byte
	if err := logsDB.QueryRow(
		`SELECT other::text FROM usage_logs WHERE request_id = $1::uuid`, req).Scan(&raw); err != nil {
		t.Fatalf("read usage log other: %v", err)
	}
	var other map[string]any
	if err := json.Unmarshal(raw, &other); err != nil {
		t.Fatalf("decode other: %v", err)
	}
	if got, ok := other["usage_semantic"]; ok {
		t.Fatalf("usage_semantic = %v, want absent（OpenAI 语义）", got)
	}
}

// TestIntegrationSessionUsageSemanticFromFact 验证事实路径必须带**正向
// Claude 指示**：RawFormat 未标注时，只有 Usage.Raw 携带 Claude 专有缓存键
// 才写入 anthropic（PromptIncludesCache=false 单独不足以判定，见下一条回归）。
func TestIntegrationSessionUsageSemanticFromFact(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	logsDB := openLogsTestDB(t)
	userID := createBillingTestUser(t, db, 500)

	req := newTestUUID(t)
	cleanupUsageLog(t, logsDB, req)

	e := NewEngine(gormDB, logsDB, nil, billingTestConfig(), nil, nil, NewDBLogger(logsDB))
	rc := &contract.RelayContext{
		RequestID:   req,
		UserID:      int(userID),
		OriginModel: "fact-model",
		UsingGroup:  "default",
		RawFormat:   contract.RelayFormat("openai"), // 协议路径不标记
	}
	s := e.NewSession(rc).(*Session)
	rc.PriceSnapshot = ratioSnapshot(100, 0)

	est := contract.QuotaEstimate{Model: "fact-model", PromptTokens: 1_000_000}
	if err := s.PreConsume(est); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	fact := contract.UsageFact{
		Model: "fact-model",
		Usage: contract.Usage{
			Prompt:              1_000_000,
			PromptIncludesCache: false, // 缓存读为独立维度
			Source:              contract.UsageUpstreamExact,
			// Claude 专有键是事实路径的正向指示。
			Raw: map[string]any{"cache_read_input_tokens": float64(200)},
		},
	}
	if err := s.Terminate(fact); err != nil {
		t.Fatalf("Terminate: %v", err)
	}

	var raw []byte
	if err := logsDB.QueryRow(
		`SELECT other::text FROM usage_logs WHERE request_id = $1::uuid`, req).Scan(&raw); err != nil {
		t.Fatalf("read usage log other: %v", err)
	}
	var other map[string]any
	if err := json.Unmarshal(raw, &other); err != nil {
		t.Fatalf("decode other: %v", err)
	}
	if got := other["usage_semantic"]; got != "anthropic" {
		t.Fatalf("usage_semantic = %v, want anthropic（Claude 专有 Raw 键）", got)
	}
}

// TestIntegrationSessionUsageSemanticHeaderOpenAINotClaude 是 F1 回归：
// `x-model-response-usage` 头解析在缺 `prompt_includes_cache` 时默认 false，
// 但该请求是 OpenAI 语义（Raw 无 Claude 专有键）。此时绝不能写
// usage_semantic="anthropic"——false 布尔单独不是 Claude 证据。
func TestIntegrationSessionUsageSemanticHeaderOpenAINotClaude(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	logsDB := openLogsTestDB(t)
	userID := createBillingTestUser(t, db, 500)

	req := newTestUUID(t)
	cleanupUsageLog(t, logsDB, req)

	e := NewEngine(gormDB, logsDB, nil, billingTestConfig(), nil, nil, NewDBLogger(logsDB))
	rc := &contract.RelayContext{
		RequestID:   req,
		UserID:      int(userID),
		OriginModel: "header-openai-model",
		UsingGroup:  "default",
		RawFormat:   contract.RelayFormat("openai"),
	}
	s := e.NewSession(rc).(*Session)
	rc.PriceSnapshot = ratioSnapshot(100, 0)

	est := contract.QuotaEstimate{Model: "header-openai-model", PromptTokens: 1_000_000}
	if err := s.PreConsume(est); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	fact := contract.UsageFact{
		Model: "header-openai-model",
		Usage: contract.Usage{
			Prompt: 1_000_000,
			// 头部解析缺省值：false，但没有任何 Claude 专有键。
			PromptIncludesCache: false,
			Source:              contract.UsageUpstreamExact,
			Raw:                 map[string]any{"prompt_tokens": float64(1_000_000)},
		},
	}
	if err := s.Terminate(fact); err != nil {
		t.Fatalf("Terminate: %v", err)
	}

	var raw []byte
	if err := logsDB.QueryRow(
		`SELECT other::text FROM usage_logs WHERE request_id = $1::uuid`, req).Scan(&raw); err != nil {
		t.Fatalf("read usage log other: %v", err)
	}
	var other map[string]any
	if err := json.Unmarshal(raw, &other); err != nil {
		t.Fatalf("decode other: %v", err)
	}
	if got, ok := other["usage_semantic"]; ok {
		t.Fatalf("usage_semantic = %v, want absent（header 派生 OpenAI 不得标 anthropic）", got)
	}
}
