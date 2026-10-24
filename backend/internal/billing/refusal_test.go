package billing

import (
	"context"
	"database/sql"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// validRefusalInput 返回一条字段齐全的中继前拒绝输入（单测基准）。
func validRefusalInput() Refusal {
	tokenID := int64(11)
	return Refusal{
		RequestID:    "0b6f5f3e-5f4e-4d3c-8b2a-1f0e9d8c7b6a",
		UserID:       7,
		TokenID:      &tokenID,
		Model:        "gpt-4o",
		Endpoint:     "chat_completions",
		Stage:        RefusalStageAuth,
		ErrorClass:   "unauthorized",
		ErrorMessage: "网关在调用上游前拒绝了请求",
		Structure:    "valid",
	}
}

// TestRecordRefusalValidation 验证必填字段与冻结 Stage 词表的入参校验。
//
// 校验发生在日志库检查之前：合法输入在 nil 日志库上得到「没有日志库」
// 错误，非法输入则在触碰任何 DB 之前就被拒绝。
func TestRecordRefusalValidation(t *testing.T) {
	tests := []struct {
		name            string
		mutate          func(*Refusal)
		wantErrContains string
	}{
		{
			name:            "missing request id",
			mutate:          func(r *Refusal) { r.RequestID = "" },
			wantErrContains: "request id",
		},
		{
			name:            "blank request id",
			mutate:          func(r *Refusal) { r.RequestID = "   " },
			wantErrContains: "request id",
		},
		{
			name:            "missing stage",
			mutate:          func(r *Refusal) { r.Stage = "" },
			wantErrContains: "stage",
		},
		{
			name:            "unknown stage",
			mutate:          func(r *Refusal) { r.Stage = "upstream_error" },
			wantErrContains: "unknown refusal stage",
		},
		{
			name:            "stage is case sensitive",
			mutate:          func(r *Refusal) { r.Stage = "Auth" },
			wantErrContains: "unknown refusal stage",
		},
		{
			name:            "valid input reaches the db check",
			mutate:          func(*Refusal) {},
			wantErrContains: "no logs database",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := validRefusalInput()
			tc.mutate(&in)
			err := NewDBLogger(nil).RecordRefusal(context.Background(), in)
			if err == nil {
				t.Fatal("RecordRefusal = nil, want error")
			}
			if !strings.Contains(err.Error(), tc.wantErrContains) {
				t.Fatalf("RecordRefusal error = %v, want contains %q", err, tc.wantErrContains)
			}
		})
	}
}

// TestRefusalStagesFrozen 钉死五种合法 Stage 的**字面量**。
//
// 为什么不能用包内常量来构造用例：validRefusalStage 的判定基准就是这些常量，
// 拿常量去喂它永远自洽——哪怕常量被改成 "quotas"，这个测试依然全绿，而
// admin_info.triage.stage 已经和 triage 引擎（以及前端字典 key）脱钩，
// 现场表现只是判定悄悄退化成 UNKNOWN。因此这里两边都钉字面量。
func TestRefusalStagesFrozen(t *testing.T) {
	constants := [][2]string{
		{RefusalStageQuota, "quota"},
		{RefusalStageRateLimit, "rate_limit"},
		{RefusalStageAuth, "auth"},
		{RefusalStageRequestStructure, "request_structure"},
		{RefusalStageModelAdmission, "model_admission"},
	}
	for _, pair := range constants {
		if pair[0] != pair[1] {
			t.Fatalf("Stage 常量 = %q, want %q（词表已漂移，读取端会匹配不到）", pair[0], pair[1])
		}
	}

	for _, stage := range []string{"quota", "rate_limit", "auth", "request_structure", "model_admission"} {
		if !validRefusalStage(stage) {
			t.Fatalf("validRefusalStage(%q) = false, want true", stage)
		}
	}
	for _, stage := range []string{"", "Auth", "upstream_error", "quotas"} {
		if validRefusalStage(stage) {
			t.Fatalf("validRefusalStage(%q) = true, want false", stage)
		}
	}
}

// TestRecordRefusalNilLogger 验证 nil *DBLogger 与 nil logs DB 都返回错误而不是 panic。
func TestRecordRefusalNilLogger(t *testing.T) {
	in := validRefusalInput()

	var nilLogger *DBLogger
	if err := nilLogger.RecordRefusal(context.Background(), in); err == nil {
		t.Fatal("nil *DBLogger.RecordRefusal = nil, want error")
	}
	if err := NewDBLogger(nil).RecordRefusal(context.Background(), in); err == nil {
		t.Fatal("NewDBLogger(nil).RecordRefusal = nil, want error")
	}
}

// TestBuildRefusalRow 验证投影结果：状态、金额归零、NULL 语义与 triage 证据。
func TestBuildRefusalRow(t *testing.T) {
	in := validRefusalInput()
	row, err := buildRefusalRow(in)
	if err != nil {
		t.Fatalf("buildRefusalRow: %v", err)
	}

	if row.status != UsageLogStatusRefused {
		t.Fatalf("status = %d, want %d (refused)", row.status, UsageLogStatusRefused)
	}
	// 拒绝发生在计费之前：所有数量/金额字段必须为零。
	amounts := map[string]int{
		"promptTokens":     row.promptTokens,
		"completionTokens": row.completionTokens,
		"cacheReadTokens":  row.cacheReadTokens,
		"cacheWriteTokens": row.cacheWriteTokens,
	}
	for name, got := range amounts {
		if got != 0 {
			t.Fatalf("%s = %d, want 0", name, got)
		}
	}
	if row.preDeducted != 0 || row.cost != 0 || row.refunded != 0 {
		t.Fatalf("amounts = %d/%d/%d, want 0/0/0", row.preDeducted, row.cost, row.refunded)
	}
	if row.retryChain.Valid {
		t.Fatalf("retryChain = %+v, want NULL（未到达上游）", row.retryChain)
	}
	if row.stream {
		t.Fatal("stream = true, want false")
	}
	if row.latencyMS.Valid || row.firstTokenMS.Valid {
		t.Fatalf("latency/firstToken 应为 NULL: %+v %+v", row.latencyMS, row.firstTokenMS)
	}
	if row.channelID.Valid || row.group.Valid {
		t.Fatalf("channel/group 应为 NULL: %+v %+v", row.channelID, row.group)
	}
	if !row.tokenID.Valid || row.tokenID.Int64 != 11 {
		t.Fatalf("tokenID = %+v, want 11", row.tokenID)
	}
	if row.model != "gpt-4o" || row.userID != 7 {
		t.Fatalf("model/user = %s/%d", row.model, row.userID)
	}
	if row.errorClass.String != "unauthorized" || !row.errorClass.Valid {
		t.Fatalf("errorClass = %+v", row.errorClass)
	}

	var other map[string]any
	if err := json.Unmarshal([]byte(row.other), &other); err != nil {
		t.Fatalf("other 不是 JSON: %v (%s)", err, row.other)
	}
	admin, ok := other["admin_info"].(map[string]any)
	if !ok {
		t.Fatalf("other.admin_info = %T, want object: %v", other["admin_info"], other)
	}
	triage, ok := admin["triage"].(map[string]any)
	if !ok {
		t.Fatalf("admin_info.triage = %T, want object: %v", admin["triage"], admin)
	}
	if got := triage["stage"]; got != RefusalStageAuth {
		t.Fatalf("triage.stage = %v, want %s", got, RefusalStageAuth)
	}
	if got := triage["endpoint"]; got != "chat_completions" {
		t.Fatalf("triage.endpoint = %v, want chat_completions", got)
	}
	if got := triage["request_structure"]; got != "valid" {
		t.Fatalf("triage.request_structure = %v, want valid", got)
	}
}

// TestBuildRefusalTokenID 验证 nil 与 0 的区分：nil 写 NULL，0 原样写 0。
func TestBuildRefusalTokenID(t *testing.T) {
	zero := int64(0)
	tests := []struct {
		name      string
		tokenID   *int64
		wantValid bool
		wantValue int64
	}{
		{"nil token", nil, false, 0},
		{"token zero", &zero, true, 0},
		{"token set", func() *int64 { v := int64(11); return &v }(), true, 11},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := validRefusalInput()
			in.TokenID = tc.tokenID
			row, err := buildRefusalRow(in)
			if err != nil {
				t.Fatalf("buildRefusalRow: %v", err)
			}
			if row.tokenID.Valid != tc.wantValid || row.tokenID.Int64 != tc.wantValue {
				t.Fatalf("tokenID = %+v, want valid=%v value=%d",
					row.tokenID, tc.wantValid, tc.wantValue)
			}
		})
	}
}

// TestBuildRefusalStructureOmitted 验证 Structure=="" 时省略 request_structure 键。
func TestBuildRefusalStructureOmitted(t *testing.T) {
	tests := []struct {
		name      string
		structure string
		wantKey   bool
	}{
		{"empty omits key", "", false},
		{"valid writes key", "valid", true},
		{"invalid writes key", "invalid", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := validRefusalInput()
			in.Structure = tc.structure
			other, err := buildRefusalOther(in)
			if err != nil {
				t.Fatalf("buildRefusalOther: %v", err)
			}
			var decoded map[string]any
			if err := json.Unmarshal([]byte(other), &decoded); err != nil {
				t.Fatalf("other 不是 JSON: %v (%s)", err, other)
			}
			admin := decoded["admin_info"].(map[string]any)
			triage := admin["triage"].(map[string]any)
			got, ok := triage["request_structure"]
			if ok != tc.wantKey {
				t.Fatalf("request_structure 存在 = %v, want %v (triage=%v)", ok, tc.wantKey, triage)
			}
			if tc.wantKey && got != tc.structure {
				t.Fatalf("request_structure = %v, want %q", got, tc.structure)
			}
		})
	}
}

// TestRefusalInsertArgs 钉死 21 个绑参的顺序：与 writeUsageLog 共用
// usageLogInsertSQL，顺序错位会把证据写进错误的列。
func TestRefusalInsertArgs(t *testing.T) {
	row, err := buildRefusalRow(validRefusalInput())
	if err != nil {
		t.Fatalf("buildRefusalRow: %v", err)
	}
	args := usageLogInsertArgs(row)
	if len(args) != 21 {
		t.Fatalf("参数个数 = %d, want 21", len(args))
	}

	// SQL 占位符必须是按出现顺序的 $1..$21，且总数与绑参一致。
	placeholders := regexp.MustCompile(`\$(\d+)`).FindAllStringSubmatch(usageLogInsertSQL, -1)
	if len(placeholders) != 21 {
		t.Fatalf("SQL 占位符 = %d, want 21", len(placeholders))
	}
	for i, match := range placeholders {
		if match[1] != strconv.Itoa(i+1) {
			t.Fatalf("第 %d 个占位符 = $%s, want $%d", i, match[1], i+1)
		}
	}

	want := []any{
		row.requestID, row.userID, row.tokenID, row.channelID, row.model,
		row.group, row.promptTokens, row.completionTokens, row.cacheReadTokens,
		row.cacheWriteTokens, row.preDeducted, row.cost, row.refunded,
		row.latencyMS, row.firstTokenMS, row.stream, row.status,
		row.errorClass, row.errorMessage, row.retryChain, row.other,
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("args[%d] = %#v, want %#v", i, args[i], want[i])
		}
	}
	// 拒绝行的关键位置：$17=status（refused），$7..$13 全零，$20=retry_chain NULL。
	if args[16] != UsageLogStatusRefused {
		t.Fatalf("args[16] (status) = %v, want %d", args[16], UsageLogStatusRefused)
	}
	// $7..$10 是 int 列，$11..$13 是 bigint 列；按各自动态类型断言为零。
	for _, idx := range []int{6, 7, 8, 9} {
		if got, ok := args[idx].(int); !ok || got != 0 {
			t.Fatalf("args[%d] = %#v, want int(0)（拒绝行 token 数归零）", idx, args[idx])
		}
	}
	for _, idx := range []int{10, 11, 12} {
		if got, ok := args[idx].(int64); !ok || got != 0 {
			t.Fatalf("args[%d] = %#v, want int64(0)（拒绝行金额归零）", idx, args[idx])
		}
	}
	if args[19] != (sql.NullString{}) {
		t.Fatalf("args[19] (retry_chain) = %#v, want NULL", args[19])
	}
}

// TestUsageLogInsertArgsOrderPinned 用逐位互不相同的哨兵值钉死 21 个绑参的顺序。
//
// TestRefusalInsertArgs 的期望值是拿同一个 row 重新拼出来的，而拒绝行的
// token 数、金额、延迟全是零值/NULL——把任意两个零值位对调，那个测试依然
// 全绿。绑参顺序是跨任务契约（writeUsageLog、批量路径、RecordRefusal 共用
// 同一条 usageLogInsertSQL），错位会把证据写进错误的列且不报错，因此这里
// 用可区分类型的哨兵值逐一比对：int / int64 / int16 / bool / sql.NullString /
// sql.NullInt64 的类型差异也在比对范围内。
func TestUsageLogInsertArgsOrderPinned(t *testing.T) {
	ns := func(v string) sql.NullString { return sql.NullString{String: v, Valid: true} }
	ni := func(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: true} }

	row := usageLogRow{
		requestID:        "s1-request-id",
		userID:           201,
		tokenID:          ni(202),
		channelID:        ni(203),
		model:            "s5-model",
		group:            ns("s6-group"),
		promptTokens:     207,
		completionTokens: 208,
		cacheReadTokens:  209,
		cacheWriteTokens: 210,
		preDeducted:      211,
		cost:             212,
		refunded:         213,
		latencyMS:        ni(214),
		firstTokenMS:     ni(215),
		stream:           true,
		status:           216,
		errorClass:       ns("s18-class"),
		errorMessage:     ns("s19-message"),
		retryChain:       ns("s20-chain"),
		other:            "s21-other",
	}

	want := []any{
		"s1-request-id", int64(201), ni(202), ni(203), "s5-model",
		ns("s6-group"), 207, 208, 209, 210,
		int64(211), int64(212), int64(213), ni(214), ni(215),
		true, int16(216), ns("s18-class"), ns("s19-message"), ns("s20-chain"), "s21-other",
	}

	args := usageLogInsertArgs(row)
	if len(args) != len(want) {
		t.Fatalf("参数个数 = %d, want %d", len(args), len(want))
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("绑参第 %d 位（$%d）= %#v, want %#v：顺序或类型已错位", i, i+1, args[i], want[i])
		}
	}
}
