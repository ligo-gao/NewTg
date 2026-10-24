package reconcile

import "testing"

func TestCompareTreatsZeroRequestDayAsPresent(t *testing.T) {
	zero := int64(0)
	c := Compare(Input{
		Local:        &LocalAggregate{}, // row exists, every request failed
		Snapshot:     &Snapshot{CacheHitTokens: &zero, CacheMissTokens: &zero, OutputTokens: &zero},
		Tolerances:   Tolerances{TokenBP: 100, MinDeltaTokens: 1000, AmountMinor: 100},
		QuotaPerUnit: 100,
	})
	if c.Severity == SeverityMissingLocal {
		t.Fatalf("a present-but-zero local day must not be missing_local")
	}
	if c.Severity != SeverityOK {
		t.Fatalf("severity = %s, want ok", c.Severity)
	}
}

func TestCompareAllNullSnapshotIsPartialNotAlert(t *testing.T) {
	amount := int64(5000)
	c := Compare(Input{
		Local:        &LocalAggregate{Requests: 10, CacheHitTokens: 1000, CostCredits: 5000},
		Snapshot:     &Snapshot{AmountMinor: &amount},
		Tolerances:   Tolerances{TokenBP: 100, MinDeltaTokens: 1000, AmountMinor: 100},
		QuotaPerUnit: 100,
	})
	if !c.SnapshotPartial || c.Severity != SeverityPartialSnapshot {
		t.Fatalf("got partial=%v severity=%s", c.SnapshotPartial, c.Severity)
	}
	if c.DeltaTokenBP != nil || c.DeltaCacheHitTokens != nil {
		t.Fatalf("token deltas must be NULL when the snapshot has no token dims")
	}
	if c.RevenueMinor != 5000 {
		t.Fatalf("revenue = %d, want 5000 (money comparison must still run)", c.RevenueMinor)
	}
}

func TestCompareSeverityBoundaries(t *testing.T) {
	tol := Tolerances{TokenBP: 100, MinDeltaTokens: 1000, AmountMinor: 100}
	at := func(v int64) *int64 { return &v }
	cases := []struct {
		name     string
		local    LocalAggregate
		snapshot Snapshot
		want     Severity
	}{
		// 1001 * 10000 / 99000 = 101 bp, maxAbs 1001 > 1000 -> alert
		{"alert", LocalAggregate{CacheHitTokens: 97999}, Snapshot{CacheHitTokens: at(99000), CacheMissTokens: at(0), OutputTokens: at(0)}, SeverityAlert},
		// 1000 * 10000 / 99000 = 101 bp, maxAbs 1000 is not > 1000 -> warn
		{"over threshold but small", LocalAggregate{CacheHitTokens: 98000}, Snapshot{CacheHitTokens: at(99000), CacheMissTokens: at(0), OutputTokens: at(0)}, SeverityWarn},
		// tokens equal, amount off by 101 minor -> warn
		{"amount over tolerance", LocalAggregate{CostCredits: 100}, Snapshot{CacheHitTokens: at(0), CacheMissTokens: at(0), OutputTokens: at(0), AmountMinor: at(201)}, SeverityWarn},
		// tokens and money equal, one request with unknown semantics -> warn
		{"unknown semantics", LocalAggregate{UnknownRequests: 1, CostCredits: 100}, Snapshot{CacheHitTokens: at(0), CacheMissTokens: at(0), OutputTokens: at(0), AmountMinor: at(100)}, SeverityWarn},
		{"ok", LocalAggregate{CostCredits: 100}, Snapshot{CacheHitTokens: at(0), CacheMissTokens: at(0), OutputTokens: at(0), AmountMinor: at(100)}, SeverityOK},
	}
	for _, c := range cases {
		local := c.local
		got := Compare(Input{Local: &local, Snapshot: &c.snapshot, Tolerances: tol, QuotaPerUnit: 100})
		if got.Severity != c.want {
			t.Fatalf("%s: severity = %s, want %s", c.name, got.Severity, c.want)
		}
	}
}

func TestCompareMissingAndNullDims(t *testing.T) {
	tol := Tolerances{TokenBP: 100, MinDeltaTokens: 1000, AmountMinor: 100}
	if got := Compare(Input{Local: &LocalAggregate{}, Tolerances: tol, QuotaPerUnit: 100}); got.Severity != SeverityMissingSnapshot {
		t.Fatalf("nil snapshot = %s, want missing_snapshot", got.Severity)
	}
	if got := Compare(Input{Snapshot: &Snapshot{}, Tolerances: tol, QuotaPerUnit: 100}); got.Severity != SeverityMissingLocal {
		t.Fatalf("nil local = %s, want missing_local", got.Severity)
	}
	hit, miss := int64(100), int64(50)
	got := Compare(Input{
		Local:      &LocalAggregate{CacheHitTokens: 120, CacheMissTokens: 50},
		Snapshot:   &Snapshot{CacheHitTokens: &hit, CacheMissTokens: &miss},
		Tolerances: tol, QuotaPerUnit: 100,
	})
	if !got.SnapshotPartial {
		t.Fatal("a snapshot missing output_tokens is partial")
	}
	if got.DeltaCacheHitTokens == nil || *got.DeltaCacheHitTokens != 20 {
		t.Fatalf("hit delta = %v, want 20", got.DeltaCacheHitTokens)
	}
	if got.DeltaOutputTokens != nil {
		t.Fatalf("output delta = %v, want nil", got.DeltaOutputTokens)
	}
}

// 没有快照（新账号的常见路径）也必须计算网关侧收入，否则总览的收入/毛利
// KPI 会被系统性低估；missing_local 时本地成本未知，收入才为 0。
func TestCompareMissingSnapshotKeepsRevenue(t *testing.T) {
	tol := Tolerances{TokenBP: 100, MinDeltaTokens: 1000, AmountMinor: 100}
	got := Compare(Input{
		Local:        &LocalAggregate{CostCredits: 5000},
		Tolerances:   tol,
		QuotaPerUnit: 100,
	})
	if got.Severity != SeverityMissingSnapshot {
		t.Fatalf("severity = %s, want missing_snapshot", got.Severity)
	}
	if got.RevenueMinor != 5000 {
		t.Fatalf("revenue = %d, want 5000", got.RevenueMinor)
	}

	got = Compare(Input{
		Snapshot:     &Snapshot{},
		Tolerances:   tol,
		QuotaPerUnit: 100,
	})
	if got.Severity != SeverityMissingLocal {
		t.Fatalf("severity = %s, want missing_local", got.Severity)
	}
	if got.RevenueMinor != 0 {
		t.Fatalf("revenue = %d, want 0 when local is missing", got.RevenueMinor)
	}
}

// 已提供维度的真实差异不能被「快照不完整」掩盖：alert 优先于 partial_snapshot。
func TestCompareAlertWinsOverPartialSnapshot(t *testing.T) {
	hit := int64(0)
	c := Compare(Input{
		Local:        &LocalAggregate{CacheHitTokens: 5000},
		Snapshot:     &Snapshot{CacheHitTokens: &hit}, // miss/output 缺失
		Tolerances:   Tolerances{TokenBP: 100, MinDeltaTokens: 1000, AmountMinor: 100},
		QuotaPerUnit: 100,
	})
	if !c.SnapshotPartial {
		t.Fatal("snapshot must be marked partial")
	}
	if c.Severity != SeverityAlert {
		t.Fatalf("severity = %s, want alert", c.Severity)
	}
}
