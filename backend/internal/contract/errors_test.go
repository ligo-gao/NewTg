package contract

import "testing"

// TestErrorClassString 冻结错误类到稳定标签的映射。
//
// 这些标签会写进 usage_logs.error_class 与结构化日志，是排障时唯一可读的
// 有限枚举：必须与 relay.ErrorClassName 的历史取值逐字一致，改动会破坏
// 既有日志查询/看板的分组。
func TestErrorClassString(t *testing.T) {
	cases := []struct {
		class ErrorClass
		want  string
	}{
		{ErrUnknown, "unknown"},
		{ErrRetryable, "retryable"},
		{ErrUpstreamAuth, "upstream_auth"},
		{ErrUpstreamRateLimit, "upstream_rate_limit"},
		{ErrUpstreamServer, "upstream_server"},
		{ErrParamInvalid, "param_invalid"},
		{ErrChannelConfig, "channel_config"},
		{ErrContentFilter, "content_filter"},
		{ErrInsufficientQuota, "insufficient_quota"},
		{ErrChannelError, "channel_error"},
		{ErrEndpointUnsupported, "endpoint_unsupported"},
		{ErrChannelTypeUnimplemented, "channel_type_unimplemented"},
		{ErrConnectionTerminated, "connection_terminated"},
	}
	for _, tc := range cases {
		if got := tc.class.String(); got != tc.want {
			t.Fatalf("ErrorClass(%d).String() = %q, want %q", int(tc.class), got, tc.want)
		}
	}
	// 未冻结的取值也必须落到有界标签，不能把数字透进日志分组。
	if got := ErrorClass(999).String(); got != "unknown" {
		t.Fatalf("ErrorClass(999).String() = %q, want \"unknown\"", got)
	}
}
