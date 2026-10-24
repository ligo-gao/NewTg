package billing

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"reflect"
	"strconv"
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
//
// 带一条成功尝试：排障证据（retry_chain 与 admin_info.triage）需要 Attempts
// 才有内容，投影用例统一以此为基准。
func testRelayContext() *contract.RelayContext {
	return &contract.RelayContext{
		RequestID:        "0b6f5f3e-5f4e-4d3c-8b2a-1f0e9d8c7b6a",
		UserID:           7,
		TokenID:          11,
		UsingGroup:       "vip",
		UserGroup:        "default",
		OriginModel:      "gpt-4o",
		UpstreamModel:    "gpt-4o-2024-08-06",
		RequestPath:      "/v1/chat/completions",
		RequestStructure: "valid",
		ChannelID:        42,
		ChannelType:      contract.ChannelTypeOpenAI,
		Discount:         0.8,
		Attempts: []contract.AttemptRecord{{
			Index:          0,
			ChannelID:      42,
			ChannelType:    contract.ChannelTypeOpenAI,
			UpstreamStatus: 200,
			LatencyMS:      120,
		}},
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
	}, false)
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

	// retry_chain 由 Attempts 投影而来：非 NULL，且每个元素的键集合固定
	// （排障读取方按这些键取值，缺键会让证据静默丢失）。
	if !row.retryChain.Valid {
		t.Fatal("有 Attempts 时 retry_chain 不应为 NULL")
	}
	var chain []map[string]any
	if err := json.Unmarshal([]byte(row.retryChain.String), &chain); err != nil {
		t.Fatalf("retry_chain 不是 JSON 数组: %v (%s)", err, row.retryChain.String)
	}
	if len(chain) != 1 {
		t.Fatalf("retry_chain 长度 = %d, want 1", len(chain))
	}
	wantChainKeys := []string{
		"attempt", "channel_id", "upstream",
		"error_class", "latency_ms", "upstream_status",
	}
	for _, key := range wantChainKeys {
		if _, ok := chain[0][key]; !ok {
			t.Fatalf("retry_chain[0] 缺键 %q: %v", key, chain[0])
		}
	}
	if len(chain[0]) != len(wantChainKeys) {
		t.Fatalf("retry_chain[0] 键数 = %d, want %d: %v",
			len(chain[0]), len(wantChainKeys), chain[0])
	}
	// decision 是路由层决策，AttemptRecord 尚未携带 → 不得凭序号推断写入。
	if _, ok := chain[0]["decision"]; ok {
		t.Fatalf("retry_chain[0] 不得合成 decision: %v", chain[0])
	}
}

// TestBuildRetryChain 验证 Attempts → retry_chain 的投影：
// 空切片写 NULL、单条/多条的顺序与标签、以及防御性截断。
func TestBuildRetryChain(t *testing.T) {
	t.Run("nil context", func(t *testing.T) {
		chain, err := buildRetryChain(nil)
		if err != nil {
			t.Fatalf("buildRetryChain(nil): %v", err)
		}
		if chain != "" {
			t.Fatalf("nil 上下文应返回空串（写 NULL），得到 %q", chain)
		}
	})

	t.Run("empty attempts", func(t *testing.T) {
		chain, err := buildRetryChain(&contract.RelayContext{})
		if err != nil {
			t.Fatalf("buildRetryChain(empty): %v", err)
		}
		if chain != "" {
			t.Fatalf("空 Attempts 应返回空串（写 NULL），得到 %q", chain)
		}
	})

	t.Run("single", func(t *testing.T) {
		rc := &contract.RelayContext{
			ChannelType: contract.ChannelTypeOpenAI,
			Attempts: []contract.AttemptRecord{{
				Index:          0,
				ChannelID:      42,
				ChannelType:    contract.ChannelTypeOpenAI,
				UpstreamStatus: 429,
				ErrorClass:     contract.ErrUpstreamRateLimit,
				LatencyMS:      87,
			}},
		}
		chain, err := buildRetryChain(rc)
		if err != nil {
			t.Fatalf("buildRetryChain(single): %v", err)
		}
		steps := decodeRetryChain(t, chain)
		if len(steps) != 1 {
			t.Fatalf("steps = %d, want 1", len(steps))
		}
		step := steps[0]
		if got := step["attempt"]; got != "1/1" {
			t.Fatalf("attempt = %v, want 1/1", got)
		}
		if got := step["channel_id"]; got != float64(42) {
			t.Fatalf("channel_id = %v, want 42", got)
		}
		if got := step["upstream"]; got != "openai" {
			t.Fatalf("upstream = %v, want openai", got)
		}
		if _, ok := step["decision"]; ok {
			t.Fatalf("不得合成路由决策 decision: %v", step)
		}
		if got := step["error_class"]; got != "upstream_rate_limit" {
			t.Fatalf("error_class = %v, want upstream_rate_limit", got)
		}
		if got := step["latency_ms"]; got != float64(87) {
			t.Fatalf("latency_ms = %v, want 87", got)
		}
		if got := step["upstream_status"]; got != float64(429) {
			t.Fatalf("upstream_status = %v, want 429", got)
		}
	})

	t.Run("multiple", func(t *testing.T) {
		rc := &contract.RelayContext{
			ChannelType: contract.ChannelTypeAnthropic,
			Attempts: []contract.AttemptRecord{
				{Index: 0, ChannelID: 1, UpstreamStatus: 500, ErrorClass: contract.ErrUpstreamServer, LatencyMS: 10},
				{Index: 1, ChannelID: 2, UpstreamStatus: 0, ErrorClass: contract.ErrRetryable, LatencyMS: 20},
			},
		}
		chain, err := buildRetryChain(rc)
		if err != nil {
			t.Fatalf("buildRetryChain(multiple): %v", err)
		}
		steps := decodeRetryChain(t, chain)
		if len(steps) != 2 {
			t.Fatalf("steps = %d, want 2", len(steps))
		}
		if got := steps[0]["attempt"]; got != "1/2" {
			t.Fatalf("steps[0].attempt = %v, want 1/2", got)
		}
		if got := steps[1]["attempt"]; got != "2/2" {
			t.Fatalf("steps[1].attempt = %v, want 2/2", got)
		}
		for i, step := range steps {
			if _, ok := step["decision"]; ok {
				t.Fatalf("steps[%d] 不得合成路由决策 decision: %v", i, step)
			}
		}
		// upstream 取请求级渠道类型（结算时最终解析出的渠道族）。
		for i, step := range steps {
			if got := step["upstream"]; got != "anthropic" {
				t.Fatalf("steps[%d].upstream = %v, want anthropic", i, got)
			}
		}
		if got := steps[1]["upstream_status"]; got != float64(0) {
			t.Fatalf("steps[1].upstream_status = %v, want 0（未拿到响应）", got)
		}
	})

	t.Run("truncated to eight", func(t *testing.T) {
		rc := &contract.RelayContext{ChannelType: contract.ChannelTypeOpenAI}
		for i := 0; i < 10; i++ {
			rc.Attempts = append(rc.Attempts, contract.AttemptRecord{Index: i, ChannelID: i + 1})
		}
		chain, err := buildRetryChain(rc)
		if err != nil {
			t.Fatalf("buildRetryChain(truncated): %v", err)
		}
		steps := decodeRetryChain(t, chain)
		if len(steps) != 8 {
			t.Fatalf("steps = %d, want 8（与 relay 侧上界一致）", len(steps))
		}
		if got := steps[7]["attempt"]; got != "8/8" {
			t.Fatalf("steps[7].attempt = %v, want 8/8", got)
		}
	})
}

// TestSaturateOtherTriage 验证 admin_info.triage 的证据投影：
// endpoint/channel_type/attempts/request_structure、upstream_status 的省略语义、
// 以及 conversion 的条数与字符串截断上界。
func TestSaturateOtherTriage(t *testing.T) {
	t.Run("projects evidence", func(t *testing.T) {
		rc := testRelayContext()
		rc.Attempts = []contract.AttemptRecord{
			{Index: 0, ChannelID: 42, UpstreamStatus: 500, ErrorClass: contract.ErrUpstreamServer, LatencyMS: 100},
			{Index: 1, ChannelID: 43, UpstreamStatus: 0, ErrorClass: contract.ErrRetryable, LatencyMS: 20},
		}
		long := strings.Repeat("转", 100)
		rc.ConversionChain = []contract.ConversionStepEntry{
			{Stage: "protocol_convert", From: contract.RelayFormat("anthropic"), To: contract.RelayFormat("openai")},
			{Stage: long, From: contract.RelayFormat(long), To: contract.RelayFormat("openai")},
			{Stage: "s3"}, {Stage: "s4"}, {Stage: "s5"},
		}

		triage := triageInfoOf(t, rc)

		if got := triage["endpoint"]; got != "chat_completions" {
			t.Fatalf("endpoint = %v, want chat_completions", got)
		}
		if got := triage["channel_type"]; got != "openai" {
			t.Fatalf("channel_type = %v, want openai", got)
		}
		// 最近一次拿到的上游响应（末轮 0 = 网络错误，不覆盖之前的 500）。
		if got := triage["upstream_status"]; got != 500 {
			t.Fatalf("upstream_status = %v, want 500", got)
		}
		if got := triage["attempts"]; got != 2 {
			t.Fatalf("attempts = %v, want 2", got)
		}
		if got := triage["request_structure"]; got != "valid" {
			t.Fatalf("request_structure = %v, want valid", got)
		}

		conversion, ok := triage["conversion"].([]map[string]any)
		if !ok {
			t.Fatalf("conversion = %T, want array: %v", triage["conversion"], triage)
		}
		if len(conversion) != 4 {
			t.Fatalf("conversion 条数 = %d, want 4（上限）", len(conversion))
		}
		first := conversion[0]
		wantKeys := []string{"stage", "from", "to"}
		for _, key := range wantKeys {
			if _, ok := first[key]; !ok {
				t.Fatalf("conversion[0] 缺键 %q: %v", key, first)
			}
		}
		if len(first) != len(wantKeys) {
			t.Fatalf("conversion[0] 键数 = %d, want %d: %v", len(first), len(wantKeys), first)
		}
		if got := first["stage"]; got != "protocol_convert" {
			t.Fatalf("conversion[0].stage = %v", got)
		}
		if got := first["from"]; got != "anthropic" {
			t.Fatalf("conversion[0].from = %v", got)
		}
		if got := first["to"]; got != "openai" {
			t.Fatalf("conversion[0].to = %v", got)
		}
		second := conversion[1]
		if got, want := len([]rune(second["stage"].(string))), 64; got != want {
			t.Fatalf("conversion[1].stage runes = %d, want %d", got, want)
		}
		if got, want := len([]rune(second["from"].(string))), 64; got != want {
			t.Fatalf("conversion[1].from runes = %d, want %d", got, want)
		}
	})

	t.Run("omits upstream status without response", func(t *testing.T) {
		rc := testRelayContext()
		rc.Attempts = []contract.AttemptRecord{{Index: 0, ChannelID: 42, UpstreamStatus: 0, ErrorClass: contract.ErrRetryable}}
		triage := triageInfoOf(t, rc)
		if _, ok := triage["upstream_status"]; ok {
			t.Fatalf("upstream_status==0 必须省略，得到 %v", triage["upstream_status"])
		}
		if got := triage["attempts"]; got != 1 {
			t.Fatalf("attempts = %v, want 1", got)
		}
	})

	t.Run("dedupes consecutive identical conversion steps", func(t *testing.T) {
		rc := testRelayContext()
		request := contract.ConversionStepEntry{
			Stage: "request",
			From:  contract.RelayFormat("openai"),
			To:    contract.RelayFormat("anthropic_messages"),
		}
		response := contract.ConversionStepEntry{
			Stage: "protocol_convert",
			From:  contract.RelayFormat("anthropic"),
			To:    contract.RelayFormat("openai"),
		}
		// 同一渠道重试会重复追加 request 步；响应方向的 protocol_convert
		// 是不同 stage，必须保留。
		rc.ConversionChain = []contract.ConversionStepEntry{request, request, request, response, response}

		triage := triageInfoOf(t, rc)
		conversion, ok := triage["conversion"].([]map[string]any)
		if !ok {
			t.Fatalf("conversion = %T, want array: %v", triage["conversion"], triage)
		}
		if len(conversion) != 2 {
			t.Fatalf("conversion 条数 = %d, want 2（相邻重复去重、不同 stage 保留）: %v", len(conversion), conversion)
		}
		if conversion[0]["stage"] != "request" || conversion[1]["stage"] != "protocol_convert" {
			t.Fatalf("conversion stages = %v/%v, want request/protocol_convert", conversion[0]["stage"], conversion[1]["stage"])
		}
	})

	t.Run("no attempts", func(t *testing.T) {
		rc := &contract.RelayContext{RequestPath: "/v1/messages"}
		triage := triageInfoOf(t, rc)
		if got := triage["endpoint"]; got != "messages" {
			t.Fatalf("endpoint = %v, want messages", got)
		}
		if got := triage["attempts"]; got != 0 {
			t.Fatalf("attempts = %v, want 0", got)
		}
		if _, ok := triage["upstream_status"]; ok {
			t.Fatal("没有尝试时不得写 upstream_status")
		}
		if _, ok := triage["request_structure"]; !ok {
			t.Fatal("request_structure 键必须固定存在（空串 = 未校验）")
		}
	})
}

// TestSaturateInsertArgs 钉死 28 个绑参的顺序：writeUsageLog 与 Task 3 的
// RecordRefusal 共用本函数，顺序错位会把证据写进错误的列。
func TestSaturateInsertArgs(t *testing.T) {
	row := usageLogRow{
		requestID:         "0b6f5f3e-5f4e-4d3c-8b2a-1f0e9d8c7b6a",
		userID:            7,
		tokenID:           sql.NullInt64{Int64: 11, Valid: true},
		channelID:         sql.NullInt64{Int64: 42, Valid: true},
		model:             "gpt-4o",
		group:             sql.NullString{String: "vip", Valid: true},
		promptTokens:      1000,
		completionTokens:  200,
		cacheReadTokens:   300,
		cacheWriteTokens:  40,
		preDeducted:       500,
		cost:              321,
		refunded:          179,
		latencyMS:         sql.NullInt64{Int64: 4567, Valid: true},
		firstTokenMS:      sql.NullInt64{Int64: 123, Valid: true},
		stream:            true,
		status:            UsageLogStatusSuccess,
		errorClass:        sql.NullString{String: "upstream_server", Valid: true},
		errorMessage:      sql.NullString{String: "boom", Valid: true},
		retryChain:        sql.NullString{String: `[{"attempt":"1/1"}]`, Valid: true},
		other:             `{"billing_source":"wallet"}`,
		usageSource:       sql.NullInt16{Int16: 1, Valid: true},
		usageSemantic:     sql.NullInt16{Int16: 0, Valid: true},
		reasoningTokens:   321,
		upstreamModel:     sql.NullString{String: "gpt-4o-2026", Valid: true},
		upstreamAccountID: sql.NullInt64{Int64: 555, Valid: true},
		responseID:        sql.NullString{String: "resp_abc", Valid: true},
		attempts:          1,
	}
	args := usageLogInsertArgs(row)
	if len(args) != 28 {
		t.Fatalf("参数个数 = %d, want 28", len(args))
	}
	// 顺序 = usageLogInsertSQL 的 $1..$28；token_name/upstream 是 SQL 常量
	// NULL，不占绑定参数。
	want := []any{
		row.requestID, row.userID, row.tokenID, row.channelID, row.model,
		row.group, row.promptTokens, row.completionTokens, row.cacheReadTokens,
		row.cacheWriteTokens, row.preDeducted, row.cost, row.refunded,
		row.latencyMS, row.firstTokenMS, row.stream, row.status,
		row.errorClass, row.errorMessage, row.retryChain, row.other,
		row.usageSource, row.usageSemantic, row.reasoningTokens,
		row.upstreamModel, row.upstreamAccountID, row.responseID, row.attempts,
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("args[%d] = %#v, want %#v", i, args[i], want[i])
		}
	}
}

// decodeRetryChain 解析 retry_chain JSON 数组。
func decodeRetryChain(t *testing.T, chain string) []map[string]any {
	t.Helper()
	if chain == "" {
		t.Fatal("retry_chain 为空串，期望 JSON 数组")
	}
	var steps []map[string]any
	if err := json.Unmarshal([]byte(chain), &steps); err != nil {
		t.Fatalf("retry_chain 不是 JSON 数组: %v (%s)", err, chain)
	}
	return steps
}

// triageInfoOf 从 buildUsageLogOther 的结果中取出 admin_info.triage。
func triageInfoOf(t *testing.T, rc *contract.RelayContext) map[string]any {
	t.Helper()
	other := buildUsageLogOther(rc, "gpt-4o", sql.NullInt64{}, SettlementOutcome{}, false, nil)
	admin, ok := other["admin_info"].(map[string]any)
	if !ok {
		t.Fatalf("admin_info = %T, want object: %v", other["admin_info"], other)
	}
	triage, ok := admin["triage"].(map[string]any)
	if !ok {
		t.Fatalf("admin_info.triage = %T, want object: %v", admin["triage"], admin)
	}
	return triage
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
	}, false)
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
	// 没有 Attempts（未到达上游）时 retry_chain 必须是 NULL，不能编造空数组。
	if row.retryChain.Valid {
		t.Fatalf("无 Attempts 时 retry_chain 应为 NULL: %+v", row.retryChain)
	}

	// refunded 状态同样强制 cost=0（worker 兜底退款）。
	row, err = buildUsageLogRow(nil, rc, SettlementOutcome{
		Cost: 55, PreDeducted: 55, Refunded: 55, Status: UsageLogStatusRefunded,
	}, false)
	if err != nil {
		t.Fatalf("buildUsageLogRow(refunded): %v", err)
	}
	if row.cost != 0 {
		t.Fatalf("refunded status cost = %d, want 0", row.cost)
	}
}

// TestSaturateRejectsInvalidInput 验证缺 requestID/非法 status/负金额被拒绝。
//
// 必须直接调用 buildUsageLogRow：经 Saturate 且 logsDB=nil 时，任何输入都会
// 先撞上「没有日志库」错误，断言会变成永真的空测试。
func TestSaturateRejectsInvalidInput(t *testing.T) {
	if _, err := buildUsageLogRow(testUsageFact(), nil, SettlementOutcome{}, false); err == nil {
		t.Fatal("buildUsageLogRow(nil rc) = nil, want error")
	}
	if _, err := buildUsageLogRow(testUsageFact(), &contract.RelayContext{}, SettlementOutcome{}, false); err == nil {
		t.Fatal("buildUsageLogRow(empty request id) = nil, want error")
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
		{"negative retained", SettlementOutcome{Status: UsageLogStatusRefunded, RetainedCost: -1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := buildUsageLogRow(testUsageFact(), rc, tc.outcome, false); err == nil {
				t.Fatalf("buildUsageLogRow(%+v) = nil, want error", tc.outcome)
			}
		})
	}
}

// TestSaturateRetainedCost 钉死 R-59：refunded 状态保留的 ChargeIncrement
// 消耗写 cost（>0），纯退款仍为 0；其它非 success 状态一律 0。
func TestSaturateRetainedCost(t *testing.T) {
	rc := testRelayContext()

	row, err := buildUsageLogRow(nil, rc, SettlementOutcome{
		PreDeducted: 300, Refunded: 300, RetainedCost: 50, Status: UsageLogStatusRefunded,
	}, false)
	if err != nil {
		t.Fatalf("buildUsageLogRow(refunded retained): %v", err)
	}
	if row.cost != 50 {
		t.Fatalf("refunded retained cost = %d, want 50（与钱包保留消费一致）", row.cost)
	}
	if row.refunded != 300 {
		t.Fatalf("refunded = %d, want 300（退回的 hold）", row.refunded)
	}

	row, err = buildUsageLogRow(nil, rc, SettlementOutcome{
		PreDeducted: 300, Refunded: 300, Status: UsageLogStatusRefunded,
	}, false)
	if err != nil {
		t.Fatalf("buildUsageLogRow(plain refunded): %v", err)
	}
	if row.cost != 0 {
		t.Fatalf("plain refunded cost = %d, want 0", row.cost)
	}

	// success 只用 Cost，RetainedCost 不参与。
	row, err = buildUsageLogRow(testUsageFact(), rc, SettlementOutcome{
		Cost: 10, RetainedCost: 50, Status: UsageLogStatusSuccess,
	}, false)
	if err != nil {
		t.Fatalf("buildUsageLogRow(success): %v", err)
	}
	if row.cost != 10 {
		t.Fatalf("success cost = %d, want 10", row.cost)
	}

	// error 即使带了 RetainedCost 也强制 0（没有保留消费语义）。
	row, err = buildUsageLogRow(testUsageFact(), rc, SettlementOutcome{
		Cost: 10, RetainedCost: 50, Status: UsageLogStatusError,
	}, false)
	if err != nil {
		t.Fatalf("buildUsageLogRow(error): %v", err)
	}
	if row.cost != 0 {
		t.Fatalf("error cost = %d, want 0", row.cost)
	}
}

// TestSaturateOtherSanitizesNUL 验证 other 的字符串值去掉 NUL/非法 UTF-8：
// jsonb 不接受 \u0000（SQLSTATE 22P05），一个带 %00 的路径就能阻塞结算。
func TestSaturateOtherSanitizesNUL(t *testing.T) {
	rc := testRelayContext()
	rc.RequestPath = "/v1/chat\x00/completions"
	rc.UpstreamModel = "gpt\x00-4o-upstream"

	other := buildUsageLogOther(rc, "gpt-4o", sql.NullInt64{}, SettlementOutcome{}, false, nil)
	blob, err := json.Marshal(other)
	if err != nil {
		t.Fatalf("marshal other: %v", err)
	}
	if strings.Contains(string(blob), `\u0000`) {
		t.Fatalf("other 含 NUL 转义: %s", blob)
	}
	if got := other["request_path"]; got != "/v1/chat/completions" {
		t.Fatalf("request_path = %q, want NUL removed", got)
	}
	if got := other["upstream_model_name"]; got != "gpt-4o-upstream" {
		t.Fatalf("upstream_model_name = %q, want NUL removed", got)
	}
}

// TestSaturateClampsTimingFields 验证 latency/first_token 钳到 PG int32 域。
func TestSaturateClampsTimingFields(t *testing.T) {
	if strconv.IntSize < 64 {
		t.Skip("int 为 32 位，无法构造超 int32 的输入")
	}
	huge := int64(math.MaxInt32) + 7
	ttft := int(huge)
	fact := testUsageFact()
	fact.LatencyMS = int(huge)
	fact.FirstTokenMS = &ttft

	row, err := buildUsageLogRow(fact, testRelayContext(), SettlementOutcome{
		Status: UsageLogStatusSuccess,
	}, false)
	if err != nil {
		t.Fatalf("buildUsageLogRow: %v", err)
	}
	if row.latencyMS.Int64 != math.MaxInt32 {
		t.Fatalf("latency = %d, want MaxInt32", row.latencyMS.Int64)
	}
	if row.firstTokenMS.Int64 != math.MaxInt32 {
		t.Fatalf("first_token = %d, want MaxInt32", row.firstTokenMS.Int64)
	}
}

// TestSaturateStreamPrecedence 验证 fact.Stream 优先于 rc.Streaming；只有
// worker 退款占位（fact=nil）才回退到 rc.Streaming。
func TestSaturateStreamPrecedence(t *testing.T) {
	rc := testRelayContext()
	rc.Streaming = true
	fact := testUsageFact()
	fact.Stream = false

	row, err := buildUsageLogRow(fact, rc, SettlementOutcome{Status: UsageLogStatusSuccess}, false)
	if err != nil {
		t.Fatalf("buildUsageLogRow(fact): %v", err)
	}
	if row.stream {
		t.Fatal("fact.Stream=false 必须优先于 rc.Streaming=true")
	}

	row, err = buildUsageLogRow(nil, rc, SettlementOutcome{Status: UsageLogStatusRefunded}, false)
	if err != nil {
		t.Fatalf("buildUsageLogRow(nil fact): %v", err)
	}
	if !row.stream {
		t.Fatal("fact=nil 时应回退到 rc.Streaming=true")
	}
}

// TestSaturateNoModelMappingWhenSame 验证上游模型与请求模型相同时不写映射标记。
func TestSaturateNoModelMappingWhenSame(t *testing.T) {
	rc := testRelayContext()
	rc.UpstreamModel = rc.OriginModel
	other := buildUsageLogOther(rc, rc.OriginModel, sql.NullInt64{}, SettlementOutcome{}, false, nil)
	if _, ok := other["upstream_model_name"]; ok {
		t.Fatal("上游模型与请求模型相同，不应写 upstream_model_name")
	}
	if _, ok := other["is_model_mapped"]; ok {
		t.Fatal("上游模型与请求模型相同，不应写 is_model_mapped")
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
	}, false)
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
	// M4：订阅来源（失败退款）必须写 subscription；未传来源的零值保持 wallet。
	subRow, err := buildUsageLogRow(testUsageFact(), testRelayContext(), SettlementOutcome{
		Cost: 10, Status: UsageLogStatusSuccess, FundingSource: contract.FundingSubscription,
	}, false)
	if err != nil {
		t.Fatalf("buildUsageLogRow(subscription): %v", err)
	}
	var subOther map[string]any
	if err := json.Unmarshal([]byte(subRow.other), &subOther); err != nil {
		t.Fatalf("subscription other is not JSON: %v", err)
	}
	if got := subOther["billing_source"]; got != "subscription" {
		t.Fatalf("subscription billing_source = %v", got)
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
	other := buildUsageLogOther(rc, "gpt-4o", sql.NullInt64{}, SettlementOutcome{}, false, nil)
	if _, ok := other["applied_discount"]; ok {
		t.Fatal("rate=0 不得凭零值写入 applied_discount")
	}
	if _, ok := other["discount_target"]; ok {
		t.Fatal("契约无 DiscountTarget 字段，不得凭空写入 discount_target")
	}

	rc.Discount = 0.75
	other = buildUsageLogOther(rc, "gpt-4o", sql.NullInt64{}, SettlementOutcome{}, false, nil)
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

// TestBuildUsageLogOtherWritesRawUsageWhenEnabled 验证开关打开且原始 usage
// 非空时，JSON 原样落在 admin_info.usage_raw（对账/审计的唯一原始证据），
// 且不经过 public 分区或 setPublicKey。
func TestBuildUsageLogOtherWritesRawUsageWhenEnabled(t *testing.T) {
	raw := map[string]any{
		"prompt_tokens":     int64(1000),
		"completion_tokens": 200,
		"details":           map[string]any{"cached_tokens": int64(300)},
	}
	rc := &contract.RelayContext{RequestID: "req-raw-on"}
	other := buildUsageLogOther(rc, "gpt-4o", sql.NullInt64{},
		SettlementOutcome{}, true, raw)

	if _, leaked := other["usage_raw"]; leaked {
		t.Fatal("usage_raw must not be written to the public partition")
	}
	adminInfo, ok := other["admin_info"].(map[string]any)
	if !ok {
		t.Fatalf("admin_info = %T, want a map", other["admin_info"])
	}
	got, ok := adminInfo["usage_raw"].(map[string]any)
	if !ok {
		t.Fatalf("admin_info.usage_raw = %T, want a map", adminInfo["usage_raw"])
	}
	if !reflect.DeepEqual(got, raw) {
		t.Fatalf("usage_raw = %#v, want %#v", got, raw)
	}
}

// TestBuildUsageLogOtherOmitsRawUsageWhenDisabled 验证开关关闭时即使有原始
// usage 也不写 usage_raw（admin_info 仍由 triage 承载，不能整体缺席）。
func TestBuildUsageLogOtherOmitsRawUsageWhenDisabled(t *testing.T) {
	rc := &contract.RelayContext{RequestID: "req-raw-off"}
	other := buildUsageLogOther(rc, "gpt-4o", sql.NullInt64{},
		SettlementOutcome{}, false, map[string]any{"prompt_tokens": int64(1000)})
	adminInfo, ok := other["admin_info"].(map[string]any)
	if !ok {
		t.Fatalf("admin_info = %T, want object: %v", other["admin_info"], other)
	}
	if _, present := adminInfo["usage_raw"]; present {
		t.Fatalf("usage_raw = %v, want absent when raw usage is disabled", adminInfo["usage_raw"])
	}
}

// TestBuildUsageLogOtherOmitsEmptyRawUsage 验证 nil/空 map 不产生
// usage_raw。
func TestBuildUsageLogOtherOmitsEmptyRawUsage(t *testing.T) {
	cases := map[string]map[string]any{"nil": nil, "empty": {}}
	for name, raw := range cases {
		rc := &contract.RelayContext{RequestID: "req-raw-" + name}
		other := buildUsageLogOther(rc, "gpt-4o", sql.NullInt64{},
			SettlementOutcome{}, true, raw)
		adminInfo, ok := other["admin_info"].(map[string]any)
		if !ok {
			t.Fatalf("%s: admin_info = %T, want object: %v", name, other["admin_info"], other)
		}
		if _, present := adminInfo["usage_raw"]; present {
			t.Fatalf("%s: usage_raw = %v, want absent", name, adminInfo["usage_raw"])
		}
	}
}

// TestBuildUsageLogRowThreadsRawUsage 验证 DBLogger 的开关经
// buildUsageLogRow 传到 other；worker 的退款占位（fact == nil）
// 没有用量事实，开关打开也不 panic、不写 usage_raw。
func TestBuildUsageLogRowThreadsRawUsage(t *testing.T) {
	fact := testUsageFact()
	fact.Usage.Raw = map[string]any{"prompt_tokens": int64(1000)}
	row, err := buildUsageLogRow(fact, testRelayContext(),
		SettlementOutcome{Status: UsageLogStatusSuccess}, true)
	if err != nil {
		t.Fatalf("buildUsageLogRow: %v", err)
	}
	var other map[string]any
	if err := json.Unmarshal([]byte(row.other), &other); err != nil {
		t.Fatalf("unmarshal other: %v", err)
	}
	adminInfo, ok := other["admin_info"].(map[string]any)
	if !ok {
		t.Fatalf("admin_info = %T, want a map", other["admin_info"])
	}
	raw, ok := adminInfo["usage_raw"].(map[string]any)
	if !ok {
		t.Fatalf("admin_info.usage_raw = %T, want a map", adminInfo["usage_raw"])
	}
	// JSON 往返后数字是 float64；这里只钉住原始键值确实被透传。
	if raw["prompt_tokens"] != float64(1000) {
		t.Fatalf("usage_raw.prompt_tokens = %v, want 1000", raw["prompt_tokens"])
	}

	// worker 退款占位：fact == nil。
	rc := &contract.RelayContext{RequestID: "req-raw-nil-fact"}
	row, err = buildUsageLogRow(nil, rc,
		SettlementOutcome{Status: UsageLogStatusRefunded}, true)
	if err != nil {
		t.Fatalf("buildUsageLogRow(nil fact): %v", err)
	}
	var refundOther map[string]any
	if err := json.Unmarshal([]byte(row.other), &refundOther); err != nil {
		t.Fatalf("unmarshal refunded other: %v", err)
	}
	// admin_info 仍由 triage 承载；没有用量事实时不得出现 usage_raw。
	refundAdmin, ok := refundOther["admin_info"].(map[string]any)
	if !ok {
		t.Fatalf("nil fact admin_info = %T, want a map", refundOther["admin_info"])
	}
	if _, present := refundAdmin["usage_raw"]; present {
		t.Fatalf("nil fact usage_raw = %v, want absent", refundAdmin["usage_raw"])
	}
}

// TestDBLoggerWithRawUsageChains 验证链式装配只改开关、返回同一实例。
func TestDBLoggerWithRawUsageChains(t *testing.T) {
	logger := NewDBLogger(nil)
	if got := logger.WithRawUsage(true); got != logger {
		t.Fatal("WithRawUsage must return the same logger for chaining")
	}
	if !logger.rawUsage {
		t.Fatal("rawUsage = false, want true")
	}
	logger.WithRawUsage(false)
	if logger.rawUsage {
		t.Fatal("rawUsage = true after WithRawUsage(false)")
	}
}
