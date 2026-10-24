package httpapi

import (
	"encoding/json"
	"testing"
	"time"

	"newtg/gateway/internal/billing"
)

// TestTriageEvidenceFromRowServerFailureSeen 钉死 retry_chain → Evidence 的
// 服务端故障投影（final-review Major 2 / NF-2）：
//
//   - 任一 5xx 都算「见过服务端故障」；
//   - 任一 nil/0 状态（末轮或中途）都是「没拿到响应」，同样算；
//     `[429, 0]` 因而不得再声称「没有 5xx 或超时」；
//   - 单轮 4xx 没有服务端故障证据，排除渠道是诚实的；
//   - 但 upstream_status 仍取「最近一次非零状态码」，所以 `[0]` 的信号仍是
//     中性的 no_upstream_response（该分支不看本标记）。
//
// 同时守住 upstream_status 的既有口径：最近一次**非零**状态码。
func TestTriageEvidenceFromRowServerFailureSeen(t *testing.T) {
	tests := []struct {
		name       string
		chain      string
		wantStatus int
		wantSeen   bool
	}{
		{name: "5xx then 4xx", chain: `[{"upstream_status":503},{"upstream_status":400}]`, wantStatus: 400, wantSeen: true},
		{name: "timeout then 4xx", chain: `[{"upstream_status":0},{"upstream_status":400}]`, wantStatus: 400, wantSeen: true},
		{name: "missing status then 4xx", chain: `[{},{"upstream_status":400}]`, wantStatus: 400, wantSeen: true},
		{name: "4xx then final timeout", chain: `[{"upstream_status":429},{"upstream_status":0}]`, wantStatus: 429, wantSeen: true},
		{name: "single 4xx", chain: `[{"upstream_status":400}]`, wantStatus: 400, wantSeen: false},
		{name: "single timeout", chain: `[{"upstream_status":0}]`, wantStatus: 0, wantSeen: true},
		{name: "single missing status", chain: `[{}]`, wantStatus: 0, wantSeen: true},
		{name: "single 5xx", chain: `[{"upstream_status":503}]`, wantStatus: 503, wantSeen: true},
		{name: "5xx then timeout", chain: `[{"upstream_status":503},{"upstream_status":0}]`, wantStatus: 503, wantSeen: true},
		{name: "timeout only, no chain", chain: ``, wantStatus: 0, wantSeen: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			row := &usageLogRow{
				Status:     billing.UsageLogStatusError,
				CreatedAt:  time.Now().UTC(),
				RetryChain: []byte(tc.chain),
			}
			_, engine := triageEvidenceFromRow(row)
			if engine.UpstreamStatus != tc.wantStatus {
				t.Fatalf("UpstreamStatus = %d, want %d", engine.UpstreamStatus, tc.wantStatus)
			}
			if engine.ServerFailureSeen != tc.wantSeen {
				t.Fatalf("ServerFailureSeen = %v, want %v（chain=%s）", engine.ServerFailureSeen, tc.wantSeen, tc.chain)
			}
			if tc.chain != "" && engine.Attempts != countChainEntries(t, tc.chain) {
				t.Fatalf("Attempts = %d, want chain length", engine.Attempts)
			}
		})
	}
}

func countChainEntries(t *testing.T, raw string) int {
	t.Helper()
	var entries []map[string]any
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		t.Fatalf("decode chain %q: %v", raw, err)
	}
	return len(entries)
}
