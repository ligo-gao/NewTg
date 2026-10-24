package triage

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

// TestClassify 用表驱动覆盖 7 条规则、规则顺序，以及每条 verdict 的
// 「至少 2 条 signal，且至少一条 exclude-*/neutral」不变量。
func TestClassify(t *testing.T) {
	tests := []struct {
		name       string
		evidence   Evidence
		wantOwner  Owner
		wantSub    string
		wantRule   string
		wantCodes  []string
		wantVotes  map[string]string
		wantParams map[string]map[string]any
	}{
		{
			name:      "USR-01 request structure invalid",
			evidence:  Evidence{RequestStructure: "invalid", UpstreamStatus: 400},
			wantOwner: OwnerUser, wantSub: "request_structure", wantRule: "USR-01",
			wantCodes: []string{"request_structure_invalid", "conversion_absent", "no_upstream_server_error"},
			wantVotes: map[string]string{
				"request_structure_invalid": VoteUser,
				"conversion_absent":         VoteNeutral,
				"no_upstream_server_error":  VoteExcludeChannel,
			},
			wantParams: map[string]map[string]any{
				"no_upstream_server_error": {"attempts": 0},
			},
		},
		{
			name: "USR-01 wins over GW-01 when structure is invalid",
			evidence: Evidence{
				RequestStructure: "invalid",
				UpstreamStatus:   400,
				Attempts:         1,
				ConversionSteps:  1,
				RouteFrom:        "anthropic",
				RouteTo:          "openai",
			},
			wantOwner: OwnerUser, wantSub: "request_structure", wantRule: "USR-01",
			wantCodes: []string{"request_structure_invalid", "conversion_applied", "no_upstream_server_error"},
			wantVotes: map[string]string{
				"request_structure_invalid": VoteUser,
				// 转换存在，但结构在转换前就已非法，因此转换只作中性证据。
				"conversion_applied":       VoteNeutral,
				"no_upstream_server_error": VoteExcludeChannel,
			},
			wantParams: map[string]map[string]any{
				"conversion_applied":       {"steps": 1, "from": "anthropic", "to": "openai"},
				"no_upstream_server_error": {"attempts": 1},
			},
		},
		{
			name:      "USR-01 wins over USR-02 for a structural refusal",
			evidence:  Evidence{Status: "refused", RejectStage: "request_structure", RequestStructure: "invalid"},
			wantOwner: OwnerUser, wantSub: "request_structure", wantRule: "USR-01",
			wantCodes: []string{"request_structure_invalid", "conversion_absent", "no_upstream_attempt"},
			wantVotes: map[string]string{
				"request_structure_invalid": VoteUser,
				"conversion_absent":         VoteNeutral,
				"no_upstream_attempt":       VoteNeutral,
			},
			wantParams: map[string]map[string]any{
				"no_upstream_attempt": {"attempts": 0},
			},
		},
		{
			name:      "USR-02 refused at quota stage",
			evidence:  Evidence{Status: "refused", RejectStage: "quota", RequestStructure: "valid"},
			wantOwner: OwnerUser, wantSub: "quota", wantRule: "USR-02",
			wantCodes: []string{"refused_before_upstream", "no_upstream_attempt", "request_structure_valid"},
			wantVotes: map[string]string{
				"refused_before_upstream": VoteUser,
				"no_upstream_attempt":     VoteNeutral,
				"request_structure_valid": VoteExcludeUser,
			},
			wantParams: map[string]map[string]any{
				"refused_before_upstream": {"stage": "quota"},
				"no_upstream_attempt":     {"attempts": 0},
			},
		},
		{
			// 防御性口径（Minor 2）：refused 只应写在上游接触之前。一旦行里
			// 带着上游状态码，说明这条拒绝记录与其他证据矛盾——绝不归责用户，
			// 退回「证据不足」，让管理员看到诚实的中性事实。
			name: "refused with upstream evidence fails open to UNKNOWN",
			evidence: Evidence{
				Status:         "refused",
				RejectStage:    "rate_limit",
				UpstreamStatus: 502,
			},
			wantOwner: OwnerUnknown, wantSub: "insufficient_evidence", wantRule: "UNKNOWN",
			wantCodes: []string{"upstream_status", "request_structure_unverified", "conversion_absent"},
			wantVotes: map[string]string{
				"upstream_status":              VoteNeutral,
				"request_structure_unverified": VoteNeutral,
				"conversion_absent":            VoteNeutral,
			},
			wantParams: map[string]map[string]any{
				"upstream_status": {"upstream_status": 502},
			},
		},
		{
			name:      "refused with attempt evidence fails open to UNKNOWN",
			evidence:  Evidence{Status: "refused", RejectStage: "rate_limit", Attempts: 1},
			wantOwner: OwnerUnknown, wantSub: "insufficient_evidence", wantRule: "UNKNOWN",
			wantCodes: []string{"no_upstream_response", "request_structure_unverified", "conversion_absent"},
			wantVotes: map[string]string{
				"no_upstream_response":         VoteNeutral,
				"request_structure_unverified": VoteNeutral,
				"conversion_absent":            VoteNeutral,
			},
			wantParams: map[string]map[string]any{
				"no_upstream_response": {"attempts": 1},
			},
		},
		{
			name:      "USR-02 empty reject stage falls back to unknown",
			evidence:  Evidence{Status: "refused", RequestStructure: "valid"},
			wantOwner: OwnerUser, wantSub: "unknown", wantRule: "USR-02",
			wantCodes: []string{"refused_before_upstream", "no_upstream_attempt", "request_structure_valid"},
			wantVotes: map[string]string{
				"refused_before_upstream": VoteUser,
				"no_upstream_attempt":     VoteNeutral,
				"request_structure_valid": VoteExcludeUser,
			},
			wantParams: map[string]map[string]any{
				"refused_before_upstream": {"stage": "unknown"},
			},
		},
		{
			name:      "USR-01 keeps an upstream auth status neutral",
			evidence:  Evidence{RequestStructure: "invalid", UpstreamStatus: 401},
			wantOwner: OwnerUser, wantSub: "request_structure", wantRule: "USR-01",
			wantCodes: []string{"request_structure_invalid", "conversion_absent", "upstream_status"},
			wantVotes: map[string]string{
				"request_structure_invalid": VoteUser,
				"conversion_absent":         VoteNeutral,
				// 401 指向渠道，不能被当成「排除渠道」的证据。
				"upstream_status": VoteNeutral,
			},
			wantParams: map[string]map[string]any{
				"upstream_status": {"upstream_status": 401},
			},
		},
		{
			name:      "CH-01 upstream auth 401",
			evidence:  Evidence{UpstreamStatus: 401, RequestStructure: "valid", Attempts: 1},
			wantOwner: OwnerChannel, wantSub: "upstream_auth", wantRule: "CH-01",
			wantCodes: []string{"upstream_auth_rejected", "request_structure_valid", "conversion_absent"},
			wantVotes: map[string]string{
				"upstream_auth_rejected":  VoteChannel,
				"request_structure_valid": VoteExcludeUser,
				"conversion_absent":       VoteNeutral,
			},
			wantParams: map[string]map[string]any{
				"upstream_auth_rejected": {"upstream_status": 401},
			},
		},
		{
			name:      "CH-01 wins over GW-02",
			evidence:  Evidence{UpstreamStatus: 403, ErrorClass: "endpoint_unsupported", RequestStructure: "valid"},
			wantOwner: OwnerChannel, wantSub: "upstream_auth", wantRule: "CH-01",
			wantCodes: []string{"upstream_auth_rejected", "request_structure_valid", "conversion_absent"},
			wantVotes: map[string]string{
				"upstream_auth_rejected": VoteChannel,
				"conversion_absent":      VoteNeutral,
			},
		},
		{
			name:      "CH-02 upstream server error with retries",
			evidence:  Evidence{UpstreamStatus: 503, Attempts: 3, RequestStructure: "valid"},
			wantOwner: OwnerChannel, wantSub: "upstream_server", wantRule: "CH-02",
			wantCodes: []string{"upstream_server_error", "attempt_count", "request_structure_valid", "conversion_absent"},
			wantVotes: map[string]string{
				"upstream_server_error":   VoteChannel,
				"attempt_count":           VoteChannel,
				"request_structure_valid": VoteExcludeUser,
				"conversion_absent":       VoteNeutral,
			},
			wantParams: map[string]map[string]any{
				"upstream_server_error": {"upstream_status": 503},
				"attempt_count":         {"attempts": 3},
			},
		},
		{
			name:      "CH-02 single attempt is neutral retry evidence",
			evidence:  Evidence{UpstreamStatus: 500, Attempts: 1},
			wantOwner: OwnerChannel, wantSub: "upstream_server", wantRule: "CH-02",
			wantCodes: []string{"upstream_server_error", "attempt_count", "request_structure_unverified", "conversion_absent"},
			wantVotes: map[string]string{
				"upstream_server_error":        VoteChannel,
				"attempt_count":                VoteNeutral,
				"request_structure_unverified": VoteNeutral,
				"conversion_absent":            VoteNeutral,
			},
		},
		{
			name: "GW-01 conversion made an upstream 4xx",
			evidence: Evidence{
				UpstreamStatus:   400,
				Attempts:         1,
				ConversionSteps:  2,
				RouteFrom:        "anthropic",
				RouteTo:          "openai",
				RequestStructure: "valid",
			},
			wantOwner: OwnerGateway, wantSub: "protocol_conversion", wantRule: "GW-01",
			wantCodes: []string{"no_upstream_server_error", "conversion_applied", "request_structure_valid"},
			wantVotes: map[string]string{
				"no_upstream_server_error": VoteExcludeChannel,
				"conversion_applied":       VoteGateway,
				"request_structure_valid":  VoteExcludeUser,
			},
			wantParams: map[string]map[string]any{
				"conversion_applied":       {"steps": 2, "from": "anthropic", "to": "openai"},
				"no_upstream_server_error": {"attempts": 1},
			},
		},
		{
			name:      "GW-01 wins over GW-02 when conversion happened",
			evidence:  Evidence{UpstreamStatus: 400, ConversionSteps: 1, ErrorClass: "endpoint_unsupported", RequestStructure: "valid"},
			wantOwner: OwnerGateway, wantSub: "protocol_conversion", wantRule: "GW-01",
			wantCodes: []string{"no_upstream_server_error", "conversion_applied", "request_structure_valid"},
			wantVotes: map[string]string{
				"no_upstream_server_error": VoteExcludeChannel,
				"conversion_applied":       VoteGateway,
				"request_structure_valid":  VoteExcludeUser,
			},
		},
		{
			name:      "GW-02 unsupported endpoint",
			evidence:  Evidence{ErrorClass: "endpoint_unsupported", UpstreamStatus: 404, RequestStructure: "valid"},
			wantOwner: OwnerGateway, wantSub: "unsupported", wantRule: "GW-02",
			wantCodes: []string{"error_class_unsupported", "no_upstream_server_error", "request_structure_valid", "conversion_absent"},
			wantVotes: map[string]string{
				"error_class_unsupported":  VoteGateway,
				"no_upstream_server_error": VoteExcludeChannel,
				"request_structure_valid":  VoteExcludeUser,
				"conversion_absent":        VoteNeutral,
			},
			wantParams: map[string]map[string]any{
				"error_class_unsupported": {"class": "endpoint_unsupported"},
			},
		},
		{
			name:      "GW-02 channel type unimplemented without upstream",
			evidence:  Evidence{ErrorClass: "channel_type_unimplemented"},
			wantOwner: OwnerGateway, wantSub: "unsupported", wantRule: "GW-02",
			wantCodes: []string{"error_class_unsupported", "no_upstream_attempt", "request_structure_unverified", "conversion_absent"},
			wantVotes: map[string]string{
				"error_class_unsupported":      VoteGateway,
				"no_upstream_attempt":          VoteNeutral,
				"request_structure_unverified": VoteNeutral,
			},
		},
		{
			// Major 2：[503, 400] 的重试链以 4xx 收尾，但链上已有 5xx。
			// 此时不得声称「没有服务端故障」，也不得投 exclude-channel。
			name: "4xx after a 5xx does not exclude the channel",
			evidence: Evidence{
				UpstreamStatus:    400,
				Attempts:          2,
				ServerFailureSeen: true,
				RequestStructure:  "valid",
			},
			wantOwner: OwnerUnknown, wantSub: "insufficient_evidence", wantRule: "UNKNOWN",
			wantCodes: []string{"upstream_status", "request_structure_valid", "conversion_absent"},
			wantVotes: map[string]string{
				"upstream_status":         VoteNeutral,
				"request_structure_valid": VoteExcludeUser,
				"conversion_absent":       VoteNeutral,
			},
			wantParams: map[string]map[string]any{
				"upstream_status": {"upstream_status": 400},
			},
		},
		{
			// 对照：[400] 单轮没有服务端故障证据，排除渠道是诚实的。
			name:      "single 4xx still excludes the channel",
			evidence:  Evidence{UpstreamStatus: 400, Attempts: 1},
			wantOwner: OwnerUnknown, wantSub: "insufficient_evidence", wantRule: "UNKNOWN",
			wantCodes: []string{"no_upstream_server_error", "request_structure_unverified", "conversion_absent"},
			wantVotes: map[string]string{
				"no_upstream_server_error":     VoteExcludeChannel,
				"request_structure_unverified": VoteNeutral,
				"conversion_absent":            VoteNeutral,
			},
			wantParams: map[string]map[string]any{
				"no_upstream_server_error": {"attempts": 1},
			},
		},
		{
			name:      "UNKNOWN no rule matches",
			evidence:  Evidence{UpstreamStatus: 400, ErrorClass: "param_invalid", RequestStructure: "valid", Attempts: 1},
			wantOwner: OwnerUnknown, wantSub: "insufficient_evidence", wantRule: "UNKNOWN",
			wantCodes: []string{"no_upstream_server_error", "request_structure_valid", "error_class", "conversion_absent"},
			wantVotes: map[string]string{
				"no_upstream_server_error": VoteExcludeChannel,
				"request_structure_valid":  VoteExcludeUser,
				"error_class":              VoteNeutral,
				"conversion_absent":        VoteNeutral,
			},
			wantParams: map[string]map[string]any{
				"error_class":              {"class": "param_invalid"},
				"no_upstream_server_error": {"attempts": 1},
			},
		},
		{
			name:      "UNKNOWN timeout after retries reports no response",
			evidence:  Evidence{UpstreamStatus: 0, Attempts: 3},
			wantOwner: OwnerUnknown, wantSub: "insufficient_evidence", wantRule: "UNKNOWN",
			wantCodes: []string{"no_upstream_response", "request_structure_unverified", "conversion_absent"},
			wantVotes: map[string]string{
				"no_upstream_response":         VoteNeutral,
				"request_structure_unverified": VoteNeutral,
				"conversion_absent":            VoteNeutral,
			},
			wantParams: map[string]map[string]any{
				"no_upstream_response": {"attempts": 3},
			},
		},
		{
			// NF-2：[0] 的重试链现在把「没有拿到响应」计为 ServerFailureSeen，
			// 但信号仍必须是中性的 no_upstream_response——该分支只看状态码与
			// 尝试次数，标记不得改变它。
			name:      "timeout with server-failure flag still reports no response",
			evidence:  Evidence{UpstreamStatus: 0, Attempts: 1, ServerFailureSeen: true},
			wantOwner: OwnerUnknown, wantSub: "insufficient_evidence", wantRule: "UNKNOWN",
			wantCodes: []string{"no_upstream_response", "request_structure_unverified", "conversion_absent"},
			wantVotes: map[string]string{
				"no_upstream_response":         VoteNeutral,
				"request_structure_unverified": VoteNeutral,
				"conversion_absent":            VoteNeutral,
			},
			wantParams: map[string]map[string]any{
				"no_upstream_response": {"attempts": 1},
			},
		},
		{
			name:      "GW-02 unimplemented with timed-out attempts",
			evidence:  Evidence{ErrorClass: "channel_type_unimplemented", UpstreamStatus: 0, Attempts: 2},
			wantOwner: OwnerGateway, wantSub: "unsupported", wantRule: "GW-02",
			wantCodes: []string{"error_class_unsupported", "no_upstream_response", "request_structure_unverified", "conversion_absent"},
			wantVotes: map[string]string{
				"error_class_unsupported":      VoteGateway,
				"no_upstream_response":         VoteNeutral,
				"request_structure_unverified": VoteNeutral,
				"conversion_absent":            VoteNeutral,
			},
			wantParams: map[string]map[string]any{
				"no_upstream_response": {"attempts": 2},
			},
		},
		{
			name:      "UNKNOWN empty evidence still has two signals",
			evidence:  Evidence{},
			wantOwner: OwnerUnknown, wantSub: "insufficient_evidence", wantRule: "UNKNOWN",
			wantCodes: []string{"no_upstream_attempt", "request_structure_unverified", "conversion_absent"},
			wantVotes: map[string]string{
				"no_upstream_attempt":          VoteNeutral,
				"request_structure_unverified": VoteNeutral,
				"conversion_absent":            VoteNeutral,
			},
			wantParams: map[string]map[string]any{
				"no_upstream_attempt": {"attempts": 0},
			},
		},
	}

	validVotes := map[string]bool{
		VoteUser:           true,
		VoteChannel:        true,
		VoteGateway:        true,
		VoteExcludeUser:    true,
		VoteExcludeChannel: true,
		VoteNeutral:        true,
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Classify(tt.evidence)

			if got.Owner != tt.wantOwner || got.Sub != tt.wantSub || got.Rule != tt.wantRule {
				t.Fatalf("Classify() = owner %q sub %q rule %q, want owner %q sub %q rule %q",
					got.Owner, got.Sub, got.Rule, tt.wantOwner, tt.wantSub, tt.wantRule)
			}

			if len(got.Signals) < 2 {
				t.Fatalf("got %d signals, want at least 2: %+v", len(got.Signals), got.Signals)
			}

			var hasExclusionOrNeutral bool
			codes := make([]string, 0, len(got.Signals))
			byCode := make(map[string]Signal, len(got.Signals))
			for _, s := range got.Signals {
				if s.Code == "" {
					t.Errorf("signal has empty code: %+v", s)
				}
				if !validVotes[s.Vote] {
					t.Errorf("signal %q has invalid vote %q", s.Code, s.Vote)
				}
				if s.Vote == VoteNeutral || strings.HasPrefix(s.Vote, "exclude-") {
					hasExclusionOrNeutral = true
				}
				if _, dup := byCode[s.Code]; dup {
					t.Errorf("duplicate signal code %q", s.Code)
				}
				byCode[s.Code] = s
				codes = append(codes, s.Code)
			}

			if !hasExclusionOrNeutral {
				t.Errorf("no neutral or exclude-* signal in %+v", got.Signals)
			}
			if !slices.Equal(codes, tt.wantCodes) {
				t.Errorf("signal codes = %v, want %v", codes, tt.wantCodes)
			}

			for code, wantVote := range tt.wantVotes {
				s, ok := byCode[code]
				if !ok {
					t.Errorf("missing signal %q", code)
					continue
				}
				if s.Vote != wantVote {
					t.Errorf("signal %q vote = %q, want %q", code, s.Vote, wantVote)
				}
			}

			for code, wantParams := range tt.wantParams {
				s, ok := byCode[code]
				if !ok {
					t.Errorf("missing signal %q", code)
					continue
				}
				if !reflect.DeepEqual(s.Params, wantParams) {
					t.Errorf("signal %q params = %#v, want %#v", code, s.Params, wantParams)
				}
			}
		})
	}
}
