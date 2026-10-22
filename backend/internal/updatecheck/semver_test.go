package updatecheck

import "testing"

func TestParseSemver(t *testing.T) {
	valid := map[string]Semver{
		"0.1.0":         {Major: 0, Minor: 1, Patch: 0},
		"v1.2.3":        {Major: 1, Minor: 2, Patch: 3},
		"V2.0.0":        {Major: 2, Minor: 0, Patch: 0},
		"1.0.0-alpha.1": {Major: 1, Pre: []string{"alpha", "1"}},
		"1.0.0-rc.1+build.5": {
			Major: 1, Pre: []string{"rc", "1"},
		},
		"1.0.0+build": {Major: 1},
	}
	for raw, want := range valid {
		got, ok := ParseSemver(raw)
		if !ok {
			t.Fatalf("ParseSemver(%q) 失败", raw)
		}
		if got.Major != want.Major || got.Minor != want.Minor || got.Patch != want.Patch {
			t.Fatalf("ParseSemver(%q) = %+v，期望核心 %+v", raw, got, want)
		}
		if len(got.Pre) != len(want.Pre) {
			t.Fatalf("ParseSemver(%q).Pre = %v，期望 %v", raw, got.Pre, want.Pre)
		}
	}

	invalid := []string{
		"", "latest", "1", "1.2", "1.2.3.4", "v", "01.0.0", "1.02.0",
		"1.0.0-", "1.0.0-+build", "1.0.0+", "1.0.0-alpha..1", "1.0.0-01",
		"1.0.0-beta_1",
	}
	for _, raw := range invalid {
		if _, ok := ParseSemver(raw); ok {
			t.Fatalf("ParseSemver(%q) 应失败", raw)
		}
	}
}

func TestCompareSemverOrdering(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"1.0.0", "1.0.1", -1},
		{"1.1.0", "1.0.9", 1},
		{"2.0.0", "1.99.99", 1},
		{"1.0.0-alpha", "1.0.0", -1},
		{"1.0.0", "1.0.0-rc.1", 1},
		{"1.0.0-alpha", "1.0.0-alpha.1", -1},
		{"1.0.0-alpha.1", "1.0.0-alpha.beta", -1},
		{"1.0.0-alpha.beta", "1.0.0-beta", -1},
		{"1.0.0-beta", "1.0.0-beta.2", -1},
		{"1.0.0-beta.2", "1.0.0-beta.11", -1},
		{"1.0.0-beta.11", "1.0.0-rc.1", -1},
		// build metadata 不参与比较。
		{"1.0.0+abc", "1.0.0+xyz", 0},
	}
	for _, tc := range cases {
		a, _ := ParseSemver(tc.a)
		b, _ := ParseSemver(tc.b)
		if got := CompareSemver(a, b); got != tc.want {
			t.Fatalf("CompareSemver(%q,%q) = %d，期望 %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestSemverZeroDetection(t *testing.T) {
	zero, _ := ParseSemver("v0.0.0")
	if !zero.IsZero() {
		t.Fatal("v0.0.0 应判为未注入版本")
	}
	prerelease, _ := ParseSemver("0.0.0-dev")
	if !prerelease.IsZero() {
		t.Fatal("0.0.0-dev 的核心版本仍为 0.0.0，应判为未注入版本")
	}
	one, _ := ParseSemver("0.0.1")
	if one.IsZero() {
		t.Fatal("0.0.1 不是未注入版本")
	}
}
