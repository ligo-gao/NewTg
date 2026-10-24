package relay

import (
	"testing"

	"newtg/gateway/internal/contract"
)

// TestRecordAttemptNilSafe 冻结契约：nil 上下文不 panic（记录是尽力而为，
// 绝不能因为排障证据让热路径失败）。
func TestRecordAttemptNilSafe(t *testing.T) {
	RecordAttempt(nil, contract.AttemptRecord{Index: 0})

	rc := &contract.RelayContext{}
	RecordAttempt(rc, contract.AttemptRecord{Index: 0, ChannelID: 7, UpstreamStatus: 200})
	if len(rc.Attempts) != 1 {
		t.Fatalf("Attempts = %d, want 1", len(rc.Attempts))
	}
	if rc.Attempts[0].ChannelID != 7 || rc.Attempts[0].UpstreamStatus != 200 {
		t.Fatalf("record = %+v, want channel 7 / status 200", rc.Attempts[0])
	}
}

// TestRecordAttemptCapsAtEight 冻结上限：重试上限 5，留余量后最多保留 8 条；
// 超出部分丢弃且保留**前** 8 条（排障关心请求最早打过的渠道）。
func TestRecordAttemptCapsAtEight(t *testing.T) {
	rc := &contract.RelayContext{}
	for i := 0; i < 10; i++ {
		RecordAttempt(rc, contract.AttemptRecord{Index: i, ChannelID: 100 + i})
	}
	if len(rc.Attempts) != 8 {
		t.Fatalf("Attempts = %d, want 8 (capped)", len(rc.Attempts))
	}
	for i, rec := range rc.Attempts {
		if rec.Index != i || rec.ChannelID != 100+i {
			t.Fatalf("Attempts[%d] = %+v, want index %d / channel %d", i, rec, i, 100+i)
		}
	}
}

// TestContractEndpointLabel 冻结端点标签的四个分支（有限枚举，禁止回传原始 path）。
func TestContractEndpointLabel(t *testing.T) {
	cases := map[string]string{
		"/v1/chat/completions": "chat_completions",
		"/v1/messages":         "messages",
		"/v1/responses":        "responses",
		"/v1/embeddings":       "other",
		"":                     "other",
	}
	for path, want := range cases {
		if got := contract.EndpointLabel(path); got != want {
			t.Fatalf("contract.EndpointLabel(%q) = %q, want %q", path, got, want)
		}
	}
}

// TestRelayEndpointLabelDelegates 冻结委托关系：relay.EndpointLabel 仍按
// rc.RequestPath 取值，nil 安全语义不变。
func TestRelayEndpointLabelDelegates(t *testing.T) {
	if got := EndpointLabel(nil); got != "other" {
		t.Fatalf("EndpointLabel(nil) = %q, want other", got)
	}
	rc := &contract.RelayContext{RequestPath: "/v1/messages"}
	if got := EndpointLabel(rc); got != "messages" {
		t.Fatalf("EndpointLabel(rc) = %q, want messages", got)
	}
}
